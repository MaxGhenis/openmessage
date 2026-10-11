package cmd

import (
	"os"
	"testing"
)

// TestMain keeps the WhatsApp and Signal Desktop history feeders off for
// every serve and built-binary test in this package unless a test enables one
// with t.Setenv. The feeders read $HOME's real Desktop databases by default
// (Signal's through the login keychain), and a test run on a developer Mac
// must never capture the developer's own history or raise a keychain prompt.
func TestMain(m *testing.M) {
	for _, name := range []string{"OPENMESSAGES_WHATSAPP_DESKTOP_FEED", "OPENMESSAGES_SIGNAL_DESKTOP_FEED"} {
		if _, set := os.LookupEnv(name); !set {
			os.Setenv(name, "0")
		}
	}
	os.Exit(m.Run())
}
