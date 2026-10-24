package sqlite

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"testing/quick"
)

// Google named Michaela's image 89144 on 2026-10-18 and, in the same batch,
// sent an update of the message whose media part named nothing. The pending
// row took the empty reference and the image could never download.
func TestRecordInboundAttachmentKeepsTheReferenceWhenAnUpdateNamesNone(t *testing.T) {
	clock := newMessageTestClock(messageAttachmentTestTimeMS)
	store, repository := openMessageAttachmentTestRepository(t, clock.Now)
	seedMessageAttachmentParent(t, store, "message-named")
	named := MessageAttachment{
		MessageID: "message-named", Ordinal: 0,
		RemoteID: "thumb-1", RemoteRef: []byte(`{"v":1,"media_id":"thumb-1","decryption_key":"0a"}`),
		Filename: "IMG.jpeg", MIME: "image/jpeg",
	}
	recordMessageAttachment(t, store, repository, named)

	for _, emptyID := range []string{"", "  \t"} {
		recordMessageAttachment(t, store, repository, MessageAttachment{
			MessageID: "message-named", Ordinal: 0,
			RemoteID: emptyID, RemoteRef: []byte(`{"v":1,"media_id":"","decryption_key":""}`),
			Filename: "IMG.jpeg", MIME: "image/jpeg",
		})
		got, err := repository.GetForDownload(context.Background(), "message-named", 0)
		if err != nil {
			t.Fatalf("GetForDownload(): %v", err)
		}
		if got.RemoteID != named.RemoteID || !bytes.Equal(got.RemoteRef, named.RemoteRef) {
			t.Fatalf("after an update naming %q: reference = %q %s, want %q %s kept",
				emptyID, got.RemoteID, got.RemoteRef, named.RemoteID, named.RemoteRef)
		}
	}

	// A later update that names the media again still replaces it.
	renamed := named
	renamed.RemoteID = "full-1"
	renamed.RemoteRef = []byte(`{"v":1,"media_id":"full-1","decryption_key":"0b"}`)
	recordMessageAttachment(t, store, repository, renamed)
	got, err := repository.GetForDownload(context.Background(), "message-named", 0)
	if err != nil {
		t.Fatalf("GetForDownload(): %v", err)
	}
	if got.RemoteID != "full-1" || !bytes.Equal(got.RemoteRef, renamed.RemoteRef) {
		t.Fatalf("after renaming: reference = %q %s, want full-1", got.RemoteID, got.RemoteRef)
	}
}

// For any sequence of updates to a pending attachment, each naming one of a
// few media or nothing, the row ends up with the reference of the last update
// that named something, exactly as that update sent it, and with no
// reference only if none did.
func TestRecordInboundAttachmentReferenceIsTheLastNamedUpdate(t *testing.T) {
	clock := newMessageTestClock(messageAttachmentTestTimeMS)
	store, repository := openMessageAttachmentTestRepository(t, clock.Now)
	refFor := func(id string, update int) []byte {
		return []byte(fmt.Sprintf(`{"v":1,"media_id":%q,"update":%d}`, id, update))
	}
	ids := []string{"", "", "media-a", "media-b", "media-c"}
	cases := 0
	property := func(choices []uint8) bool {
		cases++
		messageID := fmt.Sprintf("message-%d", cases)
		seedMessageAttachmentParent(t, store, messageID)
		wantID, wantRef := "", []byte{}
		for update, choice := range choices {
			id := ids[int(choice)%len(ids)]
			ref := refFor(id, update)
			if update == 0 || id != "" {
				wantID, wantRef = id, ref
			}
			recordMessageAttachment(t, store, repository, MessageAttachment{
				MessageID: messageID, Ordinal: 0, RemoteID: id, RemoteRef: ref, MIME: "image/jpeg",
			})
		}
		if len(choices) == 0 {
			return true
		}
		got, err := repository.GetForDownload(context.Background(), messageID, 0)
		if err != nil {
			t.Fatalf("GetForDownload(%s): %v", messageID, err)
		}
		if got.RemoteID != wantID || !bytes.Equal(got.RemoteRef, wantRef) {
			t.Logf("updates %v: reference = %q %s, want %q %s", choices, got.RemoteID, got.RemoteRef, wantID, wantRef)
			return false
		}
		return true
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 300}); err != nil {
		t.Fatal(err)
	}
}
