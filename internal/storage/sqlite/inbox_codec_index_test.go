package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"testing/quick"
	"time"
)

const inboxCodecReceivedChecksum = "0ad371b2f5cb70c33e671d782ce6405ed3b55b0001c207fb0f37ea9ca73d42e5"

func TestInboxCodecReceivedMigrationAppliesToBlankAndExistingV10Database(t *testing.T) {
	t.Run("blank", func(t *testing.T) {
		store, err := Open(filepath.Join(t.TempDir(), "store.sqlite3"))
		if err != nil {
			t.Fatalf("Open(): %v", err)
		}
		t.Cleanup(func() {
			if err := store.Close(); err != nil {
				t.Errorf("Close(): %v", err)
			}
		})
		assertInboxCodecReceivedMigration(t, store.db)
	})

	t.Run("existing v10 with inbox rows", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "store.sqlite3")
		database, err := sql.Open("sqlite", storeDSN(path))
		if err != nil {
			t.Fatalf("sql.Open(): %v", err)
		}
		if err := enableWAL(context.Background(), database); err != nil {
			_ = database.Close()
			t.Fatalf("enableWAL(): %v", err)
		}
		if err := runMigrations(context.Background(), database, embeddedMigrations[:10]); err != nil {
			_ = database.Close()
			t.Fatalf("runMigrations(v10): %v", err)
		}
		v10 := &Store{db: database}
		seedMessageAccount(t, v10, "google-primary", "google_messages")
		seedMessageAccount(t, v10, "signal-primary", "signal_cli")
		var nowMS int64
		repository, err := NewMessageRepository(v10, func() time.Time { return time.UnixMilli(nowMS) })
		if err != nil {
			_ = database.Close()
			t.Fatalf("NewMessageRepository(): %v", err)
		}
		for i, codec := range []string{"google.protobuf", "signal.jsonrpc", "google.protobuf", "google.protobuf.history"} {
			nowMS = int64(5_000 - i*1_000)
			record := messageTestInbox(fmt.Sprintf("inbox-%d", i), "google-primary", fmt.Sprintf("key-%d", i), []byte("frame"))
			if codec == "signal.jsonrpc" {
				record.AccountID = "signal-primary"
			}
			record.Codec = codec
			if _, err := repository.AppendInbox(context.Background(), record); err != nil {
				_ = database.Close()
				t.Fatalf("AppendInbox(%d): %v", i, err)
			}
		}
		before := readLedgerRows(t, database)
		if len(before) != 10 {
			_ = database.Close()
			t.Fatalf("v10 ledger rows = %d, want 10", len(before))
		}
		var indexed bool
		if err := database.QueryRow(`
			SELECT EXISTS (SELECT 1 FROM sqlite_schema WHERE name = 'inbox_codec_received_idx')
		`).Scan(&indexed); err != nil || indexed {
			_ = database.Close()
			t.Fatalf("v10 has inbox_codec_received_idx = %v, %v; want absent", indexed, err)
		}
		rowsBefore := inboxRowsFingerprint(t, database)
		if err := database.Close(); err != nil {
			t.Fatalf("close v10 database: %v", err)
		}

		store, err := Open(path)
		if err != nil {
			t.Fatalf("Open(v10): %v", err)
		}
		after := readLedgerRows(t, store.db)
		if len(after) != 11 {
			t.Fatalf("migrated ledger rows = %d, want 11", len(after))
		}
		if !slices.Equal(after[:10], before) {
			t.Fatalf("migrations 0001-0010 changed:\nbefore: %+v\nafter:  %+v", before, after[:10])
		}
		assertInboxCodecReceivedMigration(t, store.db)
		if got := inboxRowsFingerprint(t, store.db); !reflect.DeepEqual(got, rowsBefore) {
			t.Fatalf("migration changed inbox rows:\nbefore: %v\nafter:  %v", rowsBefore, got)
		}
		var integrity string
		if err := store.db.QueryRow(`PRAGMA integrity_check`).Scan(&integrity); err != nil || integrity != "ok" {
			t.Fatalf("integrity_check = %q, %v; want ok", integrity, err)
		}
		if err := store.Close(); err != nil {
			t.Fatalf("Close(): %v", err)
		}

		// Reopening applies nothing more.
		reopened, err := Open(path)
		if err != nil {
			t.Fatalf("reopen: %v", err)
		}
		t.Cleanup(func() {
			if err := reopened.Close(); err != nil {
				t.Errorf("Close(): %v", err)
			}
		})
		if got := readLedgerRows(t, reopened.db); !slices.Equal(got, after) {
			t.Fatalf("reopen changed the ledger:\nbefore: %+v\nafter:  %+v", after, got)
		}
	})
}

