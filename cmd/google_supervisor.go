package cmd

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"

	"github.com/maxghenis/openmessage/internal/app"
	"github.com/maxghenis/openmessage/internal/bridge"
)

const (
	googleAccountID             = "google-primary"
	googleSupervisorStopTimeout = 30 * time.Second
	// Ninety seconds rate-limits minutes-scale cookie-revocation churn while
	// remaining transparent to legitimate expiry, measured at >= about 14 minutes.
	googleCredentialRepairDefaultMinInterval = 90 * time.Second
)

const googleCredentialRepairMinIntervalEnv = "OPENMESSAGE_REPAIR_MIN_INTERVAL"

func googleSupervisorPolicy() bridge.Policy {
	return bridge.Policy{
		ConnectTimeout:     2*time.Minute + 30*time.Second,
		ProbeEvery:         time.Minute,
		ProbeTimeout:       75 * time.Second,
		LivenessTimeout:    12 * time.Minute,
		MinBackoff:         5 * time.Second,
		MaxBackoff:         5 * time.Minute,
		MaxSameFingerprint: 5,
		// A failed repair is retried no sooner than the repairer's own pace,
		// so retries never count as repairs_paced (the fast-revocation alarm).
		RepairRetryFloor: googleCredentialRepairMinInterval(),
	}
}

type googleWallClock struct{}

func (googleWallClock) Now() time.Time { return time.Now() }

func (googleWallClock) NewTimer(delay time.Duration) bridge.Timer {
	return &googleWallTimer{timer: time.NewTimer(delay)}
}

type googleWallTimer struct{ timer *time.Timer }

func (t *googleWallTimer) C() <-chan time.Time { return t.timer.C }
func (t *googleWallTimer) Stop() bool          { return t.timer.Stop() }

type googleRandom struct{}

func (googleRandom) Int63n(n int64) int64 { return rand.Int63n(n) }

type googleCredentialRepairer struct {
	sessionPath string
	canRepair   func() bool
	refresh     func(context.Context, string) error
	flagRepair  func()
	clearRepair func()
	minInterval time.Duration
	now         func() time.Time
	logger      zerolog.Logger

	mu           sync.Mutex
	paceGate     chan struct{}
	lastRepairAt time.Time
	pacedCount   atomic.Uint64
}

func newGoogleCredentialRepairer(
	sessionPath string,
	canRepair func() bool,
	refresh func(context.Context, string) error,
	flagRepair func(),
	clearRepair func(),
	logger zerolog.Logger,
) *googleCredentialRepairer {
	return &googleCredentialRepairer{
		sessionPath: sessionPath,
		canRepair:   canRepair,
		refresh:     refresh,
		flagRepair:  flagRepair,
		clearRepair: clearRepair,
		minInterval: googleCredentialRepairMinInterval(),
		logger:      logger,
	}
}

func googleCredentialRepairMinInterval() time.Duration {
	value := os.Getenv(googleCredentialRepairMinIntervalEnv)
	interval, err := time.ParseDuration(value)
	if value == "" || err != nil || interval <= 0 {
		return googleCredentialRepairDefaultMinInterval
	}
	return interval
}

// PacedRepairCount reports how many automatic Google credential repairs were
// delayed to honour the minimum repair interval. It stays 0 through the healthy
// ~15-minute heal cycle; a climbing count is the production signal that
// something is revoking the imported cookies within minutes. Also logged as it
// happens and surfaced as google.repairs_paced in /api/status.
func (r *googleCredentialRepairer) PacedRepairCount() uint64 {
	return r.pacedCount.Load()
}

func (r *googleCredentialRepairer) repairNow() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

