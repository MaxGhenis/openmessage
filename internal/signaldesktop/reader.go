package signaldesktop

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/maxghenis/openmessage/internal/v2keys"
)

// MaxChunkMessages bounds one frame's messages, as for WhatsApp Desktop.
const MaxChunkMessages = 100

// ErrAccountMismatch means Signal Desktop is linked to a different account
// than the one v2 holds. The feed refuses to read anything in that case.
var ErrAccountMismatch = errors.New("Signal Desktop is linked to a different Signal account than OpenMessage's")

// ErrAccountUnverified means Signal Desktop's own number could not be read,
// so the feed cannot confirm the archive belongs to the v2 account.
var ErrAccountUnverified = errors.New("Signal Desktop's own number could not be read, so its account cannot be confirmed")

// Participant is one roster entry in a captured chat. Address is the form the
// live bridge keys this person by (see Facts.ChooseAddress).
type Participant struct {
	Address string
	Name    string
	IsSelf  bool
}

// Chat is a captured conversation. RemoteID is the canonical v2 remote
// conversation ID: "signal:<address>" or "signal-group:<group id>".
type Chat struct {
	RemoteID     string
	Group        bool
	Title        string
	Participants []Participant
}

// Attachment is what the body placeholder needs. Signal Desktop encrypts its
// attachment files at rest, so no file is captured.
type Attachment struct {
	ContentType string
	FileName    string
}

// Reaction is one current reaction on a captured message.
type Reaction struct {
	ActorAddress string
	ActorName    string
	IsSelf       bool
	Emoji        string
	TimestampMS  int64
}

// Message is one captured user message. AuthorAddress is empty for outgoing.
type Message struct {
	RowID         int64
	AuthorAddress string
	AuthorName    string
	Outgoing      bool
	SentAtMS      int64
	Body          string
	Attachments   []Attachment
	Sticker       bool
	Contact       bool
	Payment       bool
	Poll          bool
	Preview       bool
	ViewOnce      bool
	StoryReply    bool
	Reactions     []Reaction
}

// Chunk is one chat and at most MaxChunkMessages of its messages.
type Chunk struct {
	Chat     Chat
	Messages []Message
}

// ScanOptions picks the rows a scan reads.
type ScanOptions struct {
	// AfterRowID is the last successful scan's high-water mark. Zero reads the
	// whole archive. Skipped rows advance the mark too.
	AfterRowID int64
	// ChunkSize caps messages per chunk (at most MaxChunkMessages).
	ChunkSize int
	// SinceMS adds count-only tallies of rows sent at or after this time, for
	// dry runs. It filters nothing.
	SinceMS int64
}

// Stats describe one scan with counts only: never text, names or numbers.
type Stats struct {
	HighWaterRowID           int64            `json:"high_water_rowid"`
	MaxRowID                 int64            `json:"max_rowid"`
	AccountVerified          bool             `json:"account_verified"`
	AttachmentSource         string           `json:"attachment_source"`
	ChatsResolved            int64            `json:"chats_resolved"`
	ChatsSkippedByReason     map[string]int64 `json:"chats_skipped_by_reason"`
	RowsScanned              int64            `json:"rows_scanned"`
	RowsIncluded             int64            `json:"rows_included"`
	RowsInV2                 int64            `json:"rows_in_v2"`
	RowsSkippedByReason      map[string]int64 `json:"rows_skipped_by_reason"`
	ReactionsIncluded        int64            `json:"reactions_included"`
	ReactionsSkipped         int64            `json:"reactions_skipped"`
	RowsSince                int64            `json:"rows_since,omitempty"`
	RowsSinceIncluded        int64            `json:"rows_since_included,omitempty"`
	RowsSinceInV2            int64            `json:"rows_since_in_v2,omitempty"`
	RowsSinceSkippedByReason map[string]int64 `json:"rows_since_skipped_by_reason,omitempty"`
}

func newStats(afterRowID int64) Stats {
	return Stats{
		HighWaterRowID:           afterRowID,
		ChatsSkippedByReason:     map[string]int64{},
		RowsSkippedByReason:      map[string]int64{},
		RowsSinceSkippedByReason: map[string]int64{},
	}
}

// Reader turns Signal Desktop rows into chunks. It never writes anywhere.
type Reader struct {
	Runner Runner
}

