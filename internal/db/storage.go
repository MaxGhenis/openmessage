package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// AutoVacuum is the store's PRAGMA auto_vacuum mode.
type AutoVacuum int

const (
	AutoVacuumNone        AutoVacuum = 0
	AutoVacuumFull        AutoVacuum = 1
	AutoVacuumIncremental AutoVacuum = 2
)

func (m AutoVacuum) String() string {
	switch m {
	case AutoVacuumNone:
		return "none"
	case AutoVacuumFull:
		return "full"
	case AutoVacuumIncremental:
		return "incremental"
	}
	return fmt.Sprintf("auto_vacuum(%d)", int(m))
}

// StorageStats is how the store's main file uses its pages. Deleted rows
// leave their pages on SQLite's freelist. Later writes reuse those pages, but
// the file only shrinks when VACUUM rewrites it or, in incremental mode,
// when ReclaimFreePages hands pages back.
type StorageStats struct {
	PageSize   int64
	PageCount  int64
	FreePages  int64
	AutoVacuum AutoVacuum
}

// FileBytes is the main file's size in SQLite's page count; the WAL is apart.
func (s StorageStats) FileBytes() int64 { return s.PageSize * s.PageCount }

// FreeBytes is the space the freelist holds.
func (s StorageStats) FreeBytes() int64 { return s.PageSize * s.FreePages }

// StorageStats reads the page counts and the auto-vacuum mode in one read
// transaction, so all four come from the same snapshot.
func (s *Store) StorageStats(ctx context.Context) (StorageStats, error) {
	return StorageStatsOf(ctx, s.db)
}

// StorageStatsOf is StorageStats for a database opened some other way, such
// as read-only, which never runs New's migrations.
func StorageStatsOf(ctx context.Context, database *sql.DB) (StorageStats, error) {
	tx, err := database.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return StorageStats{}, fmt.Errorf("read storage stats: %w", err)
	}
	defer tx.Rollback()
	return storageStats(ctx, tx)
}

type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func storageStats(ctx context.Context, q rowQuerier) (StorageStats, error) {
	var stats StorageStats
	var mode int
	for _, read := range []struct {
		pragma string
		dest   any
	}{
		{"page_size", &stats.PageSize},
		{"page_count", &stats.PageCount},
		{"freelist_count", &stats.FreePages},
		{"auto_vacuum", &mode},
	} {
		if err := q.QueryRowContext(ctx, "PRAGMA "+read.pragma).Scan(read.dest); err != nil {
			return StorageStats{}, fmt.Errorf("read %s: %w", read.pragma, err)
		}
	}
	stats.AutoVacuum = AutoVacuum(mode)
	return stats, nil
}

// Checkpoint is the result row of PRAGMA wal_checkpoint.
type Checkpoint struct {
	// Busy reports that another connection kept the checkpoint from
	// finishing; a later checkpoint copies the remaining frames.
	Busy bool
	// LogFrames is the WAL's length in frames when the checkpoint ran, and
	// Checkpointed how many of them it copied into the main file.
	LogFrames    int64
	Checkpointed int64
}

// CompactReport is the store before and after Compact.
type CompactReport struct {
	Before     StorageStats
	After      StorageStats
	Checkpoint Checkpoint
}

