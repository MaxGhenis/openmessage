package signallive

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

const realisticSignalGetSenderPoison = `WARN  IncomingMessageHandler - Failed to handle incoming message
java.lang.NullPointerException: Cannot invoke "org.asamk.signal.manager.storage.recipients.RecipientId.getSender()" because "content" is null
	at org.asamk.signal.manager.helper.IncomingMessageHandler.getSender(IncomingMessageHandler.java:412)`

// syncBuffer protects test log access because the bridge's tmp-sweep goroutine,
// started by maybeSweepTmp, can write while the test reads. zerolog.SyncWriter
// only serializes writers, so it would not protect the test's String read.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestSignalCLIVersionProbeUsesExactCommandAndPrivateTemp(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	stub := filepath.Join(t.TempDir(), "signal-cli-stub")
	script := "#!/bin/sh\nprintf '%s|%s|%s' \"$1\" \"$TMPDIR\" \"$SIGNAL_CLI_OPTS\"\nmkdir -p \"$TMPDIR/libsignal-test\"\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENMESSAGES_SIGNAL_CLI", stub)

	output, err := probeSignalCLIVersion(context.Background())
	if err != nil {
		t.Fatalf("probeSignalCLIVersion(): %v (output=%s)", err, output)
	}
	parts := strings.SplitN(string(output), "|", 3)
	if len(parts) != 3 {
		t.Fatalf("unexpected version probe output: %q", output)
	}
	if parts[0] != "--version" {
		t.Fatalf("signal-cli arguments = %q, want exact --version", parts[0])
	}
	tmpDir, opts := parts[1], parts[2]
	if !strings.HasPrefix(tmpDir, signalTmpRoot()) {
		t.Fatalf("version probe TMPDIR %q not confined under %q", tmpDir, signalTmpRoot())
	}
	if !strings.Contains(opts, "-Djava.io.tmpdir="+tmpDir) {
		t.Fatalf("version probe SIGNAL_CLI_OPTS %q missing java.io.tmpdir", opts)
	}
	if _, err := os.Stat(tmpDir); !os.IsNotExist(err) {
		t.Fatalf("version probe temp dir %q should be removed, stat err = %v", tmpDir, err)
	}
}

func TestParseSignalCLIVersionBoundaryAndMalformedOutput(t *testing.T) {
	tests := []struct {
		name       string
		output     string
		want       signalCLIVersion
		wantParsed bool
		wantBelow  bool
	}{
		{
			name:       "below minimum",
			output:     "signal-cli 0.14.4\n",
			want:       signalCLIVersion{major: 0, minor: 14, patch: 4},
			wantParsed: true,
			wantBelow:  true,
		},
		{
			name:       "minimum",
			output:     "signal-cli 0.14.5\n",
			want:       minimumSignalCLIVersion,
			wantParsed: true,
		},
		{
			name:       "newer minor",
			output:     "signal-cli 0.15.0\n",
			want:       signalCLIVersion{major: 0, minor: 15, patch: 0},
			wantParsed: true,
		},
		{
			name:       "version after diagnostic line",
			output:     "WARN  startup diagnostic\nsignal-cli 1.0.0\n",
			want:       signalCLIVersion{major: 1, minor: 0, patch: 0},
			wantParsed: true,
		},
		{name: "missing patch", output: "signal-cli 0.14\n"},
		{name: "prefixed version", output: "signal-cli v0.14.5\n"},
		{name: "non-numeric version", output: "signal-cli latest\n"},
		{name: "unrelated output", output: "command not found\n"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, parsed := parseSignalCLIVersion([]byte(tc.output))
			if parsed != tc.wantParsed {
				t.Fatalf("parseSignalCLIVersion(%q) parsed = %v, want %v (version=%s)", tc.output, parsed, tc.wantParsed, got)
			}
			if !parsed {
				return
			}
			if got != tc.want {
				t.Fatalf("parseSignalCLIVersion(%q) = %s, want %s", tc.output, got, tc.want)
			}
			if below := got.less(minimumSignalCLIVersion); below != tc.wantBelow {
				t.Fatalf("%s less than minimum %s = %v, want %v", got, minimumSignalCLIVersion, below, tc.wantBelow)
			}
		})
	}
}

