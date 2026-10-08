package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"testing/quick"
	"time"
)

const (
	retentionTestDayMS  = int64(24 * time.Hour / time.Millisecond)
	retentionTestBaseMS = int64(1_790_000_000_000)
)

func TestInboxPayloadRetentionMigrationAppliesToBlankAndExistingV10Database(t *testing.T) {
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
		assertInboxPayloadRetentionMigration(t, store)
	})

	t.Run("existing v10 with frames", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "store.sqlite3")
		database, err := sql.Open("sqlite", storeDSN(path))
		if err != nil {
			t.Fatalf("sql.Open(): %v", err)
		}
		if err := database.Ping(); err != nil {
			_ = database.Close()
			t.Fatalf("Ping(): %v", err)
		}
		if err := enableWAL(context.Background(), database); err != nil {
			_ = database.Close()
			t.Fatalf("enableWAL(): %v", err)
		}
		if err := runMigrations(context.Background(), database, embeddedMigrations[:10]); err != nil {
			_ = database.Close()
			t.Fatalf("runMigrations(v10): %v", err)
		}
		mustExec(t, database, `
			INSERT INTO accounts (account_id, bridge_key, created_at_ms, updated_at_ms)
			VALUES ('account-a', 'google_messages', ?, ?)
		`, retentionTestBaseMS, retentionTestBaseMS)
		mustExec(t, database, `
			INSERT INTO inbox (
				inbox_id, account_id, generation, dedupe_key, codec, codec_version,
				received_at_ms, payload, processed_at_ms
			) VALUES
				('inbox-processed', 'account-a', 1, 'key-processed', 'test.frame', 1, ?, X'0102', ?),
				('inbox-pending', 'account-a', 1, 'key-pending', 'test.frame', 1, ?, X'0304', NULL)
		`, retentionTestBaseMS, retentionTestBaseMS+5, retentionTestBaseMS+10)
		before := readLedgerRows(t, database)
		if err := database.Close(); err != nil {
			t.Fatalf("close v10 database: %v", err)
		}

		store, err := Open(path)
		if err != nil {
			t.Fatalf("Open(v10): %v", err)
		}
		t.Cleanup(func() {
			if err := store.Close(); err != nil {
				t.Errorf("Close(): %v", err)
			}
		})
		after := readLedgerRows(t, store.db)
		if len(after) != 11 {
			t.Fatalf("migrated ledger rows = %d, want 11", len(after))
		}
		if !slices.Equal(after[:10], before) {
			t.Fatalf("migrations 0001-0010 changed:\nbefore: %+v\nafter:  %+v", before, after[:10])
		}
		assertInboxPayloadRetentionMigration(t, store)

		for _, want := range []struct {
			inboxID   string
			payload   []byte
			processed bool
		}{
			{inboxID: "inbox-processed", payload: []byte{1, 2}, processed: true},
			{inboxID: "inbox-pending", payload: []byte{3, 4}},
		} {
			record := readInboxRecord(t, store, want.inboxID)
			if !bytes.Equal(record.Payload, want.payload) {
				t.Errorf("%s payload = %x, want %x", want.inboxID, record.Payload, want.payload)
			}
			if (record.ProcessedAtMS != nil) != want.processed {
				t.Errorf("%s processed = %v, want %v", want.inboxID, record.ProcessedAtMS != nil, want.processed)
			}
			if record.QuarantinedAtMS != nil || record.PayloadPrunedAtMS != nil {
				t.Errorf("%s retention columns = (%v, %v), want both NULL", want.inboxID, record.QuarantinedAtMS, record.PayloadPrunedAtMS)
			}
		}
	})
}

