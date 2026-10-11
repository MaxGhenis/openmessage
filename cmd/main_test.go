package cmd

import (
	"os"
	"testing"
)

// TestMain keeps the WhatsApp Desktop history feeder off for every serve and
// built-binary test in this package unless a test enables it with t.Setenv.
// The feeder reads $HOME's real ChatStorage.sqlite by default, and a test run
// on a developer Mac must never capture the developer's own WhatsApp history.
func TestMain(m *testing.M) {
	if _, set := os.LookupEnv("OPENMESSAGES_WHATSAPP_DESKTOP_FEED"); !set {
		os.Setenv("OPENMESSAGES_WHATSAPP_DESKTOP_FEED", "0")
	}
	os.Exit(m.Run())
}
