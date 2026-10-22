package signallive

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/rs/zerolog"
)

// signalStopGrace is how long a signal-cli call gets to exit after SIGTERM
// before it is killed.
const signalStopGrace = 3 * time.Second

// drainBeatInterval spaces the liveness beats a drain sends while a download
// progresses.
const drainBeatInterval = 5 * time.Second

// A receive call that outlives receiveCallDeadline gets SIGTERM. signal-cli
// handles it (ReceiveCommand installs Shutdown's handler, in the JVM and the
// native build alike): it stops reading from the server and waits for the
// envelope it is handling to finish, since ManagerImpl.stopReceiveThread
// interrupts only a blocked read. Handling an envelope includes downloading
// its attachments, and signal-cli prints the envelope only after that.
//
// Killing the call before that print loses the message. signal-cli caches
// and acknowledges each envelope before handling it, so the server never
// sends it again, and the decrypt has already advanced the session, so the
// next call's retry of the cached copy fails as a duplicate and signal-cli
// deletes it without printing it (ReceiveHelper.retryFailedReceivedMessage;
// libsignal 0.103 consume_message_key). An attachment download is the long
// part of that window: the CDN client allows 30 s per read and sets no
// overall deadline (PushServiceSocket.downloadFromCdn), so a large file on a
// slow link takes minutes.
//
// So a receive call stopped by its deadline is drained instead of being
// killed signalStopGrace after SIGTERM: it keeps running while a download in
// it makes progress.
type receiveDrainPolicy struct {
	// idle: kill once no download has been in flight for this long since
	// the stop, the grace every other call gets.
	idle time.Duration
	// stall: kill once the downloads in flight have not grown for this
	// long. It outlasts the CDN client's 30 s read timeout, so a stalled
	// download fails inside signal-cli first, which then still prints the
	// envelope.
	stall time.Duration
	// limit: kill this long after the stop whatever is in flight. A
	// download that delivers a byte per read timeout would otherwise never
	// end.
	limit time.Duration
	// tick: how often the drain looks at the downloads.
	tick time.Duration
}

// receiveDrain is the production policy. Tests swap it.
var receiveDrain = receiveDrainPolicy{
	idle:  signalStopGrace,
	stall: 45 * time.Second,
	limit: 10 * time.Minute,
	tick:  250 * time.Millisecond,
}

type receiveDrainKey struct{}

// receiveDrainRequest asks runSignalCLI to drain the call it starts.
type receiveDrainRequest struct {
	// abort closes when the caller itself stops (Close, Unpair, a new
	// generation). The call then gets only the idle grace: those stops wait
	// for the receive loop and must stay prompt.
	abort <-chan struct{}
	// holding is set while the call is alive past the idle grace only to
	// finish a download, so requests give up instead of queueing behind it.
	holding *drainFlag
	// beat reports the poller alive while a download progresses. Without
	// it the supervisor's liveness probe would retire the generation, and
	// so abort the call, two and a half minutes into a long download.
	beat   func(detail string)
	logger zerolog.Logger
}

func withReceiveDrain(ctx context.Context, req receiveDrainRequest) context.Context {
	return context.WithValue(ctx, receiveDrainKey{}, req)
}

// signalDownloads is what a drain can see of a call's downloads.
type signalDownloads struct {
	files    int
	bytes    int64
	modified int64 // newest modification time, unix nanoseconds
}

// downloadWatch finds the files a signal-cli call downloads through. The CDN
// client streams ciphertext into signal-cli_tmp_*.tmp in java.io.tmpdir,
// which runSignalCLI points at the call's own run dir, appending each chunk
// as it arrives (IOUtils.createTempFile, PushServiceSocket.downloadFromCdn).
// The plaintext goes through <name>.<random>.tmp in <configDir>/attachments,
// created when the download starts and written once it completes
// (AttachmentStore.storeAttachment). One modified before the call started is
// a leftover of a killed call. On a filesystem with whole-second timestamps a
// fresh one can look older and be missed; the run dir, which needs no age
// test, still shows the download.
type downloadWatch struct {
	runDir         string
	attachmentsDir string
	since          time.Time
}

