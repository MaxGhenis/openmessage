package silencerecovery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/maxghenis/openmessage/internal/freshness"
)

// fakeSource is an activity clock over a fixed event list. Like the real
// inbox, it only shows events that have happened by the harness clock.
type fakeSource struct {
	mu     sync.Mutex
	clock  *time.Time
	events []time.Time // sorted

	latestErr    error
	betweenErr   func(from, to time.Time) error
	calls        int
	betweenCalls int
}

func (s *fakeSource) Name() string { return "fake" }

func (s *fakeSource) add(events ...time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, events...)
	sort.Slice(s.events, func(i, j int) bool { return s.events[i].Before(s.events[j]) })
}

func (s *fakeSource) Latest(context.Context) (map[string]time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.latestErr != nil {
		return nil, s.latestErr
	}
	var latest time.Time
	for _, event := range s.events {
		if event.After(*s.clock) {
			break
		}
		latest = event
	}
	if latest.IsZero() {
		return map[string]time.Time{}, nil
	}
	return map[string]time.Time{"google": latest}, nil
}

func (s *fakeSource) Between(_ context.Context, platform string, from, to time.Time) ([]time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if platform != "google" {
		return nil, nil
	}
	s.betweenCalls++
	if s.betweenErr != nil {
		if err := s.betweenErr(from, to); err != nil {
			return nil, err
		}
	}
	// The store has millisecond receipts and an inclusive range.
	fromMS, toMS := from.UnixMilli(), to.UnixMilli()
	var out []time.Time
	for _, event := range s.events {
		if event.After(*s.clock) {
			break
		}
		if ms := event.UnixMilli(); ms >= fromMS && ms <= toMS {
			out = append(out, event)
		}
	}
	return out, nil
}

// fakeRunner records window backfill calls.
type fakeRunner struct {
	mu      sync.Mutex
	ready   bool
	reason  string
	results []runOutcome // consumed in order; the last one repeats
	calls   []time.Time
	// onRun sees every call, including ones the guard refused (started false).
	onRun func(since time.Time, started bool)
}

type runOutcome struct {
	result  RunResult
	started bool
}

var goodRun = runOutcome{started: true, result: RunResult{
	Connected: true, Listed: 120, Conversations: 7, Messages: 31, HistoryTeed: 38,
}}

func (f *fakeRunner) Ready() (bool, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ready, f.reason
}

func (f *fakeRunner) RunWindowBackfill(since time.Time) (RunResult, bool) {
	f.mu.Lock()
	f.calls = append(f.calls, since)
	outcome := goodRun
	if len(f.results) > 0 {
		outcome = f.results[0]
		if len(f.results) > 1 {
			f.results = f.results[1:]
		}
	}
	onRun := f.onRun
	f.mu.Unlock()
	if onRun != nil {
		onRun(since, outcome.started)
	}
	return outcome.result, outcome.started
}

func (f *fakeRunner) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

type harness struct {
	t      *testing.T
	clock  time.Time
	source *fakeSource
	runner *fakeRunner
	path   string
	cfg    Config
	rec    *Recoverer
}

var newYork = mustZone("America/New_York")

func mustZone(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return loc
}

func testConfig() Config {
	cfg := DefaultConfig()
	cfg.Location = newYork
	return cfg
}

func newHarness(t *testing.T, start time.Time) *harness {
	t.Helper()
	h := &harness{
		t:      t,
		clock:  start,
		runner: &fakeRunner{ready: true},
		path:   filepath.Join(t.TempDir(), StateFileName),
		cfg:    testConfig(),
	}
	h.source = &fakeSource{clock: &h.clock}
	h.restart()
	return h
}

// restart builds a fresh Recoverer over the same state file, as a daemon
// restart does.
func (h *harness) restart() {
	h.rec = New(h.cfg, h.source, h.runner, h.path, zerolog.Nop())
	h.rec.now = func() time.Time { return h.clock }
}

func (h *harness) tickAt(at time.Time) {
	h.t.Helper()
	h.clock = at
	h.rec.Tick(context.Background())
}

func (h *harness) savedState() State {
	h.t.Helper()
	state, err := loadState(h.path)
	if err != nil {
		h.t.Fatalf("load state: %v", err)
	}
	return state
}

// busyBaseline adds an event every 20 minutes from 07:00 to 23:00 local on
// each of the days whole days before lastDay and on lastDay itself, whose
// last event, at 23:00, it returns. The profile is active in the local hours
// 07 through 23, with a median of 49 events a day.
func busyBaseline(source *fakeSource, lastDay time.Time, days int) time.Time {
	y, m, d := lastDay.Date()
	var events []time.Time
	for offset := -days; offset <= 0; offset++ {
		for minute := 7 * 60; minute <= 23*60; minute += 20 {
			events = append(events, time.Date(y, m, d+offset, 0, minute, 0, 0, newYork))
		}
	}
	source.add(events...)
	return time.Date(y, m, d, 23, 0, 0, 0, newYork)
}

// day is a Monday in October 2026.
var day = time.Date(2026, 10, 5, 0, 0, 0, 0, newYork)

func TestStalledSilenceEndTriggersOneWindowBackfill(t *testing.T) {
	h := newHarness(t, day)
	// No lazy watermark saves after the first: whatever reaches the file
	// below was saved at once.
	h.cfg.PersistEvery = 365 * 24 * time.Hour
	h.restart()
	last := busyBaseline(h.source, day, 14)

	h.tickAt(last.Add(time.Minute)) // first run: the watermark starts at last
	if got := h.rec.State().WatermarkMS; got != last.UnixMilli() {
		t.Fatalf("watermark = %d, want %d", got, last.UnixMilli())
	}

	// The profile is active 07:00–24:00. From 23:00 to 11:00 the next day
	// it expected activity in 5 hours (23–24 and 07–11): not yet stalled.
	h.tickAt(last.Add(12 * time.Hour))
	if flagged := h.rec.State().Flagged; flagged != nil {
		t.Fatalf("flagged after 12h with 5 expected active hours: %+v", flagged)
	}
	// By 12:00 it expected activity in 6 silent hours: stalled.
	h.tickAt(last.Add(13 * time.Hour))
	flagged := h.rec.State().Flagged
	if flagged == nil || flagged.LastEventMS != last.UnixMilli() || flagged.Rule != freshness.RuleExpectedActivity {
		t.Fatalf("flag after 13h = %+v, want one on the last event by expected_activity", flagged)
	}
	if saved := h.savedState().Flagged; saved == nil {
		t.Fatal("the flag was not persisted at once")
	}
	for i := 1; i <= 5; i++ {
		h.tickAt(last.Add(13*time.Hour + time.Duration(i)*time.Hour))
	}
	if again := h.rec.State().Flagged; again == nil || again.FlaggedAtMS != flagged.FlaggedAtMS {
		t.Fatalf("the flag was re-stamped on later ticks: %+v, first %+v", again, flagged)
	}

	// A live frame arrives 30h after the last one: the silence ended.
	end := last.Add(30 * time.Hour)
	h.source.add(end)
	h.tickAt(end.Add(30 * time.Second))
	state := h.rec.State()
	if state.Flagged != nil {
		t.Fatalf("flag kept after the silence ended: %+v", state.Flagged)
	}
	pending := state.Pending
	if pending == nil {
		t.Fatal("no pending episode after a flagged silence ended")
	}
	wantSince := last.Add(-time.Hour)
	if pending.LastEventMS != last.UnixMilli() || pending.EndedAtMS != end.UnixMilli() || pending.SinceMS != wantSince.UnixMilli() {
		t.Fatalf("pending = %+v, want last %d end %d since %d", pending, last.UnixMilli(), end.UnixMilli(), wantSince.UnixMilli())
	}
	if h.runner.callCount() != 0 {
		t.Fatal("ran before the settle delay")
	}
	if saved := h.savedState().Pending; saved == nil || saved.State != StatePending {
		t.Fatalf("pending episode not persisted at once: %+v", saved)
	}

	h.tickAt(end.Add(2*time.Minute + time.Second))
	if h.runner.callCount() != 1 || !h.runner.calls[0].Equal(wantSince) {
		t.Fatalf("calls = %v, want one from %v", h.runner.calls, wantSince)
	}
	state = h.rec.State()
	if state.Pending != nil || len(state.History) != 1 {
		t.Fatalf("state after a good run = %+v", state)
	}
	done := state.History[0]
	if done.State != StateRecovered || done.Attempts != 1 || done.LastAttempt == nil || done.LastAttempt.Outcome != OutcomeRecovered {
		t.Fatalf("finished episode = %+v", done)
	}

	// Later traffic, short gaps and restarts never run it again.
	for i := 1; i <= 50; i++ {
		at := end.Add(time.Duration(i) * 7 * time.Minute)
		h.source.add(at)
		h.tickAt(at.Add(time.Second))
		if i%10 == 0 {
			h.restart()
		}
	}
	if h.runner.callCount() != 1 {
		t.Fatalf("window backfill ran %d times for one silence", h.runner.callCount())
	}
	if saved := h.savedState(); len(saved.History) != 1 || saved.History[0].State != StateRecovered {
		t.Fatalf("saved history = %+v", saved.History)
	}
}

func TestShortOrOrdinarySilencesNeverRecover(t *testing.T) {
	h := newHarness(t, day)
	last := busyBaseline(h.source, day, 14)
	h.tickAt(last.Add(time.Second))

	// An ordinary night: 23:00 to 09:00, 2 expected active hours, 10h < 16h.
	morning := last.Add(10 * time.Hour)
	for at := last.Add(time.Hour); at.Before(morning); at = at.Add(30 * time.Minute) {
		h.tickAt(at)
	}
	h.source.add(morning)
	h.tickAt(morning.Add(time.Second))
	// A 5-hour gap in the middle of the day, below every rule's minimum.
	afternoon := morning.Add(5 * time.Hour)
	h.source.add(afternoon)
	h.tickAt(afternoon.Add(time.Second))
	h.tickAt(afternoon.Add(time.Hour))

	state := h.rec.State()
	if state.Flagged != nil || state.Pending != nil || len(state.History) != 0 {
		t.Fatalf("an unflagged silence produced recovery state: %+v", state)
	}
	if h.runner.callCount() != 0 {
		t.Fatal("ran a window backfill for an unflagged silence")
	}
}

