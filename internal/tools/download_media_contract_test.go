package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/localapi"
	"github.com/maxghenis/openmessage/internal/media"
	"github.com/maxghenis/openmessage/internal/messaging"
	"github.com/maxghenis/openmessage/internal/storage/blob"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
	"github.com/maxghenis/openmessage/internal/web"
)

type contractDownloader func() (bridge.MediaStream, error)

func (f contractDownloader) DownloadMedia(context.Context, string, bridge.MediaRef) (bridge.MediaStream, error) {
	return f()
}

// TestDownloadMediaAgainstTheRealAppHandler runs client-mode download_media
// against the app's real HTTP handler and media service, so the tool's
// reading of status codes and error bodies cannot drift from what the app
// sends (a fake daemon would hide that).
func TestDownloadMediaAgainstTheRealAppHandler(t *testing.T) {
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
	outcomes := map[string]contractDownloader{
		"ok": func() (bridge.MediaStream, error) {
			data := []byte("%PDF-1.4 contract bytes")
			return bridge.MediaStream{ReadCloser: nopReadCloser{bytes.NewReader(data)}, Size: int64(len(data)), MIME: "application/pdf"}, nil
		},
		"gone": func() (bridge.MediaStream, error) {
			return bridge.MediaStream{}, fmt.Errorf("%w: Signal never delivered attachment x.pdf", bridge.ErrMediaGone)
		},
		"offline": func() (bridge.MediaStream, error) {
			return bridge.MediaStream{}, bridge.OpError{Class: bridge.FailureTransient, Operation: "download_media", Cause: errors.New("Signal is not connected")}
		},
		"unsupported": func() (bridge.MediaStream, error) {
			return bridge.MediaStream{}, bridge.OpError{Class: bridge.FailureUnsupported, Operation: "download_media", Cause: errors.New("unsupported opaque")}
		},
	}
	for id := range outcomes {
		if err := messages.ImportMessage(ctx, sqlite.MessageProjection{
			Message: sqlite.Message{
				MessageID: "message-" + id, ConversationID: "conversation-1", AccountID: "signal-primary",
				RemoteMessageID: "remote-" + id, Direction: sqlite.MessageDirectionIncoming,
				State: sqlite.MessageStateActive, OccurredAtMS: nowMS,
			},
			Attachments: []sqlite.MessageAttachment{{Ordinal: 0, RemoteID: id, Filename: id + ".pdf", MIME: "application/pdf", RemoteRef: []byte(id)}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	blobs, err := blob.New(filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	registry := toolDownloadRegistry{download: refDispatchDownloader(outcomes)}
	service, err := media.NewService(store, registry, blobs, messaging.SystemClock{})
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := db.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = legacy.Close() })
	handler := web.APIHandlerWithOptions(legacy, nil, zerolog.Nop(), nil, web.APIOptions{
		V2Primary: true,
		V2:        &web.V2Options{V2Store: store, Media: service, Blobs: blobs},
	})
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	options := Options{V2Primary: true, Daemon: localapi.NewClient(server.URL, "")}

	for id, want := range map[string]string{
		"gone":        "attachment unavailable: media: attachment is no longer available from its transport",
		"offline":     "attachment not downloadable right now (retry later): attachment download unavailable:",
		"unsupported": "(HTTP 501)",
		"missing":     "message or attachment not found",
	} {
		result := callDownloadMedia(t, options, map[string]any{"message_id": "v2msg:message-" + id + ":0"})
		if !result.IsError || !strings.Contains(resultText(t, result), want) {
			t.Errorf("download_media(%s) = %s, want an error containing %q", id, resultText(t, result), want)
		}
	}
	saved := callDownloadMedia(t, options, map[string]any{"message_id": "message-ok", "save": true})
	if saved.IsError {
		t.Fatalf("download_media(ok) error: %s", resultText(t, saved))
	}
	payload := structuredMap(t, saved)
	assertPrivateFile(t, payload["path"].(string), []byte("%PDF-1.4 contract bytes"))
	if payload["filename"] != "ok.pdf" || payload["mime_type"] != "application/pdf" {
		t.Fatalf("download_media(ok) = %#v", payload)
	}
}

type nopReadCloser struct{ *bytes.Reader }

func (nopReadCloser) Close() error { return nil }

// refDispatchDownloader picks an outcome by the attachment's opaque ref.
type refDispatchDownloader map[string]contractDownloader

func (d refDispatchDownloader) DownloadMedia(_ context.Context, _ string, ref bridge.MediaRef) (bridge.MediaStream, error) {
	outcome, ok := d[string(ref.Opaque)]
	if !ok {
		return bridge.MediaStream{}, fmt.Errorf("no outcome for %q", ref.Opaque)
	}
	return outcome()
}
