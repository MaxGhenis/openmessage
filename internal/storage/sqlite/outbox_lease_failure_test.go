package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"regexp"
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
// and never leased anything behind it.
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

// A row whose lease keeps failing waits twice as long after each failure, up
// to five minutes, and a manual retry starts the delay over.
func TestLeaseDueBatchBacksOffARowWhoseLeaseKeepsFailing(t *testing.T) {
	ctx := context.Background()
	clock := newOutboxTestClock(outboxTestTimeMS)
	store, repository := openOutboxTestRepository(t, clock.Now)
	item := outboxTestItem("head")
	item.ScheduledForMS = outboxTestTimeMS - 100
	mustEnqueueOutbox(t, repository, item)
	mustExec(t, store.db, leaseFailureTrigger("outbox-head"))

	setAside := func() time.Duration {
		t.Helper()
		now := clock.Now()
		batch := mustLeaseDueBatch(t, repository, now, 8)
		if len(batch.Leases) != 0 || len(batch.Failed) != 1 || batch.Failed[0].SetAsideErr != nil {
			t.Fatalf("LeaseDueBatch() at %d = %+v, want the row set aside", now.UnixMilli(), batch)
		}
		retryAt := batch.Failed[0].RetryAt
		row := mustFindOutboxRow(t, repository, "outbox-head")
		if row.NextAttemptAtMS == nil || *row.NextAttemptAtMS != retryAt.UnixMilli() ||
			row.UpdatedAtMS != now.UnixMilli() || row.AttemptCount != 0 {
			t.Fatalf("row set aside at %d = %+v, want next attempt %d, updated now, no attempt",
				now.UnixMilli(), row, retryAt.UnixMilli())
		}
		return retryAt.Sub(now)
	}

	var delays []time.Duration
	for range 9 {
		delay := setAside()
		delays = append(delays, delay)
		clock.Set(clock.Now().Add(delay).UnixMilli())
	}
	want := []time.Duration{
		5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second,
		80 * time.Second, 160 * time.Second, 5 * time.Minute, 5 * time.Minute, 5 * time.Minute,
	}
	if !slices.Equal(delays, want) {
		t.Fatalf("delays after each failed lease = %v, want %v", delays, want)
	}

	// The set-aside holds the row until 5 minutes from now. A manual retry
	// makes it due at once, and its next failed lease waits the first delay
	// again.
	clock.Set(clock.Now().Add(-4 * time.Minute).UnixMilli())
	if err := repository.RetryNotDispatched(ctx, "outbox-head"); err != nil {
		t.Fatalf("RetryNotDispatched(): %v", err)
	}
	if delay := setAside(); delay != 5*time.Second {
		t.Fatalf("delay after a manual retry = %v, want 5s", delay)
	}
}

// Only a lease failure's delay doubles. The dispatcher set this row's last
// retry a minute out after another failure, so when its lease then fails, its
// delay starts over at 5 s instead of doubling that minute.
func TestLeaseDueBatchStartsTheDelayOverAfterAnotherFailure(t *testing.T) {
	ctx := context.Background()
	clock := newOutboxTestClock(outboxTestTimeMS)
	store, repository := openOutboxTestRepository(t, clock.Now)
	item := outboxTestItem("head")
	item.ScheduledForMS = outboxTestTimeMS - 100
	mustEnqueueOutbox(t, repository, item)
	batch := mustLeaseDueBatch(t, repository, clock.Now(), 1)
	if len(batch.Leases) != 1 || batch.Leases[0].LeaseToken == nil {
		t.Fatalf("LeaseDueBatch() = %+v, want the row leased", batch)
	}
	retryAt := clock.Now().Add(time.Minute)
	if err := repository.MarkNotDispatched(ctx, "outbox-head", *batch.Leases[0].LeaseToken,
		"offline", "not_connected", "transport offline", retryAt); err != nil {
		t.Fatalf("MarkNotDispatched(): %v", err)
	}

	clock.Set(retryAt.UnixMilli())
	mustExec(t, store.db, leaseFailureTrigger("outbox-head"))
	batch = mustLeaseDueBatch(t, repository, clock.Now(), 1)
	if len(batch.Leases) != 0 || len(batch.Failed) != 1 || batch.Failed[0].SetAsideErr != nil {
		t.Fatalf("LeaseDueBatch() at the retry = %+v, want the row set aside", batch)
	}
	if delay := batch.Failed[0].RetryAt.Sub(clock.Now()); delay != 5*time.Second {
		t.Fatalf("delay after the first failed lease = %v, want 5s", delay)
	}
	row := mustFindOutboxRow(t, repository, "outbox-head")
	if row.ErrorCode == nil || *row.ErrorCode != LeaseFailedCode || row.NextAttemptAtMS == nil ||
		*row.NextAttemptAtMS != batch.Failed[0].RetryAt.UnixMilli() {
		t.Fatalf("row set aside = %+v, want error code %q and the retry time the call returned",
			row, LeaseFailedCode)
	}
}

