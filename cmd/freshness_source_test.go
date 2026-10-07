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

// With v2 ingest running, silence is measured on the inbox, and every codec
// the ingest worker decodes maps to its status platform; without it, on stored
// message timestamps.
func TestFreshnessActivitySourceMapsEveryIngestCodec(t *testing.T) {
	if got := freshnessActivitySource(nil, nil); got != nil {
		t.Fatalf("freshnessActivitySource(nil, nil) = %v, want nil", got)
	}
	legacy, err := db.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer legacy.Close()
	if got := freshnessActivitySource(nil, legacy); got == nil || got.Name() != freshness.SourceMessages {
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

	source := freshnessActivitySource(&v2Stack{Store: store}, legacy)
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
