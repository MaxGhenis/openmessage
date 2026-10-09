package sqlite

import (
	"context"
	"fmt"
	"time"
)

// DefaultInboxPayloadRetention is how long a processed frame keeps its payload
// before PruneInboxPayloads may empty it.
const DefaultInboxPayloadRetention = 60 * 24 * time.Hour

// MinInboxPayloadRetention is the shortest retention PruneInboxPayloads
// accepts. A reader that decodes payloads over a receipt-time window must keep
// that window at or below this floor, so it never meets a pruned frame.
const MinInboxPayloadRetention = 45 * 24 * time.Hour

// InboxPruneResult reports one PruneInboxPayloads batch.
type InboxPruneResult struct {
	// CutoffMS is the processing time before which frames were eligible.
	CutoffMS int64
	// Rows is how many payloads the batch emptied.
	Rows int
	// Bytes is the payload size those rows held before pruning.
	Bytes int64
}

// MarkInboxQuarantined records that the worker gave up on a frame. The frame
// leaves the unprocessed queue exactly as MarkInboxProcessed would take it out,
// and quarantined_at_ms exempts its payload from retention, since that payload
// is the only record of what the frame carried.
//
// A frame that is already processed keeps its processed_at_ms and gains the
// mark; that is a frame whose message projection committed before a later
// event in it failed. A pruned frame has no payload left to keep and is left
// unchanged. Repeated calls keep the first mark, and a missing row is a
// no-op, as with MarkInboxProcessed.
func (r *MessageRepository) MarkInboxQuarantined(
	ctx context.Context,
	inboxID string,
	accountID string,
) error {
	nowMS, err := r.nowMS("mark inbox quarantined")
	if err != nil {
		return err
	}
	_, err = r.store.db.ExecContext(ctx, `
		UPDATE inbox
		SET processed_at_ms = COALESCE(processed_at_ms, ?),
		    quarantined_at_ms = COALESCE(quarantined_at_ms, ?)
		WHERE inbox_id = ?
		  AND account_id = ?
		  AND payload_pruned_at_ms IS NULL
	`, nowMS, nowMS, inboxID, accountID)
	return inboxMarkError(err, "quarantined", inboxID, accountID)
}

// MarkInboxApplied records that the worker applied every event of a processed
// frame. Until then the frame's payload is never pruned: processing only says
// the frame left the queue, which its first message projection does before
// the rest of the frame is applied. Repeated calls keep the first mark; an
// unprocessed or missing row is a no-op.
func (r *MessageRepository) MarkInboxApplied(
	ctx context.Context,
	inboxID string,
	accountID string,
) error {
	nowMS, err := r.nowMS("mark inbox applied")
	if err != nil {
		return err
	}
	_, err = r.store.db.ExecContext(ctx, `
		UPDATE inbox
		SET applied_at_ms = ?
		WHERE inbox_id = ?
		  AND account_id = ?
		  AND processed_at_ms IS NOT NULL
		  AND applied_at_ms IS NULL
	`, nowMS, inboxID, accountID)
	return inboxMarkError(err, "applied", inboxID, accountID)
}

func inboxMarkError(err error, state, inboxID, accountID string) error {
	if err == nil {
		return nil
	}
	if isSQLiteConstraint(err) {
		return fmt.Errorf(
			"mark inbox %q for account %q %s: %w: %w",
			inboxID,
			accountID,
			state,
			ErrInvalidInboxRecord,
			mapConstraintError(err),
		)
	}
	return fmt.Errorf(
		"mark inbox %q for account %q %s: %w",
		inboxID,
		accountID,
		state,
		err,
	)
}

