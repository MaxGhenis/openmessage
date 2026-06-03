package db

import "testing"

// TestMessageHasContent pins down what counts as "real" conversational content.
// Empty placeholder/reaction-artifact messages (no body, no media, no reactions)
// must not be treated as content, so they never advance a conversation's recency.
func TestMessageHasContent(t *testing.T) {
	cases := []struct {
		name string
		msg  *Message
		want bool
	}{
		{"nil", nil, false},
		{"empty", &Message{}, false},
		{"whitespace body only", &Message{Body: "   \n\t"}, false},
		{"body", &Message{Body: "hello"}, true},
		{"media only", &Message{MediaID: "media-123"}, true},
		{"reactions only", &Message{Reactions: `[{"emoji":"👍","count":1}]`}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := MessageHasContent(tc.msg); got != tc.want {
				t.Errorf("MessageHasContent = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestAdvanceConversationRecency is the core rule for the recents-ordering bug:
// a contentless message (e.g. an emoji reaction in a *group* arriving as an
// empty stub in the reactor's 1:1 thread) must NOT float that conversation up,
// while a real message must, and recency must never move backwards.
func TestAdvanceConversationRecency(t *testing.T) {
	store := newTestStore(t)
	if err := store.UpsertConversation(&Conversation{ConversationID: "c1", LastMessageTS: 1000}); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}

	tsOf := func() int64 {
		c, err := store.GetConversation("c1")
		if err != nil {
			t.Fatalf("get conversation: %v", err)
		}
		return c.LastMessageTS
	}

	// A newer but contentless message must not advance recency.
	if err := store.AdvanceConversationRecency(&Message{ConversationID: "c1", TimestampMS: 5000}); err != nil {
		t.Fatalf("advance (contentless): %v", err)
	}
	if got := tsOf(); got != 1000 {
		t.Errorf("contentless message advanced recency to %d, want 1000", got)
	}

	// A newer message with real content must advance recency.
	if err := store.AdvanceConversationRecency(&Message{ConversationID: "c1", TimestampMS: 6000, Body: "hi"}); err != nil {
		t.Fatalf("advance (content): %v", err)
	}
	if got := tsOf(); got != 6000 {
		t.Errorf("content message did not advance recency: got %d, want 6000", got)
	}

	// Recency must never move backwards, even for a real older message.
	if err := store.AdvanceConversationRecency(&Message{ConversationID: "c1", TimestampMS: 2000, Body: "old"}); err != nil {
		t.Fatalf("advance (older content): %v", err)
	}
	if got := tsOf(); got != 6000 {
		t.Errorf("older message moved recency backwards: got %d, want 6000", got)
	}
}

// TestRepairContentlessRecency fixes conversations already corrupted by the bug:
// when a conversation's recency is being held up by a contentless top message,
// it should drop to its newest content-bearing message. Conversations whose
// recency comes from metadata (newer than any stored message — e.g. history not
// yet backfilled) must be left untouched.
func TestRepairContentlessRecency(t *testing.T) {
	store := newTestStore(t)

	seed := func(convID string, lastTS int64, msgs []*Message) {
		if err := store.UpsertConversation(&Conversation{ConversationID: convID, LastMessageTS: lastTS}); err != nil {
			t.Fatalf("seed conversation %s: %v", convID, err)
		}
		for _, m := range msgs {
			if err := store.UpsertMessage(m); err != nil {
				t.Fatalf("seed message: %v", err)
			}
		}
	}

	// "phantom": top message is contentless and is setting recency -> should drop.
	seed("phantom", 5000, []*Message{
		{MessageID: "p-real", ConversationID: "phantom", Body: "hey", TimestampMS: 1000},
		{MessageID: "p-empty", ConversationID: "phantom", Body: "", TimestampMS: 5000},
	})
	// "metadata": recency is ahead of all stored messages (backfill lag) -> untouched.
	seed("metadata", 9000, []*Message{
		{MessageID: "m-real", ConversationID: "metadata", Body: "yo", TimestampMS: 2000},
	})
	// "normal": recency already matches newest content -> unchanged.
	seed("normal", 3000, []*Message{
		{MessageID: "n-real", ConversationID: "normal", Body: "sup", TimestampMS: 3000},
	})

	n, err := store.RepairContentlessRecency()
	if err != nil {
		t.Fatalf("repair: %v", err)
	}
	if n != 1 {
		t.Errorf("expected 1 conversation repaired, got %d", n)
	}

	want := map[string]int64{"phantom": 1000, "metadata": 9000, "normal": 3000}
	for id, exp := range want {
		c, err := store.GetConversation(id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		if c.LastMessageTS != exp {
			t.Errorf("%s: last_message_ts = %d, want %d", id, c.LastMessageTS, exp)
		}
	}
}
