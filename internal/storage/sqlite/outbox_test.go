package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

const outboxTestTimeMS int64 = 2_000_000_000_000

func TestOutboxEnqueueIdempotencyAndConflict(t *testing.T) {
	clock := newOutboxTestClock(outboxTestTimeMS)
	store, repository := openOutboxTestRepository(t, clock.Now)
	ctx := context.Background()

	original := outboxTestItem("original")
	row, disposition, err := repository.Enqueue(ctx, original)
	if err != nil {
		t.Fatalf("Enqueue(original): %v", err)
	}
	if disposition != EnqueueInserted {
		t.Fatalf("Enqueue(original) disposition = %q, want %q", disposition, EnqueueInserted)
	}
	if row.OutboxID != original.OutboxID || row.State != OutboxQueued {
		t.Fatalf("Enqueue(original) row = %+v", row)
	}
	if row.CreatedAtMS != outboxTestTimeMS || row.ScheduledForMS != outboxTestTimeMS {
		t.Fatalf("Enqueue(original) timestamps = %+v", row)
	}

	clock.Set(outboxTestTimeMS + 500)
	duplicate := original
	duplicate.OutboxID = "outbox-duplicate"
	duplicate.TransportRequestID = "request-duplicate"
	duplicate.LocalMessageID = "message-duplicate"
	duplicateRow, disposition, err := repository.Enqueue(ctx, duplicate)
	if err != nil {
		t.Fatalf("Enqueue(duplicate): %v", err)
	}
	if disposition != EnqueueExisting {
		t.Fatalf("Enqueue(duplicate) disposition = %q, want %q", disposition, EnqueueExisting)
	}
	if duplicateRow.OutboxID != original.OutboxID || duplicateRow.CreatedAtMS != outboxTestTimeMS {
		t.Fatalf("Enqueue(duplicate) returned %+v, want original row", duplicateRow)
	}

	conflicts := []struct {
		name   string
		mutate func(*NewOutboxItem)
	}{
		{name: "operation", mutate: func(item *NewOutboxItem) { item.Operation = "edit_text" }},
		{name: "conversation", mutate: func(item *NewOutboxItem) { item.ConversationID = "conversation-b" }},
		{name: "payload hash", mutate: func(item *NewOutboxItem) { item.PayloadHash = "different-hash" }},
	}
	for i, test := range conflicts {
		t.Run(test.name, func(t *testing.T) {
			candidate := original
			candidate.OutboxID = fmt.Sprintf("outbox-conflict-%d", i)
			candidate.TransportRequestID = fmt.Sprintf("request-conflict-%d", i)
			test.mutate(&candidate)
			_, _, err := repository.Enqueue(ctx, candidate)
			if !errors.Is(err, ErrIdempotencyConflict) {
				t.Fatalf("Enqueue(conflict) error = %v, want ErrIdempotencyConflict", err)
			}
		})
	}
	assertRowCount(t, store.db, "outbox", 1)

	byRequest, err := repository.FindByTransportRequestID(ctx, original.AccountID, original.TransportRequestID)
	if err != nil {
		t.Fatalf("FindByTransportRequestID(): %v", err)
	}
	if byRequest.OutboxID != original.OutboxID {
		t.Fatalf("FindByTransportRequestID() ID = %q, want %q", byRequest.OutboxID, original.OutboxID)
	}
	if _, err := repository.FindByID(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("FindByID(missing) error = %v, want ErrNotFound", err)
	}
}

func TestOutboxSendAgainLinkRoundTripsAndClearsWhenPredecessorDeleted(t *testing.T) {
	clock := newOutboxTestClock(outboxTestTimeMS)
	store, repository := openOutboxTestRepository(t, clock.Now)
	ctx := context.Background()

	predecessor := outboxTestItem("send-again-predecessor")
	mustEnqueueOutbox(t, repository, predecessor)

	successor := outboxTestItem("send-again-successor")
	successor.SendAgainOfOutboxID = predecessor.OutboxID
	inserted, disposition, err := repository.Enqueue(ctx, successor)
	if err != nil {
		t.Fatalf("Enqueue(successor): %v", err)
	}
	if disposition != EnqueueInserted {
		t.Fatalf("Enqueue(successor) disposition = %q, want %q", disposition, EnqueueInserted)
	}
	assertOutboxText(
		t,
		"inserted send_again_of_outbox_id",
		inserted.SendAgainOfOutboxID,
		predecessor.OutboxID,
	)

	found, err := repository.FindByID(ctx, successor.OutboxID)
	if err != nil {
		t.Fatalf("FindByID(successor): %v", err)
	}
	assertOutboxText(
		t,
		"scanned send_again_of_outbox_id",
		found.SendAgainOfOutboxID,
		predecessor.OutboxID,
	)

	if _, err := store.db.ExecContext(ctx, `DELETE FROM outbox WHERE outbox_id = ?`, predecessor.OutboxID); err != nil {
		t.Fatalf("delete predecessor: %v", err)
	}
	found, err = repository.FindByID(ctx, successor.OutboxID)
	if err != nil {
		t.Fatalf("FindByID(successor after predecessor delete): %v", err)
	}
	if found.SendAgainOfOutboxID != nil {
		t.Fatalf("send_again_of_outbox_id after predecessor delete = %v, want nil", found.SendAgainOfOutboxID)
	}
}

func TestOutboxEnqueueRejectsBlankSendAgainLink(t *testing.T) {
	_, repository := openOutboxTestRepository(t, func() time.Time {
		return time.UnixMilli(outboxTestTimeMS)
	})
	item := outboxTestItem("blank-send-again-link")
	item.SendAgainOfOutboxID = " \t "

	if _, _, err := repository.Enqueue(context.Background(), item); err == nil ||
		!strings.Contains(err.Error(), "send again predecessor outbox ID is empty") {
		t.Fatalf("Enqueue(blank send-again link) error = %v", err)
	}
}

func TestOutboxEnqueueOutgoingMessageIsAtomicAndDeduplicated(t *testing.T) {
	clock := newOutboxTestClock(outboxTestTimeMS)
	store, repository := openOutboxTestRepository(t, clock.Now)
	seedMessageIdentity(t, store, "identity-a", "account-a")
	seedMessageConversation(t, store, "conversation-a", "account-a")
	ctx := context.Background()

	item := outboxTestItem("outgoing")
	message := outboxTestOutgoingMessage(item, "hello from the durable outbox")
	row, disposition, err := repository.EnqueueOutgoingMessage(ctx, item, message)
	if err != nil {
		t.Fatalf("EnqueueOutgoingMessage(): %v", err)
	}
	if disposition != EnqueueInserted || row.OutboxID != item.OutboxID {
		t.Fatalf("EnqueueOutgoingMessage() = (%+v, %q), want inserted", row, disposition)
	}
	messageRepository, err := NewMessageRepository(store, clock.Now)
	if err != nil {
		t.Fatalf("NewMessageRepository(): %v", err)
	}
	gotMessage, err := messageRepository.GetMessage(ctx, item.LocalMessageID)
	if err != nil {
		t.Fatalf("GetMessage(): %v", err)
	}
	if gotMessage.Body != message.Body || gotMessage.Direction != MessageDirectionOutgoing ||
		gotMessage.State != MessageStateActive || gotMessage.RemoteMessageID != message.RemoteMessageID ||
		gotMessage.SenderIdentityID == nil || *gotMessage.SenderIdentityID != "identity-a" {
		t.Fatalf("optimistic message = %+v, want %+v", gotMessage, message)
	}
	assertRowCount(t, store.db, "inbox", 0)

	clock.Set(outboxTestTimeMS + 500)
	duplicate := item
	duplicate.OutboxID = "outbox-unused-duplicate"
	duplicate.LocalMessageID = "message-unused-duplicate"
	duplicate.TransportRequestID = "request-unused-duplicate"
	duplicateRow, disposition, err := repository.EnqueueOutgoingMessage(ctx, duplicate, Message{})
	if err != nil {
		t.Fatalf("EnqueueOutgoingMessage(duplicate with invalid candidate): %v", err)
	}
	if disposition != EnqueueExisting || duplicateRow.OutboxID != item.OutboxID {
		t.Fatalf("duplicate result = (%+v, %q), want original", duplicateRow, disposition)
	}

	conflict := duplicate
	conflict.PayloadHash = "different-payload-hash"
	if _, _, err := repository.EnqueueOutgoingMessage(ctx, conflict, Message{}); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("EnqueueOutgoingMessage(conflict) error = %v, want ErrIdempotencyConflict", err)
	}

	generic := outboxTestItem("generic-collision")
	mustEnqueueOutbox(t, repository, generic)
	if _, _, err := repository.EnqueueOutgoingMessage(
		ctx,
		generic,
		outboxTestOutgoingMessage(generic, "missing optimistic pair"),
	); !errors.Is(err, ErrInvalidMessage) {
		t.Fatalf("EnqueueOutgoingMessage(generic collision) error = %v, want ErrInvalidMessage", err)
	}
	foreignPair := outboxTestItem("foreign-pair")
	if _, _, err := repository.EnqueueOutgoingMessage(
		ctx,
		foreignPair,
		outboxTestOutgoingMessage(foreignPair, "belongs to another request"),
	); err != nil {
		t.Fatalf("EnqueueOutgoingMessage(foreign pair): %v", err)
	}
	genericWithForeignMessage := outboxTestItem("generic-foreign-message")
	genericWithForeignMessage.LocalMessageID = foreignPair.LocalMessageID
	mustEnqueueOutbox(t, repository, genericWithForeignMessage)
	if _, _, err := repository.EnqueueOutgoingMessage(
		ctx,
		genericWithForeignMessage,
		outboxTestOutgoingMessage(genericWithForeignMessage, "candidate is not used"),
	); !errors.Is(err, ErrInvalidMessage) {
		t.Fatalf("EnqueueOutgoingMessage(foreign message collision) error = %v, want ErrInvalidMessage", err)
	}

	invalidItem := outboxTestItem("invalid-outgoing")
	invalidMessage := outboxTestOutgoingMessage(invalidItem, "must roll back")
	invalidMessage.State = MessageStateEdited
	if _, _, err := repository.EnqueueOutgoingMessage(ctx, invalidItem, invalidMessage); !errors.Is(err, ErrInvalidMessage) {
		t.Fatalf("EnqueueOutgoingMessage(invalid) error = %v, want ErrInvalidMessage", err)
	}
	assertRowCount(t, store.db, "outbox", 4)
	assertRowCount(t, store.db, "messages", 2)
}

func TestOutboxEnqueueOutgoingMediaMessageRoundTripAndDeduplicates(t *testing.T) {
	clock := newOutboxTestClock(outboxTestTimeMS)
	store, repository := openOutboxTestRepository(t, clock.Now)
	seedMessageIdentity(t, store, "identity-a", "account-a")
	seedMessageConversation(t, store, "conversation-a", "account-a")
	ctx := context.Background()

	item := outboxTestMediaItem("media")
	message := outboxTestOutgoingMessage(item, "photo caption")
	attachment := outboxTestAttachment()
	row, disposition, err := repository.EnqueueOutgoingMediaMessage(ctx, item, message, attachment)
	if err != nil {
		t.Fatalf("EnqueueOutgoingMediaMessage(): %v", err)
	}
	if disposition != EnqueueInserted || row.OutboxID != item.OutboxID {
		t.Fatalf("EnqueueOutgoingMediaMessage() = (%+v, %q), want inserted", row, disposition)
	}

	got, err := repository.GetOutboxAttachment(ctx, item.OutboxID)
	if err != nil {
		t.Fatalf("GetOutboxAttachment(): %v", err)
	}
	want := attachment
	want.OutboxID = item.OutboxID
	want.Ordinal = 0
	want.CreatedAtMS = outboxTestTimeMS
	if got != want {
		t.Fatalf("GetOutboxAttachment() = %+v, want %+v", got, want)
	}

	messageRepository, err := NewMessageRepository(store, clock.Now)
	if err != nil {
		t.Fatalf("NewMessageRepository(): %v", err)
	}
	gotMessage, err := messageRepository.GetMessage(ctx, item.LocalMessageID)
	if err != nil {
		t.Fatalf("GetMessage(): %v", err)
	}
	if gotMessage.Body != message.Body {
		t.Fatalf("optimistic media message body = %q, want caption %q", gotMessage.Body, message.Body)
	}

	clock.Set(outboxTestTimeMS + 500)
	duplicate := item
	duplicate.OutboxID = "outbox-unused-media-duplicate"
	duplicate.LocalMessageID = "message-unused-media-duplicate"
	duplicate.TransportRequestID = "request-unused-media-duplicate"
	duplicateRow, disposition, err := repository.EnqueueOutgoingMediaMessage(
		ctx,
		duplicate,
		Message{},
		attachment,
	)
	if err != nil {
		t.Fatalf("EnqueueOutgoingMediaMessage(duplicate): %v", err)
	}
	if disposition != EnqueueExisting || duplicateRow.OutboxID != item.OutboxID {
		t.Fatalf("duplicate result = (%+v, %q), want original", duplicateRow, disposition)
	}
	assertRowCount(t, store.db, "outbox", 1)
	assertRowCount(t, store.db, "messages", 1)
	assertRowCount(t, store.db, "outbox_attachments", 1)

	if _, err := repository.GetOutboxAttachment(ctx, "missing"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetOutboxAttachment(missing) error = %v, want sql.ErrNoRows", err)
	}
}

func TestOutboxEnqueueOutgoingMediaMessageRollsBackAttachmentInsertFailure(t *testing.T) {
	clock := newOutboxTestClock(outboxTestTimeMS)
	store, repository := openOutboxTestRepository(t, clock.Now)
	seedMessageIdentity(t, store, "identity-a", "account-a")
	seedMessageConversation(t, store, "conversation-a", "account-a")
	mustExec(t, store.db, `
		CREATE TRIGGER fail_outbox_attachment_insert
		BEFORE INSERT ON outbox_attachments
		BEGIN
			SELECT RAISE(ABORT, 'injected outbox attachment failure');
		END
	`)

	item := outboxTestMediaItem("atomic-failure")
	_, _, err := repository.EnqueueOutgoingMediaMessage(
		context.Background(),
		item,
		outboxTestOutgoingMessage(item, "must roll back"),
		outboxTestAttachment(),
	)
	if err == nil {
		t.Fatal("EnqueueOutgoingMediaMessage() succeeded, want injected attachment failure")
	}
	assertRowCount(t, store.db, "outbox", 0)
	assertRowCount(t, store.db, "messages", 0)
	assertRowCount(t, store.db, "outbox_attachments", 0)
}

func TestOutboxEnqueueValidatesMediaAttachmentPair(t *testing.T) {
	clock := newOutboxTestClock(outboxTestTimeMS)
	store, repository := openOutboxTestRepository(t, clock.Now)
	seedMessageIdentity(t, store, "identity-a", "account-a")
	seedMessageConversation(t, store, "conversation-a", "account-a")
	ctx := context.Background()

	tests := []struct {
		name    string
		enqueue func() error
	}{
		{
			name: "media without attachment",
			enqueue: func() error {
				item := outboxTestMediaItem("missing-attachment")
				_, _, err := repository.EnqueueOutgoingMessage(
					ctx,
					item,
					outboxTestOutgoingMessage(item, "caption"),
				)
				return err
			},
		},
		{
			name: "text with attachment",
			enqueue: func() error {
				item := outboxTestItem("text-attachment")
				_, _, err := repository.EnqueueOutgoingMediaMessage(
					ctx,
					item,
					outboxTestOutgoingMessage(item, "body"),
					outboxTestAttachment(),
				)
				return err
			},
		},
		{
			name: "invalid blob hash",
			enqueue: func() error {
				item := outboxTestMediaItem("invalid-hash")
				attachment := outboxTestAttachment()
				attachment.BlobHash = "not-a-sha256"
				_, _, err := repository.EnqueueOutgoingMediaMessage(
					ctx, item, outboxTestOutgoingMessage(item, "caption"), attachment,
				)
				return err
			},
		},
		{
			name: "nonpositive size",
			enqueue: func() error {
				item := outboxTestMediaItem("invalid-size")
				attachment := outboxTestAttachment()
				attachment.SizeBytes = 0
				_, _, err := repository.EnqueueOutgoingMediaMessage(
					ctx, item, outboxTestOutgoingMessage(item, "caption"), attachment,
				)
				return err
			},
		},
		{
			name: "empty MIME",
			enqueue: func() error {
				item := outboxTestMediaItem("invalid-mime")
				attachment := outboxTestAttachment()
				attachment.MIME = " \t"
				_, _, err := repository.EnqueueOutgoingMediaMessage(
					ctx, item, outboxTestOutgoingMessage(item, "caption"), attachment,
				)
				return err
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.enqueue(); err == nil {
				t.Fatal("enqueue succeeded, want validation error")
			}
		})
	}
	assertRowCount(t, store.db, "outbox", 0)
	assertRowCount(t, store.db, "messages", 0)
	assertRowCount(t, store.db, "outbox_attachments", 0)
}

