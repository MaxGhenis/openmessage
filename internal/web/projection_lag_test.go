package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/readsource"
)

// stubReads serves a fixed platform-stat set as the "active read source" so a
// v2-primary daemon's freshness can be compared against the legacy store
// without standing up a whole v2 store.
type stubReads struct {
	readsource.ReadSource
	stats []db.PlatformStat
}

func (s stubReads) PlatformStats() ([]db.PlatformStat, error) { return s.stats, nil }

func (s stubReads) LatestConversationPreviews(ids []string) (map[string]string, error) {
	return map[string]string{}, nil
}

func (s stubReads) ListConversations(int) ([]*db.Conversation, error) {
	return []*db.Conversation{}, nil
}

// The 7/23–7/25 incident: the v2 Signal projection stalled while the legacy path
// kept ingesting. Freshness read only the active source, so every platform still
// reported behind_days 0 and nothing surfaced ~2 days of starvation (#155).
func TestStatusFreshnessReportsProjectionLagInV2Primary(t *testing.T) {
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	now := time.Now().UnixMilli()
	twoDaysAgo := now - 2*24*60*60*1000
	// Legacy holds current Signal traffic...
	if err := store.UpsertMessage(&db.Message{
		MessageID: "signal:fresh", ConversationID: "signal:+15550000001", Body: "current",
		TimestampMS: now, SourcePlatform: "signal", SourceID: "fresh",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertMessage(&db.Message{
		MessageID: "whatsapp:fresh", ConversationID: "whatsapp:c", Body: "current",
		TimestampMS: now, SourcePlatform: "whatsapp", SourceID: "wa-fresh",
	}); err != nil {
		t.Fatal(err)
	}
	// ...while the read source (v2) stopped receiving Signal two days ago and is
	// current on WhatsApp.
	reads := stubReads{stats: []db.PlatformStat{
		{Platform: "signal", Count: 1, LatestMS: twoDaysAgo, LatestRecvMS: twoDaysAgo},
		{Platform: "whatsapp", Count: 1, LatestMS: now, LatestRecvMS: now},
	}}

	h := APIHandlerWithOptions(store, nil, zerolog.Nop(), nil, APIOptions{
		Reads:     reads,
		V2Primary: true,
	})
	srv := httptest.NewServer(h)
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
	freshness, ok := payload["freshness"].(map[string]any)
	if !ok {
		t.Fatalf("freshness block = %v", payload["freshness"])
	}
	if stalled, _ := freshness["projection_stalled"].(bool); !stalled {
		t.Fatalf("freshness = %v, want top-level projection_stalled true", freshness)
	}
	signal, ok := freshness["signal"].(map[string]any)
	if !ok {
		t.Fatalf("signal freshness = %v", freshness["signal"])
	}
	if stalled, _ := signal["projection_stalled"].(bool); !stalled {
		t.Fatalf("signal freshness = %v, want projection_stalled true", signal)
	}
	lag, _ := signal["projection_lag_ms"].(float64)
	if int64(lag) < 24*60*60*1000 {
		t.Fatalf("signal projection_lag_ms = %v, want roughly two days", lag)
	}
	// The old signal, still reported: v2 IS two days behind in absolute terms.
	if legacyLatest, _ := signal["legacy_latest_ms"].(float64); int64(legacyLatest) != now {
		t.Fatalf("signal legacy_latest_ms = %v, want %d", legacyLatest, now)
	}
	whatsapp, ok := freshness["whatsapp"].(map[string]any)
	if !ok {
		t.Fatalf("whatsapp freshness = %v", freshness["whatsapp"])
	}
	if stalled, _ := whatsapp["projection_stalled"].(bool); stalled {
		t.Fatalf("whatsapp freshness = %v, want projection_stalled false", whatsapp)
	}
}

// Legacy-primary has one store, so there is no projection to lag: the extra
// fields must stay absent rather than compare the store against itself.
func TestStatusFreshnessOmitsProjectionLagWhenNotV2Primary(t *testing.T) {
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.UpsertMessage(&db.Message{
		MessageID: "signal:only", ConversationID: "signal:+15550000001", Body: "hi",
		TimestampMS: time.Now().UnixMilli(), SourcePlatform: "signal", SourceID: "only",
	}); err != nil {
		t.Fatal(err)
	}

	h := APIHandlerWithOptions(store, nil, zerolog.Nop(), nil, APIOptions{})
	srv := httptest.NewServer(h)
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
	freshness, _ := payload["freshness"].(map[string]any)
	if _, present := freshness["projection_stalled"]; present {
		t.Fatalf("freshness = %v, want no projection_stalled on a legacy-primary daemon", freshness)
	}
	signal, ok := freshness["signal"].(map[string]any)
	if !ok {
		t.Fatalf("signal freshness = %v", freshness["signal"])
	}
	if _, present := signal["projection_lag_ms"]; present {
		t.Fatalf("signal freshness = %v, want no projection_lag_ms", signal)
	}
}

// A platform the read source has NO rows for, while legacy does, is the most
// severe stall — it must surface rather than silently drop out of the payload.
func TestStatusFreshnessSurfacesPlatformMissingEntirelyFromReadSource(t *testing.T) {
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UnixMilli()
	if err := store.UpsertMessage(&db.Message{
		MessageID: "signal:only", ConversationID: "signal:+15550000001", Body: "hi",
		TimestampMS: now, SourcePlatform: "signal", SourceID: "only",
	}); err != nil {
		t.Fatal(err)
	}

	h := APIHandlerWithOptions(store, nil, zerolog.Nop(), nil, APIOptions{
		Reads:     stubReads{stats: []db.PlatformStat{}},
		V2Primary: true,
	})
	srv := httptest.NewServer(h)
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
	freshness, _ := payload["freshness"].(map[string]any)
	signal, ok := freshness["signal"].(map[string]any)
	if !ok {
		t.Fatalf("signal freshness = %v, want an entry synthesized for the missing platform", freshness["signal"])
	}
	if stalled, _ := signal["projection_stalled"].(bool); !stalled {
		t.Fatalf("signal freshness = %v, want projection_stalled true", signal)
	}
	if latest, _ := signal["latest_ms"].(float64); int64(latest) != 0 {
		t.Fatalf("signal latest_ms = %v, want 0 (the read source has nothing)", latest)
	}
}
