// Package cutover contains explicit, offline mechanisms used by the cutover
// runbook. It does not rename or delete operator-owned stores.
package cutover

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/maxghenis/openmessage/internal/storage/blob"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

// CarrySource exposes the old-store reads needed to classify pending work and
// stream any referenced attachment. ListCarryable must contain only queued and
// not_dispatched rows; ListCarryReview contains uncertain, dispatching, and
// store_failed rows for operator review.
type CarrySource interface {
	ListCarryable(context.Context) ([]sqlite.CarryableIntent, error)
	ListCarryReview(context.Context) ([]sqlite.CarryReviewIntent, error)
	Open(blob.BlobRef) (io.ReadCloser, error)
}

// CarryTarget exposes the fresh-store operations needed to recreate a safe
// outbound intent. The UNIQUE(account_id, idempotency_key) constraint behind
// the enqueue methods is the final idempotency boundary.
type CarryTarget interface {
	GetConversationByRemote(accountID, remoteConversationID string) (sqlite.Conversation, error)
	Put(context.Context, io.Reader, string, int64) (blob.BlobRef, error)
	EnqueueOutgoingMessage(
		context.Context,
		sqlite.NewOutboxItem,
		sqlite.Message,
	) (sqlite.OutboxItem, sqlite.EnqueueDisposition, error)
	EnqueueOutgoingMediaMessage(
		context.Context,
		sqlite.NewOutboxItem,
		sqlite.Message,
		sqlite.OutboxAttachment,
	) (sqlite.OutboxItem, sqlite.EnqueueDisposition, error)
}

// Endpoint combines a SQLite store, its outbox repository, and the adjacent
// blob store into the CarrySource/CarryTarget seams. The embedded values
// deliberately expose their existing methods without adapter-only variants.
type Endpoint struct {
	*sqlite.Store
	*sqlite.OutboxRepository
	*blob.BlobStore
}

// NewEndpoint wraps one v2 store directory for a cutover carry operation.
func NewEndpoint(
	store *sqlite.Store,
	blobs *blob.BlobStore,
	now func() time.Time,
) (Endpoint, error) {
	if store == nil {
		return Endpoint{}, fmt.Errorf("create cutover endpoint: sqlite store is nil")
	}
	if blobs == nil {
		return Endpoint{}, fmt.Errorf("create cutover endpoint: blob store is nil")
	}
	outbox, err := sqlite.NewOutboxRepository(store, now)
	if err != nil {
		return Endpoint{}, fmt.Errorf("create cutover endpoint: %w", err)
	}
	return Endpoint{
		Store:            store,
		OutboxRepository: outbox,
		BlobStore:        blobs,
	}, nil
}

// CarryEntry is one operator-visible cutover decision. Reason is populated
// only for Uncarryable rows; Reported rows expose the old maybe-sent State that
// prevents automatic retry.
type CarryEntry struct {
	OutboxID             string
	AccountID            string
	ConversationID       string
	RemoteConversationID string
	IdempotencyKey       string
	Kind                 sqlite.OutboxKind
	State                sqlite.OutboxState
	Reason               string
}

// CarryReport is the complete evidence emitted by a carry pass. Carried lists
// only newly inserted rows, so an idempotent second pass has zero Carried rows.
// Reported is repeated on every pass because manual-review obligations remain.
type CarryReport struct {
	Carried     []CarryEntry
	Reported    []CarryEntry
	Uncarryable []CarryEntry
}

