package sqlite

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestMarkInboxProcessedIsIdempotentAndRetainsPayload(t *testing.T) {
	clock := newMessageTestClock(messageTestTimeMS)
	store, repository := openMessageTestRepository(t, clock.Now)
	seedMessageAccount(t, store, "account-a", "signal")

	record := messageTestInbox(
		"inbox-mark",
		"account-a",
		"frame-mark",
		[]byte("raw transport payload"),
	)
	if _, err := repository.AppendInbox(context.Background(), record); err != nil {
		t.Fatalf("AppendInbox(): %v", err)
	}

	clock.Set(messageTestTimeMS + 100)
	for _, test := range []struct {
		name      string
		inboxID   string
		accountID string
	}{
		{name: "missing row", inboxID: "missing", accountID: "account-a"},
		{name: "wrong account", inboxID: record.InboxID, accountID: "account-b"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := repository.MarkInboxProcessed(
				context.Background(),
				test.inboxID,
				test.accountID,
			); err != nil {
				t.Fatalf("MarkInboxProcessed(): %v", err)
			}
		})
	}
	assertInboxUnprocessed(t, store, record.InboxID)

	if err := repository.MarkInboxProcessed(
		context.Background(),
		record.InboxID,
		record.AccountID,
	); err != nil {
		t.Fatalf("MarkInboxProcessed(first): %v", err)
	}
	assertInboxProcessedAt(t, store, record.InboxID, messageTestTimeMS+100)

	clock.Set(messageTestTimeMS + 200)
	if err := repository.MarkInboxProcessed(
		context.Background(),
		record.InboxID,
		record.AccountID,
	); err != nil {
		t.Fatalf("MarkInboxProcessed(replay): %v", err)
	}
	assertInboxProcessedAt(t, store, record.InboxID, messageTestTimeMS+100)

	var payload []byte
	if err := store.db.QueryRow(`
		SELECT payload
		FROM inbox
		WHERE inbox_id = ?
	`, record.InboxID).Scan(&payload); err != nil {
		t.Fatalf("read retained inbox payload: %v", err)
	}
	if !bytes.Equal(payload, record.Payload) {
		t.Fatalf("retained payload = %q, want %q", payload, record.Payload)
	}
}

func TestApplyMessageMutationEditThenDelete(t *testing.T) {
	clock := newMessageTestClock(messageTestTimeMS)
	store, repository := openMessageTestRepository(t, clock.Now)
	seedMessageProjectionGraph(t, store)

	replyToRemoteID := "remote-parent"
	message := messageTestMessage(
		"message-a",
		"conversation-a",
		"account-a",
		"remote-message-a",
		pointer("identity-a"),
	)
	message.ReplyToRemoteID = &replyToRemoteID
	message.Body = "original body"
	source := messageTestInbox("inbox-message", "account-a", "message", []byte("message"))
	if _, err := repository.AppendInbox(context.Background(), source); err != nil {
		t.Fatalf("AppendInbox(message): %v", err)
	}
	if err := repository.ProjectMessage(context.Background(), MessageProjection{
		InboxID: source.InboxID,
		Message: message,
	}); err != nil {
		t.Fatalf("ProjectMessage(): %v", err)
	}

	clock.Set(messageTestTimeMS + 100)
	edit := messageTestInbox("inbox-edit", "account-a", "edit", []byte("edit"))
	if _, err := repository.AppendInbox(context.Background(), edit); err != nil {
		t.Fatalf("AppendInbox(edit): %v", err)
	}
	clock.Set(messageTestTimeMS + 200)
	updated, err := repository.ApplyMessageMutation(
		context.Background(),
		message.AccountID,
		message.ConversationID,
		message.RemoteMessageID,
		"edit",
		"edited body",
		edit.InboxID,
	)
	if err != nil {
		t.Fatalf("ApplyMessageMutation(edit): %v", err)
	}
	if !updated {
		t.Fatal("ApplyMessageMutation(edit) updated = false, want true")
	}
	got, err := repository.GetMessage(context.Background(), message.MessageID)
	if err != nil {
		t.Fatalf("GetMessage(after edit): %v", err)
	}
	assertMutationPreservedMessageShape(t, got, message)
	if got.Body != "edited body" || got.State != MessageStateEdited {
		t.Fatalf("edited message = %+v, want edited body/state", got)
	}
	if got.UpdatedAtMS != messageTestTimeMS+200 {
		t.Fatalf("edited updated_at_ms = %d, want %d", got.UpdatedAtMS, messageTestTimeMS+200)
	}
	assertInboxProcessedAt(t, store, edit.InboxID, messageTestTimeMS+200)

	clock.Set(messageTestTimeMS + 300)
	deleteFrame := messageTestInbox("inbox-delete", "account-a", "delete", []byte("delete"))
	if _, err := repository.AppendInbox(context.Background(), deleteFrame); err != nil {
		t.Fatalf("AppendInbox(delete): %v", err)
	}
	clock.Set(messageTestTimeMS + 400)
	updated, err = repository.ApplyMessageMutation(
		context.Background(),
		message.AccountID,
		message.ConversationID,
		message.RemoteMessageID,
		"delete",
		"must be ignored",
		deleteFrame.InboxID,
	)
	if err != nil {
		t.Fatalf("ApplyMessageMutation(delete): %v", err)
	}
	if !updated {
		t.Fatal("ApplyMessageMutation(delete) updated = false, want true")
	}
	got, err = repository.GetMessage(context.Background(), message.MessageID)
	if err != nil {
		t.Fatalf("GetMessage(after delete): %v", err)
	}
	assertMutationPreservedMessageShape(t, got, message)
	if got.Body != "edited body" {
		t.Fatalf("deleted body = %q, want edited body preserved", got.Body)
	}
	if got.State != MessageStateDeleted {
		t.Fatalf("deleted state = %q, want %q", got.State, MessageStateDeleted)
	}
	if got.UpdatedAtMS != messageTestTimeMS+400 {
		t.Fatalf("deleted updated_at_ms = %d, want %d", got.UpdatedAtMS, messageTestTimeMS+400)
	}
	assertInboxProcessedAt(t, store, deleteFrame.InboxID, messageTestTimeMS+400)
}

