package cmd

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/maxghenis/openmessage/internal/app"
	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/localapi"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
	"github.com/maxghenis/openmessage/internal/web"
)

// TestServeOptionsTransportMatrix pins which serve shapes may own live
// platform connections. The MCP-stdio-only shape is the per-Claude-session
// spawn and must default to transportless client mode.
func TestServeOptionsTransportMatrix(t *testing.T) {
	tests := []struct {
		name           string
		args           []string
		wantClient     bool
		wantTransports bool
	}{
		{name: "default web daemon", args: nil, wantClient: false, wantTransports: true},
		{name: "mcp-stdio only is the client shape", args: []string{"--mcp-stdio"}, wantClient: true, wantTransports: false},
		{name: "mcp-stdio can opt back into transports", args: []string{"--mcp-stdio", "--transports"}, wantClient: true, wantTransports: true},
		{name: "web daemon can drop transports", args: []string{"--web", "--no-transports"}, wantClient: false, wantTransports: false},
		{name: "web plus stdio is a daemon", args: []string{"--web", "--mcp-stdio"}, wantClient: false, wantTransports: true},
		{name: "sse only is a daemon", args: []string{"--mcp-sse"}, wantClient: false, wantTransports: true},
		{name: "sse plus stdio is a daemon", args: []string{"--mcp-sse", "--mcp-stdio"}, wantClient: false, wantTransports: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts, err := parseServeOptions(tt.args)
			if err != nil {
				t.Fatalf("parseServeOptions(%v): %v", tt.args, err)
			}
			if got := opts.mcpClientShape(); got != tt.wantClient {
				t.Fatalf("mcpClientShape() = %v, want %v", got, tt.wantClient)
			}
			if got := opts.transportsEnabled(); got != tt.wantTransports {
				t.Fatalf("transportsEnabled() = %v, want %v", got, tt.wantTransports)
			}
		})
	}
}

func setClientModeTestEnv(t *testing.T, dataDir string) {
	t.Helper()
	t.Setenv("OPENMESSAGES_DATA_DIR", dataDir)
	t.Setenv("OPENMESSAGES_DEMO", "0")
	t.Setenv("OPENMESSAGES_V2_PRIMARY", "")
	t.Setenv("OPENMESSAGES_V2_SEND", "")
	t.Setenv("OPENMESSAGES_V2_INGEST", "")
	t.Setenv("OPENMESSAGES_APP_SANDBOX", "1")
	t.Setenv("OPENMESSAGES_MACOS_NOTIFICATIONS", "0")
	t.Setenv("OPENMESSAGE_TELEMETRY", "0")
	// Hermetic daemon probe: never reach a real daemon on the test machine.
	t.Setenv("OPENMESSAGES_PORT", "0")
}

// TestRunServeMCPStdioStartsZeroTransportSupervisors is the regression test
// for the MCP fratricide bug: `serve --mcp-stdio` (the per-Claude-session
// spawn) used to start the full Google/WhatsApp/Signal transport stack with
// the same credentials as the running app daemon, which WhatsApp treats as a
// second device login ("401: logged out from another device") and which
// corrupts signal-cli state. The client shape must start zero transport
// supervisors and leave zero transport state on disk; run-in stdin EOF makes
// RunServe return once the MCP stdio session ends.
func TestRunServeMCPStdioStartsZeroTransportSupervisors(t *testing.T) {
	dataDir := t.TempDir()
	setClientModeTestEnv(t, dataDir)

	var logs bytes.Buffer
	if err := RunServe(zerolog.New(&logs), "--mcp-stdio"); err != nil {
		t.Fatalf("RunServe(--mcp-stdio): %v\n%s", err, logs.String())
	}

	logOutput := logs.String()
	if !strings.Contains(logOutput, "MCP client mode") {
		t.Fatalf("client mode was not engaged:\n%s", logOutput)
	}
	if !strings.Contains(logOutput, "Starting MCP stdio transport") {
		t.Fatalf("MCP stdio transport did not start:\n%s", logOutput)
	}

	// No transport supervisor state may exist: WhatsApp's whatsmeow store,
	// Signal's signal-cli config, and the v2 dispatcher store are all
	// daemon-owned.
	for _, forbidden := range []string{"whatsapp-session.db", "signal-cli", "v2"} {
		if _, err := os.Stat(filepath.Join(dataDir, forbidden)); !os.IsNotExist(err) {
			t.Errorf("client mode created transport state %q (stat err = %v)", forbidden, err)
		}
	}
	// Reads still work: the legacy store opens.
	if _, err := os.Stat(filepath.Join(dataDir, "messages.db")); err != nil {
		t.Fatalf("client mode did not open the local read store: %v", err)
	}

	// Contrast case: the same stdio shape with --transports is a standalone
	// daemon and does initialize transport state. This guards the assertion
	// above against silently passing because the fixtures changed.
	daemonDataDir := t.TempDir()
	setClientModeTestEnv(t, daemonDataDir)
	var daemonLogs bytes.Buffer
	if err := RunServe(zerolog.New(&daemonLogs), "--mcp-stdio", "--transports"); err != nil {
		t.Fatalf("RunServe(--mcp-stdio --transports): %v\n%s", err, daemonLogs.String())
	}
	if _, err := os.Stat(filepath.Join(daemonDataDir, "whatsapp-session.db")); err != nil {
		t.Fatalf("--transports did not initialize the WhatsApp bridge store (assertion above may be vacuous): %v", err)
	}
}

