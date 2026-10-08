package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"slices"
	"testing"
)

const rebindTestTimeMS = int64(1_790_000_000_000)

func seedRebindFixture(t *testing.T, db *sql.DB) {
	t.Helper()
	mustExec(t, db, `
		INSERT INTO accounts (account_id, bridge_key, created_at_ms, updated_at_ms)
		VALUES ('google', 'google_messages', ?, ?)
	`, rebindTestTimeMS, rebindTestTimeMS)
	mustExec(t, db, `
		INSERT INTO identities (identity_id, account_id, kind, canonical_value, raw_value, created_at_ms, updated_at_ms)
		VALUES
			('karl', 'google', 'e164', '+12026022529', '+12026022529', ?, ?),
			('shoshana', 'google', 'e164', '+15169021075', '+15169021075', ?, ?)
	`, rebindTestTimeMS, rebindTestTimeMS, rebindTestTimeMS, rebindTestTimeMS)
	mustExec(t, db, `
		INSERT INTO conversations (conversation_id, account_id, remote_conversation_id, kind, title, last_message_at_ms, created_at_ms, updated_at_ms)
		VALUES
			('karl-sms', 'google', '6001', 'direct', 'Karl SMS', 100, ?, ?),
			('karl-rcs', 'google', '6002', 'direct', 'Karl RCS', 300, ?, ?),
			('karl-old', 'google', 'displaced:2916:karl-old', 'direct', 'Karl', 200, ?, ?),
			('family', 'google', '5001', 'group', 'Family', 100, ?, ?),
			('book-club', 'google', '5002', 'group', 'Book club', 200, ?, ?),
			('trio', 'google', '5003', 'group', 'Trio', 300, ?, ?)
	`,
		rebindTestTimeMS, rebindTestTimeMS, rebindTestTimeMS, rebindTestTimeMS,
		rebindTestTimeMS, rebindTestTimeMS, rebindTestTimeMS, rebindTestTimeMS,
		rebindTestTimeMS, rebindTestTimeMS, rebindTestTimeMS, rebindTestTimeMS,
	)
	mustExec(t, db, `
		INSERT INTO conversation_participants (account_id, conversation_id, identity_id)
		VALUES
			('google', 'karl-sms', 'karl'),
			('google', 'karl-rcs', 'karl'),
			('google', 'karl-old', 'karl'),
			('google', 'family', 'karl'),
			('google', 'family', 'shoshana'),
			('google', 'book-club', 'karl'),
			('google', 'book-club', 'shoshana'),
			('google', 'trio', 'karl')
	`)
}

func conversationIDs(conversations []Conversation) []string {
	ids := make([]string, 0, len(conversations))
	for _, conversation := range conversations {
		ids = append(ids, conversation.ConversationID)
	}
	return ids
}

func TestRosterCandidateListsReturnEveryMatchMostRecentFirst(t *testing.T) {
	store := openIdentityGraphTestStore(t)
	seedRebindFixture(t, store.db)

	directs, err := store.ListDirectConversationsBySolePeer("google", "karl")
	if err != nil {
		t.Fatalf("ListDirectConversationsBySolePeer(): %v", err)
	}
	if got, want := conversationIDs(directs), []string{"karl-rcs", "karl-old", "karl-sms"}; !slices.Equal(got, want) {
		t.Fatalf("direct candidates = %v, want %v (displaced included, most recent first)", got, want)
	}
	first, err := store.FindDirectConversationBySolePeer("google", "karl")
	if err != nil || first.ConversationID != "karl-rcs" {
		t.Fatalf("FindDirectConversationBySolePeer() = %q, %v; want karl-rcs", first.ConversationID, err)
	}
	if none, err := store.ListDirectConversationsBySolePeer("google", "shoshana"); err != nil || len(none) != 0 {
		t.Fatalf("Shoshana direct candidates = %v, %v; want none", conversationIDs(none), err)
	}
	if _, err := store.FindDirectConversationBySolePeer("google", "shoshana"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("FindDirectConversationBySolePeer(shoshana) err = %v, want ErrNotFound", err)
	}

	groups, err := store.ListGroupConversationsByPeerSet("google", []string{"shoshana", "karl"})
	if err != nil {
		t.Fatalf("ListGroupConversationsByPeerSet(): %v", err)
	}
	if got, want := conversationIDs(groups), []string{"book-club", "family"}; !slices.Equal(got, want) {
		t.Fatalf("group candidates = %v, want %v (exact rosters only, most recent first)", got, want)
	}
	if empty, err := store.ListGroupConversationsByPeerSet("google", nil); err != nil || len(empty) != 0 {
		t.Fatalf("empty peer set = %v, %v; want none", conversationIDs(empty), err)
	}
	if _, err := store.FindGroupConversationByPeerSet("google", nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("FindGroupConversationByPeerSet(nil) err = %v, want ErrNotFound", err)
	}
}

