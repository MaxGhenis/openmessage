package signallive

import (
	"errors"
	"sync"
	"time"
)

// requestLockWait bounds how long a send or attachment fetch waits for the
// command gate. It is longer than a receive call holds the gate unless it is
// drained (receiveCallDeadline plus signalStopGrace), so a request gives up
// only behind a drain it started waiting before, or a stuck call. Swapped by
// tests.
var requestLockWait = 10 * time.Second

var (
	errSignalReceiveDraining = errors.New("signal-cli is finishing an attachment download for a receive call; not run")
	errSignalCLIBusy         = errors.New("signal-cli stayed busy with another call; not run")
)

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

// lockWithin takes the gate unless it stays taken for wait.
func (g *commandGate) lockWithin(wait time.Duration) bool {
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
	}
}

func (g *commandGate) Unlock() {
	select {
	case <-g.slot:
	default:
		panic("signallive: unlock of unlocked commandGate")
	}
}

// lockForRequest takes the command gate for a send or an attachment fetch,
// which a caller is waiting on. It fails, without signal-cli having run,
// while a receive call is drained to finish an attachment download, which
// can take minutes, or once the gate stays taken for requestLockWait. The
// dispatcher sends every platform's messages one at a time, so a Signal send
// must not wait behind a drain; the caller retries it instead.
func (b *Bridge) lockForRequest() error {
	if b.receiveDraining.Load() {
		return errSignalReceiveDraining
	}
	if !b.commandMu.lockWithin(requestLockWait) {
		return errSignalCLIBusy
	}
	return nil
}
