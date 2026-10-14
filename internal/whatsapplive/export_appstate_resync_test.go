package whatsapplive

import (
	"context"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/appstate"
	watypes "go.mau.fi/whatsmeow/types"
)

// productionAppStateFullSync is fetchAppStateFullSync as the package defines
// it, captured before any test stubs it.
var productionAppStateFullSync = fetchAppStateFullSync

// StubAppStateFullSyncForTest replaces whatsmeow's full resync of an
// app-state collection (FetchAppState with fullSync, which fetches the
// server's snapshot over the socket) for the rest of the test.
func StubAppStateFullSyncForTest(t testing.TB, fullSync func(context.Context, *whatsmeow.Client, appstate.WAPatchName) error) {
	t.Helper()
	original := fetchAppStateFullSync
	fetchAppStateFullSync = fullSync
	t.Cleanup(func() { fetchAppStateFullSync = original })
}

// ProductionAppStateFullSyncForTest runs the full resync production uses.
func ProductionAppStateFullSyncForTest(ctx context.Context, cli *whatsmeow.Client, name appstate.WAPatchName) error {
	return productionAppStateFullSync(ctx, cli, name)
}

// SetAppStateResyncLimitsForTest replaces the resync budget for the rest of
// the test.
func SetAppStateResyncLimitsForTest(t testing.TB, cooldown time.Duration, maxStarts int, window time.Duration) {
	t.Helper()
	original := appStateResyncLimits
	appStateResyncLimits = appStateResyncPolicy{cooldown: cooldown, maxStarts: maxStarts, window: window}
	t.Cleanup(func() { appStateResyncLimits = original })
}

// SetAppStateResyncClockForTest replaces the clock the resync budget reads
// for the rest of the test.
func SetAppStateResyncClockForTest(t testing.TB, now func() time.Time) {
	t.Helper()
	original := appStateResyncNow
	appStateResyncNow = now
	t.Cleanup(func() { appStateResyncNow = original })
}

// AppStateResyncsInFlightForTest counts the collections whose full resync is
// running.
func AppStateResyncsInFlightForTest(b *Bridge) int {
	b.mu.RLock()
	resync := b.appStateResync
	b.mu.RUnlock()
	return resync.inFlight()
}

// ContactDisplayNameForTest returns the name OpenMessage shows for jid, read
// from whatsmeow's contact store.
func ContactDisplayNameForTest(b *Bridge, jid watypes.JID) string {
	return b.contactDisplayName(jid, "")
}
