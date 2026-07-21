package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/localapi"
)

func daemonClientFor(t *testing.T, handler http.Handler) *localapi.Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return &localapi.Client{BaseURL: server.URL, HTTP: server.Client()}
}

func deadDaemonClient() *localapi.Client {
	return localapi.NewClient("http://127.0.0.1:1", "")
}

func v2DaemonHandler(t *testing.T, deliveryState string) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"v2_send": true, "v2_primary": true, "connected": true})
	})
	mux.HandleFunc("/api/v1/outbox/messages", func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode submission: %v", err)
		}
		if request["conversation_id"] == "" || request["idempotency_key"] == "" {
			t.Errorf("submission missing fields: %v", request)
		}
		json.NewEncoder(w).Encode(map[string]any{"outbox_id": "out-1", "state": "queued"})
	})
	mux.HandleFunc("/api/v1/outbox/out-1", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"outbox_id":         "out-1",
			"state":             deliveryState,
			"remote_message_id": "remote-9",
		})
	})
	return mux
}

func TestDaemonSendToConversationConfirmed(t *testing.T) {
	options := Options{Daemon: daemonClientFor(t, v2DaemonHandler(t, "confirmed"))}
	handler := daemonSendToConversationHandler(options)

	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"conversation_id": "conv-1",
		"message":         "hello",
		"idempotency_key": "key-123",
	}
	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if result.IsError {
		t.Fatalf("expected success, got error result: %+v", result)
	}
	payload := structuredMap(t, result)
	if payload["ok"] != true {
		t.Fatalf("payload.ok = %v, want true", payload["ok"])
	}
	if payload["outbox_id"] != "out-1" {
		t.Fatalf("payload.outbox_id = %v", payload["outbox_id"])
	}
	if payload["idempotency_key"] != "key-123" {
		t.Fatalf("payload.idempotency_key = %v", payload["idempotency_key"])
	}
	if payload["remote_message_id"] != "remote-9" {
		t.Fatalf("payload.remote_message_id = %v", payload["remote_message_id"])
	}
}

func TestDaemonSendToConversationDaemonDown(t *testing.T) {
	options := Options{Daemon: deadDaemonClient()}
	handler := daemonSendToConversationHandler(options)

	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{"conversation_id": "conv-1", "message": "hello"}
	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected error result when the daemon is down")
	}
	payload := structuredMap(t, result)
	message, _ := payload["error"].(string)
	if !strings.Contains(message, "isn't running") || !strings.Contains(message, "transportless") {
		t.Fatalf("daemon-down error is not actionable: %q", message)
	}
}

func TestDaemonSendToConversationDeterministicRejection(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"v2_send": true})
	})
	mux.HandleFunc("/api/v1/outbox/messages", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "conversation not found", http.StatusNotFound)
	})
	options := Options{Daemon: daemonClientFor(t, mux)}
	handler := daemonSendToConversationHandler(options)

	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{"conversation_id": "missing", "message": "hello"}
	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected deterministic rejection to be an error result")
	}
	payload := structuredMap(t, result)
	message, _ := payload["error"].(string)
	if !strings.Contains(message, "send rejected by the app") {
		t.Fatalf("rejection message = %q", message)
	}
}

func TestDaemonSendToConversationAmbiguousSubmitIsNotAnErrorResult(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"v2_send": true})
	})
	mux.HandleFunc("/api/v1/outbox/messages", func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("response writer does not support hijacking")
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Fatalf("hijack: %v", err)
		}
		conn.Close()
	})
	options := Options{Daemon: daemonClientFor(t, mux)}
	handler := daemonSendToConversationHandler(options)

	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"conversation_id": "conv-1",
		"message":         "hello",
		"idempotency_key": "replay-key",
	}
	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if result.IsError {
		t.Fatal("ambiguous submission must not be an error result (it invites a blind resend)")
	}
	payload := structuredMap(t, result)
	if payload["ambiguous"] != true {
		t.Fatalf("payload.ambiguous = %v, want true", payload["ambiguous"])
	}
	if payload["idempotency_key"] != "replay-key" {
		t.Fatalf("payload.idempotency_key = %v, want replay-key", payload["idempotency_key"])
	}
}

func TestDaemonSendToConversationLegacyDaemon(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"connected": true})
	})
	mux.HandleFunc("/api/send", func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode legacy send: %v", err)
		}
		if request["conversation_id"] != "conv-legacy" {
			t.Errorf("legacy send conversation_id = %v", request["conversation_id"])
		}
		json.NewEncoder(w).Encode(map[string]any{"message_id": "msg-7", "status": "SUCCESS", "success": true})
	})
	options := Options{Daemon: daemonClientFor(t, mux)}
	handler := daemonSendToConversationHandler(options)

	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{"conversation_id": "conv-legacy", "message": "hello"}
	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if result.IsError {
		t.Fatalf("expected success, got error result: %+v", result)
	}
	payload := structuredMap(t, result)
	if payload["ok"] != true || payload["message_id"] != "msg-7" {
		t.Fatalf("legacy payload = %v", payload)
	}
}

