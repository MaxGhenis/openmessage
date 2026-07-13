// Package signal adapts one retained signallive signal-cli poller lifecycle to
// bridge.Run. Legacy send, media, reaction, recovery, and QR/status paths stay
// on signallive.Bridge; this adapter owns only connect/reconnect/park.
package signal

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/signallive"
)

type poller interface {
	StartPoller(context.Context) (signallive.PollerRun, error)
	Status() signallive.StatusSnapshot
	ApplyPollerFailure(signallive.PollerExit)
	InputFingerprint() string
	UnpairContext(context.Context) error
}

// Adapter owns Signal connection generations while retaining signallive's
// existing signal-cli receive loop and all legacy operation paths.
type Adapter struct {
	accountID string
	poller    poller

	mu       sync.Mutex
	current  *run
	starting bool
}

func New(accountID string, live *signallive.Bridge) *Adapter {
	adapter := &Adapter{accountID: accountID}
	if live != nil {
		adapter.poller = live
	}
	return adapter
}

func (a *Adapter) AccountID() string { return a.accountID }

func (a *Adapter) Platform() bridge.Platform { return bridge.PlatformSignal }

func (a *Adapter) DeclaredCapabilities() bridge.CapabilitySet {
	return bridge.CapabilitySet{
		TextSend:      true,
		MediaSend:     true,
		Reactions:     true,
		PairQR:        true,
		MediaDownload: true,
	}
}

