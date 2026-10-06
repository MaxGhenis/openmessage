package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
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

// uiTextPayload is exactly what the web UI's submitV2Text posts for an
// ordinary send: no guard_near_duplicates and no force ("Send anyway" adds
// force:true under the same idempotency key).
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

func TestHTTPDuplicateGuardCoversEverySubmission(t *testing.T) {
	for _, requested := range []bool{false, true} {
		if !httpDuplicateGuard(requested) {
			t.Fatalf("httpDuplicateGuard(%v) = false; Variant B guards every HTTP text submission", requested)
		}
	}
}

// Variant B scope over HTTP: every text submission is guarded, the web UI's
// request shape included, and force is the only override. The UI's "Send
// anyway" resubmits the refused idempotency key with force.
func TestV1SubmitGuardScope(t *testing.T) {
	h := newA3Harness(t, false)

	for pairIndex, pair := range [][2]string{{"ok", "ok"}, {"there at 7", "there at 8"}, {"on my way", "On my way"}} {
		firstKey := fmt.Sprintf("ui-%d-first", pairIndex)
		repeatKey := fmt.Sprintf("ui-%d-repeat", pairIndex)
		first := postA3JSON(t, h, uiTextPayload(firstKey, pair[0]))
		assertA3Status(t, first, http.StatusOK)
		prior := decodeA3JSON[v1SubmissionResponse](t, first.body)
		before := a3PendingCount(t, h)

		blocked := postA3JSON(t, h, uiTextPayload(repeatKey, pair[1]))
		assertA3Status(t, blocked, http.StatusConflict)
		var body map[string]any
		if err := json.Unmarshal(blocked.body, &body); err != nil {
			t.Fatalf("decode 409 body %q: %v", blocked.body, err)
		}
		if body["error_kind"] != NearDuplicateErrorKind || body["duplicate_of_outbox_id"] != prior.OutboxID {
			t.Fatalf("UI repeat %q after %q: 409 body = %s, want error_kind %q naming %q",
				pair[1], pair[0], blocked.body, NearDuplicateErrorKind, prior.OutboxID)
		}
		if got := a3PendingCount(t, h); got != before {
			t.Fatalf("pending after refusal = %d, want %d", got, before)
		}

		// Clearing guard_near_duplicates is not an opt-out.
		optOut := uiTextPayload(repeatKey, pair[1])
		optOut["guard_near_duplicates"] = false
		assertA3Status(t, postA3JSON(t, h, optOut), http.StatusConflict)
		if got := a3PendingCount(t, h); got != before {
			t.Fatalf("pending after guard_near_duplicates:false = %d, want %d", got, before)
		}

		// "Send anyway": the refused key with force is a new intent (the
		// refusal wrote nothing under that key).
		forced := uiTextPayload(repeatKey, pair[1])
		forced["force"] = true
		accepted := postA3JSON(t, h, forced)
		assertA3Status(t, accepted, http.StatusOK)
		sendAnyway := decodeA3JSON[v1SubmissionResponse](t, accepted.body)
		if sendAnyway.OutboxID == prior.OutboxID || sendAnyway.Deduplicated {
			t.Fatalf("forced resubmission = %+v, want a new intent beside %q", sendAnyway, prior.OutboxID)
		}
		if got := a3PendingCount(t, h); got != before+1 {
			t.Fatalf("pending after forced resubmission = %d, want %d", got, before+1)
		}

		// A later unforced replay of that key (a retried UI flush) resolves
		// to the forced intent and never meets the guard.
		replay := postA3JSON(t, h, uiTextPayload(repeatKey, pair[1]))
		assertA3Status(t, replay, http.StatusOK)
		if got := decodeA3JSON[v1SubmissionResponse](t, replay.body); got.OutboxID != sendAnyway.OutboxID || !got.Deduplicated {
			t.Fatalf("unforced replay of the forced key = %+v, want deduplicated %q", got, sendAnyway.OutboxID)
		}
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

// Variant B pin on the embedded UI. The daemon decides the guard's scope, so
// the UI never sends guard_near_duplicates; it must recognize the structured
// 409 by this package's error_kind (the UI is embedded in the same binary, so
// the two cannot skew) and offer the "Send anyway" override. e2e/ui.spec.js
// drives the override itself.
func TestWebUIHandlesNearDuplicateRefusal(t *testing.T) {
	page, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(page, []byte("guard_near_duplicates")) {
		t.Fatal("index.html sends guard_near_duplicates; the daemon guards every text submission, so the UI must not depend on the opt-in")
	}
	if !bytes.Contains(page, []byte("'"+NearDuplicateErrorKind+"'")) {
		t.Fatalf("index.html does not match error_kind %q; a near-duplicate refusal would show as a generic conflict with no override", NearDuplicateErrorKind)
	}
	for _, marker := range []string{"msg-send-anyway", "send-anyway-queued-send", "payload.force = true"} {
		if !bytes.Contains(page, []byte(marker)) {
			t.Fatalf("index.html lacks %q; the UI has no near-duplicate override", marker)
		}
	}
}
