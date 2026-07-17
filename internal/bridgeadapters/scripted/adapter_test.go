package scripted

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"testing"
	"time"

	"github.com/maxghenis/openmessage/internal/bridge"
)

func TestAdapterStartIsReadyAndCapturesConnection(t *testing.T) {
	t.Parallel()

	adapter := New("google-account", bridge.PlatformGoogle)
	sink := &recordingSink{}
	req := bridge.StartRequest{
		AccountID:       "google-account",
		DeviceID:        "device-1",
		RemoteAccountID: "remote-1",
		Generation:      7,
	}
	run, err := adapter.Start(context.Background(), req, sink)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	select {
	case <-run.Ready():
	default:
		t.Fatal("Ready() was not closed before Start returned")
	}
	if got := adapter.Sink(); got != sink {
		t.Fatalf("Sink() = %T %p, want %T %p", got, got, sink, sink)
	}
	if got := adapter.StartRequests(); !reflect.DeepEqual(got, []bridge.StartRequest{req}) {
		t.Fatalf("StartRequests() = %+v, want %+v", got, []bridge.StartRequest{req})
	}

	liveness, err := run.Probe(context.Background())
	if err != nil {
		t.Fatalf("Probe() error = %v", err)
	}
	if liveness.AliveAt.IsZero() || liveness.Detail != "scripted_ready" {
		t.Fatalf("Probe() = %+v, want non-zero scripted_ready liveness", liveness)
	}

	if err := run.Stop(context.Background()); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if terminal, ok := <-run.Done(); !ok || terminal != nil {
		t.Fatalf("first Done() receive = (%v, %v), want (nil, true)", terminal, ok)
	}
	if terminal, ok := <-run.Done(); ok || terminal != nil {
		t.Fatalf("second Done() receive = (%v, %v), want (nil, false)", terminal, ok)
	}
}

func TestAdapterScriptsTextCallsInFIFOOrder(t *testing.T) {
	t.Parallel()

	adapter := New("signal-account", bridge.PlatformSignal)
	wantResult := bridge.SendResult{
		RemoteMessageID: "signal-remote-1",
		AcceptedAt:      time.Unix(1_700_000_000, 0),
		EchoExpected:    true,
	}
	wantFailure := bridge.OpError{
		Class:       bridge.FailureRateLimited,
		Operation:   "send_text",
		RetryAt:     time.Unix(1_700_000_060, 0),
		Fingerprint: "scripted_rate_limit",
		Dispatch:    bridge.DispatchNotCalled,
		Cause:       errors.New("try later"),
	}
	adapter.EnqueueTextResult(wantResult)
	adapter.EnqueueTextError(wantFailure)

	req1 := bridge.TextRequest{
		AccountID:    "signal-account",
		Conversation: bridge.ConversationRef{RemoteID: "conversation-1"},
		Body:         "first",
		ReplyTo:      &bridge.MessageRef{RemoteID: "reply-1"},
		RequestID:    "request-1",
	}
	result, err := adapter.SendText(context.Background(), req1)
	if err != nil || !reflect.DeepEqual(result, wantResult) {
		t.Fatalf("first SendText() = (%+v, %v), want (%+v, nil)", result, err, wantResult)
	}
	req1.ReplyTo.RemoteID = "mutated-after-call"

	req2 := bridge.TextRequest{AccountID: "signal-account", Body: "second", RequestID: "request-2"}
	result, err = adapter.SendText(context.Background(), req2)
	if result != (bridge.SendResult{}) {
		t.Fatalf("second SendText() result = %+v, want zero", result)
	}
	assertOpError(t, err, wantFailure)

	_, err = adapter.SendText(context.Background(), bridge.TextRequest{RequestID: "request-3"})
	var exhausted bridge.OpError
	if !errors.As(err, &exhausted) {
		t.Fatalf("empty-script SendText() error = %T %v, want bridge.OpError", err, err)
	}
	if exhausted.Class != bridge.FailureTransient ||
		exhausted.Dispatch != bridge.DispatchNotCalled ||
		exhausted.Operation != "send_text" ||
		exhausted.Fingerprint != "scripted_text_queue_empty" {
		t.Fatalf("empty-script SendText() error = %+v", exhausted)
	}

	wantRequests := []bridge.TextRequest{
		{
			AccountID:    "signal-account",
			Conversation: bridge.ConversationRef{RemoteID: "conversation-1"},
			Body:         "first",
			ReplyTo:      &bridge.MessageRef{RemoteID: "reply-1"},
			RequestID:    "request-1",
		},
		req2,
		{RequestID: "request-3"},
	}
	if got := adapter.TextRequests(); !reflect.DeepEqual(got, wantRequests) {
		t.Fatalf("TextRequests() = %+v, want %+v", got, wantRequests)
	}
	got := adapter.TextRequests()
	got[0].ReplyTo.RemoteID = "mutated-snapshot"
	if again := adapter.TextRequests(); again[0].ReplyTo.RemoteID != "reply-1" {
		t.Fatalf("TextRequests() returned aliased reply: %+v", again[0].ReplyTo)
	}
}

