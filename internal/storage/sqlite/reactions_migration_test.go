package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestReactionsMigrationAppliesToBlankAndExistingV9Database(t *testing.T) {
	t.Run("blank", func(t *testing.T) {
		store, err := Open(filepath.Join(t.TempDir(), "store.sqlite3"))
		if err != nil {
			t.Fatalf("Open(): %v", err)
		}
		t.Cleanup(func() {
			if err := store.Close(); err != nil {
				t.Errorf("Close(): %v", err)
			}
		})

		assertReactionsMigration(t, store)
	})

	t.Run("existing v9", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "store.sqlite3")
		database, err := sql.Open("sqlite", storeDSN(path))
		if err != nil {
			t.Fatalf("sql.Open(): %v", err)
		}
		if err := database.Ping(); err != nil {
			_ = database.Close()
			t.Fatalf("Ping(): %v", err)
		}
		if err := enableWAL(context.Background(), database); err != nil {
			_ = database.Close()
			t.Fatalf("enableWAL(): %v", err)
		}
		if err := verifyConnectionPragmas(context.Background(), database); err != nil {
			_ = database.Close()
			t.Fatalf("verifyConnectionPragmas(): %v", err)
		}
		if err := runMigrations(context.Background(), database, embeddedMigrations[:9]); err != nil {
			_ = database.Close()
			t.Fatalf("runMigrations(v9): %v", err)
		}
		before := readLedgerRows(t, database)
		if len(before) != 9 {
			_ = database.Close()
			t.Fatalf("v9 ledger rows = %d, want 9", len(before))
		}
		if err := database.Close(); err != nil {
			t.Fatalf("close v9 database: %v", err)
		}

		store, err := Open(path)
		if err != nil {
			t.Fatalf("Open(v9): %v", err)
		}
		t.Cleanup(func() {
			if err := store.Close(); err != nil {
				t.Errorf("Close(): %v", err)
			}
		})
		after := readLedgerRows(t, store.db)
		if len(after) != 11 {
			t.Fatalf("migrated ledger rows = %d, want 11", len(after))
		}
		if !slices.Equal(after[:9], before) {
			t.Fatalf("migrations 0001-0009 changed:\nbefore: %+v\nafter:  %+v", before, after[:9])
		}
		assertReactionsMigration(t, store)
	})
}

func assertReactionsMigration(t *testing.T, store *Store) {
	t.Helper()
	if len(embeddedMigrations) != 11 {
		t.Fatalf("embedded migrations = %d, want 11", len(embeddedMigrations))
	}
	assertPragmaInt(t, store.db, "user_version", 11)
	ledger := readLedgerRow(t, store.db, 10)
	if ledger.name != "reactions" {
		t.Fatalf("migration 0010 name = %q, want reactions", ledger.name)
	}
	if ledger.checksum != embeddedMigrations[9].checksumSHA256 {
		t.Fatalf(
			"migration 0010 checksum = %q, want %q",
			ledger.checksum,
			embeddedMigrations[9].checksumSHA256,
		)
	}
	const wantChecksum = "dfab4551335d92045cb895e5b2c781f4f57216bbc428a705fc26bbdd474db0b1"
	if ledger.checksum != wantChecksum {
		t.Fatalf("migration 0010 checksum = %q, want pinned %q", ledger.checksum, wantChecksum)
	}

	for _, table := range []string{"reactions", "reaction_snapshot_fences"} {
		var strict int
		if err := store.db.QueryRow(`
			SELECT strict
			FROM pragma_table_list
			WHERE schema = 'main' AND name = ?
		`, table).Scan(&strict); err != nil {
			t.Fatalf("read %s STRICT flag: %v", table, err)
		}
		if strict != 1 {
			t.Fatalf("%s strict = %d, want 1", table, strict)
		}
	}
	for _, index := range []string{"reactions_message_state_idx", "reactions_conversation_idx"} {
		var exists bool
		if err := store.db.QueryRow(`
			SELECT EXISTS (
				SELECT 1 FROM sqlite_schema
				WHERE type = 'index' AND name = ?
			)
		`, index).Scan(&exists); err != nil {
			t.Fatalf("inspect migration index %q: %v", index, err)
		}
		if !exists {
			t.Fatalf("migration index %q is missing", index)
		}
	}
}

