package messaging

import (
	"context"
	"database/sql"
	"errors"
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
		stopped:    make(chan struct{}),
	}
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

// settled waits for the queued text to settle while Run keeps running.
func (f *resilienceFixture) settled(t *testing.T) Delivery {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	type waited struct {
		delivery Delivery
		err      error
	}
	result := make(chan waited, 1)
	go func() {
		delivery, err := f.service.Wait(ctx, f.submission.OutboxID)
		result <- waited{delivery, err}
	}()
	select {
	case got := <-result:
		if got.err != nil {
			t.Fatalf("Wait(): %v; row = %+v", got.err, mustOutboxItem(t, f.service, f.submission.OutboxID))
		}
		return got.delivery
	case <-f.stopped:
		t.Fatalf("Run() returned %v before the text settled", f.runErr)
	}
	return Delivery{}
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

	f.dropTrigger(t, "fail_lease")
	if delivery := f.settled(t); delivery.State != OutboxConfirmed {
		t.Fatalf("delivery = %+v, want confirmed", delivery)
	}
	if got := f.sender.requestCount(); got != 1 {
		t.Fatalf("transport requests = %d, want 1", got)
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
	if delivery := f.settled(t); delivery.State != OutboxConfirmed {
		t.Fatalf("delivery = %+v, want confirmed", delivery)
	}
	if got := f.sender.requestCount(); got != 1 {
		t.Fatalf("transport requests = %d, want 1", got)
	}
}
