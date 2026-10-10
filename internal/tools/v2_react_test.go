package tools

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/maxghenis/openmessage/internal/app"
	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

const (
	v2ReactConversationID = "v2-react-conversation"
	v2ReactMessageID      = "v2-react-target"
)

// v2ToolReactionSender records each reaction and answers with the next
// scripted step, accepting once the script runs out.
type v2ToolReactionSender struct {
	mu       sync.Mutex
	steps    []v2ToolSendStep
	requests []bridge.ReactionRequest
}

func (s *v2ToolReactionSender) SendReaction(_ context.Context, request bridge.ReactionRequest) (bridge.SendResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, request)
	if len(s.steps) == 0 {
		return bridge.SendResult{AcceptedAt: time.Now()}, nil
	}
	step := s.steps[0]
	s.steps = s.steps[1:]
	return step.result, step.err
}

func (s *v2ToolReactionSender) snapshotRequests() []bridge.ReactionRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]bridge.ReactionRequest(nil), s.requests...)
}

// newV2ReactToolHarness is a v2-primary MCP surface over a v2 store holding
// one conversation and one incoming message, whose transport can react.
func newV2ReactToolHarness(t *testing.T, steps ...v2ToolSendStep) (*v2ToolHarness, *v2ToolReactionSender, *server.MCPServer) {
	t.Helper()
	harness := newV2ToolHarness(t)
	sender := &v2ToolReactionSender{steps: steps}
	harness.deps.Registry.(*v2ToolRegistry).reaction = sender

	store := harness.deps.V2Store
	nowMS := time.Now().UnixMilli()
	if err := store.UpsertAccount(sqlite.Account{
		AccountID: "google-primary", BridgeKey: "google_messages", DisplayName: "Google",
		Mode: sqlite.AccountModeLive, Enabled: true, ConfigJSON: "{}", CreatedAtMS: nowMS, UpdatedAtMS: nowMS,
	}); err != nil {
		t.Fatalf("UpsertAccount(): %v", err)
	}
	if err := store.UpsertConversation(sqlite.Conversation{
		ConversationID: v2ReactConversationID, AccountID: "google-primary", RemoteConversationID: "remote-react-thread",
		Kind: sqlite.ConversationKindDirect, Title: "React thread", NotificationMode: sqlite.NotificationModeAll,
		MetadataJSON: "{}", CreatedAtMS: nowMS, UpdatedAtMS: nowMS,
	}); err != nil {
		t.Fatalf("UpsertConversation(): %v", err)
	}
	messages, err := sqlite.NewMessageRepository(store, time.Now)
	if err != nil {
		t.Fatalf("NewMessageRepository(): %v", err)
	}
	if err := messages.ImportMessage(context.Background(), sqlite.MessageProjection{Message: sqlite.Message{
		MessageID: v2ReactMessageID, ConversationID: v2ReactConversationID, AccountID: "google-primary",
		RemoteMessageID: "remote-react-target", Direction: sqlite.MessageDirectionIncoming,
		Body: "react to this", State: sqlite.MessageStateActive, OccurredAtMS: nowMS - 60_000,
	}}); err != nil {
		t.Fatalf("ImportMessage(): %v", err)
	}

	// The legacy senders read the legacy store, which holds none of these
	// IDs; a v2-primary reaction must never reach them.
	originalWhatsApp, originalSignal := sendWhatsAppReactionMessage, sendSignalReactionMessage
	failLegacy := func(*app.App, string, string, string, string) error {
		t.Error("a v2-primary reaction reached a legacy sender")
		return errors.New("legacy sender")
	}
	sendWhatsAppReactionMessage, sendSignalReactionMessage = failLegacy, failLegacy
	t.Cleanup(func() {
		sendWhatsAppReactionMessage, sendSignalReactionMessage = originalWhatsApp, originalSignal
	})

	mcpServer := server.NewMCPServer("v2-react-test", "test")
	deps := harness.deps
	RegisterWithOptions(mcpServer, harness.app, Options{Reads: harness.app.Store, V2Primary: true, V2: &deps})
	return harness, sender, mcpServer
}

