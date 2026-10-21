package web

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestStatusReportsSilenceRecovery(t *testing.T) {
	ts := newTestServerWithOptions(t, APIOptions{
		SilenceRecovery: func() any {
			return map[string]any{"google": map[string]any{
				"enabled": true,
				"pending": map[string]any{"state": "pending", "since_ms": 1759705200000},
			}}
		},
	})
	for _, path := range []string{"/api/status", "/api/diagnostics"} {
		resp, err := http.Get(ts.server.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		var payload map[string]any
		err = json.NewDecoder(resp.Body).Decode(&payload)
		resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		recovery, _ := payload["silence_recovery"].(map[string]any)
		google, _ := recovery["google"].(map[string]any)
		pending, _ := google["pending"].(map[string]any)
		if google["enabled"] != true || pending["state"] != "pending" {
			t.Fatalf("%s silence_recovery = %v", path, payload["silence_recovery"])
		}
	}
}

func TestStatusOmitsSilenceRecoveryWithoutProvider(t *testing.T) {
	ts := newTestServer(t)
	resp, err := http.Get(ts.server.URL + "/api/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if _, ok := payload["silence_recovery"]; ok {
		t.Fatalf("silence_recovery reported without a provider: %v", payload["silence_recovery"])
	}
}
