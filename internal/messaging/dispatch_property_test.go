package messaging

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/quick"
	"time"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

// dispatchFault names the outbox write a trigger fails during one pass.
type dispatchFault int

const (
	noDispatchFault dispatchFault = iota
	// leaseDispatchFault fails LeaseDueBatch's lease UPDATE, so the row is set
	// aside to retry.
	leaseDispatchFault
	// markCalledDispatchFault fails MarkTransportCalled, before the call.
	markCalledDispatchFault
	// settleDispatchFault fails Confirm and MarkStoreFailed, after the call.
	settleDispatchFault
	dispatchFaultCount
)

const injectedDispatchFault = "injected property fault"

var dispatchFaultConditions = map[dispatchFault]string{
	leaseDispatchFault:      `UPDATE OF state ON outbox WHEN NEW.state = 'dispatching'`,
	markCalledDispatchFault: `UPDATE OF transport_called_at_ms ON outbox WHEN NEW.transport_called_at_ms IS NOT NULL`,
	settleDispatchFault:     `UPDATE OF state ON outbox WHEN NEW.state IN ('confirmed', 'store_failed')`,
}

// checkedTables are the tables whose CHECK requires updated_at_ms >=
// created_at_ms.
var checkedTables = []string{
	"accounts", "devices", "identities", "people", "conversations",
	"messages", "outbox", "message_attachments", "reactions",
}

// dispatchScript is one generated history of a text send: the gap between
// SendText's two clock reads (scheduled_for_ms, then created_at_ms), the
// first pass's time relative to the first read, then passes, each after a
// clock step and with at most one fault. A negative step is a wall clock set
// back; a long one carries a lease past its expiry.
type dispatchScript struct {
	SubmitReadGapMS int64
	FirstPassMS     int64
	Passes          []dispatchScriptPass
}

type dispatchScriptPass struct {
	ClockStepMS int64
	Fault       dispatchFault
}

// Generate makes testing/quick draw scripts around the 2026-11-03 race: the
// first pass often falls between SendText's two clock reads.
func (dispatchScript) Generate(r *rand.Rand, _ int) reflect.Value {
	script := dispatchScript{
		SubmitReadGapMS: int64(r.Intn(40)),
		FirstPassMS:     int64(r.Intn(100) - 50),
	}
	for range r.Intn(12) {
		pass := dispatchScriptPass{Fault: dispatchFault(r.Intn(int(dispatchFaultCount)))}
		switch r.Intn(4) {
		case 0:
			pass.ClockStepMS = -int64(r.Intn(2_000))
		case 1:
			pass.ClockStepMS = int64(r.Intn(60_000))
		default:
			pass.ClockStepMS = int64(r.Intn(2_000))
		}
		script.Passes = append(script.Passes, pass)
	}
	return reflect.ValueOf(script)
}

// TestDispatchInvariantsHoldForAnyTimingAndFaults runs Run one pass at a time
// over generated submit, lease and transition times and storage faults, and
// checks after every pass:
//
//   - no row of checkedTables has updated_at_ms below its created_at_ms, or
//     below its value before the pass;
//   - a pass without a fault never fails, and leases and sends every row
//     that is due when it runs;
//   - a failed lease never fails its pass: the store sets the due row aside,
//     uncalled, until a later retry;
//   - Run keeps running after a failed pass: it reports the failure to
//     OnPassFailure and DispatchHealth, waits the poll delay doubled for each
//     earlier failure in the same run of failures, up to the longest poll
//     delay, and passes again.
//
// Once the faults stop, leases expire and retries come due, the text must be
// confirmed, or uncertain when a failure followed its transport call, after
// exactly one transport request, and Run must return only when its context is
// canceled.
//
// Each history sends one text. TestOutboxWritesNeverStampUpdatedAtBelowCreatedAt
// and TestStoreWritesNeverStampUpdatedAtBelowCreatedAt (internal/storage/sqlite)
// check each write's clamp on its own, on a clock before the row's creation
// and on one after its creation but before its last update.
func TestDispatchInvariantsHoldForAnyTimingAndFaults(t *testing.T) {
	property := func(script dispatchScript) bool {
		if err := runDispatchScript(t, script); err != nil {
			t.Logf("script %+v: %v", script, err)
			return false
		}
		return true
	}
	if err := quick.Check(property, dispatchPropertyConfig(t, 50, 10)); err != nil {
		t.Fatal(err)
	}
}

