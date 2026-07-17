package signallive_test

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/ingest"
	"github.com/maxghenis/openmessage/internal/messaging"
	"github.com/maxghenis/openmessage/internal/signallive"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
	"github.com/maxghenis/openmessage/internal/v2keys"
)

func TestSignalACICaptureDecoderConvergesWithLegacy(t *testing.T) {
	const (
		account        = "+15551230000"
		aci            = "9f4b50e3-ebf2-413c-a856-161756a6161a"
		resolvedSource = "+15551234567"
		timestamp      = int64(1_700_000_000_123)
	)
	store, err := db.New(filepath.Join(t.TempDir(), "legacy.sqlite3"))
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Store.Close(): %v", err)
		}
	})

	line := []byte(`{"account":"+15551230000","envelope":{"sourceName":"Taylor","sourceServiceId":"9f4b50e3-ebf2-413c-a856-161756a6161a","timestamp":1700000000123,"dataMessage":{"timestamp":1700000000123,"message":"hi from service id"}}}`)
	captured, processed, err := signallive.CaptureAndProcessReceiveLineForTest(
		store,
		t.TempDir(),
		map[string]string{aci: resolvedSource},
		account,
		line,
	)
	if err != nil || !processed {
		t.Fatalf("capture + legacy receive = (%+v, %v, %v), want processed", captured, processed, err)
	}
	if captured.ResolvedSource != resolvedSource || captured.ResolvedDestination != "" ||
		!bytes.Equal(captured.Line, line) {
		t.Fatalf("captured ingress = %+v, want byte-identical line resolved to %q", captured, resolvedSource)
	}

	record, ephemeral, err := ingest.BuildSignalIngress(
		"signal-primary",
		1,
		captured.Account,
		captured.Line,
		captured.ResolvedSource,
		captured.ResolvedDestination,
		time.UnixMilli(timestamp),
	)
	if err != nil || record == nil || ephemeral != nil {
		t.Fatalf("BuildSignalIngress() = (%+v, %+v, %v), want durable", record, ephemeral, err)
	}
	events, err := ingest.NewSignalDecoder().Decode(context.Background(), *record)
	if err != nil {
		t.Fatalf("Decode(): %v", err)
	}
	if len(events) != 1 || events[0].Message == nil {
		t.Fatalf("Decode() = %+v, want one message", events)
	}

	legacyMessages, err := store.GetMessagesByConversation("signal:"+resolvedSource, 10)
	if err != nil {
		t.Fatalf("GetMessagesByConversation(): %v", err)
	}
	if len(legacyMessages) != 1 {
		t.Fatalf("legacy messages = %+v, want one", legacyMessages)
	}
	legacy := legacyMessages[0]
	decoded := events[0].Message
	if !bytes.Equal([]byte(decoded.RemoteConversationID), []byte(legacy.ConversationID)) {
		t.Fatalf("decoder conversation %q != legacy conversation %q", decoded.RemoteConversationID, legacy.ConversationID)
	}
	if !bytes.Equal([]byte(decoded.RemoteMessageID), []byte(legacy.SourceID)) {
		t.Fatalf("decoder message id %q != legacy SourceID %q", decoded.RemoteMessageID, legacy.SourceID)
	}
	wantSourceID := v2keys.SignalIncomingSourceID("signal:"+resolvedSource, resolvedSource, timestamp)
	if legacy.ConversationID != "signal:+15551234567" || legacy.SourceID != wantSourceID {
		t.Fatalf("legacy keys = (%q, %q), want resolved conversation and sha1 %q", legacy.ConversationID, legacy.SourceID, wantSourceID)
	}
}

