package db

import "testing"

// TestUnreadSurvivesStaleConversationResync reproduces the recurring-unread bug:
// the user reads a conversation locally (mark-read → unread_count=0), but Google
// Messages keeps its own unread flag set and re-broadcasts the same conversation
// (same last_message_ts, GetUnread()==true) on every periodic sync. That stale
// re-sync must NOT resurrect the unread badge.
func TestUnreadSurvivesStaleConversationResync(t *testing.T) {
	store := newTestStore(t)

	// A genuinely unread conversation arrives from Google.
	if err := store.UpsertConversation(&Conversation{
		ConversationID: "c1", Name: "559 316-5695",
		LastMessageTS: 1000, UnreadCount: 1,
	}); err != nil {
		t.Fatalf("initial upsert: %v", err)
	}
	if got, _ := store.GetConversation("c1"); got.UnreadCount != 1 {
		t.Fatalf("fresh unread: got %d, want 1", got.UnreadCount)
	}

	// User opens the thread → marked read locally.
	if err := store.MarkConversationRead("c1"); err != nil {
		t.Fatalf("mark read: %v", err)
	}
	if got, _ := store.GetConversation("c1"); got.UnreadCount != 0 {
		t.Fatalf("after mark-read: got %d, want 0", got.UnreadCount)
	}

	// Google re-syncs the SAME conversation, still flagged unread (no new
	// message — same last_message_ts). This is the periodic resync that keeps
	// resurrecting the badge.
	if err := store.UpsertConversation(&Conversation{
		ConversationID: "c1", Name: "559 316-5695",
		LastMessageTS: 1000, UnreadCount: 1,
	}); err != nil {
		t.Fatalf("stale resync: %v", err)
	}
	if got, _ := store.GetConversation("c1"); got.UnreadCount != 0 {
		t.Errorf("BUG: stale resync resurrected unread badge: got %d, want 0", got.UnreadCount)
	}
}

// TestUnreadRaisedByGenuinelyNewMessage guards that the fix does NOT suppress
// real new activity: a conversation event carrying a newer last_message_ts must
// still mark the thread unread, even after it was previously read.
func TestUnreadRaisedByGenuinelyNewMessage(t *testing.T) {
	store := newTestStore(t)

	if err := store.UpsertConversation(&Conversation{
		ConversationID: "c1", Name: "Bob", LastMessageTS: 1000, UnreadCount: 1,
	}); err != nil {
		t.Fatalf("initial: %v", err)
	}
	if err := store.MarkConversationRead("c1"); err != nil {
		t.Fatalf("mark read: %v", err)
	}

	// A genuinely new message arrives → newer last_message_ts, unread flag set.
	if err := store.UpsertConversation(&Conversation{
		ConversationID: "c1", Name: "Bob", LastMessageTS: 2000, UnreadCount: 1,
	}); err != nil {
		t.Fatalf("new message upsert: %v", err)
	}
	if got, _ := store.GetConversation("c1"); got.UnreadCount != 1 {
		t.Errorf("genuinely new message should mark unread: got %d, want 1", got.UnreadCount)
	}

	// Read it, then another stale resync at the same ts → stays read.
	if err := store.MarkConversationRead("c1"); err != nil {
		t.Fatalf("mark read 2: %v", err)
	}
	if err := store.UpsertConversation(&Conversation{
		ConversationID: "c1", Name: "Bob", LastMessageTS: 2000, UnreadCount: 1,
	}); err != nil {
		t.Fatalf("stale resync 2: %v", err)
	}
	if got, _ := store.GetConversation("c1"); got.UnreadCount != 0 {
		t.Errorf("stale resync after second read: got %d, want 0", got.UnreadCount)
	}
}

// TestUnreadClearedByUpstreamRead guards that when Google itself reports the
// conversation as read (GetUnread()==false), we honor it even without a newer
// message — e.g. the user read it on their phone.
func TestUnreadClearedByUpstreamRead(t *testing.T) {
	store := newTestStore(t)

	if err := store.UpsertConversation(&Conversation{
		ConversationID: "c1", Name: "Carol", LastMessageTS: 1000, UnreadCount: 1,
	}); err != nil {
		t.Fatalf("initial: %v", err)
	}
	// Google reports it read (unread=0) at the same ts.
	if err := store.UpsertConversation(&Conversation{
		ConversationID: "c1", Name: "Carol", LastMessageTS: 1000, UnreadCount: 0,
	}); err != nil {
		t.Fatalf("upstream read: %v", err)
	}
	if got, _ := store.GetConversation("c1"); got.UnreadCount != 0 {
		t.Errorf("upstream read should clear unread: got %d, want 0", got.UnreadCount)
	}
}
