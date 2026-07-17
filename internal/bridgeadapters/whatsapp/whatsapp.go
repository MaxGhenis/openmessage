// Package whatsapp adapts one whatsmeow-backed live bridge to a
// generation-owned bridge.Run. Legacy message/media operations stay on App;
// all connection, retry, liveness, and pairing decisions belong to Supervisor.
package whatsapp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	wastore "go.mau.fi/whatsmeow/store"
	waevents "go.mau.fi/whatsmeow/types/events"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/whatsapplive"
)

type Adapter struct {
	accountID string
	host      *whatsapplive.Bridge

	connect             func(context.Context) error
	disconnect          func()
	probe               func(context.Context) error
	accountInfo         func() whatsapplive.AccountInfo
	cooldown            func() time.Time
	prepareQR           func(context.Context) (<-chan whatsmeow.QRChannelItem, error)
	connectPairing      func(context.Context) error
	applyQR             func(whatsmeow.QRChannelItem) whatsapplive.QRSnapshot
	pairPhone           func(context.Context, string) (string, error)
	unpair              func() error
	now                 func() time.Time
	mediaReady          func() bool // Shared connection gate; it does no media-specific work.
	sendTextRequest     func(string, string, string, string) (string, time.Time, error)
	sendReactionRequest func(string, string, string, string, string, string) error
	markReadRequest     func(string, []string, string, time.Time) error
	sendMediaRequest    func(string, []byte, string, string, string, string, string) (string, time.Time, error)
	downloadMediaRef    func(string, whatsapplive.StoredMediaRef, string, string, string) ([]byte, string, error)
	observeIngress      func(func(whatsapplive.IngressFrame)) func()
	logIngressError     func(error, string, uint64)

	mu       sync.Mutex
	current  *run
	starting bool
	pairing  *pairAttempt
}

func New(accountID string, host *whatsapplive.Bridge) *Adapter {
	a := &Adapter{
		accountID: accountID,
		host:      host,
		now:       time.Now,
	}
	if host != nil {
		a.connect = host.ConnectPaired
		a.disconnect = host.DisconnectTransport
		a.probe = host.ProbeKeepAlive
		a.accountInfo = host.PairedAccount
		a.cooldown = host.TemporaryBanDeadline
		a.prepareQR = host.PrepareQRPairing
		a.connectPairing = host.ConnectPairing
		a.applyQR = host.ApplyQRItem
		a.pairPhone = host.PairPhoneContext
		a.unpair = host.Unpair
		a.mediaReady = func() bool { return host.Status().Connected }
		a.sendTextRequest = host.SendTextRequest
		a.sendReactionRequest = host.SendReactionRequest
		a.markReadRequest = host.MarkReadRequest
		a.sendMediaRequest = host.SendMediaRequest
		a.downloadMediaRef = host.DownloadMediaRef
		a.observeIngress = host.ObserveIngress
		a.logIngressError = host.LogIngressError
		host.ObserveLifecycle(a.handleLifecycleEvent)
	}
	return a
}

func (a *Adapter) AccountID() string { return a.accountID }

func (a *Adapter) Platform() bridge.Platform { return bridge.PlatformWhatsApp }

func (a *Adapter) DeclaredCapabilities() bridge.CapabilitySet {
	return bridge.CapabilitySet{
		StartConversation: true,
		TextSend:          true,
		MediaSend:         true,
		Reactions:         true,
		ReadReceipts:      true,
		PairQR:            true,
		PairPhone:         true,
		MediaDownload:     true,
	}
}

// SendText bridges the v2 request contract to the already-owned whatsmeow
// transport. Readiness is checked without constructing or connecting a client.
func (a *Adapter) SendText(ctx context.Context, req bridge.TextRequest) (bridge.SendResult, error) {
	if a == nil || a.mediaReady == nil || !a.mediaReady() || a.sendTextRequest == nil {
		return bridge.SendResult{}, whatsappTextPreCallFailure(
			"whatsapp_not_connected",
			whatsmeow.ErrNotConnected,
		)
	}
	if ctx == nil {
		return bridge.SendResult{}, whatsappTextPreCallFailure(
			"whatsapp_text_context_missing",
			errors.New("WhatsApp text send context is nil"),
		)
	}
	if err := ctx.Err(); err != nil {
		return bridge.SendResult{}, whatsappTextPreCallFailure("whatsapp_text_context_done", err)
	}

	replyToID := ""
	if req.ReplyTo != nil {
		replyToID = req.ReplyTo.RemoteID
	}
	remoteID, acceptedAt, err := a.sendTextRequest(
		req.Conversation.RemoteID,
		req.Body,
		replyToID,
		req.RequestID,
	)
	if err != nil {
		// Deliberately no adapter-level lifecycle report: the retained transport
		// core owns reconnect reporting through its guarded reportConnectionError
		// -> OnConnectionError chain.
		failure := a.classifyError(err, "send_text", "whatsapp_send_text_failed")
		if errors.Is(err, whatsapplive.ErrSendNotDispatched) {
			failure.Dispatch = bridge.DispatchNotCalled
			if errors.Is(err, whatsmeow.ErrNotConnected) {
				failure.Fingerprint = "whatsapp_not_connected"
			}
		}
		return bridge.SendResult{}, failure
	}

	return bridge.SendResult{
		RemoteMessageID: remoteID,
		AcceptedAt:      acceptedAt,
		EchoExpected:    false,
	}, nil
}

