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

// Tests for generationSink.AppendReplayedIngress (supervisor.go): the optional
// ReplayIngressSink path a transport uses for a frame its live channel
// delivered under an earlier generation, which it held until a later
// generation could commit it. It keeps AppendIngress's account/generation
// identity check and commit-time fence, forwards to the downstream sink's
// live AppendIngress, and enqueues no generation activity: the frame is not
// evidence that the current connection works, and activity would reset the
// failure history that sizes backoff. Activity is observed as in
// history_ingress_test.go: append, Supervisor.Sync, compare Snapshot.

// replayFenceSink is a downstream sink whose AppendIngress can fail.
type replayFenceSink struct {
	supervisorRecordingSink
	errMu     sync.Mutex
	appendErr error
}

func (s *replayFenceSink) AppendIngress(ctx context.Context, record RawIngressRecord) error {
	record.Payload = bytes.Clone(record.Payload)
	_ = s.supervisorRecordingSink.AppendIngress(ctx, record)
	s.errMu.Lock()
	defer s.errMu.Unlock()
	return s.appendErr
}

func (s *replayFenceSink) records() []RawIngressRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]RawIngressRecord(nil), s.ingress...)
}

func replayFenceRecord(generation Generation, at time.Time) RawIngressRecord {
	return RawIngressRecord{
		AccountID:    "account-1",
		Generation:   generation,
		DedupeKey:    "hist:0a0b0c0d",
		Codec:        "whatsapp.event",
		CodecVersion: 1,
		ReceivedAt:   at,
		Payload:      []byte(`{"kind":"history_sync","proto_b64":"AA=="}`),
	}
}

func requireReplayIngressSink(t *testing.T, sink ConnectionSink) ReplayIngressSink {
	t.Helper()
	replay, ok := sink.(ReplayIngressSink)
	if !ok {
		t.Fatalf("generation sink %T does not implement ReplayIngressSink", sink)
	}
	return replay
}

func appendReplayedForTest(t *testing.T, sink ConnectionSink, record RawIngressRecord) error {
	t.Helper()
	ctx, cancel := supervisorTestContext(t)
	defer cancel()
	return requireReplayIngressSink(t, sink).AppendReplayedIngress(ctx, record)
}

// A current-generation replayed frame reaches the downstream live
// AppendIngress byte for byte, keeping the receive time it was captured with.
func TestReplayedIngressForwardsExactRecordToLiveSink(t *testing.T) {
	downstream := &replayFenceSink{}
	_, _, sink, _ := historyFenceOnline(t, WithConnectionSink(downstream))

	record := replayFenceRecord(1, supervisorTestEpoch.Add(-time.Hour))
	if err := appendReplayedForTest(t, sink, record); err != nil {
		t.Fatalf("AppendReplayedIngress() error = %v", err)
	}
	got := downstream.records()
	if len(got) != 1 || !reflect.DeepEqual(got[0], record) {
		t.Fatalf("forwarded records = %+v, want exactly %+v", got, record)
	}
}

func TestReplayedIngressRejectsForeignAccountOrGeneration(t *testing.T) {
	downstream := &replayFenceSink{}
	_, _, sink, _ := historyFenceOnline(t, WithConnectionSink(downstream))

	at := supervisorTestEpoch
	cases := map[string]RawIngressRecord{
		"other account":    func() RawIngressRecord { r := replayFenceRecord(1, at); r.AccountID = "account-2"; return r }(),
		"empty account":    func() RawIngressRecord { r := replayFenceRecord(1, at); r.AccountID = ""; return r }(),
		"older generation": replayFenceRecord(0, at),
		"newer generation": replayFenceRecord(2, at),
	}
	for name, record := range cases {
		t.Run(name, func(t *testing.T) {
			if err := appendReplayedForTest(t, sink, record); !errors.Is(err, ErrStaleGeneration) {
				t.Fatalf("AppendReplayedIngress(%+v) error = %v, want ErrStaleGeneration", record, err)
			}
		})
	}
	if got := downstream.records(); len(got) != 0 {
		t.Fatalf("downstream received %d mismatched records", len(got))
	}
}

