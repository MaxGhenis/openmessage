package bridge

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const supervisorTestTimeout = 2 * time.Second

var supervisorTestEpoch = time.Date(2026, time.July, 12, 12, 0, 0, 0, time.UTC)

func TestSupervisorFencesCallbacksFromOlderGenerations(t *testing.T) {
	clock := newSupervisorManualClock(supervisorTestEpoch)
	runOne := newSupervisorTestRun()
	runTwo := newSupervisorTestRun()
	lifecycle := &supervisorTestLifecycle{scripts: []supervisorStartScript{
		{run: runOne},
		{run: runTwo},
	}}
	random := &supervisorScriptedRandom{values: []int64{int64(time.Second)}}
	downstream := &supervisorRecordingSink{}
	policy := supervisorTestPolicy()
	supervisor := newTestSupervisor(
		t,
		lifecycle,
		policy,
		clock,
		random,
		WithConnectionSink(downstream),
	)

	if err := supervisorStart(t, supervisor, StartRequest{DeviceID: "device-1"}); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	firstCalls := awaitSupervisorStartCount(t, lifecycle, 1)
	if got := firstCalls[0].request.Generation; got != 1 {
		t.Fatalf("first Start generation = %d, want 1", got)
	}

	// Generation one never becomes ready and is retired at its hard deadline.
	clock.Advance(policy.ConnectTimeout)
	backoff := awaitSupervisorSnapshot(t, supervisor, "generation one backoff", func(snapshot Snapshot) bool {
		return snapshot.Generation == 1 && snapshot.State == StateBackoff
	})
	if got := runOne.StopCount(); got != 1 {
		t.Fatalf("generation one Stop calls = %d, want 1", got)
	}
	clock.Advance(backoff.RetryAt.Sub(clock.Now()))
	secondCalls := awaitSupervisorStartCount(t, lifecycle, 2)
	if got := secondCalls[1].request.Generation; got != 2 {
		t.Fatalf("second Start generation = %d, want 2", got)
	}
	runTwo.MarkReady()
	want := awaitSupervisorSnapshot(t, supervisor, "generation two online", func(snapshot Snapshot) bool {
		return snapshot.Generation == 2 && snapshot.State == StateOnline
	})

	// Every callback surface retained by generation one is now stale: durable
	// and ephemeral events are rejected, beats are discarded, and late Ready
	// and Done signals cannot change generation two.
	oldSink := secondCalls[0].sink
	ctx, cancel := supervisorTestContext(t)
	err := oldSink.AppendIngress(ctx, RawIngressRecord{
		AccountID:  "account-1",
		Generation: 1,
		DedupeKey:  "late-event",
	})
	cancel()
	if !errors.Is(err, ErrStaleGeneration) {
		t.Fatalf("stale AppendIngress() error = %v, want ErrStaleGeneration", err)
	}
	ctx, cancel = supervisorTestContext(t)
	err = oldSink.EmitEphemeral(ctx, EphemeralEvent{
		AccountID:  "account-1",
		Generation: 1,
		Typing:     &TypingEvent{Typing: true},
	})
	cancel()
	if !errors.Is(err, ErrStaleGeneration) {
		t.Fatalf("stale EmitEphemeral() error = %v, want ErrStaleGeneration", err)
	}
	oldSink.Beat(1, clock.Now(), "late beat")
	runOne.MarkReady()
	runOne.Complete(errors.New("late completion"))
	supervisorSync(t, supervisor)

	got := supervisor.Snapshot()
	if got.State != want.State || got.Generation != want.Generation ||
		!got.LastEventAt.Equal(want.LastEventAt) ||
		!got.LastProbeOKAt.Equal(want.LastProbeOKAt) ||
		!got.LivenessDeadline.Equal(want.LivenessDeadline) {
		t.Fatalf("late generation-one callbacks changed snapshot:\n got  %+v\n want %+v", got, want)
	}
	if ingress, ephemeral := downstream.Counts(); ingress != 0 || ephemeral != 0 {
		t.Fatalf("downstream received stale callbacks: ingress=%d ephemeral=%d", ingress, ephemeral)
	}
}

func TestSupervisorReadyIsRequiredAndConnectedBooleanDoesNotRenewLiveness(t *testing.T) {
	clock := newSupervisorManualClock(supervisorTestEpoch)
	run := newSupervisorTestRun()
	run.probe = func(ctx context.Context) (Liveness, error) {
		select {
		case <-ctx.Done():
			return Liveness{}, ctx.Err()
		case <-time.After(supervisorTestTimeout):
			return Liveness{}, errors.New("test probe was not canceled")
		}
	}
	lifecycle := &supervisorTestLifecycle{scripts: []supervisorStartScript{{run: run}}}
	policy := supervisorTestPolicy()
	supervisor := newTestSupervisor(
		t,
		lifecycle,
		policy,
		clock,
		&supervisorScriptedRandom{},
	)

	if err := supervisorStart(t, supervisor, StartRequest{}); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	run.connected.Store(true) // Deliberately not part of the Run contract.
	supervisorSync(t, supervisor)
	if got := supervisor.Snapshot().State; got != StateConnecting {
		t.Fatalf("state with Connected=true but Ready open = %q, want connecting", got)
	}

	run.MarkReady()
	online := awaitSupervisorSnapshot(t, supervisor, "ready closes", func(snapshot Snapshot) bool {
		return snapshot.State == StateOnline
	})
	if !online.LastEventAt.IsZero() || !online.LastProbeOKAt.IsZero() {
		t.Fatalf("Ready/Connected fabricated liveness facts: %+v", online)
	}

	// Leaving the adapter's library-specific boolean true is inert. With no
	// event or explicit Beat, the original liveness deadline still expires.
	clock.Advance(policy.LivenessTimeout)
	receiveSupervisorValue(t, run.probeStarted, "probe start after liveness deadline")
	degraded := awaitSupervisorSnapshot(t, supervisor, "degraded after liveness miss", func(snapshot Snapshot) bool {
		return snapshot.State == StateDegraded
	})
	if !degraded.LastEventAt.IsZero() || !degraded.LastProbeOKAt.IsZero() {
		t.Fatalf("Connected=true renewed liveness: %+v", degraded)
	}
}

