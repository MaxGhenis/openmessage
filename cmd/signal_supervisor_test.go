package cmd

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/signallive"
)

func TestSignalSupervisorControlConnectStartsOnceAndActiveDuplicateIsNoop(t *testing.T) {
	lifecycle := &signalControlTestLifecycle{}
	supervisor := newSignalControlTestSupervisor(t, lifecycle)
	control := newSignalSupervisorControl(supervisor, nil, func() string { return "signal-input-v1" })
	t.Cleanup(func() { stopSignalControlForTest(t, control) })

	const callers = 8
	start := make(chan struct{})
	errorsByCaller := make(chan error, callers)
	var callersDone sync.WaitGroup
	callersDone.Add(callers)
	for range callers {
		go func() {
			defer callersDone.Done()
			<-start
			errorsByCaller <- control.Connect()
		}()
	}
	close(start)
	callersDone.Wait()
	close(errorsByCaller)
	for err := range errorsByCaller {
		if err != nil {
			t.Fatalf("concurrent Connect() error = %v", err)
		}
	}

	awaitSignalSupervisorGenerationState(t, supervisor, bridge.StateOnline, 1)
	if got := lifecycle.StartCount(); got != 1 {
		t.Fatalf("lifecycle starts after concurrent Connect() = %d, want 1", got)
	}

	if err := control.Connect(); err != nil {
		t.Fatalf("Connect() while active error = %v", err)
	}
	if got := lifecycle.StartCount(); got != 1 {
		t.Fatalf("lifecycle starts after active duplicate = %d, want 1", got)
	}
}

func TestSignalSupervisorControlBlockedCommandsRetryAndAcceptChangedInputs(t *testing.T) {
	lifecycle := &signalControlTestLifecycle{}
	supervisor := newSignalControlTestSupervisor(t, lifecycle)
	var inputFingerprint atomic.Value
	inputFingerprint.Store("signal-input-v1")
	control := newSignalSupervisorControl(
		supervisor,
		nil,
		func() string { return inputFingerprint.Load().(string) },
	)
	t.Cleanup(func() { stopSignalControlForTest(t, control) })

	if err := control.Connect(); err != nil {
		t.Fatalf("initial Connect() error = %v", err)
	}
	awaitSignalSupervisorGenerationState(t, supervisor, bridge.StateOnline, 1)

	lifecycle.Run(0).Fail(bridge.OpError{
		Class:       bridge.FailureUpgradeRequired,
		Operation:   "receive",
		Fingerprint: "signal-cli-too-old",
		Cause:       errors.New("upgrade signal-cli"),
	})
	awaitSignalSupervisorGenerationState(t, supervisor, bridge.StateBlocked, 1)

	// An ordinary Start is forbidden from StateBlocked. Connect must use the
	// supervisor-owned RetryBlocked command when inputs have not changed.
	if err := control.Connect(); err != nil {
		t.Fatalf("Connect() via RetryBlocked error = %v", err)
	}
	awaitSignalSupervisorGenerationState(t, supervisor, bridge.StateOnline, 2)

	lifecycle.Run(1).Fail(bridge.OpError{
		Class:       bridge.FailureUpgradeRequired,
		Operation:   "receive",
		Fingerprint: "signal-cli-too-old",
		Cause:       errors.New("upgrade signal-cli"),
	})
	awaitSignalSupervisorGenerationState(t, supervisor, bridge.StateBlocked, 2)

	inputFingerprint.Store("signal-input-v2")
	if err := control.Connect(); err != nil {
		t.Fatalf("Connect() via InputsChanged error = %v", err)
	}
	awaitSignalSupervisorGenerationState(t, supervisor, bridge.StateOnline, 3)
	if got := lifecycle.StartCount(); got != 3 {
		t.Fatalf("lifecycle starts = %d, want 3", got)
	}
	control.mu.Lock()
	gotFingerprint := control.lastFingerprint
	control.mu.Unlock()
	if gotFingerprint != "signal-input-v2" {
		t.Fatalf("remembered input fingerprint = %q, want %q", gotFingerprint, "signal-input-v2")
	}
}

