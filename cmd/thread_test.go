package cmd

import (
	"testing"
	"time"

	"github.com/maxghenis/openmessage/internal/db"
)

// newThreadTestStore builds an in-memory store seeded with two conversations:
//   - conv "2787" ("Korey Klein", sms): a 3-message back-and-forth.
//   - conv "ava-1" ("Ava"): a single message, used for no-cross-talk checks.
//
// It returns the store so tests can exercise resolveThread directly without
// touching any real data dir.
func newThreadTestStore(t *testing.T) *db.Store {
	t.Helper()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	mustUpsertConv(t, store, &db.Conversation{
		ConversationID: "2787",
		Name:           "Korey Klein",
		Participants:   `[{"name":"Korey Klein","number":"+15551112222"}]`,
		SourcePlatform: "sms",
		LastMessageTS:  day("2026-03-13"),
	})
	mustUpsertConv(t, store, &db.Conversation{
		ConversationID: "ava-1",
		Name:           "Ava",
		Participants:   `[{"name":"Ava","number":"+15553334444"}]`,
		SourcePlatform: "sms",
		LastMessageTS:  day("2026-02-01"),
	})

	// Seed out of chronological order on purpose: resolveThread must sort.
	mustUpsertMsg(t, store, &db.Message{
		MessageID: "k2", ConversationID: "2787",
		SenderName: "Korey Klein", SenderNumber: "+15551112222",
		Body: "Hey, Max - it's Korey at BG.", TimestampMS: dayT("2026-03-11", 1),
	})
	mustUpsertMsg(t, store, &db.Message{
		MessageID: "k3", ConversationID: "2787",
		Body: "Great to meet you", TimestampMS: dayT("2026-03-13", 2), IsFromMe: true,
	})
	mustUpsertMsg(t, store, &db.Message{
		MessageID: "k1", ConversationID: "2787",
		Body: "RCS chat with Korey", TimestampMS: dayT("2026-03-11", 0), IsFromMe: true,
	})
	mustUpsertMsg(t, store, &db.Message{
		MessageID: "a1", ConversationID: "ava-1",
		SenderName: "Ava", SenderNumber: "+15553334444",
		Body: "unrelated", TimestampMS: dayT("2026-02-01", 0),
	})

	return store
}

func mustUpsertConv(t *testing.T, s *db.Store, c *db.Conversation) {
	t.Helper()
	if err := s.UpsertConversation(c); err != nil {
		t.Fatalf("upsert conversation %s: %v", c.ConversationID, err)
	}
}

func mustUpsertMsg(t *testing.T, s *db.Store, m *db.Message) {
	t.Helper()
	if err := s.UpsertMessage(m); err != nil {
		t.Fatalf("upsert message %s: %v", m.MessageID, err)
	}
}

// day returns the local-midnight ms for a YYYY-MM-DD date.
func day(d string) int64 {
	t, err := time.ParseInLocation("2006-01-02", d, time.Local)
	if err != nil {
		panic(err)
	}
	return t.UnixMilli()
}

// dayT offsets a date by a few minutes so seeded messages have distinct,
// orderable timestamps within the same day.
func dayT(d string, minute int) int64 {
	return day(d) + int64(minute)*60_000
}

func bodies(msgs []*db.Message) []string {
	out := make([]string, len(msgs))
	for i, m := range msgs {
		out[i] = m.Body
	}
	return out
}

func TestResolveThread_ByConversationID(t *testing.T) {
	store := newThreadTestStore(t)

	res, err := resolveThread(store, "2787", threadOptions{Limit: 50})
	if err != nil {
		t.Fatalf("resolveThread: %v", err)
	}
	if len(res.Matches) != 0 {
		t.Fatalf("expected a resolved thread, got %d disambiguation matches", len(res.Matches))
	}
	if res.Conversation == nil || res.Conversation.ConversationID != "2787" {
		t.Fatalf("conversation: got %+v, want id 2787", res.Conversation)
	}
	if res.Label != "Korey Klein" {
		t.Errorf("label: got %q, want %q", res.Label, "Korey Klein")
	}
	got := bodies(res.Messages)
	want := []string{"RCS chat with Korey", "Hey, Max - it's Korey at BG.", "Great to meet you"}
	if len(got) != len(want) {
		t.Fatalf("message count: got %d %v, want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("message[%d]: got %q, want %q (chronological order)", i, got[i], want[i])
		}
	}
	// Cross-conversation isolation: Ava's message must not appear.
	for _, m := range res.Messages {
		if m.ConversationID != "2787" {
			t.Errorf("leaked message from %s", m.ConversationID)
		}
	}
}