func TestSupervisorLivenessTimeoutBoundsProbeAndJoinsBeforeRestart(t *testing.T) {
	clock := newSupervisorManualClock(supervisorTestEpoch)
	runOne := newSupervisorTestRun()
	runOne.probe = func(ctx context.Context) (Liveness, error) {
		select {
		case <-ctx.Done():
			return Liveness{}, ctx.Err()
		case <-time.After(supervisorTestTimeout):
			return Liveness{}, errors.New("test probe was not canceled")
		}
	}
	runTwo := newSupervisorTestRun()
	lifecycle := &supervisorTestLifecycle{scripts: []supervisorStartScript{
		{run: runOne},
		{run: runTwo},
	}}
	random := &supervisorScriptedRandom{values: []int64{int64(time.Second)}}
	policy := supervisorTestPolicy()
	supervisor := newTestSupervisor(t, lifecycle, policy, clock, random)

	if err := supervisorStart(t, supervisor, StartRequest{}); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	runOne.MarkReady()
	awaitSupervisorSnapshot(t, supervisor, "online", func(snapshot Snapshot) bool {
		return snapshot.State == StateOnline
	})

	clock.Advance(policy.LivenessTimeout)
	receiveSupervisorValue(t, runOne.probeStarted, "bounded probe start")
	awaitSupervisorSnapshot(t, supervisor, "degraded", func(snapshot Snapshot) bool {
		return snapshot.State == StateDegraded
	})
	if got := runOne.ProbeCount(); got != 1 {
		t.Fatalf("Probe calls = %d, want exactly 1", got)
	}

	clock.Advance(policy.ProbeTimeout)
	backoff := awaitSupervisorSnapshot(t, supervisor, "backoff after probe timeout", func(snapshot Snapshot) bool {
		return snapshot.State == StateBackoff
	})
	if got := runOne.StopCount(); got != 1 {
		t.Fatalf("generation one Stop calls before backoff = %d, want 1", got)
	}
	if got := lifecycle.CallCount(); got != 1 {
		t.Fatalf("starts before backoff deadline = %d, want 1", got)
	}

	clock.Advance(backoff.RetryAt.Sub(clock.Now()))
	calls := awaitSupervisorStartCount(t, lifecycle, 2)
	if runOne.StopCount() != 1 {
		t.Fatal("generation two started before generation one Stop joined")
	}
	if calls[1].request.Generation != 2 {
		t.Fatalf("restart generation = %d, want 2", calls[1].request.Generation)
	}
	runTwo.MarkReady()
	awaitSupervisorSnapshot(t, supervisor, "restarted online", func(snapshot Snapshot) bool {
		return snapshot.State == StateOnline && snapshot.Generation == 2
	})
}

func TestSupervisorTransientBackoffUsesFullJitterExponentialCaps(t *testing.T) {
	clock := newSupervisorManualClock(supervisorTestEpoch)
	policy := supervisorTestPolicy()
	policy.MinBackoff = 4 * time.Second
	policy.MaxBackoff = 16 * time.Second
	policy.MaxSameFingerprint = 20
	wantCaps := []time.Duration{
		4 * time.Second,
		8 * time.Second,
		16 * time.Second,
		16 * time.Second,
	}
	wantDelays := []time.Duration{
		time.Second,
		7 * time.Second,
		15 * time.Second,
		3 * time.Second,
	}
	random := &supervisorScriptedRandom{values: []int64{
		int64(wantDelays[0]),
		int64(wantDelays[1]),
		int64(wantDelays[2]),
		int64(wantDelays[3]),
	}}
	lifecycle := &supervisorTestLifecycle{scripts: []supervisorStartScript{
		{err: transientSupervisorError("attempt-1")},
		{err: transientSupervisorError("attempt-2")},
		{err: transientSupervisorError("attempt-3")},
		{err: transientSupervisorError("attempt-4")},
	}}
	supervisor := newTestSupervisor(t, lifecycle, policy, clock, random)

	if err := supervisorStart(t, supervisor, StartRequest{}); err == nil {
		t.Fatal("Start() error = nil, want first scripted transient failure")
	}
	for attempt := range wantCaps {
		wantGeneration := Generation(attempt + 1)
		snapshot := awaitSupervisorSnapshot(t, supervisor, "transient backoff", func(snapshot Snapshot) bool {
			return snapshot.State == StateBackoff && snapshot.Generation == wantGeneration
		})
		delay := snapshot.RetryAt.Sub(clock.Now())
		if delay != wantDelays[attempt] {
			t.Fatalf("attempt %d jitter delay = %s, want %s", attempt+1, delay, wantDelays[attempt])
		}
		if delay < 0 || delay >= wantCaps[attempt] {
			t.Fatalf("attempt %d jitter delay %s is outside [0, %s)", attempt+1, delay, wantCaps[attempt])
		}
		bounds := random.Bounds()
		if len(bounds) != attempt+1 || time.Duration(bounds[attempt]) != wantCaps[attempt] {
			t.Fatalf("attempt %d random bounds = %v, want cap %s", attempt+1, bounds, wantCaps[attempt])
		}
		if attempt+1 < len(wantCaps) {
			clock.Advance(delay)
			awaitSupervisorStartCount(t, lifecycle, attempt+2)
		}
	}
}

