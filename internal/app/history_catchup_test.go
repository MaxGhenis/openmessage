package app

// App-level tests for handing Google catch-up history to v2 ingest
// (design and invariants: ~/reviews/openmessage-history-v2-2026-10-08/DESIGN.md).
//
// Every test builds a REAL v2 side: a sqlite store, the ingest worker with
// both Google registrations exactly as cmd/v2stack.go wires them (live
// GoogleCodec plus the History:true GoogleHistoryCodec), and an ingest sink
// with deterministic counter inbox IDs. The recipe follows
// internal/bridgeadapters/google/google_test.go
// (TestIngressTeeThroughRealSinkDedupesExactFrames). The catch-up entry
// points run against the existing mockGMClient, makeConv, makeMsg and
// newTestApp in backfill_test.go.
//
// hcdIngress is the test GoogleHistoryIngress. It builds records the same way
// the adapter run does (bridgeadapters/google/google.go appendHistory:
// ingest.GoogleHistoryConversationRecord / GoogleHistoryMessageRecord, then
// AppendHistoryIngress) for account google-primary, generation 1, stamped
// with time.Now(). It calls ingest.Sink directly, so the supervisor's
// generation fence is not exercised here.
//
// Known, intended legacy/v2 divergences (DESIGN "Known, intended
// divergences"). The differential excludes them from equality and asserts
// each one explicitly:
//
//	D2 occurred ms <= 0: legacy stores the row (timestamp_ms 0); v2
//	   quarantines the frame because occurred_at_ms must be positive.
//	D1 a contentless message with a nil MessageStatus: legacy reads the status
//	   as "unknown" and skips it as an empty stub; the decoder reads "" and
//	   keeps an empty-body row. This is tracked separately.
//	Placement: v2 may file a 1:1 message by sender identity (#176), so messages
//	   are matched by remote message ID. Placement is asserted only for groups.
//
// The generator also keeps away from #176's roster-merge design, which is
// not under test here. Every group roster includes a member unique to that
// group, because findThreadByRoster would bind two remote IDs with identical
// rosters to one thread. Every 1:1 peer is unique. Every message gets a
// unique millisecond timestamp, because FindMessageContentDuplicate would
// otherwise fold identical body+time+sender messages.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/quick"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/mautrix-gmessages/pkg/libgm"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/client"
	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/ingest"
	"github.com/maxghenis/openmessage/internal/messaging"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
	"github.com/maxghenis/openmessage/internal/v2keys"
)

const (
	hcdAccountID  = "google-primary"
	hcdSelfNumber = "+15550009999"
	// hcdBaseMS is the first generated message time (2025-10-09 UTC).
	hcdBaseMS int64 = 1_760_000_000_000
)

const hcdGeneration bridge.Generation = 1

// ---------------------------------------------------------------------------
// v2 side
// ---------------------------------------------------------------------------

type hcdV2 struct {
	path     string
	store    *sqlite.Store
	messages *sqlite.MessageRepository
	counters *ingest.Counters
	sink     *ingest.Sink
	echoes   *hcdEchoObserver
	inspect  *sql.DB

	stopOnce sync.Once
	stopFn   func()
}

// hcdEchoObserver stands in for messaging.MessageService, which production
// wires as the worker's EchoObserver (cmd/v2stack.go). It owns no outbox
// rows, so every echo reports not-found, and it counts how often history
// frames reach echo reconciliation.
type hcdEchoObserver struct{ calls atomic.Int64 }

func (o *hcdEchoObserver) ObserveTransportEcho(
	context.Context,
	messaging.TransportEcho,
) (messaging.EchoOutcome, error) {
	o.calls.Add(1)
	return messaging.EchoNotFound, nil
}

