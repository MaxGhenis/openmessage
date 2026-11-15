package db

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/quick"
)

func TestWithJournalSizeLimit(t *testing.T) {
	limit := fmt.Sprintf("_pragma=journal_size_limit(%d)", JournalSizeLimitBytes)
	for dsn, want := range map[string]string{
		"":                                   "",
		":memory:":                           ":memory:?" + limit,
		"/tmp/messages.db":                   "/tmp/messages.db?" + limit,
		"/tmp/messages.db?_txlock=immediate": "/tmp/messages.db?_txlock=immediate&" + limit,
		"file:messages.db?_pragma=journal_size_limit(0)": "file:messages.db?_pragma=journal_size_limit(0)",
	} {
		if got := withJournalSizeLimit(dsn); got != want {
			t.Errorf("withJournalSizeLimit(%q) = %q, want %q", dsn, got, want)
		}
	}
}

// With no idle connections kept, each query runs on a connection the pool
// has just opened, the way a dropped connection gets replaced. Every one must
// carry the limit: it is per connection, and the connection that writes first
// after a checkpoint is the one that truncates the WAL.
func TestEveryLegacyConnectionCapsTheWAL(t *testing.T) {
	store, err := New(filepath.Join(t.TempDir(), "messages.db"))
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	defer store.Close()
	store.db.SetMaxIdleConns(0)
	for i := 0; i < 3; i++ {
		var limit int64
		if err := store.db.QueryRow(`PRAGMA journal_size_limit`).Scan(&limit); err != nil {
			t.Fatalf("query %d: read journal_size_limit: %v", i, err)
		}
		if limit != JournalSizeLimitBytes {
			t.Errorf("query %d: journal_size_limit = %d, want %d", i, limit, JournalSizeLimitBytes)
		}
	}
	if closed := store.db.Stats().MaxIdleClosed; closed < 3 {
		t.Fatalf("pool closed %d connections after use, want >= 3: the queries did not each get a new connection", closed)
	}
}

