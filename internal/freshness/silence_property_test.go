package freshness

import (
	"math"
	"math/rand"
	"reflect"
	"testing"
	"testing/quick"
	"time"
	_ "time/tzdata" // the zones below must resolve on any CI image
)

// Zones chosen for awkward clocks: DST in both hemispheres, half-hour and
// 45-minute offsets, Lord Howe's 30-minute DST shift, Chatham's DST jump at
// 02:45, and Santiago's DST change at midnight.
var propertyZones = []string{
	"UTC",
	"America/New_York",
	"Asia/Kolkata",
	"Australia/Lord_Howe",
	"Pacific/Chatham",
	"America/Santiago",
}

// silenceCase is a random silence: a last event, a baseline that spills past
// both ends of the baseline window, a clock reading, and a config.
type silenceCase struct {
	Loc      *time.Location
	Last     time.Time
	Baseline []time.Time
	Now      time.Time
	Cfg      SilenceConfig
}

func (silenceCase) Generate(r *rand.Rand, _ int) reflect.Value {
	loc, err := time.LoadLocation(propertyZones[r.Intn(len(propertyZones))])
	if err != nil {
		panic(err)
	}
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	last := start.Add(time.Duration(r.Int63n(int64(365 * 24 * time.Hour))))
	// A third of the cases put the silence or its baseline across a DST
	// change, where clock arithmetic breaks first.
	if r.Intn(3) == 0 {
		if _, change := last.In(loc).ZoneBounds(); !change.IsZero() {
			last = change.Add(time.Duration(r.Int63n(int64(36*time.Hour))) - 18*time.Hour)
		}
	}

	// Optionally confine traffic to a random set of clock hours so profiles
	// are not uniformly flat.
	var hours []int
	if r.Intn(2) == 0 {
		for hour := 0; hour < 24; hour++ {
			if r.Intn(3) != 0 {
				hours = append(hours, hour)
			}
		}
	}
	n := r.Intn(4000)
	span := int64(18 * 24 * time.Hour)
	baseline := make([]time.Time, 0, n)
	for len(baseline) < n {
		at := last.Add(-17 * 24 * time.Hour).Add(time.Duration(r.Int63n(span)))
		if hours != nil {
			local := at.In(loc)
			if !containsInt(hours, local.Hour()) {
				continue
			}
		}
		baseline = append(baseline, at)
	}

	cfg := DefaultSilenceConfig
	if r.Intn(2) == 0 {
		cfg = SilenceConfig{
			BaselineDays:             1 + r.Intn(21),
			MinActiveDays:            r.Intn(15),
			MinEventsPerActiveDay:    float64(r.Intn(60)),
			ExpectedActiveHoursLimit: 0.5 + 12*r.Float64(),
			MaxSilence:               time.Duration(r.Int63n(int64(40 * time.Hour))),
		}
	}
	now := last.Add(time.Duration(r.Int63n(int64(48*time.Hour))) - time.Hour)
	return reflect.ValueOf(silenceCase{Loc: loc, Last: last, Baseline: baseline, Now: now, Cfg: cfg})
}

func containsInt(values []int, want int) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

func checkProperty(t *testing.T, name string, property any) {
	t.Helper()
	if err := quick.Check(property, &quick.Config{MaxCount: 300, Rand: rand.New(rand.NewSource(20261007))}); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
}

// Bounds: the profile holds fractions, the expected-active-hours score never
// exceeds the silent hours, active days never exceed the window, and silence
// is never negative.
func TestSilencePropertyBounds(t *testing.T) {
	checkProperty(t, "bounds", func(c silenceCase) bool {
		from, to := BaselineRange(c.Last, c.Loc, c.Cfg)
		profile := BuildProfile(c.Baseline, from, to, c.Loc)
		for _, p := range profile.Hours {
			if p < 0 || p > 1 {
				return false
			}
		}
		if profile.ActiveDays > c.Cfg.BaselineDays {
			return false
		}
		inWindow := 0
		for _, at := range c.Baseline {
			if !at.Before(from) && at.Before(to) {
				inWindow++
			}
		}
		v := EvaluateSilence(c.Last, c.Baseline, c.Now, c.Loc, c.Cfg)
		return v.BaselineEvents == inWindow &&
			v.BaselineActiveDays == profile.ActiveDays &&
			v.Silence >= 0 &&
			v.ExpectedActiveHours >= 0 &&
			v.ExpectedActiveHours <= v.Silence.Hours()+1e-9
	})
}

// A stall needs a judged baseline and at least as much silence as the
// cheaper rule demands: the score can only reach the limit after that many
// hours, or the cap after MaxSilence.
func TestSilencePropertyStallNeedsBaselineAndSilence(t *testing.T) {
	checkProperty(t, "stall preconditions", func(c silenceCase) bool {
		v := EvaluateSilence(c.Last, c.Baseline, c.Now, c.Loc, c.Cfg)
		if !v.Stalled {
			return v.Rule == ""
		}
		limitHours := time.Duration(c.Cfg.ExpectedActiveHoursLimit * float64(time.Hour))
		floor := limitHours
		if c.Cfg.MaxSilence > 0 && c.Cfg.MaxSilence < floor {
			floor = c.Cfg.MaxSilence
		}
		return v.Evaluated && v.Silence >= floor-time.Millisecond &&
			(v.Rule == RuleExpectedActivity || v.Rule == RuleMaxSilence)
	})
}

