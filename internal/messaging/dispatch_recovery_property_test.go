package messaging

import (
	"context"
	"database/sql"
	"fmt"
	"maps"
	"math/rand"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/quick"
	"time"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

// strandedScript is one generated history for
// TestRecoveryFailuresNeverHoldOtherSends: leases that crashed passes left
// behind, texts queued after them, the dispatcher's batch limit, and passes.
type strandedScript struct {
	Stranded []strandedText
	Queued   int
	Limit    int
	Passes   []strandedPass
}

type strandedText struct {
	// Called: the crashed pass had started the text's transport call.
	Called bool
	// RecoveryFailsUntil: the recovery of the text's expired lease fails in
	// every pass before this one, as it would on a row with a CHECK it cannot
	// meet, until someone repairs it.
	RecoveryFailsUntil int
}

type strandedPass struct {
	ClockStepMS int64
	// StoreError: recovery meets a store error (SQLITE_ERROR) on each expired
	// lease whose own recovery does not fail first.
	StoreError bool
}

func (strandedScript) Generate(r *rand.Rand, _ int) reflect.Value {
	script := strandedScript{Queued: r.Intn(4), Limit: 1 + r.Intn(3)}
	passes := 1 + r.Intn(8)
	for range 1 + r.Intn(4) {
		text := strandedText{Called: r.Intn(2) == 0}
		if r.Intn(3) != 0 {
			text.RecoveryFailsUntil = 1 + r.Intn(passes)
		}
		script.Stranded = append(script.Stranded, text)
	}
	for range passes {
		pass := strandedPass{StoreError: r.Intn(6) == 0}
		switch r.Intn(4) {
		case 0:
			pass.ClockStepMS = -int64(r.Intn(1_500))
		case 1:
			pass.ClockStepMS = int64(r.Intn(60_000))
		default:
			pass.ClockStepMS = int64(r.Intn(3_000))
		}
		script.Passes = append(script.Passes, pass)
	}
	return reflect.ValueOf(script)
}

// TestRecoveryFailuresNeverHoldOtherSends checks, over generated histories
// that start with leases crashed passes left behind, before or after their
// transport call, and texts queued after them, that a lease whose recovery
// keeps failing holds back nothing. A model predicts each pass: it ends each
// expired lease whose recovery works (uncertain when its call had started,
// due at once otherwise), leaves each whose recovery fails as it was, and
// then leases and sends the first due texts, up to the batch limit. So, in
// every pass:
//
//   - a failed recovery neither fails the pass nor holds back another row:
//     the pass recovers the other expired leases and sends the first due
//     texts, recovered ones among them;
//   - a lease whose recovery failed stays as it was, and DispatchHealth
//     counts each such failure once;
//   - a text whose transport call had started never reaches the transport
//     again, and no text reaches it twice;
//   - a store error in recovery fails the whole pass, which changes no row
//     and counts no failed recovery;
//   - once the faults stop, every stranded lease ends: uncertain, never
//     resent, when its call had started, else confirmed after one transport
//     call; and every queued text ends confirmed after one.
func TestRecoveryFailuresNeverHoldOtherSends(t *testing.T) {
	property := func(script strandedScript) bool {
		if err := runStrandedScript(t, script); err != nil {
			t.Logf("script %+v: %v", script, err)
			return false
		}
		return true
	}
	if err := quick.Check(property, dispatchPropertyConfig(t, 60, 8)); err != nil {
		t.Fatal(err)
	}
}

func runStrandedScript(t *testing.T, script strandedScript) error {
	ctx := context.Background()
	clock := newManualClock(messagingTestTime)
	store, raw := openReconcileTestStore(t, clock.Now())
	texts := len(script.Stranded) + script.Queued
	steps := make([]sendStep, texts)
	for text := range steps {
		steps[text] = sendStep{result: bridge.SendResult{RemoteMessageID: fmt.Sprintf("remote-%d", text)}}
	}
	sender := &scriptedTextSender{steps: steps}
	registry := newScriptedRegistry("recovery-property", sender)
	registry.setAvailable(true)
	service, err := NewMessageService(store, registry, nil, clock, &sequentialIDs{})
	if err != nil {
		return fmt.Errorf("NewMessageService: %w", err)
	}
	service.batchLimit = script.Limit

	var ids []string
	for index, text := range script.Stranded {
		ids = append(ids, strandTextLease(t, service, clock, fmt.Sprintf("key-stranded-%d", index), text.Called).OutboxID)
	}
	for index := range script.Queued {
		ids = append(ids, mustSendText(t, service, SendTextCommand{
			CommonCommand: testCommonCommand(fmt.Sprintf("key-queued-%d", index)),
			Body:          fmt.Sprintf("queued text %d", index),
		}).OutboxID)
		clock.Advance(time.Millisecond)
	}
	rows := func() ([]sqlite.OutboxItem, error) {
		items := make([]sqlite.OutboxItem, 0, len(ids))
		for _, id := range ids {
			item, err := service.outbox.FindByID(ctx, id)
			if err != nil {
				return nil, fmt.Errorf("read row %q: %w", id, err)
			}
			items = append(items, item)
		}
		return items, nil
	}

	// Every stranded lease lasted a second, so it has expired by the first
	// pass, unless the clock steps back past its expiry.
	clock.Advance(2 * time.Second)
	latest := clock.Now()
	for index, pass := range script.Passes {
		if index > 0 {
			clock.Advance(time.Duration(pass.ClockStepMS) * time.Millisecond)
		}
		now := clock.Now()
		if now.After(latest) {
			latest = now
		}
		failing := make(map[string]bool)
		for text, stranded := range script.Stranded {
			if index < stranded.RecoveryFailsUntil {
				failing[ids[text]] = true
			}
		}
		if err := setStrandedFaults(raw, failing, pass.StoreError); err != nil {
			return err
		}
		before, err := rows()
		if err != nil {
			return fmt.Errorf("pass %d: %w", index, err)
		}
		updatedAt, err := updatedAtByRow(raw)
		if err != nil {
			return fmt.Errorf("pass %d: %w", index, err)
		}
		requestsBefore := requestsByID(sender)
		recoveriesBefore := service.DispatchHealth().FailedRecoveries
		passErr := service.dispatchPass(ctx)
		after, err := rows()
		if err != nil {
			return fmt.Errorf("pass %d: %w", index, err)
		}
		if err := checkStrandedPass(now, script.Limit, pass.StoreError, failing, before, after,
			requestsBefore, requestsByID(sender), passErr,
			service.DispatchHealth().FailedRecoveries-recoveriesBefore); err != nil {
			return fmt.Errorf("pass %d at +%d ms: %w", index, now.Sub(messagingTestTime).Milliseconds(), err)
		}
		if err := checkUpdatedAt(raw, updatedAt); err != nil {
			return fmt.Errorf("pass %d: %w", index, err)
		}
	}

	// The faults stop, and the clock passes every lease and retry. Each pass
	// sends up to the batch limit.
	if err := setStrandedFaults(raw, nil, false); err != nil {
		return err
	}
	settled, err := rows()
	if err != nil {
		return err
	}
	clock.Advance(settleTime(latest.Add(service.leaseTime), settled...).Add(time.Second).Sub(clock.Now()))
	recoveries := service.DispatchHealth().FailedRecoveries
	for index := 0; ; index++ {
		if err := service.dispatchPass(ctx); err != nil {
			return fmt.Errorf("pass %d after the faults stopped: %w", index, err)
		}
		if settled, err = rows(); err != nil {
			return err
		}
		if !slices.ContainsFunc(settled, func(row sqlite.OutboxItem) bool {
			return row.State != sqlite.OutboxConfirmed && row.State != sqlite.OutboxUncertain
		}) {
			break
		}
		if index > texts {
			return fmt.Errorf("texts still pending %d passes after the faults stopped: %+v", index+1, settled)
		}
	}
	if got := service.DispatchHealth().FailedRecoveries; got != recoveries {
		return fmt.Errorf("%d recoveries failed after the faults stopped", got-recoveries)
	}
	requests := requestsByID(sender)
	for text, row := range settled {
		want, wantRequests := sqlite.OutboxConfirmed, 1
		if text < len(script.Stranded) && script.Stranded[text].Called {
			want, wantRequests = sqlite.OutboxUncertain, 0
		}
		if row.State != want || requests[row.TransportRequestID] != wantRequests {
			return fmt.Errorf("final row %+v after %d transport requests, want %s after %d",
				row, requests[row.TransportRequestID], want, wantRequests)
		}
	}
	return nil
}

// checkStrandedPass checks one pass against the model in
// TestRecoveryFailuresNeverHoldOtherSends. failing holds the rows whose
// recovery fails in this pass, and failedRecoveries how many recovery
// failures DispatchHealth counted in it.
func checkStrandedPass(
	now time.Time,
	limit int,
	storeError bool,
	failing map[string]bool,
	before, after []sqlite.OutboxItem,
	requestsBefore, requestsAfter map[string]int,
	passErr error,
	failedRecoveries int64,
) error {
	nowMS := now.UnixMilli()
	for text, was := range before {
		requests := requestsAfter[was.TransportRequestID]
		switch {
		case requests > 1:
			return fmt.Errorf("text %d reached the transport %d times", text, requests)
		case was.TransportCalledAtMS != nil && requests > 0:
			return fmt.Errorf("text %d, whose transport call had started, reached the transport again", text)
		}
	}

	// The pass first ends every expired lease it can. A store error fails
	// recovery when it meets a lease whose own recovery does not fail first.
	var expired []int
	failsWhole := false
	for text, was := range before {
		if was.State == sqlite.OutboxDispatching && *was.LeaseExpiresAtMS <= nowMS {
			expired = append(expired, text)
			failsWhole = failsWhole || storeError && !failing[was.OutboxID]
		}
	}
	if failsWhole {
		if passErr == nil || !strings.Contains(passErr.Error(), "integer overflow") {
			return fmt.Errorf("pass error = %v, want the store's integer overflow", passErr)
		}
		if !reflect.DeepEqual(after, before) || failedRecoveries != 0 || !maps.Equal(requestsAfter, requestsBefore) {
			return fmt.Errorf("a pass whose recovery met a store error changed rows from %+v to %+v, "+
				"counted %d failed recoveries, or called the transport", before, after, failedRecoveries)
		}
		return nil
	}
	if passErr != nil {
		return fmt.Errorf("pass failed: %w", passErr)
	}
	predicted := slices.Clone(before)
	var unrecovered int64
	for _, text := range expired {
		switch {
		case failing[before[text].OutboxID]:
			unrecovered++
		case before[text].TransportCalledAtMS != nil:
			predicted[text].State = sqlite.OutboxUncertain
		default:
			predicted[text].State = sqlite.OutboxNotDispatched
			predicted[text].NextAttemptAtMS = &nowMS
		}
	}
	if failedRecoveries != unrecovered {
		return fmt.Errorf("pass counted %d failed recoveries, want %d", failedRecoveries, unrecovered)
	}

	// Then it leases and sends the first due texts, up to the batch limit.
	var due []int
	for text, row := range predicted {
		if (row.State == sqlite.OutboxQueued || row.State == sqlite.OutboxNotDispatched) && dueAt(row, now) {
			due = append(due, text)
		}
	}
	slices.SortFunc(due, func(a, b int) int { return compareDueOrder(predicted[a], predicted[b]) })
	sent := make(map[int]bool)
	for _, text := range due[:min(limit, len(due))] {
		sent[text] = true
	}
	for text, was := range before {
		row := after[text]
		called := requestsAfter[was.TransportRequestID] - requestsBefore[was.TransportRequestID]
		recovered := was.State == sqlite.OutboxDispatching && predicted[text].State != sqlite.OutboxDispatching
		switch {
		case sent[text]:
			if row.State != sqlite.OutboxConfirmed || called != 1 {
				return fmt.Errorf("due text %d = %+v after %d calls, want it sent", text, row, called)
			}
		case recovered && (row.State != predicted[text].State || called != 0 ||
			row.LeaseToken != nil || row.TransportCalledAtMS != nil || row.AttemptCount != was.AttemptCount ||
			predicted[text].NextAttemptAtMS != nil && (row.NextAttemptAtMS == nil || *row.NextAttemptAtMS != nowMS)):
			return fmt.Errorf("text %d whose expired lease the pass recovered = %+v after %d calls, want %s",
				text, row, called, predicted[text].State)
		case !recovered && (!reflect.DeepEqual(row, was) || called != 0):
			return fmt.Errorf("text %d the pass should leave as it was changed from %+v to %+v after %d calls",
				text, was, row, called)
		}
	}
	return nil
}

// setStrandedFaults replaces the recovery fault triggers: one that fails the
// recovery of each row in failing, and, when storeError is set, one that
// raises a store error on the recovery of every other row.
func setStrandedFaults(raw *sql.DB, failing map[string]bool, storeError bool) error {
	for _, name := range []string{"stranded_recovery_fault", "stranded_store_error"} {
		if _, err := raw.Exec(`DROP TRIGGER IF EXISTS ` + name); err != nil {
			return fmt.Errorf("drop trigger %s: %w", name, err)
		}
	}
	list := "'" + strings.Join(slices.Sorted(maps.Keys(failing)), "', '") + "'"
	recovery := `UPDATE OF state ON outbox
		WHEN OLD.state = 'dispatching' AND NEW.state IN ('not_dispatched', 'uncertain')`
	if len(failing) > 0 {
		if _, err := raw.Exec(`CREATE TRIGGER stranded_recovery_fault BEFORE ` + recovery + `
				AND OLD.outbox_id IN (` + list + `)
			BEGIN
				SELECT RAISE(ABORT, '` + injectedDispatchFault + `');
			END`); err != nil {
			return fmt.Errorf("create recovery fault trigger: %w", err)
		}
	}
	if storeError {
		if _, err := raw.Exec(`CREATE TRIGGER stranded_store_error BEFORE ` + recovery + `
				AND OLD.outbox_id NOT IN (` + list + `)
			BEGIN
				SELECT abs(-9223372036854775807 - 1);
			END`); err != nil {
			return fmt.Errorf("create store error trigger: %w", err)
		}
	}
	return nil
}
