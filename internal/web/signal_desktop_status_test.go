package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestStatusSeparatesSignalDesktopHistoryFromLiveBridge(t *testing.T) {
	live := map[string]any{"paired": false, "connected": false, "connecting": false, "pairing": false, "qr_available": false}
	ts := newV1RecorderHarness(t, APIOptions{
		SignalStatus: func() any { return live },
		SignalDesktopStatus: func() any {
			return map[string]any{"enabled": true, "available": true, "active": true, "gate": "live_unpaired", "high_water_rowid": 42}
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
	var signal map[string]any
	if err := json.Unmarshal(status["signal"], &signal); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(signal, live) {
		t.Fatalf("signal block = %v, want the live bridge's status unchanged", signal)
	}
	var desktop struct {
		Enabled, Available, Active bool
		Gate                       string
		HighWaterRowID             int64 `json:"high_water_rowid"`
	}
	if err := json.Unmarshal(status["signal_desktop"], &desktop); err != nil {
		t.Fatal(err)
	}
	if !desktop.Enabled || !desktop.Available || !desktop.Active || desktop.Gate != "live_unpaired" || desktop.HighWaterRowID != 42 {
		t.Fatalf("signal_desktop = %+v", desktop)
	}
}
