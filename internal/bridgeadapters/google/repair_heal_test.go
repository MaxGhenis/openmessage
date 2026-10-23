package google

import (
	"context"
	"errors"
	"math/rand"
	"sync"
	"testing"
	"time"

	"go.mau.fi/mautrix-gmessages/pkg/libgm/events"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"

	"github.com/maxghenis/openmessage/internal/app"
	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/client"
)

// The 2026-10-22 incident: a v2 text send's conversation lookup got HTTP 401
// at 19:26:58, the supervisor started a cookie repair, the refresh failed, and
// the supervisor parked in Blocked with nothing to retry it. SMS stayed dead
// until a human refreshed the cookies and restarted the app. These tests drive
// that path through the real adapter and the real supervisor: the repair must
// be retried until it works, and the account must come back online.

const repairHealTimeout = 5 * time.Second

func TestIncidentSendPath401WithFailingRefreshHealsWithoutHelp(t *testing.T) {
	h := newRepairHealHarness(t)
	h.start(t)

	fake := &fakeTextSendClient{
		conversationErr: errors.New("HTTP 401: 16: Request had invalid authentication credentials."),
	}
	installTextSendClient(t, h.host.GetClient(), fake)
	_, err := h.adapter.SendText(context.Background(), bridge.TextRequest{
		Conversation: bridge.ConversationRef{RemoteID: "conversation-id"},
		Body:         "queued text",
		RequestID:    "request-1",
	})
	if failure := requireTextOpError(t, err); failure.Class != bridge.FailureCredentialsExpired {
		t.Fatalf("send failure = %+v, want credentials_expired", failure)
	}

	h.awaitHealed(t, 3)
	if status := h.host.GoogleStatus(); !status.Connected || status.AuthExpired {
		t.Fatalf("status after heal = %+v, want connected and auth_expired cleared", status)
	}
}

func TestPing401RoutesToCredentialRepairWithoutThreeFailures(t *testing.T) {
	h := newRepairHealHarness(t)
	h.start(t)

	h.transport(0).emit(&events.PingFailed{
		ErrorCount: 1,
		Error:      errors.New("failed to notify ditto activity: HTTP 401: 16: Request had invalid authentication credentials."),
	})
	h.awaitHealed(t, 0)
}

// repairHealHarness wires one real Adapter to one real Supervisor on the wall
// clock with millisecond policy, plus a repairer that hands each attempt to
// the test, which fails a scripted number of them before one succeeds.
type repairHealHarness struct {
	host          *app.App
	adapter       *Adapter
	supervisor    *bridge.Supervisor
	repairStarted chan bridge.OpError
	repairResult  chan error

	mu         sync.Mutex
	transports []*fakeTransport
	repairs    int
	repairedAt []time.Time
}

func newRepairHealHarness(t *testing.T) *repairHealHarness {
	t.Helper()
	h := &repairHealHarness{
		host:          newTestApp(t),
		repairStarted: make(chan bridge.OpError),
		repairResult:  make(chan error),
	}
	h.adapter = New("google-primary", h.host, func() bool { return true })
	h.adapter.newClient = func() (*client.Client, transportClient, error) {
		transport := &fakeTransport{}
		h.mu.Lock()
		h.transports = append(h.transports, transport)
		h.mu.Unlock()
		return newLegacyClient(t), transport, nil
	}
	repairer := bridge.CredentialRepairFunc(func(ctx context.Context, _ string, failure bridge.OpError) error {
		h.mu.Lock()
		h.repairs++
		h.repairedAt = append(h.repairedAt, time.Now())
		h.mu.Unlock()
		select {
		case h.repairStarted <- failure:
		case <-ctx.Done():
			return ctx.Err()
		}
		select {
		case err := <-h.repairResult:
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	supervisor, err := bridge.NewSupervisor(
		"google-primary",
		bridge.PlatformGoogle,
		h.adapter,
		bridge.Policy{
			ConnectTimeout:     repairHealTimeout,
			ProbeEvery:         time.Hour,
			ProbeTimeout:       time.Second,
			LivenessTimeout:    time.Hour,
			MinBackoff:         time.Millisecond,
			MaxBackoff:         20 * time.Millisecond,
			MaxSameFingerprint: 5,
			RepairRetryFloor:   5 * time.Millisecond,
		},
		repairHealClock{},
		&repairHealRandom{r: rand.New(rand.NewSource(1022))},
		bridge.WithCredentialRepairer(repairer),
	)
	if err != nil {
		t.Fatalf("NewSupervisor() error = %v", err)
	}
	h.supervisor = supervisor
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), repairHealTimeout)
		defer cancel()
		if err := supervisor.Stop(ctx); err != nil {
			t.Errorf("Supervisor.Stop() error = %v", err)
		}
	})
	return h
}

func (h *repairHealHarness) transport(index int) *fakeTransport {
	h.mu.Lock()
	defer h.mu.Unlock()
	if index >= len(h.transports) {
		return nil
	}
	return h.transports[index]
}