// Compact rewrites the store with VACUUM, which leaves no free pages, and
// switches it to incremental auto-vacuum, so ReclaimFreePages can hand back
// the pages that later deletes free without another rewrite. A TRUNCATE
// checkpoint then copies the rewritten pages into the main file, which
// shrinks it to the live pages, and empties the WAL.
//
// VACUUM holds the write lock until it finishes, and it needs free disk for
// a temporary copy of the live pages and for those pages again in the WAL.
// Another process's write waits busy_timeout and then fails, so only an owner
// that has ruled out a running daemon calls it (openmessage compact). Every
// row keeps its content; VACUUM may renumber the rowids of tables without an
// INTEGER PRIMARY KEY, and nothing reads this store's rowids.
func (s *Store) Compact(ctx context.Context) (CompactReport, error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return CompactReport{}, fmt.Errorf("compact: %w", err)
	}
	defer conn.Close()

	var report CompactReport
	if report.Before, err = storageStats(ctx, conn); err != nil {
		return CompactReport{}, fmt.Errorf("compact: %w", err)
	}
	// On a store that already has tables, the new mode waits for the next
	// VACUUM on this same connection.
	if _, err := conn.ExecContext(ctx, "PRAGMA auto_vacuum=INCREMENTAL"); err != nil {
		return CompactReport{}, fmt.Errorf("compact: set incremental auto-vacuum: %w", err)
	}
	if _, err := conn.ExecContext(ctx, "VACUUM"); err != nil {
		return CompactReport{}, fmt.Errorf("compact: vacuum: %w", err)
	}
	// A reader in another process that still holds an older snapshot keeps
	// the checkpoint from finishing. Each attempt waits busy_timeout for it,
	// so a few attempts ride out a slow reader; if one is still busy, the
	// next checkpoint copies the rest and shrinks the file then.
	for attempt := 1; ; attempt++ {
		if report.Checkpoint, err = checkpoint(ctx, conn, "TRUNCATE"); err != nil {
			return CompactReport{}, fmt.Errorf("compact: checkpoint: %w", err)
		}
		if !report.Checkpoint.Busy || attempt == compactCheckpointAttempts {
			break
		}
	}
	if report.After, err = storageStats(ctx, conn); err != nil {
		return CompactReport{}, fmt.Errorf("compact: %w", err)
	}
	if report.After.AutoVacuum != AutoVacuumIncremental {
		return report, fmt.Errorf("compact: VACUUM left auto_vacuum %s, want incremental", report.After.AutoVacuum)
	}
	return report, nil
}

const compactCheckpointAttempts = 3

func checkpoint(ctx context.Context, conn *sql.Conn, mode string) (Checkpoint, error) {
	var busy int
	var result Checkpoint
	if err := conn.QueryRowContext(ctx, "PRAGMA wal_checkpoint("+mode+")").Scan(&busy, &result.LogFrames, &result.Checkpointed); err != nil {
		return Checkpoint{}, err
	}
	result.Busy = busy != 0
	return result, nil
}

// ReclaimFreePages hands up to maxPages free pages back to the filesystem
// with PRAGMA incremental_vacuum, which moves pages from the end of the file
// into free slots and truncates the file behind them, and returns how many
// pages left the freelist. Each call is one write transaction, so a caller
// bounds how long it holds the write lock with maxPages. A store that is not
// in incremental mode is left alone: only a VACUUM (Compact) can hand its
// free pages back.
func (s *Store) ReclaimFreePages(ctx context.Context, maxPages int64) (int64, error) {
	// incremental_vacuum with a count of zero or less empties the whole
	// freelist in one statement.
	if maxPages <= 0 {
		return 0, nil
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return 0, fmt.Errorf("reclaim free pages: %w", err)
	}
	defer conn.Close()

	before, err := storageStats(ctx, conn)
	if err != nil {
		return 0, fmt.Errorf("reclaim free pages: %w", err)
	}
	if before.AutoVacuum != AutoVacuumIncremental || before.FreePages == 0 {
		return 0, nil
	}
	// The pragma returns a row after each page it frees and frees the next
	// one only when stepped again. The driver's Exec steps a statement once,
	// so it would free a single page; reading every row frees them all.
	rows, err := conn.QueryContext(ctx, fmt.Sprintf("PRAGMA incremental_vacuum(%d)", maxPages))
	if err != nil {
		return 0, fmt.Errorf("reclaim free pages: %w", err)
	}
	for rows.Next() {
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("reclaim free pages: %w", err)
	}
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("reclaim free pages: %w", err)
	}
	var after int64
	if err := conn.QueryRowContext(ctx, "PRAGMA freelist_count").Scan(&after); err != nil {
		return 0, fmt.Errorf("reclaim free pages: read freelist_count: %w", err)
	}
	return before.FreePages - after, nil
}

// TableDigest summarizes one table's rows: how many there are and a sum of
// one SHA-256 per row. The sum ignores row order and rowids, so a rewrite
// that keeps every row's values keeps the digest.
type TableDigest struct {
	Rows int64
	Sum  [sha256.Size]byte
}

