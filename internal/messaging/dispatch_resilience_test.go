package messaging

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

// resilienceFixture runs Run over one queued text while a trigger, created
// through a second connection, fails a chosen outbox write.
type resilienceFixture struct {
	clock      *notifyingReconcileClock
	service    *MessageService
	raw        *sql.DB
	sender     *scriptedTextSender
	submission Submission
	failures   chan FailedDispatchPass

	stopped chan struct{}
	runErr  error
}

func newResilienceFixture(t *testing.T, key, trigger string) *resilienceFixture {
	t.Helper()
	clock := newNotifyingReconcileClock(messagingTestTime)
	store, raw := openReconcileTestStore(t, clock.Now())
	sender := &scriptedTextSender{steps: []sendStep{{result: bridge.SendResult{RemoteMessageID: "remote-" + key}}}}
	registry := newScriptedRegistry(bridge.Platform("resilience-"+key), sender)
	registry.setAvailable(true)
	service := newMessagingTestService(t, store, registry, clock)
	// The manual clock does not drive timers, so keep the real waits short.
	service.pollDelay, service.maxPollDelay = 10*time.Millisecond, 40*time.Millisecond
	if _, err := raw.ExecContext(context.Background(), trigger); err != nil {
		t.Fatalf("create failure trigger: %v", err)
	}
	submission := mustSendText(t, service, SendTextCommand{
		CommonCommand: testCommonCommand(key),
		Body:          "sent once the store recovers",
	})
	drainTimerSignals(clock)

	f := &resilienceFixture{
		clock:      clock,
		service:    service,
		raw:        raw,
		sender:     sender,
		submission: submission,
		failures:   make(chan FailedDispatchPass, 64),
		stopped:    make(chan struct{}),
	}
	service.OnPassFailure(func(err error, pass FailedDispatchPass) {
		if err == nil || err.Error() != pass.Error {
			t.Errorf("OnPassFailure(%v, %+v): error and pass disagree", err, pass)
		}
		select {
		case f.failures <- pass:
		default:
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		f.runErr = service.Run(ctx)
		close(f.stopped)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-f.stopped:
			if !errors.Is(f.runErr, context.Canceled) {
				t.Errorf("Run() error = %v, want context.Canceled", f.runErr)
			}
		case <-time.After(5 * time.Second):
			t.Error("Run() did not stop after cancellation")
		}
	})
	return f
}

// passEnded waits until Run finishes a pass and arms its next timer, and
// fails the test if Run returns instead.
func (f *resilienceFixture) passEnded(t *testing.T) {
	t.Helper()
	select {
	case <-f.clock.timerCreated:
	case <-f.stopped:
		t.Fatalf("Run() returned %v; want it to keep dispatching after a failed pass", f.runErr)
	case <-time.After(5 * time.Second):
		t.Fatal("Run() did not finish a pass")
	}
}

func (f *resilienceFixture) dropTrigger(t *testing.T, name string) {
	t.Helper()
	if _, err := f.raw.ExecContext(context.Background(), `DROP TRIGGER `+name); err != nil {
		t.Fatalf("drop trigger %s: %v", name, err)
	}
}

// confirmed waits for the queued text to be confirmed while Run keeps
// running. Wait alone would return at not_dispatched, which lease recovery
// passes through on the way to a retry.
func (f *resilienceFixture) confirmed(t *testing.T) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		changes := f.service.Changes()
		delivery, err := f.service.Get(context.Background(), f.submission.OutboxID)
		if err != nil {
			t.Fatalf("Get(): %v", err)
		}
		if delivery.State == OutboxConfirmed {
			return
		}
		select {
		case <-changes:
		case <-f.stopped:
			t.Fatalf("Run() returned %v before the text was confirmed", f.runErr)
		case <-deadline:
			t.Fatalf("text not confirmed; row = %+v", mustOutboxItem(t, f.service, f.submission.OutboxID))
		}
	}
}

