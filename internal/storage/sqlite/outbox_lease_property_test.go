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
	"time"
)

// leaseRowFault says how the writes to one row fail in a generated history.
type leaseRowFault int

const (
	leaseRowHealthy leaseRowFault = iota
	// leaseRowLeaseAborts: a trigger aborts the row's lease, as a CHECK the
	// row can no longer meet would. The set-aside goes through.
	leaseRowLeaseAborts
	// leaseRowLeaseSkipped: a trigger skips the row's lease (RAISE(IGNORE)).
	leaseRowLeaseSkipped
	// leaseRowUnreadable: the row's created_at_ms holds a NULL, so the leased
	// row does not read back.
	leaseRowUnreadable
	// leaseRowWritesAbort: a trigger aborts every write to the row while it
	// is due, the set-aside's too.
	leaseRowWritesAbort
	// leaseRowWritesSkipped: a trigger skips every write to the row while it
	// is due.
	leaseRowWritesSkipped
	leaseRowFaultCount
)

// cause returns text that every error the fault produces contains.
func (fault leaseRowFault) cause() string {
	switch fault {
	case leaseRowLeaseAborts:
		return "injected lease failure"
	case leaseRowLeaseSkipped, leaseRowWritesSkipped:
		return "the update changed no row"
	case leaseRowUnreadable:
		return "stored values do not read back"
	case leaseRowWritesAbort:
		return "injected write failure"
	default:
		return ""
	}
}

// setsAside reports whether the call can still set the row aside after its
// lease fails.
func (fault leaseRowFault) setsAside() bool {
	switch fault {
	case leaseRowLeaseAborts, leaseRowLeaseSkipped, leaseRowUnreadable:
		return true
	default:
		return false
	}
}

// leaseDueScript is one generated history of LeaseDueBatch calls. Each row
// takes its due time and its creation time from a few values, so rows often
// tie on the first keys of the due order, and the outbox ids follow neither.
// Each script draws its own share of failing rows, so some hold long runs of
// them that span several of the call's pages. Between calls the clock moves
// forward by amounts that fall short of, meet or pass the retry delays.
type leaseDueScript struct {
	Rows  []leaseDueRow
	Calls []leaseDueCall
}

type leaseDueRow struct {
	ID string
	// DueSlot and CreatedSlot set scheduled_for_ms and created_at_ms a few
	// milliseconds apart.
	DueSlot, CreatedSlot int64
	// Future schedules the row after every call, so it never comes due.
	Future bool
	Fault  leaseRowFault
}

type leaseDueCall struct {
	AdvanceMS int64
	Limit     int
	// StoreError, when not empty, names a healthy row whose lease raises a
	// store error in this call.
	StoreError string
}

func (leaseDueScript) Generate(r *rand.Rand, _ int) reflect.Value {
	count := 1 + r.Intn(40)
	failingShare := r.Float64()
	ids := r.Perm(count)
	script := leaseDueScript{Rows: make([]leaseDueRow, count)}
	for index := range script.Rows {
		row := leaseDueRow{
			ID:          fmt.Sprintf("outbox-p%02d", ids[index]),
			DueSlot:     int64(r.Intn(4)),
			CreatedSlot: int64(r.Intn(3)),
			Future:      r.Intn(10) == 0,
		}
		if r.Float64() < failingShare {
			row.Fault = leaseRowFault(1 + r.Intn(int(leaseRowFaultCount)-1))
		}
		script.Rows[index] = row
	}
	var healthy []string
	for _, row := range script.Rows {
		if row.Fault == leaseRowHealthy && !row.Future {
			healthy = append(healthy, row.ID)
		}
	}
	// Up to twelve calls, so a row whose lease keeps failing can double its
	// delay up to the 5-minute cap, which takes seven failed leases.
	advances := []int64{0, 1_000, 4_999, 5_000, 10_000, 30_000, 70_000, 200_000, 400_000}
	for index := range 1 + r.Intn(12) {
		call := leaseDueCall{Limit: 1 + r.Intn(6)}
		if r.Intn(8) == 0 {
			call.Limit = 32
		}
		if index > 0 {
			call.AdvanceMS = advances[r.Intn(len(advances))]
		}
		if len(healthy) > 0 && r.Intn(4) == 0 {
			call.StoreError = healthy[r.Intn(len(healthy))]
		}
		script.Calls = append(script.Calls, call)
	}
	return reflect.ValueOf(script)
}