func TestResolveThread_ByName(t *testing.T) {
	store := newThreadTestStore(t)

	res, err := resolveThread(store, "Korey", threadOptions{Limit: 50})
	if err != nil {
		t.Fatalf("resolveThread: %v", err)
	}
	if len(res.Matches) != 0 {
		t.Fatalf("expected single match, got %d", len(res.Matches))
	}
	if res.Conversation == nil || res.Conversation.ConversationID != "2787" {
		t.Fatalf("resolved wrong conversation: %+v", res.Conversation)
	}
	if len(res.Messages) != 3 {
		t.Errorf("messages: got %d, want 3", len(res.Messages))
	}
}

func TestResolveThread_ByNumber(t *testing.T) {
	store := newThreadTestStore(t)

	// The number belongs to the Korey conversation via its participants JSON, so
	// metadata resolution should land on conv 2787.
	res, err := resolveThread(store, "+15551112222", threadOptions{Limit: 50})
	if err != nil {
		t.Fatalf("resolveThread: %v", err)
	}
	if len(res.Messages) == 0 {
		t.Fatal("expected messages for the number, got none")
	}
	for _, m := range res.Messages {
		if m.ConversationID != "2787" {
			t.Errorf("unexpected conversation %s in number thread", m.ConversationID)
		}
	}
}

func TestResolveThread_NumberInMultipleConversations(t *testing.T) {
	store := newThreadTestStore(t)

	// The same number appears (as a sender) in two conversations. Resolving by
	// that number is ambiguous and must disambiguate rather than dump a thread.
	shared := "+15557778888"
	mustUpsertConv(t, store, &db.Conversation{
		ConversationID: "c-a", Name: "Team A", Participants: "[]",
		SourcePlatform: "sms", LastMessageTS: dayT("2026-04-02", 0),
	})
	mustUpsertConv(t, store, &db.Conversation{
		ConversationID: "c-b", Name: "Team B", Participants: "[]",
		SourcePlatform: "sms", LastMessageTS: dayT("2026-04-01", 0),
	})
	mustUpsertMsg(t, store, &db.Message{
		MessageID: "ca1", ConversationID: "c-a",
		SenderNumber: shared, Body: "from A", TimestampMS: dayT("2026-04-02", 0),
	})
	mustUpsertMsg(t, store, &db.Message{
		MessageID: "cb1", ConversationID: "c-b",
		SenderNumber: shared, Body: "from B", TimestampMS: dayT("2026-04-01", 0),
	})

	res, err := resolveThread(store, shared, threadOptions{Limit: 50})
	if err != nil {
		t.Fatalf("resolveThread: %v", err)
	}
	if len(res.Messages) != 0 {
		t.Errorf("ambiguous number must not dump a thread, got %d messages", len(res.Messages))
	}
	if len(res.Matches) != 2 {
		t.Fatalf("expected 2 disambiguation matches for shared number, got %d", len(res.Matches))
	}
}

func TestResolveThread_Limit(t *testing.T) {
	store := newThreadTestStore(t)

	res, err := resolveThread(store, "2787", threadOptions{Limit: 2})
	if err != nil {
		t.Fatalf("resolveThread: %v", err)
	}
	if len(res.Messages) != 2 {
		t.Fatalf("limit: got %d messages, want 2", len(res.Messages))
	}
	// --limit takes the most recent N but still prints chronologically, so the
	// two kept messages are the latest two, oldest-first.
	got := bodies(res.Messages)
	want := []string{"Hey, Max - it's Korey at BG.", "Great to meet you"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("limited message[%d]: got %q, want %q", i, got[i], want[i])
		}
	}
}

func TestResolveThread_SinceUntil(t *testing.T) {
	store := newThreadTestStore(t)

	// Window covering only 2026-03-11 should drop the 03-13 message.
	since := day("2026-03-11")
	until := day("2026-03-11") + (24*60*60*1000 - 1)
	res, err := resolveThread(store, "2787", threadOptions{Limit: 50, SinceMS: since, UntilMS: until})
	if err != nil {
		t.Fatalf("resolveThread: %v", err)
	}
	got := bodies(res.Messages)
	want := []string{"RCS chat with Korey", "Hey, Max - it's Korey at BG."}
	if len(got) != len(want) {
		t.Fatalf("windowed count: got %d %v, want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("windowed message[%d]: got %q, want %q", i, got[i], want[i])
		}
	}

	// --since alone (no --until) keeps only the later message.
	res2, err := resolveThread(store, "2787", threadOptions{Limit: 50, SinceMS: day("2026-03-12")})
	if err != nil {
		t.Fatalf("resolveThread since-only: %v", err)
	}
	if g := bodies(res2.Messages); len(g) != 1 || g[0] != "Great to meet you" {
		t.Errorf("since-only: got %v, want [Great to meet you]", g)
	}

	// --until is inclusive of the whole day: a bound on 2026-03-11 keeps both
	// of that day's messages (they sit at 20:22 local).
	res3, err := resolveThread(store, "2787", threadOptions{Limit: 50, UntilMS: day("2026-03-11") + (24*60*60*1000 - 1)})
	if err != nil {
		t.Fatalf("resolveThread until-only: %v", err)
	}
	if g := bodies(res3.Messages); len(g) != 2 {
		t.Errorf("until-only inclusive: got %v, want 2 messages", g)
	}
}

