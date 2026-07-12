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
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

const (
	textOperation     = "send_text"
	defaultLeaseTime  = 30 * time.Second
	defaultRetryDelay = 5 * time.Second
	defaultPollDelay  = time.Second
	defaultBatchLimit = 32
	workerOwner       = "message-service"
)

// MessageService owns text intent submission and durable dispatch. It has no
// knowledge of concrete platforms; bridge.Registry is its transport boundary.
type MessageService struct {
	store    *sqlite.Store
	outbox   *sqlite.OutboxRepository
	messages messageRepository
	bridges  bridge.Registry
	clock    Clock
	ids      IDSource

	leaseTime  time.Duration
	retryDelay time.Duration
	pollDelay  time.Duration
	batchLimit int

	wake chan struct{}

	changeMu sync.Mutex
	changed  chan struct{}
}

// NewMessageService constructs the text service over an existing SQLite
// store. The caller retains ownership of the store and registry.
func NewMessageService(
	store *sqlite.Store,
	bridges bridge.Registry,
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
		store:      store,
		outbox:     outbox,
		messages:   messages,
		bridges:    bridges,
		clock:      clock,
		ids:        ids,
		leaseTime:  defaultLeaseTime,
		retryDelay: defaultRetryDelay,
		pollDelay:  defaultPollDelay,
		batchLimit: defaultBatchLimit,
		wake:       make(chan struct{}, 1),
		changed:    make(chan struct{}),
	}, nil
}

// SendText atomically creates an optimistic outgoing message and its durable
// outbox intent. Identical account-scoped idempotent submissions return the
// original durable identities.
func (s *MessageService) SendText(
	ctx context.Context,
	cmd SendTextCommand,
) (Submission, error) {
	return s.sendText(ctx, cmd, "")
}

func (s *MessageService) sendText(
	ctx context.Context,
	cmd SendTextCommand,
	sendAgainOf string,
) (Submission, error) {
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
		OutboxID:            outboxID,
		AccountID:           cmd.AccountID,
		ConversationID:      cmd.ConversationID,
		Kind:                sqlite.OutboxKindText,
		IdempotencyKey:      cmd.IdempotencyKey,
		PayloadHash:         payloadHash,
		Operation:           textOperation,
		LocalMessageID:      localMessageID,
		TransportRequestID:  requestID,
		SendAgainOfOutboxID: sendAgainOf,
		ScheduledFor:        scheduledFor,
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

	submission := submissionFromItem(item, disposition == sqlite.EnqueueExisting)
	if disposition == sqlite.EnqueueInserted {
		s.signalChange()
		s.signalWake()
	}
	return submission, nil
}

// Get returns the current durable delivery state.
func (s *MessageService) Get(ctx context.Context, outboxID string) (Delivery, error) {
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
	if current.State == OutboxQueued {
		s.signalWake()
		return current, nil
	}
	if current.State != OutboxNotDispatched {
		return current, fmt.Errorf(
			"%w: cannot retry outbox item %q from %q",
			ErrInvalidState,
			outboxID,
			current.State,
		)
	}
	if err := s.outbox.RetryNotDispatched(ctx, outboxID, s.clock.Now()); err != nil {
		return Delivery{}, fmt.Errorf("retry delivery: %w", err)
	}
	s.signalChange()
	s.signalWake()
	return s.Get(ctx, outboxID)
}

// SendAgain creates a distinct linked intent for an ambiguous or rejected
// delivery. It never makes the prior intent retryable.
func (s *MessageService) SendAgain(
	ctx context.Context,
	outboxID string,
	newIdempotencyKey string,
) (Submission, error) {
	item, err := s.outbox.FindByID(ctx, outboxID)
	if err != nil {
		return Submission{}, fmt.Errorf("send again: %w", err)
	}
	if item.State != sqlite.OutboxUncertain && item.State != sqlite.OutboxRejected {
		return Submission{}, fmt.Errorf(
			"%w: cannot send outbox item %q again from %q",
			ErrInvalidState,
			outboxID,
			item.State,
		)
	}
	if item.Kind != sqlite.OutboxKindText || item.LocalMessageID == nil {
		return Submission{}, fmt.Errorf("send again: %w: prior intent is not text", ErrInvalidState)
	}
	message, err := s.messages.GetMessage(ctx, *item.LocalMessageID)
	if err != nil {
		return Submission{}, fmt.Errorf("send again: read prior message: %w", err)
	}

	cmd := SendTextCommand{
		CommonCommand: CommonCommand{
			AccountID:      item.AccountID,
			ConversationID: item.ConversationID,
			IdempotencyKey: newIdempotencyKey,
		},
		Body: message.Body,
	}
	return s.sendText(ctx, cmd, item.OutboxID)
}

// ObserveTransportEcho reserves the M1 correlation seam. Full state repair is
// intentionally deferred to M5; M1 validates that the durable request exists.
func (s *MessageService) ObserveTransportEcho(
	ctx context.Context,
	echo TransportEcho,
) error {
	if strings.TrimSpace(echo.AccountID) == "" ||
		strings.TrimSpace(echo.TransportRequestID) == "" {
		return fmt.Errorf("observe transport echo: %w: account and request IDs are required", ErrInvalidCommand)
	}
	if _, err := s.outbox.FindByTransportRequestID(
		ctx,
		echo.AccountID,
		echo.TransportRequestID,
	); err != nil {
		return fmt.Errorf("observe transport echo: %w", err)
	}
	return nil
}

func (s *MessageService) newSubmissionIDs() (string, string, string, error) {
	values := make([]string, 3)
	for i := range values {
		value, err := s.ids.NewID()
		if err != nil {
			return "", "", "", fmt.Errorf("send text: generate durable ID: %w", err)
		}
		if strings.TrimSpace(value) == "" {
			return "", "", "", fmt.Errorf("send text: ID source returned an empty ID")
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

func submissionFromItem(item sqlite.OutboxItem, deduplicated bool) Submission {
	return Submission{
		OutboxID:       item.OutboxID,
		LocalMessageID: stringValue(item.LocalMessageID),
		State:          item.State,
		Deduplicated:   deduplicated,
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
		delivery.Warning = "delivery outcome is unknown; use SendAgain to create a new intent"
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