func assertInboxCodecReceivedMigration(t *testing.T, db *sql.DB) {
	t.Helper()
	if len(embeddedMigrations) != 11 {
		t.Fatalf("embedded migrations = %d, want 11", len(embeddedMigrations))
	}
	assertPragmaInt(t, db, "user_version", 11)
	ledger := readLedgerRow(t, db, 11)
	if ledger.name != "inbox_codec_received" {
		t.Fatalf("migration 0011 name = %q, want inbox_codec_received", ledger.name)
	}
	if ledger.checksum != embeddedMigrations[10].checksumSHA256 || ledger.checksum != inboxCodecReceivedChecksum {
		t.Fatalf("migration 0011 checksum = %q, want pinned %q", ledger.checksum, inboxCodecReceivedChecksum)
	}

	rows, err := db.Query(`SELECT name FROM pragma_index_info('inbox_codec_received_idx') ORDER BY seqno`)
	if err != nil {
		t.Fatalf("read inbox_codec_received_idx columns: %v", err)
	}
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var column string
		if err := rows.Scan(&column); err != nil {
			t.Fatalf("scan index column: %v", err)
		}
		columns = append(columns, column)
	}
	if want := []string{"codec", "received_at_ms"}; !slices.Equal(columns, want) {
		t.Fatalf("inbox_codec_received_idx columns = %v, want %v", columns, want)
	}
	var partial, unique int
	if err := db.QueryRow(`
		SELECT partial, "unique" FROM pragma_index_list('inbox') WHERE name = 'inbox_codec_received_idx'
	`).Scan(&partial, &unique); err != nil {
		t.Fatalf("read inbox_codec_received_idx flags: %v", err)
	}
	if partial != 0 || unique != 0 {
		t.Fatalf("inbox_codec_received_idx partial=%d unique=%d, want a plain full index", partial, unique)
	}
}

func inboxRowsFingerprint(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`
		SELECT rowid, inbox_id, account_id, generation, dedupe_key, codec, codec_version,
			received_at_ms, hex(payload), coalesce(processed_at_ms, -1)
		FROM inbox ORDER BY rowid
	`)
	if err != nil {
		t.Fatalf("read inbox rows: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var rowID, generation, codecVersion, receivedAtMS, processedAtMS int64
		var inboxID, accountID, dedupeKey, codec, payload string
		if err := rows.Scan(&rowID, &inboxID, &accountID, &generation, &dedupeKey, &codec, &codecVersion, &receivedAtMS, &payload, &processedAtMS); err != nil {
			t.Fatalf("scan inbox row: %v", err)
		}
		out = append(out, fmt.Sprint(rowID, inboxID, accountID, generation, dedupeKey, codec, codecVersion, receivedAtMS, payload, processedAtMS))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate inbox rows: %v", err)
	}
	return out
}

// queryPlan returns the EXPLAIN QUERY PLAN detail lines for query.
func queryPlan(t *testing.T, db *sql.DB, query string, args ...any) []string {
	t.Helper()
	rows, err := db.Query("EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatalf("EXPLAIN QUERY PLAN: %v", err)
	}
	defer rows.Close()
	var details []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatalf("scan plan row: %v", err)
		}
		details = append(details, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate plan rows: %v", err)
	}
	return details
}

