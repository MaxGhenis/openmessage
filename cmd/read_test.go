package cmd

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
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

// TestOpenCommandReadSourceLegacyDoesNotRepairStore pins the read-only
// contract of the one-shot CLI reader behind `read` and `status`: opening the
// legacy store for reads must not run the startup repair sweeps against the
// live store it shares with the running app. (The app.New contrast leg of
// TestRunServeMCPClientDoesNotRepairStore proves the same seeded row does
// trigger a sweep under a store-owning open, so this cannot pass vacuously.)
func TestOpenCommandReadSourceLegacyDoesNotRepairStore(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("OPENMESSAGES_DATA_DIR", dataDir)
	t.Setenv("OPENMESSAGES_DEMO", "0")
	t.Setenv("OPENMESSAGES_APP_SANDBOX", "1")
	t.Setenv("OPENMESSAGES_V2_PRIMARY", "0")
	t.Setenv("OPENMESSAGES_V2_SEND", "0")
	t.Setenv("OPENMESSAGES_V2_INGEST", "0")

	seedLegacyReactionPlaceholder(t, dataDir)

	var banner bytes.Buffer
	session, err := openCommandReadSource(zerolog.Nop(), &banner)
	if err != nil {
		t.Fatalf("openCommandReadSource(): %v", err)
	}
	conversation, err := session.Reads.GetConversation("whatsapp:group@g.us")
	if err != nil {
		session.Close()
		t.Fatalf("GetConversation(): %v", err)
	}
	if conversation == nil {
		session.Close()
		t.Fatal("seeded conversation not readable through the session")
	}
	session.Close()

	if !legacyReactionPlaceholderPresent(t, dataDir) {
		t.Fatal("openCommandReadSource ran the startup repair sweeps: the legacy reaction placeholder was repaired away")
	}
}

// TestOpenCommandReadSourceNeverMigratesTheV2Store pins the other half of the
// read-only contract: `read` and `status` from a binary newer than the app's
// store refuse it instead of migrating it. A migration holds SQLite's write
// lock for its whole run, so applying one under the running app would stall
// (and, past busy_timeout, fail) the daemon's inbox appends.
func TestOpenCommandReadSourceNeverMigratesTheV2Store(t *testing.T) {
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
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	latest := rollBackLedgerOneVersion(t, storePath)

	session, err := openCommandReadSource(zerolog.Nop(), io.Discard)
	if err == nil {
		session.Close()
		t.Fatal("openCommandReadSource() opened a store with a pending migration; want a refusal")
	}
	if !errors.Is(err, sqlite.ErrMigrationPending) || !strings.Contains(err.Error(), "reopen the OpenMessage app") {
		t.Fatalf("openCommandReadSource() error = %v, want ErrMigrationPending telling the user to reopen the app", err)
	}
	if got := ledgerVersionCount(t, storePath); got != latest-1 {
		t.Fatalf("ledger rows = %d after the refused read, want %d: the reader applied a migration", got, latest-1)
	}
}

// rollBackLedgerOneVersion removes the newest ledger row and lowers
// user_version to match: the shape of a store the running app has not
// upgraded to this build's schema yet. It returns the version it removed.
func rollBackLedgerOneVersion(t *testing.T, storePath string) int {
	t.Helper()
	latest := ledgerVersionCount(t, storePath)
	database, err := sql.Open("sqlite", storePath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := database.Exec(`DELETE FROM schema_migrations WHERE version = ?`, latest); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, latest-1)); err != nil {
		t.Fatal(err)
	}
	return latest
}

func ledgerVersionCount(t *testing.T, storePath string) int {
	t.Helper()
	database, err := sql.Open("sqlite", storePath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	var rows int
	if err := database.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	return rows
}
