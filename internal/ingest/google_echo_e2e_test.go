package ingest

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
	"google.golang.org/protobuf/proto"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/messaging"
	"github.com/maxghenis/openmessage/internal/storage/blob"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
	"github.com/maxghenis/openmessage/internal/v2wire"
)

const (
	googleEchoAccountID            = "google-primary"
	googleEchoConversationID       = "google-echo-conversation"
	googleEchoRemoteConversationID = "google-remote-conversation"
)

var googleEchoNow = time.Date(2026, time.July, 17, 16, 0, 0, 0, time.UTC)

func TestGoogleEchoEndToEndOrderAEnrichesPlaceholder(t *testing.T) {
	harness := newGoogleEchoHarness(t, nil)
	submission, requestID := harness.enqueueAndConfirm(t, "order-a", "hello from v2")

	const permanentID = "google-permanent-order-a"
	harness.appendMessage(t, googleEchoMessage(
		permanentID,
		requestID,
		"hello from v2",
		true,
		googleEchoNow.Add(time.Minute),
	))
	harness.waitFor(t, "Order-A echo enrichment and projection", func(snapshot CounterSnapshot) bool {
		return snapshot.EchoEnriched == 1 && snapshot.Projected == 1
	})

	harness.assertSinglePermanentMessage(t, submission.LocalMessageID, requestID, permanentID)
	delivery, err := harness.service.Get(context.Background(), submission.OutboxID)
	if err != nil {
		t.Fatalf("MessageService.Get(): %v", err)
	}
	if delivery.State != messaging.OutboxConfirmed || delivery.RemoteMessageID != permanentID {
		t.Fatalf("delivery after echo = %+v, want confirmed at %q", delivery, permanentID)
	}
}

func TestGoogleEchoEndToEndOrderBEchoFirstReplayConverges(t *testing.T) {
	harness := newGoogleEchoHarness(t, nil)
	submission, requestID := harness.enqueue(t, "order-b", "echo wins the first race")

	const permanentID = "google-permanent-order-b"
	record := harness.messageRecord(t, googleEchoMessage(
		permanentID,
		requestID,
		"echo wins the first race",
		true,
		googleEchoNow.Add(2*time.Minute),
	))
	harness.appendRecord(t, record)
	harness.waitFor(t, "Order-B initial echo projection", func(snapshot CounterSnapshot) bool {
		return snapshot.Projected == 1 && snapshot.EchoNoop == 1
	})
	if got := harness.countV2Messages(t); got != 2 {
		t.Fatalf("messages after echo outran confirm = %d, want optimistic plus echoed rows", got)
	}

	// A Google send confirms provisionally with result_remote_id equal to its
	// transport request ID, so Confirm alone does not converge the two rows: the
	// repoint early-returns because the "real" id is still the request id.
	// Convergence is driven by re-delivery of the echo, which production
	// guarantees — Google re-delivers the same MessageID on every status
	// transition (sent -> delivered -> read), and the content hash in the dedupe
	// key lets those changed frames through. Re-delivery then takes the enrich
	// path: it deletes the projected collision, preserves the optimistic local
	// row, and the ensuing processed-inbox projection is a benign stale conflict.
	harness.confirm(t, submission.OutboxID)
	harness.appendRecord(t, record)
	harness.waitFor(t, "Order-B collision cleanup and stale replay", func(snapshot CounterSnapshot) bool {
		return snapshot.EchoEnriched == 1 && snapshot.StaleReplays == 1
	})

	harness.assertSinglePermanentMessage(t, submission.LocalMessageID, requestID, permanentID)
	if snapshot := harness.counters.Snapshot(googleEchoAccountID); snapshot.Deduped != 1 {
		t.Fatalf("Order-B counters = %+v, want one deduplicated frame replay", snapshot)
	}
}