func assertInboxPayloadRetentionMigration(t *testing.T, store *Store) {
	t.Helper()
	if len(embeddedMigrations) != 11 {
		t.Fatalf("embedded migrations = %d, want 11", len(embeddedMigrations))
	}
	assertPragmaInt(t, store.db, "user_version", 11)
	ledger := readLedgerRow(t, store.db, 11)
	if ledger.name != "inbox_payload_retention" {
		t.Fatalf("migration 0011 name = %q, want inbox_payload_retention", ledger.name)
	}
	if ledger.checksum != embeddedMigrations[10].checksumSHA256 {
		t.Fatalf("migration 0011 checksum = %q, want %q", ledger.checksum, embeddedMigrations[10].checksumSHA256)
	}
	// internal/migration pins the same checksum as an integrity gate on the
	// staged store; editing the SQL must move both pins together.
	const wantChecksum = "95ecd6607310400fed66927b038b41a59a34aa244a77370c3f589bed33ec8312"
	if ledger.checksum != wantChecksum {
		t.Fatalf("migration 0011 checksum = %q, want pinned %q", ledger.checksum, wantChecksum)
	}

	columns := map[string]bool{}
	rows, err := store.db.Query(`SELECT name, "notnull" FROM pragma_table_info('inbox')`)
	if err != nil {
		t.Fatalf("read inbox columns: %v", err)
	}
	for rows.Next() {
		var name string
		var notNull bool
		if err := rows.Scan(&name, &notNull); err != nil {
			t.Fatalf("scan inbox column: %v", err)
		}
		columns[name] = notNull
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("close inbox columns: %v", err)
	}
	for _, column := range []string{"quarantined_at_ms", "payload_pruned_at_ms"} {
		notNull, exists := columns[column]
		if !exists {
			t.Fatalf("inbox column %q is missing", column)
		}
		if notNull {
			t.Fatalf("inbox column %q is NOT NULL, want nullable", column)
		}
	}
	var strict int
	if err := store.db.QueryRow(`
		SELECT strict FROM pragma_table_list WHERE schema = 'main' AND name = 'inbox'
	`).Scan(&strict); err != nil {
		t.Fatalf("read inbox STRICT flag: %v", err)
	}
	if strict != 1 {
		t.Fatalf("inbox strict = %d, want 1", strict)
	}
	var indexExists bool
	if err := store.db.QueryRow(`
		SELECT EXISTS (
			SELECT 1 FROM sqlite_schema
			WHERE type = 'index' AND name = 'inbox_payload_prune_idx'
		)
	`).Scan(&indexExists); err != nil {
		t.Fatalf("inspect inbox_payload_prune_idx: %v", err)
	}
	if !indexExists {
		t.Fatal("inbox_payload_prune_idx is missing")
	}
}

func TestInboxRetentionColumnsEnforceTheirInvariants(t *testing.T) {
	store, _ := openMessageTestRepository(t, func() time.Time { return time.UnixMilli(retentionTestBaseMS) })
	seedMessageAccount(t, store, "account-a", "google_messages")
	insertRetentionTestFrame(t, store, retentionTestFrame{inboxID: "pending", receivedAtMS: 100, payload: []byte("p")})
	insertRetentionTestFrame(t, store, retentionTestFrame{inboxID: "processed", receivedAtMS: 100, processedAtMS: ptr64(200), payload: []byte("p")})
	insertRetentionTestFrame(t, store, retentionTestFrame{inboxID: "quarantined", receivedAtMS: 100, processedAtMS: ptr64(200), quarantinedAtMS: ptr64(200), payload: []byte("p")})
	insertRetentionTestFrame(t, store, retentionTestFrame{inboxID: "pruned", receivedAtMS: 100, processedAtMS: ptr64(200), prunedAtMS: ptr64(300)})

	for _, test := range []struct {
		name  string
		query string
	}{
		{"quarantine an unprocessed frame", `UPDATE inbox SET quarantined_at_ms = 500 WHERE inbox_id = 'pending'`},
		{"non-positive quarantine time", `UPDATE inbox SET quarantined_at_ms = 0 WHERE inbox_id = 'processed'`},
		{"prune an unprocessed frame", `UPDATE inbox SET payload = X'', payload_pruned_at_ms = 500 WHERE inbox_id = 'pending'`},
		{"prune while keeping the payload", `UPDATE inbox SET payload_pruned_at_ms = 500 WHERE inbox_id = 'processed'`},
		{"prune a quarantined frame", `UPDATE inbox SET payload = X'', payload_pruned_at_ms = 500 WHERE inbox_id = 'quarantined'`},
		{"non-positive prune time", `UPDATE inbox SET payload = X'', payload_pruned_at_ms = 0 WHERE inbox_id = 'processed'`},
		{"quarantine a pruned frame", `UPDATE inbox SET quarantined_at_ms = 500 WHERE inbox_id = 'pruned'`},
		{"restore a pruned frame's payload", `UPDATE inbox SET payload = X'01' WHERE inbox_id = 'pruned'`},
		{"unprocess a pruned frame", `UPDATE inbox SET processed_at_ms = NULL WHERE inbox_id = 'pruned'`},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := store.db.Exec(test.query)
			if err == nil || !isSQLiteConstraint(err) {
				t.Fatalf("Exec() error = %v, want a constraint violation", err)
			}
		})
	}
}

