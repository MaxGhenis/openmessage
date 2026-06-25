package web

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/maxghenis/openmessage/internal/db"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestGIFSearchEndpointReturnsProxiedKlipyResults(t *testing.T) {
	t.Setenv("OPENMESSAGES_KLIPY_API_KEY", "test-klipy-key")
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("q"); got != "thumbs up" {
			t.Fatalf("query = %q, want thumbs up", got)
		}
		if got := r.URL.Query().Get("key"); got != "test-klipy-key" {
			t.Fatalf("provider API key = %q, want test-klipy-key", got)
		}
		if got := r.URL.Query().Get("page"); got != "2" {
			t.Fatalf("provider page = %q, want 2", got)
		}
		if got := r.URL.Query().Get("per_page"); got != "5" {
			t.Fatalf("provider per_page = %q, want 5", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"results": [{
				"id": "gif-1",
				"title": "Thumbs up",
				"media_formats": {
					"tinygif": {"url": "https://media.klipy.com/preview.gif", "size": 12000, "dims": [120, 90]},
					"gif": {"url": "https://media.klipy.com/full.gif", "size": 4000000, "dims": [640, 480]},
					"mediumgif": {"url": "https://media.klipy.com/medium.gif", "size": 2200000, "dims": [320, 240]},
					"webp": {"url": "https://media.klipy.com/full.webp", "size": 900000, "dims": [640, 480]}
				}
			}]
		}`))
	}))
	defer provider.Close()

	oldEndpoint := klipySearchEndpoint
	oldClient := gifHTTPClient
	klipySearchEndpoint = provider.URL
	gifHTTPClient = provider.Client()
	t.Cleanup(func() {
		klipySearchEndpoint = oldEndpoint
		gifHTTPClient = oldClient
	})

	ts := newTestServer(t)
	resp, err := http.Get(ts.server.URL + "/api/gifs?q=thumbs%20up&limit=5&page=2")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, string(raw))
	}

	var payload struct {
		Results []gifSearchResult `json:"results"`
		Page    int               `json:"page"`
		HasMore bool              `json:"has_more"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Results) != 1 {
		t.Fatalf("got %d results, want 1", len(payload.Results))
	}
	if payload.Page != 2 {
		t.Fatalf("page = %d, want 2", payload.Page)
	}
	if payload.HasMore {
		t.Fatal("has_more should be false when fewer results than limit are returned")
	}
	result := payload.Results[0]
	if result.URL != "https://media.klipy.com/full.webp" {
		t.Fatalf("selected URL = %q, want smaller webp", result.URL)
	}
	if result.MimeType != "image/webp" {
		t.Fatalf("mime = %q, want image/webp", result.MimeType)
	}
	if !strings.HasPrefix(result.PreviewURL, "/api/gifs/preview?url=") {
		t.Fatalf("preview URL = %q, want local proxy path", result.PreviewURL)
	}
}

func TestGIFSearchEndpointRequiresKlipyAPIKey(t *testing.T) {
	t.Setenv("OPENMESSAGES_KLIPY_API_KEY", "")
	t.Setenv("KLIPY_API_KEY", "")

	ts := newTestServer(t)
	resp, err := http.Get(ts.server.URL + "/api/gifs?q=thumbs%20up&limit=5")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotImplemented {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 501: %s", resp.StatusCode, string(raw))
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "OPENMESSAGES_KLIPY_API_KEY") {
		t.Fatalf("body = %q, want setup hint", string(raw))
	}
}

func TestDownloadGIFMediaRejectsNonKlipyURL(t *testing.T) {
	if _, _, _, err := downloadGIFMedia(context.Background(), "https://example.com/not-allowed.gif", maxGIFSendBytes); err == nil {
		t.Fatal("expected non-Klipy GIF URL to be rejected")
	}
}

func TestSendGIFUsesSignalMediaSender(t *testing.T) {
	oldClient := gifHTTPClient
	gifHTTPClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != "https://media.klipy.com/fake/openmessage.gif" {
			t.Fatalf("download URL = %s", r.URL.String())
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"image/gif"}},
			Body:       io.NopCloser(strings.NewReader("GIF89a")),
			Request:    r,
		}, nil
	})}
	t.Cleanup(func() {
		gifHTTPClient = oldClient
	})

	var gotConversationID, gotFilename, gotMIME, gotCaption, gotReplyToID string
	var gotData []byte
	ts := newTestServerWithOptions(t, APIOptions{
		SendSignalMedia: func(conversationID string, data []byte, filename, mime, caption, replyToID string) (*db.Message, error) {
			gotConversationID = conversationID
			gotFilename = filename
			gotMIME = mime
			gotCaption = caption
			gotReplyToID = replyToID
			gotData = append([]byte(nil), data...)
			return &db.Message{
				MessageID:      "signal:local:gif-1",
				ConversationID: conversationID,
				Body:           caption,
				IsFromMe:       true,
				TimestampMS:    1234,
				Status:         "sent",
				MediaID:        "signallocal:gif",
				MimeType:       mime,
				ReplyToID:      replyToID,
				SourcePlatform: "signal",
			}, nil
		},
	})
	if err := ts.store.UpsertConversation(&db.Conversation{
		ConversationID: "signal:+15551234567",
		Name:           "Taylor Price",
		SourcePlatform: "signal",
	}); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Post(ts.server.URL+"/api/send-gif", "application/json", strings.NewReader(`{
		"conversation_id": "signal:+15551234567",
		"url": "https://media.klipy.com/fake/openmessage.gif",
		"caption": "gif caption",
		"reply_to_id": "signal:reply-1"
	}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, string(raw))
	}
	if gotConversationID != "signal:+15551234567" {
		t.Fatalf("conversation_id = %q", gotConversationID)
	}
	if gotFilename != "openmessage.gif" {
		t.Fatalf("filename = %q, want openmessage.gif", gotFilename)
	}
	if gotMIME != "image/gif" {
		t.Fatalf("mime = %q, want image/gif", gotMIME)
	}
	if gotCaption != "gif caption" {
		t.Fatalf("caption = %q, want gif caption", gotCaption)
	}
	if gotReplyToID != "signal:reply-1" {
		t.Fatalf("reply_to_id = %q, want signal:reply-1", gotReplyToID)
	}
	if string(gotData) != "GIF89a" {
		t.Fatalf("data = %q, want GIF89a", string(gotData))
	}
}
