package cmd

import (
	"os"
	"testing"
)

// TestMain turns the WhatsApp Desktop history feeder off for every serve and
// built-binary test in this package, whatever the developer's shell exports.
// The feeder reads $HOME's real ChatStorage.sqlite by default, and a test run
// on a developer Mac must never capture the developer's own WhatsApp history.
// Tests that exercise the feeder enable it explicitly with t.Setenv.
func TestMain(m *testing.M) {
	os.Setenv("OPENMESSAGES_WHATSAPP_DESKTOP_FEED", "0")
	os.Exit(m.Run())
}