// CarryPendingOutbox copies only provably-unsent queued/not_dispatched intents
// from old to fresh. It reports maybe-sent states without retrying them and
// leaves terminal states untouched. Neither endpoint is renamed or deleted.
//
// PRECONDITION (the zero-double-send guarantee depends on it): the old store's
// backend MUST be stopped before this runs. The cutover runbook (R8) enforces
// this by ordering — stop the backend, move the store aside, then open it here
// — and callers should open the old endpoint read-only, since carry never
// writes it. A live dispatcher against the old store could transition a row
// between ListCarryReview and ListCarryable, so this function assumes a
// quiescent source and does not defend against a concurrent writer.
func CarryPendingOutbox(
	ctx context.Context,
	old CarrySource,
	fresh CarryTarget,
) (CarryReport, error) {
	report := CarryReport{
		Carried:     make([]CarryEntry, 0),
		Reported:    make([]CarryEntry, 0),
		Uncarryable: make([]CarryEntry, 0),
	}
	if ctx == nil {
		return report, fmt.Errorf("carry pending outbox: context is nil")
	}
	if old == nil {
		return report, fmt.Errorf("carry pending outbox: old source is nil")
	}
	if fresh == nil {
		return report, fmt.Errorf("carry pending outbox: fresh target is nil")
	}

	review, err := old.ListCarryReview(ctx)
	if err != nil {
		return report, fmt.Errorf("carry pending outbox: list manual-review rows: %w", err)
	}
	for _, item := range review {
		report.Reported = append(report.Reported, carryEntryFromReview(item))
	}

	intents, err := old.ListCarryable(ctx)
	if err != nil {
		return report, fmt.Errorf("carry pending outbox: list carryable rows: %w", err)
	}
	for _, intent := range intents {
		entry := carryEntryFromIntent(intent)
		if intent.State != sqlite.OutboxQueued && intent.State != sqlite.OutboxNotDispatched {
			return report, fmt.Errorf(
				"carry pending outbox item %q: source returned non-carryable state %q",
				intent.OutboxID,
				intent.State,
			)
		}
		if strings.TrimSpace(intent.RemoteConversationID) == "" {
			entry.Reason = "old conversation has no remote conversation natural key"
			report.Uncarryable = append(report.Uncarryable, entry)
			continue
		}

		conversation, err := fresh.GetConversationByRemote(
			intent.AccountID,
			intent.RemoteConversationID,
		)
		if errors.Is(err, sqlite.ErrNotFound) {
			entry.Reason = fmt.Sprintf(
				"unresolved fresh conversation for account %q and remote ID %q",
				intent.AccountID,
				intent.RemoteConversationID,
			)
			report.Uncarryable = append(report.Uncarryable, entry)
			continue
		}
		if err != nil {
			return report, fmt.Errorf(
				"carry pending outbox item %q: resolve fresh conversation: %w",
				intent.OutboxID,
				err,
			)
		}
		if conversation.AccountID != intent.AccountID {
			return report, fmt.Errorf(
				"carry pending outbox item %q: fresh conversation %q belongs to account %q, want %q",
				intent.OutboxID,
				conversation.ConversationID,
				conversation.AccountID,
				intent.AccountID,
			)
		}
		if !intent.MessagePresent || intent.LocalMessageID == nil ||
			strings.TrimSpace(*intent.LocalMessageID) == "" || intent.MessageOccurredAtMS <= 0 {
			entry.Reason = "optimistic outgoing message payload is missing"
			report.Uncarryable = append(report.Uncarryable, entry)
			continue
		}

		item := sqlite.NewOutboxItem{
			OutboxID:           intent.OutboxID,
			AccountID:          intent.AccountID,
			ConversationID:     conversation.ConversationID,
			Kind:               intent.Kind,
			IdempotencyKey:     intent.IdempotencyKey,
			PayloadHash:        intent.PayloadHash,
			Operation:          intent.Operation,
			LocalMessageID:     *intent.LocalMessageID,
			TransportRequestID: intent.TransportRequestID,
			ScheduledForMS:     intent.ScheduledForMS,
		}
		// A carried intent keeps its send window: if the window closed while
		// the stores were cut over, the fresh store's expiry sweep cancels it
		// instead of transmitting stale.
		if intent.ExpiresAtMS != nil {
			item.ExpiresAtMS = *intent.ExpiresAtMS
		}
		message := sqlite.Message{
			MessageID:       *intent.LocalMessageID,
			ConversationID:  conversation.ConversationID,
			AccountID:       intent.AccountID,
			RemoteMessageID: intent.TransportRequestID,
			Direction:       sqlite.MessageDirectionOutgoing,
			Body:            intent.Body,
			ReplyToRemoteID: intent.ReplyToRemoteID,
			State:           sqlite.MessageStateActive,
			OccurredAtMS:    intent.MessageOccurredAtMS,
		}

		var (
			carried     sqlite.OutboxItem
			disposition sqlite.EnqueueDisposition
		)
		switch intent.Kind {
		case sqlite.OutboxKindText:
			carried, disposition, err = fresh.EnqueueOutgoingMessage(ctx, item, message)

		case sqlite.OutboxKindMedia:
			if strings.TrimSpace(intent.BlobHash) == "" || intent.BlobSizeBytes <= 0 ||
				strings.TrimSpace(intent.MIME) == "" {
				entry.Reason = "media blob metadata is missing"
				report.Uncarryable = append(report.Uncarryable, entry)
				continue
			}
			reader, openErr := old.Open(blob.BlobRef{Hash: intent.BlobHash})
			if openErr != nil {
				entry.Reason = fmt.Sprintf("media blob %q is unavailable: %v", intent.BlobHash, openErr)
				report.Uncarryable = append(report.Uncarryable, entry)
				continue
			}
			copied, putErr := fresh.Put(ctx, reader, intent.MIME, intent.BlobSizeBytes)
			closeErr := reader.Close()
			if putErr != nil {
				return report, fmt.Errorf(
					"carry pending outbox item %q: copy media blob %q: %w",
					intent.OutboxID,
					intent.BlobHash,
					putErr,
				)
			}
			if closeErr != nil {
				return report, fmt.Errorf(
					"carry pending outbox item %q: close media blob %q: %w",
					intent.OutboxID,
					intent.BlobHash,
					closeErr,
				)
			}
			if copied.Hash != intent.BlobHash || copied.Size != intent.BlobSizeBytes {
				entry.Reason = fmt.Sprintf(
					"media blob %q failed integrity check (copied hash %q, size %d; want size %d)",
					intent.BlobHash,
					copied.Hash,
					copied.Size,
					intent.BlobSizeBytes,
				)
				report.Uncarryable = append(report.Uncarryable, entry)
				continue
			}
			carried, disposition, err = fresh.EnqueueOutgoingMediaMessage(
				ctx,
				item,
				message,
				sqlite.OutboxAttachment{
					BlobHash:  intent.BlobHash,
					SizeBytes: intent.BlobSizeBytes,
					MIME:      intent.MIME,
					Filename:  intent.Filename,
				},
			)

		default:
			entry.Reason = fmt.Sprintf("outbox kind %q has no cutover re-enqueue payload", intent.Kind)
			report.Uncarryable = append(report.Uncarryable, entry)
			continue
		}
		if err != nil {
			// A foreign collision — the key already names a different fresh
			// intent (migration- or ingest-created) — is an anomaly to surface,
			// not a reason to abort every later row. Report it un-carryable
			// (loud) and keep the batch moving; the pre-existing fresh row is
			// untouched and this intent is neither carried nor dropped.
			if errors.Is(err, sqlite.ErrIdempotencyConflict) {
				entry.Reason = fmt.Sprintf("idempotency key %q already names a different fresh intent", intent.IdempotencyKey)
				report.Uncarryable = append(report.Uncarryable, entry)
				continue
			}
			return report, fmt.Errorf(
				"carry pending outbox item %q into conversation %q: %w",
				intent.OutboxID,
				conversation.ConversationID,
				err,
			)
		}
		if err := validateExistingCarry(carried, intent, conversation.ConversationID); err != nil {
			// DO NOTHING matched an existing row that differs from this intent:
			// the same foreign-collision anomaly by another route. Report and
			// continue rather than abort.
			entry.Reason = err.Error()
			report.Uncarryable = append(report.Uncarryable, entry)
			continue
		}
		if disposition == sqlite.EnqueueInserted {
			report.Carried = append(report.Carried, entry)
		}
	}

	return report, nil
}

