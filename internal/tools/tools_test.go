package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/rs/zerolog"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"

	"github.com/maxghenis/openmessage/internal/app"
	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/importer"
	"github.com/maxghenis/openmessage/internal/signallive"
	"github.com/maxghenis/openmessage/internal/whatsapplive"
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

func structuredMap(t *testing.T, result *mcp.CallToolResult) map[string]any {
	t.Helper()
	payload, ok := result.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("expected structured content map, got %T", result.StructuredContent)
	}
	return payload
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
	if !strings.Contains(text, "Alice") {
		t.Errorf("expected Alice in output, got: %s", text)
	}
	if !strings.Contains(text, "Hello!") {
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
	if !strings.Contains(text, "From Alice") {
		t.Errorf("expected 'From Alice', got: %s", text)
	}
	if strings.Contains(text, "From Bob") {
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
	if !strings.Contains(text, "Hello world") {
		t.Errorf("expected 'Hello world', got: %s", text)
	}
	payload := structuredMap(t, result)
	messages, ok := payload["messages"].([]messageSummary)
	if !ok {
		t.Fatalf("expected typed messages slice, got %T", payload["messages"])
	}
	if len(messages) != 1 || messages[0].Body != "Hello world" {
		t.Fatalf("unexpected structured messages: %#v", messages)
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
	if !strings.Contains(text, "Alice") {
		t.Errorf("expected Alice, got: %s", text)
	}
	if !strings.Contains(text, "[group]") {
		t.Errorf("expected [group], got: %s", text)
	}
	payload := structuredMap(t, result)
	conversations, ok := payload["conversations"].([]conversationSummary)
	if !ok {
		t.Fatalf("expected typed conversations slice, got %T", payload["conversations"])
	}
	if len(conversations) != 2 {
		t.Fatalf("expected 2 conversations, got %d", len(conversations))
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
	if !strings.Contains(text, "Hi there") {
		t.Errorf("expected 'Hi there', got: %s", text)
	}
	payload := structuredMap(t, result)
	messages, ok := payload["messages"].([]messageSummary)
	if !ok {
		t.Fatalf("expected typed messages slice, got %T", payload["messages"])
	}
	if len(messages) != 1 || messages[0].Body != "Hi there" {
		t.Fatalf("unexpected structured messages: %#v", messages)
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
	if !strings.Contains(text, "not connected") {
		t.Errorf("expected 'not connected' error, got: %s", text)
	}
	payload := structuredMap(t, result)
	if payload["error"] == "" {
		t.Fatalf("expected structured error payload, got %#v", payload)
	}
}

func TestSendMessageSignalDirect(t *testing.T) {
	a := testApp(t)
	now := time.Now().UnixMilli()

	originalSendSignalText := sendSignalText
	sendSignalText = func(_ *app.App, conversationID, body, replyToID string) (*db.Message, error) {
		if conversationID != "signal:+15551230000" {
			t.Fatalf("conversationID = %q, want signal:+15551230000", conversationID)
		}
		if body != "hi signal" {
			t.Fatalf("body = %q, want hi signal", body)
		}
		if replyToID != "" {
			t.Fatalf("replyToID = %q, want empty", replyToID)
		}
		return &db.Message{
			MessageID:      "signal:direct-1",
			ConversationID: conversationID,
			Body:           body,
			TimestampMS:    now,
			IsFromMe:       true,
			SourcePlatform: "signal",
		}, nil
	}
	t.Cleanup(func() {
		sendSignalText = originalSendSignalText
	})

	handler := sendMessageHandler(a)
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"platform":  "signal",
		"recipient": "+15551230000",
		"message":   "hi signal",
	}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %v", result.Content)
	}
	convo, err := a.Store.GetConversation("signal:+15551230000")
	if err != nil {
		t.Fatalf("GetConversation: %v", err)
	}
	if convo == nil || convo.SourcePlatform != "signal" {
		t.Fatalf("expected persisted signal conversation, got %#v", convo)
	}
	payload := structuredMap(t, result)
	if payload["ok"] != true {
		t.Fatalf("expected ok=true, got %#v", payload["ok"])
	}
	msg, err := a.Store.GetMessageByID("signal:direct-1")
	if err != nil {
		t.Fatalf("GetMessageByID: %v", err)
	}
	if msg == nil {
		t.Fatal("expected persisted signal direct-send message")
	}
}

func TestSendMessageWhatsAppCanonicalizesPhone(t *testing.T) {
	a := testApp(t)
	now := time.Now().UnixMilli()

	originalSendWhatsAppText := sendWhatsAppText
	sendWhatsAppText = func(_ *app.App, conversationID, body, replyToID string) (*db.Message, error) {
		if conversationID != "whatsapp:15551230000@s.whatsapp.net" {
			t.Fatalf("conversationID = %q, want canonical WhatsApp direct chat id", conversationID)
		}
		if body != "hi whatsapp" {
			t.Fatalf("body = %q, want hi whatsapp", body)
		}
		return &db.Message{
			MessageID:      "whatsapp:direct-1",
			ConversationID: conversationID,
			Body:           body,
			TimestampMS:    now,
			IsFromMe:       true,
			SourcePlatform: "whatsapp",
		}, nil
	}
	t.Cleanup(func() {
		sendWhatsAppText = originalSendWhatsAppText
	})

	handler := sendMessageHandler(a)
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"platform":     "whatsapp",
		"phone_number": "+1 (555) 123-0000",
		"message":      "hi whatsapp",
	}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %v", result.Content)
	}
	convo, err := a.Store.GetConversation("whatsapp:15551230000@s.whatsapp.net")
	if err != nil {
		t.Fatalf("GetConversation: %v", err)
	}
	if convo == nil || convo.SourcePlatform != "whatsapp" {
		t.Fatalf("expected persisted WhatsApp conversation, got %#v", convo)
	}
}

