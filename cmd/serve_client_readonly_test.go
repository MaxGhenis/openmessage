package cmd

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/rs/zerolog"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

const clientFixtureTimeMS int64 = 1_900_000_000_000

// seedOlderV2Store writes one account, a direct conversation with Alice, an
// incoming message with an attachment and a reaction, and an outgoing reply
// through the store's own repositories, at whatever schema the fixture was
// built to.
func seedOlderV2Store(store *sqlite.Store) error {
	now := func() time.Time { return time.UnixMilli(clientFixtureTimeMS) }
	if err := store.UpsertAccount(sqlite.Account{
		AccountID:   "google-primary",
		BridgeKey:   "google_messages",
		DisplayName: "Google Messages",
		Mode:        sqlite.AccountModeLive,
		Enabled:     true,
		ConfigJSON:  `{}`,
		CreatedAtMS: clientFixtureTimeMS,
		UpdatedAtMS: clientFixtureTimeMS,
	}); err != nil {
		return err
	}
	if err := store.UpsertIdentity(sqlite.Identity{
		IdentityID:     "identity-alice",
		AccountID:      "google-primary",
		Kind:           sqlite.IdentityKind("e164"),
		CanonicalValue: "+15550000001",
		RawValue:       "+1 555 000 0001",
		DisplayName:    "Alice Example",
		MetadataJSON:   `{}`,
		CreatedAtMS:    clientFixtureTimeMS,
		UpdatedAtMS:    clientFixtureTimeMS,
	}); err != nil {
		return err
	}
	if err := store.UpsertConversation(sqlite.Conversation{
		ConversationID:       "conversation-alice",
		AccountID:            "google-primary",
		RemoteConversationID: "remote-alice",
		Kind:                 sqlite.ConversationKindDirect,
		Title:                "Alice Example",
		NotificationMode:     sqlite.NotificationModeAll,
		LastMessageAtMS:      clientFixtureTimeMS,
		MetadataJSON:         `{}`,
		CreatedAtMS:          clientFixtureTimeMS,
		UpdatedAtMS:          clientFixtureTimeMS,
	}); err != nil {
		return err
	}
	if err := store.ReplaceConversationParticipants("conversation-alice", []sqlite.ConversationParticipant{{
		AccountID:      "google-primary",
		ConversationID: "conversation-alice",
		IdentityID:     "identity-alice",
		Role:           sqlite.ParticipantRoleMember,
		DisplayName:    "Alice Example",
		IsActive:       true,
	}}); err != nil {
		return err
	}
	messages, err := sqlite.NewMessageRepository(store, now)
	if err != nil {
		return err
	}
	sender := "identity-alice"
	size := int64(2048)
	if err := messages.ImportMessage(context.Background(), sqlite.MessageProjection{
		Message: sqlite.Message{
			MessageID:        "message-incoming",
			ConversationID:   "conversation-alice",
			AccountID:        "google-primary",
			RemoteMessageID:  "remote-incoming",
			SenderIdentityID: &sender,
			Direction:        sqlite.MessageDirectionIncoming,
			Body:             "schema ten says hello",
			State:            sqlite.MessageStateActive,
			OccurredAtMS:     clientFixtureTimeMS - 2_000,
		},
		Attachments: []sqlite.MessageAttachment{{
			Ordinal:   0,
			RemoteID:  "remote-media",
			RemoteRef: []byte("opaque"),
			Filename:  "photo.png",
			MIME:      "image/png",
			SizeBytes: &size,
		}},
	}); err != nil {
		return err
	}
	if err := messages.ImportMessage(context.Background(), sqlite.MessageProjection{
		Message: sqlite.Message{
			MessageID:       "message-outgoing",
			ConversationID:  "conversation-alice",
			AccountID:       "google-primary",
			RemoteMessageID: "remote-outgoing",
			Direction:       sqlite.MessageDirectionOutgoing,
			Body:            "schema ten reply from me",
			State:           sqlite.MessageStateActive,
			OccurredAtMS:    clientFixtureTimeMS - 1_000,
		},
	}); err != nil {
		return err
	}
	reactions, err := sqlite.NewReactionRepository(store, now)
	if err != nil {
		return err
	}
	_, err = reactions.ApplyReaction(context.Background(), sqlite.ReactionApply{
		AccountID:         "google-primary",
		ConversationID:    "conversation-alice",
		MessageID:         "message-incoming",
		ReactorKey:        "identity-alice",
		ReactorIdentityID: &sender,
		ReactorLabel:      "Alice Example",
		Emoji:             "👍",
		Action:            bridge.ReactionAdd,
		OccurredAtMS:      clientFixtureTimeMS - 1_500,
	})
	return err
}

