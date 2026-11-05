package messaging

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
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

// rowFault names how a trigger fails one queued text's outbox writes during
// one pass.
type rowFault int

const (
	noRowFault rowFault = iota
	// abortedLeaseRowFault fails the text's lease; its set-aside goes
	// through.
	abortedLeaseRowFault
	// skippedLeaseRowFault skips the text's lease (RAISE(IGNORE)); its
	// set-aside goes through.
	skippedLeaseRowFault
	// stuckRowFault fails every write to the text while it is due, so the
	// store can neither lease it nor set it aside.
	stuckRowFault
	// skippedStuckRowFault skips every write to the text while it is due.
	skippedStuckRowFault
	// settleRowFault fails Confirm and MarkStoreFailed after the text's
	// transport call, so the store loses the call's outcome.
	settleRowFault
	// failedRecoveryRowFault fails the recovery of the text's expired lease,
	// so the text keeps it.
	failedRecoveryRowFault
	rowFaultCount
)

// rowFaultTriggers holds each fault's trigger event, with the text's outbox
// ID as %s, and its action.
var rowFaultTriggers = map[rowFault]struct{ event, action string }{
	abortedLeaseRowFault: {
		`UPDATE OF state ON outbox WHEN NEW.state = 'dispatching' AND OLD.outbox_id = '%s'`,
		`RAISE(ABORT, '` + injectedDispatchFault + `')`,
	},
	skippedLeaseRowFault: {
		`UPDATE OF state ON outbox WHEN NEW.state = 'dispatching' AND OLD.outbox_id = '%s'`,
		`RAISE(IGNORE)`,
	},
	stuckRowFault: {
		`UPDATE ON outbox WHEN OLD.state IN ('queued', 'not_dispatched') AND OLD.outbox_id = '%s'`,
		`RAISE(ABORT, '` + injectedDispatchFault + `')`,
	},
	skippedStuckRowFault: {
		`UPDATE ON outbox WHEN OLD.state IN ('queued', 'not_dispatched') AND OLD.outbox_id = '%s'`,
		`RAISE(IGNORE)`,
	},
	settleRowFault: {
		`UPDATE OF state ON outbox WHEN NEW.state IN ('confirmed', 'store_failed') AND OLD.outbox_id = '%s'`,
		`RAISE(ABORT, '` + injectedDispatchFault + `')`,
	},
	failedRecoveryRowFault: {
		`UPDATE OF state ON outbox WHEN OLD.state = 'dispatching'
			AND NEW.state IN ('not_dispatched', 'uncertain') AND OLD.outbox_id = '%s'`,
		`RAISE(ABORT, '` + injectedDispatchFault + `')`,
	},
}

const maxLeaseScriptTexts = 6

// leaseScript holds one generated history: the texts the test queues over
// several passes, and the dispatcher's batch limit. Before each pass the clock
// steps (back, a little, or past several retry delays) and the test may queue
// new texts; during the pass, each queued text's writes may fail. Beside
// that, each recovery of text i's expired lease fails in every pass before
// RecoveryFailsUntil[i], as it would on a row with a CHECK it cannot meet,
// until someone repairs it.
type leaseScript struct {
	Limit              int
	Passes             []leaseScriptPass
	RecoveryFailsUntil []int
}

type leaseScriptPass struct {
	ClockStepMS int64
	// NewTexts is how many texts SendText queues just before the pass.
	NewTexts int
	// Faults holds a fault for each text queued so far, oldest first.
	Faults []rowFault
}

