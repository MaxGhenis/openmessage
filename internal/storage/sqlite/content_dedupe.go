package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// FindMessageContentDuplicate returns a message in the same conversation with
// identical content (direction, sender, occurrence millisecond, and body) but
// a different remote message ID. This is the signature of a re-delivery after
// a device ID-space reset: the phone re-serves an already-stored message under
// fresh conversation and message IDs, so the remote natural key cannot dedupe
// it. Millisecond-exact occurrence plus identical sender and body cannot name
// two distinct messages.
func (r *MessageRepository) FindMessageContentDuplicate(
	ctx context.Context,
	accountID string,
	conversationID string,
	remoteMessageID string,
	direction MessageDirection,
	senderIdentityID *string,
	occurredAtMS int64,
	body string,
) (Message, bool, error) {
	message, err := scanMessage(r.store.db.QueryRowContext(ctx, `
		SELECT `+messageColumns+`
		FROM messages
		WHERE account_id = ?
		  AND conversation_id = ?
		  AND occurred_at_ms = ?
		  AND direction = ?
		  AND body = ?
		  AND sender_identity_id IS ?
		  AND remote_message_id <> ?
		ORDER BY created_at_ms, message_id
		LIMIT 1
	`,
		accountID,
		conversationID,
		occurredAtMS,
		direction,
		body,
		senderIdentityID,
		remoteMessageID,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return Message{}, false, nil
	}
	if err != nil {
		return Message{}, false, fmt.Errorf(
			"find content duplicate in conversation %q at %d: %w",
			conversationID,
			occurredAtMS,
			err,
		)
	}
	return message, true, nil
}
