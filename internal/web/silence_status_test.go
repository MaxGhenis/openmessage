package web

import (
	"context"
	"encoding/json"
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
// phone_responding the whole time. Judged against its own baseline, eight
// silent hours that are normally all active are a stall.
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

// The baseline is read once per last event; re-judging a silent platform on
// every refresh reuses it.
func TestAddSilenceCachesBaselineUntilANewEvent(t *testing.T) {
	now := time.Now()
	last := now.Add(-3 * time.Hour)
	activity := &stubActivity{events: map[string][]time.Time{"google": steadyTraffic(last)}}
	cache := silenceBaselineCache{}
	for i := 0; i < 3; i++ {
		out := map[string]any{"google": map[string]any{"stale": false, "stale_reason": ""}}
		addSilence(out, activity, cache, now.Add(time.Duration(i)*time.Minute), time.Local)
	}
	if activity.calls != 1 {
		t.Fatalf("baseline reads = %d, want 1", activity.calls)
	}
	activity.events["google"] = append(activity.events["google"], now)
	out := map[string]any{"google": map[string]any{"stale": false, "stale_reason": ""}}
	addSilence(out, activity, cache, now, time.Local)
	if activity.calls != 2 {
		t.Fatalf("baseline reads after a new event = %d, want 2", activity.calls)
	}
}
