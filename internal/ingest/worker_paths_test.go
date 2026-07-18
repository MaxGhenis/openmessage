package ingest

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
	"github.com/maxghenis/openmessage/internal/v2keys"
)

const (
	workerPathAccountID = "worker-path-account"
	workerPathCodec     = "worker.path.fake"
	workerPathNowMS     = int64(2_000_000_000_000)
)

type workerPathDecoder struct {
	eventsByPayload map[string][]bridge.Event
}

func (d *workerPathDecoder) Decode(
	_ context.Context,
	record bridge.RawIngressRecord,
) ([]bridge.Event, error) {
	events, ok := d.eventsByPayload[string(record.Payload)]
	if !ok {
		return nil, fmt.Errorf("worker-path decoder has no script for payload %q", record.Payload)
	}
	return append([]bridge.Event(nil), events...), nil
}

type workerPathHarness struct {
	store     *sqlite.Store
	messages  *sqlite.MessageRepository
	reactions *sqlite.ReactionRepository
	worker    *Worker
	nextID    int
}

func newWorkerPathHarness(
	t *testing.T,
	platform bridge.Platform,
	eventsByPayload map[string][]bridge.Event,
) *workerPathHarness {
	t.Helper()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "worker-path.sqlite3"))
	if err != nil {
		t.Fatalf("sqlite.Open(): %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Store.Close(): %v", err)
		}
	})
	if err := store.UpsertAccount(sqlite.Account{
		AccountID:   workerPathAccountID,
		BridgeKey:   string(platform),
		Mode:        sqlite.AccountModeLive,
		Enabled:     true,
		ConfigJSON:  `{}`,
		CreatedAtMS: workerPathNowMS,
		UpdatedAtMS: workerPathNowMS,
	}); err != nil {
		t.Fatalf("UpsertAccount(): %v", err)
	}
	now := func() time.Time { return time.UnixMilli(workerPathNowMS) }
	messages, err := sqlite.NewMessageRepository(store, now)
	if err != nil {
		t.Fatalf("NewMessageRepository(): %v", err)
	}
	reactions, err := sqlite.NewReactionRepository(store, now)
	if err != nil {
		t.Fatalf("NewReactionRepository(): %v", err)
	}
	worker, err := NewWorker(WorkerConfig{
		Store:     store,
		Messages:  messages,
		Reactions: reactions,
		Counters:  &Counters{},
		Now:       now,
		Decoders: []DecoderRegistration{{
			Codec:    workerPathCodec,
			Platform: platform,
			Decoder: &workerPathDecoder{
				eventsByPayload: eventsByPayload,
			},
		}},
	})
	if err != nil {
		t.Fatalf("NewWorker(): %v", err)
	}
	return &workerPathHarness{
		store:     store,
		messages:  messages,
		reactions: reactions,
		worker:    worker,
	}
}

func (h *workerPathHarness) process(t *testing.T, payload string) string {
	t.Helper()
	h.nextID++
	inboxID := fmt.Sprintf("worker-path-inbox-%d", h.nextID)
	record := bridge.RawIngressRecord{
		AccountID:    workerPathAccountID,
		Generation:   1,
		DedupeKey:    "worker-path-dedupe-" + payload,
		Codec:        workerPathCodec,
		CodecVersion: 1,
		ReceivedAt:   time.UnixMilli(workerPathNowMS),
		Payload:      []byte(payload),
	}
	effectiveID, err := h.messages.AppendInbox(context.Background(), sqlite.InboxRecord{
		InboxID:      inboxID,
		AccountID:    record.AccountID,
		Generation:   int64(record.Generation),
		DedupeKey:    record.DedupeKey,
		Codec:        record.Codec,
		CodecVersion: int64(record.CodecVersion),
		Payload:      record.Payload,
	})
	if err != nil {
		t.Fatalf("AppendInbox(%q): %v", payload, err)
	}
	if effectiveID != inboxID {
		t.Fatalf("AppendInbox(%q) ID = %q, want %q", payload, effectiveID, inboxID)
	}
	if err := h.worker.processRecord(context.Background(), inboxID, record); err != nil {
		t.Fatalf("processRecord(%q): %v", payload, err)
	}
	return inboxID
}

