package whatsapp

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	waevents "go.mau.fi/whatsmeow/types/events"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/whatsapplive"
)

const whatsappAdapterTestTimeout = 2 * time.Second

var whatsappAdapterTestEpoch = time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)

func TestEventFailureClassification(t *testing.T) {
	a := &Adapter{}
	retryAt := whatsappAdapterTestEpoch.Add(30 * time.Minute)

	tests := []struct {
		name        string
		event       whatsapplive.LifecycleEvent
		terminal    bool
		wantClass   bridge.FailureClass
		wantRetryAt time.Time
	}{
		{
			name: "connected is non-terminal",
			event: whatsapplive.LifecycleEvent{
				Raw:    &waevents.Connected{},
				Paired: true,
				At:     whatsappAdapterTestEpoch,
			},
			terminal: false,
		},
		{
			name: "disconnected is transient",
			event: whatsapplive.LifecycleEvent{
				Raw: &waevents.Disconnected{},
				At:  whatsappAdapterTestEpoch,
			},
			terminal:  true,
			wantClass: bridge.FailureTransient,
		},
		{
			name: "unknown connect failure is transient",
			event: whatsapplive.LifecycleEvent{
				Raw: &waevents.ConnectFailure{
					Reason:  waevents.ConnectFailureGeneric,
					Message: "unknown connection failure",
				},
				At: whatsappAdapterTestEpoch,
			},
			terminal:  true,
			wantClass: bridge.FailureTransient,
		},
		{
			name: "logged out requires reauth",
			event: whatsapplive.LifecycleEvent{
				Raw: &waevents.LoggedOut{
					OnConnect: true,
					Reason:    waevents.ConnectFailureLoggedOut,
				},
				At: whatsappAdapterTestEpoch,
			},
			terminal:  true,
			wantClass: bridge.FailureReauthRequired,
		},
		{
			name: "temporary ban uses supplied retry deadline",
			event: whatsapplive.LifecycleEvent{
				Raw: &waevents.TemporaryBan{
					Code:   waevents.TempBanSentTooManySameMessage,
					Expire: 30 * time.Minute,
				},
				At:      whatsappAdapterTestEpoch,
				RetryAt: retryAt,
			},
			terminal:    true,
			wantClass:   bridge.FailureRateLimited,
			wantRetryAt: retryAt,
		},
		{
			name: "temporary ban derives retry deadline from expiry",
			event: whatsapplive.LifecycleEvent{
				Raw: &waevents.TemporaryBan{
					Code:   waevents.TempBanSentTooManySameMessage,
					Expire: 30 * time.Minute,
				},
				At: whatsappAdapterTestEpoch,
			},
			terminal:    true,
			wantClass:   bridge.FailureRateLimited,
			wantRetryAt: retryAt,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			failure, terminal := a.classifyEvent(test.event)
			if terminal != test.terminal {
				t.Fatalf("terminal = %v, want %v", terminal, test.terminal)
			}
			if failure.Class != test.wantClass {
				t.Fatalf("class = %q, want %q", failure.Class, test.wantClass)
			}
			if !failure.RetryAt.Equal(test.wantRetryAt) {
				t.Fatalf("RetryAt = %s, want %s", failure.RetryAt, test.wantRetryAt)
			}
		})
	}

	t.Run("reported temporary operation error is transient", func(t *testing.T) {
		failure := a.classifyError(
			errors.New("temporary send failure"),
			"operation",
			"whatsapp_operation_failed",
		)
		if failure.Class != bridge.FailureTransient {
			t.Fatalf("class = %q, want %q", failure.Class, bridge.FailureTransient)
		}
	})
}

