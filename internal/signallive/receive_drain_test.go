package signallive

import (
	"context"
	"errors"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"testing/quick"
	"time"

	"github.com/rs/zerolog"
)

// --- drainClock -------------------------------------------------------------

var testDrainStart = time.Unix(1700000000, 0)

// drainLook is one look a drain takes at a stopped call.
type drainLook struct {
	After     time.Duration // since the stop
	Downloads signalDownloads
	Aborted   bool
}

// drainHistory is a generated run of looks at a stopped call: times only
// move forward, downloads come from a few shapes (none, a growing file, a
// file that stopped growing), and the caller may stop part-way.
type drainHistory struct {
	Policy receiveDrainPolicy
	Looks  []drainLook
}

func (drainHistory) Generate(r *rand.Rand, _ int) reflect.Value {
	ms := func(lo, hi int) time.Duration { return time.Duration(lo+r.Intn(hi-lo+1)) * time.Millisecond }
	policy := receiveDrainPolicy{idle: ms(1, 400)}
	policy.stall = policy.idle + ms(0, 600)
	policy.limit = policy.stall + ms(0, 2000)
	policy.tick = ms(1, 50)

	abortAt := -1
	if r.Intn(3) == 0 {
		abortAt = r.Intn(40)
	}
	var looks []drainLook
	var after time.Duration
	downloads := signalDownloads{}
	for i := 0; i < 1+r.Intn(60); i++ {
		switch r.Intn(5) {
		case 0:
			downloads = signalDownloads{}
		case 1, 2:
			downloads.files = max(downloads.files, 1)
			downloads.bytes += int64(1 + r.Intn(4096))
			downloads.modified = testDrainStart.Add(after).UnixNano()
		default:
			// unchanged: a stall, or still nothing in flight
		}
		looks = append(looks, drainLook{After: after, Downloads: downloads, Aborted: abortAt >= 0 && i >= abortAt})
		after += ms(0, 120)
	}
	return reflect.ValueOf(drainHistory{Policy: policy, Looks: looks})
}

// referenceDrainKill decides the kill at look k from the whole history, the
// way the drain is specified, for a differential check of the incremental
// clock.
func referenceDrainKill(policy receiveDrainPolicy, looks []drainLook, k int) bool {
	at := looks[k].After
	if at >= policy.limit {
		return true
	}
	for i := 0; i <= k; i++ {
		if looks[i].Aborted {
			return at-looks[i].After >= policy.idle
		}
	}
	if looks[k].Downloads.files > 0 {
		var lastProgress time.Duration
		seenDownload := false
		for i := 0; i <= k; i++ {
			d := looks[i].Downloads
			if d.files == 0 {
				continue
			}
			if !seenDownload || (i > 0 && d != looks[i-1].Downloads) {
				lastProgress = looks[i].After
			}
			seenDownload = true
		}
		return at-lastProgress >= policy.stall
	}
	var lastInFlight time.Duration
	for i := 0; i <= k; i++ {
		if looks[i].Downloads.files > 0 {
			lastInFlight = looks[i].After
		}
	}
	return at-lastInFlight >= policy.idle
}

func TestDrainClockMatchesItsSpecificationProperty(t *testing.T) {
	property := func(h drainHistory) bool {
		clock := drainClock{policy: h.Policy, stoppedAt: testDrainStart}
		for k, look := range h.Looks {
			at := testDrainStart.Add(look.After)
			verdict := clock.observe(at, look.Downloads, look.Aborted)
			if verdict.kill != referenceDrainKill(h.Policy, h.Looks, k) {
				t.Logf("look %d of %+v: kill = %v, reference disagrees", k, h, verdict.kill)
				return false
			}
			// Never earlier than the grace every call had before drains.
			if verdict.kill && look.After < h.Policy.idle {
				t.Logf("look %d: killed %s after the stop, before the %s idle grace", k, look.After, h.Policy.idle)
				return false
			}
			// Bounded: nothing outlives the limit.
			if look.After >= h.Policy.limit && !verdict.kill {
				t.Logf("look %d: alive %s after the stop, past the %s limit", k, look.After, h.Policy.limit)
				return false
			}
			// Holding means alive past the idle grace for a download only.
			if verdict.holding && (verdict.kill || look.Downloads.files == 0 || look.Aborted || look.After < h.Policy.idle) {
				t.Logf("look %d: holding with %+v", k, look)
				return false
			}
			if verdict.kill {
				return true
			}
		}
		return true
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 4000}); err != nil {
		t.Fatal(err)
	}
}

