package cmd

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"
)

// The CLI prints why a durable send is not going out: the rejected branch
// shows error_detail (and says when the app gave up after its retry budget),
// and a retrying send shows its last failure.

func TestWriteCLIDeliveryRejectedPrintsDetail(t *testing.T) {
	const detail = "retry budget exhausted after 6 attempts; last failure transient [google_conversation_not_found]: send_text: transient: no conversation"
	var output bytes.Buffer
	err := writeCLIDelivery(&output, outboxDelivery{
		OutboxID:       "out",
		State:          "rejected",
		ErrorClass:     "retry_exhausted",
		ErrorDetail:    detail,
		AttemptCount:   6,
		RetryExhausted: true,
	}, "key", false)
	if err == nil || !strings.Contains(err.Error(), "will not retry") || !strings.Contains(err.Error(), detail) {
		t.Fatalf("rejected error = %v", err)
	}
	for _, line := range []string{
		"not sent: the app gave up after 6 attempts; nothing was sent",
		"delivery was rejected; the app will not retry it",
		"reason: " + detail,
	} {
		if !strings.Contains(output.String(), line+"\n") {
			t.Fatalf("output missing %q:\n%s", line, output.String())
		}
	}

	// A plain rejection without detail keeps its old output and error.
	output.Reset()
	err = writeCLIDelivery(&output, outboxDelivery{OutboxID: "out", State: "rejected"}, "key", false)
	if err == nil || err.Error() != "delivery was rejected; the app will not retry it" {
		t.Fatalf("plain rejected error = %v", err)
	}
	if strings.Contains(output.String(), "not sent:") || strings.Contains(output.String(), "reason:") {
		t.Fatalf("plain rejected output gained detail lines:\n%s", output.String())
	}
}

func TestWriteCLIDeliveryNotDispatchedPrintsLastFailure(t *testing.T) {
	var output bytes.Buffer
	if err := writeCLIDelivery(&output, outboxDelivery{
		OutboxID:    "out",
		State:       "not_dispatched",
		ErrorDetail: "[google_conversation_not_found] send_text: transient: no conversation",
	}, "key", false); err != nil {
		t.Fatalf("not_dispatched error = %v", err)
	}
	if !strings.Contains(output.String(), "last failure: [google_conversation_not_found]") ||
		!strings.Contains(output.String(), "Do not resend") {
		t.Fatalf("not_dispatched output:\n%s", output.String())
	}
}

func TestRunSendRejectedReturnsTheDaemonsReason(t *testing.T) {
	const detail = "send_text: reauth_required: [google_account_pairing_switched] the phone switched Google Messages to Google-account pairing"
	client := &http.Client{Transport: sendRoundTripper(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/api/status":
			return sendJSONResponse(200, `{"v2_primary":true}`), nil
		case "/api/v1/outbox/messages":
			return sendJSONResponse(200, `{"outbox_id":"out-1","state":"queued"}`), nil
		case "/api/v1/outbox/out-1":
			return sendJSONResponse(200, `{"outbox_id":"out-1","state":"rejected","error_class":"reauth_required","error_detail":"`+detail+`","attempt_count":1}`), nil
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
			return nil, nil
		}
	})}
	var output bytes.Buffer
	deps := sendCommandDeps{
		client:     client,
		baseURL:    "http://127.0.0.1",
		newKey:     func() (string, error) { return "key", nil },
		output:     &output,
		legacySend: func(string, string) error { t.Fatal("legacy path"); return nil },
	}
	err := runSendWithDeps(context.Background(), deps, "conv", "hello", nil)
	if err == nil || !strings.Contains(err.Error(), detail) {
		t.Fatalf("error = %v, want the daemon's reason", err)
	}
	for _, line := range []string{
		"not sent: the account must be re-linked before it can send; re-linking is the user's call",
		"reason: " + detail,
	} {
		if !strings.Contains(output.String(), line+"\n") {
			t.Fatalf("output missing %q:\n%s", line, output.String())
		}
	}
	if strings.Contains(output.String(), "gave up") {
		t.Fatalf("a reauth refusal is not a spent retry budget:\n%s", output.String())
	}
}
