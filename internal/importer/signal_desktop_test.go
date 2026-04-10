package importer

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/maxghenis/openmessage/internal/db"
)

func TestSignalDesktopImportFromDB(t *testing.T) {
	tempDir := t.TempDir()
	store, err := db.New(filepath.Join(tempDir, "messages.db"))
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	defer store.Close()

	supportDir := filepath.Join(tempDir, "Signal")
	if err := os.MkdirAll(filepath.Join(supportDir, "sql"), 0o700); err != nil {
		t.Fatalf("mkdir sql dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(supportDir, "sql", "db.sqlite"), []byte("stub"), 0o600); err != nil {
		t.Fatalf("write stub db: %v", err)
	}
	attachmentPath := filepath.Join(supportDir, "attachments.noindex", "bd", "file.png")
	if err := os.MkdirAll(filepath.Dir(attachmentPath), 0o700); err != nil {
		t.Fatalf("mkdir attachment dir: %v", err)
	}
	if err := os.WriteFile(attachmentPath, []byte("png"), 0o600); err != nil {
		t.Fatalf("write attachment: %v", err)
	}

	exported := signalDesktopExport{
		Conversations: []signalDesktopConversationRow{
			{
				ID:              "priv-ashley",
				Type:            "private",
				ProfileFullName: "Ashley N",
				E164:            "+15551234567",
				ServiceID:       "cdebb474-9b7a-4cb5-8bbc-b0ce740a513a",
				ActiveAt:        2_000,
			},
			{
				ID:              "priv-paul",
				Type:            "private",
				ProfileFullName: "Paul",
				ServiceID:       "000a58a9-e8cc-4204-bf5e-e52c3d5b234c",
				ActiveAt:        1_000,
			},
			{
				ID:       "group-row",
				Type:     "group",
				Name:     "Strategy Lab",
				GroupID:  "group-id-1",
				Members:  "cdebb474-9b7a-4cb5-8bbc-b0ce740a513a 000a58a9-e8cc-4204-bf5e-e52c3d5b234c",
				ActiveAt: 3_000,
			},
		},
		Messages: []signalDesktopMessageRow{
			{
				ID:              "msg-1",
				ConversationID:  "priv-ashley",
				Type:            "incoming",
				Body:            "hello there",
				SentAt:          1_000,
				SourceServiceID: "cdebb474-9b7a-4cb5-8bbc-b0ce740a513a",
				ReactionsJSON:   `[{"emoji":"❤️","fromId":"cdebb474-9b7a-4cb5-8bbc-b0ce740a513a"}]`,
			},
			{
				ID:             "msg-2",
				ConversationID: "priv-ashley",
				Type:           "outgoing",
				Body:           "got it",
				SentAt:         2_000,
				QuoteJSON:      `{"id":1000,"authorAci":"cdebb474-9b7a-4cb5-8bbc-b0ce740a513a","text":"hello there"}`,
			},
			{
				ID:              "msg-3",
				ConversationID:  "group-row",
				Type:            "incoming",
				SentAt:          3_000,
				SourceServiceID: "000a58a9-e8cc-4204-bf5e-e52c3d5b234c",
				ContentType:     "image/png",
				AttachmentPath:  "bd/file.png",
				ReactionsJSON:   `[{"emoji":"👍","fromId":"cdebb474-9b7a-4cb5-8bbc-b0ce740a513a"}]`,
			},
		},
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
	defer func() {
		runSignalDesktopHelper = originalRunHelper
		signalDesktopNodePathFn = originalNodePath
		signalDesktopAddonPathFn = originalAddonPath
		signalDesktopSQLKeyFn = originalSQLKey
		writeSignalDesktopHelperFn = originalWriteHelper
	}()

	runSignalDesktopHelper = func(_ context.Context, helperPath, nodePath string, args ...string) ([]byte, error) {
		if helperPath != "/tmp/helper.cjs" {
			t.Fatalf("helperPath = %q", helperPath)
		}
		if nodePath != "/usr/bin/node" {
			t.Fatalf("nodePath = %q", nodePath)
		}
		if len(args) < 4 {
			t.Fatalf("expected helper args, got %v", args)
		}
		return raw, nil
	}
	signalDesktopNodePathFn = func() (string, error) { return "/usr/bin/node", nil }
	signalDesktopAddonPathFn = func() (string, error) { return "/tmp/addon.node", nil }
	signalDesktopSQLKeyFn = func(string) (string, error) { return "deadbeef", nil }
	writeSignalDesktopHelperFn = func() (string, func(), error) {
		return "/tmp/helper.cjs", func() {}, nil
	}

	importer := &SignalDesktop{
		SupportDir: supportDir,
		MyName:     "Max",
		MyAddress:  "+15550001111",
		SinceMS:    -1,
	}
	result, err := importer.ImportFromDB(store)
	if err != nil {
		t.Fatalf("ImportFromDB: %v", err)
	}
	if result.ConversationsCreated != 2 {
		t.Fatalf("ConversationsCreated = %d, want 2", result.ConversationsCreated)
	}
	if result.MessagesImported != 3 {
		t.Fatalf("MessagesImported = %d, want 3", result.MessagesImported)
	}

	directConvo, err := store.GetConversation("signal:+15551234567")
	if err != nil {
		t.Fatalf("GetConversation direct: %v", err)
	}
	if directConvo.Name != "Ashley N" {
		t.Fatalf("direct convo name = %q, want Ashley N", directConvo.Name)
	}

	groupConvo, err := store.GetConversation("signal-group:group-id-1")
	if err != nil {
		t.Fatalf("GetConversation group: %v", err)
	}
	if groupConvo.Name != "Strategy Lab" {
		t.Fatalf("group convo name = %q, want Strategy Lab", groupConvo.Name)
	}
	if !strings.Contains(groupConvo.Participants, "Paul") {
		t.Fatalf("group participants missing resolved member name: %s", groupConvo.Participants)
	}

	incomingID, _ := signalDesktopMessageIDs("signal:+15551234567", "+15551234567", 1_000, "hello there", false)
	incomingMsg, err := store.GetMessageByID(incomingID)
	if err != nil {
		t.Fatalf("GetMessageByID incoming: %v", err)
	}
	if incomingMsg.SenderName != "Ashley N" {
		t.Fatalf("incoming sender = %q, want Ashley N", incomingMsg.SenderName)
	}
	if !strings.Contains(incomingMsg.Reactions, "❤️") {
		t.Fatalf("incoming reactions = %q, want heart reaction", incomingMsg.Reactions)
	}

	outgoingID, _ := signalDesktopMessageIDs("signal:+15551234567", "+15550001111", 2_000, "got it", true)
	outgoingMsg, err := store.GetMessageByID(outgoingID)
	if err != nil {
		t.Fatalf("GetMessageByID outgoing: %v", err)
	}
	if outgoingMsg.ReplyToID != incomingID {
		t.Fatalf("outgoing ReplyToID = %q, want %q", outgoingMsg.ReplyToID, incomingID)
	}

	groupID, _ := signalDesktopMessageIDs("signal-group:group-id-1", "000a58a9-e8cc-4204-bf5e-e52c3d5b234c", 3_000, "[Photo]", false)
	groupMsg, err := store.GetMessageByID(groupID)
	if err != nil {
		t.Fatalf("GetMessageByID group: %v", err)
	}
	if groupMsg.SenderName != "Paul" {
		t.Fatalf("group sender = %q, want Paul", groupMsg.SenderName)
	}
	if !strings.HasPrefix(groupMsg.MediaID, "signallocal:") {
		t.Fatalf("group media id = %q, want local signal attachment ref", groupMsg.MediaID)
	}
	if groupMsg.MimeType != "image/png" {
		t.Fatalf("group mime type = %q, want image/png", groupMsg.MimeType)
	}
}
