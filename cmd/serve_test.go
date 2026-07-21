package cmd

import (
	"bytes"
	"context"
	"errors"
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
	"github.com/rs/zerolog"

	"github.com/maxghenis/openmessage/internal/whatsapplive"
)

func TestInitializeWhatsAppForServeSkipsSupervisorOnInitializationError(t *testing.T) {
	var logs bytes.Buffer
	logger := zerolog.New(&logs)
	initErr := errors.New("unable to open WhatsApp database")

	whatsappBridge, initialized := initializeWhatsAppForServe(logger, func() (*whatsapplive.Bridge, error) {
		return nil, initErr
	})

	if initialized {
		t.Fatal("expected WhatsApp supervisor wiring to be skipped")
	}
	if whatsappBridge != nil {
		t.Fatal("expected no WhatsApp bridge after initialization failure")
	}
	logOutput := logs.String()
	if !strings.Contains(logOutput, "WhatsApp live bridge unavailable") || !strings.Contains(logOutput, initErr.Error()) {
		t.Fatalf("warning log = %q, want unavailable message and initialization error", logOutput)
	}
}

func TestRunServeV2PrimaryFailsFast(t *testing.T) {
	tests := []struct {
		name          string
		args          []string
		send          string
		ingest        string
		errorContains string
	}{
		{
			name:          "demo mode",
			args:          []string{"--demo", "--mcp-stdio"},
			errorContains: "OPENMESSAGES_V2_PRIMARY is not available in demo mode",
		},
		{
			name:          "explicit send conflict",
			args:          []string{"--mcp-stdio"},
			send:          "0",
			errorContains: "OPENMESSAGES_V2_PRIMARY requires v2 send and ingest",
		},
		{
			name:          "explicit ingest conflict",
			args:          []string{"--mcp-stdio"},
			ingest:        "0",
			errorContains: "OPENMESSAGES_V2_PRIMARY requires v2 send and ingest",
		},
		{
			name:          "migrated store absent",
			args:          []string{"--mcp-stdio"},
			errorContains: "run: openmessage migrate",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dataDir := t.TempDir()
			t.Setenv("OPENMESSAGES_DATA_DIR", dataDir)
			t.Setenv("OPENMESSAGES_DEMO", "0")
			t.Setenv("OPENMESSAGES_V2_PRIMARY", "1")
			t.Setenv("OPENMESSAGES_V2_SEND", tt.send)
			t.Setenv("OPENMESSAGES_V2_INGEST", tt.ingest)
			t.Setenv("OPENMESSAGES_APP_SANDBOX", "1")
			t.Setenv("OPENMESSAGES_MACOS_NOTIFICATIONS", "0")
			t.Setenv("OPENMESSAGE_TELEMETRY", "0")
			// Keep the client shape's daemon probe hermetic: never reach a
			// real daemon that may be running on the test machine.
			t.Setenv("OPENMESSAGES_PORT", "0")

			err := RunServe(zerolog.Nop(), tt.args...)
			if err == nil {
				t.Fatal("RunServe() succeeded, want startup error")
			}
			if !strings.Contains(err.Error(), tt.errorContains) {
				t.Fatalf("RunServe() error = %q, want %q", err, tt.errorContains)
			}
			if tt.name == "migrated store absent" {
				wantPath := filepath.Join(dataDir, "v2", "store.sqlite3")
				if !strings.Contains(err.Error(), wantPath) {
					t.Fatalf("RunServe() error = %q, want store path %q", err, wantPath)
				}
				if _, statErr := os.Stat(filepath.Join(dataDir, "v2")); !os.IsNotExist(statErr) {
					t.Fatalf("absent-store refusal created v2 state: %v", statErr)
				}
				if _, statErr := os.Stat(filepath.Join(dataDir, "messages.db")); !os.IsNotExist(statErr) {
					t.Fatalf("absent-store refusal touched the legacy store: %v", statErr)
				}
			}
		})
	}
}

