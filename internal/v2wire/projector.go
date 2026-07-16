package v2wire

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/messaging"
	"github.com/maxghenis/openmessage/internal/storage/blob"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

const (
	projectorInitialBatchLimit = 128
	projectorFallbackInterval  = 5 * time.Second
	projectorReplayWindow      = 24 * time.Hour
)

// EventPublisher is the cycle-free subset of web.EventBroker used by the
// projector. internal/web imports v2wire for its handlers, so v2wire cannot
// import web solely to name the concrete broker.
type EventPublisher interface {
	PublishMessages(conversationID string)
	PublishConversations()
}

// Projector makes confirmed v2 sends visible to the still-legacy-reading UI.
// Blobs is retained in the wiring shape because v2blob references are owned by
// that store, though projection itself needs only the attachment hash metadata.
type Projector struct {
	V2Store *sqlite.Store
	Outbox  *sqlite.OutboxRepository
	Legacy  *db.Store
	Blobs   *blob.BlobStore
	Events  EventPublisher
	Service *messaging.MessageService
	Logger  zerolog.Logger
	Now     func() time.Time
}

type projectorCursor struct {
	updatedAtMS   int64
	processedRows map[projectorRowKey]struct{}
}

type projectorRowKey struct {
	updatedAtMS int64
	outboxID    string
}

func newProjectorCursor(updatedAtMS int64) projectorCursor {
	return projectorCursor{
		updatedAtMS:   updatedAtMS,
		processedRows: make(map[projectorRowKey]struct{}),
	}
}

