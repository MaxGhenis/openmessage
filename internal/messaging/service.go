package messaging

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/storage/blob"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

// DefaultMaxMediaBytes is the default hard cap for one outgoing attachment.
const DefaultMaxMediaBytes int64 = 128 << 20

const (
	textOperation       = "send_text"
	mediaOperation      = "send_media"
	reactionOperation   = "reaction"
	readOperation       = "read_receipt"
	defaultLeaseTime    = 30 * time.Second
	defaultFinalizeTime = 5 * time.Second
	defaultRetryDelay   = 5 * time.Second
	defaultPollDelay    = time.Second
	defaultMaxPollDelay = 30 * time.Second
	defaultBatchLimit   = 32
	maxListPending      = 500
	summaryMaxRunes     = 120
	workerOwner         = "message-service"

	// Near-duplicate guard defaults: a text whose body is this similar to one
	// submitted to the same conversation within the window is blocked unless
	// the command carries Force. 0.75 catches the incident shape ("lunch
	// tomorrow…" resent as "lunch today…") while leaving short conversational
	// repeats ("ok" / "ok!") alone.
	defaultDuplicateWindow    = 10 * time.Minute
	defaultDuplicateThreshold = 0.75
	duplicateCandidateLimit   = 8
	duplicateCompareMaxRunes  = 1000
)

// ListPendingQuery selects outbox-tray deliveries in deterministic due order.
type ListPendingQuery struct {
	AccountID      string
	ConversationID string
	Limit          int
}

// PendingDelivery is the application-facing preview of one durable intent
// that remains visible in the outbox tray.
type PendingDelivery struct {
	OutboxID       string
	AccountID      string
	ConversationID string
	Kind           sqlite.OutboxKind
	State          OutboxState
	ScheduledFor   time.Time
	NextAttemptAt  time.Time
	ExpiresAt      time.Time // zero means the intent never expires
	AttemptCount   int64
	CreatedAt      time.Time
	Summary        string
	ErrorClass     string
	ErrorCode      string
}

// MessageService owns message intent submission and durable dispatch. Concrete
// platform selection remains behind bridge.Registry.
type MessageService struct {
	store    *sqlite.Store
	outbox   *sqlite.OutboxRepository
	messages messageRepository
	bridges  bridge.Registry
	blobs    *blob.BlobStore
	clock    Clock
	ids      IDSource

	leaseTime     time.Duration
	retryDelay    time.Duration
	pollDelay     time.Duration
	maxPollDelay  time.Duration
	maxMediaBytes int64
	batchLimit    int

	duplicateWindow    time.Duration
	duplicateThreshold float64

	wake chan struct{}

	changeMu sync.Mutex
	changed  chan struct{}
}

// NewMessageService constructs the message service over an existing SQLite
// store. The caller retains ownership of the store and registry.
func NewMessageService(
	store *sqlite.Store,
	bridges bridge.Registry,
	blobs *blob.BlobStore,
	clock Clock,
	ids IDSource,
) (*MessageService, error) {
	if store == nil {
		return nil, fmt.Errorf("create message service: store is nil")
	}
	if bridges == nil {
		return nil, fmt.Errorf("create message service: bridge registry is nil")
	}
	if clock == nil {
		return nil, fmt.Errorf("create message service: clock is nil")
	}
	if ids == nil {
		return nil, fmt.Errorf("create message service: ID source is nil")
	}
	outbox, err := sqlite.NewOutboxRepository(store, clock.Now)
	if err != nil {
		return nil, fmt.Errorf("create message service outbox: %w", err)
	}
	messages, err := sqlite.NewMessageRepository(store, clock.Now)
	if err != nil {
		return nil, fmt.Errorf("create message service messages: %w", err)
	}
	return &MessageService{
		store:              store,
		outbox:             outbox,
		messages:           messages,
		bridges:            bridges,
		blobs:              blobs,
		clock:              clock,
		ids:                ids,
		leaseTime:          defaultLeaseTime,
		retryDelay:         defaultRetryDelay,
		pollDelay:          defaultPollDelay,
		maxPollDelay:       defaultMaxPollDelay,
		maxMediaBytes:      DefaultMaxMediaBytes,
		batchLimit:         defaultBatchLimit,
		duplicateWindow:    defaultDuplicateWindow,
		duplicateThreshold: defaultDuplicateThreshold,
		wake:               make(chan struct{}, 1),
		changed:            make(chan struct{}),
	}, nil
}

// SendText atomically creates an optimistic outgoing message and its durable
// outbox intent. An identical account-scoped idempotent submission returns the
// original durable identities.
func (s *MessageService) SendText(
	ctx context.Context,
	cmd SendTextCommand,
) (Submission, error) {
	if ctx == nil {
		return Submission{}, fmt.Errorf("send text: context is nil")
	}
	if err := validateSendTextCommand(cmd); err != nil {
		return Submission{}, err
	}

	conversation, err := s.store.GetConversation(cmd.ConversationID)
	if err != nil {
		return Submission{}, fmt.Errorf("send text: read conversation: %w", err)
	}
	if conversation.AccountID != cmd.AccountID {
		return Submission{}, fmt.Errorf(
			"%w: conversation %q belongs to account %q, not %q",
			ErrInvalidCommand,
			cmd.ConversationID,
			conversation.AccountID,
			cmd.AccountID,
		)
	}

	var replyToRemoteID *string
	if cmd.ReplyToMessageID != "" {
		reply, err := s.messages.GetMessage(ctx, cmd.ReplyToMessageID)
		if err != nil {
			return Submission{}, fmt.Errorf("send text: read reply target: %w", err)
		}
		if reply.AccountID != cmd.AccountID || reply.ConversationID != cmd.ConversationID {
			return Submission{}, fmt.Errorf(
				"%w: reply target %q is outside conversation %q",
				ErrInvalidCommand,
				cmd.ReplyToMessageID,
				cmd.ConversationID,
			)
		}
		replyToRemoteID = &reply.RemoteMessageID
	}

	outboxID, localMessageID, requestID, err := s.newSubmissionIDs()
	if err != nil {
		return Submission{}, err
	}
	now := s.clock.Now()
	scheduledFor := cmd.NotBefore
	if scheduledFor.IsZero() {
		scheduledFor = now
	}
	expiresAtMS, err := expiryMilliseconds(cmd.CommonCommand, scheduledFor)
	if err != nil {
		return Submission{}, err
	}
	if err := s.guardNearDuplicateText(ctx, cmd, now); err != nil {
		return Submission{}, err
	}
	payloadHash, err := textPayloadHash(cmd.Body, cmd.ReplyToMessageID)
	if err != nil {
		return Submission{}, fmt.Errorf("send text: hash payload: %w", err)
	}

	item, disposition, err := s.outbox.EnqueueOutgoingMessage(ctx, sqlite.NewOutboxItem{
		OutboxID:           outboxID,
		AccountID:          cmd.AccountID,
		ConversationID:     cmd.ConversationID,
		Kind:               sqlite.OutboxKindText,
		IdempotencyKey:     cmd.IdempotencyKey,
		PayloadHash:        payloadHash,
		Operation:          textOperation,
		LocalMessageID:     localMessageID,
		TransportRequestID: requestID,
		ScheduledFor:       scheduledFor,
		ExpiresAtMS:        expiresAtMS,
	}, sqlite.Message{
		MessageID:       localMessageID,
		ConversationID:  cmd.ConversationID,
		AccountID:       cmd.AccountID,
		RemoteMessageID: requestID,
		Direction:       sqlite.MessageDirectionOutgoing,
		Body:            cmd.Body,
		ReplyToRemoteID: replyToRemoteID,
		State:           sqlite.MessageStateActive,
		OccurredAtMS:    now.UnixMilli(),
	})
	if err != nil {
		return Submission{}, fmt.Errorf("send text: %w", err)
	}

	submission := submissionFromItem(item, disposition)
	if disposition == sqlite.EnqueueInserted {
		s.signalChange()
		s.signalWake()
	}
	return submission, nil
}