// A retired generation's sink refuses replayed frames even when the record
// names that generation; the successor's sink takes them.
func TestReplayedIngressFencedAfterGenerationRetired(t *testing.T) {
	clock := newSupervisorManualClock(supervisorTestEpoch)
	runOne := newSupervisorTestRun()
	runTwo := newSupervisorTestRun()
	lifecycle := &supervisorTestLifecycle{scripts: []supervisorStartScript{{run: runOne}, {run: runTwo}}}
	random := &supervisorScriptedRandom{values: []int64{int64(time.Second)}}
	downstream := &replayFenceSink{}
	policy := supervisorTestPolicy()
	supervisor := newTestSupervisor(t, lifecycle, policy, clock, random, WithConnectionSink(downstream))

	if err := supervisorStart(t, supervisor, StartRequest{DeviceID: "device-1"}); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	awaitSupervisorStartCount(t, lifecycle, 1)
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
	if err := appendReplayedForTest(t, oldSink, replayFenceRecord(1, clock.Now())); !errors.Is(err, ErrStaleGeneration) {
		t.Fatalf("retired generation AppendReplayedIngress() error = %v, want ErrStaleGeneration", err)
	}
	if err := appendReplayedForTest(t, oldSink, replayFenceRecord(2, clock.Now())); !errors.Is(err, ErrStaleGeneration) {
		t.Fatalf("retired sink with the current generation number error = %v, want ErrStaleGeneration", err)
	}
	current := replayFenceRecord(2, clock.Now())
	if err := appendReplayedForTest(t, newSink, current); err != nil {
		t.Fatalf("current generation AppendReplayedIngress() error = %v", err)
	}
	if got := downstream.records(); len(got) != 1 || !reflect.DeepEqual(got[0], current) {
		t.Fatalf("downstream records = %+v, want exactly the generation-two record", got)
	}
}

// Legacy-only mode (no downstream sink) discards a replayed frame as it
// discards a live one, so a transport releases what it held; once the
// generation is retired the frame is stale instead.
func TestReplayedIngressWithoutDownstreamSink(t *testing.T) {
	supervisor, clock, sink, online := historyFenceOnline(t)
	if err := appendReplayedForTest(t, sink, replayFenceRecord(1, clock.Now())); err != nil {
		t.Fatalf("AppendReplayedIngress() without downstream error = %v, want nil", err)
	}
	supervisorSync(t, supervisor)
	if got := supervisor.Snapshot(); !historySnapshotEqual(got, online) {
		t.Fatalf("replayed append without downstream changed the snapshot:\n got  %+v\n want %+v", got, online)
	}

	ctx, cancel := supervisorTestContext(t)
	err := supervisor.Stop(ctx)
	cancel()
	if err != nil {
		t.Fatalf("Supervisor.Stop() error = %v", err)
	}
	if err := appendReplayedForTest(t, sink, replayFenceRecord(1, clock.Now())); !errors.Is(err, ErrStaleGeneration) {
		t.Fatalf("AppendReplayedIngress() after Stop error = %v, want ErrStaleGeneration", err)
	}
}

func TestReplayedIngressReturnsDownstreamError(t *testing.T) {
	commitErr := errors.New("inbox unavailable")
	downstream := &replayFenceSink{appendErr: commitErr}
	_, clock, sink, _ := historyFenceOnline(t, WithConnectionSink(downstream))
	err := appendReplayedForTest(t, sink, replayFenceRecord(1, clock.Now()))
	if !errors.Is(err, commitErr) || errors.Is(err, ErrStaleGeneration) {
		t.Fatalf("AppendReplayedIngress() error = %v, want downstream %v", err, commitErr)
	}
}

// A replayed append enqueues no generation activity: while Online it leaves
// LastEventAt, LastProbeOKAt and LivenessDeadline exactly as they were.
// Contrast: a live AppendIngress of the same record advances them.
func TestReplayedIngressRecordsNoGenerationActivity(t *testing.T) {
	downstream := &replayFenceSink{}
	supervisor, clock, sink, online := historyFenceOnline(t, WithConnectionSink(downstream))
	policy := supervisorTestPolicy()

	clock.Advance(time.Second)
	at := clock.Now()
	for i := 0; i < 3; i++ {
		if err := appendReplayedForTest(t, sink, replayFenceRecord(1, at)); err != nil {
			t.Fatalf("AppendReplayedIngress() #%d error = %v", i+1, err)
		}
	}
	supervisorSync(t, supervisor)
	if got := supervisor.Snapshot(); !historySnapshotEqual(got, online) {
		t.Fatalf("replayed appends changed generation liveness:\n got  %+v\n want %+v", got, online)
	}
	if got := len(downstream.records()); got != 3 {
		t.Fatalf("downstream appends = %d, want 3 (the appends did commit)", got)
	}

	if err := appendLiveForTest(t, sink, replayFenceRecord(1, at)); err != nil {
		t.Fatalf("contrast AppendIngress() error = %v", err)
	}
	supervisorSync(t, supervisor)
	got := supervisor.Snapshot()
	if !got.LastEventAt.Equal(at) || !got.LivenessDeadline.Equal(at.Add(policy.LivenessTimeout)) {
		t.Fatalf("contrast: live append left LastEventAt %s, LivenessDeadline %s", got.LastEventAt, got.LivenessDeadline)
	}
}

