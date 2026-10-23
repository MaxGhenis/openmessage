package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/quick"
	"time"

	"github.com/rs/zerolog"

	"github.com/maxghenis/openmessage/internal/bridge"
)

func TestGoogleSupervisorCredentialRepairControlsGenerationStart(t *testing.T) {
	tests := []struct {
		name             string
		canRepair        bool
		refreshFailures  int32
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
			// 2026-10-22: one failed refresh used to park Google for good.
			name:             "failed refresh is retried until it works, never flagging needs_repair",
			canRepair:        true,
			refreshFailures:  3,
			wantRefreshCalls: 4,
			wantStarts:       2,
			wantState:        bridge.StateOnline,
			wantClearCalls:   1,
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
			// Millisecond pacing so retries finish inside the test; the policy
			// and the repairer both read it, as in production.
			t.Setenv(googleCredentialRepairMinIntervalEnv, "5ms")
			lifecycle := &googleRepairTestLifecycle{}
			var refreshCalls atomic.Int32
			var flagCalls atomic.Int32
			var clearCalls atomic.Int32
			repairer := newGoogleCredentialRepairer(
				"session.json",
				func() bool { return tc.canRepair },
				func(context.Context, string) error {
					if refreshCalls.Add(1) <= tc.refreshFailures {
						return errors.New("Chrome cookie database unavailable")
					}
					return nil
				},
				func() { flagCalls.Add(1) },
				func() { clearCalls.Add(1) },
				zerolog.Nop(),
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
			if got := repairer.PacedRepairCount(); got != 0 {
				t.Fatalf("paced repairs = %d, want 0: retries must wait out the pace themselves", got)
			}
		})
	}
}

// TestGoogleRepairRetriesNeverOutrunThePacingFloor runs the real repairer
// under the real supervisor on the wall clock for arbitrary numbers of failed
// refreshes and pacing intervals. For every case the refreshes are spaced at
// least the pacing interval apart, none is delayed by the pacing gate (so a
// retry never inflates repairs_paced, the fast-revocation alarm), needs_repair
// is never raised, and the account comes back online.
func TestGoogleRepairRetriesNeverOutrunThePacingFloor(t *testing.T) {
	property := func(failureCount, intervalMS uint8) bool {
		failures := int32(failureCount % 5)
		interval := time.Duration(intervalMS%10+2) * time.Millisecond
		t.Setenv(googleCredentialRepairMinIntervalEnv, interval.String())
		lifecycle := &googleRepairTestLifecycle{}
		var mu sync.Mutex
		var refreshedAt []time.Time
		var flagCalls atomic.Int32
		repairer := newGoogleCredentialRepairer(
			"session.json",
			func() bool { return true },
			func(context.Context, string) error {
				mu.Lock()
				defer mu.Unlock()
				refreshedAt = append(refreshedAt, time.Now())
				if int32(len(refreshedAt)) <= failures {
					return errors.New("read Chrome Safe Storage from keychain: exit status 51")
				}
				return nil
			},
			func() { flagCalls.Add(1) },
			func() {},
			zerolog.Nop(),
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
		defer func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_ = supervisor.Stop(ctx)
		}()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		err = supervisor.Start(ctx, bridge.StartRequest{})
		cancel()
		if err != nil {
			t.Fatalf("Start() error = %v", err)
		}
		awaitGoogleSupervisorState(t, supervisor, bridge.StateOnline)
		lifecycle.Run(0).Fail(bridge.OpError{
			Class:       bridge.FailureCredentialsExpired,
			Operation:   "ping",
			Fingerprint: "google_auth_expired",
			Cause:       errors.New("HTTP 401: invalid authentication credentials"),
		})
		awaitGoogleSupervisorGenerationStateWithin(t, supervisor, bridge.StateOnline, 2, 5*time.Second)

		mu.Lock()
		defer mu.Unlock()
		if got := int32(len(refreshedAt)); got != failures+1 {
			t.Logf("failures=%d interval=%s: refreshes = %d, want %d", failures, interval, got, failures+1)
			return false
		}
		for i := 1; i < len(refreshedAt); i++ {
			if gap := refreshedAt[i].Sub(refreshedAt[i-1]); gap < interval {
				t.Logf("failures=%d interval=%s: refresh %d came %s after the last", failures, interval, i+1, gap)
				return false
			}
		}
		if paced := repairer.PacedRepairCount(); paced != 0 {
			t.Logf("failures=%d interval=%s: %d retries hit the pacing gate", failures, interval, paced)
			return false
		}
		if flagged := flagCalls.Load(); flagged != 0 {
			t.Logf("failures=%d interval=%s: needs_repair raised %d times", failures, interval, flagged)
			return false
		}
		return true
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 16, Rand: rand.New(rand.NewSource(1022))}); err != nil {
		t.Fatalf("repair pacing property: %v", err)
	}
}

