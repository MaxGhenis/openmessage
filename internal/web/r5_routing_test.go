package web

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/media"
	"github.com/maxghenis/openmessage/internal/messaging"
	"github.com/maxghenis/openmessage/internal/readsource"
	"github.com/maxghenis/openmessage/internal/storage/blob"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

type routingReadSource struct {
	readsource.ReadSource
	calls map[string]int
}

func newRoutingReadSource() *routingReadSource {
	return &routingReadSource{calls: make(map[string]int)}
}

func (s *routingReadSource) ListConversations(limit int) ([]*db.Conversation, error) {
	s.calls["list"]++
	return []*db.Conversation{{
		ConversationID: "v2-conversation",
		Name:           "V2 Alice",
		LastMessageTS:  200,
		SourcePlatform: "sms",
	}}, nil
}

func (s *routingReadSource) GetConversation(id string) (*db.Conversation, error) {
	s.calls["conversation"]++
	return &db.Conversation{
		ConversationID: id,
		Name:           "V2 Alice",
		LastMessageTS:  200,
		SourcePlatform: "sms",
	}, nil
}

func (s *routingReadSource) GetMessagesByConversation(conversationID string, limit int) ([]*db.Message, error) {
	s.calls["messages"]++
	return []*db.Message{routingMessage("latest from v2")}, nil
}

func (s *routingReadSource) GetMessagesByConversationBefore(conversationID string, beforeMS int64, beforeID string, limit int) ([]*db.Message, error) {
	s.calls["before"]++
	return []*db.Message{routingMessage("before from v2")}, nil
}

func (s *routingReadSource) GetMessagesByConversationAfter(conversationID string, afterMS int64, afterID string, limit int) ([]*db.Message, error) {
	s.calls["after"]++
	return []*db.Message{routingMessage("after from v2")}, nil
}

func (s *routingReadSource) GetMessagesAroundMessage(conversationID, messageID string, before, after int) ([]*db.Message, error) {
	s.calls["around"]++
	return []*db.Message{routingMessage("around from v2")}, nil
}

func (s *routingReadSource) SearchMessagesFiltered(query string, filter db.SearchFilter) ([]*db.Message, error) {
	s.calls["search"]++
	return []*db.Message{routingMessage("search from v2")}, nil
}

func (s *routingReadSource) PlatformStats() ([]db.PlatformStat, error) {
	s.calls["platform_stats"]++
	return []db.PlatformStat{{Platform: "sms", Count: 42, LatestMS: 200, LatestRecvMS: 100}}, nil
}

func (s *routingReadSource) MessageCount(sourcePlatform string) (int, error) {
	s.calls["message_count"]++
	if sourcePlatform == "" {
		return 42, nil
	}
	if sourcePlatform == "sms" {
		return 42, nil
	}
	return 0, nil
}

func (s *routingReadSource) ConversationCount(sourcePlatform string) (int, error) {
	s.calls["conversation_count"]++
	if sourcePlatform == "" || sourcePlatform == "sms" {
		return 3, nil
	}
	return 0, nil
}

func (s *routingReadSource) LatestTimestamp(sourcePlatform string) (int64, error) {
	s.calls["latest_timestamp"]++
	if sourcePlatform == "sms" {
		return 200, nil
	}
	return 0, nil
}

func (s *routingReadSource) LatestConversationPreviews(ids []string) (map[string]string, error) {
	s.calls["previews"]++
	return map[string]string{"v2-conversation": "preview from v2"}, nil
}

func routingMessage(body string) *db.Message {
	return &db.Message{
		MessageID:      "v2-message",
		ConversationID: "v2-conversation",
		Body:           body,
		TimestampMS:    200,
		SourcePlatform: "sms",
	}
}