func TestSupervisorRateLimitedHonorsRetryAt(t *testing.T) {
	clock := newSupervisorManualClock(supervisorTestEpoch)
	retryAt := supervisorTestEpoch.Add(30 * time.Second)
	runTwo := newSupervisorTestRun()
	lifecycle := &supervisorTestLifecycle{scripts: []supervisorStartScript{
		{err: OpError{
			Class:       FailureRateLimited,
			Operation:   "start",
			RetryAt:     retryAt,
			Fingerprint: "remote-throttle",
		}},
		{run: runTwo},
	}}
	supervisor := newTestSupervisor(
		t,
		lifecycle,
		supervisorTestPolicy(),
		clock,
		&supervisorScriptedRandom{},
	)

	if err := supervisorStart(t, supervisor, StartRequest{}); err == nil {
		t.Fatal("Start() error = nil, want rate-limited failure")
	}
	snapshot := supervisor.Snapshot()
	if snapshot.State != StateBackoff || !snapshot.RetryAt.Equal(retryAt) {
		t.Fatalf("rate-limited snapshot = %+v, want backoff until %s", snapshot, retryAt)
	}

	clock.Advance(29 * time.Second)
	supervisorSync(t, supervisor)
	if got := lifecycle.CallCount(); got != 1 {
		t.Fatalf("starts before RetryAt = %d, want 1", got)
	}
	clock.Advance(time.Second)
	calls := awaitSupervisorStartCount(t, lifecycle, 2)
	if now := clock.Now(); !now.Equal(retryAt) {
		t.Fatalf("restart time = %s, want RetryAt %s", now, retryAt)
	}
	if calls[1].request.Generation != 2 {
		t.Fatalf("rate-limited restart generation = %d, want 2", calls[1].request.Generation)
	}
}

func TestSupervisorRepeatedFingerprintOpensCircuit(t *testing.T) {
	clock := newSupervisorManualClock(supervisorTestEpoch)
	policy := supervisorTestPolicy()
	policy.MaxSameFingerprint = 3
	lifecycle := &supervisorTestLifecycle{scripts: []supervisorStartScript{
		{err: transientSupervisorError("same-poison")},
		{err: transientSupervisorError("same-poison")},
		{err: transientSupervisorError("same-poison")},
	}}
	random := &supervisorScriptedRandom{values: []int64{
		int64(time.Second),
		int64(2 * time.Second),
	}}
	supervisor := newTestSupervisor(t, lifecycle, policy, clock, random)

	if err := supervisorStart(t, supervisor, StartRequest{}); err == nil {
		t.Fatal("Start() error = nil, want transient failure")
	}
	for generation := Generation(1); generation < 3; generation++ {
		snapshot := awaitSupervisorSnapshot(t, supervisor, "pre-circuit backoff", func(snapshot Snapshot) bool {
			return snapshot.State == StateBackoff && snapshot.Generation == generation
		})
		clock.Advance(snapshot.RetryAt.Sub(clock.Now()))
		awaitSupervisorStartCount(t, lifecycle, int(generation)+1)
	}
	blocked := awaitSupervisorSnapshot(t, supervisor, "open circuit", func(snapshot Snapshot) bool {
		return snapshot.State == StateBlocked && snapshot.Generation == 3
	})
	if blocked.ErrorFingerprint != "same-poison" || blocked.ErrorClass != FailureTransient {
		t.Fatalf("circuit snapshot = %+v", blocked)
	}
	if !blocked.RetryAt.IsZero() {
		t.Fatalf("circuit RetryAt = %s, want zero", blocked.RetryAt)
	}
	clock.Advance(24 * time.Hour)
	supervisorSync(t, supervisor)
	if got := lifecycle.CallCount(); got != 3 {
		t.Fatalf("starts after circuit opened = %d, want 3", got)
	}
}

