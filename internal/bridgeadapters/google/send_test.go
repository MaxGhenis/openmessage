package google

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/client"
)

func TestSendMediaNotConnectedIsClassifiedPreCall(t *testing.T) {
	host := newTestApp(t)
	a := New("google-primary", host, func() bool { return true })
	a.newClient = func() (*client.Client, transportClient, error) {
		t.Fatal("SendMedia must not construct a Google transport")
		return nil, nil, nil
	}

	result, err := a.SendMedia(context.Background(), bridge.MediaRequest{
		AccountID: "google-primary",
		Reader:    panicReader{},
		Size:      1,
	})
	if result != (bridge.SendResult{}) {
		t.Fatalf("result = %+v, want zero value", result)
	}
	failure := requireMediaOpError(t, err)
	if failure.Class != bridge.FailureTransient ||
		failure.Dispatch != bridge.DispatchNotCalled ||
		failure.Operation != "send_media" ||
		failure.Fingerprint != "google_not_connected" {
		t.Fatalf("failure = %+v, want transient pre-call google_not_connected", failure)
	}
}

func TestSendMediaInstalledClientBeforeReadyIsClassifiedPreCall(t *testing.T) {
	host := newTestApp(t)
	legacy := newLegacyClient(t)
	generation := host.BeginGoogleGeneration(legacy)
	t.Cleanup(generation.Release)
	fake := &fakeMediaSendClient{}
	installMediaSendClient(t, legacy, fake)
	a := New("google-primary", host, func() bool { return true })

	_, err := a.SendMedia(context.Background(), bridge.MediaRequest{
		Reader: strings.NewReader("x"),
		Size:   1,
	})
	failure := requireMediaOpError(t, err)
	if failure.Class != bridge.FailureTransient ||
		failure.Dispatch != bridge.DispatchNotCalled ||
		failure.Operation != "send_media" ||
		failure.Fingerprint != "google_not_connected" {
		t.Fatalf("failure = %+v, want transient pre-call google_not_connected", failure)
	}
	if fake.uploadCalls != 0 {
		t.Fatalf("UploadMedia calls = %d, want 0 before lifecycle readiness", fake.uploadCalls)
	}
}

func TestSendMediaSuccessUsesStablePayloadAndMapsResult(t *testing.T) {
	media := &gmproto.MediaContent{
		Format:    gmproto.MediaFormats_IMAGE_PNG,
		MediaID:   "uploaded-media-id",
		MediaName: "photo.png",
		MimeType:  "image/png",
		Size:      4,
	}
	sim := &gmproto.SIMPayload{Two: 7, SIMNumber: 2}
	conversation := &gmproto.Conversation{
		ConversationID: "remote-conversation",
		Participants: []*gmproto.Participant{{
			ID:         &gmproto.SmallInfo{Number: "+15551234567"},
			IsMe:       true,
			SimPayload: sim,
		}},
	}
	fake := &fakeMediaSendClient{
		uploadResult:       media,
		conversationResult: conversation,
		sendResults: []*gmproto.SendMessageResponse{{
			Status: gmproto.SendMessageResponse_SUCCESS,
		}},
	}
	a := newMediaSendTestAdapter(t, fake)

	result, err := a.SendMedia(context.Background(), bridge.MediaRequest{
		AccountID: "google-primary",
		Conversation: bridge.ConversationRef{
			RemoteID: "remote-conversation",
		},
		Reader:    bytes.NewBufferString("data"),
		Size:      4,
		Filename:  "photo.png",
		MIME:      "image/png",
		RequestID: "transport-request-id",
	})
	if err != nil {
		t.Fatalf("SendMedia() error = %v", err)
	}
	if result.RemoteMessageID != "transport-request-id" || !result.EchoExpected || !result.AcceptedAt.IsZero() {
		t.Fatalf("result = %+v, want stable TmpID, echo expected, and zero AcceptedAt", result)
	}
	if got := string(fake.uploadData); got != "data" {
		t.Fatalf("UploadMedia data = %q, want data", got)
	}
	if fake.uploadFilename != "photo.png" || fake.uploadMIME != "image/png" {
		t.Fatalf("UploadMedia metadata = (%q, %q), want (photo.png, image/png)", fake.uploadFilename, fake.uploadMIME)
	}
	if fake.conversationID != "remote-conversation" {
		t.Fatalf("GetConversation ID = %q, want remote-conversation", fake.conversationID)
	}
	if len(fake.sent) != 1 {
		t.Fatalf("SendMessage calls = %d, want 1", len(fake.sent))
	}
	payload := fake.sent[0]
	if payload.GetTmpID() != "transport-request-id" ||
		payload.GetMessagePayload().GetTmpID() != "transport-request-id" ||
		payload.GetMessagePayload().GetTmpID2() != "transport-request-id" {
		t.Fatalf("payload TmpIDs = (%q, %q, %q), want transport-request-id in all positions",
			payload.GetTmpID(), payload.GetMessagePayload().GetTmpID(), payload.GetMessagePayload().GetTmpID2())
	}
	if payload.GetMessagePayload().GetParticipantID() != "+15551234567" || payload.GetSIMPayload() != sim {
		t.Fatalf("payload routing = participant %q SIM %p, want +15551234567 and %p",
			payload.GetMessagePayload().GetParticipantID(), payload.GetSIMPayload(), sim)
	}
	infos := payload.GetMessagePayload().GetMessageInfo()
	if len(infos) != 1 || infos[0].GetMediaContent() != media {
		t.Fatalf("media payload = %+v, want uploaded media pointer %p", infos, media)
	}
}

