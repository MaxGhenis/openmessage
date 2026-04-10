package app

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
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
)

// Backfill fetches existing conversations and recent messages from
// Google Messages and stores them in the local database.
func (a *App) Backfill() error {
	if !a.beginBackfill() {
		return fmt.Errorf("backfill already running")
	}
	defer a.endBackfill()

	cli := a.GetClient()
	if cli == nil {
		return fmt.Errorf("client not connected")
	}

	a.Logger.Info().Msg("Starting backfill of conversations and messages")

	resp, err := cli.GM.ListConversations(100, gmproto.ListConversationsRequest_INBOX)
	if err != nil {
		return fmt.Errorf("list conversations: %w", err)
	}

	convos := resp.GetConversations()
	a.Logger.Info().Int("count", len(convos)).Msg("Fetched conversations")

	for _, conv := range convos {
		if err := a.storeConversation(conv); err != nil {
			a.Logger.Error().Err(err).Str("conv_id", conv.GetConversationID()).Msg("Failed to store conversation")
			continue
		}

		msgResp, err := cli.GM.FetchMessages(conv.GetConversationID(), 20, nil)
		if err != nil {
			a.Logger.Warn().Err(err).Str("conv_id", conv.GetConversationID()).Msg("Failed to fetch messages")
			continue
		}

		for _, msg := range msgResp.GetMessages() {
			a.storeMessage(msg)
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

func (a *App) deepBackfill() {
	defer a.endBackfill()

	gm, clientToken := a.currentBackfillClient()
	if gm == nil {
		a.Logger.Error().Msg("Deep backfill: client not connected")
		return
	}

	a.BackfillProgress.reset()
	defer a.BackfillProgress.finish()

	a.Logger.Info().Msg("Starting deep backfill of all messages")

	// Phase A: Paginate ALL folders to discover conversations
	seen := map[string]bool{}
	folders := []gmproto.ListConversationsRequest_Folder{
		gmproto.ListConversationsRequest_INBOX,
		gmproto.ListConversationsRequest_ARCHIVE,
		gmproto.ListConversationsRequest_SPAM_BLOCKED,
	}

	for _, folder := range folders {
		n, aborted := a.paginateFolder(gm, folder, seen, clientToken)
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

	for convID := range seen {
		n, aborted := a.deepBackfillConversationWithToken(gm, convID, clientToken)
		a.BackfillProgress.add(0, n, 0, 0)
		if aborted {
			a.emitConversationsChange()
			a.emitMessagesChange("")
			return
		}
	}

	// Phase C: Contact-based discovery for orphan phone numbers
	a.BackfillProgress.setPhase(BackfillPhaseContacts)
	if a.discoverFromContacts(gm, seen, clientToken) {
		a.emitConversationsChange()
		a.emitMessagesChange("")
		return
	}

	progress := a.BackfillProgress.snapshot()
	a.Logger.Info().
		Int("conversations", progress.ConversationsFound).
		Int("messages", progress.MessagesFound).
		Int("contacts_checked", progress.ContactsChecked).
		Int("errors", progress.Errors).
		Msg("Deep backfill complete")
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
// It stores each conversation and adds its ID to the seen map. Returns the
// number of new conversations found in this folder.
func (a *App) paginateFolder(gm GMClient, folder gmproto.ListConversationsRequest_Folder, seen map[string]bool, clientToken any) (int, bool) {
	found := 0
	var cursor *gmproto.Cursor

	for {
		if a.deepBackfillShouldAbort(clientToken, "folders") {
			return found, true
		}
		resp, err := gm.ListConversationsWithCursor(100, folder, cursor)
		if err != nil {
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
			convID := conv.GetConversationID()
			if seen[convID] {
				continue
			}
			seen[convID] = true
			found++

			if err := a.storeConversation(conv); err != nil {
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

// deepBackfillConversation fetches all messages in a conversation using cursor pagination.
func (a *App) deepBackfillConversation(gm GMClient, convID string) int {
	total, _ := a.deepBackfillConversationWithToken(gm, convID, nil)
	return total
}

func (a *App) deepBackfillConversationWithToken(gm GMClient, convID string, clientToken any) (int, bool) {
	total := 0
	var cursor *gmproto.Cursor

	for {
		if a.deepBackfillShouldAbort(clientToken, "messages") {
			return total, true
		}
		resp, err := gm.FetchMessages(convID, 50, cursor)
		if err != nil {
			a.Logger.Warn().Err(err).Str("conv_id", convID).Msg("Deep backfill: fetch messages failed")
			a.BackfillProgress.addError(fmt.Sprintf("fetch messages %s: %v", convID, err))
			break
		}

		msgs := resp.GetMessages()
		if len(msgs) == 0 {
			break
		}

		for _, msg := range msgs {
			a.storeMessage(msg)
			total++
		}

		cursor = resp.GetCursor()
		if cursor == nil {
			break
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

// discoverFromContacts lists all contacts and tries to find conversations
// for phone numbers not already seen in the folder scan.
func (a *App) discoverFromContacts(gm GMClient, seen map[string]bool, clientToken any) bool {
	if a.deepBackfillShouldAbort(clientToken, "contacts") {
		return true
	}
	contactsResp, err := gm.ListContacts()
	if err != nil {
		a.Logger.Warn().Err(err).Msg("Deep backfill: list contacts failed")
		a.BackfillProgress.addError(fmt.Sprintf("list contacts: %v", err))
		return false
	}

	contacts := contactsResp.GetContacts()
	a.Logger.Info().Int("count", len(contacts)).Msg("Deep backfill: checking contacts for orphan conversations")

	for _, contact := range contacts {
		if a.deepBackfillShouldAbort(clientToken, "contacts") {
			return true
		}
		num := contact.GetNumber()
		if num == nil || num.GetNumber() == "" {
			continue
		}
		phone := num.GetNumber()

		a.BackfillProgress.add(0, 0, 1, 0)

		convResp, err := gm.GetOrCreateConversation(&gmproto.GetOrCreateConversationRequest{
			Numbers: []*gmproto.ContactNumber{
				{
					MysteriousInt: 2,
					Number:        phone,
					Number2:       phone,
				},
			},
		})
		if err != nil {
			a.Logger.Debug().Err(err).Str("phone", phone).Msg("Deep backfill: GetOrCreateConversation failed for contact")
			a.BackfillProgress.addError("")
			continue
		}

		conv := convResp.GetConversation()
		if conv == nil {
			continue
		}

		convID := conv.GetConversationID()
		if seen[convID] {
			continue
		}
		seen[convID] = true

		if err := a.storeConversation(conv); err != nil {
			a.Logger.Error().Err(err).Str("conv_id", convID).Msg("Deep backfill: store contact conversation failed")
			continue
		}

		n, aborted := a.deepBackfillConversationWithToken(gm, convID, clientToken)
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
	gm := a.getGMClient()
	if gm == nil {
		return fmt.Errorf("client not connected")
	}

	convResp, err := gm.GetOrCreateConversation(&gmproto.GetOrCreateConversationRequest{
		Numbers: NewContactNumbers([]string{phone}),
	})
	if err != nil {
		return fmt.Errorf("get or create conversation: %w", err)
	}

	conv := convResp.GetConversation()
	if conv == nil {
		return fmt.Errorf("no conversation returned for %s", phone)
	}

	if err := a.storeConversation(conv); err != nil {
		return fmt.Errorf("store conversation: %w", err)
	}

	n := a.deepBackfillConversation(gm, conv.GetConversationID())
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

	gm, clientToken := a.currentBackfillClient()
	if gm == nil {
		a.Logger.Warn().Str("reason", reason).Msg("Skipping recent reconcile because client is not connected")
		return
	}

	a.Logger.Info().
		Str("reason", reason).
		Int("conversation_limit", recentReconcileConversationLimit).
		Int("message_limit", recentReconcileMessageLimit).
		Msg("Reconciling recent conversations")

	resp, err := gm.ListConversationsWithCursor(recentReconcileConversationLimit, gmproto.ListConversationsRequest_INBOX, nil)
	if err != nil {
		a.Logger.Warn().Err(err).Str("reason", reason).Msg("Recent reconcile: list conversations failed")
		return
	}

	convos := resp.GetConversations()
	if len(convos) == 0 {
		return
	}

	var changed bool
	for _, conv := range convos {
		if !a.backfillClientStillCurrent(clientToken) {
			a.Logger.Warn().Str("reason", reason).Msg("Recent reconcile aborted because client changed or disconnected")
			return
		}

		if err := a.storeConversation(conv); err != nil {
			a.Logger.Warn().Err(err).Str("conv_id", conv.GetConversationID()).Msg("Recent reconcile: store conversation failed")
		} else {
			changed = true
		}

		storedMessages, aborted := a.reconcileRecentConversationMessages(gm, conv.GetConversationID(), clientToken)
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

func (a *App) reconcileRecentConversationMessages(gm GMClient, convID string, clientToken any) (bool, bool) {
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
		if !a.backfillClientStillCurrent(clientToken) {
			return storedAny, true
		}

		msgResp, err := gm.FetchMessages(convID, recentReconcileMessageLimit, cursor)
		if err != nil {
			a.Logger.Warn().Err(err).Str("conv_id", convID).Int("page", page).Msg("Recent reconcile: fetch messages failed")
			return storedAny, false
		}

		msgs := msgResp.GetMessages()
		if len(msgs) == 0 {
			return storedAny, false
		}

		for _, msg := range msgs {
			a.storeMessage(msg)
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
	gm, clientToken := a.currentBackfillClient()
	if gm == nil {
		a.Logger.Warn().Str("conv_id", convID).Str("msg_id", messageID).Msg("Pending media refresh skipped because client is not connected")
		return false, true
	}

	var cursor *gmproto.Cursor
	for page := 0; page < recentReconcileMaxPages; page++ {
		if clientToken != nil && !a.backfillClientStillCurrent(clientToken) {
			a.Logger.Warn().Str("conv_id", convID).Str("msg_id", messageID).Msg("Pending media refresh aborted because client changed or disconnected")
			return false, true
		}
		msgResp, err := gm.FetchMessages(convID, recentReconcileMessageLimit, cursor)
		if err != nil {
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
			a.storeMessage(msg)
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
	if ps := conv.GetParticipants(); len(ps) > 0 {
		type pInfo struct {
			Name   string `json:"name"`
			Number string `json:"number"`
			IsMe   bool   `json:"is_me,omitempty"`
		}
		var infos []pInfo
		for _, p := range ps {
			info := pInfo{
				Name: p.GetFullName(),
				IsMe: p.GetIsMe(),
			}
			if id := p.GetID(); id != nil {
				info.Number = id.GetNumber()
			}
			if info.Number == "" {
				info.Number = p.GetFormattedNumber()
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

	return a.Store.UpsertConversation(&db.Conversation{
		ConversationID: conv.GetConversationID(),
		Name:           conv.GetName(),
		IsGroup:        conv.GetIsGroupChat(),
		Participants:   participantsJSON,
		LastMessageTS:  conv.GetLastMessageTimestamp() / 1000,
		UnreadCount:    unread,
	})
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

	if err := a.Store.UpsertMessage(dbMsg); err != nil {
		a.Logger.Error().Err(err).Str("msg_id", dbMsg.MessageID).Msg("Failed to store backfill message")
	}
}
