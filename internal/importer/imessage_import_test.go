package importer

import (
	"database/sql"
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/maxghenis/openmessage/internal/db"

	_ "modernc.org/sqlite"
)

// Invariants pinned here for IMessage.ImportFromDB (legacy store):
//   M1  Re-importing an unchanged chat.db writes nothing: no commit at all,
//       every message counted as a duplicate, unread counts untouched.
//   M2  Polling with one reused importer after each batch of new chat.db rows
//       ends in the same store as one full import of the final chat.db,
//       including rows inserted late with older dates and tapbacks.
//   M3  A full import over a store that polling built changes nothing
//       (what a daemon restart does).
//   M4  A tapback whose quoted message is stored becomes a reaction, not a
//       text row; a later removal wins.
//   M5  A chat.db whose rows end below AfterRowID is read in full.

// fakeChatDB is a minimal Messages chat.db with the real table and column
// names the importer reads.
type fakeChatDB struct {
	t    *testing.T
	path string
	db   *sql.DB
}

func newFakeChatDB(t *testing.T) *fakeChatDB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "chat.db")
	conn, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open fake chat.db: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	for _, stmt := range []string{
		`CREATE TABLE chat (ROWID INTEGER PRIMARY KEY AUTOINCREMENT, guid TEXT UNIQUE NOT NULL, style INTEGER, display_name TEXT)`,
		`CREATE TABLE handle (ROWID INTEGER PRIMARY KEY AUTOINCREMENT, id TEXT NOT NULL, uncanonicalized_id TEXT)`,
		`CREATE TABLE chat_handle_join (chat_id INTEGER, handle_id INTEGER)`,
		`CREATE TABLE message (ROWID INTEGER PRIMARY KEY AUTOINCREMENT, guid TEXT UNIQUE NOT NULL, text TEXT, attributedBody BLOB, handle_id INTEGER DEFAULT 0, date INTEGER, is_from_me INTEGER DEFAULT 0)`,
		`CREATE TABLE chat_message_join (chat_id INTEGER, message_id INTEGER)`,
	} {
		if _, err := conn.Exec(stmt); err != nil {
			t.Fatalf("fake chat.db schema: %v", err)
		}
	}
	return &fakeChatDB{t: t, path: path, db: conn}
}

func (f *fakeChatDB) exec(query string, args ...any) int64 {
	f.t.Helper()
	res, err := f.db.Exec(query, args...)
	if err != nil {
		f.t.Fatalf("fake chat.db: %v", err)
	}
	id, _ := res.LastInsertId()
	return id
}

func (f *fakeChatDB) addHandle(id string) int64 {
	return f.exec(`INSERT INTO handle (id, uncanonicalized_id) VALUES (?, ?)`, id, id)
}

// addChat adds a chat; a nil name stores NULL, as Messages does for some.
func (f *fakeChatDB) addChat(guid string, name *string, group bool, handles ...int64) int64 {
	style := 45
	if group {
		style = 43
	}
	chatID := f.exec(`INSERT INTO chat (guid, style, display_name) VALUES (?, ?, ?)`, guid, style, name)
	for _, h := range handles {
		f.exec(`INSERT INTO chat_handle_join (chat_id, handle_id) VALUES (?, ?)`, chatID, h)
	}
	return chatID
}

// addMessage adds a message at unixSeconds; text "" with attributed set
// stores the body only in attributedBody, as modern Messages does.
func (f *fakeChatDB) addMessage(chatID int64, guid, text string, attributed bool, unixSeconds int64, fromMe bool, handle int64) int64 {
	var textArg any = text
	var blob any
	if attributed {
		textArg = nil
		blob = makeAttributedBody(text)
	}
	if fromMe {
		handle = 0
	}
	date := (unixSeconds - coreDataEpoch) * 1_000_000_000
	rowID := f.exec(`INSERT INTO message (guid, text, attributedBody, handle_id, date, is_from_me) VALUES (?, ?, ?, ?, ?, ?)`,
		guid, textArg, blob, handle, date, fromMe)
	f.exec(`INSERT INTO chat_message_join (chat_id, message_id) VALUES (?, ?)`, chatID, rowID)
	return rowID
}

func newIMessageTestStore(t *testing.T) (*db.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "messages.db")
	store, err := db.New(path)
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store, path
}

// storeDump is everything an iMessage import can write, read through a
// second connection.
type storeDump struct {
	Messages      []string
	Conversations []string
}

