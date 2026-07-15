package whatsapp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/whatsapplive"
)

func TestAdapterImplementsMediaSender(t *testing.T) {
	var _ bridge.MediaSender = (*Adapter)(nil)
}

func TestAdapterImplementsTextSender(t *testing.T) {
	var _ bridge.TextSender = (*Adapter)(nil)
}

func TestSendTextRequiresReadyTransportWithoutLifecycleAction(t *testing.T) {
	var connectCalls, sendCalls int
	a := &Adapter{
		accountID: "whatsapp-primary",
		connect: func(context.Context) error {
			connectCalls++
			return nil
		},
		mediaReady: func() bool { return false },
		sendTextRequest: func(string, string, string, string) (string, time.Time, error) {
			sendCalls++
			return "", time.Time{}, nil
		},
	}

	result, err := a.SendText(context.Background(), bridge.TextRequest{})
	if result != (bridge.SendResult{}) {
		t.Fatalf("SendText() result = %+v, want zero result", result)
	}
	failure, ok := asOpError(err)
	if !ok {
		t.Fatalf("SendText() error = %T %v, want bridge.OpError", err, err)
	}
	if failure.Class != bridge.FailureTransient ||
		failure.Dispatch != bridge.DispatchNotCalled ||
		failure.Operation != "send_text" ||
		failure.Fingerprint != "whatsapp_not_connected" ||
		!errors.Is(failure.Cause, whatsmeow.ErrNotConnected) {
		t.Fatalf("SendText() failure = %+v, want classified not-connected pre-call failure", failure)
	}
	if connectCalls != 0 {
		t.Fatalf("connect calls = %d, want zero", connectCalls)
	}
	if sendCalls != 0 {
		t.Fatalf("transport send calls = %d, want zero", sendCalls)
	}
}

func TestSendTextContextGuardsRunBeforeTransport(t *testing.T) {
	tests := []struct {
		name            string
		ctx             context.Context
		wantFingerprint string
		wantCause       error
	}{
		{
			name:            "nil context",
			ctx:             nil,
			wantFingerprint: "whatsapp_text_context_missing",
		},
		{
			name:            "done context",
			ctx:             canceledTextSendContext(),
			wantFingerprint: "whatsapp_text_context_done",
			wantCause:       context.Canceled,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var sendCalls int
			a := &Adapter{
				mediaReady: func() bool { return true },
				sendTextRequest: func(string, string, string, string) (string, time.Time, error) {
					sendCalls++
					return "unexpected", time.Time{}, nil
				},
			}

			_, err := a.SendText(test.ctx, bridge.TextRequest{})
			failure, ok := asOpError(err)
			if !ok || failure.Class != bridge.FailureTransient ||
				failure.Dispatch != bridge.DispatchNotCalled ||
				failure.Operation != "send_text" ||
				failure.Fingerprint != test.wantFingerprint {
				t.Fatalf("SendText() error = %v, want context pre-call failure %q", err, test.wantFingerprint)
			}
			if test.wantCause != nil && !errors.Is(failure.Cause, test.wantCause) {
				t.Fatalf("SendText() cause = %v, want %v", failure.Cause, test.wantCause)
			}
			if sendCalls != 0 {
				t.Fatalf("transport send calls = %d, want zero", sendCalls)
			}
		})
	}
}

func canceledTextSendContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

func TestSendTextSuccessMapping(t *testing.T) {
	acceptedAt := time.Date(2026, time.July, 13, 15, 4, 5, 0, time.UTC)
	var gotConversationID, gotBody, gotReplyToID, gotRequestID string
	a := &Adapter{
		accountID:  "whatsapp-primary",
		mediaReady: func() bool { return true },
		sendTextRequest: func(conversationID, body, replyToID, requestID string) (string, time.Time, error) {
			gotConversationID = conversationID
			gotBody = body
			gotReplyToID = replyToID
			gotRequestID = requestID
			return "derived-whatsapp-message-id", acceptedAt, nil
		},
	}

	result, err := a.SendText(context.Background(), bridge.TextRequest{
		AccountID: "whatsapp-primary",
		Conversation: bridge.ConversationRef{
			RemoteID: "whatsapp:15551234567@s.whatsapp.net",
		},
		Body:      "hello from v2",
		ReplyTo:   &bridge.MessageRef{RemoteID: "whatsapp:reply-123"},
		RequestID: "outbox-request-123",
	})
	if err != nil {
		t.Fatalf("SendText() error = %v", err)
	}
	if result != (bridge.SendResult{
		RemoteMessageID: "derived-whatsapp-message-id",
		AcceptedAt:      acceptedAt,
		EchoExpected:    false,
	}) {
		t.Fatalf("SendText() result = %+v, want derived remote ID and transport timestamp", result)
	}
	if gotConversationID != "whatsapp:15551234567@s.whatsapp.net" ||
		gotBody != "hello from v2" ||
		gotReplyToID != "whatsapp:reply-123" ||
		gotRequestID != "outbox-request-123" {
		t.Fatalf("SendTextRequest args = conversation=%q body=%q reply=%q request=%q",
			gotConversationID, gotBody, gotReplyToID, gotRequestID)
	}
}

