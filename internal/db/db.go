package db

import (
	"database/sql"
	"fmt"
	"strings"

	_ "modernc.org/sqlite"
)

type Store struct {
	db         *sql.DB
	ftsEnabled bool
}

type Conversation struct {
	ConversationID     string
	Name               string
	IsGroup            bool
	Participants       string // JSON array
	LastMessageTS      int64
	UnreadCount        int
	SourcePlatform     string `json:"source_platform,omitempty"`  // sms, gchat, imessage, whatsapp, signal, telegram
	DisplayProtocol    string `json:"display_protocol,omitempty"` // display-only SMS/RCS protocol detail
	LastMessagePreview string `json:"last_message_preview,omitempty"`
	UnifiedID          string `json:"unified_id,omitempty"`
	UnifiedName        string `json:"unified_name,omitempty"`
	IsFavorite         bool   `json:"is_favorite,omitempty"`
	NotificationMode   string `json:"notification_mode,omitempty"` // all, mentions, muted
	Tab                string `json:"tab,omitempty"`               // "" = Recent (inbox), "archive", or a custom tab id
}

type Message struct {
	MessageID       string
	ConversationID  string
	SenderName      string
	SenderNumber    string
	Body            string
	TimestampMS     int64
	Status          string
	IsFromMe        bool
	MentionsMe      bool   `json:"mentions_me,omitempty"`
	MediaID         string `json:",omitempty"`
	MimeType        string `json:",omitempty"`
	DecryptionKey   string `json:"-"`          // hex-encoded, never exposed in API
	Reactions       string `json:",omitempty"` // JSON array of {emoji, count}
	ReplyToID       string `json:",omitempty"`
	SourcePlatform  string `json:"source_platform,omitempty"` // sms, gchat, imessage, whatsapp, signal, telegram
	SourceID        string `json:"source_id,omitempty"`       // platform-specific original ID for dedup
	Transcript      string `json:"transcript,omitempty"`
	TranscribedAtMS int64  `json:"transcribed_at_ms,omitempty"`
	TranscriptModel string `json:"transcript_model,omitempty"`
}

type Contact struct {
	ContactID string
	Name      string
	Number    string
}

type ContactAvatar struct {
	AvatarID        string
	SourcePlatform  string
	ParticipantID   string
	ContactID       string
	PhoneNumber     string
	DisplayName     string
	MimeType        string
	ImageData       []byte
	ImageHash       string
	UpdatedAtMS     int64
	LastCheckedAtMS int64
}

type ContactAvatarCandidate struct {
	SourcePlatform string
	ParticipantID  string
	ContactID      string
	PhoneNumber    string
	DisplayName    string
	Source         string
}