func dumpLegacyStore(t *testing.T, path string) storeDump {
	t.Helper()
	conn, err := sql.Open("sqlite", path+"?mode=ro")
	if err != nil {
		t.Fatalf("open store for dump: %v", err)
	}
	defer conn.Close()
	read := func(query string) []string {
		rows, err := conn.Query(query)
		if err != nil {
			t.Fatalf("dump: %v", err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Fatalf("dump scan: %v", err)
			}
			out = append(out, line)
		}
		return out
	}
	return storeDump{
		Messages: read(`SELECT message_id || '|' || conversation_id || '|' || sender_name || '|' || sender_number || '|' ||
			body || '|' || timestamp_ms || '|' || is_from_me || '|' || status || '|' || IFNULL(reactions, '') || '|' ||
			source_platform || '|' || source_id FROM messages ORDER BY message_id`),
		Conversations: read(`SELECT conversation_id || '|' || name || '|' || is_group || '|' || participants || '|' ||
			last_message_ts || '|' || unread_count || '|' || source_platform FROM conversations ORDER BY conversation_id`),
	}
}

// dataVersion is SQLite's per-connection counter of commits made by other
// connections; it moves iff the store committed something.
func dataVersion(t *testing.T, conn *sql.DB) int64 {
	t.Helper()
	var v int64
	if err := conn.QueryRow(`PRAGMA data_version`).Scan(&v); err != nil {
		t.Fatalf("data_version: %v", err)
	}
	return v
}

func importIMessage(t *testing.T, imp *IMessage, store *db.Store) *ImportResult {
	t.Helper()
	result, err := imp.ImportFromDB(store)
	if err != nil {
		t.Fatalf("ImportFromDB: %v", err)
	}
	if len(result.Errors) > 0 {
		t.Fatalf("ImportFromDB errors: %v", result.Errors)
	}
	return result
}

func strPtr(s string) *string { return &s }

func TestIMessageImportStoresChatsAndMessages(t *testing.T) {
	chat := newFakeChatDB(t)
	alice := chat.addHandle("+15550000001")
	bob := chat.addHandle("bob@example.com")
	direct := chat.addChat("iMessage;-;+15550000001", strPtr(""), false, alice)
	// Messages leaves display_name NULL on some chats; they used to be skipped.
	group := chat.addChat("iMessage;+;chat123", nil, true, alice, bob)
	chat.addMessage(direct, "m1", "hello", false, 1_700_000_000, false, alice)
	chat.addMessage(direct, "m2", "attributed only", true, 1_700_000_060, true, 0)
	chat.addMessage(direct, "m3", "", false, 1_700_000_120, false, alice) // no body: skipped
	chat.addMessage(group, "m4", "group hi", false, 1_700_000_180, false, bob)

	store, _ := newIMessageTestStore(t)
	imp := &IMessage{DBPath: chat.path, MyName: "Me"}
	result := importIMessage(t, imp, store)
	if result.ConversationsCreated != 2 || result.MessagesImported != 3 || result.MessagesDuplicate != 0 {
		t.Fatalf("result = %+v, want 2 conversations, 3 imported", result)
	}
	if imp.AfterRowID != 4 {
		t.Fatalf("AfterRowID = %d, want 4", imp.AfterRowID)
	}

	got, err := store.GetMessageByID("imessage:m2")
	if err != nil || got == nil {
		t.Fatalf("m2 missing: %v", err)
	}
	if got.Body != "attributed only" || !got.IsFromMe || got.SenderName != "Me" || got.TimestampMS != 1_700_000_060_000 {
		t.Fatalf("m2 = %+v", got)
	}
	conv, err := store.GetConversation("imessage:iMessage;+;chat123")
	if err != nil {
		t.Fatalf("group conversation missing: %v", err)
	}
	if !conv.IsGroup || conv.Name != "+15550000001, bob@example.com" {
		t.Fatalf("group conversation = %+v", conv)
	}
}

func TestIMessageReimportWritesNothing(t *testing.T) {
	chat := newFakeChatDB(t)
	alice := chat.addHandle("+15550000001")
	direct := chat.addChat("iMessage;-;+15550000001", strPtr("Alice"), false, alice)
	for i := 0; i < 5; i++ {
		chat.addMessage(direct, fmt.Sprintf("m%d", i), fmt.Sprintf("body %d", i), i%2 == 0, 1_700_000_000+int64(i)*60, i%3 == 0, alice)
	}

	store, path := newIMessageTestStore(t)
	importIMessage(t, &IMessage{DBPath: chat.path}, store)

	// A reader marks the thread unread-count 2; a rewrite of the chat would
	// reset it to 0.
	observer, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer observer.Close()
	if _, err := observer.Exec(`UPDATE conversations SET unread_count = 2 WHERE conversation_id = ?`, "imessage:iMessage;-;+15550000001"); err != nil {
		t.Fatal(err)
	}
	before := dumpLegacyStore(t, path)
	version := dataVersion(t, observer)

	for _, imp := range []*IMessage{{DBPath: chat.path}, {DBPath: chat.path}} {
		result := importIMessage(t, imp, store)
		if result.MessagesImported != 0 || result.ConversationsCreated != 0 || result.MessagesDuplicate != 5 {
			t.Fatalf("re-import result = %+v, want 5 duplicates and nothing else", result)
		}
		// The same importer again has nothing past its high-water mark.
		again := importIMessage(t, imp, store)
		if again.MessagesImported != 0 || again.MessagesDuplicate != 0 {
			t.Fatalf("poll with nothing new = %+v", again)
		}
	}
	if v := dataVersion(t, observer); v != version {
		t.Fatalf("re-import committed to the store (data_version %d -> %d)", version, v)
	}
	if after := dumpLegacyStore(t, path); !reflect.DeepEqual(before, after) {
		t.Fatalf("re-import changed the store:\nbefore %v\nafter  %v", before, after)
	}
}

