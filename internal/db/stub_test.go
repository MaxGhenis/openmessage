package db

import "testing"

func TestIsEmptyStubMessage(t *testing.T) {
	cases := []struct {
		name string
		msg  *Message
		want bool
	}{
		{"completed empty incoming", &Message{Body: "", Status: "INCOMING_COMPLETE"}, true},
		{"completed empty delivered", &Message{Body: "", Status: "DELIVERED"}, true},
		{"whitespace-only body", &Message{Body: "   ", Status: "INCOMING_COMPLETE"}, true},
		{"auto-downloading placeholder", &Message{Body: "", Status: "INCOMING_AUTO_DOWNLOADING"}, false},
		{"sending placeholder", &Message{Body: "", Status: "OUTGOING_SENDING"}, false},
		{"has body", &Message{Body: "hey", Status: "INCOMING_COMPLETE"}, false},
		{"has media", &Message{MediaID: "m1", Status: "INCOMING_COMPLETE"}, false},
		{"has reactions", &Message{Reactions: `[{"emoji":"❤️","count":1}]`, Status: "INCOMING_COMPLETE"}, false},
		{"tombstone/system", &Message{Body: "", Status: "TOMBSTONE_ENCRYPTED"}, false},
		{"unknown status", &Message{Body: "", Status: ""}, false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsEmptyStubMessage(tc.msg); got != tc.want {
				t.Errorf("IsEmptyStubMessage = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRepairEmptyStubMessages(t *testing.T) {
	store := newTestStore(t)
	must := func(m *Message) {
		if err := store.UpsertMessage(m); err != nil {
			t.Fatalf("upsert: %v", err)
		}
	}
	seedConv := func(id string, lastTS int64) {
		if err := store.UpsertConversation(&Conversation{ConversationID: id, LastMessageTS: lastTS}); err != nil {
			t.Fatalf("seed conv: %v", err)
		}
	}

	// c1: a real message + a completed empty stub that's setting recency.
	seedConv("c1", 5000)
	must(&Message{MessageID: "c1-real", ConversationID: "c1", Body: "hi", TimestampMS: 1000, Status: "INCOMING_COMPLETE"})
	must(&Message{MessageID: "c1-stub", ConversationID: "c1", Body: "", TimestampMS: 5000, Status: "INCOMING_COMPLETE"})

	// c2 (the Joey case): only empty stubs.
	seedConv("c2", 3000)
	must(&Message{MessageID: "c2-s1", ConversationID: "c2", Body: "", TimestampMS: 2000, Status: "INCOMING_COMPLETE"})
	must(&Message{MessageID: "c2-s2", ConversationID: "c2", Body: "", TimestampMS: 3000, Status: "INCOMING_COMPLETE"})

	// c3: only real messages — untouched.
	seedConv("c3", 4000)
	must(&Message{MessageID: "c3-real", ConversationID: "c3", Body: "yo", TimestampMS: 4000, Status: "INCOMING_COMPLETE"})

	// c4: an auto-downloading placeholder — kept (content may still arrive).
	seedConv("c4", 6000)
	must(&Message{MessageID: "c4-ph", ConversationID: "c4", Body: "", TimestampMS: 6000, Status: "INCOMING_AUTO_DOWNLOADING"})

	n, err := store.RepairEmptyStubMessages()
	if err != nil {
		t.Fatalf("repair: %v", err)
	}
	if n != 3 {
		t.Errorf("deleted = %d, want 3 (c1-stub + c2 x2)", n)
	}

	// Stubs gone, real/placeholder messages kept.
	for _, gone := range []string{"c1-stub", "c2-s1", "c2-s2"} {
		if m, _ := store.GetMessageByID(gone); m != nil {
			t.Errorf("%s should be deleted", gone)
		}
	}
	for _, kept := range []string{"c1-real", "c3-real", "c4-ph"} {
		if m, _ := store.GetMessageByID(kept); m == nil {
			t.Errorf("%s should be kept", kept)
		}
	}

	// Recency recomputed for affected conversations.
	want := map[string]int64{"c1": 1000, "c2": 0, "c3": 4000, "c4": 6000}
	for id, exp := range want {
		c, _ := store.GetConversation(id)
		if c == nil || c.LastMessageTS != exp {
			got := int64(-1)
			if c != nil {
				got = c.LastMessageTS
			}
			t.Errorf("%s last_message_ts = %d, want %d", id, got, exp)
		}
	}
}
