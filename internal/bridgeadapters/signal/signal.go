// Package signal adapts one retained signallive signal-cli poller lifecycle to
// bridge.Run and durable media dispatch. Legacy send, media, reaction,
// recovery, and QR/status paths remain on signallive.Bridge.
package signal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/signallive"
)

type poller interface {
	StartPoller(context.Context) (signallive.PollerRun, error)
	SendTextRequest(string, string, string) (int64, error)
	SendReactionRequest(string, string, string, string, string) error
	SendMediaRequest(string, io.Reader, int64, string, string, string, string) (int64, error)
	DownloadMediaRef(string, string, string, string, string, bool) ([]byte, string, error)
	Status() signallive.StatusSnapshot
	ApplyPollerFailure(signallive.PollerExit)
	InputFingerprint() string
	UnpairContext(context.Context) error
}

// signalDownloadOpaqueV1 is the Signal-owned Wave-4 -> M4b wire contract.
// The Wave-4 Signal decoder MUST pack conversation_id for every remote
// attachment: getAttachment needs a recipient or group target, while
// bridge.MediaDownloader intentionally carries no conversation argument.
// No v2 decoder writes remote_ref yet, so this adapter is unit-testable but the
// production path remains inert until Wave-4 ingest starts packing this shape.
type signalDownloadOpaqueV1 struct {
	Version        int    `json:"v"`
	Kind           string `json:"kind"`
	AttachmentID   string `json:"att_id"`
	Path           string `json:"path"`
	ConversationID string `json:"conversation_id"`
}

type decodedSignalDownloadRef struct {
	payload signalDownloadOpaqueV1
	target  string
	isGroup bool
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
	// ReadReceipts deliberately remains undeclared. A 1:1 signal-cli
	// sendReceipt adapter is deferred to a follow-up because sendReceipt has no
	// --group-id and group receipt semantics cannot yet be served honestly.
	return bridge.CapabilitySet{
		TextSend:      true,
		MediaSend:     true,
		Reactions:     true,
		PairQR:        true,
		MediaDownload: true,
	}
}

// SendText adapts a durable text request to signal-cli's structured send path.
// Signal's canonical identity is the returned timestamp; RequestID remains
// local-dedupe metadata, so an uncertain retry can duplicate until M5
// reconciliation makes that retry safe.
func (a *Adapter) SendText(
	ctx context.Context,
	req bridge.TextRequest,
) (bridge.SendResult, error) {
	if ctx == nil {
		return bridge.SendResult{}, bridge.OpError{
			Class:       bridge.FailureTransient,
			Operation:   "send_text",
			Fingerprint: "signal_send_text_context_missing",
			Dispatch:    bridge.DispatchNotCalled,
			Cause:       errors.New("Signal text send context is nil"),
		}
	}
	if err := ctx.Err(); err != nil {
		return bridge.SendResult{}, bridge.OpError{
			Class:       bridge.FailureTransient,
			Operation:   "send_text",
			Fingerprint: "signal_send_text_context_done",
			Dispatch:    bridge.DispatchNotCalled,
			Cause:       err,
		}
	}
	if a == nil || a.poller == nil {
		return bridge.SendResult{}, signalNotConnectedError("send_text")
	}
	status := a.poller.Status()
	if !status.Connected || strings.TrimSpace(status.Account) == "" {
		return bridge.SendResult{}, signalNotConnectedError("send_text")
	}

	replyToID := ""
	if req.ReplyTo != nil {
		replyToID = req.ReplyTo.RemoteID
	}
	timestampMS, err := a.poller.SendTextRequest(
		req.Conversation.RemoteID,
		req.Body,
		replyToID,
	)
	if err != nil {
		a.ReportError(err)
		failure := bridge.OpError{
			Class:       bridge.FailureTransient,
			Operation:   "send_text",
			Fingerprint: "signal_send_text_failed",
			Cause:       err,
		}
		if !signallive.IsCommandError(err) || signallive.IsSendNotDispatchedError(err) {
			failure.Dispatch = bridge.DispatchNotCalled
		}
		return bridge.SendResult{}, failure
	}

	return bridge.SendResult{
		RemoteMessageID: strconv.FormatInt(timestampMS, 10),
		EchoExpected:    false,
	}, nil
}

