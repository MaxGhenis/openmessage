package sqlite

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// OutboxKind identifies the shape of an outbound intent.
type OutboxKind string

const (
	OutboxKindText     OutboxKind = "text"
	OutboxKindMedia    OutboxKind = "media"
	OutboxKindReaction OutboxKind = "reaction"
	OutboxKindRead     OutboxKind = "read"
)

// OutboxState is the persisted delivery state of an outbound intent.
type OutboxState string

const (
	OutboxQueued        OutboxState = "queued"
	OutboxDispatching   OutboxState = "dispatching"
	OutboxNotDispatched OutboxState = "not_dispatched"
	OutboxUncertain     OutboxState = "uncertain"
	OutboxConfirmed     OutboxState = "confirmed"
	OutboxStoreFailed   OutboxState = "store_failed"
	OutboxRejected      OutboxState = "rejected"
	OutboxCanceled      OutboxState = "canceled"
)

// EnqueueDisposition describes whether Enqueue inserted a new intent or
// returned the row already associated with its idempotency key.
type EnqueueDisposition string

const (
	EnqueueInserted EnqueueDisposition = "inserted"
	EnqueueExisting EnqueueDisposition = "existing"

	// Longer aliases keep call sites self-documenting when the shorter names
	// would be ambiguous next to other repository results.
	EnqueueDispositionInserted = EnqueueInserted
	EnqueueDispositionExisting = EnqueueExisting
)

// NewOutboxItem contains the caller-owned identity and intent fields for a new
// durable outbound operation. A zero ScheduledFor means immediately eligible.
type NewOutboxItem struct {
	OutboxID           string
	AccountID          string
	ConversationID     string
	Kind               OutboxKind
	IdempotencyKey     string
	PayloadHash        string
	Operation          string
	LocalMessageID     string
	TransportRequestID string
	ScheduledFor       time.Time
	ScheduledForMS     int64
}

// OutboxItem mirrors one row in outbox. Nullable database fields are pointers.
type OutboxItem struct {
	OutboxID            string
	AccountID           string
	ConversationID      string
	Kind                OutboxKind
	IdempotencyKey      string
	PayloadHash         string
	Operation           string
	State               OutboxState
	LocalMessageID      *string
	TransportRequestID  string
	ResultRemoteID      *string
	ErrorClass          *string
	ErrorCode           *string
	ErrorDetail         *string
	AttemptCount        int64
	LeaseOwner          *string
	LeaseToken          *string
	LeaseExpiresAtMS    *int64
	TransportCalledAtMS *int64
	ScheduledForMS      int64
	NextAttemptAtMS     *int64
	CreatedAtMS         int64
	UpdatedAtMS         int64
}

// LeaseRequest controls one atomic due-row claim.
type LeaseRequest struct {
	Owner    string
	Now      time.Time
	Duration time.Duration
	Limit    int
}

// Lease is an outbox row with an active dispatch lease. OutboxItem is embedded
// so callers can use the persisted fields directly.
type Lease struct {
	OutboxItem
}

// Attempt identifies the active lease crossing the transport-call boundary.
// AttemptToken and ConnectionGeneration are reserved for the dispatcher layer;
// this repository persists only the lease phase and stable request identity.
type Attempt struct {
	OutboxID             string
	LeaseToken           string
	AttemptToken         string
	ConnectionGeneration uint64
	StartedAt            time.Time
}

// Confirmation records a known remote result for the active lease.
type Confirmation struct {
	OutboxID          string
	LeaseToken        string
	ResultRemoteID    string
	TransportResultID string
}

// RecoveredLeases reports how expired dispatch leases were classified.
type RecoveredLeases struct {
	NotDispatched int
	Uncertain     int
}

// OutboxRepository owns durable outbound intent, leasing, and phase changes.
type OutboxRepository struct {
	store *Store
	now   func() time.Time
}

// NewOutboxRepository creates an outbox repository. The clock is required so
// all storage-owned timestamps are deterministic in tests.
func NewOutboxRepository(
	store *Store,
	now func() time.Time,
) (*OutboxRepository, error) {
	if store == nil || store.db == nil {
		return nil, fmt.Errorf("create outbox repository: store is nil")
	}
	if now == nil {
		return nil, fmt.Errorf("create outbox repository: now function is nil")
	}
	return &OutboxRepository{store: store, now: now}, nil
}

// Enqueue inserts a queued outbound intent. Reusing an account-scoped
// idempotency key returns the original row only when operation, conversation,
// and payload hash are identical.
func (r *OutboxRepository) Enqueue(
	ctx context.Context,
	item NewOutboxItem,
) (OutboxItem, EnqueueDisposition, error) {
	return r.enqueue(ctx, item, nil)
}