// A row the call can neither lease nor set aside stays as it was, still due,
// and the call leases the rows behind it however many such rows come first,
// up to the batch limit and no further.
func TestLeaseDueBatchLeasesPastRowsThatCanBeNeitherLeasedNorSetAside(t *testing.T) {
	clock := newOutboxTestClock(outboxTestTimeMS)
	store, repository := openOutboxTestRepository(t, clock.Now)
	for offset, id := range []string{"a", "b", "c", "d", "e", "f"} {
		item := outboxTestItem(id)
		item.ScheduledForMS = outboxTestTimeMS - 100 + int64(offset)
		mustEnqueueOutbox(t, repository, item)
	}
	mustExec(t, store.db, `
		CREATE TRIGGER fail_due_writes
		BEFORE UPDATE ON outbox
		WHEN OLD.outbox_id IN ('outbox-a', 'outbox-b', 'outbox-c')
		  AND OLD.state IN ('queued', 'not_dispatched')
		BEGIN
			SELECT RAISE(ABORT, 'injected write failure');
		END`)
	stuck := []string{"outbox-a", "outbox-b", "outbox-c"}
	before := make(map[string]OutboxItem)
	for _, id := range stuck {
		before[id] = mustFindOutboxRow(t, repository, id)
	}

	// The limit is 2, so the first page holds only rows that fail. The second
	// holds the third stuck row and three that lease, one more than the batch
	// has room for, so f waits for the next call.
	batch := mustLeaseDueBatch(t, repository, clock.Now(), 2)
	if got, want := leasedOutboxIDs(batch), []string{"outbox-d", "outbox-e"}; !slices.Equal(got, want) {
		t.Fatalf("LeaseDueBatch() leased %v, want %v", got, want)
	}
	assertStuckFailures(t, batch, stuck, "injected write failure")
	for _, id := range stuck {
		if after := mustFindOutboxRow(t, repository, id); !reflect.DeepEqual(after, before[id]) {
			t.Fatalf("row that could not be set aside = %+v, want it unchanged from %+v", after, before[id])
		}
	}

	assertOutboxRowsQueued(t, repository, "outbox-f")

	again := mustLeaseDueBatch(t, repository, clock.Now(), 2)
	if got, want := leasedOutboxIDs(again), []string{"outbox-f"}; !slices.Equal(got, want) {
		t.Fatalf("LeaseDueBatch() again leased %v, want %v", got, want)
	}
	assertStuckFailures(t, again, stuck, "injected write failure")
}