func TestAdapterScriptsMediaAndCapturesBytes(t *testing.T) {
	t.Parallel()

	adapter := New("whatsapp-account", bridge.PlatformWhatsApp)
	wantResult := bridge.SendResult{RemoteMessageID: "wa-media-1", EchoExpected: true}
	wantFailure := bridge.OpError{
		Class:       bridge.FailureTransient,
		Operation:   "send_media",
		Fingerprint: "scripted_media_failure",
		Dispatch:    bridge.DispatchUncertain,
		Cause:       errors.New("connection reset"),
	}
	adapter.EnqueueMediaResult(wantResult)
	adapter.EnqueueMediaError(wantFailure)

	req1 := bridge.MediaRequest{
		AccountID:    "whatsapp-account",
		Conversation: bridge.ConversationRef{RemoteID: "wa-chat"},
		Reader:       bytes.NewReader([]byte("first payload")),
		Size:         int64(len("first payload")),
		Filename:     "first.txt",
		MIME:         "text/plain",
		Caption:      "caption",
		ReplyTo:      &bridge.MessageRef{RemoteID: "wa-reply"},
		RequestID:    "media-request-1",
	}
	result, err := adapter.SendMedia(context.Background(), req1)
	if err != nil || !reflect.DeepEqual(result, wantResult) {
		t.Fatalf("first SendMedia() = (%+v, %v), want (%+v, nil)", result, err, wantResult)
	}

	req2 := bridge.MediaRequest{Reader: bytes.NewReader([]byte("second payload")), RequestID: "media-request-2"}
	result, err = adapter.SendMedia(context.Background(), req2)
	if result != (bridge.SendResult{}) {
		t.Fatalf("second SendMedia() result = %+v, want zero", result)
	}
	assertOpError(t, err, wantFailure)

	calls := adapter.MediaRequests()
	if len(calls) != 2 {
		t.Fatalf("len(MediaRequests()) = %d, want 2", len(calls))
	}
	if string(calls[0].Data) != "first payload" || string(calls[1].Data) != "second payload" {
		t.Fatalf("MediaRequests() data = %q, %q", calls[0].Data, calls[1].Data)
	}
	if calls[0].Request.Reader != nil || calls[1].Request.Reader != nil {
		t.Fatal("MediaRequests() retained a caller-owned Reader")
	}
	if calls[0].Request.ReplyTo == req1.ReplyTo {
		t.Fatal("MediaRequests() retained a caller-owned ReplyTo pointer")
	}

	calls[0].Data[0] = 'X'
	calls[0].Request.ReplyTo.RemoteID = "mutated"
	again := adapter.MediaRequests()
	if string(again[0].Data) != "first payload" || again[0].Request.ReplyTo.RemoteID != "wa-reply" {
		t.Fatalf("MediaRequests() returned aliased state: %+v data=%q", again[0].Request, again[0].Data)
	}
}

