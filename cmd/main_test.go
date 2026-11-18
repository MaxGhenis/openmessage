package cmd

import (
	"os"
	"testing"
)

// TestMain turns the Signal Desktop history feeder off for every serve and
// built-binary test in this package, whatever the developer's shell exports.
// The feeder reads $HOME's real Signal Desktop database through the login
// keychain by default, and a test run on a developer Mac must never capture the
// developer's own history or raise a keychain prompt. Tests that exercise the
// feeder enable it explicitly with t.Setenv.
func TestMain(m *testing.M) {
	os.Setenv("OPENMESSAGES_SIGNAL_DESKTOP_FEED", "0")
	os.Exit(m.Run())
}