// TestLeaseDueBatchInvariantsHoldForAnyDueSetAndFaults checks LeaseDueBatch
// against a model over generated histories of due rows, any of whose writes
// may fail in any of the ways a row's write can fail. The model knows only
// what the script says of each row and what the earlier calls did. It walks
// the due rows in due order: a row whose writes work leases, a row whose
// lease fails goes aside, a row the call can neither lease nor set aside
// stays as it was, until the batch fills. The test reads every column of
// every row before and after each call. So, in every call:
//
//   - the call leases the first due rows in due order whose own writes work,
//     up to the batch limit, however many rows before them fail;
//   - it meets each due row at most once, in due order: each row it meets
//     comes back once, as a lease or as a failure;
//   - it sets aside a row whose lease fails on the row itself: the row comes
//     due again 5 s later, twice its last delay after each further failed
//     lease, up to 5 minutes, and the call records the error and changes no
//     other column;
//   - a row it can neither lease nor set aside stays as it was, byte for
//     byte, and so does every row it does not meet;
//   - a store error fails the whole call and changes no row;
//   - once the faults clear and every retry comes due, one call leases every
//     row that has not leased yet, so no failure loses an intent.
//
// The clock moves only forward here, so the delays double exactly; the
// dispatcher's TestLeaseFailuresNeverHoldOtherSends steps it back too.
func TestLeaseDueBatchInvariantsHoldForAnyDueSetAndFaults(t *testing.T) {
	dir := t.TempDir()
	property := func(script leaseDueScript) bool {
		if err := runLeaseDueScript(t, dir, script); err != nil {
			t.Logf("script %+v: %v", script, err)
			return false
		}
		return true
	}
	if err := quick.Check(property, storagePropertyConfig(t, 20261104, 100, 8)); err != nil {
		t.Fatal(err)
	}
}

