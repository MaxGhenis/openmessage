package google

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/mautrix-gmessages/pkg/libgm"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/events"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"

	"github.com/maxghenis/openmessage/internal/app"
	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/client"
)

func TestEventFailureClassification(t *testing.T) {
	a := New("google-primary", nil, func() bool { return true })
	r := &run{adapter: a}

	tests := []struct {
		name      string
		event     any
		terminal  bool
		wantClass bridge.FailureClass
	}{
		{
			name:      "listen fatal transient",
			event:     &events.ListenFatalError{Error: errors.New("temporary token refresh failure")},
			terminal:  true,
			wantClass: bridge.FailureTransient,
		},
		{
			name:      "listen fatal without cause is transient",
			event:     &events.ListenFatalError{},
			terminal:  true,
			wantClass: bridge.FailureTransient,
		},
		{
			name:      "listen fatal auth expiry requires credential repair",
			event:     &events.ListenFatalError{Error: errors.New("HTTP 401: invalid authentication credentials")},
			terminal:  true,
			wantClass: bridge.FailureCredentialsExpired,
		},
		{
			name:      "Gaia logout requires reauth",
			event:     &events.GaiaLoggedOut{},
			terminal:  true,
			wantClass: bridge.FailureReauthRequired,
		},
		{
			name:     "second ping is tolerated",
			event:    &events.PingFailed{ErrorCount: 2},
			terminal: false,
		},
		{
			name:      "third ping ends generation",
			event:     &events.PingFailed{ErrorCount: 3, Error: errors.New("phone ping failed")},
			terminal:  true,
			wantClass: bridge.FailureTransient,
		},
		{
			name:     "phone offline is not credential failure",
			event:    &events.PhoneNotResponding{},
			terminal: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			failure, terminal := r.classifyEvent(test.event)
			if terminal != test.terminal {
				t.Fatalf("terminal = %v, want %v", terminal, test.terminal)
			}
			if failure.Class != test.wantClass {
				t.Fatalf("class = %q, want %q", failure.Class, test.wantClass)
			}
		})
	}
}

func TestAuthExpiredWithoutRepairIsBlocked(t *testing.T) {
	a := New("google-primary", nil, func() bool { return false })
	failure := a.classifyTransportError(
		errors.New("HTTP 401: invalid authentication credentials"),
		"connect",
		"connect_failed",
	)
	if failure.Class != bridge.FailureUpgradeRequired {
		t.Fatalf("class = %q, want %q", failure.Class, bridge.FailureUpgradeRequired)
	}
}

func TestReadyUsesForkRealPingerSignal(t *testing.T) {
	host := newTestApp(t)
	fake := &fakeTransport{probeResponse: make(chan *libgm.IncomingRPCMessage, 1)}
	a := newTestAdapter(t, host, fake)
	sink := &recordingSink{}

	run, err := a.Start(context.Background(), bridge.StartRequest{
		AccountID:  "google-primary",
		Generation: 1,
	}, sink)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { stopRun(t, run) })

	assertNotClosed(t, run.Ready(), "Connect return")
	fake.emit(&events.AuthTokenRefreshed{})
	assertNotClosed(t, run.Ready(), "AuthTokenRefreshed")

	fake.emit(&events.PhoneNotResponding{})
	select {
	case <-run.Ready():
	case <-time.After(time.Second):
		t.Fatal("Ready did not close after the fork's PhoneNotResponding pinger signal")
	}
	if !host.Connected.Load() {
		t.Fatal("compatibility Connected status was not published on readiness")
	}
	if host.GoogleStatus().PhoneResponding {
		t.Fatal("PhoneNotResponding readiness evidence was mistaken for phone reachability")
	}
	if got := sink.beatCount(); got != 1 {
		t.Fatalf("liveness beats = %d, want 1", got)
	}
}

