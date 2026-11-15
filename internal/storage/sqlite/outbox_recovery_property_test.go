package sqlite

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"maps"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/quick"
)

// recoveryRowFault says how the recovery write of one expired lease fails in
// a generated history.
type recoveryRowFault int

const (
	recoveryRowHealthy recoveryRowFault = iota
	// recoveryRowAborts: a trigger aborts the row's recovery write, as a
	// constraint the row can no longer meet would.
	recoveryRowAborts
	// recoveryRowSkipped: a trigger skips the row's recovery write
	// (RAISE(IGNORE)).
	recoveryRowSkipped
	// recoveryRowBreaksCheck: the row's created_at_ms lies past its
	// updated_at_ms and past every call's time, as only a write that skipped
	// the checks could leave it, so the recovery write breaks the CHECK
	// updated_at_ms >= created_at_ms.
	recoveryRowBreaksCheck
	recoveryRowFaultCount
)

// cause returns text that the error of every recovery the fault fails
// contains.
func (fault recoveryRowFault) cause() string {
	switch fault {
	case recoveryRowAborts:
		return "injected recovery failure"
	case recoveryRowSkipped:
		return "the update changed no row"
	case recoveryRowBreaksCheck:
		return "CHECK constraint failed"
	default:
		return ""
	}
}

// Times in a recovery script, after the time every row is written.
const (
	// recoveryExpiryBaseMS is the earliest lease expiry. Expiry slots 0 to 4
	// lie a second apart from it.
	recoveryExpiryBaseMS = 10_000
	// recoveryFarExpiryMS is the expiry of slot 5, which only the final call
	// reaches.
	recoveryFarExpiryMS = 1_000_000
	// recoveryUpdatedAheadMS is the updated_at_ms of a row that a writer
	// whose clock ran ahead wrote last: past the time of every call but the
	// final one.
	recoveryUpdatedAheadMS = 1_500_000
	// recoveryFinalCallMS is the time of the call after the faults clear,
	// past every expiry.
	recoveryFinalCallMS = 2_000_000
	// recoveryBrokenCreatedMS is the created_at_ms of a row that breaks the
	// CHECK, past the time of every call.
	recoveryBrokenCreatedMS = 5_000_000
)

// recoveryScript is one generated history of RecoverExpiredLeases calls over
// rows that crashed dispatch passes left leased. Leases expire at one of a
// few times, so they often tie on expiry, and the outbox ids, which follow
// neither the order the rows were written in nor their faults, break the
// tie. Each script draws its own share of rows whose recovery fails. Between
// calls the clock moves by amounts that fall short of, meet or pass the next
// expiry, and sometimes back, as a wall clock stepped back does.
type recoveryScript struct {
	Rows  []recoveryRow
	Calls []recoveryCall
}

type recoveryRow struct {
	ID string
	// Leased: a crashed pass left the row dispatching. Otherwise the row
	// stays queued for a time after every call, and no call meets it.
	Leased bool
	// Called: the lease's transport call had started.
	Called bool
	// ExpirySlot picks the lease's expiry: slots 0 to 4 a second apart from
	// recoveryExpiryBaseMS, slot 5 recoveryFarExpiryMS.
	ExpirySlot int64
	// UpdatedAhead sets updated_at_ms to recoveryUpdatedAheadMS, so the
	// recovery write keeps it.
	UpdatedAhead bool
	Fault        recoveryRowFault
}

type recoveryCall struct {
	AdvanceMS int64
	// StoreFault, when not empty, names a healthy leased row whose recovery
	// write fails the store in this call: with an integer overflow
	// (SQLITE_ERROR), or, when Rollback is set, with a RAISE(ROLLBACK) that
	// ends the transaction.
	StoreFault string
	Rollback   bool
}

