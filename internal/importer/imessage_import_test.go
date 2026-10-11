package importer

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/maxghenis/openmessage/internal/db"

	_ "modernc.org/sqlite"
)

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

// addChat adds a chat; a nil name stores NULL, as Messages does for some.
func (f *fakeChatDB) addChat(guid string, name *string, group bool, handles ...string) int64 {
	style := 45
	if group {
		style = 43
	}
	chatID := f.exec(`INSERT INTO chat (guid, style, display_name) VALUES (?, ?, ?)`, guid, style, name)
	for _, h := range handles {
		handleID := f.exec(`INSERT INTO handle (id, uncanonicalized_id) VALUES (?, ?)`, h, h)
		f.exec(`INSERT INTO chat_handle_join (chat_id, handle_id) VALUES (?, ?)`, chatID, handleID)
	}
	return chatID
}

// addMessage adds a message at unixSeconds; attributed stores the body only
// in attributedBody, as modern Messages does.
func (f *fakeChatDB) addMessage(chatID int64, guid, text string, attributed bool, unixSeconds int64, fromMe bool) {
	var textArg any = text
	var blob any
	if attributed {
		textArg = nil
		blob = makeAttributedBody(text)
	}
	date := (unixSeconds - coreDataEpoch) * 1_000_000_000
	rowID := f.exec(`INSERT INTO message (guid, text, attributedBody, date, is_from_me) VALUES (?, ?, ?, ?, ?)`,
		guid, textArg, blob, date, fromMe)
	f.exec(`INSERT INTO chat_message_join (chat_id, message_id) VALUES (?, ?)`, chatID, rowID)
}

// TestIMessageImportFinishes pins the two loadChats fixes in #231. Since
// ed03e741 (2026-02-22) the importer held one chat.db connection and queried
// participants with the chat rows still open, so it never returned on a
// chat.db with any chat; and a chat whose display_name is NULL (13 of 2,020
// on the author's Mac) failed its scan and was dropped.
func TestIMessageImportFinishes(t *testing.T) {
	chat := newFakeChatDB(t)
	empty := ""
	direct := chat.addChat("iMessage;-;+15550000001", &empty, false, "+15550000001")
	group := chat.addChat("iMessage;+;chat123", nil, true, "+15550000001", "bob@example.com")
	chat.addMessage(direct, "m1", "hello", false, 1_700_000_000, false)
	chat.addMessage(direct, "m2", "attributed only", true, 1_700_000_060, true)
	chat.addMessage(group, "m3", "group hi", false, 1_700_000_120, false)

	store, err := db.New(filepath.Join(t.TempDir(), "messages.db"))
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	defer store.Close()

	type outcome struct {
		result *ImportResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := (&IMessage{DBPath: chat.path, MyName: "Me"}).ImportFromDB(store)
		done <- outcome{result, err}
	}()
	var got outcome
	select {
	case got = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("ImportFromDB did not return: nested chat.db query on a single connection")
	}
	if got.err != nil {
		t.Fatalf("ImportFromDB: %v", got.err)
	}
	if got.result.ConversationsCreated != 2 || got.result.MessagesImported != 3 || len(got.result.Errors) != 0 {
		t.Fatalf("result = %+v, want both chats and all three messages", got.result)
	}

	conv, err := store.GetConversation("imessage:iMessage;+;chat123")
	if err != nil {
		t.Fatalf("NULL-name group chat dropped: %v", err)
	}
	if !conv.IsGroup || conv.Name != "+15550000001, bob@example.com" {
		t.Fatalf("group conversation = %+v", conv)
	}
	m2, err := store.GetMessageByID("imessage:m2")
	if err != nil || m2 == nil {
		t.Fatalf("attributedBody-only message missing: %v", err)
	}
	if m2.Body != "attributed only" || !m2.IsFromMe || m2.SenderName != "Me" || m2.TimestampMS != 1_700_000_060_000 {
		t.Fatalf("m2 = %+v", m2)
	}
}
