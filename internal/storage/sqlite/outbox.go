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
	OutboxID            string
	AccountID           string
	ConversationID      string
	Kind                OutboxKind
	IdempotencyKey      string
	PayloadHash         string
	Operation           string
	LocalMessageID      string
	TransportRequestID  string
	SendAgainOfOutboxID string
	ScheduledFor        time.Time
	ScheduledForMS      int64
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
	SendAgainOfOutboxID *string
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

// ListPendingParams scopes a deterministic pending-outbox read. Empty account
// and conversation IDs leave their respective dimensions unfiltered.
type ListPendingParams struct {
	AccountID      string
	ConversationID string
	Limit          int
}

// PendingRow embeds the durable outbox row and carries nullable sources used
// by the messaging layer to build kind-specific summaries.
type PendingRow struct {
	OutboxItem
	Body      *string
	MediaFile *string
	MediaMIME *string
	Emoji     *string
}

// CarryableIntent is the complete, self-contained payload needed to recreate
// one provably-unsent outgoing message in a freshly migrated store. The
// account-scoped remote conversation ID is the cross-store natural key; the
// old conversation primary key is retained only for operator-facing reports.
// MessagePresent distinguishes a missing optimistic row from a legitimate
// empty body, while empty blob fields identify a missing media carrier.
type CarryableIntent struct {
	OutboxItem
	RemoteConversationID string
	Body                 string
	ReplyToRemoteID      *string
	MessageOccurredAtMS  int64
	MessagePresent       bool
	BlobHash             string
	BlobSizeBytes        int64
	MIME                 string
	Filename             string
}

// CarryReviewIntent identifies a maybe-sent row that cutover must report and
// must never enqueue automatically.
type CarryReviewIntent struct {
	OutboxID             string
	AccountID            string
	ConversationID       string
	RemoteConversationID string
	Kind                 OutboxKind
	State                OutboxState
	IdempotencyKey       string
}

// OutboxAttachment is the persisted blob metadata for one media outbox intent.
// M2 stores exactly one attachment at ordinal zero. Enqueue stamps OutboxID,
// Ordinal, and CreatedAtMS; GetOutboxAttachment populates every field.
type OutboxAttachment struct {
	OutboxID    string
	BlobHash    string
	MIME        string
	Filename    string
	Ordinal     int64
	SizeBytes   int64
	CreatedAtMS int64
}

// OutboxReaction is the persisted payload for one reaction intent. Enqueue
// stamps OutboxID and CreatedAtMS; GetOutboxReaction populates every field.
type OutboxReaction struct {
	OutboxID        string
	TargetMessageID string
	Emoji           string
	Action          string
	CreatedAtMS     int64
}

