package app

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

func TestNewDemoUsesIsolatedTempDataDir(t *testing.T) {
	realDataDir := filepath.Join(t.TempDir(), "real-data")
	if err := os.MkdirAll(realDataDir, 0o700); err != nil {
		t.Fatalf("MkdirAll(): %v", err)
	}

	t.Setenv("OPENMESSAGES_DATA_DIR", realDataDir)
	t.Setenv("OPENMESSAGES_DEMO", "1")

	a, err := New(zerolog.Nop())
	if err != nil {
		t.Fatalf("New(): %v", err)
	}

	if a.DataDir == realDataDir {
		t.Fatalf("DataDir = %q, want isolated temp dir", a.DataDir)
	}
	if a.tempDataDir == "" {
		t.Fatal("expected tempDataDir to be set in demo mode")
	}
	if filepath.Dir(a.SessionPath) != a.DataDir {
		t.Fatalf("SessionPath dir = %q, want %q", filepath.Dir(a.SessionPath), a.DataDir)
	}
	if filepath.Dir(a.WhatsAppSessionPath) != a.DataDir {
		t.Fatalf("WhatsAppSessionPath dir = %q, want %q", filepath.Dir(a.WhatsAppSessionPath), a.DataDir)
	}
	if _, err := os.Stat(filepath.Join(a.DataDir, "messages.db")); err != nil {
		t.Fatalf("expected demo db to exist: %v", err)
	}
	if count, err := a.Store.ConversationCount(""); err != nil {
		t.Fatalf("ConversationCount(): %v", err)
	} else if count == 0 {
		t.Fatal("expected seeded demo conversations")
	}
	if entries, err := os.ReadDir(realDataDir); err != nil {
		t.Fatalf("ReadDir(realDataDir): %v", err)
	} else if len(entries) != 0 {
		t.Fatalf("real data dir should stay untouched, found %d entries", len(entries))
	}

	demoDir := a.DataDir
	a.Close()

	if _, err := os.Stat(demoDir); !os.IsNotExist(err) {
		t.Fatalf("expected demo dir cleanup, stat err = %v", err)
	}
}

func TestDemoModeEnvParsing(t *testing.T) {
	t.Run("disabled when empty", func(t *testing.T) {
		t.Setenv("OPENMESSAGES_DEMO", "")
		if DemoMode() {
			t.Fatal("expected demo mode off")
		}
	})

	t.Run("enabled for truthy values", func(t *testing.T) {
		for _, value := range []string{"1", "true", "yes", "demo"} {
			t.Run(value, func(t *testing.T) {
				t.Setenv("OPENMESSAGES_DEMO", value)
				if !DemoMode() {
					t.Fatalf("expected demo mode on for %q", value)
				}
			})
		}
	})

	t.Run("disabled for explicit false values", func(t *testing.T) {
		for _, value := range []string{"0", "false", "off", "no"} {
			t.Run(value, func(t *testing.T) {
				t.Setenv("OPENMESSAGES_DEMO", value)
				if DemoMode() {
					t.Fatalf("expected demo mode off for %q", value)
				}
			})
		}
	})
}

