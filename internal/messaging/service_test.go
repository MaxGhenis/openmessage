package messaging

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

var messagingTestTime = time.Date(2026, time.July, 12, 12, 0, 0, 0, time.UTC)

func TestUnavailableTextReturnsToQueuedWithoutAttempt(t *testing.T) {
	for _, platform := range []bridge.Platform{"scripted-alpha", "future-platform"} {
		t.Run(string(platform), func(t *testing.T) {
			clock := newManualClock(messagingTestTime)
			store := openMessagingTestStore(t, clock.Now())
			sender := &scriptedTextSender{steps: []sendStep{{
				result: bridge.SendResult{RemoteMessageID: "remote-available"},
			}}}
			registry := newScriptedRegistry(platform, sender)
			service := newMessagingTestService(t, store, registry, clock)
			submission := mustSendText(t, service, SendTextCommand{
				CommonCommand: testCommonCommand("key-unavailable"),
				Body:          "durable hello",
			})

			processed, err := service.DispatchDue(context.Background(), 8)
			if err != nil || processed != 1 {
				t.Fatalf("DispatchDue(unavailable) = %d, %v; want 1, nil", processed, err)
			}
			row := mustOutboxItem(t, service, submission.OutboxID)
			if row.State != sqlite.OutboxQueued || row.AttemptCount != 0 || row.NextAttemptAtMS != nil {
				t.Fatalf("unavailable row = %+v, want queued/attempt=0/no next-attempt", row)
			}
			if got := sender.requestCount(); got != 0 {
				t.Fatalf("unavailable send count = %d, want 0", got)
			}

			registry.setAvailable(true)
			processed, err = service.DispatchDue(context.Background(), 8)
			if err != nil || processed != 1 {
				t.Fatalf("DispatchDue(available) = %d, %v; want 1, nil", processed, err)
			}
			if got := mustDelivery(t, service, submission.OutboxID); got.State != OutboxConfirmed ||
				got.RemoteMessageID != "remote-available" {
				t.Fatalf("available delivery = %+v, want confirmed", got)
			}
		})
	}
}

func TestAvailableTextDispatchesOnceAndConfirms(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	sender := &scriptedTextSender{steps: []sendStep{{
		result: bridge.SendResult{RemoteMessageID: "remote-1", AcceptedAt: clock.Now()},
	}}}
	registry := newScriptedRegistry("not-hard-coded", sender)
	registry.setAvailable(true)
	service := newMessagingTestService(t, store, registry, clock)
	submission := mustSendText(t, service, SendTextCommand{
		CommonCommand: testCommonCommand("key-confirm"),
		Body:          "hello once",
	})

	processed, err := service.DispatchDue(context.Background(), 4)
	if err != nil || processed != 1 {
		t.Fatalf("DispatchDue() = %d, %v; want 1, nil", processed, err)
	}
	processed, err = service.DispatchDue(context.Background(), 4)
	if err != nil || processed != 0 {
		t.Fatalf("DispatchDue(after confirm) = %d, %v; want 0, nil", processed, err)
	}
	requests := sender.snapshotRequests()
	if len(requests) != 1 {
		t.Fatalf("requests = %+v, want one", requests)
	}
	row := mustOutboxItem(t, service, submission.OutboxID)
	if row.State != sqlite.OutboxConfirmed || row.AttemptCount != 1 ||
		requests[0].RequestID != row.TransportRequestID ||
		requests[0].Conversation.RemoteID != "remote-conversation" ||
		requests[0].Body != "hello once" {
		t.Fatalf("request/row = %+v / %+v", requests[0], row)
	}
}