func TestSignalSupervisorControlParkRetestRetriesOnlyUnreadablePark(t *testing.T) {
	lifecycle := &signalControlTestLifecycle{}
	supervisor := newSignalControlTestSupervisor(t, lifecycle)
	control := newSignalSupervisorControl(supervisor, nil, func() string { return "signal-input-v1" })
	t.Cleanup(func() { stopSignalControlForTest(t, control) })
	control.StartParkRetest(20*time.Millisecond, zerolog.Nop())

	if err := control.Connect(); err != nil {
		t.Fatalf("initial Connect() error = %v", err)
	}
	awaitSignalSupervisorGenerationState(t, supervisor, bridge.StateOnline, 1)

	// A park whose only evidence was local (signal-cli unable to read an
	// account that accounts.json still lists) must heal on the paced retest
	// without any manual /api/signal/connect.
	lifecycle.Run(0).Fail(bridge.OpError{
		Class:       bridge.FailureReauthRequired,
		Operation:   "probe_account",
		Fingerprint: signallive.SignalAccountUnreadableFingerprint,
		Cause:       errors.New("signal-cli cannot read the linked Signal account"),
	})
	awaitSignalSupervisorGenerationState(t, supervisor, bridge.StateOnline, 2)
	if got := lifecycle.StartCount(); got != 2 {
		t.Fatalf("lifecycle starts after unreadable-park retest = %d, want 2", got)
	}

	// A server-backed reauth park stays user-owned: no retest tick may retry
	// it, no matter how many elapse.
	lifecycle.Run(1).Fail(bridge.OpError{
		Class:       bridge.FailureReauthRequired,
		Operation:   "receive",
		Fingerprint: signallive.SignalAccountInvalidFingerprint,
		Cause:       errors.New("account is not registered"),
	})
	awaitSignalSupervisorGenerationState(t, supervisor, bridge.StateBlocked, 2)
	time.Sleep(200 * time.Millisecond)
	snapshot := awaitSignalSupervisorGenerationState(t, supervisor, bridge.StateBlocked, 2)
	if snapshot.ErrorFingerprint != signallive.SignalAccountInvalidFingerprint {
		t.Fatalf("blocked fingerprint = %q, want %q", snapshot.ErrorFingerprint, signallive.SignalAccountInvalidFingerprint)
	}
	if got := lifecycle.StartCount(); got != 2 {
		t.Fatalf("lifecycle starts after genuine reauth park = %d, want 2 (no automatic retry)", got)
	}
}

