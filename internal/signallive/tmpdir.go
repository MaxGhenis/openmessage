package signallive

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
)

// signal-cli runs on the JVM; libsignal-client extracts ~21MB of native
// libraries into a fresh java.io.tmpdir subdirectory (libsignal*) on every
// invocation and does not reliably remove it on exit. Because the bridge
// invokes signal-cli repeatedly (receive polling, sends, metadata refresh),
// leaked directories can fill the disk (issue #27). Three defenses:
//
//  1. Every invocation runs with TMPDIR/java.io.tmpdir pointed at a private
//     per-run directory under signalTmpRoot(), removed as soon as the
//     process exits — no accumulation by construction. The native build
//     takes java.io.tmpdir only as an argument (signalCLITmpDirArgs).
//  2. signalTmpRoot() is swept at startup and periodically for entries old
//     enough that no live invocation can still own them (crash backstop).
//  3. A one-time sweep of the system temp dir removes libsignal* dirs
//     abandoned by earlier versions, so existing installs recover their
//     disk space without manual cleanup.

const (
	// signalRunTmpMaxAge must exceed the longest legitimate signal-cli run.
	// Receive polls finish in seconds and sends within sendTimeout, but a
	// receive call can be drained for receiveDrain.limit and a `link`
	// session can stay open while the user fetches their phone, so keep a
	// generous margin. It also gates the attachments/*.tmp sweep.
	signalRunTmpMaxAge = 30 * time.Minute

	signalTmpSweepInterval = 10 * time.Minute

	// legacyLibsignalMaxAge gates the one-time system temp dir sweep. A day
	// is far older than any live signal-cli process while young enough to
	// recover a runaway leak quickly.
	legacyLibsignalMaxAge = 24 * time.Hour

	signalTmpSweepEnvVar = "OPENMESSAGES_SIGNAL_TMP_SWEEP"
)

// signalTmpRoot lives under the system temp dir rather than the Signal
// config dir: Unpair removes the config dir wholesale (and tests remove
// their temp config dirs on cleanup), which would race with a lingering
// invocation still creating its run dir inside. The uid suffix keeps the
// root private per user on systems with a shared /tmp.
func signalTmpRoot() string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("openmessage-signal-cli-%d", os.Getuid()))
}

// newSignalRunTmpDir creates a private temp dir for one signal-cli
// invocation. The returned cleanup removes it; callers must invoke cleanup
// only after the subprocess has exited.
func newSignalRunTmpDir() (string, func(), error) {
	root := signalTmpRoot()
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", nil, err
	}
	dir, err := os.MkdirTemp(root, "run-")
	if err != nil {
		return "", nil, err
	}
	return dir, func() { _ = os.RemoveAll(dir) }, nil
}

// signalCLIEnv rebases the subprocess's temp space onto dir. TMPDIR covers
// signal-cli itself plus anything it spawns; java.io.tmpdir (appended last
// to SIGNAL_CLI_OPTS so it wins) covers JVMs that derive their temp dir from
// the platform default instead of TMPDIR.
func signalCLIEnv(base []string, dir string) []string {
	javaOpt := "-Djava.io.tmpdir=" + dir
	opts := javaOpt
	env := make([]string, 0, len(base)+2)
	for _, kv := range base {
		switch {
		case strings.HasPrefix(kv, "TMPDIR="):
			continue
		case strings.HasPrefix(kv, "SIGNAL_CLI_OPTS="):
			if existing := strings.TrimSpace(strings.TrimPrefix(kv, "SIGNAL_CLI_OPTS=")); existing != "" {
				opts = existing + " " + javaOpt
			}
			continue
		}
		env = append(env, kv)
	}
	return append(env, "TMPDIR="+dir, "SIGNAL_CLI_OPTS="+opts)
}

// signalCLITmpDirArgs points java.io.tmpdir at dir for a native signal-cli.
// Homebrew ships signal-cli as a GraalVM native image, which reads neither
// SIGNAL_CLI_OPTS (a variable of the JVM start script) nor TMPDIR: on macOS
// it takes java.io.tmpdir from the per-user Darwin temp dir. It does honor a
// leading -Djava.io.tmpdir argument (0.14.9, checked 2026-10-21: libsignal
// was extracted into the given dir and removed on a graceful exit). Only a
// native executable gets it, and only once a --version run with the argument
// has succeeded: that is the build it was checked against, a JVM install
// takes java.io.tmpdir from SIGNAL_CLI_OPTS (signalCLIEnv), and a compiled
// wrapper around a JVM install would pass the argument to signal-cli's own
// parser.
func signalCLITmpDirArgs(executable, dir string) []string {
	if dir == "" || !acceptsTmpDirArg(executable) {
		return nil
	}
	return []string{"-Djava.io.tmpdir=" + dir}
}

// tmpDirArgSupport caches acceptsTmpDirArg per executable path, size and
// modification time, so an upgraded signal-cli is checked again.
var tmpDirArgSupport sync.Map

func acceptsTmpDirArg(executable string) bool {
	info, err := os.Stat(executable)
	if err != nil {
		return false
	}
	key := fmt.Sprintf("%s|%d|%d", executable, info.Size(), info.ModTime().UnixNano())
	if supported, ok := tmpDirArgSupport.Load(key); ok {
		return supported.(bool)
	}
	supported := isNativeExecutable(executable) && probeTmpDirArg(executable)
	tmpDirArgSupport.Store(key, supported)
	return supported
}

