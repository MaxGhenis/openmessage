// Package dataversion tells whether anything has committed to a SQLite
// database since the last look, for the cost of one PRAGMA.
package dataversion

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"

	_ "modernc.org/sqlite"
)

const busyTimeoutMS = 5000

// ErrClosed reports a read from a probe after Close.
var ErrClosed = errors.New("data version probe is closed")

// Probe reads SQLite's PRAGMA data_version on a read-only connection of its
// own, separate from any store's pool. SQLite changes that value on a
// connection whenever another connection, in this process or any other,
// commits a change to the database, so two equal reads mean nothing
// committed in between. A transaction that changes no rows commits nothing.
// A wal_checkpoint(TRUNCATE) moves the value without changing any data
// (PASSIVE, FULL and RESTART checkpoints do not), so callers can see a change
// with nothing behind it, never miss one.
//
// Values are only comparable while they come from the same connection. The
// probe therefore never reconnects silently: when its connection fails, that
// read returns an error and drops the connection, and the next read opens a
// fresh one. Callers must treat an error as "unknown, assume changed" and
// must not compare a value read before an error with one read after it.
type Probe struct {
	path string

	mu     sync.Mutex
	db     *sql.DB
	conn   *sql.Conn
	closed bool
}

// New returns a probe for the SQLite database file at path. It opens its
// connection on first use, so a missing or unreadable file shows up as a read
// error.
func New(path string) *Probe {
	return &Probe{path: path}
}

// Read returns the current PRAGMA data_version on the probe's connection.
func (p *Probe) Read(ctx context.Context) (int64, error) {
	if p == nil || strings.TrimSpace(p.path) == "" {
		return 0, errors.New("read data version: probe has no database path")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return 0, fmt.Errorf("read data version: %w", ErrClosed)
	}
	if p.conn == nil {
		if err := p.connectLocked(ctx); err != nil {
			return 0, fmt.Errorf("read data version of %q: %w", p.path, err)
		}
	}
	var version int64
	if err := p.conn.QueryRowContext(ctx, `PRAGMA data_version`).Scan(&version); err != nil {
		// The next read gets a new connection, whose values are not
		// comparable with this one's; the error tells the caller so.
		p.disconnectLocked()
		return 0, fmt.Errorf("read data version of %q: %w", p.path, err)
	}
	return version, nil
}

// Close releases the probe's connection. Reads after Close fail with
// ErrClosed.
func (p *Probe) Close() error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	return p.disconnectLocked()
}

func (p *Probe) connectLocked(ctx context.Context) error {
	db, err := sql.Open("sqlite", readOnlyDSN(p.path))
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	db.SetMaxOpenConns(1)
	conn, err := db.Conn(ctx)
	if err != nil {
		_ = db.Close()
		return fmt.Errorf("connect: %w", err)
	}
	p.db = db
	p.conn = conn
	return nil
}

func (p *Probe) disconnectLocked() error {
	var err error
	if p.conn != nil {
		err = p.conn.Close()
		p.conn = nil
	}
	if p.db != nil {
		err = errors.Join(err, p.db.Close())
		p.db = nil
	}
	if err != nil {
		return fmt.Errorf("close data version probe for %q: %w", p.path, err)
	}
	return nil
}

// readOnlyDSN opens path read-only, so the probe cannot write even by
// mistake. A writing probe would hide its own commits from itself.
func readOnlyDSN(path string) string {
	normalizedPath := strings.ReplaceAll(path, `\`, "/")
	if isWindowsAbsolutePath(normalizedPath) {
		normalizedPath = "/" + normalizedPath
	}
	query := make(url.Values)
	query.Set("mode", "ro")
	query.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", busyTimeoutMS))
	return (&url.URL{
		Scheme:   "file",
		Path:     normalizedPath,
		RawQuery: query.Encode(),
	}).String()
}

func isWindowsAbsolutePath(path string) bool {
	if len(path) < 3 {
		return false
	}
	drive := path[0]
	return ((drive >= 'a' && drive <= 'z') || (drive >= 'A' && drive <= 'Z')) &&
		path[1] == ':' && path[2] == '/'
}