// TestRunServeMCPClientAdoptsDaemonTruth verifies the daemon-truth read mode:
// when the running app reports v2-primary for the same data directory, the
// client serves v2 reads without any OPENMESSAGES_V2_* environment and
// without running its own dispatcher.
func TestRunServeMCPClientAdoptsDaemonTruth(t *testing.T) {
	dataDir := t.TempDir()

	// Provision a migrated v2 store the way the daemon would have.
	v2Dir := filepath.Join(dataDir, "v2")
	if err := os.MkdirAll(v2Dir, 0o700); err != nil {
		t.Fatalf("create v2 dir: %v", err)
	}
	storePath := filepath.Join(v2Dir, "store.sqlite3")
	store, err := sqlite.Open(storePath)
	if err != nil {
		t.Fatalf("provision v2 store: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close provisioned v2 store: %v", err)
	}

	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/status" {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"connected":  true,
			"v2_primary": true,
			"v2_send":    true,
			"auth":       map[string]any{"data_dir": dataDir},
		})
	}))
	defer daemon.Close()
	daemonURL, err := url.Parse(daemon.URL)
	if err != nil {
		t.Fatalf("parse daemon URL: %v", err)
	}

	setClientModeTestEnv(t, dataDir)
	t.Setenv("OPENMESSAGES_PORT", daemonURL.Port())

	var logs bytes.Buffer
	if err := RunServe(zerolog.New(&logs), "--mcp-stdio"); err != nil {
		t.Fatalf("RunServe(--mcp-stdio): %v\n%s", err, logs.String())
	}
	logOutput := logs.String()
	if !strings.Contains(logOutput, `"daemon_running":true`) {
		t.Fatalf("daemon probe did not succeed:\n%s", logOutput)
	}
	if !strings.Contains(logOutput, `"v2_primary_reads":true`) {
		t.Fatalf("daemon-truth v2-primary read mode was not adopted:\n%s", logOutput)
	}
	// Client mode attaches to the daemon's stores; it must not provision the
	// blob store or any other dispatcher-side state.
	if _, err := os.Stat(filepath.Join(v2Dir, "blobs")); !os.IsNotExist(err) {
		t.Fatalf("client mode provisioned v2 blob state: %v", err)
	}
}

// TestRunServeMCPClientAdoptsDaemonDataDir verifies that with no
// OPENMESSAGES_DATA_DIR pinned, the client adopts the data directory the
// running daemon reports, so an env-less MCP spawn serves the app's store
// instead of the stale CLI-default directory.
func TestRunServeMCPClientAdoptsDaemonDataDir(t *testing.T) {
	daemonDataDir := t.TempDir()

	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"connected": true,
			"auth":      map[string]any{"data_dir": daemonDataDir},
		})
	}))
	defer daemon.Close()
	daemonURL, err := url.Parse(daemon.URL)
	if err != nil {
		t.Fatalf("parse daemon URL: %v", err)
	}

	// setClientModeTestEnv pins OPENMESSAGES_DATA_DIR (t.Setenv registers the
	// restore); the unset afterwards is what this test is about. If adoption
	// regresses, app.New would fall through to the real CLI-default dir, so
	// fail fast on the log assertion rather than writing there.
	setClientModeTestEnv(t, t.TempDir())
	t.Setenv("OPENMESSAGES_PORT", daemonURL.Port())
	if err := os.Unsetenv("OPENMESSAGES_DATA_DIR"); err != nil {
		t.Fatalf("unset data dir: %v", err)
	}

	var logs bytes.Buffer
	if err := RunServe(zerolog.New(&logs), "--mcp-stdio"); err != nil {
		t.Fatalf("RunServe(--mcp-stdio): %v\n%s", err, logs.String())
	}
	logOutput := logs.String()
	if !strings.Contains(logOutput, "adopted the running app's data directory") {
		t.Fatalf("data dir adoption did not happen:\n%s", logOutput)
	}
	if !strings.Contains(logOutput, daemonDataDir) {
		t.Fatalf("adopted dir was not the daemon's:\n%s", logOutput)
	}
	// The adopted directory now holds the client's read store.
	if _, err := os.Stat(filepath.Join(daemonDataDir, "messages.db")); err != nil {
		t.Fatalf("client did not open the store in the adopted dir: %v", err)
	}
}

