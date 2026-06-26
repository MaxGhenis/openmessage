package db

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// messageColumns is the canonical column list for SELECT queries on messages.
const messageColumns = `message_id, conversation_id, sender_name, sender_number, body, timestamp_ms, status, is_from_me, mentions_me, media_id, mime_type, decryption_key, reactions, reply_to_id, source_platform, source_id, transcript, transcribed_at, transcript_model`

var ErrMessageNotFound = errors.New("message not found")

const (
	MaxTranscriptBytes      = 64 << 10
	MaxTranscriptModelBytes = 128
)

func (s *Store) UpsertMessage(m *Message) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if err := upsertMessageTx(tx, m); err != nil {
		return err
	}
	body, err := messageSearchBodyTx(tx, m.MessageID)
	if err != nil {
		return err
	}
	if err := s.syncMessageSearchIndex(tx, m.MessageID, body); err != nil {
		return err
	}
	return tx.Commit()
}

func upsertMessageTx(tx *sql.Tx, m *Message) error {
	if strings.TrimSpace(m.SourcePlatform) == "" {
		m.SourcePlatform = "sms"
	}
	// On conflict we must NOT blindly overwrite content columns with the
	// incoming row: the live bridges re-deliver the same message_id for
	// status-only updates (delivery/read receipts), where media_id /
	// decryption_key / reactions / body come back empty. Overwriting then
	// permanently wipes media references and reactions on an already-complete
	// row. So for content fields, keep the existing value when the incoming
	// one is empty (an edit carries a non-empty body and still updates).
	// Volatile fields (status, is_from_me, mentions_me, timestamp) are
	// last-writer-wins because that's exactly what a status update changes.
	_, err := tx.Exec(`
		INSERT INTO messages (message_id, conversation_id, sender_name, sender_number, body, timestamp_ms, status, is_from_me, mentions_me, media_id, mime_type, decryption_key, reactions, reply_to_id, source_platform, source_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(message_id) DO UPDATE SET
			conversation_id=excluded.conversation_id,
			sender_name=CASE WHEN excluded.sender_name != '' THEN excluded.sender_name ELSE messages.sender_name END,
			sender_number=CASE WHEN excluded.sender_number != '' THEN excluded.sender_number ELSE messages.sender_number END,
			body=CASE WHEN excluded.body != '' THEN excluded.body ELSE messages.body END,
			timestamp_ms=CASE WHEN excluded.timestamp_ms > 0 THEN excluded.timestamp_ms ELSE messages.timestamp_ms END,
			status=excluded.status,
			is_from_me=excluded.is_from_me,
			mentions_me=excluded.mentions_me,
			media_id=CASE WHEN excluded.media_id != '' THEN excluded.media_id ELSE messages.media_id END,
			mime_type=CASE WHEN excluded.mime_type != '' THEN excluded.mime_type ELSE messages.mime_type END,
			decryption_key=CASE WHEN excluded.decryption_key != '' THEN excluded.decryption_key ELSE messages.decryption_key END,
			reactions=CASE WHEN excluded.reactions != '' THEN excluded.reactions ELSE messages.reactions END,
			reply_to_id=CASE WHEN excluded.reply_to_id != '' THEN excluded.reply_to_id ELSE messages.reply_to_id END,
			source_platform=excluded.source_platform,
			source_id=CASE WHEN excluded.source_id != '' THEN excluded.source_id ELSE messages.source_id END
	`, m.MessageID, m.ConversationID, m.SenderName, m.SenderNumber, m.Body, m.TimestampMS, m.Status, m.IsFromMe, m.MentionsMe, m.MediaID, m.MimeType, m.DecryptionKey, m.Reactions, m.ReplyToID, m.SourcePlatform, m.SourceID)
	if err != nil {
		return err
	}
	if protocol := DisplayProtocolFromStatus(m.Status); protocol != "" && strings.TrimSpace(m.ConversationID) != "" {
		if _, err := tx.Exec(`UPDATE conversations SET display_protocol = ? WHERE conversation_id = ?`, protocol, m.ConversationID); err != nil {
			return err
		}
	}
	return nil
}

