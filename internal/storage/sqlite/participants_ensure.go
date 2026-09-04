package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// EnsureConversationParticipant inserts one participant link if the
// (conversation, identity) pair is absent, leaving existing rows untouched.
// It reports whether a row was inserted. Account rules match
// ReplaceConversationParticipants: an empty account inherits the
// conversation's, a mismatched one is rejected.
func (s *Store) EnsureConversationParticipant(
	participant ConversationParticipant,
) (bool, error) {
	ctx := context.Background()
	conversationID := participant.ConversationID
	if conversationID == "" {
		return false, invalidParticipantError(nil, "participant names no conversation")
	}

	var conversationAccountID string
	if err := s.db.QueryRowContext(
		ctx,
		`SELECT account_id FROM conversations WHERE conversation_id = ?`,
		conversationID,
	).Scan(&conversationAccountID); errors.Is(err, sql.ErrNoRows) {
		return false, notFound("conversation", conversationID)
	} else if err != nil {
		return false, fmt.Errorf(
			"ensure participant for conversation %q: read account: %w", conversationID, err,
		)
	}

	accountID := participant.AccountID
	if accountID == "" {
		accountID = conversationAccountID
	}
	if accountID != conversationAccountID {
		return false, invalidParticipantError(
			ErrCrossAccountParticipant,
			"participant account %q does not match conversation account %q",
			accountID,
			conversationAccountID,
		)
	}

	result, err := s.db.ExecContext(ctx, `
		INSERT INTO conversation_participants (
			account_id,
			conversation_id,
			identity_id,
			role,
			display_name,
			is_active,
			joined_at_ms,
			left_at_ms
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (conversation_id, identity_id) DO NOTHING
	`,
		accountID,
		conversationID,
		participant.IdentityID,
		participant.Role,
		participant.DisplayName,
		participant.IsActive,
		participant.JoinedAtMS,
		participant.LeftAtMS,
	)
	if err != nil {
		if isSQLiteConstraint(err) {
			return false, invalidParticipantConstraintError(
				nil,
				err,
				"ensure participant identity %q",
				participant.IdentityID,
			)
		}
		return false, fmt.Errorf(
			"ensure participant for conversation %q identity %q: %w",
			conversationID,
			participant.IdentityID,
			err,
		)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf(
			"ensure participant for conversation %q identity %q: rows affected: %w",
			conversationID,
			participant.IdentityID,
			err,
		)
	}
	return inserted > 0, nil
}

// ListDirectConversationsWithoutParticipants returns direct conversations
// that carry no participant rows at all — the shape minted by message-frame
// projection before peers were ensured (issue #155).
func (s *Store) ListDirectConversationsWithoutParticipants() ([]Conversation, error) {
	rows, err := s.db.QueryContext(
		context.Background(),
		"SELECT "+conversationColumns+`
		 FROM conversations
		 WHERE kind = 'direct'
		   AND NOT EXISTS (
		       SELECT 1 FROM conversation_participants p
		       WHERE p.conversation_id = conversations.conversation_id
		   )
		 ORDER BY conversation_id`,
	)
	if err != nil {
		return nil, fmt.Errorf("list direct conversations without participants: %w", err)
	}
	conversations, err := collectRows(rows, scanConversation)
	if err != nil {
		return nil, fmt.Errorf("list direct conversations without participants: %w", err)
	}
	return conversations, nil
}

// CountDistinctInboundSenders returns how many distinct sender identities have
// sent an inbound message in a conversation. Two or more is positive evidence
// that the thread is not a 1:1 — the one signal available for platforms whose
// remote conversation ID is opaque about shape (Google Messages).
func (s *Store) CountDistinctInboundSenders(conversationID string) (int, error) {
	var count int
	if err := s.db.QueryRowContext(
		context.Background(),
		`SELECT COUNT(DISTINCT sender_identity_id)
		 FROM messages
		 WHERE conversation_id = ?
		   AND direction = 'incoming'
		   AND sender_identity_id IS NOT NULL`,
		conversationID,
	).Scan(&count); err != nil {
		return 0, fmt.Errorf(
			"count distinct inbound senders for conversation %q: %w", conversationID, err,
		)
	}
	return count, nil
}

// LatestInboundSenderIdentityID returns the sender identity of the most
// recent inbound message in a conversation, or ok=false when the
// conversation has no attributed inbound messages.
func (s *Store) LatestInboundSenderIdentityID(
	conversationID string,
) (string, bool, error) {
	var identityID string
	err := s.db.QueryRowContext(
		context.Background(),
		`SELECT sender_identity_id
		 FROM messages
		 WHERE conversation_id = ?
		   AND direction = 'incoming'
		   AND sender_identity_id IS NOT NULL
		 ORDER BY occurred_at_ms DESC, message_id
		 LIMIT 1`,
		conversationID,
	).Scan(&identityID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf(
			"latest inbound sender for conversation %q: %w", conversationID, err,
		)
	}
	return identityID, identityID != "", nil
}
