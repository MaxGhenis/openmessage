package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"
)

// TestOpenReadOnlyOnSchema10FixtureLeavesStoreUnchanged is the store-level
// regression for the version-skew hazard: a client built with migration 0011
// used to migrate a schema-10 store it merely read (sqlite.Open always runs
// migrations), after which the schema-10 daemon refused the store. The
// read-only attach must serve the whole client read inventory from the
// schema-10 store and leave its ledger, pragmas, rows and main file intact.
func TestOpenReadOnlyOnSchema10FixtureLeavesStoreUnchanged(t *testing.T) {
	if LatestSchemaVersion() <= 10 {
		t.Fatalf("LatestSchemaVersion() = %d; this fixture needs a build newer than schema 10", LatestSchemaVersion())
	}
	path, fixture := buildClientFixture(t, 10, defaultClientFixtureSpec())
	before := mustFingerprint(t, path)
	if before.UserVersion != 10 || len(before.Ledger) != 10 {
		t.Fatalf("fixture user_version=%d ledger rows=%d, want 10/10", before.UserVersion, len(before.Ledger))
	}
	filesBefore := dirNames(t, filepath.Dir(path))

	store, info, err := OpenReadOnly(path)
	if err != nil {
		t.Fatalf("OpenReadOnly(schema 10): %v", err)
	}
	if info.SchemaVersion != 10 || info.BuildSchemaVersion != LatestSchemaVersion() {
		t.Fatalf("ReadOnlyInfo = %+v, want store 10, build %d", info, LatestSchemaVersion())
	}
	if !store.ReadOnly() || store.SchemaVersion() != 10 {
		t.Fatalf("store ReadOnly()=%v SchemaVersion()=%d, want true/10", store.ReadOnly(), store.SchemaVersion())
	}

	ctx := context.Background()
	for _, read := range clientReadInventory {
		count, err := read.run(ctx, store, fixture)
		if err != nil {
			t.Errorf("%s on schema 10: %v", read.name, err)
			continue
		}
		if count == 0 {
			t.Errorf("%s on schema 10 returned no rows; the fixture seeds data for every read", read.name)
		}
	}
	repository, err := NewOutboxRepository(store, time.Now)
	if err != nil {
		t.Fatalf("NewOutboxRepository(): %v", err)
	}
	state, ok, err := repository.LatestStateForLocalMessage(ctx, fixture.messageAccount[fixture.outgoingMsgID], fixture.outgoingMsgID)
	if err != nil || !ok || state != OutboxConfirmed {
		t.Fatalf("LatestStateForLocalMessage() = %q, %v, %v; want confirmed", state, ok, err)
	}
	// A full outbox row read needs 0011's expires_at_ms. The client inventory
	// never does one; this pins why MinClientReadSchemaVersion stays honest
	// only while client reads select narrow columns.
	if _, err := repository.FindByID(ctx, fixture.outboxID); err == nil || !strings.Contains(err.Error(), "expires_at_ms") {
		t.Fatalf("full outbox row read on schema 10 error = %v, want a missing expires_at_ms column", err)
	}

	writeErr := store.UpsertAccount(Account{
		AccountID:   "account-written-by-client",
		BridgeKey:   "signal",
		DisplayName: "must not land",
		Mode:        AccountModeLive,
		Enabled:     true,
		ConfigJSON:  `{}`,
		CreatedAtMS: readOnlyFixtureTimeMS,
		UpdatedAtMS: readOnlyFixtureTimeMS,
	})
	if !IsReadOnlyError(writeErr) {
		t.Fatalf("UpsertAccount on a read-only store error = %v, want IsReadOnlyError", writeErr)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close(): %v", err)
	}

	assertFingerprintUnchanged(t, "schema-10 store after a read-only session", before, mustFingerprint(t, path))
	assertOnlyWALSidecarsAdded(t, path, filesBefore)

	// Contrast: the migrating open on a copy of the same fixture does move it
	// to the build's version, so the assertions above are not vacuous.
	copyPath := copyStoreFile(t, path)
	migrated, err := Open(copyPath)
	if err != nil {
		t.Fatalf("Open(copy of schema 10): %v", err)
	}
	if err := migrated.Close(); err != nil {
		t.Fatalf("Close(migrated copy): %v", err)
	}
	if got := mustFingerprint(t, copyPath).UserVersion; got != LatestSchemaVersion() {
		t.Fatalf("migrating Open left the copy at user_version %d, want %d", got, LatestSchemaVersion())
	}
}

