package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/v2read"
)

// The v2 read source must keep the count-free freshness read; without it every
// /api/status freshness refresh falls back to PlatformStats and counts every
// message.
var _ platformLatestSource = (*v2read.Source)(nil)

// latestOnlyReads answers freshness from PlatformLatest and fails the test if
// anything asks it to count messages.
type latestOnlyReads struct {
	stubReads
	t      *testing.T
	latest []db.PlatformLatest
}

func (s latestOnlyReads) PlatformLatest() ([]db.PlatformLatest, error) { return s.latest, nil }

func (s latestOnlyReads) PlatformStats() ([]db.PlatformStat, error) {
	s.t.Errorf("/api/status freshness called PlatformStats; it must use PlatformLatest")
	return nil, nil
}

func TestStatusFreshnessReadsPlatformLatestWithoutCounting(t *testing.T) {
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	now := time.Now().UnixMilli()
	fourDaysAgo := now - 4*24*60*60*1000
	reads := latestOnlyReads{t: t, latest: []db.PlatformLatest{
		{Platform: "whatsapp", LatestMS: now, LatestRecvMS: now - 1},
		{Platform: "signal", LatestMS: fourDaysAgo, LatestRecvMS: fourDaysAgo - 1},
	}}
	h := APIHandlerWithOptions(store, nil, zerolog.Nop(), nil, APIOptions{Reads: reads})
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
	if newest, _ := freshness["newest_ms"].(float64); int64(newest) != now {
		t.Fatalf("newest_ms = %v, want %d", freshness["newest_ms"], now)
	}
	whatsapp, _ := freshness["whatsapp"].(map[string]any)
	if received, _ := whatsapp["latest_received_ms"].(float64); int64(received) != now-1 {
		t.Fatalf("whatsapp freshness = %v, want latest_received_ms %d", whatsapp, now-1)
	}
	signal, _ := freshness["signal"].(map[string]any)
	if stale, _ := signal["stale"].(bool); !stale {
		t.Fatalf("signal freshness = %v, want stale (four days behind whatsapp)", signal)
	}
}