func TestMarkInboxQuarantinedRecordsTheFrameAndKeepsItsPayload(t *testing.T) {
	clock := newMessageTestClock(retentionTestBaseMS)
	store, repository := openMessageTestRepository(t, clock.Now)
	seedMessageAccount(t, store, "account-a", "google_messages")
	ctx := context.Background()

	for _, inbox := range []InboxRecord{
		messageTestInbox("pending", "account-a", "key-pending", []byte("pending payload")),
		messageTestInbox("processed", "account-a", "key-processed", []byte("processed payload")),
		messageTestInbox("pruned", "account-a", "key-pruned", []byte("pruned payload")),
	} {
		if _, err := repository.AppendInbox(ctx, inbox); err != nil {
			t.Fatalf("AppendInbox(%s): %v", inbox.InboxID, err)
		}
	}
	for _, inboxID := range []string{"processed", "pruned"} {
		if err := repository.MarkInboxProcessed(ctx, inboxID, "account-a"); err != nil {
			t.Fatalf("MarkInboxProcessed(%s): %v", inboxID, err)
		}
	}
	clock.Set(retentionTestBaseMS + 61*retentionTestDayMS)
	// Prune only "pruned": quarantine "processed" first so it is exempt.
	if err := repository.MarkInboxQuarantined(ctx, "processed", "account-a"); err != nil {
		t.Fatalf("MarkInboxQuarantined(processed): %v", err)
	}
	pruned := pruneAllInboxPayloads(t, repository, DefaultInboxPayloadRetention, 10)
	if pruned.Rows != 1 {
		t.Fatalf("pruned rows = %d, want only the unquarantined processed frame", pruned.Rows)
	}

	quarantineAtMS := retentionTestBaseMS + 62*retentionTestDayMS
	clock.Set(quarantineAtMS)
	for _, inboxID := range []string{"pending", "processed", "pruned", "missing"} {
		if err := repository.MarkInboxQuarantined(ctx, inboxID, "account-a"); err != nil {
			t.Fatalf("MarkInboxQuarantined(%s): %v", inboxID, err)
		}
	}
	// A second call keeps the first mark.
	clock.Set(quarantineAtMS + 1_000)
	if err := repository.MarkInboxQuarantined(ctx, "pending", "account-a"); err != nil {
		t.Fatalf("MarkInboxQuarantined(pending, repeat): %v", err)
	}
	// Another account's call never touches the row.
	if err := repository.MarkInboxQuarantined(ctx, "pending", "account-b"); err != nil {
		t.Fatalf("MarkInboxQuarantined(pending, other account): %v", err)
	}

	pending := readInboxRecord(t, store, "pending")
	if pending.ProcessedAtMS == nil || *pending.ProcessedAtMS != quarantineAtMS {
		t.Fatalf("pending processed_at_ms = %v, want %d", pending.ProcessedAtMS, quarantineAtMS)
	}
	if pending.QuarantinedAtMS == nil || *pending.QuarantinedAtMS != quarantineAtMS {
		t.Fatalf("pending quarantined_at_ms = %v, want first mark %d", pending.QuarantinedAtMS, quarantineAtMS)
	}

	processed := readInboxRecord(t, store, "processed")
	if processed.ProcessedAtMS == nil || *processed.ProcessedAtMS != retentionTestBaseMS {
		t.Fatalf("processed processed_at_ms = %v, want original %d", processed.ProcessedAtMS, retentionTestBaseMS)
	}
	if processed.QuarantinedAtMS == nil || *processed.QuarantinedAtMS != retentionTestBaseMS+61*retentionTestDayMS {
		t.Fatalf("processed quarantined_at_ms = %v, want the earlier mark", processed.QuarantinedAtMS)
	}
	if string(processed.Payload) != "processed payload" || processed.PayloadPrunedAtMS != nil {
		t.Fatalf("quarantined processed frame lost its payload: %q pruned=%v", processed.Payload, processed.PayloadPrunedAtMS)
	}

	prunedRecord := readInboxRecord(t, store, "pruned")
	if prunedRecord.QuarantinedAtMS != nil {
		t.Fatalf("pruned frame quarantined_at_ms = %v, want unchanged NULL", *prunedRecord.QuarantinedAtMS)
	}
	assertRowCount(t, store.db, "inbox", 3)

	// Quarantined frames survive any later pass, however old they get.
	clock.Set(retentionTestBaseMS + 400*retentionTestDayMS)
	if result := pruneAllInboxPayloads(t, repository, MinInboxPayloadRetention, 10); result.Rows != 0 {
		t.Fatalf("pruned rows after quarantine = %d, want 0", result.Rows)
	}
	for _, inboxID := range []string{"pending", "processed"} {
		if record := readInboxRecord(t, store, inboxID); len(record.Payload) == 0 {
			t.Fatalf("quarantined %s payload was pruned", inboxID)
		}
	}
}