// Queries returns the read-only queries one scan runs, in one transaction.
// They use only columns the legacy importer has read from Signal Desktop since
// 2025, plus json_extract of the message's own JSON for everything else.
func Queries(afterRowID int64) []Query {
	return []Query{
		{
			Name:     "self",
			SQL:      `SELECT id AS id, COALESCE(json_extract(json, '$.value'), '') AS value FROM items WHERE id IN ('number_id', 'uuid_id') ORDER BY id`,
			Optional: true,
		},
		{
			Name: "max_rowid",
			SQL:  `SELECT COALESCE(MAX(rowid), 0) AS max_rowid FROM messages`,
		},
		{
			Name: "conversations",
			SQL: `SELECT
				c.id AS id,
				c.type AS type,
				COALESCE(c.e164, '') AS e164,
				COALESCE(c.serviceId, '') AS service_id,
				COALESCE(c.groupId, '') AS group_id,
				COALESCE(c.name, '') AS name,
				COALESCE(c.profileName, '') AS profile_name,
				COALESCE(c.profileFamilyName, '') AS profile_family_name,
				COALESCE(c.profileFullName, '') AS profile_full_name,
				COALESCE(c.members, '') AS members,
				COALESCE(json_extract(c.json, '$.membersV2'), '') AS members_v2
			FROM conversations c
			WHERE c.type IN ('private', 'group')
			ORDER BY c.id`,
		},
		{
			Name: "messages",
			SQL: `SELECT
				m.rowid AS row_id,
				m.id AS id,
				COALESCE(m.conversationId, '') AS conversation_id,
				COALESCE(m.type, '') AS type,
				COALESCE(m.body, '') AS body,
				COALESCE(m.sent_at, 0) AS sent_at,
				-- messages.source is declared STRING, which SQLite gives
				-- numeric affinity: a stored "+1555..." comes back as the
				-- integer 1555.... Put the plus back.
				CASE WHEN typeof(m.source) = 'integer' THEN '+' || m.source ELSE COALESCE(m.source, '') END AS source,
				COALESCE(m.sourceServiceId, '') AS source_service_id,
				COALESCE(json_extract(m.json, '$.deletedForEveryone'), 0) AS deleted_for_everyone,
				COALESCE(json_extract(m.json, '$.isViewOnce'), 0) AS view_once,
				json_type(m.json, '$.sticker') AS sticker_type,
				COALESCE(json_array_length(m.json, '$.contact'), 0) AS contact_count,
				json_type(m.json, '$.payment') AS payment_type,
				COALESCE(json_array_length(m.json, '$.preview'), 0) AS preview_count,
				json_type(m.json, '$.poll') AS poll_type,
				COALESCE(json_extract(m.json, '$.storyId'), '') AS story_id,
				COALESCE(json_extract(m.json, '$.sendStateByConversationId'), '') AS send_state,
				COALESCE(json_extract(m.json, '$.reactions'), '') AS reactions,
				COALESCE(json_extract(m.json, '$.attachments'), '') AS json_attachments
			FROM messages m
			WHERE m.rowid > ?
			ORDER BY m.rowid`,
			Params: []any{afterRowID},
		},
		{
			// Signal Desktop 7.x+ keeps attachments in their own table, typed so
			// quote thumbnails, link previews and older edit versions are not
			// mistaken for the message's own attachment.
			Name: "attachments_typed",
			SQL: `SELECT
				a.messageId AS message_id,
				COALESCE(a.attachmentType, '') AS attachment_type,
				COALESCE(a.editHistoryIndex, -1) AS edit_history_index,
				COALESCE(a.contentType, '') AS content_type,
				COALESCE(a.fileName, '') AS file_name
			FROM message_attachments a
			WHERE a.messageId IN (SELECT id FROM messages WHERE rowid > ?)
			ORDER BY a.messageId, a.orderInMessage, a.rowid`,
			Params:   []any{afterRowID},
			Optional: true,
		},
		{
			// The legacy importer's columns, if the typed ones are missing.
			Name: "attachments_basic",
			SQL: `SELECT
				a.messageId AS message_id,
				COALESCE(a.contentType, '') AS content_type,
				COALESCE(a.fileName, '') AS file_name
			FROM message_attachments a
			WHERE a.messageId IN (SELECT id FROM messages WHERE rowid > ?)
			ORDER BY a.messageId, a.rowid`,
			Params:   []any{afterRowID},
			Optional: true,
		},
	}
}