// SendMedia stores an attachment before atomically creating its optimistic
// outgoing message, durable outbox intent, and blob metadata reference.
func (s *MessageService) SendMedia(
	ctx context.Context,
	cmd SendMediaCommand,
) (Submission, error) {
	if ctx == nil {
		return Submission{}, fmt.Errorf("send media: context is nil")
	}
	if err := validateSendMediaCommand(&cmd); err != nil {
		return Submission{}, err
	}
	if !s.bridges.Capabilities(cmd.AccountID).MediaSend {
		return Submission{}, ErrUnsupported
	}

	conversation, err := s.store.GetConversation(cmd.ConversationID)
	if err != nil {
		return Submission{}, fmt.Errorf("send media: read conversation: %w", err)
	}
	if conversation.AccountID != cmd.AccountID {
		return Submission{}, fmt.Errorf(
			"%w: conversation %q belongs to account %q, not %q",
			ErrInvalidCommand,
			cmd.ConversationID,
			conversation.AccountID,
			cmd.AccountID,
		)
	}

	var replyToRemoteID *string
	if cmd.ReplyToMessageID != "" {
		reply, err := s.messages.GetMessage(ctx, cmd.ReplyToMessageID)
		if err != nil {
			return Submission{}, fmt.Errorf("send media: read reply target: %w", err)
		}
		if reply.AccountID != cmd.AccountID || reply.ConversationID != cmd.ConversationID {
			return Submission{}, fmt.Errorf(
				"%w: reply target %q is outside conversation %q",
				ErrInvalidCommand,
				cmd.ReplyToMessageID,
				cmd.ConversationID,
			)
		}
		replyToRemoteID = &reply.RemoteMessageID
	}

	if s.blobs == nil {
		return Submission{}, fmt.Errorf("send media: blob store is not configured")
	}
	ref, err := s.blobs.Put(ctx, cmd.Content, cmd.MIME, s.maxMediaBytes)
	if err != nil {
		var tooLarge *blob.ErrTooLarge
		if errors.As(err, &tooLarge) {
			return Submission{}, fmt.Errorf(
				"%w: attachment too large (limit %d MB): %v",
				ErrTooLarge,
				tooLarge.Max/(1<<20),
				tooLarge,
			)
		}
		return Submission{}, fmt.Errorf("send media: store blob: %w", err)
	}
	if ref.Size == 0 {
		// Rejecting after Put is intentional: Content is a stream, so emptiness is
		// only known once read. The residue is a single constant zero-byte blob
		// (the empty-content hash) with no durable outbox/message row referencing
		// it — content-addressed, so all empty attempts converge on that one file,
		// and it is swept by the unreferenced-blob GC.
		return Submission{}, fmt.Errorf("%w: attachment is empty", ErrInvalidCommand)
	}

	outboxID, localMessageID, requestID, err := s.newSubmissionIDs()
	if err != nil {
		return Submission{}, err
	}
	now := s.clock.Now()
	scheduledFor := cmd.NotBefore
	if scheduledFor.IsZero() {
		scheduledFor = now
	}
	expiresAtMS, err := expiryMilliseconds(cmd.CommonCommand, scheduledFor)
	if err != nil {
		return Submission{}, err
	}
	payloadHash, err := mediaPayloadHash(
		ref.Hash,
		ref.Size,
		cmd.MIME,
		cmd.Filename,
		cmd.Caption,
		cmd.ReplyToMessageID,
	)
	if err != nil {
		return Submission{}, fmt.Errorf("send media: hash payload: %w", err)
	}

	item, disposition, err := s.outbox.EnqueueOutgoingMediaMessage(ctx, sqlite.NewOutboxItem{
		OutboxID:           outboxID,
		AccountID:          cmd.AccountID,
		ConversationID:     cmd.ConversationID,
		Kind:               sqlite.OutboxKindMedia,
		IdempotencyKey:     cmd.IdempotencyKey,
		PayloadHash:        payloadHash,
		Operation:          mediaOperation,
		LocalMessageID:     localMessageID,
		TransportRequestID: requestID,
		ScheduledFor:       scheduledFor,
		ExpiresAtMS:        expiresAtMS,
	}, sqlite.Message{
		MessageID:       localMessageID,
		ConversationID:  cmd.ConversationID,
		AccountID:       cmd.AccountID,
		RemoteMessageID: requestID,
		Direction:       sqlite.MessageDirectionOutgoing,
		Body:            cmd.Caption,
		ReplyToRemoteID: replyToRemoteID,
		State:           sqlite.MessageStateActive,
		OccurredAtMS:    now.UnixMilli(),
	}, sqlite.OutboxAttachment{
		BlobHash:  ref.Hash,
		SizeBytes: ref.Size,
		MIME:      cmd.MIME,
		Filename:  cmd.Filename,
	})
	if err != nil {
		return Submission{}, fmt.Errorf("send media: %w", err)
	}

	submission := submissionFromItem(item, disposition)
	if disposition == sqlite.EnqueueInserted {
		s.signalChange()
		s.signalWake()
	}
	return submission, nil
}

