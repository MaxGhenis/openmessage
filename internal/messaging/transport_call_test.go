package messaging

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"strings"
	"sync"
	"testing"
	"testing/quick"
	"time"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

// testTransportTimeout keeps the hard bound short enough for unit tests while
// leaving a scripted call that returns at once far inside it.
const testTransportTimeout = 40 * time.Millisecond

func withTestTransportBounds(service *MessageService) *MessageService {
	service.transportTimeout = testTransportTimeout
	service.mediaTransportTimeout = testTransportTimeout
	service.transportShutdownGrace = 20 * time.Millisecond
	return service
}

// gate blocks scripted transport calls the way libgm's response wait does: it
// ignores the call's context and returns only when the test releases it.
type gate struct {
	once    sync.Once
	release chan struct{}
	entered chan struct{}
}

func newGate() *gate {
	return &gate{release: make(chan struct{}), entered: make(chan struct{}, 64)}
}

func (g *gate) block() {
	g.entered <- struct{}{}
	<-g.release
}

func (g *gate) open() { g.once.Do(func() { close(g.release) }) }

// hangOnCall returns an onSend hook that blocks only the given calls (1-based).
func hangOnCall(g *gate, calls ...int) func() {
	var mu sync.Mutex
	count := 0
	hang := map[int]bool{}
	for _, call := range calls {
		hang[call] = true
	}
	return func() {
		mu.Lock()
		count++
		blocked := hang[count]
		mu.Unlock()
		if blocked {
			g.block()
		}
	}
}

func dispatchWithin(t *testing.T, service *MessageService, ctx context.Context, limit int, bound time.Duration) (int, error) {
	t.Helper()
	type result struct {
		processed int
		err       error
	}
	done := make(chan result, 1)
	go func() {
		processed, err := service.DispatchDue(ctx, limit)
		done <- result{processed, err}
	}()
	select {
	case r := <-done:
		return r.processed, r.err
	case <-time.After(bound):
		t.Fatalf("DispatchDue did not return within %s: a hung transport still blocks the dispatch loop", bound)
		return 0, nil
	}
}

func waitForStillRunning(t *testing.T, service *MessageService, want int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if got := service.DispatchHealth().AbandonedStillRunning; got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("AbandonedStillRunning = %d, want %d", service.DispatchHealth().AbandonedStillRunning, want)
		}
		time.Sleep(time.Millisecond)
	}
}