func TestIMessageImportAppliesTapbacks(t *testing.T) {
	chat := newFakeChatDB(t)
	alice := chat.addHandle("+15550000001")
	direct := chat.addChat("iMessage;-;+15550000001", strPtr("Alice"), false, alice)
	chat.addMessage(direct, "m1", "see you then", false, 1_700_000_000, true, 0)
	chat.addMessage(direct, "t1", "Loved “see you then”", false, 1_700_000_060, false, alice)
	chat.addMessage(direct, "t2", "Liked “nothing like this was sent”", false, 1_700_000_090, false, alice)

	store, path := newIMessageTestStore(t)
	imp := &IMessage{DBPath: chat.path}
	result := importIMessage(t, imp, store)
	if result.MessagesImported != 3 {
		t.Fatalf("result = %+v, want the message, the reaction and the unmatched tapback", result)
	}
	if m, _ := store.GetMessageByID("imessage:t1"); m != nil {
		t.Fatalf("applied tapback stored as text: %+v", m)
	}
	if m, _ := store.GetMessageByID("imessage:t2"); m == nil {
		t.Fatal("a tapback with no target must stay as text")
	}
	target, _ := store.GetMessageByID("imessage:m1")
	if target == nil || !strings.Contains(target.Reactions, "+15550000001") {
		t.Fatalf("target reactions = %+v", target)
	}

	chat.addMessage(direct, "t3", "Removed a heart from “see you then”", false, 1_700_000_120, false, alice)
	importIMessage(t, imp, store)
	target, _ = store.GetMessageByID("imessage:m1")
	if strings.Contains(target.Reactions, "+15550000001") {
		t.Fatalf("removal did not win: %q", target.Reactions)
	}

	// A restart's full read replays both tapbacks and lands where it was.
	before := dumpLegacyStore(t, path)
	importIMessage(t, &IMessage{DBPath: chat.path}, store)
	if after := dumpLegacyStore(t, path); !reflect.DeepEqual(before, after) {
		t.Fatalf("full re-read changed the store:\nbefore %v\nafter  %v", before, after)
	}
}

func TestIMessageImportRereadsAReplacedChatDB(t *testing.T) {
	first := newFakeChatDB(t)
	alice := first.addHandle("+15550000001")
	chatID := first.addChat("iMessage;-;+15550000001", strPtr("Alice"), false, alice)
	for i := 0; i < 4; i++ {
		first.addMessage(chatID, fmt.Sprintf("old%d", i), fmt.Sprintf("old %d", i), false, 1_700_000_000+int64(i), false, alice)
	}
	store, _ := newIMessageTestStore(t)
	imp := &IMessage{DBPath: first.path}
	importIMessage(t, imp, store)
	if imp.AfterRowID != 4 {
		t.Fatalf("AfterRowID = %d", imp.AfterRowID)
	}

	// A restored chat.db holds fewer rows: ROWIDs end below the mark.
	restored := newFakeChatDB(t)
	alice = restored.addHandle("+15550000001")
	chatID = restored.addChat("iMessage;-;+15550000001", strPtr("Alice"), false, alice)
	restored.addMessage(chatID, "new0", "after the restore", false, 1_800_000_000, false, alice)
	imp.DBPath = restored.path
	result := importIMessage(t, imp, store)
	if result.MessagesImported != 1 || imp.AfterRowID != 1 {
		t.Fatalf("result = %+v AfterRowID = %d, want the restored row read", result, imp.AfterRowID)
	}
}

func TestIMessageImportKeepsTheMarkWhenItFails(t *testing.T) {
	chat := newFakeChatDB(t)
	alice := chat.addHandle("+15550000001")
	chatID := chat.addChat("iMessage;-;+15550000001", strPtr("Alice"), false, alice)
	chat.addMessage(chatID, "m1", "hello", false, 1_700_000_000, false, alice)

	store, _ := newIMessageTestStore(t)
	store.Close()
	imp := &IMessage{DBPath: chat.path}
	result, err := imp.ImportFromDB(store)
	if err != nil {
		t.Fatalf("ImportFromDB: %v", err)
	}
	if len(result.Errors) == 0 {
		t.Fatal("expected per-row errors against a closed store")
	}
	if imp.AfterRowID != 0 {
		t.Fatalf("AfterRowID advanced to %d past rows that failed", imp.AfterRowID)
	}
}

