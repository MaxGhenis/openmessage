// Package freshness decides whether a messaging platform has gone quiet for
// longer than its own recent traffic can explain.
//
// The relative freshness rule in the web status ("this platform trails the
// newest platform by 3+ days") cannot see a stall on the platform that carries
// most of the traffic: when Google Messages stops relaying, it is still the
// newest platform, so it is never "behind". On 2026-10-06 Google ingest went
// silent at 01:11 for 38 hours while /api/status reported connected=true,
// phone_responding=true and stale=false the whole time. This package judges
// silence against the platform's own baseline instead.
package freshness

import (
	"math"
	"time"
)

// SilenceConfig tunes when a quiet platform counts as stalled.
type SilenceConfig struct {
	// BaselineDays is how many whole local calendar days, ending at the start
	// of the day of the last event, build the hour-of-day activity profile.
	// The day the silence starts on is left out: it is cut short by the
	// silence itself and would make its unreached hours look quiet.
	BaselineDays int
	// MinActiveDays is how many local calendar days in the baseline must carry
	// at least one event before silence is judged at all.
	MinActiveDays int
	// MinEventsPerActiveDay is the average event count per active baseline day
	// below which a platform is too quiet for silence to mean anything.
	MinEventsPerActiveDay float64
	// ExpectedActiveHoursLimit flags a stall once the profile predicts activity
	// in at least this many of the silent hours.
	ExpectedActiveHoursLimit float64
	// MaxSilence flags a stall after this long without events, whatever the
	// profile predicts. It bounds detection when the silence starts in hours
	// the profile rates as quiet.
	MaxSilence time.Duration
}

// DefaultSilenceConfig is calibrated against the Google Messages ingest history
// of the install that hit the 2026-10-06 stall (80 days, 8,800 frames). From
// 2026-09-04, when that history became continuous, to the incident, the largest
// expected-active-hours score any ordinary quiet stretch reached was 5.0 and the
// longest one lasted 12.1 hours, so neither rule fires on ordinary nights or
// weekends. The incident reaches the limit at about 13:45 on 2026-10-06, 12.5
// hours after the last frame.
var DefaultSilenceConfig = SilenceConfig{
	BaselineDays:             14,
	MinActiveDays:            7,
	MinEventsPerActiveDay:    20,
	ExpectedActiveHoursLimit: 6,
	MaxSilence:               16 * time.Hour,
}

// Rules reported in SilenceVerdict.Rule.
const (
	RuleExpectedActivity = "expected_activity"
	RuleMaxSilence       = "max_silence"
)

// Profile holds, for each local hour of the day, the fraction of active
// baseline days that saw at least one event in that hour.
type Profile struct {
	Hours      [24]float64
	ActiveDays int
	Events     int
}

// BaselineRange returns the half-open window [from, to) whose events build the
// profile for a silence that began at last: the cfg.BaselineDays whole local
// days before the day of last.
func BaselineRange(last time.Time, loc *time.Location, cfg SilenceConfig) (time.Time, time.Time) {
	if loc == nil {
		loc = time.Local
	}
	year, month, day := last.In(loc).Date()
	// Count days on the calendar, not by adding 24-hour spans, so a DST
	// change inside the window never shifts its first day.
	fromYear, fromMonth, fromDay := time.Date(year, month, day-cfg.BaselineDays, 12, 0, 0, 0, time.UTC).Date()
	return startOfLocalDay(fromYear, fromMonth, fromDay, loc), startOfLocalDay(year, month, day, loc)
}

// startOfLocalDay returns the first instant of a local calendar day. Where a
// DST change skips midnight (America/Santiago springs from 00:00 to 01:00),
// time.Date may normalize the missing midnight into the previous day; the day
// then starts where that zone period ends.
func startOfLocalDay(year int, month time.Month, day int, loc *time.Location) time.Time {
	midnight := time.Date(year, month, day, 0, 0, 0, 0, loc)
	if y, m, d := midnight.Date(); y == year && m == month && d == day {
		return midnight
	}
	if _, end := midnight.ZoneBounds(); !end.IsZero() {
		return end
	}
	return midnight
}