func TestReadyRequiresRealPairedConnectedEvent(t *testing.T) {
	client := newFakeLifecycleClient()
	a := newTestAdapter(t, client, func() time.Time { return whatsappAdapterTestEpoch })
	sink := &recordingSink{}

	lifecycleRun, err := a.Start(context.Background(), bridge.StartRequest{
		AccountID:  "whatsapp-primary",
		Generation: 7,
	}, sink)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { stopRun(t, lifecycleRun) })

	awaitCondition(t, "successful fake Connect return", func() bool {
		return client.stats().connectCompleted == 1
	})
	concreteRun, ok := lifecycleRun.(*run)
	if !ok {
		t.Fatalf("Start() Run type = %T, want *run", lifecycleRun)
	}
	concreteRun.workers.Wait()
	assertNotClosed(t, lifecycleRun.Ready(), "successful Connect return and paired account boolean")

	client.emit(a, whatsapplive.LifecycleEvent{
		Raw:    &waevents.Connected{},
		Paired: false,
		At:     whatsappAdapterTestEpoch,
	})
	assertNotClosed(t, lifecycleRun.Ready(), "unpaired Connected pairing transport event")
	if got := sink.beatsSnapshot(); len(got) != 0 {
		t.Fatalf("liveness beats after unpaired Connected = %+v, want none", got)
	}

	connectedAt := whatsappAdapterTestEpoch.Add(time.Second)
	client.emit(a, whatsapplive.LifecycleEvent{
		Raw:    &waevents.Connected{},
		Paired: true,
		At:     connectedAt,
	})
	select {
	case <-lifecycleRun.Ready():
	case <-time.After(whatsappAdapterTestTimeout):
		t.Fatal("Ready did not close after paired Connected event")
	}

	beats := sink.beatsSnapshot()
	if len(beats) != 1 {
		t.Fatalf("liveness beats = %+v, want exactly one", beats)
	}
	if beats[0].generation != 7 || !beats[0].at.Equal(connectedAt) ||
		beats[0].detail != "event:whatsapp.connected" {
		t.Fatalf("Connected beat = %+v, want generation 7 at %s", beats[0], connectedAt)
	}
}

func TestLifecycleEventsDriveSupervisorState(t *testing.T) {
	tests := []struct {
		name        string
		event       func(time.Time) whatsapplive.LifecycleEvent
		wantState   bridge.State
		wantClass   bridge.FailureClass
		wantRetryAt func(time.Time) time.Time
	}{
		{
			name: "paired Connected becomes online",
			event: func(at time.Time) whatsapplive.LifecycleEvent {
				return whatsapplive.LifecycleEvent{
					Raw:    &waevents.Connected{},
					Paired: true,
					At:     at,
				}
			},
			wantState: bridge.StateOnline,
		},
		{
			name: "Disconnected enters transient backoff",
			event: func(at time.Time) whatsapplive.LifecycleEvent {
				return whatsapplive.LifecycleEvent{Raw: &waevents.Disconnected{}, Paired: true, At: at}
			},
			wantState: bridge.StateBackoff,
			wantClass: bridge.FailureTransient,
		},
		{
			name: "unknown connect failure enters transient backoff",
			event: func(at time.Time) whatsapplive.LifecycleEvent {
				return whatsapplive.LifecycleEvent{
					Raw: &waevents.ConnectFailure{
						Reason:  waevents.ConnectFailureGeneric,
						Message: "unknown connection failure",
					},
					Paired: true,
					At:     at,
				}
			},
			wantState: bridge.StateBackoff,
			wantClass: bridge.FailureTransient,
		},
		{
			name: "LoggedOut blocks for reauth",
			event: func(at time.Time) whatsapplive.LifecycleEvent {
				return whatsapplive.LifecycleEvent{
					Raw: &waevents.LoggedOut{
						OnConnect: true,
						Reason:    waevents.ConnectFailureLoggedOut,
					},
					At: at,
				}
			},
			wantState: bridge.StateBlocked,
			wantClass: bridge.FailureReauthRequired,
		},
		{
			name: "TemporaryBan enters rate-limited backoff",
			event: func(at time.Time) whatsapplive.LifecycleEvent {
				return whatsapplive.LifecycleEvent{
					Raw: &waevents.TemporaryBan{
						Code:   waevents.TempBanSentTooManySameMessage,
						Expire: 30 * time.Minute,
					},
					Paired:  true,
					At:      at,
					RetryAt: at.Add(30 * time.Minute),
				}
			},
			wantState: bridge.StateBackoff,
			wantClass: bridge.FailureRateLimited,
			wantRetryAt: func(at time.Time) time.Time {
				return at.Add(30 * time.Minute)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clock := newManualClock(whatsappAdapterTestEpoch)
			client := newFakeLifecycleClient()
			a := newTestAdapter(t, client, clock.Now)
			supervisor := newTestSupervisor(t, a, clock)

			startSupervisor(t, supervisor)
			awaitCondition(t, "first WhatsApp connect", func() bool {
				return client.stats().connectCompleted == 1
			})
			at := clock.Now()
			client.emit(a, test.event(at))

			snapshot := awaitSnapshot(t, supervisor, test.name, func(snapshot bridge.Snapshot) bool {
				if snapshot.State != test.wantState || snapshot.ErrorClass != test.wantClass {
					return false
				}
				if test.wantState == bridge.StateOnline {
					return snapshot.LastProbeOKAt.Equal(at)
				}
				return true
			})
			if test.wantRetryAt != nil {
				want := test.wantRetryAt(at)
				if !snapshot.RetryAt.Equal(want) {
					t.Fatalf("RetryAt = %s, want %s", snapshot.RetryAt, want)
				}
			}
		})
	}
}

