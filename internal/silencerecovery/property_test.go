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
	cases := 40
	if testing.Short() {
		cases = 8
	}
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
			path := filepath.Join(t.TempDir(), StateFileName)
			type run struct {
				since     time.Time
				at        time.Time
				completed bool
			}
			var runs []*run
			var current *run
			crashNext := false
			var atCrash []byte
			runner := &fakeRunner{ready: true}
			runner.onRun = func(since time.Time, started bool) {
				if !started {
					return // the guard was busy: nothing ran
				}
				current = &run{since: since, at: clock}
				runs = append(runs, current)
				if crashNext {
					crashNext = false
					// What a process killed now leaves on disk.
					atCrash, _ = os.ReadFile(path)
					panic(errCrash)
				}
			}
			newRecoverer := func() *Recoverer {
				rec := New(cfg, src, runner, nil, path, zerolog.Nop())
				rec.now = func() time.Time { return clock }
				return rec
			}
			rec := newRecoverer()
			for clock.Before(s.end) {
				// A busy guard on one attempt in five.
				if r.Intn(5) == 0 {
					runner.results = []runOutcome{{started: false}, goodRun}
				} else {
					runner.results = []runOutcome{goodRun}
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
			// Let anything still owed finish.
			for i := 0; i < 40; i++ {
				runner.results = []runOutcome{goodRun}
				rec.Tick(context.Background())
				clock = clock.Add(10 * time.Minute)
			}

			endedBy := map[int64]time.Time{}
			prev := time.Time{}
			for _, event := range s.events {
				if !prev.IsZero() {
					endedBy[prev.UnixMilli()] = event
				}
				prev = event
			}
			tooLarge := func(key int64) bool {
				return endedBy[key].Sub(time.UnixMilli(key)) > cfg.MaxWindow
			}
			completed := map[int64]int{}
			for _, run := range runs {
				key := run.since.Add(cfg.Margin).UnixMilli()
				if !want[key] {
					t.Fatalf("fetched from %v, which no stalled silence explains", run.since)
				}
				if tooLarge(key) {
					t.Fatalf("fetched a window longer than MaxWindow from %v", run.since)
				}
				if end := endedBy[key]; run.at.Before(end.Add(cfg.Settle)) {
					t.Fatalf("ran at %v, before the silence ending %v had settled", run.at, end)
				}
				if run.completed {
					completed[key]++
				}
			}
			history := map[int64]Episode{}
			for _, episode := range rec.State().History {
				history[episode.LastEventMS] = episode
			}
			for key := range want {
				episode, ok := history[key]
				if !ok {
					t.Fatalf("stalled silence after %v never reached the history (pending %+v)", time.UnixMilli(key), rec.State().Pending)
				}
				if tooLarge(key) {
					if completed[key] != 0 || episode.State != StateWindowTooLarge {
						t.Fatalf("silence after %v: %d runs, state %s; want a notice and no run", time.UnixMilli(key), completed[key], episode.State)
					}
					continue
				}
				if completed[key] != 1 || episode.State != StateRecovered {
					t.Fatalf("silence after %v: %d completed runs, state %s; want exactly one recovery", time.UnixMilli(key), completed[key], episode.State)
				}
			}
			if len(history) != len(want) {
				t.Fatalf("history has %d episodes, oracle %d", len(history), len(want))
			}
		})
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
