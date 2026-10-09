package cmd

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/maxghenis/openmessage/internal/storage/sqlite"
	"github.com/maxghenis/openmessage/internal/web"
)

func TestInboxPayloadRetentionFromEnv(t *testing.T) {
	day := 24 * time.Hour
	tests := []struct {
		name     string
		value    string
		unset    bool
		want     time.Duration
		wantNote string
	}{
		{name: "unset", unset: true, want: sqlite.DefaultInboxPayloadRetention},
		{name: "empty", value: "", want: sqlite.DefaultInboxPayloadRetention},
		{name: "zero disables", value: "0", want: 0},
		{name: "longer", value: "90", want: 90 * day},
		{name: "whitespace", value: " 75\t", want: 75 * day},
		{name: "at floor", value: "45", want: 45 * day},
		{name: "below floor", value: "10", want: sqlite.MinInboxPayloadRetention, wantNote: "below the 45-day floor"},
		{name: "one day", value: "1", want: sqlite.MinInboxPayloadRetention, wantNote: "below the 45-day floor"},
		{name: "negative", value: "-3", want: sqlite.DefaultInboxPayloadRetention, wantNote: "not a whole number"},
		{name: "fraction", value: "1.5", want: sqlite.DefaultInboxPayloadRetention, wantNote: "not a whole number"},
		{name: "word", value: "off", want: sqlite.DefaultInboxPayloadRetention, wantNote: "not a whole number"},
		{name: "at cap", value: "36500", want: 36500 * day},
		{name: "above cap", value: "36501", want: 36500 * day, wantNote: "above the 36500-day cap"},
		// 106,752 days is the first count whose nanoseconds overflow int64.
		{name: "duration overflow", value: "106752", want: 36500 * day, wantNote: "above the 36500-day cap"},
		{name: "far past overflow", value: "999999999999", want: 36500 * day, wantNote: "above the 36500-day cap"},
		{name: "integer overflow", value: "99999999999999999999999", want: sqlite.DefaultInboxPayloadRetention, wantNote: "not a whole number"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(inboxRetentionEnv, tt.value)
			if tt.unset {
				if err := os.Unsetenv(inboxRetentionEnv); err != nil {
					t.Fatalf("unset %s: %v", inboxRetentionEnv, err)
				}
			}
			got, note := inboxPayloadRetention()
			if got != tt.want {
				t.Fatalf("inboxPayloadRetention() = %s, want %s for %q", got, tt.want, tt.value)
			}
			if tt.wantNote == "" && note != "" {
				t.Fatalf("note = %q, want none for %q", note, tt.value)
			}
			if tt.wantNote != "" && !strings.Contains(note, tt.wantNote) {
				t.Fatalf("note = %q, want it to mention %q", note, tt.wantNote)
			}
			if got != 0 && got < sqlite.MinInboxPayloadRetention {
				t.Fatalf("retention %s is below the floor", got)
			}
			// Whatever the value, the stack must start: a retention the pruner
			// rejects would keep the daemon from coming up.
			stack, err := newV2Stack(v2StackDeps{Logger: zerolog.Nop(), DataDir: t.TempDir()})
			if err != nil {
				t.Fatalf("newV2Stack() with %s=%q: %v", inboxRetentionEnv, tt.value, err)
			}
			if err := stack.Store.Close(); err != nil {
				t.Fatalf("close store: %v", err)
			}
			if (stack.pruner != nil) != (got != 0) {
				t.Fatalf("pruner configured = %t for retention %s", stack.pruner != nil, got)
			}
		})
	}
}

func TestNewV2StackRunsInboxPayloadPrunerUnlessDisabled(t *testing.T) {
	for _, test := range []struct {
		name       string
		value      string
		wantPruner bool
	}{
		{name: "default", value: "", wantPruner: true},
		{name: "disabled", value: "0", wantPruner: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv(inboxRetentionEnv, test.value)
			stack, err := newV2Stack(v2StackDeps{Logger: zerolog.Nop(), DataDir: t.TempDir()})
			if err != nil {
				t.Fatalf("newV2Stack(): %v", err)
			}
			if (stack.pruner != nil) != test.wantPruner {
				t.Fatalf("pruner configured = %t, want %t", stack.pruner != nil, test.wantPruner)
			}
			// Start and stop must not hang on the pruner's start delay.
			stop := stack.Start(context.Background(), nil, web.NewEventBroker(), true)
			stopped := make(chan struct{})
			go func() {
				stop()
				close(stopped)
			}()
			select {
			case <-stopped:
			case <-time.After(10 * time.Second):
				t.Fatal("stack stop did not return")
			}
		})
	}
}