func (r *googleCredentialRepairer) acquireRepairCooldown(
	ctx context.Context,
) (func(), error) {
	r.mu.Lock()
	if r.paceGate == nil {
		r.paceGate = make(chan struct{}, 1)
		r.paceGate <- struct{}{}
	}
	paceGate := r.paceGate
	r.mu.Unlock()

	select {
	case <-paceGate:
		if err := ctx.Err(); err != nil {
			paceGate <- struct{}{}
			return nil, err
		}
		return func() { paceGate <- struct{}{} }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (r *googleCredentialRepairer) waitForRepairCooldown(ctx context.Context) error {
	release, err := r.acquireRepairCooldown(ctx)
	if err != nil {
		return err
	}
	defer release()

	r.mu.Lock()
	now := r.repairNow()
	lastRepairAt := r.lastRepairAt
	r.mu.Unlock()
	elapsed := now.Sub(lastRepairAt)
	if r.minInterval > 0 && !lastRepairAt.IsZero() && elapsed < r.minInterval {
		wait := r.minInterval - elapsed
		paced := r.pacedCount.Add(1)
		r.logger.Warn().
			Dur("wait", wait).
			Dur("since_last_repair", elapsed).
			Dur("min_interval", r.minInterval).
			Uint64("paced_total", paced).
			Msg("Delaying Google credential repair — repairs requested faster than the minimum interval (cookies may be revoked within minutes)")

		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return ctx.Err()
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}

	r.mu.Lock()
	r.lastRepairAt = r.repairNow()
	r.mu.Unlock()
	return nil
}

func (r *googleCredentialRepairer) RepairCredentials(
	ctx context.Context,
	_ string,
	_ bridge.OpError,
) error {
	if err := r.waitForRepairCooldown(ctx); err != nil {
		return err
	}

	if r.canRepair == nil || !r.canRepair() || r.refresh == nil {
		// Terminal: without a refresh mechanism no retry can succeed, so this
		// is the one repair failure that asks for a re-pair.
		if r.flagRepair != nil {
			r.flagRepair()
		}
		return bridge.OpError{
			Class:       bridge.FailureUpgradeRequired,
			Operation:   "refresh_google_session_cookies",
			Fingerprint: "google_cookie_refresh_unavailable",
			Cause:       errors.New("Google cookie refresh is unavailable; pair again"),
		}
	}

	// A failed refresh is recoverable (Chrome, the keychain, a cookie-DB
	// copy, or the session write can each fail once): the supervisor retries
	// it with backoff, so it must not raise needs_repair.
	repairCtx, cancel := context.WithTimeout(r.logger.WithContext(ctx), googleCookieRefreshTimeout)
	defer cancel()
	if err := r.refresh(repairCtx, r.sessionPath); err != nil {
		return fmt.Errorf("refresh Google session cookies: %w", err)
	}
	if r.clearRepair != nil {
		r.clearRepair()
	}
	return nil
}

// googleSupervisorTransitionLogger logs the Google supervisor's lifecycle
// transitions, and every credential-repair attempt and result with its
// error. Before it, a failed repair parked the account with no log line at
// all (2026-10-22).
func googleSupervisorTransitionLogger(logger zerolog.Logger) bridge.SnapshotObserver {
	return func(previous, current bridge.Snapshot) {
		if current.LastRepairOutcome == bridge.RepairOutcomeRunning &&
			!current.LastRepairStartedAt.Equal(previous.LastRepairStartedAt) {
			logger.Info().
				Int("attempt", current.RepairAttempts).
				Str("trigger_class", string(current.ErrorClass)).
				Str("trigger", current.ErrorFingerprint).
				Uint64("generation", uint64(current.Generation)).
				Msg("Google credential repair: refreshing session cookies")
		}
		if previous.LastRepairOutcome == bridge.RepairOutcomeRunning &&
			current.LastRepairOutcome != bridge.RepairOutcomeRunning {
			took := current.LastRepairFinishedAt.Sub(current.LastRepairStartedAt)
			switch current.LastRepairOutcome {
			case bridge.RepairOutcomeSucceeded:
				logger.Info().
					Int("attempt", current.RepairAttempts).
					Dur("took", took).
					Msg("Google credential repair succeeded; reconnecting")
			case bridge.RepairOutcomeRetrying:
				logger.Warn().
					Int("attempt", current.RepairAttempts).
					Dur("took", took).
					Str("error", current.LastRepairError).
					Time("retry_at", current.RetryAt).
					Dur("retry_in", current.RetryAt.Sub(current.LastRepairFinishedAt)).
					Msg("Google credential repair failed; will retry")
			case bridge.RepairOutcomeBlocked:
				logger.Error().
					Int("attempt", current.RepairAttempts).
					Dur("took", took).
					Str("error", current.LastRepairError).
					Str("block_reason", current.BlockReason).
					Msg("Google credential repair failed and cannot be retried automatically; Google Messages is parked until Reconnect or re-pair")
			}
		}
		if current.State == previous.State {
			return
		}
		if current.State == bridge.StateBlocked &&
			!(previous.LastRepairOutcome == bridge.RepairOutcomeRunning &&
				current.LastRepairOutcome == bridge.RepairOutcomeBlocked) {
			logger.Error().
				Str("block_reason", current.BlockReason).
				Str("error_class", string(current.ErrorClass)).
				Str("error", current.ErrorFingerprint).
				Int("repair_attempts", current.RepairAttempts).
				Str("last_repair_error", current.LastRepairError).
				Msg("Google Messages supervisor parked; it will not retry until Reconnect")
			return
		}
		event := logger.Debug()
		if previous.State == bridge.StateBlocked {
			event = logger.Info()
		}
		event.
			Str("from", string(previous.State)).
			Str("to", string(current.State)).
			Uint64("generation", uint64(current.Generation)).
			Str("error_class", string(current.ErrorClass)).
			Str("error", current.ErrorFingerprint).
			Msg("Google Messages supervisor state changed")
	}
}

// googleSupervisorStatus renders a supervisor snapshot for /api/status.
func googleSupervisorStatus(snapshot bridge.Snapshot) app.GoogleSupervisorStatus {
	return app.GoogleSupervisorStatus{
		State:            string(snapshot.State),
		Generation:       uint64(snapshot.Generation),
		ErrorClass:       string(snapshot.ErrorClass),
		ErrorFingerprint: snapshot.ErrorFingerprint,
		BlockReason:      snapshot.BlockReason,
		RetryAtMS:        unixMilliOrZero(snapshot.RetryAt),
		Repair: app.GoogleRepairStatus{
			Attempts:       snapshot.RepairAttempts,
			LastStartedMS:  unixMilliOrZero(snapshot.LastRepairStartedAt),
			LastFinishedMS: unixMilliOrZero(snapshot.LastRepairFinishedAt),
			LastOutcome:    string(snapshot.LastRepairOutcome),
			LastError:      snapshot.LastRepairError,
		},
	}
}

func unixMilliOrZero(at time.Time) int64 {
	if at.IsZero() {
		return 0
	}
	return at.UnixMilli()
}

type googleSupervisorControl struct {
	supervisor    *bridge.Supervisor
	newSupervisor func() (*bridge.Supervisor, error)
	sessionPath   string

	mu                sync.Mutex
	inputFingerprint  string
	supervisorStopped bool
	stopping          bool
	closed            bool
}

func newGoogleSupervisorControl(
	supervisor *bridge.Supervisor,
	sessionPath string,
	newSupervisor func() (*bridge.Supervisor, error),
) *googleSupervisorControl {
	fingerprint, _ := googleSessionFingerprint(sessionPath)
	return &googleSupervisorControl{
		supervisor:       supervisor,
		newSupervisor:    newSupervisor,
		sessionPath:      sessionPath,
		inputFingerprint: fingerprint,
	}
}

func (c *googleSupervisorControl) Reconnect() error {
	c.mu.Lock()
	if c.closed || c.stopping {
		c.mu.Unlock()
		return bridge.ErrSupervisorStopped
	}
	if c.supervisorStopped {
		fingerprint, err := googleSessionFingerprint(c.sessionPath)
		if err != nil {
			c.mu.Unlock()
			return errors.New("Google Messages must be paired before reconnecting")
		}
		if c.newSupervisor == nil {
			c.mu.Unlock()
			return errors.New("Google Messages supervisor cannot be restarted")
		}
		supervisor, err := c.newSupervisor()
		if err != nil {
			c.mu.Unlock()
			return fmt.Errorf("rebuild Google Messages supervisor: %w", err)
		}
		c.supervisor = supervisor
		c.inputFingerprint = fingerprint
		c.supervisorStopped = false
	}
	supervisor := c.supervisor
	inputFingerprint := c.inputFingerprint
	c.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), googleSupervisorPolicy().ConnectTimeout)
	defer cancel()

	snapshot := supervisor.Snapshot()
	switch snapshot.State {
	case bridge.StateStopped:
		err := supervisor.Start(ctx, bridge.StartRequest{})
		if errors.Is(err, bridge.ErrSupervisorBusy) {
			return nil
		}
		return err
	case bridge.StateBlocked:
		fingerprint, err := googleSessionFingerprint(c.sessionPath)
		if err != nil {
			return err
		}
		changed := fingerprint != inputFingerprint
		if !changed {
			return supervisor.RetryBlocked(ctx)
		}
		if err := supervisor.InputsChanged(ctx, fingerprint); err != nil {
			return err
		}
		c.mu.Lock()
		if c.supervisor == supervisor && !c.supervisorStopped {
			c.inputFingerprint = fingerprint
		}
		c.mu.Unlock()
		return nil
	case bridge.StateRepairingCredentials:
		// A failed repair waiting out its backoff is tried again now (the
		// repairer's pacing still applies); a running one is already owned.
		err := supervisor.RetryCredentialRepairNow(ctx)
		if errors.Is(err, bridge.ErrSupervisorBusy) || errors.Is(err, bridge.ErrNoRepairScheduled) {
			return nil
		}
		return err
	case bridge.StateOnline, bridge.StateConnecting, bridge.StateDegraded,
		bridge.StateBackoff:
		// A reconnect is already owned by this supervisor. Treat a duplicate
		// button press as successful admission, never a second owner.
		return nil
	case bridge.StateUnpaired:
		return errors.New("Google Messages must be paired before reconnecting")
	case bridge.StateStopping:
		return bridge.ErrSupervisorStopped
	default:
		return fmt.Errorf("Google Messages cannot reconnect from supervisor state %q", snapshot.State)
	}
}

