package tools

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"testing/quick"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"

	"github.com/maxghenis/openmessage/internal/app"
	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/localapi"
	"github.com/maxghenis/openmessage/internal/messaging"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
	"github.com/maxghenis/openmessage/internal/v2read"
	"github.com/maxghenis/openmessage/internal/web"
)

// The v2-primary reply fixture: one account, a thread holding the messages a
// send to it may quote, and a second thread whose message it may not.
const (
	replyThreadID        = "reply-thread"
	replyOtherThreadID   = "reply-other-thread"
	replyTargetID        = "reply-target"
	replyTargetRemoteID  = "remote-reply-target"
	replyTargetBody      = "quote me"
	replySecondID        = "reply-second"
	replySecondRemoteID  = "remote-reply-second"
	replyElsewhereID     = "reply-elsewhere"
	replyUnknownID       = "no-such-message"
	replyRefusalCode     = "reply_target_unavailable"
	replyFixtureAccount  = "google-primary"
	replyFixtureTargetMS = int64(1_700_000_000_000)
)

// newReplyFixture is a v2-primary tool harness over a store that holds
// reply-thread, with the messages a send there may quote, and
// reply-other-thread, with a message no send to reply-thread may quote.
func newReplyFixture(t *testing.T) *v2ToolHarness {
	t.Helper()
	harness := newV2ToolHarness(t)
	harness.deps.V2Primary = true
	store := harness.deps.V2Store
	nowMS := time.Now().UnixMilli()
	if err := store.UpsertAccount(sqlite.Account{
		AccountID: replyFixtureAccount, BridgeKey: "google_messages", DisplayName: "Google",
		Mode: sqlite.AccountModeLive, Enabled: true, ConfigJSON: "{}", CreatedAtMS: nowMS, UpdatedAtMS: nowMS,
	}); err != nil {
		t.Fatalf("UpsertAccount(): %v", err)
	}
	for _, conversationID := range []string{replyThreadID, replyOtherThreadID} {
		if err := store.UpsertConversation(sqlite.Conversation{
			ConversationID: conversationID, AccountID: replyFixtureAccount, RemoteConversationID: "remote-" + conversationID,
			Kind: sqlite.ConversationKindDirect, Title: conversationID, NotificationMode: sqlite.NotificationModeAll,
			MetadataJSON: "{}", CreatedAtMS: nowMS, UpdatedAtMS: nowMS,
		}); err != nil {
			t.Fatalf("UpsertConversation(%s): %v", conversationID, err)
		}
	}
	messages, err := sqlite.NewMessageRepository(store, time.Now)
	if err != nil {
		t.Fatalf("NewMessageRepository(): %v", err)
	}
	for _, message := range []sqlite.Message{
		{MessageID: replyTargetID, ConversationID: replyThreadID, RemoteMessageID: replyTargetRemoteID, Body: replyTargetBody, OccurredAtMS: replyFixtureTargetMS},
		{MessageID: replySecondID, ConversationID: replyThreadID, RemoteMessageID: replySecondRemoteID, Body: "or quote me", OccurredAtMS: replyFixtureTargetMS + 1_000},
		{MessageID: replyElsewhereID, ConversationID: replyOtherThreadID, RemoteMessageID: "remote-reply-elsewhere", Body: "not in this thread", OccurredAtMS: replyFixtureTargetMS},
	} {
		message.AccountID = replyFixtureAccount
		message.Direction = sqlite.MessageDirectionIncoming
		message.State = sqlite.MessageStateActive
		if err := messages.ImportMessage(context.Background(), sqlite.MessageProjection{Message: message}); err != nil {
			t.Fatalf("ImportMessage(%s): %v", message.MessageID, err)
		}
	}
	return harness
}

// sendEffects is what sends leave behind: IDs the message service allocated,
// transport calls, outbox rows, and outgoing messages in the v2 store.
type sendEffects struct {
	IDs, TransportCalls, OutboxRows, OutgoingMessages int
}

func (h *v2ToolHarness) effects(t *testing.T) sendEffects {
	t.Helper()
	inspection, err := sql.Open("sqlite", "file:"+h.v2Path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open a second connection to the v2 store: %v", err)
	}
	defer inspection.Close()
	effects := sendEffects{IDs: h.ids.count(), TransportCalls: h.sender.requestCount()}
	if err := inspection.QueryRow(`SELECT COUNT(*) FROM outbox`).Scan(&effects.OutboxRows); err != nil {
		t.Fatalf("count outbox rows: %v", err)
	}
	if err := inspection.QueryRow(`SELECT COUNT(*) FROM messages WHERE direction = 'outgoing'`).Scan(&effects.OutgoingMessages); err != nil {
		t.Fatalf("count outgoing messages: %v", err)
	}
	return effects
}

func callTool(t *testing.T, ctx context.Context, handler server.ToolHandlerFunc, arguments map[string]any) *mcp.CallToolResult {
	t.Helper()
	result, err := handler(ctx, v2ToolCall(arguments))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	return result
}

