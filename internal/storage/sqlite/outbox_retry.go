package sqlite

import (
	"context"
	"fmt"
	"time"

	"github.com/maxghenis/openmessage/internal/bridge"
)

// RetryExhaustedErrorClass is the error_class recorded on an outbox intent
// that the dispatcher rejected after its transport retry budget ran out. The
// intent was never dispatched; it is terminal (state rejected) and may only be
// sent again explicitly.
const RetryExhaustedErrorClass = "retry_exhausted"

// DefaultMaxTransportAttempts is the transport retry budget of one outbox
// intent: the dispatcher rejects the intent (RetryExhaustedErrorClass) on the
// budget-consuming failure that reaches it and never calls the transport again
// for a row at or over it. The cutover carry refuses to recreate such a row.
const DefaultMaxTransportAttempts int64 = 6

// TrayRejectedWindow is how long after its last update a rejected text or
// media intent the user can act on stays listed by ListPending.
const TrayRejectedWindow = 24 * time.Hour

// MarkCalledNotDispatchedRefundingAttempt is MarkCalledNotDispatched for a
// failure that must not consume the intent's retry budget (the account was
// offline, or its credentials are being repaired): it makes the same
// transition under the same lease and call-marker predicates and gives back
// the attempt MarkTransportCalled charged. attempt_count never drops below 0.
func (r *OutboxRepository) MarkCalledNotDispatchedRefundingAttempt(
	ctx context.Context,
	outboxID, leaseToken, class, code, detail string,
	retryAt time.Time,
) error {
	retryAtMS := retryAt.UnixMilli()
	if retryAtMS <= 0 {
		return fmt.Errorf("mark outbox item %q not dispatched: retry time is not positive", outboxID)
	}
	nowMS, err := r.nowMS("mark outbox item not dispatched")
	if err != nil {
		return err
	}
	result, err := r.store.db.ExecContext(ctx, `
		UPDATE outbox
		SET state = 'not_dispatched',
			error_class = ?,
			error_code = ?,
			error_detail = ?,
			attempt_count = CASE WHEN attempt_count > 0 THEN attempt_count - 1 ELSE 0 END,
			next_attempt_at_ms = ?,
			lease_owner = NULL,
			lease_token = NULL,
			lease_expires_at_ms = NULL,
			transport_called_at_ms = NULL,
			updated_at_ms = ?
		WHERE outbox_id = ?
		  AND state = 'dispatching'
		  AND lease_token = ?
		  AND transport_called_at_ms IS NOT NULL
	`,
		nullableOutboxText(class),
		nullableOutboxText(code),
		nullableOutboxText(detail),
		retryAtMS,
		nowMS,
		outboxID,
		leaseToken,
	)
	if err != nil {
		return fmt.Errorf("mark outbox item %q not dispatched: %w", outboxID, err)
	}
	return r.requireLeaseMutation(ctx, "mark called not dispatched (refunded)", outboxID, result)
}

// trayRejectedClause returns the ListPending predicate (and its arguments)
// that keeps a rejected intent visible while the user can still act on it:
// a text or media intent that gave up after its retry budget or needs the
// account re-linked, updated within TrayRejectedWindow of nowMS, and not yet
// sent again. Other rejections (for example an unsupported operation) stay
// out of the tray. The NOT EXISTS probe is served by outbox_send_again_of_idx.
func trayRejectedClause(nowMS int64) (string, []any) {
	clause := `(o.state = 'rejected'
			  AND o.kind IN ('text', 'media')
			  AND o.error_class IN (?, ?)
			  AND o.updated_at_ms > ?
			  AND NOT EXISTS (
				  SELECT 1 FROM outbox successor
				  WHERE successor.send_again_of_outbox_id = o.outbox_id
			  ))`
	return clause, []any{
		RetryExhaustedErrorClass,
		string(bridge.FailureReauthRequired),
		nowMS - TrayRejectedWindow.Milliseconds(),
	}
}

// RestoreCarriedAttemptCount gives a freshly carried intent the attempt count
// it had already spent in the store it was carried from, so a cutover can
// never hand an intent a fresh retry budget. It applies only to a just
// inserted, never leased row (state queued, attempt_count 0, no lease or
// transport marker); attempts must lie in (0, DefaultMaxTransportAttempts),
// because the carry refuses intents at or over the budget.
func (r *OutboxRepository) RestoreCarriedAttemptCount(
	ctx context.Context,
	outboxID string,
	attempts int64,
) error {
	if attempts <= 0 || attempts >= DefaultMaxTransportAttempts {
		return fmt.Errorf(
			"restore carried attempt count of outbox item %q: %d is outside (0, %d)",
			outboxID,
			attempts,
			DefaultMaxTransportAttempts,
		)
	}
	nowMS, err := r.nowMS("restore carried attempt count")
	if err != nil {
		return err
	}
	result, err := r.store.db.ExecContext(ctx, `
		UPDATE outbox
		SET attempt_count = ?,
			updated_at_ms = MAX(updated_at_ms, ?)
		WHERE outbox_id = ?
		  AND state = 'queued'
		  AND attempt_count = 0
		  AND lease_token IS NULL
		  AND transport_called_at_ms IS NULL
	`, attempts, nowMS, outboxID)
	if err != nil {
		return fmt.Errorf("restore carried attempt count of outbox item %q: %w", outboxID, err)
	}
	return r.requireStateMutation(ctx, "restore carried attempt count of", outboxID, result)
}