// TestLegacyCheckpointAfterLargeWriteLeavesWALWithinLimit writes one
// transaction larger than the limit, checkpoints, and writes again: the WAL
// must end at or under the limit. The same steps on a connection without the
// limit leave the WAL at the large transaction's size.
func TestLegacyCheckpointAfterLargeWriteLeavesWALWithinLimit(t *testing.T) {
	if raceDetectorEnabled {
		// Two writes past 64 MiB take seconds here and about ten times as
		// long under -race; the test checks file sizes, not concurrency, and
		// the plain test job runs it.
		t.Skip("writes over 128 MiB; covered by the plain test job")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "messages.db")
	store, err := New(path)
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	defer store.Close()
	if got := walAfterLargeWriteAndCheckpoint(t, store.db, path); got > JournalSizeLimitBytes {
		t.Fatalf("WAL after checkpoint and next write = %d bytes, want <= %d", got, JournalSizeLimitBytes)
	}

	controlPath := filepath.Join(dir, "control.db")
	control, err := sql.Open("sqlite", controlPath)
	if err != nil {
		t.Fatalf("open control: %v", err)
	}
	defer control.Close()
	control.SetMaxOpenConns(1)
	if _, err := control.Exec(`PRAGMA journal_mode=WAL`); err != nil {
		t.Fatalf("control WAL: %v", err)
	}
	if got := walAfterLargeWriteAndCheckpoint(t, control, controlPath); got <= JournalSizeLimitBytes {
		t.Fatalf("control WAL without journal_size_limit = %d bytes, want > %d (the test no longer exercises a WAL the limit has to shrink)", got, JournalSizeLimitBytes)
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
	for written := 0; written < JournalSizeLimitBytes+8<<20; written += blob {
		if _, err := tx.ExecContext(ctx, `INSERT INTO wal_probe (payload) VALUES (zeroblob(?))`, blob); err != nil {
			tx.Rollback()
			t.Fatalf("large write: %v", err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit large write: %v", err)
	}
	if size := fileSizeOf(t, path+"-wal"); size <= JournalSizeLimitBytes {
		t.Fatalf("WAL after the large write = %d bytes, want > %d", size, JournalSizeLimitBytes)
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
	return fileSizeOf(t, path+"-wal")
}

func fileSizeOf(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return info.Size()
}

func TestNewCreatesStoresWithIncrementalAutoVacuum(t *testing.T) {
	store, err := New(filepath.Join(t.TempDir(), "messages.db"))
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	defer store.Close()
	stats, err := store.StorageStats(context.Background())
	if err != nil {
		t.Fatalf("StorageStats(): %v", err)
	}
	if stats.AutoVacuum != AutoVacuumIncremental {
		t.Fatalf("new store auto_vacuum = %s, want incremental", stats.AutoVacuum)
	}
}

// Opening a store never rewrites it: one created without auto-vacuum keeps
// that mode until Compact.
func TestNewKeepsAnExistingStoresAutoVacuumMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "messages.db")
	store := openStoreWithoutAutoVacuum(t, path)
	defer store.Close()
	stats, err := store.StorageStats(context.Background())
	if err != nil {
		t.Fatalf("StorageStats(): %v", err)
	}
	if stats.AutoVacuum != AutoVacuumNone {
		t.Fatalf("existing store auto_vacuum after New = %s, want none", stats.AutoVacuum)
	}
}

// openStoreWithoutAutoVacuum creates path the way stores were created before
// New chose incremental auto-vacuum, then opens it with New.
func openStoreWithoutAutoVacuum(t *testing.T, path string) *Store {
	t.Helper()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw store: %v", err)
	}
	if _, err := raw.Exec(`PRAGMA journal_mode=WAL; CREATE TABLE created_before_new (x)`); err != nil {
		raw.Close()
		t.Fatalf("create raw store: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw store: %v", err)
	}
	store, err := New(path)
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	return store
}

// fillAndThin saves count messages with bodies of about bodyBytes through
// the store's own write path, which keeps the search index in step, then
// deletes all but every keepEvery-th one. It returns the IDs it kept.
func fillAndThin(t *testing.T, store *Store, count, bodyBytes, keepEvery int) []string {
	t.Helper()
	if err := store.UpsertConversation(&Conversation{ConversationID: "c1", Name: "c1"}); err != nil {
		t.Fatalf("UpsertConversation(): %v", err)
	}
	words := []string{"harbor", "lantern", "meadow", "quartz", "saffron", "tundra", "velvet", "zephyr"}
	r := rand.New(rand.NewSource(int64(count*31 + bodyBytes)))
	var kept []string
	for i := 0; i < count; i++ {
		var body strings.Builder
		fmt.Fprintf(&body, "note%05d", i)
		for body.Len() < bodyBytes {
			body.WriteByte(' ')
			body.WriteString(words[r.Intn(len(words))])
		}
		id := fmt.Sprintf("m%05d", i)
		if err := store.UpsertMessage(&Message{
			MessageID:      id,
			ConversationID: "c1",
			Body:           body.String(),
			TimestampMS:    int64(1_700_000_000_000 + i),
			SourcePlatform: "sms",
		}); err != nil {
			t.Fatalf("UpsertMessage(%s): %v", id, err)
		}
		if i%keepEvery == 0 {
			kept = append(kept, id)
		}
	}
	for i := 0; i < count; i++ {
		if i%keepEvery == 0 {
			continue
		}
		if err := store.DeleteMessageByID(fmt.Sprintf("m%05d", i)); err != nil {
			t.Fatalf("DeleteMessageByID(): %v", err)
		}
	}
	return kept
}

// Compact leaves no free pages, switches the store to incremental mode,
// shrinks the main file to the pages it reports, empties the WAL, and keeps
// every row of every table, the search index included.
func TestCompactDropsFreePagesAndKeepsEveryRow(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "messages.db")
	store := openStoreWithoutAutoVacuum(t, path)
	defer func() { store.Close() }()
	count := 400
	if raceDetectorEnabled {
		count = 80
	}
	kept := fillAndThin(t, store, count, 1500, 10)

	before, err := store.StorageStats(ctx)
	if err != nil {
		t.Fatalf("StorageStats(): %v", err)
	}
	if before.AutoVacuum != AutoVacuumNone || before.FreePages == 0 {
		t.Fatalf("setup: auto_vacuum %s with %d free pages, want none with some free", before.AutoVacuum, before.FreePages)
	}
	digestBefore, err := store.ContentDigest(ctx)
	if err != nil {
		t.Fatalf("ContentDigest(): %v", err)
	}

	report, err := store.Compact(ctx)
	if err != nil {
		t.Fatalf("Compact(): %v", err)
	}
	if report.Before != before {
		t.Errorf("report.Before = %+v, want %+v", report.Before, before)
	}
	after := report.After
	if after.FreePages != 0 || after.AutoVacuum != AutoVacuumIncremental {
		t.Errorf("after Compact: auto_vacuum %s with %d free pages, want incremental with none", after.AutoVacuum, after.FreePages)
	}
	if after.PageCount >= before.PageCount {
		t.Errorf("after Compact: %d pages, want fewer than %d", after.PageCount, before.PageCount)
	}
	if report.Checkpoint.Busy || report.Checkpoint.LogFrames != report.Checkpoint.Checkpointed {
		t.Errorf("checkpoint = %+v, want every frame copied", report.Checkpoint)
	}
	if size := fileSizeOf(t, path); size != after.FileBytes() {
		t.Errorf("main file = %d bytes, want the %d bytes the store reports", size, after.FileBytes())
	}
	if size := fileSizeOf(t, path+"-wal"); size != 0 {
		t.Errorf("WAL after Compact = %d bytes, want 0", size)
	}
	assertSameContent(t, store, digestBefore)

	results, err := store.SearchMessages("note00010", "", 10)
	if err != nil {
		t.Fatalf("SearchMessages(): %v", err)
	}
	if len(results) != 1 || results[0].MessageID != kept[1] {
		t.Fatalf("search after Compact = %d results, want the one kept message %s", len(results), kept[1])
	}
	if results, err := store.SearchMessages("note00011", "", 10); err != nil || len(results) != 0 {
		t.Fatalf("search for a deleted message after Compact = %d results (err %v), want none", len(results), err)
	}

	// A second run and a reopen change nothing.
	again, err := store.Compact(ctx)
	if err != nil {
		t.Fatalf("second Compact(): %v", err)
	}
	if again.After.FreePages != 0 || again.After.AutoVacuum != AutoVacuumIncremental {
		t.Errorf("second Compact left %+v", again.After)
	}
	assertSameContent(t, store, digestBefore)
	if err := store.Close(); err != nil {
		t.Fatalf("Close(): %v", err)
	}
	if store, err = New(path); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if stats, err := store.StorageStats(ctx); err != nil || stats.AutoVacuum != AutoVacuumIncremental {
		t.Fatalf("after reopen: %+v (err %v), want incremental", stats, err)
	}
	assertSameContent(t, store, digestBefore)
}

