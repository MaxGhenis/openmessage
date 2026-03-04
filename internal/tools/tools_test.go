package tools

import (
	"context"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/rs/zerolog"

	"github.com/maxghenis/openmessage/internal/app"
	"github.com/maxghenis/openmessage/internal/db"
)

func testApp(t *testing.T) *app.App {
	t.Helper()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("create db: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return &app.App{
		Store:  store,
		Logger: zerolog.Nop(),
	}
}

func TestRegisterTools(t *testing.T) {
	a := testApp(t)
	s := server.NewMCPServer("gmessages-test", "0.1.0")
	Register(s, a)
	// Just verify it doesn't panic
}

func TestGetMessagesEmpty(t *testing.T) {
	a := testApp(t)
	handler := getMessagesHandler(a)

	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected tool error: %v", result.Content)
	}
	text := result.Content[0].(mcp.TextContent).Text
	if text != "No messages found." {
		t.Errorf("expected 'No messages found.', got: %s", text)
	}
}

func TestGetMessagesWithData(t *testing.T) {
	a := testApp(t)
	now := time.Now().UnixMilli()

	a.Store.UpsertMessage(&db.Message{
		MessageID:      "msg-1",
		ConversationID: "c1",
		SenderName:     "Alice",
		SenderNumber:   "+15551234567",
		Body:           "Hello!",
		TimestampMS:    now,
		IsFromMe:       false,
	})

	handler := getMessagesHandler(a)
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	text := result.Content[0].(mcp.TextContent).Text
	if text == "No messages found." {
		t.Error("expected messages, got none")
	}
	if !contains(text, "Alice") {
		t.Errorf("expected Alice in output, got: %s", text)
	}
	if !contains(text, "Hello!") {
		t.Errorf("expected Hello! in output, got: %s", text)
	}
}

func TestGetMessagesFilterByPhone(t *testing.T) {
	a := testApp(t)
	now := time.Now().UnixMilli()

	a.Store.UpsertMessage(&db.Message{
		MessageID: "1", ConversationID: "c1", SenderNumber: "+15551111111",
		Body: "From Alice", TimestampMS: now,
	})
	a.Store.UpsertMessage(&db.Message{
		MessageID: "2", ConversationID: "c1", SenderNumber: "+15552222222",
		Body: "From Bob", TimestampMS: now + 1,
	})

	handler := getMessagesHandler(a)
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{"phone_number": "+15551111111"}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	text := result.Content[0].(mcp.TextContent).Text
	if !contains(text, "From Alice") {
		t.Errorf("expected 'From Alice', got: %s", text)
	}
	if contains(text, "From Bob") {
		t.Errorf("should not contain 'From Bob', got: %s", text)
	}
}

func TestSearchMessages(t *testing.T) {
	a := testApp(t)
	now := time.Now().UnixMilli()

	a.Store.UpsertMessage(&db.Message{
		MessageID: "1", ConversationID: "c1", Body: "Hello world", TimestampMS: now,
	})
	a.Store.UpsertMessage(&db.Message{
		MessageID: "2", ConversationID: "c1", Body: "Goodbye", TimestampMS: now + 1,
	})

	handler := searchMessagesHandler(a)

	// Search for "hello"
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{"query": "hello"}
	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	text := result.Content[0].(mcp.TextContent).Text
	if !contains(text, "Hello world") {
		t.Errorf("expected 'Hello world', got: %s", text)
	}

	// Empty query
	req.Params.Arguments = map[string]any{}
	result, err = handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !result.IsError {
		t.Error("expected error for missing query")
	}
}

func TestListConversations(t *testing.T) {
	a := testApp(t)
	now := time.Now().UnixMilli()

	a.Store.UpsertConversation(&db.Conversation{
		ConversationID: "c1", Name: "Alice", LastMessageTS: now,
	})
	a.Store.UpsertConversation(&db.Conversation{
		ConversationID: "c2", Name: "Group Chat", IsGroup: true, LastMessageTS: now + 1,
	})

	handler := listConversationsHandler(a)
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	text := result.Content[0].(mcp.TextContent).Text
	if !contains(text, "Alice") {
		t.Errorf("expected Alice, got: %s", text)
	}
	if !contains(text, "[group]") {
		t.Errorf("expected [group], got: %s", text)
	}
}

func TestGetConversation(t *testing.T) {
	a := testApp(t)
	now := time.Now().UnixMilli()

	a.Store.UpsertConversation(&db.Conversation{
		ConversationID: "c1", Name: "Alice",
	})
	a.Store.UpsertMessage(&db.Message{
		MessageID: "m1", ConversationID: "c1", Body: "Hi there", TimestampMS: now,
	})

	handler := getConversationHandler(a)

	// Valid conversation
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{"conversation_id": "c1"}
	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	text := result.Content[0].(mcp.TextContent).Text
	if !contains(text, "Hi there") {
		t.Errorf("expected 'Hi there', got: %s", text)
	}

	// Missing conversation_id
	req.Params.Arguments = map[string]any{}
	result, err = handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !result.IsError {
		t.Error("expected error for missing conversation_id")
	}
}

func TestSendMessageNotConnected(t *testing.T) {
	a := testApp(t)

	handler := sendMessageHandler(a)
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"phone_number": "+15551234567",
		"message":      "Hello",
	}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !result.IsError {
		t.Error("expected error when not connected")
	}
	text := result.Content[0].(mcp.TextContent).Text
	if !contains(text, "not connected") {
		t.Errorf("expected 'not connected' error, got: %s", text)
	}
}