func runLeaseDueScript(t *testing.T, dir string, script leaseDueScript) error {
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

	model := leaseDueModel{rows: make(map[string]*leaseModelRow)}
	byFault := make(map[leaseRowFault][]string)
	for _, row := range script.Rows {
		clock.Set(outboxTestTimeMS + row.CreatedSlot)
		item := outboxTestItem(strings.TrimPrefix(row.ID, "outbox-"))
		item.ScheduledForMS = outboxTestTimeMS - 100 + row.DueSlot
		if row.Future {
			item.ScheduledForMS = outboxTestTimeMS + 100_000_000
		}
		mustEnqueueOutbox(t, repository, item)
		modelRow := &leaseModelRow{
			id:          row.ID,
			scheduledMS: item.ScheduledForMS,
			createdKey:  outboxTestTimeMS + row.CreatedSlot,
			fault:       row.Fault,
		}
		if row.Fault == leaseRowUnreadable {
			// The due order reads a NULL created_at_ms as 0.
			modelRow.createdKey = 0
		}
		model.rows[row.ID] = modelRow
		byFault[row.Fault] = append(byFault[row.Fault], row.ID)
	}
	if ids := byFault[leaseRowUnreadable]; len(ids) > 0 {
		relaxOutboxNotNull(t, store, "created_at_ms", "INTEGER", ids...)
	}
	triggers := map[leaseRowFault]string{
		leaseRowLeaseAborts: `CREATE TRIGGER property_lease_aborts BEFORE UPDATE OF state ON outbox
			WHEN NEW.state = 'dispatching' AND OLD.outbox_id IN (%s)
			BEGIN SELECT RAISE(ABORT, 'injected lease failure'); END`,
		leaseRowLeaseSkipped: `CREATE TRIGGER property_lease_skipped BEFORE UPDATE OF state ON outbox
			WHEN NEW.state = 'dispatching' AND OLD.outbox_id IN (%s)
			BEGIN SELECT RAISE(IGNORE); END`,
		leaseRowWritesAbort: `CREATE TRIGGER property_writes_abort BEFORE UPDATE ON outbox
			WHEN OLD.outbox_id IN (%s) AND OLD.state IN ('queued', 'not_dispatched')
			BEGIN SELECT RAISE(ABORT, 'injected write failure'); END`,
		leaseRowWritesSkipped: `CREATE TRIGGER property_writes_skipped BEFORE UPDATE ON outbox
			WHEN OLD.outbox_id IN (%s) AND OLD.state IN ('queued', 'not_dispatched')
			BEGIN SELECT RAISE(IGNORE); END`,
	}
	for fault, trigger := range triggers {
		if ids := byFault[fault]; len(ids) > 0 {
			mustExec(t, store.db, fmt.Sprintf(trigger, "'"+strings.Join(ids, "', '")+"'"))
		}
	}

	leasedBefore := make(map[string]bool)
	nowMS := outboxTestTimeMS + 10
	call := func(index int, limit int, storeError string) error {
		clock.Set(nowMS)
		before, err := readOutboxRawRows(ctx, store.db)
		if err != nil {
			return fmt.Errorf("read rows before: %w", err)
		}
		if storeError != "" {
			mustExec(t, store.db, `CREATE TRIGGER property_store_error BEFORE UPDATE OF state ON outbox
				WHEN NEW.state = 'dispatching' AND OLD.outbox_id = '`+storeError+`'
				BEGIN SELECT abs(-9223372036854775807 - 1); END`)
		}
		batch, callErr := repository.LeaseDueBatch(ctx, LeaseRequest{
			Owner: "dispatcher", Now: clock.Now(), Duration: time.Minute, Limit: limit,
		})
		if storeError != "" {
			mustExec(t, store.db, `DROP TRIGGER property_store_error`)
		}
		after, err := readOutboxRawRows(ctx, store.db)
		if err != nil {
			return fmt.Errorf("read rows after: %w", err)
		}
		want := model.call(nowMS, limit, storeError)
		if err := checkLeaseDueCall(before, after, batch, callErr, want, nowMS); err != nil {
			return fmt.Errorf("call %d at +%d ms, limit %d, store error on %q: %w",
				index, nowMS-outboxTestTimeMS, limit, storeError, err)
		}
		for _, lease := range batch.Leases {
			if leasedBefore[lease.OutboxID] {
				return fmt.Errorf("call %d leased %s, which an earlier call leased", index, lease.OutboxID)
			}
			leasedBefore[lease.OutboxID] = true
		}
		model.apply(want, nowMS)
		return nil
	}
	for index, step := range script.Calls {
		nowMS += step.AdvanceMS
		if err := call(index, step.Limit, step.StoreError); err != nil {
			return err
		}
	}

	// The faults clear, and the clock passes every retry: one call leases
	// every row that has not leased yet, in due order.
	for _, name := range []string{
		"property_lease_aborts", "property_lease_skipped", "property_writes_abort", "property_writes_skipped",
	} {
		mustExec(t, store.db, `DROP TRIGGER IF EXISTS `+name)
	}
	for _, row := range script.Rows {
		if row.Fault == leaseRowUnreadable {
			mustExec(t, store.db, `UPDATE outbox SET created_at_ms = ? WHERE outbox_id = ?`,
				outboxTestTimeMS+row.CreatedSlot, row.ID)
			model.rows[row.ID].createdKey = outboxTestTimeMS + row.CreatedSlot
		}
		model.rows[row.ID].fault = leaseRowHealthy
	}
	nowMS += 10 * time.Minute.Milliseconds()
	if err := call(len(script.Calls), len(script.Rows), ""); err != nil {
		return err
	}
	final, err := readOutboxRawRows(ctx, store.db)
	if err != nil {
		return fmt.Errorf("read rows at the end: %w", err)
	}
	for _, row := range script.Rows {
		want := "dispatching"
		if row.Future {
			want = "queued"
		}
		if state := final[row.ID]["state"]; state != want {
			return fmt.Errorf("after the faults cleared, %s is %v, want %s", row.ID, state, want)
		}
	}
	return nil
}

// leaseDueModel predicts what each LeaseDueBatch call does from what it
// knows of each row, never from the store.
type leaseDueModel struct {
	rows map[string]*leaseModelRow
}

type leaseModelRow struct {
	id                      string
	scheduledMS, createdKey int64
	fault                   leaseRowFault
	leased                  bool
	// retryAtMS holds the time a set-aside row comes due again, 0 before its
	// first set-aside; lastDelayMS holds the delay that set-aside chose.
	retryAtMS, lastDelayMS int64
}

