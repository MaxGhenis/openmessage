package tools

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/maxghenis/openmessage/internal/app"
	"github.com/maxghenis/openmessage/internal/db"
)

// TestLegacySendToolDescriptorParity pins the complete flag-off tools/list
// surface. In particular, v2-only descriptions or arguments must not leak into
// the legacy descriptors while OPENMESSAGES_V2_SEND is disabled.
func TestLegacySendToolDescriptorParity(t *testing.T) {
	tests := []struct {
		name string
		tool mcp.Tool
		want string
	}{
		{
			name: "send_message",
			tool: sendMessageTool(),
			want: `{"annotations":{"readOnlyHint":false,"destructiveHint":false,"idempotentHint":false,"openWorldHint":true},"description":"Send a direct text message on ONE explicit platform. Defaults to SMS/RCS when no platform is specified. The requested platform is a hard contract: if it cannot send, the tool fails with the reason and queues nothing — it never falls back to a different platform.","inputSchema":{"type":"object","properties":{"message":{"description":"Message text to send","type":"string"},"phone_number":{"description":"Legacy alias for recipient. For SMS/RCS use a phone number with country code (e.g., +15551234567).","type":"string"},"platform":{"description":"Target platform: sms (covers RCS), whatsapp, or signal. Defaults to sms. imessage is import/read-only — OpenMessage cannot send iMessages.","type":"string"},"recipient":{"description":"Recipient identifier. Use a phone number for SMS/RCS or Signal, and a phone number or WhatsApp JID for WhatsApp.","type":"string"}},"required":["message"]},"name":"send_message"}`,
		},
		{
			name: "send_to_conversation",
			tool: sendToConversationTool(),
			want: `{"annotations":{"readOnlyHint":false,"destructiveHint":false,"idempotentHint":false,"openWorldHint":true},"description":"Send a text message to an existing conversation by conversation ID. The message goes out on the conversation's own platform — there is never a fallback to a different platform; pass the optional platform argument to assert which platform you intend, and the tool fails on a mismatch instead of sending.","inputSchema":{"type":"object","properties":{"conversation_id":{"description":"Existing conversation ID from list_conversations or get_conversation","type":"string"},"message":{"description":"Message text to send","type":"string"},"platform":{"description":"Optional assertion of the platform this conversation must be on (sms, whatsapp, signal). Mismatch fails the send instead of routing to an unintended channel.","type":"string"}},"required":["conversation_id","message"]},"name":"send_to_conversation"}`,
		},
		{
			name: "send_media_to_conversation",
			tool: sendMediaToConversationTool(),
			want: `{"annotations":{"readOnlyHint":false,"destructiveHint":false,"idempotentHint":false,"openWorldHint":true},"description":"Send a media attachment to an existing conversation by conversation ID across supported platforms","inputSchema":{"type":"object","properties":{"caption":{"description":"Optional caption for platforms that support media captions","type":"string"},"conversation_id":{"description":"Existing conversation ID from list_conversations or get_conversation","type":"string"},"file_path":{"description":"Absolute or relative path to the local file to send","type":"string"},"mime_type":{"description":"Optional MIME type override, for example image/png","type":"string"},"reply_to_id":{"description":"Optional message ID to reply to when the platform supports media replies","type":"string"}},"required":["conversation_id","file_path"]},"name":"send_media_to_conversation"}`,
		},
		{
			name: "send_group_message",
			tool: sendGroupMessageTool(),
			want: `{"annotations":{"readOnlyHint":false,"destructiveHint":false,"idempotentHint":false,"openWorldHint":true},"description":"Send a text message to a group conversation (MMS group). Creates the group if it doesn't exist.","inputSchema":{"type":"object","properties":{"message":{"description":"Message text to send","type":"string"},"phone_numbers":{"description":"JSON array of phone numbers with country code, e.g. [\"+15551234567\", \"+15559876543\"]","type":"string"}},"required":["phone_numbers","message"]},"name":"send_group_message"}`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertLegacyJSON(t, test.tool, test.want)
		})
	}
}

