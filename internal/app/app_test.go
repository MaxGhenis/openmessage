package app

import (
	"os"
	"path/filepath"
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

	// Surfaced in status only while connected.
	a.Connected.Store(true)
	if !a.GoogleStatus().NeedsRepair {
		t.Fatal("connected + stuck should report needs_repair")
	}
	a.Connected.Store(false)
	if a.GoogleStatus().NeedsRepair {
		t.Fatal("needs_repair must not surface while disconnected (normal pairing flow handles that)")
	}

	// A single success clears it.
	a.Connected.Store(true)
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