func TestOpenReadOnlyMissingFileCreatesNothing(t *testing.T) {
	dir := t.TempDir()
	for _, path := range []string{
		filepath.Join(dir, "store.sqlite3"),
		filepath.Join(dir, "v2", "store.sqlite3"),
	} {
		store, _, err := OpenReadOnly(path)
		if store != nil {
			_ = store.Close()
		}
		if !errors.Is(err, ErrStoreMissing) {
			t.Fatalf("OpenReadOnly(%s) error = %v, want ErrStoreMissing", path, err)
		}
		if !strings.Contains(err.Error(), path) {
			t.Fatalf("OpenReadOnly(%s) error %q does not name the path", path, err)
		}
	}
	if names := dirNames(t, dir); len(names) != 0 {
		t.Fatalf("OpenReadOnly on missing stores created %v", names)
	}

	// The stat is not the only guard: if the file vanishes between the stat
	// and the open, the mode=ro DSN itself refuses to create it, even though
	// the driver always passes SQLITE_OPEN_CREATE.
	raced := filepath.Join(dir, "raced.sqlite3")
	db, err := sql.Open("sqlite", readOnlyDSN(raced))
	if err != nil {
		t.Fatalf("sql.Open(read-only DSN): %v", err)
	}
	pingErr := db.Ping()
	_ = db.Close()
	if pingErr == nil {
		t.Fatal("read-only DSN opened a missing file")
	}
	if code, ok := sqliteErrorCode(pingErr); !ok || code&0xff != sqliteCantOpenCode {
		t.Fatalf("read-only DSN on a missing file error = %v, want SQLITE_CANTOPEN", pingErr)
	}
	if !errors.Is(mapReadOnlyAttachError(pingErr), ErrReadOnlyAttach) {
		t.Fatalf("mapReadOnlyAttachError(%v) is not ErrReadOnlyAttach", pingErr)
	}
	if names := dirNames(t, dir); len(names) != 0 {
		t.Fatalf("read-only DSN on a missing file created %v", names)
	}

	// A directory at the store path is not a store either.
	if err := os.Mkdir(filepath.Join(dir, "directory.sqlite3"), 0o700); err != nil {
		t.Fatalf("Mkdir(): %v", err)
	}
	if _, _, err := OpenReadOnly(filepath.Join(dir, "directory.sqlite3")); !errors.Is(err, ErrStoreMissing) {
		t.Fatalf("OpenReadOnly(directory) error = %v, want ErrStoreMissing", err)
	}
	if _, _, err := OpenReadOnly("  "); err == nil {
		t.Fatal("OpenReadOnly(blank path) succeeded")
	}
}