func TestSendMediaCaptionUsesStableFollowUpTextPayload(t *testing.T) {
	sim := &gmproto.SIMPayload{Two: 1, SIMNumber: 1}
	fake := &fakeMediaSendClient{
		uploadResult: &gmproto.MediaContent{MediaID: "media-id"},
		conversationResult: &gmproto.Conversation{Participants: []*gmproto.Participant{{
			ID:         &gmproto.SmallInfo{Number: "+15557654321"},
			IsMe:       true,
			SimPayload: sim,
		}}},
		sendResults: []*gmproto.SendMessageResponse{
			{Status: gmproto.SendMessageResponse_SUCCESS},
			{Status: gmproto.SendMessageResponse_SUCCESS},
		},
	}
	a := newMediaSendTestAdapter(t, fake)

	result, err := a.SendMedia(context.Background(), bridge.MediaRequest{
		Conversation: bridge.ConversationRef{RemoteID: "conversation-id"},
		Reader:       strings.NewReader("x"),
		Size:         1,
		Caption:      "  a useful caption  ",
		ReplyTo:      &bridge.MessageRef{RemoteID: "reply-id"},
		RequestID:    "media-request-id",
	})
	if err != nil {
		t.Fatalf("SendMedia() error = %v", err)
	}
	if result.RemoteMessageID != "media-request-id" {
		t.Fatalf("RemoteMessageID = %q, want media-request-id", result.RemoteMessageID)
	}
	if len(fake.sent) != 2 {
		t.Fatalf("SendMessage calls = %d, want media plus caption", len(fake.sent))
	}
	caption := fake.sent[1]
	if caption.GetTmpID() != "media-request-id:caption" ||
		caption.GetMessagePayload().GetTmpID() != "media-request-id:caption" ||
		caption.GetMessagePayload().GetTmpID2() != "media-request-id:caption" {
		t.Fatalf("caption TmpIDs = (%q, %q, %q), want stable :caption suffix",
			caption.GetTmpID(), caption.GetMessagePayload().GetTmpID(), caption.GetMessagePayload().GetTmpID2())
	}
	infos := caption.GetMessagePayload().GetMessageInfo()
	if len(infos) != 1 || infos[0].GetMessageContent().GetContent() != "a useful caption" {
		t.Fatalf("caption MessageInfo = %+v, want trimmed caption", infos)
	}
	if caption.GetReply().GetMessageID() != "reply-id" {
		t.Fatalf("caption reply ID = %q, want reply-id", caption.GetReply().GetMessageID())
	}
}