func TestReportedTemporaryErrorDrivesSupervisorBackoff(t *testing.T) {
	clock := newManualClock(whatsappAdapterTestEpoch)
	client := newFakeLifecycleClient()
	a := newTestAdapter(t, client, clock.Now)
	supervisor := newTestSupervisor(t, a, clock)

	startSupervisor(t, supervisor)
	awaitCondition(t, "initial WhatsApp connect", func() bool {
		return client.stats().connectCompleted == 1
	})
	client.emit(a, whatsapplive.LifecycleEvent{
		Raw:    &waevents.Connected{},
		Paired: true,
		At:     clock.Now(),
	})
	awaitSnapshot(t, supervisor, "online before reported error", func(snapshot bridge.Snapshot) bool {
		return snapshot.State == bridge.StateOnline
	})

	if !a.ReportError(errors.New("temporary legacy send error")) {
		t.Fatal("ReportError() did not retire the active generation")
	}
	snapshot := awaitSnapshot(t, supervisor, "reported error backoff", func(snapshot bridge.Snapshot) bool {
		return snapshot.State == bridge.StateBackoff && snapshot.ErrorClass == bridge.FailureTransient
	})
	if snapshot.ErrorFingerprint != "whatsapp_operation_failed" {
		t.Fatalf("error fingerprint = %q, want whatsapp_operation_failed", snapshot.ErrorFingerprint)
	}
}

