package web

// The v1 delivery and pending JSON carry why a send is not going out
// (error_detail), how many budget-consuming attempts it made, when it retries
// next, and whether the dispatcher gave up (retry_exhausted). The stuck-send
// incident of 2026-10-07 showed only "error class transient" for hours.

import (
	"encoding/json"
	"math/rand"
	"reflect"
	"testing"
	"testing/quick"
	"time"

	"github.com/maxghenis/openmessage/internal/localapi"
	"github.com/maxghenis/openmessage/internal/messaging"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

var v1AllOutboxStates = []messaging.OutboxState{
	messaging.OutboxQueued,
	messaging.OutboxDispatching,
	messaging.OutboxNotDispatched,
	messaging.OutboxUncertain,
	messaging.OutboxConfirmed,
	messaging.OutboxStoreFailed,
	messaging.OutboxRejected,
	messaging.OutboxCanceled,
}

var v1ErrorClasses = []string{
	"",
	sqlite.RetryExhaustedErrorClass,
	"reauth_required",
	"transient",
	"credentials_expired",
	"misconfigured",
	"permanent",
}

func decodeJSONMap(t *testing.T, value any) map[string]any {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	decoded := map[string]any{}
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return decoded
}

func TestV1DeliveryResponseCarriesFailureDetail(t *testing.T) {
	next := time.UnixMilli(1_760_000_000_123)
	got := decodeJSONMap(t, deliveryResponse(messaging.Delivery{
		OutboxID:       "outbox-1",
		State:          messaging.OutboxRejected,
		ErrorClass:     sqlite.RetryExhaustedErrorClass,
		ErrorCode:      "send_text",
		ErrorDetail:    "retry budget exhausted after 6 attempts; last failure transient [google_conversation_not_found]: no conversation",
		AttemptCount:   6,
		NextAttemptAt:  next,
		RetryExhausted: true,
	}))
	want := map[string]any{
		"outbox_id":          "outbox-1",
		"state":              "rejected",
		"error_class":        "retry_exhausted",
		"error_code":         "send_text",
		"error_detail":       "retry budget exhausted after 6 attempts; last failure transient [google_conversation_not_found]: no conversation",
		"attempt_count":      float64(6),
		"next_attempt_at_ms": float64(next.UnixMilli()),
		"retry_exhausted":    true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("delivery JSON = %#v\nwant %#v", got, want)
	}

	// A confirmed delivery keeps its old shape: the new keys are omitempty.
	confirmed := decodeJSONMap(t, deliveryResponse(messaging.Delivery{
		OutboxID:        "outbox-2",
		State:           messaging.OutboxConfirmed,
		RemoteMessageID: "remote-2",
	}))
	for _, key := range []string{"error_detail", "attempt_count", "next_attempt_at_ms", "retry_exhausted"} {
		if _, present := confirmed[key]; present {
			t.Fatalf("confirmed delivery JSON carries %q: %#v", key, confirmed)
		}
	}
}

func TestV1PendingResponseCarriesFailureDetail(t *testing.T) {
	got := decodeJSONMap(t, pendingResponse(messaging.PendingDelivery{
		OutboxID:       "outbox-1",
		AccountID:      "google-primary",
		ConversationID: "conversation-1",
		Kind:           sqlite.OutboxKindText,
		State:          messaging.OutboxRejected,
		AttemptCount:   6,
		ErrorClass:     sqlite.RetryExhaustedErrorClass,
		ErrorCode:      "send_text",
		ErrorDetail:    "retry budget exhausted after 6 attempts",
		RetryExhausted: true,
	}))
	if got["error_detail"] != "retry budget exhausted after 6 attempts" || got["retry_exhausted"] != true {
		t.Fatalf("pending JSON = %#v", got)
	}

	plain := decodeJSONMap(t, pendingResponse(messaging.PendingDelivery{
		OutboxID: "outbox-2",
		Kind:     sqlite.OutboxKindText,
		State:    messaging.OutboxQueued,
	}))
	for _, key := range []string{"error_detail", "retry_exhausted"} {
		if _, present := plain[key]; present {
			t.Fatalf("queued pending JSON carries %q: %#v", key, plain)
		}
	}
	// attempt_count stays always-present on the pending shape (pinned by the
	// A3 contract test); only the new keys are omitempty.
	if _, present := plain["attempt_count"]; !present {
		t.Fatalf("pending JSON dropped attempt_count: %#v", plain)
	}
}

// TestV1RetryExhaustedPassesThrough is exhaustive over state x class x flag:
// the delivery and pending JSON report exactly the service's RetryExhausted,
// whatever the state and class, so the API never derives or rewrites it. It
// does not test the flag's definition (rejected and class retry_exhausted,
// DESIGN I8); the messaging service owns that
// (TestDeliveryFromItemRetryExhaustedAndNextAttemptExhaustive).
func TestV1RetryExhaustedPassesThrough(t *testing.T) {
	for _, state := range v1AllOutboxStates {
		for _, class := range v1ErrorClasses {
			for _, want := range []bool{false, true} {
				delivery := decodeJSONMap(t, deliveryResponse(messaging.Delivery{
					OutboxID:       "outbox",
					State:          state,
					ErrorClass:     class,
					RetryExhausted: want,
				}))
				if got := delivery["retry_exhausted"] == true; got != want {
					t.Fatalf("delivery state=%s class=%q: retry_exhausted=%v, want %v", state, class, got, want)
				}
				pending := decodeJSONMap(t, pendingResponse(messaging.PendingDelivery{
					OutboxID:       "outbox",
					State:          state,
					ErrorClass:     class,
					RetryExhausted: want,
				}))
				if got := pending["retry_exhausted"] == true; got != want {
					t.Fatalf("pending state=%s class=%q: retry_exhausted=%v, want %v", state, class, got, want)
				}
			}
		}
	}
}

// v1QuickDelivery generates deliveries with millisecond-precision retry times
// (the wire carries milliseconds).
type v1QuickDelivery struct{ messaging.Delivery }

func (v1QuickDelivery) Generate(r *rand.Rand, size int) reflect.Value {
	text := func() string {
		value, _ := quick.Value(reflect.TypeOf(""), r)
		return value.String()
	}
	delivery := messaging.Delivery{
		OutboxID:        text(),
		State:           v1AllOutboxStates[r.Intn(len(v1AllOutboxStates))],
		LocalMessageID:  text(),
		RemoteMessageID: text(),
		ErrorClass:      v1ErrorClasses[r.Intn(len(v1ErrorClasses))],
		ErrorCode:       text(),
		ErrorDetail:     text(),
		AttemptCount:    r.Int63n(1 << 40),
		RetryExhausted:  r.Intn(2) == 0,
		Warning:         text(),
	}
	if r.Intn(4) != 0 {
		delivery.NextAttemptAt = time.UnixMilli(1 + r.Int63n(1<<45))
	}
	return reflect.ValueOf(v1QuickDelivery{delivery})
}

// TestV1DeliveryJSONRoundTripsThroughLocalAPI is the API half of the daemon
// round trip (internal/tools checks localapi.Delivery -> messaging.Delivery):
// every delivery field the CLI and transportless MCP read survives the v1
// JSON encoding into localapi.Delivery unchanged.
func TestV1DeliveryJSONRoundTripsThroughLocalAPI(t *testing.T) {
	property := func(generated v1QuickDelivery) bool {
		in := generated.Delivery
		encoded, err := json.Marshal(deliveryResponse(in))
		if err != nil {
			return false
		}
		var out localapi.Delivery
		if err := json.Unmarshal(encoded, &out); err != nil {
			return false
		}
		wantNext := int64(0)
		if !in.NextAttemptAt.IsZero() {
			wantNext = in.NextAttemptAt.UnixMilli()
		}
		return out == localapi.Delivery{
			OutboxID:        in.OutboxID,
			State:           string(in.State),
			LocalMessageID:  in.LocalMessageID,
			RemoteMessageID: in.RemoteMessageID,
			ErrorClass:      in.ErrorClass,
			ErrorCode:       in.ErrorCode,
			ErrorDetail:     in.ErrorDetail,
			AttemptCount:    in.AttemptCount,
			NextAttemptAtMS: wantNext,
			RetryExhausted:  in.RetryExhausted,
			Warning:         in.Warning,
		}
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 500}); err != nil {
		t.Fatal(err)
	}
}
