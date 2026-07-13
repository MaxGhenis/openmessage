package whatsapplive

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	waE2E "go.mau.fi/whatsmeow/proto/waE2E"
	watypes "go.mau.fi/whatsmeow/types"

	"github.com/maxghenis/openmessage/internal/db"
)

func TestLegacySendMediaTransportContractIsPreserved(t *testing.T) {
	bridge := connectedMediaTestBridge()

	originalSend := sendTextMessage
	originalUpload := uploadMedia
	originalConnect := connectClient
	originalIsConnected := clientIsConnected
	defer func() {
		sendTextMessage = originalSend
		uploadMedia = originalUpload
		connectClient = originalConnect
		clientIsConnected = originalIsConnected
	}()
	clientIsConnected = func(*whatsmeow.Client) bool { return true }
	var connectCalls int
	connectClient = func(*whatsmeow.Client) error {
		connectCalls++
		return nil
	}

	uploadResult := whatsmeow.UploadResponse{
		URL:           "https://cdn.example.test/image",
		DirectPath:    "/mms/image",
		MediaKey:      []byte{0x01, 0x02},
		FileEncSHA256: []byte{0x03, 0x04},
		FileSHA256:    []byte{0x05, 0x06},
		FileLength:    9,
	}
	var uploadCalls int
	var uploadContext context.Context
	uploadMedia = func(_ *whatsmeow.Client, ctx context.Context, plaintext []byte, mediaType whatsmeow.MediaType) (whatsmeow.UploadResponse, error) {
		uploadCalls++
		uploadContext = ctx
		if !bytes.Equal(plaintext, []byte("png-bytes")) {
			t.Fatalf("upload plaintext = %q, want png-bytes", plaintext)
		}
		if mediaType != whatsmeow.MediaImage {
			t.Fatalf("upload media type = %v, want image", mediaType)
		}
		return uploadResult, nil
	}

	acceptedAt := time.UnixMilli(1700000000456)
	var sendCalls int
	var sendContext context.Context
	var sentTo watypes.JID
	var sentMessage *waE2E.Message
	var sentRequestID string
	sendTextMessage = func(_ *whatsmeow.Client, ctx context.Context, to watypes.JID, message *waE2E.Message, extra ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error) {
		sendCalls++
		sendContext = ctx
		sentTo = to
		sentMessage = message
		if len(extra) != 1 {
			t.Fatalf("SendMessage extras = %d, want exactly one request options value", len(extra))
		}
		sentRequestID = string(extra[0].ID)
		return whatsmeow.SendResponse{
			ID:        watypes.MessageID("server-legacy-id"),
			Timestamp: acceptedAt,
		}, nil
	}

	message, err := bridge.SendMedia(
		"whatsapp:15551234567@s.whatsapp.net",
		[]byte("png-bytes"),
		"photo.png",
		"image/png",
		" check this out ",
		"whatsapp:reply-123",
	)
	if err != nil {
		t.Fatalf("SendMedia() error = %v", err)
	}

	if connectCalls != 0 {
		t.Fatalf("connect calls = %d, want zero", connectCalls)
	}
	if uploadCalls != 1 || sendCalls != 1 {
		t.Fatalf("transport calls = upload %d, send %d; want one each", uploadCalls, sendCalls)
	}
	if uploadContext == nil || sendContext == nil || uploadContext == sendContext {
		t.Fatalf("transport contexts = upload %v, send %v; want separate bounded contexts", uploadContext, sendContext)
	}
	assertContextDeadlineNear(t, uploadContext, 90*time.Second)
	assertContextDeadlineNear(t, sendContext, 45*time.Second)
	if sentTo.String() != "15551234567@s.whatsapp.net" {
		t.Fatalf("sent to = %q, want target JID", sentTo.String())
	}
	assertWhatsAppWebMessageID(t, sentRequestID)
	image := sentMessage.GetImageMessage()
	if image == nil {
		t.Fatal("legacy SendMedia sent no image payload")
	}
	if image.GetCaption() != "check this out" ||
		image.GetMimetype() != "image/png" ||
		image.GetDirectPath() != "/mms/image" ||
		image.GetContextInfo().GetStanzaID() != "reply-123" {
		t.Fatalf("legacy outgoing image = %+v, transport arguments changed", image)
	}

	wantMessage := &db.Message{
		MessageID:      "whatsapp:server-legacy-id",
		ConversationID: "whatsapp:15551234567@s.whatsapp.net",
		SenderName:     "OpenMessage",
		SenderNumber:   "+15551230000",
		Body:           "check this out",
		TimestampMS:    1700000000456,
		Status:         "sent",
		IsFromMe:       true,
		MediaID:        "wa:eyJ1cmwiOiJodHRwczovL2Nkbi5leGFtcGxlLnRlc3QvaW1hZ2UiLCJkaXJlY3RfcGF0aCI6Ii9tbXMvaW1hZ2UiLCJmaWxlX3NoYTI1NiI6IjA1MDYiLCJmaWxlX2VuY19zaGEyNTYiOiIwMzA0IiwiZmlsZV9sZW5ndGgiOjl9",
		MimeType:       "image/png",
		DecryptionKey:  "0102",
		ReplyToID:      "whatsapp:reply-123",
		SourcePlatform: "whatsapp",
		SourceID:       "server-legacy-id",
	}
	if !reflect.DeepEqual(message, wantMessage) {
		t.Fatalf("legacy SendMedia message =\n%#v\nwant unchanged value =\n%#v", message, wantMessage)
	}
}

func TestLegacySendMediaUploadErrorContractIsPreserved(t *testing.T) {
	bridge := connectedMediaTestBridge()

	originalUpload := uploadMedia
	originalIsConnected := clientIsConnected
	defer func() {
		uploadMedia = originalUpload
		clientIsConnected = originalIsConnected
	}()
	clientIsConnected = func(*whatsmeow.Client) bool { return true }
	want := errors.New("legacy upload failure")
	uploadMedia = func(*whatsmeow.Client, context.Context, []byte, whatsmeow.MediaType) (whatsmeow.UploadResponse, error) {
		return whatsmeow.UploadResponse{}, want
	}

	_, err := bridge.SendMedia(
		"whatsapp:15551234567@s.whatsapp.net",
		[]byte("png-bytes"),
		"photo.png",
		"image/png",
		"",
		"",
	)
	if !errors.Is(err, want) {
		t.Fatalf("SendMedia() error = %v, want legacy upload cause", err)
	}
	if err.Error() != "upload WhatsApp media: legacy upload failure" {
		t.Fatalf("SendMedia() error text = %q, want unchanged legacy text", err.Error())
	}
	if errors.Is(err, ErrMediaNotDispatched) {
		t.Fatalf("legacy SendMedia() error gained the v2-only marker: %v", err)
	}
}

func assertContextDeadlineNear(t *testing.T, ctx context.Context, timeout time.Duration) {
	t.Helper()
	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatalf("context has no deadline, want %s timeout", timeout)
	}
	remaining := time.Until(deadline)
	if remaining <= timeout-time.Second || remaining > timeout {
		t.Fatalf("context deadline remaining = %s, want near %s", remaining, timeout)
	}
}