func TestSendMessageSMSPersistsConversationAndOutgoingMessage(t *testing.T) {
	a := testApp(t)

	originalGetOrCreateGoogleConversation := getOrCreateGoogleConversation
	originalSendGoogleTextPayload := sendGoogleTextPayload
	getOrCreateGoogleConversation = func(_ *app.App, phone string) (*gmproto.Conversation, error) {
		if phone != "+15551230000" {
			t.Fatalf("phone = %q, want +15551230000", phone)
		}
		return &gmproto.Conversation{
			ConversationID:       "sms-conv-1",
			Name:                 "Taylor",
			LastMessageTimestamp: 1234000,
		}, nil
	}
	sendGoogleTextPayload = func(_ *app.App, payload *gmproto.SendMessageRequest) (*gmproto.SendMessageResponse, error) {
		if payload.GetConversationID() != "sms-conv-1" {
			t.Fatalf("conversationID = %q, want sms-conv-1", payload.GetConversationID())
		}
		if payload.GetMessagePayload().GetMessageInfo()[0].GetMessageContent().GetContent() != "hi sms" {
			t.Fatalf("unexpected message payload: %#v", payload.GetMessagePayload())
		}
		return &gmproto.SendMessageResponse{Status: gmproto.SendMessageResponse_SUCCESS}, nil
	}
	t.Cleanup(func() {
		getOrCreateGoogleConversation = originalGetOrCreateGoogleConversation
		sendGoogleTextPayload = originalSendGoogleTextPayload
	})

	handler := sendMessageHandler(a)
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"phone_number": "+15551230000",
		"message":      "hi sms",
	}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %v", result.Content)
	}
	convo, err := a.Store.GetConversation("sms-conv-1")
	if err != nil {
		t.Fatalf("GetConversation: %v", err)
	}
	if convo == nil || convo.Name != "Taylor" {
		t.Fatalf("expected persisted SMS conversation, got %#v", convo)
	}
	msgs, err := a.Store.GetMessagesByConversation("sms-conv-1", 10)
	if err != nil {
		t.Fatalf("GetMessagesByConversation: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Body != "hi sms" {
		t.Fatalf("expected persisted outgoing sms message, got %#v", msgs)
	}
	payload := structuredMap(t, result)
	message, ok := payload["message"].(messageSummary)
	if !ok {
		t.Fatalf("expected typed message summary, got %T", payload["message"])
	}
	if message.Body != "hi sms" {
		t.Fatalf("unexpected structured message: %#v", message)
	}
}

