package db

import (
	"bytes"
	"testing"
)

func TestScheduledMessageMedia(t *testing.T) {
	store := newTestStore(t)
	now := int64(1_700_000_000_000)
	blob := []byte{0x89, 0x50, 0x4e, 0x47, 1, 2, 3}

	m := &ScheduledMessage{
		ID: "m1", ConversationID: "c1", Body: "look at this", SendAt: now + 60_000, CreatedAt: now,
		MediaData: blob, MediaFilename: "photo.png", MediaMime: "image/png",
	}
	if err := store.CreateScheduledMessage(m); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Get returns the metadata but NOT the (potentially large) blob.
	got, err := store.GetScheduledMessage("m1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.MediaFilename != "photo.png" || got.MediaMime != "image/png" || got.Body != "look at this" {
		t.Errorf("metadata wrong: %+v", got)
	}
	if len(got.MediaData) != 0 {
		t.Errorf("GetScheduledMessage should not load the media blob")
	}

	// The blob loads on demand (used by the scheduler at send time).
	data, err := store.GetScheduledMediaData("m1")
	if err != nil {
		t.Fatalf("media data: %v", err)
	}
	if !bytes.Equal(data, blob) {
		t.Errorf("blob mismatch: got %v want %v", data, blob)
	}

	// The list (UI) carries metadata but not the blob.
	list, err := store.ListScheduledMessages("c1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].MediaFilename != "photo.png" {
		t.Fatalf("list metadata wrong: %+v", list)
	}
	if len(list[0].MediaData) != 0 {
		t.Errorf("list must not include the media blob")
	}
}
