package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/quick"
	"time"
)

// backfillWindowNow is the fixed clock for parseBackfillSince tests. It is a
// whole second so "one millisecond later" is unambiguously in the future.
var backfillWindowNow = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

func backfillWindowRequest(body string) *http.Request {
	return httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/backfill", strings.NewReader(body))
}

type backfillWindowErrReader struct{}

func (backfillWindowErrReader) Read([]byte) (int, error) {
	return 0, errors.New("backfill window test: body read failed")
}

func TestParseBackfillSince(t *testing.T) {
	offsetInstant := time.Date(2026, 10, 5, 13, 30, 0, 0, time.UTC) // 09:30 at -04:00
	unixMS := offsetInstant.UnixMilli()
	localDate := time.Date(2026, 10, 5, 0, 0, 0, 0, time.Local)

	tests := []struct {
		name         string
		request      func() *http.Request
		wantWindowed bool
		want         time.Time
		// wantErr is a required substring of the error; empty means no error.
		wantErr string
		// wantOffset, when set, is the zone offset (seconds) the parsed time
		// must keep, since the handler echoes since in that zone.
		wantOffset *int
		wantLocal  bool
	}{
		// Absent window: these all mean "the original deep backfill".
		{
			name: "nil body",
			request: func() *http.Request {
				request := backfillWindowRequest("")
				request.Body = nil
				return request
			},
		},
		{
			name: "http.NoBody",
			request: func() *http.Request {
				request := backfillWindowRequest("")
				request.Body = http.NoBody
				return request
			},
		},
		{name: "empty body", request: func() *http.Request { return backfillWindowRequest("") }},
		{name: "whitespace-only body", request: func() *http.Request { return backfillWindowRequest(" \r\n\t ") }},
		{name: "empty object", request: func() *http.Request { return backfillWindowRequest(`{}`) }},
		{name: "since null", request: func() *http.Request { return backfillWindowRequest(`{"since": null}`) }},
		{name: "since null with whitespace", request: func() *http.Request { return backfillWindowRequest(`{"since":  null  }`) }},
		{
			// Documented: a bare JSON null decodes into the request struct as a
			// no-op, so it is the same as an empty body (deep backfill).
			name:    "top-level JSON null",
			request: func() *http.Request { return backfillWindowRequest(`null`) },
		},

		// Accepted window starts.
		{
			name:         "RFC 3339 with offset keeps instant and zone",
			request:      func() *http.Request { return backfillWindowRequest(`{"since":"2026-10-05T09:30:00-04:00"}`) },
			wantWindowed: true,
			want:         offsetInstant,
			wantOffset:   backfillWindowIntPointer(-4 * 60 * 60),
		},
		{
			name:         "RFC 3339 UTC",
			request:      func() *http.Request { return backfillWindowRequest(`{"since":"2026-10-05T13:30:00Z"}`) },
			wantWindowed: true,
			want:         offsetInstant,
			wantOffset:   backfillWindowIntPointer(0),
		},
		{
			// time.Parse accepts a fractional second even though the layout is
			// RFC3339 without one; the full nanosecond precision is preserved.
			name:         "RFC 3339 with fractional seconds",
			request:      func() *http.Request { return backfillWindowRequest(`{"since":"2026-10-05T09:30:00.123456789-04:00"}`) },
			wantWindowed: true,
			want:         offsetInstant.Add(123456789 * time.Nanosecond),
		},
		{
			name:         "RFC 3339 surrounded by whitespace",
			request:      func() *http.Request { return backfillWindowRequest(`{"since":"  2026-10-05T13:30:00Z\t"}`) },
			wantWindowed: true,
			want:         offsetInstant,
		},
		{
			name:         "local date is local midnight",
			request:      func() *http.Request { return backfillWindowRequest(`{"since":"2026-10-05"}`) },
			wantWindowed: true,
			want:         localDate,
			wantLocal:    true,
		},
		{
			name:         "unix milliseconds number",
			request:      func() *http.Request { return backfillWindowRequest(fmt.Sprintf(`{"since":%d}`, unixMS)) },
			wantWindowed: true,
			want:         time.UnixMilli(unixMS),
			wantLocal:    true,
		},
		{
			name:         "unix milliseconds string",
			request:      func() *http.Request { return backfillWindowRequest(fmt.Sprintf(`{"since":"%d"}`, unixMS)) },
			wantWindowed: true,
			want:         time.UnixMilli(unixMS),
			wantLocal:    true,
		},
		{
			name:         "unix milliseconds string with whitespace",
			request:      func() *http.Request { return backfillWindowRequest(fmt.Sprintf(`{"since":" %d "}`, unixMS)) },
			wantWindowed: true,
			want:         time.UnixMilli(unixMS),
		},
		{
			// Boundary: since == now is not in the future.
			name: "exactly now",
			request: func() *http.Request {
				return backfillWindowRequest(fmt.Sprintf(`{"since":%d}`, backfillWindowNow.UnixMilli()))
			},
			wantWindowed: true,
			want:         backfillWindowNow,
		},
		{
			// Boundary: the smallest millisecond value accepted (March 1973);
			// anything smaller is almost certainly Unix seconds.
			name:         "smallest accepted millisecond value",
			request:      func() *http.Request { return backfillWindowRequest(`{"since":100000000000}`) },
			wantWindowed: true,
			want:         time.UnixMilli(100_000_000_000),
		},
		{
			name:    "Unix seconds as a number",
			request: func() *http.Request { return backfillWindowRequest(`{"since":1759671000}`) },
			wantErr: "Unix seconds must be multiplied by 1000",
		},
		{
			name:    "Unix seconds as a string",
			request: func() *http.Request { return backfillWindowRequest(`{"since":"1759671000"}`) },
			wantErr: "Unix seconds must be multiplied by 1000",
		},
		{
			name:    "one millisecond after the epoch",
			request: func() *http.Request { return backfillWindowRequest(`{"since":1}`) },
			wantErr: "Unix seconds must be multiplied by 1000",
		},
		{
			// Documented: encoding/json matches field names case-insensitively,
			// so a differently cased key is the since field, not an unknown
			// field (and therefore not a silent deep backfill either).
			name:         "field name matched case-insensitively",
			request:      func() *http.Request { return backfillWindowRequest(`{"SINCE":"2026-10-05T13:30:00Z"}`) },
			wantWindowed: true,
			want:         offsetInstant,
		},

		// Future starts.
		{
			name: "one millisecond in the future",
			request: func() *http.Request {
				return backfillWindowRequest(fmt.Sprintf(`{"since":%d}`, backfillWindowNow.UnixMilli()+1))
			},
			wantErr: "since is in the future",
		},
		{
			name:    "future RFC 3339",
			request: func() *http.Request { return backfillWindowRequest(`{"since":"2027-01-01T00:00:00Z"}`) },
			wantErr: "since is in the future",
		},
		{
			// Local midnight of 2026-10-10 is after 2026-10-08T12:00Z in every
			// zone (UTC-12 through UTC+14).
			name:    "future local date",
			request: func() *http.Request { return backfillWindowRequest(`{"since":"2026-10-10"}`) },
			wantErr: "since is in the future",
		},

		// Non-positive starts.
		{name: "zero number", request: func() *http.Request { return backfillWindowRequest(`{"since":0}`) }, wantErr: "after the Unix epoch"},
		{name: "zero string", request: func() *http.Request { return backfillWindowRequest(`{"since":"0"}`) }, wantErr: "after the Unix epoch"},
		{name: "negative number", request: func() *http.Request { return backfillWindowRequest(`{"since":-1}`) }, wantErr: "after the Unix epoch"},
		{name: "negative string", request: func() *http.Request { return backfillWindowRequest(`{"since":"-1759671000000"}`) }, wantErr: "after the Unix epoch"},
		{
			name:    "pre-epoch RFC 3339",
			request: func() *http.Request { return backfillWindowRequest(`{"since":"1969-12-31T23:59:59Z"}`) },
			wantErr: "after the Unix epoch",
		},
		{
			// The epoch guard is on whole milliseconds: 500µs after the epoch
			// truncates to 0 ms and is rejected.
			name:    "sub-millisecond after the epoch",
			request: func() *http.Request { return backfillWindowRequest(`{"since":"1970-01-01T00:00:00.0005Z"}`) },
			wantErr: "after the Unix epoch",
		},

		// Typos and malformed bodies must fail closed, never fall through to a
		// deep backfill.
		{
			name:    "unknown field typo",
			request: func() *http.Request { return backfillWindowRequest(`{"sinc":"2026-10-05"}`) },
			wantErr: `unknown field "sinc"`,
		},
		{
			name:    "unknown field next to a valid since",
			request: func() *http.Request { return backfillWindowRequest(`{"since":"2026-10-05","deep":true}`) },
			wantErr: `unknown field "deep"`,
		},
		{name: "truncated object", request: func() *http.Request { return backfillWindowRequest(`{`) }, wantErr: "invalid JSON"},
		{name: "missing value", request: func() *http.Request { return backfillWindowRequest(`{"since": }`) }, wantErr: "invalid JSON"},
		{name: "top-level string", request: func() *http.Request { return backfillWindowRequest(`"2026-10-05"`) }, wantErr: "invalid JSON"},
		{name: "top-level array", request: func() *http.Request { return backfillWindowRequest(`[]`) }, wantErr: "invalid JSON"},
		{name: "top-level number", request: func() *http.Request { return backfillWindowRequest(`1759671000000`) }, wantErr: "invalid JSON"},
		{name: "since boolean", request: func() *http.Request { return backfillWindowRequest(`{"since":true}`) }, wantErr: "since must be"},
		{name: "since object", request: func() *http.Request { return backfillWindowRequest(`{"since":{}}`) }, wantErr: "since must be"},
		{name: "since array", request: func() *http.Request { return backfillWindowRequest(`{"since":[1759671000000]}`) }, wantErr: "since must be"},
		{
			name:    "since fractional milliseconds",
			request: func() *http.Request { return backfillWindowRequest(`{"since":1759671000000.5}`) },
			wantErr: "since must be",
		},
		{
			// Documented: an integral value in exponent form is not an int64
			// to encoding/json, so it is rejected rather than rounded.
			name:    "since exponent number",
			request: func() *http.Request { return backfillWindowRequest(`{"since":1.759671e12}`) },
			wantErr: "since must be",
		},
		{
			name:    "since overflows int64",
			request: func() *http.Request { return backfillWindowRequest(`{"since":99999999999999999999}`) },
			wantErr: "since must be",
		},
		{
			// An empty string is a present-but-invalid value, not an absent one.
			name:    "since empty string",
			request: func() *http.Request { return backfillWindowRequest(`{"since":""}`) },
			wantErr: `since "" must be`,
		},
		{
			name:    "since free text",
			request: func() *http.Request { return backfillWindowRequest(`{"since":"yesterday"}`) },
			wantErr: `since "yesterday" must be`,
		},
		{
			// A zone-less timestamp is rejected rather than guessed.
			name:    "since timestamp without zone",
			request: func() *http.Request { return backfillWindowRequest(`{"since":"2026-10-05T09:30:00"}`) },
			wantErr: `since "2026-10-05T09:30:00" must be`,
		},
		{
			name:    "since date with single-digit day",
			request: func() *http.Request { return backfillWindowRequest(`{"since":"2026-10-5"}`) },
			wantErr: "must be an RFC 3339 time, a YYYY-MM-DD local date, or Unix milliseconds",
		},
		{
			name: "body read error",
			request: func() *http.Request {
				request := backfillWindowRequest("")
				request.Body = io.NopCloser(backfillWindowErrReader{})
				return request
			},
			wantErr: "read request body",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, windowed, err := parseBackfillSince(tt.request(), backfillWindowNow)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("parseBackfillSince() = (%v, %t, nil), want error containing %q", got, windowed, tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("parseBackfillSince() error = %q, want it to contain %q", err, tt.wantErr)
				}
				if windowed || !got.IsZero() {
					t.Fatalf("parseBackfillSince() error path returned (%v, %t), want zero time and windowed=false", got, windowed)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseBackfillSince() error = %v", err)
			}
			if windowed != tt.wantWindowed {
				t.Fatalf("parseBackfillSince() windowed = %t, want %t", windowed, tt.wantWindowed)
			}
			if !tt.wantWindowed {
				if !got.IsZero() {
					t.Fatalf("parseBackfillSince() without a window returned since %v, want zero", got)
				}
				return
			}
			if !got.Equal(tt.want) {
				t.Fatalf("parseBackfillSince() since = %s, want %s", got.Format(time.RFC3339Nano), tt.want.Format(time.RFC3339Nano))
			}
			if tt.wantOffset != nil {
				if _, offset := got.Zone(); offset != *tt.wantOffset {
					t.Fatalf("parseBackfillSince() zone offset = %d, want %d", offset, *tt.wantOffset)
				}
			}
			if tt.wantLocal && got.Location() != time.Local {
				t.Fatalf("parseBackfillSince() location = %v, want time.Local", got.Location())
			}
		})
	}
}

