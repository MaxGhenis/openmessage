// Package scripted provides a deterministic, in-memory bridge adapter for
// hermetic integration tests. It owns no real transport and performs no
// network I/O.
package scripted

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/maxghenis/openmessage/internal/bridge"
)

// MediaCall is one captured MediaSender invocation. Data contains the bytes
// read from the request's one-shot Reader; Request.Reader is always nil.
type MediaCall struct {
	Request bridge.MediaRequest
	Data    []byte
}

// DownloadCall is one captured MediaDownloader invocation.
type DownloadCall struct {
	AccountID string
	Ref       bridge.MediaRef
}

type sendStep struct {
	result  bridge.SendResult
	failure *bridge.OpError
}

type downloadStep struct {
	data     []byte
	filename string
	mime     string
	failure  *bridge.OpError
}

// Adapter is a FIFO-scripted bridge.Adapter. The concrete type deliberately
// implements TextSender, MediaSender, and MediaDownloader so registering it
// exposes all three capabilities through bridge.Registry.
type Adapter struct {
	accountID string
	platform  bridge.Platform

	mu sync.Mutex

	sink          bridge.ConnectionSink
	startRequests []bridge.StartRequest

	textSteps    []sendStep
	textRequests []bridge.TextRequest

	mediaSteps    []sendStep
	mediaRequests []MediaCall

	downloadSteps    []downloadStep
	downloadRequests []DownloadCall
}

// New constructs a deterministic adapter for one bridge account.
func New(accountID string, platform bridge.Platform) *Adapter {
	return &Adapter{accountID: accountID, platform: platform}
}

func (a *Adapter) AccountID() string {
	if a == nil {
		return ""
	}
	return a.accountID
}

func (a *Adapter) Platform() bridge.Platform {
	if a == nil {
		return ""
	}
	return a.platform
}

// Start captures the generation's ConnectionSink and returns an immediately
// ready run. The run remains active until its parent context ends or Stop is
// called.
func (a *Adapter) Start(
	ctx context.Context,
	req bridge.StartRequest,
	sink bridge.ConnectionSink,
) (bridge.Run, error) {
	if ctx == nil {
		return nil, errors.New("scripted lifecycle: nil start context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if a == nil {
		return nil, bridge.OpError{
			Class:       bridge.FailureMisconfigured,
			Operation:   "connect",
			Fingerprint: "scripted_adapter_nil",
			Cause:       errors.New("scripted adapter is nil"),
		}
	}
	if req.AccountID != a.accountID {
		return nil, bridge.OpError{
			Class:       bridge.FailureMisconfigured,
			Operation:   "connect",
			Fingerprint: "scripted_account_mismatch",
			Cause: fmt.Errorf(
				"start account %q does not match adapter account %q",
				req.AccountID,
				a.accountID,
			),
		}
	}

	a.mu.Lock()
	a.sink = sink
	a.startRequests = append(a.startRequests, req)
	a.mu.Unlock()

	runCtx, cancel := context.WithCancel(ctx)
	r := &run{
		ctx:       runCtx,
		cancel:    cancel,
		ready:     make(chan struct{}),
		done:      make(chan error, 1),
		stopped:   make(chan struct{}),
		finish:    make(chan error, 1),
		startedAt: time.Now(),
	}
	close(r.ready)
	go r.coordinate()
	return r, nil
}

// Sink returns the ConnectionSink captured by the most recent successful
// Start call.
func (a *Adapter) Sink() bridge.ConnectionSink {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sink
}

// StartRequests returns a race-safe snapshot of successful Start calls.
func (a *Adapter) StartRequests() []bridge.StartRequest {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]bridge.StartRequest(nil), a.startRequests...)
}

// EnqueueTextResult appends one successful SendText outcome.
func (a *Adapter) EnqueueTextResult(result bridge.SendResult) {
	a.mu.Lock()
	a.textSteps = append(a.textSteps, sendStep{result: result})
	a.mu.Unlock()
}

// EnqueueTextError appends one classified SendText failure. The OpError is
// returned unchanged when its step is consumed.
func (a *Adapter) EnqueueTextError(failure bridge.OpError) {
	step := sendStep{failure: copyOpError(failure)}
	a.mu.Lock()
	a.textSteps = append(a.textSteps, step)
	a.mu.Unlock()
}

func (a *Adapter) SendText(
	_ context.Context,
	req bridge.TextRequest,
) (bridge.SendResult, error) {
	if a == nil {
		return bridge.SendResult{}, nilAdapterFailure("send_text")
	}

	a.mu.Lock()
	a.textRequests = append(a.textRequests, cloneTextRequest(req))
	if len(a.textSteps) == 0 {
		a.mu.Unlock()
		return bridge.SendResult{}, queueEmptyFailure(
			"send_text",
			"scripted_text_queue_empty",
			true,
		)
	}
	step := a.textSteps[0]
	a.textSteps = a.textSteps[1:]
	a.mu.Unlock()

	if step.failure != nil {
		return bridge.SendResult{}, *step.failure
	}
	return step.result, nil
}

