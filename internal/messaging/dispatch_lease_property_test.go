package messaging

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"reflect"
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
	// stuckRowFault fails every write to the text while it is due, so it can
	// be neither leased nor set aside.
	stuckRowFault
	// skippedStuckRowFault skips every write to the text while it is due.
	skippedStuckRowFault
	// settleRowFault fails Confirm and MarkStoreFailed after the text's
	// transport call, so the call's outcome is lost.
	settleRowFault
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
}

const maxLeaseScriptTexts = 6

// leaseScript is one generated history of texts queued over several passes.
// Before each pass the clock steps (back, a little, or past several retry
// delays) and new texts may be queued; during the pass each queued text's
// writes may fail.
type leaseScript struct {
	Passes []leaseScriptPass
}

type leaseScriptPass struct {
	ClockStepMS int64
	// NewTexts is how many texts SendText queues just before the pass.
	NewTexts int
	// Faults holds a fault for each text queued so far, oldest first.
	Faults []rowFault
}

func (leaseScript) Generate(r *rand.Rand, _ int) reflect.Value {
	var script leaseScript
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
	return reflect.ValueOf(script)
}

// TestLeaseFailuresNeverHoldOtherSends checks, over generated histories of
// several texts whose writes fail in any pass and in any of the ways a row's
// write can fail:
//
//   - one row's lease failure never blocks other due rows: every due text
//     with no fault is sent in that pass, unless another text's call lost
//     its outcome first and the pass stopped;
//   - a due text whose lease fails is set aside, never called, due again
//     after 5 s, or twice its last such delay up to 5 minutes;
//   - a due text that can be neither leased nor set aside is left exactly as
//     it was, and its pass fails; nothing else fails a pass but a lost
//     outcome;
//   - no fix path resends a called intent: each text reaches the transport
//     at most once, and a text the transport has seen never becomes due
//     again;
//   - a text that is not due changes only when its called lease expires and
//     it becomes uncertain, and updated_at_ms never moves back or below
//     created_at_ms;
//   - once the faults stop and every retry comes due, each text is
//     confirmed, or uncertain when its outcome was lost, after exactly one
//     transport call.
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

	var texts []Submission
	lostOutcome := make(map[string]bool)
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
		before, err := outboxRows(ctx, service, texts)
		if err != nil {
			return fmt.Errorf("pass %d: %w", index, err)
		}
		updatedAt, err := updatedAtByRow(raw)
		if err != nil {
			return fmt.Errorf("pass %d: %w", index, err)
		}
		requestsBefore := requestsByID(sender)
		passErr := service.dispatchPass(ctx)
		after, err := outboxRows(ctx, service, texts)
		if err != nil {
			return fmt.Errorf("pass %d: %w", index, err)
		}
		requestsAfter := requestsByID(sender)
		if err := checkLeasePass(now, pass.Faults, before, after, requestsBefore, requestsAfter, passErr, lostOutcome); err != nil {
			return fmt.Errorf("pass %d at %d: %w", index, now.UnixMilli(), err)
		}
		if err := checkUpdatedAt(raw, updatedAt); err != nil {
			return fmt.Errorf("pass %d: %w", index, err)
		}
	}

	// The faults stop, every lease expires and every retry comes due.
	if err := setRowFaults(raw, texts, nil); err != nil {
		return err
	}
	rows, err := outboxRows(ctx, service, texts)
	if err != nil {
		return err
	}
	clock.set(settleTime(latest.Add(service.leaseTime), rows...).Add(time.Second), 0)
	for index := range 3 {
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
	}
	final, err := outboxRows(ctx, service, texts)
	if err != nil {
		return err
	}
	requests := requestsByID(sender)
	for _, row := range final {
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

// checkLeasePass checks one pass's invariants (see
// TestLeaseFailuresNeverHoldOtherSends) and records the texts whose outcome
// it lost.
func checkLeasePass(
	now time.Time,
	faults []rowFault,
	before, after []sqlite.OutboxItem,
	requestsBefore, requestsAfter map[string]int,
	passErr error,
	lostOutcome map[string]bool,
) error {
	faultOf := func(text int) rowFault {
		if text < len(faults) {
			return faults[text]
		}
		return noRowFault
	}
	stuckDue, outcomeLost := false, false
	for text, row := range before {
		switch faultOf(text) {
		case stuckRowFault, skippedStuckRowFault:
			stuckDue = stuckDue || dueAt(row, now)
		case settleRowFault:
			if requestsAfter[row.TransportRequestID] > requestsBefore[row.TransportRequestID] {
				outcomeLost = true
				lostOutcome[row.OutboxID] = true
			}
		}
	}
	switch {
	case stuckDue && passErr == nil:
		return fmt.Errorf("a due text that could not be set aside did not fail the pass")
	case passErr != nil && !stuckDue && !outcomeLost:
		return fmt.Errorf("pass failed with no stuck text or lost outcome: %w", passErr)
	case passErr != nil && !strings.Contains(passErr.Error(), injectedDispatchFault) &&
		!strings.Contains(passErr.Error(), "the update changed no row"):
		return fmt.Errorf("pass failed for a reason no fault explains: %w", passErr)
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

		if !dueAt(was, now) {
			if expiredCalledLease(was, now) {
				if row.State != sqlite.OutboxUncertain {
					return fmt.Errorf("text %d whose called lease expired = %+v, want uncertain", text, row)
				}
				continue
			}
			if !reflect.DeepEqual(row, was) {
				return fmt.Errorf("text %d was not due and changed from %+v to %+v", text, was, row)
			}
			continue
		}
		switch faultOf(text) {
		case noRowFault:
			if row.State != sqlite.OutboxConfirmed && (!outcomeLost || row.State != sqlite.OutboxQueued) {
				return fmt.Errorf("due text %d with no fault = %+v, want it sent", text, row)
			}
		case abortedLeaseRowFault, skippedLeaseRowFault:
			if !setAsideAfterLeaseFailure(row, now) || called != 0 ||
				row.AttemptCount != was.AttemptCount || row.UpdatedAtMS != max(was.UpdatedAtMS, now.UnixMilli()) {
				return fmt.Errorf("due text %d whose lease failed = %+v, want it set aside from %+v", text, row, was)
			}
			if was.State == sqlite.OutboxQueued || was.State == sqlite.OutboxNotDispatched {
				if delay := *row.NextAttemptAtMS - now.UnixMilli(); delay != leaseRetryDelayMSAfter(was) {
					return fmt.Errorf("due text %d set aside for %d ms, want %d after %+v",
						text, delay, leaseRetryDelayMSAfter(was), was)
				}
			}
		case stuckRowFault, skippedStuckRowFault:
			if !reflect.DeepEqual(row, was) {
				return fmt.Errorf("due text %d that could not be set aside changed from %+v to %+v", text, was, row)
			}
		case settleRowFault:
			if called == 1 && (row.State != sqlite.OutboxDispatching || row.TransportCalledAtMS == nil) {
				return fmt.Errorf("text %d whose outcome was lost = %+v, want its called lease kept", text, row)
			}
			if called == 0 && row.State != sqlite.OutboxQueued {
				return fmt.Errorf("due text %d released untouched = %+v, want queued", text, row)
			}
		}
	}
	return nil
}

// leaseRetryDelayMSAfter is how long LeaseDueBatch sets aside a due row whose
// lease fails, given the row before the failure: 5 s, or twice the row's last
// delay when its last failure was a failed lease too, up to 5 minutes.
func leaseRetryDelayMSAfter(item sqlite.OutboxItem) int64 {
	const first, longest = int64(5_000), int64(300_000)
	if item.ErrorCode != nil && *item.ErrorCode == sqlite.LeaseFailedCode &&
		item.NextAttemptAtMS != nil && *item.NextAttemptAtMS > item.UpdatedAtMS {
		return min(max(2*(*item.NextAttemptAtMS-item.UpdatedAtMS), first), longest)
	}
	return first
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