func TestSendToConversationValidation(t *testing.T) {
	a := testApp(t)
	handler := sendToConversationHandler(a)
	req := mcp.CallToolRequest{}

	req.Params.Arguments = map[string]any{"message": "Hello"}
	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected error for missing conversation_id")
	}

	req.Params.Arguments = map[string]any{"conversation_id": "c1"}
	result, err = handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected error for missing message")
	}
}

func TestSendToConversationNotConnected(t *testing.T) {
	a := testApp(t)
	if err := a.Store.UpsertConversation(&db.Conversation{
		ConversationID: "c1",
		Name:           "Alice",
		LastMessageTS:  time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}

	handler := sendToConversationHandler(a)
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"conversation_id": "c1",
		"message":         "Hello",
	}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected error when not connected")
	}
	text := result.Content[0].(mcp.TextContent).Text
	if !strings.Contains(text, "not connected") {
		t.Fatalf("expected not connected error, got: %s", text)
	}
	payload := structuredMap(t, result)
	if payload["error"] == "" {
		t.Fatalf("expected structured error payload, got %#v", payload)
	}
}

func TestGetStatus(t *testing.T) {
	a := testApp(t)
	a.DataDir = t.TempDir()

	originalGoogleStatus := googleStatus
	originalWhatsAppStatus := whatsAppStatus
	originalSignalStatus := signalStatus
	googleStatus = func(*app.App) app.GoogleStatusSnapshot {
		return app.GoogleStatusSnapshot{
			Connected: false,
			Paired:    true,
		}
	}
	whatsAppStatus = func(*app.App) whatsapplive.StatusSnapshot {
		return whatsapplive.StatusSnapshot{
			Connected:  true,
			Paired:     true,
			AccountJID: "15551230000@s.whatsapp.net",
			PushName:   "Max",
		}
	}
	signalStatus = func(*app.App) signallive.StatusSnapshot {
		return signallive.StatusSnapshot{
			Paired:      true,
			QRAvailable: true,
			LastError:   "signal-cli unavailable",
		}
	}
	t.Cleanup(func() {
		googleStatus = originalGoogleStatus
		whatsAppStatus = originalWhatsAppStatus
		signalStatus = originalSignalStatus
	})

	handler := getStatusHandler(a)
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	text := result.Content[0].(mcp.TextContent).Text
	if !strings.Contains(text, "Overall: connected") {
		t.Errorf("expected overall connected summary, got: %s", text)
	}
	if !strings.Contains(text, "WhatsApp:") {
		t.Errorf("expected WhatsApp section, got: %s", text)
	}
	if !strings.Contains(text, "Signal:") {
		t.Errorf("expected Signal section, got: %s", text)
	}
	if !strings.Contains(text, "signal-cli unavailable") {
		t.Errorf("expected Signal last error, got: %s", text)
	}
	payload := structuredMap(t, result)
	if payload["overall_connected"] != true {
		t.Fatalf("expected overall_connected=true, got %#v", payload["overall_connected"])
	}
}

