package whatsapplive

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	waE2E "go.mau.fi/whatsmeow/proto/waE2E"
	wastore "go.mau.fi/whatsmeow/store"
	watypes "go.mau.fi/whatsmeow/types"
)

func TestDeriveWebMessageIDPreservesMediaDerivationBytes(t *testing.T) {
	const requestID = "outbox-request-123"
	const want = "3EB05F7E578787F3196B49"

	if got := deriveWebMessageID(requestID); !bytes.Equal([]byte(got), []byte(want)) {
		t.Fatalf("deriveWebMessageID(%q) bytes = %x, want pre-rename bytes %x", requestID, []byte(got), []byte(want))
	}
	if got := deriveWebMessageID(requestID); got != want {
		t.Fatalf("second deriveWebMessageID(%q) = %q, want deterministic %q", requestID, got, want)
	}
	if got := deriveWebMessageID(requestID + "-different"); got == want {
		t.Fatalf("different request ID derived %q, want a distinct transport ID", got)
	}
	assertWhatsAppWebMessageID(t, want)
}

func TestErrMediaNotDispatchedAliasesErrSendNotDispatched(t *testing.T) {
	if ErrMediaNotDispatched != ErrSendNotDispatched {
		t.Fatal("ErrMediaNotDispatched must preserve identity with ErrSendNotDispatched")
	}
	want := errors.New("pre-send failure")
	err := markSendNotDispatched(want, true)
	if !errors.Is(err, ErrSendNotDispatched) || !errors.Is(err, ErrMediaNotDispatched) || !errors.Is(err, want) {
		t.Fatalf("marked error = %v, want send marker, media compatibility alias, and cause", err)
	}
}

func TestSendMediaRequestUsesDerivedIDAndReturnsTransportResult(t *testing.T) {
	bridge := connectedMediaTestBridge()

	originalSend := sendTextMessage
	originalUpload := uploadMedia
	originalIsConnected := clientIsConnected
	defer func() {
		sendTextMessage = originalSend
		uploadMedia = originalUpload
		clientIsConnected = originalIsConnected
	}()
	clientIsConnected = func(*whatsmeow.Client) bool { return true }

	uploadMedia = func(_ *whatsmeow.Client, _ context.Context, plaintext []byte, mediaType whatsmeow.MediaType) (whatsmeow.UploadResponse, error) {
		if !bytes.Equal(plaintext, []byte("png-bytes")) {
			t.Fatalf("upload bytes = %q, want png-bytes", plaintext)
		}
		if mediaType != whatsmeow.MediaImage {
			t.Fatalf("upload media type = %v, want image", mediaType)
		}
		return whatsmeow.UploadResponse{
			URL:           "https://cdn.example.test/image",
			DirectPath:    "/mms/image",
			MediaKey:      []byte{0x01, 0x02},
			FileEncSHA256: []byte{0x03, 0x04},
			FileSHA256:    []byte{0x05, 0x06},
			FileLength:    9,
		}, nil
	}

	const wantRequestID = "3EB05F7E578787F3196B49"
	wantTimestamp := time.UnixMilli(1700000000456)
	var gotTo watypes.JID
	var gotMessage *waE2E.Message
	var gotRequestID string
	sendTextMessage = func(_ *whatsmeow.Client, _ context.Context, to watypes.JID, message *waE2E.Message, extra ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error) {
		gotTo = to
		gotMessage = message
		if len(extra) != 1 {
			t.Fatalf("SendMessage extras = %d, want exactly one", len(extra))
		}
		gotRequestID = string(extra[0].ID)
		return whatsmeow.SendResponse{
			ID:        watypes.MessageID("server-remote-id"),
			Timestamp: wantTimestamp,
		}, nil
	}

	remoteID, acceptedAt, err := bridge.SendMediaRequest(
		"whatsapp:15551234567@s.whatsapp.net",
		[]byte("png-bytes"),
		"photo.png",
		"image/png",
		" check this out ",
		"whatsapp:reply-123",
		"outbox-request-123",
	)
	if err != nil {
		t.Fatalf("SendMediaRequest() error = %v", err)
	}
	if remoteID != "server-remote-id" {
		t.Fatalf("remote ID = %q, want raw server ID", remoteID)
	}
	if !acceptedAt.Equal(wantTimestamp) {
		t.Fatalf("accepted at = %s, want %s", acceptedAt, wantTimestamp)
	}
	if gotRequestID != wantRequestID {
		t.Fatalf("transport request ID = %q, want derived %q", gotRequestID, wantRequestID)
	}
	if gotTo.String() != "15551234567@s.whatsapp.net" {
		t.Fatalf("recipient = %q, want target JID", gotTo.String())
	}
	image := gotMessage.GetImageMessage()
	if image == nil {
		t.Fatal("sent message has no image payload")
	}
	if image.GetCaption() != "check this out" {
		t.Fatalf("caption = %q, want trimmed caption", image.GetCaption())
	}
	if image.GetContextInfo().GetStanzaID() != "reply-123" {
		t.Fatalf("reply stanza = %q, want reply-123", image.GetContextInfo().GetStanzaID())
	}
}