// start brings generation 1 online the way libgm does: a long-poll event.
func (h *repairHealHarness) start(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), repairHealTimeout)
	defer cancel()
	if err := h.supervisor.Start(ctx, bridge.StartRequest{}); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	h.awaitTransport(t, 0).emit(&gmproto.Conversation{ConversationID: "ready"})
	h.awaitState(t, "generation 1 online", func(snapshot bridge.Snapshot) bool {
		return snapshot.State == bridge.StateOnline && snapshot.Generation == 1
	})
}

// awaitHealed fails the first `failures` repair attempts the way the
// incident's refresh failed, lets the next one succeed, and waits for the
// repaired generation 2 to come online. Each failed attempt must be retried,
// never parked, and no generation may start before a repair succeeds.
func (h *repairHealHarness) awaitHealed(t *testing.T, failures int) {
	t.Helper()
	for attempt := 1; attempt <= failures+1; attempt++ {
		var trigger bridge.OpError
		select {
		case trigger = <-h.repairStarted:
		case <-time.After(repairHealTimeout):
			t.Fatalf("repair attempt %d never started; supervisor %+v", attempt, h.supervisor.Snapshot())
		}
		if trigger.Class != bridge.FailureCredentialsExpired || trigger.Fingerprint != "google_auth_expired" {
			t.Fatalf("attempt %d trigger = %+v, want credentials_expired/google_auth_expired", attempt, trigger)
		}
		repairing := h.supervisor.Snapshot()
		if repairing.State != bridge.StateRepairingCredentials || repairing.Generation != 1 ||
			repairing.RepairAttempts != attempt {
			t.Fatalf("attempt %d snapshot = %+v, want generation 1 repairing, attempt %d", attempt, repairing, attempt)
		}
		if status := h.host.GoogleStatus(); !status.AuthExpired || status.Connected {
			t.Fatalf("status during repair = %+v, want auth_expired and disconnected", status)
		}
		if h.transport(1) != nil {
			t.Fatalf("a generation started before any repair succeeded (attempt %d)", attempt)
		}
		if attempt <= failures {
			h.repairResult <- errors.New("refresh Google cookies: query cookies: database disk image is malformed")
			continue
		}
		h.repairResult <- nil
	}
	h.awaitTransport(t, 1).emit(&gmproto.Conversation{ConversationID: "ready-again"})
	online := h.awaitState(t, "repaired generation 2 online", func(snapshot bridge.Snapshot) bool {
		return snapshot.State == bridge.StateOnline && snapshot.Generation == 2
	})
	if online.LastRepairOutcome != bridge.RepairOutcomeSucceeded {
		t.Fatalf("last repair outcome = %q, want succeeded", online.LastRepairOutcome)
	}
	h.mu.Lock()
	repairs, starts := h.repairs, len(h.transports)
	repairedAt := append([]time.Time(nil), h.repairedAt...)
	h.mu.Unlock()
	if repairs != failures+1 {
		t.Fatalf("repair attempts = %d, want %d failures then one success", repairs, failures+1)
	}
	if starts != 2 {
		t.Fatalf("generations started = %d, want 2 (none before the repair worked)", starts)
	}
	for i := 1; i < len(repairedAt); i++ {
		if gap := repairedAt[i].Sub(repairedAt[i-1]); gap < 5*time.Millisecond {
			t.Fatalf("repair %d retried %s after the last, under the 5ms floor", i+1, gap)
		}
	}
}

func (h *repairHealHarness) awaitTransport(t *testing.T, index int) *fakeTransport {
	t.Helper()
	deadline := time.Now().Add(repairHealTimeout)
	for time.Now().Before(deadline) {
		if transport := h.transport(index); transport != nil && transport.currentHandler() != nil {
			return transport
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("generation %d never started; supervisor %+v", index+1, h.supervisor.Snapshot())
	return nil
}

func (h *repairHealHarness) awaitState(
	t *testing.T,
	description string,
	predicate func(bridge.Snapshot) bool,
) bridge.Snapshot {
	t.Helper()
	deadline := time.Now().Add(repairHealTimeout)
	for {
		snapshot := h.supervisor.Snapshot()
		if predicate(snapshot) {
			return snapshot
		}
		if snapshot.State == bridge.StateBlocked {
			t.Fatalf("parked while waiting for %s: %+v", description, snapshot)
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s; last snapshot %+v", description, snapshot)
		}
		time.Sleep(time.Millisecond)
	}
}

type repairHealClock struct{}

func (repairHealClock) Now() time.Time { return time.Now() }

func (repairHealClock) NewTimer(delay time.Duration) bridge.Timer {
	return repairHealTimer{timer: time.NewTimer(delay)}
}

type repairHealTimer struct{ timer *time.Timer }

func (t repairHealTimer) C() <-chan time.Time { return t.timer.C }
func (t repairHealTimer) Stop() bool          { return t.timer.Stop() }

type repairHealRandom struct {
	mu sync.Mutex
	r  *rand.Rand
}

func (r *repairHealRandom) Int63n(n int64) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.r.Int63n(n)
}