// dispatchPropertyConfig returns the cases and seed for a dispatch property
// test: fewer cases under -race, where the pure-Go SQLite engine runs about
// tenfold slower. OPENMESSAGE_PROPERTY_CASES and OPENMESSAGE_PROPERTY_SEED
// override both, so a sweep over seeds needs no rebuild.
func dispatchPropertyConfig(t *testing.T, cases, raceCases int) *quick.Config {
	t.Helper()
	if raceDetectorEnabled {
		cases = raceCases
	}
	seed := int64(20261103)
	for name, target := range map[string]func(int64){
		"OPENMESSAGE_PROPERTY_CASES": func(value int64) { cases = int(value) },
		"OPENMESSAGE_PROPERTY_SEED":  func(value int64) { seed = value },
	} {
		raw := os.Getenv(name)
		if raw == "" {
			continue
		}
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || value <= 0 {
			t.Fatalf("%s = %q, want a positive integer", name, raw)
		}
		target(value)
	}
	return &quick.Config{MaxCount: cases, Rand: rand.New(rand.NewSource(seed))}
}

func runDispatchScript(t *testing.T, script dispatchScript) (err error) {
	ctx := context.Background()
	clock := newRunStepClock(time.Hour)
	store, raw := openReconcileTestStore(t, clock.Now())
	sender := &scriptedTextSender{steps: []sendStep{{result: bridge.SendResult{RemoteMessageID: "remote-property"}}}}
	registry := newScriptedRegistry("property", sender)
	registry.setAvailable(true)
	service, err := NewMessageService(store, registry, nil, clock, &sequentialIDs{})
	if err != nil {
		return fmt.Errorf("NewMessageService: %w", err)
	}
	// Run waits between the poll delay and the longest poll delay. An hour and
	// eight hours exceed every transport bound, so the clock can tell Run's
	// waits from a transport call's.
	service.pollDelay, service.maxPollDelay = clock.floor, 8*clock.floor
	failures := &passFailures{}
	service.OnPassFailure(failures.record)

	submitAt := messagingTestTime.Add(time.Second)
	clock.set(submitAt, time.Duration(script.SubmitReadGapMS)*time.Millisecond)
	submission, err := service.SendText(ctx, SendTextCommand{
		CommonCommand: testCommonCommand("key-property"),
		Body:          "sent under any timing",
	})
	if err != nil {
		return fmt.Errorf("SendText: %w", err)
	}
	run := newSteppedRun(service, clock)
	defer func() {
		if stopErr := run.stop(); err == nil {
			err = stopErr
		}
	}()

	now := submitAt.Add(time.Duration(script.FirstPassMS) * time.Millisecond)
	latest := submitAt.Add(time.Duration(script.SubmitReadGapMS) * time.Millisecond)
	postCallFailure := false
	var failed, inARow int64

	for index, pass := range script.Passes {
		if index > 0 {
			now = now.Add(time.Duration(pass.ClockStepMS) * time.Millisecond)
		}
		if now.After(latest) {
			latest = now
		}
		clock.set(now, 0)
		if err := setDispatchFault(raw, pass.Fault); err != nil {
			return err
		}
		before, err := service.outbox.FindByID(ctx, submission.OutboxID)
		if err != nil {
			return fmt.Errorf("pass %d: read row: %w", index, err)
		}
		rows, err := updatedAtByRow(raw)
		if err != nil {
			return fmt.Errorf("pass %d: %w", index, err)
		}
		requestsBefore := sender.requestCount()
		wait, err := run.pass()
		if err != nil {
			return fmt.Errorf("pass %d at %d with fault %d: %w", index, now.UnixMilli(), pass.Fault, err)
		}
		after, err := service.outbox.FindByID(ctx, submission.OutboxID)
		if err != nil {
			return fmt.Errorf("pass %d: read row: %w", index, err)
		}
		passErr, reported, err := failures.after(failed)
		if err != nil {
			return fmt.Errorf("pass %d: %w", index, err)
		}
		if passErr != nil && (pass.Fault == noDispatchFault || pass.Fault == leaseDispatchFault ||
			!strings.Contains(passErr.Error(), injectedDispatchFault)) {
			return fmt.Errorf("pass %d at %d with fault %d failed: %w", index, now.UnixMilli(), pass.Fault, passErr)
		}
		if pass.Fault == noDispatchFault && dueAt(before, now) && after.State != sqlite.OutboxConfirmed {
			return fmt.Errorf("pass %d at %d left a due row %+v as %+v", index, now.UnixMilli(), before, after)
		}
		if pass.Fault == leaseDispatchFault && dueAt(before, now) && !setAsideAfterLeaseFailure(after, now) {
			return fmt.Errorf("pass %d at %d left a due row whose lease failed %+v as %+v, want it set aside",
				index, now.UnixMilli(), before, after)
		}
		if passErr != nil && sender.requestCount() > requestsBefore {
			postCallFailure = true
		}
		if passErr != nil {
			failed++
			inARow++
		} else {
			inARow = 0
		}
		if err := checkRunWait(service, now, wait, failed, inARow, passErr, reported); err != nil {
			return fmt.Errorf("pass %d: %w", index, err)
		}
		if err := checkUpdatedAt(raw, rows); err != nil {
			return fmt.Errorf("pass %d: %w", index, err)
		}
		if requests := sender.requestCount(); requests > 1 {
			return fmt.Errorf("pass %d: the text reached the transport %d times", index, requests)
		}
	}

	// The faults stop, every lease expires and every retry comes due.
	if err := setDispatchFault(raw, noDispatchFault); err != nil {
		return err
	}
	settled, err := service.outbox.FindByID(ctx, submission.OutboxID)
	if err != nil {
		return fmt.Errorf("read row after the faults stopped: %w", err)
	}
	now = settleTime(latest.Add(service.leaseTime), settled).Add(time.Second)
	clock.set(now, 0)
	for index := range 2 {
		rows, err := updatedAtByRow(raw)
		if err != nil {
			return fmt.Errorf("pass %d after the faults stopped: %w", index, err)
		}
		wait, err := run.pass()
		if err != nil {
			return fmt.Errorf("pass %d after the faults stopped: %w", index, err)
		}
		passErr, _, err := failures.after(failed)
		if err != nil || passErr != nil {
			return fmt.Errorf("pass %d after the faults stopped failed: %w", index, errors.Join(err, passErr))
		}
		if err := checkRunWait(service, now, wait, failed, 0, nil, FailedDispatchPass{}); err != nil {
			return fmt.Errorf("pass %d after the faults stopped: %w", index, err)
		}
		if err := checkUpdatedAt(raw, rows); err != nil {
			return fmt.Errorf("pass %d after the faults stopped: %w", index, err)
		}
	}
	final, err := service.outbox.FindByID(ctx, submission.OutboxID)
	if err != nil {
		return fmt.Errorf("read final row: %w", err)
	}
	want := sqlite.OutboxConfirmed
	if postCallFailure {
		want = sqlite.OutboxUncertain
	}
	if final.State != want || sender.requestCount() != 1 {
		return fmt.Errorf("final row %+v after %d transport requests, want %s after 1", final, sender.requestCount(), want)
	}
	return nil
}

