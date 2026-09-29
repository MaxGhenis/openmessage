package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"strings"
)

// MinClientReadSchemaVersion is the oldest schema version a read-only client
// serves. The client read inventory (internal/v2read) needs the reactions
// table from migration 0010. TestClientReadInventoryAcrossSupportedVersions
// runs that inventory on a fixture at every version from this one through
// LatestSchemaVersion, and checks that it fails one version below, so the
// constant cannot go stale when a client read starts needing a newer column.
const MinClientReadSchemaVersion = 10

const (
	sqliteReadOnlyCode = 8  // SQLITE_READONLY
	sqliteCantOpenCode = 14 // SQLITE_CANTOPEN
)

// LatestSchemaVersion returns the newest schema version this build migrates
// a store to.
func LatestSchemaVersion() int {
	return len(embeddedMigrations)
}

// ReadOnlyInfo describes the schema versions involved in a read-only attach.
type ReadOnlyInfo struct {
	// SchemaVersion is the store's on-disk migration-ledger version. It is
	// set on success and on ErrSchemaNewer or ErrSchemaTooOld, and zero when
	// the ledger could not be read or does not name a version.
	SchemaVersion int
	// BuildSchemaVersion is the newest version this build knows.
	BuildSchemaVersion int
}

// OpenReadOnly attaches to an existing store without migrating, creating, or
// writing it. It is the open for clients of a store another process owns:
// the transportless MCP client and openmessage read and status.
//
// The file must already exist (ErrStoreMissing otherwise; nothing is
// created). The connection uses SQLite's mode=ro plus query_only, never sets
// journal_mode, and never begins a write transaction, so it never takes
// SQLite's write reservation: it neither waits for nor blocks the owner's
// writes (like any WAL reader, a read in progress can only hold back a
// checkpoint). The only filesystem effect is SQLite creating the -wal and -shm
// sidecars a WAL reader needs when they are missing. The main database file
// is never written.
//
// Within one read snapshot it checks the store against this build's
// migrations with classifyLedger, the same rule the migrating Open applies.
// It serves a store whose version is in [MinClientReadSchemaVersion,
// LatestSchemaVersion()] and refuses anything else: ErrSchemaNewer when a
// newer binary migrated the store, ErrSchemaTooOld when the owner has not
// migrated it far enough yet, ErrLedgerMismatch for a ledger no build of this
// lineage writes. SQLite refuses every write through the returned store: a
// mutating method that reaches the database fails with an error that
// satisfies IsReadOnlyError (the repositories wrap driver errors with %w).
func OpenReadOnly(path string) (*Store, ReadOnlyInfo, error) {
	return openReadOnly(path, embeddedMigrations, MinClientReadSchemaVersion)
}

// openReadOnly is OpenReadOnly with the known migrations and minimum version
// injected, so tests can model a build that is older or newer than the store.
func openReadOnly(path string, known []migration, minVersion int) (*Store, ReadOnlyInfo, error) {
	info := ReadOnlyInfo{BuildSchemaVersion: len(known)}
	if strings.TrimSpace(path) == "" {
		return nil, info, fmt.Errorf("open sqlite store read-only: path is empty")
	}
	if err := validateMigrationList(known); err != nil {
		return nil, info, err
	}

	stat, err := os.Stat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, info, fmt.Errorf(
			"open sqlite store read-only: %w: %s (a client never creates a store; the running OpenMessage app provisions it)",
			ErrStoreMissing,
			path,
		)
	case err != nil:
		return nil, info, fmt.Errorf("open sqlite store read-only: stat %s: %w", path, err)
	case !stat.Mode().IsRegular():
		return nil, info, fmt.Errorf(
			"open sqlite store read-only: %w: %s is not a regular file",
			ErrStoreMissing,
			path,
		)
	}

	db, err := sql.Open("sqlite", readOnlyDSN(path))
	if err != nil {
		return nil, info, fmt.Errorf("open sqlite store read-only: %w", err)
	}
	ctx := context.Background()
	snapshot, err := readLedgerSnapshot(ctx, db)
	if err != nil {
		_ = db.Close()
		return nil, info, fmt.Errorf("open sqlite store read-only %s: %w", path, mapReadOnlyAttachError(err))
	}
	version, err := classifyLedger(snapshot, known, minVersion)
	info.SchemaVersion = version
	if err != nil {
		_ = db.Close()
		return nil, info, fmt.Errorf(
			"open sqlite store read-only %s: %w; %s",
			path,
			err,
			readOnlyRemediation(err),
		)
	}
	return &Store{db: db, readOnly: true, schemaVersion: version}, info, nil
}

