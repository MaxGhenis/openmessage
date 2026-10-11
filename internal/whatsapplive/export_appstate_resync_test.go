package whatsapplive

import (
	"context"
	"testing"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/appstate"
	watypes "go.mau.fi/whatsmeow/types"
)

// StubAppStateFullSyncForTest replaces whatsmeow's full resync of an
// app-state collection (FetchAppState with fullSync, which fetches the
// server's snapshot over the socket) for the rest of the test.
func StubAppStateFullSyncForTest(t testing.TB, fullSync func(context.Context, *whatsmeow.Client, appstate.WAPatchName) error) {
	t.Helper()
	original := fetchAppStateFullSync
	fetchAppStateFullSync = fullSync
	t.Cleanup(func() { fetchAppStateFullSync = original })
}

// ContactDisplayNameForTest returns the name OpenMessage shows for jid, read
// from whatsmeow's contact store.
func ContactDisplayNameForTest(b *Bridge, jid watypes.JID) string {
	return b.contactDisplayName(jid, "")
}
