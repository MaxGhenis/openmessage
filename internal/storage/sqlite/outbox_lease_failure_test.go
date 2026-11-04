package sqlite

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"
)

// leaseFailureTrigger fails every lease of one outbox row, the way a CHECK
// the row can no longer meet would, and lets every other write through.
func leaseFailureTrigger(outboxID string) string {
	return `
		CREATE TRIGGER fail_one_lease
		BEFORE UPDATE OF state ON outbox
		WHEN NEW.state = 'dispatching' AND OLD.outbox_id = '` + outboxID + `'
		BEGIN
			SELECT RAISE(ABORT, 'injected lease failure');
		END`
}

// A row whose lease always fails must not hold back the rows due after it.
// LeaseDue leased its batch in one transaction and the first failed lease
// rolled the whole batch back, so the next call met the same row first again
// and nothing behind it was ever leased.
func TestLeaseDueLeasesPastARowWhoseLeaseFails(t *testing.T) {
	ctx := context.Background()
	clock := newOutboxTestClock(outboxTestTimeMS)
	store, repository := openOutboxTestRepository(t, clock.Now)
	for offset, id := range []string{"head", "second", "third"} {
		item := outboxTestItem(id)
		item.ScheduledForMS = outboxTestTimeMS - 100 + int64(offset)
		mustEnqueueOutbox(t, repository, item)
	}
	mustExec(t, store.db, leaseFailureTrigger("outbox-head"))

	leases, err := repository.LeaseDue(ctx, LeaseRequest{
		Owner: "dispatcher", Now: clock.Now(), Duration: time.Minute, Limit: 8,
	})
	if err != nil {
		t.Fatalf("LeaseDue() with one row that cannot be leased: %v", err)
	}
	var leased []string
	for _, lease := range leases {
		leased = append(leased, lease.OutboxID)
	}
	if want := []string{"outbox-second", "outbox-third"}; !slices.Equal(leased, want) {
		t.Fatalf("LeaseDue() leased %v, want %v", leased, want)
	}

	head, err := repository.FindByID(ctx, "outbox-head")
	if err != nil {
		t.Fatalf("FindByID(head): %v", err)
	}
	if head.State != OutboxNotDispatched ||
		head.ErrorClass == nil || *head.ErrorClass != "transient" ||
		head.ErrorCode == nil || *head.ErrorCode != "lease" ||
		head.ErrorDetail == nil || !strings.Contains(*head.ErrorDetail, "injected lease failure") ||
		head.NextAttemptAtMS == nil || *head.NextAttemptAtMS <= outboxTestTimeMS ||
		head.AttemptCount != 0 || head.TransportCalledAtMS != nil || head.LeaseToken != nil {
		t.Fatalf("row whose lease failed = %+v, want it set aside as not_dispatched with the error and a later retry", head)
	}

	again, err := repository.LeaseDue(ctx, LeaseRequest{
		Owner: "dispatcher", Now: clock.Now(), Duration: time.Minute, Limit: 8,
	})
	if err != nil || len(again) != 0 {
		t.Fatalf("LeaseDue() again before the retry = %+v, %v; want nothing due", again, err)
	}
}
