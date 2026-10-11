package cmd

import (
	"os"
	"testing"
)

// TestMain turns the WhatsApp and Signal Desktop history feeders off for every
// serve and built-binary test in this package, whatever the developer's shell
// exports. The feeders read $HOME's real Desktop databases by default
// (Signal's through the login keychain), and a test run on a developer Mac must
// never capture the developer's own history or raise a keychain prompt. Tests
// that exercise a feeder enable it explicitly with t.Setenv.
func TestMain(m *testing.M) {
	os.Setenv("OPENMESSAGES_WHATSAPP_DESKTOP_FEED", "0")
	os.Setenv("OPENMESSAGES_SIGNAL_DESKTOP_FEED", "0")
	os.Exit(m.Run())
}
