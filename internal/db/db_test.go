package db

import (
	"testing"
	"time"
)

func TestNewDB(t *testing.T) {
	store, err := New(":memory:")
	if err != nil {
		t.Fatalf("failed to create db: %v", err)
	}
	defer store.Close()
}

func TestConversationCRUD(t *testing.T) {
	store, err := New(":memory:")
	if err != nil {
		t.Fatalf("failed to create db: %v", err)
	}
	defer store.Close()

	conv := &Conversation{
		ConversationID: "conv-1",
		Name:           "Alice",
		IsGroup:        false,
		Participants:   `[{"name":"Alice","number":"+15551234567"}]`,
		LastMessageTS:  time.Now().UnixMilli(),
		UnreadCount:    2,
	}

	// Upsert
	err = store.UpsertConversation(conv)
	if err != nil {
		t.Fatalf("upsert conversation: %v", err)
	}

	// Get
	got, err := store.GetConversation("conv-1")
	if err != nil {
		t.Fatalf("get conversation: %v", err)
	}
	if got.Name != "Alice" {
		t.Errorf("expected name Alice, got %s", got.Name)
	}
	if got.UnreadCount != 2 {
		t.Errorf("expected unread 2, got %d", got.UnreadCount)
	}

	// Update
	conv.Name = "Alice Smith"
	conv.UnreadCount = 0
	err = store.UpsertConversation(conv)
	if err != nil {
		t.Fatalf("upsert update: %v", err)
	}
	got, err = store.GetConversation("conv-1")
	if err != nil {
		t.Fatalf("get after update: %v", err)
	}
	if got.Name != "Alice Smith" {
		t.Errorf("expected name Alice Smith, got %s", got.Name)
	}

	// List
	convs, err := store.ListConversations(10)
	if err != nil {
		t.Fatalf("list conversations: %v", err)
	}
	if len(convs) != 1 {
		t.Errorf("expected 1 conversation, got %d", len(convs))
	}
}