// TestInboxQueryPlansAvoidFullScans pins the plans that keep these reads off
// a full inbox scan. The inbox only grows, so a plan that falls back to
// scanning costs more every day; on a 1M-row copy of a real inbox the
// baseline query took about 300 ms as a scan and under 1 ms on the index.
// The store never runs ANALYZE, so these are the plans production gets.
func TestInboxQueryPlansAvoidFullScans(t *testing.T) {
	store, repository := openMessageTestRepository(t, func() time.Time { return time.UnixMilli(1_000) })
	seedMessageAccount(t, store, "google-primary", "google_messages")
	record := messageTestInbox("inbox-1", "google-primary", "key-1", []byte("frame"))
	record.Codec = "google.protobuf"
	if _, err := repository.AppendInbox(context.Background(), record); err != nil {
		t.Fatalf("AppendInbox(): %v", err)
	}

	hasLine := func(plan []string, want string) bool {
		return slices.Contains(plan, want)
	}
	scansInbox := func(plan []string) bool {
		for _, line := range plan {
			if strings.HasPrefix(line, "SCAN inbox") {
				return true
			}
		}
		return false
	}

	t.Run("receipts between, one codec", func(t *testing.T) {
		plan := queryPlan(t, store.db, inboxReceiptsBetweenQuery(1), "google.protobuf", int64(0), int64(10))
		want := "SEARCH inbox USING COVERING INDEX inbox_codec_received_idx (codec=? AND received_at_ms>? AND received_at_ms<?)"
		if !hasLine(plan, want) || len(plan) != 1 {
			t.Fatalf("plan = %q, want only %q (no scan, no sort)", plan, want)
		}
	})

	t.Run("receipts between, several codecs", func(t *testing.T) {
		plan := queryPlan(t, store.db, inboxReceiptsBetweenQuery(2), "google.protobuf", "google.alt", int64(0), int64(10))
		want := "SEARCH inbox USING COVERING INDEX inbox_codec_received_idx (codec=? AND received_at_ms>? AND received_at_ms<?)"
		if !hasLine(plan, want) || scansInbox(plan) {
			t.Fatalf("plan = %q, want %q and no inbox scan", plan, want)
		}
	})

	t.Run("receipts after row, incremental", func(t *testing.T) {
		plan := queryPlan(t, store.db, inboxReceiptsAfterRowQuery, int64(1))
		want := "SEARCH inbox USING INTEGER PRIMARY KEY (rowid>?)"
		if !hasLine(plan, want) || scansInbox(plan) {
			t.Fatalf("plan = %q, want %q and no inbox scan (an index scan here reads every row on every refresh)", plan, want)
		}
	})

	t.Run("receipts after row, full rescan", func(t *testing.T) {
		plan := queryPlan(t, store.db, inboxLatestReceiptByCodecQuery)
		for _, want := range []string{
			"SEARCH inbox USING COVERING INDEX inbox_codec_received_idx (codec>?)",
			"SEARCH inbox USING COVERING INDEX inbox_codec_received_idx (codec=?)",
		} {
			if !hasLine(plan, want) {
				t.Fatalf("plan = %q, want a line %q", plan, want)
			}
		}
		if scansInbox(plan) {
			t.Fatalf("plan = %q, want no inbox scan", plan)
		}
	})

	t.Run("dedupe lookup", func(t *testing.T) {
		plan := queryPlan(t, store.db, inboxDedupeLookupQuery, "google-primary", "key-1")
		want := "SEARCH inbox USING INDEX inbox_dedupe_uq (account_id=? AND dedupe_key=?)"
		if !hasLine(plan, want) || len(plan) != 1 {
			t.Fatalf("plan = %q, want only %q", plan, want)
		}
	})
}

// inboxActivityCase is a random inbox: frames appended in arbitrary time
// order (so rowid order and receipt order disagree), a few re-deliveries of an
// earlier dedupe key, and the arguments of one read of each kind.
type inboxActivityCase struct {
	Frames  []inboxActivityFrame
	After   int64
	Codecs  []string
	FromMS  int64
	ToMS    int64
	Repeats []int
}

type inboxActivityFrame struct {
	Codec      string
	ReceivedMS int64
}

var inboxActivityCodecs = []string{"google.protobuf", "google.protobuf.history", "signal.jsonrpc", "whatsapp.event"}

