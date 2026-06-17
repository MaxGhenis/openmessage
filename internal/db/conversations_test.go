package db

import (
	"database/sql"
	"fmt"
	"testing"
)

func TestUpsertConversation_InsertAndUpdate(t *testing.T) {
	store := newTestStore(t)

	t.Run("insert new conversation", func(t *testing.T) {
		conv := &Conversation{
			ConversationID: "conv-1",
			Name:           "Alice",
			IsGroup:        false,
			Participants:   `[{"name":"Alice","number":"+15551234567"}]`,
			LastMessageTS:  1000,
			UnreadCount:    3,
		}
		if err := store.UpsertConversation(conv); err != nil {
			t.Fatalf("upsert: %v", err)
		}

		got, err := store.GetConversation("conv-1")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got.Name != "Alice" {
			t.Errorf("name: got %q, want %q", got.Name, "Alice")
		}
		if got.IsGroup {
			t.Error("is_group: got true, want false")
		}
		if got.Participants != `[{"name":"Alice","number":"+15551234567"}]` {
			t.Errorf("participants: got %q", got.Participants)
		}
		if got.LastMessageTS != 1000 {
			t.Errorf("last_message_ts: got %d, want 1000", got.LastMessageTS)
		}
		if got.UnreadCount != 3 {
			t.Errorf("unread_count: got %d, want 3", got.UnreadCount)
		}
	})

	t.Run("update existing conversation", func(t *testing.T) {
		conv := &Conversation{
			ConversationID: "conv-1",
			Name:           "Alice Smith",
			IsGroup:        false,
			Participants:   `[{"name":"Alice Smith","number":"+15551234567"}]`,
			LastMessageTS:  2000,
			UnreadCount:    0,
		}
		if err := store.UpsertConversation(conv); err != nil {
			t.Fatalf("upsert update: %v", err)
		}

		got, err := store.GetConversation("conv-1")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got.Name != "Alice Smith" {
			t.Errorf("name after update: got %q, want %q", got.Name, "Alice Smith")
		}
		if got.LastMessageTS != 2000 {
			t.Errorf("last_message_ts after update: got %d, want 2000", got.LastMessageTS)
		}
		if got.UnreadCount != 0 {
			t.Errorf("unread_count after update: got %d, want 0", got.UnreadCount)
		}
	})

	t.Run("upsert group conversation", func(t *testing.T) {
		conv := &Conversation{
			ConversationID: "group-1",
			Name:           "Family Chat",
			IsGroup:        true,
			Participants:   `[{"name":"Alice"},{"name":"Bob"},{"name":"Charlie"}]`,
			LastMessageTS:  3000,
			UnreadCount:    5,
		}
		if err := store.UpsertConversation(conv); err != nil {
			t.Fatalf("upsert group: %v", err)
		}

		got, err := store.GetConversation("group-1")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if !got.IsGroup {
			t.Error("is_group: got false, want true")
		}
		if got.Name != "Family Chat" {
			t.Errorf("name: got %q, want %q", got.Name, "Family Chat")
		}
	})
}

