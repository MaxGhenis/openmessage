package sqlite

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Adversarial checks written by the round-two independent review of #230
// (GPT-6.1 Sol, against 75b2dd33) and kept as regression tests.

// Check both sides of the expansion cutoff with real identities, messages
// from another sender, and every additional filter. The count intentionally
// measures the sender/date range; the result statements must keep all filters.
func TestSenderFallbackCutoffMatchesLikeWithAllFilters(t *testing.T) {
	store := openRepositoryTestStore(t)
	db := store.db
	db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	repository := mustMessageRepository(t, store, 100)
	for _, account := range []string{"a", "b"} {
		mustExec(t, db, `INSERT INTO accounts (account_id, bridge_key, created_at_ms, updated_at_ms) VALUES (?, 'signal_cli', 1, 1)`, account)
		mustExec(t, db, `INSERT INTO conversations (conversation_id, account_id, remote_conversation_id, kind, created_at_ms, updated_at_ms) VALUES (?, ?, ?, 'direct', 1, 1)`, "conversation-"+account, account, account)
		mustExec(t, db, `INSERT INTO identities (identity_id, account_id, kind, canonical_value, raw_value, created_at_ms, updated_at_ms) VALUES (?, ?, 'phone', 'target', 'target', 1, 1)`, "sender-"+account, account)
	}
	mustExec(t, db, `INSERT INTO identities (identity_id, account_id, kind, canonical_value, raw_value, created_at_ms, updated_at_ms) VALUES ('other', 'a', 'phone', 'other', 'other', 1, 1)`)
	mustExec(t, db, `WITH RECURSIVE n(i) AS (VALUES(1) UNION ALL SELECT i+1 FROM n WHERE i < 998)
		INSERT INTO identities (identity_id, account_id, kind, canonical_value, raw_value, created_at_ms, updated_at_ms)
		SELECT printf('padding-%04d', i), 'a', printf('kind-%04d', i), 'target', 'target', 1, 1 FROM n`)
	for i := range 24 {
		account, sender := "a", "sender-a"
		if i%3 == 1 {
			account, sender = "b", "sender-b"
		} else if i%3 == 2 {
			sender = "other"
		}
		body := "other words"
		if i < 6 {
			body = "lunch at the quay"
		}
		mustExec(t, db, `INSERT INTO messages (message_id, conversation_id, account_id, remote_message_id, sender_identity_id, direction, body, occurred_at_ms, reply_to_remote_id, created_at_ms, updated_at_ms)
			VALUES (?, ?, ?, ?, ?, 'incoming', ?, ?, ?, 1, 2)`, fmt.Sprintf("message-%02d", i), "conversation-"+account, account, fmt.Sprintf("remote-%d", i), sender, body, 100+i/3, "reply\x00id")
	}
	filters := []SearchQuery{
		{SenderCanonicalValue: "target", Limit: 50},
		{SenderCanonicalValue: "target", AccountID: "a", Limit: 1},
		{SenderCanonicalValue: "target", AccountID: "b", Limit: 500},
		{SenderCanonicalValue: "target", AccountID: "none", Limit: 50},
		{SenderCanonicalValue: "target", ConversationID: "conversation-a", AccountID: "a", SinceMS: 100, UntilMS: 104, Limit: 50},
		{SenderCanonicalValue: "target", ConversationID: "conversation-a", AccountID: "b", SinceMS: 100, UntilMS: 104, Limit: 50},
		{SenderCanonicalValue: "target", SinceMS: 101, UntilMS: 106, Limit: 50},
	}
	for _, size := range []int{1000, 1001} {
		if size == 1001 {
			mustExec(t, db, `INSERT INTO identities (identity_id, account_id, kind, canonical_value, raw_value, created_at_ms, updated_at_ms) VALUES ('one-more', 'a', 'one-more', 'target', 'target', 1, 1)`)
		}
		ids, err := senderIdentityIDs(ctx, db, "target")
		if err != nil || len(ids) != size {
			t.Fatalf("resolve %d identities: len=%d, err=%v", size, len(ids), err)
		}
		for _, filter := range filters {
			likeSQL, likeArgs := likeSearchMessagesStatement("quay", filter)
			want := queryMessages(t, db, likeSQL, likeArgs...)
			statement, args := senderLikeSearchStatement("quay", filter, ids)
			if got := queryMessages(t, db, statement, args...); !reflect.DeepEqual(got, want) {
				t.Fatalf("%d identities, sender LIKE %+v: got %v, want %v", size, filter, messageIDs(got), messageIDs(want))
			}
			if len(args) > 1006 {
				t.Fatalf("%d identities: LIKE has %d parameters", size, len(args))
			}
			statement, args = senderRangeCountStatement(filter, ids)
			args = append(args, 100)
			var count, expected int
			if err := db.QueryRowContext(ctx, statement, args...).Scan(&count); err != nil {
				t.Fatal(err)
			}
			conditions, countArgs := searchMessageFilters(SearchQuery{SenderCanonicalValue: "target", SinceMS: filter.SinceMS, UntilMS: filter.UntilMS})
			if err := db.QueryRowContext(ctx, `SELECT count(*) FROM messages AS m WHERE `+strings.Join(conditions, " AND "), countArgs...).Scan(&expected); err != nil || count != expected {
				t.Fatalf("%d identities, count %+v: got %d, want %d, err=%v", size, filter, count, expected, err)
			}
			expression, _ := trigramMatchQuery("quay")
			for _, bound := range []int{4, 7} {
				got, complete, err := scopeIndexSearch(ctx, db, expression, "quay", filter, ids, bound)
				if err != nil || complete != (bound == 7) || (complete && !reflect.DeepEqual(got, want)) || (!complete && got != nil) {
					t.Fatalf("%d identities, index %+v bound=%d: got %v, complete=%v, err=%v, want %v", size, filter, bound, messageIDs(got), complete, err, messageIDs(want))
				}
			}
			for _, plan := range []searchPlan{{window: 1, indexMinRows: 1000}, {window: 1, indexMinRows: 16}, {window: 1, indexMinRows: 8}} {
				got, path, err := repository.searchMessages(ctx, "quay", filter, plan)
				if err != nil || !reflect.DeepEqual(got, want) {
					t.Fatalf("%d identities, search %+v plan=%+v path=%s: got %v, want %v, err=%v", size, filter, plan, path, messageIDs(got), messageIDs(want), err)
				}
			}
		}
	}
}

