package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ListMessagesCreatedSince returns an account's messages whose row was created
// at or after sinceMS (projection time, not occurrence time), oldest first.
func (s *Store) ListMessagesCreatedSince(accountID string, sinceMS int64) ([]Message, error) {
	rows, err := s.db.QueryContext(context.Background(), `
		SELECT `+messageColumns+`
		FROM messages
		WHERE account_id = ? AND created_at_ms >= ?
		ORDER BY created_at_ms, message_id
	`, accountID, sinceMS)
	if err != nil {
		return nil, fmt.Errorf("list messages created since %d: %w", sinceMS, err)
	}
	messages, err := collectRows(rows, scanMessage)
	if err != nil {
		return nil, fmt.Errorf("list messages created since %d: %w", sinceMS, err)
	}
	return messages, nil
}

// CountMessagesCreatedBefore counts a conversation's rows created before
// sinceMS — its history from before the window under repair.
func (s *Store) CountMessagesCreatedBefore(conversationID string, sinceMS int64) (int64, error) {
	var count int64
	if err := s.db.QueryRowContext(context.Background(), `
		SELECT COUNT(*) FROM messages
		WHERE conversation_id = ? AND created_at_ms < ?
	`, conversationID, sinceMS).Scan(&count); err != nil {
		return 0, fmt.Errorf("count messages before %d in %q: %w", sinceMS, conversationID, err)
	}
	return count, nil
}

// ListInboundSenderIdentityIDsBefore returns the distinct attributed inbound
// senders of a conversation's rows created before sinceMS.
func (s *Store) ListInboundSenderIdentityIDsBefore(conversationID string, sinceMS int64) ([]string, error) {
	rows, err := s.db.QueryContext(context.Background(), `
		SELECT DISTINCT sender_identity_id FROM messages
		WHERE conversation_id = ? AND created_at_ms < ?
		  AND direction = 'incoming' AND sender_identity_id IS NOT NULL
		ORDER BY sender_identity_id
	`, conversationID, sinceMS)
	if err != nil {
		return nil, fmt.Errorf("list inbound senders before %d in %q: %w", sinceMS, conversationID, err)
	}
	return collectRows(rows, func(row rowScanner) (string, error) {
		var id string
		err := row.Scan(&id)
		return id, err
	})
}

// MessageHasReadCursor reports whether any read cursor points at the message.
// Such a row cannot change conversation without breaking the cursor's
// composite foreign key.
func (s *Store) MessageHasReadCursor(messageID string) (bool, error) {
	var count int64
	if err := s.db.QueryRowContext(context.Background(), `
		SELECT COUNT(*) FROM read_cursors WHERE last_read_message_id = ?
	`, messageID).Scan(&count); err != nil {
		return false, fmt.Errorf("check read cursors for message %q: %w", messageID, err)
	}
	return count > 0, nil
}

// SelfIdentityID returns one self identity for the account, if any.
func (s *Store) SelfIdentityID(accountID string) (string, bool, error) {
	var id string
	err := s.db.QueryRowContext(context.Background(), `
		SELECT identity_id FROM identities
		WHERE account_id = ? AND is_self = 1
		ORDER BY updated_at_ms DESC, identity_id
		LIMIT 1
	`, accountID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("find self identity for %q: %w", accountID, err)
	}
	return id, true, nil
}

// RepairParticipant is one roster entry carried by a mint or meta step.
type RepairParticipant struct {
	IdentityID  string `json:"identity_id"`
	DisplayName string `json:"display_name,omitempty"`
	Role        string `json:"role,omitempty"`
	IsActive    bool   `json:"is_active"`
}

