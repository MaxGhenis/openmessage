package cmd

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"time"

	"github.com/maxghenis/openmessage/internal/bridge"
)

const (
	whatsappAccountID             = "whatsapp-primary"
	whatsappSupervisorStopTimeout = 30 * time.Second
	whatsappPairCodeTimeout       = 35 * time.Second
)

func whatsappSupervisorPolicy() bridge.Policy {
	return bridge.Policy{
		ConnectTimeout:     45 * time.Second,
		ProbeEvery:         30 * time.Second,
		ProbeTimeout:       20 * time.Second,
		LivenessTimeout:    3 * time.Minute,
		MinBackoff:         5 * time.Second,
		MaxBackoff:         5 * time.Minute,
		MaxSameFingerprint: 3,
	}
}

type whatsappWallClock struct{}

func (whatsappWallClock) Now() time.Time { return time.Now() }

func (whatsappWallClock) NewTimer(delay time.Duration) bridge.Timer {
	return &whatsappWallTimer{timer: time.NewTimer(delay)}
}

type whatsappWallTimer struct{ timer *time.Timer }

func (t *whatsappWallTimer) C() <-chan time.Time { return t.timer.C }
func (t *whatsappWallTimer) Stop() bool          { return t.timer.Stop() }

type whatsappRandom struct{}

func (whatsappRandom) Int63n(n int64) int64 { return rand.Int63n(n) }

// whatsappSupervisorControl is the only command-side entry point for
// WhatsApp connect, reconnect, pairing, and unpairing. It never dials the
// transport itself: every attempt is admitted by the account supervisor.
type whatsappSupervisorControl struct {
	supervisor    *bridge.Supervisor
	newSupervisor func(bridge.State) (*bridge.Supervisor, error)
	paired        func() bool

	mu                sync.Mutex
	supervisorStopped bool
	stopping          bool
	closed            bool
	pairWatchActive   bool
	pairWatchCancel   context.CancelFunc
	pairWatchEpoch    uint64
	pairWatchWG       sync.WaitGroup
}

func newWhatsAppSupervisorControl(
	supervisor *bridge.Supervisor,
	newSupervisor func(bridge.State) (*bridge.Supervisor, error),
	paired func() bool,
) *whatsappSupervisorControl {
	return &whatsappSupervisorControl{
		supervisor:    supervisor,
		newSupervisor: newSupervisor,
		paired:        paired,
	}
}

// Reconnect starts a paired lifecycle generation or, for an unpaired account,
// admits one user-initiated QR pairing attempt. Duplicate requests while the
// supervisor is already working are successful no-ops.
func (c *whatsappSupervisorControl) Reconnect() error {
	supervisor, err := c.activeSupervisor()
	if err != nil {
		return err
	}

	snapshot := supervisor.Snapshot()
	switch snapshot.State {
	case bridge.StateStopped:
		if !c.isPaired() {
			return c.beginPairing(supervisor, bridge.PairRequest{Method: bridge.PairQR}, nil)
		}
		ctx, cancel := context.WithTimeout(context.Background(), whatsappSupervisorPolicy().ConnectTimeout)
		defer cancel()
		err := supervisor.Start(ctx, bridge.StartRequest{})
		if errors.Is(err, bridge.ErrSupervisorBusy) {
			return nil
		}
		return err
	case bridge.StateUnpaired:
		return c.beginPairing(supervisor, bridge.PairRequest{Method: bridge.PairQR}, nil)
	case bridge.StatePairing, bridge.StateConnecting, bridge.StateOnline,
		bridge.StateDegraded, bridge.StateBackoff, bridge.StateRepairingCredentials:
		return nil
	case bridge.StateBlocked:
		if !c.isPaired() {
			supervisor, err = c.rebuildUnpairedSupervisor(supervisor)
			if err != nil {
				return err
			}
			return c.beginPairing(supervisor, bridge.PairRequest{Method: bridge.PairQR}, nil)
		}
		return fmt.Errorf(
			"WhatsApp requires re-pairing before reconnecting (%s: %s)",
			snapshot.ErrorClass,
			snapshot.ErrorFingerprint,
		)
	case bridge.StateStopping:
		return bridge.ErrSupervisorStopped
	default:
		return fmt.Errorf("WhatsApp cannot reconnect from supervisor state %q", snapshot.State)
	}
}

