package messaging

// The send window enforced at the transport boundary. LeaseDue refuses
// expired rows, but DispatchDue handles a leased batch one item at a time, so
// an earlier item's transport call (or a slow bridge acquisition) can outlast
// a later item's window. crossTransportBoundary commits the transport-call
// marker through the storage gate, which cancels such an item as expired
// instead. Invariants executed here:
//
//	I1 no transport call starts at or after the message's expires_at_ms
//	   (repository clock, millisecond resolution; the call follows the marker
//	   commit, so the marker instant is the call's start instant).
//	I4 a closed window, or a lease that expired right at the boundary, never
//	   makes DispatchDue return an error; the rest of the batch continues.
//	I5 each message with a window ends either confirmed after exactly one
//	   transport call started before its window closed, or canceled as
//	   expired (Delivery.Expired) with zero transport calls.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"sync"
	"testing"
	"testing/quick"
	"time"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

// acquireHookRegistry runs onAcquire before delegating each Acquire, so a
// test can move the clock (a slow bridge) or disturb the lease between the
// lease and the transport-call marker.
type acquireHookRegistry struct {
	*scriptedRegistry
	onAcquire func()
}

func (r *acquireHookRegistry) Acquire(
	ctx context.Context,
	accountID string,
	capability bridge.Capability,
) (*bridge.DispatchLease, error) {
	if r.onAcquire != nil {
		r.onAcquire()
	}
	return r.scriptedRegistry.Acquire(ctx, accountID, capability)
}

func windowedTextCommand(key string, ttl time.Duration) SendTextCommand {
	command := testCommonCommand(key)
	command.TTL = ttl
	// A body derived from the key's hash keeps sends in one batch dissimilar,
	// so the near-duplicate guard stays out of these tests without Force.
	digest := sha256.Sum256([]byte(key))
	return SendTextCommand{CommonCommand: command, Body: hex.EncodeToString(digest[:])}
}

func assertExpiredUnsent(t *testing.T, service *MessageService, outboxID string) {
	t.Helper()
	delivery := mustDelivery(t, service, outboxID)
	if delivery.State != OutboxCanceled || !delivery.Expired() {
		t.Fatalf("delivery = %+v, want canceled as expired", delivery)
	}
	if delivery.Warning == "" {
		t.Fatalf("expired delivery has no warning: %+v", delivery)
	}
	row := mustOutboxItem(t, service, outboxID)
	if row.ErrorCode == nil || *row.ErrorCode != sqlite.TTLErrorCode {
		t.Fatalf("error_code = %v, want %q", row.ErrorCode, sqlite.TTLErrorCode)
	}
	if row.LeaseToken != nil || row.TransportCalledAtMS != nil || row.AttemptCount != 0 {
		t.Fatalf("expired row = %+v, want no lease, no transport call, zero attempts", row)
	}
}

// TestExpiryDuringEarlierBatchSendCancelsLaterItem is the reproduction of the
// review's P1: A (no window) takes 20s in the transport; B, leased in the same
// batch with a 10s window, must not be sent when its turn comes at 20s.
func TestExpiryDuringEarlierBatchSendCancelsLaterItem(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	sender := &scriptedTextSender{
		steps: []sendStep{
			{result: bridge.SendResult{RemoteMessageID: "remote-slow-a"}},
			{result: bridge.SendResult{RemoteMessageID: "remote-late-b"}},
		},
	}
	var advanceOnce sync.Once
	sender.onSend = func() { advanceOnce.Do(func() { clock.Advance(20 * time.Second) }) }
	registry := newScriptedRegistry("batch-window", sender)
	registry.setAvailable(true)
	service := newMessagingTestService(t, store, registry, clock)

	a := mustSendText(t, service, windowedTextCommand("slow-a", 0))
	b := mustSendText(t, service, windowedTextCommand("windowed-b", 10*time.Second))

	processed, err := service.DispatchDue(context.Background(), 8)
	if err != nil || processed != 2 {
		t.Fatalf("DispatchDue() = %d, %v; want 2, nil", processed, err)
	}
	if got := sender.requestCount(); got != 1 {
		t.Fatalf("transport calls = %d, want 1 (B must not be sent after its window)", got)
	}
	if got := mustDelivery(t, service, a.OutboxID).State; got != OutboxConfirmed {
		t.Fatalf("A state = %q, want confirmed", got)
	}
	assertExpiredUnsent(t, service, b.OutboxID)

	waitCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	waited, err := service.Wait(waitCtx, b.OutboxID)
	if err != nil || waited.State != OutboxCanceled || !waited.Expired() {
		t.Fatalf("Wait(B) = %+v, %v; want canceled/expired promptly", waited, err)
	}
}