// SendReaction adapts a durable reaction request to Signal's store-free
// signal-cli path. Reactions intentionally confirm without a remote result ID.
func (a *Adapter) SendReaction(
	ctx context.Context,
	req bridge.ReactionRequest,
) (bridge.SendResult, error) {
	if ctx == nil {
		return bridge.SendResult{}, bridge.OpError{
			Class:       bridge.FailureTransient,
			Operation:   "send_reaction",
			Fingerprint: "signal_send_reaction_context_missing",
			Dispatch:    bridge.DispatchNotCalled,
			Cause:       errors.New("Signal reaction send context is nil"),
		}
	}
	if err := ctx.Err(); err != nil {
		return bridge.SendResult{}, bridge.OpError{
			Class:       bridge.FailureTransient,
			Operation:   "send_reaction",
			Fingerprint: "signal_send_reaction_context_done",
			Dispatch:    bridge.DispatchNotCalled,
			Cause:       err,
		}
	}
	if a == nil || a.poller == nil {
		return bridge.SendResult{}, signalNotConnectedError("send_reaction")
	}
	status := a.poller.Status()
	if !status.Connected || strings.TrimSpace(status.Account) == "" {
		return bridge.SendResult{}, signalNotConnectedError("send_reaction")
	}

	err := a.poller.SendReactionRequest(
		req.Conversation.RemoteID,
		req.Target.RemoteID,
		req.Target.AuthorID,
		req.Emoji,
		string(req.Action),
	)
	if err != nil {
		a.ReportError(err)
		failure := bridge.OpError{
			Class:       bridge.FailureTransient,
			Operation:   "send_reaction",
			Fingerprint: "signal_reaction_failed",
			Cause:       err,
		}
		if !signallive.IsCommandError(err) || signallive.IsSendNotDispatchedError(err) {
			failure.Dispatch = bridge.DispatchNotCalled
		}
		return bridge.SendResult{}, failure
	}

	return bridge.SendResult{}, nil
}

func (a *Adapter) SendMedia(
	ctx context.Context,
	req bridge.MediaRequest,
) (bridge.SendResult, error) {
	if ctx == nil {
		return bridge.SendResult{}, bridge.OpError{
			Class:       bridge.FailureTransient,
			Operation:   "send_media",
			Fingerprint: "signal_send_media_context_missing",
			Dispatch:    bridge.DispatchNotCalled,
			Cause:       errors.New("Signal media send context is nil"),
		}
	}
	if err := ctx.Err(); err != nil {
		return bridge.SendResult{}, bridge.OpError{
			Class:       bridge.FailureTransient,
			Operation:   "send_media",
			Fingerprint: "signal_send_media_context_done",
			Dispatch:    bridge.DispatchNotCalled,
			Cause:       err,
		}
	}
	if a == nil || a.poller == nil {
		return bridge.SendResult{}, signalNotConnectedError("send_media")
	}
	status := a.poller.Status()
	if !status.Connected || strings.TrimSpace(status.Account) == "" {
		return bridge.SendResult{}, signalNotConnectedError("send_media")
	}

	replyToID := ""
	if req.ReplyTo != nil {
		replyToID = req.ReplyTo.RemoteID
	}
	timestampMS, err := a.poller.SendMediaRequest(
		req.Conversation.RemoteID,
		req.Reader,
		req.Size,
		req.Filename,
		req.MIME,
		req.Caption,
		replyToID,
	)
	if err != nil {
		a.ReportError(err)
		failure := bridge.OpError{
			Class:       bridge.FailureTransient,
			Operation:   "send_media",
			Fingerprint: "signal_send_media_failed",
			Cause:       err,
		}
		if !signallive.IsCommandError(err) || signallive.IsSendNotDispatchedError(err) {
			failure.Dispatch = bridge.DispatchNotCalled
		}
		return bridge.SendResult{}, failure
	}

	return bridge.SendResult{
		RemoteMessageID: strconv.FormatInt(timestampMS, 10),
		EchoExpected:    false,
	}, nil
}

// DownloadMedia adapts Signal's retained fully-buffered attachment downloader
// to bridge.MediaStream. This forfeits true streaming, matching the retained
// signal-cli boundary until a future transport exposes a streaming response.
func (a *Adapter) DownloadMedia(
	ctx context.Context,
	accountID string,
	ref bridge.MediaRef,
) (bridge.MediaStream, error) {
	_ = accountID // Logical registry ID; the ready status owns the Signal account.
	if ctx == nil {
		return bridge.MediaStream{}, bridge.OpError{
			Class:       bridge.FailureTransient,
			Operation:   "download_media",
			Fingerprint: "signal_download_media_context_missing",
			Cause:       errors.New("Signal media download context is nil"),
		}
	}
	if err := ctx.Err(); err != nil {
		return bridge.MediaStream{}, bridge.OpError{
			Class:       bridge.FailureTransient,
			Operation:   "download_media",
			Fingerprint: "signal_download_media_context_done",
			Cause:       err,
		}
	}
	if a == nil || a.poller == nil {
		return bridge.MediaStream{}, signalDownloadNotConnectedError()
	}
	status := a.poller.Status()
	account := strings.TrimSpace(status.Account)
	if !status.Connected || account == "" {
		return bridge.MediaStream{}, signalDownloadNotConnectedError()
	}

	decoded, err := decodeSignalDownloadOpaque(ref.Opaque)
	if err != nil {
		return bridge.MediaStream{}, err
	}
	data, transportMIME, err := a.poller.DownloadMediaRef(
		account,
		decoded.payload.Kind,
		decoded.payload.AttachmentID,
		decoded.payload.Path,
		decoded.target,
		decoded.isGroup,
	)
	if err != nil {
		// Preserve C6: ReportError only acts on unambiguous local-account
		// CommandErrors and never widens recipient/network failures into receive
		// lifecycle transitions.
		a.ReportError(err)
		return bridge.MediaStream{}, bridge.OpError{
			Class:       bridge.FailureTransient,
			Operation:   "download_media",
			Fingerprint: "signal_download_media_failed",
			Cause:       err,
		}
	}

	return bridge.MediaStream{
		ReadCloser: io.NopCloser(bytes.NewReader(data)),
		Size:       int64(len(data)),
		Filename:   ref.Filename,
		MIME:       firstNonEmpty(transportMIME, ref.MIME),
	}, nil
}