func TestRetryableNotDispatchedReusesTransportRequestID(t *testing.T) {
	classes := []bridge.FailureClass{
		bridge.FailureTransient,
		bridge.FailureRateLimited,
		bridge.FailureCredentialsExpired,
	}
	for _, class := range classes {
		class := class
		t.Run(string(class), func(t *testing.T) {
			clock := newManualClock(messagingTestTime)
			store := openMessagingTestStore(t, clock.Now())
			cause := error(errors.New("failed before remote dispatch"))
			if class == bridge.FailureTransient {
				// An explicit clean not-dispatched result remains safe even when
				// the adapter's preflight cause is a deadline.
				cause = context.DeadlineExceeded
			}
			sender := &scriptedTextSender{steps: []sendStep{
				{err: bridge.OpError{
					Class:     class,
					Operation: "prepare_send",
					Dispatch:  bridge.DispatchNotCalled,
					Cause:     cause,
				}},
				{result: bridge.SendResult{RemoteMessageID: "remote-retry"}},
			}}
			registry := newScriptedRegistry("retry-platform", sender)
			registry.setAvailable(true)
			service := newMessagingTestService(t, store, registry, clock)
			submission := mustSendText(t, service, SendTextCommand{
				CommonCommand: testCommonCommand("key-retry-" + string(class)),
				Body:          "retry safely",
			})

			if processed, err := service.DispatchDue(context.Background(), 1); err != nil || processed != 1 {
				t.Fatalf("DispatchDue(first) = %d, %v", processed, err)
			}
			first := mustDelivery(t, service, submission.OutboxID)
			if first.State != OutboxNotDispatched || first.ErrorClass != string(class) {
				t.Fatalf("first delivery = %+v, want %q not_dispatched", first, class)
			}
			row := mustOutboxItem(t, service, submission.OutboxID)
			if row.AttemptCount != 1 {
				t.Fatalf("attempt count = %d, want 1", row.AttemptCount)
			}

			retried, err := service.RetryNotDispatched(context.Background(), submission.OutboxID)
			if err != nil || retried.State != OutboxNotDispatched {
				t.Fatalf("RetryNotDispatched() = %+v, %v; want due not_dispatched", retried, err)
			}
			if processed, err := service.DispatchDue(context.Background(), 1); err != nil || processed != 1 {
				t.Fatalf("DispatchDue(retry) = %d, %v", processed, err)
			}
			if got := mustDelivery(t, service, submission.OutboxID).State; got != OutboxConfirmed {
				t.Fatalf("retry state = %q, want confirmed", got)
			}
			requests := sender.snapshotRequests()
			if len(requests) != 2 || requests[0].RequestID == "" ||
				requests[0].RequestID != requests[1].RequestID {
				t.Fatalf("request IDs = %+v, want same stable ID", requests)
			}
		})
	}
}

func TestContextCanceledDuringTransportStillRecordsUncertain(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	ctx, cancel := context.WithCancel(context.Background())
	sender := &scriptedTextSender{
		steps:  []sendStep{{err: context.Canceled}},
		onSend: cancel,
	}
	registry := newScriptedRegistry("cancel-during-send", sender)
	registry.setAvailable(true)
	service := newMessagingTestService(t, store, registry, clock)
	submission := mustSendText(t, service, SendTextCommand{
		CommonCommand: testCommonCommand("key-cancel-during-send"),
		Body:          "record my ambiguous cancellation",
	})

	if processed, err := service.DispatchDue(ctx, 1); err != nil || processed != 1 {
		t.Fatalf("DispatchDue(canceled send) = %d, %v; want 1, nil", processed, err)
	}
	row := mustOutboxItem(t, service, submission.OutboxID)
	if row.State != sqlite.OutboxUncertain || row.AttemptCount != 1 || row.LeaseToken != nil {
		t.Fatalf("canceled transport row = %+v, want uncertain finalized row", row)
	}
}

func TestDispatchDueRecoversLaterLeasesThatExpireWithinBatch(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	var advanceOnce sync.Once
	sender := &scriptedTextSender{
		steps: []sendStep{
			{result: bridge.SendResult{RemoteMessageID: "remote-slow-first"}},
			{result: bridge.SendResult{RemoteMessageID: "remote-second"}},
		},
		onSend: func() {
			advanceOnce.Do(func() { clock.Advance(defaultLeaseTime + time.Second) })
		},
	}
	registry := newScriptedRegistry("batch-expiry", sender)
	registry.setAvailable(true)
	service := newMessagingTestService(t, store, registry, clock)
	first := mustSendText(t, service, SendTextCommand{
		CommonCommand: testCommonCommand("key-slow-first"),
		Body:          "first",
	})
	second := mustSendText(t, service, SendTextCommand{
		CommonCommand: testCommonCommand("key-expired-second"),
		Body:          "second",
	})

	if processed, err := service.DispatchDue(context.Background(), 2); err != nil || processed != 1 {
		t.Fatalf("DispatchDue(expiring batch) = %d, %v; want 1, nil", processed, err)
	}
	if got := mustDelivery(t, service, first.OutboxID).State; got != OutboxConfirmed {
		t.Fatalf("first state = %q, want confirmed", got)
	}
	secondRow := mustOutboxItem(t, service, second.OutboxID)
	if secondRow.State != sqlite.OutboxNotDispatched || secondRow.AttemptCount != 0 || secondRow.LeaseToken != nil {
		t.Fatalf("expired later lease = %+v, want recoverable not_dispatched", secondRow)
	}
	if processed, err := service.DispatchDue(context.Background(), 1); err != nil || processed != 1 {
		t.Fatalf("DispatchDue(recovered second) = %d, %v; want 1, nil", processed, err)
	}
	if got := mustDelivery(t, service, second.OutboxID).State; got != OutboxConfirmed {
		t.Fatalf("second state = %q, want confirmed", got)
	}
}