// The 2026-10-20 incident: a Google text send got no answer, the dispatch loop
// waited on it forever, and a later text sat queued behind it for hours.
func TestHungTextSendBecomesUncertainAndLaterTextStillSends(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	hung := newGate()
	t.Cleanup(hung.open)
	sender := &scriptedTextSender{
		steps: []sendStep{
			{result: bridge.SendResult{RemoteMessageID: "remote-never-answered"}},
			{result: bridge.SendResult{RemoteMessageID: "remote-second"}},
		},
		onSend: hangOnCall(hung, 1),
	}
	registry := newScriptedRegistry("hung-text", sender)
	registry.setAvailable(true)
	service := withTestTransportBounds(newMessagingTestService(t, store, registry, clock))
	first := mustSendText(t, service, SendTextCommand{
		CommonCommand: testCommonCommand("key-hung-first"),
		Body:          "the phone never answers this one",
	})
	second := mustSendText(t, service, SendTextCommand{
		CommonCommand: testCommonCommand("key-queued-second"),
		Body:          "queued behind it",
	})

	processed, err := dispatchWithin(t, service, context.Background(), 4, 5*time.Second)
	if err != nil || processed != 2 {
		t.Fatalf("DispatchDue(hung then queued) = %d, %v; want 2, nil", processed, err)
	}
	firstDelivery := mustDelivery(t, service, first.OutboxID)
	if firstDelivery.State != OutboxUncertain ||
		firstDelivery.ErrorCode != transportTimeoutOperation ||
		firstDelivery.Warning == "" {
		t.Fatalf("hung send delivery = %+v, want uncertain %q with a warning", firstDelivery, transportTimeoutOperation)
	}
	firstRow := mustOutboxItem(t, service, first.OutboxID)
	if firstRow.AttemptCount != 1 || firstRow.LeaseToken != nil || firstRow.ErrorDetail == nil ||
		!strings.Contains(*firstRow.ErrorDetail, "did not answer within") {
		t.Fatalf("hung send row = %+v, want one attempt, released lease, timeout detail", firstRow)
	}
	if got := mustDelivery(t, service, second.OutboxID).State; got != OutboxConfirmed {
		t.Fatalf("queued second state = %q, want confirmed", got)
	}

	health := service.DispatchHealth()
	if health.InFlight != nil || health.AbandonedCalls != 1 || health.AbandonedStillRunning != 1 ||
		health.LastAbandoned == nil || health.LastAbandoned.OutboxID != first.OutboxID ||
		health.LastAbandoned.Operation != textOperation || health.LastAbandoned.Reason != abandonReasonTimeout {
		t.Fatalf("health after abandoning = %+v (last %+v)", health, health.LastAbandoned)
	}

	// Uncertain is final for the dispatcher: the abandoned request may have
	// reached the phone, so nothing sends it again.
	if processed, err := dispatchWithin(t, service, context.Background(), 4, 5*time.Second); err != nil || processed != 0 {
		t.Fatalf("DispatchDue(after abandon) = %d, %v; want 0, nil", processed, err)
	}
	if got := sender.requestCount(); got != 2 {
		t.Fatalf("transport calls = %d, want 2 (the hung send is never resent)", got)
	}

	// The late answer is dropped; the goroutine that waited for it ends.
	hung.open()
	waitForStillRunning(t, service, 0)
	if got := mustDelivery(t, service, first.OutboxID).State; got != OutboxUncertain {
		t.Fatalf("hung send after its late return = %q, want still uncertain", got)
	}
	if health := service.DispatchHealth(); health.AbandonedCalls != 1 {
		t.Fatalf("AbandonedCalls after late return = %d, want 1", health.AbandonedCalls)
	}
}

func TestTransportAnsweringInsideTheBoundIsUnchanged(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	var inFlight *InFlightTransportCall
	var service *MessageService
	sender := &scriptedTextSender{
		steps: []sendStep{{result: bridge.SendResult{RemoteMessageID: "remote-prompt"}}},
		onSend: func() {
			inFlight = service.DispatchHealth().InFlight
		},
	}
	registry := newScriptedRegistry("prompt-text", sender)
	registry.setAvailable(true)
	service = newMessagingTestService(t, store, registry, clock)
	submission := mustSendText(t, service, SendTextCommand{
		CommonCommand: testCommonCommand("key-prompt"),
		Body:          "answered at once",
	})

	if processed, err := dispatchWithin(t, service, context.Background(), 1, 5*time.Second); err != nil || processed != 1 {
		t.Fatalf("DispatchDue(prompt) = %d, %v; want 1, nil", processed, err)
	}
	if got := mustDelivery(t, service, submission.OutboxID); got.State != OutboxConfirmed ||
		got.RemoteMessageID != "remote-prompt" {
		t.Fatalf("prompt delivery = %+v, want confirmed", got)
	}
	if inFlight == nil || inFlight.OutboxID != submission.OutboxID || inFlight.Operation != textOperation ||
		inFlight.AccountID != "account-1" || inFlight.TimeoutMS != defaultTransportCallTimeout.Milliseconds() {
		t.Fatalf("in-flight call seen by the transport = %+v", inFlight)
	}
	if health := service.DispatchHealth(); health.InFlight != nil || health.AbandonedCalls != 0 ||
		health.AbandonedStillRunning != 0 || health.LastAbandoned != nil {
		t.Fatalf("health after a prompt call = %+v", health)
	}
}

