package app

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/maxghenis/openmessage/internal/db"
)

// seedRepairableStore writes one trigger row for each startup repair sweep
// into a fresh legacy store, then closes it. Each row is the canonical shape
// the corresponding sweep exists to fix (mirroring internal/db's repair
// tests):
//   - RepairLegacyArtifacts: a WhatsApp "[Reaction]" placeholder (deleted, and
//     its conversation's recency recomputed).
//   - RepairTapbacks: an iMessage `Loved "..."` text whose target message
//     exists (converted into a reaction, row deleted).
//   - RepairEmptyStubMessages: an empty body/media/reactions row with a
//     terminal status (deleted).
//   - RepairContentlessRecency: a contentless newest message currently setting
//     its conversation's last_message_ts (recency lowered). Status stays empty
//     so the empty-stub sweep leaves the row itself alone.
func seedRepairableStore(t *testing.T, dbPath string) {
	t.Helper()
	store, err := db.New(dbPath)
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	defer store.Close()

	conv := func(c *db.Conversation) {
		t.Helper()
		if err := store.UpsertConversation(c); err != nil {
			t.Fatalf("UpsertConversation(%s): %v", c.ConversationID, err)
		}
	}
	msg := func(m *db.Message) {
		t.Helper()
		if err := store.UpsertMessage(m); err != nil {
			t.Fatalf("UpsertMessage(%s): %v", m.MessageID, err)
		}
	}

	conv(&db.Conversation{ConversationID: "whatsapp:group@g.us", Name: "Group", IsGroup: true, LastMessageTS: 3000, SourcePlatform: "whatsapp"})
	msg(&db.Message{MessageID: "whatsapp:real", ConversationID: "whatsapp:group@g.us", Body: "real message", TimestampMS: 1000, SourcePlatform: "whatsapp", SourceID: "real"})
	msg(&db.Message{MessageID: "whatsapp:reaction-placeholder", ConversationID: "whatsapp:group@g.us", Body: "[Reaction]", TimestampMS: 3000, SourcePlatform: "whatsapp", SourceID: "reaction-placeholder"})

	conv(&db.Conversation{ConversationID: "imessage:chat", Name: "Chat", LastMessageTS: 2000, SourcePlatform: "imessage"})
	msg(&db.Message{MessageID: "imessage:target", ConversationID: "imessage:chat", Body: "great idea", TimestampMS: 1000, IsFromMe: true, SourcePlatform: "imessage"})
	msg(&db.Message{MessageID: "imessage:tapback", ConversationID: "imessage:chat", SenderNumber: "+15550100", Body: `Loved "great idea"`, TimestampMS: 2000, SourcePlatform: "imessage"})

	conv(&db.Conversation{ConversationID: "sms:stub", Name: "Stub", LastMessageTS: 4000, SourcePlatform: "sms"})
	msg(&db.Message{MessageID: "sms:stub-real", ConversationID: "sms:stub", Body: "hello", TimestampMS: 1000, SourcePlatform: "sms"})
	msg(&db.Message{MessageID: "sms:stub-empty", ConversationID: "sms:stub", Body: "", Status: "READ", TimestampMS: 4000, SourcePlatform: "sms"})

	conv(&db.Conversation{ConversationID: "sms:phantom", Name: "Phantom", LastMessageTS: 5000, SourcePlatform: "sms"})
	msg(&db.Message{MessageID: "sms:phantom-real", ConversationID: "sms:phantom", Body: "hey", TimestampMS: 1000, SourcePlatform: "sms"})
	msg(&db.Message{MessageID: "sms:phantom-empty", ConversationID: "sms:phantom", Body: "", TimestampMS: 5000, SourcePlatform: "sms"})
}

// snapshotStore captures every messages and conversations row (all columns)
// plus a row count for every other real table, through a read-only connection
// that provably writes nothing. FTS shadow tables are excluded: db.New's
// count-guarded FTS rebuild belongs to "the SQLite open itself", and every
// repair sweep that rewrites FTS also rewrites messages rows, which the
// full-row dump catches.
func snapshotStore(t *testing.T, dbPath string) string {
	t.Helper()
	conn, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro")
	if err != nil {
		t.Fatalf("open read-only snapshot connection: %v", err)
	}
	defer conn.Close()
	conn.SetMaxOpenConns(1)

	var b strings.Builder
	dumpTable(t, &b, conn, "messages", "message_id")
	dumpTable(t, &b, conn, "conversations", "conversation_id")

	rows, err := conn.Query(`
		SELECT name FROM sqlite_master
		WHERE type = 'table'
			AND name NOT LIKE 'sqlite_%'
			AND name NOT LIKE '%\_fts%' ESCAPE '\'
			AND name NOT IN ('messages', 'conversations')
		ORDER BY name`)
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			t.Fatalf("scan table name: %v", err)
		}
		tables = append(tables, name)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate tables: %v", err)
	}
	for _, table := range tables {
		var count int
		if err := conn.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		fmt.Fprintf(&b, "count %s=%d\n", table, count)
	}
	return b.String()
}