func TestEpisodeEndDetectedAfterRestartWhileDown(t *testing.T) {
	h := newHarness(t, day)
	last := busyBaseline(h.source, day, 14)
	h.tickAt(last.Add(time.Second)) // persists the watermark (first save)
	if h.savedState().WatermarkMS != last.UnixMilli() {
		t.Fatal("watermark not saved on the first tick")
	}

	// The daemon is down for the whole silence and comes back after the
	// first live frame landed: nothing ever flagged it live.
	end := last.Add(40 * time.Hour)
	h.source.add(end, end.Add(time.Minute))
	h.clock = end.Add(90 * time.Second)
	h.restart()
	h.tickAt(end.Add(90 * time.Second))
	pending := h.rec.State().Pending
	if pending == nil || pending.LastEventMS != last.UnixMilli() || pending.EndedAtMS != end.UnixMilli() {
		t.Fatalf("pending after restart = %+v", pending)
	}
	if pending.Rule != freshness.RuleExpectedActivity {
		t.Errorf("rule = %q", pending.Rule)
	}
	h.tickAt(end.Add(5 * time.Minute))
	if h.runner.callCount() != 1 {
		t.Fatalf("calls = %d, want 1", h.runner.callCount())
	}
}

func TestStaleWatermarkRescanDoesNotRepeatARecovery(t *testing.T) {
	h := newHarness(t, day)
	last := busyBaseline(h.source, day, 14)
	h.tickAt(last.Add(time.Second))
	end := last.Add(20 * time.Hour)
	h.source.add(end)
	h.tickAt(end.Add(time.Second))
	h.tickAt(end.Add(3 * time.Minute))
	if h.runner.callCount() != 1 {
		t.Fatalf("calls = %d", h.runner.callCount())
	}

	// A crash loses the latest watermark: the file says the silence's last
	// event again, so the next start rescans the same gap.
	state := h.savedState()
	state.WatermarkMS = last.UnixMilli()
	if err := saveState(h.path, state); err != nil {
		t.Fatal(err)
	}
	h.restart()
	h.tickAt(end.Add(10 * time.Minute))
	h.tickAt(end.Add(20 * time.Minute))
	if h.runner.callCount() != 1 {
		t.Fatalf("a rescan after a lost watermark ran the recovery again (%d calls)", h.runner.callCount())
	}
}

func TestInterruptedRunRetriesAfterRestart(t *testing.T) {
	h := newHarness(t, day)
	last := busyBaseline(h.source, day, 14)
	h.tickAt(last.Add(time.Second))
	end := last.Add(20 * time.Hour)
	h.source.add(end)
	h.tickAt(end.Add(time.Second))

	// The daemon dies during the run: capture the file as the run starts.
	var during State
	h.runner.onRun = func(time.Time, bool) { during = h.savedState() }
	h.tickAt(end.Add(3 * time.Minute))
	if during.Pending == nil || during.Pending.State != StateRunning || during.Pending.Attempts != 1 {
		t.Fatalf("state persisted before the run = %+v, want running with the attempt counted", during.Pending)
	}
	if err := saveState(h.path, during); err != nil {
		t.Fatal(err)
	}
	h.runner.onRun = nil
	h.restart()
	restartAt := end.Add(4 * time.Minute)
	h.tickAt(restartAt)
	pending := h.rec.State().Pending
	if pending == nil || pending.State != StatePending || pending.Failures != 1 || pending.LastAttempt == nil || pending.LastAttempt.Outcome != OutcomeCrashed {
		t.Fatalf("an interrupted run did not read back as a failed attempt: %+v", pending)
	}
	if pending.NextAttemptMS != restartAt.Add(h.cfg.Backoff[0]).UnixMilli() {
		t.Fatalf("next attempt = %d, want the first backoff after the restart", pending.NextAttemptMS)
	}
	if h.runner.callCount() != 1 {
		t.Fatalf("calls = %d: retried at once instead of backing off", h.runner.callCount())
	}
	h.tickAt(restartAt.Add(h.cfg.Backoff[0]))
	if h.runner.callCount() != 2 {
		t.Fatalf("calls = %d, want the interrupted run plus one retry", h.runner.callCount())
	}
	if done := h.rec.State().History; len(done) != 1 || done[0].Attempts != 2 || done[0].State != StateRecovered {
		t.Fatalf("history = %+v", done)
	}
}

func TestCrashLoopingRunIsGivenUp(t *testing.T) {
	h := newHarness(t, day)
	last := busyBaseline(h.source, day, 14)
	h.tickAt(last.Add(time.Second))
	end := last.Add(20 * time.Hour)
	h.source.add(end)
	h.tickAt(end.Add(time.Second))

	// Every run kills the daemon: the file keeps saying running.
	var during State
	h.runner.onRun = func(time.Time, bool) { during = h.savedState() }
	at := end.Add(3 * time.Minute)
	for i := 0; i < 50 && len(h.rec.State().History) == 0; i++ {
		calls := h.runner.callCount()
		h.tickAt(at)
		if h.runner.callCount() > calls {
			if err := saveState(h.path, during); err != nil {
				t.Fatal(err)
			}
			h.restart()
		}
		at = at.Add(5 * time.Hour)
	}
	maxFailures := len(h.cfg.Backoff) + 1
	if h.runner.callCount() != maxFailures {
		t.Fatalf("calls = %d, want %d before giving up", h.runner.callCount(), maxFailures)
	}
	done := h.rec.State().History
	if len(done) != 1 || done[0].State != StateGaveUp || !strings.Contains(done[0].Notice, "daemon stopped") {
		t.Fatalf("history = %+v", done)
	}
}

func TestBusyGuardRetriesWithoutCountingAnAttempt(t *testing.T) {
	h := newHarness(t, day)
	last := busyBaseline(h.source, day, 14)
	h.tickAt(last.Add(time.Second))
	end := last.Add(20 * time.Hour)
	h.source.add(end)
	h.tickAt(end.Add(time.Second))

	h.runner.results = []runOutcome{{started: false}, goodRun}
	first := end.Add(3 * time.Minute)
	h.tickAt(first)
	pending := h.rec.State().Pending
	if pending == nil || pending.Attempts != 0 || pending.Waiting != "backfill_busy" || pending.NextAttemptMS != first.Add(2*time.Minute).UnixMilli() {
		t.Fatalf("pending after a busy guard = %+v", pending)
	}
	h.tickAt(first.Add(time.Minute))
	if h.runner.callCount() != 1 {
		t.Fatalf("retried before BusyRetry: %d calls", h.runner.callCount())
	}
	h.tickAt(first.Add(2 * time.Minute))
	if done := h.rec.State().History; len(done) != 1 || done[0].Attempts != 1 || done[0].State != StateRecovered {
		t.Fatalf("history = %+v", done)
	}
}

func TestEmptyPullsAreNotCountedAsRecovered(t *testing.T) {
	h := newHarness(t, day)
	last := busyBaseline(h.source, day, 14)
	h.tickAt(last.Add(time.Second))
	end := last.Add(20 * time.Hour)
	h.source.add(end)
	h.tickAt(end.Add(time.Second))

	emptyListing := runOutcome{started: true, result: RunResult{Connected: true, InboxOutcome: InboxEmpty}}
	// The 10/7 shape: INBOX answers with nothing, ARCHIVE still lists a
	// conversation, and a phone backfill elsewhere returns data.
	inboxOnlyEmpty := runOutcome{started: true, result: RunResult{Connected: true, InboxOutcome: InboxNoPayload, Listed: 1, Conversations: 1, Messages: 3, HistoryTeed: 4}}
	h.runner.results = []runOutcome{emptyListing, inboxOnlyEmpty, goodRun}
	at := end.Add(3 * time.Minute)
	h.tickAt(at)
	pending := h.rec.State().Pending
	if pending == nil || pending.LastAttempt == nil || pending.LastAttempt.Outcome != OutcomeEmpty || pending.LastAttempt.InboxOutcome != InboxEmpty {
		t.Fatalf("an empty INBOX listing was not an empty outcome: %+v", pending)
	}
	if pending.NextAttemptMS != at.Add(5*time.Minute).UnixMilli() || pending.Failures != 0 {
		t.Fatalf("pending = %+v, want a 5-minute probe and no failure charged", pending)
	}
	at = at.Add(5 * time.Minute)
	h.tickAt(at)
	pending = h.rec.State().Pending
	if pending == nil || pending.Attempts != 2 || pending.LastAttempt.Outcome != OutcomeEmpty {
		t.Fatalf("a run whose INBOX came back without a payload counted as recovered: %+v", pending)
	}
	h.tickAt(at.Add(15 * time.Minute))
	done := h.rec.State().History
	if len(done) != 1 || done[0].State != StateRecovered || done[0].Attempts != 3 {
		t.Fatalf("history = %+v", done)
	}
}

func TestAbortedRunsKeepTryingUntilTheWindowBound(t *testing.T) {
	h := newHarness(t, day)
	last := busyBaseline(h.source, day, 14)
	h.tickAt(last.Add(time.Second))
	end := last.Add(20 * time.Hour)
	h.source.add(end)
	h.tickAt(end.Add(time.Second))
	aborted := runOutcome{started: true, result: RunResult{Connected: true, Aborted: true, Listed: 4}}
	results := make([]runOutcome, 0, 12)
	for i := 0; i < 11; i++ {
		results = append(results, aborted)
	}
	h.runner.results = append(results, goodRun)
	at := end.Add(3 * time.Minute)
	for i := 0; i < 2000 && h.rec.State().Pending != nil; i++ {
		h.tickAt(at)
		at = at.Add(5 * time.Minute)
	}
	done := h.rec.State().History
	if len(done) != 1 || done[0].State != StateRecovered || done[0].Attempts != 12 || done[0].Failures != 0 {
		t.Fatalf("history = %+v, want recovery on the 12th attempt with no failure charged for reconnects", done)
	}
}