// A call meets each row it can neither lease nor set aside once, and pays for
// it with a few writes, not with a select over the whole due set. A call that
// selected again for each refill, leaving out every row it had tried, would
// pay a select for each such row: 6,000 of them between the 31st and 32nd
// healthy rows kept such a call running for tens of seconds, near or past the
// dispatcher's 30 s lease, while it held the store's write lock. The test
// measures the call against the writes it cannot avoid, one failed UPDATE
// per stuck row, so the bound holds on a fast machine and under the race
// detector alike.
func TestLeaseDueBatchCostsAFewWritesPerStuckRow(t *testing.T) {
	const stuck = 2000
	ctx := context.Background()
	clock := newOutboxTestClock(outboxTestTimeMS)
	store, repository := openOutboxTestRepository(t, clock.Now)
	for index := range 31 {
		item := outboxTestItem(fmt.Sprintf("healthy-%02d", index))
		item.ScheduledForMS = outboxTestTimeMS - 20_000 + int64(index)
		mustEnqueueOutbox(t, repository, item)
	}
	mustExec(t, store.db, `
		WITH RECURSIVE n(x) AS (VALUES (1) UNION ALL SELECT x + 1 FROM n WHERE x < ?)
		INSERT INTO outbox (
			outbox_id, account_id, conversation_id, kind, idempotency_key,
			payload_hash, operation, state, transport_request_id,
			scheduled_for_ms, created_at_ms, updated_at_ms
		)
		SELECT printf('stuck-%05d', x), account_id, conversation_id, kind, 'stuck-key-' || x,
			payload_hash, operation, 'queued', 'stuck-request-' || x,
			?, created_at_ms, updated_at_ms
		FROM n CROSS JOIN outbox
		WHERE outbox_id = 'outbox-healthy-00'
	`, stuck, outboxTestTimeMS-10_000)
	tail := outboxTestItem("tail")
	tail.ScheduledForMS = outboxTestTimeMS - 1
	mustEnqueueOutbox(t, repository, tail)
	mustExec(t, store.db, `
		CREATE TRIGGER fail_stuck_writes
		BEFORE UPDATE ON outbox
		WHEN OLD.outbox_id LIKE 'stuck-%'
		BEGIN
			SELECT RAISE(ABORT, 'injected write failure');
		END`)

	floor := timeFailedWrites(t, store, stuck)
	// The dispatcher's lease time. A call that runs longer hands out leases
	// that have already expired by the wall clock, and the dispatcher recovers
	// them instead of sending.
	const leaseTime = 30 * time.Second
	started := time.Now()
	batch, err := repository.LeaseDueBatch(ctx, LeaseRequest{
		Owner: "dispatcher", Now: clock.Now(), Duration: leaseTime, Limit: 32,
	})
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("LeaseDueBatch(): %v", err)
	}
	t.Logf("LeaseDueBatch() past %d stuck rows took %v; one failed write per row took %v", stuck, elapsed, floor)
	if elapsed > 40*floor || elapsed >= leaseTime {
		t.Fatalf("LeaseDueBatch() past %d stuck rows took %v, %.0f times the %v one failed write per row takes; want under 40 times, and under the %v lease",
			stuck, elapsed, float64(elapsed)/float64(floor), floor, leaseTime)
	}
	leased := leasedOutboxIDs(batch)
	if len(leased) != 32 || leased[0] != "outbox-healthy-00" || leased[31] != "outbox-tail" {
		t.Fatalf("LeaseDueBatch() leased %d rows (%v), want the 31 healthy rows and the tail", len(leased), leased)
	}
	if len(batch.Failed) != stuck {
		t.Fatalf("LeaseDueBatch() reported %d failures, want %d", len(batch.Failed), stuck)
	}
	for index, failure := range batch.Failed {
		if want := fmt.Sprintf("stuck-%05d", index+1); failure.OutboxID != want || failure.SetAsideErr == nil {
			t.Fatalf("failure %d = %+v, want %q neither leased nor set aside", index, failure, want)
		}
	}
}

// timeFailedWrites times one failed UPDATE of each of the count stuck rows,
// in one transaction that it rolls back.
func timeFailedWrites(t *testing.T, store *Store, count int) time.Duration {
	t.Helper()
	ctx := context.Background()
	started := time.Now()
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin transaction: %v", err)
	}
	defer tx.Rollback()
	for x := 1; x <= count; x++ {
		if _, err := tx.ExecContext(ctx, `
			UPDATE outbox SET updated_at_ms = MAX(updated_at_ms, ?) WHERE outbox_id = ?
		`, outboxTestTimeMS, fmt.Sprintf("stuck-%05d", x)); err == nil {
			t.Fatalf("UPDATE of stuck row %d succeeded, want the trigger to fail it", x)
		}
	}
	return time.Since(started)
}

