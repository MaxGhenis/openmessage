package silencerecovery

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"testing/quick"
	"time"
	_ "time/tzdata" // the zones below must resolve on any CI image

	"github.com/rs/zerolog"

	"github.com/maxghenis/openmessage/internal/freshness"
)

// Zones with awkward clocks, as in internal/freshness's property tests.
var propertyZones = []string{
	"UTC",
	"America/New_York",
	"Asia/Kolkata",
	"Australia/Lord_Howe",
	"Pacific/Chatham",
	"America/Santiago",
}

// silenceInput is a random silence judged at two clock readings.
type silenceInput struct {
	Loc      *time.Location
	Last     time.Time
	Baseline []time.Time
	Cfg      freshness.SilenceConfig
	// Short is a silence length below MinStallDuration(Cfg).
	Short time.Duration
	// Early and Late are silence lengths with Early <= Late.
	Early, Late time.Duration
}

func (silenceInput) Generate(r *rand.Rand, _ int) reflect.Value {
	loc := mustZone(propertyZones[r.Intn(len(propertyZones))])
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	last := start.Add(time.Duration(r.Int63n(int64(365 * 24 * time.Hour))))
	cfg := freshness.SilenceConfig{
		BaselineDays:          1 + r.Intn(21),
		MinActiveDays:         r.Intn(10),
		MinEventsPerActiveDay: float64(r.Intn(40)),
	}
	if r.Intn(5) > 0 {
		cfg.ExpectedActiveHoursLimit = 0.25 + r.Float64()*20
	}
	if r.Intn(4) > 0 {
		cfg.MaxSilence = time.Duration(r.Int63n(int64(48*time.Hour))) + time.Minute
	}
	if r.Intn(4) > 0 {
		cfg.LongSilence = time.Duration(r.Int63n(int64(200*time.Hour))) + time.Minute
	}
	// A baseline dense enough to be busy at times, spilling past both ends
	// of the window.
	var baseline []time.Time
	n := r.Intn(3000)
	span := time.Duration(cfg.BaselineDays+3) * 24 * time.Hour
	for i := 0; i < n; i++ {
		baseline = append(baseline, last.Add(-span+time.Duration(r.Int63n(int64(span+24*time.Hour)))))
	}
	in := silenceInput{Loc: loc, Last: last, Baseline: baseline, Cfg: cfg}
	if minimum := MinStallDuration(cfg); minimum > time.Millisecond && minimum < 1000*time.Hour {
		in.Short = time.Duration(r.Int63n(int64(minimum)))
		// Hit the boundary often: just under the minimum.
		if r.Intn(3) == 0 {
			in.Short = minimum - time.Duration(1+r.Intn(1000))*time.Millisecond
			if in.Short < 0 {
				in.Short = 0
			}
		}
	}
	in.Early = time.Duration(r.Int63n(int64(240 * time.Hour)))
	in.Late = in.Early + time.Duration(r.Int63n(int64(240*time.Hour)))
	return reflect.ValueOf(in)
}

// TestMinStallDurationIsALowerBound: no silence shorter than
// MinStallDuration is stalled, whatever the baseline, zone or config. The
// Recoverer skips judging such gaps, so this bound must hold exactly.
func TestMinStallDurationIsALowerBound(t *testing.T) {
	property := func(in silenceInput) bool {
		if MinStallDuration(in.Cfg) >= 1000*time.Hour {
			return true
		}
		verdict := freshness.EvaluateSilence(in.Last, in.Baseline, in.Last.Add(in.Short), in.Loc, in.Cfg)
		if verdict.Stalled {
			t.Logf("stalled after %v < %v: %+v (cfg %+v, zone %s)", in.Short, MinStallDuration(in.Cfg), verdict, in.Cfg, in.Loc)
		}
		return !verdict.Stalled
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 400, Rand: rand.New(rand.NewSource(20261008))}); err != nil {
		t.Fatal(err)
	}
}