func TestExpiryDuringBridgeAcquireCancelsWithoutTransport(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	sender := &scriptedTextSender{steps: []sendStep{{result: bridge.SendResult{RemoteMessageID: "remote-late"}}}}
	inner := newScriptedRegistry("slow-acquire", sender)
	inner.setAvailable(true)
	registry := &acquireHookRegistry{scriptedRegistry: inner, onAcquire: func() { clock.Advance(20 * time.Second) }}
	service := newMessagingTestService(t, store, registry, clock)

	submission := mustSendText(t, service, windowedTextCommand("slow-acquire", 10*time.Second))
	processed, err := service.DispatchDue(context.Background(), 8)
	if err != nil || processed != 1 {
		t.Fatalf("DispatchDue() = %d, %v; want 1, nil", processed, err)
	}
	if got := sender.requestCount(); got != 0 {
		t.Fatalf("transport calls = %d, want 0", got)
	}
	assertExpiredUnsent(t, service, submission.OutboxID)
}

// TestSendWindowGateCoversEveryKind drives the gate at each of the four
// dispatch call sites: a 10s window closes during a 20s bridge acquisition,
// while the 30s lease is still live, and no kind may reach its transport.
func TestSendWindowGateCoversEveryKind(t *testing.T) {
	newHarness := func(t *testing.T) (*MessageService, *scriptedMediaSender, *scriptedReactionSender, *scriptedReadReceiptSender, *scriptedTextSender, sqlite.Message) {
		clock := newManualClock(messagingTestTime)
		store := openMessagingTestStore(t, clock.Now())
		seedDispatchDevice(t, store, "device-window", clock.Now())
		identityID := "identity-window-author"
		seedDispatchIdentity(t, store, identityID, "window-author@example.test", clock.Now())
		target := mustProjectDispatchMessage(t, store, clock, sqlite.Message{
			MessageID:        "message-window-target",
			ConversationID:   "conversation-1",
			AccountID:        "account-1",
			RemoteMessageID:  "remote-window-target",
			SenderIdentityID: &identityID,
			Direction:        sqlite.MessageDirectionIncoming,
			Body:             "window target",
			State:            sqlite.MessageStateActive,
			OccurredAtMS:     clock.Now().Add(-time.Minute).UnixMilli(),
		})
		text := &scriptedTextSender{steps: []sendStep{{result: bridge.SendResult{RemoteMessageID: "remote-text"}}}}
		media := &scriptedMediaSender{steps: []sendStep{{result: bridge.SendResult{RemoteMessageID: "remote-media"}}}}
		reaction := &scriptedReactionSender{steps: []sendStep{{result: bridge.SendResult{RemoteMessageID: "remote-reaction"}}}}
		read := &scriptedReadReceiptSender{steps: []readReceiptStep{{}}}
		inner := newScriptedRegistry("window-kinds", text)
		inner.setMediaSender(media)
		inner.setReactionSender(reaction)
		inner.setReadReceiptSender(read)
		inner.setAvailable(true)
		registry := &acquireHookRegistry{scriptedRegistry: inner, onAcquire: func() { clock.Advance(20 * time.Second) }}
		service := newDispatchTestService(t, store, registry, newDispatchTestBlobStore(t), clock)
		return service, media, reaction, read, text, target
	}

	for _, kind := range []string{"text", "media", "reaction", "read"} {
		t.Run(kind, func(t *testing.T) {
			service, media, reaction, read, text, target := newHarness(t)
			command := testCommonCommand("window-" + kind)
			command.TTL = 10 * time.Second
			var submission Submission
			var calls func() int
			switch kind {
			case "text":
				submission = mustSendText(t, service, SendTextCommand{CommonCommand: command, Body: "windowed text"})
				calls = text.requestCount
			case "media":
				submission = mustSendDispatchMedia(t, service, SendMediaCommand{
					CommonCommand: command,
					Content:       bytes.NewReader([]byte("windowed media")),
					Filename:      "window.bin",
					MIME:          "application/octet-stream",
				})
				calls = media.requestCount
			case "reaction":
				submission = mustSendDispatchReaction(t, service, SendReactionCommand{
					CommonCommand:   command,
					TargetMessageID: target.MessageID,
					Emoji:           "👍",
				})
				calls = reaction.requestCount
			case "read":
				submission = mustMarkDispatchRead(t, service, MarkReadCommand{
					CommonCommand:     command,
					DeviceID:          "device-window",
					LastReadMessageID: target.MessageID,
				})
				calls = read.requestCount
			}
			if submission.ExpiresAt.IsZero() {
				t.Fatalf("%s submission carries no window: %+v", kind, submission)
			}
			processed, err := service.DispatchDue(context.Background(), 8)
			if err != nil || processed != 1 {
				t.Fatalf("DispatchDue(%s) = %d, %v; want 1, nil", kind, processed, err)
			}
			if got := calls(); got != 0 {
				t.Fatalf("%s transport calls = %d, want 0", kind, got)
			}
			assertExpiredUnsent(t, service, submission.OutboxID)
		})
	}
}

