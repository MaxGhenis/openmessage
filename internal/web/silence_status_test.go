package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/freshness"
)

// stubActivity serves fixed per-platform activity times as the freshness
// activity source.
type stubActivity struct {
	events map[string][]time.Time
	calls  int
}

func (s *stubActivity) Name() string { return freshness.SourceV2Inbox }

func (s *stubActivity) Latest(context.Context) (map[string]time.Time, error) {
	latest := map[string]time.Time{}
	for platform, events := range s.events {
		for _, at := range events {
			if at.After(latest[platform]) {
				latest[platform] = at
			}
		}
	}
	return latest, nil
}

func (s *stubActivity) Between(_ context.Context, platform string, from, to time.Time) ([]time.Time, error) {
	s.calls++
	var out []time.Time
	for _, at := range s.events[platform] {
		if !at.Before(from) && !at.After(to) {
			out = append(out, at)
		}
	}
	return out, nil
}

// steadyTraffic is one event every half hour, every hour of the day, from 16
// days before last up to and including last. Every clock hour is active on
// every baseline day, so the expected-active-hours score equals the silent
// hours in any zone the test runs in.
func steadyTraffic(last time.Time) []time.Time {
	var events []time.Time
	for at := last.Add(-16 * 24 * time.Hour); !at.After(last); at = at.Add(30 * time.Minute) {
		events = append(events, at)
	}
	return events
}

func fetchFreshness(t *testing.T, opts APIOptions, store *db.Store) map[string]any {
	t.Helper()
	srv := httptest.NewServer(APIHandlerWithOptions(store, nil, zerolog.Nop(), nil, opts))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	fresh, ok := payload["freshness"].(map[string]any)
	if !ok {
		t.Fatalf("status payload has no freshness block: %v", payload)
	}
	return fresh
}