func TestWorkerPathsMutationEditDeleteAndMissingTarget(t *testing.T) {
	const (
		remoteConversationID = "whatsapp:15551234567@s.whatsapp.net"
		conversationID       = "worker-path-mutation-conversation"
		remoteMessageID      = "worker-path-target-message"
	)
	harness := newWorkerPathHarness(t, bridge.PlatformWhatsApp, map[string][]bridge.Event{
		"edit": {{
			Kind: bridge.EventMessageMutation,
			MessageMutation: &bridge.MessageMutationEvent{
				RemoteConversationID: remoteConversationID,
				RemoteMessageID:      remoteMessageID,
				Kind:                 "edit",
				Body:                 "edited body",
			},
		}},
		"delete": {{
			Kind: bridge.EventMessageMutation,
			MessageMutation: &bridge.MessageMutationEvent{
				RemoteConversationID: remoteConversationID,
				RemoteMessageID:      remoteMessageID,
				Kind:                 "delete",
				Body:                 "must not replace the edited body",
			},
		}},
		"missing": {{
			Kind: bridge.EventMessageMutation,
			MessageMutation: &bridge.MessageMutationEvent{
				RemoteConversationID: remoteConversationID,
				RemoteMessageID:      "missing-target",
				Kind:                 "edit",
				Body:                 "ignored",
			},
		}},
	})
	workerPathSeedConversation(
		t,
		harness.store,
		conversationID,
		remoteConversationID,
		"Mutation thread",
	)
	workerPathSeedMessage(
		t,
		harness.messages,
		"worker-path-message",
		conversationID,
		remoteMessageID,
		sqlite.MessageDirectionIncoming,
		"original body",
		workerPathNowMS-1_000,
	)

	harness.process(t, "edit")
	got, err := harness.messages.GetMessageByRemote(
		context.Background(),
		workerPathAccountID,
		conversationID,
		remoteMessageID,
	)
	if err != nil {
		t.Fatalf("GetMessageByRemote(after edit): %v", err)
	}
	if got.State != sqlite.MessageStateEdited || got.Body != "edited body" {
		t.Fatalf("message after edit = %+v, want edited state/body", got)
	}

	harness.process(t, "delete")
	got, err = harness.messages.GetMessageByRemote(
		context.Background(),
		workerPathAccountID,
		conversationID,
		remoteMessageID,
	)
	if err != nil {
		t.Fatalf("GetMessageByRemote(after delete): %v", err)
	}
	if got.State != sqlite.MessageStateDeleted || got.Body != "edited body" {
		t.Fatalf("message after delete = %+v, want deleted state and preserved body", got)
	}

	missingInboxID := harness.process(t, "missing")
	if _, err := harness.messages.GetMessageByRemote(
		context.Background(),
		workerPathAccountID,
		conversationID,
		"missing-target",
	); !errors.Is(err, sqlite.ErrNotFound) {
		t.Fatalf("missing mutation target lookup error = %v, want ErrNotFound", err)
	}
	workerPathAssertProcessed(t, harness.messages, missingInboxID)
	snapshot := harness.worker.Counters().Snapshot(workerPathAccountID)
	if snapshot.Mutations != 2 || snapshot.Quarantined != 0 {
		t.Fatalf("mutation counters = %+v, want mutations=2 and quarantined=0", snapshot)
	}
	workerPathAssertAllProcessed(t, harness.messages)
}