func TestSignalSupervisorControlStopAndUnpairJoinsThenRebuilds(t *testing.T) {
	firstStopRelease := make(chan struct{})
	lifecycle := &signalControlTestLifecycle{firstStopRelease: firstStopRelease}
	var supervisorCount atomic.Int32
	newSupervisor := func() (*bridge.Supervisor, error) {
		supervisorCount.Add(1)
		return bridge.NewSupervisor(
			signalAccountID,
			bridge.PlatformSignal,
			lifecycle,
			signalSupervisorPolicy(),
			signalWallClock{},
			signalRandom{},
		)
	}
	firstSupervisor, err := newSupervisor()
	if err != nil {
		t.Fatalf("NewSupervisor() error = %v", err)
	}
	control := newSignalSupervisorControl(
		firstSupervisor,
		newSupervisor,
		func() string { return "signal-input-v1" },
	)
	t.Cleanup(func() { stopSignalControlForTest(t, control) })

	if err := control.Connect(); err != nil {
		t.Fatalf("initial Connect() error = %v", err)
	}
	awaitSignalSupervisorGenerationState(t, firstSupervisor, bridge.StateOnline, 1)
	firstRun := lifecycle.Run(0)

	unpairCalled := make(chan struct{})
	stopResult := make(chan error, 1)
	go func() {
		stopResult <- control.StopAndUnpair(func(context.Context) error {
			if !firstRun.Joined() {
				return errors.New("unpair ran before the Signal generation joined")
			}
			close(unpairCalled)
			return nil
		})
	}()

	select {
	case <-firstRun.StopEntered():
	case <-time.After(time.Second):
		t.Fatal("old Signal generation did not begin stopping")
	}
	select {
	case <-unpairCalled:
		t.Fatal("unpair ran while the old Signal generation was still stopping")
	default:
	}
	close(firstStopRelease)
	select {
	case err := <-stopResult:
		if err != nil {
			t.Fatalf("StopAndUnpair() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("StopAndUnpair() did not return after the old generation joined")
	}

	if err := control.Connect(); err != nil {
		t.Fatalf("Connect() after re-pair error = %v", err)
	}
	control.mu.Lock()
	secondSupervisor := control.supervisor
	control.mu.Unlock()
	if secondSupervisor == firstSupervisor {
		t.Fatal("Connect() reused the terminal supervisor after unpair")
	}
	awaitSignalSupervisorGenerationState(t, secondSupervisor, bridge.StateOnline, 1)
	if got := lifecycle.StartCount(); got != 2 {
		t.Fatalf("lifecycle starts = %d, want 2", got)
	}
	if lifecycle.Run(1) == firstRun {
		t.Fatal("Connect() reused the stopped Signal run after unpair")
	}
	if got := supervisorCount.Load(); got != 2 {
		t.Fatalf("supervisors constructed = %d, want 2", got)
	}
}

func TestSignalSupervisorControlExpiredPairingRequiresExplicitFreshConnect(t *testing.T) {
	lifecycle := &signalControlTestLifecycle{}
	var supervisorCount atomic.Int32
	newSupervisor := func() (*bridge.Supervisor, error) {
		supervisorCount.Add(1)
		return bridge.NewSupervisor(
			signalAccountID,
			bridge.PlatformSignal,
			lifecycle,
			signalSupervisorPolicy(),
			signalWallClock{},
			signalRandom{},
		)
	}
	firstSupervisor, err := newSupervisor()
	if err != nil {
		t.Fatalf("NewSupervisor() error = %v", err)
	}
	control := newSignalSupervisorControl(
		firstSupervisor,
		newSupervisor,
		func() string { return "signal-input-v1" },
	)
	t.Cleanup(func() { stopSignalControlForTest(t, control) })

	if err := control.Connect(); err != nil {
		t.Fatalf("initial Connect() error = %v", err)
	}
	awaitSignalSupervisorGenerationState(t, firstSupervisor, bridge.StateOnline, 1)
	lifecycle.Run(0).Fail(bridge.OpError{
		Class:       bridge.FailureUnpaired,
		Operation:   "pair",
		Fingerprint: "signal_pairing_incomplete",
		Cause:       errors.New("Signal link QR expired"),
	})
	awaitSignalSupervisorGenerationState(t, firstSupervisor, bridge.StateUnpaired, 1)

	time.Sleep(20 * time.Millisecond)
	if got := lifecycle.StartCount(); got != 1 {
		t.Fatalf("lifecycle starts without explicit reconnect = %d, want 1", got)
	}
	if err := control.Connect(); err != nil {
		t.Fatalf("explicit Connect() after pairing expiry error = %v", err)
	}
	control.mu.Lock()
	secondSupervisor := control.supervisor
	control.mu.Unlock()
	if secondSupervisor == firstSupervisor {
		t.Fatal("explicit Connect() reused the supervisor left unpaired")
	}
	awaitSignalSupervisorGenerationState(t, secondSupervisor, bridge.StateOnline, 1)
	if got := lifecycle.StartCount(); got != 2 {
		t.Fatalf("lifecycle starts after explicit reconnect = %d, want 2", got)
	}
	if got := supervisorCount.Load(); got != 2 {
		t.Fatalf("supervisors constructed = %d, want 2", got)
	}
}

func TestSignalSupervisorControlTerminalStopPreventsRebuild(t *testing.T) {
	lifecycle := &signalControlTestLifecycle{}
	var supervisorCount atomic.Int32
	newSupervisor := func() (*bridge.Supervisor, error) {
		supervisorCount.Add(1)
		return bridge.NewSupervisor(
			signalAccountID,
			bridge.PlatformSignal,
			lifecycle,
			signalSupervisorPolicy(),
			signalWallClock{},
			signalRandom{},
		)
	}
	supervisor, err := newSupervisor()
	if err != nil {
		t.Fatalf("NewSupervisor() error = %v", err)
	}
	control := newSignalSupervisorControl(
		supervisor,
		newSupervisor,
		func() string { return "signal-input-v1" },
	)

	if err := control.Connect(); err != nil {
		t.Fatalf("initial Connect() error = %v", err)
	}
	awaitSignalSupervisorGenerationState(t, supervisor, bridge.StateOnline, 1)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	if err := control.Stop(ctx); err != nil {
		cancel()
		t.Fatalf("Stop() error = %v", err)
	}
	cancel()

	if err := control.Connect(); !errors.Is(err, bridge.ErrSupervisorStopped) {
		t.Fatalf("Connect() after terminal Stop error = %v, want ErrSupervisorStopped", err)
	}
	if got := supervisorCount.Load(); got != 1 {
		t.Fatalf("supervisors after terminal Stop = %d, want 1", got)
	}
}

func newSignalControlTestSupervisor(
	t *testing.T,
	lifecycle bridge.Lifecycle,
) *bridge.Supervisor {
	t.Helper()
	supervisor, err := bridge.NewSupervisor(
		signalAccountID,
		bridge.PlatformSignal,
		lifecycle,
		signalSupervisorPolicy(),
		signalWallClock{},
		signalRandom{},
	)
	if err != nil {
		t.Fatalf("NewSupervisor() error = %v", err)
	}
	return supervisor
}

func stopSignalControlForTest(t *testing.T, control *signalSupervisorControl) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := control.Stop(ctx); err != nil {
		t.Errorf("signal control Stop() error = %v", err)
	}
}

func awaitSignalSupervisorGenerationState(
	t *testing.T,
	supervisor *bridge.Supervisor,
	want bridge.State,
	wantGeneration bridge.Generation,
) bridge.Snapshot {
	t.Helper()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		snapshot := supervisor.Snapshot()
		if snapshot.State == want && snapshot.Generation == wantGeneration {
			return snapshot
		}
		select {
		case <-deadline.C:
			t.Fatalf(
				"supervisor state/generation = %q/%d, want %q/%d (snapshot %+v)",
				snapshot.State,
				snapshot.Generation,
				want,
				wantGeneration,
				snapshot,
			)
		case <-ticker.C:
		}
	}
}