func DisplayProtocolFromStatus(status string) string {
	upper := strings.ToUpper(strings.TrimSpace(status))
	if upper == "" {
		return ""
	}
	if strings.Contains(upper, "RCS") {
		return "RCS"
	}
	if strings.Contains(upper, "TEXT") || strings.Contains(upper, "SMS") {
		return "Text"
	}
	return ""
}

// UpdateMessageReactions explicitly sets the reactions JSON for a message,
// including clearing it (empty string) when the last reaction is removed.
// Reaction changes go through this dedicated path rather than UpsertMessage
// because UpsertMessage deliberately preserves existing reactions when handed
// an empty value (to survive status-only re-deliveries) — which would
// otherwise make reaction removal impossible. Reactions are not part of the
// FTS body, so no search-index sync is needed.
func (s *Store) UpdateMessageReactions(messageID, reactions string) error {
	_, err := s.db.Exec(`UPDATE messages SET reactions = ? WHERE message_id = ?`, reactions, messageID)
	return err
}

func (s *Store) RecordOutgoingMessage(m *Message, deleteDraftID string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if err := upsertMessageTx(tx, m); err != nil {
		return err
	}
	body, err := messageSearchBodyTx(tx, m.MessageID)
	if err != nil {
		return err
	}
	if err := s.syncMessageSearchIndex(tx, m.MessageID, body); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE conversations SET last_message_ts = ? WHERE conversation_id = ?`, m.TimestampMS, m.ConversationID); err != nil {
		return err
	}
	if deleteDraftID != "" {
		if _, err := tx.Exec(`DELETE FROM drafts WHERE draft_id = ?`, deleteDraftID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func messageSearchBodyTx(tx *sql.Tx, messageID string) (string, error) {
	var body string
	if err := tx.QueryRow(`SELECT body FROM messages WHERE message_id = ?`, messageID).Scan(&body); err != nil {
		return "", err
	}
	return body, nil
}

func (s *Store) GetMessagesByConversation(conversationID string, limit int) ([]*Message, error) {
	rows, err := s.db.Query(`
		SELECT `+messageColumns+`
		FROM messages
		WHERE conversation_id = ?
		ORDER BY timestamp_ms DESC, message_id DESC
		LIMIT ?
	`, conversationID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMessages(rows)
}

func (s *Store) GetMessagesByConversationBefore(conversationID string, beforeMS int64, beforeID string, limit int) ([]*Message, error) {
	query := `
		SELECT ` + messageColumns + `
		FROM messages
		WHERE conversation_id = ? AND timestamp_ms < ?
		ORDER BY timestamp_ms DESC, message_id DESC
		LIMIT ?
	`
	args := []any{conversationID, beforeMS, limit}
	if beforeID != "" {
		query = `
		SELECT ` + messageColumns + `
		FROM messages
		WHERE conversation_id = ? AND (timestamp_ms < ? OR (timestamp_ms = ? AND message_id < ?))
		ORDER BY timestamp_ms DESC, message_id DESC
		LIMIT ?
	`
		args = []any{conversationID, beforeMS, beforeMS, beforeID, limit}
	}
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMessages(rows)
}

func (s *Store) GetMessagesByConversationAfter(conversationID string, afterMS int64, afterID string, limit int) ([]*Message, error) {
	query := `
		SELECT ` + messageColumns + `
		FROM messages
		WHERE conversation_id = ? AND timestamp_ms > ?
		ORDER BY timestamp_ms ASC, message_id ASC
		LIMIT ?
	`
	args := []any{conversationID, afterMS, limit}
	if afterID != "" {
		query = `
		SELECT ` + messageColumns + `
		FROM messages
		WHERE conversation_id = ? AND (timestamp_ms > ? OR (timestamp_ms = ? AND message_id > ?))
		ORDER BY timestamp_ms ASC, message_id ASC
		LIMIT ?
	`
		args = []any{conversationID, afterMS, afterMS, afterID, limit}
	}
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMessages(rows)
}

func (s *Store) GetMessages(phoneNumber string, afterMS, beforeMS int64, limit int) ([]*Message, error) {
	var conditions []string
	var args []any

	if phoneNumber != "" {
		conditions = append(conditions, "sender_number = ?")
		args = append(args, phoneNumber)
	}
	if afterMS > 0 {
		conditions = append(conditions, "timestamp_ms >= ?")
		args = append(args, afterMS)
	}
	if beforeMS > 0 {
		conditions = append(conditions, "timestamp_ms <= ?")
		args = append(args, beforeMS)
	}

	query := `SELECT ` + messageColumns + ` FROM messages`
	if len(conditions) > 0 {
		query += " WHERE " + strings.Join(conditions, " AND ")
	}
	query += " ORDER BY timestamp_ms DESC LIMIT ?"
	args = append(args, limit)

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("query messages: %w", err)
	}
	defer rows.Close()
	return scanMessages(rows)
}

// SearchFilter narrows a text search beyond the query string. Zero values mean
// "no constraint": empty Phone, zero SinceMS/UntilMS, and Limit<=0 → default.
type SearchFilter struct {
	Phone          string // restrict to this sender number
	ConversationID string // restrict to one conversation
	SinceMS        int64  // only messages at/after this ms (0 = no lower bound)
	UntilMS        int64  // only messages at/before this ms (0 = no upper bound)
	Limit          int    // max rows (<=0 → 20)
}

func (s *Store) SearchMessages(query, phoneNumber string, limit int) ([]*Message, error) {
	return s.SearchMessagesFiltered(query, SearchFilter{Phone: phoneNumber, Limit: limit})
}

// SearchMessagesFiltered runs a text search constrained by f. It tries FTS first
// (when enabled) and falls back to a LIKE scan if FTS is unavailable or finds
// nothing. The same filter conditions are applied on both paths so results are
// consistent regardless of which backend answers.
func (s *Store) SearchMessagesFiltered(query string, f SearchFilter) ([]*Message, error) {
	if f.Limit <= 0 {
		f.Limit = 20
	}
	if f.SinceMS > 0 && f.UntilMS > 0 && f.UntilMS < f.SinceMS {
		f.SinceMS, f.UntilMS = f.UntilMS, f.SinceMS
	}
	if s.ftsEnabled {
		msgs, err := s.searchMessagesFTS(query, f)
		if err == nil && len(msgs) > 0 {
			return msgs, nil
		}
	}
	return s.searchMessagesLike(query, f)
}

func (s *Store) searchMessagesFTS(query string, f SearchFilter) ([]*Message, error) {
	conditions := []string{"f.body MATCH ?"}
	args := []any{`"` + strings.ReplaceAll(query, `"`, `""`) + `"`}

	if f.Phone != "" {
		conditions = append(conditions, "m.sender_number = ?")
		args = append(args, f.Phone)
	}
	if f.ConversationID != "" {
		conditions = append(conditions, "m.conversation_id = ?")
		args = append(args, f.ConversationID)
	}
	if f.SinceMS > 0 {
		conditions = append(conditions, "m.timestamp_ms >= ?")
		args = append(args, f.SinceMS)
	}
	if f.UntilMS > 0 {
		conditions = append(conditions, "m.timestamp_ms <= ?")
		args = append(args, f.UntilMS)
	}
	args = append(args, f.Limit)

	q := `SELECT m.` + messageColumns + `
		FROM messages_fts f
		JOIN messages m ON m.message_id = f.message_id
		WHERE ` + strings.Join(conditions, " AND ") + `
		ORDER BY m.timestamp_ms DESC, m.message_id DESC
		LIMIT ?`

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMessages(rows)
}