func newStatusTestStore(t *testing.T) *db.Store {
	t.Helper()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// The 2026-10-06 incident: Google ingest went silent at 01:11 for 38 hours.
// Google was still the newest platform, so the relative rule kept it at
// behind_days 0 and stale=false while /api/status reported connected and
// phone_responding apart from brief reconnects. Judged against its own
// baseline, eight silent hours that are normally all active are a stall.
func TestStatusFreshnessFlagsSilentGoogleIngest(t *testing.T) {
	now := time.Now()
	googleLast := now.Add(-8 * time.Hour)
	activity := &stubActivity{events: map[string][]time.Time{"google": steadyTraffic(googleLast)}}
	reads := stubReads{stats: []db.PlatformStat{
		{Platform: "sms", Count: 100, LatestMS: googleLast.UnixMilli(), LatestRecvMS: googleLast.UnixMilli()},
		{Platform: "whatsapp", Count: 10, LatestMS: googleLast.Add(-time.Hour).UnixMilli()},
	}}

	fresh := fetchFreshness(t, APIOptions{Reads: reads, Activity: activity}, newStatusTestStore(t))

	google := fresh["google"].(map[string]any)
	if google["behind_days"].(float64) != 0 {
		t.Fatalf("google behind_days = %v, want 0 (it is the newest platform)", google["behind_days"])
	}
	if google["stale"] != true || google["stale_reason"] != "silent" {
		t.Fatalf("google stale = %v (%v), want true (silent)", google["stale"], google["stale_reason"])
	}
	silence := google["silence"].(map[string]any)
	if silence["stalled"] != true || silence["evaluated"] != true || silence["rule"] != freshness.RuleExpectedActivity {
		t.Fatalf("google silence = %v, want evaluated stall by %s", silence, freshness.RuleExpectedActivity)
	}
	if got := silence["expected_active_hours"].(float64); got < 7.99 || got > 8.01 {
		t.Fatalf("expected_active_hours = %v, want 8 (every silent hour is normally active)", got)
	}
	if got := int64(silence["last_event_ms"].(float64)); got != googleLast.UnixMilli() {
		t.Fatalf("last_event_ms = %d, want %d", got, googleLast.UnixMilli())
	}
	if silence["source"] != freshness.SourceV2Inbox || silence["baseline_active_days"].(float64) != 14 {
		t.Fatalf("silence source/baseline = %v/%v, want %s/14", silence["source"], silence["baseline_active_days"], freshness.SourceV2Inbox)
	}
	if fresh["silence_stalled"] != true {
		t.Fatalf("top-level silence_stalled = %v, want true", fresh["silence_stalled"])
	}
	// WhatsApp has no activity source data: its entry keeps the relative
	// verdict and carries no silence block.
	whatsapp := fresh["whatsapp"].(map[string]any)
	if whatsapp["stale"] != false || whatsapp["stale_reason"] != "" || whatsapp["silence"] != nil {
		t.Fatalf("whatsapp entry = %v, want not stale and no silence block", whatsapp)
	}
}

// A quiet hour is not a stall, and the verdict clears with the next event.
func TestStatusFreshnessKeepsRecentGoogleFresh(t *testing.T) {
	now := time.Now()
	googleLast := now.Add(-90 * time.Minute)
	activity := &stubActivity{events: map[string][]time.Time{"google": steadyTraffic(googleLast)}}
	reads := stubReads{stats: []db.PlatformStat{{Platform: "sms", Count: 100, LatestMS: googleLast.UnixMilli()}}}

	fresh := fetchFreshness(t, APIOptions{Reads: reads, Activity: activity}, newStatusTestStore(t))

	google := fresh["google"].(map[string]any)
	if google["stale"] != false || google["stale_reason"] != "" {
		t.Fatalf("google stale = %v (%v), want false", google["stale"], google["stale_reason"])
	}
	silence := google["silence"].(map[string]any)
	if silence["stalled"] != false || silence["evaluated"] != true {
		t.Fatalf("google silence = %v, want evaluated and not stalled", silence)
	}
	if fresh["silence_stalled"] != false {
		t.Fatalf("top-level silence_stalled = %v, want false", fresh["silence_stalled"])
	}
}

// A platform days behind the others is "behind" even when it is also silent:
// the relative verdict names the likelier cause (logged out, unpaired).
func TestStatusFreshnessBehindOutranksSilent(t *testing.T) {
	now := time.Now()
	signalLast := now.Add(-5 * 24 * time.Hour)
	activity := &stubActivity{events: map[string][]time.Time{
		"signal": steadyTraffic(signalLast),
		"google": steadyTraffic(now.Add(-time.Minute)),
	}}
	reads := stubReads{stats: []db.PlatformStat{
		{Platform: "sms", Count: 100, LatestMS: now.Add(-time.Minute).UnixMilli()},
		{Platform: "signal", Count: 100, LatestMS: signalLast.UnixMilli()},
	}}

	fresh := fetchFreshness(t, APIOptions{Reads: reads, Activity: activity}, newStatusTestStore(t))

	signal := fresh["signal"].(map[string]any)
	if signal["stale"] != true || signal["stale_reason"] != "behind" {
		t.Fatalf("signal stale = %v (%v), want true (behind)", signal["stale"], signal["stale_reason"])
	}
	if signal["silence"].(map[string]any)["stalled"] != true {
		t.Fatalf("signal silence = %v, want stalled", signal["silence"])
	}
	google := fresh["google"].(map[string]any)
	if google["stale"] != false {
		t.Fatalf("google entry = %v, want fresh", google)
	}
	// Signal's silence verdict is stalled but its reason is "behind", so the
	// top-level flag stays clear: a platform dead for weeks must not hold it
	// true while the live ones are fine. (Review of PR #190.)
	if fresh["silence_stalled"] != false {
		t.Fatalf("silence_stalled = %v, want false when only a behind platform is stalled", fresh["silence_stalled"])
	}
}

// Without an activity source the status payload keeps its previous shape.
func TestStatusFreshnessWithoutActivitySourceHasNoSilence(t *testing.T) {
	now := time.Now()
	reads := stubReads{stats: []db.PlatformStat{{Platform: "sms", Count: 1, LatestMS: now.UnixMilli()}}}
	fresh := fetchFreshness(t, APIOptions{Reads: reads}, newStatusTestStore(t))
	if _, ok := fresh["silence_stalled"]; ok {
		t.Fatalf("silence_stalled present without an activity source: %v", fresh)
	}
	google := fresh["google"].(map[string]any)
	if _, ok := google["silence"]; ok {
		t.Fatalf("google silence present without an activity source: %v", google)
	}
	if google["stale_reason"] != "" {
		t.Fatalf("google stale_reason = %v, want empty", google["stale_reason"])
	}
}

// The baseline window depends only on the date of the last event, so new
// events on the same day reuse the cached baseline and a new day reads it once.
func TestAddSilenceCachesBaselinePerWindow(t *testing.T) {
	loc := time.UTC
	last := time.Date(2026, 10, 6, 1, 11, 7, 0, loc)
	activity := &stubActivity{events: map[string][]time.Time{"google": steadyTraffic(last)}}
	cache := silenceBaselineCache{}
	judge := func(now time.Time) map[string]any {
		out := map[string]any{"google": map[string]any{"stale": false, "stale_reason": ""}}
		addSilence(out, activity, cache, nil, now, loc)
		return out
	}
	for i := 0; i < 3; i++ {
		judge(last.Add(time.Duration(i+1) * time.Hour))
	}
	if activity.calls != 1 {
		t.Fatalf("baseline reads for one silence = %d, want 1", activity.calls)
	}
	activity.events["google"] = append(activity.events["google"], last.Add(10*time.Hour))
	judge(last.Add(11 * time.Hour))
	if activity.calls != 1 {
		t.Fatalf("baseline reads after a same-day event = %d, want 1", activity.calls)
	}
	activity.events["google"] = append(activity.events["google"], last.Add(30*time.Hour))
	judge(last.Add(31 * time.Hour))
	if activity.calls != 2 {
		t.Fatalf("baseline reads after a next-day event = %d, want 2", activity.calls)
	}
}

// flakyActivity fails its queries on demand.
type flakyActivity struct {
	stubActivity
	failLatest  bool
	failBetween bool
}

func (f *flakyActivity) Latest(ctx context.Context) (map[string]time.Time, error) {
	if f.failLatest {
		return nil, errors.New("database is locked")
	}
	return f.stubActivity.Latest(ctx)
}

func (f *flakyActivity) Between(ctx context.Context, platform string, from, to time.Time) ([]time.Time, error) {
	if f.failBetween {
		return nil, errors.New("database is locked")
	}
	return f.stubActivity.Between(ctx, platform, from, to)
}

// A failing activity query must not clear a stall that is still going on: the
// platform keeps its last verdict, marked carried_over. (Review of PR #190:
// one query error turned an eight-hour stall back into stale=false.)
func TestAddSilenceKeepsLastVerdictWhenQueriesFail(t *testing.T) {
	loc := time.UTC
	now := time.Date(2026, 10, 6, 14, 0, 0, 0, loc)
	last := now.Add(-8 * time.Hour)
	fresh := func() map[string]any {
		return map[string]any{"google": map[string]any{"behind_days": 0, "stale": false, "stale_reason": ""}}
	}
	activity := &flakyActivity{stubActivity: stubActivity{events: map[string][]time.Time{"google": steadyTraffic(last)}}}

	first := fresh()
	addSilence(first, activity, silenceBaselineCache{}, nil, now, loc)
	if first["google"].(map[string]any)["stale_reason"] != "silent" {
		t.Fatalf("first verdict = %v, want a silent stall", first["google"])
	}

	for _, tc := range []struct {
		name            string
		latest, between bool
	}{
		{"latest fails", true, false},
		{"baseline read fails", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			activity.failLatest, activity.failBetween = tc.latest, tc.between
			out := fresh()
			// An empty cache forces the baseline read in the second case.
			addSilence(out, activity, silenceBaselineCache{}, first, now.Add(time.Minute), loc)
			google := out["google"].(map[string]any)
			if google["stale"] != true || google["stale_reason"] != "silent" {
				t.Fatalf("google after a failed query = %v, want the stall kept", google)
			}
			silence := google["silence"].(map[string]any)
			if silence["carried_over"] != true || silence["stalled"] != true {
				t.Fatalf("silence block = %v, want the previous stalled block marked carried_over", silence)
			}
			if out["silence_stalled"] != true {
				t.Fatalf("silence_stalled = %v, want true", out["silence_stalled"])
			}
		})
	}

	// With no previous verdict there is nothing to keep, and nothing is invented.
	activity.failLatest = true
	out := fresh()
	addSilence(out, activity, silenceBaselineCache{}, nil, now, loc)
	if google := out["google"].(map[string]any); google["stale"] != false || google["silence"] != nil {
		t.Fatalf("google with no previous verdict = %v, want untouched", google)
	}
}

