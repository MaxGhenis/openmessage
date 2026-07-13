package signal

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/signallive"
)

const signalAdapterTestTimeout = 2 * time.Second

var signalAdapterTestEpoch = time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)

func TestRunTranslatesRetainedPollerSignals(t *testing.T) {
	poller := newFakePoller()
	adapter := &Adapter{accountID: "signal-primary", poller: poller}
	sink := &recordingSink{beats: make(chan recordedBeat, 1)}

	run, err := adapter.Start(context.Background(), bridge.StartRequest{
		AccountID:  "signal-primary",
		Generation: 7,
	}, sink)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { stopAdapterRun(t, run) })
	retained := receiveValue(t, poller.started, "retained poller start")

	assertChannelOpen(t, run.Ready(), "before retained poller readiness")
	retained.markReady()
	select {
	case <-run.Ready():
	case <-time.After(signalAdapterTestTimeout):
		t.Fatal("adapter Ready did not follow retained poller Ready")
	}

	activity := signallive.PollerActivity{
		At:     time.Now().Add(time.Hour),
		Detail: "receive_batch",
	}
	retained.emitActivity(activity)
	beat := receiveValue(t, sink.beats, "retained poller activity beat")
	if beat.generation != 7 || !beat.at.Equal(activity.At) || beat.detail != activity.Detail {
		t.Fatalf("Beat = %+v, want generation 7 and activity %+v", beat, activity)
	}

	probeContext, probeCancel := context.WithTimeout(context.Background(), signalAdapterTestTimeout)
	defer probeCancel()
	liveness, err := run.Probe(probeContext)
	if err != nil {
		t.Fatalf("Probe() error = %v", err)
	}
	if !liveness.AliveAt.Equal(activity.At) || liveness.Detail != activity.Detail {
		t.Fatalf("Probe() = %+v, want activity %+v", liveness, activity)
	}

	retained.complete(signallive.PollerExit{
		Kind:        signallive.PollerFailureTransient,
		Operation:   "receive",
		Fingerprint: signallive.SignalReceiveFailureFingerprint,
		Err:         errors.New("receive failed repeatedly"),
	})
	terminal := receiveValue(t, run.Done(), "adapter terminal result")
	assertOpError(t, terminal, bridge.FailureTransient, signallive.SignalReceiveFailureFingerprint)
	if _, ok := <-run.Done(); ok {
		t.Fatal("adapter Done remained open after its one terminal result")
	}
	if got := retained.stopCount(); got != 1 {
		t.Fatalf("retained poller join calls after natural exit = %d, want 1", got)
	}
}

func TestAdapterStartIsSingleFlight(t *testing.T) {
	poller := newFakePoller()
	poller.startEntered = make(chan struct{}, 1)
	poller.startRelease = make(chan struct{})
	adapter := &Adapter{accountID: "signal-primary", poller: poller}

	type startResult struct {
		run bridge.Run
		err error
	}
	firstResult := make(chan startResult, 1)
	go func() {
		run, err := adapter.Start(context.Background(), bridge.StartRequest{
			AccountID:  "signal-primary",
			Generation: 1,
		}, nil)
		firstResult <- startResult{run: run, err: err}
	}()
	receiveValue(t, poller.startEntered, "first StartPoller admission")

	second, err := adapter.Start(context.Background(), bridge.StartRequest{
		AccountID:  "signal-primary",
		Generation: 2,
	}, nil)
	if second != nil {
		t.Fatalf("overlapping Start() run = %T, want nil", second)
	}
	assertOpError(t, err, bridge.FailureMisconfigured, "overlapping_signal_generation")
	if got := poller.startCount(); got != 1 {
		t.Fatalf("StartPoller calls while first start is admitted = %d, want 1", got)
	}

	close(poller.startRelease)
	first := receiveValue(t, firstResult, "first adapter Start result")
	if first.err != nil {
		t.Fatalf("first Start() error = %v", first.err)
	}
	if first.run == nil {
		t.Fatal("first Start() run = nil")
	}
	stopAdapterRun(t, first.run)
}