func TestSignalCaptureReplayDedupeSurvivesContactResolutionDrift(t *testing.T) {
	const (
		accountID = "signal-primary"
		account   = "+15551230000"
		aci       = "9f4b50e3-ebf2-413c-a856-161756a6161a"
	)
	legacy, err := db.New(filepath.Join(t.TempDir(), "legacy.sqlite3"))
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	t.Cleanup(func() {
		if err := legacy.Close(); err != nil {
			t.Errorf("legacy.Close(): %v", err)
		}
	})
	v2Store, err := sqlite.Open(filepath.Join(t.TempDir(), "v2.sqlite3"))
	if err != nil {
		t.Fatalf("sqlite.Open(): %v", err)
	}
	t.Cleanup(func() {
		if err := v2Store.Close(); err != nil {
			t.Errorf("v2 Store.Close(): %v", err)
		}
	})
	now := time.UnixMilli(1_700_000_000_123)
	if err := v2Store.UpsertAccount(sqlite.Account{
		AccountID:   accountID,
		BridgeKey:   "signal_cli",
		DisplayName: "Signal",
		Mode:        sqlite.AccountModeLive,
		Enabled:     true,
		ConfigJSON:  `{}`,
		CreatedAtMS: now.UnixMilli(),
		UpdatedAtMS: now.UnixMilli(),
	}); err != nil {
		t.Fatalf("UpsertAccount(): %v", err)
	}
	messages, err := sqlite.NewMessageRepository(v2Store, func() time.Time { return now })
	if err != nil {
		t.Fatalf("NewMessageRepository(): %v", err)
	}
	counters := &ingest.Counters{}
	worker, err := ingest.NewWorker(ingest.WorkerConfig{
		Store:    v2Store,
		Messages: messages,
		Counters: counters,
	})
	if err != nil {
		t.Fatalf("NewWorker(): %v", err)
	}
	nextID := 0
	sink, err := ingest.NewSink(ingest.SinkConfig{
		Messages: messages,
		Worker:   worker,
		Counters: counters,
		IDs: messaging.IDSourceFunc(func() (string, error) {
			nextID++
			return fmt.Sprintf("signal-drift-%d", nextID), nil
		}),
	})
	if err != nil {
		t.Fatalf("NewSink(): %v", err)
	}

	line := []byte(`{"account":"+15551230000","envelope":{"sourceServiceId":"9f4b50e3-ebf2-413c-a856-161756a6161a","timestamp":1700000000123,"dataMessage":{"timestamp":1700000000123,"message":"same raw replay"}}}`)
	contacts := map[string]string{aci: "+15551234567"}
	for index, resolved := range []string{"+15551234567", "+15557654321"} {
		contacts[aci] = resolved
		captured, processed, err := signallive.CaptureAndProcessReceiveLineForTest(
			legacy,
			t.TempDir(),
			contacts,
			account,
			line,
		)
		if err != nil || !processed {
			t.Fatalf("capture[%d] = (%+v, %v, %v), want processed", index, captured, processed, err)
		}
		if captured.ResolvedSource != resolved || !bytes.Equal(captured.Line, line) {
			t.Fatalf("capture[%d] = %+v, want same raw line resolved to %q", index, captured, resolved)
		}
		record, ephemeral, err := ingest.BuildSignalIngress(
			accountID,
			1,
			captured.Account,
			captured.Line,
			captured.ResolvedSource,
			captured.ResolvedDestination,
			now,
		)
		if err != nil || record == nil || ephemeral != nil {
			t.Fatalf("BuildSignalIngress[%d] = (%+v, %+v, %v), want durable", index, record, ephemeral, err)
		}
		if err := sink.AppendIngress(context.Background(), *record); err != nil {
			t.Fatalf("AppendIngress[%d]: %v", index, err)
		}
	}

	inbox, err := messages.Unprocessed(context.Background())
	if err != nil {
		t.Fatalf("Unprocessed(): %v", err)
	}
	if len(inbox) != 1 {
		t.Fatalf("inbox rows = %d, want one raw-keyed replay row", len(inbox))
	}
	if snapshot := counters.Snapshot(accountID); snapshot.Appended != 1 || snapshot.Deduped != 1 {
		t.Fatalf("capture drift counters = %+v, want one append and one dedupe", snapshot)
	}
}