func TestReadyUsesForkRealLongPollDataEvidence(t *testing.T) {
	tests := []struct {
		name  string
		event any
	}{
		{name: "no data pinger signal", event: &events.NoDataReceived{}},
		{name: "phone responding pinger signal", event: &events.PhoneRespondingAgain{}},
		{name: "conversation", event: &gmproto.Conversation{ConversationID: "ready-conversation"}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			host := newTestApp(t)
			fake := &fakeTransport{}
			a := newTestAdapter(t, host, fake)
			run, err := a.Start(context.Background(), bridge.StartRequest{
				AccountID:  "google-primary",
				Generation: 1,
			}, nil)
			if err != nil {
				t.Fatalf("Start() error = %v", err)
			}
			t.Cleanup(func() { stopRun(t, run) })

			fake.emit(test.event)
			select {
			case <-run.Ready():
			case <-time.After(time.Second):
				t.Fatalf("Ready did not close after %T", test.event)
			}
		})
	}
}

func TestLateCallbackCannotMutateNewGeneration(t *testing.T) {
	host := newTestApp(t)
	firstTransport := &fakeTransport{}
	secondTransport := &fakeTransport{}
	transports := []*fakeTransport{firstTransport, secondTransport}
	a := New("google-primary", host, func() bool { return true })
	a.newClient = func() (*client.Client, transportClient, error) {
		transport := transports[0]
		transports = transports[1:]
		return newLegacyClient(t), transport, nil
	}

	first, err := a.Start(context.Background(), bridge.StartRequest{AccountID: "google-primary", Generation: 1}, nil)
	if err != nil {
		t.Fatalf("first Start() error = %v", err)
	}
	firstHandler := firstTransport.currentHandler()
	firstTransport.emit(&events.PhoneNotResponding{})
	<-first.Ready()
	a.ReportError(errors.New("temporary disconnect"))
	terminalError(t, first.Done(), bridge.FailureTransient)

	second, err := a.Start(context.Background(), bridge.StartRequest{AccountID: "google-primary", Generation: 2}, nil)
	if err != nil {
		t.Fatalf("second Start() error = %v", err)
	}
	t.Cleanup(func() { stopRun(t, second) })
	secondTransport.emit(&gmproto.Conversation{ConversationID: "generation-2-ready"})
	<-second.Ready()
	current := host.GetClient()

	firstHandler(&events.GaiaLoggedOut{})
	if host.GetClient() != current {
		t.Fatal("late generation-1 callback replaced or cleared generation-2 client")
	}
	if !host.Connected.Load() {
		t.Fatal("late generation-1 callback marked generation 2 disconnected")
	}
	if !host.GooglePaired() {
		t.Fatal("late generation-1 callback deleted current stored session")
	}
}

func TestRecoveredHandlerPanicEndsGenerationTransient(t *testing.T) {
	host := newTestApp(t)
	fake := &fakeTransport{}
	a := newTestAdapter(t, host, fake)
	run, err := a.Start(context.Background(), bridge.StartRequest{AccountID: "google-primary", Generation: 1}, nil)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	host.EventHandler.OnSessionInvalid = func() { panic("malformed Google event") }
	fake.emit(&events.GaiaLoggedOut{})
	terminalError(t, run.Done(), bridge.FailureTransient)
	if host.Connected.Load() {
		t.Fatal("recovered callback panic left Google connected")
	}
}

func TestParkCurrentClearsConnectedAndSurfacesNeedsRepair(t *testing.T) {
	host := newTestApp(t)
	fake := &fakeTransport{}
	a := newTestAdapter(t, host, fake)
	run, err := a.Start(context.Background(), bridge.StartRequest{
		AccountID:  "google-primary",
		Generation: 1,
	}, nil)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	fake.emit(&gmproto.Conversation{ConversationID: "park-ready"})
	<-run.Ready()
	if !host.Connected.Load() {
		t.Fatal("precondition: generation did not publish connected status")
	}

	if !a.ParkCurrent(errors.New("linked device requires repair")) {
		t.Fatal("ParkCurrent() did not terminate the active generation")
	}
	terminalError(t, run.Done(), bridge.FailureUpgradeRequired)
	if host.Connected.Load() {
		t.Fatal("parked generation left compatibility Connected=true")
	}
	if !host.GoogleStatus().NeedsRepair {
		t.Fatal("parked generation did not surface needs_repair")
	}
}

