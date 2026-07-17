package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/ingest"
	"github.com/maxghenis/openmessage/internal/media"
	"github.com/maxghenis/openmessage/internal/messaging"
	"github.com/maxghenis/openmessage/internal/storage/blob"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
	"github.com/maxghenis/openmessage/internal/v2wire"
)

func TestV1RoutesReturnServiceUnavailableWhenDisabled(t *testing.T) {
	ts := newV1RecorderHarness(t, APIOptions{})

	tests := []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/api/v1/outbox/messages"},
		{http.MethodPost, "/api/v1/outbox/media"},
		{http.MethodGet, "/api/v1/outbox"},
		{http.MethodGet, "/api/v1/outbox/outbox-1"},
		{http.MethodPost, "/api/v1/outbox/outbox-1/cancel"},
		{http.MethodPost, "/api/v1/outbox/outbox-1/retry"},
		{http.MethodPost, "/api/v1/outbox/outbox-1/send-again"},
		{http.MethodPost, "/api/v1/outbox/outbox-1/repair"},
		{http.MethodGet, "/api/v1/outbox/outbox-1/media"},
		{http.MethodGet, "/api/v1/messages/message-1/attachments/0"},
		{http.MethodGet, "/api/v1/not-a-route"},
	}

	for _, test := range tests {
		t.Run(test.method+" "+test.path, func(t *testing.T) {
			req := httptest.NewRequest(test.method, "http://127.0.0.1"+test.path, bytes.NewReader(nil))
			resp := ts.do(t, req)
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusServiceUnavailable {
				body, _ := io.ReadAll(resp.Body)
				t.Fatalf("status = %d, want 503; body=%s", resp.StatusCode, body)
			}
			var payload map[string]string
			if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			if payload["error"] != "v2_send_disabled" {
				t.Fatalf("error = %q, want v2_send_disabled", payload["error"])
			}
		})
	}
}

func TestStatusReportsV2SendAvailability(t *testing.T) {
	tests := []struct {
		name string
		v2   *V2Options
		want bool
	}{
		{name: "disabled", want: false},
		{name: "enabled", v2: &V2Options{}, want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ts := newV1RecorderHarness(t, APIOptions{V2: test.v2})
			resp := ts.do(t, httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/status", nil))
			defer resp.Body.Close()

			var payload map[string]any
			if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			if got, ok := payload["v2_send"].(bool); !ok || got != test.want {
				t.Fatalf("v2_send = %#v, want %t", payload["v2_send"], test.want)
			}
		})
	}
}