// dueKey is the row's first key in the due order: its retry time once it
// has one, else its scheduled time.
func (row *leaseModelRow) dueKey() int64 {
	if row.retryAtMS != 0 {
		return row.retryAtMS
	}
	return row.scheduledMS
}

type predictedCall struct {
	storeError bool
	leased     []string
	failed     []predictedFailure
}

type predictedFailure struct {
	id    string
	fault leaseRowFault
	// retryAtMS is the time the set-aside row comes due again, 0 when the
	// call cannot set the row aside.
	retryAtMS int64
}

func (model leaseDueModel) call(nowMS int64, limit int, storeError string) predictedCall {
	var due []*leaseModelRow
	for _, row := range model.rows {
		if !row.leased && row.scheduledMS <= nowMS && row.dueKey() <= nowMS {
			due = append(due, row)
		}
	}
	slices.SortFunc(due, func(a, b *leaseModelRow) int {
		return cmp.Or(cmp.Compare(a.dueKey(), b.dueKey()), cmp.Compare(a.createdKey, b.createdKey), strings.Compare(a.id, b.id))
	})
	var predicted predictedCall
	for _, row := range due {
		if len(predicted.leased) == limit {
			break
		}
		switch {
		case row.id == storeError:
			return predictedCall{storeError: true}
		case row.fault == leaseRowHealthy:
			predicted.leased = append(predicted.leased, row.id)
		case row.fault.setsAside():
			// The documented backoff: 5 s, then twice the last delay, up to 5
			// minutes.
			delayMS := int64(5_000)
			if row.lastDelayMS > 0 {
				delayMS = min(2*row.lastDelayMS, 300_000)
			}
			predicted.failed = append(predicted.failed, predictedFailure{id: row.id, fault: row.fault, retryAtMS: nowMS + delayMS})
		default:
			predicted.failed = append(predicted.failed, predictedFailure{id: row.id, fault: row.fault})
		}
	}
	return predicted
}

func (model leaseDueModel) apply(predicted predictedCall, nowMS int64) {
	for _, id := range predicted.leased {
		model.rows[id].leased = true
	}
	for _, failure := range predicted.failed {
		if failure.retryAtMS != 0 {
			row := model.rows[failure.id]
			row.lastDelayMS = failure.retryAtMS - nowMS
			row.retryAtMS = failure.retryAtMS
		}
	}
}

