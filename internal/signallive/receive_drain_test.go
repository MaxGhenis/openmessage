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
	write(filepath.Join(attachments, "D49ZGyDHvzgVPhh.tmp"), 60, fresh) // a stored attachment named .tmp by its sender

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
// STUB_MODE picks its download: grow streams a chunk of ciphertext into its
// temp dir every 50 ms until it has written STUB_STEPS chunks or STUB_FINISH
// exists, and then prints STUB_ENVELOPE and exits 0; stall creates the
// ciphertext file and never writes to it; store creates a fresh
// attachments/*.tmp in STUB_CONFIG and never writes to it; idle downloads
// nothing. It touches STUB_READY once its trap is set and its download has
// begun, so a drain that starts after that always sees the download.
const drainStubSignalCLI = `#!/bin/sh
trap 'echo "INFO  Shutdown - Received SIGTERM signal, shutting down ..." >&2' TERM
f="$TMPDIR/signal-cli_tmp_1.tmp"
case "$STUB_MODE" in
grow|stall) : > "$f" ;;
store) mkdir -p "$STUB_CONFIG/attachments" && : > "$STUB_CONFIG/attachments/photo.jpg.42.tmp" ;;
esac
: > "$STUB_READY"
if [ "$STUB_MODE" = grow ]; then
	i=0
	while [ "$i" -lt "$STUB_STEPS" ] && [ ! -e "$STUB_FINISH" ]; do
		printf 'ciphertext' >> "$f"
		sleep 0.05
		i=$((i+1))
	done
	rm -f "$f"
	printf '%s\n' "$STUB_ENVELOPE"
	exit 0
fi
while :; do sleep 0.05; done
`

const drainStubEnvelope = `{"envelope":{"source":"+15551234567","timestamp":1700000000123}}`

// stubReadyWait bounds how long a stub may take to start. A freshly written
// script can take seconds to start on a loaded macOS host.
const stubReadyWait = time.Minute

// stubStop ends a stub call's context deadline after the stub is ready, and
// records when it did. The tests time what the drain does from that stop,
// not from the spawn: how long the stub takes to start is no part of the
// drain, and on a loaded host it varies by seconds.
type stubStop struct {
	ctx      context.Context
	returned chan struct{} // closed by stopped once the call has returned
	done     chan struct{} // closed once the stopper has finished
	ready    bool          // STUB_READY appeared; read after done
	at       time.Time     // when the deadline ended ctx, zero if it never did; read after done
}

// stubDeadline returns a stop whose context ends deadline after STUB_READY
// appears, so the SIGTERM always reaches a shell that has set its trap and
// begun its download.
func stubDeadline(t *testing.T, deadline time.Duration) *stubStop {
	t.Helper()
	ready := filepath.Join(t.TempDir(), "ready")
	t.Setenv("STUB_READY", ready)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	stop := &stubStop{ctx: ctx, returned: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(stop.done)
		poll := time.NewTicker(5 * time.Millisecond)
		defer poll.Stop()
		giveUp := time.After(stubReadyWait)
		for !stop.ready {
			if _, err := os.Stat(ready); err == nil {
				stop.ready = true
				break
			}
			select {
			case <-stop.returned:
				return
			case <-giveUp:
				cancel() // end the call; stopped reports the stub never started
				return
			case <-poll.C:
			}
		}
		select {
		case <-stop.returned:
			return
		case <-time.After(deadline):
		}
		stop.at = time.Now()
		cancel()
	}()
	return stop
}

// stopped, called once the call has returned, gives the moment the deadline
// stopped it. It fails the test if the stub never started or the call ended
// before its deadline.
func (s *stubStop) stopped(t *testing.T, err error, output []byte) time.Time {
	t.Helper()
	close(s.returned)
	<-s.done
	if !s.ready {
		t.Fatalf("the stub did not start within %s: err = %v, output = %q", stubReadyWait, err, output)
	}
	if s.at.IsZero() {
		t.Fatalf("the call ended before its deadline stopped it: err = %v, output = %q", err, output)
	}
	return s.at
}

type drainRun struct {
	output     string
	err        error
	afterStop  time.Duration // from the deadline's stop to runSignalCLI's return
	afterAbort time.Duration // from the caller's stop to the return; zero without one
	held       bool          // the drain held the call open for its download
	beats      int32
	holdLeft   bool // still holding after runSignalCLI returned
}

// drainEvents says what happens once the drain first holds the call open.
type drainEvents struct {
	// callerStops closes the drain request's abort, as Close, Unpair or a
	// new generation does.
	callerStops bool
	// finishAfter, when positive, ends the stub's download this long later.
	finishAfter time.Duration
}