func hcdNewV2(t *testing.T) *hcdV2 {
	t.Helper()
	path := filepath.Join(t.TempDir(), "v2.sqlite3")
	store, err := sqlite.Open(path)
	if err != nil {
		t.Fatalf("sqlite.Open(): %v", err)
	}
	nowMS := time.Now().UnixMilli()
	if err := store.UpsertAccount(sqlite.Account{
		AccountID:   hcdAccountID,
		BridgeKey:   "google_messages",
		DisplayName: "Google Messages",
		Mode:        sqlite.AccountModeLive,
		Enabled:     true,
		ConfigJSON:  "{}",
		CreatedAtMS: nowMS,
		UpdatedAtMS: nowMS,
	}); err != nil {
		_ = store.Close()
		t.Fatalf("UpsertAccount(): %v", err)
	}
	messages, err := sqlite.NewMessageRepository(store, time.Now)
	if err != nil {
		_ = store.Close()
		t.Fatalf("NewMessageRepository(): %v", err)
	}
	reactions, err := sqlite.NewReactionRepository(store, time.Now)
	if err != nil {
		_ = store.Close()
		t.Fatalf("NewReactionRepository(): %v", err)
	}
	counters := &ingest.Counters{}
	echoes := &hcdEchoObserver{}
	worker, err := ingest.NewWorker(ingest.WorkerConfig{
		Store:        store,
		Messages:     messages,
		Reactions:    reactions,
		EchoObserver: echoes,
		Counters:     counters,
		Logger:       zerolog.Nop(),
		Decoders: []ingest.DecoderRegistration{
			{
				Codec:    ingest.GoogleCodec,
				Platform: bridge.PlatformGoogle,
				Decoder:  ingest.NewGoogleDecoder(counters),
			},
			{
				Codec:    ingest.GoogleHistoryCodec,
				Platform: bridge.PlatformGoogle,
				Decoder:  ingest.NewGoogleDecoder(counters),
				History:  true,
			},
		},
	})
	if err != nil {
		_ = store.Close()
		t.Fatalf("NewWorker(): %v", err)
	}
	var next atomic.Uint64
	sink, err := ingest.NewSink(ingest.SinkConfig{
		Messages: messages,
		Worker:   worker,
		Counters: counters,
		IDs: messaging.IDSourceFunc(func() (string, error) {
			return fmt.Sprintf("hcd-%08d", next.Add(1)), nil
		}),
	})
	if err != nil {
		_ = store.Close()
		t.Fatalf("NewSink(): %v", err)
	}
	inspect, err := sql.Open("sqlite", path)
	if err != nil {
		_ = store.Close()
		t.Fatalf("sql.Open(inspect): %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()

	v := &hcdV2{
		path:     path,
		store:    store,
		messages: messages,
		counters: counters,
		sink:     sink,
		echoes:   echoes,
		inspect:  inspect,
	}
	v.stopFn = func() {
		cancel()
		select {
		case runErr := <-done:
			if runErr != nil {
				t.Errorf("Worker.Run(): %v", runErr)
			}
		case <-time.After(5 * time.Second):
			t.Error("Worker.Run() did not stop")
		}
		_ = inspect.Close()
		_ = store.Close()
	}
	t.Cleanup(v.stop)
	return v
}

func (v *hcdV2) stop() { v.stopOnce.Do(v.stopFn) }

// drain waits until the worker has settled: every inbox row is processed and
// its counters have stopped moving. An empty unprocessed set alone is not
// enough. A re-fetched frame is replayed from the worker's in-memory queue
// without ever being an unprocessed row, and a counter is bumped just after
// its row is marked processed; on a slow machine a test that read the counters
// the moment the inbox emptied saw them a few frames short.
func (v *hcdV2) drain(t *testing.T) {
	t.Helper()
	const quiet = 15 // consecutive unchanged polls, 10 ms apart
	deadline := time.Now().Add(30 * time.Second)
	last := v.snapshot()
	stable := 0
	for {
		pending, err := v.messages.Unprocessed(context.Background())
		if err != nil {
			t.Fatalf("Unprocessed(): %v", err)
		}
		current := v.snapshot()
		if len(pending) == 0 && current == last {
			stable++
			if stable >= quiet {
				return
			}
		} else {
			stable = 0
			last = current
		}
		if time.Now().After(deadline) {
			t.Fatalf("v2 ingest did not settle before the deadline: %d unprocessed frames, counters %+v", len(pending), current)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// await waits for the worker's counters to satisfy done. Use it for counts
// that come from replays, which the worker handles after the inbox is empty.
func (v *hcdV2) await(t *testing.T, what string, done func(ingest.CounterSnapshot) bool) ingest.CounterSnapshot {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		current := v.snapshot()
		if done(current) {
			return current
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s; counters %+v", what, current)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (v *hcdV2) snapshot() ingest.CounterSnapshot { return v.counters.Snapshot(hcdAccountID) }

func (v *hcdV2) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := v.inspect.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", query, err)
	}
	return n
}

// ---------------------------------------------------------------------------
// Test GoogleHistoryIngress
// ---------------------------------------------------------------------------

type hcdOffer struct {
	Kind           string // "conv" or "msg"
	ConversationID string
	SnapshotID     string // ID of the conversation snapshot carried with a message; "" when nil
	MessageID      string
}

type hcdIngress struct {
	sink *ingest.Sink // nil: record offers only
	// script, when set, decides each call's fate by its 1-based call number
	// before anything is appended; a non-nil error is returned in place of
	// the append.
	script func(call int) error

	mu     sync.Mutex
	calls  int
	offers []hcdOffer
}

var _ GoogleHistoryIngress = (*hcdIngress)(nil)

func (h *hcdIngress) AppendHistoryConversation(ctx context.Context, conversation *gmproto.Conversation) error {
	if err := h.begin(hcdOffer{Kind: "conv", ConversationID: conversation.GetConversationID()}); err != nil {
		return err
	}
	if h.sink == nil {
		return nil
	}
	record, err := ingest.GoogleHistoryConversationRecord(hcdAccountID, hcdGeneration, conversation, time.Now())
	if err != nil {
		return err
	}
	return h.sink.AppendHistoryIngress(ctx, record)
}

func (h *hcdIngress) AppendHistoryMessage(
	ctx context.Context,
	conversationID string,
	conversation *gmproto.Conversation,
	message *gmproto.Message,
) error {
	offer := hcdOffer{Kind: "msg", ConversationID: conversationID, MessageID: message.GetMessageID()}
	if conversation != nil {
		offer.SnapshotID = conversation.GetConversationID()
	}
	if err := h.begin(offer); err != nil {
		return err
	}
	if h.sink == nil {
		return nil
	}
	record, err := ingest.GoogleHistoryMessageRecord(
		hcdAccountID,
		hcdGeneration,
		conversationID,
		conversation,
		message,
		time.Now(),
	)
	if err != nil {
		return err
	}
	return h.sink.AppendHistoryIngress(ctx, record)
}

func (h *hcdIngress) begin(offer hcdOffer) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls++
	h.offers = append(h.offers, offer)
	if h.script != nil {
		return h.script(h.calls)
	}
	return nil
}

func (h *hcdIngress) snapshot() (int, []hcdOffer) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls, append([]hcdOffer(nil), h.offers...)
}

// offeredConversationIDs is every conversation the ingress was handed, as a
// conversation frame or as a message's conversation.
func (h *hcdIngress) offeredConversationIDs() map[string]bool {
	_, offers := h.snapshot()
	ids := map[string]bool{}
	for _, offer := range offers {
		ids[offer.ConversationID] = true
	}
	return ids
}

// ---------------------------------------------------------------------------
// Recording GM client (wraps mockGMClient)
// ---------------------------------------------------------------------------

type hcdListCall struct {
	Count  int
	Folder gmproto.ListConversationsRequest_Folder
	Cursor string
}

type hcdFetchCall struct {
	ConversationID string
	Count          int64
	Cursor         string
}

// hcdRecordingGM records every call and every message the catch-up was
// handed. With honorCount it trims message pages to the requested count,
// as the phone does. mockGMClient ignores count.
type hcdRecordingGM struct {
	*mockGMClient
	honorCount bool

	mu        sync.Mutex
	listCalls []hcdListCall
	fetches   []hcdFetchCall
	fetched   map[string]*gmproto.Message
}

func hcdNewRecordingGM(mock *mockGMClient) *hcdRecordingGM {
	return &hcdRecordingGM{mockGMClient: mock, fetched: map[string]*gmproto.Message{}}
}

func (g *hcdRecordingGM) ListConversationsWithCursor(
	count int,
	folder gmproto.ListConversationsRequest_Folder,
	cursor *gmproto.Cursor,
) (*gmproto.ListConversationsResponse, error) {
	g.mu.Lock()
	g.listCalls = append(g.listCalls, hcdListCall{Count: count, Folder: folder, Cursor: cursor.GetLastItemID()})
	g.mu.Unlock()
	return g.mockGMClient.ListConversationsWithCursor(count, folder, cursor)
}

func (g *hcdRecordingGM) FetchMessages(
	conversationID string,
	count int64,
	cursor *gmproto.Cursor,
) (*gmproto.ListMessagesResponse, error) {
	resp, err := g.mockGMClient.FetchMessages(conversationID, count, cursor)
	g.mu.Lock()
	defer g.mu.Unlock()
	g.fetches = append(g.fetches, hcdFetchCall{ConversationID: conversationID, Count: count, Cursor: cursor.GetLastItemID()})
	if err != nil || resp == nil {
		return resp, err
	}
	if g.honorCount && int64(len(resp.Messages)) > count {
		resp = &gmproto.ListMessagesResponse{Messages: resp.Messages[:count], Cursor: resp.Cursor}
	}
	for _, msg := range resp.GetMessages() {
		g.fetched[msg.GetMessageID()] = msg
	}
	return resp, nil
}

func (g *hcdRecordingGM) fetchedMessages() map[string]*gmproto.Message {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make(map[string]*gmproto.Message, len(g.fetched))
	for id, msg := range g.fetched {
		out[id] = msg
	}
	return out
}

func (g *hcdRecordingGM) fetchCursors(conversationID string) []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	var cursors []string
	for _, call := range g.fetches {
		if call.ConversationID == conversationID {
			cursors = append(cursors, call.Cursor)
		}
	}
	return cursors
}

// ---------------------------------------------------------------------------
// App with a file-backed legacy store
// ---------------------------------------------------------------------------

// hcdNewApp is newTestApp (backfill_test.go) with a file-backed legacy store,
// so the test can read every legacy column over a second connection, and with
// the recording client and the history ingress installed through the test
// seams (gmClient, gmHistory).
func hcdNewApp(t *testing.T, mock *mockGMClient, history GoogleHistoryIngress) (*App, *hcdRecordingGM, *sql.DB) {
	t.Helper()
	// Keep the avatar sync goroutine off so nothing but the catch-up calls
	// the mock or writes the legacy store.
	t.Setenv("OPENMESSAGE_GOOGLE_AVATAR_SYNC", "0")
	a := newTestApp(t, mock)
	path := filepath.Join(t.TempDir(), "messages.db")
	store, err := db.New(path)
	if err != nil {
		t.Fatalf("db.New(%s): %v", path, err)
	}
	t.Cleanup(func() { _ = store.Close() })
	a.Store = store
	gm := hcdNewRecordingGM(mock)
	a.gmClient = gm
	a.gmHistory = history
	inspect, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open(legacy inspect): %v", err)
	}
	t.Cleanup(func() { _ = inspect.Close() })
	return a, gm, inspect
}

// ---------------------------------------------------------------------------
// Store readers
// ---------------------------------------------------------------------------

type hcdLegacyMessage struct {
	MessageID      string
	ConversationID string
	SenderNumber   string
	Body           string
	TimestampMS    int64
	Status         string
	IsFromMe       bool
	MediaID        string
	MimeType       string
	DecryptionKey  string
	ReplyToID      string
}

func hcdLegacyMessages(t *testing.T, legacy *sql.DB) map[string]hcdLegacyMessage {
	t.Helper()
	rows, err := legacy.Query(`
		SELECT message_id, conversation_id, sender_number, body, timestamp_ms, status,
		       is_from_me, media_id, mime_type, decryption_key, reply_to_id
		FROM messages`)
	if err != nil {
		t.Fatalf("query legacy messages: %v", err)
	}
	defer rows.Close()
	out := map[string]hcdLegacyMessage{}
	for rows.Next() {
		var m hcdLegacyMessage
		if err := rows.Scan(&m.MessageID, &m.ConversationID, &m.SenderNumber, &m.Body, &m.TimestampMS, &m.Status,
			&m.IsFromMe, &m.MediaID, &m.MimeType, &m.DecryptionKey, &m.ReplyToID); err != nil {
			t.Fatalf("scan legacy message: %v", err)
		}
		out[m.MessageID] = m
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate legacy messages: %v", err)
	}
	return out
}

func hcdLegacyConversationIDs(t *testing.T, legacy *sql.DB) map[string]bool {
	t.Helper()
	rows, err := legacy.Query(`SELECT conversation_id FROM conversations`)
	if err != nil {
		t.Fatalf("query legacy conversations: %v", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan legacy conversation: %v", err)
		}
		out[id] = true
	}
	return out
}

type hcdV2Message struct {
	MessageID            string
	RemoteMessageID      string
	Body                 string
	OccurredAtMS         int64
	Direction            string
	SenderCanonical      string
	ReplyTo              string
	RemoteConversationID string
	ConversationKind     string
	HasAttachment        bool
	AttachmentRemoteID   string
	AttachmentMIME       string
	AttachmentRemoteRef  []byte
	Rows                 int // rows sharing this remote message ID (must be 1)
}

func hcdV2Messages(t *testing.T, v2 *sql.DB) map[string]*hcdV2Message {
	t.Helper()
	rows, err := v2.Query(`
		SELECT m.message_id, m.remote_message_id, m.body, m.occurred_at_ms, m.direction,
		       COALESCE(i.canonical_value, ''), COALESCE(m.reply_to_remote_id, ''),
		       c.remote_conversation_id, c.kind,
		       a.message_id IS NOT NULL, COALESCE(a.remote_id, ''), COALESCE(a.mime, ''),
		       COALESCE(a.remote_ref, x'')
		FROM messages m
		JOIN conversations c ON c.conversation_id = m.conversation_id
		LEFT JOIN identities i ON i.identity_id = m.sender_identity_id
		LEFT JOIN message_attachments a ON a.message_id = m.message_id AND a.ordinal = 0
		WHERE m.account_id = ?`, hcdAccountID)
	if err != nil {
		t.Fatalf("query v2 messages: %v", err)
	}
	defer rows.Close()
	out := map[string]*hcdV2Message{}
	for rows.Next() {
		var m hcdV2Message
		if err := rows.Scan(&m.MessageID, &m.RemoteMessageID, &m.Body, &m.OccurredAtMS, &m.Direction,
			&m.SenderCanonical, &m.ReplyTo, &m.RemoteConversationID, &m.ConversationKind,
			&m.HasAttachment, &m.AttachmentRemoteID, &m.AttachmentMIME, &m.AttachmentRemoteRef); err != nil {
			t.Fatalf("scan v2 message: %v", err)
		}
		if prior, ok := out[m.RemoteMessageID]; ok {
			prior.Rows++
			continue
		}
		m.Rows = 1
		out[m.RemoteMessageID] = &m
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate v2 messages: %v", err)
	}
	return out
}

func hcdV2ConversationRemoteIDs(t *testing.T, v2 *sql.DB) map[string]string {
	t.Helper()
	rows, err := v2.Query(`SELECT remote_conversation_id, kind FROM conversations WHERE account_id = ?`, hcdAccountID)
	if err != nil {
		t.Fatalf("query v2 conversations: %v", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, kind string
		if err := rows.Scan(&id, &kind); err != nil {
			t.Fatalf("scan v2 conversation: %v", err)
		}
		out[id] = kind
	}
	return out
}

// hcdDumpTable renders every row of query with every column, for whole-table
// equality checks.
func hcdDumpTable(t *testing.T, database *sql.DB, query string, args ...any) []string {
	t.Helper()
	rows, err := database.Query(query, args...)
	if err != nil {
		t.Fatalf("dump %q: %v", query, err)
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		t.Fatalf("dump columns: %v", err)
	}
	var out []string
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			t.Fatalf("dump scan: %v", err)
		}
		parts := make([]string, len(columns))
		for i, value := range values {
			if raw, ok := value.([]byte); ok {
				value = string(raw)
			}
			parts[i] = fmt.Sprintf("%s=%#v", columns[i], value)
		}
		out = append(out, strings.Join(parts, " "))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("dump iterate: %v", err)
	}
	return out
}

// ---------------------------------------------------------------------------
// Oracle helpers: the two stub policies, sender canonicalization
// ---------------------------------------------------------------------------

func hcdStatus(msg *gmproto.Message, nilStatus string) string {
	if status := msg.GetMessageStatus(); status != nil {
		return status.GetStatus().String()
	}
	return nilStatus
}

func hcdIsStub(msg *gmproto.Message, nilStatus string) bool {
	mediaID := ""
	if media := client.ExtractMediaInfo(msg); media != nil {
		mediaID = media.MediaID
	}
	reactions := ""
	if client.ExtractReactions(msg) != nil {
		reactions = "present"
	}
	return db.IsEmptyStubMessage(&db.Message{
		Body:      client.ExtractMessageBody(msg),
		MediaID:   mediaID,
		Reactions: reactions,
		Status:    hcdStatus(msg, nilStatus),
	})
}

// hcdLegacyStub is storeMessage's policy (backfill.go: nil status is "unknown").
func hcdLegacyStub(msg *gmproto.Message) bool { return hcdIsStub(msg, "unknown") }

// hcdDecoderStub is googleMessageIsEmptyStub's policy (nil status is "").
func hcdDecoderStub(msg *gmproto.Message) bool { return hcdIsStub(msg, "") }

func hcdOccurredMS(msg *gmproto.Message) int64 { return msg.GetTimestamp() / 1000 }

func hcdCanonical(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	key, err := v2keys.IdentityKey(hcdAccountID, string(bridge.PlatformGoogle), raw)
	if err != nil {
		return "<invalid " + raw + ">"
	}
	return key.Canonical
}

// ---------------------------------------------------------------------------
// Generator (testing/quick, custom Generate)
// ---------------------------------------------------------------------------

type hcdConv struct {
	Conv   *gmproto.Conversation
	Folder gmproto.ListConversationsRequest_Folder
	Group  bool
	Phone  string
	Pages  [][]*gmproto.Message // newest first
}

type hcdScenario struct {
	Convs []*hcdConv
}