func TestResolveContactRoutesPrefersSMSThread(t *testing.T) {
	a := testApp(t)
	now := time.Now().UnixMilli()

	if err := a.Store.UpsertConversation(&db.Conversation{
		ConversationID: "sms-conv-1",
		Name:           "Leigh Gibson",
		Participants:   `[{"name":"Leigh Gibson","number":"+15551230000"}]`,
		LastMessageTS:  now,
		SourcePlatform: "sms",
	}); err != nil {
		t.Fatalf("seed sms conversation: %v", err)
	}
	if err := a.Store.UpsertConversation(&db.Conversation{
		ConversationID: "whatsapp:15551230000@s.whatsapp.net",
		Name:           "Leigh Gibson",
		Participants:   `[{"name":"Leigh Gibson","number":"+15551230000"}]`,
		LastMessageTS:  now + 1,
		SourcePlatform: "whatsapp",
	}); err != nil {
		t.Fatalf("seed whatsapp conversation: %v", err)
	}
	if err := a.Store.UpsertUnifiedContact(&db.UnifiedContact{
		UnifiedID:   "leigh",
		DisplayName: "Leigh Gibson",
		Identifiers: `[{"platform":"sms","value":"+15551230000"},{"platform":"whatsapp","value":"+15551230000"}]`,
	}); err != nil {
		t.Fatalf("seed unified contact: %v", err)
	}

	originalWhatsAppStatus := whatsAppStatus
	originalSignalStatus := signalStatus
	whatsAppStatus = func(*app.App) whatsapplive.StatusSnapshot {
		return whatsapplive.StatusSnapshot{Connected: true}
	}
	signalStatus = func(*app.App) signallive.StatusSnapshot {
		return signallive.StatusSnapshot{}
	}
	t.Cleanup(func() {
		whatsAppStatus = originalWhatsAppStatus
		signalStatus = originalSignalStatus
	})

	handler := resolveContactRoutesHandler(a)
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{"query": "Leigh"}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %v", result.Content)
	}

	payload := structuredMap(t, result)
	matches, ok := payload["matches"].([]resolvedRouteMatch)
	if !ok {
		t.Fatalf("expected typed route matches, got %T", payload["matches"])
	}
	if len(matches) != 1 {
		t.Fatalf("expected 1 route match, got %d (%#v)", len(matches), matches)
	}
	match := matches[0]
	if match.PreferredReplyConversationID != "sms-conv-1" {
		t.Fatalf("preferred reply route = %q, want sms-conv-1", match.PreferredReplyConversationID)
	}
	if match.RouteCount != 2 {
		t.Fatalf("route count = %d, want 2", match.RouteCount)
	}
	if len(match.Routes) != 2 {
		t.Fatalf("routes length = %d, want 2", len(match.Routes))
	}
	if match.Routes[0].Conversation.SourcePlatform != "sms" {
		t.Fatalf("expected sms route first, got %#v", match.Routes)
	}
}

func TestSendToConversationSignal(t *testing.T) {
	a := testApp(t)
	now := time.Now().UnixMilli()
	if err := a.Store.UpsertConversation(&db.Conversation{
		ConversationID: "signal:group-1",
		Name:           "Taylor",
		LastMessageTS:  now,
		SourcePlatform: "signal",
	}); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}

	originalSendTextToConversation := sendTextToConversation
	called := false
	sendTextToConversation = func(_ *app.App, conversationID, body string) (conversationSummary, messageSummary, error) {
		called = true
		if conversationID != "signal:group-1" {
			t.Fatalf("conversationID = %q, want signal:group-1", conversationID)
		}
		if body != "Hello from MCP" {
			t.Fatalf("body = %q, want Hello from MCP", body)
		}
		return conversationSummary{
				ConversationID: conversationID,
				Name:           "Taylor",
				SourcePlatform: "signal",
				IsGroup:        true,
				LastMessageTS:  now,
			}, messageSummary{
				MessageID:      "signal:out-1",
				ConversationID: conversationID,
				Body:           body,
				TimestampMS:    now + 1,
				IsFromMe:       true,
				SourcePlatform: "signal",
			}, nil
	}
	t.Cleanup(func() {
		sendTextToConversation = originalSendTextToConversation
	})

	handler := sendToConversationHandler(a)
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"conversation_id": "signal:group-1",
		"message":         "Hello from MCP",
	}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %v", result.Content)
	}
	if !called {
		t.Fatal("expected sendSignalText to be called")
	}
	text := result.Content[0].(mcp.TextContent).Text
	if !strings.Contains(text, "Taylor") {
		t.Fatalf("expected conversation name in response, got: %s", text)
	}
	payload := structuredMap(t, result)
	message, ok := payload["message"].(messageSummary)
	if !ok {
		t.Fatalf("expected typed message summary, got %T", payload["message"])
	}
	if message.MessageID != "signal:out-1" {
		t.Fatalf("unexpected structured message: %#v", message)
	}
}

