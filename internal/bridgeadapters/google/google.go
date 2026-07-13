// Package google adapts the pinned libgm connection lifecycle to one
// generation-owned bridge.Run. Legacy web operations continue to use
// app.App.GetClient; durable media dispatch reaches that retained client
// through the adapter shim in send.go.
package google

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"go.mau.fi/mautrix-gmessages/pkg/libgm"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/events"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"

	"github.com/maxghenis/openmessage/internal/app"
	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/client"
)

type transportClient interface {
	SetEventHandler(libgm.EventHandler)
	Connect() error
	Disconnect()
	NotifyDittoActivity() (<-chan *libgm.IncomingRPCMessage, error)
}

type clientFactory func() (*client.Client, transportClient, error)

// Adapter owns Google connection generations while leaving the legacy
// transport operations installed on App.
type Adapter struct {
	accountID string
	host      *app.App
	canRepair func() bool
	newClient clientFactory

	mu       sync.Mutex
	current  *run
	starting bool
}

func New(accountID string, host *app.App, canRepair func() bool) *Adapter {
	a := &Adapter{accountID: accountID, host: host, canRepair: canRepair}
	a.newClient = a.loadClient
	return a
}

func (a *Adapter) AccountID() string { return a.accountID }

func (a *Adapter) Platform() bridge.Platform { return bridge.PlatformGoogle }

func (a *Adapter) DeclaredCapabilities() bridge.CapabilitySet {
	return bridge.CapabilitySet{
		StartConversation: true,
		TextSend:          true,
		MediaSend:         true,
		Reactions:         true,
		ReadReceipts:      true,
		PairQR:            true,
		MediaDownload:     true,
	}
}

func (a *Adapter) loadClient() (*client.Client, transportClient, error) {
	data, err := client.LoadSession(a.host.SessionPath)
	if err != nil {
		return nil, nil, fmt.Errorf("load Google session: %w", err)
	}
	legacy, err := client.NewFromSession(data, a.host.Logger)
	if err != nil {
		return nil, nil, fmt.Errorf("create Google client: %w", err)
	}
	return legacy, legacy.GM, nil
}

