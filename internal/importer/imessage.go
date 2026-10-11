package importer

import (
	"bytes"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/maxghenis/openmessage/internal/db"

	_ "modernc.org/sqlite"
)

// macOS Core Data epoch: 2001-01-01 00:00:00 UTC in Unix seconds.
const coreDataEpoch = 978307200

// IMessage imports messages from the macOS Messages chat.db.
// Requires Full Disk Access to read ~/Library/Messages/chat.db.
//
// An import writes only what the store lacks or holds differently: a message
// already stored with the same conversation, sender, body, time and
// direction is counted in MessagesDuplicate and not rewritten, and a chat is
// upserted only when it is new to the store, differs from it, or one of its
// messages was written. MessagesImported counts the messages written, new or
// changed.
// Tapbacks ("Loved “…”") become reactions on the message they quote, as
// Store.RepairTapbacks does at startup, instead of standalone text rows.
type IMessage struct {
	// DBPath is the path to chat.db. Defaults to ~/Library/Messages/chat.db.
	DBPath string
	// MyName is the display name for outgoing messages (default "Me").
	MyName string
	// AfterRowID limits an import to chat.db messages whose ROWID is greater.
	// Messages assigns ROWIDs in insertion order, so this also catches older
	// messages that iCloud sync inserts late, which a date window would miss.
	// An import that finishes without errors advances it to the highest ROWID
	// it saw, so one IMessage reused across polls reads only what arrived
	// since the last poll. Zero reads everything; a chat.db whose highest
	// ROWID is below it (replaced, restored, or its newest rows deleted) is
	// read in full again. An edit to an already-read row is picked up by the
	// next full read, which a daemon does once per start.
	AfterRowID int64

	// The chat table's own high-water mark, valid while AfterRowID still
	// holds the value this importer set (markedRowID): Messages can add a
	// chat before it has any message, and a full read creates those too.
	afterChatRowID int64
	markedRowID    int64
}

// ImportFromDB imports the chat.db messages after AfterRowID; see IMessage.
func (im *IMessage) ImportFromDB(store *db.Store) (*ImportResult, error) {
	dbPath := im.DBPath
	if dbPath == "" {
		home, _ := os.UserHomeDir()
		dbPath = filepath.Join(home, "Library", "Messages", "chat.db")
	}

	chatDB, err := sql.Open("sqlite", dbPath+"?mode=ro")
	if err != nil {
		return nil, fmt.Errorf("open iMessage db: %w", err)
	}
	defer chatDB.Close()
	chatDB.SetMaxOpenConns(1)

	// Bound this import at the newest rows now present, so a message Messages
	// writes while it runs is read by the next import, not half by this one.
	var highWater, chatHighWater int64
	if err := chatDB.QueryRow(`SELECT COALESCE((SELECT MAX(ROWID) FROM message), 0), COALESCE((SELECT MAX(ROWID) FROM chat), 0)`).Scan(&highWater, &chatHighWater); err != nil {
		return nil, fmt.Errorf("read iMessage high-water mark: %w", err)
	}
	after := im.AfterRowID
	if after < 0 || after > highWater {
		after = 0
	}
	// -1: chats are selected by their messages alone.
	afterChat := int64(-1)
	if after > 0 && after == im.markedRowID {
		afterChat = im.afterChatRowID
	}

	result := &ImportResult{}
	if after > 0 && after == highWater && afterChat >= chatHighWater {
		return result, nil
	}

	chats, err := im.loadChats(chatDB, after, highWater, afterChat, chatHighWater)
	if err != nil {
		return nil, fmt.Errorf("load chats: %w", err)
	}

	for _, chat := range chats {
		convID := "imessage:" + chat.guid

		msgs, err := im.loadMessages(chatDB, chat.rowID, after, highWater)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("messages for %s: %v", chat.guid, err))
			continue
		}

		var writes []*db.Message
		var tapbacks []*db.Message
		for _, m := range msgs {
			msg := &db.Message{
				MessageID:      "imessage:" + m.guid,
				ConversationID: convID,
				SenderName:     m.senderName,
				SenderNumber:   m.senderID,
				Body:           m.text,
				TimestampMS:    m.timestampMS,
				Status:         "delivered",
				IsFromMe:       m.isFromMe,
				SourcePlatform: "imessage",
				SourceID:       m.guid,
			}
			existing, err := store.GetMessageByID(msg.MessageID)
			if err != nil {
				result.Errors = append(result.Errors, fmt.Sprintf("message %s: %v", m.guid, err))
				continue
			}
			if existing != nil && sameIMessageRow(existing, msg) {
				result.MessagesDuplicate++
				continue
			}
			if existing == nil {
				if _, ok := db.ParseTapback(msg.Body); ok {
					tapbacks = append(tapbacks, msg)
					continue
				}
			}
			writes = append(writes, msg)
		}

		participants, _ := json.Marshal(chat.participants)
		conv := &db.Conversation{
			ConversationID: convID,
			Name:           chat.displayName,
			IsGroup:        chat.isGroup,
			Participants:   string(participants),
			LastMessageTS:  chat.lastMessageTS,
			SourcePlatform: "imessage",
		}
		existingConv, _ := store.GetConversation(convID)
		if existingConv == nil || !sameIMessageConversation(existingConv, conv) || len(writes) > 0 || len(tapbacks) > 0 {
			if err := store.UpsertConversation(conv); err != nil {
				result.Errors = append(result.Errors, fmt.Sprintf("conversation %s: %v", chat.guid, err))
				continue
			}
			if existingConv == nil {
				result.ConversationsCreated++
			}
		}

		// Messages are loaded oldest first; a tapback is applied after every
		// message of its batch is stored, so it finds a target that arrived
		// in the same import, and in order, so a later removal wins.
		for _, msg := range writes {
			if err := store.UpsertMessage(msg); err != nil {
				if strings.Contains(err.Error(), "UNIQUE constraint") {
					result.MessagesDuplicate++
				} else {
					result.Errors = append(result.Errors, fmt.Sprintf("message %s: %v", msg.SourceID, err))
				}
				continue
			}
			result.MessagesImported++
		}
		for _, msg := range tapbacks {
			applied, changed, err := store.ApplyTapbackChange(msg)
			if err != nil {
				result.Errors = append(result.Errors, fmt.Sprintf("tapback %s: %v", msg.SourceID, err))
				continue
			}
			if applied {
				if changed {
					result.MessagesImported++
				} else {
					result.MessagesDuplicate++
				}
				continue
			}
			// No target yet: keep it as text, as every other path does;
			// RepairTapbacks converts it once its target is stored.
			if err := store.UpsertMessage(msg); err != nil {
				result.Errors = append(result.Errors, fmt.Sprintf("message %s: %v", msg.SourceID, err))
				continue
			}
			result.MessagesImported++
		}
	}

	if len(result.Errors) == 0 {
		im.AfterRowID = highWater
		im.markedRowID = highWater
		im.afterChatRowID = chatHighWater
	}
	return result, nil
}