// A new event ends the silence even if its baseline can't be read: the old
// stalled verdict must not be carried into the new episode. (Re-review of
// PR #190.) The new silence is judged without a baseline, so only the 72 h
// floor could fire.
func TestAddSilenceDoesNotCarryAStallPastANewEvent(t *testing.T) {
	loc := time.UTC
	now := time.Date(2026, 10, 7, 15, 31, 0, 0, loc)
	stalledAt := time.Date(2026, 10, 6, 1, 11, 0, 0, loc)
	activity := &flakyActivity{stubActivity: stubActivity{events: map[string][]time.Time{"google": steadyTraffic(stalledAt)}}}
	fresh := func() map[string]any {
		return map[string]any{"google": map[string]any{"behind_days": 0, "stale": false, "stale_reason": ""}}
	}
	first := fresh()
	addSilence(first, activity, silenceBaselineCache{}, nil, now, loc)
	if first["google"].(map[string]any)["stale_reason"] != "silent" {
		t.Fatalf("first verdict = %v, want a silent stall", first["google"])
	}

	// The phone relays again at 15:30; the baseline read for the new day fails.
	activity.events["google"] = append(activity.events["google"], now.Add(-time.Minute))
	activity.failBetween = true
	out := fresh()
	addSilence(out, activity, silenceBaselineCache{}, first, now, loc)
	google := out["google"].(map[string]any)
	silence := google["silence"].(map[string]any)
	if google["stale"] != false || silence["stalled"] != false || silence["carried_over"] != nil {
		t.Fatalf("google after a new event = %v, want fresh and not carried over", google)
	}
	if silence["baseline_unavailable"] != true || silence["last_event_ms"] != now.Add(-time.Minute).UnixMilli() {
		t.Fatalf("silence block = %v, want the new event judged without a baseline", silence)
	}
	if out["silence_stalled"] != false {
		t.Fatalf("silence_stalled = %v, want false", out["silence_stalled"])
	}
}

