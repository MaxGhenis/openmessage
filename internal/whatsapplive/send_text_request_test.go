package whatsapplive

import (
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

func TestSendTextRequestUsesDerivedIDAndReturnsTransportResult(t *testing.T) {
	bridge := connectedMediaTestBridge()

	originalSend := sendTextMessage
	originalIsConnected := clientIsConnected
	defer func() {
		sendTextMessage = originalSend
		clientIsConnected = originalIsConnected
	}()
	clientIsConnected = func(*whatsmeow.Client) bool { return true }

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

	remoteID, acceptedAt, err := bridge.SendTextRequest(
		"whatsapp:15551234567@s.whatsapp.net",
		"  hello from v2  ",
		"whatsapp:reply-123",
		"outbox-request-123",
	)
	if err != nil {
		t.Fatalf("SendTextRequest() error = %v", err)
	}
	if remoteID != "server-remote-id" {
		t.Fatalf("remote ID = %q, want raw server ID", remoteID)
	}
	if !acceptedAt.Equal(wantTimestamp) {
		t.Fatalf("accepted at = %s, want %s", acceptedAt, wantTimestamp)
	}
	if gotRequestID != "3EB05F7E578787F3196B49" {
		t.Fatalf("transport request ID = %q, want derived web message ID", gotRequestID)
	}
	if gotTo.String() != "15551234567@s.whatsapp.net" {
		t.Fatalf("recipient = %q, want target JID", gotTo.String())
	}
	if gotMessage.GetExtendedTextMessage().GetText() != "hello from v2" {
		t.Fatalf("sent text = %q, want trimmed body", gotMessage.GetExtendedTextMessage().GetText())
	}
	if gotMessage.GetExtendedTextMessage().GetContextInfo().GetStanzaID() != "reply-123" {
		t.Fatalf("reply stanza = %q, want reply-123", gotMessage.GetExtendedTextMessage().GetContextInfo().GetStanzaID())
	}
}

func TestSendTextRequestFallsBackToDerivedIDWhenResponseOmitsID(t *testing.T) {
	bridge := connectedMediaTestBridge()

	originalSend := sendTextMessage
	originalIsConnected := clientIsConnected
	defer func() {
		sendTextMessage = originalSend
		clientIsConnected = originalIsConnected
	}()
	clientIsConnected = func(*whatsmeow.Client) bool { return true }
	sendTextMessage = func(*whatsmeow.Client, context.Context, watypes.JID, *waE2E.Message, ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error) {
		return whatsmeow.SendResponse{Timestamp: time.UnixMilli(1700000000456)}, nil
	}

	remoteID, _, err := bridge.SendTextRequest(
		"whatsapp:15551234567@s.whatsapp.net",
		"hello",
		"",
		"outbox-request-123",
	)
	if err != nil {
		t.Fatalf("SendTextRequest() error = %v", err)
	}
	if remoteID != "3EB05F7E578787F3196B49" {
		t.Fatalf("remote ID = %q, want derived request ID fallback", remoteID)
	}
}

func TestSendTextRequestMarksOnlyPreDispatchFailures(t *testing.T) {
	t.Run("invalid conversation", func(t *testing.T) {
		bridge := connectedMediaTestBridge()

		_, _, err := bridge.SendTextRequest("invalid", "hello", "", "request-1")
		if err == nil || !errors.Is(err, ErrSendNotDispatched) {
			t.Fatalf("SendTextRequest() error = %v, want not-dispatched marker", err)
		}
		if err.Error() != "invalid WhatsApp conversation id: invalid" {
			t.Fatalf("error text = %q, want legacy-compatible parse error", err.Error())
		}
	})

	t.Run("send failure", func(t *testing.T) {
		bridge := connectedMediaTestBridge()
		originalSend := sendTextMessage
		originalIsConnected := clientIsConnected
		defer func() {
			sendTextMessage = originalSend
			clientIsConnected = originalIsConnected
		}()
		clientIsConnected = func(*whatsmeow.Client) bool { return true }
		want := errors.New("send acknowledgement lost")
		sendTextMessage = func(*whatsmeow.Client, context.Context, watypes.JID, *waE2E.Message, ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error) {
			return whatsmeow.SendResponse{}, want
		}

		_, _, err := bridge.SendTextRequest(
			"whatsapp:15551234567@s.whatsapp.net",
			"hello",
			"",
			"request-1",
		)
		if !errors.Is(err, want) {
			t.Fatalf("SendTextRequest() error = %v, want send cause", err)
		}
		if errors.Is(err, ErrSendNotDispatched) {
			t.Fatalf("SendTextRequest() error = %v, ambiguous send must not claim no dispatch", err)
		}
		if err.Error() != "send WhatsApp message: send acknowledgement lost" {
			t.Fatalf("error text = %q, want legacy-compatible send error", err.Error())
		}
	})

	t.Run("transport disconnected before send", func(t *testing.T) {
		bridge := &Bridge{}

		_, _, err := bridge.SendTextRequest(
			"whatsapp:15551234567@s.whatsapp.net",
			"hello",
			"",
			"request-1",
		)
		if !errors.Is(err, whatsmeow.ErrNotConnected) || !errors.Is(err, ErrSendNotDispatched) {
			t.Fatalf("SendTextRequest() error = %v, want not-connected cause and not-dispatched marker", err)
		}
	})
}

func TestLegacySendTextContractIsPreserved(t *testing.T) {
	bridge := connectedMediaTestBridge()

	originalSend := sendTextMessage
	originalIsConnected := clientIsConnected
	defer func() {
		sendTextMessage = originalSend
		clientIsConnected = originalIsConnected
	}()
	clientIsConnected = func(*whatsmeow.Client) bool { return true }

	var generatedID string
	sendTextMessage = func(_ *whatsmeow.Client, _ context.Context, _ watypes.JID, message *waE2E.Message, extra ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error) {
		if len(extra) != 1 {
			t.Fatalf("SendMessage extras = %d, want exactly one", len(extra))
		}
		generatedID = string(extra[0].ID)
		if generatedID == "" {
			t.Fatal("legacy SendText passed an empty generated message ID")
		}
		if generatedID == string(deriveWebMessageID("")) {
			t.Fatal("legacy SendText derived the empty request ID instead of calling GenerateMessageID")
		}
		if message.GetExtendedTextMessage().GetText() != "legacy text" {
			t.Fatalf("sent text = %q, want trimmed legacy text", message.GetExtendedTextMessage().GetText())
		}
		return whatsmeow.SendResponse{Timestamp: time.UnixMilli(1700000000123)}, nil
	}

	message, err := bridge.SendText(
		"whatsapp:15551234567@s.whatsapp.net",
		"  legacy text  ",
		"whatsapp:reply-123",
	)
	if err != nil {
		t.Fatalf("SendText() error = %v", err)
	}
	wantMessage := &db.Message{
		MessageID:      "whatsapp:" + generatedID,
		ConversationID: "whatsapp:15551234567@s.whatsapp.net",
		SenderName:     "OpenMessage",
		SenderNumber:   "+15551230000",
		Body:           "legacy text",
		TimestampMS:    1700000000123,
		Status:         "sent",
		IsFromMe:       true,
		ReplyToID:      "whatsapp:reply-123",
		SourcePlatform: "whatsapp",
		SourceID:       generatedID,
	}
	if !reflect.DeepEqual(message, wantMessage) {
		t.Fatalf("legacy SendText message =\n%#v\nwant unchanged value =\n%#v", message, wantMessage)
	}
}

func TestLegacySendTextPreDispatchErrorContractIsPreserved(t *testing.T) {
	_, err := (&Bridge{}).SendText("invalid", "hello", "")
	if err == nil || err.Error() != "invalid WhatsApp conversation id: invalid" {
		t.Fatalf("SendText() error = %v, want unchanged parse error", err)
	}
	if errors.Is(err, ErrSendNotDispatched) {
		t.Fatalf("legacy SendText() error gained the v2-only marker: %v", err)
	}
}