func whatsappTextPreCallFailure(fingerprint string, cause error) bridge.OpError {
	return bridge.OpError{
		Class:       bridge.FailureTransient,
		Operation:   "send_text",
		Fingerprint: fingerprint,
		Dispatch:    bridge.DispatchNotCalled,
		Cause:       cause,
	}
}

var _ bridge.TextSender = (*Adapter)(nil)

// SendReaction bridges the v2 reaction request to the existing whatsmeow
// transport without loading or updating the legacy stored reaction target.
func (a *Adapter) SendReaction(ctx context.Context, req bridge.ReactionRequest) (bridge.SendResult, error) {
	if a == nil || a.mediaReady == nil || !a.mediaReady() || a.sendReactionRequest == nil {
		return bridge.SendResult{}, whatsappReactionPreCallFailure(
			"whatsapp_not_connected",
			whatsmeow.ErrNotConnected,
		)
	}
	if ctx == nil {
		return bridge.SendResult{}, whatsappReactionPreCallFailure(
			"whatsapp_reaction_context_missing",
			errors.New("WhatsApp reaction send context is nil"),
		)
	}
	if err := ctx.Err(); err != nil {
		return bridge.SendResult{}, whatsappReactionPreCallFailure("whatsapp_reaction_context_done", err)
	}

	err := a.sendReactionRequest(
		req.Conversation.RemoteID,
		req.Target.RemoteID,
		req.Target.AuthorID,
		req.Emoji,
		string(req.Action),
		req.RequestID,
	)
	if err != nil {
		// Deliberately no adapter-level lifecycle report. The retained core's
		// guarded reportConnectionError path owns reconnect-worthy failures.
		failure := a.classifyError(err, "send_reaction", "whatsapp_reaction_failed")
		if errors.Is(err, whatsapplive.ErrSendNotDispatched) {
			failure.Dispatch = bridge.DispatchNotCalled
		}
		return bridge.SendResult{}, failure
	}

	// Reaction dispatch intentionally confirms without a transport result.
	return bridge.SendResult{}, nil
}

func whatsappReactionPreCallFailure(fingerprint string, cause error) bridge.OpError {
	return bridge.OpError{
		Class:       bridge.FailureTransient,
		Operation:   "send_reaction",
		Fingerprint: fingerprint,
		Dispatch:    bridge.DispatchNotCalled,
		Cause:       cause,
	}
}

var _ bridge.ReactionSender = (*Adapter)(nil)

// MarkRead bridges the v2 read-receipt request to the already-owned whatsmeow
// transport. Readiness is checked without constructing or connecting a client.
func (a *Adapter) MarkRead(ctx context.Context, req bridge.ReadReceiptRequest) error {
	if a == nil || a.mediaReady == nil || !a.mediaReady() || a.markReadRequest == nil {
		return whatsappReadPreCallFailure(
			"whatsapp_not_connected",
			whatsmeow.ErrNotConnected,
		)
	}
	if ctx == nil {
		return whatsappReadPreCallFailure(
			"whatsapp_mark_read_context_missing",
			errors.New("WhatsApp mark-read context is nil"),
		)
	}
	if err := ctx.Err(); err != nil {
		return whatsappReadPreCallFailure("whatsapp_mark_read_context_done", err)
	}
	if len(req.Messages) == 0 {
		return whatsappReadPreCallFailure(
			"whatsapp_mark_read_no_messages",
			errors.New("WhatsApp mark-read request has no messages"),
		)
	}

	messageIDs := make([]string, len(req.Messages))
	for i, message := range req.Messages {
		messageIDs[i] = message.RemoteID
	}
	err := a.markReadRequest(
		req.Conversation.RemoteID,
		messageIDs,
		req.Messages[0].AuthorID,
		req.ReadAt,
	)
	if err != nil {
		// Deliberately no adapter-level lifecycle report: the retained core owns
		// reconnect reporting through its guarded reportConnectionError path.
		failure := a.classifyError(err, "mark_read", "whatsapp_mark_read_failed")
		// Deliberate idempotent-read divergence: even after whatsmeow's transport
		// call, repeating a read receipt is harmless. Preserve every retryable
		// class as DispatchNotCalled instead of uncertain; terminal classes are
		// still rejected by their classification before certainty is consulted.
		failure.Dispatch = bridge.DispatchNotCalled
		return failure
	}
	return nil
}