func TestGivesUpWithANoticeAfterTheLastFailedAttempt(t *testing.T) {
	h := newHarness(t, day)
	last := busyBaseline(h.source, day, 14)
	h.tickAt(last.Add(time.Second))
	end := last.Add(20 * time.Hour)
	h.source.add(end)
	h.tickAt(end.Add(time.Second))

	partial := runOutcome{started: true, result: RunResult{Connected: true, Listed: 50, Conversations: 3, Messages: 9, Errors: 2, HistoryTeed: 12}}
	h.runner.results = []runOutcome{partial}
	at := end.Add(3 * time.Minute)
	for i := 0; i < 200 && h.rec.State().Pending != nil; i++ {
		h.tickAt(at)
		at = at.Add(5 * time.Minute)
	}
	maxFailures := len(h.cfg.Backoff) + 1
	if h.runner.callCount() != maxFailures {
		t.Fatalf("calls = %d, want %d", h.runner.callCount(), maxFailures)
	}
	done := h.rec.State().History
	if len(done) != 1 || done[0].State != StateGaveUp || done[0].Failures != maxFailures {
		t.Fatalf("history = %+v", done)
	}
	notice := h.rec.Snapshot().Notice
	if !strings.Contains(notice, "gave up") || !strings.Contains(notice, "/api/backfill") || !strings.Contains(notice, last.Add(-time.Hour).In(newYork).Format(time.RFC3339)) {
		t.Fatalf("notice = %q", notice)
	}
	for i := 0; i < 20; i++ {
		h.tickAt(at.Add(time.Duration(i) * time.Hour))
	}
	if h.runner.callCount() != maxFailures {
		t.Fatal("ran again after giving up")
	}
}

func TestEmptyPullsProbeUntilTheWindowBound(t *testing.T) {
	h := newHarness(t, day)
	last := busyBaseline(h.source, day, 14)
	h.tickAt(last.Add(time.Second))
	end := last.Add(20 * time.Hour)
	h.source.add(end)
	h.tickAt(end.Add(time.Second))

	h.runner.results = []runOutcome{{started: true, result: RunResult{Connected: true}}}
	at := end.Add(3 * time.Minute)
	var waits []time.Duration
	prevNext := int64(0)
	for i := 0; i < 2000 && h.rec.State().Pending != nil; i++ {
		h.tickAt(at)
		if pending := h.rec.State().Pending; pending != nil && pending.NextAttemptMS != prevNext {
			if prevNext != 0 {
				waits = append(waits, time.Duration(pending.NextAttemptMS-at.UnixMilli())*time.Millisecond)
			}
			prevNext = pending.NextAttemptMS
		}
		at = at.Add(5 * time.Minute)
	}
	done := h.rec.State().History
	if len(done) != 1 || done[0].State != StateWindowTooLarge || done[0].Failures != 0 {
		t.Fatalf("history = %+v", done)
	}
	// Probing kept going after the backoff list ran out, at its last step.
	if h.runner.callCount() <= len(h.cfg.Backoff)+1 {
		t.Fatalf("calls = %d: empty attempts gave up like failed ones", h.runner.callCount())
	}
	for _, wait := range waits {
		if wait > h.cfg.Backoff[len(h.cfg.Backoff)-1] {
			t.Fatalf("waited %v between probes, more than the last backoff", wait)
		}
	}
	since := last.Add(-time.Hour)
	if final := done[0].FinishedAtMS; time.UnixMilli(final).Sub(since) <= h.cfg.MaxWindow {
		t.Fatalf("stopped probing at %v, inside the window bound", time.UnixMilli(final))
	}
	notice := h.rec.Snapshot().Notice
	if !strings.Contains(notice, "7-day") || !strings.Contains(notice, OutcomeEmpty) {
		t.Fatalf("notice = %q, want the window bound and the empty last attempt", notice)
	}

	// Once the pull path works again within the bound, a probe recovers.
	h2 := newHarness(t, day)
	last2 := busyBaseline(h2.source, day, 14)
	h2.tickAt(last2.Add(time.Second))
	end2 := last2.Add(20 * time.Hour)
	h2.source.add(end2)
	h2.tickAt(end2.Add(time.Second))
	empty := runOutcome{started: true, result: RunResult{Connected: true}}
	h2.runner.results = []runOutcome{empty, empty, empty, empty, empty, empty, empty, empty, empty, goodRun}
	at = end2.Add(3 * time.Minute)
	for i := 0; i < 2000 && h2.rec.State().Pending != nil; i++ {
		h2.tickAt(at)
		at = at.Add(5 * time.Minute)
	}
	if done := h2.rec.State().History; len(done) != 1 || done[0].State != StateRecovered || done[0].Attempts != 10 {
		t.Fatalf("history = %+v", done)
	}
}

func TestWindowTooLargeFallsBackToANotice(t *testing.T) {
	h := newHarness(t, day)
	last := busyBaseline(h.source, day, 14)
	h.tickAt(last.Add(time.Second))
	end := last.Add(8 * 24 * time.Hour)
	h.source.add(end)
	h.tickAt(end.Add(time.Second))
	h.tickAt(end.Add(3 * time.Minute))
	if h.runner.callCount() != 0 {
		t.Fatal("ran a window backfill longer than MaxWindow")
	}
	done := h.rec.State().History
	if len(done) != 1 || done[0].State != StateWindowTooLarge {
		t.Fatalf("history = %+v", done)
	}
	snap := h.rec.Snapshot()
	if !strings.Contains(snap.Notice, "7-day") || !strings.Contains(snap.Notice, "/api/backfill") {
		t.Fatalf("notice = %q", snap.Notice)
	}
}

func TestWaitsWhileGoogleIsNotReady(t *testing.T) {
	h := newHarness(t, day)
	last := busyBaseline(h.source, day, 14)
	h.tickAt(last.Add(time.Second))
	end := last.Add(20 * time.Hour)
	h.source.add(end)
	h.tickAt(end.Add(time.Second))

	h.runner.ready, h.runner.reason = false, "phone_not_responding"
	for i := 3; i < 60; i += 5 {
		h.tickAt(end.Add(time.Duration(i) * time.Minute))
	}
	if h.runner.callCount() != 0 {
		t.Fatal("ran while Google was not ready")
	}
	pending := h.rec.State().Pending
	if pending == nil || pending.Waiting != "phone_not_responding" || pending.Attempts != 0 {
		t.Fatalf("pending = %+v", pending)
	}
	if saved := h.savedState().Pending; saved == nil || saved.Waiting != "phone_not_responding" {
		t.Fatalf("waiting reason not persisted: %+v", saved)
	}
	h.runner.ready, h.runner.reason = true, ""
	h.tickAt(end.Add(time.Hour))
	if h.runner.callCount() != 1 {
		t.Fatalf("calls = %d after Google became ready", h.runner.callCount())
	}
}

func TestSecondSilenceJoinsThePendingWindow(t *testing.T) {
	h := newHarness(t, day)
	last := busyBaseline(h.source, day, 14)
	h.tickAt(last.Add(time.Second))
	end := last.Add(20 * time.Hour)
	h.source.add(end)
	h.tickAt(end.Add(time.Second))
	h.runner.ready, h.runner.reason = false, "disconnected"

	// While the first window is still owed, a second silence (17h) ends.
	second := end.Add(17 * time.Hour)
	h.source.add(second)
	h.tickAt(second.Add(time.Second))
	pending := h.rec.State().Pending
	if pending == nil || pending.LastEventMS != last.UnixMilli() || len(pending.Covers) != 1 || pending.Covers[0].LastEventMS != end.UnixMilli() {
		t.Fatalf("pending = %+v, want the second silence covered", pending)
	}
	if pending.SinceMS != last.Add(-time.Hour).UnixMilli() {
		t.Fatalf("since moved to %d", pending.SinceMS)
	}
	h.runner.ready = true
	h.tickAt(second.Add(5 * time.Minute))
	if h.runner.callCount() != 1 {
		t.Fatalf("calls = %d, want one run covering both", h.runner.callCount())
	}
	if done := h.rec.State().History; len(done) != 1 || done[0].State != StateRecovered {
		t.Fatalf("history = %+v", done)
	}
}

func TestLiveFlagIsHonouredWhenTheBaselineIsLost(t *testing.T) {
	h := newHarness(t, day)
	last := busyBaseline(h.source, day, 14)
	h.tickAt(last.Add(time.Second))
	h.tickAt(last.Add(15 * time.Hour))
	if h.rec.State().Flagged == nil {
		t.Fatal("not flagged after 15h")
	}

	// From here the baseline query fails, so judging the gap at its end sees
	// no baseline and only the 72h floor applies.
	baselineFrom, _ := freshness.BaselineRange(last, newYork, h.cfg.Silence)
	h.source.betweenErr = func(from, _ time.Time) error {
		if !from.After(baselineFrom) {
			return errors.New("database is locked")
		}
		return nil
	}
	h.rec.baselines = map[int64][]time.Time{}
	h.restart()
	end := last.Add(20 * time.Hour)
	h.source.add(end)
	h.tickAt(end.Add(time.Second))
	pending := h.rec.State().Pending
	if pending == nil || pending.LastEventMS != last.UnixMilli() || pending.Rule != freshness.RuleExpectedActivity {
		t.Fatalf("the live flag was not honoured: %+v", pending)
	}
}

func TestActivityErrorsAreReportedAndRetried(t *testing.T) {
	h := newHarness(t, day)
	last := busyBaseline(h.source, day, 14)
	h.tickAt(last.Add(time.Second))
	h.source.latestErr = errors.New("disk I/O error")
	h.tickAt(last.Add(time.Hour))
	if got := h.rec.Snapshot().ActivityError; got != "disk I/O error" {
		t.Fatalf("activity error = %q", got)
	}
	h.source.latestErr = nil
	h.tickAt(last.Add(2 * time.Hour)) // still quiet, but the query works again
	if got := h.rec.Snapshot().ActivityError; got != "" {
		t.Fatalf("activity error kept after a good query: %q", got)
	}
	end := last.Add(20 * time.Hour)
	h.source.add(end)
	h.tickAt(end.Add(time.Second))
	snap := h.rec.Snapshot()
	if snap.ActivityError != "" || snap.Pending == nil {
		t.Fatalf("snapshot after recovery of the source = %+v", snap)
	}
}

