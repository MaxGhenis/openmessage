package sqlite

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const messageTestTimeMS int64 = 1_900_000_000_000

func TestInboxAppendIsIdempotentByAccountDedupeKey(t *testing.T) {
	clock := newMessageTestClock(messageTestTimeMS)
	store, repository := openMessageTestRepository(t, clock.Now)
	seedMessageAccount(t, store, "account-a", "signal")
	seedMessageAccount(t, store, "account-b", "whatsapp")

	first := messageTestInbox("inbox-original", "account-a", "remote-event-1", []byte("original"))
	firstID, err := repository.AppendInbox(context.Background(), first)
	if err != nil {
		t.Fatalf("AppendInbox(first): %v", err)
	}
	if firstID != first.InboxID {
		t.Fatalf("AppendInbox(first) ID = %q, want %q", firstID, first.InboxID)
	}

	clock.Set(messageTestTimeMS + 100)
	duplicate := messageTestInbox("inbox-discarded", "account-a", first.DedupeKey, []byte("replacement"))
	duplicateID, err := repository.AppendInbox(context.Background(), duplicate)
	if err != nil {
		t.Fatalf("AppendInbox(duplicate): %v", err)
	}
	if duplicateID != first.InboxID {
		t.Fatalf("AppendInbox(duplicate) ID = %q, want existing %q", duplicateID, first.InboxID)
	}

	records, err := repository.Unprocessed(context.Background())
	if err != nil {
		t.Fatalf("Unprocessed(): %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("unprocessed inbox records = %d, want 1", len(records))
	}
	got := records[0]
	if got.InboxID != first.InboxID || got.ReceivedAtMS != messageTestTimeMS {
		t.Fatalf("deduplicated inbox row = %+v, want original ID and timestamp", got)
	}
	if !bytes.Equal(got.Payload, first.Payload) {
		t.Fatalf("deduplicated inbox payload = %q, want %q", got.Payload, first.Payload)
	}
	if got.ProcessedAtMS != nil {
		t.Fatalf("deduplicated inbox processed_at_ms = %v, want nil", got.ProcessedAtMS)
	}

	accountB := messageTestInbox("inbox-account-b", "account-b", first.DedupeKey, []byte("other account"))
	if _, err := repository.AppendInbox(context.Background(), accountB); err != nil {
		t.Fatalf("AppendInbox(same key, other account): %v", err)
	}
	for i := range 2 {
		record := messageTestInbox(fmt.Sprintf("inbox-no-dedupe-%d", i), "account-a", "", []byte{byte(i)})
		if _, err := repository.AppendInbox(context.Background(), record); err != nil {
			t.Fatalf("AppendInbox(empty dedupe %d): %v", i, err)
		}
	}
	assertRowCount(t, store.db, "inbox", 4)
	records, err = repository.Unprocessed(context.Background())
	if err != nil {
		t.Fatalf("Unprocessed() ordered records: %v", err)
	}
	wantOrder := []string{
		"inbox-original",
		"inbox-account-b",
		"inbox-no-dedupe-0",
		"inbox-no-dedupe-1",
	}
	if len(records) != len(wantOrder) {
		t.Fatalf("ordered unprocessed records = %d, want %d", len(records), len(wantOrder))
	}
	for i, want := range wantOrder {
		if records[i].InboxID != want {
			t.Fatalf("unprocessed record %d = %q, want %q", i, records[i].InboxID, want)
		}
	}
}

func TestAppendInboxConcurrentDuplicatesReturnOneID(t *testing.T) {
	store, repository := openMessageTestRepository(
		t,
		func() time.Time { return time.UnixMilli(messageTestTimeMS) },
	)
	seedMessageAccount(t, store, "account-a", "signal")

	const writers = 12
	start := make(chan struct{})
	ids := make(chan string, writers)
	errs := make(chan error, writers)
	var workers sync.WaitGroup
	for i := range writers {
		workers.Add(1)
		go func(i int) {
			defer workers.Done()
			<-start
			record := messageTestInbox(
				fmt.Sprintf("inbox-concurrent-%02d", i),
				"account-a",
				"one-remote-event",
				[]byte{byte(i)},
			)
			id, err := repository.AppendInbox(context.Background(), record)
			ids <- id
			errs <- err
		}(i)
	}
	close(start)
	workers.Wait()
	close(ids)
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent AppendInbox(): %v", err)
		}
	}
	var effectiveID string
	for id := range ids {
		if effectiveID == "" {
			effectiveID = id
		}
		if id != effectiveID {
			t.Fatalf("concurrent AppendInbox() ID = %q, want common ID %q", id, effectiveID)
		}
	}
	assertRowCount(t, store.db, "inbox", 1)
}

func TestAppendInboxMapsSQLiteConstraints(t *testing.T) {
	store, repository := openMessageTestRepository(
		t,
		func() time.Time { return time.UnixMilli(messageTestTimeMS) },
	)
	record := messageTestInbox("inbox-orphan", "account-missing", "event-orphan", []byte("frame"))
	_, err := repository.AppendInbox(context.Background(), record)
	for _, want := range []error{
		ErrInvalidInboxRecord,
		ErrConstraintViolation,
		ErrOrphanInboxAccount,
	} {
		if !errors.Is(err, want) {
			t.Fatalf("AppendInbox(orphan account) error = %v, want %v", err, want)
		}
	}
	assertRowCount(t, store.db, "inbox", 0)
}

func TestImportMessageSupportsBothDirectionsWithoutInbox(t *testing.T) {
	store, repository := openMessageTestRepository(
		t,
		func() time.Time { return time.UnixMilli(messageTestTimeMS) },
	)
	seedMessageProjectionGraph(t, store)

	incoming := messageTestMessage(
		"message-import-incoming",
		"conversation-a",
		"account-a",
		"remote-import-incoming",
		pointer("identity-a"),
	)
	if err := repository.ImportMessage(context.Background(), MessageProjection{
		InboxID: "inbox-that-does-not-exist",
		Message: incoming,
	}); err != nil {
		t.Fatalf("ImportMessage(incoming): %v", err)
	}

	outgoing := messageTestMessage(
		"message-import-outgoing",
		"conversation-a",
		"account-a",
		"remote-import-outgoing",
		nil,
	)
	outgoing.Direction = MessageDirectionOutgoing
	outgoing.Body = "historical outgoing message"
	if err := repository.ImportMessage(context.Background(), MessageProjection{
		Message: outgoing,
	}); err != nil {
		t.Fatalf("ImportMessage(outgoing): %v", err)
	}

	assertRowCount(t, store.db, "inbox", 0)
	assertRowCount(t, store.db, "messages", 2)
	for _, want := range []Message{incoming, outgoing} {
		got, err := repository.GetMessage(context.Background(), want.MessageID)
		if err != nil {
			t.Fatalf("GetMessage(%q): %v", want.MessageID, err)
		}
		if got.Direction != want.Direction ||
			got.Body != want.Body ||
			!optionalStringEqual(got.SenderIdentityID, want.SenderIdentityID) ||
			got.CreatedAtMS != messageTestTimeMS ||
			got.UpdatedAtMS != messageTestTimeMS {
			t.Fatalf("imported message = %+v, want normalized fields from %+v", got, want)
		}
	}
}