func TestStopWaitsForAdmittedCallback(t *testing.T) {
	host := newTestApp(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	host.OnStatusChange = func(connected bool) {
		if connected {
			close(entered)
			<-release
		}
	}
	fake := &fakeTransport{}
	a := newTestAdapter(t, host, fake)
	run, err := a.Start(context.Background(), bridge.StartRequest{AccountID: "google-primary", Generation: 1}, nil)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	go fake.emit(&gmproto.Conversation{ConversationID: "stop-ready"})
	<-entered

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := run.Stop(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Stop() error = %v, want deadline while callback is admitted", err)
	}
	close(release)
	stopRun(t, run)
	if fake.disconnectCount() != 1 {
		t.Fatalf("Disconnect calls = %d, want 1", fake.disconnectCount())
	}
}

func TestProbeCompletionCannotConsumeRunTerminalError(t *testing.T) {
	host := newTestApp(t)
	fake := &fakeTransport{probeResponse: make(chan *libgm.IncomingRPCMessage)}
	a := newTestAdapter(t, host, fake)
	run, err := a.Start(context.Background(), bridge.StartRequest{AccountID: "google-primary", Generation: 1}, nil)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	probeDone := make(chan error, 1)
	go func() {
		_, probeErr := run.Probe(context.Background())
		probeDone <- probeErr
	}()
	a.ReportError(errors.New("temporary disconnect"))
	select {
	case err := <-probeDone:
		if err == nil {
			t.Fatal("Probe unexpectedly succeeded after generation ended")
		}
	case <-time.After(time.Second):
		t.Fatal("Probe did not stop with its generation")
	}
	terminalError(t, run.Done(), bridge.FailureTransient)
}

func TestProbeWithoutProtocolActivityStillTimesOut(t *testing.T) {
	host := newTestApp(t)
	fake := &fakeTransport{}
	a := newTestAdapter(t, host, fake)
	run, err := a.Start(context.Background(), bridge.StartRequest{
		AccountID:  "google-primary",
		Generation: 1,
	}, nil)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { stopRun(t, run) })

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, probeErr := run.Probe(ctx); !errors.Is(probeErr, context.DeadlineExceeded) {
		t.Fatalf("Probe error = %v, want deadline with no pinger or long-poll activity", probeErr)
	}
}

func TestPhoneUnreachableSurvivesRepeatedLivenessWindowsAndRecovers(t *testing.T) {
	host := newTestApp(t)
	fake := &fakeTransport{}
	a := newTestAdapter(t, host, fake)
	sink := &recordingSink{}
	run, err := a.Start(context.Background(), bridge.StartRequest{
		AccountID:  "google-primary",
		Generation: 1,
	}, sink)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { stopRun(t, run) })

	// The pinned fork emits this after its first ditto ping times out. It is
	// readiness evidence for the linked-device session, but not evidence that
	// the Android phone itself is reachable.
	fake.emit(&events.PhoneNotResponding{})
	select {
	case <-run.Ready():
	case <-time.After(time.Second):
		t.Fatal("PhoneNotResponding did not make the generation ready")
	}
	if host.GoogleStatus().PhoneResponding {
		t.Fatal("precondition: phone should be recorded as unreachable")
	}

	// Model three consecutive supervisor liveness windows. NotifyDittoActivity
	// has no phone response in any window, but the known phone-off state keeps
	// the healthy generation alive instead of triggering reconnect churn.
	for window := 1; window <= 3; window++ {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		liveness, probeErr := run.Probe(ctx)
		cancel()
		if probeErr != nil {
			t.Fatalf("Probe window %d error = %v", window, probeErr)
		}
		if liveness.Detail != "phone_not_responding" {
			t.Fatalf("Probe window %d detail = %q, want phone_not_responding", window, liveness.Detail)
		}
		select {
		case err := <-run.Done():
			t.Fatalf("generation ended during phone-off window %d: %v", window, err)
		default:
		}
	}
	if got := fake.probeCount(); got != 3 {
		t.Fatalf("NotifyDittoActivity calls = %d, want 3", got)
	}

	// Because the generation and its callback remain installed, the fork's
	// recovery signal is still handled and renews liveness/reconciliation.
	fake.emit(&events.PhoneRespondingAgain{})
	if !host.GoogleStatus().PhoneResponding {
		t.Fatal("PhoneRespondingAgain was ignored after repeated phone-off probes")
	}
	if got := sink.beatCount(); got != 2 {
		t.Fatalf("liveness beats = %d, want 2 (offline and recovery)", got)
	}
	select {
	case err := <-run.Done():
		t.Fatalf("generation ended instead of accepting PhoneRespondingAgain: %v", err)
	default:
	}
}

