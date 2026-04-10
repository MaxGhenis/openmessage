package db

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// messageColumns is the canonical column list for SELECT queries on messages.
const messageColumns = `message_id, conversation_id, sender_name, sender_number, body, timestamp_ms, status, is_from_me, mentions_me, media_id, mime_type, decryption_key, reactions, reply_to_id, source_platform, source_id`

func (s *Store) UpsertMessage(m *Message) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if err := upsertMessageTx(tx, m); err != nil {
		return err
	}
	if err := s.syncMessageSearchIndex(tx, m.MessageID, m.Body); err != nil {
		return err
	}
	return tx.Commit()
}

func upsertMessageTx(tx *sql.Tx, m *Message) error {
	_, err := tx.Exec(`
		INSERT INTO messages (message_id, conversation_id, sender_name, sender_number, body, timestamp_ms, status, is_from_me, mentions_me, media_id, mime_type, decryption_key, reactions, reply_to_id, source_platform, source_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(message_id) DO UPDATE SET
			conversation_id=excluded.conversation_id,
			sender_name=excluded.sender_name,
			sender_number=excluded.sender_number,
			body=excluded.body,
			timestamp_ms=excluded.timestamp_ms,
			status=excluded.status,
			is_from_me=excluded.is_from_me,
			mentions_me=excluded.mentions_me,
			media_id=excluded.media_id,
			mime_type=excluded.mime_type,
			decryption_key=excluded.decryption_key,
			reactions=excluded.reactions,
			reply_to_id=excluded.reply_to_id,
			source_platform=excluded.source_platform,
			source_id=excluded.source_id
	`, m.MessageID, m.ConversationID, m.SenderName, m.SenderNumber, m.Body, m.TimestampMS, m.Status, m.IsFromMe, m.MentionsMe, m.MediaID, m.MimeType, m.DecryptionKey, m.Reactions, m.ReplyToID, m.SourcePlatform, m.SourceID)
	if err != nil {
		return err
	}
	return nil
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
	if err := s.syncMessageSearchIndex(tx, m.MessageID, m.Body); err != nil {
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

func (s *Store) SearchMessages(query, phoneNumber string, limit int) ([]*Message, error) {
	if s.ftsEnabled {
		msgs, err := s.searchMessagesFTS(query, phoneNumber, limit)
		if err == nil && len(msgs) > 0 {
			return msgs, nil
		}
	}
	return s.searchMessagesLike(query, phoneNumber, limit)
}

func (s *Store) searchMessagesFTS(query, phoneNumber string, limit int) ([]*Message, error) {
	var conditions []string
	var args []any

	conditions = append(conditions, "f.body MATCH ?")
	args = append(args, `"`+strings.ReplaceAll(query, `"`, `""`)+`"`)

	if phoneNumber != "" {
		conditions = append(conditions, "m.sender_number = ?")
		args = append(args, phoneNumber)
	}
	args = append(args, limit)

	q := `SELECT m.` + messageColumns + `
		FROM messages_fts f
		JOIN messages m ON m.message_id = f.message_id
		WHERE ` + strings.Join(conditions, " AND ") + `
		ORDER BY m.timestamp_ms DESC
		LIMIT ?`

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMessages(rows)
}

func (s *Store) searchMessagesLike(query, phoneNumber string, limit int) ([]*Message, error) {
	var conditions []string
	var args []any

	conditions = append(conditions, "body LIKE ?")
	args = append(args, "%"+query+"%")

	if phoneNumber != "" {
		conditions = append(conditions, "sender_number = ?")
		args = append(args, phoneNumber)
	}

	q := `SELECT ` + messageColumns + ` FROM messages`
	if len(conditions) > 0 {
		q += " WHERE " + strings.Join(conditions, " AND ")
	}
	q += " ORDER BY timestamp_ms DESC LIMIT ?"
	args = append(args, limit)

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
	err := row.Scan(&m.MessageID, &m.ConversationID, &m.SenderName, &m.SenderNumber, &m.Body, &m.TimestampMS, &m.Status, &m.IsFromMe, &m.MentionsMe, &m.MediaID, &m.MimeType, &m.DecryptionKey, &m.Reactions, &m.ReplyToID, &m.SourcePlatform, &m.SourceID)
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

func (s *Store) ListLegacyWhatsAppMediaPlaceholders(limit int) ([]*Message, error) {
	rows, err := s.db.Query(`
		SELECT `+messageColumns+`
		FROM messages
		WHERE source_platform = 'whatsapp'
			AND body IN ('[Photo]', '[Video]', '[Audio]', '[Voice note]')
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
		FROM messages
		WHERE conversation_id IN (`+strings.Join(placeholders, ",")+`)
		ORDER BY timestamp_ms ASC
		LIMIT ?
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
		FROM messages
		WHERE `+conditions+`
		ORDER BY timestamp_ms ASC
		LIMIT ?
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

func scanMessages(rows interface {
	Next() bool
	Scan(...any) error
	Err() error
}) ([]*Message, error) {
	var msgs []*Message
	for rows.Next() {
		m := &Message{}
		if err := rows.Scan(&m.MessageID, &m.ConversationID, &m.SenderName, &m.SenderNumber, &m.Body, &m.TimestampMS, &m.Status, &m.IsFromMe, &m.MentionsMe, &m.MediaID, &m.MimeType, &m.DecryptionKey, &m.Reactions, &m.ReplyToID, &m.SourcePlatform, &m.SourceID); err != nil {
			return nil, err
		}
		msgs = append(msgs, m)
	}
	return msgs, rows.Err()
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
