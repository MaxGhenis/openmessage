package tools

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/readsource"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

type toolRoutingReadSource struct {
	readsource.ReadSource
	listCalls      int
	searchCalls    int
	statusCalls    int
	getConvCalls   int
	batchCalls     int
	rangeCalls     int
	lastQuery      string
	lastFilter     db.SearchFilter
	lastBatchIDs   []string
	lastBatchLimit int
	lastAfterMS    int64
	lastBeforeMS   int64
}

func (s *toolRoutingReadSource) ListConversations(limit int) ([]*db.Conversation, error) {
	s.listCalls++
	return []*db.Conversation{{
		ConversationID: "v2-conversation",
		Name:           "V2 Conversation",
		SourcePlatform: "sms",
		LastMessageTS:  200,
	}}, nil
}

func (s *toolRoutingReadSource) SearchMessagesFiltered(query string, filter db.SearchFilter) ([]*db.Message, error) {
	s.searchCalls++
	s.lastQuery = query
	s.lastFilter = filter
	return []*db.Message{{
		MessageID:      "v2-message",
		ConversationID: "v2-conversation",
		SenderName:     "V2 Alice",
		Body:           "v2 routed body",
		TimestampMS:    200,
		SourcePlatform: "sms",
	}}, nil
}

func (s *toolRoutingReadSource) PlatformStats() ([]db.PlatformStat, error) {
	s.statusCalls++
	return []db.PlatformStat{{Platform: "sms", Count: 7, LatestMS: 200, LatestRecvMS: 100}}, nil
}

func (s *toolRoutingReadSource) GetMessagesByConversation(conversationID string, limit int) ([]*db.Message, error) {
	s.getConvCalls++
	return []*db.Message{{
		MessageID:      "v2-thread-message",
		ConversationID: conversationID,
		SenderName:     "V2 Thread Sender",
		Body:           "v2 thread body",
		TimestampMS:    200,
		SourcePlatform: "sms",
	}}, nil
}

func (s *toolRoutingReadSource) GetConversation(id string) (*db.Conversation, error) {
	return &db.Conversation{ConversationID: id, Name: "V2 Thread", SourcePlatform: "sms"}, nil
}

func (s *toolRoutingReadSource) GetMessagesByConversations(conversationIDs []string, limit int) ([]*db.Message, error) {
	s.batchCalls++
	s.lastBatchIDs = conversationIDs
	s.lastBatchLimit = limit
	return []*db.Message{{
		MessageID:      "v2-person-message",
		ConversationID: "v2-conversation",
		SenderName:     "V2 Alice",
		Body:           "v2 person body",
		TimestampMS:    200,
		SourcePlatform: "sms",
	}}, nil
}

func (s *toolRoutingReadSource) GetMessagesByConversationsRange(conversationIDs []string, afterMS, beforeMS int64, limit int) ([]*db.Message, error) {
	s.rangeCalls++
	s.lastBatchIDs = conversationIDs
	s.lastBatchLimit = limit
	s.lastAfterMS = afterMS
	s.lastBeforeMS = beforeMS
	return []*db.Message{{
		MessageID:      "v2-person-range-message",
		ConversationID: "v2-conversation",
		SenderName:     "V2 Alice",
		Body:           "v2 person range body",
		TimestampMS:    200,
		SourcePlatform: "sms",
	}}, nil
}

func TestR5MCPReadHandlersUseConfiguredSource(t *testing.T) {
	a := testApp(t)
	reads := &toolRoutingReadSource{}
	options := Options{Reads: reads, V2Primary: true}

	t.Run("list conversations", func(t *testing.T) {
		result, err := listConversationsHandler(a, options)(context.Background(), toolRequest(nil))
		if err != nil {
			t.Fatal(err)
		}
		if result.IsError || !strings.Contains(resultText(t, result), "V2 Conversation") {
			t.Fatalf("unexpected result: %#v", result)
		}
		if reads.listCalls != 1 {
			t.Fatalf("ListConversations calls = %d, want 1", reads.listCalls)
		}
	})

	t.Run("get messages", func(t *testing.T) {
		result, err := getMessagesHandler(a, options)(context.Background(), toolRequest(map[string]any{
			"phone_number": "+15551230000",
			"after":        "1970-01-01",
			"limit":        float64(12),
		}))
		if err != nil {
			t.Fatal(err)
		}
		if result.IsError || !strings.Contains(resultText(t, result), "v2 routed body") {
			t.Fatalf("unexpected result: %#v", result)
		}
		if reads.lastQuery != "" || reads.lastFilter.Phone != "+15551230000" || reads.lastFilter.Limit != 12 {
			t.Fatalf("get_messages filter = %#v, query=%q", reads.lastFilter, reads.lastQuery)
		}
	})

	t.Run("search messages", func(t *testing.T) {
		result, err := searchMessagesHandler(a, options)(context.Background(), toolRequest(map[string]any{
			"query":        "routed",
			"phone_number": "+15551230000",
			"limit":        float64(9),
		}))
		if err != nil {
			t.Fatal(err)
		}
		if result.IsError || !strings.Contains(resultText(t, result), "v2 routed body") {
			t.Fatalf("unexpected result: %#v", result)
		}
		if reads.lastQuery != "routed" || reads.lastFilter.Phone != "+15551230000" || reads.lastFilter.Limit != 9 {
			t.Fatalf("search_messages filter = %#v, query=%q", reads.lastFilter, reads.lastQuery)
		}
	})
}