func (leaseScript) Generate(r *rand.Rand, _ int) reflect.Value {
	// Mostly limits below the number of texts, so the batch fills and the
	// rows after the last lease wait for a later pass.
	script := leaseScript{Limit: defaultBatchLimit}
	if r.Intn(4) != 0 {
		script.Limit = 1 + r.Intn(4)
	}
	texts := 0
	for index := range 1 + r.Intn(10) {
		pass := leaseScriptPass{}
		switch {
		case index == 0:
			pass.NewTexts = 1 + r.Intn(3)
		case r.Intn(3) == 0:
			pass.NewTexts = 1 + r.Intn(2)
		}
		pass.NewTexts = min(pass.NewTexts, maxLeaseScriptTexts-texts)
		texts += pass.NewTexts
		pass.Faults = make([]rowFault, texts)
		for text := range pass.Faults {
			if r.Intn(2) == 0 {
				pass.Faults[text] = rowFault(1 + r.Intn(int(rowFaultCount)-1))
			}
		}
		switch r.Intn(4) {
		case 0:
			pass.ClockStepMS = -int64(r.Intn(2_000))
		case 1:
			pass.ClockStepMS = int64(r.Intn(int((6 * time.Minute).Milliseconds())))
		default:
			pass.ClockStepMS = int64(r.Intn(12_000))
		}
		script.Passes = append(script.Passes, pass)
	}
	script.RecoveryFailsUntil = make([]int, maxLeaseScriptTexts)
	for text := range script.RecoveryFailsUntil {
		if r.Intn(2) == 0 {
			script.RecoveryFailsUntil[text] = 1 + r.Intn(len(script.Passes))
		}
	}
	return reflect.ValueOf(script)
}

// TestLeaseFailuresNeverHoldOtherSends checks, over generated histories of
// several texts whose writes fail in any pass and in any of the ways a row's
// write can fail, with batch limits mostly below the number of texts, that
// each pass does what a model of it predicts. The model walks the due texts
// in due order: the store sets aside a text whose lease fails, leaves as it
// was a text it can neither lease nor set aside, and leases every other text,
// until the batch fills; the texts after that wait. Then the dispatcher sends
// the leased texts in order, until one loses its outcome. So:
//
//   - one row's lease failure never blocks other due rows: the dispatcher
//     leases and sends the first due texts whose writes work, up to the batch
//     limit, whatever fails on the texts before them;
//   - the store sets aside a due text whose lease fails, and the text comes
//     due again 5 s to 5 minutes later, never later than twice its last such
//     delay, with no transport call; the model reckons the delay from what it
//     remembers of the text's last set-aside, not from the row;
//   - a due text the store can neither lease nor set aside stays as it was,
//     and fails its pass; nothing else fails a pass but a lost outcome;
//   - a text whose expired lease the store fails to recover stays as it was,
//     holds back no other text and fails no pass, and DispatchHealth counts
//     each such failure once;
//   - no fix path calls the transport again for an intent a call may have
//     sent: each text reaches the transport at most once, as these
//     transports never prove a call sent nothing, and a called text never
//     becomes due again;
//   - a text the pass does not reach changes only when its called lease
//     expires and it becomes uncertain, and updated_at_ms never moves back or
//     below created_at_ms;
//   - once the faults stop and every retry comes due, each text ends
//     confirmed, or uncertain when the store lost its outcome, after one
//     transport call and no second.
func TestLeaseFailuresNeverHoldOtherSends(t *testing.T) {
	property := func(script leaseScript) bool {
		if err := runLeaseScript(t, script); err != nil {
			t.Logf("script %+v: %v", script, err)
			return false
		}
		return true
	}
	if err := quick.Check(property, dispatchPropertyConfig(t, 60, 8)); err != nil {
		t.Fatal(err)
	}
}

// leaseChain holds what the model remembers of a text whose last write was a
// set-aside: the delay that set-aside gave it, and how far the clock then ran
// behind the text's updated_at_ms.
type leaseChain struct {
	delayMS, behindMS int64
}