func TestOutboxAttachmentCascadesAndExistingMediaRequiresAttachment(t *testing.T) {
	clock := newOutboxTestClock(outboxTestTimeMS)
	store, repository := openOutboxTestRepository(t, clock.Now)
	seedMessageIdentity(t, store, "identity-a", "account-a")
	seedMessageConversation(t, store, "conversation-a", "account-a")
	ctx := context.Background()

	item := outboxTestMediaItem("cascade")
	if _, _, err := repository.EnqueueOutgoingMediaMessage(
		ctx,
		item,
		outboxTestOutgoingMessage(item, "caption"),
		outboxTestAttachment(),
	); err != nil {
		t.Fatalf("EnqueueOutgoingMediaMessage(): %v", err)
	}
	assertRowCount(t, store.db, "outbox_attachments", 1)
	mustExec(t, store.db, `DELETE FROM outbox WHERE outbox_id = ?`, item.OutboxID)
	assertRowCount(t, store.db, "outbox_attachments", 0)

	item = outboxTestMediaItem("missing-existing-attachment")
	if _, _, err := repository.EnqueueOutgoingMediaMessage(
		ctx,
		item,
		outboxTestOutgoingMessage(item, "caption"),
		outboxTestAttachment(),
	); err != nil {
		t.Fatalf("EnqueueOutgoingMediaMessage(second): %v", err)
	}
	mustExec(t, store.db, `DELETE FROM outbox_attachments WHERE outbox_id = ?`, item.OutboxID)
	duplicate := item
	duplicate.OutboxID = "outbox-unused-missing-existing-attachment"
	duplicate.LocalMessageID = "message-unused-missing-existing-attachment"
	duplicate.TransportRequestID = "request-unused-missing-existing-attachment"
	if _, _, err := repository.EnqueueOutgoingMediaMessage(
		ctx,
		duplicate,
		Message{},
		outboxTestAttachment(),
	); err == nil {
		t.Fatal("EnqueueOutgoingMediaMessage(existing without attachment) succeeded")
	}
}

func TestOutboxEnqueueReactionRoundTripsAndDeduplicates(t *testing.T) {
	clock := newOutboxTestClock(outboxTestTimeMS)
	store, repository := openOutboxTestRepository(t, clock.Now)
	seedMessageConversation(t, store, "conversation-a", "account-a")
	seedOutboxTestMessage(t, store, "target-a", "account-a", "conversation-a")
	ctx := context.Background()

	item := outboxTestReactionItem("reaction")
	reaction := OutboxReaction{
		TargetMessageID: "target-a",
		Emoji:           "👍",
		Action:          "add",
	}
	row, disposition, err := repository.EnqueueReaction(ctx, item, reaction)
	if err != nil {
		t.Fatalf("EnqueueReaction(): %v", err)
	}
	if disposition != EnqueueInserted || row.OutboxID != item.OutboxID || row.LocalMessageID != nil {
		t.Fatalf("EnqueueReaction() = (%+v, %q), want message-less inserted row", row, disposition)
	}
	got, err := repository.GetOutboxReaction(ctx, item.OutboxID)
	if err != nil {
		t.Fatalf("GetOutboxReaction(): %v", err)
	}
	want := reaction
	want.OutboxID = item.OutboxID
	want.CreatedAtMS = outboxTestTimeMS
	if got != want {
		t.Fatalf("GetOutboxReaction() = %+v, want %+v", got, want)
	}
	assertRowCount(t, store.db, "messages", 1)

	clock.Set(outboxTestTimeMS + 500)
	duplicate := item
	duplicate.OutboxID = "outbox-unused-reaction-duplicate"
	duplicate.TransportRequestID = "request-unused-reaction-duplicate"
	duplicateRow, disposition, err := repository.EnqueueReaction(ctx, duplicate, reaction)
	if err != nil {
		t.Fatalf("EnqueueReaction(duplicate): %v", err)
	}
	if disposition != EnqueueExisting || duplicateRow.OutboxID != item.OutboxID {
		t.Fatalf("duplicate result = (%+v, %q), want original", duplicateRow, disposition)
	}
	assertRowCount(t, store.db, "outbox", 1)
	assertRowCount(t, store.db, "outbox_reactions", 1)

	conflict := duplicate
	conflict.OutboxID = "outbox-reaction-conflict"
	conflict.TransportRequestID = "request-reaction-conflict"
	conflict.PayloadHash = "different-reaction-hash"
	if _, _, err := repository.EnqueueReaction(ctx, conflict, reaction); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("EnqueueReaction(conflict) error = %v, want ErrIdempotencyConflict", err)
	}

	mustExec(t, store.db, `DELETE FROM outbox_reactions WHERE outbox_id = ?`, item.OutboxID)
	missingPair := duplicate
	missingPair.OutboxID = "outbox-unused-missing-reaction"
	missingPair.TransportRequestID = "request-unused-missing-reaction"
	if _, _, err := repository.EnqueueReaction(ctx, missingPair, reaction); err == nil {
		t.Fatal("EnqueueReaction(existing without side row) succeeded")
	}
	if _, err := repository.GetOutboxReaction(ctx, "missing"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetOutboxReaction(missing) error = %v, want sql.ErrNoRows", err)
	}
}

func TestOutboxEnqueueReadReceiptRoundTripsAndSkipsCursorOnReplay(t *testing.T) {
	clock := newOutboxTestClock(outboxTestTimeMS)
	store, repository := openOutboxTestRepository(t, clock.Now)
	seedMessageConversation(t, store, "conversation-a", "account-a")
	seedOutboxTestDevice(t, store, "device-a", "account-a")
	seedOutboxTestMessage(t, store, "target-a", "account-a", "conversation-a")
	ctx := context.Background()

	item := outboxTestReadItem("read")
	receipt := OutboxReadReceipt{
		DeviceID:          "device-a",
		LastReadMessageID: "target-a",
		ReadAtMS:          outboxTestTimeMS - 100,
	}
	targetID := "target-a"
	cursor := ReadCursor{
		AccountID:         "account-a",
		DeviceID:          "device-a",
		ConversationID:    "conversation-a",
		LastReadMessageID: &targetID,
		LastReadAtMS:      receipt.ReadAtMS,
		UpdatedAtMS:       outboxTestTimeMS,
	}
	row, disposition, err := repository.EnqueueReadReceipt(ctx, item, receipt, cursor)
	if err != nil {
		t.Fatalf("EnqueueReadReceipt(): %v", err)
	}
	if disposition != EnqueueInserted || row.OutboxID != item.OutboxID || row.LocalMessageID != nil {
		t.Fatalf("EnqueueReadReceipt() = (%+v, %q), want message-less inserted row", row, disposition)
	}
	gotReceipt, err := repository.GetOutboxReadReceipt(ctx, item.OutboxID)
	if err != nil {
		t.Fatalf("GetOutboxReadReceipt(): %v", err)
	}
	wantReceipt := receipt
	wantReceipt.OutboxID = item.OutboxID
	wantReceipt.CreatedAtMS = outboxTestTimeMS
	if gotReceipt != wantReceipt {
		t.Fatalf("GetOutboxReadReceipt() = %+v, want %+v", gotReceipt, wantReceipt)
	}
	gotCursor, err := store.GetReadCursor("device-a", "conversation-a")
	if err != nil {
		t.Fatalf("GetReadCursor(): %v", err)
	}
	if gotCursor.LastReadMessageID == nil || *gotCursor.LastReadMessageID != targetID ||
		gotCursor.LastReadAtMS != receipt.ReadAtMS || gotCursor.UpdatedAtMS != cursor.UpdatedAtMS {
		t.Fatalf("GetReadCursor() = %+v, want %+v", gotCursor, cursor)
	}

	clock.Set(outboxTestTimeMS + 500)
	duplicate := item
	duplicate.OutboxID = "outbox-unused-read-duplicate"
	duplicate.TransportRequestID = "request-unused-read-duplicate"
	candidateCursor := cursor
	candidateCursor.LastReadAtMS += 10_000
	candidateCursor.UpdatedAtMS += 10_000
	duplicateRow, disposition, err := repository.EnqueueReadReceipt(
		ctx,
		duplicate,
		receipt,
		candidateCursor,
	)
	if err != nil {
		t.Fatalf("EnqueueReadReceipt(duplicate): %v", err)
	}
	if disposition != EnqueueExisting || duplicateRow.OutboxID != item.OutboxID {
		t.Fatalf("duplicate result = (%+v, %q), want original", duplicateRow, disposition)
	}
	unchangedCursor, err := store.GetReadCursor("device-a", "conversation-a")
	if err != nil {
		t.Fatalf("GetReadCursor(after replay): %v", err)
	}
	if unchangedCursor.LastReadAtMS != cursor.LastReadAtMS || unchangedCursor.UpdatedAtMS != cursor.UpdatedAtMS {
		t.Fatalf("replay changed cursor: got %+v, want %+v", unchangedCursor, cursor)
	}
	assertRowCount(t, store.db, "outbox", 1)
	assertRowCount(t, store.db, "outbox_read_receipts", 1)
	assertRowCount(t, store.db, "read_cursors", 1)

	conflict := duplicate
	conflict.OutboxID = "outbox-read-conflict"
	conflict.TransportRequestID = "request-read-conflict"
	conflict.PayloadHash = "different-read-hash"
	if _, _, err := repository.EnqueueReadReceipt(ctx, conflict, receipt, cursor); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("EnqueueReadReceipt(conflict) error = %v, want ErrIdempotencyConflict", err)
	}

	mustExec(t, store.db, `DELETE FROM outbox_read_receipts WHERE outbox_id = ?`, item.OutboxID)
	missingPair := duplicate
	missingPair.OutboxID = "outbox-unused-missing-read-receipt"
	missingPair.TransportRequestID = "request-unused-missing-read-receipt"
	if _, _, err := repository.EnqueueReadReceipt(ctx, missingPair, receipt, cursor); err == nil {
		t.Fatal("EnqueueReadReceipt(existing without side row) succeeded")
	}
	if _, err := repository.GetOutboxReadReceipt(ctx, "missing"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetOutboxReadReceipt(missing) error = %v, want sql.ErrNoRows", err)
	}
}

func TestOutboxCarrierInsertsAreAtomic(t *testing.T) {
	t.Run("reaction side row", func(t *testing.T) {
		clock := newOutboxTestClock(outboxTestTimeMS)
		store, repository := openOutboxTestRepository(t, clock.Now)
		seedMessageConversation(t, store, "conversation-a", "account-a")
		seedOutboxTestMessage(t, store, "target-a", "account-a", "conversation-a")
		mustExec(t, store.db, `
			CREATE TRIGGER fail_outbox_reaction_insert
			BEFORE INSERT ON outbox_reactions
			BEGIN
				SELECT RAISE(ABORT, 'injected reaction carrier failure');
			END
		`)
		_, _, err := repository.EnqueueReaction(
			context.Background(),
			outboxTestReactionItem("atomic-reaction"),
			OutboxReaction{TargetMessageID: "target-a", Emoji: "❤️", Action: "add"},
		)
		if err == nil {
			t.Fatal("EnqueueReaction() succeeded, want injected side-row failure")
		}
		assertRowCount(t, store.db, "outbox", 0)
		assertRowCount(t, store.db, "outbox_reactions", 0)
		assertRowCount(t, store.db, "messages", 1)
	})

	t.Run("read cursor", func(t *testing.T) {
		clock := newOutboxTestClock(outboxTestTimeMS)
		store, repository := openOutboxTestRepository(t, clock.Now)
		seedMessageConversation(t, store, "conversation-a", "account-a")
		seedOutboxTestDevice(t, store, "device-a", "account-a")
		seedOutboxTestMessage(t, store, "target-a", "account-a", "conversation-a")
		mustExec(t, store.db, `
			CREATE TRIGGER fail_read_cursor_insert
			BEFORE INSERT ON read_cursors
			BEGIN
				SELECT RAISE(ABORT, 'injected read cursor failure');
			END
		`)
		targetID := "target-a"
		_, _, err := repository.EnqueueReadReceipt(
			context.Background(),
			outboxTestReadItem("atomic-read"),
			OutboxReadReceipt{
				DeviceID: "device-a", LastReadMessageID: targetID, ReadAtMS: outboxTestTimeMS,
			},
			ReadCursor{
				AccountID: "account-a", DeviceID: "device-a", ConversationID: "conversation-a",
				LastReadMessageID: &targetID, LastReadAtMS: outboxTestTimeMS, UpdatedAtMS: outboxTestTimeMS,
			},
		)
		if err == nil {
			t.Fatal("EnqueueReadReceipt() succeeded, want injected cursor failure")
		}
		assertRowCount(t, store.db, "outbox", 0)
		assertRowCount(t, store.db, "outbox_read_receipts", 0)
		assertRowCount(t, store.db, "read_cursors", 0)
	})
}

func TestOutboxValidatesReactionAndReadCarrierPairing(t *testing.T) {
	clock := newOutboxTestClock(outboxTestTimeMS)
	store, repository := openOutboxTestRepository(t, clock.Now)
	seedMessageConversation(t, store, "conversation-a", "account-a")
	seedOutboxTestDevice(t, store, "device-a", "account-a")
	seedOutboxTestMessage(t, store, "target-a", "account-a", "conversation-a")
	ctx := context.Background()
	targetID := "target-a"
	reaction := OutboxReaction{TargetMessageID: targetID, Emoji: "👍", Action: "add"}
	receipt := OutboxReadReceipt{DeviceID: "device-a", LastReadMessageID: targetID, ReadAtMS: outboxTestTimeMS}
	cursor := ReadCursor{
		AccountID: "account-a", DeviceID: "device-a", ConversationID: "conversation-a",
		LastReadMessageID: &targetID, LastReadAtMS: outboxTestTimeMS, UpdatedAtMS: outboxTestTimeMS,
	}

	tests := []struct {
		name    string
		enqueue func() error
	}{
		{
			name: "reaction without reaction carrier",
			enqueue: func() error {
				_, _, err := repository.Enqueue(ctx, outboxTestReactionItem("missing-reaction"))
				return err
			},
		},
		{
			name: "text with reaction carrier",
			enqueue: func() error {
				_, _, err := repository.EnqueueReaction(ctx, outboxTestItem("text-reaction"), reaction)
				return err
			},
		},
		{
			name: "read without receipt carrier",
			enqueue: func() error {
				_, _, err := repository.Enqueue(ctx, outboxTestReadItem("missing-receipt"))
				return err
			},
		},
		{
			name: "text with receipt carrier",
			enqueue: func() error {
				_, _, err := repository.EnqueueReadReceipt(ctx, outboxTestItem("text-receipt"), receipt, cursor)
				return err
			},
		},
		{
			name: "reaction with attachment carrier",
			enqueue: func() error {
				item := outboxTestReactionItem("reaction-attachment")
				_, _, err := repository.enqueue(ctx, item, nil, enqueueCarriers{
					attachment: &OutboxAttachment{
						BlobHash: strings.Repeat("a", 64), SizeBytes: 1, MIME: "image/png",
					},
					reaction: &reaction,
				})
				return err
			},
		},
		{
			name: "read with reaction carrier",
			enqueue: func() error {
				item := outboxTestReadItem("read-reaction")
				_, _, err := repository.enqueue(ctx, item, nil, enqueueCarriers{
					reaction:    &reaction,
					readReceipt: &receipt,
					readCursor:  &cursor,
				})
				return err
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.enqueue(); err == nil {
				t.Fatal("enqueue succeeded, want carrier-pairing error")
			}
		})
	}
	assertRowCount(t, store.db, "outbox", 0)
}