// TestIMessagePollingMatchesAFullImport is M2/M3 over seeded random
// histories: chats appear over time, some rows arrive late with older dates
// (iCloud), some are attributedBody-only or empty, and tapbacks add and
// remove reactions on earlier messages.
func TestIMessagePollingMatchesAFullImport(t *testing.T) {
	const cases = 60
	kinds := map[string]int{}
	for seed := uint64(1); seed <= cases; seed++ {
		r := rand.New(rand.NewPCG(seed, 0x5eed))
		chat := newFakeChatDB(t)
		handles := []int64{chat.addHandle("+15550000001"), chat.addHandle("+15550000002"), chat.addHandle("carol@example.com")}

		var chats []int64
		bodies := map[int64][]string{} // chat -> stored, quotable bodies
		newestDate := int64(1_700_000_000)
		polled, pollPath := newIMessageTestStore(t)
		poller := &IMessage{DBPath: chat.path, MyName: "Me"}
		msgSeq := 0

		steps := 1 + r.IntN(6)
		for step := 0; step < steps; step++ {
			if len(chats) == 0 || r.IntN(4) == 0 {
				var name *string
				switch r.IntN(3) {
				case 0:
					name = nil
				case 1:
					name = strPtr("")
				default:
					name = strPtr(fmt.Sprintf("Chat %d", len(chats)))
				}
				chats = append(chats, chat.addChat(fmt.Sprintf("iMessage;-;chat%d", len(chats)), name, r.IntN(2) == 0, handles[r.IntN(len(handles))]))
			}
			for n := 1 + r.IntN(5); n > 0; n-- {
				chatID := chats[r.IntN(len(chats))]
				msgSeq++
				guid := fmt.Sprintf("g%d", msgSeq)
				fromMe := r.IntN(3) == 0
				handle := handles[r.IntN(len(handles))]
				// Kind from the high bits of a fresh draw (see the LCG note in
				// the review guide): every branch must be exercised.
				switch kind := r.Uint64() >> 62; {
				case kind == 0 && len(bodies[chatID]) > 0:
					// Tapback on an earlier message, dated after everything.
					quoted := bodies[chatID][r.IntN(len(bodies[chatID]))]
					verb := []string{"Loved “%s”", "Liked “%s”", "Removed a heart from “%s”", "Removed a like from “%s”"}[r.IntN(4)]
					newestDate += 60
					kinds["tapback"]++
					chat.addMessage(chatID, guid, fmt.Sprintf(verb, quoted), false, newestDate, fromMe, handle)
				case kind == 1:
					// Late iCloud insert: an older date with a fresh body
					// nothing quotes yet.
					kinds["late"]++
					chat.addMessage(chatID, guid, fmt.Sprintf("late %d", msgSeq), r.IntN(2) == 0, newestDate-int64(1+r.IntN(5000)), fromMe, handle)
				case kind == 2:
					kinds["empty"]++
					chat.addMessage(chatID, guid, "", false, newestDate+1, fromMe, handle)
				default:
					body := fmt.Sprintf("message %d", msgSeq)
					newestDate += 60
					kinds["text"]++
					chat.addMessage(chatID, guid, body, r.IntN(2) == 0, newestDate, fromMe, handle)
					bodies[chatID] = append(bodies[chatID], body)
				}
			}
			importIMessage(t, poller, polled)
		}

		full, fullPath := newIMessageTestStore(t)
		importIMessage(t, &IMessage{DBPath: chat.path, MyName: "Me"}, full)
		pollDump, fullDump := dumpLegacyStore(t, pollPath), dumpLegacyStore(t, fullPath)
		if !reflect.DeepEqual(pollDump, fullDump) {
			t.Fatalf("seed %d: polling and a full import disagree:\npoll %v\nfull %v", seed, pollDump, fullDump)
		}

		// M3: a restart's full read over the polled store changes nothing.
		importIMessage(t, &IMessage{DBPath: chat.path, MyName: "Me"}, polled)
		if again := dumpLegacyStore(t, pollPath); !reflect.DeepEqual(pollDump, again) {
			t.Fatalf("seed %d: full re-read changed the polled store:\nbefore %v\nafter  %v", seed, pollDump, again)
		}
	}
	for _, kind := range []string{"tapback", "late", "empty", "text"} {
		if kinds[kind] < 10 {
			t.Fatalf("generator exercised %q only %d times: %v", kind, kinds[kind], kinds)
		}
	}
}