func backfillWindowIntPointer(value int) *int { return &value }

// TestParseBackfillSinceLocalDateIsMidnight checks the date form against the
// local calendar, whatever TZ the test process runs under.
func TestParseBackfillSinceLocalDateIsMidnight(t *testing.T) {
	got, windowed, err := parseBackfillSince(backfillWindowRequest(`{"since":"2026-03-08"}`), backfillWindowNow)
	if err != nil || !windowed {
		t.Fatalf("parseBackfillSince(date) = (%v, %t, %v), want a window", got, windowed, err)
	}
	local := got.In(time.Local)
	if local.Year() != 2026 || local.Month() != time.March || local.Day() != 8 ||
		local.Hour() != 0 || local.Minute() != 0 || local.Second() != 0 || local.Nanosecond() != 0 {
		t.Fatalf("parseBackfillSince(date) = %s local, want 2026-03-08 00:00:00 local", local.Format(time.RFC3339Nano))
	}
}

// TestParseBackfillSinceOversizedBody documents the 4096-byte limit: a body
// of at most 4096 bytes is parsed whole, and any longer body is rejected
// outright rather than truncated (a truncated body could read as empty and
// start a deep backfill).
func TestParseBackfillSinceOversizedBody(t *testing.T) {
	const limit = 4096
	valid := `{"since":"2026-10-05T13:30:00Z"}`
	want := time.Date(2026, 10, 5, 13, 30, 0, 0, time.UTC)

	// paddedObject builds a valid object whose closing brace is byte n.
	paddedObject := func(n int) string {
		open := `{"since":"2026-10-05T13:30:00Z"`
		return open + strings.Repeat(" ", n-len(open)-1) + "}"
	}

	t.Run("object within the limit followed by more than the limit of whitespace", func(t *testing.T) {
		body := valid + strings.Repeat(" ", 2*limit)
		got, windowed, err := parseBackfillSince(backfillWindowRequest(body), backfillWindowNow)
		if err == nil || !strings.Contains(err.Error(), "exceeds 4096 bytes") || windowed {
			t.Fatalf("parseBackfillSince(%d bytes) = (%v, %t, %v), want a size error", len(body), got, windowed, err)
		}
	})
	t.Run("object ending exactly at the limit", func(t *testing.T) {
		body := paddedObject(limit)
		if len(body) != limit {
			t.Fatalf("fixture length = %d, want %d", len(body), limit)
		}
		got, windowed, err := parseBackfillSince(backfillWindowRequest(body), backfillWindowNow)
		if err != nil || !windowed || !got.Equal(want) {
			t.Fatalf("parseBackfillSince(%d bytes) = (%v, %t, %v), want window from %v", len(body), got, windowed, err, want)
		}
	})
	t.Run("object ending one byte past the limit", func(t *testing.T) {
		body := paddedObject(limit + 1)
		got, windowed, err := parseBackfillSince(backfillWindowRequest(body), backfillWindowNow)
		if err == nil || !strings.Contains(err.Error(), "exceeds 4096 bytes") {
			t.Fatalf("parseBackfillSince(%d bytes) = (%v, %t, %v), want a size error", len(body), got, windowed, err)
		}
		if windowed {
			t.Fatal("oversized window request reported windowed=true")
		}
	})
	t.Run("since value longer than the limit", func(t *testing.T) {
		body := `{"since":"` + strings.Repeat("9", 2*limit) + `"}`
		_, windowed, err := parseBackfillSince(backfillWindowRequest(body), backfillWindowNow)
		if err == nil || !strings.Contains(err.Error(), "exceeds 4096 bytes") || windowed {
			t.Fatalf("parseBackfillSince(long since) = (windowed %t, %v), want a size error", windowed, err)
		}
	})
	t.Run("unknown field padding past the limit", func(t *testing.T) {
		body := `{"since":"2026-10-05","pad":"` + strings.Repeat("x", 2*limit) + `"}`
		_, windowed, err := parseBackfillSince(backfillWindowRequest(body), backfillWindowNow)
		if err == nil || windowed {
			t.Fatalf("parseBackfillSince(padded unknown field) = (windowed %t, %v), want an error", windowed, err)
		}
	})
}

