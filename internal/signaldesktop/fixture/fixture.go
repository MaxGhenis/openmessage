// Package fixture writes synthetic Signal Desktop archives for tests: a plain
// (unencrypted) SQLite file with the subset of Signal Desktop's schema that
// the v2 history reader and the legacy importer query. Every value in a
// fixture is synthetic.
package fixture

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	_ "modernc.org/sqlite"
)

// Conversation is one row of Signal Desktop's conversations table.
type Conversation struct {
	ID          string
	Type        string // "private" or "group"
	E164        string
	ServiceID   string
	GroupID     string
	Name        string
	ProfileName string
	ProfileFull string
	Members     []string // the older members column
	MembersV2   []string // json.membersV2[].aci
}

// Attachment is one row of message_attachments.
type Attachment struct {
	Type             string // attachment, quote, preview, long-message, sticker
	Order            int
	EditHistoryIndex *int
	ContentType      string
	FileName         string
	Path             string
}

// Reaction is one json.reactions entry.
type Reaction struct {
	Emoji     string
	FromID    string // a Desktop conversation ID
	Timestamp int64
}

// Message is one row of Signal Desktop's messages table.
type Message struct {
	RowID              int64
	ID                 string
	ConversationID     string
	Type               string
	Body               string
	SentAt             int64
	ReceivedAt         int64
	Source             string
	SourceServiceID    string
	DeletedForEveryone bool
	ViewOnce           bool
	Sticker            bool
	Contact            bool
	Payment            bool
	Preview            bool
	Poll               bool
	StoryID            string
	SendState          map[string]string // conversation ID -> status
	Reactions          []Reaction
	JSONAttachments    []Attachment // attachments kept in json (older rows)
	Attachments        []Attachment // rows in message_attachments
}

// Archive is a whole synthetic Desktop database.
type Archive struct {
	SelfNumber string // items.number_id, without the device suffix
	SelfACI    string // items.uuid_id, without the device suffix
	// LegacyAttachmentsTable drops the typed message_attachments columns, as
	// on a Desktop version that predates them.
	LegacyAttachmentsTable bool
	Conversations          []Conversation
	Messages               []Message
}

const schema = `
CREATE TABLE items (id STRING PRIMARY KEY ASC, json TEXT);
CREATE TABLE conversations (
	id STRING PRIMARY KEY ASC, json TEXT, active_at INTEGER, type STRING,
	members TEXT, name TEXT, profileName TEXT, profileFamilyName TEXT,
	profileFullName TEXT, e164 TEXT, serviceId TEXT, groupId TEXT
);
CREATE TABLE messages (
	rowid INTEGER PRIMARY KEY ASC, id STRING UNIQUE, json TEXT,
	sent_at INTEGER, conversationId STRING, received_at INTEGER,
	source STRING, type STRING, body TEXT, sourceServiceId TEXT
);
`

const typedAttachments = `
CREATE TABLE message_attachments (
	messageId TEXT NOT NULL, editHistoryIndex INTEGER, attachmentType TEXT NOT NULL,
	orderInMessage INTEGER NOT NULL, contentType TEXT NOT NULL, path TEXT,
	fileName TEXT, caption TEXT
);
`

const legacyAttachments = `
CREATE TABLE message_attachments (
	messageId TEXT NOT NULL, contentType TEXT NOT NULL, path TEXT,
	fileName TEXT, caption TEXT
);
`