func TestSupervisorCredentialRepairControlsRestart(t *testing.T) {
	t.Run("success starts exactly one new generation", func(t *testing.T) {
		clock := newSupervisorManualClock(supervisorTestEpoch)
		runTwo := newSupervisorTestRun()
		failure := OpError{
			Class:       FailureCredentialsExpired,
			Operation:   "start",
			Fingerprint: "expired-cookie-set",
		}
		lifecycle := &supervisorTestLifecycle{scripts: []supervisorStartScript{
			{err: failure},
			{run: runTwo},
		}}
		repairer := newSupervisorControlledRepairer()
		supervisor := newTestSupervisor(
			t,
			lifecycle,
			supervisorTestPolicy(),
			clock,
			&supervisorScriptedRandom{},
			WithCredentialRepairer(repairer),
		)

		if err := supervisorStart(t, supervisor, StartRequest{}); err == nil {
			t.Fatal("Start() error = nil, want credentials-expired failure")
		}
		call := receiveSupervisorValue(t, repairer.started, "credential repair start")
		if call.accountID != "account-1" || call.failure.Fingerprint != failure.Fingerprint {
			t.Fatalf("repair call = %+v, want account/failure propagated", call)
		}
		if got := supervisor.Snapshot().State; got != StateRepairingCredentials {
			t.Fatalf("state during repair = %q, want repairing_credentials", got)
		}
		sendSupervisorValue(t, repairer.responses, error(nil), "successful repair")
		calls := awaitSupervisorStartCount(t, lifecycle, 2)
		if calls[1].request.Generation != 2 {
			t.Fatalf("post-repair generation = %d, want 2", calls[1].request.Generation)
		}
		awaitSupervisorSnapshot(t, supervisor, "post-repair connecting", func(snapshot Snapshot) bool {
			return snapshot.State == StateConnecting && snapshot.Generation == 2
		})
		supervisorSync(t, supervisor)
		if got := lifecycle.CallCount(); got != 2 {
			t.Fatalf("starts after successful repair = %d, want exactly 2", got)
		}
	})

	t.Run("failure blocks and never reconnects", func(t *testing.T) {
		clock := newSupervisorManualClock(supervisorTestEpoch)
		failure := OpError{
			Class:       FailureCredentialsExpired,
			Operation:   "start",
			Fingerprint: "expired-cookie-set",
		}
		lifecycle := &supervisorTestLifecycle{scripts: []supervisorStartScript{{err: failure}}}
		repairer := newSupervisorControlledRepairer()
		supervisor := newTestSupervisor(
			t,
			lifecycle,
			supervisorTestPolicy(),
			clock,
			&supervisorScriptedRandom{},
			WithCredentialRepairer(repairer),
		)

		if err := supervisorStart(t, supervisor, StartRequest{}); err == nil {
			t.Fatal("Start() error = nil, want credentials-expired failure")
		}
		receiveSupervisorValue(t, repairer.started, "first credential repair start")
		sendSupervisorValue(t, repairer.responses, errors.New("refresh rejected"), "failed repair")
		awaitSupervisorSnapshot(t, supervisor, "blocked after repair failure", func(snapshot Snapshot) bool {
			return snapshot.State == StateBlocked
		})
		clock.Advance(24 * time.Hour)
		supervisorSync(t, supervisor)
		if got := lifecycle.CallCount(); got != 1 {
			t.Fatalf("starts after failed repair = %d, want 1", got)
		}

		// An explicit retry retries the atomic repair; a second failure still
		// does not bypass repair by starting a connection.
		if err := supervisorRetryBlocked(t, supervisor); err != nil {
			t.Fatalf("RetryBlocked() error = %v", err)
		}
		receiveSupervisorValue(t, repairer.started, "second credential repair start")
		sendSupervisorValue(t, repairer.responses, errors.New("still rejected"), "second failed repair")
		awaitSupervisorSnapshot(t, supervisor, "blocked after second repair failure", func(snapshot Snapshot) bool {
			return snapshot.State == StateBlocked
		})
		if got := lifecycle.CallCount(); got != 1 {
			t.Fatalf("starts after retried failed repair = %d, want 1", got)
		}
	})
}

func TestSupervisorTerminalFailuresRequireExplicitExit(t *testing.T) {
	terminalClasses := []FailureClass{
		FailureReauthRequired,
		FailureUpgradeRequired,
		FailureMisconfigured,
		FailureUnsupported,
	}
	for _, class := range terminalClasses {
		class := class
		t.Run(string(class), func(t *testing.T) {
			clock := newSupervisorManualClock(supervisorTestEpoch)
			runTwo := newSupervisorTestRun()
			lifecycle := &supervisorTestLifecycle{scripts: []supervisorStartScript{
				{err: OpError{Class: class, Operation: "start", Fingerprint: "inputs-v1"}},
				{run: runTwo},
			}}
			supervisor := newTestSupervisor(
				t,
				lifecycle,
				supervisorTestPolicy(),
				clock,
				&supervisorScriptedRandom{},
			)

			if err := supervisorStart(t, supervisor, StartRequest{}); err == nil {
				t.Fatal("Start() error = nil, want terminal failure")
			}
			blocked := supervisor.Snapshot()
			if blocked.State != StateBlocked || blocked.ErrorClass != class {
				t.Fatalf("terminal snapshot = %+v, want blocked class %q", blocked, class)
			}
			clock.Advance(24 * time.Hour)
			supervisorSync(t, supervisor)
			if got := lifecycle.CallCount(); got != 1 {
				t.Fatalf("automatic starts while blocked = %d, want 1", got)
			}

			if err := supervisorInputsChanged(t, supervisor, "inputs-v1"); !errors.Is(err, ErrFingerprintUnchanged) {
				t.Fatalf("InputsChanged(unchanged) error = %v, want ErrFingerprintUnchanged", err)
			}
			if got := lifecycle.CallCount(); got != 1 {
				t.Fatalf("starts after unchanged fingerprint = %d, want 1", got)
			}
			if err := supervisorInputsChanged(t, supervisor, "inputs-v2"); err != nil {
				t.Fatalf("InputsChanged(changed) error = %v", err)
			}
			calls := awaitSupervisorStartCount(t, lifecycle, 2)
			if calls[1].request.Generation != 2 {
				t.Fatalf("changed-input generation = %d, want 2", calls[1].request.Generation)
			}
		})
	}

	t.Run("user retry", func(t *testing.T) {
		clock := newSupervisorManualClock(supervisorTestEpoch)
		runTwo := newSupervisorTestRun()
		lifecycle := &supervisorTestLifecycle{scripts: []supervisorStartScript{
			{err: OpError{
				Class:       FailureUpgradeRequired,
				Operation:   "start",
				Fingerprint: "binary-v1",
			}},
			{run: runTwo},
		}}
		supervisor := newTestSupervisor(
			t,
			lifecycle,
			supervisorTestPolicy(),
			clock,
			&supervisorScriptedRandom{},
		)
		if err := supervisorStart(t, supervisor, StartRequest{}); err == nil {
			t.Fatal("Start() error = nil, want upgrade-required failure")
		}
		if err := supervisorRetryBlocked(t, supervisor); err != nil {
			t.Fatalf("RetryBlocked() error = %v", err)
		}
		calls := awaitSupervisorStartCount(t, lifecycle, 2)
		if calls[1].request.Generation != 2 {
			t.Fatalf("explicit-retry generation = %d, want 2", calls[1].request.Generation)
		}
	})
}