func TestShutdownKeepsAContextAwareTransportsOwnClassification(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sender := &contextAwareTextSender{entered: make(chan struct{}, 1)}
	registry := newScriptedRegistry("aware-text", sender)
	registry.setAvailable(true)
	service := withTestTransportBounds(newMessagingTestService(t, store, registry, clock))
	service.transportShutdownGrace = 5 * time.Second
	submission := mustSendText(t, service, SendTextCommand{
		CommonCommand: testCommonCommand("key-aware"),
		Body:          "canceled before it left",
	})
	service.transportTimeout = time.Hour
	go func() {
		<-sender.entered
		cancel()
	}()

	if _, err := dispatchWithin(t, service, ctx, 1, 5*time.Second); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("DispatchDue(canceled) error = %v", err)
	}
	row := mustOutboxItem(t, service, submission.OutboxID)
	if row.State != sqlite.OutboxNotDispatched {
		t.Fatalf("context-aware cancel row = %+v, want the transport's own not_dispatched", row)
	}
	if health := service.DispatchHealth(); health.AbandonedCalls != 0 || health.InFlight != nil {
		t.Fatalf("health after a context-aware cancel = %+v, want nothing abandoned", health)
	}
}

func TestShutdownAbandonsATransportThatIgnoresCancellation(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hung := newGate()
	t.Cleanup(hung.open)
	sender := &scriptedTextSender{onSend: hangOnCall(hung, 1)}
	registry := newScriptedRegistry("ignoring-text", sender)
	registry.setAvailable(true)
	service := withTestTransportBounds(newMessagingTestService(t, store, registry, clock))
	service.transportTimeout = time.Hour
	submission := mustSendText(t, service, SendTextCommand{
		CommonCommand: testCommonCommand("key-ignoring"),
		Body:          "in flight at shutdown",
	})
	go func() {
		<-hung.entered
		cancel()
	}()

	if _, err := dispatchWithin(t, service, ctx, 1, 5*time.Second); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("DispatchDue(shutdown) error = %v", err)
	}
	row := mustOutboxItem(t, service, submission.OutboxID)
	if row.State != sqlite.OutboxUncertain || row.ErrorDetail == nil ||
		!strings.Contains(*row.ErrorDetail, "abandoned at shutdown") {
		t.Fatalf("shutdown row = %+v, want uncertain abandoned at shutdown", row)
	}
	if health := service.DispatchHealth(); health.LastAbandoned == nil ||
		health.LastAbandoned.Reason != abandonReasonShutdown || health.AbandonedStillRunning != 1 {
		t.Fatalf("health after shutdown abandon = %+v", health)
	}
	hung.open()
	waitForStillRunning(t, service, 0)
}

func TestHungReactionIsBoundedAndNotResent(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	identityID := "identity-hung-reaction"
	seedDispatchIdentity(t, store, identityID, "hung-reaction@example.test", clock.Now())
	target := mustProjectDispatchMessage(t, store, clock, sqlite.Message{
		MessageID:        "message-hung-reaction-target",
		ConversationID:   "conversation-1",
		AccountID:        "account-1",
		RemoteMessageID:  "remote-hung-reaction-target",
		SenderIdentityID: &identityID,
		Direction:        sqlite.MessageDirectionIncoming,
		Body:             "heart me",
		State:            sqlite.MessageStateActive,
		OccurredAtMS:     clock.Now().Add(-time.Minute).UnixMilli(),
	})
	hung := newGate()
	t.Cleanup(hung.open)
	sender := &scriptedReactionSender{onSend: hangOnCall(hung, 1)}
	registry := newScriptedRegistry("hung-reaction", &scriptedTextSender{})
	registry.setReactionSender(sender)
	registry.setAvailable(true)
	service := withTestTransportBounds(newMessagingTestService(t, store, registry, clock))
	submission := mustSendDispatchReaction(t, service, SendReactionCommand{
		CommonCommand:   testCommonCommand("hung-reaction"),
		TargetMessageID: target.MessageID,
		Emoji:           "❤️",
		Action:          bridge.ReactionAdd,
	})

	if processed, err := dispatchWithin(t, service, context.Background(), 1, 5*time.Second); err != nil || processed != 1 {
		t.Fatalf("DispatchDue(hung reaction) = %d, %v; want 1, nil", processed, err)
	}
	if got := mustDelivery(t, service, submission.OutboxID); got.State != OutboxUncertain ||
		got.ErrorCode != transportTimeoutOperation {
		t.Fatalf("hung reaction delivery = %+v, want uncertain timeout", got)
	}
	if processed, err := dispatchWithin(t, service, context.Background(), 1, 5*time.Second); err != nil || processed != 0 {
		t.Fatalf("DispatchDue(after hung reaction) = %d, %v; want 0, nil", processed, err)
	}
	if got := sender.requestCount(); got != 1 {
		t.Fatalf("reaction calls = %d, want 1", got)
	}
	if last := service.DispatchHealth().LastAbandoned; last == nil || last.Operation != reactionOperation {
		t.Fatalf("last abandoned = %+v, want the reaction", last)
	}
}