// buildV2StoreAt provisions dataDir/v2/store.sqlite3 at schema version the
// way an app of that schema would have left it, and returns its path.
func buildV2StoreAt(t *testing.T, dataDir string, version int) string {
	t.Helper()
	v2Dir := filepath.Join(dataDir, "v2")
	if err := os.MkdirAll(v2Dir, 0o700); err != nil {
		t.Fatalf("create v2 dir: %v", err)
	}
	storePath := filepath.Join(v2Dir, "store.sqlite3")
	if err := sqlite.BuildStoreAtVersion(storePath, version, seedOlderV2Store); err != nil {
		t.Fatalf("BuildStoreAtVersion(%d): %v", version, err)
	}
	return storePath
}

func v2Fingerprint(t *testing.T, storePath string) sqlite.StoreFingerprint {
	t.Helper()
	fingerprint, err := sqlite.ReadStoreFingerprint(storePath)
	if err != nil {
		t.Fatalf("ReadStoreFingerprint(%s): %v", storePath, err)
	}
	return fingerprint
}

func assertV2StoreUnchanged(t *testing.T, label string, before, after sqlite.StoreFingerprint) {
	t.Helper()
	if reflect.DeepEqual(before, after) {
		return
	}
	if before.Dump != after.Dump {
		t.Errorf("%s: v2 logical dump changed:\nbefore:\n%s\nafter:\n%s", label, before.Dump, after.Dump)
	}
	before.Dump, after.Dump = "", ""
	t.Fatalf("%s: v2 store changed:\nbefore: %+v\nafter:  %+v", label, before, after)
}

// fakeClientDaemon is an httptest daemon that reports v2-primary for dataDir,
// serves one pending outbox row, and records every request it receives.
type fakeClientDaemon struct {
	server   *httptest.Server
	mu       sync.Mutex
	requests []string
}

func newFakeClientDaemon(t *testing.T, dataDir string) *fakeClientDaemon {
	t.Helper()
	daemon := &fakeClientDaemon{}
	daemon.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		daemon.mu.Lock()
		daemon.requests = append(daemon.requests, r.Method+" "+r.URL.Path)
		daemon.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/status":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"connected":  true,
				"v2_primary": true,
				"v2_send":    true,
				"auth":       map[string]any{"data_dir": dataDir},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/outbox":
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"outbox_id":        "outbox-from-daemon",
				"account_id":       "google-primary",
				"conversation_id":  "conversation-alice",
				"kind":             "text",
				"state":            "queued",
				"scheduled_for_ms": clientFixtureTimeMS,
				"created_at_ms":    clientFixtureTimeMS,
				"summary":          "queued text",
			}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(daemon.server.Close)
	daemonURL, err := url.Parse(daemon.server.URL)
	if err != nil {
		t.Fatalf("parse daemon URL: %v", err)
	}
	t.Setenv("OPENMESSAGES_PORT", daemonURL.Port())
	return daemon
}

func (d *fakeClientDaemon) Requests() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.requests)
}

