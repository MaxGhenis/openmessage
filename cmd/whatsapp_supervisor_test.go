package cmd

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/maxghenis/openmessage/internal/bridge"
)

func TestWhatsAppSupervisorControlRemoteLoggedOutCanPairAgain(t *testing.T) {
	tests := []struct {
		name       string
		wantMethod bridge.PairMethod
		recover    func(*whatsappSupervisorControl) error
	}{
		{
			name:       "Reconnect starts QR pairing",
			wantMethod: bridge.PairQR,
			recover:    (*whatsappSupervisorControl).Reconnect,
		},
		{
			name:       "PairPhone starts phone-code pairing",
			wantMethod: bridge.PairPhoneCode,
			recover: func(control *whatsappSupervisorControl) error {
				code, err := control.PairPhone("+15551234567")
				if err == nil && code != whatsappControlTestPairCode {
					return errors.New("PairPhone returned the wrong code")
				}
				return err
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			harness := newWhatsAppControlTestHarness(t, bridge.StateStopped, true)

			if err := harness.control.Reconnect(); err != nil {
				t.Fatalf("initial Reconnect() error = %v", err)
			}
			awaitWhatsAppControlState(t, harness.first, bridge.StateOnline)

			// whatsapplive deletes the local session before publishing LoggedOut.
			// The adapter then completes the active generation with ReauthRequired.
			harness.adapter.paired.Store(false)
			harness.adapter.Run(0).Fail(bridge.OpError{
				Class:       bridge.FailureReauthRequired,
				Operation:   "event",
				Fingerprint: "whatsapp_logged_out",
				Cause:       errors.New("remote device logged out"),
			})
			awaitWhatsAppControlState(t, harness.first, bridge.StateBlocked)

			if err := test.recover(harness.control); err != nil {
				t.Fatalf("re-pair after remote LoggedOut error = %v", err)
			}
			replacement := harness.currentSupervisor()
			if replacement == harness.first {
				t.Fatal("re-pair reused the blocked supervisor after remote LoggedOut")
			}
			awaitWhatsAppControlState(t, replacement, bridge.StateOnline)

			if got := harness.adapter.PairCount(); got != 1 {
				t.Fatalf("pair attempts = %d, want 1", got)
			}
			if request := harness.adapter.PairRequest(0); request.Method != test.wantMethod {
				t.Fatalf("pair method = %q, want %q", request.Method, test.wantMethod)
			}
			if got := harness.adapter.StartCount(); got != 2 {
				t.Fatalf("connection starts = %d, want initial start plus one post-pair start", got)
			}
			if got := harness.supervisorCount.Load(); got != 2 {
				t.Fatalf("supervisors constructed = %d, want 2", got)
			}
		})
	}
}

func TestWhatsAppSupervisorControlUnpairCanPairPhoneAgain(t *testing.T) {
	harness := newWhatsAppControlTestHarness(t, bridge.StateStopped, true)

	if err := harness.control.Reconnect(); err != nil {
		t.Fatalf("initial Reconnect() error = %v", err)
	}
	awaitWhatsAppControlState(t, harness.first, bridge.StateOnline)
	if err := harness.control.StopAndUnpair(func() error {
		harness.adapter.paired.Store(false)
		return nil
	}); err != nil {
		t.Fatalf("StopAndUnpair() error = %v", err)
	}

	code, err := harness.control.PairPhone(" +15551234567 ")
	if err != nil {
		t.Fatalf("PairPhone() after unpair error = %v", err)
	}
	if code != whatsappControlTestPairCode {
		t.Fatalf("PairPhone() code = %q, want %q", code, whatsappControlTestPairCode)
	}
	replacement := harness.currentSupervisor()
	if replacement == harness.first {
		t.Fatal("PairPhone() reused the terminal supervisor after unpair")
	}
	awaitWhatsAppControlState(t, replacement, bridge.StateOnline)

	request := harness.adapter.PairRequest(0)
	if request.Method != bridge.PairPhoneCode || request.Phone != "+15551234567" {
		t.Fatalf("pair request = %+v, want trimmed phone-code request", request)
	}
	if got := harness.adapter.StartCount(); got != 2 {
		t.Fatalf("connection starts = %d, want initial start plus one post-pair start", got)
	}
}

func TestWhatsAppSupervisorPairWatcherStartsExactlyOnce(t *testing.T) {
	harness := newWhatsAppControlTestHarness(t, bridge.StateUnpaired, false)

	code, err := harness.control.PairPhone("+15557654321")
	if err != nil {
		t.Fatalf("PairPhone() error = %v", err)
	}
	if code != whatsappControlTestPairCode {
		t.Fatalf("PairPhone() code = %q, want %q", code, whatsappControlTestPairCode)
	}
	awaitWhatsAppControlState(t, harness.first, bridge.StateOnline)

	// Give any duplicate watcher enough time to observe StateStopped and issue
	// another Start. The supervisor itself would reject overlap, while the
	// lifecycle count proves the handoff performed exactly one real start.
	time.Sleep(50 * time.Millisecond)
	if got := harness.adapter.StartCount(); got != 1 {
		t.Fatalf("post-pair connection starts = %d, want exactly 1", got)
	}
	if got := harness.adapter.PairCount(); got != 1 {
		t.Fatalf("pair attempts = %d, want exactly 1", got)
	}
}