func assertSameContent(t *testing.T, store *Store, want map[string]TableDigest) {
	t.Helper()
	got, err := store.ContentDigest(context.Background())
	if err != nil {
		t.Fatalf("ContentDigest(): %v", err)
	}
	if changed := ChangedTables(want, got); len(changed) != 0 {
		t.Fatalf("tables changed: %v", changed)
	}
}

// reclaimCase is a table of blobs, some of them deleted, and a sequence of
// incremental_vacuum budgets, zero and negative ones among them.
type reclaimCase struct {
	Sizes  []int
	Delete []bool
	Steps  []int64
}

func (reclaimCase) Generate(r *rand.Rand, _ int) reflect.Value {
	// Cases under -race hold fewer rows: each row costs the race job far
	// more time, and the properties check data, not concurrency.
	maxRows := 120
	if raceDetectorEnabled {
		maxRows = 30
	}
	c := reclaimCase{
		Sizes:  make([]int, 1+r.Intn(maxRows)),
		Delete: make([]bool, 0),
		Steps:  make([]int64, 1+r.Intn(4)),
	}
	for i := range c.Sizes {
		// Up to 20 KB, so some rows spill onto overflow pages.
		c.Sizes[i] = 1 + r.Intn(20_000)
		c.Delete = append(c.Delete, r.Intn(3) != 0)
	}
	for i := range c.Steps {
		c.Steps[i] = int64(r.Intn(403) - 2)
	}
	return reflect.ValueOf(c)
}