func TestSupervisorPairingIsSingleFlightAndExpiryDoesNotAutoPair(t *testing.T) {
	clock := newSupervisorManualClock(supervisorTestEpoch)
	lifecycle := &supervisorTestLifecycle{}
	pairer := newSupervisorControlledPairer()
	supervisor := newTestSupervisor(
		t,
		lifecycle,
		supervisorTestPolicy(),
		clock,
		&supervisorScriptedRandom{},
		WithPairer(pairer),
		WithInitialState(StateUnpaired),
	)

	request := PairRequest{Method: PairQR}
	if err := supervisorBeginPairing(t, supervisor, request); err != nil {
		t.Fatalf("BeginPairing() error = %v", err)
	}
	call := receiveSupervisorValue(t, pairer.started, "pairing worker start")
	if call.request.AccountID != "account-1" {
		t.Fatalf("pairing account = %q, want account-1", call.request.AccountID)
	}
	if err := supervisorBeginPairing(t, supervisor, request); !errors.Is(err, ErrPairingInProgress) {
		t.Fatalf("concurrent BeginPairing() error = %v, want ErrPairingInProgress", err)
	}

	expiresAt := clock.Now().Add(5 * time.Second)
	ctx, cancel := supervisorTestContext(t)
	err := call.sink.EmitPairEvent(ctx, PairEvent{
		Kind:      "qr",
		Payload:   "qr-payload",
		ExpiresAt: expiresAt,
	})
	cancel()
	if err != nil {
		t.Fatalf("EmitPairEvent(qr) error = %v", err)
	}
	clock.Advance(5 * time.Second)
	expired := awaitSupervisorSnapshot(t, supervisor, "pairing expiry", func(snapshot Snapshot) bool {
		return snapshot.State == StateUnpaired
	})
	if !expired.RetryAt.IsZero() {
		t.Fatalf("QR expiry RetryAt = %s, want zero", expired.RetryAt)
	}
	receiveSupervisorValue(t, pairer.completed, "expired pairing worker join")
	clock.Advance(time.Hour)
	supervisorSync(t, supervisor)
	if got := pairer.CallCount(); got != 1 {
		t.Fatalf("pair attempts after QR expiry = %d, want exactly 1", got)
	}
	if got := lifecycle.CallCount(); got != 0 {
		t.Fatalf("connection starts after QR expiry = %d, want 0", got)
	}
}

func TestSupervisorPairingTemporaryBanPersistsRetryAt(t *testing.T) {
	clock := newSupervisorManualClock(supervisorTestEpoch)
	lifecycle := &supervisorTestLifecycle{}
	pairer := newSupervisorControlledPairer()
	supervisor := newTestSupervisor(
		t,
		lifecycle,
		supervisorTestPolicy(),
		clock,
		&supervisorScriptedRandom{},
		WithPairer(pairer),
		WithInitialState(StateUnpaired),
	)
	request := PairRequest{Method: PairQR}
	if err := supervisorBeginPairing(t, supervisor, request); err != nil {
		t.Fatalf("BeginPairing() error = %v", err)
	}
	call := receiveSupervisorValue(t, pairer.started, "temporarily banned pairing start")
	blockedUntil := clock.Now().Add(30 * time.Second)
	ctx, cancel := supervisorTestContext(t)
	err := call.sink.EmitPairEvent(ctx, PairEvent{
		Kind:      "blocked",
		ExpiresAt: blockedUntil,
		Message:   "temporary-ban",
	})
	cancel()
	if err != nil {
		t.Fatalf("EmitPairEvent(blocked) error = %v", err)
	}
	blocked := awaitSupervisorSnapshot(t, supervisor, "pairing temporary ban", func(snapshot Snapshot) bool {
		return snapshot.State == StateUnpaired && snapshot.RetryAt.Equal(blockedUntil)
	})
	if blocked.ErrorClass != FailureRateLimited || blocked.ErrorFingerprint != "temporary-ban" {
		t.Fatalf("temporary-ban snapshot = %+v", blocked)
	}
	receiveSupervisorValue(t, pairer.completed, "temporarily banned pairing worker join")
	awaitPairingAdmissionError(t, supervisor, request, ErrPairingRateLimited)

	clock.Advance(29 * time.Second)
	awaitPairingAdmissionError(t, supervisor, request, ErrPairingRateLimited)
	if got := pairer.CallCount(); got != 1 {
		t.Fatalf("pair attempts before temporary-ban expiry = %d, want 1", got)
	}
	clock.Advance(time.Second)
	supervisorSync(t, supervisor)
	if got := pairer.CallCount(); got != 1 {
		t.Fatalf("automatic pair attempts at temporary-ban expiry = %d, want 1", got)
	}
	if err := supervisorBeginPairing(t, supervisor, request); err != nil {
		t.Fatalf("user BeginPairing() at expiry error = %v", err)
	}
	receiveSupervisorValue(t, pairer.started, "user pairing after temporary-ban expiry")
	if got := pairer.CallCount(); got != 2 {
		t.Fatalf("pair attempts after user retry = %d, want 2", got)
	}
}