// After the clock steps back below a row's updated_at_ms, the next delay of a
// row whose lease keeps failing comes out short of double, never under 5 s,
// so the row retries sooner, never later. The set-aside reads a row's previous
// delay as its next_attempt_at_ms less its updated_at_ms, and updated_at_ms
// never moves back. Here the first failure comes 1 s behind the row's last
// write, so the second delay comes to 8 s, not 10 s; the clock has caught up
// by then, and the third delay doubles the second.
func TestLeaseDueBatchShortensTheDelayAfterTheClockStepsBack(t *testing.T) {
	ctx := context.Background()
	clock := newOutboxTestClock(outboxTestTimeMS)
	store, repository := openOutboxTestRepository(t, clock.Now)
	mustEnqueueOutbox(t, repository, outboxTestItem("head"))
	leases := mustLeaseDueBatch(t, repository, clock.Now(), 8).Leases
	if len(leases) != 1 {
		t.Fatalf("LeaseDueBatch() leased %d rows, want 1", len(leases))
	}
	clock.Set(outboxTestTimeMS + 10_000)
	if err := repository.ReleaseUnavailable(ctx, "outbox-head", *leases[0].LeaseToken); err != nil {
		t.Fatalf("ReleaseUnavailable(): %v", err)
	}
	mustExec(t, store.db, leaseFailureTrigger("outbox-head"))

	var delays []time.Duration
	for _, nowMS := range []int64{outboxTestTimeMS + 9_000, outboxTestTimeMS + 14_000, outboxTestTimeMS + 22_000} {
		clock.Set(nowMS)
		before := mustFindOutboxRow(t, repository, "outbox-head")
		batch := mustLeaseDueBatch(t, repository, clock.Now(), 8)
		if len(batch.Leases) != 0 || len(batch.Failed) != 1 || batch.Failed[0].SetAsideErr != nil {
			t.Fatalf("LeaseDueBatch() at %d = %+v, want the row set aside", nowMS, batch)
		}
		row := mustFindOutboxRow(t, repository, "outbox-head")
		if row.UpdatedAtMS != max(before.UpdatedAtMS, nowMS) ||
			row.NextAttemptAtMS == nil || *row.NextAttemptAtMS != batch.Failed[0].RetryAt.UnixMilli() {
			t.Fatalf("row set aside at %d = %+v, want updated_at_ms %d and next attempt %d",
				nowMS, row, max(before.UpdatedAtMS, nowMS), batch.Failed[0].RetryAt.UnixMilli())
		}
		delays = append(delays, batch.Failed[0].RetryAt.Sub(clock.Now()))
	}
	if want := []time.Duration{5 * time.Second, 8 * time.Second, 16 * time.Second}; !slices.Equal(delays, want) {
		t.Fatalf("delays after each failed lease = %v, want %v", delays, want)
	}
}

// LeaseDueBatch handles a row whose writes a real CHECK refuses like one whose
// writes a trigger refuses. This row's updated_at_ms trails its
// created_at_ms, which the CHECK forbids, so every write to it fails until
// something repairs it.
func TestLeaseDueBatchLeasesPastARowThatBreaksACheckOnEveryWrite(t *testing.T) {
	ctx := context.Background()
	clock := newOutboxTestClock(outboxTestTimeMS)
	store, repository := openOutboxTestRepository(t, clock.Now)
	for offset, id := range []string{"head", "second"} {
		item := outboxTestItem(id)
		item.ScheduledForMS = outboxTestTimeMS - 100 + int64(offset)
		mustEnqueueOutbox(t, repository, item)
	}
	withPinnedConnection(t, store, func(conn *sql.Conn) {
		mustExecConn(t, conn, `PRAGMA ignore_check_constraints = ON`)
		mustExecConn(t, conn, `
			UPDATE outbox SET created_at_ms = ?, updated_at_ms = ?
			WHERE outbox_id = 'outbox-head'
		`, outboxTestTimeMS+50, outboxTestTimeMS+40)
		mustExecConn(t, conn, `PRAGMA ignore_check_constraints = OFF`)
	})
	before := mustFindOutboxRow(t, repository, "outbox-head")

	batch, err := repository.LeaseDueBatch(ctx, LeaseRequest{
		Owner: "dispatcher", Now: clock.Now(), Duration: time.Minute, Limit: 8,
	})
	if err != nil {
		t.Fatalf("LeaseDueBatch(): %v", err)
	}
	if got, want := leasedOutboxIDs(batch), []string{"outbox-second"}; !slices.Equal(got, want) {
		t.Fatalf("LeaseDueBatch() leased %v, want %v", got, want)
	}
	assertStuckFailures(t, batch, []string{"outbox-head"}, "CHECK constraint failed")
	if after := mustFindOutboxRow(t, repository, "outbox-head"); !reflect.DeepEqual(after, before) {
		t.Fatalf("row that breaks a CHECK = %+v, want it unchanged from %+v", after, before)
	}
}