// SendReaction creates a durable reaction intent without projecting a local
// message row. An identical account-scoped idempotent submission returns the
// original durable identity.
func (s *MessageService) SendReaction(
	ctx context.Context,
	cmd SendReactionCommand,
) (Submission, error) {
	if ctx == nil {
		return Submission{}, fmt.Errorf("send reaction: context is nil")
	}
	if err := validateSendReactionCommand(&cmd); err != nil {
		return Submission{}, err
	}
	if !s.bridges.Capabilities(cmd.AccountID).Reactions {
		return Submission{}, ErrUnsupported
	}

	conversation, err := s.store.GetConversation(cmd.ConversationID)
	if err != nil {
		return Submission{}, fmt.Errorf("send reaction: read conversation: %w", err)
	}
	if conversation.AccountID != cmd.AccountID {
		return Submission{}, fmt.Errorf(
			"%w: conversation %q belongs to account %q, not %q",
			ErrInvalidCommand,
			cmd.ConversationID,
			conversation.AccountID,
			cmd.AccountID,
		)
	}

	target, err := s.messages.GetMessage(ctx, cmd.TargetMessageID)
	if err != nil {
		return Submission{}, fmt.Errorf("send reaction: read target: %w", err)
	}
	if target.AccountID != cmd.AccountID || target.ConversationID != cmd.ConversationID {
		return Submission{}, fmt.Errorf(
			"%w: reaction target %q is outside conversation %q",
			ErrInvalidCommand,
			cmd.TargetMessageID,
			cmd.ConversationID,
		)
	}
	// Reactions cannot target a deleted message. MarkRead deliberately permits
	// deleted cursor targets because deletion does not undo that the user read it.
	if target.State == sqlite.MessageStateDeleted {
		return Submission{}, fmt.Errorf(
			"%w: reaction target %q is deleted",
			ErrInvalidCommand,
			cmd.TargetMessageID,
		)
	}

	outboxID, _, requestID, err := s.newSubmissionIDs()
	if err != nil {
		return Submission{}, err
	}
	now := s.clock.Now()
	scheduledFor := cmd.NotBefore
	if scheduledFor.IsZero() {
		scheduledFor = now
	}
	payloadHash, err := reactionPayloadHash(cmd.TargetMessageID, cmd.Emoji, cmd.Action)
	if err != nil {
		return Submission{}, fmt.Errorf("send reaction: hash payload: %w", err)
	}

	item, disposition, err := s.outbox.EnqueueReaction(ctx, sqlite.NewOutboxItem{
		OutboxID:           outboxID,
		AccountID:          cmd.AccountID,
		ConversationID:     cmd.ConversationID,
		Kind:               sqlite.OutboxKindReaction,
		IdempotencyKey:     cmd.IdempotencyKey,
		PayloadHash:        payloadHash,
		Operation:          reactionOperation,
		TransportRequestID: requestID,
		ScheduledFor:       scheduledFor,
	}, sqlite.OutboxReaction{
		TargetMessageID: cmd.TargetMessageID,
		Emoji:           cmd.Emoji,
		Action:          string(cmd.Action),
	})
	if err != nil {
		return Submission{}, fmt.Errorf("send reaction: %w", err)
	}

	submission := submissionFromItem(item, disposition)
	if disposition == sqlite.EnqueueInserted {
		s.signalChange()
		s.signalWake()
	}
	return submission, nil
}

// MarkRead atomically records the device cursor and creates its durable
// outbound receipt intent. The cursor remains advanced even if dispatch later
// reaches a terminal failure.
func (s *MessageService) MarkRead(
	ctx context.Context,
	cmd MarkReadCommand,
) (Submission, error) {
	if ctx == nil {
		return Submission{}, fmt.Errorf("mark read: context is nil")
	}
	if err := validateMarkReadCommand(cmd); err != nil {
		return Submission{}, err
	}
	if !s.bridges.Capabilities(cmd.AccountID).ReadReceipts {
		return Submission{}, ErrUnsupported
	}

	conversation, err := s.store.GetConversation(cmd.ConversationID)
	if err != nil {
		return Submission{}, fmt.Errorf("mark read: read conversation: %w", err)
	}
	if conversation.AccountID != cmd.AccountID {
		return Submission{}, fmt.Errorf(
			"%w: conversation %q belongs to account %q, not %q",
			ErrInvalidCommand,
			cmd.ConversationID,
			conversation.AccountID,
			cmd.AccountID,
		)
	}

	target, err := s.messages.GetMessage(ctx, cmd.LastReadMessageID)
	if err != nil {
		return Submission{}, fmt.Errorf("mark read: read cursor target: %w", err)
	}
	if target.AccountID != cmd.AccountID || target.ConversationID != cmd.ConversationID {
		return Submission{}, fmt.Errorf(
			"%w: read cursor target %q is outside conversation %q",
			ErrInvalidCommand,
			cmd.LastReadMessageID,
			cmd.ConversationID,
		)
	}
	// Unlike reactions, a read cursor may name a deleted message: deletion does
	// not regress the user's durable read position.
	device, err := s.store.GetDevice(cmd.DeviceID)
	if err != nil {
		return Submission{}, fmt.Errorf("mark read: read device: %w", err)
	}
	if device.AccountID != cmd.AccountID {
		return Submission{}, fmt.Errorf(
			"%w: device %q belongs to account %q, not %q",
			ErrInvalidCommand,
			cmd.DeviceID,
			device.AccountID,
			cmd.AccountID,
		)
	}

	outboxID, _, requestID, err := s.newSubmissionIDs()
	if err != nil {
		return Submission{}, err
	}
	now := s.clock.Now()
	if cmd.LastReadAt.IsZero() {
		cmd.LastReadAt = now
	}
	scheduledFor := cmd.NotBefore
	if scheduledFor.IsZero() {
		scheduledFor = now
	}
	payloadHash, err := readPayloadHash(cmd.DeviceID, cmd.LastReadMessageID)
	if err != nil {
		return Submission{}, fmt.Errorf("mark read: hash payload: %w", err)
	}
	lastReadMessageID := cmd.LastReadMessageID

	item, disposition, err := s.outbox.EnqueueReadReceipt(ctx, sqlite.NewOutboxItem{
		OutboxID:           outboxID,
		AccountID:          cmd.AccountID,
		ConversationID:     cmd.ConversationID,
		Kind:               sqlite.OutboxKindRead,
		IdempotencyKey:     cmd.IdempotencyKey,
		PayloadHash:        payloadHash,
		Operation:          readOperation,
		TransportRequestID: requestID,
		ScheduledFor:       scheduledFor,
	}, sqlite.OutboxReadReceipt{
		DeviceID:          cmd.DeviceID,
		LastReadMessageID: cmd.LastReadMessageID,
		ReadAtMS:          cmd.LastReadAt.UnixMilli(),
	}, sqlite.ReadCursor{
		AccountID:         cmd.AccountID,
		DeviceID:          cmd.DeviceID,
		ConversationID:    cmd.ConversationID,
		LastReadMessageID: &lastReadMessageID,
		LastReadAtMS:      cmd.LastReadAt.UnixMilli(),
		UpdatedAtMS:       now.UnixMilli(),
	})
	if err != nil {
		return Submission{}, fmt.Errorf("mark read: %w", err)
	}

	submission := submissionFromItem(item, disposition)
	if disposition == sqlite.EnqueueInserted {
		s.signalChange()
		s.signalWake()
	}
	return submission, nil
}

