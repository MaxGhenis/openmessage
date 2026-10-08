package bridge

import (
	"bytes"
	"context"
	"errors"
	"math/rand"
	"reflect"
	"sync"
	"testing"
	"testing/quick"
	"time"
)

// Tests for generationSink.AppendHistoryIngress (supervisor.go): the optional
// HistoryIngressSink path a transport uses for catch-up frames it fetched on
// request. It must keep AppendIngress's account/generation identity check and
// commit-time fence (DESIGN.md I5) but enqueue no generation activity (I6).
//
// Observable for "activity enqueued": every activity reaches
// supervisorLoop.handleActivity (supervisor.go:801-822), which advances
// Snapshot().LastEventAt for an event, calls resetFailureHistory
// (supervisor.go:1217-1226), and refreshes Snapshot().LivenessDeadline when
// Online. Supervisor.Sync is a barrier for activity already enqueued: the
// commandSync handler runs drainReadyWork (supervisor.go:1284-1313), which
// handles every buffered activity before Sync returns. So "append, then Sync,
// then compare Snapshot" observes any activity the append enqueued.

// historyFenceSink is a downstream ConnectionSink that also implements
// HistoryIngressSink. Live AppendIngress/EmitEphemeral are recorded by the
// embedded supervisorRecordingSink (supervisor_test.go) exactly as in the
// existing supervisor tests; history appends are recorded apart so a test can
// tell which path a frame took.
type historyFenceSink struct {
	supervisorRecordingSink

	historyMu  sync.Mutex
	history    []RawIngressRecord
	historyErr error
	// entered, when set, receives once per history append before it blocks
	// on release (also when set). Lets a test hold an append in flight.
	entered chan struct{}
	release chan struct{}
}

func (s *historyFenceSink) AppendHistoryIngress(_ context.Context, record RawIngressRecord) error {
	if s.entered != nil {
		s.entered <- struct{}{}
	}
	if s.release != nil {
		<-s.release
	}
	s.historyMu.Lock()
	defer s.historyMu.Unlock()
	record.Payload = bytes.Clone(record.Payload)
	s.history = append(s.history, record)
	return s.historyErr
}

func (s *historyFenceSink) historyRecords() []RawIngressRecord {
	s.historyMu.Lock()
	defer s.historyMu.Unlock()
	return append([]RawIngressRecord(nil), s.history...)
}

var (
	_ ConnectionSink     = (*historyFenceSink)(nil)
	_ HistoryIngressSink = (*historyFenceSink)(nil)
)

// historyFenceOnline starts a supervisor whose single generation becomes
// Online at supervisorTestEpoch and returns the generation sink the lifecycle
// received (the *generationSink a real adapter keeps as its run sink).
func historyFenceOnline(
	t *testing.T,
	options ...SupervisorOption,
) (*Supervisor, *supervisorManualClock, ConnectionSink, Snapshot) {
	t.Helper()
	clock := newSupervisorManualClock(supervisorTestEpoch)
	run := newSupervisorTestRun()
	lifecycle := &supervisorTestLifecycle{scripts: []supervisorStartScript{{run: run}}}
	supervisor := newTestSupervisor(
		t,
		lifecycle,
		supervisorTestPolicy(),
		clock,
		&supervisorScriptedRandom{},
		options...,
	)
	if err := supervisorStart(t, supervisor, StartRequest{}); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	calls := awaitSupervisorStartCount(t, lifecycle, 1)
	run.MarkReady()
	online := awaitSupervisorSnapshot(t, supervisor, "generation one online", func(snapshot Snapshot) bool {
		return snapshot.Generation == 1 && snapshot.State == StateOnline
	})
	return supervisor, clock, calls[0].sink, online
}

func historyFenceRecord(generation Generation, at time.Time) RawIngressRecord {
	return RawIngressRecord{
		AccountID:    "account-1",
		Generation:   generation,
		DedupeKey:    "msg:history-1:0a0b0c0d",
		Codec:        "google.protobuf.history",
		CodecVersion: 1,
		ReceivedAt:   at,
		Payload:      []byte(`{"kind":"message","proto_b64":"AA==","is_old":true}`),
	}
}