// OutboxReadReceipt is the persisted payload for one read-receipt intent.
// Enqueue stamps OutboxID and CreatedAtMS; GetOutboxReadReceipt populates every
// field.
type OutboxReadReceipt struct {
	OutboxID          string
	DeviceID          string
	LastReadMessageID string
	ReadAtMS          int64
	CreatedAtMS       int64
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

// ReconcileRequest identifies one accepted transport request and its
// authoritative remote result without requiring an active dispatch lease.
type ReconcileRequest struct {
	AccountID          string
	TransportRequestID string
	ResultRemoteID     string
}

// ReconcileOutcome describes whether a transport result changed durable
// state. A missing correlation row and an inapplicable result are distinct so
// callers can observe the storage decision without treating either as an
// error.
type ReconcileOutcome string

const (
	ReconcileOutcomeReconciled ReconcileOutcome = "reconciled"
	ReconcileOutcomeEnriched   ReconcileOutcome = "enriched"
	ReconcileOutcomeNoop       ReconcileOutcome = "noop"
	ReconcileOutcomeNotFound   ReconcileOutcome = "notfound"
)

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

type enqueueCarriers struct {
	attachment  *OutboxAttachment
	reaction    *OutboxReaction
	readReceipt *OutboxReadReceipt
	readCursor  *ReadCursor
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
	return r.enqueue(ctx, item, nil, enqueueCarriers{})
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
	return r.enqueue(ctx, item, &message, enqueueCarriers{})
}

// EnqueueOutgoingMediaMessage atomically inserts a media intent, its
// optimistic outgoing message, and its blob metadata. An idempotent replay
// returns the existing intent only when its complete persisted pair exists.
func (r *OutboxRepository) EnqueueOutgoingMediaMessage(
	ctx context.Context,
	item NewOutboxItem,
	message Message,
	attachment OutboxAttachment,
) (OutboxItem, EnqueueDisposition, error) {
	return r.enqueue(ctx, item, &message, enqueueCarriers{attachment: &attachment})
}

// EnqueueReaction atomically inserts a message-less reaction intent and its
// typed carrier row. An idempotent replay requires the persisted pair to
// remain intact.
func (r *OutboxRepository) EnqueueReaction(
	ctx context.Context,
	item NewOutboxItem,
	reaction OutboxReaction,
) (OutboxItem, EnqueueDisposition, error) {
	return r.enqueue(ctx, item, nil, enqueueCarriers{reaction: &reaction})
}

// EnqueueReadReceipt atomically inserts a message-less read-receipt intent,
// its typed carrier row, and the monotone local read cursor. An idempotent
// replay validates the carrier but deliberately does not update the cursor.
func (r *OutboxRepository) EnqueueReadReceipt(
	ctx context.Context,
	item NewOutboxItem,
	receipt OutboxReadReceipt,
	cursor ReadCursor,
) (OutboxItem, EnqueueDisposition, error) {
	return r.enqueue(ctx, item, nil, enqueueCarriers{
		readReceipt: &receipt,
		readCursor:  &cursor,
	})
}

// GetOutboxAttachment returns the ordinal-zero attachment for outboxID. A
// missing row wraps database/sql.ErrNoRows so callers can use errors.Is.
func (r *OutboxRepository) GetOutboxAttachment(
	ctx context.Context,
	outboxID string,
) (OutboxAttachment, error) {
	if strings.TrimSpace(outboxID) == "" {
		return OutboxAttachment{}, fmt.Errorf("get outbox attachment: outbox ID is empty")
	}

	var attachment OutboxAttachment
	if err := r.store.db.QueryRowContext(ctx, `
		SELECT outbox_id, ordinal, blob_hash, size_bytes, mime, filename, created_at_ms
		FROM outbox_attachments
		WHERE outbox_id = ? AND ordinal = 0
	`, outboxID).Scan(
		&attachment.OutboxID,
		&attachment.Ordinal,
		&attachment.BlobHash,
		&attachment.SizeBytes,
		&attachment.MIME,
		&attachment.Filename,
		&attachment.CreatedAtMS,
	); err != nil {
		return OutboxAttachment{}, fmt.Errorf("get outbox attachment for outbox item %q: %w", outboxID, err)
	}
	return attachment, nil
}

// GetOutboxReaction returns the typed reaction payload for outboxID. A
// missing row wraps database/sql.ErrNoRows so callers can use errors.Is.
func (r *OutboxRepository) GetOutboxReaction(
	ctx context.Context,
	outboxID string,
) (OutboxReaction, error) {
	if strings.TrimSpace(outboxID) == "" {
		return OutboxReaction{}, fmt.Errorf("get outbox reaction: outbox ID is empty")
	}

	var reaction OutboxReaction
	if err := r.store.db.QueryRowContext(ctx, `
		SELECT outbox_id, target_message_id, emoji, action, created_at_ms
		FROM outbox_reactions
		WHERE outbox_id = ?
	`, outboxID).Scan(
		&reaction.OutboxID,
		&reaction.TargetMessageID,
		&reaction.Emoji,
		&reaction.Action,
		&reaction.CreatedAtMS,
	); err != nil {
		return OutboxReaction{}, fmt.Errorf(
			"get outbox reaction for outbox item %q: %w",
			outboxID,
			err,
		)
	}
	return reaction, nil
}

// GetOutboxReadReceipt returns the typed read-receipt payload for outboxID. A
// missing row wraps database/sql.ErrNoRows so callers can use errors.Is.
func (r *OutboxRepository) GetOutboxReadReceipt(
	ctx context.Context,
	outboxID string,
) (OutboxReadReceipt, error) {
	if strings.TrimSpace(outboxID) == "" {
		return OutboxReadReceipt{}, fmt.Errorf("get outbox read receipt: outbox ID is empty")
	}

	var receipt OutboxReadReceipt
	if err := r.store.db.QueryRowContext(ctx, `
		SELECT outbox_id, device_id, last_read_message_id, read_at_ms, created_at_ms
		FROM outbox_read_receipts
		WHERE outbox_id = ?
	`, outboxID).Scan(
		&receipt.OutboxID,
		&receipt.DeviceID,
		&receipt.LastReadMessageID,
		&receipt.ReadAtMS,
		&receipt.CreatedAtMS,
	); err != nil {
		return OutboxReadReceipt{}, fmt.Errorf(
			"get outbox read receipt for outbox item %q: %w",
			outboxID,
			err,
		)
	}
	return receipt, nil
}

func (r *OutboxRepository) enqueue(
	ctx context.Context,
	item NewOutboxItem,
	message *Message,
	carriers enqueueCarriers,
) (OutboxItem, EnqueueDisposition, error) {
	if err := validateNewOutboxItem(item); err != nil {
		return OutboxItem{}, "", fmt.Errorf("enqueue outbox item: %w", err)
	}
	if err := validateOutboxAttachmentPair(item, carriers); err != nil {
		return OutboxItem{}, "", fmt.Errorf("enqueue outbox item %q: %w", item.OutboxID, err)
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
			send_again_of_outbox_id,
			attempt_count,
			scheduled_for_ms,
			created_at_ms,
			updated_at_ms
		) VALUES (?, ?, ?, ?, ?, ?, ?, 'queued', ?, ?, ?, 0, ?, ?, ?)
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
		nullableOutboxText(item.SendAgainOfOutboxID),
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
		// The SendAgain link is part of an intent's identity: reusing one
		// idempotency key against two different predecessors (whose payloads may
		// hash identically) must conflict loudly, not silently return the first
		// successor.
		if err == nil && (row.Operation != item.Operation ||
			row.ConversationID != item.ConversationID ||
			row.PayloadHash != item.PayloadHash ||
			!sameSendAgainLink(row.SendAgainOfOutboxID, item.SendAgainOfOutboxID)) {
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
	if disposition == EnqueueExisting && row.Kind == OutboxKindMedia {
		if err := validateExistingOutboxAttachment(ctx, tx, row.OutboxID); err != nil {
			return OutboxItem{}, "", err
		}
	}
	if disposition == EnqueueExisting && row.Kind == OutboxKindReaction {
		if err := validateExistingOutboxReaction(ctx, tx, row.OutboxID); err != nil {
			return OutboxItem{}, "", err
		}
	}
	if disposition == EnqueueExisting && row.Kind == OutboxKindRead {
		if err := validateExistingOutboxReadReceipt(ctx, tx, row.OutboxID); err != nil {
			return OutboxItem{}, "", err
		}
	}
	if disposition == EnqueueInserted && message != nil {
		if err := r.insertOutgoingMessage(ctx, tx, item, *message, nowMS); err != nil {
			return OutboxItem{}, "", err
		}
	}
	if disposition == EnqueueInserted && carriers.attachment != nil {
		if err := r.insertOutboxAttachment(ctx, tx, item.OutboxID, *carriers.attachment, nowMS); err != nil {
			return OutboxItem{}, "", err
		}
	}
	if disposition == EnqueueInserted && carriers.reaction != nil {
		if err := r.insertOutboxReaction(ctx, tx, item.OutboxID, *carriers.reaction, nowMS); err != nil {
			return OutboxItem{}, "", err
		}
	}
	if disposition == EnqueueInserted && carriers.readReceipt != nil {
		if err := r.insertOutboxReadReceipt(ctx, tx, item.OutboxID, *carriers.readReceipt, nowMS); err != nil {
			return OutboxItem{}, "", err
		}
		if err := r.store.upsertReadCursorTx(ctx, tx, *carriers.readCursor); err != nil {
			return OutboxItem{}, "", err
		}
	}
	if err := tx.Commit(); err != nil {
		return OutboxItem{}, "", fmt.Errorf("enqueue outbox item %q: commit: %w", item.OutboxID, err)
	}
	return row, disposition, nil
}

func (r *OutboxRepository) insertOutboxAttachment(
	ctx context.Context,
	tx *sql.Tx,
	outboxID string,
	attachment OutboxAttachment,
	nowMS int64,
) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO outbox_attachments (
			outbox_id,
			ordinal,
			blob_hash,
			size_bytes,
			mime,
			filename,
			created_at_ms
		) VALUES (?, 0, ?, ?, ?, ?, ?)
	`,
		outboxID,
		attachment.BlobHash,
		attachment.SizeBytes,
		attachment.MIME,
		attachment.Filename,
		nowMS,
	)
	if err != nil {
		return fmt.Errorf(
			"enqueue outbox attachment for outbox item %q: %w",
			outboxID,
			mapConstraintError(err),
		)
	}
	return nil
}

func (r *OutboxRepository) insertOutboxReaction(
	ctx context.Context,
	tx *sql.Tx,
	outboxID string,
	reaction OutboxReaction,
	nowMS int64,
) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO outbox_reactions (
			outbox_id,
			target_message_id,
			emoji,
			action,
			created_at_ms
		) VALUES (?, ?, ?, ?, ?)
	`,
		outboxID,
		reaction.TargetMessageID,
		reaction.Emoji,
		reaction.Action,
		nowMS,
	)
	if err != nil {
		return fmt.Errorf(
			"enqueue outbox reaction for outbox item %q: %w",
			outboxID,
			mapConstraintError(err),
		)
	}
	return nil
}