func TestOutboxReactionAndReadReceiptForeignKeys(t *testing.T) {
	tests := []struct {
		name    string
		enqueue func(*testing.T, *Store, *OutboxRepository) error
	}{
		{
			name: "unknown reaction target",
			enqueue: func(t *testing.T, _ *Store, repository *OutboxRepository) error {
				_, _, err := repository.EnqueueReaction(
					context.Background(), outboxTestReactionItem("unknown-target"),
					OutboxReaction{TargetMessageID: "missing", Emoji: "👍", Action: "add"},
				)
				return err
			},
		},
		{
			name: "unknown read target",
			enqueue: func(t *testing.T, _ *Store, repository *OutboxRepository) error {
				targetID := "missing"
				_, _, err := repository.EnqueueReadReceipt(
					context.Background(), outboxTestReadItem("unknown-read-target"),
					OutboxReadReceipt{DeviceID: "device-a", LastReadMessageID: targetID, ReadAtMS: outboxTestTimeMS},
					ReadCursor{
						AccountID: "account-a", DeviceID: "device-a", ConversationID: "conversation-a",
						LastReadMessageID: &targetID, LastReadAtMS: outboxTestTimeMS, UpdatedAtMS: outboxTestTimeMS,
					},
				)
				return err
			},
		},
		{
			name: "unknown device",
			enqueue: func(t *testing.T, _ *Store, repository *OutboxRepository) error {
				targetID := "target-a"
				_, _, err := repository.EnqueueReadReceipt(
					context.Background(), outboxTestReadItem("unknown-device"),
					OutboxReadReceipt{DeviceID: "missing", LastReadMessageID: targetID, ReadAtMS: outboxTestTimeMS},
					ReadCursor{
						AccountID: "account-a", DeviceID: "missing", ConversationID: "conversation-a",
						LastReadMessageID: &targetID, LastReadAtMS: outboxTestTimeMS, UpdatedAtMS: outboxTestTimeMS,
					},
				)
				return err
			},
		},
		{
			name: "cross-conversation cursor target",
			enqueue: func(t *testing.T, store *Store, repository *OutboxRepository) error {
				seedMessageConversation(t, store, "conversation-other", "account-a")
				seedOutboxTestMessage(t, store, "target-other", "account-a", "conversation-other")
				targetID := "target-other"
				_, _, err := repository.EnqueueReadReceipt(
					context.Background(), outboxTestReadItem("cross-conversation"),
					OutboxReadReceipt{DeviceID: "device-a", LastReadMessageID: targetID, ReadAtMS: outboxTestTimeMS},
					ReadCursor{
						AccountID: "account-a", DeviceID: "device-a", ConversationID: "conversation-a",
						LastReadMessageID: &targetID, LastReadAtMS: outboxTestTimeMS, UpdatedAtMS: outboxTestTimeMS,
					},
				)
				return err
			},
		},
		{
			name: "cross-account device",
			enqueue: func(t *testing.T, store *Store, repository *OutboxRepository) error {
				seedMessageAccount(t, store, "account-b", "test-b")
				seedOutboxTestDevice(t, store, "device-b", "account-b")
				targetID := "target-a"
				_, _, err := repository.EnqueueReadReceipt(
					context.Background(), outboxTestReadItem("cross-account"),
					OutboxReadReceipt{DeviceID: "device-b", LastReadMessageID: targetID, ReadAtMS: outboxTestTimeMS},
					ReadCursor{
						AccountID: "account-a", DeviceID: "device-b", ConversationID: "conversation-a",
						LastReadMessageID: &targetID, LastReadAtMS: outboxTestTimeMS, UpdatedAtMS: outboxTestTimeMS,
					},
				)
				return err
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clock := newOutboxTestClock(outboxTestTimeMS)
			store, repository := openOutboxTestRepository(t, clock.Now)
			seedMessageConversation(t, store, "conversation-a", "account-a")
			seedOutboxTestDevice(t, store, "device-a", "account-a")
			seedOutboxTestMessage(t, store, "target-a", "account-a", "conversation-a")
			if err := test.enqueue(t, store, repository); !errors.Is(err, ErrConstraintViolation) {
				t.Fatalf("enqueue error = %v, want ErrConstraintViolation", err)
			}
			assertRowCount(t, store.db, "outbox", 0)
			assertRowCount(t, store.db, "outbox_reactions", 0)
			assertRowCount(t, store.db, "outbox_read_receipts", 0)
		})
	}
}

func TestOutboxConfirmWithoutResultRequiresCalledActiveLease(t *testing.T) {
	clock := newOutboxTestClock(outboxTestTimeMS)
	_, repository := openOutboxTestRepository(t, clock.Now)
	ctx := context.Background()
	item := mustEnqueueOutbox(t, repository, outboxTestItem("confirm-without-result"))
	lease := mustLeaseOne(t, repository, LeaseRequest{
		Owner: "worker", Now: clock.Now(), Duration: time.Minute, Limit: 1,
	})
	token := mustLeaseToken(t, lease)

	if err := repository.ConfirmWithoutResult(ctx, item.OutboxID, token); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("ConfirmWithoutResult(pre-call) error = %v, want ErrLeaseLost", err)
	}
	if err := repository.MarkTransportCalled(ctx, Attempt{OutboxID: item.OutboxID, LeaseToken: token}); err != nil {
		t.Fatalf("MarkTransportCalled(): %v", err)
	}
	if err := repository.Confirm(ctx, Confirmation{OutboxID: item.OutboxID, LeaseToken: token}); err == nil {
		t.Fatal("Confirm(empty result ID) succeeded")
	}
	if err := repository.ConfirmWithoutResult(ctx, item.OutboxID, "stale-token"); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("ConfirmWithoutResult(stale token) error = %v, want ErrLeaseLost", err)
	}
	if err := repository.ConfirmWithoutResult(ctx, item.OutboxID, token); err != nil {
		t.Fatalf("ConfirmWithoutResult(): %v", err)
	}
	confirmed, err := repository.FindByID(ctx, item.OutboxID)
	if err != nil {
		t.Fatalf("FindByID(): %v", err)
	}
	if confirmed.State != OutboxConfirmed || confirmed.ResultRemoteID != nil ||
		confirmed.LeaseOwner != nil || confirmed.LeaseToken != nil ||
		confirmed.LeaseExpiresAtMS != nil || confirmed.TransportCalledAtMS != nil ||
		confirmed.ErrorClass != nil || confirmed.ErrorCode != nil || confirmed.ErrorDetail != nil {
		t.Fatalf("confirmed without-result row = %+v", confirmed)
	}
}

func TestOutboxReconcileConfirmMergesCollisionAndPreservesLocalAnchor(t *testing.T) {
	clock := newOutboxTestClock(outboxTestTimeMS)
	store, repository := openOutboxTestRepository(t, clock.Now)
	seedMessageIdentity(t, store, "identity-a", "account-a")
	seedMessageConversation(t, store, "conversation-a", "account-a")
	ctx := context.Background()

	item := outboxTestItem("reconcile-collision")
	mustEnqueueOutgoingOutbox(t, repository, item, "optimistic body")
	lease := mustLeaseOne(t, repository, LeaseRequest{
		Owner: "worker", Now: clock.Now(), Duration: time.Minute, Limit: 1,
	})
	token := mustLeaseToken(t, lease)
	if err := repository.MarkTransportCalled(ctx, Attempt{
		OutboxID: item.OutboxID, LeaseToken: token,
	}); err != nil {
		t.Fatalf("MarkTransportCalled(): %v", err)
	}
	if err := repository.MarkUncertain(
		ctx,
		item.OutboxID,
		token,
		"timeout",
		"deadline",
		"outcome unknown",
	); err != nil {
		t.Fatalf("MarkUncertain(): %v", err)
	}

	const echoMessageID = "message-echo-duplicate"
	seedOutboxTestMessage(t, store, echoMessageID, item.AccountID, item.ConversationID)
	realID := "remote-" + echoMessageID

	dependent := outboxTestReactionItem("reconcile-local-anchor")
	if _, disposition, err := repository.EnqueueReaction(ctx, dependent, OutboxReaction{
		TargetMessageID: item.LocalMessageID,
		Emoji:           "thumbs-up",
		Action:          "add",
	}); err != nil {
		t.Fatalf("EnqueueReaction(dependent): %v", err)
	} else if disposition != EnqueueInserted {
		t.Fatalf("EnqueueReaction(dependent) disposition = %q, want %q", disposition, EnqueueInserted)
	}

	clock.Set(outboxTestTimeMS + 1_000)
	outcome, err := repository.ReconcileConfirm(ctx, ReconcileRequest{
		AccountID:          item.AccountID,
		TransportRequestID: item.TransportRequestID,
		ResultRemoteID:     realID,
	})
	if err != nil {
		t.Fatalf("ReconcileConfirm(): %v", err)
	}
	if outcome != ReconcileOutcome("reconciled") {
		t.Fatalf("ReconcileConfirm() outcome = %q, want reconciled", outcome)
	}

	reconciled, err := repository.FindByID(ctx, item.OutboxID)
	if err != nil {
		t.Fatalf("FindByID(reconciled): %v", err)
	}
	if reconciled.State != OutboxConfirmed || reconciled.ResultRemoteID == nil ||
		*reconciled.ResultRemoteID != realID || reconciled.ErrorClass != nil ||
		reconciled.ErrorCode != nil || reconciled.ErrorDetail != nil {
		t.Fatalf("reconciled row = %+v", reconciled)
	}

	messages, err := NewMessageRepository(store, clock.Now)
	if err != nil {
		t.Fatalf("NewMessageRepository(): %v", err)
	}
	survivor, err := messages.GetMessage(ctx, item.LocalMessageID)
	if err != nil {
		t.Fatalf("GetMessage(local survivor): %v", err)
	}
	if survivor.RemoteMessageID != realID {
		t.Fatalf("local survivor remote ID = %q, want %q", survivor.RemoteMessageID, realID)
	}
	if _, err := messages.GetMessage(ctx, echoMessageID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetMessage(echo duplicate) error = %v, want ErrNotFound", err)
	}
	carrier, err := repository.GetOutboxReaction(ctx, dependent.OutboxID)
	if err != nil {
		t.Fatalf("GetOutboxReaction(dependent): %v", err)
	}
	if carrier.TargetMessageID != item.LocalMessageID {
		t.Fatalf("dependent target = %q, want local survivor %q", carrier.TargetMessageID, item.LocalMessageID)
	}
	var survivorID string
	if err := store.db.QueryRowContext(ctx, `
		SELECT message_id
		FROM messages
		WHERE account_id = ? AND conversation_id = ? AND remote_message_id = ?
	`, item.AccountID, item.ConversationID, realID).Scan(&survivorID); err != nil {
		t.Fatalf("read survivor by remote ID: %v", err)
	}
	if survivorID != item.LocalMessageID {
		t.Fatalf("remote-ID survivor = %q, want %q", survivorID, item.LocalMessageID)
	}

	duplicate, err := repository.ReconcileConfirm(ctx, ReconcileRequest{
		AccountID:          item.AccountID,
		TransportRequestID: item.TransportRequestID,
		ResultRemoteID:     realID,
	})
	if err != nil {
		t.Fatalf("ReconcileConfirm(duplicate): %v", err)
	}
	if duplicate != ReconcileOutcome("noop") {
		t.Fatalf("ReconcileConfirm(duplicate) outcome = %q, want noop", duplicate)
	}
	foreign, err := repository.ReconcileConfirm(ctx, ReconcileRequest{
		AccountID:          item.AccountID,
		TransportRequestID: item.TransportRequestID,
		ResultRemoteID:     "remote-foreign",
	})
	if err != nil {
		t.Fatalf("ReconcileConfirm(foreign): %v", err)
	}
	if foreign != ReconcileOutcome("noop") {
		t.Fatalf("ReconcileConfirm(foreign) outcome = %q, want noop", foreign)
	}
	unchanged, err := messages.GetMessage(ctx, item.LocalMessageID)
	if err != nil {
		t.Fatalf("GetMessage(after foreign echo): %v", err)
	}
	if unchanged.RemoteMessageID != realID {
		t.Fatalf("message remote ID after foreign echo = %q, want %q", unchanged.RemoteMessageID, realID)
	}
}

func TestOutboxReconcileConfirmEnrichesPlaceholderAndLeavesDispatchOwnerInCharge(t *testing.T) {
	clock := newOutboxTestClock(outboxTestTimeMS)
	store, repository := openOutboxTestRepository(t, clock.Now)
	seedMessageIdentity(t, store, "identity-a", "account-a")
	seedMessageConversation(t, store, "conversation-a", "account-a")
	ctx := context.Background()

	provisional := outboxTestItem("reconcile-enrich")
	mustEnqueueOutgoingOutbox(t, repository, provisional, "provisional body")
	lease := mustLeaseOne(t, repository, LeaseRequest{
		Owner: "worker", Now: clock.Now(), Duration: time.Minute, Limit: 1,
	})
	token := mustLeaseToken(t, lease)
	if err := repository.MarkTransportCalled(ctx, Attempt{
		OutboxID: provisional.OutboxID, LeaseToken: token,
	}); err != nil {
		t.Fatalf("MarkTransportCalled(provisional): %v", err)
	}
	if err := repository.Confirm(ctx, Confirmation{
		OutboxID:       provisional.OutboxID,
		LeaseToken:     token,
		ResultRemoteID: provisional.TransportRequestID,
	}); err != nil {
		t.Fatalf("Confirm(provisional): %v", err)
	}

	const permanentID = "remote-permanent"
	outcome, err := repository.ReconcileConfirm(ctx, ReconcileRequest{
		AccountID:          provisional.AccountID,
		TransportRequestID: provisional.TransportRequestID,
		ResultRemoteID:     permanentID,
	})
	if err != nil {
		t.Fatalf("ReconcileConfirm(enrich): %v", err)
	}
	if outcome != ReconcileOutcome("enriched") {
		t.Fatalf("ReconcileConfirm(enrich) outcome = %q, want enriched", outcome)
	}
	confirmed, err := repository.FindByID(ctx, provisional.OutboxID)
	if err != nil {
		t.Fatalf("FindByID(enriched): %v", err)
	}
	if confirmed.State != OutboxConfirmed || confirmed.ResultRemoteID == nil ||
		*confirmed.ResultRemoteID != permanentID {
		t.Fatalf("enriched row = %+v", confirmed)
	}
	messages, err := NewMessageRepository(store, clock.Now)
	if err != nil {
		t.Fatalf("NewMessageRepository(): %v", err)
	}
	enrichedMessage, err := messages.GetMessage(ctx, provisional.LocalMessageID)
	if err != nil {
		t.Fatalf("GetMessage(enriched): %v", err)
	}
	if enrichedMessage.RemoteMessageID != permanentID {
		t.Fatalf("enriched message remote ID = %q, want %q", enrichedMessage.RemoteMessageID, permanentID)
	}

	racing := outboxTestItem("reconcile-dispatching")
	mustEnqueueOutgoingOutbox(t, repository, racing, "racing body")
	lease = mustLeaseOne(t, repository, LeaseRequest{
		Owner: "lease-owner", Now: clock.Now(), Duration: time.Minute, Limit: 1,
	})
	token = mustLeaseToken(t, lease)
	if err := repository.MarkTransportCalled(ctx, Attempt{
		OutboxID: racing.OutboxID, LeaseToken: token,
	}); err != nil {
		t.Fatalf("MarkTransportCalled(racing): %v", err)
	}
	racedOutcome, err := repository.ReconcileConfirm(ctx, ReconcileRequest{
		AccountID:          racing.AccountID,
		TransportRequestID: racing.TransportRequestID,
		ResultRemoteID:     "remote-racing",
	})
	if err != nil {
		t.Fatalf("ReconcileConfirm(dispatching): %v", err)
	}
	if racedOutcome != ReconcileOutcome("noop") {
		t.Fatalf("ReconcileConfirm(dispatching) outcome = %q, want noop", racedOutcome)
	}
	stillDispatching, err := repository.FindByID(ctx, racing.OutboxID)
	if err != nil {
		t.Fatalf("FindByID(dispatching): %v", err)
	}
	if stillDispatching.State != OutboxDispatching || stillDispatching.LeaseToken == nil ||
		*stillDispatching.LeaseToken != token {
		t.Fatalf("dispatching row changed during echo = %+v", stillDispatching)
	}
	if err := repository.Confirm(ctx, Confirmation{
		OutboxID: racing.OutboxID, LeaseToken: token, ResultRemoteID: "remote-racing",
	}); err != nil {
		t.Fatalf("Confirm(lease owner): %v", err)
	}
	replayed, err := repository.ReconcileConfirm(ctx, ReconcileRequest{
		AccountID:          racing.AccountID,
		TransportRequestID: racing.TransportRequestID,
		ResultRemoteID:     "remote-racing",
	})
	if err != nil {
		t.Fatalf("ReconcileConfirm(after lease confirm): %v", err)
	}
	if replayed != ReconcileOutcome("noop") {
		t.Fatalf("ReconcileConfirm(after lease confirm) outcome = %q, want noop", replayed)
	}

	missing, err := repository.ReconcileConfirm(ctx, ReconcileRequest{
		AccountID:          provisional.AccountID,
		TransportRequestID: "request-missing",
		ResultRemoteID:     "remote-missing",
	})
	if err != nil {
		t.Fatalf("ReconcileConfirm(missing): %v", err)
	}
	if missing != ReconcileOutcome("notfound") {
		t.Fatalf("ReconcileConfirm(missing) outcome = %q, want notfound", missing)
	}
}