func TestSendMediaBoundsAndLengthChecksReaderBeforeUpload(t *testing.T) {
	tests := []struct {
		name        string
		reader      io.Reader
		size        int64
		fingerprint string
	}{
		{
			name:        "reader error",
			reader:      errorReader{err: errors.New("blob read failed")},
			size:        4,
			fingerprint: "google_media_read_failed",
		},
		{
			name:        "short reader",
			reader:      strings.NewReader("abc"),
			size:        4,
			fingerprint: "google_media_size_mismatch",
		},
		{
			name:        "long reader",
			reader:      strings.NewReader("abcde"),
			size:        4,
			fingerprint: "google_media_size_mismatch",
		},
		{
			name:        "nil reader",
			reader:      nil,
			size:        4,
			fingerprint: "google_media_read_failed",
		},
		{
			name:        "negative size",
			reader:      strings.NewReader(""),
			size:        -1,
			fingerprint: "google_media_size_mismatch",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := &fakeMediaSendClient{}
			a := newMediaSendTestAdapter(t, fake)
			_, err := a.SendMedia(context.Background(), bridge.MediaRequest{
				Reader: test.reader,
				Size:   test.size,
			})
			failure := requireMediaOpError(t, err)
			if failure.Class != bridge.FailureTransient || failure.Dispatch != bridge.DispatchNotCalled ||
				failure.Operation != "send_media" || failure.Fingerprint != test.fingerprint {
				t.Fatalf("failure = %+v, want transient pre-call %s", failure, test.fingerprint)
			}
			if fake.uploadCalls != 0 {
				t.Fatalf("UploadMedia calls = %d, want 0", fake.uploadCalls)
			}
		})
	}

	reader := strings.NewReader("abcdef")
	fake := &fakeMediaSendClient{}
	a := newMediaSendTestAdapter(t, fake)
	_, _ = a.SendMedia(context.Background(), bridge.MediaRequest{Reader: reader, Size: 3})
	if reader.Len() != 2 {
		t.Fatalf("reader remaining bytes = %d, want 2 after bounded Size+1 slurp", reader.Len())
	}
}

func TestSendMediaPreDispatchTransportFailuresAreClassifiedNotCalled(t *testing.T) {
	tests := []struct {
		name        string
		configure   func(*fakeMediaSendClient)
		wantClass   bridge.FailureClass
		fingerprint string
	}{
		{
			name: "upload",
			configure: func(fake *fakeMediaSendClient) {
				fake.uploadErr = errors.New("upload unavailable")
			},
			wantClass:   bridge.FailureTransient,
			fingerprint: "google_media_upload_failed",
		},
		{
			name:        "nil upload result",
			configure:   func(*fakeMediaSendClient) {},
			wantClass:   bridge.FailureTransient,
			fingerprint: "google_media_upload_failed",
		},
		{
			name: "nil conversation result",
			configure: func(fake *fakeMediaSendClient) {
				fake.uploadResult = &gmproto.MediaContent{MediaID: "media-id"}
			},
			wantClass:   bridge.FailureTransient,
			fingerprint: "google_conversation_get_failed",
		},
		{
			name: "conversation auth expiry",
			configure: func(fake *fakeMediaSendClient) {
				fake.uploadResult = &gmproto.MediaContent{MediaID: "media-id"}
				fake.conversationErr = errors.New("HTTP 401: invalid authentication credentials")
			},
			wantClass:   bridge.FailureCredentialsExpired,
			fingerprint: "google_auth_expired",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := &fakeMediaSendClient{}
			test.configure(fake)
			a := newMediaSendTestAdapter(t, fake)
			_, err := a.SendMedia(context.Background(), bridge.MediaRequest{
				Conversation: bridge.ConversationRef{RemoteID: "conversation-id"},
				Reader:       strings.NewReader("x"),
				Size:         1,
			})
			failure := requireMediaOpError(t, err)
			if failure.Class != test.wantClass || failure.Dispatch != bridge.DispatchNotCalled ||
				failure.Operation != "send_media" || failure.Fingerprint != test.fingerprint {
				t.Fatalf("failure = %+v, want class %q pre-call %s", failure, test.wantClass, test.fingerprint)
			}
			if len(fake.sent) != 0 {
				t.Fatalf("SendMessage calls = %d, want 0", len(fake.sent))
			}
		})
	}
}

