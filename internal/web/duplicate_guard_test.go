package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/messaging"
)

// postA3JSON submits an arbitrary JSON text submission, so tests can send the
// exact shapes each client produces.
func postA3JSON(t *testing.T, h *a3Harness, payload map[string]any) a3HTTPResponse {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return h.request(t, http.MethodPost, "/api/v1/outbox/messages", "application/json", body)
}

// uiTextPayload is exactly what the web UI's submitV2Text posts: no
// guard_near_duplicates and no force.
func uiTextPayload(key, body string) map[string]any {
	return map[string]any{
		"conversation_id": a3GoogleConversationID,
		"body":            body,
		"reply_to_id":     "",
		"idempotency_key": key,
	}
}

func guardedTextPayload(key, body string) map[string]any {
	payload := uiTextPayload(key, body)
	payload["guard_near_duplicates"] = true
	return payload
}

func a3PendingCount(t *testing.T, h *a3Harness) int {
	t.Helper()
	pending, err := h.service.ListPending(context.Background(), messaging.ListPendingQuery{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	return len(pending)
}

func TestHTTPDuplicateGuardIsOptIn(t *testing.T) {
	if httpDuplicateGuard(false) {
		t.Fatal("an HTTP submission without guard_near_duplicates is guarded (Variant A guards only opt-in submissions)")
	}
	if !httpDuplicateGuard(true) {
		t.Fatal("an HTTP submission with guard_near_duplicates is not guarded")
	}
}

// Variant A scope over HTTP: UI-shaped submissions are never blocked;
// submissions that ask for the guard are, with force as the override.
func TestV1SubmitGuardScope(t *testing.T) {
	h := newA3Harness(t, false)

	for pairIndex, pair := range [][2]string{{"ok", "ok"}, {"there at 7", "there at 8"}, {"on my way", "On my way"}} {
		for index, body := range pair {
			response := postA3JSON(t, h, uiTextPayload(fmt.Sprintf("ui-%d-%d", pairIndex, index), body))
			assertA3Status(t, response, http.StatusOK)
		}
	}
	accepted := a3PendingCount(t, h)
	if accepted != 6 {
		t.Fatalf("pending after UI repeats = %d, want 6", accepted)
	}

	// A guarded submission sees every recent intent, including the unguarded
	// UI ones, as a candidate.
	blocked := postA3JSON(t, h, guardedTextPayload("agent-ok", "ok"))
	assertA3Status(t, blocked, http.StatusConflict)
	if got := a3PendingCount(t, h); got != accepted {
		t.Fatalf("pending after refusal = %d, want %d", got, accepted)
	}

	forced := guardedTextPayload("agent-ok", "ok")
	forced["force"] = true
	assertA3Status(t, postA3JSON(t, h, forced), http.StatusOK)
	if got := a3PendingCount(t, h); got != accepted+1 {
		t.Fatalf("pending after forced resubmission = %d, want %d", got, accepted+1)
	}
}

func TestV1DuplicateConflictBody(t *testing.T) {
	h := newA3Harness(t, false)
	first := postA3JSON(t, h, guardedTextPayload("agent-first", "Lunch tomorrow at noon at Sfoglina?"))
	assertA3Status(t, first, http.StatusOK)
	prior := decodeA3JSON[v1SubmissionResponse](t, first.body)

	blocked := postA3JSON(t, h, guardedTextPayload("agent-second", "Lunch today at noon at Sfoglina?"))
	assertA3Status(t, blocked, http.StatusConflict)
	var body map[string]any
	if err := json.Unmarshal(blocked.body, &body); err != nil {
		t.Fatalf("decode 409 body %q: %v", blocked.body, err)
	}
	if got, _ := body["error"].(string); !strings.Contains(got, "near-duplicate") {
		t.Fatalf("error = %q, want the near-duplicate wording older clients match on", got)
	}
	if body["error_kind"] != NearDuplicateErrorKind {
		t.Fatalf("error_kind = %v, want %q", body["error_kind"], NearDuplicateErrorKind)
	}
	if body["duplicate_of_outbox_id"] != prior.OutboxID {
		t.Fatalf("duplicate_of_outbox_id = %v, want %q", body["duplicate_of_outbox_id"], prior.OutboxID)
	}
	if body["duplicate_state"] != string(messaging.OutboxQueued) {
		t.Fatalf("duplicate_state = %v, want queued", body["duplicate_state"])
	}
	if age, ok := body["duplicate_age_ms"].(float64); !ok || age < 0 {
		t.Fatalf("duplicate_age_ms = %v, want a non-negative number", body["duplicate_age_ms"])
	}

	// Replaying the accepted key never meets the guard: 200 with the original.
	replay := postA3JSON(t, h, guardedTextPayload("agent-first", "Lunch tomorrow at noon at Sfoglina?"))
	assertA3Status(t, replay, http.StatusOK)
	if got := decodeA3JSON[v1SubmissionResponse](t, replay.body); got.OutboxID != prior.OutboxID || !got.Deduplicated {
		t.Fatalf("replay = %+v, want deduplicated %q", got, prior.OutboxID)
	}

	// Other 409s keep their plain body.
	conflict := postA3JSON(t, h, guardedTextPayload("agent-first", "a changed body"))
	assertA3Status(t, conflict, http.StatusConflict)
	var conflictBody map[string]any
	if err := json.Unmarshal(conflict.body, &conflictBody); err != nil {
		t.Fatal(err)
	}
	if _, present := conflictBody["error_kind"]; present || strings.Contains(string(conflict.body), "near-duplicate") {
		t.Fatalf("idempotency conflict body = %s, want a plain error without the near-duplicate kind", conflict.body)
	}
}

// SendAgain is a deliberate resend and never guarded, even beside a recent
// guarded near-duplicate.
func TestV1SendAgainNotGuarded(t *testing.T) {
	h := newA3Harness(t, true, a3SendStep{err: bridge.OpError{
		Class:     bridge.FailureTransient,
		Operation: "send_text",
		Dispatch:  bridge.DispatchUncertain,
		Cause:     context.DeadlineExceeded,
	}})
	first := postA3JSON(t, h, guardedTextPayload("agent-uncertain", "maybe delivered"))
	assertA3Status(t, first, http.StatusOK)
	original := decodeA3JSON[v1SubmissionResponse](t, first.body)
	if processed, err := h.service.DispatchDue(context.Background(), 10); err != nil || processed != 1 {
		t.Fatalf("DispatchDue() = %d, %v; want 1, nil", processed, err)
	}
	h.registry.setOnline(false)

	resent := h.request(t, http.MethodPost, "/api/v1/outbox/"+original.OutboxID+"/send-again",
		"application/json", []byte(`{"idempotency_key":"send-again-key"}`))
	assertA3Status(t, resent, http.StatusOK)
}

// Media is never guarded: a media send whose caption repeats a recent
// guarded text is accepted.
func TestV1MediaNotGuarded(t *testing.T) {
	h := newA3Harness(t, false)
	assertA3Status(t, postA3JSON(t, h, guardedTextPayload("agent-text", "look at this")), http.StatusOK)

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for field, value := range map[string]string{
		"conversation_id":       a3GoogleConversationID,
		"idempotency_key":       "media-key",
		"caption":               "look at this",
		"guard_near_duplicates": "true",
	} {
		if err := writer.WriteField(field, value); err != nil {
			t.Fatal(err)
		}
	}
	part, err := writer.CreateFormFile("file", "photo.png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte("png bytes")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	response := h.request(t, http.MethodPost, "/api/v1/outbox/media", writer.FormDataContentType(), body.Bytes())
	assertA3Status(t, response, http.StatusOK)
}

// Variant A pin: the web UI never opts into the guard and never forces, so a
// person's sends are never refused as near-duplicates. Variant B would flip
// this together with httpDuplicateGuard and add a "Send anyway" override.
func TestWebUINeverRequestsNearDuplicateGuard(t *testing.T) {
	page, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(page, []byte("guard_near_duplicates")) {
		t.Fatal("index.html sends guard_near_duplicates; under Variant A the UI must stay unguarded")
	}
	if match := regexp.MustCompile(`\bforce\s*:|\.force\b|['"]force['"]`).Find(page); match != nil {
		t.Fatalf("index.html sets a force field (%q); the UI has no near-duplicate override under Variant A", match)
	}
}