func TestSignalCLIVersionGateParksBelowMinimumBeforeAccountOrReceive(t *testing.T) {
	bridge := &Bridge{
		account:   "+15551230000",
		configDir: t.TempDir(),
		logger:    zerolog.Nop(),
	}

	var versionCalls atomic.Int32
	var accountCalls atomic.Int32
	var receiveCalls atomic.Int32
	installSignalGateStubs(t, bridge,
		func(context.Context) ([]byte, error) {
			versionCalls.Add(1)
			return []byte("signal-cli 0.14.4\n"), nil
		},
		func(_ context.Context, _ string, args ...string) ([]byte, error) {
			switch {
			case hasSignalCLIArg(args, "listAccounts"):
				accountCalls.Add(1)
				return []byte(`[{"number":"+15551230000"}]`), nil
			case hasSignalCLIArg(args, "receive"):
				receiveCalls.Add(1)
			}
			return nil, nil
		},
	)

	if err := bridge.ConnectIfPaired(); err != nil {
		t.Fatalf("ConnectIfPaired(): %v", err)
	}
	waitForCondition(t, 2*time.Second, func() bool {
		status := bridge.Status()
		return status.UpgradeRequired && !status.Connected && !status.Connecting
	})

	status := bridge.Status()
	if !strings.Contains(status.LastError, "0.14.4") ||
		!strings.Contains(status.LastError, minimumSignalCLIVersion.String()) {
		t.Fatalf("last_error = %q, want detected and minimum versions", status.LastError)
	}
	encodedStatus, err := json.Marshal(status)
	if err != nil {
		t.Fatalf("json.Marshal(status): %v", err)
	}
	if !bytes.Contains(encodedStatus, []byte(`"upgrade_required":true`)) {
		t.Fatalf("status JSON = %s, want upgrade_required=true", encodedStatus)
	}
	if got := versionCalls.Load(); got != 1 {
		t.Fatalf("version probe calls = %d, want 1", got)
	}
	if got := accountCalls.Load(); got != 0 {
		t.Fatalf("listAccounts calls = %d, want 0", got)
	}
	if got := receiveCalls.Load(); got != 0 {
		t.Fatalf("receive calls = %d, want 0", got)
	}
}

func TestSignalCLIVersionGateAllowsMinimumVersionToReceive(t *testing.T) {
	bridge := &Bridge{
		account:   "+15551230000",
		configDir: t.TempDir(),
		logger:    zerolog.Nop(),
	}

	var receiveCalls atomic.Int32
	installSignalGateStubs(t, bridge,
		func(context.Context) ([]byte, error) {
			return []byte("signal-cli 0.14.5\n"), nil
		},
		func(ctx context.Context, _ string, args ...string) ([]byte, error) {
			if hasSignalCLIArg(args, "listAccounts") {
				return []byte(`[{"number":"+15551230000"}]`), nil
			}
			if hasSignalCLIArg(args, "receive") {
				receiveCalls.Add(1)
				<-ctx.Done()
				return nil, ctx.Err()
			}
			return []byte("[]"), nil
		},
	)

	if err := bridge.ConnectIfPaired(); err != nil {
		t.Fatalf("ConnectIfPaired(): %v", err)
	}
	waitForCondition(t, 2*time.Second, func() bool {
		return receiveCalls.Load() > 0
	})

	status := bridge.Status()
	if !status.Connected || status.Connecting || status.UpgradeRequired {
		t.Fatalf("0.14.5 status = %+v, want connected without upgrade requirement", status)
	}
}