// assertReplyRefused checks the tool's answer to a reply target the app
// refused: a tool error with the reply_target_unavailable code, naming the
// conversation and the refused ID, and saying nothing was sent.
func assertReplyRefused(t *testing.T, result *mcp.CallToolResult, conversationID, replyToID string) {
	t.Helper()
	text := resultText(t, result)
	if !result.IsError {
		t.Fatalf("reply to %q in %q = %q, want a tool error", replyToID, conversationID, text)
	}
	payload := v2ToolPayload(t, result)
	assertV2ToolBool(t, payload, "ok", false)
	assertV2ToolString(t, payload, "error_code", replyRefusalCode)
	assertV2ToolString(t, payload, "conversation_id", conversationID)
	assertV2ToolString(t, payload, "reply_to_id", replyToID)
	for _, want := range []string{"NOT sent", replyRefusalCode, "Nothing was queued"} {
		if !strings.Contains(text, want) {
			t.Fatalf("refusal text %q lacks %q", text, want)
		}
	}
}

// assertQuotes checks that the transport's request quotes the given message.
func assertQuotes(t *testing.T, request bridge.TextRequest, remoteID, body string, sentAtMS int64) {
	t.Helper()
	if request.ReplyTo == nil {
		t.Fatalf("transport request %+v carries no reply ref", request)
	}
	ref := request.ReplyTo
	if ref.RemoteID != remoteID || ref.Text != body || ref.Outgoing || !ref.SentAt.Equal(time.UnixMilli(sentAtMS)) {
		t.Fatalf("reply ref = %+v, want remote ID %q, text %q, incoming, sent at %d", *ref, remoteID, body, sentAtMS)
	}
}

func assertStoredReply(t *testing.T, store *sqlite.Store, localMessageID, conversationID, remoteID string) {
	t.Helper()
	messages, err := sqlite.NewMessageRepository(store, time.Now)
	if err != nil {
		t.Fatalf("NewMessageRepository(): %v", err)
	}
	stored, err := messages.GetMessage(context.Background(), localMessageID)
	if err != nil {
		t.Fatalf("GetMessage(%q): %v", localMessageID, err)
	}
	if stored.ConversationID != conversationID || stored.Direction != sqlite.MessageDirectionOutgoing {
		t.Fatalf("stored message = %+v, want an outgoing message in %q", stored, conversationID)
	}
	if stored.ReplyToRemoteID == nil || *stored.ReplyToRemoteID != remoteID {
		t.Fatalf("stored ReplyToRemoteID = %v, want %q", stored.ReplyToRemoteID, remoteID)
	}
}

// replyAppClient serves the app's real local API over the harness's stores
// and dispatcher, the way a v2-primary app answers an MCP client.
func replyAppClient(t *testing.T, harness *v2ToolHarness) *localapi.Client {
	t.Helper()
	return appAPI(t, harness.app.Store, web.APIOptions{
		V2Primary: true,
		V2: &web.V2Options{
			Service:  harness.deps.Service,
			V2Store:  harness.deps.V2Store,
			Registry: harness.deps.Registry,
		},
	})
}

// A v2-primary send_to_conversation with reply_to_id queues one send whose
// stored message records the quoted message's remote ID, and the transport
// gets a ref to that message.
func TestV2PrimarySendToConversationQuoteRepliesThroughTheOutbox(t *testing.T) {
	harness := newReplyFixture(t)
	result := callTool(t, harness.ctx, sendToConversationHandler(harness.app, &harness.deps), map[string]any{
		"conversation_id": replyThreadID,
		"message":         "replying",
		"reply_to_id":     replyTargetID,
		"idempotency_key": "reply-confirmed",
	})
	if result.IsError {
		t.Fatalf("reply returned a tool error: %s", resultText(t, result))
	}
	payload := v2ToolPayload(t, result)
	assertV2ToolBool(t, payload, "ok", true)
	assertV2ToolString(t, payload, "state", string(messaging.OutboxConfirmed))

	requests := harness.sender.snapshotRequests()
	if len(requests) != 1 {
		t.Fatalf("transport requests = %d, want 1", len(requests))
	}
	if requests[0].Body != "replying" || requests[0].Conversation.RemoteID != "remote-"+replyThreadID {
		t.Fatalf("transport request = %+v, want the reply's body in the thread", requests[0])
	}
	assertQuotes(t, requests[0], replyTargetRemoteID, replyTargetBody, replyFixtureTargetMS)
	assertStoredReply(t, harness.deps.V2Store, v2ToolString(payload, "local_message_id"), replyThreadID, replyTargetRemoteID)
}

// A reply target the conversation doesn't hold, whether another thread's
// message or no message at all, comes back as reply_target_unavailable with
// nothing allocated, queued, stored or sent. The refused call leaves its
// idempotency key unused, so the same key then sends a plain message.
func TestV2PrimarySendToConversationRefusesAReplyTargetOutsideTheConversation(t *testing.T) {
	for name, replyToID := range map[string]string{
		"another conversation's message": replyElsewhereID,
		"unknown message":                replyUnknownID,
	} {
		t.Run(name, func(t *testing.T) {
			harness := newReplyFixture(t)
			handler := sendToConversationHandler(harness.app, &harness.deps)
			result := callTool(t, harness.ctx, handler, map[string]any{
				"conversation_id": replyThreadID,
				"message":         "replying",
				"reply_to_id":     replyToID,
				"idempotency_key": "reply-refused",
			})
			assertReplyRefused(t, result, replyThreadID, replyToID)
			if got := harness.effects(t); got != (sendEffects{}) {
				t.Fatalf("a refused reply left %+v, want nothing", got)
			}

			plain := callTool(t, harness.ctx, handler, map[string]any{
				"conversation_id": replyThreadID,
				"message":         "replying",
				"idempotency_key": "reply-refused",
			})
			if plain.IsError {
				t.Fatalf("plain send after the refusal = %s, want it sent", resultText(t, plain))
			}
			payload := v2ToolPayload(t, plain)
			assertV2ToolString(t, payload, "state", string(messaging.OutboxConfirmed))
			assertV2ToolBool(t, payload, "deduplicated", false)
			if requests := harness.sender.snapshotRequests(); len(requests) != 1 || requests[0].ReplyTo != nil {
				t.Fatalf("transport requests = %+v, want one plain send", requests)
			}
		})
	}
}