// probeTmpDirArg runs `<executable> -Djava.io.tmpdir=<dir> --version`, which
// reads no config dir. Swapped by tests.
var probeTmpDirArg = func(executable string) bool {
	if err := os.MkdirAll(signalTmpRoot(), 0o700); err != nil {
		return false
	}
	dir, err := os.MkdirTemp(signalTmpRoot(), "probe-")
	if err != nil {
		return false
	}
	defer os.RemoveAll(dir)
	ctx, cancel := context.WithTimeout(context.Background(), versionProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, "-Djava.io.tmpdir="+dir, "--version")
	cmd.Env = signalCLIEnv(os.Environ(), dir)
	configureSignalCancel(cmd)
	output, err := cmd.CombinedOutput()
	return err == nil && strings.HasPrefix(strings.TrimSpace(string(output)), "signal-cli ")
}

// isNativeExecutable reports whether path is a Mach-O or ELF binary rather
// than a script.
func isNativeExecutable(path string) bool {
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer file.Close()
	var magic [4]byte
	if _, err := io.ReadFull(file, magic[:]); err != nil {
		return false
	}
	switch binary.BigEndian.Uint32(magic[:]) {
	case 0x7f454c46, // ELF
		0xfeedface, 0xcefaedfe, 0xfeedfacf, 0xcffaedfe, // Mach-O, 32 and 64 bit, either byte order
		0xcafebabe, 0xbebafeca, 0xcafebabf, 0xbfbafeca: // universal Mach-O, 32 and 64 bit
		return true
	}
	return false
}

func signalTmpSweepDisabled() bool {
	return strings.TrimSpace(os.Getenv(signalTmpSweepEnvVar)) == "0"
}

// sweepSignalTmpRoot removes entries under the app-owned tmp root older
// than maxAge. The bridge owns this directory outright, so every stale
// entry is a leftover from a crashed run regardless of its name.
func sweepSignalTmpRoot(logger zerolog.Logger, maxAge time.Duration) {
	if signalTmpSweepDisabled() {
		return
	}
	removed, bytes := sweepDirEntries(signalTmpRoot(), maxAge, func(string) bool { return true })
	if removed > 0 {
		logger.Info().
			Int("dirs", removed).
			Str("reclaimed", humanBytes(bytes)).
			Msg("Swept stale signal-cli temp dirs left by interrupted runs")
	}
}

// sweepLegacyLibsignalTemp clears libsignal* dirs that earlier OpenMessage
// versions leaked into the system temp dir (issue #27). Only dirs beyond
// legacyLibsignalMaxAge are touched: anything that old cannot belong to a
// live signal-cli run, and libsignal re-extracts on demand if another app
// somehow still references one.
func sweepLegacyLibsignalTemp(logger zerolog.Logger) {
	if signalTmpSweepDisabled() {
		return
	}
	removed, bytes := sweepDirEntries(os.TempDir(), legacyLibsignalMaxAge, func(name string) bool {
		return strings.HasPrefix(name, "libsignal")
	})
	if removed > 0 {
		logger.Warn().
			Int("dirs", removed).
			Str("reclaimed", humanBytes(bytes)).
			Msg("Removed libsignal temp dirs leaked by earlier versions (see issue #27)")
	}
}

// sweepSignalAttachmentTemps removes the <name>.<digits>.tmp files a killed
// receive call leaves in signal-cli's attachments dir. signal-cli creates
// one when an attachment download starts and deletes it in a finally block
// that a SIGKILL skips; nothing removes it after that. maxAge must exceed the
// longest receive call, which runs with the bridge's command gate held while
// this sweep does not.
func sweepSignalAttachmentTemps(logger zerolog.Logger, configDir string, maxAge time.Duration) {
	if signalTmpSweepDisabled() || configDir == "" {
		return
	}
	dir := filepath.Join(configDir, "attachments")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := now().Add(-maxAge)
	removed := 0
	var reclaimed int64
	for _, entry := range entries {
		if !entry.Type().IsRegular() || !isSignalAttachmentTemp(entry.Name()) {
			continue
		}
		info, err := entry.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil {
			continue
		}
		removed++
		reclaimed += info.Size()
	}
	if removed > 0 {
		logger.Info().
			Int("files", removed).
			Str("reclaimed", humanBytes(reclaimed)).
			Msg("Swept partial Signal attachment files left by killed receive calls")
	}
}

func sweepDirEntries(root string, maxAge time.Duration, match func(name string) bool) (int, int64) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return 0, 0
	}
	cutoff := now().Add(-maxAge)
	removed := 0
	var reclaimed int64
	for _, entry := range entries {
		if !entry.IsDir() || !match(entry.Name()) {
			continue
		}
		info, err := entry.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		path := filepath.Join(root, entry.Name())
		size := dirSize(path)
		if err := os.RemoveAll(path); err != nil {
			continue
		}
		removed++
		reclaimed += size
	}
	return removed, reclaimed
}

func dirSize(root string) int64 {
	var total int64
	_ = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return strconv.FormatInt(n, 10) + " B"
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return strconv.FormatFloat(float64(n)/float64(div), 'f', 1, 64) + " " + string("KMGTPE"[exp]) + "iB"
}
