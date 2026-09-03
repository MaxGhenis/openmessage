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
