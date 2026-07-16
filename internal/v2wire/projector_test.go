package v2wire

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/messaging"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

func TestProjectorGoldensAndEchoReconciliation(t *testing.T) {
	tests := []struct {
		name           string
		conversationID string
		platform       string
		accountID      string
		requestID      string
		remoteID       string
		messageID      string
		status         string
		sourcePlatform string
		sourceID       string
		replyRemoteID  string
		replyLegacyID  string
		preinsertEcho  bool
	}{
		{
			name: "google tmp delete swap", conversationID: "google-chat", platform: "sms",
			accountID: googleAccountID, requestID: "google-request", remoteID: "google-request",
			messageID: "google-request", status: "OUTGOING_SENDING", sourcePlatform: "sms",
			replyRemoteID: "google-reply", replyLegacyID: "google-reply",
			preinsertEcho: true,
		},
		{
			name: "whatsapp same id upsert", conversationID: "whatsapp:chat", platform: "whatsapp",
			accountID: whatsappAccountID, requestID: "wa-request", remoteID: "wa-remote",
			messageID: "whatsapp:wa-remote", status: "sent", sourcePlatform: "whatsapp", sourceID: "wa-remote",
			replyRemoteID: "wa-reply", replyLegacyID: "whatsapp:wa-reply",
			preinsertEcho: true,
		},
		{
			name: "signal exact timestamp id", conversationID: "signal:+15551230000", platform: "signal",
			accountID: signalAccountID, requestID: "signal-request", remoteID: "1700000000123",
			messageID: "signal:1700000000123", status: "sent", sourcePlatform: "signal", sourceID: "1700000000123",
			replyRemoteID: "signal:1600000000123", replyLegacyID: "signal:1600000000123",
			preinsertEcho: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			legacy := openLegacyTestStore(t)
			v2 := openV2TestStore(t)
			seedLegacyConversation(t, legacy, test.conversationID, test.platform, false)
			if _, _, err := MirrorConversation(legacy, v2, test.conversationID); err != nil {
				t.Fatalf("MirrorConversation(): %v", err)
			}
			clock := &projectorTestClock{now: time.UnixMilli(1_900_000_000_000)}
			outbox := newProjectorTestOutbox(t, v2, clock)
			confirmedAtMS := enqueueConfirmedProjectorMessage(
				t, v2, outbox, clock, projectorMessageSeed{
					OutboxID: "outbox-" + test.name, AccountID: test.accountID,
					ConversationID: test.conversationID, Kind: sqlite.OutboxKindText,
					LocalMessageID: "local-" + test.name, RequestID: test.requestID,
					RemoteID: test.remoteID, Body: "projected body", ReplyToRemoteID: test.replyRemoteID,
				},
			)

			want := &db.Message{
				MessageID: test.messageID, ConversationID: test.conversationID,
				Body: "projected body", TimestampMS: confirmedAtMS,
				Status: test.status, IsFromMe: true, ReplyToID: test.replyLegacyID,
				SourcePlatform: test.sourcePlatform, SourceID: test.sourceID,
			}
			if test.preinsertEcho {
				echo := *want
				if test.accountID == googleAccountID {
					echo.MessageID = "google-permanent"
				}
				echo.Status = "echo-arrived-first"
				echo.TimestampMS--
				if err := legacy.UpsertMessage(&echo); err != nil {
					t.Fatalf("UpsertMessage(preinsert echo): %v", err)
				}
			}

			events := &projectorTestEvents{}
			projector := Projector{
				V2Store: v2, Outbox: outbox, Legacy: legacy, Events: events,
				Logger: zerolog.Nop(), Now: clock.Now,
			}
			cursor := newProjectorCursor(confirmedAtMS - 1)
			if err := projector.projectConfirmedSince(context.Background(), &cursor); err != nil {
				t.Fatalf("projectConfirmedSince(): %v", err)
			}
			got, err := legacy.GetMessageByID(want.MessageID)
			if err != nil {
				t.Fatalf("GetMessageByID(): %v", err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("projected legacy row =\n%+v\nwant =\n%+v", got, want)
			}
			if events.messages != 1 || events.conversations != 1 ||
				len(events.conversationIDs) != 1 || events.conversationIDs[0] != test.conversationID {
				t.Fatalf("published events = %+v, want one messages + one conversations", events)
			}

			if test.accountID == googleAccountID {
				permanent := *want
				permanent.MessageID = "google-permanent"
				permanent.Status = "OUTGOING_COMPLETE"
				permanent.TimestampMS++
				if err := legacy.UpsertMessage(&permanent); err != nil {
					t.Fatalf("UpsertMessage(permanent echo): %v", err)
				}
				if err := legacy.DeleteMessageByID(test.requestID); err != nil {
					t.Fatalf("DeleteMessageByID(tmp): %v", err)
				}
				if tmp, err := legacy.GetMessageByID(test.requestID); err != nil || tmp != nil {
					t.Fatalf("tmp after echo swap = %+v, error %v", tmp, err)
				}
			}
			messages, err := legacy.GetMessagesByConversation(test.conversationID, 10)
			if err != nil {
				t.Fatalf("GetMessagesByConversation(): %v", err)
			}
			if len(messages) != 1 {
				t.Fatalf("messages after echo simulation = %+v, want exactly one", messages)
			}
		})
	}
}

