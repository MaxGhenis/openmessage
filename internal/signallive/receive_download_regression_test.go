package signallive

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/maxghenis/openmessage/internal/db"
)

// slowDownloadSignalCLI stands in for signal-cli. Its first receive call
// behaves like signal-cli handling an envelope with a large attachment: it
// takes SIGTERM without exiting (signal-cli's Shutdown handler lets the
// envelope finish), streams "ciphertext" into signal-cli_tmp_1.tmp in its
// temp dir for STUB_STEPS tenths of a second, then prints the envelope and
// exits 0. Later receive calls find nothing.
const slowDownloadSignalCLI = `#!/bin/sh
case " $* " in
*" --version "*) echo "signal-cli 0.14.9"; exit 0 ;;
*" listAccounts "*) printf '[{"number":"%s"}]\n' "$STUB_ACCOUNT"; exit 0 ;;
*" receive "*) ;;
*) exit 0 ;;
esac
trap 'echo "INFO  Shutdown - Received SIGTERM signal, shutting down ..." >&2' TERM
if mkdir "$STUB_STATE/first-receive" 2>/dev/null; then
	f="$TMPDIR/signal-cli_tmp_1.tmp"
	: > "$f"
	i=0
	while [ "$i" -lt "$STUB_STEPS" ]; do
		printf 'ciphertext' >> "$f"
		sleep 0.1
		i=$((i+1))
	done
	rm -f "$f"
	printf '%s\n' "$STUB_ENVELOPE"
	exit 0
fi
sleep 0.05
`

// A receive call that runs into its deadline while signal-cli downloads an
// attachment must not be killed before signal-cli prints the envelope.
// signal-cli has already acknowledged it, and a retry cannot decrypt it
// again, so a kill loses the message. The download here outlasts the
// 3 s SIGTERM-to-SIGKILL grace every signal-cli call used to get; this
// test fails on a build that kills the call then.
func TestStartReceiveLoopKeepsAMessageWhoseAttachmentDownloadOutlastsTheStopGrace(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a stub signal-cli for about five seconds")
	}
	const sender = "+15551234567"
	const body = "the photo you asked for"
	envelope := failedReceiveEnvelope(sender, 1700000000123, body)

	t.Setenv("TMPDIR", t.TempDir())
	stub := filepath.Join(t.TempDir(), "signal-cli-stub")
	if err := os.WriteFile(stub, []byte(slowDownloadSignalCLI), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENMESSAGES_SIGNAL_CLI", stub)
	t.Setenv("STUB_ACCOUNT", failedReceiveAccount)
	t.Setenv("STUB_STATE", t.TempDir())
	t.Setenv("STUB_STEPS", "40")
	t.Setenv("STUB_ENVELOPE", envelope)

	store, err := db.New(filepath.Join(t.TempDir(), "messages.db"))
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	bridge := &Bridge{store: store, logger: zerolog.Nop(), configDir: t.TempDir()}
	originalDeadline := receiveCallDeadline
	t.Cleanup(func() {
		_ = bridge.Close()
		bridge.commandMu.Lock()
		receiveCallDeadline = originalDeadline
		bridge.commandMu.Unlock()
		store.Close()
	})
	receiveCallDeadline = 300 * time.Millisecond

	bridge.goTracked(func() { bridge.startReceiveLoop(failedReceiveAccount, false) })
	waitForCondition(t, 20*time.Second, func() bool {
		return len(signalBodies(t, store, sender)) > 0
	})
	if got := signalBodies(t, store, sender); !slices.Equal(got, []string{body}) {
		t.Fatalf("stored bodies = %q, want the envelope printed after its download", got)
	}
}