// A baseline read that keeps failing must not freeze the verdict: the silence
// is judged again on every refresh without a baseline, so the 72 h floor still
// fires. (Third review of PR #190: the unjudged verdict used to be carried
// forever with its first silent_ms.)
func TestAddSilenceKeepsJudgingWhileTheBaselineReadKeepsFailing(t *testing.T) {
	loc := time.UTC
	last := time.Date(2026, 10, 6, 1, 11, 0, 0, loc)
	activity := &flakyActivity{stubActivity: stubActivity{events: map[string][]time.Time{"google": steadyTraffic(last)}}, failBetween: true}
	var prev map[string]any
	for _, tc := range []struct {
		after   time.Duration
		stalled bool
	}{{time.Hour, false}, {20 * time.Hour, false}, {80 * time.Hour, true}, {200 * time.Hour, true}} {
		out := map[string]any{"google": map[string]any{"stale": false, "stale_reason": ""}}
		addSilence(out, activity, silenceBaselineCache{}, prev, last.Add(tc.after), loc)
		silence := out["google"].(map[string]any)["silence"].(map[string]any)
		if silence["stalled"] != tc.stalled || silence["baseline_unavailable"] != true || silence["carried_over"] != nil {
			t.Fatalf("at +%v: silence = %v, want stalled=%v, judged without a baseline", tc.after, silence, tc.stalled)
		}
		if got := silence["silent_ms"].(int64); got != tc.after.Milliseconds() {
			t.Fatalf("at +%v: silent_ms = %d, want %d", tc.after, got, tc.after.Milliseconds())
		}
		if tc.stalled && silence["rule"] != freshness.RuleLongSilence {
			t.Fatalf("at +%v: rule = %v, want %s", tc.after, silence["rule"], freshness.RuleLongSilence)
		}
		prev = out
	}
	// If the activity query itself starts failing too, the unjudged verdict
	// is judged again from its last event rather than dropped.
	activity.failLatest = true
	out := map[string]any{"google": map[string]any{"stale": false, "stale_reason": ""}}
	addSilence(out, activity, silenceBaselineCache{}, prev, last.Add(300*time.Hour), loc)
	silence := out["google"].(map[string]any)["silence"].(map[string]any)
	if silence["stalled"] != true || silence["silent_ms"].(int64) != (300*time.Hour).Milliseconds() {
		t.Fatalf("after Latest fails too: silence = %v, want stalled at 300 h", silence)
	}
}