// readOnlyDSN is the client DSN. mode=ro makes SQLite open the file
// read-only, so the driver's SQLITE_OPEN_CREATE flag cannot create a file that
// vanished after the stat (TestOpenReadOnlyMissingFileCreatesNothing);
// query_only rejects any write statement. There is no _txlock: every
// transaction is a plain deferred BEGIN, which never asks for the write
// reservation. immutable=1 must never be added: it tells SQLite the file can
// never change, which turns off the locking and change detection that keep
// reads consistent while the owner writes.
func readOnlyDSN(path string) string {
	query := make(url.Values)
	query.Set("mode", "ro")
	query.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", busyTimeoutMS))
	query.Add("_pragma", "query_only(1)")
	query.Add("_pragma", "foreign_keys(ON)")
	return fileURI(path, query)
}

// readLedgerSnapshot reads the migration ledger, user_version, and
// application_id inside one read-only transaction, so all three come from the
// same snapshot even while the owner is migrating.
func readLedgerSnapshot(ctx context.Context, db *sql.DB) (ledgerSnapshot, error) {
	if err := db.PingContext(ctx); err != nil {
		return ledgerSnapshot{}, fmt.Errorf("connect: %w", err)
	}
	// modernc.org/sqlite issues a plain BEGIN for read-only transactions.
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return ledgerSnapshot{}, fmt.Errorf("begin read transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	applied, ledgerExists, err := readAppliedMigrations(ctx, tx)
	if err != nil {
		return ledgerSnapshot{}, err
	}
	userVersion, gotApplicationID, err := readVersionPragmas(ctx, tx)
	if err != nil {
		return ledgerSnapshot{}, err
	}
	return ledgerSnapshot{
		ledgerExists:  ledgerExists,
		applied:       applied,
		userVersion:   userVersion,
		applicationID: gotApplicationID,
	}, nil
}

// mapReadOnlyAttachError turns SQLite's refusal to open the file read-only
// into an actionable error. SQLITE_READONLY at attach time means SQLite
// cannot create the WAL index (-shm) because the directory is not writable
// (observed: SQLITE_READONLY_DIRECTORY, extended code 1544); SQLITE_CANTOPEN
// means the file vanished or is unreadable.
func mapReadOnlyAttachError(err error) error {
	code, ok := sqliteErrorCode(err)
	if !ok {
		return err
	}
	switch code & 0xff {
	case sqliteReadOnlyCode:
		return fmt.Errorf(
			"%w: %w; SQLite must create the store's WAL index (-shm) beside it, so the directory must be writable by this user, or the running OpenMessage app must have created the index first",
			ErrReadOnlyAttach,
			err,
		)
	case sqliteCantOpenCode:
		return fmt.Errorf("%w: %w; check that the file exists and is readable by this user", ErrReadOnlyAttach, err)
	default:
		return err
	}
}

func readOnlyRemediation(err error) string {
	switch {
	case errors.Is(err, ErrSchemaNewer):
		return "a newer OpenMessage build migrated this store; update this openmessage binary to match the running app"
	case errors.Is(err, ErrSchemaTooOld):
		return "start the OpenMessage app, which migrates the store on startup, then retry"
	default:
		return "the store was not written by a compatible OpenMessage build; check which data directory this command reads"
	}
}
