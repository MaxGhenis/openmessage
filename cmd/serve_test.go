package cmd

import (
	"io"
	"net"
	"net/http"
	"os"
	"testing"
	"time"
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
