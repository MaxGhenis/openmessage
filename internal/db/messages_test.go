package db

import (
	"fmt"
	"testing"
)

// newTestStore creates an in-memory Store for testing.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := New(":memory:")
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func TestUpsertMessage_InsertAndUpdate(t *testing.T) {
	store := newTestStore(t)

	t.Run("insert new message", func(t *testing.T) {
		msg := &Message{
			MessageID:      "msg-1",
			ConversationID: "conv-1",
			SenderName:     "Alice",
			SenderNumber:   "+15551234567",
			Body:           "Hello",
			TimestampMS:    1000,
			Status:         "sent",
			IsFromMe:       false,
		}
		if err := store.UpsertMessage(msg); err != nil {
			t.Fatalf("insert message: %v", err)
		}

		got, err := store.GetMessageByID("msg-1")
		if err != nil {
			t.Fatalf("get message: %v", err)
		}
		if got == nil {
			t.Fatal("expected message, got nil")
		}
		if got.Body != "Hello" {
			t.Errorf("body: got %q, want %q", got.Body, "Hello")
		}
		if got.SenderName != "Alice" {
			t.Errorf("sender_name: got %q, want %q", got.SenderName, "Alice")
		}
		if got.Status != "sent" {
			t.Errorf("status: got %q, want %q", got.Status, "sent")
		}
	})

	t.Run("update existing message", func(t *testing.T) {
		msg := &Message{
			MessageID:      "msg-1",
			ConversationID: "conv-1",
			SenderName:     "Alice",
			SenderNumber:   "+15551234567",
			Body:           "Hello (edited)",
			TimestampMS:    1000,
			Status:         "delivered",
			IsFromMe:       false,
		}
		if err := store.UpsertMessage(msg); err != nil {
			t.Fatalf("update message: %v", err)
		}

		got, err := store.GetMessageByID("msg-1")
		if err != nil {
			t.Fatalf("get message: %v", err)
		}
		if got.Body != "Hello (edited)" {
			t.Errorf("body after update: got %q, want %q", got.Body, "Hello (edited)")
		}
		if got.Status != "delivered" {
			t.Errorf("status after update: got %q, want %q", got.Status, "delivered")
		}
	})

	t.Run("upsert preserves all fields", func(t *testing.T) {
		msg := &Message{
			MessageID:      "msg-full",
			ConversationID: "conv-1",
			SenderName:     "Bob",
			SenderNumber:   "+15559876543",
			Body:           "Full message",
			TimestampMS:    2000,
			Status:         "read",
			IsFromMe:       true,
			MentionsMe:     true,
			MediaID:        "media-xyz",
			MimeType:       "image/png",
			DecryptionKey:  "deadbeef",
			Reactions:      `[{"emoji":"thumbsup","count":1}]`,
			ReplyToID:      "msg-1",
		}
		if err := store.UpsertMessage(msg); err != nil {
			t.Fatalf("upsert full message: %v", err)
		}

		got, err := store.GetMessageByID("msg-full")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got.MediaID != "media-xyz" {
			t.Errorf("MediaID: got %q, want %q", got.MediaID, "media-xyz")
		}
		if got.MimeType != "image/png" {
			t.Errorf("MimeType: got %q, want %q", got.MimeType, "image/png")
		}
		if got.DecryptionKey != "deadbeef" {
			t.Errorf("DecryptionKey: got %q, want %q", got.DecryptionKey, "deadbeef")
		}
		if got.Reactions != `[{"emoji":"thumbsup","count":1}]` {
			t.Errorf("Reactions: got %q", got.Reactions)
		}
		if got.ReplyToID != "msg-1" {
			t.Errorf("ReplyToID: got %q, want %q", got.ReplyToID, "msg-1")
		}
		if !got.IsFromMe {
			t.Error("IsFromMe: got false, want true")
		}
		if !got.MentionsMe {
			t.Error("MentionsMe: got false, want true")
		}
	})
}