// TestLeaseExpiryAtTransportBoundaryIsNotFatal is the adjacent liveness fix:
// the lease can expire between DispatchDue's per-item check and the marker.
// That ErrLeaseLost used to stop DispatchDue (and with it Run, which nothing
// restarts); it must instead recover the lease and continue.
func TestLeaseExpiryAtTransportBoundaryIsNotFatal(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	sender := &scriptedTextSender{steps: []sendStep{
		{result: bridge.SendResult{RemoteMessageID: "remote-after-recovery-1"}},
		{result: bridge.SendResult{RemoteMessageID: "remote-after-recovery-2"}},
	}}
	inner := newScriptedRegistry("lease-boundary", sender)
	inner.setAvailable(true)
	var stallOnce sync.Once
	registry := &acquireHookRegistry{scriptedRegistry: inner, onAcquire: func() {
		stallOnce.Do(func() { clock.Advance(defaultLeaseTime + time.Second) })
	}}
	service := newMessagingTestService(t, store, registry, clock)

	unbounded := mustSendText(t, service, windowedTextCommand("lease-unbounded", 0))
	windowed := mustSendText(t, service, windowedTextCommand("lease-windowed", 45*time.Second))

	processed, err := service.DispatchDue(context.Background(), 8)
	if err != nil {
		t.Fatalf("DispatchDue() error = %v, want nil (a lease expiring at the boundary is not fatal)", err)
	}
	if processed != 1 {
		t.Fatalf("DispatchDue() processed = %d, want 1", processed)
	}
	if got := sender.requestCount(); got != 0 {
		t.Fatalf("transport calls = %d, want 0", got)
	}
	for _, id := range []string{unbounded.OutboxID, windowed.OutboxID} {
		row := mustOutboxItem(t, service, id)
		if row.State != sqlite.OutboxNotDispatched || row.LeaseToken != nil || row.AttemptCount != 0 {
			t.Fatalf("row %s = %+v, want recovered not_dispatched with zero attempts", id, row)
		}
	}

	// The next loop retries both; the windowed one is still inside its 45s
	// window (31s elapsed), so both go out exactly once.
	clock.Advance(defaultRetryDelay)
	if processed, err := service.DispatchDue(context.Background(), 8); err != nil || processed != 2 {
		t.Fatalf("DispatchDue(retry) = %d, %v; want 2, nil", processed, err)
	}
	for _, id := range []string{unbounded.OutboxID, windowed.OutboxID} {
		if got := mustDelivery(t, service, id).State; got != OutboxConfirmed {
			t.Fatalf("%s state = %q, want confirmed", id, got)
		}
	}
	if got := sender.requestCount(); got != 2 {
		t.Fatalf("transport calls = %d, want 2", got)
	}
}

