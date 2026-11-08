package localapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

	// The freshness block stays raw so a caller can show the daemon's
	// silence verdict unchanged.
	t.Run("daemon reports freshness", func(t *testing.T) {
		const silence = `{"silent_ms":46800000,"stalled":true,"rule":"expected_activity"}`
		client := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, `{"freshness":{"newest_ms":1791473060776,"google":{"stale":true,"silence":`+silence+`}}}`)
		}))
		status, _, err := client.Status(context.Background())
		if err != nil {
			t.Fatalf("Status() = %v", err)
		}
		var freshness struct {
			NewestMS json.RawMessage `json:"newest_ms"`
			Google   struct {
				Silence json.RawMessage `json:"silence"`
			} `json:"google"`
		}
		if err := json.Unmarshal(status.Freshness, &freshness); err != nil {
			t.Fatalf("decode freshness: %v", err)
		}
		if string(freshness.NewestMS) != "1791473060776" || string(freshness.Google.Silence) != silence {
			t.Fatalf("freshness = %s, want newest_ms 1791473060776 and google.silence %s", status.Freshness, silence)
		}
	})

	// The send and MCP paths read only the mode fields, so a freshness block
	// of any JSON shape must not turn a reachable daemon into a probe failure.
	t.Run("daemon reports a non-object freshness", func(t *testing.T) {
		for _, freshness := range []string{`[]`, `"stale"`, `7`, `true`, `null`} {
			client := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, `{"v2_primary":true,"freshness":`+freshness+`}`)
			}))
			status, reachable, err := client.Status(context.Background())
			if err != nil || !reachable || !status.V2Primary {
				t.Fatalf("freshness %s: Status() = %+v, reachable %v, err %v", freshness, status, reachable, err)
			}
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

// The app refuses a reply target with HTTP 422 and
// {"error":"reply_target_unavailable"}; only that answer classifies as one.
func TestIsReplyTargetUnavailable(t *testing.T) {
	refusal := func(status int, body string) error {
		return &ResponseError{StatusCode: status, Body: body}
	}
	for name, test := range map[string]struct {
		err  error
		want bool
	}{
		"the app's refusal":            {refusal(http.StatusUnprocessableEntity, `{"error":"reply_target_unavailable"}`+"\n"), true},
		"wrapped":                      {fmt.Errorf("submit: %w", refusal(http.StatusUnprocessableEntity, `{"error":"reply_target_unavailable"}`)), true},
		"another 422":                  {refusal(http.StatusUnprocessableEntity, `{"error":"reaction_target_unavailable"}`), false},
		"the code with another status": {refusal(http.StatusNotFound, `{"error":"reply_target_unavailable"}`), false},
		"a plain-text body":            {refusal(http.StatusUnprocessableEntity, "reply_target_unavailable"), false},
		"a transport error":            {errors.New("reply_target_unavailable"), false},
		"no error":                     {nil, false},
	} {
		if got := IsReplyTargetUnavailable(test.err); got != test.want {
			t.Errorf("%s: IsReplyTargetUnavailable(%v) = %v, want %v", name, test.err, got, test.want)
		}
	}

	client := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request TextSubmission
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode: %v", err)
		}
		if request.ReplyToID != "msg-elsewhere" {
			t.Errorf("reply_to_id = %q, want msg-elsewhere", request.ReplyToID)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		json.NewEncoder(w).Encode(map[string]string{"error": "reply_target_unavailable"})
	}))
	_, err := client.SubmitText(context.Background(), TextSubmission{
		ConversationID: "conv", Body: "hi", ReplyToID: "msg-elsewhere", IdempotencyKey: "key",
	})
	if !IsReplyTargetUnavailable(err) || !IsDeterministicRejection(err) {
		t.Fatalf("SubmitText error %v: want a deterministic reply-target refusal", err)
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

func TestReactionsViaOutboxNeedsV2Primary(t *testing.T) {
	for _, status := range []DaemonStatus{
		{}, {V2Send: true}, {V2Primary: true}, {V2Send: true, V2Primary: true},
	} {
		if got := status.ReactionsViaOutbox(); got != status.V2Primary {
			t.Fatalf("%+v.ReactionsViaOutbox() = %v, want %v", status, got, status.V2Primary)
		}
	}
}

func TestSubmitReactionPostsToTheOutbox(t *testing.T) {
	var path string
	var body map[string]any
	client := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.Method + " " + r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode: %v", err)
		}
		json.NewEncoder(w).Encode(map[string]any{"outbox_id": "out", "state": "queued", "deduplicated": true})
	}))
	submission, err := client.SubmitReaction(context.Background(), ReactionSubmission{
		MessageID: "msg", Emoji: "👍", IdempotencyKey: "key",
	})
	if err != nil || submission.OutboxID != "out" || !submission.Deduplicated {
		t.Fatalf("SubmitReaction() = %+v, %v", submission, err)
	}
	if path != "POST /api/v1/outbox/reactions" {
		t.Fatalf("request = %s", path)
	}
	// conversation_id and action are optional and omitted when empty.
	if len(body) != 3 || body["message_id"] != "msg" || body["emoji"] != "👍" || body["idempotency_key"] != "key" {
		t.Fatalf("body = %v", body)
	}

	empty := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"state": "queued"})
	}))
	if _, err := empty.SubmitReaction(context.Background(), ReactionSubmission{MessageID: "m", Emoji: "👍", IdempotencyKey: "k"}); err == nil {
		t.Fatal("expected an error for a response without outbox_id")
	}
}