// RepairStep is one write in a repair plan. Ops:
//
//	mint         create conversation ConversationID bound to RemoteConversationID
//	             with Kind, Title and roster (Participants, else
//	             ParticipantIdentityIDs); any current holder of the remote id is
//	             displaced first
//	move         move message MessageID (and its reactions) to TargetConversationID
//	delete       delete message MessageID (attachments/reactions cascade)
//	rebind       bind RemoteConversationID to TargetConversationID, displacing any
//	             other holder
//	meta         set ConversationID's Title and replace its roster with Participants
//	drop         delete conversation ConversationID if it holds no messages
//	recency      recompute ConversationID.last_message_at_ms from its messages
type RepairStep struct {
	Op                     string              `json:"op"`
	ConversationID         string              `json:"conversation_id,omitempty"`
	RemoteConversationID   string              `json:"remote_conversation_id,omitempty"`
	TargetConversationID   string              `json:"target_conversation_id,omitempty"`
	MessageID              string              `json:"message_id,omitempty"`
	Kind                   string              `json:"kind,omitempty"`
	Title                  string              `json:"title,omitempty"`
	ParticipantIdentityIDs []string            `json:"participant_identity_ids,omitempty"`
	Participants           []RepairParticipant `json:"participants,omitempty"`
	CreatedAtMS            int64               `json:"created_at_ms,omitempty"`
	Reason                 string              `json:"reason,omitempty"`
}

func (step RepairStep) roster() []RepairParticipant {
	if len(step.Participants) > 0 {
		return step.Participants
	}
	roster := make([]RepairParticipant, 0, len(step.ParticipantIdentityIDs))
	for _, id := range step.ParticipantIdentityIDs {
		roster = append(roster, RepairParticipant{IdentityID: id, Role: "member", IsActive: true})
	}
	return roster
}

// ListConversationsUpdatedSince returns an account's conversations whose row
// was touched at or after sinceMS.
func (s *Store) ListConversationsUpdatedSince(accountID string, sinceMS int64) ([]Conversation, error) {
	rows, err := s.db.QueryContext(context.Background(), `
		SELECT `+conversationColumns+`
		FROM conversations
		WHERE account_id = ? AND updated_at_ms >= ?
		ORDER BY conversation_id
	`, accountID, sinceMS)
	if err != nil {
		return nil, fmt.Errorf("list conversations updated since %d: %w", sinceMS, err)
	}
	conversations, err := collectRows(rows, scanConversation)
	if err != nil {
		return nil, fmt.Errorf("list conversations updated since %d: %w", sinceMS, err)
	}
	return conversations, nil
}