// TestForeignLeaseLossAtTransportBoundaryStaysAnError pins the other side of
// the liveness fix: only a lease the clock shows expired is recovered. A lease
// lost while still live on the clock is a state-machine fault and is returned.
func TestForeignLeaseLossAtTransportBoundaryStaysAnError(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	sender := &scriptedTextSender{}
	inner := newScriptedRegistry("foreign-loss", sender)
	inner.setAvailable(true)
	registry := &acquireHookRegistry{scriptedRegistry: inner}
	service := newMessagingTestService(t, store, registry, clock)
	submission := mustSendText(t, service, windowedTextCommand("foreign-loss", time.Minute))

	var stolen sync.Once
	registry.onAcquire = func() {
		stolen.Do(func() {
			row := mustOutboxItem(t, service, submission.OutboxID)
			if err := service.outbox.ReleaseUnavailable(context.Background(), row.OutboxID, *row.LeaseToken); err != nil {
				t.Errorf("steal lease: %v", err)
			}
		})
	}
	_, err := service.DispatchDue(context.Background(), 8)
	if !errors.Is(err, sqlite.ErrLeaseLost) {
		t.Fatalf("DispatchDue() error = %v, want ErrLeaseLost for a lease lost while live", err)
	}
	if got := sender.requestCount(); got != 0 {
		t.Fatalf("transport calls = %d, want 0", got)
	}
}

func TestSendAgainCarriesNoExpiry(t *testing.T) {
	// SendAgain is an explicit user action from the web UI outbox tray, whose
	// sends carry no window; copying the predecessor's absolute expiry would
	// usually give a row born expired. Pinned so a change is deliberate: even
	// a predecessor that had a window yields a resend without one.
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	// An empty transport result makes the predecessor uncertain.
	sender := &scriptedTextSender{steps: []sendStep{{result: bridge.SendResult{}}}}
	registry := newScriptedRegistry("send-again-window", sender)
	registry.setAvailable(true)
	service := newMessagingTestService(t, store, registry, clock)
	prior := mustSendText(t, service, windowedTextCommand("send-again-windowed-prior", 10*time.Minute))
	if prior.ExpiresAt.IsZero() {
		t.Fatalf("predecessor has no window: %+v", prior)
	}
	if processed, err := service.DispatchDue(context.Background(), 1); err != nil || processed != 1 {
		t.Fatalf("DispatchDue(prior) = %d, %v; want 1, nil", processed, err)
	}
	if got := mustDelivery(t, service, prior.OutboxID).State; got != OutboxUncertain {
		t.Fatalf("predecessor state = %q, want uncertain", got)
	}
	clock.Advance(time.Hour) // the predecessor's window has long closed

	resent, err := service.SendAgain(context.Background(), prior.OutboxID, "send-again-no-window")
	if err != nil {
		t.Fatalf("SendAgain(): %v", err)
	}
	if !resent.ExpiresAt.IsZero() {
		t.Fatalf("SendAgain submission expiry = %v, want zero (no window)", resent.ExpiresAt)
	}
	if row := mustOutboxItem(t, service, resent.OutboxID); row.ExpiresAtMS != nil {
		t.Fatalf("SendAgain row expires_at_ms = %d, want NULL", *row.ExpiresAtMS)
	}
}

