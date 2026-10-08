package freshness

import (
	"math"
	"math/rand/v2"
	"sort"
	"testing"
	"time"
)

// Tests judge in UTC so calendar days are fixed.
var testSMSPathConfig = func() SMSPathConfig {
	cfg := DefaultSMSPathConfig
	cfg.Location = time.UTC
	return cfg
}()

var smsPathNow = time.Date(2026, 10, 4, 16, 30, 0, 0, time.UTC)

// smsDailyTraffic returns a heavy texter's history from start to end: an SMS
// arrival at 08:00, 11:00, 14:00, 17:00 and 20:00 every day (five a day), and
// an RCS message every waking hour.
func smsDailyTraffic(start, end time.Time) []TransportEvent {
	var events []TransportEvent
	for day := start.Truncate(24 * time.Hour); day.Before(end); day = day.Add(24 * time.Hour) {
		for hour := 8; hour <= 22; hour++ {
			at := day.Add(time.Duration(hour)*time.Hour + 30*time.Minute)
			if !at.Before(start) && !at.After(end) {
				events = append(events, TransportEvent{At: at, Transport: TransportRCS})
			}
		}
		for _, hour := range []int{8, 11, 14, 17, 20} {
			at := day.Add(time.Duration(hour) * time.Hour)
			if !at.Before(start) && !at.After(end) {
				events = append(events, TransportEvent{At: at, Transport: TransportSMS})
			}
		}
	}
	return events
}

// outage returns 35 days of daily traffic up to lastSMS, then RCS only, every
// 30 minutes, up to rcsUntil.
func outage(lastSMS, rcsUntil time.Time) []TransportEvent {
	events := smsDailyTraffic(lastSMS.Add(-35*24*time.Hour), lastSMS.Add(-time.Minute))
	events = append(events, TransportEvent{At: lastSMS, Transport: TransportSMS})
	for at := lastSMS.Add(30 * time.Minute); !at.After(rcsUntil); at = at.Add(30 * time.Minute) {
		events = append(events, TransportEvent{At: at, Transport: TransportRCS})
	}
	return events
}

func TestEvaluateSMSPathFlagsSMSSilenceWhileRCSFlows(t *testing.T) {
	lastSMS := time.Date(2026, 10, 3, 20, 0, 0, 0, time.UTC)
	now := lastSMS.Add(30 * time.Hour)
	verdict := EvaluateSMSPath(outage(lastSMS, now), now, testSMSPathConfig)
	if !verdict.Evaluated || !verdict.Stalled || verdict.Reason != SMSPathStalled {
		t.Fatalf("verdict = %+v, want stalled", verdict)
	}
	if !verdict.LastSMS.Equal(lastSMS) || verdict.Silence != 30*time.Hour {
		t.Fatalf("last SMS %s silence %s", verdict.LastSMS, verdict.Silence)
	}
	if verdict.ActiveDays != 28 || verdict.BaselineArrivals < 130 {
		t.Fatalf("baseline: %d active days, %d arrivals", verdict.ActiveDays, verdict.BaselineArrivals)
	}
	if math.Abs(verdict.ArrivalsPerDay-5) > 0.1 || verdict.ExpectedArrivals < 6 {
		t.Fatalf("pace %.2f/day, expected %.2f", verdict.ArrivalsPerDay, verdict.ExpectedArrivals)
	}
	if verdict.RCSInWindow != 48 {
		t.Fatalf("RCS in window = %d, want 48 (every 30 minutes over 24h)", verdict.RCSInWindow)
	}
}

// At five arrivals a day, six expected arrivals take 28.8 hours, past the
// 24-hour floor: the verdict waits for the larger of the two.
func TestEvaluateSMSPathWaitsForTheWindowAndTheUsualPace(t *testing.T) {
	lastSMS := time.Date(2026, 10, 3, 20, 0, 0, 0, time.UTC)
	events := outage(lastSMS, lastSMS.Add(40*time.Hour))
	for _, tc := range []struct {
		after  time.Duration
		reason string
	}{
		{23 * time.Hour, SMSPathFlowing},
		{28 * time.Hour, SMSPathUsualPace},
		{29 * time.Hour, SMSPathStalled},
	} {
		if got := EvaluateSMSPath(events, lastSMS.Add(tc.after), testSMSPathConfig); got.Reason != tc.reason {
			t.Fatalf("%s after the last SMS: %+v, want %q", tc.after, got, tc.reason)
		}
	}
}