type signalControlTestLifecycle struct {
	mu               sync.Mutex
	runs             []*signalControlTestRun
	firstStopRelease <-chan struct{}
}

func (l *signalControlTestLifecycle) Start(
	_ context.Context,
	_ bridge.StartRequest,
	_ bridge.ConnectionSink,
) (bridge.Run, error) {
	l.mu.Lock()
	stopRelease := (<-chan struct{})(nil)
	if len(l.runs) == 0 {
		stopRelease = l.firstStopRelease
	}
	run := &signalControlTestRun{
		ready:       make(chan struct{}),
		done:        make(chan error, 1),
		stopEntered: make(chan struct{}),
		stopRelease: stopRelease,
	}
	l.runs = append(l.runs, run)
	l.mu.Unlock()
	close(run.ready)
	return run, nil
}

func (l *signalControlTestLifecycle) StartCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.runs)
}

func (l *signalControlTestLifecycle) Run(index int) *signalControlTestRun {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.runs[index]
}

type signalControlTestRun struct {
	ready       chan struct{}
	done        chan error
	stopEntered chan struct{}
	stopRelease <-chan struct{}

	doneOnce sync.Once
	stopOnce sync.Once
	joined   atomic.Bool
}

func (r *signalControlTestRun) Ready() <-chan struct{} { return r.ready }
func (r *signalControlTestRun) Done() <-chan error     { return r.done }
func (r *signalControlTestRun) Probe(context.Context) (bridge.Liveness, error) {
	return bridge.Liveness{AliveAt: time.Now(), Detail: "test"}, nil
}
func (r *signalControlTestRun) Stop(ctx context.Context) error {
	var stopErr error
	r.stopOnce.Do(func() {
		close(r.stopEntered)
		if r.stopRelease != nil {
			select {
			case <-r.stopRelease:
			case <-ctx.Done():
				stopErr = ctx.Err()
				return
			}
		}
		r.joined.Store(true)
		r.doneOnce.Do(func() { close(r.done) })
	})
	return stopErr
}
func (r *signalControlTestRun) Fail(err error) {
	r.doneOnce.Do(func() {
		r.done <- err
		close(r.done)
	})
}
func (r *signalControlTestRun) Joined() bool                 { return r.joined.Load() }
func (r *signalControlTestRun) StopEntered() <-chan struct{} { return r.stopEntered }
