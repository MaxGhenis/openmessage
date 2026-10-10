package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

const substringSearchChecksum = "b2c0f767402191cae6ebb3aec846acea5dccaff2a87898bd522b1210db710f74"

// closeTestStore closes store when the test ends, after checking that every
// write the test made left each trigram index equal to its table. The package's
// store helpers use it, so every repository test also tests the triggers that
// maintain the indexes.
func closeTestStore(t *testing.T, store *Store) {
	t.Helper()
	t.Cleanup(func() {
		if err := store.VerifySearchIndexes(context.Background()); err != nil &&
			!strings.Contains(err.Error(), "database is closed") {
			t.Errorf("search indexes at the end of the test: %v", err)
		}
		if err := store.Close(); err != nil {
			t.Errorf("Close(): %v", err)
		}
	})
}

// substringSearchMigration returns the position of the substring_search
// migration, found by name so the tests survive a renumbering when parallel
// migrations land first.
func substringSearchMigration(t *testing.T) int {
	t.Helper()
	for i, migration := range embeddedMigrations {
		if migration.name == "substring_search" {
			return i
		}
	}
	t.Fatal("embedded migrations have no substring_search")
	return -1
}

// Invariant: likePatternUsesTrigrams(p) is true exactly when FTS5 answers
// LIKE p from the trigram postings rather than by visiting every row. The
// probe makes the difference observable: the indexed column holds one row the
// index has no postings for, so the row comes back only from a full visit.
func TestLikePatternUsesTrigramsMatchesFTS5(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "probe.sqlite3"))
	if err != nil {
		t.Fatalf("sql.Open(): %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	for _, statement := range []string{
		`CREATE TABLE content (body TEXT NOT NULL) STRICT`,
		`CREATE VIRTUAL TABLE probe USING fts5(body, content = 'content', content_rowid = 'rowid',
			tokenize = 'trigram case_sensitive 0', detail = none, columnsize = 0)`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	usesIndex := func(pattern string) bool {
		t.Helper()
		if _, err := db.Exec(`DELETE FROM content`); err != nil {
			t.Fatal(err)
		}
		// A body the pattern matches, written to the content table only.
		// Wildcards become ASCII letters, so no two bytes of the pattern join
		// into a different UTF-8 sequence in the body.
		body := strings.NewReplacer("%", "z", "_", "x").Replace(pattern)
		if i := strings.IndexByte(body, 0); i >= 0 {
			body = body[:i]
		}
		if _, err := db.Exec(`INSERT INTO content (rowid, body) VALUES (1, ?)`, body); err != nil {
			t.Fatal(err)
		}
		var matched bool
		var like int
		if err := db.QueryRow(`SELECT ? LIKE ?`, body, pattern).Scan(&like); err != nil {
			t.Fatal(err)
		}
		if like != 1 {
			t.Fatalf("probe body %q does not match %q", body, pattern)
		}
		if err := db.QueryRow(`SELECT EXISTS (SELECT 1 FROM probe WHERE body LIKE ?)`, pattern).Scan(&matched); err != nil {
			t.Fatalf("probe %q: %v", pattern, err)
		}
		return !matched
	}

	fixed := []string{
		"", "a", "ab", "abc", "%ab%", "%abc%", "ab%c", "a_bc", "abc_", "_abc",
		"日本", "日本語", "😀😀", "😀😀😀", "e\u0301e", "e\u0301", "a\x00bc", "abc\x00",
		"\x80\x80\x80", "a\x80bc", "\xffab", "\xff\xfe\xfd", `a\bc`, `%"%"%`,
	}
	for _, pattern := range fixed {
		if got, want := likePatternUsesTrigrams(pattern), usesIndex(pattern); got != want {
			t.Errorf("likePatternUsesTrigrams(%q) = %v, FTS5 used the index: %v", pattern, got, want)
		}
	}
	r := rand.New(rand.NewSource(7))
	pieces := []string{"a", "B", "%", "_", "é", "日", "😀", "\x00", "\x80", "\xc3", "\xff", " ", `\`, `"`}
	for range 3000 {
		var b strings.Builder
		for range r.Intn(9) {
			b.WriteString(pieces[r.Intn(len(pieces))])
		}
		pattern := b.String()
		if got, want := likePatternUsesTrigrams(pattern), usesIndex(pattern); got != want {
			t.Fatalf("likePatternUsesTrigrams(%q) = %v, FTS5 used the index: %v", pattern, got, want)
		}
	}
}

func TestTrigramCandidatePatternMatchesEveryLiteralMatch(t *testing.T) {
	store := openRepositoryTestStore(t)
	r := rand.New(rand.NewSource(11))
	pieces := []string{"a", "A", "%", "_", `\`, "é", "É", "x"}
	random := func(n int) string {
		var b strings.Builder
		for range n {
			b.WriteString(pieces[r.Intn(len(pieces))])
		}
		return b.String()
	}
	for range 2000 {
		text, value := random(1+r.Intn(4)), random(r.Intn(8))
		var exact, candidate int
		if err := store.db.QueryRow(`SELECT ? LIKE ? ESCAPE '\', ? LIKE ?`,
			value, "%"+escapeLikePattern(text)+"%",
			value, "%"+trigramCandidatePattern(text)+"%",
		).Scan(&exact, &candidate); err != nil {
			t.Fatal(err)
		}
		if exact == 1 && candidate != 1 {
			t.Fatalf("%q contains %q literally, but the candidate pattern %q misses it", value, text, trigramCandidatePattern(text))
		}
	}
}

func TestSubstringSearchMigrationAppliesToBlankAndExistingDatabase(t *testing.T) {
	position := substringSearchMigration(t)
	if got := embeddedMigrations[position].checksumSHA256; got != substringSearchChecksum {
		t.Fatalf("substring_search checksum = %s, want pinned %s (an applied migration must never change)", got, substringSearchChecksum)
	}

	t.Run("blank", func(t *testing.T) {
		store := openRepositoryTestStore(t)
		assertSubstringSearchSchema(t, store.db)
	})

	t.Run("existing store with rows", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "store.sqlite3")
		database, err := sql.Open("sqlite", storeDSN(path))
		if err != nil {
			t.Fatalf("sql.Open(): %v", err)
		}
		if err := enableWAL(context.Background(), database); err != nil {
			_ = database.Close()
			t.Fatalf("enableWAL(): %v", err)
		}
		if err := runMigrations(context.Background(), database, embeddedMigrations[:position]); err != nil {
			_ = database.Close()
			t.Fatalf("runMigrations(before substring_search): %v", err)
		}
		before := &Store{db: database}
		seed := seedReadStoreInto(t, before, 4243)
		if len(seed.messages) == 0 {
			_ = database.Close()
			t.Fatal("seed wrote no messages; the migration would index nothing")
		}
		// Bodies, titles and names the indexes must find after the rebuild.
		mustExec(t, database, `UPDATE messages SET body = 'Ünïcode ' || message_id || ' 日本語 trail' WHERE rowid % 3 = 0`)
		mustExec(t, database, `UPDATE conversations SET title = 'Group ' || conversation_id`)
		mustExec(t, database, `UPDATE conversation_participants SET display_name = 'Member ' || identity_id`)
		mustExec(t, database, `UPDATE identities SET display_name = 'Person ' || identity_id`)
		ledgerBefore := readLedgerRows(t, database)
		rowsBefore := substringSearchFingerprint(t, database)
		if err := database.Close(); err != nil {
			t.Fatalf("close pre-migration database: %v", err)
		}

		store, err := Open(path)
		if err != nil {
			t.Fatalf("Open(): %v", err)
		}
		ledgerAfter := readLedgerRows(t, store.db)
		if len(ledgerAfter) != len(embeddedMigrations) {
			t.Fatalf("migrated ledger rows = %d, want %d", len(ledgerAfter), len(embeddedMigrations))
		}
		if !slices.Equal(ledgerAfter[:position], ledgerBefore) {
			t.Fatalf("earlier ledger rows changed:\nbefore: %+v\nafter:  %+v", ledgerBefore, ledgerAfter[:position])
		}
		assertSubstringSearchSchema(t, store.db)
		if got := substringSearchFingerprint(t, store.db); !reflect.DeepEqual(got, rowsBefore) {
			t.Fatalf("migration changed rows:\nbefore: %v\nafter:  %v", rowsBefore, got)
		}
		if err := store.VerifySearchIndexes(context.Background()); err != nil {
			t.Fatalf("VerifySearchIndexes() after the rebuild: %v", err)
		}
		// The rebuilt index answers searches exactly as LIKE does.
		for _, term := range []string{"ünïcode", "日本語", "trail", "message-", "absent-term", "e T"} {
			assertMessageSearchMatchesLike(t, store.db, term, SearchQuery{Limit: 500})
		}
		for _, term := range []string{"group", "member identity", "PERSON", "+1555", "uuid-", "nobody"} {
			assertConversationSearchMatchesLike(t, store.db, term, 500)
		}
		var integrity string
		if err := store.db.QueryRow(`PRAGMA integrity_check`).Scan(&integrity); err != nil || integrity != "ok" {
			t.Fatalf("integrity_check = %q, %v; want ok", integrity, err)
		}
		if err := store.Close(); err != nil {
			t.Fatalf("Close(): %v", err)
		}

		reopened, err := Open(path)
		if err != nil {
			t.Fatalf("reopen: %v", err)
		}
		closeTestStore(t, reopened)
		if got := readLedgerRows(t, reopened.db); !slices.Equal(got, ledgerAfter) {
			t.Fatalf("reopen changed the ledger:\nbefore: %+v\nafter:  %+v", ledgerAfter, got)
		}
	})
}

