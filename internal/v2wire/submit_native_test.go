package v2wire

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/messaging"
	"github.com/maxghenis/openmessage/internal/storage/blob"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

func TestSubmitTextV2ResolvesAccountFromV2ConversationAndForwardsCommand(t *testing.T) {
	v2 := openV2TestStore(t)
	seedNativeConversation(t, v2, "native-account", "v2-conversation")
	projectV2TestMessage(t, v2, sqlite.Message{
		MessageID:       "v2-parent",
		ConversationID:  "v2-conversation",
		AccountID:       "native-account",
		RemoteMessageID: "remote-parent",
		Direction:       sqlite.MessageDirectionIncoming,
		Body:            "parent",
		State:           sqlite.MessageStateActive,
		OccurredAtMS:    1_900_000_000_000,
	})
	registry := submitTestRegistry{caps: map[string]bridge.CapabilitySet{
		"native-account": {TextSend: true},
	}}
	service := newSubmitTestService(t, v2, registry, nil)
	notBefore := time.Now().Add(2 * time.Hour).Truncate(time.Millisecond)

	submission, err := SubmitTextV2(context.Background(), NativeDeps{
		V2: v2, Service: service, Registry: registry,
	}, TextInput{
		ConversationID: "v2-conversation",
		Body:           "native reply",
		ReplyToID:      "v2-parent",
		IdempotencyKey: "native-text-key",
		NotBefore:      notBefore,
	})
	if err != nil {
		t.Fatalf("SubmitTextV2(): %v", err)
	}
	if submission.State != messaging.OutboxQueued || submission.ScheduledFor.UnixMilli() != notBefore.UnixMilli() {
		t.Fatalf("submission = %+v, want queued at %v", submission, notBefore)
	}
	deduplicated, err := SubmitTextV2(context.Background(), NativeDeps{
		V2: v2, Service: service, Registry: registry,
	}, TextInput{
		ConversationID: "v2-conversation",
		Body:           "native reply",
		ReplyToID:      "v2-parent",
		IdempotencyKey: "native-text-key",
		NotBefore:      notBefore,
	})
	if err != nil {
		t.Fatalf("SubmitTextV2(deduplicate): %v", err)
	}
	if !deduplicated.Deduplicated ||
		deduplicated.OutboxID != submission.OutboxID ||
		deduplicated.LocalMessageID != submission.LocalMessageID {
		t.Fatalf("deduplicated submission = %+v, want identities from %+v", deduplicated, submission)
	}

	repository, err := sqlite.NewMessageRepository(v2, time.Now)
	if err != nil {
		t.Fatalf("NewMessageRepository(): %v", err)
	}
	message, err := repository.GetMessage(context.Background(), submission.LocalMessageID)
	if err != nil {
		t.Fatalf("GetMessage(): %v", err)
	}
	if message.AccountID != "native-account" ||
		message.ConversationID != "v2-conversation" ||
		message.Body != "native reply" ||
		message.ReplyToRemoteID == nil ||
		*message.ReplyToRemoteID != "remote-parent" {
		t.Fatalf("optimistic message = %+v", message)
	}
}