func (hcdScenario) Generate(r *rand.Rand, _ int) reflect.Value {
	return reflect.ValueOf(hcdGenerateScenario(r))
}

// GoString keeps quick.Check's failure report readable.
func (s hcdScenario) GoString() string {
	var b strings.Builder
	for _, c := range s.Convs {
		fmt.Fprintf(&b, "conv %s group=%t folder=%s roster=%d last=%d phone=%q pages=",
			c.Conv.GetConversationID(), c.Group, c.Folder, len(c.Conv.GetParticipants()),
			c.Conv.GetLastMessageTimestamp(), c.Phone)
		for _, page := range c.Pages {
			b.WriteString("[")
			for _, m := range page {
				fmt.Fprintf(&b, "%s@%d(out=%t,status=%s,body=%q,media=%t,stubL=%t,stubD=%t) ",
					m.GetMessageID(), m.GetTimestamp(), client.MessageIsFromMe(m), hcdStatus(m, "nil"),
					client.ExtractMessageBody(m), client.ExtractMediaInfo(m) != nil, hcdLegacyStub(m), hcdDecoderStub(m))
			}
			b.WriteString("]")
		}
		b.WriteString("\n")
	}
	return b.String()
}

func (s *hcdScenario) mock() *mockGMClient {
	mock := &mockGMClient{
		conversations:      map[gmproto.ListConversationsRequest_Folder][][]*gmproto.Conversation{},
		messages:           map[string][][]*gmproto.Message{},
		getOrCreateResults: map[string]*gmproto.Conversation{},
		fetchCalls:         map[string]int{},
	}
	byFolder := map[gmproto.ListConversationsRequest_Folder][]*gmproto.Conversation{}
	for _, c := range s.Convs {
		byFolder[c.Folder] = append(byFolder[c.Folder], c.Conv)
		mock.messages[c.Conv.GetConversationID()] = c.Pages
		mock.getOrCreateResults[c.Phone] = c.Conv
	}
	// Two conversations per listing page, so deep/window backfills page the
	// folders and shallow backfill/reconcile see only the first page.
	for folder, convs := range byFolder {
		for i := 0; i < len(convs); i += 2 {
			mock.conversations[folder] = append(mock.conversations[folder], convs[i:min(i+2, len(convs))])
		}
	}
	return mock
}

func (s *hcdScenario) groups() map[string]bool {
	out := map[string]bool{}
	for _, c := range s.Convs {
		out[c.Conv.GetConversationID()] = c.Group
	}
	return out
}

var hcdWords = []string{
	"hello", "on my way", "lunch?", "ok", "see you at 5", "café ☕", "call me",
	"running late", "thanks!", "sounds good 👍", "can't talk", "new line\nhere",
}

func hcdFormatE164(r *rand.Rand, digits string) string {
	// digits is 11 characters: country 1, area, exchange, line.
	switch r.Intn(3) {
	case 0:
		return "+" + digits
	case 1:
		return fmt.Sprintf("+%s %s-%s-%s", digits[:1], digits[1:4], digits[4:7], digits[7:])
	default:
		return fmt.Sprintf("+%s (%s) %s-%s", digits[:1], digits[1:4], digits[4:7], digits[7:])
	}
}

func hcdParticipant(r *rand.Rand, raw, name string, isMe bool) *gmproto.Participant {
	participant := &gmproto.Participant{FullName: name, IsMe: isMe}
	if r.Intn(4) == 0 {
		// The number only in FormattedNumber: legacy (client.ExtractSenderInfo,
		// storeConversation) and the decoder both fall back to it.
		participant.ID = &gmproto.SmallInfo{ParticipantID: "pid-" + name}
		participant.FormattedNumber = raw
	} else {
		participant.ID = &gmproto.SmallInfo{Number: raw, ParticipantID: "pid-" + name}
	}
	return participant
}

func hcdPickFolder(r *rand.Rand) gmproto.ListConversationsRequest_Folder {
	switch n := r.Intn(10); {
	case n < 7:
		return gmproto.ListConversationsRequest_INBOX
	case n < 9:
		return gmproto.ListConversationsRequest_ARCHIVE
	default:
		return gmproto.ListConversationsRequest_SPAM_BLOCKED
	}
}

func hcdGenerateScenario(r *rand.Rand) hcdScenario {
	var s hcdScenario
	shared := []string{"15550300001", "15550300002", "15550300003"}
	k := 0 // global ordinal: unique message IDs and unique millisecond times
	nConvs := 1 + r.Intn(4)
	for i := 0; i < nConvs; i++ {
		convID := fmt.Sprintf("%d", 1000+10*i+r.Intn(10))
		group := r.Intn(5) < 2
		conv := &gmproto.Conversation{ConversationID: convID, IsGroupChat: group, Name: "Thread " + convID}
		c := &hcdConv{Conv: conv, Group: group, Folder: hcdPickFolder(r)}
		self := hcdParticipant(r, hcdSelfNumber, "Me", true)

		var pickSender func() *gmproto.Participant
		if group {
			members := []string{fmt.Sprintf("1555020%04d", i)} // unique to this group
			for _, digits := range shared {
				if r.Intn(2) == 0 {
					members = append(members, digits)
				}
			}
			if len(members) < 2 {
				members = append(members, shared[r.Intn(len(shared))])
			}
			conv.Participants = append(conv.Participants, self)
			for _, digits := range members {
				conv.Participants = append(conv.Participants,
					hcdParticipant(r, hcdFormatE164(r, digits), fmt.Sprintf("Member %s", digits[7:]), false))
			}
			c.Phone = "group:" + convID
			pickSender = func() *gmproto.Participant {
				switch n := r.Intn(10); {
				case n == 0:
					return nil
				case n == 1:
					// A display name only: legacy stores an empty sender and v2
					// projects a NULL sender (worker.go messageProjection).
					return &gmproto.Participant{FullName: "Unknown sender"}
				case n == 2:
					outsider := fmt.Sprintf("1555040%04d", r.Intn(10000))
					return hcdParticipant(r, hcdFormatE164(r, outsider), "Outsider", false)
				default:
					digits := members[r.Intn(len(members))]
					return hcdParticipant(r, hcdFormatE164(r, digits), fmt.Sprintf("Member %s", digits[7:]), false)
				}
			}
		} else {
			var peerRaw func() string
			if r.Intn(5) == 0 {
				code := fmt.Sprintf("%d", 22000+i) // a short code: identity kind username
				peerRaw = func() string { return code }
			} else {
				digits := fmt.Sprintf("1555010%04d", i)
				peerRaw = func() string { return hcdFormatE164(r, digits) }
			}
			if r.Intn(10) < 7 {
				conv.Participants = []*gmproto.Participant{self, hcdParticipant(r, peerRaw(), "Peer", false)}
			}
			c.Phone = peerRaw()
			pickSender = func() *gmproto.Participant {
				if r.Intn(10) == 0 {
					return &gmproto.Participant{FullName: "Peer (no number)"}
				}
				return hcdParticipant(r, peerRaw(), "Peer", false)
			}
		}

		n := 1 + r.Intn(6)
		var msgs []*gmproto.Message // oldest first
		var maxTS int64
		for j := 0; j < n; j++ {
			msg := hcdGenerateMessage(r, k, convID, pickSender, msgs)
			k++
			msgs = append(msgs, msg)
			if msg.GetTimestamp() > maxTS {
				maxTS = msg.GetTimestamp()
			}
		}
		if r.Intn(6) != 0 {
			conv.LastMessageTimestamp = maxTS
		} // else 0: the phone did not say
		sort.SliceStable(msgs, func(a, b int) bool { return msgs[a].GetTimestamp() > msgs[b].GetTimestamp() })
		for len(msgs) > 0 {
			size := min(1+r.Intn(3), len(msgs))
			c.Pages = append(c.Pages, msgs[:size])
			msgs = msgs[size:]
		}
		s.Convs = append(s.Convs, c)
	}
	return s
}

func hcdGenerateMessage(
	r *rand.Rand,
	k int,
	convID string,
	pickSender func() *gmproto.Participant,
	earlier []*gmproto.Message,
) *gmproto.Message {
	ms := hcdBaseMS + int64(k)*1000 + r.Int63n(1000)
	ts := ms*1000 + r.Int63n(1000) // µs; legacy and the decoder both floor to ms
	if r.Intn(15) == 0 {
		ts = r.Int63n(1000) // zero or sub-millisecond: occurred ms 0 (D2)
	}
	msg := &gmproto.Message{
		MessageID:      fmt.Sprintf("%d", 70000+3*k+r.Intn(3)),
		ConversationID: convID,
		Timestamp:      ts,
	}
	if r.Intn(100) < 35 { // outgoing
		switch r.Intn(3) {
		case 0:
			msg.SenderParticipant = hcdParticipant(r, hcdSelfNumber, "Me", true)
		case 1:
			msg.MessageStatus = &gmproto.MessageStatus{Status: gmproto.MessageStatusType_OUTGOING_DELIVERED}
		default:
			msg.SenderParticipant = hcdParticipant(r, hcdSelfNumber, "Me", true)
			msg.MessageStatus = &gmproto.MessageStatus{Status: gmproto.MessageStatusType_OUTGOING_COMPLETE}
		}
		if r.Intn(10) < 3 {
			msg.TmpID = fmt.Sprintf("tmp_%d", r.Int63())
		}
	} else {
		if r.Intn(2) == 0 {
			msg.MessageStatus = &gmproto.MessageStatus{Status: gmproto.MessageStatusType_INCOMING_COMPLETE}
		}
		msg.SenderParticipant = pickSender()
	}

	var infos []*gmproto.MessageInfo
	switch n := r.Intn(20); {
	case n < 16:
		body := hcdWords[r.Intn(len(hcdWords))]
		infos = append(infos, &gmproto.MessageInfo{Data: &gmproto.MessageInfo_MessageContent{
			MessageContent: &gmproto.MessageContent{Content: body},
		}})
	case n == 16:
		// Whitespace only: both stub policies trim it.
		infos = append(infos, &gmproto.MessageInfo{Data: &gmproto.MessageInfo_MessageContent{
			MessageContent: &gmproto.MessageContent{Content: "  "},
		}})
	}
	if r.Intn(4) == 0 {
		mimes := []string{"image/jpeg", "video/mp4", "image/png", ""}
		media := &gmproto.MediaContent{
			MediaID:  fmt.Sprintf("media-%d-%x", k, r.Uint32()),
			MimeType: mimes[r.Intn(len(mimes))],
		}
		if media.MimeType == "" {
			// client.ExtractMediaInfo derives the MIME from the format.
			media.Format = gmproto.MediaFormats(r.Intn(10))
		}
		if r.Intn(4) != 0 {
			key := make([]byte, 1+r.Intn(16))
			_, _ = r.Read(key)
			media.DecryptionKey = key
		}
		if r.Intn(2) == 0 {
			media.MediaName = "photo.jpg"
			media.Size = int64(r.Intn(1 << 20))
		}
		infos = append(infos, &gmproto.MessageInfo{Data: &gmproto.MessageInfo_MediaContent{MediaContent: media}})
	}
	r.Shuffle(len(infos), func(a, b int) { infos[a], infos[b] = infos[b], infos[a] })
	msg.MessageInfo = infos

	if r.Intn(10) == 0 {
		emojis := []string{"👍", "❤️", "😂"}
		actors := []string{"pid-react-a"}
		if r.Intn(2) == 0 {
			actors = append(actors, "pid-react-b")
		}
		msg.Reactions = []*gmproto.ReactionEntry{{
			Data:           &gmproto.ReactionData{Unicode: emojis[r.Intn(len(emojis))]},
			ParticipantIDs: actors,
		}}
	}
	if r.Intn(5) == 0 {
		target := fmt.Sprintf("%d", 99000+r.Intn(1000)) // not fetched
		if len(earlier) > 0 && r.Intn(2) == 0 {
			target = earlier[r.Intn(len(earlier))].GetMessageID()
		}
		msg.ReplyMessage = &gmproto.ReplyMessage{MessageID: target, ConversationID: convID}
	}
	return msg
}

