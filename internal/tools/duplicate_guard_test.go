package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/localapi"
	"github.com/maxghenis/openmessage/internal/messaging"
)

// In-process MCP (entry point: guard on). A replay of the first key after a
// forced near-duplicate sibling must report the original durable send, not
// "NOT QUEUED".
func TestV2SendSameKeyReplayAfterForcedSiblingReturnsOriginal(t *testing.T) {
	harness := newV2ToolHarness(t,
		v2ToolSendStep{result: bridge.SendResult{RemoteMessageID: "remote-replay-1"}},
		v2ToolSendStep{result: bridge.SendResult{RemoteMessageID: "remote-replay-2"}},
	)
	handler := sendToConversationHandler(harness.app, &harness.deps)
	send := func(key string, force bool) *mcp.CallToolResult {
		t.Helper()
		result, err := handler(context.Background(), v2ToolCall(map[string]any{
			"conversation_id": v2ToolConversationID,
			"message":         "Lunch tomorrow at noon at Sfoglina?",
			"idempotency_key": key,
			"force":           force,
		}))
		if err != nil {
			t.Fatalf("handler(%s): %v", key, err)
		}
		return result
	}

	first := send("replay-first", false)
	if first.IsError {
		t.Fatalf("first send failed: %v", first.Content)
	}
	firstOutbox := v2ToolString(v2ToolPayload(t, first), "outbox_id")
	if forced := send("replay-forced-sibling", true); forced.IsError {
		t.Fatalf("forced sibling failed: %v", forced.Content)
	}

	replay := send("replay-first", false)
	if replay.IsError {
		t.Fatalf("replay of the first key was refused: %v", replay.Content)
	}
	payload := v2ToolPayload(t, replay)
	if got := v2ToolString(payload, "outbox_id"); got != firstOutbox {
		t.Fatalf("replay outbox_id = %q, want the original %q", got, firstOutbox)
	}
	assertV2ToolBool(t, payload, "deduplicated", true)
	if text := replay.Content[0].(mcp.TextContent).Text; strings.Contains(text, "NOT QUEUED") {
		t.Fatalf("replay reported NOT QUEUED for a durably queued send: %q", text)
	}
}

// fakeDuplicateDaemon is a v2 daemon whose submit route records each request
// body and answers with the scripted status and body.
func fakeDuplicateDaemon(t *testing.T, status int, body string) (*localapi.Client, func() []map[string]any) {
	t.Helper()
	var mu sync.Mutex
	var requests []map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"v2_send": true, "v2_primary": true})
	})
	mux.HandleFunc("/api/v1/outbox/messages", func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode submission: %v", err)
		}
		mu.Lock()
		requests = append(requests, request)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	})
	mux.HandleFunc("/api/v1/outbox/out-1", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"outbox_id": "out-1", "state": "confirmed", "remote_message_id": "remote-1"})
	})
	return daemonClientFor(t, mux), func() []map[string]any {
		mu.Lock()
		defer mu.Unlock()
		return append([]map[string]any(nil), requests...)
	}
}

func callDaemonSendToConversation(t *testing.T, client *localapi.Client, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	request := mcp.CallToolRequest{}
	request.Params.Arguments = args
	result, err := daemonSendToConversationHandler(Options{Daemon: client})(context.Background(), request)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	return result
}

// Daemon-mode MCP (entry point: guard on): every text submission asks the
// daemon for the guard, and force passes through.
func TestDaemonSendRequestsNearDuplicateGuard(t *testing.T) {
	client, requests := fakeDuplicateDaemon(t, http.StatusOK, `{"outbox_id":"out-1","state":"queued"}`)
	for _, force := range []bool{false, true} {
		result := callDaemonSendToConversation(t, client, map[string]any{
			"conversation_id": "conv-1",
			"message":         "hello",
			"idempotency_key": fmt.Sprintf("key-force-%v", force),
			"force":           force,
		})
		if result.IsError {
			t.Fatalf("send failed: %v", result.Content)
		}
	}
	got := requests()
	if len(got) != 2 {
		t.Fatalf("requests = %d, want 2", len(got))
	}
	for index, request := range got {
		if request["guard_near_duplicates"] != true {
			t.Fatalf("request %d guard_near_duplicates = %v, want true", index, request["guard_near_duplicates"])
		}
	}
	if _, present := got[0]["force"]; present {
		t.Fatalf("unforced request carries force: %v", got[0])
	}
	if got[1]["force"] != true {
		t.Fatalf("forced request force = %v, want true", got[1]["force"])
	}
}