// The message_id that get_conversation and search_messages return on a
// v2-primary install is the reply_to_id send_to_conversation takes.
func TestV2PrimaryReplyTakesTheMessageIDTheReadToolsReturn(t *testing.T) {
	harness := newReplyFixture(t)
	mcpServer := server.NewMCPServer("reply-round-trip", "test")
	deps := harness.deps
	RegisterWithOptions(mcpServer, harness.app, Options{Reads: v2read.New(harness.deps.V2Store), V2Primary: true, V2: &deps})
	call := func(tool string, arguments map[string]any) *mcp.CallToolResult {
		t.Helper()
		registered := mcpServer.GetTool(tool)
		if registered == nil {
			t.Fatalf("%s is not registered", tool)
		}
		return callTool(t, harness.ctx, registered.Handler, arguments)
	}
	messageIDOf := func(result *mcp.CallToolResult, body string) string {
		t.Helper()
		if result.IsError {
			t.Fatalf("read tool error: %s", resultText(t, result))
		}
		var read struct {
			Messages []messageSummary `json:"messages"`
		}
		raw, err := json.Marshal(result.StructuredContent)
		if err != nil {
			t.Fatalf("marshal read result: %v", err)
		}
		if err := json.Unmarshal(raw, &read); err != nil {
			t.Fatalf("decode read result %s: %v", raw, err)
		}
		for _, message := range read.Messages {
			if message.Body == body {
				return message.MessageID
			}
		}
		t.Fatalf("read result %s holds no message %q", raw, body)
		return ""
	}

	fromConversation := messageIDOf(call("get_conversation", map[string]any{"conversation_id": replyThreadID}), replyTargetBody)
	fromSearch := messageIDOf(call("search_messages", map[string]any{"query": replyTargetBody, "conversation_id": replyThreadID}), replyTargetBody)
	if fromConversation != fromSearch {
		t.Fatalf("get_conversation gave %q and search_messages %q for the same message", fromConversation, fromSearch)
	}

	result := call("send_to_conversation", map[string]any{
		"conversation_id": replyThreadID,
		"message":         "replying",
		"reply_to_id":     fromConversation,
	})
	if result.IsError {
		t.Fatalf("reply with the read tools' message_id = %s", resultText(t, result))
	}
	requests := harness.sender.snapshotRequests()
	if len(requests) != 1 {
		t.Fatalf("transport requests = %d, want 1", len(requests))
	}
	assertQuotes(t, requests[0], replyTargetRemoteID, replyTargetBody, replyFixtureTargetMS)
}

// With v2 sending on a legacy-primary install (the mirror), reply_to_id is
// the legacy message ID the read tools return there, and the send's
// conversation must hold it.
func TestMirrorSendToConversationQuoteRepliesByLegacyMessageID(t *testing.T) {
	const (
		quotedID       = "legacy-quoted"
		elsewhereID    = "legacy-elsewhere"
		otherConvID    = "v2-tool-other-conversation"
		quotedBody     = "legacy quote"
		quotedAtMS     = int64(1_700_000_100_000)
		elsewhereAtMS  = int64(1_700_000_200_000)
		mirrorReplyKey = "mirror-reply"
	)
	seed := func(t *testing.T) *v2ToolHarness {
		t.Helper()
		harness := newV2ToolHarness(t)
		store := harness.app.Store
		if err := store.UpsertConversation(&db.Conversation{
			ConversationID: otherConvID, Name: "Other", Participants: `[]`, SourcePlatform: "sms",
		}); err != nil {
			t.Fatalf("seed other conversation: %v", err)
		}
		for _, message := range []*db.Message{
			{MessageID: quotedID, ConversationID: v2ToolConversationID, Body: quotedBody, TimestampMS: quotedAtMS, SourcePlatform: "sms"},
			{MessageID: elsewhereID, ConversationID: otherConvID, Body: "elsewhere", TimestampMS: elsewhereAtMS, SourcePlatform: "sms"},
		} {
			if err := store.UpsertMessage(message); err != nil {
				t.Fatalf("seed legacy message %s: %v", message.MessageID, err)
			}
		}
		return harness
	}

	t.Run("a message of the conversation", func(t *testing.T) {
		harness := seed(t)
		result := callTool(t, harness.ctx, sendToConversationHandler(harness.app, &harness.deps), map[string]any{
			"conversation_id": v2ToolConversationID,
			"message":         "replying",
			"reply_to_id":     quotedID,
			"idempotency_key": mirrorReplyKey,
		})
		if result.IsError {
			t.Fatalf("mirror reply = %s", resultText(t, result))
		}
		payload := v2ToolPayload(t, result)
		assertV2ToolString(t, payload, "state", string(messaging.OutboxConfirmed))
		requests := harness.sender.snapshotRequests()
		if len(requests) != 1 {
			t.Fatalf("transport requests = %d, want 1", len(requests))
		}
		assertQuotes(t, requests[0], quotedID, quotedBody, quotedAtMS)
		assertStoredReply(t, harness.deps.V2Store, v2ToolString(payload, "local_message_id"), v2ToolConversationID, quotedID)
	})
	for name, replyToID := range map[string]string{
		"another conversation's message": elsewhereID,
		"unknown message":                "legacy-missing",
	} {
		t.Run(name, func(t *testing.T) {
			harness := seed(t)
			result := callTool(t, harness.ctx, sendToConversationHandler(harness.app, &harness.deps), map[string]any{
				"conversation_id": v2ToolConversationID,
				"message":         "replying",
				"reply_to_id":     replyToID,
				"idempotency_key": mirrorReplyKey,
			})
			assertReplyRefused(t, result, v2ToolConversationID, replyToID)
			if got := harness.effects(t); got != (sendEffects{}) {
				t.Fatalf("a refused mirror reply left %+v, want nothing", got)
			}
		})
	}
}