func TestGetMessages_Filters(t *testing.T) {
	store := newTestStore(t)

	// Seed messages from different senders at different times.
	msgs := []Message{
		{MessageID: "m1", ConversationID: "c1", SenderNumber: "+1111", Body: "A", TimestampMS: 1000},
		{MessageID: "m2", ConversationID: "c1", SenderNumber: "+2222", Body: "B", TimestampMS: 2000},
		{MessageID: "m3", ConversationID: "c1", SenderNumber: "+1111", Body: "C", TimestampMS: 3000},
		{MessageID: "m4", ConversationID: "c2", SenderNumber: "+1111", Body: "D", TimestampMS: 4000},
		{MessageID: "m5", ConversationID: "c2", SenderNumber: "+3333", Body: "E", TimestampMS: 5000},
	}
	for i := range msgs {
		if err := store.UpsertMessage(&msgs[i]); err != nil {
			t.Fatalf("seed message %d: %v", i, err)
		}
	}

	t.Run("filter by phone number", func(t *testing.T) {
		got, err := store.GetMessages("+1111", 0, 0, 100)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if len(got) != 3 {
			t.Fatalf("count: got %d, want 3", len(got))
		}
		// Results should be ordered by timestamp DESC.
		if got[0].MessageID != "m4" {
			t.Errorf("first result: got %q, want m4", got[0].MessageID)
		}
	})

	t.Run("filter by after timestamp", func(t *testing.T) {
		got, err := store.GetMessages("", 3000, 0, 100)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if len(got) != 3 {
			t.Fatalf("count: got %d, want 3 (timestamps 3000, 4000, 5000)", len(got))
		}
	})

	t.Run("filter by before timestamp", func(t *testing.T) {
		got, err := store.GetMessages("", 0, 2000, 100)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("count: got %d, want 2 (timestamps 1000, 2000)", len(got))
		}
	})

	t.Run("filter by after and before timestamp", func(t *testing.T) {
		got, err := store.GetMessages("", 2000, 4000, 100)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if len(got) != 3 {
			t.Fatalf("count: got %d, want 3 (timestamps 2000, 3000, 4000)", len(got))
		}
	})

	t.Run("filter by phone number and time range", func(t *testing.T) {
		got, err := store.GetMessages("+1111", 1500, 3500, 100)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("count: got %d, want 1 (only m3 at 3000)", len(got))
		}
		if got[0].MessageID != "m3" {
			t.Errorf("got %q, want m3", got[0].MessageID)
		}
	})

	t.Run("limit constrains results", func(t *testing.T) {
		got, err := store.GetMessages("", 0, 0, 2)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("count: got %d, want 2", len(got))
		}
		// Should be the two most recent (DESC order).
		if got[0].MessageID != "m5" {
			t.Errorf("first: got %q, want m5", got[0].MessageID)
		}
		if got[1].MessageID != "m4" {
			t.Errorf("second: got %q, want m4", got[1].MessageID)
		}
	})

	t.Run("no filters returns all up to limit", func(t *testing.T) {
		got, err := store.GetMessages("", 0, 0, 100)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if len(got) != 5 {
			t.Fatalf("count: got %d, want 5", len(got))
		}
	})

	t.Run("wrong phone number returns empty", func(t *testing.T) {
		got, err := store.GetMessages("+9999", 0, 0, 100)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("count: got %d, want 0", len(got))
		}
	})
}