func TestPruneInboxPayloadsRejectsShortRetentionAndBadLimits(t *testing.T) {
	store, repository := openMessageTestRepository(t, func() time.Time { return time.UnixMilli(retentionTestBaseMS) })
	seedMessageAccount(t, store, "account-a", "google_messages")
	insertRetentionTestFrame(t, store, retentionTestFrame{
		inboxID:       "old",
		receivedAtMS:  retentionTestBaseMS - 400*retentionTestDayMS,
		processedAtMS: ptr64(retentionTestBaseMS - 400*retentionTestDayMS),
		payload:       []byte("old"),
	})
	ctx := context.Background()
	if _, err := repository.PruneInboxPayloads(ctx, MinInboxPayloadRetention-time.Millisecond, 10); err == nil {
		t.Fatal("PruneInboxPayloads(below floor) error = nil, want refusal")
	}
	for _, limit := range []int{0, -1} {
		if _, err := repository.PruneInboxPayloads(ctx, DefaultInboxPayloadRetention, limit); err == nil {
			t.Fatalf("PruneInboxPayloads(limit %d) error = nil, want refusal", limit)
		}
	}
	if record := readInboxRecord(t, store, "old"); string(record.Payload) != "old" {
		t.Fatalf("refused prune changed the payload to %q", record.Payload)
	}

	// A clock too early for any frame to be past retention prunes nothing.
	early := mustMessageRepository(t, store, 1_000)
	result, err := early.PruneInboxPayloads(ctx, DefaultInboxPayloadRetention, 10)
	if err != nil {
		t.Fatalf("PruneInboxPayloads(early clock): %v", err)
	}
	if result.Rows != 0 || result.CutoffMS > 0 {
		t.Fatalf("early-clock result = %+v, want no rows and a non-positive cutoff", result)
	}
}

