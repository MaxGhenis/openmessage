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
type IMessage struct {
	// DBPath is the path to chat.db. Defaults to ~/Library/Messages/chat.db.
	DBPath string
	// MyName is the display name for outgoing messages (default "Me").
	MyName string
}

// ImportFromDB reads the iMessage database and imports all messages.
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

	result := &ImportResult{}

	// Get all chats (conversations)
	chats, err := im.loadChats(chatDB)
	if err != nil {
		return nil, fmt.Errorf("load chats: %w", err)
	}

	for _, chat := range chats {
		convID := "imessage:" + chat.guid

		participants, _ := json.Marshal(chat.participants)
		if err := store.UpsertConversation(&db.Conversation{
			ConversationID: convID,
			Name:           chat.displayName,
			IsGroup:        chat.isGroup,
			Participants:   string(participants),
			LastMessageTS:  chat.lastMessageTS,
			SourcePlatform: "imessage",
		}); err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("conversation %s: %v", chat.guid, err))
			continue
		}
		result.ConversationsCreated++

		// Import messages for this chat
		msgs, err := im.loadMessages(chatDB, chat.rowID)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("messages for %s: %v", chat.guid, err))
			continue
		}

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

			if err := store.UpsertMessage(msg); err != nil {
				if strings.Contains(err.Error(), "UNIQUE constraint") {
					result.MessagesDuplicate++
				} else {
					result.Errors = append(result.Errors, fmt.Sprintf("message %s: %v", m.guid, err))
				}
				continue
			}
			result.MessagesImported++
		}
	}

	return result, nil
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

func (im *IMessage) loadChats(chatDB *sql.DB) ([]imessageChat, error) {
	rows, err := chatDB.Query(`
		SELECT c.ROWID, c.guid, c.display_name, c.style,
			COALESCE(MAX(m.date), 0) as last_date
		FROM chat c
		LEFT JOIN chat_message_join cmj ON c.ROWID = cmj.chat_id
		LEFT JOIN message m ON cmj.message_id = m.ROWID
		GROUP BY c.ROWID
		ORDER BY last_date DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

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

		// Load participants
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

		chats = append(chats, c)
	}
	return chats, rows.Err()
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

func (im *IMessage) loadMessages(chatDB *sql.DB, chatRowID int) ([]imessageMessage, error) {
	rows, err := chatDB.Query(`
		SELECT m.guid, COALESCE(m.text, ''), m.attributedBody, m.date, m.is_from_me,
			COALESCE(h.id, '') as handle_id,
			COALESCE(h.uncanonicalized_id, h.id, '') as handle_display
		FROM message m
		JOIN chat_message_join cmj ON m.ROWID = cmj.message_id
		LEFT JOIN handle h ON m.handle_id = h.ROWID
		WHERE cmj.chat_id = ?
		ORDER BY m.date ASC
	`, chatRowID)
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