// In client mode the tool hands reply_to_id to the app's outbox, which
// checks it, and passes on the app's reply_target_unavailable refusal.
func TestDaemonSendToConversationQuoteRepliesThroughTheAppsOutbox(t *testing.T) {
	t.Run("a message of the conversation", func(t *testing.T) {
		harness := newReplyFixture(t)
		handler := daemonSendToConversationHandler(Options{V2Primary: true, Daemon: replyAppClient(t, harness)})
		result := callTool(t, harness.ctx, handler, map[string]any{
			"conversation_id": replyThreadID,
			"message":         "replying",
			"reply_to_id":     replyTargetID,
			"idempotency_key": "daemon-reply",
		})
		if result.IsError {
			t.Fatalf("client-mode reply = %s", resultText(t, result))
		}
		payload := v2ToolPayload(t, result)
		assertV2ToolBool(t, payload, "ok", true)
		assertV2ToolString(t, payload, "state", string(messaging.OutboxConfirmed))
		requests := harness.sender.snapshotRequests()
		if len(requests) != 1 {
			t.Fatalf("transport requests = %d, want 1", len(requests))
		}
		assertQuotes(t, requests[0], replyTargetRemoteID, replyTargetBody, replyFixtureTargetMS)
		assertStoredReply(t, harness.deps.V2Store, v2ToolString(payload, "local_message_id"), replyThreadID, replyTargetRemoteID)
	})
	for name, replyToID := range map[string]string{
		"another conversation's message": replyElsewhereID,
		"unknown message":                replyUnknownID,
	} {
		t.Run(name, func(t *testing.T) {
			harness := newReplyFixture(t)
			handler := daemonSendToConversationHandler(Options{V2Primary: true, Daemon: replyAppClient(t, harness)})
			result := callTool(t, harness.ctx, handler, map[string]any{
				"conversation_id": replyThreadID,
				"message":         "replying",
				"reply_to_id":     replyToID,
				"idempotency_key": "daemon-reply",
			})
			assertReplyRefused(t, result, replyThreadID, replyToID)
			if got := harness.effects(t); got != (sendEffects{}) {
				t.Fatalf("a refused client-mode reply left %+v, want nothing", got)
			}
		})
	}
}

// The reply target is part of the send's intent. Repeating the key with the
// same target replays the first send; with another target or none, the
// outbox refuses the key and nothing else goes out.
func TestV2PrimaryIdempotencyKeyCoversTheReplyTarget(t *testing.T) {
	harness := newReplyFixture(t)
	handler := sendToConversationHandler(harness.app, &harness.deps)
	send := func(replyToID any) *mcp.CallToolResult {
		t.Helper()
		arguments := map[string]any{
			"conversation_id": replyThreadID,
			"message":         "replying",
			"idempotency_key": "reply-intent",
		}
		if replyToID != nil {
			arguments["reply_to_id"] = replyToID
		}
		return callTool(t, harness.ctx, handler, arguments)
	}

	first := send(replyTargetID)
	if first.IsError {
		t.Fatalf("first reply = %s", resultText(t, first))
	}
	replay := send(replyTargetID)
	if replay.IsError {
		t.Fatalf("replayed reply = %s", resultText(t, replay))
	}
	firstPayload, replayPayload := v2ToolPayload(t, first), v2ToolPayload(t, replay)
	assertV2ToolBool(t, replayPayload, "deduplicated", true)
	if v2ToolString(firstPayload, "outbox_id") != v2ToolString(replayPayload, "outbox_id") {
		t.Fatalf("replay outbox %q, want the first send's %q", v2ToolString(replayPayload, "outbox_id"), v2ToolString(firstPayload, "outbox_id"))
	}

	for name, replyToID := range map[string]any{"another target": replySecondID, "no target": nil} {
		conflict := send(replyToID)
		if text := resultText(t, conflict); !conflict.IsError || !strings.Contains(text, "idempotency") {
			t.Fatalf("same key with %s = %q (error %v), want an idempotency conflict", name, text, conflict.IsError)
		}
	}
	if got := harness.effects(t); got.TransportCalls != 1 || got.OutboxRows != 1 || got.OutgoingMessages != 1 {
		t.Fatalf("after one reply and its repeats: %+v, want one send", got)
	}
}