func TestSupervisorStopJoinsCurrentGeneration(t *testing.T) {
	clock := newSupervisorManualClock(supervisorTestEpoch)
	run := newSupervisorTestRun()
	joinGate := make(chan struct{})
	stopEntered := make(chan struct{}, 1)
	var releaseJoin sync.Once
	var joined atomic.Bool
	run.stop = func(context.Context) error {
		select {
		case stopEntered <- struct{}{}:
		default:
		}
		select {
		case <-joinGate:
			joined.Store(true)
			return nil
		case <-time.After(supervisorTestTimeout):
			return errors.New("test generation did not receive join release")
		}
	}
	lifecycle := &supervisorTestLifecycle{scripts: []supervisorStartScript{{run: run}}}
	supervisor := newTestSupervisor(
		t,
		lifecycle,
		supervisorTestPolicy(),
		clock,
		&supervisorScriptedRandom{},
	)
	// This cleanup is registered after the supervisor cleanup, so LIFO cleanup
	// always releases a deliberately blocked fake Stop before calling Stop again.
	t.Cleanup(func() { releaseJoin.Do(func() { close(joinGate) }) })

	if err := supervisorStart(t, supervisor, StartRequest{}); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	run.MarkReady()
	awaitSupervisorSnapshot(t, supervisor, "online before stop", func(snapshot Snapshot) bool {
		return snapshot.State == StateOnline
	})

	stopResult := make(chan error, 1)
	stopCtx, stopCancel := context.WithTimeout(context.Background(), supervisorTestTimeout)
	t.Cleanup(stopCancel)
	go func() {
		stopResult <- supervisor.Stop(stopCtx)
	}()
	receiveSupervisorValue(t, stopEntered, "Run.Stop entry")
	awaitSupervisorSnapshot(t, supervisor, "stopping while joining", func(snapshot Snapshot) bool {
		return snapshot.State == StateStopping
	})
	select {
	case err := <-stopResult:
		t.Fatalf("Supervisor.Stop() returned before generation joined: %v", err)
	default:
	}

	releaseJoin.Do(func() { close(joinGate) })
	if err := receiveSupervisorValue(t, stopResult, "Supervisor.Stop completion"); err != nil {
		t.Fatalf("Supervisor.Stop() error = %v", err)
	}
	if !joined.Load() {
		t.Fatal("Supervisor.Stop() returned before fake generation reported joined")
	}
	if got := run.StopCount(); got != 1 {
		t.Fatalf("Run.Stop calls = %d, want 1", got)
	}
	if got := supervisor.Snapshot().State; got != StateStopped {
		t.Fatalf("final state = %q, want stopped", got)
	}
}

func supervisorTestPolicy() Policy {
	return Policy{
		ConnectTimeout:     20 * time.Second,
		ProbeEvery:         2 * time.Second,
		ProbeTimeout:       3 * time.Second,
		LivenessTimeout:    10 * time.Second,
		MinBackoff:         4 * time.Second,
		MaxBackoff:         time.Minute,
		MaxSameFingerprint: 5,
	}
}

func newTestSupervisor(
	t *testing.T,
	lifecycle Lifecycle,
	policy Policy,
	clock Clock,
	random Random,
	options ...SupervisorOption,
) *Supervisor {
	t.Helper()
	supervisor, err := NewSupervisor(
		"account-1",
		Platform("test"),
		lifecycle,
		policy,
		clock,
		random,
		options...,
	)
	if err != nil {
		t.Fatalf("NewSupervisor() error = %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), supervisorTestTimeout)
		defer cancel()
		if err := supervisor.Stop(ctx); err != nil {
			t.Errorf("Supervisor.Stop() cleanup error = %v", err)
		}
	})
	return supervisor
}

func supervisorTestContext(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	return context.WithTimeout(context.Background(), supervisorTestTimeout)
}

func supervisorStart(t *testing.T, supervisor *Supervisor, request StartRequest) error {
	t.Helper()
	ctx, cancel := supervisorTestContext(t)
	defer cancel()
	return supervisor.Start(ctx, request)
}

func supervisorRetryBlocked(t *testing.T, supervisor *Supervisor) error {
	t.Helper()
	ctx, cancel := supervisorTestContext(t)
	defer cancel()
	return supervisor.RetryBlocked(ctx)
}

func supervisorInputsChanged(t *testing.T, supervisor *Supervisor, fingerprint string) error {
	t.Helper()
	ctx, cancel := supervisorTestContext(t)
	defer cancel()
	return supervisor.InputsChanged(ctx, fingerprint)
}

func supervisorBeginPairing(t *testing.T, supervisor *Supervisor, request PairRequest) error {
	t.Helper()
	ctx, cancel := supervisorTestContext(t)
	defer cancel()
	return supervisor.BeginPairing(ctx, request, nil)
}

func supervisorSync(t *testing.T, supervisor *Supervisor) {
	t.Helper()
	ctx, cancel := supervisorTestContext(t)
	defer cancel()
	if err := supervisor.Sync(ctx); err != nil {
		t.Fatalf("Supervisor.Sync() error = %v", err)
	}
}

