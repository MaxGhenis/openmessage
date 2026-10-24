package sqlite

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"testing/quick"
)

// Google named only the thumbnail of most stored images, and an update can
// name the thumbnail again after the original. The original's reference,
// once stored, is the one kept.
func TestRecordInboundAttachmentKeepsTheOriginalOverAThumbnail(t *testing.T) {
	clock := newMessageTestClock(messageAttachmentTestTimeMS)
	store, repository := openMessageAttachmentTestRepository(t, clock.Now)
	seedMessageAttachmentParent(t, store, "message-image")
	thumbnail := MessageAttachment{
		MessageID: "message-image", Ordinal: 0, RemoteID: "thumb-1", RemoteRef: []byte(`{"media_id":"thumb-1"}`),
		RemotePreview: true, Filename: "thumb.jpeg", MIME: "image/jpeg",
	}
	original := MessageAttachment{
		MessageID: "message-image", Ordinal: 0, RemoteID: "full-1", RemoteRef: []byte(`{"media_id":"full-1"}`),
		Filename: "IMG.jpeg", MIME: "image/heic",
	}
	recordMessageAttachment(t, store, repository, thumbnail)
	recordMessageAttachment(t, store, repository, original)
	recordMessageAttachment(t, store, repository, thumbnail)
	got, err := repository.GetForDownload(context.Background(), "message-image", 0)
	if err != nil {
		t.Fatalf("GetForDownload(): %v", err)
	}
	if got.RemoteID != "full-1" || got.RemotePreview || got.Filename != "IMG.jpeg" || got.MIME != "image/heic" {
		t.Fatalf("after thumbnail, original, thumbnail: %+v, want the original and its description", got)
	}
}

// A downloaded thumbnail takes the reference of its original, and keeps its
// bytes and their description until the original replaces them. A
// downloaded original takes nothing.
func TestRecordInboundAttachmentUpgradesOnlyADownloadedThumbnail(t *testing.T) {
	clock := newMessageTestClock(messageAttachmentTestTimeMS)
	store, repository := openMessageAttachmentTestRepository(t, clock.Now)
	ctx := context.Background()
	for _, messageID := range []string{"message-thumb", "message-full"} {
		seedMessageAttachmentParent(t, store, messageID)
	}
	recordMessageAttachment(t, store, repository, MessageAttachment{
		MessageID: "message-thumb", Ordinal: 0, RemoteID: "thumb-1", RemotePreview: true, MIME: "image/jpeg",
	})
	recordMessageAttachment(t, store, repository, MessageAttachment{
		MessageID: "message-full", Ordinal: 0, RemoteID: "full-1", MIME: "image/jpeg",
	})
	thumbHash, fullHash := strings.Repeat("a", 64), strings.Repeat("b", 64)
	if err := repository.MarkDownloaded(ctx, "message-thumb", 0, thumbHash, 10, "image/jpeg", true); err != nil {
		t.Fatalf("MarkDownloaded(thumb): %v", err)
	}
	if err := repository.MarkDownloaded(ctx, "message-full", 0, fullHash, 1000, "image/jpeg", false); err != nil {
		t.Fatalf("MarkDownloaded(full): %v", err)
	}

	for _, messageID := range []string{"message-thumb", "message-full"} {
		// Another thumbnail changes neither row.
		recordMessageAttachment(t, store, repository, MessageAttachment{
			MessageID: messageID, Ordinal: 0, RemoteID: "thumb-2", RemotePreview: true, MIME: "image/png",
		})
		recordMessageAttachment(t, store, repository, MessageAttachment{
			MessageID: messageID, Ordinal: 0, RemoteID: "full-2", MIME: "image/heic", Filename: "IMG.heic",
		})
	}
	thumb, err := repository.GetForDownload(ctx, "message-thumb", 0)
	if err != nil {
		t.Fatalf("GetForDownload(thumb): %v", err)
	}
	if thumb.RemoteID != "full-2" || thumb.RemotePreview || !thumb.NeedsOriginal() ||
		*thumb.BlobHash != thumbHash || thumb.MIME != "image/jpeg" || thumb.Filename != "" || *thumb.SizeBytes != 10 {
		t.Fatalf("downloaded thumbnail after its original was named: %+v", thumb)
	}
	full, err := repository.GetForDownload(ctx, "message-full", 0)
	if err != nil {
		t.Fatalf("GetForDownload(full): %v", err)
	}
	if full.RemoteID != "full-1" || full.NeedsOriginal() || full.AwaitsOriginal() || *full.BlobHash != fullHash || full.MIME != "image/jpeg" {
		t.Fatalf("downloaded original after more updates: %+v, want it untouched", full)
	}

	if replaced, err := repository.ReplaceDownloaded(ctx, "message-thumb", 0, thumbHash, fullHash, 1000, "image/heic", false); err != nil || !replaced {
		t.Fatalf("ReplaceDownloaded() = %v, %v", replaced, err)
	}
	if upgraded, err := repository.GetForDownload(ctx, "message-thumb", 0); err != nil {
		t.Fatalf("GetForDownload(upgraded): %v", err)
	} else if upgraded.BlobPreview || upgraded.NeedsOriginal() || upgraded.AwaitsOriginal() || *upgraded.BlobHash != fullHash {
		t.Fatalf("thumbnail after its original replaced it: %+v", upgraded)
	}
}