// ---------------------------------------------------------------------------
// Catch-up entry points
// ---------------------------------------------------------------------------

type hcdEntry struct {
	name string
	run  func(t *testing.T, a *App, s *hcdScenario)
}

func hcdEntryPoints() []hcdEntry {
	return []hcdEntry{
		{"DeepBackfill", func(t *testing.T, a *App, _ *hcdScenario) {
			a.DeepBackfill()
		}},
		{"Backfill", func(t *testing.T, a *App, _ *hcdScenario) {
			if err := a.Backfill(); err != nil {
				t.Fatalf("Backfill(): %v", err)
			}
		}},
		{"reconcileRecentConversations", func(t *testing.T, a *App, _ *hcdScenario) {
			a.reconcileRunning.Store(true)
			a.reconcileRecentConversations("history_test")
		}},
		{"windowBackfill", func(t *testing.T, a *App, _ *hcdScenario) {
			if !a.beginBackfill() {
				t.Fatal("beginBackfill() refused on an idle app")
			}
			// since = 1 ms: every generated conversation and message with a
			// positive time is in the window.
			a.windowBackfill(time.UnixMilli(1))
		}},
		{"BackfillConversationByPhone", func(t *testing.T, a *App, s *hcdScenario) {
			for _, c := range s.Convs {
				if err := a.BackfillConversationByPhone(c.Phone); err != nil {
					t.Fatalf("BackfillConversationByPhone(%q): %v", c.Phone, err)
				}
			}
		}},
	}
}

// ---------------------------------------------------------------------------
// Differential check (I1)
// ---------------------------------------------------------------------------

type hcdReport struct {
	t        *testing.T
	label    string
	failures int
}

func (r *hcdReport) errorf(format string, args ...any) {
	r.t.Helper()
	r.failures++
	if r.failures <= 30 {
		r.t.Errorf(r.label+": "+format, args...)
	}
}

type hcdCoverage struct {
	compared, groups, outgoing, incomingWithSender, incomingNoSender, media,
	replies, reactions, d1, d2, shortCode, echoes int
}

func hcdCheckDifferential(
	t *testing.T,
	label string,
	s *hcdScenario,
	gm *hcdRecordingGM,
	ingress *hcdIngress,
	legacyDB *sql.DB,
	v2 *hcdV2,
	coverage *hcdCoverage,
) bool {
	t.Helper()
	report := &hcdReport{t: t, label: label}
	fetched := gm.fetchedMessages()
	legacy := hcdLegacyMessages(t, legacyDB)
	stored := hcdV2Messages(t, v2.inspect)
	groups := s.groups()

	// Oracle sanity: legacy holds exactly the fetched messages its stub
	// policy keeps (backfill.go storeMessage).
	for id, msg := range fetched {
		_, inLegacy := legacy[id]
		if want := !hcdLegacyStub(msg); inLegacy != want {
			report.errorf("legacy holds %s = %t, want %t (legacy stub policy)", id, inLegacy, want)
		}
	}
	for id := range legacy {
		if _, ok := fetched[id]; !ok {
			report.errorf("legacy holds %s, which no catch-up fetched", id)
		}
	}

	// I1: every legacy message is in v2 with equal content.
	for id, l := range legacy {
		v, inV2 := stored[id]
		if l.TimestampMS <= 0 {
			// INTENDED DIVERGENCE D2: v2 rejects a non-positive occurrence time.
			coverage.d2++
			if inV2 {
				report.errorf("D2 message %s (timestamp_ms %d) reached v2", id, l.TimestampMS)
			}
			continue
		}
		if !inV2 {
			report.errorf("legacy message %s is missing from v2", id)
			continue
		}
		coverage.compared++
		if v.Rows != 1 {
			report.errorf("v2 holds %d rows for remote message %s, want 1", v.Rows, id)
		}
		if v.Body != l.Body {
			report.errorf("message %s body: v2 %q, legacy %q", id, v.Body, l.Body)
		}
		if v.OccurredAtMS != l.TimestampMS {
			report.errorf("message %s occurred_at_ms %d, legacy timestamp_ms %d", id, v.OccurredAtMS, l.TimestampMS)
		}
		wantDirection := "incoming"
		if l.IsFromMe {
			wantDirection = "outgoing"
			coverage.outgoing++
		}
		if v.Direction != wantDirection {
			report.errorf("message %s direction %q, want %q (legacy is_from_me=%t)", id, v.Direction, wantDirection, l.IsFromMe)
		}
		// v2 records a sender identity only for incoming messages
		// (worker.go messageProjection). Legacy keeps the sender of
		// outgoing rows too, so outgoing is compared against "".
		wantSender := ""
		if !l.IsFromMe {
			wantSender = hcdCanonical(l.SenderNumber)
			if wantSender == "" {
				coverage.incomingNoSender++
			} else {
				coverage.incomingWithSender++
				if !strings.HasPrefix(wantSender, "+") {
					coverage.shortCode++
				}
			}
		}
		if v.SenderCanonical != wantSender {
			report.errorf("message %s sender canonical %q, want %q (legacy sender_number %q)",
				id, v.SenderCanonical, wantSender, l.SenderNumber)
		}
		if v.ReplyTo != l.ReplyToID {
			report.errorf("message %s reply_to_remote_id %q, legacy reply_to_id %q", id, v.ReplyTo, l.ReplyToID)
		}
		if l.ReplyToID != "" {
			coverage.replies++
		}
		if (l.MediaID != "") != v.HasAttachment {
			report.errorf("message %s attachment present=%t, legacy media_id %q", id, v.HasAttachment, l.MediaID)
		}
		if l.MediaID != "" && v.HasAttachment {
			coverage.media++
			if v.AttachmentRemoteID != l.MediaID {
				report.errorf("message %s attachment remote_id %q, legacy media_id %q", id, v.AttachmentRemoteID, l.MediaID)
			}
			if v.AttachmentMIME != l.MimeType {
				report.errorf("message %s attachment mime %q, legacy mime_type %q", id, v.AttachmentMIME, l.MimeType)
			}
			var ref struct {
				MediaID       string `json:"media_id"`
				DecryptionKey string `json:"decryption_key"`
			}
			if err := json.Unmarshal(v.AttachmentRemoteRef, &ref); err != nil {
				report.errorf("message %s attachment remote_ref %q: %v", id, v.AttachmentRemoteRef, err)
			} else if ref.MediaID != l.MediaID || ref.DecryptionKey != l.DecryptionKey {
				report.errorf("message %s remote_ref media_id/key %q/%q, legacy %q/%q",
					id, ref.MediaID, ref.DecryptionKey, l.MediaID, l.DecryptionKey)
			}
		}
		// Placement is asserted for groups only (see the file comment).
		if groups[l.ConversationID] {
			coverage.groups++
			if v.RemoteConversationID != l.ConversationID || v.ConversationKind != "group" {
				report.errorf("group message %s filed in %q (%s), want group %q",
					id, v.RemoteConversationID, v.ConversationKind, l.ConversationID)
			}
		}
		// Supplementary (DESIGN decision 4): an inserted message carries its
		// embedded reaction snapshot.
		if msg := fetched[id]; msg != nil && len(msg.GetReactions()) > 0 {
			coverage.reactions++
			entry := msg.GetReactions()[0]
			active := v2.count(t, `SELECT COUNT(*) FROM reactions WHERE message_id = ? AND state = 'active' AND emoji = ?`,
				v.MessageID, entry.GetData().GetUnicode())
			if active != len(entry.GetParticipantIDs()) {
				report.errorf("message %s holds %d active %s reactions, want %d",
					id, active, entry.GetData().GetUnicode(), len(entry.GetParticipantIDs()))
			}
		}
	}

	// v2 holds no Google message legacy lacks, except D1.
	wantInV2 := 0
	quarantined := 0
	decoderStubs := 0
	echoes := 0
	for _, msg := range fetched {
		decoderStub := hcdDecoderStub(msg)
		if decoderStub {
			decoderStubs++
		}
		if !decoderStub && hcdOccurredMS(msg) > 0 {
			wantInV2++
		}
		if !decoderStub && hcdOccurredMS(msg) <= 0 {
			quarantined++
		}
		if !decoderStub && client.MessageIsFromMe(msg) && msg.GetTmpID() != "" && msg.GetTmpID() != msg.GetMessageID() {
			echoes++
		}
	}
	for id, v := range stored {
		if _, inLegacy := legacy[id]; inLegacy {
			continue
		}
		msg, wasFetched := fetched[id]
		if !wasFetched {
			report.errorf("v2 holds %s, which no catch-up fetched", id)
			continue
		}
		// INTENDED DIVERGENCE D1: a contentless nil-status message is a
		// legacy stub ("unknown") but not a decoder stub ("").
		if !(hcdLegacyStub(msg) && !hcdDecoderStub(msg)) {
			report.errorf("v2 holds %s, which legacy lacks, and it is not a D1 contentless nil-status message", id)
			continue
		}
		coverage.d1++
		if strings.TrimSpace(v.Body) != "" || v.HasAttachment {
			report.errorf("D1 message %s holds body %q / attachment %t in v2, want contentless", id, v.Body, v.HasAttachment)
		}
	}
	if len(stored) != wantInV2 {
		report.errorf("v2 holds %d Google messages, want %d (fetched, non-stub by the decoder, positive time)", len(stored), wantInV2)
	}

	// The conversation sets agree: legacy stored every listed conversation and
	// v2 created every offered one, with no merge or reroute.
	legacyConversations := hcdLegacyConversationIDs(t, legacyDB)
	v2Conversations := hcdV2ConversationRemoteIDs(t, v2.inspect)
	offeredConversations := ingress.offeredConversationIDs()
	if !reflect.DeepEqual(hcdKeys(legacyConversations), hcdKeys(offeredConversations)) {
		report.errorf("legacy conversations %v, offered to v2 %v", hcdKeys(legacyConversations), hcdKeys(offeredConversations))
	}
	if !reflect.DeepEqual(hcdKeys(v2Conversations), hcdKeys(offeredConversations)) {
		report.errorf("v2 conversations %v, offered %v", hcdKeys(v2Conversations), hcdKeys(offeredConversations))
	}
	for id, kind := range v2Conversations {
		if want := map[bool]string{true: "group", false: "direct"}[groups[id]]; kind != want {
			report.errorf("v2 conversation %s kind %q, want %q", id, kind, want)
		}
	}

	// Counters: history never touches the live counters (DESIGN decision 6),
	// and every offered frame landed once.
	calls, _ := ingress.snapshot()
	c := v2.snapshot()
	if c.Appended != 0 || c.Deduped != 0 || c.Projected != 0 || c.Imported != 0 {
		report.errorf("live counters moved: appended=%d deduped=%d projected=%d imported=%d",
			c.Appended, c.Deduped, c.Projected, c.Imported)
	}
	if c.HistoryAppended != uint64(calls) || c.HistoryDeduped != 0 {
		report.errorf("history_appended=%d history_deduped=%d, want %d/0", c.HistoryAppended, c.HistoryDeduped, calls)
	}
	if c.HistoryImported != uint64(len(stored)) || c.HistoryExisting != 0 {
		report.errorf("history_imported=%d history_existing=%d, want %d/0", c.HistoryImported, c.HistoryExisting, len(stored))
	}
	if c.HistoryConversations != uint64(len(offeredConversations)) {
		report.errorf("history_conversations=%d, want %d", c.HistoryConversations, len(offeredConversations))
	}
	if c.Quarantined != uint64(quarantined) {
		report.errorf("quarantined=%d, want %d (non-stub frames with occurred ms <= 0)", c.Quarantined, quarantined)
	}
	if c.EmptyStubsSkipped != uint64(decoderStubs) {
		report.errorf("empty_stubs_skipped=%d, want %d", c.EmptyStubsSkipped, decoderStubs)
	}
	if c.ReactionsOrphaned != 0 {
		report.errorf("reactions_orphaned=%d, want 0 (history never counts orphans)", c.ReactionsOrphaned)
	}
	// Observed behavior, not a DESIGN invariant: a NEW outgoing history message
	// with a TmpID still reaches echo reconciliation (before the time check).
	if got := v2.echoes.calls.Load(); got != int64(echoes) {
		report.errorf("echo observer calls=%d, want %d", got, echoes)
	}
	coverage.echoes += echoes
	return report.failures == 0
}

func hcdKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ---------------------------------------------------------------------------
// 1) I1 differential across every catch-up entry point
// ---------------------------------------------------------------------------

func TestHistoryCatchUpDifferentialLegacyAndV2AgreeOnFetchedContent(t *testing.T) {
	entries := hcdEntryPoints()
	coverage := &hcdCoverage{}
	scenarios := 0
	property := func(s hcdScenario) bool {
		scenarios++
		for _, entry := range entries {
			mock := s.mock()
			v2 := hcdNewV2(t)
			ingress := &hcdIngress{sink: v2.sink}
			a, gm, legacyDB := hcdNewApp(t, mock, ingress)
			entry.run(t, a, &s)
			v2.drain(t)
			ok := hcdCheckDifferential(t, fmt.Sprintf("scenario %d %s", scenarios, entry.name),
				&s, gm, ingress, legacyDB, v2, coverage)
			v2.stop()
			if !ok {
				return false
			}
		}
		return true
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 15, Rand: rand.New(rand.NewSource(2026_10_08))}); err != nil {
		t.Fatal(err)
	}
	// The property is not vacuous: every class it is meant to cover occurred.
	t.Logf("coverage over %d scenarios x %d entry points: %+v", scenarios, len(entries), *coverage)
	for name, n := range map[string]int{
		"compared": coverage.compared, "group placement": coverage.groups, "outgoing": coverage.outgoing,
		"incoming with sender": coverage.incomingWithSender, "incoming without sender": coverage.incomingNoSender,
		"media": coverage.media, "reply": coverage.replies, "reactions": coverage.reactions,
		"D1": coverage.d1, "D2": coverage.d2, "short code sender": coverage.shortCode, "echo": coverage.echoes,
	} {
		if n == 0 {
			t.Errorf("generator never produced a compared %s case; widen it", name)
		}
	}
}

// ---------------------------------------------------------------------------
// 2) I4 legacy unchanged with and without the tee
// ---------------------------------------------------------------------------

// hcdLegacyDump is every column of the legacy conversations and messages
// tables. Neither table has a wall-clock column that backfill writes:
// messages.transcribed_at stays 0 for backfill rows. contact_avatars carries
// wall-clock updated_at_ms/last_checked_at_ms, but it is out of scope and
// avatar sync is off.
func hcdLegacyDump(t *testing.T, legacy *sql.DB) []string {
	t.Helper()
	dump := hcdDumpTable(t, legacy, `SELECT * FROM conversations ORDER BY conversation_id`)
	return append(dump, hcdDumpTable(t, legacy, `SELECT * FROM messages ORDER BY message_id`)...)
}

func TestHistoryTeeLeavesLegacyStoreIdentical(t *testing.T) {
	entries := hcdEntryPoints()
	scenarios := 0
	teed := 0
	compared := 0
	property := func(s hcdScenario) bool {
		scenarios++
		for _, entry := range entries {
			plain, _, plainDB := hcdNewApp(t, s.mock(), nil)
			entry.run(t, plain, &s)

			v2 := hcdNewV2(t)
			ingress := &hcdIngress{sink: v2.sink}
			withTee, _, teeDB := hcdNewApp(t, s.mock(), ingress)
			entry.run(t, withTee, &s)
			v2.drain(t)
			calls, _ := ingress.snapshot()
			teed += calls
			v2.stop()

			plainDump := hcdLegacyDump(t, plainDB)
			teeDump := hcdLegacyDump(t, teeDB)
			compared += len(plainDump)
			if !reflect.DeepEqual(plainDump, teeDump) {
				t.Errorf("scenario %d %s: legacy rows differ with the tee installed\nwithout: %v\nwith:    %v",
					scenarios, entry.name, plainDump, teeDump)
				return false
			}
		}
		return true
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 6, Rand: rand.New(rand.NewSource(2026_10_09))}); err != nil {
		t.Fatal(err)
	}
	if teed == 0 || compared == 0 {
		t.Fatalf("tee calls=%d, legacy rows compared=%d; the comparison proved nothing", teed, compared)
	}
}

// ---------------------------------------------------------------------------
// 3) I2 idempotence at app level
// ---------------------------------------------------------------------------

// hcdV2ContentDump is every column (timestamps included) of the v2 tables a
// history frame can write.
func hcdV2ContentDump(t *testing.T, v2 *hcdV2) []string {
	t.Helper()
	var dump []string
	for _, query := range []string{
		`SELECT * FROM conversations ORDER BY conversation_id`,
		`SELECT * FROM conversation_participants ORDER BY conversation_id, identity_id`,
		`SELECT * FROM identities ORDER BY identity_id`,
		`SELECT * FROM messages ORDER BY message_id`,
		`SELECT * FROM message_attachments ORDER BY message_id, ordinal`,
		`SELECT * FROM reactions ORDER BY message_id, reactor_key`,
		`SELECT * FROM reaction_snapshot_fences ORDER BY message_id`,
	} {
		dump = append(dump, query)
		dump = append(dump, hcdDumpTable(t, v2.inspect, query)...)
	}
	return dump
}

func TestHistoryDeepBackfillTwiceIsIdempotentInV2(t *testing.T) {
	property := func(s hcdScenario) bool {
		v2 := hcdNewV2(t)
		ingress := &hcdIngress{sink: v2.sink}
		a, _, _ := hcdNewApp(t, s.mock(), ingress)

		a.DeepBackfill()
		v2.drain(t)
		firstDump := hcdV2ContentDump(t, v2)
		firstInbox := v2.count(t, `SELECT COUNT(*) FROM inbox`)
		firstCalls, _ := ingress.snapshot()
		first := v2.snapshot()

		a.DeepBackfill()
		v2.drain(t)
		secondDump := hcdV2ContentDump(t, v2)
		secondInbox := v2.count(t, `SELECT COUNT(*) FROM inbox`)
		secondCalls, _ := ingress.snapshot()
		second := v2.snapshot()
		v2.stop()

		ok := true
		if firstCalls == 0 || secondCalls != 2*firstCalls {
			t.Errorf("ingress calls %d then %d, want n>0 then 2n", firstCalls, secondCalls)
			ok = false
		}
		if !reflect.DeepEqual(firstDump, secondDump) {
			t.Errorf("v2 content changed on the second deep backfill\nfirst:  %v\nsecond: %v", firstDump, secondDump)
			ok = false
		}
		if secondInbox != firstInbox || firstInbox != firstCalls {
			t.Errorf("inbox rows %d then %d, want %d both times", firstInbox, secondInbox, firstCalls)
			ok = false
		}
		// A byte-identical re-fetch dedupes onto its own history row, so the
		// second run is counted as history_deduped and appends nothing.
		if second.HistoryAppended != first.HistoryAppended ||
			second.HistoryDeduped-first.HistoryDeduped != uint64(secondCalls-firstCalls) {
			t.Errorf("history_appended %d->%d, history_deduped %d->%d; want appended unchanged and deduped +%d",
				first.HistoryAppended, second.HistoryAppended, first.HistoryDeduped, second.HistoryDeduped, secondCalls-firstCalls)
			ok = false
		}
		if second.HistoryImported != first.HistoryImported || second.HistoryConversations != first.HistoryConversations {
			t.Errorf("history_imported %d->%d, history_conversations %d->%d; want unchanged",
				first.HistoryImported, second.HistoryImported, first.HistoryConversations, second.HistoryConversations)
			ok = false
		}
		if second.Appended != 0 || second.Deduped != 0 {
			t.Errorf("live appended/deduped = %d/%d, want 0/0", second.Appended, second.Deduped)
			ok = false
		}
		return ok
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 6, Rand: rand.New(rand.NewSource(2026_10_10))}); err != nil {
		t.Fatal(err)
	}
}

// ---------------------------------------------------------------------------
// 4) Window backfill
// ---------------------------------------------------------------------------

// hcdMsg is makeMsg (backfill_test.go) with a sender and a status.
func hcdMsg(id, convID, body string, ms int64, sender string, status gmproto.MessageStatusType) *gmproto.Message {
	msg := makeMsg(id, convID, body, ms)
	if sender != "" {
		msg.SenderParticipant = &gmproto.Participant{ID: &gmproto.SmallInfo{Number: sender}, FullName: "Sender " + sender}
	}
	if status != gmproto.MessageStatusType_STATUS_UNKNOWN {
		msg.MessageStatus = &gmproto.MessageStatus{Status: status}
	}
	return msg
}