func (recoveryScript) Generate(r *rand.Rand, _ int) reflect.Value {
	count := 1 + r.Intn(30)
	failingShare := r.Float64()
	ids := r.Perm(count)
	script := recoveryScript{Rows: make([]recoveryRow, count)}
	var healthy []string
	for index := range script.Rows {
		leased := r.Intn(8) != 0
		row := recoveryRow{
			ID:           fmt.Sprintf("outbox-r%02d", ids[index]),
			Leased:       leased,
			Called:       leased && r.Intn(2) == 0,
			ExpirySlot:   int64(r.Intn(6)),
			UpdatedAhead: r.Intn(4) == 0,
		}
		if leased && r.Float64() < failingShare {
			row.Fault = recoveryRowFault(1 + r.Intn(int(recoveryRowFaultCount)-1))
		}
		if leased && row.Fault == recoveryRowHealthy {
			healthy = append(healthy, row.ID)
		}
		script.Rows[index] = row
	}
	// The clock starts a second before the earliest expiry, so the first
	// calls often meet no lease or some of a tied slot, and later ones pass
	// the next slots.
	advances := []int64{-1_500, 0, 400, 999, 1_000, 1_001, 2_000, 4_000}
	for range 1 + r.Intn(10) {
		call := recoveryCall{AdvanceMS: advances[r.Intn(len(advances))]}
		if len(healthy) > 0 && r.Intn(4) == 0 {
			call.StoreFault = healthy[r.Intn(len(healthy))]
			call.Rollback = r.Intn(2) == 0
		}
		script.Calls = append(script.Calls, call)
	}
	return reflect.ValueOf(script)
}

// TestRecoverExpiredLeasesInvariantsHoldForAnyLeasesAndFaults checks
// RecoverExpiredLeases against a model over generated histories of expired
// leases, any of whose recovery writes may fail in any of the ways a row's
// write can fail. The model knows only what the script says of each row and
// what the earlier calls did: it walks the expired leases in expiry order,
// recovers each lease whose write works and reports each whose write fails.
// The test reads every column of every row before and after each call. So,
// in every call:
//
//   - the call meets each lease that has expired by its time once, in order
//     of expiry and then outbox id, and no other row: each lease it meets
//     comes back once, as recovered or as a failure;
//   - it ends each expired lease whose transport call never started with the
//     row not_dispatched and due at once, and each whose call started with
//     the row uncertain, and changes no other column, attempt_count
//     included, but updated_at_ms, which never falls;
//   - a row whose transport call started never comes due again, whatever
//     fails;
//   - a lease whose recovery write fails on the row itself stays as it was,
//     byte for byte, comes back as a failure with its cause, and does not
//     stop the call from recovering the leases after it;
//   - every row the call does not meet stays as it was;
//   - a store error, or a trigger that ends the transaction, fails the whole
//     call and changes no row;
//   - once the faults clear, one call recovers every lease that no call has
//     recovered yet, so no failure strands an intent.
func TestRecoverExpiredLeasesInvariantsHoldForAnyLeasesAndFaults(t *testing.T) {
	dir := t.TempDir()
	property := func(script recoveryScript) bool {
		if err := runRecoveryScript(t, dir, script); err != nil {
			t.Logf("script %+v: %v", script, err)
			return false
		}
		return true
	}
	if err := quick.Check(property, storagePropertyConfig(t, 20261105, 100, 8)); err != nil {
		t.Fatal(err)
	}
}