// One failed lease must not stop the dispatcher. On 2026-11-03 LeaseDue's
// UPDATE failed a CHECK, Run returned the error, and no send left the outbox
// until the app restarted. The trigger fails every lease until it is dropped.
func TestRunKeepsDispatchingAfterAFailedLease(t *testing.T) {
	f := newResilienceFixture(t, "key-failed-lease", `
		CREATE TRIGGER fail_lease
		BEFORE UPDATE OF state ON outbox
		WHEN NEW.state = 'dispatching'
		BEGIN
			SELECT RAISE(ABORT, 'injected lease failure');
		END
	`)
	// Two failed passes: Run retries, rather than surviving one failure.
	f.passEnded(t)
	f.passEnded(t)
	if got := mustOutboxItem(t, f.service, f.submission.OutboxID); got.State != sqlite.OutboxQueued || got.AttemptCount != 0 {
		t.Fatalf("row while leases fail = %+v, want queued with no attempt", got)
	}
	for want := int64(1); want <= 2; want++ {
		pass := <-f.failures
		if pass.Consecutive != want || !strings.Contains(pass.Error, "injected lease failure") {
			t.Fatalf("failed pass %d = %+v, want consecutive %d naming the injected failure", want, pass, want)
		}
		if pass.RetryAtMS <= pass.AtMS {
			t.Fatalf("failed pass %d retries at %d, not after it failed at %d", want, pass.RetryAtMS, pass.AtMS)
		}
	}
	health := f.service.DispatchHealth()
	if health.FailedPasses < 2 || health.ConsecutiveFailedPasses < 2 || health.LastFailedPass == nil {
		t.Fatalf("health while leases fail = %+v, want two or more failed passes in a row", health)
	}

	f.dropTrigger(t, "fail_lease")
	f.confirmed(t)
	if got := f.sender.requestCount(); got != 1 {
		t.Fatalf("transport requests = %d, want 1", got)
	}
	// The pass that sent the text ended the run of failures; the history
	// stays.
	f.passEnded(t)
	recovered := f.service.DispatchHealth()
	if recovered.ConsecutiveFailedPasses != 0 || recovered.FailedPasses < health.FailedPasses || recovered.LastFailedPass == nil {
		t.Fatalf("health after recovery = %+v, want no current failures and the earlier ones kept", recovered)
	}
}

func TestNextPassBackoffDoublesUpToTheLongestPollDelay(t *testing.T) {
	const shortest, longest = 10 * time.Millisecond, 45 * time.Millisecond
	var backoff time.Duration
	var got []time.Duration
	for range 5 {
		backoff = nextPassBackoff(backoff, shortest, longest)
		got = append(got, backoff)
	}
	want := []time.Duration{shortest, 20 * time.Millisecond, 40 * time.Millisecond, longest, longest}
	if !slices.Equal(got, want) {
		t.Fatalf("backoffs = %v, want %v", got, want)
	}
	if got := nextPassBackoff(0, time.Hour, time.Minute); got != time.Hour {
		t.Fatalf("backoff with the shortest delay above the longest = %v, want the shortest", got)
	}
	if got := nextPassBackoff(0, 0, time.Minute); got != defaultPollDelay {
		t.Fatalf("backoff with no shortest delay = %v, want %v", got, defaultPollDelay)
	}
}