func TestV2SendToolDescriptorsExplainIdempotencyAndUncertainDelivery(t *testing.T) {
	tests := []struct {
		name string
		tool mcp.Tool
	}{
		{name: "send_message", tool: sendMessageTool(true)},
		{name: "send_to_conversation", tool: sendToConversationTool(true)},
		{name: "send_media_to_conversation", tool: sendMediaToConversationTool(true)},
		{name: "send_group_message", tool: sendGroupMessageTool(true)},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			idempotency, ok := test.tool.InputSchema.Properties["idempotency_key"].(map[string]any)
			if !ok {
				t.Fatalf("idempotency_key schema = %#v, want a property object", test.tool.InputSchema.Properties["idempotency_key"])
			}
			if got := idempotency["type"]; got != "string" {
				t.Fatalf("idempotency_key type = %#v, want string", got)
			}
			propertyDescription, _ := idempotency["description"].(string)
			if !strings.Contains(strings.ToLower(propertyDescription), "same key") {
				t.Fatalf("idempotency_key description does not explain replay semantics: %q", propertyDescription)
			}
			for _, required := range test.tool.InputSchema.Required {
				if required == "idempotency_key" {
					t.Fatal("idempotency_key must remain optional")
				}
			}

			description := strings.ToLower(test.tool.Description)
			if !strings.Contains(description, "uncertain") ||
				!strings.Contains(description, "may have accepted") ||
				!strings.Contains(description, "do not retry automatically") {
				t.Fatalf("v2 tool description does not explain uncertain/do-not-retry semantics: %q", test.tool.Description)
			}
			if test.tool.Annotations.IdempotentHint == nil || *test.tool.Annotations.IdempotentHint {
				t.Fatalf("idempotentHint = %#v, want explicit false", test.tool.Annotations.IdempotentHint)
			}
		})
	}
}

func TestRegisterNilOrDisabledV2UsesLegacySendToolsAndHandler(t *testing.T) {
	originalSend := sendTextToConversation
	legacyCalls := 0
	sendTextToConversation = func(_ *app.App, conversationID, body string) (conversationSummary, messageSummary, error) {
		legacyCalls++
		return conversationSummary{
				ConversationID: conversationID,
				Name:           "Legacy Selection",
				SourcePlatform: "sms",
			}, messageSummary{
				MessageID:      "legacy-selection-message",
				ConversationID: conversationID,
				Body:           body,
				TimestampMS:    1_700_000_001_000,
				IsFromMe:       true,
				SourcePlatform: "sms",
			}, nil
	}
	t.Cleanup(func() { sendTextToConversation = originalSend })

	legacyTools := map[string]mcp.Tool{
		"send_message":               sendMessageTool(),
		"send_to_conversation":       sendToConversationTool(),
		"send_media_to_conversation": sendMediaToConversationTool(),
		"send_group_message":         sendGroupMessageTool(),
	}
	tests := []struct {
		name string
		deps *V2Dependencies
	}{
		{name: "nil", deps: nil},
		{name: "disabled", deps: &V2Dependencies{Enabled: false}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			a := testApp(t)
			s := mcpserver.NewMCPServer("legacy-selection", "test")
			Register(s, a, test.deps)

			for name, want := range legacyTools {
				registered := s.GetTool(name)
				if registered == nil {
					t.Fatalf("registered tool %q not found", name)
				}
				gotJSON, err := json.Marshal(registered.Tool)
				if err != nil {
					t.Fatalf("marshal registered %s: %v", name, err)
				}
				wantJSON, err := json.Marshal(want)
				if err != nil {
					t.Fatalf("marshal legacy %s: %v", name, err)
				}
				if string(gotJSON) != string(wantJSON) {
					t.Fatalf("registered %s selected a non-legacy descriptor\n got: %s\nwant: %s", name, gotJSON, wantJSON)
				}
			}

			sendTool := s.GetTool("send_to_conversation")
			result, err := sendTool.Handler(context.Background(), legacyCallRequest("send_to_conversation", map[string]any{
				"conversation_id": "legacy-selection-conversation",
				"message":         "stay legacy",
			}))
			if err != nil {
				t.Fatalf("registered legacy handler: %v", err)
			}
			if result.IsError {
				t.Fatalf("registered legacy handler returned error: %v", result.Content)
			}
			payload := structuredMap(t, result)
			conversation, ok := payload["conversation"].(conversationSummary)
			if !ok || conversation.Name != "Legacy Selection" {
				t.Fatalf("registered handler did not select legacy response: %#v", payload)
			}
		})
	}
	if legacyCalls != len(tests) {
		t.Fatalf("legacy handler calls = %d, want %d", legacyCalls, len(tests))
	}
}