func TestProjectorMediaUsesV2BlobAcrossPlatforms(t *testing.T) {
	tests := []struct {
		name           string
		conversationID string
		platform       string
		accountID      string
		requestID      string
		remoteID       string
		messageID      string
		status         string
		sourcePlatform string
		sourceID       string
		replyRemoteID  string
		replyLegacyID  string
	}{
		{name: "google", conversationID: "google-media", platform: "sms", accountID: googleAccountID, requestID: "google-media-request", remoteID: "google-media-request", messageID: "google-media-request", status: "OUTGOING_SENDING", sourcePlatform: "sms", replyRemoteID: "google-media-reply", replyLegacyID: "google-media-reply"},
		{name: "whatsapp", conversationID: "whatsapp:media", platform: "whatsapp", accountID: whatsappAccountID, requestID: "wa-media-request", remoteID: "wa-media-remote", messageID: "whatsapp:wa-media-remote", status: "sent", sourcePlatform: "whatsapp", sourceID: "wa-media-remote", replyRemoteID: "wa-media-reply", replyLegacyID: "whatsapp:wa-media-reply"},
		{name: "signal", conversationID: "signal:media", platform: "signal", accountID: signalAccountID, requestID: "signal-media-request", remoteID: "1700000000456", messageID: "signal:1700000000456", status: "sent", sourcePlatform: "signal", sourceID: "1700000000456", replyRemoteID: "signal:1600000000456", replyLegacyID: "signal:1600000000456"},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			legacy := openLegacyTestStore(t)
			v2 := openV2TestStore(t)
			seedLegacyConversation(t, legacy, test.conversationID, test.platform, false)
			if _, _, err := MirrorConversation(legacy, v2, test.conversationID); err != nil {
				t.Fatalf("MirrorConversation(): %v", err)
			}
			clock := &projectorTestClock{now: time.UnixMilli(1_900_000_100_000 + int64(index*100))}
			outbox := newProjectorTestOutbox(t, v2, clock)
			blobHash := strings.Repeat(fmt.Sprintf("%x", index+10), 64)
			blobHash = blobHash[:64]
			confirmedAtMS := enqueueConfirmedProjectorMessage(
				t, v2, outbox, clock, projectorMessageSeed{
					OutboxID: "outbox-media-" + test.name, AccountID: test.accountID,
					ConversationID: test.conversationID, Kind: sqlite.OutboxKindMedia,
					LocalMessageID: "local-media-" + test.name, RequestID: test.requestID,
					RemoteID: test.remoteID, Body: "media caption", ReplyToRemoteID: test.replyRemoteID,
					BlobHash: blobHash, MIME: "image/jpeg", Filename: "photo.jpg",
				},
			)
			events := &projectorTestEvents{}
			projector := Projector{V2Store: v2, Outbox: outbox, Legacy: legacy, Events: events, Logger: zerolog.Nop()}
			cursor := newProjectorCursor(confirmedAtMS - 1)
			if err := projector.projectConfirmedSince(context.Background(), &cursor); err != nil {
				t.Fatalf("projectConfirmedSince(): %v", err)
			}
			got, err := legacy.GetMessageByID(test.messageID)
			if err != nil {
				t.Fatalf("GetMessageByID(): %v", err)
			}
			want := &db.Message{
				MessageID: test.messageID, ConversationID: test.conversationID,
				Body: "media caption", TimestampMS: confirmedAtMS,
				Status: test.status, IsFromMe: true,
				MediaID: "v2blob:" + blobHash, MimeType: "image/jpeg", ReplyToID: test.replyLegacyID,
				SourcePlatform: test.sourcePlatform, SourceID: test.sourceID,
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("projected media row =\n%+v\nwant =\n%+v", got, want)
			}
			if events.messages != 1 || events.conversations != 1 {
				t.Fatalf("published events = %+v, want one each", events)
			}
		})
	}
}

