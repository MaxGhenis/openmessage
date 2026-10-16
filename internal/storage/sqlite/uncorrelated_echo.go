package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Google Messages normally echoes a sent message with the TmpID it was sent
// with, and the ingest worker correlates that TmpID to the outbox row's
// transport request ID. On the live install the phone echoes every sent
// message with an empty TmpID (observed in raw inbox frames on 2026-10-16, on
// both the QR and the Google-account pairing), so nothing correlates the echo
// and it projects as a second copy of the sent message beside the local row.
//
// The functions here correlate such an echo by content instead: the oldest
// sent-but-unbound text intent in the same conversation with the identical
// body, created inside a short window around the echo's timestamp. Once
// correlated, the echo goes through the same reconcile as a TmpID echo, so
// the local row takes the real remote ID and any echo row already projected
// is merged into it (repointLocalMessage).

const (
	// uncorrelatedEchoMaxLagMS is how long after the send an echo may be
	// stamped. The echo carries the phone's timestamp for the message.
	uncorrelatedEchoMaxLagMS = int64(30 * 60 * 1000)
	// uncorrelatedEchoMaxLeadMS allows the phone's clock to run ahead of or
	// behind this machine's, so an echo may be stamped before the send.
	uncorrelatedEchoMaxLeadMS = int64(2 * 60 * 1000)
)

// UncorrelatedEchoRequest is an outgoing message frame that names no
// transport request.
type UncorrelatedEchoRequest struct {
	AccountID            string
	RemoteConversationID string
	RemoteMessageID      string
	Body                 string
	OccurredAtMS         int64
}

// unboundTextIntentPredicate selects text intents the transport accepted (or
// may have accepted) whose local row still carries the transport request ID
// because no echo has bound it to the real remote ID. It expects the outbox
// as o and the intent's local message as m.
const unboundTextIntentPredicate = `
	o.kind = 'text'
	AND o.state IN ('confirmed', 'uncertain', 'store_failed')
	AND (o.result_remote_id IS NULL OR o.result_remote_id = o.transport_request_id)
	AND m.remote_message_id = o.transport_request_id
	AND m.direction = 'outgoing'
`

const uncorrelatedEchoCandidateQuery = `
	SELECT o.transport_request_id
	FROM outbox AS o
	JOIN conversations AS c
	  ON c.account_id = o.account_id AND c.conversation_id = o.conversation_id
	JOIN messages AS m ON m.message_id = o.local_message_id
	WHERE o.account_id = ?
	  AND c.remote_conversation_id = ?
	  AND m.body = ?
	  AND o.created_at_ms BETWEEN ? AND ?
	  AND ` + unboundTextIntentPredicate + `
	ORDER BY o.created_at_ms, o.outbox_id
	LIMIT 1
`

// ReconcileUncorrelatedEcho correlates an echo that names no transport
// request to the oldest matching unbound text intent and reconciles it
// exactly as ReconcileConfirm would with that intent's request ID. An echo
// whose remote ID already belongs to an intent is a Noop; one with no
// matching intent (a message sent from the phone itself) is NotFound.
func (r *OutboxRepository) ReconcileUncorrelatedEcho(
	ctx context.Context,
	req UncorrelatedEchoRequest,
) (ReconcileOutcome, error) {
	checks := []struct {
		name  string
		value string
	}{
		{name: "account ID", value: req.AccountID},
		{name: "remote conversation ID", value: req.RemoteConversationID},
		{name: "remote message ID", value: req.RemoteMessageID},
		{name: "body", value: req.Body},
	}
	for _, check := range checks {
		if strings.TrimSpace(check.value) == "" {
			return "", fmt.Errorf("reconcile uncorrelated echo: %s is empty", check.name)
		}
	}
	if req.OccurredAtMS <= 0 {
		return "", fmt.Errorf("reconcile uncorrelated echo: occurrence time is not positive")
	}

	tx, err := r.store.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("reconcile uncorrelated echo: begin transaction: %w", err)
	}
	defer tx.Rollback()

	var bound int
	err = tx.QueryRowContext(ctx, `
		SELECT 1
		FROM outbox
		WHERE account_id = ? AND result_remote_id = ?
		LIMIT 1
	`, req.AccountID, req.RemoteMessageID).Scan(&bound)
	if err == nil {
		return ReconcileOutcomeNoop, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("reconcile uncorrelated echo: read existing correlation: %w", err)
	}

	var transportRequestID string
	err = tx.QueryRowContext(ctx, uncorrelatedEchoCandidateQuery,
		req.AccountID,
		req.RemoteConversationID,
		req.Body,
		req.OccurredAtMS-uncorrelatedEchoMaxLagMS,
		req.OccurredAtMS+uncorrelatedEchoMaxLeadMS,
	).Scan(&transportRequestID)
	if errors.Is(err, sql.ErrNoRows) {
		return ReconcileOutcomeNotFound, nil
	}
	if err != nil {
		return "", fmt.Errorf("reconcile uncorrelated echo: find matching intent: %w", err)
	}

	outcome, err := r.reconcileConfirmTx(ctx, tx, ReconcileRequest{
		AccountID:          req.AccountID,
		TransportRequestID: transportRequestID,
		ResultRemoteID:     req.RemoteMessageID,
	})
	if err != nil {
		return "", err
	}
	if err := commitReconcile(tx, outcome, transportRequestID); err != nil {
		return "", err
	}
	return outcome, nil
}