func TestOutboxReconcileConfirmRepairsStoreFailedAndNoopsTerminalStates(t *testing.T) {
	clock := newOutboxTestClock(outboxTestTimeMS)
	store, repository := openOutboxTestRepository(t, clock.Now)
	seedMessageIdentity(t, store, "identity-a", "account-a")
	seedMessageConversation(t, store, "conversation-a", "account-a")
	ctx := context.Background()

	failed := outboxTestItem("reconcile-store-failed")
	mustEnqueueOutgoingOutbox(t, repository, failed, "store-failed body")
	lease := mustLeaseOne(t, repository, LeaseRequest{
		Owner: "worker", Now: clock.Now(), Duration: time.Minute, Limit: 1,
	})
	token := mustLeaseToken(t, lease)
	if err := repository.MarkTransportCalled(ctx, Attempt{
		OutboxID: failed.OutboxID, LeaseToken: token,
	}); err != nil {
		t.Fatalf("MarkTransportCalled(store failed): %v", err)
	}
	const resultID = "remote-store-failed"
	if err := repository.MarkStoreFailed(ctx, failed.OutboxID, token, resultID, "local write failed"); err != nil {
		t.Fatalf("MarkStoreFailed(): %v", err)
	}
	outcome, err := repository.ReconcileConfirm(ctx, ReconcileRequest{
		AccountID:          failed.AccountID,
		TransportRequestID: failed.TransportRequestID,
		ResultRemoteID:     resultID,
	})
	if err != nil {
		t.Fatalf("ReconcileConfirm(store failed): %v", err)
	}
	if outcome != ReconcileOutcome("reconciled") {
		t.Fatalf("ReconcileConfirm(store failed) outcome = %q, want reconciled", outcome)
	}
	repaired, err := repository.FindByID(ctx, failed.OutboxID)
	if err != nil {
		t.Fatalf("FindByID(repaired): %v", err)
	}
	if repaired.State != OutboxConfirmed || repaired.ResultRemoteID == nil ||
		*repaired.ResultRemoteID != resultID || repaired.ErrorClass != nil ||
		repaired.ErrorCode != nil || repaired.ErrorDetail != nil {
		t.Fatalf("repaired row = %+v", repaired)
	}
	messages, err := NewMessageRepository(store, clock.Now)
	if err != nil {
		t.Fatalf("NewMessageRepository(): %v", err)
	}
	repairedMessage, err := messages.GetMessage(ctx, failed.LocalMessageID)
	if err != nil {
		t.Fatalf("GetMessage(repaired): %v", err)
	}
	if repairedMessage.RemoteMessageID != resultID {
		t.Fatalf("repaired message remote ID = %q, want %q", repairedMessage.RemoteMessageID, resultID)
	}

	terminalCases := []struct {
		name       string
		transition func(OutboxItem) error
		wantState  OutboxState
	}{
		{
			name: "queued",
			transition: func(OutboxItem) error {
				return nil
			},
			wantState: OutboxQueued,
		},
		{
			name: "canceled",
			transition: func(item OutboxItem) error {
				return repository.Cancel(ctx, item.OutboxID)
			},
			wantState: OutboxCanceled,
		},
	}
	for _, test := range terminalCases {
		t.Run(test.name, func(t *testing.T) {
			item := outboxTestItem("reconcile-noop-" + test.name)
			mustEnqueueOutgoingOutbox(t, repository, item, test.name+" body")
			current, err := repository.FindByID(ctx, item.OutboxID)
			if err != nil {
				t.Fatalf("FindByID(before transition): %v", err)
			}
			if err := test.transition(current); err != nil {
				t.Fatalf("transition: %v", err)
			}
			got, err := repository.ReconcileConfirm(ctx, ReconcileRequest{
				AccountID:          item.AccountID,
				TransportRequestID: item.TransportRequestID,
				ResultRemoteID:     "remote-anomalous-" + test.name,
			})
			if err != nil {
				t.Fatalf("ReconcileConfirm(): %v", err)
			}
			if got != ReconcileOutcome("noop") {
				t.Fatalf("ReconcileConfirm() outcome = %q, want noop", got)
			}
			unchanged, err := repository.FindByID(ctx, item.OutboxID)
			if err != nil {
				t.Fatalf("FindByID(after echo): %v", err)
			}
			if unchanged.State != test.wantState {
				t.Fatalf("state after echo = %q, want %q", unchanged.State, test.wantState)
			}
			message, err := messages.GetMessage(ctx, item.LocalMessageID)
			if err != nil {
				t.Fatalf("GetMessage(after echo): %v", err)
			}
			if message.RemoteMessageID != item.TransportRequestID {
				t.Fatalf("message remote ID after echo = %q, want %q", message.RemoteMessageID, item.TransportRequestID)
			}
		})
	}
}

func TestOutboxFindStoreFailedDueIsBoundedOrderedAndStateFiltered(t *testing.T) {
	clock := newOutboxTestClock(outboxTestTimeMS)
	_, repository := openOutboxTestRepository(t, clock.Now)
	ctx := context.Background()

	items := []NewOutboxItem{
		outboxTestItem("store-a"),
		outboxTestItem("store-b"),
		outboxTestItem("store-c"),
	}
	for _, item := range items {
		mustEnqueueOutbox(t, repository, item)
	}
	leases, err := repository.LeaseDue(ctx, LeaseRequest{
		Owner: "worker", Now: clock.Now(), Duration: time.Minute, Limit: len(items),
	})
	if err != nil {
		t.Fatalf("LeaseDue(store failed candidates): %v", err)
	}
	if len(leases) != len(items) {
		t.Fatalf("LeaseDue(store failed candidates) = %d leases, want %d", len(leases), len(items))
	}
	leasesByID := make(map[string]OutboxItem, len(leases))
	for _, lease := range leases {
		leasesByID[lease.OutboxID] = lease.OutboxItem
		if err := repository.MarkTransportCalled(ctx, Attempt{
			OutboxID: lease.OutboxID, LeaseToken: mustLeaseToken(t, lease.OutboxItem),
		}); err != nil {
			t.Fatalf("MarkTransportCalled(%q): %v", lease.OutboxID, err)
		}
	}

	clock.Set(outboxTestTimeMS + 100)
	for _, id := range []string{"outbox-store-c", "outbox-store-a"} {
		lease := leasesByID[id]
		if err := repository.MarkStoreFailed(
			ctx,
			id,
			mustLeaseToken(t, lease),
			"remote-"+id,
			"store failed",
		); err != nil {
			t.Fatalf("MarkStoreFailed(%q): %v", id, err)
		}
	}
	clock.Set(outboxTestTimeMS + 200)
	lease := leasesByID["outbox-store-b"]
	if err := repository.MarkStoreFailed(
		ctx,
		lease.OutboxID,
		mustLeaseToken(t, lease),
		"remote-"+lease.OutboxID,
		"store failed",
	); err != nil {
		t.Fatalf("MarkStoreFailed(%q): %v", lease.OutboxID, err)
	}

	confirmedInput := outboxTestItem("store-filter-confirmed")
	mustEnqueueOutbox(t, repository, confirmedInput)
	confirmedLease := mustLeaseOne(t, repository, LeaseRequest{
		Owner: "worker", Now: clock.Now(), Duration: time.Minute, Limit: 1,
	})
	confirmedToken := mustLeaseToken(t, confirmedLease)
	if err := repository.MarkTransportCalled(ctx, Attempt{
		OutboxID: confirmedInput.OutboxID, LeaseToken: confirmedToken,
	}); err != nil {
		t.Fatalf("MarkTransportCalled(confirmed filter): %v", err)
	}
	if err := repository.Confirm(ctx, Confirmation{
		OutboxID:       confirmedInput.OutboxID,
		LeaseToken:     confirmedToken,
		ResultRemoteID: confirmedInput.TransportRequestID,
	}); err != nil {
		t.Fatalf("Confirm(filter): %v", err)
	}
	mustEnqueueOutbox(t, repository, outboxTestItem("store-filter-queued"))

	bounded, err := repository.FindStoreFailedDue(ctx, 2)
	if err != nil {
		t.Fatalf("FindStoreFailedDue(2): %v", err)
	}
	wantBounded := []string{"outbox-store-a", "outbox-store-c"}
	if got := outboxIDs(bounded); !slices.Equal(got, wantBounded) {
		t.Fatalf("FindStoreFailedDue(2) IDs = %v, want %v", got, wantBounded)
	}

	all, err := repository.FindStoreFailedDue(ctx, 10)
	if err != nil {
		t.Fatalf("FindStoreFailedDue(10): %v", err)
	}
	wantAll := []string{"outbox-store-a", "outbox-store-c", "outbox-store-b"}
	if got := outboxIDs(all); !slices.Equal(got, wantAll) {
		t.Fatalf("FindStoreFailedDue(10) IDs = %v, want %v", got, wantAll)
	}
	for _, item := range all {
		if item.State != OutboxStoreFailed {
			t.Fatalf("FindStoreFailedDue() returned state %q for %q", item.State, item.OutboxID)
		}
	}
	if _, err := repository.FindStoreFailedDue(ctx, 0); err == nil {
		t.Fatal("FindStoreFailedDue(0) succeeded")
	}
}

func TestOutboxLeaseDueNeverClaimsArtificiallyArmedUncertainRow(t *testing.T) {
	clock := newOutboxTestClock(outboxTestTimeMS)
	store, repository := openOutboxTestRepository(t, clock.Now)
	ctx := context.Background()
	item := mustEnqueueOutbox(t, repository, outboxTestItem("armed-uncertain"))
	lease := mustLeaseOne(t, repository, LeaseRequest{
		Owner: "worker", Now: clock.Now(), Duration: time.Minute, Limit: 1,
	})
	token := mustLeaseToken(t, lease)
	if err := repository.MarkTransportCalled(ctx, Attempt{
		OutboxID: item.OutboxID, LeaseToken: token,
	}); err != nil {
		t.Fatalf("MarkTransportCalled(): %v", err)
	}
	if err := repository.MarkUncertain(ctx, item.OutboxID, token, "timeout", "deadline", "unknown"); err != nil {
		t.Fatalf("MarkUncertain(): %v", err)
	}

	artificialNextAttemptMS := clock.Now().Add(time.Second).UnixMilli()
	if _, err := store.db.ExecContext(ctx, `
		UPDATE outbox
		SET next_attempt_at_ms = ?
		WHERE outbox_id = ? AND state = 'uncertain'
	`, artificialNextAttemptMS, item.OutboxID); err != nil {
		t.Fatalf("artificially arm uncertain row: %v", err)
	}
	armed, err := repository.FindByID(ctx, item.OutboxID)
	if err != nil {
		t.Fatalf("FindByID(armed uncertain): %v", err)
	}
	if armed.State != OutboxUncertain || armed.NextAttemptAtMS == nil ||
		*armed.NextAttemptAtMS != artificialNextAttemptMS {
		t.Fatalf("artificially armed row = %+v", armed)
	}

	leases, err := repository.LeaseDue(ctx, LeaseRequest{
		Owner: "worker-later", Now: clock.Now().Add(time.Hour), Duration: time.Minute, Limit: 1,
	})
	if err != nil {
		t.Fatalf("LeaseDue(artificially armed uncertain): %v", err)
	}
	if len(leases) != 0 {
		t.Fatalf("LeaseDue(artificially armed uncertain) = %+v, want none", leases)
	}
	unchanged, err := repository.FindByID(ctx, item.OutboxID)
	if err != nil {
		t.Fatalf("FindByID(after LeaseDue): %v", err)
	}
	if unchanged.State != OutboxUncertain {
		t.Fatalf("state after LeaseDue = %q, want %q", unchanged.State, OutboxUncertain)
	}
}