func TestSendToConversationUnsupportedPlatform(t *testing.T) {
	a := testApp(t)
	if err := a.Store.UpsertConversation(&db.Conversation{
		ConversationID: "gchat:1",
		Name:           "Imported Chat",
		LastMessageTS:  time.Now().UnixMilli(),
		SourcePlatform: "gchat",
	}); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}

	handler := sendToConversationHandler(a)
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"conversation_id": "gchat:1",
		"message":         "Hello",
	}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected unsupported platform error")
	}
	text := result.Content[0].(mcp.TextContent).Text
	if !strings.Contains(text, "not supported") {
		t.Fatalf("expected unsupported platform message, got: %s", text)
	}
}

func TestSendMediaToConversationSignal(t *testing.T) {
	a := testApp(t)
	now := time.Now().UnixMilli()
	if err := a.Store.UpsertConversation(&db.Conversation{
		ConversationID: "signal:group-2",
		Name:           "Signal Crew",
		LastMessageTS:  now,
		SourcePlatform: "signal",
	}); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}

	filePath := filepath.Join(t.TempDir(), "photo.jpg")
	if err := os.WriteFile(filePath, []byte("jpeg-bytes"), 0644); err != nil {
		t.Fatalf("WriteFile(%s): %v", filePath, err)
	}

	originalSendSignalMediaMessage := sendSignalMediaMessage
	sendSignalMediaMessage = func(_ *app.App, conversationID string, data []byte, filename, mimeType, caption, replyToID string) (*db.Message, error) {
		if conversationID != "signal:group-2" {
			t.Fatalf("conversationID = %q, want signal:group-2", conversationID)
		}
		if filename != "photo.jpg" {
			t.Fatalf("filename = %q, want photo.jpg", filename)
		}
		if mimeType != "image/jpeg" {
			t.Fatalf("mimeType = %q, want image/jpeg", mimeType)
		}
		if caption != "look at this" {
			t.Fatalf("caption = %q, want look at this", caption)
		}
		if replyToID != "signal:quoted-1" {
			t.Fatalf("replyToID = %q, want signal:quoted-1", replyToID)
		}
		if string(data) != "jpeg-bytes" {
			t.Fatalf("data = %q, want jpeg-bytes", string(data))
		}
		return &db.Message{
			MessageID:      "signal:media-out-1",
			ConversationID: conversationID,
			Body:           caption,
			TimestampMS:    now + 1,
			IsFromMe:       true,
			MediaID:        "signalatt:1",
			MimeType:       mimeType,
			ReplyToID:      replyToID,
			SourcePlatform: "signal",
		}, nil
	}
	t.Cleanup(func() {
		sendSignalMediaMessage = originalSendSignalMediaMessage
	})

	handler := sendMediaToConversationHandler(a)
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"conversation_id": "signal:group-2",
		"file_path":       filePath,
		"caption":         "look at this",
		"reply_to_id":     "signal:quoted-1",
	}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %v", result.Content)
	}
	msg, err := a.Store.GetMessageByID("signal:media-out-1")
	if err != nil {
		t.Fatalf("GetMessageByID: %v", err)
	}
	if msg == nil {
		t.Fatal("expected outgoing Signal media message to be persisted")
	}
}