func runLeaseScript(t *testing.T, script leaseScript) error {
	ctx := context.Background()
	clock := &splitReadClock{now: messagingTestTime}
	store, raw := openReconcileTestStore(t, clock.Now())
	var steps []sendStep
	for text := range maxLeaseScriptTexts {
		steps = append(steps, sendStep{result: bridge.SendResult{RemoteMessageID: fmt.Sprintf("remote-%d", text)}})
	}
	sender := &scriptedTextSender{steps: steps}
	registry := newScriptedRegistry("lease-property", sender)
	registry.setAvailable(true)
	service, err := NewMessageService(store, registry, nil, clock, &sequentialIDs{})
	if err != nil {
		return fmt.Errorf("NewMessageService: %w", err)
	}
	service.batchLimit = script.Limit

	var texts []Submission
	lostOutcome := make(map[string]bool)
	chains := make(map[string]leaseChain)
	now := messagingTestTime.Add(time.Second)
	latest := now
	for index, pass := range script.Passes {
		if index > 0 {
			now = now.Add(time.Duration(pass.ClockStepMS) * time.Millisecond)
		}
		if now.After(latest) {
			latest = now
		}
		clock.set(now, 0)
		if err := setRowFaults(raw, texts, nil); err != nil {
			return err
		}
		for range pass.NewTexts {
			submission, err := service.SendText(ctx, SendTextCommand{
				CommonCommand: testCommonCommand(fmt.Sprintf("key-%d", len(texts))),
				Body:          fmt.Sprintf("text %d", len(texts)),
			})
			if err != nil {
				return fmt.Errorf("pass %d: SendText: %w", index, err)
			}
			texts = append(texts, submission)
		}
		if err := setRowFaults(raw, texts, pass.Faults); err != nil {
			return err
		}
		recoveryFails := make([]bool, len(texts))
		for text := range texts {
			recoveryFails[text] = index < script.RecoveryFailsUntil[text]
		}
		if err := setRecoveryFaults(raw, texts, recoveryFails); err != nil {
			return err
		}
		before, err := outboxRows(ctx, service, texts)
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
		after, err := outboxRows(ctx, service, texts)
		if err != nil {
			return fmt.Errorf("pass %d: %w", index, err)
		}
		requestsAfter := requestsByID(sender)
		failedRecoveries := service.DispatchHealth().FailedRecoveries - recoveriesBefore
		if err := checkLeasePass(now, script.Limit, pass.Faults, recoveryFails, before, after,
			requestsBefore, requestsAfter, passErr, failedRecoveries, lostOutcome, chains); err != nil {
			return fmt.Errorf("pass %d at %d: %w", index, now.UnixMilli(), err)
		}
		if err := checkUpdatedAt(raw, updatedAt); err != nil {
			return fmt.Errorf("pass %d: %w", index, err)
		}
	}

	// The faults stop, every lease expires and every retry comes due. Each
	// pass sends up to the batch limit.
	if err := setRowFaults(raw, texts, nil); err != nil {
		return err
	}
	if err := setRecoveryFaults(raw, texts, nil); err != nil {
		return err
	}
	rows, err := outboxRows(ctx, service, texts)
	if err != nil {
		return err
	}
	clock.set(settleTime(latest.Add(service.leaseTime), rows...).Add(time.Second), 0)
	for index := 0; ; index++ {
		updatedAt, err := updatedAtByRow(raw)
		if err != nil {
			return fmt.Errorf("pass %d after the faults stopped: %w", index, err)
		}
		if err := service.dispatchPass(ctx); err != nil {
			return fmt.Errorf("pass %d after the faults stopped: %w", index, err)
		}
		if err := checkUpdatedAt(raw, updatedAt); err != nil {
			return fmt.Errorf("pass %d after the faults stopped: %w", index, err)
		}
		if rows, err = outboxRows(ctx, service, texts); err != nil {
			return err
		}
		if !slices.ContainsFunc(rows, func(row sqlite.OutboxItem) bool {
			return row.State != sqlite.OutboxConfirmed && row.State != sqlite.OutboxUncertain
		}) {
			break
		}
		if index > maxLeaseScriptTexts {
			return fmt.Errorf("texts still pending %d passes after the faults stopped: %+v", index+1, rows)
		}
	}
	requests := requestsByID(sender)
	for _, row := range rows {
		want := sqlite.OutboxConfirmed
		if lostOutcome[row.OutboxID] {
			want = sqlite.OutboxUncertain
		}
		if row.State != want || requests[row.TransportRequestID] != 1 {
			return fmt.Errorf("final row %+v after %d transport requests, want %s after 1",
				row, requests[row.TransportRequestID], want)
		}
	}
	return nil
}