// backfillWindowCalls records starter invocations. The handler runs on the
// test server's goroutines, so every access is locked.
type backfillWindowCalls struct {
	mu          sync.Mutex
	deep        int
	window      []time.Time
	deepResult  bool
	windowValue bool
}

func newBackfillWindowCalls() *backfillWindowCalls {
	return &backfillWindowCalls{deepResult: true, windowValue: true}
}

func (c *backfillWindowCalls) startDeep() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deep++
	return c.deepResult
}

func (c *backfillWindowCalls) startWindow(since time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.window = append(c.window, since)
	return c.windowValue
}

func (c *backfillWindowCalls) snapshot() (int, []time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.deep, append([]time.Time(nil), c.window...)
}

func (c *backfillWindowCalls) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deep = 0
	c.window = nil
}

// backfillWindowPost sends one request through the test server built by the
// shared newTestServerWithOptions helper (api_test.go) and decodes the JSON
// response object.
func backfillWindowPost(t *testing.T, ts *testServer, method string, body string, headers map[string]string) (int, map[string]string) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request, err := http.NewRequest(method, ts.server.URL+"/api/backfill", reader)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response, err := ts.server.Client().Do(request)
	if err != nil {
		t.Fatalf("%s /api/backfill: %v", method, err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	var payload map[string]string
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("decode response %q: %v", raw, err)
	}
	return response.StatusCode, payload
}