func TestHistoryWindowBackfillFillsHoleBelowNewerMessages(t *testing.T) {
	const sinceMS int64 = 1_760_000_100_000
	since := time.UnixMilli(sinceMS)
	incoming := gmproto.MessageStatusType_INCOMING_COMPLETE
	const (
		alice = "+15550500001"
		bob   = "+15550500002"
	)
	at := func(deltaSeconds int64) int64 { return sinceMS + deltaSeconds*1000 }

	old := makeConv("old", "Old thread")
	old.LastMessageTimestamp = at(-50) * 1000
	old2 := makeConv("old2", "Old thread 2")
	old2.LastMessageTimestamp = at(-90) * 1000
	win := makeConv("win", "Window thread")
	win.LastMessageTimestamp = at(30) * 1000
	zero := makeConv("zero", "Unknown recency")
	zero.LastMessageTimestamp = 0
	arch := makeConv("arch", "Archived")
	arch.LastMessageTimestamp = at(1) * 1000
	hole := &gmproto.Conversation{
		ConversationID:       "hole",
		Name:                 "Hole group",
		IsGroupChat:          true,
		LastMessageTimestamp: at(40) * 1000,
		Participants: []*gmproto.Participant{
			{ID: &gmproto.SmallInfo{Number: hcdSelfNumber}, IsMe: true, FullName: "Me"},
			{ID: &gmproto.SmallInfo{Number: alice}, FullName: "Alice"},
			{ID: &gmproto.SmallInfo{Number: bob}, FullName: "Bob"},
		},
	}

	// The recovery shape: the live channel delivered hNew1/hNew2 to both
	// stores after it resumed; h1..h3 fell into the hole.
	hNew1 := hcdMsg("h-new-1", "hole", "back online", at(40), alice, incoming)
	hNew2Live := hcdMsg("h-new-2", "hole", "did you get my texts?", at(35), bob, incoming)
	// The fetched copy of h-new-2 differs from the delivered frame (status
	// absent), so it is a new history frame rather than an inbox dedupe hit.
	hNew2Fetched := hcdMsg("h-new-2", "hole", "did you get my texts?", at(35), bob, gmproto.MessageStatusType_STATUS_UNKNOWN)
	h1 := hcdMsg("h-1", "hole", "hole one", at(20), alice, incoming)
	h2 := hcdMsg("h-2", "hole", "hole two", at(10), bob, incoming)
	h3 := hcdMsg("h-3", "hole", "hole three", at(1), alice, incoming)
	hOld := hcdMsg("h-old", "hole", "just before the window", at(-1), bob, incoming)
	hOlder := hcdMsg("h-older", "hole", "page past the boundary", at(-5), bob, incoming)

	mock := &mockGMClient{
		conversations: map[gmproto.ListConversationsRequest_Folder][][]*gmproto.Conversation{
			gmproto.ListConversationsRequest_INBOX: {
				{old, win, hole},
				{zero, old2},
			},
			gmproto.ListConversationsRequest_ARCHIVE: {{arch}},
		},
		messages: map[string][][]*gmproto.Message{
			"old":  {{makeMsg("o-1", "old", "too old", at(-50))}},
			"old2": {{makeMsg("o2-1", "old2", "older", at(-90))}},
			"win": {
				{makeMsg("w-1", "win", "w one", at(30)), makeMsg("w-2", "win", "w two", at(20))},
				{makeMsg("w-3", "win", "w three", at(10)), makeMsg("w-4", "win", "w four", at(-5))},
				{makeMsg("w-5", "win", "w five", at(-10)), makeMsg("w-6", "win", "w six", at(-20))},
			},
			"zero": {
				{makeMsg("z-1", "zero", "z one", at(1))},
				{makeMsg("z-2", "zero", "z two", at(-1))},
				{makeMsg("z-3", "zero", "z three", at(-2))},
			},
			"arch": {{makeMsg("a-1", "arch", "a one", at(0)+500)}},
			"hole": {
				{hNew1, hNew2Fetched, h1},
				{h2, h3, hOld},
				{hOlder},
			},
		},
		fetchCalls: map[string]int{},
	}

	v2 := hcdNewV2(t)
	ingress := &hcdIngress{sink: v2.sink}
	a, gm, legacyDB := hcdNewApp(t, mock, ingress)

	// Seed both stores with what the live channel delivered.
	ctx := context.Background()
	convRecord, err := ingest.GoogleConversationRecord(hcdAccountID, hcdGeneration, hole, time.Now())
	if err != nil {
		t.Fatalf("GoogleConversationRecord(): %v", err)
	}
	if err := v2.sink.AppendIngress(ctx, convRecord); err != nil {
		t.Fatalf("AppendIngress(conversation): %v", err)
	}
	for _, msg := range []*gmproto.Message{hNew1, hNew2Live} {
		record, err := ingest.GoogleMessageRecord(hcdAccountID, hcdGeneration, &libgm.WrappedMessage{Message: msg}, time.Now())
		if err != nil {
			t.Fatalf("GoogleMessageRecord(%s): %v", msg.GetMessageID(), err)
		}
		if err := v2.sink.AppendIngress(ctx, record); err != nil {
			t.Fatalf("AppendIngress(%s): %v", msg.GetMessageID(), err)
		}
	}
	v2.drain(t)
	if err := a.storeConversation(hole); err != nil {
		t.Fatalf("seed legacy conversation: %v", err)
	}
	a.storeMessage(hNew1)
	a.storeMessage(hNew2Live)
	seededQuery := `SELECT * FROM messages WHERE remote_message_id IN ('h-new-1', 'h-new-2') ORDER BY message_id`
	seededBefore := hcdDumpTable(t, v2.inspect, seededQuery)
	if len(seededBefore) != 2 {
		t.Fatalf("seeded v2 rows = %d, want 2", len(seededBefore))
	}
	before := v2.snapshot()

	if !a.StartGoogleWindowBackfill(since) {
		t.Fatal("StartGoogleWindowBackfill() refused on an idle app")
	}
	deadline := time.Now().Add(10 * time.Second)
	for a.IsDeepBackfillRunning() {
		if time.Now().After(deadline) {
			t.Fatal("window backfill did not finish")
		}
		time.Sleep(2 * time.Millisecond)
	}
	if a.StartGoogleWindowBackfill(since) {
		// Wait for the second run too, so it cannot race the assertions.
		for a.IsDeepBackfillRunning() {
			time.Sleep(2 * time.Millisecond)
		}
	} else {
		t.Fatal("StartGoogleWindowBackfill() refused after the first run ended")
	}
	// The second run above re-fetches the same window: everything it hands
	// over is a dedupe hit or already in v2 (I2), so the counts below are
	// first-run counts plus pure dedupes.
	v2.drain(t)

	// Folder listing: every folder and every listing page was scanned.
	for _, folder := range backfillFolders {
		found := false
		for _, call := range gm.listCalls {
			if call.Folder == folder {
				found = true
			}
		}
		if !found {
			t.Errorf("window backfill never listed folder %s", folder)
		}
	}

	// Out-of-window conversations are neither fetched nor stored.
	for _, id := range []string{"old", "old2"} {
		if n := mock.fetchCalls[id]; n != 0 {
			t.Errorf("out-of-window conversation %s fetched %d pages, want 0", id, n)
		}
		if n := v2.count(t, `SELECT COUNT(*) FROM conversations WHERE remote_conversation_id = ?`, id); n != 0 {
			t.Errorf("out-of-window conversation %s is in v2", id)
		}
		var n int
		if err := legacyDB.QueryRow(`SELECT COUNT(*) FROM conversations WHERE conversation_id = ?`, id).Scan(&n); err != nil || n != 0 {
			t.Errorf("out-of-window conversation %s in legacy: count=%d err=%v", id, n, err)
		}
	}
	if ingress.offeredConversationIDs()["old"] || ingress.offeredConversationIDs()["old2"] {
		t.Error("out-of-window conversations were offered to v2")
	}

	// Paging stops after the first page that crosses since (two runs, so
	// every count is doubled). arch's single page stays inside the window and
	// carries no cursor, so the backfill asks once more below its oldest
	// message before concluding the conversation is exhausted.
	for id, wantPages := range map[string]int{"win": 2, "zero": 2, "arch": 2, "hole": 2} {
		if got := mock.fetchCalls[id]; got != 2*wantPages {
			t.Errorf("conversation %s fetched %d pages over two runs, want %d", id, got, 2*wantPages)
		}
	}
	if got, want := gm.fetchCursors("win"), []string{"", "msgpage_1", "", "msgpage_1"}; !reflect.DeepEqual(got, want) {
		t.Errorf("win fetch cursors = %q, want %q (page 3 never requested)", got, want)
	}

	legacy := hcdLegacyMessages(t, legacyDB)
	stored := hcdV2Messages(t, v2.inspect)
	inWindow := []string{"w-1", "w-2", "w-3", "w-4", "z-1", "z-2", "a-1", "h-new-1", "h-new-2", "h-1", "h-2", "h-3", "h-old"}
	for _, id := range inWindow {
		if _, ok := legacy[id]; !ok {
			t.Errorf("legacy lacks %s", id)
		}
		v, ok := stored[id]
		if !ok {
			t.Errorf("v2 lacks %s", id)
			continue
		}
		if l := legacy[id]; v.Body != l.Body || v.OccurredAtMS != l.TimestampMS {
			t.Errorf("%s v2 body/time %q/%d, legacy %q/%d", id, v.Body, v.OccurredAtMS, l.Body, l.TimestampMS)
		}
	}
	for _, id := range []string{"w-5", "w-6", "z-3", "h-older", "o-1", "o2-1"} {
		if _, ok := legacy[id]; ok {
			t.Errorf("legacy holds %s, which is past the window boundary page", id)
		}
		if _, ok := stored[id]; ok {
			t.Errorf("v2 holds %s, which is past the window boundary page", id)
		}
	}
	if len(stored) != len(inWindow) {
		t.Errorf("v2 holds %d messages, want %d", len(stored), len(inWindow))
	}
	// The hole is filed into the group, beside the live messages.
	for _, id := range []string{"h-new-1", "h-new-2", "h-1", "h-2", "h-3", "h-old"} {
		if v := stored[id]; v != nil && (v.RemoteConversationID != "hole" || v.ConversationKind != "group") {
			t.Errorf("%s filed in %q (%s), want group hole", id, v.RemoteConversationID, v.ConversationKind)
		}
	}
	// I3 (insert-only): the rows the live channel wrote are untouched.
	if after := hcdDumpTable(t, v2.inspect, seededQuery); !reflect.DeepEqual(after, seededBefore) {
		t.Errorf("window backfill modified live-delivered v2 rows\nbefore: %v\nafter:  %v", seededBefore, after)
	}

	// Counters. First run: 4 conversation frames (win, zero, arch, hole) and 13
	// message frames, each its own history row: history keys never collide
	// with live keys, so even hole's snapshot and h-new-1, byte-identical to
	// the live frames, are appended. h-new-1 and h-new-2's changed copy are
	// skipped as existing. The second run's 17 frames all dedupe onto their
	// history rows and are replayed; its 13 messages are all found existing.
	// Replays are handled from the worker's queue after the inbox is empty,
	// so wait for them (17 is well under the queue's capacity; none is dropped).
	c := v2.await(t, "the second run's replays", func(s ingest.CounterSnapshot) bool {
		return s.HistoryExisting-before.HistoryExisting >= 2+13
	})
	if got := c.HistoryAppended - before.HistoryAppended; got != 17 {
		t.Errorf("history_appended +%d, want +17", got)
	}
	if got := c.HistoryDeduped - before.HistoryDeduped; got != 17 {
		t.Errorf("history_deduped +%d, want +17", got)
	}
	if got := c.HistoryImported - before.HistoryImported; got != 11 {
		t.Errorf("history_imported +%d, want +11 (w-1..4, z-1..2, a-1, h-1..3, h-old)", got)
	}
	if got := c.HistoryExisting - before.HistoryExisting; got != 2+13 {
		t.Errorf("history_existing +%d, want +15 (h-new-1 and h-new-2, then the second run's 13)", got)
	}
	if got := c.HistorySkipped - before.HistorySkipped; got != 0 {
		t.Errorf("history_skipped +%d, want 0", got)
	}
	if got := c.HistoryConversations - before.HistoryConversations; got != 3 {
		t.Errorf("history_conversations +%d, want +3 (win, zero, arch; hole was bound)", got)
	}
	if c.Appended != before.Appended || c.Projected != before.Projected {
		t.Errorf("live appended/projected moved: %d->%d / %d->%d", before.Appended, c.Appended, before.Projected, c.Projected)
	}

	// Progress reports the hand-offs of the last run.
	progress := a.GetBackfillProgress()
	if progress.Running {
		t.Error("progress still reports running")
	}
	if progress.HistoryTeed != 17 || progress.HistoryTeeFailed != 0 {
		t.Errorf("progress history_teed=%d history_tee_failed=%d, want 17/0", progress.HistoryTeed, progress.HistoryTeeFailed)
	}
	if calls, _ := ingress.snapshot(); calls != 2*17 {
		t.Errorf("ingress calls = %d, want 34 over two runs", calls)
	}
	if progress.MessagesFound != 13 {
		t.Errorf("progress messages_found = %d, want 13", progress.MessagesFound)
	}
}

