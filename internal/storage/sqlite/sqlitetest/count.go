// Package sqlitetest instruments the v2 SQLite store for tests: OpenCounting
// opens a store whose every SQL statement is counted, so a test can bound the
// round trips a read path issues rather than only its result.
package sqlitetest

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

// Counter records the statements issued through one counting store.
type Counter struct {
	mu         sync.Mutex
	statements []string
}

func (c *Counter) record(query string) {
	c.mu.Lock()
	c.statements = append(c.statements, query)
	c.mu.Unlock()
}

// Count returns the number of statements issued since the last Reset.
func (c *Counter) Count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.statements)
}

// Statements returns the SQL text of each statement since the last Reset.
func (c *Counter) Statements() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.statements...)
}

// Reset forgets the statements counted so far.
func (c *Counter) Reset() {
	c.mu.Lock()
	c.statements = nil
	c.mu.Unlock()
}

var registered atomic.Int64

// OpenCounting opens the store at path (sqlite.OpenWithDriver, so migrations
// run as usual) through a driver that records every statement in the returned
// Counter. Statements run while opening are counted too; Reset before
// measuring.
func OpenCounting(path string) (*sqlite.Store, *Counter, error) {
	base, err := baseDriver()
	if err != nil {
		return nil, nil, err
	}
	counter := &Counter{}
	name := fmt.Sprintf("sqlite-counting-%d", registered.Add(1))
	sql.Register(name, countingDriver{base: base, counter: counter})
	store, err := sqlite.OpenWithDriver(name, path)
	if err != nil {
		return nil, nil, err
	}
	return store, counter, nil
}

func baseDriver() (driver.Driver, error) {
	db, err := sql.Open("sqlite", "")
	if err != nil {
		return nil, fmt.Errorf("resolve sqlite driver: %w", err)
	}
	defer db.Close()
	return db.Driver(), nil
}

type countingDriver struct {
	base    driver.Driver
	counter *Counter
}

func (d countingDriver) Open(name string) (driver.Conn, error) {
	conn, err := d.base.Open(name)
	if err != nil {
		return nil, err
	}
	return &countingConn{base: conn, counter: d.counter}, nil
}

// countingConn forwards every optional driver interface the modernc conn
// implements, counting each statement prepared, queried or executed.
type countingConn struct {
	base    driver.Conn
	counter *Counter
}

func (c *countingConn) Prepare(query string) (driver.Stmt, error) {
	c.counter.record(query)
	return c.base.Prepare(query)
}

func (c *countingConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	c.counter.record(query)
	if prepare, ok := c.base.(driver.ConnPrepareContext); ok {
		return prepare.PrepareContext(ctx, query)
	}
	return c.base.Prepare(query)
}

func (c *countingConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	queryer, ok := c.base.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	rows, err := queryer.QueryContext(ctx, query, args)
	if !errors.Is(err, driver.ErrSkip) {
		c.counter.record(query)
	}
	return rows, err
}

func (c *countingConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	execer, ok := c.base.(driver.ExecerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	result, err := execer.ExecContext(ctx, query, args)
	if !errors.Is(err, driver.ErrSkip) {
		c.counter.record(query)
	}
	return result, err
}

func (c *countingConn) Begin() (driver.Tx, error) {
	return c.base.Begin() //nolint:staticcheck // forwarded for completeness
}

func (c *countingConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if begin, ok := c.base.(driver.ConnBeginTx); ok {
		return begin.BeginTx(ctx, opts)
	}
	return c.base.Begin() //nolint:staticcheck // fallback when BeginTx is absent
}

func (c *countingConn) Close() error { return c.base.Close() }

func (c *countingConn) Ping(ctx context.Context) error {
	if pinger, ok := c.base.(driver.Pinger); ok {
		return pinger.Ping(ctx)
	}
	return nil
}

func (c *countingConn) ResetSession(ctx context.Context) error {
	if resetter, ok := c.base.(driver.SessionResetter); ok {
		return resetter.ResetSession(ctx)
	}
	return nil
}

func (c *countingConn) IsValid() bool {
	if validator, ok := c.base.(driver.Validator); ok {
		return validator.IsValid()
	}
	return true
}