// Monotone in time: with no new event, more silence never lowers the score
// and never clears a stall.
func TestSilencePropertyMonotoneInNow(t *testing.T) {
	checkProperty(t, "monotone", func(c silenceCase, extraMinutes uint16) bool {
		later := c.Now.Add(time.Duration(extraMinutes) * time.Minute)
		a := EvaluateSilence(c.Last, c.Baseline, c.Now, c.Loc, c.Cfg)
		b := EvaluateSilence(c.Last, c.Baseline, later, c.Loc, c.Cfg)
		return b.ExpectedActiveHours >= a.ExpectedActiveHours-1e-9 && (!a.Stalled || b.Stalled)
	})
}

// The score is an integral, so it splits at any midpoint.
func TestSilencePropertyExpectedHoursAdditive(t *testing.T) {
	checkProperty(t, "additive", func(c silenceCase, split uint16) bool {
		from, to := BaselineRange(c.Last, c.Loc, c.Cfg)
		profile := BuildProfile(c.Baseline, from, to, c.Loc)
		end := c.Last.Add(40 * time.Hour)
		mid := c.Last.Add(time.Duration(split) * time.Minute % (40 * time.Hour))
		whole := profile.ExpectedActiveHours(c.Last, end, c.Loc)
		parts := profile.ExpectedActiveHours(c.Last, mid, c.Loc) + profile.ExpectedActiveHours(mid, end, c.Loc)
		return math.Abs(whole-parts) < 1e-9
	})
}

// Determinism: baseline order and events outside the baseline window do not
// change the verdict.
func TestSilencePropertyIgnoresOrderAndOutOfWindowEvents(t *testing.T) {
	checkProperty(t, "order and window", func(c silenceCase, seed int64) bool {
		r := rand.New(rand.NewSource(seed))
		shuffled := append([]time.Time(nil), c.Baseline...)
		r.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		from, to := BaselineRange(c.Last, c.Loc, c.Cfg)
		padded := append(shuffled,
			from.Add(-time.Nanosecond),
			from.Add(-time.Duration(r.Int63n(int64(30*24*time.Hour)))-time.Nanosecond),
			to,
			to.Add(time.Duration(r.Int63n(int64(30*24*time.Hour)))),
		)
		want := EvaluateSilence(c.Last, c.Baseline, c.Now, c.Loc, c.Cfg)
		return EvaluateSilence(c.Last, shuffled, c.Now, c.Loc, c.Cfg) == want &&
			EvaluateSilence(c.Last, padded, c.Now, c.Loc, c.Cfg) == want
	})
}

// A fresh event, or a clock reading before the last event, is never a stall.
func TestSilencePropertyNoSilenceNoStall(t *testing.T) {
	checkProperty(t, "fresh", func(c silenceCase, skewMinutes uint16) bool {
		for _, now := range []time.Time{c.Last, c.Last.Add(-time.Duration(skewMinutes) * time.Minute)} {
			v := EvaluateSilence(c.Last, c.Baseline, now, c.Loc, c.Cfg)
			if v.Stalled || v.Silence != 0 || v.ExpectedActiveHours != 0 {
				return false
			}
		}
		return true
	})
}

// In zones without DST, shifting every timestamp by whole days changes
// nothing but the reported last event. The shift stays within ten years:
// Kolkata's offset has been +05:30 since 1945 but was not before.
func TestSilencePropertyWholeDayShiftInvariant(t *testing.T) {
	checkProperty(t, "day shift", func(c silenceCase, days int16) bool {
		if c.Loc.String() != "UTC" && c.Loc.String() != "Asia/Kolkata" {
			return true
		}
		shift := time.Duration(int(days)%3650) * 24 * time.Hour
		moved := make([]time.Time, len(c.Baseline))
		for i, at := range c.Baseline {
			moved[i] = at.Add(shift)
		}
		a := EvaluateSilence(c.Last, c.Baseline, c.Now, c.Loc, c.Cfg)
		b := EvaluateSilence(c.Last.Add(shift), moved, c.Now.Add(shift), c.Loc, c.Cfg)
		a.LastEvent = b.LastEvent
		return a.Silence == b.Silence && a.Stalled == b.Stalled && a.Rule == b.Rule &&
			a.Evaluated == b.Evaluated && a.BaselineEvents == b.BaselineEvents &&
			a.BaselineActiveDays == b.BaselineActiveDays &&
			math.Abs(a.ExpectedActiveHours-b.ExpectedActiveHours) < 1e-9
	})
}