// PairPhone admits one phone-code pairing attempt and waits only until the
// display code is available. The supervisor-owned Pairer remains active until
// PairSuccess, failure, or cancellation.
func (c *whatsappSupervisorControl) PairPhone(phone string) (string, error) {
	phone = strings.TrimSpace(phone)
	if phone == "" {
		return "", errors.New("WhatsApp phone number is required")
	}
	supervisor, err := c.activeSupervisor()
	if err != nil {
		return "", err
	}
	snapshot := supervisor.Snapshot()
	if snapshot.State == bridge.StateBlocked && !c.isPaired() {
		supervisor, err = c.rebuildUnpairedSupervisor(supervisor)
		if err != nil {
			return "", err
		}
		snapshot = supervisor.Snapshot()
	}
	if snapshot.State != bridge.StateUnpaired {
		if snapshot.State == bridge.StatePairing {
			return "", bridge.ErrPairingInProgress
		}
		return "", fmt.Errorf("%w: state %q", bridge.ErrPairingNotAllowed, snapshot.State)
	}

	sink := newWhatsAppPairCodeSink()
	if err := c.beginPairing(supervisor, bridge.PairRequest{
		Method: bridge.PairPhoneCode,
		Phone:  phone,
	}, sink); err != nil {
		return "", err
	}

	timer := time.NewTimer(whatsappPairCodeTimeout)
	defer timer.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case event := <-sink.events:
			switch event.Kind {
			case "code":
				if strings.TrimSpace(event.Payload) == "" {
					return "", errors.New("WhatsApp returned an empty pairing code")
				}
				return event.Payload, nil
			case "blocked":
				return "", fmt.Errorf("%w until %s", bridge.ErrPairingRateLimited, event.ExpiresAt.Format(time.RFC3339Nano))
			}
		case <-ticker.C:
			snapshot := supervisor.Snapshot()
			if snapshot.State != bridge.StatePairing {
				return "", fmt.Errorf(
					"WhatsApp pairing ended before a code was available (%s: %s)",
					snapshot.ErrorClass,
					snapshot.ErrorFingerprint,
				)
			}
		case <-timer.C:
			return "", errors.New("timed out waiting for WhatsApp pairing code")
		}
	}
}

func (c *whatsappSupervisorControl) beginPairing(
	supervisor *bridge.Supervisor,
	request bridge.PairRequest,
	sink bridge.PairSink,
) error {
	ctx, cancel := context.WithTimeout(context.Background(), whatsappSupervisorPolicy().ConnectTimeout)
	defer cancel()
	if err := supervisor.BeginPairing(ctx, request, sink); err != nil {
		return err
	}
	c.startPairWatcher(supervisor)
	return nil
}

