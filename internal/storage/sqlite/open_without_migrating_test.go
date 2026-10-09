package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// openStoreAtVersion builds a store migrated through the first version
// migrations only, the shape of a store the app has not upgraded yet.
func openStoreAtVersion(t *testing.T, path string, version int) {
	t.Helper()
	database, err := sql.Open("sqlite", storeDSN(path))
	if err != nil {
		t.Fatalf("sql.Open(): %v", err)
	}
	defer database.Close()
	if err := enableWAL(context.Background(), database); err != nil {
		t.Fatalf("enableWAL(): %v", err)
	}
	if err := runMigrations(context.Background(), database, embeddedMigrations[:version]); err != nil {
		t.Fatalf("runMigrations(%d): %v", version, err)
	}
}

func TestOpenWithoutMigratingServesACurrentStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.sqlite3")
	owner, err := Open(path)
	if err != nil {
		t.Fatalf("Open(): %v", err)
	}
	mustRepositoryWrite(t, "UpsertAccount", owner.UpsertAccount(repositoryTestAccount("account-a")))
	ledger := readLedgerRows(t, owner.db)
	if err := owner.Close(); err != nil {
		t.Fatalf("Close(): %v", err)
	}

	client, err := OpenWithoutMigrating(path)
	if err != nil {
		t.Fatalf("OpenWithoutMigrating(): %v", err)
	}
	defer client.Close()
	if _, err := client.GetAccount("account-a"); err != nil {
		t.Fatalf("GetAccount through the client store: %v", err)
	}
	if got := readLedgerRows(t, client.db); !slices.Equal(got, ledger) {
		t.Fatalf("ledger changed:\nbefore: %+v\nafter:  %+v", ledger, got)
	}
}

// A newer client meeting a store the app has not upgraded refuses it and
// leaves it exactly as it was: no migration, no ledger row, no user_version
// change. (Open would apply the pending migrations under the app's daemon.)
func TestOpenWithoutMigratingRefusesAStoreWithPendingMigrations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.sqlite3")
	behind := len(embeddedMigrations) - 1
	openStoreAtVersion(t, path, behind)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	store, err := OpenWithoutMigrating(path)
	if !errors.Is(err, ErrMigrationPending) {
		if store != nil {
			store.Close()
		}
		t.Fatalf("OpenWithoutMigrating() error = %v, want ErrMigrationPending", err)
	}
	if !strings.Contains(err.Error(), "the OpenMessage app applies them when it starts") {
		t.Fatalf("error %q does not say who applies the migrations", err)
	}

	database, err := sql.Open("sqlite", storeDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if got := len(readLedgerRows(t, database)); got != behind {
		t.Fatalf("ledger rows = %d after the refused open, want %d (nothing applied)", got, behind)
	}
	assertPragmaInt(t, database, "user_version", behind)
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(before, after) {
		t.Fatal("the refused open changed the database file")
	}
}

func TestOpenWithoutMigratingRefusesNewerAndMissingStores(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.sqlite3")
	owner, err := Open(path)
	if err != nil {
		t.Fatalf("Open(): %v", err)
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	// A build that knows one migration fewer than the store has applied.
	if store, err := openWithoutMigrating(path, embeddedMigrations[:len(embeddedMigrations)-1]); err == nil ||
		!strings.Contains(err.Error(), "newer than supported") {
		if store != nil {
			store.Close()
		}
		t.Fatalf("older build opening a newer store: error = %v, want newer than supported", err)
	}

	missing := filepath.Join(t.TempDir(), "missing.sqlite3")
	if store, err := OpenWithoutMigrating(missing); err == nil || !errors.Is(err, os.ErrNotExist) {
		if store != nil {
			store.Close()
		}
		t.Fatalf("OpenWithoutMigrating(missing) error = %v, want os.ErrNotExist", err)
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("OpenWithoutMigrating created %s: %v", missing, err)
	}
}

// The owner may hold SQLite's write reservation for seconds (a migration, a
// large projection). A client open must not queue behind it, and once open the
// client must not hold anything that delays the owner's next write.
func TestOpenWithoutMigratingNeverTakesTheWriteLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.sqlite3")
	owner, err := Open(path)
	if err != nil {
		t.Fatalf("Open(): %v", err)
	}
	defer owner.Close()
	ctx := context.Background()

	writer, err := owner.db.BeginTx(ctx, nil) // BEGIN IMMEDIATE via the DSN
	if err != nil {
		t.Fatalf("owner BeginTx(): %v", err)
	}
	if _, err := writer.ExecContext(ctx, `
		INSERT INTO accounts (account_id, bridge_key, created_at_ms, updated_at_ms)
		VALUES ('held', 'signal', 1, 1)
	`); err != nil {
		t.Fatalf("owner write: %v", err)
	}
	start := time.Now()
	client, err := OpenWithoutMigrating(path)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("OpenWithoutMigrating() while the owner writes: %v", err)
	}
	defer client.Close()
	if elapsed > time.Second {
		t.Fatalf("OpenWithoutMigrating() took %s with the write lock held; it waited for the lock (busy_timeout is %d ms)", elapsed, busyTimeoutMS)
	}
	if err := writer.Commit(); err != nil {
		t.Fatalf("owner Commit(): %v", err)
	}

	start = time.Now()
	next, err := owner.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("owner BeginTx() after the client opened: %v", err)
	}
	if _, err := next.ExecContext(ctx, `UPDATE accounts SET display_name = 'x' WHERE account_id = 'held'`); err != nil {
		t.Fatalf("owner write after the client opened: %v", err)
	}
	if err := next.Commit(); err != nil {
		t.Fatalf("owner Commit(): %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("owner write took %s with a client store open", elapsed)
	}
}
