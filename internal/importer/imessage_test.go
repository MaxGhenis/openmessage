package importer

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/maxghenis/openmessage/internal/db"

	_ "modernc.org/sqlite"
)

// --- Unit tests for helper functions ---

func TestExtractTextFromAttributedBody(t *testing.T) {
	tests := []struct {
		name string
		blob []byte
		want string
	}{
		{
			name: "empty blob",
			blob: nil,
			want: "",
		},
		{
			name: "no NSString marker",
			blob: []byte{0x01, 0x02, 0x03, 0x04},
			want: "",
		},
		{
			name: "valid short text with 1-byte length",
			blob: buildAttributedBody("Hello world"),
			want: "Hello world",
		},
		{
			name: "text with object replacement char",
			blob: buildAttributedBody("Hello \uFFFCworld"),
			want: "Hello world",
		},
		{
			name: "multiple NSString markers (uses last)",
			blob: func() []byte {
				// First marker with different text
				first := buildAttributedBody("First")
				// Second marker with the real text
				second := buildAttributedBody("Second")
				return append(first, second...)
			}(),
			want: "Second",
		},
		{
			name: "truncated blob after marker",
			blob: []byte("NSString"),
			want: "",
		},
		{
			name: "2-byte length encoding",
			blob: buildAttributedBodyLong(300),
			want: func() string {
				b := make([]byte, 300)
				for i := range b {
					b[i] = 'A'
				}
				return string(b)
			}(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractTextFromAttributedBody(tt.blob)
			if got != tt.want {
				t.Errorf("extractTextFromAttributedBody() = %q, want %q", got, tt.want)
			}
		})
	}
}

// buildAttributedBody creates a minimal fake typedstream blob with 1-byte length.
func buildAttributedBody(text string) []byte {
	textBytes := []byte(text)
	if len(textBytes) > 127 {
		return buildAttributedBodyWithLen(textBytes)
	}
	// prefix + "NSString" + padding + 0x2B + length + text
	var blob []byte
	blob = append(blob, []byte("some prefix data ")...)
	blob = append(blob, []byte("NSString")...)
	blob = append(blob, 0x00) // padding byte
	blob = append(blob, 0x2B) // + marker
	blob = append(blob, byte(len(textBytes)))
	blob = append(blob, textBytes...)
	return blob
}

// buildAttributedBodyLong creates a blob with 2-byte (0x81) length encoding.
func buildAttributedBodyLong(n int) []byte {
	textBytes := make([]byte, n)
	for i := range textBytes {
		textBytes[i] = 'A'
	}
	return buildAttributedBodyWithLen(textBytes)
}

func buildAttributedBodyWithLen(textBytes []byte) []byte {
	var blob []byte
	blob = append(blob, []byte("some prefix data ")...)
	blob = append(blob, []byte("NSString")...)
	blob = append(blob, 0x00) // padding
	blob = append(blob, 0x2B) // + marker
	blob = append(blob, 0x81) // 2-byte length marker
	blob = append(blob, byte(len(textBytes)&0xFF), byte(len(textBytes)>>8))
	blob = append(blob, textBytes...)
	return blob
}

