package sqlite

import (
	"context"
	"database/sql"
	"reflect"
	"strings"
	"testing"
	"time"
)

// strandLease leaves the row id with a dispatch lease that expires at
// expiresAtMS, as a pass that crashed would: leased at the clock's time and,
// when called, past MarkTransportCalled. The row must be the only due row.
func strandLease(
	t *testing.T,
	repository *OutboxRepository,
	clock *outboxTestClock,
	id string,
	called bool,
	expiresAtMS int64,
) OutboxItem {
	t.Helper()
	ctx := context.Background()
	item := outboxTestItem(id)
	item.ScheduledForMS = clock.Now().UnixMilli()
	mustEnqueueOutbox(t, repository, item)
	leased := mustLeaseOne(t, repository, LeaseRequest{
		Owner:    "crashed-pass",
		Now:      clock.Now(),
		Duration: time.UnixMilli(expiresAtMS).Sub(clock.Now()),
		Limit:    1,
	})
	if called {
		if err := repository.MarkTransportCalled(ctx, Attempt{
			OutboxID: leased.OutboxID, LeaseToken: mustLeaseToken(t, leased),
		}); err != nil {
			t.Fatalf("MarkTransportCalled(%q): %v", leased.OutboxID, err)
		}
	}
	return mustFindOutboxRow(t, repository, leased.OutboxID)
}

// recoveryFailureTrigger fails the recovery of each named row, the way a
// CHECK the row can no longer meet would, and lets every other write through.
func recoveryFailureTrigger(outboxIDs ...string) string {
	return `
		CREATE TRIGGER fail_recovery
		BEFORE UPDATE OF state ON outbox
		WHEN OLD.state = 'dispatching' AND NEW.state IN ('not_dispatched', 'uncertain')
			AND OLD.outbox_id IN ('` + strings.Join(outboxIDs, "', '") + `')
		BEGIN
			SELECT RAISE(ABORT, 'injected recovery failure');
		END`
}

// One expired lease whose recovery always fails must not hold the recovery
// of the others. RecoverExpiredLeases rewrote every expired lease in two
// multi-row UPDATEs in one transaction, so the first row it could not write
// rolled every other recovery back, and every call failed the same way.
func TestRecoverExpiredLeasesRecoversPastALeaseWhoseRecoveryFails(t *testing.T) {
	ctx := context.Background()
	clock := newOutboxTestClock(outboxTestTimeMS)
	store, repository := openOutboxTestRepository(t, clock.Now)
	// Expiries ascend, so the call meets the rows in this order.
	failingUncalled := strandLease(t, repository, clock, "failing-uncalled", false, outboxTestTimeMS+1_000)
	uncalled := strandLease(t, repository, clock, "uncalled", false, outboxTestTimeMS+1_001)
	failingCalled := strandLease(t, repository, clock, "failing-called", true, outboxTestTimeMS+1_002)
	called := strandLease(t, repository, clock, "called", true, outboxTestTimeMS+1_003)
	unexpired := strandLease(t, repository, clock, "unexpired", true, outboxTestTimeMS+60_000)
	mustExec(t, store.db, recoveryFailureTrigger(failingUncalled.OutboxID, failingCalled.OutboxID))

	nowMS := outboxTestTimeMS + 2_000
	clock.Set(nowMS)
	for attempt := range 2 {
		recovered, err := repository.RecoverExpiredLeases(ctx, clock.Now())
		if err != nil {
			t.Fatalf("RecoverExpiredLeases() %d with two leases whose recovery fails: %v", attempt, err)
		}
		// The first call recovers one healthy lease of each kind; the second
		// finds only the two it could not recover.
		wantRecovered := 0
		if attempt == 0 {
			wantRecovered = 1
		}
		if recovered.NotDispatched != wantRecovered || recovered.Uncertain != wantRecovered {
			t.Fatalf("RecoverExpiredLeases() %d = %+v, want %d of each recovered", attempt, recovered, wantRecovered)
		}
		assertRecoveryFailures(t, recovered, []LeaseRecoveryFailure{
			{OutboxID: failingUncalled.OutboxID},
			{OutboxID: failingCalled.OutboxID, TransportCalled: true},
		}, "injected recovery failure")
		for _, failing := range []OutboxItem{failingUncalled, failingCalled} {
			if got := mustFindOutboxRow(t, repository, failing.OutboxID); !reflect.DeepEqual(got, failing) {
				t.Fatalf("lease whose recovery fails = %+v, want it as it was, %+v", got, failing)
			}
		}
	}

	assertRecoveredPreCall(t, repository, uncalled, nowMS)
	assertRecoveredPostCall(t, repository, called, nowMS)
	if got := mustFindOutboxRow(t, repository, unexpired.OutboxID); !reflect.DeepEqual(got, unexpired) {
		t.Fatalf("unexpired lease = %+v, want it as it was, %+v", got, unexpired)
	}
	// A row the call left as it was is never due: nothing leases or sends it.
	if batch := mustLeaseDueBatch(t, repository, clock.Now().Add(time.Hour), 8); len(batch.Failed) != 0 ||
		len(batch.Leases) != 1 || batch.Leases[0].OutboxID != uncalled.OutboxID {
		t.Fatalf("LeaseDueBatch() = %+v, want only the recovered uncalled row", batch)
	}

	// Once the cause is gone, the next call recovers each row as it would
	// have the first time.
	mustExec(t, store.db, `DROP TRIGGER fail_recovery`)
	nowMS += 1_000
	clock.Set(nowMS)
	recovered, err := repository.RecoverExpiredLeases(ctx, clock.Now())
	if err != nil || recovered.NotDispatched != 1 || recovered.Uncertain != 1 || len(recovered.Failed) != 0 {
		t.Fatalf("RecoverExpiredLeases() after the trigger is gone = %+v, %v; want one of each recovered", recovered, err)
	}
	assertRecoveredPreCall(t, repository, failingUncalled, nowMS)
	assertRecoveredPostCall(t, repository, failingCalled, nowMS)
}