func callClientTool(t *testing.T, server *mcpserver.MCPServer, name string, args map[string]any) string {
	t.Helper()
	registered := server.GetTool(name)
	if registered == nil {
		t.Fatalf("MCP tool %q is not registered", name)
	}
	request := mcp.CallToolRequest{}
	request.Params.Arguments = args
	result, err := registered.Handler(context.Background(), request)
	if err != nil {
		t.Fatalf("MCP %s: %v", name, err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal %s result: %v", name, err)
	}
	if result.IsError {
		t.Fatalf("MCP %s returned an error result: %s", name, encoded)
	}
	return string(encoded)
}

// TestMCPClientOpeningOlderV2DoesNotMigrate is the regression test for the
// review's P1 finding on PR #166: the transportless MCP client opened the
// daemon's v2 store with sqlite.Open, which runs migrations. A client built
// with migration 0011 started beside a schema-10 daemon therefore migrated
// the live store to 11, and the schema-10 daemon refused the store on its next
// start ("database schema version 11 is newer than supported version 10").
//
// The client must attach read-only: after a session that serves real reads
// through the MCP tools, the store's user_version, migration ledger, schema
// cookie, logical contents and main file are unchanged.
func TestMCPClientOpeningOlderV2DoesNotMigrate(t *testing.T) {
	if sqlite.LatestSchemaVersion() <= 10 {
		t.Fatalf("LatestSchemaVersion() = %d; this regression needs a build newer than schema 10", sqlite.LatestSchemaVersion())
	}
	dataDir := t.TempDir()
	setClientModeTestEnv(t, dataDir)
	storePath := buildV2StoreAt(t, dataDir, 10)
	before := v2Fingerprint(t, storePath)
	if before.UserVersion != 10 || len(before.Ledger) != 10 {
		t.Fatalf("fixture user_version=%d ledger rows=%d, want 10/10", before.UserVersion, len(before.Ledger))
	}
	v2FilesBefore := dirEntryNames(t, filepath.Dir(storePath))
	daemon := newFakeClientDaemon(t, dataDir)

	var logs syncBuffer
	server, cleanup, err := newMCPClientServer(zerolog.New(&logs), serveOptions{mcpStdio: true})
	if err != nil {
		t.Fatalf("newMCPClientServer beside a schema-10 store: %v\n%s", err, logs.String())
	}
	for _, want := range []string{`"v2_primary_reads":true`, `"store_schema_version":10`, fmt.Sprintf(`"build_schema_version":%d`, sqlite.LatestSchemaVersion())} {
		if !strings.Contains(logs.String(), want) {
			cleanup()
			t.Fatalf("client log lacks %s:\n%s", want, logs.String())
		}
	}

	checks := []struct {
		tool string
		args map[string]any
		want []string
	}{
		{tool: "list_conversations", args: map[string]any{"limit": float64(10)}, want: []string{"conversation-alice", "Alice Example"}},
		{tool: "get_conversation", args: map[string]any{"conversation_id": "conversation-alice"}, want: []string{"schema ten says hello", "schema ten reply from me"}},
		{tool: "get_messages", args: map[string]any{"limit": float64(10)}, want: []string{"schema ten says hello"}},
		{tool: "search_messages", args: map[string]any{"query": "schema ten"}, want: []string{"schema ten says hello", "schema ten reply from me"}},
		{tool: "get_person_messages", args: map[string]any{"name": "Alice"}, want: []string{"schema ten says hello"}},
		{tool: "list_outbox", args: map[string]any{}, want: []string{"outbox-from-daemon"}},
	}
	for _, check := range checks {
		result := callClientTool(t, server, check.tool, check.args)
		for _, want := range check.want {
			if !strings.Contains(result, want) {
				cleanup()
				t.Fatalf("MCP %s result lacks %q: %s", check.tool, want, result)
			}
		}
	}
	cleanup()

	after := v2Fingerprint(t, storePath)
	assertV2StoreUnchanged(t, "schema-10 v2 store after an MCP client session", before, after)
	if after.UserVersion != 10 || len(after.Ledger) != 10 {
		t.Fatalf("after the client session user_version=%d ledger rows=%d, want 10/10", after.UserVersion, len(after.Ledger))
	}
	for _, name := range dirEntryNames(t, filepath.Dir(storePath)) {
		if !slices.Contains(v2FilesBefore, name) && name != "store.sqlite3-wal" && name != "store.sqlite3-shm" {
			t.Fatalf("client session created %q in the v2 dir", name)
		}
	}
	for _, request := range daemon.Requests() {
		if request != "GET /api/status" && request != "GET /api/v1/outbox" {
			t.Fatalf("client sent an unexpected daemon request %q (all: %v)", request, daemon.Requests())
		}
	}

	// Contrast: the migrating open does move this fixture to the build's
	// schema, so the assertions above are not vacuous.
	copyPath := filepath.Join(t.TempDir(), "store.sqlite3")
	copyTestFile(t, storePath, copyPath)
	migrated, err := sqlite.Open(copyPath)
	if err != nil {
		t.Fatalf("sqlite.Open(copy): %v", err)
	}
	_ = migrated.Close()
	if got := v2Fingerprint(t, copyPath).UserVersion; got != sqlite.LatestSchemaVersion() {
		t.Fatalf("sqlite.Open left the copy at user_version %d, want %d", got, sqlite.LatestSchemaVersion())
	}
}

// TestRunServeMCPClientRefusesNewerV2Store covers the other direction of
// version skew: a client binary older than the store (the app updated, the
// MCP host still runs an old openmessage) must refuse to start, name the
// binary to update, and leave the store alone.
func TestRunServeMCPClientRefusesNewerV2Store(t *testing.T) {
	dataDir := t.TempDir()
	setClientModeTestEnv(t, dataDir)
	storePath := buildV2StoreAt(t, dataDir, sqlite.LatestSchemaVersion())
	future := sqlite.LatestSchemaVersion() + 1
	appendFutureV2LedgerRow(t, storePath, future)
	before := v2Fingerprint(t, storePath)
	newFakeClientDaemon(t, dataDir)

	var logs syncBuffer
	server, cleanup, err := newMCPClientServer(zerolog.New(&logs), serveOptions{mcpStdio: true})
	if err == nil {
		cleanup()
		t.Fatalf("newMCPClientServer served a schema-%d store with a schema-%d build (server %v)", future, sqlite.LatestSchemaVersion(), server != nil)
	}
	if !errors.Is(err, sqlite.ErrSchemaNewer) {
		t.Fatalf("startup error = %v, want sqlite.ErrSchemaNewer", err)
	}
	for _, want := range []string{
		storePath,
		fmt.Sprintf("database schema version %d is newer than supported version %d", future, sqlite.LatestSchemaVersion()),
		"update this openmessage binary",
		"this binary:",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("startup error %q does not contain %q", err, want)
		}
	}
	assertV2StoreUnchanged(t, "newer v2 store after a refused client start", before, v2Fingerprint(t, storePath))
}