func TestOutboxLeaseDueRespectsScheduleRetryAndLiveLease(t *testing.T) {
	clock := newOutboxTestClock(outboxTestTimeMS)
	_, repository := openOutboxTestRepository(t, clock.Now)
	ctx := context.Background()
	now := clock.Now()

	live := outboxTestItem("live")
	live.ScheduledFor = now.Add(-time.Second)
	mustEnqueueOutbox(t, repository, live)
	liveLease := mustLeaseOne(t, repository, LeaseRequest{
		Owner: "worker-live", Now: now, Duration: 10 * time.Second, Limit: 1,
	})

	retry := outboxTestItem("retry")
	mustEnqueueOutbox(t, repository, retry)
	retryLease := mustLeaseOne(t, repository, LeaseRequest{
		Owner: "worker-retry", Now: now, Duration: 10 * time.Second, Limit: 1,
	})
	if err := repository.MarkNotDispatched(
		ctx,
		retry.OutboxID,
		mustLeaseToken(t, retryLease),
		"offline",
		"not_connected",
		"bridge is offline",
		now.Add(5*time.Second),
	); err != nil {
		t.Fatalf("MarkNotDispatched(): %v", err)
	}

	due := outboxTestItem("due")
	mustEnqueueOutbox(t, repository, due)
	future := outboxTestItem("future")
	future.ScheduledFor = now.Add(10 * time.Second)
	mustEnqueueOutbox(t, repository, future)

	leases, err := repository.LeaseDue(ctx, LeaseRequest{
		Owner: "worker-due", Now: now, Duration: 3 * time.Second, Limit: 10,
	})
	if err != nil {
		t.Fatalf("LeaseDue(): %v", err)
	}
	if len(leases) != 1 || leases[0].OutboxID != due.OutboxID {
		t.Fatalf("LeaseDue() leases = %+v, want only %q", leases, due.OutboxID)
	}
	lease := leases[0]
	if lease.LeaseOwner == nil || *lease.LeaseOwner != "worker-due" ||
		lease.LeaseToken == nil || *lease.LeaseToken == "" ||
		lease.LeaseExpiresAtMS == nil || *lease.LeaseExpiresAtMS != now.Add(3*time.Second).UnixMilli() {
		t.Fatalf("claimed lease fields = %+v", lease.OutboxItem)
	}

	again, err := repository.LeaseDue(ctx, LeaseRequest{
		Owner: "worker-other", Now: now, Duration: time.Second, Limit: 10,
	})
	if err != nil {
		t.Fatalf("LeaseDue(again): %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("LeaseDue(again) = %+v, want no live/scheduled/retry leases", again)
	}
	if got, err := repository.FindByID(ctx, live.OutboxID); err != nil {
		t.Fatalf("FindByID(live): %v", err)
	} else if got.State != OutboxDispatching || got.LeaseToken == nil || *got.LeaseToken != mustLeaseToken(t, liveLease) {
		t.Fatalf("live row changed unexpectedly: %+v", got)
	}
}

func TestOutboxEarliestDueReturnsMinimumLeaseOrderingKey(t *testing.T) {
	clock := newOutboxTestClock(outboxTestTimeMS)
	_, repository := openOutboxTestRepository(t, clock.Now)
	ctx := context.Background()
	now := clock.Now()

	earliest, ok, err := repository.EarliestDue(ctx)
	if err != nil {
		t.Fatalf("EarliestDue(empty): %v", err)
	}
	if ok || !earliest.IsZero() {
		t.Fatalf("EarliestDue(empty) = (%v, %t), want (zero, false)", earliest, ok)
	}

	futureAt := now.Add(10 * time.Second)
	future := outboxTestItem("earliest-future")
	future.ScheduledFor = futureAt
	mustEnqueueOutbox(t, repository, future)
	earliest, ok, err = repository.EarliestDue(ctx)
	if err != nil {
		t.Fatalf("EarliestDue(only future): %v", err)
	}
	if !ok || !earliest.Equal(futureAt) {
		t.Fatalf("EarliestDue(only future) = (%v, %t), want (%v, true)", earliest, ok, futureAt)
	}

	retry := outboxTestItem("earliest-retry")
	mustEnqueueOutbox(t, repository, retry)
	retryLease := mustLeaseOne(t, repository, LeaseRequest{
		Owner: "worker-retry", Now: now, Duration: time.Minute, Limit: 1,
	})
	retryAt := now.Add(5 * time.Second)
	if err := repository.MarkNotDispatched(
		ctx,
		retry.OutboxID,
		mustLeaseToken(t, retryLease),
		"transient",
		"offline",
		"retry later",
		retryAt,
	); err != nil {
		t.Fatalf("MarkNotDispatched(): %v", err)
	}
	earliest, ok, err = repository.EarliestDue(ctx)
	if err != nil {
		t.Fatalf("EarliestDue(retry and future): %v", err)
	}
	if !ok || !earliest.Equal(retryAt) {
		t.Fatalf("EarliestDue(retry and future) = (%v, %t), want (%v, true)", earliest, ok, retryAt)
	}

	dueAt := now.Add(-time.Second)
	due := outboxTestItem("earliest-due")
	due.ScheduledFor = dueAt
	mustEnqueueOutbox(t, repository, due)
	earliest, ok, err = repository.EarliestDue(ctx)
	if err != nil {
		t.Fatalf("EarliestDue(due, retry, and future): %v", err)
	}
	if !ok || !earliest.Equal(dueAt) {
		t.Fatalf("EarliestDue(due, retry, and future) = (%v, %t), want (%v, true)", earliest, ok, dueAt)
	}
}

func TestOutboxListPendingReturnsTrayRowsDueOrderedWithSummarySources(t *testing.T) {
	clock := newOutboxTestClock(outboxTestTimeMS)
	store, repository := openOutboxTestRepository(t, clock.Now)
	ctx := context.Background()
	now := clock.Now()

	seedMessageIdentity(t, store, "identity-a", "account-a")
	seedMessageConversation(t, store, "conversation-a", "account-a")
	seedMessageConversation(t, store, "conversation-b", "account-a")
	seedMessageAccount(t, store, "account-b", "test-b")
	seedMessageConversation(t, store, "conversation-c", "account-b")
	seedOutboxTestMessage(t, store, "target-reaction", "account-b", "conversation-c")
	seedOutboxTestDevice(t, store, "device-a", "account-a")
	seedOutboxTestMessage(t, store, "target-read", "account-a", "conversation-a")

	confirmed := outboxTestItem("list-confirmed")
	mustEnqueueOutbox(t, repository, confirmed)
	confirmedLease := mustLeaseOne(t, repository, LeaseRequest{
		Owner: "worker-confirmed", Now: now, Duration: time.Minute, Limit: 1,
	})
	confirmedToken := mustLeaseToken(t, confirmedLease)
	if err := repository.MarkTransportCalled(ctx, Attempt{
		OutboxID: confirmed.OutboxID, LeaseToken: confirmedToken,
	}); err != nil {
		t.Fatalf("MarkTransportCalled(confirmed): %v", err)
	}
	if err := repository.ConfirmWithoutResult(ctx, confirmed.OutboxID, confirmedToken); err != nil {
		t.Fatalf("ConfirmWithoutResult(): %v", err)
	}

	canceled := outboxTestItem("list-canceled")
	mustEnqueueOutbox(t, repository, canceled)
	if err := repository.Cancel(ctx, canceled.OutboxID); err != nil {
		t.Fatalf("Cancel(): %v", err)
	}

	rejected := outboxTestItem("list-rejected")
	mustEnqueueOutbox(t, repository, rejected)
	rejectedLease := mustLeaseOne(t, repository, LeaseRequest{
		Owner: "worker-rejected", Now: now, Duration: time.Minute, Limit: 1,
	})
	if err := repository.Reject(
		ctx,
		rejected.OutboxID,
		mustLeaseToken(t, rejectedLease),
		"permanent",
		"invalid",
		"rejected",
	); err != nil {
		t.Fatalf("Reject(): %v", err)
	}

	dispatching := outboxTestItem("list-dispatching")
	dispatching.ScheduledFor = now.Add(-45 * time.Second)
	mustEnqueueOutgoingOutbox(t, repository, dispatching, "dispatching body")
	dispatchingLease := mustLeaseOne(t, repository, LeaseRequest{
		Owner: "worker-dispatching", Now: now, Duration: time.Minute, Limit: 1,
	})
	if dispatchingLease.OutboxID != dispatching.OutboxID {
		t.Fatalf("LeaseDue(dispatching) ID = %q, want %q", dispatchingLease.OutboxID, dispatching.OutboxID)
	}

	uncertain := outboxTestItem("list-uncertain")
	uncertain.ScheduledFor = now.Add(-30 * time.Second)
	mustEnqueueOutgoingOutbox(t, repository, uncertain, "uncertain body")
	uncertainLease := mustLeaseOne(t, repository, LeaseRequest{
		Owner: "worker-uncertain", Now: now, Duration: time.Minute, Limit: 1,
	})
	uncertainToken := mustLeaseToken(t, uncertainLease)
	if err := repository.MarkTransportCalled(ctx, Attempt{
		OutboxID: uncertain.OutboxID, LeaseToken: uncertainToken,
	}); err != nil {
		t.Fatalf("MarkTransportCalled(uncertain): %v", err)
	}
	if err := repository.MarkUncertain(
		ctx,
		uncertain.OutboxID,
		uncertainToken,
		"timeout",
		"deadline",
		"outcome unknown",
	); err != nil {
		t.Fatalf("MarkUncertain(): %v", err)
	}

	storeFailed := outboxTestItem("list-store-failed")
	storeFailed.ScheduledFor = now.Add(-15 * time.Second)
	mustEnqueueOutgoingOutbox(t, repository, storeFailed, "store failed body")
	storeFailedLease := mustLeaseOne(t, repository, LeaseRequest{
		Owner: "worker-store-failed", Now: now, Duration: time.Minute, Limit: 1,
	})
	storeFailedToken := mustLeaseToken(t, storeFailedLease)
	if err := repository.MarkTransportCalled(ctx, Attempt{
		OutboxID: storeFailed.OutboxID, LeaseToken: storeFailedToken,
	}); err != nil {
		t.Fatalf("MarkTransportCalled(store failed): %v", err)
	}
	if err := repository.MarkStoreFailed(
		ctx,
		storeFailed.OutboxID,
		storeFailedToken,
		"remote-store-failed",
		"local write failed",
	); err != nil {
		t.Fatalf("MarkStoreFailed(): %v", err)
	}

	reaction := outboxTestReactionItem("list-reaction")
	reaction.AccountID = "account-b"
	reaction.ConversationID = "conversation-c"
	reaction.ScheduledFor = now.Add(-2 * time.Minute)
	if _, disposition, err := repository.EnqueueReaction(ctx, reaction, OutboxReaction{
		TargetMessageID: "target-reaction",
		Emoji:           "👍",
		Action:          "add",
	}); err != nil {
		t.Fatalf("EnqueueReaction(): %v", err)
	} else if disposition != EnqueueInserted {
		t.Fatalf("EnqueueReaction() disposition = %q, want %q", disposition, EnqueueInserted)
	}
	reactionLease := mustLeaseOne(t, repository, LeaseRequest{
		Owner: "worker-reaction", Now: now, Duration: time.Minute, Limit: 1,
	})
	retryAt := now.Add(5 * time.Minute)
	if err := repository.MarkNotDispatched(
		ctx,
		reaction.OutboxID,
		mustLeaseToken(t, reactionLease),
		"transient",
		"offline",
		"bridge unavailable",
		retryAt,
	); err != nil {
		t.Fatalf("MarkNotDispatched(reaction): %v", err)
	}

	dueText := outboxTestItem("list-due-text")
	dueText.ScheduledFor = now.Add(-time.Minute)
	mustEnqueueOutgoingOutbox(t, repository, dueText, "due text body")

	media := outboxTestMediaItem("list-media")
	media.ConversationID = "conversation-b"
	media.ScheduledFor = now.Add(10 * time.Minute)
	attachment := outboxTestAttachment()
	if _, disposition, err := repository.EnqueueOutgoingMediaMessage(
		ctx,
		media,
		outboxTestOutgoingMessage(media, "media caption"),
		attachment,
	); err != nil {
		t.Fatalf("EnqueueOutgoingMediaMessage(): %v", err)
	} else if disposition != EnqueueInserted {
		t.Fatalf("EnqueueOutgoingMediaMessage() disposition = %q, want %q", disposition, EnqueueInserted)
	}

	read := outboxTestReadItem("list-read")
	read.ScheduledFor = now.Add(20 * time.Minute)
	targetID := "target-read"
	if _, disposition, err := repository.EnqueueReadReceipt(
		ctx,
		read,
		OutboxReadReceipt{
			DeviceID:          "device-a",
			LastReadMessageID: targetID,
			ReadAtMS:          now.Add(-time.Second).UnixMilli(),
		},
		ReadCursor{
			AccountID:         "account-a",
			DeviceID:          "device-a",
			ConversationID:    "conversation-a",
			LastReadMessageID: &targetID,
			LastReadAtMS:      now.Add(-time.Second).UnixMilli(),
			UpdatedAtMS:       now.UnixMilli(),
		},
	); err != nil {
		t.Fatalf("EnqueueReadReceipt(): %v", err)
	} else if disposition != EnqueueInserted {
		t.Fatalf("EnqueueReadReceipt() disposition = %q, want %q", disposition, EnqueueInserted)
	}

	futureText := outboxTestItem("list-future-text")
	futureText.ScheduledFor = now.Add(30 * time.Minute)
	mustEnqueueOutgoingOutbox(t, repository, futureText, "future text body")

	rows, err := repository.ListPending(ctx, ListPendingParams{Limit: 20})
	if err != nil {
		t.Fatalf("ListPending(): %v", err)
	}
	wantIDs := []string{
		dueText.OutboxID,
		dispatching.OutboxID,
		uncertain.OutboxID,
		storeFailed.OutboxID,
		reaction.OutboxID,
		media.OutboxID,
		read.OutboxID,
		futureText.OutboxID,
	}
	gotIDs := make([]string, len(rows))
	for i, row := range rows {
		gotIDs[i] = row.OutboxID
	}
	if !slices.Equal(gotIDs, wantIDs) {
		t.Fatalf("ListPending() IDs = %v, want %v", gotIDs, wantIDs)
	}
	trayStates := map[OutboxState]bool{
		OutboxQueued:        true,
		OutboxDispatching:   true,
		OutboxNotDispatched: true,
		OutboxUncertain:     true,
		OutboxStoreFailed:   true,
	}
	for _, row := range rows {
		if !trayStates[row.State] {
			t.Fatalf("ListPending() returned terminal row = %+v", row)
		}
		if row.ScheduledForMS <= 0 || row.CreatedAtMS != now.UnixMilli() {
			t.Fatalf("ListPending() timing fields = %+v", row)
		}
	}

	if rows[0].Body == nil || *rows[0].Body != "due text body" ||
		rows[0].MediaFile != nil || rows[0].MediaMIME != nil || rows[0].Emoji != nil {
		t.Fatalf("text summary sources = %+v", rows[0])
	}
	if rows[1].State != OutboxDispatching || rows[1].AttemptCount != 0 ||
		rows[1].NextAttemptAtMS != nil || rows[1].Body == nil || *rows[1].Body != "dispatching body" {
		t.Fatalf("dispatching row = %+v", rows[1])
	}
	if rows[2].State != OutboxUncertain || rows[2].AttemptCount != 1 ||
		rows[2].NextAttemptAtMS != nil || rows[2].ErrorClass == nil ||
		*rows[2].ErrorClass != "timeout" || rows[2].ErrorCode == nil ||
		*rows[2].ErrorCode != "deadline" || rows[2].Body == nil || *rows[2].Body != "uncertain body" {
		t.Fatalf("uncertain row = %+v", rows[2])
	}
	if rows[3].State != OutboxStoreFailed || rows[3].AttemptCount != 1 ||
		rows[3].NextAttemptAtMS != nil || rows[3].ResultRemoteID == nil ||
		*rows[3].ResultRemoteID != "remote-store-failed" || rows[3].Body == nil ||
		*rows[3].Body != "store failed body" {
		t.Fatalf("store-failed row = %+v", rows[3])
	}
	if rows[4].State != OutboxNotDispatched || rows[4].NextAttemptAtMS == nil ||
		*rows[4].NextAttemptAtMS != retryAt.UnixMilli() || rows[4].AttemptCount != 0 ||
		rows[4].ErrorClass == nil || *rows[4].ErrorClass != "transient" ||
		rows[4].ErrorCode == nil || *rows[4].ErrorCode != "offline" ||
		rows[4].Emoji == nil || *rows[4].Emoji != "👍" || rows[4].Body != nil {
		t.Fatalf("not-dispatched reaction row = %+v", rows[4])
	}
	if rows[5].Body == nil || *rows[5].Body != "media caption" ||
		rows[5].MediaFile == nil || *rows[5].MediaFile != attachment.Filename ||
		rows[5].MediaMIME == nil || *rows[5].MediaMIME != attachment.MIME || rows[5].Emoji != nil {
		t.Fatalf("media summary sources = %+v", rows[5])
	}
	if rows[6].Kind != OutboxKindRead || rows[6].Body != nil || rows[6].MediaFile != nil ||
		rows[6].MediaMIME != nil || rows[6].Emoji != nil {
		t.Fatalf("read summary sources = %+v", rows[6])
	}
	if rows[7].Body == nil || *rows[7].Body != "future text body" {
		t.Fatalf("future text summary sources = %+v", rows[7])
	}

	accountRows, err := repository.ListPending(ctx, ListPendingParams{
		AccountID: "account-a",
		Limit:     2,
	})
	if err != nil {
		t.Fatalf("ListPending(account, limit): %v", err)
	}
	if len(accountRows) != 2 {
		t.Fatalf("ListPending(account, limit) returned %d rows, want 2: %+v", len(accountRows), accountRows)
	}
	if got := []string{accountRows[0].OutboxID, accountRows[1].OutboxID}; !slices.Equal(got, []string{dueText.OutboxID, dispatching.OutboxID}) {
		t.Fatalf("ListPending(account, limit) IDs = %v", got)
	}

	reactionRows, err := repository.ListPending(ctx, ListPendingParams{
		ConversationID: "conversation-c",
		Limit:          20,
	})
	if err != nil {
		t.Fatalf("ListPending(conversation): %v", err)
	}
	if len(reactionRows) != 1 || reactionRows[0].OutboxID != reaction.OutboxID {
		t.Fatalf("ListPending(conversation) = %+v, want only %q", reactionRows, reaction.OutboxID)
	}

	conversationRows, err := repository.ListPending(ctx, ListPendingParams{
		AccountID:      "account-a",
		ConversationID: "conversation-a",
		Limit:          20,
	})
	if err != nil {
		t.Fatalf("ListPending(account, conversation): %v", err)
	}
	conversationIDs := make([]string, len(conversationRows))
	for i, row := range conversationRows {
		conversationIDs[i] = row.OutboxID
	}
	if want := []string{
		dueText.OutboxID,
		dispatching.OutboxID,
		uncertain.OutboxID,
		storeFailed.OutboxID,
		read.OutboxID,
		futureText.OutboxID,
	}; !slices.Equal(conversationIDs, want) {
		t.Fatalf("ListPending(account, conversation) IDs = %v, want %v", conversationIDs, want)
	}

	if _, err := repository.ListPending(ctx, ListPendingParams{}); err == nil {
		t.Fatal("ListPending(non-positive limit) succeeded")
	}
}

func TestOutboxListConfirmedSinceReturnsJoinedRowsInWatermarkOrder(t *testing.T) {
	clock := newOutboxTestClock(outboxTestTimeMS)
	store, repository := openOutboxTestRepository(t, clock.Now)
	ctx := context.Background()

	seedMessageIdentity(t, store, "identity-a", "account-a")
	seedMessageConversation(t, store, "conversation-a", "account-a")
	seedOutboxTestDevice(t, store, "device-a", "account-a")
	seedOutboxTestMessage(t, store, "target-a", "account-a", "conversation-a")

	confirm := func(outboxID string, atMS int64, withoutResult bool) {
		t.Helper()
		lease := mustLeaseOne(t, repository, LeaseRequest{
			Owner: "worker-" + outboxID,
			Now:   clock.Now(), Duration: time.Minute, Limit: 1,
		})
		if lease.OutboxID != outboxID {
			t.Fatalf("LeaseDue() returned %q, want %q", lease.OutboxID, outboxID)
		}
		token := mustLeaseToken(t, lease)
		if err := repository.MarkTransportCalled(ctx, Attempt{
			OutboxID: outboxID, LeaseToken: token,
		}); err != nil {
			t.Fatalf("MarkTransportCalled(%q): %v", outboxID, err)
		}
		clock.Set(atMS)
		if withoutResult {
			if err := repository.ConfirmWithoutResult(ctx, outboxID, token); err != nil {
				t.Fatalf("ConfirmWithoutResult(%q): %v", outboxID, err)
			}
			return
		}
		if err := repository.Confirm(ctx, Confirmation{
			OutboxID: outboxID, LeaseToken: token, ResultRemoteID: "remote-" + outboxID,
		}); err != nil {
			t.Fatalf("Confirm(%q): %v", outboxID, err)
		}
	}

	boundaryMS := outboxTestTimeMS + 100
	oldText := outboxTestItem("confirmed-boundary")
	mustEnqueueOutgoingOutbox(t, repository, oldText, "boundary body")
	confirm(oldText.OutboxID, boundaryMS, false)

	confirmedMS := boundaryMS + 100
	media := outboxTestMediaItem("confirmed-a-media")
	attachment := outboxTestAttachment()
	if _, disposition, err := repository.EnqueueOutgoingMediaMessage(
		ctx,
		media,
		outboxTestOutgoingMessage(media, "media caption"),
		attachment,
	); err != nil {
		t.Fatalf("EnqueueOutgoingMediaMessage(): %v", err)
	} else if disposition != EnqueueInserted {
		t.Fatalf("EnqueueOutgoingMediaMessage() disposition = %q, want %q", disposition, EnqueueInserted)
	}
	confirm(media.OutboxID, confirmedMS, false)

	reaction := outboxTestReactionItem("confirmed-b-reaction")
	if _, disposition, err := repository.EnqueueReaction(ctx, reaction, OutboxReaction{
		TargetMessageID: "target-a",
		Emoji:           "👍",
		Action:          "add",
	}); err != nil {
		t.Fatalf("EnqueueReaction(): %v", err)
	} else if disposition != EnqueueInserted {
		t.Fatalf("EnqueueReaction() disposition = %q, want %q", disposition, EnqueueInserted)
	}
	confirm(reaction.OutboxID, confirmedMS, true)

	read := outboxTestReadItem("confirmed-c-read")
	targetID := "target-a"
	if _, disposition, err := repository.EnqueueReadReceipt(
		ctx,
		read,
		OutboxReadReceipt{
			DeviceID:          "device-a",
			LastReadMessageID: targetID,
			ReadAtMS:          confirmedMS,
		},
		ReadCursor{
			AccountID:         "account-a",
			DeviceID:          "device-a",
			ConversationID:    "conversation-a",
			LastReadMessageID: &targetID,
			LastReadAtMS:      confirmedMS,
			UpdatedAtMS:       confirmedMS,
		},
	); err != nil {
		t.Fatalf("EnqueueReadReceipt(): %v", err)
	} else if disposition != EnqueueInserted {
		t.Fatalf("EnqueueReadReceipt() disposition = %q, want %q", disposition, EnqueueInserted)
	}
	confirm(read.OutboxID, confirmedMS, true)

	text := outboxTestItem("confirmed-d-text")
	mustEnqueueOutgoingOutbox(t, repository, text, "confirmed text body")
	confirm(text.OutboxID, confirmedMS, false)

	queued := outboxTestItem("confirmed-filter-queued")
	mustEnqueueOutgoingOutbox(t, repository, queued, "not confirmed")

	rows, err := repository.ListConfirmedSince(ctx, boundaryMS, 20)
	if err != nil {
		t.Fatalf("ListConfirmedSince(): %v", err)
	}
	wantIDs := []string{media.OutboxID, reaction.OutboxID, read.OutboxID, text.OutboxID}
	gotIDs := make([]string, len(rows))
	for i, row := range rows {
		gotIDs[i] = row.OutboxID
		if row.State != OutboxConfirmed || row.UpdatedAtMS != confirmedMS {
			t.Fatalf("ListConfirmedSince() row = %+v, want confirmed at %d", row, confirmedMS)
		}
	}
	if !slices.Equal(gotIDs, wantIDs) {
		t.Fatalf("ListConfirmedSince() IDs = %v, want %v", gotIDs, wantIDs)
	}
	if rows[0].Body == nil || *rows[0].Body != "media caption" ||
		rows[0].MediaFile == nil || *rows[0].MediaFile != attachment.Filename ||
		rows[0].MediaMIME == nil || *rows[0].MediaMIME != attachment.MIME || rows[0].Emoji != nil {
		t.Fatalf("confirmed media sources = %+v", rows[0])
	}
	if rows[1].Kind != OutboxKindReaction || rows[1].Emoji == nil || *rows[1].Emoji != "👍" ||
		rows[1].Body != nil || rows[1].ResultRemoteID != nil {
		t.Fatalf("confirmed reaction sources = %+v", rows[1])
	}
	if rows[2].Kind != OutboxKindRead || rows[2].Body != nil || rows[2].MediaFile != nil ||
		rows[2].MediaMIME != nil || rows[2].Emoji != nil || rows[2].ResultRemoteID != nil {
		t.Fatalf("confirmed read sources = %+v", rows[2])
	}
	if rows[3].Kind != OutboxKindText || rows[3].Body == nil || *rows[3].Body != "confirmed text body" ||
		rows[3].MediaFile != nil || rows[3].Emoji != nil || rows[3].ResultRemoteID == nil {
		t.Fatalf("confirmed text sources = %+v", rows[3])
	}

	limited, err := repository.ListConfirmedSince(ctx, boundaryMS, 2)
	if err != nil {
		t.Fatalf("ListConfirmedSince(limit): %v", err)
	}
	if got := []string{limited[0].OutboxID, limited[1].OutboxID}; !slices.Equal(got, wantIDs[:2]) {
		t.Fatalf("ListConfirmedSince(limit) IDs = %v, want %v", got, wantIDs[:2])
	}
	strict, err := repository.ListConfirmedSince(ctx, confirmedMS, 20)
	if err != nil {
		t.Fatalf("ListConfirmedSince(strict watermark): %v", err)
	}
	if len(strict) != 0 {
		t.Fatalf("ListConfirmedSince(strict watermark) = %+v, want empty", strict)
	}
	if _, err := repository.ListConfirmedSince(ctx, boundaryMS, 0); err == nil {
		t.Fatal("ListConfirmedSince(non-positive limit) succeeded")
	}
}

func TestOutboxPreCallExpiredLeaseRetriesAndStaleTokenLoses(t *testing.T) {
	clock := newOutboxTestClock(outboxTestTimeMS)
	_, repository := openOutboxTestRepository(t, clock.Now)
	ctx := context.Background()
	item := outboxTestItem("pre-call")
	mustEnqueueOutbox(t, repository, item)
	first := mustLeaseOne(t, repository, LeaseRequest{
		Owner: "worker-a", Now: clock.Now(), Duration: time.Second, Limit: 1,
	})
	firstToken := mustLeaseToken(t, first)

	clock.Set(outboxTestTimeMS + 1_000)
	recovered, err := repository.RecoverExpiredLeases(ctx, clock.Now())
	if err != nil {
		t.Fatalf("RecoverExpiredLeases(): %v", err)
	}
	if recovered.NotDispatched != 1 || recovered.Uncertain != 0 {
		t.Fatalf("RecoverExpiredLeases() = %+v", recovered)
	}
	retryable, err := repository.FindByID(ctx, item.OutboxID)
	if err != nil {
		t.Fatalf("FindByID(recovered): %v", err)
	}
	if retryable.State != OutboxNotDispatched || retryable.NextAttemptAtMS == nil ||
		*retryable.NextAttemptAtMS != clock.Now().UnixMilli() || retryable.LeaseToken != nil {
		t.Fatalf("pre-call recovered row = %+v", retryable)
	}

	second := mustLeaseOne(t, repository, LeaseRequest{
		Owner: "worker-b", Now: clock.Now(), Duration: time.Second, Limit: 1,
	})
	secondToken := mustLeaseToken(t, second)
	if secondToken == firstToken {
		t.Fatal("re-leased row reused its stale lease token")
	}
	err = repository.MarkNotDispatched(
		ctx,
		item.OutboxID,
		firstToken,
		"late",
		"stale_worker",
		"late result",
		clock.Now().Add(time.Second),
	)
	if !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("late MarkNotDispatched() error = %v, want ErrLeaseLost", err)
	}
	stillLeased, err := repository.FindByID(ctx, item.OutboxID)
	if err != nil {
		t.Fatalf("FindByID(re-leased): %v", err)
	}
	if stillLeased.State != OutboxDispatching || stillLeased.LeaseToken == nil || *stillLeased.LeaseToken != secondToken {
		t.Fatalf("stale mutation changed re-leased row: %+v", stillLeased)
	}
}