// What a pass does to one text, as the model predicts it.
type leaseOutcome int

const (
	// The pass does not reach the text: it is not due, or due after the
	// batch filled.
	unreachedLeaseOutcome leaseOutcome = iota
	// Its called lease expired, and recovery makes it uncertain.
	recoveredLeaseOutcome
	// Its called lease expired, and its recovery fails, so it keeps the
	// lease.
	unrecoveredLeaseOutcome
	setAsideLeaseOutcome
	stuckLeaseOutcome
	sentLeaseOutcome
	// Leased and called, but the store lost its outcome.
	lostLeaseOutcome
	// Leased after another text lost its outcome, and released uncalled.
	releasedLeaseOutcome
)

// checkLeasePass checks one pass against the model in
// TestLeaseFailuresNeverHoldOtherSends, and updates what the model remembers:
// the texts whose outcome the store lost, and each text's last set-aside.
func checkLeasePass(
	now time.Time,
	limit int,
	faults []rowFault,
	recoveryFails []bool,
	before, after []sqlite.OutboxItem,
	requestsBefore, requestsAfter map[string]int,
	passErr error,
	failedRecoveries int64,
	lostOutcome map[string]bool,
	chains map[string]leaseChain,
) error {
	faultOf := func(text int) rowFault {
		if text < len(faults) {
			return faults[text]
		}
		return noRowFault
	}
	nowMS := now.UnixMilli()

	outcomes := make([]leaseOutcome, len(before))
	var due []int
	var unrecovered int64
	for text, was := range before {
		switch {
		case expiredCalledLease(was, now) && (faultOf(text) == failedRecoveryRowFault || recoveryFails[text]):
			outcomes[text] = unrecoveredLeaseOutcome
			unrecovered++
		case expiredCalledLease(was, now):
			outcomes[text] = recoveredLeaseOutcome
		case was.State == sqlite.OutboxDispatching && dueAt(was, now):
			// Every pass here sends or releases each lease it takes.
			return fmt.Errorf("text %d holds an expired lease whose call never started: %+v", text, was)
		case dueAt(was, now):
			due = append(due, text)
		}
	}
	slices.SortFunc(due, func(a, b int) int { return compareDueOrder(before[a], before[b]) })
	var leased []int
	stuckMet := false
	for _, text := range due {
		if len(leased) == limit {
			break
		}
		switch faultOf(text) {
		case abortedLeaseRowFault, skippedLeaseRowFault:
			outcomes[text] = setAsideLeaseOutcome
		case stuckRowFault, skippedStuckRowFault:
			outcomes[text] = stuckLeaseOutcome
			stuckMet = true
		default:
			leased = append(leased, text)
		}
	}
	lost := false
	for _, text := range leased {
		switch {
		case lost:
			outcomes[text] = releasedLeaseOutcome
		case faultOf(text) == settleRowFault:
			outcomes[text] = lostLeaseOutcome
			lost = true
		default:
			outcomes[text] = sentLeaseOutcome
		}
	}

	switch {
	case (lost || stuckMet) && passErr == nil:
		return fmt.Errorf("pass succeeded, want it to fail (outcome lost %t, stuck text met %t)", lost, stuckMet)
	case !lost && !stuckMet && passErr != nil:
		return fmt.Errorf("pass failed with no stuck text met and no outcome lost: %w", passErr)
	case passErr != nil && !strings.Contains(passErr.Error(), injectedDispatchFault) &&
		!strings.Contains(passErr.Error(), "the update changed no row"):
		return fmt.Errorf("pass failed for a reason no fault explains: %w", passErr)
	case failedRecoveries != unrecovered:
		return fmt.Errorf("pass counted %d failed recoveries, want %d", failedRecoveries, unrecovered)
	}

	for text, was := range before {
		row := after[text]
		requests := requestsAfter[was.TransportRequestID]
		called := requests - requestsBefore[was.TransportRequestID]
		switch {
		case requests > 1:
			return fmt.Errorf("text %d reached the transport %d times", text, requests)
		case requests > 0 && (row.State == sqlite.OutboxQueued || row.State == sqlite.OutboxNotDispatched):
			return fmt.Errorf("text %d was called and is due again: %+v", text, row)
		case row.UpdatedAtMS < was.UpdatedAtMS:
			return fmt.Errorf("text %d: updated_at_ms moved back from %d to %d", text, was.UpdatedAtMS, row.UpdatedAtMS)
		case row.AttemptCount < was.AttemptCount:
			return fmt.Errorf("text %d: attempt_count moved back from %d to %d", text, was.AttemptCount, row.AttemptCount)
		}

		switch outcomes[text] {
		case unreachedLeaseOutcome, stuckLeaseOutcome, unrecoveredLeaseOutcome:
			if !reflect.DeepEqual(row, was) {
				return fmt.Errorf("text %d the pass should leave as it was (outcome %d) changed from %+v to %+v",
					text, outcomes[text], was, row)
			}
		case recoveredLeaseOutcome:
			if row.State != sqlite.OutboxUncertain || called != 0 {
				return fmt.Errorf("text %d whose called lease expired = %+v after %d calls, want uncertain", text, row, called)
			}
		case setAsideLeaseOutcome:
			chain, chained := chains[was.OutboxID]
			delayMS := modelLeaseRetryDelayMS(chain, chained)
			if !setAsideAfterLeaseFailure(row, now) || called != 0 ||
				row.AttemptCount != was.AttemptCount || row.UpdatedAtMS != max(was.UpdatedAtMS, nowMS) ||
				*row.NextAttemptAtMS != nowMS+delayMS {
				return fmt.Errorf("due text %d whose lease failed = %+v, want it set aside for %d ms from %+v",
					text, row, delayMS, was)
			}
			ladderMS := int64(5_000)
			if chained {
				ladderMS = min(max(2*chain.delayMS, 5_000), 300_000)
			}
			if delayMS < 5_000 || delayMS > ladderMS {
				return fmt.Errorf("due text %d set aside for %d ms, want 5000 to %d", text, delayMS, ladderMS)
			}
			chains[was.OutboxID] = leaseChain{delayMS: delayMS, behindMS: max(was.UpdatedAtMS-nowMS, 0)}
		case sentLeaseOutcome:
			if row.State != sqlite.OutboxConfirmed || called != 1 {
				return fmt.Errorf("due text %d with no fault = %+v after %d calls, want it sent", text, row, called)
			}
			delete(chains, was.OutboxID)
		case lostLeaseOutcome:
			if called != 1 || row.State != sqlite.OutboxDispatching || row.TransportCalledAtMS == nil {
				return fmt.Errorf("text %d whose outcome was lost = %+v after %d calls, want its called lease kept",
					text, row, called)
			}
			lostOutcome[was.OutboxID] = true
			delete(chains, was.OutboxID)
		case releasedLeaseOutcome:
			if row.State != sqlite.OutboxQueued || called != 0 {
				return fmt.Errorf("due text %d leased after a lost outcome = %+v after %d calls, want it released uncalled",
					text, row, called)
			}
			delete(chains, was.OutboxID)
		}
	}
	return nil
}

