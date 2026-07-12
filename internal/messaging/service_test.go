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

func TestUnavailableTextStaysQueuedThenConfirms(t *testing.T) {
	for _, platform := range []bridge.Platform{"scripted-alpha", "telegram-like"} {
		t.Run(string(platform), func(t *testing.T) {
			clock := newManualClock(messagingTestTime)
			store := openMessagingTestStore(t, "message.sqlite", clock.Now())
			sender := &scriptedTextSender{steps: []sendStep{{
				result: bridge.SendResult{RemoteMessageID: "remote-1", AcceptedAt: clock.Now()},
			}}}
			registry := newScriptedRegistry(platform, sender)
			registry.setAvailable(false)
			service := newMessagingTestService(t, store, registry, clock)

			submission := mustSendText(t, service, SendTextCommand{
				CommonCommand: testCommonCommand("key-unavailable"),
				Body:          "durable hello",
			})
			if submission.State != OutboxQueued || submission.Deduplicated {
				t.Fatalf("SendText() = %+v, want new queued submission", submission)
			}

			if processed, err := service.DispatchDue(context.Background(), 8); err != nil {
				t.Fatalf("DispatchDue(unavailable): %v", err)
			} else if processed != 1 {
				t.Fatalf("DispatchDue(unavailable) processed = %d, want 1 durable lease", processed)
			}
			row := mustOutboxItem(t, service, submission.OutboxID)
			if row.State != sqlite.OutboxQueued || row.AttemptCount != 0 || row.NextAttemptAtMS == nil {
				t.Fatalf("unavailable outbox row = %+v, want queued/attempt=0/next attempt", row)
			}
			if got := sender.requestCount(); got != 0 {
				t.Fatalf("unavailable bridge send count = %d, want 0", got)
			}

			registry.setAvailable(true)
			if processed, err := service.DispatchDue(context.Background(), 8); err != nil {
				t.Fatalf("DispatchDue(available): %v", err)
			} else if processed != 1 {
				t.Fatalf("DispatchDue(available) processed = %d, want 1", processed)
			}
			delivery := mustDelivery(t, service, submission.OutboxID)
			if delivery.State != OutboxConfirmed || delivery.RemoteMessageID != "remote-1" {
				t.Fatalf("delivery = %+v, want confirmed remote-1", delivery)
			}
			requests := sender.snapshotRequests()
			if len(requests) != 1 || requests[0].Body != "durable hello" ||
				requests[0].Conversation.RemoteID != "remote-conversation" {
				t.Fatalf("transport requests = %+v", requests)
			}
			row = mustOutboxItem(t, service, submission.OutboxID)
			if requests[0].RequestID != row.TransportRequestID || row.AttemptCount != 1 {
				t.Fatalf("request/outbox = %+v / %+v", requests[0], row)
			}
			message, err := service.messages.GetMessage(context.Background(), submission.LocalMessageID)
			if err != nil {
				t.Fatalf("GetMessage(confirmed optimistic row): %v", err)
			}
			if message.RemoteMessageID != "remote-1" {
				t.Fatalf("confirmed message remote ID = %q, want remote-1", message.RemoteMessageID)
			}
		})
	}
}