// A trigger that skips a row's lease (RAISE(IGNORE)) changes nothing, so the
// lease UPDATE affects no row. That failure belongs to the row too: the call
// sets the row aside, or leaves it as it was when a trigger skips its
// set-aside as well.
func TestLeaseDueBatchLeasesPastARowWhoseWritesATriggerSkips(t *testing.T) {
	for _, test := range []struct {
		name, when string
		setAside   bool
	}{
		{
			name:     "lease skipped",
			when:     `UPDATE OF state ON outbox WHEN NEW.state = 'dispatching' AND OLD.outbox_id = 'outbox-head'`,
			setAside: true,
		},
		{
			name: "every write skipped",
			when: `UPDATE ON outbox WHEN OLD.outbox_id = 'outbox-head' AND OLD.state IN ('queued', 'not_dispatched')`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			clock := newOutboxTestClock(outboxTestTimeMS)
			store, repository := openOutboxTestRepository(t, clock.Now)
			for offset, id := range []string{"head", "second"} {
				item := outboxTestItem(id)
				item.ScheduledForMS = outboxTestTimeMS - 100 + int64(offset)
				mustEnqueueOutbox(t, repository, item)
			}
			mustExec(t, store.db, `CREATE TRIGGER skip_head BEFORE `+test.when+` BEGIN SELECT RAISE(IGNORE); END`)
			before := mustFindOutboxRow(t, repository, "outbox-head")

			batch := mustLeaseDueBatch(t, repository, clock.Now(), 8)
			if got, want := leasedOutboxIDs(batch), []string{"outbox-second"}; !slices.Equal(got, want) {
				t.Fatalf("LeaseDueBatch() leased %v, want %v", got, want)
			}
			if !test.setAside {
				assertStuckFailures(t, batch, []string{"outbox-head"}, "the update changed no row")
				if after := mustFindOutboxRow(t, repository, "outbox-head"); !reflect.DeepEqual(after, before) {
					t.Fatalf("row whose writes are skipped = %+v, want it unchanged from %+v", after, before)
				}
				return
			}
			if len(batch.Failed) != 1 || batch.Failed[0].OutboxID != "outbox-head" ||
				!strings.Contains(batch.Failed[0].Err.Error(), "the update changed no row") ||
				batch.Failed[0].SetAsideErr != nil || batch.Failed[0].RetryAt.IsZero() {
				t.Fatalf("LeaseDueBatch() failures = %+v, want the head set aside", batch.Failed)
			}
			head := mustFindOutboxRow(t, repository, "outbox-head")
			if head.State != OutboxNotDispatched || head.ErrorCode == nil || *head.ErrorCode != LeaseFailedCode ||
				head.ErrorDetail == nil || !strings.Contains(*head.ErrorDetail, "the update changed no row") {
				t.Fatalf("row whose lease a trigger skips = %+v, want it set aside with the cause", head)
			}
		})
	}
}