// modelLeaseRetryDelayMS returns the delay the model expects a text's
// set-aside to give it, from what the model remembers of the text's last
// set-aside: 5 s with none, else twice the last delay, less twice how far the
// clock then ran behind the text's updated_at_ms, at least 5 s and at most 5
// minutes; 5 s when nothing of the last delay remains.
func modelLeaseRetryDelayMS(chain leaseChain, chained bool) int64 {
	const first, longest = int64(5_000), int64(300_000)
	if !chained || chain.delayMS <= chain.behindMS {
		return first
	}
	return min(max(2*(chain.delayMS-chain.behindMS), first), longest)
}

// compareDueOrder orders due rows as LeaseDueBatch meets them.
func compareDueOrder(a, b sqlite.OutboxItem) int {
	dueAtMS := func(item sqlite.OutboxItem) int64 {
		if item.NextAttemptAtMS != nil {
			return *item.NextAttemptAtMS
		}
		return item.ScheduledForMS
	}
	return cmp.Or(
		cmp.Compare(dueAtMS(a), dueAtMS(b)),
		cmp.Compare(a.CreatedAtMS, b.CreatedAtMS),
		strings.Compare(a.OutboxID, b.OutboxID),
	)
}

// expiredCalledLease reports whether a pass at now recovers item as
// uncertain: its lease expired after its transport call started.
func expiredCalledLease(item sqlite.OutboxItem, now time.Time) bool {
	return item.State == sqlite.OutboxDispatching && item.TransportCalledAtMS != nil &&
		item.LeaseExpiresAtMS != nil && *item.LeaseExpiresAtMS <= now.UnixMilli()
}

