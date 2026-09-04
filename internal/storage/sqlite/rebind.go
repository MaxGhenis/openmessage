package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// DisplacedRemoteIDPrefix marks a conversation whose remote ID binding was
// taken over by another thread. Google Messages remote conversation IDs are
// device-local row IDs: a phone swap or backup restore re-keys every thread,
// so a numeric ID observed on the wire can name a different thread than the
// one that ID named before the swap. When ingest proves a stored binding
// stale, the old row keeps its history under this marker (which can never
// collide with a wire ID) and stays reachable for a later peer-match rebind.
const DisplacedRemoteIDPrefix = "displaced:"

func displacedRemoteID(remoteID, conversationID string) string {
	return DisplacedRemoteIDPrefix + remoteID + ":" + conversationID
}

// ListConversationPeerIdentities returns the active non-self participant
// identities of a conversation — a direct thread's peer, or a group's members.
func (s *Store) ListConversationPeerIdentities(
	accountID string,
	conversationID string,
) ([]Identity, error) {
	rows, err := s.db.QueryContext(context.Background(), `
		SELECT i.identity_id, i.account_id, i.kind, i.canonical_value, i.raw_value,
		       i.display_name, i.is_self, i.metadata_json, i.created_at_ms, i.updated_at_ms
		FROM conversation_participants p
		JOIN identities i
		  ON i.account_id = p.account_id AND i.identity_id = p.identity_id
		WHERE p.account_id = ? AND p.conversation_id = ?
		  AND p.is_active = 1 AND i.is_self = 0
		ORDER BY i.identity_id
	`, accountID, conversationID)
	if err != nil {
		return nil, fmt.Errorf("list peer identities for conversation %q: %w", conversationID, err)
	}
	identities, err := collectRows(rows, func(row rowScanner) (Identity, error) {
		var identity Identity
		err := row.Scan(
			&identity.IdentityID,
			&identity.AccountID,
			&identity.Kind,
			&identity.CanonicalValue,
			&identity.RawValue,
			&identity.DisplayName,
			&identity.IsSelf,
			&identity.MetadataJSON,
			&identity.CreatedAtMS,
			&identity.UpdatedAtMS,
		)
		return identity, err
	})
	if err != nil {
		return nil, fmt.Errorf("list peer identities for conversation %q: %w", conversationID, err)
	}
	return identities, nil
}

// FindDirectConversationBySolePeer returns the direct conversation whose only
// active non-self participant is the given identity, preferring the most
// recently active one. Displaced rows are eligible: that is how a thread that
// lost its binding to an ID-space reset gets re-linked when its peer's
// messages arrive under the new ID.
func (s *Store) FindDirectConversationBySolePeer(
	accountID string,
	identityID string,
) (Conversation, error) {
	conversation, err := scanConversation(s.db.QueryRowContext(context.Background(), `
		SELECT `+conversationColumns+`
		FROM conversations c
		WHERE c.account_id = ? AND c.kind = 'direct'
		  AND EXISTS (
		      SELECT 1 FROM conversation_participants p
		      WHERE p.account_id = c.account_id AND p.conversation_id = c.conversation_id
		        AND p.identity_id = ? AND p.is_active = 1
		  )
		  AND NOT EXISTS (
		      SELECT 1 FROM conversation_participants p2
		      JOIN identities i2
		        ON i2.account_id = p2.account_id AND i2.identity_id = p2.identity_id
		      WHERE p2.account_id = c.account_id AND p2.conversation_id = c.conversation_id
		        AND p2.is_active = 1 AND i2.is_self = 0 AND p2.identity_id <> ?
		  )
		ORDER BY c.last_message_at_ms DESC, c.conversation_id
		LIMIT 1
	`, accountID, identityID, identityID))
	if errors.Is(err, sql.ErrNoRows) {
		return Conversation{}, fmt.Errorf(
			"direct conversation with sole peer %q: %w",
			identityID,
			ErrNotFound,
		)
	}
	if err != nil {
		return Conversation{}, fmt.Errorf(
			"find direct conversation with sole peer %q: %w",
			identityID,
			err,
		)
	}
	return conversation, nil
}