func validateExistingCarry(
	carried sqlite.OutboxItem,
	intent sqlite.CarryableIntent,
	freshConversationID string,
) error {
	if carried.AccountID != intent.AccountID ||
		carried.ConversationID != freshConversationID ||
		carried.Kind != intent.Kind ||
		carried.IdempotencyKey != intent.IdempotencyKey ||
		carried.PayloadHash != intent.PayloadHash ||
		carried.Operation != intent.Operation ||
		carried.ScheduledForMS != intent.ScheduledForMS {
		return fmt.Errorf(
			"carry pending outbox item %q: idempotency key %q resolved to a different fresh intent",
			intent.OutboxID,
			intent.IdempotencyKey,
		)
	}
	return nil
}

func carryEntryFromIntent(intent sqlite.CarryableIntent) CarryEntry {
	return CarryEntry{
		OutboxID:             intent.OutboxID,
		AccountID:            intent.AccountID,
		ConversationID:       intent.ConversationID,
		RemoteConversationID: intent.RemoteConversationID,
		IdempotencyKey:       intent.IdempotencyKey,
		Kind:                 intent.Kind,
		State:                intent.State,
	}
}

func carryEntryFromReview(intent sqlite.CarryReviewIntent) CarryEntry {
	return CarryEntry{
		OutboxID:             intent.OutboxID,
		AccountID:            intent.AccountID,
		ConversationID:       intent.ConversationID,
		RemoteConversationID: intent.RemoteConversationID,
		IdempotencyKey:       intent.IdempotencyKey,
		Kind:                 intent.Kind,
		State:                intent.State,
	}
}

var _ CarrySource = Endpoint{}
var _ CarryTarget = Endpoint{}