func TestPostCallTimeoutBecomesUncertainAndIsNotRedispatched(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	sender := &scriptedTextSender{steps: []sendStep{{err: bridge.OpError{
		Class:     bridge.FailureTransient,
		Operation: "send_text",
		Dispatch:  bridge.DispatchUncertain,
		Cause:     context.DeadlineExceeded,
	}}}}
	registry := newScriptedRegistry("timeout-platform", sender)
	registry.setAvailable(true)
	service := newMessagingTestService(t, store, registry, clock)
	submission := mustSendText(t, service, SendTextCommand{
		CommonCommand: testCommonCommand("key-timeout"),
		Body:          "ambiguous text",
	})

	if processed, err := service.DispatchDue(context.Background(), 4); err != nil || processed != 1 {
		t.Fatalf("DispatchDue(timeout) = %d, %v", processed, err)
	}
	if got := mustDelivery(t, service, submission.OutboxID); got.State != OutboxUncertain || got.Warning == "" {
		t.Fatalf("timeout delivery = %+v, want uncertain warning", got)
	}
	if processed, err := service.DispatchDue(context.Background(), 4); err != nil || processed != 0 {
		t.Fatalf("DispatchDue(after timeout) = %d, %v; want 0, nil", processed, err)
	}
	if got := sender.requestCount(); got != 1 {
		t.Fatalf("timeout send count = %d, want 1", got)
	}
}

func TestTerminalFailuresAreRejected(t *testing.T) {
	classes := []bridge.FailureClass{
		bridge.FailureUnpaired,
		bridge.FailureReauthRequired,
		bridge.FailureUpgradeRequired,
		bridge.FailureMisconfigured,
		bridge.FailureUnsupported,
	}
	for _, class := range classes {
		class := class
		t.Run(string(class), func(t *testing.T) {
			clock := newManualClock(messagingTestTime)
			store := openMessagingTestStore(t, clock.Now())
			sender := &scriptedTextSender{steps: []sendStep{{err: bridge.OpError{
				Class:     class,
				Operation: "send_text",
				Dispatch:  bridge.DispatchNotCalled,
				Cause:     errors.New("terminal failure"),
			}}}}
			registry := newScriptedRegistry("terminal-platform", sender)
			registry.setAvailable(true)
			service := newMessagingTestService(t, store, registry, clock)
			submission := mustSendText(t, service, SendTextCommand{
				CommonCommand: testCommonCommand("key-terminal-" + string(class)),
				Body:          "cannot send",
			})

			if processed, err := service.DispatchDue(context.Background(), 1); err != nil || processed != 1 {
				t.Fatalf("DispatchDue() = %d, %v", processed, err)
			}
			delivery := mustDelivery(t, service, submission.OutboxID)
			if delivery.State != OutboxRejected || delivery.ErrorClass != string(class) {
				t.Fatalf("delivery = %+v, want rejected %q", delivery, class)
			}
			if processed, err := service.DispatchDue(context.Background(), 1); err != nil || processed != 0 {
				t.Fatalf("DispatchDue(after reject) = %d, %v", processed, err)
			}
		})
	}
}

func TestNewServiceOverSameStoreDispatchesQueuedText(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	unavailable := newScriptedRegistry("before-restart", &scriptedTextSender{})
	first := newMessagingTestService(t, store, unavailable, clock)
	submission := mustSendText(t, first, SendTextCommand{
		CommonCommand: testCommonCommand("key-restart"),
		Body:          "survive service restart",
	})

	sender := &scriptedTextSender{steps: []sendStep{{
		result: bridge.SendResult{RemoteMessageID: "remote-after-restart"},
	}}}
	available := newScriptedRegistry("after-restart", sender)
	available.setAvailable(true)
	restarted := newMessagingTestService(t, store, available, clock)

	if before := mustDelivery(t, restarted, submission.OutboxID); before.State != OutboxQueued ||
		before.LocalMessageID != submission.LocalMessageID {
		t.Fatalf("restarted delivery = %+v, want original queued IDs", before)
	}
	if processed, err := restarted.DispatchDue(context.Background(), 4); err != nil || processed != 1 {
		t.Fatalf("DispatchDue(restarted) = %d, %v", processed, err)
	}
	if after := mustDelivery(t, restarted, submission.OutboxID); after.State != OutboxConfirmed ||
		after.RemoteMessageID != "remote-after-restart" {
		t.Fatalf("delivery after restart = %+v", after)
	}
	requests := sender.snapshotRequests()
	if len(requests) != 1 || requests[0].Body != "survive service restart" {
		t.Fatalf("request after restart = %+v", requests)
	}
}

