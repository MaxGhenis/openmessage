package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type sendRoundTripper func(*http.Request) (*http.Response, error)

func (f sendRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func sendJSONResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}

func TestRunSendV2PrimaryUsesOutboxAPI(t *testing.T) {
	var output bytes.Buffer
	var requests []string
	notBefore := time.Now().Add(time.Hour).UnixMilli()
	deps := sendCommandDeps{
		mode:   func() (v2RuntimeMode, error) { return v2RuntimeMode{}, nil },
		newKey: func() (string, error) { return "stable-key", nil },
		output: &output,
		client: &http.Client{Transport: sendRoundTripper(func(r *http.Request) (*http.Response, error) {
			requests = append(requests, r.Method+" "+r.URL.Path)
			switch r.URL.Path {
			case "/api/status":
				return sendJSONResponse(http.StatusOK, `{"v2_send":true,"v2_primary":true}`), nil
			case "/api/v1/outbox/messages":
				var body struct {
					ConversationID string `json:"conversation_id"`
					Message        string `json:"body"`
					Key            string `json:"idempotency_key"`
					NotBeforeMS    *int64 `json:"not_before_ms"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				if body.ConversationID != "conv-1" || body.Message != "hello" || body.Key != "stable-key" {
					t.Fatalf("POST body = %+v", body)
				}
				if body.NotBeforeMS == nil || *body.NotBeforeMS != notBefore {
					t.Fatalf("not_before_ms = %v", body.NotBeforeMS)
				}
				return sendJSONResponse(http.StatusOK, fmt.Sprintf(`{"outbox_id":"out-1","state":"queued","scheduled_for_ms":%d,"deduplicated":false}`, *body.NotBeforeMS)), nil
			case "/api/v1/outbox/out-1":
				return sendJSONResponse(http.StatusOK, `{"outbox_id":"out-1","state":"not_dispatched","error_class":"transient"}`), nil
			default:
				t.Fatalf("unexpected request %s", r.URL.Path)
				return nil, nil
			}
		})},
		legacySend: func(string, string) error { t.Fatal("legacy send called"); return nil },
	}
	if err := runSendWithDeps(context.Background(), deps, "conv-1", "hello", &notBefore); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(requests, ","); got != "GET /api/status,POST /api/v1/outbox/messages,GET /api/v1/outbox/out-1" {
		t.Fatalf("requests = %s", got)
	}
	for _, want := range []string{"outbox_id=out-1", "state=not_dispatched", "idempotency_key=stable-key", fmt.Sprintf("scheduled_for_ms=%d", notBefore), "scheduled; the app will send it at"} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("output %q missing %q", output.String(), want)
		}
	}
}

func TestRunSendAmbiguousSubmissionDoesNotRetry(t *testing.T) {
	var output bytes.Buffer
	posts := 0
	deps := sendCommandDeps{
		mode:   func() (v2RuntimeMode, error) { return v2RuntimeMode{Primary: true}, nil },
		newKey: func() (string, error) { return "replay-key", nil }, output: &output,
		client: &http.Client{Transport: sendRoundTripper(func(r *http.Request) (*http.Response, error) {
			if r.URL.Path == "/api/status" {
				return sendJSONResponse(200, `{"v2_primary":true}`), nil
			}
			posts++
			return nil, errors.New("timeout")
		})},
		legacySend: func(string, string) error { t.Fatal("legacy send called"); return nil },
	}
	err := runSendWithDeps(context.Background(), deps, "conv-1", "hello", nil)
	if err == nil {
		t.Fatal("expected ambiguous error")
	}
	if posts != 1 {
		t.Fatalf("POST count = %d", posts)
	}
	for _, want := range []string{"replay-key", "same idempotency key", "Do not send with a new key"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
}

func TestRunSendFallsBackToLegacyOutsideV2Primary(t *testing.T) {
	legacyCalls := 0
	deps := sendCommandDeps{
		mode: func() (v2RuntimeMode, error) { return v2RuntimeMode{}, nil },
		client: &http.Client{Transport: sendRoundTripper(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("connection refused")
		})},
		legacySend: func(conversationID, message string) error {
			legacyCalls++
			if conversationID != "conv-1" || message != "hello" {
				t.Fatalf("legacy args = %q %q", conversationID, message)
			}
			return nil
		},
	}
	if err := runSendWithDeps(context.Background(), deps, "conv-1", "hello", nil); err != nil {
		t.Fatal(err)
	}
	if legacyCalls != 1 {
		t.Fatalf("legacy calls = %d", legacyCalls)
	}
}

func TestRunSendDaemonNotV2PrimaryFallsBackToLegacy(t *testing.T) {
	legacyCalls := 0
	deps := sendCommandDeps{
		mode: func() (v2RuntimeMode, error) {
			t.Fatal("local mode consulted despite reachable daemon")
			return v2RuntimeMode{}, nil
		},
		client: &http.Client{Transport: sendRoundTripper(func(r *http.Request) (*http.Response, error) {
			if r.URL.Path != "/api/status" {
				t.Fatalf("unexpected path %s", r.URL.Path)
			}
			return sendJSONResponse(200, `{"v2_primary":false}`), nil
		})},
		legacySend: func(string, string) error { legacyCalls++; return nil },
	}
	if err := runSendWithDeps(context.Background(), deps, "conv-1", "hello", nil); err != nil {
		t.Fatal(err)
	}
	if legacyCalls != 1 {
		t.Fatalf("legacy calls = %d", legacyCalls)
	}
}

func TestRunSendDeduplicatedReplayIsSuccess(t *testing.T) {
	var output bytes.Buffer
	deps := sendCommandDeps{
		mode:   func() (v2RuntimeMode, error) { return v2RuntimeMode{Primary: true}, nil },
		newKey: func() (string, error) { return "reused-key", nil }, output: &output,
		client: &http.Client{Transport: sendRoundTripper(func(r *http.Request) (*http.Response, error) {
			switch r.URL.Path {
			case "/api/status":
				return sendJSONResponse(200, `{"v2_primary":true}`), nil
			case "/api/v1/outbox/messages":
				return sendJSONResponse(200, `{"outbox_id":"out-1","state":"confirmed","deduplicated":true}`), nil
			case "/api/v1/outbox/out-1":
				return sendJSONResponse(200, `{"outbox_id":"out-1","state":"confirmed"}`), nil
			default:
				t.Fatalf("unexpected path %s", r.URL.Path)
				return nil, nil
			}
		})},
		legacySend: func(string, string) error { t.Fatal("legacy send called"); return nil },
	}
	if err := runSendWithDeps(context.Background(), deps, "conv-1", "hello", nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "deduplicated=true") {
		t.Fatalf("output = %q", output.String())
	}
}

func TestRunSendV2PrimaryDaemonAbsentDoesNotConnect(t *testing.T) {
	deps := sendCommandDeps{
		mode:       func() (v2RuntimeMode, error) { return v2RuntimeMode{Primary: true}, nil },
		client:     &http.Client{Transport: sendRoundTripper(func(*http.Request) (*http.Response, error) { return nil, errors.New("connection refused") })},
		legacySend: func(string, string) error { t.Fatal("legacy send called"); return nil },
	}
	err := runSendWithDeps(context.Background(), deps, "conv-1", "hello", nil)
	if err == nil || !strings.Contains(err.Error(), "OpenMessage isn't running; start it to send") {
		t.Fatalf("error = %v", err)
	}
}

func TestRunSendGroupV2PrimaryRejectsCreationWithoutConnecting(t *testing.T) {
	legacyCalls := 0
	err := runSendGroupWithDeps(sendGroupCommandDeps{
		mode: func() (v2RuntimeMode, error) { t.Fatal("local mode consulted"); return v2RuntimeMode{}, nil },
		client: &http.Client{Transport: sendRoundTripper(func(*http.Request) (*http.Response, error) {
			return sendJSONResponse(200, `{"v2_send":true,"v2_primary":true}`), nil
		})},
		legacySend: func([]string, string) error { legacyCalls++; return nil },
	}, []string{"+15551234567"}, "hello")
	if err == nil || !strings.Contains(err.Error(), "creating a new group isn't supported in v2 yet") {
		t.Fatalf("error = %v", err)
	}
	if legacyCalls != 0 {
		t.Fatalf("legacy calls = %d", legacyCalls)
	}
}

func TestRunSendGroupRoutingCells(t *testing.T) {
	tests := []struct {
		name       string
		status     string
		probeErr   error
		local      v2RuntimeMode
		wantLegacy bool
		wantStop   bool
	}{
		{"v2 daemon bare terminal", `{"v2_send":true}`, nil, v2RuntimeMode{}, false, false},
		{"legacy daemon primary terminal", `{"v2_send":false,"v2_primary":false}`, nil, v2RuntimeMode{Primary: true}, true, false},
		{"no daemon legacy terminal", "", errors.New("refused"), v2RuntimeMode{}, true, false},
		{"no daemon send terminal", "", errors.New("refused"), v2RuntimeMode{Send: true}, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			legacyCalls := 0
			deps := sendGroupCommandDeps{
				mode: func() (v2RuntimeMode, error) { return tt.local, nil },
				client: &http.Client{Transport: sendRoundTripper(func(*http.Request) (*http.Response, error) {
					if tt.probeErr != nil {
						return nil, tt.probeErr
					}
					return sendJSONResponse(200, tt.status), nil
				})},
				legacySend: func([]string, string) error { legacyCalls++; return nil },
			}
			err := runSendGroupWithDeps(deps, []string{"+15551234567"}, "hello")
			if tt.wantStop && (err == nil || !strings.Contains(err.Error(), "OpenMessage isn't running")) {
				t.Fatalf("error = %v", err)
			}
			if !tt.wantStop && !tt.wantLegacy && (err == nil || !strings.Contains(err.Error(), "isn't supported in v2")) {
				t.Fatalf("error = %v", err)
			}
			if legacyCalls != btoi(tt.wantLegacy) {
				t.Fatalf("legacy calls = %d", legacyCalls)
			}
		})
	}
}

func TestAmbiguousErrorExposesCleanOperatorInstruction(t *testing.T) {
	err := ambiguousSendError("stable-key", errors.New("timeout"))
	if got := OperatorInstruction(err); !strings.Contains(got, "same idempotency key: stable-key") || strings.Contains(got, "timeout") {
		t.Fatalf("instruction = %q", got)
	}
}

func TestRunSendRoutingCells(t *testing.T) {
	tests := []struct {
		name       string
		status     string
		probeErr   error
		local      v2RuntimeMode
		wantLegacy bool
		wantStop   bool
	}{
		{"gui primary daemon bare terminal", `{"v2_send":true,"v2_primary":true}`, nil, v2RuntimeMode{}, false, false},
		{"send daemon bare terminal", `{"v2_send":true,"v2_primary":false}`, nil, v2RuntimeMode{}, false, false},
		{"legacy daemon primary terminal", `{"v2_send":false,"v2_primary":false}`, nil, v2RuntimeMode{Primary: true}, true, false},
		{"no daemon legacy terminal", "", errors.New("refused"), v2RuntimeMode{}, true, false},
		{"no daemon send terminal", "", errors.New("refused"), v2RuntimeMode{Send: true}, false, true},
		{"no daemon primary terminal", "", errors.New("refused"), v2RuntimeMode{Primary: true}, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			legacyCalls := 0
			deps := sendCommandDeps{
				mode:   func() (v2RuntimeMode, error) { return tt.local, nil },
				newKey: func() (string, error) { return "routing-key", nil },
				output: io.Discard,
				client: &http.Client{Transport: sendRoundTripper(func(r *http.Request) (*http.Response, error) {
					if r.URL.Path == "/api/status" {
						if tt.probeErr != nil {
							return nil, tt.probeErr
						}
						return sendJSONResponse(200, tt.status), nil
					}
					if r.Method == http.MethodPost {
						return sendJSONResponse(200, `{"outbox_id":"out","state":"confirmed"}`), nil
					}
					return sendJSONResponse(200, `{"outbox_id":"out","state":"confirmed"}`), nil
				})},
				legacySend: func(string, string) error { legacyCalls++; return nil },
			}
			err := runSendWithDeps(context.Background(), deps, "conv", "hello", nil)
			if tt.wantStop != (err != nil && strings.Contains(err.Error(), "OpenMessage isn't running")) {
				t.Fatalf("error = %v", err)
			}
			if legacyCalls != btoi(tt.wantLegacy) {
				t.Fatalf("legacy calls = %d", legacyCalls)
			}
		})
	}
}

func btoi(value bool) int {
	if value {
		return 1
	}
	return 0
}

func TestRunSendDeterministicRejections(t *testing.T) {
	for _, status := range []int{400, 404, 409, 413, 422} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			deps := sendCommandDeps{
				mode:   func() (v2RuntimeMode, error) { return v2RuntimeMode{}, nil },
				newKey: func() (string, error) { return "conflicting-key", nil }, output: io.Discard,
				client: &http.Client{Transport: sendRoundTripper(func(r *http.Request) (*http.Response, error) {
					if r.URL.Path == "/api/status" {
						return sendJSONResponse(200, `{"v2_send":true}`), nil
					}
					return sendJSONResponse(status, `{"error":"specific rejection"}`), nil
				})},
				legacySend: func(string, string) error { t.Fatal("legacy send called"); return nil },
			}
			err := runSendWithDeps(context.Background(), deps, "conv", "hello", nil)
			if err == nil || !strings.Contains(err.Error(), "specific rejection") {
				t.Fatalf("error = %v", err)
			}
			if strings.Contains(err.Error(), "outcome is unknown") || strings.Contains(err.Error(), "same idempotency key") {
				t.Fatalf("deterministic error was ambiguous: %v", err)
			}
			if status == 409 && !strings.Contains(err.Error(), "use a new key") {
				t.Fatalf("409 error = %v", err)
			}
		})
	}
}

func TestWriteCLIDeliveryExitSemantics(t *testing.T) {
	var output bytes.Buffer
	if err := writeCLIDelivery(&output, outboxDelivery{OutboxID: "out", State: "uncertain"}, "key", false); err != nil {
		t.Fatalf("uncertain error = %v", err)
	}
	if err := writeCLIDelivery(&output, outboxDelivery{OutboxID: "out", State: "rejected"}, "key", false); err == nil || !strings.Contains(err.Error(), "will not retry") {
		t.Fatalf("rejected error = %v", err)
	}
}

func TestRunSendRealTransportSequence(t *testing.T) {
	var sequence []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sequence = append(sequence, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/status":
			fmt.Fprint(w, `{"v2_send":true}`)
		case "/api/v1/outbox/messages":
			fmt.Fprint(w, `{"outbox_id":"real-out","state":"queued"}`)
		case "/api/v1/outbox/real-out":
			fmt.Fprint(w, `{"outbox_id":"real-out","state":"confirmed"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	deps := sendCommandDeps{mode: func() (v2RuntimeMode, error) { return v2RuntimeMode{}, nil }, client: server.Client(), baseURL: server.URL, newKey: func() (string, error) { return "real-key", nil }, output: io.Discard, legacySend: func(string, string) error { t.Fatal("legacy"); return nil }}
	if err := runSendWithDeps(context.Background(), deps, "conv", "hello", nil); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(sequence, ","); got != "GET /api/status,POST /api/v1/outbox/messages,GET /api/v1/outbox/real-out" {
		t.Fatalf("sequence = %s", got)
	}
}

func TestRunSendRealTransportDoesNotRetry500(t *testing.T) {
	var posts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/status" {
			fmt.Fprint(w, `{"v2_send":true}`)
			return
		}
		posts.Add(1)
		http.Error(w, "temporary", http.StatusInternalServerError)
	}))
	defer server.Close()
	deps := sendCommandDeps{mode: func() (v2RuntimeMode, error) { return v2RuntimeMode{}, nil }, client: server.Client(), baseURL: server.URL, newKey: func() (string, error) { return "real-key", nil }, output: io.Discard, legacySend: func(string, string) error { t.Fatal("legacy"); return nil }}
	err := runSendWithDeps(context.Background(), deps, "conv", "hello", nil)
	if err == nil || !strings.Contains(err.Error(), "outcome is unknown") {
		t.Fatalf("error = %v", err)
	}
	if got := posts.Load(); got != 1 {
		t.Fatalf("POST count = %d", got)
	}
}
