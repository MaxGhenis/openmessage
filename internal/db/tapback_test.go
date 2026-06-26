package db

import (
	"strings"
	"testing"
)

func TestParseTapback(t *testing.T) {
	cases := []struct {
		body   string
		emoji  string
		quoted string
		remove bool
		ok     bool
	}{
		{`Loved "Great news!"`, "❤️", "Great news!", false, true},
		{`Liked "ok sounds good"`, "👍", "ok sounds good", false, true},
		{`Disliked "no way"`, "👎", "no way", false, true},
		{`Laughed at "lol"`, "😂", "lol", false, true},
		{`Emphasized "this is important"`, "‼️", "this is important", false, true},
		{`Questioned "really?"`, "❓", "really?", false, true},
		// Curly quotes (what iMessage actually sends)
		{"Loved “hello there”", "❤️", "hello there", false, true},
		// Removal
		{`Removed a heart from "hello"`, "❤️", "hello", true, true},
		// Newer iOS sends the actual emoji
		{`Reacted 🎉 to "we did it"`, "🎉", "we did it", false, true},
		// Not tapbacks
		{`Loved it`, "", "", false, false},
		{`Hey, are you free tonight?`, "", "", false, false},
		{``, "", "", false, false},
	}
	for _, tc := range cases {
		tb, ok := ParseTapback(tc.body)
		if ok != tc.ok {
			t.Errorf("ParseTapback(%q) ok = %v, want %v", tc.body, ok, tc.ok)
			continue
		}
		if !ok {
			continue
		}
		if tb.Emoji != tc.emoji || tb.Quoted != tc.quoted || tb.Remove != tc.remove {
			t.Errorf("ParseTapback(%q) = {%q, %q, remove=%v}, want {%q, %q, remove=%v}",
				tc.body, tb.Emoji, tb.Quoted, tb.Remove, tc.emoji, tc.quoted, tc.remove)
		}
	}
}

func TestApplyTapback(t *testing.T) {
	store := newTestStore(t)
	mustUpsert := func(m *Message) {
		if err := store.UpsertMessage(m); err != nil {
			t.Fatalf("upsert: %v", err)
		}
	}

	// Original message in the thread.
	mustUpsert(&Message{MessageID: "m1", ConversationID: "c1", SenderName: "Me", Body: "dinner at 7?", TimestampMS: 1000, IsFromMe: true})

	// An iPhone "Loved" tapback arrives as SMS text.
	applied, err := store.ApplyTapback(&Message{
		MessageID: "t1", ConversationID: "c1", SenderName: "Sarah", SenderNumber: "+15551234567",
		Body: `Loved "dinner at 7?"`, TimestampMS: 2000,
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !applied {
		t.Fatal("expected tapback to be applied")
	}
	target, _ := store.GetMessageByID("m1")
	if target == nil || !strings.Contains(target.Reactions, "❤️") {
		t.Errorf("expected ❤️ reaction on m1, got reactions=%q", target.Reactions)
	}
	// The tapback text itself must NOT be stored as a standalone message.
	if got, _ := store.GetMessageByID("t1"); got != nil {
		t.Errorf("tapback message should not be stored as a regular message")
	}

	// Plain text is not a tapback.
	if applied, _ := store.ApplyTapback(&Message{ConversationID: "c1", Body: "see you then", TimestampMS: 3000}); applied {
		t.Error("plain text should not be treated as a tapback")
	}

	// A tapback whose quoted text matches nothing is not applied (caller keeps it).
	if applied, _ := store.ApplyTapback(&Message{ConversationID: "c1", Body: `Liked "nonexistent message"`, TimestampMS: 4000}); applied {
		t.Error("tapback with no matching target should not be applied")
	}

	// Truncated quote (long original) matches by prefix.
	mustUpsert(&Message{MessageID: "m2", ConversationID: "c1", Body: "This is a much longer message that iMessage truncates", TimestampMS: 5000})
	applied2, _ := store.ApplyTapback(&Message{ConversationID: "c1", SenderNumber: "+15551234567", Body: "Laughed at “This is a much longer message that…”", TimestampMS: 6000})
	if !applied2 {
		t.Error("expected truncated-quote tapback to match the original by prefix")
	}

	// Guard against false positives: a NON-truncated quote must match a message
	// in full. A user who literally types `Loved "hi"` must not have it silently
	// converted just because some earlier message starts with "hi".
	mustUpsert(&Message{MessageID: "m3", ConversationID: "c1", Body: "hi there, how are you?", TimestampMS: 7000})
	appliedFP, _ := store.ApplyTapback(&Message{ConversationID: "c1", SenderNumber: "+15551234567", Body: `Loved "hi"`, TimestampMS: 8000})
	if appliedFP {
		t.Error("a non-truncated quote must require an exact match, not a prefix match")
	}

	// Regression: a tapback on a message that ends in a period must still match.
	// The quoted text carries the trailing `.`, so a non-truncated comparison
	// must not strip it (it previously did, leaking the tapback through as the
	// literal text `Loved "see you then."`).
	mustUpsert(&Message{MessageID: "m4", ConversationID: "c1", Body: "see you then.", TimestampMS: 9000})
	appliedDot, _ := store.ApplyTapback(&Message{ConversationID: "c1", SenderNumber: "+15551234567", Body: `Loved "see you then."`, TimestampMS: 10000})
	if !appliedDot {
		t.Error("expected a tapback on a period-terminated message to match exactly")
	}
	if target, _ := store.GetMessageByID("m4"); target == nil || !strings.Contains(target.Reactions, "❤️") {
		t.Errorf("expected ❤️ reaction on m4 (period-terminated), got %v", target)
	}
}

func TestRepairTapbacks(t *testing.T) {
	store := newTestStore(t)
	must := func(m *Message) {
		if err := store.UpsertMessage(m); err != nil {
			t.Fatalf("upsert: %v", err)
		}
	}
	must(&Message{MessageID: "a", ConversationID: "c1", Body: "great idea", TimestampMS: 1000, IsFromMe: true})
	must(&Message{MessageID: "b", ConversationID: "c1", SenderNumber: "+1555", Body: `Loved "great idea"`, TimestampMS: 2000})
	must(&Message{MessageID: "c", ConversationID: "c1", Body: "a normal reply", TimestampMS: 3000})
	must(&Message{MessageID: "d", ConversationID: "c1", SenderNumber: "+1555", Body: `Liked "no such message"`, TimestampMS: 4000}) // no target

	n, err := store.RepairTapbacks()
	if err != nil {
		t.Fatalf("repair: %v", err)
	}
	if n != 1 {
		t.Errorf("repaired = %d, want 1", n)
	}
	// The matched tapback row is gone, its reaction lives on the target.
	if got, _ := store.GetMessageByID("b"); got != nil {
		t.Error("converted tapback message should be deleted")
	}
	if target, _ := store.GetMessageByID("a"); target == nil || !strings.Contains(target.Reactions, "❤️") {
		t.Error("expected ❤️ reaction on the target message")
	}
	// The unmatched tapback and normal messages are untouched.
	if got, _ := store.GetMessageByID("d"); got == nil {
		t.Error("unmatched tapback should be kept as a normal message")
	}
}
