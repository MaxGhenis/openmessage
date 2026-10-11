package whatsapplive

import (
	"context"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/appstate"
)

// fetchAppStateFullSync resyncs one app-state collection from scratch:
// whatsmeow deletes the stored version (which cascades its value MACs) and
// fetches the server's snapshot.
var fetchAppStateFullSync = func(ctx context.Context, cli *whatsmeow.Client, name appstate.WAPatchName) error {
	return cli.FetchAppState(ctx, name, true, false)
}