func TestSignalCLIVersionGateFailsOpenWithWarning(t *testing.T) {
	tests := []struct {
		name   string
		output []byte
		err    error
	}{
		{
			name: "probe error",
			err:  errors.New("signal-cli version probe failed"),
		},
		{
			name:   "malformed output",
			output: []byte("signal-cli version unknown\n"),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var log syncBuffer
			bridge := &Bridge{
				account:   "+15551230000",
				configDir: t.TempDir(),
				logger:    zerolog.New(&log),
			}

			var receiveCalls atomic.Int32
			installSignalGateStubs(t, bridge,
				func(context.Context) ([]byte, error) {
					return tc.output, tc.err
				},
				func(ctx context.Context, _ string, args ...string) ([]byte, error) {
					if hasSignalCLIArg(args, "listAccounts") {
						return []byte(`[{"number":"+15551230000"}]`), nil
					}
					if hasSignalCLIArg(args, "receive") {
						receiveCalls.Add(1)
						<-ctx.Done()
						return nil, ctx.Err()
					}
					return []byte("[]"), nil
				},
			)

			if err := bridge.ConnectIfPaired(); err != nil {
				t.Fatalf("ConnectIfPaired(): %v", err)
			}
			waitForCondition(t, 2*time.Second, func() bool {
				return receiveCalls.Load() > 0
			})

			status := bridge.Status()
			if !status.Connected || status.UpgradeRequired {
				t.Fatalf("undetectable version status = %+v, want fail-open connection", status)
			}
			if !strings.Contains(log.String(), "Unable to detect signal-cli version") ||
				!strings.Contains(log.String(), "continuing receive without version gate") {
				t.Fatalf("warning log missing fail-open explanation: %q", log.String())
			}
		})
	}
}