func TestConversationNotificationModeLifecycle(t *testing.T) {
	store := newTestStore(t)

	if err := store.UpsertConversation(&Conversation{
		ConversationID:   "conv-muted",
		Name:             "Muted",
		LastMessageTS:    1000,
		NotificationMode: NotificationModeMentions,
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	got, err := store.GetConversation("conv-muted")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.NotificationMode != NotificationModeMentions {
		t.Fatalf("notification_mode = %q, want %q", got.NotificationMode, NotificationModeMentions)
	}

	if err := store.UpsertConversation(&Conversation{
		ConversationID: "conv-muted",
		Name:           "Muted renamed",
		LastMessageTS:  2000,
	}); err != nil {
		t.Fatalf("upsert update: %v", err)
	}

	got, err = store.GetConversation("conv-muted")
	if err != nil {
		t.Fatalf("get after update: %v", err)
	}
	if got.NotificationMode != NotificationModeMentions {
		t.Fatalf("notification_mode after update = %q, want %q", got.NotificationMode, NotificationModeMentions)
	}

	if err := store.SetConversationNotificationMode("conv-muted", NotificationModeMuted); err != nil {
		t.Fatalf("SetConversationNotificationMode(): %v", err)
	}
	got, err = store.GetConversation("conv-muted")
	if err != nil {
		t.Fatalf("get after set: %v", err)
	}
	if got.NotificationMode != NotificationModeMuted {
		t.Fatalf("notification_mode after set = %q, want %q", got.NotificationMode, NotificationModeMuted)
	}

	if err := store.SetConversationNotificationMode("conv-muted", "loud"); err == nil {
		t.Fatal("expected invalid notification mode error")
	}
}

func TestConversationFavoriteLifecycle(t *testing.T) {
	store := newTestStore(t)

	if err := store.UpsertConversation(&Conversation{
		ConversationID: "conv-favorite",
		Name:           "Favorite",
		LastMessageTS:  1000,
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	got, err := store.GetConversation("conv-favorite")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.IsFavorite {
		t.Fatal("new conversation should not be favorite by default")
	}

	if err := store.SetConversationFavorite("conv-favorite", true); err != nil {
		t.Fatalf("SetConversationFavorite(true): %v", err)
	}
	got, err = store.GetConversation("conv-favorite")
	if err != nil {
		t.Fatalf("get after favorite: %v", err)
	}
	if !got.IsFavorite {
		t.Fatal("conversation should be favorite after set")
	}

	if err := store.UpsertConversation(&Conversation{
		ConversationID: "conv-favorite",
		Name:           "Favorite renamed",
		LastMessageTS:  2000,
	}); err != nil {
		t.Fatalf("upsert update: %v", err)
	}
	got, err = store.GetConversation("conv-favorite")
	if err != nil {
		t.Fatalf("get after update: %v", err)
	}
	if !got.IsFavorite {
		t.Fatal("sync-style upsert should preserve favorite state")
	}

	if err := store.SetConversationFavorite("conv-favorite", false); err != nil {
		t.Fatalf("SetConversationFavorite(false): %v", err)
	}
	got, err = store.GetConversation("conv-favorite")
	if err != nil {
		t.Fatalf("get after unfavorite: %v", err)
	}
	if got.IsFavorite {
		t.Fatal("conversation should not be favorite after clearing")
	}

	if err := store.SetConversationFavorite("missing-favorite", true); err != sql.ErrNoRows {
		t.Fatalf("SetConversationFavorite missing = %v, want sql.ErrNoRows", err)
	}
}

func TestGetConversation_NotFound(t *testing.T) {
	store := newTestStore(t)

	_, err := store.GetConversation("nonexistent")
	if err == nil {
		t.Error("expected error for nonexistent conversation, got nil")
	}
}

func TestListConversations_Ordering(t *testing.T) {
	store := newTestStore(t)

	// Insert conversations with varying timestamps (out of order).
	conversations := []Conversation{
		{ConversationID: "c-old", Name: "Old", LastMessageTS: 1000},
		{ConversationID: "c-new", Name: "New", LastMessageTS: 5000},
		{ConversationID: "c-mid", Name: "Middle", LastMessageTS: 3000},
		{ConversationID: "c-newest", Name: "Newest", LastMessageTS: 8000},
		{ConversationID: "c-ancient", Name: "Ancient", LastMessageTS: 100},
	}
	for i := range conversations {
		if err := store.UpsertConversation(&conversations[i]); err != nil {
			t.Fatalf("upsert: %v", err)
		}
	}

	t.Run("returns all ordered by last_message_ts DESC", func(t *testing.T) {
		got, err := store.ListConversations(100)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(got) != 5 {
			t.Fatalf("count: got %d, want 5", len(got))
		}
		expectedOrder := []string{"Newest", "New", "Middle", "Old", "Ancient"}
		for i, name := range expectedOrder {
			if got[i].Name != name {
				t.Errorf("position %d: got %q, want %q", i, got[i].Name, name)
			}
		}
	})

	t.Run("limit constrains results", func(t *testing.T) {
		got, err := store.ListConversations(2)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("count: got %d, want 2", len(got))
		}
		if got[0].Name != "Newest" {
			t.Errorf("first: got %q, want Newest", got[0].Name)
		}
		if got[1].Name != "New" {
			t.Errorf("second: got %q, want New", got[1].Name)
		}
	})

	t.Run("favorite outside limit is still returned", func(t *testing.T) {
		if err := store.SetConversationFavorite("c-ancient", true); err != nil {
			t.Fatalf("favorite ancient: %v", err)
		}
		got, err := store.ListConversations(2)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(got) != 3 {
			t.Fatalf("count: got %d, want 3", len(got))
		}
		if got[0].Name != "Newest" || got[1].Name != "New" || got[2].Name != "Ancient" {
			t.Fatalf("order = [%s %s %s], want [Newest New Ancient]", got[0].Name, got[1].Name, got[2].Name)
		}
	})

	t.Run("limit zero returns empty", func(t *testing.T) {
		got, err := store.ListConversations(0)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("count: got %d, want 0", len(got))
		}
	})
}

func TestListConversations_Empty(t *testing.T) {
	store := newTestStore(t)

	got, err := store.ListConversations(100)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("count: got %d, want 0", len(got))
	}
}

func TestUpdateConversationTimestamp(t *testing.T) {
	store := newTestStore(t)

	store.UpsertConversation(&Conversation{
		ConversationID: "conv-ts",
		Name:           "Timestamp Test",
		LastMessageTS:  1000,
	})

	t.Run("updates timestamp", func(t *testing.T) {
		if err := store.UpdateConversationTimestamp("conv-ts", 9999); err != nil {
			t.Fatalf("update ts: %v", err)
		}

		got, err := store.GetConversation("conv-ts")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got.LastMessageTS != 9999 {
			t.Errorf("last_message_ts: got %d, want 9999", got.LastMessageTS)
		}
	})

	t.Run("update on nonexistent conversation does not error", func(t *testing.T) {
		// SQLite UPDATE with no matching rows is not an error.
		if err := store.UpdateConversationTimestamp("no-such-id", 5000); err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
	})
}

func TestListConversations_AfterTimestampUpdate(t *testing.T) {
	store := newTestStore(t)

	// Insert two conversations.
	store.UpsertConversation(&Conversation{ConversationID: "c1", Name: "First", LastMessageTS: 1000})
	store.UpsertConversation(&Conversation{ConversationID: "c2", Name: "Second", LastMessageTS: 2000})

	// Verify initial ordering.
	got, _ := store.ListConversations(100)
	if got[0].Name != "Second" {
		t.Fatalf("initial first: got %q, want Second", got[0].Name)
	}

	// Update first conversation to have the latest timestamp.
	store.UpdateConversationTimestamp("c1", 5000)

	got, _ = store.ListConversations(100)
	if got[0].Name != "First" {
		t.Errorf("after update first: got %q, want First", got[0].Name)
	}
}

func TestUpsertConversation_DefaultValues(t *testing.T) {
	store := newTestStore(t)

	// Insert with minimal fields.
	conv := &Conversation{
		ConversationID: "minimal",
	}
	if err := store.UpsertConversation(conv); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	got, err := store.GetConversation("minimal")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "" {
		t.Errorf("name: got %q, want empty", got.Name)
	}
	if got.IsGroup {
		t.Error("is_group: got true, want false")
	}
	if got.LastMessageTS != 0 {
		t.Errorf("last_message_ts: got %d, want 0", got.LastMessageTS)
	}
	if got.UnreadCount != 0 {
		t.Errorf("unread_count: got %d, want 0", got.UnreadCount)
	}
}

func TestMergeConversationIDs(t *testing.T) {
	store := newTestStore(t)

	if err := store.UpsertConversation(&Conversation{
		ConversationID: "whatsapp:raw@lid",
		Name:           "Max Ghenis",
		Participants:   `[{"name":"Jenn","number":"+134149377675278"}]`,
		LastMessageTS:  3000,
		UnreadCount:    2,
		SourcePlatform: "whatsapp",
		IsFavorite:     true,
	}); err != nil {
		t.Fatalf("seed raw conversation: %v", err)
	}
	if err := store.UpsertConversation(&Conversation{
		ConversationID: "whatsapp:14699991654@s.whatsapp.net",
		Name:           "Jamie Rivera",
		Participants:   `[{"name":"Jamie Rivera","number":"+14699991654"}]`,
		LastMessageTS:  2000,
		UnreadCount:    0,
		SourcePlatform: "whatsapp",
	}); err != nil {
		t.Fatalf("seed canonical conversation: %v", err)
	}
	if err := store.UpsertMessage(&Message{
		MessageID:      "whatsapp:m1",
		ConversationID: "whatsapp:raw@lid",
		Body:           "hello",
		TimestampMS:    3000,
		SourcePlatform: "whatsapp",
		SourceID:       "m1",
	}); err != nil {
		t.Fatalf("seed raw message: %v", err)
	}
	if err := store.UpsertDraft(&Draft{
		DraftID:        "d1",
		ConversationID: "whatsapp:raw@lid",
		Body:           "draft",
		CreatedAt:      1,
	}); err != nil {
		t.Fatalf("seed raw draft: %v", err)
	}

	if err := store.MergeConversationIDs("whatsapp:raw@lid", "whatsapp:14699991654@s.whatsapp.net"); err != nil {
		t.Fatalf("MergeConversationIDs(): %v", err)
	}

	if _, err := store.GetConversation("whatsapp:raw@lid"); err == nil {
		t.Fatal("expected raw conversation to be deleted")
	}
	convo, err := store.GetConversation("whatsapp:14699991654@s.whatsapp.net")
	if err != nil {
		t.Fatalf("GetConversation(): %v", err)
	}
	if convo.Name != "Jamie Rivera" {
		t.Fatalf("name = %q, want canonical name", convo.Name)
	}
	if convo.LastMessageTS != 3000 {
		t.Fatalf("last message ts = %d, want 3000", convo.LastMessageTS)
	}
	if convo.UnreadCount != 2 {
		t.Fatalf("unread count = %d, want 2", convo.UnreadCount)
	}
	if !convo.IsFavorite {
		t.Fatal("favorite state should survive conversation merge")
	}

	msgs, err := store.GetMessagesByConversation("whatsapp:14699991654@s.whatsapp.net", 10)
	if err != nil {
		t.Fatalf("GetMessagesByConversation(): %v", err)
	}
	if len(msgs) != 1 || msgs[0].ConversationID != "whatsapp:14699991654@s.whatsapp.net" {
		t.Fatalf("messages after merge = %#v", msgs)
	}
	drafts, err := store.ListDrafts("whatsapp:14699991654@s.whatsapp.net")
	if err != nil {
		t.Fatalf("ListDrafts(): %v", err)
	}
	if len(drafts) != 1 || drafts[0].ConversationID != "whatsapp:14699991654@s.whatsapp.net" {
		t.Fatalf("drafts after merge = %#v", drafts)
	}
}

func TestDeleteConversationRemovesConversationMessagesAndDrafts(t *testing.T) {
	store := newTestStore(t)

	if err := store.UpsertConversation(&Conversation{
		ConversationID: "wa-group",
		Name:           "Spam Group",
		IsGroup:        true,
		LastMessageTS:  1234,
		SourcePlatform: "whatsapp",
	}); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}
	if err := store.UpsertMessage(&Message{
		MessageID:      "m1",
		ConversationID: "wa-group",
		Body:           "hi",
		TimestampMS:    1234,
		SourcePlatform: "whatsapp",
	}); err != nil {
		t.Fatalf("seed message: %v", err)
	}
	if err := store.UpsertDraft(&Draft{
		DraftID:        "d1",
		ConversationID: "wa-group",
		Body:           "draft",
		CreatedAt:      1,
	}); err != nil {
		t.Fatalf("seed draft: %v", err)
	}

	if err := store.DeleteConversation("wa-group"); err != nil {
		t.Fatalf("DeleteConversation(): %v", err)
	}

	if _, err := store.GetConversation("wa-group"); err == nil {
		t.Fatal("expected conversation to be deleted")
	}
	msgs, err := store.GetMessagesByConversation("wa-group", 10)
	if err != nil {
		t.Fatalf("GetMessagesByConversation(): %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("expected messages to be deleted, got %d", len(msgs))
	}
	drafts, err := store.ListDrafts("wa-group")
	if err != nil {
		t.Fatalf("ListDrafts(): %v", err)
	}
	if len(drafts) != 0 {
		t.Fatalf("expected drafts to be deleted, got %d", len(drafts))
	}
}

func TestListConversations_ManyEntries(t *testing.T) {
	store := newTestStore(t)

	// Insert many conversations.
	for i := 0; i < 50; i++ {
		store.UpsertConversation(&Conversation{
			ConversationID: fmt.Sprintf("conv-%03d", i),
			Name:           fmt.Sprintf("Conv %d", i),
			LastMessageTS:  int64(i * 100),
		})
	}

	t.Run("limit 10 returns 10", func(t *testing.T) {
		got, err := store.ListConversations(10)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(got) != 10 {
			t.Errorf("count: got %d, want 10", len(got))
		}
	})

	t.Run("all 50 returned with high limit", func(t *testing.T) {
		got, err := store.ListConversations(1000)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(got) != 50 {
			t.Errorf("count: got %d, want 50", len(got))
		}
	})
}

func TestSearchConversationsByMetadata(t *testing.T) {
	store := newTestStore(t)

	if err := store.UpsertConversation(&Conversation{
		ConversationID: "c1",
		Name:           "+1 (267) 555-0100",
		Participants:   `[{"name":"","number":"+12675550100"}]`,
		LastMessageTS:  2000,
		SourcePlatform: "sms",
	}); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}
	if err := store.UpsertMessage(&Message{
		MessageID:      "m1",
		ConversationID: "c1",
		SenderName:     "Nathan",
		SenderNumber:   "+12675550100",
		Body:           "See you soon",
		TimestampMS:    2000,
		SourcePlatform: "sms",
	}); err != nil {
		t.Fatalf("seed message: %v", err)
	}
	if err := store.UpsertContact(&Contact{
		ContactID: "contact-1",
		Name:      "Nathan",
		Number:    "+12675550100",
	}); err != nil {
		t.Fatalf("seed contact: %v", err)
	}

	got, err := store.SearchConversationsByMetadata("Nathan", 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d results, want 1", len(got))
	}
	if got[0].ConversationID != "c1" {
		t.Fatalf("got conversation %q, want c1", got[0].ConversationID)
	}
}