func TestCorruptStateFileIsMovedAsideAndReported(t *testing.T) {
	h := newHarness(t, day)
	if err := os.WriteFile(h.path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.restart()
	if h.rec.Snapshot().StateLoadError == "" {
		t.Fatal("a corrupt state file was not reported")
	}
	backups, _ := filepath.Glob(h.path + ".unreadable-*")
	if len(backups) != 1 {
		t.Fatalf("backups = %v, want one", backups)
	}
	if kept, err := os.ReadFile(backups[0]); err != nil || string(kept) != "{not json" {
		t.Fatalf("the unreadable file was not kept aside: %q, %v", kept, err)
	}
	last := busyBaseline(h.source, day, 14)
	h.tickAt(last.Add(time.Second))
	if state := h.savedState(); state.WatermarkMS != last.UnixMilli() {
		t.Fatalf("state not rewritten: %+v", state)
	}
	snap := h.rec.Snapshot()
	if snap.StateError != "" || snap.StateLoadError == "" {
		t.Fatalf("after a good save: state_error %q, state_load_error %q; want the load error kept", snap.StateError, snap.StateLoadError)
	}
}

func TestNewerStateVersionSurvivesADowngrade(t *testing.T) {
	h := newHarness(t, day)
	newer := []byte(`{"version": 99, "watermark_ms": 5}`)
	if err := os.WriteFile(h.path, newer, 0o600); err != nil {
		t.Fatal(err)
	}
	h.restart()
	if !strings.Contains(h.rec.Snapshot().StateLoadError, "version 99") {
		t.Fatalf("load error = %q", h.rec.Snapshot().StateLoadError)
	}
	backups, _ := filepath.Glob(h.path + ".unreadable-*")
	if len(backups) != 1 {
		t.Fatalf("backups = %v", backups)
	}
	if kept, _ := os.ReadFile(backups[0]); string(kept) != string(newer) {
		t.Fatalf("newer state not kept aside: %q", kept)
	}
	// A second unreadable file never overwrites the first backup.
	if err := os.WriteFile(h.path, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.restart()
	backups, _ = filepath.Glob(h.path + ".unreadable-*")
	if len(backups) != 2 {
		t.Fatalf("backups after a second bad file = %v, want two", backups)
	}
	contents := map[string]bool{}
	for _, backup := range backups {
		data, _ := os.ReadFile(backup)
		contents[string(data)] = true
	}
	if !contents[string(newer)] || !contents["garbage"] {
		t.Fatalf("backups hold %v", contents)
	}
}

func TestUnreadableStateThatCantBePreservedIsNeverOverwritten(t *testing.T) {
	h := newHarness(t, day)
	if err := os.WriteFile(h.path, []byte("{bad"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(h.path)
	// A read-only directory: the file can be neither moved nor copied aside.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	h.restart()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.rec.Snapshot().StateLoadError, "nothing is saved") {
		t.Fatalf("load error = %q", h.rec.Snapshot().StateLoadError)
	}
	last := busyBaseline(h.source, day, 14)
	h.tickAt(last.Add(time.Second))
	h.tickAt(last.Add(time.Hour))
	if data, _ := os.ReadFile(h.path); string(data) != "{bad" {
		t.Fatalf("the unreadable original was overwritten: %q", data)
	}
}

func TestFailedSaveIsRetried(t *testing.T) {
	h := newHarness(t, day)
	last := busyBaseline(h.source, day, 14)
	// The state file's directory disappears: every save fails.
	dir := filepath.Dir(h.path)
	h.path = filepath.Join(dir, "gone", StateFileName)
	h.restart()
	h.tickAt(last.Add(time.Second))
	if h.rec.Snapshot().StateError == "" {
		t.Fatal("a failed save was not reported")
	}
	if err := os.MkdirAll(filepath.Dir(h.path), 0o700); err != nil {
		t.Fatal(err)
	}
	// Nothing new happens, yet the next tick saves.
	h.tickAt(last.Add(time.Minute))
	if h.rec.Snapshot().StateError != "" {
		t.Fatalf("still failing: %q", h.rec.Snapshot().StateError)
	}
	if got := h.savedState().WatermarkMS; got != last.UnixMilli() {
		t.Fatalf("saved watermark = %d", got)
	}
}

func TestStateFileIsPrivateAndAtomic(t *testing.T) {
	h := newHarness(t, day)
	last := busyBaseline(h.source, day, 14)
	h.tickAt(last.Add(time.Second))
	info, err := os.Stat(h.path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("state file mode = %v, want 0600", perm)
	}
	entries, err := os.ReadDir(filepath.Dir(h.path))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".tmp") {
			t.Fatalf("temporary file left behind: %s", entry.Name())
		}
	}
	data, err := os.ReadFile(h.path)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil || decoded["version"] != float64(stateVersion) {
		t.Fatalf("state file = %s (%v)", data, err)
	}
}

func TestWatermarkPersistsLazily(t *testing.T) {
	h := newHarness(t, day)
	last := busyBaseline(h.source, day, 14)
	h.tickAt(last.Add(time.Second))
	next := last.Add(time.Minute)
	h.source.add(next)
	h.tickAt(next.Add(time.Second))
	if got := h.savedState().WatermarkMS; got != last.UnixMilli() {
		t.Fatalf("watermark saved again within PersistEvery: %d", got)
	}
	h.tickAt(next.Add(h.cfg.PersistEvery + time.Second))
	if got := h.savedState().WatermarkMS; got != next.UnixMilli() {
		t.Fatalf("watermark = %d after PersistEvery, want %d", got, next.UnixMilli())
	}
}

func TestClassify(t *testing.T) {
	good := RunResult{Connected: true, Listed: 10, Conversations: 2, Messages: 5, HistoryTeed: 7, InboxOutcome: InboxOK}
	cases := []struct {
		name      string
		result    RunResult
		requireV2 bool
		want      string
	}{
		{"recovered", good, true, OutcomeRecovered},
		{"recovered without an inbox record", RunResult{Connected: true, Listed: 10, Conversations: 2, Messages: 5, HistoryTeed: 7}, true, OutcomeRecovered},
		{"aborted wins", RunResult{Connected: true, Aborted: true, Listed: 10, InboxOutcome: InboxEmpty}, false, OutcomeAborted},
		{"inbox empty", RunResult{Connected: true, InboxOutcome: InboxEmpty}, false, OutcomeEmpty},
		{"inbox empty, nothing listed anywhere", RunResult{Connected: true, InboxOutcome: InboxEmpty}, false, OutcomeEmpty},
		{"inbox empty while archive lists data", RunResult{Connected: true, Listed: 3, Conversations: 1, Messages: 4, HistoryTeed: 5, InboxOutcome: InboxEmpty}, false, OutcomePartial},
		{"inbox empty and the other listings failed", RunResult{Connected: true, Errors: 2, InboxOutcome: InboxEmpty}, false, OutcomeEmpty},
		{"inbox empty, archive listed, a fetch failed", RunResult{Connected: true, Listed: 3, Conversations: 1, Messages: 2, Errors: 1, HistoryTeed: 3, InboxOutcome: InboxEmpty}, false, OutcomePartial},
		{"inbox without payload", RunResult{Connected: true, Listed: 3, InboxOutcome: InboxNoPayload}, false, OutcomeEmpty},
		{"inbox listing failed", RunResult{Connected: true, Errors: 3, InboxOutcome: InboxError}, false, OutcomePartial},
		{"inbox timed out but archive listed", RunResult{Connected: true, Listed: 4, Conversations: 1, Messages: 2, Errors: 1, HistoryTeed: 3, InboxOutcome: InboxError}, false, OutcomePartial},
		{"nothing listed", RunResult{Connected: true}, false, OutcomeEmpty},
		{"listings failed", RunResult{Connected: true, Errors: 3}, false, OutcomePartial},
		{"every in-window fetch empty", RunResult{Connected: true, Listed: 10, Conversations: 3, InboxOutcome: InboxOK}, false, OutcomeEmpty},
		{"every in-window fetch failed", RunResult{Connected: true, Listed: 10, Conversations: 3, Errors: 3, InboxOutcome: InboxOK}, false, OutcomePartial},
		{"one in-window fetch empty", RunResult{Connected: true, Listed: 40, Conversations: 5, Messages: 60, EmptyConversations: 1, HistoryTeed: 65, InboxOutcome: InboxOK}, false, OutcomePartial},
		{"fetch errors", RunResult{Connected: true, Listed: 10, Conversations: 2, Messages: 3, Errors: 1, HistoryTeed: 5, InboxOutcome: InboxOK}, false, OutcomePartial},
		{"tee failures when readers use v2", RunResult{Connected: true, Listed: 10, Conversations: 2, Messages: 3, HistoryTeed: 4, HistoryTeeFailed: 1, InboxOutcome: InboxOK}, true, OutcomePartial},
		{"tee failures when readers use the legacy store", RunResult{Connected: true, Listed: 10, Conversations: 2, Messages: 3, HistoryTeed: 4, HistoryTeeFailed: 1, InboxOutcome: InboxOK}, false, OutcomeRecovered},
		{"v2 missing", RunResult{Connected: true, Listed: 10, Conversations: 2, Messages: 3, InboxOutcome: InboxOK}, true, OutcomePartial},
		{"v2 not required", RunResult{Connected: true, Listed: 10, Conversations: 2, Messages: 3, InboxOutcome: InboxOK}, false, OutcomeRecovered},
		{"nothing in window", RunResult{Connected: true, Listed: 10, InboxOutcome: InboxOK}, true, OutcomeNothingInWindow},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, detail := Classify(tc.result, tc.requireV2)
			if got != tc.want {
				t.Fatalf("Classify = %q (%s), want %q", got, detail, tc.want)
			}
			if detail == "" {
				t.Fatal("no detail")
			}
		})
	}
}

func TestMinStallDurationDefault(t *testing.T) {
	if got := MinStallDuration(freshness.DefaultSilenceConfig); got != 6*time.Hour {
		t.Fatalf("MinStallDuration(default) = %v, want 6h", got)
	}
	if got := MinStallDuration(freshness.SilenceConfig{}); got != time.Duration(1<<63-1) {
		t.Fatalf("no rules = %v, want MaxInt64", got)
	}
	if got := MinStallDuration(freshness.SilenceConfig{ExpectedActiveHoursLimit: 10, MaxSilence: 3 * time.Hour, LongSilence: time.Hour}); got != time.Hour {
		t.Fatalf("got %v, want the smallest rule", got)
	}
}