func TestListMessagesByConversationPaginationAndAround(t *testing.T) {
	store, repository := openMessageTestRepository(
		t,
		func() time.Time { return time.UnixMilli(messageTestTimeMS) },
	)
	seedMessageAccount(t, store, "account-a", "google_messages")
	seedMessageAccount(t, store, "account-b", "whatsmeow")
	seedMessageConversation(t, store, "conversation-a", "account-a")
	seedMessageConversation(t, store, "conversation-b", "account-b")

	importMessage := func(id, conversationID, accountID string, occurredAtMS int64) {
		t.Helper()
		message := messageTestMessage(id, conversationID, accountID, "remote-"+id, nil)
		message.OccurredAtMS = occurredAtMS
		if err := repository.ImportMessage(context.Background(), MessageProjection{Message: message}); err != nil {
			t.Fatalf("ImportMessage(%q): %v", id, err)
		}
	}
	for _, item := range []struct {
		id string
		ts int64
	}{
		{id: "message-old", ts: 100},
		{id: "message-tie-a", ts: 200},
		{id: "message-tie-b", ts: 200},
		{id: "message-new", ts: 300},
		{id: "message-newest", ts: 400},
	} {
		importMessage(item.id, "conversation-a", "account-a", item.ts)
	}
	importMessage("message-other", "conversation-b", "account-b", 1_000)

	assertIDs := func(name string, got []Message, want ...string) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("%s IDs = %d rows %+v, want %d %v", name, len(got), got, len(want), want)
		}
		for i, wantID := range want {
			if got[i].MessageID != wantID {
				t.Fatalf("%s ID %d = %q, want %q", name, i, got[i].MessageID, wantID)
			}
		}
	}

	latest, err := repository.ListMessagesByConversation(
		context.Background(), "conversation-a", 0, "", 3,
	)
	if err != nil {
		t.Fatalf("ListMessagesByConversation(latest): %v", err)
	}
	assertIDs("latest", latest, "message-newest", "message-new", "message-tie-b")

	before, err := repository.ListMessagesByConversation(
		context.Background(), "conversation-a", 200, "message-tie-b", 10,
	)
	if err != nil {
		t.Fatalf("ListMessagesByConversation(before cursor): %v", err)
	}
	assertIDs("before cursor", before, "message-tie-a", "message-old")

	strictlyBefore, err := repository.ListMessagesByConversation(
		context.Background(), "conversation-a", 200, "", 10,
	)
	if err != nil {
		t.Fatalf("ListMessagesByConversation(before timestamp): %v", err)
	}
	assertIDs("before timestamp", strictlyBefore, "message-old")

	after, err := repository.ListMessagesByConversationAfter(
		context.Background(), "conversation-a", 200, "message-tie-a", 10,
	)
	if err != nil {
		t.Fatalf("ListMessagesByConversationAfter(cursor): %v", err)
	}
	assertIDs("after cursor", after, "message-tie-b", "message-new", "message-newest")

	strictlyAfter, err := repository.ListMessagesByConversationAfter(
		context.Background(), "conversation-a", 200, "", 10,
	)
	if err != nil {
		t.Fatalf("ListMessagesByConversationAfter(timestamp): %v", err)
	}
	assertIDs("after timestamp", strictlyAfter, "message-new", "message-newest")

	around, err := repository.ListMessagesAroundMessage(
		context.Background(), "conversation-a", "message-tie-b", 2, 2,
	)
	if err != nil {
		t.Fatalf("ListMessagesAroundMessage(): %v", err)
	}
	assertIDs(
		"around",
		around,
		"message-old",
		"message-tie-a",
		"message-tie-b",
		"message-new",
		"message-newest",
	)

	for _, test := range []struct {
		name           string
		conversationID string
		messageID      string
	}{
		{name: "missing anchor", conversationID: "conversation-a", messageID: "missing"},
		{name: "wrong conversation", conversationID: "conversation-b", messageID: "message-tie-b"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := repository.ListMessagesAroundMessage(
				context.Background(), test.conversationID, test.messageID, 1, 1,
			)
			if !errors.Is(err, ErrNotFound) {
				t.Fatalf("ListMessagesAroundMessage() error = %v, want ErrNotFound", err)
			}
		})
	}
}

