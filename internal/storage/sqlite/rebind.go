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
	conversations, err := s.ListDirectConversationsBySolePeer(accountID, identityID)
	if err != nil {
		return Conversation{}, err
	}
	if len(conversations) == 0 {
		return Conversation{}, fmt.Errorf(
			"direct conversation with sole peer %q: %w",
			identityID,
			ErrNotFound,
		)
	}
	return conversations[0], nil
}

// ListDirectConversationsBySolePeer returns every direct conversation whose
// only active non-self participant is the given identity, most recently active
// first. Displaced rows are included.
func (s *Store) ListDirectConversationsBySolePeer(
	accountID string,
	identityID string,
) ([]Conversation, error) {
	rows, err := s.db.QueryContext(context.Background(), `
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
	`, accountID, identityID, identityID)
	if err != nil {
		return nil, fmt.Errorf(
			"list direct conversations with sole peer %q: %w",
			identityID,
			err,
		)
	}
	conversations, err := collectRows(rows, scanConversation)
	if err != nil {
		return nil, fmt.Errorf(
			"list direct conversations with sole peer %q: %w",
			identityID,
			err,
		)
	}
	return conversations, nil
}

// FindGroupConversationByPeerSet returns the group conversation whose active
// non-self participant identities are exactly the given set, preferring the
// most recently active one on ties.
func (s *Store) FindGroupConversationByPeerSet(
	accountID string,
	identityIDs []string,
) (Conversation, error) {
	groups, err := s.ListGroupConversationsByPeerSet(accountID, identityIDs)
	if err != nil {
		return Conversation{}, err
	}
	if len(groups) == 0 {
		return Conversation{}, fmt.Errorf("group conversation with given peer set: %w", ErrNotFound)
	}
	return groups[0], nil
}

// ListGroupConversationsByPeerSet returns every group conversation whose
// active non-self participant identities are exactly the given set, most
// recently active first. Displaced rows are included.
func (s *Store) ListGroupConversationsByPeerSet(
	accountID string,
	identityIDs []string,
) ([]Conversation, error) {
	if len(identityIDs) == 0 {
		return nil, nil
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
		return nil, fmt.Errorf("list group conversations: %w", err)
	}
	groups, err := collectRows(rows, scanConversation)
	if err != nil {
		return nil, fmt.Errorf("list group conversations: %w", err)
	}
	matches := make([]Conversation, 0, 1)
	for _, group := range groups {
		peers, err := s.ListConversationPeerIdentities(accountID, group.ConversationID)
		if err != nil {
			return nil, err
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
			matches = append(matches, group)
		}
	}
	return matches, nil
}

// RemoteIDSpaceEpoch returns the account's current device ID-space epoch: the
// number of remote ID-space resets ingest has detected for it.
func (s *Store) RemoteIDSpaceEpoch(accountID string) (int64, error) {
	var epoch int64
	err := s.db.QueryRowContext(context.Background(), `
		SELECT remote_idspace_epoch FROM accounts WHERE account_id = ?
	`, accountID).Scan(&epoch)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("remote ID-space epoch for account %q: %w", accountID, ErrNotFound)
	}
	if err != nil {
		return 0, fmt.Errorf("read remote ID-space epoch for account %q: %w", accountID, err)
	}
	return epoch, nil
}

// AdvanceRemoteIDSpaceEpoch moves the account from epoch from to from+1 and
// returns the account's epoch afterwards. When the account has already moved
// past from, it returns that newer epoch without advancing again.
func (s *Store) AdvanceRemoteIDSpaceEpoch(accountID string, from int64) (int64, error) {
	if _, err := s.db.ExecContext(context.Background(), `
		UPDATE accounts
		SET remote_idspace_epoch = remote_idspace_epoch + 1
		WHERE account_id = ? AND remote_idspace_epoch = ?
	`, accountID, from); err != nil {
		return 0, fmt.Errorf("advance remote ID-space epoch for account %q: %w", accountID, err)
	}
	return s.RemoteIDSpaceEpoch(accountID)
}

// RemoteBinding is a conversation's device ID-space provenance (see migration
// 0011).
type RemoteBinding struct {
	// Provisional is true for a thread ingest minted from a message frame that
	// no ConversationEvent has reached yet.
	Provisional bool
	// AnnouncedEpoch is the latest ID-space epoch in which a ConversationEvent
	// was applied to the row. Zero when Provisional.
	AnnouncedEpoch int64
	// BoundEpoch is the epoch in which the row's current wire id was bound.
	BoundEpoch int64
}

// ConversationRemoteBinding returns the conversation's ID-space provenance.
func (s *Store) ConversationRemoteBinding(accountID, conversationID string) (RemoteBinding, error) {
	var announced sql.NullInt64
	var binding RemoteBinding
	err := s.db.QueryRowContext(context.Background(), `
		SELECT remote_announced_epoch, remote_bound_epoch FROM conversations
		WHERE account_id = ? AND conversation_id = ?
	`, accountID, conversationID).Scan(&announced, &binding.BoundEpoch)
	if errors.Is(err, sql.ErrNoRows) {
		return RemoteBinding{}, fmt.Errorf("remote binding of conversation %q: %w", conversationID, ErrNotFound)
	}
	if err != nil {
		return RemoteBinding{}, fmt.Errorf("read remote binding of conversation %q: %w", conversationID, err)
	}
	binding.Provisional = !announced.Valid
	binding.AnnouncedEpoch = announced.Int64
	return binding, nil
}