// runDrained runs the stub through the real runSignalCLI as the receive loop
// does: stopped by its deadline, drained, and with then happening once the
// drain holds the call.
func runDrained(t *testing.T, policy receiveDrainPolicy, mode string, deadline time.Duration, then drainEvents) drainRun {
	t.Helper()
	t.Setenv("TMPDIR", t.TempDir())
	stub := filepath.Join(t.TempDir(), "signal-cli-stub")
	if err := os.WriteFile(stub, []byte(drainStubSignalCLI), 0o755); err != nil {
		t.Fatal(err)
	}
	configDir := t.TempDir()
	finish := filepath.Join(t.TempDir(), "finish")
	t.Setenv("OPENMESSAGES_SIGNAL_CLI", stub)
	t.Setenv("STUB_MODE", mode)
	t.Setenv("STUB_CONFIG", configDir)
	t.Setenv("STUB_ENVELOPE", drainStubEnvelope)
	t.Setenv("STUB_FINISH", finish)
	if os.Getenv("STUB_STEPS") == "" {
		t.Setenv("STUB_STEPS", "1")
	}
	original := receiveDrain
	receiveDrain = policy
	t.Cleanup(func() { receiveDrain = original })

	var holding drainFlag
	held := holding.started() // closes the first time the drain holds the call
	abort := make(chan struct{})
	var abortedAt time.Time
	returned := make(chan struct{})
	acted := make(chan struct{})
	go func() {
		defer close(acted)
		if !then.callerStops && then.finishAfter <= 0 {
			return
		}
		select {
		case <-held:
		case <-returned:
			return
		}
		if then.callerStops {
			abortedAt = time.Now()
			close(abort)
		}
		if then.finishAfter > 0 {
			select {
			case <-time.After(then.finishAfter):
			case <-returned:
				return
			}
			if err := os.WriteFile(finish, nil, 0o600); err != nil {
				t.Errorf("finish the stub's download: %v", err)
			}
		}
	}()

	var beats atomic.Int32
	stop := stubDeadline(t, deadline)
	ctx := withReceiveDrain(stop.ctx, receiveDrainRequest{
		abort:   abort,
		holding: &holding,
		beat:    func(string) { beats.Add(1) },
		logger:  zerolog.Nop(),
	})
	output, err := runSignalCLI(ctx, configDir, "-a", "+15550000000", "--output", "json", "receive")
	returnedAt := time.Now()
	left := holding.Load()
	close(returned)
	<-acted
	run := drainRun{
		output:    string(output),
		err:       err,
		afterStop: returnedAt.Sub(stop.stopped(t, err, output)),
		held:      isClosed(held),
		beats:     beats.Load(),
		holdLeft:  left,
	}
	if !abortedAt.IsZero() {
		run.afterAbort = returnedAt.Sub(abortedAt)
	}
	return run
}

func testDrainPolicy() receiveDrainPolicy {
	return receiveDrainPolicy{idle: 300 * time.Millisecond, stall: 1500 * time.Millisecond, limit: 20 * time.Second, tick: 25 * time.Millisecond}
}

// drainSlack bounds how long after the drain's verdict a test accepts the
// call to return: the next tick, the kill and the stub's exit. It is
// counted from the stop, which the drain itself starts from, so a slow
// spawn never uses it up.
const drainSlack = 3 * time.Second

func wasKilled(err error) bool {
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr) && strings.Contains(exitErr.Error(), "killed")
}

