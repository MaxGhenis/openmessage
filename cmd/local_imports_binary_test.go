package cmd_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestBuiltBinaryLocalImportsDecision checks the decision a real daemon
// process makes from its environment: the launcher's sandbox flag (now "0"
// on the unsandboxed app), an inherited App Sandbox container id, and the
// v2-primary lever. HOME points at an empty directory so an enabled run
// cannot read the developer's own Messages, Signal or WhatsApp data.
func TestBuiltBinaryLocalImportsDecision(t *testing.T) {
	binary, _ := buildTestBinary(t)

	run := func(t *testing.T, dataDir string, env ...string) string {
		t.Helper()
		home := t.TempDir()
		cmd := exec.Command(binary, "serve", "--mcp-stdio", "--transports")
		cmd.Env = append(
			os.Environ(),
			"HOME="+home,
			"OPENMESSAGES_DATA_DIR="+dataDir,
			"OPENMESSAGES_DEMO=0",
			"OPENMESSAGES_MACOS_NOTIFICATIONS=0",
			"OPENMESSAGES_LOG_LEVEL=info",
			"OPENMESSAGE_TELEMETRY=0",
			"OPENMESSAGES_STARTUP_BACKFILL=off",
			"APP_SANDBOX_CONTAINER_ID=",
		)
		cmd.Env = append(cmd.Env, env...)
		cmd.Stdin = strings.NewReader("")
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("serve --mcp-stdio --transports: %v\n%s", err, output)
		}
		return string(output)
	}
	const (
		on        = "Local desktop imports on:"
		v2Primary = "Local desktop imports off: this daemon reads the v2 store"
		sandbox   = "Local desktop imports off: the app runs in the macOS App Sandbox"
	)
	expectOnly := func(t *testing.T, output, want string) {
		t.Helper()
		for _, line := range []string{on, v2Primary, sandbox} {
			if got := strings.Count(output, line); (line == want) != (got == 1) || got > 1 {
				t.Fatalf("%q appears %d times, want it only for %q:\n%s", line, got, want, output)
			}
		}
	}

	t.Run("unsandboxed legacy-primary daemon runs them", func(t *testing.T) {
		dataDir := filepath.Join(t.TempDir(), "data")
		expectOnly(t, run(t, dataDir, "OPENMESSAGES_APP_SANDBOX=0", "OPENMESSAGES_V2_PRIMARY=0"), on)
	})
	t.Run("launcher flag keeps them off", func(t *testing.T) {
		dataDir := filepath.Join(t.TempDir(), "data")
		expectOnly(t, run(t, dataDir, "OPENMESSAGES_APP_SANDBOX=1", "OPENMESSAGES_V2_PRIMARY=0"), sandbox)
	})
	t.Run("inherited sandbox container id keeps them off", func(t *testing.T) {
		dataDir := filepath.Join(t.TempDir(), "data")
		expectOnly(t, run(t, dataDir, "OPENMESSAGES_APP_SANDBOX=0", "APP_SANDBOX_CONTAINER_ID=com.openmessage.app"), sandbox)
	})
	t.Run("unsandboxed v2-primary daemon keeps them off", func(t *testing.T) {
		dataDir := filepath.Join(t.TempDir(), "data")
		// Provision the v2 store the primary lever requires.
		run(t, dataDir, "OPENMESSAGES_APP_SANDBOX=1", "OPENMESSAGES_V2_INGEST=1")
		if _, err := os.Stat(filepath.Join(dataDir, "v2", "store.sqlite3")); err != nil {
			t.Fatalf("v2 store not provisioned: %v", err)
		}
		expectOnly(t, run(t, dataDir, "OPENMESSAGES_APP_SANDBOX=0", "OPENMESSAGES_V2_PRIMARY=1"), v2Primary)
	})
}