func TestDaemonSendMessageResolvesPlatformsWithoutTransports(t *testing.T) {
	options := Options{Daemon: daemonClientFor(t, v2DaemonHandler(t, "confirmed"))}
	handler := daemonSendMessageHandler(options)

	t.Run("whatsapp recipient", func(t *testing.T) {
		req := mcp.CallToolRequest{}
		req.Params.Arguments = map[string]any{
			"recipient": "+16505550100",
			"platform":  "whatsapp",
			"message":   "hi",
		}
		result, err := handler(context.Background(), req)
		if err != nil {
			t.Fatalf("handler: %v", err)
		}
		if result.IsError {
			t.Fatalf("expected success, got %+v", result)
		}
	})

	t.Run("sms without existing conversation", func(t *testing.T) {
		store, err := db.New(":memory:")
		if err != nil {
			t.Fatalf("create db: %v", err)
		}
		t.Cleanup(func() { store.Close() })
		withReads := options
		withReads.Reads = store
		handler := daemonSendMessageHandler(withReads)

		req := mcp.CallToolRequest{}
		req.Params.Arguments = map[string]any{
			"recipient": "+16505550100",
			"message":   "hi",
		}
		result, err := handler(context.Background(), req)
		if err != nil {
			t.Fatalf("handler: %v", err)
		}
		if !result.IsError {
			t.Fatal("expected error result for a brand-new SMS thread in client mode")
		}
		payload := structuredMap(t, result)
		message, _ := payload["error"].(string)
		if !strings.Contains(message, "no existing SMS conversation") {
			t.Fatalf("sms error = %q", message)
		}
	})

	t.Run("sms with existing conversation", func(t *testing.T) {
		store, err := db.New(":memory:")
		if err != nil {
			t.Fatalf("create db: %v", err)
		}
		t.Cleanup(func() { store.Close() })
		if err := store.UpsertConversation(&db.Conversation{
			ConversationID: "conv-1",
			Name:           "Alice",
			Participants:   `[{"name":"Alice","number":"+1 (650) 555-0100"}]`,
			SourcePlatform: "sms",
		}); err != nil {
			t.Fatalf("seed conversation: %v", err)
		}
		withReads := options
		withReads.Reads = store
		handler := daemonSendMessageHandler(withReads)

		req := mcp.CallToolRequest{}
		req.Params.Arguments = map[string]any{
			"recipient": "6505550100",
			"message":   "hi",
		}
		result, err := handler(context.Background(), req)
		if err != nil {
			t.Fatalf("handler: %v", err)
		}
		if result.IsError {
			t.Fatalf("expected success via existing conversation, got %+v", result)
		}
		payload := structuredMap(t, result)
		if payload["ok"] != true {
			t.Fatalf("payload = %v", payload)
		}
	})
}

func TestDaemonSendGroupMessageAlwaysDirectsToApp(t *testing.T) {
	handler := daemonSendGroupMessageHandler()
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{"phone_numbers": []any{"+15551", "+15552"}, "message": "hi"}
	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected group creation to be refused in client mode")
	}
}

func TestDaemonReactToMessage(t *testing.T) {
	t.Run("routes through the app", func(t *testing.T) {
		reacted := false
		mux := http.NewServeMux()
		mux.HandleFunc("/api/react", func(w http.ResponseWriter, r *http.Request) {
			reacted = true
			var request map[string]any
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode react: %v", err)
			}
			if request["message_id"] != "msg-1" || request["emoji"] != "👍" {
				t.Errorf("react payload = %v", request)
			}
			json.NewEncoder(w).Encode(map[string]any{"success": true})
		})
		options := Options{Daemon: daemonClientFor(t, mux)}
		handler := daemonReactToMessageHandler(options)

		req := mcp.CallToolRequest{}
		req.Params.Arguments = map[string]any{"message_id": "msg-1", "emoji": "👍"}
		result, err := handler(context.Background(), req)
		if err != nil {
			t.Fatalf("handler: %v", err)
		}
		if result.IsError {
			t.Fatalf("expected success, got %+v", result)
		}
		if !reacted {
			t.Fatal("daemon /api/react was never called")
		}
	})

	t.Run("daemon down", func(t *testing.T) {
		options := Options{Daemon: deadDaemonClient()}
		handler := daemonReactToMessageHandler(options)
		req := mcp.CallToolRequest{}
		req.Params.Arguments = map[string]any{"message_id": "msg-1", "emoji": "👍"}
		result, err := handler(context.Background(), req)
		if err != nil {
			t.Fatalf("handler: %v", err)
		}
		if !result.IsError {
			t.Fatal("expected error result when the daemon is down")
		}
	})
}

func TestDaemonGetStatus(t *testing.T) {
	t.Run("daemon reachable serves daemon truth", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(map[string]any{
				"connected":  true,
				"v2_primary": true,
				"v2_send":    true,
				"whatsapp":   map[string]any{"connected": true, "paired": true},
			})
		})
		a := testApp(t)
		options := Options{Reads: a.Store, Daemon: daemonClientFor(t, mux)}
		handler := daemonGetStatusHandler(a, options)

		result, err := handler(context.Background(), mcp.CallToolRequest{})
		if err != nil {
			t.Fatalf("handler: %v", err)
		}
		if result.IsError {
			t.Fatalf("expected success, got %+v", result)
		}
		payload := structuredMap(t, result)
		if payload["daemon_reachable"] != true || payload["mcp_mode"] != "client" {
			t.Fatalf("payload = %v", payload)
		}
		text := resultText(t, result)
		if !strings.Contains(text, "RUNNING") || !strings.Contains(text, "WhatsApp: connected=true") {
			t.Fatalf("status text = %q", text)
		}
	})

	t.Run("daemon down reports local-only mode", func(t *testing.T) {
		a := testApp(t)
		options := Options{Reads: a.Store, Daemon: deadDaemonClient()}
		handler := daemonGetStatusHandler(a, options)

		result, err := handler(context.Background(), mcp.CallToolRequest{})
		if err != nil {
			t.Fatalf("handler: %v", err)
		}
		if result.IsError {
			t.Fatalf("status must not be an error result when the app is closed, got %+v", result)
		}
		payload := structuredMap(t, result)
		if payload["daemon_reachable"] != false {
			t.Fatalf("payload = %v", payload)
		}
		text := resultText(t, result)
		if !strings.Contains(text, "NOT RUNNING") {
			t.Fatalf("status text = %q", text)
		}
	})
}