func TestAdapterStartDoesNotWaitForReady(t *testing.T) {
	poller := newFakePoller()
	adapter := &Adapter{accountID: "signal-primary", poller: poller}

	returned := make(chan bridge.Run, 1)
	errorsReturned := make(chan error, 1)
	go func() {
		run, err := adapter.Start(context.Background(), bridge.StartRequest{
			AccountID:  "signal-primary",
			Generation: 1,
		}, nil)
		if err != nil {
			errorsReturned <- err
			return
		}
		returned <- run
	}()

	var run bridge.Run
	select {
	case err := <-errorsReturned:
		t.Fatalf("Start() error = %v", err)
	case run = <-returned:
	case <-time.After(signalAdapterTestTimeout):
		t.Fatal("Start blocked waiting for retained poller Ready")
	}
	assertChannelOpen(t, run.Ready(), "after non-blocking Start return")
	stopAdapterRun(t, run)
}

func TestPollerTerminalOutranksRacingTransientSendFailure(t *testing.T) {
	poller := newFakePoller()
	adapter := &Adapter{accountID: "signal-primary", poller: poller}
	run, err := adapter.Start(context.Background(), bridge.StartRequest{
		AccountID:  "signal-primary",
		Generation: 1,
	}, nil)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	retained := receiveValue(t, poller.started, "retained poller start")
	retained.setStopExit(signallive.PollerExit{
		Kind:        signallive.PollerFailureUpgrade,
		Operation:   "receive",
		Fingerprint: "incoming_message_get_sender_content_null",
		Err:         errors.New("signal-cli poison envelope"),
	})

	if reported := adapter.ReportError(signallive.NewCommandError("send failed: authorization failed")); !reported {
		t.Fatal("ReportError() rejected an admitted local-account send failure")
	}
	terminal := receiveValue(t, run.Done(), "arbitrated terminal result")
	assertOpError(
		t,
		terminal,
		bridge.FailureUpgradeRequired,
		"incoming_message_get_sender_content_null",
	)
	if got := poller.appliedCount(); got != 0 {
		t.Fatalf("send failure status projections after poller terminal won = %d, want 0", got)
	}
}

func TestStrongestAlreadyAdmittedSendFailureWins(t *testing.T) {
	poller := newFakePoller()
	adapter := &Adapter{accountID: "signal-primary", poller: poller}
	started, err := adapter.Start(context.Background(), bridge.StartRequest{
		AccountID:  "signal-primary",
		Generation: 1,
	}, nil)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	receiveValue(t, poller.started, "retained poller start")
	r := started.(*run)
	if !r.admitCallback() || !r.admitCallback() {
		t.Fatal("failed to admit send callbacks before terminal selection")
	}
	transientExit := signallive.PollerExit{
		Kind:        signallive.PollerFailureTransient,
		Operation:   "send",
		Fingerprint: "signal_send_failed",
		Err:         errors.New("temporary send failure"),
	}
	reauthExit := signallive.PollerExit{
		Kind:        signallive.PollerFailureReauth,
		Operation:   "send",
		Fingerprint: signallive.SignalAccountInvalidFingerprint,
		Err:         errors.New("account is not registered"),
	}
	r.requestFinish(terminalCandidate{
		err:      classifyPollerExit(transientExit),
		sendExit: &transientExit,
	})
	r.requestFinish(terminalCandidate{
		err:      classifyPollerExit(reauthExit),
		sendExit: &reauthExit,
	})
	r.callbacks.Done()
	r.callbacks.Done()

	terminal := receiveValue(t, started.Done(), "strongest send terminal result")
	assertOpError(
		t,
		terminal,
		bridge.FailureReauthRequired,
		signallive.SignalAccountInvalidFingerprint,
	)
	if got := poller.appliedCount(); got != 1 {
		t.Fatalf("winning send status projections = %d, want 1", got)
	}
}