// TestOpenCommandReadSourceV2DoesNotMigrate: openmessage read and status
// share the MCP client's hazard (cmd/read.go opened the v2 store with
// sqlite.Open) and its fix.
func TestOpenCommandReadSourceV2DoesNotMigrate(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("OPENMESSAGES_DATA_DIR", dataDir)
	t.Setenv("OPENMESSAGES_DEMO", "0")
	t.Setenv("OPENMESSAGES_APP_SANDBOX", "1")
	t.Setenv("OPENMESSAGES_V2_PRIMARY", "1")
	t.Setenv("OPENMESSAGES_V2_SEND", "")
	t.Setenv("OPENMESSAGES_V2_INGEST", "")
	storePath := buildV2StoreAt(t, dataDir, 10)
	before := v2Fingerprint(t, storePath)

	session, err := openCommandReadSource(zerolog.Nop(), io.Discard)
	if err != nil {
		t.Fatalf("openCommandReadSource(schema 10): %v", err)
	}
	conversation, err := session.Reads.GetConversation("conversation-alice")
	if err != nil || conversation == nil || conversation.Name != "Alice Example" {
		session.Close()
		t.Fatalf("GetConversation() = %+v, %v", conversation, err)
	}
	messages, err := session.Reads.GetMessagesByConversation("conversation-alice", 10)
	if err != nil || len(messages) != 2 {
		session.Close()
		t.Fatalf("GetMessagesByConversation() = %d messages, %v; want 2", len(messages), err)
	}
	session.Close()
	assertV2StoreUnchanged(t, "schema-10 v2 store after openmessage read", before, v2Fingerprint(t, storePath))

	// The newer-store refusal reaches the CLI too.
	newerDir := t.TempDir()
	t.Setenv("OPENMESSAGES_DATA_DIR", newerDir)
	newerPath := buildV2StoreAt(t, newerDir, sqlite.LatestSchemaVersion())
	appendFutureV2LedgerRow(t, newerPath, sqlite.LatestSchemaVersion()+1)
	if _, err := openCommandReadSource(zerolog.Nop(), io.Discard); !errors.Is(err, sqlite.ErrSchemaNewer) {
		t.Fatalf("openCommandReadSource(newer store) error = %v, want sqlite.ErrSchemaNewer", err)
	}
}

