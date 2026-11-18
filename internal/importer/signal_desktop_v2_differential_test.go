package importer

// Differential test: the legacy Signal Desktop importer and the v2 history
// feed read the same synthetic Signal Desktop archive. The legacy side runs
// its own SQL, cut from the helper it ships (signal_desktop_export.cjs), so a
// change there is caught here. Every row either matches (same conversation,
// identity, time, direction and body) or differs for a named reason.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/ingest"
	"github.com/maxghenis/openmessage/internal/signaldesktop"
	"github.com/maxghenis/openmessage/internal/signaldesktop/fixture"
	"github.com/maxghenis/openmessage/internal/v2keys"
	_ "modernc.org/sqlite"
)

const (
	diffSelf     = "+15555550100"
	diffSelfACI  = "00000000-0000-4000-8000-000000000000"
	diffPeerE164 = "+15555550101"
	diffPeerACI  = "11111111-2222-4333-8444-555555555555"
	diffOtherACI = "66666666-7777-4888-9999-aaaaaaaaaaaa"
	diffGroupID  = "c3ludGhldGljLWdyb3VwLWlkLWZvci10ZXN0cy0wMQ=="
	diffBaseMS   = int64(1_790_000_000_000)
)

func legacySQL(t *testing.T) (conversations, messages string) {
	t.Helper()
	raw, err := signalDesktopExportFS.ReadFile("signal_desktop_export.cjs")
	if err != nil {
		t.Fatal(err)
	}
	source := string(raw)
	between := func(start, end string) string {
		t.Helper()
		from := strings.Index(source, start)
		if from < 0 {
			t.Fatalf("legacy helper no longer contains %q", start)
		}
		from += len(start)
		to := strings.Index(source[from:], end)
		if to < 0 {
			t.Fatalf("legacy helper no longer contains %q after %q", end, start)
		}
		return source[from : from+to]
	}
	conversations = between("const conversations = db.prepare(`", "`).all();")
	messages = between("const messagesSQL = `", "`;") +
		" ORDER BY (CASE WHEN COALESCE(m.sent_at, 0) > 0 THEN m.sent_at ELSE m.received_at END) ASC, m.id ASC"
	return conversations, messages
}

func queryJSONRows(t *testing.T, database *sql.DB, query string) []map[string]any {
	t.Helper()
	rows, err := database.Query(query)
	if err != nil {
		t.Fatalf("legacy query: %v", err)
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			t.Fatal(err)
		}
		row := map[string]any{}
		for i, column := range columns {
			if raw, ok := values[i].([]byte); ok {
				values[i] = string(raw)
			}
			row[column] = values[i]
		}
		out = append(out, row)
	}
	return out
}

type diffRow struct {
	conversation string
	identity     string // incoming: the remote ID; outgoing: "out:<sent ms>"
	timestamp    int64
	outgoing     bool
	body         string
}

func (r diffRow) key() string { return r.conversation + "|" + r.identity }