func TestEvaluateSMSPathDoesNotBlameSMSWhenRCSIsQuietToo(t *testing.T) {
	lastSMS := time.Date(2026, 10, 3, 20, 0, 0, 0, time.UTC)
	now := lastSMS.Add(40 * time.Hour)

	// The whole relay stopped at the last SMS.
	verdict := EvaluateSMSPath(outage(lastSMS, lastSMS), now, testSMSPathConfig)
	if verdict.Stalled || verdict.Reason != SMSPathRCSQuiet {
		t.Fatalf("relay-wide silence: %+v, want %q", verdict, SMSPathRCSQuiet)
	}
	// The relay stopped four hours ago: the RCS from before still fills the
	// window, but nothing current shows RCS flowing.
	verdict = EvaluateSMSPath(outage(lastSMS, now.Add(-4*time.Hour)), now, testSMSPathConfig)
	if verdict.Stalled || verdict.Reason != SMSPathRCSQuiet || verdict.RCSInWindow < 3 {
		t.Fatalf("stale RCS burst: %+v, want %q with a full window", verdict, SMSPathRCSQuiet)
	}
	// Two RCS messages are below the minimum of three.
	events := append(outage(lastSMS, lastSMS),
		TransportEvent{At: now.Add(-2 * time.Hour), Transport: TransportRCS},
		TransportEvent{At: now.Add(-time.Hour), Transport: TransportRCS})
	verdict = EvaluateSMSPath(events, now, testSMSPathConfig)
	if verdict.Stalled || verdict.Reason != SMSPathRCSQuiet || verdict.RCSInWindow != 2 {
		t.Fatalf("two RCS: %+v, want %q with 2", verdict, SMSPathRCSQuiet)
	}
}

// A relay that stops while SMS has been quiet for any stretch a normal day
// produces is never reported as an SMS-path failure: by the time the usual
// pace calls the SMS silence abnormal, RCS has been silent past RCSRecency
// too (review round 2 found up to 12 hours of misattribution before).
func TestEvaluateSMSPathRelayStallNeverReadsAsAnSMSFailure(t *testing.T) {
	for quiet := time.Hour; quiet <= 18*time.Hour; quiet += time.Hour {
		lastSMS := time.Date(2026, 10, 3, 20, 0, 0, 0, time.UTC)
		stop := lastSMS.Add(quiet)
		events := outage(lastSMS, stop)
		for now := stop; now.Before(stop.Add(4 * 24 * time.Hour)); now = now.Add(15 * time.Minute) {
			if verdict := EvaluateSMSPath(events, now, testSMSPathConfig); verdict.Stalled {
				t.Fatalf("relay stopped %s after the last SMS: stalled at %s (%+v)", quiet, now, verdict)
			}
		}
	}
}

func TestEvaluateSMSPathSkipsSparseOrIrregularTexters(t *testing.T) {
	now := smsPathNow
	rcs := func(events []TransportEvent) []TransportEvent {
		for at := now.Add(-48 * time.Hour); !at.After(now); at = at.Add(time.Hour) {
			events = append(events, TransportEvent{At: at, Transport: TransportRCS})
		}
		return events
	}
	sms := func(times []time.Time) []TransportEvent {
		var events []TransportEvent
		for _, at := range times {
			events = append(events, TransportEvent{At: at, Transport: TransportSMS})
		}
		return rcs(events)
	}
	if v := EvaluateSMSPath(rcs(nil), now, testSMSPathConfig); v.Reason != SMSPathNoHistory || v.Evaluated {
		t.Fatalf("RCS only: %+v", v)
	}
	lastSMS := now.Add(-30 * time.Hour)
	cases := map[string][]time.Time{}
	// One stray SMS eight days back, then a burst of nine codes.
	burst := []time.Time{lastSMS.Add(-8 * 24 * time.Hour)}
	for i := 0; i < 9; i++ {
		burst = append(burst, lastSMS.Add(-time.Duration(i)*2*time.Minute))
	}
	cases["stray plus a burst"] = burst
	// Five SMS every ten days.
	var tenDay []time.Time
	for c := 0; c < 3; c++ {
		for i := 0; i < 5; i++ {
			tenDay = append(tenDay, lastSMS.Add(-time.Duration(c)*10*24*time.Hour-time.Duration(i)*time.Minute))
		}
	}
	cases["clusters ten days apart"] = tenDay
	// A weekly cluster of six.
	var weekly []time.Time
	for w := 0; w < 4; w++ {
		for i := 0; i < 6; i++ {
			weekly = append(weekly, lastSMS.Add(-time.Duration(w)*7*24*time.Hour-time.Duration(i)*40*time.Minute))
		}
	}
	cases["weekly cluster"] = weekly
	for name, times := range cases {
		if v := EvaluateSMSPath(sms(times), now, testSMSPathConfig); v.Stalled || v.Reason != SMSPathThinBaseline {
			t.Errorf("%s: %+v, want %q", name, v, SMSPathThinBaseline)
		}
	}
	// One SMS a day is judged, and 30 quiet hours are within its pace.
	var daily []time.Time
	for d := 0; d < 30; d++ {
		daily = append(daily, lastSMS.Add(-time.Duration(d)*24*time.Hour))
	}
	if v := EvaluateSMSPath(sms(daily), now, testSMSPathConfig); v.Stalled || v.Reason != SMSPathUsualPace {
		t.Errorf("one SMS a day, 30h quiet: %+v, want %q", v, SMSPathUsualPace)
	}
}

