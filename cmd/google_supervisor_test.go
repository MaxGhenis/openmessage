package cmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/maxghenis/openmessage/internal/bridge"
)

func TestGoogleSupervisorCredentialRepairControlsGenerationStart(t *testing.T) {
	tests := []struct {
		name             string
		canRepair        bool
		refreshErr       error
		wantRefreshCalls int32
		wantStarts       int
		wantState        bridge.State
		wantFlagCalls    int32
		wantClearCalls   int32
	}{
		{
			name:          "unavailable refresh parks without reconnect",
			wantStarts:    1,
			wantState:     bridge.StateBlocked,
			wantFlagCalls: 1,
		},
		{
			name:             "failed refresh parks without reconnect",
			canRepair:        true,
			refreshErr:       errors.New("Chrome cookie database unavailable"),
			wantRefreshCalls: 1,
			wantStarts:       1,
			wantState:        bridge.StateBlocked,
			wantFlagCalls:    1,
		},
		{
			name:             "successful refresh starts exactly one new generation",
			canRepair:        true,
			wantRefreshCalls: 1,
			wantStarts:       2,
			wantState:        bridge.StateOnline,
			wantClearCalls:   1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			lifecycle := &googleRepairTestLifecycle{}
			var refreshCalls atomic.Int32
			var flagCalls atomic.Int32
			var clearCalls atomic.Int32
			repairer := newGoogleCredentialRepairer(
				"session.json",
				func() bool { return tc.canRepair },
				func(context.Context, string) error {
					refreshCalls.Add(1)
					return tc.refreshErr
				},
				func() { flagCalls.Add(1) },
				func() { clearCalls.Add(1) },
			)
			supervisor, err := bridge.NewSupervisor(
				googleAccountID,
				bridge.PlatformGoogle,
				lifecycle,
				googleSupervisorPolicy(),
				googleWallClock{},
				googleRandom{},
				bridge.WithCredentialRepairer(repairer),
			)
			if err != nil {
				t.Fatalf("NewSupervisor() error = %v", err)
			}
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				if err := supervisor.Stop(ctx); err != nil {
					t.Errorf("Supervisor.Stop() error = %v", err)
				}
			})

			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			if err := supervisor.Start(ctx, bridge.StartRequest{}); err != nil {
				cancel()
				t.Fatalf("Start() error = %v", err)
			}
			cancel()
			awaitGoogleSupervisorState(t, supervisor, bridge.StateOnline)
			lifecycle.Run(0).Fail(bridge.OpError{
				Class:       bridge.FailureCredentialsExpired,
				Operation:   "listen",
				Fingerprint: "google_auth_expired",
				Cause:       errors.New("HTTP 401: invalid authentication credentials"),
			})

			if tc.wantStarts > 1 {
				awaitGoogleSupervisorGenerationState(t, supervisor, tc.wantState, bridge.Generation(tc.wantStarts))
			} else {
				awaitGoogleSupervisorState(t, supervisor, tc.wantState)
			}
			if got := lifecycle.StartCount(); got != tc.wantStarts {
				t.Fatalf("generation starts = %d, want %d", got, tc.wantStarts)
			}
			if got := refreshCalls.Load(); got != tc.wantRefreshCalls {
				t.Fatalf("refresh calls = %d, want %d", got, tc.wantRefreshCalls)
			}
			if got := flagCalls.Load(); got != tc.wantFlagCalls {
				t.Fatalf("needs_repair flag calls = %d, want %d", got, tc.wantFlagCalls)
			}
			if got := clearCalls.Load(); got != tc.wantClearCalls {
				t.Fatalf("repair-clear calls = %d, want %d", got, tc.wantClearCalls)
			}
		})
	}
}

func TestGoogleSupervisorManualReconnectUsesOwnedCommands(t *testing.T) {
	sessionPath := filepath.Join(t.TempDir(), "session.json")
	if err := os.WriteFile(sessionPath, []byte("session-v1"), 0o600); err != nil {
		t.Fatalf("write session: %v", err)
	}
	lifecycle := &googleRepairTestLifecycle{}
	supervisor, err := bridge.NewSupervisor(
		googleAccountID,
		bridge.PlatformGoogle,
		lifecycle,
		googleSupervisorPolicy(),
		googleWallClock{},
		googleRandom{},
	)
	if err != nil {
		t.Fatalf("NewSupervisor() error = %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := supervisor.Stop(ctx); err != nil {
			t.Errorf("Supervisor.Stop() error = %v", err)
		}
	})
	control := newGoogleSupervisorControl(supervisor, sessionPath, nil)

	if err := control.Reconnect(); err != nil {
		t.Fatalf("Reconnect() from stopped: %v", err)
	}
	awaitGoogleSupervisorGenerationState(t, supervisor, bridge.StateOnline, 1)

	terminal := func(run *googleRepairTestRun) {
		run.Fail(bridge.OpError{
			Class:       bridge.FailureUpgradeRequired,
			Operation:   "repair",
			Fingerprint: "google_manual_repair_required",
			Cause:       errors.New("pair again"),
		})
		awaitGoogleSupervisorState(t, supervisor, bridge.StateBlocked)
	}

	terminal(lifecycle.Run(0))
	if err := control.Reconnect(); err != nil {
		t.Fatalf("Reconnect() via RetryBlocked: %v", err)
	}
	awaitGoogleSupervisorGenerationState(t, supervisor, bridge.StateOnline, 2)

	terminal(lifecycle.Run(1))
	if err := os.WriteFile(sessionPath, []byte("session-v2"), 0o600); err != nil {
		t.Fatalf("replace session: %v", err)
	}
	if err := control.Reconnect(); err != nil {
		t.Fatalf("Reconnect() via InputsChanged: %v", err)
	}
	awaitGoogleSupervisorGenerationState(t, supervisor, bridge.StateOnline, 3)
	if got := lifecycle.StartCount(); got != 3 {
		t.Fatalf("generation starts = %d, want 3", got)
	}
}