func TestAdapterScriptsDownloadsAndRegistryCapabilities(t *testing.T) {
	t.Parallel()

	adapter := New("google-account", bridge.PlatformGoogle)
	adapter.EnqueueDownload([]byte("downloaded bytes"), "photo.jpg", "image/jpeg")
	wantFailure := bridge.OpError{
		Class:       bridge.FailureUnsupported,
		Operation:   "download_media",
		Fingerprint: "scripted_download_unsupported",
		Cause:       errors.New("missing remote object"),
	}
	adapter.EnqueueDownloadError(wantFailure)

	ref1 := bridge.MediaRef{RemoteID: "remote-media-1", Opaque: []byte("opaque-1"), Filename: "original.jpg"}
	stream, err := adapter.DownloadMedia(context.Background(), "google-account", ref1)
	if err != nil {
		t.Fatalf("first DownloadMedia() error = %v", err)
	}
	data, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("read first DownloadMedia() stream: %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("close first DownloadMedia() stream: %v", err)
	}
	if string(data) != "downloaded bytes" || stream.Size != int64(len(data)) ||
		stream.Filename != "photo.jpg" || stream.MIME != "image/jpeg" {
		t.Fatalf("first DownloadMedia() stream = data %q metadata %+v", data, stream)
	}

	ref1.Opaque[0] = 'X'
	_, err = adapter.DownloadMedia(context.Background(), "google-account", bridge.MediaRef{RemoteID: "remote-media-2"})
	assertOpError(t, err, wantFailure)
	requests := adapter.DownloadRequests()
	if len(requests) != 2 || string(requests[0].Ref.Opaque) != "opaque-1" {
		t.Fatalf("DownloadRequests() = %+v", requests)
	}

	registry := bridge.NewRegistry()
	if err := registry.Register(adapter); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	caps := registry.Capabilities("google-account")
	if !caps.TextSend || !caps.MediaSend || !caps.MediaDownload {
		t.Fatalf("Capabilities() = %+v, want text/media/download", caps)
	}
	for _, capability := range []bridge.Capability{
		bridge.CapabilityTextSend,
		bridge.CapabilityMediaSend,
		bridge.CapabilityMediaDownload,
	} {
		lease, err := registry.Acquire(context.Background(), "google-account", capability)
		if err != nil {
			t.Fatalf("Acquire(%q) error = %v", capability, err)
		}
		if lease.Text == nil || lease.Media == nil || lease.Download == nil {
			t.Fatalf("Acquire(%q) lease = %+v, want all scripted capabilities", capability, lease)
		}
		lease.Close()
	}
}

func TestAdapterRejectsUnclassifiedScriptErrors(t *testing.T) {
	t.Parallel()

	tests := map[string]func(*Adapter){
		"text":     func(adapter *Adapter) { adapter.EnqueueTextError(bridge.OpError{}) },
		"media":    func(adapter *Adapter) { adapter.EnqueueMediaError(bridge.OpError{}) },
		"download": func(adapter *Adapter) { adapter.EnqueueDownloadError(bridge.OpError{}) },
	}
	for name, enqueue := range tests {
		name, enqueue := name, enqueue
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			defer func() {
				if recovered := recover(); recovered == nil {
					t.Fatal("enqueueing an unclassified bridge.OpError did not panic")
				}
			}()
			enqueue(New("account", bridge.PlatformGoogle))
		})
	}
}

func assertOpError(t *testing.T, got error, want bridge.OpError) {
	t.Helper()
	var failure bridge.OpError
	if !errors.As(got, &failure) {
		t.Fatalf("error = %T %v, want bridge.OpError %+v", got, got, want)
	}
	if !reflect.DeepEqual(failure, want) {
		t.Fatalf("bridge.OpError = %+v, want %+v", failure, want)
	}
}

type recordingSink struct{}

func (*recordingSink) AppendIngress(context.Context, bridge.RawIngressRecord) error { return nil }

func (*recordingSink) EmitEphemeral(context.Context, bridge.EphemeralEvent) error { return nil }

func (*recordingSink) Beat(bridge.Generation, time.Time, string) {}

var (
	_ bridge.Adapter         = (*Adapter)(nil)
	_ bridge.TextSender      = (*Adapter)(nil)
	_ bridge.MediaSender     = (*Adapter)(nil)
	_ bridge.MediaDownloader = (*Adapter)(nil)
	_ bridge.ConnectionSink  = (*recordingSink)(nil)
)