// A real CHECK fails the same way: this row's updated_at_ms sits below its
// created_at_ms, which only a write that skipped the checks could leave, and
// the clamped recovery write keeps it there.
func TestRecoverExpiredLeasesRecoversPastALeaseThatBreaksACheck(t *testing.T) {
	ctx := context.Background()
	clock := newOutboxTestClock(outboxTestTimeMS)
	store, repository := openOutboxTestRepository(t, clock.Now)
	strandLease(t, repository, clock, "broken", false, outboxTestTimeMS+1_000)
	healthy := strandLease(t, repository, clock, "healthy", false, outboxTestTimeMS+1_001)
	withPinnedConnection(t, store, func(conn *sql.Conn) {
		mustExecConn(t, conn, `PRAGMA ignore_check_constraints = ON`)
		mustExecConn(t, conn, `
			UPDATE outbox SET created_at_ms = ?, updated_at_ms = ?
			WHERE outbox_id = 'outbox-broken'
		`, outboxTestTimeMS+5_000, outboxTestTimeMS+4_000)
		mustExecConn(t, conn, `PRAGMA ignore_check_constraints = OFF`)
	})
	broken := mustFindOutboxRow(t, repository, "outbox-broken")

	nowMS := outboxTestTimeMS + 2_000
	clock.Set(nowMS)
	recovered, err := repository.RecoverExpiredLeases(ctx, clock.Now())
	if err != nil || recovered.NotDispatched != 1 || recovered.Uncertain != 0 {
		t.Fatalf("RecoverExpiredLeases() = %+v, %v; want the healthy lease recovered", recovered, err)
	}
	assertRecoveryFailures(t, recovered, []LeaseRecoveryFailure{{OutboxID: broken.OutboxID}}, "CHECK constraint failed")
	if got := mustFindOutboxRow(t, repository, broken.OutboxID); !reflect.DeepEqual(got, broken) {
		t.Fatalf("lease that breaks a CHECK = %+v, want it as it was, %+v", got, broken)
	}
	assertRecoveredPreCall(t, repository, healthy, nowMS)
}

// A trigger that skips a recovery write (RAISE(IGNORE)) changes nothing, so
// the UPDATE affects no row. That failure belongs to the row too.
func TestRecoverExpiredLeasesRecoversPastALeaseWhoseRecoveryATriggerSkips(t *testing.T) {
	ctx := context.Background()
	clock := newOutboxTestClock(outboxTestTimeMS)
	store, repository := openOutboxTestRepository(t, clock.Now)
	skipped := strandLease(t, repository, clock, "skipped", true, outboxTestTimeMS+1_000)
	healthy := strandLease(t, repository, clock, "healthy", true, outboxTestTimeMS+1_001)
	mustExec(t, store.db, `
		CREATE TRIGGER skip_recovery BEFORE UPDATE ON outbox
		WHEN OLD.outbox_id = 'outbox-skipped'
		BEGIN SELECT RAISE(IGNORE); END`)

	nowMS := outboxTestTimeMS + 2_000
	clock.Set(nowMS)
	recovered, err := repository.RecoverExpiredLeases(ctx, clock.Now())
	if err != nil || recovered.NotDispatched != 0 || recovered.Uncertain != 1 {
		t.Fatalf("RecoverExpiredLeases() = %+v, %v; want the healthy called lease recovered", recovered, err)
	}
	assertRecoveryFailures(t, recovered, []LeaseRecoveryFailure{
		{OutboxID: skipped.OutboxID, TransportCalled: true},
	}, "the update changed no row")
	if got := mustFindOutboxRow(t, repository, skipped.OutboxID); !reflect.DeepEqual(got, skipped) {
		t.Fatalf("lease whose recovery a trigger skips = %+v, want it as it was, %+v", got, skipped)
	}
	assertRecoveredPostCall(t, repository, healthy, nowMS)
}

