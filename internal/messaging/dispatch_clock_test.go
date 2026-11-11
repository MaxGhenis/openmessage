package messaging

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

// splitReadClock answers one read with now and then steps forward by
// afterRead, so two reads that a real clock could split across a millisecond
// boundary land on different milliseconds every time.
type splitReadClock struct {
	mu        sync.Mutex
	now       time.Time
	afterRead time.Duration
}

func (c *splitReadClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now
	if c.afterRead != 0 {
		c.now = c.now.Add(c.afterRead)
		c.afterRead = 0
	}
	return now
}

func (c *splitReadClock) set(now time.Time, afterRead time.Duration) {
	c.mu.Lock()
	c.now, c.afterRead = now, afterRead
	c.mu.Unlock()
}

func (c *splitReadClock) NewTimer(delay time.Duration) Timer {
	return systemTimer{Timer: time.NewTimer(delay)}
}

// leaseBetweenSubmitReads queues a text whose SendText clock read (its
// scheduled_for_ms) falls 1 ms before the insert's (its created_at_ms), then
// sets the clock back to the first read: the time a dispatcher that read its
// lease time between the two reads would use. On 2026-11-03 that interleaving
// failed the outbox's updated_at_ms >= created_at_ms CHECK in the lease
// UPDATE, and Run stopped for good.
func leaseBetweenSubmitReads(
	t *testing.T,
	key string,
) (*MessageService, *scriptedTextSender, Submission) {
	t.Helper()
	clock := &splitReadClock{now: messagingTestTime}
	store := openMessagingTestStore(t, clock.Now())
	sender := &scriptedTextSender{steps: []sendStep{{result: bridge.SendResult{RemoteMessageID: "remote-" + key}}}}
	registry := newScriptedRegistry("scripted-alpha", sender)
	registry.setAvailable(true)
	service := newMessagingTestService(t, store, registry, clock)

	sendAt := messagingTestTime.Add(time.Second)
	clock.set(sendAt, time.Millisecond)
	submission := mustSendText(t, service, SendTextCommand{
		CommonCommand: testCommonCommand(key),
		Body:          "leased between the two reads",
	})
	row := mustOutboxItem(t, service, submission.OutboxID)
	if row.ScheduledForMS != sendAt.UnixMilli() || row.CreatedAtMS != sendAt.UnixMilli()+1 {
		t.Fatalf(
			"queued row scheduled_for_ms=%d created_at_ms=%d, want %d and %d",
			row.ScheduledForMS, row.CreatedAtMS, sendAt.UnixMilli(), sendAt.UnixMilli()+1,
		)
	}
	clock.set(sendAt, 0)
	return service, sender, submission
}

func TestDispatchDueLeasesRowInsertedAfterItsLeaseTime(t *testing.T) {
	service, sender, submission := leaseBetweenSubmitReads(t, "key-race-dispatch-due")

	processed, err := service.DispatchDue(context.Background(), 8)
	if err != nil || processed != 1 {
		t.Fatalf("DispatchDue() = %d, %v; want 1, nil", processed, err)
	}
	row := mustOutboxItem(t, service, submission.OutboxID)
	if row.State != sqlite.OutboxConfirmed || row.AttemptCount != 1 {
		t.Fatalf("row after dispatch = %+v, want confirmed after one attempt", row)
	}
	if row.UpdatedAtMS < row.CreatedAtMS {
		t.Fatalf("updated_at_ms = %d, below created_at_ms %d", row.UpdatedAtMS, row.CreatedAtMS)
	}
	if got := sender.requestCount(); got != 1 {
		t.Fatalf("transport requests = %d, want 1", got)
	}
}

func TestRunDispatchesRowInsertedAfterItsLeaseTime(t *testing.T) {
	service, sender, submission := leaseBetweenSubmitReads(t, "key-race-run")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- service.Run(ctx) }()

	waitCtx, cancelWait := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelWait()
	settled := make(chan Delivery, 1)
	go func() {
		delivery, _ := service.Wait(waitCtx, submission.OutboxID)
		settled <- delivery
	}()

	select {
	case err := <-done:
		t.Fatalf("Run() returned %v before the send settled; want it to dispatch the row", err)
	case delivery := <-settled:
		if delivery.State != OutboxConfirmed {
			t.Fatalf("delivery = %+v, want confirmed", delivery)
		}
	}
	if got := sender.requestCount(); got != 1 {
		t.Fatalf("transport requests = %d, want 1", got)
	}

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run() error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run() did not stop after cancellation")
	}
}