func TestOpenReadOnlyRefusesNewerSchema(t *testing.T) {
	t.Run("build older than store", func(t *testing.T) {
		path, _ := buildClientFixture(t, LatestSchemaVersion(), defaultClientFixtureSpec())
		before := mustFingerprint(t, path)
		older := embeddedMigrations[:len(embeddedMigrations)-1]
		store, info, err := openReadOnly(path, older, MinClientReadSchemaVersion)
		if store != nil {
			_ = store.Close()
		}
		if !errors.Is(err, ErrSchemaNewer) {
			t.Fatalf("openReadOnly(older build) error = %v, want ErrSchemaNewer", err)
		}
		if info.SchemaVersion != LatestSchemaVersion() || info.BuildSchemaVersion != len(older) {
			t.Fatalf("ReadOnlyInfo = %+v, want store %d build %d", info, LatestSchemaVersion(), len(older))
		}
		for _, want := range []string{
			fmt.Sprintf("database schema version %d is newer than supported version %d", LatestSchemaVersion(), len(older)),
			"update this openmessage binary",
		} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("error %q does not contain %q", err, want)
			}
		}
		assertFingerprintUnchanged(t, "newer store after a refused attach", before, mustFingerprint(t, path))
	})

	t.Run("ledger row appended by a future build", func(t *testing.T) {
		path, _ := buildClientFixture(t, LatestSchemaVersion(), defaultClientFixtureSpec())
		future := LatestSchemaVersion() + 1
		appendFutureLedgerRow(t, path, future)
		before := mustFingerprint(t, path)
		_, info, err := OpenReadOnly(path)
		if !errors.Is(err, ErrSchemaNewer) || errors.Is(err, ErrLedgerMismatch) || errors.Is(err, ErrSchemaTooOld) {
			t.Fatalf("OpenReadOnly(future ledger) error = %v, want only ErrSchemaNewer", err)
		}
		if info.SchemaVersion != future {
			t.Fatalf("ReadOnlyInfo.SchemaVersion = %d, want %d", info.SchemaVersion, future)
		}
		assertFingerprintUnchanged(t, "future store after a refused attach", before, mustFingerprint(t, path))

		// The owner's migrating open refuses the same store with the message
		// it has always used, now also classified.
		_, openErr := Open(path)
		if !errors.Is(openErr, ErrSchemaNewer) {
			t.Fatalf("Open(future ledger) error = %v, want ErrSchemaNewer", openErr)
		}
		want := fmt.Sprintf("migrate sqlite store: database schema version %d is newer than supported version %d", future, LatestSchemaVersion())
		if openErr.Error() != want {
			t.Fatalf("Open(future ledger) error text = %q, want unchanged %q", openErr, want)
		}
	})
}

func TestOpenReadOnlyRefusesBelowMinimum(t *testing.T) {
	path, _ := buildClientFixture(t, MinClientReadSchemaVersion-1, defaultClientFixtureSpec())
	before := mustFingerprint(t, path)
	store, info, err := OpenReadOnly(path)
	if store != nil {
		_ = store.Close()
	}
	if !errors.Is(err, ErrSchemaTooOld) {
		t.Fatalf("OpenReadOnly(schema %d) error = %v, want ErrSchemaTooOld", MinClientReadSchemaVersion-1, err)
	}
	if info.SchemaVersion != MinClientReadSchemaVersion-1 {
		t.Fatalf("ReadOnlyInfo.SchemaVersion = %d, want %d", info.SchemaVersion, MinClientReadSchemaVersion-1)
	}
	if !strings.Contains(err.Error(), "start the OpenMessage app") {
		t.Fatalf("error %q gives no remediation", err)
	}
	assertFingerprintUnchanged(t, "old store after a refused attach", before, mustFingerprint(t, path))
}

