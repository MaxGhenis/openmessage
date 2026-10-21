package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/maxghenis/openmessage/internal/app"
	"github.com/maxghenis/openmessage/internal/db"
)

// fakeReadCursor records the remote mark-read calls the handler makes.
type fakeReadCursor struct {
	mu    sync.Mutex
	calls [][2]string
	err   error
}

func (f *fakeReadCursor) MarkRead(conversationID, messageID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, [2]string{conversationID, messageID})
	return f.err
}

func (f *fakeReadCursor) seen() [][2]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][2]string(nil), f.calls...)
}

// seedThread inserts a conversation plus an incoming and an outgoing message.
// The outgoing message is the NEWEST so the tests prove the derived cursor
// skips it.
func seedThread(t *testing.T, a *app.App, convID string) {
	t.Helper()
	if err := a.Store.UpsertConversation(&db.Conversation{
		ConversationID: convID,
		Name:           "Test Thread",
		UnreadCount:    1,
		LastMessageTS:  2000,
	}); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}
	for _, m := range []*db.Message{
		{MessageID: convID + "-in", ConversationID: convID, Body: "hi", TimestampMS: 1000, IsFromMe: false},
		{MessageID: convID + "-out", ConversationID: convID, Body: "hello", TimestampMS: 2000, IsFromMe: true},
	} {
		if err := a.Store.UpsertMessage(m); err != nil {
			t.Fatalf("seed message: %v", err)
		}
	}
}

func unreadCount(t *testing.T, a *app.App, convID string) int {
	t.Helper()
	conv, err := a.Store.GetConversation(convID)
	if err != nil {
		t.Fatalf("get conversation: %v", err)
	}
	if conv == nil {
		t.Fatalf("conversation %s missing", convID)
	}
	return conv.UnreadCount
}

func callMarkRead(t *testing.T, handlerApp *app.App, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	var req mcp.CallToolRequest
	req.Params.Arguments = args
	result, err := markReadHandler(handlerApp)(context.Background(), req)
	if err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	return result
}

func TestMarkReadClearsLocalAndRemote(t *testing.T) {
	a := testApp(t)
	seedThread(t, a, "conv-1")

	cursor := &fakeReadCursor{}
	restore := app.SwapReadCursorForTest(cursor)
	t.Cleanup(restore)

	result := callMarkRead(t, a, map[string]any{"conversation_id": "conv-1"})
	if result.IsError {
		t.Fatalf("expected success, got error result: %+v", result.Content)
	}
	if got := unreadCount(t, a, "conv-1"); got != 0 {
		t.Fatalf("unread_count = %d, want 0", got)
	}

	calls := cursor.seen()
	if len(calls) != 1 {
		t.Fatalf("expected 1 remote call, got %d (%v)", len(calls), calls)
	}
	// The cursor must point at the newest INCOMING message, not the newer
	// outgoing one.
	if calls[0] != [2]string{"conv-1", "conv-1-in"} {
		t.Fatalf("remote call = %v, want [conv-1 conv-1-in]", calls[0])
	}
}

func TestMarkReadHonorsExplicitMessageID(t *testing.T) {
	a := testApp(t)
	seedThread(t, a, "conv-1")

	cursor := &fakeReadCursor{}
	t.Cleanup(app.SwapReadCursorForTest(cursor))

	result := callMarkRead(t, a, map[string]any{
		"conversation_id": "conv-1",
		"message_id":      "explicit-id",
	})
	if result.IsError {
		t.Fatalf("expected success, got %+v", result.Content)
	}
	calls := cursor.seen()
	if len(calls) != 1 || calls[0][1] != "explicit-id" {
		t.Fatalf("remote calls = %v, want one call with explicit-id", calls)
	}
}

func TestMarkReadReportsRemoteFailureAsError(t *testing.T) {
	a := testApp(t)
	seedThread(t, a, "conv-1")

	cursor := &fakeReadCursor{err: fmt.Errorf("google said no")}
	t.Cleanup(app.SwapReadCursorForTest(cursor))

	result := callMarkRead(t, a, map[string]any{"conversation_id": "conv-1"})
	if !result.IsError {
		t.Fatal("expected an error result when the phone was not reached")
	}
	// The local clear still happened — the tool must not claim otherwise, and
	// must not roll it back.
	if got := unreadCount(t, a, "conv-1"); got != 0 {
		t.Fatalf("unread_count = %d, want 0 (local clear should persist)", got)
	}
	text := result.Content[0].(mcp.TextContent).Text
	if want := "NOT on the phone"; !strings.Contains(text, want) {
		t.Fatalf("error text %q should mention %q", text, want)
	}
}

func TestMarkReadRequiresConversationID(t *testing.T) {
	a := testApp(t)
	result := callMarkRead(t, a, map[string]any{})
	if !result.IsError {
		t.Fatal("expected an error result with no conversation id")
	}
}

func TestMarkReadBulkMarksEveryThread(t *testing.T) {
	a := testApp(t)
	for _, id := range []string{"conv-1", "conv-2", "conv-3"} {
		seedThread(t, a, id)
	}

	cursor := &fakeReadCursor{}
	t.Cleanup(app.SwapReadCursorForTest(cursor))

	result := callMarkRead(t, a, map[string]any{
		"conversation_ids": []any{"conv-1", "conv-2", "conv-3"},
	})
	if result.IsError {
		t.Fatalf("expected success, got %+v", result.Content)
	}
	if got := len(cursor.seen()); got != 3 {
		t.Fatalf("remote calls = %d, want 3", got)
	}
	for _, id := range []string{"conv-1", "conv-2", "conv-3"} {
		if got := unreadCount(t, a, id); got != 0 {
			t.Fatalf("%s unread_count = %d, want 0", id, got)
		}
	}
	payload := structuredMap(t, result)
	synced, ok := payload["synced_to_phone"].([]string)
	if !ok || len(synced) != 3 {
		t.Fatalf("synced_to_phone = %#v, want 3 ids", payload["synced_to_phone"])
	}
}

func TestMarkReadBulkDedupesAndIgnoresBlanks(t *testing.T) {
	got := markReadTargets(map[string]any{
		"conversation_id":  "conv-1",
		"conversation_ids": []any{"conv-1", " conv-2 ", "", "conv-2"},
	})
	want := []string{"conv-1", "conv-2"}
	if len(got) != len(want) {
		t.Fatalf("targets = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("targets = %v, want %v", got, want)
		}
	}
}

func TestDaemonMarkReadPostsToApp(t *testing.T) {
	var posted []map[string]any
	client := daemonClientFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/mark-read" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
		}
		posted = append(posted, body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))

	var req mcp.CallToolRequest
	req.Params.Arguments = map[string]any{"conversation_id": "conv-9"}
	result, err := daemonMarkReadHandler(Options{Daemon: client})(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if result.IsError {
		t.Fatalf("expected success, got %+v", result.Content)
	}
	if len(posted) != 1 || posted[0]["conversation_id"] != "conv-9" {
		t.Fatalf("posted = %#v, want one call for conv-9", posted)
	}
}

func TestDaemonMarkReadReportsDaemonDown(t *testing.T) {
	var req mcp.CallToolRequest
	req.Params.Arguments = map[string]any{"conversation_id": "conv-9"}
	result, err := daemonMarkReadHandler(Options{Daemon: deadDaemonClient()})(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected an error result when the daemon is down")
	}
}