// reply_to_id must be a string. Any other type is an error before anything
// is queued, and a blank one sends a plain message.
func TestSendToConversationReplyToIDArgument(t *testing.T) {
	t.Run("in process", func(t *testing.T) {
		harness := newReplyFixture(t)
		handler := sendToConversationHandler(harness.app, &harness.deps)
		for _, bad := range []any{42, true, []any{replyTargetID}, map[string]any{"id": replyTargetID}} {
			result := callTool(t, harness.ctx, handler, map[string]any{
				"conversation_id": replyThreadID, "message": "replying", "reply_to_id": bad,
			})
			if text := resultText(t, result); !result.IsError || !strings.Contains(text, "reply_to_id must be a string") {
				t.Fatalf("reply_to_id %#v = %q (error %v), want a type error", bad, text, result.IsError)
			}
		}
		if got := harness.effects(t); got != (sendEffects{}) {
			t.Fatalf("mistyped reply_to_id left %+v, want nothing", got)
		}
		for _, blank := range []any{"", "   ", nil} {
			result := callTool(t, harness.ctx, handler, map[string]any{
				"conversation_id": replyThreadID, "message": "plain", "reply_to_id": blank,
			})
			if result.IsError {
				t.Fatalf("blank reply_to_id %#v = %s, want a plain send", blank, resultText(t, result))
			}
		}
		for _, request := range harness.sender.snapshotRequests() {
			if request.ReplyTo != nil {
				t.Fatalf("a blank reply_to_id quoted %+v", *request.ReplyTo)
			}
		}
	})
	t.Run("client mode", func(t *testing.T) {
		var submits int
		mux := http.NewServeMux()
		mux.HandleFunc("/api/status", func(w http.ResponseWriter, _ *http.Request) {
			json.NewEncoder(w).Encode(map[string]any{"v2_send": true, "v2_primary": true})
		})
		mux.HandleFunc("/api/v1/outbox/messages", func(w http.ResponseWriter, _ *http.Request) {
			submits++
			http.Error(w, "unexpected submit", http.StatusInternalServerError)
		})
		handler := daemonSendToConversationHandler(Options{Daemon: daemonClientFor(t, mux)})
		result := callTool(t, context.Background(), handler, map[string]any{
			"conversation_id": replyThreadID, "message": "replying", "reply_to_id": 42,
		})
		if text := resultText(t, result); !result.IsError || !strings.Contains(text, "reply_to_id must be a string") {
			t.Fatalf("client-mode reply_to_id 42 = %q (error %v), want a type error", text, result.IsError)
		}
		if submits != 0 {
			t.Fatalf("a mistyped reply_to_id reached the app %d times", submits)
		}
	})
}

// Routes that can't check a reply target refuse reply_to_id before they
// send anything, rather than drop it or pass it on unchecked.
func TestSendToConversationRefusesReplyToIDWithoutTheOutbox(t *testing.T) {
	t.Run("in process without v2", func(t *testing.T) {
		original := sendTextToConversation
		t.Cleanup(func() { sendTextToConversation = original })
		sendTextToConversation = func(*app.App, string, string) (conversationSummary, messageSummary, error) {
			t.Error("a refused reply reached the legacy sender")
			return conversationSummary{}, messageSummary{}, errors.New("legacy sender")
		}
		result := callTool(t, context.Background(), sendToConversationHandler(testApp(t)), map[string]any{
			"conversation_id": "conv-legacy", "message": "replying", "reply_to_id": "msg-1",
		})
		if text := resultText(t, result); !result.IsError || text != legacyReplyRefusal {
			t.Fatalf("legacy reply = %q (error %v), want the legacy refusal", text, result.IsError)
		}
	})
	t.Run("client mode with a legacy app", func(t *testing.T) {
		mux, sends := legacyDaemonMux(t)
		handler := daemonSendToConversationHandler(Options{Daemon: daemonClientFor(t, mux)})
		result := callTool(t, context.Background(), handler, map[string]any{
			"conversation_id": "conv-legacy", "message": "replying", "reply_to_id": "msg-1",
		})
		if text := resultText(t, result); !result.IsError || text != legacyDaemonReplyRefusal {
			t.Fatalf("legacy-app reply = %q (error %v), want the legacy-app refusal", text, result.IsError)
		}
		if got := sends(); got != 0 {
			t.Fatalf("a refused reply reached /api/send %d times", got)
		}
	})
}

// legacyDaemonMux is a legacy-primary app without v2 sending. It counts the
// /api/send calls it answers.
func legacyDaemonMux(t *testing.T) (http.Handler, func() int) {
	t.Helper()
	var mu sync.Mutex
	sends := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/api/status", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"connected": true})
	})
	mux.HandleFunc("/api/send", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		sends++
		mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"message_id": "msg-7", "status": "SUCCESS", "success": true})
	})
	return mux, func() int {
		mu.Lock()
		defer mu.Unlock()
		return sends
	}
}