func TestBackfillWindowHandlerStartsWindowBackfill(t *testing.T) {
	offsetInstant := time.Date(2026, 10, 5, 13, 30, 0, 0, time.UTC)
	tests := []struct {
		name string
		body string
		want time.Time
		// wantEcho is the since the response must report: RFC 3339 in the
		// parsed value's own zone, to whole seconds.
		wantEcho string
	}{
		{
			name:     "RFC 3339 with offset",
			body:     `{"since":"2026-10-05T09:30:00-04:00"}`,
			want:     offsetInstant,
			wantEcho: "2026-10-05T09:30:00-04:00",
		},
		{
			// Intended: the callback receives full precision; the echo is
			// RFC 3339 (whole seconds) for display only.
			name:     "RFC 3339 with fractional seconds",
			body:     `{"since":"2026-10-05T09:30:00.250-04:00"}`,
			want:     offsetInstant.Add(250 * time.Millisecond),
			wantEcho: "2026-10-05T09:30:00-04:00",
		},
		{
			name:     "local date",
			body:     `{"since":"2026-10-05"}`,
			want:     time.Date(2026, 10, 5, 0, 0, 0, 0, time.Local),
			wantEcho: time.Date(2026, 10, 5, 0, 0, 0, 0, time.Local).Format(time.RFC3339),
		},
		{
			name:     "unix milliseconds number",
			body:     fmt.Sprintf(`{"since":%d}`, offsetInstant.UnixMilli()),
			want:     offsetInstant,
			wantEcho: offsetInstant.In(time.Local).Format(time.RFC3339),
		},
		{
			name:     "unix milliseconds string",
			body:     fmt.Sprintf(`{"since":"%d"}`, offsetInstant.UnixMilli()),
			want:     offsetInstant,
			wantEcho: offsetInstant.In(time.Local).Format(time.RFC3339),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := newBackfillWindowCalls()
			ts := newTestServerWithOptions(t, APIOptions{
				StartDeepBackfill:   calls.startDeep,
				StartWindowBackfill: calls.startWindow,
			})
			status, payload := backfillWindowPost(t, ts, http.MethodPost, tt.body, nil)
			if status != http.StatusOK {
				t.Fatalf("status = %d (%v), want 200", status, payload)
			}
			wantPayload := map[string]string{"status": "started", "since": tt.wantEcho}
			if !reflect.DeepEqual(payload, wantPayload) {
				t.Fatalf("response = %#v, want %#v", payload, wantPayload)
			}
			deep, window := calls.snapshot()
			if deep != 0 {
				t.Fatalf("window request started %d deep backfills, want 0", deep)
			}
			if len(window) != 1 || !window[0].Equal(tt.want) {
				t.Fatalf("StartWindowBackfill calls = %v, want exactly [%s]", window, tt.want.Format(time.RFC3339Nano))
			}
			echoed, err := time.Parse(time.RFC3339, payload["since"])
			if err != nil || !echoed.Equal(tt.want.Truncate(time.Second)) {
				t.Fatalf("echoed since %q parses to %v (%v), want %v", payload["since"], echoed, err, tt.want.Truncate(time.Second))
			}
		})
	}
}

func TestBackfillWindowHandlerBusyReturnsConflict(t *testing.T) {
	calls := newBackfillWindowCalls()
	calls.windowValue = false
	ts := newTestServerWithOptions(t, APIOptions{
		StartDeepBackfill:   calls.startDeep,
		StartWindowBackfill: calls.startWindow,
	})
	status, payload := backfillWindowPost(t, ts, http.MethodPost, `{"since":"2026-10-05T13:30:00Z"}`, nil)
	if status != http.StatusConflict {
		t.Fatalf("status = %d (%v), want 409", status, payload)
	}
	if !strings.Contains(payload["error"], "already running") {
		t.Fatalf("409 error = %q, want the shared busy message", payload["error"])
	}
	deep, window := calls.snapshot()
	if deep != 0 {
		t.Fatalf("busy window request fell back to %d deep backfills", deep)
	}
	if len(window) != 1 {
		t.Fatalf("StartWindowBackfill called %d times, want 1", len(window))
	}
}