func TestClassifiedNotDispatchedRetriesWithStableRequestID(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, "message.sqlite", clock.Now())
	sender := &scriptedTextSender{steps: []sendStep{
		{err: bridge.OpError{
			Class:     bridge.FailureTransient,
			Operation: "prepare_send",
			Dispatch:  bridge.DispatchNotCalled,
			Cause:     errors.New("socket unavailable before write"),
		}},
		{result: bridge.SendResult{RemoteMessageID: "remote-retry"}},
	}}
	registry := newScriptedRegistry("scripted", sender)
	registry.setAvailable(true)
	service := newMessagingTestService(t, store, registry, clock)
	submission := mustSendText(t, service, SendTextCommand{
		CommonCommand: testCommonCommand("key-retry"),
		Body:          "retry me safely",
	})

	if _, err := service.DispatchDue(context.Background(), 1); err != nil {
		t.Fatalf("DispatchDue(first): %v", err)
	}
	delivery := mustDelivery(t, service, submission.OutboxID)
	if delivery.State != OutboxNotDispatched || delivery.ErrorClass != string(bridge.FailureTransient) {
		t.Fatalf("first delivery = %+v, want transient not_dispatched", delivery)
	}
	row := mustOutboxItem(t, service, submission.OutboxID)
	if row.AttemptCount != 1 {
		t.Fatalf("first attempt count = %d, want 1", row.AttemptCount)
	}

	retried, err := service.RetryNotDispatched(context.Background(), submission.OutboxID)
	if err != nil {
		t.Fatalf("RetryNotDispatched(): %v", err)
	}
	if retried.State != OutboxQueued {
		t.Fatalf("RetryNotDispatched() = %+v, want queued", retried)
	}
	if _, err := service.DispatchDue(context.Background(), 1); err != nil {
		t.Fatalf("DispatchDue(retry): %v", err)
	}
	if got := mustDelivery(t, service, submission.OutboxID).State; got != OutboxConfirmed {
		t.Fatalf("retry state = %q, want confirmed", got)
	}
	requests := sender.snapshotRequests()
	if len(requests) != 2 || requests[0].RequestID == "" || requests[0].RequestID != requests[1].RequestID {
		t.Fatalf("retry request IDs = %+v, want same stable nonempty ID", requests)
	}
}

func TestPostCallTimeoutIsUncertainAndSendAgainLinksNewIntent(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, "message.sqlite", clock.Now())
	sender := &scriptedTextSender{steps: []sendStep{
		{err: bridge.OpError{
			Class:     bridge.FailureTransient,
			Operation: "send_text",
			Dispatch:  bridge.DispatchUncertain,
			Cause:     context.DeadlineExceeded,
		}},
		{result: bridge.SendResult{RemoteMessageID: "remote-send-again"}},
	}}
	registry := newScriptedRegistry("synthetic-new-platform", sender)
	registry.setAvailable(true)
	service := newMessagingTestService(t, store, registry, clock)
	first := mustSendText(t, service, SendTextCommand{
		CommonCommand: testCommonCommand("key-timeout"),
		Body:          "ambiguous text",
	})

	if _, err := service.DispatchDue(context.Background(), 4); err != nil {
		t.Fatalf("DispatchDue(timeout): %v", err)
	}
	uncertain := mustDelivery(t, service, first.OutboxID)
	if uncertain.State != OutboxUncertain || uncertain.Warning == "" {
		t.Fatalf("timeout delivery = %+v, want uncertain warning", uncertain)
	}
	if processed, err := service.DispatchDue(context.Background(), 4); err != nil {
		t.Fatalf("DispatchDue(after uncertain): %v", err)
	} else if processed != 0 {
		t.Fatalf("DispatchDue(after uncertain) processed = %d, want 0", processed)
	}
	if got := sender.requestCount(); got != 1 {
		t.Fatalf("uncertain auto-retry send count = %d, want 1", got)
	}

	second, err := service.SendAgain(context.Background(), first.OutboxID, "key-send-again")
	if err != nil {
		t.Fatalf("SendAgain(): %v", err)
	}
	if second.OutboxID == first.OutboxID || second.LocalMessageID == first.LocalMessageID {
		t.Fatalf("SendAgain() = %+v, want distinct intent/message from %+v", second, first)
	}
	secondRow := mustOutboxItem(t, service, second.OutboxID)
	if secondRow.SendAgainOfOutboxID == nil || *secondRow.SendAgainOfOutboxID != first.OutboxID {
		t.Fatalf("SendAgain link = %+v, want %q", secondRow.SendAgainOfOutboxID, first.OutboxID)
	}
	if _, err := service.DispatchDue(context.Background(), 4); err != nil {
		t.Fatalf("DispatchDue(send again): %v", err)
	}
	if got := mustDelivery(t, service, second.OutboxID).State; got != OutboxConfirmed {
		t.Fatalf("SendAgain state = %q, want confirmed", got)
	}
	requests := sender.snapshotRequests()
	if len(requests) != 2 || requests[0].RequestID == requests[1].RequestID {
		t.Fatalf("SendAgain request IDs = %+v, want distinct IDs", requests)
	}
	if got := mustDelivery(t, service, first.OutboxID).State; got != OutboxUncertain {
		t.Fatalf("prior intent state = %q, want uncertain", got)
	}
}

