package signallive

import (
	"context"
	"os"
	"os/exec"
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
	t.Setenv("TMPDIR", t.TempDir())
	dir, cleanup, err := newSignalRunTmpDir()
	if err != nil {
		t.Fatalf("newSignalRunTmpDir: %v", err)
	}
	if !strings.HasPrefix(dir, signalTmpRoot()) {
		t.Fatalf("run dir %q not under tmp root %q", dir, signalTmpRoot())
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
	t.Setenv("TMPDIR", t.TempDir())
	root := signalTmpRoot()
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

	sweepSignalTmpRoot(zerolog.Nop(), signalRunTmpMaxAge)

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
	sweepSignalTmpRoot(zerolog.Nop(), signalRunTmpMaxAge)

	if _, err := os.Stat(leak); err != nil {
		t.Fatalf("opt-out must disable all sweeping: %v", err)
	}
}

// TestRunSignalCLIConfinesTempAndCleansUp exercises the real runSignalCLI
// against a stub executable to prove the subprocess sees the confined
// TMPDIR/SIGNAL_CLI_OPTS and that the per-run dir is removed afterwards.
func TestRunSignalCLIConfinesTempAndCleansUp(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
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
	if !strings.HasPrefix(tmpdir, signalTmpRoot()) {
		t.Fatalf("subprocess TMPDIR %q not confined under %q", tmpdir, signalTmpRoot())
	}
	if !strings.Contains(opts, "-Djava.io.tmpdir="+tmpdir) {
		t.Fatalf("subprocess SIGNAL_CLI_OPTS %q missing java.io.tmpdir", opts)
	}
	if _, err := os.Stat(tmpdir); !os.IsNotExist(err) {
		t.Fatalf("per-run tmp dir %q should be removed after the run, stat err = %v", tmpdir, err)
	}
	entries, err := os.ReadDir(signalTmpRoot())
	if err != nil {
		t.Fatalf("tmp root should still exist: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("tmp root should be empty after run, has %d entries", len(entries))
	}
}

func TestIsNativeExecutableTellsBinariesFromScripts(t *testing.T) {
	dir := t.TempDir()
	for name, tc := range map[string]struct {
		content []byte
		want    bool
	}{
		"mach-o 64":      {[]byte{0xcf, 0xfa, 0xed, 0xfe, 0x0c, 0, 0, 1}, true},
		"mach-o 32":      {[]byte{0xce, 0xfa, 0xed, 0xfe, 7, 0, 0, 0}, true},
		"mach-o big end": {[]byte{0xfe, 0xed, 0xfa, 0xcf, 0, 0, 0, 0}, true},
		"universal":      {[]byte{0xca, 0xfe, 0xba, 0xbe, 0, 0, 0, 2}, true},
		"universal 64":   {[]byte{0xca, 0xfe, 0xba, 0xbf, 0, 0, 0, 2}, true},
		"elf":            {[]byte{0x7f, 'E', 'L', 'F', 2, 1, 1, 0}, true},
		"start script":   {[]byte("#!/usr/bin/env sh\nexec java \"$@\"\n"), false},
		"short":          {[]byte{0x7f, 'E'}, false},
		"empty":          {nil, false},
		"something else": {[]byte("PK\x03\x04zip"), false},
	} {
		path := filepath.Join(dir, strings.ReplaceAll(name, " ", "-"))
		if err := os.WriteFile(path, tc.content, 0o755); err != nil {
			t.Fatal(err)
		}
		if got := isNativeExecutable(path); got != tc.want {
			t.Errorf("isNativeExecutable(%s) = %v, want %v", name, got, tc.want)
		}
	}
	if isNativeExecutable(filepath.Join(dir, "missing")) || isNativeExecutable("signal-cli-not-on-path") {
		t.Error("a missing executable counted as native")
	}
}

// A native signal-cli takes java.io.tmpdir only as a leading argument.
// /bin/echo is a native binary that prints the arguments it was given.
func TestRunSignalCLIPassesTheRunDirToANativeBinaryAsAnArgument(t *testing.T) {
	echo, err := exec.LookPath("echo")
	if err != nil || !isNativeExecutable(echo) {
		t.Skipf("no native echo binary (%q, %v)", echo, err)
	}
	stubTmpDirArgProbe(t, true)
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("OPENMESSAGES_SIGNAL_CLI", echo)
	configDir := t.TempDir()
	out, err := runSignalCLI(context.Background(), configDir, "receive")
	if err != nil {
		t.Fatalf("runSignalCLI: %v (out=%s)", err, out)
	}
	args := strings.Fields(string(out))
	if len(args) != 4 || !strings.HasPrefix(args[0], "-Djava.io.tmpdir="+signalTmpRoot()) ||
		args[1] != "--config" || args[2] != configDir || args[3] != "receive" {
		t.Fatalf("native signal-cli argv = %q, want -Djava.io.tmpdir=<run dir> --config %s receive", args, configDir)
	}
}

func stubTmpDirArgProbe(t *testing.T, supported bool) *int {
	t.Helper()
	original := probeTmpDirArg
	t.Cleanup(func() { probeTmpDirArg = original })
	tmpDirArgSupport.Clear()
	t.Cleanup(tmpDirArgSupport.Clear)
	probes := new(int)
	probeTmpDirArg = func(string) bool {
		*probes++
		return supported
	}
	return probes
}

// A native binary whose --version run rejects the argument (a compiled
// wrapper around a JVM install, say) never gets it; the check runs once per
// build of the file.
func TestSignalCLITmpDirArgsNeedsAProbeTheBinaryPassed(t *testing.T) {
	echo, err := exec.LookPath("echo")
	if err != nil || !isNativeExecutable(echo) {
		t.Skipf("no native echo binary (%q, %v)", echo, err)
	}
	probes := stubTmpDirArgProbe(t, false)
	for range 3 {
		if args := signalCLITmpDirArgs(echo, "/tmp/run-1"); args != nil {
			t.Fatalf("args = %q for a binary that failed the probe", args)
		}
	}
	if *probes != 1 {
		t.Fatalf("probed %d times, want once per build", *probes)
	}
	script := filepath.Join(t.TempDir(), "signal-cli")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if args := signalCLITmpDirArgs(script, "/tmp/run-1"); args != nil || *probes != 1 {
		t.Fatalf("script got args %q after %d probes, want none and no probe", args, *probes)
	}
}

func TestSweepSignalAttachmentTempsRemovesOnlyStalePartialFiles(t *testing.T) {
	configDir := t.TempDir()
	dir := filepath.Join(configDir, "attachments")
	if err := os.MkdirAll(filepath.Join(dir, "old.tmp.d"), 0o700); err != nil {
		t.Fatal(err)
	}
	stale := time.Now().Add(-2 * signalRunTmpMaxAge)
	files := map[string]time.Time{
		"photo.jpg.123.tmp":   stale,      // a killed call's partial file
		"video.mp4.456.tmp":   time.Now(), // a download in progress
		"D49ZGyDHvzgVPhh.jpg": stale,      // a stored attachment
		"Q7pXk2LmNbVcZ.tmp":   stale,      // a stored attachment its sender named .tmp
	}
	for name, modified := range files {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, modified, modified); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chtimes(filepath.Join(dir, "old.tmp.d"), stale, stale); err != nil {
		t.Fatal(err)
	}

	sweepSignalAttachmentTemps(zerolog.Nop(), configDir, signalRunTmpMaxAge)

	for name, want := range map[string]bool{
		"photo.jpg.123.tmp":   false,
		"video.mp4.456.tmp":   true,
		"D49ZGyDHvzgVPhh.jpg": true,
		"Q7pXk2LmNbVcZ.tmp":   true,
		"old.tmp.d":           true,
	} {
		if _, err := os.Stat(filepath.Join(dir, name)); (err == nil) != want {
			t.Errorf("%s exists = %v after the sweep, want %v", name, err == nil, want)
		}
	}
	sweepSignalAttachmentTemps(zerolog.Nop(), filepath.Join(configDir, "missing"), signalRunTmpMaxAge)
}