func TestReportedErrorIgnoredUntilCurrentRunIsReady(t *testing.T) {
	clock := newManualClock(whatsappAdapterTestEpoch)
	client := newFakeLifecycleClient()
	a := newTestAdapter(t, client, clock.Now)
	supervisor := newTestSupervisor(t, a, clock)

	startSupervisor(t, supervisor)
	awaitCondition(t, "initial WhatsApp connect", func() bool {
		return client.stats().connectCompleted == 1
	})
	awaitSnapshot(t, supervisor, "connecting generation before Ready", func(snapshot bridge.Snapshot) bool {
		return snapshot.State == bridge.StateConnecting && snapshot.Generation == 1
	})

	if a.ReportError(whatsmeow.ErrNotConnected) {
		t.Fatal("ReportError() retired the connecting generation before Ready")
	}
	syncSupervisor(t, supervisor)
	snapshot := supervisor.Snapshot()
	if snapshot.State != bridge.StateConnecting || snapshot.Generation != 1 {
		t.Fatalf("snapshot after pre-Ready report = %+v, want connecting generation 1", snapshot)
	}
	if snapshot.ErrorClass != "" || snapshot.ErrorFingerprint != "" || !snapshot.RetryAt.IsZero() {
		t.Fatalf("failure state after pre-Ready report = %+v, want no recorded failure or retry", snapshot)
	}

	client.emit(a, whatsapplive.LifecycleEvent{
		Raw:    &waevents.Connected{},
		Paired: true,
		At:     clock.Now(),
	})
	awaitSnapshot(t, supervisor, "online current generation", func(snapshot bridge.Snapshot) bool {
		return snapshot.State == bridge.StateOnline && snapshot.Generation == 1
	})

	if !a.ReportError(whatsmeow.ErrNotConnected) {
		t.Fatal("ReportError() ignored the ready generation")
	}
	snapshot = awaitSnapshot(t, supervisor, "first reported-error backoff", func(snapshot bridge.Snapshot) bool {
		return snapshot.State == bridge.StateBackoff &&
			snapshot.Generation == 1 &&
			snapshot.ErrorClass == bridge.FailureTransient
	})
	wantRetryAt := clock.Now().Add(5 * time.Second)
	if !snapshot.RetryAt.Equal(wantRetryAt) {
		t.Fatalf("RetryAt = %s, want first-attempt retry at %s", snapshot.RetryAt, wantRetryAt)
	}
}

func TestAdapterCooldownRejectsStartsUntilRetryAt(t *testing.T) {
	clock := newManualClock(whatsappAdapterTestEpoch)
	client := newFakeLifecycleClient()
	a := newTestAdapter(t, client, clock.Now)
	retryAt := clock.Now().Add(30 * time.Second)
	client.emit(a, whatsapplive.LifecycleEvent{
		Raw: &waevents.TemporaryBan{
			Code:   waevents.TempBanSentTooManySameMessage,
			Expire: 30 * time.Second,
		},
		Paired:  true,
		At:      clock.Now(),
		RetryAt: retryAt,
	})

	for attempt, advance := range []time.Duration{0, 29 * time.Second} {
		clock.Advance(advance)
		lifecycleRun, err := a.Start(context.Background(), bridge.StartRequest{
			AccountID:  "whatsapp-primary",
			Generation: bridge.Generation(attempt + 1),
		}, nil)
		if lifecycleRun != nil {
			stopRun(t, lifecycleRun)
			t.Fatalf("pre-expiry Start %d returned a Run", attempt+1)
		}
		failure, ok := asOpError(err)
		if !ok || failure.Class != bridge.FailureRateLimited || !failure.RetryAt.Equal(retryAt) {
			t.Fatalf("pre-expiry Start %d error = %v, want rate limit until %s", attempt+1, err, retryAt)
		}
		if got := client.stats().connectCalls; got != 0 {
			t.Fatalf("connect calls before RetryAt = %d, want zero", got)
		}
	}

	clock.Advance(time.Second)
	lifecycleRun, err := a.Start(context.Background(), bridge.StartRequest{
		AccountID:  "whatsapp-primary",
		Generation: 3,
	}, nil)
	if err != nil {
		t.Fatalf("Start() at RetryAt error = %v", err)
	}
	t.Cleanup(func() { stopRun(t, lifecycleRun) })
	awaitCondition(t, "adapter Connect at RetryAt", func() bool {
		return client.stats().connectCompleted == 1
	})
	if now := clock.Now(); !now.Equal(retryAt) {
		t.Fatalf("eligible Start time = %s, want %s", now, retryAt)
	}
}

