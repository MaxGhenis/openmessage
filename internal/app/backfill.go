package app

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"

	"github.com/maxghenis/openmessage/internal/client"
	"github.com/maxghenis/openmessage/internal/db"
)

const (
	recentReconcileConversationLimit = 50
	recentReconcileMessageLimit      = 30
	recentReconcileMaxPages          = 4
	// windowBackfillMaxPages bounds one conversation's paging in a window
	// backfill (50 messages a page), so a reply that never crosses the window
	// boundary cannot page forever.
	windowBackfillMaxPages = 400
)

// orphanContactDiscoveryEnabled reports whether deep backfill should run
// Phase C (contact-based orphan discovery).
//
// Phase C calls GetOrCreateConversation for every contact without prior
// message history. Google Messages treats GetOrCreateConversation as a
// thread-creation call: for each contact that has no existing thread, an
// empty SMS thread is created on the user's phone. For users who only want
// a deep history sync, that is an unwanted side effect.
//
// Phase C is therefore opt-in. Set OPENMESSAGES_BACKFILL_DISCOVER_ORPHANS=1
// (or "true"/"yes"/"on") to enable. Default is disabled.
func orphanContactDiscoveryEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("OPENMESSAGES_BACKFILL_DISCOVER_ORPHANS"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func (a *App) abortBackfillForGoogleAuthError(err error, phase, detail string) bool {
	if !a.HandleGoogleAuthExpiredError(err) {
		return false
	}
	if detail != "" {
		a.BackfillProgress.addError(detail)
	}
	a.Logger.Warn().Err(err).Str("phase", phase).Msg("Deep backfill aborted because Google auth expired")
	return true
}

// Backfill fetches existing conversations and recent messages from
// Google Messages and stores them in the local database.
func (a *App) Backfill() error {
	if !a.beginBackfill() {
		return fmt.Errorf("backfill already running")
	}
	defer a.endBackfill()

	catchUp := a.beginGoogleCatchUp("startup_backfill")
	if catchUp == nil {
		return fmt.Errorf("client not connected")
	}
	defer catchUp.finish()

	a.Logger.Info().Msg("Starting backfill of conversations and messages")

	resp, err := catchUp.gm.ListConversationsWithCursor(100, gmproto.ListConversationsRequest_INBOX, nil)
	if err != nil {
		a.HandleGoogleAuthExpiredError(err)
		return fmt.Errorf("list conversations: %w", err)
	}

	convos := resp.GetConversations()
	a.Logger.Info().Int("count", len(convos)).Msg("Fetched conversations")

	for _, conv := range convos {
		if catchUp.closed {
			break
		}
		if err := catchUp.storeConversation(conv); err != nil {
			a.Logger.Error().Err(err).Str("conv_id", conv.GetConversationID()).Msg("Failed to store conversation")
			continue
		}

		msgResp, err := catchUp.gm.FetchMessages(conv.GetConversationID(), 20, nil)
		if err != nil {
			if a.HandleGoogleAuthExpiredError(err) {
				return fmt.Errorf("fetch messages %s: %w", conv.GetConversationID(), err)
			}
			a.Logger.Warn().Err(err).Str("conv_id", conv.GetConversationID()).Msg("Failed to fetch messages")
			continue
		}

		for _, msg := range msgResp.GetMessages() {
			catchUp.storeMessage(conv.GetConversationID(), conv, msg)
		}
	}

	a.Logger.Info().Int("conversations", len(convos)).Msg("Backfill complete")
	a.emitConversationsChange()
	a.emitMessagesChange("")
	return nil
}

// DeepBackfill fetches ALL conversations from ALL folders with cursor pagination,
// fetches ALL messages for each conversation, and discovers conversations via
// contacts that may not appear in any folder listing.
func (a *App) DeepBackfill() {
	if !a.beginBackfill() {
		a.Logger.Warn().Msg("Deep backfill already running")
		return
	}
	a.deepBackfill()
}

// backfillFolders are the folders deep and window backfills scan.
var backfillFolders = []gmproto.ListConversationsRequest_Folder{
	gmproto.ListConversationsRequest_INBOX,
	gmproto.ListConversationsRequest_ARCHIVE,
	gmproto.ListConversationsRequest_SPAM_BLOCKED,
}

func (a *App) deepBackfill() {
	defer a.endBackfill()

	catchUp := a.beginGoogleCatchUp("deep_backfill")
	if catchUp == nil {
		a.Logger.Error().Msg("Deep backfill: client not connected")
		return
	}
	defer catchUp.finish()
	catchUp.progress = &a.BackfillProgress

	a.BackfillProgress.reset()
	defer a.BackfillProgress.finish()

	a.Logger.Info().Msg("Starting deep backfill of all messages")

	// Phase A: Paginate ALL folders to discover conversations
	seen := map[string]*gmproto.Conversation{}
	for _, folder := range backfillFolders {
		n, aborted := a.paginateFolder(catchUp, folder, seen, nil)
		if aborted {
			a.emitConversationsChange()
			a.emitMessagesChange("")
			return
		}
		a.BackfillProgress.add(0, 0, 0, 1)
		a.Logger.Info().
			Str("folder", folder.String()).
			Int("conversations", n).
			Msg("Deep backfill: folder scan complete")
	}

	// Phase B: Deep backfill messages for each discovered conversation
	a.BackfillProgress.setPhase(BackfillPhaseMessages)

	for convID, conv := range seen {
		n, aborted := a.deepBackfillConversationWithToken(catchUp, convID, conv, 0)
		a.BackfillProgress.add(0, n, 0, 0)
		if aborted {
			a.emitConversationsChange()
			a.emitMessagesChange("")
			return
		}
	}

	// Phase C: Contact-based discovery for orphan phone numbers.
	// Off by default because GetOrCreateConversation creates an empty SMS
	// thread on the user's phone for each contact lacking one. Opt in via
	// OPENMESSAGES_BACKFILL_DISCOVER_ORPHANS=1.
	if orphanContactDiscoveryEnabled() {
		a.BackfillProgress.setPhase(BackfillPhaseContacts)
		if a.discoverFromContacts(catchUp, seen) {
			a.emitConversationsChange()
			a.emitMessagesChange("")
			return
		}
	} else {
		a.Logger.Info().
			Msg("Skipping Phase C (orphan-contact discovery); set OPENMESSAGES_BACKFILL_DISCOVER_ORPHANS=1 to enable. Note: enabling creates empty SMS threads for contacts without prior message history.")
	}

	progress := a.BackfillProgress.snapshot()
	a.Logger.Info().
		Int("conversations", progress.ConversationsFound).
		Int("messages", progress.MessagesFound).
		Int("contacts_checked", progress.ContactsChecked).
		Int("errors", progress.Errors).
		Int("history_teed", progress.HistoryTeed).
		Int("history_tee_failed", progress.HistoryTeeFailed).
		Msg("Deep backfill complete")
	a.emitConversationsChange()
	a.emitMessagesChange("")
}

// StartGoogleWindowBackfill starts a guarded background re-fetch of every
// Google message from since onward (see windowBackfill). It reports false when
// a backfill or catch-up is already running.
func (a *App) StartGoogleWindowBackfill(since time.Time) bool {
	if !a.beginBackfill() {
		return false
	}
	go a.windowBackfill(since)
	return true
}

// windowBackfill re-fetches the messages the phone holds from since onward,
// into both stores. It lists every folder, keeps the conversations whose last
// message is at or after since, and pages each one's messages newest first
// until a page reaches older than since. This is the recovery for a window the
// live channel skipped (a phone that stopped relaying): unlike DeepBackfill it
// does not re-fetch every message the phone has ever held, and unlike the
// recent reconcile it does not stop at the newest message already stored, which
// after the live channel resumes sits above the hole.
func (a *App) windowBackfill(since time.Time) {
	defer a.endBackfill()

	// Progress is reset before anything can fail, so /api/backfill/status
	// reports this run (with its error) rather than the previous one.
	a.BackfillProgress.reset()
	defer a.BackfillProgress.finish()

	catchUp := a.beginGoogleCatchUp("window_backfill")
	if catchUp == nil {
		a.Logger.Error().Msg("Window backfill: client not connected")
		a.BackfillProgress.addError("window backfill: Google client not connected")
		return
	}
	defer catchUp.finish()
	catchUp.progress = &a.BackfillProgress

	sinceMS := since.UnixMilli()
	a.Logger.Info().Time("since", since).Msg("Starting window backfill")

	inWindow := func(conv *gmproto.Conversation) bool {
		// LastMessageTimestamp is in microseconds; 0 means the phone did not
		// say, so keep the conversation rather than risk skipping the hole.
		last := conv.GetLastMessageTimestamp() / 1000
		return last == 0 || last >= sinceMS
	}
	seen := map[string]*gmproto.Conversation{}
	aborted := func() {
		a.BackfillProgress.addError("window backfill stopped early: the Google connection changed or ended; run it again")
		a.emitConversationsChange()
		a.emitMessagesChange("")
	}
	for _, folder := range backfillFolders {
		n, stopped := a.paginateFolder(catchUp, folder, seen, inWindow)
		if stopped {
			aborted()
			return
		}
		a.BackfillProgress.add(0, 0, 0, 1)
		a.Logger.Info().
			Str("folder", folder.String()).
			Int("conversations_in_window", n).
			Msg("Window backfill: folder scan complete")
	}

	a.BackfillProgress.setPhase(BackfillPhaseMessages)
	for convID, conv := range seen {
		n, stopped := a.deepBackfillConversationWithToken(catchUp, convID, conv, sinceMS)
		a.BackfillProgress.add(0, n, 0, 0)
		if stopped {
			aborted()
			return
		}
	}

	progress := a.BackfillProgress.snapshot()
	a.Logger.Info().
		Time("since", since).
		Int("conversations", progress.ConversationsFound).
		Int("messages", progress.MessagesFound).
		Int("errors", progress.Errors).
		Int("history_teed", progress.HistoryTeed).
		Int("history_tee_failed", progress.HistoryTeeFailed).
		Msg("Window backfill complete")
	a.emitConversationsChange()
	a.emitMessagesChange("")
}

func (a *App) deepBackfillShouldAbort(clientToken any, phase string) bool {
	if clientToken == nil || a.backfillClientStillCurrent(clientToken) {
		return false
	}
	a.Logger.Warn().Str("phase", phase).Msg("Deep backfill aborted because client changed or disconnected")
	return true
}

// paginateFolder fetches all conversations in a folder using cursor pagination.
// It stores each conversation that keep accepts (all of them when keep is nil)
// and records it in seen. Returns the number of new conversations kept from
// this folder.
func (a *App) paginateFolder(
	catchUp *googleCatchUp,
	folder gmproto.ListConversationsRequest_Folder,
	seen map[string]*gmproto.Conversation,
	keep func(*gmproto.Conversation) bool,
) (int, bool) {
	found := 0
	var cursor *gmproto.Cursor

	for {
		if catchUp.shouldAbort("folders") {
			return found, true
		}
		resp, err := catchUp.gm.ListConversationsWithCursor(100, folder, cursor)
		if err != nil {
			if a.abortBackfillForGoogleAuthError(err, "folders", fmt.Sprintf("list %s: %v", folder.String(), err)) {
				return found, true
			}
			a.Logger.Error().Err(err).Str("folder", folder.String()).Msg("Deep backfill: list conversations failed")
			a.BackfillProgress.addError(fmt.Sprintf("list %s: %v", folder.String(), err))
			break
		}

		convos := resp.GetConversations()
		if len(convos) == 0 {
			break
		}

		batchFound := 0
		batchErrors := 0
		for _, conv := range convos {
			if catchUp.closed {
				break
			}
			convID := conv.GetConversationID()
			if _, ok := seen[convID]; ok {
				continue
			}
			if keep != nil && !keep(conv) {
				continue
			}
			seen[convID] = conv
			found++

			if err := catchUp.storeConversation(conv); err != nil {
				a.Logger.Error().Err(err).Str("conv_id", convID).Msg("Deep backfill: store conversation failed")
				batchErrors++
				continue
			}
			batchFound++
		}
		a.BackfillProgress.add(batchFound, 0, 0, 0)
		if batchErrors > 0 {
			// Record count but don't spam ErrorDetails with per-conversation store failures
			for range batchErrors {
				a.BackfillProgress.addError("")
			}
		}

		cursor = resp.GetCursor()
		if cursor == nil {
			break
		}

		a.Logger.Debug().
			Str("folder", folder.String()).
			Int("batch", len(convos)).
			Int("found_so_far", found).
			Msg("Deep backfill: fetched conversation batch")
	}

	return found, false
}

// deepBackfillConversationWithToken fetches a conversation's messages newest
// first with cursor pagination. With stopBeforeMS > 0 (a window backfill) it
// stops after the first page that reaches a message older than stopBeforeMS,
// and when a reply carries no cursor it continues from the oldest message it
// has seen, as libgm's own bridge does, because a missing cursor does not mean
// the conversation is exhausted. With stopBeforeMS == 0 it fetches every page
// the phone hands a cursor for. conv is the conversation's listed snapshot
// (nil when the caller has none). It reports the number of messages fetched
// and whether the catch-up must abort.
func (a *App) deepBackfillConversationWithToken(
	catchUp *googleCatchUp,
	convID string,
	conv *gmproto.Conversation,
	stopBeforeMS int64,
) (int, bool) {
	total := 0
	var cursor *gmproto.Cursor
	windowed := stopBeforeMS > 0
	// oldestSeenMS is the oldest positive message time stored so far;
	// synthesized marks a cursor this loop built because the reply had none.
	oldestSeenMS := int64(0)
	synthesized := false

	for page := 0; ; page++ {
		if catchUp.shouldAbort("messages") {
			return total, true
		}
		if windowed && page >= windowBackfillMaxPages {
			a.Logger.Warn().Str("conv_id", convID).Int("pages", page).Msg("Window backfill: page limit reached before the window boundary")
			a.BackfillProgress.addError(fmt.Sprintf("fetch messages %s: page limit reached before the window boundary", convID))
			break
		}
		resp, err := catchUp.gm.FetchMessages(convID, 50, cursor)
		if err != nil {
			if a.abortBackfillForGoogleAuthError(err, "messages", fmt.Sprintf("fetch messages %s: %v", convID, err)) {
				return total, true
			}
			a.Logger.Warn().Err(err).Str("conv_id", convID).Msg("Deep backfill: fetch messages failed")
			a.BackfillProgress.addError(fmt.Sprintf("fetch messages %s: %v", convID, err))
			break
		}

		msgs := resp.GetMessages()
		if len(msgs) == 0 {
			break
		}

		// Only a real (positive) timestamp can place a message before the
		// window; a message with no timestamp says nothing about where the
		// page is.
		var oldest *gmproto.Message
		for _, msg := range msgs {
			if ts := msg.GetTimestamp() / 1000; ts > 0 && (oldest == nil || ts < oldest.GetTimestamp()/1000) {
				oldest = msg
			}
		}
		if synthesized && (oldest == nil || oldest.GetTimestamp()/1000 >= oldestSeenMS) {
			// The page fetched below the oldest message holds nothing older:
			// the conversation is exhausted (or the cursor was not honoured).
			break
		}

		reachedBoundary := false
		for _, msg := range msgs {
			if catchUp.closed {
				break
			}
			catchUp.storeMessage(convID, conv, msg)
			total++
			if ts := msg.GetTimestamp() / 1000; windowed && ts > 0 && ts < stopBeforeMS {
				reachedBoundary = true
			}
		}
		if reachedBoundary {
			break
		}
		if oldest != nil && (oldestSeenMS == 0 || oldest.GetTimestamp()/1000 < oldestSeenMS) {
			oldestSeenMS = oldest.GetTimestamp() / 1000
		}

		cursor = resp.GetCursor()
		synthesized = false
		if cursor == nil {
			if !windowed || oldestSeenMS == 0 {
				break
			}
			// No cursor, boundary not reached: a missing cursor does not mean
			// the conversation is exhausted, so continue below the oldest
			// message stored so far.
			cursor = &gmproto.Cursor{LastItemID: oldestMessageID(msgs, oldest), LastItemTimestamp: oldestSeenMS}
			synthesized = true
		}

		a.Logger.Debug().
			Str("conv_id", convID).
			Int("batch", len(msgs)).
			Int("total_so_far", total).
			Msg("Deep backfill: fetched message batch")
	}

	if total > 0 {
		a.Logger.Info().
			Str("conv_id", convID).
			Int("messages", total).
			Msg("Deep backfill: conversation complete")
	}

	return total, false
}

// oldestMessageID names the message a synthesized cursor continues below: the
// page's oldest timestamped message, else the page's last message.
func oldestMessageID(msgs []*gmproto.Message, oldest *gmproto.Message) string {
	if oldest != nil {
		return oldest.GetMessageID()
	}
	return msgs[len(msgs)-1].GetMessageID()
}

// discoverFromContacts lists all contacts and tries to find conversations
// for phone numbers not already seen in the folder scan.
func (a *App) discoverFromContacts(catchUp *googleCatchUp, seen map[string]*gmproto.Conversation) bool {
	if catchUp.shouldAbort("contacts") {
		return true
	}
	contactsResp, err := catchUp.gm.ListContacts()
	if err != nil {
		if a.abortBackfillForGoogleAuthError(err, "contacts", fmt.Sprintf("list contacts: %v", err)) {
			return true
		}
		a.Logger.Warn().Err(err).Msg("Deep backfill: list contacts failed")
		a.BackfillProgress.addError(fmt.Sprintf("list contacts: %v", err))
		return false
	}

	contacts := contactsResp.GetContacts()
	a.Logger.Info().Int("count", len(contacts)).Msg("Deep backfill: checking contacts for orphan conversations")

	for _, contact := range contacts {
		if catchUp.shouldAbort("contacts") {
			return true
		}
		num := contact.GetNumber()
		if num == nil || num.GetNumber() == "" {
			continue
		}
		phone := num.GetNumber()

		a.BackfillProgress.add(0, 0, 1, 0)

		convResp, err := catchUp.gm.GetOrCreateConversation(&gmproto.GetOrCreateConversationRequest{
			Numbers: []*gmproto.ContactNumber{
				{
					MysteriousInt: 2,
					Number:        phone,
					Number2:       phone,
				},
			},
		})
		if err != nil {
			if a.abortBackfillForGoogleAuthError(err, "contacts", fmt.Sprintf("get or create conversation %s: %v", phone, err)) {
				return true
			}
			a.Logger.Debug().Err(err).Str("phone", phone).Msg("Deep backfill: GetOrCreateConversation failed for contact")
			a.BackfillProgress.addError("")
			continue
		}

		conv := convResp.GetConversation()
		if conv == nil {
			continue
		}

		convID := conv.GetConversationID()
		if _, ok := seen[convID]; ok {
			continue
		}
		seen[convID] = conv

		if err := catchUp.storeConversation(conv); err != nil {
			a.Logger.Error().Err(err).Str("conv_id", convID).Msg("Deep backfill: store contact conversation failed")
			continue
		}

		n, aborted := a.deepBackfillConversationWithToken(catchUp, convID, conv, 0)
		a.BackfillProgress.add(1, n, 0, 0)
		if aborted {
			return true
		}

		a.Logger.Info().
			Str("phone", phone).
			Str("conv_id", convID).
			Int("messages", n).
			Msg("Deep backfill: discovered conversation via contact")
	}
	return false
}

// BackfillConversationByPhone looks up or creates a conversation for a specific
// phone number, stores it, and deep-backfills all its messages.
func (a *App) BackfillConversationByPhone(phone string) error {
	catchUp := a.beginGoogleCatchUp("phone_backfill")
	if catchUp == nil {
		return fmt.Errorf("client not connected")
	}
	defer catchUp.finish()

	convResp, err := catchUp.gm.GetOrCreateConversation(&gmproto.GetOrCreateConversationRequest{
		Numbers: NewContactNumbers([]string{phone}),
	})
	if err != nil {
		a.HandleGoogleAuthExpiredError(err)
		return fmt.Errorf("get or create conversation: %w", err)
	}

	conv := convResp.GetConversation()
	if conv == nil {
		return fmt.Errorf("no conversation returned for %s", phone)
	}

	if err := catchUp.storeConversation(conv); err != nil {
		return fmt.Errorf("store conversation: %w", err)
	}

	// A phone backfill is a one-shot user request: it finishes even if the
	// client reconnects meanwhile (the fetched rows are still valid history),
	// so drop the client-change token its message paging would abort on.
	catchUp.token = nil
	n, _ := a.deepBackfillConversationWithToken(catchUp, conv.GetConversationID(), conv, 0)
	a.Logger.Info().
		Str("phone", phone).
		Str("conv_id", conv.GetConversationID()).
		Int("messages", n).
		Msg("Phone backfill complete")
	a.emitConversationsChange()
	a.emitMessagesChange(conv.GetConversationID())

	return nil
}

func (a *App) reconcileRecentConversations(reason string) {
	defer a.reconcileRunning.Store(false)

	catchUp := a.beginGoogleCatchUp("reconcile_" + reason)
	if catchUp == nil {
		a.Logger.Warn().Str("reason", reason).Msg("Skipping recent reconcile because client is not connected")
		return
	}
	defer catchUp.finish()

	a.Logger.Info().
		Str("reason", reason).
		Int("conversation_limit", recentReconcileConversationLimit).
		Int("message_limit", recentReconcileMessageLimit).
		Msg("Reconciling recent conversations")

	resp, err := catchUp.gm.ListConversationsWithCursor(recentReconcileConversationLimit, gmproto.ListConversationsRequest_INBOX, nil)
	if err != nil {
		if a.HandleGoogleAuthExpiredError(err) {
			a.Logger.Warn().Err(err).Str("reason", reason).Msg("Recent reconcile aborted because Google auth expired")
			return
		}
		a.Logger.Warn().Err(err).Str("reason", reason).Msg("Recent reconcile: list conversations failed")
		return
	}

	convos := resp.GetConversations()
	if len(convos) == 0 {
		return
	}

	var changed bool
	for _, conv := range convos {
		if !catchUp.stillCurrent() {
			a.Logger.Warn().Str("reason", reason).Msg("Recent reconcile aborted because client changed or disconnected")
			return
		}

		if err := catchUp.storeConversation(conv); err != nil {
			a.Logger.Warn().Err(err).Str("conv_id", conv.GetConversationID()).Msg("Recent reconcile: store conversation failed")
		} else {
			changed = true
		}

		storedMessages, aborted := a.reconcileRecentConversationMessages(catchUp, conv)
		if aborted {
			a.Logger.Warn().Str("reason", reason).Str("conv_id", conv.GetConversationID()).Msg("Recent reconcile aborted while fetching messages")
			return
		}
		if storedMessages {
			changed = true
		}
	}

	if changed {
		a.emitConversationsChange()
		a.emitMessagesChange("")
	}
}

func (a *App) reconcileRecentConversationMessages(catchUp *googleCatchUp, conv *gmproto.Conversation) (bool, bool) {
	convID := conv.GetConversationID()
	localLatest, err := a.Store.GetMessagesByConversation(convID, 1)
	if err != nil {
		a.Logger.Warn().Err(err).Str("conv_id", convID).Msg("Recent reconcile: read local boundary failed")
	}

	var (
		localLatestTS int64
		localLatestID string
		cursor        *gmproto.Cursor
		storedAny     bool
	)
	if len(localLatest) > 0 {
		localLatestTS = localLatest[0].TimestampMS
		localLatestID = localLatest[0].MessageID
	}

	for page := 0; page < recentReconcileMaxPages; page++ {
		if !catchUp.stillCurrent() {
			return storedAny, true
		}

		msgResp, err := catchUp.gm.FetchMessages(convID, recentReconcileMessageLimit, cursor)
		if err != nil {
			if a.HandleGoogleAuthExpiredError(err) {
				return storedAny, true
			}
			a.Logger.Warn().Err(err).Str("conv_id", convID).Int("page", page).Msg("Recent reconcile: fetch messages failed")
			return storedAny, false
		}

		msgs := msgResp.GetMessages()
		if len(msgs) == 0 {
			return storedAny, false
		}

		for _, msg := range msgs {
			catchUp.storeMessage(convID, conv, msg)
		}
		storedAny = true

		if localLatestTS == 0 {
			return storedAny, false
		}
		if reconcileBatchReachedLocalBoundary(msgs, localLatestTS, localLatestID) {
			return storedAny, false
		}

		cursor = msgResp.GetCursor()
		if cursor == nil {
			return storedAny, false
		}
	}

	return storedAny, false
}

func (a *App) refreshPendingMediaMessageWithSchedule(convID, messageID string, schedule []time.Duration) {
	for idx, delay := range schedule {
		if idx > 0 && delay > 0 {
			time.Sleep(delay)
		}
		refreshed, resolved := a.refreshPendingMediaMessageAttempt(convID, messageID)
		if refreshed {
			a.emitMessagesChange(convID)
		}
		if resolved {
			return
		}
	}
}

func (a *App) refreshPendingMediaMessageAttempt(convID, messageID string) (bool, bool) {
	catchUp := a.beginGoogleCatchUp("pending_media_refresh")
	if catchUp == nil {
		a.Logger.Warn().Str("conv_id", convID).Str("msg_id", messageID).Msg("Pending media refresh skipped because client is not connected")
		return false, true
	}
	defer catchUp.finish()

	var cursor *gmproto.Cursor
	for page := 0; page < recentReconcileMaxPages; page++ {
		if !catchUp.stillCurrent() {
			a.Logger.Warn().Str("conv_id", convID).Str("msg_id", messageID).Msg("Pending media refresh aborted because client changed or disconnected")
			return false, true
		}
		msgResp, err := catchUp.gm.FetchMessages(convID, recentReconcileMessageLimit, cursor)
		if err != nil {
			if a.HandleGoogleAuthExpiredError(err) {
				return false, true
			}
			a.Logger.Warn().Err(err).Str("conv_id", convID).Str("msg_id", messageID).Msg("Pending media refresh fetch failed")
			return false, false
		}
		msgs := msgResp.GetMessages()
		if len(msgs) == 0 {
			return false, false
		}

		for _, msg := range msgs {
			if strings.TrimSpace(msg.GetMessageID()) != messageID {
				continue
			}
			// v2 history is insert-only, so this offers v2 the message only if
			// it never arrived there; hydrating a stored placeholder stays with
			// the live channel.
			catchUp.storeMessage(convID, nil, msg)
			refreshed, resolved := a.pendingMediaRefreshResolved(msg)
			return refreshed, resolved
		}

		cursor = msgResp.GetCursor()
		if cursor == nil {
			return false, false
		}
	}

	return false, false
}

func (a *App) pendingMediaRefreshResolved(msg *gmproto.Message) (bool, bool) {
	if msg == nil {
		return false, false
	}
	if media := client.ExtractMediaInfo(msg); media != nil && strings.TrimSpace(media.MediaID) != "" {
		return true, true
	}
	body := strings.ToLower(strings.TrimSpace(client.ExtractMessageBody(msg)))
	if msg.GetType() != 3 && !strings.HasSuffix(body, "from phone") {
		return true, true
	}
	return true, false
}

func reconcileBatchReachedLocalBoundary(msgs []*gmproto.Message, localLatestTS int64, localLatestID string) bool {
	if localLatestTS == 0 || len(msgs) == 0 {
		return true
	}

	oldestTS := msgs[0].GetTimestamp() / 1000
	for _, msg := range msgs {
		ts := msg.GetTimestamp() / 1000
		if ts < oldestTS {
			oldestTS = ts
		}
		if localLatestID != "" && msg.GetMessageID() == localLatestID {
			return true
		}
	}

	return oldestTS < localLatestTS
}

func (a *App) storeConversation(conv *gmproto.Conversation) error {
	participantsJSON := "[]"
	var avatarCandidates []db.ContactAvatarCandidate
	if ps := conv.GetParticipants(); len(ps) > 0 {
		type pInfo struct {
			Name      string `json:"name"`
			Number    string `json:"number"`
			IsMe      bool   `json:"is_me,omitempty"`
			ID        string `json:"id,omitempty"` // participant ID, used to resolve reaction actors to names
			ContactID string `json:"contact_id,omitempty"`
		}
		var infos []pInfo
		for _, p := range ps {
			info := pInfo{
				Name:      p.GetFullName(),
				IsMe:      p.GetIsMe(),
				ContactID: p.GetContactID(),
			}
			if id := p.GetID(); id != nil {
				info.Number = id.GetNumber()
				info.ID = id.GetParticipantID()
			}
			if info.Number == "" {
				info.Number = p.GetFormattedNumber()
			}
			if !info.IsMe {
				avatarCandidates = append(avatarCandidates, db.ContactAvatarCandidate{
					SourcePlatform: "sms",
					ParticipantID:  info.ID,
					ContactID:      info.ContactID,
					PhoneNumber:    info.Number,
					DisplayName:    info.Name,
					Source:         "backfill",
				})
			}
			infos = append(infos, info)
		}
		if b, err := json.Marshal(infos); err == nil {
			participantsJSON = string(b)
		}
	}

	unread := 0
	if conv.GetUnread() {
		unread = 1
	}

	if err := a.Store.ApplyConversationSnapshot(&db.Conversation{
		ConversationID: conv.GetConversationID(),
		Name:           conv.GetName(),
		IsGroup:        conv.GetIsGroupChat(),
		Participants:   participantsJSON,
		LastMessageTS:  conv.GetLastMessageTimestamp() / 1000,
		UnreadCount:    unread,
	}); err != nil {
		return err
	}
	a.QueueGoogleAvatarCandidates(avatarCandidates)
	return nil
}

func (a *App) storeMessage(msg *gmproto.Message) {
	body := client.ExtractMessageBody(msg)
	senderName, senderNumber := client.ExtractSenderInfo(msg)

	status := "unknown"
	if ms := msg.GetMessageStatus(); ms != nil {
		status = ms.GetStatus().String()
	}

	dbMsg := &db.Message{
		MessageID:      msg.GetMessageID(),
		ConversationID: msg.GetConversationID(),
		SenderName:     senderName,
		SenderNumber:   senderNumber,
		Body:           body,
		TimestampMS:    msg.GetTimestamp() / 1000,
		Status:         status,
		IsFromMe:       client.MessageIsFromMe(msg),
		SourcePlatform: "sms",
	}

	if media := client.ExtractMediaInfo(msg); media != nil {
		dbMsg.MediaID = media.MediaID
		dbMsg.MimeType = media.MimeType
		dbMsg.DecryptionKey = hex.EncodeToString(media.DecryptionKey)
	}

	if reactions := client.ExtractReactions(msg); reactions != nil {
		if b, err := json.Marshal(reactions); err == nil {
			dbMsg.Reactions = string(b)
		}
	}
	dbMsg.ReplyToID = client.ExtractReplyToID(msg)

	// Skip empty contentless stubs so backfill doesn't repopulate "Empty
	// message" rows that the live path and startup repair remove.
	if db.IsEmptyStubMessage(dbMsg) {
		return
	}

	if err := a.Store.UpsertMessage(dbMsg); err != nil {
		a.Logger.Error().Err(err).Str("msg_id", dbMsg.MessageID).Msg("Failed to store backfill message")
	}
}