// checkRunWait checks what Run did after a pass that ran at now and was
// followed by wait: failed passes in all, the last inARow of them in a row
// (0 when the pass did not fail, and passErr is nil). After the k-th failed
// pass in a row Run waits the poll delay doubled k-1 times, up to the longest
// poll delay, and DispatchHealth and the OnPassFailure report (reported)
// describe the pass; after a pass that did not fail it waits a poll delay in
// that range.
func checkRunWait(service *MessageService, now time.Time, wait time.Duration, failed, inARow int64, passErr error, reported FailedDispatchPass) error {
	health := service.DispatchHealth()
	if health.FailedPasses != failed || health.ConsecutiveFailedPasses != inARow {
		return fmt.Errorf(
			"DispatchHealth() counts %d failed passes, %d in a row; want %d, %d",
			health.FailedPasses, health.ConsecutiveFailedPasses, failed, inARow,
		)
	}
	if passErr == nil {
		if wait < service.pollDelay || wait > service.maxPollDelay {
			return fmt.Errorf("Run waits %v after a pass, want %v to %v", wait, service.pollDelay, service.maxPollDelay)
		}
		return nil
	}
	backoff := service.pollDelay
	for range inARow - 1 {
		backoff = min(2*backoff, service.maxPollDelay)
	}
	if wait != backoff {
		return fmt.Errorf("Run waits %v after failed pass %d in a row, want %v", wait, inARow, backoff)
	}
	want := FailedDispatchPass{
		AtMS:        now.UnixMilli(),
		Error:       passErr.Error(),
		Consecutive: inARow,
		RetryAtMS:   now.Add(backoff).UnixMilli(),
	}
	if health.LastFailedPass == nil || *health.LastFailedPass != want {
		return fmt.Errorf("DispatchHealth().LastFailedPass = %+v, want %+v", health.LastFailedPass, want)
	}
	if reported != want {
		return fmt.Errorf("OnPassFailure reported %+v, want %+v", reported, want)
	}
	return nil
}