func TestTemporaryBanCooldownHonorsRetryAt(t *testing.T) {
	clock := newManualClock(whatsappAdapterTestEpoch)
	client := newFakeLifecycleClient()
	a := newTestAdapter(t, client, clock.Now)
	supervisor := newTestSupervisor(t, a, clock)

	startSupervisor(t, supervisor)
	awaitCondition(t, "initial WhatsApp connect", func() bool {
		return client.stats().connectCompleted == 1
	})

	retryAt := clock.Now().Add(30 * time.Second)
	client.emit(a, whatsapplive.LifecycleEvent{
		Raw: &waevents.TemporaryBan{
			Code:   waevents.TempBanSentTooManySameMessage,
			Expire: 30 * time.Second,
		},
		Paired:  true,
		At:      clock.Now(),
		RetryAt: retryAt,
	})
	snapshot := awaitSnapshot(t, supervisor, "temporary-ban backoff", func(snapshot bridge.Snapshot) bool {
		return snapshot.State == bridge.StateBackoff &&
			snapshot.ErrorClass == bridge.FailureRateLimited
	})
	if !snapshot.RetryAt.Equal(retryAt) {
		t.Fatalf("RetryAt = %s, want %s", snapshot.RetryAt, retryAt)
	}
	if got := client.stats().connectCalls; got != 1 {
		t.Fatalf("connect Starts immediately after ban = %d, want 1", got)
	}

	clock.Advance(29 * time.Second)
	syncSupervisor(t, supervisor)
	if got := client.stats().connectCalls; got != 1 {
		t.Fatalf("connect Starts before RetryAt = %d, want 1", got)
	}

	clock.Advance(time.Second)
	awaitCondition(t, "connect eligibility at RetryAt", func() bool {
		return client.stats().connectCalls == 2
	})
	if now := clock.Now(); !now.Equal(retryAt) {
		t.Fatalf("restart time = %s, want RetryAt %s", now, retryAt)
	}
}

func TestConcurrentStartsAreSingleFlight(t *testing.T) {
	client := newFakeLifecycleClient()
	client.accountInfoGate = make(chan struct{})
	client.accountInfoEntered = make(chan struct{}, 2)
	client.connectGate = make(chan struct{})
	a := newTestAdapter(t, client, func() time.Time { return whatsappAdapterTestEpoch })

	type startResult struct {
		run bridge.Run
		err error
	}
	results := make(chan startResult, 2)
	releaseStarts := make(chan struct{})
	for range 2 {
		go func() {
			<-releaseStarts
			run, err := a.Start(context.Background(), bridge.StartRequest{
				AccountID:  "whatsapp-primary",
				Generation: 1,
			}, nil)
			results <- startResult{run: run, err: err}
		}()
	}
	close(releaseStarts)
	receiveValue(t, client.accountInfoEntered, "first Start account check")
	receiveValue(t, client.accountInfoEntered, "second Start account check")
	close(client.accountInfoGate)

	got := []startResult{
		receiveValue(t, results, "first Start result"),
		receiveValue(t, results, "second Start result"),
	}
	var successful []bridge.Run
	var failures []error
	for _, result := range got {
		if result.err == nil {
			successful = append(successful, result.run)
		} else {
			failures = append(failures, result.err)
		}
	}
	for _, run := range successful {
		defer stopRun(t, run)
	}

	awaitCondition(t, "single admitted Connect call", func() bool {
		stats := client.stats()
		return stats.connectCalls >= 1 && stats.activeConnects >= 1
	})
	stats := client.stats()
	if len(successful) != 1 || len(failures) != 1 {
		t.Fatalf("Start results = %d successes, %d failures; want one each", len(successful), len(failures))
	}
	if successful[0] == nil {
		t.Fatal("successful Start returned a nil Run")
	}
	failure, ok := asOpError(failures[0])
	if !ok || failure.Class != bridge.FailureMisconfigured ||
		failure.Fingerprint != "overlapping_whatsapp_generation" {
		t.Fatalf("overlapping Start error = %v, want overlapping misconfigured failure", failures[0])
	}
	if stats.connectCalls != 1 || stats.activeConnects != 1 || stats.maxActiveConnects != 1 {
		t.Fatalf("Connect concurrency = %+v, want one call and max concurrency one", stats)
	}
}