func TestR5CanonicalReadRoutesUseConfiguredSource(t *testing.T) {
	legacy, err := db.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = legacy.Close() })
	if err := legacy.UpsertConversation(&db.Conversation{
		ConversationID: "legacy-conversation",
		Name:           "Legacy Alice",
		LastMessageTS:  100,
	}); err != nil {
		t.Fatal(err)
	}
	if err := legacy.UpsertMessage(&db.Message{
		MessageID:      "legacy-message",
		ConversationID: "legacy-conversation",
		Body:           "legacy body",
		TimestampMS:    100,
	}); err != nil {
		t.Fatal(err)
	}

	reads := newRoutingReadSource()
	handler := APIHandlerWithOptions(legacy, nil, zerolog.Nop(), nil, APIOptions{
		Reads:     reads,
		V2Primary: true,
	})

	tests := []struct {
		name     string
		path     string
		wantBody string
		wantCall string
	}{
		{name: "conversations", path: "/api/conversations", wantBody: "V2 Alice", wantCall: "list"},
		{name: "messages", path: "/api/conversations/v2-conversation/messages", wantBody: "latest from v2", wantCall: "messages"},
		{name: "before", path: "/api/conversations/v2-conversation/messages?before=200&before_id=v2-message", wantBody: "before from v2", wantCall: "before"},
		{name: "after", path: "/api/conversations/v2-conversation/messages?after=200&after_id=v2-message", wantBody: "after from v2", wantCall: "after"},
		{name: "around", path: "/api/conversations/v2-conversation/messages-around?message_id=v2-message", wantBody: "around from v2", wantCall: "around"},
		{name: "thread search", path: "/api/conversations/v2-conversation/search?q=search", wantBody: "search from v2", wantCall: "search"},
		{name: "global search", path: "/api/search?q=search", wantBody: "search from v2", wantCall: "search"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			before := reads.calls[test.wantCall]
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "http://127.0.0.1"+test.path, nil))
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
			}
			if !strings.Contains(recorder.Body.String(), test.wantBody) {
				t.Fatalf("body = %s, want content %q", recorder.Body.String(), test.wantBody)
			}
			if strings.Contains(recorder.Body.String(), "legacy") {
				t.Fatalf("body leaked legacy data: %s", recorder.Body.String())
			}
			if reads.calls[test.wantCall] <= before {
				t.Fatalf("configured read source method %q was not called", test.wantCall)
			}
		})
	}

	if reads.calls["previews"] == 0 {
		t.Fatal("conversation previews did not use configured read source")
	}
}

func TestR5LegacySendEndpointsQuiescedInV2Primary(t *testing.T) {
	legacy, err := db.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = legacy.Close() })
	handler := APIHandlerWithOptions(legacy, nil, zerolog.Nop(), nil, APIOptions{V2Primary: true})

	for _, path := range []string{"/api/send", "/api/send-media", "/api/send-gif", "/api/drafts/send"} {
		t.Run(path, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "http://127.0.0.1"+path, strings.NewReader("{}")))
			response := recorder.Result()
			defer response.Body.Close()
			if response.StatusCode != http.StatusConflict {
				raw, _ := io.ReadAll(response.Body)
				t.Fatalf("status = %d, want 409; body=%s", response.StatusCode, raw)
			}
			var payload map[string]string
			if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			if payload["error"] != "v2 primary: use /api/v1/outbox" {
				t.Fatalf("error = %q", payload["error"])
			}
		})
	}
}

func TestR5V1SubmitUsesNativeV2ConversationIDInV2Primary(t *testing.T) {
	harness := newA3Harness(t, false)
	nowMS := time.Now().UnixMilli()
	if err := harness.v2.UpsertAccount(sqlite.Account{
		AccountID:   "google-primary",
		BridgeKey:   "google_messages",
		DisplayName: "Google",
		Mode:        sqlite.AccountModeLive,
		Enabled:     true,
		ConfigJSON:  "{}",
		CreatedAtMS: nowMS,
		UpdatedAtMS: nowMS,
	}); err != nil {
		t.Fatal(err)
	}
	const conversationID = "v2-native-conversation"
	if err := harness.v2.UpsertConversation(sqlite.Conversation{
		ConversationID:       conversationID,
		AccountID:            "google-primary",
		RemoteConversationID: "remote-v2-native-conversation",
		Kind:                 sqlite.ConversationKindDirect,
		Title:                "Native v2 thread",
		NotificationMode:     sqlite.NotificationModeAll,
		MetadataJSON:         "{}",
		CreatedAtMS:          nowMS,
		UpdatedAtMS:          nowMS,
	}); err != nil {
		t.Fatal(err)
	}

	handler := APIHandlerWithOptions(harness.legacy, nil, zerolog.Nop(), nil, APIOptions{
		V2Primary: true,
		V2: &V2Options{
			Service:  harness.service,
			V2Store:  harness.v2,
			Registry: harness.registry,
		},
	})
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(
		http.MethodPost,
		"http://127.0.0.1/api/v1/outbox/messages",
		strings.NewReader(`{"conversation_id":"v2-native-conversation","body":"native","idempotency_key":"native-route-key"}`),
	)
	request.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	if legacyConversation, _ := harness.legacy.GetConversation(conversationID); legacyConversation != nil {
		t.Fatalf("native v2 id unexpectedly created a legacy mirror row: conversation=%+v", legacyConversation)
	}
}