// TestOpenReadOnlyRejectsTamperedStore covers every ledger state no build of
// this lineage writes. Each is refused with ErrLedgerMismatch and left as is.
func TestOpenReadOnlyRejectsTamperedStore(t *testing.T) {
	tests := []struct {
		name   string
		tamper func(t *testing.T, path string)
	}{
		{name: "checksum flipped", tamper: func(t *testing.T, path string) {
			execOnStoreFile(t, path, `UPDATE schema_migrations SET checksum_sha256 = ? WHERE version = 3`, strings.Repeat("0", 64))
		}},
		{name: "migration renamed", tamper: func(t *testing.T, path string) {
			execOnStoreFile(t, path, `UPDATE schema_migrations SET name = 'renamed' WHERE version = 7`)
		}},
		{name: "middle ledger row missing", tamper: func(t *testing.T, path string) {
			execOnStoreFile(t, path, `DELETE FROM schema_migrations WHERE version = 5`)
		}},
		{name: "user_version behind ledger", tamper: func(t *testing.T, path string) {
			execOnStoreFile(t, path, fmt.Sprintf(`PRAGMA user_version = %d`, LatestSchemaVersion()-1))
		}},
		{name: "wrong application_id", tamper: func(t *testing.T, path string) {
			execOnStoreFile(t, path, `PRAGMA application_id = 1234`)
		}},
		{name: "empty ledger", tamper: func(t *testing.T, path string) {
			execOnStoreFile(t, path, `DELETE FROM schema_migrations`)
		}},
		{name: "tables but no ledger", tamper: func(t *testing.T, path string) {
			execOnStoreFile(t, path, `DROP TABLE schema_migrations`)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path, _ := buildClientFixture(t, LatestSchemaVersion(), defaultClientFixtureSpec())
			tt.tamper(t, path)
			before := mustFingerprint(t, path)
			store, _, err := OpenReadOnly(path)
			if store != nil {
				_ = store.Close()
			}
			if !errors.Is(err, ErrLedgerMismatch) || errors.Is(err, ErrSchemaNewer) || errors.Is(err, ErrSchemaTooOld) {
				t.Fatalf("OpenReadOnly(%s) error = %v, want only ErrLedgerMismatch", tt.name, err)
			}
			assertFingerprintUnchanged(t, tt.name, before, mustFingerprint(t, path))
		})
	}

	// A blank database is the one state the owner accepts (it provisions it)
	// and the client refuses: a client never provisions a store.
	t.Run("blank database", func(t *testing.T) {
		for name, content := range map[string][]byte{"zero-byte file": nil, "garbage": []byte("not a database")} {
			path := filepath.Join(t.TempDir(), "store.sqlite3")
			if err := os.WriteFile(path, content, 0o600); err != nil {
				t.Fatalf("WriteFile(): %v", err)
			}
			store, _, err := OpenReadOnly(path)
			if store != nil {
				_ = store.Close()
			}
			if err == nil {
				t.Fatalf("OpenReadOnly(%s) succeeded", name)
			}
			if len(content) == 0 && !errors.Is(err, ErrLedgerMismatch) {
				t.Fatalf("OpenReadOnly(%s) error = %v, want ErrLedgerMismatch", name, err)
			}
			got, readErr := os.ReadFile(path)
			if readErr != nil || string(got) != string(content) {
				t.Fatalf("OpenReadOnly(%s) changed the file: %q, %v", name, got, readErr)
			}
		}
	})
}

