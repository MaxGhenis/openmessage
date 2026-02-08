package db

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
}

type Conversation struct {
	ConversationID string
	Name           string
	IsGroup        bool
	Participants   string // JSON array
	LastMessageTS  int64
	UnreadCount    int
}

type Message struct {
	MessageID      string
	ConversationID string
	SenderName     string
	SenderNumber   string
	Body           string
	TimestampMS    int64
	Status         string
	IsFromMe       bool
	MediaID        string `json:",omitempty"`
	MimeType       string `json:",omitempty"`
	DecryptionKey  string `json:"-"` // hex-encoded, never exposed in API
	Reactions      string `json:",omitempty"` // JSON array of {emoji, count}
	ReplyToID      string `json:",omitempty"`
}

type Contact struct {
	ContactID string
	Name      string
	Number    string
}

type Draft struct {
	DraftID        string
	ConversationID string
	Body           string
	CreatedAt      int64
}

func New(dsn string) (*Store, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("set WAL mode: %w", err)
	}
	if _, err := db.Exec("PRAGMA foreign_keys=ON"); err != nil {
		db.Close()
		return nil, fmt.Errorf("enable foreign keys: %w", err)
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) migrate() error {
	schema := `
	CREATE TABLE IF NOT EXISTS conversations (
		conversation_id TEXT PRIMARY KEY,
		name TEXT NOT NULL DEFAULT '',
		is_group INTEGER NOT NULL DEFAULT 0,
		participants TEXT NOT NULL DEFAULT '[]',
		last_message_ts INTEGER NOT NULL DEFAULT 0,
		unread_count INTEGER NOT NULL DEFAULT 0
	);

	CREATE TABLE IF NOT EXISTS messages (
		message_id TEXT PRIMARY KEY,
		conversation_id TEXT NOT NULL DEFAULT '',
		sender_name TEXT NOT NULL DEFAULT '',
		sender_number TEXT NOT NULL DEFAULT '',
		body TEXT NOT NULL DEFAULT '',
		timestamp_ms INTEGER NOT NULL DEFAULT 0,
		status TEXT NOT NULL DEFAULT '',
		is_from_me INTEGER NOT NULL DEFAULT 0,
		media_id TEXT NOT NULL DEFAULT '',
		mime_type TEXT NOT NULL DEFAULT '',
		decryption_key TEXT NOT NULL DEFAULT '',
		reactions TEXT NOT NULL DEFAULT '',
		reply_to_id TEXT NOT NULL DEFAULT ''
	);

	CREATE INDEX IF NOT EXISTS idx_messages_conv_ts ON messages(conversation_id, timestamp_ms);
	CREATE INDEX IF NOT EXISTS idx_messages_ts ON messages(timestamp_ms DESC);

	CREATE TABLE IF NOT EXISTS contacts (
		contact_id TEXT PRIMARY KEY,
		name TEXT NOT NULL DEFAULT '',
		number TEXT NOT NULL DEFAULT ''
	);

	CREATE TABLE IF NOT EXISTS drafts (
		draft_id TEXT PRIMARY KEY,
		conversation_id TEXT NOT NULL,
		body TEXT NOT NULL DEFAULT '',
		created_at INTEGER NOT NULL DEFAULT 0
	);
	`
	if _, err := s.db.Exec(schema); err != nil {
		return err
	}
	// Migrate existing DBs: add media columns if missing (ignore errors if they already exist)
	for _, col := range []string{
		"ALTER TABLE messages ADD COLUMN media_id TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE messages ADD COLUMN mime_type TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE messages ADD COLUMN decryption_key TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE messages ADD COLUMN reactions TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE messages ADD COLUMN reply_to_id TEXT NOT NULL DEFAULT ''",
	} {
		s.db.Exec(col) // ignore "duplicate column" errors
	}
	return nil
}