// UnifiedContact maps a person across messaging platforms.
type UnifiedContact struct {
	UnifiedID   string `json:"unified_id"`
	DisplayName string `json:"display_name"`
	Identifiers string `json:"identifiers"` // JSON: [{"platform":"sms","value":"+1234"},{"platform":"gchat","value":"user@gmail.com"}]
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
	// The daemon (serve) and one-shot CLI commands (read/status/send/import)
	// open the same DB concurrently. Without a busy timeout, a second writer
	// gets an immediate SQLITE_BUSY and the write is lost; wait-and-retry
	// instead so concurrent writes serialize rather than drop.
	if _, err := db.Exec("PRAGMA busy_timeout=5000"); err != nil {
		db.Close()
		return nil, fmt.Errorf("set busy timeout: %w", err)
	}
	// NORMAL is the recommended durability level under WAL: safe across app
	// crashes (only an OS crash/power loss can lose the last transaction) and
	// markedly faster on the write-heavy live-sync path.
	if _, err := db.Exec("PRAGMA synchronous=NORMAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("set synchronous mode: %w", err)
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
INSERT OR IGNORE INTO conversations (conversation_id, name, is_group, participants, last_message_ts, unread_count, source_platform) VALUES('conv3','Weekend Hiking Group',1,'[{"name":"Emily Park","number":"+13105553456"},{"name":"David Kim","number":"+14085557890"},{"name":"Alex Thompson","number":"+17185552222"}]',1738960200000,0,'sms');
INSERT OR IGNORE INTO conversations (conversation_id, name, is_group, participants, last_message_ts, unread_count, source_platform) VALUES('conv1','Sarah Chen',0,'[{"name":"Sarah Chen","number":"+14155551234"}]',1738958400000,0,'sms');
INSERT OR IGNORE INTO conversations (conversation_id, name, is_group, participants, last_message_ts, unread_count, source_platform) VALUES('conv2','Marcus Johnson',0,'[{"name":"Marcus Johnson","number":"+12125559876"}]',1738956600000,2,'sms');
INSERT OR IGNORE INTO conversations (conversation_id, name, is_group, participants, last_message_ts, unread_count, source_platform) VALUES('conv4','Emily Park',0,'[{"name":"Emily Park","number":"+13105553456"}]',1738951200000,0,'sms');
INSERT OR IGNORE INTO conversations (conversation_id, name, is_group, participants, last_message_ts, unread_count, source_platform) VALUES('conv5','Lisa Rodriguez',0,'[{"name":"Lisa Rodriguez","number":"+12025551111"}]',1738947600000,1,'sms');
INSERT OR IGNORE INTO conversations (conversation_id, name, is_group, participants, last_message_ts, unread_count, source_platform) VALUES('conv6','David Kim',0,'[{"name":"David Kim","number":"+14085557890"}]',1738944000000,0,'sms');
INSERT OR IGNORE INTO conversations (conversation_id, name, is_group, participants, last_message_ts, unread_count, source_platform) VALUES('conv7','Rachel Green',0,'[{"name":"Rachel Green","number":"+16505553333"}]',1738940400000,0,'sms');
INSERT OR IGNORE INTO conversations (conversation_id, name, is_group, participants, last_message_ts, unread_count, source_platform) VALUES('conv8','Alex Thompson',0,'[{"name":"Alex Thompson","number":"+17185552222"}]',1738936800000,0,'sms');

INSERT OR IGNORE INTO messages (message_id, conversation_id, sender_name, sender_number, body, timestamp_ms, status, is_from_me, media_id, mime_type, decryption_key, reactions, reply_to_id, source_platform, source_id) VALUES('m3a','conv3','Emily Park','+13105553456','Anyone up for a hike this Saturday? Weather looks amazing',1738951200000,'delivered',0,'','','','','','sms','');
INSERT OR IGNORE INTO messages (message_id, conversation_id, sender_name, sender_number, body, timestamp_ms, status, is_from_me, media_id, mime_type, decryption_key, reactions, reply_to_id, source_platform, source_id) VALUES('m3b','conv3','David Kim','+14085557890','I''m in! Lands End or Battery to Bluffs?',1738953000000,'delivered',0,'','','','','','sms','');
INSERT OR IGNORE INTO messages (message_id, conversation_id, sender_name, sender_number, body, timestamp_ms, status, is_from_me, media_id, mime_type, decryption_key, reactions, reply_to_id, source_platform, source_id) VALUES('m3c','conv3','Alex Thompson','+17185552222','Lands End! The wildflowers should be gorgeous right now',1738955400000,'delivered',0,'','','','','','sms','');
INSERT OR IGNORE INTO messages (message_id, conversation_id, sender_name, sender_number, body, timestamp_ms, status, is_from_me, media_id, mime_type, decryption_key, reactions, reply_to_id, source_platform, source_id) VALUES('m3d','conv3','Emily Park','+13105553456','Lands End it is! 9am at the trailhead?',1738957800000,'delivered',0,'','','','','','sms','');
INSERT OR IGNORE INTO messages (message_id, conversation_id, sender_name, sender_number, body, timestamp_ms, status, is_from_me, media_id, mime_type, decryption_key, reactions, reply_to_id, source_platform, source_id) VALUES('m3e','conv3','David Kim','+14085557890','Perfect. I''ll bring coffee for everyone',1738960200000,'delivered',0,'','','','','','sms','');

INSERT OR IGNORE INTO messages (message_id, conversation_id, sender_name, sender_number, body, timestamp_ms, status, is_from_me, media_id, mime_type, decryption_key, reactions, reply_to_id, source_platform, source_id) VALUES('m1a','conv1','Sarah Chen','+14155551234','Hey! Are you free for dinner tonight?',1738951200000,'delivered',0,'','','','','','sms','');
INSERT OR IGNORE INTO messages (message_id, conversation_id, sender_name, sender_number, body, timestamp_ms, status, is_from_me, media_id, mime_type, decryption_key, reactions, reply_to_id, source_platform, source_id) VALUES('m1b','conv1','Me','+15551234567','Yes! What did you have in mind?',1738952100000,'delivered',1,'','','','','','sms','');
INSERT OR IGNORE INTO messages (message_id, conversation_id, sender_name, sender_number, body, timestamp_ms, status, is_from_me, media_id, mime_type, decryption_key, reactions, reply_to_id, source_platform, source_id) VALUES('m1c','conv1','Sarah Chen','+14155551234','There is a new Thai place on Valencia that just opened. Heard great things about their pad see ew',1738953000000,'delivered',0,'','','','','','sms','');
INSERT OR IGNORE INTO messages (message_id, conversation_id, sender_name, sender_number, body, timestamp_ms, status, is_from_me, media_id, mime_type, decryption_key, reactions, reply_to_id, source_platform, source_id) VALUES('m1d','conv1','Me','+15551234567','That sounds perfect! What time works for you?',1738954800000,'delivered',1,'','','','','','sms','');
INSERT OR IGNORE INTO messages (message_id, conversation_id, sender_name, sender_number, body, timestamp_ms, status, is_from_me, media_id, mime_type, decryption_key, reactions, reply_to_id, source_platform, source_id) VALUES('m1e','conv1','Sarah Chen','+14155551234','How about 7:30? I can make a reservation',1738956600000,'delivered',0,'','','','','','sms','');
INSERT OR IGNORE INTO messages (message_id, conversation_id, sender_name, sender_number, body, timestamp_ms, status, is_from_me, media_id, mime_type, decryption_key, reactions, reply_to_id, source_platform, source_id) VALUES('m1f','conv1','Me','+15551234567','Perfect, see you there!',1738958400000,'delivered',1,'','','','','','sms','');

INSERT OR IGNORE INTO messages (message_id, conversation_id, sender_name, sender_number, body, timestamp_ms, status, is_from_me, media_id, mime_type, decryption_key, reactions, reply_to_id, source_platform, source_id) VALUES('m2a','conv2','Marcus Johnson','+12125559876','Quick update on the project - we hit our Q1 milestone early!',1738944000000,'delivered',0,'','','','','','sms','');
INSERT OR IGNORE INTO messages (message_id, conversation_id, sender_name, sender_number, body, timestamp_ms, status, is_from_me, media_id, mime_type, decryption_key, reactions, reply_to_id, source_platform, source_id) VALUES('m2b','conv2','Me','+15551234567','That is awesome news! The team did a great job.',1738945800000,'delivered',1,'','','','','','sms','');
INSERT OR IGNORE INTO messages (message_id, conversation_id, sender_name, sender_number, body, timestamp_ms, status, is_from_me, media_id, mime_type, decryption_key, reactions, reply_to_id, source_platform, source_id) VALUES('m2c','conv2','Marcus Johnson','+12125559876','Agreed. Want to hop on a call Monday to discuss next steps?',1738947600000,'delivered',0,'','','','','','sms','');
INSERT OR IGNORE INTO messages (message_id, conversation_id, sender_name, sender_number, body, timestamp_ms, status, is_from_me, media_id, mime_type, decryption_key, reactions, reply_to_id, source_platform, source_id) VALUES('m2d','conv2','Marcus Johnson','+12125559876','Also, I sent over the slide deck to review when you get a chance',1738956600000,'delivered',0,'','','','','','sms','');

INSERT OR IGNORE INTO messages (message_id, conversation_id, sender_name, sender_number, body, timestamp_ms, status, is_from_me, media_id, mime_type, decryption_key, reactions, reply_to_id, source_platform, source_id) VALUES('m4a','conv4','Emily Park','+13105553456','Thanks for the book recommendation! I am already halfway through it',1738940400000,'delivered',0,'','','','','','sms','');
INSERT OR IGNORE INTO messages (message_id, conversation_id, sender_name, sender_number, body, timestamp_ms, status, is_from_me, media_id, mime_type, decryption_key, reactions, reply_to_id, source_platform, source_id) VALUES('m4b','conv4','Me','+15551234567','Glad you are enjoying it! The second half gets even better',1738951200000,'delivered',1,'','','','','','sms','');

INSERT OR IGNORE INTO messages (message_id, conversation_id, sender_name, sender_number, body, timestamp_ms, status, is_from_me, media_id, mime_type, decryption_key, reactions, reply_to_id, source_platform, source_id) VALUES('m5a','conv5','Lisa Rodriguez','+12025551111','Are we still on for coffee tomorrow morning?',1738936800000,'delivered',0,'','','','','','sms','');
INSERT OR IGNORE INTO messages (message_id, conversation_id, sender_name, sender_number, body, timestamp_ms, status, is_from_me, media_id, mime_type, decryption_key, reactions, reply_to_id, source_platform, source_id) VALUES('m5b','conv5','Me','+15551234567','Absolutely! Blue Bottle at 10?',1738938600000,'delivered',1,'','','','','','sms','');
INSERT OR IGNORE INTO messages (message_id, conversation_id, sender_name, sender_number, body, timestamp_ms, status, is_from_me, media_id, mime_type, decryption_key, reactions, reply_to_id, source_platform, source_id) VALUES('m5c','conv5','Lisa Rodriguez','+12025551111','Sounds great! I have some exciting news to share',1738947600000,'delivered',0,'','','','','','sms','');

INSERT OR IGNORE INTO messages (message_id, conversation_id, sender_name, sender_number, body, timestamp_ms, status, is_from_me, media_id, mime_type, decryption_key, reactions, reply_to_id, source_platform, source_id) VALUES('m6a','conv6','Me','+15551234567','Hey, did you see the Warriors game last night?',1738933200000,'delivered',1,'','','','','','sms','');
INSERT OR IGNORE INTO messages (message_id, conversation_id, sender_name, sender_number, body, timestamp_ms, status, is_from_me, media_id, mime_type, decryption_key, reactions, reply_to_id, source_platform, source_id) VALUES('m6b','conv6','David Kim','+14085557890','Incredible comeback! Curry was unreal in the 4th quarter',1738936800000,'delivered',0,'','','','','','sms','');
INSERT OR IGNORE INTO messages (message_id, conversation_id, sender_name, sender_number, body, timestamp_ms, status, is_from_me, media_id, mime_type, decryption_key, reactions, reply_to_id, source_platform, source_id) VALUES('m6c','conv6','Me','+15551234567','We should catch the next home game together',1738944000000,'delivered',1,'','','','','','sms','');

INSERT OR IGNORE INTO messages (message_id, conversation_id, sender_name, sender_number, body, timestamp_ms, status, is_from_me, media_id, mime_type, decryption_key, reactions, reply_to_id, source_platform, source_id) VALUES('m7a','conv7','Rachel Green','+16505553333','Just landed! Flight was smooth. Thanks for the ride to the airport',1738929600000,'delivered',0,'','','','','','sms','');
INSERT OR IGNORE INTO messages (message_id, conversation_id, sender_name, sender_number, body, timestamp_ms, status, is_from_me, media_id, mime_type, decryption_key, reactions, reply_to_id, source_platform, source_id) VALUES('m7b','conv7','Me','+15551234567','Anytime! Have an amazing trip',1738940400000,'delivered',1,'','','','','','sms','');

INSERT OR IGNORE INTO messages (message_id, conversation_id, sender_name, sender_number, body, timestamp_ms, status, is_from_me, media_id, mime_type, decryption_key, reactions, reply_to_id, source_platform, source_id) VALUES('m8a','conv8','Alex Thompson','+17185552222','Found that restaurant we were talking about - it is called Nopa',1738929600000,'delivered',0,'','','','','','sms','');
INSERT OR IGNORE INTO messages (message_id, conversation_id, sender_name, sender_number, body, timestamp_ms, status, is_from_me, media_id, mime_type, decryption_key, reactions, reply_to_id, source_platform, source_id) VALUES('m8b','conv8','Me','+15551234567','Nice find! Let us go next week',1738936800000,'delivered',1,'','','','','','sms','');

INSERT OR IGNORE INTO conversations (conversation_id, name, is_group, participants, last_message_ts, unread_count, source_platform) VALUES('wa1','Jordan Rivera',0,'[{"name":"Jordan Rivera","number":"+14699991654"}]',1738959600000,0,'whatsapp');
INSERT OR IGNORE INTO conversations (conversation_id, name, is_group, participants, last_message_ts, unread_count, source_platform) VALUES('conv9','Jordan Rivera',0,'[{"name":"Jordan Rivera","number":"+14699991654"}]',1738959000000,0,'sms');
INSERT OR IGNORE INTO conversations (conversation_id, name, is_group, participants, last_message_ts, unread_count, source_platform) VALUES('wa2','Weekend Plans',1,'[{"name":"Mia Torres","number":"+12025557777"},{"name":"Noah Patel","number":"+13105558888"}]',1738957200000,0,'whatsapp');
INSERT OR IGNORE INTO conversations (conversation_id, name, is_group, participants, last_message_ts, unread_count, source_platform) VALUES('wa3','Mia Torres',0,'[{"name":"Mia Torres","number":"+12025557777"}]',1738950000000,0,'whatsapp');

-- Signal conversations. Signal contacts are keyed by ACI (account identifier)
-- UUID rather than phone number — participants.number holds the ACI. Some
-- contacts overlap with SMS/WhatsApp (Jordan Rivera) so the sidebar
-- demonstrates the people-first grouping across all three platforms.
INSERT OR IGNORE INTO conversations (conversation_id, name, is_group, participants, last_message_ts, unread_count, source_platform) VALUES('signal:a1a98e48-1111-4000-a000-000000000001','Jordan Rivera',0,'[{"name":"Jordan Rivera","number":"a1a98e48-1111-4000-a000-000000000001"}]',1738958700000,0,'signal');
INSERT OR IGNORE INTO conversations (conversation_id, name, is_group, participants, last_message_ts, unread_count, source_platform) VALUES('signal:b2b98e48-2222-4000-a000-000000000002','Priya Shah',0,'[{"name":"Priya Shah","number":"b2b98e48-2222-4000-a000-000000000002"}]',1738946400000,1,'signal');
INSERT OR IGNORE INTO conversations (conversation_id, name, is_group, participants, last_message_ts, unread_count, source_platform) VALUES('signal-group:c3c98e48-3333-4000-a000-000000000003','Climbing Crew',1,'[{"name":"Jordan Rivera","number":"a1a98e48-1111-4000-a000-000000000001"},{"name":"Priya Shah","number":"b2b98e48-2222-4000-a000-000000000002"},{"name":"Theo Nakamura","number":"d4d98e48-4444-4000-a000-000000000004"}]',1738935600000,0,'signal');

-- Jordan Rivera — Signal thread. Narrative: moved the sensitive part of the
-- conversation (early draft of a job offer letter) onto Signal. Back-and-forth
-- about timing and a redacted excerpt.
INSERT OR IGNORE INTO messages (message_id, conversation_id, sender_name, sender_number, body, timestamp_ms, status, is_from_me, media_id, mime_type, decryption_key, reactions, reply_to_id, source_platform, source_id) VALUES('sig1a','signal:a1a98e48-1111-4000-a000-000000000001','Jordan Rivera','a1a98e48-1111-4000-a000-000000000001','hey, moving this to Signal — don''t want the offer draft floating in a backed-up thread',1738940400000,'delivered',0,'','','','','','signal','');
INSERT OR IGNORE INTO messages (message_id, conversation_id, sender_name, sender_number, body, timestamp_ms, status, is_from_me, media_id, mime_type, decryption_key, reactions, reply_to_id, source_platform, source_id) VALUES('sig1b','signal:a1a98e48-1111-4000-a000-000000000001','Me','+15551234567','Good call. What''s the number looking like?',1738941000000,'delivered',1,'','','','','','signal','');
INSERT OR IGNORE INTO messages (message_id, conversation_id, sender_name, sender_number, body, timestamp_ms, status, is_from_me, media_id, mime_type, decryption_key, reactions, reply_to_id, source_platform, source_id) VALUES('sig1c','signal:a1a98e48-1111-4000-a000-000000000001','Jordan Rivera','a1a98e48-1111-4000-a000-000000000001','base is 185, equity is a real number this time. signing happens before Friday if you''re good',1738941720000,'delivered',0,'','','','','','signal','');
INSERT OR IGNORE INTO messages (message_id, conversation_id, sender_name, sender_number, body, timestamp_ms, status, is_from_me, media_id, mime_type, decryption_key, reactions, reply_to_id, source_platform, source_id) VALUES('sig1d','signal:a1a98e48-1111-4000-a000-000000000001','Me','+15551234567','ok, I''ll read it tonight and come back tomorrow morning',1738942200000,'delivered',1,'','','','','','signal','');
INSERT OR IGNORE INTO messages (message_id, conversation_id, sender_name, sender_number, body, timestamp_ms, status, is_from_me, media_id, mime_type, decryption_key, reactions, reply_to_id, source_platform, source_id) VALUES('sig1e','signal:a1a98e48-1111-4000-a000-000000000001','Jordan Rivera','a1a98e48-1111-4000-a000-000000000001','perfect. and obviously: keep the number off any thread we don''t trust',1738955400000,'delivered',0,'','','','','','signal','');
INSERT OR IGNORE INTO messages (message_id, conversation_id, sender_name, sender_number, body, timestamp_ms, status, is_from_me, media_id, mime_type, decryption_key, reactions, reply_to_id, source_platform, source_id) VALUES('sig1f','signal:a1a98e48-1111-4000-a000-000000000001','Me','+15551234567','100%. on here and nowhere else.',1738958700000,'delivered',1,'','','','','','signal','');

INSERT OR IGNORE INTO messages (message_id, conversation_id, sender_name, sender_number, body, timestamp_ms, status, is_from_me, media_id, mime_type, decryption_key, reactions, reply_to_id, source_platform, source_id) VALUES('sig2a','signal:b2b98e48-2222-4000-a000-000000000002','Priya Shah','b2b98e48-2222-4000-a000-000000000002','hey can we talk about the draft on here?',1738944000000,'delivered',0,'','','','','','signal','');
INSERT OR IGNORE INTO messages (message_id, conversation_id, sender_name, sender_number, body, timestamp_ms, status, is_from_me, media_id, mime_type, decryption_key, reactions, reply_to_id, source_platform, source_id) VALUES('sig2b','signal:b2b98e48-2222-4000-a000-000000000002','Priya Shah','b2b98e48-2222-4000-a000-000000000002','got a few notes that are probably sensitive',1738946400000,'delivered',0,'','','','','','signal','');

INSERT OR IGNORE INTO messages (message_id, conversation_id, sender_name, sender_number, body, timestamp_ms, status, is_from_me, media_id, mime_type, decryption_key, reactions, reply_to_id, source_platform, source_id) VALUES('sig3a','signal-group:c3c98e48-3333-4000-a000-000000000003','Theo Nakamura','d4d98e48-4444-4000-a000-000000000004','anyone up for Mission Cliffs tomorrow?',1738932000000,'delivered',0,'','','','','','signal','');
INSERT OR IGNORE INTO messages (message_id, conversation_id, sender_name, sender_number, body, timestamp_ms, status, is_from_me, media_id, mime_type, decryption_key, reactions, reply_to_id, source_platform, source_id) VALUES('sig3b','signal-group:c3c98e48-3333-4000-a000-000000000003','Priya Shah','b2b98e48-2222-4000-a000-000000000002','in! 6pm?',1738933200000,'delivered',0,'','','','','','signal','');
INSERT OR IGNORE INTO messages (message_id, conversation_id, sender_name, sender_number, body, timestamp_ms, status, is_from_me, media_id, mime_type, decryption_key, reactions, reply_to_id, source_platform, source_id) VALUES('sig3c','signal-group:c3c98e48-3333-4000-a000-000000000003','Jordan Rivera','a1a98e48-1111-4000-a000-000000000001','I can do 6:30',1738935600000,'delivered',0,'','','','','','signal','');

-- Jordan Rivera — WhatsApp thread. Casual side of the relationship: photos
-- from a Lisbon trip, where they're eating tonight, asking about dessert.
INSERT OR IGNORE INTO messages (message_id, conversation_id, sender_name, sender_number, body, timestamp_ms, status, is_from_me, media_id, mime_type, decryption_key, reactions, reply_to_id, source_platform, source_id) VALUES('wa1a','wa1','Jordan Rivera','+14699991654','just landed! Lisbon was unreal',1738867200000,'delivered',0,'','','','','','whatsapp','');
INSERT OR IGNORE INTO messages (message_id, conversation_id, sender_name, sender_number, body, timestamp_ms, status, is_from_me, media_id, mime_type, decryption_key, reactions, reply_to_id, source_platform, source_id) VALUES('wa1b','wa1','Jordan Rivera','+14699991654','the Alfama photo is for you specifically',1738867500000,'delivered',0,'','','','','','whatsapp','');
INSERT OR IGNORE INTO messages (message_id, conversation_id, sender_name, sender_number, body, timestamp_ms, status, is_from_me, media_id, mime_type, decryption_key, reactions, reply_to_id, source_platform, source_id) VALUES('wa1c','wa1','Me','+15551234567','that light 😍 welcome back. dinner this week?',1738871100000,'delivered',1,'','','','','','whatsapp','');
INSERT OR IGNORE INTO messages (message_id, conversation_id, sender_name, sender_number, body, timestamp_ms, status, is_from_me, media_id, mime_type, decryption_key, reactions, reply_to_id, source_platform, source_id) VALUES('wa1d','wa1','Jordan Rivera','+14699991654','thursday works. you pick the place',1738876800000,'delivered',0,'','','','','','whatsapp','');
INSERT OR IGNORE INTO messages (message_id, conversation_id, sender_name, sender_number, body, timestamp_ms, status, is_from_me, media_id, mime_type, decryption_key, reactions, reply_to_id, source_platform, source_id) VALUES('wa1e','wa1','Me','+15551234567','Nopa. 7:30. they have that short-rib thing you liked',1738884600000,'delivered',1,'','','','','','whatsapp','');
INSERT OR IGNORE INTO messages (message_id, conversation_id, sender_name, sender_number, body, timestamp_ms, status, is_from_me, media_id, mime_type, decryption_key, reactions, reply_to_id, source_platform, source_id) VALUES('wa1f','wa1','Jordan Rivera','+14699991654','sold. sending you the menu in case WhatsApp is easier than their pdf',1738955400000,'delivered',0,'','','','','','whatsapp','');
INSERT OR IGNORE INTO messages (message_id, conversation_id, sender_name, sender_number, body, timestamp_ms, status, is_from_me, media_id, mime_type, decryption_key, reactions, reply_to_id, source_platform, source_id) VALUES('wa1g','wa1','Jordan Rivera','+14699991654','also: want me to bring dessert or are we doing their olive oil cake situation?',1738959600000,'delivered',0,'','','','','','whatsapp','');

-- Jordan Rivera — SMS thread. Day-of logistics that hop to SMS because it
-- works when someone's on airplane wifi / data-only / just faster. Short,
-- transactional.
INSERT OR IGNORE INTO messages (message_id, conversation_id, sender_name, sender_number, body, timestamp_ms, status, is_from_me, media_id, mime_type, decryption_key, reactions, reply_to_id, source_platform, source_id) VALUES('m9a','conv9','Jordan Rivera','+14699991654','we still on for 7:30?',1738950000000,'delivered',0,'','','','','','sms','');
INSERT OR IGNORE INTO messages (message_id, conversation_id, sender_name, sender_number, body, timestamp_ms, status, is_from_me, media_id, mime_type, decryption_key, reactions, reply_to_id, source_platform, source_id) VALUES('m9b','conv9','Me','+15551234567','yes. running 10 min late',1738951200000,'delivered',1,'','','','','','sms','');
INSERT OR IGNORE INTO messages (message_id, conversation_id, sender_name, sender_number, body, timestamp_ms, status, is_from_me, media_id, mime_type, decryption_key, reactions, reply_to_id, source_platform, source_id) VALUES('m9c','conv9','Jordan Rivera','+14699991654','no rush, just got a table',1738952400000,'delivered',0,'','','','','','sms','');
INSERT OR IGNORE INTO messages (message_id, conversation_id, sender_name, sender_number, body, timestamp_ms, status, is_from_me, media_id, mime_type, decryption_key, reactions, reply_to_id, source_platform, source_id) VALUES('m9d','conv9','Me','+15551234567','walking up now',1738959000000,'delivered',1,'','','','','','sms','');

INSERT OR IGNORE INTO messages (message_id, conversation_id, sender_name, sender_number, body, timestamp_ms, status, is_from_me, media_id, mime_type, decryption_key, reactions, reply_to_id, source_platform, source_id) VALUES('wa2a','wa2','Mia Torres','+12025557777','Should we do brunch or dinner Saturday?',1738951200000,'delivered',0,'','','','','','whatsapp','');
INSERT OR IGNORE INTO messages (message_id, conversation_id, sender_name, sender_number, body, timestamp_ms, status, is_from_me, media_id, mime_type, decryption_key, reactions, reply_to_id, source_platform, source_id) VALUES('wa2b','wa2','Noah Patel','+13105558888','Brunch! That new place on 14th has a great patio',1738953000000,'delivered',0,'','','','','','whatsapp','');
INSERT OR IGNORE INTO messages (message_id, conversation_id, sender_name, sender_number, body, timestamp_ms, status, is_from_me, media_id, mime_type, decryption_key, reactions, reply_to_id, source_platform, source_id) VALUES('wa2c','wa2','Me','+15551234567','I am in for brunch. 11am?',1738957200000,'delivered',1,'','','','','','whatsapp','');

INSERT OR IGNORE INTO messages (message_id, conversation_id, sender_name, sender_number, body, timestamp_ms, status, is_from_me, media_id, mime_type, decryption_key, reactions, reply_to_id, source_platform, source_id) VALUES('wa3a','wa3','Mia Torres','+12025557777','Hey, can you send me that article you mentioned?',1738944000000,'delivered',0,'','','','','','whatsapp','');
INSERT OR IGNORE INTO messages (message_id, conversation_id, sender_name, sender_number, body, timestamp_ms, status, is_from_me, media_id, mime_type, decryption_key, reactions, reply_to_id, source_platform, source_id) VALUES('wa3b','wa3','Me','+15551234567','Just sent it! Let me know what you think',1738950000000,'delivered',1,'','','','','','whatsapp','');

INSERT OR IGNORE INTO contacts VALUES('c1','Sarah Chen','+14155551234');
INSERT OR IGNORE INTO contacts VALUES('c2','Marcus Johnson','+12125559876');
INSERT OR IGNORE INTO contacts VALUES('c3','Emily Park','+13105553456');
INSERT OR IGNORE INTO contacts VALUES('c4','David Kim','+14085557890');
INSERT OR IGNORE INTO contacts VALUES('c5','Lisa Rodriguez','+12025551111');
INSERT OR IGNORE INTO contacts VALUES('c6','Alex Thompson','+17185552222');
INSERT OR IGNORE INTO contacts VALUES('c7','Rachel Green','+16505553333');
INSERT OR IGNORE INTO contacts VALUES('c8','Jordan Rivera','+14699991654');
INSERT OR IGNORE INTO contacts VALUES('c9','Mia Torres','+12025557777');
INSERT OR IGNORE INTO contacts VALUES('c10','Noah Patel','+13105558888');
INSERT OR IGNORE INTO contacts VALUES('c11','Priya Shah','b2b98e48-2222-4000-a000-000000000002');
INSERT OR IGNORE INTO contacts VALUES('c12','Theo Nakamura','d4d98e48-4444-4000-a000-000000000004');

-- Unified identity for Jordan Rivera: SMS phone + WhatsApp phone (same number)
-- + Signal ACI. This is what enables the people-first sidebar to collapse
-- all three conversations into a single row with route tabs in the thread,
-- which is the feature we're showing off in launch screenshots/GIFs.
INSERT OR IGNORE INTO unified_contacts (unified_id, display_name, identifiers) VALUES(
  'u-jordan',
  'Jordan Rivera',
  '[{"platform":"sms","value":"+14699991654"},{"platform":"whatsapp","value":"+14699991654"},{"platform":"signal","value":"a1a98e48-1111-4000-a000-000000000001"}]'
);

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
		unread_count INTEGER NOT NULL DEFAULT 0,
		notification_mode TEXT NOT NULL DEFAULT 'all',
		tab TEXT NOT NULL DEFAULT ''
	);

	CREATE TABLE IF NOT EXISTS tabs (
		tab_id TEXT PRIMARY KEY,
		name TEXT NOT NULL DEFAULT '',
		position INTEGER NOT NULL DEFAULT 0,
		created_at INTEGER NOT NULL DEFAULT 0
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
		mentions_me INTEGER NOT NULL DEFAULT 0,
		media_id TEXT NOT NULL DEFAULT '',
		mime_type TEXT NOT NULL DEFAULT '',
		decryption_key TEXT NOT NULL DEFAULT '',
		reactions TEXT NOT NULL DEFAULT '',
		reply_to_id TEXT NOT NULL DEFAULT ''
	);

	CREATE INDEX IF NOT EXISTS idx_messages_conv_ts ON messages(conversation_id, timestamp_ms);
	CREATE INDEX IF NOT EXISTS idx_messages_ts ON messages(timestamp_ms DESC);
	-- Person/phone lookups (get_person_messages, --phone, metadata search JOIN)
	-- filter on sender_number; without this they full-scan the messages table.
	CREATE INDEX IF NOT EXISTS idx_messages_sender ON messages(sender_number);

	CREATE TABLE IF NOT EXISTS contacts (
		contact_id TEXT PRIMARY KEY,
		name TEXT NOT NULL DEFAULT '',
		number TEXT NOT NULL DEFAULT ''
	);

	CREATE TABLE IF NOT EXISTS contact_avatars (
		avatar_id TEXT PRIMARY KEY,
		source_platform TEXT NOT NULL,
		participant_id TEXT NOT NULL DEFAULT '',
		contact_id TEXT NOT NULL DEFAULT '',
		phone_number TEXT NOT NULL DEFAULT '',
		display_name TEXT NOT NULL DEFAULT '',
		mime_type TEXT NOT NULL DEFAULT 'image/jpeg',
		image_data BLOB,
		image_hash TEXT NOT NULL DEFAULT '',
		updated_at_ms INTEGER NOT NULL DEFAULT 0,
		last_checked_at_ms INTEGER NOT NULL DEFAULT 0
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
	// Migrate existing DBs: add columns if missing (ignore errors if they already exist)
	for _, col := range []string{
		"ALTER TABLE messages ADD COLUMN mentions_me INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE messages ADD COLUMN media_id TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE messages ADD COLUMN mime_type TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE messages ADD COLUMN decryption_key TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE messages ADD COLUMN reactions TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE messages ADD COLUMN reply_to_id TEXT NOT NULL DEFAULT ''",
		// Multi-source support
		"ALTER TABLE messages ADD COLUMN source_platform TEXT NOT NULL DEFAULT 'sms'",
		"ALTER TABLE messages ADD COLUMN source_id TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE messages ADD COLUMN transcript TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE messages ADD COLUMN transcribed_at INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE messages ADD COLUMN transcript_model TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE conversations ADD COLUMN source_platform TEXT NOT NULL DEFAULT 'sms'",
		"ALTER TABLE conversations ADD COLUMN display_protocol TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE conversations ADD COLUMN notification_mode TEXT NOT NULL DEFAULT 'all'",
		"ALTER TABLE conversations ADD COLUMN is_favorite INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE conversations ADD COLUMN tab TEXT NOT NULL DEFAULT ''",
	} {
		s.db.Exec(col) // ignore "duplicate column" errors
	}
	if _, err := s.db.Exec(`UPDATE messages SET source_platform = 'sms' WHERE IFNULL(source_platform, '') = ''`); err != nil {
		return fmt.Errorf("normalize blank message source platform: %w", err)
	}
	if _, err := s.db.Exec(`UPDATE conversations SET source_platform = 'sms' WHERE IFNULL(source_platform, '') = ''`); err != nil {
		return fmt.Errorf("normalize blank conversation source platform: %w", err)
	}

	// Unified contacts table
	s.db.Exec(`CREATE TABLE IF NOT EXISTS unified_contacts (
		unified_id TEXT PRIMARY KEY,
		display_name TEXT NOT NULL DEFAULT '',
		identifiers TEXT NOT NULL DEFAULT '[]'
	)`)

	// Index for dedup on import
	s.db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_messages_source ON messages(source_platform, source_id) WHERE source_id != ''`)

	// Index for platform-filtered conversation queries
	s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_conversations_platform ON conversations(source_platform)`)

	// Index for the contacts.number = messages.sender_number JOIN in metadata search
	s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_contacts_number ON contacts(number)`)

	if _, err := s.db.Exec(`
		DROP INDEX IF EXISTS idx_contact_avatars_source_participant;
		DROP INDEX IF EXISTS idx_contact_avatars_source_contact;
		DROP INDEX IF EXISTS idx_contact_avatars_source_phone_only;
		CREATE INDEX IF NOT EXISTS idx_contact_avatars_source_participant
			ON contact_avatars(source_platform, participant_id)
			WHERE participant_id != '';
		CREATE INDEX IF NOT EXISTS idx_contact_avatars_source_contact
			ON contact_avatars(source_platform, contact_id)
			WHERE contact_id != '';
		CREATE UNIQUE INDEX IF NOT EXISTS idx_contact_avatars_source_phone_only
			ON contact_avatars(source_platform, phone_number)
			WHERE participant_id = '' AND contact_id = '' AND phone_number != '';
		CREATE INDEX IF NOT EXISTS idx_contact_avatars_source_phone
			ON contact_avatars(source_platform, phone_number);
	`); err != nil {
		return err
	}

	// One-shot: consolidate any Signal duplicate clusters left over from
	// the pre-v0.2.9 schema where the message_id hash included body. Now
	// that hash is body-independent, (conv, sender, timestamp) is the true
	// identity. Keep the row with the most reactions / longest body per
	// cluster; delete the rest. Idempotent — no-op when run again.
	s.db.Exec(`
		WITH ranked AS (
			SELECT
				rowid,
				ROW_NUMBER() OVER (
					PARTITION BY conversation_id, sender_number, timestamp_ms
					ORDER BY length(reactions) DESC, length(body) DESC, message_id ASC
				) AS rn
			FROM messages
			WHERE source_platform = 'signal' AND timestamp_ms > 0
		)
		DELETE FROM messages
		WHERE rowid IN (SELECT rowid FROM ranked WHERE rn > 1)
	`)

	if err := s.enableFTS(); err != nil {
		return err
	}

	return nil
}

func (s *Store) enableFTS() error {
	if err := s.rebuildFTS(); err != nil {
		if !isRecoverableFTSError(err) {
			return nil
		}
		if dropErr := s.dropFTSArtifacts(); dropErr != nil {
			return fmt.Errorf("recover messages search index: %w", dropErr)
		}
		if rebuildErr := s.rebuildFTS(); rebuildErr != nil {
			return fmt.Errorf("recover messages search index: %w", rebuildErr)
		}
	}
	return nil
}

// rebuildFTS ensures the FTS table exists and is populated. It only pays the
// cost of a full repopulation when the index is actually out of sync with the
// messages table (row counts differ). On a normal start the incremental
// maintenance in syncMessageSearchIndex keeps them equal, so this is just a
// cheap count check instead of re-indexing the entire archive on every CLI
// invocation and daemon start.
func (s *Store) rebuildFTS() error {
	if _, err := s.db.Exec(`CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts USING fts5(
		message_id UNINDEXED,
		body,
		tokenize='trigram'
	)`); err != nil {
		return fmt.Errorf("create messages search index: %w", err)
	}
	s.ftsEnabled = true
	var ftsCount, msgCount int
	if err := s.db.QueryRow(`SELECT count(*) FROM messages_fts`).Scan(&ftsCount); err != nil {
		return fmt.Errorf("count messages search index: %w", err)
	}
	if err := s.db.QueryRow(`SELECT count(*) FROM messages`).Scan(&msgCount); err != nil {
		return fmt.Errorf("count messages: %w", err)
	}
	if ftsCount == msgCount {
		return nil
	}
	return s.forceRebuildFTS()
}

// forceRebuildFTS unconditionally repopulates the FTS index from messages.
// Assumes the messages_fts table already exists.
func (s *Store) forceRebuildFTS() error {
	if _, err := s.db.Exec(`DELETE FROM messages_fts`); err != nil {
		return fmt.Errorf("reset messages search index: %w", err)
	}
	if _, err := s.db.Exec(`INSERT INTO messages_fts(message_id, body) SELECT message_id, body FROM messages`); err != nil {
		return fmt.Errorf("rebuild messages search index: %w", err)
	}
	return nil
}

func (s *Store) dropFTSArtifacts() error {
	for _, stmt := range []string{
		`DROP TABLE IF EXISTS messages_fts`,
		`DROP TABLE IF EXISTS messages_fts_data`,
		`DROP TABLE IF EXISTS messages_fts_idx`,
		`DROP TABLE IF EXISTS messages_fts_content`,
		`DROP TABLE IF EXISTS messages_fts_docsize`,
		`DROP TABLE IF EXISTS messages_fts_config`,
	} {
		if _, err := s.db.Exec(stmt); err != nil {
			return err
		}
	}
	s.ftsEnabled = false
	return nil
}

func isRecoverableFTSError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "messages_fts") ||
		strings.Contains(msg, "vtable constructor failed") ||
		strings.Contains(msg, "database disk image is malformed")
}
