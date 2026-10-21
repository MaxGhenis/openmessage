package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// Identity rewrites move every row that names an identity onto another and
// delete it. These tests pin how each kind of row moves and how a collision
// with a row the target already has resolves.

const rewriteAccount = "whatsapp-primary"

type rewriteFixture struct {
	t     *testing.T
	store *Store
}

func newRewriteFixture(t *testing.T) *rewriteFixture {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "store.sqlite3"))
	if err != nil {
		t.Fatalf("Open(): %v", err)
	}
	t.Cleanup(func() { store.Close() })
	f := &rewriteFixture{t: t, store: store}
	f.exec(`INSERT INTO accounts (account_id, bridge_key, created_at_ms, updated_at_ms) VALUES (?, 'whatsmeow', 1, 1)`, rewriteAccount)
	return f
}

func (f *rewriteFixture) exec(query string, args ...any) {
	f.t.Helper()
	if _, err := f.store.db.Exec(query, args...); err != nil {
		f.t.Fatalf("%s: %v", strings.TrimSpace(query), err)
	}
}

func (f *rewriteFixture) identity(id, kind, canonical, name string, self bool) {
	f.exec(`INSERT INTO identities (identity_id, account_id, kind, canonical_value, raw_value, display_name, is_self, created_at_ms, updated_at_ms)
		VALUES (?, ?, ?, ?, ?, ?, ?, 10, 10)`, id, rewriteAccount, kind, canonical, canonical, name, self)
}

func (f *rewriteFixture) conversation(id, kind, title string) {
	f.exec(`INSERT INTO conversations (conversation_id, account_id, remote_conversation_id, kind, title, created_at_ms, updated_at_ms)
		VALUES (?, ?, ?, ?, ?, 10, 10)`, id, rewriteAccount, "whatsapp:"+id, kind, title)
}

