package ingest

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"testing"

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

	bareTimestamp := strconv.FormatInt(timestamp, 10)
	resolved, err := harness.worker.signalOutgoingRemoteID(
		ctx,
		signalDecoderAccountID,
		conversation.ConversationID,
		remoteConversationID,
		bareTimestamp,
	)
	if err != nil {
		t.Fatalf("signalOutgoingRemoteID(): %v", err)
	}
	if resolved != alias {
		t.Fatalf("signalOutgoingRemoteID() = %q, want reconciled alias %q", resolved, alias)
	}
	if _, err := harness.messages.GetMessageByRemote(
		ctx,
		signalDecoderAccountID,
		conversation.ConversationID,
		bareTimestamp,
	); !errors.Is(err, sqlite.ErrNotFound) {
		t.Fatalf("bare-timestamp lookup error = %v, want ErrNotFound", err)
	}
	if got := countSignalRows(t, harness.path, "messages"); got != 1 {
		t.Fatalf("message rows = %d, want one converged reconciled row", got)
	}
	if reconciled.RemoteMessageID != alias {
		t.Fatalf("reconciled remote message ID = %q, want %q", reconciled.RemoteMessageID, alias)
	}
}
