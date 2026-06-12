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
