package importer

import (
	"path/filepath"
	"testing"

	"github.com/maxghenis/openmessage/internal/db"
)

// signalRowsAt returns the IDs of the stored rows of desktopReactionExport's
// first message: its conversation, sender and sent time.
func signalRowsAt(t *testing.T, store *db.Store) []string {
	t.Helper()
	rows, err := store.GetMessagesByConversationAtTimestamp(desktopReactionConversation, desktopReactionSentAt, 10)
	if err != nil {
		t.Fatalf("GetMessagesByConversationAtTimestamp(): %v", err)
	}
	var ids []string
	for _, row := range rows {
		if row.SenderNumber == desktopReactionPeer {
			ids = append(ids, row.MessageID)
		}
	}
	return ids
}

func reopenStore(t *testing.T, store *db.Store, path string) *db.Store {
	t.Helper()
	if err := store.Close(); err != nil {
		t.Fatalf("Close(): %v", err)
	}
	reopened, err := db.New(path)
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	return reopened
}

// The store already holds a Desktop message under another ID, with a reaction
// the Desktop row lacks, so the store-open cleanup would delete an imported
// copy. Every import skips the copy and counts it as already stored, instead
// of storing it, reporting it recovered, and losing it again at the next open.
func TestSignalDesktopImportSkipsACopyTheCleanupWouldDrop(t *testing.T) {
	path := filepath.Join(t.TempDir(), "messages.db")
	store, err := db.New(path)
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	defer func() { store.Close() }()
	if err := store.UpsertMessage(&db.Message{
		MessageID: "signal:stored-copy", ConversationID: desktopReactionConversation,
		SenderName: "Peer", SenderNumber: desktopReactionPeer, Body: "sent time known",
		TimestampMS: desktopReactionSentAt, Status: "received", Reactions: `[{"emoji":"👍"}]`,
		SourcePlatform: "signal",
	}); err != nil {
		t.Fatalf("seed the stored copy: %v", err)
	}

	for run := 0; run < 2; run++ {
		result := importDesktopReactionFixture(t, store)
		wantImported := 0
		if run == 0 {
			wantImported = 1 // the row without a sent time
		}
		if result.MessagesImported != wantImported || result.MessagesDuplicate != 2-wantImported {
			t.Fatalf("import %d = %+v, want %d imported and the stored copy counted as a duplicate", run+1, result, wantImported)
		}
		store = reopenStore(t, store, path)
		if got := signalRowsAt(t, store); len(got) != 1 || got[0] != "signal:stored-copy" {
			t.Fatalf("after import %d and a reopen, rows for the message = %v, want only the stored copy", run+1, got)
		}
	}
}

// When the Desktop row outranks the stored copy, the import stores it, the
// next open's cleanup deletes the weaker copy, and later imports find the
// Desktop row in place.
func TestSignalDesktopImportReplacesAWeakerCopyOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "messages.db")
	store, err := db.New(path)
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	defer func() { store.Close() }()
	if err := store.UpsertMessage(&db.Message{
		MessageID: "signal:stored-copy", ConversationID: desktopReactionConversation,
		SenderName: "Peer", SenderNumber: desktopReactionPeer, Body: "sent",
		TimestampMS: desktopReactionSentAt, Status: "received", SourcePlatform: "signal",
	}); err != nil {
		t.Fatalf("seed the stored copy: %v", err)
	}

	if result := importDesktopReactionFixture(t, store); result.MessagesImported != 2 || result.MessagesDuplicate != 0 {
		t.Fatalf("first import = %+v, want both Desktop rows imported", result)
	}
	store = reopenStore(t, store, path)
	desktopRows := signalRowsAt(t, store)
	if len(desktopRows) != 1 || desktopRows[0] == "signal:stored-copy" {
		t.Fatalf("rows for the message after a reopen = %v, want only the Desktop row", desktopRows)
	}
	if result := importDesktopReactionFixture(t, store); result.MessagesImported != 0 || result.MessagesDuplicate != 2 {
		t.Fatalf("second import = %+v, want both Desktop rows already stored", result)
	}
	if got := signalRowsAt(t, store); len(got) != 1 || got[0] != desktopRows[0] {
		t.Fatalf("rows for the message after the second import = %v, want %v", got, desktopRows)
	}
}
