package tools

// ttl_seconds / OPENMESSAGES_SEND_TTL_SECONDS / wait_seconds parsing. Every
// range check must run on the float64 before any float-to-Duration
// conversion: that conversion is implementation-defined out of range (on
// amd64 a huge or NaN value becomes math.MinInt64 ns; on arm64 NaN becomes
// 0), so an unchecked input could silently become "never expire" or a
// negative wait. CI's linux/amd64 job exercises the platform-sensitive side.

import (
	"context"
	"encoding/json"
	"math"
	"math/rand"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"testing/quick"
	"time"

	"github.com/maxghenis/openmessage/internal/messaging"
)

func TestParseSendTTLArgument(t *testing.T) {
	t.Setenv(sendTTLEnvVar, "")
	accepted := []struct {
		seconds any
		want    time.Duration
	}{
		{float64(0), 0},
		{float64(120), 2 * time.Minute},
		{int(30), 30 * time.Second},
		{0.001, time.Millisecond},
		{0.0015, 2 * time.Millisecond}, // rounded to the millisecond, never truncated to 0 or 1
		{86400.0, 24 * time.Hour},
	}
	for _, test := range accepted {
		ttl, err := parseSendTTL(map[string]any{"ttl_seconds": test.seconds})
		if err != nil || ttl != test.want {
			t.Errorf("parseSendTTL(ttl_seconds=%v) = %v, %v; want %v, nil", test.seconds, ttl, err, test.want)
		}
	}
	rejected := []struct {
		seconds any
		reason  string
	}{
		{1e10, "must not exceed 86400 (24 hours)"},  // was MinInt64 ns on amd64: never expire via the daemon
		{1e300, "must not exceed 86400 (24 hours)"}, // likewise
		{86400.001, "must not exceed 86400 (24 hours)"},
		{1e-10, "at least 0.001"},  // was 0 (never expire) on arm64
		{0.0005, "at least 0.001"}, // was born expired in-process and never-expire via the daemon
		{-1.0, "must not be negative"},
		{math.NaN(), "finite"},
		{math.Inf(1), "finite"},
		{"600", "must be a number"},
	}
	for _, test := range rejected {
		ttl, err := parseSendTTL(map[string]any{"ttl_seconds": test.seconds})
		if err == nil {
			t.Errorf("parseSendTTL(ttl_seconds=%v) = %v, nil; want an error", test.seconds, ttl)
			continue
		}
		if !strings.HasPrefix(err.Error(), "ttl_seconds ") || !strings.Contains(err.Error(), test.reason) {
			t.Errorf("parseSendTTL(ttl_seconds=%v) error = %q, want ttl_seconds ... %q", test.seconds, err, test.reason)
		}
	}
}

func TestParseSendTTLEnvironmentOverride(t *testing.T) {
	accepted := map[string]time.Duration{
		"":       defaultSendTTL,
		"0":      0,
		"300":    5 * time.Minute,
		" 90 ":   90 * time.Second,
		"0.0015": 2 * time.Millisecond,
		"86400":  24 * time.Hour,
	}
	for raw, want := range accepted {
		t.Setenv(sendTTLEnvVar, raw)
		ttl, err := parseSendTTL(map[string]any{})
		if err != nil || ttl != want {
			t.Errorf("%s=%q: parseSendTTL = %v, %v; want %v, nil", sendTTLEnvVar, raw, ttl, err, want)
		}
	}
	// NaN used to disable the default window on arm64 (and every entry but
	// garbage became a never-expiring MinInt64 window on amd64).
	for _, raw := range []string{"NaN", "nan", "Inf", "+Inf", "-Inf", "infinity", "1e-12", "0.0005", "1e10", "86401", "-1", "ten minutes"} {
		t.Setenv(sendTTLEnvVar, raw)
		ttl, err := parseSendTTL(map[string]any{})
		if err == nil {
			t.Errorf("%s=%q: parseSendTTL = %v, nil; want an error", sendTTLEnvVar, raw, ttl)
			continue
		}
		if !strings.Contains(err.Error(), sendTTLEnvVar) {
			t.Errorf("%s=%q: error %q does not name the variable", sendTTLEnvVar, raw, err)
		}
	}
	// An explicit argument wins over the environment, even a broken one.
	t.Setenv(sendTTLEnvVar, "NaN")
	if ttl, err := parseSendTTL(map[string]any{"ttl_seconds": float64(60)}); err != nil || ttl != time.Minute {
		t.Errorf("explicit ttl_seconds with broken env = %v, %v; want 1m, nil", ttl, err)
	}
}