// A send can fail for many reasons that do NOT indict the local Signal account:
// an unregistered *recipient* (whose "not registered" text is identical to an
// unregistered local account), an untrusted-identity change, a group-permission
// denial, a rate/proof challenge, or a plain network blip. None of these may
// retire the healthy receive generation — the account-scoped receive probe owns
// account validity. Regression for the C6 D1/D2 send-classification defect.
func TestReportErrorIgnoresRecipientAndNonAccountSendFailures(t *testing.T) {
	cases := []struct {
		name string
		text string
	}{
		{"unregistered recipient", "send failed: Untrusted or not registered: user +15551230000 is not registered"},
		{"untrusted identity", "send failed: Untrusted identity key for +15551230000"},
		{"group permission", "send failed: User is not authorized to send to this group"},
		{"rate limited", "send failed: Rate limit challenge required (proof of work)"},
		{"network blip", "send failed: Connection reset by peer"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			poller := newFakePoller()
			adapter := &Adapter{accountID: "signal-primary", poller: poller}
			run, err := adapter.Start(context.Background(), bridge.StartRequest{
				AccountID:  "signal-primary",
				Generation: 1,
			}, nil)
			if err != nil {
				t.Fatalf("Start() error = %v", err)
			}
			receiveValue(t, poller.started, "retained poller start")
			t.Cleanup(func() { stopAdapterRun(t, run) })

			if reported := adapter.ReportError(signallive.NewCommandError(tc.text)); reported {
				t.Fatalf("ReportError(%q) acted on a non-account send failure", tc.text)
			}
			select {
			case terminal := <-run.Done():
				t.Fatalf("ignored send failure produced terminal %v", terminal)
			default:
			}
			if got := poller.appliedCount(); got != 0 {
				t.Fatalf("status projections after ignored send failure = %d, want 0", got)
			}
		})
	}
}

// An unambiguous local-account send failure ("authorization failed" / "invalid
// account") is worth a nudge, but only as a *transient* bounce — never reauth.
// The next generation's account-scoped probe, not this send's text, is what
// parks reauth, so a send can never by itself drive the bridge to Blocked.
func TestReportErrorTransientlyReportsLocalAccountSendFailure(t *testing.T) {
	for _, text := range []string{
		"send failed: authorization failed",
		"send failed: invalid account",
	} {
		t.Run(text, func(t *testing.T) {
			poller := newFakePoller()
			adapter := &Adapter{accountID: "signal-primary", poller: poller}
			run, err := adapter.Start(context.Background(), bridge.StartRequest{
				AccountID:  "signal-primary",
				Generation: 1,
			}, nil)
			if err != nil {
				t.Fatalf("Start() error = %v", err)
			}
			receiveValue(t, poller.started, "retained poller start")
			t.Cleanup(func() { stopAdapterRun(t, run) })

			if reported := adapter.ReportError(signallive.NewCommandError(text)); !reported {
				t.Fatalf("ReportError(%q) ignored a local-account send failure", text)
			}
			// FailureTransient + signal_send_account_check IS the proof it is not
			// reauth/signal_account_invalid — the probe, not the send, owns reauth.
			terminal := receiveValue(t, run.Done(), "local-account send terminal")
			assertOpError(t, terminal, bridge.FailureTransient, "signal_send_account_check")
		})
	}
}

// End to end: texting a number that is not on Signal must not disturb the live
// receive loop. The supervised generation stays Online — no reconnect, no
// projection, and never Blocked or needs-reauth.
func TestSupervisorKeepsReceivingThroughUnregisteredRecipientSend(t *testing.T) {
	poller := newFakePoller()
	adapter := &Adapter{accountID: "signal-primary", poller: poller}
	clock := newManualClock(signalAdapterTestEpoch)
	supervisor := newSignalTestSupervisor(t, adapter, signalSupervisorTestPolicy(), clock)

	startSupervisor(t, supervisor)
	readyNextGeneration(t, supervisor, poller, 1)

	if reported := adapter.ReportError(signallive.NewCommandError(
		"send failed: user +15551230000 is not registered",
	)); reported {
		t.Fatal("ReportError() acted on an unregistered-recipient send failure")
	}

	syncSupervisor(t, supervisor)
	if got := poller.startCount(); got != 1 {
		t.Fatalf("StartPoller calls after recipient send failure = %d, want 1", got)
	}
	if got := poller.appliedCount(); got != 0 {
		t.Fatalf("status projections after recipient send failure = %d, want 0", got)
	}
	snapshot := supervisor.Snapshot()
	if snapshot.State != bridge.StateOnline || snapshot.Generation != 1 {
		t.Fatalf("snapshot after recipient send failure = %+v, want online generation 1", snapshot)
	}
	if snapshot.ErrorClass == bridge.FailureReauthRequired ||
		snapshot.ErrorFingerprint == signallive.SignalAccountInvalidFingerprint {
		t.Fatalf("recipient send failure produced reauth: %+v", snapshot)
	}
}

