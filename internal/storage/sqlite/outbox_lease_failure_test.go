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

	// The row is set aside until 5 minutes from now. A manual retry makes it
	// due at once, and its next failed lease waits the first delay again.
	clock.Set(clock.Now().Add(-4 * time.Minute).UnixMilli())
	if err := repository.RetryNotDispatched(ctx, "outbox-head"); err != nil {
		t.Fatalf("RetryNotDispatched(): %v", err)
	}
	if delay := setAside(); delay != 5*time.Second {
		t.Fatalf("delay after a manual retry = %v, want 5s", delay)
	}
}

// A row that can be neither leased nor set aside stays as it was, still due,
// and the call leases the rows behind it however many such rows come first.
func TestLeaseDueBatchLeasesPastRowsThatCanBeNeitherLeasedNorSetAside(t *testing.T) {
	clock := newOutboxTestClock(outboxTestTimeMS)
	store, repository := openOutboxTestRepository(t, clock.Now)
	for offset, id := range []string{"a", "b", "c", "d", "e"} {
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

	// The limit is 2, so the first select returns only rows that fail.
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

	again := mustLeaseDueBatch(t, repository, clock.Now(), 2)
	if len(again.Leases) != 0 {
		t.Fatalf("LeaseDueBatch() again leased %v, want nothing", leasedOutboxIDs(again))
	}
	assertStuckFailures(t, again, stuck, "injected write failure")
}

// A row whose writes a real CHECK refuses is handled like one a trigger
// refuses. This row's updated_at_ms is below its created_at_ms, which the
// CHECK forbids, so every write to it fails until something repairs it.
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
// lease UPDATE affects no row. That is the row's failure too: the row is set
// aside, or left as it was when its set-aside is skipped as well.
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

// A leased row whose stored values do not read back is the row's failure: the
// lease is undone and the row set aside. Here a schema edit lets kind hold a
// NULL that OutboxItem cannot scan.
func TestLeaseDueBatchSetsAsideALeasedRowThatDoesNotReadBack(t *testing.T) {
	ctx := context.Background()
	clock := newOutboxTestClock(outboxTestTimeMS)
	store, repository := openOutboxTestRepository(t, clock.Now)
	for offset, id := range []string{"head", "second"} {
		item := outboxTestItem(id)
		item.ScheduledForMS = outboxTestTimeMS - 100 + int64(offset)
		mustEnqueueOutbox(t, repository, item)
	}
	withPinnedConnection(t, store, func(conn *sql.Conn) {
		var schema string
		if err := conn.QueryRowContext(ctx,
			`SELECT sql FROM sqlite_schema WHERE type = 'table' AND name = 'outbox'`,
		).Scan(&schema); err != nil {
			t.Fatalf("read outbox schema: %v", err)
		}
		notNullKind := regexp.MustCompile(`(?m)^(\s*kind\s+TEXT) NOT NULL`)
		if !notNullKind.MatchString(schema) {
			t.Fatalf("outbox schema has no NOT NULL kind column to relax:\n%s", schema)
		}
		var version int64
		if err := conn.QueryRowContext(ctx, `PRAGMA schema_version`).Scan(&version); err != nil {
			t.Fatalf("read schema version: %v", err)
		}
		mustExecConn(t, conn, `PRAGMA writable_schema = ON`)
		mustExecConn(t, conn,
			`UPDATE sqlite_schema SET sql = ? WHERE type = 'table' AND name = 'outbox'`,
			notNullKind.ReplaceAllString(schema, "$1"),
		)
		mustExecConn(t, conn, fmt.Sprintf(`PRAGMA schema_version = %d`, version+1))
		mustExecConn(t, conn, `PRAGMA writable_schema = RESET`)
		mustExecConn(t, conn, `UPDATE outbox SET kind = NULL WHERE outbox_id = 'outbox-head'`)
	})

	batch := mustLeaseDueBatch(t, repository, clock.Now(), 8)
	if got, want := leasedOutboxIDs(batch), []string{"outbox-second"}; !slices.Equal(got, want) {
		t.Fatalf("LeaseDueBatch() leased %v, want %v", got, want)
	}
	if len(batch.Failed) != 1 || batch.Failed[0].OutboxID != "outbox-head" ||
		!strings.Contains(batch.Failed[0].Err.Error(), "stored values do not read back") ||
		batch.Failed[0].SetAsideErr != nil || batch.Failed[0].RetryAt.IsZero() {
		t.Fatalf("LeaseDueBatch() failures = %+v, want the unreadable head set aside", batch.Failed)
	}
	// FindByID cannot read the row either, so read the columns that matter.
	var state, code string
	var leaseToken sql.NullString
	var nextAttemptAtMS int64
	if err := store.db.QueryRowContext(ctx, `
		SELECT state, error_code, lease_token, next_attempt_at_ms
		FROM outbox WHERE outbox_id = 'outbox-head'
	`).Scan(&state, &code, &leaseToken, &nextAttemptAtMS); err != nil {
		t.Fatalf("read the unreadable row's state: %v", err)
	}
	if state != string(OutboxNotDispatched) || code != LeaseFailedCode || leaseToken.Valid ||
		nextAttemptAtMS != batch.Failed[0].RetryAt.UnixMilli() {
		t.Fatalf("unreadable row = state %q, code %q, lease %v, next attempt %d; want it set aside with no lease",
			state, code, leaseToken, nextAttemptAtMS)
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

// assertStuckFailures checks that batch reports exactly ids, in order, as rows
// that could be neither leased nor set aside, both for cause.
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