// Get returns the current durable delivery state.
func (s *MessageService) Get(ctx context.Context, outboxID string) (Delivery, error) {
	if ctx == nil {
		return Delivery{}, fmt.Errorf("get delivery: context is nil")
	}
	if strings.TrimSpace(outboxID) == "" {
		return Delivery{}, fmt.Errorf("get delivery: outbox ID is empty")
	}
	item, err := s.outbox.FindByID(ctx, outboxID)
	if err != nil {
		return Delivery{}, fmt.Errorf("get delivery: %w", err)
	}
	return deliveryFromItem(item), nil
}

// ListPending returns every outbox-tray delivery in deterministic storage due
// order. State-specific APIs decide which follow-up actions are safe.
func (s *MessageService) ListPending(
	ctx context.Context,
	q ListPendingQuery,
) ([]PendingDelivery, error) {
	if ctx == nil {
		return nil, fmt.Errorf("list pending deliveries: context is nil")
	}
	if q.Limit <= 0 {
		return nil, fmt.Errorf("list pending deliveries: limit must be positive")
	}
	if q.Limit > maxListPending {
		q.Limit = maxListPending
	}
	rows, err := s.outbox.ListPending(ctx, sqlite.ListPendingParams{
		AccountID:      q.AccountID,
		ConversationID: q.ConversationID,
		Limit:          q.Limit,
	})
	if err != nil {
		return nil, fmt.Errorf("list pending deliveries: %w", err)
	}

	deliveries := make([]PendingDelivery, 0, len(rows))
	for _, row := range rows {
		delivery := PendingDelivery{
			OutboxID:       row.OutboxID,
			AccountID:      row.AccountID,
			ConversationID: row.ConversationID,
			Kind:           row.Kind,
			State:          row.State,
			ScheduledFor:   time.UnixMilli(row.ScheduledForMS),
			AttemptCount:   row.AttemptCount,
			CreatedAt:      time.UnixMilli(row.CreatedAtMS),
			Summary:        pendingSummary(row),
			ErrorClass:     stringValue(row.ErrorClass),
			ErrorCode:      stringValue(row.ErrorCode),
		}
		if row.NextAttemptAtMS != nil {
			delivery.NextAttemptAt = time.UnixMilli(*row.NextAttemptAtMS)
		}
		if row.ExpiresAtMS != nil {
			delivery.ExpiresAt = time.UnixMilli(*row.ExpiresAtMS)
		}
		deliveries = append(deliveries, delivery)
	}
	return deliveries, nil
}

func pendingSummary(row sqlite.PendingRow) string {
	var summary string
	switch row.Kind {
	case sqlite.OutboxKindText:
		summary = stringValue(row.Body)
	case sqlite.OutboxKindMedia:
		summary = stringValue(row.MediaFile)
		if summary == "" {
			summary = "media"
		}
		if caption := stringValue(row.Body); caption != "" {
			summary += ": " + caption
		}
	case sqlite.OutboxKindReaction:
		summary = stringValue(row.Emoji)
	case sqlite.OutboxKindRead:
		return ""
	}
	runes := []rune(summary)
	if len(runes) > summaryMaxRunes {
		return string(runes[:summaryMaxRunes])
	}
	return summary
}

func (s *MessageService) nextPollDelay(ctx context.Context) time.Duration {
	earliest, ok, err := s.outbox.EarliestDue(ctx)
	if err != nil || !ok {
		return s.maxPollDelay
	}
	delay := earliest.Sub(s.clock.Now())
	if delay < s.pollDelay {
		return s.pollDelay
	}
	if delay > s.maxPollDelay {
		return s.maxPollDelay
	}
	return delay
}

// Wait blocks until the intent reaches a user-actionable or terminal state.
func (s *MessageService) Wait(ctx context.Context, outboxID string) (Delivery, error) {
	if ctx == nil {
		return Delivery{}, fmt.Errorf("wait for delivery: context is nil")
	}
	for {
		changed := s.changeChannel()
		delivery, err := s.Get(ctx, outboxID)
		if err != nil || deliverySettled(delivery.State) {
			return delivery, err
		}

		timer := s.clock.NewTimer(s.pollDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return delivery, ctx.Err()
		case <-changed:
			timer.Stop()
		case <-timer.C():
		}
	}
}