func TestGoogleSupervisorTransitionLoggerRecordsEveryRepairAttemptAndResult(t *testing.T) {
	var logs bytes.Buffer
	var logsMu sync.Mutex
	logger := zerolog.New(zerologSyncWriter{w: &logs, mu: &logsMu})
	t.Setenv(googleCredentialRepairMinIntervalEnv, "5ms")
	lifecycle := &googleRepairTestLifecycle{}
	var refreshCalls atomic.Int32
	repairer := newGoogleCredentialRepairer(
		"session.json",
		func() bool { return true },
		func(context.Context, string) error {
			if refreshCalls.Add(1) == 1 {
				return errors.New("chrome cookie DB not found under /profile")
			}
			return nil
		},
		func() {},
		func() {},
		logger,
	)
	supervisor, err := bridge.NewSupervisor(
		googleAccountID,
		bridge.PlatformGoogle,
		lifecycle,
		googleSupervisorPolicy(),
		googleWallClock{},
		googleRandom{},
		bridge.WithCredentialRepairer(repairer),
		bridge.WithSnapshotObserver(googleSupervisorTransitionLogger(logger)),
	)
	if err != nil {
		t.Fatalf("NewSupervisor() error = %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = supervisor.Stop(ctx)
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
		Operation:   "send_text",
		Fingerprint: "google_auth_expired",
		Cause:       errors.New("get Google conversation: HTTP 401"),
	})
	awaitGoogleSupervisorGenerationState(t, supervisor, bridge.StateOnline, 2)

	logsMu.Lock()
	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	logsMu.Unlock()
	type entry = map[string]any
	var found []entry
	for _, line := range lines {
		var e entry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("parse log line %q: %v", line, err)
		}
		found = append(found, e)
	}
	want := []struct {
		level, message string
		check          func(entry) bool
	}{
		{"info", "Google credential repair: refreshing session cookies", func(e entry) bool {
			return e["attempt"] == float64(1) && e["trigger"] == "google_auth_expired"
		}},
		{"warn", "Google credential repair failed; will retry", func(e entry) bool {
			return e["attempt"] == float64(1) &&
				e["error"] == "refresh Google session cookies: chrome cookie DB not found under /profile" &&
				e["retry_in"] != nil
		}},
		{"info", "Google credential repair: refreshing session cookies", func(e entry) bool {
			return e["attempt"] == float64(2)
		}},
		{"info", "Google credential repair succeeded; reconnecting", func(e entry) bool {
			return e["attempt"] == float64(2)
		}},
	}
	next := 0
	for _, e := range found {
		if next < len(want) && e["level"] == want[next].level && e["message"] == want[next].message &&
			want[next].check(e) {
			next++
		}
	}
	if next != len(want) {
		t.Fatalf("missing log %d (%s %q) in order; got:\n%s", next, want[next].level, want[next].message, strings.Join(lines, "\n"))
	}
}

type zerologSyncWriter struct {
	w  *bytes.Buffer
	mu *sync.Mutex
}

func (w zerologSyncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.w.Write(p)
}