func TestGetMessagesByConversation(t *testing.T) {
	store := newTestStore(t)

	// Seed messages across two conversations.
	for i := 0; i < 5; i++ {
		store.UpsertMessage(&Message{
			MessageID:      fmt.Sprintf("c1-m%d", i),
			ConversationID: "conv-1",
			Body:           fmt.Sprintf("msg %d", i),
			TimestampMS:    int64(1000 + i*100),
		})
	}
	for i := 0; i < 3; i++ {
		store.UpsertMessage(&Message{
			MessageID:      fmt.Sprintf("c2-m%d", i),
			ConversationID: "conv-2",
			Body:           fmt.Sprintf("msg %d", i),
			TimestampMS:    int64(2000 + i*100),
		})
	}

	t.Run("returns only messages from specified conversation", func(t *testing.T) {
		got, err := store.GetMessagesByConversation("conv-1", 100)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if len(got) != 5 {
			t.Fatalf("count: got %d, want 5", len(got))
		}
		for _, m := range got {
			if m.ConversationID != "conv-1" {
				t.Errorf("message %s has conversation %q, want conv-1", m.MessageID, m.ConversationID)
			}
		}
	})

	t.Run("ordered by timestamp DESC", func(t *testing.T) {
		got, err := store.GetMessagesByConversation("conv-1", 100)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		for i := 1; i < len(got); i++ {
			if got[i].TimestampMS > got[i-1].TimestampMS {
				t.Errorf("not DESC: msg[%d].ts=%d > msg[%d].ts=%d", i, got[i].TimestampMS, i-1, got[i-1].TimestampMS)
			}
		}
	})

	t.Run("limit constrains results", func(t *testing.T) {
		got, err := store.GetMessagesByConversation("conv-1", 3)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if len(got) != 3 {
			t.Fatalf("count: got %d, want 3", len(got))
		}
	})

	t.Run("nonexistent conversation returns empty", func(t *testing.T) {
		got, err := store.GetMessagesByConversation("nonexistent", 100)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("count: got %d, want 0", len(got))
		}
	})
}

