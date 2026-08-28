package tools

// Incident-derived contract tests (2026-08-05→06): send_message reported
// {ok:true, settled:true, state:"confirmed"} for a message that sat ~15 hours
// before transmitting, a retry then double-texted the recipient, and a
// WhatsApp send 404ed while get_status showed the platform connected. These
// tests pin the corrected contract end to end at the MCP tool layer.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/localapi"
	"github.com/maxghenis/openmessage/internal/messaging"
)

// TestDaemonQueuedSendNeverReportsSettledOrConfirmed is the core incident
// invariant: while the daemon holds the message in its outbox, the tool
// result must say queued/untransmitted — never settled, never ok, never any
// "confirmed" language.
func TestDaemonQueuedSendNeverReportsSettledOrConfirmed(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"v2_send": true})
	})
	mux.HandleFunc("/api/v1/outbox/messages", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"outbox_id":        "outbox-stuck",
			"local_message_id": "local-stuck",
			"state":            "queued",
			"scheduled_for_ms": time.Now().UnixMilli(),
		})
	})
	// The outbox item never leaves queued — the overnight incident shape.
	mux.HandleFunc("/api/v1/outbox/outbox-stuck", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"outbox_id":       "outbox-stuck",
			"conversation_id": "conversation-stuck",
			"platform":        "sms",
			"state":           "queued",
			"expires_at_ms":   time.Now().Add(10 * time.Minute).UnixMilli(),
		})
	})
	options := Options{Daemon: daemonClientFor(t, mux)}
	handler := daemonSendToConversationHandler(options)

	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"conversation_id": "conversation-stuck",
		"message":         "meet for lunch tomorrow?",
		"idempotency_key": "queued-truth-key",
		"wait_seconds":    float64(1),
	}
	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if result.IsError {
		t.Fatalf("queued send returned a tool error, inviting a resend: %v", result.Content)
	}

	payload := structuredMap(t, result)
	if got, _ := payload["settled"].(bool); got {
		t.Fatalf("settled = true for a queued-not-transmitted send; payload=%v", payload)
	}
	if got, _ := payload["ok"].(bool); got {
		t.Fatalf("ok = true for a queued-not-transmitted send; payload=%v", payload)
	}
	if got, _ := payload["transmitted"].(bool); got {
		t.Fatalf("transmitted = true for a queued send; payload=%v", payload)
	}
	if got, _ := payload["transport_state"].(string); got != "queued" {
		t.Fatalf("transport_state = %q, want queued; payload=%v", got, payload)
	}
	if got, _ := payload["platform"].(string); got != "sms" {
		t.Fatalf("platform = %q, want sms (transport echoed from the daemon)", got)
	}
	if got, _ := payload["conversation_id"].(string); got != "conversation-stuck" {
		t.Fatalf("conversation_id = %q, want conversation-stuck", got)
	}
	if _, present := payload["expires_at_ms"]; !present {
		t.Fatalf("expires_at_ms missing from queued result; payload=%v", payload)
	}

	text := result.Content[0].(mcp.TextContent).Text
	for _, fragment := range []string{"NOT YET TRANSMITTED", "durably queued", "do NOT send this message again"} {
		if !strings.Contains(text, fragment) {
			t.Fatalf("queued text missing %q: %q", fragment, text)
		}
	}
	if strings.Contains(strings.ToLower(text), "delivery confirmed") {
		t.Fatalf("queued text claims delivery: %q", text)
	}
}

// TestDaemonWaitForTransmitHoldsThroughRetries verifies wait_for_transmit
// keeps polling through auto-retrying states until the transport acknowledges.
func TestDaemonWaitForTransmitHoldsThroughRetries(t *testing.T) {
	var polls atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"v2_send": true})
	})
	mux.HandleFunc("/api/v1/outbox/messages", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"outbox_id": "outbox-retrying",
			"state":     "queued",
		})
	})
	mux.HandleFunc("/api/v1/outbox/outbox-retrying", func(w http.ResponseWriter, r *http.Request) {
		// not_dispatched twice (would end a default wait), then confirmed.
		if polls.Add(1) < 3 {
			json.NewEncoder(w).Encode(map[string]any{
				"outbox_id":   "outbox-retrying",
				"state":       "not_dispatched",
				"error_class": "transient",
			})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"outbox_id":         "outbox-retrying",
			"conversation_id":   "conversation-retrying",
			"platform":          "sms",
			"state":             "confirmed",
			"remote_message_id": "remote-finally",
		})
	})
	options := Options{Daemon: daemonClientFor(t, mux)}
	handler := daemonSendToConversationHandler(options)

	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"conversation_id":   "conversation-retrying",
		"message":           "hold for the ack",
		"idempotency_key":   "wait-transmit-key",
		"wait_for_transmit": true,
		"wait_seconds":      float64(10),
	}
	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	payload := structuredMap(t, result)
	if got, _ := payload["transmitted"].(bool); !got {
		t.Fatalf("transmitted = false after transport ack; payload=%v", payload)
	}
	if got, _ := payload["transport_state"].(string); got != "transmitted" {
		t.Fatalf("transport_state = %q, want transmitted", got)
	}
	if got := polls.Load(); got < 3 {
		t.Fatalf("delivery polled %d times, want ≥3 (held through not_dispatched)", got)
	}
}