// FindGroupConversationByPeerSet returns the group conversation whose active
// non-self participant identities are exactly the given set, preferring the
// most recently active one on ties.
func (s *Store) FindGroupConversationByPeerSet(
	accountID string,
	identityIDs []string,
) (Conversation, error) {
	if len(identityIDs) == 0 {
		return Conversation{}, fmt.Errorf("group conversation peer set is empty: %w", ErrNotFound)
	}
	want := make(map[string]struct{}, len(identityIDs))
	for _, id := range identityIDs {
		want[id] = struct{}{}
	}

	rows, err := s.db.QueryContext(context.Background(), `
		SELECT `+conversationColumns+`
		FROM conversations c
		WHERE c.account_id = ? AND c.kind = 'group'
		ORDER BY c.last_message_at_ms DESC, c.conversation_id
	`, accountID)
	if err != nil {
		return Conversation{}, fmt.Errorf("list group conversations: %w", err)
	}
	groups, err := collectRows(rows, scanConversation)
	if err != nil {
		return Conversation{}, fmt.Errorf("list group conversations: %w", err)
	}
	for _, group := range groups {
		peers, err := s.ListConversationPeerIdentities(accountID, group.ConversationID)
		if err != nil {
			return Conversation{}, err
		}
		if len(peers) != len(want) {
			continue
		}
		match := true
		for _, peer := range peers {
			if _, ok := want[peer.IdentityID]; !ok {
				match = false
				break
			}
		}
		if match {
			return group, nil
		}
	}
	return Conversation{}, fmt.Errorf("group conversation with given peer set: %w", ErrNotFound)
}

// ReassignConversationRemoteID moves an account-scoped remote conversation ID
// binding to the given conversation. A different current holder is displaced:
// it keeps its rows and history but its remote_conversation_id becomes a
// displaced marker, freeing the wire ID. Both writes commit atomically.
func (s *Store) ReassignConversationRemoteID(
	accountID string,
	remoteID string,
	toConversationID string,
	nowMS int64,
) error {
	remoteID = strings.TrimSpace(remoteID)
	if remoteID == "" {
		return fmt.Errorf("reassign remote conversation ID: remote ID is empty")
	}
	if strings.TrimSpace(toConversationID) == "" {
		return fmt.Errorf("reassign remote conversation ID %q: target conversation is empty", remoteID)
	}
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("reassign remote conversation ID %q: begin: %w", remoteID, err)
	}
	defer tx.Rollback()

	var holderID string
	err = tx.QueryRowContext(ctx, `
		SELECT conversation_id FROM conversations
		WHERE account_id = ? AND remote_conversation_id = ?
	`, accountID, remoteID).Scan(&holderID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// Unbound: nothing to displace.
	case err != nil:
		return fmt.Errorf("reassign remote conversation ID %q: read holder: %w", remoteID, err)
	case holderID == toConversationID:
		return nil
	default:
		if _, err := tx.ExecContext(ctx, `
			UPDATE conversations
			SET remote_conversation_id = ?, updated_at_ms = MAX(updated_at_ms, ?)
			WHERE account_id = ? AND conversation_id = ?
		`, displacedRemoteID(remoteID, holderID), nowMS, accountID, holderID); err != nil {
			return fmt.Errorf(
				"reassign remote conversation ID %q: displace holder %q: %w",
				remoteID,
				holderID,
				err,
			)
		}
	}

	result, err := tx.ExecContext(ctx, `
		UPDATE conversations
		SET remote_conversation_id = ?, updated_at_ms = MAX(updated_at_ms, ?)
		WHERE account_id = ? AND conversation_id = ?
	`, remoteID, nowMS, accountID, toConversationID)
	if err != nil {
		return fmt.Errorf(
			"reassign remote conversation ID %q to %q: %w",
			remoteID,
			toConversationID,
			err,
		)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("reassign remote conversation ID %q: rows affected: %w", remoteID, err)
	}
	if affected != 1 {
		return fmt.Errorf(
			"reassign remote conversation ID %q: target conversation %q: %w",
			remoteID,
			toConversationID,
			ErrNotFound,
		)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("reassign remote conversation ID %q: commit: %w", remoteID, err)
	}
	return nil
}

// DisplaceConversationRemoteID releases an account-scoped remote conversation
// ID binding without giving it to another row, so a subsequent insert can
// claim it. Reports whether a holder existed.
func (s *Store) DisplaceConversationRemoteID(
	accountID string,
	remoteID string,
	nowMS int64,
) (bool, error) {
	remoteID = strings.TrimSpace(remoteID)
	if remoteID == "" {
		return false, fmt.Errorf("displace remote conversation ID: remote ID is empty")
	}
	ctx := context.Background()
	var holderID string
	err := s.db.QueryRowContext(ctx, `
		SELECT conversation_id FROM conversations
		WHERE account_id = ? AND remote_conversation_id = ?
	`, accountID, remoteID).Scan(&holderID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("displace remote conversation ID %q: read holder: %w", remoteID, err)
	}
	if _, err := s.db.ExecContext(ctx, `
		UPDATE conversations
		SET remote_conversation_id = ?, updated_at_ms = MAX(updated_at_ms, ?)
		WHERE account_id = ? AND conversation_id = ?
	`, displacedRemoteID(remoteID, holderID), nowMS, accountID, holderID); err != nil {
		return false, fmt.Errorf(
			"displace remote conversation ID %q from %q: %w",
			remoteID,
			holderID,
			err,
		)
	}
	return true, nil
}