func TestSubmitMediaV2ResolvesAccountFromV2ConversationAndForwardsCommand(t *testing.T) {
	v2 := openV2TestStore(t)
	seedNativeConversation(t, v2, "native-media-account", "v2-media-conversation")
	projectV2TestMessage(t, v2, sqlite.Message{
		MessageID:       "v2-media-parent",
		ConversationID:  "v2-media-conversation",
		AccountID:       "native-media-account",
		RemoteMessageID: "remote-media-parent",
		Direction:       sqlite.MessageDirectionIncoming,
		Body:            "media parent",
		State:           sqlite.MessageStateActive,
		OccurredAtMS:    1_900_000_000_001,
	})
	registry := submitTestRegistry{caps: map[string]bridge.CapabilitySet{
		"native-media-account": {MediaSend: true},
	}}
	blobs, err := blob.New(filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatalf("blob.New(): %v", err)
	}
	service := newSubmitTestService(t, v2, registry, blobs)
	content := []byte("native media bytes")
	notBefore := time.Now().Add(3 * time.Hour).Truncate(time.Millisecond)

	submission, err := SubmitMediaV2(context.Background(), NativeDeps{
		V2: v2, Service: service, Registry: registry,
	}, MediaInput{
		ConversationID: "v2-media-conversation",
		Content:        bytes.NewReader(content),
		Filename:       "native.jpg",
		MIME:           "image/jpeg",
		Caption:        "native caption",
		ReplyToID:      "v2-media-parent",
		IdempotencyKey: "native-media-key",
		NotBefore:      notBefore,
	})
	if err != nil {
		t.Fatalf("SubmitMediaV2(): %v", err)
	}
	if submission.State != messaging.OutboxQueued || submission.ScheduledFor.UnixMilli() != notBefore.UnixMilli() {
		t.Fatalf("submission = %+v, want queued at %v", submission, notBefore)
	}

	repository, err := sqlite.NewMessageRepository(v2, time.Now)
	if err != nil {
		t.Fatalf("NewMessageRepository(): %v", err)
	}
	message, err := repository.GetMessage(context.Background(), submission.LocalMessageID)
	if err != nil {
		t.Fatalf("GetMessage(): %v", err)
	}
	if message.AccountID != "native-media-account" ||
		message.ConversationID != "v2-media-conversation" ||
		message.Body != "native caption" ||
		message.ReplyToRemoteID == nil ||
		*message.ReplyToRemoteID != "remote-media-parent" {
		t.Fatalf("optimistic message = %+v", message)
	}

	outbox, err := sqlite.NewOutboxRepository(v2, time.Now)
	if err != nil {
		t.Fatalf("NewOutboxRepository(): %v", err)
	}
	attachment, err := outbox.GetOutboxAttachment(context.Background(), submission.OutboxID)
	if err != nil {
		t.Fatalf("GetOutboxAttachment(): %v", err)
	}
	if attachment.Filename != "native.jpg" ||
		attachment.MIME != "image/jpeg" ||
		attachment.SizeBytes != int64(len(content)) {
		t.Fatalf("attachment = %+v", attachment)
	}
	reader, err := blobs.Open(blob.BlobRef{Hash: attachment.BlobHash})
	if err != nil {
		t.Fatalf("blobs.Open(): %v", err)
	}
	defer reader.Close()
	got, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("ReadAll(blob): %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("blob = %q, want %q", got, content)
	}
}

func TestSubmitV2RejectsMissingCapabilitiesBeforeEnqueue(t *testing.T) {
	tests := []struct {
		name   string
		submit func(context.Context, NativeDeps) error
	}{
		{
			name: "text",
			submit: func(ctx context.Context, deps NativeDeps) error {
				_, err := SubmitTextV2(ctx, deps, TextInput{
					ConversationID: "v2-conversation",
					Body:           "must not enqueue",
					IdempotencyKey: "unsupported-text-key",
				})
				return err
			},
		},
		{
			name: "media",
			submit: func(ctx context.Context, deps NativeDeps) error {
				_, err := SubmitMediaV2(ctx, deps, MediaInput{
					ConversationID: "v2-conversation",
					Content:        bytes.NewReader([]byte("must not enqueue")),
					IdempotencyKey: "unsupported-media-key",
				})
				return err
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			v2 := openV2TestStore(t)
			seedNativeConversation(t, v2, "native-account", "v2-conversation")
			registry := submitTestRegistry{caps: map[string]bridge.CapabilitySet{}}
			service := newSubmitTestService(t, v2, registry, nil)

			err := test.submit(context.Background(), NativeDeps{
				V2: v2, Service: service, Registry: registry,
			})
			if !errors.Is(err, ErrPlatformNotSendable) {
				t.Fatalf("submit error = %v, want ErrPlatformNotSendable", err)
			}
			pending, listErr := service.ListPending(context.Background(), messaging.ListPendingQuery{Limit: 10})
			if listErr != nil {
				t.Fatalf("ListPending(): %v", listErr)
			}
			if len(pending) != 0 {
				t.Fatalf("pending = %+v, want empty", pending)
			}
		})
	}
}