func TestParseSendWaitOptionsClampsBeforeConversion(t *testing.T) {
	accepted := []struct {
		args map[string]any
		want time.Duration
	}{
		{map[string]any{}, defaultSendWait},
		{map[string]any{"wait_seconds": float64(0)}, 0},
		{map[string]any{"wait_seconds": float64(30)}, 30 * time.Second},
		{map[string]any{"wait_seconds": 1.5}, 1500 * time.Millisecond},
		{map[string]any{"wait_seconds": float64(120)}, maxSendWait},
		{map[string]any{"wait_seconds": float64(121)}, maxSendWait},
		{map[string]any{"wait_seconds": 1e10}, maxSendWait},  // was a negative Duration on amd64
		{map[string]any{"wait_seconds": 1e300}, maxSendWait}, // likewise
		{map[string]any{"wait_seconds": math.MaxFloat64}, maxSendWait},
	}
	for _, test := range accepted {
		options, err := parseSendWaitOptions(test.args)
		if err != nil || options.Wait != test.want {
			t.Errorf("parseSendWaitOptions(%v) = %v, %v; want wait %v", test.args, options.Wait, err, test.want)
		}
	}
	for _, seconds := range []any{-1.0, math.NaN(), math.Inf(1), math.Inf(-1), "25"} {
		if options, err := parseSendWaitOptions(map[string]any{"wait_seconds": seconds}); err == nil {
			t.Errorf("parseSendWaitOptions(wait_seconds=%v) = %+v, nil; want an error", seconds, options)
		}
	}
}

// ttlArgument is a generated ttl_seconds argument biased toward the edges of
// the accepted range and toward values whose float conversion is
// implementation-defined.
type ttlArgument float64

func (ttlArgument) Generate(r *rand.Rand, _ int) reflect.Value {
	edges := []float64{
		0, 0.001, 0.0015, 0.0009999999, 1e-10, 86400, 86400.0000001, 1e10, 1e300,
		math.NaN(), math.Inf(1), -1, math.Nextafter(0.001, 1), math.Nextafter(86400, 0),
	}
	var value float64
	switch r.Intn(5) {
	case 0:
		value = math.Float64frombits(r.Uint64())
	case 1:
		value = r.Float64() * 0.003
	case 2:
		value = r.Float64() * 90_000
	case 3:
		value = edges[r.Intn(len(edges))]
	default:
		value = float64(r.Intn(86_401)) + r.Float64()
	}
	return reflect.ValueOf(ttlArgument(value))
}

// TestQuickParseSendTTLBoundedOnEveryPlatform executes I7 through the tool's
// own parser: any accepted positive window is 1ms..24h and ms-aligned.
func TestQuickParseSendTTLBoundedOnEveryPlatform(t *testing.T) {
	t.Setenv(sendTTLEnvVar, "")
	property := func(input ttlArgument) bool {
		ttl, err := parseSendTTL(map[string]any{"ttl_seconds": float64(input)})
		if err != nil {
			return true
		}
		if float64(input) == 0 {
			return ttl == 0
		}
		return ttl >= time.Millisecond && ttl <= maxSendTTL && ttl%time.Millisecond == 0
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 20_000, Rand: rand.New(rand.NewSource(166_31))}); err != nil {
		t.Fatal(err)
	}
}