const whatsappControlTestPairCode = "1234-5678"

type whatsappControlTestHarness struct {
	control         *whatsappSupervisorControl
	adapter         *whatsappControlTestAdapter
	first           *bridge.Supervisor
	supervisorCount atomic.Int32
}

func newWhatsAppControlTestHarness(
	t *testing.T,
	initial bridge.State,
	paired bool,
) *whatsappControlTestHarness {
	t.Helper()
	harness := &whatsappControlTestHarness{adapter: &whatsappControlTestAdapter{}}
	harness.adapter.paired.Store(paired)
	newSupervisor := func(initial bridge.State) (*bridge.Supervisor, error) {
		harness.supervisorCount.Add(1)
		return bridge.NewSupervisor(
			whatsappAccountID,
			bridge.PlatformWhatsApp,
			harness.adapter,
			whatsappSupervisorPolicy(),
			whatsappWallClock{},
			whatsappRandom{},
			bridge.WithPairer(harness.adapter),
			bridge.WithInitialState(initial),
		)
	}
	first, err := newSupervisor(initial)
	if err != nil {
		t.Fatalf("NewSupervisor() error = %v", err)
	}
	harness.first = first
	harness.control = newWhatsAppSupervisorControl(
		first,
		newSupervisor,
		func() bool { return harness.adapter.paired.Load() },
	)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := harness.control.Stop(ctx); err != nil {
			t.Errorf("control.Stop() error = %v", err)
		}
	})
	return harness
}

func (h *whatsappControlTestHarness) currentSupervisor() *bridge.Supervisor {
	h.control.mu.Lock()
	defer h.control.mu.Unlock()
	return h.control.supervisor
}

type whatsappControlTestAdapter struct {
	paired atomic.Bool

	mu           sync.Mutex
	runs         []*whatsappControlTestRun
	pairRequests []bridge.PairRequest
}

func (a *whatsappControlTestAdapter) Start(
	_ context.Context,
	_ bridge.StartRequest,
	_ bridge.ConnectionSink,
) (bridge.Run, error) {
	run := newWhatsAppControlTestRun()
	a.mu.Lock()
	a.runs = append(a.runs, run)
	a.mu.Unlock()
	return run, nil
}

func (a *whatsappControlTestAdapter) Pair(
	ctx context.Context,
	request bridge.PairRequest,
	sink bridge.PairSink,
) (bridge.PairResult, error) {
	a.mu.Lock()
	a.pairRequests = append(a.pairRequests, request)
	a.mu.Unlock()
	if request.Method == bridge.PairPhoneCode {
		if err := sink.EmitPairEvent(ctx, bridge.PairEvent{
			Kind:      "code",
			Payload:   whatsappControlTestPairCode,
			ExpiresAt: time.Now().Add(time.Minute),
		}); err != nil {
			return bridge.PairResult{}, err
		}
	}
	a.paired.Store(true)
	return bridge.PairResult{
		RemoteAccountID: "whatsapp-user",
		RemoteDeviceID:  "whatsapp-device",
	}, nil
}

func (a *whatsappControlTestAdapter) Unpair(context.Context, string) error {
	a.paired.Store(false)
	return nil
}

func (a *whatsappControlTestAdapter) StartCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.runs)
}

func (a *whatsappControlTestAdapter) PairCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.pairRequests)
}

func (a *whatsappControlTestAdapter) Run(index int) *whatsappControlTestRun {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.runs[index]
}

func (a *whatsappControlTestAdapter) PairRequest(index int) bridge.PairRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.pairRequests[index]
}

type whatsappControlTestRun struct {
	ready chan struct{}
	done  chan error
	once  sync.Once
}

func newWhatsAppControlTestRun() *whatsappControlTestRun {
	run := &whatsappControlTestRun{
		ready: make(chan struct{}),
		done:  make(chan error, 1),
	}
	close(run.ready)
	return run
}

func (r *whatsappControlTestRun) Ready() <-chan struct{} { return r.ready }
func (r *whatsappControlTestRun) Done() <-chan error     { return r.done }
func (r *whatsappControlTestRun) Probe(context.Context) (bridge.Liveness, error) {
	return bridge.Liveness{AliveAt: time.Now(), Detail: "test"}, nil
}
func (r *whatsappControlTestRun) Stop(context.Context) error {
	r.finish(nil)
	return nil
}
func (r *whatsappControlTestRun) Fail(err error) { r.finish(err) }
func (r *whatsappControlTestRun) finish(err error) {
	r.once.Do(func() {
		if err != nil {
			r.done <- err
		}
		close(r.done)
	})
}

func awaitWhatsAppControlState(
	t *testing.T,
	supervisor *bridge.Supervisor,
	want bridge.State,
) bridge.Snapshot {
	t.Helper()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		snapshot := supervisor.Snapshot()
		if snapshot.State == want {
			return snapshot
		}
		select {
		case <-deadline.C:
			t.Fatalf("supervisor state = %q, want %q (snapshot %+v)", snapshot.State, want, snapshot)
		case <-ticker.C:
		}
	}
}

var _ bridge.Lifecycle = (*whatsappControlTestAdapter)(nil)
var _ bridge.Pairer = (*whatsappControlTestAdapter)(nil)