func TestPanickingRunIsRetriedWithoutARestart(t *testing.T) {
	h := newHarness(t, day)
	last := busyBaseline(h.source, day, 14)
	h.tickAt(last.Add(time.Second))
	end := last.Add(20 * time.Hour)
	h.source.add(end)
	h.tickAt(end.Add(time.Second))

	h.runner.onRun = func(time.Time, bool) { panic("boom") }
	h.clock = end.Add(3 * time.Minute)
	h.rec.safeTick(context.Background()) // as Start's loop does
	pending := h.rec.State().Pending
	if pending == nil || pending.State != StatePending || pending.Attempts != 1 || pending.Failures != 1 || pending.LastAttempt == nil || pending.LastAttempt.Outcome != OutcomeCrashed {
		t.Fatalf("pending after a panicking run = %+v", pending)
	}
	h.runner.onRun = nil
	h.tickAt(end.Add(3*time.Minute + h.cfg.Backoff[0]))
	if done := h.rec.State().History; len(done) != 1 || done[0].State != StateRecovered || done[0].Attempts != 2 {
		t.Fatalf("history = %+v", done)
	}
}

func TestFailedBaselineReadKeepsTheGapUntilItCanBeJudged(t *testing.T) {
	h := newHarness(t, day)
	last := busyBaseline(h.source, day, 14)
	h.tickAt(last.Add(time.Second))

	// The silence ends while the daemon is down, so nothing flagged it live,
	// and the baseline reads after the restart fail 40 times in a row.
	end := last.Add(40 * time.Hour)
	h.source.add(end, end.Add(time.Minute))
	baselineFrom, _ := freshness.BaselineRange(last, newYork, h.cfg.Silence)
	failures := 40
	h.source.betweenErr = func(from, _ time.Time) error {
		if from.Equal(baselineFrom) && failures > 0 {
			failures--
			return errors.New("database is locked")
		}
		return nil
	}
	h.clock = end.Add(90 * time.Second)
	h.restart()
	h.tickAt(end.Add(90 * time.Second))
	state := h.rec.State()
	if state.Pending != nil || state.WatermarkMS != end.Add(time.Minute).UnixMilli() || len(state.Unjudged) != 1 || state.Unjudged[0].LastEventMS != last.UnixMilli() {
		t.Fatalf("after a failed baseline read: %+v; want the gap kept unjudged and the scan moved on", state)
	}
	if saved := h.savedState(); len(saved.Unjudged) != 1 {
		t.Fatalf("the unjudged gap was not saved at once: %+v", saved.Unjudged)
	}
	if !strings.Contains(h.rec.Snapshot().BaselineError, "database is locked") {
		t.Fatalf("baseline failure not reported: %q", h.rec.Snapshot().BaselineError)
	}
	at := end.Add(2 * time.Minute)
	for i := 0; i < 60 && h.rec.State().Pending == nil && len(h.rec.State().History) == 0; i++ {
		h.tickAt(at)
		at = at.Add(time.Minute)
		if i == 20 {
			h.restart() // survives restarts too
		}
	}
	state = h.rec.State()
	owner := state.Pending
	if owner == nil && len(state.History) > 0 {
		owner = &state.History[0]
	}
	if owner == nil || owner.LastEventMS != last.UnixMilli() || len(state.Unjudged) != 0 {
		t.Fatalf("the gap was not judged once its baseline came back: %+v", state)
	}
	if got := h.rec.Snapshot().BaselineError; got != "" {
		t.Fatalf("baseline_error kept after a good read: %q", got)
	}
}

func TestGapThatCanNeverBeJudgedEndsWithANotice(t *testing.T) {
	h := newHarness(t, day)
	last := busyBaseline(h.source, day, 14)
	h.tickAt(last.Add(time.Second))
	end := last.Add(40 * time.Hour)
	h.source.add(end)
	baselineFrom, _ := freshness.BaselineRange(last, newYork, h.cfg.Silence)
	h.source.betweenErr = func(from, _ time.Time) error {
		if from.Equal(baselineFrom) {
			return errors.New("disk I/O error")
		}
		return nil
	}
	at := end.Add(time.Second)
	for at.Before(last.Add(7*24*time.Hour + time.Hour)) {
		h.tickAt(at)
		at = at.Add(30 * time.Minute)
	}
	state := h.rec.State()
	if len(state.Unjudged) != 0 || len(state.History) != 1 || state.History[0].State != StateUnjudged {
		t.Fatalf("state = %+v, want the gap finished as unjudged", state)
	}
	if h.runner.callCount() != 0 {
		t.Fatal("fetched a window nobody judged")
	}
	notice := h.rec.Snapshot().Notice
	if !strings.Contains(notice, "could not be judged") || !strings.Contains(notice, "disk I/O error") || !strings.Contains(notice, "/api/backfill") {
		t.Fatalf("notice = %q", notice)
	}
}

func TestQuietBaselineErrorsAreReported(t *testing.T) {
	h := newHarness(t, day)
	last := busyBaseline(h.source, day, 14)
	h.tickAt(last.Add(time.Second))
	h.source.betweenErr = func(time.Time, time.Time) error { return errors.New("database is locked") }
	h.tickAt(last.Add(20 * time.Hour))
	snap := h.rec.Snapshot()
	if !strings.Contains(snap.BaselineError, "database is locked") {
		t.Fatalf("baseline error during a quiet silence = %q", snap.BaselineError)
	}
	if snap.Flagged != nil {
		t.Fatalf("flagged without a baseline: %+v", snap.Flagged)
	}
}

func TestShortSpansSkipTheEventQuery(t *testing.T) {
	h := newHarness(t, day)
	last := busyBaseline(h.source, day, 14)
	h.tickAt(last.Add(time.Second))
	before := h.source.betweenCalls
	at := last
	for i := 0; i < 60; i++ {
		at = at.Add(7 * time.Minute)
		h.source.add(at)
		h.tickAt(at.Add(time.Second))
	}
	if got := h.source.betweenCalls - before; got != 0 {
		t.Fatalf("%d event queries for an hour of ordinary traffic; want none", got)
	}
	if h.rec.State().WatermarkMS != at.UnixMilli() {
		t.Fatal("the watermark did not follow the traffic")
	}
}

func TestCoveredSilenceIsOwedAgainWhenTheWindowIsTooLarge(t *testing.T) {
	h := newHarness(t, day)
	last := busyBaseline(h.source, day, 14)
	h.tickAt(last.Add(time.Second))
	end := last.Add(20 * time.Hour)
	h.source.add(end)
	h.runner.ready, h.runner.reason = false, "needs_repair"
	h.tickAt(end.Add(time.Second))

	// Days of traffic while Google can't serve a fetch, then a second
	// stalled silence (20h) that ends inside the first one's 7 days.
	at := end
	for at.Before(last.Add(5 * 24 * time.Hour)) {
		at = at.Add(30 * time.Minute)
		h.source.add(at)
		h.tickAt(at.Add(time.Second))
	}
	secondLast := at
	secondEnd := secondLast.Add(20 * time.Hour)
	h.source.add(secondEnd)
	h.tickAt(secondEnd.Add(time.Second))
	if pending := h.rec.State().Pending; pending == nil || len(pending.Covers) != 1 || pending.Covers[0].LastEventMS != secondLast.UnixMilli() {
		t.Fatalf("pending = %+v, want the second silence covered", pending)
	}
	// The first window outgrows 7 days while Google is still not ready.
	at = secondEnd
	for at.Before(last.Add(7*24*time.Hour + 2*time.Hour)) {
		at = at.Add(30 * time.Minute)
		h.source.add(at)
		h.tickAt(at.Add(time.Second))
	}
	state := h.rec.State()
	if len(state.History) != 1 || state.History[0].State != StateWindowTooLarge {
		t.Fatalf("history = %+v, want the first silence finished as too large", state.History)
	}
	if state.Pending == nil || state.Pending.LastEventMS != secondLast.UnixMilli() {
		t.Fatalf("pending = %+v, want the covered silence owed again", state.Pending)
	}
	h.runner.ready, h.runner.reason = true, ""
	h.tickAt(at.Add(5 * time.Minute))
	if h.runner.callCount() != 1 || !h.runner.calls[0].Equal(secondLast.Add(-time.Hour)) {
		t.Fatalf("calls = %v, want one from the second silence's own window", h.runner.calls)
	}
}

func TestWindowBoundAppliesAtRunTime(t *testing.T) {
	h := newHarness(t, day)
	last := busyBaseline(h.source, day, 14)
	h.tickAt(last.Add(time.Second))
	end := last.Add(20 * time.Hour)
	h.source.add(end)
	h.tickAt(end.Add(time.Second))
	h.runner.ready, h.runner.reason = false, "disconnected"
	h.tickAt(last.Add(6 * 24 * time.Hour))
	if h.rec.State().Pending == nil {
		t.Fatal("finished before the window passed 7 days")
	}
	h.runner.ready, h.runner.reason = true, ""
	h.tickAt(last.Add(7 * 24 * time.Hour)) // since is last-1h, so this is past 7 days
	if h.runner.callCount() != 0 {
		t.Fatal("ran a window that had grown past 7 days while waiting")
	}
	if done := h.rec.State().History; len(done) != 1 || done[0].State != StateWindowTooLarge {
		t.Fatalf("history = %+v", done)
	}
}

func TestNoClientIsNotAnAttempt(t *testing.T) {
	h := newHarness(t, day)
	last := busyBaseline(h.source, day, 14)
	h.tickAt(last.Add(time.Second))
	end := last.Add(20 * time.Hour)
	h.source.add(end)
	h.tickAt(end.Add(time.Second))
	h.runner.results = []runOutcome{{started: true, result: RunResult{Connected: false}}, goodRun}
	at := end.Add(3 * time.Minute)
	h.tickAt(at)
	pending := h.rec.State().Pending
	if pending == nil || pending.Attempts != 0 || pending.Waiting != "disconnected" || pending.NextAttemptMS != at.Add(h.cfg.BusyRetry).UnixMilli() {
		t.Fatalf("pending after a run with no client = %+v", pending)
	}
	h.tickAt(at.Add(h.cfg.BusyRetry))
	if done := h.rec.State().History; len(done) != 1 || done[0].Attempts != 1 {
		t.Fatalf("history = %+v", done)
	}
}