// TestQuickDaemonAndInProcessTTLAgree is the I9 differential at the tool
// layer. In-process sends pass parseSendTTL's Duration straight to
// messaging; daemon-routed sends forward ttl.Milliseconds() as ttl_ms, which
// the daemon parses with messaging.TTLFromMilliseconds. Both must stamp the
// same window for every accepted ttl_seconds.
func TestQuickDaemonAndInProcessTTLAgree(t *testing.T) {
	t.Setenv(sendTTLEnvVar, "")
	property := func(input ttlArgument) bool {
		inProcess, err := parseSendTTL(map[string]any{"ttl_seconds": float64(input)})
		if err != nil || inProcess == 0 {
			return true // rejected on both paths; 0 omits ttl_ms, which is also no expiry
		}
		daemon, err := messaging.TTLFromMilliseconds(inProcess.Milliseconds())
		return err == nil && daemon == inProcess
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 20_000, Rand: rand.New(rand.NewSource(166_32))}); err != nil {
		t.Fatal(err)
	}
}

// TestDaemonSendCarriesMillisecondExactTTL drives the daemon-routed tool end
// to end: ttl_ms on the wire is the rounded millisecond window (0.0015s used
// to be truncated to 1 and 0.0005s to 0, i.e. never expire), and an
// out-of-range ttl_seconds never reaches the daemon.
func TestDaemonSendCarriesMillisecondExactTTL(t *testing.T) {
	t.Setenv(sendTTLEnvVar, "")
	var (
		mu        sync.Mutex
		submitted []map[string]any
	)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"v2_send": true, "v2_primary": true, "connected": true})
	})
	mux.HandleFunc("/api/v1/outbox/messages", func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode submission: %v", err)
		}
		mu.Lock()
		submitted = append(submitted, request)
		mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"outbox_id": "out-ttl", "state": "queued"})
	})
	mux.HandleFunc("/api/v1/outbox/out-ttl", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"outbox_id": "out-ttl", "state": "confirmed", "remote_message_id": "remote-ttl",
		})
	})
	handler := daemonSendToConversationHandler(Options{Daemon: daemonClientFor(t, mux)})
	send := func(ttlSeconds any) (map[string]any, bool) {
		t.Helper()
		mu.Lock()
		before := len(submitted)
		mu.Unlock()
		args := map[string]any{
			"conversation_id": "conv-ttl",
			"message":         "windowed daemon send",
			"wait_seconds":    float64(1),
		}
		if ttlSeconds != nil {
			args["ttl_seconds"] = ttlSeconds
		}
		result, err := handler(context.Background(), v2ToolCall(args))
		if err != nil {
			t.Fatalf("handler(ttl_seconds=%v): %v", ttlSeconds, err)
		}
		mu.Lock()
		defer mu.Unlock()
		if len(submitted) == before {
			if !result.IsError {
				t.Fatalf("ttl_seconds=%v: no submission and no error result", ttlSeconds)
			}
			return nil, false
		}
		return submitted[len(submitted)-1], true
	}

	for _, test := range []struct {
		seconds any
		wantMS  int64
	}{
		{0.0015, 2},
		{1.2345, 1235},
		{0.001, 1},
		{float64(600), 600_000},
		{float64(86400), 86_400_000},
	} {
		request, ok := send(test.seconds)
		if !ok {
			t.Fatalf("ttl_seconds=%v was not submitted", test.seconds)
		}
		got, present := request["ttl_ms"].(float64)
		if !present || int64(got) != test.wantMS {
			t.Errorf("ttl_seconds=%v: ttl_ms = %v, want %d", test.seconds, request["ttl_ms"], test.wantMS)
		}
		if daemonTTL, err := messaging.TTLFromMilliseconds(int64(got)); err != nil || daemonTTL <= 0 {
			t.Errorf("ttl_seconds=%v: daemon would parse ttl_ms=%v as %v, %v", test.seconds, got, daemonTTL, err)
		}
	}
	if request, ok := send(float64(0)); !ok {
		t.Fatal("ttl_seconds=0 was not submitted")
	} else if _, present := request["ttl_ms"]; present {
		t.Errorf("ttl_seconds=0 sent ttl_ms=%v, want it omitted (no expiry)", request["ttl_ms"])
	}
	for _, seconds := range []any{0.0005, 1e-10, 1e10, 86400.5, -1.0} {
		if request, ok := send(seconds); ok {
			t.Errorf("ttl_seconds=%v reached the daemon as %v, want it refused locally", seconds, request)
		}
	}
}