// send_message passes reply_to_id to the outbox on the routes that go
// through it, and refuses it on the routes that can't check it.
func TestSendMessageReplyToID(t *testing.T) {
	t.Run("in process without v2 refuses every platform", func(t *testing.T) {
		originalWhatsApp, originalSignal := sendWhatsAppText, sendSignalText
		originalGetOrCreate, originalGoogle := getOrCreateGoogleConversation, sendGoogleTextPayload
		t.Cleanup(func() {
			sendWhatsAppText, sendSignalText = originalWhatsApp, originalSignal
			getOrCreateGoogleConversation, sendGoogleTextPayload = originalGetOrCreate, originalGoogle
		})
		failText := func(*app.App, string, string, string) (*db.Message, error) {
			t.Error("a refused reply reached a legacy sender")
			return nil, errors.New("legacy sender")
		}
		sendWhatsAppText, sendSignalText = failText, failText
		getOrCreateGoogleConversation = func(*app.App, string) (*gmproto.Conversation, error) {
			t.Error("a refused reply asked the phone for a thread")
			return nil, errors.New("get or create")
		}
		sendGoogleTextPayload = func(*app.App, *gmproto.SendMessageRequest) (*gmproto.SendMessageResponse, error) {
			t.Error("a refused reply reached the Google sender")
			return nil, errors.New("google sender")
		}
		handler := sendMessageHandler(testApp(t))
		for _, platform := range []string{"whatsapp", "signal", "sms"} {
			result := callTool(t, context.Background(), handler, map[string]any{
				"platform": platform, "recipient": "+15551230000", "message": "replying", "reply_to_id": "msg-1",
			})
			if text := resultText(t, result); !result.IsError || text != legacyReplyRefusal {
				t.Fatalf("%s reply without v2 = %q (error %v), want the legacy refusal", platform, text, result.IsError)
			}
		}
	})

	t.Run("in process SMS refuses before asking the phone for a thread", func(t *testing.T) {
		harness := newV2ToolHarness(t)
		original := getOrCreateGoogleConversation
		t.Cleanup(func() { getOrCreateGoogleConversation = original })
		getOrCreateGoogleConversation = func(*app.App, string) (*gmproto.Conversation, error) {
			t.Error("an SMS reply asked the phone for a thread")
			return nil, errors.New("get or create")
		}
		result := callTool(t, harness.ctx, sendMessageHandler(harness.app, &harness.deps), map[string]any{
			"recipient": "+15551230000", "message": "replying", "reply_to_id": "msg-1",
		})
		if text := resultText(t, result); !result.IsError || text != directSMSReplyRefusal {
			t.Fatalf("SMS reply = %q (error %v), want the SMS refusal", text, result.IsError)
		}
		if got := harness.effects(t); got != (sendEffects{}) {
			t.Fatalf("a refused SMS reply left %+v, want nothing", got)
		}
	})

	t.Run("in process Signal goes through the outbox", func(t *testing.T) {
		const (
			recipient   = "+15551230000"
			quotedID    = "signal:1700000300000"
			quotedAtMS  = int64(1_700_000_300_000)
			elsewhereID = "signal:1700000400000"
		)
		seed := func(t *testing.T) *v2ToolHarness {
			t.Helper()
			harness := newV2ToolHarness(t)
			harness.deps.Registry.(*v2ToolRegistry).accountID = "signal-primary"
			store := harness.app.Store
			for _, conversation := range []*db.Conversation{
				{ConversationID: "signal:" + recipient, Name: recipient, Participants: `[]`, SourcePlatform: "signal"},
				{ConversationID: "signal:+15559990000", Name: "Elsewhere", Participants: `[]`, SourcePlatform: "signal"},
			} {
				if err := store.UpsertConversation(conversation); err != nil {
					t.Fatalf("seed conversation %s: %v", conversation.ConversationID, err)
				}
			}
			for _, message := range []*db.Message{
				{MessageID: quotedID, ConversationID: "signal:" + recipient, Body: "signal quote", TimestampMS: quotedAtMS, SourcePlatform: "signal"},
				{MessageID: elsewhereID, ConversationID: "signal:+15559990000", Body: "elsewhere", TimestampMS: quotedAtMS + 100_000, SourcePlatform: "signal"},
			} {
				if err := store.UpsertMessage(message); err != nil {
					t.Fatalf("seed message %s: %v", message.MessageID, err)
				}
			}
			return harness
		}

		harness := seed(t)
		result := callTool(t, harness.ctx, sendMessageHandler(harness.app, &harness.deps), map[string]any{
			"platform": "signal", "recipient": recipient, "message": "replying", "reply_to_id": quotedID,
		})
		if result.IsError {
			t.Fatalf("Signal reply = %s", resultText(t, result))
		}
		requests := harness.sender.snapshotRequests()
		if len(requests) != 1 {
			t.Fatalf("transport requests = %d, want 1", len(requests))
		}
		assertQuotes(t, requests[0], quotedID, "signal quote", quotedAtMS)

		refused := seed(t)
		result = callTool(t, refused.ctx, sendMessageHandler(refused.app, &refused.deps), map[string]any{
			"platform": "signal", "recipient": recipient, "message": "replying", "reply_to_id": elsewhereID,
		})
		assertReplyRefused(t, result, "signal:"+recipient, elsewhereID)
		if got := refused.effects(t); got != (sendEffects{}) {
			t.Fatalf("a refused Signal reply left %+v, want nothing", got)
		}
	})

	t.Run("client mode passes it to the app's outbox", func(t *testing.T) {
		var mu sync.Mutex
		var submitted []map[string]any
		refuse := false
		mux := http.NewServeMux()
		mux.HandleFunc("/api/status", func(w http.ResponseWriter, _ *http.Request) {
			json.NewEncoder(w).Encode(map[string]any{"v2_send": true, "v2_primary": true})
		})
		mux.HandleFunc("/api/v1/outbox/messages", func(w http.ResponseWriter, r *http.Request) {
			var request map[string]any
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode submission: %v", err)
			}
			mu.Lock()
			submitted = append(submitted, request)
			refusing := refuse
			mu.Unlock()
			if refusing {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnprocessableEntity)
				json.NewEncoder(w).Encode(map[string]any{"error": replyRefusalCode})
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"outbox_id": "out-1", "state": "queued"})
		})
		mux.HandleFunc("/api/v1/outbox/out-1", func(w http.ResponseWriter, _ *http.Request) {
			json.NewEncoder(w).Encode(map[string]any{"outbox_id": "out-1", "state": "confirmed", "remote_message_id": "remote-9"})
		})
		handler := daemonSendMessageHandler(Options{Daemon: daemonClientFor(t, mux)})
		arguments := map[string]any{
			"platform": "whatsapp", "recipient": "+15551230000", "message": "replying", "reply_to_id": "wa-msg-1",
		}

		if result := callTool(t, context.Background(), handler, arguments); result.IsError {
			t.Fatalf("client-mode send_message reply = %s", resultText(t, result))
		}
		mu.Lock()
		refuse = true
		mu.Unlock()
		refused := callTool(t, context.Background(), handler, arguments)
		mu.Lock()
		defer mu.Unlock()
		if len(submitted) != 2 {
			t.Fatalf("submissions = %d, want 2", len(submitted))
		}
		for _, submission := range submitted {
			if submission["reply_to_id"] != "wa-msg-1" {
				t.Fatalf("submission = %v, want reply_to_id wa-msg-1", submission)
			}
		}
		conversationID, _ := submitted[1]["conversation_id"].(string)
		assertReplyRefused(t, refused, conversationID, "wa-msg-1")
	})

	t.Run("client mode with a legacy app refuses it", func(t *testing.T) {
		mux, sends := legacyDaemonMux(t)
		handler := daemonSendMessageHandler(Options{Daemon: daemonClientFor(t, mux)})
		result := callTool(t, context.Background(), handler, map[string]any{
			"platform": "whatsapp", "recipient": "+15551230000", "message": "replying", "reply_to_id": "wa-msg-1",
		})
		if text := resultText(t, result); !result.IsError || text != legacyDaemonReplyRefusal {
			t.Fatalf("legacy-app reply = %q (error %v), want the legacy-app refusal", text, result.IsError)
		}
		if got := sends(); got != 0 {
			t.Fatalf("a refused reply reached /api/send %d times", got)
		}
	})
}