func TestNothingInWindowCountsAsRecovered(t *testing.T) {
	h := newHarness(t, day)
	last := busyBaseline(h.source, day, 14)
	h.tickAt(last.Add(time.Second))
	end := last.Add(20 * time.Hour)
	h.source.add(end)
	h.tickAt(end.Add(time.Second))
	h.runner.results = []runOutcome{{started: true, result: RunResult{Connected: true, Listed: 30}}}
	h.tickAt(end.Add(3 * time.Minute))
	done := h.rec.State().History
	if len(done) != 1 || done[0].State != StateRecovered || done[0].LastAttempt.Outcome != OutcomeNothingInWindow {
		t.Fatalf("history = %+v", done)
	}
	h.tickAt(end.Add(time.Hour))
	if h.runner.callCount() != 1 {
		t.Fatalf("calls = %d", h.runner.callCount())
	}
}

func TestNoticeOutlivesALaterRecoveryUntilItExpires(t *testing.T) {
	h := newHarness(t, day)
	last := busyBaseline(h.source, day, 14)
	h.tickAt(last.Add(time.Second))
	end := last.Add(8 * 24 * time.Hour)
	h.source.add(end)
	h.tickAt(end.Add(time.Second))
	h.tickAt(end.Add(3 * time.Minute))
	tooLarge := h.rec.Snapshot().Notice
	if tooLarge == "" {
		t.Fatal("no notice for a window over 7 days")
	}
	// Three days of traffic, then an ordinary stall that recovers.
	at := end
	for at.Before(end.Add(3 * 24 * time.Hour)) {
		at = at.Add(30 * time.Minute)
		h.source.add(at)
		h.tickAt(at.Add(time.Second))
	}
	second := at.Add(20 * time.Hour)
	h.source.add(second)
	h.tickAt(second.Add(time.Second))
	h.tickAt(second.Add(3 * time.Minute))
	snap := h.rec.Snapshot()
	if snap.Last == nil || snap.Last.State != StateRecovered {
		t.Fatalf("last = %+v", snap.Last)
	}
	if snap.Notice != tooLarge || len(snap.Notices) != 1 {
		t.Fatalf("notice after a later recovery = %q (%d), want the unresolved one kept", snap.Notice, len(snap.Notices))
	}
	h.clock = end.Add(NoticeTTL + time.Hour)
	if got := h.rec.Snapshot().Notice; got != "" {
		t.Fatalf("notice kept past NoticeTTL: %q", got)
	}
}

func TestHistoryKeepsTheNewestEpisodes(t *testing.T) {
	h := newHarness(t, day)
	h.cfg.HistoryLimit = 2
	h.restart()
	last := busyBaseline(h.source, day, 14)
	h.tickAt(last.Add(time.Second))
	var lasts []int64
	at := last
	for i := 0; i < 3; i++ {
		lasts = append(lasts, at.UnixMilli())
		end := at.Add(20 * time.Hour)
		h.source.add(end)
		h.tickAt(end.Add(time.Second))
		h.tickAt(end.Add(3 * time.Minute))
		at = end
	}
	done := h.rec.State().History
	if len(done) != 2 || done[0].LastEventMS != lasts[2] || done[1].LastEventMS != lasts[1] {
		t.Fatalf("history = %+v, want the two newest, newest first", done)
	}
}

func TestMergePullsTheNextAttemptIn(t *testing.T) {
	// With the default backoffs (4h at most) no stalled silence (6h at
	// least) can end before the next attempt; a longer backoff can.
	h := newHarness(t, day)
	h.cfg.Backoff = []time.Duration{24 * time.Hour}
	h.restart()
	last := busyBaseline(h.source, day, 14)
	h.tickAt(last.Add(time.Second))
	end := last.Add(20 * time.Hour)
	h.source.add(end)
	h.tickAt(end.Add(time.Second))
	partial := runOutcome{started: true, result: RunResult{Connected: true, Listed: 9, Conversations: 1, Messages: 2, Errors: 1, HistoryTeed: 3}}
	h.runner.results = []runOutcome{partial, goodRun}
	at := end.Add(3 * time.Minute)
	h.tickAt(at)
	if pending := h.rec.State().Pending; pending == nil || pending.NextAttemptMS != at.Add(24*time.Hour).UnixMilli() {
		t.Fatalf("pending after a partial run = %+v", pending)
	}
	for i := 0; i < 3; i++ {
		at = at.Add(10 * time.Minute)
		h.source.add(at)
		h.tickAt(at.Add(time.Second))
	}
	secondEnd := at.Add(17 * time.Hour)
	h.source.add(secondEnd)
	h.tickAt(secondEnd.Add(time.Second))
	pending := h.rec.State().Pending
	if pending == nil || len(pending.Covers) != 1 || pending.NextAttemptMS != secondEnd.Add(h.cfg.Settle).UnixMilli() {
		t.Fatalf("pending = %+v, want the next attempt pulled in to the new silence's settle", pending)
	}
	h.tickAt(secondEnd.Add(h.cfg.Settle))
	if done := h.rec.State().History; len(done) != 1 || done[0].State != StateRecovered {
		t.Fatalf("history = %+v", done)
	}

	// An attempt already due earlier stays put.
	h2 := newHarness(t, day)
	last2 := busyBaseline(h2.source, day, 14)
	h2.tickAt(last2.Add(time.Second))
	end2 := last2.Add(20 * time.Hour)
	h2.runner.ready, h2.runner.reason = false, "disconnected"
	h2.source.add(end2)
	h2.tickAt(end2.Add(time.Second))
	due := h2.rec.State().Pending.NextAttemptMS
	at2 := end2
	for i := 0; i < 3; i++ {
		at2 = at2.Add(10 * time.Minute)
		h2.source.add(at2)
		h2.tickAt(at2.Add(time.Second))
	}
	second2 := at2.Add(17 * time.Hour)
	h2.source.add(second2)
	h2.tickAt(second2.Add(time.Second))
	// The overdue attempt now waits for the new silence to settle too.
	if got := h2.rec.State().Pending.NextAttemptMS; got <= due || got != second2.Add(h2.cfg.Settle).UnixMilli() {
		t.Fatalf("next attempt %d after a merge, want the newer silence's settle %d", got, second2.Add(h2.cfg.Settle).UnixMilli())
	}
}

// steadyAllDay adds an event every 30 minutes, every hour of the day, for the
// 15 days up to and including last, so every clock hour is active on every
// baseline day and the expected-activity score equals the silent hours.
func steadyAllDay(source *fakeSource, last time.Time) {
	var events []time.Time
	for at := last.Add(-15 * 24 * time.Hour); !at.After(last); at = at.Add(30 * time.Minute) {
		events = append(events, at)
	}
	source.add(events...)
}

func TestStallBoundaryIsExact(t *testing.T) {
	for _, tc := range []struct {
		gap  time.Duration
		want bool
	}{
		{6 * time.Hour, true},
		{6*time.Hour - time.Millisecond, false},
	} {
		h := newHarness(t, day)
		last := time.Date(2026, 10, 5, 10, 0, 0, 0, newYork)
		steadyAllDay(h.source, last)
		h.tickAt(last.Add(time.Second))
		end := last.Add(tc.gap)
		h.source.add(end)
		h.tickAt(end.Add(time.Second))
		if got := h.rec.State().Pending != nil; got != tc.want {
			t.Fatalf("gap %v: episode = %v, want %v", tc.gap, got, tc.want)
		}
	}
}

func TestQuietFlagBoundaryIsExact(t *testing.T) {
	for _, tc := range []struct {
		quiet time.Duration
		want  bool
	}{
		{6 * time.Hour, true},
		{6*time.Hour - time.Millisecond, false},
	} {
		h := newHarness(t, day)
		last := time.Date(2026, 10, 5, 10, 0, 0, 0, newYork)
		steadyAllDay(h.source, last)
		h.tickAt(last.Add(time.Second))
		h.tickAt(last.Add(tc.quiet))
		if got := h.rec.State().Flagged != nil; got != tc.want {
			t.Fatalf("quiet %v: flagged = %v, want %v", tc.quiet, got, tc.want)
		}
	}
}

func TestLateDetectionSchedulesTheFirstAttemptNow(t *testing.T) {
	h := newHarness(t, day)
	last := busyBaseline(h.source, day, 14)
	h.tickAt(last.Add(time.Second))
	end := last.Add(20 * time.Hour)
	h.source.add(end)
	h.runner.ready, h.runner.reason = false, "disconnected"
	detected := end.Add(time.Hour) // the daemon was down when it ended
	h.clock = detected
	h.restart()
	h.tickAt(detected)
	if pending := h.rec.State().Pending; pending == nil || pending.NextAttemptMS != detected.UnixMilli() {
		t.Fatalf("pending = %+v, want the first attempt due at detection, not in the past", pending)
	}
}

func TestRequireV2MakesAMissingHandOffPartial(t *testing.T) {
	h := newHarness(t, day)
	h.cfg.RequireV2 = true
	h.restart()
	last := busyBaseline(h.source, day, 14)
	h.tickAt(last.Add(time.Second))
	end := last.Add(20 * time.Hour)
	h.source.add(end)
	h.tickAt(end.Add(time.Second))
	legacyOnly := runOutcome{started: true, result: RunResult{Connected: true, Listed: 20, Conversations: 2, Messages: 6, InboxOutcome: InboxOK}}
	h.runner.results = []runOutcome{legacyOnly, goodRun}
	h.tickAt(end.Add(3 * time.Minute))
	pending := h.rec.State().Pending
	if pending == nil || pending.LastAttempt.Outcome != OutcomePartial || pending.Failures != 1 {
		t.Fatalf("pending = %+v, want a partial attempt: nothing reached v2", pending)
	}
}

func TestWindowBoundIsInclusive(t *testing.T) {
	for _, tc := range []struct {
		wait time.Duration // from the window start to the attempt
		run  bool
	}{
		{7 * 24 * time.Hour, true},
		{7*24*time.Hour + time.Millisecond, false},
	} {
		h := newHarness(t, day)
		last := busyBaseline(h.source, day, 14)
		h.tickAt(last.Add(time.Second))
		end := last.Add(20 * time.Hour)
		h.source.add(end)
		h.runner.ready, h.runner.reason = false, "disconnected"
		h.tickAt(end.Add(time.Second))
		h.runner.ready, h.runner.reason = true, ""
		since := last.Add(-h.cfg.Margin)
		h.tickAt(since.Add(tc.wait))
		if got := h.runner.callCount() == 1; got != tc.run {
			t.Fatalf("attempt %v after the window start: ran = %v, want %v", tc.wait, got, tc.run)
		}
	}
}