func (r *OutboxRepository) insertOutboxReadReceipt(
	ctx context.Context,
	tx *sql.Tx,
	outboxID string,
	receipt OutboxReadReceipt,
	nowMS int64,
) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO outbox_read_receipts (
			outbox_id,
			device_id,
			last_read_message_id,
			read_at_ms,
			created_at_ms
		) VALUES (?, ?, ?, ?, ?)
	`,
		outboxID,
		receipt.DeviceID,
		receipt.LastReadMessageID,
		receipt.ReadAtMS,
		nowMS,
	)
	if err != nil {
		return fmt.Errorf(
			"enqueue outbox read receipt for outbox item %q: %w",
			outboxID,
			mapConstraintError(err),
		)
	}
	return nil
}

func validateOutboxAttachmentPair(
	item NewOutboxItem,
	carriers enqueueCarriers,
) error {
	switch item.Kind {
	case OutboxKindMedia:
		if carriers.attachment == nil {
			return fmt.Errorf("media kind requires an attachment")
		}
		if carriers.reaction != nil || carriers.readReceipt != nil || carriers.readCursor != nil {
			return fmt.Errorf("media kind accepts only an attachment carrier")
		}
	case OutboxKindReaction:
		if carriers.reaction == nil {
			return fmt.Errorf("reaction kind requires a reaction carrier")
		}
		if carriers.attachment != nil || carriers.readReceipt != nil || carriers.readCursor != nil {
			return fmt.Errorf("reaction kind accepts only a reaction carrier")
		}
	case OutboxKindRead:
		if carriers.readReceipt == nil {
			return fmt.Errorf("read kind requires a read-receipt carrier")
		}
		if carriers.readCursor == nil {
			return fmt.Errorf("read kind requires a read cursor")
		}
		if carriers.attachment != nil || carriers.reaction != nil {
			return fmt.Errorf("read kind accepts only a read-receipt carrier")
		}
	default:
		if carriers.attachment != nil {
			return fmt.Errorf("kind %q does not accept an attachment", item.Kind)
		}
		if carriers.reaction != nil {
			return fmt.Errorf("kind %q does not accept a reaction carrier", item.Kind)
		}
		if carriers.readReceipt != nil || carriers.readCursor != nil {
			return fmt.Errorf("kind %q does not accept a read-receipt carrier", item.Kind)
		}
	}
	if carriers.attachment != nil {
		if err := validateBlobHash(carriers.attachment.BlobHash); err != nil {
			return err
		}
		if carriers.attachment.SizeBytes <= 0 {
			return fmt.Errorf("outbox attachment size must be positive")
		}
		if strings.TrimSpace(carriers.attachment.MIME) == "" {
			return fmt.Errorf("outbox attachment MIME type is empty")
		}
	}
	return nil
}

func validateExistingOutboxAttachment(
	ctx context.Context,
	tx *sql.Tx,
	outboxID string,
) error {
	var exists bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM outbox_attachments
			WHERE outbox_id = ? AND ordinal = 0
		)
	`, outboxID).Scan(&exists); err != nil {
		return fmt.Errorf(
			"enqueue outgoing media message for existing outbox item %q: inspect attachment: %w",
			outboxID,
			err,
		)
	}
	if !exists {
		return fmt.Errorf(
			"enqueue outgoing media message for existing outbox item %q: attachment is missing",
			outboxID,
		)
	}
	return nil
}