func newTestAdapter(t *testing.T, client *fakeLifecycleClient, now func() time.Time) *Adapter {
	t.Helper()
	a := New("whatsapp-primary", &whatsapplive.Bridge{})
	a.connect = client.Connect
	a.disconnect = client.Disconnect
	a.probe = client.Probe
	a.accountInfo = client.AccountInfo
	a.cooldown = client.Cooldown
	a.now = now
	return a
}

func newTestSupervisor(t *testing.T, adapter *Adapter, clock *manualClock) *bridge.Supervisor {
	t.Helper()
	supervisor, err := bridge.NewSupervisor(
		"whatsapp-primary",
		bridge.PlatformWhatsApp,
		adapter,
		bridge.Policy{
			ConnectTimeout:     time.Minute,
			ProbeEvery:         time.Minute,
			ProbeTimeout:       time.Minute,
			LivenessTimeout:    5 * time.Minute,
			MinBackoff:         10 * time.Second,
			MaxBackoff:         time.Minute,
			MaxSameFingerprint: 3,
		},
		clock,
		midpointRandom{},
	)
	if err != nil {
		t.Fatalf("NewSupervisor() error = %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), whatsappAdapterTestTimeout)
		defer cancel()
		if err := supervisor.Stop(ctx); err != nil {
			t.Errorf("Supervisor.Stop() error = %v", err)
		}
	})
	return supervisor
}

func startSupervisor(t *testing.T, supervisor *bridge.Supervisor) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), whatsappAdapterTestTimeout)
	defer cancel()
	if err := supervisor.Start(ctx, bridge.StartRequest{}); err != nil {
		t.Fatalf("Supervisor.Start() error = %v", err)
	}
}

func syncSupervisor(t *testing.T, supervisor *bridge.Supervisor) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), whatsappAdapterTestTimeout)
	defer cancel()
	if err := supervisor.Sync(ctx); err != nil {
		t.Fatalf("Supervisor.Sync() error = %v", err)
	}
}

func awaitSnapshot(
	t *testing.T,
	supervisor *bridge.Supervisor,
	description string,
	predicate func(bridge.Snapshot) bool,
) bridge.Snapshot {
	t.Helper()
	var snapshot bridge.Snapshot
	awaitCondition(t, description, func() bool {
		snapshot = supervisor.Snapshot()
		return predicate(snapshot)
	})
	return snapshot
}

func awaitCondition(t *testing.T, description string, condition func() bool) {
	t.Helper()
	timer := time.NewTimer(whatsappAdapterTestTimeout)
	defer timer.Stop()
	for !condition() {
		select {
		case <-timer.C:
			t.Fatalf("timed out waiting for %s", description)
		default:
			runtime.Gosched()
		}
	}
}

func receiveValue[T any](t *testing.T, channel <-chan T, description string) T {
	t.Helper()
	select {
	case value := <-channel:
		return value
	case <-time.After(whatsappAdapterTestTimeout):
		var zero T
		t.Fatalf("timed out waiting for %s", description)
		return zero
	}
}

func assertNotClosed(t *testing.T, channel <-chan struct{}, after string) {
	t.Helper()
	select {
	case <-channel:
		t.Fatalf("Ready closed after %s without a paired Connected event", after)
	default:
	}
}

func stopRun(t *testing.T, run bridge.Run) {
	t.Helper()
	if run == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), whatsappAdapterTestTimeout)
	defer cancel()
	if err := run.Stop(ctx); err != nil {
		t.Fatalf("Run.Stop() error = %v", err)
	}
}