func TestHungReadReceiptIsBounded(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	seedDispatchDevice(t, store, "device-hung-read", clock.Now())
	identityID := "identity-hung-read"
	seedDispatchIdentity(t, store, identityID, "hung-read@example.test", clock.Now())
	target := mustProjectDispatchMessage(t, store, clock, sqlite.Message{
		MessageID:        "message-hung-read-target",
		ConversationID:   "conversation-1",
		AccountID:        "account-1",
		RemoteMessageID:  "remote-hung-read-target",
		SenderIdentityID: &identityID,
		Direction:        sqlite.MessageDirectionIncoming,
		Body:             "read me",
		State:            sqlite.MessageStateActive,
		OccurredAtMS:     clock.Now().Add(-time.Minute).UnixMilli(),
	})
	hung := newGate()
	t.Cleanup(hung.open)
	sender := &scriptedReadReceiptSender{onSend: hangOnCall(hung, 1)}
	registry := newScriptedRegistry("hung-read", &scriptedTextSender{})
	registry.setReadReceiptSender(sender)
	registry.setAvailable(true)
	service := withTestTransportBounds(newMessagingTestService(t, store, registry, clock))
	submission := mustMarkDispatchRead(t, service, MarkReadCommand{
		CommonCommand:     testCommonCommand("hung-read"),
		DeviceID:          "device-hung-read",
		LastReadMessageID: target.MessageID,
		LastReadAt:        clock.Now(),
	})

	if processed, err := dispatchWithin(t, service, context.Background(), 1, 5*time.Second); err != nil || processed != 1 {
		t.Fatalf("DispatchDue(hung read) = %d, %v; want 1, nil", processed, err)
	}
	if got := mustDelivery(t, service, submission.OutboxID); got.State != OutboxUncertain ||
		got.ErrorCode != transportTimeoutOperation {
		t.Fatalf("hung read delivery = %+v, want uncertain timeout", got)
	}
	if last := service.DispatchHealth().LastAbandoned; last == nil || last.Operation != readOperation {
		t.Fatalf("last abandoned = %+v, want the read receipt", last)
	}
}

// slowReadingMediaSender hangs before it reads the attachment, so it reads
// only after the dispatch loop has abandoned the call and moved on.
type slowReadingMediaSender struct {
	hung    *gate
	readErr chan error
	read    chan []byte
}

func (s *slowReadingMediaSender) SendMedia(_ context.Context, request bridge.MediaRequest) (bridge.SendResult, error) {
	s.hung.block()
	content, err := io.ReadAll(request.Reader)
	s.readErr <- err
	s.read <- content
	return bridge.SendResult{RemoteMessageID: "remote-late-media"}, err
}