func (a *Adapter) Start(
	ctx context.Context,
	req bridge.StartRequest,
	sink bridge.ConnectionSink,
) (bridge.Run, error) {
	if ctx == nil {
		return nil, errors.New("signal lifecycle: nil start context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if a == nil || a.poller == nil {
		return nil, bridge.OpError{
			Class:       bridge.FailureMisconfigured,
			Operation:   "connect",
			Fingerprint: "signal_adapter_unconfigured",
			Cause:       errors.New("Signal lifecycle adapter is not configured"),
		}
	}
	if req.AccountID != a.accountID {
		return nil, fmt.Errorf("signal lifecycle: account %q does not match %q", req.AccountID, a.accountID)
	}

	a.mu.Lock()
	if a.current != nil || a.starting {
		a.mu.Unlock()
		return nil, bridge.OpError{
			Class:       bridge.FailureMisconfigured,
			Operation:   "connect",
			Fingerprint: "overlapping_signal_generation",
			Cause:       errors.New("a Signal generation is already active"),
		}
	}
	a.starting = true
	a.mu.Unlock()
	installed := false
	defer func() {
		if installed {
			return
		}
		a.mu.Lock()
		a.starting = false
		a.mu.Unlock()
	}()

	pollerRun, err := a.poller.StartPoller(ctx)
	if err != nil {
		return nil, classifyStartError(err)
	}
	if pollerRun == nil {
		return nil, bridge.OpError{
			Class:       bridge.FailureMisconfigured,
			Operation:   "connect",
			Fingerprint: "signal_poller_nil_run",
			Cause:       errors.New("Signal poller returned a nil run"),
		}
	}

	r := &run{
		adapter:      a,
		request:      req,
		sink:         sink,
		poller:       pollerRun,
		ready:        pollerRun.Ready(),
		done:         make(chan error, 1),
		stopped:      make(chan struct{}),
		finish:       make(chan struct{}, 1),
		activityWait: make(chan struct{}),
		admitting:    true,
	}
	a.mu.Lock()
	a.current = r
	a.starting = false
	installed = true
	a.mu.Unlock()

	go r.coordinate(ctx)
	return r, nil
}

// ReportError lets unchanged App-level signal-cli send paths surface a local
// account fault to the receive lifecycle. It deliberately ignores everything a
// send can fail on that does NOT indict the local account — an unregistered
// *recipient* (whose "not registered" text is indistinguishable from an
// unregistered local account), untrusted-identity changes, group-permission
// denials, rate/proof challenges, and transient network blips — because none of
// those should disturb a healthy receive loop, and the account-scoped receive
// probe (probe_account -> PollerFailureReauth) is the authoritative detector for
// a genuinely invalid account. Even for an unambiguous local-account send
// failure the generation is retired only as *transient*: the next generation's
// probe, not this send's text, is what parks reauth, so a send can never by
// itself drive the bridge to Blocked/needs-reauth. Local validation/database
// errors (non-CommandError) are ignored entirely.
func (a *Adapter) ReportError(err error) bool {
	if a == nil || a.poller == nil || err == nil || !signallive.IsCommandError(err) {
		return false
	}
	if !signallive.IsSendAccountInvalidError(err) {
		// Not a local-account fault (recipient error, untrusted identity, group
		// perms, rate limit, network). The healthy receive generation stays.
		return false
	}
	status := a.poller.Status()
	if status.UpgradeRequired || status.NeedsReauth {
		// The poller has already established a stronger terminal state. Its Done
		// result owns the exact fingerprint and must win over this send failure.
		return false
	}
	r := a.currentRun()
	if r == nil || !r.admitCallback() {
		return false
	}
	defer r.callbacks.Done()

	// Retire as transient (circuit-exempt): the next generation's account-scoped
	// probe authoritatively parks reauth iff the account is really invalid.
	exit := signallive.PollerExit{
		Kind:        signallive.PollerFailureTransient,
		Operation:   "send",
		Fingerprint: "signal_send_account_check",
		Err:         err,
	}
	r.requestFinish(terminalCandidate{
		err:      classifyPollerExit(exit),
		sendExit: &exit,
	})
	return true
}

func (a *Adapter) InputFingerprint() string {
	if a == nil || a.poller == nil {
		return ""
	}
	return a.poller.InputFingerprint()
}

func (a *Adapter) Unpair(ctx context.Context) error {
	if ctx == nil {
		return errors.New("signal unpair: nil context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if a == nil || a.poller == nil {
		return errors.New("Signal lifecycle adapter is not configured")
	}
	a.mu.Lock()
	active := a.current != nil || a.starting
	a.mu.Unlock()
	if active {
		return errors.New("cannot unpair Signal while a lifecycle generation is active")
	}
	return a.poller.UnpairContext(ctx)
}

func (a *Adapter) currentRun() *run {
	a.mu.Lock()
	r := a.current
	a.mu.Unlock()
	return r
}

func (a *Adapter) clearCurrent(candidate *run) {
	a.mu.Lock()
	if a.current == candidate {
		a.current = nil
	}
	a.mu.Unlock()
}

func classifyStartError(err error) bridge.OpError {
	var failure bridge.OpError
	if errors.As(err, &failure) {
		return failure
	}
	return bridge.OpError{
		Class:       bridge.FailureTransient,
		Operation:   "connect",
		Fingerprint: "signal_start_failed",
		Cause:       err,
	}
}

func classifyPollerExit(exit signallive.PollerExit) error {
	if exit.Kind == "" {
		if errors.Is(exit.Err, context.Canceled) {
			return nil
		}
		return exit.Err
	}
	class := bridge.FailureTransient
	switch exit.Kind {
	case signallive.PollerFailureReauth:
		class = bridge.FailureReauthRequired
	case signallive.PollerFailureUnpaired:
		class = bridge.FailureUnpaired
	case signallive.PollerFailureUpgrade:
		class = bridge.FailureUpgradeRequired
	}
	operation := exit.Operation
	if operation == "" {
		operation = "receive"
	}
	fingerprint := exit.Fingerprint
	if fingerprint == "" {
		fingerprint = "signal_poller_stopped"
	}
	cause := exit.Err
	if cause == nil {
		cause = errors.New("Signal poller stopped")
	}
	return bridge.OpError{
		Class:       class,
		Operation:   operation,
		Fingerprint: fingerprint,
		Cause:       cause,
	}
}

type run struct {
	adapter *Adapter
	request bridge.StartRequest
	sink    bridge.ConnectionSink
	poller  signallive.PollerRun
	ready   <-chan struct{}
	done    chan error
	stopped chan struct{}
	finish  chan struct{}

	finishOnce sync.Once
	terminalMu sync.Mutex
	requested  terminalCandidate

	admissionMu sync.Mutex
	admitting   bool
	callbacks   sync.WaitGroup

	activityMu   sync.Mutex
	lastActivity bridge.Liveness
	activityWait chan struct{}
}

func (r *run) Ready() <-chan struct{} { return r.ready }

func (r *run) Done() <-chan error { return r.done }

func (r *run) Probe(ctx context.Context) (bridge.Liveness, error) {
	if ctx == nil {
		return bridge.Liveness{}, errors.New("signal probe: nil context")
	}
	started := time.Now()
	for {
		r.activityMu.Lock()
		liveness := r.lastActivity
		wait := r.activityWait
		r.activityMu.Unlock()
		if !liveness.AliveAt.Before(started) {
			return liveness, nil
		}
		select {
		case <-wait:
		case <-r.stopped:
			return bridge.Liveness{}, errors.New("Signal generation ended during liveness probe")
		case <-ctx.Done():
			return bridge.Liveness{}, ctx.Err()
		}
	}
}

func (r *run) Stop(ctx context.Context) error {
	if ctx == nil {
		return errors.New("signal run: nil stop context")
	}
	r.requestFinish(terminalCandidate{})
	select {
	case <-r.stopped:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *run) coordinate(ctx context.Context) {
	activities := r.poller.Activity()
	pollerDone := r.poller.Done()
	var terminal terminalCandidate
	selected := false
	pollerResolved := false
	for !selected {
		select {
		case activity, ok := <-activities:
			if !ok {
				activities = nil
				continue
			}
			r.recordActivity(activity)
			if r.sink != nil {
				r.sink.Beat(r.request.Generation, activity.At, activity.Detail)
			}
		case exit, ok := <-pollerDone:
			pollerResolved = true
			if !ok {
				exit = signallive.PollerExit{
					Kind:        signallive.PollerFailureTransient,
					Operation:   "receive",
					Fingerprint: "signal_poller_done_closed",
					Err:         errors.New("Signal poller completion closed without a result"),
				}
			}
			terminal.err = classifyPollerExit(exit)
			selected = true
		case <-r.finish:
			terminal = r.requestedTerminal()
			selected = true
		case <-ctx.Done():
			terminal.err = context.Canceled
			selected = true
		}
	}

	r.closeAdmission()
	// Poller.Done reports the retained receive/link loop's exit. Stop is its
	// token-bound join; host-scoped metadata remains joined by Bridge.Close.
	_ = r.poller.Stop(context.Background())
	r.callbacks.Wait()
	// A receive terminal and an already-admitted send callback can become
	// ready together. Drain both sources after the poller join and keep the
	// stronger class so upgrade/reauth can never be downgraded to transient.
	if !pollerResolved {
		select {
		case exit, ok := <-pollerDone:
			if ok {
				terminal = preferTerminal(
					terminal,
					terminalCandidate{err: classifyPollerExit(exit)},
				)
			}
		default:
		}
	}
	terminal = preferTerminal(terminal, r.requestedTerminal())
	if terminal.sendExit != nil {
		r.adapter.poller.ApplyPollerFailure(*terminal.sendExit)
	}
	r.adapter.clearCurrent(r)
	if errors.Is(terminal.err, context.Canceled) {
		terminal.err = nil
	}
	r.done <- terminal.err
	close(r.done)
	close(r.stopped)
}

type terminalCandidate struct {
	err      error
	sendExit *signallive.PollerExit
}

func preferTerminal(current, candidate terminalCandidate) terminalCandidate {
	if candidate.err == nil {
		return current
	}
	if current.err == nil || terminalRank(candidate.err) > terminalRank(current.err) {
		return candidate
	}
	return current
}

func terminalRank(err error) int {
	var failure bridge.OpError
	if !errors.As(err, &failure) {
		return 0
	}
	switch failure.Class {
	case bridge.FailureUpgradeRequired:
		return 4
	case bridge.FailureReauthRequired:
		return 3
	case bridge.FailureUnpaired:
		return 2
	case bridge.FailureTransient:
		return 1
	default:
		return 2
	}
}

func (r *run) requestFinish(candidate terminalCandidate) {
	r.closeAdmission()
	r.terminalMu.Lock()
	r.requested = preferTerminal(r.requested, candidate)
	r.terminalMu.Unlock()
	r.finishOnce.Do(func() {
		r.finish <- struct{}{}
	})
}

func (r *run) requestedTerminal() terminalCandidate {
	r.terminalMu.Lock()
	defer r.terminalMu.Unlock()
	return r.requested
}

func (r *run) closeAdmission() {
	r.admissionMu.Lock()
	r.admitting = false
	r.admissionMu.Unlock()
}

func (r *run) admitCallback() bool {
	r.admissionMu.Lock()
	defer r.admissionMu.Unlock()
	if !r.admitting {
		return false
	}
	r.callbacks.Add(1)
	return true
}

func (r *run) recordActivity(activity signallive.PollerActivity) {
	r.activityMu.Lock()
	r.lastActivity = bridge.Liveness{AliveAt: activity.At, Detail: activity.Detail}
	close(r.activityWait)
	r.activityWait = make(chan struct{})
	r.activityMu.Unlock()
}

var (
	_ bridge.Adapter            = (*Adapter)(nil)
	_ bridge.CapabilityDeclarer = (*Adapter)(nil)
	_ bridge.Run                = (*run)(nil)
)
