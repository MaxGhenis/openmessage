package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// messageContentDuplicateQuery runs for every Google message with a body. It
// must seek on the exact occurrence millisecond: through
// messages_conversation_time_idx, or messages_account_direction_time_idx, which
// the planner prefers for its third equality. Either reads only that
// millisecond's rows, and only those rows look up their outbox row, through
// outbox_local_message_idx.
const messageContentDuplicateQuery = `
	SELECT ` + messageColumns + `
	FROM messages
	WHERE account_id = ?
	  AND conversation_id = ?
	  AND occurred_at_ms = ?
	  AND direction = ?
	  AND body = ?
	  AND sender_identity_id IS ?
	  AND remote_message_id <> ?
	  AND NOT EXISTS (
	      SELECT 1 FROM outbox AS o
	      WHERE o.local_message_id = messages.message_id
	        AND o.transport_request_id = messages.remote_message_id
	  )
	ORDER BY created_at_ms, message_id
	LIMIT 1
`

// FindMessageContentDuplicate returns a message in the same conversation with
// identical content (direction, sender, occurrence millisecond, and body) but
// a different remote message ID. This is the signature of a re-delivery after
// a device ID-space reset: the phone re-serves an already-stored message under
// fresh conversation and message IDs, so the remote natural key cannot dedupe
// it. Millisecond-exact occurrence plus identical sender and body cannot name
// two distinct messages stamped by the same clock.
//
// A send still waiting for its echo never matches. Its row still holds the
// transport request ID, not a remote ID the phone served, and its time comes
// from this machine's clock when the app created the send, not from the
// phone's. An equal millisecond is then a coincidence across clocks: on CI
// (2026-11-03) the phone stamped a later identical send's echo on an earlier
// send's local millisecond, this check took the echo for that send's
// re-delivery and dropped it, and the later send never paired. Echo
// correlation pairs a send with its echo (TmpID, or pairEchoes by receipt
// time); this check must not decide it first.
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
	message, err := scanMessage(r.store.db.QueryRowContext(ctx, messageContentDuplicateQuery,
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