func TestWorkerPathsSelfReceiptMonotoneAndMissingPosition(t *testing.T) {
	const (
		remoteConversationID = "whatsapp:15557654321@s.whatsapp.net"
		conversationID       = "worker-path-receipt-conversation"
		deviceID             = "worker-path-local-device"
		remoteMessageID      = "worker-path-read-message"
		messageID            = "worker-path-read-message-local"
	)
	harness := newWorkerPathHarness(t, bridge.PlatformWhatsApp, map[string][]bridge.Event{
		"known-newer":   {workerPathReceiptEvent(remoteConversationID, remoteMessageID, workerPathNowMS+200)},
		"missing-older": {workerPathReceiptEvent(remoteConversationID, "missing-remote", workerPathNowMS+100)},
		"missing-newer": {workerPathReceiptEvent(remoteConversationID, "missing-remote", workerPathNowMS+300)},
		"known-older":   {workerPathReceiptEvent(remoteConversationID, remoteMessageID, workerPathNowMS+250)},
	})
	workerPathSeedConversation(
		t,
		harness.store,
		conversationID,
		remoteConversationID,
		"Receipt thread",
	)
	workerPathSeedLocalDevice(t, harness.store, deviceID)
	workerPathSeedMessage(
		t,
		harness.messages,
		messageID,
		conversationID,
		remoteMessageID,
		sqlite.MessageDirectionIncoming,
		"read me",
		workerPathNowMS-1_000,
	)

	harness.process(t, "known-newer")
	workerPathAssertCursor(t, harness.store, deviceID, conversationID, workerPathNowMS+200, messageID)

	harness.process(t, "missing-older")
	workerPathAssertCursor(t, harness.store, deviceID, conversationID, workerPathNowMS+200, messageID)

	harness.process(t, "missing-newer")
	workerPathAssertCursor(t, harness.store, deviceID, conversationID, workerPathNowMS+300, "")

	harness.process(t, "known-older")
	workerPathAssertCursor(t, harness.store, deviceID, conversationID, workerPathNowMS+300, "")

	snapshot := harness.worker.Counters().Snapshot(workerPathAccountID)
	if snapshot.ReceiptsSelf != 4 || snapshot.Quarantined != 0 {
		t.Fatalf("receipt counters = %+v, want receipts_self=4 and quarantined=0", snapshot)
	}
	workerPathAssertAllProcessed(t, harness.messages)
}

func TestWorkerPathsSignalOutgoingAliasConverges(t *testing.T) {
	const (
		remoteConversationID = "signal:+16505550100"
		conversationID       = "worker-path-signal-conversation"
		timestamp            = int64(1_700_000_006_000)
		migratedMessageID    = "worker-path-migrated-signal-message"
	)
	timestampID := strconv.FormatInt(timestamp, 10)
	alias := v2keys.SignalLocalAlias(remoteConversationID, timestamp)
	harness := newWorkerPathHarness(t, bridge.PlatformSignal, map[string][]bridge.Event{
		"signal-outgoing": {{
			Kind: bridge.EventMessage,
			Message: &bridge.MessageEvent{
				RemoteConversationID: remoteConversationID,
				RemoteMessageID:      timestampID,
				Sender:               bridge.IdentityRef{IsSelf: true},
				Direction:            "outgoing",
				Body:                 "outgoing sync replay",
				OccurredAt:           time.UnixMilli(timestamp),
			},
		}},
	})
	workerPathSeedConversation(
		t,
		harness.store,
		conversationID,
		remoteConversationID,
		"Signal thread",
	)
	workerPathSeedMessage(
		t,
		harness.messages,
		migratedMessageID,
		conversationID,
		alias,
		sqlite.MessageDirectionOutgoing,
		"migrated body",
		timestamp,
	)

	harness.process(t, "signal-outgoing")
	got, err := harness.messages.GetMessageByRemote(
		context.Background(),
		workerPathAccountID,
		conversationID,
		alias,
	)
	if err != nil {
		t.Fatalf("GetMessageByRemote(alias): %v", err)
	}
	if got.MessageID != migratedMessageID || got.RemoteMessageID != alias ||
		got.Body != "outgoing sync replay" {
		t.Fatalf("converged Signal message = %+v, want migrated PK/alias with replay body", got)
	}
	if _, err := harness.messages.GetMessageByRemote(
		context.Background(),
		workerPathAccountID,
		conversationID,
		timestampID,
	); !errors.Is(err, sqlite.ErrNotFound) {
		t.Fatalf("timestamp-form Signal lookup error = %v, want ErrNotFound", err)
	}
	if snapshot := harness.worker.Counters().Snapshot(workerPathAccountID); snapshot.Projected != 1 {
		t.Fatalf("Signal counters = %+v, want projected=1", snapshot)
	}
	workerPathAssertAllProcessed(t, harness.messages)
}

