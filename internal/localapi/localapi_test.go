package localapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testClient(t *testing.T, handler http.Handler) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return &Client{BaseURL: server.URL, HTTP: server.Client()}
}

func TestStatusReachability(t *testing.T) {
	t.Run("daemon absent", func(t *testing.T) {
		client := NewClient("http://127.0.0.1:1", "")
		_, reachable, err := client.Status(context.Background())
		if err == nil {
			t.Fatal("expected probe error")
		}
		if reachable {
			t.Fatal("connection refusal must report unreachable")
		}
	})

	t.Run("daemon answers garbage", func(t *testing.T) {
		client := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, "not json")
		}))
		_, reachable, err := client.Status(context.Background())
		if err == nil {
			t.Fatal("expected decode error")
		}
		if !reachable {
			t.Fatal("an answering daemon must report reachable")
		}
	})

	t.Run("daemon reports mode", func(t *testing.T) {
		client := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(map[string]any{
				"v2_primary": true,
				"auth":       map[string]any{"data_dir": "/tmp/data"},
			})
		}))
		status, reachable, err := client.Status(context.Background())
		if err != nil || !reachable {
			t.Fatalf("Status() = %v, reachable %v", err, reachable)
		}
		if !status.V2Primary || !status.SendsViaOutbox() {
			t.Fatalf("status = %+v", status)
		}
		if status.Auth.DataDir != "/tmp/data" {
			t.Fatalf("auth data dir = %q", status.Auth.DataDir)
		}
	})
}

func TestSubmitTextSendsTokenAndDecodes(t *testing.T) {
	var sawAuth atomic.Value
	client := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth.Store(r.Header.Get("Authorization"))
		var request TextSubmission
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode: %v", err)
		}
		if request.ConversationID != "conv" || request.IdempotencyKey != "key" {
			t.Errorf("request = %+v", request)
		}
		json.NewEncoder(w).Encode(map[string]any{"outbox_id": "out", "state": "queued"})
	}))
	client.Token = "secret"

	submission, err := client.SubmitText(context.Background(), TextSubmission{
		ConversationID: "conv", Body: "hi", IdempotencyKey: "key",
	})
	if err != nil {
		t.Fatalf("SubmitText: %v", err)
	}
	if submission.OutboxID != "out" {
		t.Fatalf("submission = %+v", submission)
	}
	if got := sawAuth.Load(); got != "Bearer secret" {
		t.Fatalf("Authorization = %v", got)
	}
}

func TestSubmitTextRejectsMissingOutboxID(t *testing.T) {
	client := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"state": "queued"})
	}))
	if _, err := client.SubmitText(context.Background(), TextSubmission{ConversationID: "c", IdempotencyKey: "k"}); err == nil {
		t.Fatal("expected error for response without outbox_id")
	}
}

func TestSubmitMediaStreamsMultipart(t *testing.T) {
	client := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("parse multipart: %v", err)
		}
		if got := r.FormValue("conversation_id"); got != "conv" {
			t.Errorf("conversation_id = %q", got)
		}
		if got := r.FormValue("idempotency_key"); got != "key" {
			t.Errorf("idempotency_key = %q", got)
		}
		if got := r.FormValue("caption"); got != "look" {
			t.Errorf("caption = %q", got)
		}
		file, header, err := r.FormFile("file")
		if err != nil {
			t.Errorf("form file: %v", err)
		} else {
			defer file.Close()
			data, _ := io.ReadAll(file)
			if string(data) != "payload-bytes" {
				t.Errorf("file body = %q", data)
			}
			if header.Filename != "photo.png" {
				t.Errorf("filename = %q", header.Filename)
			}
			if got := header.Header.Get("Content-Type"); got != "image/png" {
				t.Errorf("file content type = %q", got)
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"outbox_id": "out-media", "state": "queued"})
	}))

	submission, err := client.SubmitMedia(context.Background(), MediaSubmission{
		ConversationID: "conv",
		Filename:       "photo.png",
		MIME:           "image/png",
		Caption:        "look",
		IdempotencyKey: "key",
		Content:        strings.NewReader("payload-bytes"),
	})
	if err != nil {
		t.Fatalf("SubmitMedia: %v", err)
	}
	if submission.OutboxID != "out-media" {
		t.Fatalf("submission = %+v", submission)
	}
}

