package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

type ledgerRow struct {
	version     int
	name        string
	checksum    string
	appliedAtMS int64
	appVersion  string
	executionMS int64
}

func TestOpenInitializesBlankDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store ? #.sqlite3")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open(): %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close(): %v", err)
		}
	})

	instanceID, err := store.StoreInstanceID()
	if err != nil {
		t.Fatalf("StoreInstanceID(): %v", err)
	}
	decodedID, err := hex.DecodeString(instanceID)
	if err != nil {
		t.Fatalf("store instance ID %q is not hex: %v", instanceID, err)
	}
	if len(decodedID) != 16 {
		t.Fatalf("decoded store instance ID has %d bytes, want 16", len(decodedID))
	}

	rows, err := store.db.Query(`
		SELECT name
		FROM sqlite_schema
		WHERE type = 'table' AND name NOT LIKE 'sqlite_%'
		ORDER BY name
	`)
	if err != nil {
		t.Fatalf("query user tables: %v", err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			_ = rows.Close()
			t.Fatalf("scan user table: %v", err)
		}
		tables = append(tables, name)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		t.Fatalf("iterate user tables: %v", err)
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("close user table rows: %v", err)
	}
	wantTables := []string{
		"accounts",
		"attachments",
		"conversation_participants",
		"conversations",
		"devices",
		"identities",
		"inbox",
		"message_attachments",
		"messages",
		"outbox",
		"outbox_attachments",
		"outbox_reactions",
		"outbox_read_receipts",
		"people",
		"person_identities",
		"read_cursors",
		"schema_migrations",
		"store_metadata",
	}
	if !slices.Equal(tables, wantTables) {
		t.Fatalf("user tables = %v, want %v", tables, wantTables)
	}

	for _, table := range wantTables {
		var strict int
		if err := store.db.QueryRow(`
			SELECT strict
			FROM pragma_table_list
			WHERE schema = 'main' AND name = ?
		`, table).Scan(&strict); err != nil {
			t.Fatalf("read STRICT flag for %s: %v", table, err)
		}
		if strict != 1 {
			t.Errorf("table %s strict = %d, want 1", table, strict)
		}
	}

	ledger := readLedgerRow(t, store.db, 1)
	if ledger.version != 1 {
		t.Errorf("ledger version = %d, want 1", ledger.version)
	}
	if ledger.name != "storage_shell" {
		t.Errorf("ledger name = %q, want storage_shell", ledger.name)
	}
	sum := sha256.Sum256([]byte(migration0001SQL))
	wantChecksum := hex.EncodeToString(sum[:])
	if ledger.checksum != wantChecksum {
		t.Errorf("ledger checksum = %q, want %q", ledger.checksum, wantChecksum)
	}
	if ledger.appliedAtMS <= 0 {
		t.Errorf("ledger applied_at_ms = %d, want > 0", ledger.appliedAtMS)
	}
	if strings.TrimSpace(ledger.appVersion) == "" {
		t.Error("ledger app_version is blank")
	}
	if ledger.executionMS < 0 {
		t.Errorf("ledger execution_ms = %d, want >= 0", ledger.executionMS)
	}

	var (
		metadataCount int
		storedID      string
		revision      int64
		createdAtMS   int64
	)
	if err := store.db.QueryRow(`
		SELECT COUNT(*), store_instance_id, revision, created_at_ms
		FROM store_metadata
	`).Scan(&metadataCount, &storedID, &revision, &createdAtMS); err != nil {
		t.Fatalf("read store metadata: %v", err)
	}
	if metadataCount != 1 {
		t.Errorf("store metadata rows = %d, want 1", metadataCount)
	}
	if storedID != instanceID {
		t.Errorf("stored instance ID = %q, StoreInstanceID returned %q", storedID, instanceID)
	}
	if revision != 0 {
		t.Errorf("store revision = %d, want 0", revision)
	}
	if createdAtMS <= 0 {
		t.Errorf("store created_at_ms = %d, want > 0", createdAtMS)
	}

	assertRowCount(t, store.db, "schema_migrations", len(embeddedMigrations))
	assertPragmaInt(t, store.db, "user_version", len(embeddedMigrations))
	assertPragmaInt(t, store.db, "application_id", applicationID)
	assertPragmaInt(t, store.db, "foreign_keys", 1)
	assertPragmaInt(t, store.db, "busy_timeout", busyTimeoutMS)
	assertPragmaInt(t, store.db, "synchronous", 1)
	var journalMode string
	if err := store.db.QueryRow(`PRAGMA journal_mode`).Scan(&journalMode); err != nil {
		t.Fatalf("read journal_mode: %v", err)
	}
	if !strings.EqualFold(journalMode, "wal") {
		t.Errorf("journal_mode = %q, want WAL", journalMode)
	}
}

func TestOpenReopenIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.sqlite3")
	first, err := Open(path)
	if err != nil {
		t.Fatalf("first Open(): %v", err)
	}
	firstID, err := first.StoreInstanceID()
	if err != nil {
		_ = first.Close()
		t.Fatalf("first StoreInstanceID(): %v", err)
	}
	firstLedger := readLedgerRows(t, first.db)
	if err := first.Close(); err != nil {
		t.Fatalf("first Close(): %v", err)
	}

	second, err := Open(path)
	if err != nil {
		t.Fatalf("second Open(): %v", err)
	}
	t.Cleanup(func() {
		if err := second.Close(); err != nil {
			t.Errorf("second Close(): %v", err)
		}
	})
	secondID, err := second.StoreInstanceID()
	if err != nil {
		t.Fatalf("second StoreInstanceID(): %v", err)
	}
	if secondID != firstID {
		t.Errorf("instance ID changed on reopen: first %q, second %q", firstID, secondID)
	}
	secondLedger := readLedgerRows(t, second.db)
	if !slices.Equal(secondLedger, firstLedger) {
		t.Errorf("migration ledger changed on reopen:\nfirst:  %+v\nsecond: %+v", firstLedger, secondLedger)
	}

	assertRowCount(t, second.db, "schema_migrations", len(embeddedMigrations))
	assertRowCount(t, second.db, "store_metadata", 1)
	assertPragmaInt(t, second.db, "user_version", len(embeddedMigrations))
}

func TestOpenRejectsAppliedMigrationChecksumMismatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.sqlite3")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open(): %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close(): %v", err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open database for corruption: %v", err)
	}
	corruptChecksum := strings.Repeat("0", sha256.Size*2)
	result, err := db.Exec(
		`UPDATE schema_migrations SET checksum_sha256 = ? WHERE version = 1`,
		corruptChecksum,
	)
	if err != nil {
		_ = db.Close()
		t.Fatalf("corrupt migration checksum: %v", err)
	}
	if affected, err := result.RowsAffected(); err != nil {
		_ = db.Close()
		t.Fatalf("read corrupted rows affected: %v", err)
	} else if affected != 1 {
		_ = db.Close()
		t.Fatalf("corrupted rows = %d, want 1", affected)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close corruption connection: %v", err)
	}

	got, err := Open(path)
	if got != nil {
		_ = got.Close()
		t.Fatal("Open() returned a store for a corrupted migration checksum")
	}
	if !errors.Is(err, ErrMigrationChecksumMismatch) {
		t.Fatalf("Open() error = %v, want ErrMigrationChecksumMismatch", err)
	}
}

func TestConcurrentOpenInitializesOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.sqlite3")
	const openers = 8
	type result struct {
		id  string
		err error
	}

	start := make(chan struct{})
	results := make(chan result, openers)
	for range openers {
		go func() {
			<-start
			store, err := Open(path)
			if err != nil {
				results <- result{err: err}
				return
			}
			id, idErr := store.StoreInstanceID()
			closeErr := store.Close()
			if idErr != nil {
				err = idErr
			} else if closeErr != nil {
				err = fmt.Errorf("close store: %w", closeErr)
			}
			results <- result{id: id, err: err}
		}()
	}
	close(start)

	allResults := make([]result, 0, openers)
	for range openers {
		allResults = append(allResults, <-results)
	}
	var instanceID string
	for i, result := range allResults {
		if result.err != nil {
			t.Errorf("concurrent opener %d: %v", i, result.err)
			continue
		}
		if instanceID == "" {
			instanceID = result.id
		} else if result.id != instanceID {
			t.Errorf("concurrent opener %d got instance ID %q, want %q", i, result.id, instanceID)
		}
	}
	if t.Failed() {
		return
	}

	store, err := Open(path)
	if err != nil {
		t.Fatalf("final Open(): %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("final Close(): %v", err)
		}
	})
	finalID, err := store.StoreInstanceID()
	if err != nil {
		t.Fatalf("final StoreInstanceID(): %v", err)
	}
	if finalID != instanceID {
		t.Errorf("final instance ID = %q, concurrent ID = %q", finalID, instanceID)
	}
	assertRowCount(t, store.db, "schema_migrations", len(embeddedMigrations))
	assertRowCount(t, store.db, "store_metadata", 1)
}