// passFailures records what OnPassFailure reports. checkRunWait compares
// each report with the pass DispatchHealth reports.
type passFailures struct {
	mu      sync.Mutex
	reports []passFailure
}

// passFailure is one OnPassFailure call.
type passFailure struct {
	err  error
	pass FailedDispatchPass
}

func (p *passFailures) record(err error, pass FailedDispatchPass) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reports = append(p.reports, passFailure{err: err, pass: pass})
}

// after returns the failure reported after the first n, if any: passErr and
// the pass the hook described. Run reports each failed pass once, so more
// than one is an error.
func (p *passFailures) after(n int64) (passErr error, pass FailedDispatchPass, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch reported := int64(len(p.reports)); reported {
	case n:
		return nil, FailedDispatchPass{}, nil
	case n + 1:
		if p.reports[n].err == nil {
			return nil, FailedDispatchPass{}, errors.New("OnPassFailure reported a nil error")
		}
		return p.reports[n].err, p.reports[n].pass, nil
	default:
		return nil, FailedDispatchPass{}, fmt.Errorf("OnPassFailure ran %d times after %d failed passes", reported-n, n)
	}
}

// runStepClock steps Run one pass at a time. Run creates a wait timer only
// when a pass has ended, so NewTimer hands the wait to the test and blocks
// until the test releases the next pass; the timer it then returns has
// already fired. Timers shorter than floor bound transport calls (see
// callTransport) and run on the system clock: Run never waits less than its
// poll delay, which the test sets to floor.
type runStepClock struct {
	*splitReadClock
	floor    time.Duration
	passEnds chan time.Duration
	release  chan struct{}
	done     chan struct{}
}

func newRunStepClock(floor time.Duration) *runStepClock {
	return &runStepClock{
		splitReadClock: &splitReadClock{now: messagingTestTime},
		floor:          floor,
		passEnds:       make(chan time.Duration),
		release:        make(chan struct{}),
		done:           make(chan struct{}),
	}
}

func (c *runStepClock) NewTimer(delay time.Duration) Timer {
	if delay < c.floor {
		return c.splitReadClock.NewTimer(delay)
	}
	select {
	case c.passEnds <- delay:
		select {
		case <-c.release:
		case <-c.done:
		}
	case <-c.done:
	}
	fired := make(firedTimer, 1)
	fired <- time.Time{}
	return fired
}

// firedTimer is a Timer that has already fired.
type firedTimer chan time.Time

func (t firedTimer) C() <-chan time.Time { return t }

func (firedTimer) Stop() bool { return false }

// steppedRun runs Run on a runStepClock, one pass per call to pass.
type steppedRun struct {
	service  *MessageService
	clock    *runStepClock
	ctx      context.Context
	cancel   context.CancelFunc
	started  bool
	finished chan struct{}
	// err is Run's result, set before finished closes.
	err error
}

func newSteppedRun(service *MessageService, clock *runStepClock) *steppedRun {
	ctx, cancel := context.WithCancel(context.Background())
	return &steppedRun{
		service:  service,
		clock:    clock,
		ctx:      ctx,
		cancel:   cancel,
		finished: make(chan struct{}),
	}
}

// pass starts Run, or releases its next pass, and returns the wait Run asks
// for when the pass ends. Run returning instead is an error.
func (r *steppedRun) pass() (time.Duration, error) {
	if !r.started {
		r.started = true
		go func() {
			r.err = r.service.Run(r.ctx)
			close(r.finished)
		}()
	} else {
		select {
		case r.clock.release <- struct{}{}:
		case <-r.finished:
			return 0, fmt.Errorf("Run() returned %v; want it to keep dispatching", r.err)
		}
	}
	select {
	case wait := <-r.clock.passEnds:
		return wait, nil
	case <-r.finished:
		return 0, fmt.Errorf("Run() returned %v; want it to keep dispatching", r.err)
	case <-time.After(10 * time.Second):
		return 0, errors.New("Run() did not finish its pass within 10 s")
	}
}

// stop cancels Run and checks that it returns context.Canceled.
func (r *steppedRun) stop() error {
	r.cancel()
	close(r.clock.done)
	if !r.started {
		return nil
	}
	select {
	case <-r.finished:
	case <-time.After(10 * time.Second):
		return errors.New("Run() did not return within 10 s of cancellation")
	}
	if !errors.Is(r.err, context.Canceled) {
		return fmt.Errorf("Run() = %v after cancellation, want context.Canceled", r.err)
	}
	return nil
}