func runRecoveryScript(t *testing.T, dir string, script recoveryScript) error {
	ctx := context.Background()
	caseDir, err := os.MkdirTemp(dir, "case-")
	if err != nil {
		t.Fatalf("make case directory: %v", err)
	}
	store, err := Open(filepath.Join(caseDir, "store.sqlite3"))
	if err != nil {
		t.Fatalf("Open(): %v", err)
	}
	defer store.Close()
	seedMessageAccount(t, store, "account-a", "test")
	clock := newOutboxTestClock(outboxTestTimeMS)
	repository, err := NewOutboxRepository(store, clock.Now)
	if err != nil {
		t.Fatalf("NewOutboxRepository(): %v", err)
	}

	model := recoveryModel{rows: make(map[string]*recoveryModelRow)}
	byFault := make(map[recoveryRowFault][]string)
	var updatedAhead []string
	for _, row := range script.Rows {
		id := strings.TrimPrefix(row.ID, "outbox-")
		modelRow := &recoveryModelRow{id: row.ID, leased: row.Leased, called: row.Called, fault: row.Fault}
		if row.Leased {
			modelRow.expiresAtMS = outboxTestTimeMS + recoveryExpiryBaseMS + row.ExpirySlot*1_000
			if row.ExpirySlot == 5 {
				modelRow.expiresAtMS = outboxTestTimeMS + recoveryFarExpiryMS
			}
			strandLease(t, repository, clock, id, row.Called, modelRow.expiresAtMS)
		} else {
			item := outboxTestItem(id)
			item.ScheduledForMS = outboxTestTimeMS + 100_000_000
			mustEnqueueOutbox(t, repository, item)
		}
		model.rows[row.ID] = modelRow
		byFault[row.Fault] = append(byFault[row.Fault], row.ID)
		if row.UpdatedAhead {
			updatedAhead = append(updatedAhead, row.ID)
		}
	}
	inList := func(ids []string) string { return "'" + strings.Join(ids, "', '") + "'" }
	if len(updatedAhead) > 0 {
		mustExec(t, store.db, `UPDATE outbox SET updated_at_ms = ? WHERE outbox_id IN (`+inList(updatedAhead)+`)`,
			outboxTestTimeMS+recoveryUpdatedAheadMS)
	}
	written, err := readOutboxRawRows(ctx, store.db)
	if err != nil {
		t.Fatalf("read rows as written: %v", err)
	}
	if ids := byFault[recoveryRowBreaksCheck]; len(ids) > 0 {
		withPinnedConnection(t, store, func(conn *sql.Conn) {
			mustExecConn(t, conn, `PRAGMA ignore_check_constraints = ON`)
			mustExecConn(t, conn, `UPDATE outbox SET created_at_ms = ? WHERE outbox_id IN (`+inList(ids)+`)`,
				outboxTestTimeMS+recoveryBrokenCreatedMS)
			mustExecConn(t, conn, `PRAGMA ignore_check_constraints = OFF`)
		})
	}
	triggers := map[recoveryRowFault]string{
		recoveryRowAborts: `CREATE TRIGGER property_recovery_aborts BEFORE UPDATE OF state ON outbox
			WHEN OLD.state = 'dispatching' AND NEW.state IN ('not_dispatched', 'uncertain')
				AND OLD.outbox_id IN (%s)
			BEGIN SELECT RAISE(ABORT, 'injected recovery failure'); END`,
		recoveryRowSkipped: `CREATE TRIGGER property_recovery_skipped BEFORE UPDATE OF state ON outbox
			WHEN OLD.state = 'dispatching' AND NEW.state IN ('not_dispatched', 'uncertain')
				AND OLD.outbox_id IN (%s)
			BEGIN SELECT RAISE(IGNORE); END`,
	}
	for fault, trigger := range triggers {
		if ids := byFault[fault]; len(ids) > 0 {
			mustExec(t, store.db, fmt.Sprintf(trigger, inList(ids)))
		}
	}

	nowMS := outboxTestTimeMS + recoveryExpiryBaseMS - 1_000
	call := func(index int, step recoveryCall) error {
		clock.Set(nowMS)
		before, err := readOutboxRawRows(ctx, store.db)
		if err != nil {
			return fmt.Errorf("read rows before: %w", err)
		}
		if step.StoreFault != "" {
			raise := `SELECT abs(-9223372036854775807 - 1)`
			if step.Rollback {
				raise = `SELECT RAISE(ROLLBACK, 'injected rollback')`
			}
			mustExec(t, store.db, `CREATE TRIGGER property_store_fault BEFORE UPDATE OF state ON outbox
				WHEN OLD.outbox_id = '`+step.StoreFault+`'
				BEGIN `+raise+`; END`)
		}
		recovered, callErr := repository.RecoverExpiredLeases(ctx, clock.Now())
		if step.StoreFault != "" {
			mustExec(t, store.db, `DROP TRIGGER property_store_fault`)
		}
		after, err := readOutboxRawRows(ctx, store.db)
		if err != nil {
			return fmt.Errorf("read rows after: %w", err)
		}
		want := model.call(nowMS, step.StoreFault)
		if err := checkRecoveryCall(before, after, recovered, callErr, want, step.Rollback, nowMS); err != nil {
			return fmt.Errorf("call %d at +%d ms, store fault on %q (rollback %t): %w",
				index, nowMS-outboxTestTimeMS, step.StoreFault, step.Rollback, err)
		}
		model.apply(want)
		return nil
	}
	for index, step := range script.Calls {
		nowMS += step.AdvanceMS
		if err := call(index, step); err != nil {
			return err
		}
	}

	// The faults clear, and the clock passes every expiry: one call recovers
	// every lease that no call has recovered yet.
	for _, name := range []string{"property_recovery_aborts", "property_recovery_skipped"} {
		mustExec(t, store.db, `DROP TRIGGER IF EXISTS `+name)
	}
	for _, id := range byFault[recoveryRowBreaksCheck] {
		mustExec(t, store.db, `UPDATE outbox SET created_at_ms = ? WHERE outbox_id = ?`, written[id]["created_at_ms"], id)
	}
	for _, row := range model.rows {
		row.fault = recoveryRowHealthy
	}
	nowMS = outboxTestTimeMS + recoveryFinalCallMS
	if err := call(len(script.Calls), recoveryCall{}); err != nil {
		return err
	}
	final, err := readOutboxRawRows(ctx, store.db)
	if err != nil {
		return fmt.Errorf("read rows at the end: %w", err)
	}
	for _, row := range script.Rows {
		want := "queued"
		switch {
		case row.Called:
			want = "uncertain"
		case row.Leased:
			want = "not_dispatched"
		}
		if state := final[row.ID]["state"]; state != want {
			return fmt.Errorf("after the faults cleared, %s is %v, want %s", row.ID, state, want)
		}
	}
	return nil
}