func TestSendTextTransportErrorClassification(t *testing.T) {
	tests := []struct {
		name            string
		err             error
		wantDispatch    bridge.DispatchCertainty
		wantFingerprint string
	}{
		{
			name:            "connection disappeared before transport call",
			err:             fmt.Errorf("%w: %w", whatsapplive.ErrSendNotDispatched, whatsmeow.ErrNotConnected),
			wantDispatch:    bridge.DispatchNotCalled,
			wantFingerprint: "whatsapp_not_connected",
		},
		{
			name:            "validation failed before dispatch",
			err:             fmt.Errorf("%w: invalid WhatsApp conversation", whatsapplive.ErrSendNotDispatched),
			wantDispatch:    bridge.DispatchNotCalled,
			wantFingerprint: "whatsapp_send_text_failed",
		},
		{
			name:            "send returned not connected is uncertain",
			err:             fmt.Errorf("send WhatsApp message: %w", whatsmeow.ErrNotConnected),
			wantDispatch:    "",
			wantFingerprint: "whatsapp_send_text_failed",
		},
		{
			name:            "ambiguous send error is uncertain",
			err:             errors.New("websocket closed while awaiting response"),
			wantDispatch:    "",
			wantFingerprint: "whatsapp_send_text_failed",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			a := &Adapter{
				mediaReady: func() bool { return true },
				sendTextRequest: func(string, string, string, string) (string, time.Time, error) {
					return "", time.Time{}, test.err
				},
			}

			_, err := a.SendText(context.Background(), bridge.TextRequest{})
			failure, ok := asOpError(err)
			if !ok || failure.Class != bridge.FailureTransient ||
				failure.Dispatch != test.wantDispatch ||
				failure.Operation != "send_text" ||
				failure.Fingerprint != test.wantFingerprint ||
				!errors.Is(failure.Cause, test.err) {
				t.Fatalf("SendText() error = %v, want dispatch=%q fingerprint=%q",
					err, test.wantDispatch, test.wantFingerprint)
			}
		})
	}
}

// The adapter shim must never forward a text-send failure into the lifecycle.
// Reconnect-worthy transport errors are already reported from the retained
// core through its guarded reportConnectionError -> OnConnectionError chain.
func TestSendTextFailureDoesNotRetireReceiveGeneration(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
	}{
		{"ambiguous send failure", errors.New("ambiguous send failure")},
		{"not connected", fmt.Errorf("send WhatsApp message: %w", whatsmeow.ErrNotConnected)},
	} {
		t.Run(test.name, func(t *testing.T) {
			ready := make(chan struct{})
			close(ready)
			r := &run{
				ready:     ready,
				finish:    make(chan error, 1),
				admitting: true,
			}
			a := &Adapter{
				current:    r,
				mediaReady: func() bool { return true },
				sendTextRequest: func(string, string, string, string) (string, time.Time, error) {
					return "", time.Time{}, test.err
				},
			}

			_, err := a.SendText(context.Background(), bridge.TextRequest{})
			returnedFailure, ok := asOpError(err)
			if !ok || !errors.Is(returnedFailure.Cause, test.err) {
				t.Fatalf("SendText() error = %v, want classified transport failure", err)
			}
			select {
			case reported := <-r.finish:
				t.Fatalf("SendText() retired the receive generation with %v", reported)
			default:
			}
		})
	}
}

func TestSendMediaRequiresReadyTransportWithoutLifecycleAction(t *testing.T) {
	var connectCalls, sendCalls int
	a := &Adapter{
		accountID: "whatsapp-primary",
		connect: func(context.Context) error {
			connectCalls++
			return nil
		},
		mediaReady: func() bool { return false },
		sendMediaRequest: func(string, []byte, string, string, string, string, string) (string, time.Time, error) {
			sendCalls++
			return "", time.Time{}, nil
		},
	}

	result, err := a.SendMedia(context.Background(), bridge.MediaRequest{
		Reader: strings.NewReader("png"),
		Size:   3,
	})
	if result != (bridge.SendResult{}) {
		t.Fatalf("SendMedia() result = %+v, want zero result", result)
	}
	failure, ok := asOpError(err)
	if !ok {
		t.Fatalf("SendMedia() error = %T %v, want bridge.OpError", err, err)
	}
	if failure.Class != bridge.FailureTransient ||
		failure.Dispatch != bridge.DispatchNotCalled ||
		failure.Operation != "send_media" ||
		failure.Fingerprint != "whatsapp_not_connected" ||
		!errors.Is(failure.Cause, whatsmeow.ErrNotConnected) {
		t.Fatalf("SendMedia() failure = %+v, want classified not-connected pre-call failure", failure)
	}
	if connectCalls != 0 {
		t.Fatalf("connect calls = %d, want zero", connectCalls)
	}
	if sendCalls != 0 {
		t.Fatalf("transport send calls = %d, want zero", sendCalls)
	}
}

