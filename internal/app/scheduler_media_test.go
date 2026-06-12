package app

import (
	"bytes"
	"testing"
	"time"

	"github.com/maxghenis/openmessage/internal/db"
)

func TestProcessDueScheduledMessages_SendsMedia(t *testing.T) {
	a := newSchedulerTestApp(t)
	now := time.Now().UnixMilli()
	blob := []byte{0x89, 0x50, 0x4e, 0x47, 9, 8, 7}
	if err := a.Store.CreateScheduledMessage(&db.ScheduledMessage{
		ID: "sm1", ConversationID: "c1", Body: "caption here", SendAt: now - 1000,
		Status: db.ScheduleStatusPending, CreatedAt: now,
		MediaData: blob, MediaFilename: "pic.png", MediaMime: "image/png",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var (
		gotData    []byte
		gotName    string
		gotMime    string
		gotCaption string
	)
	textCalled := false
	a.sendTextOverride = func(cid, body, rt string) (*db.Message, error) {
		textCalled = true
		return &db.Message{MessageID: "wrong"}, nil
	}
	a.sendMediaOverride = func(cid string, data []byte, filename, mime, caption, rt string) (*db.Message, error) {
		gotData, gotName, gotMime, gotCaption = data, filename, mime, caption
		return &db.Message{MessageID: "mm99"}, nil
	}

	a.processDueScheduledMessages()

	if textCalled {
		t.Error("media message must not be sent via the text path")
	}
	if !bytes.Equal(gotData, blob) {
		t.Errorf("media blob mismatch: got %v", gotData)
	}
	if gotName != "pic.png" || gotMime != "image/png" || gotCaption != "caption here" {
		t.Errorf("media args wrong: name=%q mime=%q caption=%q", gotName, gotMime, gotCaption)
	}
	got, _ := a.Store.GetScheduledMessage("sm1")
	if got.Status != db.ScheduleStatusSent || got.SentMessageID != "mm99" {
		t.Errorf("after send = %+v, want sent with mm99", got)
	}
}