func (s *Store) searchMessagesLike(query string, f SearchFilter) ([]*Message, error) {
	conditions := []string{"body LIKE ?"}
	args := []any{"%" + query + "%"}

	if f.Phone != "" {
		conditions = append(conditions, "sender_number = ?")
		args = append(args, f.Phone)
	}
	if f.ConversationID != "" {
		conditions = append(conditions, "conversation_id = ?")
		args = append(args, f.ConversationID)
	}
	if f.SinceMS > 0 {
		conditions = append(conditions, "timestamp_ms >= ?")
		args = append(args, f.SinceMS)
	}
	if f.UntilMS > 0 {
		conditions = append(conditions, "timestamp_ms <= ?")
		args = append(args, f.UntilMS)
	}

	q := `SELECT ` + messageColumns + ` FROM messages WHERE ` +
		strings.Join(conditions, " AND ") +
		` ORDER BY timestamp_ms DESC, message_id DESC LIMIT ?`
	args = append(args, f.Limit)

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMessages(rows)
}

func (s *Store) GetMessageByID(messageID string) (*Message, error) {
	row := s.db.QueryRow(`
		SELECT `+messageColumns+`
		FROM messages WHERE message_id = ?
	`, messageID)
	m := &Message{}
	err := row.Scan(&m.MessageID, &m.ConversationID, &m.SenderName, &m.SenderNumber, &m.Body, &m.TimestampMS, &m.Status, &m.IsFromMe, &m.MentionsMe, &m.MediaID, &m.MimeType, &m.DecryptionKey, &m.Reactions, &m.ReplyToID, &m.SourcePlatform, &m.SourceID, &m.Transcript, &m.TranscribedAtMS, &m.TranscriptModel)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return m, nil
}