// weekdayTexter gets SMS at 09:15, 13:15 and 17:15 on weekdays only, RCS every
// waking hour every day, from start to end, with SMS skipped on holidays.
func weekdayTexter(start, end time.Time, holidays map[time.Time]bool) []TransportEvent {
	var events []TransportEvent
	for day := start.Truncate(24 * time.Hour); day.Before(end); day = day.Add(24 * time.Hour) {
		for hour := 8; hour <= 22; hour++ {
			events = append(events, TransportEvent{At: day.Add(time.Duration(hour)*time.Hour + 5*time.Minute), Transport: TransportRCS})
		}
		if day.Weekday() == time.Saturday || day.Weekday() == time.Sunday || holidays[day] {
			continue
		}
		for _, hour := range []int{9, 13, 17} {
			events = append(events, TransportEvent{At: day.Add(time.Duration(hour)*time.Hour + 15*time.Minute), Transport: TransportSMS})
		}
	}
	return events
}

// countEpisodes walks the clock in steps and counts distinct stalled episodes.
func countEpisodes(events []TransportEvent, from, to time.Time, step time.Duration, cfg SMSPathConfig) int {
	sort.Slice(events, func(i, j int) bool { return events[i].At.Before(events[j].At) })
	episodes := map[time.Time]bool{}
	for now := from; now.Before(to); now = now.Add(step) {
		end := sort.Search(len(events), func(i int) bool { return events[i].At.After(now) })
		lo := sort.Search(len(events), func(i int) bool { return !events[i].At.Before(now.Add(-42 * 24 * time.Hour)) })
		if verdict := EvaluateSMSPath(events[lo:end], now, cfg); verdict.Stalled {
			episodes[verdict.LastSMS] = true
		}
	}
	return len(episodes)
}

// A weekday-only texter has SMS on 20 of any 28 days and is never judged, so
// weekends and holiday Mondays never alarm (review round 2 found 21 of 23
// weekends flagged under the previous rule).
func TestEvaluateSMSPathNeverFlagsAWeekdayTexter(t *testing.T) {
	start := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)
	end := start.Add(26 * 7 * 24 * time.Hour)
	holidays := map[time.Time]bool{
		time.Date(2026, 1, 19, 0, 0, 0, 0, time.UTC): true,
		time.Date(2026, 2, 16, 0, 0, 0, 0, time.UTC): true,
		time.Date(2026, 5, 25, 0, 0, 0, 0, time.UTC): true,
	}
	events := weekdayTexter(start, end, holidays)
	if n := countEpisodes(events, start.Add(35*24*time.Hour), end, time.Hour, testSMSPathConfig); n != 0 {
		t.Fatalf("weekday texter: %d false episodes over 26 weeks", n)
	}
}