func TestGoogleEchoUnknownTmpIDsAreSuccessfulNoops(t *testing.T) {
	tests := []struct {
		name  string
		tmpID string
		body  string
	}{
		{name: "phone-originated", tmpID: "phone-originated-request", body: "sent from the phone"},
		{name: "media-caption-second-payload", tmpID: "reqid:caption", body: "photo caption"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			harness := newGoogleEchoHarness(t, nil)
			remoteID := "google-" + test.name
			harness.appendMessage(t, googleEchoMessage(
				remoteID,
				test.tmpID,
				test.body,
				true,
				googleEchoNow.Add(3*time.Minute),
			))
			harness.waitFor(t, "unknown TmpID projection", func(snapshot CounterSnapshot) bool {
				return snapshot.EchoNotFound == 1 && snapshot.Projected == 1
			})

			message, err := harness.messages.GetMessageByRemote(
				context.Background(),
				googleEchoAccountID,
				googleEchoConversationID,
				remoteID,
			)
			if err != nil {
				t.Fatalf("GetMessageByRemote(): %v", err)
			}
			if message.Body != test.body || message.Direction != sqlite.MessageDirectionOutgoing {
				t.Fatalf("projected message = %+v", message)
			}
			if got := harness.countV2Messages(t); got != 1 {
				t.Fatalf("messages = %d, want one normally projected message", got)
			}
		})
	}
}

func TestGoogleIngestDoesNotFeedLegacyProjector(t *testing.T) {
	harness := newGoogleEchoHarness(t, nil)
	legacy, err := db.New(filepath.Join(t.TempDir(), "legacy.sqlite3"))
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	t.Cleanup(func() { _ = legacy.Close() })

	harness.appendMessage(t, googleEchoMessage(
		"google-inbound-no-feedback",
		"",
		"inbound must stay v2-only",
		false,
		googleEchoNow.Add(4*time.Minute),
	))
	harness.waitFor(t, "inbound projection", func(snapshot CounterSnapshot) bool {
		return snapshot.Projected == 1
	})
	if got := harness.countOutboxRows(t); got != 0 {
		t.Fatalf("outbox rows after inbound ingest = %d, want zero", got)
	}

	outbox, err := sqlite.NewOutboxRepository(harness.store, harness.clock.Now)
	if err != nil {
		t.Fatalf("NewOutboxRepository(): %v", err)
	}
	projector := &v2wire.Projector{
		V2Store: harness.store,
		Outbox:  outbox,
		Legacy:  legacy,
		Blobs:   harness.blobs,
		Service: harness.service,
		Logger:  zerolog.Nop(),
		Now:     harness.clock.Now,
	}
	projectorCtx, cancelProjector := context.WithCancel(context.Background())
	projectorDone := make(chan error, 1)
	go func() { projectorDone <- projector.Run(projectorCtx) }()
	// Run scans immediately. Once it remains alive, it has reached its wait
	// loop with an empty confirmed-outbox selection.
	time.Sleep(25 * time.Millisecond)
	select {
	case err := <-projectorDone:
		t.Fatalf("Projector.Run() returned before cancellation: %v", err)
	default:
	}
	cancelProjector()
	if err := <-projectorDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("Projector.Run() error = %v, want context.Canceled", err)
	}
	if got, err := legacy.MessageCount(""); err != nil {
		t.Fatalf("legacy MessageCount(): %v", err)
	} else if got != 0 {
		t.Fatalf("legacy messages after v2 inbound projection = %d, want zero", got)
	}
}