// A leased row whose stored values do not read back fails on itself: the call
// undoes the lease and sets the row aside. Here a schema edit lets a NOT
// NULL column hold a NULL that OutboxItem cannot scan. created_at_ms also keys
// the call's pages, so a NULL there must neither fail the page select nor hide
// the row from the page after it. All three rows come due at once, and a limit
// of 1 makes the call page past each unreadable row in turn.
func TestLeaseDueBatchSetsAsideALeasedRowThatDoesNotReadBack(t *testing.T) {
	for _, column := range []struct{ name, declaration string }{
		{"kind", "TEXT"},
		{"created_at_ms", "INTEGER"},
	} {
		t.Run(column.name, func(t *testing.T) {
			ctx := context.Background()
			clock := newOutboxTestClock(outboxTestTimeMS)
			store, repository := openOutboxTestRepository(t, clock.Now)
			for _, id := range []string{"a", "b", "c"} {
				item := outboxTestItem(id)
				item.ScheduledForMS = outboxTestTimeMS - 100
				mustEnqueueOutbox(t, repository, item)
			}
			relaxOutboxNotNull(t, store, column.name, column.declaration, "outbox-a", "outbox-b")

			batch := mustLeaseDueBatch(t, repository, clock.Now(), 1)
			if got, want := leasedOutboxIDs(batch), []string{"outbox-c"}; !slices.Equal(got, want) {
				t.Fatalf("LeaseDueBatch() leased %v, want %v", got, want)
			}
			var failed []string
			for _, failure := range batch.Failed {
				failed = append(failed, failure.OutboxID)
				if !strings.Contains(failure.Err.Error(), "stored values do not read back") ||
					failure.SetAsideErr != nil || failure.RetryAt.IsZero() {
					t.Fatalf("LeaseDueBatch() failure = %+v, want the unreadable row set aside", failure)
				}
			}
			if want := []string{"outbox-a", "outbox-b"}; !slices.Equal(failed, want) {
				t.Fatalf("LeaseDueBatch() failed %v, want %v", failed, want)
			}
			for _, failure := range batch.Failed {
				// FindByID cannot read the row either, so read the columns that
				// matter.
				var state, code string
				var leaseToken sql.NullString
				var nextAttemptAtMS int64
				if err := store.db.QueryRowContext(ctx, `
					SELECT state, error_code, lease_token, next_attempt_at_ms
					FROM outbox WHERE outbox_id = ?
				`, failure.OutboxID).Scan(&state, &code, &leaseToken, &nextAttemptAtMS); err != nil {
					t.Fatalf("read unreadable row %s: %v", failure.OutboxID, err)
				}
				if state != string(OutboxNotDispatched) || code != LeaseFailedCode || leaseToken.Valid ||
					nextAttemptAtMS != failure.RetryAt.UnixMilli() {
					t.Fatalf("unreadable row %s = state %q, code %q, lease %v, next attempt %d; want it set aside with no lease",
						failure.OutboxID, state, code, leaseToken, nextAttemptAtMS)
				}
			}
			// Every read of the row fails, the pending list's too, so the API's
			// outbox listing, retry and cancel fail while it exists, as
			// docs/agent-runbook.md tells operators. Change both together.
			if _, err := repository.FindByID(ctx, "outbox-a"); err == nil {
				t.Fatal("FindByID(unreadable row) succeeded, want the scan to fail")
			}
			if _, err := repository.ListPending(ctx, ListPendingParams{Limit: 100}); err == nil {
				t.Fatal("ListPending() with an unreadable row succeeded, want the scan to fail")
			}
		})
	}
}

