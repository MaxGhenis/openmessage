package sqlite

import (
	"context"
	"database/sql"
	"fmt"
)

// The per-account aggregates below replace reads that loaded every
// conversation of an account and paged through every message. MAX on a
// literal direction is a single seek of messages_account_direction_time_idx;
// COUNT(*) is a covering range of an account-leading index, which never reads
// the message rows themselves.
const (
	latestMessageTimesQuery = `
		SELECT
			(SELECT MAX(occurred_at_ms) FROM messages
			 WHERE account_id = ? AND direction = 'incoming'),
			(SELECT MAX(occurred_at_ms) FROM messages
			 WHERE account_id = ? AND direction = 'outgoing')
	`
	countMessagesQuery      = `SELECT COUNT(*) FROM messages WHERE account_id = ?`
	countConversationsQuery = `SELECT COUNT(*) FROM conversations WHERE account_id = ?`
)

// LatestMessageTimes returns the occurred_at_ms of the account's newest
// message in either direction and of its newest incoming message. Each is 0
// when the account has no such message.
func (s *Store) LatestMessageTimes(accountID string) (latestMS, latestIncomingMS int64, err error) {
	var incoming, outgoing sql.NullInt64
	if err := s.db.QueryRowContext(
		context.Background(), latestMessageTimesQuery, accountID, accountID,
	).Scan(&incoming, &outgoing); err != nil {
		return 0, 0, fmt.Errorf("latest message times for account %q: %w", accountID, err)
	}
	latestMS = max(incoming.Int64, outgoing.Int64)
	return latestMS, incoming.Int64, nil
}

// CountMessages returns the number of stored messages of the account, in
// every direction and state.
func (s *Store) CountMessages(accountID string) (int64, error) {
	var count int64
	if err := s.db.QueryRowContext(
		context.Background(), countMessagesQuery, accountID,
	).Scan(&count); err != nil {
		return 0, fmt.Errorf("count messages for account %q: %w", accountID, err)
	}
	return count, nil
}

// CountConversations returns the number of conversations of the account,
// archived ones included.
func (s *Store) CountConversations(accountID string) (int64, error) {
	var count int64
	if err := s.db.QueryRowContext(
		context.Background(), countConversationsQuery, accountID,
	).Scan(&count); err != nil {
		return 0, fmt.Errorf("count conversations for account %q: %w", accountID, err)
	}
	return count, nil
}
