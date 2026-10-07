package freshness

import (
	"math"
	"testing"
	"time"
)

func mustLoad(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("LoadLocation(%q): %v", name, err)
	}
	return loc
}

// dailyTraffic returns events every interval between startHour and endHour
// local time, on each of the days before (not including) the day of until.
func dailyTraffic(until time.Time, days, startHour, endHour int, interval time.Duration, loc *time.Location) []time.Time {
	local := until.In(loc)
	dayStart := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
	var events []time.Time
	for d := days; d >= 1; d-- {
		day := dayStart.AddDate(0, 0, -d)
		for at := day.Add(time.Duration(startHour) * time.Hour); at.Before(day.Add(time.Duration(endHour) * time.Hour)); at = at.Add(interval) {
			events = append(events, at)
		}
	}
	return events
}

// The 2026-10-06 incident: Google ingest delivered its last frame at 01:11
// after a normal day, then nothing for 38 hours while the status said
// connected. With traffic from 07:00 to 23:00 every day, the night hours carry
// no expectation, each morning hour adds one, and the stall is flagged once six
// normally-active hours have passed in silence.
func TestEvaluateSilenceFlagsOvernightOnsetStallByMidday(t *testing.T) {
	loc := mustLoad(t, "America/New_York")
	last := time.Date(2026, 10, 6, 1, 11, 7, 0, loc)
	baseline := dailyTraffic(last, 14, 7, 23, 15*time.Minute, loc)
	cfg := DefaultSilenceConfig

	// 10:15, when the stall was first noticed by hand: three active hours
	// have passed, which an ordinary late morning also produces.
	early := EvaluateSilence(last, baseline, time.Date(2026, 10, 6, 10, 15, 0, 0, loc), loc, cfg)
	if !early.Evaluated || early.Stalled {
		t.Fatalf("10:15 verdict = %+v, want evaluated and not stalled", early)
	}
	if got := early.ExpectedActiveHours; math.Abs(got-3.25) > 1e-9 {
		t.Fatalf("10:15 expected active hours = %v, want 3.25", got)
	}

	justBefore := EvaluateSilence(last, baseline, time.Date(2026, 10, 6, 12, 59, 0, 0, loc), loc, cfg)
	if justBefore.Stalled {
		t.Fatalf("12:59 verdict = %+v, want not stalled", justBefore)
	}
	flagged := EvaluateSilence(last, baseline, time.Date(2026, 10, 6, 13, 0, 0, 0, loc), loc, cfg)
	if !flagged.Stalled || flagged.Rule != RuleExpectedActivity {
		t.Fatalf("13:00 verdict = %+v, want stalled by %s", flagged, RuleExpectedActivity)
	}
	if flagged.BaselineActiveDays != 14 || flagged.BaselineEvents != 14*16*4 {
		t.Fatalf("baseline = %d days / %d events, want 14 / %d", flagged.BaselineActiveDays, flagged.BaselineEvents, 14*16*4)
	}
	if want := 11*time.Hour + 48*time.Minute + 53*time.Second; flagged.Silence != want {
		t.Fatalf("silence = %v, want %v", flagged.Silence, want)
	}
}

// An ordinary night is not a stall: from the last message at 23:00 to 07:30
// only half an hour of a normally-active hour has passed.
func TestEvaluateSilenceIgnoresOrdinaryNight(t *testing.T) {
	loc := mustLoad(t, "America/New_York")
	last := time.Date(2026, 9, 29, 23, 0, 0, 0, loc)
	baseline := dailyTraffic(last, 14, 7, 23, 15*time.Minute, loc)
	verdict := EvaluateSilence(last, baseline, time.Date(2026, 9, 30, 7, 30, 0, 0, loc), loc, DefaultSilenceConfig)
	if !verdict.Evaluated || verdict.Stalled {
		t.Fatalf("verdict = %+v, want evaluated and not stalled", verdict)
	}
	if math.Abs(verdict.ExpectedActiveHours-0.5) > 1e-9 {
		t.Fatalf("expected active hours = %v, want 0.5", verdict.ExpectedActiveHours)
	}
}

// A silence that starts in hours the profile rates as quiet still gets
// flagged once it reaches MaxSilence.
func TestEvaluateSilenceMaxSilenceBoundsQuietHourOnset(t *testing.T) {
	loc := time.UTC
	last := time.Date(2026, 9, 29, 3, 0, 0, 0, loc)
	// Traffic only between 02:00 and 05:00: the silence from 03:00 runs
	// through 21 hours the profile expects nothing from.
	baseline := dailyTraffic(last, 14, 2, 5, 5*time.Minute, loc)
	cfg := DefaultSilenceConfig
	before := EvaluateSilence(last, baseline, last.Add(cfg.MaxSilence-time.Minute), loc, cfg)
	if before.Stalled {
		t.Fatalf("verdict before MaxSilence = %+v, want not stalled", before)
	}
	at := EvaluateSilence(last, baseline, last.Add(cfg.MaxSilence), loc, cfg)
	if !at.Stalled || at.Rule != RuleMaxSilence {
		t.Fatalf("verdict at MaxSilence = %+v, want stalled by %s", at, RuleMaxSilence)
	}
}