// assertSubstringSearchSchema checks the ledger row, the four FTS5 tables with
// their options, their shadow tables and the twelve maintenance triggers.
func assertSubstringSearchSchema(t *testing.T, db *sql.DB) {
	t.Helper()
	position := substringSearchMigration(t)
	ledger := readLedgerRow(t, db, position+1)
	if ledger.name != "substring_search" || ledger.checksum != substringSearchChecksum {
		t.Fatalf("ledger row %d = %+v, want substring_search with the pinned checksum", position+1, ledger)
	}
	wantContent := map[string]string{
		"messages_fts":                  "messages",
		"conversations_fts":             "conversations",
		"conversation_participants_fts": "conversation_participants",
		"identities_fts":                "identities",
	}
	for _, index := range searchIndexes {
		var statement string
		if err := db.QueryRow(`SELECT sql FROM sqlite_schema WHERE type = 'table' AND name = ?`, index).Scan(&statement); err != nil {
			t.Fatalf("read %s definition: %v", index, err)
		}
		for _, option := range []string{
			"USING fts5(", "content = '" + wantContent[index] + "'", "content_rowid = 'rowid'",
			"tokenize = 'trigram case_sensitive 0'", "detail = none", "columnsize = 0",
		} {
			if !strings.Contains(statement, option) {
				t.Errorf("%s definition lacks %q:\n%s", index, option, statement)
			}
		}
		var tableType string
		if err := db.QueryRow(`SELECT type FROM pragma_table_list WHERE schema = 'main' AND name = ?`, index).Scan(&tableType); err != nil || tableType != "virtual" {
			t.Errorf("%s pragma_table_list type = %q, %v; want virtual", index, tableType, err)
		}
		for _, shadow := range []string{"_config", "_data", "_idx"} {
			if err := db.QueryRow(`SELECT type FROM pragma_table_list WHERE schema = 'main' AND name = ?`, index+shadow).Scan(&tableType); err != nil || tableType != "shadow" {
				t.Errorf("%s%s pragma_table_list type = %q, %v; want shadow", index, shadow, tableType, err)
			}
		}
		for _, absent := range []string{"_content", "_docsize"} {
			var exists bool
			if err := db.QueryRow(`SELECT EXISTS (SELECT 1 FROM sqlite_schema WHERE name = ?)`, index+absent).Scan(&exists); err != nil || exists {
				t.Errorf("%s%s exists = %v, %v; external content and columnsize=0 store neither", index, absent, exists, err)
			}
		}
		for _, event := range []string{"insert", "delete", "update"} {
			trigger := index + "_after_" + event
			var table string
			if err := db.QueryRow(`SELECT tbl_name FROM sqlite_schema WHERE type = 'trigger' AND name = ?`, trigger).Scan(&table); err != nil || table != wantContent[index] {
				t.Errorf("trigger %s on %q, %v; want on %s", trigger, table, err, wantContent[index])
			}
		}
	}
	var triggers int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_schema WHERE type = 'trigger'`).Scan(&triggers); err != nil || triggers != 12 {
		t.Errorf("triggers = %d, %v; want the 12 index triggers", triggers, err)
	}
}

// substringSearchFingerprint lists every indexed row, so the test can show the
// migration only adds indexes.
func substringSearchFingerprint(t *testing.T, db *sql.DB) []string {
	t.Helper()
	var out []string
	for _, query := range []string{
		`SELECT rowid || '|' || message_id || '|' || body || '|' || updated_at_ms FROM messages ORDER BY rowid`,
		`SELECT rowid || '|' || conversation_id || '|' || title || '|' || updated_at_ms FROM conversations ORDER BY rowid`,
		`SELECT rowid || '|' || conversation_id || '|' || identity_id || '|' || display_name FROM conversation_participants ORDER BY rowid`,
		`SELECT rowid || '|' || identity_id || '|' || display_name || '|' || canonical_value FROM identities ORDER BY rowid`,
	} {
		rows, err := db.Query(query)
		if err != nil {
			t.Fatalf("fingerprint: %v", err)
		}
		for rows.Next() {
			var row string
			if err := rows.Scan(&row); err != nil {
				t.Fatalf("scan fingerprint: %v", err)
			}
			out = append(out, row)
		}
		rows.Close()
	}
	return out
}

// assertMessageSearchMatchesLike runs SearchMessages' whole-scope statement for
// term and filter and the plain LIKE statement, requires the same rows in the
// same order, and returns their IDs.
func assertMessageSearchMatchesLike(t *testing.T, db *sql.DB, term string, filter SearchQuery) []string {
	t.Helper()
	gotSQL, gotArgs := searchMessagesStatement(term, filter)
	likeSQL, likeArgs := likeSearchMessagesStatement(term, filter)
	got := queryMessages(t, db, gotSQL, gotArgs...)
	want := queryMessages(t, db, likeSQL, likeArgs...)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("search %q %+v = %v, LIKE = %v", term, filter, messageIDs(got), messageIDs(want))
	}
	return messageIDs(got)
}

// assertConversationSearchMatchesLike is assertMessageSearchMatchesLike for
// SearchConversationsByName.
func assertConversationSearchMatchesLike(t *testing.T, db *sql.DB, term string, limit int) []string {
	t.Helper()
	gotSQL, gotArgs := searchConversationsByNameStatement(term, limit)
	exact := "%" + escapeLikePattern(term) + "%"
	got := queryConversations(t, db, gotSQL, gotArgs...)
	want := queryConversations(t, db, likeConversationsByNameQuery, exact, exact, exact, exact, limit)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("conversation search %q = %v, LIKE = %v", term, conversationIDs(got), conversationIDs(want))
	}
	return conversationIDs(got)
}

// TestSearchIndexesFollowRepositoryWrites walks one message and its
// conversation through every projection write that changes or removes rows
// (projection, re-projection with a new body, import, historical insert, edit,
// delete, repair move, repair delete, roster and title changes, and a
// conversation drop) and checks after each that the indexes equal their tables
// and that searching finds exactly what LIKE finds.
func TestSearchIndexesFollowRepositoryWrites(t *testing.T) {
	ctx := context.Background()
	clock := newMessageTestClock(messageTestTimeMS)
	store, repository := openMessageTestRepository(t, clock.Now)
	seedMessageProjectionGraph(t, store)
	seedMessageConversation(t, store, "conversation-b", "account-a")
	check := func(step string, term string, want []string) {
		t.Helper()
		if err := store.VerifySearchIndexes(ctx); err != nil {
			t.Fatalf("%s: VerifySearchIndexes(): %v", step, err)
		}
		got := assertMessageSearchMatchesLike(t, store.db, term, SearchQuery{Limit: 50})
		if !slices.Equal(got, want) {
			t.Fatalf("%s: search %q = %v, want %v", step, term, got, want)
		}
	}
	inbox := 0
	project := func(message Message) {
		t.Helper()
		inbox++
		record := messageTestInbox(fmt.Sprintf("inbox-%d", inbox), "account-a", fmt.Sprintf("frame-%d", inbox), []byte("frame"))
		if _, err := repository.AppendInbox(ctx, record); err != nil {
			t.Fatalf("AppendInbox(): %v", err)
		}
		if err := repository.ProjectMessage(ctx, MessageProjection{InboxID: record.InboxID, Message: message}); err != nil {
			t.Fatalf("ProjectMessage(): %v", err)
		}
	}

	message := messageTestMessage("message-a", "conversation-a", "account-a", "remote-a", pointer("identity-a"))
	message.Body = "lunch at the quayside"
	project(message)
	check("project", "quayside", []string{"message-a"})

	message.Body = "dinner at the harbour"
	project(message)
	check("re-project with a new body: old text gone", "quayside", nil)
	check("re-project with a new body: new text found", "harbour", []string{"message-a"})

	imported := messageTestMessage("message-b", "conversation-a", "account-a", "remote-b", nil)
	imported.Body = "imported harbour photo"
	imported.OccurredAtMS = messageTestTimeMS - 2_000
	if err := repository.ImportMessage(ctx, MessageProjection{Message: imported}); err != nil {
		t.Fatalf("ImportMessage(): %v", err)
	}
	check("import", "harbour", []string{"message-a", "message-b"})

	historical := messageTestMessage("message-c", "conversation-b", "account-a", "remote-c", nil)
	historical.Body = "fetched harbour history"
	historical.OccurredAtMS = messageTestTimeMS - 3_000
	if inserted, err := repository.InsertHistoricalMessage(ctx, MessageProjection{Message: historical}); err != nil || !inserted {
		t.Fatalf("InsertHistoricalMessage() = %v, %v", inserted, err)
	}
	check("historical insert", "harbour", []string{"message-a", "message-b", "message-c"})

	mutate := func(kind, body string) {
		t.Helper()
		inbox++
		record := messageTestInbox(fmt.Sprintf("inbox-%d", inbox), "account-a", fmt.Sprintf("frame-%d", inbox), []byte(kind))
		if _, err := repository.AppendInbox(ctx, record); err != nil {
			t.Fatalf("AppendInbox(): %v", err)
		}
		if updated, err := repository.ApplyMessageMutation(ctx, "account-a", "conversation-a", "remote-a", kind, body, record.InboxID); err != nil || !updated {
			t.Fatalf("ApplyMessageMutation(%s) = %v, %v", kind, updated, err)
		}
	}
	mutate("edit", "dinner at the marina")
	check("edit: old text gone", "dinner at the harbour", nil)
	check("edit: new text found", "marina", []string{"message-a"})
	mutate("delete", "")
	// A delete mutation keeps the body (state 'deleted'), so the message stays
	// searchable exactly as LIKE over messages finds it.
	check("delete mutation", "marina", []string{"message-a"})

	if err := store.ApplyRepairPlan(ctx, "account-a", []RepairStep{
		{Op: "move", MessageID: "message-b", TargetConversationID: "conversation-b"},
	}, messageTestTimeMS+10); err != nil {
		t.Fatalf("ApplyRepairPlan(move): %v", err)
	}
	check("repair move", "harbour", []string{"message-b", "message-c"})
	got := assertMessageSearchMatchesLike(t, store.db, "harbour", SearchQuery{ConversationID: "conversation-b", Limit: 50})
	if !slices.Equal(got, []string{"message-b", "message-c"}) {
		t.Fatalf("search in the target conversation after the move = %v", got)
	}

	if err := store.ApplyRepairPlan(ctx, "account-a", []RepairStep{
		{Op: "delete", MessageID: "message-c"},
	}, messageTestTimeMS+20); err != nil {
		t.Fatalf("ApplyRepairPlan(delete): %v", err)
	}
	check("repair delete", "harbour", []string{"message-b"})

	if err := store.ApplyRepairPlan(ctx, "account-a", []RepairStep{
		{Op: "meta", ConversationID: "conversation-b", Kind: "group", Title: "Harbour crew",
			Participants: []RepairParticipant{{IdentityID: "identity-a", DisplayName: "Quartermaster", IsActive: true}}},
	}, messageTestTimeMS+30); err != nil {
		t.Fatalf("ApplyRepairPlan(meta): %v", err)
	}
	if err := store.VerifySearchIndexes(ctx); err != nil {
		t.Fatalf("meta: VerifySearchIndexes(): %v", err)
	}
	if got := assertConversationSearchMatchesLike(t, store.db, "harbour", 10); !slices.Equal(got, []string{"conversation-b"}) {
		t.Fatalf("title search after meta = %v", got)
	}
	if got := assertConversationSearchMatchesLike(t, store.db, "quarter", 10); !slices.Equal(got, []string{"conversation-b"}) {
		t.Fatalf("participant search after meta = %v", got)
	}

	// A conversation delete cascades to its messages and roster.
	mustExec(t, store.db, `DELETE FROM messages WHERE conversation_id = 'conversation-b'`)
	if err := store.ApplyRepairPlan(ctx, "account-a", []RepairStep{
		{Op: "drop", ConversationID: "conversation-b"},
	}, messageTestTimeMS+40); err != nil {
		t.Fatalf("ApplyRepairPlan(drop): %v", err)
	}
	check("drop", "harbour", nil)
	if got := assertConversationSearchMatchesLike(t, store.db, "quarter", 10); len(got) != 0 {
		t.Fatalf("participant search after drop = %v", got)
	}
	mustExec(t, store.db, `DELETE FROM conversations WHERE conversation_id = 'conversation-a'`)
	check("cascade from a conversation delete", "marina", nil)

	identity := repositoryTestIdentity("identity-a", "account-a", "identity-a")
	identity.DisplayName = "Harbour Master"
	identity.UpdatedAtMS = repositoryTestTimeMS + 1
	mustRepositoryWrite(t, "UpsertIdentity", store.UpsertIdentity(identity))
	if err := store.VerifySearchIndexes(ctx); err != nil {
		t.Fatalf("identity rename: VerifySearchIndexes(): %v", err)
	}
}

// Invariant: the indexes survive the copies the app makes. Backups snapshot the
// store with VACUUM INTO and a VACUUM rewrites it in place; both keep the
// rowids the indexes are keyed by, so the indexes still equal their tables
// and searches still match LIKE.
func TestSearchIndexesSurviveVacuumAndVacuumInto(t *testing.T) {
	store := openRepositoryTestStore(t)
	seedReadStoreInto(t, store, 99)
	// Leave holes in the rowid sequence and rows written after them.
	mustExec(t, store.db, `DELETE FROM messages WHERE rowid % 2 = 0`)
	mustExec(t, store.db, `UPDATE messages SET body = body || ' later ' || message_id`)
	before := substringSearchFingerprint(t, store.db)

	copyPath := filepath.Join(t.TempDir(), "backup.sqlite3")
	mustExec(t, store.db, `VACUUM INTO ?`, copyPath)
	copied, err := Open(copyPath)
	if err != nil {
		t.Fatalf("Open(copy): %v", err)
	}
	closeTestStore(t, copied)

	mustExec(t, store.db, `VACUUM`)
	for name, db := range map[string]*sql.DB{"vacuumed": store.db, "VACUUM INTO copy": copied.db} {
		if got := substringSearchFingerprint(t, db); !reflect.DeepEqual(got, before) {
			t.Fatalf("%s: rowids or rows changed:\nbefore: %v\nafter:  %v", name, before, got)
		}
		if err := (&Store{db: db}).VerifySearchIndexes(context.Background()); err != nil {
			t.Fatalf("%s: VerifySearchIndexes(): %v", name, err)
		}
		for _, term := range []string{"hello", "later message-", "plan", "absent"} {
			assertMessageSearchMatchesLike(t, db, term, SearchQuery{Limit: 100})
		}
	}
}

// TestVerifySearchIndexesReportsDrift shows the check catches an index that no
// longer matches its table, in both directions.
func TestVerifySearchIndexesReportsDrift(t *testing.T) {
	ctx := context.Background()
	t.Run("posting without a row", func(t *testing.T) {
		store, _ := openMessageTestRepository(t, func() time.Time { return time.UnixMilli(messageTestTimeMS) })
		seedReadStoreInto(t, store, 5)
		mustExec(t, store.db, `INSERT INTO messages_fts (rowid, body) VALUES (987654, 'phantom text')`)
		if err := store.VerifySearchIndexes(ctx); !errors.Is(err, ErrSearchIndexMismatch) {
			t.Fatalf("VerifySearchIndexes() = %v, want ErrSearchIndexMismatch", err)
		}
		mustExec(t, store.db, `INSERT INTO messages_fts (messages_fts) VALUES ('rebuild')`)
	})
	t.Run("row without postings", func(t *testing.T) {
		store := openRepositoryTestStore(t)
		seedReadStoreInto(t, store, 6)
		mustExec(t, store.db, `DROP TRIGGER identities_fts_after_update`)
		mustExec(t, store.db, `UPDATE identities SET display_name = 'renamed without the trigger'`)
		if err := store.VerifySearchIndexes(ctx); !errors.Is(err, ErrSearchIndexMismatch) {
			t.Fatalf("VerifySearchIndexes() = %v, want ErrSearchIndexMismatch", err)
		}
		mustExec(t, store.db, `INSERT INTO identities_fts (identities_fts) VALUES ('rebuild')`)
		if err := store.VerifySearchIndexes(ctx); err != nil {
			t.Fatalf("VerifySearchIndexes() after rebuild: %v", err)
		}
	})
}

// TestSubstringSearchPlansReadTheTrigramIndexes pins the plans of the search
// statements on a store without statistics, which is what production plans
// with. Without a conversation or sender filter, the whole-scope message
// search reads messages_fts first and joins each candidate by rowid, and the
// recent window walks messages_time_idx from its newest end; with one, the
// LIKE reads that index's range. The conversation search answers each of its
// four columns from a trigram index.
func TestSubstringSearchPlansReadTheTrigramIndexes(t *testing.T) {
	store := openRepositoryTestStore(t)
	db := store.db
	filters := []struct {
		name   string
		filter SearchQuery
		// recent is the plan of the recent-window read, count that of its
		// row count.
		recent []string
		count  []string
	}{
		{
			name:   "no filter",
			filter: SearchQuery{Limit: 30},
			recent: []string{"CO-ROUTINE (subquery-1)", "SCAN m USING INDEX messages_time_idx", "SCAN (subquery-1)"},
			count:  []string{"CO-ROUTINE (subquery-1)", "SCAN m USING COVERING INDEX messages_time_idx", "SCAN (subquery-1)"},
		},
		{
			// The account applies to the window's rows, never inside it.
			name:   "account",
			filter: SearchQuery{AccountID: "account-a", Limit: 30},
			recent: []string{"CO-ROUTINE (subquery-1)", "SCAN m USING INDEX messages_time_idx", "SCAN (subquery-1)"},
			count:  []string{"CO-ROUTINE (subquery-1)", "SCAN m USING COVERING INDEX messages_time_idx", "SCAN (subquery-1)"},
		},
		{
			name:   "date window",
			filter: SearchQuery{SinceMS: 1, UntilMS: 2, Limit: 30},
			recent: []string{"CO-ROUTINE (subquery-1)", "SEARCH m USING INDEX messages_time_idx (occurred_at_ms>? AND occurred_at_ms<?)", "SCAN (subquery-1)"},
			count:  []string{"CO-ROUTINE (subquery-1)", "SEARCH m USING COVERING INDEX messages_time_idx (occurred_at_ms>? AND occurred_at_ms<?)", "SCAN (subquery-1)"},
		},
		{
			name:   "account in a date window",
			filter: SearchQuery{AccountID: "account-a", SinceMS: 1, UntilMS: 2, Limit: 30},
			recent: []string{"CO-ROUTINE (subquery-1)", "SEARCH m USING INDEX messages_time_idx (occurred_at_ms>? AND occurred_at_ms<?)", "SCAN (subquery-1)"},
			count:  []string{"CO-ROUTINE (subquery-1)", "SEARCH m USING COVERING INDEX messages_time_idx (occurred_at_ms>? AND occurred_at_ms<?)", "SCAN (subquery-1)"},
		},
	}
	for _, tc := range filters {
		t.Run(tc.name, func(t *testing.T) {
			if indexBoundedSearch(tc.filter) {
				t.Fatal("these filters must take the window and the trigram index")
			}
			query, args := searchMessagesStatement("term", tc.filter)
			plan := explainPlanLines(t, db, query, args...)
			if len(plan) < 2 || plan[0] != "SCAN f VIRTUAL TABLE INDEX 0:L0" || plan[1] != "SEARCH m USING INTEGER PRIMARY KEY (rowid=?)" {
				t.Fatalf("whole-scope plan = %q, want messages_fts as the outer loop, then messages by rowid", plan)
			}
			if scans := planScans(plan, "m"); len(scans) > 0 {
				t.Fatalf("whole-scope plan = %q reads messages whole", plan)
			}
			query, args = recentSearchMessagesStatement("term", tc.filter, recentSearchWindow)
			if plan := explainPlanLines(t, db, query, args...); !slices.Equal(plan, tc.recent) {
				t.Fatalf("recent-window plan = %q, want %q", plan, tc.recent)
			}
			query, args = recentWindowCountStatement(tc.filter, recentSearchWindow)
			if plan := explainPlanLines(t, db, query, args...); !slices.Equal(plan, tc.count) {
				t.Fatalf("window-count plan = %q, want %q", plan, tc.count)
			}
		})
	}

	// A conversation or sender filter keeps the LIKE over that index's range:
	// the conversation's read newest first and stopped at limit matches, the
	// sender's identities each seeked.
	for _, tc := range []struct {
		name   string
		filter SearchQuery
		want   []string
	}{
		{"conversation", SearchQuery{ConversationID: "conversation-a", Limit: 30},
			[]string{"SEARCH m USING INDEX messages_conversation_time_idx (conversation_id=?)"}},
		{"conversation in a date window", SearchQuery{ConversationID: "conversation-a", SinceMS: 1, UntilMS: 2, Limit: 30},
			[]string{"SEARCH m USING INDEX messages_conversation_time_idx (conversation_id=? AND occurred_at_ms>? AND occurred_at_ms<?)"}},
		{"sender", SearchQuery{SenderCanonicalValue: "+15550000001", Limit: 30},
			[]string{"SEARCH m USING INDEX messages_sender_time_idx (sender_identity_id=?)", "LIST SUBQUERY 1", "SCAN identities", "CREATE BLOOM FILTER", "USE TEMP B-TREE FOR ORDER BY"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !indexBoundedSearch(tc.filter) {
				t.Fatal("a conversation or sender filter must keep the LIKE over its index")
			}
			query, args := searchMessagesStatement("term", tc.filter)
			if plan := explainPlanLines(t, db, query, args...); !slices.Equal(plan, tc.want) {
				t.Fatalf("plan = %q, want %q", plan, tc.want)
			}
		})
	}

	t.Run("conversation names", func(t *testing.T) {
		query, args := searchConversationsByNameStatement("term", 30)
		plan := explainPlanLines(t, db, query, args...)
		var indexReads []string
		for _, line := range plan {
			if strings.HasPrefix(line, "SCAN f VIRTUAL TABLE INDEX ") {
				indexReads = append(indexReads, line)
			}
		}
		want := []string{
			"SCAN f VIRTUAL TABLE INDEX 0:L0", // conversations_fts.title
			"SCAN f VIRTUAL TABLE INDEX 0:L0", // conversation_participants_fts.display_name
			"SCAN f VIRTUAL TABLE INDEX 0:L0", // identities_fts.display_name
			"SCAN f VIRTUAL TABLE INDEX 0:L1", // identities_fts.canonical_value
		}
		if !slices.Equal(indexReads, want) {
			t.Fatalf("plan = %q\nreads the trigram indexes as %q, want %q", plan, indexReads, want)
		}
		for _, table := range []string{"conversations", "c", "cp", "i", "identities", "conversation_participants"} {
			if scans := planScans(plan, table); len(scans) > 0 {
				t.Fatalf("plan = %q reads %s whole: %q", plan, table, scans)
			}
		}
	})
}

// TestSearchMessagesChoosesItsPath pins which path answers each kind of
// search, with the plan scaled down to a small store, and that each answer is
// the LIKE's.
func TestSearchMessagesChoosesItsPath(t *testing.T) {
	ctx := context.Background()
	store := openRepositoryTestStore(t)
	repository := mustMessageRepository(t, store, 100)
	db := store.db
	mustExec(t, db, `INSERT INTO accounts (account_id, bridge_key, created_at_ms, updated_at_ms) VALUES ('account-a', 'signal_cli', 1, 1)`)
	mustExec(t, db, `INSERT INTO identities (identity_id, account_id, kind, canonical_value, raw_value, created_at_ms, updated_at_ms) VALUES ('identity-a', 'account-a', 'phone', '+15550000001', '+15550000001', 1, 1)`)
	mustExec(t, db, `INSERT INTO conversations (conversation_id, account_id, remote_conversation_id, kind, created_at_ms, updated_at_ms) VALUES ('conversation-a', 'account-a', 'remote-a', 'direct', 1, 1)`)
	// 40 messages; "common" in every one, "rare" only in the oldest.
	for i := range 40 {
		body := "common words"
		if i == 0 {
			body = "a rare find, common words"
		}
		mustExec(t, db, `INSERT INTO messages (message_id, conversation_id, account_id, remote_message_id, sender_identity_id, direction, body, occurred_at_ms, created_at_ms, updated_at_ms)
			VALUES (?, 'conversation-a', 'account-a', ?, 'identity-a', 'incoming', ?, ?, 1, 1)`,
			fmt.Sprintf("message-%02d", i), fmt.Sprintf("remote-%d", i), body, 100+i)
	}

	window := func(n int) searchPlan { return searchPlan{window: n} }
	// scoped plans: a 20-message window; the index considered once the LIKE
	// has minRows rows left, and used below minRows/2 candidates.
	scoped := func(minRows, storeShare int) searchPlan {
		return searchPlan{window: 20, indexMinRows: minRows, indexStoreShare: storeShare}
	}
	conversation := func(limit int) SearchQuery { return SearchQuery{ConversationID: "conversation-a", Limit: limit} }
	sender := func(limit int) SearchQuery { return SearchQuery{SenderCanonicalValue: "+15550000001", Limit: limit} }
	for _, tc := range []struct {
		name   string
		query  string
		filter SearchQuery
		plan   searchPlan
		want   searchPath
	}{
		{"a common term is answered by the newest messages", "common", SearchQuery{Limit: 5}, window(20), searchPathRecentWindow},
		{"a window larger than the range reads all of it", "rare", SearchQuery{Limit: 5}, window(50), searchPathRecentWindow},
		{"a date range smaller than the window is read whole", "rare", SearchQuery{SinceMS: 100, UntilMS: 110, Limit: 5}, window(20), searchPathRecentWindow},
		{"a rare term reads the index", "rare", SearchQuery{Limit: 5}, window(20), searchPathTrigramIndex},
		{"a rare term in a wide date range reads the index", "rare", SearchQuery{SinceMS: 100, UntilMS: 200, Limit: 5}, window(20), searchPathTrigramIndex},
		{"a rare term in an account reads the index", "rare", SearchQuery{AccountID: "account-a", Limit: 5}, window(20), searchPathTrigramIndex},
		{"a short rare term scans", "ra", SearchQuery{Limit: 5}, window(20), searchPathLike},
		{"the empty query is a listing", "", SearchQuery{Limit: 5}, window(20), searchPathLike},

		{"a conversation shorter than the window runs the LIKE", "rare", conversation(5), defaultSearchPlan, searchPathLike},
		{"a short query in a conversation runs the LIKE", "ra", conversation(5), scoped(10, 0), searchPathLike},
		{"a term common in the thread is answered by its newest messages", "common", conversation(5), scoped(10, 0), searchPathConversationWindow},
		{"a thread is cut at the conversation window, not the cross-conversation one", "common", conversation(5),
			searchPlan{window: 50, conversationWindow: 20, indexMinRows: 10}, searchPathConversationWindow},
		{"a thread shorter than the conversation window runs the LIKE", "common", conversation(5),
			searchPlan{window: 20, conversationWindow: 50, indexMinRows: 10}, searchPathLike},
		{"a term rare in a long thread reads the index", "rare", conversation(5), scoped(10, 0), searchPathScopeIndex},
		{"a rare term reads the rest of a thread shorter than the minimum", "rare", conversation(5), scoped(21, 0), searchPathConversationRest},
		{"the minimum grows with the store", "rare", conversation(5), scoped(10, 1), searchPathConversationRest},
		{"a term the window finds often enough reads the rest", "common", conversation(30), scoped(10, 0), searchPathConversationRest},
		{"a rare term in a wide date range of a thread reads the index", "rare", SearchQuery{ConversationID: "conversation-a", SinceMS: 100, UntilMS: 200, Limit: 5}, scoped(10, 0), searchPathScopeIndex},
		{"a date range shorter than the window runs the LIKE", "rare", SearchQuery{ConversationID: "conversation-a", SinceMS: 100, UntilMS: 110, Limit: 5}, scoped(10, 0), searchPathLike},
		{"a rare term from a prolific sender reads the index", "rare", sender(5), scoped(10, 0), searchPathScopeIndex},
		{"a rare term from a sender below the minimum runs the LIKE", "rare", sender(5), scoped(41, 0), searchPathLike},
		{"a sender can be asked for several minimums", "rare", sender(5),
			searchPlan{window: 20, indexMinRows: 21, senderMinimums: 2}, searchPathLike},
		{"a sender with one minimum of 21 reads the index", "rare", sender(5),
			searchPlan{window: 20, indexMinRows: 21, senderMinimums: 1}, searchPathScopeIndex},
		{"an address no identity holds has no messages", "rare", SearchQuery{SenderCanonicalValue: "+19999999999", Limit: 5}, scoped(10, 0), searchPathLike},
		{"a term common for the sender runs the LIKE", "common", sender(5), scoped(10, 0), searchPathLike},
		{"a sender in a conversation is searched within the conversation", "rare", SearchQuery{ConversationID: "conversation-a", SenderCanonicalValue: "+15550000001", Limit: 5}, scoped(10, 0), searchPathScopeIndex},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, path, err := repository.searchMessages(ctx, tc.query, tc.filter, tc.plan)
			if err != nil {
				t.Fatalf("searchMessages(): %v", err)
			}
			if path != tc.want {
				t.Fatalf("path = %s, want %s", path, tc.want)
			}
			likeSQL, likeArgs := likeSearchMessagesStatement(tc.query, tc.filter)
			if want := queryMessages(t, db, likeSQL, likeArgs...); !reflect.DeepEqual(got, want) {
				t.Fatalf("rows = %v, LIKE = %v", messageIDs(got), messageIDs(want))
			}
		})
	}
}