// A failed recovery write leaves nothing behind, even what it wrote before it
// failed. Here the trigger writes a row of its own and then fails the write
// with RAISE(FAIL), which, unlike ABORT, keeps what the statement changed
// before the failure. Only the savepoint undoes that write.
func TestRecoverExpiredLeasesUndoesEverythingAFailedRecoveryWrote(t *testing.T) {
	ctx := context.Background()
	clock := newOutboxTestClock(outboxTestTimeMS)
	store, repository := openOutboxTestRepository(t, clock.Now)
	failing := strandLease(t, repository, clock, "failing", false, outboxTestTimeMS+1_000)
	healthy := strandLease(t, repository, clock, "healthy", true, outboxTestTimeMS+1_001)
	mustExec(t, store.db, `CREATE TABLE recovery_side_writes (outbox_id TEXT NOT NULL) STRICT`)
	mustExec(t, store.db, `
		CREATE TRIGGER fail_recovery_after_a_write
		BEFORE UPDATE OF state ON outbox
		WHEN OLD.outbox_id = 'outbox-failing'
		BEGIN
			INSERT INTO recovery_side_writes (outbox_id) VALUES (OLD.outbox_id);
			SELECT RAISE(FAIL, 'injected recovery failure');
		END`)

	nowMS := outboxTestTimeMS + 2_000
	clock.Set(nowMS)
	recovered, err := repository.RecoverExpiredLeases(ctx, clock.Now())
	if err != nil || recovered.NotDispatched != 0 || recovered.Uncertain != 1 {
		t.Fatalf("RecoverExpiredLeases() = %+v, %v; want the healthy called lease recovered", recovered, err)
	}
	assertRecoveryFailures(t, recovered, []LeaseRecoveryFailure{{OutboxID: failing.OutboxID}}, "injected recovery failure")
	var sideWrites int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM recovery_side_writes`).Scan(&sideWrites); err != nil {
		t.Fatalf("count side writes: %v", err)
	}
	if sideWrites != 0 {
		t.Fatalf("the failed recovery left %d side writes, want none", sideWrites)
	}
	if got := mustFindOutboxRow(t, repository, failing.OutboxID); !reflect.DeepEqual(got, failing) {
		t.Fatalf("lease whose recovery failed after a write = %+v, want it as it was, %+v", got, failing)
	}
	assertRecoveredPostCall(t, repository, healthy, nowMS)
}

// A failure that belongs to the store, not the row (here an integer overflow,
// SQLITE_ERROR), fails the whole call and commits nothing, the recovery of
// the lease the call met first included.
func TestRecoverExpiredLeasesFailsWholeOnAStoreError(t *testing.T) {
	ctx := context.Background()
	clock := newOutboxTestClock(outboxTestTimeMS)
	store, repository := openOutboxTestRepository(t, clock.Now)
	first := strandLease(t, repository, clock, "first", false, outboxTestTimeMS+1_000)
	second := strandLease(t, repository, clock, "second", true, outboxTestTimeMS+1_001)
	mustExec(t, store.db, `
		CREATE TRIGGER fail_store
		BEFORE UPDATE OF state ON outbox
		WHEN OLD.outbox_id = 'outbox-second'
		BEGIN
			SELECT abs(-9223372036854775807 - 1);
		END`)

	clock.Set(outboxTestTimeMS + 2_000)
	recovered, err := repository.RecoverExpiredLeases(ctx, clock.Now())
	if err == nil || !strings.Contains(err.Error(), "integer overflow") {
		t.Fatalf("RecoverExpiredLeases() error = %v, want the store's integer overflow", err)
	}
	if code, ok := sqliteErrorCode(err); !ok || code&0xff == sqliteConstraintCode || isRowLeaseFailure(err) {
		t.Fatalf("RecoverExpiredLeases() error code = %d (%v), want a store error, not the row's", code, ok)
	}
	if recovered.NotDispatched != 0 || recovered.Uncertain != 0 || len(recovered.Failed) != 0 {
		t.Fatalf("RecoverExpiredLeases() = %+v with an error, want nothing reported", recovered)
	}
	for _, stranded := range []OutboxItem{first, second} {
		if got := mustFindOutboxRow(t, repository, stranded.OutboxID); !reflect.DeepEqual(got, stranded) {
			t.Fatalf("lease after a store error = %+v, want it as it was, %+v", got, stranded)
		}
	}
}

// A trigger's RAISE(ROLLBACK) ends the whole transaction and leaves no
// savepoint to return to, so the call fails and commits nothing. The
// connection stays usable.
func TestRecoverExpiredLeasesFailsClosedWhenATriggerEndsTheTransaction(t *testing.T) {
	ctx := context.Background()
	clock := newOutboxTestClock(outboxTestTimeMS)
	store, repository := openOutboxTestRepository(t, clock.Now)
	first := strandLease(t, repository, clock, "first", false, outboxTestTimeMS+1_000)
	second := strandLease(t, repository, clock, "second", true, outboxTestTimeMS+1_001)
	mustExec(t, store.db, `
		CREATE TRIGGER end_transaction
		BEFORE UPDATE OF state ON outbox
		WHEN OLD.outbox_id = 'outbox-second'
		BEGIN
			SELECT RAISE(ROLLBACK, 'injected rollback');
		END`)

	nowMS := outboxTestTimeMS + 2_000
	clock.Set(nowMS)
	recovered, err := repository.RecoverExpiredLeases(ctx, clock.Now())
	if err == nil || !strings.Contains(err.Error(), "injected rollback") {
		t.Fatalf("RecoverExpiredLeases() error = %v, want the rollback", err)
	}
	if recovered.NotDispatched != 0 || recovered.Uncertain != 0 || len(recovered.Failed) != 0 {
		t.Fatalf("RecoverExpiredLeases() = %+v with an error, want nothing reported", recovered)
	}
	for _, stranded := range []OutboxItem{first, second} {
		if got := mustFindOutboxRow(t, repository, stranded.OutboxID); !reflect.DeepEqual(got, stranded) {
			t.Fatalf("lease after a rollback = %+v, want it as it was, %+v", got, stranded)
		}
	}

	mustExec(t, store.db, `DROP TRIGGER end_transaction`)
	recovered, err = repository.RecoverExpiredLeases(ctx, clock.Now())
	if err != nil || recovered.NotDispatched != 1 || recovered.Uncertain != 1 || len(recovered.Failed) != 0 {
		t.Fatalf("RecoverExpiredLeases() after the trigger is gone = %+v, %v; want both recovered", recovered, err)
	}
	assertRecoveredPreCall(t, repository, first, nowMS)
	assertRecoveredPostCall(t, repository, second, nowMS)
}

// assertRecoveryFailures checks that recovered reports want's rows, in order
// and no others, each failing for cause.
func assertRecoveryFailures(t *testing.T, recovered RecoveredLeases, want []LeaseRecoveryFailure, cause string) {
	t.Helper()
	if len(recovered.Failed) != len(want) {
		t.Fatalf("RecoverExpiredLeases() failures = %+v, want %+v", recovered.Failed, want)
	}
	for index, failure := range recovered.Failed {
		if failure.OutboxID != want[index].OutboxID || failure.TransportCalled != want[index].TransportCalled ||
			failure.Err == nil || !strings.Contains(failure.Err.Error(), cause) {
			t.Fatalf("failure %d = %+v, want %+v failing for %q", index, failure, want[index], cause)
		}
	}
}

// assertRecoveredPreCall checks that recovery at nowMS gave the expired lease
// whose call never started today's outcome: not_dispatched, due at once, with
// its lease ended and nothing else changed.
func assertRecoveredPreCall(t *testing.T, repository *OutboxRepository, was OutboxItem, nowMS int64) {
	t.Helper()
	want := was
	want.State = OutboxNotDispatched
	want.NextAttemptAtMS = &nowMS
	want.LeaseOwner, want.LeaseToken, want.LeaseExpiresAtMS, want.TransportCalledAtMS = nil, nil, nil, nil
	want.UpdatedAtMS = max(was.UpdatedAtMS, nowMS)
	if got := mustFindOutboxRow(t, repository, was.OutboxID); !reflect.DeepEqual(got, want) {
		t.Fatalf("recovered uncalled lease = %+v, want %+v", got, want)
	}
}

// assertRecoveredPostCall checks that recovery at nowMS gave the expired lease
// whose call started today's outcome: uncertain, never due again, with its
// lease ended, its attempt kept and nothing else changed.
func assertRecoveredPostCall(t *testing.T, repository *OutboxRepository, was OutboxItem, nowMS int64) {
	t.Helper()
	want := was
	want.State = OutboxUncertain
	want.NextAttemptAtMS = nil
	want.LeaseOwner, want.LeaseToken, want.LeaseExpiresAtMS, want.TransportCalledAtMS = nil, nil, nil, nil
	want.UpdatedAtMS = max(was.UpdatedAtMS, nowMS)
	if got := mustFindOutboxRow(t, repository, was.OutboxID); !reflect.DeepEqual(got, want) {
		t.Fatalf("recovered called lease = %+v, want %+v", got, want)
	}
}