func TestQueuedTextSurvivesServiceAndStoreRestart(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	path := filepath.Join(t.TempDir(), "restart.sqlite")
	store, err := sqlite.Open(path)
	if err != nil {
		t.Fatalf("sqlite.Open(first): %v", err)
	}
	seedMessagingStore(t, store, clock.Now())
	registry := newScriptedRegistry("restart-fake", &scriptedTextSender{})
	registry.setAvailable(false)
	firstService := newMessagingTestService(t, store, registry, clock)
	submission := mustSendText(t, firstService, SendTextCommand{
		CommonCommand: testCommonCommand("key-restart"),
		Body:          "survive restart",
	})
	if err := store.Close(); err != nil {
		t.Fatalf("Close(first store): %v", err)
	}

	store, err = sqlite.Open(path)
	if err != nil {
		t.Fatalf("sqlite.Open(second): %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	sender := &scriptedTextSender{steps: []sendStep{{
		result: bridge.SendResult{RemoteMessageID: "remote-after-restart"},
	}}}
	registry = newScriptedRegistry("different-fake-platform", sender)
	registry.setAvailable(true)
	secondService := newMessagingTestService(t, store, registry, clock)

	before := mustDelivery(t, secondService, submission.OutboxID)
	if before.State != OutboxQueued || before.LocalMessageID != submission.LocalMessageID {
		t.Fatalf("reopened delivery = %+v, want original queued IDs", before)
	}
	if _, err := secondService.DispatchDue(context.Background(), 4); err != nil {
		t.Fatalf("DispatchDue(after restart): %v", err)
	}
	if got := mustDelivery(t, secondService, submission.OutboxID); got.State != OutboxConfirmed ||
		got.RemoteMessageID != "remote-after-restart" {
		t.Fatalf("delivery after restart = %+v", got)
	}
	requests := sender.snapshotRequests()
	if len(requests) != 1 || requests[0].Body != "survive restart" {
		t.Fatalf("request after restart = %+v", requests)
	}
}

func TestDuplicateSendTextReturnsExistingSubmissionAndConflictsOnMismatch(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, "message.sqlite", clock.Now())
	registry := newScriptedRegistry("fake", &scriptedTextSender{})
	service := newMessagingTestService(t, store, registry, clock)
	cmd := SendTextCommand{CommonCommand: testCommonCommand("same-key"), Body: "same body"}

	first := mustSendText(t, service, cmd)
	second := mustSendText(t, service, cmd)
	if first.Deduplicated || !second.Deduplicated {
		t.Fatalf("dedup flags first=%v second=%v, want false/true", first.Deduplicated, second.Deduplicated)
	}
	if first.OutboxID != second.OutboxID || first.LocalMessageID != second.LocalMessageID ||
		!first.ScheduledFor.Equal(second.ScheduledFor) {
		t.Fatalf("duplicate submissions differ: first=%+v second=%+v", first, second)
	}

	cmd.Body = "changed body"
	if _, err := service.SendText(context.Background(), cmd); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("SendText(mismatched duplicate) error = %v, want ErrIdempotencyConflict", err)
	}
}

