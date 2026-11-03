package messaging

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"os"
	"reflect"
	"strconv"
	"strings"
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
	// leaseDispatchFault fails LeaseDue's UPDATE.
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

// TestDispatchPassInvariantsHoldForAnyTimingAndFaults checks, over generated
// submit, lease and transition times and storage faults:
//
//   - every write keeps updated_at_ms >= created_at_ms, and no write moves a
//     row's updated_at_ms backward;
//   - a pass without a fault never fails, and leases and sends every row
//     that is due when it runs;
//   - failed passes never stop dispatch: once the faults stop and leases
//     expire, the text is confirmed, or uncertain when a failure followed
//     its transport call, and it reaches the transport exactly once.
func TestDispatchPassInvariantsHoldForAnyTimingAndFaults(t *testing.T) {
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

func runDispatchScript(t *testing.T, script dispatchScript) error {
	ctx := context.Background()
	clock := &splitReadClock{now: messagingTestTime}
	store, raw := openReconcileTestStore(t, clock.Now())
	sender := &scriptedTextSender{steps: []sendStep{{result: bridge.SendResult{RemoteMessageID: "remote-property"}}}}
	registry := newScriptedRegistry("property", sender)
	registry.setAvailable(true)
	service, err := NewMessageService(store, registry, nil, clock, &sequentialIDs{})
	if err != nil {
		return fmt.Errorf("NewMessageService: %w", err)
	}

	submitAt := messagingTestTime.Add(time.Second)
	clock.set(submitAt, time.Duration(script.SubmitReadGapMS)*time.Millisecond)
	submission, err := service.SendText(ctx, SendTextCommand{
		CommonCommand: testCommonCommand("key-property"),
		Body:          "sent under any timing",
	})
	if err != nil {
		return fmt.Errorf("SendText: %w", err)
	}
	now := submitAt.Add(time.Duration(script.FirstPassMS) * time.Millisecond)
	latest := submitAt.Add(time.Duration(script.SubmitReadGapMS) * time.Millisecond)
	var lastUpdatedMS int64
	postCallFailure := false

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
		requestsBefore := sender.requestCount()
		passErr := service.dispatchPass(ctx)
		after, err := service.outbox.FindByID(ctx, submission.OutboxID)
		if err != nil {
			return fmt.Errorf("pass %d: read row: %w", index, err)
		}
		if passErr != nil && (pass.Fault == noDispatchFault || !strings.Contains(passErr.Error(), injectedDispatchFault)) {
			return fmt.Errorf("pass %d at %d with fault %d failed for another reason: %w", index, now.UnixMilli(), pass.Fault, passErr)
		}
		if pass.Fault == noDispatchFault && dueAt(before, now) && after.State != sqlite.OutboxConfirmed {
			return fmt.Errorf("pass %d at %d left a due row %+v as %+v", index, now.UnixMilli(), before, after)
		}
		if passErr != nil && sender.requestCount() > requestsBefore {
			postCallFailure = true
		}
		if after.UpdatedAtMS < lastUpdatedMS {
			return fmt.Errorf("pass %d moved updated_at_ms back from %d to %d", index, lastUpdatedMS, after.UpdatedAtMS)
		}
		lastUpdatedMS = after.UpdatedAtMS
		if err := rowsUpdatedBeforeCreated(raw); err != nil {
			return fmt.Errorf("pass %d: %w", index, err)
		}
		if requests := sender.requestCount(); requests > 1 {
			return fmt.Errorf("pass %d: the text reached the transport %d times", index, requests)
		}
	}

	// The faults stop and every lease expires.
	if err := setDispatchFault(raw, noDispatchFault); err != nil {
		return err
	}
	clock.set(latest.Add(service.leaseTime+time.Second), 0)
	for index := range 2 {
		if err := service.dispatchPass(ctx); err != nil {
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
	return rowsUpdatedBeforeCreated(raw)
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

func rowsUpdatedBeforeCreated(raw *sql.DB) error {
	for _, table := range []string{"accounts", "conversations", "messages", "outbox"} {
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
