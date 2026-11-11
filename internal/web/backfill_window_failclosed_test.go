package web

// Fail-closed regressions for parseBackfillSince. Each body below once parsed
// into a deep backfill or a window from 1970 (found by review before merge);
// the parser now rejects all of them, as its doc promises ("a typo cannot
// silently turn a window request into a full deep backfill").

import (
	"net/http"
	"strings"
	"testing"
)

// Unix seconds must not be read as Unix milliseconds: 1759671000 (2025-10-05
// in seconds) would become 1970-01-21, a window that starts before every
// message, in effect the deep backfill the window path exists to avoid.
func TestParseBackfillSinceRejectsUnixSeconds(t *testing.T) {
	got, windowed, err := parseBackfillSince(backfillWindowRequest(`{"since":1759671000}`), backfillWindowNow)
	if err == nil {
		t.Fatalf("parseBackfillSince(unix seconds) = (%s, windowed %t, nil); want an error, not a window from 1970", got.UTC(), windowed)
	}
}

// A window request whose JSON starts after 4096 bytes of leading whitespace
// must not be truncated into an empty body (which starts the deep backfill).
func TestParseBackfillSinceRejectsBodyPastLimit(t *testing.T) {
	body := strings.Repeat(" ", 4096) + `{"since":"2026-10-05T13:30:00Z"}`
	got, windowed, err := parseBackfillSince(backfillWindowRequest(body), backfillWindowNow)
	if err == nil && !windowed {
		t.Fatalf("parseBackfillSince(%d-byte window request) = (%v, false, nil): treated as an empty body (deep backfill); want an error", len(body), got)
	}

	calls := newBackfillWindowCalls()
	ts := newTestServerWithOptions(t, APIOptions{
		StartDeepBackfill:   calls.startDeep,
		StartWindowBackfill: calls.startWindow,
	})
	status, _ := backfillWindowPost(t, ts, http.MethodPost, body, nil)
	if deep, _ := calls.snapshot(); deep != 0 {
		t.Fatalf("POST /api/backfill with a padded since body started %d deep backfills (status %d); want 400", deep, status)
	}
}

// Only the first JSON value is decoded; anything after it is ignored. A body
// whose first value is {} runs the deep backfill even though a since object
// follows.
func TestParseBackfillSinceRejectsTrailingData(t *testing.T) {
	for _, body := range []string{
		`{}{"since":"2026-10-05T13:30:00Z"}`,
		`{"since":null} {"since":"2026-10-05T13:30:00Z"}`,
		`{"since":"2026-10-05T13:30:00Z"} trailing garbage`,
	} {
		got, windowed, err := parseBackfillSince(backfillWindowRequest(body), backfillWindowNow)
		if err == nil {
			t.Errorf("parseBackfillSince(%q) = (%v, windowed %t, nil); want an error for trailing data", body, got, windowed)
		}
	}
}
