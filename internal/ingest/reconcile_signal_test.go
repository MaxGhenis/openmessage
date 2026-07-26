package ingest

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/rs/zerolog"

	legacydb "github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/reconcile"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
	"github.com/maxghenis/openmessage/internal/v2keys"
)

func TestSignalReconciledOutgoingAliasIsFoundByWorkerBareTimestampPath(t *testing.T) {
	const (
		remoteConversationID = "signal:+16505550100"
		timestamp            = int64(1_700_000_006_000)
	)
	ctx := context.Background()
	harness := newSignalWorkerHarness(t, "reconcile-alias.sqlite3")
	legacy, err := legacydb.New(filepath.Join(t.TempDir(), "messages.db"))
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	t.Cleanup(func() {
		if err := legacy.Close(); err != nil {
			t.Errorf("legacy.Close(): %v", err)
		}
	})
	if err := legacy.UpsertConversation(&legacydb.Conversation{
		ConversationID: remoteConversationID,
		Name:           "Signal Peer",
		LastMessageTS:  timestamp,
		SourcePlatform: "signal",
	}); err != nil {
		t.Fatalf("UpsertConversation(): %v", err)
	}
	alias := v2keys.SignalLocalAlias(remoteConversationID, timestamp)
	if err := legacy.UpsertMessage(&legacydb.Message{
		MessageID:      "signal:legacy-outgoing-alias",
		ConversationID: remoteConversationID,
		SenderName:     "Me",
		Body:           "reconciled outgoing body",
		TimestampMS:    timestamp,
		IsFromMe:       true,
		SourcePlatform: "signal",
		SourceID:       alias,
	}); err != nil {
		t.Fatalf("UpsertMessage(): %v", err)
	}

	report, err := reconcile.Signal(ctx, reconcile.Options{
		Legacy: legacy,
		V2:     harness.store,
		Logger: zerolog.Nop(),
	})
	if err != nil {
		t.Fatalf("reconcile.Signal(): %v", err)
	}
	if report.MessagesImported != 1 || report.MessagesAlreadyPresent != 0 {
		t.Fatalf("reconcile report = %+v", report)
	}
	conversation, err := harness.store.GetConversationByRemote(
		signalDecoderAccountID,
		remoteConversationID,
	)
	if err != nil {
		t.Fatalf("GetConversationByRemote(): %v", err)
	}
	reconciled, err := harness.messages.GetMessageByRemote(
		ctx,
		signalDecoderAccountID,
		conversation.ConversationID,
		alias,
	)
	if err != nil {
		t.Fatalf("GetMessageByRemote(alias): %v", err)
	}
	wantMessageID := v2keys.DeriveID(
		"message",
		signalDecoderAccountID,
		remoteConversationID+"\x1f"+alias,
	)
	if reconciled.MessageID != wantMessageID {
		t.Fatalf(
			"reconciled message ID = %q, want %q",
			reconciled.MessageID,
			wantMessageID,
		)
	}

	// Historical import uses the wall clock. Give the real replay a later clock
	// so its update satisfies the store's monotonic timestamp constraints.
	liveMessages, err := sqlite.NewMessageRepository(
		harness.store,
		func() time.Time { return time.Now().Add(time.Hour) },
	)
	if err != nil {
		t.Fatalf("NewMessageRepository(live replay): %v", err)
	}
	liveHarness := newSignalWorkerHarnessForStore(
		t,
		harness.path,
		harness.store,
		liveMessages,
	)
	liveHarness.start(t)
	line := []byte(`{"account":"+15551230000","envelope":{"sourceNumber":"+15551230000","timestamp":1700000006000,"syncMessage":{"sentMessage":{"timestamp":1700000006000,"destinationServiceId":"7a81fd95-20f1-4437-86e2-d5c93ba18851","message":"live outgoing replay"}}}}`)
	record := mustBuildSignalRecordWithResolutions(t, line, "", "+16505550100")
	if err := liveHarness.sink.AppendIngress(ctx, record); err != nil {
		t.Fatalf("AppendIngress(live replay): %v", err)
	}
	waitSignalCondition(t, "reconciled Signal outgoing live convergence", func() bool {
		message, err := liveMessages.GetMessageByRemote(
			ctx,
			signalDecoderAccountID,
			conversation.ConversationID,
			alias,
		)
		return err == nil && message.Body == "live outgoing replay" &&
			liveHarness.counters.Snapshot(signalDecoderAccountID).Projected == 1
	})

	converged, err := liveMessages.GetMessageByRemote(
		ctx,
		signalDecoderAccountID,
		conversation.ConversationID,
		alias,
	)
	if err != nil {
		t.Fatalf("GetMessageByRemote(converged alias): %v", err)
	}
	if converged.MessageID != wantMessageID || converged.RemoteMessageID != alias {
		t.Fatalf(
			"converged message = %+v, want preserved PK %q and alias %q",
			converged,
			wantMessageID,
			alias,
		)
	}
	bareTimestamp := strconv.FormatInt(timestamp, 10)
	if _, err := liveMessages.GetMessageByRemote(
		ctx,
		signalDecoderAccountID,
		conversation.ConversationID,
		bareTimestamp,
	); !errors.Is(err, sqlite.ErrNotFound) {
		t.Fatalf("bare-timestamp lookup error = %v, want ErrNotFound", err)
	}
	if got := countSignalRows(t, harness.path, "inbox"); got != 1 {
		t.Fatalf("inbox rows = %d, want one durable live replay", got)
	}
	if got := countSignalRows(t, harness.path, "messages"); got != 1 {
		t.Fatalf("message rows = %d, want one converged reconciled row", got)
	}
}
