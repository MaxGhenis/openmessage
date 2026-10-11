package importer

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/maxghenis/openmessage/internal/db"

	_ "modernc.org/sqlite"
)

// nativeChatDB is a Messages chat.db with the real names, column types,
// indexes and delete triggers of the tables and columns the importer reads,
// in WAL mode as Messages keeps it (schema read from a real chat.db on
// 2026-10-11).
type nativeChatDB struct {
	t    *testing.T
	path string
	db   *sql.DB
}

var nativeChatSchema = []string{
	`PRAGMA journal_mode=WAL`,
	`CREATE TABLE chat (ROWID INTEGER PRIMARY KEY AUTOINCREMENT, guid TEXT UNIQUE NOT NULL, style INTEGER, display_name TEXT)`,
	`CREATE TABLE handle (ROWID INTEGER PRIMARY KEY AUTOINCREMENT UNIQUE, id TEXT NOT NULL, uncanonicalized_id TEXT)`,
	`CREATE TABLE chat_handle_join (chat_id INTEGER REFERENCES chat (ROWID) ON DELETE CASCADE, handle_id INTEGER REFERENCES handle (ROWID) ON DELETE CASCADE, UNIQUE(chat_id, handle_id))`,
	`CREATE TABLE message (ROWID INTEGER PRIMARY KEY AUTOINCREMENT, guid TEXT UNIQUE NOT NULL, text TEXT,
		handle_id INTEGER DEFAULT 0, attributedBody BLOB, date INTEGER, is_from_me INTEGER DEFAULT 0,
		associated_message_guid TEXT, associated_message_type INTEGER DEFAULT 0, associated_message_emoji TEXT)`,
	`CREATE TABLE chat_message_join (chat_id INTEGER REFERENCES chat (ROWID) ON DELETE CASCADE, message_id INTEGER REFERENCES message (ROWID) ON DELETE CASCADE, message_date INTEGER DEFAULT 0, PRIMARY KEY (chat_id, message_id))`,
	`CREATE INDEX message_idx_date ON message(date)`,
	`CREATE INDEX message_idx_handle ON message(handle_id, date)`,
	`CREATE INDEX message_idx_associated_message2 ON message(associated_message_guid) WHERE associated_message_guid IS NOT NULL`,
	`CREATE INDEX chat_message_join_idx_message_id_only ON chat_message_join(message_id)`,
	`CREATE INDEX chat_message_join_idx_message_date_id_chat_id ON chat_message_join(chat_id, message_date, message_id)`,
	// Messages' own triggers (shortened): a deleted chat drops its joins, a
	// deleted message drops its joins and the reactions aimed at it.
	`CREATE TRIGGER after_delete_on_chat AFTER DELETE ON chat BEGIN DELETE FROM chat_message_join WHERE chat_id = OLD.ROWID; END`,
	`CREATE TRIGGER after_delete_on_message_join AFTER DELETE ON message BEGIN DELETE FROM chat_message_join WHERE message_id = OLD.ROWID; END`,
	`CREATE TRIGGER delete_associated_messages_after_delete_on_message AFTER DELETE ON message BEGIN
		DELETE FROM message WHERE OLD.associated_message_guid IS NULL AND associated_message_guid IS NOT NULL
			AND (associated_message_guid = OLD.guid OR associated_message_guid = 'bp:' || OLD.guid
				OR associated_message_guid LIKE 'p:%/' || OLD.guid);
	END`,
}

func newNativeChatDB(t *testing.T) *nativeChatDB {
	t.Helper()
	return newNativeChatDBAt(t, filepath.Join(t.TempDir(), "chat.db"))
}

func newNativeChatDBAt(t *testing.T, path string) *nativeChatDB {
	t.Helper()
	f := &nativeChatDB{t: t, path: path}
	f.open()
	for _, stmt := range nativeChatSchema {
		f.exec(stmt)
	}
	return f
}

func (f *nativeChatDB) open() {
	conn, err := sql.Open("sqlite", f.path)
	if err != nil {
		f.t.Fatalf("open chat.db: %v", err)
	}
	conn.SetMaxOpenConns(1)
	f.db = conn
	f.t.Cleanup(func() { conn.Close() })
}