func TestSupervisorReceiveFailureRetriesBeyondFingerprintThreshold(t *testing.T) {
	poller := newFakePoller()
	adapter := &Adapter{accountID: "signal-primary", poller: poller}
	clock := newManualClock(signalAdapterTestEpoch)
	policy := signalSupervisorTestPolicy()
	policy.MaxSameFingerprint = 2
	supervisor := newSignalTestSupervisor(t, adapter, policy, clock)

	startSupervisor(t, supervisor)
	retained := readyNextGeneration(t, supervisor, poller, 1)
	for generation := bridge.Generation(1); generation <= 3; generation++ {
		retained.complete(signallive.PollerExit{
			Kind:        signallive.PollerFailureTransient,
			Operation:   "receive",
			Fingerprint: signallive.SignalReceiveFailureFingerprint,
			Err:         errors.New("signal-cli receive failed repeatedly"),
		})
		backoff := awaitSnapshot(t, supervisor, "transient receive backoff", func(snapshot bridge.Snapshot) bool {
			return snapshot.Generation == generation && snapshot.State == bridge.StateBackoff
		})
		if backoff.ErrorClass != bridge.FailureTransient ||
			backoff.ErrorFingerprint != signallive.SignalReceiveFailureFingerprint {
			t.Fatalf("generation %d backoff = %+v", generation, backoff)
		}
		clock.Advance(backoff.RetryAt.Sub(clock.Now()))
		retained = readyNextGeneration(t, supervisor, poller, generation+1)
	}

	if got := poller.startCount(); got != 4 {
		t.Fatalf("StartPoller calls after three identical transient failures = %d, want 4", got)
	}
	if snapshot := supervisor.Snapshot(); snapshot.State != bridge.StateOnline || snapshot.Generation != 4 {
		t.Fatalf("snapshot after transient retries = %+v, want online generation 4", snapshot)
	}
}

func TestSupervisorParksTerminalSignalFailuresWithoutReconnectChurn(t *testing.T) {
	tests := []struct {
		name        string
		exit        signallive.PollerExit
		wantClass   bridge.FailureClass
		fingerprint string
	}{
		{
			name: "account invalid requires re-pair",
			exit: signallive.PollerExit{
				Kind:        signallive.PollerFailureReauth,
				Operation:   "probe_account",
				Fingerprint: signallive.SignalAccountInvalidFingerprint,
				Err:         errors.New("account is not registered"),
			},
			wantClass:   bridge.FailureReauthRequired,
			fingerprint: signallive.SignalAccountInvalidFingerprint,
		},
		{
			name: "old signal-cli version",
			exit: signallive.PollerExit{
				Kind:        signallive.PollerFailureUpgrade,
				Operation:   "version_gate",
				Fingerprint: signallive.SignalCLIVersionFingerprint,
				Err:         errors.New("signal-cli is below the minimum version"),
			},
			wantClass:   bridge.FailureUpgradeRequired,
			fingerprint: signallive.SignalCLIVersionFingerprint,
		},
		{
			name: "getSender poison circuit",
			exit: signallive.PollerExit{
				Kind:        signallive.PollerFailureUpgrade,
				Operation:   "receive",
				Fingerprint: "incoming_message_get_sender_content_null",
				Err:         errors.New("IncomingMessageHandler.getSender content is null"),
			},
			wantClass:   bridge.FailureUpgradeRequired,
			fingerprint: "incoming_message_get_sender_content_null",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			poller := newFakePoller()
			adapter := &Adapter{accountID: "signal-primary", poller: poller}
			clock := newManualClock(signalAdapterTestEpoch)
			supervisor := newSignalTestSupervisor(t, adapter, signalSupervisorTestPolicy(), clock)

			startSupervisor(t, supervisor)
			retained := readyNextGeneration(t, supervisor, poller, 1)
			retained.complete(test.exit)
			blocked := awaitSnapshot(t, supervisor, "blocked terminal failure", func(snapshot bridge.Snapshot) bool {
				return snapshot.State == bridge.StateBlocked
			})
			if blocked.ErrorClass != test.wantClass || blocked.ErrorFingerprint != test.fingerprint {
				t.Fatalf("blocked snapshot = %+v, want class %q fingerprint %q", blocked, test.wantClass, test.fingerprint)
			}

			clock.Advance(7 * 24 * time.Hour)
			syncSupervisor(t, supervisor)
			if got := poller.startCount(); got != 1 {
				t.Fatalf("StartPoller calls after a week parked = %d, want 1", got)
			}
			if snapshot := supervisor.Snapshot(); snapshot.State != bridge.StateBlocked || snapshot.Generation != 1 {
				t.Fatalf("snapshot after parked clock advance = %+v", snapshot)
			}
		})
	}
}