// TestOpenReadOnlyBesideLiveWriter pins I5: the read-only attach never asks
// for SQLite's write reservation, so it attaches while the owner holds a
// write transaction, sees commits made after it attached, and does not stop
// the owner from checkpointing while idle. The contrast leg shows why the
// client must not use the migrating open: runMigrations takes the write
// reservation even when nothing is pending, so it waits out busy_timeout
// behind the owner's transaction and fails.
func TestOpenReadOnlyBesideLiveWriter(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "store.sqlite3")
	writer, err := Open(path)
	if err != nil {
		t.Fatalf("Open(writer): %v", err)
	}
	t.Cleanup(func() { _ = writer.Close() })
	if err := writer.UpsertAccount(testReadOnlyAccount("account-committed")); err != nil {
		t.Fatalf("seed account: %v", err)
	}

	tx, err := writer.db.BeginTx(ctx, nil) // BEGIN IMMEDIATE via storeDSN
	if err != nil {
		t.Fatalf("writer BeginTx(): %v", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO accounts (account_id, bridge_key, display_name, mode, enabled, config_json, created_at_ms, updated_at_ms)
		VALUES ('account-pending', 'signal', 'pending', 'live', 1, '{}', ?, ?)
	`, readOnlyFixtureTimeMS, readOnlyFixtureTimeMS); err != nil {
		_ = tx.Rollback()
		t.Fatalf("writer insert: %v", err)
	}

	started := time.Now()
	reader, _, err := OpenReadOnly(path)
	elapsed := time.Since(started)
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("OpenReadOnly beside a held write transaction: %v", err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	// busy_timeout is 5 s; attaching well under it proves the attach did
	// not wait on the write lock (1 s leaves room for a loaded -race run).
	if elapsed > time.Second {
		_ = tx.Rollback()
		t.Fatalf("OpenReadOnly took %v beside a held write transaction; it must not wait for the write lock", elapsed)
	}
	assertAccountIDs(t, reader, "account-committed")

	// Contrast: the migrating open's runMigrations blocks behind the same
	// transaction. A 100 ms busy timeout keeps the test fast.
	contrast, err := sql.Open("sqlite", fileURI(path, url.Values{
		"_pragma": {"busy_timeout(100)", "foreign_keys(ON)"},
		"_txlock": {"immediate"},
	}))
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("open contrast connection: %v", err)
	}
	migrateErr := runMigrations(ctx, contrast, embeddedMigrations)
	_ = contrast.Close()
	if code, ok := sqliteErrorCode(migrateErr); !ok || code&0xff != 5 {
		_ = tx.Rollback()
		t.Fatalf("runMigrations beside a held write transaction error = %v, want SQLITE_BUSY", migrateErr)
	}

	if err := tx.Commit(); err != nil {
		t.Fatalf("writer Commit(): %v", err)
	}
	assertAccountIDs(t, reader, "account-committed", "account-pending")

	var busy, logFrames, checkpointed int
	if err := writer.db.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &logFrames, &checkpointed); err != nil {
		t.Fatalf("writer wal_checkpoint(TRUNCATE): %v", err)
	}
	if busy != 0 {
		t.Fatalf("writer checkpoint busy=%d with an idle read-only client attached, want 0", busy)
	}
	if err := writer.UpsertAccount(testReadOnlyAccount("account-after-checkpoint")); err != nil {
		t.Fatalf("write after checkpoint: %v", err)
	}
	assertAccountIDs(t, reader, "account-after-checkpoint", "account-committed", "account-pending")
}

// TestOpenReadOnlyAfterCrashRecoversWAL models a daemon that crashed with
// commits still only in its WAL and no wal-index (-shm) on disk. The
// read-only attach rebuilds the index in -shm, serves the WAL-only rows, and
// never writes the main file (it cannot checkpoint).
func TestOpenReadOnlyAfterCrashRecoversWAL(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "store.sqlite3")
	writer, err := Open(path)
	if err != nil {
		t.Fatalf("Open(writer): %v", err)
	}
	t.Cleanup(func() { _ = writer.Close() })
	writer.db.SetMaxOpenConns(1)
	if _, err := writer.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatalf("checkpoint schema: %v", err)
	}
	if _, err := writer.db.ExecContext(ctx, `PRAGMA wal_autocheckpoint = 0`); err != nil {
		t.Fatalf("disable autocheckpoint: %v", err)
	}
	if err := writer.UpsertAccount(testReadOnlyAccount("account-in-wal-only")); err != nil {
		t.Fatalf("write WAL-only row: %v", err)
	}

	crashDir := t.TempDir()
	crashPath := filepath.Join(crashDir, "store.sqlite3")
	copyFile(t, path, crashPath)
	copyFile(t, path+"-wal", crashPath+"-wal")
	if _, err := os.Stat(crashPath + "-shm"); !os.IsNotExist(err) {
		t.Fatalf("crash copy has a -shm (stat err = %v); the fixture must not", err)
	}

	// Non-vacuity: the main file alone does not hold the row.
	mainOnlyPath := filepath.Join(t.TempDir(), "store.sqlite3")
	copyFile(t, path, mainOnlyPath)
	mainOnly, _, err := OpenReadOnly(mainOnlyPath)
	if err != nil {
		t.Fatalf("OpenReadOnly(main file only): %v", err)
	}
	assertAccountIDs(t, mainOnly)
	_ = mainOnly.Close()

	mainHashBefore := fileSHA256(t, crashPath)
	reader, _, err := OpenReadOnly(crashPath)
	if err != nil {
		t.Fatalf("OpenReadOnly(crashed store): %v", err)
	}
	assertAccountIDs(t, reader, "account-in-wal-only")
	if err := reader.Close(); err != nil {
		t.Fatalf("Close(): %v", err)
	}
	if got := fileSHA256(t, crashPath); got != mainHashBefore {
		t.Fatalf("read-only attach wrote the crashed store's main file: sha256 %s -> %s", mainHashBefore, got)
	}
}

// TestOpenReadOnlyReadOnlyDirWithoutSHM pins the one environment where a
// read-only WAL attach cannot work: SQLite must create the wal-index (-shm)
// and the directory forbids it. The error must say so, not surface a raw
// "attempt to write a readonly database (1544)". With the owner running
// (so -shm exists), the same read-only directory attaches fine.
func TestOpenReadOnlyReadOnlyDirWithoutSHM(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory permission bits do not restrict file creation on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permission bits")
	}
	dir := filepath.Join(t.TempDir(), "v2")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatalf("Mkdir(): %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	path := filepath.Join(dir, "store.sqlite3")
	owner, err := Open(path)
	if err != nil {
		t.Fatalf("Open(): %v", err)
	}
	if err := owner.Close(); err != nil {
		t.Fatalf("Close(): %v", err)
	}
	if names := dirNames(t, dir); !slices.Equal(names, []string{"store.sqlite3"}) {
		t.Fatalf("store dir after a clean close = %v, want only the main file", names)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("Chmod(0500): %v", err)
	}

	store, _, err := OpenReadOnly(path)
	if store != nil {
		_ = store.Close()
	}
	if !errors.Is(err, ErrReadOnlyAttach) {
		t.Fatalf("OpenReadOnly(read-only dir, no -shm) error = %v, want ErrReadOnlyAttach", err)
	}
	for _, want := range []string{path, "-shm", "writable"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %q", err, want)
		}
	}

	// With the owner attached, the sidecars exist and the same directory
	// serves a read-only client.
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("Chmod(0700): %v", err)
	}
	owner, err = Open(path)
	if err != nil {
		t.Fatalf("reopen owner: %v", err)
	}
	t.Cleanup(func() { _ = owner.Close() })
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("Chmod(0500): %v", err)
	}
	reader, _, err := OpenReadOnly(path)
	if err != nil {
		t.Fatalf("OpenReadOnly(read-only dir, owner attached): %v", err)
	}
	if _, err := reader.ListAccounts(); err != nil {
		t.Fatalf("ListAccounts(): %v", err)
	}
	_ = reader.Close()
}

// TestClientReadInventoryAcrossSupportedVersions pins I8: the whole client
// read inventory succeeds on a fixture at every version the client accepts,
// and at least one read fails one version below the minimum, so
// MinClientReadSchemaVersion cannot silently go stale in either direction.
func TestClientReadInventoryAcrossSupportedVersions(t *testing.T) {
	ctx := context.Background()
	for version := MinClientReadSchemaVersion; version <= LatestSchemaVersion(); version++ {
		t.Run(fmt.Sprintf("schema %d", version), func(t *testing.T) {
			path, fixture := buildClientFixture(t, version, defaultClientFixtureSpec())
			store, _, err := OpenReadOnly(path)
			if err != nil {
				t.Fatalf("OpenReadOnly(schema %d): %v", version, err)
			}
			defer store.Close()
			for _, read := range clientReadInventory {
				if _, err := read.run(ctx, store, fixture); err != nil {
					t.Errorf("%s on schema %d: %v", read.name, version, err)
				}
			}
		})
	}

	t.Run("tightness below the minimum", func(t *testing.T) {
		version := MinClientReadSchemaVersion - 1
		path, fixture := buildClientFixture(t, version, defaultClientFixtureSpec())
		// Bypass the minimum to run the reads the minimum exists to protect.
		store, _, err := openReadOnly(path, embeddedMigrations, 1)
		if err != nil {
			t.Fatalf("openReadOnly(schema %d, min 1): %v", version, err)
		}
		defer store.Close()
		var failed []string
		for _, read := range clientReadInventory {
			if _, err := read.run(ctx, store, fixture); err != nil {
				failed = append(failed, fmt.Sprintf("%s: %v", read.name, err))
			}
		}
		if len(failed) == 0 {
			t.Fatalf("every client read succeeds on schema %d; lower MinClientReadSchemaVersion to %d", version, version)
		}
		if !slices.ContainsFunc(failed, func(failure string) bool {
			return strings.HasPrefix(failure, "reactions.ReactionsForMessages") && strings.Contains(failure, "no such table: reactions")
		}) {
			t.Fatalf("schema %d failures = %v; want ReactionsForMessages to miss the reactions table", version, failed)
		}
	})
}

// TestClientReadInventoryCoversV2Read keeps clientReadInventory equal to the
// store calls internal/v2read actually makes, by parsing v2read's source. A
// new client read added there without an inventory entry fails here, so the
// per-version and never-mutates tests keep covering the real client.
func TestClientReadInventoryCoversV2Read(t *testing.T) {
	sourceDir := filepath.Join("..", "..", "v2read")
	files, err := filepath.Glob(filepath.Join(sourceDir, "*.go"))
	if err != nil || len(files) == 0 {
		t.Fatalf("glob v2read sources: %v (%d files)", err, len(files))
	}
	fields := map[string]bool{"store": true, "messages": true, "attachments": true, "outbox": true, "reactions": true}
	called := map[string]bool{}
	fileSet := token.NewFileSet()
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(fileSet, file, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		ast.Inspect(parsed, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			method, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			field, ok := method.X.(*ast.SelectorExpr)
			if !ok || !fields[field.Sel.Name] {
				return true
			}
			if receiver, ok := field.X.(*ast.Ident); ok && receiver.Name == "s" {
				called[field.Sel.Name+"."+method.Sel.Name] = true
			}
			return true
		})
	}
	var calledNames []string
	for name := range called {
		calledNames = append(calledNames, name)
	}
	sort.Strings(calledNames)
	inventory := clientReadNames()
	sort.Strings(inventory)
	if !slices.Equal(calledNames, inventory) {
		t.Fatalf("v2read store calls and clientReadInventory differ:\nv2read:    %v\ninventory: %v", calledNames, inventory)
	}
}

func TestIsReadOnlyError(t *testing.T) {
	if IsReadOnlyError(nil) || IsReadOnlyError(errors.New("attempt to write a readonly database")) {
		t.Fatal("IsReadOnlyError matched a non-SQLite error")
	}
	path, _ := buildClientFixture(t, LatestSchemaVersion(), clientFixtureSpec{accounts: 1})
	store, _, err := OpenReadOnly(path)
	if err != nil {
		t.Fatalf("OpenReadOnly(): %v", err)
	}
	defer store.Close()
	_, err = store.db.Exec(`PRAGMA user_version = 99`)
	if !IsReadOnlyError(err) {
		t.Fatalf("PRAGMA user_version on a read-only store error = %v, want IsReadOnlyError", err)
	}
	wrapped := fmt.Errorf("outer: %w", err)
	if !IsReadOnlyError(wrapped) {
		t.Fatalf("IsReadOnlyError(%v) = false for a wrapped read-only error", wrapped)
	}
	if _, err := store.db.Exec(`SELECT * FROM no_such_table`); IsReadOnlyError(err) {
		t.Fatalf("IsReadOnlyError matched %v", err)
	}
}

func TestBuildStoreAtVersionRefusesExistingFileAndBadVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.sqlite3")
	if err := os.WriteFile(path, []byte("keep me"), 0o600); err != nil {
		t.Fatalf("WriteFile(): %v", err)
	}
	if err := BuildStoreAtVersion(path, 3, nil); err == nil {
		t.Fatal("BuildStoreAtVersion over an existing file succeeded")
	}
	if got, _ := os.ReadFile(path); string(got) != "keep me" {
		t.Fatalf("BuildStoreAtVersion touched the existing file: %q", got)
	}
	for _, version := range []int{0, -1, LatestSchemaVersion() + 1} {
		fresh := filepath.Join(t.TempDir(), "store.sqlite3")
		if err := BuildStoreAtVersion(fresh, version, nil); err == nil {
			t.Fatalf("BuildStoreAtVersion(version %d) succeeded", version)
		}
		if _, err := os.Stat(fresh); !os.IsNotExist(err) {
			t.Fatalf("BuildStoreAtVersion(version %d) left a file (stat err = %v)", version, err)
		}
	}
	failing := filepath.Join(t.TempDir(), "store.sqlite3")
	if err := BuildStoreAtVersion(failing, 4, func(*Store) error { return errors.New("seed failed") }); err == nil {
		t.Fatal("BuildStoreAtVersion ignored a seed error")
	}
	if _, err := os.Stat(failing); !os.IsNotExist(err) {
		t.Fatalf("failed BuildStoreAtVersion left the file (stat err = %v)", err)
	}
	fixture := filepath.Join(t.TempDir(), "store.sqlite3")
	if err := BuildStoreAtVersion(fixture, 4, nil); err != nil {
		t.Fatalf("BuildStoreAtVersion(4): %v", err)
	}
	if got := mustFingerprint(t, fixture); got.UserVersion != 4 || len(got.Ledger) != 4 {
		t.Fatalf("fixture user_version=%d ledger=%d, want 4/4", got.UserVersion, len(got.Ledger))
	}
}

func testReadOnlyAccount(accountID string) Account {
	return Account{
		AccountID:   accountID,
		BridgeKey:   "signal",
		DisplayName: accountID,
		Mode:        AccountModeLive,
		Enabled:     true,
		ConfigJSON:  `{}`,
		CreatedAtMS: readOnlyFixtureTimeMS,
		UpdatedAtMS: readOnlyFixtureTimeMS,
	}
}

func assertAccountIDs(t *testing.T, store *Store, want ...string) {
	t.Helper()
	accounts, err := store.ListAccounts()
	if err != nil {
		t.Fatalf("ListAccounts(): %v", err)
	}
	got := make([]string, 0, len(accounts))
	for _, account := range accounts {
		got = append(got, account.AccountID)
	}
	sort.Strings(got)
	sort.Strings(want)
	if !slices.Equal(got, want) {
		t.Fatalf("accounts = %v, want %v", got, want)
	}
}

// execOnStoreFile runs statement on the store file through a plain writable
// connection that never migrates, to tamper with fixtures.
func execOnStoreFile(t *testing.T, path, statement string, arguments ...any) {
	t.Helper()
	db, err := sql.Open("sqlite", storeDSN(path))
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer db.Close()
	if _, err := db.Exec(statement, arguments...); err != nil {
		t.Fatalf("exec %q: %v", statement, err)
	}
}

func appendFutureLedgerRow(t *testing.T, path string, version int) {
	t.Helper()
	execOnStoreFile(t, path, `
		INSERT INTO schema_migrations (version, name, checksum_sha256, applied_at_ms, app_version, execution_ms)
		VALUES (?, ?, ?, 1, 'future-build', 0)
	`, version, fmt.Sprintf("future_%04d", version), strings.Repeat("f", 64))
	execOnStoreFile(t, path, fmt.Sprintf(`PRAGMA user_version = %d`, version))
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	source, err := os.Open(from)
	if err != nil {
		t.Fatalf("open %s: %v", from, err)
	}
	defer source.Close()
	target, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatalf("create %s: %v", to, err)
	}
	if _, err := io.Copy(target, source); err != nil {
		_ = target.Close()
		t.Fatalf("copy %s -> %s: %v", from, to, err)
	}
	if err := target.Close(); err != nil {
		t.Fatalf("close %s: %v", to, err)
	}
}

func copyStoreFile(t *testing.T, path string) string {
	t.Helper()
	copyPath := filepath.Join(t.TempDir(), filepath.Base(path))
	copyFile(t, path, copyPath)
	return copyPath
}

// fileSHA256 hashes a file without opening it through SQLite, so it cannot
// create sidecars or rebuild a wal-index as a side effect.
func fileSHA256(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}