func callReactTool(t *testing.T, mcpServer *server.MCPServer, arguments map[string]any) *mcp.CallToolResult {
	t.Helper()
	tool := mcpServer.GetTool("react_to_message")
	if tool == nil {
		t.Fatal("react_to_message is not registered")
	}
	result, err := tool.Handler(context.Background(), v2ToolCall(arguments))
	if err != nil {
		t.Fatalf("react_to_message: %v", err)
	}
	return result
}

func TestV2ReactToMessageGoesThroughTheOutbox(t *testing.T) {
	_, sender, mcpServer := newV2ReactToolHarness(t)
	if _, ok := mcpServer.GetTool("react_to_message").Tool.InputSchema.Properties["idempotency_key"]; !ok {
		t.Fatal("the v2 react_to_message descriptor does not offer idempotency_key")
	}
	arguments := map[string]any{
		"conversation_id": v2ReactConversationID, "message_id": v2ReactMessageID,
		"emoji": "👍", "idempotency_key": "react-tool-key",
	}
	result := callReactTool(t, mcpServer, arguments)
	payload := v2ToolPayload(t, result)
	if result.IsError {
		t.Fatalf("react_to_message = error %v", payload)
	}
	assertV2ToolBool(t, payload, "ok", true)
	assertV2ToolBool(t, payload, "settled", true)
	assertV2ToolString(t, payload, "state", "confirmed")
	assertV2ToolString(t, payload, "idempotency_key", "react-tool-key")
	if requests := sender.snapshotRequests(); len(requests) != 1 ||
		requests[0].Target.RemoteID != "remote-react-target" || requests[0].Conversation.RemoteID != "remote-react-thread" ||
		requests[0].Emoji != "👍" || requests[0].Action != bridge.ReactionAdd {
		t.Fatalf("reaction requests = %+v", requests)
	}

	// The same key replays the first intent and sends nothing new.
	replay := v2ToolPayload(t, callReactTool(t, mcpServer, arguments))
	assertV2ToolBool(t, replay, "deduplicated", true)
	assertV2ToolString(t, replay, "outbox_id", v2ToolString(payload, "outbox_id"))
	if requests := sender.snapshotRequests(); len(requests) != 1 {
		t.Fatalf("a replayed reaction reached the transport again: %+v", requests)
	}
}

func TestV2ReactToMessageReportsAnUndeliveredReactionWithoutAnError(t *testing.T) {
	_, _, mcpServer := newV2ReactToolHarness(t, v2ToolSendStep{err: bridge.OpError{
		Class: bridge.FailureTransient, Operation: "send_reaction", Fingerprint: "disconnected",
		Dispatch: bridge.DispatchNotCalled,
	}})
	result := callReactTool(t, mcpServer, map[string]any{
		"conversation_id": v2ReactConversationID, "message_id": v2ReactMessageID, "emoji": "👍",
	})
	payload := v2ToolPayload(t, result)
	// An error result invites the agent to react again; the app retries it.
	if result.IsError {
		t.Fatalf("an automatically retrying reaction was reported as an error: %v", payload)
	}
	assertV2ToolBool(t, payload, "ok", false)
	assertV2ToolBool(t, payload, "settled", false)
	assertV2ToolBool(t, payload, "auto_retry", true)
	assertV2ToolString(t, payload, "state", "not_dispatched")
	if key := v2ToolString(payload, "idempotency_key"); key == "" {
		t.Fatalf("payload = %v, want the minted idempotency key", payload)
	}
	if text := resultText(t, result); !strings.Contains(text, "Do NOT react again") {
		t.Fatalf("text = %q", text)
	}
}

func TestV2ReactToMessageRefusesWhatItCannotPlace(t *testing.T) {
	_, sender, mcpServer := newV2ReactToolHarness(t)
	for name, arguments := range map[string]map[string]any{
		"a legacy message ID":       {"conversation_id": v2ReactConversationID, "message_id": "signal:1700000001000", "emoji": "👍"},
		"another conversation's ID": {"conversation_id": "v2-tool-conversation", "message_id": v2ReactMessageID, "emoji": "👍"},
		"a blank idempotency key":   {"conversation_id": v2ReactConversationID, "message_id": v2ReactMessageID, "emoji": "👍", "idempotency_key": " "},
	} {
		t.Run(name, func(t *testing.T) {
			if result := callReactTool(t, mcpServer, arguments); !result.IsError {
				t.Fatalf("react_to_message = %v, want an error", v2ToolPayload(t, result))
			}
		})
	}
	if requests := sender.snapshotRequests(); len(requests) != 0 {
		t.Fatalf("refused reactions reached the transport: %+v", requests)
	}
}