func TestRemoteIDSpaceEpochAdvancesOnceFromTheObservedEpoch(t *testing.T) {
	store := openIdentityGraphTestStore(t)
	seedRebindFixture(t, store.db)

	epoch, err := store.RemoteIDSpaceEpoch("google")
	if err != nil || epoch != 0 {
		t.Fatalf("initial epoch = %d, %v; want 0", epoch, err)
	}
	if epoch, err = store.AdvanceRemoteIDSpaceEpoch("google", 0); err != nil || epoch != 1 {
		t.Fatalf("advance from 0 = %d, %v; want 1", epoch, err)
	}
	// A caller still holding epoch 0 must not advance a second time.
	if epoch, err = store.AdvanceRemoteIDSpaceEpoch("google", 0); err != nil || epoch != 1 {
		t.Fatalf("stale advance from 0 = %d, %v; want 1 unchanged", epoch, err)
	}
	if epoch, err = store.AdvanceRemoteIDSpaceEpoch("google", 1); err != nil || epoch != 2 {
		t.Fatalf("advance from 1 = %d, %v; want 2", epoch, err)
	}
	if _, err := store.RemoteIDSpaceEpoch("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing account epoch err = %v, want ErrNotFound", err)
	}

	// Account upserts carry no epoch and must leave it alone.
	account, err := store.GetAccount("google")
	if err != nil {
		t.Fatalf("GetAccount(): %v", err)
	}
	account.DisplayName = "renamed"
	if err := store.UpsertAccount(account); err != nil {
		t.Fatalf("UpsertAccount(): %v", err)
	}
	if epoch, err = store.RemoteIDSpaceEpoch("google"); err != nil || epoch != 2 {
		t.Fatalf("epoch after account upsert = %d, %v; want 2", epoch, err)
	}
}

func TestConversationRemoteBindingTransitions(t *testing.T) {
	store := openIdentityGraphTestStore(t)
	seedRebindFixture(t, store.db)

	assertBinding := func(step string, want RemoteBinding) {
		t.Helper()
		got, err := store.ConversationRemoteBinding("google", "karl-sms")
		if err != nil || got != want {
			t.Fatalf("%s: binding = %+v (err %v), want %+v", step, got, err, want)
		}
	}
	// Rows written by anything other than ingest's message mint count as
	// announced in the starting epoch.
	assertBinding("fixture insert", RemoteBinding{AnnouncedEpoch: 0, BoundEpoch: 0})

	if err := store.MarkConversationProvisional("google", "karl-sms", 1); err != nil {
		t.Fatalf("MarkConversationProvisional(): %v", err)
	}
	assertBinding("message mint", RemoteBinding{Provisional: true, BoundEpoch: 1})
	if err := store.MarkConversationBound("google", "karl-sms", 0); err != nil {
		t.Fatalf("MarkConversationBound(0): %v", err)
	}
	assertBinding("bound never moves backwards", RemoteBinding{Provisional: true, BoundEpoch: 1})
	if err := store.MarkConversationAnnounced("google", "karl-sms", 2); err != nil {
		t.Fatalf("MarkConversationAnnounced(2): %v", err)
	}
	assertBinding("announced", RemoteBinding{AnnouncedEpoch: 2, BoundEpoch: 2})
	if err := store.MarkConversationAnnounced("google", "karl-sms", 1); err != nil {
		t.Fatalf("MarkConversationAnnounced(1): %v", err)
	}
	assertBinding("announcement never moves backwards", RemoteBinding{AnnouncedEpoch: 2, BoundEpoch: 2})

	conversation, err := store.GetConversation("karl-sms")
	if err != nil {
		t.Fatalf("GetConversation(): %v", err)
	}
	conversation.Title = "Karl (SMS)"
	if err := store.UpsertConversation(conversation); err != nil {
		t.Fatalf("UpsertConversation(): %v", err)
	}
	assertBinding("after a conversation upsert", RemoteBinding{AnnouncedEpoch: 2, BoundEpoch: 2})
	if err := store.ReassignConversationRemoteID("google", "7001", "karl-sms", rebindTestTimeMS); err != nil {
		t.Fatalf("ReassignConversationRemoteID(): %v", err)
	}
	assertBinding("after a store-level rebind", RemoteBinding{AnnouncedEpoch: 2, BoundEpoch: 2})

	for name, mark := range map[string]func(string, string, int64) error{
		"announced":   store.MarkConversationAnnounced,
		"provisional": store.MarkConversationProvisional,
		"bound":       store.MarkConversationBound,
	} {
		if err := mark("google", "missing", 0); !errors.Is(err, ErrNotFound) {
			t.Fatalf("mark %s on a missing conversation: err = %v, want ErrNotFound", name, err)
		}
		if err := mark("google", "karl-sms", -1); err == nil {
			t.Fatalf("mark %s accepted a negative epoch", name)
		}
	}
	if _, err := store.ConversationRemoteBinding("google", "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing conversation binding err = %v, want ErrNotFound", err)
	}
}