// EnqueueOutgoingMessage atomically inserts an outbound intent and its
// optimistic outgoing message. When the idempotency key already names the same
// intent, it returns the existing outbox row without validating or writing the
// candidate message.
func (r *OutboxRepository) EnqueueOutgoingMessage(
	ctx context.Context,
	item NewOutboxItem,
	message Message,
) (OutboxItem, EnqueueDisposition, error) {
	return r.enqueue(ctx, item, &message)
}

func (r *OutboxRepository) enqueue(
	ctx context.Context,
	item NewOutboxItem,
	message *Message,
) (OutboxItem, EnqueueDisposition, error) {
	if err := validateNewOutboxItem(item); err != nil {
		return OutboxItem{}, "", fmt.Errorf("enqueue outbox item: %w", err)
	}
	nowMS, err := r.nowMS("enqueue outbox item")
	if err != nil {
		return OutboxItem{}, "", err
	}
	scheduledForMS, err := scheduledForMilliseconds(item, nowMS)
	if err != nil {
		return OutboxItem{}, "", fmt.Errorf("enqueue outbox item %q: %w", item.OutboxID, err)
	}

	tx, err := r.store.db.BeginTx(ctx, nil)
	if err != nil {
		return OutboxItem{}, "", fmt.Errorf("enqueue outbox item %q: begin transaction: %w", item.OutboxID, err)
	}
	defer tx.Rollback()

	result, err := tx.ExecContext(ctx, `
		INSERT INTO outbox (
			outbox_id,
			account_id,
			conversation_id,
			kind,
			idempotency_key,
			payload_hash,
			operation,
			state,
			local_message_id,
			transport_request_id,
			attempt_count,
			scheduled_for_ms,
			created_at_ms,
			updated_at_ms
		) VALUES (?, ?, ?, ?, ?, ?, ?, 'queued', ?, ?, 0, ?, ?, ?)
		ON CONFLICT(account_id, idempotency_key) DO NOTHING
	`,
		item.OutboxID,
		item.AccountID,
		item.ConversationID,
		item.Kind,
		item.IdempotencyKey,
		item.PayloadHash,
		item.Operation,
		nullableOutboxText(item.LocalMessageID),
		item.TransportRequestID,
		scheduledForMS,
		nowMS,
		nowMS,
	)
	if err != nil {
		return OutboxItem{}, "", fmt.Errorf(
			"enqueue outbox item %q: %w",
			item.OutboxID,
			mapConstraintError(err),
		)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return OutboxItem{}, "", fmt.Errorf("enqueue outbox item %q: read rows affected: %w", item.OutboxID, err)
	}

	disposition := EnqueueInserted
	var row OutboxItem
	switch affected {
	case 1:
		row, err = scanOutboxItem(tx.QueryRowContext(ctx, `
			SELECT `+outboxColumns+`
			FROM outbox
			WHERE outbox_id = ?
		`, item.OutboxID))
	case 0:
		disposition = EnqueueExisting
		row, err = scanOutboxItem(tx.QueryRowContext(ctx, `
			SELECT `+outboxColumns+`
			FROM outbox
			WHERE account_id = ? AND idempotency_key = ?
		`, item.AccountID, item.IdempotencyKey))
		if err == nil && (row.Operation != item.Operation ||
			row.ConversationID != item.ConversationID ||
			row.PayloadHash != item.PayloadHash) {
			return OutboxItem{}, "", fmt.Errorf(
				"enqueue outbox item %q for account %q and idempotency key %q: %w",
				item.OutboxID,
				item.AccountID,
				item.IdempotencyKey,
				ErrIdempotencyConflict,
			)
		}
	default:
		return OutboxItem{}, "", fmt.Errorf(
			"enqueue outbox item %q: affected %d rows, want 0 or 1",
			item.OutboxID,
			affected,
		)
	}
	if err != nil {
		return OutboxItem{}, "", fmt.Errorf("enqueue outbox item %q: read effective row: %w", item.OutboxID, err)
	}
	if disposition == EnqueueExisting && message != nil {
		if err := validateExistingOutgoingPair(ctx, tx, row); err != nil {
			return OutboxItem{}, "", err
		}
	}
	if disposition == EnqueueInserted && message != nil {
		if err := r.insertOutgoingMessage(ctx, tx, item, *message, nowMS); err != nil {
			return OutboxItem{}, "", err
		}
	}
	if err := tx.Commit(); err != nil {
		return OutboxItem{}, "", fmt.Errorf("enqueue outbox item %q: commit: %w", item.OutboxID, err)
	}
	return row, disposition, nil
}