type contact struct {
	conversationID string
	e164           string
	aci            string
	name           string
	address        string
}

type chatState struct {
	chat     Chat
	reason   string
	peer     string
	messages []Message
}

// Scan reads one consistent snapshot and emits chunks in a deterministic
// order: chats by remote ID, messages by sent time. facts decides which
// address form each contact is keyed by and which messages v2 already holds;
// a message v2 holds under any of its forms is counted (RowsInV2) and not
// emitted. The worker re-checks everything, so this only keeps no-op frames
// out of the inbox and catches the forms the worker cannot see.
func (r *Reader) Scan(ctx context.Context, facts *Facts, options ScanOptions, emit func(Chunk) error) (Stats, error) {
	stats := newStats(options.AfterRowID)
	if r == nil || r.Runner == nil {
		return stats, errors.New("Signal Desktop reader has no runner")
	}
	if facts == nil || facts.SelfE164 == "" {
		return stats, errors.New("OpenMessage has no Signal account number to match Signal Desktop against")
	}
	results, err := r.Runner.Run(ctx, Queries(options.AfterRowID))
	if err != nil {
		return stats, err
	}
	for _, name := range []string{"conversations", "messages"} {
		if result, ok := results[name]; !ok || result.Error != "" {
			return stats, fmt.Errorf("Signal Desktop query %s failed", name)
		}
	}

	if rows := results["max_rowid"].Rows; len(rows) == 1 {
		stats.MaxRowID = rows[0].int("max_rowid")
	}
	selfE164, selfACI := desktopSelf(results["self"].Rows)
	if selfE164 == "" {
		return stats, ErrAccountUnverified
	}
	if selfE164 != facts.SelfE164 {
		return stats, ErrAccountMismatch
	}
	stats.AccountVerified = true

	byConversation, byACI, byE164 := loadContacts(results["conversations"].Rows, facts, selfE164, selfACI)
	if selfACI == "" {
		if self, ok := byE164[selfE164]; ok {
			selfACI = self.aci
		}
	}
	isSelf := func(c *contact) bool {
		return c != nil && ((c.e164 != "" && c.e164 == selfE164) || (c.aci != "" && c.aci == selfACI))
	}
	lookup := func(e164, aci string) *contact {
		if aci != "" {
			if c, ok := byACI[aci]; ok {
				return c
			}
		}
		if e164 != "" {
			if c, ok := byE164[e164]; ok {
				return c
			}
		}
		if e164 == "" && aci == "" {
			return nil
		}
		c := &contact{e164: e164, aci: aci}
		c.address = facts.ChooseAddress(e164, aci)
		return c
	}

	chats := map[string]*chatState{}
	chatByDesktopID := map[string]*chatState{}
	for _, row := range results["conversations"].Rows {
		desktopID := row.str("id")
		state := &chatState{}
		switch row.str("type") {
		case "private":
			c := byConversation[desktopID]
			switch {
			case c == nil || c.address == "":
				state.reason = "unaddressable"
			case isSelf(c):
				state.chat = Chat{RemoteID: "signal:" + selfE164, Participants: []Participant{{Address: selfE164, Name: c.name, IsSelf: true}}}
				state.peer = selfE164
			default:
				state.chat = Chat{RemoteID: "signal:" + c.address, Participants: []Participant{{Address: c.address, Name: c.name}}}
				state.peer = c.address
			}
		case "group":
			groupID := strings.TrimSpace(row.str("group_id"))
			if groupID == "" {
				state.reason = "group_without_id"
				break
			}
			state.chat = Chat{RemoteID: "signal-group:" + groupID, Group: true, Title: strings.TrimSpace(row.str("name"))}
			seen := map[string]bool{}
			for _, member := range groupMembers(row) {
				// membersV2 holds ACIs; the older members column can hold
				// Desktop conversation IDs, which are UUIDs too, so those
				// resolve through the conversation first.
				c := byConversation[member]
				if c == nil {
					c = lookup(NormalizeE164(member), NormalizeACI(member))
				}
				if c == nil || c.address == "" || isSelf(c) || seen[c.address] {
					continue
				}
				seen[c.address] = true
				state.chat.Participants = append(state.chat.Participants, Participant{Address: c.address, Name: c.name})
			}
			sort.Slice(state.chat.Participants, func(i, j int) bool {
				return state.chat.Participants[i].Address < state.chat.Participants[j].Address
			})
		default:
			continue
		}
		chatByDesktopID[desktopID] = state
		if state.reason != "" {
			stats.ChatsSkippedByReason[state.reason]++
			continue
		}
		if existing, ok := chats[state.chat.RemoteID]; ok {
			// Signal Desktop can hold two rows for one person until it merges
			// them. They share one v2 thread; keep the first row's snapshot.
			chatByDesktopID[desktopID] = existing
			stats.ChatsSkippedByReason["merged_duplicate"]++
			continue
		}
		chats[state.chat.RemoteID] = state
		stats.ChatsResolved++
	}
	skipAmbiguousGroupRosters(chats, facts, &stats)

	attachments, source := loadAttachments(results)
	stats.AttachmentSource = source

	for _, row := range results["messages"].Rows {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		rowID := row.int("row_id")
		if rowID > stats.HighWaterRowID {
			stats.HighWaterRowID = rowID
		}
		stats.RowsScanned++
		sentAt := row.int("sent_at")
		since := options.SinceMS > 0 && sentAt >= options.SinceMS
		if since {
			stats.RowsSince++
		}
		skip := func(reason string) {
			stats.RowsSkippedByReason[reason]++
			if since {
				stats.RowsSinceSkippedByReason[reason]++
			}
		}
		state := chatByDesktopID[row.str("conversation_id")]
		message, reason := captureMessage(row, state, attachments, lookup, isSelf, byConversation, &stats)
		if reason != "" {
			skip(reason)
			continue
		}
		if heldByV2(facts, state, message, lookup) {
			stats.RowsInV2++
			if since {
				stats.RowsSinceInV2++
			}
			continue
		}
		state.messages = append(state.messages, message)
		stats.RowsIncluded++
		stats.ReactionsIncluded += int64(len(message.Reactions))
		if since {
			stats.RowsSinceIncluded++
		}
	}

	limit := options.ChunkSize
	if limit <= 0 || limit > MaxChunkMessages {
		limit = MaxChunkMessages
	}
	remoteIDs := make([]string, 0, len(chats))
	for remoteID := range chats {
		remoteIDs = append(remoteIDs, remoteID)
	}
	sort.Strings(remoteIDs)
	for _, remoteID := range remoteIDs {
		state := chats[remoteID]
		if len(state.messages) == 0 {
			continue
		}
		sort.Slice(state.messages, func(i, j int) bool {
			a, b := state.messages[i], state.messages[j]
			if a.SentAtMS != b.SentAtMS {
				return a.SentAtMS < b.SentAtMS
			}
			if a.Outgoing != b.Outgoing {
				return !a.Outgoing
			}
			if a.AuthorAddress != b.AuthorAddress {
				return a.AuthorAddress < b.AuthorAddress
			}
			return a.RowID < b.RowID
		})
		for start := 0; start < len(state.messages); start += limit {
			end := min(start+limit, len(state.messages))
			if emit == nil {
				continue
			}
			if err := emit(Chunk{Chat: state.chat, Messages: state.messages[start:end]}); err != nil {
				return stats, fmt.Errorf("append Signal Desktop capture: %w", err)
			}
		}
	}
	return stats, nil
}