// sameIMessageConversation reports whether a stored chat already holds every
// field an import writes. A chat's last_message_ts counts every message in
// it, including ones without text that import nothing.
func sameIMessageConversation(existing, conv *db.Conversation) bool {
	return existing.Name == conv.Name &&
		existing.IsGroup == conv.IsGroup &&
		existing.Participants == conv.Participants &&
		existing.LastMessageTS == conv.LastMessageTS &&
		existing.SourcePlatform == conv.SourcePlatform
}

// sameIMessageRow reports whether a stored row already holds everything an
// import would write for msg, so writing it again would change nothing.
func sameIMessageRow(existing, msg *db.Message) bool {
	return existing.ConversationID == msg.ConversationID &&
		existing.Body == msg.Body &&
		existing.TimestampMS == msg.TimestampMS &&
		existing.IsFromMe == msg.IsFromMe &&
		existing.SenderName == msg.SenderName &&
		existing.SenderNumber == msg.SenderNumber &&
		existing.SourcePlatform == msg.SourcePlatform &&
		existing.SourceID == msg.SourceID
}

type imessageChat struct {
	rowID         int
	guid          string
	displayName   string
	isGroup       bool
	participants  []map[string]string
	lastMessageTS int64
}

type imessageMessage struct {
	guid        string
	text        string
	senderName  string
	senderID    string
	timestampMS int64
	isFromMe    bool
}