func TestSearchMessagesLIKEFiltersAndOrdersDeterministically(t *testing.T) {
	store, repository := openMessageTestRepository(
		t,
		func() time.Time { return time.UnixMilli(messageTestTimeMS) },
	)
	seedMessageAccount(t, store, "account-a", "google_messages")
	seedMessageAccount(t, store, "account-b", "whatsmeow")
	seedMessageConversation(t, store, "conversation-a", "account-a")
	seedMessageConversation(t, store, "conversation-a-other", "account-a")
	seedMessageConversation(t, store, "conversation-b", "account-b")

	identities := []Identity{
		repositoryTestIdentity("alice-a", "account-a", "+15550000001"),
		repositoryTestIdentity("bob-a", "account-a", "+15550000002"),
		repositoryTestIdentity("alice-b", "account-b", "+15550000001"),
	}
	for _, identity := range identities {
		mustRepositoryWrite(t, "UpsertIdentity", store.UpsertIdentity(identity))
	}

	importMessage := func(id, conversationID, accountID, body, senderID string, occurredAtMS int64) {
		t.Helper()
		message := messageTestMessage(
			id,
			conversationID,
			accountID,
			"remote-"+id,
			pointer(senderID),
		)
		message.Body = body
		message.OccurredAtMS = occurredAtMS
		if err := repository.ImportMessage(context.Background(), MessageProjection{Message: message}); err != nil {
			t.Fatalf("ImportMessage(%q): %v", id, err)
		}
	}
	importMessage("message-old", "conversation-a", "account-a", "Needle oldest", "alice-a", 100)
	importMessage("message-middle", "conversation-a", "account-a", "needle middle", "bob-a", 200)
	importMessage("message-new", "conversation-a", "account-a", "NEEDLE newest", "alice-a", 300)
	importMessage("message-other-conversation", "conversation-a-other", "account-a", "needle elsewhere", "alice-a", 400)
	importMessage("message-other-account", "conversation-b", "account-b", "needle cross-account", "alice-b", 500)
	importMessage("message-no-match", "conversation-b", "account-b", "haystack", "alice-b", 600)

	assertSearch := func(name string, query SearchQuery, want ...string) {
		t.Helper()
		got, err := repository.SearchMessages(context.Background(), "needle", query)
		if err != nil {
			t.Fatalf("%s SearchMessages(): %v", name, err)
		}
		if len(got) != len(want) {
			t.Fatalf("%s results = %+v, want IDs %v", name, got, want)
		}
		for i, wantID := range want {
			if got[i].MessageID != wantID {
				t.Fatalf("%s result %d = %q, want %q", name, i, got[i].MessageID, wantID)
			}
		}
	}

	assertSearch(
		"global limit",
		SearchQuery{Limit: 3},
		"message-other-account",
		"message-other-conversation",
		"message-new",
	)
	assertSearch(
		"account",
		SearchQuery{AccountID: "account-a", Limit: 10},
		"message-other-conversation",
		"message-new",
		"message-middle",
		"message-old",
	)
	assertSearch(
		"conversation",
		SearchQuery{ConversationID: "conversation-a", Limit: 10},
		"message-new",
		"message-middle",
		"message-old",
	)
	assertSearch(
		"sender canonical value",
		SearchQuery{SenderCanonicalValue: "+15550000001", Limit: 10},
		"message-other-account",
		"message-other-conversation",
		"message-new",
		"message-old",
	)
	assertSearch(
		"time range",
		SearchQuery{SinceMS: 150, UntilMS: 350, Limit: 10},
		"message-new",
		"message-middle",
	)
	assertSearch(
		"reversed time range",
		SearchQuery{SinceMS: 350, UntilMS: 150, Limit: 10},
		"message-new",
		"message-middle",
	)
}

func TestImportMessageIsIdempotentByRemoteIDAndPersistsAttachments(t *testing.T) {
	clock := newMessageTestClock(messageTestTimeMS)
	store, repository := openMessageTestRepository(t, clock.Now)
	seedMessageProjectionGraph(t, store)

	message := messageTestMessage(
		"message-import-original",
		"conversation-a",
		"account-a",
		"remote-import-shared",
		pointer("identity-a"),
	)
	firstSize := int64(123)
	firstAttachment := MessageAttachment{
		MessageID: "caller-supplied-parent-is-ignored",
		Ordinal:   0,
		RemoteID:  "remote-media-first",
		RemoteRef: []byte("opaque-first"),
		Filename:  "first.png",
		MIME:      "image/png",
		SizeBytes: &firstSize,
	}
	if err := repository.ImportMessage(context.Background(), MessageProjection{
		Message:     message,
		Attachments: []MessageAttachment{firstAttachment},
	}); err != nil {
		t.Fatalf("ImportMessage(first): %v", err)
	}

	clock.Set(messageTestTimeMS + 100)
	updated := message
	updated.MessageID = "message-import-discarded"
	updated.Body = "edited historical body"
	updated.ReplyToRemoteID = pointer("remote-parent")
	updated.State = MessageStateEdited
	updated.OccurredAtMS++
	secondSize := int64(456)
	secondAttachment := MessageAttachment{
		MessageID: "another-untrusted-parent",
		Ordinal:   0,
		RemoteID:  "remote-media-second",
		RemoteRef: []byte("opaque-second"),
		Filename:  "second.jpg",
		MIME:      "image/jpeg",
		SizeBytes: &secondSize,
	}
	if err := repository.ImportMessage(context.Background(), MessageProjection{
		InboxID:     "still-not-required",
		Message:     updated,
		Attachments: []MessageAttachment{secondAttachment},
	}); err != nil {
		t.Fatalf("ImportMessage(upsert): %v", err)
	}

	assertRowCount(t, store.db, "messages", 1)
	assertRowCount(t, store.db, "message_attachments", 1)
	got, err := repository.GetMessage(context.Background(), message.MessageID)
	if err != nil {
		t.Fatalf("GetMessage(): %v", err)
	}
	if got.MessageID != message.MessageID ||
		got.Body != updated.Body ||
		got.State != MessageStateEdited ||
		got.ReplyToRemoteID == nil ||
		*got.ReplyToRemoteID != *updated.ReplyToRemoteID ||
		got.CreatedAtMS != messageTestTimeMS ||
		got.UpdatedAtMS != messageTestTimeMS+100 {
		t.Fatalf("upserted imported message = %+v, want stable ID/creation and updated content", got)
	}

	attachmentRepository, err := NewMessageAttachmentRepository(store, clock.Now)
	if err != nil {
		t.Fatalf("NewMessageAttachmentRepository(): %v", err)
	}
	attachment, err := attachmentRepository.GetForDownload(
		context.Background(), message.MessageID, secondAttachment.Ordinal,
	)
	if err != nil {
		t.Fatalf("GetForDownload(): %v", err)
	}
	if attachment.MessageID != message.MessageID ||
		attachment.State != "pending" ||
		attachment.BlobHash != nil ||
		attachment.RemoteID != secondAttachment.RemoteID ||
		!bytes.Equal(attachment.RemoteRef, secondAttachment.RemoteRef) ||
		attachment.Filename != secondAttachment.Filename ||
		attachment.MIME != secondAttachment.MIME ||
		attachment.SizeBytes == nil ||
		*attachment.SizeBytes != secondSize {
		t.Fatalf("imported attachment = %+v, want refreshed metadata %+v", attachment, secondAttachment)
	}

	clock.Set(messageTestTimeMS + 200)
	if err := repository.ImportMessage(context.Background(), MessageProjection{Message: updated}); err != nil {
		t.Fatalf("ImportMessage(idempotent replay): %v", err)
	}
	replayed, err := repository.GetMessage(context.Background(), message.MessageID)
	if err != nil {
		t.Fatalf("GetMessage() after replay: %v", err)
	}
	if replayed.UpdatedAtMS != messageTestTimeMS+100 {
		t.Fatalf(
			"idempotent replay updated_at_ms = %d, want unchanged %d",
			replayed.UpdatedAtMS,
			messageTestTimeMS+100,
		)
	}
	assertRowCount(t, store.db, "inbox", 0)
}