// MarkConversationAnnounced records that the transport announced the
// conversation (a ConversationEvent was applied to it) in the given ID-space
// epoch, which also confirms its current binding in that epoch. Neither epoch
// moves backwards, and the row stops being provisional.
func (s *Store) MarkConversationAnnounced(accountID, conversationID string, epoch int64) error {
	return s.updateRemoteBinding("announced", `
		UPDATE conversations
		SET remote_announced_epoch = MAX(COALESCE(remote_announced_epoch, 0), ?1),
		    remote_bound_epoch = MAX(remote_bound_epoch, ?1)
		WHERE account_id = ?2 AND conversation_id = ?3
	`, accountID, conversationID, epoch)
}

// MarkConversationProvisional records that ingest minted the conversation from
// a message frame in the given epoch: it is provisional until a
// ConversationEvent announces it.
func (s *Store) MarkConversationProvisional(accountID, conversationID string, epoch int64) error {
	return s.updateRemoteBinding("provisional", `
		UPDATE conversations
		SET remote_announced_epoch = NULL, remote_bound_epoch = ?1
		WHERE account_id = ?2 AND conversation_id = ?3
	`, accountID, conversationID, epoch)
}

// MarkConversationBound records that the conversation's current wire id was
// bound to it in the given epoch. It never moves backwards.
func (s *Store) MarkConversationBound(accountID, conversationID string, epoch int64) error {
	return s.updateRemoteBinding("bound", `
		UPDATE conversations
		SET remote_bound_epoch = MAX(remote_bound_epoch, ?1)
		WHERE account_id = ?2 AND conversation_id = ?3
	`, accountID, conversationID, epoch)
}