// Cancel cancels work that has not crossed the transport boundary.
func (s *MessageService) Cancel(ctx context.Context, outboxID string) (Delivery, error) {
	current, err := s.Get(ctx, outboxID)
	if err != nil {
		return Delivery{}, err
	}
	if current.State != OutboxQueued && current.State != OutboxNotDispatched {
		return current, fmt.Errorf(
			"%w: cannot cancel outbox item %q from %q",
			ErrInvalidState,
			outboxID,
			current.State,
		)
	}
	if err := s.outbox.Cancel(ctx, outboxID); err != nil {
		if errors.Is(err, sqlite.ErrInvalidOutboxState) {
			return current, fmt.Errorf("%w: cancel outbox item %q: %w", ErrInvalidState, outboxID, err)
		}
		return Delivery{}, fmt.Errorf("cancel delivery: %w", err)
	}
	s.signalChange()
	return s.Get(ctx, outboxID)
}

// RetryNotDispatched makes a proven-safe intent immediately due while reusing
// its stable transport request ID.
func (s *MessageService) RetryNotDispatched(
	ctx context.Context,
	outboxID string,
) (Delivery, error) {
	current, err := s.Get(ctx, outboxID)
	if err != nil {
		return Delivery{}, err
	}
	if current.State != OutboxNotDispatched {
		return current, fmt.Errorf(
			"%w: cannot retry outbox item %q from %q",
			ErrInvalidState,
			outboxID,
			current.State,
		)
	}
	if err := s.outbox.RetryNotDispatched(ctx, outboxID); err != nil {
		if errors.Is(err, sqlite.ErrInvalidOutboxState) {
			return current, fmt.Errorf("%w: retry outbox item %q: %w", ErrInvalidState, outboxID, err)
		}
		return Delivery{}, fmt.Errorf("retry delivery: %w", err)
	}
	s.signalChange()
	s.signalWake()
	return s.Get(ctx, outboxID)
}

// SendAgain creates a new text or media intent from the durable payload of an
// uncertain or rejected predecessor. The predecessor remains unchanged. A late
// echo may confirm the predecessor between the state read and the enqueue;
// that is the same accepted outcome as the echo arriving a moment after the
// resend (both delivered) — the user explicitly chose to resend a
// maybe-delivered message, and the two intents stay distinct rows.
func (s *MessageService) SendAgain(
	ctx context.Context,
	outboxID string,
	newIdempotencyKey string,
) (Submission, error) {
	if ctx == nil {
		return Submission{}, fmt.Errorf("send again: context is nil")
	}
	item, err := s.outbox.FindByID(ctx, outboxID)
	if err != nil {
		return Submission{}, fmt.Errorf("send again: read prior outbox item: %w", err)
	}
	if item.State != sqlite.OutboxUncertain && item.State != sqlite.OutboxRejected {
		return Submission{}, fmt.Errorf(
			"%w: cannot send again outbox item %q from %q",
			ErrInvalidState,
			outboxID,
			item.State,
		)
	}
	if item.Kind != sqlite.OutboxKindText && item.Kind != sqlite.OutboxKindMedia {
		return Submission{}, fmt.Errorf(
			"%w: cannot send again outbox item %q of kind %q",
			ErrInvalidState,
			outboxID,
			item.Kind,
		)
	}
	if strings.TrimSpace(newIdempotencyKey) == "" {
		return Submission{}, fmt.Errorf("%w: new idempotency key is empty", ErrInvalidCommand)
	}
	if newIdempotencyKey == item.IdempotencyKey {
		return Submission{}, fmt.Errorf(
			"%w: new idempotency key must differ from the prior key",
			ErrInvalidCommand,
		)
	}
	if item.LocalMessageID == nil || strings.TrimSpace(*item.LocalMessageID) == "" {
		return Submission{}, fmt.Errorf(
			"send again: prior outbox item %q has no local message",
			outboxID,
		)
	}
	message, err := s.messages.GetMessage(ctx, *item.LocalMessageID)
	if err != nil {
		return Submission{}, fmt.Errorf("send again: read prior local message: %w", err)
	}
	if message.State == sqlite.MessageStateDeleted {
		return Submission{}, fmt.Errorf(
			"%w: prior message %q is deleted",
			ErrInvalidCommand,
			message.MessageID,
		)
	}

	newOutboxID, newLocalMessageID, newRequestID, err := s.newSubmissionIDs()
	if err != nil {
		return Submission{}, err
	}
	now := s.clock.Now()
	newItem := sqlite.NewOutboxItem{
		OutboxID:            newOutboxID,
		AccountID:           item.AccountID,
		ConversationID:      item.ConversationID,
		Kind:                item.Kind,
		IdempotencyKey:      newIdempotencyKey,
		LocalMessageID:      newLocalMessageID,
		TransportRequestID:  newRequestID,
		ScheduledFor:        now,
		SendAgainOfOutboxID: item.OutboxID,
	}
	newMessage := sqlite.Message{
		MessageID:       newLocalMessageID,
		ConversationID:  item.ConversationID,
		AccountID:       item.AccountID,
		RemoteMessageID: newRequestID,
		Direction:       sqlite.MessageDirectionOutgoing,
		Body:            message.Body,
		ReplyToRemoteID: message.ReplyToRemoteID,
		State:           sqlite.MessageStateActive,
		OccurredAtMS:    now.UnixMilli(),
	}
	replyToRemoteID := stringValue(message.ReplyToRemoteID)

	var resent sqlite.OutboxItem
	var disposition sqlite.EnqueueDisposition
	switch item.Kind {
	case sqlite.OutboxKindText:
		newItem.Operation = textOperation
		newItem.PayloadHash, err = textPayloadHash(message.Body, replyToRemoteID)
		if err != nil {
			return Submission{}, fmt.Errorf("send again: hash text payload: %w", err)
		}
		resent, disposition, err = s.outbox.EnqueueOutgoingMessage(ctx, newItem, newMessage)
	case sqlite.OutboxKindMedia:
		attachment, attachmentErr := s.outbox.GetOutboxAttachment(ctx, item.OutboxID)
		if attachmentErr != nil {
			return Submission{}, fmt.Errorf("send again: read prior media attachment: %w", attachmentErr)
		}
		newItem.Operation = mediaOperation
		newItem.PayloadHash, err = mediaPayloadHash(
			attachment.BlobHash,
			attachment.SizeBytes,
			attachment.MIME,
			attachment.Filename,
			message.Body,
			replyToRemoteID,
		)
		if err != nil {
			return Submission{}, fmt.Errorf("send again: hash media payload: %w", err)
		}
		resent, disposition, err = s.outbox.EnqueueOutgoingMediaMessage(
			ctx,
			newItem,
			newMessage,
			sqlite.OutboxAttachment{
				BlobHash:  attachment.BlobHash,
				SizeBytes: attachment.SizeBytes,
				MIME:      attachment.MIME,
				Filename:  attachment.Filename,
			},
		)
	}
	if err != nil {
		return Submission{}, fmt.Errorf("send again: enqueue reconstructed intent: %w", err)
	}
	if disposition == sqlite.EnqueueInserted {
		s.signalChange()
		s.signalWake()
	}
	return submissionFromItem(resent, disposition), nil
}

