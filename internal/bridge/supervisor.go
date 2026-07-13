package bridge

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	ErrLifecycleRequired       = errors.New("bridge: lifecycle is required")
	ErrClockRequired           = errors.New("bridge: clock is required")
	ErrRandomRequired          = errors.New("bridge: random source is required")
	ErrInvalidPolicy           = errors.New("bridge: invalid supervisor policy")
	ErrInvalidInitialState     = errors.New("bridge: invalid initial supervisor state")
	ErrSupervisorStopped       = errors.New("bridge: supervisor is stopped")
	errStartReplyDeferred      = errors.New("bridge: start reply deferred to first attempt")
	ErrSupervisorBusy          = errors.New("bridge: supervisor is busy")
	ErrSupervisorBlocked       = errors.New("bridge: supervisor is blocked")
	ErrStaleGeneration         = errors.New("bridge: stale connection generation")
	ErrAccountMismatch         = errors.New("bridge: account does not match supervisor")
	ErrPairerUnavailable       = errors.New("bridge: pairing is unavailable")
	ErrPairingInProgress       = errors.New("bridge: pairing is already in progress")
	ErrPairingNotAllowed       = errors.New("bridge: account is not unpaired")
	ErrPairingRateLimited      = errors.New("bridge: pairing is temporarily rate limited")
	ErrFingerprintUnchanged    = errors.New("bridge: input fingerprint is unchanged")
	ErrCredentialRepairMissing = errors.New("bridge: credential repair is unavailable")
)

// Timer is the stoppable one-shot timer used by Supervisor. Implementations
// used in tests can deliver ticks only when logical time is advanced.
type Timer interface {
	C() <-chan time.Time
	Stop() bool
}

// Clock is the supervisor's only source of time. Supervisor never calls
// time.Now, time.Sleep, context.WithTimeout, or a real timer directly.
type Clock interface {
	Now() time.Time
	NewTimer(time.Duration) Timer
}

// Random supplies the draw for full-jitter backoff. Int63n must follow the
// math/rand contract and return a value in [0, n).
type Random interface {
	Int63n(n int64) int64
}

// CredentialRepairer atomically installs repaired credentials before returning
// nil. Returning an error means no new connection generation may be started.
type CredentialRepairer interface {
	RepairCredentials(ctx context.Context, accountID string, failure OpError) error
}

// CredentialRepairFunc adapts a function to CredentialRepairer.
type CredentialRepairFunc func(context.Context, string, OpError) error

func (f CredentialRepairFunc) RepairCredentials(
	ctx context.Context,
	accountID string,
	failure OpError,
) error {
	return f(ctx, accountID, failure)
}

// SupervisorOption configures optional account-scoped collaborators.
type SupervisorOption func(*supervisorOptions) error

type supervisorOptions struct {
	sink         ConnectionSink
	repairer     CredentialRepairer
	pairer       Pairer
	initialState State
}

// WithConnectionSink supplies the durable/ephemeral sink behind the
// generation fence. A nil sink is allowed and records only liveness facts.
func WithConnectionSink(sink ConnectionSink) SupervisorOption {
	return func(options *supervisorOptions) error {
		options.sink = sink
		return nil
	}
}

// WithCredentialRepairer supplies the sole atomic credential-repair callback.
func WithCredentialRepairer(repairer CredentialRepairer) SupervisorOption {
	return func(options *supervisorOptions) error {
		options.repairer = repairer
		return nil
	}
}

// WithPairer supplies the sole account pairing implementation.
func WithPairer(pairer Pairer) SupervisorOption {
	return func(options *supervisorOptions) error {
		options.pairer = pairer
		return nil
	}
}

// WithInitialState selects whether an existing account starts intentionally
// stopped or without usable credentials. The default is StateStopped.
func WithInitialState(state State) SupervisorOption {
	return func(options *supervisorOptions) error {
		if state != StateStopped && state != StateUnpaired {
			return fmt.Errorf("%w: %q", ErrInvalidInitialState, state)
		}
		options.initialState = state
		return nil
	}
}

type supervisorCommandKind uint8

const (
	commandStart supervisorCommandKind = iota + 1
	commandRetryBlocked
	commandInputsChanged
	commandBeginPairing
	commandSync
)

type supervisorCommand struct {
	kind        supervisorCommandKind
	start       StartRequest
	pair        PairRequest
	pairSink    PairSink
	fingerprint string
	response    chan error
}

type timerPurpose uint8

const (
	timerNone timerPurpose = iota
	timerConnectDeadline
	timerLivenessCheck
	timerBackoff
	timerProbeDeadline
	timerPairExpiry
)

type activityKind uint8

const (
	activityEvent activityKind = iota + 1
	activityBeat
)

type generationActivity struct {
	generation Generation
	kind       activityKind
	at         time.Time
}

type probeResult struct {
	generation Generation
	liveness   Liveness
	err        error
}

type startResult struct {
	generation Generation
	run        Run
	err        error
}

type repairResult struct {
	epoch uint64
	err   error
}

type pairingResult struct {
	epoch  uint64
	result PairResult
	err    error
}

type pairingEvent struct {
	epoch uint64
	event PairEvent
}

// Supervisor owns exactly one account's lifecycle, retry, repair, and pairing
// decisions. All mutable state is serialized by one run loop.
type Supervisor struct {
	accountID string
	platform  Platform
	lifecycle Lifecycle
	policy    Policy
	clock     Clock
	random    Random
	sink      ConnectionSink
	repairer  CredentialRepairer
	pairer    Pairer

	commands      chan supervisorCommand
	activity      chan generationActivity
	stopRequested chan struct{}
	done          chan struct{}
	stopOnce      sync.Once
	closing       atomic.Bool

	snapshotMu sync.RWMutex
	snapshot   Snapshot

	finalMu  sync.Mutex
	finalErr error

	rootCtx    context.Context
	rootCancel context.CancelFunc
	workers    sync.WaitGroup
	gate       generationFence
}