func TestBackfillWindowHandlerUnavailableReturnsNotImplemented(t *testing.T) {
	calls := newBackfillWindowCalls()
	ts := newTestServerWithOptions(t, APIOptions{
		// Deep backfill is wired; window backfill is not. A window request must
		// not quietly run the deep backfill instead.
		StartDeepBackfill: calls.startDeep,
	})
	status, payload := backfillWindowPost(t, ts, http.MethodPost, `{"since":"2026-10-05"}`, nil)
	if status != http.StatusNotImplemented {
		t.Fatalf("status = %d (%v), want 501", status, payload)
	}
	if payload["error"] != "window backfill not available" {
		t.Fatalf("501 error = %q", payload["error"])
	}
	if deep, _ := calls.snapshot(); deep != 0 {
		t.Fatalf("window request without a window starter ran %d deep backfills", deep)
	}
}

func TestBackfillWindowHandlerWithoutSinceStillStartsDeepBackfill(t *testing.T) {
	for _, body := range []string{"", "   ", `{}`, `{"since":null}`, `null`} {
		t.Run(fmt.Sprintf("body %q", body), func(t *testing.T) {
			calls := newBackfillWindowCalls()
			ts := newTestServerWithOptions(t, APIOptions{
				StartDeepBackfill:   calls.startDeep,
				StartWindowBackfill: calls.startWindow,
			})
			status, payload := backfillWindowPost(t, ts, http.MethodPost, body, nil)
			if status != http.StatusOK {
				t.Fatalf("status = %d (%v), want 200", status, payload)
			}
			if want := map[string]string{"status": "started"}; !reflect.DeepEqual(payload, want) {
				t.Fatalf("response = %#v, want %#v (no since key)", payload, want)
			}
			deep, window := calls.snapshot()
			if deep != 1 || len(window) != 0 {
				t.Fatalf("starter calls = deep %d, window %v; want deep 1, window none", deep, window)
			}
		})
	}

	t.Run("deep backfill unavailable", func(t *testing.T) {
		calls := newBackfillWindowCalls()
		ts := newTestServerWithOptions(t, APIOptions{StartWindowBackfill: calls.startWindow})
		status, payload := backfillWindowPost(t, ts, http.MethodPost, `{}`, nil)
		if status != http.StatusNotImplemented || payload["error"] != "deep backfill not available" {
			t.Fatalf("response = %d %v, want the unchanged 501 deep-backfill message", status, payload)
		}
		if _, window := calls.snapshot(); len(window) != 0 {
			t.Fatalf("a request without since started a window backfill: %v", window)
		}
	})

	t.Run("deep backfill busy", func(t *testing.T) {
		calls := newBackfillWindowCalls()
		calls.deepResult = false
		ts := newTestServerWithOptions(t, APIOptions{
			StartDeepBackfill:   calls.startDeep,
			StartWindowBackfill: calls.startWindow,
		})
		status, _ := backfillWindowPost(t, ts, http.MethodPost, "", nil)
		if status != http.StatusConflict {
			t.Fatalf("status = %d, want the unchanged 409", status)
		}
		deep, window := calls.snapshot()
		if deep != 1 || len(window) != 0 {
			t.Fatalf("starter calls = deep %d, window %v", deep, window)
		}
	})
}

func TestBackfillWindowHandlerRejectsBadSince(t *testing.T) {
	future := time.Now().Add(72 * time.Hour).UTC().Format(time.RFC3339)
	tests := []struct {
		name    string
		body    string
		wantErr string
	}{
		{name: "future", body: `{"since":"` + future + `"}`, wantErr: "since is in the future"},
		{name: "zero", body: `{"since":0}`, wantErr: "after the Unix epoch"},
		{name: "negative", body: `{"since":-5}`, wantErr: "after the Unix epoch"},
		{name: "typo field", body: `{"sinc":"2026-10-05"}`, wantErr: `unknown field "sinc"`},
		{name: "malformed JSON", body: `{"since":`, wantErr: "invalid JSON"},
		{name: "free text", body: `{"since":"last tuesday"}`, wantErr: `since "last tuesday" must be`},
		{name: "over the size limit", body: `{"since":"2026-10-05"` + strings.Repeat(" ", 5000) + `}`, wantErr: "exceeds 4096 bytes"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := newBackfillWindowCalls()
			ts := newTestServerWithOptions(t, APIOptions{
				StartDeepBackfill:   calls.startDeep,
				StartWindowBackfill: calls.startWindow,
			})
			status, payload := backfillWindowPost(t, ts, http.MethodPost, tt.body, nil)
			if status != http.StatusBadRequest {
				t.Fatalf("status = %d (%v), want 400", status, payload)
			}
			if !strings.Contains(payload["error"], tt.wantErr) {
				t.Fatalf("400 error = %q, want it to contain %q", payload["error"], tt.wantErr)
			}
			deep, window := calls.snapshot()
			if deep != 0 || len(window) != 0 {
				t.Fatalf("bad since started backfills: deep %d, window %v", deep, window)
			}
		})
	}
}