// ObserveTransportEcho correlates an accepted transport result to an existing
// durable intent. Missing and inapplicable echoes are successful no-ops.
// ObserveTransportEcho reconciles on the caller's context deliberately (no
// detached mutation context): echoes are at-least-once and reconciliation is
// idempotent, so a canceled write is simply replayed by the next delivery —
// unlike dispatch finalization, where losing the write loses real state.
func (s *MessageService) ObserveTransportEcho(
	ctx context.Context,
	echo TransportEcho,
) (EchoOutcome, error) {
	if ctx == nil {
		return "", fmt.Errorf("observe transport echo: context is nil")
	}
	checks := []struct {
		name  string
		value string
	}{
		{name: "account ID", value: echo.AccountID},
		{name: "transport request ID", value: echo.TransportRequestID},
		{name: "remote message ID", value: echo.RemoteMessageID},
	}
	for _, check := range checks {
		if strings.TrimSpace(check.value) == "" {
			return "", fmt.Errorf("%w: %s is empty", ErrInvalidCommand, check.name)
		}
	}

	outcome, err := s.outbox.ReconcileConfirm(ctx, sqlite.ReconcileRequest{
		AccountID:          echo.AccountID,
		TransportRequestID: echo.TransportRequestID,
		ResultRemoteID:     echo.RemoteMessageID,
	})
	if err != nil {
		return "", fmt.Errorf("observe transport echo: %w", err)
	}
	switch outcome {
	case sqlite.ReconcileOutcomeNotFound, sqlite.ReconcileOutcomeNoop:
		return outcome, nil
	case sqlite.ReconcileOutcomeReconciled, sqlite.ReconcileOutcomeEnriched:
		s.signalChange()
		return outcome, nil
	default:
		return "", fmt.Errorf("observe transport echo: unexpected reconcile outcome %q", outcome)
	}
}

// RepairStoreFailed re-drives local confirmation for a result the transport
// already accepted. It never sends the intent again.
func (s *MessageService) RepairStoreFailed(
	ctx context.Context,
	outboxID string,
) (Delivery, error) {
	if ctx == nil {
		return Delivery{}, fmt.Errorf("repair store-failed delivery: context is nil")
	}
	if strings.TrimSpace(outboxID) == "" {
		return Delivery{}, fmt.Errorf("repair store-failed delivery: outbox ID is empty")
	}
	item, err := s.outbox.FindByID(ctx, outboxID)
	if err != nil {
		return Delivery{}, fmt.Errorf("repair store-failed delivery: %w", err)
	}
	current := deliveryFromItem(item)
	if item.State != sqlite.OutboxStoreFailed {
		return current, fmt.Errorf(
			"%w: cannot repair outbox item %q from %q",
			ErrInvalidState,
			outboxID,
			item.State,
		)
	}
	if item.ResultRemoteID == nil || strings.TrimSpace(*item.ResultRemoteID) == "" {
		return current, fmt.Errorf(
			"repair store-failed delivery %q: persisted remote result ID is missing",
			outboxID,
		)
	}

	if _, err := s.outbox.ReconcileConfirm(ctx, sqlite.ReconcileRequest{
		AccountID:          item.AccountID,
		TransportRequestID: item.TransportRequestID,
		ResultRemoteID:     *item.ResultRemoteID,
	}); err != nil {
		return current, fmt.Errorf("repair store-failed delivery %q: %w", outboxID, err)
	}
	s.signalChange()
	return s.Get(ctx, outboxID)
}

func (s *MessageService) reconcileStoreFailedDue(ctx context.Context, limit int) (int, error) {
	if ctx == nil {
		return 0, fmt.Errorf("reconcile store-failed deliveries: context is nil")
	}
	if limit <= 0 {
		return 0, fmt.Errorf("reconcile store-failed deliveries: limit must be positive")
	}
	items, err := s.outbox.FindStoreFailedDue(ctx, limit)
	if err != nil {
		return 0, fmt.Errorf("find store-failed deliveries: %w", err)
	}

	reconciled := 0
	for _, item := range items {
		if err := ctx.Err(); err != nil {
			return reconciled, err
		}
		if item.ResultRemoteID == nil || strings.TrimSpace(*item.ResultRemoteID) == "" {
			continue
		}
		outcome, err := s.outbox.ReconcileConfirm(ctx, sqlite.ReconcileRequest{
			AccountID:          item.AccountID,
			TransportRequestID: item.TransportRequestID,
			ResultRemoteID:     *item.ResultRemoteID,
		})
		if err != nil {
			// A row that still cannot be repaired remains store_failed for a
			// later bounded pass or explicit repair.
			continue
		}
		if outcome == sqlite.ReconcileOutcomeReconciled || outcome == sqlite.ReconcileOutcomeEnriched {
			reconciled++
		}
	}
	if reconciled > 0 {
		s.signalChange()
	}
	return reconciled, nil
}

// expiryMilliseconds resolves a command's TTL against its effective schedule.
// The window opens at the later of "now" and NotBefore so a scheduled send is
// never born expired.
func expiryMilliseconds(cmd CommonCommand, scheduledFor time.Time) (int64, error) {
	if cmd.TTL < 0 {
		return 0, fmt.Errorf("%w: TTL is negative", ErrInvalidCommand)
	}
	if cmd.TTL == 0 {
		return 0, nil
	}
	return scheduledFor.Add(cmd.TTL).UnixMilli(), nil
}