// A pruned frame keeps the row that dedupes and validates replays: a
// byte-identical re-delivery collapses onto it, a same-content projection is
// an idempotent no-op, and a different one is still a projection conflict.
func TestPrunedInboxFrameStillDedupesAndValidatesReplays(t *testing.T) {
	clock := newMessageTestClock(messageTestTimeMS)
	store, repository := openMessageTestRepository(t, clock.Now)
	seedMessageProjectionGraph(t, store)
	ctx := context.Background()

	inbox := messageTestInbox("inbox-pruned-replay", "account-a", "event-pruned-replay", []byte("frame bytes"))
	if _, err := repository.AppendInbox(ctx, inbox); err != nil {
		t.Fatalf("AppendInbox(): %v", err)
	}
	message := messageTestMessage("message-pruned-replay", "conversation-a", "account-a", "remote-pruned-replay", pointer("identity-a"))
	projection := MessageProjection{InboxID: inbox.InboxID, Message: message}
	if err := repository.ProjectMessage(ctx, projection); err != nil {
		t.Fatalf("ProjectMessage(first): %v", err)
	}
	projected, err := repository.GetMessage(ctx, message.MessageID)
	if err != nil {
		t.Fatalf("GetMessage(): %v", err)
	}

	clock.Set(messageTestTimeMS + 61*retentionTestDayMS)
	result := pruneAllInboxPayloads(t, repository, DefaultInboxPayloadRetention, 10)
	if result.Rows != 1 || result.Bytes != int64(len("frame bytes")) {
		t.Fatalf("prune result = %+v, want 1 row of %d bytes", result, len("frame bytes"))
	}
	pruned := readInboxRecord(t, store, inbox.InboxID)
	if len(pruned.Payload) != 0 || pruned.PayloadPrunedAtMS == nil || pruned.DedupeKey != inbox.DedupeKey {
		t.Fatalf("pruned record = %+v, want empty payload, prune stamp, and the original dedupe key", pruned)
	}

	replay := messageTestInbox("inbox-pruned-replay-2", "account-a", inbox.DedupeKey, []byte("frame bytes"))
	effectiveID, err := repository.AppendInbox(ctx, replay)
	if err != nil {
		t.Fatalf("AppendInbox(replay): %v", err)
	}
	if effectiveID != inbox.InboxID {
		t.Fatalf("replay inbox ID = %q, want original %q", effectiveID, inbox.InboxID)
	}
	assertRowCount(t, store.db, "inbox", 1)
	if pending, err := repository.Unprocessed(ctx); err != nil || len(pending) != 0 {
		t.Fatalf("Unprocessed() after replay = %d rows, %v; want none", len(pending), err)
	}

	if err := repository.ProjectMessage(ctx, projection); err != nil {
		t.Fatalf("ProjectMessage(identical replay of pruned frame): %v", err)
	}
	changed := message
	changed.Body = "different content"
	err = repository.ProjectMessage(ctx, MessageProjection{InboxID: inbox.InboxID, Message: changed})
	if !errors.Is(err, ErrInboxProjectionConflict) {
		t.Fatalf("ProjectMessage(changed replay of pruned frame) error = %v, want ErrInboxProjectionConflict", err)
	}
	after, err := repository.GetMessage(ctx, message.MessageID)
	if err != nil {
		t.Fatalf("GetMessage() after replays: %v", err)
	}
	if !reflect.DeepEqual(after, projected) {
		t.Fatalf("message changed across replays:\nbefore: %+v\nafter:  %+v", projected, after)
	}
	assertRowCount(t, store.db, "messages", 1)
}

// retentionCase is a random inbox population, a retention, a batch size, and
// two clock readings, an earlier pass and a later one.
type retentionCase struct {
	Frames        []retentionTestFrame
	RetentionDays int
	BatchSize     int
	EarlierMS     int64
	NowMS         int64
}

func (retentionCase) Generate(r *rand.Rand, _ int) reflect.Value {
	now := retentionTestBaseMS
	retentionDays := 45 + r.Intn(100)
	if r.Intn(3) == 0 {
		retentionDays = 45 // the floor, where the edge cases sit
	}
	cutoffMS := now - int64(retentionDays)*retentionTestDayMS
	frames := make([]retentionTestFrame, r.Intn(40))
	for i := range frames {
		frame := retentionTestFrame{
			inboxID:      fmt.Sprintf("frame-%03d", i),
			receivedAtMS: now - r.Int63n(200*retentionTestDayMS),
			payload:      make([]byte, r.Intn(48)),
		}
		r.Read(frame.payload)
		if r.Intn(4) != 0 {
			// Processing follows receipt by up to 30 days but never passes now;
			// a quarter of the frames stay unprocessed. A fifth of processed
			// frames sit within a millisecond of the cutoff.
			processedAt := frame.receivedAtMS + r.Int63n(30*retentionTestDayMS)
			if r.Intn(5) == 0 {
				processedAt = cutoffMS + r.Int63n(3) - 1
				frame.receivedAtMS = processedAt - r.Int63n(2)
			}
			if processedAt > now {
				processedAt = now
			}
			frame.processedAtMS = &processedAt
			if r.Intn(6) == 0 {
				frame.quarantinedAtMS = ptr64(processedAt)
			}
		}
		frames[i] = frame
	}
	return reflect.ValueOf(retentionCase{
		Frames:        frames,
		RetentionDays: retentionDays,
		BatchSize:     1 + r.Intn(12),
		EarlierMS:     now - r.Int63n(40*retentionTestDayMS),
		NowMS:         now,
	})
}