func TestProjectorFiltersQueuedAndConfirmedNonMessageKinds(t *testing.T) {
	legacy := openLegacyTestStore(t)
	v2 := openV2TestStore(t)
	seedLegacyConversation(t, legacy, "google-chat", "sms", false)
	if _, _, err := MirrorConversation(legacy, v2, "google-chat"); err != nil {
		t.Fatalf("MirrorConversation(): %v", err)
	}
	clock := &projectorTestClock{now: time.UnixMilli(1_900_000_200_000)}
	outbox := newProjectorTestOutbox(t, v2, clock)
	projectV2TestMessage(t, v2, sqlite.Message{
		MessageID: "reaction-target", ConversationID: "google-chat", AccountID: googleAccountID,
		RemoteMessageID: "reaction-target-remote", Direction: sqlite.MessageDirectionIncoming,
		Body: "target", State: sqlite.MessageStateActive, OccurredAtMS: clock.Now().UnixMilli(),
	})
	if _, _, err := outbox.EnqueueOutgoingMessage(context.Background(), sqlite.NewOutboxItem{
		OutboxID: "queued-text", AccountID: googleAccountID, ConversationID: "google-chat",
		Kind: sqlite.OutboxKindText, IdempotencyKey: "queued-text", PayloadHash: "queued-text",
		Operation: "send_text", LocalMessageID: "queued-local", TransportRequestID: "queued-request",
		ScheduledFor: clock.Now().Add(time.Hour),
	}, sqlite.Message{
		MessageID: "queued-local", ConversationID: "google-chat", AccountID: googleAccountID,
		RemoteMessageID: "queued-request", Direction: sqlite.MessageDirectionOutgoing,
		Body: "still queued", State: sqlite.MessageStateActive, OccurredAtMS: clock.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("EnqueueOutgoingMessage(queued): %v", err)
	}
	confirmedAtMS := enqueueConfirmedReaction(t, outbox, clock, 0)

	events := &projectorTestEvents{}
	projector := Projector{V2Store: v2, Outbox: outbox, Legacy: legacy, Events: events, Logger: zerolog.Nop()}
	cursor := newProjectorCursor(confirmedAtMS - 1)
	if err := projector.projectConfirmedSince(context.Background(), &cursor); err != nil {
		t.Fatalf("projectConfirmedSince(): %v", err)
	}
	messages, err := legacy.GetMessagesByConversation("google-chat", 10)
	if err != nil {
		t.Fatalf("GetMessagesByConversation(): %v", err)
	}
	if len(messages) != 0 || events.messages != 0 || events.conversations != 0 {
		t.Fatalf("filtered projection wrote messages %+v or events %+v", messages, events)
	}
}

func TestProjectorMalformedRowDoesNotBlockLaterConfirmations(t *testing.T) {
	legacy := openLegacyTestStore(t)
	v2 := openV2TestStore(t)
	seedLegacyConversation(t, legacy, "google-chat", "sms", false)
	if _, _, err := MirrorConversation(legacy, v2, "google-chat"); err != nil {
		t.Fatalf("MirrorConversation(): %v", err)
	}
	clock := &projectorTestClock{now: time.UnixMilli(1_900_000_250_000)}
	outbox := newProjectorTestOutbox(t, v2, clock)

	// Generic Enqueue permits a message-kind row without its optimistic message.
	// This models local corruption that must remain retryable without hiding every
	// healthy confirmation ordered after it.
	if _, _, err := outbox.Enqueue(context.Background(), sqlite.NewOutboxItem{
		OutboxID: "malformed-text", AccountID: googleAccountID,
		ConversationID: "google-chat", Kind: sqlite.OutboxKindText,
		IdempotencyKey: "malformed-text", PayloadHash: "malformed-text",
		Operation: "send_text", TransportRequestID: "malformed-request",
		ScheduledFor: clock.Now(),
	}); err != nil {
		t.Fatalf("Enqueue(malformed): %v", err)
	}
	confirmProjectorOutbox(t, outbox, clock, "malformed-text", "malformed-request", true)
	malformedAtMS := clock.Now().UnixMilli()

	clock.now = clock.now.Add(time.Millisecond)
	confirmedAtMS := enqueueConfirmedProjectorMessage(
		t, v2, outbox, clock, projectorMessageSeed{
			OutboxID: "healthy-text", AccountID: googleAccountID,
			ConversationID: "google-chat", Kind: sqlite.OutboxKindText,
			LocalMessageID: "healthy-local", RequestID: "healthy-request",
			RemoteID: "healthy-request", Body: "must remain visible",
		},
	)
	if confirmedAtMS <= malformedAtMS {
		t.Fatalf("healthy confirmation at %d must follow malformed row at %d", confirmedAtMS, malformedAtMS)
	}

	events := &projectorTestEvents{}
	projector := Projector{
		V2Store: v2, Outbox: outbox, Legacy: legacy, Events: events,
		Logger: zerolog.Nop(),
	}
	cursor := newProjectorCursor(malformedAtMS - 1)
	if err := projector.projectConfirmedSince(context.Background(), &cursor); err == nil {
		t.Fatal("projectConfirmedSince() succeeded despite malformed row")
	}
	got, err := legacy.GetMessageByID("healthy-request")
	if err != nil || got == nil || got.Body != "must remain visible" {
		t.Fatalf("healthy later projection = %+v, %v", got, err)
	}
	if events.messages != 1 || events.conversations != 1 {
		t.Fatalf("events after first scan = %+v, want one healthy projection", events)
	}

	// The malformed row remains retryable, while the already-projected later row
	// is remembered and must not republish on every five-second fallback scan.
	if err := projector.projectConfirmedSince(context.Background(), &cursor); err == nil {
		t.Fatal("second projectConfirmedSince() succeeded despite malformed row")
	}
	if events.messages != 1 || events.conversations != 1 {
		t.Fatalf("events after retry = %+v, healthy row was republished", events)
	}
}

func TestProjectorWatermarkRetainsSameMillisecondTiesAcrossFullBatches(t *testing.T) {
	legacy := openLegacyTestStore(t)
	v2 := openV2TestStore(t)
	seedLegacyConversation(t, legacy, "google-chat", "sms", false)
	if _, _, err := MirrorConversation(legacy, v2, "google-chat"); err != nil {
		t.Fatalf("MirrorConversation(): %v", err)
	}
	clock := &projectorTestClock{now: time.UnixMilli(1_900_000_300_000)}
	outbox := newProjectorTestOutbox(t, v2, clock)
	projectV2TestMessage(t, v2, sqlite.Message{
		MessageID: "reaction-target", ConversationID: "google-chat", AccountID: googleAccountID,
		RemoteMessageID: "reaction-target-remote", Direction: sqlite.MessageDirectionIncoming,
		Body: "target", State: sqlite.MessageStateActive, OccurredAtMS: clock.Now().UnixMilli(),
	})
	const total = projectorInitialBatchLimit + 1
	var confirmedAtMS int64
	for index := 0; index < total; index++ {
		confirmedAtMS = enqueueConfirmedReaction(t, outbox, clock, index)
	}

	projector := Projector{V2Store: v2, Outbox: outbox, Legacy: legacy, Logger: zerolog.Nop()}
	cursor := newProjectorCursor(confirmedAtMS - 1)
	if err := projector.projectConfirmedSince(context.Background(), &cursor); err != nil {
		t.Fatalf("projectConfirmedSince(): %v", err)
	}
	if cursor.updatedAtMS != confirmedAtMS || cursor.processedCountAt(confirmedAtMS) != total {
		t.Fatalf("cursor = %+v, want %d IDs at %d", cursor, total, confirmedAtMS)
	}

	// A row confirmed later in wall-clock order can still share the same stored
	// millisecond. Requerying the inclusive tie and remembering IDs must pick it
	// up instead of losing it behind a strict `updated_at_ms > watermark` read.
	enqueueConfirmedReaction(t, outbox, clock, total)
	if err := projector.projectConfirmedSince(context.Background(), &cursor); err != nil {
		t.Fatalf("projectConfirmedSince(second tie): %v", err)
	}
	if got := cursor.processedCountAt(confirmedAtMS); got != total+1 {
		t.Fatalf("cursor retained %d same-ms IDs, want %d", got, total+1)
	}
}

func TestProjectorRunValidatesDependenciesAndAllowsNilEvents(t *testing.T) {
	legacy := openLegacyTestStore(t)
	v2 := openV2TestStore(t)
	outbox := newProjectorTestOutbox(t, v2, &projectorTestClock{now: time.UnixMilli(1_900_000_400_000)})
	registry := submitTestRegistry{caps: map[string]bridge.CapabilitySet{}}
	service := newSubmitTestService(t, v2, registry, nil)

	for _, test := range []struct {
		name      string
		projector Projector
	}{
		{name: "nil v2 store", projector: Projector{Outbox: outbox, Legacy: legacy, Service: service}},
		{name: "nil outbox", projector: Projector{V2Store: v2, Legacy: legacy, Service: service}},
		{name: "nil legacy store", projector: Projector{V2Store: v2, Outbox: outbox, Service: service}},
		{name: "nil service", projector: Projector{V2Store: v2, Outbox: outbox, Legacy: legacy}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := test.projector.Run(context.Background()); err == nil {
				t.Fatal("Run() succeeded with missing dependency")
			}
		})
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	projector := Projector{
		V2Store: v2, Outbox: outbox, Legacy: legacy, Service: service,
		Events: nil, Logger: zerolog.Nop(),
	}
	if err := projector.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run(canceled, nil events) error = %v, want context.Canceled", err)
	}
}