// poissonTexter draws memoryless SMS arrivals in waking hours (08:00-23:00) at
// rate arrivals a day, a fifth of them bursts of up to four extra texts within
// five minutes, and RCS every waking hour.
func poissonTexter(r *rand.Rand, rate float64, start time.Time, days int) []TransportEvent {
	var events []TransportEvent
	for d := 0; d < days; d++ {
		day := start.Add(time.Duration(d) * 24 * time.Hour)
		for hour := 8; hour <= 22; hour++ {
			events = append(events, TransportEvent{At: day.Add(time.Duration(hour) * time.Hour), Transport: TransportRCS})
		}
		n, limit, p := 0, math.Exp(-rate), r.Float64()
		for p >= limit {
			n++
			p *= r.Float64()
		}
		for i := 0; i < n; i++ {
			at := day.Add(8*time.Hour + time.Duration(r.Int64N(int64(15*time.Hour))))
			events = append(events, TransportEvent{At: at, Transport: TransportSMS})
			if r.IntN(5) == 0 {
				for k := r.IntN(4); k >= 0; k-- {
					events = append(events, TransportEvent{At: at.Add(time.Duration(r.Int64N(int64(5 * time.Minute)))), Transport: TransportSMS})
				}
			}
		}
	}
	return events
}

// Memoryless daily texters are the hard case: no rule can tell a long random
// lull from an outage. Over a simulated year each, the default rule stays
// within the false-alarm rates documented on DefaultSMSPathConfig.
func TestEvaluateSMSPathFalseAlarmRateForMemorylessTexters(t *testing.T) {
	if testing.Short() {
		t.Skip("simulation")
	}
	r := rand.New(rand.NewPCG(20261008, 6))
	start := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)
	total := 0
	for _, rate := range []float64{1, 2, 3, 4, 6, 10} {
		events := poissonTexter(r, rate, start, 365)
		n := countEpisodes(events, start.Add(35*24*time.Hour), start.Add(365*24*time.Hour), time.Hour, testSMSPathConfig)
		t.Logf("%.0f arrivals a day: %d false episodes in 330 days", rate, n)
		if n > 5 {
			t.Errorf("%.0f arrivals a day: %d false episodes in 330 days, want at most 5", rate, n)
		}
		total += n
	}
	if total > 15 {
		t.Errorf("%d false episodes across six texters in 330 days each", total)
	}
}

// A four-day outage ends; ten days later SMS stops again. The first outage's
// quiet days stay inside the 28-day baseline, but 24 of 28 days still had
// SMS, and the second outage is flagged within a day and a half.
func TestEvaluateSMSPathAnEarlierOutageDoesNotBlockTheNext(t *testing.T) {
	firstStart := time.Date(2026, 10, 3, 20, 0, 0, 0, time.UTC)
	recovery := firstStart.Add(4 * 24 * time.Hour)
	secondLast := recovery.Add(10 * 24 * time.Hour)
	events := smsDailyTraffic(firstStart.Add(-30*24*time.Hour), firstStart)
	events = append(events, smsDailyTraffic(recovery, secondLast.Add(time.Minute))...)
	for at := firstStart; at.Before(secondLast.Add(3 * 24 * time.Hour)); at = at.Add(30 * time.Minute) {
		events = append(events, TransportEvent{At: at, Transport: TransportRCS})
	}
	var first time.Time
	var lastVerdict SMSPathVerdict
	for now := secondLast; now.Before(secondLast.Add(3 * 24 * time.Hour)); now = now.Add(15 * time.Minute) {
		lastVerdict = EvaluateSMSPath(events, now, testSMSPathConfig)
		if lastVerdict.Stalled {
			first = now
			break
		}
	}
	if first.IsZero() {
		t.Fatalf("second outage never flagged: %+v", lastVerdict)
	}
	if lastSMS := lastVerdict.LastSMS; first.Sub(lastSMS) > 36*time.Hour {
		t.Fatalf("second outage flagged %s after its last SMS, want within 36h", first.Sub(lastSMS))
	}
}

func TestEvaluateSMSPathIgnoresUnknownTransportAndFutureEvents(t *testing.T) {
	lastSMS := time.Date(2026, 10, 3, 20, 0, 0, 0, time.UTC)
	now := lastSMS.Add(30 * time.Hour)
	events := outage(lastSMS, now)
	base := EvaluateSMSPath(events, now, testSMSPathConfig)
	noisy := append(append([]TransportEvent(nil), events...),
		TransportEvent{At: now.Add(-time.Hour), Transport: TransportUnknown},
		TransportEvent{At: now.Add(time.Minute), Transport: TransportSMS},
		TransportEvent{At: now.Add(time.Hour), Transport: TransportRCS},
	)
	if got := EvaluateSMSPath(noisy, now, testSMSPathConfig); got != base {
		t.Fatalf("verdict with unknown and future events = %+v, want %+v", got, base)
	}
}