func validateExistingOutboxReaction(
	ctx context.Context,
	tx *sql.Tx,
	outboxID string,
) error {
	var exists bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM outbox_reactions
			WHERE outbox_id = ?
		)
	`, outboxID).Scan(&exists); err != nil {
		return fmt.Errorf(
			"enqueue reaction for existing outbox item %q: inspect reaction: %w",
			outboxID,
			err,
		)
	}
	if !exists {
		return fmt.Errorf(
			"enqueue reaction for existing outbox item %q: reaction is missing",
			outboxID,
		)
	}
	return nil
}

func validateExistingOutboxReadReceipt(
	ctx context.Context,
	tx *sql.Tx,
	outboxID string,
) error {
	var exists bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM outbox_read_receipts
			WHERE outbox_id = ?
		)
	`, outboxID).Scan(&exists); err != nil {
		return fmt.Errorf(
			"enqueue read receipt for existing outbox item %q: inspect receipt: %w",
			outboxID,
			err,
		)
	}
	if !exists {
		return fmt.Errorf(
			"enqueue read receipt for existing outbox item %q: receipt is missing",
			outboxID,
		)
	}
	return nil
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
		!isRequestBoundOrConfirmedRemoteID(message.RemoteMessageID, item) {
		return fmt.Errorf(
			"enqueue outgoing message for existing outbox item %q: %w: message %q is not its active request-bound outgoing pair",
			item.OutboxID,
			ErrInvalidMessage,
			message.MessageID,
		)
	}
	return nil
}

// isRequestBoundOrConfirmedRemoteID accepts the message's remote id in both of
// its legitimate lifetimes: the placeholder transport_request_id it is born
// with, and the confirmed result id the reconcile repoint moves it to. Without
// the second arm, an idempotent replay AFTER delivery (the exact case the
// idempotency key exists for -- a lost API response, client retries the same
// key) would spuriously fail pair validation on any platform whose real remote
// id differs from the request id (Signal at Confirm time, Google at echo time).
func isRequestBoundOrConfirmedRemoteID(remoteMessageID string, item OutboxItem) bool {
	if remoteMessageID == item.TransportRequestID {
		return true
	}
	return item.ResultRemoteID != nil && remoteMessageID == *item.ResultRemoteID
}

// EarliestDue returns the wall-clock time at which the earliest queued or
// not-dispatched row becomes leaseable, or ok=false when none exist.
func (r *OutboxRepository) EarliestDue(ctx context.Context) (time.Time, bool, error) {
	var earliestMS sql.NullInt64
	if err := r.store.db.QueryRowContext(ctx, `
		SELECT MIN(COALESCE(next_attempt_at_ms, scheduled_for_ms))
		FROM outbox
		WHERE state IN ('queued','not_dispatched')
	`).Scan(&earliestMS); err != nil {
		return time.Time{}, false, fmt.Errorf("find earliest due outbox item: %w", err)
	}
	if !earliestMS.Valid {
		return time.Time{}, false, nil
	}
	return time.UnixMilli(earliestMS.Int64), true, nil
}