func (w downloadWatch) look() signalDownloads {
	var downloads signalDownloads
	add := func(dir string, match func(name string) bool, since time.Time) {
		if dir == "" {
			return
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, entry := range entries {
			if !entry.Type().IsRegular() || !match(entry.Name()) {
				continue
			}
			info, err := entry.Info()
			if err != nil || info.ModTime().Before(since) {
				continue
			}
			downloads.files++
			downloads.bytes += info.Size()
			downloads.modified = max(downloads.modified, info.ModTime().UnixNano())
		}
	}
	add(w.runDir, func(name string) bool {
		return strings.HasPrefix(name, "signal-cli_tmp_") && strings.HasSuffix(name, ".tmp")
	}, time.Time{})
	add(w.attachmentsDir, isSignalAttachmentTemp, w.since)
	return downloads
}

// isSignalAttachmentTemp matches the temp files AttachmentStore stores an
// attachment through, <stored name>.<digits>.tmp (Files.createTempFile adds
// an unsigned long), and not a stored attachment whose own extension is
// .tmp, <id>.tmp.
func isSignalAttachmentTemp(name string) bool {
	rest, ok := strings.CutSuffix(name, ".tmp")
	if !ok {
		return false
	}
	dot := strings.LastIndexByte(rest, '.')
	if dot <= 0 || dot == len(rest)-1 {
		return false
	}
	for _, r := range rest[dot+1:] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// drainClock decides, from what it has seen of a stopped call's downloads,
// when the call is killed.
type drainClock struct {
	policy       receiveDrainPolicy
	stoppedAt    time.Time
	abortedAt    time.Time // zero until the caller stops
	lastInFlight time.Time // zero until a download is seen
	lastProgress time.Time
	seen         signalDownloads
}

type drainVerdict struct {
	kill bool
	// holding: alive past the idle grace only because a download is
	// progressing.
	holding bool
	// progressed: the downloads in flight changed since the last look.
	progressed bool
}

// observe records one look at the call's downloads, taken at at.
func (c *drainClock) observe(at time.Time, downloads signalDownloads, aborted bool) drainVerdict {
	if aborted && c.abortedAt.IsZero() {
		c.abortedAt = at
	}
	var verdict drainVerdict
	inFlight := downloads.files > 0
	if inFlight {
		if c.lastInFlight.IsZero() || downloads != c.seen {
			c.lastProgress = at
			verdict.progressed = true
		}
		c.lastInFlight = at
	}
	c.seen = downloads
	switch {
	case at.Sub(c.stoppedAt) >= c.policy.limit:
		verdict.kill = true
	case !c.abortedAt.IsZero():
		verdict.kill = at.Sub(later(c.stoppedAt, c.abortedAt)) >= c.policy.idle
	case inFlight:
		verdict.kill = at.Sub(c.lastProgress) >= c.policy.stall
		verdict.holding = !verdict.kill && at.Sub(c.stoppedAt) >= c.policy.idle
	default:
		verdict.kill = at.Sub(later(c.stoppedAt, c.lastInFlight)) >= c.policy.idle
	}
	return verdict
}

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// drain watches a receive call from its SIGTERM until it exits (exited
// closes), killing it when the clock says so.
func drain(process *os.Process, req receiveDrainRequest, watch downloadWatch, policy receiveDrainPolicy, exited <-chan struct{}) {
	clock := drainClock{policy: policy, stoppedAt: time.Now()}
	ticker := time.NewTicker(policy.tick)
	defer ticker.Stop()
	defer req.holding.Store(false)
	abort := req.abort
	var lastBeat time.Time
	held := false
	for {
		at := time.Now()
		downloads := watch.look()
		verdict := clock.observe(at, downloads, isClosed(req.abort))
		req.holding.Store(verdict.holding)
		if verdict.holding && !held {
			held = true
			req.logger.Info().
				Int("downloads", downloads.files).
				Int64("bytes", downloads.bytes).
				Msg("Holding a stopped Signal receive call open while it finishes an attachment download")
		}
		if verdict.progressed && req.beat != nil && at.Sub(lastBeat) >= drainBeatInterval {
			lastBeat = at
			req.beat("receive_download")
		}
		if verdict.kill {
			if err := process.Kill(); errors.Is(err, os.ErrProcessDone) {
				return
			}
			event := req.logger.Warn().
				Dur("after_stop", at.Sub(clock.stoppedAt)).
				Bool("aborted", !clock.abortedAt.IsZero())
			if downloads.files > 0 {
				event.Int("downloads", downloads.files).
					Int64("bytes", downloads.bytes).
					Msg("Killed a Signal receive call with an attachment download in flight; signal-cli had acknowledged the envelope it was handling, so that message is lost")
			} else {
				event.Msg("Killed a Signal receive call that had not exited after SIGTERM; an envelope it was still handling, if any, is lost")
			}
			return
		}
		select {
		case <-exited:
			if held {
				req.logger.Info().
					Dur("after_stop", time.Since(clock.stoppedAt)).
					Msg("Stopped Signal receive call exited after finishing its attachment download")
			}
			return
		case <-abort:
			abort = nil
		case <-ticker.C:
		}
	}
}

func isClosed(ch <-chan struct{}) bool {
	if ch == nil {
		return false
	}
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// configureSignalStop is configureSignalCancel for a call whose context may
// carry a receiveDrainRequest: such a call is drained after SIGTERM rather
// than killed signalStopGrace later. The returned func must run once the
// command has exited; it waits for the drain to let go of the process.
func configureSignalStop(ctx context.Context, cmd *exec.Cmd, configDir, runDir string) func() {
	req, ok := ctx.Value(receiveDrainKey{}).(receiveDrainRequest)
	if !ok {
		configureSignalCancel(cmd)
		return func() {}
	}
	if req.holding == nil {
		req.holding = new(drainFlag)
	}
	policy := receiveDrain
	watch := downloadWatch{
		runDir:         runDir,
		attachmentsDir: filepath.Join(configDir, "attachments"),
		since:          time.Now(),
	}
	exited := make(chan struct{})
	drained := make(chan struct{})
	var draining atomic.Bool
	cmd.Cancel = func() error {
		if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
			return cmd.Process.Kill()
		}
		draining.Store(true)
		go func() {
			defer close(drained)
			drain(cmd.Process, req, watch, policy, exited)
		}()
		return nil
	}
	// A backstop only: the drain kills the call by policy.limit. WaitDelay
	// also bounds the wait for the output pipe to close, so an executable
	// that leaves a child holding it (a wrapper script that doesn't exec
	// signal-cli) can hold the call this long rather than signalStopGrace.
	cmd.WaitDelay = policy.limit + policy.idle
	return func() {
		close(exited)
		// exec calls Cancel, if at all, before Wait returns.
		if draining.Load() {
			<-drained
		}
	}
}
