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
	defaultLeaseTime    = 30 * time.Second
	defaultFinalizeTime = 5 * time.Second
	defaultRetryDelay   = 5 * time.Second
	defaultPollDelay    = time.Second
	defaultBatchLimit   = 32
	workerOwner         = "message-service"
)

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
	maxMediaBytes int64
	batchLimit    int

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
		store:         store,
		outbox:        outbox,
		messages:      messages,
		bridges:       bridges,
		blobs:         blobs,
		clock:         clock,
		ids:           ids,
		leaseTime:     defaultLeaseTime,
		retryDelay:    defaultRetryDelay,
		pollDelay:     defaultPollDelay,
		maxMediaBytes: DefaultMaxMediaBytes,
		batchLimit:    defaultBatchLimit,
		wake:          make(chan struct{}, 1),
		changed:       make(chan struct{}),
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

	submission := submissionFromItem(item)
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
				"%w: attachment too large (limit %d MB)",
				ErrInvalidCommand,
				tooLarge.Max/(1<<20),
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

	submission := submissionFromItem(item)
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

// SendAgain is reserved for M5, where the new intent can be durably linked to
// the ambiguous or rejected prior intent.
func (s *MessageService) SendAgain(
	context.Context,
	string,
	string,
) (Submission, error) {
	return Submission{}, fmt.Errorf("send again: %w", ErrNotImplemented)
}

// ObserveTransportEcho is reserved for the M5 reconciliation path.
func (s *MessageService) ObserveTransportEcho(context.Context, TransportEcho) error {
	return fmt.Errorf("observe transport echo: %w", ErrNotImplemented)
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

func submissionFromItem(item sqlite.OutboxItem) Submission {
	return Submission{
		OutboxID:       item.OutboxID,
		LocalMessageID: stringValue(item.LocalMessageID),
		State:          item.State,
		ScheduledFor:   time.UnixMilli(item.ScheduledForMS),
	}
}

func deliveryFromItem(item sqlite.OutboxItem) Delivery {
	delivery := Delivery{
		OutboxID:        item.OutboxID,
		State:           item.State,
		LocalMessageID:  stringValue(item.LocalMessageID),
		RemoteMessageID: stringValue(item.ResultRemoteID),
		ErrorClass:      stringValue(item.ErrorClass),
		ErrorCode:       stringValue(item.ErrorCode),
	}
	if item.State == sqlite.OutboxUncertain {
		delivery.Warning = "delivery outcome is unknown"
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

func contextError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
