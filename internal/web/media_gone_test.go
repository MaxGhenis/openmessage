package web

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/media"
	"github.com/maxghenis/openmessage/internal/messaging"
	"github.com/maxghenis/openmessage/internal/storage/blob"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

type mediaDownloaderFunc func(context.Context, string, bridge.MediaRef) (bridge.MediaStream, error)

func (f mediaDownloaderFunc) DownloadMedia(ctx context.Context, accountID string, ref bridge.MediaRef) (bridge.MediaStream, error) {
	return f(ctx, accountID, ref)
}

// downloadOnlyRegistry is one registered account that can only download.
type downloadOnlyRegistry struct {
	accountID string
	download  bridge.MediaDownloader
}

func (r *downloadOnlyRegistry) Snapshot(accountID string) (bridge.Snapshot, bool) {
	if accountID != r.accountID {
		return bridge.Snapshot{}, false
	}
	return bridge.Snapshot{AccountID: accountID, Platform: bridge.PlatformSignal, Generation: 1}, true
}

func (r *downloadOnlyRegistry) Acquire(_ context.Context, accountID string, _ bridge.Capability) (*bridge.DispatchLease, error) {
	if accountID != r.accountID {
		return nil, bridge.ErrAccountNotRegistered
	}
	return &bridge.DispatchLease{Download: r.download}, nil
}

func (r *downloadOnlyRegistry) Capabilities(accountID string) bridge.CapabilitySet {
	return bridge.CapabilitySet{MediaDownload: accountID == r.accountID}
}

const mediaGoneMessageID = "v2-signal-message"

