package v2read

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"testing/quick"
	"time"

	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

// The 2026-10-24 symptom: Google sent Michaela's image with no media ID and
// named the thumbnail a second later. The UI had already asked for the image,
// been told no, and kept "Image unavailable" because nothing it rendered from
// changed. media_version is what changes.
func TestSourceMediaVersionChangesWhenTheMediaIsNamedAndNotWhenItDownloads(t *testing.T) {
	store, messages, source := openSourceTestStore(t)
	seedSourceAccount(t, store, "google-account", "gmessages")
	seedSourceConversation(t, store, sqlite.Conversation{
		ConversationID:       "conversation-google",
		AccountID:            "google-account",
		RemoteConversationID: "2957",
		Kind:                 sqlite.ConversationKindDirect,
		Title:                "Michaela",
		NotificationMode:     sqlite.NotificationModeAll,
	})
	message := sqlite.Message{
		MessageID:       "message-image",
		ConversationID:  "conversation-google",
		AccountID:       "google-account",
		RemoteMessageID: "89926",
		Direction:       sqlite.MessageDirectionIncoming,
		State:           sqlite.MessageStateActive,
		OccurredAtMS:    100,
	}
	version := func() string {
		t.Helper()
		rows, err := source.GetMessagesByConversation("conversation-google", 10)
		if err != nil {
			t.Fatalf("GetMessagesByConversation(): %v", err)
		}
		if len(rows) != 1 || rows[0].MediaID != "v2msg:message-image:0" {
			t.Fatalf("rows = %+v, want the one image message", rows)
		}
		return rows[0].MediaVersion
	}

	importSourceMessage(t, messages, message, sqlite.MessageAttachment{Ordinal: 0, MIME: "image/jpeg"})
	if got := version(); got != "" {
		t.Fatalf("media_version before the media is named = %q, want empty", got)
	}

	importSourceMessage(t, messages, message, sqlite.MessageAttachment{
		Ordinal: 0, RemoteID: "fd2cf23c-f14a-4013-9a88-b25e27ce59f9/7a6AAZenUM2d-IBYrk3daeduA", MIME: "image/jpeg",
	})
	named := version()
	if named == "" {
		t.Fatal("media_version after the media is named is empty")
	}

	attachments, err := sqlite.NewMessageAttachmentRepository(store, func() time.Time { return time.UnixMilli(sourceTestTimeMS) })
	if err != nil {
		t.Fatalf("NewMessageAttachmentRepository(): %v", err)
	}
	if err := attachments.MarkDownloaded(
		context.Background(), "message-image", 0, strings.Repeat("a", 64), 68691, "image/jpeg", false,
	); err != nil {
		t.Fatalf("MarkDownloaded(): %v", err)
	}
	if got := version(); got != named {
		t.Fatalf("media_version after the download = %q, want %q unchanged", got, named)
	}
}

