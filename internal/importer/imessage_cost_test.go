package importer

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/maxghenis/openmessage/internal/db"
)

// buildBigChat fills one chat with n rows, every sixth of them a reaction to
// the row before (with a removal now and then), bodies in attributedBody as
// modern Messages stores them. spread is how far back the rows reach.
func buildBigChat(t *testing.T, n int, spread time.Duration) (*nativeChatDB, int64, int64, int) {
	t.Helper()
	chat := newNativeChatDB(t)
	alice := chat.addHandle("+15550000001")
	c := chat.addChat("iMessage;-;bench", strPtr("Bench"), false, alice)
	tx, err := chat.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	reactions := 0
	step := int64(spread/time.Second) / int64(n)
	start := testNow.Add(-spread).Unix()
	for i := 1; i <= n; i++ {
		unix := start + int64(i)*step
		guid := fmt.Sprintf("g%06d", i)
		var assocType int64
		var assoc any
		if i%6 == 0 {
			assocType, assoc = 2000+int64(i%6), fmt.Sprintf("p:0/g%06d", i-1)
			if i%5 == 0 {
				assocType = 3000
			}
			reactions++
		}
		res, err := tx.Exec(`INSERT INTO message (guid, attributedBody, handle_id, date, is_from_me, associated_message_type, associated_message_guid)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, guid, makeAttributedBody(fmt.Sprintf("ordinary message %d", i)), alice, coreDate(unix), i%3 == 0, assocType, assoc)
		if err != nil {
			t.Fatal(err)
		}
		rowID, _ := res.LastInsertId()
		if _, err := tx.Exec(`INSERT INTO chat_message_join (chat_id, message_id, message_date) VALUES (?, ?, ?)`, c, rowID, coreDate(unix)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return chat, c, alice, reactions
}

// TestIMessageOneMessagePollStaysCheap (I7): on a 5,000-row chat with 833
// reactions, a poll after one new message or one new reaction takes well
// under a second and writes one row; an unchanged poll is skipped. The
// "all in the window" case dates every row within the last day, the worst
// case for the in-place-edit window.
func TestIMessageOneMessagePollStaysCheap(t *testing.T) {
	const budget = time.Second
	for _, tc := range []struct {
		name   string
		spread time.Duration
	}{
		{"spread over a year", 365 * 24 * time.Hour},
		{"all in the window", 20 * time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			chat, c, alice, reactions := buildBigChat(t, 5000, tc.spread)
			store, _ := newIMessageStore(t)
			clock := &fixedClock{t: testNow}
			poller := &IMessage{DBPath: chat.path, now: clock.now}
			defer poller.Close()

			start := time.Now()
			first := runImport(t, poller, store)
			t.Logf("first import: %d rows, %d reactions in chat.db, %s", first.MessagesImported, reactions, time.Since(start).Round(time.Millisecond))

			start = time.Now()
			runImport(t, poller, store)
			if !poller.last.skipped {
				t.Fatalf("unchanged poll not skipped: %+v", poller.last)
			}
			t.Logf("unchanged poll: %s", time.Since(start).Round(time.Microsecond))

			clock.t = clock.t.Add(30 * time.Second)
			chat.message(c, "new-tail", "new tail", clock.t.Unix(), false, alice)
			start = time.Now()
			res := runImport(t, poller, store)
			elapsed := time.Since(start)
			t.Logf("one-message poll: %s, read %d rows, wrote %d", elapsed.Round(time.Millisecond), poller.last.rowsRead, res.MessagesImported)
			if poller.last.full || res.MessagesImported != 1 || (elapsed > budget && !raceDetectorEnabled) {
				t.Fatalf("one-message poll: full=%v wrote=%d in %s (budget %s)", poller.last.full, res.MessagesImported, elapsed, budget)
			}

			clock.t = clock.t.Add(30 * time.Second)
			chat.react(c, "new-love", 2000, "p:0/g000007", clock.t.Unix(), true, alice)
			start = time.Now()
			res = runImport(t, poller, store)
			elapsed = time.Since(start)
			t.Logf("one-reaction poll: %s, read %d rows, wrote %d", elapsed.Round(time.Millisecond), poller.last.rowsRead, res.MessagesImported)
			if poller.last.full || res.MessagesImported != 1 || (elapsed > budget && !raceDetectorEnabled) {
				t.Fatalf("one-reaction poll: full=%v wrote=%d in %s (budget %s)", poller.last.full, res.MessagesImported, elapsed, budget)
			}
		})
	}
}

// TestIMessageRealChatDBProbe reads the real chat.db read-only into an
// in-memory store and prints counts and timings only. It never prints
// message text, names or numbers. Run it with
//
//	OPENMESSAGE_IMESSAGE_PROBE=1 go test ./internal/importer -run TestIMessageRealChatDBProbe -v
//
// (OPENMESSAGE_IMESSAGE_PROBE_DB overrides ~/Library/Messages/chat.db). It
// needs Full Disk Access.
func TestIMessageRealChatDBProbe(t *testing.T) {
	if os.Getenv("OPENMESSAGE_IMESSAGE_PROBE") == "" {
		t.Skip("set OPENMESSAGE_IMESSAGE_PROBE=1 to read the real chat.db")
	}
	path := os.Getenv("OPENMESSAGE_IMESSAGE_PROBE_DB")
	if path == "" {
		home, _ := os.UserHomeDir()
		path = filepath.Join(home, "Library", "Messages", "chat.db")
	}
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	poller := &IMessage{DBPath: path}
	defer poller.Close()
	start := time.Now()
	res, err := poller.ImportFromDB(store)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("first import: %s; %d message rows written, %d conversations created, %d errors, %d chat.db rows read",
		time.Since(start).Round(time.Millisecond), res.MessagesImported, res.ConversationsCreated, len(res.Errors), poller.last.rowsRead)

	rows, err := store.AllIMessageRows()
	if err != nil {
		t.Fatal(err)
	}
	withReactions, typedLookalikes := 0, 0
	for _, r := range rows {
		if r.Reactions != "" {
			withReactions++
		}
		if _, ok := db.ParseTapback(r.Body); ok {
			typedLookalikes++
		}
	}
	t.Logf("store: %d iMessage rows, %d with reactions, %d typed rows that read like a tapback (kept as text)", len(rows), withReactions, typedLookalikes)

	// Where those look-alikes come from, by the row's service (counts only).
	byService := map[string]int{}
	poller.mu.Lock()
	svcRows, err := poller.src.conn.QueryContext(context.Background(), `
		SELECT COALESCE(service, ''), text, attributedBody FROM message
		WHERE COALESCE(associated_message_type, 0) = 0`)
	if err != nil {
		poller.mu.Unlock()
		t.Fatal(err)
	}
	for svcRows.Next() {
		var service string
		var text sql.NullString
		var blob []byte
		if err := svcRows.Scan(&service, &text, &blob); err != nil {
			t.Fatal(err)
		}
		body := text.String
		if body == "" {
			body = decodeAttributedBody(blob)
		}
		if _, ok := db.ParseTapback(body); ok {
			byService[service]++
		}
	}
	svcRows.Close()
	poller.mu.Unlock()
	t.Logf("tapback look-alikes in type-0 rows by service: %v", byService)

	fresh := &IMessage{DBPath: path}
	start = time.Now()
	again, err := fresh.ImportFromDB(store)
	fresh.Close()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("full re-import of the unchanged chat.db: %s; %d rows written, %d unchanged", time.Since(start).Round(time.Millisecond), again.MessagesImported, again.MessagesDuplicate)
	if again.MessagesImported != 0 || again.ConversationsCreated != 0 {
		t.Errorf("an unchanged re-import wrote rows")
	}

	start = time.Now()
	if _, err := poller.ImportFromDB(store); err != nil {
		t.Fatal(err)
	}
	t.Logf("unchanged poll: %s (skipped=%v)", time.Since(start).Round(time.Microsecond), poller.last.skipped)

	// What every poll after a chat.db write costs when no message changed:
	// pretend the data_version moved.
	poller.mu.Lock()
	poller.marks.dataVersion--
	poller.mu.Unlock()
	start = time.Now()
	if _, err := poller.ImportFromDB(store); err != nil {
		t.Fatal(err)
	}
	t.Logf("poll after a chat.db write with no message change: %s (full=%v, %d rows read)", time.Since(start).Round(time.Millisecond), poller.last.full, poller.last.rowsRead)

	// An incremental poll over the newest 200 rows: move the message mark
	// back as if they were new.
	poller.mu.Lock()
	var back int64
	var guid string
	if err := poller.src.conn.QueryRowContext(context.Background(), `SELECT ROWID, guid FROM message ORDER BY ROWID DESC LIMIT 1 OFFSET 200`).Scan(&back, &guid); err != nil {
		poller.mu.Unlock()
		t.Fatal(err)
	}
	var above int64
	if err := poller.src.conn.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM message WHERE ROWID > ?`, back).Scan(&above); err != nil {
		poller.mu.Unlock()
		t.Fatal(err)
	}
	poller.marks.msgMax, poller.marks.msgMaxGUID, poller.marks.msgCount = back, guid, poller.marks.msgCount-above
	poller.marks.dataVersion--
	poller.mu.Unlock()
	start = time.Now()
	res, err = poller.ImportFromDB(store)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("incremental poll over the newest %d rows: %s (full=%v, %d rows read, %d written)", above, time.Since(start).Round(time.Millisecond), poller.last.full, poller.last.rowsRead, res.MessagesImported)
}