// TextRequests returns a race-safe, deep-copied snapshot of SendText calls.
func (a *Adapter) TextRequests() []bridge.TextRequest {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	requests := make([]bridge.TextRequest, len(a.textRequests))
	for i := range a.textRequests {
		requests[i] = cloneTextRequest(a.textRequests[i])
	}
	return requests
}

// EnqueueMediaResult appends one successful SendMedia outcome.
func (a *Adapter) EnqueueMediaResult(result bridge.SendResult) {
	a.mu.Lock()
	a.mediaSteps = append(a.mediaSteps, sendStep{result: result})
	a.mu.Unlock()
}

// EnqueueMediaError appends one classified SendMedia failure. The OpError is
// returned unchanged when its step is consumed.
func (a *Adapter) EnqueueMediaError(failure bridge.OpError) {
	step := sendStep{failure: copyOpError(failure)}
	a.mu.Lock()
	a.mediaSteps = append(a.mediaSteps, step)
	a.mu.Unlock()
}

func (a *Adapter) SendMedia(
	_ context.Context,
	req bridge.MediaRequest,
) (bridge.SendResult, error) {
	if a == nil {
		return bridge.SendResult{}, nilAdapterFailure("send_media")
	}

	var (
		data    []byte
		readErr error
	)
	if req.Reader == nil {
		readErr = errors.New("scripted media request has a nil reader")
	} else {
		data, readErr = io.ReadAll(req.Reader)
	}
	captured := MediaCall{Request: cloneMediaRequest(req), Data: cloneBytes(data)}

	a.mu.Lock()
	a.mediaRequests = append(a.mediaRequests, captured)
	if readErr != nil {
		a.mu.Unlock()
		return bridge.SendResult{}, bridge.OpError{
			Class:       bridge.FailureTransient,
			Operation:   "send_media",
			Fingerprint: "scripted_media_read_failed",
			Dispatch:    bridge.DispatchNotCalled,
			Cause:       fmt.Errorf("read scripted media request: %w", readErr),
		}
	}
	if len(a.mediaSteps) == 0 {
		a.mu.Unlock()
		return bridge.SendResult{}, queueEmptyFailure(
			"send_media",
			"scripted_media_queue_empty",
			true,
		)
	}
	step := a.mediaSteps[0]
	a.mediaSteps = a.mediaSteps[1:]
	a.mu.Unlock()

	if step.failure != nil {
		return bridge.SendResult{}, *step.failure
	}
	return step.result, nil
}

// MediaRequests returns a race-safe, deep-copied snapshot of SendMedia calls.
func (a *Adapter) MediaRequests() []MediaCall {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	requests := make([]MediaCall, len(a.mediaRequests))
	for i := range a.mediaRequests {
		requests[i] = cloneMediaCall(a.mediaRequests[i])
	}
	return requests
}

// EnqueueDownload appends one successful DownloadMedia outcome. Data is copied
// at enqueue time and each call receives a fresh reader over another copy.
func (a *Adapter) EnqueueDownload(data []byte, filename, mime string) {
	a.mu.Lock()
	a.downloadSteps = append(a.downloadSteps, downloadStep{
		data:     cloneBytes(data),
		filename: filename,
		mime:     mime,
	})
	a.mu.Unlock()
}

// EnqueueDownloadError appends one classified DownloadMedia failure. The
// OpError is returned unchanged when its step is consumed.
func (a *Adapter) EnqueueDownloadError(failure bridge.OpError) {
	step := downloadStep{failure: copyOpError(failure)}
	a.mu.Lock()
	a.downloadSteps = append(a.downloadSteps, step)
	a.mu.Unlock()
}

func (a *Adapter) DownloadMedia(
	_ context.Context,
	accountID string,
	ref bridge.MediaRef,
) (bridge.MediaStream, error) {
	if a == nil {
		return bridge.MediaStream{}, nilAdapterFailure("download_media")
	}

	a.mu.Lock()
	a.downloadRequests = append(a.downloadRequests, DownloadCall{
		AccountID: accountID,
		Ref:       cloneMediaRef(ref),
	})
	if len(a.downloadSteps) == 0 {
		a.mu.Unlock()
		return bridge.MediaStream{}, queueEmptyFailure(
			"download_media",
			"scripted_download_queue_empty",
			false,
		)
	}
	step := a.downloadSteps[0]
	a.downloadSteps = a.downloadSteps[1:]
	a.mu.Unlock()

	if step.failure != nil {
		return bridge.MediaStream{}, *step.failure
	}
	data := cloneBytes(step.data)
	return bridge.MediaStream{
		ReadCloser: io.NopCloser(bytes.NewReader(data)),
		Size:       int64(len(data)),
		Filename:   step.filename,
		MIME:       step.mime,
	}, nil
}

