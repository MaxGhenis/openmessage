package signallive

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

func TestSignalCLIEnvReplacesTmpDirAndSetsJavaOpt(t *testing.T) {
	base := []string{"PATH=/usr/bin", "TMPDIR=/var/folders/xx/T", "HOME=/Users/u"}
	env := signalCLIEnv(base, "/data/signal-cli/tmp/run-1")

	var tmpdir, opts string
	for _, kv := range env {
		if strings.HasPrefix(kv, "TMPDIR=") {
			if tmpdir != "" {
				t.Fatalf("duplicate TMPDIR entries in env: %v", env)
			}
			tmpdir = strings.TrimPrefix(kv, "TMPDIR=")
		}
		if strings.HasPrefix(kv, "SIGNAL_CLI_OPTS=") {
			opts = strings.TrimPrefix(kv, "SIGNAL_CLI_OPTS=")
		}
	}
	if tmpdir != "/data/signal-cli/tmp/run-1" {
		t.Fatalf("TMPDIR = %q, want confined dir", tmpdir)
	}
	if opts != "-Djava.io.tmpdir=/data/signal-cli/tmp/run-1" {
		t.Fatalf("SIGNAL_CLI_OPTS = %q, want java.io.tmpdir flag", opts)
	}
	joined := strings.Join(env, "\n")
	if !strings.Contains(joined, "PATH=/usr/bin") || !strings.Contains(joined, "HOME=/Users/u") {
		t.Fatalf("unrelated env vars must pass through: %v", env)
	}
}

func TestSignalCLIEnvAppendsToExistingOptsSoOursWins(t *testing.T) {
	base := []string{"SIGNAL_CLI_OPTS=-Xmx512m -Djava.io.tmpdir=/users/choice"}
	env := signalCLIEnv(base, "/confined")

	var opts string
	for _, kv := range env {
		if strings.HasPrefix(kv, "SIGNAL_CLI_OPTS=") {
			opts = strings.TrimPrefix(kv, "SIGNAL_CLI_OPTS=")
		}
	}
	if !strings.HasPrefix(opts, "-Xmx512m") {
		t.Fatalf("user opts not preserved: %q", opts)
	}
	if !strings.HasSuffix(opts, "-Djava.io.tmpdir=/confined") {
		t.Fatalf("confined tmpdir flag must come last so it wins: %q", opts)
	}
}

func TestNewSignalRunTmpDirCreatesAndCleansUp(t *testing.T) {
	configDir := t.TempDir()
	dir, cleanup, err := newSignalRunTmpDir(configDir)
	if err != nil {
		t.Fatalf("newSignalRunTmpDir: %v", err)
	}
	if !strings.HasPrefix(dir, signalTmpRoot(configDir)) {
		t.Fatalf("run dir %q not under tmp root %q", dir, signalTmpRoot(configDir))
	}
	if err := os.WriteFile(filepath.Join(dir, "libsignal.so"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	cleanup()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("run dir still exists after cleanup: %v", err)
	}
}

func TestSweepSignalTmpRootRemovesOnlyStaleEntries(t *testing.T) {
	configDir := t.TempDir()
	root := signalTmpRoot(configDir)
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}

	stale := filepath.Join(root, "run-stale")
	fresh := filepath.Join(root, "run-fresh")
	for _, dir := range []string{stale, fresh} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "payload"), make([]byte, 128), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	staleFile := filepath.Join(root, "stray-file")
	if err := os.WriteFile(staleFile, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * signalRunTmpMaxAge)
	for _, p := range []string{stale, staleFile} {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}

	sweepSignalTmpRoot(zerolog.Nop(), configDir, signalRunTmpMaxAge)

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale run dir should be removed, stat err = %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("fresh run dir should survive: %v", err)
	}
	if _, err := os.Stat(staleFile); err != nil {
		t.Fatalf("plain files should never be swept: %v", err)
	}
}

func TestSweepLegacyLibsignalTempFiltersByNameAndAge(t *testing.T) {
	tempRoot := t.TempDir()
	t.Setenv("TMPDIR", tempRoot)

	oldLeak := filepath.Join(tempRoot, "libsignal12345")
	freshLeak := filepath.Join(tempRoot, "libsignal67890")
	unrelated := filepath.Join(tempRoot, "com.apple.thing")
	for _, dir := range []string{oldLeak, freshLeak, unrelated} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-2 * legacyLibsignalMaxAge)
	for _, p := range []string{oldLeak, unrelated} {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}

	sweepLegacyLibsignalTemp(zerolog.Nop())

	if _, err := os.Stat(oldLeak); !os.IsNotExist(err) {
		t.Fatalf("old libsignal dir should be removed, stat err = %v", err)
	}
	if _, err := os.Stat(freshLeak); err != nil {
		t.Fatalf("fresh libsignal dir should survive (could be live): %v", err)
	}
	if _, err := os.Stat(unrelated); err != nil {
		t.Fatalf("non-libsignal dir must never be touched: %v", err)
	}
}

func TestSweepRespectsOptOut(t *testing.T) {
	tempRoot := t.TempDir()
	t.Setenv("TMPDIR", tempRoot)
	t.Setenv(signalTmpSweepEnvVar, "0")

	leak := filepath.Join(tempRoot, "libsignal-optout")
	if err := os.MkdirAll(leak, 0o700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * legacyLibsignalMaxAge)
	if err := os.Chtimes(leak, old, old); err != nil {
		t.Fatal(err)
	}

	sweepLegacyLibsignalTemp(zerolog.Nop())
	sweepSignalTmpRoot(zerolog.Nop(), tempRoot, signalRunTmpMaxAge)

	if _, err := os.Stat(leak); err != nil {
		t.Fatalf("opt-out must disable all sweeping: %v", err)
	}
}

// TestRunSignalCLIConfinesTempAndCleansUp exercises the real runSignalCLI
// against a stub executable to prove the subprocess sees the confined
// TMPDIR/SIGNAL_CLI_OPTS and that the per-run dir is removed afterwards.
func TestRunSignalCLIConfinesTempAndCleansUp(t *testing.T) {
	configDir := t.TempDir()
	stub := filepath.Join(t.TempDir(), "signal-cli-stub")
	script := "#!/bin/sh\nprintf '%s|%s' \"$TMPDIR\" \"$SIGNAL_CLI_OPTS\"\nmkdir -p \"$TMPDIR/libsignal-test\"\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENMESSAGES_SIGNAL_CLI", stub)

	out, err := runSignalCLI(context.Background(), configDir, "receive")
	if err != nil {
		t.Fatalf("runSignalCLI: %v (out=%s)", err, out)
	}
	parts := strings.SplitN(string(out), "|", 2)
	if len(parts) != 2 {
		t.Fatalf("unexpected stub output: %q", out)
	}
	tmpdir, opts := parts[0], parts[1]
	if !strings.HasPrefix(tmpdir, signalTmpRoot(configDir)) {
		t.Fatalf("subprocess TMPDIR %q not confined under %q", tmpdir, signalTmpRoot(configDir))
	}
	if !strings.Contains(opts, "-Djava.io.tmpdir="+tmpdir) {
		t.Fatalf("subprocess SIGNAL_CLI_OPTS %q missing java.io.tmpdir", opts)
	}
	if _, err := os.Stat(tmpdir); !os.IsNotExist(err) {
		t.Fatalf("per-run tmp dir %q should be removed after the run, stat err = %v", tmpdir, err)
	}
	entries, err := os.ReadDir(signalTmpRoot(configDir))
	if err != nil {
		t.Fatalf("tmp root should still exist: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("tmp root should be empty after run, has %d entries", len(entries))
	}
}
