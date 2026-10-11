package sqlite

import (
	"context"
	"fmt"
)

// MessageRemoteKey is one stored message's primary key and wire identity.
type MessageRemoteKey struct {
	MessageID            string
	RemoteConversationID string
	RemoteMessageID      string
}

// ListMessageRemoteKeys returns every message of an account with its
// conversation's remote ID. A history feed reads it once per scan to leave
// out what v2 already holds before it builds a frame.
func (s *Store) ListMessageRemoteKeys(ctx context.Context, accountID string) ([]MessageRemoteKey, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT m.message_id, COALESCE(c.remote_conversation_id, ''), COALESCE(m.remote_message_id, '')
		FROM messages AS m
		LEFT JOIN conversations AS c ON c.conversation_id = m.conversation_id
		WHERE m.account_id = ?
		ORDER BY m.message_id`, accountID)
	if err != nil {
		return nil, fmt.Errorf("list message remote keys for account %q: %w", accountID, err)
	}
	defer rows.Close()
	keys := []MessageRemoteKey{}
	for rows.Next() {
		var key MessageRemoteKey
		if err := rows.Scan(&key.MessageID, &key.RemoteConversationID, &key.RemoteMessageID); err != nil {
			return nil, fmt.Errorf("list message remote keys for account %q: %w", accountID, err)
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list message remote keys for account %q: %w", accountID, err)
	}
	return keys, nil
}