func TestAbandonedMediaCallKeepsItsAttachmentReadable(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	blobs := newDispatchTestBlobStore(t)
	hung := newGate()
	t.Cleanup(hung.open)
	sender := &slowReadingMediaSender{hung: hung, readErr: make(chan error, 1), read: make(chan []byte, 1)}
	registry := newScriptedRegistry("hung-media", &scriptedTextSender{})
	registry.setMediaSender(sender)
	registry.setAvailable(true)
	service := withTestTransportBounds(newDispatchTestService(t, store, registry, blobs, clock))
	payload := []byte("a photo the phone never acknowledges")
	submission := mustSendDispatchMedia(t, service, SendMediaCommand{
		CommonCommand: testCommonCommand("hung-media"),
		Content:       bytes.NewReader(payload),
		Filename:      "photo.jpg",
		MIME:          "image/jpeg",
	})

	if processed, err := dispatchWithin(t, service, context.Background(), 1, 5*time.Second); err != nil || processed != 1 {
		t.Fatalf("DispatchDue(hung media) = %d, %v; want 1, nil", processed, err)
	}
	if got := mustDelivery(t, service, submission.OutboxID); got.State != OutboxUncertain ||
		got.ErrorCode != transportTimeoutOperation {
		t.Fatalf("hung media delivery = %+v, want uncertain timeout", got)
	}

	// The dispatch loop has returned; the abandoned call now reads its
	// attachment, which must still be open.
	hung.open()
	if err := <-sender.readErr; err != nil {
		t.Fatalf("abandoned media call read error = %v, want the attachment still open", err)
	}
	if got := <-sender.read; !bytes.Equal(got, payload) {
		t.Fatalf("abandoned media call read %q, want %q", got, payload)
	}
	waitForStillRunning(t, service, 0)
}

// contextAwareTextSender returns a not-dispatched error shortly after its
// context is canceled, as a well-behaved transport does once it has unwound.
type contextAwareTextSender struct {
	entered chan struct{}
}

func (s *contextAwareTextSender) SendText(ctx context.Context, _ bridge.TextRequest) (bridge.SendResult, error) {
	s.entered <- struct{}{}
	<-ctx.Done()
	time.Sleep(50 * time.Millisecond)
	return bridge.SendResult{}, bridge.OpError{
		Class:     bridge.FailureTransient,
		Operation: "send_text",
		Dispatch:  bridge.DispatchNotCalled,
		Cause:     ctx.Err(),
	}
}

// transportBehavior is one scripted answer to one transport call.
type transportBehavior int

const (
	behaveConfirm transportBehavior = iota
	behaveNotDispatched
	behaveUncertain
	behaveHang
	behaveCount
)

func (b transportBehavior) String() string {
	return [...]string{"confirm", "not_dispatched", "uncertain", "hang"}[b]
}

// scriptedBehaviorSender answers each call with the next scripted behavior
// and records which transport request each call carried.
type scriptedBehaviorSender struct {
	mu        sync.Mutex
	script    []transportBehavior
	calls     map[string][]transportBehavior
	hung      *gate
	hungCalls int
}

func (s *scriptedBehaviorSender) SendText(_ context.Context, request bridge.TextRequest) (bridge.SendResult, error) {
	s.mu.Lock()
	behavior := behaveConfirm
	if len(s.script) > 0 {
		behavior = s.script[0]
		s.script = s.script[1:]
	}
	s.calls[request.RequestID] = append(s.calls[request.RequestID], behavior)
	if behavior == behaveHang {
		s.hungCalls++
	}
	s.mu.Unlock()
	switch behavior {
	case behaveNotDispatched:
		return bridge.SendResult{}, bridge.OpError{
			Class: bridge.FailureTransient, Operation: "send_text", Dispatch: bridge.DispatchNotCalled,
			Cause: errors.New("scripted: not dispatched"),
		}
	case behaveUncertain:
		return bridge.SendResult{}, bridge.OpError{
			Class: bridge.FailureTransient, Operation: "send_text", Dispatch: bridge.DispatchUncertain,
			Cause: errors.New("scripted: uncertain"),
		}
	case behaveHang:
		<-s.hung.release
		return bridge.SendResult{RemoteMessageID: "remote-too-late-" + request.RequestID}, nil
	default:
		return bridge.SendResult{RemoteMessageID: "remote-" + request.RequestID}, nil
	}
}

