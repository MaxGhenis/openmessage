package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"syscall"
	"testing"
	"unicode/utf8"

	"github.com/maxghenis/openmessage/internal/localapi"
)

// Daemon stand-ins for the client shapes of the preamble test. Neither opens
// a socket: one refuses every request the way a closed port does, the other
// serves a scripted app in memory.

// refusingDaemonTransport fails every request with the error a dial to a
// port with no listener returns, wrapping syscall.ECONNREFUSED, so
// localapi.IsConnectionRefused holds and the tools take their app-not-running
// branches without dialling anything.
type refusingDaemonTransport struct{}

func (refusingDaemonTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.Body != nil {
		_ = request.Body.Close()
	}
	return nil, &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
}

// refusingDaemonClient is a daemon client for an app that is not running.
func refusingDaemonClient() *localapi.Client {
	return &localapi.Client{BaseURL: "http://127.0.0.1:1", HTTP: &http.Client{Transport: refusingDaemonTransport{}}}
}

// handlerTransport serves each request with an in-memory http.Handler, so a
// localapi.Client reaches a scripted app without a listener.
type handlerTransport struct{ handler http.Handler }

func (h handlerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	recorder := httptest.NewRecorder()
	h.handler.ServeHTTP(recorder, request)
	if request.Body != nil {
		// RoundTrip owns the body; closing it also ends a multipart
		// writer the handler did not drain.
		_ = request.Body.Close()
	}
	response := recorder.Result()
	response.Request = request
	return response, nil
}

// liveDaemonClient is a daemon client for the scripted running app.
func liveDaemonClient(t *testing.T) *localapi.Client {
	t.Helper()
	return &localapi.Client{
		BaseURL: "http://daemon.invalid",
		HTTP:    &http.Client{Transport: handlerTransport{handler: liveDaemonHandler(t)}},
	}
}

// liveDaemonHandler scripts a running, connected legacy-primary app with v2
// sending on: sends go through the durable outbox (submit, then a poll that
// finds them confirmed), reactions through /api/react, transcripts through
// /api/transcript (which a legacy-primary app accepts) and attachments
// through the media routes. Its status carries no message text, last_error
// included. Any other request fails the test.
func liveDaemonHandler(t *testing.T) http.Handler {
	t.Helper()
	var (
		mu   sync.Mutex
		next int
	)
	newOutboxID := func() string {
		mu.Lock()
		defer mu.Unlock()
		next++
		return fmt.Sprintf("preamble-out-%d", next)
	}
	writeJSON := func(w http.ResponseWriter, status int, value any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(value)
	}
	// Every attachment the app serves is untrustedAttachment, whose bytes
	// carry the marker.
	serveAttachment := func(w http.ResponseWriter, _ *http.Request) {
		sum := sha256.Sum256(untrustedAttachment)
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("X-Content-SHA256", hex.EncodeToString(sum[:]))
		_, _ = w.Write(untrustedAttachment)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"connected":  true,
			"v2_send":    true,
			"v2_primary": false,
			"google":     map[string]any{"connected": true, "paired": true},
			"whatsapp":   map[string]any{"connected": true, "paired": true},
			"signal":     map[string]any{"connected": true, "paired": true, "last_error": "receive failed: exit status 1"},
		})
	})
	mux.HandleFunc("POST /api/v1/outbox/messages", func(w http.ResponseWriter, r *http.Request) {
		var request localapi.TextSubmission
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode text submission: %v", err)
		}
		if request.ConversationID == "" || request.IdempotencyKey == "" || request.Body == "" {
			t.Errorf("text submission is missing fields: %+v", request)
		}
		writeJSON(w, http.StatusAccepted, map[string]any{"outbox_id": newOutboxID(), "state": "queued"})
	})
	mux.HandleFunc("POST /api/v1/outbox/media", func(w http.ResponseWriter, r *http.Request) {
		file, _, err := r.FormFile("file")
		if err != nil {
			t.Errorf("media submission has no file: %v", err)
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "no file"})
			return
		}
		defer file.Close()
		if data, err := io.ReadAll(file); err != nil || len(data) == 0 {
			t.Errorf("read media submission: %d bytes, %v", len(data), err)
		}
		if r.FormValue("conversation_id") == "" || r.FormValue("idempotency_key") == "" {
			t.Errorf("media submission is missing fields: conversation_id=%q idempotency_key=%q",
				r.FormValue("conversation_id"), r.FormValue("idempotency_key"))
		}
		writeJSON(w, http.StatusAccepted, map[string]any{"outbox_id": newOutboxID(), "state": "queued"})
	})
	mux.HandleFunc("GET /api/v1/outbox/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		writeJSON(w, http.StatusOK, map[string]any{
			"outbox_id":         id,
			"state":             "confirmed",
			"remote_message_id": "remote-" + id,
		})
	})
	mux.HandleFunc("POST /api/react", func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			MessageID string `json:"message_id"`
			Emoji     string `json:"emoji"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.MessageID == "" || request.Emoji == "" {
			t.Errorf("react request = %+v, %v", request, err)
		}
		writeJSON(w, http.StatusOK, map[string]any{"success": true})
	})
	mux.HandleFunc("POST /api/transcript", func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			MessageID  string  `json:"message_id"`
			Transcript string  `json:"transcript"`
			Model      *string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.MessageID == "" {
			t.Errorf("transcript request = %+v, %v", request, err)
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"success":           true,
			"message_id":        request.MessageID,
			"transcript_length": utf8.RuneCountInString(request.Transcript),
			"transcript_model":  request.Model,
		})
	})
	mux.HandleFunc("GET /api/media/{id}", serveAttachment)
	mux.HandleFunc("GET /api/v1/messages/{id}/attachments/{ordinal}", serveAttachment)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("the scripted app got an unscripted request: %s %s", r.Method, r.URL.Path)
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "not scripted"})
	})
	return mux
}

// The refusing client must look exactly like an app that is not running:
// every request fails as refused (localapi.IsConnectionRefused), and the
// status probe reports the app unreachable, so the client shapes take the
// same branches they take against a closed port.
func TestPreambleRefusingDaemonReportsConnectionRefused(t *testing.T) {
	client := refusingDaemonClient()
	ctx := context.Background()
	if _, reachable, err := client.Status(ctx); reachable || !localapi.IsConnectionRefused(err) {
		t.Fatalf("Status() = reachable %v, err %v; want unreachable and connection refused", reachable, err)
	}
	if _, err := client.RawStatus(ctx); err == nil || !localapi.IsConnectionRefused(err) {
		t.Fatalf("RawStatus() err = %v, want connection refused", err)
	}
	model := "test-model"
	if _, err := client.SetTranscript(ctx, preambleIncomingMessageID, "agent transcript", &model); !localapi.IsConnectionRefused(err) {
		t.Fatalf("SetTranscript() err = %v, want connection refused", err)
	}
	if _, err := client.MessageAttachment(ctx, preambleV2MessageID, 0, 1<<20); !localapi.IsConnectionRefused(err) {
		t.Fatalf("MessageAttachment() err = %v, want connection refused", err)
	}
	if _, err := client.React(ctx, preambleConversationID, preambleIncomingMessageID, "👍", "add", "key-1"); !localapi.IsConnectionRefused(err) {
		t.Fatalf("React() err = %v, want connection refused", err)
	} else if _, answered := localapi.AsResponseError(err); answered {
		t.Fatalf("React() err = %v looks like an answer from the app", err)
	}
}
