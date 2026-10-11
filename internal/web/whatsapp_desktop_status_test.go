package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestStatusSeparatesWhatsAppDesktopHistoryFromLiveBridge(t *testing.T) {
	ts := newV1RecorderHarness(t, APIOptions{
		WhatsAppStatus: func() any { return map[string]any{"paired": false, "connected": false} },
		WhatsAppDesktopStatus: func() any {
			return map[string]any{"enabled": true, "available": true, "active": true, "high_water_pk": 42}
		},
	})
	response := ts.do(t, httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/status", nil))
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}
	var status map[string]json.RawMessage
	if err := json.NewDecoder(response.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	var live struct{ Paired, Connected bool }
	if err := json.Unmarshal(status["whatsapp"], &live); err != nil {
		t.Fatal(err)
	}
	if live.Paired || live.Connected {
		t.Fatalf("desktop feed changed live state: %+v", live)
	}
	var desktop struct {
		Enabled, Available, Active bool
		HighWaterPK                int64 `json:"high_water_pk"`
	}
	if err := json.Unmarshal(status["whatsapp_desktop"], &desktop); err != nil {
		t.Fatal(err)
	}
	if !desktop.Enabled || !desktop.Available || !desktop.Active || desktop.HighWaterPK != 42 {
		t.Fatalf("desktop status = %+v", desktop)
	}
}