// guardNearDuplicateText blocks a text whose body is nearly identical to one
// submitted to the same conversation inside the duplicate window, unless the
// command carries Force. Same-key candidates are skipped: replaying the exact
// send with its original idempotency key is the documented safe retry and is
// resolved by enqueue-level deduplication, not the guard.
func (s *MessageService) guardNearDuplicateText(
	ctx context.Context,
	cmd SendTextCommand,
	now time.Time,
) error {
	if cmd.Force || s.duplicateWindow <= 0 {
		return nil
	}
	sinceMS := now.Add(-s.duplicateWindow).UnixMilli()
	if sinceMS < 1 {
		sinceMS = 1
	}
	recent, err := s.outbox.ListRecentTextIntents(
		ctx,
		cmd.AccountID,
		cmd.ConversationID,
		sinceMS,
		duplicateCandidateLimit,
	)
	if err != nil {
		return fmt.Errorf("send text: check for near-duplicates: %w", err)
	}
	for _, intent := range recent {
		if intent.IdempotencyKey == cmd.IdempotencyKey {
			continue
		}
		if !textsNearDuplicate(cmd.Body, intent.Body, s.duplicateThreshold) {
			continue
		}
		return &DuplicateSendError{
			PriorOutboxID:       intent.OutboxID,
			PriorState:          intent.State,
			PriorIdempotencyKey: intent.IdempotencyKey,
			PriorAgeMS:          now.UnixMilli() - intent.CreatedAtMS,
		}
	}
	return nil
}

// textsNearDuplicate reports whether two message bodies are the same message
// for guard purposes: equal after whitespace/case normalization, or within
// the similarity threshold by normalized Levenshtein distance.
func textsNearDuplicate(a, b string, threshold float64) bool {
	na, nb := normalizeGuardText(a), normalizeGuardText(b)
	if na == "" || nb == "" {
		return false
	}
	if na == nb {
		return true
	}
	ra, rb := []rune(na), []rune(nb)
	if len(ra) > duplicateCompareMaxRunes {
		ra = ra[:duplicateCompareMaxRunes]
	}
	if len(rb) > duplicateCompareMaxRunes {
		rb = rb[:duplicateCompareMaxRunes]
	}
	longest := len(ra)
	if len(rb) > longest {
		longest = len(rb)
	}
	if longest == 0 {
		return false
	}
	distance := levenshtein(ra, rb)
	similarity := 1 - float64(distance)/float64(longest)
	return similarity >= threshold
}

func normalizeGuardText(value string) string {
	return strings.Join(strings.Fields(strings.ToLower(value)), " ")
}

// levenshtein is the classic two-row edit distance over runes.
func levenshtein(a, b []rune) int {
	if len(a) == 0 {
		return len(b)
	}
	if len(b) == 0 {
		return len(a)
	}
	previous := make([]int, len(b)+1)
	current := make([]int, len(b)+1)
	for j := range previous {
		previous[j] = j
	}
	for i := 1; i <= len(a); i++ {
		current[0] = i
		for j := 1; j <= len(b); j++ {
			substitution := previous[j-1]
			if a[i-1] != b[j-1] {
				substitution++
			}
			insertion := current[j-1] + 1
			deletion := previous[j] + 1
			best := substitution
			if insertion < best {
				best = insertion
			}
			if deletion < best {
				best = deletion
			}
			current[j] = best
		}
		previous, current = current, previous
	}
	return previous[len(b)]
}

func (s *MessageService) newSubmissionIDs() (string, string, string, error) {
	values := make([]string, 3)
	for i := range values {
		value, err := s.ids.NewID()
		if err != nil {
			return "", "", "", fmt.Errorf("send message: generate durable ID: %w", err)
		}
		if strings.TrimSpace(value) == "" {
			return "", "", "", fmt.Errorf("send message: ID source returned an empty ID")
		}
		values[i] = value
	}
	return values[0], values[1], values[2], nil
}

func validateSendTextCommand(cmd SendTextCommand) error {
	checks := []struct {
		name  string
		value string
	}{
		{name: "account ID", value: cmd.AccountID},
		{name: "conversation ID", value: cmd.ConversationID},
		{name: "idempotency key", value: cmd.IdempotencyKey},
		{name: "body", value: cmd.Body},
	}
	for _, check := range checks {
		if strings.TrimSpace(check.value) == "" {
			return fmt.Errorf("%w: %s is empty", ErrInvalidCommand, check.name)
		}
	}
	if !cmd.NotBefore.IsZero() && cmd.NotBefore.UnixMilli() <= 0 {
		return fmt.Errorf("%w: scheduled Unix time is not positive", ErrInvalidCommand)
	}
	return nil
}

func validateSendMediaCommand(cmd *SendMediaCommand) error {
	if cmd == nil {
		return fmt.Errorf("%w: media command is nil", ErrInvalidCommand)
	}
	checks := []struct {
		name  string
		value string
	}{
		{name: "account ID", value: cmd.AccountID},
		{name: "conversation ID", value: cmd.ConversationID},
		{name: "idempotency key", value: cmd.IdempotencyKey},
	}
	for _, check := range checks {
		if strings.TrimSpace(check.value) == "" {
			return fmt.Errorf("%w: %s is empty", ErrInvalidCommand, check.name)
		}
	}
	if cmd.Content == nil {
		return fmt.Errorf("%w: content is nil", ErrInvalidCommand)
	}
	cmd.MIME = strings.TrimSpace(cmd.MIME)
	if cmd.MIME == "" {
		cmd.MIME = "application/octet-stream"
	}
	cmd.Filename = strings.TrimSpace(cmd.Filename)
	if len(cmd.Filename) > 512 {
		return fmt.Errorf("%w: filename exceeds 512 bytes", ErrInvalidCommand)
	}
	if strings.ContainsRune(cmd.Filename, '\x00') {
		return fmt.Errorf("%w: filename contains NUL", ErrInvalidCommand)
	}
	if !cmd.NotBefore.IsZero() && cmd.NotBefore.UnixMilli() <= 0 {
		return fmt.Errorf("%w: scheduled Unix time is not positive", ErrInvalidCommand)
	}
	return nil
}

