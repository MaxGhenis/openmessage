package sqlite

import (
	"context"
	"fmt"
)

// SelfIdentityValue is one of an account's self identities as stored.
type SelfIdentityValue struct {
	Kind           string
	CanonicalValue string
}

// SelfIdentityValues lists every self identity of an account, newest first.
// A capture-time reader picks the form it needs (a phone number) instead of
// trusting whichever self row happens to be newest.
func (s *Store) SelfIdentityValues(accountID string) ([]SelfIdentityValue, error) {
	rows, err := s.db.QueryContext(context.Background(), `
		SELECT kind, canonical_value FROM identities
		WHERE account_id = ? AND is_self = 1
		ORDER BY updated_at_ms DESC, identity_id
	`, accountID)
	if err != nil {
		return nil, fmt.Errorf("list self identities for %q: %w", accountID, err)
	}
	defer rows.Close()
	var values []SelfIdentityValue
	for rows.Next() {
		var value SelfIdentityValue
		if err := rows.Scan(&value.Kind, &value.CanonicalValue); err != nil {
			return nil, fmt.Errorf("scan self identity for %q: %w", accountID, err)
		}
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read self identities for %q: %w", accountID, err)
	}
	return values, nil
}

// MessageOutboxTransportRequestIDs lists the transport request IDs of every
// text and media send the account has queued, in any state. A transport that
// derives its wire message ID from the request ID can tell which observed
// messages are this app's own sends.
func (s *Store) MessageOutboxTransportRequestIDs(accountID string) ([]string, error) {
	rows, err := s.db.QueryContext(context.Background(), `
		SELECT transport_request_id FROM outbox
		WHERE account_id = ? AND kind IN ('text', 'media')
	`, accountID)
	if err != nil {
		return nil, fmt.Errorf("list outbox request IDs for %q: %w", accountID, err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan outbox request ID for %q: %w", accountID, err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read outbox request IDs for %q: %w", accountID, err)
	}
	return ids, nil
}
