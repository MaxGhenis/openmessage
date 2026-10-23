package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// mergeDuplicateMessage merges duplicateID into survivorID, two rows holding
// the same message, so that deleting the duplicate afterwards neither trips a
// NO ACTION foreign key nor cascades away state recorded on it. The caller
// deletes the duplicate. It covers every table with a foreign key to
// messages; outbox.local_message_id has none, and callers keep a row an
// unfinished send names out of the duplicate's place. Both rows must exist in
// one account; the survivor may be in another conversation (a repair keeps the
// copy in the thread the message belongs to). The rules:
//   - read_cursors are repointed when both rows are in one conversation. A
//     cursor's foreign key is (conversation_id, last_read_message_id), so it
//     can't follow its message into another conversation; the merge then
//     fails instead.
//   - outbox_reactions and outbox_read_receipts target the same logical
//     message, so they're repointed, and their outbox row takes the
//     survivor's conversation: the dispatcher refuses a target outside its
//     intent's conversation.
//   - reactions: when both rows have one for the same reactor, the newer one
//     stays and the other is deleted (the survivor's stays on an exact tie).
//     If either row has a reaction_snapshot_fences row, the message's
//     reactions come from full embedded snapshots. ReplaceEmbeddedReactions
//     orders those by source_seq_ms and stamps occurred_at_ms with the write
//     time, so newer means a larger (source_seq_ms, occurred_at_ms). Then,
//     as applying the newest snapshot would, a reactor still active below
//     the merged fence is tombstoned, because that snapshot didn't list it
//     (see tombstoneReactionsBelowFence for how that tombstone ranks).
//     Without a fence the rows are deltas, and newer means a larger
//     (occurred_at_ms, source_seq_ms). ApplyReaction orders deltas by
//     occurred_at_ms and lets a later write replace an equal-time row.
//     source_seq_ms is the frame's receive time in milliseconds, so the
//     larger one is the later write whenever frames were applied in the
//     order they arrived and arrived in different milliseconds. Otherwise (a
//     replay, or two frames in one millisecond) the rows don't record which
//     write one message would have kept: updated_at_ms doesn't either,
//     because rewriting a row with what it already holds leaves it
//     untouched. Moved rows take the survivor's conversation.
//   - reaction_snapshot_fences keeps the larger source_seq_ms.
//   - message_attachments: per ordinal, a downloaded survivor row stays.
//     Otherwise the duplicate's row replaces the survivor's, since a pending
//     row holds no bytes.
//
// Losing survivor rows are deleted here, and losing duplicate rows go with
// the duplicate. If both rows have the same fence, a reactor active in one
// stays active unless the other holds a tombstone for it written later at
// that fence.
func mergeDuplicateMessage(
	ctx context.Context,
	tx *sql.Tx,
	duplicateID, survivorID string,
	nowMS int64,
) error {
	if err := mergeDuplicateRows(ctx, tx, duplicateID, survivorID, nowMS); err != nil {
		return err
	}
	return tombstoneReactionsBelowFence(ctx, tx, survivorID, nowMS)
}