// seedLegacyReactionPlaceholder writes the canonical RepairLegacyArtifacts
// trigger — a WhatsApp "[Reaction]" placeholder row — into the data dir's
// legacy store, then closes it. Store-owning startup (app.New) deletes the
// row; repair-free client opens (app.NewClient) must leave it in place.
func seedLegacyReactionPlaceholder(t *testing.T, dataDir string) {
	t.Helper()
	store, err := db.New(filepath.Join(dataDir, "messages.db"))
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	defer store.Close()
	if err := store.UpsertConversation(&db.Conversation{
		ConversationID: "whatsapp:group@g.us",
		Name:           "Group",
		IsGroup:        true,
		LastMessageTS:  3000,
		SourcePlatform: "whatsapp",
	}); err != nil {
		t.Fatalf("UpsertConversation(): %v", err)
	}
	if err := store.UpsertMessage(&db.Message{
		MessageID:      "whatsapp:reaction-placeholder",
		ConversationID: "whatsapp:group@g.us",
		Body:           "[Reaction]",
		TimestampMS:    3000,
		SourcePlatform: "whatsapp",
		SourceID:       "reaction-placeholder",
	}); err != nil {
		t.Fatalf("UpsertMessage(): %v", err)
	}
}

func legacyReactionPlaceholderPresent(t *testing.T, dataDir string) bool {
	t.Helper()
	store, err := db.New(filepath.Join(dataDir, "messages.db"))
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	defer store.Close()
	m, err := store.GetMessageByID("whatsapp:reaction-placeholder")
	if err != nil {
		t.Fatalf("GetMessageByID(): %v", err)
	}
	return m != nil
}

// TestRunServeMCPClientDoesNotRepairStore pins the repair-free client open:
// MCP hosts spawn one `serve --mcp-stdio` process per session, and the daemon
// repairs the shared live store on its own startup — so the per-session
// client shape must not replay the startup repair sweeps against it. A
// store-owning open (app.New) run afterwards must repair the same row,
// proving the seed genuinely triggers a sweep (the client assertion cannot
// pass vacuously).
func TestRunServeMCPClientDoesNotRepairStore(t *testing.T) {
	dataDir := t.TempDir()
	setClientModeTestEnv(t, dataDir)
	seedLegacyReactionPlaceholder(t, dataDir)

	var logs bytes.Buffer
	if err := RunServe(zerolog.New(&logs), "--mcp-stdio"); err != nil {
		t.Fatalf("RunServe(--mcp-stdio): %v\n%s", err, logs.String())
	}
	if !strings.Contains(logs.String(), "MCP client mode") {
		t.Fatalf("client mode was not engaged:\n%s", logs.String())
	}
	if !legacyReactionPlaceholderPresent(t, dataDir) {
		t.Fatal("MCP client mode ran the startup repair sweeps: the legacy reaction placeholder was repaired away")
	}

	// Contrast: app.New — the store-owning open the daemon runs at startup —
	// must repair the row. Called directly rather than through a full daemon
	// RunServe, whose transport stack keeps background goroutines holding the
	// store briefly past return and races this verification open.
	daemonApp, err := app.New(zerolog.Nop())
	if err != nil {
		t.Fatalf("app.New(): %v", err)
	}
	daemonApp.Close()
	if legacyReactionPlaceholderPresent(t, dataDir) {
		t.Fatal("app.New left the legacy reaction placeholder unrepaired (the client assertion above may be vacuous)")
	}
}

// TestControlTokenFileConstantsAgree pins the localapi copy of the control
// token filename to the web package's authoritative constant.
func TestControlTokenFileConstantsAgree(t *testing.T) {
	if localapi.ControlTokenFile != web.ControlTokenFile {
		t.Fatalf("localapi.ControlTokenFile = %q, want %q", localapi.ControlTokenFile, web.ControlTokenFile)
	}
}
