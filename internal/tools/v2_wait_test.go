package tools

// Settled-state matrix for the v2 send tools' delivery reporting. The review
// of this change proved two double-send vectors: an interrupted Wait returned
// a bare tool error while the durable send completed, and not_dispatched was
// presented as final while the outbox kept retrying it. These tests pin the
// corrected contract: once an intent is durably enqueued the tool never
// returns IsError, every result echoes outbox_id and idempotency_key, and
// non-settled states carry explicit do-not-resend guidance.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/messaging"
)

func TestV2SendInterruptedWaitReturnsDurableStatusNotError(t *testing.T) {
	release := make(chan struct{})
	harness := newV2ToolHarness(t, v2ToolSendStep{
		result: bridge.SendResult{RemoteMessageID: "remote-after-interrupt"},
		block:  release,
	})
	handler := sendToConversationHandler(harness.app, &harness.deps)

	ctx, cancel := context.WithCancel(context.Background())
	results := make(chan *mcp.CallToolResult, 1)
	go func() {
		result, err := handler(ctx, v2ToolCall(map[string]any{
			"conversation_id": v2ToolConversationID,
			"message":         "interrupt this wait",
			"idempotency_key": "mcp-interrupt-key",
		}))
		if err != nil {
			t.Errorf("handler error: %v", err)
		}
		results <- result
	}()

	// The transport call parks on the block channel, so once it is observed
	// the row is in dispatching and the handler is inside Wait.
	waitForCondition(t, "transport call started", func() bool {
		return harness.sender.requestCount() == 1
	})
	cancel()

	var result *mcp.CallToolResult
	select {
	case result = <-results:
	case <-time.After(10 * time.Second):
		t.Fatal("handler did not return after ctx cancel")
	}
	if result.IsError {
		t.Fatalf("interrupted wait returned a tool error, inviting a resend: %v", result.Content)
	}

	payload := v2ToolPayload(t, result)
	assertV2ToolBool(t, payload, "ok", false)
	assertV2ToolBool(t, payload, "settled", false)
	assertV2ToolString(t, payload, "idempotency_key", "mcp-interrupt-key")
	outboxID := v2ToolString(payload, "outbox_id")
	if outboxID == "" {
		t.Fatalf("interrupted result must carry outbox_id; payload=%v", payload)
	}
	state := v2ToolString(payload, "state")
	if state != string(messaging.OutboxQueued) && state != string(messaging.OutboxDispatching) {
		t.Fatalf("state = %q, want queued or dispatching", state)
	}
	if got := v2ToolString(payload, "wait_error"); got == "" {
		t.Fatal("wait_error is empty")
	}
	text := result.Content[0].(mcp.TextContent).Text
	for _, fragment := range []string{"durably queued", "Do NOT send this message again", "mcp-interrupt-key"} {
		if !strings.Contains(text, fragment) {
			t.Fatalf("interrupted text missing %q: %q", fragment, text)
		}
	}

	// A guidance-following agent does not resend. The durable send finishes in
	// the background and the transport is called exactly once.
	close(release)
	waitForCondition(t, "delivery confirmed in background", func() bool {
		delivery, err := harness.deps.Service.Get(context.Background(), outboxID)
		return err == nil && delivery.State == messaging.OutboxConfirmed
	})
	if got := harness.sender.requestCount(); got != 1 {
		t.Fatalf("transport called %d times, want exactly 1", got)
	}
}