// Without downloads or a caller stop, the drain is the fixed grace every
// call had: killed exactly once idle has passed since the stop.
func TestDrainClockWithoutDownloadsIsTheFixedGraceProperty(t *testing.T) {
	property := func(h drainHistory) bool {
		clock := drainClock{policy: h.Policy, stoppedAt: testDrainStart}
		for _, look := range h.Looks {
			verdict := clock.observe(testDrainStart.Add(look.After), signalDownloads{}, false)
			if verdict.kill != (look.After >= h.Policy.idle) || verdict.holding {
				return false
			}
		}
		return true
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 2000}); err != nil {
		t.Fatal(err)
	}
}

// A download that keeps growing holds the call until the limit; a caller
// stop collapses that to the idle grace from the stop.
func TestDrainClockHoldsAGrowingDownloadUntilTheLimitOrACallerStop(t *testing.T) {
	policy := receiveDrainPolicy{idle: 3 * time.Second, stall: 45 * time.Second, limit: 10 * time.Minute}
	grow := func(step int) signalDownloads {
		return signalDownloads{files: 1, bytes: int64(step) * 32 << 10, modified: int64(step)}
	}

	clock := drainClock{policy: policy, stoppedAt: testDrainStart}
	step := 0
	for after := time.Duration(0); after < policy.limit; after += 250 * time.Millisecond {
		step++
		verdict := clock.observe(testDrainStart.Add(after), grow(step), false)
		if verdict.kill {
			t.Fatalf("killed a growing download %s after the stop, before the %s limit", after, policy.limit)
		}
		if verdict.holding != (after >= policy.idle) {
			t.Fatalf("holding = %v %s after the stop", verdict.holding, after)
		}
	}
	if !clock.observe(testDrainStart.Add(policy.limit), grow(step+1), false).kill {
		t.Fatal("a growing download outlived the limit")
	}

	clock = drainClock{policy: policy, stoppedAt: testDrainStart}
	abortAt := 90 * time.Second
	for after := time.Duration(0); ; after += 250 * time.Millisecond {
		step++
		verdict := clock.observe(testDrainStart.Add(after), grow(step), after >= abortAt)
		if !verdict.kill {
			continue
		}
		if after != abortAt+policy.idle {
			t.Fatalf("killed %s after the stop, want %s: the idle grace after the caller stopped", after, abortAt+policy.idle)
		}
		break
	}
}

// A download that stops growing is killed stall after its last growth, and
// one that finishes leaves the call the idle grace to print and exit.
func TestDrainClockKillsAStalledDownloadAndGivesAFinishedOneTheIdleGrace(t *testing.T) {
	policy := receiveDrainPolicy{idle: 3 * time.Second, stall: 45 * time.Second, limit: 10 * time.Minute}
	stuck := signalDownloads{files: 1, bytes: 4096, modified: 7}

	clock := drainClock{policy: policy, stoppedAt: testDrainStart}
	clock.observe(testDrainStart.Add(10*time.Second), stuck, false)
	if v := clock.observe(testDrainStart.Add(10*time.Second+policy.stall-time.Millisecond), stuck, false); v.kill || !v.holding {
		t.Fatalf("verdict just inside the stall window = %+v, want held", v)
	}
	if v := clock.observe(testDrainStart.Add(10*time.Second+policy.stall), stuck, false); !v.kill {
		t.Fatalf("verdict at the stall window = %+v, want killed", v)
	}

	clock = drainClock{policy: policy, stoppedAt: testDrainStart}
	clock.observe(testDrainStart.Add(20*time.Second), stuck, false)
	if v := clock.observe(testDrainStart.Add(20*time.Second+policy.idle-time.Millisecond), signalDownloads{}, false); v.kill || v.holding {
		t.Fatalf("verdict after the download finished = %+v, want the idle grace, not held", v)
	}
	if v := clock.observe(testDrainStart.Add(20*time.Second+policy.idle), signalDownloads{}, false); !v.kill {
		t.Fatalf("verdict an idle grace after the download finished = %+v, want killed", v)
	}
}

// --- downloadWatch ----------------------------------------------------------