// Snapshot reads the current supervisor's state, following a rebuild after
// unpair. ok is false once the control has stopped for good.
func (c *googleSupervisorControl) Snapshot() (bridge.Snapshot, bool) {
	c.mu.Lock()
	supervisor := c.supervisor
	closed := c.closed
	c.mu.Unlock()
	if supervisor == nil || closed {
		return bridge.Snapshot{}, false
	}
	return supervisor.Snapshot(), true
}

func (c *googleSupervisorControl) StopAndUnpair(unpair func() error) error {
	c.mu.Lock()
	if c.closed || c.stopping {
		c.mu.Unlock()
		return bridge.ErrSupervisorStopped
	}
	c.stopping = true
	supervisor := c.supervisor
	c.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), googleSupervisorStopTimeout)
	defer cancel()
	stopErr := supervisor.Stop(ctx)
	if stopErr != nil {
		// Stop is terminal once requested, but a timed-out caller does not prove
		// that the old adapter generation has joined yet. Keep reconnects gated
		// until it has, so a replacement supervisor can never overlap it.
		go c.awaitStoppedSupervisor(supervisor)
		return fmt.Errorf("stop Google Messages connection: %w", stopErr)
	}
	unpairErr := unpair()

	c.mu.Lock()
	c.supervisorStopped = true
	c.stopping = false
	c.mu.Unlock()
	return unpairErr
}

