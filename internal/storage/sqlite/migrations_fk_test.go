package sqlite

// Tests for the migration runner's foreign-key handling. SQLite ignores
// PRAGMA foreign_keys inside a transaction, and with enforcement on, DROP
// TABLE runs an implicit DELETE that fires every child's ON DELETE action. A
// standard table rebuild of a parent (CREATE new, INSERT SELECT, DROP old,
// RENAME new) inside the migration transaction therefore cascade-deleted the
// parent's children, or failed on a NO ACTION child, and the post-migration
// foreign_key_check passed because the orphans were gone.

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// rebuildMessagesSQL rebuilds messages in SQLite's documented order
// (https://www.sqlite.org/lang_altertable.html#otheralter), adding a column.
const rebuildMessagesSQL = `
CREATE TABLE messages_rebuilt (
    message_id          TEXT PRIMARY KEY CHECK (trim(message_id) <> ''),
    conversation_id     TEXT NOT NULL,
    account_id          TEXT NOT NULL,
    remote_message_id   TEXT NOT NULL CHECK (trim(remote_message_id) <> ''),
    sender_identity_id  TEXT,
    direction           TEXT NOT NULL
                        CHECK (direction IN ('incoming', 'outgoing')),
    body                TEXT NOT NULL DEFAULT '',
    reply_to_remote_id  TEXT CHECK (
        reply_to_remote_id IS NULL OR trim(reply_to_remote_id) <> ''
    ),
    state               TEXT NOT NULL DEFAULT 'active'
                        CHECK (state IN ('active', 'edited', 'deleted')),
    occurred_at_ms      INTEGER NOT NULL CHECK (occurred_at_ms > 0),
    created_at_ms       INTEGER NOT NULL CHECK (created_at_ms > 0),
    updated_at_ms       INTEGER NOT NULL CHECK (updated_at_ms >= created_at_ms),
    rebuild_marker      TEXT NOT NULL DEFAULT 'rebuilt',
    FOREIGN KEY (account_id, conversation_id)
        REFERENCES conversations(account_id, conversation_id) ON DELETE CASCADE,
    FOREIGN KEY (account_id, sender_identity_id)
        REFERENCES identities(account_id, identity_id) ON DELETE RESTRICT,
    UNIQUE (account_id, conversation_id, remote_message_id)
) STRICT;

INSERT INTO messages_rebuilt (
    message_id, conversation_id, account_id, remote_message_id,
    sender_identity_id, direction, body, reply_to_remote_id, state,
    occurred_at_ms, created_at_ms, updated_at_ms
)
SELECT
    message_id, conversation_id, account_id, remote_message_id,
    sender_identity_id, direction, body, reply_to_remote_id, state,
    occurred_at_ms, created_at_ms, updated_at_ms
FROM messages;

DROP TABLE messages;

ALTER TABLE messages_rebuilt RENAME TO messages;

CREATE INDEX messages_conversation_time_idx
    ON messages(conversation_id, occurred_at_ms DESC, message_id DESC);

CREATE INDEX messages_sender_time_idx
    ON messages(sender_identity_id, occurred_at_ms DESC)
    WHERE sender_identity_id IS NOT NULL;

CREATE UNIQUE INDEX messages_conversation_message_uq
    ON messages(conversation_id, message_id);
`

// messageChildTables are every table with a foreign key to messages.
var messageChildTables = []string{
	"reactions",
	"reaction_snapshot_fences",
	"message_attachments",
	"read_cursors",
	"outbox_reactions",
	"outbox_read_receipts",
}

