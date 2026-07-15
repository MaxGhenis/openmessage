package whatsapplive

import (
	"context"
	"errors"
	"testing"

	"go.mau.fi/whatsmeow"
	waE2E "go.mau.fi/whatsmeow/proto/waE2E"
	watypes "go.mau.fi/whatsmeow/types"
)

func TestSendReactionRequestMapsActionsAndTargetSender(t *testing.T) {
	tests := []struct {
		name            string
		action          string
		targetAuthorID  string
		wantText        string
		wantFromMe      bool
		wantParticipant string
	}{
		{
			name:            "add from another group participant",
			action:          "add",
			targetAuthorID:  "+15557654321",
			wantText:        "🔥",
			wantParticipant: "15557654321@s.whatsapp.net",
		},
		{
			name:            "switch overwrites with another emoji",
			action:          "switch",
			targetAuthorID:  "+15557654321",
			wantText:        "🔥",
			wantParticipant: "15557654321@s.whatsapp.net",
		},
		{
			name:            "remove sends empty reaction text",
			action:          "remove",
			targetAuthorID:  "+15557654321",
			wantText:        "",
			wantParticipant: "15557654321@s.whatsapp.net",
		},
		{
			name:       "empty author targets own message",
			action:     "add",
			wantText:   "🔥",
			wantFromMe: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			bridge := connectedMediaTestBridge()
			originalSend := sendTextMessage
			originalIsConnected := clientIsConnected
			defer func() {
				sendTextMessage = originalSend
				clientIsConnected = originalIsConnected
			}()
			clientIsConnected = func(*whatsmeow.Client) bool { return true }

			var gotTo watypes.JID
			var gotMessage *waE2E.Message
			var gotRequestID watypes.MessageID
			sendTextMessage = func(_ *whatsmeow.Client, _ context.Context, to watypes.JID, message *waE2E.Message, extra ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error) {
				gotTo = to
				gotMessage = message
				if len(extra) != 1 {
					t.Fatalf("SendMessage extras = %d, want exactly one", len(extra))
				}
				gotRequestID = extra[0].ID
				return whatsmeow.SendResponse{}, nil
			}

			err := bridge.SendReactionRequest(
				"whatsapp:120363019999999999@g.us",
				"target-remote-id",
				test.targetAuthorID,
				"🔥",
				test.action,
				"outbox-request-123",
			)
			if err != nil {
				t.Fatalf("SendReactionRequest() error = %v", err)
			}
			if gotTo.String() != "120363019999999999@g.us" {
				t.Fatalf("recipient = %q, want group JID", gotTo.String())
			}
			if gotRequestID != deriveWebMessageID("outbox-request-123") {
				t.Fatalf("request ID = %q, want derived ID %q", gotRequestID, deriveWebMessageID("outbox-request-123"))
			}
			reaction := extractReactionMessage(gotMessage)
			if reaction == nil {
				t.Fatal("expected outgoing reaction message")
			}
			if reaction.GetKey().GetID() != "target-remote-id" {
				t.Fatalf("target ID = %q, want target-remote-id", reaction.GetKey().GetID())
			}
			if reaction.GetText() != test.wantText {
				t.Fatalf("reaction text = %q, want %q", reaction.GetText(), test.wantText)
			}
			if reaction.GetKey().GetFromMe() != test.wantFromMe {
				t.Fatalf("target FromMe = %t, want %t", reaction.GetKey().GetFromMe(), test.wantFromMe)
			}
			if reaction.GetKey().GetParticipant() != test.wantParticipant {
				t.Fatalf("target participant = %q, want %q", reaction.GetKey().GetParticipant(), test.wantParticipant)
			}
		})
	}
}

func TestSendReactionRequestMarksOnlyPreDispatchFailures(t *testing.T) {
	t.Run("invalid conversation", func(t *testing.T) {
		err := connectedMediaTestBridge().SendReactionRequest(
			"invalid",
			"target-remote-id",
			"+15557654321",
			"🔥",
			"add",
			"request-1",
		)
		if err == nil || !errors.Is(err, ErrSendNotDispatched) {
			t.Fatalf("SendReactionRequest() error = %v, want not-dispatched marker", err)
		}
		if err.Error() != "invalid WhatsApp conversation id: invalid" {
			t.Fatalf("error text = %q, want parse error", err.Error())
		}
	})

	t.Run("transport disconnected before send", func(t *testing.T) {
		err := (&Bridge{}).SendReactionRequest(
			"whatsapp:15551234567@s.whatsapp.net",
			"target-remote-id",
			"",
			"🔥",
			"add",
			"request-1",
		)
		if !errors.Is(err, whatsmeow.ErrNotConnected) || !errors.Is(err, ErrSendNotDispatched) {
			t.Fatalf("SendReactionRequest() error = %v, want not-connected cause and not-dispatched marker", err)
		}
	})

	t.Run("send-boundary failure is uncertain and reported by retained core", func(t *testing.T) {
		bridge := connectedMediaTestBridge()
		originalSend := sendTextMessage
		originalIsConnected := clientIsConnected
		defer func() {
			sendTextMessage = originalSend
			clientIsConnected = originalIsConnected
		}()
		clientIsConnected = func(*whatsmeow.Client) bool { return true }
		want := whatsmeow.ErrNotConnected
		sendTextMessage = func(*whatsmeow.Client, context.Context, watypes.JID, *waE2E.Message, ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error) {
			return whatsmeow.SendResponse{}, want
		}
		var reported error
		bridge.callbacks.OnConnectionError = func(err error) { reported = err }

		err := bridge.SendReactionRequest(
			"whatsapp:15551234567@s.whatsapp.net",
			"target-remote-id",
			"",
			"🔥",
			"add",
			"request-1",
		)
		if !errors.Is(err, want) {
			t.Fatalf("SendReactionRequest() error = %v, want send cause", err)
		}
		if errors.Is(err, ErrSendNotDispatched) {
			t.Fatalf("SendReactionRequest() error = %v, send-boundary failure must stay uncertain", err)
		}
		if !errors.Is(reported, want) {
			t.Fatalf("retained lifecycle report = %v, want send cause", reported)
		}
	})
}