// For any table of rows and any deletes, ReclaimFreePages(k) frees exactly
// min(k, free pages) pages for k > 0 and none for k <= 0, the file loses at
// least that many pages (more when a pointer-map page goes with them), and no
// row changes.
func TestReclaimFreePagesFreesExactlyItsBudgetAndKeepsEveryRow(t *testing.T) {
	ctx := context.Background()
	property := func(c reclaimCase) bool {
		store, err := New(filepath.Join(t.TempDir(), "messages.db"))
		if err != nil {
			t.Fatalf("New(): %v", err)
		}
		defer store.Close()
		writeBlobs(t, store, c.Sizes, c.Delete)
		digest, err := store.ContentDigest(ctx)
		if err != nil {
			t.Fatalf("ContentDigest(): %v", err)
		}
		for i, budget := range c.Steps {
			before, err := store.StorageStats(ctx)
			if err != nil {
				t.Fatalf("StorageStats(): %v", err)
			}
			freed, err := store.ReclaimFreePages(ctx, budget)
			if err != nil {
				t.Fatalf("step %d: ReclaimFreePages(%d): %v", i, budget, err)
			}
			after, err := store.StorageStats(ctx)
			if err != nil {
				t.Fatalf("StorageStats(): %v", err)
			}
			want := int64(0)
			if budget > 0 {
				want = min(budget, before.FreePages)
			}
			if freed != want || after.FreePages != before.FreePages-freed {
				t.Logf("step %d budget %d: freed %d, free pages %d -> %d; want %d freed", i, budget, freed, before.FreePages, after.FreePages, want)
				return false
			}
			if after.PageCount > before.PageCount-freed || (freed == 0 && after.PageCount != before.PageCount) {
				t.Logf("step %d budget %d: freed %d but pages went %d -> %d", i, budget, freed, before.PageCount, after.PageCount)
				return false
			}
			if got, err := store.ContentDigest(ctx); err != nil || len(ChangedTables(digest, got)) != 0 {
				t.Logf("step %d budget %d: tables changed %v (err %v)", i, budget, ChangedTables(digest, got), err)
				return false
			}
		}
		return quickCheck(t, store) == "ok"
	}
	cases := 80
	if raceDetectorEnabled {
		cases = 12
	}
	if err := quick.Check(property, &quick.Config{MaxCount: cases, Rand: rand.New(rand.NewSource(20261104))}); err != nil {
		t.Fatal(err)
	}
}

// A store without auto-vacuum keeps its free pages: only Compact can return
// them.
func TestReclaimFreePagesLeavesAStoreWithoutAutoVacuumAlone(t *testing.T) {
	ctx := context.Background()
	store := openStoreWithoutAutoVacuum(t, filepath.Join(t.TempDir(), "messages.db"))
	defer store.Close()
	writeBlobs(t, store, []int{9000, 9000, 9000, 9000}, []bool{true, true, true, false})
	before, err := store.StorageStats(ctx)
	if err != nil {
		t.Fatalf("StorageStats(): %v", err)
	}
	if before.FreePages == 0 {
		t.Fatal("setup left no free pages")
	}
	freed, err := store.ReclaimFreePages(ctx, 1000)
	if err != nil {
		t.Fatalf("ReclaimFreePages(): %v", err)
	}
	after, err := store.StorageStats(ctx)
	if err != nil {
		t.Fatalf("StorageStats(): %v", err)
	}
	if freed != 0 || after != before {
		t.Fatalf("ReclaimFreePages on a store without auto-vacuum freed %d pages, stats %+v -> %+v", freed, before, after)
	}
}