func TestWorkerPathsMessageFramePreservesExistingConversation(t *testing.T) {
	const (
		remoteConversationID   = "google-remote-thread"
		existingConversationID = "mirror-verbatim-conversation-pk"
		existingTitle          = "Mirror title must survive"
		remoteMessageID        = "google-message-1"
	)
	harness := newWorkerPathHarness(t, bridge.PlatformGoogle, map[string][]bridge.Event{
		"message-only": {{
			Kind: bridge.EventMessage,
			Message: &bridge.MessageEvent{
				RemoteConversationID: remoteConversationID,
				RemoteMessageID:      remoteMessageID,
				Sender:               bridge.IdentityRef{IsSelf: true},
				Direction:            "outgoing",
				Body:                 "message frame",
				OccurredAt:           time.UnixMilli(workerPathNowMS - 500),
			},
		}},
	})
	existing := workerPathSeedConversation(
		t,
		harness.store,
		existingConversationID,
		remoteConversationID,
		existingTitle,
	)

	harness.process(t, "message-only")
	gotConversation, err := harness.store.GetConversationByRemote(
		workerPathAccountID,
		remoteConversationID,
	)
	if err != nil {
		t.Fatalf("GetConversationByRemote(): %v", err)
	}
	if gotConversation.ConversationID != existing.ConversationID ||
		gotConversation.Title != existing.Title ||
		gotConversation.Kind != existing.Kind ||
		gotConversation.CreatedAtMS != existing.CreatedAtMS {
		t.Fatalf("conversation after message-only frame = %+v, want existing %+v", gotConversation, existing)
	}
	if gotConversation.LastMessageAtMS != workerPathNowMS-500 {
		t.Fatalf(
			"last_message_at_ms = %d, want the ingested message's %d: recency must advance even though the row is never re-upserted",
			gotConversation.LastMessageAtMS,
			workerPathNowMS-500,
		)
	}
	message, err := harness.messages.GetMessageByRemote(
		context.Background(),
		workerPathAccountID,
		existingConversationID,
		remoteMessageID,
	)
	if err != nil {
		t.Fatalf("GetMessageByRemote(): %v", err)
	}
	if message.ConversationID != existingConversationID {
		t.Fatalf("projected message conversation ID = %q, want existing PK %q", message.ConversationID, existingConversationID)
	}
	conversations, err := harness.store.ListConversationsByRecency(workerPathAccountID)
	if err != nil {
		t.Fatalf("ListConversationsByRecency(): %v", err)
	}
	if len(conversations) != 1 {
		t.Fatalf("conversation rows = %d, want 1", len(conversations))
	}
	workerPathAssertAllProcessed(t, harness.messages)
}

func workerPathReceiptEvent(
	remoteConversationID string,
	remoteMessageID string,
	readAtMS int64,
) bridge.Event {
	return bridge.Event{
		Kind: bridge.EventReceipt,
		Receipt: &bridge.ReceiptEvent{
			RemoteConversationID: remoteConversationID,
			RemoteMessageIDs:     []string{remoteMessageID},
			Actor:                bridge.IdentityRef{IsSelf: true},
			Status:               "read",
			OccurredAt:           time.UnixMilli(readAtMS),
		},
	}
}

