package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
)

// ErrMigrationPending reports a store whose schema is older than this build's.
// Only a process that owns the store applies migrations, so a client refuses
// the store until the owner (the app's daemon, at its next start) upgrades it.
var ErrMigrationPending = errors.New("sqlite store has migrations this build would apply")

// OpenWithoutMigrating opens an existing store that is already at this build's
// schema, for processes that read a store another process owns: openmessage
// read and status, and the transportless MCP client. Unlike Open it never
// creates the file and never applies a migration. A migration runs inside
// BEGIN IMMEDIATE and holds SQLite's write lock until it commits, and an index
// build on a large store takes longer than the owner's busy_timeout, so a
// client that migrated underneath the running daemon would make the daemon's
// writes (inbox appends among them) fail with SQLITE_BUSY.
//
// The ledger is checked in a read-only (deferred) transaction, which never
// takes the write reservation: opening neither waits for nor blocks the
// owner's writes. A store with pending migrations returns ErrMigrationPending;
// one migrated by a newer build, or whose ledger does not match this build's
// migrations, returns the same errors Open would.
func OpenWithoutMigrating(path string) (*Store, error) {
	return openWithoutMigrating(path, embeddedMigrations)
}

func openWithoutMigrating(path string, migrations []migration) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("open sqlite store without migrating: path is empty")
	}
	if err := validateMigrationList(migrations); err != nil {
		return nil, err
	}
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("open sqlite store without migrating: %w", err)
	}

	db, err := sql.Open("sqlite", storeDSN(path))
	if err != nil {
		return nil, fmt.Errorf("open sqlite store: %w", err)
	}
	ctx := context.Background()
	if err := verifyConnectionPragmas(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := checkMigrated(ctx, db, migrations); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// checkMigrated validates the ledger against migrations without applying any.
func checkMigrated(ctx context.Context, db *sql.DB, migrations []migration) error {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return fmt.Errorf("begin read-only ledger check: %w", err)
	}
	defer tx.Rollback()

	applied, ledgerExists, err := readAppliedMigrations(ctx, tx)
	if err != nil {
		return err
	}
	if err := validateDatabaseState(ctx, tx, ledgerExists, applied, migrations); err != nil {
		return err
	}
	if len(applied) < len(migrations) {
		return fmt.Errorf(
			"%w: schema version %d, this build expects %d; the OpenMessage app applies them when it starts",
			ErrMigrationPending,
			len(applied),
			len(migrations),
		)
	}
	return nil
}
