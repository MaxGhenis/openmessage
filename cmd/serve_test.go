package cmd

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/maxghenis/openmessage/internal/app"
)

func TestHTTPServerSurvivesIndependently(t *testing.T) {
	// Mirrors the RunServe architecture: HTTP server in a goroutine
	// stays alive even when the "main" blocking call returns.

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	go http.Serve(ln, mux)

	// Simulate MCP stdio returning immediately (like EOF from /dev/null)
	done := make(chan struct{})
	go func() {
		// This returns instantly, simulating ServeStdio on closed stdin
		close(done)
	}()
	<-done

	// HTTP server should still be alive after "MCP" exits
	time.Sleep(50 * time.Millisecond)

	resp, err := http.Get("http://" + ln.Addr().String() + "/healthz")
	if err != nil {
		t.Fatalf("HTTP server not responding after MCP exit: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if string(body) != "ok" {
		t.Fatalf("got %q, want %q", body, "ok")
	}
}

func TestMCPHTTPHandlerServesCodexAndClaudeTransports(t *testing.T) {
	mcpSrv := mcpserver.NewMCPServer("test-openmessage", "test", mcpserver.WithToolCapabilities(true))
	srv := httptest.NewServer(newMCPHTTPHandler(mcpSrv, "http://example.test"))
	defer srv.Close()

	initBody := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"test","version":"0"}}}`)
	resp, err := http.Post(srv.URL+"/mcp", "application/json", bytes.NewReader(initBody))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("streamable HTTP /mcp status = %d, want 200", resp.StatusCode)
	}

	req, _ := http.NewRequest("GET", srv.URL+"/mcp/sse", nil)
	resp2, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("SSE /mcp/sse status = %d, want 200", resp2.StatusCode)
	}
	if ct := resp2.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("SSE content-type = %q, want text/event-stream", ct)
	}
}

func TestMacOSNotificationsEnabled(t *testing.T) {
	originalGOOS := runtimeGOOS
	t.Cleanup(func() {
		runtimeGOOS = originalGOOS
	})

	t.Run("defaults on for interactive darwin", func(t *testing.T) {
		runtimeGOOS = func() string { return "darwin" }
		t.Setenv("OPENMESSAGES_MACOS_NOTIFICATIONS", "")
		if !macOSNotificationsEnabled(true) {
			t.Fatal("expected notifications enabled for interactive macOS serve")
		}
	})

	t.Run("defaults off for stdio launches", func(t *testing.T) {
		runtimeGOOS = func() string { return "darwin" }
		t.Setenv("OPENMESSAGES_MACOS_NOTIFICATIONS", "")
		if macOSNotificationsEnabled(false) {
			t.Fatal("expected notifications disabled for non-interactive launch")
		}
	})

	t.Run("env can force on outside darwin", func(t *testing.T) {
		runtimeGOOS = func() string { return "linux" }
		t.Setenv("OPENMESSAGES_MACOS_NOTIFICATIONS", "true")
		if !macOSNotificationsEnabled(false) {
			t.Fatal("expected env override to force notifications on")
		}
	})

	t.Run("env can force off on darwin", func(t *testing.T) {
		runtimeGOOS = func() string { return "darwin" }
		t.Setenv("OPENMESSAGES_MACOS_NOTIFICATIONS", "0")
		if macOSNotificationsEnabled(true) {
			t.Fatal("expected env override to disable notifications")
		}
	})
}

func TestIMessageSyncSupported(t *testing.T) {
	originalGOOS := runtimeGOOS
	t.Cleanup(func() {
		runtimeGOOS = originalGOOS
	})

	runtimeGOOS = func() string { return "darwin" }
	if !iMessageSyncSupported() {
		t.Fatal("expected iMessage sync to be supported on darwin")
	}

	runtimeGOOS = func() string { return "windows" }
	if iMessageSyncSupported() {
		t.Fatal("expected iMessage sync to be unsupported on windows")
	}
}

func TestRefreshGoogleSessionCookiesSkipsWhenUnconfigured(t *testing.T) {
	t.Setenv("OPENMESSAGE_COOKIE_REFRESH_SCRIPT", "")

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := refreshGoogleSessionCookies(ctx); err != nil {
		t.Fatalf("refreshGoogleSessionCookies(): %v", err)
	}
}

func TestRefreshGoogleSessionCookiesUsesEnvScript(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "refresh.sh")
	argsPath := filepath.Join(dir, "args")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s' \"$*\" > \"$ARGS_PATH\"\n"), 0o700); err != nil {
		t.Fatalf("write script: %v", err)
	}
	t.Setenv("OPENMESSAGE_COOKIE_REFRESH_SCRIPT", script)
	t.Setenv("ARGS_PATH", argsPath)

	// Generous deadline: the script is trivial, but a 1s budget flakes under
	// heavy CI load (fork+exec of /bin/sh can slip past it), surfacing as a
	// spurious "timed out" failure.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := refreshGoogleSessionCookies(ctx); err != nil {
		t.Fatalf("refreshGoogleSessionCookies(): %v", err)
	}

	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatalf("read args: %v", err)
	}
	if got := strings.TrimSpace(string(args)); got != "--quiet --no-backup" {
		t.Fatalf("args = %q, want %q", got, "--quiet --no-backup")
	}
}

func TestGoogleStatusNeedsCookieRefreshUsesExplicitAuthExpiredFlag(t *testing.T) {
	status := app.GoogleStatusSnapshot{
		Paired:      true,
		AuthExpired: true,
		LastError:   "Google Messages connection lost; reconnecting...",
	}
	if !googleStatusNeedsCookieRefresh(status) {
		t.Fatal("expected explicit auth-expired flag to trigger cookie refresh")
	}

	status.AuthExpired = false
	status.LastError = "Google Messages session cookie expired; refreshing and reconnecting..."
	if !googleStatusNeedsCookieRefresh(status) {
		t.Fatal("expected auth-expired last error to trigger cookie refresh")
	}

	status.LastError = "Google Messages connection lost; reconnecting..."
	if googleStatusNeedsCookieRefresh(status) {
		t.Fatal("generic reconnect status should not trigger cookie refresh")
	}
}

func TestParseServeOptions(t *testing.T) {
	t.Run("defaults to normal serve", func(t *testing.T) {
		opts, err := parseServeOptions(nil)
		if err != nil {
			t.Fatalf("parseServeOptions(): %v", err)
		}
		if opts.demo {
			t.Fatal("expected demo=false by default")
		}
		if !opts.web || !opts.mcpSSE || opts.mcpStdio {
			t.Fatalf("unexpected default serve options: %+v", opts)
		}
	})

	t.Run("accepts explicit transport flags", func(t *testing.T) {
		opts, err := parseServeOptions([]string{"--demo", "--no-web", "--no-mcp-sse", "--mcp-stdio"})
		if err != nil {
			t.Fatalf("parseServeOptions(): %v", err)
		}
		if !opts.demo || opts.web || opts.mcpSSE || !opts.mcpStdio {
			t.Fatalf("unexpected serve options: %+v", opts)
		}
	})

	t.Run("transport flags switch serve into explicit mode", func(t *testing.T) {
		opts, err := parseServeOptions([]string{"--mcp-stdio"})
		if err != nil {
			t.Fatalf("parseServeOptions(): %v", err)
		}
		if opts.web || opts.mcpSSE || !opts.mcpStdio {
			t.Fatalf("unexpected serve options: %+v", opts)
		}
	})

	t.Run("rejects empty transport set", func(t *testing.T) {
		if _, err := parseServeOptions([]string{"--no-web", "--no-mcp-sse"}); err == nil {
			t.Fatal("expected error when every transport is disabled")
		}
	})

	t.Run("rejects unknown flags and removed aliases", func(t *testing.T) {
		if _, err := parseServeOptions([]string{"--mock"}); err == nil {
			t.Fatal("expected error for removed serve alias")
		}
		if _, err := parseServeOptions([]string{"--wat"}); err == nil {
			t.Fatal("expected error for unknown serve option")
		}
	})
}

func TestConfigureServeEnvRestoresPreviousValue(t *testing.T) {
	original, hadOriginal := os.LookupEnv("OPENMESSAGES_DEMO")
	t.Cleanup(func() {
		if hadOriginal {
			_ = os.Setenv("OPENMESSAGES_DEMO", original)
			return
		}
		_ = os.Unsetenv("OPENMESSAGES_DEMO")
	})

	_ = os.Unsetenv("OPENMESSAGES_DEMO")
	restore := configureServeEnv(serveOptions{demo: true})
	if got := os.Getenv("OPENMESSAGES_DEMO"); got != "1" {
		t.Fatalf("OPENMESSAGES_DEMO=%q, want 1", got)
	}
	restore()
	if _, ok := os.LookupEnv("OPENMESSAGES_DEMO"); ok {
		t.Fatal("expected OPENMESSAGES_DEMO to be unset after restore")
	}

	_ = os.Setenv("OPENMESSAGES_DEMO", "existing")
	restore = configureServeEnv(serveOptions{demo: true})
	if got := os.Getenv("OPENMESSAGES_DEMO"); got != "1" {
		t.Fatalf("OPENMESSAGES_DEMO=%q, want 1 during demo", got)
	}
	restore()
	if got := os.Getenv("OPENMESSAGES_DEMO"); got != "existing" {
		t.Fatalf("OPENMESSAGES_DEMO=%q, want existing after restore", got)
	}
}

func TestPlanGoogleReconnect(t *testing.T) {
	const authErr = "send message: HTTP 401: Request had invalid authentication credentials"
	tests := []struct {
		name      string
		status    app.GoogleStatusSnapshot
		hasScript bool
		want      googleReconnectAction
	}{
		{
			name:   "connected does nothing",
			status: app.GoogleStatusSnapshot{Paired: true, Connected: true},
			want:   googleReconnectSkip,
		},
		{
			name:   "unpaired does nothing",
			status: app.GoogleStatusSnapshot{Paired: false},
			want:   googleReconnectSkip,
		},
		{
			name:   "awaiting first-time pairing does nothing",
			status: app.GoogleStatusSnapshot{Paired: true, NeedsPairing: true},
			want:   googleReconnectSkip,
		},
		{
			name:   "ordinary disconnect retries",
			status: app.GoogleStatusSnapshot{Paired: true},
			want:   googleReconnectRetry,
		},
		{
			name:      "auth expired with refresh script refreshes",
			status:    app.GoogleStatusSnapshot{Paired: true, AuthExpired: true},
			hasScript: true,
			want:      googleReconnectRefresh,
		},
		{
			name:   "auth expired without refresh script parks (no storm)",
			status: app.GoogleStatusSnapshot{Paired: true, AuthExpired: true},
			want:   googleReconnectPark,
		},
		{
			name:   "auth expired via last-error text without script parks",
			status: app.GoogleStatusSnapshot{Paired: true, LastError: authErr},
			want:   googleReconnectPark,
		},
		{
			name:      "dead session flagged for repair still refreshes when a script can recover it",
			status:    app.GoogleStatusSnapshot{Paired: true, NeedsRepair: true, AuthExpired: true},
			hasScript: true,
			want:      googleReconnectRefresh,
		},
		{
			name:   "dead session flagged for repair without script parks (macOS)",
			status: app.GoogleStatusSnapshot{Paired: true, NeedsRepair: true, AuthExpired: true},
			want:   googleReconnectPark,
		},
		{
			name:   "zombie session needs repair but not auth-expired parks",
			status: app.GoogleStatusSnapshot{Paired: true, NeedsRepair: true},
			want:   googleReconnectPark,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := planGoogleReconnect(tc.status, tc.hasScript); got != tc.want {
				t.Fatalf("planGoogleReconnect(%+v, hasScript=%v) = %d, want %d", tc.status, tc.hasScript, got, tc.want)
			}
		})
	}
}