// A transient media send failure must never retire the receive generation (the
// C4/C5/C6 lesson): the outbox retries every few seconds, and reporting each
// attempt would bounce a healthy Google connection indefinitely — the
// over-reconnect throttle vector the runbook warns about.
func TestSendMediaTransientFailuresDoNotRetireGeneration(t *testing.T) {
	tests := []struct {
		name string
		fake *fakeMediaSendClient
	}{
		{
			name: "ambiguous transport error",
			fake: &fakeMediaSendClient{
				uploadResult:       &gmproto.MediaContent{MediaID: "media-id"},
				conversationResult: &gmproto.Conversation{},
				sendErrors:         []error{errors.New("transport timeout")},
			},
		},
		{
			name: "nil send response",
			fake: &fakeMediaSendClient{
				uploadResult:       &gmproto.MediaContent{MediaID: "media-id"},
				conversationResult: &gmproto.Conversation{},
				sendResults:        []*gmproto.SendMessageResponse{nil},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			host := newTestApp(t)
			transport := &fakeTransport{}
			a := newTestAdapter(t, host, transport)
			run, err := a.Start(context.Background(), bridge.StartRequest{
				AccountID:  "google-primary",
				Generation: 1,
			}, nil)
			if err != nil {
				t.Fatalf("Start() error = %v", err)
			}
			transport.emit(&gmproto.Conversation{ConversationID: "ready"})
			<-run.Ready()

			installMediaSendClient(t, host.GetClient(), test.fake)

			_, err = a.SendMedia(context.Background(), bridge.MediaRequest{
				Conversation: bridge.ConversationRef{RemoteID: "conversation-id"},
				Reader:       strings.NewReader("x"),
				Size:         1,
				RequestID:    "request-id",
			})
			failure := requireMediaOpError(t, err)
			if failure.Class != bridge.FailureTransient || failure.Dispatch != "" ||
				failure.Operation != "send_media" || failure.Fingerprint != "google_media_send_failed" {
				t.Fatalf("failure = %+v, want ambiguous transient google_media_send_failed", failure)
			}
			select {
			case terminal := <-run.Done():
				t.Fatalf("transient media send failure retired the receive generation with %v", terminal)
			default:
			}
			if host.GoogleStatus().NeedsRepair {
				t.Fatal("transient media send failure parked the Google lifecycle")
			}
		})
	}
}

// An auth-indicting media send failure must still reach the lifecycle owner so
// the supervisor can route to credential repair — the C4 notify-on-auth-expiry
// contract, preserved from every send path.
func TestSendMediaAuthExpiryStillReportsToLifecycle(t *testing.T) {
	host := newTestApp(t)
	transport := &fakeTransport{}
	a := newTestAdapter(t, host, transport)
	run, err := a.Start(context.Background(), bridge.StartRequest{
		AccountID:  "google-primary",
		Generation: 1,
	}, nil)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	transport.emit(&gmproto.Conversation{ConversationID: "ready"})
	<-run.Ready()

	fake := &fakeMediaSendClient{
		uploadResult:       &gmproto.MediaContent{MediaID: "media-id"},
		conversationResult: &gmproto.Conversation{},
		sendErrors:         []error{errors.New("google returned http 401 for send")},
	}
	installMediaSendClient(t, host.GetClient(), fake)

	_, err = a.SendMedia(context.Background(), bridge.MediaRequest{
		Conversation: bridge.ConversationRef{RemoteID: "conversation-id"},
		Reader:       strings.NewReader("x"),
		Size:         1,
		RequestID:    "request-id",
	})
	failure := requireMediaOpError(t, err)
	if failure.Fingerprint != "google_auth_expired" {
		t.Fatalf("failure = %+v, want google_auth_expired classification", failure)
	}
	select {
	case terminal := <-run.Done():
		terminalFailure, ok := asOpError(terminal)
		if !ok || (terminalFailure.Class != bridge.FailureCredentialsExpired &&
			terminalFailure.Class != bridge.FailureUpgradeRequired) {
			t.Fatalf("terminal = %v, want credentials_expired/upgrade_required", terminal)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("auth-expired media send failure did not reach the lifecycle owner")
	}
}

func TestSendMediaRejectedStatusIsClassifiedNotCalled(t *testing.T) {
	fake := &fakeMediaSendClient{
		uploadResult:       &gmproto.MediaContent{MediaID: "media-id"},
		conversationResult: &gmproto.Conversation{},
		sendResults: []*gmproto.SendMessageResponse{{
			Status: gmproto.SendMessageResponse_UNKNOWN,
		}},
	}
	a := newMediaSendTestAdapter(t, fake)

	_, err := a.SendMedia(context.Background(), bridge.MediaRequest{
		Conversation: bridge.ConversationRef{RemoteID: "conversation-id"},
		Reader:       strings.NewReader("x"),
		Size:         1,
		RequestID:    "request-id",
	})
	failure := requireMediaOpError(t, err)
	if failure.Class != bridge.FailureTransient || failure.Dispatch != bridge.DispatchNotCalled ||
		failure.Operation != "send_media" || failure.Fingerprint != "google_media_send_rejected" {
		t.Fatalf("failure = %+v, want transient pre-call google_media_send_rejected", failure)
	}
}

func TestSendMediaCaptionFailureRemainsAmbiguousAfterMediaSuccess(t *testing.T) {
	tests := []struct {
		name        string
		sendResults []*gmproto.SendMessageResponse
		sendErrors  []error
		fingerprint string
	}{
		{
			name: "send error",
			sendResults: []*gmproto.SendMessageResponse{
				{Status: gmproto.SendMessageResponse_SUCCESS},
				nil,
			},
			sendErrors:  []error{nil, errors.New("caption timeout")},
			fingerprint: "google_caption_send_failed",
		},
		{
			name: "rejected status",
			sendResults: []*gmproto.SendMessageResponse{
				{Status: gmproto.SendMessageResponse_SUCCESS},
				{Status: gmproto.SendMessageResponse_UNKNOWN},
			},
			fingerprint: "google_caption_send_rejected",
		},
		{
			name: "nil response",
			sendResults: []*gmproto.SendMessageResponse{
				{Status: gmproto.SendMessageResponse_SUCCESS},
				nil,
			},
			fingerprint: "google_caption_send_failed",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := &fakeMediaSendClient{
				uploadResult:       &gmproto.MediaContent{MediaID: "media-id"},
				conversationResult: &gmproto.Conversation{},
				sendResults:        test.sendResults,
				sendErrors:         test.sendErrors,
			}
			a := newMediaSendTestAdapter(t, fake)
			_, err := a.SendMedia(context.Background(), bridge.MediaRequest{
				Conversation: bridge.ConversationRef{RemoteID: "conversation-id"},
				Reader:       strings.NewReader("x"),
				Size:         1,
				Caption:      "caption",
				RequestID:    "request-id",
			})
			failure := requireMediaOpError(t, err)
			if failure.Class != bridge.FailureTransient || failure.Dispatch != "" ||
				failure.Operation != "send_media" || failure.Fingerprint != test.fingerprint {
				t.Fatalf("failure = %+v, want ambiguous transient %s", failure, test.fingerprint)
			}
			if len(fake.sent) != 2 {
				t.Fatalf("SendMessage calls = %d, want media then caption", len(fake.sent))
			}
		})
	}
}