// loadChats returns every chat when after is 0, else the chats with a message
// whose ROWID is in (after, upTo] and, when afterChat is not -1, the chats
// whose own ROWID is in (afterChat, chatUpTo].
func (im *IMessage) loadChats(chatDB *sql.DB, after, upTo, afterChat, chatUpTo int64) ([]imessageChat, error) {
	rows, err := chatDB.Query(`
		SELECT c.ROWID, c.guid, COALESCE(c.display_name, ''), c.style,
			COALESCE(MAX(m.date), 0) as last_date
		FROM chat c
		LEFT JOIN chat_message_join cmj ON c.ROWID = cmj.chat_id
		LEFT JOIN message m ON cmj.message_id = m.ROWID
		WHERE ? = 0
			OR c.ROWID IN (
				SELECT chat_id FROM chat_message_join WHERE message_id > ? AND message_id <= ?
			)
			OR (? >= 0 AND c.ROWID > ? AND c.ROWID <= ?)
		GROUP BY c.ROWID
		ORDER BY last_date DESC
	`, after, after, upTo, afterChat, afterChat, chatUpTo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// Collect every row before loading participants: ImportFromDB holds one
	// connection (MaxOpenConns=1), so a query issued while these rows are open
	// waits forever. It did, from 2026-02-22 until this fix, on any chat.db
	// with a chat; the sandbox flag kept the daemon from reaching it.
	var chats []imessageChat
	for rows.Next() {
		var c imessageChat
		var style int
		var lastDate int64
		if err := rows.Scan(&c.rowID, &c.guid, &c.displayName, &style, &lastDate); err != nil {
			continue
		}
		c.isGroup = style == 43 // iMessage group chat style
		c.lastMessageTS = coreDataToMS(lastDate)
		chats = append(chats, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()

	for i := range chats {
		c := &chats[i]
		c.participants = im.loadChatParticipants(chatDB, c.rowID)

		// Derive name if empty
		if c.displayName == "" {
			var names []string
			for _, p := range c.participants {
				if n := p["name"]; n != "" {
					names = append(names, n)
				} else if n := p["number"]; n != "" {
					names = append(names, n)
				}
			}
			c.displayName = strings.Join(names, ", ")
		}
	}
	return chats, nil
}

func (im *IMessage) loadChatParticipants(chatDB *sql.DB, chatRowID int) []map[string]string {
	rows, err := chatDB.Query(`
		SELECT h.id, COALESCE(h.uncanonicalized_id, h.id)
		FROM handle h
		JOIN chat_handle_join chj ON h.ROWID = chj.handle_id
		WHERE chj.chat_id = ?
	`, chatRowID)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var participants []map[string]string
	for rows.Next() {
		var id, displayID string
		if err := rows.Scan(&id, &displayID); err != nil {
			continue
		}
		participants = append(participants, map[string]string{
			"name":   displayID,
			"number": id,
		})
	}
	return participants
}

// loadMessages returns a chat's messages whose ROWID is in (after, upTo],
// oldest first.
func (im *IMessage) loadMessages(chatDB *sql.DB, chatRowID int, after, upTo int64) ([]imessageMessage, error) {
	rows, err := chatDB.Query(`
		SELECT m.guid, COALESCE(m.text, ''), m.attributedBody, m.date, m.is_from_me,
			COALESCE(h.id, '') as handle_id,
			COALESCE(h.uncanonicalized_id, h.id, '') as handle_display
		FROM message m
		JOIN chat_message_join cmj ON m.ROWID = cmj.message_id
		LEFT JOIN handle h ON m.handle_id = h.ROWID
		WHERE cmj.chat_id = ? AND m.ROWID > ? AND m.ROWID <= ?
		ORDER BY m.date ASC, m.ROWID ASC
	`, chatRowID, after, upTo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	myName := im.MyName
	if myName == "" {
		myName = "Me"
	}

	var msgs []imessageMessage
	for rows.Next() {
		var m imessageMessage
		var date int64
		var handleID, handleDisplay string
		var attributedBody []byte
		if err := rows.Scan(&m.guid, &m.text, &attributedBody, &date, &m.isFromMe, &handleID, &handleDisplay); err != nil {
			continue
		}
		// Modern Messages stores the body in attributedBody (an NSAttributedString
		// archive) and leaves text NULL — without this, the bulk of recent
		// messages would import as empty and be skipped.
		if m.text == "" && len(attributedBody) > 0 {
			m.text = decodeAttributedBody(attributedBody)
		}
		if m.text == "" {
			continue
		}
		m.timestampMS = coreDataToMS(date)
		if m.isFromMe {
			m.senderName = myName
		} else {
			m.senderName = handleDisplay
			m.senderID = handleID
		}
		msgs = append(msgs, m)
	}
	return msgs, rows.Err()
}

// decodeAttributedBody best-effort extracts the plain message text from an
// iMessage `attributedBody` blob (an NSArchiver "streamtyped" archive of an
// NSAttributedString). The text is stored as an NSString whose UTF-8 bytes
// follow the "NSString" class marker, a few control bytes, a '+' (0x2B), and a
// length. The length uses the streamtyped variable-length integer encoding.
// Returns "" if the structure isn't recognized or the result isn't valid UTF-8
// (so a parse failure degrades to skipping, never to garbage).
func decodeAttributedBody(blob []byte) string {
	idx := bytes.Index(blob, []byte("NSString"))
	if idx < 0 {
		return ""
	}
	rest := blob[idx+len("NSString"):]
	plus := bytes.IndexByte(rest, '+')
	if plus < 0 || plus+1 >= len(rest) {
		return ""
	}
	rest = rest[plus+1:]

	var length int
	switch {
	case rest[0] < 0x80:
		length = int(rest[0])
		rest = rest[1:]
	case rest[0] == 0x81: // next 2 bytes, little-endian
		if len(rest) < 3 {
			return ""
		}
		length = int(binary.LittleEndian.Uint16(rest[1:3]))
		rest = rest[3:]
	case rest[0] == 0x82: // next 4 bytes, little-endian
		if len(rest) < 5 {
			return ""
		}
		length = int(binary.LittleEndian.Uint32(rest[1:5]))
		rest = rest[5:]
	default:
		return ""
	}
	if length <= 0 || length > len(rest) {
		return ""
	}
	text := string(rest[:length])
	if !utf8.ValidString(text) {
		return ""
	}
	return text
}

// coreDataToMS converts a macOS Core Data timestamp (nanoseconds since 2001-01-01) to milliseconds since Unix epoch.
func coreDataToMS(date int64) int64 {
	if date == 0 {
		return 0
	}
	secs := date / 1_000_000_000
	return (secs + coreDataEpoch) * 1000
}