func TestDuplicateSendTextReturnsSameSubmissionAndDispatchesOnce(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	sender := &scriptedTextSender{steps: []sendStep{{
		result: bridge.SendResult{RemoteMessageID: "remote-deduplicated"},
	}}}
	registry := newScriptedRegistry("duplicate-platform", sender)
	registry.setAvailable(true)
	service := newMessagingTestService(t, store, registry, clock)
	command := SendTextCommand{
		CommonCommand: testCommonCommand("same-key"),
		Body:          "same body",
	}

	first := mustSendText(t, service, command)
	second := mustSendText(t, service, command)
	if first != second {
		t.Fatalf("duplicate submissions differ: first=%+v second=%+v", first, second)
	}
	if processed, err := service.DispatchDue(context.Background(), 4); err != nil || processed != 1 {
		t.Fatalf("DispatchDue() = %d, %v", processed, err)
	}
	if got := sender.requestCount(); got != 1 {
		t.Fatalf("duplicate send count = %d, want 1", got)
	}

	command.Body = "changed body"
	if _, err := service.SendText(context.Background(), command); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("SendText(mismatch) error = %v, want ErrIdempotencyConflict", err)
	}
}

func TestCancelAndDeferredAPIs(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	service := newMessagingTestService(
		t,
		store,
		newScriptedRegistry("cancel-platform", &scriptedTextSender{}),
		clock,
	)
	command := SendTextCommand{
		CommonCommand: testCommonCommand("key-cancel"),
		Body:          "cancel me",
	}
	command.NotBefore = clock.Now().Add(time.Hour)
	submission := mustSendText(t, service, command)
	if _, err := service.RetryNotDispatched(context.Background(), submission.OutboxID); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("RetryNotDispatched(queued) error = %v, want ErrInvalidState", err)
	}

	canceled, err := service.Cancel(context.Background(), submission.OutboxID)
	if err != nil || canceled.State != OutboxCanceled {
		t.Fatalf("Cancel() = %+v, %v", canceled, err)
	}
	if processed, err := service.DispatchDue(context.Background(), 1); err != nil || processed != 0 {
		t.Fatalf("DispatchDue(canceled) = %d, %v", processed, err)
	}
	if _, err := service.SendAgain(context.Background(), submission.OutboxID, "new-key"); !errors.Is(err, ErrNotImplemented) {
		t.Fatalf("SendAgain() error = %v, want ErrNotImplemented", err)
	}
	if err := service.ObserveTransportEcho(context.Background(), TransportEcho{}); !errors.Is(err, ErrNotImplemented) {
		t.Fatalf("ObserveTransportEcho() error = %v, want ErrNotImplemented", err)
	}
}

type sendStep struct {
	result bridge.SendResult
	err    error
}

type scriptedTextSender struct {
	mu       sync.Mutex
	steps    []sendStep
	requests []bridge.TextRequest
	onSend   func()
}

func (s *scriptedTextSender) SendText(
	_ context.Context,
	request bridge.TextRequest,
) (bridge.SendResult, error) {
	s.mu.Lock()
	s.requests = append(s.requests, request)
	var step sendStep
	if len(s.steps) == 0 {
		s.mu.Unlock()
		if s.onSend != nil {
			s.onSend()
		}
		return bridge.SendResult{}, nil
	}
	step = s.steps[0]
	s.steps = s.steps[1:]
	onSend := s.onSend
	s.mu.Unlock()
	if onSend != nil {
		onSend()
	}
	return step.result, step.err
}

func (s *scriptedTextSender) requestCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.requests)
}

func (s *scriptedTextSender) snapshotRequests() []bridge.TextRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]bridge.TextRequest(nil), s.requests...)
}

type scriptedRegistry struct {
	mu        sync.Mutex
	platform  bridge.Platform
	sender    bridge.TextSender
	available bool
}

func newScriptedRegistry(platform bridge.Platform, sender bridge.TextSender) *scriptedRegistry {
	return &scriptedRegistry{platform: platform, sender: sender}
}

func (r *scriptedRegistry) setAvailable(available bool) {
	r.mu.Lock()
	r.available = available
	r.mu.Unlock()
}

