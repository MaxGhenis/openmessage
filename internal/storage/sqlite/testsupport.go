package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

// This file is test support for packages that exercise the read-only client
// attach against older or tampered stores. Nothing in production calls it.

// BuildStoreAtVersion creates a NEW store at path migrated only through
// schema version version, so tests can reproduce a store an older app wrote.
// When seed is non-nil it runs against the still-open, writable handle before
// the store is closed; repository methods whose SQL needs a newer schema fail
// there, which is the point. BuildStoreAtVersion refuses an existing path
// (the file is created with O_EXCL), so it can never touch a real store.
func BuildStoreAtVersion(path string, version int, seed func(*Store) error) (err error) {
	if version < 1 || version > len(embeddedMigrations) {
		return fmt.Errorf("build store at version %d: version must be in [1, %d]", version, len(embeddedMigrations))
	}
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("build store at version %d: %w", version, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("build store at version %d: %w", version, err)
	}
	defer func() {
		if err != nil {
			for _, suffix := range []string{"", "-wal", "-shm"} {
				_ = os.Remove(path + suffix)
			}
		}
	}()

	db, err := sql.Open("sqlite", storeDSN(path))
	if err != nil {
		return fmt.Errorf("build store at version %d: %w", version, err)
	}
	store := &Store{db: db, schemaVersion: version}
	ctx := context.Background()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return fmt.Errorf("build store at version %d: %w", version, err)
	}
	if err := enableWAL(ctx, db); err != nil {
		_ = db.Close()
		return fmt.Errorf("build store at version %d: %w", version, err)
	}
	if err := runMigrations(ctx, db, embeddedMigrations[:version]); err != nil {
		_ = db.Close()
		return fmt.Errorf("build store at version %d: %w", version, err)
	}
	if seed != nil {
		if err := seed(store); err != nil {
			_ = db.Close()
			return fmt.Errorf("build store at version %d: seed: %w", version, err)
		}
	}
	if err := db.Close(); err != nil {
		return fmt.Errorf("build store at version %d: close: %w", version, err)
	}
	return nil
}

// StoreFingerprint captures everything a read-only session must leave
// unchanged in a store.
type StoreFingerprint struct {
	// MainFileSHA256 hashes the main database file only. It is comparable
	// only at rest (no writer open), and never covers the -wal/-shm sidecars,
	// which a read-only WAL attach may create.
	MainFileSHA256 string
	UserVersion    int
	ApplicationID  int
	// SchemaCookie is PRAGMA schema_version, which SQLite bumps on every
	// schema change.
	SchemaCookie int
	// Ledger lists schema_migrations rows as "version name checksum".
	Ledger []string
	// Dump is a logical dump: sqlite_schema plus every table's rows, each
	// table's rows sorted.
	Dump string
}

// ReadStoreFingerprint fingerprints the store at path through a read-only
// connection. Tests compare fingerprints taken before and after a client
// session.
func ReadStoreFingerprint(path string) (StoreFingerprint, error) {
	var fingerprint StoreFingerprint
	file, err := os.Open(path)
	if err != nil {
		return fingerprint, fmt.Errorf("fingerprint store: %w", err)
	}
	hash := sha256.New()
	_, copyErr := io.Copy(hash, file)
	closeErr := file.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		return fingerprint, fmt.Errorf("fingerprint store: hash main file: %w", err)
	}
	fingerprint.MainFileSHA256 = hex.EncodeToString(hash.Sum(nil))

	db, err := sql.Open("sqlite", readOnlyDSN(path))
	if err != nil {
		return fingerprint, fmt.Errorf("fingerprint store: %w", err)
	}
	defer db.Close()
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return fingerprint, fmt.Errorf("fingerprint store: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, pragma := range []struct {
		name   string
		target *int
	}{
		{name: "user_version", target: &fingerprint.UserVersion},
		{name: "application_id", target: &fingerprint.ApplicationID},
		{name: "schema_version", target: &fingerprint.SchemaCookie},
	} {
		if err := tx.QueryRowContext(ctx, "PRAGMA "+pragma.name).Scan(pragma.target); err != nil {
			return fingerprint, fmt.Errorf("fingerprint store: PRAGMA %s: %w", pragma.name, err)
		}
	}

	applied, _, err := readAppliedMigrations(ctx, tx)
	if err != nil {
		return fingerprint, fmt.Errorf("fingerprint store: %w", err)
	}
	for _, row := range applied {
		fingerprint.Ledger = append(fingerprint.Ledger, fmt.Sprintf("%d %s %s", row.version, row.name, row.checksumSHA256))
	}

	dump, err := logicalDump(ctx, tx)
	if err != nil {
		return fingerprint, fmt.Errorf("fingerprint store: %w", err)
	}
	fingerprint.Dump = dump
	return fingerprint, nil
}

func logicalDump(ctx context.Context, tx *sql.Tx) (string, error) {
	var out strings.Builder
	schemaRows, err := queryStrings(ctx, tx, `
		SELECT type || ' ' || name || ' ' || tbl_name || ' ' || coalesce(sql, '')
		FROM sqlite_schema
		ORDER BY type, name
	`)
	if err != nil {
		return "", fmt.Errorf("dump sqlite_schema: %w", err)
	}
	for _, row := range schemaRows {
		out.WriteString(row)
		out.WriteByte('\n')
	}

	tables, err := queryStrings(ctx, tx, `
		SELECT name
		FROM sqlite_schema
		WHERE type = 'table' AND name NOT LIKE 'sqlite_%'
		ORDER BY name
	`)
	if err != nil {
		return "", fmt.Errorf("list tables: %w", err)
	}
	for _, table := range tables {
		rows, err := dumpTable(ctx, tx, table)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&out, "== %s (%d rows)\n", table, len(rows))
		for _, row := range rows {
			out.WriteString(row)
			out.WriteByte('\n')
		}
	}
	return out.String(), nil
}

func dumpTable(ctx context.Context, tx *sql.Tx, table string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT * FROM "`+strings.ReplaceAll(table, `"`, `""`)+`"`)
	if err != nil {
		return nil, fmt.Errorf("dump table %s: %w", table, err)
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return nil, fmt.Errorf("dump table %s: %w", table, err)
	}
	var dumped []string
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			return nil, fmt.Errorf("dump table %s: %w", table, err)
		}
		fields := make([]string, len(values))
		for i, value := range values {
			fields[i] = fmt.Sprintf("%s=%#v", columns[i], value)
		}
		dumped = append(dumped, strings.Join(fields, " "))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("dump table %s: %w", table, err)
	}
	sort.Strings(dumped)
	return dumped, nil
}

func queryStrings(ctx context.Context, tx *sql.Tx, query string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var values []string
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, rows.Err()
}
