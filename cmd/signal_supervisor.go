package cmd

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"time"

	"github.com/maxghenis/openmessage/internal/bridge"
)

const (
	signalAccountID                = "signal-primary"
	signalSupervisorStopTimeout    = 30 * time.Second
	signalSupervisorCommandTimeout = 30 * time.Second
)

func signalSupervisorPolicy() bridge.Policy {
	return bridge.Policy{
		// An unpaired Start contains the existing interactive QR link flow.
		// signal-cli normally expires that flow itself; this outer deadline is
		// only a leak backstop and is intentionally not a reconnect ticker.
		ConnectTimeout:     30 * time.Minute,
		ProbeEvery:         30 * time.Second,
		ProbeTimeout:       30 * time.Second,
		LivenessTimeout:    2 * time.Minute,
		MinBackoff:         5 * time.Second,
		MaxBackoff:         5 * time.Minute,
		MaxSameFingerprint: 5,
	}
}

type signalWallClock struct{}

func (signalWallClock) Now() time.Time { return time.Now() }

func (signalWallClock) NewTimer(delay time.Duration) bridge.Timer {
	return &signalWallTimer{timer: time.NewTimer(delay)}
}

type signalWallTimer struct{ timer *time.Timer }

func (t *signalWallTimer) C() <-chan time.Time { return t.timer.C }
func (t *signalWallTimer) Stop() bool          { return t.timer.Stop() }

type signalRandom struct{}

func (signalRandom) Int63n(n int64) int64 { return rand.Int63n(n) }

type signalSupervisorControl struct {
	supervisor       *bridge.Supervisor
	newSupervisor    func() (*bridge.Supervisor, error)
	inputFingerprint func() string

	mu                sync.Mutex
	lastFingerprint   string
	supervisorStopped bool
	stopping          bool
	closed            bool
}

func newSignalSupervisorControl(
	supervisor *bridge.Supervisor,
	newSupervisor func() (*bridge.Supervisor, error),
	inputFingerprint func() string,
) *signalSupervisorControl {
	fingerprint := ""
	if inputFingerprint != nil {
		fingerprint = inputFingerprint()
	}
	return &signalSupervisorControl{
		supervisor:       supervisor,
		newSupervisor:    newSupervisor,
		inputFingerprint: inputFingerprint,
		lastFingerprint:  fingerprint,
	}
}

// Connect admits one user-owned start/retry. All automatic retry remains in
// Supervisor backoff, and active states make duplicate UI clicks no-ops.
func (c *signalSupervisorControl) Connect() error {
	c.mu.Lock()
	if c.closed || c.stopping {
		c.mu.Unlock()
		return bridge.ErrSupervisorStopped
	}
	if c.supervisorStopped {
		if c.newSupervisor == nil {
			c.mu.Unlock()
			return errors.New("Signal supervisor cannot be restarted")
		}
		supervisor, err := c.newSupervisor()
		if err != nil {
			c.mu.Unlock()
			return fmt.Errorf("rebuild Signal supervisor: %w", err)
		}
		c.supervisor = supervisor
		c.supervisorStopped = false
		if c.inputFingerprint != nil {
			c.lastFingerprint = c.inputFingerprint()
		}
	}
	supervisor := c.supervisor
	lastFingerprint := c.lastFingerprint
	c.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), signalSupervisorCommandTimeout)
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
		fingerprint := lastFingerprint
		if c.inputFingerprint != nil {
			fingerprint = c.inputFingerprint()
		}
		if fingerprint == lastFingerprint {
			return supervisor.RetryBlocked(ctx)
		}
		if err := supervisor.InputsChanged(ctx, fingerprint); err != nil {
			return err
		}
		c.mu.Lock()
		if c.supervisor == supervisor && !c.supervisorStopped {
			c.lastFingerprint = fingerprint
		}
		c.mu.Unlock()
		return nil
	case bridge.StateOnline, bridge.StateConnecting, bridge.StateDegraded,
		bridge.StateBackoff, bridge.StateRepairingCredentials, bridge.StatePairing:
		return nil
	case bridge.StateUnpaired:
		// QR expiry is deliberately terminal and non-retrying. A new pairing
		// attempt is admitted only by this explicit user action, on a fresh
		// supervisor because Stop is terminal.
		return c.restartUnpaired(supervisor)
	case bridge.StateStopping:
		return bridge.ErrSupervisorStopped
	default:
		return fmt.Errorf("Signal cannot connect from supervisor state %q", snapshot.State)
	}
}

func (c *signalSupervisorControl) restartUnpaired(old *bridge.Supervisor) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return bridge.ErrSupervisorStopped
	}
	if c.stopping || c.supervisor != old {
		c.mu.Unlock()
		return nil
	}
	c.stopping = true
	c.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), signalSupervisorStopTimeout)
	defer cancel()
	if err := old.Stop(ctx); err != nil {
		go c.awaitStoppedSupervisor(old)
		return fmt.Errorf("stop expired Signal pairing: %w", err)
	}

	c.mu.Lock()
	if c.closed {
		c.supervisorStopped = true
		c.stopping = false
		c.mu.Unlock()
		return bridge.ErrSupervisorStopped
	}
	if c.newSupervisor == nil {
		c.supervisorStopped = true
		c.stopping = false
		c.mu.Unlock()
		return errors.New("Signal supervisor cannot be restarted")
	}
	supervisor, err := c.newSupervisor()
	if err != nil {
		c.supervisorStopped = true
		c.stopping = false
		c.mu.Unlock()
		return fmt.Errorf("rebuild Signal supervisor: %w", err)
	}
	c.supervisor = supervisor
	c.supervisorStopped = false
	c.stopping = false
	if c.inputFingerprint != nil {
		c.lastFingerprint = c.inputFingerprint()
	}
	c.mu.Unlock()

	startCtx, startCancel := context.WithTimeout(context.Background(), signalSupervisorCommandTimeout)
	defer startCancel()
	return supervisor.Start(startCtx, bridge.StartRequest{})
}

// StopAndUnpair joins the old poller generation before deleting signal-cli
// state. The next Connect rebuilds a fresh supervisor because Stop is terminal.
func (c *signalSupervisorControl) StopAndUnpair(
	unpair func(context.Context) error,
) error {
	c.mu.Lock()
	if c.closed || c.stopping {
		c.mu.Unlock()
		return bridge.ErrSupervisorStopped
	}
	c.stopping = true
	supervisor := c.supervisor
	c.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), signalSupervisorStopTimeout)
	defer cancel()
	if err := supervisor.Stop(ctx); err != nil {
		go c.awaitStoppedSupervisor(supervisor)
		return fmt.Errorf("stop Signal connection: %w", err)
	}

	var unpairErr error
	if unpair != nil {
		unpairErr = unpair(ctx)
	}
	c.mu.Lock()
	c.supervisorStopped = true
	c.stopping = false
	c.mu.Unlock()
	return unpairErr
}

func (c *signalSupervisorControl) awaitStoppedSupervisor(supervisor *bridge.Supervisor) {
	_ = supervisor.Stop(context.Background())
	c.mu.Lock()
	if c.supervisor == supervisor && !c.closed {
		c.supervisorStopped = true
		c.stopping = false
	}
	c.mu.Unlock()
}

func (c *signalSupervisorControl) Stop(ctx context.Context) error {
	if ctx == nil {
		return errors.New("Signal supervisor control: nil stop context")
	}
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