// Write creates the archive at path.
func Write(path string, archive Archive) error {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return err
	}
	defer db.Close()
	ddl := schema + typedAttachments
	if archive.LegacyAttachmentsTable {
		ddl = schema + legacyAttachments
	}
	if _, err := db.Exec(ddl); err != nil {
		return fmt.Errorf("create fixture schema: %w", err)
	}
	if archive.SelfNumber != "" {
		if err := insertItem(db, "number_id", archive.SelfNumber+".1"); err != nil {
			return err
		}
	}
	if archive.SelfACI != "" {
		if err := insertItem(db, "uuid_id", archive.SelfACI+".1"); err != nil {
			return err
		}
	}
	for _, conversation := range archive.Conversations {
		body := map[string]any{"id": conversation.ID, "type": conversation.Type}
		if len(conversation.MembersV2) > 0 {
			members := []map[string]any{}
			for _, aci := range conversation.MembersV2 {
				members = append(members, map[string]any{"aci": aci, "role": 1})
			}
			body["membersV2"] = members
		}
		encoded, _ := json.Marshal(body)
		if _, err := db.Exec(`INSERT INTO conversations (id, json, active_at, type, members, name, profileName, profileFamilyName, profileFullName, e164, serviceId, groupId)
			VALUES (?, ?, 0, ?, ?, ?, ?, '', ?, ?, ?, ?)`,
			conversation.ID, string(encoded), conversation.Type, nullable(strings.Join(conversation.Members, " ")),
			nullable(conversation.Name), nullable(conversation.ProfileName), nullable(conversation.ProfileFull),
			nullable(conversation.E164), nullable(conversation.ServiceID), nullable(conversation.GroupID)); err != nil {
			return fmt.Errorf("insert fixture conversation: %w", err)
		}
	}
	for _, message := range archive.Messages {
		body := map[string]any{"id": message.ID, "type": message.Type, "sent_at": message.SentAt}
		if message.DeletedForEveryone {
			body["deletedForEveryone"] = true
		}
		if message.ViewOnce {
			body["isViewOnce"] = true
		}
		if message.Sticker {
			body["sticker"] = map[string]any{"packId": "synthetic"}
		}
		if message.Contact {
			body["contact"] = []any{map[string]any{"name": "synthetic"}}
		}
		if message.Payment {
			body["payment"] = map[string]any{"kind": "synthetic"}
		}
		if message.Preview {
			body["preview"] = []any{map[string]any{"url": "https://example.invalid"}}
		}
		if message.Poll {
			body["poll"] = map[string]any{"question": "synthetic"}
		}
		if message.StoryID != "" {
			body["storyId"] = message.StoryID
		}
		if len(message.SendState) > 0 {
			states := map[string]any{}
			for conversationID, status := range message.SendState {
				states[conversationID] = map[string]any{"status": status, "updatedAt": message.SentAt}
			}
			body["sendStateByConversationId"] = states
		}
		if len(message.Reactions) > 0 {
			var reactions []any
			for _, reaction := range message.Reactions {
				reactions = append(reactions, map[string]any{"emoji": reaction.Emoji, "fromId": reaction.FromID, "timestamp": reaction.Timestamp, "targetTimestamp": message.SentAt})
			}
			body["reactions"] = reactions
		}
		if len(message.JSONAttachments) > 0 {
			var attachments []any
			for _, attachment := range message.JSONAttachments {
				attachments = append(attachments, map[string]any{"contentType": attachment.ContentType, "fileName": attachment.FileName})
			}
			body["attachments"] = attachments
		}
		encoded, _ := json.Marshal(body)
		if _, err := db.Exec(`INSERT INTO messages (rowid, id, json, sent_at, conversationId, received_at, source, type, body, sourceServiceId)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			message.RowID, message.ID, string(encoded), message.SentAt, message.ConversationID, message.ReceivedAt,
			nullable(message.Source), message.Type, nullable(message.Body), nullable(message.SourceServiceID)); err != nil {
			return fmt.Errorf("insert fixture message: %w", err)
		}
		for _, attachment := range message.Attachments {
			var err error
			if archive.LegacyAttachmentsTable {
				_, err = db.Exec(`INSERT INTO message_attachments (messageId, contentType, path, fileName) VALUES (?, ?, ?, ?)`,
					message.ID, attachment.ContentType, nullable(attachment.Path), nullable(attachment.FileName))
			} else {
				kind := attachment.Type
				if kind == "" {
					kind = "attachment"
				}
				var edit any
				if attachment.EditHistoryIndex != nil {
					edit = *attachment.EditHistoryIndex
				}
				_, err = db.Exec(`INSERT INTO message_attachments (messageId, editHistoryIndex, attachmentType, orderInMessage, contentType, path, fileName) VALUES (?, ?, ?, ?, ?, ?, ?)`,
					message.ID, edit, kind, attachment.Order, attachment.ContentType, nullable(attachment.Path), nullable(attachment.FileName))
			}
			if err != nil {
				return fmt.Errorf("insert fixture attachment: %w", err)
			}
		}
	}
	return nil
}

func insertItem(db *sql.DB, id, value string) error {
	encoded, _ := json.Marshal(map[string]string{"id": id, "value": value})
	_, err := db.Exec(`INSERT INTO items (id, json) VALUES (?, ?)`, id, string(encoded))
	return err
}

func nullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}