// Force the scoped paths over malformed UTF-8, replacement characters,
// non-ASCII folding, wildcard/quote syntax, NULs, and longer AND expressions.
// Tied timestamps and a one-connection pool also exercise split ordering and
// transaction routing. Compare whole Message values, including nullable data.
func TestScopedSearchAdversarialBytesMatchLike(t *testing.T) {
	store := openRepositoryTestStore(t)
	db := store.db
	db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	repository := mustMessageRepository(t, store, 100)
	mustExec(t, db, `INSERT INTO accounts (account_id, bridge_key, created_at_ms, updated_at_ms) VALUES ('a', 'signal_cli', 1, 1)`)
	mustExec(t, db, `INSERT INTO conversations (conversation_id, account_id, remote_conversation_id, kind, created_at_ms, updated_at_ms) VALUES ('conversation-a', 'a', 'a', 'direct', 1, 1)`)
	for _, sender := range []string{"target", "other"} {
		mustExec(t, db, `INSERT INTO identities (identity_id, account_id, kind, canonical_value, raw_value, created_at_ms, updated_at_ms) VALUES (?, 'a', 'phone', ?, ?, 1, 1)`, sender, sender, sender)
	}
	queries := []string{
		"abc", "abc\x00def", "ab\x00cd", "a\x80bc", "\x80abc", "\x80\x80\x80",
		"\xc0\x80abc", "\xc1\xbfabc", "\xed\xa0\x80abc", "\xef\xbf\xbeabc",
		"\xf4\x90\x80\x80abc", "\xf8\x88\x80\x80\x80abc", "\xfe\x80\x80abc",
		"\xe2\x82abc", "\xf0\x9fabc", "\xc3abc", "\xffabc", "\ufffdabc", "\u0080abc",
		"ab%cde_fgh", `a"b`, `"""`, `\abc`, "Kab", "Kab", "ςab", "Σab",
		strings.Repeat("abc", 400), strings.Repeat(`a"`, 600),
	}
	for i, body := range queries {
		for _, sender := range []any{"target", "other", nil} {
			id := fmt.Sprintf("message-%02d-%v", i, sender)
			mustExec(t, db, `INSERT INTO messages (message_id, conversation_id, account_id, remote_message_id, sender_identity_id, direction, body, occurred_at_ms, created_at_ms, updated_at_ms)
				VALUES (?, 'conversation-a', 'a', ?, ?, 'incoming', ?, ?, 1, 2)`, id, id, sender, body, 100+i/3)
		}
	}
	filters := []SearchQuery{
		{ConversationID: "conversation-a", Limit: 500},
		{ConversationID: "conversation-a", Limit: 1},
		{ConversationID: "conversation-a", SenderCanonicalValue: "target", SinceMS: 101, UntilMS: 105, Limit: 500},
		{ConversationID: "conversation-a", SenderCanonicalValue: "target", SinceMS: 105, UntilMS: 101, Limit: 1},
		{ConversationID: "conversation-a", AccountID: "none", Limit: 500},
		{SenderCanonicalValue: "target", Limit: 500},
		{SenderCanonicalValue: "target", AccountID: "a", SinceMS: 101, UntilMS: 105, Limit: 1},
		{SenderCanonicalValue: "target", AccountID: "none", Limit: 500},
	}
	paths := map[searchPath]int{}
	for _, query := range queries {
		for _, filter := range filters {
			normalized := filter
			if normalized.UntilMS > 0 && normalized.UntilMS < normalized.SinceMS {
				normalized.SinceMS, normalized.UntilMS = normalized.UntilMS, normalized.SinceMS
			}
			statement, args := likeSearchMessagesStatement(query, normalized)
			want := queryMessages(t, db, statement, args...)
			for _, plan := range []searchPlan{{window: 1, indexMinRows: 1000}, {window: 2, indexMinRows: 20}, {window: 1, indexMinRows: 2}} {
				got, path, err := repository.searchMessages(ctx, query, filter, plan)
				paths[path]++
				if err != nil || !reflect.DeepEqual(got, want) {
					t.Fatalf("query=%q filter=%+v plan=%+v path=%s: got %v, want %v, err=%v", query, filter, plan, path, messageIDs(got), messageIDs(want), err)
				}
			}
			if expression, ok := trigramMatchQuery(query); ok {
				got, complete, err := scopeIndexSearch(ctx, db, expression, query, normalized, nil, 3*len(queries)+1)
				if err != nil || !complete || !reflect.DeepEqual(got, want) {
					t.Fatalf("direct index query=%q filter=%+v: got %v, want %v, complete=%v, err=%v", query, filter, messageIDs(got), messageIDs(want), complete, err)
				}
			}
		}
	}
	for _, path := range []searchPath{searchPathLike, searchPathConversationWindow, searchPathConversationRest, searchPathScopeIndex} {
		if paths[path] == 0 {
			t.Errorf("no adversarial search used %s: %v", path, paths)
		}
	}
	t.Logf("searches by path: %v", paths)
}