func TestV2SendNotDispatchedReturnsAutoRetryStatusNotError(t *testing.T) {
	harness := newV2ToolHarness(t, v2ToolSendStep{err: bridge.OpError{
		Class:     bridge.FailureTransient,
		Operation: "send_text",
		Dispatch:  bridge.DispatchNotCalled,
		Cause:     context.DeadlineExceeded,
	}})
	handler := sendToConversationHandler(harness.app, &harness.deps)

	result, err := handler(context.Background(), v2ToolCall(map[string]any{
		"conversation_id": v2ToolConversationID,
		"message":         "the transport is briefly down",
		"idempotency_key": "mcp-retrying-key",
	}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if result.IsError {
		t.Fatalf("not_dispatched returned a tool error, inviting a resend: %v", result.Content)
	}

	payload := v2ToolPayload(t, result)
	assertV2ToolBool(t, payload, "ok", false)
	assertV2ToolBool(t, payload, "settled", false)
	assertV2ToolBool(t, payload, "auto_retry", true)
	assertV2ToolString(t, payload, "state", string(messaging.OutboxNotDispatched))
	assertV2ToolString(t, payload, "idempotency_key", "mcp-retrying-key")
	text := result.Content[0].(mcp.TextContent).Text
	for _, fragment := range []string{"retrying it automatically", "do NOT send this message again"} {
		if !strings.Contains(text, fragment) {
			t.Fatalf("not_dispatched text missing %q: %q", fragment, text)
		}
	}
	if strings.Contains(text, "was not dispatched") {
		t.Fatalf("not_dispatched text still uses the resend-inviting phrasing: %q", text)
	}

	// The durable row remains pending for the dispatcher's automatic retry.
	outboxID := v2ToolString(payload, "outbox_id")
	delivery, err := harness.deps.Service.Get(context.Background(), outboxID)
	if err != nil {
		t.Fatalf("Get(%s): %v", outboxID, err)
	}
	if delivery.State != messaging.OutboxNotDispatched && delivery.State != messaging.OutboxQueued &&
		delivery.State != messaging.OutboxDispatching && delivery.State != messaging.OutboxConfirmed {
		t.Fatalf("row left auto-retry path: state=%s", delivery.State)
	}
}

func TestV2SendRejectedIsSettledWithNoRetryGuidance(t *testing.T) {
	harness := newV2ToolHarness(t, v2ToolSendStep{err: bridge.OpError{
		Class:     bridge.FailureUnsupported,
		Operation: "send_text",
		Cause:     context.DeadlineExceeded,
	}})
	handler := sendToConversationHandler(harness.app, &harness.deps)

	result, err := handler(context.Background(), v2ToolCall(map[string]any{
		"conversation_id": v2ToolConversationID,
		"message":         "the platform refuses this",
		"idempotency_key": "mcp-rejected-key",
	}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if result.IsError {
		t.Fatalf("rejected delivery must be data, not a tool error: %v", result.Content)
	}

	payload := v2ToolPayload(t, result)
	assertV2ToolBool(t, payload, "ok", false)
	assertV2ToolBool(t, payload, "settled", true)
	assertV2ToolString(t, payload, "state", string(messaging.OutboxRejected))
	assertV2ToolString(t, payload, "idempotency_key", "mcp-rejected-key")
	text := result.Content[0].(mcp.TextContent).Text
	if !strings.Contains(text, "will not retry") {
		t.Fatalf("rejected text missing no-retry statement: %q", text)
	}
}

func TestV2DeliveryTextAndOKForRepairAndCancelStates(t *testing.T) {
	storeFailed := messaging.Delivery{
		OutboxID:        "outbox-repair",
		State:           messaging.OutboxStoreFailed,
		RemoteMessageID: "remote-repair",
	}
	if !v2DeliveryOK(storeFailed.State) {
		t.Fatal("store_failed means the transport delivered; ok must be true")
	}
	text := v2DeliveryText(storeFailed)
	for _, fragment := range []string{"transport accepted", "repaired automatically", "Do not resend"} {
		if !strings.Contains(text, fragment) {
			t.Fatalf("store_failed text missing %q: %q", fragment, text)
		}
	}

	canceled := messaging.Delivery{OutboxID: "outbox-canceled", State: messaging.OutboxCanceled}
	if v2DeliveryOK(canceled.State) {
		t.Fatal("canceled must not report ok")
	}
	if got := v2DeliveryText(canceled); !strings.Contains(got, "canceled") {
		t.Fatalf("canceled text = %q", got)
	}
}

func waitForCondition(t *testing.T, what string, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
