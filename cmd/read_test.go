package cmd

import (
	"testing"
	"time"
)

func TestParseDayBound(t *testing.T) {
	if ms, err := parseDayBound("", false); err != nil || ms != 0 {
		t.Errorf("empty: got %d, %v; want 0, nil", ms, err)
	}

	start, err := parseDayBound("2026-05-18", false)
	if err != nil {
		t.Fatalf("since parse: %v", err)
	}
	wantStart := time.Date(2026, 5, 18, 0, 0, 0, 0, time.Local).UnixMilli()
	if start != wantStart {
		t.Errorf("since: got %d, want %d", start, wantStart)
	}

	end, err := parseDayBound("2026-05-18", true)
	if err != nil {
		t.Fatalf("until parse: %v", err)
	}
	wantEnd := time.Date(2026, 5, 18, 0, 0, 0, 0, time.Local).
		Add(24*time.Hour - time.Millisecond).UnixMilli()
	if end != wantEnd {
		t.Errorf("until: got %d, want %d", end, wantEnd)
	}
	if end <= start {
		t.Errorf("endOfDay (%d) must be after startOfDay (%d)", end, start)
	}

	// Explicit datetime is accepted verbatim (not pushed to end of day).
	dt, err := parseDayBound("2026-05-18 14:30", true)
	if err != nil {
		t.Fatalf("datetime parse: %v", err)
	}
	wantDT := time.Date(2026, 5, 18, 14, 30, 0, 0, time.Local).UnixMilli()
	if dt != wantDT {
		t.Errorf("datetime: got %d, want %d", dt, wantDT)
	}

	if _, err := parseDayBound("not-a-date", false); err == nil {
		t.Error("expected error for invalid date")
	}
}