// ListCarryable returns every provably-unsent outgoing intent with the full
// message/media payload required to enqueue it in another store. It is
// intentionally unbounded: cutover is an offline, all-or-reported operation,
// so silently truncating the carry set would lose durable work.
func (r *OutboxRepository) ListCarryable(ctx context.Context) ([]CarryableIntent, error) {
	rows, err := r.store.db.QueryContext(ctx, `
		SELECT `+prefixedOutboxColumns("o")+`,
			c.remote_conversation_id,
			m.message_id,
			m.body,
			m.reply_to_remote_id,
			m.occurred_at_ms,
			oa.blob_hash,
			oa.size_bytes,
			oa.mime,
			oa.filename
		FROM outbox o
		LEFT JOIN conversations c
			ON c.account_id = o.account_id AND c.conversation_id = o.conversation_id
		LEFT JOIN messages m
			ON m.message_id = o.local_message_id
			AND m.account_id = o.account_id
			AND m.conversation_id = o.conversation_id
		LEFT JOIN outbox_attachments oa
			ON oa.outbox_id = o.outbox_id AND oa.ordinal = 0
		WHERE o.state IN ('queued', 'not_dispatched')
		ORDER BY o.created_at_ms, o.outbox_id
	`)
	if err != nil {
		return nil, fmt.Errorf("list carryable outbox items: query: %w", err)
	}
	items, err := collectRows(rows, scanCarryableIntent)
	if err != nil {
		return nil, fmt.Errorf("list carryable outbox items: scan: %w", err)
	}
	return items, nil
}

// ListCarryReview returns the nonterminal states whose transport outcome may
// be known or unknown but is never provably unsent. This separate enumerator
// keeps ListCarryable's safety contract exact while giving cutover complete
// operator-facing evidence for uncertain, in-flight, and store-failed rows.
func (r *OutboxRepository) ListCarryReview(ctx context.Context) ([]CarryReviewIntent, error) {
	rows, err := r.store.db.QueryContext(ctx, `
		SELECT
			o.outbox_id,
			o.account_id,
			o.conversation_id,
			COALESCE(c.remote_conversation_id, ''),
			o.kind,
			o.state,
			o.idempotency_key
		FROM outbox o
		LEFT JOIN conversations c
			ON c.account_id = o.account_id AND c.conversation_id = o.conversation_id
		WHERE o.state IN ('uncertain', 'dispatching', 'store_failed')
		ORDER BY o.created_at_ms, o.outbox_id
	`)
	if err != nil {
		return nil, fmt.Errorf("list carry-review outbox items: query: %w", err)
	}
	items, err := collectRows(rows, func(row rowScanner) (CarryReviewIntent, error) {
		var item CarryReviewIntent
		err := row.Scan(
			&item.OutboxID,
			&item.AccountID,
			&item.ConversationID,
			&item.RemoteConversationID,
			&item.Kind,
			&item.State,
			&item.IdempotencyKey,
		)
		return item, err
	})
	if err != nil {
		return nil, fmt.Errorf("list carry-review outbox items: scan: %w", err)
	}
	return items, nil
}

// ListPending returns exactly the cancelable outbox set, ordered by the same
// due key used by LeaseDue and enriched with nullable per-kind summary data.
func (r *OutboxRepository) ListPending(
	ctx context.Context,
	p ListPendingParams,
) ([]PendingRow, error) {
	if p.Limit <= 0 {
		return nil, fmt.Errorf("list pending outbox items: limit must be positive")
	}

	query := `
		SELECT ` + prefixedOutboxColumns("o") + `,
			m.body,
			oa.filename,
			oa.mime,
			orx.emoji
		FROM outbox o
		LEFT JOIN messages m ON m.message_id = o.local_message_id
		LEFT JOIN outbox_attachments oa ON oa.outbox_id = o.outbox_id AND oa.ordinal = 0
		LEFT JOIN outbox_reactions orx ON orx.outbox_id = o.outbox_id
		WHERE o.state IN ('queued','not_dispatched')`
	args := make([]any, 0, 3)
	if p.AccountID != "" {
		query += ` AND o.account_id = ?`
		args = append(args, p.AccountID)
	}
	if p.ConversationID != "" {
		query += ` AND o.conversation_id = ?`
		args = append(args, p.ConversationID)
	}
	query += `
		ORDER BY COALESCE(o.next_attempt_at_ms, o.scheduled_for_ms), o.created_at_ms, o.outbox_id
		LIMIT ?`
	args = append(args, p.Limit)

	rows, err := r.store.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list pending outbox items: query: %w", err)
	}
	items, err := collectRows(rows, scanPendingRow)
	if err != nil {
		return nil, fmt.Errorf("list pending outbox items: scan: %w", err)
	}
	return items, nil
}