func TestApplyMessageMutationMissingTargetIsBenignAndObservable(t *testing.T) {
	clock := newMessageTestClock(messageTestTimeMS)
	store, repository := openMessageTestRepository(t, clock.Now)
	seedMessageProjectionGraph(t, store)

	record := messageTestInbox("inbox-missing-edit", "account-a", "missing-edit", []byte("edit"))
	if _, err := repository.AppendInbox(context.Background(), record); err != nil {
		t.Fatalf("AppendInbox(): %v", err)
	}
	clock.Set(messageTestTimeMS + 100)
	updated, err := repository.ApplyMessageMutation(
		context.Background(),
		"account-a",
		"conversation-a",
		"missing-remote-message",
		"edit",
		"new body",
		record.InboxID,
	)
	if err != nil {
		t.Fatalf("ApplyMessageMutation(missing): %v", err)
	}
	if updated {
		t.Fatal("ApplyMessageMutation(missing) updated = true, want false")
	}
	assertInboxProcessedAt(t, store, record.InboxID, messageTestTimeMS+100)
	assertRowCount(t, store.db, "messages", 0)
}

func TestApplyMessageMutationRollsBackWhenInboxMarkFails(t *testing.T) {
	clock := newMessageTestClock(messageTestTimeMS)
	store, repository := openMessageTestRepository(t, clock.Now)
	seedMessageProjectionGraph(t, store)

	message := messageTestMessage(
		"message-a",
		"conversation-a",
		"account-a",
		"remote-message-a",
		pointer("identity-a"),
	)
	source := messageTestInbox("inbox-message", "account-a", "message", []byte("message"))
	if _, err := repository.AppendInbox(context.Background(), source); err != nil {
		t.Fatalf("AppendInbox(message): %v", err)
	}
	if err := repository.ProjectMessage(context.Background(), MessageProjection{
		InboxID: source.InboxID,
		Message: message,
	}); err != nil {
		t.Fatalf("ProjectMessage(): %v", err)
	}

	clock.Set(messageTestTimeMS + 300)
	edit := messageTestInbox("inbox-edit", "account-a", "edit", []byte("edit"))
	if _, err := repository.AppendInbox(context.Background(), edit); err != nil {
		t.Fatalf("AppendInbox(edit): %v", err)
	}
	clock.Set(messageTestTimeMS + 299)
	updated, err := repository.ApplyMessageMutation(
		context.Background(),
		message.AccountID,
		message.ConversationID,
		message.RemoteMessageID,
		"edit",
		"must roll back",
		edit.InboxID,
	)
	if updated {
		t.Fatal("ApplyMessageMutation() updated = true after rollback")
	}
	for _, want := range []error{ErrInvalidInboxRecord, ErrConstraintViolation} {
		if !errors.Is(err, want) {
			t.Fatalf("ApplyMessageMutation() error = %v, want %v", err, want)
		}
	}
	got, getErr := repository.GetMessage(context.Background(), message.MessageID)
	if getErr != nil {
		t.Fatalf("GetMessage(): %v", getErr)
	}
	if got.Body != message.Body || got.State != message.State || got.UpdatedAtMS != messageTestTimeMS {
		t.Fatalf("message after rollback = %+v, want original body/state/timestamp", got)
	}
	assertInboxUnprocessed(t, store, edit.InboxID)
}