// Invariants, for any mix of transport answers including hangs:
//   - liveness: every DispatchDue returns within a bound set by the hard
//     timeout, and every intent leaves queued/dispatching;
//   - no resend after an ambiguous call: an intent's last transport call is
//     its only call that was a hang or an uncertain answer;
//   - each intent's final state follows its last call (confirm -> confirmed,
//     hang or uncertain -> uncertain);
//   - accounting: AbandonedCalls equals the hangs, and once the hung calls
//     return nothing is left running.
func TestPropertyHungTransportsNeverWedgeOrResend(t *testing.T) {
	property := func(seed int64) bool {
		random := rand.New(rand.NewSource(seed))
		intents := 1 + random.Intn(4)
		script := make([]transportBehavior, random.Intn(9))
		for index := range script {
			script[index] = transportBehavior(random.Intn(int(behaveCount)))
		}
		return checkHungTransportProperty(t, seed, intents, script)
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 40}); err != nil {
		t.Fatal(err)
	}
}

func checkHungTransportProperty(t *testing.T, seed int64, intents int, script []transportBehavior) bool {
	t.Helper()
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	hung := newGate()
	defer hung.open()
	sender := &scriptedBehaviorSender{
		script: append([]transportBehavior(nil), script...),
		calls:  map[string][]transportBehavior{},
		hung:   hung,
	}
	registry := newScriptedRegistry("property", sender)
	registry.setAvailable(true)
	service := withTestTransportBounds(newMessagingTestService(t, store, registry, clock))
	submissions := make([]Submission, intents)
	for index := range submissions {
		submissions[index] = mustSendText(t, service, SendTextCommand{
			CommonCommand: testCommonCommand(fmt.Sprintf("property-%d-%d", seed, index)),
			Body:          fmt.Sprintf("intent %d", index),
		})
	}

	fail := func(format string, args ...any) bool {
		t.Logf("seed %d intents %d script %v: "+format, append([]any{seed, intents, script}, args...)...)
		return false
	}
	// Each round dispatches what is due; a not-dispatched answer is retried
	// after the retry delay. len(script)+intents rounds always suffice.
	for round := 0; round <= len(script)+intents; round++ {
		started := time.Now()
		if _, err := service.DispatchDue(context.Background(), 8); err != nil {
			return fail("DispatchDue: %v", err)
		}
		if elapsed := time.Since(started); elapsed > 2*time.Second {
			return fail("DispatchDue took %s", elapsed)
		}
		clock.Advance(defaultRetryDelay + time.Second)
	}

	sender.mu.Lock()
	calls := map[string][]transportBehavior{}
	for requestID, answers := range sender.calls {
		calls[requestID] = append([]transportBehavior(nil), answers...)
	}
	hungCalls := sender.hungCalls
	sender.mu.Unlock()

	for _, submission := range submissions {
		row := mustOutboxItem(t, service, submission.OutboxID)
		answers := calls[row.TransportRequestID]
		if len(answers) == 0 {
			return fail("intent %s was never called (state %s)", submission.OutboxID, row.State)
		}
		for index, answer := range answers[:len(answers)-1] {
			if answer != behaveNotDispatched {
				return fail("intent %s called again after answer %d was %s: %v", submission.OutboxID, index, answer, answers)
			}
		}
		want := sqlite.OutboxConfirmed
		switch answers[len(answers)-1] {
		case behaveHang, behaveUncertain:
			want = sqlite.OutboxUncertain
		case behaveNotDispatched:
			return fail("intent %s still awaiting retry after every round: %v", submission.OutboxID, answers)
		}
		if row.State != want {
			return fail("intent %s state %s after %v, want %s", submission.OutboxID, row.State, answers, want)
		}
	}
	health := service.DispatchHealth()
	if health.AbandonedCalls != int64(hungCalls) || health.InFlight != nil {
		return fail("health %+v, want %d abandoned and nothing in flight", health, hungCalls)
	}
	hung.open()
	deadline := time.Now().Add(5 * time.Second)
	for service.DispatchHealth().AbandonedStillRunning != 0 {
		if time.Now().After(deadline) {
			return fail("abandoned calls still running after release: %+v", service.DispatchHealth())
		}
		time.Sleep(time.Millisecond)
	}
	return true
}
