package web

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/messaging"
)

// TestReactOutcomeAnswersEveryState is the contract of POST /api/react on a
// v2-primary daemon, checked over every outbox state and over a state this
// build does not know:
//
//   - the answer is 200, 202, 409 or 502, and always names the intent;
//   - success is true exactly on 200 and 202;
//   - 200 means the transport accepted the reaction, and nothing else does;
//   - 202 means stored and still the app's to send, and is the only answer
//     marked queued;
//   - an error is given exactly when the intent will not deliver the reaction.
func TestReactOutcomeAnswersEveryState(t *testing.T) {
	want := map[messaging.OutboxState]int{
		messaging.OutboxQueued:        http.StatusAccepted,
		messaging.OutboxDispatching:   http.StatusAccepted,
		messaging.OutboxNotDispatched: http.StatusAccepted,
		messaging.OutboxConfirmed:     http.StatusOK,
		messaging.OutboxStoreFailed:   http.StatusOK,
		messaging.OutboxUncertain:     http.StatusBadGateway,
		messaging.OutboxRejected:      http.StatusBadGateway,
		messaging.OutboxCanceled:      http.StatusConflict,
		"a-state-added-later":         http.StatusAccepted,
	}
	for state, wantStatus := range want {
		t.Run(string(state), func(t *testing.T) {
			status, body := reactOutcome(messaging.Delivery{
				OutboxID: "outbox-1", State: state, ErrorClass: "transient", ErrorCode: "send_reaction",
			})
			if status != wantStatus {
				t.Fatalf("status = %d, want %d", status, wantStatus)
			}
			if body["outbox_id"] != "outbox-1" || body["state"] != state ||
				body["error_class"] != "transient" || body["error_code"] != "send_reaction" {
				t.Fatalf("body = %v, want the intent and its error named", body)
			}
			if success, _ := body["success"].(bool); success != (status < 300) {
				t.Fatalf("success = %v on %d", body["success"], status)
			}
			if _, queued := body["queued"]; queued != (status == http.StatusAccepted) {
				t.Fatalf("queued present = %v on %d", queued, status)
			}
			message, failed := body["error"].(string)
			if failed != (status >= 400) || (failed && !strings.HasPrefix(message, "send reaction: ")) {
				t.Fatalf("error = %q on %d", message, status)
			}
			if _, err := json.Marshal(body); err != nil {
				t.Fatalf("body does not encode: %v", err)
			}
		})
	}
}

func postJSONTo(t *testing.T, handler http.Handler, path, body string) (int, map[string]any) {
	t.Helper()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "http://127.0.0.1"+path, strings.NewReader(body)))
	raw, _ := io.ReadAll(recorder.Body)
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("POST %s = %d with a body that is not a JSON object: %s", path, recorder.Code, raw)
	}
	return recorder.Code, decoded
}

// The outbox reaction route addresses messages by v2 ID, which only a
// v2-primary daemon hands out.
func TestV1ReactionRouteServesV2PrimaryOnly(t *testing.T) {
	harness := newA3Harness(t, true)
	status, body := postJSONTo(t, harness.handler, "/api/v1/outbox/reactions",
		`{"message_id":"m","emoji":"👍","idempotency_key":"k"}`)
	if status != http.StatusConflict || body["error"] != "legacy primary: use /api/react" {
		t.Fatalf("v2 send without v2 primary = %d %v, want 409 naming /api/react", status, body)
	}
}