// TestEvaluateSMSPathEpisodeIdentityIsStable walks the clock through a
// simulated 95-hour outage and checks that every stalled verdict names the
// same last SMS, so a once-per-episode alert keyed on it fires exactly once.
func TestEvaluateSMSPathEpisodeIdentityIsStable(t *testing.T) {
	lastSMS := time.Date(2026, 10, 3, 20, 16, 47, 0, time.UTC)
	recovery := lastSMS.Add(95 * time.Hour)
	events := append(outage(lastSMS, recovery), TransportEvent{At: recovery, Transport: TransportSMS})
	episodes := map[time.Time]bool{}
	for now := lastSMS; now.Before(recovery.Add(2 * time.Hour)); now = now.Add(5 * time.Minute) {
		verdict := EvaluateSMSPath(events, now, testSMSPathConfig)
		if verdict.Stalled {
			episodes[verdict.LastSMS] = true
		}
		if !now.Before(recovery) && verdict.Stalled {
			t.Fatalf("still stalled at %s after the SMS at %s", now, recovery)
		}
	}
	if len(episodes) != 1 || !episodes[lastSMS] {
		t.Fatalf("episodes = %v, want exactly one keyed on %s", episodes, lastSMS)
	}
}

// randomEvents draws a mix of SMS, RCS and unknown events around now.
func randomEvents(r *rand.Rand, now time.Time) []TransportEvent {
	n := r.IntN(120)
	events := make([]TransportEvent, 0, n)
	for i := 0; i < n; i++ {
		offset := time.Duration(r.Int64N(int64(45*24*time.Hour))) - 43*24*time.Hour
		events = append(events, TransportEvent{At: now.Add(offset).Truncate(time.Minute), Transport: Transport(r.IntN(3))})
	}
	if r.IntN(2) == 0 {
		cut := now.Add(-time.Duration(r.Int64N(int64(72 * time.Hour))))
		kept := events[:0]
		for _, event := range events {
			if event.Transport != TransportSMS || event.At.Before(cut) {
				kept = append(kept, event)
			}
		}
		events = kept
	}
	return events
}

func randomConfig(r *rand.Rand) SMSPathConfig {
	return SMSPathConfig{
		Window:              time.Duration(1+r.IntN(48)) * time.Hour,
		MinExpectedArrivals: float64(r.IntN(10)),
		ArrivalGap:          time.Duration(r.IntN(120)) * time.Minute,
		BaselineDays:        1 + r.IntN(35),
		MinActiveDays:       r.IntN(30),
		MinBaselineArrivals: r.IntN(15),
		MinRCSInWindow:      r.IntN(6),
		RCSRecency:          time.Duration(1+r.IntN(30)) * time.Hour,
		Location:            time.UTC,
	}
}

// outageShaped draws a history in the region the default rule judges: weeks
// of SMS at a random daily pace (some days skipped), then SMS silence of a
// random length while RCS arrives at a random rate, possibly stopping.
func outageShaped(r *rand.Rand, now time.Time) []TransportEvent {
	var events []TransportEvent
	lastSMS := now.Add(-time.Duration(r.Int64N(int64(80 * time.Hour))))
	perDay := 1 + r.IntN(10)
	skip := r.Float64() * 0.4
	for d := 0; d < 35; d++ {
		day := lastSMS.Add(-time.Duration(d) * 24 * time.Hour)
		if d > 0 && r.Float64() < skip {
			continue
		}
		for i := 0; i < perDay; i++ {
			at := day.Add(-time.Duration(r.Int64N(int64(20 * time.Hour))))
			if d == 0 && i == 0 {
				at = day
			}
			events = append(events, TransportEvent{At: at, Transport: TransportSMS})
		}
	}
	rcsGap := time.Duration(1+r.IntN(240)) * time.Minute
	rcsStop := now.Add(-time.Duration(r.Int64N(int64(12 * time.Hour))))
	for at := lastSMS.Add(-30 * 24 * time.Hour); at.Before(now); at = at.Add(rcsGap) {
		if at.Before(rcsStop) || r.IntN(2) == 0 {
			events = append(events, TransportEvent{At: at, Transport: TransportRCS})
		}
	}
	return events
}

