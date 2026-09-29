package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/rs/zerolog"

	"github.com/maxghenis/openmessage/internal/app"
	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/localapi"
	"github.com/maxghenis/openmessage/internal/messaging"
	"github.com/maxghenis/openmessage/internal/storage/blob"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
	omtools "github.com/maxghenis/openmessage/internal/tools"
	"github.com/maxghenis/openmessage/internal/web"
)

// TestNearDuplicateErrorKindConstantsAgree pins the localapi copy of the
// structured-409 error kind to the web package's authoritative constant.
func TestNearDuplicateErrorKindConstantsAgree(t *testing.T) {
	if localapi.NearDuplicateErrorKind != web.NearDuplicateErrorKind {
		t.Fatalf("localapi.NearDuplicateErrorKind = %q, want %q", localapi.NearDuplicateErrorKind, web.NearDuplicateErrorKind)
	}
}

func TestParseSendOptions(t *testing.T) {
	notBefore := int64(1_800_000_000_000)
	tests := []struct {
		name    string
		args    []string
		want    SendOptions
		wantErr string
	}{
		{name: "none", args: nil, want: SendOptions{}},
		{name: "force alone", args: []string{"--force"}, want: SendOptions{Force: true}},
		{
			name: "all options in any order",
			args: []string{"--idempotency-key", "key-1", "--force", "--not-before-ms", "1800000000000"},
			want: SendOptions{IdempotencyKey: "key-1", Force: true, NotBeforeMS: &notBefore},
		},
		{name: "force takes no value", args: []string{"--force", "--idempotency-key", "k"}, want: SendOptions{Force: true, IdempotencyKey: "k"}},
		{name: "missing value", args: []string{"--idempotency-key"}, wantErr: "send option --idempotency-key requires a value"},
		{name: "bad schedule", args: []string{"--not-before-ms", "soon"}, wantErr: "--not-before-ms must be an integer"},
		{name: "unknown option", args: []string{"--forced"}, wantErr: "unknown send option --forced"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := ParseSendOptions(test.args)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("ParseSendOptions(%q) error = %v, want %q", test.args, err, test.wantErr)
				}
				return
			}
			if err != nil || !reflect.DeepEqual(got, test.want) {
				t.Fatalf("ParseSendOptions(%q) = %+v, %v; want %+v", test.args, got, err, test.want)
			}
		})
	}
}

// The CLI is an agent entry point: every v2 text submission asks for the
// guard, and --force passes through.
func TestRunSendRequestsGuardAndForce(t *testing.T) {
	for _, force := range []bool{false, true} {
		t.Run(fmt.Sprintf("force=%v", force), func(t *testing.T) {
			var submitted map[string]any
			deps := sendCommandDeps{
				mode:   func() (v2RuntimeMode, error) { return v2RuntimeMode{}, nil },
				newKey: func() (string, error) { return "guard-key", nil },
				output: io.Discard,
				client: &http.Client{Transport: sendRoundTripper(func(r *http.Request) (*http.Response, error) {
					switch {
					case r.URL.Path == "/api/status":
						return sendJSONResponse(200, `{"v2_send":true}`), nil
					case r.Method == http.MethodPost:
						if err := json.NewDecoder(r.Body).Decode(&submitted); err != nil {
							t.Fatalf("decode submission: %v", err)
						}
						return sendJSONResponse(200, `{"outbox_id":"out","state":"queued"}`), nil
					default:
						return sendJSONResponse(200, `{"outbox_id":"out","state":"queued"}`), nil
					}
				})},
				legacySend: func(string, string) error { t.Fatal("legacy send called"); return nil },
			}
			if err := runSendWithDeps(context.Background(), deps, "conv", "hello", SendOptions{Force: force}); err != nil {
				t.Fatal(err)
			}
			if submitted["guard_near_duplicates"] != true {
				t.Fatalf("guard_near_duplicates = %v, want true", submitted["guard_near_duplicates"])
			}
			if gotForce, _ := submitted["force"].(bool); gotForce != force {
				t.Fatalf("force = %v, want %v", submitted["force"], force)
			}
		})
	}
}