func TestV2PrimaryReactToMessageWithoutTheOutboxIsAnError(t *testing.T) {
	a := testApp(t)
	mcpServer := server.NewMCPServer("v2-react-unconfigured", "test")
	RegisterWithOptions(mcpServer, a, Options{Reads: a.Store, V2Primary: true})
	result := callReactTool(t, mcpServer, map[string]any{
		"conversation_id": v2ReactConversationID, "message_id": v2ReactMessageID, "emoji": "👍",
	})
	if !result.IsError || !strings.Contains(resultText(t, result), "v2 outbox") {
		t.Fatalf("react_to_message = %v, want an error naming the v2 outbox", resultText(t, result))
	}
}

// reactDaemon is a fake running app: its status and the reaction routes it
// answers are set per test, and it records which routes were called.
type reactDaemon struct {
	t      *testing.T
	status map[string]any
	mu     sync.Mutex
	calls  []string
	bodies []map[string]any
}

func (d *reactDaemon) record(route string, r *http.Request) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, route)
	if r.Body != nil && r.Method == http.MethodPost {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		d.bodies = append(d.bodies, body)
	}
}

func (d *reactDaemon) routes() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.calls...)
}

func (d *reactDaemon) handler(react, submit, delivery http.HandlerFunc) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(d.status)
	})
	wrap := func(route string, next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			d.record(route, r)
			if next == nil {
				d.t.Errorf("the daemon's %s was called", route)
				http.Error(w, "unexpected", http.StatusTeapot)
				return
			}
			next(w, r)
		}
	}
	mux.HandleFunc("POST /api/react", wrap("react", react))
	mux.HandleFunc("POST /api/v1/outbox/reactions", wrap("submit", submit))
	mux.HandleFunc("GET /api/v1/outbox/{id}", wrap("delivery", delivery))
	return mux
}

func answerJSON(status int, body map[string]any) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(body)
	}
}

func callDaemonReact(t *testing.T, mux http.Handler, arguments map[string]any) *mcp.CallToolResult {
	t.Helper()
	result, err := daemonReactToMessageHandler(Options{Daemon: daemonClientFor(t, mux)})(context.Background(), v2ToolCall(arguments))
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	return result
}

