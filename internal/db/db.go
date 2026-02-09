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
	// modernc.org/sqlite requires single connection to avoid "malformed" errors
	db.SetMaxOpenConns(1)
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

// SeedDemo populates the database with fake data for screenshots/demos.
func (s *Store) SeedDemo() error {
	inserts := `
INSERT OR IGNORE INTO conversations VALUES('conv3','Weekend Hiking Group',1,'[{"name":"Emily Park","number":"+13105553456"},{"name":"David Kim","number":"+14085557890"},{"name":"Alex Thompson","number":"+17185552222"}]',1738960200000,0);
INSERT OR IGNORE INTO conversations VALUES('conv1','Sarah Chen',0,'[{"name":"Sarah Chen","number":"+14155551234"}]',1738958400000,0);
INSERT OR IGNORE INTO conversations VALUES('conv2','Marcus Johnson',0,'[{"name":"Marcus Johnson","number":"+12125559876"}]',1738956600000,2);
INSERT OR IGNORE INTO conversations VALUES('conv4','Emily Park',0,'[{"name":"Emily Park","number":"+13105553456"}]',1738951200000,0);
INSERT OR IGNORE INTO conversations VALUES('conv5','Lisa Rodriguez',0,'[{"name":"Lisa Rodriguez","number":"+12025551111"}]',1738947600000,1);
INSERT OR IGNORE INTO conversations VALUES('conv6','David Kim',0,'[{"name":"David Kim","number":"+14085557890"}]',1738944000000,0);
INSERT OR IGNORE INTO conversations VALUES('conv7','Rachel Green',0,'[{"name":"Rachel Green","number":"+16505553333"}]',1738940400000,0);
INSERT OR IGNORE INTO conversations VALUES('conv8','Alex Thompson',0,'[{"name":"Alex Thompson","number":"+17185552222"}]',1738936800000,0);

INSERT OR IGNORE INTO messages VALUES('m3a','conv3','Emily Park','+13105553456','Anyone up for a hike this Saturday? Weather looks amazing',1738951200000,'delivered',0,'','','','','');
INSERT OR IGNORE INTO messages VALUES('m3b','conv3','David Kim','+14085557890','I''m in! Lands End or Battery to Bluffs?',1738953000000,'delivered',0,'','','','','');
INSERT OR IGNORE INTO messages VALUES('m3c','conv3','Alex Thompson','+17185552222','Lands End! The wildflowers should be gorgeous right now',1738955400000,'delivered',0,'','','','','');
INSERT OR IGNORE INTO messages VALUES('m3d','conv3','Emily Park','+13105553456','Lands End it is! 9am at the trailhead?',1738957800000,'delivered',0,'','','','','');
INSERT OR IGNORE INTO messages VALUES('m3e','conv3','David Kim','+14085557890','Perfect. I''ll bring coffee for everyone',1738960200000,'delivered',0,'','','','','');

INSERT OR IGNORE INTO messages VALUES('m1a','conv1','Sarah Chen','+14155551234','Hey! Are you free for dinner tonight?',1738951200000,'delivered',0,'','','','','');
INSERT OR IGNORE INTO messages VALUES('m1b','conv1','Me','+15551234567','Yes! What did you have in mind?',1738952100000,'delivered',1,'','','','','');
INSERT OR IGNORE INTO messages VALUES('m1c','conv1','Sarah Chen','+14155551234','There is a new Thai place on Valencia that just opened. Heard great things about their pad see ew',1738953000000,'delivered',0,'','','','','');
INSERT OR IGNORE INTO messages VALUES('m1d','conv1','Me','+15551234567','That sounds perfect! What time works for you?',1738954800000,'delivered',1,'','','','','');
INSERT OR IGNORE INTO messages VALUES('m1e','conv1','Sarah Chen','+14155551234','How about 7:30? I can make a reservation',1738956600000,'delivered',0,'','','','','');
INSERT OR IGNORE INTO messages VALUES('m1f','conv1','Me','+15551234567','Perfect, see you there!',1738958400000,'delivered',1,'','','','','');

INSERT OR IGNORE INTO messages VALUES('m2a','conv2','Marcus Johnson','+12125559876','Quick update on the project - we hit our Q1 milestone early!',1738944000000,'delivered',0,'','','','','');
INSERT OR IGNORE INTO messages VALUES('m2b','conv2','Me','+15551234567','That is awesome news! The team did a great job.',1738945800000,'delivered',1,'','','','','');
INSERT OR IGNORE INTO messages VALUES('m2c','conv2','Marcus Johnson','+12125559876','Agreed. Want to hop on a call Monday to discuss next steps?',1738947600000,'delivered',0,'','','','','');
INSERT OR IGNORE INTO messages VALUES('m2d','conv2','Marcus Johnson','+12125559876','Also, I sent over the slide deck to review when you get a chance',1738956600000,'delivered',0,'','','','','');

INSERT OR IGNORE INTO messages VALUES('m4a','conv4','Emily Park','+13105553456','Thanks for the book recommendation! I am already halfway through it',1738940400000,'delivered',0,'','','','','');
INSERT OR IGNORE INTO messages VALUES('m4b','conv4','Me','+15551234567','Glad you are enjoying it! The second half gets even better',1738951200000,'delivered',1,'','','','','');

INSERT OR IGNORE INTO messages VALUES('m5a','conv5','Lisa Rodriguez','+12025551111','Are we still on for coffee tomorrow morning?',1738936800000,'delivered',0,'','','','','');
INSERT OR IGNORE INTO messages VALUES('m5b','conv5','Me','+15551234567','Absolutely! Blue Bottle at 10?',1738938600000,'delivered',1,'','','','','');
INSERT OR IGNORE INTO messages VALUES('m5c','conv5','Lisa Rodriguez','+12025551111','Sounds great! I have some exciting news to share',1738947600000,'delivered',0,'','','','','');

INSERT OR IGNORE INTO messages VALUES('m6a','conv6','Me','+15551234567','Hey, did you see the Warriors game last night?',1738933200000,'delivered',1,'','','','','');
INSERT OR IGNORE INTO messages VALUES('m6b','conv6','David Kim','+14085557890','Incredible comeback! Curry was unreal in the 4th quarter',1738936800000,'delivered',0,'','','','','');
INSERT OR IGNORE INTO messages VALUES('m6c','conv6','Me','+15551234567','We should catch the next home game together',1738944000000,'delivered',1,'','','','','');

INSERT OR IGNORE INTO messages VALUES('m7a','conv7','Rachel Green','+16505553333','Just landed! Flight was smooth. Thanks for the ride to the airport',1738929600000,'delivered',0,'','','','','');
INSERT OR IGNORE INTO messages VALUES('m7b','conv7','Me','+15551234567','Anytime! Have an amazing trip',1738940400000,'delivered',1,'','','','','');

INSERT OR IGNORE INTO messages VALUES('m8a','conv8','Alex Thompson','+17185552222','Found that restaurant we were talking about - it is called Nopa',1738929600000,'delivered',0,'','','','','');
INSERT OR IGNORE INTO messages VALUES('m8b','conv8','Me','+15551234567','Nice find! Let us go next week',1738936800000,'delivered',1,'','','','','');

INSERT OR IGNORE INTO contacts VALUES('c1','Sarah Chen','+14155551234');
INSERT OR IGNORE INTO contacts VALUES('c2','Marcus Johnson','+12125559876');
INSERT OR IGNORE INTO contacts VALUES('c3','Emily Park','+13105553456');
INSERT OR IGNORE INTO contacts VALUES('c4','David Kim','+14085557890');
INSERT OR IGNORE INTO contacts VALUES('c5','Lisa Rodriguez','+12025551111');
INSERT OR IGNORE INTO contacts VALUES('c6','Alex Thompson','+17185552222');
INSERT OR IGNORE INTO contacts VALUES('c7','Rachel Green','+16505553333');

INSERT OR IGNORE INTO drafts VALUES('draft1','conv3','Count me in for Saturday! Lands End trail looks clear — 62°F and sunny. Want me to bring snacks?',1738961000000);
	`
	_, err := s.db.Exec(inserts)
	return err
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