func TestSearchMessages_Comprehensive(t *testing.T) {
	store := newTestStore(t)

	msgs := []Message{
		{MessageID: "s1", SenderNumber: "+1111", Body: "Hello world", TimestampMS: 1000},
		{MessageID: "s2", SenderNumber: "+2222", Body: "hello there", TimestampMS: 2000},
		{MessageID: "s3", SenderNumber: "+1111", Body: "HELLO AGAIN", TimestampMS: 3000},
		{MessageID: "s4", SenderNumber: "+1111", Body: "Goodbye", TimestampMS: 4000},
		{MessageID: "s5", SenderNumber: "+2222", Body: "Nothing relevant", TimestampMS: 5000},
	}
	for i := range msgs {
		if err := store.UpsertMessage(&msgs[i]); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	t.Run("case insensitive search via LIKE", func(t *testing.T) {
		// SQLite LIKE is case-insensitive for ASCII by default.
		got, err := store.SearchMessages("hello", "", 100)
		if err != nil {
			t.Fatalf("search: %v", err)
		}
		if len(got) != 3 {
			t.Errorf("count: got %d, want 3", len(got))
		}
	})

	t.Run("search with phone number filter", func(t *testing.T) {
		got, err := store.SearchMessages("hello", "+1111", 100)
		if err != nil {
			t.Fatalf("search: %v", err)
		}
		if len(got) != 2 {
			t.Errorf("count: got %d, want 2", len(got))
		}
	})

	t.Run("search with limit", func(t *testing.T) {
		got, err := store.SearchMessages("hello", "", 1)
		if err != nil {
			t.Fatalf("search: %v", err)
		}
		if len(got) != 1 {
			t.Errorf("count: got %d, want 1", len(got))
		}
		// Should return the most recent match (DESC order).
		if got[0].MessageID != "s3" {
			t.Errorf("got %q, want s3 (most recent hello)", got[0].MessageID)
		}
	})

	t.Run("no results for nonexistent query", func(t *testing.T) {
		got, err := store.SearchMessages("zzzzz", "", 100)
		if err != nil {
			t.Fatalf("search: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("count: got %d, want 0", len(got))
		}
	})

	t.Run("special characters in search query", func(t *testing.T) {
		// Insert a message with special characters.
		store.UpsertMessage(&Message{
			MessageID:   "s-special",
			Body:        "Price is $100.00 (50% off!)",
			TimestampMS: 6000,
		})

		got, err := store.SearchMessages("$100", "", 100)
		if err != nil {
			t.Fatalf("search: %v", err)
		}
		if len(got) != 1 {
			t.Errorf("count: got %d, want 1", len(got))
		}

		got, err = store.SearchMessages("50%", "", 100)
		if err != nil {
			t.Fatalf("search: %v", err)
		}
		if len(got) != 1 {
			t.Errorf("count: got %d, want 1", len(got))
		}
	})

	t.Run("search with emoji in body", func(t *testing.T) {
		store.UpsertMessage(&Message{
			MessageID:   "s-emoji",
			Body:        "Great job! 🎉",
			TimestampMS: 7000,
		})
		got, err := store.SearchMessages("🎉", "", 100)
		if err != nil {
			t.Fatalf("search: %v", err)
		}
		if len(got) != 1 {
			t.Errorf("count: got %d, want 1", len(got))
		}
	})

	t.Run("partial word match", func(t *testing.T) {
		got, err := store.SearchMessages("ell", "", 100)
		if err != nil {
			t.Fatalf("search: %v", err)
		}
		// Should match "Hello world", "hello there", "HELLO AGAIN".
		if len(got) != 3 {
			t.Errorf("count: got %d, want 3", len(got))
		}
	})
}

func TestGetMessageByID_EdgeCases(t *testing.T) {
	store := newTestStore(t)

	store.UpsertMessage(&Message{
		MessageID:      "msg-exists",
		ConversationID: "c1",
		Body:           "I exist",
		TimestampMS:    1000,
	})

	t.Run("existing message returns correctly", func(t *testing.T) {
		got, err := store.GetMessageByID("msg-exists")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got == nil {
			t.Fatal("expected message, got nil")
		}
		if got.Body != "I exist" {
			t.Errorf("body: got %q, want %q", got.Body, "I exist")
		}
	})

	t.Run("nonexistent message returns nil without error", func(t *testing.T) {
		got, err := store.GetMessageByID("does-not-exist")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got != nil {
			t.Errorf("expected nil, got %+v", got)
		}
	})

	t.Run("empty ID returns nil without error", func(t *testing.T) {
		got, err := store.GetMessageByID("")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got != nil {
			t.Errorf("expected nil, got %+v", got)
		}
	})
}

func TestDeleteTmpMessages_Comprehensive(t *testing.T) {
	store := newTestStore(t)

	t.Run("deletes only tmp_ messages in specified conversation", func(t *testing.T) {
		store.UpsertMessage(&Message{MessageID: "tmp_aaa", ConversationID: "c1", TimestampMS: 1000})
		store.UpsertMessage(&Message{MessageID: "tmp_bbb", ConversationID: "c1", TimestampMS: 1001})
		store.UpsertMessage(&Message{MessageID: "real-1", ConversationID: "c1", TimestampMS: 900})
		store.UpsertMessage(&Message{MessageID: "tmp_ccc", ConversationID: "c2", TimestampMS: 1000})

		n, err := store.DeleteTmpMessages("c1")
		if err != nil {
			t.Fatalf("delete: %v", err)
		}
		if n != 2 {
			t.Errorf("deleted count: got %d, want 2", n)
		}

		// c1 should have only the real message.
		c1Msgs, _ := store.GetMessagesByConversation("c1", 100)
		if len(c1Msgs) != 1 {
			t.Fatalf("c1 messages: got %d, want 1", len(c1Msgs))
		}
		if c1Msgs[0].MessageID != "real-1" {
			t.Errorf("remaining message: got %q, want real-1", c1Msgs[0].MessageID)
		}

		// c2 should be untouched.
		c2Msgs, _ := store.GetMessagesByConversation("c2", 100)
		if len(c2Msgs) != 1 {
			t.Fatalf("c2 messages: got %d, want 1", len(c2Msgs))
		}
	})

	t.Run("no tmp_ messages returns zero", func(t *testing.T) {
		store2 := newTestStore(t)
		store2.UpsertMessage(&Message{MessageID: "real-only", ConversationID: "c1", TimestampMS: 1000})

		n, err := store2.DeleteTmpMessages("c1")
		if err != nil {
			t.Fatalf("delete: %v", err)
		}
		if n != 0 {
			t.Errorf("deleted count: got %d, want 0", n)
		}
	})

	t.Run("nonexistent conversation returns zero", func(t *testing.T) {
		store3 := newTestStore(t)
		n, err := store3.DeleteTmpMessages("no-such-conv")
		if err != nil {
			t.Fatalf("delete: %v", err)
		}
		if n != 0 {
			t.Errorf("deleted count: got %d, want 0", n)
		}
	})
}

func TestDeleteMessageByID_RemovesSearchIndexEntry(t *testing.T) {
	store := newTestStore(t)
	if !store.ftsEnabled {
		t.Skip("fts index not enabled in this sqlite build")
	}

	msg := &Message{
		MessageID:      "msg-delete-me",
		ConversationID: "c1",
		Body:           "Price is $100.00 (50% off!)",
		TimestampMS:    1000,
	}
	if err := store.UpsertMessage(msg); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	got, err := store.SearchMessages("$100", "", 10)
	if err != nil {
		t.Fatalf("search before delete: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 result before delete, got %d", len(got))
	}

	if err := store.DeleteMessageByID(msg.MessageID); err != nil {
		t.Fatalf("delete by id: %v", err)
	}

	got, err = store.SearchMessages("$100", "", 10)
	if err != nil {
		t.Fatalf("search after delete: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected 0 results after delete, got %d", len(got))
	}
}

func TestGetMessagesByConversationPaging(t *testing.T) {
	store := newTestStore(t)

	for i, ts := range []int64{100, 200, 300, 400, 500} {
		if err := store.UpsertMessage(&Message{
			MessageID:      fmt.Sprintf("m%d", i+1),
			ConversationID: "c1",
			Body:           fmt.Sprintf("msg %d", i+1),
			TimestampMS:    ts,
		}); err != nil {
			t.Fatalf("seed message %d: %v", i+1, err)
		}
	}

	before, err := store.GetMessagesByConversationBefore("c1", 400, "m4", 2)
	if err != nil {
		t.Fatalf("before query: %v", err)
	}
	if len(before) != 2 {
		t.Fatalf("before count: got %d, want 2", len(before))
	}
	if before[0].TimestampMS != 300 || before[1].TimestampMS != 200 {
		t.Fatalf("before ordering: got [%d %d], want [300 200]", before[0].TimestampMS, before[1].TimestampMS)
	}

	after, err := store.GetMessagesByConversationAfter("c1", 200, "m2", 2)
	if err != nil {
		t.Fatalf("after query: %v", err)
	}
	if len(after) != 2 {
		t.Fatalf("after count: got %d, want 2", len(after))
	}
	if after[0].TimestampMS != 300 || after[1].TimestampMS != 400 {
		t.Fatalf("after ordering: got [%d %d], want [300 400]", after[0].TimestampMS, after[1].TimestampMS)
	}
}

func TestGetMessagesByConversationPagingWithDuplicateTimestamps(t *testing.T) {
	store := newTestStore(t)

	for _, msg := range []*Message{
		{MessageID: "m1", ConversationID: "c1", Body: "msg 1", TimestampMS: 100},
		{MessageID: "m2", ConversationID: "c1", Body: "msg 2", TimestampMS: 200},
		{MessageID: "m3", ConversationID: "c1", Body: "msg 3", TimestampMS: 200},
		{MessageID: "m4", ConversationID: "c1", Body: "msg 4", TimestampMS: 300},
	} {
		if err := store.UpsertMessage(msg); err != nil {
			t.Fatalf("seed message %s: %v", msg.MessageID, err)
		}
	}

	before, err := store.GetMessagesByConversationBefore("c1", 200, "m3", 10)
	if err != nil {
		t.Fatalf("before query: %v", err)
	}
	if len(before) != 2 {
		t.Fatalf("before count: got %d, want 2", len(before))
	}
	if before[0].MessageID != "m2" || before[1].MessageID != "m1" {
		t.Fatalf("before boundary = [%s %s], want [m2 m1]", before[0].MessageID, before[1].MessageID)
	}

	after, err := store.GetMessagesByConversationAfter("c1", 200, "m2", 10)
	if err != nil {
		t.Fatalf("after query: %v", err)
	}
	if len(after) != 2 {
		t.Fatalf("after count: got %d, want 2", len(after))
	}
	if after[0].MessageID != "m3" || after[1].MessageID != "m4" {
		t.Fatalf("after boundary = [%s %s], want [m3 m4]", after[0].MessageID, after[1].MessageID)
	}
}

func TestRecordOutgoingMessageDeletesDraftAtomically(t *testing.T) {
	store := newTestStore(t)

	if err := store.UpsertConversation(&Conversation{
		ConversationID: "c1",
		Name:           "Alice",
		LastMessageTS:  100,
	}); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}
	if err := store.UpsertDraft(&Draft{
		DraftID:        "d1",
		ConversationID: "c1",
		Body:           "Draft body",
		CreatedAt:      200,
	}); err != nil {
		t.Fatalf("seed draft: %v", err)
	}

	msg := &Message{
		MessageID:      "m-outgoing",
		ConversationID: "c1",
		Body:           "Sent body",
		IsFromMe:       true,
		TimestampMS:    999,
		Status:         "OUTGOING_SENDING",
	}
	if err := store.RecordOutgoingMessage(msg, "d1"); err != nil {
		t.Fatalf("record outgoing message: %v", err)
	}

	got, err := store.GetMessageByID("m-outgoing")
	if err != nil {
		t.Fatalf("get outgoing message: %v", err)
	}
	if got == nil || got.Body != "Sent body" {
		t.Fatalf("stored outgoing message = %#v, want body %q", got, "Sent body")
	}

	draft, err := store.GetDraft("d1")
	if err != nil {
		t.Fatalf("get draft: %v", err)
	}
	if draft != nil {
		t.Fatalf("draft still exists after outgoing record: %#v", draft)
	}

	conv, err := store.GetConversation("c1")
	if err != nil {
		t.Fatalf("get conversation: %v", err)
	}
	if conv.LastMessageTS != 999 {
		t.Fatalf("conversation timestamp = %d, want 999", conv.LastMessageTS)
	}
}

func TestSearchMessages_FTSFallbackStillFindsSpecialQueries(t *testing.T) {
	store := newTestStore(t)
	if !store.ftsEnabled {
		t.Skip("fts index not enabled in this sqlite build")
	}

	if err := store.UpsertMessage(&Message{
		MessageID:      "special-1",
		ConversationID: "c1",
		Body:           "Price is $100.00 (50% off!)",
		TimestampMS:    1000,
	}); err != nil {
		t.Fatalf("upsert special chars: %v", err)
	}
	if err := store.UpsertMessage(&Message{
		MessageID:      "special-2",
		ConversationID: "c1",
		Body:           "Great job! 🎉",
		TimestampMS:    1001,
	}); err != nil {
		t.Fatalf("upsert emoji: %v", err)
	}

	for _, q := range []string{"$100", "50%", "🎉"} {
		got, err := store.SearchMessages(q, "", 10)
		if err != nil {
			t.Fatalf("search %q: %v", q, err)
		}
		if len(got) != 1 {
			t.Fatalf("search %q returned %d results, want 1", q, len(got))
		}
	}
}

func TestGetMessages_OrderingIsDescending(t *testing.T) {
	store := newTestStore(t)

	for i := 0; i < 10; i++ {
		store.UpsertMessage(&Message{
			MessageID:   fmt.Sprintf("m%d", i),
			TimestampMS: int64(i * 1000),
		})
	}

	got, err := store.GetMessages("", 0, 0, 100)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	for i := 1; i < len(got); i++ {
		if got[i].TimestampMS > got[i-1].TimestampMS {
			t.Errorf("not DESC at index %d: %d > %d", i, got[i].TimestampMS, got[i-1].TimestampMS)
		}
	}
}

func TestUpsertMessage_EmptyBody(t *testing.T) {
	store := newTestStore(t)

	// Media-only messages can have empty bodies.
	msg := &Message{
		MessageID:   "media-only",
		MediaID:     "mid-1",
		MimeType:    "video/mp4",
		Body:        "",
		TimestampMS: 1000,
	}
	if err := store.UpsertMessage(msg); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	got, err := store.GetMessageByID("media-only")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Body != "" {
		t.Errorf("body: got %q, want empty", got.Body)
	}
	if got.MediaID != "mid-1" {
		t.Errorf("MediaID: got %q, want mid-1", got.MediaID)
	}
}
