package tools

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSMSPathStatusTextWarnsOnlyWhenStalled(t *testing.T) {
	decode := func(body string) map[string]any {
		var raw map[string]any
		if err := json.Unmarshal([]byte(body), &raw); err != nil {
			t.Fatal(err)
		}
		return raw
	}
	stalled := decode(`{"freshness": {"google": {"sms_path": {"stalled": true, "silent_ms": 90000000}}}}`)
	if text := smsPathStatusText(stalled); !strings.Contains(text, "Google SMS: STOPPED. No incoming SMS for 25h") {
		t.Fatalf("stalled text = %q", text)
	}
	for _, body := range []string{
		`{}`,
		`{"freshness": {"google": {}}}`,
		`{"freshness": {"google": {"sms_path": {"stalled": false, "reason": "sms_recent"}}}}`,
	} {
		if text := smsPathStatusText(decode(body)); text != "" {
			t.Fatalf("%s: text = %q, want none", body, text)
		}
	}
}