func TestOutboxPostCallExpiredLeaseBecomesUncertain(t *testing.T) {
	clock := newOutboxTestClock(outboxTestTimeMS)
	_, repository := openOutboxTestRepository(t, clock.Now)
	ctx := context.Background()
	item := outboxTestItem("post-call")
	mustEnqueueOutbox(t, repository, item)
	lease := mustLeaseOne(t, repository, LeaseRequest{
		Owner: "worker", Now: clock.Now(), Duration: time.Second, Limit: 1,
	})
	if err := repository.MarkTransportCalled(ctx, Attempt{
		OutboxID: item.OutboxID, LeaseToken: mustLeaseToken(t, lease),
	}); err != nil {
		t.Fatalf("MarkTransportCalled(): %v", err)
	}
	called, err := repository.FindByID(ctx, item.OutboxID)
	if err != nil {
		t.Fatalf("FindByID(called): %v", err)
	}
	if called.AttemptCount != 1 || called.TransportCalledAtMS == nil {
		t.Fatalf("called row = %+v", called)
	}

	clock.Set(outboxTestTimeMS + 1_000)
	recovered, err := repository.RecoverExpiredLeases(ctx, clock.Now())
	if err != nil {
		t.Fatalf("RecoverExpiredLeases(): %v", err)
	}
	if recovered.NotDispatched != 0 || recovered.Uncertain != 1 {
		t.Fatalf("RecoverExpiredLeases() = %+v", recovered)
	}
	uncertain, err := repository.FindByID(ctx, item.OutboxID)
	if err != nil {
		t.Fatalf("FindByID(uncertain): %v", err)
	}
	if uncertain.State != OutboxUncertain || uncertain.NextAttemptAtMS != nil || uncertain.LeaseToken != nil {
		t.Fatalf("post-call recovered row = %+v", uncertain)
	}
	leases, err := repository.LeaseDue(ctx, LeaseRequest{
		Owner: "worker-later", Now: clock.Now().Add(time.Hour), Duration: time.Second, Limit: 10,
	})
	if err != nil {
		t.Fatalf("LeaseDue(uncertain): %v", err)
	}
	if len(leases) != 0 {
		t.Fatalf("LeaseDue(uncertain) = %+v, want no automatic retry", leases)
	}
}

func TestOutboxLateConfirmationWinsBeforeRecovery(t *testing.T) {
	clock := newOutboxTestClock(outboxTestTimeMS)
	_, repository := openOutboxTestRepository(t, clock.Now)
	ctx := context.Background()
	item := mustEnqueueOutbox(t, repository, outboxTestItem("late-confirmation"))
	lease := mustLeaseOne(t, repository, LeaseRequest{
		Owner: "worker", Now: clock.Now(), Duration: time.Second, Limit: 1,
	})
	token := mustLeaseToken(t, lease)
	if err := repository.MarkTransportCalled(ctx, Attempt{
		OutboxID: item.OutboxID, LeaseToken: token,
	}); err != nil {
		t.Fatalf("MarkTransportCalled(): %v", err)
	}

	// Expiry makes the row recoverable, but recovery has not fenced this token
	// yet. A definitive result and recovery serialize; the first commit wins.
	clock.Set(outboxTestTimeMS + 1_000)
	if err := repository.Confirm(ctx, Confirmation{
		OutboxID: item.OutboxID, LeaseToken: token, ResultRemoteID: "remote-late",
	}); err != nil {
		t.Fatalf("Confirm(after expiry, before recovery): %v", err)
	}
	recovered, err := repository.RecoverExpiredLeases(ctx, clock.Now())
	if err != nil {
		t.Fatalf("RecoverExpiredLeases(): %v", err)
	}
	if recovered != (RecoveredLeases{}) {
		t.Fatalf("RecoverExpiredLeases() = %+v, want no recovered terminal row", recovered)
	}
	confirmed, err := repository.FindByID(ctx, item.OutboxID)
	if err != nil {
		t.Fatalf("FindByID(): %v", err)
	}
	if confirmed.State != OutboxConfirmed || confirmed.ResultRemoteID == nil || *confirmed.ResultRemoteID != "remote-late" {
		t.Fatalf("late confirmed row = %+v", confirmed)
	}
}

func TestOutboxConcurrentLeaseDueClaimsEachRowOnce(t *testing.T) {
	clock := newOutboxTestClock(outboxTestTimeMS)
	_, repository := openOutboxTestRepository(t, clock.Now)
	const itemCount = 12
	for i := range itemCount {
		mustEnqueueOutbox(t, repository, outboxTestItem(fmt.Sprintf("concurrent-%02d", i)))
	}

	type leaseResult struct {
		leases []Lease
		err    error
	}
	start := make(chan struct{})
	results := make(chan leaseResult, 2)
	for _, owner := range []string{"worker-a", "worker-b"} {
		go func(owner string) {
			<-start
			leases, err := repository.LeaseDue(context.Background(), LeaseRequest{
				Owner: owner, Now: clock.Now(), Duration: time.Minute, Limit: itemCount,
			})
			results <- leaseResult{leases: leases, err: err}
		}(owner)
	}
	close(start)

	seenIDs := make(map[string]string, itemCount)
	for range 2 {
		result := <-results
		if result.err != nil {
			t.Fatalf("concurrent LeaseDue(): %v", result.err)
		}
		for _, lease := range result.leases {
			token := mustLeaseToken(t, lease.OutboxItem)
			if previous, exists := seenIDs[lease.OutboxID]; exists {
				t.Fatalf("outbox item %q leased twice with tokens %q and %q", lease.OutboxID, previous, token)
			}
			seenIDs[lease.OutboxID] = token
		}
	}
	if len(seenIDs) != itemCount {
		t.Fatalf("concurrent LeaseDue() claimed %d unique rows, want %d", len(seenIDs), itemCount)
	}
}