func TestWaitObservesCancellation(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, "message.sqlite", clock.Now())
	registry := newScriptedRegistry("fake", &scriptedTextSender{})
	service := newMessagingTestService(t, store, registry, clock)
	cmd := SendTextCommand{
		CommonCommand: testCommonCommand("key-cancel"),
		Body:          "cancel before send",
	}
	cmd.NotBefore = clock.Now().Add(time.Hour)
	submission := mustSendText(t, service, cmd)

	waitResult := make(chan Delivery, 1)
	waitError := make(chan error, 1)
	go func() {
		delivery, err := service.Wait(context.Background(), submission.OutboxID)
		waitResult <- delivery
		waitError <- err
	}()
	canceled, err := service.Cancel(context.Background(), submission.OutboxID)
	if err != nil {
		t.Fatalf("Cancel(): %v", err)
	}
	if canceled.State != OutboxCanceled {
		t.Fatalf("Cancel() = %+v, want canceled", canceled)
	}
	select {
	case delivery := <-waitResult:
		if err := <-waitError; err != nil || delivery.State != OutboxCanceled {
			t.Fatalf("Wait() = %+v, %v; want canceled, nil", delivery, err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Wait() did not observe cancellation")
	}
}

func TestRunRecoversExpiredLeaseBeforeDispatch(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, "message.sqlite", clock.Now())
	sender := &scriptedTextSender{
		steps: []sendStep{{result: bridge.SendResult{RemoteMessageID: "remote-recovered"}}},
	}
	registry := newScriptedRegistry("recovery-fake", sender)
	registry.setAvailable(true)
	firstService := newMessagingTestService(t, store, registry, clock)
	submission := mustSendText(t, firstService, SendTextCommand{
		CommonCommand: testCommonCommand("key-recovery"),
		Body:          "recover me",
	})
	leases, err := firstService.outbox.LeaseDue(context.Background(), sqlite.LeaseRequest{
		Owner: "crashed-worker", Now: clock.Now(), Duration: time.Second, Limit: 1,
	})
	if err != nil || len(leases) != 1 {
		t.Fatalf("LeaseDue(crashed worker) = %+v, %v", leases, err)
	}
	clock.Advance(2 * time.Second)

	restarted := newMessagingTestService(t, store, registry, clock)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- restarted.Run(ctx) }()
	waitCtx, stopWaiting := context.WithTimeout(context.Background(), 2*time.Second)
	delivery, waitErr := restarted.Wait(waitCtx, submission.OutboxID)
	stopWaiting()
	if waitErr != nil || delivery.State != OutboxConfirmed {
		cancel()
		t.Fatalf("Wait(recovered delivery) = %+v, %v; want confirmed", delivery, waitErr)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	if got := mustDelivery(t, restarted, submission.OutboxID).State; got != OutboxConfirmed {
		t.Fatalf("recovered delivery state = %q, want confirmed", got)
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
	if len(s.steps) > 0 {
		step = s.steps[0]
		s.steps = s.steps[1:]
	}
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
	acquires  int
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
	state := bridge.StateStopped
	if r.available {
		state = bridge.StateOnline
	}
	return bridge.Snapshot{
		AccountID:  accountID,
		Platform:   r.platform,
		State:      state,
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
	r.acquires++
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
	mu     sync.Mutex
	now    time.Time
	timers map[*manualTimer]time.Time
}

func newManualClock(now time.Time) *manualClock {
	return &manualClock{now: now, timers: make(map[*manualTimer]time.Time)}
}

func (c *manualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *manualClock) NewTimer(delay time.Duration) Timer {
	timer := &manualTimer{clock: c, ticks: make(chan time.Time, 1)}
	c.mu.Lock()
	c.timers[timer] = c.now.Add(delay)
	c.mu.Unlock()
	return timer
}

func (c *manualClock) Advance(delay time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(delay)
	now := c.now
	var due []*manualTimer
	for timer, deadline := range c.timers {
		if !deadline.After(now) {
			delete(c.timers, timer)
			due = append(due, timer)
		}
	}
	c.mu.Unlock()
	for _, timer := range due {
		timer.mu.Lock()
		if !timer.stopped {
			timer.ticks <- now
		}
		timer.mu.Unlock()
	}
}

type manualTimer struct {
	mu      sync.Mutex
	clock   *manualClock
	ticks   chan time.Time
	stopped bool
}

func (t *manualTimer) C() <-chan time.Time { return t.ticks }

func (t *manualTimer) Stop() bool {
	t.mu.Lock()
	if t.stopped {
		t.mu.Unlock()
		return false
	}
	t.stopped = true
	t.mu.Unlock()
	t.clock.mu.Lock()
	_, active := t.clock.timers[t]
	delete(t.clock.timers, t)
	t.clock.mu.Unlock()
	return active
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

func openMessagingTestStore(t *testing.T, filename string, now time.Time) *sqlite.Store {
	t.Helper()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), filename))
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

func mustSendText(t *testing.T, service *MessageService, cmd SendTextCommand) Submission {
	t.Helper()
	submission, err := service.SendText(context.Background(), cmd)
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
