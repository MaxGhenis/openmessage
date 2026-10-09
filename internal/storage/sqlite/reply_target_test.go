package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

const replyTargetTestTimeMS = int64(1_700_000_000_000)

func TestFirstAttachmentMIMEReadsStoredAttachmentsThenTheOutbox(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "store.sqlite3"))
	if err != nil {
		t.Fatalf("Open(): %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close(): %v", err)
		}
	})
	now := func() time.Time { return time.UnixMilli(replyTargetTestTimeMS) }
	messages, err := NewMessageRepository(store, now)
	if err != nil {
		t.Fatalf("NewMessageRepository(): %v", err)
	}
	outbox, err := NewOutboxRepository(store, now)
	if err != nil {
		t.Fatalf("NewOutboxRepository(): %v", err)
	}
	seedMessageAccount(t, store, "account-a", "test")
	seedMessageIdentity(t, store, "identity-a", "account-a")
	seedMessageConversation(t, store, "conversation-a", "account-a")

	insertMessage := func(messageID string) {
		t.Helper()
		mustExec(t, store.db, `
			INSERT INTO messages (
				message_id, conversation_id, account_id, remote_message_id,
				sender_identity_id, direction, body, reply_to_remote_id, state,
				occurred_at_ms, created_at_ms, updated_at_ms
			) VALUES (?, 'conversation-a', 'account-a', ?, NULL, 'incoming', '', NULL,
			          'active', ?, ?, ?)
		`, messageID, "remote-"+messageID, replyTargetTestTimeMS, replyTargetTestTimeMS, replyTargetTestTimeMS)
	}
	insertAttachment := func(messageID string, ordinal int, mime string) {
		t.Helper()
		mustExec(t, store.db, `
			INSERT INTO message_attachments (
				message_id, ordinal, mime, created_at_ms, updated_at_ms
			) VALUES (?, ?, ?, ?, ?)
		`, messageID, ordinal, mime, replyTargetTestTimeMS, replyTargetTestTimeMS)
	}

	insertMessage("message-ordinals")
	insertAttachment("message-ordinals", 2, "video/mp4")
	insertAttachment("message-ordinals", 1, "image/jpeg")
	insertMessage("message-none")

	media := outboxTestMediaItem("reply-target-media")
	if _, _, err := outbox.EnqueueOutgoingMediaMessage(
		context.Background(),
		media,
		outboxTestOutgoingMessage(media, ""),
		outboxTestAttachment(),
	); err != nil {
		t.Fatalf("EnqueueOutgoingMediaMessage(): %v", err)
	}
	text := outboxTestItem("reply-target-text")
	if _, _, err := outbox.EnqueueOutgoingMessage(
		context.Background(),
		text,
		outboxTestOutgoingMessage(text, "text only"),
	); err != nil {
		t.Fatalf("EnqueueOutgoingMessage(): %v", err)
	}
	// A stored attachment row outranks the outbox's for the same message.
	shadowed := outboxTestMediaItem("reply-target-shadowed")
	if _, _, err := outbox.EnqueueOutgoingMediaMessage(
		context.Background(),
		shadowed,
		outboxTestOutgoingMessage(shadowed, ""),
		outboxTestAttachment(),
	); err != nil {
		t.Fatalf("EnqueueOutgoingMediaMessage(shadowed): %v", err)
	}
	insertAttachment(shadowed.LocalMessageID, 0, "audio/aac")

	for _, test := range []struct {
		messageID string
		wantMIME  string
		wantOK    bool
	}{
		{messageID: "message-ordinals", wantMIME: "image/jpeg", wantOK: true},
		{messageID: "message-none"},
		{messageID: media.LocalMessageID, wantMIME: "image/png", wantOK: true},
		{messageID: text.LocalMessageID},
		{messageID: shadowed.LocalMessageID, wantMIME: "audio/aac", wantOK: true},
		{messageID: "message-missing"},
	} {
		mime, ok, err := messages.FirstAttachmentMIME(context.Background(), test.messageID)
		if err != nil || mime != test.wantMIME || ok != test.wantOK {
			t.Errorf("FirstAttachmentMIME(%q) = (%q, %v, %v), want (%q, %v, nil)",
				test.messageID, mime, ok, err, test.wantMIME, test.wantOK)
		}
	}
}