func TestReactionAndReadReceiptStampSendWindow(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	seedDispatchDevice(t, store, "device-stamp", clock.Now())
	target := mustProjectDispatchMessage(t, store, clock, sqlite.Message{
		MessageID:       "message-stamp-target",
		ConversationID:  "conversation-1",
		AccountID:       "account-1",
		RemoteMessageID: "remote-stamp-target",
		Direction:       sqlite.MessageDirectionOutgoing,
		Body:            "stamp target",
		State:           sqlite.MessageStateActive,
		OccurredAtMS:    clock.Now().UnixMilli(),
	})
	registry := newScriptedRegistry("stamp", &scriptedTextSender{})
	registry.setReactionSender(&scriptedReactionSender{})
	registry.setReadReceiptSender(&scriptedReadReceiptSender{})
	service := newMessagingTestService(t, store, registry, clock)

	reactionCommand := testCommonCommand("stamp-reaction")
	reactionCommand.TTL = 2 * time.Minute
	reactionCommand.NotBefore = messagingTestTime.Add(time.Hour)
	reaction := mustSendDispatchReaction(t, service, SendReactionCommand{
		CommonCommand: reactionCommand, TargetMessageID: target.MessageID, Emoji: "🎉",
	})
	if want := reactionCommand.NotBefore.Add(2 * time.Minute); !reaction.ExpiresAt.Equal(want) {
		t.Fatalf("reaction expiry = %v, want %v (window opens at NotBefore)", reaction.ExpiresAt, want)
	}

	readCommand := testCommonCommand("stamp-read")
	readCommand.TTL = 3 * time.Minute
	read := mustMarkDispatchRead(t, service, MarkReadCommand{
		CommonCommand: readCommand, DeviceID: "device-stamp", LastReadMessageID: target.MessageID,
	})
	if want := messagingTestTime.Add(3 * time.Minute); !read.ExpiresAt.Equal(want) {
		t.Fatalf("read expiry = %v, want %v", read.ExpiresAt, want)
	}

	unbounded := mustSendDispatchReaction(t, service, SendReactionCommand{
		CommonCommand: testCommonCommand("stamp-reaction-none"), TargetMessageID: target.MessageID, Emoji: "👍",
	})
	if !unbounded.ExpiresAt.IsZero() {
		t.Fatalf("reaction without TTL expiry = %v, want zero", unbounded.ExpiresAt)
	}
}

// ttlBatchCase is one generated dispatch history: a batch of text sends with
// mixed windows, per-call transport durations (some longer than the 30s
// lease), an optional slow bridge acquisition, and an idle gap first.
type ttlBatchCase struct {
	TTLs         []time.Duration // 0 = no expiry
	SendDuration []time.Duration
	AcquireDelay time.Duration
	Gap          time.Duration
}

func generateTTLBatchCase(r *rand.Rand, acquireDelay bool) ttlBatchCase {
	n := 1 + r.Intn(6)
	c := ttlBatchCase{}
	for i := 0; i < n; i++ {
		if r.Intn(4) == 0 {
			c.TTLs = append(c.TTLs, 0)
		} else {
			c.TTLs = append(c.TTLs, time.Duration(1+r.Intn(60_000))*time.Millisecond)
		}
		c.SendDuration = append(c.SendDuration, time.Duration(r.Intn(40_000))*time.Millisecond)
	}
	c.Gap = time.Duration(r.Intn(60_000)) * time.Millisecond
	if acquireDelay {
		c.AcquireDelay = time.Duration(r.Intn(5_000)) * time.Millisecond
	}
	return c
}

type timedTransportCall struct {
	requestID string
	at        time.Time
}

// timedTextSender records when each call starts on the shared manual clock,
// then advances the clock by that call's scripted duration.
type timedTextSender struct {
	mu        sync.Mutex
	clock     *manualClock
	durations []time.Duration
	calls     []timedTransportCall
}