func desktopSelf(rows []Row) (e164, aci string) {
	for _, row := range rows {
		value := stripDeviceSuffix(row.str("value"))
		switch row.str("id") {
		case "number_id":
			e164 = NormalizeE164(value)
		case "uuid_id":
			aci = NormalizeACI(value)
		}
	}
	return e164, aci
}

// stripDeviceSuffix turns Signal Desktop's "<address>.<device id>" item value
// into the address.
func stripDeviceSuffix(value string) string {
	value = strings.TrimSpace(value)
	if index := strings.LastIndex(value, "."); index > 0 {
		if _, err := strconv.Atoi(value[index+1:]); err == nil {
			return value[:index]
		}
	}
	return value
}

func loadContacts(rows []Row, facts *Facts, selfE164, selfACI string) (map[string]*contact, map[string]*contact, map[string]*contact) {
	byConversation := map[string]*contact{}
	byACI := map[string]*contact{}
	byE164 := map[string]*contact{}
	for _, row := range rows {
		if row.str("type") != "private" {
			continue
		}
		c := &contact{
			conversationID: row.str("id"),
			e164:           NormalizeE164(row.str("e164")),
			aci:            NormalizeACI(row.str("service_id")),
			name:           displayName(row),
		}
		if c.e164 == selfE164 && c.e164 != "" {
			c.address = selfE164
		} else if c.aci != "" && c.aci == selfACI {
			c.address = selfE164
		} else {
			c.address = facts.ChooseAddress(c.e164, c.aci)
		}
		byConversation[c.conversationID] = c
		// Rows arrive ordered by Desktop conversation ID, so when two rows
		// claim one address the first keeps it, the same way on every scan.
		if c.aci != "" {
			if _, exists := byACI[c.aci]; !exists {
				byACI[c.aci] = c
			}
		}
		if c.e164 != "" {
			if _, exists := byE164[c.e164]; !exists {
				byE164[c.e164] = c
			}
		}
	}
	return byConversation, byACI, byE164
}