// dueAt reports whether a pass at now must lease item: it is queued or not
// dispatched and due, or it holds an expired lease from before its call.
func dueAt(item sqlite.OutboxItem, now time.Time) bool {
	nowMS := now.UnixMilli()
	switch item.State {
	case sqlite.OutboxQueued, sqlite.OutboxNotDispatched:
		return item.ScheduledForMS <= nowMS && (item.NextAttemptAtMS == nil || *item.NextAttemptAtMS <= nowMS)
	case sqlite.OutboxDispatching:
		return item.LeaseExpiresAtMS != nil && *item.LeaseExpiresAtMS <= nowMS && item.TransportCalledAtMS == nil
	default:
		return false
	}
}

// setAsideAfterLeaseFailure reports whether item is how LeaseDueBatch leaves a
// due row whose lease failed at now: not dispatched with the lease error,
// due again later, its transport call never started.
func setAsideAfterLeaseFailure(item sqlite.OutboxItem, now time.Time) bool {
	return item.State == sqlite.OutboxNotDispatched &&
		item.ErrorClass != nil && *item.ErrorClass == string(bridge.FailureTransient) &&
		item.ErrorCode != nil && *item.ErrorCode == sqlite.LeaseFailedCode &&
		item.NextAttemptAtMS != nil && *item.NextAttemptAtMS > now.UnixMilli() &&
		item.TransportCalledAtMS == nil && item.LeaseToken == nil
}

// settleTime is the later of earliest and the time every row in items is due
// again.
func settleTime(earliest time.Time, items ...sqlite.OutboxItem) time.Time {
	for _, item := range items {
		if item.NextAttemptAtMS != nil && time.UnixMilli(*item.NextAttemptAtMS).After(earliest) {
			earliest = time.UnixMilli(*item.NextAttemptAtMS)
		}
	}
	return earliest
}

func setDispatchFault(raw *sql.DB, fault dispatchFault) error {
	if _, err := raw.Exec(`DROP TRIGGER IF EXISTS property_fault`); err != nil {
		return fmt.Errorf("drop fault trigger: %w", err)
	}
	if fault == noDispatchFault {
		return nil
	}
	if _, err := raw.Exec(`CREATE TRIGGER property_fault BEFORE ` + dispatchFaultConditions[fault] + `
		BEGIN
			SELECT RAISE(ABORT, '` + injectedDispatchFault + `');
		END`); err != nil {
		return fmt.Errorf("create fault trigger: %w", err)
	}
	return nil
}

// updatedAtByRow maps each row of checkedTables, named by its table, rowid
// and created_at_ms, to its updated_at_ms.
func updatedAtByRow(raw *sql.DB) (map[string]int64, error) {
	byRow := make(map[string]int64)
	for _, table := range checkedTables {
		rows, err := raw.Query(`SELECT rowid, created_at_ms, updated_at_ms FROM ` + table)
		if err != nil {
			return nil, fmt.Errorf("read %s updated_at_ms: %w", table, err)
		}
		for rows.Next() {
			var rowID, createdAtMS, updatedAtMS int64
			if err := rows.Scan(&rowID, &createdAtMS, &updatedAtMS); err != nil {
				rows.Close()
				return nil, fmt.Errorf("scan %s updated_at_ms: %w", table, err)
			}
			byRow[fmt.Sprintf("%s row %d created at %d", table, rowID, createdAtMS)] = updatedAtMS
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, fmt.Errorf("read %s updated_at_ms: %w", table, err)
		}
	}
	return byRow, nil
}

// checkUpdatedAt checks that no row of checkedTables has updated_at_ms below
// its created_at_ms, or below its value in before, from updatedAtByRow.
func checkUpdatedAt(raw *sql.DB, before map[string]int64) error {
	after, err := updatedAtByRow(raw)
	if err != nil {
		return err
	}
	for row, updatedAtMS := range after {
		if was, ok := before[row]; ok && updatedAtMS < was {
			return fmt.Errorf("%s: updated_at_ms fell from %d to %d", row, was, updatedAtMS)
		}
	}
	for _, table := range checkedTables {
		var count int
		if err := raw.QueryRow(
			`SELECT COUNT(*) FROM ` + table + ` WHERE updated_at_ms < created_at_ms`,
		).Scan(&count); err != nil {
			return fmt.Errorf("count %s rows updated before creation: %w", table, err)
		}
		if count != 0 {
			return fmt.Errorf("%d %s rows have updated_at_ms below created_at_ms", count, table)
		}
	}
	return nil
}