func (r *OutboxRepository) insertOutgoingMessage(
	ctx context.Context,
	tx *sql.Tx,
	item NewOutboxItem,
	message Message,
	nowMS int64,
) error {
	if message.Direction != MessageDirectionOutgoing {
		return fmt.Errorf(
			"enqueue outgoing message %q: %w: direction %q is not outgoing",
			message.MessageID,
			ErrInvalidMessage,
			message.Direction,
		)
	}
	if message.State != MessageStateActive {
		return fmt.Errorf(
			"enqueue outgoing message %q: %w: state %q is not active",
			message.MessageID,
			ErrInvalidMessage,
			message.State,
		)
	}
	if item.LocalMessageID == "" || message.MessageID != item.LocalMessageID {
		return fmt.Errorf(
			"enqueue outgoing message %q: %w: message ID does not match outbox local message ID %q",
			message.MessageID,
			ErrInvalidMessage,
			item.LocalMessageID,
		)
	}
	if message.AccountID != item.AccountID || message.ConversationID != item.ConversationID {
		return fmt.Errorf(
			"enqueue outgoing message %q: %w: message account/conversation does not match outbox intent",
			message.MessageID,
			ErrInvalidMessage,
		)
	}

	_, err := tx.ExecContext(ctx, `
		INSERT INTO messages (
			message_id,
			conversation_id,
			account_id,
			remote_message_id,
			sender_identity_id,
			direction,
			body,
			reply_to_remote_id,
			state,
			occurred_at_ms,
			created_at_ms,
			updated_at_ms
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		message.MessageID,
		message.ConversationID,
		message.AccountID,
		message.RemoteMessageID,
		message.SenderIdentityID,
		message.Direction,
		message.Body,
		message.ReplyToRemoteID,
		message.State,
		message.OccurredAtMS,
		nowMS,
		nowMS,
	)
	if err == nil {
		return nil
	}
	messageRepository := &MessageRepository{store: r.store, now: r.now}
	return messageRepository.mapMessageWriteError(ctx, tx, message, err)
}

func validateExistingOutgoingPair(
	ctx context.Context,
	tx *sql.Tx,
	item OutboxItem,
) error {
	if item.LocalMessageID == nil {
		return fmt.Errorf(
			"enqueue outgoing message for existing outbox item %q: %w: local message ID is missing",
			item.OutboxID,
			ErrInvalidMessage,
		)
	}
	message, err := scanMessage(tx.QueryRowContext(ctx, `
		SELECT `+messageColumns+`
		FROM messages
		WHERE message_id = ?
	`, *item.LocalMessageID))
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf(
			"enqueue outgoing message for existing outbox item %q: %w: message %q does not exist",
			item.OutboxID,
			ErrInvalidMessage,
			*item.LocalMessageID,
		)
	}
	if err != nil {
		return fmt.Errorf(
			"enqueue outgoing message for existing outbox item %q: read message %q: %w",
			item.OutboxID,
			*item.LocalMessageID,
			err,
		)
	}
	if message.AccountID != item.AccountID ||
		message.ConversationID != item.ConversationID ||
		message.Direction != MessageDirectionOutgoing ||
		message.State != MessageStateActive ||
		message.RemoteMessageID != item.TransportRequestID {
		return fmt.Errorf(
			"enqueue outgoing message for existing outbox item %q: %w: message %q is not its active request-bound outgoing pair",
			item.OutboxID,
			ErrInvalidMessage,
			message.MessageID,
		)
	}
	return nil
}

// LeaseDue atomically claims due queued, retryable, and explicitly eligible
// uncertain rows. Uncertain rows produced by recovery have no next-attempt time
// and therefore are never auto-leased.
func (r *OutboxRepository) LeaseDue(
	ctx context.Context,
	req LeaseRequest,
) ([]Lease, error) {
	if strings.TrimSpace(req.Owner) == "" {
		return nil, fmt.Errorf("lease due outbox items: owner is empty")
	}
	if req.Limit <= 0 {
		return nil, fmt.Errorf("lease due outbox items: limit must be positive")
	}
	if req.Duration <= 0 {
		return nil, fmt.Errorf("lease due outbox items: duration must be positive")
	}
	nowMS := req.Now.UnixMilli()
	if nowMS <= 0 {
		return nil, fmt.Errorf("lease due outbox items: current Unix time is not positive")
	}
	expiresAtMS := req.Now.Add(req.Duration).UnixMilli()
	if expiresAtMS <= nowMS {
		return nil, fmt.Errorf("lease due outbox items: lease expiry must be after now")
	}

	tx, err := r.store.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("lease due outbox items: begin transaction: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `
		SELECT outbox_id
		FROM outbox
		WHERE scheduled_for_ms <= ?
		  AND (
			state IN ('queued', 'not_dispatched')
			OR (state = 'uncertain' AND next_attempt_at_ms IS NOT NULL)
		  )
		  AND (next_attempt_at_ms IS NULL OR next_attempt_at_ms <= ?)
		  AND (lease_expires_at_ms IS NULL OR lease_expires_at_ms <= ?)
		ORDER BY COALESCE(next_attempt_at_ms, scheduled_for_ms), created_at_ms, outbox_id
		LIMIT ?
	`, nowMS, nowMS, nowMS, req.Limit)
	if err != nil {
		return nil, fmt.Errorf("lease due outbox items: select candidates: %w", err)
	}
	ids, err := collectRows(rows, func(row rowScanner) (string, error) {
		var id string
		err := row.Scan(&id)
		return id, err
	})
	if err != nil {
		return nil, fmt.Errorf("lease due outbox items: scan candidates: %w", err)
	}

	leases := make([]Lease, 0, len(ids))
	for _, id := range ids {
		token, err := newOutboxLeaseToken()
		if err != nil {
			return nil, fmt.Errorf("lease outbox item %q: generate token: %w", id, err)
		}
		result, err := tx.ExecContext(ctx, `
			UPDATE outbox
			SET state = 'dispatching',
				error_class = NULL,
				error_code = NULL,
				error_detail = NULL,
				lease_owner = ?,
				lease_token = ?,
				lease_expires_at_ms = ?,
				transport_called_at_ms = NULL,
				next_attempt_at_ms = NULL,
				updated_at_ms = ?
			WHERE outbox_id = ?
			  AND scheduled_for_ms <= ?
			  AND (
				state IN ('queued', 'not_dispatched')
				OR (state = 'uncertain' AND next_attempt_at_ms IS NOT NULL)
			  )
			  AND (next_attempt_at_ms IS NULL OR next_attempt_at_ms <= ?)
			  AND (lease_expires_at_ms IS NULL OR lease_expires_at_ms <= ?)
		`, req.Owner, token, expiresAtMS, nowMS, id, nowMS, nowMS, nowMS)
		if err != nil {
			return nil, fmt.Errorf("lease outbox item %q: update: %w", id, err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return nil, fmt.Errorf("lease outbox item %q: read rows affected: %w", id, err)
		}
		if affected != 1 {
			return nil, fmt.Errorf("lease outbox item %q: affected %d rows, want 1", id, affected)
		}
		item, err := scanOutboxItem(tx.QueryRowContext(ctx, `
			SELECT `+outboxColumns+`
			FROM outbox
			WHERE outbox_id = ?
		`, id))
		if err != nil {
			return nil, fmt.Errorf("lease outbox item %q: read claimed row: %w", id, err)
		}
		leases = append(leases, Lease{OutboxItem: item})
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("lease due outbox items: commit: %w", err)
	}
	return leases, nil
}

// MarkTransportCalled durably crosses the boundary after which an unknown
// outcome must not be retried automatically. A dispatcher commits this marker
// immediately before issuing the transport call, and the call consumes one
// attempt.
func (r *OutboxRepository) MarkTransportCalled(
	ctx context.Context,
	attempt Attempt,
) error {
	if strings.TrimSpace(attempt.OutboxID) == "" {
		return fmt.Errorf("mark transport called: outbox ID is empty")
	}
	if strings.TrimSpace(attempt.LeaseToken) == "" {
		return fmt.Errorf("mark transport called for outbox item %q: lease token is empty", attempt.OutboxID)
	}
	nowMS, err := r.nowMS("mark transport called")
	if err != nil {
		return err
	}
	result, err := r.store.db.ExecContext(ctx, `
		UPDATE outbox
		SET transport_called_at_ms = ?,
			attempt_count = attempt_count + 1,
			updated_at_ms = ?
		WHERE outbox_id = ?
		  AND state = 'dispatching'
		  AND lease_token = ?
		  AND lease_expires_at_ms > ?
		  AND transport_called_at_ms IS NULL
	`, nowMS, nowMS, attempt.OutboxID, attempt.LeaseToken, nowMS)
	if err != nil {
		return fmt.Errorf("mark transport called for outbox item %q: %w", attempt.OutboxID, err)
	}
	return r.requireLeaseMutation(ctx, "mark transport called", attempt.OutboxID, result)
}

// MarkNotDispatched records a pre-call failure and makes the row safe to retry
// at retryAt.
func (r *OutboxRepository) MarkNotDispatched(
	ctx context.Context,
	outboxID, leaseToken, class, code, detail string,
	retryAt time.Time,
) error {
	return r.markNotDispatched(ctx, outboxID, leaseToken, class, code, detail, retryAt, false)
}

// MarkCalledNotDispatched records a transport result that explicitly proves no
// remote dispatch occurred even though the conservative transport-call marker
// was already committed. Crashes after that marker still recover as uncertain.
func (r *OutboxRepository) MarkCalledNotDispatched(
	ctx context.Context,
	outboxID, leaseToken, class, code, detail string,
	retryAt time.Time,
) error {
	return r.markNotDispatched(ctx, outboxID, leaseToken, class, code, detail, retryAt, true)
}

func (r *OutboxRepository) markNotDispatched(
	ctx context.Context,
	outboxID, leaseToken, class, code, detail string,
	retryAt time.Time,
	called bool,
) error {
	retryAtMS := retryAt.UnixMilli()
	if retryAtMS <= 0 {
		return fmt.Errorf("mark outbox item %q not dispatched: retry time is not positive", outboxID)
	}
	nowMS, err := r.nowMS("mark outbox item not dispatched")
	if err != nil {
		return err
	}
	calledPredicate := "transport_called_at_ms IS NULL"
	operation := "mark not dispatched"
	if called {
		calledPredicate = "transport_called_at_ms IS NOT NULL"
		operation = "mark called not dispatched"
	}
	result, err := r.store.db.ExecContext(ctx, `
		UPDATE outbox
		SET state = 'not_dispatched',
			error_class = ?,
			error_code = ?,
			error_detail = ?,
			next_attempt_at_ms = ?,
			lease_owner = NULL,
			lease_token = NULL,
			lease_expires_at_ms = NULL,
			transport_called_at_ms = NULL,
			updated_at_ms = ?
		WHERE outbox_id = ?
		  AND state = 'dispatching'
		  AND lease_token = ?
		  AND `+calledPredicate+`
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
	return r.requireLeaseMutation(ctx, operation, outboxID, result)
}

// ReleaseUnavailable returns an uncalled dispatch lease to queued state
// without consuming a transport attempt or scheduling a retry time.
func (r *OutboxRepository) ReleaseUnavailable(
	ctx context.Context,
	outboxID, leaseToken string,
) error {
	nowMS, err := r.nowMS("release unavailable outbox item")
	if err != nil {
		return err
	}
	result, err := r.store.db.ExecContext(ctx, `
		UPDATE outbox
		SET state = 'queued',
			next_attempt_at_ms = NULL,
			lease_owner = NULL,
			lease_token = NULL,
			lease_expires_at_ms = NULL,
			transport_called_at_ms = NULL,
			updated_at_ms = ?
		WHERE outbox_id = ?
		  AND state = 'dispatching'
		  AND lease_token = ?
		  AND transport_called_at_ms IS NULL
	`, nowMS, outboxID, leaseToken)
	if err != nil {
		return fmt.Errorf("release unavailable outbox item %q: %w", outboxID, err)
	}
	return r.requireLeaseMutation(ctx, "release unavailable", outboxID, result)
}

// RetryNotDispatched makes one explicitly safe-to-retry row immediately due
// while preserving its transport request identity.
func (r *OutboxRepository) RetryNotDispatched(
	ctx context.Context,
	outboxID string,
) error {
	nowMS, err := r.nowMS("retry not-dispatched outbox item")
	if err != nil {
		return err
	}
	result, err := r.store.db.ExecContext(ctx, `
		UPDATE outbox
		SET next_attempt_at_ms = ?,
			updated_at_ms = ?
		WHERE outbox_id = ?
		  AND state = 'not_dispatched'
	`, nowMS, nowMS, outboxID)
	if err != nil {
		return fmt.Errorf("retry not-dispatched outbox item %q: %w", outboxID, err)
	}
	return r.requireStateMutation(ctx, "retry not dispatched", outboxID, result)
}

// Cancel transitions pending work to a terminal canceled state. Active or
// already-terminal rows are rejected so a transport call cannot race a cancel.
func (r *OutboxRepository) Cancel(ctx context.Context, outboxID string) error {
	nowMS, err := r.nowMS("cancel outbox item")
	if err != nil {
		return err
	}
	result, err := r.store.db.ExecContext(ctx, `
		UPDATE outbox
		SET state = 'canceled',
			next_attempt_at_ms = NULL,
			updated_at_ms = ?
		WHERE outbox_id = ?
		  AND state IN ('queued', 'not_dispatched')
	`, nowMS, outboxID)
	if err != nil {
		return fmt.Errorf("cancel outbox item %q: %w", outboxID, err)
	}
	return r.requireStateMutation(ctx, "cancel", outboxID, result)
}

// MarkUncertain records an unknown outcome after the transport call boundary.
// The resulting row has no next-attempt time and is not auto-retryable.
func (r *OutboxRepository) MarkUncertain(
	ctx context.Context,
	outboxID, leaseToken, class, code, detail string,
) error {
	nowMS, err := r.nowMS("mark outbox item uncertain")
	if err != nil {
		return err
	}
	result, err := r.store.db.ExecContext(ctx, `
		UPDATE outbox
		SET state = 'uncertain',
			error_class = ?,
			error_code = ?,
			error_detail = ?,
			next_attempt_at_ms = NULL,
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
		nowMS,
		outboxID,
		leaseToken,
	)
	if err != nil {
		return fmt.Errorf("mark outbox item %q uncertain: %w", outboxID, err)
	}
	return r.requireLeaseMutation(ctx, "mark uncertain", outboxID, result)
}

// Reject records a terminal rejection. A rejection may occur on either side of
// the call boundary, so only active lease ownership is required.
func (r *OutboxRepository) Reject(
	ctx context.Context,
	outboxID, leaseToken, class, code, detail string,
) error {
	nowMS, err := r.nowMS("reject outbox item")
	if err != nil {
		return err
	}
	result, err := r.store.db.ExecContext(ctx, `
		UPDATE outbox
		SET state = 'rejected',
			error_class = ?,
			error_code = ?,
			error_detail = ?,
			next_attempt_at_ms = NULL,
			lease_owner = NULL,
			lease_token = NULL,
			lease_expires_at_ms = NULL,
			transport_called_at_ms = NULL,
			updated_at_ms = ?
		WHERE outbox_id = ?
		  AND state = 'dispatching'
		  AND lease_token = ?
	`,
		nullableOutboxText(class),
		nullableOutboxText(code),
		nullableOutboxText(detail),
		nowMS,
		outboxID,
		leaseToken,
	)
	if err != nil {
		return fmt.Errorf("reject outbox item %q: %w", outboxID, err)
	}
	return r.requireLeaseMutation(ctx, "reject", outboxID, result)
}

// Confirm atomically records a known remote result for the active called
// transport lease.
func (r *OutboxRepository) Confirm(
	ctx context.Context,
	confirmation Confirmation,
) error {
	resultRemoteID := confirmation.ResultRemoteID
	if resultRemoteID == "" {
		resultRemoteID = confirmation.TransportResultID
	}
	if strings.TrimSpace(resultRemoteID) == "" {
		return fmt.Errorf("confirm outbox item %q: remote result ID is empty", confirmation.OutboxID)
	}
	nowMS, err := r.nowMS("confirm outbox item")
	if err != nil {
		return err
	}
	tx, err := r.store.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("confirm outbox item %q: begin transaction: %w", confirmation.OutboxID, err)
	}
	defer tx.Rollback()

	result, err := tx.ExecContext(ctx, `
		UPDATE outbox
		SET state = 'confirmed',
			result_remote_id = ?,
			error_class = NULL,
			error_code = NULL,
			error_detail = NULL,
			next_attempt_at_ms = NULL,
			lease_owner = NULL,
			lease_token = NULL,
			lease_expires_at_ms = NULL,
			transport_called_at_ms = NULL,
			updated_at_ms = ?
		WHERE outbox_id = ?
		  AND state = 'dispatching'
		  AND lease_token = ?
		  AND transport_called_at_ms IS NOT NULL
	`, resultRemoteID, nowMS, confirmation.OutboxID, confirmation.LeaseToken)
	if err != nil {
		return fmt.Errorf("confirm outbox item %q: update: %w", confirmation.OutboxID, err)
	}
	if err := r.requireLeaseMutationWithQueryer(ctx, tx, "confirm", confirmation.OutboxID, result); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("confirm outbox item %q: commit: %w", confirmation.OutboxID, err)
	}
	return nil
}

// MarkStoreFailed records that the remote operation was accepted but its local
// result could not be committed by the projection layer.
func (r *OutboxRepository) MarkStoreFailed(
	ctx context.Context,
	outboxID, leaseToken, transportResultID, detail string,
) error {
	if strings.TrimSpace(transportResultID) == "" {
		return fmt.Errorf("mark outbox item %q store failed: transport result ID is empty", outboxID)
	}
	nowMS, err := r.nowMS("mark outbox item store failed")
	if err != nil {
		return err
	}
	result, err := r.store.db.ExecContext(ctx, `
		UPDATE outbox
		SET state = 'store_failed',
			result_remote_id = ?,
			error_detail = ?,
			next_attempt_at_ms = NULL,
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
		transportResultID,
		nullableOutboxText(detail),
		nowMS,
		outboxID,
		leaseToken,
	)
	if err != nil {
		return fmt.Errorf("mark outbox item %q store failed: %w", outboxID, err)
	}
	return r.requireLeaseMutation(ctx, "mark store failed", outboxID, result)
}

// RecoverExpiredLeases makes expired pre-call leases immediately retryable and
// makes expired post-call leases uncertain. Both classifications commit in one
// transaction.
func (r *OutboxRepository) RecoverExpiredLeases(
	ctx context.Context,
	now time.Time,
) (RecoveredLeases, error) {
	nowMS := now.UnixMilli()
	if nowMS <= 0 {
		return RecoveredLeases{}, fmt.Errorf("recover expired outbox leases: current Unix time is not positive")
	}
	tx, err := r.store.db.BeginTx(ctx, nil)
	if err != nil {
		return RecoveredLeases{}, fmt.Errorf("recover expired outbox leases: begin transaction: %w", err)
	}
	defer tx.Rollback()

	preCall, err := tx.ExecContext(ctx, `
		UPDATE outbox
		SET state = 'not_dispatched',
			next_attempt_at_ms = ?,
			lease_owner = NULL,
			lease_token = NULL,
			lease_expires_at_ms = NULL,
			transport_called_at_ms = NULL,
			updated_at_ms = ?
		WHERE state = 'dispatching'
		  AND lease_expires_at_ms <= ?
		  AND transport_called_at_ms IS NULL
	`, nowMS, nowMS, nowMS)
	if err != nil {
		return RecoveredLeases{}, fmt.Errorf("recover expired pre-call outbox leases: %w", err)
	}
	postCall, err := tx.ExecContext(ctx, `
		UPDATE outbox
		SET state = 'uncertain',
			next_attempt_at_ms = NULL,
			lease_owner = NULL,
			lease_token = NULL,
			lease_expires_at_ms = NULL,
			transport_called_at_ms = NULL,
			updated_at_ms = ?
		WHERE state = 'dispatching'
		  AND lease_expires_at_ms <= ?
		  AND transport_called_at_ms IS NOT NULL
	`, nowMS, nowMS)
	if err != nil {
		return RecoveredLeases{}, fmt.Errorf("recover expired post-call outbox leases: %w", err)
	}
	preCallCount, err := preCall.RowsAffected()
	if err != nil {
		return RecoveredLeases{}, fmt.Errorf("recover expired pre-call outbox leases: read rows affected: %w", err)
	}
	postCallCount, err := postCall.RowsAffected()
	if err != nil {
		return RecoveredLeases{}, fmt.Errorf("recover expired post-call outbox leases: read rows affected: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return RecoveredLeases{}, fmt.Errorf("recover expired outbox leases: commit: %w", err)
	}
	return RecoveredLeases{
		NotDispatched: int(preCallCount),
		Uncertain:     int(postCallCount),
	}, nil
}

// FindByID returns an outbox row by its globally unique ID.
func (r *OutboxRepository) FindByID(
	ctx context.Context,
	outboxID string,
) (OutboxItem, error) {
	item, err := scanOutboxItem(r.store.db.QueryRowContext(ctx, `
		SELECT `+outboxColumns+`
		FROM outbox
		WHERE outbox_id = ?
	`, outboxID))
	if errors.Is(err, sql.ErrNoRows) {
		return OutboxItem{}, notFound("outbox item", outboxID)
	}
	if err != nil {
		return OutboxItem{}, fmt.Errorf("find outbox item %q: %w", outboxID, err)
	}
	return item, nil
}

// FindByTransportRequestID returns the account-scoped row carrying the stable
// request ID supplied to the transport.
func (r *OutboxRepository) FindByTransportRequestID(
	ctx context.Context,
	accountID, requestID string,
) (OutboxItem, error) {
	item, err := scanOutboxItem(r.store.db.QueryRowContext(ctx, `
		SELECT `+outboxColumns+`
		FROM outbox
		WHERE account_id = ? AND transport_request_id = ?
	`, accountID, requestID))
	if errors.Is(err, sql.ErrNoRows) {
		return OutboxItem{}, notFound("outbox transport request", accountID+"/"+requestID)
	}
	if err != nil {
		return OutboxItem{}, fmt.Errorf(
			"find outbox transport request %q for account %q: %w",
			requestID,
			accountID,
			err,
		)
	}
	return item, nil
}

const outboxColumns = `
	outbox_id,
	account_id,
	conversation_id,
	kind,
	idempotency_key,
	payload_hash,
	operation,
	state,
	local_message_id,
	transport_request_id,
	result_remote_id,
	error_class,
	error_code,
	error_detail,
	attempt_count,
	lease_owner,
	lease_token,
	lease_expires_at_ms,
	transport_called_at_ms,
	scheduled_for_ms,
	next_attempt_at_ms,
	created_at_ms,
	updated_at_ms`

func scanOutboxItem(row rowScanner) (OutboxItem, error) {
	var item OutboxItem
	err := row.Scan(
		&item.OutboxID,
		&item.AccountID,
		&item.ConversationID,
		&item.Kind,
		&item.IdempotencyKey,
		&item.PayloadHash,
		&item.Operation,
		&item.State,
		&item.LocalMessageID,
		&item.TransportRequestID,
		&item.ResultRemoteID,
		&item.ErrorClass,
		&item.ErrorCode,
		&item.ErrorDetail,
		&item.AttemptCount,
		&item.LeaseOwner,
		&item.LeaseToken,
		&item.LeaseExpiresAtMS,
		&item.TransportCalledAtMS,
		&item.ScheduledForMS,
		&item.NextAttemptAtMS,
		&item.CreatedAtMS,
		&item.UpdatedAtMS,
	)
	return item, err
}

func validateNewOutboxItem(item NewOutboxItem) error {
	checks := []struct {
		name  string
		value string
	}{
		{name: "outbox ID", value: item.OutboxID},
		{name: "account ID", value: item.AccountID},
		{name: "conversation ID", value: item.ConversationID},
		{name: "idempotency key", value: item.IdempotencyKey},
		{name: "payload hash", value: item.PayloadHash},
		{name: "operation", value: item.Operation},
		{name: "transport request ID", value: item.TransportRequestID},
	}
	for _, check := range checks {
		if strings.TrimSpace(check.value) == "" {
			return fmt.Errorf("%s is empty", check.name)
		}
	}
	switch item.Kind {
	case OutboxKindText, OutboxKindMedia, OutboxKindReaction, OutboxKindRead:
	default:
		return fmt.Errorf("kind %q is invalid", item.Kind)
	}
	if item.ScheduledForMS < 0 {
		return fmt.Errorf("scheduled time is negative")
	}
	if !item.ScheduledFor.IsZero() && item.ScheduledForMS != 0 {
		return fmt.Errorf("ScheduledFor and ScheduledForMS are both set")
	}
	return nil
}

func scheduledForMilliseconds(item NewOutboxItem, nowMS int64) (int64, error) {
	scheduledForMS := item.ScheduledForMS
	if !item.ScheduledFor.IsZero() {
		scheduledForMS = item.ScheduledFor.UnixMilli()
	}
	if scheduledForMS == 0 {
		scheduledForMS = nowMS
	}
	if scheduledForMS <= 0 {
		return 0, fmt.Errorf("scheduled Unix time is not positive")
	}
	return scheduledForMS, nil
}

func nullableOutboxText(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func newOutboxLeaseToken() (string, error) {
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(token[:]), nil
}

func (r *OutboxRepository) nowMS(operation string) (int64, error) {
	nowMS := r.now().UnixMilli()
	if nowMS <= 0 {
		return 0, fmt.Errorf("%s: current Unix time is not positive", operation)
	}
	return nowMS, nil
}

func (r *OutboxRepository) requireLeaseMutation(
	ctx context.Context,
	operation, outboxID string,
	result sql.Result,
) error {
	return r.requireLeaseMutationWithQueryer(ctx, r.store.db, operation, outboxID, result)
}

func (r *OutboxRepository) requireStateMutation(
	ctx context.Context,
	operation, outboxID string,
	result sql.Result,
) error {
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s outbox item %q: read rows affected: %w", operation, outboxID, err)
	}
	if affected == 1 {
		return nil
	}
	if affected != 0 {
		return fmt.Errorf("%s outbox item %q: affected %d rows, want 1", operation, outboxID, affected)
	}
	var exists bool
	if err := r.store.db.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM outbox WHERE outbox_id = ?)
	`, outboxID).Scan(&exists); err != nil {
		return fmt.Errorf("%s outbox item %q: inspect state: %w", operation, outboxID, err)
	}
	if !exists {
		return notFound("outbox item", outboxID)
	}
	return fmt.Errorf("%s outbox item %q: %w", operation, outboxID, ErrInvalidOutboxState)
}

type outboxQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func (r *OutboxRepository) requireLeaseMutationWithQueryer(
	ctx context.Context,
	queryer outboxQueryer,
	operation, outboxID string,
	result sql.Result,
) error {
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s outbox item %q: read rows affected: %w", operation, outboxID, err)
	}
	if affected == 1 {
		return nil
	}
	if affected != 0 {
		return fmt.Errorf("%s outbox item %q: affected %d rows, want 1", operation, outboxID, affected)
	}
	var exists bool
	if err := queryer.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM outbox WHERE outbox_id = ?)
	`, outboxID).Scan(&exists); err != nil {
		return fmt.Errorf("%s outbox item %q: inspect lost lease: %w", operation, outboxID, err)
	}
	if !exists {
		return notFound("outbox item", outboxID)
	}
	return fmt.Errorf("%s outbox item %q: %w", operation, outboxID, ErrLeaseLost)
}
