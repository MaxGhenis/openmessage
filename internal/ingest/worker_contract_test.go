package ingest

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/messaging"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

func TestNewWorkerUsesOnlyConfiguredDecoders(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "explicit-decoders.sqlite3"))
	if err != nil {
		t.Fatalf("sqlite.Open(): %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Store.Close(): %v", err)
		}
	})
	messages, err := sqlite.NewMessageRepository(store, time.Now)
	if err != nil {
		t.Fatalf("NewMessageRepository(): %v", err)
	}
	worker, err := NewWorker(WorkerConfig{
		Store:    store,
		Messages: messages,
	})
	if err != nil {
		t.Fatalf("NewWorker(): %v", err)
	}
	if got := len(worker.decoders); got != 0 {
		t.Fatalf("NewWorker() registered %d implicit decoders, want none", got)
	}
}

func TestWorkerOnlyFirstMessageRoutesEchoAndAdditionalMessagesImport(t *testing.T) {
	recorder := &i01EchoRecorder{}
	decoder := i01DecoderFunc(func(
		_ context.Context,
		_ bridge.RawIngressRecord,
	) ([]bridge.Event, error) {
		first := i01OutgoingMessageEvents("multi-first", "first body", "request-first")[0]
		second := i01OutgoingMessageEvents("multi-second", "second body", "request-second")[0]
		return []bridge.Event{first, second}, nil
	})
	harness := i01NewHarness(t, decoder, recorder)
	i01StartWorker(t, harness.worker)

	i01MustAppend(t, harness.sink, i01IngressRecord("multi:frame", []byte("multi")))
	i01WaitFor(t, "multi-message projection", func() bool {
		snapshot := harness.counters.Snapshot(i01AccountID)
		return snapshot.Projected == 1 && snapshot.Imported == 1
	})

	calls := recorder.snapshot()
	want := messaging.TransportEcho{
		AccountID:          i01AccountID,
		TransportRequestID: "request-first",
		RemoteMessageID:    "multi-first",
	}
	if len(calls) != 1 || calls[0] != want {
		t.Fatalf("ObserveTransportEcho calls = %+v, want only first message %+v", calls, want)
	}
	for _, remoteID := range []string{"multi-first", "multi-second"} {
		if _, err := i01GetMessage(harness.messages, remoteID); err != nil {
			t.Errorf("GetMessageByRemote(%q): %v", remoteID, err)
		}
	}
	snapshot := harness.counters.Snapshot(i01AccountID)
	if snapshot.EchoReconciled != 1 || snapshot.DecodedEvents != 2 {
		t.Fatalf("multi-message counters = %+v, want one echo and two decoded events", snapshot)
	}
	i01AssertNoPending(t, harness.messages)
}

func TestWorkerQuarantinesUnknownCodecAndProcessesNoEventFrame(t *testing.T) {
	decoder := i01DecoderFunc(func(
		_ context.Context,
		_ bridge.RawIngressRecord,
	) ([]bridge.Event, error) {
		return nil, nil
	})
	harness := i01NewHarness(t, decoder, nil)
	i01StartWorker(t, harness.worker)

	unknown := i01IngressRecord("unknown:codec", []byte("retained unknown payload"))
	unknown.Codec = "unknown.codec"
	i01MustAppend(t, harness.sink, unknown)
	i01MustAppend(t, harness.sink, i01IngressRecord("no:events", []byte("no events")))
	i01WaitFor(t, "unknown quarantine and no-event processing", func() bool {
		snapshot := harness.counters.Snapshot(i01AccountID)
		pending, err := harness.messages.Unprocessed(context.Background())
		return err == nil && len(pending) == 0 && snapshot.Quarantined == 1
	})

	snapshot := harness.counters.Snapshot(i01AccountID)
	if snapshot.Appended != 2 || snapshot.Quarantined != 1 || snapshot.DecodedEvents != 0 {
		t.Fatalf("unknown/no-event counters = %+v", snapshot)
	}
	payload, processedAt := i01InboxPayload(t, harness.path, "unknown:codec")
	if string(payload) != "retained unknown payload" || !processedAt.Valid {
		t.Fatalf("unknown-codec durable row = (%q, %+v), want retained and processed", payload, processedAt)
	}
}

type panickingEchoObserver struct{}

func (panickingEchoObserver) ObserveTransportEcho(context.Context, messaging.TransportEcho) (messaging.EchoOutcome, error) {
	panic("scripted echo observer panic")
}

func TestWorkerContainsApplyPanicAndContinues(t *testing.T) {
	decoder := i01DecoderFunc(func(
		_ context.Context,
		record bridge.RawIngressRecord,
	) ([]bridge.Event, error) {
		if string(record.Payload) == "panic" {
			return i01OutgoingMessageEvents("panic-message", "panic", "panic-request"), nil
		}
		return i01OutgoingMessageEvents("after-panic", "continued", ""), nil
	})
	harness := i01NewHarness(t, decoder, panickingEchoObserver{})
	i01StartWorker(t, harness.worker)

	i01MustAppend(t, harness.sink, i01IngressRecord("apply:panic", []byte("panic")))
	i01MustAppend(t, harness.sink, i01IngressRecord("apply:continue", []byte("continue")))
	i01WaitFor(t, "apply panic quarantine and continuation", func() bool {
		snapshot := harness.counters.Snapshot(i01AccountID)
		message, err := i01GetMessage(harness.messages, "after-panic")
		return snapshot.Quarantined == 1 && err == nil && message.Body == "continued"
	})
	if _, err := i01GetMessage(harness.messages, "panic-message"); !errors.Is(err, sqlite.ErrNotFound) {
		t.Fatalf("panicking message lookup error = %v, want ErrNotFound", err)
	}
	i01AssertNoPending(t, harness.messages)
}