func TestMigrationTableRebuildOfParentKeepsChildRows(t *testing.T) {
	for _, withReferences := range []bool{false, true} {
		name := "cascade children only"
		if withReferences {
			name = "with NO ACTION references"
		}
		t.Run(name, func(t *testing.T) {
			store := openMigrationFKTestStore(t)
			seedEchoMergeGraph(t, store)
			const messageID = "message-rebuild-parent"
			seedOutboxTestMessage(t, store, messageID, "account-a", "conversation-a")
			seedEchoCascadeChildren(t, store, messageID)
			if withReferences {
				repository, err := NewOutboxRepository(store, func() time.Time { return time.UnixMilli(outboxTestTimeMS) })
				if err != nil {
					t.Fatalf("NewOutboxRepository(): %v", err)
				}
				seedEchoNoActionReferences(t, store, repository, messageID)
			}
			before := countRows(t, store.db, messageChildTables)
			indexesBefore := messagesIndexSQL(t, store.db)

			migrations := withTestMigration(newMigration(len(embeddedMigrations)+1, "rebuild_messages", rebuildMessagesSQL, nil))
			if err := runMigrations(context.Background(), store.db, migrations); err != nil {
				t.Fatalf("runMigrations(rebuild messages): %v", err)
			}

			after := countRows(t, store.db, messageChildTables)
			for _, table := range messageChildTables {
				if after[table] != before[table] {
					t.Errorf("%s rows after rebuild = %d, want %d", table, after[table], before[table])
				}
			}
			if before["reactions"] == 0 || before["message_attachments"] == 0 ||
				(withReferences && before["read_cursors"] == 0) {
				t.Fatalf("seed did not create the children under test: %v", before)
			}
			var marker string
			if err := store.db.QueryRow(`SELECT rebuild_marker FROM messages WHERE message_id = ?`, messageID).Scan(&marker); err != nil {
				t.Fatalf("read rebuilt column: %v", err)
			}
			if marker != "rebuilt" {
				t.Fatalf("rebuild_marker = %q, want rebuilt", marker)
			}
			if got := messagesIndexSQL(t, store.db); got != indexesBefore {
				t.Fatalf("messages indexes after rebuild =\n%s\nwant\n%s", got, indexesBefore)
			}
			assertRowCount(t, store.db, "schema_migrations", len(migrations))
			assertPragmaInt(t, store.db, "user_version", len(migrations))
			assertForeignKeyCheckClean(t, store.db)
			assertForeignKeysEnforced(t, store.db)
		})
	}
}

// openMigrationFKTestStore opens a migrated store whose pool holds a single
// connection, so the connection the migration runner pins is the one every
// later query in the test reuses unless the runner discarded it.
func openMigrationFKTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "store.sqlite3"))
	if err != nil {
		t.Fatalf("Open(): %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close(): %v", err)
		}
	})
	store.db.SetMaxOpenConns(1)
	seedMessageAccount(t, store, "account-a", "test")
	return store
}

func withTestMigration(extra ...migration) []migration {
	migrations := append([]migration(nil), embeddedMigrations...)
	return append(migrations, extra...)
}

func countRows(t *testing.T, db *sql.DB, tables []string) map[string]int {
	t.Helper()
	counts := make(map[string]int, len(tables))
	for _, table := range tables {
		var count int
		if err := db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil {
			t.Fatalf("count %s rows: %v", table, err)
		}
		counts[table] = count
	}
	return counts
}

func messagesIndexSQL(t *testing.T, db *sql.DB) string {
	t.Helper()
	rows, err := db.Query(`
		SELECT name, COALESCE(sql, '')
		FROM sqlite_schema
		WHERE type = 'index' AND tbl_name = 'messages'
		ORDER BY name
	`)
	if err != nil {
		t.Fatalf("list messages indexes: %v", err)
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var name, definition string
		if err := rows.Scan(&name, &definition); err != nil {
			t.Fatalf("scan messages index: %v", err)
		}
		if strings.HasPrefix(name, "sqlite_autoindex_") {
			// Autoindex names follow the table's creation name; compare their
			// presence, not the name.
			name = "sqlite_autoindex"
		}
		lines = append(lines, name+": "+strings.Join(strings.Fields(definition), " "))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate messages indexes: %v", err)
	}
	return strings.Join(lines, "\n")
}

// assertForeignKeysEnforced proves the pooled connection enforces foreign
// keys again: the pragma reads 1 and an orphan insert is rejected.
func assertForeignKeysEnforced(t *testing.T, db *sql.DB) {
	t.Helper()
	assertPragmaInt(t, db, "foreign_keys", 1)
	if _, err := db.Exec(`
		INSERT INTO reaction_snapshot_fences (message_id, source_seq_ms, updated_at_ms)
		VALUES ('message-that-does-not-exist', 1, 1)
	`); err == nil {
		t.Fatal("orphan reaction_snapshot_fences insert succeeded; foreign keys are not enforced")
	}
}