type projectorMessageSeed struct {
	OutboxID        string
	AccountID       string
	ConversationID  string
	Kind            sqlite.OutboxKind
	LocalMessageID  string
	RequestID       string
	RemoteID        string
	Body            string
	ReplyToRemoteID string
	BlobHash        string
	MIME            string
	Filename        string
}

func enqueueConfirmedProjectorMessage(
	t *testing.T,
	store *sqlite.Store,
	outbox *sqlite.OutboxRepository,
	clock *projectorTestClock,
	seed projectorMessageSeed,
) int64 {
	t.Helper()
	var reply *string
	if seed.ReplyToRemoteID != "" {
		value := seed.ReplyToRemoteID
		reply = &value
	}
	item := sqlite.NewOutboxItem{
		OutboxID: seed.OutboxID, AccountID: seed.AccountID, ConversationID: seed.ConversationID,
		Kind: seed.Kind, IdempotencyKey: seed.OutboxID, PayloadHash: "hash-" + seed.OutboxID,
		Operation: "send_text", LocalMessageID: seed.LocalMessageID,
		TransportRequestID: seed.RequestID, ScheduledFor: clock.Now(),
	}
	message := sqlite.Message{
		MessageID: seed.LocalMessageID, ConversationID: seed.ConversationID, AccountID: seed.AccountID,
		RemoteMessageID: seed.RequestID, Direction: sqlite.MessageDirectionOutgoing,
		Body: seed.Body, ReplyToRemoteID: reply, State: sqlite.MessageStateActive,
		OccurredAtMS: clock.Now().UnixMilli(),
	}
	var err error
	if seed.Kind == sqlite.OutboxKindMedia {
		item.Operation = "send_media"
		_, _, err = outbox.EnqueueOutgoingMediaMessage(context.Background(), item, message, sqlite.OutboxAttachment{
			BlobHash: seed.BlobHash, MIME: seed.MIME, Filename: seed.Filename, SizeBytes: 123,
		})
	} else {
		_, _, err = outbox.EnqueueOutgoingMessage(context.Background(), item, message)
	}
	if err != nil {
		t.Fatalf("enqueue confirmed projector message: %v", err)
	}
	confirmProjectorOutbox(t, outbox, clock, seed.OutboxID, seed.RemoteID, true)
	return clock.Now().UnixMilli()
}