func TestOutboxTerminalAndUncertainTransitions(t *testing.T) {
	tests := []struct {
		name       string
		wantState  OutboxState
		transition func(context.Context, *OutboxRepository, OutboxItem, string) error
		assert     func(*testing.T, OutboxItem)
	}{
		{
			name: "confirm", wantState: OutboxConfirmed,
			transition: func(ctx context.Context, repository *OutboxRepository, item OutboxItem, token string) error {
				return repository.Confirm(ctx, Confirmation{
					OutboxID: item.OutboxID, LeaseToken: token, ResultRemoteID: "remote-confirmed",
				})
			},
			assert: func(t *testing.T, item OutboxItem) {
				if item.ResultRemoteID == nil || *item.ResultRemoteID != "remote-confirmed" {
					t.Fatalf("confirmed result = %+v", item.ResultRemoteID)
				}
			},
		},
		{
			name: "reject", wantState: OutboxRejected,
			transition: func(ctx context.Context, repository *OutboxRepository, item OutboxItem, token string) error {
				return repository.Reject(ctx, item.OutboxID, token, "permanent", "blocked", "recipient blocked")
			},
			assert: func(t *testing.T, item OutboxItem) {
				assertOutboxText(t, "error class", item.ErrorClass, "permanent")
				assertOutboxText(t, "error code", item.ErrorCode, "blocked")
				assertOutboxText(t, "error detail", item.ErrorDetail, "recipient blocked")
			},
		},
		{
			name: "store failed", wantState: OutboxStoreFailed,
			transition: func(ctx context.Context, repository *OutboxRepository, item OutboxItem, token string) error {
				return repository.MarkStoreFailed(ctx, item.OutboxID, token, "remote-accepted", "disk full")
			},
			assert: func(t *testing.T, item OutboxItem) {
				assertOutboxText(t, "result remote ID", item.ResultRemoteID, "remote-accepted")
				assertOutboxText(t, "error detail", item.ErrorDetail, "disk full")
			},
		},
		{
			name: "uncertain", wantState: OutboxUncertain,
			transition: func(ctx context.Context, repository *OutboxRepository, item OutboxItem, token string) error {
				return repository.MarkUncertain(ctx, item.OutboxID, token, "timeout", "deadline", "outcome unknown")
			},
			assert: func(t *testing.T, item OutboxItem) {
				assertOutboxText(t, "error class", item.ErrorClass, "timeout")
				if item.NextAttemptAtMS != nil {
					t.Fatalf("uncertain next attempt = %v, want nil", item.NextAttemptAtMS)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clock := newOutboxTestClock(outboxTestTimeMS)
			_, repository := openOutboxTestRepository(t, clock.Now)
			ctx := context.Background()
			input := outboxTestItem(stringsForOutboxID(test.name))
			item := mustEnqueueOutbox(t, repository, input)
			lease := mustLeaseOne(t, repository, LeaseRequest{
				Owner: "worker", Now: clock.Now(), Duration: time.Minute, Limit: 1,
			})
			token := mustLeaseToken(t, lease)
			if err := repository.MarkTransportCalled(ctx, Attempt{
				OutboxID: item.OutboxID, LeaseToken: token,
			}); err != nil {
				t.Fatalf("MarkTransportCalled(): %v", err)
			}
			if err := test.transition(ctx, repository, item, token); err != nil {
				t.Fatalf("transition: %v", err)
			}
			got, err := repository.FindByID(ctx, item.OutboxID)
			if err != nil {
				t.Fatalf("FindByID(): %v", err)
			}
			if got.State != test.wantState || got.LeaseToken != nil || got.LeaseOwner != nil || got.LeaseExpiresAtMS != nil {
				t.Fatalf("terminal row = %+v, want state %q and no lease", got, test.wantState)
			}
			test.assert(t, got)
		})
	}
}

func TestOutboxPhaseTransitionsRejectWrongSideOfCallBoundary(t *testing.T) {
	clock := newOutboxTestClock(outboxTestTimeMS)
	_, repository := openOutboxTestRepository(t, clock.Now)
	ctx := context.Background()
	item := mustEnqueueOutbox(t, repository, outboxTestItem("phase"))
	lease := mustLeaseOne(t, repository, LeaseRequest{
		Owner: "worker", Now: clock.Now(), Duration: time.Minute, Limit: 1,
	})
	token := mustLeaseToken(t, lease)
	if err := repository.MarkUncertain(ctx, item.OutboxID, token, "", "", ""); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("pre-call MarkUncertain() error = %v, want ErrLeaseLost", err)
	}
	if err := repository.MarkTransportCalled(ctx, Attempt{OutboxID: item.OutboxID, LeaseToken: token}); err != nil {
		t.Fatalf("MarkTransportCalled(): %v", err)
	}
	if err := repository.MarkNotDispatched(
		ctx, item.OutboxID, token, "", "", "", clock.Now().Add(time.Second),
	); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("post-call MarkNotDispatched() error = %v, want ErrLeaseLost", err)
	}
}

func TestOutboxReleaseUnavailableReturnsLeaseToQueuedWithoutAttempt(t *testing.T) {
	clock := newOutboxTestClock(outboxTestTimeMS)
	_, repository := openOutboxTestRepository(t, clock.Now)
	ctx := context.Background()
	item := outboxTestItem("unavailable")
	mustEnqueueOutbox(t, repository, item)
	lease := mustLeaseOne(t, repository, LeaseRequest{
		Owner: "worker", Now: clock.Now(), Duration: time.Minute, Limit: 1,
	})
	token := mustLeaseToken(t, lease)
	if err := repository.ReleaseUnavailable(ctx, item.OutboxID, "stale-token"); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("ReleaseUnavailable(stale token) error = %v, want ErrLeaseLost", err)
	}
	if err := repository.ReleaseUnavailable(ctx, item.OutboxID, token); err != nil {
		t.Fatalf("ReleaseUnavailable(): %v", err)
	}
	queued, err := repository.FindByID(ctx, item.OutboxID)
	if err != nil {
		t.Fatalf("FindByID(): %v", err)
	}
	if queued.State != OutboxQueued || queued.AttemptCount != 0 || queued.LeaseToken != nil ||
		queued.NextAttemptAtMS != nil || queued.TransportCalledAtMS != nil ||
		queued.TransportRequestID != item.TransportRequestID {
		t.Fatalf("released unavailable row = %+v", queued)
	}
}

func TestOutboxMarkCalledNotDispatchedRecordsExplicitSafeRetry(t *testing.T) {
	clock := newOutboxTestClock(outboxTestTimeMS)
	_, repository := openOutboxTestRepository(t, clock.Now)
	ctx := context.Background()
	item := outboxTestItem("called-not-dispatched")
	mustEnqueueOutbox(t, repository, item)
	lease := mustLeaseOne(t, repository, LeaseRequest{
		Owner: "worker", Now: clock.Now(), Duration: time.Minute, Limit: 1,
	})
	token := mustLeaseToken(t, lease)
	if err := repository.MarkCalledNotDispatched(
		ctx, item.OutboxID, token, "transient", "preflight", "not sent", clock.Now().Add(time.Minute),
	); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("MarkCalledNotDispatched(pre-call) error = %v, want ErrLeaseLost", err)
	}
	if err := repository.MarkTransportCalled(ctx, Attempt{OutboxID: item.OutboxID, LeaseToken: token}); err != nil {
		t.Fatalf("MarkTransportCalled(): %v", err)
	}
	retryAt := clock.Now().Add(time.Minute)
	if err := repository.MarkCalledNotDispatched(
		ctx, item.OutboxID, token, "transient", "preflight", "transport proved no dispatch", retryAt,
	); err != nil {
		t.Fatalf("MarkCalledNotDispatched(): %v", err)
	}
	notDispatched, err := repository.FindByID(ctx, item.OutboxID)
	if err != nil {
		t.Fatalf("FindByID(): %v", err)
	}
	if notDispatched.State != OutboxNotDispatched || notDispatched.TransportCalledAtMS != nil ||
		notDispatched.NextAttemptAtMS == nil || *notDispatched.NextAttemptAtMS != retryAt.UnixMilli() ||
		notDispatched.TransportRequestID != item.TransportRequestID || notDispatched.AttemptCount != 1 {
		t.Fatalf("called-not-dispatched row = %+v", notDispatched)
	}
	assertOutboxText(t, "error class", notDispatched.ErrorClass, "transient")
	assertOutboxText(t, "error code", notDispatched.ErrorCode, "preflight")
}

func TestOutboxRetryNotDispatchedMakesRowDueWithSameRequestID(t *testing.T) {
	clock := newOutboxTestClock(outboxTestTimeMS)
	_, repository := openOutboxTestRepository(t, clock.Now)
	ctx := context.Background()
	item := outboxTestItem("manual-retry")
	mustEnqueueOutbox(t, repository, item)
	lease := mustLeaseOne(t, repository, LeaseRequest{
		Owner: "worker", Now: clock.Now(), Duration: time.Minute, Limit: 1,
	})
	if err := repository.MarkNotDispatched(
		ctx, item.OutboxID, mustLeaseToken(t, lease), "transient", "offline", "retry later", clock.Now().Add(time.Hour),
	); err != nil {
		t.Fatalf("MarkNotDispatched(): %v", err)
	}
	clock.Set(outboxTestTimeMS + 1_000)
	if err := repository.RetryNotDispatched(ctx, item.OutboxID); err != nil {
		t.Fatalf("RetryNotDispatched(): %v", err)
	}
	retryable, err := repository.FindByID(ctx, item.OutboxID)
	if err != nil {
		t.Fatalf("FindByID(): %v", err)
	}
	if retryable.State != OutboxNotDispatched || retryable.NextAttemptAtMS == nil ||
		*retryable.NextAttemptAtMS != clock.Now().UnixMilli() ||
		retryable.TransportRequestID != item.TransportRequestID || retryable.ErrorClass == nil ||
		*retryable.ErrorClass != "transient" {
		t.Fatalf("manual retry row = %+v", retryable)
	}
	queuedItem := outboxTestItem("manual-retry-wrong-state")
	mustEnqueueOutbox(t, repository, queuedItem)
	if err := repository.RetryNotDispatched(ctx, queuedItem.OutboxID); !errors.Is(err, ErrInvalidOutboxState) {
		t.Fatalf("RetryNotDispatched(queued) error = %v, want ErrInvalidOutboxState", err)
	}
	if err := repository.RetryNotDispatched(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("RetryNotDispatched(missing) error = %v, want ErrNotFound", err)
	}
}

func TestOutboxCancelAllowsOnlyQueuedOrNotDispatched(t *testing.T) {
	clock := newOutboxTestClock(outboxTestTimeMS)
	_, repository := openOutboxTestRepository(t, clock.Now)
	ctx := context.Background()

	queuedItem := outboxTestItem("cancel-queued")
	mustEnqueueOutbox(t, repository, queuedItem)
	if err := repository.Cancel(ctx, queuedItem.OutboxID); err != nil {
		t.Fatalf("Cancel(queued): %v", err)
	}
	canceled, err := repository.FindByID(ctx, queuedItem.OutboxID)
	if err != nil {
		t.Fatalf("FindByID(canceled): %v", err)
	}
	if canceled.State != OutboxCanceled || canceled.NextAttemptAtMS != nil {
		t.Fatalf("canceled queued row = %+v", canceled)
	}
	if err := repository.Cancel(ctx, queuedItem.OutboxID); !errors.Is(err, ErrInvalidOutboxState) {
		t.Fatalf("Cancel(canceled) error = %v, want ErrInvalidOutboxState", err)
	}

	notDispatchedItem := outboxTestItem("cancel-not-dispatched")
	mustEnqueueOutbox(t, repository, notDispatchedItem)
	lease := mustLeaseOne(t, repository, LeaseRequest{
		Owner: "worker", Now: clock.Now(), Duration: time.Minute, Limit: 1,
	})
	if err := repository.MarkNotDispatched(
		ctx, notDispatchedItem.OutboxID, mustLeaseToken(t, lease), "transient", "offline", "retry later", clock.Now().Add(time.Hour),
	); err != nil {
		t.Fatalf("MarkNotDispatched(): %v", err)
	}
	if err := repository.Cancel(ctx, notDispatchedItem.OutboxID); err != nil {
		t.Fatalf("Cancel(not dispatched): %v", err)
	}

	dispatchingItem := outboxTestItem("cancel-dispatching")
	mustEnqueueOutbox(t, repository, dispatchingItem)
	lease = mustLeaseOne(t, repository, LeaseRequest{
		Owner: "worker", Now: clock.Now(), Duration: time.Minute, Limit: 1,
	})
	if lease.OutboxID != dispatchingItem.OutboxID {
		t.Fatalf("LeaseDue(cancel dispatching) = %+v, want %q", lease, dispatchingItem.OutboxID)
	}
	if err := repository.Cancel(ctx, dispatchingItem.OutboxID); !errors.Is(err, ErrInvalidOutboxState) {
		t.Fatalf("Cancel(dispatching) error = %v, want ErrInvalidOutboxState", err)
	}
	if err := repository.Cancel(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Cancel(missing) error = %v, want ErrNotFound", err)
	}
}

func TestOutboxMigrationIsChecksummedAndStrict(t *testing.T) {
	store, _ := openOutboxTestRepository(t, func() time.Time {
		return time.UnixMilli(outboxTestTimeMS)
	})
	if len(embeddedMigrations) != 11 {
		t.Fatalf("embedded migrations = %d, want 11", len(embeddedMigrations))
	}
	assertPragmaInt(t, store.db, "user_version", 11)
	ledger := readLedgerRow(t, store.db, 5)
	if ledger.name != "outbox" {
		t.Fatalf("migration 0005 name = %q, want outbox", ledger.name)
	}
	if ledger.checksum != embeddedMigrations[4].checksumSHA256 {
		t.Fatalf("migration 0005 checksum = %q, want %q", ledger.checksum, embeddedMigrations[4].checksumSHA256)
	}
	var strict int
	if err := store.db.QueryRow(`
		SELECT strict
		FROM pragma_table_list
		WHERE schema = 'main' AND name = 'outbox'
	`).Scan(&strict); err != nil {
		t.Fatalf("read outbox STRICT flag: %v", err)
	}
	if strict != 1 {
		t.Fatalf("outbox strict = %d, want 1", strict)
	}
}