// The v2 send_to_conversation and send_message descriptors offer reply_to_id
// as an optional string that says what the app refuses; the legacy
// descriptors and send_group_message don't offer it.
func TestReplyToIDDescriptors(t *testing.T) {
	for _, tool := range []mcp.Tool{sendToConversationTool(true), sendMessageTool(true)} {
		property, ok := tool.InputSchema.Properties["reply_to_id"].(map[string]any)
		if !ok {
			t.Fatalf("%s reply_to_id schema = %#v, want a property object", tool.Name, tool.InputSchema.Properties["reply_to_id"])
		}
		if property["type"] != "string" {
			t.Fatalf("%s reply_to_id type = %#v, want string", tool.Name, property["type"])
		}
		description, _ := property["description"].(string)
		neverDropped := map[string]string{
			"send_to_conversation": "refused, never dropped",
			"send_message":         "neither drops it",
		}[tool.Name]
		for _, want := range []string{"message_id", "get_conversation", replyRefusalCode, "sends nothing", neverDropped} {
			if !strings.Contains(description, want) {
				t.Fatalf("%s reply_to_id description lacks %q: %q", tool.Name, want, description)
			}
		}
		for _, required := range tool.InputSchema.Required {
			if required == "reply_to_id" {
				t.Fatalf("%s requires reply_to_id", tool.Name)
			}
		}
	}
	for _, tool := range []mcp.Tool{sendToConversationTool(), sendMessageTool(), sendGroupMessageTool(true), sendGroupMessageTool()} {
		if _, offered := tool.InputSchema.Properties["reply_to_id"]; offered {
			t.Fatalf("%s offers reply_to_id", tool.Name)
		}
	}
}

// replyCase is one generated send_to_conversation call: which reply target it
// names, the text it sends, and the ID or blank it drew for an unknown or
// blank target.
type replyCase struct {
	Target replyTargetKind
	Body   string
	Drawn  string
}

type replyTargetKind int

const (
	replyToTarget replyTargetKind = iota
	replyToSecond
	replyToOwn
	replyToElsewhere
	replyToUnknown
	replyToBlank
	replyToNone
	replyTargetKinds
)