func diffArchive() fixture.Archive {
	m := func(row int64, conversation, kind, body string, author string) fixture.Message {
		return fixture.Message{RowID: row, ID: fmt.Sprintf("m%02d", row), ConversationID: conversation, Type: kind, Body: body, SentAt: diffBaseMS + row*1000, ReceivedAt: diffBaseMS + row*1000 + 7, SourceServiceID: author}
	}
	photo := m(4, "conv-peer", "incoming", "", diffPeerACI)
	photo.Attachments = []fixture.Attachment{{ContentType: "image/jpeg", FileName: "synthetic.jpg", Path: "ab/synthetic"}}
	video := m(5, "conv-group", "outgoing", "", "")
	video.Attachments = []fixture.Attachment{{ContentType: "video/mp4"}}
	sticker := m(6, "conv-other", "incoming", "", diffOtherACI)
	sticker.Sticker = true
	deleted := m(7, "conv-peer", "incoming", "", diffPeerACI)
	deleted.DeletedForEveryone = true
	unsent := m(8, "conv-peer", "outgoing", "synthetic unsent", "")
	unsent.SendState = map[string]string{"conv-peer": "Failed"}
	noTime := m(9, "conv-peer", "incoming", "synthetic old", diffPeerACI)
	noTime.SentAt = 0
	quoted := m(10, "conv-group", "incoming", "", diffOtherACI)
	quoted.Attachments = []fixture.Attachment{{Type: "quote", ContentType: "image/png"}, {Type: "attachment", Order: 0, ContentType: "audio/aac"}}
	selfIncoming := m(11, "conv-group", "incoming", "synthetic echo", diffSelfACI)
	byNumber := m(12, "conv-peer", "incoming", "synthetic by number", diffPeerACI)
	return fixture.Archive{
		SelfNumber: diffSelf, SelfACI: diffSelfACI,
		Conversations: []fixture.Conversation{
			{ID: "conv-self", Type: "private", E164: diffSelf, ServiceID: diffSelfACI},
			{ID: "conv-peer", Type: "private", E164: diffPeerE164, ServiceID: diffPeerACI, Name: "Synthetic Peer"},
			{ID: "conv-other", Type: "private", ServiceID: diffOtherACI, ProfileFull: "Synthetic Other"},
			{ID: "conv-group", Type: "group", GroupID: diffGroupID, Name: "Synthetic Group", Members: []string{diffPeerACI, diffOtherACI}, MembersV2: []string{diffSelfACI, diffPeerACI, diffOtherACI}},
		},
		Messages: []fixture.Message{
			m(1, "conv-peer", "incoming", "synthetic hello", diffPeerACI),
			m(2, "conv-peer", "outgoing", "synthetic reply", ""),
			m(3, "conv-group", "incoming", "synthetic group", diffOtherACI),
			photo, video, sticker, deleted, unsent, noTime, quoted, selfIncoming, byNumber,
			m(13, "conv-other", "story", "synthetic story", diffOtherACI),
			m(14, "conv-other", "group-v2-change", "", diffOtherACI),
			m(15, "conv-self", "outgoing", "synthetic note", ""),
		},
	}
}

