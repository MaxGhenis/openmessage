package whatsapplive

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.mau.fi/whatsmeow"
	waE2E "go.mau.fi/whatsmeow/proto/waE2E"
	watypes "go.mau.fi/whatsmeow/types"
)

func TestSendReactionRequestMapsActionsAndTargetSender(t *testing.T) {
	const (
		group  = "whatsapp:120363019999999999@g.us"
		direct = "whatsapp:15557654321@s.whatsapp.net"
	)
	tests := []struct {
		name            string
		chat            string
		action          string
		targetAuthorID  string
		targetOutgoing  bool
		wantText        string
		wantFromMe      bool
		wantParticipant string
	}{
		{
			name:            "add from another group participant",
			chat:            group,
			action:          "add",
			targetAuthorID:  "+15557654321",
			wantText:        "🔥",
			wantParticipant: "15557654321@s.whatsapp.net",
		},
		{
			name:            "switch overwrites with another emoji",
			chat:            group,
			action:          "switch",
			targetAuthorID:  "+15557654321",
			wantText:        "🔥",
			wantParticipant: "15557654321@s.whatsapp.net",
		},
		{
			name:            "remove sends empty reaction text",
			chat:            group,
			action:          "remove",
			targetAuthorID:  "+15557654321",
			wantText:        "",
			wantParticipant: "15557654321@s.whatsapp.net",
		},
		{
			name:           "own message in a group",
			chat:           group,
			action:         "add",
			targetOutgoing: true,
			wantText:       "🔥",
			wantFromMe:     true,
		},
		{
			name:           "own message in a 1:1 chat",
			chat:           direct,
			action:         "add",
			targetOutgoing: true,
			wantText:       "🔥",
			wantFromMe:     true,
		},
		// The direction decides, not the stored author: an own message
		// whose author names another number is still this account's.
		{
			name:           "own message whatever its stored author",
			chat:           group,
			action:         "add",
			targetAuthorID: "+15557654321",
			targetOutgoing: true,
			wantText:       "🔥",
			wantFromMe:     true,
		},
		// An incoming 1:1 message stored with no sender is the other
		// party's, as the legacy sender had it. Addressed with an empty
		// sender it would name a message of this account's.
		{
			name:     "incoming 1:1 message with no stored sender",
			chat:     direct,
			action:   "add",
			wantText: "🔥",
		},
		{
			name:           "incoming 1:1 message with a stored sender",
			chat:           direct,
			action:         "add",
			targetAuthorID: "+15557654321",
			wantText:       "🔥",
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

			sentAt, err := bridge.SendReactionRequest(
				test.chat,
				"target-remote-id",
				test.targetAuthorID,
				test.targetOutgoing,
				"🔥",
				test.action,
				"outbox-request-123",
			)
			if err != nil {
				t.Fatalf("SendReactionRequest() error = %v", err)
			}
			wantTo := strings.TrimPrefix(test.chat, "whatsapp:")
			if gotTo.String() != wantTo {
				t.Fatalf("recipient = %q, want the chat %q", gotTo.String(), wantTo)
			}
			if gotRequestID != deriveWebMessageID("outbox-request-123") {
				t.Fatalf("request ID = %q, want derived ID %q", gotRequestID, deriveWebMessageID("outbox-request-123"))
			}
			reaction := extractReactionMessage(gotMessage)
			if reaction == nil {
				t.Fatal("expected outgoing reaction message")
			}
			// The returned time is the reaction's own sender timestamp, the
			// one the ingest path records for it.
			if ms := reaction.GetSenderTimestampMS(); ms <= 0 || sentAt.UnixMilli() != ms {
				t.Fatalf("returned time %d, want the reaction's sender timestamp %d", sentAt.UnixMilli(), ms)
			}
			if reaction.GetKey().GetID() != "target-remote-id" || reaction.GetKey().GetRemoteJID() != wantTo {
				t.Fatalf("target key = %+v, want target-remote-id in %q", reaction.GetKey(), wantTo)
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

// An incoming group message stored with no sender cannot be named: WhatsApp
// keys it by its sender, and an empty one means this account. The reaction is
// refused before anything is sent rather than addressed to a message of this
// account's that does not exist.
func TestSendReactionRequestRefusesAnIncomingGroupTargetWithNoSender(t *testing.T) {
	bridge := connectedMediaTestBridge()
	originalSend := sendTextMessage
	originalIsConnected := clientIsConnected
	defer func() {
		sendTextMessage = originalSend
		clientIsConnected = originalIsConnected
	}()
	clientIsConnected = func(*whatsmeow.Client) bool { return true }
	sends := 0
	sendTextMessage = func(*whatsmeow.Client, context.Context, watypes.JID, *waE2E.Message, ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error) {
		sends++
		return whatsmeow.SendResponse{}, nil
	}

	_, err := bridge.SendReactionRequest(
		"whatsapp:120363019999999999@g.us",
		"target-remote-id",
		"  ",
		false,
		"🔥",
		"add",
		"request-1",
	)
	if !errors.Is(err, ErrReactionTargetAuthorUnknown) || !errors.Is(err, ErrSendNotDispatched) {
		t.Fatalf("SendReactionRequest() error = %v, want the unnamed-target refusal marked not dispatched", err)
	}
	if sends != 0 {
		t.Fatalf("WhatsApp sends = %d, want none", sends)
	}
}

func TestSendReactionRequestMarksOnlyPreDispatchFailures(t *testing.T) {
	t.Run("invalid conversation", func(t *testing.T) {
		_, err := connectedMediaTestBridge().SendReactionRequest(
			"invalid",
			"target-remote-id",
			"+15557654321",
			false,
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
		_, err := (&Bridge{}).SendReactionRequest(
			"whatsapp:15551234567@s.whatsapp.net",
			"target-remote-id",
			"",
			false,
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

		_, err := bridge.SendReactionRequest(
			"whatsapp:15551234567@s.whatsapp.net",
			"target-remote-id",
			"",
			false,
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