func TestBackfillWindowHandlerRejectsNonPost(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		t.Run(method, func(t *testing.T) {
			calls := newBackfillWindowCalls()
			ts := newTestServerWithOptions(t, APIOptions{
				StartDeepBackfill:   calls.startDeep,
				StartWindowBackfill: calls.startWindow,
			})
			status, payload := backfillWindowPost(t, ts, method, `{"since":"2026-10-05"}`, nil)
			if status != http.StatusMethodNotAllowed || payload["error"] != "method not allowed" {
				t.Fatalf("%s response = %d %v, want 405", method, status, payload)
			}
			deep, window := calls.snapshot()
			if deep != 0 || len(window) != 0 {
				t.Fatalf("%s started backfills: deep %d, window %v", method, deep, window)
			}
		})
	}
}

// The local-control guard (ProtectLocalControl) runs before the handler, so a
// cross-origin page cannot start a window backfill either.
func TestBackfillWindowHandlerCrossOriginForbidden(t *testing.T) {
	calls := newBackfillWindowCalls()
	ts := newTestServerWithOptions(t, APIOptions{
		StartDeepBackfill:   calls.startDeep,
		StartWindowBackfill: calls.startWindow,
	})
	status, _ := backfillWindowPost(t, ts, http.MethodPost, `{"since":"2026-10-05"}`, map[string]string{
		"Origin": "http://evil.example.com",
	})
	if status != http.StatusForbidden {
		t.Fatalf("cross-origin window request status = %d, want 403", status)
	}
	deep, window := calls.snapshot()
	if deep != 0 || len(window) != 0 {
		t.Fatalf("cross-origin request started backfills: deep %d, window %v", deep, window)
	}

	status, _ = backfillWindowPost(t, ts, http.MethodPost, `{"since":"2026-10-05"}`, map[string]string{
		"Origin": ts.server.URL,
	})
	if status != http.StatusOK {
		t.Fatalf("same-origin window request status = %d, want 200", status)
	}
	if _, window := calls.snapshot(); len(window) != 1 {
		t.Fatalf("same-origin window request starts = %v, want 1", window)
	}
}

// ---------------------------------------------------------------------------
// Properties (testing/quick, repo convention: custom Generate, fixed seed).

func backfillWindowQuickConfig(seed int64) *quick.Config {
	return &quick.Config{MaxCount: 400, Rand: rand.New(rand.NewSource(seed))}
}

// backfillWindowZone is a fixed offset between UTC-12 and UTC+14 in quarter
// hours, covering the half- and quarter-hour zones.
func backfillWindowZone(r *rand.Rand) *time.Location {
	offset := (r.Intn(26*4+1) - 12*4) * 15 * 60
	return time.FixedZone(fmt.Sprintf("Q%+d", offset), offset)
}

// backfillWindowValidInstant is a random accepted window start: in
// [minBackfillSinceMS, now], biased toward both boundaries.
type backfillWindowValidInstant struct {
	MS   int64
	Zone *time.Location
	Nano int
}

func (backfillWindowValidInstant) Generate(r *rand.Rand, _ int) reflect.Value {
	nowMS := backfillWindowNow.UnixMilli()
	var ms int64
	switch r.Intn(6) {
	case 0:
		ms = minBackfillSinceMS + r.Int63n(1000)
	case 1:
		ms = nowMS - r.Int63n(1000)
	default:
		ms = minBackfillSinceMS + r.Int63n(nowMS-minBackfillSinceMS)
	}
	return reflect.ValueOf(backfillWindowValidInstant{MS: ms, Zone: backfillWindowZone(r), Nano: r.Intn(1_000_000)})
}

// Round trip: every accepted instant, in each accepted encoding, parses back to
// exactly that instant with windowed=true.
func TestParseBackfillSincePropertyRoundTrip(t *testing.T) {
	property := func(c backfillWindowValidInstant) bool {
		instant := time.UnixMilli(c.MS)
		bodies := map[string]time.Time{
			fmt.Sprintf(`{"since":%d}`, c.MS):                                    instant,
			fmt.Sprintf(`{"since":"%d"}`, c.MS):                                  instant,
			fmt.Sprintf(`{"since":%q}`, instant.In(c.Zone).Format(time.RFC3339)): instant.Truncate(time.Second),
		}
		// Sub-millisecond precision survives the RFC 3339 form, as long as the
		// instant stays at or before now.
		precise := instant.Add(time.Duration(c.Nano))
		if !precise.After(backfillWindowNow) {
			bodies[fmt.Sprintf(`{"since":%q}`, precise.In(c.Zone).Format(time.RFC3339Nano))] = precise
		}
		for body, want := range bodies {
			got, windowed, err := parseBackfillSince(backfillWindowRequest(body), backfillWindowNow)
			if want.UnixMilli() <= 0 {
				// Truncating to whole seconds can land on or before the epoch.
				if err == nil || windowed {
					t.Logf("body %s: want epoch rejection, got (%v, %t, %v)", body, got, windowed, err)
					return false
				}
				continue
			}
			if err != nil || !windowed || !got.Equal(want) {
				t.Logf("body %s: got (%s, %t, %v), want %s", body, got.Format(time.RFC3339Nano), windowed, err, want.Format(time.RFC3339Nano))
				return false
			}
		}
		return true
	}
	if err := quick.Check(property, backfillWindowQuickConfig(20261008)); err != nil {
		t.Fatal(err)
	}
}

// backfillWindowRejectedMS is a millisecond value outside
// [minBackfillSinceMS, now]. NumericOnly marks values that are rejected only in
// the millisecond encodings (Unix-seconds-sized numbers): the same instant as
// an explicit RFC 3339 time is a legitimate, if early, window start.
type backfillWindowRejectedMS struct {
	MS          int64
	NumericOnly bool
}