// referenceSMSPathStalled restates the stalled rule as directly as possible,
// sharing no code with EvaluateSMSPath, for a differential check.
func referenceSMSPathStalled(events []TransportEvent, now time.Time, cfg SMSPathConfig) bool {
	var sms, rcs []time.Time
	for _, e := range events {
		if e.At.After(now) {
			continue
		}
		if e.Transport == TransportSMS {
			sms = append(sms, e.At)
		} else if e.Transport == TransportRCS {
			rcs = append(rcs, e.At)
		}
	}
	if len(sms) == 0 {
		return false
	}
	sort.Slice(sms, func(i, j int) bool { return sms[i].Before(sms[j]) })
	last := sms[len(sms)-1]
	// Calendar days by a day number rather than time.Date normalization.
	dayIndex := func(at time.Time) int {
		at = at.In(cfg.Location)
		return int(time.Date(at.Year(), at.Month(), at.Day(), 12, 0, 0, 0, time.UTC).Unix() / 86400)
	}
	lastIndex := dayIndex(last)
	var base []time.Time
	active := map[int]bool{}
	for _, at := range sms {
		if lastIndex-dayIndex(at) < cfg.BaselineDays {
			base = append(base, at)
			active[dayIndex(at)] = true
		}
	}
	arrivals := 0
	for i := range base {
		if i == 0 || base[i].Sub(base[i-1]) > cfg.ArrivalGap {
			arrivals++
		}
	}
	if arrivals < cfg.MinBaselineArrivals || len(active) < cfg.MinActiveDays {
		return false
	}
	silence := now.Sub(last)
	if silence < cfg.Window {
		return false
	}
	expected := 0.0
	if span := last.Sub(base[0]); arrivals > 1 && span > 0 {
		expected = float64(arrivals-1) / span.Hours() * silence.Hours()
	}
	if expected < cfg.MinExpectedArrivals {
		return false
	}
	in := 0
	var newest time.Time
	for _, at := range rcs {
		if at.After(now.Add(-cfg.Window)) {
			in++
		}
		if at.After(newest) {
			newest = at
		}
	}
	return in >= cfg.MinRCSInWindow && !newest.IsZero() && now.Sub(newest) <= cfg.RCSRecency
}

// TestEvaluateSMSPathMatchesTheReferenceRule compares EvaluateSMSPath with the
// restatement on outage-shaped histories, mostly under the defaults, and
// requires enough of both verdicts that the comparison is not vacuous.
func TestEvaluateSMSPathMatchesTheReferenceRule(t *testing.T) {
	r := rand.New(rand.NewPCG(20261008, 3))
	stalled, clear := 0, 0
	for i := 0; i < 3000; i++ {
		events := outageShaped(r, smsPathNow)
		cfg := testSMSPathConfig
		if i%4 == 3 {
			cfg = randomConfig(r)
		}
		got := EvaluateSMSPath(events, smsPathNow, cfg)
		if want := referenceSMSPathStalled(events, smsPathNow, cfg); got.Stalled != want {
			t.Fatalf("case %d: stalled = %v, reference says %v: %+v cfg %+v", i, got.Stalled, want, got, cfg)
		}
		if i%4 != 3 {
			if got.Stalled {
				stalled++
			} else {
				clear++
			}
		}
	}
	if stalled < 150 || clear < 150 {
		t.Fatalf("defaults judged %d stalled and %d clear histories; the generator no longer exercises both", stalled, clear)
	}
}