func TestProjectMessageIsIdempotentByRemoteID(t *testing.T) {
	clock := newMessageTestClock(messageTestTimeMS)
	store, repository := openMessageTestRepository(t, clock.Now)
	seedMessageProjectionGraph(t, store)

	firstInbox := messageTestInbox("inbox-1", "account-a", "event-1", []byte("frame 1"))
	if _, err := repository.AppendInbox(context.Background(), firstInbox); err != nil {
		t.Fatalf("AppendInbox(first): %v", err)
	}
	message := messageTestMessage("message-original", "conversation-a", "account-a", "remote-message-1", pointer("identity-a"))
	if err := repository.ProjectMessage(context.Background(), MessageProjection{
		InboxID: firstInbox.InboxID,
		Message: message,
	}); err != nil {
		t.Fatalf("ProjectMessage(first): %v", err)
	}

	clock.Set(messageTestTimeMS + 1_000)
	if err := repository.ProjectMessage(context.Background(), MessageProjection{
		InboxID: firstInbox.InboxID,
		Message: message,
	}); err != nil {
		t.Fatalf("ProjectMessage(replay same inbox): %v", err)
	}

	secondInbox := messageTestInbox("inbox-2", "account-a", "event-2", []byte("frame 2"))
	if _, err := repository.AppendInbox(context.Background(), secondInbox); err != nil {
		t.Fatalf("AppendInbox(second): %v", err)
	}
	duplicateMessage := message
	duplicateMessage.MessageID = "message-discarded"
	if err := repository.ProjectMessage(context.Background(), MessageProjection{
		InboxID: secondInbox.InboxID,
		Message: duplicateMessage,
	}); err != nil {
		t.Fatalf("ProjectMessage(same remote ID): %v", err)
	}

	assertRowCount(t, store.db, "messages", 1)
	got, err := repository.GetMessage(context.Background(), message.MessageID)
	if err != nil {
		t.Fatalf("GetMessage(): %v", err)
	}
	if got.MessageID != message.MessageID {
		t.Fatalf("projected message ID = %q, want original %q", got.MessageID, message.MessageID)
	}
	if got.CreatedAtMS != messageTestTimeMS || got.UpdatedAtMS != messageTestTimeMS {
		t.Fatalf(
			"projected timestamps = (%d, %d), want unchanged (%d, %d)",
			got.CreatedAtMS,
			got.UpdatedAtMS,
			messageTestTimeMS,
			messageTestTimeMS,
		)
	}
	assertInboxProcessedAt(t, store, firstInbox.InboxID, messageTestTimeMS)
	assertInboxProcessedAt(t, store, secondInbox.InboxID, messageTestTimeMS+1_000)

	clock.Set(messageTestTimeMS + 2_000)
	thirdInbox := messageTestInbox("inbox-3", "account-a", "event-3", []byte("frame 3"))
	if _, err := repository.AppendInbox(context.Background(), thirdInbox); err != nil {
		t.Fatalf("AppendInbox(third): %v", err)
	}
	updatedMessage := message
	updatedMessage.MessageID = "message-second-discarded"
	updatedMessage.Body = "edited body"
	updatedMessage.ReplyToRemoteID = pointer("remote-parent")
	updatedMessage.State = MessageStateEdited
	updatedMessage.OccurredAtMS++
	if err := repository.ProjectMessage(context.Background(), MessageProjection{
		InboxID: thirdInbox.InboxID,
		Message: updatedMessage,
	}); err != nil {
		t.Fatalf("ProjectMessage(changed same remote ID): %v", err)
	}
	got, err = repository.GetMessage(context.Background(), message.MessageID)
	if err != nil {
		t.Fatalf("GetMessage() after changed upsert: %v", err)
	}
	if got.MessageID != message.MessageID || got.CreatedAtMS != messageTestTimeMS {
		t.Fatalf("changed upsert replaced stable identity/creation: %+v", got)
	}
	if got.UpdatedAtMS != messageTestTimeMS+2_000 ||
		got.Body != updatedMessage.Body ||
		got.State != MessageStateEdited ||
		got.ReplyToRemoteID == nil ||
		*got.ReplyToRemoteID != *updatedMessage.ReplyToRemoteID {
		t.Fatalf("changed upsert message = %+v, want updated normalized fields", got)
	}
	assertInboxProcessedAt(t, store, thirdInbox.InboxID, messageTestTimeMS+2_000)
	unprocessed, err := repository.Unprocessed(context.Background())
	if err != nil {
		t.Fatalf("Unprocessed(): %v", err)
	}
	if len(unprocessed) != 0 {
		t.Fatalf("unprocessed inbox records = %d, want 0", len(unprocessed))
	}
}

func TestProcessedInboxReplayStillValidatesMessage(t *testing.T) {
	store, repository := openMessageTestRepository(
		t,
		func() time.Time { return time.UnixMilli(messageTestTimeMS) },
	)
	seedMessageProjectionGraph(t, store)
	inbox := messageTestInbox("inbox-validated-replay", "account-a", "event-validated-replay", []byte("frame"))
	if _, err := repository.AppendInbox(context.Background(), inbox); err != nil {
		t.Fatalf("AppendInbox(): %v", err)
	}
	message := messageTestMessage("message-validated-replay", "conversation-a", "account-a", "remote-validated-replay", pointer("identity-a"))
	projection := MessageProjection{InboxID: inbox.InboxID, Message: message}
	if err := repository.ProjectMessage(context.Background(), projection); err != nil {
		t.Fatalf("ProjectMessage(first): %v", err)
	}

	orphanReplay := message
	orphanReplay.SenderIdentityID = pointer("identity-missing")
	err := repository.ProjectMessage(context.Background(), MessageProjection{
		InboxID: inbox.InboxID,
		Message: orphanReplay,
	})
	for _, want := range []error{
		ErrInvalidMessage,
		ErrConstraintViolation,
		ErrOrphanMessage,
		ErrOrphanMessageIdentity,
	} {
		if !errors.Is(err, want) {
			t.Fatalf("ProjectMessage(orphan replay) error = %v, want %v", err, want)
		}
	}

	changedReplay := message
	changedReplay.Body = "different content for processed inbox"
	err = repository.ProjectMessage(context.Background(), MessageProjection{
		InboxID: inbox.InboxID,
		Message: changedReplay,
	})
	for _, want := range []error{ErrInvalidMessage, ErrInboxProjectionConflict} {
		if !errors.Is(err, want) {
			t.Fatalf("ProjectMessage(changed replay) error = %v, want %v", err, want)
		}
	}

	differentMessage := message
	differentMessage.MessageID = "message-different-replay"
	differentMessage.RemoteMessageID = "remote-different-replay"
	err = repository.ProjectMessage(context.Background(), MessageProjection{
		InboxID: inbox.InboxID,
		Message: differentMessage,
	})
	for _, want := range []error{ErrInvalidMessage, ErrInboxProjectionConflict} {
		if !errors.Is(err, want) {
			t.Fatalf("ProjectMessage(different replay) error = %v, want %v", err, want)
		}
	}
	assertRowCount(t, store.db, "messages", 1)
	assertInboxProcessedAt(t, store, inbox.InboxID, messageTestTimeMS)
	got, err := repository.GetMessage(context.Background(), message.MessageID)
	if err != nil {
		t.Fatalf("GetMessage() after rejected replays: %v", err)
	}
	if got.SenderIdentityID == nil || *got.SenderIdentityID != "identity-a" {
		t.Fatalf("sender after rejected replays = %v, want identity-a", got.SenderIdentityID)
	}
	if got.Body != message.Body {
		t.Fatalf("body after rejected replays = %q, want %q", got.Body, message.Body)
	}
}

func TestInboxProjectionCrashReplaySurvivesReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "store.sqlite3")
	firstStore, err := Open(path)
	if err != nil {
		t.Fatalf("first Open(): %v", err)
	}
	seedMessageProjectionGraph(t, firstStore)
	firstRepository := mustMessageRepository(t, firstStore, messageTestTimeMS)
	record := messageTestInbox("inbox-replay", "account-a", "event-replay", []byte("durable raw frame"))
	if _, err := firstRepository.AppendInbox(ctx, record); err != nil {
		_ = firstStore.Close()
		t.Fatalf("AppendInbox(): %v", err)
	}
	if err := firstStore.Close(); err != nil {
		t.Fatalf("close after inbox append: %v", err)
	}

	secondStore, err := Open(path)
	if err != nil {
		t.Fatalf("second Open(): %v", err)
	}
	secondRepository := mustMessageRepository(t, secondStore, messageTestTimeMS+500)
	pending, err := secondRepository.Unprocessed(ctx)
	if err != nil {
		_ = secondStore.Close()
		t.Fatalf("Unprocessed() after restart: %v", err)
	}
	if len(pending) != 1 || !bytes.Equal(pending[0].Payload, record.Payload) {
		_ = secondStore.Close()
		t.Fatalf("pending records after restart = %+v, want durable raw frame", pending)
	}
	message := messageTestMessage("message-replay", "conversation-a", "account-a", "remote-replay", pointer("identity-a"))
	projection := MessageProjection{InboxID: record.InboxID, Message: message}
	if err := secondRepository.ProjectMessage(ctx, projection); err != nil {
		_ = secondStore.Close()
		t.Fatalf("ProjectMessage() after restart: %v", err)
	}
	if err := secondStore.Close(); err != nil {
		t.Fatalf("close after projection: %v", err)
	}

	thirdStore, err := Open(path)
	if err != nil {
		t.Fatalf("third Open(): %v", err)
	}
	t.Cleanup(func() {
		if err := thirdStore.Close(); err != nil {
			t.Errorf("third Close(): %v", err)
		}
	})
	thirdRepository := mustMessageRepository(t, thirdStore, messageTestTimeMS+5_000)
	if err := thirdRepository.ProjectMessage(ctx, projection); err != nil {
		t.Fatalf("ProjectMessage() replay after second restart: %v", err)
	}
	assertRowCount(t, thirdStore.db, "messages", 1)
	assertInboxProcessedAt(t, thirdStore, record.InboxID, messageTestTimeMS+500)
	got, err := thirdRepository.GetMessage(ctx, message.MessageID)
	if err != nil {
		t.Fatalf("GetMessage() after replay: %v", err)
	}
	if got.CreatedAtMS != messageTestTimeMS+500 || got.UpdatedAtMS != messageTestTimeMS+500 {
		t.Fatalf("message timestamps after replay = (%d, %d), want first projection time", got.CreatedAtMS, got.UpdatedAtMS)
	}
}

func TestProjectMessageRollsBackMessageWhenInboxMarkFails(t *testing.T) {
	clock := newMessageTestClock(messageTestTimeMS)
	store, repository := openMessageTestRepository(t, clock.Now)
	seedMessageProjectionGraph(t, store)
	inbox := messageTestInbox("inbox-clock-rollback", "account-a", "event-clock-rollback", []byte("frame"))
	if _, err := repository.AppendInbox(context.Background(), inbox); err != nil {
		t.Fatalf("AppendInbox(): %v", err)
	}

	// The inbox CHECK rejects a processed time before receipt. The preceding
	// valid message upsert must roll back with that failed update.
	clock.Set(messageTestTimeMS - 1)
	message := messageTestMessage("message-clock-rollback", "conversation-a", "account-a", "remote-clock-rollback", pointer("identity-a"))
	err := repository.ProjectMessage(context.Background(), MessageProjection{
		InboxID: inbox.InboxID,
		Message: message,
	})
	for _, want := range []error{ErrInvalidMessage, ErrConstraintViolation} {
		if !errors.Is(err, want) {
			t.Fatalf("ProjectMessage() error = %v, want %v", err, want)
		}
	}
	assertRowCount(t, store.db, "messages", 0)
	assertInboxUnprocessed(t, store, inbox.InboxID)
}

func TestProjectMessagePersistsAttachmentsInProjectionTransaction(t *testing.T) {
	clock := newMessageTestClock(messageTestTimeMS)
	store, repository := openMessageTestRepository(t, clock.Now)
	seedMessageProjectionGraph(t, store)
	inbox := messageTestInbox("inbox-attachment", "account-a", "event-attachment", []byte("frame"))
	if _, err := repository.AppendInbox(context.Background(), inbox); err != nil {
		t.Fatalf("AppendInbox(): %v", err)
	}
	message := messageTestMessage(
		"message-attachment", "conversation-a", "account-a", "remote-attachment", pointer("identity-a"),
	)
	size := int64(321)
	attachment := MessageAttachment{
		MessageID: message.MessageID,
		Ordinal:   0,
		RemoteID:  "remote-media-1",
		RemoteRef: []byte("opaque-ref"),
		Filename:  "photo.png",
		MIME:      "image/png",
		SizeBytes: &size,
	}
	if err := repository.ProjectMessage(context.Background(), MessageProjection{
		InboxID:     inbox.InboxID,
		Message:     message,
		Attachments: []MessageAttachment{attachment},
	}); err != nil {
		t.Fatalf("ProjectMessage(): %v", err)
	}

	attachmentRepository, err := NewMessageAttachmentRepository(store, clock.Now)
	if err != nil {
		t.Fatalf("NewMessageAttachmentRepository(): %v", err)
	}
	got, err := attachmentRepository.GetForDownload(context.Background(), message.MessageID, 0)
	if err != nil {
		t.Fatalf("GetForDownload(): %v", err)
	}
	if got.AccountID != "account-a" || got.State != "pending" || got.BlobHash != nil ||
		got.RemoteID != attachment.RemoteID || !bytes.Equal(got.RemoteRef, attachment.RemoteRef) ||
		got.Filename != attachment.Filename || got.MIME != attachment.MIME ||
		got.SizeBytes == nil || *got.SizeBytes != size {
		t.Fatalf("projected attachment = %+v, want %+v", got, attachment)
	}
	assertInboxProcessedAt(t, store, inbox.InboxID, messageTestTimeMS)
}