// ListConfirmedSince returns confirmed outbox rows updated strictly after the
// watermark, ordered deterministically and enriched with nullable per-kind
// payload data.
func (r *OutboxRepository) ListConfirmedSince(
	ctx context.Context,
	sinceMS int64,
	limit int,
) ([]PendingRow, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("list confirmed outbox items: limit must be positive")
	}

	rows, err := r.store.db.QueryContext(ctx, `
		SELECT `+prefixedOutboxColumns("o")+`,
			m.body,
			oa.filename,
			oa.mime,
			orx.emoji
		FROM outbox o
		LEFT JOIN messages m ON m.message_id = o.local_message_id
		LEFT JOIN outbox_attachments oa ON oa.outbox_id = o.outbox_id AND oa.ordinal = 0
		LEFT JOIN outbox_reactions orx ON orx.outbox_id = o.outbox_id
		WHERE o.state = 'confirmed'
		  AND o.updated_at_ms > ?
		ORDER BY o.updated_at_ms, o.outbox_id
		LIMIT ?
	`, sinceMS, limit)
	if err != nil {
		return nil, fmt.Errorf("list confirmed outbox items: query: %w", err)
	}
	items, err := collectRows(rows, scanPendingRow)
	if err != nil {
		return nil, fmt.Errorf("list confirmed outbox items: scan: %w", err)
	}
	return items, nil
}

// LeaseDue atomically claims due queued and retryable rows. An uncertain row is
// structurally ineligible regardless of its next-attempt value.
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
		  AND state IN ('queued', 'not_dispatched')
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
			  AND state IN ('queued', 'not_dispatched')
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