func whatsappReadPreCallFailure(fingerprint string, cause error) bridge.OpError {
	return bridge.OpError{
		Class:       bridge.FailureTransient,
		Operation:   "mark_read",
		Fingerprint: fingerprint,
		Dispatch:    bridge.DispatchNotCalled,
		Cause:       cause,
	}
}

var _ bridge.ReadReceiptSender = (*Adapter)(nil)

// SendMedia bridges the v2 request contract to the already-owned whatsmeow
// transport. Readiness is checked without constructing or connecting a client.
func (a *Adapter) SendMedia(ctx context.Context, req bridge.MediaRequest) (bridge.SendResult, error) {
	if a == nil || a.mediaReady == nil || !a.mediaReady() || a.sendMediaRequest == nil {
		return bridge.SendResult{}, whatsappMediaPreCallFailure(
			"whatsapp_not_connected",
			whatsmeow.ErrNotConnected,
		)
	}
	if ctx == nil {
		return bridge.SendResult{}, whatsappMediaPreCallFailure(
			"whatsapp_media_context_missing",
			errors.New("WhatsApp media send context is nil"),
		)
	}
	if err := ctx.Err(); err != nil {
		return bridge.SendResult{}, whatsappMediaPreCallFailure("whatsapp_media_context_done", err)
	}
	if req.Reader == nil {
		return bridge.SendResult{}, whatsappMediaPreCallFailure(
			"whatsapp_media_read_failed",
			errors.New("WhatsApp media reader is nil"),
		)
	}
	limit := req.Size + 1
	if req.Size < 0 || limit <= 0 {
		return bridge.SendResult{}, whatsappMediaPreCallFailure(
			"whatsapp_media_size_mismatch",
			fmt.Errorf("invalid WhatsApp media size %d", req.Size),
		)
	}
	data, err := io.ReadAll(io.LimitReader(req.Reader, limit))
	if err != nil {
		return bridge.SendResult{}, whatsappMediaPreCallFailure(
			"whatsapp_media_read_failed",
			fmt.Errorf("read WhatsApp media: %w", err),
		)
	}
	if int64(len(data)) != req.Size {
		return bridge.SendResult{}, whatsappMediaPreCallFailure(
			"whatsapp_media_size_mismatch",
			fmt.Errorf("WhatsApp media reader returned %d bytes, want %d", len(data), req.Size),
		)
	}
	if err := ctx.Err(); err != nil {
		return bridge.SendResult{}, whatsappMediaPreCallFailure("whatsapp_media_context_done", err)
	}

	replyToID := ""
	if req.ReplyTo != nil {
		replyToID = req.ReplyTo.RemoteID
	}
	remoteID, acceptedAt, err := a.sendMediaRequest(
		req.Conversation.RemoteID,
		data,
		req.Filename,
		req.MIME,
		req.Caption,
		replyToID,
		req.RequestID,
	)
	if err != nil {
		// Deliberately no adapter-level lifecycle report: a media send failure
		// must never retire a healthy receive generation (the C4/C5/C6 lesson).
		// The transport core already routes genuinely reconnect-worthy failures
		// through its internal reportConnectionError -> OnConnectionError chain,
		// which carries the legacy ShouldReconnect guard.
		failure := a.classifyError(err, "send_media", "whatsapp_send_media_failed")
		if errors.Is(err, whatsapplive.ErrSendNotDispatched) {
			failure.Dispatch = bridge.DispatchNotCalled
			if errors.Is(err, whatsmeow.ErrNotConnected) {
				failure.Fingerprint = "whatsapp_not_connected"
			}
		}
		return bridge.SendResult{}, failure
	}

	return bridge.SendResult{
		RemoteMessageID: remoteID,
		AcceptedAt:      acceptedAt,
		EchoExpected:    false,
	}, nil
}