func (c *googleSupervisorControl) awaitStoppedSupervisor(supervisor *bridge.Supervisor) {
	_ = supervisor.Stop(context.Background())
	c.mu.Lock()
	if c.supervisor == supervisor && !c.closed {
		c.supervisorStopped = true
		c.stopping = false
	}
	c.mu.Unlock()
}

func (c *googleSupervisorControl) Stop(ctx context.Context) error {
	c.mu.Lock()
	if c.supervisor == nil {
		c.closed = true
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.stopping = true
	supervisor := c.supervisor
	c.mu.Unlock()

	err := supervisor.Stop(ctx)
	c.mu.Lock()
	c.supervisorStopped = true
	c.stopping = false
	c.mu.Unlock()
	return err
}

func googleSessionFingerprint(sessionPath string) (string, error) {
	data, err := os.ReadFile(sessionPath)
	if err != nil {
		return "", fmt.Errorf("read Google session fingerprint: %w", err)
	}
	sum := sha256.Sum256(data)
	return fmt.Sprintf("sha256:%x", sum[:]), nil
}

func waitForGoogleOnline(ctx context.Context, supervisor *bridge.Supervisor) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		snapshot := supervisor.Snapshot()
		switch snapshot.State {
		case bridge.StateOnline:
			return nil
		case bridge.StateBlocked, bridge.StateStopped, bridge.StateUnpaired:
			return fmt.Errorf(
				"Google Messages supervisor entered %s (%s: %s)",
				snapshot.State,
				snapshot.ErrorClass,
				snapshot.ErrorFingerprint,
			)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// startGoogleBackfill preserves the pre-supervisor startup modes, but is only
// called after Run.Ready has put the Google supervisor online.
func startGoogleBackfill(a *app.App, logger zerolog.Logger) {
	runShallowBackfill := func() {
		if err := a.Backfill(); err != nil {
			logger.Warn().Err(err).Msg("Backfill failed")
		}
	}
	switch startupBackfillMode() {
	case "off":
		logger.Info().Msg("Startup backfill disabled")
	case "deep":
		logger.Info().Msg("Starting deep startup backfill")
		a.DeepBackfill()
	case "shallow":
		runShallowBackfill()
	default:
		smsCount, err := a.Store.MessageCount("sms")
		if err != nil {
			logger.Warn().Err(err).Msg("Failed to inspect local SMS cache; falling back to shallow backfill")
			runShallowBackfill()
		} else if smsCount == 0 {
			logger.Info().Msg("No cached SMS history found; starting deep startup backfill")
			a.DeepBackfill()
		} else {
			runShallowBackfill()
		}
	}
}
