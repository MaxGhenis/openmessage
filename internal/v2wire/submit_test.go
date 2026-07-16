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
	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/messaging"
	"github.com/maxghenis/openmessage/internal/storage/blob"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

func TestSubmitTextMirrorsReplyAndForwardsSchedulingAndIdempotency(t *testing.T) {
	legacy := openLegacyTestStore(t)
	v2 := openV2TestStore(t)
	seedLegacyConversation(t, legacy, "google-chat", "sms", false)
	if err := legacy.UpsertMessage(&dbMessageForSubmitTest); err != nil {
		t.Fatalf("UpsertMessage(reply): %v", err)
	}
	registry := submitTestRegistry{caps: map[string]bridge.CapabilitySet{
		googleAccountID: {TextSend: true},
	}}
	service := newSubmitTestService(t, v2, registry, nil)
	notBefore := time.Now().Add(2 * time.Hour).Truncate(time.Millisecond)

	submission, err := SubmitText(context.Background(), Deps{
		Legacy: legacy, V2: v2, Service: service, Registry: registry,
	}, TextInput{
		ConversationID: "google-chat",
		Body:           "reply body",
		ReplyToID:      dbMessageForSubmitTest.MessageID,
		IdempotencyKey: "submit-text-key",
		NotBefore:      notBefore,
	})
	if err != nil {
		t.Fatalf("SubmitText(): %v", err)
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
	if message.AccountID != googleAccountID || message.ConversationID != "google-chat" ||
		message.ReplyToRemoteID == nil || *message.ReplyToRemoteID != dbMessageForSubmitTest.MessageID {
		t.Fatalf("optimistic message = %+v", message)
	}
}

func TestSubmitMediaMirrorsAndStreamsIntoOutboxBlob(t *testing.T) {
	legacy := openLegacyTestStore(t)
	v2 := openV2TestStore(t)
	seedLegacyConversation(t, legacy, "whatsapp:chat", "whatsapp", false)
	registry := submitTestRegistry{caps: map[string]bridge.CapabilitySet{
		whatsappAccountID: {MediaSend: true},
	}}
	blobs, err := blob.New(filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatalf("blob.New(): %v", err)
	}
	service := newSubmitTestService(t, v2, registry, blobs)
	content := []byte("media bytes")

	submission, err := SubmitMedia(context.Background(), Deps{
		Legacy: legacy, V2: v2, Service: service, Registry: registry,
	}, MediaInput{
		ConversationID: "whatsapp:chat",
		Content:        bytes.NewReader(content),
		Filename:       "photo.jpg",
		MIME:           "image/jpeg",
		Caption:        "caption",
		IdempotencyKey: "submit-media-key",
	})
	if err != nil {
		t.Fatalf("SubmitMedia(): %v", err)
	}
	outbox, err := sqlite.NewOutboxRepository(v2, time.Now)
	if err != nil {
		t.Fatalf("NewOutboxRepository(): %v", err)
	}
	attachment, err := outbox.GetOutboxAttachment(context.Background(), submission.OutboxID)
	if err != nil {
		t.Fatalf("GetOutboxAttachment(): %v", err)
	}
	if attachment.Filename != "photo.jpg" || attachment.MIME != "image/jpeg" || attachment.SizeBytes != int64(len(content)) {
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

func TestSubmitFacadeRejectsArchivesAndMissingCapabilitiesBeforeEnqueue(t *testing.T) {
	tests := []struct {
		name           string
		conversationID string
		platform       string
		caps           bridge.CapabilitySet
	}{
		{name: "archive platform", conversationID: "archive", platform: "imessage", caps: bridge.CapabilitySet{TextSend: true}},
		{name: "live platform without text capability", conversationID: "google-chat", platform: "sms"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			legacy := openLegacyTestStore(t)
			v2 := openV2TestStore(t)
			seedLegacyConversation(t, legacy, test.conversationID, test.platform, false)
			registry := submitTestRegistry{caps: map[string]bridge.CapabilitySet{
				googleAccountID: test.caps,
			}}
			service := newSubmitTestService(t, v2, registry, nil)
			_, err := SubmitText(context.Background(), Deps{
				Legacy: legacy, V2: v2, Service: service, Registry: registry,
			}, TextInput{
				ConversationID: test.conversationID,
				Body:           "must not enqueue",
				IdempotencyKey: "unsupported-key",
			})
			if !errors.Is(err, ErrPlatformNotSendable) {
				t.Fatalf("SubmitText() error = %v, want ErrPlatformNotSendable", err)
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

func TestSubmitTextRejectsCrossConversationReplyAsUnavailable(t *testing.T) {
	legacy := openLegacyTestStore(t)
	v2 := openV2TestStore(t)
	seedLegacyConversation(t, legacy, "google-chat-a", "sms", false)
	seedLegacyConversation(t, legacy, "google-chat-b", "sms", false)
	if err := legacy.UpsertMessage(&db.Message{
		MessageID:      "parent-in-b",
		ConversationID: "google-chat-b",
		Body:           "parent",
		TimestampMS:    1_900_000_000_000,
		SourcePlatform: "sms",
	}); err != nil {
		t.Fatalf("UpsertMessage(reply): %v", err)
	}
	registry := submitTestRegistry{caps: map[string]bridge.CapabilitySet{
		googleAccountID: {TextSend: true},
	}}
	service := newSubmitTestService(t, v2, registry, nil)

	_, err := SubmitText(context.Background(), Deps{
		Legacy: legacy, V2: v2, Service: service, Registry: registry,
	}, TextInput{
		ConversationID: "google-chat-a",
		Body:           "reply",
		ReplyToID:      "parent-in-b",
		IdempotencyKey: "cross-conversation-reply",
	})
	if !errors.Is(err, ErrReplyTargetUnavailable) {
		t.Fatalf("SubmitText() error = %v, want ErrReplyTargetUnavailable", err)
	}
}

var dbMessageForSubmitTest = db.Message{
	MessageID:      "google-parent",
	ConversationID: "google-chat",
	Body:           "parent body",
	TimestampMS:    1_900_000_000_000,
	SourcePlatform: "sms",
}

type submitTestRegistry struct {
	caps map[string]bridge.CapabilitySet
}

func (r submitTestRegistry) Snapshot(string) (bridge.Snapshot, bool) { return bridge.Snapshot{}, false }

func (r submitTestRegistry) Acquire(context.Context, string, bridge.Capability) (*bridge.DispatchLease, error) {
	return nil, bridge.ErrCapabilityUnavailable
}

func (r submitTestRegistry) Capabilities(accountID string) bridge.CapabilitySet {
	return r.caps[accountID]
}

func newSubmitTestService(
	t *testing.T,
	store *sqlite.Store,
	registry bridge.Registry,
	blobs *blob.BlobStore,
) *messaging.MessageService {
	t.Helper()
	service, err := messaging.NewMessageService(
		store,
		registry,
		blobs,
		messaging.SystemClock{},
		messaging.CryptoIDSource{},
	)
	if err != nil {
		t.Fatalf("NewMessageService(): %v", err)
	}
	return service
}
