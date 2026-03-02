package importer

import (
	"bytes"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

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
	var emptyBlobs int // blobs parsed OK but text was only UFFC/whitespace

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

		// Load attachment MIME types (rows are closed, safe for nested query)
		mimeTypes := im.loadAttachmentMimeTypes(chatDB, chat.rowID)

		for _, m := range msgs {
			// Resolve message text: prefer text column, fall back to attributedBody
			text := m.text
			if text == "" && len(m.attributedBody) > 0 {
				extracted := extractTextFromAttributedBody(m.attributedBody)
				if extracted != "" {
					text = extracted
				} else {
					emptyBlobs++ // text was only UFFC or whitespace
				}
			}

			// Skip tapback removals (associated_message_type 3000+)
			if m.associatedMessageType >= 3000 {
				continue
			}

			// Handle tapback reactions (associated_message_type 2000-2005)
			if m.associatedMessageType >= 2000 && m.associatedMessageType <= 2005 {
				emoji := tapbackEmoji(m.associatedMessageType)
				parentGUID := extractParentGUID(m.associatedMessageGUID)
				msg := &db.Message{
					MessageID:      "imessage:" + m.guid,
					ConversationID: convID,
					SenderName:     m.senderName,
					SenderNumber:   m.senderID,
					Body:           emoji,
					TimestampMS:    m.timestampMS,
					Status:         "delivered",
					IsFromMe:       m.isFromMe,
					SourcePlatform: "imessage",
					SourceID:       m.guid,
				}
				if parentGUID != "" {
					msg.ReplyToID = "imessage:" + parentGUID
				}

				if err := store.UpsertMessage(msg); err != nil {
					if strings.Contains(err.Error(), "UNIQUE constraint") {
						result.MessagesDuplicate++
					} else {
						result.Errors = append(result.Errors, fmt.Sprintf("message %s: %v", m.guid, err))
					}
				} else {
					result.MessagesImported++
				}
				continue
			}

			// For attachment-only messages with no text
			if text == "" && m.cacheHasAttachments {
				if mt, ok := mimeTypes[m.guid]; ok {
					text = mediaPlaceholder(mt)
				} else {
					text = "[attachment]"
				}
			}

			// Skip messages with no text at all
			if text == "" {
				continue
			}

			msg := &db.Message{
				MessageID:      "imessage:" + m.guid,
				ConversationID: convID,
				SenderName:     m.senderName,
				SenderNumber:   m.senderID,
				Body:           text,
				TimestampMS:    m.timestampMS,
				Status:         "delivered",
				IsFromMe:       m.isFromMe,
				SourcePlatform: "imessage",
				SourceID:       m.guid,
			}

			// Populate reply threading
			if m.replyToGUID != "" {
				msg.ReplyToID = "imessage:" + m.replyToGUID
			}

			// Set MIME type if attachment
			if mt, ok := mimeTypes[m.guid]; ok {
				msg.MimeType = mt
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

	if emptyBlobs > 0 {
		log.Printf("imessage: %d messages had attributedBody with no text content (attachment-only or whitespace)", emptyBlobs)
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
	guid                  string
	text                  string
	attributedBody        []byte
	senderName            string
	senderID              string
	timestampMS           int64
	isFromMe              bool
	associatedMessageType int
	associatedMessageGUID string
	cacheHasAttachments   bool
	replyToGUID           string
}

func (im *IMessage) loadChats(chatDB *sql.DB) ([]imessageChat, error) {
	rows, err := chatDB.Query(`
		SELECT c.ROWID, c.guid, COALESCE(c.display_name, ''), c.style,
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

	// Collect all chats first — rows must be closed before running
	// nested queries (loadChatParticipants) on a single-conn SQLite DB.
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

	// Now load participants for each chat (safe — rows are closed).
	for i := range chats {
		chats[i].participants = im.loadChatParticipants(chatDB, chats[i].rowID)
		if chats[i].displayName == "" {
			var names []string
			for _, p := range chats[i].participants {
				if n := p["name"]; n != "" {
					names = append(names, n)
				} else if n := p["number"]; n != "" {
					names = append(names, n)
				}
			}
			chats[i].displayName = strings.Join(names, ", ")
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

func (im *IMessage) loadMessages(chatDB *sql.DB, chatRowID int) ([]imessageMessage, error) {
	rows, err := chatDB.Query(`
		SELECT m.guid, COALESCE(m.text, ''), m.attributedBody, m.date, m.is_from_me,
			COALESCE(h.id, '') as handle_id,
			COALESCE(h.uncanonicalized_id, h.id, '') as handle_display,
			COALESCE(m.associated_message_type, 0),
			COALESCE(m.associated_message_guid, ''),
			COALESCE(m.cache_has_attachments, 0),
			COALESCE(m.reply_to_guid, '')
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
		var cacheHasAttachments int
		if err := rows.Scan(&m.guid, &m.text, &attributedBody, &date, &m.isFromMe,
			&handleID, &handleDisplay,
			&m.associatedMessageType, &m.associatedMessageGUID,
			&cacheHasAttachments, &m.replyToGUID); err != nil {
			continue
		}
		m.attributedBody = attributedBody
		m.cacheHasAttachments = cacheHasAttachments != 0
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

// loadAttachmentMimeTypes returns a map from message GUID to MIME type
// for messages that have attachments.
func (im *IMessage) loadAttachmentMimeTypes(chatDB *sql.DB, chatRowID int) map[string]string {
	rows, err := chatDB.Query(`
		SELECT m.guid, COALESCE(a.mime_type, '')
		FROM message m
		JOIN chat_message_join cmj ON m.ROWID = cmj.message_id
		JOIN message_attachment_join maj ON m.ROWID = maj.message_id
		JOIN attachment a ON maj.attachment_id = a.ROWID
		WHERE cmj.chat_id = ?
	`, chatRowID)
	if err != nil {
		return nil
	}
	defer rows.Close()

	result := make(map[string]string)
	for rows.Next() {
		var guid, mimeType string
		if err := rows.Scan(&guid, &mimeType); err != nil {
			continue
		}
		// Keep the first MIME type found per message
		if _, exists := result[guid]; !exists {
			result[guid] = mimeType
		}
	}
	return result
}

// coreDataToMS converts a macOS Core Data timestamp (nanoseconds since 2001-01-01) to milliseconds since Unix epoch.
func coreDataToMS(date int64) int64 {
	if date == 0 {
		return 0
	}
	secs := date / 1_000_000_000
	return (secs + coreDataEpoch) * 1000
}

// --- Pure helper functions ---

// extractTextFromAttributedBody extracts plain text from a macOS typedstream
// attributedBody BLOB. Returns "" on any parse failure.
func extractTextFromAttributedBody(blob []byte) string {
	if len(blob) == 0 {
		return ""
	}

	defer func() {
		// Safety net: never panic on malformed data
		recover()
	}()

	// Find the last NSString marker in the binary
	marker := []byte("NSString")
	idx := bytes.LastIndex(blob, marker)
	if idx < 0 {
		return ""
	}

	// Move past the marker
	pos := idx + len(marker)

	// Scan forward for 0x2B (+) byte within 8 bytes
	limit := pos + 8
	if limit > len(blob) {
		limit = len(blob)
	}
	found := false
	for pos < limit {
		if blob[pos] == 0x2B {
			pos++
			found = true
			break
		}
		pos++
	}
	if !found || pos >= len(blob) {
		return ""
	}

	// Read length
	var length int
	if blob[pos] == 0x81 {
		// 2-byte little-endian uint16 length
		pos++
		if pos+2 > len(blob) {
			return ""
		}
		length = int(binary.LittleEndian.Uint16(blob[pos : pos+2]))
		pos += 2
	} else {
		// 1-byte length
		length = int(blob[pos])
		pos++
	}

	if length <= 0 || pos+length > len(blob) {
		return ""
	}

	text := string(blob[pos : pos+length])

	// Strip object replacement character (used for inline attachments)
	text = strings.ReplaceAll(text, "\uFFFC", "")

	return strings.TrimSpace(text)
}

// extractParentGUID extracts the parent message GUID from an associated_message_guid
// value. Format is typically "p:0/<GUID>" or "bp:<GUID>".
func extractParentGUID(assocGUID string) string {
	if assocGUID == "" {
		return ""
	}
	// Format: "p:0/GUID" or "p:1/GUID"
	if idx := strings.LastIndex(assocGUID, "/"); idx >= 0 {
		return assocGUID[idx+1:]
	}
	// Format: "bp:GUID"
	if strings.HasPrefix(assocGUID, "bp:") {
		return assocGUID[3:]
	}
	return assocGUID
}

// tapbackEmoji returns the emoji for an iMessage tapback type.
func tapbackEmoji(associatedType int) string {
	switch associatedType {
	case 2000:
		return "\u2764\uFE0F" // heart
	case 2001:
		return "\U0001F44D" // thumbsup
	case 2002:
		return "\U0001F44E" // thumbsdown
	case 2003:
		return "\U0001F602" // laugh
	case 2004:
		return "\u2757" // exclamation
	case 2005:
		return "\u2753" // question
	default:
		return ""
	}
}

// mediaPlaceholder returns a placeholder string based on MIME type.
func mediaPlaceholder(mimeType string) string {
	if mimeType == "" {
		return "[attachment]"
	}
	if strings.HasPrefix(mimeType, "image/") {
		return "[image]"
	}
	if strings.HasPrefix(mimeType, "video/") {
		return "[video]"
	}
	if strings.HasPrefix(mimeType, "audio/") {
		return "[audio]"
	}
	return "[document]"
}