// NewSupervisor starts an idle, joinable supervisor loop. The caller must call
// Stop; tests should register Stop in cleanup immediately after construction.
func NewSupervisor(
	accountID string,
	platform Platform,
	lifecycle Lifecycle,
	policy Policy,
	clock Clock,
	random Random,
	options ...SupervisorOption,
) (*Supervisor, error) {
	if strings.TrimSpace(accountID) == "" || strings.TrimSpace(accountID) != accountID {
		return nil, fmt.Errorf("%w: %q", ErrInvalidAccountID, accountID)
	}
	if strings.TrimSpace(string(platform)) == "" ||
		strings.TrimSpace(string(platform)) != string(platform) {
		return nil, fmt.Errorf("%w: %q", ErrInvalidPlatform, platform)
	}
	if isNilSupervisorDependency(lifecycle) {
		return nil, ErrLifecycleRequired
	}
	if isNilSupervisorDependency(clock) {
		return nil, ErrClockRequired
	}
	if isNilSupervisorDependency(random) {
		return nil, ErrRandomRequired
	}
	if err := validatePolicy(policy); err != nil {
		return nil, err
	}

	configuration := supervisorOptions{initialState: StateStopped}
	for _, option := range options {
		if option == nil {
			continue
		}
		if err := option(&configuration); err != nil {
			return nil, err
		}
	}
	if configuration.initialState != StateStopped && configuration.initialState != StateUnpaired {
		return nil, fmt.Errorf("%w: %q", ErrInvalidInitialState, configuration.initialState)
	}

	rootCtx, rootCancel := context.WithCancel(context.Background())
	supervisor := &Supervisor{
		accountID:     accountID,
		platform:      platform,
		lifecycle:     lifecycle,
		policy:        policy,
		clock:         clock,
		random:        random,
		sink:          nilIfTypedNil[ConnectionSink](configuration.sink),
		repairer:      nilIfTypedNil[CredentialRepairer](configuration.repairer),
		pairer:        nilIfTypedNil[Pairer](configuration.pairer),
		commands:      make(chan supervisorCommand),
		activity:      make(chan generationActivity, 256),
		stopRequested: make(chan struct{}),
		done:          make(chan struct{}),
		rootCtx:       rootCtx,
		rootCancel:    rootCancel,
		snapshot: Snapshot{
			AccountID: accountID,
			Platform:  platform,
			State:     configuration.initialState,
		},
	}

	go supervisor.run()
	return supervisor, nil
}

// Start explicitly starts a connection. AccountID and Generation are owned by
// the supervisor and are overwritten after a mismatched account is rejected.
func (s *Supervisor) Start(ctx context.Context, request StartRequest) error {
	if request.AccountID != "" && request.AccountID != s.accountID {
		return fmt.Errorf("%w: got %q, want %q", ErrAccountMismatch, request.AccountID, s.accountID)
	}
	request.AccountID = s.accountID
	request.Generation = 0
	return s.sendCommand(ctx, supervisorCommand{kind: commandStart, start: request})
}

// RetryBlocked is an explicit user action that exits StateBlocked. A failed
// credential repair is retried atomically; it is never bypassed by reconnect.
func (s *Supervisor) RetryBlocked(ctx context.Context) error {
	return s.sendCommand(ctx, supervisorCommand{kind: commandRetryBlocked})
}

// InputsChanged exits StateBlocked only when the supplied credential, binary,
// or configuration fingerprint differs from the blocked fingerprint.
func (s *Supervisor) InputsChanged(ctx context.Context, fingerprint string) error {
	return s.sendCommand(ctx, supervisorCommand{
		kind:        commandInputsChanged,
		fingerprint: strings.TrimSpace(fingerprint),
	})
}

// BeginPairing starts one user-initiated, account-scoped pairing attempt. It
// returns after admission; the Pairer continues in a tracked worker.
func (s *Supervisor) BeginPairing(
	ctx context.Context,
	request PairRequest,
	sink PairSink,
) error {
	if request.AccountID != "" && request.AccountID != s.accountID {
		return fmt.Errorf("%w: got %q, want %q", ErrAccountMismatch, request.AccountID, s.accountID)
	}
	request.AccountID = s.accountID
	return s.sendCommand(ctx, supervisorCommand{
		kind:     commandBeginPairing,
		pair:     request,
		pairSink: nilIfTypedNil[PairSink](sink),
	})
}

// Sync waits until the run loop has accepted a command. It is useful for
// deterministic tests after delivering an adapter callback.
func (s *Supervisor) Sync(ctx context.Context) error {
	return s.sendCommand(ctx, supervisorCommand{kind: commandSync})
}

// Snapshot returns an immutable observability copy without exposing the run or
// adapter. It remains available after Stop.
func (s *Supervisor) Snapshot() Snapshot {
	s.snapshotMu.RLock()
	snapshot := s.snapshot
	s.snapshotMu.RUnlock()
	return snapshot
}