func TestGoogleFrameDedupeAndContentHash(t *testing.T) {
	harness := newGoogleEchoHarness(t, nil)
	first := harness.messageRecord(t, googleEchoMessage(
		"google-content-hash",
		"",
		"first content",
		false,
		googleEchoNow.Add(5*time.Minute),
	))
	harness.appendRecord(t, first)
	harness.waitFor(t, "first frame", func(snapshot CounterSnapshot) bool {
		return snapshot.Projected == 1
	})
	harness.appendRecord(t, first)
	harness.waitFor(t, "exact replay dedupe", func(snapshot CounterSnapshot) bool {
		return snapshot.Deduped == 1
	})
	if got := harness.countInboxRows(t); got != 1 {
		t.Fatalf("inbox rows after exact replay = %d, want one", got)
	}

	changed := harness.messageRecord(t, googleEchoMessage(
		"google-content-hash",
		"",
		"changed content",
		false,
		googleEchoNow.Add(5*time.Minute),
	))
	if changed.DedupeKey == first.DedupeKey {
		t.Fatalf("changed same-ID frames share dedupe key %q", changed.DedupeKey)
	}
	harness.appendRecord(t, changed)
	harness.waitFor(t, "changed frame append", func(snapshot CounterSnapshot) bool {
		return snapshot.Appended == 2 && snapshot.Projected >= 3
	})
	if got := harness.countInboxRows(t); got != 2 {
		t.Fatalf("inbox rows after changed same-ID frame = %d, want two", got)
	}
	if got := harness.countV2Messages(t); got != 1 {
		t.Fatalf("messages after changed same-ID frame = %d, want one natural-key row", got)
	}
	message, err := harness.messages.GetMessageByRemote(
		context.Background(),
		googleEchoAccountID,
		googleEchoConversationID,
		"google-content-hash",
	)
	if err != nil {
		t.Fatalf("GetMessageByRemote(): %v", err)
	}
	if message.Body != "changed content" {
		t.Fatalf("message body = %q, want changed content", message.Body)
	}
}

func TestGoogleDecoderPanicIsQuarantinedAndWorkerContinues(t *testing.T) {
	counters := &Counters{}
	decoder := &panicOnMalformedGoogleDecoder{delegate: NewGoogleDecoder(counters)}
	harness := newGoogleEchoHarness(t, decoder)
	harness.appendRecord(t, bridge.RawIngressRecord{
		AccountID:    googleEchoAccountID,
		Generation:   1,
		DedupeKey:    "malformed-google-envelope",
		Codec:        GoogleCodec,
		CodecVersion: GoogleCodecVersion,
		ReceivedAt:   googleEchoNow,
		Payload:      []byte(`{"kind":"message","proto_b64":`),
	})
	harness.waitFor(t, "decoder panic quarantine", func(snapshot CounterSnapshot) bool {
		return snapshot.Quarantined == 1
	})

	harness.appendMessage(t, googleEchoMessage(
		"google-after-decoder-panic",
		"",
		"worker stayed alive",
		false,
		googleEchoNow.Add(6*time.Minute),
	))
	harness.waitFor(t, "projection after decoder panic", func(snapshot CounterSnapshot) bool {
		return snapshot.Projected == 1
	})
	if pending, err := harness.messages.Unprocessed(context.Background()); err != nil {
		t.Fatalf("Unprocessed(): %v", err)
	} else if len(pending) != 0 {
		t.Fatalf("unprocessed frames = %+v, want panic quarantined and valid frame processed", pending)
	}
}

type googleEchoHarness struct {
	store     *sqlite.Store
	storePath string
	messages  *sqlite.MessageRepository
	service   *messaging.MessageService
	sink      *Sink
	worker    *Worker
	counters  *Counters
	clock     *googleEchoClock
	blobs     *blob.BlobStore
	done      chan error
	cancel    context.CancelFunc
}