func TestApplyMessageMutationRejectsUnknownKindWithoutProcessingInbox(t *testing.T) {
	clock := newMessageTestClock(messageTestTimeMS)
	store, repository := openMessageTestRepository(t, clock.Now)
	seedMessageAccount(t, store, "account-a", "signal")
	record := messageTestInbox("inbox-unknown", "account-a", "unknown", []byte("unknown"))
	if _, err := repository.AppendInbox(context.Background(), record); err != nil {
		t.Fatalf("AppendInbox(): %v", err)
	}
	updated, err := repository.ApplyMessageMutation(
		context.Background(),
		"account-a",
		"conversation-a",
		"remote-message-a",
		"replace",
		"body",
		record.InboxID,
	)
	if updated || err == nil || !strings.Contains(err.Error(), "unsupported kind") {
		t.Fatalf("ApplyMessageMutation(unknown) = (%t, %v), want false and unsupported-kind error", updated, err)
	}
	assertInboxUnprocessed(t, store, record.InboxID)
}

func TestApplyMessageMutationRejectsMissingAndCrossAccountInbox(t *testing.T) {
	clock := newMessageTestClock(messageTestTimeMS)
	store, repository := openMessageTestRepository(t, clock.Now)
	seedMessageProjectionGraph(t, store)
	seedMessageAccount(t, store, "account-b", "signal")
	foreign := messageTestInbox("inbox-account-b", "account-b", "foreign", []byte("foreign"))
	if _, err := repository.AppendInbox(context.Background(), foreign); err != nil {
		t.Fatalf("AppendInbox(foreign): %v", err)
	}

	for _, test := range []struct {
		name    string
		inboxID string
		want    error
	}{
		{name: "missing", inboxID: "missing-inbox", want: ErrOrphanMessageInbox},
		{name: "cross account", inboxID: foreign.InboxID, want: ErrCrossAccountMessage},
	} {
		t.Run(test.name, func(t *testing.T) {
			updated, err := repository.ApplyMessageMutation(
				context.Background(),
				"account-a",
				"conversation-a",
				"missing-target",
				"edit",
				"body",
				test.inboxID,
			)
			if updated || !errors.Is(err, ErrInvalidMessage) || !errors.Is(err, test.want) {
				t.Fatalf("ApplyMessageMutation() = (%t, %v), want invalid message wrapping %v", updated, err, test.want)
			}
		})
	}
	assertInboxUnprocessed(t, store, foreign.InboxID)
}