// BuildProfile builds the hour-of-day profile from the events inside
// [from, to). Events outside the window are ignored. A day is active when it
// carries at least one event; days without any event do not dilute the
// profile, so an outage inside the window cannot teach the profile that the
// outage hours are normally quiet.
func BuildProfile(events []time.Time, from, to time.Time, loc *time.Location) Profile {
	if loc == nil {
		loc = time.Local
	}
	type dayKey struct {
		year  int
		month time.Month
		day   int
	}
	hoursByDay := map[dayKey]*[24]bool{}
	var profile Profile
	for _, event := range events {
		if event.Before(from) || !event.Before(to) {
			continue
		}
		profile.Events++
		local := event.In(loc)
		key := dayKey{local.Year(), local.Month(), local.Day()}
		hours := hoursByDay[key]
		if hours == nil {
			hours = &[24]bool{}
			hoursByDay[key] = hours
		}
		hours[local.Hour()] = true
	}
	profile.ActiveDays = len(hoursByDay)
	if profile.ActiveDays == 0 {
		return profile
	}
	for _, hours := range hoursByDay {
		for hour, active := range hours {
			if active {
				profile.Hours[hour]++
			}
		}
	}
	for hour := range profile.Hours {
		profile.Hours[hour] /= float64(profile.ActiveDays)
	}
	return profile
}

// ExpectedActiveHours integrates the profile over [from, to): each slice of a
// local clock hour contributes its length in hours times the fraction of
// baseline days that were active in that hour. The result is the number of
// hours in the interval that would normally have carried traffic, so it never
// exceeds the interval's length in hours.
func (p Profile) ExpectedActiveHours(from, to time.Time, loc *time.Location) float64 {
	if loc == nil {
		loc = time.Local
	}
	if !to.After(from) {
		return 0
	}
	var expected float64
	for cursor := from; cursor.Before(to); {
		local := cursor.In(loc)
		// Step to the next local hour boundary or the next zone change,
		// whichever comes first. Local minutes and seconds keep half-hour and
		// 45-minute zones aligned to their own clock hours; stopping at zone
		// changes keeps a DST jump off the hour (Chatham springs from 02:45 to
		// 03:45) from crediting the far side of the jump to the near hour.
		// Every step is positive, so the loop always ends.
		toBoundary := time.Hour -
			time.Duration(local.Minute())*time.Minute -
			time.Duration(local.Second())*time.Second -
			time.Duration(local.Nanosecond())
		next := cursor.Add(toBoundary)
		if _, zoneEnd := local.ZoneBounds(); !zoneEnd.IsZero() && zoneEnd.After(cursor) && zoneEnd.Before(next) {
			next = zoneEnd
		}
		if next.After(to) {
			next = to
		}
		expected += next.Sub(cursor).Hours() * p.Hours[local.Hour()]
		cursor = next
	}
	return expected
}

// SilenceVerdict reports how long a platform has been quiet and whether that
// quiet is longer than its baseline can explain.
type SilenceVerdict struct {
	LastEvent           time.Time
	Silence             time.Duration
	ExpectedActiveHours float64
	BaselineActiveDays  int
	BaselineEvents      int
	// Evaluated is false when there is no last event or the baseline is too
	// thin to judge; Stalled is then always false.
	Evaluated bool
	Stalled   bool
	// Rule names the rule that flagged the stall: RuleExpectedActivity when
	// the profile predicted enough activity, otherwise RuleMaxSilence.
	Rule string
}

// EvaluateSilence judges the silence since last as of now. baseline holds the
// event times that build the profile; only those inside BaselineRange count,
// and their order does not matter.
func EvaluateSilence(
	last time.Time,
	baseline []time.Time,
	now time.Time,
	loc *time.Location,
	cfg SilenceConfig,
) SilenceVerdict {
	if last.IsZero() {
		return SilenceVerdict{}
	}
	verdict := SilenceVerdict{LastEvent: last}
	if now.After(last) {
		verdict.Silence = now.Sub(last)
	}
	from, to := BaselineRange(last, loc, cfg)
	profile := BuildProfile(baseline, from, to, loc)
	verdict.BaselineActiveDays = profile.ActiveDays
	verdict.BaselineEvents = profile.Events
	verdict.ExpectedActiveHours = profile.ExpectedActiveHours(last, last.Add(verdict.Silence), loc)
	if profile.ActiveDays < cfg.MinActiveDays || profile.ActiveDays == 0 {
		return verdict
	}
	if float64(profile.Events)/float64(profile.ActiveDays) < cfg.MinEventsPerActiveDay {
		return verdict
	}
	verdict.Evaluated = true
	switch {
	case verdict.ExpectedActiveHours >= cfg.ExpectedActiveHoursLimit:
		verdict.Stalled = true
		verdict.Rule = RuleExpectedActivity
	case cfg.MaxSilence > 0 && verdict.Silence >= cfg.MaxSilence:
		verdict.Stalled = true
		verdict.Rule = RuleMaxSilence
	}
	return verdict
}

// RoundHours rounds an expected-active-hours score for display, keeping two
// decimals so the status payload stays readable and stable across polls.
func RoundHours(hours float64) float64 {
	return math.Round(hours*100) / 100
}
