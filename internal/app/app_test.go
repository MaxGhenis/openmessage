package app

import (
	"errors"
	"fmt"
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

func TestIsSessionInvalidated(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil error", nil, false},
		{"401 status code", errors.New("http 401 while polling"), true},
		{"SESSION_COOKIE_INVALID code", errors.New("RPC failed: SESSION_COOKIE_INVALID"), true},
		{"401 in wrapped libgm error",
			fmt.Errorf("listen: %w", errors.New("server returned status 401")), true},
		{"transient network failure",
			errors.New("dial tcp: i/o timeout"), false},
		{"unrelated 500 error",
			errors.New("http 500 internal server error"), false},
		{"transient RPC error",
			errors.New("context deadline exceeded"), false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isSessionInvalidated(tc.err); got != tc.want {
				t.Fatalf("isSessionInvalidated(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
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
