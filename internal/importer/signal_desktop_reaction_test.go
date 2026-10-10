package importer

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/bridgeadapters/scripted"
	signaladapter "github.com/maxghenis/openmessage/internal/bridgeadapters/signal"
	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/messaging"
	"github.com/maxghenis/openmessage/internal/migration"
	"github.com/maxghenis/openmessage/internal/signallive"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
	"github.com/maxghenis/openmessage/internal/v2keys"
)

const (
	desktopReactionAccount      = "+15550001111"
	desktopReactionPeer         = "+15551234567"
	desktopReactionConversation = "signal:" + desktopReactionPeer
	desktopReactionSentAt       = int64(1_700_000_000_123)
	desktopReactionReceivedAt   = int64(1_700_000_000_500)
)

// stubSignalDesktopExport makes ImportFromDB read exported instead of running
// the Node helper against a Signal Desktop database, and returns the support
// directory to import from.
func stubSignalDesktopExport(t *testing.T, exported signalDesktopExport) string {
	t.Helper()
	supportDir := filepath.Join(t.TempDir(), "Signal")
	if err := os.MkdirAll(filepath.Join(supportDir, "sql"), 0o700); err != nil {
		t.Fatalf("mkdir sql dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(supportDir, "sql", "db.sqlite"), []byte("stub"), 0o600); err != nil {
		t.Fatalf("write stub db: %v", err)
	}
	raw, err := json.Marshal(exported)
	if err != nil {
		t.Fatalf("marshal export: %v", err)
	}
	originalRunHelper := runSignalDesktopHelper
	originalNodePath := signalDesktopNodePathFn
	originalAddonPath := signalDesktopAddonPathFn
	originalSQLKey := signalDesktopSQLKeyFn
	originalWriteHelper := writeSignalDesktopHelperFn
	t.Cleanup(func() {
		runSignalDesktopHelper = originalRunHelper
		signalDesktopNodePathFn = originalNodePath
		signalDesktopAddonPathFn = originalAddonPath
		signalDesktopSQLKeyFn = originalSQLKey
		writeSignalDesktopHelperFn = originalWriteHelper
	})
	runSignalDesktopHelper = func(context.Context, string, string, ...string) ([]byte, error) { return raw, nil }
	signalDesktopNodePathFn = func() (string, error) { return "/usr/bin/node", nil }
	signalDesktopAddonPathFn = func() (string, error) { return "/tmp/addon.node", nil }
	signalDesktopSQLKeyFn = func(string) (string, error) { return "deadbeef", nil }
	writeSignalDesktopHelperFn = func() (string, func(), error) { return "/tmp/helper.cjs", func() {}, nil }
	return supportDir
}

func desktopReactionExport() signalDesktopExport {
	return signalDesktopExport{
		Conversations: []signalDesktopConversationRow{{
			ID: "priv-peer", Type: "private", ProfileFullName: "Peer", E164: desktopReactionPeer, ActiveAt: desktopReactionReceivedAt,
		}},
		Messages: []signalDesktopMessageRow{
			{
				ID: "with-sent-time", ConversationID: "priv-peer", Type: "incoming", Body: "sent time known",
				SentAt: desktopReactionSentAt, ReceivedAt: desktopReactionSentAt + 900, Source: desktopReactionPeer,
			},
			{
				// The fallback row: Signal Desktop recorded no sent time, so
				// the importer stores the time it was received.
				ID: "no-sent-time", ConversationID: "priv-peer", Type: "incoming", Body: "sent time unknown",
				SentAt: 0, ReceivedAt: desktopReactionReceivedAt, Source: desktopReactionPeer,
			},
		},
	}
}

func importDesktopReactionFixture(t *testing.T, store *db.Store) *ImportResult {
	t.Helper()
	supportDir := stubSignalDesktopExport(t, desktopReactionExport())
	result, err := (&SignalDesktop{
		SupportDir: supportDir, MyName: "Me", MyAddress: desktopReactionAccount, SinceMS: -1,
	}).ImportFromDB(store)
	if err != nil || len(result.Errors) != 0 {
		t.Fatalf("ImportFromDB = %+v, %v", result, err)
	}
	return result
}

// reactionRecorder is a scripted Signal account that also accepts reactions,
// recording each request.
type reactionRecorder struct {
	*scripted.Adapter

	mu       sync.Mutex
	requests []bridge.ReactionRequest
}

func (a *reactionRecorder) SendReaction(_ context.Context, req bridge.ReactionRequest) (bridge.SendResult, error) {
	a.mu.Lock()
	a.requests = append(a.requests, req)
	a.mu.Unlock()
	return bridge.SendResult{}, nil
}

// TestSignalDesktopRowWithoutASentTimeCannotBeAReactionTarget follows two
// incoming Signal Desktop rows from the importer through the migration and the
// v2 reaction dispatcher into the Signal transport's argument builder. A row
// with a sent time is named by it. A row without one is stored under the time
// it was received, which is not the timestamp Signal knows the message by, so
// the transport must refuse it rather than send that time as the target.
func TestSignalDesktopRowWithoutASentTimeCannotBeAReactionTarget(t *testing.T) {
	root := t.TempDir()
	legacyPath := filepath.Join(root, "messages.db")
	legacy, err := db.New(legacyPath)
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	if result := importDesktopReactionFixture(t, legacy); result.MessagesImported != 2 {
		t.Fatalf("ImportFromDB = %+v, want both rows imported", result)
	}

	// What the importer wrote.
	sentHash := v2keys.SignalIncomingSourceID(desktopReactionConversation, desktopReactionPeer, desktopReactionSentAt)
	receivedHash := v2keys.SignalIncomingSourceID(desktopReactionConversation, desktopReactionPeer, desktopReactionReceivedAt)
	receivedSourceID := v2keys.SignalReceivedSourceID(desktopReactionConversation, desktopReactionPeer, desktopReactionReceivedAt)
	withSent, err := legacy.GetMessageByID("signal:" + sentHash)
	if err != nil || withSent == nil || withSent.SourceID != sentHash || withSent.TimestampMS != desktopReactionSentAt {
		t.Fatalf("row with a sent time = %+v, %v; want it keyed by its sent timestamp", withSent, err)
	}
	withoutSent, err := legacy.GetMessageByID("signal:" + receivedHash)
	if err != nil || withoutSent == nil || withoutSent.SourceID != receivedSourceID ||
		withoutSent.TimestampMS != desktopReactionReceivedAt || withoutSent.IsFromMe {
		t.Fatalf("row without a sent time = %+v, %v; want source ID %q at the received time", withoutSent, err, receivedSourceID)
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("close legacy store: %v", err)
	}

	staged := filepath.Join(root, "staged.sqlite3")
	if _, err := migration.Transform(context.Background(), migration.Options{
		SourcePath:      legacyPath,
		TempStorePath:   staged,
		TempBlobPath:    filepath.Join(root, "staged-blobs"),
		TargetPath:      filepath.Join(root, "target"),
		TargetStorePath: filepath.Join(root, "target", "store.sqlite3"),
		Check:           true,
	}); err != nil {
		t.Fatalf("migration.Transform: %v", err)
	}
	v2, err := sqlite.Open(staged)
	if err != nil {
		t.Fatalf("open migrated store: %v", err)
	}
	t.Cleanup(func() { _ = v2.Close() })
	messages, err := sqlite.NewMessageRepository(v2, time.Now)
	if err != nil {
		t.Fatalf("NewMessageRepository: %v", err)
	}
	const accountID = "signal-primary"
	conversation, err := v2.GetConversationByRemote(accountID, desktopReactionConversation)
	if err != nil {
		t.Fatalf("migrated conversation: %v", err)
	}

	signal := &reactionRecorder{Adapter: scripted.New(accountID, bridge.PlatformSignal)}
	registry := bridge.NewRegistry()
	if err := registry.Register(signal); err != nil {
		t.Fatalf("Register: %v", err)
	}
	service, err := messaging.NewMessageService(v2, registry, nil, messaging.SystemClock{}, messaging.CryptoIDSource{})
	if err != nil {
		t.Fatalf("NewMessageService: %v", err)
	}

	react := func(remoteID string) (bridge.MessageRef, []string, error) {
		t.Helper()
		target, err := messages.GetMessageByRemote(context.Background(), accountID, conversation.ConversationID, remoteID)
		if err != nil {
			t.Fatalf("migrated message %q: %v", remoteID, err)
		}
		if _, err := service.SendReaction(context.Background(), messaging.SendReactionCommand{
			CommonCommand: messaging.CommonCommand{
				AccountID: accountID, ConversationID: conversation.ConversationID, IdempotencyKey: "react-" + remoteID,
			},
			TargetMessageID: target.MessageID,
			Emoji:           "👍",
		}); err != nil {
			t.Fatalf("SendReaction(%q): %v", remoteID, err)
		}
		before := len(signal.requests)
		if processed, err := service.DispatchDue(context.Background(), 1); err != nil || processed != 1 {
			t.Fatalf("DispatchDue(%q) = %d, %v; want 1, nil", remoteID, processed, err)
		}
		if len(signal.requests) != before+1 {
			t.Fatalf("reaction to %q reached the transport %d times, want once", remoteID, len(signal.requests)-before)
		}
		ref := signal.requests[before].Target
		args, err := signallive.ReactionTargetArgs(signaladapter.ReactionTarget(ref), desktopReactionAccount)
		return ref, args, err
	}

	ref, args, err := react(sentHash)
	want := []string{"-a", desktopReactionPeer, "-t", "1700000000123"}
	if err != nil || !slices.Equal(args, want) {
		t.Fatalf("reaction to the row with a sent time = %q, %v (ref %+v); want %q", args, err, ref, want)
	}

	ref, args, err = react(receivedSourceID)
	if ref.AuthorID != desktopReactionPeer || ref.Outgoing || !ref.SentAt.Equal(time.UnixMilli(desktopReactionReceivedAt)) {
		t.Fatalf("ref for the row without a sent time = %+v, want the peer's incoming message at the received time", ref)
	}
	if err == nil || err.Error() != "signal reaction target timestamp is unavailable" || args != nil {
		t.Fatalf("reaction to the row without a sent time = %q, %v; want it refused, not sent with the received time", args, err)
	}
}

// A row an earlier import stored under the unmarked hash of its received time
// is rewritten in place by the next import, not duplicated, so a legacy store
// stops presenting it as a message keyed by a sent timestamp.
func TestSignalDesktopReimportMarksARowStoredBeforeTheMarker(t *testing.T) {
	store, err := db.New(filepath.Join(t.TempDir(), "messages.db"))
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	defer store.Close()
	receivedHash := v2keys.SignalIncomingSourceID(desktopReactionConversation, desktopReactionPeer, desktopReactionReceivedAt)
	if err := store.UpsertConversation(&db.Conversation{
		ConversationID: desktopReactionConversation, Name: "Peer", SourcePlatform: "signal",
		LastMessageTS: desktopReactionReceivedAt, Participants: "[]",
	}); err != nil {
		t.Fatalf("UpsertConversation: %v", err)
	}
	if err := store.UpsertMessage(&db.Message{
		MessageID: "signal:" + receivedHash, ConversationID: desktopReactionConversation,
		SenderName: "Peer", SenderNumber: desktopReactionPeer, Body: "sent time unknown",
		TimestampMS: desktopReactionReceivedAt, Status: "received",
		SourcePlatform: "signal", SourceID: receivedHash,
	}); err != nil {
		t.Fatalf("seed the row as the earlier importer wrote it: %v", err)
	}

	if result := importDesktopReactionFixture(t, store); result.MessagesImported != 1 || result.MessagesDuplicate != 1 {
		t.Fatalf("ImportFromDB = %+v, want one new row and one already stored", result)
	}

	rows, err := store.GetMessagesByConversation(desktopReactionConversation, 10)
	if err != nil || len(rows) != 2 {
		t.Fatalf("messages after re-import = %d, %v; want the two Desktop rows and no duplicate", len(rows), err)
	}
	row, err := store.GetMessageByID("signal:" + receivedHash)
	want := v2keys.SignalReceivedSourceID(desktopReactionConversation, desktopReactionPeer, desktopReactionReceivedAt)
	if err != nil || row == nil || row.SourceID != want {
		t.Fatalf("re-imported row = %+v, %v; want source ID %q", row, err, want)
	}
}