func (s *Store) GetMessagesByConversationAtTimestamp(conversationID string, timestampMS int64, limit int) ([]*Message, error) {
	if limit <= 0 {
		limit = 10
	}
	rows, err := s.db.Query(`
		SELECT `+messageColumns+`
		FROM messages
		WHERE conversation_id = ? AND timestamp_ms = ?
		ORDER BY message_id ASC
		LIMIT ?
	`, conversationID, timestampMS, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMessages(rows)
}

func (s *Store) GetMessagesByConversationBetween(conversationID string, startMS, endMS int64, limit int) ([]*Message, error) {
	if limit <= 0 {
		limit = 25
	}
	if endMS < startMS {
		startMS, endMS = endMS, startMS
	}
	rows, err := s.db.Query(`
		SELECT `+messageColumns+`
		FROM messages
		WHERE conversation_id = ?
			AND timestamp_ms >= ?
			AND timestamp_ms <= ?
		ORDER BY timestamp_ms ASC, message_id ASC
		LIMIT ?
	`, conversationID, startMS, endMS, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMessages(rows)
}

func (s *Store) GetMessagesAroundMessage(conversationID, messageID string, before, after int) ([]*Message, error) {
	if before < 0 {
		before = 0
	}
	if after < 0 {
		after = 0
	}
	if before == 0 {
		before = 40
	}
	if after == 0 {
		after = 40
	}
	anchor, err := s.GetMessageByID(messageID)
	if err != nil {
		return nil, err
	}
	if anchor == nil {
		return nil, ErrMessageNotFound
	}
	if anchor.ConversationID != conversationID {
		return nil, ErrMessageNotFound
	}
	beforeRows, err := s.db.Query(`
		SELECT `+messageColumns+`
		FROM messages
		WHERE conversation_id = ?
			AND (timestamp_ms < ? OR (timestamp_ms = ? AND message_id < ?))
		ORDER BY timestamp_ms DESC, message_id DESC
		LIMIT ?
	`, conversationID, anchor.TimestampMS, anchor.TimestampMS, anchor.MessageID, before)
	if err != nil {
		return nil, err
	}
	beforeMsgs, err := scanMessages(beforeRows)
	beforeRows.Close()
	if err != nil {
		return nil, err
	}
	afterRows, err := s.db.Query(`
		SELECT `+messageColumns+`
		FROM messages
		WHERE conversation_id = ?
			AND (timestamp_ms > ? OR (timestamp_ms = ? AND message_id > ?))
		ORDER BY timestamp_ms ASC, message_id ASC
		LIMIT ?
	`, conversationID, anchor.TimestampMS, anchor.TimestampMS, anchor.MessageID, after)
	if err != nil {
		return nil, err
	}
	afterMsgs, err := scanMessages(afterRows)
	afterRows.Close()
	if err != nil {
		return nil, err
	}
	result := make([]*Message, 0, len(beforeMsgs)+1+len(afterMsgs))
	for i := len(beforeMsgs) - 1; i >= 0; i-- {
		result = append(result, beforeMsgs[i])
	}
	result = append(result, anchor)
	result = append(result, afterMsgs...)
	return result, nil
}

func (s *Store) ListLegacyWhatsAppMediaPlaceholders(limit int) ([]*Message, error) {
	rows, err := s.db.Query(`
		SELECT `+messageColumns+`
		FROM messages
		WHERE source_platform = 'whatsapp'
			AND body IN ('[Photo]', '[Video]', '[Audio]', '[Voice note]', '[Sticker]')
			AND IFNULL(media_id, '') = ''
		ORDER BY timestamp_ms DESC, message_id DESC
		LIMIT ?
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMessages(rows)
}

func (s *Store) FindUnresolvedWhatsAppPlaceholderAlias(conversationID string, timestampMS int64, body, sourceID string) (*Message, error) {
	rows, err := s.db.Query(`
		SELECT `+messageColumns+`
		FROM messages
		WHERE source_platform = 'whatsapp'
			AND conversation_id = ?
			AND timestamp_ms = ?
			AND body = ?
			AND IFNULL(media_id, '') = ''
			AND IFNULL(source_id, '') <> ?
		ORDER BY message_id ASC
		LIMIT 2
	`, conversationID, timestampMS, strings.TrimSpace(body), strings.TrimSpace(sourceID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	messages, err := scanMessages(rows)
	if err != nil {
		return nil, err
	}
	if len(messages) != 1 {
		return nil, nil
	}
	return messages[0], nil
}

// GetMessagesByConversations returns messages from multiple conversations,
// ordered by timestamp ascending. Useful for cross-platform person queries.
func (s *Store) GetMessagesByConversations(conversationIDs []string, limit int) ([]*Message, error) {
	if len(conversationIDs) == 0 {
		return nil, nil
	}
	placeholders := make([]string, len(conversationIDs))
	args := make([]any, len(conversationIDs))
	for i, id := range conversationIDs {
		placeholders[i] = "?"
		args[i] = id
	}
	args = append(args, limit)

	rows, err := s.db.Query(`
		SELECT `+messageColumns+`
		FROM (
			SELECT `+messageColumns+`
			FROM messages
			WHERE conversation_id IN (`+strings.Join(placeholders, ",")+`)
			ORDER BY timestamp_ms DESC, message_id DESC
			LIMIT ?
		)
		ORDER BY timestamp_ms ASC, message_id ASC
	`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMessages(rows)
}

// GetMessagesByConversationsRange returns messages from multiple conversations
// within a time range, ordered by timestamp ascending.
func (s *Store) GetMessagesByConversationsRange(conversationIDs []string, afterMS, beforeMS int64, limit int) ([]*Message, error) {
	if len(conversationIDs) == 0 {
		return nil, nil
	}
	placeholders := make([]string, len(conversationIDs))
	args := make([]any, len(conversationIDs))
	for i, id := range conversationIDs {
		placeholders[i] = "?"
		args[i] = id
	}

	conditions := "conversation_id IN (" + strings.Join(placeholders, ",") + ")"
	if afterMS > 0 {
		conditions += " AND timestamp_ms >= ?"
		args = append(args, afterMS)
	}
	if beforeMS > 0 {
		conditions += " AND timestamp_ms <= ?"
		args = append(args, beforeMS)
	}
	args = append(args, limit)

	rows, err := s.db.Query(`
		SELECT `+messageColumns+`
		FROM (
			SELECT `+messageColumns+`
			FROM messages
			WHERE `+conditions+`
			ORDER BY timestamp_ms DESC, message_id DESC
			LIMIT ?
		)
		ORDER BY timestamp_ms ASC, message_id ASC
	`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMessages(rows)
}

// DeleteTmpMessages removes locally-created tmp_ messages for a conversation.
// Called when the server echo arrives with a real message ID.
func (s *Store) DeleteTmpMessages(conversationID string) (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	result, err := s.deleteMessages(tx, `conversation_id = ? AND message_id LIKE 'tmp_%'`, conversationID)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func (s *Store) DeleteMessageByID(messageID string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := s.deleteMessages(tx, `message_id = ?`, messageID); err != nil {
		return err
	}
	return tx.Commit()
}

// MessageCount returns the total number of messages, optionally filtered by source platform.
func (s *Store) MessageCount(sourcePlatform string) (int, error) {
	var count int
	var err error
	if sourcePlatform != "" {
		err = s.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE source_platform = ?`, sourcePlatform).Scan(&count)
	} else {
		err = s.db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&count)
	}
	return count, err
}

