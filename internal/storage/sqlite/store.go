// Package sqlite provides the clean-slate OpenMessage SQLite store.
package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"strings"
	"sync"

	_ "modernc.org/sqlite"
)

const busyTimeoutMS = 5000

var openMu sync.Mutex

// Store owns a connection pool for an OpenMessage SQLite database.
type Store struct {
	db *sql.DB
}

// Open opens path, configures SQLite, and migrates the database to the latest
// embedded schema version.
func Open(path string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("open sqlite store: path is empty")
	}
	// In-process callers can race while a blank file is switching to WAL.
	// Serializing the short open/migrate path avoids transient SQLITE_BUSY
	// failures there; BEGIN IMMEDIATE still protects migration decisions from
	// other processes.
	openMu.Lock()
	defer openMu.Unlock()

	db, err := sql.Open("sqlite", storeDSN(path))
	if err != nil {
		return nil, fmt.Errorf("open sqlite store: %w", err)
	}

	ctx := context.Background()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open sqlite store connection: %w", err)
	}
	if err := enableWAL(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := verifyConnectionPragmas(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := runMigrations(ctx, db, embeddedMigrations); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate sqlite store: %w", err)
	}

	return &Store{db: db}, nil
}

// Close closes the store's database connections.
func (s *Store) Close() error {
	return s.db.Close()
}

// StoreInstanceID returns the stable identifier assigned when the store was
// first initialized.
func (s *Store) StoreInstanceID() (string, error) {
	var id string
	if err := s.db.QueryRowContext(
		context.Background(),
		`SELECT store_instance_id FROM store_metadata WHERE singleton = 1`,
	).Scan(&id); err != nil {
		return "", fmt.Errorf("read store instance ID: %w", err)
	}
	return id, nil
}

func storeDSN(path string) string {
	normalizedPath := strings.ReplaceAll(path, `\`, "/")
	if isWindowsAbsolutePath(normalizedPath) {
		normalizedPath = "/" + normalizedPath
	}

	query := make(url.Values)
	query.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", busyTimeoutMS))
	query.Add("_pragma", "foreign_keys(ON)")
	query.Add("_pragma", "synchronous(NORMAL)")
	// modernc.org/sqlite maps this to BEGIN IMMEDIATE for writable
	// database/sql transactions. Migration state is therefore rechecked only
	// after acquiring SQLite's write reservation.
	query.Set("_txlock", "immediate")

	return (&url.URL{
		Scheme:   "file",
		Path:     normalizedPath,
		RawQuery: query.Encode(),
	}).String()
}

func enableWAL(ctx context.Context, db *sql.DB) error {
	var journalMode string
	if err := db.QueryRowContext(ctx, `PRAGMA journal_mode = WAL`).Scan(&journalMode); err != nil {
		return fmt.Errorf("set sqlite journal mode to WAL: %w", err)
	}
	if !strings.EqualFold(journalMode, "wal") {
		return fmt.Errorf("set sqlite journal mode to WAL: got %q", journalMode)
	}
	return nil
}

func isWindowsAbsolutePath(path string) bool {
	if len(path) < 3 {
		return false
	}
	drive := path[0]
	return ((drive >= 'a' && drive <= 'z') || (drive >= 'A' && drive <= 'Z')) &&
		path[1] == ':' && path[2] == '/'
}

func verifyConnectionPragmas(ctx context.Context, db *sql.DB) error {
	var journalMode string
	if err := db.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&journalMode); err != nil {
		return fmt.Errorf("verify sqlite journal mode: %w", err)
	}
	if !strings.EqualFold(journalMode, "wal") {
		return fmt.Errorf("verify sqlite journal mode: got %q, want WAL", journalMode)
	}

	checks := []struct {
		pragma string
		want   int
	}{
		{pragma: "foreign_keys", want: 1},
		{pragma: "busy_timeout", want: busyTimeoutMS},
		{pragma: "synchronous", want: 1}, // NORMAL
	}
	for _, check := range checks {
		var got int
		if err := db.QueryRowContext(ctx, "PRAGMA "+check.pragma).Scan(&got); err != nil {
			return fmt.Errorf("verify sqlite %s pragma: %w", check.pragma, err)
		}
		if got != check.want {
			return fmt.Errorf(
				"verify sqlite %s pragma: got %d, want %d",
				check.pragma,
				got,
				check.want,
			)
		}
	}
	return nil
}