func TestListConversationsOrdering(t *testing.T) {
	store, err := New(":memory:")
	if err != nil {
		t.Fatalf("failed to create db: %v", err)
	}
	defer store.Close()

	now := time.Now().UnixMilli()
	for i, name := range []string{"Old", "New", "Middle"} {
		ts := now + int64((i-1)*1000) // Old=-1s, New=0s, Middle=+1s
		err := store.UpsertConversation(&Conversation{
			ConversationID: name,
			Name:           name,
			LastMessageTS:  ts,
		})
		if err != nil {
			t.Fatalf("upsert: %v", err)
		}
	}

	convs, err := store.ListConversations(10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(convs) != 3 {
		t.Fatalf("expected 3, got %d", len(convs))
	}
	// Should be ordered by last_message_ts DESC
	if convs[0].Name != "Middle" {
		t.Errorf("expected first=Middle, got %s", convs[0].Name)
	}
	if convs[2].Name != "Old" {
		t.Errorf("expected last=Old, got %s", convs[2].Name)
	}
}

func TestMessageCRUD(t *testing.T) {
	store, err := New(":memory:")
	if err != nil {
		t.Fatalf("failed to create db: %v", err)
	}
	defer store.Close()

	now := time.Now().UnixMilli()

	msg := &Message{
		MessageID:      "msg-1",
		ConversationID: "conv-1",
		SenderName:     "Alice",
		SenderNumber:   "+15551234567",
		Body:           "Hello world",
		TimestampMS:    now,
		Status:         "delivered",
		IsFromMe:       false,
	}

	err = store.UpsertMessage(msg)
	if err != nil {
		t.Fatalf("upsert message: %v", err)
	}

	// Get by conversation
	msgs, err := store.GetMessagesByConversation("conv-1", 10)
	if err != nil {
		t.Fatalf("get by conversation: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if msgs[0].Body != "Hello world" {
		t.Errorf("expected body 'Hello world', got %s", msgs[0].Body)
	}

	// Get recent with filters
	msgs, err = store.GetMessages("+15551234567", now-1000, now+1000, 10)
	if err != nil {
		t.Fatalf("get messages filtered: %v", err)
	}
	if len(msgs) != 1 {
		t.Errorf("expected 1, got %d", len(msgs))
	}

	// Filter by wrong number
	msgs, err = store.GetMessages("+15559999999", 0, 0, 10)
	if err != nil {
		t.Fatalf("get messages wrong number: %v", err)
	}
	if len(msgs) != 0 {
		t.Errorf("expected 0, got %d", len(msgs))
	}
}

func TestSearchMessages(t *testing.T) {
	store, err := New(":memory:")
	if err != nil {
		t.Fatalf("failed to create db: %v", err)
	}
	defer store.Close()

	now := time.Now().UnixMilli()
	messages := []Message{
		{MessageID: "1", ConversationID: "c1", Body: "Hello world", TimestampMS: now},
		{MessageID: "2", ConversationID: "c1", Body: "Goodbye world", TimestampMS: now + 1},
		{MessageID: "3", ConversationID: "c2", Body: "Hello there", TimestampMS: now + 2},
		{MessageID: "4", ConversationID: "c2", Body: "Something else", TimestampMS: now + 3},
	}
	for i := range messages {
		if err := store.UpsertMessage(&messages[i]); err != nil {
			t.Fatalf("upsert: %v", err)
		}
	}

	// Search for "hello"
	results, err := store.SearchMessages("hello", "", 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) != 2 {
		t.Errorf("expected 2 results for 'hello', got %d", len(results))
	}

	// Search for "goodbye"
	results, err = store.SearchMessages("goodbye", "", 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) != 1 {
		t.Errorf("expected 1 result for 'goodbye', got %d", len(results))
	}

	// Search with no results
	results, err = store.SearchMessages("nonexistent", "", 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected 0, got %d", len(results))
	}
}

func TestContactCRUD(t *testing.T) {
	store, err := New(":memory:")
	if err != nil {
		t.Fatalf("failed to create db: %v", err)
	}
	defer store.Close()

	contact := &Contact{
		ContactID: "contact-1",
		Name:      "Alice",
		Number:    "+15551234567",
	}

	err = store.UpsertContact(contact)
	if err != nil {
		t.Fatalf("upsert contact: %v", err)
	}

	// List all
	contacts, err := store.ListContacts("", 10)
	if err != nil {
		t.Fatalf("list contacts: %v", err)
	}
	if len(contacts) != 1 {
		t.Fatalf("expected 1 contact, got %d", len(contacts))
	}
	if contacts[0].Name != "Alice" {
		t.Errorf("expected name Alice, got %s", contacts[0].Name)
	}

	// Search by name
	contacts, err = store.ListContacts("ali", 10)
	if err != nil {
		t.Fatalf("search contacts: %v", err)
	}
	if len(contacts) != 1 {
		t.Errorf("expected 1, got %d", len(contacts))
	}

	// Search by number
	contacts, err = store.ListContacts("555123", 10)
	if err != nil {
		t.Fatalf("search by number: %v", err)
	}
	if len(contacts) != 1 {
		t.Errorf("expected 1, got %d", len(contacts))
	}

	// No match
	contacts, err = store.ListContacts("bob", 10)
	if err != nil {
		t.Fatalf("search no match: %v", err)
	}
	if len(contacts) != 0 {
		t.Errorf("expected 0, got %d", len(contacts))
	}
}

func TestMessageMediaFields(t *testing.T) {
	store, err := New(":memory:")
	if err != nil {
		t.Fatalf("failed to create db: %v", err)
	}
	defer store.Close()

	msg := &Message{
		MessageID:      "msg-img",
		ConversationID: "c1",
		Body:           "",
		TimestampMS:    1000,
		MediaID:        "media-123",
		MimeType:       "image/jpeg",
		DecryptionKey:  "aabbccdd",
	}
	if err := store.UpsertMessage(msg); err != nil {
		t.Fatalf("upsert message with media: %v", err)
	}

	msgs, err := store.GetMessagesByConversation("c1", 10)
	if err != nil {
		t.Fatalf("get messages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1, got %d", len(msgs))
	}
	if msgs[0].MediaID != "media-123" {
		t.Errorf("expected MediaID 'media-123', got %q", msgs[0].MediaID)
	}
	if msgs[0].MimeType != "image/jpeg" {
		t.Errorf("expected MimeType 'image/jpeg', got %q", msgs[0].MimeType)
	}
	if msgs[0].DecryptionKey != "aabbccdd" {
		t.Errorf("expected DecryptionKey 'aabbccdd', got %q", msgs[0].DecryptionKey)
	}
}

func TestGetMessageByID(t *testing.T) {
	store, err := New(":memory:")
	if err != nil {
		t.Fatalf("failed to create db: %v", err)
	}
	defer store.Close()

	msg := &Message{
		MessageID:      "msg-1",
		ConversationID: "c1",
		Body:           "Hello",
		MediaID:        "media-abc",
		MimeType:       "image/png",
		DecryptionKey:  "deadbeef",
		TimestampMS:    1000,
	}
	store.UpsertMessage(msg)

	got, err := store.GetMessageByID("msg-1")
	if err != nil {
		t.Fatalf("get message by id: %v", err)
	}
	if got == nil {
		t.Fatal("expected message, got nil")
	}
	if got.MediaID != "media-abc" {
		t.Errorf("expected MediaID 'media-abc', got %q", got.MediaID)
	}
	if got.DecryptionKey != "deadbeef" {
		t.Errorf("expected DecryptionKey, got %q", got.DecryptionKey)
	}

	// Not found
	got, err = store.GetMessageByID("nonexistent")
	if err != nil {
		t.Fatalf("get nonexistent: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for nonexistent, got %+v", got)
	}
}

func TestMessageReactionsField(t *testing.T) {
	store, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	msg := &Message{
		MessageID:      "m1",
		ConversationID: "c1",
		Body:           "Funny message",
		TimestampMS:    1000,
		Reactions:      `[{"emoji":"😂","count":2},{"emoji":"❤️","count":1}]`,
	}
	if err := store.UpsertMessage(msg); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	msgs, err := store.GetMessagesByConversation("c1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("got %d, want 1", len(msgs))
	}
	if msgs[0].Reactions != `[{"emoji":"😂","count":2},{"emoji":"❤️","count":1}]` {
		t.Errorf("reactions mismatch: %q", msgs[0].Reactions)
	}
}

func TestMessageReplyToField(t *testing.T) {
	store, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// Original message
	store.UpsertMessage(&Message{
		MessageID:      "m1",
		ConversationID: "c1",
		Body:           "Original message",
		TimestampMS:    1000,
	})
	// Reply
	store.UpsertMessage(&Message{
		MessageID:      "m2",
		ConversationID: "c1",
		Body:           "This is a reply",
		TimestampMS:    2000,
		ReplyToID:      "m1",
	})

	msgs, err := store.GetMessagesByConversation("c1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("got %d, want 2", len(msgs))
	}
	// Messages come DESC, so m2 first
	if msgs[0].ReplyToID != "m1" {
		t.Errorf("expected ReplyToID 'm1', got %q", msgs[0].ReplyToID)
	}
	if msgs[1].ReplyToID != "" {
		t.Errorf("expected empty ReplyToID, got %q", msgs[1].ReplyToID)
	}
}

func TestMediaFieldMigration(t *testing.T) {
	// Verify that a fresh DB has media columns
	store, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// Insert a message with media fields - should not error
	err = store.UpsertMessage(&Message{
		MessageID:     "m1",
		MediaID:       "mid",
		MimeType:      "image/gif",
		DecryptionKey: "key",
	})
	if err != nil {
		t.Fatalf("expected media columns to exist: %v", err)
	}
}

func TestDeleteTmpMessages(t *testing.T) {
	store, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// Simulate: user sends a message, we store it with tmp_ ID
	store.UpsertMessage(&Message{
		MessageID:      "tmp_001234567890",
		ConversationID: "c1",
		Body:           "Hello from send",
		IsFromMe:       true,
		TimestampMS:    1000,
	})
	// Also store a regular message that should NOT be deleted
	store.UpsertMessage(&Message{
		MessageID:      "real-msg-1",
		ConversationID: "c1",
		Body:           "Normal message",
		TimestampMS:    900,
	})
	// And a tmp_ in a different conversation
	store.UpsertMessage(&Message{
		MessageID:      "tmp_999999999999",
		ConversationID: "c2",
		Body:           "Other convo",
		IsFromMe:       true,
		TimestampMS:    1000,
	})

	// Delete tmp_ messages for c1 only
	n, err := store.DeleteTmpMessages("c1")
	if err != nil {
		t.Fatalf("delete tmp: %v", err)
	}
	if n != 1 {
		t.Errorf("expected 1 deleted, got %d", n)
	}

	// c1 should have only the real message
	msgs, err := store.GetMessagesByConversation("c1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message in c1, got %d", len(msgs))
	}
	if msgs[0].MessageID != "real-msg-1" {
		t.Errorf("expected real-msg-1, got %s", msgs[0].MessageID)
	}

	// c2 should still have its tmp_ message
	msgs2, err := store.GetMessagesByConversation("c2", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs2) != 1 {
		t.Fatalf("expected 1 message in c2, got %d", len(msgs2))
	}
}

func TestSeedDemo(t *testing.T) {
	store, err := New(":memory:")
	if err != nil {
		t.Fatalf("failed to create db: %v", err)
	}
	defer store.Close()

	if err := store.SeedDemo(); err != nil {
		t.Fatalf("SeedDemo: %v", err)
	}

	// Should have 16 conversations (9 SMS + 4 WhatsApp + 3 Signal)
	convs, err := store.ListConversations(100)
	if err != nil {
		t.Fatalf("list conversations: %v", err)
	}
	if len(convs) != 16 {
		t.Errorf("expected 16 conversations, got %d", len(convs))
	}

	// Should have 12 contacts (10 with phone numbers + 2 Signal ACIs)
	contacts, err := store.ListContacts("", 100)
	if err != nil {
		t.Fatalf("list contacts: %v", err)
	}
	if len(contacts) != 12 {
		t.Errorf("expected 12 contacts, got %d", len(contacts))
	}

	// Demo data should cover all three live platforms so screenshots and
	// demos actually show the multi-platform story. If someone removes
	// all of a platform's conversations, this test will loudly remind them.
	platforms := map[string]int{}
	for _, c := range convs {
		platforms[c.SourcePlatform]++
	}
	for _, want := range []string{"sms", "whatsapp", "signal"} {
		if platforms[want] == 0 {
			t.Errorf("demo data is missing %s conversations; got %v", want, platforms)
		}
	}

	// Check a specific conversation has messages
	msgs, err := store.GetMessagesByConversation("conv1", 100)
	if err != nil {
		t.Fatalf("get messages: %v", err)
	}
	if len(msgs) != 6 {
		t.Errorf("expected 6 messages in conv1, got %d", len(msgs))
	}

	// Idempotent — running again should not error
	if err := store.SeedDemo(); err != nil {
		t.Fatalf("SeedDemo idempotent: %v", err)
	}
}

func TestGetMessagesNoFilters(t *testing.T) {
	store, err := New(":memory:")
	if err != nil {
		t.Fatalf("failed to create db: %v", err)
	}
	defer store.Close()

	now := time.Now().UnixMilli()
	for i := 0; i < 5; i++ {
		err := store.UpsertMessage(&Message{
			MessageID:      "msg-" + string(rune('a'+i)),
			ConversationID: "c1",
			Body:           "Message",
			TimestampMS:    now + int64(i*1000),
		})
		if err != nil {
			t.Fatalf("upsert: %v", err)
		}
	}

	// No filters, limit 3
	msgs, err := store.GetMessages("", 0, 0, 3)
	if err != nil {
		t.Fatalf("get messages: %v", err)
	}
	if len(msgs) != 3 {
		t.Errorf("expected 3, got %d", len(msgs))
	}
}