func TestLegacySendMessageExchangeParity(t *testing.T) {
	a := testApp(t)
	original := sendSignalText
	sendSignalText = func(_ *app.App, conversationID, body, replyToID string) (*db.Message, error) {
		if conversationID != "signal:+15551230000" || body != "legacy hello" || replyToID != "" {
			t.Fatalf("legacy send args = (%q, %q, %q)", conversationID, body, replyToID)
		}
		return &db.Message{
			MessageID:      "signal:legacy-parity-1",
			ConversationID: conversationID,
			Body:           body,
			TimestampMS:    1_700_000_000_123,
			Status:         "sent",
			IsFromMe:       true,
			SourcePlatform: "signal",
		}, nil
	}
	t.Cleanup(func() { sendSignalText = original })

	req := legacyCallRequest("send_message", map[string]any{
		"message":   "legacy hello",
		"platform":  "signal",
		"recipient": "+15551230000",
	})
	assertLegacyJSON(t, req, `{"method":"tools/call","params":{"name":"send_message","arguments":{"message":"legacy hello","platform":"signal","recipient":"+15551230000"}}}`)

	result, err := sendMessageHandler(a)(context.Background(), req)
	if err != nil {
		t.Fatalf("sendMessageHandler() error = %v", err)
	}
	assertLegacyJSON(t, result, `{"content":[{"type":"text","text":"Message sent to +15551230000: legacy hello"}],"structuredContent":{"conversation":{"conversation_id":"signal:+15551230000","name":"+15551230000","source_platform":"signal","is_group":false},"message":{"message_id":"signal:legacy-parity-1","conversation_id":"signal:+15551230000","body":"legacy hello","timestamp_ms":1700000000123,"status":"sent","is_from_me":true,"source_platform":"signal","display_text":"legacy hello"},"ok":true}}`)
}

func TestLegacySendToConversationExchangeParity(t *testing.T) {
	a := testApp(t)
	original := sendTextToConversation
	sendTextToConversation = func(_ *app.App, conversationID, body string) (conversationSummary, messageSummary, error) {
		if conversationID != "signal:legacy-thread" || body != "hello thread" {
			t.Fatalf("legacy send args = (%q, %q)", conversationID, body)
		}
		return conversationSummary{
				ConversationID: conversationID,
				Name:           "Legacy Thread",
				SourcePlatform: "signal",
				IsGroup:        true,
			}, messageSummary{
				MessageID:      "signal:legacy-parity-2",
				ConversationID: conversationID,
				Body:           body,
				TimestampMS:    1_700_000_000_456,
				Status:         "sent",
				IsFromMe:       true,
				SourcePlatform: "signal",
				DisplayText:    body,
			}, nil
	}
	t.Cleanup(func() { sendTextToConversation = original })

	req := legacyCallRequest("send_to_conversation", map[string]any{
		"conversation_id": "signal:legacy-thread",
		"message":         "hello thread",
	})
	assertLegacyJSON(t, req, `{"method":"tools/call","params":{"name":"send_to_conversation","arguments":{"conversation_id":"signal:legacy-thread","message":"hello thread"}}}`)

	result, err := sendToConversationHandler(a)(context.Background(), req)
	if err != nil {
		t.Fatalf("sendToConversationHandler() error = %v", err)
	}
	assertLegacyJSON(t, result, `{"content":[{"type":"text","text":"Message sent to Legacy Thread (signal:legacy-thread): hello thread"}],"structuredContent":{"conversation":{"conversation_id":"signal:legacy-thread","name":"Legacy Thread","source_platform":"signal","is_group":true},"message":{"message_id":"signal:legacy-parity-2","conversation_id":"signal:legacy-thread","body":"hello thread","timestamp_ms":1700000000456,"status":"sent","is_from_me":true,"source_platform":"signal","display_text":"hello thread"},"ok":true,"platform":"signal"}}`)
}