func (backfillWindowRejectedMS) Generate(r *rand.Rand, _ int) reflect.Value {
	nowMS := backfillWindowNow.UnixMilli()
	var ms int64
	switch r.Intn(6) {
	case 0:
		ms = nowMS + 1 + r.Int63n(1000)
	case 1:
		ms = nowMS + 1 + r.Int63n(1<<50)
	case 2:
		ms = -r.Int63n(1000)
	case 3:
		ms = -r.Int63n(1 << 50)
	case 4:
		return reflect.ValueOf(backfillWindowRejectedMS{MS: 1 + r.Int63n(minBackfillSinceMS-1), NumericOnly: true})
	default:
		ms = 0
	}
	return reflect.ValueOf(backfillWindowRejectedMS{MS: ms})
}

// Bounds: a start in the future or at/before the epoch is always an error, in
// both millisecond encodings and RFC 3339.
func TestParseBackfillSincePropertyRejectsOutOfRange(t *testing.T) {
	property := func(c backfillWindowRejectedMS) bool {
		bodies := []string{
			fmt.Sprintf(`{"since":%d}`, c.MS),
			fmt.Sprintf(`{"since":"%d"}`, c.MS),
		}
		if instant := time.UnixMilli(c.MS); !c.NumericOnly && instant.Year() >= 0 && instant.Year() <= 9999 {
			bodies = append(bodies, fmt.Sprintf(`{"since":%q}`, instant.UTC().Format(time.RFC3339Nano)))
		}
		for _, body := range bodies {
			got, windowed, err := parseBackfillSince(backfillWindowRequest(body), backfillWindowNow)
			if err == nil || windowed || !got.IsZero() {
				t.Logf("body %s: got (%v, %t, %v), want rejection", body, got, windowed, err)
				return false
			}
		}
		return true
	}
	if err := quick.Check(property, backfillWindowQuickConfig(20261009)); err != nil {
		t.Fatal(err)
	}
}

// backfillWindowBody is a random, often malformed, request body built from
// JSON fragments: since values of every type, typo and case-variant keys,
// extra fields, whitespace, and random truncation.
type backfillWindowBody struct{ Body string }

var backfillWindowKeys = []string{"since", "since", "since", "Since", "SINCE", "sinc", "sinse", "since_ms", "deep", "", " since"}

func backfillWindowValue(r *rand.Rand) string {
	switch r.Intn(14) {
	case 0:
		return "null"
	case 1:
		return strconv.FormatInt(r.Int63n(2*backfillWindowNow.UnixMilli())-backfillWindowNow.UnixMilli()/2, 10)
	case 2:
		return strconv.Quote(strconv.FormatInt(r.Int63n(2*backfillWindowNow.UnixMilli()), 10))
	case 3:
		at := time.Unix(r.Int63n(4_000_000_000)-1_000_000_000, int64(r.Intn(1e9)))
		return strconv.Quote(at.In(backfillWindowZone(r)).Format(time.RFC3339Nano))
	case 4:
		return strconv.Quote(fmt.Sprintf("%04d-%02d-%02d", 1960+r.Intn(80), 1+r.Intn(12), 1+r.Intn(28)))
	case 5:
		return "true"
	case 6:
		return `""`
	case 7:
		return "{}"
	case 8:
		return "[1,2]"
	case 9:
		return strconv.FormatFloat(r.Float64()*2e12, 'g', -1, 64)
	case 10:
		return strconv.Quote("yesterday")
	case 11:
		return "99999999999999999999"
	case 12:
		return `"2026-10-05"`
	default:
		return strconv.Quote(string(rune('a' + r.Intn(26))))
	}
}

func (backfillWindowBody) Generate(r *rand.Rand, _ int) reflect.Value {
	space := func() string { return strings.Repeat([]string{" ", "\n", "\t", "\r"}[r.Intn(4)], r.Intn(3)) }
	var body string
	switch r.Intn(10) {
	case 0:
		body = space()
	case 1:
		raw := make([]byte, r.Intn(24))
		for i := range raw {
			raw[i] = byte(r.Intn(256))
		}
		body = string(raw)
	case 2:
		body = backfillWindowValue(r)
	default:
		fields := make([]string, 0, 3)
		for i, n := 0, r.Intn(3); i < n; i++ {
			fields = append(fields, fmt.Sprintf("%s%q%s:%s%s", space(), backfillWindowKeys[r.Intn(len(backfillWindowKeys))], space(), space(), backfillWindowValue(r)))
		}
		body = space() + "{" + strings.Join(fields, ",") + "}" + space()
	}
	if r.Intn(6) == 0 && len(body) > 0 {
		body = body[:r.Intn(len(body))]
	}
	return reflect.ValueOf(backfillWindowBody{Body: body})
}

// Contract invariants for every body: an error never comes with a window; a
// window is always in (epoch, now]; no window and no error means zero since;
// and the result is deterministic.
func TestParseBackfillSincePropertyContract(t *testing.T) {
	property := func(c backfillWindowBody) bool {
		got, windowed, err := parseBackfillSince(backfillWindowRequest(c.Body), backfillWindowNow)
		switch {
		case err != nil:
			if windowed || !got.IsZero() {
				t.Logf("body %q: error %v came with (%v, %t)", c.Body, err, got, windowed)
				return false
			}
		case windowed:
			if got.UnixMilli() <= 0 || got.After(backfillWindowNow) {
				t.Logf("body %q: window %v outside (epoch, now]", c.Body, got)
				return false
			}
		default:
			if !got.IsZero() {
				t.Logf("body %q: no window but since %v", c.Body, got)
				return false
			}
		}
		again, againWindowed, againErr := parseBackfillSince(backfillWindowRequest(c.Body), backfillWindowNow)
		if !again.Equal(got) || againWindowed != windowed || fmt.Sprint(againErr) != fmt.Sprint(err) {
			t.Logf("body %q: nondeterministic result", c.Body)
			return false
		}
		return true
	}
	if err := quick.Check(property, backfillWindowQuickConfig(20261010)); err != nil {
		t.Fatal(err)
	}
}