func dumpTable(t *testing.T, b *strings.Builder, conn *sql.DB, table, orderColumn string) {
	t.Helper()
	rows, err := conn.Query("SELECT * FROM " + table + " ORDER BY " + orderColumn)
	if err != nil {
		t.Fatalf("dump %s: %v", table, err)
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		t.Fatalf("columns of %s: %v", table, err)
	}
	values := make([]any, len(columns))
	pointers := make([]any, len(columns))
	for i := range values {
		pointers[i] = &values[i]
	}
	for rows.Next() {
		if err := rows.Scan(pointers...); err != nil {
			t.Fatalf("scan %s row: %v", table, err)
		}
		fmt.Fprintf(b, "%s|", table)
		for i, v := range values {
			if raw, ok := v.([]byte); ok {
				v = string(raw)
			}
			fmt.Fprintf(b, "%s=%v|", columns[i], v)
		}
		b.WriteString("\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate %s: %v", table, err)
	}
}

// TestNewClientPerformsNoStoreWrites is the client-mode no-writes contract:
// NewClient must leave every row in messages.db untouched — the startup
// repair sweeps (RepairLegacyArtifacts, RepairContentlessRecency,
// RepairTapbacks, RepairEmptyStubMessages, and the WhatsApp media-placeholder
// repair) run only in New. MCP hosts spawn one client process per session, so
// with dozens of sessions open, per-client sweeps would hit the daemon's live
// store with that many concurrent write bursts at every session start.
func TestNewClientPerformsNoStoreWrites(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("OPENMESSAGES_DATA_DIR", dataDir)
	t.Setenv("OPENMESSAGES_DEMO", "0")
	// Keep the control leg below off the machine's real WhatsApp desktop
	// store; the four store-level sweeps run regardless.
	t.Setenv("OPENMESSAGES_APP_SANDBOX", "1")

	dbPath := filepath.Join(dataDir, "messages.db")
	seedRepairableStore(t, dbPath)
	before := snapshotStore(t, dbPath)

	a, err := NewClient(zerolog.Nop())
	if err != nil {
		t.Fatalf("NewClient(): %v", err)
	}
	a.Close()

	if after := snapshotStore(t, dbPath); after != before {
		t.Fatalf("NewClient wrote to the store beyond the SQLite open itself:\n--- before ---\n%s\n--- after ---\n%s", before, after)
	}

	// Control: New on the same store must fire every sweep. This proves the
	// seeded rows genuinely trigger repairs, i.e. the no-writes assertion
	// above cannot pass vacuously.
	a2, err := New(zerolog.Nop())
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	defer a2.Close()

	if m, err := a2.Store.GetMessageByID("whatsapp:reaction-placeholder"); err != nil || m != nil {
		t.Errorf("RepairLegacyArtifacts did not delete the WhatsApp reaction placeholder (m=%v, err=%v)", m, err)
	}
	if m, err := a2.Store.GetMessageByID("imessage:tapback"); err != nil || m != nil {
		t.Errorf("RepairTapbacks did not delete the tapback row (m=%v, err=%v)", m, err)
	}
	if target, err := a2.Store.GetMessageByID("imessage:target"); err != nil || target == nil || !strings.Contains(target.Reactions, "❤️") {
		t.Errorf("RepairTapbacks did not attach the reaction to the target (target=%+v, err=%v)", target, err)
	}
	if m, err := a2.Store.GetMessageByID("sms:stub-empty"); err != nil || m != nil {
		t.Errorf("RepairEmptyStubMessages did not delete the empty stub (m=%v, err=%v)", m, err)
	}
	if c, err := a2.Store.GetConversation("sms:phantom"); err != nil || c == nil || c.LastMessageTS != 1000 {
		t.Errorf("RepairContentlessRecency did not lower the phantom conversation's recency (c=%+v, err=%v)", c, err)
	}
}