func (s *Store) updateRemoteBinding(
	what string,
	statement string,
	accountID string,
	conversationID string,
	epoch int64,
) error {
	if epoch < 0 {
		return fmt.Errorf("mark conversation %q %s: epoch %d is negative", conversationID, what, epoch)
	}
	result, err := s.db.ExecContext(context.Background(), statement, epoch, accountID, conversationID)
	if err != nil {
		return fmt.Errorf("mark conversation %q %s: %w", conversationID, what, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("mark conversation %q %s: rows affected: %w", conversationID, what, err)
	}
	if affected != 1 {
		return fmt.Errorf("mark conversation %q %s: %w", conversationID, what, ErrNotFound)
	}
	return nil
}

// ConversationMerge reports what MergeConversationInto did.
type ConversationMerge struct {
	// Moved messages now live in the target conversation.
	Moved int
	// Duplicates were deleted because the target already held the same
	// message (direction, sender, millisecond and body) under another remote id.
	Duplicates int
	// Kept messages stayed behind because the target already uses their remote
	// message id for a different message.
	Kept int
	// Dropped is true when the emptied source conversation was deleted.
	Dropped bool
}

// MergeConversationInto folds the conversation bound to remoteID into target
// and binds remoteID to target, in one transaction. It is the undo for a
// thread minted under a re-keyed wire id before the ID-space reset that
// re-keyed it was detected: each message moves to target, except a content
// duplicate of a target message (deleted) or one whose remote message id the
// target already uses (left behind). The source keeps whatever is left under a
// displaced marker, and is deleted when nothing references it.
func (s *Store) MergeConversationInto(
	ctx context.Context,
	accountID string,
	remoteID string,
	sourceID string,
	targetID string,
	nowMS int64,
) (ConversationMerge, error) {
	var merge ConversationMerge
	if sourceID == targetID {
		return merge, fmt.Errorf("merge conversation %q into itself", sourceID)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return merge, fmt.Errorf("merge conversation %q: begin: %w", sourceID, err)
	}
	defer tx.Rollback()

	var holderID string
	err = tx.QueryRowContext(ctx, `
		SELECT conversation_id FROM conversations
		WHERE account_id = ? AND remote_conversation_id = ?
	`, accountID, remoteID).Scan(&holderID)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && holderID != sourceID) {
		return merge, fmt.Errorf("merge conversation %q: %q is no longer bound to it: %w", sourceID, remoteID, ErrNotFound)
	}
	if err != nil {
		return merge, fmt.Errorf("merge conversation %q: read holder: %w", sourceID, err)
	}
	var targetExists int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM conversations WHERE account_id = ? AND conversation_id = ?
	`, accountID, targetID).Scan(&targetExists); err != nil {
		return merge, fmt.Errorf("merge conversation %q: read target: %w", sourceID, err)
	}
	if targetExists != 1 {
		return merge, fmt.Errorf("merge conversation %q: target %q: %w", sourceID, targetID, ErrNotFound)
	}

	// Read cursors point at messages of their own conversation; the source's
	// cursors cannot follow its messages.
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM read_cursors WHERE account_id = ? AND conversation_id = ?
	`, accountID, sourceID); err != nil {
		return merge, fmt.Errorf("merge conversation %q: drop read cursors: %w", sourceID, err)
	}

	type sourceMessage struct {
		messageID, remoteMessageID, direction, body string
		senderIdentityID                            sql.NullString
		occurredAtMS                                int64
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT message_id, remote_message_id, direction, body, sender_identity_id, occurred_at_ms
		FROM messages WHERE account_id = ? AND conversation_id = ?
		ORDER BY occurred_at_ms, message_id
	`, accountID, sourceID)
	if err != nil {
		return merge, fmt.Errorf("merge conversation %q: list messages: %w", sourceID, err)
	}
	var messages []sourceMessage
	for rows.Next() {
		var message sourceMessage
		if err := rows.Scan(
			&message.messageID,
			&message.remoteMessageID,
			&message.direction,
			&message.body,
			&message.senderIdentityID,
			&message.occurredAtMS,
		); err != nil {
			rows.Close()
			return merge, fmt.Errorf("merge conversation %q: scan message: %w", sourceID, err)
		}
		messages = append(messages, message)
	}
	if err := rows.Close(); err != nil {
		return merge, fmt.Errorf("merge conversation %q: list messages: %w", sourceID, err)
	}

	for _, message := range messages {
		if strings.TrimSpace(message.body) != "" {
			var duplicate int
			if err := tx.QueryRowContext(ctx, `
				SELECT COUNT(*) FROM messages
				WHERE account_id = ? AND conversation_id = ? AND occurred_at_ms = ?
				  AND direction = ? AND sender_identity_id IS ? AND body = ?
				  AND remote_message_id <> ?
			`, accountID, targetID, message.occurredAtMS, message.direction,
				message.senderIdentityID, message.body, message.remoteMessageID).Scan(&duplicate); err != nil {
				return merge, fmt.Errorf("merge conversation %q: match duplicate: %w", sourceID, err)
			}
			if duplicate > 0 {
				if _, err := tx.ExecContext(ctx, `
					DELETE FROM messages WHERE account_id = ? AND message_id = ?
				`, accountID, message.messageID); err != nil {
					return merge, fmt.Errorf("merge conversation %q: delete duplicate: %w", sourceID, err)
				}
				merge.Duplicates++
				continue
			}
		}
		var collision int
		if err := tx.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM messages
			WHERE account_id = ? AND conversation_id = ? AND remote_message_id = ?
		`, accountID, targetID, message.remoteMessageID).Scan(&collision); err != nil {
			return merge, fmt.Errorf("merge conversation %q: match remote id: %w", sourceID, err)
		}
		if collision > 0 {
			merge.Kept++
			continue
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE reactions SET conversation_id = ?, updated_at_ms = MAX(updated_at_ms, ?)
			WHERE account_id = ? AND message_id = ?
		`, targetID, nowMS, accountID, message.messageID); err != nil {
			return merge, fmt.Errorf("merge conversation %q: move reactions: %w", sourceID, err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE messages SET conversation_id = ?, updated_at_ms = MAX(updated_at_ms, ?)
			WHERE account_id = ? AND message_id = ?
		`, targetID, nowMS, accountID, message.messageID); err != nil {
			return merge, fmt.Errorf("merge conversation %q: move message: %w", sourceID, err)
		}
		merge.Moved++
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE conversations
		SET remote_conversation_id = ?, updated_at_ms = MAX(updated_at_ms, ?)
		WHERE account_id = ? AND conversation_id = ?
	`, displacedRemoteID(remoteID, sourceID), nowMS, accountID, sourceID); err != nil {
		return merge, fmt.Errorf("merge conversation %q: displace: %w", sourceID, err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE conversations
		SET remote_conversation_id = ?,
		    last_message_at_ms = MAX(last_message_at_ms, COALESCE((
		        SELECT MAX(occurred_at_ms) FROM messages
		        WHERE account_id = conversations.account_id
		          AND conversation_id = conversations.conversation_id
		    ), 0)),
		    updated_at_ms = MAX(updated_at_ms, ?)
		WHERE account_id = ? AND conversation_id = ?
	`, remoteID, nowMS, accountID, targetID); err != nil {
		return merge, fmt.Errorf("merge conversation %q: bind target %q: %w", sourceID, targetID, err)
	}
	if merge.Kept == 0 {
		result, err := tx.ExecContext(ctx, `
			DELETE FROM conversations
			WHERE account_id = ? AND conversation_id = ?
			  AND NOT EXISTS (SELECT 1 FROM messages WHERE conversation_id = conversations.conversation_id)
			  AND NOT EXISTS (SELECT 1 FROM outbox WHERE conversation_id = conversations.conversation_id)
		`, accountID, sourceID)
		if err != nil {
			return merge, fmt.Errorf("merge conversation %q: drop: %w", sourceID, err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return merge, fmt.Errorf("merge conversation %q: drop: rows affected: %w", sourceID, err)
		}
		merge.Dropped = affected == 1
	}
	if err := tx.Commit(); err != nil {
		return merge, fmt.Errorf("merge conversation %q: commit: %w", sourceID, err)
	}
	return merge, nil
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