func newGoogleEchoHarness(t *testing.T, decoder bridge.Decoder) *googleEchoHarness {
	t.Helper()
	storePath := filepath.Join(t.TempDir(), "v2.sqlite3")
	store, err := sqlite.Open(storePath)
	if err != nil {
		t.Fatalf("sqlite.Open(): %v", err)
	}
	clock := &googleEchoClock{now: googleEchoNow}
	seedGoogleEchoStore(t, store, clock.Now())
	blobs, err := blob.New(filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatalf("blob.New(): %v", err)
	}
	sender := &googleEchoTextSender{clock: clock}
	service, err := messaging.NewMessageService(
		store,
		&googleEchoRegistry{sender: sender},
		blobs,
		clock,
		&googleEchoIDs{},
	)
	if err != nil {
		t.Fatalf("NewMessageService(): %v", err)
	}
	messages, err := sqlite.NewMessageRepository(store, clock.Now)
	if err != nil {
		t.Fatalf("NewMessageRepository(): %v", err)
	}
	counters := &Counters{}
	if decoder == nil {
		decoder = NewGoogleDecoder(counters)
	}
	worker, err := NewWorker(WorkerConfig{
		Store:        store,
		Messages:     messages,
		EchoObserver: service,
		Counters:     counters,
		Logger:       zerolog.Nop(),
		Now:          clock.Now,
		Decoders: []DecoderRegistration{{
			Codec:    GoogleCodec,
			Platform: bridge.PlatformGoogle,
			Decoder:  decoder,
		}},
	})
	if err != nil {
		t.Fatalf("NewWorker(): %v", err)
	}
	sink, err := NewSink(SinkConfig{
		Messages: messages,
		Worker:   worker,
		Counters: counters,
		IDs:      &googleEchoIDs{prefix: "inbox"},
	})
	if err != nil {
		t.Fatalf("NewSink(): %v", err)
	}
	workerCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- worker.Run(workerCtx) }()

	harness := &googleEchoHarness{
		store:     store,
		storePath: storePath,
		messages:  messages,
		service:   service,
		sink:      sink,
		worker:    worker,
		counters:  counters,
		clock:     clock,
		blobs:     blobs,
		done:      done,
		cancel:    cancel,
	}
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Worker.Run() cleanup error: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Error("Worker.Run() did not stop")
		}
		if err := store.Close(); err != nil {
			t.Errorf("Store.Close(): %v", err)
		}
	})
	return harness
}

func seedGoogleEchoStore(t *testing.T, store *sqlite.Store, now time.Time) {
	t.Helper()
	nowMS := now.UnixMilli()
	if err := store.UpsertAccount(sqlite.Account{
		AccountID:   googleEchoAccountID,
		BridgeKey:   "google_messages",
		DisplayName: "Google Messages",
		Mode:        sqlite.AccountModeLive,
		Enabled:     true,
		ConfigJSON:  `{}`,
		CreatedAtMS: nowMS,
		UpdatedAtMS: nowMS,
	}); err != nil {
		t.Fatalf("UpsertAccount(): %v", err)
	}
	if err := store.UpsertConversation(sqlite.Conversation{
		ConversationID:       googleEchoConversationID,
		AccountID:            googleEchoAccountID,
		RemoteConversationID: googleEchoRemoteConversationID,
		Kind:                 sqlite.ConversationKindDirect,
		Title:                "Echo test",
		NotificationMode:     sqlite.NotificationModeAll,
		MetadataJSON:         `{}`,
		CreatedAtMS:          nowMS,
		UpdatedAtMS:          nowMS,
	}); err != nil {
		t.Fatalf("UpsertConversation(): %v", err)
	}
}

func (h *googleEchoHarness) enqueue(t *testing.T, key, body string) (messaging.Submission, string) {
	t.Helper()
	submission, err := h.service.SendText(context.Background(), messaging.SendTextCommand{
		CommonCommand: messaging.CommonCommand{
			AccountID:      googleEchoAccountID,
			ConversationID: googleEchoConversationID,
			IdempotencyKey: "google-echo-" + key,
		},
		Body: body,
	})
	if err != nil {
		t.Fatalf("MessageService.SendText(): %v", err)
	}
	outbox, err := sqlite.NewOutboxRepository(h.store, h.clock.Now)
	if err != nil {
		t.Fatalf("NewOutboxRepository(): %v", err)
	}
	item, err := outbox.FindByID(context.Background(), submission.OutboxID)
	if err != nil {
		t.Fatalf("FindByID(): %v", err)
	}
	return submission, item.TransportRequestID
}