// ContentDigest digests every ordinary table, the search index's shadow
// tables among them; virtual tables are skipped, since their rows live in
// those shadow tables. It reads one snapshot. openmessage compact compares
// the digests of the store, its backup, and the rewritten store.
func (s *Store) ContentDigest(ctx context.Context) (map[string]TableDigest, error) {
	return ContentDigestOf(ctx, s.db)
}

// ContentDigestOf is ContentDigest for a database opened some other way, such
// as a read-only backup.
func ContentDigestOf(ctx context.Context, database *sql.DB) (map[string]TableDigest, error) {
	tx, err := database.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("digest store content: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `SELECT name FROM sqlite_schema
		WHERE type = 'table' AND sql NOT LIKE 'CREATE VIRTUAL TABLE%' ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("digest store content: list tables: %w", err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return nil, fmt.Errorf("digest store content: list tables: %w", err)
		}
		tables = append(tables, name)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("digest store content: list tables: %w", err)
	}

	digests := make(map[string]TableDigest, len(tables))
	for _, table := range tables {
		digest, err := tableDigest(ctx, tx, table)
		if err != nil {
			return nil, fmt.Errorf("digest store content: %s: %w", table, err)
		}
		digests[table] = digest
	}
	return digests, nil
}

func tableDigest(ctx context.Context, q rowQuerier, table string) (TableDigest, error) {
	rows, err := q.QueryContext(ctx, `SELECT * FROM "`+strings.ReplaceAll(table, `"`, `""`)+`"`)
	if err != nil {
		return TableDigest{}, err
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return TableDigest{}, err
	}
	values := make([]any, len(columns))
	pointers := make([]any, len(columns))
	for i := range values {
		pointers[i] = &values[i]
	}
	var digest TableDigest
	var lanes [sha256.Size / 8]uint64
	for rows.Next() {
		if err := rows.Scan(pointers...); err != nil {
			return TableDigest{}, err
		}
		hash := sha256.New()
		for _, value := range values {
			writeDigestValue(hash, value)
		}
		var rowSum [sha256.Size]byte
		hash.Sum(rowSum[:0])
		// Adding the row hashes lane by lane, rather than hashing the rows
		// in sequence, makes the digest independent of row order.
		for i := range lanes {
			lanes[i] += binary.LittleEndian.Uint64(rowSum[i*8:])
		}
		digest.Rows++
	}
	if err := rows.Err(); err != nil {
		return TableDigest{}, err
	}
	for i, lane := range lanes {
		binary.LittleEndian.PutUint64(digest.Sum[i*8:], lane)
	}
	return digest, nil
}

// writeDigestValue writes a type tag and the value, length-prefixed where
// its length varies, so no two different rows serialize alike.
func writeDigestValue(hash interface{ Write([]byte) (int, error) }, value any) {
	var scratch [9]byte
	write := func(tag byte, payload []byte) {
		scratch[0] = tag
		binary.LittleEndian.PutUint64(scratch[1:], uint64(len(payload)))
		hash.Write(scratch[:])
		hash.Write(payload)
	}
	switch v := value.(type) {
	case nil:
		write('n', nil)
	case int64:
		var payload [8]byte
		binary.LittleEndian.PutUint64(payload[:], uint64(v))
		write('i', payload[:])
	case float64:
		var payload [8]byte
		binary.LittleEndian.PutUint64(payload[:], math.Float64bits(v))
		write('f', payload[:])
	case bool:
		payload := []byte{0}
		if v {
			payload[0] = 1
		}
		write('b', payload)
	case string:
		write('t', []byte(v))
	case []byte:
		write('x', v)
	case time.Time:
		write('d', []byte(v.UTC().Format(time.RFC3339Nano)))
	default:
		write('?', []byte(fmt.Sprintf("%T:%v", v, v)))
	}
}

// ChangedTables lists, sorted, the tables whose digests differ between two
// ContentDigest results, including a table present in only one of them.
func ChangedTables(before, after map[string]TableDigest) []string {
	var changed []string
	for table, digest := range before {
		if other, ok := after[table]; !ok || other != digest {
			changed = append(changed, table)
		}
	}
	for table := range after {
		if _, ok := before[table]; !ok {
			changed = append(changed, table)
		}
	}
	sort.Strings(changed)
	return changed
}