func TestNoticesSurviveAHistoryBurst(t *testing.T) {
	h := newHarness(t, day)
	h.cfg.HistoryLimit = 3
	h.restart()
	last := busyBaseline(h.source, day, 14)
	h.tickAt(last.Add(time.Second))
	end := last.Add(8 * 24 * time.Hour)
	h.source.add(end)
	h.tickAt(end.Add(time.Second))
	h.tickAt(end.Add(3 * time.Minute))
	tooLarge := h.rec.Snapshot().Notice
	if tooLarge == "" {
		t.Fatal("no notice")
	}
	// Five ordinary stalls recover afterwards, more than HistoryLimit.
	at := end
	for i := 0; i < 5; i++ {
		next := at.Add(20 * time.Hour)
		h.source.add(next)
		h.tickAt(next.Add(time.Second))
		h.tickAt(next.Add(3 * time.Minute))
		at = next
	}
	state := h.rec.State()
	if len(state.History) != 4 {
		t.Fatalf("history has %d episodes, want 3 plus the one with a live notice", len(state.History))
	}
	if got := h.rec.Snapshot().Notice; got != tooLarge {
		t.Fatalf("notice after a burst of later episodes = %q, want it kept", got)
	}
}

func TestEarlierLateJudgedGapExtendsThePendingWindow(t *testing.T) {
	h := newHarness(t, day)
	last := busyBaseline(h.source, day, 14)
	h.tickAt(last.Add(time.Second))
	endA := last.Add(20 * time.Hour)
	endB := endA.Add(20 * time.Hour)
	h.source.add(endA, endB)
	fromA, _ := freshness.BaselineRange(last, newYork, h.cfg.Silence)
	failOnce := true
	h.source.betweenErr = func(from, _ time.Time) error {
		if from.Equal(fromA) && failOnce {
			failOnce = false
			return errors.New("database is locked")
		}
		return nil
	}
	h.tickAt(endB.Add(30 * time.Second)) // B pending, A unjudged
	if pending := h.rec.State().Pending; pending == nil || pending.LastEventMS != endA.UnixMilli() {
		t.Fatalf("pending = %+v, want B (the silence after A's end)", pending)
	}
	h.tickAt(endB.Add(90 * time.Second)) // A judged stalled: it takes over
	pending := h.rec.State().Pending
	if pending == nil || pending.LastEventMS != last.UnixMilli() || pending.SinceMS != last.Add(-h.cfg.Margin).UnixMilli() ||
		len(pending.Covers) != 1 || pending.Covers[0].LastEventMS != endA.UnixMilli() {
		t.Fatalf("pending = %+v, want A owning the window with B covered", pending)
	}
	if pending.EndedAtMS != endA.UnixMilli() || pending.SilentMS != endA.Sub(last).Milliseconds() || pending.Rule == "" {
		t.Fatalf("pending = %+v, want A's own end, length and rule", pending)
	}
	if pending.NextAttemptMS < endB.Add(h.cfg.Settle).UnixMilli() {
		t.Fatalf("next attempt %d before B settled", pending.NextAttemptMS)
	}
	h.tickAt(endB.Add(3 * time.Minute))
	if h.runner.callCount() != 1 || h.runner.calls[0].After(last.Add(-h.cfg.Margin)) {
		t.Fatalf("calls = %v, want one run from %v or earlier", h.runner.calls, last.Add(-h.cfg.Margin))
	}
}

func TestLateJudgedGapJoinsAnEpisodeWaitingForGoogle(t *testing.T) {
	h := newHarness(t, day)
	last := busyBaseline(h.source, day, 14)
	h.tickAt(last.Add(time.Second))
	endA := last.Add(20 * time.Hour)
	endB := endA.Add(20 * time.Hour)
	h.source.add(endA, endB)
	fromA, _ := freshness.BaselineRange(last, newYork, h.cfg.Silence)
	failing := true
	h.source.betweenErr = func(from, _ time.Time) error {
		if from.Equal(fromA) && failing {
			return errors.New("database is locked")
		}
		return nil
	}
	h.runner.ready, h.runner.reason = false, "disconnected"
	at := endB.Add(time.Minute)
	for i := 0; i < 24*60/30; i++ { // a day of failures while B waits
		h.tickAt(at)
		at = at.Add(30 * time.Minute)
	}
	failing = false
	h.tickAt(at)
	h.runner.ready, h.runner.reason = true, ""
	h.tickAt(at.Add(time.Minute))
	if h.runner.callCount() != 1 || h.runner.calls[0].After(last.Add(-h.cfg.Margin)) {
		t.Fatalf("calls = %v, want one run reaching back to %v", h.runner.calls, last.Add(-h.cfg.Margin))
	}
	done := h.rec.State().History
	if len(done) != 1 || done[0].LastEventMS != last.UnixMilli() || done[0].State != StateRecovered {
		t.Fatalf("history = %+v", done)
	}
}

func TestUnjudgedRetriesRotate(t *testing.T) {
	h := newHarness(t, day)
	last := time.Date(2026, 10, 5, 10, 0, 0, 0, newYork)
	steadyAllDay(h.source, last)
	h.tickAt(last.Add(time.Second))
	// Five 25-hour gaps in a row, each starting on its own local day so each
	// has its own baseline; the first four keep failing their baseline.
	var lasts []time.Time
	at := last
	for i := 0; i < 5; i++ {
		lasts = append(lasts, at)
		at = at.Add(25 * time.Hour)
		h.source.add(at)
	}
	failing := map[int64]bool{}
	for _, l := range lasts[:4] {
		from, _ := freshness.BaselineRange(l, newYork, h.cfg.Silence)
		failing[from.UnixMilli()] = true
	}
	fifthFrom, _ := freshness.BaselineRange(lasts[4], newYork, h.cfg.Silence)
	healthy := false
	h.source.betweenErr = func(from, _ time.Time) error {
		if failing[from.UnixMilli()] || (!healthy && from.Equal(fifthFrom)) {
			return errors.New("database is locked")
		}
		return nil
	}
	h.tickAt(at.Add(time.Second)) // the walk: every baseline fails
	if got := len(h.rec.State().Unjudged); got != 5 {
		t.Fatalf("unjudged = %d, want 5", got)
	}
	healthy = true // only the fifth gap's baseline can be read now
	before := h.source.betweenCalls
	h.tickAt(at.Add(time.Minute))
	if reads := h.source.betweenCalls - before; reads != maxUnjudgedPerTick {
		t.Fatalf("%d baseline reads in a tick, want the cap %d", reads, maxUnjudgedPerTick)
	}
	h.runner.ready, h.runner.reason = false, "disconnected" // keep it pending to inspect
	h.tickAt(at.Add(2 * time.Minute))
	state := h.rec.State()
	if len(state.Unjudged) != 4 || state.Pending == nil || state.Pending.LastEventMS != lasts[4].UnixMilli() {
		t.Fatalf("after two ticks: unjudged %d, pending %+v; want the fifth gap judged despite four failing ahead of it", len(state.Unjudged), state.Pending)
	}
}

