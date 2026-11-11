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

// Without a trustworthy profile the length rules still apply, so a stall is
// never invisible for good. (Review of PR #190: a thin baseline used to skip
// every rule, so a light user's newest platform stayed fresh forever.)
func TestEvaluateSilenceWithoutAProfileStillFlagsLongSilence(t *testing.T) {
	loc := time.UTC
	last := time.Date(2026, 10, 6, 1, 11, 0, 0, loc)
	cfg := DefaultSilenceConfig

	cases := []struct {
		name     string
		baseline []time.Time
		rule     string
		after    time.Duration
	}{
		// The days after a long outage: busy when active, too few days to
		// trust the hourly profile. MaxSilence applies.
		{"few active days", dailyTraffic(last, cfg.MinActiveDays-1, 0, 24, 5*time.Minute, loc), RuleMaxSilence, cfg.MaxSilence},
		// A light user: ten events a day. Hours of silence mean nothing;
		// three days do.
		{"low volume", dailyTraffic(last, 14, 0, 10, time.Hour, loc), RuleLongSilence, cfg.LongSilence},
		{"no baseline", nil, RuleLongSilence, cfg.LongSilence},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := EvaluateSilence(last, tc.baseline, last.Add(tc.after-time.Minute), loc, cfg)
			if before.Evaluated || before.Stalled {
				t.Fatalf("verdict just before %v = %+v, want not evaluated and not stalled", tc.after, before)
			}
			at := EvaluateSilence(last, tc.baseline, last.Add(tc.after), loc, cfg)
			if at.Evaluated || !at.Stalled || at.Rule != tc.rule {
				t.Fatalf("verdict at %v = %+v, want not evaluated, stalled by %s", tc.after, at, tc.rule)
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

// nextTransition returns the first zone change in loc after from.
func nextTransition(t *testing.T, from time.Time, loc *time.Location) time.Time {
	t.Helper()
	_, end := from.In(loc).ZoneBounds()
	if end.IsZero() {
		t.Fatalf("%s has no zone change after %v", loc, from)
	}
	return end
}

// Chatham springs forward from 02:45 to 03:45, off the hour. The slice of
// clock hour 2 ends at the jump; the 15 minutes after it belong to hour 3.
// (Review of PR #190: a slice crossing the jump was credited wholly to hour 2.)
func TestExpectedActiveHoursSplitsAtOffHourDSTJump(t *testing.T) {
	loc := mustLoad(t, "Pacific/Chatham")
	jump := nextTransition(t, time.Date(2026, 9, 20, 0, 0, 0, 0, loc), loc)
	if got := jump.In(loc).Format("15:04"); got != "03:45" {
		t.Fatalf("Chatham spring-forward lands at %s, want 03:45", got)
	}
	var profile Profile
	profile.Hours[2] = 1                // hour 3 stays 0
	from := jump.Add(-45 * time.Minute) // 02:00 local
	to := jump.Add(15 * time.Minute)    // 04:00 local
	if got := profile.ExpectedActiveHours(from, to, loc); math.Abs(got-0.75) > 1e-9 {
		t.Fatalf("expected active hours across the jump = %v, want 0.75", got)
	}
	split := profile.ExpectedActiveHours(from, jump, loc) + profile.ExpectedActiveHours(jump, to, loc)
	if math.Abs(split-0.75) > 1e-9 {
		t.Fatalf("split at the jump = %v, want 0.75", split)
	}
}

// Santiago springs forward at midnight, so the day it happens on starts at
// 01:00. The baseline must still be exactly BaselineDays local dates.
// (Review of PR #190: the missing midnight normalized into the previous day
// and the window covered 15 dates.)
func TestBaselineRangeOnASkippedMidnight(t *testing.T) {
	loc := mustLoad(t, "America/Santiago")
	var skipped time.Time
	for day := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC); day.Year() == 2026; day = day.AddDate(0, 0, 1) {
		y, m, d := day.Date()
		if mid := time.Date(y, m, d, 0, 0, 0, 0, loc); mid.Day() != d {
			skipped = time.Date(y, m, d, 12, 0, 0, 0, loc)
			break
		}
	}
	if skipped.IsZero() {
		t.Fatal("found no skipped midnight in Santiago after 2026-08-01")
	}
	for _, last := range []time.Time{skipped, skipped.AddDate(0, 0, 3)} {
		from, to := BaselineRange(last, loc, DefaultSilenceConfig)
		if to.In(loc).Day() != last.In(loc).Day() || to.In(loc).Hour() > 1 {
			t.Fatalf("to = %v, want the start of %v", to.In(loc), last.In(loc).Format("2006-01-02"))
		}
		dates := map[string]bool{}
		for at := from; at.Before(to); at = at.Add(30 * time.Minute) {
			dates[at.In(loc).Format("2006-01-02")] = true
		}
		if len(dates) != DefaultSilenceConfig.BaselineDays {
			t.Fatalf("baseline for %v covers %d local dates (%v .. %v), want %d",
				last.In(loc).Format("2006-01-02"), len(dates), from.In(loc), to.In(loc), DefaultSilenceConfig.BaselineDays)
		}
	}
}

// One burst (a history sync after pairing) on an otherwise sparse platform
// must not make it look busy: the volume gate uses the median day, so its
// ordinary day-long gaps are not stalls; only LongSilence applies.
// (Review of PR #190: a mean-based gate fired the 16 h cap daily.)
func TestEvaluateSilenceIgnoresASingleBurstWhenJudgingVolume(t *testing.T) {
	loc := time.UTC
	last := time.Date(2026, 10, 6, 12, 0, 0, 0, loc)
	baseline := dailyTraffic(last, 14, 12, 13, time.Hour, loc) // one event a day at noon
	burstDay := time.Date(2026, 9, 30, 9, 0, 0, 0, loc)
	for i := 0; i < 2000; i++ {
		baseline = append(baseline, burstDay.Add(time.Duration(i)*time.Second))
	}
	cfg := DefaultSilenceConfig
	day := EvaluateSilence(last, baseline, last.Add(cfg.MaxSilence+8*time.Hour), loc, cfg)
	if day.Stalled || day.Evaluated || day.BaselineMedianDailyEvents != 1 {
		t.Fatalf("verdict a day into silence = %+v, want not evaluated, not stalled, median 1", day)
	}
	long := EvaluateSilence(last, baseline, last.Add(cfg.LongSilence), loc, cfg)
	if !long.Stalled || long.Rule != RuleLongSilence {
		t.Fatalf("verdict at LongSilence = %+v, want stalled by %s", long, RuleLongSilence)
	}
}

// A non-positive score limit must not turn a fresh event into a stall.
func TestEvaluateSilenceNonPositiveLimitNeverFlagsAFreshEvent(t *testing.T) {
	loc := time.UTC
	last := time.Date(2026, 10, 6, 12, 0, 0, 0, loc)
	baseline := dailyTraffic(last, 14, 0, 24, 5*time.Minute, loc)
	for _, limit := range []float64{0, -1} {
		cfg := DefaultSilenceConfig
		cfg.ExpectedActiveHoursLimit = limit
		if v := EvaluateSilence(last, baseline, last, loc, cfg); v.Stalled {
			t.Fatalf("limit %v: fresh event verdict = %+v, want not stalled", limit, v)
		}
	}
}

// Integrating up to Go's maximum time terminates (Time.Add saturates there).
func TestExpectedActiveHoursTerminatesNearMaxTime(t *testing.T) {
	var full Profile
	for hour := range full.Hours {
		full.Hours[hour] = 1
	}
	maxTime := time.Unix(1<<63-1-62135596800, 999999999)
	done := make(chan float64, 1)
	go func() {
		done <- full.ExpectedActiveHours(maxTime.Add(-time.Hour).Truncate(time.Second), maxTime, time.UTC)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ExpectedActiveHours did not terminate near the maximum time")
	}
}

// A pairing day's history burst over only one or two active days is still the
// median; the platform is not busy until there are MinBusyDays active days.
// (Third review of PR #190.)
func TestEvaluateSilenceNeedsSeveralDaysBeforeABurstCountsAsBusy(t *testing.T) {
	loc := time.UTC
	last := time.Date(2026, 10, 6, 21, 0, 0, 0, loc)
	cfg := DefaultSilenceConfig
	pairingDay := time.Date(2026, 10, 4, 10, 0, 0, 0, loc)
	var baseline []time.Time
	for i := 0; i < 40; i++ { // history sync on the pairing day
		baseline = append(baseline, pairingDay.Add(time.Duration(i)*time.Minute))
	}
	for i := 0; i < 3; i++ { // an ordinary light day after it
		baseline = append(baseline, time.Date(2026, 10, 5, 9+4*i, 0, 0, 0, loc))
	}
	overnight := EvaluateSilence(last, baseline, last.Add(cfg.MaxSilence+2*time.Hour), loc, cfg)
	if overnight.Stalled || overnight.BaselineActiveDays != 2 {
		t.Fatalf("verdict after an ordinary overnight gap = %+v, want 2 active days and not stalled", overnight)
	}
	if long := EvaluateSilence(last, baseline, last.Add(cfg.LongSilence), loc, cfg); !long.Stalled || long.Rule != RuleLongSilence {
		t.Fatalf("verdict at LongSilence = %+v, want stalled by %s", long, RuleLongSilence)
	}
}

// The default MinBusyDays is a boundary: three busy active days are enough
// for the 16 h cap, two are not.
func TestEvaluateSilenceBusyNeedsExactlyMinBusyDays(t *testing.T) {
	loc := time.UTC
	last := time.Date(2026, 10, 6, 21, 0, 0, 0, loc)
	cfg := DefaultSilenceConfig
	for _, tc := range []struct {
		days    int
		stalled bool
	}{{2, false}, {3, true}} {
		baseline := dailyTraffic(last, tc.days, 9, 21, 30*time.Minute, loc) // 24 events a day
		v := EvaluateSilence(last, baseline, last.Add(cfg.MaxSilence), loc, cfg)
		if v.Stalled != tc.stalled || (tc.stalled && v.Rule != RuleMaxSilence) {
			t.Fatalf("%d busy days at MaxSilence: verdict = %+v, want stalled=%v by %s", tc.days, v, tc.stalled, RuleMaxSilence)
		}
	}
}

// SilenceBlock is the freshness.<platform>.silence contract that /api/status
// and `openmessage status --json` share. Readers key on these names, and the
// daemon's carry-over path type-asserts last_event_ms (int64),
// baseline_active_days (int) and baseline_median_daily_events (float64), so
// both the names and the Go types are pinned here.
func TestSilenceBlockFieldsAndTypes(t *testing.T) {
	loc := mustLoad(t, "America/New_York")
	last := time.Date(2026, 10, 6, 1, 11, 0, 0, loc)
	cfg := DefaultSilenceConfig
	verdict := EvaluateSilence(last, dailyTraffic(last, 14, 8, 23, 30*time.Minute, loc), last.Add(13*time.Hour+time.Minute), loc, cfg)

	block := SilenceBlock(SourceV2Inbox, verdict, cfg, false)
	want := map[string]any{
		"source":                       SourceV2Inbox,
		"last_event_ms":                last.UnixMilli(),
		"silent_ms":                    (13*time.Hour + time.Minute).Milliseconds(),
		"expected_active_hours":        6.2,
		"expected_active_hours_limit":  6.0,
		"max_silent_ms":                (16 * time.Hour).Milliseconds(),
		"long_silent_ms":               (72 * time.Hour).Milliseconds(),
		"baseline_days":                14,
		"baseline_active_days":         14,
		"baseline_events":              14 * 30,
		"baseline_median_daily_events": 30.0,
		"evaluated":                    true,
		"stalled":                      true,
		"rule":                         RuleExpectedActivity,
	}
	if len(block) != len(want) {
		t.Fatalf("SilenceBlock has %d fields, want %d: %v", len(block), len(want), block)
	}
	for key, value := range want {
		if got, ok := block[key]; !ok || got != value {
			t.Errorf("SilenceBlock[%q] = %#v, want %#v", key, block[key], value)
		}
	}

	unavailable := SilenceBlock(SourceMessages, EvaluateSilence(last, nil, last.Add(time.Hour), loc, cfg), cfg, true)
	if unavailable["baseline_unavailable"] != true || unavailable["source"] != SourceMessages || unavailable["stalled"] != false {
		t.Fatalf("baseline-unavailable block = %v", unavailable)
	}
	if _, ok := block["baseline_unavailable"]; ok {
		t.Fatal("a block judged with its baseline must not carry baseline_unavailable")
	}
}