// recoveryModel predicts what each RecoverExpiredLeases call does from what
// it knows of each row, never from the store.
type recoveryModel struct {
	rows map[string]*recoveryModelRow
}

type recoveryModelRow struct {
	id string
	// leased: the row still holds its lease, which expires at expiresAtMS.
	leased      bool
	expiresAtMS int64
	called      bool
	fault       recoveryRowFault
}

type predictedRecovery struct {
	storeError bool
	// preCall and postCall hold the leases the call recovers, by whether
	// their transport call had started.
	preCall, postCall []string
	failed            []predictedRecoveryFailure
}

type predictedRecoveryFailure struct {
	id     string
	called bool
	fault  recoveryRowFault
}

func (model recoveryModel) call(nowMS int64, storeFault string) predictedRecovery {
	var expired []*recoveryModelRow
	for _, row := range model.rows {
		if row.leased && row.expiresAtMS <= nowMS {
			expired = append(expired, row)
		}
	}
	slices.SortFunc(expired, func(a, b *recoveryModelRow) int {
		return cmp.Or(cmp.Compare(a.expiresAtMS, b.expiresAtMS), strings.Compare(a.id, b.id))
	})
	var predicted predictedRecovery
	for _, row := range expired {
		switch {
		case row.id == storeFault:
			return predictedRecovery{storeError: true}
		case row.fault != recoveryRowHealthy:
			predicted.failed = append(predicted.failed, predictedRecoveryFailure{id: row.id, called: row.called, fault: row.fault})
		case row.called:
			predicted.postCall = append(predicted.postCall, row.id)
		default:
			predicted.preCall = append(predicted.preCall, row.id)
		}
	}
	return predicted
}

func (model recoveryModel) apply(predicted predictedRecovery) {
	for _, id := range slices.Concat(predicted.preCall, predicted.postCall) {
		model.rows[id].leased = false
	}
}