// A carried verdict reports the silence up to now and the length rules apply
// to it again (here the 16 h cap for a busy platform), so a failing query can
// neither freeze the menu-bar hours nor hold a growing silence fresh.
func TestAddSilenceCarriedVerdictTracksTheSilence(t *testing.T) {
	loc := time.UTC
	last := time.Date(2026, 10, 6, 1, 11, 0, 0, loc)
	activity := &flakyActivity{stubActivity: stubActivity{events: map[string][]time.Time{
		// Busy, but only at 03:00, so the profile rule stays quiet overnight.
		"google": nil,
	}}}
	for d := 1; d <= 16; d++ {
		for i := 0; i < 30; i++ {
			activity.events["google"] = append(activity.events["google"],
				time.Date(2026, 10, 6-d, 3, i, 0, 0, loc))
		}
	}
	activity.events["google"] = append(activity.events["google"], last)
	cache := silenceBaselineCache{}
	first := map[string]any{"google": map[string]any{"stale": false, "stale_reason": ""}}
	addSilence(first, activity, cache, nil, last.Add(time.Hour), loc)
	if s := first["google"].(map[string]any)["silence"].(map[string]any); s["stalled"] != false || s["evaluated"] != true {
		t.Fatalf("first verdict = %v, want evaluated and fresh", s)
	}
	activity.failLatest = true
	for _, tc := range []struct {
		after   time.Duration
		stalled bool
		rule    string
	}{{10 * time.Hour, false, ""}, {17 * time.Hour, true, freshness.RuleMaxSilence}} {
		out := map[string]any{"google": map[string]any{"stale": false, "stale_reason": ""}}
		addSilence(out, activity, cache, first, last.Add(tc.after), loc)
		s := out["google"].(map[string]any)["silence"].(map[string]any)
		if s["carried_over"] != true || s["silent_ms"].(int64) != tc.after.Milliseconds() || s["stalled"] != tc.stalled || s["rule"] != tc.rule {
			t.Fatalf("at +%v carried: silence = %v, want carried, silent_ms current, stalled=%v rule=%q", tc.after, s, tc.stalled, tc.rule)
		}
	}
}

// The length rules a carried verdict re-applies honor MinBusyDays: a baseline
// with too few active days only gets the 72 h floor, not the 16 h cap.
func TestAddSilenceCarriedVerdictHonorsMinBusyDays(t *testing.T) {
	loc := time.UTC
	last := time.Date(2026, 10, 6, 1, 11, 0, 0, loc)
	prev := map[string]any{"google": map[string]any{
		"stale": false, "stale_reason": "",
		"silence": map[string]any{
			"source":                       freshness.SourceV2Inbox,
			"last_event_ms":                last.UnixMilli(),
			"silent_ms":                    int64(0),
			"baseline_active_days":         freshness.DefaultSilenceConfig.MinBusyDays - 1,
			"baseline_median_daily_events": float64(500),
			"evaluated":                    false,
			"stalled":                      false,
			"rule":                         "",
		},
	}}
	activity := &flakyActivity{failLatest: true}
	for _, tc := range []struct {
		after time.Duration
		rule  string
	}{{17 * time.Hour, ""}, {72 * time.Hour, freshness.RuleLongSilence}} {
		out := map[string]any{"google": map[string]any{"stale": false, "stale_reason": ""}}
		addSilence(out, activity, silenceBaselineCache{}, prev, last.Add(tc.after), loc)
		s := out["google"].(map[string]any)["silence"].(map[string]any)
		if s["carried_over"] != true || s["rule"] != tc.rule || s["stalled"] != (tc.rule != "") {
			t.Fatalf("at +%v: carried silence = %v, want rule %q", tc.after, s, tc.rule)
		}
	}
}
