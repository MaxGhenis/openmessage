package messaging

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

// A row whose lease always fails must not stop the sends queued after it.
// LeaseDue gave up its whole batch at the first failed lease, and every pass
// met that row first again, so after PR #274 Run kept retrying a pass that
// could never lease anything and no later text, media, reaction or read
// receipt left the outbox.
func TestDispatchPassSendsPastARowWhoseLeaseAlwaysFails(t *testing.T) {
	ctx := context.Background()
	clock := newManualClock(messagingTestTime)
	store, raw := openReconcileTestStore(t, clock.Now())
	sender := &scriptedTextSender{steps: []sendStep{
		{result: bridge.SendResult{RemoteMessageID: "remote-second"}},
		{result: bridge.SendResult{RemoteMessageID: "remote-third"}},
	}}
	registry := newScriptedRegistry("lease-failure", sender)
	registry.setAvailable(true)
	service := newMessagingTestService(t, store, registry, clock)

	var queued []Submission
	for _, key := range []string{"key-head", "key-second", "key-third"} {
		queued = append(queued, mustSendText(t, service, SendTextCommand{
			CommonCommand: testCommonCommand(key),
			Body:          "queued as " + key,
		}))
		clock.Advance(time.Millisecond)
	}
	head := queued[0]
	if _, err := raw.ExecContext(ctx, `
		CREATE TRIGGER fail_head_lease
		BEFORE UPDATE OF state ON outbox
		WHEN NEW.state = 'dispatching' AND OLD.outbox_id = '`+head.OutboxID+`'
		BEGIN
			SELECT RAISE(ABORT, 'injected lease failure');
		END
	`); err != nil {
		t.Fatalf("create lease failure trigger: %v", err)
	}

	for pass := range 3 {
		clock.Advance(time.Second)
		if err := service.dispatchPass(ctx); err != nil {
			t.Errorf("pass %d: %v", pass, err)
		}
	}
	for _, later := range queued[1:] {
		if got := mustOutboxItem(t, service, later.OutboxID); got.State != sqlite.OutboxConfirmed {
			t.Errorf("text queued after the row whose lease fails = %+v, want confirmed", got)
		}
	}
	if got := sender.requestCount(); got != 2 {
		t.Errorf("transport requests = %d, want 2 (the texts behind the failing row)", got)
	}
	if got := mustOutboxItem(t, service, head.OutboxID); got.State != sqlite.OutboxNotDispatched || got.AttemptCount != 0 {
		t.Errorf("row whose lease fails = %+v, want not_dispatched with no transport attempt", got)
	}
}

// An expired lease whose recovery write always fails must hold neither the
// recovery of the other expired leases nor the sends queued after it.
// RecoverExpiredLeases rewrote every expired lease in two multi-row UPDATEs,
// and dispatchPass runs it before DispatchDue, so one row whose recovery
// failed failed every pass before it sent anything.
func TestDispatchPassSendsPastALeaseWhoseRecoveryAlwaysFails(t *testing.T) {
	ctx := context.Background()
	clock := newManualClock(messagingTestTime)
	store, raw := openReconcileTestStore(t, clock.Now())
	sender := &scriptedTextSender{steps: []sendStep{
		{result: bridge.SendResult{RemoteMessageID: "remote-first"}},
		{result: bridge.SendResult{RemoteMessageID: "remote-second"}},
	}}
	registry := newScriptedRegistry("recovery-failure", sender)
	registry.setAvailable(true)
	service := newMessagingTestService(t, store, registry, clock)

	// A pass that crashed left four leases behind: two before their transport
	// call started and two after.
	failingUncalled := strandTextLease(t, service, clock, "key-failing-uncalled", false)
	failingCalled := strandTextLease(t, service, clock, "key-failing-called", true)
	uncalled := strandTextLease(t, service, clock, "key-uncalled", false)
	called := strandTextLease(t, service, clock, "key-called", true)
	if _, err := raw.ExecContext(ctx, failRecoveryTrigger(failingUncalled.OutboxID, failingCalled.OutboxID)); err != nil {
		t.Fatalf("create recovery failure trigger: %v", err)
	}
	clock.Advance(2 * time.Second)
	later := mustSendText(t, service, SendTextCommand{
		CommonCommand: testCommonCommand("key-later"),
		Body:          "queued after the leases expired",
	})

	for pass := range 3 {
		clock.Advance(time.Second)
		if err := service.dispatchPass(ctx); err != nil {
			t.Errorf("pass %d: %v", pass, err)
		}
	}
	for _, sent := range []string{uncalled.OutboxID, later.OutboxID} {
		if got := mustOutboxItem(t, service, sent); got.State != sqlite.OutboxConfirmed {
			t.Errorf("text whose lease recovers or that was queued later = %+v, want confirmed", got)
		}
	}
	if got := mustOutboxItem(t, service, called.OutboxID); got.State != sqlite.OutboxUncertain {
		t.Errorf("text whose called lease recovers = %+v, want uncertain", got)
	}
	requests := requestsByID(sender)
	if len(requests) != 2 || requests[uncalled.TransportRequestID] != 1 ||
		requests[mustOutboxItem(t, service, later.OutboxID).TransportRequestID] != 1 {
		t.Errorf("transport requests = %v, want one each for the recovered uncalled text and the later text", requests)
	}
	for _, failing := range []sqlite.OutboxItem{failingUncalled, failingCalled} {
		if got := mustOutboxItem(t, service, failing.OutboxID); !reflect.DeepEqual(got, failing) {
			t.Errorf("text whose recovery fails = %+v, want it as it was, %+v", got, failing)
		}
	}
}

// strandTextLease queues a text and leaves it leased for a second, as a pass
// that crashed would: before its transport call started or, when called is
// set, after. The text must be the only due row.
func strandTextLease(t *testing.T, service *MessageService, clock *manualClock, key string, called bool) sqlite.OutboxItem {
	t.Helper()
	ctx := context.Background()
	submission := mustSendText(t, service, SendTextCommand{
		CommonCommand: testCommonCommand(key),
		Body:          "stranded as " + key,
	})
	batch, err := service.outbox.LeaseDueBatch(ctx, sqlite.LeaseRequest{
		Owner: "crashed-pass", Now: clock.Now(), Duration: time.Second, Limit: 1,
	})
	if err != nil || len(batch.Leases) != 1 || batch.Leases[0].OutboxID != submission.OutboxID {
		t.Fatalf("LeaseDueBatch(%s) = %+v, %v; want its one lease", key, batch, err)
	}
	if called {
		if err := service.outbox.MarkTransportCalled(ctx, sqlite.Attempt{
			OutboxID: submission.OutboxID, LeaseToken: *batch.Leases[0].LeaseToken,
		}); err != nil {
			t.Fatalf("MarkTransportCalled(%s): %v", key, err)
		}
	}
	clock.Advance(time.Millisecond)
	return mustOutboxItem(t, service, submission.OutboxID)
}

// failRecoveryTrigger returns a trigger, fail_recovery, that fails the
// recovery of each named row's expired lease, the way a CHECK the row can no
// longer meet would, and lets every other write through.
func failRecoveryTrigger(outboxIDs ...string) string {
	return `
		CREATE TRIGGER fail_recovery
		BEFORE UPDATE OF state ON outbox
		WHEN OLD.state = 'dispatching' AND NEW.state IN ('not_dispatched', 'uncertain')
			AND OLD.outbox_id IN ('` + strings.Join(outboxIDs, "', '") + `')
		BEGIN
			SELECT RAISE(ABORT, 'injected recovery failure');
		END`
}
