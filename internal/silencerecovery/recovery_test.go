package silencerecovery

import (
	"context"
	"encoding/json"
	"errors"
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

	latestErr  error
	betweenErr func(from, to time.Time) error
	calls      int
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
	pull   *PullHealth
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
	h.rec = New(h.cfg, h.source, h.runner, func() *PullHealth {
		if h.pull == nil {
			return nil
		}
		cp := *h.pull
		return &cp
	}, h.path, zerolog.Nop())
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
	if pending := h.rec.State().Pending; pending == nil || pending.State != StatePending {
		t.Fatalf("an interrupted run did not read back as pending: %+v", pending)
	}
	h.tickAt(end.Add(4 * time.Minute))
	if h.runner.callCount() != 2 {
		t.Fatalf("calls = %d, want the interrupted run plus one retry", h.runner.callCount())
	}
	if done := h.rec.State().History; len(done) != 1 || done[0].Attempts != 2 || done[0].State != StateRecovered {
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

	emptyListing := runOutcome{started: true, result: RunResult{Connected: true}}
	h.runner.results = []runOutcome{emptyListing, goodRun, goodRun}
	at := end.Add(3 * time.Minute)
	h.tickAt(at)
	pending := h.rec.State().Pending
	if pending == nil || pending.LastAttempt == nil || pending.LastAttempt.Outcome != OutcomeEmpty {
		t.Fatalf("an empty listing was not an empty outcome: %+v", pending)
	}
	if pending.NextAttemptMS != at.Add(5*time.Minute).UnixMilli() {
		t.Fatalf("next attempt = %d, want 5 minutes later", pending.NextAttemptMS)
	}

	// The next run lists data, but pull health says a pull came back empty.
	h.pull = &PullHealth{ConsecutiveDataless: 1, EmptyWithLocalHistory: true}
	at = at.Add(5 * time.Minute)
	h.tickAt(at)
	pending = h.rec.State().Pending
	if pending == nil || pending.Attempts != 2 || !pending.LastAttempt.PullDataless || pending.LastAttempt.Outcome != OutcomeEmpty {
		t.Fatalf("a run during data-less pulls counted as recovered: %+v", pending)
	}
	if pending.NextAttemptMS != at.Add(15*time.Minute).UnixMilli() {
		t.Fatalf("next attempt = %d, want 15 minutes later", pending.NextAttemptMS)
	}

	h.pull = &PullHealth{}
	h.tickAt(at.Add(15 * time.Minute))
	done := h.rec.State().History
	if len(done) != 1 || done[0].State != StateRecovered || done[0].Attempts != 3 {
		t.Fatalf("history = %+v", done)
	}
	if h.runner.callCount() != 3 {
		t.Fatalf("calls = %d", h.runner.callCount())
	}
}

func TestGivesUpWithANoticeAfterTheLastAttempt(t *testing.T) {
	h := newHarness(t, day)
	last := busyBaseline(h.source, day, 14)
	h.tickAt(last.Add(time.Second))
	end := last.Add(20 * time.Hour)
	h.source.add(end)
	h.tickAt(end.Add(time.Second))

	h.runner.results = []runOutcome{{started: true, result: RunResult{Connected: true}}}
	at := end.Add(3 * time.Minute)
	for i := 0; i < 200 && h.rec.State().Pending != nil; i++ {
		h.tickAt(at)
		at = at.Add(5 * time.Minute)
	}
	maxAttempts := len(h.cfg.Backoff) + 1
	if h.runner.callCount() != maxAttempts {
		t.Fatalf("calls = %d, want %d", h.runner.callCount(), maxAttempts)
	}
	done := h.rec.State().History
	if len(done) != 1 || done[0].State != StateGaveUp || done[0].Attempts != maxAttempts {
		t.Fatalf("history = %+v", done)
	}
	notice := h.rec.Snapshot().Notice
	if !strings.Contains(notice, "gave up") || !strings.Contains(notice, "/api/backfill") || !strings.Contains(notice, last.Add(-time.Hour).Format(time.RFC3339)) {
		t.Fatalf("notice = %q", notice)
	}
	for i := 0; i < 20; i++ {
		h.tickAt(at.Add(time.Duration(i) * time.Hour))
	}
	if h.runner.callCount() != maxAttempts {
		t.Fatal("ran again after giving up")
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
	if pending == nil || pending.LastEventMS != last.UnixMilli() || len(pending.Covers) != 1 || pending.Covers[0] != end.UnixMilli() {
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
	end := last.Add(20 * time.Hour)
	h.source.add(end)
	h.tickAt(end.Add(time.Second))
	snap := h.rec.Snapshot()
	if snap.ActivityError != "" || snap.Pending == nil {
		t.Fatalf("snapshot after recovery of the source = %+v", snap)
	}
}

func TestCorruptStateFileStartsFreshAndIsReplaced(t *testing.T) {
	h := newHarness(t, day)
	if err := os.WriteFile(h.path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.restart()
	if h.rec.Snapshot().StateError == "" {
		t.Fatal("a corrupt state file was not reported")
	}
	last := busyBaseline(h.source, day, 14)
	h.tickAt(last.Add(time.Second))
	if state := h.savedState(); state.WatermarkMS != last.UnixMilli() {
		t.Fatalf("state not rewritten: %+v", state)
	}
	if got := h.rec.Snapshot().StateError; got != "" {
		t.Fatalf("state error kept after a good save: %q", got)
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
	good := RunResult{Connected: true, Listed: 10, Conversations: 2, Messages: 5, HistoryTeed: 7}
	cases := []struct {
		name      string
		result    RunResult
		health    *PullHealth
		requireV2 bool
		want      string
	}{
		{"recovered", good, nil, true, OutcomeRecovered},
		{"recovered with clean pull health", good, &PullHealth{}, true, OutcomeRecovered},
		{"aborted wins", RunResult{Connected: true, Aborted: true, Listed: 10}, nil, false, OutcomeAborted},
		{"nothing listed", RunResult{Connected: true}, nil, false, OutcomeEmpty},
		{"pull health data-less", good, &PullHealth{ConsecutiveDataless: 2}, false, OutcomeEmpty},
		{"pull health empty with history", good, &PullHealth{EmptyWithLocalHistory: true}, false, OutcomeEmpty},
		{"in-window fetch empty", RunResult{Connected: true, Listed: 10, Conversations: 2, Messages: 1, EmptyConversations: 1, HistoryTeed: 3}, nil, false, OutcomeEmpty},
		{"fetch errors", RunResult{Connected: true, Listed: 10, Conversations: 2, Messages: 3, Errors: 1, HistoryTeed: 5}, nil, false, OutcomePartial},
		{"tee failures", RunResult{Connected: true, Listed: 10, Conversations: 2, Messages: 3, HistoryTeed: 4, HistoryTeeFailed: 1}, nil, false, OutcomePartial},
		{"v2 missing", RunResult{Connected: true, Listed: 10, Conversations: 2, Messages: 3}, nil, true, OutcomePartial},
		{"v2 not required", RunResult{Connected: true, Listed: 10, Conversations: 2, Messages: 3}, nil, false, OutcomeRecovered},
		{"nothing in window", RunResult{Connected: true, Listed: 10}, nil, true, OutcomeNothingInWindow},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, detail := Classify(tc.result, tc.health, tc.requireV2)
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
	if pending == nil || pending.State != StatePending || pending.Attempts != 1 || pending.LastAttempt == nil || pending.LastAttempt.Outcome != OutcomeAborted {
		t.Fatalf("pending after a panicking run = %+v", pending)
	}
	h.runner.onRun = nil
	h.tickAt(end.Add(3*time.Minute + h.cfg.Backoff[0]))
	if done := h.rec.State().History; len(done) != 1 || done[0].State != StateRecovered || done[0].Attempts != 2 {
		t.Fatalf("history = %+v", done)
	}
}