func TestDownloadWatchCountsOnlyTheCallsOwnDownloadFiles(t *testing.T) {
	runDir := t.TempDir()
	configDir := t.TempDir()
	attachments := filepath.Join(configDir, "attachments")
	if err := os.MkdirAll(filepath.Join(runDir, "libsignal123"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(attachments, 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(path string, size int, modified time.Time) {
		t.Helper()
		if err := os.WriteFile(path, make([]byte, size), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, modified, modified); err != nil {
			t.Fatal(err)
		}
	}
	since := time.Now().Truncate(time.Second)
	fresh := since.Add(time.Second)
	write(filepath.Join(runDir, "signal-cli_tmp_1.tmp"), 300, since.Add(-time.Hour)) // ciphertext: age doesn't matter
	write(filepath.Join(runDir, "other.tmp"), 7, fresh)
	write(filepath.Join(runDir, "libsignal123", "signal-cli_tmp_2.tmp"), 9, fresh)
	write(filepath.Join(attachments, "abc.jpg.123.tmp"), 0, fresh)                    // this call's store
	write(filepath.Join(attachments, "old.jpg.456.tmp"), 50, since.Add(-time.Minute)) // a killed call's leftover
	write(filepath.Join(attachments, "done.jpg"), 80, fresh)

	got := downloadWatch{runDir: runDir, attachmentsDir: attachments, since: since}.look()
	if got.files != 2 || got.bytes != 300 {
		t.Fatalf("look() = %+v, want the run dir's ciphertext and this call's attachment store (2 files, 300 bytes)", got)
	}
	if got := (downloadWatch{runDir: filepath.Join(runDir, "missing"), attachmentsDir: ""}).look(); got != (signalDownloads{}) {
		t.Fatalf("look() at missing dirs = %+v, want nothing", got)
	}
}

// --- runSignalCLI with a stub signal-cli -------------------------------------

// drainStubSignalCLI ignores SIGTERM like signal-cli finishing an envelope.
// STUB_MODE picks what it does meanwhile: grow streams STUB_STEPS chunks of
// ciphertext into its temp dir and then prints STUB_ENVELOPE; stall creates
// the ciphertext file and never writes to it; store creates a fresh
// attachments/*.tmp in STUB_CONFIG and never writes to it; idle downloads
// nothing. Each step is 50 ms.
const drainStubSignalCLI = `#!/bin/sh
trap 'echo "INFO  Shutdown - Received SIGTERM signal, shutting down ..." >&2' TERM
f="$TMPDIR/signal-cli_tmp_1.tmp"
case "$STUB_MODE" in
grow)
	: > "$f"
	i=0
	while [ "$i" -lt "$STUB_STEPS" ]; do
		printf 'ciphertext' >> "$f"
		sleep 0.05
		i=$((i+1))
	done
	rm -f "$f"
	printf '%s\n' "$STUB_ENVELOPE"
	exit 0 ;;
stall) : > "$f" ;;
store) mkdir -p "$STUB_CONFIG/attachments" && : > "$STUB_CONFIG/attachments/photo.jpg.42.tmp" ;;
esac
while :; do sleep 0.05; done
`

const drainStubEnvelope = `{"envelope":{"source":"+15551234567","timestamp":1700000000123}}`

type drainRun struct {
	output   string
	err      error
	elapsed  time.Duration
	held     bool
	beats    int32
	holdLeft bool // still holding after runSignalCLI returned
}

// runDrained runs the stub through the real runSignalCLI as the receive loop
// does: deadline after deadline, drained, with abort closing after abortAfter
// (zero: never).
func runDrained(t *testing.T, policy receiveDrainPolicy, mode string, deadline, abortAfter time.Duration) drainRun {
	t.Helper()
	t.Setenv("TMPDIR", t.TempDir())
	stub := filepath.Join(t.TempDir(), "signal-cli-stub")
	if err := os.WriteFile(stub, []byte(drainStubSignalCLI), 0o755); err != nil {
		t.Fatal(err)
	}
	configDir := t.TempDir()
	t.Setenv("OPENMESSAGES_SIGNAL_CLI", stub)
	t.Setenv("STUB_MODE", mode)
	t.Setenv("STUB_CONFIG", configDir)
	t.Setenv("STUB_ENVELOPE", drainStubEnvelope)
	if os.Getenv("STUB_STEPS") == "" {
		t.Setenv("STUB_STEPS", "1")
	}
	original := receiveDrain
	receiveDrain = policy
	t.Cleanup(func() { receiveDrain = original })

	abort := make(chan struct{})
	if abortAfter > 0 {
		timer := time.AfterFunc(abortAfter, func() { close(abort) })
		t.Cleanup(func() { timer.Stop() })
	}
	var holding atomic.Bool
	var held atomic.Bool
	var beats atomic.Int32
	watching := make(chan struct{})
	watched := make(chan struct{})
	go func() {
		defer close(watched)
		for {
			if holding.Load() {
				held.Store(true)
			}
			select {
			case <-watching:
				return
			case <-time.After(5 * time.Millisecond):
			}
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	ctx = withReceiveDrain(ctx, receiveDrainRequest{
		abort:   abort,
		holding: &holding,
		beat:    func(string) { beats.Add(1) },
		logger:  zerolog.Nop(),
	})
	started := time.Now()
	output, err := runSignalCLI(ctx, configDir, "-a", "+15550000000", "--output", "json", "receive")
	elapsed := time.Since(started)
	left := holding.Load()
	close(watching)
	<-watched
	return drainRun{output: string(output), err: err, elapsed: elapsed, held: held.Load(), beats: beats.Load(), holdLeft: left}
}

func testDrainPolicy() receiveDrainPolicy {
	return receiveDrainPolicy{idle: 300 * time.Millisecond, stall: 1500 * time.Millisecond, limit: 20 * time.Second, tick: 25 * time.Millisecond}
}

func wasKilled(err error) bool {
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr) && strings.Contains(exitErr.Error(), "killed")
}

func TestRunSignalCLIDrainsAStoppedReceiveWhileItsDownloadGrows(t *testing.T) {
	t.Setenv("STUB_STEPS", "30") // 1.5 s, five idle graces
	run := runDrained(t, testDrainPolicy(), "grow", 200*time.Millisecond, 0)
	if !strings.Contains(run.output, drainStubEnvelope) {
		t.Fatalf("output = %q (err %v after %s), want the envelope printed once the download finished", run.output, run.err, run.elapsed)
	}
	if !errors.Is(run.err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the deadline: the call exited 0 after its stop", run.err)
	}
	if !run.held || run.holdLeft {
		t.Fatalf("held = %v, still holding after return = %v; want held during the download and released after", run.held, run.holdLeft)
	}
	if run.beats < 1 {
		t.Fatal("the drain never beat the poller while the download progressed")
	}
}

func TestRunSignalCLIKillsADrainedReceiveWhoseDownloadStalls(t *testing.T) {
	policy := testDrainPolicy()
	deadline := 200 * time.Millisecond
	run := runDrained(t, policy, "stall", deadline, 0)
	if !wasKilled(run.err) {
		t.Fatalf("err = %v after %s, want the call killed", run.err, run.elapsed)
	}
	if run.elapsed < deadline+policy.stall || run.elapsed > deadline+policy.stall+3*time.Second {
		t.Fatalf("killed after %s, want about the deadline plus the %s stall window", run.elapsed, policy.stall)
	}
}

// signal-cli creates the attachments/*.tmp an attachment is stored through
// when the download starts; it stays empty until the download completes.
// Its presence alone holds the call, up to the stall window.
func TestRunSignalCLIHoldsAReceiveStoringAnAttachmentUntilTheStallWindow(t *testing.T) {
	policy := testDrainPolicy()
	deadline := 200 * time.Millisecond
	run := runDrained(t, policy, "store", deadline, 0)
	if !wasKilled(run.err) || !run.held {
		t.Fatalf("err = %v, held = %v after %s; want held, then killed", run.err, run.held, run.elapsed)
	}
	if run.elapsed < deadline+policy.stall {
		t.Fatalf("killed after %s, before the %s stall window: the store file was not counted", run.elapsed, policy.stall)
	}
}

func TestRunSignalCLIKillsADrainedReceiveAtTheLimit(t *testing.T) {
	t.Setenv("STUB_STEPS", "1000000")
	policy := testDrainPolicy()
	policy.limit = 1500 * time.Millisecond
	deadline := 200 * time.Millisecond
	run := runDrained(t, policy, "grow", deadline, 0)
	if !wasKilled(run.err) {
		t.Fatalf("err = %v after %s, want the call killed", run.err, run.elapsed)
	}
	if run.elapsed < deadline+policy.limit || run.elapsed > deadline+policy.limit+3*time.Second {
		t.Fatalf("killed after %s, want about the deadline plus the %s limit", run.elapsed, policy.limit)
	}
}

func TestRunSignalCLIKillsAStoppedReceiveWithoutADownloadAfterTheIdleGrace(t *testing.T) {
	policy := testDrainPolicy()
	deadline := 200 * time.Millisecond
	run := runDrained(t, policy, "idle", deadline, 0)
	if !wasKilled(run.err) || run.held {
		t.Fatalf("err = %v, held = %v after %s; want killed without being held", run.err, run.held, run.elapsed)
	}
	if run.elapsed < deadline+policy.idle || run.elapsed > deadline+policy.idle+3*time.Second {
		t.Fatalf("killed after %s, want about the deadline plus the %s idle grace", run.elapsed, policy.idle)
	}
}

// A stop by the caller (Close, Unpair, a new generation) waits for the
// receive loop, so it ends the drain: the call gets the idle grace from
// there even with a download growing.
func TestRunSignalCLIEndsTheDrainWhenTheCallerStops(t *testing.T) {
	t.Setenv("STUB_STEPS", "1000000")
	policy := testDrainPolicy()
	deadline := 200 * time.Millisecond
	abortAfter := 900 * time.Millisecond
	run := runDrained(t, policy, "grow", deadline, abortAfter)
	if !wasKilled(run.err) {
		t.Fatalf("err = %v after %s, want the call killed", run.err, run.elapsed)
	}
	if run.elapsed < abortAfter+policy.idle || run.elapsed > abortAfter+policy.idle+3*time.Second {
		t.Fatalf("killed after %s, want about the caller's stop plus the %s idle grace", run.elapsed, policy.idle)
	}
}

// Every call other than a receive keeps the fixed grace: a stub that ignores
// SIGTERM while "downloading" is killed signalStopGrace after its deadline.
func TestRunSignalCLIWithoutADrainRequestKeepsTheFixedGrace(t *testing.T) {
	if testing.Short() {
		t.Skip("waits out the real 3 s stop grace")
	}
	t.Setenv("TMPDIR", t.TempDir())
	stub := filepath.Join(t.TempDir(), "signal-cli-stub")
	if err := os.WriteFile(stub, []byte(drainStubSignalCLI), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENMESSAGES_SIGNAL_CLI", stub)
	t.Setenv("STUB_MODE", "grow")
	t.Setenv("STUB_STEPS", "1000000")
	deadline := 200 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	started := time.Now()
	_, err := runSignalCLI(ctx, t.TempDir(), "listContacts")
	elapsed := time.Since(started)
	if !wasKilled(err) || elapsed < deadline+signalStopGrace || elapsed > deadline+signalStopGrace+3*time.Second {
		t.Fatalf("err = %v after %s, want killed about %s after the deadline", err, elapsed, signalStopGrace)
	}
}

// --- the receive loop's request ------------------------------------------------

// The receive loop must hand every receive call a drain request whose abort
// is the loop's own stop, and whose holding flag is the one sends consult.
func TestStartReceiveLoopDrainsReceiveCallsUntilTheLoopStops(t *testing.T) {
	type seen struct {
		req receiveDrainRequest
		ok  bool
	}
	requests := make(chan seen, 64)
	bridge, _, calls := newFailedReceiveBridge(t, func(ctx context.Context, call int32) ([]byte, error) {
		req, ok := ctx.Value(receiveDrainKey{}).(receiveDrainRequest)
		select {
		case requests <- seen{req, ok}:
		default:
		}
		if call == 1 {
			// Runs into the deadline, as a call still downloading would.
			return runIntoDeadline(ctx, failedReceiveEnvelope("+15551234567", 1700000000123, "hi"))
		}
		return []byte{}, nil
	})
	go bridge.startReceiveLoop(failedReceiveAccount, false)
	waitForCondition(t, 2*time.Second, func() bool { return calls.Load() >= 2 })

	first := <-requests
	if !first.ok {
		t.Fatal("receive call context carries no drain request")
	}
	if first.req.holding != &bridge.receiveDraining {
		t.Fatal("drain request does not report holding through bridge.receiveDraining")
	}
	if first.req.abort == nil || isClosed(first.req.abort) {
		t.Fatal("drain request's abort is missing or already closed while the loop runs")
	}
	if err := bridge.Close(); err != nil {
		t.Fatal(err)
	}
	if !isClosed(first.req.abort) {
		t.Fatal("drain request's abort did not close when the loop stopped")
	}
}

func TestReceiveDrainPolicyFitsTheTimeoutsAroundIt(t *testing.T) {
	const cdnReadTimeout = 30 * time.Second // PushServiceSocket.soTimeoutMillis
	const supervisorLivenessTimeout = 2 * time.Minute
	p := receiveDrain
	for _, check := range []struct {
		ok   bool
		what string
	}{
		{p.idle == signalStopGrace, "idle is the grace every other call gets"},
		{p.idle <= p.stall && p.stall <= p.limit, "idle <= stall <= limit"},
		{p.stall > cdnReadTimeout, "a stalled download fails inside signal-cli (30 s read timeout) before the drain kills it"},
		{p.tick < p.idle, "the drain looks more often than the idle grace"},
		{drainBeatInterval < supervisorLivenessTimeout, "a progressing drain beats before the supervisor probes liveness"},
		{signalRunTmpMaxAge > receiveCallDeadline+p.limit+p.idle, "no sweep removes the files of a call still being drained"},
		{requestLockWait > receiveCallDeadline+signalStopGrace, "a request outwaits a receive call that is not drained"},
	} {
		if !check.ok {
			t.Errorf("receiveDrain %+v: %s", p, check.what)
		}
	}
}
