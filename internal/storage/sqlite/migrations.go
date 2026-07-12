package sqlite

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"runtime/debug"
	"strings"
	"time"
)

const applicationID = 0x4F4D5347 // OMSG

// ErrMigrationChecksumMismatch indicates that an applied migration no longer
// matches the SQL embedded in this build.
var ErrMigrationChecksumMismatch = errors.New("sqlite migration checksum mismatch")

type migrationArguments func() ([]any, error)

type migration struct {
	version        int
	name           string
	upSQL          string
	checksumSHA256 string
	arguments      migrationArguments
}

type appliedMigration struct {
	version        int
	name           string
	checksumSHA256 string
}

//go:embed migrations/0001_storage_shell.sql
var migration0001SQL string

var embeddedMigrations = []migration{
	newMigration(1, "storage_shell", migration0001SQL, newStorageShellArguments),
}

func newMigration(
	version int,
	name string,
	upSQL string,
	arguments migrationArguments,
) migration {
	sum := sha256.Sum256([]byte(upSQL))
	return migration{
		version:        version,
		name:           name,
		upSQL:          upSQL,
		checksumSHA256: hex.EncodeToString(sum[:]),
		arguments:      arguments,
	}
}

func runMigrations(ctx context.Context, db *sql.DB, migrations []migration) error {
	if err := validateMigrationList(migrations); err != nil {
		return err
	}

	for {
		// storeDSN configures writable transactions as BEGIN IMMEDIATE. Reading
		// and validating the ledger after BeginTx prevents concurrent Open calls
		// from both deciding that the same migration is pending.
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin migration transaction: %w", err)
		}

		applied, ledgerExists, err := readAppliedMigrations(ctx, tx)
		if err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := validateDatabaseState(ctx, tx, ledgerExists, applied, migrations); err != nil {
			_ = tx.Rollback()
			return err
		}

		if len(applied) == len(migrations) {
			if err := tx.Commit(); err != nil {
				return fmt.Errorf("commit migration verification: %w", err)
			}
			return nil
		}

		next := migrations[len(applied)]
		if err := applyMigration(ctx, tx, next); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("apply migration %04d %q: %w", next.version, next.name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %04d %q: %w", next.version, next.name, err)
		}

		if len(applied)+1 == len(migrations) {
			return nil
		}
	}
}

func validateMigrationList(migrations []migration) error {
	seenNames := make(map[string]struct{}, len(migrations))
	for i, migration := range migrations {
		wantVersion := i + 1
		if migration.version != wantVersion {
			return fmt.Errorf(
				"invalid embedded migration order: index %d has version %d, want %d",
				i,
				migration.version,
				wantVersion,
			)
		}
		if strings.TrimSpace(migration.name) == "" {
			return fmt.Errorf("invalid embedded migration %04d: name is empty", migration.version)
		}
		if _, exists := seenNames[migration.name]; exists {
			return fmt.Errorf("invalid embedded migration %04d: duplicate name %q", migration.version, migration.name)
		}
		seenNames[migration.name] = struct{}{}
		if strings.TrimSpace(migration.upSQL) == "" {
			return fmt.Errorf("invalid embedded migration %04d %q: SQL is empty", migration.version, migration.name)
		}

		sum := sha256.Sum256([]byte(migration.upSQL))
		checksum := hex.EncodeToString(sum[:])
		if migration.checksumSHA256 != checksum {
			return fmt.Errorf("invalid embedded migration %04d %q: checksum is stale", migration.version, migration.name)
		}
	}
	return nil
}