// A replayed append leaves the failure history alone, so the next failure's
// backoff cap still grows: after one failed start, the cap stays at attempt 2
// (8s). Contrast: a live append resets it to attempt 1 (4s). Same setup as
// TestHistoryIngressDoesNotResetFailureHistory.
func TestReplayedIngressDoesNotResetFailureHistory(t *testing.T) {
	for _, tc := range []struct {
		name    string
		append  func(*testing.T, ConnectionSink, RawIngressRecord) error
		wantCap time.Duration
	}{
		{name: "replayed keeps transient attempt", append: appendReplayedForTest, wantCap: 8 * time.Second},
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
			supervisor := newTestSupervisor(t, lifecycle, policy, clock, random, WithConnectionSink(&replayFenceSink{}))

			if err := supervisorStart(t, supervisor, StartRequest{}); err == nil {
				t.Fatal("Start() error = nil, want the scripted transient failure")
			}
			backoff := awaitSupervisorSnapshot(t, supervisor, "generation one backoff", func(snapshot Snapshot) bool {
				return snapshot.Generation == 1 && snapshot.State == StateBackoff
			})
			clock.Advance(backoff.RetryAt.Sub(clock.Now()))
			calls := awaitSupervisorStartCount(t, lifecycle, 2)
			runTwo.MarkReady()
			awaitSupervisorSnapshot(t, supervisor, "generation two online", func(snapshot Snapshot) bool {
				return snapshot.Generation == 2 && snapshot.State == StateOnline
			})

			if err := tc.append(t, calls[1].sink, replayFenceRecord(2, clock.Now())); err != nil {
				t.Fatalf("append error = %v", err)
			}
			supervisorSync(t, supervisor)
			runTwo.Complete(errors.New("connection dropped"))
			awaitSupervisorSnapshot(t, supervisor, "generation two backoff", func(snapshot Snapshot) bool {
				return snapshot.Generation == 2 && snapshot.State == StateBackoff
			})
			if bounds := random.Bounds(); len(bounds) != 2 || time.Duration(bounds[1]) != tc.wantCap {
				t.Fatalf("second backoff bounds = %v, want cap %s", bounds, tc.wantCap)
			}
		})
	}
}

// Property: for every record, AppendReplayedIngress on generation one's sink
// succeeds iff the record names (account-1, generation 1), and success
// forwards exactly that record once to the live path; failure is
// ErrStaleGeneration and forwards nothing. No generation activity is recorded
// over the whole run.
func TestReplayedIngressPropertyIdentityGateAndExactForwarding(t *testing.T) {
	downstream := &replayFenceSink{}
	supervisor, _, sink, online := historyFenceOnline(t, WithConnectionSink(downstream))
	replay := requireReplayIngressSink(t, sink)

	property := func(c historyIngressCase) bool {
		before := len(downstream.records())
		ctx, cancel := supervisorTestContext(t)
		err := replay.AppendReplayedIngress(ctx, c.Record)
		cancel()
		after := downstream.records()
		if c.Record.AccountID != "account-1" || c.Record.Generation != 1 {
			return errors.Is(err, ErrStaleGeneration) && len(after) == before
		}
		if err != nil || len(after) != before+1 {
			t.Logf("record %+v: error = %v, appends %d -> %d", c.Record, err, before, len(after))
			return false
		}
		got, want := after[len(after)-1], c.Record
		if len(want.Payload) == 0 {
			got.Payload, want.Payload = nil, nil
		}
		return reflect.DeepEqual(got, want)
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 300, Rand: rand.New(rand.NewSource(20261011))}); err != nil {
		t.Fatalf("identity gate property: %v", err)
	}
	if len(downstream.records()) == 0 {
		t.Fatal("property run never exercised the accepting branch")
	}
	supervisorSync(t, supervisor)
	if got := supervisor.Snapshot(); !historySnapshotEqual(got, online) {
		t.Fatalf("replayed appends recorded generation activity:\n got  %+v\n want %+v", got, online)
	}
}