func newTestApp(t *testing.T) *app.App {
	t.Helper()
	t.Setenv("OPENMESSAGES_DATA_DIR", t.TempDir())
	t.Setenv("OPENMESSAGE_GOOGLE_AVATAR_SYNC", "0")
	host, err := app.New(zerolog.Nop())
	if err != nil {
		t.Fatalf("app.New() error = %v", err)
	}
	if err := client.SaveSession(host.SessionPath, &client.SessionData{AuthDataJSON: []byte(`{}`)}); err != nil {
		host.Close()
		t.Fatalf("SaveSession() error = %v", err)
	}
	t.Cleanup(host.Close)
	return host
}

func newLegacyClient(t *testing.T) *client.Client {
	t.Helper()
	legacy, err := client.NewFromSession(
		&client.SessionData{AuthDataJSON: []byte(`{}`)},
		zerolog.Nop(),
	)
	if err != nil {
		t.Fatalf("NewFromSession() error = %v", err)
	}
	return legacy
}

func newTestAdapter(t *testing.T, host *app.App, fake *fakeTransport) *Adapter {
	t.Helper()
	a := New("google-primary", host, func() bool { return true })
	a.newClient = func() (*client.Client, transportClient, error) {
		return newLegacyClient(t), fake, nil
	}
	return a
}

func assertNotClosed(t *testing.T, channel <-chan struct{}, after string) {
	t.Helper()
	select {
	case <-channel:
		t.Fatalf("Ready closed after %s without a real long-poll event", after)
	default:
	}
}

func terminalError(t *testing.T, done <-chan error, want bridge.FailureClass) {
	t.Helper()
	select {
	case err := <-done:
		failure, ok := asOpError(err)
		if !ok || failure.Class != want {
			t.Fatalf("Done error = %v, want class %q", err, want)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for Run.Done")
	}
}

func stopRun(t *testing.T, run bridge.Run) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := run.Stop(ctx); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
}

type fakeTransport struct {
	mu            sync.Mutex
	handler       libgm.EventHandler
	connectErr    error
	disconnects   int
	probeResponse chan *libgm.IncomingRPCMessage
	probeErr      error
	probes        int
}

func (f *fakeTransport) SetEventHandler(handler libgm.EventHandler) {
	f.mu.Lock()
	f.handler = handler
	f.mu.Unlock()
}

func (f *fakeTransport) Connect() error { return f.connectErr }

func (f *fakeTransport) Disconnect() {
	f.mu.Lock()
	f.disconnects++
	f.mu.Unlock()
}

func (f *fakeTransport) NotifyDittoActivity() (<-chan *libgm.IncomingRPCMessage, error) {
	f.mu.Lock()
	f.probes++
	response, err := f.probeResponse, f.probeErr
	f.mu.Unlock()
	return response, err
}

func (f *fakeTransport) emit(event any) {
	if handler := f.currentHandler(); handler != nil {
		handler(event)
	}
}

func (f *fakeTransport) currentHandler() libgm.EventHandler {
	f.mu.Lock()
	handler := f.handler
	f.mu.Unlock()
	return handler
}

func (f *fakeTransport) disconnectCount() int {
	f.mu.Lock()
	count := f.disconnects
	f.mu.Unlock()
	return count
}

func (f *fakeTransport) probeCount() int {
	f.mu.Lock()
	count := f.probes
	f.mu.Unlock()
	return count
}

type recordingSink struct {
	mu    sync.Mutex
	beats int
}

func (*recordingSink) AppendIngress(context.Context, bridge.RawIngressRecord) error { return nil }

func (*recordingSink) EmitEphemeral(context.Context, bridge.EphemeralEvent) error { return nil }

func (s *recordingSink) Beat(bridge.Generation, time.Time, string) {
	s.mu.Lock()
	s.beats++
	s.mu.Unlock()
}

func (s *recordingSink) beatCount() int {
	s.mu.Lock()
	count := s.beats
	s.mu.Unlock()
	return count
}