func (h *googleEchoHarness) enqueueAndConfirm(
	t *testing.T,
	key string,
	body string,
) (messaging.Submission, string) {
	t.Helper()
	submission, requestID := h.enqueue(t, key, body)
	h.confirm(t, submission.OutboxID)
	return submission, requestID
}

func (h *googleEchoHarness) confirm(t *testing.T, outboxID string) {
	t.Helper()
	processed, err := h.service.DispatchDue(context.Background(), 1)
	if err != nil || processed != 1 {
		t.Fatalf("DispatchDue() = %d, %v; want 1, nil", processed, err)
	}
	delivery, err := h.service.Get(context.Background(), outboxID)
	if err != nil {
		t.Fatalf("MessageService.Get(): %v", err)
	}
	if delivery.State != messaging.OutboxConfirmed || delivery.RemoteMessageID == "" {
		t.Fatalf("provisional delivery = %+v, want confirmed with request ID", delivery)
	}
}

func (h *googleEchoHarness) appendMessage(t *testing.T, message *gmproto.Message) {
	t.Helper()
	h.appendRecord(t, h.messageRecord(t, message))
}

func (h *googleEchoHarness) appendRecord(t *testing.T, record bridge.RawIngressRecord) {
	t.Helper()
	if err := h.sink.AppendIngress(context.Background(), record); err != nil {
		t.Fatalf("AppendIngress(): %v", err)
	}
}

func (h *googleEchoHarness) messageRecord(
	t *testing.T,
	message *gmproto.Message,
) bridge.RawIngressRecord {
	t.Helper()
	protoBytes, err := proto.Marshal(message)
	if err != nil {
		t.Fatalf("proto.Marshal(): %v", err)
	}
	payload, err := json.Marshal(struct {
		Kind     string `json:"kind"`
		ProtoB64 []byte `json:"proto_b64"`
		IsOld    bool   `json:"is_old"`
	}{
		Kind:     "message",
		ProtoB64: protoBytes,
	})
	if err != nil {
		t.Fatalf("json.Marshal(): %v", err)
	}
	hash := sha256.Sum256(protoBytes)
	return bridge.RawIngressRecord{
		AccountID:    googleEchoAccountID,
		Generation:   1,
		DedupeKey:    "msg:" + message.GetMessageID() + ":" + hex.EncodeToString(hash[:])[:8],
		Codec:        GoogleCodec,
		CodecVersion: GoogleCodecVersion,
		ReceivedAt:   googleEchoNow,
		Payload:      payload,
	}
}

func googleEchoMessage(
	remoteID string,
	tmpID string,
	body string,
	fromMe bool,
	occurredAt time.Time,
) *gmproto.Message {
	participant := &gmproto.Participant{
		IsMe:     fromMe,
		FullName: "Echo Sender",
		ID:       &gmproto.SmallInfo{Number: "+15551234567"},
	}
	status := gmproto.MessageStatusType_INCOMING_COMPLETE
	if fromMe {
		status = gmproto.MessageStatusType_OUTGOING_COMPLETE
	}
	return &gmproto.Message{
		MessageID:         remoteID,
		ConversationID:    googleEchoRemoteConversationID,
		TmpID:             tmpID,
		Timestamp:         occurredAt.UnixMilli() * 1000,
		SenderParticipant: participant,
		MessageStatus:     &gmproto.MessageStatus{Status: status},
		MessageInfo: []*gmproto.MessageInfo{{
			Data: &gmproto.MessageInfo_MessageContent{
				MessageContent: &gmproto.MessageContent{Content: body},
			},
		}},
	}
}

func (h *googleEchoHarness) waitFor(
	t *testing.T,
	description string,
	condition func(CounterSnapshot) bool,
) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		snapshot := h.counters.Snapshot(googleEchoAccountID)
		if condition(snapshot) {
			return
		}
		select {
		case err := <-h.done:
			t.Fatalf("Worker.Run() stopped while waiting for %s: %v", description, err)
		default:
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s; counters=%+v", description, h.counters.Snapshot(googleEchoAccountID))
}