// Migration 0012 leaves every stored row an original, and the store refuses
// to mark bytes it does not hold as a reduced copy.
func TestAttachmentPreviewColumnsDefaultToOriginals(t *testing.T) {
	clock := newMessageTestClock(messageAttachmentTestTimeMS)
	store, repository := openMessageAttachmentTestRepository(t, clock.Now)
	seedMessageAttachmentParent(t, store, "message-old")
	mustExec(t, store.db, `
		INSERT INTO message_attachments (
			message_id, ordinal, remote_id, remote_ref, filename, mime, size_bytes,
			state, blob_hash, last_error, created_at_ms, updated_at_ms
		) VALUES ('message-old', 0, 'thumb-1', X'', '', 'image/jpeg', NULL, 'pending', NULL, NULL, 1, 1)
	`)
	row, err := repository.GetForDownload(context.Background(), "message-old", 0)
	if err != nil {
		t.Fatalf("GetForDownload(): %v", err)
	}
	if row.RemotePreview || row.BlobPreview {
		t.Fatalf("row written without the columns = %+v, want neither flag", row)
	}
	if _, err := store.db.Exec(`UPDATE message_attachments SET blob_preview = 1 WHERE message_id = 'message-old'`); err == nil {
		t.Fatal("blob_preview = 1 without a blob_hash was accepted")
	}
	if _, err := store.db.Exec(`UPDATE message_attachments SET remote_preview = 2 WHERE message_id = 'message-old'`); err == nil {
		t.Fatal("remote_preview = 2 was accepted")
	}
}

// previewModel states the attachment rules for one row over a sequence of
// updates and the media service's download of whatever the row names.
type previewModel struct {
	exists      bool
	id          string
	ref         []byte
	preview     bool
	mime        string
	downloaded  bool
	blobPreview bool
	blob        string
}

func (m *previewModel) update(id string, ref []byte, preview bool, mime string) {
	if id == "" {
		preview = false
	}
	if !m.exists {
		*m = previewModel{exists: true, id: id, ref: ref, preview: preview, mime: mime}
		return
	}
	named := id != ""
	original := named && !preview
	storedOriginal := m.id != "" && !m.preview
	switch {
	case m.downloaded && !(m.blobPreview && original):
		// A downloaded row takes only the original of its reduced copy.
	case m.downloaded:
		m.id, m.ref, m.preview = id, ref, false
	case named && !(preview && storedOriginal):
		m.id, m.ref, m.preview, m.mime = id, ref, preview, mime
	case m.id == "":
		m.mime = mime
	}
}

// For any sequence of updates to an attachment, each naming nothing, a
// thumbnail or an original, with the service downloading whatever the row
// names at random points, the row matches previewModel after every step. So:
// an original's reference is never replaced by a thumbnail's; downloaded
// bytes never change; a downloaded original never changes at all; and a
// downloaded thumbnail ends up naming the last original named after it.
func TestRecordInboundAttachmentFollowsThePreviewModel(t *testing.T) {
	clock := newMessageTestClock(messageAttachmentTestTimeMS)
	store, repository := openMessageAttachmentTestRepository(t, clock.Now)
	ctx := context.Background()
	type step struct {
		id      string
		preview bool
	}
	steps := []step{
		{"", false}, {"thumb-a", true}, {"thumb-b", true}, {"full-a", false}, {"full-b", false},
		{"download", false},
	}
	cases := 0
	property := func(choices []uint8) bool {
		cases++
		messageID := fmt.Sprintf("message-%d", cases)
		seedMessageAttachmentParent(t, store, messageID)
		var model previewModel
		for index, choice := range choices {
			chosen := steps[int(choice)%len(steps)]
			if chosen.id == "download" {
				if !model.exists || model.downloaded || model.id == "" {
					continue
				}
				hash := fmt.Sprintf("%064x", index+1)
				if err := repository.MarkDownloaded(ctx, messageID, 0, hash, int64(index), "image/x-downloaded", model.preview); err != nil {
					t.Fatalf("MarkDownloaded(): %v", err)
				}
				model.downloaded, model.blobPreview, model.blob, model.mime = true, model.preview, hash, "image/x-downloaded"
			} else {
				ref := []byte(fmt.Sprintf(`{"media_id":%q,"update":%d}`, chosen.id, index))
				mime := fmt.Sprintf("image/x-update-%d", index)
				recordMessageAttachment(t, store, repository, MessageAttachment{
					MessageID: messageID, Ordinal: 0, RemoteID: chosen.id, RemoteRef: ref,
					RemotePreview: chosen.preview, MIME: mime,
				})
				model.update(chosen.id, ref, chosen.preview, mime)
			}
			got, err := repository.GetForDownload(ctx, messageID, 0)
			if err != nil {
				t.Fatalf("GetForDownload(%s): %v", messageID, err)
			}
			blob := ""
			if got.BlobHash != nil {
				blob = *got.BlobHash
			}
			if got.RemoteID != model.id || !bytes.Equal(got.RemoteRef, model.ref) || got.RemotePreview != model.preview ||
				got.MIME != model.mime || (got.State == "downloaded") != model.downloaded ||
				got.BlobPreview != model.blobPreview || blob != model.blob {
				t.Logf("steps %v, after step %d: row %+v, model %+v", choices, index, got, model)
				return false
			}
		}
		return true
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 400}); err != nil {
		t.Fatal(err)
	}
}