// checkRecoveryCall compares one call's result and every row's columns after
// it with the model's prediction and the rows before it. It first checks, from
// the rows alone, what must hold whatever the model says.
func checkRecoveryCall(
	before, after map[string]outboxRawRow,
	recovered RecoveredLeases,
	callErr error,
	want predictedRecovery,
	rollback bool,
	nowMS int64,
) error {
	if len(after) != len(before) {
		return fmt.Errorf("%d rows after the call, want %d", len(after), len(before))
	}
	var expired []string
	for id, was := range before {
		now, ok := after[id]
		if !ok {
			return fmt.Errorf("row %s is gone", id)
		}
		if was["transport_called_at_ms"] != nil && (now["state"] == "queued" || now["state"] == "not_dispatched") {
			return fmt.Errorf("row %s, whose transport call started, is %v", id, now["state"])
		}
		if !reflect.DeepEqual(now["attempt_count"], was["attempt_count"]) {
			return fmt.Errorf("row %s attempt_count = %v, want %v", id, now["attempt_count"], was["attempt_count"])
		}
		if was["state"] == "dispatching" && was["lease_expires_at_ms"].(int64) <= nowMS {
			expired = append(expired, id)
		}
	}

	if want.storeError {
		cause := "integer overflow"
		if rollback {
			cause = "injected rollback"
		}
		if callErr == nil || !strings.Contains(callErr.Error(), cause) {
			return fmt.Errorf("error = %v, want %q", callErr, cause)
		}
		if !rollback && isRowLeaseFailure(callErr) {
			return fmt.Errorf("error %v belongs to the row, want the store's", callErr)
		}
		if recovered.NotDispatched != 0 || recovered.Uncertain != 0 || len(recovered.Failed) != 0 {
			return fmt.Errorf("RecoverExpiredLeases() = %+v with an error, want nothing reported", recovered)
		}
		if !reflect.DeepEqual(after, before) {
			return fmt.Errorf("a failed call changed rows: %s", describeRowChanges(before, after))
		}
		return nil
	}
	if callErr != nil {
		return fmt.Errorf("error = %v, want none", callErr)
	}
	if met := recovered.NotDispatched + recovered.Uncertain + len(recovered.Failed); met != len(expired) {
		return fmt.Errorf("RecoverExpiredLeases() = %+v met %d leases, want the %d that had expired: %v",
			recovered, met, len(expired), expired)
	}
	if recovered.NotDispatched != len(want.preCall) || recovered.Uncertain != len(want.postCall) {
		return fmt.Errorf("RecoverExpiredLeases() = %+v, want %d not_dispatched and %d uncertain",
			recovered, len(want.preCall), len(want.postCall))
	}
	if len(recovered.Failed) != len(want.failed) {
		return fmt.Errorf("failures %+v, want %+v", recovered.Failed, want.failed)
	}
	for index, failure := range recovered.Failed {
		predicted := want.failed[index]
		if failure.OutboxID != predicted.id || failure.TransportCalled != predicted.called ||
			failure.Err == nil || !strings.Contains(failure.Err.Error(), predicted.fault.cause()) {
			return fmt.Errorf("failure %d = %+v, want %s (transport called %t) failing for %q",
				index, failure, predicted.id, predicted.called, predicted.fault.cause())
		}
	}

	outcome := make(map[string]string)
	for _, id := range want.preCall {
		outcome[id] = "not_dispatched"
	}
	for _, id := range want.postCall {
		outcome[id] = "uncertain"
	}
	for id, was := range before {
		expected := maps.Clone(was)
		if state, ok := outcome[id]; ok {
			expected["state"] = state
			expected["next_attempt_at_ms"] = nil
			if state == "not_dispatched" {
				expected["next_attempt_at_ms"] = nowMS
			}
			expected["lease_owner"], expected["lease_token"] = nil, nil
			expected["lease_expires_at_ms"], expected["transport_called_at_ms"] = nil, nil
			expected["updated_at_ms"] = max(was["updated_at_ms"].(int64), nowMS)
		}
		if now := after[id]; !reflect.DeepEqual(now, expected) {
			return fmt.Errorf("row %s: %s", id, describeRowChanges(map[string]outboxRawRow{id: expected}, map[string]outboxRawRow{id: now}))
		}
	}
	return nil
}