func TestReactionsMigrationEnforcesTombstoneEmojiAndForeignKeys(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "store.sqlite3"))
	if err != nil {
		t.Fatalf("Open(): %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close(): %v", err)
		}
	})

	const nowMS = int64(2_000_000_000_000)
	mustExec(t, store.db, `
		INSERT INTO accounts (account_id, bridge_key, created_at_ms, updated_at_ms)
		VALUES ('account-reactions', 'test', ?, ?)
	`, nowMS, nowMS)
	mustExec(t, store.db, `
		INSERT INTO conversations (
			conversation_id, account_id, remote_conversation_id, kind,
			created_at_ms, updated_at_ms
		) VALUES ('conversation-reactions', 'account-reactions', 'remote-conversation', 'direct', ?, ?)
	`, nowMS, nowMS)
	mustExec(t, store.db, `
		INSERT INTO identities (
			identity_id, account_id, kind, canonical_value, raw_value,
			created_at_ms, updated_at_ms
		) VALUES ('identity-reactor', 'account-reactions', 'phone', '+15550100001', '+15550100001', ?, ?)
	`, nowMS, nowMS)
	mustExec(t, store.db, `
		INSERT INTO messages (
			message_id, conversation_id, account_id, remote_message_id,
			direction, occurred_at_ms, created_at_ms, updated_at_ms
		) VALUES (
			'message-reactions', 'conversation-reactions', 'account-reactions',
			'remote-message', 'incoming', ?, ?, ?
		)
	`, nowMS, nowMS, nowMS)

	insert := func(reactorKey, identityID, emoji, state string) error {
		_, err := store.db.Exec(`
			INSERT INTO reactions (
				message_id, reactor_key, account_id, conversation_id,
				reactor_identity_id, emoji, state, occurred_at_ms,
				created_at_ms, updated_at_ms
			) VALUES (
				'message-reactions', ?, 'account-reactions', 'conversation-reactions',
				NULLIF(?, ''), ?, ?, ?, ?, ?
			)
		`, reactorKey, identityID, emoji, state, nowMS, nowMS, nowMS)
		return err
	}
	if err := insert("identity-reactor", "identity-reactor", "👍", "active"); err != nil {
		t.Fatalf("insert active reaction: %v", err)
	}
	if err := insert("remove-before-add", "", "", "removed"); err != nil {
		t.Fatalf("insert empty-emoji tombstone: %v", err)
	}
	if err := insert("non-empty-whitespace", "", "   ", "active"); err == nil {
		t.Fatal("insert whitespace-only active reaction succeeded, want CHECK constraint error")
	}
	for _, test := range []struct {
		name, emoji, state string
	}{
		{name: "active empty", emoji: "", state: "active"},
		{name: "active over limit", emoji: strings.Repeat("a", 65), state: "active"},
		{name: "tombstone over limit", emoji: strings.Repeat("a", 65), state: "removed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := insert("invalid-"+test.name, "", test.emoji, test.state); err == nil {
				t.Fatal("reaction insert succeeded, want CHECK constraint error")
			}
		})
	}
	if err := insert("missing-identity", "identity-missing", "👍", "active"); err == nil {
		t.Fatal("reaction with missing identity succeeded, want foreign-key error")
	}
	if _, err := store.db.Exec(`
		INSERT INTO reactions (
			message_id, reactor_key, account_id, conversation_id, emoji, state,
			occurred_at_ms, created_at_ms, updated_at_ms
		) VALUES (
			'message-missing', 'missing-message', 'account-reactions',
			'conversation-reactions', '👍', 'active', ?, ?, ?
		)
	`, nowMS, nowMS, nowMS); err == nil {
		t.Fatal("reaction with missing message succeeded, want foreign-key error")
	}
	if _, err := store.db.Exec(`
		INSERT INTO reactions (
			message_id, reactor_key, account_id, conversation_id, emoji, state,
			occurred_at_ms, created_at_ms, updated_at_ms
		) VALUES (
			'message-reactions', 'missing-conversation', 'account-reactions',
			'conversation-missing', '👍', 'active', ?, ?, ?
		)
	`, nowMS, nowMS, nowMS); err == nil {
		t.Fatal("reaction with missing conversation succeeded, want foreign-key error")
	}

	if _, err := store.db.Exec(`DELETE FROM messages WHERE message_id = 'message-reactions'`); err != nil {
		t.Fatalf("delete reaction parent message: %v", err)
	}
	assertRowCount(t, store.db, "reactions", 0)
}