// displayName follows the legacy importer: the contact name, then the
// profile's full name, then given plus family name.
func displayName(row Row) string {
	if name := strings.TrimSpace(row.str("name")); name != "" {
		return name
	}
	if full := strings.TrimSpace(row.str("profile_full_name")); full != "" {
		return full
	}
	return strings.TrimSpace(strings.TrimSpace(row.str("profile_name")) + " " + strings.TrimSpace(row.str("profile_family_name")))
}

func groupMembers(row Row) []string {
	var members []string
	if raw := strings.TrimSpace(row.str("members_v2")); raw != "" {
		var entries []struct {
			ACI string `json:"aci"`
		}
		if err := json.Unmarshal([]byte(raw), &entries); err == nil {
			for _, entry := range entries {
				if entry.ACI != "" {
					members = append(members, entry.ACI)
				}
			}
		}
	}
	if len(members) == 0 {
		members = strings.Fields(row.str("members"))
	}
	return members
}

// skipAmbiguousGroupRosters drops every unbound group whose non-empty roster
// another unbound group shares exactly. The worker creates a thread only for
// a roster no existing thread answers to, so with two such groups whichever
// frame landed first would win; skipping both keeps the result independent of
// frame order. A bound group is left to the worker's own rules.
func skipAmbiguousGroupRosters(chats map[string]*chatState, facts *Facts, stats *Stats) {
	byRoster := map[string][]string{}
	for remoteID, state := range chats {
		if !state.chat.Group || len(state.chat.Participants) == 0 || facts.Bound(remoteID) {
			continue
		}
		addresses := make([]string, 0, len(state.chat.Participants))
		for _, participant := range state.chat.Participants {
			addresses = append(addresses, participant.Address)
		}
		key := strings.Join(addresses, "\x1f")
		byRoster[key] = append(byRoster[key], remoteID)
	}
	for _, remoteIDs := range byRoster {
		if len(remoteIDs) < 2 {
			continue
		}
		for _, remoteID := range remoteIDs {
			chats[remoteID].reason = "ambiguous_roster"
			delete(chats, remoteID)
			stats.ChatsResolved--
			stats.ChatsSkippedByReason["ambiguous_roster"]++
		}
	}
}

func loadAttachments(results map[string]Result) (map[string][]Attachment, string) {
	byMessage := map[string][]Attachment{}
	if typed, ok := results["attachments_typed"]; ok && typed.Error == "" {
		for _, row := range typed.Rows {
			if row.str("attachment_type") != "attachment" || row.int("edit_history_index") != -1 {
				continue
			}
			id := row.str("message_id")
			byMessage[id] = append(byMessage[id], Attachment{ContentType: row.str("content_type"), FileName: row.str("file_name")})
		}
		return byMessage, "typed"
	}
	if basic, ok := results["attachments_basic"]; ok && basic.Error == "" {
		for _, row := range basic.Rows {
			id := row.str("message_id")
			byMessage[id] = append(byMessage[id], Attachment{ContentType: row.str("content_type"), FileName: row.str("file_name")})
		}
		return byMessage, "basic"
	}
	return byMessage, "json"
}

