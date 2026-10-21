package tools

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/localapi"
	"github.com/maxghenis/openmessage/internal/media"
	"github.com/maxghenis/openmessage/internal/messaging"
	"github.com/maxghenis/openmessage/internal/storage/blob"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

const v2MediaMessageID = "2b1f7e0c9d8a4f6b8e3c1a0d5f7b9c2e"

// fakeMediaDaemon answers the v1 attachment route the way a running app
// does: bytes with the digest header, or the app's JSON error.
type fakeMediaDaemon struct {
	mu       sync.Mutex
	paths    []string
	status   int
	errorMsg string
	body     []byte
	digest   string
}

func (d *fakeMediaDaemon) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	d.paths = append(d.paths, r.URL.EscapedPath())
	status, errorMsg, body, digest := d.status, d.errorMsg, d.body, d.digest
	d.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer test-token" {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	if status != 0 && status != http.StatusOK {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprintf(w, "{\"error\":%q}\n", errorMsg)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="Report.html"`)
	if digest == "" {
		sum := sha256.Sum256(body)
		digest = hex.EncodeToString(sum[:])
	}
	w.Header().Set("X-Content-SHA256", digest)
	_, _ = w.Write(body)
}

func (d *fakeMediaDaemon) requestedPaths() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.paths...)
}

func clientModeOptions(t *testing.T, daemon *fakeMediaDaemon) Options {
	t.Helper()
	server := httptest.NewServer(daemon)
	t.Cleanup(server.Close)
	return Options{V2Primary: true, Daemon: localapi.NewClient(server.URL, "test-token")}
}

func callDownloadMedia(t *testing.T, options Options, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	request := mcp.CallToolRequest{}
	request.Params.Arguments = args
	result, err := downloadMediaHandler(testApp(t), options)(context.Background(), request)
	if err != nil {
		t.Fatalf("downloadMediaHandler(): %v", err)
	}
	return result
}

// Before the fix this tool looked every id up in the legacy store, which
// froze at the v2 cutover, and answered "message not found" for both the v2
// message ID and the media_id get_messages reported for it.
func TestDownloadMediaResolvesV2MessageIDAndMediaIDThroughTheApp(t *testing.T) {
	exportRoot := t.TempDir()
	t.Setenv("OPENMESSAGES_EXPORT_DIR", exportRoot)
	content := []byte("<html>attachment bytes</html>")
	daemon := &fakeMediaDaemon{body: content}
	options := clientModeOptions(t, daemon)

	for _, tc := range []struct {
		arg      string
		wantPath string
		ordinal  int64
	}{
		{v2MediaMessageID, "/api/v1/messages/" + v2MediaMessageID + "/attachments/0", 0},
		{"v2msg:" + v2MediaMessageID + ":0", "/api/v1/messages/" + v2MediaMessageID + "/attachments/0", 0},
		{"v2msg:" + v2MediaMessageID + ":2", "/api/v1/messages/" + v2MediaMessageID + "/attachments/2", 2},
	} {
		result := callDownloadMedia(t, options, map[string]any{"message_id": tc.arg})
		if result.IsError {
			t.Fatalf("download_media(%q) error: %s", tc.arg, resultText(t, result))
		}
		payload := structuredMap(t, result)
		sum := sha256.Sum256(content)
		if payload["message_id"] != v2MediaMessageID || fmt.Sprint(payload["ordinal"]) != fmt.Sprint(tc.ordinal) ||
			payload["filename"] != "Report.html" || fmt.Sprint(payload["size_bytes"]) != fmt.Sprint(len(content)) ||
			payload["sha256"] != hex.EncodeToString(sum[:]) || payload["url"] != tc.wantPath {
			t.Fatalf("download_media(%q) = %#v", tc.arg, payload)
		}
		if _, saved := payload["path"]; saved {
			t.Fatalf("download_media(%q) saved a file without save=true: %#v", tc.arg, payload)
		}
		paths := daemon.requestedPaths()
		if got := paths[len(paths)-1]; got != tc.wantPath {
			t.Fatalf("download_media(%q) asked the app for %q, want %q", tc.arg, got, tc.wantPath)
		}
	}
	if entries, _ := os.ReadDir(exportRoot); len(entries) != 0 {
		t.Fatalf("export root holds %d entries without save=true", len(entries))
	}
}

func TestDownloadMediaSaveWritesAPrivateFileUnderTheExportRoot(t *testing.T) {
	exportRoot := t.TempDir()
	t.Setenv("OPENMESSAGES_EXPORT_DIR", exportRoot)
	content := []byte("<html>saved</html>")
	options := clientModeOptions(t, &fakeMediaDaemon{body: content})

	result := callDownloadMedia(t, options, map[string]any{"message_id": "v2msg:" + v2MediaMessageID + ":0", "save": true})
	if result.IsError {
		t.Fatalf("download_media(save) error: %s", resultText(t, result))
	}
	path, _ := structuredMap(t, result)["path"].(string)
	want := filepath.Join(exportRoot, "media", v2MediaMessageID+"-0-Report.html")
	if resolved, _ := filepath.EvalSymlinks(filepath.Dir(path)); path == "" || resolved != mustEvalSymlinks(t, filepath.Dir(want)) || filepath.Base(path) != filepath.Base(want) {
		t.Fatalf("saved path = %q, want %q", path, want)
	}
	assertPrivateFile(t, path, content)

	custom := callDownloadMedia(t, options, map[string]any{"message_id": v2MediaMessageID, "output_path": "andrew/spec.html"})
	if custom.IsError {
		t.Fatalf("download_media(output_path) error: %s", resultText(t, custom))
	}
	assertPrivateFile(t, structuredMap(t, custom)["path"].(string), content)

	outside := callDownloadMedia(t, options, map[string]any{"message_id": v2MediaMessageID, "output_path": filepath.Join(t.TempDir(), "x.html")})
	if !outside.IsError || !strings.Contains(resultText(t, outside), "outside export root") {
		t.Fatalf("download_media(outside export root) = %s, want a refusal", resultText(t, outside))
	}
}

func mustEvalSymlinks(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", path, err)
	}
	return resolved
}

func assertPrivateFile(t *testing.T, path string, want []byte) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat saved attachment: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("saved attachment mode = %v, want 0600", info.Mode().Perm())
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("saved attachment = %q, %v; want %q", got, err, want)
	}
}

func TestDownloadMediaReportsTheAppsReason(t *testing.T) {
	for name, tc := range map[string]struct {
		daemon *fakeMediaDaemon
		want   string
	}{
		"gone": {&fakeMediaDaemon{status: http.StatusNotFound, errorMsg: "media: attachment is no longer available from its transport: signal-cli did not store attachment x.html"},
			"attachment unavailable: media: attachment is no longer available from its transport: signal-cli did not store attachment x.html"},
		"unknown":     {&fakeMediaDaemon{status: http.StatusNotFound, errorMsg: "not found"}, "message or attachment not found"},
		"unreachable": {&fakeMediaDaemon{status: http.StatusServiceUnavailable, errorMsg: "attachment download unavailable: Signal is not connected"}, "attachment not downloadable right now (retry later): attachment download unavailable: Signal is not connected"},
		"bad digest":  {&fakeMediaDaemon{body: []byte("bytes"), digest: strings.Repeat("0", 64)}, "but reported " + strings.Repeat("0", 64)},
	} {
		result := callDownloadMedia(t, clientModeOptions(t, tc.daemon), map[string]any{"message_id": v2MediaMessageID, "save": true})
		if !result.IsError || !strings.Contains(resultText(t, result), tc.want) {
			t.Errorf("download_media(%s) = %s, want an error containing %q", name, resultText(t, result), tc.want)
		}
	}
	downResult := callDownloadMedia(t, Options{V2Primary: true, Daemon: localapi.NewClient("http://127.0.0.1:1", "test-token")}, map[string]any{"message_id": v2MediaMessageID})
	if !downResult.IsError || !strings.Contains(resultText(t, downResult), "isn't running") {
		t.Fatalf("download_media(app down) = %s, want the app-down explanation", resultText(t, downResult))
	}
}

type toolDownloadRegistry struct{ download bridge.MediaDownloader }

func (r toolDownloadRegistry) Snapshot(accountID string) (bridge.Snapshot, bool) {
	return bridge.Snapshot{AccountID: accountID, Platform: bridge.PlatformSignal, Generation: 1}, accountID == "signal-primary"
}

func (r toolDownloadRegistry) Acquire(context.Context, string, bridge.Capability) (*bridge.DispatchLease, error) {
	return &bridge.DispatchLease{Download: r.download}, nil
}

func (r toolDownloadRegistry) Capabilities(string) bridge.CapabilitySet {
	return bridge.CapabilitySet{MediaDownload: true}
}

type toolGoneDownloader struct{}

func (toolGoneDownloader) DownloadMedia(context.Context, string, bridge.MediaRef) (bridge.MediaStream, error) {
	return bridge.MediaStream{}, fmt.Errorf("%w: signal-cli did not store attachment x.html", bridge.ErrMediaGone)
}

// In a process that owns the v2 store (the app's own MCP endpoint), the tool
// resolves through the media service directly.
func TestDownloadMediaResolvesV2InProcess(t *testing.T) {
	t.Setenv("OPENMESSAGES_EXPORT_DIR", t.TempDir())
	ctx := context.Background()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "v2.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	nowMS := time.Now().UnixMilli()
	if err := store.UpsertAccount(sqlite.Account{
		AccountID: "signal-primary", BridgeKey: "signal_cli", DisplayName: "Signal", Mode: sqlite.AccountModeLive,
		Enabled: true, ConfigJSON: "{}", CreatedAtMS: nowMS, UpdatedAtMS: nowMS,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertConversation(sqlite.Conversation{
		ConversationID: "conversation-1", AccountID: "signal-primary", RemoteConversationID: "signal:+15551234567",
		Kind: sqlite.ConversationKindDirect, Title: "Sam", NotificationMode: sqlite.NotificationModeAll,
		MetadataJSON: "{}", CreatedAtMS: nowMS, UpdatedAtMS: nowMS,
	}); err != nil {
		t.Fatal(err)
	}
	messages, err := sqlite.NewMessageRepository(store, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"message-gone", "message-imported"} {
		if err := messages.ImportMessage(ctx, sqlite.MessageProjection{
			Message: sqlite.Message{
				MessageID: id, ConversationID: "conversation-1", AccountID: "signal-primary",
				RemoteMessageID: "remote-" + id, Direction: sqlite.MessageDirectionIncoming,
				State: sqlite.MessageStateActive, OccurredAtMS: nowMS,
			},
			Attachments: []sqlite.MessageAttachment{{Ordinal: 0, RemoteID: "x.html", Filename: "Report.html", MIME: "text/html"}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	blobs, err := blob.New(filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	service, err := media.NewService(store, toolDownloadRegistry{download: toolGoneDownloader{}}, blobs, messaging.SystemClock{})
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("<html>imported</html>")
	if _, _, err := service.Import(ctx, media.ImportCommand{AccountID: "signal-primary", MessageID: "message-imported", Content: bytes.NewReader(content)}); err != nil {
		t.Fatalf("Import(): %v", err)
	}
	options := Options{V2Primary: true, V2: &V2Dependencies{Enabled: true, V2Primary: true, V2Store: store, Media: service}}

	saved := callDownloadMedia(t, options, map[string]any{"message_id": "v2msg:message-imported:0", "save": true})
	if saved.IsError {
		t.Fatalf("download_media(imported) error: %s", resultText(t, saved))
	}
	assertPrivateFile(t, structuredMap(t, saved)["path"].(string), content)

	gone := callDownloadMedia(t, options, map[string]any{"message_id": "message-gone"})
	if !gone.IsError || !strings.Contains(resultText(t, gone), "attachment unavailable: "+media.ErrGone.Error()) {
		t.Fatalf("download_media(gone) = %s, want the gone reason", resultText(t, gone))
	}
	missing := callDownloadMedia(t, options, map[string]any{"message_id": "message-missing"})
	if !missing.IsError || resultText(t, missing) != "message or attachment not found" {
		t.Fatalf("download_media(missing) = %s", resultText(t, missing))
	}
}

func TestParseMediaReference(t *testing.T) {
	for raw, want := range map[string]mediaReference{
		"plain-id":              {MessageID: "plain-id"},
		"v2msg:abc:0":           {MessageID: "abc", Ordinal: 0, MediaID: true},
		"v2msg:abc:7":           {MessageID: "abc", Ordinal: 7, MediaID: true},
		"v2msg:abc":             {MessageID: "abc", MediaID: true},
		"v2msg:abc:x":           {MessageID: "v2msg:abc:x"},
		"whatsapp:thread:msg-1": {MessageID: "whatsapp:thread:msg-1"},
	} {
		if got := parseMediaReference(raw); got != want {
			t.Errorf("parseMediaReference(%q) = %+v, want %+v", raw, got, want)
		}
	}
}