func (inboxActivityCase) Generate(r *rand.Rand, size int) reflect.Value {
	var c inboxActivityCase
	n := r.Intn(min(size, 40) + 1)
	for range n {
		c.Frames = append(c.Frames, inboxActivityFrame{
			Codec: inboxActivityCodecs[r.Intn(len(inboxActivityCodecs))],
			// A narrow time range so receipts tie often.
			ReceivedMS: 1 + r.Int63n(30),
		})
	}
	c.After = r.Int63n(int64(n)+4) - 2
	for _, codec := range inboxActivityCodecs {
		if r.Intn(2) == 0 {
			c.Codecs = append(c.Codecs, codec)
		}
	}
	c.FromMS = r.Int63n(34) - 2
	c.ToMS = r.Int63n(34) - 2
	if n > 0 {
		for range r.Intn(4) {
			c.Repeats = append(c.Repeats, r.Intn(n))
		}
	}
	return reflect.ValueOf(c)
}

type inboxReferenceRow struct {
	rowID      int64
	codec      string
	receivedMS int64
}

func referenceReceiptsAfterRow(rows []inboxReferenceRow, after int64) (map[string]int64, int64) {
	latest := map[string]int64{}
	high := after
	for _, row := range rows {
		if row.rowID <= after {
			continue
		}
		latest[row.codec] = max(latest[row.codec], row.receivedMS)
		high = max(high, row.rowID)
	}
	return latest, high
}

