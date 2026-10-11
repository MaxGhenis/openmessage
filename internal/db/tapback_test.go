package db

import (
	"database/sql"
	"path/filepath"
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

func TestRebuildTapbackReactions(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "messages.db")
	store, err := New(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	mustUpsert := func(m *Message) {
		t.Helper()
		if err := store.UpsertMessage(m); err != nil {
			t.Fatalf("upsert: %v", err)
		}
	}
	reactions := func(id string) string {
		t.Helper()
		m, err := store.GetMessageByID(id)
		if err != nil || m == nil {
			t.Fatalf("message %s: %v", id, err)
		}
		return m.Reactions
	}
	hearted := func(id string) bool { return strings.Contains(reactions(id), "❤️") }
	tapback := func(id, body string, ts int64) *Message {
		return &Message{MessageID: id, ConversationID: "c1", SenderNumber: "+15551234567", Body: body, TimestampMS: ts}
	}
	rebuild := func(list ...*Message) int {
		t.Helper()
		n, err := store.RebuildTapbackReactions("c1", list)
		if err != nil {
			t.Fatalf("rebuild: %v", err)
		}
		return n
	}

	mustUpsert(&Message{MessageID: "hello10", ConversationID: "c1", Body: "hello", TimestampMS: 10_000, IsFromMe: true})
	mustUpsert(&Message{MessageID: "other", ConversationID: "c2", Body: "hello", TimestampMS: 10_000, Reactions: `[{"emoji":"👍","count":1}]`})
	loved := tapback("t20", "Loved “hello”", 20_000)
	removed := tapback("t30", "Removed a heart from “hello”", 30_000)

	// Order is the caller's: add then remove leaves nothing, whatever
	// order the two arrived in.
	if n := rebuild(removed); n != 0 || hearted("hello10") {
		t.Fatalf("removal alone: changed %d, heart %v", n, hearted("hello10"))
	}
	if n := rebuild(loved, removed); n != 0 || hearted("hello10") {
		t.Fatalf("add then remove: changed %d, heart %v", n, hearted("hello10"))
	}
	if n := rebuild(loved); n != 1 || !hearted("hello10") {
		t.Fatalf("add: changed %d, heart %v", n, hearted("hello10"))
	}

	// A rebuild that lands where the store already is commits nothing.
	observer, err := sql.Open("sqlite", dbPath+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer observer.Close()
	var before, after int64
	observer.QueryRow(`PRAGMA data_version`).Scan(&before)
	if n := rebuild(loved); n != 0 {
		t.Fatalf("replay changed %d rows", n)
	}
	observer.QueryRow(`PRAGMA data_version`).Scan(&after)
	if before != after {
		t.Fatalf("replay committed (data_version %d -> %d)", before, after)
	}

	// An older message with the same text, stored later, becomes the target.
	mustUpsert(&Message{MessageID: "hello15", ConversationID: "c1", Body: "hello", TimestampMS: 15_000})
	if n := rebuild(loved); n != 2 || hearted("hello10") || !hearted("hello15") {
		t.Fatalf("moved target: changed %d, hello10 %v, hello15 %v", n, hearted("hello10"), hearted("hello15"))
	}

	// A tapback with no target is stored as text, and becomes a reaction
	// once its target is stored.
	early := tapback("t40", "Liked “see you”", 40_000)
	if n := rebuild(loved, early); n != 1 {
		t.Fatalf("unmatched tapback: changed %d", n)
	}
	if m, _ := store.GetMessageByID("t40"); m == nil || m.Body != early.Body {
		t.Fatalf("unmatched tapback not stored as text: %+v", m)
	}
	mustUpsert(&Message{MessageID: "seeyou", ConversationID: "c1", Body: "see you", TimestampMS: 35_000})
	if n := rebuild(loved, early); n != 2 {
		t.Fatalf("matched tapback: changed %d", n)
	}
	if m, _ := store.GetMessageByID("t40"); m != nil {
		t.Fatalf("matched tapback still stored as text: %+v", m)
	}
	if !strings.Contains(reactions("seeyou"), "👍") {
		t.Fatalf("seeyou reactions = %q", reactions("seeyou"))
	}

	// No tapbacks left: no reactions left.
	if n := rebuild(); n != 2 || hearted("hello15") || reactions("seeyou") != "" {
		t.Fatalf("empty rebuild: changed %d, hello15 %q, seeyou %q", n, reactions("hello15"), reactions("seeyou"))
	}

	// Only the named conversation is touched.
	if got := reactions("other"); got != `[{"emoji":"👍","count":1}]` {
		t.Fatalf("other conversation reactions changed: %q", got)
	}
	if _, err := store.RebuildTapbackReactions("c1", []*Message{{MessageID: "x", ConversationID: "c2", Body: "Loved “hello”"}}); err == nil {
		t.Fatal("a tapback from another conversation must be refused")
	}
}

// TestTapbackTargetIgnoresInsertionOrder: two messages with the same text
// and millisecond are matched the same way however they were inserted.
func TestTapbackTargetIgnoresInsertionOrder(t *testing.T) {
	targetOf := func(order []string) string {
		store := newTestStore(t)
		for _, id := range order {
			if err := store.UpsertMessage(&Message{MessageID: id, ConversationID: "c1", Body: "same", TimestampMS: 1000}); err != nil {
				t.Fatal(err)
			}
		}
		if applied, err := store.ApplyTapback(&Message{ConversationID: "c1", SenderNumber: "+1555", Body: "Loved “same”", TimestampMS: 2000}); err != nil || !applied {
			t.Fatalf("apply: %v %v", applied, err)
		}
		for _, id := range order {
			if m, _ := store.GetMessageByID(id); m != nil && m.Reactions != "" {
				return id
			}
		}
		return ""
	}
	if a, b := targetOf([]string{"m-a", "m-b"}), targetOf([]string{"m-b", "m-a"}); a != b || a == "" {
		t.Fatalf("targets differ by insertion order: %q vs %q", a, b)
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
