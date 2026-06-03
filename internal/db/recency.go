package db

import "strings"

// MessageHasContent reports whether a message carries real conversational
// content. Empty placeholder/reaction-artifact messages — e.g. an emoji
// reaction made in a group that arrives as an empty stub in the reactor's 1:1
// thread, read receipts, or auto-download placeholders — have no body, media,
// or reactions of their own and must never advance a conversation's recency.
func MessageHasContent(m *Message) bool {
	if m == nil {
		return false
	}
	return strings.TrimSpace(m.Body) != "" ||
		strings.TrimSpace(m.MediaID) != "" ||
		strings.TrimSpace(m.Reactions) != ""
}

// AdvanceConversationRecency raises a conversation's last_message_ts to the
// message's timestamp, but only when the message carries real content (see
// MessageHasContent) and only ever forwards in time. Contentless messages are
// ignored so they cannot float a conversation to the top of the inbox.
func (s *Store) AdvanceConversationRecency(m *Message) error {
	if !MessageHasContent(m) {
		return nil
	}
	return s.BumpConversationTimestamp(m.ConversationID, m.TimestampMS)
}

// RepairContentlessRecency repairs conversations already corrupted by a
// contentless message having advanced their recency. For each conversation
// whose newest stored message is contentless AND is currently setting the
// conversation's last_message_ts, it lowers last_message_ts to the newest
// content-bearing message.
//
// Conversations whose recency comes from metadata (last_message_ts newer than
// any stored message — typical when recent history hasn't been backfilled yet)
// are intentionally left untouched, because that recency is legitimate. It
// returns the number of conversations changed.
func (s *Store) RepairContentlessRecency() (int, error) {
	const contentPredicate = `(TRIM(body) != '' OR TRIM(COALESCE(media_id,'')) != '' OR TRIM(COALESCE(reactions,'')) != '')`
	res, err := s.db.Exec(`
		UPDATE conversations
		SET last_message_ts = (
			SELECT MAX(timestamp_ms) FROM messages m
			WHERE m.conversation_id = conversations.conversation_id AND ` + contentPredicate + `
		)
		WHERE
			-- a stored message is currently setting recency...
			last_message_ts = (
				SELECT MAX(timestamp_ms) FROM messages m
				WHERE m.conversation_id = conversations.conversation_id
			)
			-- ...and that newest message is contentless (its ts is above the newest content message)...
			AND last_message_ts > (
				SELECT MAX(timestamp_ms) FROM messages m
				WHERE m.conversation_id = conversations.conversation_id AND ` + contentPredicate + `
			)
			-- ...and at least one content-bearing message exists to drop down to.
			AND EXISTS (
				SELECT 1 FROM messages m
				WHERE m.conversation_id = conversations.conversation_id AND ` + contentPredicate + `
			)
	`)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}
