package v2read

import (
	"fmt"
	"testing"

	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

func seedBatchConversation(t *testing.T, store *sqlite.Store, conversationID string) {
	t.Helper()
	seedSourceConversation(t, store, sqlite.Conversation{
		ConversationID:       conversationID,
		AccountID:            "batch-account",
		RemoteConversationID: "remote-" + conversationID,
		Kind:                 sqlite.ConversationKindDirect,
		NotificationMode:     sqlite.NotificationModeAll,
	})
}

func seedBatchMessage(t *testing.T, messages *sqlite.MessageRepository, conversationID, messageID string, occurredAtMS int64) {
	t.Helper()
	importSourceMessage(t, messages, sqlite.Message{
		MessageID:       messageID,
		ConversationID:  conversationID,
		AccountID:       "batch-account",
		RemoteMessageID: "remote-" + messageID,
		Direction:       sqlite.MessageDirectionIncoming,
		Body:            "body " + messageID,
		State:           sqlite.MessageStateActive,
		OccurredAtMS:    occurredAtMS,
	})
}

func TestBatchGetMessagesByConversationsReturnsNewestLimitAscending(t *testing.T) {
	store, messages, source := openSourceTestStore(t)
	seedSourceAccount(t, store, "batch-account", "google_messages")
	seedBatchConversation(t, store, "conv-1")
	seedBatchConversation(t, store, "conv-2")

	for i := 0; i < 6; i++ {
		seedBatchMessage(t, messages, "conv-1", fmt.Sprintf("conv-1-%d", i), int64(1000+i*100))
		seedBatchMessage(t, messages, "conv-2", fmt.Sprintf("conv-2-%d", i), int64(1050+i*100))
	}

	got, err := source.GetMessagesByConversations([]string{"conv-1", "conv-2"}, 5)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	assertMessageIDs(t, got, "conv-2-3", "conv-1-4", "conv-2-4", "conv-1-5", "conv-2-5")
}

func TestBatchGetMessagesByConversationsRangeBoundsAreInclusive(t *testing.T) {
	store, messages, source := openSourceTestStore(t)
	seedSourceAccount(t, store, "batch-account", "google_messages")
	seedBatchConversation(t, store, "conv-1")
	seedBatchConversation(t, store, "conv-2")

	for i := 0; i < 6; i++ {
		seedBatchMessage(t, messages, "conv-1", fmt.Sprintf("range-1-%d", i), int64(1000+i*100))
		seedBatchMessage(t, messages, "conv-2", fmt.Sprintf("range-2-%d", i), int64(1050+i*100))
	}

	got, err := source.GetMessagesByConversationsRange([]string{"conv-1", "conv-2"}, 1200, 1600, 4)
	if err != nil {
		t.Fatalf("get range: %v", err)
	}
	assertMessageIDs(t, got, "range-1-4", "range-2-4", "range-1-5", "range-2-5")

	// Exact-boundary timestamps stay included on both ends.
	edges, err := source.GetMessagesByConversationsRange([]string{"conv-1"}, 1200, 1400, 10)
	if err != nil {
		t.Fatalf("get edge range: %v", err)
	}
	assertMessageIDs(t, edges, "range-1-2", "range-1-3", "range-1-4")
}

func TestBatchGetMessagesByConversationsUsesMessageIDTieBreaker(t *testing.T) {
	store, messages, source := openSourceTestStore(t)
	seedSourceAccount(t, store, "batch-account", "google_messages")
	seedBatchConversation(t, store, "conv-1")

	for _, id := range []string{"a", "b", "c"} {
		seedBatchMessage(t, messages, "conv-1", id, 1000)
	}

	got, err := source.GetMessagesByConversations([]string{"conv-1"}, 2)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	assertMessageIDs(t, got, "b", "c")

	ranged, err := source.GetMessagesByConversationsRange([]string{"conv-1"}, 900, 1100, 2)
	if err != nil {
		t.Fatalf("get range: %v", err)
	}
	assertMessageIDs(t, ranged, "b", "c")
}

func TestBatchGetMessagesByConversationsWalksBeyondPageSize(t *testing.T) {
	store, messages, source := openSourceTestStore(t)
	seedSourceAccount(t, store, "batch-account", "google_messages")
	seedBatchConversation(t, store, "conv-1")

	total := sourceMessagePageSize + 50
	for i := 0; i < total; i++ {
		seedBatchMessage(t, messages, "conv-1", fmt.Sprintf("m-%04d", i), int64(1000+i))
	}

	limit := sourceMessagePageSize + 10
	got, err := source.GetMessagesByConversationsRange([]string{"conv-1"}, 1, 0, limit)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got) != limit {
		t.Fatalf("count: got %d, want %d", len(got), limit)
	}
	// Newest `limit` of the seeded set, ascending.
	if got[0].MessageID != fmt.Sprintf("m-%04d", total-limit) {
		t.Fatalf("first = %q, want %q", got[0].MessageID, fmt.Sprintf("m-%04d", total-limit))
	}
	if got[len(got)-1].MessageID != fmt.Sprintf("m-%04d", total-1) {
		t.Fatalf("last = %q, want %q", got[len(got)-1].MessageID, fmt.Sprintf("m-%04d", total-1))
	}
}