// historySnapshotEqual compares every Snapshot field, using time.Equal for
// the timestamps.
func historySnapshotEqual(a, b Snapshot) bool {
	return a.AccountID == b.AccountID && a.Platform == b.Platform && a.State == b.State &&
		a.Generation == b.Generation && a.LastEventAt.Equal(b.LastEventAt) &&
		a.LastProbeOKAt.Equal(b.LastProbeOKAt) && a.LivenessDeadline.Equal(b.LivenessDeadline) &&
		a.RetryAt.Equal(b.RetryAt) && a.ErrorClass == b.ErrorClass &&
		a.ErrorFingerprint == b.ErrorFingerprint
}

func requireHistoryIngressSink(t *testing.T, sink ConnectionSink) HistoryIngressSink {
	t.Helper()
	history, ok := sink.(HistoryIngressSink)
	if !ok {
		t.Fatalf("generation sink %T does not implement HistoryIngressSink", sink)
	}
	return history
}

func appendHistoryForTest(t *testing.T, sink ConnectionSink, record RawIngressRecord) error {
	t.Helper()
	ctx, cancel := supervisorTestContext(t)
	defer cancel()
	return requireHistoryIngressSink(t, sink).AppendHistoryIngress(ctx, record)
}

func appendLiveForTest(t *testing.T, sink ConnectionSink, record RawIngressRecord) error {
	t.Helper()
	ctx, cancel := supervisorTestContext(t)
	defer cancel()
	return sink.AppendIngress(ctx, record)
}

// The generation sink forwards a current-generation history frame to the
// downstream history path byte-for-byte, and never through AppendIngress.
func TestHistoryIngressForwardsExactRecordToHistorySink(t *testing.T) {
	downstream := &historyFenceSink{}
	_, _, sink, _ := historyFenceOnline(t, WithConnectionSink(downstream))

	record := historyFenceRecord(1, supervisorTestEpoch.Add(-time.Hour))
	if err := appendHistoryForTest(t, sink, record); err != nil {
		t.Fatalf("AppendHistoryIngress() error = %v", err)
	}

	got := downstream.historyRecords()
	if len(got) != 1 {
		t.Fatalf("downstream history appends = %d, want 1", len(got))
	}
	if !reflect.DeepEqual(got[0], record) {
		t.Fatalf("forwarded history record:\n got  %+v\n want %+v", got[0], record)
	}
	if ingress, ephemeral := downstream.Counts(); ingress != 0 || ephemeral != 0 {
		t.Fatalf("history frame reached the live path: AppendIngress=%d EmitEphemeral=%d", ingress, ephemeral)
	}
}

// The identity check matches AppendIngress: a record that names another
// account or another generation number is stale even while the fence is open.
func TestHistoryIngressRejectsForeignAccountOrGeneration(t *testing.T) {
	downstream := &historyFenceSink{}
	_, _, sink, _ := historyFenceOnline(t, WithConnectionSink(downstream))

	at := supervisorTestEpoch
	cases := map[string]RawIngressRecord{
		"other account":     func() RawIngressRecord { r := historyFenceRecord(1, at); r.AccountID = "account-2"; return r }(),
		"empty account":     func() RawIngressRecord { r := historyFenceRecord(1, at); r.AccountID = ""; return r }(),
		"older generation":  historyFenceRecord(0, at),
		"newer generation":  historyFenceRecord(2, at),
		"far generation":    historyFenceRecord(1<<40, at),
		"padded account id": func() RawIngressRecord { r := historyFenceRecord(1, at); r.AccountID = " account-1"; return r }(),
	}
	for name, record := range cases {
		t.Run(name, func(t *testing.T) {
			err := appendHistoryForTest(t, sink, record)
			if !errors.Is(err, ErrStaleGeneration) {
				t.Fatalf("AppendHistoryIngress(%+v) error = %v, want ErrStaleGeneration", record, err)
			}
		})
	}
	if got := downstream.historyRecords(); len(got) != 0 {
		t.Fatalf("downstream received %d mismatched history records: %+v", len(got), got)
	}
	if ingress, _ := downstream.Counts(); ingress != 0 {
		t.Fatalf("mismatched history records reached AppendIngress %d times", ingress)
	}
}

