package db

import (
	"database/sql"
	"fmt"
	"strings"
)

// messageColumns is the canonical column list for SELECT queries on messages.
const messageColumns = `message_id, conversation_id, sender_name, sender_number, body, timestamp_ms, status, is_from_me, media_id, mime_type, decryption_key, reactions, reply_to_id, source_platform, source_id`

func (s *Store) UpsertMessage(m *Message) error {
	_, err := s.db.Exec(`
		INSERT INTO messages (message_id, conversation_id, sender_name, sender_number, body, timestamp_ms, status, is_from_me, media_id, mime_type, decryption_key, reactions, reply_to_id, source_platform, source_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(message_id) DO UPDATE SET
			conversation_id=excluded.conversation_id,
			sender_name=excluded.sender_name,
			sender_number=excluded.sender_number,
			body=excluded.body,
			timestamp_ms=excluded.timestamp_ms,
			status=excluded.status,
			is_from_me=excluded.is_from_me,
			media_id=excluded.media_id,
			mime_type=excluded.mime_type,
			decryption_key=excluded.decryption_key,
			reactions=excluded.reactions,
			reply_to_id=excluded.reply_to_id,
			source_platform=excluded.source_platform,
			source_id=excluded.source_id
	`, m.MessageID, m.ConversationID, m.SenderName, m.SenderNumber, m.Body, m.TimestampMS, m.Status, m.IsFromMe, m.MediaID, m.MimeType, m.DecryptionKey, m.Reactions, m.ReplyToID, m.SourcePlatform, m.SourceID)
	return err
}

func (s *Store) GetMessagesByConversation(conversationID string, limit int) ([]*Message, error) {
	rows, err := s.db.Query(`
		SELECT `+messageColumns+`
		FROM messages
		WHERE conversation_id = ?
		ORDER BY timestamp_ms DESC
		LIMIT ?
	`, conversationID, limit)
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
	err := row.Scan(&m.MessageID, &m.ConversationID, &m.SenderName, &m.SenderNumber, &m.Body, &m.TimestampMS, &m.Status, &m.IsFromMe, &m.MediaID, &m.MimeType, &m.DecryptionKey, &m.Reactions, &m.ReplyToID, &m.SourcePlatform, &m.SourceID)
	if err != nil {
		if err.Error() == "sql: no rows in result set" {
			return nil, nil
		}
		return nil, err
	}
	return m, nil
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

// DeleteTmpMessages removes locally-created tmp_ messages for a conversation.
// Called when the server echo arrives with a real message ID.
func (s *Store) DeleteTmpMessages(conversationID string) (int64, error) {
	result, err := s.db.Exec(
		`DELETE FROM messages WHERE conversation_id = ? AND message_id LIKE 'tmp_%'`,
		conversationID,
	)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
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
		if err := rows.Scan(&m.MessageID, &m.ConversationID, &m.SenderName, &m.SenderNumber, &m.Body, &m.TimestampMS, &m.Status, &m.IsFromMe, &m.MediaID, &m.MimeType, &m.DecryptionKey, &m.Reactions, &m.ReplyToID, &m.SourcePlatform, &m.SourceID); err != nil {
			return nil, err
		}
		msgs = append(msgs, m)
	}
	return msgs, rows.Err()
}