func awaitSupervisorSnapshot(
	t *testing.T,
	supervisor *Supervisor,
	description string,
	predicate func(Snapshot) bool,
) Snapshot {
	t.Helper()
	timer := time.NewTimer(supervisorTestTimeout)
	defer timer.Stop()
	for {
		snapshot := supervisor.Snapshot()
		if predicate(snapshot) {
			return snapshot
		}
		select {
		case <-timer.C:
			t.Fatalf("timed out waiting for %s; last snapshot: %+v", description, snapshot)
		default:
			runtime.Gosched()
		}
	}
}

func awaitSupervisorStartCount(
	t *testing.T,
	lifecycle *supervisorTestLifecycle,
	want int,
) []supervisorStartCall {
	t.Helper()
	timer := time.NewTimer(supervisorTestTimeout)
	defer timer.Stop()
	for {
		calls := lifecycle.Calls()
		if len(calls) >= want {
			return calls
		}
		select {
		case <-timer.C:
			t.Fatalf("timed out waiting for %d lifecycle starts; got %d", want, len(calls))
		default:
			runtime.Gosched()
		}
	}
}

func receiveSupervisorValue[T any](t *testing.T, channel <-chan T, description string) T {
	t.Helper()
	select {
	case value := <-channel:
		return value
	case <-time.After(supervisorTestTimeout):
		var zero T
		t.Fatalf("timed out waiting for %s", description)
		return zero
	}
}

func sendSupervisorValue[T any](t *testing.T, channel chan<- T, value T, description string) {
	t.Helper()
	select {
	case channel <- value:
	case <-time.After(supervisorTestTimeout):
		t.Fatalf("timed out sending %s", description)
	}
}

func awaitPairingAdmissionError(
	t *testing.T,
	supervisor *Supervisor,
	request PairRequest,
	want error,
) {
	t.Helper()
	timer := time.NewTimer(supervisorTestTimeout)
	defer timer.Stop()
	for {
		err := supervisorBeginPairing(t, supervisor, request)
		if errors.Is(err, want) {
			return
		}
		if !errors.Is(err, ErrPairingInProgress) {
			t.Fatalf("BeginPairing() error = %v, want %v", err, want)
		}
		select {
		case <-timer.C:
			t.Fatalf("timed out waiting for BeginPairing() error %v", want)
		default:
			runtime.Gosched()
		}
	}
}

func transientSupervisorError(fingerprint string) error {
	return OpError{
		Class:       FailureTransient,
		Operation:   "start",
		Fingerprint: fingerprint,
	}
}

type supervisorManualClock struct {
	mu     sync.Mutex
	now    time.Time
	timers map[*supervisorManualTimer]struct{}
}

func newSupervisorManualClock(now time.Time) *supervisorManualClock {
	return &supervisorManualClock{
		now:    now,
		timers: make(map[*supervisorManualTimer]struct{}),
	}
}

func (c *supervisorManualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *supervisorManualClock) NewTimer(delay time.Duration) Timer {
	if delay < 0 {
		delay = 0
	}
	c.mu.Lock()
	timer := &supervisorManualTimer{
		clock:    c,
		deadline: c.now.Add(delay),
		channel:  make(chan time.Time, 1),
		active:   true,
	}
	due := delay == 0
	if due {
		timer.active = false
	} else {
		c.timers[timer] = struct{}{}
	}
	now := c.now
	c.mu.Unlock()
	if due {
		timer.channel <- now
	}
	return timer
}

func (c *supervisorManualClock) Advance(delta time.Duration) {
	if delta < 0 {
		panic("manual clock cannot move backward")
	}
	c.mu.Lock()
	c.now = c.now.Add(delta)
	now := c.now
	var due []*supervisorManualTimer
	for timer := range c.timers {
		if !now.Before(timer.deadline) {
			timer.active = false
			delete(c.timers, timer)
			due = append(due, timer)
		}
	}
	c.mu.Unlock()
	for _, timer := range due {
		timer.channel <- now
	}
}

type supervisorManualTimer struct {
	clock    *supervisorManualClock
	deadline time.Time
	channel  chan time.Time
	active   bool
}

func (t *supervisorManualTimer) C() <-chan time.Time {
	return t.channel
}

func (t *supervisorManualTimer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	if !t.active {
		return false
	}
	t.active = false
	delete(t.clock.timers, t)
	return true
}

type supervisorStartScript struct {
	run *supervisorTestRun
	err error
}

type supervisorStartCall struct {
	request StartRequest
	sink    ConnectionSink
}

type supervisorTestLifecycle struct {
	mu      sync.Mutex
	scripts []supervisorStartScript
	calls   []supervisorStartCall
}

func (l *supervisorTestLifecycle) Start(
	_ context.Context,
	request StartRequest,
	sink ConnectionSink,
) (Run, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	callIndex := len(l.calls)
	l.calls = append(l.calls, supervisorStartCall{request: request, sink: sink})
	if callIndex >= len(l.scripts) {
		return nil, errors.New("unexpected lifecycle start")
	}
	script := l.scripts[callIndex]
	return script.run, script.err
}

func (l *supervisorTestLifecycle) Calls() []supervisorStartCall {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]supervisorStartCall(nil), l.calls...)
}

func (l *supervisorTestLifecycle) CallCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.calls)
}