func TestSignalReceivePoisonFingerprint(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		output string
		want   string
	}{
		{
			name:   "real signal-cli crash",
			err:    errors.New("exit status 1"),
			output: realisticSignalGetSenderPoison,
			want:   signalGetSenderPoisonFingerprint,
		},
		{
			name:   "fingerprint split across error and output",
			err:    errors.New("getSender() failed"),
			output: `java.lang.NullPointerException: "content" is null`,
			want:   signalGetSenderPoisonFingerprint,
		},
		{
			name:   "getSender without null content",
			err:    errors.New("exit status 1"),
			output: "at IncomingMessageHandler.getSender(IncomingMessageHandler.java:412)",
		},
		{
			name:   "null content without getSender",
			err:    errors.New("exit status 1"),
			output: `NullPointerException: "content" is null`,
		},
		{
			name:   "unrelated receive failure",
			err:    context.DeadlineExceeded,
			output: "Connection closed unexpectedly",
		},
		{name: "empty"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := signalReceivePoisonFingerprint(tc.err, []byte(tc.output)); got != tc.want {
				t.Fatalf("signalReceivePoisonFingerprint() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRepeatedSignalReceivePoisonParksAtThresholdAndCannotAutoRestart(t *testing.T) {
	bridge := &Bridge{
		account:   "+15551230000",
		configDir: t.TempDir(),
		logger:    zerolog.Nop(),
	}

	var versionCalls atomic.Int32
	var receiveCalls atomic.Int32
	installSignalGateStubs(t, bridge,
		func(context.Context) ([]byte, error) {
			versionCalls.Add(1)
			return []byte("signal-cli 0.14.5\n"), nil
		},
		func(_ context.Context, _ string, args ...string) ([]byte, error) {
			if hasSignalCLIArg(args, "listAccounts") {
				return []byte(`[{"number":"+15551230000"}]`), nil
			}
			if hasSignalCLIArg(args, "receive") {
				receiveCalls.Add(1)
				return []byte(realisticSignalGetSenderPoison), errors.New("exit status 1")
			}
			return []byte("[]"), nil
		},
	)

	if err := bridge.ConnectIfPaired(); err != nil {
		t.Fatalf("ConnectIfPaired(): %v", err)
	}
	waitForCondition(t, 3*time.Second, func() bool {
		return bridge.Status().UpgradeRequired
	})

	status := bridge.Status()
	if status.Connected || status.Connecting {
		t.Fatalf("poison status = %+v, want parked", status)
	}
	if !strings.Contains(status.LastError, "IncomingMessageHandler.getSender()") ||
		!strings.Contains(status.LastError, "content is null") ||
		!strings.Contains(status.LastError, minimumSignalCLIVersion.String()) {
		t.Fatalf("poison last_error = %q, want fingerprint and upgrade instruction", status.LastError)
	}
	if got := receiveCalls.Load(); got != int32(receivePoisonLimit) {
		t.Fatalf("receive calls at park = %d, want exact poison threshold %d", got, receivePoisonLimit)
	}

	versionBefore := versionCalls.Load()
	receiveBefore := receiveCalls.Load()
	if err := bridge.ConnectIfPaired(); err != nil {
		t.Fatalf("ConnectIfPaired() while parked: %v", err)
	}
	time.Sleep(150 * time.Millisecond)
	if got := versionCalls.Load(); got != versionBefore {
		t.Fatalf("version probe restarted while parked: calls = %d, want %d", got, versionBefore)
	}
	if got := receiveCalls.Load(); got != receiveBefore {
		t.Fatalf("receive restarted while parked: calls = %d, want %d", got, receiveBefore)
	}
	if status := bridge.Status(); !status.UpgradeRequired || status.Connecting || status.Connected {
		t.Fatalf("status changed after automatic reconnect attempt: %+v", status)
	}
}

func TestSignalReceivePoisonConsecutiveCountResetsAfterIdlePoll(t *testing.T) {
	bridge := &Bridge{
		account:   "+15551230000",
		configDir: t.TempDir(),
		logger:    zerolog.Nop(),
	}

	var receiveCalls atomic.Int32
	fourthCallStarted := make(chan struct{})
	releaseFourthCall := make(chan struct{})
	installSignalGateStubs(t, bridge,
		func(context.Context) ([]byte, error) {
			return []byte("signal-cli 0.14.5\n"), nil
		},
		func(ctx context.Context, _ string, args ...string) ([]byte, error) {
			if hasSignalCLIArg(args, "listAccounts") {
				return []byte(`[{"number":"+15551230000"}]`), nil
			}
			if !hasSignalCLIArg(args, "receive") {
				return []byte("[]"), nil
			}

			switch call := receiveCalls.Add(1); call {
			case 1, 3:
				return []byte(realisticSignalGetSenderPoison), errors.New("exit status 1")
			case 2:
				return nil, context.DeadlineExceeded
			case 4:
				close(fourthCallStarted)
				select {
				case <-releaseFourthCall:
					return []byte(realisticSignalGetSenderPoison), errors.New("exit status 1")
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			default:
				return nil, errors.New("unexpected receive after poison threshold")
			}
		},
	)

	if err := bridge.ConnectIfPaired(); err != nil {
		t.Fatalf("ConnectIfPaired(): %v", err)
	}
	select {
	case <-fourthCallStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("fourth receive did not start")
	}
	if status := bridge.Status(); status.UpgradeRequired {
		t.Fatalf("single poison after idle interruption parked early: %+v", status)
	}

	close(releaseFourthCall)
	waitForCondition(t, 2*time.Second, func() bool {
		return bridge.Status().UpgradeRequired
	})
	if got := receiveCalls.Load(); got != 4 {
		t.Fatalf("receive calls = %d, want 4 (poison, idle, poison, poison)", got)
	}
}

func TestStartPollerPublishesRealReadyActivityAndJoinedDone(t *testing.T) {
	bridge := &Bridge{
		account:   "+15551230000",
		configDir: t.TempDir(),
		logger:    zerolog.Nop(),
	}
	var receiveCalls atomic.Int32
	installSignalGateStubs(t, bridge,
		func(context.Context) ([]byte, error) {
			return []byte("signal-cli 0.14.5\n"), nil
		},
		func(ctx context.Context, _ string, args ...string) ([]byte, error) {
			switch {
			case hasSignalCLIArg(args, "listAccounts"):
				return []byte(`[{"number":"+15551230000"}]`), nil
			case hasSignalCLIArg(args, "receive"):
				if receiveCalls.Add(1) == 1 {
					return nil, context.DeadlineExceeded
				}
				<-ctx.Done()
				return nil, ctx.Err()
			default:
				return []byte("[]"), nil
			}
		},
	)

	ctx, cancel := context.WithCancel(context.Background())
	run, err := bridge.StartPoller(ctx)
	if err != nil {
		cancel()
		t.Fatalf("StartPoller(): %v", err)
	}
	select {
	case <-run.Ready():
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("poller did not become ready after account probe")
	}

	seenIdle := false
	deadline := time.After(2 * time.Second)
	for !seenIdle {
		select {
		case activity := <-run.Activity():
			seenIdle = activity.Detail == "receive_idle"
		case <-deadline:
			cancel()
			t.Fatal("poller did not publish completed receive activity")
		}
	}
	cancel()
	select {
	case exit := <-run.Done():
		if exit.Kind != "" || !errors.Is(exit.Err, context.Canceled) {
			t.Fatalf("controlled poller exit = %+v, want canceled without failure class", exit)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("poller Done did not resolve after receive loop exit")
	}
	stopCtx, stopCancel := context.WithTimeout(context.Background(), time.Second)
	defer stopCancel()
	if err := run.Stop(stopCtx); err != nil {
		t.Fatalf("PollerRun.Stop() after Done: %v", err)
	}
}

func TestStartPollerClassifiesAccountInvalidAtInitialProbe(t *testing.T) {
	bridge := &Bridge{
		account:   "+15551230000",
		configDir: t.TempDir(),
		logger:    zerolog.Nop(),
	}
	installSignalGateStubs(t, bridge,
		func(context.Context) ([]byte, error) {
			return []byte("signal-cli 0.14.5\n"), nil
		},
		func(_ context.Context, _ string, args ...string) ([]byte, error) {
			if hasSignalCLIArg(args, "listAccounts") {
				return []byte("User +15551230000 is not registered."), errors.New("exit status 1")
			}
			return nil, nil
		},
	)

	run, err := bridge.StartPoller(context.Background())
	if err != nil {
		t.Fatalf("StartPoller(): %v", err)
	}
	select {
	case exit := <-run.Done():
		if exit.Kind != PollerFailureReauth || exit.Fingerprint != SignalAccountInvalidFingerprint {
			t.Fatalf("initial account-invalid exit = %+v, want reauth/%s", exit, SignalAccountInvalidFingerprint)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("account-invalid poller did not exit")
	}
	if status := bridge.Status(); !status.NeedsReauth || status.Connected || status.Connecting {
		t.Fatalf("account-invalid status = %+v, want parked reauth", status)
	}
}

func TestStartPollerClassifiesExpiredLinkAsUnpaired(t *testing.T) {
	bridge := &Bridge{
		configDir: t.TempDir(),
		logger:    zerolog.Nop(),
	}
	originalStartLink := startSignalLink
	originalRunSignalCLI := runSignalCLI
	startSignalLink = func(context.Context, string) (io.ReadCloser, func() error, error) {
		return io.NopCloser(strings.NewReader("sgnl://linkdevice?uuid=expired\n")), func() error {
			return errors.New("Signal link QR expired")
		}, nil
	}
	runSignalCLI = func(context.Context, string, ...string) ([]byte, error) {
		return []byte("[]"), nil
	}
	t.Cleanup(func() {
		_ = bridge.Close()
		startSignalLink = originalStartLink
		bridge.commandMu.Lock()
		runSignalCLI = originalRunSignalCLI
		bridge.commandMu.Unlock()
	})

	run, err := bridge.StartPoller(context.Background())
	if err != nil {
		t.Fatalf("StartPoller(): %v", err)
	}
	select {
	case exit := <-run.Done():
		if exit.Kind != PollerFailureUnpaired ||
			exit.Operation != "pair" ||
			exit.Fingerprint != SignalPairingIncompleteFingerprint {
			t.Fatalf("expired pairing exit = %+v, want unpaired/%s", exit, SignalPairingIncompleteFingerprint)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expired pairing did not resolve poller Done")
	}
	stopCtx, stopCancel := context.WithTimeout(context.Background(), time.Second)
	defer stopCancel()
	if err := run.Stop(stopCtx); err != nil {
		t.Fatalf("PollerRun.Stop() after expired pairing: %v", err)
	}
	status := bridge.Status()
	if status.Paired || status.Pairing || status.Connecting || status.QRAvailable {
		t.Fatalf("expired pairing status = %+v, want idle unpaired", status)
	}
}

func installSignalGateStubs(
	t *testing.T,
	bridge *Bridge,
	versionProbe func(context.Context) ([]byte, error),
	cli func(context.Context, string, ...string) ([]byte, error),
) {
	t.Helper()
	originalVersionProbe := probeSignalCLIVersion
	originalRunSignalCLI := runSignalCLI
	probeSignalCLIVersion = versionProbe
	runSignalCLI = cli
	t.Cleanup(func() {
		_ = bridge.Close()
		probeSignalCLIVersion = originalVersionProbe
		bridge.commandMu.Lock()
		runSignalCLI = originalRunSignalCLI
		bridge.commandMu.Unlock()
	})
}

func hasSignalCLIArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}