// LatestTimestamp returns the most recent timestamp_ms for a given source platform.
// Returns 0 if no messages exist for that platform.
func (s *Store) LatestTimestamp(sourcePlatform string) (int64, error) {
	var ts sql.NullInt64
	err := s.db.QueryRow(
		`SELECT MAX(timestamp_ms) FROM messages WHERE source_platform = ?`,
		sourcePlatform,
	).Scan(&ts)
	if err != nil || !ts.Valid {
		return 0, err
	}
	return ts.Int64, nil
}

// LatestReceivedTimestamp returns the most recent timestamp_ms for incoming
// messages on the given source platform. Used by incremental importers so
// that a user's own recent outgoing timestamp doesn't hide gaps in incoming
// coverage (e.g. a reply sent from the phone that was received by Signal
// Desktop but missed by signal-cli's live WebSocket during a restart).
// Returns 0 if no incoming messages exist for that platform.
func (s *Store) LatestReceivedTimestamp(sourcePlatform string) (int64, error) {
	var ts sql.NullInt64
	err := s.db.QueryRow(
		`SELECT MAX(timestamp_ms) FROM messages WHERE source_platform = ? AND is_from_me = 0`,
		sourcePlatform,
	).Scan(&ts)
	if err != nil || !ts.Valid {
		return 0, err
	}
	return ts.Int64, nil
}

