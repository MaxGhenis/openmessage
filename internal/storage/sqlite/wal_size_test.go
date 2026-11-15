package sqlite

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

// TestEveryStoreConnectionCapsTheWAL holds several connections at once so
// the pool must open each of them, and checks that every one carries the
// limit: journal_size_limit is per connection, and the connection that
// writes first after a checkpoint is the one that truncates the WAL.
func TestEveryStoreConnectionCapsTheWAL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.sqlite3")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open(): %v", err)
	}
	defer store.Close()
	client, err := OpenWithoutMigrating(path)
	if err != nil {
		t.Fatalf("OpenWithoutMigrating(): %v", err)
	}
	defer client.Close()

	ctx := context.Background()
	for name, db := range map[string]*sql.DB{"Open": store.db, "OpenWithoutMigrating": client.db} {
		var conns []*sql.Conn
		for i := 0; i < 4; i++ {
			conn, err := db.Conn(ctx)
			if err != nil {
				t.Fatalf("%s: Conn(%d): %v", name, i, err)
			}
			conns = append(conns, conn)
		}
		for i, conn := range conns {
			var limit int64
			if err := conn.QueryRowContext(ctx, `PRAGMA journal_size_limit`).Scan(&limit); err != nil {
				t.Fatalf("%s: connection %d: read journal_size_limit: %v", name, i, err)
			}
			if limit != journalSizeLimitBytes {
				t.Errorf("%s: connection %d: journal_size_limit = %d, want %d", name, i, limit, journalSizeLimitBytes)
			}
		}
		for _, conn := range conns {
			conn.Close()
		}
	}
}

// TestCheckpointAfterLargeWriteLeavesWALWithinLimit writes one transaction
// larger than the limit, checkpoints, and writes again: the WAL must end at
// or under the limit. The same steps on a connection without the limit leave
// the WAL at the large transaction's size, so the pragma is what shrinks it.
func TestCheckpointAfterLargeWriteLeavesWALWithinLimit(t *testing.T) {
	if raceDetectorEnabled {
		// Two writes past 64 MiB take seconds here and about ten times as
		// long under -race; the test checks file sizes, not concurrency, and
		// the plain test job runs it.
		t.Skip("writes over 128 MiB; covered by the plain test job")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "store.sqlite3")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open(): %v", err)
	}
	defer store.Close()
	if got := walAfterLargeWriteAndCheckpoint(t, store.db, path); got > journalSizeLimitBytes {
		t.Fatalf("WAL after checkpoint and next write = %d bytes, want <= %d", got, journalSizeLimitBytes)
	}

	controlPath := filepath.Join(dir, "control.sqlite3")
	control, err := sql.Open("sqlite", controlPath)
	if err != nil {
		t.Fatalf("open control: %v", err)
	}
	defer control.Close()
	control.SetMaxOpenConns(1)
	if _, err := control.Exec(`PRAGMA journal_mode = WAL`); err != nil {
		t.Fatalf("control WAL: %v", err)
	}
	if got := walAfterLargeWriteAndCheckpoint(t, control, controlPath); got <= journalSizeLimitBytes {
		t.Fatalf("control WAL without journal_size_limit = %d bytes, want > %d (the test no longer exercises a WAL the limit has to shrink)", got, journalSizeLimitBytes)
	}
}

// walAfterLargeWriteAndCheckpoint commits one transaction 8 MiB larger than
// the limit, runs a checkpoint that must copy every frame back, commits one
// small row, and returns the WAL file's size.
func walAfterLargeWriteAndCheckpoint(t *testing.T, db *sql.DB, path string) int64 {
	t.Helper()
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("Conn(): %v", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `CREATE TABLE wal_probe (payload BLOB)`); err != nil {
		t.Fatalf("create probe table: %v", err)
	}
	const blob = 1 << 20
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin large write: %v", err)
	}
	for written := 0; written < journalSizeLimitBytes+8<<20; written += blob {
		if _, err := tx.ExecContext(ctx, `INSERT INTO wal_probe (payload) VALUES (zeroblob(?))`, blob); err != nil {
			tx.Rollback()
			t.Fatalf("large write: %v", err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit large write: %v", err)
	}
	if size := fileSize(t, path+"-wal"); size <= journalSizeLimitBytes {
		t.Fatalf("WAL after the large write = %d bytes, want > %d", size, journalSizeLimitBytes)
	}

	var busy, logFrames, checkpointed int64
	if err := conn.QueryRowContext(ctx, `PRAGMA wal_checkpoint(PASSIVE)`).Scan(&busy, &logFrames, &checkpointed); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if busy != 0 || logFrames != checkpointed {
		t.Fatalf("checkpoint = busy %d, %d of %d frames copied; want every frame copied", busy, checkpointed, logFrames)
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO wal_probe (payload) VALUES (x'00')`); err != nil {
		t.Fatalf("write after checkpoint: %v", err)
	}
	return fileSize(t, path+"-wal")
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return info.Size()
}