type supervisorTestRun struct {
	ready chan struct{}
	done  chan error

	readyOnce sync.Once
	doneOnce  sync.Once
	mu        sync.Mutex
	probes    int
	stops     int
	probe     func(context.Context) (Liveness, error)
	stop      func(context.Context) error

	probeStarted chan struct{}
	connected    atomic.Bool
}

func newSupervisorTestRun() *supervisorTestRun {
	return &supervisorTestRun{
		ready:        make(chan struct{}),
		done:         make(chan error, 1),
		probeStarted: make(chan struct{}, 1),
	}
}

func (r *supervisorTestRun) Ready() <-chan struct{} {
	return r.ready
}

func (r *supervisorTestRun) Done() <-chan error {
	return r.done
}

func (r *supervisorTestRun) Probe(ctx context.Context) (Liveness, error) {
	r.mu.Lock()
	r.probes++
	probe := r.probe
	r.mu.Unlock()
	select {
	case r.probeStarted <- struct{}{}:
	default:
	}
	if probe == nil {
		return Liveness{}, errors.New("unexpected probe")
	}
	return probe(ctx)
}

func (r *supervisorTestRun) Stop(ctx context.Context) error {
	r.mu.Lock()
	r.stops++
	stop := r.stop
	r.mu.Unlock()
	if stop == nil {
		return nil
	}
	return stop(ctx)
}

func (r *supervisorTestRun) MarkReady() {
	r.readyOnce.Do(func() { close(r.ready) })
}

func (r *supervisorTestRun) Complete(err error) {
	r.doneOnce.Do(func() {
		if err != nil {
			r.done <- err
		}
		close(r.done)
	})
}

func (r *supervisorTestRun) ProbeCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.probes
}

func (r *supervisorTestRun) StopCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stops
}

type supervisorScriptedRandom struct {
	mu     sync.Mutex
	values []int64
	bounds []int64
}

func (r *supervisorScriptedRandom) Int63n(bound int64) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.bounds = append(r.bounds, bound)
	index := len(r.bounds) - 1
	if index >= len(r.values) {
		return 0
	}
	return r.values[index]
}

func (r *supervisorScriptedRandom) Bounds() []int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]int64(nil), r.bounds...)
}

type supervisorRecordingSink struct {
	mu        sync.Mutex
	ingress   []RawIngressRecord
	ephemeral []EphemeralEvent
}

func (s *supervisorRecordingSink) AppendIngress(_ context.Context, record RawIngressRecord) error {
	s.mu.Lock()
	s.ingress = append(s.ingress, record)
	s.mu.Unlock()
	return nil
}

func (s *supervisorRecordingSink) EmitEphemeral(_ context.Context, event EphemeralEvent) error {
	s.mu.Lock()
	s.ephemeral = append(s.ephemeral, event)
	s.mu.Unlock()
	return nil
}

func (*supervisorRecordingSink) Beat(Generation, time.Time, string) {}

func (s *supervisorRecordingSink) Counts() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.ingress), len(s.ephemeral)
}

type supervisorRepairCall struct {
	accountID string
	failure   OpError
}

type supervisorControlledRepairer struct {
	started   chan supervisorRepairCall
	responses chan error
}

func newSupervisorControlledRepairer() *supervisorControlledRepairer {
	return &supervisorControlledRepairer{
		started:   make(chan supervisorRepairCall, 4),
		responses: make(chan error, 4),
	}
}

func (r *supervisorControlledRepairer) RepairCredentials(
	ctx context.Context,
	accountID string,
	failure OpError,
) error {
	select {
	case r.started <- supervisorRepairCall{accountID: accountID, failure: failure}:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-r.responses:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

type supervisorPairCall struct {
	request PairRequest
	sink    PairSink
}

type supervisorPairResponse struct {
	result PairResult
	err    error
}

type supervisorControlledPairer struct {
	calls     atomic.Int32
	started   chan supervisorPairCall
	completed chan struct{}
	responses chan supervisorPairResponse
}

func newSupervisorControlledPairer() *supervisorControlledPairer {
	return &supervisorControlledPairer{
		started:   make(chan supervisorPairCall, 4),
		completed: make(chan struct{}, 4),
		responses: make(chan supervisorPairResponse, 4),
	}
}

func (p *supervisorControlledPairer) Pair(
	ctx context.Context,
	request PairRequest,
	sink PairSink,
) (PairResult, error) {
	p.calls.Add(1)
	select {
	case p.started <- supervisorPairCall{request: request, sink: sink}:
	case <-ctx.Done():
		return PairResult{}, ctx.Err()
	}
	defer func() {
		select {
		case p.completed <- struct{}{}:
		default:
		}
	}()
	select {
	case response := <-p.responses:
		return response.result, response.err
	case <-ctx.Done():
		return PairResult{}, ctx.Err()
	}
}

func (*supervisorControlledPairer) Unpair(context.Context, string) error {
	return nil
}

func (p *supervisorControlledPairer) CallCount() int {
	return int(p.calls.Load())
}

var (
	_ Clock              = (*supervisorManualClock)(nil)
	_ Timer              = (*supervisorManualTimer)(nil)
	_ Lifecycle          = (*supervisorTestLifecycle)(nil)
	_ Run                = (*supervisorTestRun)(nil)
	_ Random             = (*supervisorScriptedRandom)(nil)
	_ ConnectionSink     = (*supervisorRecordingSink)(nil)
	_ CredentialRepairer = (*supervisorControlledRepairer)(nil)
	_ Pairer             = (*supervisorControlledPairer)(nil)
)