func TestOutboxSendAgainMigrationAppliesToBlankAndExistingV8DatabaseWithRows(t *testing.T) {
	t.Run("blank", func(t *testing.T) {
		store, err := Open(filepath.Join(t.TempDir(), "store.sqlite3"))
		if err != nil {
			t.Fatalf("Open(): %v", err)
		}
		t.Cleanup(func() {
			if err := store.Close(); err != nil {
				t.Errorf("Close(): %v", err)
			}
		})
		assertOutboxSendAgainMigration(t, store)
	})

	t.Run("existing v8 with rows", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "store.sqlite3")
		db, err := sql.Open("sqlite", storeDSN(path))
		if err != nil {
			t.Fatalf("sql.Open(): %v", err)
		}
		if err := db.Ping(); err != nil {
			_ = db.Close()
			t.Fatalf("Ping(): %v", err)
		}
		if err := enableWAL(context.Background(), db); err != nil {
			_ = db.Close()
			t.Fatalf("enableWAL(): %v", err)
		}
		if err := verifyConnectionPragmas(context.Background(), db); err != nil {
			_ = db.Close()
			t.Fatalf("verifyConnectionPragmas(): %v", err)
		}
		if err := runMigrations(context.Background(), db, embeddedMigrations[:8]); err != nil {
			_ = db.Close()
			t.Fatalf("runMigrations(v8): %v", err)
		}
		mustExec(t, db, `
			INSERT INTO accounts (account_id, bridge_key, created_at_ms, updated_at_ms)
			VALUES ('account-existing-v8', 'test', ?, ?)
		`, outboxTestTimeMS, outboxTestTimeMS)
		mustExec(t, db, `
			INSERT INTO outbox (
				outbox_id,
				account_id,
				conversation_id,
				kind,
				idempotency_key,
				payload_hash,
				operation,
				state,
				transport_request_id,
				attempt_count,
				scheduled_for_ms,
				created_at_ms,
				updated_at_ms
			) VALUES (?, ?, ?, 'text', ?, ?, 'send_text', 'uncertain', ?, 1, ?, ?, ?)
		`,
			"outbox-existing-v8",
			"account-existing-v8",
			"conversation-existing-v8",
			"idempotency-existing-v8",
			"payload-existing-v8",
			"request-existing-v8",
			outboxTestTimeMS,
			outboxTestTimeMS,
			outboxTestTimeMS,
		)
		before := readLedgerRows(t, db)
		if len(before) != 8 {
			_ = db.Close()
			t.Fatalf("v8 ledger rows = %d, want 8", len(before))
		}
		if err := db.Close(); err != nil {
			t.Fatalf("close v8 database: %v", err)
		}

		store, err := Open(path)
		if err != nil {
			t.Fatalf("Open(v8): %v", err)
		}
		t.Cleanup(func() {
			if err := store.Close(); err != nil {
				t.Errorf("Close(): %v", err)
			}
		})
		after := readLedgerRows(t, store.db)
		if len(after) != 11 {
			t.Fatalf("migrated ledger rows = %d, want 11", len(after))
		}
		if !slices.Equal(after[:8], before) {
			t.Fatalf("migrations 0001-0008 changed:\nbefore: %+v\nafter:  %+v", before, after[:8])
		}
		assertOutboxSendAgainMigration(t, store)
		var sendAgainOf sql.NullString
		if err := store.db.QueryRow(`
			SELECT send_again_of_outbox_id
			FROM outbox
			WHERE outbox_id = 'outbox-existing-v8'
		`).Scan(&sendAgainOf); err != nil {
			t.Fatalf("read existing v8 outbox row: %v", err)
		}
		if sendAgainOf.Valid {
			t.Fatalf("existing v8 send_again_of_outbox_id = %q, want NULL", sendAgainOf.String)
		}
		var foreignKeyViolations int
		if err := store.db.QueryRow(`SELECT count(*) FROM pragma_foreign_key_check`).Scan(&foreignKeyViolations); err != nil {
			t.Fatalf("PRAGMA foreign_key_check: %v", err)
		}
		if foreignKeyViolations != 0 {
			t.Fatalf("PRAGMA foreign_key_check returned %d violations, want 0", foreignKeyViolations)
		}
	})
}

func assertOutboxSendAgainMigration(t *testing.T, store *Store) {
	t.Helper()
	assertPragmaInt(t, store.db, "user_version", 11)
	ledger := readLedgerRow(t, store.db, 9)
	if ledger.name != "outbox_send_again" {
		t.Fatalf("migration 0009 name = %q, want outbox_send_again", ledger.name)
	}
	if ledger.checksum != embeddedMigrations[8].checksumSHA256 {
		t.Fatalf(
			"migration 0009 checksum = %q, want %q",
			ledger.checksum,
			embeddedMigrations[8].checksumSHA256,
		)
	}

	var columnType string
	var notNull int
	var defaultValue sql.NullString
	var primaryKey int
	if err := store.db.QueryRow(`
		SELECT type, "notnull", dflt_value, pk
		FROM pragma_table_info('outbox')
		WHERE name = 'send_again_of_outbox_id'
	`).Scan(&columnType, &notNull, &defaultValue, &primaryKey); err != nil {
		t.Fatalf("inspect send_again_of_outbox_id: %v", err)
	}
	if columnType != "TEXT" || notNull != 0 || defaultValue.Valid || primaryKey != 0 {
		t.Fatalf(
			"send_again_of_outbox_id schema = type %q, notnull %d, default %v, pk %d",
			columnType,
			notNull,
			defaultValue,
			primaryKey,
		)
	}

	var partial int
	if err := store.db.QueryRow(`
		SELECT partial
		FROM pragma_index_list('outbox')
		WHERE name = 'outbox_send_again_of_idx'
	`).Scan(&partial); err != nil {
		t.Fatalf("inspect outbox_send_again_of_idx: %v", err)
	}
	if partial != 1 {
		t.Fatalf("outbox_send_again_of_idx partial = %d, want 1", partial)
	}
}

func TestOutboxAttachmentsMigrationAppliesToBlankAndExistingV5Database(t *testing.T) {
	t.Run("blank", func(t *testing.T) {
		store, err := Open(filepath.Join(t.TempDir(), "store.sqlite3"))
		if err != nil {
			t.Fatalf("Open(): %v", err)
		}
		t.Cleanup(func() {
			if err := store.Close(); err != nil {
				t.Errorf("Close(): %v", err)
			}
		})
		assertOutboxAttachmentsMigration(t, store)
	})

	t.Run("existing v5", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "store.sqlite3")
		db, err := sql.Open("sqlite", storeDSN(path))
		if err != nil {
			t.Fatalf("sql.Open(): %v", err)
		}
		if err := db.Ping(); err != nil {
			_ = db.Close()
			t.Fatalf("Ping(): %v", err)
		}
		if err := enableWAL(context.Background(), db); err != nil {
			_ = db.Close()
			t.Fatalf("enableWAL(): %v", err)
		}
		if err := verifyConnectionPragmas(context.Background(), db); err != nil {
			_ = db.Close()
			t.Fatalf("verifyConnectionPragmas(): %v", err)
		}
		if err := runMigrations(context.Background(), db, embeddedMigrations[:5]); err != nil {
			_ = db.Close()
			t.Fatalf("runMigrations(v5): %v", err)
		}
		before := readLedgerRows(t, db)
		if len(before) != 5 {
			_ = db.Close()
			t.Fatalf("v5 ledger rows = %d, want 5", len(before))
		}
		if err := db.Close(); err != nil {
			t.Fatalf("close v5 database: %v", err)
		}

		store, err := Open(path)
		if err != nil {
			t.Fatalf("Open(v5): %v", err)
		}
		t.Cleanup(func() {
			if err := store.Close(); err != nil {
				t.Errorf("Close(): %v", err)
			}
		})
		after := readLedgerRows(t, store.db)
		if len(after) != 11 {
			t.Fatalf("migrated ledger rows = %d, want 11", len(after))
		}
		if !slices.Equal(after[:5], before) {
			t.Fatalf("migrations 0001-0005 changed:\nbefore: %+v\nafter:  %+v", before, after[:5])
		}
		assertOutboxAttachmentsMigration(t, store)
	})
}

func assertOutboxAttachmentsMigration(t *testing.T, store *Store) {
	t.Helper()
	assertPragmaInt(t, store.db, "user_version", 11)
	ledger := readLedgerRow(t, store.db, 6)
	if ledger.name != "outbox_attachments" {
		t.Fatalf("migration 0006 name = %q, want outbox_attachments", ledger.name)
	}
	if ledger.checksum != embeddedMigrations[5].checksumSHA256 {
		t.Fatalf(
			"migration 0006 checksum = %q, want %q",
			ledger.checksum,
			embeddedMigrations[5].checksumSHA256,
		)
	}
	var strict int
	if err := store.db.QueryRow(`
		SELECT strict
		FROM pragma_table_list
		WHERE schema = 'main' AND name = 'outbox_attachments'
	`).Scan(&strict); err != nil {
		t.Fatalf("read outbox_attachments STRICT flag: %v", err)
	}
	if strict != 1 {
		t.Fatalf("outbox_attachments strict = %d, want 1", strict)
	}
}

func TestReactionsReadMigrationAppliesToBlankAndReopens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.sqlite3")
	first, err := Open(path)
	if err != nil {
		t.Fatalf("first Open(): %v", err)
	}
	assertReactionsReadMigration(t, first)
	firstLedger := readLedgerRows(t, first.db)
	if err := first.Close(); err != nil {
		t.Fatalf("first Close(): %v", err)
	}

	second, err := Open(path)
	if err != nil {
		t.Fatalf("second Open(): %v", err)
	}
	t.Cleanup(func() {
		if err := second.Close(); err != nil {
			t.Errorf("second Close(): %v", err)
		}
	})
	assertReactionsReadMigration(t, second)
	secondLedger := readLedgerRows(t, second.db)
	if !slices.Equal(secondLedger, firstLedger) {
		t.Fatalf("migration ledger changed on reopen:\nfirst:  %+v\nsecond: %+v", firstLedger, secondLedger)
	}
}

func assertReactionsReadMigration(t *testing.T, store *Store) {
	t.Helper()
	assertPragmaInt(t, store.db, "user_version", 11)
	ledger := readLedgerRow(t, store.db, 7)
	if ledger.name != "reactions_read" {
		t.Fatalf("migration 0007 name = %q, want reactions_read", ledger.name)
	}
	if ledger.checksum != embeddedMigrations[6].checksumSHA256 {
		t.Fatalf(
			"migration 0007 checksum = %q, want %q",
			ledger.checksum,
			embeddedMigrations[6].checksumSHA256,
		)
	}
	for _, table := range []string{"outbox_reactions", "outbox_read_receipts", "read_cursors"} {
		var strict int
		if err := store.db.QueryRow(`
			SELECT strict
			FROM pragma_table_list
			WHERE schema = 'main' AND name = ?
		`, table).Scan(&strict); err != nil {
			t.Fatalf("read %s STRICT flag: %v", table, err)
		}
		if strict != 1 {
			t.Fatalf("%s strict = %d, want 1", table, strict)
		}
	}
	for _, index := range []string{
		"messages_conversation_message_uq",
		"outbox_reactions_target_idx",
		"outbox_read_receipts_message_idx",
		"read_cursors_conversation_idx",
	} {
		var exists bool
		if err := store.db.QueryRow(`
			SELECT EXISTS (
				SELECT 1 FROM sqlite_schema
				WHERE type = 'index' AND name = ?
			)
		`, index).Scan(&exists); err != nil {
			t.Fatalf("inspect migration index %q: %v", index, err)
		}
		if !exists {
			t.Fatalf("migration index %q is missing", index)
		}
	}
}

type outboxTestClock struct {
	mu    sync.Mutex
	nowMS int64
}

func newOutboxTestClock(nowMS int64) *outboxTestClock {
	return &outboxTestClock{nowMS: nowMS}
}

func (c *outboxTestClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.UnixMilli(c.nowMS)
}

func (c *outboxTestClock) Set(nowMS int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nowMS = nowMS
}

func openOutboxTestRepository(
	t *testing.T,
	now func() time.Time,
) (*Store, *OutboxRepository) {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "store.sqlite3"))
	if err != nil {
		t.Fatalf("Open(): %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close(): %v", err)
		}
	})
	seedMessageAccount(t, store, "account-a", "test")
	repository, err := NewOutboxRepository(store, now)
	if err != nil {
		t.Fatalf("NewOutboxRepository(): %v", err)
	}
	return store, repository
}

func outboxTestItem(id string) NewOutboxItem {
	return NewOutboxItem{
		OutboxID:           "outbox-" + id,
		AccountID:          "account-a",
		ConversationID:     "conversation-a",
		Kind:               OutboxKindText,
		IdempotencyKey:     "idempotency-" + id,
		PayloadHash:        "payload-hash-" + id,
		Operation:          "send_text",
		LocalMessageID:     "message-" + id,
		TransportRequestID: "request-" + id,
	}
}

func outboxTestMediaItem(id string) NewOutboxItem {
	item := outboxTestItem(id)
	item.Kind = OutboxKindMedia
	item.Operation = "send_media"
	return item
}

func outboxTestReactionItem(id string) NewOutboxItem {
	item := outboxTestItem(id)
	item.Kind = OutboxKindReaction
	item.Operation = "reaction"
	item.LocalMessageID = ""
	return item
}

func outboxTestReadItem(id string) NewOutboxItem {
	item := outboxTestItem(id)
	item.Kind = OutboxKindRead
	item.Operation = "read_receipt"
	item.LocalMessageID = ""
	return item
}

func outboxTestAttachment() OutboxAttachment {
	return OutboxAttachment{
		BlobHash:  "abababababababababababababababababababababababababababababababab",
		SizeBytes: 42,
		MIME:      "image/png",
		Filename:  "photo.png",
	}
}

func outboxTestOutgoingMessage(item NewOutboxItem, body string) Message {
	senderIdentityID := "identity-a"
	return Message{
		MessageID:        item.LocalMessageID,
		ConversationID:   item.ConversationID,
		AccountID:        item.AccountID,
		RemoteMessageID:  item.TransportRequestID,
		SenderIdentityID: &senderIdentityID,
		Direction:        MessageDirectionOutgoing,
		Body:             body,
		State:            MessageStateActive,
		OccurredAtMS:     outboxTestTimeMS,
	}
}

func seedOutboxTestDevice(t *testing.T, store *Store, deviceID, accountID string) {
	t.Helper()
	mustRepositoryWrite(t, "seed UpsertDevice", store.UpsertDevice(Device{
		DeviceID:    deviceID,
		AccountID:   accountID,
		Kind:        DeviceKindLocalInstallation,
		State:       DeviceStateActive,
		CreatedAtMS: outboxTestTimeMS,
		UpdatedAtMS: outboxTestTimeMS,
	}))
}

func seedOutboxTestMessage(
	t *testing.T,
	store *Store,
	messageID, accountID, conversationID string,
) {
	t.Helper()
	mustExec(t, store.db, `
		INSERT INTO messages (
			message_id,
			conversation_id,
			account_id,
			remote_message_id,
			sender_identity_id,
			direction,
			body,
			reply_to_remote_id,
			state,
			occurred_at_ms,
			created_at_ms,
			updated_at_ms
		) VALUES (?, ?, ?, ?, NULL, 'incoming', '', NULL, 'active', ?, ?, ?)
	`,
		messageID,
		conversationID,
		accountID,
		"remote-"+messageID,
		outboxTestTimeMS-1_000,
		outboxTestTimeMS,
		outboxTestTimeMS,
	)
}

func mustEnqueueOutbox(
	t *testing.T,
	repository *OutboxRepository,
	item NewOutboxItem,
) OutboxItem {
	t.Helper()
	row, disposition, err := repository.Enqueue(context.Background(), item)
	if err != nil {
		t.Fatalf("Enqueue(%q): %v", item.OutboxID, err)
	}
	if disposition != EnqueueInserted {
		t.Fatalf("Enqueue(%q) disposition = %q, want %q", item.OutboxID, disposition, EnqueueInserted)
	}
	return row
}

func mustEnqueueOutgoingOutbox(
	t *testing.T,
	repository *OutboxRepository,
	item NewOutboxItem,
	body string,
) OutboxItem {
	t.Helper()
	row, disposition, err := repository.EnqueueOutgoingMessage(
		context.Background(),
		item,
		outboxTestOutgoingMessage(item, body),
	)
	if err != nil {
		t.Fatalf("EnqueueOutgoingMessage(%q): %v", item.OutboxID, err)
	}
	if disposition != EnqueueInserted {
		t.Fatalf(
			"EnqueueOutgoingMessage(%q) disposition = %q, want %q",
			item.OutboxID,
			disposition,
			EnqueueInserted,
		)
	}
	return row
}

func outboxIDs(items []OutboxItem) []string {
	ids := make([]string, len(items))
	for index, item := range items {
		ids[index] = item.OutboxID
	}
	return ids
}

func mustLeaseOne(
	t *testing.T,
	repository *OutboxRepository,
	req LeaseRequest,
) OutboxItem {
	t.Helper()
	leases, err := repository.LeaseDue(context.Background(), req)
	if err != nil {
		t.Fatalf("LeaseDue(): %v", err)
	}
	if len(leases) != 1 {
		t.Fatalf("LeaseDue() returned %d leases, want 1: %+v", len(leases), leases)
	}
	return leases[0].OutboxItem
}

func mustLeaseToken(t *testing.T, item OutboxItem) string {
	t.Helper()
	if item.LeaseToken == nil || *item.LeaseToken == "" {
		t.Fatalf("outbox item %q has no lease token: %+v", item.OutboxID, item)
	}
	return *item.LeaseToken
}

func assertOutboxText(t *testing.T, field string, got *string, want string) {
	t.Helper()
	if got == nil || *got != want {
		t.Fatalf("%s = %v, want %q", field, got, want)
	}
}

func stringsForOutboxID(value string) string {
	result := make([]byte, 0, len(value))
	for i := range len(value) {
		if value[i] == ' ' {
			result = append(result, '-')
			continue
		}
		result = append(result, value[i])
	}
	return string(result)
}