func (s *Store) LatestConversationPreviews(conversationIDs []string) (map[string]string, error) {
	ids := make([]string, 0, len(conversationIDs))
	seen := map[string]struct{}{}
	for _, id := range conversationIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	previews := make(map[string]string, len(ids))
	if len(ids) == 0 {
		return previews, nil
	}

	stmt, err := s.db.Prepare(`
		SELECT body, media_id, mime_type, is_from_me
		FROM messages
		WHERE conversation_id = ?
		ORDER BY timestamp_ms DESC, message_id DESC
		LIMIT 1
	`)
	if err != nil {
		return nil, err
	}
	defer stmt.Close()

	for _, conversationID := range ids {
		var body, mediaID, mimeType string
		var isFromMe bool
		err := stmt.QueryRow(conversationID).Scan(&body, &mediaID, &mimeType, &isFromMe)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		previews[conversationID] = formatLastMessagePreview(body, mediaID, mimeType, isFromMe)
	}
	return previews, nil
}

func formatLastMessagePreview(body, mediaID, mimeType string, isFromMe bool) string {
	const previewRuneLimit = 120

	preview := strings.Join(strings.Fields(body), " ")
	if preview == "" && strings.TrimSpace(mediaID) != "" {
		switch {
		case strings.HasPrefix(strings.ToLower(mimeType), "image/"):
			preview = "Photo"
		case strings.HasPrefix(strings.ToLower(mimeType), "video/"):
			preview = "Video"
		case strings.HasPrefix(strings.ToLower(mimeType), "audio/"):
			preview = "Audio"
		default:
			preview = "Attachment"
		}
	}
	if preview == "" {
		return ""
	}
	if isFromMe {
		preview = "You: " + preview
	}
	return truncateRunes(preview, previewRuneLimit)
}

func truncateRunes(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit-1]) + "…"
}

// PlatformStat summarizes stored message coverage for one source platform.
type PlatformStat struct {
	Platform     string
	Count        int
	LatestMS     int64 // most recent message (sent or received), 0 if none
	LatestRecvMS int64 // most recent received message, 0 if none
}