// A near-duplicate 409 tells the operator to rerun with --force (a new key
// would be refused again); only an idempotency conflict says "use a new key".
func TestRunSendNearDuplicateRejection(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		fragments []string
	}{
		{
			name:      "structured",
			body:      `{"error":"messaging: near-duplicate send blocked: …","error_kind":"near_duplicate_blocked","duplicate_of_outbox_id":"outbox-prior","duplicate_state":"queued","duplicate_age_ms":120000}`,
			fragments: []string{"near-duplicate of outbox outbox-prior", "state queued", "2m0s ago", "nothing was queued", "--force"},
		},
		{
			name:      "older daemon",
			body:      `{"error":"messaging: near-duplicate send blocked: a very similar message was submitted to this conversation 2m0s ago (outbox old-1, state queued); if this is intentional, resubmit with force"}`,
			fragments: []string{"near-duplicate", "outbox old-1", "nothing was queued", "--force"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			deps := sendCommandDeps{
				mode:   func() (v2RuntimeMode, error) { return v2RuntimeMode{}, nil },
				newKey: func() (string, error) { return "dup-key", nil },
				output: io.Discard,
				client: &http.Client{Transport: sendRoundTripper(func(r *http.Request) (*http.Response, error) {
					if r.URL.Path == "/api/status" {
						return sendJSONResponse(200, `{"v2_send":true}`), nil
					}
					return sendJSONResponse(http.StatusConflict, test.body), nil
				})},
				legacySend: func(string, string) error { t.Fatal("legacy send called"); return nil },
			}
			err := runSendWithDeps(context.Background(), deps, "conv", "hello", SendOptions{})
			if err == nil {
				t.Fatal("near-duplicate 409 returned no error")
			}
			for _, fragment := range test.fragments {
				if !strings.Contains(err.Error(), fragment) {
					t.Fatalf("error missing %q: %v", fragment, err)
				}
			}
			if strings.Contains(err.Error(), "use a new key") || strings.Contains(err.Error(), "outcome is unknown") {
				t.Fatalf("near-duplicate error gives the wrong remedy: %v", err)
			}
			if OperatorInstruction(err) != "" {
				t.Fatalf("near-duplicate refusal carries a replay instruction: %q", OperatorInstruction(err))
			}
		})
	}
}

// --- Entry-point scope matrix against a real daemon -------------------------

const guardMatrixConversationID = "guard-matrix-thread"

type guardMatrixRegistry struct{}

func (guardMatrixRegistry) Snapshot(accountID string) (bridge.Snapshot, bool) {
	return bridge.Snapshot{AccountID: accountID, Platform: bridge.PlatformGoogle, Generation: 1}, accountID == googleAccountID
}

func (guardMatrixRegistry) Acquire(context.Context, string, bridge.Capability) (*bridge.DispatchLease, error) {
	// Offline: nothing dispatches, so every accepted intent stays queued.
	return nil, bridge.ErrAccountNotRegistered
}

func (guardMatrixRegistry) Capabilities(accountID string) bridge.CapabilitySet {
	if accountID != googleAccountID {
		return bridge.CapabilitySet{}
	}
	return bridge.CapabilitySet{TextSend: true, MediaSend: true}
}

type guardMatrixIDs struct {
	mu   sync.Mutex
	next int
}

func (s *guardMatrixIDs) NewID() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next++
	return fmt.Sprintf("matrix-%03d", s.next), nil
}

type guardMatrixDaemon struct {
	legacy  *db.Store
	service *messaging.MessageService
	server  *httptest.Server
	client  *localapi.Client
}

// newGuardMatrixDaemon serves the real v1 API over a real message service
// and v2 store, exactly the surface MCP daemon mode, the CLI, and the web UI
// submit to.
func newGuardMatrixDaemon(t *testing.T) *guardMatrixDaemon {
	t.Helper()
	legacy, err := db.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = legacy.Close() })
	if err := legacy.UpsertConversation(&db.Conversation{
		ConversationID: guardMatrixConversationID,
		Name:           "Guard matrix",
		Participants:   `[]`,
		SourcePlatform: "sms",
	}); err != nil {
		t.Fatal(err)
	}
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "v2.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	blobs, err := blob.New(filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	registry := guardMatrixRegistry{}
	service, err := messaging.NewMessageService(store, registry, blobs, messaging.SystemClock{}, &guardMatrixIDs{})
	if err != nil {
		t.Fatal(err)
	}
	handler := web.APIHandlerWithOptions(legacy, nil, zerolog.Nop(), nil, web.APIOptions{V2: &web.V2Options{
		Service:  service,
		V2Store:  store,
		Blobs:    blobs,
		Registry: registry,
	}})
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return &guardMatrixDaemon{
		legacy:  legacy,
		service: service,
		server:  server,
		client:  &localapi.Client{BaseURL: server.URL, HTTP: server.Client()},
	}
}

