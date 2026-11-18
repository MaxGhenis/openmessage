package signallive

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// Valid JSON that is not a receive payload may parse after a decoder fix, so
// it stays queued as unmarshal_failed.
func TestHandleReceiveOutputQueuesJSONThatIsNotAReceivePayload(t *testing.T) {
	bridge := &Bridge{logger: zerolog.Nop(), configDir: t.TempDir()}
	originalNow := now
	defer func() { now = originalNow }()
	now = func() time.Time { return time.UnixMilli(1700000000123) }
	// lastReceiveRecoveryAt is now: the recovery sync stays inside its window
	// and never runs signal-cli.
	bridge.lastReceiveRecoveryAt = now().UnixMilli()

	if err := bridge.handleReceiveOutput("+15551230000", []byte(`{"envelope":"not an object"}`+"\n")); err != nil {
		t.Fatalf("handleReceiveOutput(): %v", err)
	}
	status := bridge.receiveRecoveryStatus()
	if status == nil || status.PendingCount != 1 || status.UnparseableCount != 0 || status.LastIssueReason != "unmarshal_failed" {
		t.Fatalf("receive recovery status = %+v, want 1 pending unmarshal_failed", status)
	}
}

// The six records in the live queue (2026-11-07 and 2026-11-16) are bare "{"
// lines queued as unmarshal_failed by earlier versions. They count as
// unparseable, not pending, so the watchdog's pending_count >= 5 alert stops;
// the next replay drops them and keeps the count; a real record stays.
func TestRecoveryQueueTreatsQueuedLoneBraceRecordsAsUnparseable(t *testing.T) {
	bridge := &Bridge{logger: zerolog.Nop(), configDir: t.TempDir()}
	var lines []string
	for _, ts := range []int64{1794024497148, 1794024497148, 1794024500344, 1794024500344, 1794866014779, 1794866014779} {
		encoded, err := json.Marshal(signalReceiveRecoveryRecord{TimestampMS: ts, Account: "+15551230000", Reason: "unmarshal_failed", Error: "unexpected end of JSON input", Raw: "{"})
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, string(encoded))
	}
	genuine, err := json.Marshal(signalReceiveRecoveryRecord{TimestampMS: 1794000000000, Account: "+15551230000", Reason: "unmarshal_failed", Raw: `{"envelope":"not an object"}`})
	if err != nil {
		t.Fatal(err)
	}
	lines = append(lines, string(genuine))
	if err := os.WriteFile(bridge.receiveRecoveryPath(), []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	check := func(stage string) {
		t.Helper()
		status := bridge.receiveRecoveryStatus()
		if status == nil || status.PendingCount != 1 || status.LastIssueAt != 1794000000000 || status.LastIssueReason != "unmarshal_failed" ||
			status.UnparseableCount != 6 || status.LastUnparseableAt != 1794866014779 {
			t.Fatalf("%s: receive recovery status = %+v, want 1 pending and 6 unparseable", stage, status)
		}
	}
	check("before replay")

	bridge.replayReceiveRecoveryQueue("+15551230000")
	remaining, err := os.ReadFile(bridge.receiveRecoveryPath())
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(remaining)); got != string(genuine) {
		t.Fatalf("queue after replay = %q, want only the replayable record", got)
	}
	check("after replay")

	// Replaying again changes nothing.
	bridge.replayReceiveRecoveryQueue("+15551230000")
	check("after a second replay")
}

func TestReceiveOutputHasEnvelopesIgnoresFragments(t *testing.T) {
	for _, tc := range []struct {
		name   string
		output string
		want   bool
	}{
		{name: "envelope", output: "INFO starting\n" + `{"envelope":{"timestamp":1}}` + "\n", want: true},
		{name: "log lines only", output: "INFO starting\nWARN slow\n"},
		{name: "pretty-printed object", output: "{\n  \"error\" : \"synthetic\"\n}\n"},
		{name: "lone brace", output: "{\n"},
		{name: "cut-off line", output: `{"envelope":{"timest` + "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := receiveOutputHasEnvelopes([]byte(tc.output)); got != tc.want {
				t.Fatalf("receiveOutputHasEnvelopes() = %v, want %v", got, tc.want)
			}
		})
	}
}