func TestRunSignalCLIDrainsAStoppedReceiveWhileItsDownloadGrows(t *testing.T) {
	t.Setenv("STUB_STEPS", "1000000") // grows until the test finishes it
	policy := testDrainPolicy()
	// The download keeps growing for three idle graces after the drain
	// first holds the call, then finishes.
	run := runDrained(t, policy, "grow", 200*time.Millisecond, drainEvents{finishAfter: 3 * policy.idle})
	if !strings.Contains(run.output, drainStubEnvelope) {
		t.Fatalf("output = %q (err %v, %s after the stop), want the envelope printed once the download finished", run.output, run.err, run.afterStop)
	}
	if !errors.Is(run.err, context.Canceled) {
		t.Fatalf("err = %v, want the stop: the call exited 0 after it", run.err)
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
	run := runDrained(t, policy, "stall", 200*time.Millisecond, drainEvents{})
	if !wasKilled(run.err) {
		t.Fatalf("err = %v %s after the stop, want the call killed", run.err, run.afterStop)
	}
	if run.afterStop < policy.stall || run.afterStop > policy.stall+drainSlack {
		t.Fatalf("killed %s after the stop, want about the %s stall window", run.afterStop, policy.stall)
	}
}

// signal-cli creates the attachments/*.tmp an attachment is stored through
// when the download starts; it stays empty until the download completes.
// Its presence alone holds the call, up to the stall window.
func TestRunSignalCLIHoldsAReceiveStoringAnAttachmentUntilTheStallWindow(t *testing.T) {
	policy := testDrainPolicy()
	run := runDrained(t, policy, "store", 200*time.Millisecond, drainEvents{})
	if !wasKilled(run.err) || !run.held {
		t.Fatalf("err = %v, held = %v %s after the stop; want held, then killed", run.err, run.held, run.afterStop)
	}
	if run.afterStop < policy.stall {
		t.Fatalf("killed %s after the stop, before the %s stall window: the store file was not counted", run.afterStop, policy.stall)
	}
}

func TestRunSignalCLIKillsADrainedReceiveAtTheLimit(t *testing.T) {
	t.Setenv("STUB_STEPS", "1000000")
	policy := testDrainPolicy()
	policy.limit = 1500 * time.Millisecond
	run := runDrained(t, policy, "grow", 200*time.Millisecond, drainEvents{})
	if !wasKilled(run.err) {
		t.Fatalf("err = %v %s after the stop, want the call killed", run.err, run.afterStop)
	}
	if run.afterStop < policy.limit || run.afterStop > policy.limit+drainSlack {
		t.Fatalf("killed %s after the stop, want about the %s limit", run.afterStop, policy.limit)
	}
}

func TestRunSignalCLIKillsAStoppedReceiveWithoutADownloadAfterTheIdleGrace(t *testing.T) {
	policy := testDrainPolicy()
	run := runDrained(t, policy, "idle", 200*time.Millisecond, drainEvents{})
	if !wasKilled(run.err) || run.held {
		t.Fatalf("err = %v, held = %v %s after the stop; want killed without being held", run.err, run.held, run.afterStop)
	}
	if run.afterStop < policy.idle || run.afterStop > policy.idle+drainSlack {
		t.Fatalf("killed %s after the stop, want about the %s idle grace", run.afterStop, policy.idle)
	}
}

// A stop by the caller (Close, Unpair, a new generation) waits for the
// receive loop, so it ends the drain: the call gets the idle grace from
// there even with a download growing.
func TestRunSignalCLIEndsTheDrainWhenTheCallerStops(t *testing.T) {
	t.Setenv("STUB_STEPS", "1000000")
	policy := testDrainPolicy()
	run := runDrained(t, policy, "grow", 200*time.Millisecond, drainEvents{callerStops: true})
	if !wasKilled(run.err) || !run.held {
		t.Fatalf("err = %v, held = %v %s after the stop; want the call held for its download, then killed", run.err, run.held, run.afterStop)
	}
	if run.afterAbort < policy.idle || run.afterAbort > policy.idle+drainSlack {
		t.Fatalf("killed %s after the caller stopped, want about the %s idle grace", run.afterAbort, policy.idle)
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
	t.Setenv("STUB_FINISH", filepath.Join(t.TempDir(), "finish"))
	stop := stubDeadline(t, 200*time.Millisecond)
	output, err := runSignalCLI(stop.ctx, t.TempDir(), "listContacts")
	afterStop := time.Since(stop.stopped(t, err, output))
	if !wasKilled(err) || afterStop < signalStopGrace || afterStop > signalStopGrace+drainSlack {
		t.Fatalf("err = %v %s after the stop, want killed %s after it", err, afterStop, signalStopGrace)
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
	startTrackedReceiveLoop(bridge, failedReceiveAccount)
	waitForCondition(t, receiveLoopWait, func() bool { return calls.Load() >= 2 })

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
		{p.stall+drainBeatInterval < supervisorLivenessTimeout, "the longest gap between a drain's beats (a stall) ends before the liveness probe"},
		{signalRunTmpMaxAge > receiveCallDeadline+p.limit+p.idle, "no sweep removes the files of a call still being drained"},
		{requestLockWait > receiveCallDeadline+signalStopGrace, "a request outwaits a receive call that is not drained"},
	} {
		if !check.ok {
			t.Errorf("receiveDrain %+v: %s", p, check.what)
		}
	}
}

func TestIsSignalAttachmentTempMatchesOnlyJavaTempNames(t *testing.T) {
	for name, want := range map[string]bool{
		"D49ZGyDHvzgVPhhQxljN.html.8812345678901234567.tmp": true,
		"photo.jpg.42.tmp":     true,
		"noext.7.tmp":          true,
		"D49ZGyDHvzgVPhh.tmp":  false, // stored attachment whose extension is .tmp
		"12345.tmp":            false,
		".42.tmp":              false,
		"photo.jpg..tmp":       false,
		"photo.jpg.4x2.tmp":    false,
		"photo.jpg.42.tmp.jpg": false,
		"photo.jpg":            false,
	} {
		if got := isSignalAttachmentTemp(name); got != want {
			t.Errorf("isSignalAttachmentTemp(%q) = %v, want %v", name, got, want)
		}
	}
}