func TestRunServeV2PrimaryRejectsCorruptPresentStoreWithoutFallback(t *testing.T) {
	tests := []struct {
		name           string
		args           []string
		errorContains  string
		wantNoLegacyDB bool
	}{
		// Daemon shape (explicit transports): the full v2 stack refuses.
		{name: "daemon shape", args: []string{"--mcp-stdio", "--transports"}, errorContains: "init v2 stack"},
		// MCP client shape: the read-store attach refuses before app init,
		// so the refusal leaves no legacy store behind either.
		{name: "client shape", args: []string{"--mcp-stdio"}, errorContains: "open v2 read store", wantNoLegacyDB: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dataDir := t.TempDir()
			v2Dir := filepath.Join(dataDir, "v2")
			if err := os.MkdirAll(v2Dir, 0o700); err != nil {
				t.Fatalf("create v2 dir: %v", err)
			}
			storePath := filepath.Join(v2Dir, "store.sqlite3")
			corrupt := []byte("not a sqlite database")
			if err := os.WriteFile(storePath, corrupt, 0o600); err != nil {
				t.Fatalf("write corrupt store: %v", err)
			}
			t.Setenv("OPENMESSAGES_DATA_DIR", dataDir)
			t.Setenv("OPENMESSAGES_DEMO", "0")
			t.Setenv("OPENMESSAGES_V2_PRIMARY", "1")
			t.Setenv("OPENMESSAGES_V2_SEND", "")
			t.Setenv("OPENMESSAGES_V2_INGEST", "")
			t.Setenv("OPENMESSAGES_APP_SANDBOX", "1")
			t.Setenv("OPENMESSAGES_MACOS_NOTIFICATIONS", "0")
			t.Setenv("OPENMESSAGE_TELEMETRY", "0")
			// Keep the client shape's daemon probe hermetic: never reach a
			// real daemon that may be running on the test machine.
			t.Setenv("OPENMESSAGES_PORT", "0")

			err := RunServe(zerolog.Nop(), tt.args...)
			if err == nil || !strings.Contains(err.Error(), tt.errorContains) {
				t.Fatalf("RunServe() error = %v, want corrupt v2 store startup failure containing %q", err, tt.errorContains)
			}
			got, readErr := os.ReadFile(storePath)
			if readErr != nil {
				t.Fatalf("read corrupt sentinel: %v", readErr)
			}
			if !bytes.Equal(got, corrupt) {
				t.Fatalf("corrupt v2 store was replaced: got %q, want %q", got, corrupt)
			}
			if _, statErr := os.Stat(filepath.Join(v2Dir, "blobs")); !os.IsNotExist(statErr) {
				t.Fatalf("corrupt-store refusal continued into v2 blob setup: %v", statErr)
			}
			if tt.wantNoLegacyDB {
				if _, statErr := os.Stat(filepath.Join(dataDir, "messages.db")); !os.IsNotExist(statErr) {
					t.Fatalf("corrupt-store refusal touched the legacy store: %v", statErr)
				}
			}
		})
	}
}

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
	// Point the native refresh at an empty profile dir so NativeSupported()
	// is false regardless of whether the CI host has a real Chrome profile;
	// this test asserts the no-script, no-native path is a clean no-op.
	t.Setenv("OPENMESSAGE_CHROME_PROFILE", t.TempDir())

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := refreshGoogleSessionCookies(ctx, ""); err != nil {
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
	if err := refreshGoogleSessionCookies(ctx, ""); err != nil {
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

func TestParseServeOptions(t *testing.T) {
	t.Run("defaults to web only", func(t *testing.T) {
		opts, err := parseServeOptions(nil)
		if err != nil {
			t.Fatalf("parseServeOptions(): %v", err)
		}
		if opts.demo {
			t.Fatal("expected demo=false by default")
		}
		if !opts.web || opts.mcpSSE || opts.mcpStdio {
			t.Fatalf("unexpected default serve options: %+v", opts)
		}
	})

	t.Run("MCP SSE flag enables MCP SSE only", func(t *testing.T) {
		opts, err := parseServeOptions([]string{"--mcp-sse"})
		if err != nil {
			t.Fatalf("parseServeOptions(): %v", err)
		}
		if opts.web || !opts.mcpSSE || opts.mcpStdio {
			t.Fatalf("unexpected serve options: %+v", opts)
		}
	})

	t.Run("web and MCP SSE can be enabled together", func(t *testing.T) {
		opts, err := parseServeOptions([]string{"--web", "--mcp-sse"})
		if err != nil {
			t.Fatalf("parseServeOptions(): %v", err)
		}
		if !opts.web || !opts.mcpSSE || opts.mcpStdio {
			t.Fatalf("unexpected serve options: %+v", opts)
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