type fakeLifecycleClient struct {
	mu sync.Mutex

	paired        bool
	cooldownUntil time.Time
	connectCalls  int
	connectDone   int
	active        int
	maxActive     int
	disconnects   int

	accountInfoGate    chan struct{}
	accountInfoEntered chan struct{}
	connectGate        chan struct{}
}

type fakeLifecycleStats struct {
	connectCalls      int
	connectCompleted  int
	activeConnects    int
	maxActiveConnects int
	disconnects       int
}

func newFakeLifecycleClient() *fakeLifecycleClient {
	return &fakeLifecycleClient{paired: true}
}

func (f *fakeLifecycleClient) Connect(ctx context.Context) error {
	f.mu.Lock()
	f.connectCalls++
	f.active++
	if f.active > f.maxActive {
		f.maxActive = f.active
	}
	gate := f.connectGate
	f.mu.Unlock()

	var err error
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			err = ctx.Err()
		}
	}

	f.mu.Lock()
	f.active--
	f.connectDone++
	f.mu.Unlock()
	return err
}

func (f *fakeLifecycleClient) Disconnect() {
	f.mu.Lock()
	f.disconnects++
	f.mu.Unlock()
}

func (*fakeLifecycleClient) Probe(context.Context) error { return nil }

func (f *fakeLifecycleClient) AccountInfo() whatsapplive.AccountInfo {
	f.mu.Lock()
	paired := f.paired
	gate := f.accountInfoGate
	entered := f.accountInfoEntered
	f.mu.Unlock()
	if gate != nil {
		if entered != nil {
			entered <- struct{}{}
		}
		<-gate
	}
	return whatsapplive.AccountInfo{Paired: paired, JID: "15551234567@s.whatsapp.net"}
}

func (f *fakeLifecycleClient) Cooldown() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cooldownUntil
}

func (f *fakeLifecycleClient) emit(a *Adapter, event whatsapplive.LifecycleEvent) {
	if ban, ok := event.Raw.(*waevents.TemporaryBan); ok && ban != nil {
		retryAt := event.RetryAt
		if retryAt.IsZero() && ban.Expire > 0 {
			retryAt = event.At.Add(ban.Expire)
		}
		f.mu.Lock()
		if retryAt.After(f.cooldownUntil) {
			f.cooldownUntil = retryAt
		}
		f.mu.Unlock()
	}
	a.handleLifecycleEvent(event)
}

func (f *fakeLifecycleClient) stats() fakeLifecycleStats {
	f.mu.Lock()
	defer f.mu.Unlock()
	return fakeLifecycleStats{
		connectCalls:      f.connectCalls,
		connectCompleted:  f.connectDone,
		activeConnects:    f.active,
		maxActiveConnects: f.maxActive,
		disconnects:       f.disconnects,
	}
}

type recordingBeat struct {
	generation bridge.Generation
	at         time.Time
	detail     string
}

type recordingSink struct {
	mu    sync.Mutex
	beats []recordingBeat
}

func (*recordingSink) AppendIngress(context.Context, bridge.RawIngressRecord) error { return nil }

func (*recordingSink) EmitEphemeral(context.Context, bridge.EphemeralEvent) error { return nil }

func (s *recordingSink) Beat(generation bridge.Generation, at time.Time, detail string) {
	s.mu.Lock()
	s.beats = append(s.beats, recordingBeat{generation: generation, at: at, detail: detail})
	s.mu.Unlock()
}

func (s *recordingSink) beatsSnapshot() []recordingBeat {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]recordingBeat(nil), s.beats...)
}

type midpointRandom struct{}

func (midpointRandom) Int63n(n int64) int64 {
	if n <= 1 {
		return 0
	}
	return n / 2
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

var (
	_ bridge.Clock          = (*manualClock)(nil)
	_ bridge.Timer          = (*manualTimer)(nil)
	_ bridge.Random         = midpointRandom{}
	_ bridge.ConnectionSink = (*recordingSink)(nil)
)