func TestSendMediaToConversationUnsupportedPlatform(t *testing.T) {
	a := testApp(t)
	if err := a.Store.UpsertConversation(&db.Conversation{
		ConversationID: "gchat:media-1",
		Name:           "Imported Chat",
		LastMessageTS:  time.Now().UnixMilli(),
		SourcePlatform: "gchat",
	}); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}

	filePath := filepath.Join(t.TempDir(), "photo.jpg")
	if err := os.WriteFile(filePath, []byte("jpeg-bytes"), 0644); err != nil {
		t.Fatalf("WriteFile(%s): %v", filePath, err)
	}

	handler := sendMediaToConversationHandler(a)
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"conversation_id": "gchat:media-1",
		"file_path":       filePath,
	}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected unsupported platform error")
	}
	text := result.Content[0].(mcp.TextContent).Text
	if !strings.Contains(text, "not supported") {
		t.Fatalf("expected unsupported platform message, got: %s", text)
	}
}

func TestReactToMessageWhatsApp(t *testing.T) {
	a := testApp(t)
	if err := a.Store.UpsertConversation(&db.Conversation{
		ConversationID: "whatsapp:chat-1",
		Name:           "Jenn",
		LastMessageTS:  time.Now().UnixMilli(),
		SourcePlatform: "whatsapp",
	}); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}

	originalSendWhatsAppReactionMessage := sendWhatsAppReactionMessage
	called := false
	sendWhatsAppReactionMessage = func(_ *app.App, conversationID, messageID, emoji, action string) error {
		called = true
		if conversationID != "whatsapp:chat-1" {
			t.Fatalf("conversationID = %q, want whatsapp:chat-1", conversationID)
		}
		if messageID != "whatsapp:msg-1" {
			t.Fatalf("messageID = %q, want whatsapp:msg-1", messageID)
		}
		if emoji != "🔥" {
			t.Fatalf("emoji = %q, want 🔥", emoji)
		}
		if action != "remove" {
			t.Fatalf("action = %q, want remove", action)
		}
		return nil
	}
	t.Cleanup(func() {
		sendWhatsAppReactionMessage = originalSendWhatsAppReactionMessage
	})

	handler := reactToMessageHandler(a)
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"conversation_id": "whatsapp:chat-1",
		"message_id":      "whatsapp:msg-1",
		"emoji":           "🔥",
		"action":          "remove",
	}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %v", result.Content)
	}
	if !called {
		t.Fatal("expected sendWhatsAppReactionMessage to be called")
	}
}

func TestReactToMessageUnsupportedPlatform(t *testing.T) {
	a := testApp(t)
	if err := a.Store.UpsertConversation(&db.Conversation{
		ConversationID: "gchat:react-1",
		Name:           "Imported Chat",
		LastMessageTS:  time.Now().UnixMilli(),
		SourcePlatform: "gchat",
	}); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}

	handler := reactToMessageHandler(a)
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"conversation_id": "gchat:react-1",
		"message_id":      "gchat:msg-1",
		"emoji":           "👍",
	}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected unsupported platform error")
	}
	text := result.Content[0].(mcp.TextContent).Text
	if !strings.Contains(text, "not supported") {
		t.Fatalf("expected unsupported platform message, got: %s", text)
	}
}