// Once the merged set is full, later conversations are walked only down to
// the oldest kept row, so the read work is bounded by the result, not by
// (conversations × limit).
func TestBatchGetMessagesByConversationsUsesMergedFloorToBoundPageWalks(t *testing.T) {
	store, messages, source := openSourceTestStore(t)
	seedSourceAccount(t, store, "batch-account", "google_messages")
	seedBatchConversation(t, store, "conv-new")
	seedBatchConversation(t, store, "conv-old")

	total := sourceMessagePageSize + 100
	for i := 0; i < total; i++ {
		seedBatchMessage(t, messages, "conv-new", fmt.Sprintf("new-%04d", i), int64(10_000+i))
		seedBatchMessage(t, messages, "conv-old", fmt.Sprintf("old-%04d", i), int64(1+i))
	}

	limit := sourceMessagePageSize + 50
	before := source.batchPageQueries.Load()
	got, err := source.GetMessagesByConversations([]string{"conv-new", "conv-old"}, limit)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got) != limit {
		t.Fatalf("count: got %d, want %d", len(got), limit)
	}
	if got[0].MessageID != fmt.Sprintf("new-%04d", total-limit) || got[len(got)-1].MessageID != fmt.Sprintf("new-%04d", total-1) {
		t.Fatalf("range = [%s, %s], want the newest %d of conv-new", got[0].MessageID, got[len(got)-1].MessageID, limit)
	}
	// conv-new needs two pages (200 + 50) to fill the result; conv-old's first
	// page is entirely below the floor, so it costs exactly one page.
	if pages := source.batchPageQueries.Load() - before; pages != 3 {
		t.Fatalf("page queries = %d, want 3 (2 to fill from conv-new, 1 floor check on conv-old)", pages)
	}
}

func TestBatchGetMessagesByConversationsDedupesInputIDsAndHandlesEmpty(t *testing.T) {
	store, messages, source := openSourceTestStore(t)
	seedSourceAccount(t, store, "batch-account", "google_messages")
	seedBatchConversation(t, store, "conv-1")
	seedBatchMessage(t, messages, "conv-1", "only", 1000)

	got, err := source.GetMessagesByConversations([]string{"conv-1", "conv-1"}, 10)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	assertMessageIDs(t, got, "only")

	empty, err := source.GetMessagesByConversations(nil, 10)
	if err != nil {
		t.Fatalf("get empty: %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("empty ids returned %d messages", len(empty))
	}

	zero, err := source.GetMessagesByConversations([]string{"conv-1"}, 0)
	if err != nil {
		t.Fatalf("get zero limit: %v", err)
	}
	if len(zero) != 0 {
		t.Fatalf("zero limit returned %d messages", len(zero))
	}
}