func referenceReceiptsBetween(rows []inboxReferenceRow, codecs []string, fromMS, toMS int64) []int64 {
	var out []int64
	for _, row := range rows {
		if slices.Contains(codecs, row.codec) && row.receivedMS >= fromMS && row.receivedMS <= toMS {
			out = append(out, row.receivedMS)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func readInboxReferenceRows(t *testing.T, db *sql.DB) []inboxReferenceRow {
	t.Helper()
	rows, err := db.Query(`SELECT rowid, codec, received_at_ms FROM inbox NOT INDEXED`)
	if err != nil {
		t.Fatalf("read reference rows: %v", err)
	}
	defer rows.Close()
	var out []inboxReferenceRow
	for rows.Next() {
		var row inboxReferenceRow
		if err := rows.Scan(&row.rowID, &row.codec, &row.receivedMS); err != nil {
			t.Fatalf("scan reference row: %v", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate reference rows: %v", err)
	}
	return out
}

func emptyIfNil[T any](values []T) []T {
	if values == nil {
		return []T{}
	}
	return values
}

// TestInboxActivityQueriesMatchReferenceProperty checks, for random inboxes,
// the properties the index and the rewritten queries must preserve:
//
//   - InboxReceiptsAfterRow(after) returns, per codec, the newest receipt of
//     the rows whose rowid exceeds after (every row when after <= 0), and the
//     largest such rowid or after itself; codecs with no such row are absent.
//   - The full-rescan form (after <= 0) and the rowid-range form agree.
//   - InboxReceiptsBetween returns exactly the receipts of the listed codecs
//     inside the inclusive window, ascending, duplicates kept.
//   - Every answer is the same with inbox_codec_received_idx dropped.
//   - Re-appending a dedupe key returns the first frame's inbox ID and adds no
//     row.
func TestInboxActivityQueriesMatchReferenceProperty(t *testing.T) {
	property := func(c inboxActivityCase) bool {
		var nowMS int64
		store, repository := openMessageTestRepository(t, func() time.Time { return time.UnixMilli(nowMS) })
		seedMessageAccount(t, store, "account-a", "google_messages")
		ctx := context.Background()
		for i, frame := range c.Frames {
			nowMS = frame.ReceivedMS
			record := messageTestInbox(fmt.Sprintf("inbox-%03d", i), "account-a", fmt.Sprintf("key-%03d", i), []byte("frame"))
			record.Codec = frame.Codec
			if _, err := repository.AppendInbox(ctx, record); err != nil {
				t.Fatalf("AppendInbox(%d): %v", i, err)
			}
		}
		for _, i := range c.Repeats {
			nowMS = 999
			record := messageTestInbox(fmt.Sprintf("redelivery-%03d", i), "account-a", fmt.Sprintf("key-%03d", i), []byte("again"))
			record.Codec = c.Frames[i].Codec
			got, err := repository.AppendInbox(ctx, record)
			if err != nil || got != fmt.Sprintf("inbox-%03d", i) {
				t.Errorf("re-append key-%03d = %q, %v; want the first frame's id inbox-%03d", i, got, err, i)
				return false
			}
		}
		reference := readInboxReferenceRows(t, store.db)
		if len(reference) != len(c.Frames) {
			t.Errorf("inbox rows = %d, want %d (re-deliveries must not add rows)", len(reference), len(c.Frames))
			return false
		}
		maxRowID := int64(0)
		for _, row := range reference {
			maxRowID = max(maxRowID, row.rowID)
		}

		type answers struct {
			After     map[int64]map[string]int64
			AfterHigh map[int64]int64
			Between   []int64
		}
		read := func(label string) (answers, bool) {
			out := answers{After: map[int64]map[string]int64{}, AfterHigh: map[int64]int64{}}
			for _, after := range []int64{c.After, -1, 0, 1, maxRowID, maxRowID + 1} {
				latest, high, err := store.InboxReceiptsAfterRow(ctx, after)
				if err != nil {
					t.Errorf("%s: InboxReceiptsAfterRow(%d): %v", label, after, err)
					return out, false
				}
				wantLatest, wantHigh := referenceReceiptsAfterRow(reference, after)
				if !reflect.DeepEqual(latest, wantLatest) || high != wantHigh {
					t.Errorf("%s: InboxReceiptsAfterRow(%d) = %v, %d; want %v, %d", label, after, latest, high, wantLatest, wantHigh)
					return out, false
				}
				out.After[after], out.AfterHigh[after] = latest, high
			}
			// The rowid-range form at 0 must agree with the full-rescan form.
			rows, err := store.db.QueryContext(ctx, inboxReceiptsAfterRowQuery, int64(0))
			if err != nil {
				t.Errorf("%s: rowid-range form: %v", label, err)
				return out, false
			}
			ranged := map[string]int64{}
			for rows.Next() {
				var codec string
				var receivedMS, rowID int64
				if err := rows.Scan(&codec, &receivedMS, &rowID); err != nil {
					t.Errorf("%s: scan rowid-range form: %v", label, err)
					rows.Close()
					return out, false
				}
				ranged[codec] = receivedMS
			}
			rows.Close()
			if !reflect.DeepEqual(ranged, out.After[0]) {
				t.Errorf("%s: rowid-range form at 0 = %v, full rescan = %v", label, ranged, out.After[0])
				return out, false
			}

			between, err := store.InboxReceiptsBetween(ctx, c.Codecs, c.FromMS, c.ToMS)
			if err != nil {
				t.Errorf("%s: InboxReceiptsBetween: %v", label, err)
				return out, false
			}
			if want := referenceReceiptsBetween(reference, c.Codecs, c.FromMS, c.ToMS); !slices.Equal(emptyIfNil(between), emptyIfNil(want)) {
				t.Errorf("%s: InboxReceiptsBetween(%v, %d, %d) = %v, want %v", label, c.Codecs, c.FromMS, c.ToMS, between, want)
				return out, false
			}
			out.Between = emptyIfNil(between)
			return out, true
		}

		indexed, ok := read("indexed")
		if !ok {
			return false
		}
		if _, err := store.db.ExecContext(ctx, `DROP INDEX inbox_codec_received_idx`); err != nil {
			t.Fatalf("drop index: %v", err)
		}
		unindexed, ok := read("without index")
		if !ok {
			return false
		}
		if !reflect.DeepEqual(indexed, unindexed) {
			t.Errorf("answers differ without the index:\nindexed:   %+v\nunindexed: %+v", indexed, unindexed)
			return false
		}
		return true
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 80, Rand: rand.New(rand.NewSource(20261009))}); err != nil {
		t.Fatal(err)
	}
}

func TestInboxActivityQueriesOnEmptyInbox(t *testing.T) {
	store, _ := openMessageTestRepository(t, func() time.Time { return time.UnixMilli(1_000) })
	for _, after := range []int64{-1, 0, 7} {
		latest, high, err := store.InboxReceiptsAfterRow(context.Background(), after)
		if err != nil || len(latest) != 0 || high != after {
			t.Fatalf("InboxReceiptsAfterRow(%d) on empty inbox = %v, %d, %v; want empty, %d", after, latest, high, err, after)
		}
	}
	got, err := store.InboxReceiptsBetween(context.Background(), []string{"google.protobuf"}, 0, 1<<62)
	if err != nil || len(got) != 0 {
		t.Fatalf("InboxReceiptsBetween on empty inbox = %v, %v; want empty", got, err)
	}
}