// TestResolveThread_LimitWithinWindow guards the correctness property of the
// date-bounded path: when --limit is smaller than the number of messages in the
// window, the *newest* ones are kept (not the oldest), still printed oldest→newest.
func TestResolveThread_LimitWithinWindow(t *testing.T) {
	store := newThreadTestStore(t)

	// Three messages on the same day; window covers all three, limit keeps 2.
	for i, body := range []string{"w-oldest", "w-middle", "w-newest"} {
		mustUpsertMsg(t, store, &db.Message{
			MessageID:      "w" + string(rune('0'+i)),
			ConversationID: "2787",
			SenderName:     "Korey Klein",
			Body:           body,
			TimestampMS:    dayT("2026-07-01", i*5),
		})
	}
	since := day("2026-07-01")
	until := day("2026-07-01") + (24*60*60*1000 - 1)

	res, err := resolveThread(store, "2787", threadOptions{Limit: 2, SinceMS: since, UntilMS: until})
	if err != nil {
		t.Fatalf("resolveThread: %v", err)
	}
	got := bodies(res.Messages)
	want := []string{"w-middle", "w-newest"} // newest 2, chronological
	if len(got) != len(want) {
		t.Fatalf("limited window count: got %d %v, want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("limited window[%d]: got %q, want %q", i, got[i], want[i])
		}
	}
}

func TestResolveThread_NoMatch(t *testing.T) {
	store := newThreadTestStore(t)

	res, err := resolveThread(store, "Nobody", threadOptions{Limit: 50})
	if err != nil {
		t.Fatalf("resolveThread: %v", err)
	}
	if res.Conversation != nil {
		t.Errorf("expected no conversation, got %+v", res.Conversation)
	}
	if len(res.Messages) != 0 {
		t.Errorf("expected no messages, got %d", len(res.Messages))
	}
	if len(res.Matches) != 0 {
		t.Errorf("expected no matches, got %d", len(res.Matches))
	}
}

func TestResolveThread_MultiMatchDisambiguation(t *testing.T) {
	store := newThreadTestStore(t)

	// Two conversations whose names share a token → ambiguous query.
	mustUpsertConv(t, store, &db.Conversation{
		ConversationID: "smith-1", Name: "Alex Smith",
		Participants: "[]", SourcePlatform: "sms", LastMessageTS: dayT("2026-05-02", 0),
	})
	mustUpsertConv(t, store, &db.Conversation{
		ConversationID: "smith-2", Name: "Jordan Smith",
		Participants: "[]", SourcePlatform: "imessage", LastMessageTS: dayT("2026-05-01", 0),
	})

	res, err := resolveThread(store, "Smith", threadOptions{Limit: 50})
	if err != nil {
		t.Fatalf("resolveThread: %v", err)
	}
	if len(res.Messages) != 0 {
		t.Errorf("disambiguation must not dump a thread, got %d messages", len(res.Messages))
	}
	if len(res.Matches) != 2 {
		t.Fatalf("expected 2 disambiguation matches, got %d", len(res.Matches))
	}
	// Ordered by last_message_ts DESC → smith-1 first.
	if res.Matches[0].ConversationID != "smith-1" || res.Matches[1].ConversationID != "smith-2" {
		t.Errorf("disambiguation order: got %s,%s want smith-1,smith-2",
			res.Matches[0].ConversationID, res.Matches[1].ConversationID)
	}
}

func TestReverseMessages(t *testing.T) {
	msgs := []*db.Message{{MessageID: "1"}, {MessageID: "2"}, {MessageID: "3"}}
	reverseMessages(msgs)
	if msgs[0].MessageID != "3" || msgs[2].MessageID != "1" {
		t.Errorf("reverse: got %s..%s, want 3..1", msgs[0].MessageID, msgs[2].MessageID)
	}
}
