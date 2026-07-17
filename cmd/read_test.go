package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

func TestOpenCommandReadSourceSelectsLegacyByDefault(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("OPENMESSAGES_DATA_DIR", dataDir)
	t.Setenv("OPENMESSAGES_DEMO", "0")
	t.Setenv("OPENMESSAGES_APP_SANDBOX", "1")
	t.Setenv("OPENMESSAGES_V2_PRIMARY", "0")
	t.Setenv("OPENMESSAGES_V2_SEND", "0")
	t.Setenv("OPENMESSAGES_V2_INGEST", "0")

	legacy, err := db.New(filepath.Join(dataDir, "messages.db"))
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	if err := legacy.UpsertConversation(&db.Conversation{
		ConversationID: "legacy-conversation",
		Name:           "Legacy conversation",
		SourcePlatform: "sms",
	}); err != nil {
		legacy.Close()
		t.Fatalf("UpsertConversation(): %v", err)
	}
	legacy.Close()

	var banner bytes.Buffer
	session, err := openCommandReadSource(zerolog.Nop(), &banner)
	if err != nil {
		t.Fatalf("openCommandReadSource(): %v", err)
	}
	defer session.Close()
	conversation, err := session.Reads.GetConversation("legacy-conversation")
	if err != nil {
		t.Fatalf("GetConversation(): %v", err)
	}
	if conversation == nil || conversation.Name != "Legacy conversation" {
		t.Fatalf("legacy conversation = %+v", conversation)
	}
	if session.StorePath != filepath.Join(dataDir, "messages.db") {
		t.Fatalf("StorePath = %q, want legacy path", session.StorePath)
	}
	if banner.Len() != 0 {
		t.Fatalf("legacy-primary banner = %q, want empty", banner.String())
	}
}

func TestOpenCommandReadSourceSelectsV2AndAnnouncesIt(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("OPENMESSAGES_DATA_DIR", dataDir)
	t.Setenv("OPENMESSAGES_DEMO", "0")
	t.Setenv("OPENMESSAGES_APP_SANDBOX", "1")
	t.Setenv("OPENMESSAGES_V2_PRIMARY", "1")
	t.Setenv("OPENMESSAGES_V2_SEND", "")
	t.Setenv("OPENMESSAGES_V2_INGEST", "")

	v2Dir := filepath.Join(dataDir, "v2")
	if err := os.MkdirAll(v2Dir, 0o700); err != nil {
		t.Fatalf("create v2 dir: %v", err)
	}
	storePath := filepath.Join(v2Dir, "store.sqlite3")
	store, err := sqlite.Open(storePath)
	if err != nil {
		t.Fatalf("sqlite.Open(): %v", err)
	}
	nowMS := time.Now().UnixMilli()
	if err := store.UpsertAccount(sqlite.Account{
		AccountID:   "google-primary",
		BridgeKey:   "google_messages",
		DisplayName: "Google Messages",
		Mode:        sqlite.AccountModeLive,
		Enabled:     true,
		ConfigJSON:  "{}",
		CreatedAtMS: nowMS,
		UpdatedAtMS: nowMS,
	}); err != nil {
		store.Close()
		t.Fatalf("UpsertAccount(): %v", err)
	}
	if err := store.UpsertConversation(sqlite.Conversation{
		ConversationID:       "v2-conversation",
		AccountID:            "google-primary",
		RemoteConversationID: "remote-v2-conversation",
		Kind:                 sqlite.ConversationKindDirect,
		Title:                "V2 conversation",
		NotificationMode:     sqlite.NotificationModeAll,
		MetadataJSON:         "{}",
		CreatedAtMS:          nowMS,
		UpdatedAtMS:          nowMS,
	}); err != nil {
		store.Close()
		t.Fatalf("UpsertConversation(): %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close v2 seed store: %v", err)
	}

	var banner bytes.Buffer
	session, err := openCommandReadSource(zerolog.Nop(), &banner)
	if err != nil {
		t.Fatalf("openCommandReadSource(): %v", err)
	}
	defer session.Close()
	conversation, err := session.Reads.GetConversation("v2-conversation")
	if err != nil {
		t.Fatalf("GetConversation(): %v", err)
	}
	if conversation == nil || conversation.Name != "V2 conversation" || conversation.SourcePlatform != "sms" {
		t.Fatalf("v2 conversation = %+v", conversation)
	}
	if session.StorePath != storePath {
		t.Fatalf("StorePath = %q, want %q", session.StorePath, storePath)
	}
	if banner.String() != "reading v2 store\n" {
		t.Fatalf("v2-primary banner = %q", banner.String())
	}
	if _, err := os.Stat(filepath.Join(dataDir, "messages.db")); !os.IsNotExist(err) {
		t.Fatalf("v2-primary CLI touched legacy store: %v", err)
	}
}

func TestParseDayBound(t *testing.T) {
	if ms, err := parseDayBound("", false); err != nil || ms != 0 {
		t.Errorf("empty: got %d, %v; want 0, nil", ms, err)
	}

	start, err := parseDayBound("2026-05-18", false)
	if err != nil {
		t.Fatalf("since parse: %v", err)
	}
	wantStart := time.Date(2026, 5, 18, 0, 0, 0, 0, time.Local).UnixMilli()
	if start != wantStart {
		t.Errorf("since: got %d, want %d", start, wantStart)
	}

	end, err := parseDayBound("2026-05-18", true)
	if err != nil {
		t.Fatalf("until parse: %v", err)
	}
	wantEnd := time.Date(2026, 5, 18, 0, 0, 0, 0, time.Local).
		Add(24*time.Hour - time.Millisecond).UnixMilli()
	if end != wantEnd {
		t.Errorf("until: got %d, want %d", end, wantEnd)
	}
	if end <= start {
		t.Errorf("endOfDay (%d) must be after startOfDay (%d)", end, start)
	}

	// Explicit datetime is accepted verbatim (not pushed to end of day).
	dt, err := parseDayBound("2026-05-18 14:30", true)
	if err != nil {
		t.Fatalf("datetime parse: %v", err)
	}
	wantDT := time.Date(2026, 5, 18, 14, 30, 0, 0, time.Local).UnixMilli()
	if dt != wantDT {
		t.Errorf("datetime: got %d, want %d", dt, wantDT)
	}

	if _, err := parseDayBound("not-a-date", false); err == nil {
		t.Error("expected error for invalid date")
	}
}