// A retired generation's sink is fenced for history exactly as for live
// ingress, even when the record names that generation correctly (setup copied
// from TestSupervisorFencesCallbacksFromOlderGenerations). The successor
// generation's sink keeps working.
func TestHistoryIngressFencedAfterGenerationRetired(t *testing.T) {
	clock := newSupervisorManualClock(supervisorTestEpoch)
	runOne := newSupervisorTestRun()
	runTwo := newSupervisorTestRun()
	lifecycle := &supervisorTestLifecycle{scripts: []supervisorStartScript{
		{run: runOne},
		{run: runTwo},
	}}
	random := &supervisorScriptedRandom{values: []int64{int64(time.Second)}}
	downstream := &historyFenceSink{}
	policy := supervisorTestPolicy()
	supervisor := newTestSupervisor(t, lifecycle, policy, clock, random, WithConnectionSink(downstream))

	if err := supervisorStart(t, supervisor, StartRequest{DeviceID: "device-1"}); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	awaitSupervisorStartCount(t, lifecycle, 1)

	// Generation one never becomes ready and is retired at its hard deadline.
	clock.Advance(policy.ConnectTimeout)
	backoff := awaitSupervisorSnapshot(t, supervisor, "generation one backoff", func(snapshot Snapshot) bool {
		return snapshot.Generation == 1 && snapshot.State == StateBackoff
	})
	clock.Advance(backoff.RetryAt.Sub(clock.Now()))
	calls := awaitSupervisorStartCount(t, lifecycle, 2)
	runTwo.MarkReady()
	awaitSupervisorSnapshot(t, supervisor, "generation two online", func(snapshot Snapshot) bool {
		return snapshot.Generation == 2 && snapshot.State == StateOnline
	})

	oldSink, newSink := calls[0].sink, calls[1].sink
	if err := appendHistoryForTest(t, oldSink, historyFenceRecord(1, clock.Now())); !errors.Is(err, ErrStaleGeneration) {
		t.Fatalf("retired generation AppendHistoryIngress() error = %v, want ErrStaleGeneration", err)
	}
	// Naming the current generation through the old sink is an identity
	// mismatch, not a way around the fence.
	if err := appendHistoryForTest(t, oldSink, historyFenceRecord(2, clock.Now())); !errors.Is(err, ErrStaleGeneration) {
		t.Fatalf("retired sink with current generation number error = %v, want ErrStaleGeneration", err)
	}
	if got := downstream.historyRecords(); len(got) != 0 {
		t.Fatalf("retired generation committed %d history records", len(got))
	}

	current := historyFenceRecord(2, clock.Now())
	if err := appendHistoryForTest(t, newSink, current); err != nil {
		t.Fatalf("current generation AppendHistoryIngress() error = %v", err)
	}
	got := downstream.historyRecords()
	if len(got) != 1 || !reflect.DeepEqual(got[0], current) {
		t.Fatalf("downstream history records = %+v, want exactly the generation-two record", got)
	}
}

// Supervisor.Stop deactivates the fence (shutdown -> retireCurrent ->
// clearGenerationContext); a history append afterwards never commits.
func TestHistoryIngressFencedAfterSupervisorStop(t *testing.T) {
	downstream := &historyFenceSink{}
	supervisor, clock, sink, _ := historyFenceOnline(t, WithConnectionSink(downstream))

	ctx, cancel := supervisorTestContext(t)
	err := supervisor.Stop(ctx)
	cancel()
	if err != nil {
		t.Fatalf("Supervisor.Stop() error = %v", err)
	}
	if err := appendHistoryForTest(t, sink, historyFenceRecord(1, clock.Now())); !errors.Is(err, ErrStaleGeneration) {
		t.Fatalf("AppendHistoryIngress() after Stop error = %v, want ErrStaleGeneration", err)
	}
	if got := downstream.historyRecords(); len(got) != 0 {
		t.Fatalf("history committed after Stop: %+v", got)
	}
}