// A failure that belongs to the store, not the row, still fails the whole
// call and commits nothing, not even the leases taken before it. The trigger
// raises an integer overflow (SQLITE_ERROR), not a constraint.
func TestLeaseDueBatchFailsWholeOnAStoreError(t *testing.T) {
	ctx := context.Background()
	clock := newOutboxTestClock(outboxTestTimeMS)
	store, repository := openOutboxTestRepository(t, clock.Now)
	for offset, id := range []string{"first", "second"} {
		item := outboxTestItem(id)
		item.ScheduledForMS = outboxTestTimeMS - 100 + int64(offset)
		mustEnqueueOutbox(t, repository, item)
	}
	mustExec(t, store.db, `
		CREATE TRIGGER fail_store
		BEFORE UPDATE OF state ON outbox
		WHEN NEW.state = 'dispatching' AND OLD.outbox_id = 'outbox-second'
		BEGIN
			SELECT abs(-9223372036854775807 - 1);
		END`)

	batch, err := repository.LeaseDueBatch(ctx, LeaseRequest{
		Owner: "dispatcher", Now: clock.Now(), Duration: time.Minute, Limit: 8,
	})
	if err == nil || !strings.Contains(err.Error(), "integer overflow") {
		t.Fatalf("LeaseDueBatch() error = %v, want the store's integer overflow", err)
	}
	if code, ok := sqliteErrorCode(err); !ok || code&0xff == sqliteConstraintCode || isRowLeaseFailure(err) {
		t.Fatalf("LeaseDueBatch() error code = %d (%v), want a store error, not the row's", code, ok)
	}
	if len(batch.Leases) != 0 || len(batch.Failed) != 0 {
		t.Fatalf("LeaseDueBatch() = %+v with an error, want an empty batch", batch)
	}
	assertOutboxRowsQueued(t, repository, "outbox-first", "outbox-second")
}

// A trigger's RAISE(ROLLBACK) ends the whole transaction and leaves no
// savepoint to return to, so the call fails and commits nothing. The
// connection stays usable.
func TestLeaseDueBatchFailsClosedWhenATriggerEndsTheTransaction(t *testing.T) {
	ctx := context.Background()
	clock := newOutboxTestClock(outboxTestTimeMS)
	store, repository := openOutboxTestRepository(t, clock.Now)
	for offset, id := range []string{"first", "second"} {
		item := outboxTestItem(id)
		item.ScheduledForMS = outboxTestTimeMS - 100 + int64(offset)
		mustEnqueueOutbox(t, repository, item)
	}
	mustExec(t, store.db, `
		CREATE TRIGGER end_transaction
		BEFORE UPDATE OF state ON outbox
		WHEN NEW.state = 'dispatching' AND OLD.outbox_id = 'outbox-second'
		BEGIN
			SELECT RAISE(ROLLBACK, 'injected rollback');
		END`)

	batch, err := repository.LeaseDueBatch(ctx, LeaseRequest{
		Owner: "dispatcher", Now: clock.Now(), Duration: time.Minute, Limit: 8,
	})
	if err == nil || !strings.Contains(err.Error(), "injected rollback") {
		t.Fatalf("LeaseDueBatch() error = %v, want the rollback", err)
	}
	if len(batch.Leases) != 0 || len(batch.Failed) != 0 {
		t.Fatalf("LeaseDueBatch() = %+v with an error, want an empty batch", batch)
	}
	assertOutboxRowsQueued(t, repository, "outbox-first", "outbox-second")

	mustExec(t, store.db, `DROP TRIGGER end_transaction`)
	recovered := mustLeaseDueBatch(t, repository, clock.Now(), 8)
	if got, want := leasedOutboxIDs(recovered), []string{"outbox-first", "outbox-second"}; !slices.Equal(got, want) {
		t.Fatalf("LeaseDueBatch() after the trigger is gone leased %v, want %v", got, want)
	}
}

func mustLeaseDueBatch(t *testing.T, repository *OutboxRepository, now time.Time, limit int) LeaseBatch {
	t.Helper()
	batch, err := repository.LeaseDueBatch(context.Background(), LeaseRequest{
		Owner: "dispatcher", Now: now, Duration: time.Minute, Limit: limit,
	})
	if err != nil {
		t.Fatalf("LeaseDueBatch(): %v", err)
	}
	return batch
}

func leasedOutboxIDs(batch LeaseBatch) []string {
	ids := make([]string, 0, len(batch.Leases))
	for _, lease := range batch.Leases {
		ids = append(ids, lease.OutboxID)
	}
	return ids
}