func TestProjectMessageRollsBackWhenAttachmentInsertFails(t *testing.T) {
	clock := newMessageTestClock(messageTestTimeMS)
	store, repository := openMessageTestRepository(t, clock.Now)
	seedMessageProjectionGraph(t, store)
	inbox := messageTestInbox("inbox-attachment-rollback", "account-a", "event-attachment-rollback", []byte("frame"))
	if _, err := repository.AppendInbox(context.Background(), inbox); err != nil {
		t.Fatalf("AppendInbox(): %v", err)
	}
	mustExec(t, store.db, `
		CREATE TRIGGER fail_message_attachment_insert
		BEFORE INSERT ON message_attachments
		BEGIN
			SELECT RAISE(ABORT, 'injected message attachment failure');
		END
	`)
	message := messageTestMessage(
		"message-attachment-rollback", "conversation-a", "account-a", "remote-attachment-rollback", pointer("identity-a"),
	)
	err := repository.ProjectMessage(context.Background(), MessageProjection{
		InboxID: inbox.InboxID,
		Message: message,
		Attachments: []MessageAttachment{{
			MessageID: message.MessageID,
			Ordinal:   0,
			MIME:      "image/png",
		}},
	})
	if err == nil {
		t.Fatal("ProjectMessage() succeeded, want injected attachment failure")
	}
	assertRowCount(t, store.db, "messages", 0)
	assertRowCount(t, store.db, "message_attachments", 0)
	assertInboxUnprocessed(t, store, inbox.InboxID)
}

func TestProjectMessageAttachmentReplayPreservesDownloadedBlob(t *testing.T) {
	clock := newMessageTestClock(messageTestTimeMS)
	store, repository := openMessageTestRepository(t, clock.Now)
	seedMessageProjectionGraph(t, store)
	message := messageTestMessage(
		"message-attachment-replay", "conversation-a", "account-a", "remote-attachment-replay", pointer("identity-a"),
	)

	firstInbox := messageTestInbox("inbox-attachment-first", "account-a", "event-attachment-first", []byte("first"))
	if _, err := repository.AppendInbox(context.Background(), firstInbox); err != nil {
		t.Fatalf("AppendInbox(first): %v", err)
	}
	firstSize := int64(10)
	first := MessageAttachment{
		MessageID: message.MessageID,
		Ordinal:   0,
		RemoteID:  "remote-first",
		RemoteRef: []byte("ref-first"),
		Filename:  "first.png",
		MIME:      "image/png",
		SizeBytes: &firstSize,
	}
	firstProjection := MessageProjection{InboxID: firstInbox.InboxID, Message: message, Attachments: []MessageAttachment{first}}
	if err := repository.ProjectMessage(context.Background(), firstProjection); err != nil {
		t.Fatalf("ProjectMessage(first): %v", err)
	}

	clock.Set(messageTestTimeMS + 100)
	secondInbox := messageTestInbox("inbox-attachment-second", "account-a", "event-attachment-second", []byte("second"))
	if _, err := repository.AppendInbox(context.Background(), secondInbox); err != nil {
		t.Fatalf("AppendInbox(second): %v", err)
	}
	secondSize := int64(20)
	second := MessageAttachment{
		MessageID: message.MessageID,
		Ordinal:   0,
		RemoteID:  "remote-second",
		RemoteRef: []byte("ref-second"),
		Filename:  "second.jpg",
		MIME:      "image/jpeg",
		SizeBytes: &secondSize,
	}
	duplicateMessage := message
	duplicateMessage.MessageID = "message-attachment-replay-discarded"
	if err := repository.ProjectMessage(context.Background(), MessageProjection{
		InboxID: secondInbox.InboxID, Message: duplicateMessage, Attachments: []MessageAttachment{second},
	}); err != nil {
		t.Fatalf("ProjectMessage(pending replay): %v", err)
	}
	attachmentRepository, err := NewMessageAttachmentRepository(store, clock.Now)
	if err != nil {
		t.Fatalf("NewMessageAttachmentRepository(): %v", err)
	}
	pending, err := attachmentRepository.GetForDownload(context.Background(), message.MessageID, 0)
	if err != nil {
		t.Fatalf("GetForDownload(pending replay): %v", err)
	}
	if pending.RemoteID != second.RemoteID || !bytes.Equal(pending.RemoteRef, second.RemoteRef) ||
		pending.Filename != second.Filename || pending.MIME != second.MIME ||
		pending.SizeBytes == nil || *pending.SizeBytes != secondSize {
		t.Fatalf("pending replay attachment = %+v, want refreshed metadata %+v", pending, second)
	}

	blobHash := strings.Repeat("3c", 32)
	clock.Set(messageTestTimeMS + 200)
	if err := attachmentRepository.MarkDownloaded(
		context.Background(), message.MessageID, 0, blobHash, 30, "image/webp",
	); err != nil {
		t.Fatalf("MarkDownloaded(): %v", err)
	}

	clock.Set(messageTestTimeMS + 300)
	thirdInbox := messageTestInbox("inbox-attachment-third", "account-a", "event-attachment-third", []byte("third"))
	if _, err := repository.AppendInbox(context.Background(), thirdInbox); err != nil {
		t.Fatalf("AppendInbox(third): %v", err)
	}
	thirdSize := int64(40)
	third := MessageAttachment{
		MessageID: message.MessageID,
		Ordinal:   0,
		RemoteID:  "remote-third",
		RemoteRef: []byte("ref-third"),
		Filename:  "third.txt",
		MIME:      "text/plain",
		SizeBytes: &thirdSize,
	}
	thirdProjection := MessageProjection{InboxID: thirdInbox.InboxID, Message: message, Attachments: []MessageAttachment{third}}
	if err := repository.ProjectMessage(context.Background(), thirdProjection); err != nil {
		t.Fatalf("ProjectMessage(downloaded replay): %v", err)
	}
	clock.Set(messageTestTimeMS + 400)
	if err := repository.ProjectMessage(context.Background(), MessageProjection{
		InboxID: thirdInbox.InboxID, Message: message, Attachments: []MessageAttachment{first},
	}); err != nil {
		t.Fatalf("ProjectMessage(processed replay): %v", err)
	}

	downloaded, err := attachmentRepository.GetForDownload(context.Background(), message.MessageID, 0)
	if err != nil {
		t.Fatalf("GetForDownload(downloaded replay): %v", err)
	}
	if downloaded.State != "downloaded" || downloaded.BlobHash == nil || *downloaded.BlobHash != blobHash ||
		downloaded.RemoteID != second.RemoteID || !bytes.Equal(downloaded.RemoteRef, second.RemoteRef) ||
		downloaded.Filename != second.Filename || downloaded.MIME != "image/webp" ||
		downloaded.SizeBytes == nil || *downloaded.SizeBytes != 30 {
		t.Fatalf("downloaded replay attachment = %+v, want downloaded row preserved", downloaded)
	}
}