func TestSendMediaBoundedSlurpAndSuccessMapping(t *testing.T) {
	acceptedAt := time.Date(2026, time.July, 13, 15, 4, 5, 0, time.UTC)
	var gotConversationID, gotFilename, gotMIME, gotCaption, gotReplyToID, gotRequestID string
	var gotData []byte
	a := &Adapter{
		accountID:  "whatsapp-primary",
		mediaReady: func() bool { return true },
		sendMediaRequest: func(conversationID string, data []byte, filename, mime, caption, replyToID, requestID string) (string, time.Time, error) {
			gotConversationID = conversationID
			gotData = append([]byte(nil), data...)
			gotFilename = filename
			gotMIME = mime
			gotCaption = caption
			gotReplyToID = replyToID
			gotRequestID = requestID
			return "raw-whatsapp-message-id", acceptedAt, nil
		},
	}

	result, err := a.SendMedia(context.Background(), bridge.MediaRequest{
		AccountID: "whatsapp-primary",
		Conversation: bridge.ConversationRef{
			RemoteID: "whatsapp:15551234567@s.whatsapp.net",
		},
		Reader:    strings.NewReader("png-bytes"),
		Size:      9,
		Filename:  "photo.png",
		MIME:      "image/png",
		Caption:   "look at this",
		ReplyTo:   &bridge.MessageRef{RemoteID: "whatsapp:reply-123"},
		RequestID: "outbox-request-123",
	})
	if err != nil {
		t.Fatalf("SendMedia() error = %v", err)
	}
	if result != (bridge.SendResult{
		RemoteMessageID: "raw-whatsapp-message-id",
		AcceptedAt:      acceptedAt,
		EchoExpected:    false,
	}) {
		t.Fatalf("SendMedia() result = %+v, want raw remote ID and transport timestamp", result)
	}
	if gotConversationID != "whatsapp:15551234567@s.whatsapp.net" ||
		string(gotData) != "png-bytes" ||
		gotFilename != "photo.png" ||
		gotMIME != "image/png" ||
		gotCaption != "look at this" ||
		gotReplyToID != "whatsapp:reply-123" ||
		gotRequestID != "outbox-request-123" {
		t.Fatalf("SendMediaRequest args = conversation=%q data=%q filename=%q mime=%q caption=%q reply=%q request=%q",
			gotConversationID, gotData, gotFilename, gotMIME, gotCaption, gotReplyToID, gotRequestID)
	}
}

func TestSendMediaRejectsReaderLengthMismatchBeforeTransport(t *testing.T) {
	tests := []struct {
		name string
		data string
		size int64
	}{
		{name: "reader longer than declared size", data: "four", size: 3},
		{name: "reader shorter than declared size", data: "to", size: 3},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var sendCalls int
			a := &Adapter{
				mediaReady: func() bool { return true },
				sendMediaRequest: func(string, []byte, string, string, string, string, string) (string, time.Time, error) {
					sendCalls++
					return "unexpected", time.Time{}, nil
				},
			}

			_, err := a.SendMedia(context.Background(), bridge.MediaRequest{
				Reader: strings.NewReader(test.data),
				Size:   test.size,
			})
			failure, ok := asOpError(err)
			if !ok || failure.Class != bridge.FailureTransient ||
				failure.Dispatch != bridge.DispatchNotCalled ||
				failure.Operation != "send_media" ||
				failure.Fingerprint != "whatsapp_media_size_mismatch" {
				t.Fatalf("SendMedia() error = %v, want size-mismatch pre-call failure", err)
			}
			if sendCalls != 0 {
				t.Fatalf("transport send calls = %d, want zero", sendCalls)
			}
		})
	}
}

func TestSendMediaClassifiesReaderErrorAsNotDispatched(t *testing.T) {
	want := errors.New("blob read failed")
	a := &Adapter{
		mediaReady: func() bool { return true },
		sendMediaRequest: func(string, []byte, string, string, string, string, string) (string, time.Time, error) {
			t.Fatal("transport called after reader failure")
			return "", time.Time{}, nil
		},
	}

	_, err := a.SendMedia(context.Background(), bridge.MediaRequest{
		Reader: errorReader{err: want},
		Size:   10,
	})
	failure, ok := asOpError(err)
	if !ok || failure.Class != bridge.FailureTransient ||
		failure.Dispatch != bridge.DispatchNotCalled ||
		failure.Fingerprint != "whatsapp_media_read_failed" ||
		!errors.Is(failure.Cause, want) {
		t.Fatalf("SendMedia() error = %v, want reader pre-call failure", err)
	}
}