var systemTypes = map[string]bool{
	"keychange": true, "verified-change": true, "group-v1-migration": true,
	"group-v2-change": true, "profile-change": true, "timer-notification": true,
	"delivery-issue": true, "universal-timer-notification": true,
	"change-number-notification": true, "contact-removed-notification": true,
	"conversation-merge": true, "title-transition-notification": true,
	"phone-number-discovery": true, "message-request-response-event": true,
	"session-switchover": true, "chat-session-refreshed": true, "call-history": true,
	"group": true, "pinned-message-notification": true, "poll-terminate": true,
}

func captureMessage(
	row Row,
	state *chatState,
	attachments map[string][]Attachment,
	lookup func(e164, aci string) *contact,
	isSelf func(*contact) bool,
	byConversation map[string]*contact,
	stats *Stats,
) (Message, string) {
	if state == nil {
		return Message{}, "chat_missing"
	}
	if state.reason != "" {
		return Message{}, "chat_" + state.reason
	}
	kind := row.str("type")
	switch {
	case kind == "story":
		return Message{}, "story"
	case systemTypes[kind]:
		return Message{}, "system"
	case kind != "incoming" && kind != "outgoing":
		return Message{}, "type_other"
	}
	if row.truthy("deleted_for_everyone") {
		return Message{}, "deleted"
	}
	sentAt := row.int("sent_at")
	if sentAt <= 0 {
		return Message{}, "no_timestamp"
	}
	message := Message{
		RowID:      row.int("row_id"),
		Outgoing:   kind == "outgoing",
		SentAtMS:   sentAt,
		Body:       strings.TrimSpace(row.str("body")),
		Sticker:    row.str("sticker_type") != "" && row.str("sticker_type") != "null",
		Contact:    row.int("contact_count") > 0,
		Payment:    row.str("payment_type") != "" && row.str("payment_type") != "null",
		Poll:       row.str("poll_type") != "" && row.str("poll_type") != "null",
		Preview:    row.int("preview_count") > 0,
		ViewOnce:   row.truthy("view_once"),
		StoryReply: strings.TrimSpace(row.str("story_id")) != "",
	}
	message.Attachments = attachments[row.str("id")]
	if len(message.Attachments) == 0 {
		message.Attachments = jsonAttachments(row.str("json_attachments"))
	}
	if message.Outgoing {
		if !outgoingSent(row.str("send_state")) {
			return Message{}, "outgoing_unsent"
		}
	} else {
		author := lookup(NormalizeE164(row.str("source")), NormalizeACI(row.str("source_service_id")))
		switch {
		case author == nil && !state.chat.Group:
			// Older 1:1 rows can omit the source; the peer is the author.
			author = &contact{address: state.peer}
		case author == nil || author.address == "":
			return Message{}, "unknown_author"
		}
		if isSelf(author) {
			return Message{}, "self_incoming"
		}
		if !state.chat.Group && author.address != state.peer {
			return Message{}, "author_not_peer"
		}
		message.AuthorAddress = author.address
		message.AuthorName = author.name
	}
	if message.Body == "" && len(message.Attachments) == 0 && !message.Sticker && !message.Contact &&
		!message.Payment && !message.Poll && !message.Preview && !message.ViewOnce {
		return Message{}, "empty"
	}
	message.Reactions = captureReactions(row.str("reactions"), state, byConversation, isSelf, stats)
	return message, ""
}

func jsonAttachments(raw string) []Attachment {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var entries []struct {
		ContentType string `json:"contentType"`
		FileName    string `json:"fileName"`
	}
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		return nil
	}
	out := make([]Attachment, 0, len(entries))
	for _, entry := range entries {
		out = append(out, Attachment{ContentType: entry.ContentType, FileName: entry.FileName})
	}
	return out
}

// outgoingSent reports whether an outgoing row reached anyone. Desktop keeps
// messages that failed or are still pending; the live bridge only ever sees
// what was sent. A row with no readable send state is kept.
func outgoingSent(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return true
	}
	var states map[string]struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal([]byte(raw), &states); err != nil || len(states) == 0 {
		return true
	}
	for _, state := range states {
		switch state.Status {
		case "Sent", "Delivered", "Read", "Viewed":
			return true
		}
	}
	return false
}