func TestWaitDeliveryPollsUntilSettled(t *testing.T) {
	var calls atomic.Int64
	client := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state := "queued"
		if calls.Add(1) >= 3 {
			state = "confirmed"
		}
		json.NewEncoder(w).Encode(map[string]any{"outbox_id": "out", "state": state})
	}))

	delivery, settled, err := client.WaitDelivery(context.Background(), "out", 5*time.Second)
	if err != nil {
		t.Fatalf("WaitDelivery: %v", err)
	}
	if !settled || delivery.State != "confirmed" {
		t.Fatalf("delivery = %+v settled = %v", delivery, settled)
	}
	if calls.Load() < 3 {
		t.Fatalf("expected at least 3 polls, got %d", calls.Load())
	}
}

func TestWaitDeliveryTimeoutReturnsLastObserved(t *testing.T) {
	client := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"outbox_id": "out", "state": "dispatching"})
	}))

	delivery, settled, err := client.WaitDelivery(context.Background(), "out", 300*time.Millisecond)
	if err != nil {
		t.Fatalf("WaitDelivery: %v", err)
	}
	if settled {
		t.Fatal("expected unsettled result at timeout")
	}
	if delivery.State != "dispatching" {
		t.Fatalf("delivery = %+v", delivery)
	}
}

func TestWaitDeliveryNeverObservedReturnsError(t *testing.T) {
	client := NewClient("http://127.0.0.1:1", "")
	client.HTTP.Timeout = 100 * time.Millisecond
	_, settled, err := client.WaitDelivery(context.Background(), "out", 250*time.Millisecond)
	if err == nil {
		t.Fatal("expected error when the delivery state was never readable")
	}
	if settled {
		t.Fatal("unreadable delivery cannot be settled")
	}
}

func TestDeliverySettled(t *testing.T) {
	for state, want := range map[string]bool{
		"":               false,
		"queued":         false,
		"dispatching":    false,
		"confirmed":      true,
		"uncertain":      true,
		"rejected":       true,
		"canceled":       true,
		"store_failed":   true,
		"not_dispatched": true,
	} {
		if got := (Delivery{State: state}).Settled(); got != want {
			t.Errorf("Settled(%q) = %v, want %v", state, got, want)
		}
	}
}

func TestErrorClassification(t *testing.T) {
	notFound := &ResponseError{StatusCode: http.StatusNotFound, Body: "missing"}
	if !IsDeterministicRejection(notFound) {
		t.Fatal("404 must classify as deterministic rejection")
	}
	if IsDeterministicRejection(errors.New("network")) {
		t.Fatal("transport errors are not deterministic rejections")
	}
	if !IsAuthError(&ResponseError{StatusCode: http.StatusUnauthorized}) {
		t.Fatal("401 must classify as auth error")
	}
	if IsAuthError(notFound) {
		t.Fatal("404 is not an auth error")
	}
	if got := notFound.Error(); got != "local API returned HTTP 404: missing" {
		t.Fatalf("ResponseError text = %q", got)
	}
}

func TestLoadControlToken(t *testing.T) {
	dir := t.TempDir()
	if got := LoadControlToken(dir); got != "" {
		t.Fatalf("missing token file should load empty, got %q", got)
	}
	if err := os.WriteFile(filepath.Join(dir, ControlTokenFile), []byte(" secret-token\n"), 0o600); err != nil {
		t.Fatalf("write token: %v", err)
	}
	if got := LoadControlToken(dir); got != "secret-token" {
		t.Fatalf("token = %q", got)
	}
}

func TestLegacySendText(t *testing.T) {
	client := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/send" {
			t.Errorf("path = %q", r.URL.Path)
		}
		json.NewEncoder(w).Encode(map[string]any{"message_id": "m1", "status": "SUCCESS", "success": true})
	}))
	result, err := client.LegacySendText(context.Background(), "conv", "hi", "", "key")
	if err != nil {
		t.Fatalf("LegacySendText: %v", err)
	}
	if result.MessageID != "m1" || !result.Success {
		t.Fatalf("result = %+v", result)
	}
}

func TestDefaultBaseURLHonorsPortEnv(t *testing.T) {
	t.Setenv("OPENMESSAGES_PORT", "7100")
	if got := DefaultBaseURL(); got != "http://127.0.0.1:7100" {
		t.Fatalf("DefaultBaseURL() = %q", got)
	}
	t.Setenv("OPENMESSAGES_PORT", "")
	if got := DefaultBaseURL(); got != "http://127.0.0.1:7007" {
		t.Fatalf("DefaultBaseURL() = %q", got)
	}
}