func TestImportMessagesSignal(t *testing.T) {
	a := testApp(t)

	originalImportSignalDesktop := importSignalDesktop
	importSignalDesktop = func(store *db.Store, supportDir, name, address string) (*importer.ImportResult, error) {
		if store != a.Store {
			t.Fatal("expected import to use app store")
		}
		if supportDir != "/tmp/Signal" {
			t.Fatalf("supportDir = %q, want /tmp/Signal", supportDir)
		}
		if name != "Max" {
			t.Fatalf("name = %q, want Max", name)
		}
		if address != "+15551230000" {
			t.Fatalf("address = %q, want +15551230000", address)
		}
		return &importer.ImportResult{
			ConversationsCreated: 2,
			MessagesImported:     15,
			MessagesDuplicate:    1,
		}, nil
	}
	t.Cleanup(func() {
		importSignalDesktop = originalImportSignalDesktop
	})

	handler := importMessagesHandler(a)
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"source":  "signal",
		"path":    "/tmp/Signal",
		"name":    "Max",
		"address": "+15551230000",
	}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %v", result.Content)
	}
	text := result.Content[0].(mcp.TextContent).Text
	if !strings.Contains(text, "Conversations created: 2") {
		t.Fatalf("expected conversation count in response, got: %s", text)
	}
	if !strings.Contains(text, "Messages imported: 15") {
		t.Fatalf("expected import count in response, got: %s", text)
	}
}

