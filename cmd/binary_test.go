package cmd_test

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// buildTestBinary compiles the openmessage binary into a temp directory and
// returns the binary path and a clean data directory for OPENMESSAGES_DATA_DIR.
func buildTestBinary(t *testing.T) (binary, dataDir string) {
	t.Helper()
	tmpDir := t.TempDir()
	binary = filepath.Join(tmpDir, "openmessage")
	build := exec.Command("go", "build", "-o", binary, "..")
	build.Dir = filepath.Join(".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build failed: %v\n%s", err, out)
	}
	dataDir = filepath.Join(tmpDir, "data")
	os.MkdirAll(dataDir, 0700)
	return binary, dataDir
}

// TestBuiltBinaryAcceptsPairCommand verifies the compiled binary doesn't crash
// on startup when given the "pair" command. It should start the pairing flow
// (which will eventually fail without a phone), not exit with a panic or
// immediate error.
func TestBuiltBinaryAcceptsPairCommand(t *testing.T) {
	binary, dataDir := buildTestBinary(t)

	cmd := exec.Command(binary, "pair")
	cmd.Env = append(os.Environ(), "OPENMESSAGES_DATA_DIR="+dataDir)

	out := &strings.Builder{}
	cmd.Stdout = out
	cmd.Stderr = out

	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start binary: %v", err)
	}

	// We only want to confirm the binary doesn't crash on startup. Surviving
	// the grace period is a pass in itself: pairing may legitimately produce
	// no output for a while (it contacts Google before printing the QR), so
	// a kill-then-inspect approach can't tell a quiet healthy process from a
	// crashed one.
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		// Exited on its own — fine if it said something first (e.g. a
		// pairing error without a phone), a crash if it was silent.
		if err != nil && len(out.String()) == 0 {
			t.Errorf("binary exited immediately with no output: %v", err)
		}
	case <-time.After(3 * time.Second):
		// Still running after the grace period: startup succeeded.
		_ = cmd.Process.Kill()
		<-done
	}

	// Should not contain panic traces in either outcome.
	output := out.String()
	if strings.Contains(output, "panic:") || strings.Contains(output, "runtime error") {
		t.Errorf("binary panicked:\n%s", output)
	}
}

// TestBuiltBinaryRejectsSendGroupNoArgs verifies that running "send-group"
// with no additional arguments exits non-zero and prints usage information.
func TestBuiltBinaryRejectsSendGroupNoArgs(t *testing.T) {
	binary, dataDir := buildTestBinary(t)

	cmd := exec.Command(binary, "send-group")
	cmd.Env = append(os.Environ(), "OPENMESSAGES_DATA_DIR="+dataDir)

	out, err := cmd.CombinedOutput()
	output := string(out)

	if err == nil {
		t.Fatal("expected non-zero exit code, but command succeeded")
	}

	if !strings.Contains(output, "Usage") && !strings.Contains(output, "send-group") {
		t.Errorf("expected output to contain 'Usage' or 'send-group', got:\n%s", output)
	}
}

func TestBuiltBinaryDemoServesSeededDataWithoutTouchingConfiguredDataDir(t *testing.T) {
	binary, dataDir := buildTestBinary(t)
	port := reserveTCPPort(t)

	cmd := exec.Command(binary, "demo")
	cmd.Env = append(
		os.Environ(),
		"OPENMESSAGES_DATA_DIR="+dataDir,
		"OPENMESSAGES_HOST=127.0.0.1",
		"OPENMESSAGES_PORT="+port,
	)

	out := &strings.Builder{}
	cmd.Stdout = out
	cmd.Stderr = out

	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start demo binary: %v", err)
	}
	defer stopProcess(t, cmd)

	baseURL := "http://127.0.0.1:" + port
	if err := waitForHTTP(baseURL+"/api/status", 5*time.Second); err != nil {
		t.Fatalf("demo server did not become ready: %v\n%s", err, out.String())
	}

	resp, err := http.Get(baseURL + "/api/status")
	if err != nil {
		t.Fatalf("GET /api/status: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/status status = %d, want 200\n%s", resp.StatusCode, out.String())
	}

	var status map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatalf("decode /api/status: %v", err)
	}
	if connected, _ := status["connected"].(bool); !connected {
		t.Fatalf("status.connected = %v, want true", status["connected"])
	}

	resp, err = http.Get(baseURL + "/api/conversations?limit=50")
	if err != nil {
		t.Fatalf("GET /api/conversations: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/conversations status = %d, want 200\n%s", resp.StatusCode, out.String())
	}

	var conversations []struct {
		Name           string `json:"Name"`
		SourcePlatform string `json:"source_platform"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&conversations); err != nil {
		t.Fatalf("decode /api/conversations: %v", err)
	}
	if len(conversations) == 0 {
		t.Fatal("expected seeded demo conversations")
	}
	if !containsConversation(conversations, "Sarah Chen", "sms") {
		t.Fatalf("seeded SMS demo conversation missing: %+v", conversations)
	}
	if !containsConversation(conversations, "Jordan Rivera", "whatsapp") {
		t.Fatalf("seeded WhatsApp demo conversation missing: %+v", conversations)
	}

	entries, err := os.ReadDir(dataDir)
	if err != nil {
		t.Fatalf("ReadDir(%q): %v", dataDir, err)
	}
	if len(entries) != 0 {
		t.Fatalf("configured data dir should stay untouched in demo mode, found %d entries", len(entries))
	}
}

func reserveTCPPort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	defer ln.Close()

	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("unexpected addr type %T", ln.Addr())
	}
	return strconv.Itoa(addr.Port)
}

func waitForHTTP(url string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			lastErr = fmt.Errorf("status %d", resp.StatusCode)
		} else {
			lastErr = err
		}
		time.Sleep(100 * time.Millisecond)
	}
	return lastErr
}

func stopProcess(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	if cmd.Process == nil {
		return
	}
	_ = cmd.Process.Signal(os.Interrupt)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-time.After(3 * time.Second):
		_ = cmd.Process.Kill()
		<-done
	case <-done:
	}
}

func containsConversation(conversations []struct {
	Name           string `json:"Name"`
	SourcePlatform string `json:"source_platform"`
}, name, platform string) bool {
	for _, convo := range conversations {
		if convo.Name == name && convo.SourcePlatform == platform {
			return true
		}
	}
	return false
}