// startPairWatcher performs the one required post-pair lifecycle Start. Pairer
// returns only after credentials have been installed and its pairing transport
// has been disconnected, so StateStopped is the handoff point between the two
// supervisor-owned activities.
func (c *whatsappSupervisorControl) startPairWatcher(supervisor *bridge.Supervisor) {
	c.mu.Lock()
	if c.closed || c.stopping || c.supervisor != supervisor || c.pairWatchActive {
		c.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	c.pairWatchEpoch++
	epoch := c.pairWatchEpoch
	c.pairWatchActive = true
	c.pairWatchCancel = cancel
	c.pairWatchWG.Add(1)
	c.mu.Unlock()

	go func() {
		defer c.pairWatchWG.Done()
		defer func() {
			c.mu.Lock()
			if c.supervisor == supervisor && c.pairWatchEpoch == epoch {
				c.pairWatchActive = false
				c.pairWatchCancel = nil
			}
			c.mu.Unlock()
		}()

		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			switch supervisor.Snapshot().State {
			case bridge.StateStopped:
				startCtx, startCancel := context.WithTimeout(ctx, whatsappSupervisorPolicy().ConnectTimeout)
				err := supervisor.Start(startCtx, bridge.StartRequest{})
				startCancel()
				if err == nil || errors.Is(err, bridge.ErrSupervisorBusy) || errors.Is(err, context.Canceled) {
					return
				}
				return
			case bridge.StatePairing:
			case bridge.StateUnpaired, bridge.StateBlocked, bridge.StateStopping:
				return
			case bridge.StateConnecting, bridge.StateOnline, bridge.StateDegraded,
				bridge.StateBackoff, bridge.StateRepairingCredentials:
				return
			}

			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

func (c *whatsappSupervisorControl) activeSupervisor() (*bridge.Supervisor, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.stopping {
		return nil, bridge.ErrSupervisorStopped
	}
	if c.supervisorStopped {
		if c.newSupervisor == nil {
			return nil, errors.New("WhatsApp supervisor cannot be restarted")
		}
		initial := bridge.StateUnpaired
		if c.isPairedLocked() {
			initial = bridge.StateStopped
		}
		supervisor, err := c.newSupervisor(initial)
		if err != nil {
			return nil, fmt.Errorf("rebuild WhatsApp supervisor: %w", err)
		}
		c.supervisor = supervisor
		c.supervisorStopped = false
	}
	if c.supervisor == nil {
		return nil, errors.New("WhatsApp supervisor is unavailable")
	}
	return c.supervisor, nil
}

func (c *whatsappSupervisorControl) isPaired() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.isPairedLocked()
}

func (c *whatsappSupervisorControl) isPairedLocked() bool {
	return c.paired != nil && c.paired()
}

// rebuildUnpairedSupervisor terminally joins a blocked supervisor whose
// remote logout already removed the local session, then reuses the normal
// stopped-supervisor rebuild path. The replacement is therefore admitted in
// StateUnpaired and can own a fresh pairing attempt.
func (c *whatsappSupervisorControl) rebuildUnpairedSupervisor(
	supervisor *bridge.Supervisor,
) (*bridge.Supervisor, error) {
	c.mu.Lock()
	if c.closed || c.stopping {
		c.mu.Unlock()
		return nil, bridge.ErrSupervisorStopped
	}
	if c.supervisor != supervisor {
		c.mu.Unlock()
		return c.activeSupervisor()
	}
	c.stopping = true
	cancelWatcher := c.pairWatchCancel
	c.mu.Unlock()

	if cancelWatcher != nil {
		cancelWatcher()
	}
	ctx, cancel := context.WithTimeout(context.Background(), whatsappSupervisorStopTimeout)
	defer cancel()
	if err := supervisor.Stop(ctx); err != nil {
		go c.awaitStoppedSupervisor(supervisor)
		return nil, fmt.Errorf("stop blocked WhatsApp supervisor: %w", err)
	}
	c.pairWatchWG.Wait()

	c.mu.Lock()
	if c.closed {
		c.stopping = false
		c.mu.Unlock()
		return nil, bridge.ErrSupervisorStopped
	}
	if c.supervisor == supervisor {
		c.supervisorStopped = true
		c.pairWatchActive = false
		c.pairWatchCancel = nil
	}
	c.stopping = false
	c.mu.Unlock()

	return c.activeSupervisor()
}

func (c *whatsappSupervisorControl) StopAndUnpair(unpair func() error) error {
	c.mu.Lock()
	if c.closed || c.stopping {
		c.mu.Unlock()
		return bridge.ErrSupervisorStopped
	}
	c.stopping = true
	supervisor := c.supervisor
	cancelWatcher := c.pairWatchCancel
	c.mu.Unlock()

	if cancelWatcher != nil {
		cancelWatcher()
	}
	ctx, cancel := context.WithTimeout(context.Background(), whatsappSupervisorStopTimeout)
	defer cancel()
	stopErr := supervisor.Stop(ctx)
	if stopErr != nil {
		go c.awaitStoppedSupervisor(supervisor)
		return fmt.Errorf("stop WhatsApp connection: %w", stopErr)
	}
	c.pairWatchWG.Wait()

	var unpairErr error
	if unpair != nil {
		unpairErr = unpair()
	}
	c.mu.Lock()
	c.supervisorStopped = true
	c.stopping = false
	c.pairWatchActive = false
	c.pairWatchCancel = nil
	c.mu.Unlock()
	return unpairErr
}

func (c *whatsappSupervisorControl) awaitStoppedSupervisor(supervisor *bridge.Supervisor) {
	_ = supervisor.Stop(context.Background())
	c.pairWatchWG.Wait()
	c.mu.Lock()
	if c.supervisor == supervisor && !c.closed {
		c.supervisorStopped = true
		c.stopping = false
		c.pairWatchActive = false
		c.pairWatchCancel = nil
	}
	c.mu.Unlock()
}

func (c *whatsappSupervisorControl) Stop(ctx context.Context) error {
	c.mu.Lock()
	if c.supervisor == nil {
		c.closed = true
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.stopping = true
	supervisor := c.supervisor
	cancelWatcher := c.pairWatchCancel
	c.mu.Unlock()

	if cancelWatcher != nil {
		cancelWatcher()
	}
	err := supervisor.Stop(ctx)
	if err != nil {
		return err
	}
	c.pairWatchWG.Wait()
	c.mu.Lock()
	c.supervisorStopped = true
	c.stopping = false
	c.pairWatchActive = false
	c.pairWatchCancel = nil
	c.mu.Unlock()
	return nil
}

type whatsappPairCodeSink struct {
	events chan bridge.PairEvent
}

func newWhatsAppPairCodeSink() *whatsappPairCodeSink {
	return &whatsappPairCodeSink{events: make(chan bridge.PairEvent, 4)}
}

func (s *whatsappPairCodeSink) EmitPairEvent(ctx context.Context, event bridge.PairEvent) error {
	select {
	case s.events <- event:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

var _ bridge.PairSink = (*whatsappPairCodeSink)(nil)