func (f *rewriteFixture) participant(conversation, identity, name string, active bool, role string, joined any) {
	f.exec(`INSERT INTO conversation_participants (account_id, conversation_id, identity_id, role, display_name, is_active, joined_at_ms)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, rewriteAccount, conversation, identity, role, name, active, joined)
}

func (f *rewriteFixture) message(id, conversation, sender string) {
	f.exec(`INSERT INTO messages (message_id, conversation_id, account_id, remote_message_id, sender_identity_id, direction, occurred_at_ms, created_at_ms, updated_at_ms)
		VALUES (?, ?, ?, ?, ?, 'incoming', 100, 10, 10)`, id, conversation, rewriteAccount, id, sender)
}

func (f *rewriteFixture) reaction(message, conversation, reactor, emoji string, occurred, seq int64) {
	f.exec(`INSERT INTO reactions (message_id, reactor_key, account_id, conversation_id, reactor_identity_id, emoji, occurred_at_ms, source_seq_ms, created_at_ms, updated_at_ms)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 10, 10)`, message, reactor, rewriteAccount, conversation, reactor, emoji, occurred, seq)
}

func (f *rewriteFixture) person(id string) {
	f.exec(`INSERT INTO people (person_id, created_at_ms, updated_at_ms) VALUES (?, 10, 10)`, id)
}

func (f *rewriteFixture) link(identity, person string, primary bool) {
	f.exec(`INSERT INTO person_identities (identity_id, person_id, provenance, confidence, is_primary, linked_at_ms)
		VALUES (?, ?, 'import', 1.0, ?, 10)`, identity, person, primary)
}

func (f *rewriteFixture) scalar(query string, args ...any) string {
	f.t.Helper()
	var value sql.NullString
	if err := f.store.db.QueryRow(query, args...).Scan(&value); err != nil && !errors.Is(err, sql.ErrNoRows) {
		f.t.Fatalf("%s: %v", query, err)
	}
	return value.String
}

func (f *rewriteFixture) apply(rewrites ...IdentityRewrite) error {
	return f.store.ApplyIdentityRewrites(context.Background(), rewriteAccount, rewrites, 1000)
}

func lidRewrite(from, fromValue, to, toKind, toValue string, raw ...string) IdentityRewrite {
	return IdentityRewrite{
		FromIdentityID: from, FromKind: "e164", FromCanonicalValue: fromValue,
		ToIdentityID: to, ToKind: IdentityKind(toKind), ToCanonicalValue: toValue, ToRawValue: toValue,
		RawNames: raw,
	}
}

func TestIdentityRewriteMintsTheTargetAndMovesEveryRow(t *testing.T) {
	f := newRewriteFixture(t)
	f.identity("fake", "e164", "+98765432109876", "Ana", false)
	f.conversation("direct", "direct", "+98765432109876")
	f.conversation("group", "group", "+98765432109876")
	f.participant("direct", "fake", "+98765432109876", true, "member", nil)
	f.participant("group", "fake", "Ana B", true, "admin", 5)
	f.message("m1", "group", "fake")
	f.message("m2", "direct", "fake")
	f.reaction("m1", "group", "fake", "👍", 200, 2)
	f.person("p")
	f.link("fake", "p", true)

	if err := f.apply(lidRewrite("fake", "+98765432109876", "lid", "jid", "98765432109876@lid", "+98765432109876")); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got := f.scalar(`SELECT COUNT(*) FROM identities WHERE identity_id = 'fake'`); got != "0" {
		t.Fatalf("the rewritten identity is still there")
	}
	if got := f.scalar(`SELECT kind || ' ' || canonical_value || ' ' || raw_value || ' ' || display_name || ' ' || created_at_ms FROM identities WHERE identity_id = 'lid'`); got != "jid 98765432109876@lid 98765432109876@lid Ana 10" {
		t.Fatalf("target = %q", got)
	}
	if got := f.scalar(`SELECT group_concat(message_id) FROM (SELECT message_id FROM messages WHERE sender_identity_id = 'lid' ORDER BY message_id)`); got != "m1,m2" {
		t.Fatalf("messages from the target = %q", got)
	}
	if got := f.scalar(`SELECT group_concat(conversation_id || ':' || display_name || ':' || role) FROM (SELECT * FROM conversation_participants WHERE identity_id = 'lid' ORDER BY conversation_id)`); got != "direct::member,group:Ana B:admin" {
		t.Fatalf("participants = %q, want the raw name cleared and the real one kept", got)
	}
	if got := f.scalar(`SELECT title FROM conversations WHERE conversation_id = 'direct'`); got != "" {
		t.Fatalf("direct title = %q, want the fake number cleared", got)
	}
	if got := f.scalar(`SELECT title FROM conversations WHERE conversation_id = 'group'`); got != "+98765432109876" {
		t.Fatalf("group title = %q, want it untouched", got)
	}
	if got := f.scalar(`SELECT reactor_key || reactor_identity_id FROM reactions WHERE message_id = 'm1'`); got != "lidlid" {
		t.Fatalf("reaction = %q", got)
	}
	if got := f.scalar(`SELECT person_id || is_primary FROM person_identities WHERE identity_id = 'lid'`); got != "p1" {
		t.Fatalf("person link = %q", got)
	}

	// Applying the plan again is a no-op.
	if err := f.apply(lidRewrite("fake", "+98765432109876", "lid", "jid", "98765432109876@lid")); err != nil {
		t.Fatalf("re-apply: %v", err)
	}
}

func TestIdentityRewriteMergesIntoAnExistingTarget(t *testing.T) {
	f := newRewriteFixture(t)
	f.identity("fake", "e164", "+98765432109876", "+98765432109876", false)
	f.identity("phone", "e164", "+15557654321", "", false)
	f.conversation("group", "group", "")
	f.conversation("other", "group", "")
	// Both in "group": the active row wins; its blank name is filled.
	f.participant("group", "fake", "Ana", true, "admin", 5)
	f.participant("group", "phone", "", false, "member", 9)
	f.participant("other", "fake", "", true, "member", nil)
	f.message("m1", "group", "fake")
	f.message("m2", "group", "phone")
	f.message("m3", "group", "phone")
	// m1: the rewritten identity's reaction is newer; m2: the target's is;
	// m3: an exact tie keeps the target's.
	f.reaction("m1", "group", "fake", "🔥", 300, 3)
	f.reaction("m1", "group", "phone", "👍", 200, 2)
	f.reaction("m2", "group", "fake", "🔥", 100, 1)
	f.reaction("m2", "group", "phone", "👍", 200, 2)
	f.reaction("m3", "group", "fake", "🔥", 200, 2)
	f.reaction("m3", "group", "phone", "👍", 200, 2)
	f.person("p")
	f.link("fake", "p", true)
	f.link("phone", "p", false)

	if err := f.apply(lidRewrite("fake", "+98765432109876", "ignored", "e164", "+15557654321", "+98765432109876")); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got := f.scalar(`SELECT display_name FROM identities WHERE identity_id = 'phone'`); got != "" {
		t.Fatalf("target name = %q, want the raw name not carried", got)
	}
	if got := f.scalar(`SELECT COUNT(*) FROM identities WHERE identity_id IN ('fake', 'ignored')`); got != "0" {
		t.Fatalf("identities left = %s", got)
	}
	if got := f.scalar(`SELECT group_concat(conversation_id || ':' || display_name || ':' || role || ':' || is_active || ':' || COALESCE(joined_at_ms, '-')) FROM (SELECT * FROM conversation_participants WHERE identity_id = 'phone' ORDER BY conversation_id)`); got != "group:Ana:admin:1:5,other::member:1:-" {
		t.Fatalf("participants = %q", got)
	}
	if got := f.scalar(`SELECT group_concat(message_id || emoji) FROM (SELECT * FROM reactions WHERE reactor_key = 'phone' ORDER BY message_id)`); got != "m1🔥,m2👍,m3👍" {
		t.Fatalf("reactions = %q, want the newer per message and the target's on a tie", got)
	}
	if got := f.scalar(`SELECT COUNT(*) FROM reactions`); got != "3" {
		t.Fatalf("reaction rows = %s, want one per message", got)
	}
	if got := f.scalar(`SELECT group_concat(identity_id || is_primary) FROM person_identities`); got != "phone1" {
		t.Fatalf("person links = %q, want the target's, now primary", got)
	}
}

func TestIdentityRewriteFailsWholeOnAnUnsafeStep(t *testing.T) {
	cases := []struct {
		name  string
		setup func(f *rewriteFixture)
		want  string
	}{
		{
			name: "links to two people",
			setup: func(f *rewriteFixture) {
				f.person("p")
				f.person("q")
				f.link("fake", "p", false)
				f.link("phone", "q", false)
			},
			want: "links to person",
		},
		{
			name: "changed since the plan",
			setup: func(f *rewriteFixture) {
				f.exec(`UPDATE identities SET canonical_value = '+111' WHERE identity_id = 'fake'`)
			},
			want: "not the e164",
		},
		{
			name:  "own identity",
			setup: func(f *rewriteFixture) { f.exec(`UPDATE identities SET is_self = 1 WHERE identity_id = 'fake'`) },
			want:  "own",
		},
		{
			name:  "own target",
			setup: func(f *rewriteFixture) { f.exec(`UPDATE identities SET is_self = 1 WHERE identity_id = 'phone'`) },
			want:  "own",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newRewriteFixture(t)
			f.identity("first", "e164", "+11111111111111", "", false)
			f.identity("fake", "e164", "+98765432109876", "", false)
			f.identity("phone", "e164", "+15557654321", "", false)
			f.conversation("group", "group", "")
			f.message("m1", "group", "first")
			f.message("m2", "group", "fake")
			tc.setup(f)
			err := f.apply(
				lidRewrite("first", "+11111111111111", "first-lid", "jid", "11111111111111@lid"),
				lidRewrite("fake", "+98765432109876", "ignored", "e164", "+15557654321"),
			)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("apply error = %v, want %q", err, tc.want)
			}
			// The first rewrite rolled back with the second.
			if got := f.scalar(`SELECT sender_identity_id FROM messages WHERE message_id = 'm1'`); got != "first" {
				t.Fatalf("m1 sender = %q after a failed plan, want it untouched", got)
			}
			if got := f.scalar(`SELECT COUNT(*) FROM identities WHERE identity_id = 'first-lid'`); got != "0" {
				t.Fatal("a failed plan left a minted identity")
			}
		})
	}
}

// Live ingest keys a person's reaction by their identity id. A row keyed
// otherwise keeps its key, so re-keying it cannot collide, and names the
// target.
func TestIdentityRewriteRepointsAReactionKeyedOtherwise(t *testing.T) {
	f := newRewriteFixture(t)
	f.identity("fake", "e164", "+98765432109876", "", false)
	f.identity("phone", "e164", "+15557654321", "", false)
	f.conversation("group", "group", "")
	f.message("m1", "group", "fake")
	// A reaction whose key is not the identity id but whose identity is.
	f.exec(`INSERT INTO reactions (message_id, reactor_key, account_id, conversation_id, reactor_identity_id, emoji, occurred_at_ms, created_at_ms, updated_at_ms)
		VALUES ('m1', 'self', ?, 'group', 'phone', '👍', 1, 1, 1)`, rewriteAccount)
	if err := f.apply(lidRewrite("phone", "+15557654321", "x", "jid", "15557654321@lid")); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got := f.scalar(`SELECT reactor_key || '/' || reactor_identity_id FROM reactions`); got != "self/x" {
		t.Fatalf("reaction = %q, want its key kept and its identity repointed", got)
	}
}

// Under a snapshot fence the newer reaction is the later write (source_seq_ms
// first), as mergeDuplicateRows orders them; without one, the later event.
func TestIdentityRewriteOrdersReactionsLikeMessageMerges(t *testing.T) {
	f := newRewriteFixture(t)
	f.identity("fake", "e164", "+98765432109876", "", false)
	f.identity("phone", "e164", "+15557654321", "", false)
	f.conversation("group", "group", "")
	f.message("fenced", "group", "phone")
	f.message("delta", "group", "phone")
	f.exec(`INSERT INTO reaction_snapshot_fences (message_id, source_seq_ms, updated_at_ms) VALUES ('fenced', 3, 1)`)
	for _, message := range []string{"fenced", "delta"} {
		f.reaction(message, "group", "fake", "🔥", 100, 3)
		f.reaction(message, "group", "phone", "👍", 500, 2)
	}
	if err := f.apply(lidRewrite("fake", "+98765432109876", "x", "e164", "+15557654321")); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got := f.scalar(`SELECT emoji FROM reactions WHERE message_id = 'fenced'`); got != "🔥" {
		t.Errorf("fenced reaction = %q, want the later write", got)
	}
	if got := f.scalar(`SELECT emoji FROM reactions WHERE message_id = 'delta'`); got != "👍" {
		t.Errorf("delta reaction = %q, want the later event", got)
	}
}

// With no raw names to clear, the rewrite leaves the target's other chats
// alone: an untitled chat is not one whose raw title was cleared.
func TestIdentityRewriteWithoutRawNamesTouchesNoUnrelatedChat(t *testing.T) {
	f := newRewriteFixture(t)
	f.identity("fake", "e164", "+98765432109876", "Ana", false)
	f.identity("phone", "e164", "+15557654321", "", false)
	f.conversation("solo", "direct", "")
	f.conversation("lid-chat", "direct", "")
	f.participant("solo", "phone", "", true, "member", nil)
	f.participant("lid-chat", "fake", "", true, "member", nil)
	if err := f.apply(lidRewrite("fake", "+98765432109876", "x", "e164", "+15557654321")); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got := f.scalar(`SELECT group_concat(conversation_id || ':' || updated_at_ms) FROM (SELECT * FROM conversations ORDER BY conversation_id)`); got != "lid-chat:10,solo:10" {
		t.Fatalf("conversations = %q, want both untouched", got)
	}
}
