package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

const unreadConversationQueryBatchSize = 500

// ReadCursor is the latest device-scoped read position for one conversation.
// LastReadMessageID may be nil for imported cursors that only approximate a
// position; M3 read-receipt submissions always provide it.
type ReadCursor struct {
	AccountID         string
	DeviceID          string
	ConversationID    string
	LastReadMessageID *string
	LastReadAtMS      int64
	SourceUpdatedAtMS *int64
	UpdatedAtMS       int64
}

// UpsertReadCursor advances a cursor monotonically by read time. An older
// write succeeds without changing the stored cursor; an equal time replaces
// the message position and update timestamp.
func (s *Store) UpsertReadCursor(cursor ReadCursor) error {
	if err := upsertReadCursor(
		context.Background(),
		s.db,
		cursor,
	); err != nil {
		return fmt.Errorf(
			"upsert read cursor for device %q and conversation %q: %w",
			cursor.DeviceID,
			cursor.ConversationID,
			err,
		)
	}
	return nil
}

func (s *Store) upsertReadCursorTx(
	ctx context.Context,
	tx *sql.Tx,
	cursor ReadCursor,
) error {
	if err := upsertReadCursor(ctx, tx, cursor); err != nil {
		return fmt.Errorf(
			"upsert read cursor for device %q and conversation %q: %w",
			cursor.DeviceID,
			cursor.ConversationID,
			err,
		)
	}
	return nil
}

type readCursorExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func upsertReadCursor(
	ctx context.Context,
	execer readCursorExecer,
	cursor ReadCursor,
) error {
	_, err := execer.ExecContext(ctx, `
		INSERT INTO read_cursors (
			account_id,
			device_id,
			conversation_id,
			last_read_message_id,
			last_read_at_ms,
			source_updated_at_ms,
			updated_at_ms
		) VALUES (?, ?, ?, ?, ?, NULL, ?)
		ON CONFLICT(device_id, conversation_id) DO UPDATE SET
			last_read_message_id = excluded.last_read_message_id,
			last_read_at_ms      = excluded.last_read_at_ms,
			updated_at_ms        = excluded.updated_at_ms
		WHERE excluded.last_read_at_ms >= read_cursors.last_read_at_ms
	`,
		cursor.AccountID,
		cursor.DeviceID,
		cursor.ConversationID,
		cursor.LastReadMessageID,
		cursor.LastReadAtMS,
		cursor.UpdatedAtMS,
	)
	if err != nil {
		return mapConstraintError(err)
	}
	return nil
}

// GetReadCursor returns the cursor for one device and conversation.
func (s *Store) GetReadCursor(deviceID, conversationID string) (ReadCursor, error) {
	var cursor ReadCursor
	err := s.db.QueryRowContext(context.Background(), `
		SELECT
			account_id,
			device_id,
			conversation_id,
			last_read_message_id,
			last_read_at_ms,
			source_updated_at_ms,
			updated_at_ms
		FROM read_cursors
		WHERE device_id = ? AND conversation_id = ?
	`, deviceID, conversationID).Scan(
		&cursor.AccountID,
		&cursor.DeviceID,
		&cursor.ConversationID,
		&cursor.LastReadMessageID,
		&cursor.LastReadAtMS,
		&cursor.SourceUpdatedAtMS,
		&cursor.UpdatedAtMS,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return ReadCursor{}, notFound("read cursor", deviceID+"/"+conversationID)
	}
	if err != nil {
		return ReadCursor{}, fmt.Errorf(
			"get read cursor for device %q and conversation %q: %w",
			deviceID,
			conversationID,
			err,
		)
	}
	return cursor, nil
}

// UnreadCountsForConversations derives unread state for a conversation batch
// from the current local device's cursor. A missing cursor is equivalent to a
// cursor at zero, so every incoming message in that conversation is unread.
func (s *Store) UnreadCountsForConversations(
	ctx context.Context,
	conversationIDs []string,
) (map[string]int, error) {
	result := make(map[string]int)
	if len(conversationIDs) == 0 {
		return result, nil
	}

	unique := make([]string, 0, len(conversationIDs))
	seen := make(map[string]struct{}, len(conversationIDs))
	for _, conversationID := range conversationIDs {
		if _, exists := seen[conversationID]; exists {
			continue
		}
		seen[conversationID] = struct{}{}
		unique = append(unique, conversationID)
	}
	for start := 0; start < len(unique); start += unreadConversationQueryBatchSize {
		end := min(start+unreadConversationQueryBatchSize, len(unique))
		batch := unique[start:end]
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(batch)), ",")
		arguments := make([]any, len(batch))
		for index, conversationID := range batch {
			arguments[index] = conversationID
		}

		rows, err := s.db.QueryContext(ctx, `
			WITH current_cursors AS (
				SELECT r.conversation_id, MAX(r.last_read_at_ms) AS last_read_at_ms
				FROM read_cursors AS r
				JOIN devices AS d
				  ON d.account_id = r.account_id
				 AND d.device_id = r.device_id
				WHERE d.is_current = 1
				  AND r.conversation_id IN (`+placeholders+`)
				GROUP BY r.conversation_id
			)
			SELECT m.conversation_id, COUNT(*)
			FROM messages AS m
			LEFT JOIN current_cursors AS c
			  ON c.conversation_id = m.conversation_id
			WHERE m.conversation_id IN (`+placeholders+`)
			  AND m.direction = 'incoming'
			  AND m.occurred_at_ms > COALESCE(c.last_read_at_ms, 0)
			GROUP BY m.conversation_id
		`, append(arguments, arguments...)...)
		if err != nil {
			return nil, fmt.Errorf("list unread counts for conversations: %w", err)
		}
		for rows.Next() {
			var conversationID string
			var count int
			if err := rows.Scan(&conversationID, &count); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("scan unread count for conversations: %w", err)
			}
			result[conversationID] = count
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("iterate unread counts for conversations: %w", err)
		}
		if err := rows.Close(); err != nil {
			return nil, fmt.Errorf("close unread counts for conversations: %w", err)
		}
	}
	return result, nil
}