func TestMigrationRelyingOnDeleteCascadeFailsLoudly(t *testing.T) {
	store := openMigrationFKTestStore(t)
	seedEchoMergeGraph(t, store)
	seedOutboxTestMessage(t, store, "message-cascade", "account-a", "conversation-a")
	seedEchoCascadeChildren(t, store, "message-cascade")
	before := countRows(t, store.db, append([]string{"conversations", "messages"}, messageChildTables...))

	// With enforcement on this would cascade through messages to reactions,
	// fences and attachments. With it off nothing cascades, the orphans fail
	// foreign_key_check, and the migration rolls back.
	migrations := withTestMigration(newMigration(len(embeddedMigrations)+1, "delete_conversations", `
		DELETE FROM conversations;
	`, nil))
	err := runMigrations(context.Background(), store.db, migrations)
	if err == nil || !strings.Contains(err.Error(), "foreign_key_check failed") {
		t.Fatalf("runMigrations(delete parents) error = %v, want a foreign_key_check failure", err)
	}

	after := countRows(t, store.db, append([]string{"conversations", "messages"}, messageChildTables...))
	for table, want := range before {
		if after[table] != want {
			t.Errorf("%s rows after failed migration = %d, want %d", table, after[table], want)
		}
	}
	assertRowCount(t, store.db, "schema_migrations", len(embeddedMigrations))
	assertPragmaInt(t, store.db, "user_version", len(embeddedMigrations))
	assertForeignKeysEnforced(t, store.db)
}

func TestMigrationDroppingParentTableFails(t *testing.T) {
	t.Run("parent with no child rows", func(t *testing.T) {
		store := openMigrationFKTestStore(t)
		// people has no rows, so foreign_key_check alone would pass with
		// person_identities pointing at a missing table.
		migrations := withTestMigration(newMigration(len(embeddedMigrations)+1, "drop_people", `
			DROP TABLE people;
		`, nil))
		err := runMigrations(context.Background(), store.db, migrations)
		if err == nil || !strings.Contains(err.Error(), `references missing table "people"`) {
			t.Fatalf("runMigrations(drop parent) error = %v, want a missing-parent failure", err)
		}
		assertRowCount(t, store.db, "people", 0)
		assertRowCount(t, store.db, "schema_migrations", len(embeddedMigrations))
		assertForeignKeysEnforced(t, store.db)
	})

	t.Run("rebuild that renames the old table first", func(t *testing.T) {
		store := openMigrationFKTestStore(t)
		seedEchoMergeGraph(t, store)
		seedOutboxTestMessage(t, store, "message-wrong-order", "account-a", "conversation-a")
		seedEchoCascadeChildren(t, store, "message-wrong-order")
		before := countRows(t, store.db, messageChildTables)

		// RENAME rewrites every child's REFERENCES to the new name, so the
		// children follow messages_old and are left pointing at a dropped
		// table. SQLite's procedure warns against this order; the runner must
		// refuse it rather than commit dangling foreign keys.
		wrongOrder := strings.Replace(rebuildMessagesSQL, "DROP TABLE messages;\n\nALTER TABLE messages_rebuilt RENAME TO messages;", "", 1)
		wrongOrder = strings.Replace(wrongOrder, "CREATE TABLE messages_rebuilt (", "ALTER TABLE messages RENAME TO messages_old;\nCREATE TABLE messages_rebuilt (", 1)
		wrongOrder = strings.Replace(wrongOrder, "FROM messages;", "FROM messages_old;\nDROP TABLE messages_old;\nALTER TABLE messages_rebuilt RENAME TO messages;", 1)
		wrongOrder = strings.Replace(wrongOrder, "CREATE INDEX messages_conversation_time_idx", "DROP INDEX IF EXISTS messages_conversation_time_idx;\nCREATE INDEX messages_conversation_time_idx", 1)
		if strings.Count(wrongOrder, "RENAME TO") != 2 || !strings.Contains(wrongOrder, "DROP TABLE messages_old;") {
			t.Fatalf("wrong-order SQL was not assembled:\n%s", wrongOrder)
		}
		migrations := withTestMigration(newMigration(len(embeddedMigrations)+1, "rebuild_messages_wrong_order", wrongOrder, nil))
		err := runMigrations(context.Background(), store.db, migrations)
		if err == nil {
			t.Fatal("runMigrations(wrong-order rebuild) succeeded, want it refused")
		}
		t.Logf("wrong-order rebuild refused: %v", err)

		after := countRows(t, store.db, messageChildTables)
		for table, want := range before {
			if after[table] != want {
				t.Errorf("%s rows after refused migration = %d, want %d", table, after[table], want)
			}
		}
		assertRowCount(t, store.db, "schema_migrations", len(embeddedMigrations))
		assertForeignKeysEnforced(t, store.db)
	})
}