func TestMergeConversationIntoMovesHistoryAndBindsTheWireID(t *testing.T) {
	store := openIdentityGraphTestStore(t)
	seedRebindFixture(t, store.db)
	// karl-rcs (6002) was minted for a thread that is really karl-sms's
	// continuation: it holds a re-served copy of a karl-sms message, one new
	// message, and one whose remote ID karl-sms already uses for another message.
	mustExec(t, store.db, `
		INSERT INTO messages (message_id, conversation_id, account_id, remote_message_id, sender_identity_id, direction, body, occurred_at_ms, created_at_ms, updated_at_ms)
		VALUES
			('old-1', 'karl-sms', 'google', '100', 'karl', 'incoming', 'hello', 1000, ?1, ?1),
			('old-2', 'karl-sms', 'google', '300', 'karl', 'incoming', 'older 300', 900, ?1, ?1),
			('dup-1', 'karl-rcs', 'google', '200', 'karl', 'incoming', 'hello', 1000, ?1, ?1),
			('new-1', 'karl-rcs', 'google', '201', 'karl', 'incoming', 'new', 5000, ?1, ?1),
			('clash', 'karl-rcs', 'google', '300', 'karl', 'incoming', 'newer 300', 6000, ?1, ?1)
	`, rebindTestTimeMS)

	merge, err := store.MergeConversationInto(context.Background(), "google", "6002", "karl-rcs", "karl-sms", rebindTestTimeMS)
	if err != nil {
		t.Fatalf("MergeConversationInto(): %v", err)
	}
	if merge != (ConversationMerge{Moved: 1, Duplicates: 1, Kept: 1}) {
		t.Fatalf("merge = %+v, want 1 moved, 1 duplicate, 1 kept", merge)
	}
	target, err := store.GetConversationByRemote("google", "6002")
	if err != nil || target.ConversationID != "karl-sms" {
		t.Fatalf("6002 bound to %q (err %v), want karl-sms", target.ConversationID, err)
	}
	if target.LastMessageAtMS != 5000 {
		t.Fatalf("karl-sms recency = %d, want 5000 from the moved message", target.LastMessageAtMS)
	}
	source, err := store.GetConversation("karl-rcs")
	if err != nil {
		t.Fatalf("source with a kept message was dropped: %v", err)
	}
	if source.RemoteConversationID != displacedRemoteID("6002", "karl-rcs") {
		t.Fatalf("source remote ID = %q, want displaced", source.RemoteConversationID)
	}
	var homes = map[string]string{}
	rows, err := store.db.Query(`SELECT message_id, conversation_id FROM messages`)
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	for rows.Next() {
		var id, conversation string
		if err := rows.Scan(&id, &conversation); err != nil {
			t.Fatalf("scan: %v", err)
		}
		homes[id] = conversation
	}
	rows.Close()
	want := map[string]string{"old-1": "karl-sms", "old-2": "karl-sms", "new-1": "karl-sms", "clash": "karl-rcs"}
	if len(homes) != len(want) {
		t.Fatalf("messages after merge = %v, want %v (the duplicate deleted)", homes, want)
	}
	for id, conversation := range want {
		if homes[id] != conversation {
			t.Fatalf("message %s in %q, want %q", id, homes[id], conversation)
		}
	}

	// A source left empty is deleted; one that no longer holds the wire ID is
	// refused.
	mustExec(t, store.db, `
		INSERT INTO conversations (conversation_id, account_id, remote_conversation_id, kind, created_at_ms, updated_at_ms)
		VALUES ('empty', 'google', '6003', 'direct', ?1, ?1)
	`, rebindTestTimeMS)
	merge, err = store.MergeConversationInto(context.Background(), "google", "6003", "empty", "karl-sms", rebindTestTimeMS)
	if err != nil || !merge.Dropped {
		t.Fatalf("merge of an empty source = %+v, %v; want dropped", merge, err)
	}
	if _, err := store.GetConversation("empty"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty source still present: %v", err)
	}
	if _, err := store.MergeConversationInto(context.Background(), "google", "6001", "karl-rcs", "karl-sms", rebindTestTimeMS); !errors.Is(err, ErrNotFound) {
		t.Fatalf("merge of a source that does not hold the wire ID: err = %v, want ErrNotFound", err)
	}
}

