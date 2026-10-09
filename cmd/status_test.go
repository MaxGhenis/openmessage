package cmd

import (
	"testing"
	"time"
)

func TestCommaInt(t *testing.T) {
	cases := []struct {
		in   int
		want string
	}{
		{0, "0"},
		{5, "5"},
		{42, "42"},
		{100, "100"},
		{999, "999"},
		{1000, "1,000"},
		{1234, "1,234"},
		{12345, "12,345"},
		{123456, "123,456"},
		{1234567, "1,234,567"},
		{174212, "174,212"},
		{-1234, "-1,234"},
		{-1000000, "-1,000,000"},
	}
	for _, c := range cases {
		if got := commaInt(c.in); got != c.want {
			t.Errorf("commaInt(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestHumanAge(t *testing.T) {
	now := time.UnixMilli(1_000_000_000_000) // fixed reference instant
	day := int64(24 * 60 * 60 * 1000)

	if got := humanAge(0, now); got != "—" {
		t.Errorf("humanAge(0) = %q, want em dash", got)
	}
	if got := humanAge(now.UnixMilli(), now); got != "today" {
		t.Errorf("humanAge(now) = %q, want today", got)
	}
	if got := humanAge(now.UnixMilli()-day, now); got != "1d ago" {
		t.Errorf("humanAge(now-1d) = %q, want 1d ago", got)
	}
	if got := humanAge(now.UnixMilli()-19*day, now); got != "19d ago" {
		t.Errorf("humanAge(now-19d) = %q, want 19d ago", got)
	}
	// A future timestamp clamps to "today" rather than going negative.
	if got := humanAge(now.UnixMilli()+5*day, now); got != "today" {
		t.Errorf("humanAge(future) = %q, want today", got)
	}
}

func TestDaysBetween(t *testing.T) {
	day := int64(24 * 60 * 60 * 1000)
	base := int64(1_000_000_000_000)

	if got := daysBetween(0, base); got != 0 {
		t.Errorf("daysBetween(0, base) = %d, want 0", got)
	}
	if got := daysBetween(base, base); got != 0 {
		t.Errorf("daysBetween(equal) = %d, want 0", got)
	}
	if got := daysBetween(base, base-day); got != 0 {
		t.Errorf("daysBetween(older newer than newer) = %d, want 0", got)
	}
	if got := daysBetween(base, base+14*day); got != 14 {
		t.Errorf("daysBetween(14d behind) = %d, want 14", got)
	}
}

func TestStaleWarning(t *testing.T) {
	hours := func(h time.Duration) int64 { return (h * time.Hour).Milliseconds() }
	cases := []struct {
		name    string
		behind  int
		silence *platformSilence
		want    string
	}{
		{"fresh, no verdict", 0, nil, ""},
		{"fresh verdict", 0, &platformSilence{SilentMS: hours(2)}, ""},
		{"long quiet the baseline explains", 0, &platformSilence{SilentMS: hours(11)}, ""},
		{"behind peers", 14, nil, "  ⚠ 14d behind"},
		{"at the behind threshold", 3, nil, "  ⚠ 3d behind"},
		{"just under the behind threshold", 2, nil, ""},
		// The 2026-10-06 stall: Google was the newest platform, so it was never
		// behind; its silence verdict is what flags it.
		{"silent newest platform", 0, &platformSilence{SilentMS: hours(13) + 60_000, Stalled: true}, "  ⚠ silent 13h"},
		{"hours are floored", 0, &platformSilence{SilentMS: hours(14) - 60_000, Stalled: true}, "  ⚠ silent 13h"},
		{"silent for days", 0, &platformSilence{SilentMS: hours(80), Stalled: true}, "  ⚠ silent 80h"},
		// A stalled verdict whose silent_ms was missing or negative.
		{"stalled, hours unknown", 0, &platformSilence{Stalled: true}, "  ⚠ silent"},
		// Behind outranks silent, as /api/status stale_reason does.
		{"behind and silent", 5, &platformSilence{SilentMS: hours(120), Stalled: true}, "  ⚠ 5d behind"},
	}
	for _, c := range cases {
		if got := staleWarning(c.behind, c.silence); got != c.want {
			t.Errorf("%s: staleWarning = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestFirstNonEmpty(t *testing.T) {
	if got := firstNonEmpty("", "  ", "Alice", "Bob"); got != "Alice" {
		t.Errorf("firstNonEmpty = %q, want Alice", got)
	}
	if got := firstNonEmpty("", "   ", ""); got != "" {
		t.Errorf("firstNonEmpty(all blank) = %q, want empty", got)
	}
	if got := firstNonEmpty("first"); got != "first" {
		t.Errorf("firstNonEmpty(single) = %q, want first", got)
	}
}