// Two ways to return every free page, a full rewrite and incremental steps,
// end with the same rows and no free pages.
func TestCompactAndStepwiseReclaimAgree(t *testing.T) {
	ctx := context.Background()
	property := func(c reclaimCase) bool {
		dir := t.TempDir()
		var results []map[string]TableDigest
		for _, name := range []string{"compact.db", "steps.db"} {
			store, err := New(filepath.Join(dir, name))
			if err != nil {
				t.Fatalf("New(): %v", err)
			}
			writeBlobs(t, store, c.Sizes, c.Delete)
			if name == "compact.db" {
				_, err = store.Compact(ctx)
			} else {
				for err == nil {
					var freed int64
					if freed, err = store.ReclaimFreePages(ctx, 7); freed == 0 {
						break
					}
				}
			}
			if err != nil {
				store.Close()
				t.Fatalf("%s: %v", name, err)
			}
			stats, err := store.StorageStats(ctx)
			if err != nil {
				store.Close()
				t.Fatalf("StorageStats(): %v", err)
			}
			digest, err := store.ContentDigest(ctx)
			store.Close()
			if err != nil {
				t.Fatalf("ContentDigest(): %v", err)
			}
			if stats.FreePages != 0 {
				t.Logf("%s: %d free pages left", name, stats.FreePages)
				return false
			}
			results = append(results, digest)
		}
		if changed := ChangedTables(results[0], results[1]); len(changed) != 0 {
			t.Logf("compact and stepwise reclaim disagree on %v", changed)
			return false
		}
		return true
	}
	cases := 40
	if raceDetectorEnabled {
		cases = 8
	}
	if err := quick.Check(property, &quick.Config{MaxCount: cases, Rand: rand.New(rand.NewSource(1104))}); err != nil {
		t.Fatal(err)
	}
}

// The digest ignores row order and rowids but not any value.
func TestContentDigestSeesValuesNotOrder(t *testing.T) {
	ctx := context.Background()
	digestOf := func(rows []string) map[string]TableDigest {
		t.Helper()
		store, err := New(filepath.Join(t.TempDir(), "messages.db"))
		if err != nil {
			t.Fatalf("New(): %v", err)
		}
		defer store.Close()
		if _, err := store.db.Exec(`CREATE TABLE notes (body TEXT, n INTEGER, blob BLOB)`); err != nil {
			t.Fatalf("create table: %v", err)
		}
		for i, body := range rows {
			if _, err := store.db.Exec(`INSERT INTO notes VALUES (?, ?, ?)`, body, len(body), []byte(body)); err != nil {
				t.Fatalf("insert %d: %v", i, err)
			}
		}
		digest, err := store.ContentDigest(ctx)
		if err != nil {
			t.Fatalf("ContentDigest(): %v", err)
		}
		return digest
	}
	base := digestOf([]string{"alpha", "beta", "gamma", "beta"})
	if changed := ChangedTables(base, digestOf([]string{"beta", "gamma", "beta", "alpha"})); len(changed) != 0 {
		t.Errorf("reordered rows changed %v", changed)
	}
	if changed := ChangedTables(base, digestOf([]string{"alpha", "beta", "gamma", "betb"})); !reflect.DeepEqual(changed, []string{"notes"}) {
		t.Errorf("one changed value changed %v, want [notes]", changed)
	}
	if changed := ChangedTables(base, digestOf([]string{"alpha", "beta", "gamma"})); !reflect.DeepEqual(changed, []string{"notes"}) {
		t.Errorf("a dropped duplicate row changed %v, want [notes]", changed)
	}
}

// writeBlobs creates a table of rows with the given payload sizes, then
// deletes the rows marked in del, each in one transaction.
func writeBlobs(t *testing.T, store *Store, sizes []int, del []bool) {
	t.Helper()
	tx, err := store.db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS blobs (id INTEGER PRIMARY KEY, payload BLOB)`); err != nil {
		tx.Rollback()
		t.Fatalf("create blobs: %v", err)
	}
	for i, size := range sizes {
		// The bytes follow from the row alone, so two stores given the same
		// case hold the same rows.
		payload := make([]byte, size)
		for j := range payload {
			payload[j] = byte(i*131 + j*7)
		}
		if _, err := tx.Exec(`INSERT INTO blobs (id, payload) VALUES (?, ?)`, i, payload); err != nil {
			tx.Rollback()
			t.Fatalf("insert blob %d: %v", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit blobs: %v", err)
	}
	tx, err = store.db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	for i, remove := range del {
		if !remove {
			continue
		}
		if _, err := tx.Exec(`DELETE FROM blobs WHERE id = ?`, i); err != nil {
			tx.Rollback()
			t.Fatalf("delete blob %d: %v", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit deletes: %v", err)
	}
}

func quickCheck(t *testing.T, store *Store) string {
	t.Helper()
	var result string
	if err := store.db.QueryRow(`PRAGMA quick_check`).Scan(&result); err != nil {
		t.Fatalf("quick_check: %v", err)
	}
	return result
}