// Migration 0011 leaves every existing row announced in epoch 0 with its
// binding in epoch 0, so no existing thread becomes re-keyable by a
// same-roster twin on upgrade.
func TestRemoteIDSpaceMigrationDefaultsExistingV10Rows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.sqlite3")
	database, err := sql.Open("sqlite", storeDSN(path))
	if err != nil {
		t.Fatalf("sql.Open(): %v", err)
	}
	ctx := context.Background()
	if err := enableWAL(ctx, database); err != nil {
		_ = database.Close()
		t.Fatalf("enableWAL(): %v", err)
	}
	if err := verifyConnectionPragmas(ctx, database); err != nil {
		_ = database.Close()
		t.Fatalf("verifyConnectionPragmas(): %v", err)
	}
	if err := runMigrations(ctx, database, embeddedMigrations[:10]); err != nil {
		_ = database.Close()
		t.Fatalf("runMigrations(v10): %v", err)
	}
	seedRebindFixture(t, database)
	before := readLedgerRows(t, database)
	if err := database.Close(); err != nil {
		t.Fatalf("close v10 database: %v", err)
	}

	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open(v10): %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close(): %v", err)
		}
	})
	after := readLedgerRows(t, store.db)
	if len(after) != 11 || !slices.Equal(after[:10], before) {
		t.Fatalf("ledger after upgrade = %+v, want 0001-0010 unchanged plus 0011", after)
	}
	if after[10].name != "remote_idspace_epochs" {
		t.Fatalf("migration 0011 name = %q", after[10].name)
	}
	assertPragmaInt(t, store.db, "user_version", 11)

	if epoch, err := store.RemoteIDSpaceEpoch("google"); err != nil || epoch != 0 {
		t.Fatalf("account epoch after upgrade = %d, %v; want 0", epoch, err)
	}
	for _, id := range []string{"karl-sms", "karl-rcs", "karl-old", "family", "book-club", "trio"} {
		binding, err := store.ConversationRemoteBinding("google", id)
		if err != nil || binding != (RemoteBinding{}) {
			t.Fatalf("%s after upgrade: %+v (err %v), want announced and bound in epoch 0", id, binding, err)
		}
	}
	expectExecError(t, store.db, "negative account epoch",
		`UPDATE accounts SET remote_idspace_epoch = -1 WHERE account_id = 'google'`)
	expectExecError(t, store.db, "negative announced epoch",
		`UPDATE conversations SET remote_announced_epoch = -1 WHERE conversation_id = 'family'`)
	expectExecError(t, store.db, "negative bound epoch",
		`UPDATE conversations SET remote_bound_epoch = -1 WHERE conversation_id = 'family'`)
}