func TestApplyConversationSnapshotGuardsStaleState(t *testing.T) {
	store := newTestStore(t)

	seed := &Conversation{
		ConversationID: "conv-snap",
		Name:           "Alice",
		Participants:   `[{"name":"Alice","number":"+15551234567"}]`,
		LastMessageTS:  5000,
		UnreadCount:    1,
	}
	if err := store.UpsertConversation(seed); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := store.MarkConversationRead("conv-snap"); err != nil {
		t.Fatalf("mark read: %v", err)
	}

	t.Run("stale snapshot cannot resurrect unread or regress recency", func(t *testing.T) {
		stale := &Conversation{
			ConversationID: "conv-snap",
			Name:           "Alice",
			Participants:   seed.Participants,
			LastMessageTS:  4000, // older than stored 5000
			UnreadCount:    1,
		}
		if err := store.ApplyConversationSnapshot(stale); err != nil {
			t.Fatalf("apply: %v", err)
		}
		got, _ := store.GetConversation("conv-snap")
		if got.UnreadCount != 0 {
			t.Errorf("unread resurrected by stale snapshot: got %d, want 0", got.UnreadCount)
		}
		if got.LastMessageTS != 5000 {
			t.Errorf("recency regressed: got %d, want 5000", got.LastMessageTS)
		}
	})

	t.Run("equal-timestamp snapshot cannot resurrect unread", func(t *testing.T) {
		same := &Conversation{
			ConversationID: "conv-snap",
			Participants:   seed.Participants,
			LastMessageTS:  5000,
			UnreadCount:    1,
		}
		if err := store.ApplyConversationSnapshot(same); err != nil {
			t.Fatalf("apply: %v", err)
		}
		got, _ := store.GetConversation("conv-snap")
		if got.UnreadCount != 0 {
			t.Errorf("unread resurrected by same-age snapshot: got %d, want 0", got.UnreadCount)
		}
	})

	t.Run("newer snapshot sets unread and advances recency", func(t *testing.T) {
		newer := &Conversation{
			ConversationID: "conv-snap",
			Participants:   seed.Participants,
			LastMessageTS:  6000,
			UnreadCount:    1,
		}
		if err := store.ApplyConversationSnapshot(newer); err != nil {
			t.Fatalf("apply: %v", err)
		}
		got, _ := store.GetConversation("conv-snap")
		if got.UnreadCount != 1 || got.LastMessageTS != 6000 {
			t.Errorf("newer snapshot not applied: unread=%d ts=%d, want 1/6000", got.UnreadCount, got.LastMessageTS)
		}
	})

	t.Run("phone-side read always clears unread", func(t *testing.T) {
		read := &Conversation{
			ConversationID: "conv-snap",
			Participants:   seed.Participants,
			LastMessageTS:  6000,
			UnreadCount:    0,
		}
		if err := store.ApplyConversationSnapshot(read); err != nil {
			t.Fatalf("apply: %v", err)
		}
		got, _ := store.GetConversation("conv-snap")
		if got.UnreadCount != 0 {
			t.Errorf("phone read not honored: got %d, want 0", got.UnreadCount)
		}
	})

	t.Run("new conversation inserts as-is", func(t *testing.T) {
		fresh := &Conversation{
			ConversationID: "conv-new",
			Name:           "Bob",
			LastMessageTS:  100,
			UnreadCount:    1,
		}
		if err := store.ApplyConversationSnapshot(fresh); err != nil {
			t.Fatalf("apply: %v", err)
		}
		got, _ := store.GetConversation("conv-new")
		if got == nil || got.UnreadCount != 1 {
			t.Fatalf("fresh insert wrong: %+v", got)
		}
	})
}