// TestStalledIsMonotoneInTime: once a silence is stalled it stays stalled as
// it goes on. The Recoverer relies on this to judge a silence once, at its
// end, and know whether it was flagged at any point while it lasted.
func TestStalledIsMonotoneInTime(t *testing.T) {
	property := func(in silenceInput) bool {
		early := freshness.EvaluateSilence(in.Last, in.Baseline, in.Last.Add(in.Early), in.Loc, in.Cfg)
		late := freshness.EvaluateSilence(in.Last, in.Baseline, in.Last.Add(in.Late), in.Loc, in.Cfg)
		if early.Stalled && !late.Stalled {
			t.Logf("stalled at %v but not at %v: %+v vs %+v", in.Early, in.Late, early, late)
			return false
		}
		return true
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 400, Rand: rand.New(rand.NewSource(20261009))}); err != nil {
		t.Fatal(err)
	}
}

// stream is a random activity history for the model test: a busy baseline,
// then gaps drawn from short, ordinary, long and very long silences.
type stream struct {
	seed   int64
	events []time.Time
	start  time.Time // first tick
	end    time.Time // last tick
}

func makeStream(r *rand.Rand, seed int64) stream {
	loc := newYork
	base := time.Date(2026, 3, 1+r.Intn(200), 0, 0, 0, 0, loc)
	used := map[int64]bool{}
	var events []time.Time
	add := func(at time.Time) {
		ms := at.UnixMilli()
		if used[ms] {
			return
		}
		used[ms] = true
		events = append(events, time.UnixMilli(ms))
	}
	// 15–25 busy days: 30–80 events a day between 07:00 and 24:00.
	days := 15 + r.Intn(11)
	for d := 0; d < days; d++ {
		n := 30 + r.Intn(51)
		dayStart := time.Date(base.Year(), base.Month(), base.Day()+d, 7, 0, 0, 0, loc)
		for i := 0; i < n; i++ {
			add(dayStart.Add(time.Duration(r.Int63n(int64(17 * time.Hour)))))
		}
	}
	sort.Slice(events, func(i, j int) bool { return events[i].Before(events[j]) })
	start := events[len(events)-1].Add(time.Duration(1+r.Intn(30)) * time.Minute)
	cursor := events[len(events)-1]
	for i := 0; i < 40; i++ {
		var gap time.Duration
		switch p := r.Intn(100); {
		case p < 60:
			gap = time.Duration(1+r.Intn(60)) * time.Minute
		case p < 80:
			gap = time.Duration(1+r.Intn(8*60)) * time.Minute
		case p < 97:
			gap = time.Duration(8*60+r.Intn(72*60)) * time.Minute
		default:
			// Clearly longer than MaxWindow plus margin and settle.
			gap = 7*24*time.Hour + time.Duration(3*60+r.Intn(48*60))*time.Minute
		}
		cursor = cursor.Add(gap + time.Duration(r.Intn(1000))*time.Millisecond)
		add(cursor)
	}
	sort.Slice(events, func(i, j int) bool { return events[i].Before(events[j]) })
	return stream{seed: seed, events: events, start: start, end: cursor.Add(24 * time.Hour)}
}

// oracleEpisodes judges every gap after the first tick's latest event offline,
// at the moment the gap ended.
func oracleEpisodes(s stream, cfg Config) map[int64]bool {
	src := &fakeSource{clock: &s.end, events: s.events}
	var watermark time.Time
	for _, event := range s.events {
		if event.After(s.start) {
			break
		}
		watermark = event
	}
	stalled := map[int64]bool{}
	prev := watermark
	for _, event := range s.events {
		if !event.After(prev) {
			continue
		}
		from, to := freshness.BaselineRange(prev, cfg.Location, cfg.Silence)
		baseline, _ := src.Between(context.Background(), "google", from, to)
		if freshness.EvaluateSilence(prev, baseline, event, cfg.Location, cfg.Silence).Stalled {
			stalled[prev.UnixMilli()] = true
		}
		prev = event
	}
	return stalled
}