func TestV1ReactionRouteValidatesTheRequest(t *testing.T) {
	harness := newA3Harness(t, true)
	handler := APIHandlerWithOptions(harness.legacy, nil, zerolog.Nop(), nil, APIOptions{
		V2Primary: true,
		V2:        &V2Options{Service: harness.service, V2Store: harness.v2, Registry: harness.registry},
	})
	for name, request := range map[string]struct {
		body       string
		wantStatus int
		wantError  string
	}{
		"not JSON":           {`{`, http.StatusBadRequest, ""},
		"no message":         {`{"emoji":"👍","idempotency_key":"k"}`, http.StatusBadRequest, "message_id and emoji are required"},
		"no emoji":           {`{"message_id":"m","emoji":"  ","idempotency_key":"k"}`, http.StatusBadRequest, "message_id and emoji are required"},
		"no idempotency key": {`{"message_id":"m","emoji":"👍"}`, http.StatusBadRequest, "idempotency_key is required"},
		"an unusable key":    {`{"message_id":"m","emoji":"👍","idempotency_key":"a b"}`, http.StatusBadRequest, "idempotency_key contains unsupported characters"},
		"an unknown message": {`{"message_id":"m","emoji":"👍","idempotency_key":"k"}`, http.StatusUnprocessableEntity, "reaction_target_unavailable"},
		"an unknown action":  {`{"message_id":"m","emoji":"👍","action":"toggle","idempotency_key":"k"}`, http.StatusBadRequest, ""},
	} {
		t.Run(name, func(t *testing.T) {
			status, body := postJSONTo(t, handler, "/api/v1/outbox/reactions", request.body)
			if status != request.wantStatus || (request.wantError != "" && body["error"] != request.wantError) {
				t.Fatalf("POST = %d %v, want %d %q", status, body, request.wantStatus, request.wantError)
			}
		})
	}

	// /api/react applies the same rules, except that it mints a key when the
	// caller gives none.
	for name, request := range map[string]struct {
		body       string
		wantStatus int
		wantError  string
	}{
		"an unusable key":    {`{"message_id":"m","emoji":"👍","idempotency_key":"a b"}`, http.StatusBadRequest, "idempotency_key contains unsupported characters"},
		"an unknown message": {`{"message_id":"m","emoji":"👍"}`, http.StatusUnprocessableEntity, "reaction_target_unavailable"},
	} {
		t.Run("/api/react "+name, func(t *testing.T) {
			status, body := postJSONTo(t, handler, "/api/react", request.body)
			if status != request.wantStatus || body["error"] != request.wantError {
				t.Fatalf("POST = %d %v, want %d %q", status, body, request.wantStatus, request.wantError)
			}
		})
	}
	pending, err := harness.service.ListPending(t.Context(), messaging.ListPendingQuery{Limit: 10})
	if err != nil || len(pending) != 0 {
		t.Fatalf("refused reactions left outbox rows: %+v, %v", pending, err)
	}
}

// With v2 send enabled but the legacy store still primary, the read API hands
// out legacy IDs, so /api/react must keep using the legacy senders and never
// touch the outbox.
func TestReactStaysOnLegacySendersUntilV2IsPrimary(t *testing.T) {
	harness := newA3Harness(t, true)
	var calls []string
	handler := APIHandlerWithOptions(harness.legacy, nil, zerolog.Nop(), nil, APIOptions{
		V2: &V2Options{Service: harness.service, V2Store: harness.v2, Registry: harness.registry},
		SendSignalReaction: func(conversationID, messageID, emoji, action string) error {
			calls = append(calls, conversationID+"|"+messageID+"|"+emoji+"|"+action)
			return nil
		},
	})
	if err := harness.legacy.UpsertConversation(&db.Conversation{
		ConversationID: "signal:+15551234567", Name: "Taylor Price", SourcePlatform: "signal",
	}); err != nil {
		t.Fatal(err)
	}
	status, body := postJSONTo(t, handler, "/api/react",
		`{"conversation_id":"signal:+15551234567","message_id":"signal:target","emoji":"😂","action":"add"}`)
	if status != http.StatusOK || body["success"] != true || len(body) != 1 {
		t.Fatalf("POST /api/react = %d %v, want the legacy answer {success:true}", status, body)
	}
	if len(calls) != 1 || calls[0] != "signal:+15551234567|signal:target|😂|add" {
		t.Fatalf("legacy Signal sender calls = %v", calls)
	}
	pending, err := harness.service.ListPending(t.Context(), messaging.ListPendingQuery{Limit: 10})
	if err != nil || len(pending) != 0 {
		t.Fatalf("a legacy-primary reaction reached the outbox: %+v, %v", pending, err)
	}
}