// Run scans immediately, then wakes on service state transitions with a
// five-second fallback. Projection errors are retried because the legacy
// upsert is idempotent; a single malformed or temporarily blocked row must not
// permanently stop visibility for later confirmations.
func (p *Projector) Run(ctx context.Context) error {
	if p == nil {
		return errors.New("run v2 projector: projector is nil")
	}
	if ctx == nil {
		return errors.New("run v2 projector: context is nil")
	}
	if p.V2Store == nil {
		return errors.New("run v2 projector: v2 store is nil")
	}
	if p.Outbox == nil {
		return errors.New("run v2 projector: outbox repository is nil")
	}
	if p.Legacy == nil {
		return errors.New("run v2 projector: legacy store is nil")
	}
	if p.Service == nil {
		return errors.New("run v2 projector: message service is nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	now := p.Now
	if now == nil {
		now = time.Now
	}
	cursor := newProjectorCursor(now().Add(-projectorReplayWindow).UnixMilli())
	ticker := time.NewTicker(projectorFallbackInterval)
	defer ticker.Stop()

	for {
		// Capture the broadcast channel before scanning so a change concurrent
		// with the query remains observable after the scan finishes.
		changes := p.Service.Changes()
		if err := p.projectConfirmedSince(ctx, &cursor); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			p.Logger.Error().Err(err).Msg("V2 legacy visibility projection failed")
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changes:
		case <-ticker.C:
		}
	}
}

func (p *Projector) projectConfirmedSince(ctx context.Context, cursor *projectorCursor) error {
	if cursor == nil {
		return errors.New("project confirmed sends: cursor is nil")
	}
	rows, err := p.listConfirmedIncludingCursorTies(ctx, cursor)
	if err != nil {
		return err
	}
	repository, err := sqlite.NewMessageRepository(p.V2Store, time.Now)
	if err != nil {
		return fmt.Errorf("project confirmed sends: create message repository: %w", err)
	}
	maxSeenMS := cursor.updatedAtMS
	var (
		batchErr         error
		earliestFailedMS int64
		hasFailure       bool
	)
	for _, row := range rows {
		if row.UpdatedAtMS > maxSeenMS {
			maxSeenMS = row.UpdatedAtMS
		}
		if cursor.alreadyProcessed(row) {
			continue
		}
		if row.State != sqlite.OutboxConfirmed {
			// ListConfirmedSince is authoritative, but keep the write-side fence
			// explicit: no queued/uncertain/rejected intent may reach legacy.
			cursor.markProcessed(row)
			continue
		}
		switch row.Kind {
		case sqlite.OutboxKindText, sqlite.OutboxKindMedia:
			if err := p.projectMessageRow(ctx, repository, row); err != nil {
				batchErr = errors.Join(batchErr, err)
				if !hasFailure || row.UpdatedAtMS < earliestFailedMS {
					earliestFailedMS = row.UpdatedAtMS
					hasFailure = true
				}
				continue
			}
		default:
			// Reactions and outbound read receipts deliberately stay outside the
			// Wave-3 visibility bridge.
		}
		cursor.markProcessed(row)
	}
	if hasFailure {
		// Keep the inclusive query watermark at the oldest failed row so it is
		// retried. Successfully projected later rows remain in processedRows and
		// therefore do not republish on every fallback scan.
		cursor.advanceTo(earliestFailedMS)
	} else {
		cursor.advanceTo(maxSeenMS)
	}
	return batchErr
}

// listConfirmedIncludingCursorTies turns the storage API's strict millisecond
// watermark into a lossless in-memory cursor. It queries from one millisecond
// before the cursor and remembers processed outbox IDs at and after that
// timestamp (later IDs are retained while an earlier malformed row is retried).
// If the result fills the limit, it doubles and requeries before processing;
// this prevents a batch boundary from hiding rows that share updated_at_ms. A
// later row at that same millisecond is picked up by the next inclusive scan.
func (p *Projector) listConfirmedIncludingCursorTies(
	ctx context.Context,
	cursor *projectorCursor,
) ([]sqlite.PendingRow, error) {
	sinceMS := cursor.updatedAtMS
	if sinceMS > 0 {
		sinceMS--
	}
	limit := projectorInitialBatchLimit
	for {
		rows, err := p.Outbox.ListConfirmedSince(ctx, sinceMS, limit)
		if err != nil {
			return nil, fmt.Errorf("project confirmed sends: list outbox: %w", err)
		}
		if len(rows) < limit {
			return rows, nil
		}
		maxInt := int(^uint(0) >> 1)
		if limit > maxInt/2 {
			return nil, errors.New("project confirmed sends: result exceeds addressable batch size")
		}
		limit *= 2
	}
}

func (p *Projector) projectMessageRow(
	ctx context.Context,
	repository *sqlite.MessageRepository,
	row sqlite.PendingRow,
) error {
	if row.LocalMessageID == nil || strings.TrimSpace(*row.LocalMessageID) == "" {
		return fmt.Errorf("project confirmed outbox item %q: local message id is missing", row.OutboxID)
	}
	message, err := repository.GetMessage(ctx, *row.LocalMessageID)
	if err != nil {
		return fmt.Errorf("project confirmed outbox item %q: load v2 message: %w", row.OutboxID, err)
	}
	if message.AccountID != row.AccountID || message.ConversationID != row.ConversationID {
		return fmt.Errorf(
			"project confirmed outbox item %q: v2 message belongs to account %q conversation %q",
			row.OutboxID,
			message.AccountID,
			message.ConversationID,
		)
	}

	legacyMessage, err := legacyProjection(row, message)
	if err != nil {
		return err
	}
	if row.Kind == sqlite.OutboxKindMedia {
		attachment, err := p.Outbox.GetOutboxAttachment(ctx, row.OutboxID)
		if err != nil {
			return fmt.Errorf("project confirmed media outbox item %q: load attachment: %w", row.OutboxID, err)
		}
		legacyMessage.MediaID = "v2blob:" + attachment.BlobHash
		legacyMessage.MimeType = attachment.MIME
	}

	if row.AccountID == googleAccountID {
		// The Google receive path does not persist a correlation from a
		// permanent message ID back to TmpID. Therefore an echo that beat this
		// insert cannot be detected reliably from legacy rows; do not invent a
		// body/time heuristic just to emit the documented residual-race WARN.
		// Correct TmpID identity preserves the normal delete-swap reconciliation.
	}
	if err := p.Legacy.RecordOutgoingMessage(legacyMessage, ""); err != nil {
		return fmt.Errorf("project confirmed outbox item %q into legacy store: %w", row.OutboxID, err)
	}
	if p.Events != nil {
		p.Events.PublishMessages(row.ConversationID)
		p.Events.PublishConversations()
	}
	return nil
}

func legacyProjection(row sqlite.PendingRow, message sqlite.Message) (*db.Message, error) {
	resultRemoteID := ""
	if row.ResultRemoteID != nil {
		resultRemoteID = strings.TrimSpace(*row.ResultRemoteID)
	}
	projected := &db.Message{
		ConversationID: row.ConversationID,
		Body:           message.Body,
		TimestampMS:    row.UpdatedAtMS,
		IsFromMe:       true,
		ReplyToID:      legacyReplyID(row.AccountID, message.ReplyToRemoteID),
	}
	switch row.AccountID {
	case googleAccountID:
		projected.MessageID = row.TransportRequestID
		projected.Status = "OUTGOING_SENDING"
	case whatsappAccountID:
		if resultRemoteID == "" {
			return nil, fmt.Errorf("project confirmed outbox item %q: WhatsApp remote id is missing", row.OutboxID)
		}
		projected.MessageID = "whatsapp:" + resultRemoteID
		projected.Status = "sent"
		projected.SourcePlatform = "whatsapp"
		projected.SourceID = resultRemoteID
	case signalAccountID:
		if resultRemoteID == "" {
			return nil, fmt.Errorf("project confirmed outbox item %q: Signal remote id is missing", row.OutboxID)
		}
		projected.MessageID = "signal:" + resultRemoteID
		projected.Status = "sent"
		projected.SourcePlatform = "signal"
		projected.SourceID = resultRemoteID
	default:
		return nil, fmt.Errorf(
			"project confirmed outbox item %q: account %q is not projectable",
			row.OutboxID,
			row.AccountID,
		)
	}
	return projected, nil
}

func legacyReplyID(accountID string, replyToRemoteID *string) string {
	if replyToRemoteID == nil {
		return ""
	}
	remoteID := strings.TrimSpace(*replyToRemoteID)
	if remoteID == "" {
		return ""
	}
	switch accountID {
	case whatsappAccountID:
		if !strings.HasPrefix(remoteID, "whatsapp:") {
			return "whatsapp:" + remoteID
		}
	case signalAccountID:
		if !strings.HasPrefix(remoteID, "signal:") {
			return "signal:" + remoteID
		}
	}
	return remoteID
}

func (c *projectorCursor) alreadyProcessed(row sqlite.PendingRow) bool {
	if row.UpdatedAtMS < c.updatedAtMS {
		return true
	}
	_, ok := c.processedRows[projectorRowKey{
		updatedAtMS: row.UpdatedAtMS,
		outboxID:    row.OutboxID,
	}]
	return ok
}

func (c *projectorCursor) markProcessed(row sqlite.PendingRow) {
	if c.processedRows == nil {
		c.processedRows = make(map[projectorRowKey]struct{})
	}
	c.processedRows[projectorRowKey{
		updatedAtMS: row.UpdatedAtMS,
		outboxID:    row.OutboxID,
	}] = struct{}{}
}

func (c *projectorCursor) advanceTo(updatedAtMS int64) {
	if updatedAtMS < c.updatedAtMS {
		return
	}
	c.updatedAtMS = updatedAtMS
	for key := range c.processedRows {
		if key.updatedAtMS < updatedAtMS {
			delete(c.processedRows, key)
		}
	}
}

func (c *projectorCursor) processedCountAt(updatedAtMS int64) int {
	count := 0
	for key := range c.processedRows {
		if key.updatedAtMS == updatedAtMS {
			count++
		}
	}
	return count
}