func TestFailedMigrationRollsBackDDLAndLedger(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.sqlite3")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open(): %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close(): %v", err)
		}
	})

	testMigrations := append([]migration(nil), embeddedMigrations...)
	testMigrations = append(testMigrations, newMigration(len(testMigrations)+1, "broken", `
		CREATE TABLE migration_should_rollback (id INTEGER) STRICT;
		THIS IS NOT VALID SQL;
	`, nil))
	err = runMigrations(context.Background(), store.db, testMigrations)
	if err == nil {
		t.Fatal("runMigrations() succeeded for invalid SQL")
	}

	var tableExists bool
	if err := store.db.QueryRow(`
		SELECT EXISTS (
			SELECT 1 FROM sqlite_schema
			WHERE type = 'table' AND name = 'migration_should_rollback'
		)
	`).Scan(&tableExists); err != nil {
		t.Fatalf("inspect rolled-back table: %v", err)
	}
	if tableExists {
		t.Error("failed migration left partial DDL behind")
	}
	assertRowCount(t, store.db, "schema_migrations", len(embeddedMigrations))
	assertPragmaInt(t, store.db, "user_version", len(embeddedMigrations))
}

func readLedgerRow(t *testing.T, db *sql.DB, version int) ledgerRow {
	t.Helper()
	var row ledgerRow
	if err := db.QueryRow(`
		SELECT version, name, checksum_sha256, applied_at_ms, app_version, execution_ms
		FROM schema_migrations
		WHERE version = ?
	`, version).Scan(
		&row.version,
		&row.name,
		&row.checksum,
		&row.appliedAtMS,
		&row.appVersion,
		&row.executionMS,
	); err != nil {
		t.Fatalf("read migration ledger row: %v", err)
	}
	return row
}

func readLedgerRows(t *testing.T, db *sql.DB) []ledgerRow {
	t.Helper()
	rows, err := db.Query(`
		SELECT version, name, checksum_sha256, applied_at_ms, app_version, execution_ms
		FROM schema_migrations
		ORDER BY version
	`)
	if err != nil {
		t.Fatalf("read migration ledger: %v", err)
	}
	defer rows.Close()

	var ledger []ledgerRow
	for rows.Next() {
		var row ledgerRow
		if err := rows.Scan(
			&row.version,
			&row.name,
			&row.checksum,
			&row.appliedAtMS,
			&row.appVersion,
			&row.executionMS,
		); err != nil {
			t.Fatalf("scan migration ledger row: %v", err)
		}
		ledger = append(ledger, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate migration ledger: %v", err)
	}
	return ledger
}

func assertRowCount(t *testing.T, db *sql.DB, table string, want int) {
	t.Helper()
	var got int
	if err := db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&got); err != nil {
		t.Fatalf("count %s rows: %v", table, err)
	}
	if got != want {
		t.Errorf("%s rows = %d, want %d", table, got, want)
	}
}

func assertPragmaInt(t *testing.T, db *sql.DB, pragma string, want int) {
	t.Helper()
	var got int
	if err := db.QueryRow("PRAGMA " + pragma).Scan(&got); err != nil {
		t.Fatalf("read %s pragma: %v", pragma, err)
	}
	if got != want {
		t.Errorf("%s pragma = %d, want %d", pragma, got, want)
	}
}