func readAppliedMigrations(
	ctx context.Context,
	tx *sql.Tx,
) ([]appliedMigration, bool, error) {
	var ledgerExists bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM sqlite_schema
			WHERE type = 'table' AND name = 'schema_migrations'
		)
	`).Scan(&ledgerExists); err != nil {
		return nil, false, fmt.Errorf("inspect migration ledger: %w", err)
	}
	if !ledgerExists {
		return nil, false, nil
	}

	rows, err := tx.QueryContext(ctx, `
		SELECT version, name, checksum_sha256
		FROM schema_migrations
		ORDER BY version
	`)
	if err != nil {
		return nil, true, fmt.Errorf("read migration ledger: %w", err)
	}
	defer rows.Close()

	var applied []appliedMigration
	for rows.Next() {
		var migration appliedMigration
		if err := rows.Scan(
			&migration.version,
			&migration.name,
			&migration.checksumSHA256,
		); err != nil {
			return nil, true, fmt.Errorf("scan migration ledger: %w", err)
		}
		applied = append(applied, migration)
	}
	if err := rows.Err(); err != nil {
		return nil, true, fmt.Errorf("iterate migration ledger: %w", err)
	}
	return applied, true, nil
}

func validateDatabaseState(
	ctx context.Context,
	tx *sql.Tx,
	ledgerExists bool,
	applied []appliedMigration,
	migrations []migration,
) error {
	if !ledgerExists {
		return validateBlankDatabase(ctx, tx)
	}
	if len(applied) == 0 {
		return fmt.Errorf("migration ledger exists without migration 0001")
	}
	if len(applied) > len(migrations) {
		return fmt.Errorf(
			"database schema version %d is newer than supported version %d",
			applied[len(applied)-1].version,
			len(migrations),
		)
	}

	for i, recorded := range applied {
		expectedVersion := i + 1
		if recorded.version != expectedVersion {
			return fmt.Errorf(
				"migration ledger is not contiguous: got version %d at position %d, want %d",
				recorded.version,
				i,
				expectedVersion,
			)
		}
		expected := migrations[i]
		if recorded.name != expected.name {
			return fmt.Errorf(
				"migration %04d name mismatch: recorded %q, embedded %q",
				recorded.version,
				recorded.name,
				expected.name,
			)
		}
		if recorded.checksumSHA256 != expected.checksumSHA256 {
			return fmt.Errorf(
				"migration %04d %q: %w: recorded %s, embedded %s",
				recorded.version,
				recorded.name,
				ErrMigrationChecksumMismatch,
				recorded.checksumSHA256,
				expected.checksumSHA256,
			)
		}
	}

	var userVersion int
	if err := tx.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&userVersion); err != nil {
		return fmt.Errorf("read sqlite user_version: %w", err)
	}
	wantVersion := applied[len(applied)-1].version
	if userVersion != wantVersion {
		return fmt.Errorf(
			"sqlite user_version mismatch: got %d, migration ledger is at %d",
			userVersion,
			wantVersion,
		)
	}

	var gotApplicationID int
	if err := tx.QueryRowContext(ctx, `PRAGMA application_id`).Scan(&gotApplicationID); err != nil {
		return fmt.Errorf("read sqlite application_id: %w", err)
	}
	if gotApplicationID != applicationID {
		return fmt.Errorf(
			"sqlite application_id mismatch: got %#x, want %#x",
			gotApplicationID,
			applicationID,
		)
	}
	return nil
}

func validateBlankDatabase(ctx context.Context, tx *sql.Tx) error {
	var objectName string
	err := tx.QueryRowContext(ctx, `
		SELECT name
		FROM sqlite_schema
		WHERE name NOT LIKE 'sqlite_%'
		ORDER BY name
		LIMIT 1
	`).Scan(&objectName)
	switch {
	case err == nil:
		return fmt.Errorf("database has object %q but no migration ledger", objectName)
	case !errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("inspect blank sqlite schema: %w", err)
	}

	var userVersion int
	if err := tx.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&userVersion); err != nil {
		return fmt.Errorf("read sqlite user_version: %w", err)
	}
	if userVersion != 0 {
		return fmt.Errorf("database has user_version %d but no migration ledger", userVersion)
	}

	var gotApplicationID int
	if err := tx.QueryRowContext(ctx, `PRAGMA application_id`).Scan(&gotApplicationID); err != nil {
		return fmt.Errorf("read sqlite application_id: %w", err)
	}
	if gotApplicationID != 0 {
		return fmt.Errorf("database has application_id %#x but no migration ledger", gotApplicationID)
	}
	return nil
}

func applyMigration(ctx context.Context, tx *sql.Tx, migration migration) error {
	var arguments []any
	if migration.arguments != nil {
		var err error
		arguments, err = migration.arguments()
		if err != nil {
			return fmt.Errorf("prepare migration arguments: %w", err)
		}
	}

	started := time.Now()
	if _, err := tx.ExecContext(ctx, migration.upSQL, arguments...); err != nil {
		return fmt.Errorf("execute SQL: %w", err)
	}
	if err := checkForeignKeys(ctx, tx); err != nil {
		return err
	}
	executionMS := time.Since(started).Milliseconds()
	appliedAtMS := time.Now().UnixMilli()
	if appliedAtMS <= 0 {
		return fmt.Errorf("current Unix time is not positive")
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO schema_migrations (
			version,
			name,
			checksum_sha256,
			applied_at_ms,
			app_version,
			execution_ms
		) VALUES (?, ?, ?, ?, ?, ?)
	`,
		migration.version,
		migration.name,
		migration.checksumSHA256,
		appliedAtMS,
		currentAppVersion(),
		executionMS,
	); err != nil {
		return fmt.Errorf("record migration ledger row: %w", err)
	}

	if _, err := tx.ExecContext(
		ctx,
		fmt.Sprintf("PRAGMA user_version = %d", migration.version),
	); err != nil {
		return fmt.Errorf("set sqlite user_version: %w", err)
	}
	return nil
}

func checkForeignKeys(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return fmt.Errorf("run sqlite foreign_key_check: %w", err)
	}

	if rows.Next() {
		var table string
		var rowID sql.NullInt64
		var parent string
		var foreignKeyID int
		if err := rows.Scan(&table, &rowID, &parent, &foreignKeyID); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan sqlite foreign_key_check result: %w", err)
		}
		_ = rows.Close()
		return fmt.Errorf(
			"sqlite foreign_key_check failed: table=%q rowid=%v parent=%q foreign_key=%d",
			table,
			rowID,
			parent,
			foreignKeyID,
		)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("iterate sqlite foreign_key_check: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close sqlite foreign_key_check: %w", err)
	}
	return nil
}

func newStorageShellArguments() ([]any, error) {
	id, err := randomStoreInstanceID()
	if err != nil {
		return nil, err
	}
	createdAtMS := time.Now().UnixMilli()
	if createdAtMS <= 0 {
		return nil, fmt.Errorf("current Unix time is not positive")
	}
	return []any{
		sql.Named("store_instance_id", id),
		sql.Named("created_at_ms", createdAtMS),
	}, nil
}

func randomStoreInstanceID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate store instance ID: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}

func currentAppVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok &&
		info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "dev"
}
