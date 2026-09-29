package localapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSubmitTextSendsTTLAndForce(t *testing.T) {
	var received map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/outbox/messages" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		json.NewEncoder(w).Encode(map[string]any{"outbox_id": "outbox-1", "state": "queued"})
	}))
	t.Cleanup(server.Close)

	client := NewClient(server.URL, "")
	ttlMS := int64(600_000)
	if _, err := client.SubmitText(context.Background(), TextSubmission{
		ConversationID:      "conversation-1",
		Body:                "windowed",
		IdempotencyKey:      "key-1",
		TTLMS:               &ttlMS,
		GuardNearDuplicates: true,
		Force:               true,
	}); err != nil {
		t.Fatalf("SubmitText(): %v", err)
	}
	if got, _ := received["ttl_ms"].(float64); int64(got) != ttlMS {
		t.Fatalf("ttl_ms = %v, want %d", received["ttl_ms"], ttlMS)
	}
	if got, _ := received["force"].(bool); !got {
		t.Fatalf("force = %v, want true", received["force"])
	}
	if got, _ := received["guard_near_duplicates"].(bool); !got {
		t.Fatalf("guard_near_duplicates = %v, want true", received["guard_near_duplicates"])
	}

	// Unset flags are omitted, so a daemon applies its own defaults.
	received = nil
	if _, err := client.SubmitText(context.Background(), TextSubmission{
		ConversationID: "conversation-1",
		Body:           "plain",
		IdempotencyKey: "key-2",
	}); err != nil {
		t.Fatalf("SubmitText(plain): %v", err)
	}
	for _, field := range []string{"guard_near_duplicates", "force", "ttl_ms"} {
		if _, present := received[field]; present {
			t.Fatalf("%s present in %v, want omitted", field, received)
		}
	}
}

func TestAsNearDuplicateRejection(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want NearDuplicateRejection
		ok   bool
	}{
		{
			name: "structured 409",
			err:  &ResponseError{StatusCode: http.StatusConflict, Body: `{"error":"messaging: near-duplicate send blocked: …","error_kind":"near_duplicate_blocked","duplicate_of_outbox_id":"outbox-prior","duplicate_state":"queued","duplicate_age_ms":120000}`},
			want: NearDuplicateRejection{
				Message:             "messaging: near-duplicate send blocked: …",
				Structured:          true,
				DuplicateOfOutboxID: "outbox-prior",
				DuplicateState:      "queued",
				DuplicateAgeMS:      120_000,
			},
			ok: true,
		},
		{
			name: "older daemon plain JSON 409",
			err:  &ResponseError{StatusCode: http.StatusConflict, Body: `{"error":"messaging: near-duplicate send blocked: a very similar message…"}`},
			want: NearDuplicateRejection{Message: "messaging: near-duplicate send blocked: a very similar message…"},
			ok:   true,
		},
		{
			name: "plain text 409",
			err:  &ResponseError{StatusCode: http.StatusConflict, Body: "near-duplicate send blocked"},
			want: NearDuplicateRejection{Message: "near-duplicate send blocked"},
			ok:   true,
		},
		{
			name: "error_kind is authoritative",
			err:  &ResponseError{StatusCode: http.StatusConflict, Body: `{"error":"mentions near-duplicate","error_kind":"idempotency_conflict"}`},
		},
		{
			name: "idempotency conflict",
			err:  &ResponseError{StatusCode: http.StatusConflict, Body: `{"error":"outbox idempotency conflict"}`},
		},
		{
			name: "not a conflict",
			err:  &ResponseError{StatusCode: http.StatusBadRequest, Body: `{"error":"near-duplicate","error_kind":"near_duplicate_blocked"}`},
		},
		{name: "not a response error", err: errors.New("near-duplicate")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := AsNearDuplicateRejection(fmt.Errorf("wrapped: %w", test.err))
			if ok != test.ok || got != test.want {
				t.Fatalf("AsNearDuplicateRejection() = %+v, %v; want %+v, %v", got, ok, test.want, test.ok)
			}
		})
	}
}

func TestDeliveryDecodesTransportFields(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"outbox_id":       "outbox-2",
			"account_id":      "google-primary",
			"conversation_id": "conversation-2",
			"platform":        "sms",
			"state":           "canceled",
			"error_class":     "ttl",
			"expires_at_ms":   1_700_000_000_000,
			"expired":         true,
		})
	}))
	t.Cleanup(server.Close)

	delivery, err := NewClient(server.URL, "").Delivery(context.Background(), "outbox-2")
	if err != nil {
		t.Fatalf("Delivery(): %v", err)
	}
	if delivery.Platform != "sms" || delivery.ConversationID != "conversation-2" ||
		delivery.ExpiresAtMS != 1_700_000_000_000 || !delivery.Expired {
		t.Fatalf("delivery = %+v", delivery)
	}
}

func TestListPendingAndCancelDelivery(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/outbox":
			if got := r.URL.Query().Get("conversation_id"); got != "conversation-3" {
				t.Fatalf("conversation_id query = %q", got)
			}
			if got := r.URL.Query().Get("limit"); got != "25" {
				t.Fatalf("limit query = %q", got)
			}
			json.NewEncoder(w).Encode([]map[string]any{{
				"outbox_id":       "outbox-3",
				"conversation_id": "conversation-3",
				"kind":            "text",
				"state":           "queued",
				"expires_at_ms":   1_700_000_000_000,
			}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/outbox/outbox-3/cancel":
			json.NewEncoder(w).Encode(map[string]any{"outbox_id": "outbox-3", "state": "canceled"})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)

	client := NewClient(server.URL, "")
	pending, err := client.ListPending(context.Background(), "conversation-3", 25)
	if err != nil {
		t.Fatalf("ListPending(): %v", err)
	}
	if len(pending) != 1 || pending[0].OutboxID != "outbox-3" || pending[0].ExpiresAtMS != 1_700_000_000_000 {
		t.Fatalf("pending = %+v", pending)
	}

	delivery, err := client.CancelDelivery(context.Background(), "outbox-3")
	if err != nil {
		t.Fatalf("CancelDelivery(): %v", err)
	}
	if delivery.State != "canceled" {
		t.Fatalf("state = %q, want canceled", delivery.State)
	}
}

func TestSendCapabilityForDistinguishesUnknownFromUnavailable(t *testing.T) {
	old := DaemonStatus{}
	if _, known := old.SendCapabilityFor("whatsapp"); known {
		t.Fatal("daemon without a send block must report unknown, not unavailable")
	}
	status := DaemonStatus{Send: map[string]PlatformSendCapability{
		"whatsapp": {Available: false, Reason: "not paired"},
	}}
	capability, known := status.SendCapabilityFor("whatsapp")
	if !known || capability.Available {
		t.Fatalf("capability = %+v known=%v", capability, known)
	}
	if _, known := status.SendCapabilityFor("sms"); known {
		t.Fatal("platform missing from the send block must report unknown")
	}
}