// retentionEligible is the reference rule: processed, never quarantined, not
// already pruned, and both processed and received before the cutoff.
func retentionEligible(frame retentionTestFrame, cutoffMS int64) bool {
	return frame.processedAtMS != nil &&
		frame.quarantinedAtMS == nil &&
		frame.prunedAtMS == nil &&
		*frame.processedAtMS < cutoffMS &&
		frame.receivedAtMS < cutoffMS
}

// The pruner agrees with the reference rule for every population, retention,
// and batch size, and it holds these invariants:
//   - rows are never added or deleted, and only payload and
//     payload_pruned_at_ms ever change;
//   - unprocessed, quarantined, and in-window frames keep their payloads;
//   - reported rows and bytes equal what was emptied;
//   - an earlier pass prunes a subset of a later pass;
//   - a repeated pass prunes nothing (idempotence).
func TestPruneInboxPayloadsMatchesReferenceRuleProperty(t *testing.T) {
	property := func(c retentionCase) bool {
		clock := newMessageTestClock(c.EarlierMS)
		store, err := Open(filepath.Join(t.TempDir(), "store.sqlite3"))
		if err != nil {
			t.Errorf("Open(): %v", err)
			return false
		}
		defer store.Close()
		repository, err := NewMessageRepository(store, clock.Now)
		if err != nil {
			t.Errorf("NewMessageRepository(): %v", err)
			return false
		}
		seedMessageAccount(t, store, "account-a", "google_messages")
		for _, frame := range c.Frames {
			insertRetentionTestFrame(t, store, frame)
		}
		before := readAllInboxRecords(t, store)
		retention := time.Duration(c.RetentionDays) * 24 * time.Hour

		earlier := pruneAllInboxPayloads(t, repository, retention, c.BatchSize)
		earlierPruned := prunedInboxIDs(readAllInboxRecords(t, store))

		clock.Set(c.NowMS)
		later := pruneAllInboxPayloads(t, repository, retention, c.BatchSize)
		after := readAllInboxRecords(t, store)
		laterPruned := prunedInboxIDs(after)
		again := pruneAllInboxPayloads(t, repository, retention, c.BatchSize)

		cutoffMS := c.NowMS - retention.Milliseconds()
		wantRows, wantBytes := 0, int64(0)
		for _, frame := range c.Frames {
			if retentionEligible(frame, cutoffMS) {
				wantRows++
				wantBytes += int64(len(frame.payload))
			}
		}
		if got := earlier.Rows + later.Rows; got != wantRows {
			t.Errorf("pruned rows = %d, want %d (case %+v)", got, wantRows, c)
			return false
		}
		if got := earlier.Bytes + later.Bytes; got != wantBytes {
			t.Errorf("pruned bytes = %d, want %d", got, wantBytes)
			return false
		}
		if again.Rows != 0 {
			t.Errorf("repeated pass pruned %d rows, want 0", again.Rows)
			return false
		}
		for id := range earlierPruned {
			if !laterPruned[id] {
				t.Errorf("frame %s pruned early but not later", id)
				return false
			}
		}
		if len(after) != len(before) {
			t.Errorf("inbox rows = %d after pruning, want %d", len(after), len(before))
			return false
		}
		for i, frame := range c.Frames {
			got, want := after[i], before[i]
			if got.InboxID != frame.inboxID || want.InboxID != frame.inboxID {
				t.Errorf("row order changed at %d", i)
				return false
			}
			eligible := retentionEligible(frame, cutoffMS)
			if eligible {
				if len(got.Payload) != 0 || got.PayloadPrunedAtMS == nil {
					t.Errorf("eligible frame %s kept its payload", frame.inboxID)
					return false
				}
				if frame.processedAtMS == nil || frame.quarantinedAtMS != nil || frame.receivedAtMS >= cutoffMS {
					t.Errorf("frame %s violates a pruning invariant", frame.inboxID)
					return false
				}
			} else if !bytes.Equal(got.Payload, want.Payload) || got.PayloadPrunedAtMS != nil {
				t.Errorf("ineligible frame %s changed: payload %x -> %x", frame.inboxID, want.Payload, got.Payload)
				return false
			}
			got.Payload, want.Payload = nil, nil
			got.PayloadPrunedAtMS, want.PayloadPrunedAtMS = nil, nil
			if !reflect.DeepEqual(got, want) {
				t.Errorf("frame %s changed beyond payload:\nbefore: %+v\nafter:  %+v", frame.inboxID, want, got)
				return false
			}
		}
		return true
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 80}); err != nil {
		t.Fatal(err)
	}
}