func (s *timedTextSender) SendText(_ context.Context, request bridge.TextRequest) (bridge.SendResult, error) {
	s.mu.Lock()
	index := len(s.calls)
	s.calls = append(s.calls, timedTransportCall{requestID: request.RequestID, at: s.clock.Now()})
	delay := time.Duration(0)
	if len(s.durations) > 0 {
		delay = s.durations[index%len(s.durations)]
	}
	s.mu.Unlock()
	s.clock.Advance(delay)
	return bridge.SendResult{RemoteMessageID: "remote-" + request.RequestID}, nil
}

// runTTLBatch drives one generated history through the production loop
// steps (sweep, then dispatch) and checks I1, I4, and I5. It returns a
// description of the first violation, or "" when every invariant held.
// ttlBatchEnv is the store, clock, and ID source shared by the cases of one
// property run. Opening a migrated store per case dominated the run time
// (over 20s per property under -race); sharing is safe because every case
// leaves its rows terminal before the next starts, so later cases never
// lease them, and IDs, keys, and bodies stay unique across cases.
type ttlBatchEnv struct {
	store *sqlite.Store
	clock *manualClock
	ids   *sequentialIDs
	cases int
	// Outcome tallies across the run, so a generator that stops reaching
	// either outcome fails loudly instead of passing vacuously.
	confirmed int
	expired   int
}

func newTTLBatchEnv(t *testing.T) *ttlBatchEnv {
	t.Helper()
	clock := newManualClock(messagingTestTime)
	return &ttlBatchEnv{store: openMessagingTestStore(t, clock.Now()), clock: clock, ids: &sequentialIDs{}}
}

func runTTLBatch(t *testing.T, env *ttlBatchEnv, c ttlBatchCase) string {
	t.Helper()
	env.cases++
	// Start each case well clear of the previous one's windows and leases.
	env.clock.Advance(24 * time.Hour)
	clock := env.clock
	sender := &timedTextSender{clock: clock, durations: c.SendDuration}
	inner := newScriptedRegistry("ttl-quick", sender)
	inner.setAvailable(true)
	var registry bridge.Registry = inner
	if c.AcquireDelay > 0 {
		registry = &acquireHookRegistry{scriptedRegistry: inner, onAcquire: func() { clock.Advance(c.AcquireDelay) }}
	}
	service, err := NewMessageService(env.store, registry, nil, clock, env.ids)
	if err != nil {
		t.Fatalf("NewMessageService(): %v", err)
	}
	ctx := context.Background()

	type sent struct {
		outboxID  string
		requestID string
		expiresAt int64 // 0 = no window
	}
	var messages []sent
	for i, ttl := range c.TTLs {
		command := windowedTextCommand(fmt.Sprintf("quick-%d-%d", env.cases, i), ttl)
		submission := mustSendText(t, service, command)
		item := mustOutboxItem(t, service, submission.OutboxID)
		message := sent{outboxID: submission.OutboxID, requestID: item.TransportRequestID}
		if item.ExpiresAtMS != nil {
			message.expiresAt = *item.ExpiresAtMS
		}
		messages = append(messages, message)
	}
	clock.Advance(c.Gap)
	// Run the production loop steps until every message is terminal (the
	// 40-iteration cap is far above what any generated history needs; a
	// history still pending at the cap fails I5 below).
	for iteration := 0; iteration < 40; iteration++ {
		if err := service.cancelExpiredDue(ctx); err != nil {
			return "cancelExpiredDue: " + err.Error()
		}
		if _, err := service.DispatchDue(ctx, defaultBatchLimit); err != nil {
			return "I4: DispatchDue returned an error (Run would stop): " + err.Error()
		}
		clock.Advance(defaultRetryDelay + time.Second)
		settled := true
		for _, message := range messages {
			state := mustDelivery(t, service, message.outboxID).State
			if state != OutboxConfirmed && state != OutboxCanceled {
				settled = false
				break
			}
		}
		if settled {
			break
		}
	}

	callsByRequest := map[string][]time.Time{}
	for _, call := range sender.calls {
		callsByRequest[call.requestID] = append(callsByRequest[call.requestID], call.at)
	}
	for _, message := range messages {
		calls := callsByRequest[message.requestID]
		for _, at := range calls {
			if message.expiresAt != 0 && at.UnixMilli() >= message.expiresAt {
				return fmt.Sprintf("I1: transport call for %s at %d ms >= expires_at %d ms (late by %d ms)",
					message.outboxID, at.UnixMilli(), message.expiresAt, at.UnixMilli()-message.expiresAt)
			}
		}
		delivery := mustDelivery(t, service, message.outboxID)
		switch {
		case delivery.State == OutboxConfirmed && len(calls) == 1:
			env.confirmed++
		case delivery.Expired() && len(calls) == 0 && message.expiresAt != 0:
			env.expired++
		default:
			return fmt.Sprintf("I5: %s ended %s (expired=%v) after %d transport calls",
				message.outboxID, delivery.State, delivery.Expired(), len(calls))
		}
	}
	return ""
}