func newMediaSendTestAdapter(t *testing.T, fake *fakeMediaSendClient) *Adapter {
	t.Helper()
	host := newTestApp(t)
	legacy := newLegacyClient(t)
	generation := host.BeginGoogleGeneration(legacy)
	t.Cleanup(generation.Release)
	host.Connected.Store(true)
	installMediaSendClient(t, legacy, fake)
	return New("google-primary", host, func() bool { return true })
}

func installMediaSendClient(t *testing.T, want *client.Client, fake mediaSendClient) {
	t.Helper()
	previous := mediaSendClientFor
	mediaSendClientFor = func(got *client.Client) mediaSendClient {
		if got != want {
			t.Fatalf("media client source = %p, want App.GetClient() %p", got, want)
		}
		return fake
	}
	t.Cleanup(func() { mediaSendClientFor = previous })
}

func requireMediaOpError(t *testing.T, err error) bridge.OpError {
	t.Helper()
	if err == nil {
		t.Fatal("SendMedia() error = nil, want classified failure")
	}
	failure, ok := asOpError(err)
	if !ok {
		t.Fatalf("SendMedia() error = %T %v, want bridge.OpError", err, err)
	}
	return failure
}

type fakeMediaSendClient struct {
	uploadResult       *gmproto.MediaContent
	uploadErr          error
	conversationResult *gmproto.Conversation
	conversationErr    error
	sendResults        []*gmproto.SendMessageResponse
	sendErrors         []error

	uploadCalls    int
	uploadData     []byte
	uploadFilename string
	uploadMIME     string
	conversationID string
	sent           []*gmproto.SendMessageRequest
}

func (f *fakeMediaSendClient) UploadMedia(data []byte, filename, mime string) (*gmproto.MediaContent, error) {
	f.uploadCalls++
	f.uploadData = append([]byte(nil), data...)
	f.uploadFilename = filename
	f.uploadMIME = mime
	return f.uploadResult, f.uploadErr
}

func (f *fakeMediaSendClient) GetConversation(conversationID string) (*gmproto.Conversation, error) {
	f.conversationID = conversationID
	return f.conversationResult, f.conversationErr
}

func (f *fakeMediaSendClient) SendMessage(payload *gmproto.SendMessageRequest) (*gmproto.SendMessageResponse, error) {
	f.sent = append(f.sent, payload)
	index := len(f.sent) - 1
	var result *gmproto.SendMessageResponse
	if index < len(f.sendResults) {
		result = f.sendResults[index]
	}
	var err error
	if index < len(f.sendErrors) {
		err = f.sendErrors[index]
	}
	return result, err
}

type panicReader struct{}

func (panicReader) Read([]byte) (int, error) { panic("reader used before readiness check") }

type errorReader struct{ err error }

func (r errorReader) Read([]byte) (int, error) { return 0, r.err }