// TestDaemonSendBlockedWhenPlatformCannotSend pins send-time enforcement of
// the daemon's per-platform capability: a hard-down platform (adapter
// unregistered) refuses without queuing, while a merely-disconnected
// (queueable) platform still queues.
func TestDaemonSendBlockedWhenPlatformCannotSend(t *testing.T) {
	var submits atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"v2_send": true,
			"send": map[string]any{
				"sms":      map[string]any{"available": true},
				"whatsapp": map[string]any{"available": false, "queueable": false, "reason": "the platform adapter is not registered with the v2 send stack in this run (receive-only); sends on this platform fail rather than queue"},
				"signal":   map[string]any{"available": false, "queueable": true, "reason": "signal is disconnected; a send submitted now would wait in the outbox until it reconnects"},
			},
		})
	})
	mux.HandleFunc("/api/v1/outbox/messages", func(w http.ResponseWriter, r *http.Request) {
		submits.Add(1)
		json.NewEncoder(w).Encode(map[string]any{
			"outbox_id": "outbox-queued-signal",
			"state":     "queued",
		})
	})
	mux.HandleFunc("/api/v1/outbox/outbox-queued-signal", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"outbox_id": "outbox-queued-signal",
			"state":     "queued",
		})
	})
	options := Options{Daemon: daemonClientFor(t, mux)}
	handler := daemonSendToConversationHandler(options)

	// WhatsApp: hard-down → refused, nothing submitted.
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"conversation_id": "whatsapp:15551230000@s.whatsapp.net",
		"message":         "hi over whatsapp",
	}
	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if !result.IsError {
		t.Fatalf("expected an error result for an unsendable platform, got %v", result.Content)
	}
	payload := structuredMap(t, result)
	if got, _ := payload["error_kind"].(string); got != "platform_unsendable" {
		t.Fatalf("error_kind = %q, want platform_unsendable", got)
	}
	if got, _ := payload["platform"].(string); got != "whatsapp" {
		t.Fatalf("platform = %q, want whatsapp", got)
	}
	text := result.Content[0].(mcp.TextContent).Text
	for _, fragment := range []string{"Cannot send on whatsapp", "NOT queued", "No fallback to another platform"} {
		if !strings.Contains(text, fragment) {
			t.Fatalf("unsendable text missing %q: %q", fragment, text)
		}
	}
	if got := submits.Load(); got != 0 {
		t.Fatalf("submit called %d times for an unsendable platform, want 0", got)
	}

	// Signal: disconnected but queueable → the durable outbox accepts it.
	req = mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"conversation_id": "signal:+15551230000",
		"message":         "hi over signal",
		"idempotency_key": "signal-queueable-key",
		"wait_seconds":    float64(1),
	}
	result, err = handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if result.IsError {
		t.Fatalf("queueable outage must queue, not error: %v", result.Content)
	}
	if got := submits.Load(); got != 1 {
		t.Fatalf("submit called %d times for a queueable platform, want 1", got)
	}
}

