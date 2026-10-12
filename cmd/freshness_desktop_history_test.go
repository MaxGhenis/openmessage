package cmd

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/maxghenis/openmessage/internal/ingest"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
	"github.com/maxghenis/openmessage/internal/whatsapplive"
)

// Desktop history must never reset live silence or inflate the live baseline,
// including when a scan happens during a disconnect and live delivery resumes.
func TestFreshnessActivitySourceIgnoresWhatsAppDesktopHistory(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "store.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	const account = "whatsapp-primary"
	if err := store.UpsertAccount(sqlite.Account{AccountID: account, BridgeKey: "whatsapp", Mode: sqlite.AccountModeLive, Enabled: true, ConfigJSON: "{}", CreatedAtMS: base.UnixMilli(), UpdatedAtMS: base.UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	clock := base
	repository, err := sqlite.NewMessageRepository(store, func() time.Time { return clock })
	if err != nil {
		t.Fatal(err)
	}
	appendAt := func(at time.Time, id, codec string) {
		t.Helper()
		clock = at
		if _, err := repository.AppendInbox(ctx, sqlite.InboxRecord{InboxID: id, AccountID: account, DedupeKey: id, Codec: codec, CodecVersion: 1, Payload: []byte(`{}`)}); err != nil {
			t.Fatal(err)
		}
	}
	appendAt(base, "live-first", whatsapplive.IngressCodec)
	appendAt(base.Add(time.Hour), "desktop-first", ingest.WhatsAppDesktopHistoryCodec)
	source := freshnessActivitySource(&v2Stack{Store: store}, nil, true)
	checkLatest := func(want time.Time) {
		t.Helper()
		latest, err := source.Latest(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !latest["whatsapp"].Equal(want) {
			t.Fatalf("latest WhatsApp = %v, want live receipt %v", latest, want)
		}
	}
	checkLatest(base) // full-scan path
	appendAt(base.Add(2*time.Hour), "desktop-second", ingest.WhatsAppDesktopHistoryCodec)
	checkLatest(base) // incremental path
	resumed := base.Add(3 * time.Hour)
	appendAt(resumed, "live-resumed", whatsapplive.IngressCodec)
	checkLatest(resumed)
	receipts, err := source.Between(ctx, "whatsapp", base, base.Add(4*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(receipts) != 2 || !receipts[0].Equal(base) || !receipts[1].Equal(resumed) {
		t.Fatalf("baseline receipts = %v, want only live delivery", receipts)
	}
}