// checkLeaseDueCall compares one call's result and every row's columns
// after it with the model's prediction and the rows before it.
func checkLeaseDueCall(
	before, after map[string]outboxRawRow,
	batch LeaseBatch,
	callErr error,
	want predictedCall,
	nowMS int64,
) error {
	if want.storeError {
		if callErr == nil || !strings.Contains(callErr.Error(), "integer overflow") || isRowLeaseFailure(callErr) {
			return fmt.Errorf("error = %v, want the store's integer overflow", callErr)
		}
		if len(batch.Leases) != 0 || len(batch.Failed) != 0 {
			return fmt.Errorf("batch = %+v with a store error, want it empty", batch)
		}
		if !reflect.DeepEqual(after, before) {
			return fmt.Errorf("a store error changed rows: %s", describeRowChanges(before, after))
		}
		return nil
	}
	if callErr != nil {
		return fmt.Errorf("error = %v, want none", callErr)
	}
	if got := leasedOutboxIDs(batch); !slices.Equal(got, want.leased) {
		return fmt.Errorf("leased %v, want %v", got, want.leased)
	}
	met := make(map[string]bool)
	for _, id := range want.leased {
		met[id] = true
	}
	if len(batch.Failed) != len(want.failed) {
		return fmt.Errorf("failures %+v, want %+v", batch.Failed, want.failed)
	}
	setAside := make(map[string]predictedFailure)
	for index, failure := range batch.Failed {
		predicted := want.failed[index]
		if met[failure.OutboxID] {
			return fmt.Errorf("the call met %s twice", failure.OutboxID)
		}
		met[failure.OutboxID] = true
		if failure.OutboxID != predicted.id || failure.Err == nil || !strings.Contains(failure.Err.Error(), predicted.fault.cause()) {
			return fmt.Errorf("failure %d = %+v, want %s failing for %q", index, failure, predicted.id, predicted.fault.cause())
		}
		if predicted.retryAtMS != 0 {
			if failure.SetAsideErr != nil || failure.RetryAt.UnixMilli() != predicted.retryAtMS {
				return fmt.Errorf("failure %d = %+v, want %s set aside until %d", index, failure, predicted.id, predicted.retryAtMS)
			}
			setAside[predicted.id] = predicted
			continue
		}
		if failure.SetAsideErr == nil || !strings.Contains(failure.SetAsideErr.Error(), predicted.fault.cause()) || !failure.RetryAt.IsZero() {
			return fmt.Errorf("failure %d = %+v, want %s neither leased nor set aside", index, failure, predicted.id)
		}
	}

	if len(after) != len(before) {
		return fmt.Errorf("%d rows after the call, want %d", len(after), len(before))
	}
	leased := make(map[string]bool)
	for _, id := range want.leased {
		leased[id] = true
	}
	for id, was := range before {
		now, ok := after[id]
		if !ok {
			return fmt.Errorf("row %s is gone", id)
		}
		expected := maps.Clone(was)
		updatedAtMS := max(was["updated_at_ms"].(int64), nowMS)
		if leased[id] {
			if token, ok := now["lease_token"].(string); !ok || token == "" {
				return fmt.Errorf("leased row %s has lease token %v", id, now["lease_token"])
			}
			expected["state"] = "dispatching"
			expected["error_class"], expected["error_code"], expected["error_detail"] = nil, nil, nil
			expected["lease_owner"] = "dispatcher"
			expected["lease_token"] = now["lease_token"]
			expected["lease_expires_at_ms"] = nowMS + time.Minute.Milliseconds()
			expected["transport_called_at_ms"] = nil
			expected["next_attempt_at_ms"] = nil
			expected["updated_at_ms"] = updatedAtMS
		}
		if failure, ok := setAside[id]; ok {
			if detail, ok := now["error_detail"].(string); !ok || !strings.Contains(detail, failure.fault.cause()) {
				return fmt.Errorf("set-aside row %s has error detail %v, want the cause", id, now["error_detail"])
			}
			expected["state"] = "not_dispatched"
			expected["error_class"] = "transient"
			expected["error_code"] = "lease"
			expected["error_detail"] = now["error_detail"]
			expected["next_attempt_at_ms"] = failure.retryAtMS
			expected["updated_at_ms"] = updatedAtMS
		}
		if !reflect.DeepEqual(now, expected) {
			return fmt.Errorf("row %s: %s", id, describeRowChanges(map[string]outboxRawRow{id: expected}, map[string]outboxRawRow{id: now}))
		}
	}
	return nil
}

// outboxRawRow holds every column of one outbox row. The test reads rows
// straight from the table, since FindByID fails on a row whose values do not
// read back.
type outboxRawRow map[string]any

func readOutboxRawRows(ctx context.Context, db *sql.DB) (map[string]outboxRawRow, error) {
	rows, err := db.QueryContext(ctx, `SELECT * FROM outbox`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	result := make(map[string]outboxRawRow)
	for rows.Next() {
		values := make([]any, len(columns))
		targets := make([]any, len(columns))
		for index := range values {
			targets[index] = &values[index]
		}
		if err := rows.Scan(targets...); err != nil {
			return nil, err
		}
		row := make(outboxRawRow, len(columns))
		for index, column := range columns {
			row[column] = values[index]
		}
		id, ok := row["outbox_id"].(string)
		if !ok {
			return nil, fmt.Errorf("outbox_id = %v, want text", row["outbox_id"])
		}
		result[id] = row
	}
	return result, rows.Err()
}

// describeRowChanges names each column whose value differs between two
// readings of the same rows.
func describeRowChanges(want, got map[string]outboxRawRow) string {
	var changes []string
	for _, id := range slices.Sorted(maps.Keys(want)) {
		for _, column := range slices.Sorted(maps.Keys(want[id])) {
			if !reflect.DeepEqual(want[id][column], got[id][column]) {
				changes = append(changes, fmt.Sprintf("%s.%s = %v, want %v", id, column, got[id][column], want[id][column]))
			}
		}
	}
	return strings.Join(changes, "; ")
}