func TestGetLocalInstallationDeviceUsesAccountScopedRole(t *testing.T) {
	store, _ := openMessageTestRepository(t, func() time.Time {
		return time.UnixMilli(messageTestTimeMS)
	})
	seedMessageAccount(t, store, "account-a", "signal")
	seedMessageAccount(t, store, "account-b", "whatsapp")

	devices := []Device{
		{
			DeviceID:    "remote-endpoint",
			AccountID:   "account-a",
			Kind:        DeviceKindRemoteEndpoint,
			State:       DeviceStateActive,
			CreatedAtMS: repositoryTestTimeMS,
			UpdatedAtMS: repositoryTestTimeMS,
		},
		{
			DeviceID:    "derived-local-id",
			AccountID:   "account-a",
			Kind:        DeviceKindLocalInstallation,
			State:       DeviceStateRevoked,
			CreatedAtMS: repositoryTestTimeMS,
			UpdatedAtMS: repositoryTestTimeMS,
		},
		{
			DeviceID:    "local-primary:account-a",
			AccountID:   "account-a",
			Kind:        DeviceKindLocalInstallation,
			State:       DeviceStateActive,
			IsCurrent:   true,
			CreatedAtMS: repositoryTestTimeMS,
			UpdatedAtMS: repositoryTestTimeMS,
		},
		{
			DeviceID:    "local-primary:account-b",
			AccountID:   "account-b",
			Kind:        DeviceKindLocalInstallation,
			State:       DeviceStateActive,
			IsCurrent:   true,
			CreatedAtMS: repositoryTestTimeMS,
			UpdatedAtMS: repositoryTestTimeMS,
		},
	}
	for _, device := range devices {
		if err := store.UpsertDevice(device); err != nil {
			t.Fatalf("UpsertDevice(%q): %v", device.DeviceID, err)
		}
	}

	got, err := store.GetLocalInstallationDevice(context.Background(), "account-a")
	if err != nil {
		t.Fatalf("GetLocalInstallationDevice(): %v", err)
	}
	if got.DeviceID != "local-primary:account-a" || got.AccountID != "account-a" ||
		got.Kind != DeviceKindLocalInstallation {
		t.Fatalf("GetLocalInstallationDevice() = %+v, want account-a current local device", got)
	}

	if _, err := store.GetLocalInstallationDevice(context.Background(), "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetLocalInstallationDevice(missing) error = %v, want ErrNotFound", err)
	}
}

func assertMutationPreservedMessageShape(t *testing.T, got, original Message) {
	t.Helper()
	if got.MessageID != original.MessageID ||
		got.ConversationID != original.ConversationID ||
		got.AccountID != original.AccountID ||
		got.RemoteMessageID != original.RemoteMessageID ||
		got.Direction != original.Direction ||
		got.OccurredAtMS != original.OccurredAtMS ||
		got.CreatedAtMS != messageTestTimeMS ||
		(got.SenderIdentityID == nil) != (original.SenderIdentityID == nil) ||
		(got.SenderIdentityID != nil && *got.SenderIdentityID != *original.SenderIdentityID) ||
		(got.ReplyToRemoteID == nil) != (original.ReplyToRemoteID == nil) ||
		(got.ReplyToRemoteID != nil && *got.ReplyToRemoteID != *original.ReplyToRemoteID) {
		t.Fatalf("mutated message shape = %+v, want immutable fields from %+v", got, original)
	}
}

func TestBumpConversationRecencyIsMonotoneAndMissingRowIsNoOp(t *testing.T) {
	clock := newMessageTestClock(messageTestTimeMS)
	store, _ := openMessageTestRepository(t, clock.Now)
	seedMessageAccount(t, store, "account-a", "google")
	seedMessageConversation(t, store, "conversation-a", "account-a")

	read := func() int64 {
		t.Helper()
		conversation, err := store.GetConversationByRemote("account-a", "remote-conversation-a")
		if err != nil {
			t.Fatalf("GetConversationByRemote(): %v", err)
		}
		return conversation.LastMessageAtMS
	}

	if err := store.BumpConversationRecency("conversation-a", messageTestTimeMS+5_000); err != nil {
		t.Fatalf("BumpConversationRecency(newer): %v", err)
	}
	if got := read(); got != messageTestTimeMS+5_000 {
		t.Fatalf("last_message_at_ms after newer bump = %d, want %d", got, messageTestTimeMS+5_000)
	}

	if err := store.BumpConversationRecency("conversation-a", messageTestTimeMS+1_000); err != nil {
		t.Fatalf("BumpConversationRecency(older): %v", err)
	}
	if got := read(); got != messageTestTimeMS+5_000 {
		t.Fatalf("last_message_at_ms after older bump = %d, want unchanged %d", got, messageTestTimeMS+5_000)
	}

	if err := store.BumpConversationRecency("conversation-missing", messageTestTimeMS+9_000); err != nil {
		t.Fatalf("BumpConversationRecency(missing) = %v, want no-op nil", err)
	}
	if err := store.BumpConversationRecency("conversation-a", 0); err == nil {
		t.Fatal("BumpConversationRecency(0) = nil, want error for non-positive time")
	}
}
