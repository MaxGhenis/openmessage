package whatsapplive

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/whatsmeow"
	waCommon "go.mau.fi/whatsmeow/proto/waCommon"
	waE2E "go.mau.fi/whatsmeow/proto/waE2E"
	wastore "go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	watypes "go.mau.fi/whatsmeow/types"
	waevents "go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"

	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/whatsappmedia"
)

func TestExtractMessageBody(t *testing.T) {
	t.Run("conversation text", func(t *testing.T) {
		msg := &waE2E.Message{Conversation: strPtr("hello")}
		if got := extractMessageBody(msg); got != "hello" {
			t.Fatalf("got %q, want hello", got)
		}
	})

	t.Run("extended text", func(t *testing.T) {
		msg := &waE2E.Message{
			ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: strPtr("linked text")},
		}
		if got := extractMessageBody(msg); got != "linked text" {
			t.Fatalf("got %q, want linked text", got)
		}
	})

	t.Run("media placeholder", func(t *testing.T) {
		msg := &waE2E.Message{
			ImageMessage: &waE2E.ImageMessage{},
		}
		if got := extractMessageBody(msg); got != "[Photo]" {
			t.Fatalf("got %q, want [Photo]", got)
		}
	})

	t.Run("wrapped media placeholder", func(t *testing.T) {
		msg := &waE2E.Message{
			EphemeralMessage: &waE2E.FutureProofMessage{
				Message: &waE2E.Message{
					ViewOnceMessageV2: &waE2E.FutureProofMessage{
						Message: &waE2E.Message{
							ImageMessage: &waE2E.ImageMessage{Caption: strPtr("wrapped photo")},
						},
					},
				},
			},
		}
		if got := extractMessageBody(msg); got != "wrapped photo" {
			t.Fatalf("got %q, want wrapped photo", got)
		}
	})

	t.Run("location with name", func(t *testing.T) {
		msg := &waE2E.Message{
			LocationMessage: &waE2E.LocationMessage{Name: strPtr("Blue Bottle Coffee")},
		}
		if got := extractMessageBody(msg); got != "[Location: Blue Bottle Coffee]" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("location without name falls back to address", func(t *testing.T) {
		msg := &waE2E.Message{
			LocationMessage: &waE2E.LocationMessage{Address: strPtr("123 Market St")},
		}
		if got := extractMessageBody(msg); got != "[Location: 123 Market St]" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("bare location", func(t *testing.T) {
		msg := &waE2E.Message{LocationMessage: &waE2E.LocationMessage{}}
		if got := extractMessageBody(msg); got != "[Location]" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("live location", func(t *testing.T) {
		msg := &waE2E.Message{LiveLocationMessage: &waE2E.LiveLocationMessage{}}
		if got := extractMessageBody(msg); got != "[Live location]" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("poll creation v3 with question", func(t *testing.T) {
		msg := &waE2E.Message{
			PollCreationMessageV3: &waE2E.PollCreationMessage{Name: strPtr("Dinner Friday?")},
		}
		if got := extractMessageBody(msg); got != "[Poll: Dinner Friday?]" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("poll creation v2 with question", func(t *testing.T) {
		msg := &waE2E.Message{
			PollCreationMessageV2: &waE2E.PollCreationMessage{Name: strPtr("Which park?")},
		}
		if got := extractMessageBody(msg); got != "[Poll: Which park?]" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("poll update", func(t *testing.T) {
		msg := &waE2E.Message{PollUpdateMessage: &waE2E.PollUpdateMessage{}}
		if got := extractMessageBody(msg); got != "[Poll vote]" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("event", func(t *testing.T) {
		msg := &waE2E.Message{
			EventMessage: &waE2E.EventMessage{Name: strPtr("Hike at Lands End")},
		}
		if got := extractMessageBody(msg); got != "[Event: Hike at Lands End]" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("event canceled", func(t *testing.T) {
		b := true
		msg := &waE2E.Message{
			EventMessage: &waE2E.EventMessage{Name: strPtr("Dinner"), IsCanceled: &b},
		}
		if got := extractMessageBody(msg); got != "[Event canceled: Dinner]" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("group invite", func(t *testing.T) {
		msg := &waE2E.Message{
			GroupInviteMessage: &waE2E.GroupInviteMessage{GroupName: strPtr("Hikers")},
		}
		if got := extractMessageBody(msg); got != "[Group invite: Hikers]" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("pin in chat (pin)", func(t *testing.T) {
		pinType := waE2E.PinInChatMessage_Type(1)
		msg := &waE2E.Message{
			PinInChatMessage: &waE2E.PinInChatMessage{Type: &pinType},
		}
		if got := extractMessageBody(msg); got != "[Pinned message]" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("pin in chat (unpin)", func(t *testing.T) {
		unpinType := waE2E.PinInChatMessage_Type(2)
		msg := &waE2E.Message{
			PinInChatMessage: &waE2E.PinInChatMessage{Type: &unpinType},
		}
		if got := extractMessageBody(msg); got != "[Unpinned message]" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("call log (voice)", func(t *testing.T) {
		msg := &waE2E.Message{CallLogMesssage: &waE2E.CallLogMessage{}}
		if got := extractMessageBody(msg); got != "[Voice call]" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("call log (video)", func(t *testing.T) {
		video := true
		msg := &waE2E.Message{
			CallLogMesssage: &waE2E.CallLogMessage{IsVideo: &video},
		}
		if got := extractMessageBody(msg); got != "[Video call]" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("ProtocolMessage is dropped silently", func(t *testing.T) {
		// Admin/control traffic (group-key rotations, ephemeral settings,
		// history sync notifications, etc.). Surfacing these as rows
		// spams threads with [Unsupported message] every time someone
		// rotates keys. Revoke and edit have their own ingestion paths.
		msg := &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{}}
		if got := extractMessageBody(msg); got != "" {
			t.Fatalf("got %q, want empty", got)
		}
	})

	t.Run("SenderKeyDistributionMessage is dropped silently", func(t *testing.T) {
		msg := &waE2E.Message{
			SenderKeyDistributionMessage: &waE2E.SenderKeyDistributionMessage{},
		}
		if got := extractMessageBody(msg); got != "" {
			t.Fatalf("got %q, want empty", got)
		}
	})

	t.Run("InteractiveMessage surfaces body text", func(t *testing.T) {
		msg := &waE2E.Message{
			InteractiveMessage: &waE2E.InteractiveMessage{
				Body: &waE2E.InteractiveMessage_Body{Text: strPtr("RSVP by Friday")},
			},
		}
		if got := extractMessageBody(msg); got != "RSVP by Friday" {
			t.Fatalf("got %q, want RSVP by Friday", got)
		}
	})

	t.Run("ButtonsMessage surfaces content text", func(t *testing.T) {
		msg := &waE2E.Message{
			ButtonsMessage: &waE2E.ButtonsMessage{ContentText: strPtr("Pick a time")},
		}
		if got := extractMessageBody(msg); got != "Pick a time" {
			t.Fatalf("got %q, want Pick a time", got)
		}
	})

	t.Run("ListMessage surfaces description", func(t *testing.T) {
		msg := &waE2E.Message{
			ListMessage: &waE2E.ListMessage{Description: strPtr("Select an option")},
		}
		if got := extractMessageBody(msg); got != "Select an option" {
			t.Fatalf("got %q, want Select an option", got)
		}
	})

	t.Run("KeepInChatMessage placeholder", func(t *testing.T) {
		msg := &waE2E.Message{KeepInChatMessage: &waE2E.KeepInChatMessage{}}
		if got := extractMessageBody(msg); got != "[Kept message]" {
			t.Fatalf("got %q, want [Kept message]", got)
		}
	})

	t.Run("TemplateMessage placeholder", func(t *testing.T) {
		msg := &waE2E.Message{TemplateMessage: &waE2E.TemplateMessage{}}
		if got := extractMessageBody(msg); got != "[Template message]" {
			t.Fatalf("got %q, want [Template message]", got)
		}
	})

	t.Run("PTV video with caption", func(t *testing.T) {
		msg := &waE2E.Message{
			PtvMessage: &waE2E.VideoMessage{Caption: strPtr("hi from beach")},
		}
		if got := extractMessageBody(msg); got != "hi from beach" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("PTV video no caption", func(t *testing.T) {
		msg := &waE2E.Message{PtvMessage: &waE2E.VideoMessage{}}
		if got := extractMessageBody(msg); got != "[Video]" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("album placeholder", func(t *testing.T) {
		msg := &waE2E.Message{AlbumMessage: &waE2E.AlbumMessage{}}
		if got := extractMessageBody(msg); got != "[Album]" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("sticker pack with name", func(t *testing.T) {
		msg := &waE2E.Message{
			StickerPackMessage: &waE2E.StickerPackMessage{Name: strPtr("Cats")},
		}
		if got := extractMessageBody(msg); got != "[Sticker pack: Cats]" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("comment recurses into image caption", func(t *testing.T) {
		// A comment wrapping an image with a caption should return the caption,
		// not the generic [Comment] placeholder.
		msg := &waE2E.Message{
			CommentMessage: &waE2E.CommentMessage{
				Message: &waE2E.Message{
					ImageMessage: &waE2E.ImageMessage{Caption: strPtr("cute pic")},
				},
			},
		}
		if got := extractMessageBody(msg); got != "cute pic" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("comment on unknown subtype still returns placeholder", func(t *testing.T) {
		msg := &waE2E.Message{
			CommentMessage: &waE2E.CommentMessage{
				Message: &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{}},
			},
		}
		if got := extractMessageBody(msg); got != "[Comment]" {
			t.Fatalf("got %q", got)
		}
	})
}

func TestDescribeWhatsAppMessageContent(t *testing.T) {
	t.Run("nil message", func(t *testing.T) {
		if got := describeWhatsAppMessageContent(nil); got != "" {
			t.Fatalf("got %q, want empty", got)
		}
	})

	t.Run("protocol message is surfaced", func(t *testing.T) {
		msg := &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{}}
		got := describeWhatsAppMessageContent(msg)
		if !strings.Contains(got, "Protocol") {
			t.Fatalf("got %q, want it to mention Protocol", got)
		}
	})

	t.Run("multiple types are joined", func(t *testing.T) {
		msg := &waE2E.Message{
			ProtocolMessage: &waE2E.ProtocolMessage{},
			ButtonsMessage:  &waE2E.ButtonsMessage{},
		}
		got := describeWhatsAppMessageContent(msg)
		if !strings.Contains(got, "Buttons") || !strings.Contains(got, "Protocol") {
			t.Fatalf("got %q, expected both Buttons and Protocol", got)
		}
	})
}

func TestExtractReplyToID(t *testing.T) {
	msg := &waE2E.Message{
		ExtendedTextMessage: &waE2E.ExtendedTextMessage{
			ContextInfo: &waE2E.ContextInfo{StanzaID: strPtr("abc123")},
		},
	}
	if got := extractReplyToID(msg); got != "whatsapp:abc123" {
		t.Fatalf("got %q, want whatsapp:abc123", got)
	}
}

func TestQRCodeRendersDataURL(t *testing.T) {
	bridge := &Bridge{
		qr: QRSnapshot{
			Code: "2@ABCDEF",
		},
	}
	snap, err := bridge.QRCode()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(snap.PNGDataURL, "data:image/png;base64,") {
		t.Fatalf("unexpected QR data url: %q", snap.PNGDataURL)
	}
}

func TestSessionStoreDSNUsesModerncPragmas(t *testing.T) {
	got := sessionStoreDSN("/tmp/wa store.db")
	if !strings.Contains(got, "_pragma=foreign_keys(1)") {
		t.Fatalf("dsn missing foreign_keys pragma: %q", got)
	}
	if !strings.Contains(got, "_pragma=busy_timeout(5000)") {
		t.Fatalf("dsn missing busy_timeout pragma: %q", got)
	}
	if !strings.Contains(got, "/tmp/wa%20store.db") {
		t.Fatalf("dsn did not encode path correctly: %q", got)
	}
}

func TestBridgeNewInitializesSQLiteSessionStore(t *testing.T) {
	dataDir := t.TempDir()
	store, err := db.New(filepath.Join(dataDir, "messages.db"))
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	defer store.Close()

	sessionPath := filepath.Join(dataDir, "whatsapp-session.db")
	bridge, err := New(sessionPath, store, zerolog.Nop(), Callbacks{})
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	defer bridge.Close()

	info, err := os.Stat(sessionPath)
	if err != nil {
		t.Fatalf("session store not created: %v", err)
	}
	if info.IsDir() {
		t.Fatalf("session path is a directory")
	}
}

func TestBridgeSendTextBuildsOutgoingWhatsAppMessage(t *testing.T) {
	ownJID := watypes.NewJID("15551230000", watypes.DefaultUserServer)
	bridge := &Bridge{
		connected: true,
		client: &whatsmeow.Client{
			Store: &wastore.Device{
				ID:       &ownJID,
				PushName: "OpenMessage",
			},
		},
	}

	originalSend := sendTextMessage
	originalIsConnected := clientIsConnected
	defer func() {
		sendTextMessage = originalSend
		clientIsConnected = originalIsConnected
	}()
	clientIsConnected = func(_ *whatsmeow.Client) bool { return true }

	var capturedTo watypes.JID
	var capturedMsg *waE2E.Message
	var capturedID string
	sendTextMessage = func(_ *whatsmeow.Client, _ context.Context, to watypes.JID, message *waE2E.Message, extra ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error) {
		capturedTo = to
		capturedMsg = message
		if len(extra) > 0 {
			capturedID = string(extra[0].ID)
		}
		return whatsmeow.SendResponse{
			ID:        watypes.MessageID(capturedID),
			Timestamp: time.UnixMilli(1700000000123),
		}, nil
	}

	msg, err := bridge.SendText("whatsapp:15551234567@s.whatsapp.net", "hello from wa", "whatsapp:reply-123")
	if err != nil {
		t.Fatalf("SendText(): %v", err)
	}

	if capturedTo.String() != "15551234567@s.whatsapp.net" {
		t.Fatalf("sent to %q, want target jid", capturedTo.String())
	}
	if capturedMsg.GetExtendedTextMessage().GetText() != "hello from wa" {
		t.Fatalf("sent text = %q, want body", capturedMsg.GetExtendedTextMessage().GetText())
	}
	if capturedMsg.GetExtendedTextMessage().GetContextInfo().GetStanzaID() != "reply-123" {
		t.Fatalf("reply stanza = %q, want reply-123", capturedMsg.GetExtendedTextMessage().GetContextInfo().GetStanzaID())
	}
	if capturedID == "" {
		t.Fatal("expected generated WhatsApp message id")
	}
	if msg.MessageID != "whatsapp:"+capturedID {
		t.Fatalf("message id = %q, want whatsapp:%s", msg.MessageID, capturedID)
	}
	if msg.SourceID != capturedID {
		t.Fatalf("source id = %q, want %q", msg.SourceID, capturedID)
	}
	if msg.SenderName != "OpenMessage" {
		t.Fatalf("sender name = %q, want OpenMessage", msg.SenderName)
	}
	if msg.SenderNumber != "+15551230000" {
		t.Fatalf("sender number = %q, want +15551230000", msg.SenderNumber)
	}
	if msg.ReplyToID != "whatsapp:reply-123" {
		t.Fatalf("reply to = %q, want whatsapp:reply-123", msg.ReplyToID)
	}
	if msg.TimestampMS != 1700000000123 {
		t.Fatalf("timestamp = %d, want 1700000000123", msg.TimestampMS)
	}
}

func TestBridgeSendTextReconnectsStaleClientBeforeSend(t *testing.T) {
	ownJID := watypes.NewJID("15551230000", watypes.DefaultUserServer)
	bridge := &Bridge{
		connected: true,
		client: &whatsmeow.Client{
			Store: &wastore.Device{
				ID:       &ownJID,
				PushName: "OpenMessage",
			},
		},
	}

	originalSend := sendTextMessage
	originalConnect := connectClient
	originalIsConnected := clientIsConnected
	originalLaunch := launchConnect
	defer func() {
		sendTextMessage = originalSend
		connectClient = originalConnect
		clientIsConnected = originalIsConnected
		launchConnect = originalLaunch
	}()

	var wireConnected bool
	var connectCalls int
	var sendCalls int
	clientIsConnected = func(_ *whatsmeow.Client) bool {
		return wireConnected
	}
	launchConnect = func(b *Bridge, cli *whatsmeow.Client) {
		b.runConnect(cli)
	}
	connectClient = func(_ *whatsmeow.Client) error {
		connectCalls++
		bridge.mu.Lock()
		wireConnected = true
		bridge.connected = true
		bridge.connecting = false
		bridge.lastError = ""
		bridge.mu.Unlock()
		return nil
	}
	sendTextMessage = func(_ *whatsmeow.Client, _ context.Context, _ watypes.JID, _ *waE2E.Message, extra ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error) {
		sendCalls++
		return whatsmeow.SendResponse{
			ID:        watypes.MessageID(extra[0].ID),
			Timestamp: time.UnixMilli(1700000000999),
		}, nil
	}

	msg, err := bridge.SendText("whatsapp:15551234567@s.whatsapp.net", "hello after reconnect", "")
	if err != nil {
		t.Fatalf("SendText(): %v", err)
	}
	if connectCalls != 1 {
		t.Fatalf("connect calls = %d, want 1", connectCalls)
	}
	if sendCalls != 1 {
		t.Fatalf("send calls = %d, want 1", sendCalls)
	}
	if msg.Body != "hello after reconnect" {
		t.Fatalf("body = %q, want hello after reconnect", msg.Body)
	}
	if !bridge.Status().Connected {
		t.Fatal("expected bridge status to report connected after reconnect")
	}
}

func TestBridgeLeaveGroupLeavesRemoteAndDeletesLocalThread(t *testing.T) {
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	defer store.Close()

	if err := store.UpsertConversation(&db.Conversation{
		ConversationID: "whatsapp:120363019999999999@g.us",
		Name:           "Spam Group",
		IsGroup:        true,
		LastMessageTS:  1000,
		SourcePlatform: "whatsapp",
	}); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}
	if err := store.UpsertMessage(&db.Message{
		MessageID:      "whatsapp:m1",
		ConversationID: "whatsapp:120363019999999999@g.us",
		Body:           "join us",
		TimestampMS:    1000,
		SourcePlatform: "whatsapp",
	}); err != nil {
		t.Fatalf("seed message: %v", err)
	}

	ownJID := watypes.NewJID("15551230000", watypes.DefaultUserServer)
	bridge := &Bridge{
		store:     store,
		connected: true,
		client: &whatsmeow.Client{
			Store: &wastore.Device{
				ID:       &ownJID,
				PushName: "OpenMessage",
			},
		},
	}

	originalLeaveGroup := leaveGroup
	originalIsConnected := clientIsConnected
	defer func() {
		leaveGroup = originalLeaveGroup
		clientIsConnected = originalIsConnected
	}()
	clientIsConnected = func(_ *whatsmeow.Client) bool { return true }

	var leftJID watypes.JID
	leaveGroup = func(_ *whatsmeow.Client, _ context.Context, jid watypes.JID) error {
		leftJID = jid
		return nil
	}

	if err := bridge.LeaveGroup("whatsapp:120363019999999999@g.us"); err != nil {
		t.Fatalf("LeaveGroup(): %v", err)
	}
	if leftJID.String() != "120363019999999999@g.us" {
		t.Fatalf("left jid = %q, want group jid", leftJID.String())
	}
	if _, err := store.GetConversation("whatsapp:120363019999999999@g.us"); err == nil {
		t.Fatal("expected group conversation to be deleted locally")
	}
	msgs, err := store.GetMessagesByConversation("whatsapp:120363019999999999@g.us", 10)
	if err != nil {
		t.Fatalf("GetMessagesByConversation(): %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("expected group messages to be deleted, got %d", len(msgs))
	}
}

func TestBridgeLeaveGroupSuppressesLaterStubRecreation(t *testing.T) {
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	defer store.Close()

	const conversationID = "whatsapp:120363019999999999@g.us"
	if err := store.UpsertConversation(&db.Conversation{
		ConversationID: conversationID,
		Name:           "Spam Group",
		IsGroup:        true,
		LastMessageTS:  1000,
		SourcePlatform: "whatsapp",
	}); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}

	ownJID := watypes.NewJID("15551230000", watypes.DefaultUserServer)
	groupJID := watypes.NewJID("120363019999999999", watypes.GroupServer)
	bridge := &Bridge{
		store:     store,
		connected: true,
		client: &whatsmeow.Client{
			Store: &wastore.Device{
				ID:       &ownJID,
				PushName: "OpenMessage",
			},
		},
		logger: zerolog.Nop(),
	}

	originalLeaveGroup := leaveGroup
	originalIsConnected := clientIsConnected
	defer func() {
		leaveGroup = originalLeaveGroup
		clientIsConnected = originalIsConnected
	}()
	clientIsConnected = func(_ *whatsmeow.Client) bool { return true }
	leaveGroup = func(_ *whatsmeow.Client, _ context.Context, jid watypes.JID) error {
		if jid != groupJID {
			t.Fatalf("jid = %q, want %q", jid.String(), groupJID.String())
		}
		return nil
	}

	if err := bridge.LeaveGroup(conversationID); err != nil {
		t.Fatalf("LeaveGroup(): %v", err)
	}

	bridge.handleGroupInfo(&waevents.GroupInfo{JID: groupJID})

	if _, err := store.GetConversation(conversationID); err == nil {
		t.Fatal("expected tombstoned group to stay deleted after later group-info update")
	}
}

func TestBridgeLeaveGroupRejectsDirectChats(t *testing.T) {
	bridge := &Bridge{}
	err := bridge.LeaveGroup("whatsapp:15551234567@s.whatsapp.net")
	if err == nil || !strings.Contains(err.Error(), "not a WhatsApp group") {
		t.Fatalf("LeaveGroup() error = %v, want group validation", err)
	}
}

func TestHandleGroupInfoDeletesConversationWhenOwnAccountLeavesGroup(t *testing.T) {
	dataDir := t.TempDir()
	store, err := db.New(filepath.Join(dataDir, "messages.db"))
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	defer store.Close()

	const conversationID = "whatsapp:120363019999999999@g.us"
	if err := store.UpsertConversation(&db.Conversation{
		ConversationID: conversationID,
		Name:           "Spam Group",
		IsGroup:        true,
		LastMessageTS:  1000,
		SourcePlatform: "whatsapp",
	}); err != nil {
		t.Fatalf("UpsertConversation(): %v", err)
	}
	if err := store.UpsertMessage(&db.Message{
		MessageID:      "whatsapp:m1",
		ConversationID: conversationID,
		Body:           "hi",
		TimestampMS:    1000,
		SourcePlatform: "whatsapp",
	}); err != nil {
		t.Fatalf("UpsertMessage(): %v", err)
	}

	ownJID := watypes.NewJID("15551230000", watypes.DefaultUserServer)
	groupJID := watypes.NewJID("120363019999999999", watypes.GroupServer)
	bridge := &Bridge{
		store: store,
		client: &whatsmeow.Client{
			Store: &wastore.Device{
				ID: &ownJID,
			},
		},
		logger: zerolog.Nop(),
	}

	bridge.handleGroupInfo(&waevents.GroupInfo{
		JID:   groupJID,
		Name:  &watypes.GroupName{Name: "Spam Group"},
		Leave: []watypes.JID{ownJID},
	})

	if _, err := store.GetConversation(conversationID); err == nil {
		t.Fatal("expected conversation to be deleted after own leave event")
	}
	msgs, err := store.GetMessagesByConversation(conversationID, 10)
	if err != nil {
		t.Fatalf("GetMessagesByConversation(): %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("expected group messages to be deleted, got %d", len(msgs))
	}
}

func TestHandleGroupInfoOwnJoinClearsSuppressionAndRestoresGroup(t *testing.T) {
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	defer store.Close()

	ownJID := watypes.NewJID("15551230000", watypes.DefaultUserServer)
	groupJID := watypes.NewJID("120363019999999999", watypes.GroupServer)
	conversationID := waConversationID(groupJID)
	bridge := &Bridge{
		store:              store,
		recentlyLeftGroups: map[string]time.Time{conversationID: time.Now().Add(time.Hour)},
		client: &whatsmeow.Client{
			Store: &wastore.Device{
				ID: &ownJID,
			},
		},
		logger: zerolog.Nop(),
	}

	bridge.handleGroupInfo(&waevents.GroupInfo{
		JID:  groupJID,
		Name: &watypes.GroupName{Name: "Back Again"},
		Join: []watypes.JID{ownJID},
	})

	convo, err := store.GetConversation(conversationID)
	if err != nil {
		t.Fatalf("GetConversation(): %v", err)
	}
	if convo.Name != "Back Again" {
		t.Fatalf("name = %q, want Back Again", convo.Name)
	}
	if bridge.shouldSuppressLeftGroup(conversationID) {
		t.Fatal("expected own join to clear left-group suppression")
	}
}

func TestBridgeSendTextTimeoutStartsReconnect(t *testing.T) {
	ownJID := watypes.NewJID("15551230000", watypes.DefaultUserServer)
	bridge := &Bridge{
		connected: true,
		client: &whatsmeow.Client{
			Store: &wastore.Device{
				ID:       &ownJID,
				PushName: "OpenMessage",
			},
		},
	}

	originalSend := sendTextMessage
	originalConnect := connectClient
	originalIsConnected := clientIsConnected
	originalLaunch := launchConnect
	defer func() {
		sendTextMessage = originalSend
		connectClient = originalConnect
		clientIsConnected = originalIsConnected
		launchConnect = originalLaunch
	}()

	var wireConnected = true
	reconnectStarted := make(chan struct{}, 1)
	clientIsConnected = func(_ *whatsmeow.Client) bool {
		return wireConnected
	}
	launchConnect = func(b *Bridge, cli *whatsmeow.Client) {
		b.runConnect(cli)
	}
	connectClient = func(_ *whatsmeow.Client) error {
		select {
		case reconnectStarted <- struct{}{}:
		default:
		}
		return nil
	}
	sendTextMessage = func(_ *whatsmeow.Client, _ context.Context, _ watypes.JID, _ *waE2E.Message, extra ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error) {
		_ = extra
		return whatsmeow.SendResponse{}, context.DeadlineExceeded
	}

	_, err := bridge.SendText("whatsapp:15551234567@s.whatsapp.net", "hello timeout", "")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("SendText() error = %v, want context deadline exceeded", err)
	}
	select {
	case <-reconnectStarted:
	case <-time.After(time.Second):
		t.Fatal("expected reconnect to start after send timeout")
	}
	status := bridge.Status()
	if !status.Connecting {
		t.Fatal("expected bridge to report reconnecting after send timeout")
	}
	if status.Connected {
		t.Fatal("did not expect bridge to remain connected after send timeout")
	}
}

func TestBridgeSendMediaUploadTimeoutStartsReconnect(t *testing.T) {
	ownJID := watypes.NewJID("15551230000", watypes.DefaultUserServer)
	bridge := &Bridge{
		connected: true,
		client: &whatsmeow.Client{
			Store: &wastore.Device{
				ID:       &ownJID,
				PushName: "OpenMessage",
			},
		},
	}

	originalUpload := uploadMedia
	originalConnect := connectClient
	originalIsConnected := clientIsConnected
	originalLaunch := launchConnect
	defer func() {
		uploadMedia = originalUpload
		connectClient = originalConnect
		clientIsConnected = originalIsConnected
		launchConnect = originalLaunch
	}()

	reconnectStarted := make(chan struct{}, 1)
	clientIsConnected = func(_ *whatsmeow.Client) bool { return true }
	launchConnect = func(b *Bridge, cli *whatsmeow.Client) {
		b.runConnect(cli)
	}
	connectClient = func(_ *whatsmeow.Client) error {
		select {
		case reconnectStarted <- struct{}{}:
		default:
		}
		return nil
	}
	uploadMedia = func(_ *whatsmeow.Client, _ context.Context, _ []byte, _ whatsmeow.MediaType) (whatsmeow.UploadResponse, error) {
		return whatsmeow.UploadResponse{}, context.DeadlineExceeded
	}

	_, err := bridge.SendMedia("whatsapp:15551234567@s.whatsapp.net", []byte("png-bytes"), "photo.png", "image/png", "", "")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("SendMedia() error = %v, want context deadline exceeded", err)
	}
	select {
	case <-reconnectStarted:
	case <-time.After(time.Second):
		t.Fatal("expected reconnect to start after media upload timeout")
	}
	status := bridge.Status()
	if !status.Connecting {
		t.Fatal("expected bridge to report reconnecting after media upload timeout")
	}
	if status.Connected {
		t.Fatal("did not expect bridge to remain connected after media upload timeout")
	}
}

func TestBridgeProfilePhotoFetchesAndCachesAvatar(t *testing.T) {
	bridge := &Bridge{
		connected: true,
		client:    &whatsmeow.Client{},
	}

	originalInfo := getProfilePictureInfo
	originalDownload := downloadProfilePhoto
	originalIsConnected := clientIsConnected
	defer func() {
		getProfilePictureInfo = originalInfo
		downloadProfilePhoto = originalDownload
		clientIsConnected = originalIsConnected
	}()
	clientIsConnected = func(_ *whatsmeow.Client) bool { return true }

	infoCalls := 0
	downloadCalls := 0
	getProfilePictureInfo = func(_ *whatsmeow.Client, _ context.Context, jid watypes.JID, params *whatsmeow.GetProfilePictureParams) (*watypes.ProfilePictureInfo, error) {
		infoCalls++
		if jid.String() != "15551234567@s.whatsapp.net" {
			t.Fatalf("jid = %q, want 15551234567@s.whatsapp.net", jid.String())
		}
		if params == nil || !params.Preview {
			t.Fatalf("expected preview profile photo request, got %#v", params)
		}
		return &watypes.ProfilePictureInfo{
			ID:  "avatar-123",
			URL: "https://example.com/avatar.jpg",
		}, nil
	}
	downloadProfilePhoto = func(_ context.Context, rawURL string) ([]byte, string, error) {
		downloadCalls++
		if rawURL != "https://example.com/avatar.jpg" {
			t.Fatalf("url = %q, want https://example.com/avatar.jpg", rawURL)
		}
		return []byte("jpeg-bytes"), "image/jpeg", nil
	}

	data, mime, err := bridge.ProfilePhoto("whatsapp:15551234567@s.whatsapp.net")
	if err != nil {
		t.Fatalf("ProfilePhoto(): %v", err)
	}
	if string(data) != "jpeg-bytes" {
		t.Fatalf("data = %q, want jpeg-bytes", string(data))
	}
	if mime != "image/jpeg" {
		t.Fatalf("mime = %q, want image/jpeg", mime)
	}

	data2, mime2, err := bridge.ProfilePhoto("whatsapp:15551234567@s.whatsapp.net")
	if err != nil {
		t.Fatalf("ProfilePhoto() cached: %v", err)
	}
	if string(data2) != "jpeg-bytes" || mime2 != "image/jpeg" {
		t.Fatalf("cached avatar = %q %q", string(data2), mime2)
	}
	if infoCalls != 1 {
		t.Fatalf("profile info calls = %d, want 1", infoCalls)
	}
	if downloadCalls != 1 {
		t.Fatalf("download calls = %d, want 1", downloadCalls)
	}
}

func TestBridgeSendReactionBuildsOutgoingWhatsAppReactionAndUpdatesLocalState(t *testing.T) {
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	defer store.Close()

	ownJID := watypes.NewJID("15551230000", watypes.DefaultUserServer)
	bridge := &Bridge{
		store:     store,
		connected: true,
		client: &whatsmeow.Client{
			Store: &wastore.Device{
				ID: &ownJID,
			},
		},
	}

	if err := store.UpsertMessage(&db.Message{
		MessageID:      "whatsapp:target-msg",
		ConversationID: "whatsapp:15551234567@s.whatsapp.net",
		SenderName:     "Jamie Rivera",
		SenderNumber:   "+15551234567",
		Body:           "hello",
		TimestampMS:    1700000000000,
		SourcePlatform: "whatsapp",
		SourceID:       "target-msg",
	}); err != nil {
		t.Fatalf("UpsertMessage(): %v", err)
	}

	originalSend := sendTextMessage
	originalIsConnected := clientIsConnected
	defer func() {
		sendTextMessage = originalSend
		clientIsConnected = originalIsConnected
	}()
	clientIsConnected = func(_ *whatsmeow.Client) bool { return true }

	var capturedTo watypes.JID
	var capturedMsg *waE2E.Message
	sendTextMessage = func(_ *whatsmeow.Client, _ context.Context, to watypes.JID, message *waE2E.Message, _ ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error) {
		capturedTo = to
		capturedMsg = message
		return whatsmeow.SendResponse{}, nil
	}

	if err := bridge.SendReaction("whatsapp:15551234567@s.whatsapp.net", "whatsapp:target-msg", "😂", "add"); err != nil {
		t.Fatalf("SendReaction(): %v", err)
	}

	if capturedTo.String() != "15551234567@s.whatsapp.net" {
		t.Fatalf("sent to %q, want 15551234567@s.whatsapp.net", capturedTo.String())
	}
	reaction := extractReactionMessage(capturedMsg)
	if reaction == nil {
		t.Fatal("expected outgoing reaction message")
	}
	if reaction.GetKey().GetID() != "target-msg" {
		t.Fatalf("target id = %q, want target-msg", reaction.GetKey().GetID())
	}
	if reaction.GetKey().GetFromMe() {
		t.Fatal("expected reaction target to be marked as not-from-me")
	}
	if reaction.GetKey().GetParticipant() != "" {
		t.Fatalf("participant = %q, want empty for direct chat", reaction.GetKey().GetParticipant())
	}
	if reaction.GetText() != "😂" {
		t.Fatalf("emoji = %q, want 😂", reaction.GetText())
	}

	msg, err := store.GetMessageByID("whatsapp:target-msg")
	if err != nil {
		t.Fatalf("GetMessageByID(): %v", err)
	}
	reactions, err := parseStoredReactions(msg.Reactions)
	if err != nil {
		t.Fatalf("parseStoredReactions(): %v", err)
	}
	if len(reactions) != 1 || reactions[0].Emoji != "😂" || reactions[0].Count != 1 {
		t.Fatalf("reactions = %#v, want single 😂 reaction", reactions)
	}
	if len(reactions[0].Actors) != 1 || reactions[0].Actors[0] != "15551230000@s.whatsapp.net" {
		t.Fatalf("reaction actors = %#v, want own jid", reactions[0].Actors)
	}
}

func TestBridgeSendReactionUsesParticipantForWhatsAppGroupMessages(t *testing.T) {
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	defer store.Close()

	ownJID := watypes.NewJID("15551230000", watypes.DefaultUserServer)
	bridge := &Bridge{
		store:     store,
		connected: true,
		client: &whatsmeow.Client{
			Store: &wastore.Device{
				ID: &ownJID,
			},
		},
	}

	if err := store.UpsertMessage(&db.Message{
		MessageID:      "whatsapp:target-group-msg",
		ConversationID: "whatsapp:120363019999999999@g.us",
		SenderName:     "Taylor Price",
		SenderNumber:   "+15557654321",
		Body:           "group hello",
		TimestampMS:    1700000000001,
		SourcePlatform: "whatsapp",
		SourceID:       "target-group-msg",
	}); err != nil {
		t.Fatalf("UpsertMessage(): %v", err)
	}

	originalSend := sendTextMessage
	originalIsConnected := clientIsConnected
	defer func() {
		sendTextMessage = originalSend
		clientIsConnected = originalIsConnected
	}()
	clientIsConnected = func(_ *whatsmeow.Client) bool { return true }

	var capturedMsg *waE2E.Message
	sendTextMessage = func(_ *whatsmeow.Client, _ context.Context, _ watypes.JID, message *waE2E.Message, _ ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error) {
		capturedMsg = message
		return whatsmeow.SendResponse{}, nil
	}

	if err := bridge.SendReaction("whatsapp:120363019999999999@g.us", "whatsapp:target-group-msg", "🔥", "add"); err != nil {
		t.Fatalf("SendReaction(): %v", err)
	}

	reaction := extractReactionMessage(capturedMsg)
	if reaction == nil {
		t.Fatal("expected outgoing reaction message")
	}
	if reaction.GetKey().GetParticipant() != "15557654321@s.whatsapp.net" {
		t.Fatalf("participant = %q, want 15557654321@s.whatsapp.net", reaction.GetKey().GetParticipant())
	}
	if reaction.GetKey().GetFromMe() {
		t.Fatal("expected group reaction target to be marked as not-from-me")
	}
}

func TestBridgeProfilePhotoReturnsMissingWhenNotFound(t *testing.T) {
	bridge := &Bridge{
		connected: true,
		client:    &whatsmeow.Client{},
	}

	originalInfo := getProfilePictureInfo
	originalIsConnected := clientIsConnected
	defer func() {
		getProfilePictureInfo = originalInfo
		clientIsConnected = originalIsConnected
	}()
	clientIsConnected = func(_ *whatsmeow.Client) bool { return true }

	getProfilePictureInfo = func(_ *whatsmeow.Client, _ context.Context, _ watypes.JID, _ *whatsmeow.GetProfilePictureParams) (*watypes.ProfilePictureInfo, error) {
		return nil, nil
	}

	_, _, err := bridge.ProfilePhoto("whatsapp:15551234567@s.whatsapp.net")
	if !errors.Is(err, ErrProfilePhotoNotFound) {
		t.Fatalf("err = %v, want ErrProfilePhotoNotFound", err)
	}
}

func TestDownloadStoredMediaReadsLocalWhatsAppDesktopFile(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv("HOME", tempHome)

	root := filepath.Join(tempHome, "Library", "Group Containers", "group.net.whatsapp.WhatsApp.shared", "Message")
	mediaDir := filepath.Join(root, "Media", "jenn")
	if err := os.MkdirAll(mediaDir, 0o755); err != nil {
		t.Fatalf("mkdir media dir: %v", err)
	}
	fullPath := filepath.Join(mediaDir, "voice.opus")
	if err := os.WriteFile(fullPath, []byte("opus-bytes"), 0o644); err != nil {
		t.Fatalf("write local media: %v", err)
	}

	bridge := &Bridge{}
	data, mimeType, err := bridge.DownloadStoredMedia(&db.Message{
		MessageID:      "whatsapp:m1",
		ConversationID: "whatsapp:14699991654@s.whatsapp.net",
		MediaID:        whatsappmedia.EncodeLocalMediaRef("Media/jenn/voice.opus"),
		SourcePlatform: "whatsapp",
	})
	if err != nil {
		t.Fatalf("DownloadStoredMedia(): %v", err)
	}
	if string(data) != "opus-bytes" {
		t.Fatalf("data = %q, want opus-bytes", string(data))
	}
	if mimeType != "audio/ogg" {
		t.Fatalf("mimeType = %q, want audio/ogg", mimeType)
	}
}

func TestRepairUnavailableMediaPlaceholdersRequestsPeerResendForDirectChat(t *testing.T) {
	dataDir := t.TempDir()
	store, err := db.New(filepath.Join(dataDir, "messages.db"))
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	defer store.Close()

	if err := store.UpsertMessage(&db.Message{
		MessageID:      "whatsapp:3A576038A9DE8BFF3F55",
		ConversationID: "whatsapp:15551234567@s.whatsapp.net",
		SenderName:     "Jenn",
		SenderNumber:   "+15551234567",
		Body:           "[Audio]",
		TimestampMS:    1774986930000,
		Status:         "delivered",
		IsFromMe:       false,
		SourcePlatform: "whatsapp",
		SourceID:       "3A576038A9DE8BFF3F55",
	}); err != nil {
		t.Fatalf("UpsertMessage(): %v", err)
	}

	ownJID := watypes.NewJID("15551230000", watypes.DefaultUserServer)
	bridge := &Bridge{
		store:     store,
		connected: true,
		client: &whatsmeow.Client{
			Store: &wastore.Device{ID: &ownJID},
		},
	}

	originalSend := sendTextMessage
	defer func() { sendTextMessage = originalSend }()

	var capturedTo watypes.JID
	var capturedPeer bool
	var capturedKey *waCommon.MessageKey
	sendTextMessage = func(_ *whatsmeow.Client, _ context.Context, to watypes.JID, message *waE2E.Message, extra ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error) {
		capturedTo = to
		if len(extra) > 0 {
			capturedPeer = extra[len(extra)-1].Peer
		}
		req := message.GetProtocolMessage().GetPeerDataOperationRequestMessage().GetPlaceholderMessageResendRequest()
		if len(req) != 1 {
			t.Fatalf("placeholder resend request count = %d, want 1", len(req))
		}
		capturedKey = req[0].GetMessageKey()
		return whatsmeow.SendResponse{ID: "peer-request"}, nil
	}

	if err := bridge.RepairUnavailableMediaPlaceholders(10); err != nil {
		t.Fatalf("RepairUnavailableMediaPlaceholders(): %v", err)
	}

	if capturedTo.String() != "15551234567@s.whatsapp.net" {
		t.Fatalf("sent to %q, want direct chat jid", capturedTo.String())
	}
	if !capturedPeer {
		t.Fatal("expected placeholder resend request to be sent as a peer message")
	}
	if capturedKey == nil {
		t.Fatal("expected placeholder resend message key")
	}
	if capturedKey.GetID() != "3A576038A9DE8BFF3F55" {
		t.Fatalf("message key id = %q, want source id", capturedKey.GetID())
	}
	if capturedKey.GetRemoteJID() != "15551234567@s.whatsapp.net" {
		t.Fatalf("remote jid = %q, want direct chat jid", capturedKey.GetRemoteJID())
	}
	if capturedKey.GetFromMe() {
		t.Fatal("expected resend request to target an incoming message")
	}
}

func TestRepairUnavailableMediaPlaceholdersDoesNotRepeatRecentRequest(t *testing.T) {
	dataDir := t.TempDir()
	store, err := db.New(filepath.Join(dataDir, "messages.db"))
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	defer store.Close()

	if err := store.UpsertMessage(&db.Message{
		MessageID:      "whatsapp:3A576038A9DE8BFF3F55",
		ConversationID: "whatsapp:15551234567@s.whatsapp.net",
		Body:           "[Audio]",
		TimestampMS:    1774986930000,
		Status:         "delivered",
		SourcePlatform: "whatsapp",
		SourceID:       "3A576038A9DE8BFF3F55",
	}); err != nil {
		t.Fatalf("UpsertMessage(): %v", err)
	}

	ownJID := watypes.NewJID("15551230000", watypes.DefaultUserServer)
	bridge := &Bridge{
		store:     store,
		connected: true,
		client: &whatsmeow.Client{
			Store: &wastore.Device{ID: &ownJID},
		},
	}

	originalSend := sendTextMessage
	defer func() { sendTextMessage = originalSend }()

	calls := 0
	sendTextMessage = func(_ *whatsmeow.Client, _ context.Context, _ watypes.JID, _ *waE2E.Message, _ ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error) {
		calls++
		return whatsmeow.SendResponse{ID: "peer-request"}, nil
	}

	if err := bridge.RepairUnavailableMediaPlaceholders(10); err != nil {
		t.Fatalf("first RepairUnavailableMediaPlaceholders(): %v", err)
	}
	if err := bridge.RepairUnavailableMediaPlaceholders(10); err != nil {
		t.Fatalf("second RepairUnavailableMediaPlaceholders(): %v", err)
	}
	if calls != 1 {
		t.Fatalf("send calls = %d, want 1", calls)
	}
}

func TestRepairUnavailableMediaPlaceholdersRequestsLIDAliasWhenMapped(t *testing.T) {
	dataDir := t.TempDir()
	store, err := db.New(filepath.Join(dataDir, "messages.db"))
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	defer store.Close()

	if err := store.UpsertMessage(&db.Message{
		MessageID:      "whatsapp:3A576038A9DE8BFF3F55",
		ConversationID: "whatsapp:15551234567@s.whatsapp.net",
		Body:           "[Audio]",
		TimestampMS:    1774986930000,
		Status:         "delivered",
		SourcePlatform: "whatsapp",
		SourceID:       "3A576038A9DE8BFF3F55",
	}); err != nil {
		t.Fatalf("UpsertMessage(): %v", err)
	}

	sessionPath := filepath.Join(dataDir, "whatsapp-session.db")
	sessionDB, err := sql.Open("sqlite", sessionStoreDSN(sessionPath))
	if err != nil {
		t.Fatalf("sql.Open(session store): %v", err)
	}
	defer sessionDB.Close()
	if _, err := sessionDB.Exec(`CREATE TABLE whatsmeow_lid_map (lid TEXT PRIMARY KEY, pn TEXT NOT NULL)`); err != nil {
		t.Fatalf("create lid map: %v", err)
	}
	if _, err := sessionDB.Exec(`INSERT INTO whatsmeow_lid_map (lid, pn) VALUES (?, ?)`, "134149377675278", "15551234567"); err != nil {
		t.Fatalf("seed lid map: %v", err)
	}

	ownJID := watypes.NewJID("15551230000", watypes.DefaultUserServer)
	bridge := &Bridge{
		store:       store,
		sessionPath: sessionPath,
		connected:   true,
		client: &whatsmeow.Client{
			Store: &wastore.Device{ID: &ownJID},
		},
	}

	originalSend := sendTextMessage
	defer func() { sendTextMessage = originalSend }()

	var chats []string
	sendTextMessage = func(_ *whatsmeow.Client, _ context.Context, to watypes.JID, _ *waE2E.Message, _ ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error) {
		chats = append(chats, to.String())
		return whatsmeow.SendResponse{ID: "peer-request"}, nil
	}

	if err := bridge.RepairUnavailableMediaPlaceholders(10); err != nil {
		t.Fatalf("RepairUnavailableMediaPlaceholders(): %v", err)
	}

	if len(chats) != 2 {
		t.Fatalf("send calls = %d, want 2", len(chats))
	}
	if chats[0] != "15551234567@s.whatsapp.net" {
		t.Fatalf("first chat = %q, want canonical phone jid", chats[0])
	}
	if chats[1] != "134149377675278@lid" {
		t.Fatalf("second chat = %q, want lid jid", chats[1])
	}
}

func TestHandleMessageDeletesMatchingPlaceholderAliasWhenMediaArrives(t *testing.T) {
	dataDir := t.TempDir()
	store, err := db.New(filepath.Join(dataDir, "messages.db"))
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	defer store.Close()

	if err := store.UpsertMessage(&db.Message{
		MessageID:      "whatsapp:old-placeholder",
		ConversationID: "whatsapp:15551234567@s.whatsapp.net",
		SenderName:     "Jenn",
		SenderNumber:   "+15551234567",
		Body:           "[Audio]",
		TimestampMS:    1774986930000,
		Status:         "delivered",
		SourcePlatform: "whatsapp",
		SourceID:       "old-placeholder",
	}); err != nil {
		t.Fatalf("UpsertMessage(): %v", err)
	}

	bridge := &Bridge{
		store:  store,
		client: &whatsmeow.Client{},
	}

	evt := &waevents.Message{
		Info: watypes.MessageInfo{
			MessageSource: watypes.MessageSource{
				Chat:     watypes.NewJID("15551234567", watypes.DefaultUserServer),
				Sender:   watypes.NewJID("15551234567", watypes.DefaultUserServer),
				IsFromMe: false,
				IsGroup:  false,
			},
			ID:        "new-media-id",
			PushName:  "Jenn",
			Timestamp: time.UnixMilli(1774986930000),
		},
		Message: &waE2E.Message{
			AudioMessage: &waE2E.AudioMessage{
				Mimetype:      strPtr("audio/ogg"),
				URL:           strPtr("https://cdn.example.test/audio"),
				DirectPath:    strPtr("/mms/audio"),
				MediaKey:      []byte{0x01, 0x02},
				FileEncSHA256: []byte{0x03, 0x04},
				FileSHA256:    []byte{0x05, 0x06},
				FileLength:    uint64Ptr(9),
			},
		},
	}

	bridge.handleMessage(evt)

	if msg, err := store.GetMessageByID("whatsapp:old-placeholder"); err != nil {
		t.Fatalf("GetMessageByID(old): %v", err)
	} else if msg != nil {
		t.Fatalf("expected old placeholder to be deleted, got %#v", msg)
	}
	if msg, err := store.GetMessageByID("whatsapp:new-media-id"); err != nil {
		t.Fatalf("GetMessageByID(new): %v", err)
	} else if msg == nil || msg.MediaID == "" {
		t.Fatalf("expected repaired media message, got %#v", msg)
	}
}

func TestHandleMessageMarksMentionsFromWhatsAppContextInfo(t *testing.T) {
	dataDir := t.TempDir()
	store, err := db.New(filepath.Join(dataDir, "messages.db"))
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	defer store.Close()

	ownJID := watypes.NewJID("15551230000", watypes.DefaultUserServer)
	bridge := &Bridge{
		store: store,
		client: &whatsmeow.Client{
			Store: &wastore.Device{
				ID:       &ownJID,
				PushName: "Max",
			},
		},
	}

	bridge.handleMessage(&waevents.Message{
		Info: watypes.MessageInfo{
			MessageSource: watypes.MessageSource{
				Chat:     watypes.NewJID("120363019999999999", watypes.GroupServer),
				Sender:   watypes.NewJID("15551234567", watypes.DefaultUserServer),
				IsFromMe: false,
				IsGroup:  true,
			},
			ID:        "mention-msg",
			PushName:  "Jenn",
			Timestamp: time.UnixMilli(1775000000000),
		},
		Message: &waE2E.Message{
			ExtendedTextMessage: &waE2E.ExtendedTextMessage{
				Text: strPtr("you should take a look"),
				ContextInfo: &waE2E.ContextInfo{
					MentionedJID: []string{ownJID.String()},
				},
			},
		},
	})

	msg, err := store.GetMessageByID("whatsapp:mention-msg")
	if err != nil {
		t.Fatalf("GetMessageByID(): %v", err)
	}
	if msg == nil {
		t.Fatal("expected stored WhatsApp message")
	}
	if !msg.MentionsMe {
		t.Fatal("expected MentionsMe to be true")
	}
}

func TestHandleMessageFormatsMentionedNamesLikeWhatsApp(t *testing.T) {
	store, err := db.New(filepath.Join(t.TempDir(), "messages.db"))
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	defer store.Close()

	if err := store.UpsertConversation(&db.Conversation{
		ConversationID: "whatsapp:120363019999999999@g.us",
		Name:           "Hobbies and investing",
		IsGroup:        true,
		Participants:   `[{"name":"Priya Shah","number":"+261997383958549"},{"name":"Jordan Thibodeau","number":"+15551234567"}]`,
		SourcePlatform: "whatsapp",
	}); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}

	bridge := &Bridge{store: store}
	bridge.handleMessage(&waevents.Message{
		Info: watypes.MessageInfo{
			MessageSource: watypes.MessageSource{
				Chat:     watypes.NewJID("120363019999999999", watypes.GroupServer),
				Sender:   watypes.NewJID("15551234567", watypes.DefaultUserServer),
				IsFromMe: false,
				IsGroup:  true,
			},
			ID:        "mention-name",
			PushName:  "Jordan",
			Timestamp: time.UnixMilli(1775001000000),
		},
		Message: &waE2E.Message{
			ExtendedTextMessage: &waE2E.ExtendedTextMessage{
				Text: strPtr(`I feel like I need to start charging people a monthly fee to see @261997383958549's posts "Thibodeau premium" or something.`),
				ContextInfo: &waE2E.ContextInfo{
					MentionedJID: []string{"261997383958549@s.whatsapp.net"},
				},
			},
		},
	})

	msg, err := store.GetMessageByID("whatsapp:mention-name")
	if err != nil {
		t.Fatalf("GetMessageByID(): %v", err)
	}
	if msg == nil {
		t.Fatal("expected stored WhatsApp message")
	}
	if !strings.Contains(msg.Body, "@~Priya's posts") {
		t.Fatalf("body = %q, want @~Priya mention", msg.Body)
	}
	if strings.Contains(msg.Body, "@261997383958549") {
		t.Fatalf("body = %q, did not expect raw numeric mention", msg.Body)
	}
}

func TestConnectIfPairedStartsAsyncConnect(t *testing.T) {
	ownJID := watypes.NewJID("15551230000", watypes.DefaultUserServer)
	bridge := &Bridge{
		client: &whatsmeow.Client{
			Store: &wastore.Device{ID: &ownJID},
		},
	}

	originalConnect := connectClient
	defer func() { connectClient = originalConnect }()

	started := make(chan struct{}, 1)
	connectClient = func(_ *whatsmeow.Client) error {
		started <- struct{}{}
		return nil
	}

	if err := bridge.ConnectIfPaired(); err != nil {
		t.Fatalf("ConnectIfPaired(): %v", err)
	}

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("expected async WhatsApp connect to start")
	}

	status := bridge.Status()
	if !status.Paired {
		t.Fatal("expected paired status")
	}
	if !status.Connecting {
		t.Fatal("expected connecting status while async connect is in flight")
	}
	if status.Connected {
		t.Fatal("did not expect connected before connected event")
	}
}

func TestConnectIfPairedSkipsDuplicateAttemptWhileConnecting(t *testing.T) {
	ownJID := watypes.NewJID("15551230000", watypes.DefaultUserServer)
	bridge := &Bridge{
		client: &whatsmeow.Client{
			Store: &wastore.Device{ID: &ownJID},
		},
	}

	originalConnect := connectClient
	defer func() { connectClient = originalConnect }()

	started := make(chan struct{}, 2)
	release := make(chan struct{})
	connectClient = func(_ *whatsmeow.Client) error {
		started <- struct{}{}
		<-release
		return nil
	}

	if err := bridge.ConnectIfPaired(); err != nil {
		t.Fatalf("first ConnectIfPaired(): %v", err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("expected first async connect to start")
	}

	if err := bridge.ConnectIfPaired(); err != nil {
		t.Fatalf("second ConnectIfPaired(): %v", err)
	}
	select {
	case <-started:
		t.Fatal("expected duplicate connect attempt to be suppressed")
	case <-time.After(100 * time.Millisecond):
	}

	close(release)
}

func TestConnectRecoversPersistedSessionBeforeStartingPairing(t *testing.T) {
	dataDir := t.TempDir()
	store, err := db.New(filepath.Join(dataDir, "messages.db"))
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	defer store.Close()

	sessionPath := filepath.Join(dataDir, "whatsapp-session.db")
	container, err := sqlstore.New(context.Background(), "sqlite", sessionStoreDSN(sessionPath), waLog.Noop)
	if err != nil {
		t.Fatalf("sqlstore.New(): %v", err)
	}
	device := container.NewDevice()
	ownJID := watypes.NewJID("15551230000", watypes.DefaultUserServer)
	device.ID = &ownJID
	device.PushName = "OpenMessage"
	sessionDB, err := sql.Open("sqlite", sessionStoreDSN(sessionPath))
	if err != nil {
		t.Fatalf("sql.Open(session store): %v", err)
	}
	defer sessionDB.Close()
	if _, err := sessionDB.Exec(`
		INSERT INTO whatsmeow_device (
			jid, registration_id, noise_key, identity_key,
			signed_pre_key, signed_pre_key_id, signed_pre_key_sig,
			adv_key, adv_details, adv_account_sig, adv_account_sig_key, adv_device_sig,
			push_name
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		device.ID.String(),
		device.RegistrationID,
		device.NoiseKey.Priv[:],
		device.IdentityKey.Priv[:],
		device.SignedPreKey.Priv[:],
		device.SignedPreKey.KeyID,
		device.SignedPreKey.Signature[:],
		device.AdvSecretKey,
		[]byte{},
		make([]byte, 64),
		make([]byte, 32),
		make([]byte, 64),
		device.PushName,
	); err != nil {
		t.Fatalf("seed session device: %v", err)
	}
	if err := container.Close(); err != nil {
		t.Fatalf("container.Close(): %v", err)
	}

	bridge, err := New(sessionPath, store, zerolog.Nop(), Callbacks{})
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	defer bridge.Close()

	// Simulate a stale in-memory client that lost its session identity while the
	// persisted store still contains a paired WhatsApp device.
	bridge.mu.Lock()
	bridge.client.Store = &wastore.Device{}
	bridge.mu.Unlock()

	originalConnect := connectClient
	defer func() { connectClient = originalConnect }()

	started := make(chan struct{}, 1)
	connectClient = func(_ *whatsmeow.Client) error {
		started <- struct{}{}
		return nil
	}

	if status := bridge.Status(); !status.Paired {
		t.Fatal("expected status to report paired session from persisted store")
	}

	if err := bridge.Connect(); err != nil {
		t.Fatalf("Connect(): %v", err)
	}

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("expected paired reconnect attempt to start")
	}

	status := bridge.Status()
	if !status.Paired {
		t.Fatal("expected paired status after reconnect recovery")
	}
	if status.Pairing {
		t.Fatal("did not expect QR pairing flow for persisted session")
	}
	if !status.Connecting {
		t.Fatal("expected connecting status while async reconnect is in flight")
	}
}

func TestConsumeQRTimeoutClearsConnecting(t *testing.T) {
	bridge := &Bridge{
		connecting: true,
		pairing:    true,
	}

	ch := make(chan whatsmeow.QRChannelItem, 1)
	ch <- whatsmeow.QRChannelItem{Event: "timeout"}
	close(ch)

	bridge.consumeQR(ch)

	status := bridge.Status()
	if status.Connecting {
		t.Fatal("expected QR timeout to clear connecting state")
	}
	if status.Pairing {
		t.Fatal("expected QR timeout to clear pairing state")
	}
	if status.LastError != "timeout" {
		t.Fatalf("last error = %q, want timeout", status.LastError)
	}
}

func TestBridgeSendMediaBuildsOutgoingWhatsAppMessage(t *testing.T) {
	ownJID := watypes.NewJID("15551230000", watypes.DefaultUserServer)
	bridge := &Bridge{
		connected: true,
		client: &whatsmeow.Client{
			Store: &wastore.Device{
				ID:       &ownJID,
				PushName: "OpenMessage",
			},
		},
	}

	originalSend := sendTextMessage
	originalUpload := uploadMedia
	originalIsConnected := clientIsConnected
	defer func() {
		sendTextMessage = originalSend
		uploadMedia = originalUpload
		clientIsConnected = originalIsConnected
	}()
	clientIsConnected = func(_ *whatsmeow.Client) bool { return true }

	var capturedType whatsmeow.MediaType
	var uploadCtx context.Context
	uploadMedia = func(_ *whatsmeow.Client, ctx context.Context, plaintext []byte, mediaType whatsmeow.MediaType) (whatsmeow.UploadResponse, error) {
		uploadCtx = ctx
		capturedType = mediaType
		if !bytes.Equal(plaintext, []byte("png-bytes")) {
			t.Fatalf("upload bytes = %q, want png-bytes", string(plaintext))
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

	var capturedTo watypes.JID
	var capturedMsg *waE2E.Message
	var capturedID string
	var sendCtx context.Context
	sendTextMessage = func(_ *whatsmeow.Client, ctx context.Context, to watypes.JID, message *waE2E.Message, extra ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error) {
		sendCtx = ctx
		capturedTo = to
		capturedMsg = message
		if len(extra) > 0 {
			capturedID = string(extra[0].ID)
		}
		return whatsmeow.SendResponse{
			ID:        watypes.MessageID(capturedID),
			Timestamp: time.UnixMilli(1700000000456),
		}, nil
	}

	msg, err := bridge.SendMedia("whatsapp:15551234567@s.whatsapp.net", []byte("png-bytes"), "photo.png", "image/png", "check this out", "whatsapp:reply-123")
	if err != nil {
		t.Fatalf("SendMedia(): %v", err)
	}

	if capturedType != whatsmeow.MediaImage {
		t.Fatalf("upload media type = %v, want image", capturedType)
	}
	if capturedTo.String() != "15551234567@s.whatsapp.net" {
		t.Fatalf("sent to %q, want target jid", capturedTo.String())
	}
	image := capturedMsg.GetImageMessage()
	if image == nil {
		t.Fatal("expected WhatsApp image message")
	}
	if image.GetCaption() != "check this out" {
		t.Fatalf("caption = %q, want check this out", image.GetCaption())
	}
	if image.GetContextInfo().GetStanzaID() != "reply-123" {
		t.Fatalf("reply stanza = %q, want reply-123", image.GetContextInfo().GetStanzaID())
	}
	if image.GetMimetype() != "image/png" {
		t.Fatalf("mime = %q, want image/png", image.GetMimetype())
	}
	if image.GetDirectPath() != "/mms/image" {
		t.Fatalf("direct path = %q, want /mms/image", image.GetDirectPath())
	}
	if capturedID == "" {
		t.Fatal("expected generated WhatsApp media message id")
	}
	if uploadCtx == nil || sendCtx == nil {
		t.Fatal("expected upload and send contexts to be captured")
	}
	if uploadCtx == sendCtx {
		t.Fatal("expected upload and send to use separate contexts")
	}
	if msg.MessageID != "whatsapp:"+capturedID {
		t.Fatalf("message id = %q, want whatsapp:%s", msg.MessageID, capturedID)
	}
	if msg.MimeType != "image/png" {
		t.Fatalf("stored mime = %q, want image/png", msg.MimeType)
	}
	if msg.Body != "check this out" {
		t.Fatalf("stored body = %q, want check this out", msg.Body)
	}
	if msg.ReplyToID != "whatsapp:reply-123" {
		t.Fatalf("stored reply_to_id = %q, want whatsapp:reply-123", msg.ReplyToID)
	}
	if msg.DecryptionKey != "0102" {
		t.Fatalf("stored media key = %q, want 0102", msg.DecryptionKey)
	}
	ref, err := decodeStoredMediaRef(msg.MediaID)
	if err != nil {
		t.Fatalf("decodeStoredMediaRef(): %v", err)
	}
	if ref.DirectPath != "/mms/image" {
		t.Fatalf("stored direct path = %q, want /mms/image", ref.DirectPath)
	}
	if ref.FileSHA256 != "0506" {
		t.Fatalf("stored file hash = %q, want 0506", ref.FileSHA256)
	}
	if ref.FileEncSHA256 != "0304" {
		t.Fatalf("stored enc hash = %q, want 0304", ref.FileEncSHA256)
	}
	if ref.FileLength != 9 {
		t.Fatalf("stored file length = %d, want 9", ref.FileLength)
	}
}

func TestBridgeSendMediaBuildsOutgoingWhatsAppAudioMessage(t *testing.T) {
	ownJID := watypes.NewJID("15551230000", watypes.DefaultUserServer)
	bridge := &Bridge{
		connected: true,
		client: &whatsmeow.Client{
			Store: &wastore.Device{
				ID:       &ownJID,
				PushName: "OpenMessage",
			},
		},
	}

	originalSend := sendTextMessage
	originalUpload := uploadMedia
	originalIsConnected := clientIsConnected
	defer func() {
		sendTextMessage = originalSend
		uploadMedia = originalUpload
		clientIsConnected = originalIsConnected
	}()
	clientIsConnected = func(_ *whatsmeow.Client) bool { return true }

	var capturedType whatsmeow.MediaType
	uploadMedia = func(_ *whatsmeow.Client, _ context.Context, plaintext []byte, mediaType whatsmeow.MediaType) (whatsmeow.UploadResponse, error) {
		capturedType = mediaType
		return whatsmeow.UploadResponse{
			URL:           "https://cdn.example.test/audio",
			DirectPath:    "/mms/audio",
			MediaKey:      []byte{0x0a, 0x0b},
			FileEncSHA256: []byte{0x0c, 0x0d},
			FileSHA256:    []byte{0x0e, 0x0f},
			FileLength:    11,
		}, nil
	}

	var capturedMsg *waE2E.Message
	sendTextMessage = func(_ *whatsmeow.Client, _ context.Context, _ watypes.JID, message *waE2E.Message, extra ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error) {
		capturedMsg = message
		return whatsmeow.SendResponse{
			ID:        watypes.MessageID("audio-msg"),
			Timestamp: time.UnixMilli(1700000000789),
		}, nil
	}

	msg, err := bridge.SendMedia("whatsapp:15551234567@s.whatsapp.net", []byte("ogg-bytes"), "voice-note.ogg", "audio/ogg", "", "")
	if err != nil {
		t.Fatalf("SendMedia(audio): %v", err)
	}

	if capturedType != whatsmeow.MediaAudio {
		t.Fatalf("upload media type = %v, want audio", capturedType)
	}
	audio := capturedMsg.GetAudioMessage()
	if audio == nil {
		t.Fatal("expected WhatsApp audio message")
	}
	if audio.GetMimetype() != "audio/ogg" {
		t.Fatalf("mime = %q, want audio/ogg", audio.GetMimetype())
	}
	if audio.GetDirectPath() != "/mms/audio" {
		t.Fatalf("direct path = %q, want /mms/audio", audio.GetDirectPath())
	}
	if msg.MimeType != "audio/ogg" {
		t.Fatalf("stored mime = %q, want audio/ogg", msg.MimeType)
	}
}

func TestBridgeDownloadStoredMediaUsesEncodedReference(t *testing.T) {
	bridge := &Bridge{
		connected: true,
		client:    &whatsmeow.Client{},
	}

	originalDownload := downloadMediaWithPath
	originalIsConnected := clientIsConnected
	defer func() {
		downloadMediaWithPath = originalDownload
		clientIsConnected = originalIsConnected
	}()
	clientIsConnected = func(_ *whatsmeow.Client) bool { return true }

	downloadMediaWithPath = func(_ *whatsmeow.Client, _ context.Context, directPath string, encFileHash, fileHash, mediaKey []byte, fileLength int, mediaType whatsmeow.MediaType, _ string) ([]byte, error) {
		if directPath != "/mms/image" {
			t.Fatalf("direct path = %q, want /mms/image", directPath)
		}
		if string(encFileHash) != string([]byte{0x03, 0x04}) {
			t.Fatalf("enc hash = %x, want 0304", encFileHash)
		}
		if string(fileHash) != string([]byte{0x05, 0x06}) {
			t.Fatalf("file hash = %x, want 0506", fileHash)
		}
		if string(mediaKey) != string([]byte{0x01, 0x02}) {
			t.Fatalf("media key = %x, want 0102", mediaKey)
		}
		if fileLength != 9 {
			t.Fatalf("file length = %d, want 9", fileLength)
		}
		if mediaType != whatsmeow.MediaImage {
			t.Fatalf("media type = %v, want image", mediaType)
		}
		return []byte("image-bytes"), nil
	}

	msg := &db.Message{
		MessageID:      "whatsapp:m1",
		ConversationID: "whatsapp:15551234567@s.whatsapp.net",
		MediaID: encodeStoredMediaRef(storedMediaRef{
			URL:           "https://cdn.example.test/image",
			DirectPath:    "/mms/image",
			FileSHA256:    "0506",
			FileEncSHA256: "0304",
			FileLength:    9,
		}),
		MimeType:      "image/png",
		DecryptionKey: "0102",
	}

	data, mime, err := bridge.DownloadStoredMedia(msg)
	if err != nil {
		t.Fatalf("DownloadStoredMedia(): %v", err)
	}
	if string(data) != "image-bytes" {
		t.Fatalf("downloaded data = %q, want image-bytes", string(data))
	}
	if mime != "image/png" {
		t.Fatalf("mime = %q, want image/png", mime)
	}
}

func TestHandleMessageCanonicalizesLIDToPhoneNumberThread(t *testing.T) {
	store, err := db.New(filepath.Join(t.TempDir(), "messages.db"))
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	defer store.Close()

	lid := watypes.NewJID("134149377675278", watypes.HiddenUserServer)
	pn := watypes.NewJID("14699991654", watypes.DefaultUserServer)
	bridge := &Bridge{
		store: store,
		client: &whatsmeow.Client{
			Store: &wastore.Device{
				LIDs: &testLIDStore{
					NoopStore: wastore.NoopStore{},
					lidToPN: map[string]watypes.JID{
						lid.String(): pn,
					},
				},
			},
		},
	}
	if err := store.UpsertConversation(&db.Conversation{
		ConversationID: "whatsapp:134149377675278@lid",
		Name:           "Max Ghenis",
		Participants:   `[{"name":"Jenn","number":"+134149377675278"}]`,
		LastMessageTS:  1699999999000,
		UnreadCount:    1,
		SourcePlatform: "whatsapp",
	}); err != nil {
		t.Fatalf("seed raw conversation: %v", err)
	}
	if err := store.UpsertMessage(&db.Message{
		MessageID:      "whatsapp:old-lid-msg",
		ConversationID: "whatsapp:134149377675278@lid",
		SenderName:     "Jenn",
		SenderNumber:   "+134149377675278",
		Body:           "older import",
		TimestampMS:    1699999999000,
		SourcePlatform: "whatsapp",
		SourceID:       "old-lid-msg",
	}); err != nil {
		t.Fatalf("seed raw message: %v", err)
	}

	bridge.handleMessage(&waevents.Message{
		Info: watypes.MessageInfo{
			MessageSource: watypes.MessageSource{
				Chat:   lid,
				Sender: lid,
			},
			ID:        "msg-1",
			Timestamp: time.UnixMilli(1700000000000),
		},
		Message: &waE2E.Message{
			Conversation: strPtr("hello from lid"),
		},
	})

	msgs, err := store.GetMessagesByConversation("whatsapp:14699991654@s.whatsapp.net", 10)
	if err != nil {
		t.Fatalf("GetMessagesByConversation(): %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2", len(msgs))
	}
	if msgs[0].SenderNumber != "+14699991654" {
		t.Fatalf("sender number = %q, want +14699991654", msgs[0].SenderNumber)
	}
	convo, err := store.GetConversation("whatsapp:14699991654@s.whatsapp.net")
	if err != nil {
		t.Fatalf("GetConversation(): %v", err)
	}
	if convo == nil {
		t.Fatal("expected canonical WhatsApp conversation to be stored")
	}
	if convo.Participants == "" || !strings.Contains(convo.Participants, "+14699991654") {
		t.Fatalf("participants = %q, want canonical phone number", convo.Participants)
	}
	if _, err := store.GetConversation("whatsapp:134149377675278@lid"); err == nil {
		t.Fatal("expected raw lid conversation to be merged away")
	}
}

func TestHandleMessageStoresWrappedWhatsAppMedia(t *testing.T) {
	store, err := db.New(filepath.Join(t.TempDir(), "messages.db"))
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	defer store.Close()

	bridge := &Bridge{store: store}
	bridge.handleMessage(&waevents.Message{
		Info: watypes.MessageInfo{
			MessageSource: watypes.MessageSource{
				Chat:   watypes.NewJID("15551234567", watypes.DefaultUserServer),
				Sender: watypes.NewJID("15551234567", watypes.DefaultUserServer),
			},
			ID:        "wrapped-media",
			Timestamp: time.UnixMilli(1700000001234),
		},
		Message: &waE2E.Message{
			DeviceSentMessage: &waE2E.DeviceSentMessage{
				Message: &waE2E.Message{
					EphemeralMessage: &waE2E.FutureProofMessage{
						Message: &waE2E.Message{
							ImageMessage: &waE2E.ImageMessage{
								Caption:       strPtr("hi"),
								Mimetype:      strPtr("image/jpeg"),
								URL:           strPtr("https://cdn.example.test/image"),
								DirectPath:    strPtr("/mms/image"),
								MediaKey:      []byte{0x01, 0x02},
								FileSHA256:    []byte{0x03, 0x04},
								FileEncSHA256: []byte{0x05, 0x06},
								FileLength:    uint64Ptr(7),
							},
						},
					},
				},
			},
		},
	})

	msg, err := store.GetMessageByID("whatsapp:wrapped-media")
	if err != nil {
		t.Fatalf("GetMessageByID(): %v", err)
	}
	if msg == nil {
		t.Fatal("expected stored message")
	}
	if msg.MimeType != "image/jpeg" {
		t.Fatalf("mime_type = %q, want image/jpeg", msg.MimeType)
	}
	if msg.MediaID == "" {
		t.Fatal("expected media_id to be stored")
	}
	if msg.DecryptionKey != "0102" {
		t.Fatalf("decryption_key = %q, want 0102", msg.DecryptionKey)
	}
}

func TestHandleMessageStoresWrappedWhatsAppAudio(t *testing.T) {
	store, err := db.New(filepath.Join(t.TempDir(), "messages.db"))
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	defer store.Close()

	bridge := &Bridge{store: store}
	bridge.handleMessage(&waevents.Message{
		Info: watypes.MessageInfo{
			MessageSource: watypes.MessageSource{
				Chat:   watypes.NewJID("15551234567", watypes.DefaultUserServer),
				Sender: watypes.NewJID("15551234567", watypes.DefaultUserServer),
			},
			ID:        "wrapped-audio",
			Timestamp: time.UnixMilli(1700000002234),
		},
		Message: &waE2E.Message{
			EphemeralMessage: &waE2E.FutureProofMessage{
				Message: &waE2E.Message{
					AudioMessage: &waE2E.AudioMessage{
						Mimetype:      strPtr("audio/ogg"),
						URL:           strPtr("https://cdn.example.test/audio"),
						DirectPath:    strPtr("/mms/audio"),
						MediaKey:      []byte{0x01, 0x02},
						FileSHA256:    []byte{0x03, 0x04},
						FileEncSHA256: []byte{0x05, 0x06},
						FileLength:    uint64Ptr(7),
						PTT:           boolPtr(true),
					},
				},
			},
		},
	})

	msg, err := store.GetMessageByID("whatsapp:wrapped-audio")
	if err != nil {
		t.Fatalf("GetMessageByID(): %v", err)
	}
	if msg == nil {
		t.Fatal("expected stored audio message")
	}
	if msg.MimeType != "audio/ogg" {
		t.Fatalf("mime_type = %q, want audio/ogg", msg.MimeType)
	}
	if msg.MediaID == "" {
		t.Fatal("expected media_id to be stored")
	}
	if msg.DecryptionKey != "0102" {
		t.Fatalf("decryption_key = %q, want 0102", msg.DecryptionKey)
	}
}

func TestHandleMessageStoresWhatsAppStickerMedia(t *testing.T) {
	store, err := db.New(filepath.Join(t.TempDir(), "messages.db"))
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	defer store.Close()

	bridge := &Bridge{store: store}
	bridge.handleMessage(&waevents.Message{
		Info: watypes.MessageInfo{
			MessageSource: watypes.MessageSource{
				Chat:   watypes.NewJID("15551234567", watypes.DefaultUserServer),
				Sender: watypes.NewJID("15551234567", watypes.DefaultUserServer),
			},
			ID:        "sticker-msg",
			Timestamp: time.UnixMilli(1700000003234),
		},
		Message: &waE2E.Message{
			StickerMessage: &waE2E.StickerMessage{
				Mimetype:      strPtr("image/webp"),
				URL:           strPtr("https://cdn.example.test/sticker"),
				DirectPath:    strPtr("/mms/sticker"),
				MediaKey:      []byte{0x01, 0x02},
				FileSHA256:    []byte{0x03, 0x04},
				FileEncSHA256: []byte{0x05, 0x06},
				FileLength:    uint64Ptr(7),
			},
		},
	})

	msg, err := store.GetMessageByID("whatsapp:sticker-msg")
	if err != nil {
		t.Fatalf("GetMessageByID(): %v", err)
	}
	if msg == nil {
		t.Fatal("expected stored sticker message")
	}
	if msg.Body != "[Sticker]" {
		t.Fatalf("body = %q, want [Sticker]", msg.Body)
	}
	if msg.MimeType != "image/webp" {
		t.Fatalf("mime_type = %q, want image/webp", msg.MimeType)
	}
	if msg.MediaID == "" {
		t.Fatal("expected media_id to be stored")
	}
	if msg.DecryptionKey != "0102" {
		t.Fatalf("decryption_key = %q, want 0102", msg.DecryptionKey)
	}
}

func TestHandleMessageSkipsUnsupportedWhatsAppPlaceholders(t *testing.T) {
	store, err := db.New(filepath.Join(t.TempDir(), "messages.db"))
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	defer store.Close()

	bridge := &Bridge{store: store}
	bridge.handleMessage(&waevents.Message{
		Info: watypes.MessageInfo{
			MessageSource: watypes.MessageSource{
				Chat:   watypes.NewJID("15551234567", watypes.DefaultUserServer),
				Sender: watypes.NewJID("15551234567", watypes.DefaultUserServer),
			},
			ID:        "unsupported-msg",
			Timestamp: time.UnixMilli(1700000004234),
		},
		Message: &waE2E.Message{
			SendPaymentMessage: &waE2E.SendPaymentMessage{},
		},
	})

	msg, err := store.GetMessageByID("whatsapp:unsupported-msg")
	if err != nil {
		t.Fatalf("GetMessageByID(): %v", err)
	}
	if msg != nil {
		t.Fatalf("expected unsupported placeholder to be skipped, got %+v", msg)
	}
}

func TestHandleMessageAppliesWhatsAppReactionsToTargetMessage(t *testing.T) {
	store, err := db.New(filepath.Join(t.TempDir(), "messages.db"))
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	defer store.Close()

	if err := store.UpsertMessage(&db.Message{
		MessageID:      "whatsapp:target-msg",
		ConversationID: "whatsapp:15551234567@s.whatsapp.net",
		SenderName:     "Jenn",
		SenderNumber:   "+15551234567",
		Body:           "hello",
		TimestampMS:    1700000000000,
		SourcePlatform: "whatsapp",
		SourceID:       "target-msg",
	}); err != nil {
		t.Fatalf("seed target message: %v", err)
	}

	bridge := &Bridge{store: store}
	reactionEvent := func(eventID, emoji string) *waevents.Message {
		return &waevents.Message{
			Info: watypes.MessageInfo{
				MessageSource: watypes.MessageSource{
					Chat:   watypes.NewJID("15551234567", watypes.DefaultUserServer),
					Sender: watypes.NewJID("15551234567", watypes.DefaultUserServer),
				},
				ID:        watypes.MessageID(eventID),
				Timestamp: time.UnixMilli(1700000001000),
			},
			Message: &waE2E.Message{
				ReactionMessage: &waE2E.ReactionMessage{
					Key:  &waCommon.MessageKey{ID: strPtr("target-msg")},
					Text: strPtr(emoji),
				},
			},
		}
	}

	bridge.handleMessage(reactionEvent("reaction-1", "😂"))

	msg, err := store.GetMessageByID("whatsapp:target-msg")
	if err != nil {
		t.Fatalf("GetMessageByID(target): %v", err)
	}
	if msg == nil {
		t.Fatal("expected target message to exist")
	}

	var reactions []storedReaction
	if err := json.Unmarshal([]byte(msg.Reactions), &reactions); err != nil {
		t.Fatalf("json.Unmarshal(reactions): %v", err)
	}
	if len(reactions) != 1 || reactions[0].Emoji != "😂" || reactions[0].Count != 1 {
		t.Fatalf("unexpected reactions after add: %+v", reactions)
	}

	placeholder, err := store.GetMessageByID("whatsapp:reaction-1")
	if err != nil {
		t.Fatalf("GetMessageByID(placeholder): %v", err)
	}
	if placeholder != nil {
		t.Fatalf("expected no standalone reaction placeholder, got %+v", placeholder)
	}

	bridge.handleMessage(reactionEvent("reaction-2", "❤️"))

	msg, err = store.GetMessageByID("whatsapp:target-msg")
	if err != nil {
		t.Fatalf("GetMessageByID(target after change): %v", err)
	}
	if err := json.Unmarshal([]byte(msg.Reactions), &reactions); err != nil {
		t.Fatalf("json.Unmarshal(reactions after change): %v", err)
	}
	if len(reactions) != 1 || reactions[0].Emoji != "❤️" || reactions[0].Count != 1 {
		t.Fatalf("unexpected reactions after change: %+v", reactions)
	}

	bridge.handleMessage(reactionEvent("reaction-3", ""))

	msg, err = store.GetMessageByID("whatsapp:target-msg")
	if err != nil {
		t.Fatalf("GetMessageByID(target after remove): %v", err)
	}
	if strings.TrimSpace(msg.Reactions) != "" {
		t.Fatalf("expected reactions to be cleared, got %q", msg.Reactions)
	}
}

func TestHandleProtocolMessageEditsAndRevokes(t *testing.T) {
	store, err := db.New(filepath.Join(t.TempDir(), "messages.db"))
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	defer store.Close()

	seed := func(id, body string) {
		if err := store.UpsertMessage(&db.Message{
			MessageID:      "whatsapp:" + id,
			ConversationID: "whatsapp:15551234567@s.whatsapp.net",
			SenderName:     "Jenn",
			SenderNumber:   "+15551234567",
			Body:           body,
			TimestampMS:    1700000000000,
			MediaID:        "media-keep",
			SourcePlatform: "whatsapp",
			SourceID:       id,
		}); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}

	bridge := &Bridge{store: store}
	protocolEvent := func(eventID string, pm *waE2E.ProtocolMessage) *waevents.Message {
		return &waevents.Message{
			Info: watypes.MessageInfo{
				MessageSource: watypes.MessageSource{
					Chat:   watypes.NewJID("15551234567", watypes.DefaultUserServer),
					Sender: watypes.NewJID("15551234567", watypes.DefaultUserServer),
				},
				ID:        watypes.MessageID(eventID),
				Timestamp: time.UnixMilli(1700000002000),
			},
			Message: &waE2E.Message{ProtocolMessage: pm},
		}
	}

	t.Run("edit updates body, preserves media", func(t *testing.T) {
		seed("edit-target", "original text")
		bridge.handleMessage(protocolEvent("edit-evt", &waE2E.ProtocolMessage{
			Type: waE2E.ProtocolMessage_MESSAGE_EDIT.Enum(),
			Key:  &waCommon.MessageKey{ID: strPtr("edit-target")},
			EditedMessage: &waE2E.Message{
				Conversation: strPtr("edited text"),
			},
		}))
		got, err := store.GetMessageByID("whatsapp:edit-target")
		if err != nil || got == nil {
			t.Fatalf("get edited: %v / %v", got, err)
		}
		if got.Body != "edited text" {
			t.Errorf("edit not applied: got %q, want %q", got.Body, "edited text")
		}
		if got.MediaID != "media-keep" {
			t.Errorf("edit wiped media: got %q", got.MediaID)
		}
		// The edit envelope itself must not create a standalone row.
		if extra, _ := store.GetMessageByID("whatsapp:edit-evt"); extra != nil {
			t.Errorf("edit envelope leaked a row: %+v", extra)
		}
	})

	t.Run("revoke deletes the target", func(t *testing.T) {
		seed("revoke-target", "delete me")
		bridge.handleMessage(protocolEvent("revoke-evt", &waE2E.ProtocolMessage{
			Type: waE2E.ProtocolMessage_REVOKE.Enum(),
			Key:  &waCommon.MessageKey{ID: strPtr("revoke-target")},
		}))
		got, err := store.GetMessageByID("whatsapp:revoke-target")
		if err != nil {
			t.Fatalf("get revoked: %v", err)
		}
		if got != nil {
			t.Errorf("revoke did not delete message: %+v", got)
		}
	})

	t.Run("typeless protocol message without key is ignored, not treated as revoke", func(t *testing.T) {
		seed("safe-target", "keep me")
		// A ProtocolMessage with the zero-value type (REVOKE) but no key must
		// not delete anything.
		bridge.handleMessage(protocolEvent("noop-evt", &waE2E.ProtocolMessage{}))
		got, err := store.GetMessageByID("whatsapp:safe-target")
		if err != nil || got == nil {
			t.Fatalf("typeless protocol message should be a no-op, got %v / %v", got, err)
		}
	})
}

func TestHandleReceiptUpdatesOutgoingWhatsAppStatus(t *testing.T) {
	store, err := db.New(filepath.Join(t.TempDir(), "messages.db"))
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	defer store.Close()

	if err := store.UpsertMessage(&db.Message{
		MessageID:      "whatsapp:receipt-msg",
		ConversationID: "whatsapp:15551234567@s.whatsapp.net",
		Body:           "pending",
		TimestampMS:    1700000000000,
		Status:         "OUTGOING_SENDING",
		IsFromMe:       true,
		SourcePlatform: "whatsapp",
		SourceID:       "receipt-msg",
	}); err != nil {
		t.Fatalf("seed message: %v", err)
	}

	var changed []string
	conversationRefreshes := 0
	bridge := &Bridge{
		store: store,
		callbacks: Callbacks{
			OnMessagesChange: func(conversationID string) {
				changed = append(changed, conversationID)
			},
			OnConversationsChange: func() {
				conversationRefreshes++
			},
		},
	}

	bridge.handleReceipt(&waevents.Receipt{
		MessageIDs: []watypes.MessageID{"receipt-msg"},
		Type:       watypes.ReceiptTypeDelivered,
	})

	msg, err := store.GetMessageByID("whatsapp:receipt-msg")
	if err != nil {
		t.Fatalf("GetMessageByID(): %v", err)
	}
	if msg == nil {
		t.Fatal("expected message to exist")
	}
	if msg.Status != "delivered" {
		t.Fatalf("status = %q, want delivered", msg.Status)
	}
	if len(changed) != 1 || changed[0] != "whatsapp:15551234567@s.whatsapp.net" {
		t.Fatalf("changed conversations = %v", changed)
	}
	if conversationRefreshes != 1 {
		t.Fatalf("conversation refreshes = %d, want 1", conversationRefreshes)
	}

	bridge.handleReceipt(&waevents.Receipt{
		MessageIDs: []watypes.MessageID{"receipt-msg"},
		Type:       watypes.ReceiptTypeRead,
	})

	msg, err = store.GetMessageByID("whatsapp:receipt-msg")
	if err != nil {
		t.Fatalf("GetMessageByID() after read: %v", err)
	}
	if msg.Status != "read" {
		t.Fatalf("status after read = %q, want read", msg.Status)
	}
}

func TestHandleReceiptDoesNotDowngradeWhatsAppStatus(t *testing.T) {
	store, err := db.New(filepath.Join(t.TempDir(), "messages.db"))
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	defer store.Close()

	if err := store.UpsertMessage(&db.Message{
		MessageID:      "whatsapp:receipt-msg",
		ConversationID: "whatsapp:15551234567@s.whatsapp.net",
		Body:           "read already",
		TimestampMS:    1700000000000,
		Status:         "read",
		IsFromMe:       true,
		SourcePlatform: "whatsapp",
		SourceID:       "receipt-msg",
	}); err != nil {
		t.Fatalf("seed message: %v", err)
	}

	bridge := &Bridge{store: store}
	bridge.handleReceipt(&waevents.Receipt{
		MessageIDs: []watypes.MessageID{"receipt-msg"},
		Type:       watypes.ReceiptTypeDelivered,
	})

	msg, err := store.GetMessageByID("whatsapp:receipt-msg")
	if err != nil {
		t.Fatalf("GetMessageByID(): %v", err)
	}
	if msg.Status != "read" {
		t.Fatalf("status = %q, want read", msg.Status)
	}
}

func TestCanonicalJIDFallsBackToSessionStore(t *testing.T) {
	dataDir := t.TempDir()
	sessionPath := filepath.Join(dataDir, "whatsapp-session.db")

	container, err := sqlstore.New(context.Background(), "sqlite", sessionStoreDSN(sessionPath), waLog.Noop)
	if err != nil {
		t.Fatalf("sqlstore.New(): %v", err)
	}
	if _, err := container.GetFirstDevice(context.Background()); err != nil {
		t.Fatalf("GetFirstDevice(): %v", err)
	}

	lid := watypes.NewJID("134149377675278", watypes.HiddenUserServer)
	pn := watypes.NewJID("14699991654", watypes.DefaultUserServer)
	sessionDB, err := sql.Open("sqlite", sessionStoreDSN(sessionPath))
	if err != nil {
		t.Fatalf("sql.Open(session store): %v", err)
	}
	defer sessionDB.Close()
	if _, err := sessionDB.Exec(`INSERT INTO whatsmeow_lid_map (lid, pn) VALUES (?, ?)`, lid.User, pn.User); err != nil {
		t.Fatalf("insert lid mapping: %v", err)
	}
	if err := container.Close(); err != nil {
		t.Fatalf("container.Close(): %v", err)
	}

	bridge := &Bridge{
		sessionPath: sessionPath,
		client: &whatsmeow.Client{
			Store: &wastore.Device{
				LIDs: &wastore.NoopStore{},
			},
		},
	}

	got := bridge.canonicalJID(lid)
	if got.String() != pn.String() {
		t.Fatalf("canonical jid = %s, want %s", got.String(), pn.String())
	}
}

type testLIDStore struct {
	wastore.NoopStore
	lidToPN map[string]watypes.JID
}

func (s *testLIDStore) GetPNForLID(_ context.Context, lid watypes.JID) (watypes.JID, error) {
	if alt, ok := s.lidToPN[lid.String()]; ok {
		return alt, nil
	}
	return watypes.EmptyJID, nil
}

func strPtr(value string) *string {
	return &value
}

func uint64Ptr(value uint64) *uint64 {
	return &value
}

func boolPtr(value bool) *bool {
	return &value
}