// assertStuckFailures checks that batch reports ids, in order and no others,
// as rows the call could neither lease nor set aside, both for cause.
func assertStuckFailures(t *testing.T, batch LeaseBatch, ids []string, cause string) {
	t.Helper()
	if len(batch.Failed) != len(ids) {
		t.Fatalf("LeaseDueBatch() failures = %+v, want %v", batch.Failed, ids)
	}
	for index, failure := range batch.Failed {
		if failure.OutboxID != ids[index] ||
			failure.Err == nil || !strings.Contains(failure.Err.Error(), cause) ||
			failure.SetAsideErr == nil || !strings.Contains(failure.SetAsideErr.Error(), cause) ||
			!failure.RetryAt.IsZero() {
			t.Fatalf("failure %d = %+v, want %q neither leased nor set aside for %q", index, failure, ids[index], cause)
		}
	}
}

func assertOutboxRowsQueued(t *testing.T, repository *OutboxRepository, ids ...string) {
	t.Helper()
	for _, id := range ids {
		row := mustFindOutboxRow(t, repository, id)
		if row.State != OutboxQueued || row.LeaseToken != nil || row.ErrorCode != nil {
			t.Fatalf("row %q = %+v, want it still queued with no lease or error", id, row)
		}
	}
}

func mustFindOutboxRow(t *testing.T, repository *OutboxRepository, id string) OutboxItem {
	t.Helper()
	row, err := repository.FindByID(context.Background(), id)
	if err != nil {
		t.Fatalf("FindByID(%q): %v", id, err)
	}
	return row
}

// relaxOutboxNotNull drops the NOT NULL from one outbox column with a schema
// edit, as a botched table rebuild could, and then stores a NULL in that
// column of each named row. Everything runs on one connection, since
// writable_schema holds per connection.
func relaxOutboxNotNull(t *testing.T, store *Store, column, declaration string, outboxIDs ...string) {
	t.Helper()
	ctx := context.Background()
	withPinnedConnection(t, store, func(conn *sql.Conn) {
		var schema string
		if err := conn.QueryRowContext(ctx,
			`SELECT sql FROM sqlite_schema WHERE type = 'table' AND name = 'outbox'`,
		).Scan(&schema); err != nil {
			t.Fatalf("read outbox schema: %v", err)
		}
		notNull := regexp.MustCompile(`(?m)^(\s*` + column + `\s+` + declaration + `) NOT NULL`)
		if !notNull.MatchString(schema) {
			t.Fatalf("outbox schema has no NOT NULL %s column to relax:\n%s", column, schema)
		}
		var version int64
		if err := conn.QueryRowContext(ctx, `PRAGMA schema_version`).Scan(&version); err != nil {
			t.Fatalf("read schema version: %v", err)
		}
		mustExecConn(t, conn, `PRAGMA writable_schema = ON`)
		mustExecConn(t, conn,
			`UPDATE sqlite_schema SET sql = ? WHERE type = 'table' AND name = 'outbox'`,
			notNull.ReplaceAllString(schema, "$1"),
		)
		mustExecConn(t, conn, fmt.Sprintf(`PRAGMA schema_version = %d`, version+1))
		mustExecConn(t, conn, `PRAGMA writable_schema = RESET`)
		for _, id := range outboxIDs {
			mustExecConn(t, conn, `UPDATE outbox SET `+column+` = NULL WHERE outbox_id = ?`, id)
		}
	})
}

// withPinnedConnection runs fn on one connection of the store's pool, for
// pragmas that hold per connection.
func withPinnedConnection(t *testing.T, store *Store, fn func(*sql.Conn)) {
	t.Helper()
	conn, err := store.db.Conn(context.Background())
	if err != nil {
		t.Fatalf("pin a store connection: %v", err)
	}
	defer conn.Close()
	fn(conn)
}

func mustExecConn(t *testing.T, conn *sql.Conn, statement string, arguments ...any) {
	t.Helper()
	if _, err := conn.ExecContext(context.Background(), statement, arguments...); err != nil {
		t.Fatalf("execute fixture SQL %q: %v", statement, err)
	}
}