func (h *googleEchoHarness) assertSinglePermanentMessage(
	t *testing.T,
	wantLocalID string,
	placeholderID string,
	permanentID string,
) {
	t.Helper()
	message, err := h.messages.GetMessageByRemote(
		context.Background(),
		googleEchoAccountID,
		googleEchoConversationID,
		permanentID,
	)
	if err != nil {
		t.Fatalf("GetMessageByRemote(permanent): %v", err)
	}
	if message.MessageID != wantLocalID || message.RemoteMessageID != permanentID {
		t.Fatalf("permanent message = %+v, want local %q remote %q", message, wantLocalID, permanentID)
	}
	if _, err := h.messages.GetMessageByRemote(
		context.Background(),
		googleEchoAccountID,
		googleEchoConversationID,
		placeholderID,
	); !errors.Is(err, sqlite.ErrNotFound) {
		t.Fatalf("GetMessageByRemote(placeholder) error = %v, want ErrNotFound", err)
	}
	if got := h.countV2Messages(t); got != 1 {
		t.Fatalf("messages after convergence = %d, want exactly one", got)
	}
}

func (h *googleEchoHarness) countV2Messages(t *testing.T) int {
	t.Helper()
	return countSQLiteRows(t, h.storePath,
		"SELECT COUNT(*) FROM messages WHERE account_id = ? AND conversation_id = ?",
		googleEchoAccountID,
		googleEchoConversationID,
	)
}

func (h *googleEchoHarness) countInboxRows(t *testing.T) int {
	t.Helper()
	return countSQLiteRows(t, h.storePath,
		"SELECT COUNT(*) FROM inbox WHERE account_id = ?",
		googleEchoAccountID,
	)
}

func (h *googleEchoHarness) countOutboxRows(t *testing.T) int {
	t.Helper()
	return countSQLiteRows(t, h.storePath,
		"SELECT COUNT(*) FROM outbox WHERE account_id = ?",
		googleEchoAccountID,
	)
}

func countSQLiteRows(t *testing.T, path, query string, args ...any) int {
	t.Helper()
	inspection, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open(inspect): %v", err)
	}
	defer inspection.Close()
	var count int
	if err := inspection.QueryRow(query, args...).Scan(&count); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	return count
}

type googleEchoTextSender struct {
	clock *googleEchoClock
}

func (s *googleEchoTextSender) SendText(
	_ context.Context,
	request bridge.TextRequest,
) (bridge.SendResult, error) {
	return bridge.SendResult{
		RemoteMessageID: request.RequestID,
		AcceptedAt:      s.clock.Now(),
		EchoExpected:    true,
	}, nil
}

type googleEchoRegistry struct {
	sender bridge.TextSender
}

func (r *googleEchoRegistry) Snapshot(accountID string) (bridge.Snapshot, bool) {
	if accountID != googleEchoAccountID {
		return bridge.Snapshot{}, false
	}
	return bridge.Snapshot{
		AccountID: googleEchoAccountID,
		Platform:  bridge.PlatformGoogle,
		State:     bridge.StateOnline,
	}, true
}

func (r *googleEchoRegistry) Acquire(
	ctx context.Context,
	accountID string,
	capability bridge.Capability,
) (*bridge.DispatchLease, error) {
	if ctx == nil {
		return nil, errors.New("nil acquire context")
	}
	if accountID != googleEchoAccountID {
		return nil, bridge.ErrAccountNotRegistered
	}
	if capability != bridge.CapabilityTextSend || r.sender == nil {
		return nil, bridge.ErrCapabilityUnavailable
	}
	return &bridge.DispatchLease{
		AccountID: googleEchoAccountID,
		Platform:  bridge.PlatformGoogle,
		Ctx:       ctx,
		Text:      r.sender,
	}, nil
}