// A thumbnail's version holds while it downloads and while the phone is
// asked for the original, and changes twice more: when the original is named
// (the reader may be served the thumbnail while the original downloads) and
// when the original replaces the thumbnail's bytes.
func TestSourceMediaVersionChangesWhenTheOriginalIsNamedAndWhenItLands(t *testing.T) {
	store, messages, source := openSourceTestStore(t)
	seedSourceAccount(t, store, "google-account", "gmessages")
	seedSourceConversation(t, store, sqlite.Conversation{
		ConversationID: "conversation-google", AccountID: "google-account", RemoteConversationID: "2957",
		Kind: sqlite.ConversationKindDirect, Title: "Michaela", NotificationMode: sqlite.NotificationModeAll,
	})
	message := sqlite.Message{
		MessageID: "message-image", ConversationID: "conversation-google", AccountID: "google-account",
		RemoteMessageID: "89926", Direction: sqlite.MessageDirectionIncoming, State: sqlite.MessageStateActive,
		OccurredAtMS: 100,
	}
	version := func() string {
		t.Helper()
		rows, err := source.GetMessagesByConversation("conversation-google", 10)
		if err != nil || len(rows) != 1 {
			t.Fatalf("GetMessagesByConversation() = %+v, %v", rows, err)
		}
		return rows[0].MediaVersion
	}
	attachments, err := sqlite.NewMessageAttachmentRepository(store, func() time.Time { return time.UnixMilli(sourceTestTimeMS) })
	if err != nil {
		t.Fatalf("NewMessageAttachmentRepository(): %v", err)
	}
	ctx := context.Background()
	thumbHash, fullHash := strings.Repeat("a", 64), strings.Repeat("b", 64)

	importSourceMessage(t, messages, message, sqlite.MessageAttachment{
		Ordinal: 0, RemoteID: "thumb-1", RemotePreview: true, MIME: "image/jpeg",
	})
	thumbnail := version()
	if err := attachments.MarkDownloaded(ctx, "message-image", 0, thumbHash, 10, "image/jpeg", true); err != nil {
		t.Fatalf("MarkDownloaded(): %v", err)
	}
	if got := version(); got != thumbnail {
		t.Fatalf("version after the thumbnail downloaded = %q, want %q unchanged", got, thumbnail)
	}
	importSourceMessage(t, messages, message, sqlite.MessageAttachment{Ordinal: 0, RemoteID: "full-1", MIME: "image/jpeg"})
	named := version()
	if named == "" || named == thumbnail {
		t.Fatalf("version after the original was named = %q, want a new one (thumbnail %q)", named, thumbnail)
	}
	if replaced, err := attachments.ReplaceDownloaded(ctx, "message-image", 0, thumbHash, fullHash, 1000, "image/jpeg", false); err != nil || !replaced {
		t.Fatalf("ReplaceDownloaded() = %v, %v", replaced, err)
	}
	landed := version()
	if landed == "" || landed == named || landed == thumbnail {
		t.Fatalf("version after the original landed = %q, want one unlike %q and %q", landed, named, thumbnail)
	}
}

// Properties of attachmentMediaVersion over arbitrary remote IDs: it depends
// on the trimmed remote ID and on whether the row holds a reduced copy whose
// original is named (NeedsOriginal) alone, is empty exactly when the ID is,
// is otherwise 12 lowercase hex digits, and tells different IDs, and a
// reduced copy from its original, apart.
func TestAttachmentMediaVersionProperties(t *testing.T) {
	hex12 := regexp.MustCompile(`^[0-9a-f]{12}$`)
	versionOf := func(remoteID string) string {
		return attachmentMediaVersion(sqlite.MessageAttachment{RemoteID: remoteID})
	}
	property := func(remoteID, other, mime string, size int64) bool {
		got := versionOf(remoteID)
		withOtherFields := attachmentMediaVersion(sqlite.MessageAttachment{
			RemoteID: remoteID, MIME: mime, SizeBytes: &size, State: "downloaded",
		})
		if got != withOtherFields || got != versionOf("  "+remoteID+"\t") {
			return false
		}
		if strings.TrimSpace(remoteID) == "" {
			return got == ""
		}
		if !hex12.MatchString(got) {
			return false
		}
		if strings.TrimSpace(other) != strings.TrimSpace(remoteID) && versionOf(other) == got {
			return false
		}
		hash := strings.Repeat("a", 64)
		reduced := attachmentMediaVersion(sqlite.MessageAttachment{
			RemoteID: remoteID, State: "downloaded", BlobHash: &hash, BlobPreview: true,
		})
		awaiting := attachmentMediaVersion(sqlite.MessageAttachment{
			RemoteID: remoteID, State: "downloaded", BlobHash: &hash, BlobPreview: true, RemotePreview: true,
		})
		if !hex12.MatchString(reduced) || reduced == got || reduced == versionOf(other) || awaiting != got {
			return false
		}
		return true
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 2000}); err != nil {
		t.Fatal(err)
	}
}