func TestConversationDisplayProtocolPersistsFromMessages(t *testing.T) {
	store := newTestStore(t)

	if err := store.UpsertConversation(&Conversation{
		ConversationID: "c1",
		Name:           "Alice",
		LastMessageTS:  100,
		SourcePlatform: "sms",
	}); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}
	if err := store.UpsertMessage(&Message{
		MessageID:      "m1",
		ConversationID: "c1",
		Body:           "hello",
		TimestampMS:    100,
		Status:         "TOMBSTONE_RCS",
	}); err != nil {
		t.Fatalf("seed message: %v", err)
	}
	got, err := store.GetConversation("c1")
	if err != nil {
		t.Fatalf("get conversation: %v", err)
	}
	if got.DisplayProtocol != "RCS" {
		t.Fatalf("display protocol = %q, want RCS", got.DisplayProtocol)
	}

	if err := store.UpsertConversation(&Conversation{
		ConversationID: "c1",
		Name:           "Alice Updated",
		LastMessageTS:  200,
		SourcePlatform: "sms",
	}); err != nil {
		t.Fatalf("update conversation without protocol: %v", err)
	}
	got, err = store.GetConversation("c1")
	if err != nil {
		t.Fatalf("get updated conversation: %v", err)
	}
	if got.DisplayProtocol != "RCS" {
		t.Fatalf("display protocol after empty update = %q, want RCS", got.DisplayProtocol)
	}
}