func TestStatusReportsV2IngestCounters(t *testing.T) {
	tests := []struct {
		name           string
		provider       func() map[string]ingest.CounterSnapshot
		wantEnabled    bool
		wantPerAccount map[string]ingest.CounterSnapshot
	}{
		{
			name:           "disabled",
			wantPerAccount: map[string]ingest.CounterSnapshot{},
		},
		{
			name: "enabled",
			provider: func() map[string]ingest.CounterSnapshot {
				return map[string]ingest.CounterSnapshot{
					"google:primary": {
						Appended:      7,
						DecodedEvents: 6,
						Projected:     5,
						Quarantined:   1,
					},
				}
			},
			wantEnabled: true,
			wantPerAccount: map[string]ingest.CounterSnapshot{
				"google:primary": {
					Appended:      7,
					DecodedEvents: 6,
					Projected:     5,
					Quarantined:   1,
				},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ts := newV1RecorderHarness(t, APIOptions{V2IngestCounters: test.provider})
			resp := ts.do(t, httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/status", nil))
			defer resp.Body.Close()

			var payload struct {
				V2Ingest struct {
					Enabled    bool                              `json:"enabled"`
					PerAccount map[string]ingest.CounterSnapshot `json:"per_account"`
				} `json:"v2_ingest"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			if payload.V2Ingest.Enabled != test.wantEnabled {
				t.Fatalf("v2_ingest.enabled = %t, want %t", payload.V2Ingest.Enabled, test.wantEnabled)
			}
			if !reflect.DeepEqual(payload.V2Ingest.PerAccount, test.wantPerAccount) {
				t.Fatalf("v2_ingest.per_account = %#v, want %#v", payload.V2Ingest.PerAccount, test.wantPerAccount)
			}
		})
	}
}

func TestV1SubmitRejectsTooShortScheduleAtHandler(t *testing.T) {
	// The dependencies are deliberately otherwise empty: schedule validation must
	// happen before mirroring or submission and must therefore return 400 rather
	// than dereferencing a v2 dependency.
	ts := newV1RecorderHarness(t, APIOptions{V2: &V2Options{}})
	notBeforeMS := time.Now().Add(time.Second).UnixMilli()
	body := fmt.Sprintf(`{
		"conversation_id":"conversation-1",
		"body":"later",
		"idempotency_key":"schedule-too-soon",
		"not_before_ms":%d
	}`, notBeforeMS)
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/v1/outbox/messages", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	resp := ts.do(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 400; body=%s", resp.StatusCode, raw)
	}
}

func TestV1SubmitRequiresIdempotencyKeyBeforeMirroring(t *testing.T) {
	ts := newV1RecorderHarness(t, APIOptions{V2: &V2Options{}})
	req := httptest.NewRequest(
		http.MethodPost,
		"http://127.0.0.1/api/v1/outbox/messages",
		bytes.NewBufferString(`{"conversation_id":"conversation-1","body":"hello"}`),
	)
	req.Header.Set("Content-Type", "application/json")
	resp := ts.do(t, req)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 400; body=%s", resp.StatusCode, raw)
	}
	var payload map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload["error"] != "idempotency_key is required" {
		t.Fatalf("error = %q, want required-key error", payload["error"])
	}
}

func TestV1SubmitRequiresConversationBeforeMirroring(t *testing.T) {
	ts := newV1RecorderHarness(t, APIOptions{V2: &V2Options{}})
	req := httptest.NewRequest(
		http.MethodPost,
		"http://127.0.0.1/api/v1/outbox/messages",
		bytes.NewBufferString(`{"body":"hello","idempotency_key":"missing-conversation"}`),
	)
	req.Header.Set("Content-Type", "application/json")
	resp := ts.do(t, req)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 400; body=%s", resp.StatusCode, raw)
	}
}

func TestV1ErrorStatusMapping(t *testing.T) {
	tests := []struct {
		name        string
		err         error
		wantStatus  int
		wantMessage string
	}{
		{name: "idempotency conflict", err: messaging.ErrIdempotencyConflict, wantStatus: http.StatusConflict},
		{name: "invalid command", err: messaging.ErrInvalidCommand, wantStatus: http.StatusBadRequest},
		{name: "invalid state", err: messaging.ErrInvalidState, wantStatus: http.StatusConflict},
		{name: "platform", err: v2wire.ErrPlatformNotSendable, wantStatus: http.StatusNotImplemented},
		{name: "reply", err: v2wire.ErrReplyTargetUnavailable, wantStatus: http.StatusUnprocessableEntity, wantMessage: "reply_target_unavailable"},
		{name: "media unavailable", err: media.ErrUnavailable, wantStatus: http.StatusServiceUnavailable},
		{name: "not found", err: sqlite.ErrNotFound, wantStatus: http.StatusNotFound, wantMessage: "not found"},
		{name: "too large", err: messaging.ErrTooLarge, wantStatus: http.StatusRequestEntityTooLarge},
		{name: "internal", err: fmt.Errorf("boom"), wantStatus: http.StatusInternalServerError, wantMessage: "internal server error"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			status, message := v1ErrorResponse(test.err)
			if status != test.wantStatus {
				t.Fatalf("status = %d, want %d", status, test.wantStatus)
			}
			if test.wantMessage != "" && message != test.wantMessage {
				t.Fatalf("message = %q, want %q", message, test.wantMessage)
			}
		})
	}
}

func TestLegacyMediaServesV2BlobBeforePlatformDownloader(t *testing.T) {
	blobs, err := blob.New(filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("v2 projected media")
	ref, err := blobs.Put(context.Background(), bytes.NewReader(content), "text/plain", int64(len(content)))
	if err != nil {
		t.Fatal(err)
	}

	downloaderCalled := false
	ts := newV1RecorderHarness(t, APIOptions{
		V2: &V2Options{Blobs: blobs},
		DownloadWhatsAppMedia: func(*db.Message) ([]byte, string, error) {
			downloaderCalled = true
			return nil, "", fmt.Errorf("platform downloader must not be called")
		},
	})
	if err := ts.store.UpsertConversation(&db.Conversation{
		ConversationID: "whatsapp:thread-1",
		SourcePlatform: "whatsapp",
	}); err != nil {
		t.Fatal(err)
	}
	if err := ts.store.UpsertMessage(&db.Message{
		MessageID:      "whatsapp:message-1",
		ConversationID: "whatsapp:thread-1",
		MediaID:        "v2blob:" + ref.Hash,
		MimeType:       "text/plain",
		SourcePlatform: "whatsapp",
		TimestampMS:    1,
	}); err != nil {
		t.Fatal(err)
	}

	resp := ts.do(t, httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/media/whatsapp:message-1", nil))
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, got)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("body = %q, want %q", got, content)
	}
	if downloaderCalled {
		t.Fatal("WhatsApp downloader was called for v2blob media")
	}
}

func TestMarkReadBestEffortWritesV2Cursor(t *testing.T) {
	v2Store, err := sqlite.Open(filepath.Join(t.TempDir(), "v2.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = v2Store.Close() })

	ts := newV1RecorderHarness(t, APIOptions{V2: &V2Options{V2Store: v2Store}})
	if err := ts.store.UpsertConversation(&db.Conversation{
		ConversationID: "google-thread-1",
		Name:           "Alice",
		SourcePlatform: "sms",
		UnreadCount:    3,
	}); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/mark-read", bytes.NewBufferString(`{"conversation_id":"google-thread-1"}`))
	req.Header.Set("Content-Type", "application/json")
	resp := ts.do(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, raw)
	}

	cursor, err := v2Store.GetReadCursor("local-primary:google-primary", "google-thread-1")
	if err != nil {
		t.Fatal(err)
	}
	if cursor.AccountID != "google-primary" || cursor.LastReadMessageID != nil {
		t.Fatalf("cursor = %+v, want google account with nil message ref", cursor)
	}
	if cursor.LastReadAtMS <= 0 || cursor.UpdatedAtMS <= 0 {
		t.Fatalf("cursor timestamps = %+v, want positive", cursor)
	}
}

func TestMarkReadV2FailureDoesNotFailLegacyResponse(t *testing.T) {
	// A nonnil V2 option with no store simulates a local v2 write failure. The
	// legacy unread-count mutation remains authoritative for this response.
	ts := newV1RecorderHarness(t, APIOptions{V2: &V2Options{}})
	if err := ts.store.UpsertConversation(&db.Conversation{
		ConversationID: "google-thread-v2-failure",
		SourcePlatform: "sms",
		UnreadCount:    2,
	}); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/mark-read", bytes.NewBufferString(`{"conversation_id":"google-thread-v2-failure"}`))
	req.Header.Set("Content-Type", "application/json")
	resp := ts.do(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, raw)
	}
	conversation, err := ts.store.GetConversation("google-thread-v2-failure")
	if err != nil {
		t.Fatal(err)
	}
	if conversation.UnreadCount != 0 {
		t.Fatalf("legacy unread count = %d, want 0", conversation.UnreadCount)
	}
}

type v1RecorderHarness struct {
	store   *db.Store
	handler http.Handler
}

func newV1RecorderHarness(t *testing.T, opts APIOptions) *v1RecorderHarness {
	t.Helper()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return &v1RecorderHarness{
		store:   store,
		handler: APIHandlerWithOptions(store, nil, zerolog.Nop(), nil, opts),
	}
}

func (h *v1RecorderHarness) do(t *testing.T, request *http.Request) *http.Response {
	t.Helper()
	recorder := httptest.NewRecorder()
	h.handler.ServeHTTP(recorder, request)
	return recorder.Result()
}