// Stop is terminal and idempotent. It cancels every supervisor-owned operation,
// calls Run.Stop at most once for the active generation, joins all workers, and
// returns only after the run loop has exited.
func (s *Supervisor) Stop(ctx context.Context) error {
	if ctx == nil {
		return errors.New("bridge: stop context is nil")
	}
	s.stopOnce.Do(func() {
		s.closing.Store(true)
		close(s.stopRequested)
	})

	select {
	case <-s.done:
		s.finalMu.Lock()
		err := s.finalErr
		s.finalMu.Unlock()
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Supervisor) sendCommand(ctx context.Context, command supervisorCommand) error {
	if ctx == nil {
		return errors.New("bridge: supervisor command context is nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.closing.Load() {
		return ErrSupervisorStopped
	}

	command.response = make(chan error, 1)
	select {
	case s.commands <- command:
	case <-s.done:
		return ErrSupervisorStopped
	case <-ctx.Done():
		return ctx.Err()
	}

	select {
	case err := <-command.response:
		return err
	case <-s.done:
		return ErrSupervisorStopped
	case <-ctx.Done():
		return ctx.Err()
	}
}

type supervisorLoop struct {
	supervisor *Supervisor
	snapshot   Snapshot

	timer         Timer
	timerPurpose  timerPurpose
	timerDeadline time.Time

	startRequest      StartRequest
	startResults      <-chan startResult
	startTimedOut     bool
	pendingStartReply chan error

	run              Run
	runContext       context.Context
	runCancel        context.CancelFunc
	ready            <-chan struct{}
	done             <-chan error
	connectDeadline  time.Time
	livenessBaseline time.Time

	transientAttempt int
	sameFingerprint  string
	sameFailureCount int
	lastFailure      OpError
	repairFailed     bool

	probeResults  <-chan probeResult
	probeCancel   context.CancelFunc
	probeStarted  time.Time
	probeTimedOut bool
	probeFailure  *OpError

	repairEpoch   uint64
	repairResults <-chan repairResult
	repairCancel  context.CancelFunc

	pairEpoch     uint64
	pairActive    bool
	pairExpired   bool
	pairExpiresAt time.Time
	pairResults   <-chan pairingResult
	pairEvents    <-chan pairingEvent
	pairCancel    context.CancelFunc
	pairBoundSink *pairingSink
}

func (s *Supervisor) run() {
	loop := supervisorLoop{supervisor: s, snapshot: s.Snapshot()}
	defer close(s.done)

	for {
		if s.closing.Load() {
			s.setFinalError(loop.shutdown())
			return
		}
		if loop.enforceDueDeadline() {
			continue
		}

		var timerChannel <-chan time.Time
		if loop.timer != nil {
			timerChannel = loop.timer.C()
		}

		select {
		case <-s.stopRequested:
			s.closing.Store(true)
		case command := <-s.commands:
			if s.closing.Load() {
				command.response <- ErrSupervisorStopped
				continue
			}
			if reply := loop.handleCommand(command); reply != errStartReplyDeferred {
				command.response <- reply
			}
		case activity := <-s.activity:
			loop.handleActivity(activity)
		case <-timerChannel:
			// The loop-top deadline comparison makes logical time authoritative
			// and makes stale/buffered timer ticks harmless.
		case _, ok := <-loop.ready:
			if ok || loop.ready != nil {
				loop.handleReady()
			}
		case err, ok := <-loop.done:
			if !ok {
				err = nil
			}
			loop.handleRunDone(err)
		case result := <-loop.startResults:
			loop.handleStartResult(result)
		case result := <-loop.probeResults:
			loop.handleProbeResult(result)
		case result := <-loop.repairResults:
			loop.handleRepairResult(result)
		case event := <-loop.pairEvents:
			loop.handlePairEvent(event)
		case result := <-loop.pairResults:
			loop.handlePairResult(result)
		}
	}
}

func (s *Supervisor) setFinalError(err error) {
	s.finalMu.Lock()
	s.finalErr = err
	s.finalMu.Unlock()
}

func (l *supervisorLoop) handleCommand(command supervisorCommand) error {
	switch command.kind {
	case commandStart:
		if l.pairActive || l.snapshot.State == StatePairing ||
			l.snapshot.State == StateRepairingCredentials ||
			l.snapshot.State == StateConnecting || l.snapshot.State == StateOnline ||
			l.snapshot.State == StateDegraded || l.snapshot.State == StateBackoff ||
			l.snapshot.State == StateStopping {
			return fmt.Errorf("%w: state %q", ErrSupervisorBusy, l.snapshot.State)
		}
		if l.snapshot.State == StateUnpaired {
			return fmt.Errorf("%w: state %q", ErrPairingNotAllowed, l.snapshot.State)
		}
		if l.snapshot.State == StateBlocked {
			return ErrSupervisorBlocked
		}
		l.resetFailureHistory()
		l.startRequest = command.start
		return l.startAndDeferReply(command.response)
	case commandRetryBlocked:
		if l.snapshot.State != StateBlocked {
			return fmt.Errorf("%w: state %q", ErrSupervisorBlocked, l.snapshot.State)
		}
		failure := l.lastFailure
		l.resetFailureHistory()
		if failure.Class == FailureCredentialsExpired {
			l.beginCredentialRepair(failure)
			return nil
		}
		if failure.Operation == "pair" {
			l.snapshot.State = StateUnpaired
			l.publish()
			return nil
		}
		return l.startAndDeferReply(command.response)
	case commandInputsChanged:
		if l.snapshot.State != StateBlocked {
			return fmt.Errorf("%w: state %q", ErrSupervisorBlocked, l.snapshot.State)
		}
		if command.fingerprint == "" || command.fingerprint == l.snapshot.ErrorFingerprint {
			return ErrFingerprintUnchanged
		}
		failure := l.lastFailure
		l.resetFailureHistory()
		if failure.Class == FailureCredentialsExpired {
			l.beginCredentialRepair(failure)
			return nil
		}
		if failure.Operation == "pair" {
			l.snapshot.State = StateUnpaired
			l.publish()
			return nil
		}
		return l.startAndDeferReply(command.response)
	case commandBeginPairing:
		return l.beginPairing(command.pair, command.pairSink)
	case commandSync:
		l.drainReadyWork()
		return nil
	default:
		return errors.New("bridge: unknown supervisor command")
	}
}

func (l *supervisorLoop) startGeneration() error {
	if l.supervisor.closing.Load() {
		return ErrSupervisorStopped
	}
	if l.run != nil || l.startResults != nil {
		return fmt.Errorf("%w: generation %d is still active", ErrSupervisorBusy, l.snapshot.Generation)
	}

	l.snapshot.Generation++
	generation := l.snapshot.Generation
	now := l.supervisor.clock.Now()
	l.snapshot.State = StateConnecting
	l.snapshot.LastEventAt = time.Time{}
	l.snapshot.LastProbeOKAt = time.Time{}
	l.snapshot.LivenessDeadline = time.Time{}
	l.snapshot.RetryAt = time.Time{}
	l.snapshot.ErrorClass = ""
	l.snapshot.ErrorFingerprint = ""
	l.connectDeadline = now.Add(l.supervisor.policy.ConnectTimeout)
	l.livenessBaseline = now
	l.replaceTimer(timerConnectDeadline, l.connectDeadline)
	l.publish()

	l.startRequest.AccountID = l.supervisor.accountID
	l.startRequest.Generation = generation
	l.runContext, l.runCancel = context.WithCancel(l.supervisor.rootCtx)
	l.supervisor.gate.activate(generation)
	sink := &generationSink{
		supervisor: l.supervisor,
		generation: generation,
	}

	results := make(chan startResult, 1)
	l.startResults = results
	l.startTimedOut = false
	lifecycle := l.supervisor.lifecycle
	startContext := l.runContext
	request := l.startRequest
	l.supervisor.workers.Add(1)
	go func() {
		defer l.supervisor.workers.Done()
		run, err := callLifecycleStart(lifecycle, startContext, request, sink)
		if startContext.Err() != nil && !isNilSupervisorDependency(run) {
			stopErr := callRunStop(run, context.Background())
			run = nil
			if err == nil {
				err = startContext.Err()
			}
			if stopErr != nil {
				err = errors.Join(err, stopErr)
			}
		}
		results <- startResult{generation: generation, run: run, err: err}
	}()
	return nil
}

func (l *supervisorLoop) startAndDeferReply(response chan error) error {
	if err := l.startGeneration(); err != nil {
		return err
	}
	l.pendingStartReply = response
	return errStartReplyDeferred
}

// resolvePendingStart delivers the first-attempt outcome to a deferred
// Start/RetryBlocked/InputsChanged caller exactly once. The response channel
// is buffered (cap 1), so the send never blocks the run loop.
func (l *supervisorLoop) resolvePendingStart(err error) {
	if l.pendingStartReply != nil {
		l.pendingStartReply <- err
		l.pendingStartReply = nil
	}
}

func (l *supervisorLoop) handleStartResult(result startResult) {
	if l.startResults == nil || result.generation != l.snapshot.Generation {
		if !isNilSupervisorDependency(result.run) {
			_ = callRunStop(result.run, context.Background())
		}
		return
	}
	timedOut := l.startTimedOut
	l.startResults = nil
	l.startTimedOut = false

	if timedOut {
		if !isNilSupervisorDependency(result.run) {
			_ = callRunStop(result.run, context.Background())
		}
		l.clearGenerationContext()
		failure := connectTimeoutFailure()
		l.routeFailure(failure)
		l.resolvePendingStart(failure)
		return
	}
	if result.err != nil {
		if !isNilSupervisorDependency(result.run) {
			l.installRun(result.run)
		}
		l.clearTimer()
		l.retireCurrent()
		failure := classifyFailure(result.err, "start", "start_failed")
		l.routeFailure(failure)
		l.resolvePendingStart(failure)
		return
	}
	if isNilSupervisorDependency(result.run) {
		l.clearTimer()
		l.clearGenerationContext()
		failure := OpError{
			Class:       FailureMisconfigured,
			Operation:   "start",
			Fingerprint: "lifecycle_nil_run",
			Cause:       errors.New("lifecycle returned a nil run"),
		}
		l.routeFailure(failure)
		l.resolvePendingStart(failure)
		return
	}

	l.installRun(result.run)
	if !l.supervisor.clock.Now().Before(l.connectDeadline) {
		l.handleConnectTimeout()
		l.resolvePendingStart(connectTimeoutFailure())
		return
	}
	l.resolvePendingStart(nil)
}

func (l *supervisorLoop) installRun(run Run) {
	l.run = run
	l.ready = run.Ready()
	l.done = run.Done()
}

func (l *supervisorLoop) handleReady() {
	if l.run == nil || l.snapshot.State != StateConnecting {
		l.ready = nil
		return
	}

	// A terminal completion or hard connect deadline wins over readiness at
	// the same logical instant.
	if l.done != nil {
		select {
		case err, ok := <-l.done:
			if !ok {
				err = nil
			}
			l.handleRunDone(err)
			return
		default:
		}
	}
	if !l.supervisor.clock.Now().Before(l.connectDeadline) {
		l.handleConnectTimeout()
		return
	}

	l.ready = nil
	l.clearTimer()
	l.livenessBaseline = l.supervisor.clock.Now()
	l.snapshot.State = StateOnline
	l.snapshot.RetryAt = time.Time{}
	l.snapshot.ErrorClass = ""
	l.snapshot.ErrorFingerprint = ""
	l.refreshLivenessDeadline()
	l.scheduleLivenessCheck()
	l.publish()
}

func (l *supervisorLoop) handleRunDone(err error) {
	if l.run == nil {
		l.done = nil
		return
	}
	if l.snapshot.State == StateConnecting &&
		!l.supervisor.clock.Now().Before(l.connectDeadline) {
		l.handleConnectTimeout()
		return
	}

	l.done = nil
	l.clearTimer()
	l.retireCurrent()
	if err == nil {
		err = errors.New("connection generation completed unexpectedly")
	}
	l.routeFailure(classifyFailure(err, "run", "run_completed"))
}

func (l *supervisorLoop) handleConnectTimeout() {
	if l.snapshot.State != StateConnecting {
		return
	}
	l.clearTimer()
	if l.startResults != nil {
		l.startTimedOut = true
		if l.runCancel != nil {
			l.runCancel()
		}
		l.supervisor.gate.deactivate(l.snapshot.Generation)
		return
	}
	if l.run == nil {
		return
	}
	l.retireCurrent()
	l.routeFailure(connectTimeoutFailure())
}

func connectTimeoutFailure() OpError {
	return OpError{
		Class:       FailureTransient,
		Operation:   "connect",
		Fingerprint: "connect_timeout",
		Cause:       errors.New("connection readiness deadline exceeded"),
	}
}

func (l *supervisorLoop) handleActivity(activity generationActivity) {
	if activity.generation != l.snapshot.Generation || l.run == nil {
		return
	}
	now := l.supervisor.clock.Now()
	at := clampActivityTime(activity.at, now)
	if activity.kind == activityEvent {
		if at.After(l.snapshot.LastEventAt) {
			l.snapshot.LastEventAt = at
		}
	} else if activity.kind == activityBeat {
		if at.After(l.snapshot.LastProbeOKAt) {
			l.snapshot.LastProbeOKAt = at
		}
	}
	l.resetFailureHistory()
	if l.snapshot.State == StateOnline {
		l.refreshLivenessDeadline()
		l.scheduleLivenessCheck()
	}
	l.publish()
}

func (l *supervisorLoop) refreshLivenessDeadline() {
	baseline := l.livenessBaseline
	if l.snapshot.LastEventAt.After(baseline) {
		baseline = l.snapshot.LastEventAt
	}
	if l.snapshot.LastProbeOKAt.After(baseline) {
		baseline = l.snapshot.LastProbeOKAt
	}
	l.snapshot.LivenessDeadline = baseline.Add(l.supervisor.policy.LivenessTimeout)
}

func (l *supervisorLoop) scheduleLivenessCheck() {
	if l.snapshot.State != StateOnline {
		return
	}
	now := l.supervisor.clock.Now()
	next := now.Add(l.supervisor.policy.ProbeEvery)
	if l.snapshot.LivenessDeadline.Before(next) {
		next = l.snapshot.LivenessDeadline
	}
	l.replaceTimer(timerLivenessCheck, next)
}

func (l *supervisorLoop) beginProbe() {
	if l.run == nil || l.snapshot.State != StateOnline || l.probeResults != nil {
		return
	}
	l.clearTimer()
	l.snapshot.State = StateDegraded

	generation := l.snapshot.Generation
	l.probeStarted = l.supervisor.clock.Now()
	l.probeTimedOut = false
	probeContext, cancel := context.WithCancel(l.runContext)
	l.probeCancel = cancel
	results := make(chan probeResult, 1)
	l.probeResults = results
	run := l.run
	l.supervisor.workers.Add(1)
	go func() {
		defer l.supervisor.workers.Done()
		liveness, err := callRunProbe(run, probeContext)
		results <- probeResult{generation: generation, liveness: liveness, err: err}
	}()
	l.replaceTimer(timerProbeDeadline, l.probeStarted.Add(l.supervisor.policy.ProbeTimeout))
	l.publish()
}

func (l *supervisorLoop) handleProbeResult(result probeResult) {
	if l.probeResults == nil || result.generation != l.snapshot.Generation {
		return
	}
	timedOut := l.probeTimedOut
	l.probeResults = nil
	if l.probeCancel != nil {
		l.probeCancel()
		l.probeCancel = nil
	}
	l.probeTimedOut = false
	l.clearTimer()

	if timedOut {
		l.retireCurrent()
		l.routeFailure(OpError{
			Class:       FailureTransient,
			Operation:   "probe",
			Fingerprint: "probe_timeout",
			Cause:       errors.New("liveness probe deadline exceeded"),
		})
		return
	}
	if result.err != nil {
		l.retireCurrent()
		l.routeFailure(classifyFailure(result.err, "probe", "probe_failed"))
		return
	}
	if l.run == nil || l.snapshot.State != StateDegraded {
		return
	}

	now := l.supervisor.clock.Now()
	aliveAt := clampActivityTime(result.liveness.AliveAt, now)
	if aliveAt.Before(l.probeStarted) {
		aliveAt = l.probeStarted
	}
	if aliveAt.After(l.snapshot.LastProbeOKAt) {
		l.snapshot.LastProbeOKAt = aliveAt
	}
	l.resetFailureHistory()
	l.snapshot.State = StateOnline
	l.refreshLivenessDeadline()
	l.publish()
	l.scheduleLivenessCheck()
}

func (l *supervisorLoop) beginCredentialRepair(failure OpError) {
	l.clearTimer()
	l.snapshot.State = StateRepairingCredentials
	l.snapshot.RetryAt = time.Time{}
	l.snapshot.ErrorClass = failure.Class
	l.snapshot.ErrorFingerprint = failure.Fingerprint
	l.lastFailure = failure
	l.repairFailed = false
	l.publish()

	if l.supervisor.repairer == nil {
		l.repairFailed = true
		l.snapshot.State = StateBlocked
		l.publish()
		return
	}

	l.repairEpoch++
	epoch := l.repairEpoch
	ctx, cancel := context.WithCancel(l.supervisor.rootCtx)
	l.repairCancel = cancel
	results := make(chan repairResult, 1)
	l.repairResults = results
	repairer := l.supervisor.repairer
	accountID := l.supervisor.accountID
	l.supervisor.workers.Add(1)
	go func() {
		defer l.supervisor.workers.Done()
		err := callCredentialRepair(repairer, ctx, accountID, failure)
		results <- repairResult{epoch: epoch, err: err}
	}()
}

func (l *supervisorLoop) handleRepairResult(result repairResult) {
	if l.repairResults == nil || result.epoch != l.repairEpoch {
		return
	}
	l.repairResults = nil
	if l.repairCancel != nil {
		l.repairCancel()
		l.repairCancel = nil
	}
	if l.snapshot.State != StateRepairingCredentials {
		return
	}
	if result.err != nil {
		l.repairFailed = true
		l.snapshot.State = StateBlocked
		l.publish()
		return
	}

	l.repairFailed = false
	_ = l.startGeneration()
}

func (l *supervisorLoop) beginPairing(request PairRequest, sink PairSink) error {
	if l.supervisor.pairer == nil {
		return ErrPairerUnavailable
	}
	if l.pairActive {
		return ErrPairingInProgress
	}
	if l.snapshot.State != StateUnpaired {
		return fmt.Errorf("%w: state %q", ErrPairingNotAllowed, l.snapshot.State)
	}
	now := l.supervisor.clock.Now()
	if !l.snapshot.RetryAt.IsZero() && now.Before(l.snapshot.RetryAt) {
		return fmt.Errorf("%w until %s", ErrPairingRateLimited, l.snapshot.RetryAt.Format(time.RFC3339Nano))
	}

	l.clearTimer()
	l.snapshot.State = StatePairing
	l.snapshot.RetryAt = time.Time{}
	l.snapshot.ErrorClass = ""
	l.snapshot.ErrorFingerprint = ""
	l.publish()

	l.pairEpoch++
	epoch := l.pairEpoch
	l.pairActive = true
	l.pairExpired = false
	l.pairExpiresAt = time.Time{}
	ctx, cancel := context.WithCancel(l.supervisor.rootCtx)
	l.pairCancel = cancel
	results := make(chan pairingResult, 1)
	events := make(chan pairingEvent, 32)
	l.pairResults = results
	l.pairEvents = events
	pairer := l.supervisor.pairer
	boundSink := &pairingSink{epoch: epoch, active: true, events: events, downstream: sink}
	l.pairBoundSink = boundSink
	l.supervisor.workers.Add(1)
	go func() {
		defer l.supervisor.workers.Done()
		result, err := callPair(pairer, ctx, request, boundSink)
		boundSink.close()
		results <- pairingResult{epoch: epoch, result: result, err: err}
	}()
	return nil
}

func (l *supervisorLoop) handlePairEvent(event pairingEvent) {
	if !l.pairActive || event.epoch != l.pairEpoch || l.snapshot.State != StatePairing {
		return
	}

	if event.event.Kind == "blocked" {
		l.clearTimer()
		l.pairExpired = true
		l.pairExpiresAt = time.Time{}
		if l.pairBoundSink != nil {
			l.pairBoundSink.close()
		}
		if l.pairCancel != nil {
			l.pairCancel()
		}
		l.snapshot.State = StateUnpaired
		l.snapshot.RetryAt = event.event.ExpiresAt
		l.snapshot.ErrorClass = FailureRateLimited
		l.snapshot.ErrorFingerprint = strings.TrimSpace(event.event.Message)
		l.publish()
		return
	}

	if (event.event.Kind == "qr" || event.event.Kind == "code") && !event.event.ExpiresAt.IsZero() {
		l.pairExpiresAt = event.event.ExpiresAt
		if !l.supervisor.clock.Now().Before(l.pairExpiresAt) {
			l.expirePairing()
			return
		}
		l.replaceTimer(timerPairExpiry, l.pairExpiresAt)
	}
}

func (l *supervisorLoop) expirePairing() {
	if !l.pairActive || l.snapshot.State != StatePairing {
		return
	}
	l.clearTimer()
	l.pairExpired = true
	if l.pairBoundSink != nil {
		l.pairBoundSink.close()
	}
	if l.pairCancel != nil {
		l.pairCancel()
	}
	l.snapshot.State = StateUnpaired
	l.snapshot.RetryAt = time.Time{}
	l.snapshot.ErrorClass = ""
	l.snapshot.ErrorFingerprint = ""
	l.publish()
}

func (l *supervisorLoop) handlePairResult(result pairingResult) {
	if !l.pairActive || result.epoch != l.pairEpoch {
		return
	}
	if !l.pairExpiresAt.IsZero() && !l.supervisor.clock.Now().Before(l.pairExpiresAt) {
		l.expirePairing()
	}

	l.pairActive = false
	l.pairResults = nil
	l.pairEvents = nil
	l.pairBoundSink = nil
	if l.pairCancel != nil {
		l.pairCancel()
		l.pairCancel = nil
	}
	l.clearTimer()
	if l.pairExpired {
		if l.snapshot.State == StatePairing {
			l.snapshot.State = StateUnpaired
			l.publish()
		}
		return
	}

	if result.err == nil {
		l.startRequest.RemoteAccountID = result.result.RemoteAccountID
		l.startRequest.DeviceID = result.result.RemoteDeviceID
		l.snapshot.State = StateStopped
		l.snapshot.RetryAt = time.Time{}
		l.snapshot.ErrorClass = ""
		l.snapshot.ErrorFingerprint = ""
		l.publish()
		return
	}

	failure := classifyFailure(result.err, "pair", "pair_failed")
	l.lastFailure = failure
	l.snapshot.ErrorClass = failure.Class
	l.snapshot.ErrorFingerprint = failure.Fingerprint
	l.snapshot.RetryAt = time.Time{}
	switch failure.Class {
	case FailureRateLimited:
		l.snapshot.State = StateUnpaired
		l.snapshot.RetryAt = failure.RetryAt
	case FailureReauthRequired, FailureUpgradeRequired, FailureMisconfigured, FailureUnsupported:
		l.snapshot.State = StateBlocked
	default:
		l.snapshot.State = StateUnpaired
	}
	l.publish()
}

func (l *supervisorLoop) routeFailure(failure OpError) {
	switch failure.Class {
	case FailureTransient, FailureRateLimited, FailureCredentialsExpired, FailureUnpaired,
		FailureReauthRequired, FailureUpgradeRequired, FailureMisconfigured,
		FailureUnsupported:
	default:
		failure.Class = FailureTransient
	}
	l.lastFailure = failure
	l.snapshot.ErrorClass = failure.Class
	l.snapshot.ErrorFingerprint = failure.Fingerprint
	l.snapshot.RetryAt = time.Time{}
	l.snapshot.LivenessDeadline = time.Time{}
	if failure.Class == FailureUnpaired {
		// A lifecycle can discover that no linked account exists (notably an
		// expired QR attempt). This is intentionally idle until an explicit
		// user action; it is neither reconnect backoff nor a blocked circuit.
		l.resetFailureHistory()
		l.lastFailure = failure
		l.snapshot.ErrorClass = failure.Class
		l.snapshot.ErrorFingerprint = failure.Fingerprint
		l.snapshot.State = StateUnpaired
		l.publish()
		return
	}

	if l.recordFingerprint(failure.Class, failure.Fingerprint) {
		l.snapshot.State = StateBlocked
		l.publish()
		return
	}

	switch failure.Class {
	case FailureTransient:
		l.transientAttempt++
		cap := backoffCap(
			l.supervisor.policy.MinBackoff,
			l.supervisor.policy.MaxBackoff,
			l.transientAttempt,
		)
		delay, err := l.drawJitter(cap)
		if err != nil {
			l.snapshot.State = StateBlocked
			l.snapshot.ErrorClass = FailureMisconfigured
			l.snapshot.ErrorFingerprint = "invalid_jitter_source"
			l.publish()
			return
		}
		l.snapshot.State = StateBackoff
		l.snapshot.RetryAt = l.supervisor.clock.Now().Add(delay)
		l.replaceTimer(timerBackoff, l.snapshot.RetryAt)
		l.publish()
	case FailureRateLimited:
		l.snapshot.State = StateBackoff
		l.snapshot.RetryAt = failure.RetryAt
		deadline := failure.RetryAt
		if deadline.Before(l.supervisor.clock.Now()) {
			deadline = l.supervisor.clock.Now()
		}
		l.replaceTimer(timerBackoff, deadline)
		l.publish()
	case FailureCredentialsExpired:
		l.beginCredentialRepair(failure)
	case FailureReauthRequired, FailureUpgradeRequired, FailureMisconfigured, FailureUnsupported:
		l.snapshot.State = StateBlocked
		l.publish()
	}
}

func (l *supervisorLoop) recordFingerprint(class FailureClass, fingerprint string) bool {
	// Connectivity failures are paced by bounded exponential backoff and must
	// remain recoverable without user action, even when every attempt reports
	// the same transport fingerprint. The circuit remains available for
	// repeated non-transient failures such as an ineffective credential repair.
	if class == FailureTransient {
		return false
	}
	if fingerprint == "" {
		l.sameFingerprint = ""
		l.sameFailureCount = 0
		return false
	}
	if fingerprint == l.sameFingerprint {
		l.sameFailureCount++
	} else {
		l.sameFingerprint = fingerprint
		l.sameFailureCount = 1
	}
	return l.sameFailureCount >= l.supervisor.policy.MaxSameFingerprint
}

func (l *supervisorLoop) resetFailureHistory() {
	l.transientAttempt = 0
	l.sameFingerprint = ""
	l.sameFailureCount = 0
	l.repairFailed = false
	l.lastFailure = OpError{}
	l.snapshot.RetryAt = time.Time{}
	l.snapshot.ErrorClass = ""
	l.snapshot.ErrorFingerprint = ""
}

func (l *supervisorLoop) drawJitter(cap time.Duration) (delay time.Duration, err error) {
	if cap <= 0 {
		return 0, fmt.Errorf("%w: backoff cap %s", ErrInvalidPolicy, cap)
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("random source panicked: %v", recovered)
		}
	}()
	value := l.supervisor.random.Int63n(int64(cap))
	if value < 0 || value >= int64(cap) {
		return 0, fmt.Errorf("random source returned %d for upper bound %d", value, cap)
	}
	return time.Duration(value), nil
}

func (l *supervisorLoop) enforceDueDeadline() bool {
	if l.timerPurpose == timerNone || l.timerDeadline.IsZero() ||
		l.supervisor.clock.Now().Before(l.timerDeadline) {
		return false
	}

	purpose := l.timerPurpose
	l.clearTimer()
	switch purpose {
	case timerConnectDeadline:
		l.handleConnectTimeout()
	case timerLivenessCheck:
		if l.snapshot.State != StateOnline {
			return true
		}
		if !l.supervisor.clock.Now().Before(l.snapshot.LivenessDeadline) {
			l.beginProbe()
		} else {
			l.scheduleLivenessCheck()
		}
	case timerBackoff:
		if l.snapshot.State == StateBackoff && !l.supervisor.closing.Load() {
			_ = l.startGeneration()
		}
	case timerProbeDeadline:
		if l.probeResults != nil && l.snapshot.State == StateDegraded {
			l.probeTimedOut = true
			if l.probeCancel != nil {
				l.probeCancel()
			}
		}
	case timerPairExpiry:
		if l.pairActive && !l.pairExpiresAt.IsZero() &&
			!l.supervisor.clock.Now().Before(l.pairExpiresAt) {
			l.expirePairing()
		}
	}
	return true
}

// drainReadyWork makes Sync a barrier for callbacks and timer deadlines that
// were already ready before the Sync command was accepted. It never waits for
// future adapter work.
func (l *supervisorLoop) drainReadyWork() {
	for {
		if l.enforceDueDeadline() {
			continue
		}
		select {
		case activity := <-l.supervisor.activity:
			l.handleActivity(activity)
		case err, ok := <-l.done:
			if !ok {
				err = nil
			}
			l.handleRunDone(err)
		case <-l.ready:
			l.handleReady()
		case result := <-l.probeResults:
			l.handleProbeResult(result)
		case result := <-l.repairResults:
			l.handleRepairResult(result)
		case event := <-l.pairEvents:
			l.handlePairEvent(event)
		case result := <-l.pairResults:
			l.handlePairResult(result)
		default:
			return
		}
	}
}

func (l *supervisorLoop) replaceTimer(purpose timerPurpose, deadline time.Time) {
	l.clearTimer()
	delay := deadline.Sub(l.supervisor.clock.Now())
	if delay < 0 {
		delay = 0
	}
	l.timer = l.supervisor.clock.NewTimer(delay)
	l.timerPurpose = purpose
	l.timerDeadline = deadline
}

func (l *supervisorLoop) clearTimer() {
	if l.timer != nil {
		l.timer.Stop()
	}
	l.timer = nil
	l.timerPurpose = timerNone
	l.timerDeadline = time.Time{}
}

func (l *supervisorLoop) retireCurrent() error {
	if l.probeCancel != nil {
		l.probeCancel()
		l.probeCancel = nil
	}
	l.probeResults = nil
	l.probeFailure = nil
	l.probeTimedOut = false
	return l.retireCurrentPreservingProbe()
}

func (l *supervisorLoop) retireCurrentPreservingProbe() error {
	l.clearGenerationContext()
	run := l.run
	l.run = nil
	if run == nil {
		return nil
	}
	return callRunStop(run, context.Background())
}

func (l *supervisorLoop) clearGenerationContext() {
	generation := l.snapshot.Generation
	if l.runCancel != nil {
		l.runCancel()
		l.runCancel = nil
	}
	l.supervisor.gate.deactivate(generation)
	l.ready = nil
	l.done = nil
	l.runContext = nil
}

func (l *supervisorLoop) shutdown() error {
	l.clearTimer()
	l.snapshot.State = StateStopping
	l.snapshot.LivenessDeadline = time.Time{}
	l.snapshot.RetryAt = time.Time{}
	l.publish()

	if l.probeCancel != nil {
		l.probeCancel()
		l.probeCancel = nil
	}
	if l.repairCancel != nil {
		l.repairCancel()
		l.repairCancel = nil
	}
	if l.pairCancel != nil {
		l.pairCancel()
		l.pairCancel = nil
	}
	if l.pairBoundSink != nil {
		l.pairBoundSink.close()
		l.pairBoundSink = nil
	}
	l.supervisor.rootCancel()
	runStopErr := l.retireCurrent()
	l.supervisor.workers.Wait()
	l.resolvePendingStart(ErrSupervisorStopped)

	l.probeResults = nil
	l.repairResults = nil
	l.pairResults = nil
	l.pairEvents = nil
	l.pairActive = false
	l.snapshot.State = StateStopped
	l.snapshot.LivenessDeadline = time.Time{}
	l.snapshot.RetryAt = time.Time{}
	l.publish()
	return runStopErr
}

func (l *supervisorLoop) publish() {
	l.supervisor.snapshotMu.Lock()
	l.supervisor.snapshot = l.snapshot
	l.supervisor.snapshotMu.Unlock()
}

type generationFence struct {
	mu         sync.RWMutex
	generation Generation
	active     bool
}

func (f *generationFence) activate(generation Generation) {
	f.mu.Lock()
	f.generation = generation
	f.active = true
	f.mu.Unlock()
}

func (f *generationFence) deactivate(generation Generation) {
	f.mu.Lock()
	if f.generation == generation {
		f.active = false
	}
	f.mu.Unlock()
}

type generationSink struct {
	supervisor *Supervisor
	generation Generation
}

func (s *generationSink) AppendIngress(ctx context.Context, record RawIngressRecord) error {
	if record.AccountID != s.supervisor.accountID || record.Generation != s.generation {
		return ErrStaleGeneration
	}
	return s.forward(ctx, func(sink ConnectionSink) error {
		return sink.AppendIngress(ctx, record)
	}, activityEvent, s.supervisor.clock.Now())
}

func (s *generationSink) EmitEphemeral(ctx context.Context, event EphemeralEvent) error {
	if event.AccountID != s.supervisor.accountID || event.Generation != s.generation {
		return ErrStaleGeneration
	}
	return s.forward(ctx, func(sink ConnectionSink) error {
		return sink.EmitEphemeral(ctx, event)
	}, activityEvent, s.supervisor.clock.Now())
}

func (s *generationSink) Beat(generation Generation, aliveAt time.Time, _ string) {
	if generation != s.generation {
		return
	}
	fence := &s.supervisor.gate
	fence.mu.RLock()
	current := fence.active && fence.generation == s.generation
	fence.mu.RUnlock()
	if !current {
		return
	}
	s.supervisor.enqueueActivity(generationActivity{
		generation: s.generation,
		kind:       activityBeat,
		at:         aliveAt,
	})
}

func (s *generationSink) forward(
	ctx context.Context,
	forward func(ConnectionSink) error,
	kind activityKind,
	at time.Time,
) error {
	if ctx == nil {
		return errors.New("bridge: connection sink context is nil")
	}
	fence := &s.supervisor.gate
	fence.mu.RLock()
	if !fence.active || fence.generation != s.generation {
		fence.mu.RUnlock()
		return ErrStaleGeneration
	}
	var err error
	if s.supervisor.sink != nil {
		err = forward(s.supervisor.sink)
	}
	fence.mu.RUnlock()
	if err != nil {
		return err
	}
	s.supervisor.enqueueActivity(generationActivity{
		generation: s.generation,
		kind:       kind,
		at:         at,
	})
	return nil
}

func (s *Supervisor) enqueueActivity(activity generationActivity) {
	select {
	case s.activity <- activity:
	default:
		// A full queue already contains many recent positive-liveness facts.
		// Never block an adapter callback that Run.Stop may be joining.
	}
}

type pairingSink struct {
	mu         sync.RWMutex
	epoch      uint64
	active     bool
	events     chan<- pairingEvent
	downstream PairSink
}

func (s *pairingSink) EmitPairEvent(ctx context.Context, event PairEvent) error {
	if ctx == nil {
		return errors.New("bridge: pair sink context is nil")
	}
	s.mu.RLock()
	if !s.active {
		s.mu.RUnlock()
		return ErrPairingNotAllowed
	}
	if s.downstream != nil {
		if err := s.downstream.EmitPairEvent(ctx, event); err != nil {
			s.mu.RUnlock()
			return err
		}
	}
	select {
	case s.events <- pairingEvent{epoch: s.epoch, event: event}:
		s.mu.RUnlock()
		return nil
	default:
		s.mu.RUnlock()
		return fmt.Errorf("%w: pairing event queue is full", ErrSupervisorBusy)
	}
}

func (s *pairingSink) close() {
	s.mu.Lock()
	s.active = false
	s.mu.Unlock()
}

func validatePolicy(policy Policy) error {
	if policy.ConnectTimeout <= 0 {
		return fmt.Errorf("%w: ConnectTimeout must be positive", ErrInvalidPolicy)
	}
	if policy.ProbeEvery <= 0 {
		return fmt.Errorf("%w: ProbeEvery must be positive", ErrInvalidPolicy)
	}
	if policy.ProbeTimeout <= 0 {
		return fmt.Errorf("%w: ProbeTimeout must be positive", ErrInvalidPolicy)
	}
	if policy.LivenessTimeout <= 0 {
		return fmt.Errorf("%w: LivenessTimeout must be positive", ErrInvalidPolicy)
	}
	if policy.MinBackoff <= 0 {
		return fmt.Errorf("%w: MinBackoff must be positive", ErrInvalidPolicy)
	}
	if policy.MaxBackoff < policy.MinBackoff {
		return fmt.Errorf("%w: MaxBackoff must be at least MinBackoff", ErrInvalidPolicy)
	}
	if policy.MaxSameFingerprint <= 0 {
		return fmt.Errorf("%w: MaxSameFingerprint must be positive", ErrInvalidPolicy)
	}
	return nil
}

func backoffCap(minimum, maximum time.Duration, attempt int) time.Duration {
	cap := minimum
	for i := 1; i < attempt && cap < maximum; i++ {
		if cap > maximum/2 {
			return maximum
		}
		cap *= 2
	}
	if cap > maximum {
		return maximum
	}
	return cap
}

func classifyFailure(err error, operation, fallbackFingerprint string) OpError {
	failure := OpError{}
	var pointer *OpError
	if errors.As(err, &pointer) && pointer != nil {
		failure = *pointer
	} else {
		var value OpError
		if errors.As(err, &value) {
			failure = value
		}
	}
	if failure.Class == "" {
		failure.Class = FailureTransient
	}
	if failure.Operation == "" {
		failure.Operation = operation
	}
	if failure.Fingerprint == "" {
		failure.Fingerprint = fallbackFingerprint
	}
	if failure.Cause == nil {
		failure.Cause = err
	}
	return failure
}

func clampActivityTime(at, now time.Time) time.Time {
	if at.IsZero() || at.After(now) {
		return now
	}
	return at
}

func isNilSupervisorDependency(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

func nilIfTypedNil[T any](value T) T {
	if isNilSupervisorDependency(value) {
		var zero T
		return zero
	}
	return value
}

func callLifecycleStart(
	lifecycle Lifecycle,
	ctx context.Context,
	request StartRequest,
	sink ConnectionSink,
) (run Run, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("lifecycle start panic: %v", recovered)
		}
	}()
	return lifecycle.Start(ctx, request, sink)
}

func callRunProbe(run Run, ctx context.Context) (liveness Liveness, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("run probe panic: %v", recovered)
		}
	}()
	return run.Probe(ctx)
}

func callRunStop(run Run, ctx context.Context) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("run stop panic: %v", recovered)
		}
	}()
	return run.Stop(ctx)
}

func callCredentialRepair(
	repairer CredentialRepairer,
	ctx context.Context,
	accountID string,
	failure OpError,
) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("credential repair panic: %v", recovered)
		}
	}()
	return repairer.RepairCredentials(ctx, accountID, failure)
}

func callPair(
	pairer Pairer,
	ctx context.Context,
	request PairRequest,
	sink PairSink,
) (result PairResult, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("pair panic: %v", recovered)
		}
	}()
	return pairer.Pair(ctx, request, sink)
}

var _ ConnectionSink = (*generationSink)(nil)
var _ PairSink = (*pairingSink)(nil)