// backfillWindowTypoBody is a single JSON object (well under the size limit)
// with at least one key that is not since under any casing, next to a valid
// since.
type backfillWindowTypoBody struct{ Body string }

func (backfillWindowTypoBody) Generate(r *rand.Rand, _ int) reflect.Value {
	typos := []string{"sinc", "sinse", "since_ms", "deep", "from", "start", "", " since", "since ", "sincе"} // last has a Cyrillic e
	typo := typos[r.Intn(len(typos))]
	fields := []string{fmt.Sprintf("%q:%s", typo, backfillWindowValue(r))}
	if r.Intn(2) == 0 {
		fields = append(fields, `"since":"2026-10-05T13:30:00Z"`)
	}
	r.Shuffle(len(fields), func(i, j int) { fields[i], fields[j] = fields[j], fields[i] })
	return reflect.ValueOf(backfillWindowTypoBody{Body: "{" + strings.Join(fields, ",") + "}"})
}

// Fail closed on typos: an object carrying any key that is not since (under
// any casing) is an error, so a typo can never become a deep backfill (no
// window, no error) or a window from a value the client did not mean.
func TestParseBackfillSincePropertyTypoFailsClosed(t *testing.T) {
	property := func(c backfillWindowTypoBody) bool {
		_, windowed, err := parseBackfillSince(backfillWindowRequest(c.Body), backfillWindowNow)
		if err == nil || windowed {
			t.Logf("body %s: got (windowed %t, %v), want an error", c.Body, windowed, err)
			return false
		}
		return true
	}
	if err := quick.Check(property, backfillWindowQuickConfig(20261011)); err != nil {
		t.Fatal(err)
	}
}

// backfillWindowHandlerCase is a body whose classification cannot depend on
// the instant the handler reads time.Now(): accepted values are at least a
// week old, future values at least two days ahead.
type backfillWindowHandlerCase struct{ Body string }

func (backfillWindowHandlerCase) Generate(r *rand.Rand, _ int) reflect.Value {
	past := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	pastMS := 1 + r.Int63n(past.UnixMilli())
	futureMS := time.Now().Add(48*time.Hour).UnixMilli() + r.Int63n(1<<40)
	var body string
	switch r.Intn(11) {
	case 0:
		body = ""
	case 1:
		body = `{}`
	case 2:
		body = `{"since":null}`
	case 3:
		body = fmt.Sprintf(`{"since":%d}`, pastMS)
	case 4:
		body = fmt.Sprintf(`{"since":"%d"}`, pastMS)
	case 5:
		body = fmt.Sprintf(`{"since":%q}`, time.UnixMilli(pastMS).In(backfillWindowZone(r)).Format(time.RFC3339Nano))
	case 6:
		body = fmt.Sprintf(`{"since":%d}`, futureMS)
	case 7:
		body = fmt.Sprintf(`{"since":%d}`, -r.Int63n(1<<40))
	case 8:
		body = fmt.Sprintf(`{"sinc":%d}`, pastMS)
	case 9:
		body = fmt.Sprintf(`{"since":%q}`, time.UnixMilli(pastMS).In(time.Local).Format("2006-01-02"))
	default:
		body = `{"since":`
	}
	return reflect.ValueOf(backfillWindowHandlerCase{Body: body})
}

// Differential: the handler's outcome is exactly what parseBackfillSince
// decides. A parse error is a 400 that starts nothing; a window is a 200 that
// starts exactly one window backfill with the parsed instant; no window is the
// unchanged deep backfill.
func TestBackfillWindowHandlerPropertyMatchesParser(t *testing.T) {
	calls := newBackfillWindowCalls()
	ts := newTestServerWithOptions(t, APIOptions{
		StartDeepBackfill:   calls.startDeep,
		StartWindowBackfill: calls.startWindow,
	})
	property := func(c backfillWindowHandlerCase) bool {
		calls.reset()
		since, windowed, parseErr := parseBackfillSince(backfillWindowRequest(c.Body), time.Now())
		status, payload := backfillWindowPost(t, ts, http.MethodPost, c.Body, nil)
		deep, window := calls.snapshot()
		switch {
		case parseErr != nil:
			if status != http.StatusBadRequest || payload["error"] != parseErr.Error() || deep != 0 || len(window) != 0 {
				t.Logf("body %q: parser error %v, handler %d %v deep=%d window=%v", c.Body, parseErr, status, payload, deep, window)
				return false
			}
		case windowed:
			if status != http.StatusOK || deep != 0 || len(window) != 1 || !window[0].Equal(since) ||
				payload["since"] != since.Format(time.RFC3339) {
				t.Logf("body %q: parser window %v, handler %d %v deep=%d window=%v", c.Body, since, status, payload, deep, window)
				return false
			}
		default:
			if status != http.StatusOK || deep != 1 || len(window) != 0 || payload["since"] != "" {
				t.Logf("body %q: parser no window, handler %d %v deep=%d window=%v", c.Body, status, payload, deep, window)
				return false
			}
		}
		return true
	}
	config := backfillWindowQuickConfig(20261012)
	config.MaxCount = 150
	if err := quick.Check(property, config); err != nil {
		t.Fatal(err)
	}
}