func captureReactions(raw string, state *chatState, byConversation map[string]*contact, isSelf func(*contact) bool, stats *Stats) []Reaction {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var entries []struct {
		Emoji     string `json:"emoji"`
		FromID    string `json:"fromId"`
		Timestamp int64  `json:"timestamp"`
	}
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		return nil
	}
	var out []Reaction
	seen := map[string]bool{}
	for _, entry := range entries {
		emoji := strings.TrimSpace(entry.Emoji)
		actor := byConversation[entry.FromID]
		switch {
		case emoji == "" || actor == nil || actor.address == "":
			stats.ReactionsSkipped++
			continue
		case isSelf(actor):
			if seen["self"] {
				continue
			}
			seen["self"] = true
			out = append(out, Reaction{IsSelf: true, Emoji: emoji, TimestampMS: entry.Timestamp})
			continue
		case !state.chat.Group && actor.address != state.peer:
			stats.ReactionsSkipped++
			continue
		}
		if seen[actor.address] {
			continue
		}
		seen[actor.address] = true
		out = append(out, Reaction{ActorAddress: actor.address, ActorName: actor.name, Emoji: emoji, TimestampMS: entry.Timestamp})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].IsSelf != out[j].IsSelf {
			return out[i].IsSelf
		}
		return out[i].ActorAddress < out[j].ActorAddress
	})
	return out
}

// heldByV2 reports whether v2 already stores this message under any address
// form the live bridge could have keyed it by.
func heldByV2(facts *Facts, state *chatState, message Message, lookup func(e164, aci string) *contact) bool {
	conversations := []string{state.chat.RemoteID}
	authors := []string{message.AuthorAddress}
	if !state.chat.Group {
		peer := lookup(NormalizeE164(state.peer), NormalizeACI(state.peer))
		if peer != nil {
			conversations = conversations[:0]
			for _, form := range []string{peer.e164, peer.aci, state.peer} {
				if form != "" && !containsString(conversations, "signal:"+form) {
					conversations = append(conversations, "signal:"+form)
				}
			}
		}
	} else if !message.Outgoing {
		author := lookup(NormalizeE164(message.AuthorAddress), NormalizeACI(message.AuthorAddress))
		if author != nil {
			for _, form := range []string{author.e164, author.aci} {
				if form != "" && !containsString(authors, form) {
					authors = append(authors, form)
				}
			}
		}
	}
	timestamp := strconv.FormatInt(message.SentAtMS, 10)
	for _, conversation := range conversations {
		if message.Outgoing {
			if facts.HasMessage(conversation, timestamp) ||
				facts.HasMessage(conversation, v2keys.SignalLocalAlias(conversation, message.SentAtMS)) {
				return true
			}
			continue
		}
		candidates := authors
		if !state.chat.Group {
			// In a 1:1 thread the author is the address the thread is keyed by.
			candidates = []string{strings.TrimPrefix(conversation, "signal:")}
		}
		for _, author := range candidates {
			if facts.HasMessage(conversation, v2keys.SignalIncomingSourceID(conversation, author, message.SentAtMS)) {
				return true
			}
		}
	}
	return false
}

func containsString(values []string, value string) bool {
	for _, existing := range values {
		if existing == value {
			return true
		}
	}
	return false
}

func (r Row) str(column string) string {
	raw := bytes.TrimSpace(r[column])
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return ""
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}
	return string(raw)
}

func (r Row) int(column string) int64 {
	raw := bytes.TrimSpace(r[column])
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return 0
	}
	var number json.Number
	if err := json.Unmarshal(raw, &number); err == nil {
		if value, err := number.Int64(); err == nil {
			return value
		}
		if value, err := number.Float64(); err == nil {
			return int64(value)
		}
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		if value, err := strconv.ParseInt(strings.TrimSpace(text), 10, 64); err == nil {
			return value
		}
	}
	return 0
}

func (r Row) truthy(column string) bool {
	raw := bytes.TrimSpace(r[column])
	switch string(raw) {
	case "", "null", "0", "false", `""`, `"0"`, `"false"`:
		return false
	}
	return true
}
