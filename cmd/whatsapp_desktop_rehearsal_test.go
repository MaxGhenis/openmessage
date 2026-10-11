package cmd

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/maxghenis/openmessage/internal/whatsapplive"
)

// TestWhatsAppDesktopFeedRealDataRehearsal runs the real reader, decoder and
// worker against a COPY of a live data directory before a deploy. It is
// skipped unless OPENMESSAGES_DESKTOP_REHEARSAL_DIR names that copy (holding
// v2/store.sqlite3 and, optionally, whatsapp-session.db); it reads the Desktop
// database read-only, writes only the copy, and logs counts, never content.
// OPENMESSAGES_DESKTOP_REHEARSAL_CHAT may name one chat JID whose newest v2
// message is compared with Desktop's newest eligible stanza.
//
//	OPENMESSAGES_DESKTOP_REHEARSAL_DIR=/path/to/copy GOWORK=off go test ./cmd \
//	  -run TestWhatsAppDesktopFeedRealDataRehearsal -v -timeout 30m
func TestWhatsAppDesktopFeedRealDataRehearsal(t *testing.T) {
	dir := os.Getenv("OPENMESSAGES_DESKTOP_REHEARSAL_DIR")
	if dir == "" {
		t.Skip("set OPENMESSAGES_DESKTOP_REHEARSAL_DIR to a copy of a data directory")
	}
	t.Setenv("OPENMESSAGES_WHATSAPP_DESKTOP_FEED", "1")
	stack, err := newV2Stack(v2StackDeps{Logger: zerolog.Nop(), DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer stack.Store.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = stack.worker.Run(ctx) }()

	unpaired := func() whatsapplive.StatusSnapshot { return whatsapplive.StatusSnapshot{} }
	feed := newWhatsAppDesktopFeed(stack, dir, true, false, &whatsappDesktopGate{}, unpaired, zerolog.Nop())
	if feed.initErr != nil {
		t.Fatalf("feed init: %v", feed.initErr)
	}
	db := rehearsalDB(t, filepath.Join(dir, "v2", "store.sqlite3"))
	before := rehearsalCounts(t, db)

	scanAndDrain := func(label string, at time.Time) {
		started := time.Now()
		feed.scanOnce(ctx, at)
		rehearsalWaitDrained(t, db)
		status := feed.Snapshot()
		counters := stack.Counters.Snapshot(whatsappAccountID)
		t.Logf("%s: %.1fs frames_appended=%d high_water=%d last_error=%q skipped=%v",
			label, time.Since(started).Seconds(), status.FramesAppended, status.HighWaterPK, status.LastError, status.RowsSkippedByReason)
		t.Logf("%s: history appended=%d deduped=%d imported=%d existing=%d conversations=%d skipped=%d quarantined=%d",
			label, counters.HistoryAppended, counters.HistoryDeduped, counters.HistoryImported, counters.HistoryExisting,
			counters.HistoryConversations, counters.HistorySkipped, counters.Quarantined)
		if status.LastError != "" || counters.Quarantined != 0 {
			t.Fatalf("%s failed: %+v", label, status)
		}
	}
	scanAndDrain("full scan", time.Now())
	afterFirst := rehearsalCounts(t, db)
	importedFirst := stack.Counters.Snapshot(whatsappAccountID).HistoryImported

	// A second full scan over the same Desktop rows must insert nothing.
	feed.mu.Lock()
	feed.status.LastFullScanAtMS = 0
	feed.mu.Unlock()
	scanAndDrain("rescan", time.Now())
	afterSecond := rehearsalCounts(t, db)
	if got := stack.Counters.Snapshot(whatsappAccountID).HistoryImported; got != importedFirst || afterSecond != afterFirst {
		t.Fatalf("rescan inserted rows: imported %d -> %d, counts %+v -> %+v", importedFirst, got, afterFirst, afterSecond)
	}

	t.Logf("v2 WhatsApp before: %+v", before)
	t.Logf("v2 WhatsApp after:  %+v", afterFirst)
	var duplicates, lidChats int
	mustScan(t, db.QueryRow(`SELECT count(*) FROM (SELECT remote_message_id FROM messages WHERE account_id=?
		GROUP BY remote_message_id HAVING count(DISTINCT conversation_id) > 1)`, whatsappAccountID), &duplicates)
	mustScan(t, db.QueryRow(`SELECT count(*) FROM conversations WHERE account_id=? AND remote_conversation_id LIKE '%@lid%'`, whatsappAccountID), &lidChats)
	if duplicates != 0 || lidChats != 0 {
		t.Fatalf("stanza IDs in more than one thread = %d, @lid threads = %d", duplicates, lidChats)
	}
	if chat := os.Getenv("OPENMESSAGES_DESKTOP_REHEARSAL_CHAT"); chat != "" {
		var stanza string
		var at int64
		mustScan(t, db.QueryRow(`SELECT m.remote_message_id, m.occurred_at_ms FROM messages m
			JOIN conversations c ON c.conversation_id = m.conversation_id
			WHERE c.account_id=? AND c.remote_conversation_id=? ORDER BY m.occurred_at_ms DESC, m.message_id DESC LIMIT 1`,
			whatsappAccountID, "whatsapp:"+chat), &stanza, &at)
		t.Logf("chat newest v2 message: stanza=%s at=%s", stanza, time.UnixMilli(at).Format(time.RFC3339))
	}
}

type rehearsalTotals struct {
	Messages, Conversations, SinceOutage int
	NewestMS                             int64
}

func rehearsalCounts(t *testing.T, db *sql.DB) rehearsalTotals {
	t.Helper()
	var totals rehearsalTotals
	mustScan(t, db.QueryRow(`SELECT count(*), COALESCE(max(occurred_at_ms),0), COALESCE(sum(occurred_at_ms >= 1787260699000),0)
		FROM messages WHERE account_id=?`, whatsappAccountID), &totals.Messages, &totals.NewestMS, &totals.SinceOutage)
	mustScan(t, db.QueryRow(`SELECT count(*) FROM conversations WHERE account_id=?`, whatsappAccountID), &totals.Conversations)
	return totals
}

func rehearsalWaitDrained(t *testing.T, db *sql.DB) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Minute)
	for {
		var pending int
		mustScan(t, db.QueryRow(`SELECT count(*) FROM inbox WHERE processed_at_ms IS NULL`), &pending)
		if pending == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("worker left %d frames unprocessed", pending)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func rehearsalDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro"}).String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func mustScan(t *testing.T, row *sql.Row, dest ...any) {
	t.Helper()
	if err := row.Scan(dest...); err != nil {
		t.Fatal(err)
	}
}