func validateSendReactionCommand(cmd *SendReactionCommand) error {
	if cmd == nil {
		return fmt.Errorf("%w: reaction command is nil", ErrInvalidCommand)
	}
	checks := []struct {
		name  string
		value string
	}{
		{name: "account ID", value: cmd.AccountID},
		{name: "conversation ID", value: cmd.ConversationID},
		{name: "idempotency key", value: cmd.IdempotencyKey},
		{name: "target message ID", value: cmd.TargetMessageID},
	}
	for _, check := range checks {
		if strings.TrimSpace(check.value) == "" {
			return fmt.Errorf("%w: %s is empty", ErrInvalidCommand, check.name)
		}
	}
	cmd.Emoji = strings.TrimSpace(cmd.Emoji)
	if cmd.Emoji == "" {
		return fmt.Errorf("%w: emoji is empty", ErrInvalidCommand)
	}
	if len(cmd.Emoji) > 64 {
		return fmt.Errorf("%w: emoji exceeds 64 bytes", ErrInvalidCommand)
	}
	if cmd.Action == "" {
		cmd.Action = bridge.ReactionAdd
	}
	switch cmd.Action {
	case bridge.ReactionAdd, bridge.ReactionRemove, bridge.ReactionSwitch:
	default:
		return fmt.Errorf("%w: reaction action %q is invalid", ErrInvalidCommand, cmd.Action)
	}
	if !cmd.NotBefore.IsZero() && cmd.NotBefore.UnixMilli() <= 0 {
		return fmt.Errorf("%w: scheduled Unix time is not positive", ErrInvalidCommand)
	}
	return nil
}

func validateMarkReadCommand(cmd MarkReadCommand) error {
	checks := []struct {
		name  string
		value string
	}{
		{name: "account ID", value: cmd.AccountID},
		{name: "conversation ID", value: cmd.ConversationID},
		{name: "idempotency key", value: cmd.IdempotencyKey},
		{name: "device ID", value: cmd.DeviceID},
		{name: "last read message ID", value: cmd.LastReadMessageID},
	}
	for _, check := range checks {
		if strings.TrimSpace(check.value) == "" {
			return fmt.Errorf("%w: %s is empty", ErrInvalidCommand, check.name)
		}
	}
	if !cmd.LastReadAt.IsZero() && cmd.LastReadAt.UnixMilli() <= 0 {
		return fmt.Errorf("%w: last-read Unix time is not positive", ErrInvalidCommand)
	}
	if !cmd.NotBefore.IsZero() && cmd.NotBefore.UnixMilli() <= 0 {
		return fmt.Errorf("%w: scheduled Unix time is not positive", ErrInvalidCommand)
	}
	return nil
}

func textPayloadHash(body, replyToMessageID string) (string, error) {
	payload, err := json.Marshal(struct {
		Body             string `json:"body"`
		ReplyToMessageID string `json:"reply_to_message_id,omitempty"`
	}{Body: body, ReplyToMessageID: replyToMessageID})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}

func mediaPayloadHash(
	blobHash string,
	size int64,
	mime string,
	filename string,
	caption string,
	replyToMessageID string,
) (string, error) {
	payload, err := json.Marshal(struct {
		BlobHash         string `json:"blob_hash"`
		Size             int64  `json:"size"`
		MIME             string `json:"mime"`
		Filename         string `json:"filename"`
		Caption          string `json:"caption,omitempty"`
		ReplyToMessageID string `json:"reply_to_message_id,omitempty"`
	}{blobHash, size, mime, filename, caption, replyToMessageID})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}

func reactionPayloadHash(
	targetMessageID string,
	emoji string,
	action bridge.ReactionAction,
) (string, error) {
	payload, err := json.Marshal(struct {
		TargetMessageID string                `json:"target_message_id"`
		Emoji           string                `json:"emoji"`
		Action          bridge.ReactionAction `json:"action"`
	}{
		TargetMessageID: targetMessageID,
		Emoji:           emoji,
		Action:          action,
	})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}

func readPayloadHash(deviceID, lastReadMessageID string) (string, error) {
	// ReadAt is intentionally excluded: the idempotent intent is the device's
	// cursor position, so a replay at a later wall-clock time remains identical.
	payload, err := json.Marshal(struct {
		DeviceID          string `json:"device_id"`
		LastReadMessageID string `json:"last_read_message_id"`
	}{
		DeviceID:          deviceID,
		LastReadMessageID: lastReadMessageID,
	})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}

func submissionFromItem(item sqlite.OutboxItem, disposition sqlite.EnqueueDisposition) Submission {
	submission := Submission{
		OutboxID:       item.OutboxID,
		LocalMessageID: stringValue(item.LocalMessageID),
		State:          item.State,
		ScheduledFor:   time.UnixMilli(item.ScheduledForMS),
		Deduplicated:   disposition == sqlite.EnqueueExisting,
	}
	if item.ExpiresAtMS != nil {
		submission.ExpiresAt = time.UnixMilli(*item.ExpiresAtMS)
	}
	return submission
}

func deliveryFromItem(item sqlite.OutboxItem) Delivery {
	delivery := Delivery{
		OutboxID:        item.OutboxID,
		AccountID:       item.AccountID,
		ConversationID:  item.ConversationID,
		State:           item.State,
		LocalMessageID:  stringValue(item.LocalMessageID),
		RemoteMessageID: stringValue(item.ResultRemoteID),
		ErrorClass:      stringValue(item.ErrorClass),
		ErrorCode:       stringValue(item.ErrorCode),
	}
	if item.ExpiresAtMS != nil {
		delivery.ExpiresAt = time.UnixMilli(*item.ExpiresAtMS)
	}
	if item.State == sqlite.OutboxUncertain {
		delivery.Warning = "delivery outcome is unknown"
	}
	if delivery.Expired() {
		delivery.Warning = "the send window expired before the message reached the transport; it was NOT sent"
	}
	return delivery
}

func deliverySettled(state OutboxState) bool {
	switch state {
	case OutboxNotDispatched, OutboxUncertain, OutboxConfirmed,
		OutboxStoreFailed, OutboxRejected, OutboxCanceled:
		return true
	default:
		return false
	}
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func (s *MessageService) signalWake() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *MessageService) signalChange() {
	s.changeMu.Lock()
	close(s.changed)
	s.changed = make(chan struct{})
	s.changeMu.Unlock()
}

func (s *MessageService) changeChannel() <-chan struct{} {
	s.changeMu.Lock()
	defer s.changeMu.Unlock()
	return s.changed
}

// Changes returns the current delivery-change broadcast channel. The channel
// closes on the next change; callers must call Changes again after it closes.
func (s *MessageService) Changes() <-chan struct{} {
	return s.changeChannel()
}

func contextError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