func TestProjectMessageMapsSQLiteOwnershipFailures(t *testing.T) {
	store, repository := openMessageTestRepository(
		t,
		func() time.Time { return time.UnixMilli(messageTestTimeMS) },
	)
	seedMessageProjectionGraph(t, store)
	seedMessageAccount(t, store, "account-b", "whatsapp")
	seedMessageIdentity(t, store, "identity-b", "account-b")
	seedMessageConversation(t, store, "conversation-b", "account-b")

	tests := []struct {
		name         string
		conversation string
		sender       *string
		want         error
		wantDetail   error
	}{
		{
			name:         "cross-account conversation",
			conversation: "conversation-b",
			sender:       pointer("identity-a"),
			want:         ErrCrossAccountMessage,
		},
		{
			name:         "cross-account sender",
			conversation: "conversation-a",
			sender:       pointer("identity-b"),
			want:         ErrCrossAccountMessage,
		},
		{
			name:         "orphan conversation",
			conversation: "conversation-missing",
			sender:       pointer("identity-a"),
			want:         ErrOrphanMessage,
			wantDetail:   ErrOrphanMessageConversation,
		},
		{
			name:         "orphan sender",
			conversation: "conversation-a",
			sender:       pointer("identity-missing"),
			want:         ErrOrphanMessage,
			wantDetail:   ErrOrphanMessageIdentity,
		},
	}
	for i, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			inbox := messageTestInbox(
				fmt.Sprintf("inbox-invalid-%d", i),
				"account-a",
				fmt.Sprintf("event-invalid-%d", i),
				[]byte(test.name),
			)
			if _, err := repository.AppendInbox(context.Background(), inbox); err != nil {
				t.Fatalf("AppendInbox(): %v", err)
			}
			message := messageTestMessage(
				fmt.Sprintf("message-invalid-%d", i),
				test.conversation,
				"account-a",
				fmt.Sprintf("remote-invalid-%d", i),
				test.sender,
			)
			err := repository.ProjectMessage(context.Background(), MessageProjection{
				InboxID: inbox.InboxID,
				Message: message,
			})
			for _, want := range []error{ErrInvalidMessage, ErrConstraintViolation, test.want} {
				if !errors.Is(err, want) {
					t.Fatalf("ProjectMessage() error = %v, want %v", err, want)
				}
			}
			if test.wantDetail != nil && !errors.Is(err, test.wantDetail) {
				t.Fatalf("ProjectMessage() error = %v, want %v", err, test.wantDetail)
			}
			assertInboxUnprocessed(t, store, inbox.InboxID)
		})
	}
	assertRowCount(t, store.db, "messages", 0)

	// The composite foreign keys themselves are the enforcement boundary, not
	// only repository-side ownership checks.
	_, err := store.db.Exec(`
		INSERT INTO messages (
			message_id, conversation_id, account_id, remote_message_id,
			sender_identity_id, direction, occurred_at_ms, created_at_ms, updated_at_ms
		) VALUES ('message-direct-sql', 'conversation-a', 'account-a',
		          'remote-direct-sql', 'identity-b', 'incoming', ?, ?, ?)
	`, messageTestTimeMS, messageTestTimeMS, messageTestTimeMS)
	if !isSQLiteErrorCode(err, sqliteConstraintForeignKeyCode) {
		t.Fatalf("direct cross-account message error = %v, want SQLite foreign-key constraint", err)
	}
}

func TestProjectMessageAllowsNilSystemSender(t *testing.T) {
	store, repository := openMessageTestRepository(
		t,
		func() time.Time { return time.UnixMilli(messageTestTimeMS) },
	)
	seedMessageProjectionGraph(t, store)
	inbox := messageTestInbox("inbox-system", "account-a", "event-system", []byte("system"))
	if _, err := repository.AppendInbox(context.Background(), inbox); err != nil {
		t.Fatalf("AppendInbox(): %v", err)
	}
	message := messageTestMessage("message-system", "conversation-a", "account-a", "remote-system", nil)
	if err := repository.ProjectMessage(context.Background(), MessageProjection{
		InboxID: inbox.InboxID,
		Message: message,
	}); err != nil {
		t.Fatalf("ProjectMessage(nil sender): %v", err)
	}
	got, err := repository.GetMessage(context.Background(), message.MessageID)
	if err != nil {
		t.Fatalf("GetMessage(): %v", err)
	}
	if got.SenderIdentityID != nil {
		t.Fatalf("sender identity = %v, want nil", got.SenderIdentityID)
	}
}

