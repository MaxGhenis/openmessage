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

// MessageHasOpenSend reports whether an unfinished send (an outbox row not yet
// confirmed, rejected or canceled) names the message as its local copy.
// Dispatch and carry-over read that row inside the send's own conversation,
// so it can't move to another conversation or be deleted while the send is
// open.
func (s *Store) MessageHasOpenSend(messageID string) (bool, error) {
	var exists bool
	if err := s.db.QueryRowContext(context.Background(), `
		SELECT EXISTS (
			SELECT 1 FROM outbox
			WHERE local_message_id = ? AND state NOT IN `+closedOutboxStates+`
		)
	`, messageID).Scan(&exists); err != nil {
		return false, fmt.Errorf("check open sends for message %q: %w", messageID, err)
	}
	return exists, nil
}

// closedOutboxStates are the terminal outbox states, as an SQL list.
const closedOutboxStates = `('confirmed', 'rejected', 'canceled')`

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
//	move         move message MessageID to TargetConversationID with its
//	             reactions and the reaction and read-receipt intents that
//	             target it; refused while a read cursor or an unfinished send
//	             names it
//	delete       merge message MessageID into SurvivorMessageID, a copy with
//	             the same direction, sender, occurrence time and body (see
//	             mergeDuplicateMessage), then delete it; refused while an
//	             unfinished send names it
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
	SurvivorMessageID      string              `json:"survivor_message_id,omitempty"`
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

// ApplyRepairPlan executes the steps in order inside one transaction. A step
// naming a message that isn't in the account (gone already, when a plan is
// applied again) is a no-op; any other failure rolls the whole plan back.
func (s *Store) ApplyRepairPlan(ctx context.Context, accountID string, steps []RepairStep, nowMS int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("apply repair plan: begin: %w", err)
	}
	defer tx.Rollback()

	// mergedInto maps each message a delete step removed to the copy it was
	// merged into, so a later step whose survivor is already gone reaches the
	// copy that stayed.
	mergedInto := make(map[string]string)

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
			stepErr = applyRepairMove(ctx, tx, accountID, step, nowMS)
		case "delete":
			stepErr = applyRepairDelete(ctx, tx, accountID, step, mergedInto, nowMS)
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

func applyRepairMove(ctx context.Context, tx *sql.Tx, accountID string, step RepairStep, nowMS int64) error {
	inAccount, err := messageInAccount(ctx, tx, accountID, step.MessageID)
	if err != nil || !inAccount {
		return err
	}
	deviceID, found, err := readCursorDeviceOutside(ctx, tx, step.MessageID, step.TargetConversationID)
	if err != nil {
		return err
	}
	if found {
		return fmt.Errorf(
			"the read cursor of device %q names message %q, and a cursor can't follow it into conversation %q",
			deviceID,
			step.MessageID,
			step.TargetConversationID,
		)
	}
	if err := refuseOpenSend(ctx, tx, step.MessageID, step.TargetConversationID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE reactions SET conversation_id = ?, updated_at_ms = MAX(updated_at_ms, ?)
		WHERE message_id = ?
	`, step.TargetConversationID, nowMS, step.MessageID); err != nil {
		return err
	}
	if err := retargetMessageIntents(ctx, tx, step.MessageID, step.TargetConversationID, nowMS); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		UPDATE messages SET conversation_id = ?, updated_at_ms = MAX(updated_at_ms, ?)
		WHERE message_id = ? AND account_id = ?
	`, step.TargetConversationID, nowMS, step.MessageID, accountID)
	return err
}

// applyRepairDelete merges a duplicate into its survivor and deletes it. A
// plain DELETE would fail on the NO ACTION intents that still name the
// duplicate, and cascade away its reactions, fence and attachments.
func applyRepairDelete(
	ctx context.Context,
	tx *sql.Tx,
	accountID string,
	step RepairStep,
	mergedInto map[string]string,
	nowMS int64,
) error {
	if step.SurvivorMessageID == "" {
		return fmt.Errorf(
			"delete of message %q names no survivor; a plain delete would cascade away its reactions and attachments",
			step.MessageID,
		)
	}
	inAccount, err := messageInAccount(ctx, tx, accountID, step.MessageID)
	if err != nil || !inAccount {
		return err
	}
	// Every conversation id is non-empty, so this refuses any open send.
	if err := refuseOpenSend(ctx, tx, step.MessageID, ""); err != nil {
		return err
	}
	survivorID := step.SurvivorMessageID
	// Every hop lands on a message that existed when it was merged into, so
	// the walk ends within len(mergedInto) hops.
	for range len(mergedInto) {
		next, merged := mergedInto[survivorID]
		if !merged {
			break
		}
		survivorID = next
	}
	duplicate, err := readMessageContent(ctx, tx, step.MessageID)
	if err != nil {
		return err
	}
	survivor, err := readMessageContent(ctx, tx, survivorID)
	if err != nil {
		return fmt.Errorf("read survivor %q: %w", survivorID, err)
	}
	if survivor != duplicate {
		return fmt.Errorf("survivor %q holds different content than message %q", survivorID, step.MessageID)
	}
	if err := mergeDuplicateMessage(ctx, tx, step.MessageID, survivorID, nowMS); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM messages WHERE message_id = ? AND account_id = ?
	`, step.MessageID, accountID); err != nil {
		return err
	}
	mergedInto[step.MessageID] = survivorID
	return nil
}

// refuseOpenSend fails if an unfinished send names messageID as its local
// copy and belongs to a conversation other than conversationID.
func refuseOpenSend(ctx context.Context, tx *sql.Tx, messageID, conversationID string) error {
	var outboxID, state string
	err := tx.QueryRowContext(ctx, `
		SELECT outbox_id, state FROM outbox
		WHERE local_message_id = ? AND conversation_id <> ? AND state NOT IN `+closedOutboxStates+`
		ORDER BY outbox_id
		LIMIT 1
	`, messageID, conversationID).Scan(&outboxID, &state)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil
	case err != nil:
		return fmt.Errorf("check open sends for message %q: %w", messageID, err)
	default:
		return fmt.Errorf(
			"unfinished send %q (%s) names message %q as its local copy, and dispatch reads it in the send's own conversation",
			outboxID,
			state,
			messageID,
		)
	}
}

// messageContent is what makes two rows the same message for a repair: the
// planner's content key.
type messageContent struct {
	direction        string
	senderIdentityID sql.NullString
	occurredAtMS     int64
	body             string
}

func readMessageContent(ctx context.Context, tx *sql.Tx, messageID string) (messageContent, error) {
	var content messageContent
	err := tx.QueryRowContext(ctx, `
		SELECT direction, sender_identity_id, occurred_at_ms, body FROM messages WHERE message_id = ?
	`, messageID).Scan(&content.direction, &content.senderIdentityID, &content.occurredAtMS, &content.body)
	if errors.Is(err, sql.ErrNoRows) {
		return messageContent{}, notFound("message", messageID)
	}
	if err != nil {
		return messageContent{}, fmt.Errorf("read message %q: %w", messageID, err)
	}
	return content, nil
}

func messageInAccount(ctx context.Context, tx *sql.Tx, accountID, messageID string) (bool, error) {
	var exists bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM messages WHERE message_id = ? AND account_id = ?)
	`, messageID, accountID).Scan(&exists); err != nil {
		return false, fmt.Errorf("look up message %q: %w", messageID, err)
	}
	return exists, nil
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
