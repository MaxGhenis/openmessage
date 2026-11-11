package signallive

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// requestLockWait bounds how long a send or attachment fetch waits for the
// command gate. It is longer than a receive call holds the gate unless it is
// drained (receiveCallDeadline plus signalStopGrace). A request can still
// give up behind a stuck call or a queue of ordinary calls ahead of it (a
// receive call, then a contact refresh); it is then not dispatched and
// retried. Swapped by tests.
var requestLockWait = 10 * time.Second

// ErrReceiveDraining is why a request gave up without signal-cli running: a
// receive call was being drained to finish an attachment download.
var ErrReceiveDraining = errors.New("signal-cli is finishing an attachment download for a receive call; not run")

var errSignalCLIBusy = errors.New("signal-cli stayed busy with another call; not run")

// IsReceiveDrainingError reports whether err is a request that gave up for
// ErrReceiveDraining. A drain ends on its own within minutes, so a caller that
// limits its attempts should not count this one.
func IsReceiveDrainingError(err error) bool {
	return errors.Is(err, ErrReceiveDraining)
}

// commandGate serializes signal-cli invocations on the bridge's config dir:
// signal-cli locks the account file itself (SignalAccount), so a second
// process would wait for the first or fail. Waiters are served in arrival
// order, and unlike a sync.Mutex a waiter can give up (lockWithin).
type commandGate struct {
	once sync.Once
	slot chan struct{}
}

func (g *commandGate) init() {
	g.once.Do(func() { g.slot = make(chan struct{}, 1) })
}

func (g *commandGate) Lock() {
	g.init()
	g.slot <- struct{}{}
}

// lockWithin takes the gate unless it stays taken for wait, or stop closes
// first.
func (g *commandGate) lockWithin(wait time.Duration, stop <-chan struct{}) bool {
	g.init()
	select {
	case g.slot <- struct{}{}:
		return true
	default:
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case g.slot <- struct{}{}:
		return true
	case <-timer.C:
		return false
	case <-stop:
		return false
	}
}

func (g *commandGate) Unlock() {
	select {
	case <-g.slot:
	default:
		panic("signallive: unlock of unlocked commandGate")
	}
}

// drainFlag is set while a receive call stopped by its deadline is held open
// to finish an attachment download. Its zero value is clear.
type drainFlag struct {
	mu      sync.Mutex
	holding bool
	// begun is closed while holding and dropped when the flag clears, so a
	// waiter can stop waiting the moment a drain begins.
	begun chan struct{}
}

func (f *drainFlag) Store(holding bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if holding == f.holding {
		return
	}
	f.holding = holding
	if holding {
		if f.begun == nil {
			f.begun = make(chan struct{})
		}
		close(f.begun)
	} else {
		f.begun = nil
	}
}

func (f *drainFlag) Load() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.holding
}

// started returns a channel that is closed while the flag is set.
func (f *drainFlag) started() <-chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.begun == nil {
		f.begun = make(chan struct{})
		if f.holding {
			close(f.begun)
		}
	}
	return f.begun
}

// lockForRequest takes the command gate for a send or an attachment fetch,
// which a caller is waiting on. It fails, without signal-cli having run,
// while a receive call is drained to finish an attachment download, which
// can take minutes, or once the gate stays taken for requestLockWait. The
// dispatcher sends every platform's messages one at a time, so a Signal send
// must not wait behind a drain; the caller retries it instead.
func (b *Bridge) lockForRequest() error {
	drain := b.receiveDraining.started()
	if isClosed(drain) {
		return ErrReceiveDraining
	}
	if b.commandMu.lockWithin(requestLockWait, drain) {
		return nil
	}
	if isClosed(drain) {
		return ErrReceiveDraining
	}
	return errSignalCLIBusy
}

// commandGateError is a request that gave up on the command gate: a
// CommandError carrying its text that also unwraps to the reason.
type commandGateError struct {
	*CommandError
	reason error
}

func (e *commandGateError) Unwrap() []error { return []error{e.CommandError, e.reason} }

// gateNotDispatchedError reports a send that gave up on the command gate:
// signal-cli never ran, so it was not dispatched.
func gateNotDispatchedError(prefix string, reason error) error {
	return &signalSendNotDispatchedError{err: &commandGateError{
		CommandError: &CommandError{message: fmt.Sprintf("%s: %s", prefix, reason)},
		reason:       reason,
	}}
}
