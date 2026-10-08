package cmd

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"go.mau.fi/mautrix-gmessages/pkg/libgm"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/ingest"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

// Catch-up history must not read as live delivery. A reconcile runs exactly
// when the long-poll reconnects, so if fetched history counted, every
// reconnect during a phone-side stall would reset the silence clock, and its
// bursts would inflate the hour-of-day baseline. History frames use their own
// codec; the v2-primary silence source (the production wiring, real SQL) must
// see only the live codec's receipts, on both its full-scan and incremental
// paths and in Between.
func TestFreshnessActivitySourceIgnoresGoogleHistoryFrames(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "store.sqlite3"))
	if err != nil {
		t.Fatalf("sqlite.Open(): %v", err)
	}
	defer store.Close()
	const accountID = "google-primary"
	base := time.Date(2026, 10, 6, 1, 11, 7, 0, time.UTC)
	if err := store.UpsertAccount(sqlite.Account{
		AccountID:   accountID,
		BridgeKey:   "google_messages",
		DisplayName: "Google Messages",
		Mode:        sqlite.AccountModeLive,
		Enabled:     true,
		ConfigJSON:  "{}",
		CreatedAtMS: base.UnixMilli(),
		UpdatedAtMS: base.UnixMilli(),
	}); err != nil {
		t.Fatalf("UpsertAccount(): %v", err)
	}

	clock := base
	repository, err := sqlite.NewMessageRepository(store, func() time.Time { return clock })
	if err != nil {
		t.Fatal(err)
	}
	sequence := 0
	appendAt := func(at time.Time, record bridge.RawIngressRecord) {
		t.Helper()
		clock = at
		sequence++
		if _, err := repository.AppendInbox(context.Background(), sqlite.InboxRecord{
			InboxID:      "inbox-" + time.Duration(sequence).String(),
			AccountID:    record.AccountID,
			Generation:   int64(record.Generation),
			DedupeKey:    record.DedupeKey,
			Codec:        record.Codec,
			CodecVersion: int64(record.CodecVersion),
			Payload:      record.Payload,
		}); err != nil {
			t.Fatalf("AppendInbox(%s): %v", record.Codec, err)
		}
	}
	message := func(id string) *gmproto.Message {
		return &gmproto.Message{
			MessageID:      id,
			ConversationID: "thread",
			Timestamp:      base.UnixMicro(),
			MessageStatus:  &gmproto.MessageStatus{Status: gmproto.MessageStatusType_INCOMING_COMPLETE},
			MessageInfo: []*gmproto.MessageInfo{{
				Data: &gmproto.MessageInfo_MessageContent{MessageContent: &gmproto.MessageContent{Content: id}},
			}},
		}
	}
	conversation := &gmproto.Conversation{ConversationID: "thread", Name: "Thread"}
	live := func(at time.Time, id string) {
		t.Helper()
		record, err := ingest.GoogleMessageRecord(accountID, 1, &libgm.WrappedMessage{Message: message(id)}, at)
		if err != nil {
			t.Fatal(err)
		}
		appendAt(at, record)
	}
	history := func(at time.Time, id string) {
		t.Helper()
		record, err := ingest.GoogleHistoryMessageRecord(accountID, 1, "thread", conversation, message(id), at)
		if err != nil {
			t.Fatal(err)
		}
		appendAt(at, record)
		snapshot, err := ingest.GoogleHistoryConversationRecord(accountID, 1, conversation, at)
		if err != nil {
			t.Fatal(err)
		}
		snapshot.DedupeKey += ":" + id // a fresh snapshot row per catch-up
		appendAt(at, snapshot)
	}

	source := freshnessActivitySource(&v2Stack{Store: store}, nil, true)
	if source == nil {
		t.Fatal("freshnessActivitySource(v2-primary) = nil")
	}
	latestGoogle := func() time.Time {
		t.Helper()
		latest, err := source.Latest(context.Background())
		if err != nil {
			t.Fatalf("Latest(): %v", err)
		}
		return latest["google"]
	}

	lastLive := base.Add(time.Minute)
	live(lastLive, "live-1")
	history(base.Add(2*time.Hour), "fetched-1") // a reconcile two hours into a stall
	if got := latestGoogle(); !got.Equal(lastLive) {
		t.Fatalf("Latest (full scan) = %v, want the last live receipt %v", got, lastLive)
	}
	// The incremental path reads only rows past its high-water mark.
	history(base.Add(5*time.Hour), "fetched-2")
	history(base.Add(9*time.Hour), "fetched-3")
	if got := latestGoogle(); !got.Equal(lastLive) {
		t.Fatalf("Latest (incremental) = %v, want the last live receipt %v", got, lastLive)
	}

	receipts, err := source.Between(context.Background(), "google", base, base.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("Between(): %v", err)
	}
	if len(receipts) != 1 || !receipts[0].Equal(lastLive) {
		t.Fatalf("Between() = %v, want only the live receipt %v", receipts, lastLive)
	}

	// Live delivery still moves the clock.
	resumed := base.Add(10 * time.Hour)
	live(resumed, "live-2")
	if got := latestGoogle(); !got.Equal(resumed) {
		t.Fatalf("Latest after live delivery resumed = %v, want %v", got, resumed)
	}
}