// mergeDuplicateRows is mergeDuplicateMessage without its last step, the
// tombstone of reactors still active below the merged fence. A caller that
// merges several duplicates into one survivor runs that step once, after the
// last of them (tombstoneReactionsBelowFence), so that every comparison is
// between rows the writers stored and a removed reactor keeps the row that
// removed it.
func mergeDuplicateRows(
	ctx context.Context,
	tx *sql.Tx,
	duplicateID, survivorID string,
	nowMS int64,
) error {
	if duplicateID == survivorID {
		return fmt.Errorf("message %q is its own survivor", duplicateID)
	}
	duplicateAccountID, duplicateConversationID, err := messageLocation(ctx, tx, duplicateID)
	if err != nil {
		return fmt.Errorf("read duplicate %q: %w", duplicateID, err)
	}
	survivorAccountID, survivorConversationID, err := messageLocation(ctx, tx, survivorID)
	if err != nil {
		return fmt.Errorf("read survivor %q: %w", survivorID, err)
	}
	if survivorAccountID != duplicateAccountID {
		return fmt.Errorf(
			"survivor %q is in account %q, duplicate %q in account %q",
			survivorID,
			survivorAccountID,
			duplicateID,
			duplicateAccountID,
		)
	}
	if survivorConversationID != duplicateConversationID {
		deviceID, found, err := readCursorDeviceOutside(ctx, tx, duplicateID, survivorConversationID)
		if err != nil {
			return err
		}
		if found {
			return fmt.Errorf(
				"the read cursor of device %q names message %q, and a cursor can't follow it into conversation %q",
				deviceID,
				duplicateID,
				survivorConversationID,
			)
		}
	}
	if err := retargetMessageIntents(ctx, tx, duplicateID, survivorConversationID, nowMS); err != nil {
		return err
	}

	var fence sql.NullInt64
	if err := tx.QueryRowContext(ctx, `
		SELECT MAX(source_seq_ms)
		FROM reaction_snapshot_fences
		WHERE message_id IN (?, ?)
	`, duplicateID, survivorID).Scan(&fence); err != nil {
		return fmt.Errorf("read reaction snapshot fences: %w", err)
	}
	duplicateReactionIsNewer := `(duplicate.occurred_at_ms, duplicate.source_seq_ms) >
		(reactions.occurred_at_ms, reactions.source_seq_ms)`
	if fence.Valid {
		duplicateReactionIsNewer = `(duplicate.source_seq_ms, duplicate.occurred_at_ms) >
			(reactions.source_seq_ms, reactions.occurred_at_ms)`
	}

	arguments := []any{
		sql.Named("survivor_conversation_id", survivorConversationID),
		sql.Named("duplicate_id", duplicateID),
		sql.Named("survivor_id", survivorID),
		sql.Named("now_ms", nowMS),
	}
	steps := []struct {
		name  string
		query string
	}{
		{
			name: "repoint read cursors",
			query: `
				UPDATE read_cursors
				SET last_read_message_id = :survivor_id,
					updated_at_ms = MAX(updated_at_ms, :now_ms)
				WHERE conversation_id = :survivor_conversation_id
				  AND last_read_message_id = :duplicate_id
			`,
		},
		{
			name: "repoint reaction intents",
			query: `
				UPDATE outbox_reactions
				SET target_message_id = :survivor_id
				WHERE target_message_id = :duplicate_id
			`,
		},
		{
			name: "repoint read-receipt intents",
			query: `
				UPDATE outbox_read_receipts
				SET last_read_message_id = :survivor_id
				WHERE last_read_message_id = :duplicate_id
			`,
		},
		{
			name: "drop survivor reactions the duplicate supersedes",
			query: `
				DELETE FROM reactions
				WHERE message_id = :survivor_id
				  AND EXISTS (
					SELECT 1
					FROM reactions AS duplicate
					WHERE duplicate.message_id = :duplicate_id
					  AND duplicate.reactor_key = reactions.reactor_key
					  AND ` + duplicateReactionIsNewer + `
				  )
			`,
		},
		{
			name: "move reactions",
			query: `
				UPDATE reactions
				SET message_id = :survivor_id,
					conversation_id = :survivor_conversation_id,
					updated_at_ms = MAX(updated_at_ms, :now_ms)
				WHERE message_id = :duplicate_id
				  AND NOT EXISTS (
					SELECT 1
					FROM reactions AS survivor
					WHERE survivor.message_id = :survivor_id
					  AND survivor.reactor_key = reactions.reactor_key
				  )
			`,
		},
		{
			name: "merge reaction snapshot fence",
			query: `
				INSERT INTO reaction_snapshot_fences (message_id, source_seq_ms, updated_at_ms)
				SELECT :survivor_id, source_seq_ms, :now_ms
				FROM reaction_snapshot_fences
				WHERE message_id = :duplicate_id
				ON CONFLICT(message_id) DO UPDATE SET
					source_seq_ms = excluded.source_seq_ms,
					updated_at_ms = MAX(reaction_snapshot_fences.updated_at_ms, excluded.updated_at_ms)
				WHERE excluded.source_seq_ms > reaction_snapshot_fences.source_seq_ms
			`,
		},
		{
			name: "drop pending survivor attachments the duplicate covers",
			query: `
				DELETE FROM message_attachments
				WHERE message_id = :survivor_id
				  AND blob_hash IS NULL
				  AND EXISTS (
					SELECT 1
					FROM message_attachments AS duplicate
					WHERE duplicate.message_id = :duplicate_id
					  AND duplicate.ordinal = message_attachments.ordinal
				  )
			`,
		},
		{
			name: "move attachments",
			query: `
				UPDATE message_attachments
				SET message_id = :survivor_id,
					updated_at_ms = MAX(updated_at_ms, :now_ms)
				WHERE message_id = :duplicate_id
				  AND NOT EXISTS (
					SELECT 1
					FROM message_attachments AS survivor
					WHERE survivor.message_id = :survivor_id
					  AND survivor.ordinal = message_attachments.ordinal
				  )
			`,
		},
	}
	for _, step := range steps {
		if _, err := tx.ExecContext(ctx, step.query, arguments...); err != nil {
			return fmt.Errorf("%s: %w", step.name, err)
		}
	}
	return nil
}