// TestRecovererMatchesAnOfflineOracle drives the Recoverer through random
// histories with random tick spacing, busy guards, restarts that lose the
// unsaved watermark, and crashes in the middle of a run, and checks it against
// an offline scan of the same history: every gap the scan judges stalled is
// recovered exactly once from its last event minus the margin (or, when its
// window exceeds MaxWindow, given a notice and never fetched), and nothing
// else is fetched.
func TestRecovererMatchesAnOfflineOracle(t *testing.T) {
	cases := 120
	if testing.Short() {
		cases = 8
	}
	mergesSeen, tooLargeSeen, injectedFailures, latestOutages, unjudgedSeen, gaveUpSeen := 0, 0, 0, 0, 0, 0
	for c := 0; c < cases; c++ {
		seed := int64(20261008 + c)
		t.Run(fmt.Sprintf("seed%d", seed), func(t *testing.T) {
			r := rand.New(rand.NewSource(seed))
			s := makeStream(r, seed)
			cfg := testConfig()
			cfg.HistoryLimit = 1000
			want := oracleEpisodes(s, cfg)

			clock := s.start
			src := &fakeSource{clock: &clock, events: s.events}
			// One baseline read in ten fails (baseline reads span weeks; the
			// gap walks span hours).
			// Baseline reads (local midnight to local midnight) fail one in ten
			// at random, and all of them during bursts of 1h to 9 days.
			baselineFailures := true
			var burstUntil time.Time
			isBaseline := func(from, to time.Time) bool {
				f, e := from.In(cfg.Location), to.In(cfg.Location)
				return f.Hour() == 0 && f.Minute() == 0 && f.Second() == 0 && f.Nanosecond() == 0 &&
					e.Hour() == 0 && e.Minute() == 0 && e.Second() == 0 && e.Nanosecond() == 0
			}
			failedAt := map[int64][]time.Time{} // baseline start -> failed read times
			src.betweenErr = func(from, to time.Time) error {
				if !baselineFailures || !isBaseline(from, to) {
					return nil
				}
				if clock.Before(burstUntil) || r.Intn(10) == 0 {
					injectedFailures++
					failedAt[from.UnixMilli()] = append(failedAt[from.UnixMilli()], clock)
					return fmt.Errorf("database is locked")
				}
				return nil
			}
			// One case in eight is heavy in partial runs, so episodes give up.
			partialHeavy := c%8 == 0
			path := filepath.Join(t.TempDir(), StateFileName)
			type run struct {
				since     time.Time
				at        time.Time
				good      bool // the run's result was a full recovery
				completed bool // its result reached the Recoverer
			}
			var runs []*run
			var current *run
			crashNext := false
			var atCrash []byte
			runner := &fakeRunner{ready: true}
			nextGood := true
			runner.onRun = func(since time.Time, started bool) {
				if !started {
					return // the guard was busy: nothing ran
				}
				current = &run{since: since, at: clock, good: nextGood}
				runs = append(runs, current)
				if crashNext {
					crashNext = false
					// What a process killed now leaves on disk.
					atCrash, _ = os.ReadFile(path)
					panic(errCrash)
				}
			}
			newRecoverer := func() *Recoverer {
				rec := New(cfg, src, runner, path, zerolog.Nop())
				rec.now = func() time.Time { return clock }
				return rec
			}
			rec := newRecoverer()
			readyAgain := time.Time{}
			var latestOutageUntil, downUntil time.Time
			first := true
			for clock.Before(s.end) {
				if !first {
					// The activity clock can't be read for 1h to 3 days, so the
					// next good read crosses many gaps at once.
					if latestOutageUntil.IsZero() && r.Intn(200) == 0 {
						latestOutageUntil = clock.Add(time.Hour + time.Duration(r.Int63n(int64(3*24*time.Hour))))
						src.latestErr = fmt.Errorf("database is locked")
						latestOutages++
					}
					if r.Intn(200) == 0 {
						burstUntil = clock.Add(time.Hour + time.Duration(r.Int63n(int64(9*24*time.Hour))))
					}
					// The daemon is down for 1h to 4 days: no ticks, then a restart.
					if r.Intn(300) == 0 {
						downUntil = clock.Add(time.Hour + time.Duration(r.Int63n(int64(4*24*time.Hour))))
					}
				}
				first = false
				if !latestOutageUntil.IsZero() && !clock.Before(latestOutageUntil) {
					latestOutageUntil = time.Time{}
					src.latestErr = nil
				}
				if clock.Before(downUntil) {
					clock = clock.Add(time.Duration(1+r.Intn(45)) * time.Minute)
					if !clock.Before(downUntil) {
						rec = newRecoverer()
					}
					continue
				}
				// Google is sometimes unable to serve a fetch for a while (1h to
				// 10 days), which makes later silences merge into a pending one
				// and windows outgrow MaxWindow.
				if runner.ready && r.Intn(150) == 0 {
					runner.ready, runner.reason = false, "disconnected"
					readyAgain = clock.Add(time.Hour + time.Duration(r.Int63n(int64(10*24*time.Hour))))
				} else if !runner.ready && !clock.Before(readyAgain) {
					runner.ready, runner.reason = true, ""
				}
				// This tick's run, if one starts: a busy guard, an empty pull
				// path, a partial fetch, or a good recovery.
				nextGood = false
				switch p := r.Intn(100); {
				case p < 20:
					runner.results = []runOutcome{{started: false}}
				case p < 30:
					runner.results = []runOutcome{{started: true, result: RunResult{Connected: true, InboxOutcome: InboxEmpty}}}
				case p < 34 || (partialHeavy && p < 85):
					runner.results = []runOutcome{{started: true, result: RunResult{Connected: true, Listed: 5, Conversations: 1, Messages: 2, Errors: 1, HistoryTeed: 3, InboxOutcome: InboxOK}}}
				default:
					runner.results = []runOutcome{goodRun}
					nextGood = true
				}
				crashNext = r.Intn(40) == 0
				crashed := tickCatchingCrash(rec)
				switch {
				case crashed:
					// The process died during the run: only what was saved
					// before the run survives (the attempt, marked running).
					if err := os.WriteFile(path, atCrash, 0o600); err != nil {
						t.Fatal(err)
					}
					rec = newRecoverer()
				case current != nil:
					current.completed = true
				}
				current = nil
				if r.Intn(60) == 0 {
					rec = newRecoverer() // a restart that drops the unsaved watermark
				}
				clock = clock.Add(time.Duration(1+r.Intn(45)) * time.Minute)
			}
			// Let anything still owed finish. Tick a few times regardless, so a
			// scan an outage held back at the end still happens.
			runner.ready, runner.reason = true, ""
			crashNext = false
			baselineFailures = false
			src.latestErr = nil
			for i := 0; i < 400 && (i < 5 || rec.State().Pending != nil || len(rec.State().Unjudged) > 0); i++ {
				runner.results = []runOutcome{goodRun}
				nextGood = true
				rec.Tick(context.Background())
				if current != nil {
					current.completed = true
					current = nil
				}
				clock = clock.Add(10 * time.Minute)
			}
			if state := rec.State(); state.Pending != nil || len(state.Unjudged) > 0 {
				t.Fatalf("still owed at the end: pending %+v, unjudged %+v", state.Pending, state.Unjudged)
			}

			endedBy := map[int64]time.Time{}
			prev := time.Time{}
			for _, event := range s.events {
				if !prev.IsZero() {
					endedBy[prev.UnixMilli()] = event
				}
				prev = event
			}
			// Every finished episode, and which episode owns each silence.
			owner := map[int64]Episode{}
			episodes := map[int64]Episode{}
			for _, episode := range rec.State().History {
				episodes[episode.LastEventMS] = episode
				if _, dup := owner[episode.LastEventMS]; dup {
					t.Fatalf("silence after %v owned twice", time.UnixMilli(episode.LastEventMS))
				}
				owner[episode.LastEventMS] = episode
				for _, covered := range episode.Covers {
					if _, dup := owner[covered.LastEventMS]; dup {
						t.Fatalf("silence after %v owned twice", time.UnixMilli(covered.LastEventMS))
					}
					owner[covered.LastEventMS] = episode
				}
			}
			completed := map[int64][]*run{}
			for _, run := range runs {
				key := run.since.Add(cfg.Margin).UnixMilli()
				// A run starts at its episode's owner. An earlier silence judged
				// late can take ownership afterwards, so a run that didn't
				// recover may start at a silence now covered by the episode.
				episode, ok := owner[key]
				if !ok || !want[key] {
					t.Fatalf("fetched from %v, which no stalled silence explains", run.since)
				}
				if run.completed && run.good && episode.LastEventMS != key {
					t.Fatalf("a good run from %v, but the silence it started at is owned by the episode after %v", run.since, time.UnixMilli(episode.LastEventMS))
				}
				if run.at.Sub(run.since) > cfg.MaxWindow {
					t.Fatalf("fetched a %v window from %v, over MaxWindow", run.at.Sub(run.since), run.since)
				}
				if end := endedBy[key]; run.at.Before(end.Add(cfg.Settle)) {
					t.Fatalf("ran at %v, before the silence ending %v had settled", run.at, end)
				}
				if run.completed && run.good {
					completed[key] = append(completed[key], run)
					if episode.State != StateRecovered {
						t.Fatalf("a good run for an episode finished as %s", episode.State)
					}
				}
			}
			merges := 0
			for key := range want {
				episode, ok := owner[key]
				if !ok {
					t.Fatalf("stalled silence after %v never recorded", time.UnixMilli(key))
				}
				switch episode.State {
				case StateRecovered:
					runs := completed[episode.LastEventMS]
					if len(runs) != 1 {
						t.Fatalf("episode after %v recovered by %d completed runs, want 1", time.UnixMilli(episode.LastEventMS), len(runs))
					}
					if runs[0].at.Before(endedBy[key].Add(cfg.Settle)) {
						t.Fatalf("silence after %v (to %v) was fetched at %v, before it settled", time.UnixMilli(key), endedBy[key], runs[0].at)
					}
					// The run must cover this silence: start at or before it
					// and run after it ended.
					if runs[0].since.After(time.UnixMilli(key).Add(-cfg.Margin)) || runs[0].at.Before(endedBy[key]) {
						t.Fatalf("silence after %v (to %v) not covered by the run from %v at %v", time.UnixMilli(key), endedBy[key], runs[0].since, runs[0].at)
					}
					if key != episode.LastEventMS {
						merges++
					}
				case StateGaveUp:
					gaveUpSeen++
					if episode.Failures <= len(cfg.Backoff) {
						t.Fatalf("episode after %v gave up after %d failures", time.UnixMilli(episode.LastEventMS), episode.Failures)
					}
				case StateUnjudged:
					// A baseline burst outlasted the window: allowed, with a
					// notice, but only after a read at or past its expiry failed.
					unjudgedSeen++
					if key != episode.LastEventMS {
						t.Fatalf("silence after %v covered by an unjudged gap", time.UnixMilli(key))
					}
					from, _ := freshness.BaselineRange(time.UnixMilli(key), cfg.Location, cfg.Silence)
					expiry := time.UnixMilli(key).Add(-cfg.Margin).Add(cfg.MaxWindow)
					justified := false
					for _, at := range failedAt[from.UnixMilli()] {
						if at.After(expiry) {
							justified = true
						}
					}
					if !justified {
						t.Fatalf("silence after %v ended unjudged without a failed read after its expiry %v", time.UnixMilli(key), expiry)
					}
				case StateWindowTooLarge:
					tooLargeSeen++
					if time.UnixMilli(episode.FinishedAtMS).Sub(time.UnixMilli(episode.SinceMS)) <= cfg.MaxWindow {
						t.Fatalf("episode after %v finished too large with a window of %v", time.UnixMilli(episode.LastEventMS), time.UnixMilli(episode.FinishedAtMS).Sub(time.UnixMilli(episode.SinceMS)))
					}
					if key != episode.LastEventMS {
						t.Fatalf("silence after %v left covered by an episode finished too large", time.UnixMilli(key))
					}
					if len(completed[key]) != 0 {
						t.Fatalf("a too-large episode was fetched")
					}
				default:
					t.Fatalf("silence after %v ended as %s", time.UnixMilli(key), episode.State)
				}
			}
			for key, episode := range owner {
				if !want[key] && episode.State != StateUnjudged {
					t.Fatalf("episode after %v (%s) that no stalled silence explains", time.UnixMilli(key), episode.State)
				}
			}
			mergesSeen += merges
		})
	}
	t.Logf("across all cases: %d merged silences, %d too-large episodes, %d failed baseline reads, %d activity outages, %d stalled silences never judged, %d silences given up",
		mergesSeen, tooLargeSeen, injectedFailures, latestOutages, unjudgedSeen, gaveUpSeen)
	if !testing.Short() && (mergesSeen == 0 || tooLargeSeen == 0 || injectedFailures == 0 || gaveUpSeen == 0) {
		t.Fatalf("the generator no longer produces merges (%d), too-large windows (%d), baseline failures (%d) and give-ups (%d)", mergesSeen, tooLargeSeen, injectedFailures, gaveUpSeen)
	}
}

var errCrash = fmt.Errorf("simulated crash during a run")

// tickCatchingCrash ticks and reports whether the runner simulated a crash.
func tickCatchingCrash(rec *Recoverer) (crashed bool) {
	defer func() {
		if recovered := recover(); recovered != nil {
			if recovered != errCrash {
				panic(recovered)
			}
			crashed = true
		}
	}()
	rec.Tick(context.Background())
	return false
}