func TestExtractParentGUID(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"", ""},
		{"p:0/ABC-DEF-123", "ABC-DEF-123"},
		{"p:1/XYZ-789", "XYZ-789"},
		{"bp:SOME-GUID", "SOME-GUID"},
		{"PLAIN-GUID", "PLAIN-GUID"},
	}

	for _, tt := range tests {
		got := extractParentGUID(tt.input)
		if got != tt.want {
			t.Errorf("extractParentGUID(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestTapbackEmoji(t *testing.T) {
	tests := []struct {
		input int
		want  string
	}{
		{2000, "\u2764\uFE0F"},
		{2001, "\U0001F44D"},
		{2002, "\U0001F44E"},
		{2003, "\U0001F602"},
		{2004, "\u2757"},
		{2005, "\u2753"},
		{1999, ""},
		{2006, ""},
	}

	for _, tt := range tests {
		got := tapbackEmoji(tt.input)
		if got != tt.want {
			t.Errorf("tapbackEmoji(%d) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestMediaPlaceholder(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"", "[attachment]"},
		{"image/jpeg", "[image]"},
		{"image/png", "[image]"},
		{"image/heic", "[image]"},
		{"video/mp4", "[video]"},
		{"video/quicktime", "[video]"},
		{"audio/mpeg", "[audio]"},
		{"audio/amr", "[audio]"},
		{"application/pdf", "[document]"},
		{"text/vcard", "[document]"},
	}

	for _, tt := range tests {
		got := mediaPlaceholder(tt.input)
		if got != tt.want {
			t.Errorf("mediaPlaceholder(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

// --- Integration test with fabricated chat.db ---

func TestIMessageImport(t *testing.T) {
	dir := t.TempDir()

	// Create a fake chat.db with the iMessage schema
	chatDBPath := filepath.Join(dir, "chat.db")
	chatDB, err := sql.Open("sqlite", chatDBPath)
	if err != nil {
		t.Fatalf("create chat.db: %v", err)
	}
	chatDB.SetMaxOpenConns(1)

	// Create iMessage schema
	schema := `
	CREATE TABLE handle (
		ROWID INTEGER PRIMARY KEY,
		id TEXT NOT NULL,
		uncanonicalized_id TEXT
	);

	CREATE TABLE chat (
		ROWID INTEGER PRIMARY KEY,
		guid TEXT UNIQUE NOT NULL,
		display_name TEXT,
		style INTEGER DEFAULT 45
	);

	CREATE TABLE message (
		ROWID INTEGER PRIMARY KEY,
		guid TEXT UNIQUE NOT NULL,
		text TEXT,
		attributedBody BLOB,
		date INTEGER DEFAULT 0,
		is_from_me INTEGER DEFAULT 0,
		handle_id INTEGER DEFAULT 0,
		associated_message_type INTEGER DEFAULT 0,
		associated_message_guid TEXT DEFAULT '',
		cache_has_attachments INTEGER DEFAULT 0,
		reply_to_guid TEXT DEFAULT ''
	);

	CREATE TABLE chat_handle_join (
		chat_id INTEGER,
		handle_id INTEGER
	);

	CREATE TABLE chat_message_join (
		chat_id INTEGER,
		message_id INTEGER
	);

	CREATE TABLE attachment (
		ROWID INTEGER PRIMARY KEY,
		guid TEXT UNIQUE NOT NULL,
		mime_type TEXT
	);

	CREATE TABLE message_attachment_join (
		message_id INTEGER,
		attachment_id INTEGER
	);
	`
	if _, err := chatDB.Exec(schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}

	// Insert test data
	// Handle (contact)
	_, err = chatDB.Exec(`INSERT INTO handle (ROWID, id, uncanonicalized_id) VALUES (1, '+15551234567', 'Alice')`)
	if err != nil {
		t.Fatalf("insert handle: %v", err)
	}

	// Chat
	_, err = chatDB.Exec(`INSERT INTO chat (ROWID, guid, display_name, style) VALUES (1, 'chat-guid-1', 'Alice Chat', 45)`)
	if err != nil {
		t.Fatalf("insert chat: %v", err)
	}

	// Link handle to chat
	_, err = chatDB.Exec(`INSERT INTO chat_handle_join (chat_id, handle_id) VALUES (1, 1)`)
	if err != nil {
		t.Fatalf("insert chat_handle_join: %v", err)
	}

	// Core Data timestamp: 2025-01-15 12:00:00 UTC
	// Unix: 1736942400 - coreDataEpoch(978307200) = 758635200 seconds
	// In nanoseconds: 758635200 * 1_000_000_000 = 758635200000000000
	baseDate := int64(758635200000000000)

	// Message 1: Normal message with text column
	_, err = chatDB.Exec(`INSERT INTO message (ROWID, guid, text, date, is_from_me, handle_id) VALUES (1, 'msg-normal-1', 'Hello from text column', ?, 0, 1)`, baseDate)
	if err != nil {
		t.Fatalf("insert msg 1: %v", err)
	}
	_, err = chatDB.Exec(`INSERT INTO chat_message_join (chat_id, message_id) VALUES (1, 1)`)
	if err != nil {
		t.Fatalf("insert cmj 1: %v", err)
	}

	// Message 2: attributedBody message (NULL text)
	attrBody := buildAttributedBody("Hello from attributedBody")
	_, err = chatDB.Exec(`INSERT INTO message (ROWID, guid, text, attributedBody, date, is_from_me, handle_id) VALUES (2, 'msg-attrbody-2', NULL, ?, ?, 1, 0)`, attrBody, baseDate+1000000000)
	if err != nil {
		t.Fatalf("insert msg 2: %v", err)
	}
	_, err = chatDB.Exec(`INSERT INTO chat_message_join (chat_id, message_id) VALUES (1, 2)`)
	if err != nil {
		t.Fatalf("insert cmj 2: %v", err)
	}

	// Message 3: Tapback reaction (heart on msg-normal-1)
	_, err = chatDB.Exec(`INSERT INTO message (ROWID, guid, text, date, is_from_me, handle_id, associated_message_type, associated_message_guid) VALUES (3, 'msg-tapback-3', 'Loved "Hello from text column"', ?, 0, 1, 2000, 'p:0/msg-normal-1')`, baseDate+2000000000)
	if err != nil {
		t.Fatalf("insert msg 3: %v", err)
	}
	_, err = chatDB.Exec(`INSERT INTO chat_message_join (chat_id, message_id) VALUES (1, 3)`)
	if err != nil {
		t.Fatalf("insert cmj 3: %v", err)
	}

	// Message 4: Attachment-only (no text, has attachment)
	_, err = chatDB.Exec(`INSERT INTO message (ROWID, guid, text, date, is_from_me, handle_id, cache_has_attachments) VALUES (4, 'msg-attach-4', NULL, ?, 0, 1, 1)`, baseDate+3000000000)
	if err != nil {
		t.Fatalf("insert msg 4: %v", err)
	}
	_, err = chatDB.Exec(`INSERT INTO chat_message_join (chat_id, message_id) VALUES (1, 4)`)
	if err != nil {
		t.Fatalf("insert cmj 4: %v", err)
	}

	// Attachment for message 4
	_, err = chatDB.Exec(`INSERT INTO attachment (ROWID, guid, mime_type) VALUES (1, 'att-1', 'image/jpeg')`)
	if err != nil {
		t.Fatalf("insert attachment: %v", err)
	}
	_, err = chatDB.Exec(`INSERT INTO message_attachment_join (message_id, attachment_id) VALUES (4, 1)`)
	if err != nil {
		t.Fatalf("insert maj: %v", err)
	}

	// Message 5: Reply to message 1
	_, err = chatDB.Exec(`INSERT INTO message (ROWID, guid, text, date, is_from_me, handle_id, reply_to_guid) VALUES (5, 'msg-reply-5', 'This is a reply', ?, 1, 0, 'msg-normal-1')`, baseDate+4000000000)
	if err != nil {
		t.Fatalf("insert msg 5: %v", err)
	}
	_, err = chatDB.Exec(`INSERT INTO chat_message_join (chat_id, message_id) VALUES (1, 5)`)
	if err != nil {
		t.Fatalf("insert cmj 5: %v", err)
	}

	// Message 6: Tapback removal (should be skipped)
	_, err = chatDB.Exec(`INSERT INTO message (ROWID, guid, text, date, is_from_me, handle_id, associated_message_type, associated_message_guid) VALUES (6, 'msg-removal-6', '', ?, 0, 1, 3000, 'p:0/msg-normal-1')`, baseDate+5000000000)
	if err != nil {
		t.Fatalf("insert msg 6: %v", err)
	}
	_, err = chatDB.Exec(`INSERT INTO chat_message_join (chat_id, message_id) VALUES (1, 6)`)
	if err != nil {
		t.Fatalf("insert cmj 6: %v", err)
	}

	chatDB.Close()

	// Create openmessage store
	store, err := db.New(filepath.Join(dir, "openmessage.db"))
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	defer store.Close()

	// Run import
	im := &IMessage{
		DBPath: chatDBPath,
		MyName: "Max",
	}
	result, err := im.ImportFromDB(store)
	if err != nil {
		t.Fatalf("import: %v", err)
	}

	// Verify results
	if result.ConversationsCreated != 1 {
		t.Errorf("conversations = %d, want 1", result.ConversationsCreated)
	}

	// Expected: msg1 (normal), msg2 (attributedBody), msg3 (tapback), msg4 (attachment), msg5 (reply) = 5
	// msg6 (tapback removal) should be skipped
	if result.MessagesImported != 5 {
		t.Errorf("messages imported = %d, want 5", result.MessagesImported)
	}
	if len(result.Errors) > 0 {
		t.Errorf("unexpected errors: %v", result.Errors)
	}

	// Verify conversation
	conv, err := store.GetConversation("imessage:chat-guid-1")
	if err != nil {
		t.Fatalf("get conversation: %v", err)
	}
	if conv.Name != "Alice Chat" {
		t.Errorf("conv name = %q, want 'Alice Chat'", conv.Name)
	}
	if conv.SourcePlatform != "imessage" {
		t.Errorf("conv source = %q, want 'imessage'", conv.SourcePlatform)
	}

	// Verify individual messages
	// Message 1: normal text
	msg1, err := store.GetMessageByID("imessage:msg-normal-1")
	if err != nil {
		t.Fatalf("get msg 1: %v", err)
	}
	if msg1.Body != "Hello from text column" {
		t.Errorf("msg1 body = %q, want 'Hello from text column'", msg1.Body)
	}
	if msg1.SenderName != "Alice" {
		t.Errorf("msg1 sender = %q, want 'Alice'", msg1.SenderName)
	}
	if msg1.IsFromMe {
		t.Error("msg1 should not be from me")
	}

	// Message 2: attributedBody text
	msg2, err := store.GetMessageByID("imessage:msg-attrbody-2")
	if err != nil {
		t.Fatalf("get msg 2: %v", err)
	}
	if msg2.Body != "Hello from attributedBody" {
		t.Errorf("msg2 body = %q, want 'Hello from attributedBody'", msg2.Body)
	}
	if !msg2.IsFromMe {
		t.Error("msg2 should be from me")
	}
	if msg2.SenderName != "Max" {
		t.Errorf("msg2 sender = %q, want 'Max'", msg2.SenderName)
	}

	// Message 3: tapback
	msg3, err := store.GetMessageByID("imessage:msg-tapback-3")
	if err != nil {
		t.Fatalf("get msg 3: %v", err)
	}
	if msg3.Body != "\u2764\uFE0F" {
		t.Errorf("msg3 body = %q, want heart emoji", msg3.Body)
	}
	if msg3.ReplyToID != "imessage:msg-normal-1" {
		t.Errorf("msg3 reply_to = %q, want 'imessage:msg-normal-1'", msg3.ReplyToID)
	}

	// Message 4: attachment-only
	msg4, err := store.GetMessageByID("imessage:msg-attach-4")
	if err != nil {
		t.Fatalf("get msg 4: %v", err)
	}
	if msg4.Body != "[image]" {
		t.Errorf("msg4 body = %q, want '[image]'", msg4.Body)
	}

	// Message 5: reply
	msg5, err := store.GetMessageByID("imessage:msg-reply-5")
	if err != nil {
		t.Fatalf("get msg 5: %v", err)
	}
	if msg5.Body != "This is a reply" {
		t.Errorf("msg5 body = %q, want 'This is a reply'", msg5.Body)
	}
	if msg5.ReplyToID != "imessage:msg-normal-1" {
		t.Errorf("msg5 reply_to = %q, want 'imessage:msg-normal-1'", msg5.ReplyToID)
	}

	// Message 6 (tapback removal) should NOT exist
	msg6, err := store.GetMessageByID("imessage:msg-removal-6")
	if err == nil && msg6 != nil {
		t.Error("msg6 (tapback removal) should not have been imported")
	}
}

func TestIMessageImportDedup(t *testing.T) {
	dir := t.TempDir()
	chatDBPath := filepath.Join(dir, "chat.db")

	// Create minimal chat.db
	chatDB, err := sql.Open("sqlite", chatDBPath)
	if err != nil {
		t.Fatalf("create chat.db: %v", err)
	}
	chatDB.SetMaxOpenConns(1)
	chatDB.Exec(`
		CREATE TABLE handle (ROWID INTEGER PRIMARY KEY, id TEXT, uncanonicalized_id TEXT);
		CREATE TABLE chat (ROWID INTEGER PRIMARY KEY, guid TEXT UNIQUE, display_name TEXT, style INTEGER DEFAULT 45);
		CREATE TABLE message (ROWID INTEGER PRIMARY KEY, guid TEXT UNIQUE, text TEXT, attributedBody BLOB,
			date INTEGER DEFAULT 0, is_from_me INTEGER DEFAULT 0, handle_id INTEGER DEFAULT 0,
			associated_message_type INTEGER DEFAULT 0, associated_message_guid TEXT DEFAULT '',
			cache_has_attachments INTEGER DEFAULT 0, reply_to_guid TEXT DEFAULT '');
		CREATE TABLE chat_handle_join (chat_id INTEGER, handle_id INTEGER);
		CREATE TABLE chat_message_join (chat_id INTEGER, message_id INTEGER);
		CREATE TABLE attachment (ROWID INTEGER PRIMARY KEY, guid TEXT UNIQUE, mime_type TEXT);
		CREATE TABLE message_attachment_join (message_id INTEGER, attachment_id INTEGER);

		INSERT INTO handle VALUES (1, '+15551234567', 'Alice');
		INSERT INTO chat VALUES (1, 'chat-1', 'Alice', 45);
		INSERT INTO chat_handle_join VALUES (1, 1);
		INSERT INTO message VALUES (1, 'msg-1', 'Hello', NULL, 758635200000000000, 0, 1, 0, '', 0, '');
		INSERT INTO chat_message_join VALUES (1, 1);
	`)
	chatDB.Close()

	store, err := db.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	defer store.Close()

	im := &IMessage{DBPath: chatDBPath}

	// First import
	r1, err := im.ImportFromDB(store)
	if err != nil {
		t.Fatalf("first import: %v", err)
	}
	if r1.MessagesImported != 1 {
		t.Errorf("first: imported = %d, want 1", r1.MessagesImported)
	}

	// Second import (same data — upsert)
	r2, err := im.ImportFromDB(store)
	if err != nil {
		t.Fatalf("second import: %v", err)
	}
	// UpsertMessage uses ON CONFLICT DO UPDATE, so it succeeds
	if r2.MessagesImported != 1 {
		t.Errorf("second: imported = %d, want 1", r2.MessagesImported)
	}

	count, _ := store.MessageCount("imessage")
	if count != 1 {
		t.Errorf("total messages = %d, want 1", count)
	}
}