func TestSupervisorLeavesExpiredPairingUnpairedWithoutReconnectChurn(t *testing.T) {
	poller := newFakePoller()
	adapter := &Adapter{accountID: "signal-primary", poller: poller}
	clock := newManualClock(signalAdapterTestEpoch)
	supervisor := newSignalTestSupervisor(t, adapter, signalSupervisorTestPolicy(), clock)

	startSupervisor(t, supervisor)
	retained := receiveValue(t, poller.started, "pairing poller start")
	retained.complete(signallive.PollerExit{
		Kind:        signallive.PollerFailureUnpaired,
		Operation:   "pair",
		Fingerprint: signallive.SignalPairingIncompleteFingerprint,
		Err:         errors.New("Signal link QR expired"),
	})
	unpaired := awaitSnapshot(t, supervisor, "unpaired pairing expiry", func(snapshot bridge.Snapshot) bool {
		return snapshot.State == bridge.StateUnpaired
	})
	if unpaired.ErrorClass != bridge.FailureUnpaired ||
		unpaired.ErrorFingerprint != signallive.SignalPairingIncompleteFingerprint ||
		!unpaired.RetryAt.IsZero() {
		t.Fatalf("unpaired snapshot = %+v", unpaired)
	}

	clock.Advance(7 * 24 * time.Hour)
	syncSupervisor(t, supervisor)
	if got := poller.startCount(); got != 1 {
		t.Fatalf("StartPoller calls after a week unpaired = %d, want 1", got)
	}
	if snapshot := supervisor.Snapshot(); snapshot.State != bridge.StateUnpaired {
		t.Fatalf("snapshot after unpaired clock advance = %+v", snapshot)
	}
}

type fakePoller struct {
	mu           sync.Mutex
	starts       int
	runs         []*fakePollerRun
	started      chan *fakePollerRun
	startEntered chan struct{}
	startRelease chan struct{}
	applied      []signallive.PollerExit
	fingerprint  string
}

func newFakePoller() *fakePoller {
	return &fakePoller{started: make(chan *fakePollerRun, 16)}
}

func (p *fakePoller) StartPoller(ctx context.Context) (signallive.PollerRun, error) {
	p.mu.Lock()
	p.starts++
	entered := p.startEntered
	release := p.startRelease
	p.mu.Unlock()
	if entered != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
	}
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	run := newFakePollerRun()
	p.mu.Lock()
	p.runs = append(p.runs, run)
	p.mu.Unlock()
	select {
	case p.started <- run:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return run, nil
}

func (*fakePoller) Status() signallive.StatusSnapshot { return signallive.StatusSnapshot{} }

func (p *fakePoller) ApplyPollerFailure(exit signallive.PollerExit) {
	p.mu.Lock()
	p.applied = append(p.applied, exit)
	p.mu.Unlock()
}

func (p *fakePoller) appliedCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.applied)
}

func (p *fakePoller) InputFingerprint() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.fingerprint
}

func (*fakePoller) UnpairContext(context.Context) error { return nil }

func (p *fakePoller) startCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.starts
}