func TestLegacySendMediaExchangeParity(t *testing.T) {
	a := testApp(t)
	if err := a.Store.UpsertConversation(&db.Conversation{
		ConversationID: "signal:legacy-media",
		Name:           "Legacy Media",
		SourcePlatform: "signal",
	}); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}
	t.Chdir(t.TempDir())
	if err := os.WriteFile("legacy-parity.jpg", []byte("legacy jpeg bytes"), 0o600); err != nil {
		t.Fatalf("write media fixture: %v", err)
	}

	original := sendSignalMediaMessage
	sendSignalMediaMessage = func(_ *app.App, conversationID string, data []byte, filename, mimeType, caption, replyToID string) (*db.Message, error) {
		if conversationID != "signal:legacy-media" || string(data) != "legacy jpeg bytes" ||
			filename != "legacy-parity.jpg" || mimeType != "image/jpeg" ||
			caption != "legacy caption" || replyToID != "signal:parent" {
			t.Fatalf("legacy media args = (%q, %q, %q, %q, %q, %q)", conversationID, data, filename, mimeType, caption, replyToID)
		}
		return &db.Message{
			MessageID:      "signal:legacy-parity-media",
			ConversationID: conversationID,
			Body:           caption,
			TimestampMS:    1_700_000_000_789,
			Status:         "sent",
			IsFromMe:       true,
			MediaID:        "signalatt:legacy-parity-media",
			MimeType:       mimeType,
			ReplyToID:      replyToID,
			SourcePlatform: "signal",
		}, nil
	}
	t.Cleanup(func() { sendSignalMediaMessage = original })

	req := legacyCallRequest("send_media_to_conversation", map[string]any{
		"caption":         "legacy caption",
		"conversation_id": "signal:legacy-media",
		"file_path":       "legacy-parity.jpg",
		"mime_type":       "image/jpeg",
		"reply_to_id":     "signal:parent",
	})
	assertLegacyJSON(t, req, `{"method":"tools/call","params":{"name":"send_media_to_conversation","arguments":{"caption":"legacy caption","conversation_id":"signal:legacy-media","file_path":"legacy-parity.jpg","mime_type":"image/jpeg","reply_to_id":"signal:parent"}}}`)

	result, err := sendMediaToConversationHandler(a)(context.Background(), req)
	if err != nil {
		t.Fatalf("sendMediaToConversationHandler() error = %v", err)
	}
	assertLegacyJSON(t, result, `{"content":[{"type":"text","text":"Media sent to Legacy Media (signal:legacy-media): legacy-parity.jpg"}]}`)
}

func TestLegacySendGroupExchangeParity(t *testing.T) {
	a := testApp(t)
	req := legacyCallRequest("send_group_message", map[string]any{
		"phone_numbers": `["+15551234567"]`,
		"message":       "legacy group",
	})
	assertLegacyJSON(t, req, `{"method":"tools/call","params":{"name":"send_group_message","arguments":{"message":"legacy group","phone_numbers":"[\"+15551234567\"]"}}}`)

	result, err := sendGroupMessageHandler(a)(context.Background(), req)
	if err != nil {
		t.Fatalf("sendGroupMessageHandler() error = %v", err)
	}
	assertLegacyJSON(t, result, `{"content":[{"type":"text","text":"phone_numbers must contain at least 2 numbers for a group message"}],"isError":true,"structuredContent":{"error":"phone_numbers must contain at least 2 numbers for a group message","ok":false}}`)
}

func legacyCallRequest(name string, arguments map[string]any) mcp.CallToolRequest {
	req := mcp.CallToolRequest{}
	req.Method = string(mcp.MethodToolsCall)
	req.Params.Name = name
	req.Params.Arguments = arguments
	return req
}

func assertLegacyJSON(t *testing.T, value any, want string) {
	t.Helper()
	got, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("json.Marshal(%T): %v", value, err)
	}
	if string(got) != want {
		t.Fatalf("legacy JSON changed\n got: %s\nwant: %s", got, want)
	}
}