func TestR5CanonicalMediaRouteServesV2PrimaryAttachment(t *testing.T) {
	ctx := context.Background()
	v2, err := sqlite.Open(filepath.Join(t.TempDir(), "v2.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = v2.Close() })
	blobs, err := blob.New(filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("v2 canonical attachment")
	ref, err := blobs.Put(ctx, bytes.NewReader(content), "text/plain", int64(len(content)))
	if err != nil {
		t.Fatal(err)
	}
	nowMS := time.Now().UnixMilli()
	if err := v2.UpsertAccount(sqlite.Account{
		AccountID:   "google-primary",
		BridgeKey:   "google_messages",
		DisplayName: "Google",
		Mode:        sqlite.AccountModeLive,
		Enabled:     true,
		ConfigJSON:  "{}",
		CreatedAtMS: nowMS,
		UpdatedAtMS: nowMS,
	}); err != nil {
		t.Fatal(err)
	}
	if err := v2.UpsertConversation(sqlite.Conversation{
		ConversationID:       "v2-media-conversation",
		AccountID:            "google-primary",
		RemoteConversationID: "remote-v2-media-conversation",
		Kind:                 sqlite.ConversationKindDirect,
		Title:                "V2 media",
		NotificationMode:     sqlite.NotificationModeAll,
		MetadataJSON:         "{}",
		CreatedAtMS:          nowMS,
		UpdatedAtMS:          nowMS,
	}); err != nil {
		t.Fatal(err)
	}
	messages, err := sqlite.NewMessageRepository(v2, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	const messageID = "v2-media-message"
	if err := messages.ImportMessage(ctx, sqlite.MessageProjection{
		Message: sqlite.Message{
			MessageID:       messageID,
			ConversationID:  "v2-media-conversation",
			AccountID:       "google-primary",
			RemoteMessageID: "remote-v2-media-message",
			Direction:       sqlite.MessageDirectionIncoming,
			State:           sqlite.MessageStateActive,
			OccurredAtMS:    nowMS,
		},
		Attachments: []sqlite.MessageAttachment{{
			Ordinal:  0,
			RemoteID: "remote-attachment",
			Filename: "attachment.txt",
			MIME:     "text/plain",
		}},
	}); err != nil {
		t.Fatal(err)
	}
	attachments, err := sqlite.NewMessageAttachmentRepository(v2, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if err := attachments.MarkDownloaded(ctx, messageID, 0, ref.Hash, int64(len(content)), "text/plain"); err != nil {
		t.Fatal(err)
	}
	mediaService, err := media.NewService(v2, &a3Registry{accountID: "google-primary"}, blobs, messaging.SystemClock{})
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := db.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = legacy.Close() })
	handler := APIHandlerWithOptions(legacy, nil, zerolog.Nop(), nil, APIOptions{
		V2Primary: true,
		V2: &V2Options{
			V2Store: v2,
			Media:   mediaService,
		},
	})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/media/"+messageID, nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	if !bytes.Equal(recorder.Body.Bytes(), content) {
		t.Fatalf("body = %q, want %q", recorder.Body.Bytes(), content)
	}
}

func TestR5StatusCountsRouteToConfiguredSourceInV2Primary(t *testing.T) {
	legacy, err := db.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = legacy.Close() })
	// Legacy has one throwaway conversation with a different platform, so if the
	// handler read legacy the counts would not match the spy's v2 figures.
	reads := newRoutingReadSource()
	handler := APIHandlerWithOptions(legacy, nil, zerolog.Nop(), nil, APIOptions{
		Reads:     reads,
		V2Primary: true,
	})

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/diagnostics", nil))
	response := recorder.Result()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}
	var payload struct {
		MessageCount      int            `json:"message_count"`
		ConversationCount int            `json:"conversation_count"`
		MessageCounts     map[string]int `json:"message_counts"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload.MessageCount != 42 || payload.ConversationCount != 3 {
		t.Fatalf("counts = (msg %d, conv %d), want (42, 3) from the v2 source", payload.MessageCount, payload.ConversationCount)
	}
	if payload.MessageCounts["sms"] != 42 {
		t.Fatalf("message_counts[sms] = %d, want 42 from the v2 source", payload.MessageCounts["sms"])
	}
	if reads.calls["message_count"] == 0 || reads.calls["conversation_count"] == 0 {
		t.Fatalf("status did not consult the configured source: calls=%v", reads.calls)
	}
}

func TestR5StatsAndStoryUnavailableInV2Primary(t *testing.T) {
	legacy, err := db.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = legacy.Close() })
	handler := APIHandlerWithOptions(legacy, nil, zerolog.Nop(), nil, APIOptions{
		Reads:     newRoutingReadSource(),
		V2Primary: true,
	})

	for _, path := range []string{"http://127.0.0.1/api/stats/v2-conversation", "http://127.0.0.1/api/story/v2-conversation"} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if got := recorder.Result().StatusCode; got != http.StatusConflict {
			t.Fatalf("%s status = %d, want 409 (unavailable in v2-primary)", path, got)
		}
	}
}