// ---------------------------------------------------------------------------
// 5) Tee failure handling
// ---------------------------------------------------------------------------

func hcdTwoConversationMock() *mockGMClient {
	return &mockGMClient{
		conversations: map[gmproto.ListConversationsRequest_Folder][][]*gmproto.Conversation{
			gmproto.ListConversationsRequest_INBOX: {{makeConv("t1", "One"), makeConv("t2", "Two")}},
		},
		messages: map[string][][]*gmproto.Message{
			"t1": {{makeMsg("t1-a", "t1", "a", 3000), makeMsg("t1-b", "t1", "b", 2000), makeMsg("t1-c", "t1", "c", 1000)}},
			"t2": {{makeMsg("t2-a", "t2", "a", 3500), makeMsg("t2-b", "t2", "b", 2500), makeMsg("t2-c", "t2", "c", 1500)}},
		},
	}
}

// When the generation that is fetching ends, its history ingress reports
// closed and the catch-up stops: it stores nothing more, in either store. A
// message written only to legacy past that point would be hidden from v2 for
// good, because later reconciles stop at the newest legacy message; stopping
// lets the next generation's catch-up fetch it into both.
func TestHistoryCatchUpStopsWhenItsGenerationCloses(t *testing.T) {
	t.Run("deep backfill, closed on a conversation", func(t *testing.T) {
		v2 := hcdNewV2(t)
		ingress := &hcdIngress{sink: v2.sink, script: func(call int) error {
			if call == 2 {
				return fmt.Errorf("google generation 1 ended: %w", ErrGoogleHistoryClosed)
			}
			return nil
		}}
		mock := hcdTwoConversationMock()
		mock.fetchCalls = map[string]int{}
		a, _, legacyDB := hcdNewApp(t, mock, ingress)

		a.DeepBackfill()
		v2.drain(t)

		calls, offers := ingress.snapshot()
		if calls != 2 {
			t.Fatalf("ingress calls = %d, want 2: a closed generation must stop every later hand-off (offers %+v)", calls, offers)
		}
		progress := a.GetBackfillProgress()
		if progress.HistoryTeed != 1 || progress.HistoryTeeFailed != 1 {
			t.Errorf("progress history_teed=%d history_tee_failed=%d, want 1/1", progress.HistoryTeed, progress.HistoryTeeFailed)
		}
		// Only what was handed to v2 before the close is in legacy: the first
		// conversation. The refused one and every message are in neither.
		if offers[0].Kind != "conv" {
			t.Fatalf("first offer = %+v, want a conversation", offers[0])
		}
		if got := hcdKeys(hcdLegacyConversationIDs(t, legacyDB)); !reflect.DeepEqual(got, []string{offers[0].ConversationID}) {
			t.Errorf("legacy conversations = %v, want only %s", got, offers[0].ConversationID)
		}
		if got := hcdKeys(hcdV2ConversationRemoteIDs(t, v2.inspect)); !reflect.DeepEqual(got, []string{offers[0].ConversationID}) {
			t.Errorf("v2 conversations = %v, want only %s", got, offers[0].ConversationID)
		}
		if got := len(hcdLegacyMessages(t, legacyDB)); got != 0 {
			t.Errorf("legacy holds %d messages after the close, want 0", got)
		}
		if n := v2.count(t, `SELECT COUNT(*) FROM messages`); n != 0 {
			t.Errorf("v2 holds %d messages, want 0", n)
		}
		if len(mock.fetchCalls) != 0 {
			t.Errorf("messages were fetched after the close: %v", mock.fetchCalls)
		}
	})

	t.Run("deep backfill, closed on a message", func(t *testing.T) {
		v2 := hcdNewV2(t)
		// Calls 1-2 are the two conversations; call 3 is the first message,
		// call 4 the second.
		ingress := &hcdIngress{sink: v2.sink, script: func(call int) error {
			if call == 4 {
				return fmt.Errorf("google generation 1 retired: %w", ErrGoogleHistoryClosed)
			}
			return nil
		}}
		a, _, legacyDB := hcdNewApp(t, hcdTwoConversationMock(), ingress)

		a.DeepBackfill()
		v2.drain(t)

		calls, offers := ingress.snapshot()
		if calls != 4 {
			t.Fatalf("ingress calls = %d, want 4 (offers %+v)", calls, offers)
		}
		legacy := hcdLegacyMessages(t, legacyDB)
		stored := hcdV2Messages(t, v2.inspect)
		if len(legacy) != 1 || len(stored) != 1 {
			t.Fatalf("legacy holds %d messages and v2 %d, want exactly the one handed over before the close", len(legacy), len(stored))
		}
		for id := range legacy {
			if _, ok := stored[id]; !ok {
				t.Errorf("legacy holds %s, which v2 was never given", id)
			}
		}
		if progress := a.GetBackfillProgress(); progress.HistoryTeed != 3 || progress.HistoryTeeFailed != 1 {
			t.Errorf("progress history_teed=%d history_tee_failed=%d, want 3/1", progress.HistoryTeed, progress.HistoryTeeFailed)
		}
	})

	t.Run("shallow backfill", func(t *testing.T) {
		shallow := &hcdIngress{script: func(int) error { return fmt.Errorf("retired: %w", ErrGoogleHistoryClosed) }}
		mock := hcdTwoConversationMock()
		mock.fetchCalls = map[string]int{}
		b, _, shallowDB := hcdNewApp(t, mock, shallow)
		if err := b.Backfill(); err != nil {
			t.Fatalf("Backfill(): %v", err)
		}
		if calls, _ := shallow.snapshot(); calls != 1 {
			t.Errorf("shallow backfill ingress calls = %d, want 1", calls)
		}
		if got := len(hcdLegacyMessages(t, shallowDB)); got != 0 {
			t.Errorf("shallow backfill legacy holds %d messages after the close, want 0", got)
		}
		if got := hcdKeys(hcdLegacyConversationIDs(t, shallowDB)); len(got) != 0 {
			t.Errorf("shallow backfill legacy conversations = %v, want none", got)
		}
		if len(mock.fetchCalls) != 0 {
			t.Errorf("messages were fetched after the close: %v", mock.fetchCalls)
		}
	})

	t.Run("phone backfill", func(t *testing.T) {
		// A phone backfill ignores a client change but still stops when the
		// generation that is fetching has closed its history ingress.
		ingress := &hcdIngress{script: func(call int) error {
			if call == 3 {
				return fmt.Errorf("retired: %w", ErrGoogleHistoryClosed)
			}
			return nil
		}}
		mock := hcdTwoConversationMock()
		mock.getOrCreateResults = map[string]*gmproto.Conversation{"+15550001111": makeConv("t1", "One")}
		a, _, legacyDB := hcdNewApp(t, mock, ingress)
		if err := a.BackfillConversationByPhone("+15550001111"); err != nil {
			t.Fatalf("BackfillConversationByPhone(): %v", err)
		}
		if calls, _ := ingress.snapshot(); calls != 3 {
			t.Errorf("ingress calls = %d, want 3 (conversation, one message, the refused one)", calls)
		}
		if got := len(hcdLegacyMessages(t, legacyDB)); got != 1 {
			t.Errorf("legacy holds %d messages, want only the one handed over before the close", got)
		}
	})
}

// A legacy-only install has no v2 ingest: the ingress reports disabled. The
// catch-up keeps filling the legacy store, stops offering history, and counts
// neither a hand-off nor a failure (it used to count every item as handed to
// v2 and log that it had).
func TestHistoryCatchUpWithIngestDisabledOnlyFillsLegacy(t *testing.T) {
	ingress := &hcdIngress{script: func(int) error {
		return fmt.Errorf("%w: no ingest sink", ErrGoogleHistoryDisabled)
	}}
	a, _, legacyDB := hcdNewApp(t, hcdTwoConversationMock(), ingress)

	a.DeepBackfill()

	if calls, _ := ingress.snapshot(); calls != 1 {
		t.Errorf("ingress calls = %d, want 1: after disabled, nothing more is offered", calls)
	}
	if got := len(hcdLegacyMessages(t, legacyDB)); got != 6 {
		t.Errorf("legacy holds %d messages, want all 6", got)
	}
	if got := hcdKeys(hcdLegacyConversationIDs(t, legacyDB)); !reflect.DeepEqual(got, []string{"t1", "t2"}) {
		t.Errorf("legacy conversations = %v, want [t1 t2]", got)
	}
	progress := a.GetBackfillProgress()
	if progress.HistoryTeed != 0 || progress.HistoryTeeFailed != 0 || progress.Errors != 0 {
		t.Errorf("progress = %+v, want no history counts and no errors", progress)
	}
	if progress.MessagesFound != 6 || progress.ConversationsFound != 2 {
		t.Errorf("progress messages=%d conversations=%d, want 6/2", progress.MessagesFound, progress.ConversationsFound)
	}
}

func TestHistoryTeeKeepsTryingAfterOtherErrors(t *testing.T) {
	v2 := hcdNewV2(t)
	ingress := &hcdIngress{sink: v2.sink, script: func(call int) error {
		if call == 2 || call == 4 {
			return errors.New("v2 inbox write failed")
		}
		return nil
	}}
	a, _, legacyDB := hcdNewApp(t, hcdTwoConversationMock(), ingress)

	a.DeepBackfill()
	v2.drain(t)

	calls, offers := ingress.snapshot()
	if calls != 8 {
		t.Fatalf("ingress calls = %d, want 8 (2 conversations + 6 messages): a non-closed error must not stop the tee", calls)
	}
	progress := a.GetBackfillProgress()
	if progress.HistoryTeed != 6 || progress.HistoryTeeFailed != 2 {
		t.Errorf("progress history_teed=%d history_tee_failed=%d, want 6/2", progress.HistoryTeed, progress.HistoryTeeFailed)
	}
	if got := len(hcdLegacyMessages(t, legacyDB)); got != 6 {
		t.Errorf("legacy holds %d messages, want all 6", got)
	}
	// Deep backfill lists every conversation before fetching messages, so
	// call 2 is t2's conversation frame and call 4 is a message.
	if offers[1].Kind != "conv" || offers[3].Kind != "msg" {
		t.Fatalf("offers = %+v, want calls 2 and 4 to be a conversation and a message", offers)
	}
	stored := hcdV2Messages(t, v2.inspect)
	if len(stored) != 5 {
		t.Errorf("v2 holds %d messages, want 5", len(stored))
	}
	if _, ok := stored[offers[3].MessageID]; ok {
		t.Errorf("v2 holds %s, whose hand-off failed", offers[3].MessageID)
	}
	// The refused conversation frame is recovered: its messages carry the
	// conversation snapshot (DESIGN decision 3).
	if got := hcdV2ConversationRemoteIDs(t, v2.inspect); got["t1"] != "direct" || got["t2"] != "direct" || len(got) != 2 {
		t.Errorf("v2 conversations = %v, want t1 and t2", got)
	}
	if c := v2.snapshot(); c.HistoryAppended != 6 || c.HistoryConversations != 2 {
		t.Errorf("history_appended=%d history_conversations=%d, want 6/2", c.HistoryAppended, c.HistoryConversations)
	}
}