func TestDaemonStructuredDuplicateRejection(t *testing.T) {
	client, _ := fakeDuplicateDaemon(t, http.StatusConflict,
		`{"error":"messaging: near-duplicate send blocked: …","error_kind":"near_duplicate_blocked","duplicate_of_outbox_id":"outbox-prior","duplicate_state":"uncertain","duplicate_age_ms":95000}`)
	result := callDaemonSendToConversation(t, client, map[string]any{
		"conversation_id": "conv-1",
		"message":         "hello again",
		"idempotency_key": "key-structured",
	})
	if !result.IsError {
		t.Fatalf("near-duplicate refusal must be an error result (nothing was queued): %v", result.Content)
	}
	payload := structuredMap(t, result)
	if payload["error_kind"] != localapi.NearDuplicateErrorKind ||
		payload["duplicate_of_outbox_id"] != "outbox-prior" ||
		payload["duplicate_state"] != "uncertain" {
		t.Fatalf("payload = %v, want the structured prior", payload)
	}
	if age, _ := payload["duplicate_age_ms"].(int64); age != 95_000 {
		t.Fatalf("duplicate_age_ms = %v, want 95000", payload["duplicate_age_ms"])
	}
	text := result.Content[0].(mcp.TextContent).Text
	for _, fragment := range []string{"NOT QUEUED", "outbox-prior", "1m35s ago", "force=true"} {
		if !strings.Contains(text, fragment) {
			t.Fatalf("text missing %q: %q", fragment, text)
		}
	}
}

// A daemon that predates the structured 409 is still recognized by its
// wording, without inventing a prior outbox ID.
func TestDaemonLegacyDuplicateRejectionStillRecognized(t *testing.T) {
	client, _ := fakeDuplicateDaemon(t, http.StatusConflict,
		`{"error":"messaging: near-duplicate send blocked: a very similar message was submitted to this conversation 2m0s ago (outbox old-1, state queued); if this is intentional, resubmit with force"}`)
	result := callDaemonSendToConversation(t, client, map[string]any{
		"conversation_id": "conv-1",
		"message":         "hello again",
		"idempotency_key": "key-legacy",
	})
	if !result.IsError {
		t.Fatalf("legacy near-duplicate refusal must be an error result: %v", result.Content)
	}
	payload := structuredMap(t, result)
	if payload["error_kind"] != localapi.NearDuplicateErrorKind {
		t.Fatalf("error_kind = %v, want %q", payload["error_kind"], localapi.NearDuplicateErrorKind)
	}
	if _, present := payload["duplicate_of_outbox_id"]; present {
		t.Fatalf("legacy refusal invented a prior outbox: %v", payload)
	}
	if text := result.Content[0].(mcp.TextContent).Text; !strings.Contains(text, "outbox old-1") || !strings.Contains(text, "force=true") {
		t.Fatalf("legacy text = %q, want the daemon's wording and the force guidance", text)
	}
}

// An idempotency-key conflict is also a 409, but it is not a near-duplicate
// and must not be rendered as one.
func TestDaemonIdempotencyConflictIsNotNearDuplicate(t *testing.T) {
	client, _ := fakeDuplicateDaemon(t, http.StatusConflict, `{"error":"enqueue outbox item: outbox idempotency conflict"}`)
	result := callDaemonSendToConversation(t, client, map[string]any{
		"conversation_id": "conv-1",
		"message":         "hello",
		"idempotency_key": "key-conflict",
	})
	if !result.IsError {
		t.Fatalf("conflict must be an error result: %v", result.Content)
	}
	payload := structuredMap(t, result)
	if payload["error_kind"] == localapi.NearDuplicateErrorKind {
		t.Fatalf("idempotency conflict rendered as a near-duplicate: %v", payload)
	}
}

func TestInProcessAndDaemonDuplicateResultsAgree(t *testing.T) {
	inProcess := structuredMap(t, duplicateBlockedResult(&messaging.DuplicateSendError{
		PriorOutboxID: "outbox-prior",
		PriorState:    messaging.OutboxQueued,
		PriorAgeMS:    60_000,
	}))
	daemon := structuredMap(t, daemonDuplicateBlockedResult(localapi.NearDuplicateRejection{
		Structured:          true,
		DuplicateOfOutboxID: "outbox-prior",
		DuplicateState:      "queued",
		DuplicateAgeMS:      60_000,
	}))
	if fmt.Sprint(inProcess) != fmt.Sprint(daemon) {
		t.Fatalf("in-process and daemon near-duplicate results differ:\n in-process %v\n daemon     %v", inProcess, daemon)
	}
}

// The force argument and the tool description state the guard's real window
// and that same-key replays are never blocked.
func TestForceDescriptionMatchesGuardWindow(t *testing.T) {
	window := fmt.Sprintf("%d minutes", int(messaging.DefaultDuplicateWindow/time.Minute))
	tool := sendToConversationTool(true)
	force, _ := tool.InputSchema.Properties["force"].(map[string]any)
	description, _ := force["description"].(string)
	if !strings.Contains(description, window) || !strings.Contains(description, "same idempotency_key") {
		t.Fatalf("force description = %q, want the %s window and the replay guarantee", description, window)
	}
	if !strings.Contains(tool.Description, "within "+window+" are blocked unless force=true") {
		t.Fatalf("tool description does not state the %s guard window: %q", window, tool.Description)
	}
}