// ValidateDownloadOpaque reports whether raw is a well-formed v1 download Opaque this
// adapter can decode. It is the exported round-trip check the Wave-4
// migration uses to prove the refs it packs will unpack here.
func ValidateDownloadOpaque(raw []byte) error {
	_, err := decodeSignalDownloadOpaque(raw)
	return err
}

func decodeSignalDownloadOpaque(raw []byte) (decodedSignalDownloadRef, error) {
	if !utf8.Valid(raw) {
		return decodedSignalDownloadRef{}, signalDownloadOpaqueError(
			"signal_download_media_opaque_invalid_utf8",
			errors.New("Signal media Opaque is not valid UTF-8"),
		)
	}
	var payload signalDownloadOpaqueV1
	if err := json.Unmarshal(raw, &payload); err != nil {
		return decodedSignalDownloadRef{}, signalDownloadOpaqueError(
			"signal_download_media_opaque_malformed",
			fmt.Errorf("decode Signal media Opaque JSON: %w", err),
		)
	}
	if payload.Version != 1 {
		return decodedSignalDownloadRef{}, signalDownloadOpaqueError(
			"opaque_version_unsupported",
			fmt.Errorf("unsupported Signal media Opaque version %d", payload.Version),
		)
	}
	if payload.Kind == "" {
		return decodedSignalDownloadRef{}, signalDownloadOpaqueError(
			"signal_download_media_kind_missing",
			errors.New("Signal media Opaque requires kind"),
		)
	}

	switch payload.Kind {
	case "remote":
		payload.AttachmentID = strings.TrimSpace(payload.AttachmentID)
		payload.ConversationID = strings.TrimSpace(payload.ConversationID)
		if payload.AttachmentID == "" || payload.ConversationID == "" {
			return decodedSignalDownloadRef{}, signalDownloadOpaqueError(
				"signal_download_media_remote_missing_field",
				errors.New("Signal remote media Opaque requires att_id and conversation_id"),
			)
		}
		target, isGroup, err := signallive.ParseConversationTarget(payload.ConversationID)
		if err != nil {
			return decodedSignalDownloadRef{}, signalDownloadOpaqueError(
				"signal_download_media_conversation_invalid",
				fmt.Errorf("parse Signal media conversation_id: %w", err),
			)
		}
		return decodedSignalDownloadRef{
			payload: payload,
			target:  target,
			isGroup: isGroup,
		}, nil
	case "local":
		if strings.TrimSpace(payload.Path) == "" {
			return decodedSignalDownloadRef{}, signalDownloadOpaqueError(
				"signal_download_media_local_missing_field",
				errors.New("Signal local media Opaque requires path"),
			)
		}
		return decodedSignalDownloadRef{payload: payload}, nil
	default:
		return decodedSignalDownloadRef{}, signalDownloadOpaqueError(
			"signal_download_media_kind_unsupported",
			fmt.Errorf("unsupported Signal media Opaque kind %q", payload.Kind),
		)
	}
}

// Structural Opaque failures are terminal unsupported errors: retrying the
// same persisted bytes can never make invalid UTF-8/JSON or missing fields
// decodable. Each structural category has its own fingerprint; version skew
// uses the cross-platform opaque_version_unsupported contract.
func signalDownloadOpaqueError(fingerprint string, cause error) bridge.OpError {
	return bridge.OpError{
		Class:       bridge.FailureUnsupported,
		Operation:   "download_media",
		Fingerprint: fingerprint,
		Cause:       cause,
	}
}

func signalDownloadNotConnectedError() bridge.OpError {
	return bridge.OpError{
		Class:       bridge.FailureTransient,
		Operation:   "download_media",
		Fingerprint: "signal_not_connected",
		Cause:       errors.New("Signal is not connected"),
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func signalNotConnectedError(operation string) bridge.OpError {
	return bridge.OpError{
		Class:       bridge.FailureTransient,
		Operation:   operation,
		Fingerprint: "signal_not_connected",
		Dispatch:    bridge.DispatchNotCalled,
		Cause:       errors.New("Signal is not connected"),
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
	_ bridge.TextSender         = (*Adapter)(nil)
	_ bridge.ReactionSender     = (*Adapter)(nil)
	_ bridge.MediaSender        = (*Adapter)(nil)
	_ bridge.MediaDownloader    = (*Adapter)(nil)
	_ bridge.Run                = (*run)(nil)
)