func TestWithForeignKeysOffNeverReturnsItsConnectionToThePool(t *testing.T) {
	cases := []struct {
		name string
		fn   func(context.Context, *sql.Conn) error
	}{
		{
			name: "fn succeeds leaving a TEMP table",
			fn: func(ctx context.Context, conn *sql.Conn) error {
				_, err := conn.ExecContext(ctx, `CREATE TEMP TABLE migration_scratch (x INTEGER)`)
				return err
			},
		},
		{
			name: "fn fails leaving a raw write transaction open",
			fn: func(ctx context.Context, conn *sql.Conn) error {
				if _, err := conn.ExecContext(ctx, `CREATE TEMP TABLE migration_scratch (x INTEGER)`); err != nil {
					return err
				}
				if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
					return err
				}
				return errors.New("migration step failed")
			},
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			store := openMigrationFKTestStore(t)
			ctx := context.Background()
			var sawEnforcement int
			err := withForeignKeysOff(ctx, store.db, func(conn *sql.Conn) error {
				if err := conn.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&sawEnforcement); err != nil {
					return err
				}
				return test.fn(ctx, conn)
			})
			if sawEnforcement != 0 {
				t.Fatalf("foreign_keys inside withForeignKeysOff = %d, want 0", sawEnforcement)
			}
			if strings.Contains(test.name, "fails") != (err != nil) {
				t.Fatalf("withForeignKeysOff() error = %v", err)
			}

			// The pool holds one connection. Had the pinned one been returned,
			// these would reuse it: enforcement off, its TEMP table visible, and
			// in the failing case its write lock still held.
			assertForeignKeysEnforced(t, store.db)
			var scratch int
			if err := store.db.QueryRow(`
				SELECT COUNT(*) FROM sqlite_temp_schema WHERE name = 'migration_scratch'
			`).Scan(&scratch); err != nil {
				t.Fatalf("inspect temp schema: %v", err)
			}
			if scratch != 0 {
				t.Fatal("the migration connection's TEMP table is visible from the pool")
			}
			seedMessageConversation(t, store, "conversation-after-discard", "account-a")
		})
	}
}

// A panicking migration must still release its transaction, or closing the
// migration connection would block forever.
func TestRunMigrationsReleasesConnectionWhenAMigrationPanics(t *testing.T) {
	store := openMigrationFKTestStore(t)
	migrations := withTestMigration(newMigration(len(embeddedMigrations)+1, "panics", `SELECT 1;`, func() ([]any, error) {
		panic("migration arguments exploded")
	}))
	done := make(chan any, 1)
	go func() {
		defer func() { done <- recover() }()
		_ = runMigrations(context.Background(), store.db, migrations)
	}()
	select {
	case recovered := <-done:
		if recovered == nil {
			t.Fatal("runMigrations() did not panic")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("runMigrations() hung after a panicking migration")
	}
	assertRowCount(t, store.db, "schema_migrations", len(embeddedMigrations))
	assertForeignKeysEnforced(t, store.db)
}

// The missing-parent check reads only the main schema, so a TEMP table that
// shadows a main table's name can neither hide a dropped parent nor make a
// valid one look missing.
func TestMigrationParentCheckIgnoresTempTables(t *testing.T) {
	t.Run("TEMP child cannot hide a dropped parent", func(t *testing.T) {
		store := openMigrationFKTestStore(t)
		migrations := withTestMigration(newMigration(len(embeddedMigrations)+1, "shadow_and_drop", `
			CREATE TEMP TABLE person_identities (x TEXT);
			DROP TABLE people;
		`, nil))
		err := runMigrations(context.Background(), store.db, migrations)
		if err == nil || !strings.Contains(err.Error(), `table "person_identities" references missing table "people"`) {
			t.Fatalf("runMigrations() error = %v, want main.person_identities reported", err)
		}
		assertRowCount(t, store.db, "people", 0)
		assertRowCount(t, store.db, "schema_migrations", len(embeddedMigrations))
	})

	t.Run("TEMP child with a TEMP parent is not a missing main parent", func(t *testing.T) {
		store := openMigrationFKTestStore(t)
		migrations := withTestMigration(newMigration(len(embeddedMigrations)+1, "shadow_child", `
			CREATE TEMP TABLE temp_parent (id TEXT PRIMARY KEY);
			CREATE TEMP TABLE person_identities (x TEXT REFERENCES temp_parent(id));
		`, nil))
		if err := runMigrations(context.Background(), store.db, migrations); err != nil {
			t.Fatalf("runMigrations(): %v", err)
		}
		assertRowCount(t, store.db, "schema_migrations", len(migrations))
		assertForeignKeysEnforced(t, store.db)
	})
}
