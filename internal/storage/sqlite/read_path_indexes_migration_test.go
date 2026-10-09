package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
)

const readPathIndexesChecksum = "d23db3d6dfcd5b9d22423e4cbd1414d274eb557807892c376e5096533d171c3f"

// readPathIndexesMigration returns the position of the read_path_indexes
// migration, found by name so the test survives a renumbering when parallel
// migrations land first.
func readPathIndexesMigration(t *testing.T) int {
	t.Helper()
	for i, migration := range embeddedMigrations {
		if migration.name == "read_path_indexes" {
			return i
		}
	}
	t.Fatal("embedded migrations have no read_path_indexes")
	return -1
}

func TestReadPathIndexesMigrationAppliesToBlankAndExistingDatabase(t *testing.T) {
	position := readPathIndexesMigration(t)
	if got := embeddedMigrations[position].checksumSHA256; got != readPathIndexesChecksum {
		t.Fatalf("read_path_indexes checksum = %s, want pinned %s (an applied migration must never change)", got, readPathIndexesChecksum)
	}

	t.Run("blank", func(t *testing.T) {
		store := openRepositoryTestStore(t)
		assertReadPathIndexes(t, store.db)
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
			t.Fatalf("runMigrations(before read_path_indexes): %v", err)
		}
		before := &Store{db: database}
		seed := seedReadStoreInto(t, before, 4242)
		if len(seed.messages) == 0 {
			_ = database.Close()
			t.Fatal("seed wrote no messages; the migration would index nothing")
		}
		ledgerBefore := readLedgerRows(t, database)
		rowsBefore := readPathFingerprint(t, database)
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
		assertReadPathIndexes(t, store.db)
		if got := readPathFingerprint(t, store.db); !reflect.DeepEqual(got, rowsBefore) {
			t.Fatalf("migration changed rows:\nbefore: %v\nafter:  %v", rowsBefore, got)
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
		t.Cleanup(func() {
			if err := reopened.Close(); err != nil {
				t.Errorf("Close(): %v", err)
			}
		})
		if got := readLedgerRows(t, reopened.db); !slices.Equal(got, ledgerAfter) {
			t.Fatalf("reopen changed the ledger:\nbefore: %+v\nafter:  %+v", ledgerAfter, got)
		}
	})
}

func assertReadPathIndexes(t *testing.T, db *sql.DB) {
	t.Helper()
	position := readPathIndexesMigration(t)
	ledger := readLedgerRow(t, db, position+1)
	if ledger.name != "read_path_indexes" || ledger.checksum != readPathIndexesChecksum {
		t.Fatalf("ledger row %d = %+v, want read_path_indexes with the pinned checksum", position+1, ledger)
	}
	for _, index := range []struct {
		name    string
		table   string
		columns []string
		partial bool
	}{
		{"messages_account_direction_time_idx", "messages", []string{"account_id", "direction", "occurred_at_ms"}, false},
		{"messages_time_idx", "messages", []string{"occurred_at_ms", "message_id"}, false},
		{"outbox_local_message_idx", "outbox", []string{"account_id", "local_message_id", "created_at_ms", "outbox_id"}, true},
	} {
		rows, err := db.Query(`SELECT name FROM pragma_index_info(?) ORDER BY seqno`, index.name)
		if err != nil {
			t.Fatalf("read %s columns: %v", index.name, err)
		}
		var columns []string
		for rows.Next() {
			var column string
			if err := rows.Scan(&column); err != nil {
				t.Fatalf("scan %s column: %v", index.name, err)
			}
			columns = append(columns, column)
		}
		rows.Close()
		if !slices.Equal(columns, index.columns) {
			t.Fatalf("%s columns = %v, want %v", index.name, columns, index.columns)
		}
		var table string
		var partial bool
		if err := db.QueryRow(`
			SELECT tbl_name, sql LIKE '%WHERE%' FROM sqlite_schema WHERE type = 'index' AND name = ?
		`, index.name).Scan(&table, &partial); err != nil {
			t.Fatalf("read %s definition: %v", index.name, err)
		}
		if table != index.table || partial != index.partial {
			t.Fatalf("%s on %s partial=%v, want on %s partial=%v", index.name, table, partial, index.table, index.partial)
		}
	}
}

// readPathFingerprint lists every row the new indexes cover, so the test can
// show the migration only adds indexes.
func readPathFingerprint(t *testing.T, db *sql.DB) []string {
	t.Helper()
	var out []string
	for _, query := range []string{
		`SELECT message_id || '|' || account_id || '|' || direction || '|' || occurred_at_ms FROM messages ORDER BY message_id`,
		`SELECT outbox_id || '|' || account_id || '|' || ifnull(local_message_id, '') || '|' || created_at_ms || '|' || state FROM outbox ORDER BY outbox_id`,
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