func TestGoogleSupervisorControlReconnectAfterUnpairRebuildsSupervisor(t *testing.T) {
	sessionPath := filepath.Join(t.TempDir(), "session.json")
	if err := os.WriteFile(sessionPath, []byte("session-v1"), 0o600); err != nil {
		t.Fatalf("write initial session: %v", err)
	}

	lifecycle := &googleRepairTestLifecycle{}
	var supervisorCount atomic.Int32
	newSupervisor := func() (*bridge.Supervisor, error) {
		supervisorCount.Add(1)
		return bridge.NewSupervisor(
			googleAccountID,
			bridge.PlatformGoogle,
			lifecycle,
			googleSupervisorPolicy(),
			googleWallClock{},
			googleRandom{},
		)
	}
	firstSupervisor, err := newSupervisor()
	if err != nil {
		t.Fatalf("NewSupervisor() error = %v", err)
	}
	control := newGoogleSupervisorControl(firstSupervisor, sessionPath, newSupervisor)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := control.Stop(ctx); err != nil {
			t.Errorf("control.Stop() error = %v", err)
		}
	})

	if err := control.Reconnect(); err != nil {
		t.Fatalf("initial Reconnect() error = %v", err)
	}
	awaitGoogleSupervisorGenerationState(t, firstSupervisor, bridge.StateOnline, 1)

	if err := control.StopAndUnpair(func() error { return os.Remove(sessionPath) }); err != nil {
		t.Fatalf("StopAndUnpair() error = %v", err)
	}
	if err := os.WriteFile(sessionPath, []byte("session-v2"), 0o600); err != nil {
		t.Fatalf("write re-paired session: %v", err)
	}
	if err := control.Reconnect(); err != nil {
		t.Fatalf("Reconnect() after re-pair error = %v", err)
	}

	control.mu.Lock()
	secondSupervisor := control.supervisor
	control.mu.Unlock()
	if secondSupervisor == firstSupervisor {
		t.Fatal("Reconnect() reused the terminal supervisor after re-pair")
	}
	awaitGoogleSupervisorGenerationState(t, secondSupervisor, bridge.StateOnline, 1)
	if got := lifecycle.StartCount(); got != 2 {
		t.Fatalf("generation starts = %d, want 2", got)
	}
	if got := supervisorCount.Load(); got != 2 {
		t.Fatalf("supervisors constructed = %d, want 2", got)
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), time.Second)
	if err := control.Stop(shutdownCtx); err != nil {
		shutdownCancel()
		t.Fatalf("control.Stop() error = %v", err)
	}
	shutdownCancel()
	if err := control.Reconnect(); !errors.Is(err, bridge.ErrSupervisorStopped) {
		t.Fatalf("Reconnect() after terminal control stop error = %v, want ErrSupervisorStopped", err)
	}
	if got := supervisorCount.Load(); got != 2 {
		t.Fatalf("supervisors after terminal stop = %d, want 2", got)
	}
}

type googleRepairTestLifecycle struct {
	mu   sync.Mutex
	runs []*googleRepairTestRun
}

func (l *googleRepairTestLifecycle) Start(
	_ context.Context,
	_ bridge.StartRequest,
	_ bridge.ConnectionSink,
) (bridge.Run, error) {
	run := &googleRepairTestRun{ready: make(chan struct{}), done: make(chan error, 1)}
	close(run.ready)
	l.mu.Lock()
	l.runs = append(l.runs, run)
	l.mu.Unlock()
	return run, nil
}

func (l *googleRepairTestLifecycle) StartCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.runs)
}

func (l *googleRepairTestLifecycle) Run(index int) *googleRepairTestRun {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.runs[index]
}

type googleRepairTestRun struct {
	ready chan struct{}
	done  chan error
	once  sync.Once
}

func (r *googleRepairTestRun) Ready() <-chan struct{} { return r.ready }
func (r *googleRepairTestRun) Done() <-chan error     { return r.done }
func (r *googleRepairTestRun) Probe(context.Context) (bridge.Liveness, error) {
	return bridge.Liveness{AliveAt: time.Now(), Detail: "test"}, nil
}
func (r *googleRepairTestRun) Stop(context.Context) error { return nil }
func (r *googleRepairTestRun) Fail(err error) {
	r.once.Do(func() {
		r.done <- err
		close(r.done)
	})
}

func awaitGoogleSupervisorState(t *testing.T, supervisor *bridge.Supervisor, want bridge.State) bridge.Snapshot {
	return awaitGoogleSupervisorGenerationState(t, supervisor, want, 0)
}

func awaitGoogleSupervisorGenerationState(
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
		if snapshot.State == want && (wantGeneration == 0 || snapshot.Generation == wantGeneration) {
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
