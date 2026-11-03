package db

import (
	"fmt"
	"math/rand"
	"path/filepath"
	"reflect"
	"testing"
	"testing/quick"
	"time"
)

// searchIndexEntries returns every (message_id, body) pair the search index
// holds, sorted.
func searchIndexEntries(t testing.TB, s *Store) []string {
	t.Helper()
	return sortedMessagePairs(t, s, `SELECT message_id, body FROM messages_fts ORDER BY message_id, body`)
}

// messageEntries returns every stored (message_id, body) pair, sorted.
func messageEntries(t testing.TB, s *Store) []string {
	t.Helper()
	return sortedMessagePairs(t, s, `SELECT message_id, body FROM messages ORDER BY message_id, body`)
}

func sortedMessagePairs(t testing.TB, s *Store, query string) []string {
	t.Helper()
	rows, err := s.db.Query(query)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	defer rows.Close()
	var pairs []string
	for rows.Next() {
		var id, body string
		if err := rows.Scan(&id, &body); err != nil {
			t.Fatalf("scan %s: %v", query, err)
		}
		pairs = append(pairs, id+"\x00"+body)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return pairs
}

// searchIndexRowIDs maps each search index rowid to its message_id. A write
// that leaves the index alone leaves the map unchanged; one that deletes and
// re-inserts an entry moves it to a new rowid.
func searchIndexRowIDs(t testing.TB, s *Store) map[int64]string {
	t.Helper()
	rows, err := s.db.Query(`SELECT rowid, message_id FROM messages_fts`)
	if err != nil {
		t.Fatalf("read search index rowids: %v", err)
	}
	defer rows.Close()
	ids := map[int64]string{}
	for rows.Next() {
		var rowID int64
		var messageID string
		if err := rows.Scan(&rowID, &messageID); err != nil {
			t.Fatalf("scan search index rowid: %v", err)
		}
		ids[rowID] = messageID
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read search index rowids: %v", err)
	}
	return ids
}

func signalDuplicateCount(t testing.TB, s *Store) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM (` + signalDuplicateRanks + `) WHERE rn > 1`).Scan(&n); err != nil {
		t.Fatalf("count Signal duplicates: %v", err)
	}
	return n
}

// indexOp is one write in a random sequence. Its fields index small pools, so
// a sequence keeps revisiting the same messages, conversations and Signal
// duplicate groups.
type indexOp struct {
	Kind, ID, Conv, Sender, Time, Body, Reactions, Platform int
}

const (
	opUpsert = iota
	opResave
	opRecordOutgoing
	opImportSignal
	opDelete
	opDeleteTmp
	opDeleteConversation
	opMergeConversations
	opDedupe
	opRepair
	opReopen
	indexOpKinds
)

var (
	indexOpIDs       = []string{"signal:a", "signal:B", "signal:é", "m-1", "tmp_1", "tmp_2"}
	indexOpConvs     = []string{"c1", "c2"}
	indexOpSenders   = []string{"+15550001", "+15550002"}
	indexOpTimes     = []int64{0, 1000, 2000}
	indexOpBodies    = []string{"", "  ", "needle", "needle in a haystack", "日本語", "[Reaction]"}
	indexOpReactions = []string{"", "[a]", "[é]"}
	indexOpPlatforms = []string{"signal", "sms", "whatsapp"}
)

func (op indexOp) message() *Message {
	return &Message{
		MessageID:      indexOpIDs[op.ID],
		ConversationID: indexOpConvs[op.Conv],
		SenderNumber:   indexOpSenders[op.Sender],
		Body:           indexOpBodies[op.Body],
		TimestampMS:    indexOpTimes[op.Time],
		Reactions:      indexOpReactions[op.Reactions],
		SourcePlatform: indexOpPlatforms[op.Platform],
		SourceID:       "source-" + indexOpIDs[op.ID],
	}
}

func randomIndexOp(r *rand.Rand, kind int) indexOp {
	return indexOp{
		Kind:      kind,
		ID:        r.Intn(len(indexOpIDs)),
		Conv:      r.Intn(len(indexOpConvs)),
		Sender:    r.Intn(len(indexOpSenders)),
		Time:      r.Intn(len(indexOpTimes)),
		Body:      r.Intn(len(indexOpBodies)),
		Reactions: r.Intn(len(indexOpReactions)),
		Platform:  r.Intn(len(indexOpPlatforms)),
	}
}

type indexCase struct{ Ops []indexOp }

func (indexCase) Generate(r *rand.Rand, _ int) reflect.Value {
	ops := make([]indexOp, 1+r.Intn(40))
	for i := range ops {
		ops[i] = randomIndexOp(r, r.Intn(indexOpKinds))
	}
	return reflect.ValueOf(indexCase{Ops: ops})
}

// After any sequence of store writes, including reopening the store, the
// search index holds exactly one entry per message, carrying the message's
// stored body. Re-saving a message unchanged leaves the index untouched, and
// no Signal duplicate survives a cleanup or a reopen.
func TestSearchIndexMatchesMessagesOverRandomWrites(t *testing.T) {
	property := func(c indexCase) bool {
		path := filepath.Join(t.TempDir(), "messages.db")
		store, err := New(path)
		if err != nil {
			t.Fatalf("New(): %v", err)
		}
		defer func() { store.Close() }()
		if !store.ftsEnabled {
			t.Skip("fts index not enabled in this sqlite build")
		}
		for _, conv := range indexOpConvs {
			if err := store.UpsertConversation(&Conversation{ConversationID: conv, Name: conv}); err != nil {
				t.Fatalf("UpsertConversation(%s): %v", conv, err)
			}
		}
		for i, op := range c.Ops {
			msg := op.message()
			switch op.Kind {
			case opUpsert:
				err = store.UpsertMessage(msg)
			case opResave:
				var stored *Message
				if stored, err = store.GetMessageByID(msg.MessageID); err != nil || stored == nil {
					break
				}
				before := searchIndexRowIDs(t, store)
				if err = store.UpsertMessage(stored); err == nil && !reflect.DeepEqual(searchIndexRowIDs(t, store), before) {
					t.Logf("op %d %+v: re-saving an unchanged message rewrote the search index", i, op)
					return false
				}
			case opRecordOutgoing:
				err = store.RecordOutgoingMessage(msg, "")
			case opImportSignal:
				// What the Signal Desktop importer does with each message.
				msg.SourcePlatform = "signal"
				var existing *Message
				if existing, err = store.GetMessageByID(msg.MessageID); err != nil {
					break
				}
				dropped := false
				if existing == nil {
					if dropped, err = store.SignalDuplicateWouldBeDropped(msg); err != nil {
						break
					}
				}
				if !dropped {
					err = store.UpsertMessage(msg)
				}
			case opDelete:
				err = store.DeleteMessageByID(msg.MessageID)
			case opDeleteTmp:
				_, err = store.DeleteTmpMessages(msg.ConversationID)
			case opDeleteConversation:
				err = store.DeleteConversation(msg.ConversationID)
			case opMergeConversations:
				err = store.MergeConversationIDs(msg.ConversationID, indexOpConvs[1-op.Conv])
			case opDedupe:
				err = store.dedupeSignalMessages()
			case opRepair:
				_, err = store.RepairLegacyArtifacts()
			case opReopen:
				if err = store.Close(); err == nil {
					store, err = New(path)
				}
			}
			if err != nil {
				t.Fatalf("op %d %+v: %v", i, op, err)
			}
			if got, want := searchIndexEntries(t, store), messageEntries(t, store); !reflect.DeepEqual(got, want) {
				t.Logf("op %d %+v: search index %q, messages %q", i, op, got, want)
				return false
			}
			if op.Kind == opDedupe || op.Kind == opReopen {
				if n := signalDuplicateCount(t, store); n != 0 {
					t.Logf("op %d %+v: %d Signal duplicates survived", i, op, n)
					return false
				}
			}
		}
		return true
	}
	cases := 300
	if raceDetectorEnabled {
		cases = 60
	}
	if err := quick.Check(property, &quick.Config{MaxCount: cases, Rand: rand.New(rand.NewSource(20261103))}); err != nil {
		t.Fatal(err)
	}
}

// signalDuplicateCase is a store of messages and a Signal message it doesn't
// hold yet. The candidate's IDs interleave the stored IDs in sort order, and
// the bodies and reactions mix one-byte and multi-byte characters, so the
// message_id tie-break goes both ways and byte and character lengths differ.
type signalDuplicateCase struct {
	Stored    []indexOp
	Candidate indexOp
}

var (
	storedSignalIDs     = []string{"signal:1", "signal:3", "signal:5", "signal:B", "signal:é"}
	candidateSignalIDs  = []string{"signal:0", "signal:2", "signal:4", "signal:6", "signal:a", "signal:z"}
	duplicateBodies     = []string{"", "ab", "abc", "日本", "日本語語"}
	duplicateReactions  = []string{"", "x", "é", "xy", "日本語"}
	duplicatePlatforms  = []string{"signal", "signal", "signal", "sms"}
	duplicateTimestamps = []int64{0, 1000}
)

func (op indexOp) duplicateMessage(ids []string) *Message {
	return &Message{
		MessageID:      ids[op.ID%len(ids)],
		ConversationID: indexOpConvs[op.Conv],
		SenderNumber:   indexOpSenders[op.Sender],
		Body:           duplicateBodies[op.Body%len(duplicateBodies)],
		TimestampMS:    duplicateTimestamps[op.Time%len(duplicateTimestamps)],
		Reactions:      duplicateReactions[op.Reactions%len(duplicateReactions)],
		SourcePlatform: duplicatePlatforms[op.Platform%len(duplicatePlatforms)],
	}
}

func (signalDuplicateCase) Generate(r *rand.Rand, _ int) reflect.Value {
	randomOp := func() indexOp {
		return indexOp{
			ID: r.Intn(6), Conv: r.Intn(len(indexOpConvs)), Sender: r.Intn(len(indexOpSenders)),
			Time: r.Intn(len(duplicateTimestamps)), Body: r.Intn(len(duplicateBodies)),
			Reactions: r.Intn(len(duplicateReactions)), Platform: r.Intn(len(duplicatePlatforms)),
		}
	}
	c := signalDuplicateCase{Stored: make([]indexOp, r.Intn(9)), Candidate: randomOp()}
	for i := range c.Stored {
		c.Stored[i] = randomOp()
	}
	return reflect.ValueOf(c)
}

// SignalDuplicateWouldBeDropped says a new Signal message will be dropped if
// and only if the store-open cleanup deletes it once it is stored.
func TestSignalDuplicateWouldBeDroppedAgreesWithTheCleanup(t *testing.T) {
	// Every case shares one store and starts by emptying it: opening a store
	// runs the whole migration, which costs far more than a case.
	store := newTestStore(t)
	property := func(c signalDuplicateCase) bool {
		tx, err := store.db.Begin()
		if err != nil {
			t.Fatalf("Begin(): %v", err)
		}
		if _, err := store.deleteMessages(tx, "1 = 1"); err != nil {
			tx.Rollback()
			t.Fatalf("empty the store: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit(): %v", err)
		}
		for _, op := range c.Stored {
			if err := store.UpsertMessage(op.duplicateMessage(storedSignalIDs)); err != nil {
				t.Fatalf("seed: %v", err)
			}
		}
		candidate := c.Candidate.duplicateMessage(candidateSignalIDs)
		predicted, err := store.SignalDuplicateWouldBeDropped(candidate)
		if err != nil {
			t.Fatalf("SignalDuplicateWouldBeDropped(): %v", err)
		}
		if err := store.UpsertMessage(candidate); err != nil {
			t.Fatalf("UpsertMessage(candidate): %v", err)
		}
		if err := store.dedupeSignalMessages(); err != nil {
			t.Fatalf("dedupeSignalMessages(): %v", err)
		}
		kept, err := store.GetMessageByID(candidate.MessageID)
		if err != nil {
			t.Fatalf("GetMessageByID(candidate): %v", err)
		}
		if predicted != (kept == nil) {
			t.Logf("case %+v: predicted dropped=%v, but the cleanup kept=%v", c, predicted, kept != nil)
			return false
		}
		return true
	}
	cases := 2000
	if raceDetectorEnabled {
		cases = 400
	}
	if err := quick.Check(property, &quick.Config{MaxCount: cases, Rand: rand.New(rand.NewSource(1103))}); err != nil {
		t.Fatal(err)
	}
}

// Re-saving a message unchanged, as the periodic Signal rescan and the Google
// reconcile do thousands of times an hour, leaves the search index untouched,
// and so does a status-only update. An edit replaces the message's one entry.
func TestSaveTouchesSearchIndexOnlyWhenTheBodyChanges(t *testing.T) {
	store := newTestStore(t)
	if !store.ftsEnabled {
		t.Skip("fts index not enabled in this sqlite build")
	}
	for _, msg := range []*Message{
		{MessageID: "m0", ConversationID: "c1", Body: "first needle", TimestampMS: 500},
		{MessageID: "m1", ConversationID: "c1", Body: "needle", TimestampMS: 1000, Status: "received"},
	} {
		if err := store.UpsertMessage(msg); err != nil {
			t.Fatalf("seed %s: %v", msg.MessageID, err)
		}
	}
	before := searchIndexRowIDs(t, store)

	resave := &Message{MessageID: "m1", ConversationID: "c1", Body: "needle", TimestampMS: 1000, Status: "received"}
	if err := store.UpsertMessage(resave); err != nil {
		t.Fatalf("re-save: %v", err)
	}
	if err := store.UpsertMessage(&Message{MessageID: "m1", ConversationID: "c1", TimestampMS: 1000, Status: "read"}); err != nil {
		t.Fatalf("status-only update: %v", err)
	}
	if err := store.RecordOutgoingMessage(resave, ""); err != nil {
		t.Fatalf("RecordOutgoingMessage(): %v", err)
	}
	if got := searchIndexRowIDs(t, store); !reflect.DeepEqual(got, before) {
		t.Fatalf("search index rowids after unchanged saves = %v, want %v untouched", got, before)
	}

	if err := store.UpsertMessage(&Message{MessageID: "m1", ConversationID: "c1", Body: "haystack", TimestampMS: 1000}); err != nil {
		t.Fatalf("edit: %v", err)
	}
	if got, want := searchIndexEntries(t, store), []string{"m0\x00first needle", "m1\x00haystack"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("search index after an edit = %q, want %q", got, want)
	}
	// Search the index itself: SearchMessages answers from LIKE over messages
	// whenever the index query fails or finds nothing.
	rows, err := store.db.Query(`SELECT message_id FROM messages_fts WHERE messages_fts MATCH '"needle"' ORDER BY message_id`)
	if err != nil {
		t.Fatalf("MATCH needle: %v", err)
	}
	defer rows.Close()
	var matched []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		matched = append(matched, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("MATCH needle: %v", err)
	}
	if !reflect.DeepEqual(matched, []string{"m0"}) {
		t.Fatalf("index search for the old body = %q, want only m0", matched)
	}
}

// SeedDemo writes its rows with plain inserts, so it indexes them itself.
func TestSeedDemoIndexesItsMessages(t *testing.T) {
	store := newTestStore(t)
	if !store.ftsEnabled {
		t.Skip("fts index not enabled in this sqlite build")
	}
	if err := store.SeedDemo(); err != nil {
		t.Fatalf("SeedDemo(): %v", err)
	}
	got, want := searchIndexEntries(t, store), messageEntries(t, store)
	if len(want) == 0 || !reflect.DeepEqual(got, want) {
		t.Fatalf("search index after SeedDemo = %d entries, want the %d messages", len(got), len(want))
	}
}

// A merge with no source conversation returns before beginning a write
// transaction, so another process's writer doesn't hold it up.
func TestMergeConversationIDsWithNoSourceSkipsTheWriteLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "messages.db")
	store, err := New(path)
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	defer store.Close()
	other, err := New(path)
	if err != nil {
		t.Fatalf("New(other): %v", err)
	}
	defer other.Close()
	holder, err := other.db.Begin()
	if err != nil {
		t.Fatalf("Begin(other): %v", err)
	}
	defer holder.Rollback()
	if _, err := holder.Exec(`UPDATE conversations SET name = name`); err != nil {
		t.Fatalf("take the write lock: %v", err)
	}
	start := time.Now()
	if err := store.MergeConversationIDs("absent", "target"); err != nil {
		t.Fatalf("MergeConversationIDs(absent): %v", err)
	}
	if waited := time.Since(start); waited > time.Second {
		t.Fatalf("MergeConversationIDs(absent) waited %v for another writer", waited)
	}
}

// A rebuild replaces the index in one transaction. Rolled back, as a process
// killed mid-rebuild is, it leaves the index exactly as it was, never empty.
func TestSearchIndexRebuildRollsBackWhole(t *testing.T) {
	store := newTestStore(t)
	if !store.ftsEnabled {
		t.Skip("fts index not enabled in this sqlite build")
	}
	for i, body := range []string{"alpha needle", "beta needle", "gamma"} {
		if err := store.UpsertMessage(&Message{MessageID: fmt.Sprintf("m%d", i), ConversationID: "c1", Body: body, TimestampMS: int64(i + 1)}); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	// Make the index stale, as a write by an older build can.
	if _, err := store.db.Exec(`DELETE FROM messages_fts WHERE message_id = 'm1'`); err != nil {
		t.Fatalf("drop an index entry: %v", err)
	}
	stale := searchIndexEntries(t, store)

	tx, err := store.db.Begin()
	if err != nil {
		t.Fatalf("Begin(): %v", err)
	}
	if err := rebuildSearchIndexTx(tx); err != nil {
		t.Fatalf("rebuildSearchIndexTx(): %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback(): %v", err)
	}
	if got := searchIndexEntries(t, store); !reflect.DeepEqual(got, stale) {
		t.Fatalf("search index after a rolled-back rebuild = %q, want it unchanged, %q", got, stale)
	}

	if err := store.rebuildFTS(); err != nil {
		t.Fatalf("rebuildFTS(): %v", err)
	}
	if got, want := searchIndexEntries(t, store), messageEntries(t, store); !reflect.DeepEqual(got, want) {
		t.Fatalf("search index after rebuildFTS = %q, want %q", got, want)
	}
}

// The store-open Signal cleanup deletes the search index entries of the rows
// it removes, so the next open finds the index in sync and leaves it alone.
func TestSignalCleanupKeepsTheSearchIndexInSync(t *testing.T) {
	path := filepath.Join(t.TempDir(), "messages.db")
	store, err := New(path)
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	defer func() { store.Close() }()
	if !store.ftsEnabled {
		t.Skip("fts index not enabled in this sqlite build")
	}
	// Two rows for one Signal message; the one with a reaction ranks first.
	// The first message is deleted before the reopen, so the live index rowids
	// differ from the 1..n a rebuild would assign.
	for _, msg := range []*Message{
		{MessageID: "gone", ConversationID: "c0", Body: "deleted"},
		{MessageID: "signal:b", ConversationID: "c1", SenderNumber: "+15550001", Body: "needle", TimestampMS: 1000, SourcePlatform: "signal"},
		{MessageID: "signal:a", ConversationID: "c1", SenderNumber: "+15550001", Body: "needle", TimestampMS: 1000, Reactions: "[x]", SourcePlatform: "signal"},
		{MessageID: "sms-1", ConversationID: "c2", Body: "unrelated", TimestampMS: 1000},
	} {
		if err := store.UpsertMessage(msg); err != nil {
			t.Fatalf("seed %s: %v", msg.MessageID, err)
		}
	}
	if err := store.DeleteMessageByID("gone"); err != nil {
		t.Fatalf("DeleteMessageByID(): %v", err)
	}
	want := map[int64]string{}
	for rowID, messageID := range searchIndexRowIDs(t, store) {
		if messageID != "signal:b" {
			want[rowID] = messageID
		}
	}

	reopen := func() {
		t.Helper()
		if err := store.Close(); err != nil {
			t.Fatalf("Close(): %v", err)
		}
		if store, err = New(path); err != nil {
			t.Fatalf("New(): %v", err)
		}
	}
	for open := 1; open <= 2; open++ {
		reopen()
		if got, want := messageEntries(t, store), []string{"signal:a\x00needle", "sms-1\x00unrelated"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("open %d: messages = %q, want %q", open, got, want)
		}
		if got := searchIndexRowIDs(t, store); !reflect.DeepEqual(got, want) {
			t.Fatalf("open %d: search index rowids = %v, want %v: only the duplicate's entry deleted, nothing rebuilt", open, got, want)
		}
		if got, want := searchIndexEntries(t, store), messageEntries(t, store); !reflect.DeepEqual(got, want) {
			t.Fatalf("open %d: search index = %q, want %q", open, got, want)
		}
	}
}

// A save reads the stored body before it writes. It waits for another
// connection's write lock, as a write-first save always did, instead of
// failing at once with SQLITE_BUSY.
func TestUpsertMessageWaitsForAnotherConnectionsWriteLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "messages.db")
	first, err := New(path)
	if err != nil {
		t.Fatalf("New(first): %v", err)
	}
	defer first.Close()
	second, err := New(path)
	if err != nil {
		t.Fatalf("New(second): %v", err)
	}
	defer second.Close()
	if err := first.UpsertMessage(&Message{MessageID: "m1", ConversationID: "c1", Body: "before", TimestampMS: 1000}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	holder, err := second.db.Begin()
	if err != nil {
		t.Fatalf("Begin(second): %v", err)
	}
	if _, err := holder.Exec(`UPDATE conversations SET name = name`); err != nil {
		holder.Rollback()
		t.Fatalf("take the write lock: %v", err)
	}
	saved := make(chan error, 1)
	go func() {
		saved <- first.UpsertMessage(&Message{MessageID: "m1", ConversationID: "c1", Body: "after", TimestampMS: 1000})
	}()
	select {
	case err := <-saved:
		holder.Rollback()
		t.Fatalf("UpsertMessage returned while another connection held the write lock: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := holder.Commit(); err != nil {
		t.Fatalf("Commit(second): %v", err)
	}
	if err := <-saved; err != nil {
		t.Fatalf("UpsertMessage once the lock was free: %v", err)
	}
	if got, want := searchIndexEntries(t, first), []string{"m1\x00after"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("search index = %q, want %q", got, want)
	}
}

func TestWithImmediateWriteTransactions(t *testing.T) {
	for dsn, want := range map[string]string{
		"":                                  "",
		":memory:":                          ":memory:?_txlock=immediate",
		"/tmp/messages.db":                  "/tmp/messages.db?_txlock=immediate",
		"file:messages.db?mode=rwc":         "file:messages.db?mode=rwc&_txlock=immediate",
		"file:messages.db?_txlock=deferred": "file:messages.db?_txlock=deferred",
	} {
		if got := withImmediateWriteTransactions(dsn); got != want {
			t.Errorf("withImmediateWriteTransactions(%q) = %q, want %q", dsn, got, want)
		}
	}
}

// benchmarkStore opens a store of n messages, indexed as an open rebuilds them.
func benchmarkStore(b *testing.B, n int) *Store {
	b.Helper()
	path := filepath.Join(b.TempDir(), "messages.db")
	store, err := New(path)
	if err != nil {
		b.Fatalf("New(): %v", err)
	}
	if _, err := store.db.Exec(`
		WITH RECURSIVE seq(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM seq WHERE i < ?)
		INSERT INTO messages (message_id, conversation_id, sender_number, body, timestamp_ms, source_platform)
		SELECT 'm' || i, 'c' || (i % 200), '+1555000' || (i % 50),
			'message number ' || i || ' about dinner plans, the weekend and a shared document', i, 'sms'
		FROM seq`, n); err != nil {
		b.Fatalf("seed: %v", err)
	}
	if err := store.Close(); err != nil {
		b.Fatalf("Close(): %v", err)
	}
	if store, err = New(path); err != nil {
		b.Fatalf("reopen: %v", err)
	}
	b.Cleanup(func() { store.Close() })
	return store
}

// BenchmarkSaveIntoLargeStore saves messages into a store of 80,000, about
// the size of the archive the periodic rescans re-save into.
func BenchmarkSaveIntoLargeStore(b *testing.B) {
	const n = 80_000
	message := func(i int, body string) *Message {
		return &Message{
			MessageID: fmt.Sprintf("m%d", i), ConversationID: fmt.Sprintf("c%d", i%200),
			SenderNumber: fmt.Sprintf("+1555000%d", i%50), Body: body, TimestampMS: int64(i), SourcePlatform: "sms",
		}
	}
	storedBody := func(i int) string {
		return fmt.Sprintf("message number %d about dinner plans, the weekend and a shared document", i)
	}
	b.Run("unchanged re-save", func(b *testing.B) {
		store := benchmarkStore(b, n)
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			id := 1 + i%n
			if err := store.UpsertMessage(message(id, storedBody(id))); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("new message", func(b *testing.B) {
		store := benchmarkStore(b, n)
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if err := store.UpsertMessage(message(n+1+i, "a new message")); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("edited body", func(b *testing.B) {
		store := benchmarkStore(b, n)
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			id := 1 + i%n
			if err := store.UpsertMessage(message(id, fmt.Sprintf("edit %d", i))); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// BenchmarkOpenStore opens a store of 80,000 messages, 16,000 of them Signal,
// whose search index is in sync: what every CLI command and MCP server start
// pays before its first query.
func BenchmarkOpenStore(b *testing.B) {
	path := filepath.Join(b.TempDir(), "messages.db")
	store, err := New(path)
	if err != nil {
		b.Fatalf("New(): %v", err)
	}
	if _, err := store.db.Exec(`
		WITH RECURSIVE seq(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM seq WHERE i < 80000)
		INSERT INTO messages (message_id, conversation_id, sender_number, body, timestamp_ms, source_platform)
		SELECT 'm' || i, 'c' || (i % 300), '+1555000' || (i % 50),
			'message number ' || i || ' about dinner plans, the weekend and a shared document', i,
			CASE WHEN i % 5 = 0 THEN 'signal' ELSE 'sms' END
		FROM seq`); err != nil {
		b.Fatalf("seed: %v", err)
	}
	if err := store.Close(); err != nil {
		b.Fatalf("Close(): %v", err)
	}
	if store, err = New(path); err != nil { // indexes the seeded messages
		b.Fatalf("reopen: %v", err)
	}
	if err := store.Close(); err != nil {
		b.Fatalf("Close(): %v", err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		store, err := New(path)
		if err != nil {
			b.Fatal(err)
		}
		store.Close()
	}
}