func workerPathSeedConversation(
	t *testing.T,
	store *sqlite.Store,
	conversationID string,
	remoteConversationID string,
	title string,
) sqlite.Conversation {
	t.Helper()
	conversation := sqlite.Conversation{
		ConversationID:       conversationID,
		AccountID:            workerPathAccountID,
		RemoteConversationID: remoteConversationID,
		Kind:                 sqlite.ConversationKindGroup,
		Title:                title,
		NotificationMode:     sqlite.NotificationModeAll,
		IsFavorite:           true,
		LastMessageAtMS:      workerPathNowMS - 2_000,
		MetadataJSON:         `{"origin":"worker-path"}`,
		CreatedAtMS:          workerPathNowMS - 10_000,
		UpdatedAtMS:          workerPathNowMS - 5_000,
	}
	if err := store.UpsertConversation(conversation); err != nil {
		t.Fatalf("UpsertConversation(%q): %v", conversationID, err)
	}
	return conversation
}

func workerPathSeedMessage(
	t *testing.T,
	messages *sqlite.MessageRepository,
	messageID string,
	conversationID string,
	remoteMessageID string,
	direction sqlite.MessageDirection,
	body string,
	occurredAtMS int64,
) {
	t.Helper()
	if err := messages.ImportMessage(context.Background(), sqlite.MessageProjection{
		Message: sqlite.Message{
			MessageID:       messageID,
			ConversationID:  conversationID,
			AccountID:       workerPathAccountID,
			RemoteMessageID: remoteMessageID,
			Direction:       direction,
			Body:            body,
			State:           sqlite.MessageStateActive,
			OccurredAtMS:    occurredAtMS,
		},
	}); err != nil {
		t.Fatalf("ImportMessage(%q): %v", messageID, err)
	}
}

func workerPathSeedLocalDevice(t *testing.T, store *sqlite.Store, deviceID string) {
	t.Helper()
	if err := store.UpsertDevice(sqlite.Device{
		DeviceID:    deviceID,
		AccountID:   workerPathAccountID,
		Kind:        sqlite.DeviceKindLocalInstallation,
		State:       sqlite.DeviceStateActive,
		IsCurrent:   true,
		CreatedAtMS: workerPathNowMS - 10_000,
		UpdatedAtMS: workerPathNowMS - 5_000,
	}); err != nil {
		t.Fatalf("UpsertDevice(%q): %v", deviceID, err)
	}
}

func workerPathAssertCursor(
	t *testing.T,
	store *sqlite.Store,
	deviceID string,
	conversationID string,
	wantReadAtMS int64,
	wantMessageID string,
) {
	t.Helper()
	cursor, err := store.GetReadCursor(deviceID, conversationID)
	if err != nil {
		t.Fatalf("GetReadCursor(): %v", err)
	}
	if cursor.LastReadAtMS != wantReadAtMS {
		t.Fatalf("cursor read time = %d, want %d", cursor.LastReadAtMS, wantReadAtMS)
	}
	if wantMessageID == "" {
		if cursor.LastReadMessageID != nil {
			t.Fatalf("cursor message position = %q, want NULL", *cursor.LastReadMessageID)
		}
		return
	}
	if cursor.LastReadMessageID == nil || *cursor.LastReadMessageID != wantMessageID {
		t.Fatalf("cursor message position = %v, want %q", cursor.LastReadMessageID, wantMessageID)
	}
}

func workerPathAssertProcessed(
	t *testing.T,
	messages *sqlite.MessageRepository,
	inboxID string,
) {
	t.Helper()
	records, err := messages.Unprocessed(context.Background())
	if err != nil {
		t.Fatalf("Unprocessed(): %v", err)
	}
	for _, record := range records {
		if record.InboxID == inboxID {
			t.Fatalf("inbox %q remains unprocessed", inboxID)
		}
	}
}

func workerPathAssertAllProcessed(t *testing.T, messages *sqlite.MessageRepository) {
	t.Helper()
	records, err := messages.Unprocessed(context.Background())
	if err != nil {
		t.Fatalf("Unprocessed(): %v", err)
	}
	if len(records) != 0 {
		t.Fatalf("unprocessed inbox records = %+v, want none", records)
	}
}