type fakePollerRun struct {
	ready      chan struct{}
	activity   chan signallive.PollerActivity
	done       chan signallive.PollerExit
	stopped    chan struct{}
	readyOnce  sync.Once
	finishOnce sync.Once
	mu         sync.Mutex
	stops      int
	stopExit   signallive.PollerExit
}

func newFakePollerRun() *fakePollerRun {
	return &fakePollerRun{
		ready:    make(chan struct{}),
		activity: make(chan signallive.PollerActivity, 4),
		done:     make(chan signallive.PollerExit, 1),
		stopped:  make(chan struct{}),
	}
}

func (r *fakePollerRun) Ready() <-chan struct{} { return r.ready }

func (r *fakePollerRun) Activity() <-chan signallive.PollerActivity { return r.activity }

func (r *fakePollerRun) Done() <-chan signallive.PollerExit { return r.done }

func (r *fakePollerRun) Stop(ctx context.Context) error {
	r.mu.Lock()
	r.stops++
	exit := r.stopExit
	r.mu.Unlock()
	r.complete(exit)
	select {
	case <-r.stopped:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *fakePollerRun) setStopExit(exit signallive.PollerExit) {
	r.mu.Lock()
	r.stopExit = exit
	r.mu.Unlock()
}

func (r *fakePollerRun) markReady() {
	r.readyOnce.Do(func() { close(r.ready) })
}

func (r *fakePollerRun) emitActivity(activity signallive.PollerActivity) {
	r.activity <- activity
}

func (r *fakePollerRun) complete(exit signallive.PollerExit) {
	r.finishOnce.Do(func() {
		r.done <- exit
		close(r.done)
		close(r.activity)
		close(r.stopped)
	})
}

func (r *fakePollerRun) stopCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stops
}

type recordedBeat struct {
	generation bridge.Generation
	at         time.Time
	detail     string
}

type recordingSink struct {
	beats chan recordedBeat
}

func (*recordingSink) AppendIngress(context.Context, bridge.RawIngressRecord) error { return nil }

func (*recordingSink) EmitEphemeral(context.Context, bridge.EphemeralEvent) error { return nil }

func (s *recordingSink) Beat(generation bridge.Generation, at time.Time, detail string) {
	s.beats <- recordedBeat{generation: generation, at: at, detail: detail}
}

type manualClock struct {
	mu     sync.Mutex
	now    time.Time
	timers map[*manualTimer]struct{}
}

func newManualClock(now time.Time) *manualClock {
	return &manualClock{now: now, timers: make(map[*manualTimer]struct{})}
}

func (c *manualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *manualClock) NewTimer(delay time.Duration) bridge.Timer {
	if delay < 0 {
		delay = 0
	}
	c.mu.Lock()
	timer := &manualTimer{
		clock:    c,
		deadline: c.now.Add(delay),
		channel:  make(chan time.Time, 1),
		active:   delay != 0,
	}
	if timer.active {
		c.timers[timer] = struct{}{}
	}
	now := c.now
	c.mu.Unlock()
	if delay == 0 {
		timer.channel <- now
	}
	return timer
}

func (c *manualClock) Advance(delta time.Duration) {
	if delta < 0 {
		panic("manual clock cannot move backward")
	}
	c.mu.Lock()
	c.now = c.now.Add(delta)
	now := c.now
	var due []*manualTimer
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

type manualTimer struct {
	clock    *manualClock
	deadline time.Time
	channel  chan time.Time
	active   bool
}

func (t *manualTimer) C() <-chan time.Time { return t.channel }

func (t *manualTimer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	if !t.active {
		return false
	}
	t.active = false
	delete(t.clock.timers, t)
	return true
}

type midpointRandom struct{}

func (midpointRandom) Int63n(bound int64) int64 { return bound / 2 }

func signalSupervisorTestPolicy() bridge.Policy {
	return bridge.Policy{
		ConnectTimeout:     time.Hour,
		ProbeEvery:         time.Hour,
		ProbeTimeout:       time.Hour,
		LivenessTimeout:    time.Hour,
		MinBackoff:         10 * time.Second,
		MaxBackoff:         time.Minute,
		MaxSameFingerprint: 3,
	}
}

func newSignalTestSupervisor(
	t *testing.T,
	adapter *Adapter,
	policy bridge.Policy,
	clock *manualClock,
) *bridge.Supervisor {
	t.Helper()
	supervisor, err := bridge.NewSupervisor(
		"signal-primary",
		bridge.PlatformSignal,
		adapter,
		policy,
		clock,
		midpointRandom{},
	)
	if err != nil {
		t.Fatalf("NewSupervisor() error = %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), signalAdapterTestTimeout)
		defer cancel()
		if err := supervisor.Stop(ctx); err != nil {
			t.Errorf("Supervisor.Stop() cleanup error = %v", err)
		}
	})
	return supervisor
}

func startSupervisor(t *testing.T, supervisor *bridge.Supervisor) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), signalAdapterTestTimeout)
	defer cancel()
	if err := supervisor.Start(ctx, bridge.StartRequest{}); err != nil {
		t.Fatalf("Supervisor.Start() error = %v", err)
	}
}