// setRowFaults replaces the fault triggers with faults, one per text in
// texts; nil faults drops them all.
func setRowFaults(raw *sql.DB, texts []Submission, faults []rowFault) error {
	for text := range maxLeaseScriptTexts {
		if _, err := raw.Exec(fmt.Sprintf(`DROP TRIGGER IF EXISTS row_fault_%d`, text)); err != nil {
			return fmt.Errorf("drop fault trigger: %w", err)
		}
	}
	for text, fault := range faults {
		if fault == noRowFault {
			continue
		}
		trigger := rowFaultTriggers[fault]
		if _, err := raw.Exec(fmt.Sprintf(`CREATE TRIGGER row_fault_%d BEFORE %s
			BEGIN
				SELECT %s;
			END`, text, fmt.Sprintf(trigger.event, texts[text].OutboxID), trigger.action)); err != nil {
			return fmt.Errorf("create fault trigger: %w", err)
		}
	}
	return nil
}

// setRecoveryFaults replaces the triggers that fail the recovery of a text's
// expired lease with one for each text whose recoveryFails entry is set; nil
// drops them all.
func setRecoveryFaults(raw *sql.DB, texts []Submission, recoveryFails []bool) error {
	for text := range maxLeaseScriptTexts {
		if _, err := raw.Exec(fmt.Sprintf(`DROP TRIGGER IF EXISTS recovery_fault_%d`, text)); err != nil {
			return fmt.Errorf("drop recovery fault trigger: %w", err)
		}
	}
	trigger := rowFaultTriggers[failedRecoveryRowFault]
	for text, fails := range recoveryFails {
		if !fails {
			continue
		}
		if _, err := raw.Exec(fmt.Sprintf(`CREATE TRIGGER recovery_fault_%d BEFORE %s
			BEGIN
				SELECT %s;
			END`, text, fmt.Sprintf(trigger.event, texts[text].OutboxID), trigger.action)); err != nil {
			return fmt.Errorf("create recovery fault trigger: %w", err)
		}
	}
	return nil
}

func outboxRows(ctx context.Context, service *MessageService, texts []Submission) ([]sqlite.OutboxItem, error) {
	rows := make([]sqlite.OutboxItem, 0, len(texts))
	for _, text := range texts {
		row, err := service.outbox.FindByID(ctx, text.OutboxID)
		if err != nil {
			return nil, fmt.Errorf("read row %q: %w", text.OutboxID, err)
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// requestsByID counts the sender's requests by transport request ID.
func requestsByID(sender *scriptedTextSender) map[string]int {
	counts := make(map[string]int)
	for _, request := range sender.snapshotRequests() {
		counts[request.RequestID]++
	}
	return counts
}
