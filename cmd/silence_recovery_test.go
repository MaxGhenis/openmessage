package cmd

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/maxghenis/openmessage/internal/app"
	"github.com/maxghenis/openmessage/internal/silencerecovery"
)

func TestGoogleWindowBackfillReady(t *testing.T) {
	healthy := app.GoogleStatusSnapshot{Paired: true, Connected: true, PhoneResponding: true}
	cases := []struct {
		name   string
		mutate func(*app.GoogleStatusSnapshot)
		ready  bool
		reason string
	}{
		{"healthy", func(*app.GoogleStatusSnapshot) {}, true, ""},
		{"unpaired", func(s *app.GoogleStatusSnapshot) { s.Paired = false }, false, "not_paired"},
		{"disconnected", func(s *app.GoogleStatusSnapshot) { s.Connected = false }, false, "disconnected"},
		{"auth expired", func(s *app.GoogleStatusSnapshot) { s.AuthExpired = true }, false, "auth_expired"},
		{"needs repair", func(s *app.GoogleStatusSnapshot) { s.NeedsRepair = true }, false, "needs_repair"},
		{"phone silent", func(s *app.GoogleStatusSnapshot) { s.PhoneResponding = false }, false, "phone_not_responding"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status := healthy
			tc.mutate(&status)
			ready, reason := googleWindowBackfillReady(status)
			if ready != tc.ready || reason != tc.reason {
				t.Fatalf("ready/reason = %v/%q, want %v/%q", ready, reason, tc.ready, tc.reason)
			}
		})
	}
}

func TestSilenceRecoveryKillSwitch(t *testing.T) {
	for _, value := range []string{"0", "false", "OFF", " no "} {
		t.Setenv(silenceRecoveryEnv, value)
		if !silenceRecoveryDisabled() {
			t.Fatalf("%s=%q did not disable the recovery", silenceRecoveryEnv, value)
		}
	}
	for _, value := range []string{"", "1", "on"} {
		t.Setenv(silenceRecoveryEnv, value)
		if silenceRecoveryDisabled() {
			t.Fatalf("%s=%q disabled the recovery", silenceRecoveryEnv, value)
		}
	}
}

func TestNewGoogleSilenceRecovery(t *testing.T) {
	a := &app.App{DataDir: t.TempDir(), Logger: zerolog.Nop()}
	if rec, reason := newGoogleSilenceRecovery(a, nil, true, zerolog.Nop()); rec != nil || reason != "no activity clock" {
		t.Fatalf("without a clock = %v, %q", rec, reason)
	}
	source := freshnessActivitySource(nil, nil, false)
	if source != nil {
		t.Fatal("expected no activity source without stores")
	}

	t.Setenv(silenceRecoveryEnv, "0")
	stub := stubActivitySource{}
	if rec, reason := newGoogleSilenceRecovery(a, stub, true, zerolog.Nop()); rec != nil || reason != "disabled by "+silenceRecoveryEnv {
		t.Fatalf("with the kill switch = %v, %q", rec, reason)
	}
	t.Setenv(silenceRecoveryEnv, "")
	rec, reason := newGoogleSilenceRecovery(a, stub, true, zerolog.Nop())
	if rec == nil || reason != "" {
		t.Fatalf("enabled = %v, %q", rec, reason)
	}
	snap := rec.Snapshot()
	if !snap.Enabled || snap.StatePath == "" || !snap.RequireV2 || snap.MaxFailedAttempts != len(silencerecovery.DefaultConfig().Backoff)+1 {
		t.Fatalf("snapshot = %+v", snap)
	}
	legacy, _ := newGoogleSilenceRecovery(&app.App{DataDir: t.TempDir(), Logger: zerolog.Nop()}, stub, false, zerolog.Nop())
	if legacy == nil || legacy.Snapshot().RequireV2 {
		t.Fatal("a legacy-reads daemon required v2 hand-offs")
	}

	encoded, err := json.Marshal(silenceRecoveryStatus(nil, "transports disabled")())
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != `{"google":{"enabled":false,"reason":"transports disabled"}}` {
		t.Fatalf("disabled status = %s", encoded)
	}
}

// TestSilenceRecoveryRunResultCarriesEveryCounter fills each field of the app
// result with a distinct value and checks it lands in the matching field, so
// a counter dropped or crossed in the adapter fails here.
func TestSilenceRecoveryRunResultCarriesEveryCounter(t *testing.T) {
	in := app.GoogleWindowBackfillResult{
		Connected:          true,
		Aborted:            true,
		Listed:             1,
		Conversations:      2,
		Messages:           3,
		EmptyConversations: 4,
		Errors:             5,
		HistoryTeed:        6,
		HistoryTeeFailed:   7,
		InboxOutcome:       app.GooglePullEmpty,
	}
	want := silencerecovery.RunResult{
		Connected:          true,
		Aborted:            true,
		Listed:             1,
		Conversations:      2,
		Messages:           3,
		EmptyConversations: 4,
		Errors:             5,
		HistoryTeed:        6,
		HistoryTeeFailed:   7,
		InboxOutcome:       silencerecovery.InboxEmpty,
	}
	if got := silenceRecoveryRunResult(in); got != want {
		t.Fatalf("mapped %+v, want %+v", got, want)
	}
	// Every RunResult field is set above, so a field added later without a
	// mapping fails this check.
	value := reflect.ValueOf(want)
	for i := 0; i < value.NumField(); i++ {
		if value.Field(i).IsZero() {
			t.Fatalf("RunResult.%s is not covered by this test", value.Type().Field(i).Name)
		}
	}
}

// stubActivitySource is an activity clock with no activity.
type stubActivitySource struct{}

func (stubActivitySource) Name() string { return "stub" }

func (stubActivitySource) Latest(context.Context) (map[string]time.Time, error) {
	return map[string]time.Time{}, nil
}

func (stubActivitySource) Between(context.Context, string, time.Time, time.Time) ([]time.Time, error) {
	return nil, nil
}

// TestInboxOutcomeNamesMatchPullHealth pins the recovery's inbox outcome names
// to pull health's, which the adapter passes through as strings.
func TestInboxOutcomeNamesMatchPullHealth(t *testing.T) {
	pairs := map[app.GooglePullOutcome]string{
		app.GooglePullOK:        silencerecovery.InboxOK,
		app.GooglePullEmpty:     silencerecovery.InboxEmpty,
		app.GooglePullNoPayload: silencerecovery.InboxNoPayload,
		app.GooglePullError:     silencerecovery.InboxError,
	}
	for pull, inbox := range pairs {
		if string(pull) != inbox {
			t.Fatalf("pull health %q != recovery %q", pull, inbox)
		}
	}
}