type retentionTestFrame struct {
	inboxID         string
	receivedAtMS    int64
	processedAtMS   *int64
	quarantinedAtMS *int64
	prunedAtMS      *int64
	payload         []byte
}

func insertRetentionTestFrame(t *testing.T, store *Store, frame retentionTestFrame) {
	t.Helper()
	payload := frame.payload
	if payload == nil {
		payload = []byte{}
	}
	mustExec(t, store.db, `
		INSERT INTO inbox (
			inbox_id, account_id, generation, dedupe_key, codec, codec_version,
			received_at_ms, payload, processed_at_ms, quarantined_at_ms, payload_pruned_at_ms
		) VALUES (?, 'account-a', 1, ?, 'test.frame', 1, ?, ?, ?, ?, ?)
	`,
		frame.inboxID,
		"key-"+frame.inboxID,
		frame.receivedAtMS,
		payload,
		frame.processedAtMS,
		frame.quarantinedAtMS,
		frame.prunedAtMS,
	)
}

func pruneAllInboxPayloads(
	t *testing.T,
	repository *MessageRepository,
	retention time.Duration,
	batchSize int,
) InboxPruneResult {
	t.Helper()
	var total InboxPruneResult
	for {
		batch, err := repository.PruneInboxPayloads(context.Background(), retention, batchSize)
		if err != nil {
			t.Fatalf("PruneInboxPayloads(): %v", err)
		}
		total.CutoffMS = batch.CutoffMS
		total.Rows += batch.Rows
		total.Bytes += batch.Bytes
		if batch.Rows > batchSize {
			t.Fatalf("batch pruned %d rows, above its limit %d", batch.Rows, batchSize)
		}
		if batch.Rows < batchSize {
			return total
		}
	}
}

func readInboxRecord(t *testing.T, store *Store, inboxID string) InboxRecord {
	t.Helper()
	record, err := scanInboxRecord(store.db.QueryRow(`
		SELECT `+inboxColumns+`
		FROM inbox
		WHERE inbox_id = ?
	`, inboxID))
	if err != nil {
		t.Fatalf("read inbox %q: %v", inboxID, err)
	}
	return record
}

func readAllInboxRecords(t *testing.T, store *Store) []InboxRecord {
	t.Helper()
	rows, err := store.db.Query(`SELECT ` + inboxColumns + ` FROM inbox ORDER BY inbox_id`)
	if err != nil {
		t.Fatalf("read inbox: %v", err)
	}
	records, err := collectRows(rows, scanInboxRecord)
	if err != nil {
		t.Fatalf("read inbox: %v", err)
	}
	return records
}

func prunedInboxIDs(records []InboxRecord) map[string]bool {
	pruned := map[string]bool{}
	for _, record := range records {
		if record.PayloadPrunedAtMS != nil {
			pruned[record.InboxID] = true
		}
	}
	return pruned
}

func ptr64(value int64) *int64 {
	return &value
}