// A pass that fails after leasing leaves its lease behind. Run must recover
// that lease once it expires and send the text without a restart. Before,
// only Run's startup recovered expired leases.
func TestRunRecoversALeaseItsFailedPassLeftBehind(t *testing.T) {
	f := newResilienceFixture(t, "key-stranded-lease", `
		CREATE TRIGGER fail_mark_called
		BEFORE UPDATE OF transport_called_at_ms ON outbox
		WHEN NEW.transport_called_at_ms IS NOT NULL
		BEGIN
			SELECT RAISE(ABORT, 'injected mark-called failure');
		END
	`)
	f.passEnded(t)
	if got := mustOutboxItem(t, f.service, f.submission.OutboxID); got.State != sqlite.OutboxDispatching {
		t.Fatalf("row after the failed pass = %+v, want its lease left dispatching", got)
	}

	f.dropTrigger(t, "fail_mark_called")
	f.passEnded(t)
	if got := mustOutboxItem(t, f.service, f.submission.OutboxID); got.State != sqlite.OutboxDispatching {
		t.Fatalf("row before its lease expires = %+v, want the lease kept", got)
	}
	if got := f.sender.requestCount(); got != 0 {
		t.Fatalf("transport requests before the lease expires = %d, want 0", got)
	}

	f.clock.Advance(f.service.leaseTime)
	f.confirmed(t)
	if got := f.sender.requestCount(); got != 1 {
		t.Fatalf("transport requests = %d, want 1", got)
	}
}

// A failed pass waits out its backoff even when a submission wakes the
// dispatcher. A wake that ended the wait would let a steady stream of sends
// turn a storage fault that persists into a tight retry loop.
func TestRunWaitsOutItsBackoffWhenASubmissionWakesIt(t *testing.T) {
	clock := &waitProbeClock{
		splitReadClock: &splitReadClock{now: messagingTestTime},
		waits:          make(chan time.Duration, 8),
	}
	store, raw := openReconcileTestStore(t, clock.Now())
	registry := newScriptedRegistry("wake-probe", &scriptedTextSender{})
	registry.setAvailable(true)
	service := newMessagingTestService(t, store, registry, clock)
	service.pollDelay, service.maxPollDelay = time.Hour, 8*time.Hour
	if _, err := raw.ExecContext(context.Background(), `
		CREATE TRIGGER fail_lease
		BEFORE UPDATE OF state ON outbox
		WHEN NEW.state = 'dispatching'
		BEGIN
			SELECT RAISE(ABORT, 'injected lease failure');
		END
	`); err != nil {
		t.Fatalf("create failure trigger: %v", err)
	}
	mustSendText(t, service, SendTextCommand{
		CommonCommand: testCommonCommand("key-before-the-fault"),
		Body:          "queued while leases fail",
	})
	drainReconcileWake(service)

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan error, 1)
	go func() { stopped <- service.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-stopped:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("Run() error = %v, want context.Canceled", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("Run() did not stop after cancellation")
		}
	})

	select {
	case wait := <-clock.waits:
		if wait != service.pollDelay {
			t.Fatalf("Run waits %v after its first failed pass, want the poll delay %v", wait, service.pollDelay)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run() did not finish its first pass")
	}
	mustSendText(t, service, SendTextCommand{
		CommonCommand: testCommonCommand("key-during-the-backoff"),
		Body:          "submitted while Run backs off",
	})
	select {
	case wait := <-clock.waits:
		t.Fatalf("the submission's wake ended the backoff: Run passed again and waits %v", wait)
	case err := <-stopped:
		t.Fatalf("Run() returned %v during its backoff", err)
	case <-time.After(500 * time.Millisecond):
	}
	if health := service.DispatchHealth(); health.FailedPasses != 1 {
		t.Fatalf("failed passes = %d during the backoff, want 1", health.FailedPasses)
	}
}

// waitProbeClock reports each wait of an hour or more, the waits Run starts
// when the test sets its poll delay to an hour, and never fires their timers,
// so only a wake or cancellation can end one. Shorter timers bound transport
// calls and run on the system clock.
type waitProbeClock struct {
	*splitReadClock
	waits chan time.Duration
}

func (c *waitProbeClock) NewTimer(delay time.Duration) Timer {
	if delay < time.Hour {
		return c.splitReadClock.NewTimer(delay)
	}
	select {
	case c.waits <- delay:
	default:
	}
	return heldTimer{}
}

// heldTimer never fires: a receive from its nil channel blocks forever.
type heldTimer struct{}

func (heldTimer) C() <-chan time.Time { return nil }

func (heldTimer) Stop() bool { return true }