func TestDaemonReactToMessageOnAV2PrimaryApp(t *testing.T) {
	arguments := map[string]any{
		"conversation_id": "v2-conv", "message_id": "v2-msg", "emoji": "❤️", "action": "Switch",
		"idempotency_key": "client-react-key",
	}

	t.Run("queues on the outbox and follows it to delivery", func(t *testing.T) {
		daemon := &reactDaemon{t: t, status: map[string]any{"v2_primary": true, "v2_send": true}}
		mux := daemon.handler(nil,
			answerJSON(http.StatusOK, map[string]any{"outbox_id": "out-react", "state": "queued"}),
			answerJSON(http.StatusOK, map[string]any{"outbox_id": "out-react", "state": "confirmed"}))
		result := callDaemonReact(t, mux, arguments)
		payload := v2ToolPayload(t, result)
		if result.IsError {
			t.Fatalf("result = error %v", payload)
		}
		assertV2ToolBool(t, payload, "ok", true)
		assertV2ToolBool(t, payload, "settled", true)
		assertV2ToolString(t, payload, "outbox_id", "out-react")
		assertV2ToolString(t, payload, "idempotency_key", "client-react-key")
		if routes := daemon.routes(); len(routes) < 2 || routes[0] != "submit" || routes[len(routes)-1] != "delivery" {
			t.Fatalf("daemon routes = %v, want the outbox submit then delivery polls", routes)
		}
		want := map[string]any{
			"conversation_id": "v2-conv", "message_id": "v2-msg", "emoji": "❤️",
			"action": "switch", "idempotency_key": "client-react-key",
		}
		if got := daemon.bodies[0]; len(got) != len(want) || got["action"] != want["action"] ||
			got["message_id"] != want["message_id"] || got["idempotency_key"] != want["idempotency_key"] {
			t.Fatalf("submitted reaction = %v, want %v", got, want)
		}
	})

	t.Run("a reaction the app keeps retrying is not an error", func(t *testing.T) {
		daemon := &reactDaemon{t: t, status: map[string]any{"v2_primary": true}}
		mux := daemon.handler(nil,
			answerJSON(http.StatusOK, map[string]any{"outbox_id": "out-react", "state": "queued"}),
			answerJSON(http.StatusOK, map[string]any{"outbox_id": "out-react", "state": "not_dispatched", "error_class": "transient"}))
		result := callDaemonReact(t, mux, arguments)
		payload := v2ToolPayload(t, result)
		if result.IsError {
			t.Fatalf("result = error %v", payload)
		}
		assertV2ToolBool(t, payload, "settled", false)
		assertV2ToolBool(t, payload, "auto_retry", true)
	})

	t.Run("a refused reaction is an error", func(t *testing.T) {
		daemon := &reactDaemon{t: t, status: map[string]any{"v2_primary": true}}
		mux := daemon.handler(nil, answerJSON(http.StatusUnprocessableEntity, map[string]any{"error": "reaction_target_unavailable"}), nil)
		result := callDaemonReact(t, mux, arguments)
		if !result.IsError || !strings.Contains(resultText(t, result), "rejected by the app") {
			t.Fatalf("result = %q, want a rejection", resultText(t, result))
		}
	})

	t.Run("a lost answer is reported as unknown, not as an error", func(t *testing.T) {
		daemon := &reactDaemon{t: t, status: map[string]any{"v2_primary": true}}
		mux := daemon.handler(nil, func(w http.ResponseWriter, r *http.Request) {
			connection, _, err := http.NewResponseController(w).Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			_ = connection.Close()
		}, nil)
		result := callDaemonReact(t, mux, arguments)
		payload := v2ToolPayload(t, result)
		if result.IsError {
			t.Fatalf("a lost answer was reported as an error, which invites a second reaction: %v", payload)
		}
		assertV2ToolBool(t, payload, "ambiguous", true)
		assertV2ToolString(t, payload, "idempotency_key", "client-react-key")
	})
}

func TestDaemonReactToMessageOnALegacyPrimaryApp(t *testing.T) {
	arguments := map[string]any{"conversation_id": "signal:+15551234567", "message_id": "signal:1", "emoji": "👍"}

	t.Run("v2 send without v2 primary keeps reactions on /api/react", func(t *testing.T) {
		daemon := &reactDaemon{t: t, status: map[string]any{"v2_send": true, "v2_primary": false}}
		mux := daemon.handler(answerJSON(http.StatusOK, map[string]any{"success": true}), nil, nil)
		result := callDaemonReact(t, mux, arguments)
		if result.IsError {
			t.Fatalf("result = %q", resultText(t, result))
		}
		if routes := daemon.routes(); len(routes) != 1 || routes[0] != "react" {
			t.Fatalf("daemon routes = %v, want only /api/react", routes)
		}
	})

	t.Run("an app answering success false did not apply the reaction", func(t *testing.T) {
		daemon := &reactDaemon{t: t, status: map[string]any{}}
		mux := daemon.handler(answerJSON(http.StatusOK, map[string]any{"success": false}), nil, nil)
		if result := callDaemonReact(t, mux, arguments); !result.IsError {
			t.Fatalf("result = %q, want an error", resultText(t, result))
		}
	})

	t.Run("a queued answer on /api/react is reported as queued", func(t *testing.T) {
		// When the status probe fails the client falls back to /api/react,
		// which a v2-primary app answers with 202 while the reaction is queued.
		daemon := &reactDaemon{t: t, status: map[string]any{}}
		mux := daemon.handler(answerJSON(http.StatusAccepted, map[string]any{
			"success": true, "queued": true, "outbox_id": "out-compat", "state": "not_dispatched",
		}), nil, nil)
		result := callDaemonReact(t, mux, arguments)
		payload := v2ToolPayload(t, result)
		if result.IsError {
			t.Fatalf("result = error %v", payload)
		}
		assertV2ToolBool(t, payload, "settled", false)
		assertV2ToolString(t, payload, "outbox_id", "out-compat")
		if _, minted := payload["idempotency_key"]; minted {
			t.Fatalf("payload = %v invents an idempotency key the app never returned", payload)
		}
	})
}