func (k replyTargetKind) String() string {
	return [...]string{"target", "second", "own", "elsewhere", "unknown", "blank", "none"}[k]
}

func (k replyTargetKind) refused() bool {
	return k == replyToElsewhere || k == replyToUnknown
}

type replyScript struct{ Cases []replyCase }

func (replyScript) Generate(r *rand.Rand, _ int) reflect.Value {
	script := replyScript{}
	for i, count := 0, 1+r.Intn(4); i < count; i++ {
		call := replyCase{
			Target: replyTargetKind(r.Intn(int(replyTargetKinds))),
			Body:   fmt.Sprintf("body-%d", r.Intn(1_000_000)),
		}
		switch call.Target {
		case replyToUnknown:
			call.Drawn = fmt.Sprintf("unknown-%d", r.Intn(1_000))
		case replyToBlank:
			call.Drawn = strings.Repeat(" ", r.Intn(3))
		}
		script.Cases = append(script.Cases, call)
	}
	return reflect.ValueOf(script)
}

func storedRemoteID(t *testing.T, store *sqlite.Store, messageID string) string {
	t.Helper()
	messages, err := sqlite.NewMessageRepository(store, time.Now)
	if err != nil {
		t.Fatalf("NewMessageRepository(): %v", err)
	}
	message, err := messages.GetMessage(context.Background(), messageID)
	if err != nil {
		t.Fatalf("GetMessage(%q): %v", messageID, err)
	}
	return message.RemoteMessageID
}

// For any sequence of sends to reply-thread, in process and in client mode:
// a refused reply target (another thread's message, or no message) changes
// nothing, with no ID, outbox row, stored message or transport call, and both
// modes refuse it alike. Any other send queues exactly one message, and the
// transport quotes the named message's remote ID, or nothing when the call
// named no target. Named targets include the test's own previous send.
func TestReplyTargetPropertiesHoldInProcessAndThroughTheApp(t *testing.T) {
	harness := newReplyFixture(t)
	modes := []struct {
		name    string
		handler server.ToolHandlerFunc
	}{
		{"in process", sendToConversationHandler(harness.app, &harness.deps)},
		{"client mode", daemonSendToConversationHandler(Options{V2Primary: true, Daemon: replyAppClient(t, harness)})},
	}
	sent, quotedOwn := 0, 0
	lastSent := "" // local_message_id of the newest send to the thread

	property := func(script replyScript) bool {
		for i, call := range script.Cases {
			for _, mode := range modes {
				arguments := map[string]any{"conversation_id": replyThreadID, "message": call.Body}
				wantQuoted := ""
				switch call.Target {
				case replyToTarget:
					arguments["reply_to_id"], wantQuoted = replyTargetID, replyTargetRemoteID
				case replyToSecond:
					arguments["reply_to_id"], wantQuoted = replySecondID, replySecondRemoteID
				case replyToOwn:
					if lastSent != "" {
						arguments["reply_to_id"], wantQuoted = lastSent, storedRemoteID(t, harness.deps.V2Store, lastSent)
						quotedOwn++
					}
				case replyToElsewhere:
					arguments["reply_to_id"] = replyElsewhereID
				case replyToUnknown, replyToBlank:
					arguments["reply_to_id"] = call.Drawn
				}
				before := harness.effects(t)
				result := callTool(t, harness.ctx, mode.handler, arguments)
				after := harness.effects(t)
				payload := v2ToolPayload(t, result)

				if call.Target.refused() {
					if !result.IsError || v2ToolString(payload, "error_code") != replyRefusalCode {
						t.Logf("%s call %d (%s): %s, want reply_target_unavailable", mode.name, i, call.Target, resultText(t, result))
						return false
					}
					if after != before {
						t.Logf("%s call %d (%s): refused reply changed %+v to %+v", mode.name, i, call.Target, before, after)
						return false
					}
					continue
				}

				if result.IsError || v2ToolString(payload, "state") != string(messaging.OutboxConfirmed) {
					t.Logf("%s call %d (%s): %s, want a confirmed send", mode.name, i, call.Target, resultText(t, result))
					return false
				}
				sent++
				lastSent = v2ToolString(payload, "local_message_id")
				if after.TransportCalls != before.TransportCalls+1 || after.OutboxRows != before.OutboxRows+1 ||
					after.OutgoingMessages != before.OutgoingMessages+1 {
					t.Logf("%s call %d (%s): effects %+v to %+v, want one more send", mode.name, i, call.Target, before, after)
					return false
				}
				requests := harness.sender.snapshotRequests()
				last := requests[len(requests)-1]
				quoted := ""
				if last.ReplyTo != nil {
					quoted = last.ReplyTo.RemoteID
				}
				if last.Body != call.Body || quoted != wantQuoted {
					t.Logf("%s call %d (%s): transport got body %q quoting %q, want %q quoting %q",
						mode.name, i, call.Target, last.Body, quoted, call.Body, wantQuoted)
					return false
				}
			}
		}
		return true
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 10, Rand: rand.New(rand.NewSource(20261107))}); err != nil {
		t.Fatal(err)
	}
	if sent == 0 || quotedOwn == 0 {
		t.Fatalf("the generated scripts made %d sends and quoted their own %d times, want both", sent, quotedOwn)
	}
}