// TestMCPClientStartsWhileDaemonHoldsWriteLock pins the second half of the
// hazard: sqlite.Open always takes SQLite's write reservation (BEGIN
// IMMEDIATE) even when no migration is pending, so a daemon write burst
// longer than the 5 s busy timeout used to fail MCP client startup. The
// read-only attach never asks for that lock.
func TestMCPClientStartsWhileDaemonHoldsWriteLock(t *testing.T) {
	dataDir := t.TempDir()
	setClientModeTestEnv(t, dataDir)
	storePath := buildV2StoreAt(t, dataDir, sqlite.LatestSchemaVersion())
	newFakeClientDaemon(t, dataDir)

	daemonDB, err := sql.Open("sqlite", storePath)
	if err != nil {
		t.Fatalf("open daemon connection: %v", err)
	}
	t.Cleanup(func() { _ = daemonDB.Close() })
	daemonConn, err := daemonDB.Conn(context.Background())
	if err != nil {
		t.Fatalf("daemon Conn(): %v", err)
	}
	t.Cleanup(func() { _ = daemonConn.Close() })
	if _, err := daemonConn.ExecContext(context.Background(), `BEGIN IMMEDIATE`); err != nil {
		t.Fatalf("daemon BEGIN IMMEDIATE: %v", err)
	}
	defer func() { _, _ = daemonConn.ExecContext(context.Background(), `ROLLBACK`) }()

	started := time.Now()
	var logs syncBuffer
	server, cleanup, err := newMCPClientServer(zerolog.New(&logs), serveOptions{mcpStdio: true})
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("newMCPClientServer while the daemon holds the v2 write lock: %v (after %v)\n%s", err, elapsed, logs.String())
	}
	defer cleanup()
	// The busy timeout is 5 s; 2 s leaves headroom for a loaded -race run
	// while still proving startup did not wait for the lock.
	if elapsed > 2*time.Second {
		t.Fatalf("client startup took %v while the daemon held the write lock; it must not wait for it", elapsed)
	}
	if result := callClientTool(t, server, "list_conversations", map[string]any{}); !strings.Contains(result, "conversation-alice") {
		t.Fatalf("list_conversations under the daemon's write lock = %s", result)
	}
}

func appendFutureV2LedgerRow(t *testing.T, storePath string, version int) {
	t.Helper()
	database, err := sql.Open("sqlite", storePath)
	if err != nil {
		t.Fatalf("open %s: %v", storePath, err)
	}
	defer database.Close()
	if _, err := database.Exec(`
		INSERT INTO schema_migrations (version, name, checksum_sha256, applied_at_ms, app_version, execution_ms)
		VALUES (?, ?, ?, 1, 'future-build', 0)
	`, version, fmt.Sprintf("future_%04d", version), strings.Repeat("f", 64)); err != nil {
		t.Fatalf("append ledger row %d: %v", version, err)
	}
	if _, err := database.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, version)); err != nil {
		t.Fatalf("set user_version %d: %v", version, err)
	}
}

func dirEntryNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func copyTestFile(t *testing.T, from, to string) {
	t.Helper()
	content, err := os.ReadFile(from)
	if err != nil {
		t.Fatalf("read %s: %v", from, err)
	}
	if err := os.WriteFile(to, content, 0o600); err != nil {
		t.Fatalf("write %s: %v", to, err)
	}
}