func syncSupervisor(t *testing.T, supervisor *bridge.Supervisor) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), signalAdapterTestTimeout)
	defer cancel()
	if err := supervisor.Sync(ctx); err != nil {
		t.Fatalf("Supervisor.Sync() error = %v", err)
	}
}

func readyNextGeneration(
	t *testing.T,
	supervisor *bridge.Supervisor,
	poller *fakePoller,
	generation bridge.Generation,
) *fakePollerRun {
	t.Helper()
	retained := receiveValue(t, poller.started, "retained poller generation start")
	retained.markReady()
	awaitSnapshot(t, supervisor, "online generation", func(snapshot bridge.Snapshot) bool {
		return snapshot.State == bridge.StateOnline && snapshot.Generation == generation
	})
	return retained
}

func awaitSnapshot(
	t *testing.T,
	supervisor *bridge.Supervisor,
	description string,
	predicate func(bridge.Snapshot) bool,
) bridge.Snapshot {
	t.Helper()
	deadline := time.NewTimer(signalAdapterTestTimeout)
	defer deadline.Stop()
	for {
		snapshot := supervisor.Snapshot()
		if predicate(snapshot) {
			return snapshot
		}
		select {
		case <-deadline.C:
			t.Fatalf("timed out waiting for %s; last snapshot: %+v", description, snapshot)
		default:
			runtime.Gosched()
		}
	}
}

func assertChannelOpen(t *testing.T, channel <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-channel:
		t.Fatalf("channel closed %s", description)
	default:
	}
}

func assertOpError(
	t *testing.T,
	err error,
	wantClass bridge.FailureClass,
	wantFingerprint string,
) {
	t.Helper()
	var operationError bridge.OpError
	if !errors.As(err, &operationError) {
		t.Fatalf("error = %v (%T), want bridge.OpError", err, err)
	}
	if operationError.Class != wantClass || operationError.Fingerprint != wantFingerprint {
		t.Fatalf("OpError = %+v, want class %q fingerprint %q", operationError, wantClass, wantFingerprint)
	}
}

func stopAdapterRun(t *testing.T, run bridge.Run) {
	t.Helper()
	if run == nil {
		return
	}
	select {
	case <-run.Done():
		return
	default:
	}
	ctx, cancel := context.WithTimeout(context.Background(), signalAdapterTestTimeout)
	defer cancel()
	if err := run.Stop(ctx); err != nil {
		t.Fatalf("Run.Stop() error = %v", err)
	}
}

func receiveValue[T any](t *testing.T, channel <-chan T, description string) T {
	t.Helper()
	select {
	case value := <-channel:
		return value
	case <-time.After(signalAdapterTestTimeout):
		var zero T
		t.Fatalf("timed out waiting for %s", description)
		return zero
	}
}

var (
	_ poller                = (*fakePoller)(nil)
	_ signallive.PollerRun  = (*fakePollerRun)(nil)
	_ bridge.ConnectionSink = (*recordingSink)(nil)
	_ bridge.Clock          = (*manualClock)(nil)
	_ bridge.Timer          = (*manualTimer)(nil)
	_ bridge.Random         = midpointRandom{}
)