// ---------------------------------------------------------------------------
// 6) Generation capture
// ---------------------------------------------------------------------------

// hcdNewLegacyClient builds a real, unconnected client the same way
// bridgeadapters/google/google_test.go newLegacyClient does; NewFromSession
// does not dial.
func hcdNewLegacyClient(t *testing.T) *client.Client {
	t.Helper()
	cli, err := client.NewFromSession(&client.SessionData{AuthDataJSON: []byte(`{}`)}, zerolog.Nop())
	if err != nil {
		t.Fatalf("NewFromSession(): %v", err)
	}
	return cli
}

func TestHistoryCatchUpCapturesTheGenerationIngressWithItsClient(t *testing.T) {
	t.Setenv("OPENMESSAGE_GOOGLE_AVATAR_SYNC", "0")
	a := newTestApp(t, nil)
	// newTestApp stores a typed-nil *mockGMClient; clear the seam so
	// beginGoogleCatchUp takes the real generation path.
	a.gmClient = nil
	if c := a.beginGoogleCatchUp("no_client"); c != nil {
		t.Fatal("beginGoogleCatchUp() began a catch-up with no client")
	}

	cli1 := hcdNewLegacyClient(t)
	ingress1 := &hcdIngress{}
	a.BeginGoogleGenerationWithHistory(cli1, ingress1)
	c1 := a.beginGoogleCatchUp("gen1")
	if c1 == nil {
		t.Fatal("beginGoogleCatchUp() = nil with generation 1 installed")
	}
	if c1.history != GoogleHistoryIngress(ingress1) {
		t.Errorf("generation 1 catch-up history = %#v, want generation 1's ingress", c1.history)
	}
	if c1.token != any(cli1.GM) {
		t.Error("generation 1 catch-up token is not generation 1's libgm client")
	}
	if real, ok := c1.gm.(*realGMClient); !ok || real.gm != cli1.GM {
		t.Errorf("generation 1 catch-up fetches with %#v, want generation 1's client", c1.gm)
	}
	if !c1.stillCurrent() {
		t.Error("generation 1 catch-up is not current while generation 1 is installed")
	}

	// A later generation without history replaces generation 1.
	cli2 := hcdNewLegacyClient(t)
	a.BeginGoogleGeneration(cli2)
	c2 := a.beginGoogleCatchUp("gen2")
	if c2 == nil {
		t.Fatal("beginGoogleCatchUp() = nil with generation 2 installed")
	}
	if c2.history != nil {
		t.Errorf("generation 2 (no history) catch-up history = %#v, want nil", c2.history)
	}
	if c2.token != any(cli2.GM) {
		t.Error("generation 2 catch-up token is not generation 2's libgm client")
	}
	// Generation 1's catch-up keeps generation 1's ingress (captured
	// together with its client) and now sees that its client is gone.
	if c1.history != GoogleHistoryIngress(ingress1) {
		t.Error("generation 1 catch-up lost its captured ingress")
	}
	if c1.stillCurrent() {
		t.Error("generation 1 catch-up still current after generation 2 replaced it")
	}
	// What generation 1's catch-up stores goes to generation 1's ingress, never
	// to generation 2.
	if err := c1.storeConversation(makeConv("late", "Late")); err != nil {
		t.Fatalf("storeConversation(): %v", err)
	}
	c1.storeMessage("late", nil, makeMsg("late-1", "late", "late body", 5000))
	if calls, offers := ingress1.snapshot(); calls != 2 || offers[0].ConversationID != "late" || offers[1].MessageID != "late-1" {
		t.Errorf("generation 1 ingress got %d calls %+v, want the late conversation and message", calls, offers)
	}

	// A client installed outside a generation never inherits a generation's
	// history.
	cli3 := hcdNewLegacyClient(t)
	ingress3 := &hcdIngress{}
	gen3 := a.BeginGoogleGenerationWithHistory(cli3, ingress3)
	cli4 := hcdNewLegacyClient(t)
	a.setClient(cli4)
	c4 := a.beginGoogleCatchUp("swapped")
	if c4 == nil || c4.history != nil || c4.token != any(cli4.GM) {
		t.Errorf("catch-up on a client swapped in outside generation 3 = %+v, want cli4 with nil history", c4)
	}

	// After the installed generation releases its client, no catch-up begins.
	cli5 := hcdNewLegacyClient(t)
	ingress5 := &hcdIngress{}
	gen5 := a.BeginGoogleGenerationWithHistory(cli5, ingress5)
	if c5 := a.beginGoogleCatchUp("gen5"); c5 == nil || c5.history != GoogleHistoryIngress(ingress5) {
		t.Fatalf("generation 5 catch-up = %+v, want generation 5's ingress", c5)
	}
	gen3.Release() // not installed: a no-op
	if c := a.beginGoogleCatchUp("after_stale_release"); c == nil || c.history != GoogleHistoryIngress(ingress5) {
		t.Error("a stale generation's Release disturbed the installed generation's capture")
	}
	gen5.Release()
	if c := a.beginGoogleCatchUp("released"); c != nil {
		t.Errorf("beginGoogleCatchUp() after Release = %+v, want nil", c)
	}
	if calls, _ := ingress3.snapshot(); calls != 0 {
		t.Errorf("generation 3 ingress got %d calls, want 0", calls)
	}
}

// ---------------------------------------------------------------------------
// 7) Shallow Backfill() now fetches through GMClient and tees
// ---------------------------------------------------------------------------

func TestHistoryShallowBackfillTeesFirstPageThroughGMClient(t *testing.T) {
	var convs []*gmproto.Conversation
	messages := map[string][][]*gmproto.Message{}
	ms := int64(1_760_000_000_000)
	for _, id := range []string{"s1", "s2", "s3", "s4-page2", "s5-archive"} {
		convs = append(convs, makeConv(id, "Shallow "+id))
		var first, second []*gmproto.Message
		for i := 0; i < 25; i++ { // newest first
			first = append(first, makeMsg(fmt.Sprintf("%s-%02d", id, i), id, fmt.Sprintf("%s body %d", id, i), ms-int64(i)*1000))
		}
		for i := 25; i < 35; i++ {
			second = append(second, makeMsg(fmt.Sprintf("%s-%02d", id, i), id, fmt.Sprintf("%s body %d", id, i), ms-int64(i)*1000))
		}
		messages[id] = [][]*gmproto.Message{first, second}
		ms += 100_000
	}
	mock := &mockGMClient{
		conversations: map[gmproto.ListConversationsRequest_Folder][][]*gmproto.Conversation{
			gmproto.ListConversationsRequest_INBOX:   {convs[:3], {convs[3]}},
			gmproto.ListConversationsRequest_ARCHIVE: {{convs[4]}},
		},
		messages:   messages,
		fetchCalls: map[string]int{},
	}
	v2 := hcdNewV2(t)
	ingress := &hcdIngress{sink: v2.sink}
	a, gm, legacyDB := hcdNewApp(t, mock, ingress)
	gm.honorCount = true // the phone returns at most count messages

	if err := a.Backfill(); err != nil {
		t.Fatalf("Backfill(): %v", err)
	}
	v2.drain(t)

	if want := []hcdListCall{{Count: 100, Folder: gmproto.ListConversationsRequest_INBOX}}; !reflect.DeepEqual(gm.listCalls, want) {
		t.Errorf("list calls = %+v, want one INBOX first-page call for 100", gm.listCalls)
	}
	wantFetches := []hcdFetchCall{{"s1", 20, ""}, {"s2", 20, ""}, {"s3", 20, ""}}
	if !reflect.DeepEqual(gm.fetches, wantFetches) {
		t.Errorf("fetch calls = %+v, want %+v", gm.fetches, wantFetches)
	}

	_, offers := ingress.snapshot()
	perConversation := map[string]int{}
	var convOffers []string
	for _, offer := range offers {
		switch offer.Kind {
		case "conv":
			convOffers = append(convOffers, offer.ConversationID)
		case "msg":
			perConversation[offer.ConversationID]++
			if offer.SnapshotID != offer.ConversationID {
				t.Errorf("message %s offered with snapshot %q, want its listed conversation %q",
					offer.MessageID, offer.SnapshotID, offer.ConversationID)
			}
		}
	}
	if !reflect.DeepEqual(convOffers, []string{"s1", "s2", "s3"}) {
		t.Errorf("conversation offers = %v, want [s1 s2 s3]", convOffers)
	}
	if want := map[string]int{"s1": 20, "s2": 20, "s3": 20}; !reflect.DeepEqual(perConversation, want) {
		t.Errorf("message offers per conversation = %v, want %v", perConversation, want)
	}

	stored := hcdV2Messages(t, v2.inspect)
	legacy := hcdLegacyMessages(t, legacyDB)
	if len(stored) != 60 || len(legacy) != 60 {
		t.Errorf("v2 holds %d messages and legacy %d, want 60 each", len(stored), len(legacy))
	}
	for _, id := range []string{"s1", "s2", "s3"} {
		for i := 0; i < 20; i++ {
			remoteID := fmt.Sprintf("%s-%02d", id, i)
			if v := stored[remoteID]; v == nil || v.RemoteConversationID != id || v.Body != legacy[remoteID].Body {
				t.Errorf("v2 lacks or misfiles %s: %+v", remoteID, v)
			}
		}
		if _, ok := stored[fmt.Sprintf("%s-20", id)]; ok {
			t.Errorf("v2 holds %s-20, beyond the 20 the shallow backfill asked for", id)
		}
	}
	c := v2.snapshot()
	if c.HistoryAppended != 63 || c.HistoryImported != 60 || c.HistoryConversations != 3 {
		t.Errorf("history_appended=%d history_imported=%d history_conversations=%d, want 63/60/3",
			c.HistoryAppended, c.HistoryImported, c.HistoryConversations)
	}
	if c.Appended != 0 || c.Projected != 0 {
		t.Errorf("live appended/projected = %d/%d, want 0/0", c.Appended, c.Projected)
	}
	// The shallow backfill keeps its own progress out of the deep-backfill
	// snapshot.
	if progress := a.GetBackfillProgress(); progress.HistoryTeed != 0 {
		t.Errorf("shallow backfill reported history_teed=%d in deep progress, want 0", progress.HistoryTeed)
	}
}