func TestWorkerConversationSnapshotAndIdentityNaturalKeysPreserveLocalRows(t *testing.T) {
	const (
		remoteConversationID = "whatsapp:15551234567@s.whatsapp.net"
		conversationID       = "mirror-conversation-pk"
		identityID           = "mirror-identity-pk"
	)
	harness := newWorkerPathHarness(t, bridge.PlatformWhatsApp, map[string][]bridge.Event{
		"snapshot": {{
			Kind: bridge.EventConversation,
			Conversation: &bridge.ConversationEvent{
				RemoteConversationID: remoteConversationID,
				Kind:                 "direct",
				Title:                "Transport title",
				RemoteRevision:       "revision-2",
				Participants: []bridge.Participant{{
					Identity: bridge.IdentityRef{
						Raw:  "+1 (555) 123-4567",
						Name: "Updated display",
					},
					Role:   "member",
					Active: true,
				}},
			},
		}},
		"message": {{
			Kind: bridge.EventMessage,
			Message: &bridge.MessageEvent{
				RemoteConversationID: remoteConversationID,
				RemoteMessageID:      "incoming-natural-key",
				Sender: bridge.IdentityRef{
					Raw:  "+1 (555) 123-4567",
					Name: "Newest display",
				},
				Direction:  "incoming",
				Body:       "incoming",
				OccurredAt: time.UnixMilli(workerPathNowMS - 500),
			},
		}},
	})
	conversation := workerPathSeedConversation(
		t,
		harness.store,
		conversationID,
		remoteConversationID,
		"Local title",
	)
	conversation.NotificationMode = sqlite.NotificationModeMuted
	if err := harness.store.UpsertConversation(conversation); err != nil {
		t.Fatalf("UpsertConversation(local preferences): %v", err)
	}
	if err := harness.store.UpsertIdentity(sqlite.Identity{
		IdentityID:     identityID,
		AccountID:      workerPathAccountID,
		Kind:           sqlite.IdentityKind("e164"),
		CanonicalValue: "+15551234567",
		RawValue:       "+15551234567",
		DisplayName:    "Mirror display",
		MetadataJSON:   `{"source":"mirror"}`,
		CreatedAtMS:    workerPathNowMS - 10_000,
		UpdatedAtMS:    workerPathNowMS - 5_000,
	}); err != nil {
		t.Fatalf("UpsertIdentity(mirror): %v", err)
	}

	harness.process(t, "snapshot")
	gotConversation, err := harness.store.GetConversationByRemote(
		workerPathAccountID,
		remoteConversationID,
	)
	if err != nil {
		t.Fatalf("GetConversationByRemote(): %v", err)
	}
	if gotConversation.ConversationID != conversationID ||
		gotConversation.Title != "Transport title" ||
		gotConversation.Kind != sqlite.ConversationKindDirect ||
		gotConversation.NotificationMode != sqlite.NotificationModeMuted ||
		!gotConversation.IsFavorite || gotConversation.MetadataJSON != conversation.MetadataJSON ||
		gotConversation.CreatedAtMS != conversation.CreatedAtMS {
		t.Fatalf("refreshed conversation = %+v, want transport fields plus preserved local fields", gotConversation)
	}
	participants, err := harness.store.ListParticipants(conversationID)
	if err != nil {
		t.Fatalf("ListParticipants(): %v", err)
	}
	if len(participants) != 1 || participants[0].IdentityID != identityID {
		t.Fatalf("participants = %+v, want existing natural-key identity %q", participants, identityID)
	}

	harness.process(t, "message")
	message, err := harness.messages.GetMessageByRemote(
		context.Background(),
		workerPathAccountID,
		conversationID,
		"incoming-natural-key",
	)
	if err != nil {
		t.Fatalf("GetMessageByRemote(): %v", err)
	}
	if message.SenderIdentityID == nil || *message.SenderIdentityID != identityID {
		t.Fatalf("sender identity = %v, want existing PK %q", message.SenderIdentityID, identityID)
	}
	identity, err := harness.store.GetIdentity(identityID)
	if err != nil {
		t.Fatalf("GetIdentity(): %v", err)
	}
	if identity.DisplayName != "Newest display" || identity.MetadataJSON != `{"source":"mirror"}` {
		t.Fatalf("refreshed identity = %+v, want new display and preserved metadata", identity)
	}
	identities, err := harness.store.ListIdentities(workerPathAccountID)
	if err != nil {
		t.Fatalf("ListIdentities(): %v", err)
	}
	if len(identities) != 1 {
		t.Fatalf("identity rows = %d, want one natural-key row", len(identities))
	}
}