func (r *scriptedRegistry) Snapshot(accountID string) (bridge.Snapshot, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if accountID != "account-1" {
		return bridge.Snapshot{}, false
	}
	return bridge.Snapshot{
		AccountID:  accountID,
		Platform:   r.platform,
		Generation: 7,
	}, true
}

func (r *scriptedRegistry) Acquire(
	ctx context.Context,
	accountID string,
	capability bridge.Capability,
) (*bridge.DispatchLease, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if accountID != "account-1" || !r.available {
		return nil, fmt.Errorf("%w: %s", bridge.ErrAccountNotRegistered, accountID)
	}
	if capability != bridge.CapabilityTextSend || r.sender == nil {
		return nil, bridge.ErrCapabilityUnavailable
	}
	return &bridge.DispatchLease{
		AccountID:  accountID,
		Platform:   r.platform,
		Generation: 7,
		Ctx:        ctx,
		Text:       r.sender,
	}, nil
}

func (r *scriptedRegistry) Capabilities(accountID string) bridge.CapabilitySet {
	r.mu.Lock()
	defer r.mu.Unlock()
	return bridge.CapabilitySet{TextSend: accountID == "account-1" && r.sender != nil}
}

type sequentialIDs struct {
	mu   sync.Mutex
	next int
}

func (s *sequentialIDs) NewID() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next++
	return fmt.Sprintf("id-%03d", s.next), nil
}

type manualClock struct {
	mu  sync.Mutex
	now time.Time
}

func newManualClock(now time.Time) *manualClock { return &manualClock{now: now} }

func (c *manualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *manualClock) Advance(delay time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(delay)
	c.mu.Unlock()
}

func (c *manualClock) NewTimer(delay time.Duration) Timer {
	return systemTimer{Timer: time.NewTimer(delay)}
}

func newMessagingTestService(
	t *testing.T,
	store *sqlite.Store,
	registry bridge.Registry,
	clock Clock,
) *MessageService {
	t.Helper()
	service, err := NewMessageService(store, registry, clock, &sequentialIDs{})
	if err != nil {
		t.Fatalf("NewMessageService(): %v", err)
	}
	return service
}

func openMessagingTestStore(t *testing.T, now time.Time) *sqlite.Store {
	t.Helper()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "message.sqlite"))
	if err != nil {
		t.Fatalf("sqlite.Open(): %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	seedMessagingStore(t, store, now)
	return store
}

func seedMessagingStore(t *testing.T, store *sqlite.Store, now time.Time) {
	t.Helper()
	nowMS := now.UnixMilli()
	if err := store.UpsertAccount(sqlite.Account{
		AccountID:   "account-1",
		BridgeKey:   "scripted",
		DisplayName: "Scripted account",
		Mode:        sqlite.AccountModeLive,
		Enabled:     true,
		ConfigJSON:  `{}`,
		CreatedAtMS: nowMS,
		UpdatedAtMS: nowMS,
	}); err != nil {
		t.Fatalf("UpsertAccount(): %v", err)
	}
	if err := store.UpsertConversation(sqlite.Conversation{
		ConversationID:       "conversation-1",
		AccountID:            "account-1",
		RemoteConversationID: "remote-conversation",
		Kind:                 sqlite.ConversationKindDirect,
		Title:                "Test conversation",
		NotificationMode:     sqlite.NotificationModeAll,
		MetadataJSON:         `{}`,
		CreatedAtMS:          nowMS,
		UpdatedAtMS:          nowMS,
	}); err != nil {
		t.Fatalf("UpsertConversation(): %v", err)
	}
}

func testCommonCommand(key string) CommonCommand {
	return CommonCommand{
		AccountID:      "account-1",
		ConversationID: "conversation-1",
		IdempotencyKey: key,
	}
}

func mustSendText(t *testing.T, service *MessageService, command SendTextCommand) Submission {
	t.Helper()
	submission, err := service.SendText(context.Background(), command)
	if err != nil {
		t.Fatalf("SendText(): %v", err)
	}
	return submission
}

func mustDelivery(t *testing.T, service *MessageService, outboxID string) Delivery {
	t.Helper()
	delivery, err := service.Get(context.Background(), outboxID)
	if err != nil {
		t.Fatalf("Get(%q): %v", outboxID, err)
	}
	return delivery
}

func mustOutboxItem(t *testing.T, service *MessageService, outboxID string) sqlite.OutboxItem {
	t.Helper()
	item, err := service.outbox.FindByID(context.Background(), outboxID)
	if err != nil {
		t.Fatalf("FindByID(%q): %v", outboxID, err)
	}
	return item
}