// The fence is atomic at commit time for history too: an append already
// inside the downstream commit holds the fence's read lock, so retirement
// (which takes the write lock in generationFence.deactivate) waits for it, and
// nothing commits after retirement returns.
func TestHistoryIngressFenceJoinsInFlightAppend(t *testing.T) {
	downstream := &historyFenceSink{
		entered: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
	supervisor, clock, sink, _ := historyFenceOnline(t, WithConnectionSink(downstream))

	inFlight := historyFenceRecord(1, clock.Now())
	appendResult := make(chan error, 1)
	go func() {
		ctx, cancel := supervisorTestContext(t)
		defer cancel()
		appendResult <- sink.(HistoryIngressSink).AppendHistoryIngress(ctx, inFlight)
	}()
	receiveSupervisorValue(t, downstream.entered, "history append to enter the downstream commit")

	stopResult := make(chan error, 1)
	go func() {
		ctx, cancel := supervisorTestContext(t)
		defer cancel()
		stopResult <- supervisor.Stop(ctx)
	}()
	// shutdown publishes Stopping before it retires the generation.
	awaitSupervisorSnapshot(t, supervisor, "shutdown begins", func(snapshot Snapshot) bool {
		return snapshot.State == StateStopping
	})
	select {
	case err := <-stopResult:
		t.Fatalf("Supervisor.Stop() returned (%v) while a history commit held the fence", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(downstream.release)
	if err := receiveSupervisorValue(t, appendResult, "in-flight history append"); err != nil {
		t.Fatalf("in-flight AppendHistoryIngress() error = %v, want nil (admitted before retirement)", err)
	}
	if err := receiveSupervisorValue(t, stopResult, "Supervisor.Stop"); err != nil {
		t.Fatalf("Supervisor.Stop() error = %v", err)
	}

	if err := appendHistoryForTest(t, sink, historyFenceRecord(1, clock.Now())); !errors.Is(err, ErrStaleGeneration) {
		t.Fatalf("AppendHistoryIngress() after retirement error = %v, want ErrStaleGeneration", err)
	}
	got := downstream.historyRecords()
	if len(got) != 1 || !reflect.DeepEqual(got[0], inFlight) {
		t.Fatalf("committed history = %+v, want only the in-flight record", got)
	}
}

// Legacy-only mode (no downstream sink): a current-generation history frame is
// accepted and dropped, like live ingress. Contrast: the live frame in the same
// mode still enqueues activity (generationSink.forward enqueues whether or not
// a downstream sink exists); the history frame does not.
func TestHistoryIngressWithoutDownstreamSinkIsAcceptedAndDropped(t *testing.T) {
	supervisor, clock, sink, online := historyFenceOnline(t)

	clock.Advance(time.Second)
	if err := appendHistoryForTest(t, sink, historyFenceRecord(1, clock.Now())); err != nil {
		t.Fatalf("AppendHistoryIngress() without downstream error = %v, want nil", err)
	}
	if err := appendHistoryForTest(t, sink, historyFenceRecord(2, clock.Now())); !errors.Is(err, ErrStaleGeneration) {
		t.Fatalf("identity check skipped without downstream: error = %v, want ErrStaleGeneration", err)
	}
	supervisorSync(t, supervisor)
	if got := supervisor.Snapshot(); !historySnapshotEqual(got, online) {
		t.Fatalf("history append without downstream changed the snapshot:\n got  %+v\n want %+v", got, online)
	}

	if err := appendLiveForTest(t, sink, historyFenceRecord(1, clock.Now())); err != nil {
		t.Fatalf("contrast AppendIngress() error = %v", err)
	}
	supervisorSync(t, supervisor)
	if got := supervisor.Snapshot().LastEventAt; !got.Equal(clock.Now()) {
		t.Fatalf("contrast: live LastEventAt = %s, want %s (live ingress records activity)", got, clock.Now())
	}
}

// A downstream that cannot take history fails closed with
// ErrHistoryIngressMissing; it never falls back to the live AppendIngress
// (which would record the frame as live delivery).
func TestHistoryIngressDownstreamWithoutHistoryPathReportsMissing(t *testing.T) {
	downstream := &supervisorRecordingSink{} // AppendIngress only
	supervisor, clock, sink, online := historyFenceOnline(t, WithConnectionSink(downstream))

	err := appendHistoryForTest(t, sink, historyFenceRecord(1, clock.Now()))
	if !errors.Is(err, ErrHistoryIngressMissing) {
		t.Fatalf("AppendHistoryIngress() error = %v, want ErrHistoryIngressMissing", err)
	}
	if errors.Is(err, ErrStaleGeneration) {
		t.Fatalf("missing history path reported as a stale generation: %v", err)
	}
	if ingress, ephemeral := downstream.Counts(); ingress != 0 || ephemeral != 0 {
		t.Fatalf("history frame fell back to the live path: AppendIngress=%d EmitEphemeral=%d", ingress, ephemeral)
	}
	supervisorSync(t, supervisor)
	if got := supervisor.Snapshot(); !historySnapshotEqual(got, online) {
		t.Fatalf("rejected history append changed the snapshot:\n got  %+v\n want %+v", got, online)
	}
}

// A downstream commit error is returned unchanged (not reclassified as stale).
func TestHistoryIngressReturnsDownstreamError(t *testing.T) {
	commitErr := errors.New("inbox unavailable")
	downstream := &historyFenceSink{historyErr: commitErr}
	_, clock, sink, _ := historyFenceOnline(t, WithConnectionSink(downstream))

	err := appendHistoryForTest(t, sink, historyFenceRecord(1, clock.Now()))
	if !errors.Is(err, commitErr) {
		t.Fatalf("AppendHistoryIngress() error = %v, want downstream %v", err, commitErr)
	}
	if errors.Is(err, ErrStaleGeneration) || errors.Is(err, ErrHistoryIngressMissing) {
		t.Fatalf("downstream error was reclassified: %v", err)
	}
}

// I6: a successful history append enqueues no generation activity. While
// Online, it leaves LastEventAt, LastProbeOKAt and LivenessDeadline exactly as
// they were. Contrast: a live AppendIngress at the same instant on the same
// sink advances LastEventAt and pushes the liveness deadline.
func TestHistoryIngressRecordsNoGenerationActivity(t *testing.T) {
	downstream := &historyFenceSink{}
	supervisor, clock, sink, online := historyFenceOnline(t, WithConnectionSink(downstream))
	policy := supervisorTestPolicy()
	if !online.LastEventAt.IsZero() ||
		!online.LivenessDeadline.Equal(supervisorTestEpoch.Add(policy.LivenessTimeout)) {
		t.Fatalf("unexpected Online baseline: %+v", online)
	}

	// Stay below ProbeEvery so no liveness check runs in between.
	clock.Advance(time.Second)
	at := clock.Now()
	for i := 0; i < 3; i++ {
		if err := appendHistoryForTest(t, sink, historyFenceRecord(1, at)); err != nil {
			t.Fatalf("AppendHistoryIngress() #%d error = %v", i+1, err)
		}
	}
	supervisorSync(t, supervisor)
	if got := supervisor.Snapshot(); !historySnapshotEqual(got, online) {
		t.Fatalf("history appends changed generation liveness:\n got  %+v\n want %+v", got, online)
	}
	if got := len(downstream.historyRecords()); got != 3 {
		t.Fatalf("downstream history appends = %d, want 3 (the appends did commit)", got)
	}

	live := historyFenceRecord(1, at)
	live.Codec = "google.protobuf"
	live.DedupeKey = "msg:live-1:01020304"
	if err := appendLiveForTest(t, sink, live); err != nil {
		t.Fatalf("contrast AppendIngress() error = %v", err)
	}
	supervisorSync(t, supervisor)
	got := supervisor.Snapshot()
	if !got.LastEventAt.Equal(at) {
		t.Fatalf("contrast: live LastEventAt = %s, want %s", got.LastEventAt, at)
	}
	if want := at.Add(policy.LivenessTimeout); !got.LivenessDeadline.Equal(want) {
		t.Fatalf("contrast: live LivenessDeadline = %s, want %s", got.LivenessDeadline, want)
	}
	if ingress, _ := downstream.Counts(); ingress != 1 {
		t.Fatalf("live AppendIngress calls = %d, want 1", ingress)
	}
}

// I6 (failure history): handleActivity calls resetFailureHistory, which zeroes
// the transient attempt counter that sizes the next backoff cap. Going Online
// does not reset it (handleReady), so after one failed start the next failure
// draws jitter under the attempt-2 cap unless some activity reset the history.
// A history append must leave the cap at attempt 2; a live append (contrast)
// resets it to attempt 1. The cap is observed through the scripted random
// source's bounds, as in TestSupervisorTransientBackoffUsesFullJitterExponentialCaps.
func TestHistoryIngressDoesNotResetFailureHistory(t *testing.T) {
	for _, tc := range []struct {
		name    string
		append  func(*testing.T, ConnectionSink, RawIngressRecord) error
		wantCap time.Duration
	}{
		{name: "history keeps transient attempt", append: appendHistoryForTest, wantCap: 8 * time.Second},
		{name: "live resets transient attempt (contrast)", append: appendLiveForTest, wantCap: 4 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := newSupervisorManualClock(supervisorTestEpoch)
			policy := supervisorTestPolicy()
			policy.MinBackoff = 4 * time.Second
			policy.MaxBackoff = 16 * time.Second
			runTwo := newSupervisorTestRun()
			lifecycle := &supervisorTestLifecycle{scripts: []supervisorStartScript{
				{err: transientSupervisorError("start-1")},
				{run: runTwo},
			}}
			random := &supervisorScriptedRandom{values: []int64{int64(time.Second), int64(time.Second)}}
			downstream := &historyFenceSink{}
			supervisor := newTestSupervisor(t, lifecycle, policy, clock, random, WithConnectionSink(downstream))

			if err := supervisorStart(t, supervisor, StartRequest{}); err == nil {
				t.Fatal("Start() error = nil, want first scripted transient failure")
			}
			backoff := awaitSupervisorSnapshot(t, supervisor, "generation one backoff", func(snapshot Snapshot) bool {
				return snapshot.Generation == 1 && snapshot.State == StateBackoff
			})
			if bounds := random.Bounds(); len(bounds) != 1 || time.Duration(bounds[0]) != 4*time.Second {
				t.Fatalf("first backoff bounds = %v, want [4s]", bounds)
			}
			clock.Advance(backoff.RetryAt.Sub(clock.Now()))
			calls := awaitSupervisorStartCount(t, lifecycle, 2)
			runTwo.MarkReady()
			awaitSupervisorSnapshot(t, supervisor, "generation two online", func(snapshot Snapshot) bool {
				return snapshot.Generation == 2 && snapshot.State == StateOnline
			})

			if err := tc.append(t, calls[1].sink, historyFenceRecord(2, clock.Now())); err != nil {
				t.Fatalf("append error = %v", err)
			}
			supervisorSync(t, supervisor)

			runTwo.Complete(errors.New("connection dropped"))
			awaitSupervisorSnapshot(t, supervisor, "generation two backoff", func(snapshot Snapshot) bool {
				return snapshot.Generation == 2 && snapshot.State == StateBackoff
			})
			bounds := random.Bounds()
			if len(bounds) != 2 || time.Duration(bounds[1]) != tc.wantCap {
				t.Fatalf("second backoff bounds = %v, want cap %s", bounds, tc.wantCap)
			}
		})
	}
}

// historyIngressCase is a random history record offered to generation one's
// sink: the account and generation are drawn mostly from near-misses of the
// sink's own identity so both outcomes are well covered.
type historyIngressCase struct {
	Record RawIngressRecord
}

func (historyIngressCase) Generate(r *rand.Rand, _ int) reflect.Value {
	accounts := []string{"account-1", "account-1", "account-1", "account-2", "", "Account-1", "account-1\x00"}
	generations := []Generation{1, 1, 1, 0, 2, Generation(r.Uint64())}
	payload := make([]byte, r.Intn(64))
	_, _ = r.Read(payload)
	key := make([]byte, 1+r.Intn(24))
	for i := range key {
		key[i] = byte('a' + r.Intn(26))
	}
	return reflect.ValueOf(historyIngressCase{Record: RawIngressRecord{
		AccountID:    accounts[r.Intn(len(accounts))],
		Generation:   generations[r.Intn(len(generations))],
		DedupeKey:    "msg:" + string(key),
		Codec:        []string{"google.protobuf.history", "google.protobuf", "x"}[r.Intn(3)],
		CodecVersion: uint32(r.Intn(3)),
		ReceivedAt:   time.Unix(r.Int63n(4_000_000_000), int64(r.Intn(1_000_000_000))).UTC(),
		Payload:      payload,
	}})
}

// Property: for every record, AppendHistoryIngress on generation one's sink
// succeeds iff the record names (account-1, generation 1); success forwards
// exactly that record once to the history path; failure is ErrStaleGeneration
// and forwards nothing. Over the whole run nothing reaches AppendIngress and no
// generation activity is recorded (I6).
func TestHistoryIngressPropertyIdentityGateAndExactForwarding(t *testing.T) {
	downstream := &historyFenceSink{}
	supervisor, _, sink, online := historyFenceOnline(t, WithConnectionSink(downstream))
	history := requireHistoryIngressSink(t, sink)

	property := func(c historyIngressCase) bool {
		before := len(downstream.historyRecords())
		ctx, cancel := supervisorTestContext(t)
		err := history.AppendHistoryIngress(ctx, c.Record)
		cancel()
		after := downstream.historyRecords()
		current := c.Record.AccountID == "account-1" && c.Record.Generation == 1
		if !current {
			if !errors.Is(err, ErrStaleGeneration) {
				t.Logf("record %+v: error = %v, want ErrStaleGeneration", c.Record, err)
				return false
			}
			return len(after) == before
		}
		if err != nil {
			t.Logf("record %+v: error = %v, want nil", c.Record, err)
			return false
		}
		if len(after) != before+1 {
			return false
		}
		got := after[len(after)-1]
		want := c.Record
		if len(want.Payload) == 0 {
			// bytes.Clone(empty) may be nil or empty; compare content only.
			got.Payload, want.Payload = nil, nil
		}
		return reflect.DeepEqual(got, want)
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 300, Rand: rand.New(rand.NewSource(20261008))}); err != nil {
		t.Fatalf("identity gate property: %v", err)
	}
	if len(downstream.historyRecords()) == 0 {
		t.Fatal("property run never exercised the accepting branch")
	}
	if ingress, ephemeral := downstream.Counts(); ingress != 0 || ephemeral != 0 {
		t.Fatalf("history property reached the live path: AppendIngress=%d EmitEphemeral=%d", ingress, ephemeral)
	}
	supervisorSync(t, supervisor)
	if got := supervisor.Snapshot(); !historySnapshotEqual(got, online) {
		t.Fatalf("history appends recorded generation activity:\n got  %+v\n want %+v", got, online)
	}
}