func TestEvaluateSilenceNeedsABaseline(t *testing.T) {
	loc := time.UTC
	last := time.Date(2026, 9, 29, 12, 0, 0, 0, loc)
	cfg := DefaultSilenceConfig
	gap := 3 * 24 * time.Hour

	cases := []struct {
		name     string
		baseline []time.Time
	}{
		// A fresh pairing or the week after a long outage: too few days.
		{"few active days", dailyTraffic(last, cfg.MinActiveDays-1, 0, 24, 5*time.Minute, loc)},
		// A platform that carries a handful of messages a day: silence is
		// ordinary there.
		{"low volume", dailyTraffic(last, 14, 9, 14, time.Hour, loc)},
		{"no baseline", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			verdict := EvaluateSilence(last, tc.baseline, last.Add(gap), loc, cfg)
			if verdict.Evaluated || verdict.Stalled {
				t.Fatalf("verdict = %+v, want not evaluated and not stalled", verdict)
			}
			if verdict.Silence != gap {
				t.Fatalf("silence = %v, want %v", verdict.Silence, gap)
			}
		})
	}
	if verdict := EvaluateSilence(time.Time{}, dailyTraffic(last, 14, 0, 24, time.Minute, loc), last, loc, cfg); verdict != (SilenceVerdict{}) {
		t.Fatalf("zero last event verdict = %+v, want zero value", verdict)
	}
}

// The day the silence begins on is cut short by the silence, so it never
// enters the baseline; the window is the BaselineDays whole local days before.
func TestBaselineRangeIsWholeLocalDaysBeforeTheSilence(t *testing.T) {
	loc := mustLoad(t, "America/New_York")
	last := time.Date(2026, 10, 6, 1, 11, 7, 0, loc)
	from, to := BaselineRange(last, loc, DefaultSilenceConfig)
	if want := time.Date(2026, 9, 22, 0, 0, 0, 0, loc); !from.Equal(want) {
		t.Fatalf("from = %v, want %v", from, want)
	}
	if want := time.Date(2026, 10, 6, 0, 0, 0, 0, loc); !to.Equal(want) {
		t.Fatalf("to = %v, want %v", to, want)
	}

	// Activity on the silence day itself (before the last event) changes
	// nothing.
	baseline := dailyTraffic(last, 14, 7, 23, 15*time.Minute, loc)
	withSameDay := append(append([]time.Time(nil), baseline...), time.Date(2026, 10, 6, 0, 30, 0, 0, loc))
	now := time.Date(2026, 10, 6, 13, 0, 0, 0, loc)
	if a, b := EvaluateSilence(last, baseline, now, loc, DefaultSilenceConfig), EvaluateSilence(last, withSameDay, now, loc, DefaultSilenceConfig); a != b {
		t.Fatalf("same-day event changed the verdict: %+v vs %+v", a, b)
	}
}

// Kolkata is UTC+05:30, so its clock hours start at :30 UTC. An event at 10:45
// local belongs to hour 10, and the 10:00-11:00 local hour is what counts.
func TestProfileUsesLocalClockHoursInHalfHourZones(t *testing.T) {
	loc := mustLoad(t, "Asia/Kolkata")
	day := time.Date(2026, 9, 1, 0, 0, 0, 0, loc)
	var events []time.Time
	for d := 0; d < 3; d++ {
		events = append(events, day.AddDate(0, 0, d).Add(10*time.Hour+45*time.Minute))
	}
	profile := BuildProfile(events, day, day.AddDate(0, 0, 3), loc)
	for hour, p := range profile.Hours {
		want := 0.0
		if hour == 10 {
			want = 1
		}
		if p != want {
			t.Fatalf("hour %d fraction = %v, want %v", hour, p, want)
		}
	}
	from := day.AddDate(0, 0, 5).Add(9*time.Hour + 30*time.Minute)
	if got := profile.ExpectedActiveHours(from, from.Add(2*time.Hour), loc); math.Abs(got-1) > 1e-9 {
		t.Fatalf("expected active hours over 09:30-11:30 = %v, want 1", got)
	}
}

// Integrating across DST transitions must neither loop nor miscount: the
// 25-hour fall-back day and the 23-hour spring-forward day each contribute
// their real length when every hour is active.
func TestExpectedActiveHoursAcrossDST(t *testing.T) {
	loc := mustLoad(t, "America/New_York")
	var full Profile
	for hour := range full.Hours {
		full.Hours[hour] = 1
	}
	for _, tc := range []struct {
		day  time.Time
		want float64
	}{
		{time.Date(2026, 11, 1, 0, 0, 0, 0, loc), 25},
		{time.Date(2026, 3, 8, 0, 0, 0, 0, loc), 23},
	} {
		got := full.ExpectedActiveHours(tc.day, tc.day.AddDate(0, 0, 1), loc)
		if math.Abs(got-tc.want) > 1e-9 {
			t.Fatalf("expected active hours on %s = %v, want %v", tc.day.Format("2006-01-02"), got, tc.want)
		}
	}
}