// TestEvaluateSMSPathProperties checks invariants that must hold for every
// input, over seeded random histories.
func TestEvaluateSMSPathProperties(t *testing.T) {
	r := rand.New(rand.NewPCG(20261007, 1))
	for i := 0; i < 4000; i++ {
		cfg := randomConfig(r)
		var events []TransportEvent
		if i%2 == 0 {
			cfg = testSMSPathConfig
			events = outageShaped(r, smsPathNow)
		} else {
			events = randomEvents(r, smsPathNow)
		}
		verdict := EvaluateSMSPath(events, smsPathNow, cfg)

		// A stalled verdict satisfies every clause of the rule.
		if verdict.Stalled {
			if !verdict.Evaluated ||
				verdict.Silence < cfg.Window ||
				verdict.ExpectedArrivals < cfg.MinExpectedArrivals ||
				verdict.ActiveDays < cfg.MinActiveDays ||
				verdict.BaselineArrivals < cfg.MinBaselineArrivals ||
				verdict.RCSInWindow < cfg.MinRCSInWindow ||
				verdict.LastRCS.IsZero() ||
				smsPathNow.Sub(verdict.LastRCS) > cfg.RCSRecency ||
				verdict.Reason != SMSPathStalled {
				t.Fatalf("case %d: stalled verdict breaks the rule: %+v cfg %+v", i, verdict, cfg)
			}
		}
		if verdict.Reason == "" || (!verdict.Evaluated && verdict.Stalled) {
			t.Fatalf("case %d: inconsistent verdict %+v", i, verdict)
		}
		if verdict.BaselineArrivals > verdict.BaselineSMS || verdict.ActiveDays > verdict.BaselineSMS {
			t.Fatalf("case %d: more arrivals or days than SMS: %+v", i, verdict)
		}

		// Order does not matter.
		shuffled := append([]TransportEvent(nil), events...)
		r.Shuffle(len(shuffled), func(a, b int) { shuffled[a], shuffled[b] = shuffled[b], shuffled[a] })
		if got := EvaluateSMSPath(shuffled, smsPathNow, cfg); got != verdict {
			t.Fatalf("case %d: shuffled verdict %+v != %+v", i, got, verdict)
		}

		// Only elapsed time matters: shifting everything by whole days (UTC,
		// so calendar days shift with it) changes nothing but the times.
		shift := time.Duration(r.IntN(400)-200) * 24 * time.Hour
		moved := make([]TransportEvent, len(events))
		for j, event := range events {
			moved[j] = TransportEvent{At: event.At.Add(shift), Transport: event.Transport}
		}
		got := EvaluateSMSPath(moved, smsPathNow.Add(shift), cfg)
		if got.Stalled != verdict.Stalled || got.Reason != verdict.Reason || got.Silence != verdict.Silence ||
			got.ActiveDays != verdict.ActiveDays || got.BaselineArrivals != verdict.BaselineArrivals {
			t.Fatalf("case %d: shifted verdict %+v != %+v", i, got, verdict)
		}

		// A fresh incoming SMS inside the window always clears a stall.
		recovered := append(append([]TransportEvent(nil), events...),
			TransportEvent{At: smsPathNow.Add(-time.Duration(r.Int64N(int64(cfg.Window)))), Transport: TransportSMS})
		if got := EvaluateSMSPath(recovered, smsPathNow, cfg); got.Stalled {
			t.Fatalf("case %d: stalled after an SMS inside the window: %+v", i, got)
		}

		// More RCS evidence never clears a stall.
		if verdict.Stalled {
			more := append(append([]TransportEvent(nil), events...),
				TransportEvent{At: smsPathNow.Add(-time.Duration(r.Int64N(int64(40 * time.Hour)))), Transport: TransportRCS})
			if got := EvaluateSMSPath(more, smsPathNow, cfg); !got.Stalled {
				t.Fatalf("case %d: an extra RCS message cleared a stall: %+v", i, got)
			}
		}

		// Events after now are invisible.
		future := append(append([]TransportEvent(nil), events...),
			TransportEvent{At: smsPathNow.Add(time.Duration(1 + r.Int64N(int64(48*time.Hour)))), Transport: Transport(r.IntN(3))})
		if got := EvaluateSMSPath(future, smsPathNow, cfg); got != verdict {
			t.Fatalf("case %d: a future event changed the verdict: %+v != %+v", i, got, verdict)
		}
	}
}

func FuzzEvaluateSMSPath(f *testing.F) {
	f.Add(uint64(1), uint64(2))
	f.Add(uint64(20261003), uint64(1616))
	f.Fuzz(func(t *testing.T, seed1, seed2 uint64) {
		r := rand.New(rand.NewPCG(seed1, seed2))
		cfg := randomConfig(r)
		events := outageShaped(r, smsPathNow)
		if r.IntN(2) == 0 {
			events = randomEvents(r, smsPathNow)
		}
		verdict := EvaluateSMSPath(events, smsPathNow, cfg)
		if verdict.Stalled != referenceSMSPathStalled(events, smsPathNow, cfg) {
			t.Fatalf("stalled = %v disagrees with the reference: %+v cfg %+v", verdict.Stalled, verdict, cfg)
		}
	})
}
