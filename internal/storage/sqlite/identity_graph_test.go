package sqlite

import (
	"database/sql"
	"path/filepath"
	"testing"
)

const identityGraphTestTimeMS int64 = 1_700_000_000_000

func TestConversationParticipantsEnforceAccountAndIdentity(t *testing.T) {
	store := openIdentityGraphTestStore(t)

	mustExec(t, store.db, `
		INSERT INTO accounts (account_id, bridge_key, created_at_ms, updated_at_ms)
		VALUES
			('account-a', 'signal', ?, ?),
			('account-b', 'whatsapp', ?, ?)
	`,
		identityGraphTestTimeMS,
		identityGraphTestTimeMS,
		identityGraphTestTimeMS,
		identityGraphTestTimeMS,
	)
	mustExec(t, store.db, `
		INSERT INTO identities (
			identity_id,
			account_id,
			kind,
			canonical_value,
			raw_value,
			created_at_ms,
			updated_at_ms
		) VALUES
			('identity-a', 'account-a', 'signal_aci', 'aci-a', 'aci-a', ?, ?),
			('identity-b', 'account-b', 'e164', '+15550000002', '+1 555 000 0002', ?, ?)
	`,
		identityGraphTestTimeMS,
		identityGraphTestTimeMS,
		identityGraphTestTimeMS,
		identityGraphTestTimeMS,
	)
	mustExec(t, store.db, `
		INSERT INTO conversations (
			conversation_id,
			account_id,
			remote_conversation_id,
			kind,
			created_at_ms,
			updated_at_ms
		) VALUES ('conversation-a', 'account-a', 'remote-a', 'direct', ?, ?)
	`, identityGraphTestTimeMS, identityGraphTestTimeMS)

	// account_id participates in both composite foreign keys, so this valid
	// same-account participant proves the constraints do not reject every row.
	mustExec(t, store.db, `
		INSERT INTO conversation_participants (account_id, conversation_id, identity_id)
		VALUES ('account-a', 'conversation-a', 'identity-a')
	`)

	expectExecError(t, store.db, "cross-account participant", `
		INSERT INTO conversation_participants (account_id, conversation_id, identity_id)
		VALUES ('account-a', 'conversation-a', 'identity-b')
	`)
	expectExecError(t, store.db, "orphan participant identity", `
		INSERT INTO conversation_participants (account_id, conversation_id, identity_id)
		VALUES ('account-a', 'conversation-a', 'missing-identity')
	`)
}

func TestPersonIdentitiesEnforceForeignKeysAndSingleOwner(t *testing.T) {
	store := openIdentityGraphTestStore(t)

	mustExec(t, store.db, `
		INSERT INTO accounts (account_id, bridge_key, created_at_ms, updated_at_ms)
		VALUES ('account-a', 'signal', ?, ?)
	`, identityGraphTestTimeMS, identityGraphTestTimeMS)
	mustExec(t, store.db, `
		INSERT INTO identities (
			identity_id,
			account_id,
			kind,
			canonical_value,
			raw_value,
			created_at_ms,
			updated_at_ms
		) VALUES ('identity-a', 'account-a', 'signal_aci', 'aci-a', 'aci-a', ?, ?)
	`, identityGraphTestTimeMS, identityGraphTestTimeMS)
	mustExec(t, store.db, `
		INSERT INTO people (person_id, display_name, created_at_ms, updated_at_ms)
		VALUES
			('person-a', 'Alice', ?, ?),
			('person-b', 'Alice B', ?, ?)
	`,
		identityGraphTestTimeMS,
		identityGraphTestTimeMS,
		identityGraphTestTimeMS,
		identityGraphTestTimeMS,
	)

	expectExecError(t, store.db, "person identity with missing person", `
		INSERT INTO person_identities (
			identity_id, person_id, provenance, confidence, linked_at_ms
		) VALUES ('identity-a', 'missing-person', 'explicit', 1.0, ?)
	`, identityGraphTestTimeMS)
	expectExecError(t, store.db, "person identity with missing identity", `
		INSERT INTO person_identities (
			identity_id, person_id, provenance, confidence, linked_at_ms
		) VALUES ('missing-identity', 'person-a', 'explicit', 1.0, ?)
	`, identityGraphTestTimeMS)

	mustExec(t, store.db, `
		INSERT INTO person_identities (
			identity_id, person_id, provenance, confidence, linked_at_ms
		) VALUES ('identity-a', 'person-a', 'explicit', 1.0, ?)
	`, identityGraphTestTimeMS)
	expectExecError(t, store.db, "identity linked to a second person", `
		INSERT INTO person_identities (
			identity_id, person_id, provenance, confidence, linked_at_ms
		) VALUES ('identity-a', 'person-b', 'explicit', 1.0, ?)
	`, identityGraphTestTimeMS)
}

func TestPeopleDoNotAutoMergeMatchingDisplayNames(t *testing.T) {
	store := openIdentityGraphTestStore(t)

	mustExec(t, store.db, `
		INSERT INTO people (person_id, display_name, created_at_ms, updated_at_ms)
		VALUES
			('person-a', 'Shared Display Name', ?, ?),
			('person-b', 'Shared Display Name', ?, ?)
	`,
		identityGraphTestTimeMS,
		identityGraphTestTimeMS,
		identityGraphTestTimeMS,
		identityGraphTestTimeMS,
	)

	var count int
	if err := store.db.QueryRow(`
		SELECT COUNT(*)
		FROM people
		WHERE display_name = 'Shared Display Name'
		  AND merged_into_person_id IS NULL
	`).Scan(&count); err != nil {
		t.Fatalf("count unmerged people with matching display names: %v", err)
	}
	if count != 2 {
		t.Fatalf("unmerged people with matching display names = %d, want 2", count)
	}
}

func openIdentityGraphTestStore(t *testing.T) *Store {
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

	if len(embeddedMigrations) != 11 {
		t.Fatalf("embedded migrations = %d, want 11", len(embeddedMigrations))
	}
	assertPragmaInt(t, store.db, "user_version", len(embeddedMigrations))
	ledger := readLedgerRow(t, store.db, 2)
	if ledger.name != "identity_graph" {
		t.Fatalf("migration 0002 name = %q, want identity_graph", ledger.name)
	}
	if ledger.checksum != embeddedMigrations[1].checksumSHA256 {
		t.Fatalf(
			"migration 0002 checksum = %q, want %q",
			ledger.checksum,
			embeddedMigrations[1].checksumSHA256,
		)
	}
	return store
}

func mustExec(t *testing.T, db *sql.DB, statement string, arguments ...any) {
	t.Helper()
	if _, err := db.Exec(statement, arguments...); err != nil {
		t.Fatalf("execute fixture SQL: %v", err)
	}
}

func expectExecError(
	t *testing.T,
	db *sql.DB,
	operation string,
	statement string,
	arguments ...any,
) {
	t.Helper()
	if _, err := db.Exec(statement, arguments...); err == nil {
		t.Fatalf("%s succeeded, want constraint error", operation)
	}
}