func whatsappMediaPreCallFailure(fingerprint string, cause error) bridge.OpError {
	return bridge.OpError{
		Class:       bridge.FailureTransient,
		Operation:   "send_media",
		Fingerprint: fingerprint,
		Dispatch:    bridge.DispatchNotCalled,
		Cause:       cause,
	}
}

func (a *Adapter) Start(
	ctx context.Context,
	req bridge.StartRequest,
	sink bridge.ConnectionSink,
) (bridge.Run, error) {
	if ctx == nil {
		return nil, errors.New("whatsapp lifecycle: nil start context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if a == nil || a.host == nil || a.connect == nil || a.disconnect == nil ||
		a.probe == nil || a.accountInfo == nil || a.cooldown == nil || a.now == nil {
		return nil, bridge.OpError{
			Class:       bridge.FailureMisconfigured,
			Operation:   "connect",
			Fingerprint: "whatsapp_adapter_unconfigured",
			Cause:       errors.New("WhatsApp lifecycle adapter is not configured"),
		}
	}
	if req.AccountID != a.accountID {
		return nil, fmt.Errorf("whatsapp lifecycle: account %q does not match %q", req.AccountID, a.accountID)
	}
	if failure := a.cooldownFailure("connect"); failure != nil {
		return nil, *failure
	}
	if !a.accountInfo().Paired {
		return nil, bridge.OpError{
			Class:       bridge.FailureReauthRequired,
			Operation:   "connect",
			Fingerprint: "whatsapp_not_paired",
			Cause:       whatsmeow.ErrNotLoggedIn,
		}
	}

	a.mu.Lock()
	if a.current != nil || a.starting || a.pairing != nil {
		a.mu.Unlock()
		return nil, bridge.OpError{
			Class:       bridge.FailureMisconfigured,
			Operation:   "connect",
			Fingerprint: "overlapping_whatsapp_generation",
			Cause:       errors.New("a WhatsApp connect or pair attempt is already active"),
		}
	}
	a.starting = true
	a.mu.Unlock()

	runCtx, cancel := context.WithCancel(ctx)
	r := &run{
		adapter:   a,
		request:   req,
		sink:      sink,
		ctx:       runCtx,
		cancel:    cancel,
		ready:     make(chan struct{}),
		done:      make(chan error, 1),
		stopped:   make(chan struct{}),
		finish:    make(chan error, 1),
		admitting: true,
	}
	r.workers.Add(1)
	a.mu.Lock()
	a.current = r
	a.starting = false
	a.mu.Unlock()
	if a.observeIngress != nil {
		r.removeIngress = a.observeIngress(r.handleIngress)
	}

	go r.coordinate()
	go r.connect()
	return r, nil
}

// ReportError retires the current generation after a legacy send/media/group
// operation reports a reconnect-worthy failure. It never calls Connect.
func (a *Adapter) ReportError(err error) bool {
	if err == nil {
		return false
	}
	a.mu.Lock()
	r := a.current
	a.mu.Unlock()
	if r == nil || !r.admitCallback() {
		return false
	}
	defer r.callbacks.Done()
	select {
	case <-r.ready:
	default:
		return false
	}
	failure := a.classifyError(err, "operation", "whatsapp_operation_failed")
	r.requestFinish(failure)
	return true
}

func (a *Adapter) handleLifecycleEvent(event whatsapplive.LifecycleEvent) {
	defer func() {
		if recovered := recover(); recovered != nil {
			a.mu.Lock()
			r := a.current
			a.mu.Unlock()
			if r != nil {
				r.requestFinish(bridge.OpError{
					Class:       bridge.FailureTransient,
					Operation:   "event",
					Fingerprint: "whatsapp_event_handler_panic",
					Cause:       fmt.Errorf("panic in WhatsApp lifecycle event handler: %v\n%s", recovered, debug.Stack()),
				})
			}
		}
	}()

	a.mu.Lock()
	r := a.current
	pairing := a.pairing
	a.mu.Unlock()
	if pairing != nil {
		pairing.deliver(event)
	}
	if r == nil || !r.admitCallback() {
		return
	}
	defer r.callbacks.Done()
	r.handleEvent(event)
}

func (a *Adapter) cooldownFailure(operation string) *bridge.OpError {
	if a.cooldown == nil || a.now == nil {
		return nil
	}
	retryAt := a.cooldown()
	if retryAt.IsZero() || !a.now().Before(retryAt) {
		return nil
	}
	return &bridge.OpError{
		Class:       bridge.FailureRateLimited,
		Operation:   operation,
		RetryAt:     retryAt,
		Fingerprint: "whatsapp_temporary_ban",
		Cause:       &whatsapplive.TemporaryBanError{RetryAt: retryAt},
	}
}

func (a *Adapter) classifyError(err error, operation, fingerprint string) bridge.OpError {
	if err == nil {
		err = errors.New("WhatsApp transport ended without an error")
	}
	if failure, ok := asOpError(err); ok {
		return failure
	}
	var ban *whatsapplive.TemporaryBanError
	if errors.As(err, &ban) && ban != nil {
		return bridge.OpError{
			Class:       bridge.FailureRateLimited,
			Operation:   operation,
			RetryAt:     ban.RetryAt,
			Fingerprint: "whatsapp_temporary_ban",
			Cause:       err,
		}
	}
	if errors.Is(err, whatsmeow.ErrNotLoggedIn) || errors.Is(err, wastore.ErrDeviceDeleted) {
		return bridge.OpError{
			Class:       bridge.FailureReauthRequired,
			Operation:   operation,
			Fingerprint: "whatsapp_session_invalid",
			Cause:       err,
		}
	}
	if errors.Is(err, whatsmeow.ErrAlreadyConnected) ||
		strings.Contains(strings.ToLower(err.Error()), "already in progress") {
		return bridge.OpError{
			Class:       bridge.FailureMisconfigured,
			Operation:   operation,
			Fingerprint: "overlapping_whatsapp_connect",
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

func (a *Adapter) classifyEvent(event whatsapplive.LifecycleEvent) (bridge.OpError, bool) {
	if event.PersistenceError != nil {
		return bridge.OpError{
			Class:       bridge.FailureMisconfigured,
			Operation:   "persist_ban",
			Fingerprint: "whatsapp_ban_persistence_failed",
			Cause:       event.PersistenceError,
		}, true
	}
	switch raw := event.Raw.(type) {
	case *waevents.Disconnected:
		return transientEvent("disconnect", "whatsapp_disconnected", errors.New("WhatsApp disconnected")), true
	case *waevents.LoggedOut:
		return bridge.OpError{
			Class:       bridge.FailureReauthRequired,
			Operation:   "connect",
			Fingerprint: "whatsapp_logged_out",
			Cause:       errors.New(raw.PermanentDisconnectDescription()),
		}, true
	case *waevents.TemporaryBan:
		retryAt := event.RetryAt
		if retryAt.IsZero() && raw.Expire > 0 {
			retryAt = event.At.Add(raw.Expire)
		}
		if retryAt.IsZero() {
			return bridge.OpError{
				Class:       bridge.FailureReauthRequired,
				Operation:   "connect",
				Fingerprint: "whatsapp_temporary_ban_without_expiry",
				Cause:       errors.New(raw.PermanentDisconnectDescription()),
			}, true
		}
		return bridge.OpError{
			Class:       bridge.FailureRateLimited,
			Operation:   "connect",
			RetryAt:     retryAt,
			Fingerprint: fmt.Sprintf("whatsapp_temporary_ban_%d", raw.Code),
			Cause:       errors.New(raw.PermanentDisconnectDescription()),
		}, true
	case *waevents.ClientOutdated:
		return bridge.OpError{
			Class:       bridge.FailureUpgradeRequired,
			Operation:   "connect",
			Fingerprint: "whatsapp_client_outdated",
			Cause:       errors.New("WhatsApp client is outdated"),
		}, true
	case *waevents.ConnectFailure:
		class := bridge.FailureTransient
		if raw.Reason.IsLoggedOut() {
			class = bridge.FailureReauthRequired
		}
		return bridge.OpError{
			Class:       class,
			Operation:   "connect",
			Fingerprint: fmt.Sprintf("whatsapp_connect_failure_%d", raw.Reason),
			Cause:       errors.New(raw.PermanentDisconnectDescription()),
		}, true
	case *waevents.StreamReplaced:
		return transientEvent("connect", "whatsapp_stream_replaced", errors.New("WhatsApp stream replaced")), true
	case *waevents.ManualLoginReconnect:
		return transientEvent("connect", "whatsapp_manual_login_reconnect", errors.New("WhatsApp requested a supervised login reconnect")), true
	case *waevents.CATRefreshError:
		return transientEvent("connect", "whatsapp_cat_refresh_failed", raw.Error), true
	case *waevents.StreamError:
		return transientEvent("connect", "whatsapp_stream_error_"+raw.Code, errors.New("WhatsApp stream error")), true
	}
	return bridge.OpError{}, false
}

func transientEvent(operation, fingerprint string, cause error) bridge.OpError {
	return bridge.OpError{
		Class:       bridge.FailureTransient,
		Operation:   operation,
		Fingerprint: fingerprint,
		Cause:       cause,
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
	adapter *Adapter
	request bridge.StartRequest
	sink    bridge.ConnectionSink
	ctx     context.Context
	cancel  context.CancelFunc

	ready      chan struct{}
	done       chan error
	stopped    chan struct{}
	finish     chan error
	readyOnce  sync.Once
	finishOnce sync.Once

	removeIngress     func()
	removeIngressOnce sync.Once

	admissionMu sync.Mutex
	admitting   bool
	callbacks   sync.WaitGroup
	workers     sync.WaitGroup
}

func (r *run) Ready() <-chan struct{} { return r.ready }
func (r *run) Done() <-chan error     { return r.done }

func (r *run) connect() {
	defer r.workers.Done()
	if err := r.adapter.connect(r.ctx); err != nil && r.ctx.Err() == nil {
		r.requestFinish(r.adapter.classifyError(err, "connect", "whatsapp_connect_failed"))
	}
}

func (r *run) Probe(ctx context.Context) (bridge.Liveness, error) {
	if ctx == nil {
		return bridge.Liveness{}, errors.New("whatsapp probe: nil context")
	}
	if err := r.adapter.probe(ctx); err != nil {
		failure := r.adapter.classifyError(err, "probe", "whatsapp_keepalive_failed")
		return bridge.Liveness{}, failure
	}
	return bridge.Liveness{AliveAt: r.adapter.now(), Detail: "whatsapp_keepalive"}, nil
}

func (r *run) Stop(ctx context.Context) error {
	if ctx == nil {
		return errors.New("whatsapp run: nil stop context")
	}
	r.requestFinish(nil)
	select {
	case <-r.stopped:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *run) coordinate() {
	var terminal error
	select {
	case terminal = <-r.finish:
	case <-r.ctx.Done():
		terminal = r.ctx.Err()
	}

	r.closeAdmission()
	r.unregisterIngress()
	r.cancel()
	r.adapter.disconnect()
	r.workers.Wait()
	r.callbacks.Wait()
	r.adapter.clearCurrent(r)
	r.done <- terminal
	close(r.done)
	close(r.stopped)
}

func (r *run) unregisterIngress() {
	r.removeIngressOnce.Do(func() {
		if r.removeIngress != nil {
			r.removeIngress()
		}
	})
}

func (r *run) requestFinish(err error) {
	r.finishOnce.Do(func() {
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

type ingressErrorRecorder interface {
	RecordIngressError(accountID string)
}

func (r *run) handleIngress(frame whatsapplive.IngressFrame) {
	if !r.admitCallback() {
		return
	}
	defer r.callbacks.Done()
	if r.sink == nil {
		return
	}

	err := r.sink.AppendIngress(context.Background(), bridge.RawIngressRecord{
		AccountID:    r.request.AccountID,
		Generation:   r.request.Generation,
		DedupeKey:    frame.DedupeKey,
		Codec:        whatsapplive.IngressCodec,
		CodecVersion: whatsapplive.IngressCodecVersion,
		ReceivedAt:   frame.ReceivedAt,
		Payload:      frame.Payload,
	})
	if err == nil || errors.Is(err, bridge.ErrStaleGeneration) {
		return
	}
	if recorder, ok := r.sink.(ingressErrorRecorder); ok {
		recorder.RecordIngressError(r.request.AccountID)
	}
	if r.adapter.logIngressError != nil {
		r.adapter.logIngressError(err, r.request.AccountID, uint64(r.request.Generation))
	}
}

func (r *run) handleEvent(event whatsapplive.LifecycleEvent) {
	if _, ok := event.Raw.(*waevents.Connected); ok && event.Paired {
		r.readyOnce.Do(func() { close(r.ready) })
		if r.sink != nil {
			r.sink.Beat(r.request.Generation, event.At, "event:whatsapp.connected")
		}
		return
	}
	if event.Paired && eventProvidesLiveness(event.Raw) && r.sink != nil {
		r.sink.Beat(r.request.Generation, event.At, fmt.Sprintf("event:%T", event.Raw))
	}
	if failure, terminal := r.adapter.classifyEvent(event); terminal {
		r.requestFinish(failure)
	}
}

func eventProvidesLiveness(raw any) bool {
	switch raw.(type) {
	case *waevents.Message, *waevents.Receipt, *waevents.HistorySync,
		*waevents.GroupInfo, *waevents.ChatPresence, *waevents.KeepAliveRestored:
		return true
	default:
		return false
	}
}

func (a *Adapter) clearCurrent(candidate *run) {
	a.mu.Lock()
	if a.current == candidate {
		a.current = nil
	}
	a.mu.Unlock()
}

type pairAttempt struct {
	events chan whatsapplive.LifecycleEvent
}

func (p *pairAttempt) deliver(event whatsapplive.LifecycleEvent) {
	select {
	case p.events <- event:
	default:
		// Pairing emits only a small fixed set of lifecycle events. If an
		// unexpected event flood fills the channel, dropping non-terminal noise
		// is safer than blocking whatsmeow's event dispatcher.
	}
}

func (a *Adapter) Pair(
	ctx context.Context,
	req bridge.PairRequest,
	sink bridge.PairSink,
) (bridge.PairResult, error) {
	if ctx == nil {
		return bridge.PairResult{}, errors.New("whatsapp pair: nil context")
	}
	if err := ctx.Err(); err != nil {
		return bridge.PairResult{}, err
	}
	if req.AccountID != a.accountID {
		return bridge.PairResult{}, fmt.Errorf("whatsapp pair: account %q does not match %q", req.AccountID, a.accountID)
	}
	if failure := a.cooldownFailure("pair"); failure != nil {
		if sink != nil {
			_ = sink.EmitPairEvent(ctx, bridge.PairEvent{
				Kind:      "blocked",
				ExpiresAt: failure.RetryAt,
				Message:   failure.Fingerprint,
			})
		}
		return bridge.PairResult{}, *failure
	}
	if a.accountInfo().Paired {
		return bridge.PairResult{}, errors.New("WhatsApp is already paired")
	}

	attempt := &pairAttempt{events: make(chan whatsapplive.LifecycleEvent, 64)}
	a.mu.Lock()
	if a.current != nil || a.starting || a.pairing != nil {
		a.mu.Unlock()
		return bridge.PairResult{}, bridge.ErrPairingInProgress
	}
	a.pairing = attempt
	a.mu.Unlock()
	defer func() {
		a.disconnect()
		a.mu.Lock()
		if a.pairing == attempt {
			a.pairing = nil
		}
		a.mu.Unlock()
	}()

	switch req.Method {
	case bridge.PairPhoneCode:
		code, err := a.pairPhone(ctx, req.Phone)
		if err != nil {
			return bridge.PairResult{}, a.classifyError(err, "pair", "whatsapp_phone_pair_failed")
		}
		if sink != nil {
			if err := sink.EmitPairEvent(ctx, bridge.PairEvent{Kind: "code", Payload: code}); err != nil {
				return bridge.PairResult{}, err
			}
		}
		return a.waitPairResult(ctx, attempt, sink)
	case bridge.PairQR:
		qrItems, err := a.prepareQR(ctx)
		if err != nil {
			return bridge.PairResult{}, a.classifyError(err, "pair", "whatsapp_qr_prepare_failed")
		}
		if err := a.connectPairing(ctx); err != nil {
			return bridge.PairResult{}, a.classifyError(err, "pair", "whatsapp_qr_connect_failed")
		}
		return a.waitQRPairResult(ctx, attempt, sink, qrItems)
	default:
		return bridge.PairResult{}, bridge.OpError{
			Class:       bridge.FailureUnsupported,
			Operation:   "pair",
			Fingerprint: "whatsapp_pair_method_unsupported",
			Cause:       fmt.Errorf("unsupported WhatsApp pair method %q", req.Method),
		}
	}
}

func (a *Adapter) waitQRPairResult(
	ctx context.Context,
	attempt *pairAttempt,
	sink bridge.PairSink,
	items <-chan whatsmeow.QRChannelItem,
) (bridge.PairResult, error) {
	for {
		select {
		case event := <-attempt.events:
			if result, err, terminal := a.pairEventResult(ctx, event, sink); terminal {
				return result, err
			}
		case item, ok := <-items:
			if !ok {
				items = nil
				continue
			}
			snapshot := a.applyQR(item)
			if item.Event == whatsmeow.QRChannelEventCode {
				if sink != nil {
					// QRs rotate within one attempt. The UI expiry remains in the
					// legacy snapshot; omitting PairEvent.ExpiresAt prevents the
					// shared supervisor from cancelling at the first rotation.
					if err := sink.EmitPairEvent(ctx, bridge.PairEvent{
						Kind:    "qr",
						Payload: item.Code,
					}); err != nil {
						return bridge.PairResult{}, err
					}
				}
				continue
			}
			if item.Event != whatsmeow.QRChannelSuccess.Event {
				message := snapshot.Error
				if message == "" {
					message = item.Event
				}
				return bridge.PairResult{}, bridge.OpError{
					Class:       bridge.FailureTransient,
					Operation:   "pair",
					Fingerprint: "whatsapp_qr_" + item.Event,
					Cause:       errors.New(message),
				}
			}
		case <-ctx.Done():
			return bridge.PairResult{}, ctx.Err()
		}
	}
}

func (a *Adapter) waitPairResult(
	ctx context.Context,
	attempt *pairAttempt,
	sink bridge.PairSink,
) (bridge.PairResult, error) {
	for {
		select {
		case event := <-attempt.events:
			if result, err, terminal := a.pairEventResult(ctx, event, sink); terminal {
				return result, err
			}
		case <-ctx.Done():
			return bridge.PairResult{}, ctx.Err()
		}
	}
}

func (a *Adapter) pairEventResult(
	ctx context.Context,
	event whatsapplive.LifecycleEvent,
	sink bridge.PairSink,
) (bridge.PairResult, error, bool) {
	if event.PersistenceError != nil {
		failure, _ := a.classifyEvent(event)
		return bridge.PairResult{}, failure, true
	}
	switch raw := event.Raw.(type) {
	case *waevents.PairSuccess:
		return bridge.PairResult{
			RemoteAccountID: raw.ID.String(),
			RemoteDeviceID:  raw.LID.String(),
		}, nil, true
	case *waevents.Connected:
		if event.Paired {
			info := a.accountInfo()
			return bridge.PairResult{RemoteAccountID: info.JID}, nil, true
		}
	case *waevents.TemporaryBan:
		failure, _ := a.classifyEvent(event)
		if sink != nil {
			_ = sink.EmitPairEvent(ctx, bridge.PairEvent{
				Kind:      "blocked",
				ExpiresAt: failure.RetryAt,
				Message:   failure.Fingerprint,
			})
		}
		return bridge.PairResult{}, failure, true
	case *waevents.PairError:
		return bridge.PairResult{}, pairFailure(a.host, raw.Error, "whatsapp_pair_error"), true
	case *waevents.PairPasskeyRequest:
		return bridge.PairResult{}, pairFailure(a.host, nil, "whatsapp_pair_passkey_required"), true
	case *waevents.PairPasskeyError:
		return bridge.PairResult{}, pairFailure(a.host, raw.Error, "whatsapp_pair_passkey_error"), true
	case *waevents.QRScannedWithoutMultidevice:
		return bridge.PairResult{}, pairFailure(a.host, nil, "whatsapp_qr_multidevice_required"), true
	case *waevents.LoggedOut, *waevents.ClientOutdated, *waevents.ConnectFailure:
		failure, _ := a.classifyEvent(event)
		return bridge.PairResult{}, failure, true
	case *waevents.Disconnected, *waevents.StreamReplaced, *waevents.StreamError, *waevents.CATRefreshError:
		failure, _ := a.classifyEvent(event)
		failure.Operation = "pair"
		return bridge.PairResult{}, failure, true
	case *waevents.ManualLoginReconnect:
		// PairSuccess is dispatched before the pairing 515 handoff. Ignore the
		// handoff here; the successful result disconnects this transport and the
		// command control asks Supervisor to Start the paired lifecycle.
	}
	return bridge.PairResult{}, nil, false
}

func pairFailure(host *whatsapplive.Bridge, cause error, fingerprint string) bridge.OpError {
	if host != nil {
		if message := strings.TrimSpace(host.Status().LastError); message != "" {
			cause = errors.New(message)
		}
	}
	if cause == nil {
		cause = errors.New("WhatsApp pairing failed")
	}
	return bridge.OpError{
		Class:       bridge.FailureTransient,
		Operation:   "pair",
		Fingerprint: fingerprint,
		Cause:       cause,
	}
}

func (a *Adapter) Unpair(ctx context.Context, accountID string) error {
	if ctx == nil {
		return errors.New("whatsapp unpair: nil context")
	}
	if accountID != "" && accountID != a.accountID {
		return fmt.Errorf("whatsapp unpair: account %q does not match %q", accountID, a.accountID)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	a.mu.Lock()
	busy := a.current != nil || a.starting || a.pairing != nil
	a.mu.Unlock()
	if busy {
		return bridge.ErrSupervisorBusy
	}
	if a.unpair == nil {
		return bridge.ErrPairerUnavailable
	}
	return a.unpair()
}

var (
	_ bridge.Adapter            = (*Adapter)(nil)
	_ bridge.CapabilityDeclarer = (*Adapter)(nil)
	_ bridge.MediaSender        = (*Adapter)(nil)
	_ bridge.Pairer             = (*Adapter)(nil)
	_ bridge.Run                = (*run)(nil)
)
