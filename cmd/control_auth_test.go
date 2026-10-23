package cmd

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/localapi"
	"github.com/maxghenis/openmessage/internal/web"
	"github.com/rs/zerolog"
)

const mcpInitialize = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"probe","version":"1"}}}`

func postMCP(t *testing.T, url, token string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(mcpInitialize))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode
}

func readControlToken(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, web.ControlTokenFile))
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

// TestHTTPMCPRefusesUnauthenticatedCalls covers both ways serve exposes MCP
// over HTTP: alone (--mcp-sse) and mounted in the web handler (--web
// --mcp-sse). An unauthenticated client must not reach the MCP server, whose
// tools read every conversation and send messages and files.
func TestHTTPMCPRefusesUnauthenticatedCalls(t *testing.T) {
	dataDir := t.TempDir()
	auth, err := web.NewControlAuth(dataDir, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	token := readControlToken(t, dataDir)
	store, err := db.New(filepath.Join(t.TempDir(), "messages.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	mcp := newMCPHTTPHandler(mcpserver.NewMCPServer("openmessage-test", "1"), "http://127.0.0.1:7007")
	for name, handler := range map[string]http.Handler{
		"mcp only":  web.ProtectLocalControl(auth.Handler(mcp)),
		"web + mcp": web.APIHandlerWithOptions(store, nil, zerolog.Nop(), mcp, web.APIOptions{Auth: auth}),
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(handler)
			defer server.Close()
			if got := postMCP(t, server.URL+"/mcp", ""); got != http.StatusUnauthorized {
				t.Fatalf("unauthenticated MCP initialize = %d, want 401", got)
			}
			if got := postMCP(t, server.URL+"/mcp", "0"+token[1:]); got != http.StatusUnauthorized {
				t.Fatalf("wrong-token MCP initialize = %d, want 401", got)
			}
			if got := postMCP(t, server.URL+"/mcp", token); got != http.StatusOK {
				t.Fatalf("authenticated MCP initialize = %d, want 200", got)
			}
			resp, err := http.Get(server.URL + "/mcp/sse")
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("unauthenticated SSE = %d, want 401", resp.StatusCode)
			}
		})
	}
}

// TestRunSendFindsTheAppTokenAgainstAnEnforcingDaemon is the terminal case:
// the CLI's default data dir is not the one the app's daemon serves, so the
// default token is refused and the send must still authenticate.
func TestRunSendFindsTheAppTokenAgainstAnEnforcingDaemon(t *testing.T) {
	appDir := t.TempDir()
	auth, err := web.NewControlAuth(appDir, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	var reached []string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/status", func(w http.ResponseWriter, _ *http.Request) {
		reached = append(reached, "status")
		fmt.Fprintf(w, `{"v2_primary":true,"auth":{"data_dir":%q}}`, appDir)
	})
	mux.HandleFunc("/api/v1/outbox/messages", func(w http.ResponseWriter, _ *http.Request) {
		reached = append(reached, "submit")
		fmt.Fprint(w, `{"outbox_id":"out-1","state":"queued"}`)
	})
	mux.HandleFunc("/api/v1/outbox/out-1", func(w http.ResponseWriter, _ *http.Request) {
		reached = append(reached, "delivery")
		fmt.Fprint(w, `{"outbox_id":"out-1","state":"confirmed"}`)
	})
	server := httptest.NewServer(web.ProtectLocalControl(auth.Handler(mux)))
	defer server.Close()

	cliDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cliDir, web.ControlTokenFile), []byte(strings.Repeat("ab", 32)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OPENMESSAGES_DATA_DIR", appDir)
	deps := sendCommandDeps{
		client:       server.Client(),
		baseURL:      server.URL,
		controlToken: localapi.LoadControlToken(cliDir),
		newKey:       func() (string, error) { return "key", nil },
		output:       io.Discard,
		legacySend:   func(string, string) error { t.Fatal("legacy path"); return nil },
	}
	if err := runSendWithDeps(context.Background(), deps, "conv", "hello", nil); err != nil {
		t.Fatal(err)
	}
	if strings.Join(reached, ",") != "status,submit,delivery" {
		t.Fatalf("handlers reached = %q", reached)
	}

	// With no readable token anywhere, the send fails as refused, and no
	// handler runs.
	reached = nil
	t.Setenv("OPENMESSAGES_DATA_DIR", t.TempDir())
	deps.controlToken = ""
	err = runSendWithDeps(context.Background(), deps, "conv", "hello", nil)
	if err == nil || !localapi.IsUnauthorized(err) {
		t.Fatalf("err = %v, want a 401", err)
	}
	if len(reached) != 0 {
		t.Fatalf("handlers reached without a token: %q", reached)
	}
}
