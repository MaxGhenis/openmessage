package whatsapplive

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	watypes "go.mau.fi/whatsmeow/types"
)

func TestMarkReadRequestMapsTransportArguments(t *testing.T) {
	readAt := time.Date(2026, time.July, 15, 14, 3, 2, 123, time.UTC)
	tests := []struct {
		name           string
		conversationID string
		messageIDs     []string
		senderID       string
		wantChat       string
		wantSender     string
	}{
		{
			name:           "group messages from participant",
			conversationID: "whatsapp:120363019999999999@g.us",
			messageIDs:     []string{"message-one", "message-two"},
			senderID:       "+15557654321",
			wantChat:       "120363019999999999@g.us",
			wantSender:     "15557654321@s.whatsapp.net",
		},
		{
			name:           "empty sender remains self",
			conversationID: "whatsapp:15551234567@s.whatsapp.net",
			messageIDs:     []string{"message-three"},
			wantChat:       "15551234567@s.whatsapp.net",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			host := connectedMediaTestBridge()
			originalMarkRead := markReadMessages
			originalIsConnected := clientIsConnected
			defer func() {
				markReadMessages = originalMarkRead
				clientIsConnected = originalIsConnected
			}()
			clientIsConnected = func(*whatsmeow.Client) bool { return true }

			var (
				gotClient    *whatsmeow.Client
				gotContext   context.Context
				gotIDs       []watypes.MessageID
				gotReadAt    time.Time
				gotChat      watypes.JID
				gotSender    watypes.JID
				gotExtra     []watypes.ReceiptType
				transportHit int
			)
			markReadMessages = func(
				cli *whatsmeow.Client,
				ctx context.Context,
				ids []watypes.MessageID,
				timestamp time.Time,
				chat, sender watypes.JID,
				extra ...watypes.ReceiptType,
			) error {
				transportHit++
				gotClient = cli
				gotContext = ctx
				gotIDs = append([]watypes.MessageID(nil), ids...)
				gotReadAt = timestamp
				gotChat = chat
				gotSender = sender
				gotExtra = append([]watypes.ReceiptType(nil), extra...)
				return nil
			}

			err := host.MarkReadRequest(
				test.conversationID,
				test.messageIDs,
				test.senderID,
				readAt,
			)
			if err != nil {
				t.Fatalf("MarkReadRequest() error = %v", err)
			}
			if transportHit != 1 {
				t.Fatalf("MarkRead transport calls = %d, want one", transportHit)
			}
			if gotClient != host.client {
				t.Fatalf("MarkRead client = %p, want host client %p", gotClient, host.client)
			}
			if gotContext == nil {
				t.Fatal("MarkRead context is nil")
			}
			if len(gotIDs) != len(test.messageIDs) {
				t.Fatalf("MarkRead IDs = %q, want %q", gotIDs, test.messageIDs)
			}
			for i, want := range test.messageIDs {
				if gotIDs[i] != watypes.MessageID(want) {
					t.Fatalf("MarkRead ID[%d] = %q, want %q", i, gotIDs[i], want)
				}
			}
			if !gotReadAt.Equal(readAt) {
				t.Fatalf("MarkRead timestamp = %s, want %s", gotReadAt, readAt)
			}
			if gotChat.String() != test.wantChat {
				t.Fatalf("MarkRead chat = %q, want %q", gotChat.String(), test.wantChat)
			}
			if test.wantSender == "" {
				if !gotSender.IsEmpty() {
					t.Fatalf("MarkRead sender = %q, want EmptyJID for self", gotSender.String())
				}
			} else if gotSender.String() != test.wantSender {
				t.Fatalf("MarkRead sender = %q, want %q", gotSender.String(), test.wantSender)
			}
			if len(gotExtra) != 0 {
				t.Fatalf("MarkRead receipt extras = %v, want none so whatsmeow defaults to read", gotExtra)
			}
		})
	}
}

func TestMarkReadRequestRejectsEmptyIDsBeforeTransport(t *testing.T) {
	originalMarkRead := markReadMessages
	defer func() { markReadMessages = originalMarkRead }()

	var transportCalls int
	markReadMessages = func(
		*whatsmeow.Client,
		context.Context,
		[]watypes.MessageID,
		time.Time,
		watypes.JID,
		watypes.JID,
		...watypes.ReceiptType,
	) error {
		transportCalls++
		return nil
	}

	err := connectedMediaTestBridge().MarkReadRequest(
		"whatsapp:15551234567@s.whatsapp.net",
		nil,
		"",
		time.Now(),
	)
	if err == nil || !errors.Is(err, ErrSendNotDispatched) {
		t.Fatalf("MarkReadRequest(empty IDs) error = %v, want not-dispatched marker", err)
	}
	if transportCalls != 0 {
		t.Fatalf("MarkRead transport calls = %d, want zero", transportCalls)
	}
}

func TestMarkReadRequestMarksOtherPreDispatchFailures(t *testing.T) {
	t.Run("invalid conversation", func(t *testing.T) {
		err := connectedMediaTestBridge().MarkReadRequest(
			"invalid",
			[]string{"message-one"},
			"",
			time.Now(),
		)
		if err == nil || !errors.Is(err, ErrSendNotDispatched) {
			t.Fatalf("MarkReadRequest(invalid conversation) error = %v, want not-dispatched marker", err)
		}
	})

	t.Run("transport disconnected before call", func(t *testing.T) {
		err := (&Bridge{}).MarkReadRequest(
			"whatsapp:15551234567@s.whatsapp.net",
			[]string{"message-one"},
			"",
			time.Now(),
		)
		if !errors.Is(err, whatsmeow.ErrNotConnected) || !errors.Is(err, ErrSendNotDispatched) {
			t.Fatalf("MarkReadRequest(disconnected) error = %v, want not-connected cause and not-dispatched marker", err)
		}
	})
}

func TestMarkReadRequestTransportErrorIsWrappedAndReported(t *testing.T) {
	host := connectedMediaTestBridge()
	originalMarkRead := markReadMessages
	originalIsConnected := clientIsConnected
	defer func() {
		markReadMessages = originalMarkRead
		clientIsConnected = originalIsConnected
	}()
	clientIsConnected = func(*whatsmeow.Client) bool { return true }

	want := whatsmeow.ErrNotConnected
	var transportCalls int
	markReadMessages = func(
		*whatsmeow.Client,
		context.Context,
		[]watypes.MessageID,
		time.Time,
		watypes.JID,
		watypes.JID,
		...watypes.ReceiptType,
	) error {
		transportCalls++
		return want
	}
	var reported error
	host.callbacks.OnConnectionError = func(err error) { reported = err }

	err := host.MarkReadRequest(
		"whatsapp:15551234567@s.whatsapp.net",
		[]string{"message-one"},
		"",
		time.Now(),
	)
	if !errors.Is(err, want) {
		t.Fatalf("MarkReadRequest() error = %v, want wrapped transport cause", err)
	}
	if err == want {
		t.Fatalf("MarkReadRequest() returned raw transport error %v, want contextual wrapper", err)
	}
	if errors.Is(err, ErrSendNotDispatched) {
		t.Fatalf("MarkReadRequest() error = %v, transport-call failure must not carry predispatch marker", err)
	}
	if transportCalls != 1 {
		t.Fatalf("MarkRead transport calls = %d, want one", transportCalls)
	}
	if !errors.Is(reported, want) {
		t.Fatalf("retained lifecycle report = %v, want transport cause %v", reported, want)
	}
}