func TestSubmitTextV2ValidatesReplyTargetByV2MessageID(t *testing.T) {
	v2 := openV2TestStore(t)
	seedNativeConversation(t, v2, "native-account", "conversation-a")
	seedNativeConversation(t, v2, "native-account", "conversation-b")
	projectV2TestMessage(t, v2, sqlite.Message{
		MessageID:       "parent-in-b",
		ConversationID:  "conversation-b",
		AccountID:       "native-account",
		RemoteMessageID: "remote-parent-in-b",
		Direction:       sqlite.MessageDirectionIncoming,
		Body:            "parent",
		State:           sqlite.MessageStateActive,
		OccurredAtMS:    1_900_000_000_000,
	})
	registry := submitTestRegistry{caps: map[string]bridge.CapabilitySet{
		"native-account": {TextSend: true},
	}}
	service := newSubmitTestService(t, v2, registry, nil)

	for _, replyToID := range []string{"parent-in-b", "missing-v2-message"} {
		t.Run(replyToID, func(t *testing.T) {
			_, err := SubmitTextV2(context.Background(), NativeDeps{
				V2: v2, Service: service, Registry: registry,
			}, TextInput{
				ConversationID: "conversation-a",
				Body:           "reply",
				ReplyToID:      replyToID,
				IdempotencyKey: "reply-" + replyToID,
			})
			if !errors.Is(err, ErrReplyTargetUnavailable) {
				t.Fatalf("SubmitTextV2() error = %v, want ErrReplyTargetUnavailable", err)
			}
		})
	}
}

func TestSubmitV2ValidatesDependenciesBeforeReadingConversation(t *testing.T) {
	tests := []struct {
		name string
		ctx  context.Context
		deps NativeDeps
	}{
		{name: "nil context", deps: NativeDeps{}},
		{name: "nil store", ctx: context.Background(), deps: NativeDeps{}},
		{name: "nil service", ctx: context.Background(), deps: NativeDeps{V2: openV2TestStore(t)}},
		{
			name: "nil registry",
			ctx:  context.Background(),
			deps: NativeDeps{V2: openV2TestStore(t), Service: &messaging.MessageService{}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := SubmitTextV2(test.ctx, test.deps, TextInput{ConversationID: "does-not-matter"})
			if err == nil {
				t.Fatal("SubmitTextV2() succeeded, want dependency error")
			}
		})
	}
}

func seedNativeConversation(t *testing.T, store *sqlite.Store, accountID, conversationID string) {
	t.Helper()
	nowMS := time.Now().UnixMilli()
	if err := store.UpsertAccount(sqlite.Account{
		AccountID:   accountID,
		BridgeKey:   "native-test",
		DisplayName: accountID,
		Mode:        sqlite.AccountModeLive,
		Enabled:     true,
		ConfigJSON:  "{}",
		CreatedAtMS: nowMS,
		UpdatedAtMS: nowMS,
	}); err != nil {
		t.Fatalf("UpsertAccount(%q): %v", accountID, err)
	}
	if err := store.UpsertConversation(sqlite.Conversation{
		ConversationID:       conversationID,
		AccountID:            accountID,
		RemoteConversationID: "remote-" + conversationID,
		Kind:                 sqlite.ConversationKindDirect,
		Title:                conversationID,
		NotificationMode:     sqlite.NotificationModeAll,
		MetadataJSON:         "{}",
		CreatedAtMS:          nowMS,
		UpdatedAtMS:          nowMS,
	}); err != nil {
		t.Fatalf("UpsertConversation(%q): %v", conversationID, err)
	}
}