func TestSendToConversationNotConnected(t *testing.T) {
	a := testApp(t)

	handler := sendToConversationHandler(a)
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"conversation_id": "conv-123",
		"message":         "Hello",
	}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !result.IsError {
		t.Error("expected error when not connected")
	}
	text := result.Content[0].(mcp.TextContent).Text
	if !contains(text, "not connected") {
		t.Errorf("expected 'not connected' error, got: %s", text)
	}
}

func TestSendToConversationMissingArgs(t *testing.T) {
	a := testApp(t)
	handler := sendToConversationHandler(a)

	// Missing conversation_id
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"message": "Hello",
	}
	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !result.IsError {
		t.Error("expected error for missing conversation_id")
	}

	// Missing message
	req.Params.Arguments = map[string]any{
		"conversation_id": "conv-123",
	}
	result, err = handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !result.IsError {
		t.Error("expected error for missing message")
	}
}

func TestGetStatus(t *testing.T) {
	a := testApp(t)

	handler := getStatusHandler(a)
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	text := result.Content[0].(mcp.TextContent).Text
	if !contains(text, "not connected") {
		t.Errorf("expected 'not connected', got: %s", text)
	}
}

func TestListContacts(t *testing.T) {
	a := testApp(t)

	a.Store.UpsertContact(&db.Contact{ContactID: "1", Name: "Alice", Number: "+15551234567"})

	handler := listContactsHandler(a)
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	text := result.Content[0].(mcp.TextContent).Text
	if !contains(text, "Alice") {
		t.Errorf("expected Alice, got: %s", text)
	}
}

func TestFormatMessageBody(t *testing.T) {
	// Plain text message — no media
	got := formatMessageBody("Hello!", "", "", "msg-1")
	if got != "Hello!" {
		t.Errorf("plain text: expected 'Hello!', got: %s", got)
	}

	// Voice message with no body text
	got = formatMessageBody("", "media-123", "audio/ogg", "msg-2")
	if !contains(got, "voice message") {
		t.Errorf("voice message: expected 'voice message' tag, got: %s", got)
	}
	if !contains(got, "msg-2") {
		t.Errorf("voice message: expected message_id in output, got: %s", got)
	}

	// Image with caption
	got = formatMessageBody("Check this out", "media-456", "image/jpeg", "msg-3")
	if !contains(got, "Check this out") {
		t.Errorf("image with caption: expected caption, got: %s", got)
	}
	if !contains(got, "image") {
		t.Errorf("image with caption: expected 'image' tag, got: %s", got)
	}

	// Video
	got = formatMessageBody("", "media-789", "video/mp4", "msg-4")
	if !contains(got, "video") {
		t.Errorf("video: expected 'video' tag, got: %s", got)
	}

	// Unknown attachment type
	got = formatMessageBody("", "media-000", "application/pdf", "msg-5")
	if !contains(got, "attachment") {
		t.Errorf("unknown: expected 'attachment' tag, got: %s", got)
	}
}

func TestGetMessagesMediaIndicator(t *testing.T) {
	a := testApp(t)
	now := time.Now().UnixMilli()

	// Insert a voice message (empty body, has media)
	a.Store.UpsertMessage(&db.Message{
		MessageID:      "vm-1",
		ConversationID: "c1",
		SenderName:     "Jenn",
		SenderNumber:   "+14699991654",
		Body:           "",
		TimestampMS:    now,
		IsFromMe:       false,
		MediaID:        "media-abc",
		MimeType:       "audio/ogg",
		DecryptionKey:  "deadbeef",
	})

	handler := getMessagesHandler(a)
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	text := result.Content[0].(mcp.TextContent).Text
	if !contains(text, "voice message") {
		t.Errorf("expected 'voice message' indicator, got: %s", text)
	}
	if !contains(text, "vm-1") {
		t.Errorf("expected message_id 'vm-1' in output for download_media, got: %s", text)
	}
}

func TestDownloadMediaNoMessage(t *testing.T) {
	a := testApp(t)

	handler := downloadMediaHandler(a)
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{"message_id": "nonexistent"}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !result.IsError {
		t.Error("expected error for nonexistent message")
	}
	text := result.Content[0].(mcp.TextContent).Text
	if !contains(text, "not found") {
		t.Errorf("expected 'not found' error, got: %s", text)
	}
}

func TestDownloadMediaNoMediaID(t *testing.T) {
	a := testApp(t)
	now := time.Now().UnixMilli()

	a.Store.UpsertMessage(&db.Message{
		MessageID:      "text-msg",
		ConversationID: "c1",
		Body:           "Just text",
		TimestampMS:    now,
	})

	handler := downloadMediaHandler(a)
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{"message_id": "text-msg"}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !result.IsError {
		t.Error("expected error for message with no media")
	}
	text := result.Content[0].(mcp.TextContent).Text
	if !contains(text, "no media") {
		t.Errorf("expected 'no media' error, got: %s", text)
	}
}

func TestDownloadMediaNotConnected(t *testing.T) {
	a := testApp(t)
	now := time.Now().UnixMilli()

	a.Store.UpsertMessage(&db.Message{
		MessageID:      "media-msg",
		ConversationID: "c1",
		Body:           "",
		TimestampMS:    now,
		MediaID:        "mid-123",
		MimeType:       "audio/ogg",
		DecryptionKey:  "deadbeef",
	})

	handler := downloadMediaHandler(a)
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{"message_id": "media-msg"}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !result.IsError {
		t.Error("expected error when not connected")
	}
	text := result.Content[0].(mcp.TextContent).Text
	if !contains(text, "not connected") {
		t.Errorf("expected 'not connected' error, got: %s", text)
	}
}

func TestDownloadMediaMissingID(t *testing.T) {
	a := testApp(t)

	handler := downloadMediaHandler(a)
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !result.IsError {
		t.Error("expected error for missing message_id")
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsStr(s, substr))
}

func containsStr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