func (a *Adapter) Start(
	ctx context.Context,
	req bridge.StartRequest,
	sink bridge.ConnectionSink,
) (bridge.Run, error) {
	overlapFailure := func() bridge.OpError {
		return bridge.OpError{
			Class:       bridge.FailureMisconfigured,
			Operation:   "connect",
			Fingerprint: "overlapping_google_generation",
			Cause:       errors.New("a Google generation is already active"),
		}
	}
	if ctx == nil {
		return nil, errors.New("google lifecycle: nil start context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if a == nil {
		return nil, bridge.OpError{
			Class:       bridge.FailureMisconfigured,
			Operation:   "connect",
			Fingerprint: "google_adapter_unconfigured",
			Cause:       errors.New("Google lifecycle adapter is nil"),
		}
	}
	if req.AccountID != a.accountID {
		return nil, fmt.Errorf("google lifecycle: account %q does not match %q", req.AccountID, a.accountID)
	}
	if a.host == nil || a.newClient == nil {
		return nil, bridge.OpError{
			Class:       bridge.FailureMisconfigured,
			Operation:   "connect",
			Fingerprint: "google_adapter_unconfigured",
			Cause:       errors.New("Google lifecycle adapter is not configured"),
		}
	}

	// Reserve the only start slot before loading or installing a client. The
	// supervisor is the production caller, but this guard also prevents a
	// concurrent accidental Start from replacing App's active generation
	// before the overlap is detected.
	a.mu.Lock()
	if a.current != nil || a.starting {
		a.mu.Unlock()
		return nil, overlapFailure()
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

	legacy, transport, err := a.newClient()
	if err != nil {
		failure := classifyStartError(err)
		if a.host.GooglePaired() {
			a.host.FlagGoogleNeedsRepair()
		}
		return nil, failure
	}
	if legacy == nil || transport == nil {
		return nil, bridge.OpError{
			Class:       bridge.FailureMisconfigured,
			Operation:   "connect",
			Fingerprint: "google_client_factory_nil",
			Cause:       errors.New("Google client factory returned a nil client"),
		}
	}
	generation := a.host.BeginGoogleGeneration(legacy)
	r := &run{
		adapter:    a,
		request:    req,
		sink:       sink,
		transport:  transport,
		generation: generation,
		ready:      make(chan struct{}),
		done:       make(chan error, 1),
		stopped:    make(chan struct{}),
		finish:     make(chan error, 1),
		activity:   make(chan struct{}),
		admitting:  true,
	}
	transport.SetEventHandler(r.handleEvent)
	a.mu.Lock()
	a.current = r
	a.starting = false
	installed = true
	a.mu.Unlock()

	if err := transport.Connect(); err != nil {
		failure := a.classifyTransportError(err, "connect", "connect_failed")
		r.applyFailureStatus(failure)
		r.closeAdmission()
		transport.SetEventHandler(nil)
		transport.Disconnect()
		r.callbacks.Wait()
		generation.Release()
		a.clearCurrent(r)
		return nil, failure
	}
	go r.coordinate(ctx)
	return r, nil
}

// ReportError terminates the current generation from an unchanged legacy
// operation path (notably a send/read HTTP 401). The supervisor remains the
// sole owner of repair and reconnect.
func (a *Adapter) ReportError(err error) bool {
	if err == nil {
		return false
	}
	r := a.currentRun()
	if r == nil || !r.admitCallback() {
		return false
	}
	defer r.callbacks.Done()
	failure := a.classifyTransportError(err, "operation", "operation_failed")
	r.applyFailureStatus(failure)
	r.requestFinish(failure)
	return true
}

// ParkCurrent reports a terminal blocked condition such as a known-dead
// linked session for which no automatic cookie refresh is available.
func (a *Adapter) ParkCurrent(err error) bool {
	if err == nil {
		err = errors.New("Google connection requires manual repair")
	}
	r := a.currentRun()
	if r == nil || !r.admitCallback() {
		return false
	}
	defer r.callbacks.Done()
	failure := bridge.OpError{
		Class:       bridge.FailureUpgradeRequired,
		Operation:   "repair",
		Fingerprint: "google_manual_repair_required",
		Cause:       err,
	}
	r.applyFailureStatus(failure)
	r.requestFinish(failure)
	return true
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

func (a *Adapter) repairAvailable() bool {
	return a.canRepair != nil && a.canRepair()
}

func (a *Adapter) classifyTransportError(err error, operation, fingerprint string) bridge.OpError {
	if err == nil {
		err = errors.New("Google transport ended without an error")
	}
	if failure, ok := asOpError(err); ok {
		return failure
	}
	if app.IsGoogleAuthExpiredError(err) {
		class := bridge.FailureCredentialsExpired
		if !a.repairAvailable() {
			class = bridge.FailureUpgradeRequired
		}
		return bridge.OpError{
			Class:       class,
			Operation:   operation,
			Fingerprint: "google_auth_expired",
			Cause:       err,
		}
	}
	lower := strings.ToLower(err.Error())
	if strings.Contains(lower, "no auth token") || strings.Contains(lower, "not logged in") {
		return bridge.OpError{
			Class:       bridge.FailureReauthRequired,
			Operation:   operation,
			Fingerprint: "google_session_invalid",
			Cause:       err,
		}
	}
	return bridge.OpError{
		Class:       bridge.FailureTransient,
		Operation:   operation,
		Fingerprint: fingerprint,
		Cause:       err,
	}
}

func classifyStartError(err error) bridge.OpError {
	class := bridge.FailureReauthRequired
	fingerprint := "google_session_unusable"
	if !errors.Is(err, os.ErrNotExist) && !strings.Contains(strings.ToLower(err.Error()), "unmarshal") {
		class = bridge.FailureMisconfigured
		fingerprint = "google_session_load_failed"
	}
	return bridge.OpError{
		Class:       class,
		Operation:   "load_credentials",
		Fingerprint: fingerprint,
		Cause:       err,
	}
}

func asOpError(err error) (bridge.OpError, bool) {
	var pointer *bridge.OpError
	if errors.As(err, &pointer) && pointer != nil {
		return *pointer, true
	}
	var value bridge.OpError
	if errors.As(err, &value) {
		return value, true
	}
	return bridge.OpError{}, false
}

type run struct {
	adapter    *Adapter
	request    bridge.StartRequest
	sink       bridge.ConnectionSink
	transport  transportClient
	generation *app.GoogleGeneration

	ready      chan struct{}
	done       chan error
	stopped    chan struct{}
	finish     chan error
	readyOnce  sync.Once
	finishOnce sync.Once

	admissionMu sync.Mutex
	admitting   bool
	callbacks   sync.WaitGroup

	activityMu         sync.Mutex
	activity           chan struct{}
	lastActivityAt     time.Time
	lastActivityDetail string
	phoneNotResponding bool
}

func (r *run) Ready() <-chan struct{} { return r.ready }

func (r *run) Done() <-chan error { return r.done }

func (r *run) Probe(ctx context.Context) (bridge.Liveness, error) {
	if ctx == nil {
		return bridge.Liveness{}, errors.New("google probe: nil context")
	}
	response, err := r.transport.NotifyDittoActivity()
	if err != nil {
		failure := r.adapter.classifyTransportError(err, "probe", "google_probe_send_failed")
		return bridge.Liveness{}, failure
	}
	if liveness, activity := r.probeActivity(); liveness.Detail == "phone_not_responding" {
		// NotifyDittoActivity is answered by the Android phone, not by the
		// Google long-poll service. Once libgm's own pinger has established
		// that the phone is offline, an absent response is expected and must
		// not turn a healthy linked-device generation into reconnect churn.
		return liveness, nil
	} else {
		select {
		case _, ok := <-response:
			if !ok {
				return bridge.Liveness{}, bridge.OpError{
					Class:       bridge.FailureTransient,
					Operation:   "probe",
					Fingerprint: "google_probe_response_closed",
					Cause:       errors.New("Google liveness response closed"),
				}
			}
			return bridge.Liveness{AliveAt: time.Now(), Detail: "notify_ditto_activity"}, nil
		case <-activity:
			// A pinger or long-poll event racing the probe is direct evidence
			// that the client machinery is still running. In particular, a
			// PhoneNotResponding event proves the missing probe response is a
			// phone-reachability condition rather than a dead Google link.
			liveness, _ := r.probeActivity()
			return liveness, nil
		case <-r.stopped:
			return bridge.Liveness{}, errors.New("Google generation ended during liveness probe")
		case <-ctx.Done():
			if liveness, _ := r.probeActivity(); liveness.Detail == "phone_not_responding" {
				return liveness, nil
			}
			return bridge.Liveness{}, ctx.Err()
		}
	}
}

func (r *run) Stop(ctx context.Context) error {
	if ctx == nil {
		return errors.New("google run: nil stop context")
	}
	r.requestFinish(nil)
	select {
	case <-r.stopped:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *run) coordinate(ctx context.Context) {
	var terminal error
	select {
	case terminal = <-r.finish:
	case <-ctx.Done():
		terminal = ctx.Err()
	}

	r.closeAdmission()
	r.transport.SetEventHandler(nil)
	r.transport.Disconnect()
	r.callbacks.Wait()
	if terminal == nil || errors.Is(terminal, context.Canceled) {
		r.generation.ConnectionLost()
	}
	r.generation.Release()
	r.adapter.clearCurrent(r)
	r.done <- terminal
	close(r.done)
	close(r.stopped)
}

func (r *run) requestFinish(err error) {
	r.finishOnce.Do(func() {
		// Make the terminal decision close callback admission immediately. This
		// prevents a second libgm or legacy-operation callback from racing in
		// with a different failure class before the supervisor retires the run.
		r.closeAdmission()
		r.finish <- err
	})
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

func (r *run) handleEvent(evt any) {
	if !r.admitCallback() {
		return
	}
	defer r.callbacks.Done()
	defer func() {
		if recovered := recover(); recovered != nil {
			r.adapter.host.Logger.Error().
				Interface("panic", recovered).
				Bytes("stack", debug.Stack()).
				Msg("Recovered from panic in Google Messages event handler")
			r.generation.SyncInterrupted()
			r.requestFinish(bridge.OpError{
				Class:       bridge.FailureTransient,
				Operation:   "event",
				Fingerprint: "google_event_handler_panic",
				Cause:       fmt.Errorf("panic in Google Messages event handler: %v", recovered),
			})
		}
	}()

	eventAt := time.Now()
	if eventProvidesLiveness(evt) {
		detail := googleEventDetail(evt)
		r.recordActivity(evt, eventAt, detail)
		if r.sink != nil {
			r.sink.Beat(r.request.Generation, eventAt, detail)
		}
	}
	if eventMakesReady(evt) {
		r.readyOnce.Do(func() {
			if r.generation.Ready() {
				close(r.ready)
			}
		})
	}

	r.generation.Handler.Handle(evt)
	if failure, terminal := r.classifyEvent(evt); terminal {
		if failure.Class == bridge.FailureCredentialsExpired ||
			failure.Class == bridge.FailureUpgradeRequired {
			r.applyFailureStatus(failure)
		}
		r.requestFinish(failure)
	}
}

func (r *run) classifyEvent(evt any) (bridge.OpError, bool) {
	switch event := evt.(type) {
	case *events.ListenFatalError:
		// ListenFatalError still keeps the stored linked-device session. Its
		// cause decides whether the supervisor retries that session directly or
		// repairs expired Google cookies before starting a new generation.
		return r.adapter.classifyTransportError(
			event.Error,
			"listen",
			"google_listen_fatal",
		), true
	case *events.GaiaLoggedOut:
		return bridge.OpError{
			Class:       bridge.FailureReauthRequired,
			Operation:   "listen",
			Fingerprint: "google_gaia_logged_out",
			Cause:       errors.New("Google account logged out server-side"),
		}, true
	case *events.PingFailed:
		if event.ErrorCount >= 3 {
			return bridge.OpError{
				Class:       bridge.FailureTransient,
				Operation:   "ping",
				Fingerprint: "google_ping_failed",
				Cause:       event.Error,
			}, true
		}
	}
	return bridge.OpError{}, false
}

func (r *run) applyFailureStatus(failure bridge.OpError) {
	switch failure.Class {
	case bridge.FailureCredentialsExpired:
		r.generation.AuthExpired(failure.Cause)
	case bridge.FailureUpgradeRequired, bridge.FailureMisconfigured, bridge.FailureUnsupported:
		if app.IsGoogleAuthExpiredError(failure.Cause) {
			r.generation.AuthExpired(failure.Cause)
		}
		r.adapter.host.FlagGoogleNeedsRepair()
	case bridge.FailureReauthRequired:
		// GaiaLoggedOut is applied by the legacy handler, which also preserves
		// the exact session deletion behavior. Other reauth failures merely end
		// the generation and park the still-present stored session for the UI.
		if r.adapter.host.GooglePaired() {
			r.adapter.host.FlagGoogleNeedsRepair()
		}
	default:
		r.generation.ConnectionLost()
	}
}

func eventMakesReady(evt any) bool {
	switch evt.(type) {
	case *events.ListenRecovered, *events.PhoneNotResponding,
		*events.PhoneRespondingAgain, *events.NoDataReceived,
		*libgm.WrappedMessage, *gmproto.Conversation, *gmproto.TypingData:
		return true
	default:
		return false
	}
}

func eventProvidesLiveness(evt any) bool {
	switch evt.(type) {
	case *libgm.WrappedMessage, *gmproto.Conversation, *gmproto.TypingData,
		*events.ListenRecovered, *events.PhoneNotResponding,
		*events.PhoneRespondingAgain, *events.NoDataReceived:
		return true
	default:
		return false
	}
}

func (r *run) recordActivity(evt any, at time.Time, detail string) {
	r.activityMu.Lock()
	r.lastActivityAt = at
	r.lastActivityDetail = detail
	switch evt.(type) {
	case *events.PhoneNotResponding:
		r.phoneNotResponding = true
	case *events.PhoneRespondingAgain:
		r.phoneNotResponding = false
	}
	close(r.activity)
	r.activity = make(chan struct{})
	r.activityMu.Unlock()
}

func (r *run) probeActivity() (bridge.Liveness, <-chan struct{}) {
	r.activityMu.Lock()
	defer r.activityMu.Unlock()
	liveness := bridge.Liveness{
		AliveAt: r.lastActivityAt,
		Detail:  r.lastActivityDetail,
	}
	if r.phoneNotResponding {
		liveness.AliveAt = time.Now()
		liveness.Detail = "phone_not_responding"
	}
	return liveness, r.activity
}

func googleEventDetail(evt any) string {
	return fmt.Sprintf("event:%T", evt)
}

var (
	_ bridge.Adapter            = (*Adapter)(nil)
	_ bridge.CapabilityDeclarer = (*Adapter)(nil)
	_ bridge.Run                = (*run)(nil)
)
