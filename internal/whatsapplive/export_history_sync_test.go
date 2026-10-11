package whatsapplive

import (
	"context"

	"go.mau.fi/whatsmeow"
	waE2E "go.mau.fi/whatsmeow/proto/waE2E"
	waHistorySync "go.mau.fi/whatsmeow/proto/waHistorySync"
	watypes "go.mau.fi/whatsmeow/types"
)

// Hooks for the external whatsapplive_test package's history-sync tests.

// HistorySyncEffectsForTest replaces the history-sync pump's effects on
// WhatsApp; a nil field keeps the production effect.
type HistorySyncEffectsForTest struct {
	Download    func(context.Context, *whatsmeow.Client, *waE2E.HistorySyncNotification) (*waHistorySync.HistorySync, error)
	SendReceipt func(context.Context, *whatsmeow.Client, watypes.MessageID) error
	DeleteMedia func(context.Context, *whatsmeow.Client, *waE2E.HistorySyncNotification) error
}

// SetHistorySyncEffectsForTest installs effects between passes.
func SetHistorySyncEffectsForTest(b *Bridge, effects HistorySyncEffectsForTest) {
	pump := b.historySyncPump()
	pump.passMu.Lock()
	defer pump.passMu.Unlock()
	if effects.Download != nil {
		pump.download = effects.Download
	}
	if effects.SendReceipt != nil {
		pump.sendReceipt = effects.SendReceipt
	}
	if effects.DeleteMedia != nil {
		pump.deleteMedia = effects.DeleteMedia
	}
}

// DownloadHistorySyncChunkForTest is the production download.
func DownloadHistorySyncChunkForTest(ctx context.Context, cli *whatsmeow.Client, notification *waE2E.HistorySyncNotification) (*waHistorySync.HistorySync, error) {
	return downloadHistorySyncChunk(ctx, cli, notification)
}

// QueuedHistorySyncForTest reports how many notifications the queue holds.
func QueuedHistorySyncForTest(b *Bridge) int64 {
	return b.historySyncPump().queue.pending()
}

// SetHistorySyncQueueBoundForTest sets the queue's row bound. Call it from
// the goroutine that delivers notifications, while none is being delivered.
func SetHistorySyncQueueBoundForTest(b *Bridge, rows int64) {
	b.historySyncPump().queue.maxRows = rows
}

// ClientHistorySyncFlagsForTest reports the whatsmeow flags that hand
// history-sync downloads and receipts to the bridge.
func ClientHistorySyncFlagsForTest(b *Bridge) (manualDownload, noAutomaticReceipt bool) {
	cli := ClientForTeardownTest(b)
	return cli.ManualHistorySyncDownload, cli.DisableManualHistorySyncReceipt
}