func TestSendMediaTransportErrorClassification(t *testing.T) {
	tests := []struct {
		name            string
		err             error
		wantClass       bridge.FailureClass
		wantDispatch    bridge.DispatchCertainty
		wantFingerprint string
	}{
		{
			name:            "connection disappeared before transport call",
			err:             fmt.Errorf("%w: %w", whatsapplive.ErrSendNotDispatched, whatsmeow.ErrNotConnected),
			wantClass:       bridge.FailureTransient,
			wantDispatch:    bridge.DispatchNotCalled,
			wantFingerprint: "whatsapp_not_connected",
		},
		{
			name:            "send returned not connected is uncertain",
			err:             fmt.Errorf("send WhatsApp media: %w", whatsmeow.ErrNotConnected),
			wantClass:       bridge.FailureTransient,
			wantDispatch:    "",
			wantFingerprint: "whatsapp_send_media_failed",
		},
		{
			name:            "upload failed before dispatch",
			err:             fmt.Errorf("%w: upload WhatsApp media: storage unavailable", whatsapplive.ErrSendNotDispatched),
			wantClass:       bridge.FailureTransient,
			wantDispatch:    bridge.DispatchNotCalled,
			wantFingerprint: "whatsapp_send_media_failed",
		},
		{
			name:            "send timeout is uncertain",
			err:             context.DeadlineExceeded,
			wantClass:       bridge.FailureTransient,
			wantDispatch:    "",
			wantFingerprint: "whatsapp_send_media_failed",
		},
		{
			name:            "ambiguous send error is uncertain",
			err:             errors.New("websocket closed while awaiting response"),
			wantClass:       bridge.FailureTransient,
			wantDispatch:    "",
			wantFingerprint: "whatsapp_send_media_failed",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			a := &Adapter{
				mediaReady: func() bool { return true },
				sendMediaRequest: func(string, []byte, string, string, string, string, string) (string, time.Time, error) {
					return "", time.Time{}, test.err
				},
			}

			_, err := a.SendMedia(context.Background(), bridge.MediaRequest{
				Reader: strings.NewReader("png"),
				Size:   3,
			})
			failure, ok := asOpError(err)
			if !ok || failure.Class != test.wantClass ||
				failure.Dispatch != test.wantDispatch ||
				failure.Operation != "send_media" ||
				failure.Fingerprint != test.wantFingerprint ||
				!errors.Is(failure.Cause, test.err) {
				t.Fatalf("SendMedia() error = %v, want class=%q dispatch=%q fingerprint=%q",
					err, test.wantClass, test.wantDispatch, test.wantFingerprint)
			}
		})
	}
}

// A media send failure must never retire the receive generation from the shim
// (the C4/C5/C6 lesson): a malformed conversation, a media-server 4xx, or an
// ambiguous transport error retrying every ~5s would otherwise bounce a healthy
// WhatsApp connection indefinitely — the throttle/ban vector the supervisor
// exists to prevent. Reconnect-worthy failures reach the adapter only through
// the transport core's guarded reportConnectionError -> OnConnectionError chain.
func TestSendMediaFailureDoesNotRetireReceiveGeneration(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
	}{
		{"ambiguous send failure", errors.New("ambiguous send failure")},
		{"not connected", fmt.Errorf("send WhatsApp media: %w", whatsmeow.ErrNotConnected)},
	} {
		t.Run(test.name, func(t *testing.T) {
			ready := make(chan struct{})
			close(ready)
			r := &run{
				ready:     ready,
				finish:    make(chan error, 1),
				admitting: true,
			}
			a := &Adapter{
				current:    r,
				mediaReady: func() bool { return true },
				sendMediaRequest: func(string, []byte, string, string, string, string, string) (string, time.Time, error) {
					return "", time.Time{}, test.err
				},
			}

			_, err := a.SendMedia(context.Background(), bridge.MediaRequest{
				Reader: strings.NewReader("png"),
				Size:   3,
			})
			returnedFailure, ok := asOpError(err)
			if !ok || !errors.Is(returnedFailure.Cause, test.err) {
				t.Fatalf("SendMedia() error = %v, want classified transport failure", err)
			}
			select {
			case reported := <-r.finish:
				t.Fatalf("SendMedia() retired the receive generation with %v", reported)
			default:
			}
		})
	}
}

type errorReader struct {
	err error
}

func (r errorReader) Read([]byte) (int, error) {
	return 0, r.err
}