func (f *nativeChatDB) exec(query string, args ...any) int64 {
	f.t.Helper()
	res, err := f.db.Exec(query, args...)
	if err != nil {
		f.t.Fatalf("chat.db: %v\n%s", err, query)
	}
	id, _ := res.LastInsertId()
	return id
}

func (f *nativeChatDB) queryInt(query string, args ...any) int64 {
	f.t.Helper()
	var n sql.NullInt64
	if err := f.db.QueryRow(query, args...).Scan(&n); err != nil && err != sql.ErrNoRows {
		f.t.Fatalf("chat.db: %v", err)
	}
	return n.Int64
}

func (f *nativeChatDB) addHandle(id string) int64 {
	return f.exec(`INSERT INTO handle (id, uncanonicalized_id) VALUES (?, ?)`, id, id)
}

// addChat adds a chat; a nil name stores NULL, as Messages does for some.
func (f *nativeChatDB) addChat(guid string, name *string, group bool, handles ...int64) int64 {
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

// row describes one message row. Body "" with Attributed false stores no
// body at all (an attachment-only row).
type row struct {
	GUID       string
	Body       string
	Attributed bool // body only in attributedBody, as modern Messages stores it
	Unix       int64
	FromMe     bool
	Handle     int64
	AssocType  int64
	AssocGUID  string
	Emoji      string
	Chats      []int64 // joined in this order; none leaves it an orphan
}

func coreDate(unix int64) int64 { return (unix - coreDataEpoch) * 1_000_000_000 }

func (f *nativeChatDB) add(r row) int64 {
	f.t.Helper()
	var text, blob, assocGUID, emoji any
	if r.Body != "" {
		if r.Attributed {
			blob = makeAttributedBody(r.Body)
		} else {
			text = r.Body
		}
	}
	if r.AssocGUID != "" {
		assocGUID = r.AssocGUID
	}
	if r.Emoji != "" {
		emoji = r.Emoji
	}
	handle := r.Handle
	id := f.exec(`INSERT INTO message (guid, text, attributedBody, handle_id, date, is_from_me,
			associated_message_guid, associated_message_type, associated_message_emoji)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.GUID, text, blob, handle, coreDate(r.Unix), r.FromMe, assocGUID, r.AssocType, emoji)
	for _, c := range r.Chats {
		f.join(c, id)
	}
	return id
}

// message adds an ordinary text row joined to chat.
func (f *nativeChatDB) message(chat int64, guid, body string, unix int64, fromMe bool, handle int64) int64 {
	return f.add(row{GUID: guid, Body: body, Unix: unix, FromMe: fromMe, Handle: handle, Chats: []int64{chat}})
}

// react adds a native reaction row (type 2000–2006 or 3000–3006) aimed at
// assoc ("p:0/<guid>", "bp:<guid>" or "<guid>"), joined to chat.
func (f *nativeChatDB) react(chat int64, guid string, assocType int64, assoc string, unix int64, fromMe bool, handle int64) int64 {
	return f.add(row{GUID: guid, Body: "(reaction text Messages also stores)", Attributed: true, Unix: unix,
		FromMe: fromMe, Handle: handle, AssocType: assocType, AssocGUID: assoc, Chats: []int64{chat}})
}

func (f *nativeChatDB) join(chat, message int64) {
	f.exec(`INSERT INTO chat_message_join (chat_id, message_id, message_date)
		VALUES (?, ?, (SELECT date FROM message WHERE ROWID = ?))`, chat, message, message)
}

// replaceWith moves next's file over this chat.db's path, as restoring a
// backup would: a new inode at the same path. This chat.db then writes the
// replacement.
func (f *nativeChatDB) replaceWith(next *nativeChatDB) {
	f.t.Helper()
	// The importer may hold the old file open, so it keeps its journal
	// mode; a TRUNCATE checkpoint empties its WAL so nothing of it can be
	// replayed into the replacement.
	next.exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	next.exec(`PRAGMA journal_mode=DELETE`)
	f.exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	for _, g := range []*nativeChatDB{next, f} {
		if err := g.db.Close(); err != nil {
			f.t.Fatal(err)
		}
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		os.Remove(f.path + suffix)
		os.Remove(next.path + suffix)
	}
	if err := os.Rename(next.path, f.path); err != nil {
		f.t.Fatal(err)
	}
	f.open()
	f.exec(`PRAGMA journal_mode=WAL`)
}

// copyTo copies this chat.db (VACUUM INTO keeps every ROWID) to a new file
// that can be edited before replacing the original.
func (f *nativeChatDB) copyTo() *nativeChatDB {
	f.t.Helper()
	path := filepath.Join(f.t.TempDir(), "chat-copy.db")
	f.exec(`VACUUM INTO ?`, path)
	g := &nativeChatDB{t: f.t, path: path}
	g.open()
	g.exec(`PRAGMA journal_mode=WAL`)
	return g
}

func newIMessageStore(t *testing.T) (*db.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "messages.db")
	store, err := db.New(path)
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store, path
}

// storeDump is every column of every message, conversation and search row,
// read through a separate connection.
type storeDump struct {
	Messages      []string
	Conversations []string
	Search        []string
}

func dumpStore(t *testing.T, path string) storeDump {
	t.Helper()
	conn, err := sql.Open("sqlite", path)
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
		cols, _ := rows.Columns()
		var out []string
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatalf("dump: %v", err)
			}
			var b strings.Builder
			for i, v := range vals {
				if bs, ok := v.([]byte); ok {
					v = string(bs)
				}
				fmt.Fprintf(&b, "%s=%q ", cols[i], fmt.Sprint(v))
			}
			out = append(out, b.String())
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("dump: %v", err)
		}
		return out
	}
	return storeDump{
		Messages:      read(`SELECT * FROM messages ORDER BY message_id`),
		Conversations: read(`SELECT * FROM conversations ORDER BY conversation_id`),
		Search:        read(`SELECT message_id, body FROM messages_fts ORDER BY message_id, body`),
	}
}

func requireSameStore(t *testing.T, label string, got, want storeDump) {
	t.Helper()
	if reflect.DeepEqual(got, want) {
		return
	}
	diff := func(name string, a, b []string) {
		as, bs := map[string]bool{}, map[string]bool{}
		for _, x := range a {
			as[x] = true
		}
		for _, x := range b {
			bs[x] = true
		}
		for _, x := range a {
			if !bs[x] {
				t.Errorf("%s: %s only in the first: %s", label, name, x)
			}
		}
		for _, x := range b {
			if !as[x] {
				t.Errorf("%s: %s only in the second: %s", label, name, x)
			}
		}
	}
	diff("message", got.Messages, want.Messages)
	diff("conversation", got.Conversations, want.Conversations)
	diff("search row", got.Search, want.Search)
	t.FailNow()
}

func storeDataVersion(t *testing.T, conn *sql.DB) int64 {
	t.Helper()
	var v int64
	if err := conn.QueryRow(`PRAGMA data_version`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func storeObserver(t *testing.T, path string) *sql.DB {
	t.Helper()
	conn, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	conn.SetMaxOpenConns(1)
	t.Cleanup(func() { conn.Close() })
	return conn
}

// runImport runs one import and fails the test on an error.
func runImport(t *testing.T, im *IMessage, store *db.Store) *ImportResult {
	t.Helper()
	res, err := im.ImportFromDB(store)
	if err != nil {
		t.Fatalf("ImportFromDB: %v (result %+v)", err, res)
	}
	if len(res.Errors) > 0 {
		t.Fatalf("ImportFromDB errors: %v", res.Errors)
	}
	return res
}

// fullImportDump imports chat.db once, with a fresh importer, into an empty
// store and dumps it.
func fullImportDump(t *testing.T, chatPath string, now time.Time) storeDump {
	t.Helper()
	store, path := newIMessageStore(t)
	im := &IMessage{DBPath: chatPath, now: func() time.Time { return now }}
	defer im.Close()
	runImport(t, im, store)
	return dumpStore(t, path)
}

// fixedClock is a settable clock for an importer.
type fixedClock struct{ t time.Time }

func (c *fixedClock) now() time.Time { return c.t }

func strPtr(s string) *string { return &s }