func TestSendMediaRequestFallsBackToDerivedIDWhenResponseOmitsID(t *testing.T) {
	bridge := connectedMediaTestBridge()

	originalSend := sendTextMessage
	originalUpload := uploadMedia
	originalIsConnected := clientIsConnected
	defer func() {
		sendTextMessage = originalSend
		uploadMedia = originalUpload
		clientIsConnected = originalIsConnected
	}()
	clientIsConnected = func(*whatsmeow.Client) bool { return true }
	uploadMedia = func(*whatsmeow.Client, context.Context, []byte, whatsmeow.MediaType) (whatsmeow.UploadResponse, error) {
		return whatsmeow.UploadResponse{}, nil
	}
	sendTextMessage = func(*whatsmeow.Client, context.Context, watypes.JID, *waE2E.Message, ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error) {
		return whatsmeow.SendResponse{Timestamp: time.UnixMilli(1700000000456)}, nil
	}

	remoteID, _, err := bridge.SendMediaRequest(
		"whatsapp:15551234567@s.whatsapp.net",
		[]byte("png"),
		"photo.png",
		"image/png",
		"",
		"",
		"outbox-request-123",
	)
	if err != nil {
		t.Fatalf("SendMediaRequest() error = %v", err)
	}
	if remoteID != "3EB05F7E578787F3196B49" {
		t.Fatalf("remote ID = %q, want derived request ID fallback", remoteID)
	}
}

func TestSendMediaRequestMarksOnlyPreDispatchFailures(t *testing.T) {
	t.Run("upload failure", func(t *testing.T) {
		bridge := connectedMediaTestBridge()
		originalUpload := uploadMedia
		originalIsConnected := clientIsConnected
		defer func() {
			uploadMedia = originalUpload
			clientIsConnected = originalIsConnected
		}()
		clientIsConnected = func(*whatsmeow.Client) bool { return true }
		want := errors.New("upload unavailable")
		uploadMedia = func(*whatsmeow.Client, context.Context, []byte, whatsmeow.MediaType) (whatsmeow.UploadResponse, error) {
			return whatsmeow.UploadResponse{}, want
		}

		_, _, err := bridge.SendMediaRequest(
			"whatsapp:15551234567@s.whatsapp.net",
			[]byte("png"),
			"photo.png",
			"image/png",
			"",
			"",
			"request-1",
		)
		if !errors.Is(err, want) || !errors.Is(err, ErrSendNotDispatched) {
			t.Fatalf("SendMediaRequest() error = %v, want upload cause and not-dispatched marker", err)
		}
		if err.Error() != "upload WhatsApp media: upload unavailable" {
			t.Fatalf("error text = %q, want legacy-compatible upload error", err.Error())
		}
	})

	t.Run("send failure", func(t *testing.T) {
		bridge := connectedMediaTestBridge()
		originalSend := sendTextMessage
		originalUpload := uploadMedia
		originalIsConnected := clientIsConnected
		defer func() {
			sendTextMessage = originalSend
			uploadMedia = originalUpload
			clientIsConnected = originalIsConnected
		}()
		clientIsConnected = func(*whatsmeow.Client) bool { return true }
		uploadMedia = func(*whatsmeow.Client, context.Context, []byte, whatsmeow.MediaType) (whatsmeow.UploadResponse, error) {
			return whatsmeow.UploadResponse{}, nil
		}
		want := errors.New("send acknowledgement lost")
		sendTextMessage = func(*whatsmeow.Client, context.Context, watypes.JID, *waE2E.Message, ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error) {
			return whatsmeow.SendResponse{}, want
		}

		_, _, err := bridge.SendMediaRequest(
			"whatsapp:15551234567@s.whatsapp.net",
			[]byte("png"),
			"photo.png",
			"image/png",
			"",
			"",
			"request-1",
		)
		if !errors.Is(err, want) {
			t.Fatalf("SendMediaRequest() error = %v, want send cause", err)
		}
		if errors.Is(err, ErrSendNotDispatched) {
			t.Fatalf("SendMediaRequest() error = %v, ambiguous send must not claim no dispatch", err)
		}
		if err.Error() != "send WhatsApp media: send acknowledgement lost" {
			t.Fatalf("error text = %q, want legacy-compatible send error", err.Error())
		}
	})
}

func assertWhatsAppWebMessageID(t *testing.T, id string) {
	t.Helper()
	if !strings.HasPrefix(id, "3EB0") || len(id) != 22 {
		t.Fatalf("message ID = %q, want 3EB0 plus 18 uppercase hex digits", id)
	}
	hexPart := strings.TrimPrefix(id, "3EB0")
	if strings.ToUpper(hexPart) != hexPart {
		t.Fatalf("message ID hex = %q, want uppercase", hexPart)
	}
	if _, err := hex.DecodeString(hexPart); err != nil {
		t.Fatalf("message ID hex = %q: %v", hexPart, err)
	}
}

func connectedMediaTestBridge() *Bridge {
	ownJID := watypes.NewJID("15551230000", watypes.DefaultUserServer)
	return &Bridge{
		connected: true,
		client: &whatsmeow.Client{
			Store: &wastore.Device{
				ID:       &ownJID,
				PushName: "OpenMessage",
			},
		},
	}
}