func (r *googleEchoRegistry) Capabilities(accountID string) bridge.CapabilitySet {
	return bridge.CapabilitySet{TextSend: accountID == googleEchoAccountID && r.sender != nil}
}

type googleEchoClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *googleEchoClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *googleEchoClock) NewTimer(delay time.Duration) messaging.Timer {
	return googleEchoTimer{Timer: time.NewTimer(delay)}
}

type googleEchoTimer struct{ *time.Timer }

func (t googleEchoTimer) C() <-chan time.Time { return t.Timer.C }

type googleEchoIDs struct {
	mu     sync.Mutex
	prefix string
	next   int
}

func (s *googleEchoIDs) NewID() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next++
	prefix := s.prefix
	if prefix == "" {
		prefix = "echo"
	}
	return fmt.Sprintf("%s-%03d", prefix, s.next), nil
}

type panicOnMalformedGoogleDecoder struct {
	delegate bridge.Decoder
}

func (d *panicOnMalformedGoogleDecoder) Decode(
	ctx context.Context,
	record bridge.RawIngressRecord,
) ([]bridge.Event, error) {
	if !json.Valid(record.Payload) {
		panic("malformed Google envelope")
	}
	return d.delegate.Decode(ctx, record)
}

// A group RCS message can arrive with a sender that has only a display name —
// the number lives in the conversation roster, not the message. The v2 ingest
// path must project it with a null sender rather than quarantining real content
// on an unresolvable identity (the legacy store tolerates an empty sender).
func TestGoogleIngestNumberlessSenderProjectsWithNullSender(t *testing.T) {
	harness := newGoogleEchoHarness(t, nil)

	const remoteID = "google-numberless-sender"
	message := &gmproto.Message{
		MessageID:      remoteID,
		ConversationID: googleEchoRemoteConversationID,
		Timestamp:      googleEchoNow.Add(time.Minute).UnixMilli() * 1000,
		SenderParticipant: &gmproto.Participant{
			IsMe:     false,
			FullName: "Roster Only",
			// No ID/Number: the identifying number is not on the message.
		},
		MessageStatus: &gmproto.MessageStatus{Status: gmproto.MessageStatusType_INCOMING_COMPLETE},
		MessageInfo: []*gmproto.MessageInfo{{
			Data: &gmproto.MessageInfo_MessageContent{
				MessageContent: &gmproto.MessageContent{Content: "group message from a roster-only sender"},
			},
		}},
	}
	harness.appendRecord(t, harness.messageRecord(t, message))
	harness.waitFor(t, "numberless sender projection", func(snapshot CounterSnapshot) bool {
		return snapshot.Projected == 1
	})

	if snapshot := harness.counters.Snapshot(googleEchoAccountID); snapshot.Quarantined != 0 {
		t.Fatalf("numberless sender quarantined the message: counters=%+v", snapshot)
	}
	stored, err := harness.messages.GetMessageByRemote(
		context.Background(),
		googleEchoAccountID,
		googleEchoConversationID,
		remoteID,
	)
	if err != nil {
		t.Fatalf("GetMessageByRemote(numberless): %v", err)
	}
	if stored.Body != "group message from a roster-only sender" {
		t.Fatalf("projected body = %q, want the roster-only message", stored.Body)
	}
	if stored.SenderIdentityID != nil {
		t.Fatalf("sender_identity_id = %v, want NULL for an unresolvable sender", *stored.SenderIdentityID)
	}
	nullSenderRows := countSQLiteRows(t, harness.storePath,
		"SELECT COUNT(*) FROM messages WHERE account_id = ? AND remote_message_id = ? AND sender_identity_id IS NULL",
		googleEchoAccountID, remoteID)
	if nullSenderRows != 1 {
		t.Fatalf("null-sender rows = %d, want 1", nullSenderRows)
	}
}