// inferredTombstoneOccurredAtMS is the occurred_at_ms of a tombstone the merge
// infers from a fence. No snapshot wrote that removal: the merge concludes it
// because the newest snapshot it has seen didn't list the reactor. Snapshot
// rows carry the time they were written, and this is the smallest value the
// schema allows, so the tombstone ranks below any row a snapshot wrote at the
// same fence (it would only tie one written in the first millisecond of
// 1970). If a copy merged later, in this plan or a later repair, holds a
// reaction a snapshot at that fence did list, that reaction wins. Stamped
// with the merge time instead, the tombstone would outrank it and an active
// reaction would be lost.
//
// Nothing else depends on the value. Reads select active reactions only.
// ApplyReaction does compare occurred_at_ms, so a later add would revive the
// reactor, but the ingest worker sends Google reactions through
// ReplaceEmbeddedReactions only, and fences originate only there: a merge
// copies a duplicate's fence to its survivor, it never invents one.
const inferredTombstoneOccurredAtMS = 1

// tombstoneReactionsBelowFence marks removed every reaction on messageID that
// is still active below the message's snapshot fence: the newest snapshot
// didn't list that reactor. It is a no-op for a message without a fence.
func tombstoneReactionsBelowFence(ctx context.Context, tx *sql.Tx, messageID string, nowMS int64) error {
	if _, err := tx.ExecContext(ctx, `
		UPDATE reactions
		SET state = 'removed',
			occurred_at_ms = :inferred_occurred_at_ms,
			source_seq_ms = fence.source_seq_ms,
			updated_at_ms = MAX(reactions.updated_at_ms, :now_ms)
		FROM reaction_snapshot_fences AS fence
		WHERE fence.message_id = :message_id
		  AND reactions.message_id = :message_id
		  AND reactions.state = 'active'
		  AND reactions.source_seq_ms < fence.source_seq_ms
	`,
		sql.Named("message_id", messageID),
		sql.Named("now_ms", nowMS),
		sql.Named("inferred_occurred_at_ms", inferredTombstoneOccurredAtMS),
	); err != nil {
		return fmt.Errorf("tombstone reactions the newest snapshot omitted on message %q: %w", messageID, err)
	}
	return nil
}

// retargetMessageIntents gives every reaction and read-receipt intent that
// targets messageID the conversation messageID is moving to. The dispatcher
// resolves an intent's target only inside the intent's own conversation, so
// an intent left behind would fail its pre-call check on every retry.
func retargetMessageIntents(
	ctx context.Context,
	tx *sql.Tx,
	messageID, conversationID string,
	nowMS int64,
) error {
	if _, err := tx.ExecContext(ctx, `
		UPDATE outbox
		SET conversation_id = :conversation_id,
			updated_at_ms = MAX(updated_at_ms, :now_ms)
		WHERE conversation_id <> :conversation_id
		  AND outbox_id IN (
			SELECT outbox_id FROM outbox_reactions WHERE target_message_id = :message_id
			UNION ALL
			SELECT outbox_id FROM outbox_read_receipts WHERE last_read_message_id = :message_id
		  )
	`,
		sql.Named("conversation_id", conversationID),
		sql.Named("message_id", messageID),
		sql.Named("now_ms", nowMS),
	); err != nil {
		return fmt.Errorf("retarget intents on message %q to conversation %q: %w", messageID, conversationID, err)
	}
	return nil
}

// messageLocation returns a message's account and conversation, wrapping
// ErrNotFound when the row doesn't exist.
func messageLocation(ctx context.Context, tx *sql.Tx, messageID string) (string, string, error) {
	var accountID, conversationID string
	err := tx.QueryRowContext(ctx, `
		SELECT account_id, conversation_id FROM messages WHERE message_id = ?
	`, messageID).Scan(&accountID, &conversationID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", notFound("message", messageID)
	}
	if err != nil {
		return "", "", err
	}
	return accountID, conversationID, nil
}

// readCursorDeviceOutside reports a device whose read cursor names messageID
// in a conversation other than conversationID: a cursor that would break if
// the message left its conversation.
func readCursorDeviceOutside(ctx context.Context, tx *sql.Tx, messageID, conversationID string) (string, bool, error) {
	var deviceID string
	err := tx.QueryRowContext(
		ctx, readCursorDeviceOutsideQuery, messageID, conversationID, messageID,
	).Scan(&deviceID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("check read cursors on message %q: %w", messageID, err)
	}
	return deviceID, true, nil
}

// readCursorDeviceOutsideQuery finds such a cursor without an index on
// last_read_message_id: the cursor's composite foreign key
// (conversation_id, last_read_message_id) -> messages(conversation_id,
// message_id) puts any cursor naming the message in the message's own
// conversation, so the lookup seeks read_cursors_conversation_idx there
// instead of scanning every cursor.
const readCursorDeviceOutsideQuery = `
	SELECT device_id FROM read_cursors
	WHERE conversation_id = (SELECT conversation_id FROM messages WHERE message_id = ?)
	  AND conversation_id <> ?
	  AND last_read_message_id = ?
	ORDER BY device_id
	LIMIT 1
`
