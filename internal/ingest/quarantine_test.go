package ingest

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

func TestRetryTransientBoundsSQLiteLockRetriesAndNeverRetriesQuarantine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "locked.sqlite3")
	dsn := "file:" + path + "?_pragma=busy_timeout(0)"
	locker, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("sql.Open(locker): %v", err)
	}
	t.Cleanup(func() { _ = locker.Close() })
	contender, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("sql.Open(contender): %v", err)
	}
	t.Cleanup(func() { _ = contender.Close() })
	if _, err := locker.Exec(`CREATE TABLE retry_probe (value INTEGER)`); err != nil {
		t.Fatalf("create retry probe: %v", err)
	}
	connection, err := locker.Conn(context.Background())
	if err != nil {
		t.Fatalf("locker.Conn(): %v", err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	if _, err := connection.ExecContext(context.Background(), `BEGIN IMMEDIATE`); err != nil {
		t.Fatalf("BEGIN IMMEDIATE: %v", err)
	}
	t.Cleanup(func() { _, _ = connection.ExecContext(context.Background(), `ROLLBACK`) })

	_, lockedErr := contender.Exec(`INSERT INTO retry_probe (value) VALUES (1)`)
	if lockedErr == nil || !isTransientDBError(lockedErr) {
		t.Fatalf("contending insert error = %v, want transient SQLite busy/locked", lockedErr)
	}

	attempts := 0
	err = retryTransient(context.Background(), func() error {
		attempts++
		return lockedErr
	})
	var exhausted transientExhaustedError
	if !errors.As(err, &exhausted) || attempts != maxTransientRetries+1 {
		t.Fatalf("retryTransient(locked) = (%d attempts, %v), want %d attempts and exhaustion", attempts, err, maxTransientRetries+1)
	}

	attempts = 0
	err = retryTransient(context.Background(), func() error {
		attempts++
		return quarantine(lockedErr)
	})
	var deterministic quarantineError
	if !errors.As(err, &deterministic) || attempts != 1 {
		t.Fatalf("retryTransient(quarantined lock error) = (%d attempts, %v), want one deterministic attempt", attempts, err)
	}
}