// EchoDuplicateRepairReport counts what RepairUncorrelatedEchoDuplicates saw.
type EchoDuplicateRepairReport struct {
	// Unbound is the number of sent text intents still on their request ID.
	Unbound int
	// Merged is how many of them found their echo row and absorbed it.
	Merged int
}

type unboundIntent struct {
	accountID          string
	transportRequestID string
	createdAtMS        int64
	localMessageID     string
	conversationID     string
	body               string
}

// RepairUncorrelatedEchoDuplicates finds Google text intents whose echo was
// projected as a separate row because it carried no TmpID, and reconciles
// each with that row's remote ID. The reconcile merges the echo row into the
// local row, so the duplicate disappears and the local row keeps its ID,
// reactions and read state. An intent's echo is the earliest outgoing row in
// the local row's conversation with the identical body, stamped inside the
// same window ReconcileUncorrelatedEcho uses, and not itself an intent's row
// or an intent's result. Intents are repaired oldest first, so repeated
// identical texts pair up in order. It is idempotent: a repaired intent is
// bound and no longer selected.
func (r *OutboxRepository) RepairUncorrelatedEchoDuplicates(ctx context.Context) (EchoDuplicateRepairReport, error) {
	var report EchoDuplicateRepairReport
	rows, err := r.store.db.QueryContext(ctx, `
		SELECT o.account_id, o.transport_request_id, o.created_at_ms,
		       m.message_id, m.conversation_id, m.body
		FROM outbox AS o
		JOIN accounts AS a ON a.account_id = o.account_id
		JOIN messages AS m ON m.message_id = o.local_message_id
		WHERE a.bridge_key = 'google_messages'
		  AND m.body <> ''
		  AND `+unboundTextIntentPredicate+`
		ORDER BY o.created_at_ms, o.outbox_id
	`)
	if err != nil {
		return report, fmt.Errorf("repair echo duplicates: list unbound intents: %w", err)
	}
	var intents []unboundIntent
	for rows.Next() {
		var intent unboundIntent
		if err := rows.Scan(
			&intent.accountID,
			&intent.transportRequestID,
			&intent.createdAtMS,
			&intent.localMessageID,
			&intent.conversationID,
			&intent.body,
		); err != nil {
			rows.Close()
			return report, fmt.Errorf("repair echo duplicates: scan unbound intent: %w", err)
		}
		intents = append(intents, intent)
	}
	if err := rows.Close(); err != nil {
		return report, fmt.Errorf("repair echo duplicates: close unbound intents: %w", err)
	}
	if err := rows.Err(); err != nil {
		return report, fmt.Errorf("repair echo duplicates: list unbound intents: %w", err)
	}
	report.Unbound = len(intents)

	for _, intent := range intents {
		merged, err := r.repairOneEchoDuplicate(ctx, intent)
		if err != nil {
			return report, err
		}
		if merged {
			report.Merged++
		}
	}
	return report, nil
}

func (r *OutboxRepository) repairOneEchoDuplicate(ctx context.Context, intent unboundIntent) (bool, error) {
	tx, err := r.store.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("repair echo duplicate %q: begin transaction: %w", intent.transportRequestID, err)
	}
	defer tx.Rollback()

	var echoRemoteID string
	err = tx.QueryRowContext(ctx, `
		SELECT e.remote_message_id
		FROM messages AS e
		WHERE e.account_id = ?
		  AND e.conversation_id = ?
		  AND e.message_id <> ?
		  AND e.direction = 'outgoing'
		  AND e.body = ?
		  AND e.occurred_at_ms BETWEEN ? AND ?
		  AND NOT EXISTS (
		      SELECT 1
		      FROM outbox AS o
		      WHERE o.account_id = e.account_id
		        AND (o.transport_request_id = e.remote_message_id
		             OR o.result_remote_id = e.remote_message_id)
		  )
		ORDER BY e.occurred_at_ms, e.message_id
		LIMIT 1
	`,
		intent.accountID,
		intent.conversationID,
		intent.localMessageID,
		intent.body,
		intent.createdAtMS-uncorrelatedEchoMaxLeadMS,
		intent.createdAtMS+uncorrelatedEchoMaxLagMS,
	).Scan(&echoRemoteID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("repair echo duplicate %q: find echo row: %w", intent.transportRequestID, err)
	}

	outcome, err := r.reconcileConfirmTx(ctx, tx, ReconcileRequest{
		AccountID:          intent.accountID,
		TransportRequestID: intent.transportRequestID,
		ResultRemoteID:     echoRemoteID,
	})
	if err != nil {
		return false, fmt.Errorf("repair echo duplicate %q: %w", intent.transportRequestID, err)
	}
	if err := commitReconcile(tx, outcome, intent.transportRequestID); err != nil {
		return false, err
	}
	return outcome == ReconcileOutcomeReconciled || outcome == ReconcileOutcomeEnriched, nil
}