func enqueueConfirmedReaction(
	t *testing.T,
	outbox *sqlite.OutboxRepository,
	clock *projectorTestClock,
	index int,
) int64 {
	t.Helper()
	id := fmt.Sprintf("reaction-%04d", index)
	if _, _, err := outbox.EnqueueReaction(context.Background(), sqlite.NewOutboxItem{
		OutboxID: id, AccountID: googleAccountID, ConversationID: "google-chat",
		Kind: sqlite.OutboxKindReaction, IdempotencyKey: id, PayloadHash: "hash-" + id,
		Operation: "reaction", TransportRequestID: "request-" + id, ScheduledFor: clock.Now(),
	}, sqlite.OutboxReaction{TargetMessageID: "reaction-target", Emoji: "👍", Action: "add"}); err != nil {
		t.Fatalf("EnqueueReaction(%d): %v", index, err)
	}
	confirmProjectorOutbox(t, outbox, clock, id, "", false)
	return clock.Now().UnixMilli()
}

func confirmProjectorOutbox(
	t *testing.T,
	outbox *sqlite.OutboxRepository,
	clock *projectorTestClock,
	outboxID string,
	remoteID string,
	withResult bool,
) {
	t.Helper()
	leases, err := outbox.LeaseDue(context.Background(), sqlite.LeaseRequest{
		Owner: "projector-test", Now: clock.Now(), Duration: time.Minute, Limit: 1,
	})
	if err != nil || len(leases) != 1 || leases[0].OutboxID != outboxID || leases[0].LeaseToken == nil {
		t.Fatalf("LeaseDue(%q) = %+v, %v", outboxID, leases, err)
	}
	leaseToken := *leases[0].LeaseToken
	if err := outbox.MarkTransportCalled(context.Background(), sqlite.Attempt{
		OutboxID: outboxID, LeaseToken: leaseToken,
	}); err != nil {
		t.Fatalf("MarkTransportCalled(%q): %v", outboxID, err)
	}
	if withResult {
		if err := outbox.Confirm(context.Background(), sqlite.Confirmation{
			OutboxID: outboxID, LeaseToken: leaseToken, ResultRemoteID: remoteID,
		}); err != nil {
			t.Fatalf("Confirm(%q): %v", outboxID, err)
		}
	} else if err := outbox.ConfirmWithoutResult(context.Background(), outboxID, leaseToken); err != nil {
		t.Fatalf("ConfirmWithoutResult(%q): %v", outboxID, err)
	}
}

func newProjectorTestOutbox(
	t *testing.T,
	store *sqlite.Store,
	clock *projectorTestClock,
) *sqlite.OutboxRepository {
	t.Helper()
	repository, err := sqlite.NewOutboxRepository(store, clock.Now)
	if err != nil {
		t.Fatalf("NewOutboxRepository(): %v", err)
	}
	return repository
}

type projectorTestClock struct{ now time.Time }

func (c *projectorTestClock) Now() time.Time { return c.now }

type projectorTestEvents struct {
	messages        int
	conversations   int
	conversationIDs []string
}

func (e *projectorTestEvents) PublishMessages(conversationID string) {
	e.messages++
	e.conversationIDs = append(e.conversationIDs, conversationID)
}

func (e *projectorTestEvents) PublishConversations() { e.conversations++ }

var _ messaging.Clock = messaging.SystemClock{}