// DownloadRequests returns a race-safe, deep-copied snapshot of DownloadMedia
// calls.
func (a *Adapter) DownloadRequests() []DownloadCall {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	requests := make([]DownloadCall, len(a.downloadRequests))
	for i := range a.downloadRequests {
		requests[i] = DownloadCall{
			AccountID: a.downloadRequests[i].AccountID,
			Ref:       cloneMediaRef(a.downloadRequests[i].Ref),
		}
	}
	return requests
}

func cloneTextRequest(req bridge.TextRequest) bridge.TextRequest {
	cloned := req
	cloned.ReplyTo = cloneMessageRef(req.ReplyTo)
	return cloned
}

func cloneMediaRequest(req bridge.MediaRequest) bridge.MediaRequest {
	cloned := req
	cloned.Reader = nil
	cloned.ReplyTo = cloneMessageRef(req.ReplyTo)
	return cloned
}

func cloneMediaCall(call MediaCall) MediaCall {
	return MediaCall{
		Request: cloneMediaRequest(call.Request),
		Data:    cloneBytes(call.Data),
	}
}

func cloneMessageRef(ref *bridge.MessageRef) *bridge.MessageRef {
	if ref == nil {
		return nil
	}
	cloned := *ref
	return &cloned
}

func cloneMediaRef(ref bridge.MediaRef) bridge.MediaRef {
	cloned := ref
	cloned.Opaque = cloneBytes(ref.Opaque)
	return cloned
}

func cloneBytes(data []byte) []byte {
	return append([]byte(nil), data...)
}

func copyOpError(failure bridge.OpError) *bridge.OpError {
	if failure.Class == "" {
		panic("scripted: bridge.OpError must have a failure class")
	}
	cloned := failure
	return &cloned
}

func nilAdapterFailure(operation string) bridge.OpError {
	return bridge.OpError{
		Class:       bridge.FailureMisconfigured,
		Operation:   operation,
		Fingerprint: "scripted_adapter_nil",
		Dispatch:    dispatchForOperation(operation),
		Cause:       errors.New("scripted adapter is nil"),
	}
}

func queueEmptyFailure(operation, fingerprint string, dispatchedOperation bool) bridge.OpError {
	failure := bridge.OpError{
		Class:       bridge.FailureTransient,
		Operation:   operation,
		Fingerprint: fingerprint,
		Cause:       errors.New("scripted outcome queue is empty"),
	}
	if dispatchedOperation {
		failure.Dispatch = bridge.DispatchNotCalled
	}
	return failure
}

func dispatchForOperation(operation string) bridge.DispatchCertainty {
	if operation == "send_text" || operation == "send_media" {
		return bridge.DispatchNotCalled
	}
	return ""
}

type run struct {
	ctx       context.Context
	cancel    context.CancelFunc
	ready     chan struct{}
	done      chan error
	stopped   chan struct{}
	finish    chan error
	startedAt time.Time

	finishOnce sync.Once
}

func (r *run) Ready() <-chan struct{} { return r.ready }

func (r *run) Done() <-chan error { return r.done }

func (r *run) Probe(ctx context.Context) (bridge.Liveness, error) {
	if ctx == nil {
		return bridge.Liveness{}, errors.New("scripted run: nil probe context")
	}
	if err := ctx.Err(); err != nil {
		return bridge.Liveness{}, err
	}
	if err := r.ctx.Err(); err != nil {
		return bridge.Liveness{}, bridge.OpError{
			Class:       bridge.FailureTransient,
			Operation:   "probe",
			Fingerprint: "scripted_run_stopped",
			Cause:       err,
		}
	}
	return bridge.Liveness{AliveAt: r.startedAt, Detail: "scripted_ready"}, nil
}

func (r *run) Stop(ctx context.Context) error {
	if ctx == nil {
		return errors.New("scripted run: nil stop context")
	}
	select {
	case <-r.stopped:
		return nil
	default:
	}
	r.finishOnce.Do(func() { r.finish <- nil })
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
	r.cancel()
	r.done <- terminal
	close(r.done)
	close(r.stopped)
}

var (
	_ bridge.Adapter         = (*Adapter)(nil)
	_ bridge.TextSender      = (*Adapter)(nil)
	_ bridge.MediaSender     = (*Adapter)(nil)
	_ bridge.MediaDownloader = (*Adapter)(nil)
	_ bridge.Run             = (*run)(nil)
)
