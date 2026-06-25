package db

import (
	"database/sql"
	"errors"
	"time"
)

const (
	OutgoingSendStatusSending          = "sending"
	OutgoingSendStatusSent             = "sent"
	OutgoingSendStatusFailed           = "failed"
	OutgoingSendStatusLocalStoreFailed = "local_store_failed"
)

type OutgoingSendKey struct {
	Key            string
	ConversationID string
	MessageID      string
	Status         string
	CreatedAtMS    int64
	UpdatedAtMS    int64
}

func (s *Store) ClaimOutgoingSendKey(key, conversationID string) (bool, *OutgoingSendKey, error) {
	now := time.Now().UnixMilli()
	res, err := s.db.Exec(`
		INSERT OR IGNORE INTO outgoing_send_keys
			(idempotency_key, conversation_id, message_id, status, created_at, updated_at)
		VALUES (?, ?, '', ?, ?, ?)
	`, key, conversationID, OutgoingSendStatusSending, now, now)
	if err != nil {
		return false, nil, err
	}
	if rows, err := res.RowsAffected(); err == nil && rows > 0 {
		return true, &OutgoingSendKey{
			Key:            key,
			ConversationID: conversationID,
			Status:         OutgoingSendStatusSending,
			CreatedAtMS:    now,
			UpdatedAtMS:    now,
		}, nil
	}
	existing, err := s.GetOutgoingSendKey(key)
	return false, existing, err
}

func (s *Store) GetOutgoingSendKey(key string) (*OutgoingSendKey, error) {
	row := s.db.QueryRow(`
		SELECT idempotency_key, conversation_id, message_id, status, created_at, updated_at
		FROM outgoing_send_keys
		WHERE idempotency_key = ?
	`, key)
	var item OutgoingSendKey
	if err := row.Scan(&item.Key, &item.ConversationID, &item.MessageID, &item.Status, &item.CreatedAtMS, &item.UpdatedAtMS); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &item, nil
}

func (s *Store) CompleteOutgoingSendKey(key, messageID, status string) error {
	_, err := s.db.Exec(`
		UPDATE outgoing_send_keys
		SET message_id = ?, status = ?, updated_at = ?
		WHERE idempotency_key = ?
	`, messageID, status, time.Now().UnixMilli(), key)
	return err
}

func (s *Store) ReleaseOutgoingSendKey(key string) error {
	_, err := s.db.Exec(`DELETE FROM outgoing_send_keys WHERE idempotency_key = ?`, key)
	return err
}
