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
			err:             fmt.Errorf("%w: %w", whatsapplive.ErrMediaNotDispatched, whatsmeow.ErrNotConnected),
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
			err:             fmt.Errorf("%w: upload WhatsApp media: storage unavailable", whatsapplive.ErrMediaNotDispatched),
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
