package app

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"

	"github.com/maxghenis/openmessage/internal/db"
)

func testSendApp(t *testing.T) *App {
	t.Helper()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("create db: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return &App{
		Store:  store,
		Logger: zerolog.Nop(),
	}
}

func TestSendTextToConversationSMSPersistsOutgoingMessage(t *testing.T) {
	a := testSendApp(t)
	if err := a.Store.UpsertConversation(&db.Conversation{
		ConversationID: "sms-conv-1",
		Name:           "Taylor",
		LastMessageTS:  time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}

	originalGetGoogleConversation := getGoogleConversationForSend
	originalSendGoogleTextPayload := sendGoogleTextPayload
	getGoogleConversationForSend = func(_ *App, conversationID string) (*gmproto.Conversation, error) {
		if conversationID != "sms-conv-1" {
			t.Fatalf("conversationID = %q, want sms-conv-1", conversationID)
		}
		return &gmproto.Conversation{ConversationID: conversationID}, nil
	}
	sendGoogleTextPayload = func(_ *App, payload *gmproto.SendMessageRequest) (*gmproto.SendMessageResponse, error) {
		if payload.GetConversationID() != "sms-conv-1" {
			t.Fatalf("conversationID = %q, want sms-conv-1", payload.GetConversationID())
		}
		if payload.GetMessagePayload().GetMessageInfo()[0].GetMessageContent().GetContent() != "hello sms" {
			t.Fatalf("unexpected message payload: %#v", payload.GetMessagePayload())
		}
		return &gmproto.SendMessageResponse{Status: gmproto.SendMessageResponse_SUCCESS}, nil
	}
	t.Cleanup(func() {
		getGoogleConversationForSend = originalGetGoogleConversation
		sendGoogleTextPayload = originalSendGoogleTextPayload
	})

	conv, msg, err := a.SendTextToConversation("sms-conv-1", "hello sms")
	if err != nil {
		t.Fatalf("SendTextToConversation(): %v", err)
	}
	if conv == nil || conv.ConversationID != "sms-conv-1" {
		t.Fatalf("unexpected conversation: %#v", conv)
	}
	if msg == nil || msg.Body != "hello sms" {
		t.Fatalf("unexpected message: %#v", msg)
	}
	stored, err := a.Store.GetMessageByID(msg.MessageID)
	if err != nil {
		t.Fatalf("GetMessageByID(): %v", err)
	}
	if stored == nil || stored.Body != "hello sms" {
		t.Fatalf("expected persisted outgoing sms message, got %#v", stored)
	}
}

func TestSendTextToConversationSMSRejectedUsesPhoneReachability(t *testing.T) {
	a := testSendApp(t)
	a.SessionPath = filepath.Join(t.TempDir(), "session.json")
	if err := os.WriteFile(a.SessionPath, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	a.RecordGooglePhoneResponding(false)
	if err := a.Store.UpsertConversation(&db.Conversation{
		ConversationID: "sms-conv-1",
		Name:           "Taylor",
		LastMessageTS:  time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}

	originalGetGoogleConversation := getGoogleConversationForSend
	originalSendGoogleTextPayload := sendGoogleTextPayload
	getGoogleConversationForSend = func(_ *App, conversationID string) (*gmproto.Conversation, error) {
		return &gmproto.Conversation{ConversationID: conversationID}, nil
	}
	sendGoogleTextPayload = func(_ *App, payload *gmproto.SendMessageRequest) (*gmproto.SendMessageResponse, error) {
		return &gmproto.SendMessageResponse{Status: gmproto.SendMessageResponse_UNKNOWN}, nil
	}
	t.Cleanup(func() {
		getGoogleConversationForSend = originalGetGoogleConversation
		sendGoogleTextPayload = originalSendGoogleTextPayload
	})

	_, _, err := a.SendTextToConversation("sms-conv-1", "hello sms")
	if err == nil {
		t.Fatal("expected send rejection")
	}
	if !strings.Contains(err.Error(), "phone isn't responding") {
		t.Fatalf("expected phone reachability error, got %v", err)
	}
	if a.googleSendFailures.Load() != 0 {
		t.Fatalf("googleSendFailures = %d, want 0", a.googleSendFailures.Load())
	}
	if a.GoogleStatus().NeedsRepair {
		t.Fatal("phone-offline send rejection must not mark Google session for repair")
	}
}

func TestSendTextToConversationLookupAuthErrorMarksDisconnected(t *testing.T) {
	a := testSendApp(t)
	a.Connected.Store(true)
	a.SessionPath = filepath.Join(t.TempDir(), "session.json")
	if err := os.WriteFile(a.SessionPath, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.Store.UpsertConversation(&db.Conversation{
		ConversationID: "sms-conv-auth",
		Name:           "Taylor",
		LastMessageTS:  time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}

	originalGetGoogleConversation := getGoogleConversationForSend
	getGoogleConversationForSend = func(_ *App, conversationID string) (*gmproto.Conversation, error) {
		return nil, errors.New("HTTP 401: invalid authentication credentials")
	}
	t.Cleanup(func() {
		getGoogleConversationForSend = originalGetGoogleConversation
	})

	_, _, err := a.SendTextToConversation("sms-conv-auth", "hello sms")
	if err == nil {
		t.Fatal("expected lookup error")
	}
	if a.Connected.Load() {
		t.Fatal("auth-invalid lookup error should mark Google disconnected")
	}
	if a.GoogleStatus().NeedsRepair {
		t.Fatal("auth-invalid lookup error should use cookie-refresh reconnect, not repair")
	}
	if got := a.GoogleStatus().LastError; got != googleAuthExpiredStatusMessage {
		t.Fatalf("last error = %q, want %q", got, googleAuthExpiredStatusMessage)
	}
}

func TestSendTextToConversationUnsupportedPlatform(t *testing.T) {
	a := testSendApp(t)
	if err := a.Store.UpsertConversation(&db.Conversation{
		ConversationID: "gchat:1",
		Name:           "Imported Chat",
		LastMessageTS:  time.Now().UnixMilli(),
		SourcePlatform: "gchat",
	}); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}

	_, _, err := a.SendTextToConversation("gchat:1", "hello")
	if err == nil {
		t.Fatal("expected unsupported platform error")
	}
	if !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSendTextToConversationGoogleAuthErrorMarksDisconnected(t *testing.T) {
	a := testSendApp(t)
	a.Connected.Store(true)
	if err := a.Store.UpsertConversation(&db.Conversation{
		ConversationID: "sms-conv-1",
		Name:           "Taylor",
		LastMessageTS:  time.Now().UnixMilli(),
		SourcePlatform: "sms",
	}); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}

	originalGetGoogleConversation := getGoogleConversationForSend
	getGoogleConversationForSend = func(_ *App, _ string) (*gmproto.Conversation, error) {
		return nil, errors.New("get conversation: HTTP 401: 16: Request had invalid authentication credentials")
	}
	t.Cleanup(func() {
		getGoogleConversationForSend = originalGetGoogleConversation
	})

	_, _, err := a.SendTextToConversation("sms-conv-1", "hello sms")
	if err == nil {
		t.Fatal("expected send error")
	}
	if a.Connected.Load() {
		t.Fatal("expected auth error to mark Google disconnected")
	}
	if got := a.GoogleStatus().LastError; got != googleAuthExpiredStatusMessage {
		t.Fatalf("last error = %q, want %q", got, googleAuthExpiredStatusMessage)
	}
}