// TestSendToConversationPlatformAssertionMismatch pins the hard channel
// contract: asserting platform=whatsapp against an sms conversation fails
// without sending anywhere.
func TestSendToConversationPlatformAssertionMismatch(t *testing.T) {
	harness := newV2ToolHarness(t)
	handler := sendToConversationHandler(harness.app, &harness.deps)

	result, err := handler(context.Background(), v2ToolCall(map[string]any{
		"conversation_id": v2ToolConversationID, // seeded as sms
		"message":         "meant for whatsapp",
		"platform":        "whatsapp",
	}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !result.IsError {
		t.Fatalf("platform mismatch must fail, got %v", result.Content)
	}
	payload := v2ToolPayload(t, result)
	if got := v2ToolString(payload, "error_kind"); got != "platform_mismatch" {
		t.Fatalf("error_kind = %q, want platform_mismatch", got)
	}
	if got := v2ToolString(payload, "requested_platform"); got != "whatsapp" {
		t.Fatalf("requested_platform = %q, want whatsapp", got)
	}
	if got := v2ToolString(payload, "actual_platform"); got != "sms" {
		t.Fatalf("actual_platform = %q, want sms", got)
	}
	text := result.Content[0].(mcp.TextContent).Text
	if !strings.Contains(text, "NOT queued") {
		t.Fatalf("mismatch text missing NOT queued: %q", text)
	}

	// The matching assertion passes through to a normal send... which then
	// fails only because the scripted sender has no steps; the mismatch gate
	// itself must not fire.
	result, err = handler(context.Background(), v2ToolCall(map[string]any{
		"conversation_id": v2ToolConversationID,
		"message":         "explicitly sms",
		"platform":        "sms",
		"idempotency_key": "assert-sms-key",
		"wait_seconds":    float64(1),
	}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if payload := v2ToolPayload(t, result); v2ToolString(payload, "error_kind") == "platform_mismatch" {
		t.Fatal("matching platform assertion must not fail as a mismatch")
	}
}

// TestV2SendCarriesTTLAndTransport pins ttl_seconds → expires_at_ms plumbing
// and the platform/conversation_id echo through the in-process v2 path.
func TestV2SendCarriesTTLAndTransport(t *testing.T) {
	harness := newV2ToolHarness(t, v2ToolSendStep{result: bridge.SendResult{
		RemoteMessageID: "remote-ttl",
	}})
	handler := sendToConversationHandler(harness.app, &harness.deps)

	before := time.Now()
	result, err := handler(context.Background(), v2ToolCall(map[string]any{
		"conversation_id": v2ToolConversationID,
		"message":         "windowed send",
		"idempotency_key": "ttl-plumb-key",
		"ttl_seconds":     float64(120),
	}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	payload := v2ToolPayload(t, result)
	if got := v2ToolString(payload, "transport_state"); got != "transmitted" {
		t.Fatalf("transport_state = %q, want transmitted; payload=%v", got, payload)
	}
	if got := v2ToolString(payload, "platform"); got != "sms" {
		t.Fatalf("platform = %q, want sms", got)
	}
	if got := v2ToolString(payload, "conversation_id"); got != v2ToolConversationID {
		t.Fatalf("conversation_id = %q, want %q", got, v2ToolConversationID)
	}
	expiresAt, ok := payload["expires_at_ms"].(float64)
	if !ok {
		t.Fatalf("expires_at_ms missing; payload=%v", payload)
	}
	wantLow := before.Add(115 * time.Second).UnixMilli()
	wantHigh := before.Add(150 * time.Second).UnixMilli()
	if int64(expiresAt) < wantLow || int64(expiresAt) > wantHigh {
		t.Fatalf("expires_at_ms = %d, want within [%d, %d]", int64(expiresAt), wantLow, wantHigh)
	}
	text := result.Content[0].(mcp.TextContent).Text
	if !strings.Contains(text, "transmitted") || strings.Contains(strings.ToLower(text), "delivery confirmed") {
		t.Fatalf("transmitted text wrong: %q", text)
	}
}

// TestV2SendNearDuplicateBlockedThenForcedThroughMCP drives the guard through
// the whole tool path: second near-identical send blocked with guidance,
// force=true passes.
func TestV2SendNearDuplicateBlockedThenForcedThroughMCP(t *testing.T) {
	harness := newV2ToolHarness(t,
		v2ToolSendStep{result: bridge.SendResult{RemoteMessageID: "remote-dup-1"}},
		v2ToolSendStep{result: bridge.SendResult{RemoteMessageID: "remote-dup-2"}},
	)
	handler := sendToConversationHandler(harness.app, &harness.deps)

	first, err := handler(context.Background(), v2ToolCall(map[string]any{
		"conversation_id": v2ToolConversationID,
		"message":         "Lunch tomorrow at noon at Sfoglina?",
		"idempotency_key": "dup-mcp-first",
	}))
	if err != nil || first.IsError {
		t.Fatalf("first send failed: err=%v result=%v", err, first)
	}
	firstOutbox := v2ToolString(v2ToolPayload(t, first), "outbox_id")

	blocked, err := handler(context.Background(), v2ToolCall(map[string]any{
		"conversation_id": v2ToolConversationID,
		"message":         "Lunch today at noon at Sfoglina?",
		"idempotency_key": "dup-mcp-second",
	}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !blocked.IsError {
		t.Fatalf("near-duplicate must be blocked, got %v", blocked.Content)
	}
	payload := v2ToolPayload(t, blocked)
	if got := v2ToolString(payload, "error_kind"); got != "near_duplicate_blocked" {
		t.Fatalf("error_kind = %q, want near_duplicate_blocked", got)
	}
	if got := v2ToolString(payload, "duplicate_of_outbox_id"); got != firstOutbox {
		t.Fatalf("duplicate_of_outbox_id = %q, want %q", got, firstOutbox)
	}
	text := blocked.Content[0].(mcp.TextContent).Text
	for _, fragment := range []string{"NOT QUEUED", "force=true"} {
		if !strings.Contains(text, fragment) {
			t.Fatalf("duplicate text missing %q: %q", fragment, text)
		}
	}

	forced, err := handler(context.Background(), v2ToolCall(map[string]any{
		"conversation_id": v2ToolConversationID,
		"message":         "Lunch today at noon at Sfoglina?",
		"idempotency_key": "dup-mcp-forced",
		"force":           true,
	}))
	if err != nil || forced.IsError {
		t.Fatalf("forced send failed: err=%v result=%v", err, forced)
	}
	if got := v2ToolString(v2ToolPayload(t, forced), "transport_state"); got != "transmitted" {
		t.Fatalf("forced transport_state = %q, want transmitted", got)
	}
}

// TestListAndCancelOutboxDaemonMode covers the new custody tools against a
// fake daemon.
func TestListAndCancelOutboxDaemonMode(t *testing.T) {
	canceled := false
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/outbox", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]map[string]any{{
			"outbox_id":       "outbox-listed",
			"conversation_id": "conversation-listed",
			"kind":            "text",
			"state":           "queued",
			"created_at_ms":   time.Now().UnixMilli(),
			"expires_at_ms":   time.Now().Add(5 * time.Minute).UnixMilli(),
			"summary":         "still waiting",
		}})
	})
	mux.HandleFunc("POST /api/v1/outbox/outbox-listed/cancel", func(w http.ResponseWriter, r *http.Request) {
		canceled = true
		json.NewEncoder(w).Encode(map[string]any{
			"outbox_id": "outbox-listed",
			"state":     "canceled",
		})
	})
	options := Options{Daemon: daemonClientFor(t, mux)}

	listReq := mcp.CallToolRequest{}
	listReq.Params.Arguments = map[string]any{}
	listResult, err := daemonListOutboxHandler(options)(context.Background(), listReq)
	if err != nil {
		t.Fatalf("list handler: %v", err)
	}
	if listResult.IsError {
		t.Fatalf("list errored: %v", listResult.Content)
	}
	listPayload := structuredMap(t, listResult)
	if got, _ := listPayload["count"].(int); got != 1 {
		if gotFloat, _ := listPayload["count"].(float64); int(gotFloat) != 1 {
			t.Fatalf("count = %v, want 1", listPayload["count"])
		}
	}
	listText := listResult.Content[0].(mcp.TextContent).Text
	for _, fragment := range []string{"outbox-listed", "queued", "do not resend"} {
		if !strings.Contains(listText, fragment) {
			t.Fatalf("list text missing %q: %q", fragment, listText)
		}
	}

	cancelReq := mcp.CallToolRequest{}
	cancelReq.Params.Arguments = map[string]any{"outbox_id": "outbox-listed"}
	cancelResult, err := daemonCancelOutboxHandler(options)(context.Background(), cancelReq)
	if err != nil {
		t.Fatalf("cancel handler: %v", err)
	}
	if cancelResult.IsError {
		t.Fatalf("cancel errored: %v", cancelResult.Content)
	}
	if !canceled {
		t.Fatal("daemon cancel endpoint was not called")
	}
	cancelText := cancelResult.Content[0].(mcp.TextContent).Text
	if !strings.Contains(cancelText, "NOT sent") {
		t.Fatalf("cancel text missing NOT sent: %q", cancelText)
	}
}

// TestV2ListAndCancelOutboxInProcess covers the custody tools against the
// in-process service: a queued send is visible, cancelable, and a confirmed
// one refuses cancellation.
func TestV2ListAndCancelOutboxInProcess(t *testing.T) {
	release := make(chan struct{})
	harness := newV2ToolHarness(t, v2ToolSendStep{
		result: bridge.SendResult{RemoteMessageID: "remote-after-cancel-check"},
		block:  release,
	})
	defer close(release)
	sendHandler := sendToConversationHandler(harness.app, &harness.deps)

	// Submit with a tiny wait so the handler returns while the item is still
	// in flight.
	sendResult, err := sendHandler(context.Background(), v2ToolCall(map[string]any{
		"conversation_id": v2ToolConversationID,
		"message":         "cancel me maybe",
		"idempotency_key": "custody-key",
		"wait_seconds":    float64(1),
	}))
	if err != nil {
		t.Fatalf("send handler: %v", err)
	}
	outboxID := v2ToolString(v2ToolPayload(t, sendResult), "outbox_id")
	if outboxID == "" {
		t.Fatal("send result missing outbox_id")
	}

	listReq := mcp.CallToolRequest{}
	listReq.Params.Arguments = map[string]any{"conversation_id": v2ToolConversationID}
	listResult, err := v2ListOutboxHandler(&harness.deps)(context.Background(), listReq)
	if err != nil {
		t.Fatalf("list handler: %v", err)
	}
	if listResult.IsError {
		t.Fatalf("list errored: %v", listResult.Content)
	}
	listText := listResult.Content[0].(mcp.TextContent).Text
	if !strings.Contains(listText, outboxID) {
		t.Fatalf("list text missing %q: %q", outboxID, listText)
	}

	// The item is dispatching (parked on the block channel), so cancel must
	// refuse: it may already have crossed the transport boundary.
	cancelReq := mcp.CallToolRequest{}
	cancelReq.Params.Arguments = map[string]any{"outbox_id": outboxID}
	cancelResult, err := v2CancelOutboxHandler(&harness.deps)(context.Background(), cancelReq)
	if err != nil {
		t.Fatalf("cancel handler: %v", err)
	}
	if !cancelResult.IsError {
		t.Fatalf("cancel of a dispatching item must refuse, got %v", cancelResult.Content)
	}
	cancelText := cancelResult.Content[0].(mcp.TextContent).Text
	if !strings.Contains(cancelText, "cannot cancel") {
		t.Fatalf("refusal text = %q", cancelText)
	}
}

// TestDaemonUnavailableDaemonPredatesSendBlock: a daemon without the send
// capability block must not be blocked on (capability unknown ≠ unavailable).
func TestDaemonUnavailableDaemonPredatesSendBlock(t *testing.T) {
	status := localapi.DaemonStatus{}
	if failure := daemonCheckPlatformSendable(status, "whatsapp"); failure != nil {
		t.Fatalf("old daemon without send block must pass through, got %v", failure.Content)
	}
}

// TestSendPayloadSettledMatrix pins settled/transmitted for every outbox
// state — the incident's overclaim, as a table.
func TestSendPayloadSettledMatrix(t *testing.T) {
	tests := []struct {
		state           messaging.OutboxState
		wantSettled     bool
		wantTransmitted bool
		wantTransport   string
	}{
		{messaging.OutboxQueued, false, false, "queued"},
		{messaging.OutboxDispatching, false, false, "queued"},
		{messaging.OutboxNotDispatched, false, false, "queued"},
		{messaging.OutboxUncertain, false, false, "uncertain"},
		{messaging.OutboxConfirmed, true, true, "transmitted"},
		{messaging.OutboxStoreFailed, true, true, "transmitted"},
		{messaging.OutboxRejected, true, false, "failed"},
		{messaging.OutboxCanceled, true, false, "canceled"},
	}
	for _, test := range tests {
		t.Run(string(test.state), func(t *testing.T) {
			payload := buildSendPayload(sendOutcome{Delivery: messaging.Delivery{
				OutboxID: "outbox-matrix",
				State:    test.state,
			}})
			if got, _ := payload["settled"].(bool); got != test.wantSettled {
				t.Fatalf("settled = %v, want %v", got, test.wantSettled)
			}
			if got, _ := payload["transmitted"].(bool); got != test.wantTransmitted {
				t.Fatalf("transmitted = %v, want %v", got, test.wantTransmitted)
			}
			if got, _ := payload["transport_state"].(string); got != test.wantTransport {
				t.Fatalf("transport_state = %q, want %q", got, test.wantTransport)
			}
		})
	}
}