func TestImportMessagesUnknownSource(t *testing.T) {
	a := testApp(t)
	handler := importMessagesHandler(a)
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"source": "telegram",
	}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected error result for unknown source")
	}
	text := result.Content[0].(mcp.TextContent).Text
	if !strings.Contains(text, "signal") {
		t.Fatalf("expected supported source list to mention signal, got: %s", text)
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
	if !strings.Contains(text, "Alice") {
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
	if !strings.Contains(got, "voice message") {
		t.Errorf("voice message: expected 'voice message' tag, got: %s", got)
	}
	if !strings.Contains(got, "msg-2") {
		t.Errorf("voice message: expected message_id in output, got: %s", got)
	}

	// Image with caption
	got = formatMessageBody("Check this out", "media-456", "image/jpeg", "msg-3")
	if !strings.Contains(got, "Check this out") {
		t.Errorf("image with caption: expected caption, got: %s", got)
	}
	if !strings.Contains(got, "image") {
		t.Errorf("image with caption: expected 'image' tag, got: %s", got)
	}

	// Video
	got = formatMessageBody("", "media-789", "video/mp4", "msg-4")
	if !strings.Contains(got, "video") {
		t.Errorf("video: expected 'video' tag, got: %s", got)
	}

	// Unknown attachment type
	got = formatMessageBody("", "media-000", "application/pdf", "msg-5")
	if !strings.Contains(got, "attachment") {
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
	if !strings.Contains(text, "voice message") {
		t.Errorf("expected 'voice message' indicator, got: %s", text)
	}
	if !strings.Contains(text, "vm-1") {
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
	if !strings.Contains(text, "not found") {
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
	if !strings.Contains(text, "no media") {
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
	if !strings.Contains(text, "not connected") {
		t.Errorf("expected 'not connected' error, got: %s", text)
	}
}

func TestDownloadMediaWhatsApp(t *testing.T) {
	a := testApp(t)
	now := time.Now().UnixMilli()

	if err := a.Store.UpsertMessage(&db.Message{
		MessageID:      "whatsapp:media-msg",
		ConversationID: "whatsapp:chat-1",
		TimestampMS:    now,
		MediaID:        "wa:media-123",
		MimeType:       "image/png",
		SourcePlatform: "whatsapp",
	}); err != nil {
		t.Fatalf("seed message: %v", err)
	}

	originalDownloadWhatsAppMedia := downloadWhatsAppMedia
	downloadWhatsAppMedia = func(_ *app.App, msg *db.Message) ([]byte, string, error) {
		if msg.MessageID != "whatsapp:media-msg" {
			t.Fatalf("messageID = %q, want whatsapp:media-msg", msg.MessageID)
		}
		return []byte("png"), "image/png", nil
	}
	t.Cleanup(func() {
		downloadWhatsAppMedia = originalDownloadWhatsAppMedia
	})

	handler := downloadMediaHandler(a)
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{"message_id": "whatsapp:media-msg"}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected tool error: %v", result.Content)
	}
	text := result.Content[0].(mcp.TextContent).Text
	path := strings.TrimSpace(text[strings.LastIndex(text, "\n")+1:])
	t.Cleanup(func() { _ = os.Remove(path) })
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	if string(data) != "png" {
		t.Fatalf("downloaded data = %q, want png", string(data))
	}
}

func TestDownloadMediaSignal(t *testing.T) {
	a := testApp(t)
	now := time.Now().UnixMilli()

	if err := a.Store.UpsertMessage(&db.Message{
		MessageID:      "signal:media-msg",
		ConversationID: "signal:chat-1",
		TimestampMS:    now,
		MediaID:        "signalatt:media-123",
		MimeType:       "image/jpeg",
		SourcePlatform: "signal",
	}); err != nil {
		t.Fatalf("seed message: %v", err)
	}

	originalDownloadSignalMedia := downloadSignalMedia
	downloadSignalMedia = func(_ *app.App, msg *db.Message) ([]byte, string, error) {
		if msg.MessageID != "signal:media-msg" {
			t.Fatalf("messageID = %q, want signal:media-msg", msg.MessageID)
		}
		return []byte("jpg"), "image/jpeg", nil
	}
	t.Cleanup(func() {
		downloadSignalMedia = originalDownloadSignalMedia
	})

	handler := downloadMediaHandler(a)
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{"message_id": "signal:media-msg"}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected tool error: %v", result.Content)
	}
	text := result.Content[0].(mcp.TextContent).Text
	path := strings.TrimSpace(text[strings.LastIndex(text, "\n")+1:])
	t.Cleanup(func() { _ = os.Remove(path) })
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	if string(data) != "jpg" {
		t.Fatalf("downloaded data = %q, want jpg", string(data))
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

func TestSendGroupMessageNotConnected(t *testing.T) {
	a := testApp(t)

	handler := sendGroupMessageHandler(a)
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"phone_numbers": `["+15551234567", "+15559876543"]`,
		"message":       "Hello group",
	}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !result.IsError {
		t.Error("expected error when not connected")
	}
	text := result.Content[0].(mcp.TextContent).Text
	if !strings.Contains(text, "not connected") {
		t.Errorf("expected 'not connected' error, got: %s", text)
	}
}

func TestSendGroupMessageMissingPhones(t *testing.T) {
	a := testApp(t)

	handler := sendGroupMessageHandler(a)
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"message": "Hello group",
	}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !result.IsError {
		t.Error("expected error for missing phone_numbers")
	}
}

func TestSendGroupMessageMissingMessage(t *testing.T) {
	a := testApp(t)

	handler := sendGroupMessageHandler(a)
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"phone_numbers": `["+15551234567", "+15559876543"]`,
	}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !result.IsError {
		t.Error("expected error for missing message")
	}
}

func TestSendGroupMessageInvalidJSON(t *testing.T) {
	a := testApp(t)

	handler := sendGroupMessageHandler(a)
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"phone_numbers": "not json",
		"message":       "Hello group",
	}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !result.IsError {
		t.Error("expected error for invalid JSON")
	}
	text := result.Content[0].(mcp.TextContent).Text
	if !strings.Contains(text, "JSON array") {
		t.Errorf("expected JSON array error, got: %s", text)
	}
}

func TestSendGroupMessageTooFewNumbers(t *testing.T) {
	a := testApp(t)

	handler := sendGroupMessageHandler(a)
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"phone_numbers": `["+15551234567"]`,
		"message":       "Hello group",
	}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !result.IsError {
		t.Error("expected error for too few numbers")
	}
	text := result.Content[0].(mcp.TextContent).Text
	if !strings.Contains(text, "at least 2") {
		t.Errorf("expected 'at least 2' error, got: %s", text)
	}
}