func TestSignalDesktopV2FeedMatchesLegacyImporter(t *testing.T) {
	supportDir := t.TempDir()
	dbPath := filepath.Join(supportDir, "sql", "db.sqlite")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		t.Fatal(err)
	}
	archive := diffArchive()
	if err := fixture.Write(dbPath, archive); err != nil {
		t.Fatal(err)
	}
	source, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()

	// Legacy: its own SQL over the archive, through its helper seam.
	conversationsSQL, messagesSQL := legacySQL(t)
	exported, err := json.Marshal(map[string]any{
		"conversations": queryJSONRows(t, source, conversationsSQL),
		"messages":      queryJSONRows(t, source, messagesSQL),
	})
	if err != nil {
		t.Fatal(err)
	}
	restore := []func(){}
	defer func() {
		for _, undo := range restore {
			undo()
		}
	}()
	originalRun, originalNode, originalAddon, originalKey, originalWrite := runSignalDesktopHelper, signalDesktopNodePathFn, signalDesktopAddonPathFn, signalDesktopSQLKeyFn, writeSignalDesktopHelperFn
	restore = append(restore, func() {
		runSignalDesktopHelper, signalDesktopNodePathFn, signalDesktopAddonPathFn, signalDesktopSQLKeyFn, writeSignalDesktopHelperFn = originalRun, originalNode, originalAddon, originalKey, originalWrite
	})
	runSignalDesktopHelper = func(context.Context, string, string, ...string) ([]byte, error) { return exported, nil }
	signalDesktopNodePathFn = func() (string, error) { return "node", nil }
	signalDesktopAddonPathFn = func() (string, error) { return "addon", nil }
	signalDesktopSQLKeyFn = func(string) (string, error) { return "00", nil }
	writeSignalDesktopHelperFn = func() (string, func(), error) { return "helper", func() {}, nil }
	legacyPath := filepath.Join(t.TempDir(), "messages.db")
	legacyStore, err := db.New(legacyPath)
	if err != nil {
		t.Fatal(err)
	}
	defer legacyStore.Close()
	if _, err := (&SignalDesktop{SupportDir: supportDir, MyAddress: diffSelf, SinceMS: -1}).ImportFromDB(legacyStore); err != nil {
		t.Fatalf("legacy import: %v", err)
	}
	legacyDB, err := sql.Open("sqlite", "file:"+legacyPath+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer legacyDB.Close()
	legacy := map[string]diffRow{}
	for _, row := range queryJSONRows(t, legacyDB, "SELECT conversation_id, source_id, timestamp_ms, is_from_me, body FROM messages WHERE source_platform = 'signal'") {
		r := diffRow{conversation: row["conversation_id"].(string), timestamp: row["timestamp_ms"].(int64), outgoing: row["is_from_me"].(int64) == 1, body: row["body"].(string)}
		r.identity = row["source_id"].(string)
		if r.outgoing {
			if r.identity != v2keys.SignalLocalAlias(r.conversation, r.timestamp) {
				t.Fatalf("legacy outgoing source ID %q is not the local alias", r.identity)
			}
			r.identity = "out:" + strconv.FormatInt(r.timestamp, 10)
		}
		legacy[r.key()] = r
	}

	// v2: the reader over the same archive, through the codec and the live
	// decoder, with nothing in v2 yet.
	facts := signaldesktop.NewFacts("signal-primary", diffSelf, nil, nil, nil)
	v2 := map[string]diffRow{}
	stats, err := (&signaldesktop.Reader{Runner: signaldesktop.SQLRunner{DB: source}}).Scan(context.Background(), facts, signaldesktop.ScanOptions{}, func(chunk signaldesktop.Chunk) error {
		record, err := ingest.SignalDesktopHistoryRecord("signal-primary", 0, chunk.HistorySnapshot(diffSelf), time.UnixMilli(diffBaseMS))
		if err != nil {
			return err
		}
		events, err := ingest.NewSignalDesktopHistoryDecoder().Decode(context.Background(), record)
		if err != nil {
			return err
		}
		for _, event := range events {
			if event.Kind != bridge.EventMessage {
				continue
			}
			message := event.Message
			r := diffRow{conversation: message.RemoteConversationID, identity: message.RemoteMessageID, timestamp: message.OccurredAt.UnixMilli(), outgoing: message.Direction == "outgoing", body: message.Body}
			if r.outgoing {
				r.identity = "out:" + message.RemoteMessageID
			}
			v2[r.key()] = r
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// Rows only legacy has, each for a named reason the v2 reader counted.
	peer := "signal:" + diffPeerE164
	group := "signal-group:" + diffGroupID
	other := "signal:" + diffOtherACI
	incoming := func(conversation, author string, row int64) string {
		return conversation + "|" + v2keys.SignalIncomingSourceID(conversation, author, diffBaseMS+row*1000)
	}
	wantLegacyOnly := map[string]string{
		incoming(peer, diffPeerE164, 7):                                                 "deleted for everyone",
		peer + "|out:" + strconv.FormatInt(diffBaseMS+8000, 10):                         "outgoing that never sent",
		peer + "|" + v2keys.SignalIncomingSourceID(peer, diffPeerE164, diffBaseMS+9007): "no sent time (legacy falls back to received_at)",
		incoming(group, diffSelf, 11):                                                   "authored by the account itself",
		incoming(other, diffOtherACI, 13):                                               "a story, which live never projects",
	}
	for key, row := range legacy {
		if _, ok := v2[key]; ok {
			continue
		}
		if _, expected := wantLegacyOnly[key]; !expected {
			t.Errorf("legacy row %+v has no v2 counterpart and no named reason", row)
		}
	}
	for key, reason := range wantLegacyOnly {
		if _, ok := legacy[key]; !ok {
			t.Errorf("expected legacy-only row (%s) is missing from legacy", reason)
		}
		if _, ok := v2[key]; ok {
			t.Errorf("v2 has the row legacy alone should have (%s)", reason)
		}
	}
	for _, reason := range []string{"deleted", "outgoing_unsent", "no_timestamp", "self_incoming", "story", "system"} {
		if stats.RowsSkippedByReason[reason] == 0 {
			t.Errorf("v2 reader did not count a %s skip: %v", reason, stats.RowsSkippedByReason)
		}
	}

	// Rows both have: same conversation, identity, time and direction; same
	// body, except where v2 follows the live decoder's placeholders.
	bodyDiffers := map[string][2]string{
		incoming(other, diffOtherACI, 6):  {"[Attachment]", "[Sticker]"},
		incoming(group, diffOtherACI, 10): {"[Photo]", "[Audio]"}, // legacy took the quote's thumbnail as the attachment
	}
	var matched []string
	for key, row := range v2 {
		old, ok := legacy[key]
		if !ok {
			t.Errorf("v2 row %+v has no legacy counterpart", row)
			continue
		}
		matched = append(matched, key)
		if old.timestamp != row.timestamp || old.outgoing != row.outgoing {
			t.Errorf("row %s: legacy %+v v2 %+v", key, old, row)
		}
		if want, ok := bodyDiffers[key]; ok {
			if old.body != want[0] || row.body != want[1] {
				t.Errorf("row %s body: legacy %q v2 %q, want %q / %q", key, old.body, row.body, want[0], want[1])
			}
			continue
		}
		if old.body != row.body {
			t.Errorf("row %s body: legacy %q v2 %q", key, old.body, row.body)
		}
	}
	sort.Strings(matched)
	if len(matched) != 9 {
		t.Fatalf("matched %d rows, want 9: %v", len(matched), matched)
	}
}

// Signal Desktop declares messages.source as STRING, so SQLite stores a
// phone-number source as an integer. The legacy export then fails as a whole;
// the v2 reader restores the number.
func TestSignalDesktopNumericSourceBreaksLegacyButNotV2(t *testing.T) {
	supportDir := t.TempDir()
	dbPath := filepath.Join(supportDir, "sql", "db.sqlite")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		t.Fatal(err)
	}
	archive := diffArchive()
	archive.Messages = []fixture.Message{{RowID: 1, ID: "m1", ConversationID: "conv-peer", Type: "incoming", Body: "synthetic", SentAt: diffBaseMS, Source: diffPeerE164}}
	if err := fixture.Write(dbPath, archive); err != nil {
		t.Fatal(err)
	}
	source, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	var storedType string
	if err := source.QueryRow("SELECT typeof(source) FROM messages").Scan(&storedType); err != nil || storedType != "integer" {
		t.Fatalf("fixture stored source as %q (%v), want integer like Signal Desktop", storedType, err)
	}
	_, messagesSQL := legacySQL(t)
	exported, err := json.Marshal(map[string]any{"conversations": []any{}, "messages": queryJSONRows(t, source, messagesSQL)})
	if err != nil {
		t.Fatal(err)
	}
	var legacy signalDesktopExport
	if err := json.Unmarshal(exported, &legacy); err == nil {
		t.Fatal("the legacy export decoded a numeric source; this test's premise no longer holds")
	}
	var chunks []signaldesktop.Chunk
	if _, err := (&signaldesktop.Reader{Runner: signaldesktop.SQLRunner{DB: source}}).Scan(context.Background(), signaldesktop.NewFacts("signal-primary", diffSelf, nil, nil, nil), signaldesktop.ScanOptions{}, func(chunk signaldesktop.Chunk) error {
		chunks = append(chunks, chunk)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 1 || chunks[0].Messages[0].AuthorAddress != diffPeerE164 {
		t.Fatalf("chunks = %+v, want the message by %s", chunks, diffPeerE164)
	}
}