// PruneInboxPayloads empties the payloads of at most limit frames that the
// worker fully applied, never quarantined, and processed more than retention
// ago, oldest first, in one transaction. Callers loop until a batch prunes
// fewer than limit rows.
//
// Only payload and payload_pruned_at_ms change. The row keeps its dedupe key,
// so a replayed frame still collapses onto it instead of projecting again, and
// rows are never deleted, so rowids stay monotonic. Unprocessed, quarantined,
// and processed-but-unapplied frames are never touched. Processing never
// precedes receipt, so every frame received inside the retention window keeps
// its payload; the query checks received_at_ms as well so that holds even
// without the schema constraint.
func (r *MessageRepository) PruneInboxPayloads(
	ctx context.Context,
	retention time.Duration,
	limit int,
) (InboxPruneResult, error) {
	if retention < MinInboxPayloadRetention {
		return InboxPruneResult{}, fmt.Errorf(
			"prune inbox payloads: retention %s is shorter than the %s floor",
			retention,
			MinInboxPayloadRetention,
		)
	}
	if limit <= 0 {
		return InboxPruneResult{}, fmt.Errorf("prune inbox payloads: batch limit %d is not positive", limit)
	}
	nowMS, err := r.nowMS("prune inbox payloads")
	if err != nil {
		return InboxPruneResult{}, err
	}
	result := InboxPruneResult{CutoffMS: nowMS - retention.Milliseconds()}
	if result.CutoffMS <= 0 {
		return result, nil
	}

	tx, err := r.store.db.BeginTx(ctx, nil)
	if err != nil {
		return result, fmt.Errorf("prune inbox payloads: begin transaction: %w", err)
	}
	defer tx.Rollback()

	type candidate struct {
		inboxID string
		bytes   int64
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT inbox_id, length(payload)
		FROM inbox
		WHERE applied_at_ms IS NOT NULL
		  AND quarantined_at_ms IS NULL
		  AND payload_pruned_at_ms IS NULL
		  AND processed_at_ms < ?
		  AND received_at_ms < ?
		ORDER BY processed_at_ms, inbox_id
		LIMIT ?
	`, result.CutoffMS, result.CutoffMS, limit)
	if err != nil {
		return result, fmt.Errorf("prune inbox payloads: select candidates: %w", err)
	}
	candidates, err := collectRows(rows, func(row rowScanner) (candidate, error) {
		var c candidate
		err := row.Scan(&c.inboxID, &c.bytes)
		return c, err
	})
	if err != nil {
		return result, fmt.Errorf("prune inbox payloads: select candidates: %w", err)
	}
	if len(candidates) == 0 {
		return result, nil
	}

	update, err := tx.PrepareContext(ctx, `
		UPDATE inbox
		SET payload = X'',
		    payload_pruned_at_ms = ?
		WHERE inbox_id = ?
		  AND applied_at_ms IS NOT NULL
		  AND quarantined_at_ms IS NULL
		  AND payload_pruned_at_ms IS NULL
		  AND processed_at_ms < ?
		  AND received_at_ms < ?
	`)
	if err != nil {
		return result, fmt.Errorf("prune inbox payloads: prepare update: %w", err)
	}
	defer update.Close()
	for _, c := range candidates {
		updated, err := update.ExecContext(ctx, nowMS, c.inboxID, result.CutoffMS, result.CutoffMS)
		if err != nil {
			return InboxPruneResult{CutoffMS: result.CutoffMS}, fmt.Errorf(
				"prune inbox payloads: empty inbox %q: %w",
				c.inboxID,
				err,
			)
		}
		affected, err := updated.RowsAffected()
		if err != nil {
			return InboxPruneResult{CutoffMS: result.CutoffMS}, fmt.Errorf(
				"prune inbox payloads: empty inbox %q: read rows affected: %w",
				c.inboxID,
				err,
			)
		}
		if affected != 1 {
			return InboxPruneResult{CutoffMS: result.CutoffMS}, fmt.Errorf(
				"prune inbox payloads: empty inbox %q: affected %d rows, want 1",
				c.inboxID,
				affected,
			)
		}
		result.Rows++
		result.Bytes += c.bytes
	}
	if err := tx.Commit(); err != nil {
		return InboxPruneResult{CutoffMS: result.CutoffMS}, fmt.Errorf("prune inbox payloads: commit: %w", err)
	}
	return result, nil
}