// checkTTLBatchProperty runs runTTLBatch over generated histories. Values
// supplies each case from generate, so the two variants share one type.
func checkTTLBatchProperty(t *testing.T, seed int64, generate func(*rand.Rand) ttlBatchCase) {
	t.Helper()
	var firstFailure string
	var failing ttlBatchCase
	env := newTTLBatchEnv(t)
	property := func(c ttlBatchCase) bool {
		failure := runTTLBatch(t, env, c)
		if failure != "" && firstFailure == "" {
			firstFailure, failing = failure, c
		}
		return failure == ""
	}
	config := &quick.Config{
		MaxCount: 150,
		Rand:     rand.New(rand.NewSource(seed)),
		Values: func(values []reflect.Value, r *rand.Rand) {
			values[0] = reflect.ValueOf(generate(r))
		},
	}
	if err := quick.Check(property, config); err != nil {
		t.Fatalf("property violated after %d cases: %s\ncounterexample: %+v", env.cases, firstFailure, failing)
	}
	if env.cases != config.MaxCount || env.confirmed == 0 || env.expired == 0 {
		t.Fatalf("vacuous run: %d cases, %d confirmed, %d expired", env.cases, env.confirmed, env.expired)
	}
}

// TestQuickNoTransportCallAtOrAfterExpiry executes I1, I4, and I5 over
// generated batches. Without the gate it fails within the first few generated
// cases (a later item's transport call starts after its window closed); the
// minimized counterexample is TestMinimizedLateTransportCounterexample.
func TestQuickNoTransportCallAtOrAfterExpiry(t *testing.T) {
	checkTTLBatchProperty(t, 166_21, func(r *rand.Rand) ttlBatchCase { return generateTTLBatchCase(r, false) })
}

// TestQuickNoTransportCallAtOrAfterExpiryWithSlowAcquire adds a bridge
// acquisition delay before every item's marker.
func TestQuickNoTransportCallAtOrAfterExpiryWithSlowAcquire(t *testing.T) {
	checkTTLBatchProperty(t, 166_22, func(r *rand.Rand) ttlBatchCase { return generateTTLBatchCase(r, true) })
}

// TestMinimizedLateTransportCounterexample is the smallest failing history
// found before the gate: two sends, the first taking 2s, the second with a
// 1s window. The second's transport call started 1000 ms after its window
// closed.
func TestMinimizedLateTransportCounterexample(t *testing.T) {
	if failure := runTTLBatch(t, newTTLBatchEnv(t), ttlBatchCase{
		TTLs:         []time.Duration{0, time.Second},
		SendDuration: []time.Duration{2 * time.Second, 0},
	}); failure != "" {
		t.Fatal(failure)
	}
}