// PlatformStats returns per-platform message counts and the latest sent/received
// timestamps in a single GROUP BY pass, ordered most-recent-activity first.
//
// It powers "openmessage status": a one-shot freshness check that reveals stale
// coverage (a platform that stopped syncing) without starting any live
// transports. Tracking received separately matters because your own outgoing
// timestamps can mask gaps in incoming coverage. Blank/unknown platforms are
// bucketed under "unknown" rather than dropped.
func (s *Store) PlatformStats() ([]PlatformStat, error) {
	rows, err := s.db.Query(`
		SELECT
			source_platform,
			COUNT(*),
			COALESCE(MAX(timestamp_ms), 0),
			COALESCE(MAX(CASE WHEN is_from_me = 0 THEN timestamp_ms END), 0)
		FROM messages
		GROUP BY source_platform
		ORDER BY 3 DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var stats []PlatformStat
	for rows.Next() {
		var st PlatformStat
		var platform sql.NullString
		if err := rows.Scan(&platform, &st.Count, &st.LatestMS, &st.LatestRecvMS); err != nil {
			return nil, err
		}
		st.Platform = strings.TrimSpace(platform.String)
		if st.Platform == "" {
			st.Platform = "unknown"
		}
		stats = append(stats, st)
	}
	return stats, rows.Err()
}

func scanMessages(rows interface {
	Next() bool
	Scan(...any) error
	Err() error
}) ([]*Message, error) {
	var msgs []*Message
	for rows.Next() {
		m := &Message{}
		if err := rows.Scan(&m.MessageID, &m.ConversationID, &m.SenderName, &m.SenderNumber, &m.Body, &m.TimestampMS, &m.Status, &m.IsFromMe, &m.MentionsMe, &m.MediaID, &m.MimeType, &m.DecryptionKey, &m.Reactions, &m.ReplyToID, &m.SourcePlatform, &m.SourceID, &m.Transcript, &m.TranscribedAtMS, &m.TranscriptModel); err != nil {
			return nil, err
		}
		msgs = append(msgs, m)
	}
	return msgs, rows.Err()
}

// SetMessageTranscript writes a transcript for an existing message. It does
// not modify the message's body, media_id, or mime_type.
func (s *Store) SetMessageTranscript(messageID, transcript string, model *string) error {
	if messageID == "" {
		return fmt.Errorf("SetMessageTranscript: empty message_id")
	}
	if err := ValidateMessageTranscript(transcript, model); err != nil {
		return err
	}
	msg, err := s.GetMessageByID(messageID)
	if err != nil {
		return fmt.Errorf("SetMessageTranscript: get message: %w", err)
	}
	if msg == nil {
		return ErrMessageNotFound
	}

	nowMS := msg.TranscribedAtMS
	modelToSave := msg.TranscriptModel
	if model != nil {
		modelToSave = *model
	}
	if transcript == "" {
		if msg.Transcript == "" && msg.TranscribedAtMS == 0 && msg.TranscriptModel == "" {
			return nil
		}
		nowMS = 0
		modelToSave = ""
	} else {
		if msg.Transcript == transcript && msg.TranscriptModel == modelToSave && msg.TranscribedAtMS != 0 {
			return nil
		}
		nowMS = time.Now().UnixMilli()
		if nowMS <= msg.TranscribedAtMS {
			nowMS = msg.TranscribedAtMS + 1
		}
	}
	res, err := s.db.Exec(`
		UPDATE messages
		   SET transcript = ?, transcribed_at = ?, transcript_model = ?
		 WHERE message_id = ?
	`, transcript, nowMS, modelToSave, messageID)
	if err != nil {
		return fmt.Errorf("SetMessageTranscript: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("SetMessageTranscript: rows affected: %w", err)
	}
	if n == 0 {
		return ErrMessageNotFound
	}
	return nil
}

func ValidateMessageTranscript(transcript string, model *string) error {
	if len(transcript) > MaxTranscriptBytes {
		return fmt.Errorf("transcript exceeds %d bytes", MaxTranscriptBytes)
	}
	if model != nil && len(*model) > MaxTranscriptModelBytes {
		return fmt.Errorf("transcript model exceeds %d bytes", MaxTranscriptModelBytes)
	}
	return nil
}

func (s *Store) syncMessageSearchIndex(exec interface {
	Exec(string, ...any) (sql.Result, error)
}, messageID, body string) error {
	if !s.ftsEnabled {
		return nil
	}
	if _, err := exec.Exec(`DELETE FROM messages_fts WHERE message_id = ?`, messageID); err != nil {
		return err
	}
	_, err := exec.Exec(`INSERT INTO messages_fts(message_id, body) VALUES (?, ?)`, messageID, body)
	return err
}

func (s *Store) deleteMessages(tx *sql.Tx, where string, args ...any) (sql.Result, error) {
	if s.ftsEnabled {
		if _, err := tx.Exec(`DELETE FROM messages_fts WHERE message_id IN (SELECT message_id FROM messages WHERE `+where+`)`, args...); err != nil {
			return nil, err
		}
	}
	return tx.Exec(`DELETE FROM messages WHERE `+where, args...)
}