func TestExpiringGapGetsOneLastRead(t *testing.T) {
	h := newHarness(t, day)
	last := busyBaseline(h.source, day, 14)
	h.tickAt(last.Add(time.Second))
	end := last.Add(40 * time.Hour)
	h.source.add(end)
	fromA, _ := freshness.BaselineRange(last, newYork, h.cfg.Silence)
	failing := true
	h.source.betweenErr = func(from, _ time.Time) error {
		if from.Equal(fromA) && failing {
			return errors.New("disk I/O error")
		}
		return nil
	}
	h.tickAt(end.Add(time.Second))
	expiry := last.Add(-h.cfg.Margin).Add(h.cfg.MaxWindow)
	// Just at the bound: still retried within the cap, still failing.
	h.tickAt(expiry)
	if len(h.rec.State().Unjudged) != 1 {
		t.Fatal("expired at the bound, not past it")
	}
	failing = false // the baseline comes back just as the window expires
	h.tickAt(expiry.Add(time.Millisecond))
	state := h.rec.State()
	if len(state.Unjudged) != 0 || len(state.History)+boolInt(state.Pending != nil) != 1 {
		t.Fatalf("state = %+v", state)
	}
	if len(state.History) == 1 && state.History[0].State == StateUnjudged {
		t.Fatal("a gap judgeable at expiry got the could-not-be-judged notice")
	}
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func TestUnjudgedFailuresSaveLazily(t *testing.T) {
	h := newHarness(t, day)
	h.cfg.PersistEvery = 365 * 24 * time.Hour
	h.restart()
	last := busyBaseline(h.source, day, 14)
	h.tickAt(last.Add(time.Second))
	end := last.Add(40 * time.Hour)
	h.source.add(end)
	fromA, _ := freshness.BaselineRange(last, newYork, h.cfg.Silence)
	h.source.betweenErr = func(from, _ time.Time) error {
		if from.Equal(fromA) {
			return errors.New("database is locked")
		}
		return nil
	}
	h.tickAt(end.Add(time.Second))
	saved := h.savedState()
	if len(saved.Unjudged) != 1 || saved.Unjudged[0].Failures != 1 {
		t.Fatalf("saved = %+v, want the new unjudged gap saved at once", saved.Unjudged)
	}
	for i := 1; i <= 10; i++ {
		h.tickAt(end.Add(time.Duration(i) * time.Minute))
	}
	if got := h.rec.State().Unjudged[0].Failures; got != 11 {
		t.Fatalf("in-memory failures = %d, want 11", got)
	}
	if got := h.savedState().Unjudged[0].Failures; got != 1 {
		t.Fatalf("saved failures = %d: retries rewrote the file every tick", got)
	}
}

func TestSettledGapIsSavedAtOnce(t *testing.T) {
	h := newHarness(t, day)
	h.cfg.PersistEvery = 365 * 24 * time.Hour
	h.restart()
	last := busyBaseline(h.source, day, 14)
	h.tickAt(last.Add(time.Second))
	end := last.Add(40 * time.Hour)
	h.source.add(end)
	fromA, _ := freshness.BaselineRange(last, newYork, h.cfg.Silence)
	failOnce := true
	h.source.betweenErr = func(from, _ time.Time) error {
		if from.Equal(fromA) && failOnce {
			failOnce = false
			return errors.New("database is locked")
		}
		return nil
	}
	h.runner.ready, h.runner.reason = false, "disconnected"
	h.tickAt(end.Add(time.Second))
	h.tickAt(end.Add(time.Minute))
	saved := h.savedState()
	if len(saved.Unjudged) != 0 || saved.Pending == nil || saved.Pending.LastEventMS != last.UnixMilli() {
		t.Fatalf("saved = %+v, want the settled gap's episode on disk", saved)
	}
}

func TestRunWaitsWhileStateCantBeSaved(t *testing.T) {
	h := newHarness(t, day)
	last := busyBaseline(h.source, day, 14)
	h.tickAt(last.Add(time.Second))
	end := last.Add(20 * time.Hour)
	h.source.add(end)
	h.tickAt(end.Add(time.Second))
	dir := filepath.Dir(h.path)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	at := end.Add(3 * time.Minute)
	h.tickAt(at)
	pending := h.rec.State().Pending
	if h.runner.callCount() != 0 || pending == nil || pending.Waiting != "state_not_saved" || pending.Attempts != 0 || pending.State != StatePending {
		t.Fatalf("ran without a durable running state: calls %d, pending %+v", h.runner.callCount(), pending)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	h.tickAt(at.Add(h.cfg.BusyRetry))
	if h.runner.callCount() != 1 {
		t.Fatalf("calls = %d once the state could be saved", h.runner.callCount())
	}
}

func TestPreserveUnreadablePicksAFreshName(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, StateFileName)
	now := time.UnixMilli(1_700_000_000_000)
	taken := fmt.Sprintf("%s.unreadable-%d", path, now.UnixMilli())
	if err := os.WriteFile(taken, []byte("older backup"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{bad"), 0o600); err != nil {
		t.Fatal(err)
	}
	aside, err := preserveUnreadable(path, now)
	if err != nil || aside != taken+"-1" {
		t.Fatalf("aside = %q, %v; want %q", aside, err, taken+"-1")
	}
	if data, _ := os.ReadFile(taken); string(data) != "older backup" {
		t.Fatalf("the older backup was overwritten: %q", data)
	}
}

func TestPreserveUnreadableCopiesWhenItCantMove(t *testing.T) {
	saved := renameUnreadable
	renameUnreadable = func(string, string) error { return errors.New("cross-device link") }
	t.Cleanup(func() { renameUnreadable = saved })
	dir := t.TempDir()
	path := filepath.Join(dir, StateFileName)
	if err := os.WriteFile(path, []byte("{bad"), 0o600); err != nil {
		t.Fatal(err)
	}
	aside, err := preserveUnreadable(path, time.UnixMilli(5))
	if err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(aside); string(data) != "{bad" {
		t.Fatalf("copy = %q", data)
	}
}

func TestPreserveUnreadableReportsAStatError(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The state path sits under a regular file: every stat of a name next to
	// it fails with "not a directory", which must not loop forever.
	path := filepath.Join(file, StateFileName)
	done := make(chan struct{})
	var rec *Recoverer
	go func() {
		rec = New(testConfig(), nil, nil, path, zerolog.Nop())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("New hung on an unreadable state path")
	}
	if rec.Snapshot().StateLoadError == "" {
		t.Fatal("no load error reported")
	}
}

func TestTakeoverStartsTheAttemptHistoryAfresh(t *testing.T) {
	h := newHarness(t, day)
	last := busyBaseline(h.source, day, 14)
	h.tickAt(last.Add(time.Second))
	endA := last.Add(20 * time.Hour)
	endB := endA.Add(20 * time.Hour)
	h.source.add(endA, endB)
	fromA, _ := freshness.BaselineRange(last, newYork, h.cfg.Silence)
	failing := true
	h.source.betweenErr = func(from, _ time.Time) error {
		if from.Equal(fromA) && failing {
			return errors.New("database is locked")
		}
		return nil
	}
	// B runs and comes back partial twice while A can't be judged.
	partial := runOutcome{started: true, result: RunResult{Connected: true, Listed: 9, Conversations: 1, Messages: 2, Errors: 1, HistoryTeed: 3, InboxOutcome: InboxOK}}
	h.runner.results = []runOutcome{partial, partial, goodRun}
	at := endB.Add(3 * time.Minute)
	for h.runner.callCount() < 2 {
		h.tickAt(at)
		at = at.Add(5 * time.Minute)
	}
	if p := h.rec.State().Pending; p == nil || p.Failures != 2 {
		t.Fatalf("pending = %+v, want B with two failures", p)
	}
	failing = false
	h.runner.ready, h.runner.reason = false, "disconnected"
	h.tickAt(at)
	p := h.rec.State().Pending
	if p == nil || p.LastEventMS != last.UnixMilli() || p.Attempts != 0 || p.Failures != 0 || p.LastAttempt != nil {
		t.Fatalf("pending after takeover = %+v, want A with a fresh attempt history", p)
	}
}

func TestTakeoverOfALeftoverRunningEpisode(t *testing.T) {
	h := newHarness(t, day)
	last := busyBaseline(h.source, day, 14)
	h.tickAt(last.Add(time.Second))
	endA := last.Add(20 * time.Hour)
	endB := endA.Add(20 * time.Hour)
	h.source.add(endA, endB)
	fromA, _ := freshness.BaselineRange(last, newYork, h.cfg.Silence)
	failing := true
	h.source.betweenErr = func(from, _ time.Time) error {
		if from.Equal(fromA) && failing {
			return errors.New("database is locked")
		}
		return nil
	}
	// The daemon dies during B's run.
	var during State
	h.runner.onRun = func(time.Time, bool) { during = h.savedState() }
	h.tickAt(endB.Add(3 * time.Minute))
	if err := saveState(h.path, during); err != nil {
		t.Fatal(err)
	}
	h.runner.onRun = nil
	failing = false
	h.restart()
	h.tickAt(endB.Add(4 * time.Minute)) // A judged: takes over the leftover
	if h.runner.callCount() != 2 || !h.runner.calls[1].Equal(last.Add(-h.cfg.Margin)) {
		t.Fatalf("calls = %v, want B's cut-off run, then one from A's start", h.runner.calls)
	}
	done := h.rec.State().History
	if len(done) != 1 || done[0].LastEventMS != last.UnixMilli() || done[0].State != StateRecovered || len(done[0].Covers) != 1 {
		t.Fatalf("history = %+v", done)
	}
}

func TestReadOnlyStateStillRuns(t *testing.T) {
	h := newHarness(t, day)
	if err := os.WriteFile(h.path, []byte("{bad"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(h.path)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	h.restart() // read-only: the bad file can be neither moved nor copied
	last := busyBaseline(h.source, day, 14)
	h.tickAt(last.Add(time.Second))
	end := last.Add(20 * time.Hour)
	h.source.add(end)
	h.tickAt(end.Add(time.Second))
	h.tickAt(end.Add(3 * time.Minute))
	if h.runner.callCount() != 1 {
		t.Fatalf("calls = %d: read-only mode blocked the recovery", h.runner.callCount())
	}
	if data, _ := os.ReadFile(h.path); string(data) != "{bad" {
		t.Fatalf("the unreadable original was overwritten: %q", data)
	}
}

func TestCancelledTickRecordsNothing(t *testing.T) {
	h := newHarness(t, day)
	last := busyBaseline(h.source, day, 14)
	h.tickAt(last.Add(time.Second))
	end := last.Add(40 * time.Hour)
	h.source.add(end)
	fromA, _ := freshness.BaselineRange(last, newYork, h.cfg.Silence)
	h.source.betweenErr = func(from, _ time.Time) error {
		if from.Equal(fromA) {
			return errors.New("database is locked")
		}
		return nil
	}
	h.tickAt(end.Add(time.Second))
	before := h.rec.State()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h.clock = last.Add(-h.cfg.Margin).Add(h.cfg.MaxWindow + time.Hour) // the gap would expire now
	h.rec.Tick(ctx)
	after := h.rec.State()
	if len(after.Unjudged) != len(before.Unjudged) || len(after.History) != 0 {
		t.Fatalf("a cancelled tick changed the state: %+v", after)
	}
}

func TestExpiringReadIgnoresTheCap(t *testing.T) {
	h := newHarness(t, day)
	last := time.Date(2026, 10, 5, 10, 0, 0, 0, newYork)
	steadyAllDay(h.source, last)
	h.tickAt(last.Add(time.Second))
	// Four 25-hour gaps, all with failing baselines.
	var lasts []time.Time
	at := last
	for i := 0; i < 4; i++ {
		lasts = append(lasts, at)
		at = at.Add(25 * time.Hour)
		h.source.add(at)
	}
	failing := map[int64]bool{}
	for _, l := range lasts {
		from, _ := freshness.BaselineRange(l, newYork, h.cfg.Silence)
		failing[from.UnixMilli()] = true
	}
	h.source.betweenErr = func(from, _ time.Time) error {
		if failing[from.UnixMilli()] {
			return errors.New("database is locked")
		}
		return nil
	}
	h.tickAt(at.Add(time.Second))
	if got := len(h.rec.State().Unjudged); got != 4 {
		t.Fatalf("unjudged = %d", got)
	}
	// Rotate the first gap to the back of the line, so the three others
	// fill the cap ahead of it.
	for i := 1; i <= 3; i++ {
		h.tickAt(at.Add(time.Duration(i) * time.Minute))
	}
	if order := h.rec.State().Unjudged; order[len(order)-1].LastEventMS != lasts[0].UnixMilli() {
		t.Fatalf("order = %+v, want the first gap last", order)
	}
	// Only the first gap's window passes 7 days now.
	expiry := lasts[0].Add(-h.cfg.Margin).Add(h.cfg.MaxWindow)
	h.tickAt(expiry.Add(time.Millisecond))
	state := h.rec.State()
	if len(state.History) != 1 || state.History[0].LastEventMS != lasts[0].UnixMilli() || state.History[0].State != StateUnjudged {
		t.Fatalf("history = %+v, want the expiring gap finished in this tick", state.History)
	}
}