func TestGoogleSendOutcomeRepairFlag(t *testing.T) {
	a := &App{}
	// A session file makes GooglePaired() true so NeedsRepair can surface.
	a.SessionPath = filepath.Join(t.TempDir(), "session.json")
	if err := os.WriteFile(a.SessionPath, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Below threshold: not yet flagged.
	a.RecordGoogleSendOutcome(false)
	a.RecordGoogleSendOutcome(false)
	if a.googleNeedsRepair.Load() {
		t.Fatal("should not flag before threshold")
	}
	// Reaching the threshold flags it.
	a.RecordGoogleSendOutcome(false)
	if !a.googleNeedsRepair.Load() {
		t.Fatalf("should flag after %d consecutive failures", googleRepairThreshold)
	}

	// Surfaces whenever paired — both the zombie (connected) and the
	// dead-credentials (disconnected) cases, since the fix is re-pair either way.
	a.Connected.Store(true)
	if !a.GoogleStatus().NeedsRepair {
		t.Fatal("connected + stuck should report needs_repair")
	}
	a.Connected.Store(false)
	if !a.GoogleStatus().NeedsRepair {
		t.Fatal("needs_repair must still surface while disconnected (auth-dead session)")
	}

	// Not surfaced when there's no session at all (the normal pairing flow).
	prev := a.SessionPath
	a.SessionPath = filepath.Join(t.TempDir(), "missing.json")
	if a.GoogleStatus().NeedsRepair {
		t.Fatal("needs_repair must not surface when unpaired")
	}
	a.SessionPath = prev

	// A single success clears it.
	a.RecordGoogleSendOutcome(true)
	if a.googleNeedsRepair.Load() || a.GoogleStatus().NeedsRepair {
		t.Fatal("a successful send must clear the repair flag")
	}

	// Counter reset by success: it takes a full threshold run to re-flag.
	a.RecordGoogleSendOutcome(false)
	if a.googleNeedsRepair.Load() {
		t.Fatal("one failure after a success must not immediately re-flag")
	}
}

func TestGooglePhoneRespondingStatus(t *testing.T) {
	a := &App{}
	a.SessionPath = filepath.Join(t.TempDir(), "session.json")
	if err := os.WriteFile(a.SessionPath, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	if !a.GoogleStatus().PhoneResponding {
		t.Fatal("unknown phone responding state should default to true")
	}
	a.RecordGooglePhoneResponding(false)
	if a.GoogleStatus().PhoneResponding {
		t.Fatal("PhoneNotResponding should surface in GoogleStatus")
	}
	a.RecordGooglePhoneResponding(true)
	if !a.GoogleStatus().PhoneResponding {
		t.Fatal("PhoneRespondingAgain should clear the offline state")
	}
}

func TestPhoneOfflineFailuresDoNotMarkGoogleForRepair(t *testing.T) {
	a := &App{}
	a.SessionPath = filepath.Join(t.TempDir(), "session.json")
	if err := os.WriteFile(a.SessionPath, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	for range googleRepairThreshold {
		a.RecordGoogleSendOutcomeWithPhone(false, false)
	}
	if a.googleSendFailures.Load() != 0 {
		t.Fatalf("googleSendFailures = %d, want 0", a.googleSendFailures.Load())
	}
	if a.GoogleStatus().NeedsRepair {
		t.Fatal("phone-offline failures should not mark Google as needing repair")
	}
}

func TestSuccessfulGoogleSendMarksPhoneResponding(t *testing.T) {
	a := &App{}
	a.SessionPath = filepath.Join(t.TempDir(), "session.json")
	if err := os.WriteFile(a.SessionPath, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	a.RecordGooglePhoneResponding(false)
	if a.GooglePhoneResponding() {
		t.Fatal("expected phone responding false after explicit offline event")
	}
	a.RecordGoogleSendOutcomeWithPhone(true, false)
	if !a.GooglePhoneResponding() {
		t.Fatal("successful send should mark phone responding true")
	}
}

func TestGoogleSendRejectedMessageUsesPhoneReachability(t *testing.T) {
	offline := GoogleSendRejectedMessage("UNKNOWN", false)
	if !strings.Contains(offline, "phone isn't responding") {
		t.Fatalf("offline error should mention phone reachability, got %q", offline)
	}
	stale := GoogleSendRejectedMessage("UNKNOWN", true)
	if !strings.Contains(stale, "Pair again") {
		t.Fatalf("responding-phone error should mention re-pairing, got %q", stale)
	}
}

func TestRecordGoogleSendErrorOnlyMarksAuthInvalidForRepair(t *testing.T) {
	a := &App{}
	a.SessionPath = filepath.Join(t.TempDir(), "session.json")
	if err := os.WriteFile(a.SessionPath, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	a.RecordGoogleSendError(errors.New("dial tcp: i/o timeout"))
	if a.GoogleStatus().NeedsRepair {
		t.Fatal("transient network send errors should not mark needs_repair")
	}

	a.RecordGoogleSendError(errors.New("send message: HTTP 401: invalid authentication credentials"))
	if !a.GoogleStatus().NeedsRepair {
		t.Fatal("auth-invalid send errors should mark needs_repair")
	}
}

func TestIsGoogleAuthInvalid(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"the real 401", errors.New("connect: failed to refresh auth token: HTTP 401: 16: Request had invalid authentication credentials."), true},
		{"unauthenticated", errors.New("rpc error: code = Unauthenticated desc = ..."), true},
		{"transient network", errors.New("dial tcp: i/o timeout"), false},
		{"unrelated 500", errors.New("HTTP 500 internal server error"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isGoogleAuthInvalid(c.err); got != c.want {
				t.Fatalf("isGoogleAuthInvalid(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

func TestHandleGoogleAuthExpiredErrorMarksDisconnected(t *testing.T) {
	a := &App{Logger: zerolog.Nop()}
	a.Connected.Store(true)
	a.googleNeedsRepair.Store(true)
	a.googleSendFailures.Store(googleRepairThreshold)
	statusEmitted := false
	a.OnStatusChange = func(connected bool) {
		statusEmitted = true
		if connected {
			t.Fatal("expected disconnected status event")
		}
	}

	err := errors.New("send message: HTTP 401: 16: Request had invalid authentication credentials")
	if !a.HandleGoogleAuthExpiredError(err) {
		t.Fatal("expected auth-expired error to be handled")
	}
	if a.Connected.Load() {
		t.Fatal("expected Google connection to be marked disconnected")
	}
	if !statusEmitted {
		t.Fatal("expected status change event")
	}
	if !a.googleNeedsRepair.Load() {
		t.Fatal("auth expiry must NOT clear an existing repair flag: clearing it defeats the reconnect-watchdog back-off and storms Google's auth endpoint on platforms with no cookie-refresh script (e.g. macOS)")
	}
	if got := a.GoogleStatus().LastError; got != googleAuthExpiredStatusMessage {
		t.Fatalf("last error = %q, want %q", got, googleAuthExpiredStatusMessage)
	}
	if !a.GoogleStatus().AuthExpired {
		t.Fatal("expected auth-expired status flag")
	}

	a.setGoogleLastError("Google Messages connection lost; reconnecting...")
	if !a.GoogleStatus().AuthExpired {
		t.Fatal("auth-expired flag should survive generic reconnect last-error text")
	}

	if a.HandleGoogleAuthExpiredError(errors.New("temporary failure in name resolution")) {
		t.Fatal("network errors should not be treated as auth expiry")
	}
}

func TestIsGoogleAuthExpiredErrorRecognizesFriendlyStatus(t *testing.T) {
	if !IsGoogleAuthExpiredError(errors.New(googleAuthExpiredStatusMessage)) {
		t.Fatal("expected friendly auth-expired status to be recognized")
	}
}