func (d *guardMatrixDaemon) pending(t *testing.T) int {
	t.Helper()
	pending, err := d.service.ListPending(context.Background(), messaging.ListPendingQuery{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	return len(pending)
}

func (d *guardMatrixDaemon) postJSON(t *testing.T, payload map[string]any) (int, string) {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	response, err := d.server.Client().Post(d.server.URL+"/api/v1/outbox/messages", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, string(raw)
}

func (d *guardMatrixDaemon) postMedia(t *testing.T, key, caption string) int {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for field, value := range map[string]string{
		"conversation_id": guardMatrixConversationID,
		"idempotency_key": key,
		"caption":         caption,
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
	response, err := d.server.Client().Post(d.server.URL+"/api/v1/outbox/media", writer.FormDataContentType(), &body)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	return response.StatusCode
}

func (d *guardMatrixDaemon) cliSend(t *testing.T, key, body string, force bool) error {
	t.Helper()
	deps := sendCommandDeps{
		mode:       func() (v2RuntimeMode, error) { return v2RuntimeMode{}, nil },
		client:     d.server.Client(),
		baseURL:    d.server.URL,
		newKey:     func() (string, error) { return key, nil },
		output:     io.Discard,
		legacySend: func(string, string) error { t.Fatal("legacy send called"); return nil },
	}
	return runSendWithDeps(context.Background(), deps, guardMatrixConversationID, body, SendOptions{Force: force})
}

func (d *guardMatrixDaemon) mcpSend(t *testing.T, key, body string, force bool) *mcp.CallToolResult {
	t.Helper()
	server := mcpserver.NewMCPServer("guard-matrix", "test")
	omtools.RegisterWithOptions(server, &app.App{Store: d.legacy, Logger: zerolog.Nop()}, omtools.Options{Daemon: d.client})
	tool := server.GetTool("send_to_conversation")
	if tool == nil {
		t.Fatal("send_to_conversation not registered in daemon mode")
	}
	request := mcp.CallToolRequest{}
	request.Params.Arguments = map[string]any{
		"conversation_id": guardMatrixConversationID,
		"message":         body,
		"idempotency_key": key,
		"force":           force,
		"wait_seconds":    float64(0),
	}
	result, err := tool.Handler(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

type guardMatrixOutcome string

const (
	guardMatrixAccepted      guardMatrixOutcome = "accepted"
	guardMatrixNearDuplicate guardMatrixOutcome = "refused as a near-duplicate of the prior"
	guardMatrixOtherFailure  guardMatrixOutcome = "failed for another reason"
)

// TestNearDuplicateGuardEntryPointScope is the Variant A scope matrix, each
// row driven through its real client code against one real daemon: agent
// entry points (MCP daemon mode, the CLI) are refused as near-duplicates with
// force as the override; the web UI's request shape and media are accepted.
// (MCP in-process sends are guarded too, and SendAgain is never guarded; the
// tools, web, and messaging packages cover those paths, which need no
// cross-package client.)
func TestNearDuplicateGuardEntryPointScope(t *testing.T) {
	const prior = "Lunch tomorrow at noon at Sfoglina?"
	const repeat = "Lunch today at noon at Sfoglina?"
	tests := []struct {
		name string
		want guardMatrixOutcome
		send func(t *testing.T, d *guardMatrixDaemon, priorOutboxID string) (guardMatrixOutcome, string)
	}{
		{
			name: "mcp daemon client",
			want: guardMatrixNearDuplicate,
			send: func(t *testing.T, d *guardMatrixDaemon, priorOutboxID string) (guardMatrixOutcome, string) {
				return mcpMatrixOutcome(d.mcpSend(t, "mcp-key", repeat, false), priorOutboxID)
			},
		},
		{
			name: "mcp daemon client with force",
			want: guardMatrixAccepted,
			send: func(t *testing.T, d *guardMatrixDaemon, priorOutboxID string) (guardMatrixOutcome, string) {
				return mcpMatrixOutcome(d.mcpSend(t, "mcp-key", repeat, true), priorOutboxID)
			},
		},
		{
			name: "cli openmessage send",
			want: guardMatrixNearDuplicate,
			send: func(t *testing.T, d *guardMatrixDaemon, priorOutboxID string) (guardMatrixOutcome, string) {
				return cliMatrixOutcome(d.cliSend(t, "cli-key", repeat, false), priorOutboxID)
			},
		},
		{
			name: "cli openmessage send --force",
			want: guardMatrixAccepted,
			send: func(t *testing.T, d *guardMatrixDaemon, priorOutboxID string) (guardMatrixOutcome, string) {
				return cliMatrixOutcome(d.cliSend(t, "cli-key", repeat, true), priorOutboxID)
			},
		},
		{
			name: "web ui request shape",
			want: guardMatrixAccepted,
			send: func(t *testing.T, d *guardMatrixDaemon, _ string) (guardMatrixOutcome, string) {
				status, body := d.postJSON(t, map[string]any{
					"conversation_id": guardMatrixConversationID,
					"body":            repeat,
					"reply_to_id":     "",
					"idempotency_key": "ui-key",
				})
				return httpMatrixOutcome(status), body
			},
		},
		{
			name: "media with a repeated caption",
			want: guardMatrixAccepted,
			send: func(t *testing.T, d *guardMatrixDaemon, _ string) (guardMatrixOutcome, string) {
				status := d.postMedia(t, "media-key", prior)
				return httpMatrixOutcome(status), fmt.Sprint(status)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			d := newGuardMatrixDaemon(t)
			status, body := d.postJSON(t, map[string]any{
				"conversation_id":       guardMatrixConversationID,
				"body":                  prior,
				"idempotency_key":       "prior-key",
				"guard_near_duplicates": true,
			})
			if status != http.StatusOK {
				t.Fatalf("prior send status = %d: %s", status, body)
			}
			var priorSubmission localapi.Submission
			if err := json.Unmarshal([]byte(body), &priorSubmission); err != nil || priorSubmission.OutboxID == "" {
				t.Fatalf("prior submission %q: %v", body, err)
			}
			before := d.pending(t)
			outcome, detail := test.send(t, d, priorSubmission.OutboxID)
			if outcome != test.want {
				t.Fatalf("outcome = %s, want %s (%s)", outcome, test.want, detail)
			}
			wantPending := before
			if test.want == guardMatrixAccepted {
				wantPending++
			}
			if got := d.pending(t); got != wantPending {
				t.Fatalf("pending = %d, want %d (%s)", got, wantPending, detail)
			}
		})
	}
}

func mcpMatrixOutcome(result *mcp.CallToolResult, priorOutboxID string) (guardMatrixOutcome, string) {
	payload, _ := result.StructuredContent.(map[string]any)
	detail := fmt.Sprint(payload)
	switch {
	case !result.IsError:
		return guardMatrixAccepted, detail
	case payload["error_kind"] == localapi.NearDuplicateErrorKind && payload["duplicate_of_outbox_id"] == priorOutboxID:
		return guardMatrixNearDuplicate, detail
	default:
		return guardMatrixOtherFailure, detail
	}
}

func cliMatrixOutcome(err error, priorOutboxID string) (guardMatrixOutcome, string) {
	switch {
	case err == nil:
		return guardMatrixAccepted, "ok"
	case strings.Contains(err.Error(), "near-duplicate of outbox "+priorOutboxID) && strings.Contains(err.Error(), "--force"):
		return guardMatrixNearDuplicate, err.Error()
	default:
		return guardMatrixOtherFailure, err.Error()
	}
}

func httpMatrixOutcome(status int) guardMatrixOutcome {
	switch status {
	case http.StatusOK:
		return guardMatrixAccepted
	case http.StatusConflict:
		return guardMatrixNearDuplicate
	default:
		return guardMatrixOtherFailure
	}
}

// A refused CLI send can be rerun with --force under the same key: the
// refusal wrote nothing.
func TestCLINearDuplicateThenForceReusesKey(t *testing.T) {
	d := newGuardMatrixDaemon(t)
	if err := d.cliSend(t, "cli-first", "Running 5 minutes late, sorry", false); err != nil {
		t.Fatal(err)
	}
	err := d.cliSend(t, "cli-second", "Running 10 minutes late, sorry", false)
	if err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("near-duplicate CLI send error = %v, want --force guidance", err)
	}
	var ambiguous *ambiguousCLIError
	if errors.As(err, &ambiguous) {
		t.Fatalf("near-duplicate refusal reported as ambiguous: %v", err)
	}
	if err := d.cliSend(t, "cli-second", "Running 10 minutes late, sorry", true); err != nil {
		t.Fatalf("forced rerun with the refused key: %v", err)
	}
	if got := d.pending(t); got != 2 {
		t.Fatalf("pending = %d, want 2", got)
	}
}