// ApplyRepairPlan executes the steps in order inside one transaction.
func (s *Store) ApplyRepairPlan(ctx context.Context, accountID string, steps []RepairStep, nowMS int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("apply repair plan: begin: %w", err)
	}
	defer tx.Rollback()

	displace := func(remoteID string) error {
		var holderID string
		err := tx.QueryRowContext(ctx, `
			SELECT conversation_id FROM conversations
			WHERE account_id = ? AND remote_conversation_id = ?
		`, accountID, remoteID).Scan(&holderID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `
			UPDATE conversations
			SET remote_conversation_id = ?, updated_at_ms = MAX(updated_at_ms, ?)
			WHERE account_id = ? AND conversation_id = ?
		`, displacedRemoteID(remoteID, holderID), nowMS, accountID, holderID)
		return err
	}

	for index, step := range steps {
		var stepErr error
		switch step.Op {
		case "mint":
			if stepErr = displace(step.RemoteConversationID); stepErr != nil {
				break
			}
			createdAt := step.CreatedAtMS
			if createdAt <= 0 {
				createdAt = nowMS
			}
			_, stepErr = tx.ExecContext(ctx, `
				INSERT INTO conversations (
					conversation_id, account_id, remote_conversation_id, kind, title,
					notification_mode, metadata_json, created_at_ms, updated_at_ms
				) VALUES (?, ?, ?, ?, ?, 'all', '{}', ?, ?)
			`, step.ConversationID, accountID, step.RemoteConversationID, step.Kind, step.Title, createdAt, nowMS)
			if stepErr != nil {
				break
			}
			stepErr = insertRepairRoster(ctx, tx, accountID, step.ConversationID, step.roster())
		case "meta":
			if _, stepErr = tx.ExecContext(ctx, `
				UPDATE conversations
				SET title = ?,
				    kind = COALESCE(NULLIF(?, ''), kind),
				    updated_at_ms = MAX(updated_at_ms, ?)
				WHERE account_id = ? AND conversation_id = ?
			`, step.Title, step.Kind, nowMS, accountID, step.ConversationID); stepErr != nil {
				break
			}
			if _, stepErr = tx.ExecContext(ctx, `
				DELETE FROM conversation_participants
				WHERE account_id = ? AND conversation_id = ?
			`, accountID, step.ConversationID); stepErr != nil {
				break
			}
			stepErr = insertRepairRoster(ctx, tx, accountID, step.ConversationID, step.Participants)
		case "move":
			if _, stepErr = tx.ExecContext(ctx, `
				UPDATE reactions SET conversation_id = ?, updated_at_ms = MAX(updated_at_ms, ?)
				WHERE message_id = ?
			`, step.TargetConversationID, nowMS, step.MessageID); stepErr != nil {
				break
			}
			_, stepErr = tx.ExecContext(ctx, `
				UPDATE messages SET conversation_id = ?, updated_at_ms = MAX(updated_at_ms, ?)
				WHERE message_id = ? AND account_id = ?
			`, step.TargetConversationID, nowMS, step.MessageID, accountID)
		case "delete":
			_, stepErr = tx.ExecContext(ctx, `
				DELETE FROM messages WHERE message_id = ? AND account_id = ?
			`, step.MessageID, accountID)
		case "rebind":
			var holderID string
			err := tx.QueryRowContext(ctx, `
				SELECT conversation_id FROM conversations
				WHERE account_id = ? AND remote_conversation_id = ?
			`, accountID, step.RemoteConversationID).Scan(&holderID)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				stepErr = err
				break
			}
			if holderID == step.TargetConversationID {
				break
			}
			if stepErr = displace(step.RemoteConversationID); stepErr != nil {
				break
			}
			_, stepErr = tx.ExecContext(ctx, `
				UPDATE conversations
				SET remote_conversation_id = ?, updated_at_ms = MAX(updated_at_ms, ?)
				WHERE account_id = ? AND conversation_id = ?
			`, step.RemoteConversationID, nowMS, accountID, step.TargetConversationID)
		case "drop":
			_, stepErr = tx.ExecContext(ctx, `
				DELETE FROM conversations
				WHERE account_id = ? AND conversation_id = ?
				  AND NOT EXISTS (SELECT 1 FROM messages WHERE conversation_id = conversations.conversation_id)
			`, accountID, step.ConversationID)
		case "recency":
			_, stepErr = tx.ExecContext(ctx, `
				UPDATE conversations
				SET last_message_at_ms = (
					SELECT COALESCE(MAX(occurred_at_ms), 0) FROM messages
					WHERE conversation_id = conversations.conversation_id
				)
				WHERE account_id = ? AND conversation_id = ?
			`, accountID, step.ConversationID)
		default:
			stepErr = fmt.Errorf("unknown op %q", step.Op)
		}
		if stepErr != nil {
			return fmt.Errorf("apply repair plan: step %d (%s %s/%s/%s): %w",
				index, step.Op, step.ConversationID, step.MessageID, step.RemoteConversationID, stepErr)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("apply repair plan: commit: %w", err)
	}
	return nil
}

func insertRepairRoster(ctx context.Context, tx *sql.Tx, accountID, conversationID string, roster []RepairParticipant) error {
	for _, participant := range roster {
		role := participant.Role
		if role == "" {
			role = "member"
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO conversation_participants (
				account_id, conversation_id, identity_id, role, display_name, is_active
			) VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT (conversation_id, identity_id) DO UPDATE SET
				role = excluded.role,
				display_name = excluded.display_name,
				is_active = excluded.is_active
		`, accountID, conversationID, participant.IdentityID, role, participant.DisplayName, participant.IsActive); err != nil {
			return err
		}
	}
	return nil
}
