package cmd

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/freshness"
	"github.com/maxghenis/openmessage/internal/ingest"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
	"github.com/maxghenis/openmessage/internal/whatsapplive"
)

// On a v2-primary daemon silence is measured on the v2 inbox, and each ingest
// codec maps to its status platform. When readers use the legacy store, even
// with v2 ingest running, it is measured on the legacy store's incoming
// messages, since rows reach it that never pass through the inbox.
func TestFreshnessActivitySourceMapsEveryIngestCodec(t *testing.T) {
	if got := freshnessActivitySource(nil, nil, true); got != nil {
		t.Fatalf("freshnessActivitySource(nil, nil, true) = %v, want nil", got)
	}
	legacy, err := db.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer legacy.Close()
	if got := freshnessActivitySource(nil, legacy, false); got == nil || got.Name() != freshness.SourceMessages {
		t.Fatalf("legacy-only source = %v, want %s", got, freshness.SourceMessages)
	}

	store, err := sqlite.Open(filepath.Join(t.TempDir(), "store.sqlite3"))
	if err != nil {
		t.Fatalf("sqlite.Open(): %v", err)
	}
	defer store.Close()
	codecs := map[string]string{
		ingest.GoogleCodec:        "google",
		whatsapplive.IngressCodec: "whatsapp",
		ingest.SignalJSONRPCCodec: "signal",
	}
	base := time.Date(2026, 10, 6, 1, 11, 7, 0, time.UTC)
	offset := time.Duration(0)
	for codec, platform := range codecs {
		offset += time.Minute
		at := base.Add(offset)
		accountID := platform + "-primary"
		if err := store.UpsertAccount(sqlite.Account{
			AccountID:   accountID,
			BridgeKey:   platform + "_bridge",
			DisplayName: platform,
			Mode:        sqlite.AccountModeLive,
			Enabled:     true,
			ConfigJSON:  "{}",
			CreatedAtMS: at.UnixMilli(),
			UpdatedAtMS: at.UnixMilli(),
		}); err != nil {
			t.Fatalf("UpsertAccount(%s): %v", accountID, err)
		}
		repository, err := sqlite.NewMessageRepository(store, func() time.Time { return at })
		if err != nil {
			t.Fatal(err)
		}
		if _, err := repository.AppendInbox(context.Background(), sqlite.InboxRecord{
			InboxID:      "inbox-" + platform,
			AccountID:    accountID,
			Generation:   1,
			DedupeKey:    "frame-" + platform,
			Codec:        codec,
			CodecVersion: 1,
			Payload:      []byte("{}"),
		}); err != nil {
			t.Fatalf("AppendInbox(%s): %v", codec, err)
		}
	}

	if got := freshnessActivitySource(&v2Stack{Store: store}, legacy, false); got == nil || got.Name() != freshness.SourceMessages {
		t.Fatalf("v2 ingest without v2-primary = %v, want %s", got, freshness.SourceMessages)
	}
	source := freshnessActivitySource(&v2Stack{Store: store}, legacy, true)
	if source == nil || source.Name() != freshness.SourceV2Inbox {
		t.Fatalf("v2 source = %v, want %s", source, freshness.SourceV2Inbox)
	}
	latest, err := source.Latest(context.Background())
	if err != nil {
		t.Fatalf("Latest(): %v", err)
	}
	for _, platform := range codecs {
		if latest[platform].IsZero() {
			t.Fatalf("Latest() = %v, missing %s", latest, platform)
		}
	}
	if len(latest) != len(codecs) {
		t.Fatalf("Latest() = %v, want exactly %d platforms", latest, len(codecs))
	}
}