func TestR5LegacyGetMessagesKeepsExactStoreQueryAndFormatting(t *testing.T) {
	a := testApp(t)
	for _, message := range []*db.Message{
		{MessageID: "same-time-a", ConversationID: "legacy", SenderName: "Alice", Body: "first", TimestampMS: 1234},
		{MessageID: "same-time-b", ConversationID: "legacy", SenderName: "Bob", Body: "second", TimestampMS: 1234},
	} {
		if err := a.Store.UpsertMessage(message); err != nil {
			t.Fatal(err)
		}
	}
	reads := &toolRoutingReadSource{}
	result, err := getMessagesHandler(a, Options{Reads: reads})(context.Background(), toolRequest(map[string]any{
		"limit": float64(20),
	}))
	if err != nil {
		t.Fatal(err)
	}
	if reads.searchCalls != 0 {
		t.Fatalf("legacy get_messages used the replacement search path %d times", reads.searchCalls)
	}
	wantMessages, err := a.Store.GetMessages("", 0, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	var want strings.Builder
	want.WriteString(messagePreamble)
	for _, message := range wantMessages {
		want.WriteString(formatMessageLine(message))
		want.WriteByte('\n')
	}
	if got := resultText(t, result); got != want.String() {
		t.Fatalf("legacy result changed\n got: %q\nwant: %q", got, want.String())
	}
}

func TestR5MCPGetStatusReadsV2CoverageWithoutChangingLegacyShape(t *testing.T) {
	a := testApp(t)
	reads := &toolRoutingReadSource{}

	legacy, err := getStatusHandler(a, Options{Reads: reads})(context.Background(), toolRequest(nil))
	if err != nil {
		t.Fatal(err)
	}
	legacyPayload := structuredMap(t, legacy)
	if _, exists := legacyPayload["stored_platforms"]; exists {
		t.Fatalf("legacy result gained v2-only field: %#v", legacyPayload)
	}
	if reads.statusCalls != 0 {
		t.Fatalf("legacy get_status called configured source %d times", reads.statusCalls)
	}

	v2, err := getStatusHandler(a, Options{Reads: reads, V2Primary: true})(context.Background(), toolRequest(nil))
	if err != nil {
		t.Fatal(err)
	}
	if v2.IsError {
		t.Fatalf("unexpected v2 tool error: %#v", v2)
	}
	payload := structuredMap(t, v2)
	stats, ok := payload["stored_platforms"].([]platformStatSummary)
	if !ok || len(stats) != 1 || stats[0].Platform != "sms" || stats[0].Count != 7 {
		t.Fatalf("stored_platforms = %#v", payload["stored_platforms"])
	}
	if reads.statusCalls != 1 {
		t.Fatalf("v2 get_status calls = %d, want 1", reads.statusCalls)
	}
}

func TestR5MCPPersonStoryAndVizToolsUnavailableInV2Primary(t *testing.T) {
	a := testApp(t)
	mcpServer := server.NewMCPServer("r5-routing", "test")
	RegisterWithOptions(mcpServer, a, Options{Reads: a.Store, V2Primary: true})

	for _, name := range []string{
		"conversation_stats",
		"generate_story",
		"person_stats",
		"generate_person_story",
		"generate_viz",
		"render_story",
	} {
		t.Run(name, func(t *testing.T) {
			registered := mcpServer.GetTool(name)
			if registered == nil {
				t.Fatalf("tool %q was not registered", name)
			}
			result, err := registered.Handler(context.Background(), toolRequest(nil))
			if err != nil {
				t.Fatal(err)
			}
			if !result.IsError {
				t.Fatalf("tool %q did not return an MCP error", name)
			}
			got := resultText(t, result)
			if got != unavailableWhileV2Serving {
				t.Fatalf("tool %q error = %q", name, got)
			}
			// The error must point agents at the tools that DO serve v2
			// reads, not dead-end them.
			for _, alternative := range []string{"get_person_messages", "search_messages", "get_messages"} {
				if !strings.Contains(got, alternative) {
					t.Fatalf("tool %q error does not name alternative %q: %q", name, alternative, got)
				}
			}
		})
	}
}

func TestR5MCPGetPersonMessagesRoutesToConfiguredSourceInV2Primary(t *testing.T) {
	a := testApp(t)
	reads := &toolRoutingReadSource{}
	options := Options{Reads: reads, V2Primary: true}

	result, err := getPersonMessagesHandler(a, options)(context.Background(), toolRequest(map[string]any{
		"name":  "V2 Conversation",
		"limit": float64(25),
	}))
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError || !strings.Contains(resultText(t, result), "v2 person body") {
		t.Fatalf("unexpected result: %#v", result)
	}
	if reads.listCalls != 1 || reads.batchCalls != 1 {
		t.Fatalf("calls = list %d batch %d, want 1/1", reads.listCalls, reads.batchCalls)
	}
	if len(reads.lastBatchIDs) != 1 || reads.lastBatchIDs[0] != "v2-conversation" || reads.lastBatchLimit != 25 {
		t.Fatalf("batch args = %v limit %d", reads.lastBatchIDs, reads.lastBatchLimit)
	}
}

func TestR5MCPGetPersonMessagesRangeRoutesToConfiguredSourceInV2Primary(t *testing.T) {
	a := testApp(t)
	reads := &toolRoutingReadSource{}
	options := Options{Reads: reads, V2Primary: true}

	result, err := getPersonMessagesRangeHandler(a, options)(context.Background(), toolRequest(map[string]any{
		"name":   "V2 Conversation",
		"after":  "2024-01-01",
		"before": "2024-03-31",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError || !strings.Contains(resultText(t, result), "v2 person range body") {
		t.Fatalf("unexpected result: %#v", result)
	}
	if reads.listCalls != 1 || reads.rangeCalls != 1 {
		t.Fatalf("calls = list %d range %d, want 1/1", reads.listCalls, reads.rangeCalls)
	}
	// Same local-time day bounds as the CLI and /api/search/messages.
	wantAfterMS, err := db.ParseDayBound("2024-01-01", false)
	if err != nil {
		t.Fatal(err)
	}
	wantBeforeMS, err := db.ParseDayBound("2024-03-31", true)
	if err != nil {
		t.Fatal(err)
	}
	if reads.lastAfterMS != wantAfterMS || reads.lastBeforeMS != wantBeforeMS {
		t.Fatalf("range = [%d, %d], want [%d, %d]", reads.lastAfterMS, reads.lastBeforeMS, wantAfterMS, wantBeforeMS)
	}
	if len(reads.lastBatchIDs) != 1 || reads.lastBatchIDs[0] != "v2-conversation" {
		t.Fatalf("range conversation ids = %v", reads.lastBatchIDs)
	}
}

func TestR5MCPPersonMessageToolsClampAgentSuppliedLimits(t *testing.T) {
	a := testApp(t)
	reads := &toolRoutingReadSource{}
	options := Options{Reads: reads, V2Primary: true}

	if _, err := getPersonMessagesHandler(a, options)(context.Background(), toolRequest(map[string]any{
		"name":  "V2 Conversation",
		"limit": float64(1_000_000),
	})); err != nil {
		t.Fatal(err)
	}
	if reads.lastBatchLimit != maxPersonMessagesLimit {
		t.Fatalf("get_person_messages limit = %d, want clamped to %d", reads.lastBatchLimit, maxPersonMessagesLimit)
	}

	if _, err := getPersonMessagesRangeHandler(a, options)(context.Background(), toolRequest(map[string]any{
		"name":   "V2 Conversation",
		"after":  "2024-01-01",
		"before": "2024-03-31",
		"limit":  float64(-5),
	})); err != nil {
		t.Fatal(err)
	}
	if reads.lastBatchLimit != 1 {
		t.Fatalf("get_person_messages_range limit = %d, want clamped to 1", reads.lastBatchLimit)
	}
}

// The un-gating itself, asserted at registration level: re-wrapping either
// tool in unavailableInV2Primary would fail here, not just in the direct
// handler tests above.
func TestR5MCPPersonMessageToolsRegisteredUngatedInV2Primary(t *testing.T) {
	a := testApp(t)
	reads := &toolRoutingReadSource{}
	mcpServer := server.NewMCPServer("r5-routing", "test")
	RegisterWithOptions(mcpServer, a, Options{Reads: reads, V2Primary: true})

	for name, args := range map[string]map[string]any{
		"get_person_messages":       {"name": "V2 Conversation"},
		"get_person_messages_range": {"name": "V2 Conversation", "after": "2024-01-01", "before": "2024-03-31"},
	} {
		t.Run(name, func(t *testing.T) {
			registered := mcpServer.GetTool(name)
			if registered == nil {
				t.Fatalf("tool %q was not registered", name)
			}
			result, err := registered.Handler(context.Background(), toolRequest(args))
			if err != nil {
				t.Fatal(err)
			}
			if result.IsError {
				t.Fatalf("tool %q is gated in v2-primary: %q", name, resultText(t, result))
			}
			if !strings.Contains(resultText(t, result), "v2 person") {
				t.Fatalf("tool %q did not serve the configured v2 source: %q", name, resultText(t, result))
			}
		})
	}
	if reads.batchCalls != 1 || reads.rangeCalls != 1 {
		t.Fatalf("configured source calls = batch %d range %d, want 1/1", reads.batchCalls, reads.rangeCalls)
	}
}

func TestR5MCPV2PrimarySubmitUsesNativeConversationID(t *testing.T) {
	harness := newV2ToolHarness(t)
	harness.deps.V2Primary = true
	nowMS := time.Now().UnixMilli()
	if err := harness.deps.V2Store.UpsertAccount(sqlite.Account{
		AccountID:   "google-primary",
		BridgeKey:   "google_messages",
		DisplayName: "Google",
		Mode:        sqlite.AccountModeLive,
		Enabled:     true,
		ConfigJSON:  "{}",
		CreatedAtMS: nowMS,
		UpdatedAtMS: nowMS,
	}); err != nil {
		t.Fatal(err)
	}
	const conversationID = "v2-native-tool-conversation"
	if err := harness.deps.V2Store.UpsertConversation(sqlite.Conversation{
		ConversationID:       conversationID,
		AccountID:            "google-primary",
		RemoteConversationID: "remote-v2-native-tool-conversation",
		Kind:                 sqlite.ConversationKindDirect,
		Title:                "Native tool thread",
		NotificationMode:     sqlite.NotificationModeAll,
		MetadataJSON:         "{}",
		CreatedAtMS:          nowMS,
		UpdatedAtMS:          nowMS,
	}); err != nil {
		t.Fatal(err)
	}

	result, err := sendToConversationHandler(harness.app, &harness.deps)(context.Background(), toolRequest(map[string]any{
		"conversation_id": conversationID,
		"message":         "native tool route",
		"idempotency_key": "native-tool-route-key",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("native tool route returned error: %#v", result)
	}
	if conversation, _ := harness.app.Store.GetConversation(conversationID); conversation != nil {
		t.Fatalf("native tool route unexpectedly created legacy conversation: %+v", conversation)
	}
}

func toolRequest(arguments map[string]any) mcp.CallToolRequest {
	req := mcp.CallToolRequest{}
	req.Params.Arguments = arguments
	return req
}

func resultText(t *testing.T, result *mcp.CallToolResult) string {
	t.Helper()
	if len(result.Content) == 0 {
		t.Fatal("tool result has no content")
	}
	content, ok := result.Content[0].(mcp.TextContent)
	if !ok {
		t.Fatalf("tool content = %T, want text", result.Content[0])
	}
	return content.Text
}

func TestR5GetConversationRoutesToConfiguredSource(t *testing.T) {
	a := testApp(t)
	reads := &toolRoutingReadSource{}
	// v2-primary emits v2 conversation ids; get_conversation must resolve them
	// against the same store, not the legacy store (which lacks that id).
	result, err := getConversationHandler(a, Options{Reads: reads, V2Primary: true})(
		context.Background(),
		toolRequest(map[string]any{"conversation_id": "v2-conversation"}),
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("get_conversation errored: %#v", result)
	}
	if reads.getConvCalls != 1 {
		t.Fatalf("GetMessagesByConversation via configured source = %d, want 1", reads.getConvCalls)
	}
	if text := resultText(t, result); !strings.Contains(text, "v2 thread body") {
		t.Fatalf("result did not come from the configured v2 source: %q", text)
	}
}