// SetTranscript keeps the legacy store's rules on the wire: a nil model is
// left out so the daemon keeps the stored one, an empty model is sent so the
// daemon clears it, and an empty transcript is sent rather than dropped.
func TestSetTranscriptKeepsTheModelRulesOnTheWire(t *testing.T) {
	var mu sync.Mutex
	var bodies []map[string]any
	client := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method != http.MethodPost || r.URL.Path != "/api/transcript" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer secret" {
			t.Errorf("Authorization = %q", got)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode: %v", err)
		}
		bodies = append(bodies, body)
		answer := map[string]any{"success": true, "message_id": body["message_id"], "transcript_length": 2}
		if len(bodies) == 1 {
			answer["transcript_model"] = "whisper"
		}
		json.NewEncoder(w).Encode(answer)
	}))
	client.Token = "secret"

	empty := ""
	saved, err := client.SetTranscript(context.Background(), "msg", "hi", nil)
	if err != nil {
		t.Fatalf("SetTranscript(nil model): %v", err)
	}
	if !saved.Success || saved.MessageID != "msg" || saved.TranscriptLength != 2 || saved.TranscriptModel == nil || *saved.TranscriptModel != "whisper" {
		t.Fatalf("SetTranscript(nil model) = %+v", saved)
	}
	older, err := client.SetTranscript(context.Background(), "msg", "", &empty)
	if err != nil {
		t.Fatalf("SetTranscript(empty model): %v", err)
	}
	if older.TranscriptModel != nil {
		t.Fatalf("an answer without transcript_model decoded as %q, want nil", *older.TranscriptModel)
	}

	mu.Lock()
	defer mu.Unlock()
	if _, sent := bodies[0]["model"]; sent {
		t.Fatalf("a nil model was sent: %v", bodies[0])
	}
	if model, sent := bodies[1]["model"]; !sent || model != "" {
		t.Fatalf("an empty model was not sent as \"\": %v", bodies[1])
	}
	if transcript, sent := bodies[1]["transcript"]; !sent || transcript != "" {
		t.Fatalf("an empty transcript was not sent as \"\": %v", bodies[1])
	}

	refused := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]any{"error": "transcripts are not available while v2 is the serving store"})
	}))
	if _, err := refused.SetTranscript(context.Background(), "msg", "hi", nil); err == nil {
		t.Fatal("expected a ResponseError for a refused transcript")
	} else if responseErr, ok := AsResponseError(err); !ok || responseErr.StatusCode != http.StatusConflict {
		t.Fatalf("SetTranscript() error = %v", err)
	}
}

func TestReactDecodesTheQueuedAnswer(t *testing.T) {
	client := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(map[string]any{"success": true, "queued": true, "outbox_id": "out", "state": "not_dispatched"})
	}))
	result, err := client.React(context.Background(), "conv", "msg", "👍", "add", "")
	if err != nil || result != (ReactResult{Success: true, Queued: true, OutboxID: "out", State: "not_dispatched"}) {
		t.Fatalf("React() = %+v, %v", result, err)
	}

	refused := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		json.NewEncoder(w).Encode(map[string]any{"success": false, "error": "send reaction: the transport refused the reaction"})
	}))
	if _, err := refused.React(context.Background(), "conv", "msg", "👍", "add", ""); err == nil {
		t.Fatal("expected a ResponseError for a refused reaction")
	} else if responseErr, ok := AsResponseError(err); !ok || responseErr.StatusCode != http.StatusBadGateway {
		t.Fatalf("React() error = %v", err)
	}

	// A v2-primary daemon that stored the reaction and then rejected it says
	// why; the caller gets that alongside the error.
	rejected := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		json.NewEncoder(w).Encode(map[string]any{
			"success": false, "outbox_id": "out-rejected", "state": "rejected",
			"error_class": "recipient_mismatch", "error_code": "send_reaction",
			"error": "send reaction: the phone's thread for this conversation now names other people, so the app sent nothing",
		})
	}))
	stored, err := rejected.React(context.Background(), "conv", "msg", "👍", "add", "")
	if _, ok := AsResponseError(err); !ok ||
		stored != (ReactResult{OutboxID: "out-rejected", State: "rejected", ErrorClass: "recipient_mismatch", ErrorCode: "send_reaction"}) {
		t.Fatalf("React(rejected) = %+v, %v; want the stored intent with its error class and code", stored, err)
	}
}

func TestMessageAttachmentReadsBytesHeadersAndLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != "/api/v1/messages/msg%2F1/attachments/3" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", `attachment; filename="Report.html"`)
		w.Header().Set("X-Content-SHA256", "ABCDEF")
		_, _ = w.Write([]byte("0123456789"))
	}))
	defer server.Close()
	client := NewClient(server.URL, "")

	attachment, err := client.MessageAttachment(context.Background(), "msg/1", 3, 10)
	if err != nil {
		t.Fatalf("MessageAttachment(): %v", err)
	}
	if string(attachment.Data) != "0123456789" || attachment.Filename != "Report.html" ||
		attachment.MIME != "application/octet-stream" || attachment.SHA256 != "abcdef" {
		t.Fatalf("MessageAttachment() = %+v", attachment)
	}
	if _, err := client.MessageAttachment(context.Background(), "msg/1", 3, 9); err == nil || !strings.Contains(err.Error(), "exceeds 9 bytes") {
		t.Fatalf("MessageAttachment(limit 9) error = %v, want the size limit", err)
	}
	if _, err := client.MessageAttachment(context.Background(), "other", 0, 10); !IsDeterministicRejection(err) {
		t.Fatalf("MessageAttachment(unknown) error = %v, want a 404 ResponseError", err)
	}
	if got := client.attachmentHTTP().Timeout; got < attachmentTimeout {
		t.Fatalf("attachment timeout = %v, want at least %v", got, attachmentTimeout)
	}
	if client.HTTP.Timeout != 10*time.Second {
		t.Fatalf("attachment requests changed the client's default timeout to %v", client.HTTP.Timeout)
	}
}