// newMediaGoneHandler serves a v2-primary API over one Signal message whose
// attachment is pending with the size Signal declared, downloading through
// download.
func newMediaGoneHandler(t *testing.T, declaredSize int64, download mediaDownloaderFunc) http.Handler {
	t.Helper()
	ctx := context.Background()
	v2, err := sqlite.Open(filepath.Join(t.TempDir(), "v2.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = v2.Close() })
	nowMS := time.Now().UnixMilli()
	if err := v2.UpsertAccount(sqlite.Account{
		AccountID: "signal-primary", BridgeKey: "signal_cli", DisplayName: "Signal",
		Mode: sqlite.AccountModeLive, Enabled: true, ConfigJSON: "{}", CreatedAtMS: nowMS, UpdatedAtMS: nowMS,
	}); err != nil {
		t.Fatal(err)
	}
	if err := v2.UpsertConversation(sqlite.Conversation{
		ConversationID: "v2-signal-conversation", AccountID: "signal-primary",
		RemoteConversationID: "signal:0d6c1f9e-3b8a-4c2e-9f71-5a4e2b8c7d10", Kind: sqlite.ConversationKindDirect,
		Title: "Sam Example", NotificationMode: sqlite.NotificationModeAll, MetadataJSON: "{}",
		CreatedAtMS: nowMS, UpdatedAtMS: nowMS,
	}); err != nil {
		t.Fatal(err)
	}
	messages, err := sqlite.NewMessageRepository(v2, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if err := messages.ImportMessage(ctx, sqlite.MessageProjection{
		Message: sqlite.Message{
			MessageID: mediaGoneMessageID, ConversationID: "v2-signal-conversation", AccountID: "signal-primary",
			RemoteMessageID: "remote-signal-message", Direction: sqlite.MessageDirectionIncoming,
			State: sqlite.MessageStateActive, OccurredAtMS: nowMS,
		},
		Attachments: []sqlite.MessageAttachment{{
			Ordinal: 0, RemoteID: "Q3x9KpLmN2vB7cRtYw4z.html", Filename: "Report.html", MIME: "text/html", SizeBytes: &declaredSize,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	blobs, err := blob.New(filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	mediaService, err := media.NewService(v2, &downloadOnlyRegistry{accountID: "signal-primary", download: download}, blobs, messaging.SystemClock{})
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := db.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = legacy.Close() })
	return APIHandlerWithOptions(legacy, nil, zerolog.Nop(), nil, APIOptions{
		V2Primary: true,
		V2:        &V2Options{V2Store: v2, Media: mediaService, Blobs: blobs},
	})
}

func serveMediaRequest(handler http.Handler, method, path string, body []byte, header http.Header) *httptest.ResponseRecorder {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request := httptest.NewRequest(method, "http://127.0.0.1"+path, reader)
	for key, values := range header {
		request.Header[key] = values
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func errorBody(t *testing.T, recorder *httptest.ResponseRecorder) string {
	t.Helper()
	var payload struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode error body %q: %v", recorder.Body.String(), err)
	}
	return payload.Error
}

// The 2026-10-21 symptom: an attachment signal-cli never stored answered
// /api/media with 500 "internal server error". It is a known, final state
// of a message that exists, so every media route answers 410 Gone with the
// reason (404 stays for an unknown message or ordinal), and only the first
// request asks the transport.
func TestV2MediaForAttachmentTheTransportNoLongerHasIs410NotA500(t *testing.T) {
	calls := 0
	handler := newMediaGoneHandler(t, 0, func(context.Context, string, bridge.MediaRef) (bridge.MediaStream, error) {
		calls++
		return bridge.MediaStream{}, fmt.Errorf(
			"%w: Signal never delivered attachment Q3x9KpLmN2vB7cRtYw4z.html: signal-cli could not download it when the message arrived and cannot fetch it again; the sender can send it again: %w",
			bridge.ErrMediaGone,
			errors.New("download Signal attachment: exit status 1: Could not find attachment with ID: Q3x9KpLmN2vB7cRtYw4z.html"),
		)
	})
	for _, path := range []string{
		"/api/media/" + mediaGoneMessageID,
		"/api/media/v2msg:" + mediaGoneMessageID + ":0",
		"/api/v1/messages/" + mediaGoneMessageID + "/attachments/0",
	} {
		recorder := serveMediaRequest(handler, http.MethodGet, path, nil, nil)
		if recorder.Code != http.StatusGone {
			t.Fatalf("GET %s status = %d, want 410; body=%s", path, recorder.Code, recorder.Body.String())
		}
		message := errorBody(t, recorder)
		if !strings.Contains(message, media.ErrGone.Error()) || !strings.Contains(message, "Could not find attachment with ID") {
			t.Fatalf("GET %s error = %q, want the gone reason", path, message)
		}
	}
	if calls != 1 {
		t.Fatalf("transport calls = %d, want 1: the attachment is marked unavailable after the first", calls)
	}
	if recorder := serveMediaRequest(handler, http.MethodGet, "/api/media/v2msg:"+mediaGoneMessageID+":1", nil, nil); recorder.Code != http.StatusNotFound || errorBody(t, recorder) != "not found" {
		t.Fatalf("GET unknown ordinal = %d %s, want plain 404", recorder.Code, recorder.Body.String())
	}
}

func TestV2MediaTransportUnavailableIs503WithRetryAfter(t *testing.T) {
	handler := newMediaGoneHandler(t, 0, func(context.Context, string, bridge.MediaRef) (bridge.MediaStream, error) {
		return bridge.MediaStream{}, bridge.OpError{
			Class:     bridge.FailureTransient,
			Operation: "download_media",
			RetryAt:   time.Now().Add(30 * time.Second),
			Cause:     errors.New("Signal is not connected"),
		}
	})
	recorder := serveMediaRequest(handler, http.MethodGet, "/api/media/"+mediaGoneMessageID, nil, nil)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", recorder.Code, recorder.Body.String())
	}
	if message := errorBody(t, recorder); !strings.Contains(message, "Signal is not connected") {
		t.Fatalf("error = %q, want the transport's reason", message)
	}
	if retry := recorder.Header().Get("Retry-After"); retry == "" || retry == "0" {
		t.Fatalf("Retry-After = %q, want the transport's retry time", retry)
	}
}

func TestV2AttachmentImportFillsAndServesAPendingAttachment(t *testing.T) {
	content := []byte("<html><body>a copy saved from another client</body></html>")
	sum := sha256.Sum256(content)
	digest := hex.EncodeToString(sum[:])
	handler := newMediaGoneHandler(t, int64(len(content)), func(context.Context, string, bridge.MediaRef) (bridge.MediaStream, error) {
		return bridge.MediaStream{}, fmt.Errorf("%w: gone", bridge.ErrMediaGone)
	})
	path := "/api/v1/messages/" + mediaGoneMessageID + "/attachments/0"

	for name, tc := range map[string]struct {
		body   []byte
		header http.Header
		code   int
	}{
		"bad digest header": {content, http.Header{"X-Content-Sha256": {"not-hex"}}, http.StatusBadRequest},
		"wrong digest":      {content, http.Header{"X-Content-Sha256": {strings.Repeat("a", 64)}}, http.StatusUnprocessableEntity},
		"declared size":     {content[1:], nil, http.StatusUnprocessableEntity},
	} {
		if recorder := serveMediaRequest(handler, http.MethodPut, path, tc.body, tc.header); recorder.Code != tc.code {
			t.Fatalf("PUT (%s) status = %d, want %d; body=%s", name, recorder.Code, tc.code, recorder.Body.String())
		}
	}
	if recorder := serveMediaRequest(handler, http.MethodPut, "/api/v1/messages/unknown/attachments/0", content, nil); recorder.Code != http.StatusNotFound {
		t.Fatalf("PUT unknown message status = %d, want 404", recorder.Code)
	}

	recorder := serveMediaRequest(handler, http.MethodPut, path, content, http.Header{"X-Content-Sha256": {digest}})
	if recorder.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	var imported v1AttachmentImportResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &imported); err != nil {
		t.Fatal(err)
	}
	if !imported.Imported || imported.State != "downloaded" || imported.BlobHash != digest ||
		imported.SizeBytes != int64(len(content)) || imported.Filename != "Report.html" {
		t.Fatalf("PUT response = %+v", imported)
	}

	for _, get := range []string{"/api/media/" + mediaGoneMessageID, "/api/media/v2msg:" + mediaGoneMessageID + ":0", path} {
		served := serveMediaRequest(handler, http.MethodGet, get, nil, nil)
		if served.Code != http.StatusOK || !bytes.Equal(served.Body.Bytes(), content) {
			t.Fatalf("GET %s = %d %q, want the imported bytes", get, served.Code, served.Body.String())
		}
		if served.Header().Get("X-Content-SHA256") != digest {
			t.Fatalf("GET %s X-Content-SHA256 = %q, want %s", get, served.Header().Get("X-Content-SHA256"), digest)
		}
		// HTML is never served inline from the API origin.
		if served.Header().Get("Content-Type") != "application/octet-stream" ||
			!strings.HasPrefix(served.Header().Get("Content-Disposition"), "attachment;") {
			t.Fatalf("GET %s headers = %v, want a forced download", get, served.Header())
		}
	}

	again := serveMediaRequest(handler, http.MethodPut, path, content, nil)
	if again.Code != http.StatusOK || !strings.Contains(again.Body.String(), `"imported":false`) {
		t.Fatalf("PUT same bytes again = %d %s, want an idempotent 200", again.Code, again.Body.String())
	}
	changed := append([]byte(nil), content...)
	changed[0] = '!'
	if conflict := serveMediaRequest(handler, http.MethodPut, path, changed, nil); conflict.Code != http.StatusConflict {
		t.Fatalf("PUT different bytes = %d %s, want 409", conflict.Code, conflict.Body.String())
	}
}

func TestParseV2MediaID(t *testing.T) {
	for raw, want := range map[string]struct {
		id      string
		ordinal int64
	}{
		"message-1":               {"message-1", 0},
		"v2msg:message-1:0":       {"message-1", 0},
		"v2msg:message-1:3":       {"message-1", 3},
		"v2msg:with:colons:2":     {"with:colons", 2},
		"v2msg:message-1":         {"message-1", 0},
		"v2msg:message-1:-1":      {"v2msg:message-1:-1", 0},
		"v2msg:message-1:notanum": {"v2msg:message-1:notanum", 0},
	} {
		id, ordinal := parseV2MediaID(raw)
		if id != want.id || ordinal != want.ordinal {
			t.Errorf("parseV2MediaID(%q) = (%q, %d), want (%q, %d)", raw, id, ordinal, want.id, want.ordinal)
		}
	}
}

// An adapter that cannot decode the attachment reference fails the same way
// on every retry; it keeps the 501 it answered before transport failures
// became 503s.
func TestV2MediaUnsupportedReferenceStays501(t *testing.T) {
	handler := newMediaGoneHandler(t, 0, func(context.Context, string, bridge.MediaRef) (bridge.MediaStream, error) {
		return bridge.MediaStream{}, bridge.OpError{
			Class:       bridge.FailureUnsupported,
			Operation:   "download_media",
			Fingerprint: "opaque_version_unsupported",
			Cause:       errors.New("unsupported Signal media Opaque version 9"),
		}
	})
	recorder := serveMediaRequest(handler, http.MethodGet, "/api/media/"+mediaGoneMessageID, nil, nil)
	if recorder.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501; body=%s", recorder.Code, recorder.Body.String())
	}
	if recorder.Header().Get("Retry-After") != "" {
		t.Fatalf("Retry-After = %q on a failure no retry fixes", recorder.Header().Get("Retry-After"))
	}
}

func TestV2MediaRetryAfterRoundsUp(t *testing.T) {
	handler := newMediaGoneHandler(t, 0, func(context.Context, string, bridge.MediaRef) (bridge.MediaStream, error) {
		return bridge.MediaStream{}, bridge.OpError{
			Class:     bridge.FailureRateLimited,
			Operation: "download_media",
			RetryAt:   time.Now().Add(300 * time.Millisecond),
			Cause:     errors.New("slow down"),
		}
	})
	recorder := serveMediaRequest(handler, http.MethodGet, "/api/media/"+mediaGoneMessageID, nil, nil)
	if recorder.Code != http.StatusServiceUnavailable || recorder.Header().Get("Retry-After") != "1" {
		t.Fatalf("status = %d, Retry-After = %q; want 503 and 1", recorder.Code, recorder.Header().Get("Retry-After"))
	}
}