func TestGetMessageByRemoteUsesAccountScopedNaturalKey(t *testing.T) {
	store, repository := openMessageTestRepository(
		t,
		func() time.Time { return time.UnixMilli(messageTestTimeMS) },
	)
	seedMessageProjectionGraph(t, store)
	inbox := messageTestInbox("inbox-remote-lookup", "account-a", "event-remote-lookup", []byte("frame"))
	if _, err := repository.AppendInbox(context.Background(), inbox); err != nil {
		t.Fatalf("AppendInbox(): %v", err)
	}
	message := messageTestMessage(
		"message-remote-lookup",
		"conversation-a",
		"account-a",
		"remote-message-lookup",
		nil,
	)
	if err := repository.ProjectMessage(context.Background(), MessageProjection{
		InboxID: inbox.InboxID,
		Message: message,
	}); err != nil {
		t.Fatalf("ProjectMessage(): %v", err)
	}

	got, err := repository.GetMessageByRemote(
		context.Background(),
		message.AccountID,
		message.ConversationID,
		message.RemoteMessageID,
	)
	if err != nil {
		t.Fatalf("GetMessageByRemote(): %v", err)
	}
	if got.MessageID != message.MessageID || got.Body != message.Body {
		t.Fatalf("GetMessageByRemote() = %+v, want message %q", got, message.MessageID)
	}

	for _, test := range []struct {
		name           string
		accountID      string
		conversationID string
		remoteID       string
	}{
		{name: "other account", accountID: "account-b", conversationID: message.ConversationID, remoteID: message.RemoteMessageID},
		{name: "other conversation", accountID: message.AccountID, conversationID: "conversation-b", remoteID: message.RemoteMessageID},
		{name: "other remote", accountID: message.AccountID, conversationID: message.ConversationID, remoteID: "missing"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := repository.GetMessageByRemote(
				context.Background(),
				test.accountID,
				test.conversationID,
				test.remoteID,
			)
			if !errors.Is(err, ErrNotFound) {
				t.Fatalf("GetMessageByRemote() error = %v, want ErrNotFound", err)
			}
		})
	}
}

func TestMessagesInboxMigrationIsChecksummedAndStrict(t *testing.T) {
	store, _ := openMessageTestRepository(
		t,
		func() time.Time { return time.UnixMilli(messageTestTimeMS) },
	)
	if len(embeddedMigrations) != 11 {
		t.Fatalf("embedded migrations = %d, want 11", len(embeddedMigrations))
	}
	assertPragmaInt(t, store.db, "user_version", len(embeddedMigrations))
	ledger := readLedgerRow(t, store.db, 4)
	if ledger.name != "messages_inbox" {
		t.Fatalf("migration 0004 name = %q, want messages_inbox", ledger.name)
	}
	if ledger.checksum != embeddedMigrations[3].checksumSHA256 {
		t.Fatalf("migration 0004 checksum = %q, want %q", ledger.checksum, embeddedMigrations[3].checksumSHA256)
	}
	for _, table := range []string{"inbox", "messages"} {
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
}

type messageTestClock struct {
	mu    sync.Mutex
	nowMS int64
}

func newMessageTestClock(nowMS int64) *messageTestClock {
	return &messageTestClock{nowMS: nowMS}
}

func (c *messageTestClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.UnixMilli(c.nowMS)
}

func (c *messageTestClock) Set(nowMS int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nowMS = nowMS
}

func openMessageTestRepository(
	t *testing.T,
	now func() time.Time,
) (*Store, *MessageRepository) {
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
	repository, err := NewMessageRepository(store, now)
	if err != nil {
		t.Fatalf("NewMessageRepository(): %v", err)
	}
	return store, repository
}

func mustMessageRepository(t *testing.T, store *Store, nowMS int64) *MessageRepository {
	t.Helper()
	repository, err := NewMessageRepository(
		store,
		func() time.Time { return time.UnixMilli(nowMS) },
	)
	if err != nil {
		t.Fatalf("NewMessageRepository(): %v", err)
	}
	return repository
}

func seedMessageProjectionGraph(t *testing.T, store *Store) {
	t.Helper()
	seedMessageAccount(t, store, "account-a", "signal")
	seedMessageIdentity(t, store, "identity-a", "account-a")
	seedMessageConversation(t, store, "conversation-a", "account-a")
}

func seedMessageAccount(t *testing.T, store *Store, accountID, bridgeKey string) {
	t.Helper()
	account := repositoryTestAccount(accountID)
	account.BridgeKey = bridgeKey
	mustRepositoryWrite(t, "seed UpsertAccount", store.UpsertAccount(account))
}

func seedMessageIdentity(t *testing.T, store *Store, identityID, accountID string) {
	t.Helper()
	identity := repositoryTestIdentity(identityID, accountID, identityID)
	mustRepositoryWrite(t, "seed UpsertIdentity", store.UpsertIdentity(identity))
}

func seedMessageConversation(t *testing.T, store *Store, conversationID, accountID string) {
	t.Helper()
	conversation := Conversation{
		ConversationID:       conversationID,
		AccountID:            accountID,
		RemoteConversationID: "remote-" + conversationID,
		Kind:                 ConversationKindDirect,
		NotificationMode:     NotificationModeAll,
		MetadataJSON:         `{}`,
		CreatedAtMS:          repositoryTestTimeMS,
		UpdatedAtMS:          repositoryTestTimeMS,
	}
	mustRepositoryWrite(t, "seed UpsertConversation", store.UpsertConversation(conversation))
}

func messageTestInbox(inboxID, accountID, dedupeKey string, payload []byte) InboxRecord {
	return InboxRecord{
		InboxID:      inboxID,
		AccountID:    accountID,
		Generation:   1,
		DedupeKey:    dedupeKey,
		Codec:        "test.frame",
		CodecVersion: 1,
		Payload:      payload,
	}
}

func messageTestMessage(
	messageID string,
	conversationID string,
	accountID string,
	remoteMessageID string,
	senderIdentityID *string,
) Message {
	return Message{
		MessageID:        messageID,
		ConversationID:   conversationID,
		AccountID:        accountID,
		RemoteMessageID:  remoteMessageID,
		SenderIdentityID: senderIdentityID,
		Direction:        MessageDirectionIncoming,
		Body:             "hello",
		State:            MessageStateActive,
		OccurredAtMS:     messageTestTimeMS - 1_000,
	}
}

func assertInboxProcessedAt(t *testing.T, store *Store, inboxID string, want int64) {
	t.Helper()
	var got int64
	if err := store.db.QueryRow(`
		SELECT processed_at_ms
		FROM inbox
		WHERE inbox_id = ?
	`, inboxID).Scan(&got); err != nil {
		t.Fatalf("read inbox %q processed_at_ms: %v", inboxID, err)
	}
	if got != want {
		t.Fatalf("inbox %q processed_at_ms = %d, want %d", inboxID, got, want)
	}
}

func assertInboxUnprocessed(t *testing.T, store *Store, inboxID string) {
	t.Helper()
	var processed bool
	if err := store.db.QueryRow(`
		SELECT processed_at_ms IS NOT NULL
		FROM inbox
		WHERE inbox_id = ?
	`, inboxID).Scan(&processed); err != nil {
		t.Fatalf("read inbox %q processing state: %v", inboxID, err)
	}
	if processed {
		t.Fatalf("inbox %q is processed after failed projection", inboxID)
	}
}

func pointer(value string) *string {
	return &value
}