// ConfirmWithoutResult terminally confirms an active called lease whose
// operation yields no remote result identity.
func (r *OutboxRepository) ConfirmWithoutResult(
	ctx context.Context,
	outboxID, leaseToken string,
) error {
	nowMS, err := r.nowMS("confirm outbox item without result")
	if err != nil {
		return err
	}
	result, err := r.store.db.ExecContext(ctx, `
		UPDATE outbox
		SET state = 'confirmed',
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
	`, nowMS, outboxID, leaseToken)
	if err != nil {
		return fmt.Errorf("confirm outbox item %q without result: %w", outboxID, err)
	}
	return r.requireLeaseMutation(ctx, "confirm without result", outboxID, result)
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

	var (
		accountID          string
		conversationID     string
		localMessageID     sql.NullString
		transportRequestID string
	)
	if err := tx.QueryRowContext(ctx, `
		SELECT account_id, conversation_id, local_message_id, transport_request_id
		FROM outbox
		WHERE outbox_id = ?
	`, confirmation.OutboxID).Scan(
		&accountID,
		&conversationID,
		&localMessageID,
		&transportRequestID,
	); err != nil {
		return fmt.Errorf(
			"confirm outbox item %q: read local message identity: %w",
			confirmation.OutboxID,
			err,
		)
	}
	if localMessageID.Valid {
		if err := r.repointLocalMessage(
			ctx,
			tx,
			accountID,
			conversationID,
			localMessageID.String,
			transportRequestID,
			resultRemoteID,
		); err != nil {
			return fmt.Errorf("confirm outbox item %q: %w", confirmation.OutboxID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("confirm outbox item %q: commit: %w", confirmation.OutboxID, err)
	}
	return nil
}

// ReconcileConfirm applies an accepted transport result without requiring a
// dispatch lease. The account-scoped transport request ID is the sole
// correlation key.
func (r *OutboxRepository) ReconcileConfirm(
	ctx context.Context,
	req ReconcileRequest,
) (ReconcileOutcome, error) {
	checks := []struct {
		name  string
		value string
	}{
		{name: "account ID", value: req.AccountID},
		{name: "transport request ID", value: req.TransportRequestID},
		{name: "remote result ID", value: req.ResultRemoteID},
	}
	for _, check := range checks {
		if strings.TrimSpace(check.value) == "" {
			return "", fmt.Errorf("reconcile transport result: %s is empty", check.name)
		}
	}

	tx, err := r.store.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("reconcile transport result: begin transaction: %w", err)
	}
	defer tx.Rollback()

	var (
		outboxID                string
		conversationID          string
		state                   OutboxState
		localMessageID          sql.NullString
		persistedResultRemoteID sql.NullString
		localRemoteMessageID    sql.NullString
	)
	err = tx.QueryRowContext(ctx, `
		SELECT
			o.outbox_id,
			o.conversation_id,
			o.state,
			o.local_message_id,
			o.result_remote_id,
			m.remote_message_id
		FROM outbox AS o
		LEFT JOIN messages AS m ON m.message_id = o.local_message_id
		WHERE o.account_id = ? AND o.transport_request_id = ?
	`, req.AccountID, req.TransportRequestID).Scan(
		&outboxID,
		&conversationID,
		&state,
		&localMessageID,
		&persistedResultRemoteID,
		&localRemoteMessageID,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return ReconcileOutcomeNotFound, nil
	}
	if err != nil {
		return "", fmt.Errorf("reconcile transport result: read correlated outbox item: %w", err)
	}

	switch state {
	case OutboxUncertain, OutboxStoreFailed:
		nowMS, err := r.nowMS("reconcile transport result")
		if err != nil {
			return "", err
		}
		result, err := tx.ExecContext(ctx, `
			UPDATE outbox
			SET state = 'confirmed',
				result_remote_id = ?,
				error_class = NULL,
				error_code = NULL,
				error_detail = NULL,
				next_attempt_at_ms = NULL,
				updated_at_ms = ?
			WHERE outbox_id = ?
			  AND state IN ('uncertain', 'store_failed')
		`, req.ResultRemoteID, nowMS, outboxID)
		if err != nil {
			return "", fmt.Errorf("reconcile outbox item %q: confirm: %w", outboxID, err)
		}
		if err := requireSingleReconcileMutation(result, outboxID); err != nil {
			return "", err
		}
		if localMessageID.Valid &&
			localRemoteMessageID.Valid &&
			localRemoteMessageID.String == req.TransportRequestID &&
			req.ResultRemoteID != req.TransportRequestID {
			if err := r.repointLocalMessage(
				ctx,
				tx,
				req.AccountID,
				conversationID,
				localMessageID.String,
				req.TransportRequestID,
				req.ResultRemoteID,
			); err != nil {
				return "", fmt.Errorf("reconcile outbox item %q: %w", outboxID, err)
			}
		}
		if err := tx.Commit(); err != nil {
			return "", fmt.Errorf("reconcile outbox item %q: commit: %w", outboxID, err)
		}
		return ReconcileOutcomeReconciled, nil

	case OutboxConfirmed:
		if persistedResultRemoteID.Valid && persistedResultRemoteID.String == req.ResultRemoteID {
			return ReconcileOutcomeNoop, nil
		}
		if !localMessageID.Valid ||
			!localRemoteMessageID.Valid ||
			localRemoteMessageID.String != req.TransportRequestID ||
			req.ResultRemoteID == req.TransportRequestID {
			return ReconcileOutcomeNoop, nil
		}

		nowMS, err := r.nowMS("reconcile transport result")
		if err != nil {
			return "", err
		}
		result, err := tx.ExecContext(ctx, `
			UPDATE outbox
			SET result_remote_id = ?,
				error_class = NULL,
				error_code = NULL,
				error_detail = NULL,
				updated_at_ms = ?
			WHERE outbox_id = ?
			  AND state = 'confirmed'
			  AND result_remote_id = transport_request_id
		`, req.ResultRemoteID, nowMS, outboxID)
		if err != nil {
			return "", fmt.Errorf("reconcile outbox item %q: enrich: %w", outboxID, err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return "", fmt.Errorf(
				"reconcile outbox item %q: read enrich rows affected: %w",
				outboxID,
				err,
			)
		}
		if affected == 0 {
			return ReconcileOutcomeNoop, nil
		}
		if affected != 1 {
			return "", fmt.Errorf(
				"reconcile outbox item %q: enrich affected %d rows, want 1",
				outboxID,
				affected,
			)
		}
		if err := r.repointLocalMessage(
			ctx,
			tx,
			req.AccountID,
			conversationID,
			localMessageID.String,
			req.TransportRequestID,
			req.ResultRemoteID,
		); err != nil {
			return "", fmt.Errorf("reconcile outbox item %q: %w", outboxID, err)
		}
		if err := tx.Commit(); err != nil {
			return "", fmt.Errorf("reconcile outbox item %q: commit enrichment: %w", outboxID, err)
		}
		return ReconcileOutcomeEnriched, nil

	default:
		return ReconcileOutcomeNoop, nil
	}
}

func requireSingleReconcileMutation(result sql.Result, outboxID string) error {
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf(
			"reconcile outbox item %q: read confirm rows affected: %w",
			outboxID,
			err,
		)
	}
	if affected != 1 {
		return fmt.Errorf(
			"reconcile outbox item %q: confirm affected %d rows, want 1",
			outboxID,
			affected,
		)
	}
	return nil
}

func (r *OutboxRepository) repointLocalMessage(
	ctx context.Context,
	tx *sql.Tx,
	accountID, conversationID, localMessageID, transportRequestID, realID string,
) error {
	if realID == transportRequestID {
		return nil
	}
	nowMS, err := r.nowMS("repoint local message")
	if err != nil {
		return err
	}

	var collisionMessageID string
	err = tx.QueryRowContext(ctx, `
		SELECT message_id
		FROM messages
		WHERE account_id = ?
		  AND conversation_id = ?
		  AND remote_message_id = ?
	`, accountID, conversationID, realID).Scan(&collisionMessageID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("repoint local message %q: read remote-ID collision: %w", localMessageID, err)
	}
	if err == nil && collisionMessageID != localMessageID {
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM messages
			WHERE message_id = ?
		`, collisionMessageID); err != nil {
			return fmt.Errorf(
				"repoint local message %q: delete echo duplicate %q: %w",
				localMessageID,
				collisionMessageID,
				mapConstraintError(err),
			)
		}
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE messages
		SET remote_message_id = ?,
			updated_at_ms = ?
		WHERE message_id = ?
	`, realID, nowMS, localMessageID); err != nil {
		return fmt.Errorf(
			"repoint local message %q: update remote ID: %w",
			localMessageID,
			mapConstraintError(err),
		)
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

// FindStoreFailedDue returns a deterministic bounded batch of accepted
// results whose local confirmation still needs repair.
func (r *OutboxRepository) FindStoreFailedDue(
	ctx context.Context,
	limit int,
) ([]OutboxItem, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("find store-failed outbox items: limit must be positive")
	}
	rows, err := r.store.db.QueryContext(ctx, `
		SELECT `+outboxColumns+`
		FROM outbox
		WHERE state = 'store_failed'
		ORDER BY updated_at_ms, outbox_id
		LIMIT ?
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("find store-failed outbox items: query: %w", err)
	}
	items, err := collectRows(rows, scanOutboxItem)
	if err != nil {
		return nil, fmt.Errorf("find store-failed outbox items: scan: %w", err)
	}
	return items, nil
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
	send_again_of_outbox_id,
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

func prefixedOutboxColumns(alias string) string {
	columns := strings.TrimSpace(outboxColumns)
	return alias + "." + strings.ReplaceAll(columns, "\n\t", "\n\t"+alias+".")
}

func scanPendingRow(row rowScanner) (PendingRow, error) {
	var pending PendingRow
	err := row.Scan(
		&pending.OutboxID,
		&pending.AccountID,
		&pending.ConversationID,
		&pending.Kind,
		&pending.IdempotencyKey,
		&pending.PayloadHash,
		&pending.Operation,
		&pending.State,
		&pending.LocalMessageID,
		&pending.TransportRequestID,
		&pending.SendAgainOfOutboxID,
		&pending.ResultRemoteID,
		&pending.ErrorClass,
		&pending.ErrorCode,
		&pending.ErrorDetail,
		&pending.AttemptCount,
		&pending.LeaseOwner,
		&pending.LeaseToken,
		&pending.LeaseExpiresAtMS,
		&pending.TransportCalledAtMS,
		&pending.ScheduledForMS,
		&pending.NextAttemptAtMS,
		&pending.CreatedAtMS,
		&pending.UpdatedAtMS,
		&pending.Body,
		&pending.MediaFile,
		&pending.MediaMIME,
		&pending.Emoji,
	)
	return pending, err
}

func scanCarryableIntent(row rowScanner) (CarryableIntent, error) {
	var (
		intent               CarryableIntent
		remoteConversationID sql.NullString
		messageID            sql.NullString
		body                 sql.NullString
		replyToRemoteID      sql.NullString
		messageOccurredAtMS  sql.NullInt64
		blobHash             sql.NullString
		blobSizeBytes        sql.NullInt64
		mime                 sql.NullString
		filename             sql.NullString
	)
	err := row.Scan(
		&intent.OutboxID,
		&intent.AccountID,
		&intent.ConversationID,
		&intent.Kind,
		&intent.IdempotencyKey,
		&intent.PayloadHash,
		&intent.Operation,
		&intent.State,
		&intent.LocalMessageID,
		&intent.TransportRequestID,
		&intent.SendAgainOfOutboxID,
		&intent.ResultRemoteID,
		&intent.ErrorClass,
		&intent.ErrorCode,
		&intent.ErrorDetail,
		&intent.AttemptCount,
		&intent.LeaseOwner,
		&intent.LeaseToken,
		&intent.LeaseExpiresAtMS,
		&intent.TransportCalledAtMS,
		&intent.ScheduledForMS,
		&intent.NextAttemptAtMS,
		&intent.CreatedAtMS,
		&intent.UpdatedAtMS,
		&remoteConversationID,
		&messageID,
		&body,
		&replyToRemoteID,
		&messageOccurredAtMS,
		&blobHash,
		&blobSizeBytes,
		&mime,
		&filename,
	)
	if err != nil {
		return CarryableIntent{}, err
	}
	intent.RemoteConversationID = remoteConversationID.String
	intent.MessagePresent = messageID.Valid
	intent.Body = body.String
	if replyToRemoteID.Valid {
		intent.ReplyToRemoteID = &replyToRemoteID.String
	}
	intent.MessageOccurredAtMS = messageOccurredAtMS.Int64
	intent.BlobHash = blobHash.String
	intent.BlobSizeBytes = blobSizeBytes.Int64
	intent.MIME = mime.String
	intent.Filename = filename.String
	return intent, nil
}

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
		&item.SendAgainOfOutboxID,
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
	if item.SendAgainOfOutboxID != "" && strings.TrimSpace(item.SendAgainOfOutboxID) == "" {
		return fmt.Errorf("send again predecessor outbox ID is empty")
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

// sameSendAgainLink compares a stored nullable link against an incoming
// command's link, where the empty string means "no link" (stored as NULL).
func sameSendAgainLink(stored *string, incoming string) bool {
	if stored == nil {
		return incoming == ""
	}
	return *stored == incoming
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