func TestGoogleCredentialRepairMinInterval(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  time.Duration
	}{
		{
			name: "unset",
			want: googleCredentialRepairDefaultMinInterval,
		},
		{
			name:  "invalid",
			value: "soon",
			want:  googleCredentialRepairDefaultMinInterval,
		},
		{
			name:  "non-positive",
			value: "0s",
			want:  googleCredentialRepairDefaultMinInterval,
		},
		{
			name:  "configured",
			value: "5m",
			want:  5 * time.Minute,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(googleCredentialRepairMinIntervalEnv, tc.value)
			if got := googleCredentialRepairMinInterval(); got != tc.want {
				t.Fatalf("googleCredentialRepairMinInterval() = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestGoogleCredentialRepairCooldownFirstRepairProceedsImmediately(t *testing.T) {
	clock := &googleCredentialRepairTestClock{}
	var refreshCalls atomic.Int32
	repairer := &googleCredentialRepairer{
		sessionPath: "session.json",
		canRepair:   func() bool { return true },
		refresh: func(context.Context, string) error {
			refreshCalls.Add(1)
			return nil
		},
		minInterval: time.Hour,
		now:         clock.Now,
	}

	if err := repairer.RepairCredentials(context.Background(), "", bridge.OpError{}); err != nil {
		t.Fatalf("RepairCredentials() error = %v", err)
	}
	if got := refreshCalls.Load(); got != 1 {
		t.Fatalf("refresh calls = %d, want 1", got)
	}
	if got := repairer.PacedRepairCount(); got != 0 {
		t.Fatalf("paced repair count = %d, want 0", got)
	}
}

func TestGoogleCredentialRepairCooldownPacesTooSoonRepair(t *testing.T) {
	const minInterval = 50 * time.Millisecond
	clock := &googleCredentialRepairTestClock{}
	var refreshMu sync.Mutex
	var refreshTimes []time.Time
	var logs bytes.Buffer
	repairer := &googleCredentialRepairer{
		sessionPath: "session.json",
		canRepair:   func() bool { return true },
		refresh: func(context.Context, string) error {
			refreshMu.Lock()
			refreshTimes = append(refreshTimes, clock.Now())
			refreshMu.Unlock()
			return nil
		},
		minInterval: minInterval,
		now:         clock.Now,
		logger:      zerolog.New(&logs),
	}

	if err := repairer.RepairCredentials(context.Background(), "", bridge.OpError{}); err != nil {
		t.Fatalf("first RepairCredentials() error = %v", err)
	}
	repairer.mu.Lock()
	firstRepairAt := repairer.lastRepairAt
	repairer.mu.Unlock()

	if err := repairer.RepairCredentials(context.Background(), "", bridge.OpError{}); err != nil {
		t.Fatalf("second RepairCredentials() error = %v", err)
	}
	refreshMu.Lock()
	gotRefreshTimes := append([]time.Time(nil), refreshTimes...)
	refreshMu.Unlock()
	if len(gotRefreshTimes) != 2 {
		t.Fatalf("refresh calls = %d, want 2", len(gotRefreshTimes))
	}
	if elapsed := gotRefreshTimes[1].Sub(firstRepairAt); elapsed < minInterval {
		t.Fatalf("second refresh elapsed from first repair = %s, want >= %s", elapsed, minInterval)
	}
	if got := repairer.PacedRepairCount(); got != 1 {
		t.Fatalf("paced repair count = %d, want 1", got)
	}
	// Pacing is the fast-cookie-revocation signal, so it must not be silent in
	// production: a delayed repair logs the wait and the running total.
	var logged map[string]any
	if err := json.Unmarshal([]byte(logs.String()), &logged); err != nil {
		t.Fatalf("parse paced repair log %q: %v", logs.String(), err)
	}
	if level, _ := logged["level"].(string); level != "warn" {
		t.Fatalf("paced repair log level = %v, want warn", logged["level"])
	}
	if total, _ := logged["paced_total"].(float64); total != 1 {
		t.Fatalf("paced repair log paced_total = %v, want 1", logged["paced_total"])
	}
	if _, ok := logged["wait"]; !ok {
		t.Fatalf("paced repair log has no wait field: %v", logged)
	}
}

func TestGoogleCredentialRepairCooldownAfterIntervalProceedsImmediately(t *testing.T) {
	const minInterval = time.Minute
	clock := &googleCredentialRepairTestClock{}
	var refreshCalls atomic.Int32
	repairer := &googleCredentialRepairer{
		sessionPath: "session.json",
		canRepair:   func() bool { return true },
		refresh: func(context.Context, string) error {
			refreshCalls.Add(1)
			return nil
		},
		minInterval: minInterval,
		now:         clock.Now,
	}

	if err := repairer.RepairCredentials(context.Background(), "", bridge.OpError{}); err != nil {
		t.Fatalf("first RepairCredentials() error = %v", err)
	}
	clock.Advance(minInterval)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := repairer.RepairCredentials(ctx, "", bridge.OpError{}); err != nil {
		t.Fatalf("second RepairCredentials() after interval error = %v", err)
	}
	if got := refreshCalls.Load(); got != 2 {
		t.Fatalf("refresh calls = %d, want 2", got)
	}
	if got := repairer.PacedRepairCount(); got != 0 {
		t.Fatalf("paced repair count = %d, want 0", got)
	}
}

func TestGoogleCredentialRepairCooldownCancellationDoesNotFlagOrRefresh(t *testing.T) {
	clock := &googleCredentialRepairTestClock{}
	var refreshCalls atomic.Int32
	var flagCalls atomic.Int32
	repairer := &googleCredentialRepairer{
		sessionPath: "session.json",
		canRepair:   func() bool { return true },
		refresh: func(context.Context, string) error {
			refreshCalls.Add(1)
			return nil
		},
		flagRepair:  func() { flagCalls.Add(1) },
		minInterval: time.Hour,
		now:         clock.Now,
	}

	if err := repairer.RepairCredentials(context.Background(), "", bridge.OpError{}); err != nil {
		t.Fatalf("first RepairCredentials() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- repairer.RepairCredentials(ctx, "", bridge.OpError{})
	}()
	awaitGooglePacedRepairCount(t, repairer, 1)
	if got := refreshCalls.Load(); got != 1 {
		t.Fatalf("refresh calls before cancellation = %d, want 1", got)
	}

	queuedBaseCtx, queuedCancel := context.WithCancel(context.Background())
	queuedCtx := &googleRepairObservedContext{
		Context:    queuedBaseCtx,
		doneCalled: make(chan struct{}),
	}
	queuedErrCh := make(chan error, 1)
	go func() {
		queuedErrCh <- repairer.RepairCredentials(queuedCtx, "", bridge.OpError{})
	}()
	select {
	case <-queuedCtx.doneCalled:
	case <-time.After(time.Second):
		t.Fatal("queued RepairCredentials() did not begin waiting for admission")
	}
	queuedCancel()
	select {
	case err := <-queuedErrCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("queued RepairCredentials() error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("queued RepairCredentials() did not return after cancellation")
	}

	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("RepairCredentials() error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("RepairCredentials() did not return after cancellation")
	}
	if got := refreshCalls.Load(); got != 1 {
		t.Fatalf("refresh calls after cancellation = %d, want 1", got)
	}
	if got := flagCalls.Load(); got != 0 {
		t.Fatalf("needs_repair flag calls = %d, want 0", got)
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

func TestGoogleSupervisorControlReconnectRetriesAWaitingRepairNow(t *testing.T) {
	// A long pace, so only an expedited retry can explain a second attempt.
	t.Setenv(googleCredentialRepairMinIntervalEnv, "1h")
	sessionPath := filepath.Join(t.TempDir(), "session.json")
	if err := os.WriteFile(sessionPath, []byte("session-v1"), 0o600); err != nil {
		t.Fatalf("write session: %v", err)
	}
	lifecycle := &googleRepairTestLifecycle{}
	var refreshCalls atomic.Int32
	attempts := make(chan int32, 4)
	repairer := bridge.CredentialRepairFunc(func(context.Context, string, bridge.OpError) error {
		n := refreshCalls.Add(1)
		attempts <- n
		if n == 1 {
			return errors.New("keychain locked")
		}
		return nil
	})
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
		_ = supervisor.Stop(ctx)
	})
	control := newGoogleSupervisorControl(supervisor, sessionPath, nil)
	if err := control.Reconnect(); err != nil {
		t.Fatalf("Reconnect() from stopped: %v", err)
	}
	awaitGoogleSupervisorGenerationState(t, supervisor, bridge.StateOnline, 1)
	lifecycle.Run(0).Fail(bridge.OpError{
		Class:       bridge.FailureCredentialsExpired,
		Operation:   "listen",
		Fingerprint: "google_auth_expired",
		Cause:       errors.New("http 401 while polling"),
	})
	<-attempts
	waiting := awaitGoogleSupervisorState(t, supervisor, bridge.StateRepairingCredentials)
	for waiting.LastRepairOutcome != bridge.RepairOutcomeRetrying {
		time.Sleep(time.Millisecond)
		waiting = supervisor.Snapshot()
	}
	if time.Until(waiting.RetryAt) < 30*time.Minute {
		t.Fatalf("retry due in %s, want the hour-long pace", time.Until(waiting.RetryAt))
	}

	if err := control.Reconnect(); err != nil {
		t.Fatalf("Reconnect() while a repair waits: %v", err)
	}
	select {
	case n := <-attempts:
		if n != 2 {
			t.Fatalf("expedited attempt = %d, want 2", n)
		}
	case <-time.After(time.Second):
		t.Fatal("Reconnect() did not retry the waiting repair")
	}
	awaitGoogleSupervisorGenerationState(t, supervisor, bridge.StateOnline, 2)
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
	return awaitGoogleSupervisorGenerationStateWithin(t, supervisor, want, wantGeneration, time.Second)
}

func awaitGoogleSupervisorGenerationStateWithin(
	t *testing.T,
	supervisor *bridge.Supervisor,
	want bridge.State,
	wantGeneration bridge.Generation,
	within time.Duration,
) bridge.Snapshot {
	t.Helper()
	deadline := time.NewTimer(within)
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

type googleCredentialRepairTestClock struct {
	offset atomic.Int64
}

type googleRepairObservedContext struct {
	context.Context
	doneCalled chan struct{}
	once       sync.Once
}

func (c *googleRepairObservedContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.doneCalled) })
	return c.Context.Done()
}

func (c *googleCredentialRepairTestClock) Now() time.Time {
	return time.Now().Add(time.Duration(c.offset.Load()))
}

func (c *googleCredentialRepairTestClock) Advance(delta time.Duration) {
	c.offset.Add(int64(delta))
}

func awaitGooglePacedRepairCount(
	t *testing.T,
	repairer *googleCredentialRepairer,
	want uint64,
) {
	t.Helper()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		if got := repairer.PacedRepairCount(); got == want {
			return
		}
		select {
		case <-deadline.C:
			t.Fatalf("paced repair count = %d, want %d", repairer.PacedRepairCount(), want)
		case <-ticker.C:
		}
	}
}
