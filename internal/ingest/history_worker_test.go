package ingest

// Worker tests for Google catch-up history (codec google.protobuf.history,
// DecoderRegistration.History). Design and invariants:
// ~/reviews/openmessage-history-v2-2026-10-08/DESIGN.md, decision 4 (history is
// insert-only) and invariants I2 (idempotence), I3 (insert-only) and I7
// (ordering).
//
// Every test runs a real SQLite store, a real Worker with both production
// Google registrations (cmd/v2stack.go: GoogleCodec live, GoogleHistoryCodec
// History:true) and a Sink with deterministic counter inbox IDs (i01NewSink).
// Frames are built with the production record builders (GoogleMessageRecord,
// GoogleConversationRecord, GoogleHistoryMessageRecord,
// GoogleHistoryConversationRecord) and appended through Sink.AppendIngress /
// Sink.AppendHistoryIngress.
//
// Most tests drive the worker synchronously with pump, which runs Worker.Run's
// loop body on the test goroutine (replays first, then a drain) the way
// idsRun drives worker.drain directly; frame order is then fully determined.
// The gap-fill and echo tests run the real Run loop instead.
//
// "Contrast" blocks show what the same frame does under live semantics. They
// are not defects: they show the history assertions are not vacuous.

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math/rand"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/quick"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/mautrix-gmessages/pkg/libgm"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/messaging"
	"github.com/maxghenis/openmessage/internal/storage/blob"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
	"github.com/maxghenis/openmessage/internal/v2keys"
)

const (
	hwtSelf     = "+16505550100"
	hwtKarl     = "+12025550101"
	hwtShoshana = "+15165550102"
	hwtAda      = "+14155550103"
	hwtBea      = "+13125550104"

	hwtGeneration = bridge.Generation(41)
)

var (
	hwtStart = time.Date(2026, time.October, 7, 12, 0, 0, 0, time.UTC)
	hwtNames = map[string]string{
		hwtSelf:     "Max",
		hwtKarl:     "Karl",
		hwtShoshana: "Shoshana",
		hwtAda:      "Ada",
		hwtBea:      "Bea",
	}
)

// ---------------------------------------------------------------------------
// Harness

type hwtHarness struct {
	*i01Harness
	reactions *sqlite.ReactionRepository
	clock     *googleEchoClock
	echoes    *i01EchoRecorder
	running   bool
	stop      func()
}

// hwtRegistrations is the production Google decoder pair (cmd/v2stack.go):
// two decoder instances sharing the worker's counters.
func hwtRegistrations(counters *Counters) []DecoderRegistration {
	return []DecoderRegistration{
		{
			Codec:    GoogleCodec,
			Platform: bridge.PlatformGoogle,
			Decoder:  NewGoogleDecoder(counters),
		},
		{
			Codec:    GoogleHistoryCodec,
			Platform: bridge.PlatformGoogle,
			Decoder:  NewGoogleDecoder(counters),
			History:  true,
		},
	}
}

// newHWTHarness builds a harness whose lifetime is the test. run starts the
// real Worker.Run loop; otherwise the test drives the worker with pump.
func newHWTHarness(t *testing.T, run bool) *hwtHarness {
	t.Helper()
	harness := openHWTHarness(t, t.TempDir(), run)
	t.Cleanup(harness.stop)
	return harness
}

// openHWTHarness builds a harness in dir; the caller owns stop (the property
// test closes one store per generated case).
func openHWTHarness(t *testing.T, dir string, run bool) *hwtHarness {
	t.Helper()
	path := filepath.Join(dir, "store.sqlite3")
	store, err := sqlite.Open(path)
	if err != nil {
		t.Fatalf("sqlite.Open(): %v", err)
	}
	i01SeedAccount(t, store)
	clock := &googleEchoClock{now: hwtStart}
	messages, err := sqlite.NewMessageRepository(store, clock.Now)
	if err != nil {
		t.Fatalf("sqlite.NewMessageRepository(): %v", err)
	}
	reactions, err := sqlite.NewReactionRepository(store, clock.Now)
	if err != nil {
		t.Fatalf("sqlite.NewReactionRepository(): %v", err)
	}
	counters := &Counters{}
	echoes := &i01EchoRecorder{}
	worker, err := NewWorker(WorkerConfig{
		Store:        store,
		Messages:     messages,
		Reactions:    reactions,
		EchoObserver: echoes,
		Counters:     counters,
		Logger:       zerolog.Nop(),
		Now:          clock.Now,
		Decoders:     hwtRegistrations(counters),
	})
	if err != nil {
		t.Fatalf("NewWorker(): %v", err)
	}
	sink := i01NewSink(t, messages, worker, counters, "inbox-hwt")
	harness := &hwtHarness{
		i01Harness: &i01Harness{
			path:     path,
			store:    store,
			messages: messages,
			counters: counters,
			worker:   worker,
			sink:     sink,
		},
		reactions: reactions,
		clock:     clock,
		echoes:    echoes,
		running:   run,
	}
	cancel := func() {}
	done := make(chan error, 1)
	if run {
		ctx, stopRun := context.WithCancel(context.Background())
		cancel = stopRun
		go func() { done <- worker.Run(ctx) }()
	}
	var once sync.Once
	harness.stop = func() {
		once.Do(func() {
			if run {
				cancel()
				select {
				case err := <-done:
					if err != nil {
						t.Errorf("Worker.Run(): %v", err)
					}
				case <-time.After(5 * time.Second):
					t.Error("Worker.Run() did not stop after cancellation")
				}
			}
			if err := store.Close(); err != nil {
				t.Errorf("close store: %v", err)
			}
		})
	}
	return harness
}

// pump runs Worker.Run's loop body (worker.go Run) on the test goroutine until
// the notification queue is empty: a deduplicated replay is first re-handled
// from the caller's record, then the durable inbox is drained.
func (h *hwtHarness) pump(t *testing.T) {
	t.Helper()
	if h.running {
		t.Fatal("pump called on a harness whose Run loop owns the queue")
	}
	ctx := context.Background()
	for {
		select {
		case item := <-h.worker.work:
			if item.replay {
				h.worker.handleRecord(ctx, item.inboxID, item.record)
			}
			h.worker.drain(ctx)
		default:
			i01AssertNoPending(t, h.messages)
			return
		}
	}
}

func (h *hwtHarness) counts() CounterSnapshot {
	return h.counters.Snapshot(i01AccountID)
}

// tick advances the shared clock one second, so every append and every write
// after it carries a distinct, later time: a rewrite of an existing row would
// move its updated_at, and a newer reaction snapshot would win its fence.
func (h *hwtHarness) tick() time.Time {
	now := h.clock.Now().Add(time.Second)
	h.clock.Set(now)
	return now
}

func (h *hwtHarness) liveConversation(t *testing.T, conversation *gmproto.Conversation) bridge.RawIngressRecord {
	t.Helper()
	record, err := GoogleConversationRecord(i01AccountID, hwtGeneration, conversation, h.tick())
	if err != nil {
		t.Fatalf("GoogleConversationRecord(%q): %v", conversation.GetConversationID(), err)
	}
	i01MustAppend(t, h.sink, record)
	return record
}

func (h *hwtHarness) liveMessage(t *testing.T, message *gmproto.Message) bridge.RawIngressRecord {
	t.Helper()
	record, err := GoogleMessageRecord(
		i01AccountID,
		hwtGeneration,
		&libgm.WrappedMessage{Message: message},
		h.tick(),
	)
	if err != nil {
		t.Fatalf("GoogleMessageRecord(%q): %v", message.GetMessageID(), err)
	}
	i01MustAppend(t, h.sink, record)
	return record
}

func (h *hwtHarness) historyConversation(t *testing.T, conversation *gmproto.Conversation) bridge.RawIngressRecord {
	t.Helper()
	record, err := GoogleHistoryConversationRecord(i01AccountID, hwtGeneration, conversation, h.tick())
	if err != nil {
		t.Fatalf("GoogleHistoryConversationRecord(%q): %v", conversation.GetConversationID(), err)
	}
	h.appendHistory(t, record)
	return record
}

// historyMessage appends a fetched message with the snapshot of the thread it
// was fetched from, as the adapter does.
func (h *hwtHarness) historyMessage(
	t *testing.T,
	conversation *gmproto.Conversation,
	message *gmproto.Message,
) bridge.RawIngressRecord {
	t.Helper()
	record, err := GoogleHistoryMessageRecord(
		i01AccountID,
		hwtGeneration,
		message.GetConversationID(),
		conversation,
		message,
		h.tick(),
	)
	if err != nil {
		t.Fatalf("GoogleHistoryMessageRecord(%q): %v", message.GetMessageID(), err)
	}
	h.appendHistory(t, record)
	return record
}

func (h *hwtHarness) appendHistory(t *testing.T, record bridge.RawIngressRecord) {
	t.Helper()
	if err := h.sink.AppendHistoryIngress(context.Background(), record); err != nil {
		t.Fatalf("AppendHistoryIngress(%q): %v", record.DedupeKey, err)
	}
}

func (h *hwtHarness) conversation(t *testing.T, remoteConversationID string) sqlite.Conversation {
	t.Helper()
	conversation, err := h.store.GetConversationByRemote(i01AccountID, remoteConversationID)
	if err != nil {
		t.Fatalf("GetConversationByRemote(%q): %v", remoteConversationID, err)
	}
	return conversation
}

func (h *hwtHarness) message(t *testing.T, remoteConversationID, remoteMessageID string) sqlite.Message {
	t.Helper()
	conversation := h.conversation(t, remoteConversationID)
	message, err := h.messages.GetMessageByRemote(
		context.Background(),
		i01AccountID,
		conversation.ConversationID,
		remoteMessageID,
	)
	if err != nil {
		t.Fatalf("GetMessageByRemote(%q, %q): %v", remoteConversationID, remoteMessageID, err)
	}
	return message
}

// peers lists a conversation's active non-self participants, sorted.
func (h *hwtHarness) peers(t *testing.T, conversationID string) []string {
	t.Helper()
	numbers := idsPeerNumbers(t, h.i01Harness, conversationID)
	sort.Strings(numbers)
	return numbers
}

func (h *hwtHarness) exec(t *testing.T, query string, args ...any) {
	t.Helper()
	database := i01OpenInspector(t, h.path)
	defer database.Close()
	if _, err := database.Exec(query, args...); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}

// rows renders every row of query with every column, for byte-level
// before/after comparisons.
func (h *hwtHarness) rows(t *testing.T, query string, args ...any) []string {
	t.Helper()
	_, dumps := hwtQueryRows(t, h.path, query, args...)
	return dumps
}

// rowsByKey renders query's rows keyed by the first column.
func (h *hwtHarness) rowsByKey(t *testing.T, query string, args ...any) map[string]string {
	t.Helper()
	keys, dumps := hwtQueryRows(t, h.path, query, args...)
	byKey := make(map[string]string, len(keys))
	for index, key := range keys {
		if _, duplicate := byKey[key]; duplicate {
			t.Fatalf("query %q: duplicate key %q", query, key)
		}
		byKey[key] = dumps[index]
	}
	return byKey
}

func hwtQueryRows(t *testing.T, path, query string, args ...any) (keys []string, dumps []string) {
	t.Helper()
	database := i01OpenInspector(t, path)
	defer database.Close()
	rows, err := database.Query(query, args...)
	if err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		t.Fatalf("columns %q: %v", query, err)
	}
	for rows.Next() {
		values := make([]any, len(columns))
		targets := make([]any, len(columns))
		for index := range values {
			targets[index] = &values[index]
		}
		if err := rows.Scan(targets...); err != nil {
			t.Fatalf("scan %q: %v", query, err)
		}
		fields := make([]string, len(columns))
		for index, value := range values {
			fields[index] = columns[index] + "=" + hwtFormat(value)
		}
		keys = append(keys, fmt.Sprint(values[0]))
		dumps = append(dumps, strings.Join(fields, " "))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows %q: %v", query, err)
	}
	return keys, dumps
}

func hwtFormat(value any) string {
	switch typed := value.(type) {
	case nil:
		return "NULL"
	case []byte:
		return "x'" + hex.EncodeToString(typed) + "'"
	case string:
		return strconv.Quote(typed)
	default:
		return fmt.Sprint(typed)
	}
}

// hwtStateTables is every table a projection can touch.
var hwtStateTables = []struct{ table, order string }{
	{"messages", "message_id"},
	{"conversations", "conversation_id"},
	{"conversation_participants", "conversation_id, identity_id"},
	{"identities", "identity_id"},
	{"reactions", "message_id, reactor_key"},
	{"reaction_snapshot_fences", "message_id"},
	{"message_attachments", "message_id, ordinal"},
	{"inbox", "inbox_id"},
}

func (h *hwtHarness) state(t *testing.T) map[string][]string {
	t.Helper()
	state := make(map[string][]string, len(hwtStateTables))
	for _, table := range hwtStateTables {
		state[table.table] = h.rows(t, "SELECT * FROM "+table.table+" ORDER BY "+table.order)
	}
	return state
}

func hwtStateDiff(before, after map[string][]string) string {
	var problems []string
	for _, table := range hwtStateTables {
		was, now := before[table.table], after[table.table]
		if reflect.DeepEqual(was, now) {
			continue
		}
		problems = append(problems, fmt.Sprintf(
			"table %s changed:\n  before:\n    %s\n  after:\n    %s",
			table.table,
			strings.Join(was, "\n    "),
			strings.Join(now, "\n    "),
		))
	}
	return strings.Join(problems, "\n")
}

// inboxRow returns the stored codec and processed flag of the inbox row that
// holds dedupeKey.
func (h *hwtHarness) inboxRow(t *testing.T, dedupeKey string) (codec string, processed bool) {
	t.Helper()
	rows := h.rows(t, `
		SELECT codec, processed_at_ms IS NOT NULL AS processed
		FROM inbox WHERE account_id = ? AND dedupe_key = ?
	`, i01AccountID, dedupeKey)
	if len(rows) != 1 {
		t.Fatalf("inbox rows for %q = %v, want one", dedupeKey, rows)
	}
	var codecValue, processedValue string
	for _, field := range strings.Split(rows[0], " ") {
		switch {
		case strings.HasPrefix(field, "codec="):
			codecValue, _ = strconv.Unquote(strings.TrimPrefix(field, "codec="))
		case strings.HasPrefix(field, "processed="):
			processedValue = strings.TrimPrefix(field, "processed=")
		}
	}
	return codecValue, processedValue == "1"
}

func (h *hwtHarness) echoCalls() int {
	h.echoes.mu.Lock()
	defer h.echoes.mu.Unlock()
	return len(h.echoes.calls)
}

func (h *hwtHarness) identityCanonical(t *testing.T, identityID *string) string {
	t.Helper()
	if identityID == nil {
		return ""
	}
	identity, err := h.store.GetIdentity(*identityID)
	if err != nil {
		t.Fatalf("GetIdentity(%q): %v", *identityID, err)
	}
	return identity.CanonicalValue
}

// ---------------------------------------------------------------------------
// Protobuf builders

func hwtParticipant(number string) *gmproto.Participant {
	return &gmproto.Participant{
		FullName: hwtNames[number],
		IsMe:     number == hwtSelf,
		ID:       &gmproto.SmallInfo{Number: number},
	}
}

// hwtConversation is a Google conversation snapshot with the account itself
// and peers on the roster.
func hwtConversation(remoteID, title string, group bool, peers ...string) *gmproto.Conversation {
	participants := []*gmproto.Participant{hwtParticipant(hwtSelf)}
	for _, peer := range peers {
		participants = append(participants, hwtParticipant(peer))
	}
	return &gmproto.Conversation{
		ConversationID: remoteID,
		Name:           title,
		IsGroupChat:    group,
		Participants:   participants,
	}
}

type hwtReaction struct{ emoji, actor string }

type hwtMessage struct {
	id           string
	conversation string
	body         string
	// from is the sender's number; empty means the account itself sent it.
	from      string
	name      string
	at        time.Time
	reply     string
	media     *gmproto.MediaContent
	reactions []hwtReaction
	tmpID     string
}

func (m hwtMessage) proto() *gmproto.Message {
	// googleEchoMessage (google_echo_e2e_test.go) supplies the complete-status
	// text message; the fields below retarget it.
	message := googleEchoMessage(m.id, m.tmpID, m.body, m.from == "", m.at)
	message.ConversationID = m.conversation
	if m.from == "" {
		message.SenderParticipant = hwtParticipant(hwtSelf)
	} else {
		name := m.name
		if name == "" {
			name = hwtNames[m.from]
		}
		message.SenderParticipant = &gmproto.Participant{
			FullName: name,
			ID:       &gmproto.SmallInfo{Number: m.from},
		}
	}
	if m.reply != "" {
		message.ReplyMessage = &gmproto.ReplyMessage{MessageID: m.reply}
	}
	if m.media != nil {
		message.MessageInfo = append(message.MessageInfo, &gmproto.MessageInfo{
			Data: &gmproto.MessageInfo_MediaContent{MediaContent: m.media},
		})
	}
	for _, reaction := range m.reactions {
		message.Reactions = append(message.Reactions, &gmproto.ReactionEntry{
			Data:           &gmproto.ReactionData{Unicode: reaction.emoji},
			ParticipantIDs: []string{reaction.actor},
		})
	}
	return message
}

// ---------------------------------------------------------------------------
// (a) Gap fill

// A fetched message v2 lacks, in a thread v2 already has, is inserted with its
// full content and counted only under the history counters. Runs the real
// Worker.Run loop.
func TestHistoryWorkerGapFillInsertsMissingMessage(t *testing.T) {
	h := newHWTHarness(t, true)
	group := hwtConversation("hwt-a", "Gap fill", true, hwtKarl, hwtShoshana)
	h.liveConversation(t, group)
	i01WaitFor(t, "live conversation frame", func() bool { return h.counts().DecodedEvents == 1 })
	i01AssertNoPending(t, h.messages)
	thread := h.conversation(t, "hwt-a")
	before := h.counts()

	at := hwtStart.Add(-36 * time.Hour) // inside the span the live channel skipped
	incoming := hwtMessage{
		id:           "hwt-a-1",
		conversation: "hwt-a",
		body:         "fetched during catch-up",
		from:         "+1 (202) 555-0101", // Karl, formatted: canonicalizes to hwtKarl
		name:         "Karl",
		at:           at,
		reply:        "hwt-a-0",
		media: &gmproto.MediaContent{
			MediaID:       "media-hwt-a",
			MediaName:     "a.png",
			MimeType:      "image/png",
			Size:          42,
			DecryptionKey: []byte{0xde, 0xad, 0xbe, 0xef},
		},
	}
	outgoing := hwtMessage{
		id:           "hwt-a-2",
		conversation: "hwt-a",
		body:         "my reply, sent from the phone",
		at:           at.Add(time.Minute),
	}
	incomingRecord := h.historyMessage(t, group, incoming.proto())
	outgoingRecord := h.historyMessage(t, group, outgoing.proto())
	i01WaitFor(t, "history gap fill", func() bool { return h.counts().HistoryImported == 2 })
	i01AssertNoPending(t, h.messages)
	after := h.counts()

	stored := h.message(t, "hwt-a", "hwt-a-1")
	if want := v2keys.DeriveID("message", i01AccountID, "hwt-a\x1fhwt-a-1"); stored.MessageID != want {
		t.Fatalf("message ID = %q, want derived %q (same key as a live projection)", stored.MessageID, want)
	}
	if stored.ConversationID != thread.ConversationID ||
		stored.Body != "fetched during catch-up" ||
		stored.OccurredAtMS != at.UnixMilli() ||
		stored.Direction != sqlite.MessageDirectionIncoming ||
		stored.State != sqlite.MessageStateActive ||
		stored.ReplyToRemoteID == nil || *stored.ReplyToRemoteID != "hwt-a-0" {
		t.Fatalf("gap-filled message = %+v", stored)
	}
	if got := h.identityCanonical(t, stored.SenderIdentityID); got != hwtKarl {
		t.Fatalf("sender canonical = %q, want %q", got, hwtKarl)
	}
	attachments := h.rows(t, `
		SELECT ordinal, remote_id, remote_ref, mime, filename, size_bytes, state
		FROM message_attachments WHERE message_id = ? ORDER BY ordinal
	`, stored.MessageID)
	wantRef := `{"v":1,"media_id":"media-hwt-a","decryption_key":"deadbeef"}`
	wantAttachment := fmt.Sprintf(
		`ordinal=0 remote_id="media-hwt-a" remote_ref=x'%s' mime="image/png" filename="a.png" size_bytes=42 state="pending"`,
		hex.EncodeToString([]byte(wantRef)),
	)
	if len(attachments) != 1 || attachments[0] != wantAttachment {
		t.Fatalf("attachments = %v, want [%s]", attachments, wantAttachment)
	}

	sent := h.message(t, "hwt-a", "hwt-a-2")
	if sent.Direction != sqlite.MessageDirectionOutgoing || sent.SenderIdentityID != nil ||
		sent.Body != outgoing.body || sent.OccurredAtMS != outgoing.at.UnixMilli() {
		t.Fatalf("gap-filled outgoing message = %+v", sent)
	}
	if got := h.conversation(t, "hwt-a").LastMessageAtMS; got != outgoing.at.UnixMilli() {
		t.Fatalf("conversation recency = %d, want the newest inserted message %d", got, outgoing.at.UnixMilli())
	}

	for _, record := range []bridge.RawIngressRecord{incomingRecord, outgoingRecord} {
		codec, processed := h.inboxRow(t, record.DedupeKey)
		if codec != GoogleHistoryCodec || !processed {
			t.Fatalf("inbox row %q codec=%q processed=%v, want %q processed", record.DedupeKey, codec, processed, GoogleHistoryCodec)
		}
	}
	// Live counters keep describing the live channel only (DESIGN decision 6).
	if after.HistoryAppended-before.HistoryAppended != 2 ||
		after.HistoryImported-before.HistoryImported != 2 ||
		after.Appended != before.Appended ||
		after.Projected != before.Projected ||
		after.Imported != before.Imported ||
		after.HistoryExisting != 0 ||
		after.HistoryDeduped != 0 ||
		after.Quarantined != 0 ||
		after.ReactionsOrphaned != 0 {
		t.Fatalf("counters before=%+v after=%+v", before, after)
	}
	if calls := h.echoCalls(); calls != 0 {
		t.Fatalf("echo observer calls = %d, want 0 (no TmpID)", calls)
	}
}

// ---------------------------------------------------------------------------
// (b) Insert-only (I3)

// A fetched copy of a message v2 already holds never touches the row, its
// reactions, its thread or its sender, even when the copy differs and the
// stored row was deleted after it arrived.
func TestHistoryWorkerNeverRewritesExistingRow(t *testing.T) {
	h := newHWTHarness(t, false)
	group := hwtConversation("hwt-b", "Insert only", true, hwtKarl, hwtShoshana)
	h.liveConversation(t, group)
	at := hwtStart.Add(-time.Hour)
	live := hwtMessage{
		id:           "hwt-b-1",
		conversation: "hwt-b",
		body:         "original words",
		from:         hwtKarl,
		at:           at,
		reactions:    []hwtReaction{{"👍", hwtShoshana}},
	}
	h.liveMessage(t, live.proto())
	h.pump(t)
	stored := h.message(t, "hwt-b", "hwt-b-1")
	assertSingleActiveReaction(t, h.reactions, stored.MessageID, "👍", hwtShoshana, false)

	// A delete reached v2 after the live frame. Move updated_at too, so any
	// rewrite (even one restoring the same columns) is visible.
	h.exec(t, `UPDATE messages SET state = 'deleted', updated_at_ms = updated_at_ms + 7 WHERE message_id = ?`, stored.MessageID)
	thread := h.conversation(t, "hwt-b")
	rowBefore := h.rows(t, `SELECT * FROM messages WHERE message_id = ?`, stored.MessageID)
	reactionsBefore := h.rows(t, `SELECT * FROM reactions WHERE message_id = ? ORDER BY reactor_key`, stored.MessageID)
	fenceBefore := h.rows(t, `SELECT * FROM reaction_snapshot_fences WHERE message_id = ?`, stored.MessageID)
	threadBefore := h.rows(t, `SELECT * FROM conversations WHERE conversation_id = ?`, thread.ConversationID)
	senderBefore := h.rows(t, `SELECT * FROM identities WHERE canonical_value = ?`, hwtKarl)
	before := h.counts()

	fetched := live
	fetched.body = "a different fetched body"
	fetched.name = "Karl (stale contact name)"
	fetched.at = at.Add(time.Hour) // newer than the thread's recency
	fetched.reactions = nil        // as live, an empty snapshot would tombstone 👍
	record := h.historyMessage(t, hwtConversation("hwt-b", "Renamed in the listing", true, hwtKarl), fetched.proto())
	h.pump(t)
	after := h.counts()

	for _, check := range []struct {
		name          string
		before, after []string
	}{
		{"message row", rowBefore, h.rows(t, `SELECT * FROM messages WHERE message_id = ?`, stored.MessageID)},
		{"reactions", reactionsBefore, h.rows(t, `SELECT * FROM reactions WHERE message_id = ? ORDER BY reactor_key`, stored.MessageID)},
		{"reaction fence", fenceBefore, h.rows(t, `SELECT * FROM reaction_snapshot_fences WHERE message_id = ?`, stored.MessageID)},
		{"thread", threadBefore, h.rows(t, `SELECT * FROM conversations WHERE conversation_id = ?`, thread.ConversationID)},
		{"sender identity", senderBefore, h.rows(t, `SELECT * FROM identities WHERE canonical_value = ?`, hwtKarl)},
	} {
		if !reflect.DeepEqual(check.before, check.after) {
			t.Fatalf("%s changed:\n before %v\n after  %v", check.name, check.before, check.after)
		}
	}
	assertSingleActiveReaction(t, h.reactions, stored.MessageID, "👍", hwtShoshana, false)
	if codec, processed := h.inboxRow(t, record.DedupeKey); codec != GoogleHistoryCodec || !processed {
		t.Fatalf("history inbox row codec=%q processed=%v", codec, processed)
	}
	if after.HistoryAppended-before.HistoryAppended != 1 ||
		after.HistoryExisting-before.HistoryExisting != 1 ||
		after.HistoryImported != before.HistoryImported ||
		after.Projected != before.Projected ||
		after.ReactionsApplied != before.ReactionsApplied ||
		after.ReactionsRemoved != before.ReactionsRemoved ||
		after.ReactionsOrphaned != before.ReactionsOrphaned ||
		after.ContentDupesSkipped != before.ContentDupesSkipped ||
		after.RemoteRebinds != 0 ||
		after.Quarantined != 0 {
		t.Fatalf("counters before=%+v after=%+v", before, after)
	}

	// Contrast (live semantics, reader-worker-semantics probe): the same
	// content as a live frame resurrects the deleted row, overwrites the body
	// and tombstones the reaction. A different sender name gives it a new
	// dedupe key, so it is projected rather than replayed.
	contrast := fetched
	contrast.name = "Karl (live)"
	h.liveMessage(t, contrast.proto())
	h.pump(t)
	clobbered := h.message(t, "hwt-b", "hwt-b-1")
	if clobbered.State != sqlite.MessageStateActive || clobbered.Body != fetched.body {
		t.Fatalf("contrast: live re-delivery = %+v, want resurrected with the new body", clobbered)
	}
	assertNoActiveReactions(t, h.reactions, stored.MessageID)
}

// ---------------------------------------------------------------------------
// (c) Insert-only via a content duplicate

// A fetched message re-keyed under a new remote ID (same thread, sender,
// millisecond and body as a stored row) is not inserted and does not refresh
// the stored row's reactions.
func TestHistoryWorkerSkipsContentDuplicateUnderNewRemoteID(t *testing.T) {
	h := newHWTHarness(t, false)
	group := hwtConversation("hwt-c", "Re-keyed", true, hwtKarl, hwtShoshana)
	h.liveConversation(t, group)
	at := hwtStart.Add(-2 * time.Hour)
	original := hwtMessage{
		id:           "hwt-c-A",
		conversation: "hwt-c",
		body:         "same words, new id",
		from:         hwtKarl,
		at:           at,
		reactions:    []hwtReaction{{"❤️", hwtShoshana}},
	}
	h.liveMessage(t, original.proto())
	h.pump(t)
	stored := h.message(t, "hwt-c", "hwt-c-A")
	assertSingleActiveReaction(t, h.reactions, stored.MessageID, "❤️", hwtShoshana, false)
	rowBefore := h.rows(t, `SELECT * FROM messages WHERE message_id = ?`, stored.MessageID)
	reactionsBefore := h.rows(t, `SELECT * FROM reactions WHERE message_id = ?`, stored.MessageID)
	fenceBefore := h.rows(t, `SELECT * FROM reaction_snapshot_fences WHERE message_id = ?`, stored.MessageID)
	before := h.counts()

	rekeyed := original
	rekeyed.id = "hwt-c-B"
	rekeyed.reactions = nil
	h.historyMessage(t, group, rekeyed.proto())
	h.pump(t)
	after := h.counts()

	if got := i01QueryInt64(t, h.path, `SELECT COUNT(*) FROM messages WHERE remote_message_id = 'hwt-c-B'`); got != 0 {
		t.Fatalf("rows for the re-keyed copy = %d, want 0", got)
	}
	if got := idsMessageCount(t, h.i01Harness, stored.ConversationID); got != 1 {
		t.Fatalf("messages in thread = %d, want 1", got)
	}
	if got := h.rows(t, `SELECT * FROM messages WHERE message_id = ?`, stored.MessageID); !reflect.DeepEqual(got, rowBefore) {
		t.Fatalf("original row changed:\n before %v\n after  %v", rowBefore, got)
	}
	if got := h.rows(t, `SELECT * FROM reactions WHERE message_id = ?`, stored.MessageID); !reflect.DeepEqual(got, reactionsBefore) {
		t.Fatalf("original reactions changed:\n before %v\n after  %v", reactionsBefore, got)
	}
	if got := h.rows(t, `SELECT * FROM reaction_snapshot_fences WHERE message_id = ?`, stored.MessageID); !reflect.DeepEqual(got, fenceBefore) {
		t.Fatalf("original reaction fence changed:\n before %v\n after  %v", fenceBefore, got)
	}
	if after.HistoryExisting-before.HistoryExisting != 1 ||
		after.ContentDupesSkipped-before.ContentDupesSkipped != 1 ||
		after.HistoryImported != before.HistoryImported ||
		after.ReactionsRemoved != before.ReactionsRemoved ||
		after.ReactionsApplied != before.ReactionsApplied ||
		after.Quarantined != 0 {
		t.Fatalf("counters before=%+v after=%+v", before, after)
	}

	// Contrast (live semantics): a live re-delivery under the new ID lets its
	// empty reaction snapshot refresh the original, tombstoning ❤️.
	contrast := rekeyed
	contrast.name = "Karl (live)"
	h.liveMessage(t, contrast.proto())
	h.pump(t)
	if got := h.counts().ReactionsRemoved - after.ReactionsRemoved; got != 1 {
		t.Fatalf("contrast: live duplicate removed %d reactions, want 1", got)
	}
	assertNoActiveReactions(t, h.reactions, stored.MessageID)
}

// ---------------------------------------------------------------------------
// (d) Create-only conversations

// A fetched snapshot of a bound thread changes nothing; a fetched snapshot of
// an unbound thread creates it with its title, kind and roster.
func TestHistoryWorkerConversationSnapshotsAreCreateOnly(t *testing.T) {
	h := newHWTHarness(t, false)
	h.liveConversation(t, hwtConversation("hwt-d-bound", "Book club", true, hwtKarl, hwtShoshana))
	h.pump(t)
	bound := h.conversation(t, "hwt-d-bound")
	threadBefore := h.rows(t, `SELECT * FROM conversations WHERE conversation_id = ?`, bound.ConversationID)
	rosterBefore := h.rows(t, `SELECT * FROM conversation_participants WHERE conversation_id = ? ORDER BY identity_id`, bound.ConversationID)
	before := h.counts()

	// An older listing: other title, roster overlapping the stored one (live
	// semantics would treat it as consistent and refresh the thread).
	stale := hwtConversation("hwt-d-bound", "Renamed in an old listing", true, hwtKarl, hwtAda)
	h.historyConversation(t, stale)
	h.historyConversation(t, hwtConversation("hwt-d-new", "Weekend plans", true, hwtAda, hwtBea))
	h.pump(t)
	after := h.counts()

	if got := h.rows(t, `SELECT * FROM conversations WHERE conversation_id = ?`, bound.ConversationID); !reflect.DeepEqual(got, threadBefore) {
		t.Fatalf("bound thread changed:\n before %v\n after  %v", threadBefore, got)
	}
	if got := h.rows(t, `SELECT * FROM conversation_participants WHERE conversation_id = ? ORDER BY identity_id`, bound.ConversationID); !reflect.DeepEqual(got, rosterBefore) {
		t.Fatalf("bound roster changed:\n before %v\n after  %v", rosterBefore, got)
	}

	created := h.conversation(t, "hwt-d-new")
	if created.Kind != sqlite.ConversationKindGroup || created.Title != "Weekend plans" || created.LastMessageAtMS != 0 {
		t.Fatalf("created thread = %+v, want group \"Weekend plans\" with no recency", created)
	}
	want := []string{hwtAda, hwtBea}
	sort.Strings(want) // peers() returns sorted numbers
	if got := h.peers(t, created.ConversationID); !reflect.DeepEqual(got, want) {
		t.Fatalf("created roster peers = %v, want %v", got, want)
	}
	if got := i01QueryInt64(t, h.path, `
		SELECT COUNT(*) FROM conversation_participants p
		JOIN identities i ON i.identity_id = p.identity_id
		WHERE p.conversation_id = ? AND i.is_self = 1
	`, created.ConversationID); got != 1 {
		t.Fatalf("self participants on created thread = %d, want 1", got)
	}
	if after.HistoryConversations-before.HistoryConversations != 1 ||
		after.HistoryAppended-before.HistoryAppended != 2 ||
		after.RemoteRebinds != 0 ||
		after.Quarantined != 0 {
		t.Fatalf("counters before=%+v after=%+v", before, after)
	}

	// Contrast (live semantics): the same snapshot pushed live (byte-identical,
	// so it dedupes onto the history row and is replayed as live) rewrites the
	// bound thread's title and roster.
	h.liveConversation(t, stale)
	h.pump(t)
	if got := h.conversation(t, "hwt-d-bound"); got.Title != "Renamed in an old listing" {
		t.Fatalf("contrast: live snapshot title = %q, want it applied", got.Title)
	}
	wantContrast := []string{hwtAda, hwtKarl}
	sort.Strings(wantContrast) // peers() returns sorted numbers
	if got := h.peers(t, bound.ConversationID); !reflect.DeepEqual(got, wantContrast) {
		t.Fatalf("contrast: live snapshot roster = %v, want %v", got, wantContrast)
	}
}

// ---------------------------------------------------------------------------
// (e) Ordering (I7)

// A message of a group v2 has never seen, fetched with its group snapshot, is
// filed into that group even though its sender already has a 1:1 thread.
func TestHistoryWorkerGroupSnapshotLeadsItsMessage(t *testing.T) {
	setup := func(t *testing.T) (*hwtHarness, sqlite.Conversation) {
		t.Helper()
		h := newHWTHarness(t, false)
		h.liveConversation(t, hwtConversation("hwt-e-karl", "Karl", false, hwtKarl))
		h.liveMessage(t, hwtMessage{
			id:           "hwt-e-k1",
			conversation: "hwt-e-karl",
			body:         "1:1 hello",
			from:         hwtKarl,
			at:           hwtStart.Add(-5 * time.Hour),
		}.proto())
		h.pump(t)
		karl := h.conversation(t, "hwt-e-karl")
		if karl.Kind != sqlite.ConversationKindDirect {
			t.Fatalf("Karl's thread = %+v, want direct", karl)
		}
		if got := h.peers(t, karl.ConversationID); !reflect.DeepEqual(got, []string{hwtKarl}) {
			t.Fatalf("Karl's thread peers = %v", got)
		}
		return h, karl
	}
	groupMessage := hwtMessage{
		id:           "hwt-e-g1",
		conversation: "hwt-e-group",
		body:         "group hello",
		from:         hwtKarl,
		at:           hwtStart.Add(-4 * time.Hour),
	}

	t.Run("history", func(t *testing.T) {
		h, karl := setup(t)
		karlBefore := h.rows(t, `SELECT * FROM conversations WHERE conversation_id = ?`, karl.ConversationID)
		karlRosterBefore := h.rows(t, `SELECT * FROM conversation_participants WHERE conversation_id = ? ORDER BY identity_id`, karl.ConversationID)

		group := hwtConversation("hwt-e-group", "Karl and Shoshana", true, hwtKarl, hwtShoshana)
		h.historyMessage(t, group, groupMessage.proto())
		h.pump(t)

		thread := h.conversation(t, "hwt-e-group")
		if thread.ConversationID == karl.ConversationID ||
			thread.Kind != sqlite.ConversationKindGroup ||
			thread.Title != "Karl and Shoshana" {
			t.Fatalf("group thread = %+v, want a new group distinct from Karl's 1:1", thread)
		}
		if got, want := h.peers(t, thread.ConversationID), []string{hwtKarl, hwtShoshana}; !reflect.DeepEqual(got, want) {
			t.Fatalf("group peers = %v, want %v", got, want)
		}
		if stored := h.message(t, "hwt-e-group", "hwt-e-g1"); stored.ConversationID != thread.ConversationID {
			t.Fatalf("group message filed in %q, want %q", stored.ConversationID, thread.ConversationID)
		}
		if got := h.conversation(t, "hwt-e-karl"); got.ConversationID != karl.ConversationID {
			t.Fatalf("Karl's wire ID now bound to %q, want %q", got.ConversationID, karl.ConversationID)
		}
		if got := h.rows(t, `SELECT * FROM conversations WHERE conversation_id = ?`, karl.ConversationID); !reflect.DeepEqual(got, karlBefore) {
			t.Fatalf("Karl's thread changed:\n before %v\n after  %v", karlBefore, got)
		}
		if got := h.rows(t, `SELECT * FROM conversation_participants WHERE conversation_id = ? ORDER BY identity_id`, karl.ConversationID); !reflect.DeepEqual(got, karlRosterBefore) {
			t.Fatalf("Karl's roster changed:\n before %v\n after  %v", karlRosterBefore, got)
		}
		if got := idsMessageCount(t, h.i01Harness, karl.ConversationID); got != 1 {
			t.Fatalf("messages in Karl's thread = %d, want 1", got)
		}
		snapshot := h.counts()
		if snapshot.RemoteRebinds != 0 || snapshot.HistoryConversations != 1 ||
			snapshot.HistoryImported != 1 || snapshot.Quarantined != 0 {
			t.Fatalf("counters = %+v", snapshot)
		}
	})

	// Contrast (intended live behavior under #176, not a history defect): the
	// same message pushed live with no conversation frame is routed by sender
	// into Karl's 1:1 thread, which takes the group's wire ID. This is why
	// history message frames carry their snapshot.
	t.Run("contrast live without snapshot", func(t *testing.T) {
		h, karl := setup(t)
		h.liveMessage(t, groupMessage.proto())
		h.pump(t)
		if _, err := h.messages.GetMessageByRemote(
			context.Background(), i01AccountID, karl.ConversationID, "hwt-e-g1",
		); err != nil {
			t.Fatalf("live group message not in Karl's thread: %v", err)
		}
		if got := h.conversation(t, "hwt-e-group"); got.ConversationID != karl.ConversationID {
			t.Fatalf("group wire ID bound to %q, want Karl's thread %q", got.ConversationID, karl.ConversationID)
		}
		if _, err := h.store.GetConversationByRemote(i01AccountID, "hwt-e-karl"); !errors.Is(err, sqlite.ErrNotFound) {
			t.Fatalf("Karl's own wire ID lookup error = %v, want ErrNotFound", err)
		}
		if got := h.counts().RemoteRebinds; got != 1 {
			t.Fatalf("remote rebinds = %d, want 1", got)
		}
	})
}

// ---------------------------------------------------------------------------
// (f) Reactions

// The embedded reaction snapshot applies only to a message history inserted;
// a skipped copy applies nothing, and tapback bodies count no orphans.
func TestHistoryWorkerAppliesReactionsOnlyToInsertedMessages(t *testing.T) {
	h := newHWTHarness(t, false)
	group := hwtConversation("hwt-f", "Reactions", true, hwtKarl, hwtShoshana)
	h.liveConversation(t, group)
	at := hwtStart.Add(-3 * time.Hour)
	existing := hwtMessage{id: "hwt-f-existing", conversation: "hwt-f", body: "already here", from: hwtKarl, at: at}
	h.liveMessage(t, existing.proto())
	h.pump(t)
	existingRow := h.message(t, "hwt-f", "hwt-f-existing")
	assertNoActiveReactions(t, h.reactions, existingRow.MessageID)
	fenceBefore := h.rows(t, `SELECT * FROM reaction_snapshot_fences WHERE message_id = ?`, existingRow.MessageID)
	before := h.counts()

	inserted := hwtMessage{
		id:           "hwt-f-new",
		conversation: "hwt-f",
		body:         "missed while offline",
		from:         hwtShoshana,
		at:           at.Add(time.Minute),
		reactions:    []hwtReaction{{"👍", hwtKarl}},
	}
	refetched := existing
	refetched.reactions = []hwtReaction{{"❤️", hwtShoshana}}
	tapback := hwtMessage{
		id:           "hwt-f-tapback",
		conversation: "hwt-f",
		body:         `Liked "already here"`,
		from:         hwtShoshana,
		at:           at.Add(2 * time.Minute),
	}
	refetchedTapback := tapback
	refetchedTapback.name = "Shoshana (refetched)"
	h.historyMessage(t, group, inserted.proto())
	h.historyMessage(t, group, refetched.proto())
	h.historyMessage(t, group, tapback.proto())
	h.historyMessage(t, group, refetchedTapback.proto()) // skipped: v2 now holds it
	h.pump(t)
	after := h.counts()

	newRow := h.message(t, "hwt-f", "hwt-f-new")
	assertSingleActiveReaction(t, h.reactions, newRow.MessageID, "👍", hwtKarl, false)
	if got := i01QueryInt64(t, h.path, `SELECT COUNT(*) FROM reactions WHERE message_id = ?`, existingRow.MessageID); got != 0 {
		t.Fatalf("reaction rows on the skipped message = %d, want 0", got)
	}
	if got := h.rows(t, `SELECT * FROM reaction_snapshot_fences WHERE message_id = ?`, existingRow.MessageID); !reflect.DeepEqual(got, fenceBefore) {
		t.Fatalf("skipped message's reaction fence changed:\n before %v\n after  %v", fenceBefore, got)
	}
	tapbackRow := h.message(t, "hwt-f", "hwt-f-tapback")
	if tapbackRow.Body != tapback.body {
		t.Fatalf("tapback row body = %q, want it stored as text like live", tapbackRow.Body)
	}
	assertNoActiveReactions(t, h.reactions, tapbackRow.MessageID)
	// The live path counts a tapback's unresolved target as orphaned
	// (TestGoogleTapbackReactionIsCountedOrphanNotQuarantined); history never
	// counts reactions_orphaned.
	if after.ReactionsApplied-before.ReactionsApplied != 1 ||
		after.ReactionsRemoved != before.ReactionsRemoved ||
		after.ReactionsOrphaned != 0 ||
		after.TapbackMessages-before.TapbackMessages != 2 ||
		after.HistoryImported-before.HistoryImported != 2 ||
		after.HistoryExisting-before.HistoryExisting != 2 ||
		after.Quarantined != 0 {
		t.Fatalf("counters before=%+v after=%+v", before, after)
	}
}

// ---------------------------------------------------------------------------
// (g) Echo (gap case)

// newHWTEchoHarness is newGoogleEchoHarness (google_echo_e2e_test.go) with the
// production Google decoder pair, so history frames reach the same
// MessageService echo observer as live ones.
func newHWTEchoHarness(t *testing.T) *googleEchoHarness {
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
	service, err := messaging.NewMessageService(
		store,
		&googleEchoRegistry{sender: &googleEchoTextSender{clock: clock}},
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
	reactions, err := sqlite.NewReactionRepository(store, clock.Now)
	if err != nil {
		t.Fatalf("NewReactionRepository(): %v", err)
	}
	counters := &Counters{}
	worker, err := NewWorker(WorkerConfig{
		Store:        store,
		Messages:     messages,
		Reactions:    reactions,
		EchoObserver: service,
		Counters:     counters,
		Logger:       zerolog.Nop(),
		Now:          clock.Now,
		Decoders:     hwtRegistrations(counters),
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
		reactions: reactions,
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

// A send whose live echo never arrived is reconciled by the fetched copy of
// the outgoing message exactly as the live echo would (EchoEnriched, compare
// TestGoogleEchoEndToEndOrderAEnrichesPlaceholder), leaving one row. A later
// re-fetch is skipped before projection and observes no echo.
func TestHistoryWorkerReconcilesSendWhoseLiveEchoWasLost(t *testing.T) {
	harness := newHWTEchoHarness(t)
	const body = "sent while the push channel stalled"
	submission, requestID := harness.enqueueAndConfirm(t, "history-lost-echo", body)
	local, err := harness.messages.GetMessageByRemote(
		context.Background(), googleEchoAccountID, googleEchoConversationID, requestID,
	)
	if err != nil {
		t.Fatalf("optimistic local row: %v", err)
	}

	const permanentID = "google-permanent-history-lost-echo"
	snapshot := &gmproto.Conversation{
		ConversationID: googleEchoRemoteConversationID,
		Name:           "Echo test (as listed)",
		Participants: []*gmproto.Participant{
			{IsMe: true, FullName: "Max", ID: &gmproto.SmallInfo{Number: hwtSelf}},
			{FullName: "Echo Sender", ID: &gmproto.SmallInfo{Number: "+15551234567"}},
		},
	}
	appendHistory := func(message *gmproto.Message) {
		t.Helper()
		record, err := GoogleHistoryMessageRecord(
			googleEchoAccountID, 1, googleEchoRemoteConversationID, snapshot, message,
			googleEchoNow.Add(2*time.Minute),
		)
		if err != nil {
			t.Fatalf("GoogleHistoryMessageRecord(): %v", err)
		}
		if err := harness.sink.AppendHistoryIngress(context.Background(), record); err != nil {
			t.Fatalf("AppendHistoryIngress(): %v", err)
		}
	}

	appendHistory(googleEchoMessage(permanentID, requestID, body, true, googleEchoNow.Add(time.Minute)))
	harness.waitFor(t, "history echo reconcile", func(snapshot CounterSnapshot) bool {
		return snapshot.EchoEnriched == 1 && snapshot.HistoryExisting == 1
	})
	i01AssertNoPending(t, harness.messages)

	harness.assertSinglePermanentMessage(t, submission.LocalMessageID, requestID, permanentID)
	delivery, err := harness.service.Get(context.Background(), submission.OutboxID)
	if err != nil {
		t.Fatalf("MessageService.Get(): %v", err)
	}
	if delivery.State != messaging.OutboxConfirmed || delivery.RemoteMessageID != permanentID {
		t.Fatalf("delivery after history echo = %+v, want confirmed at %q", delivery, permanentID)
	}
	// The echo re-keys the local row; the history insert then finds the
	// natural key taken and adds nothing (intended: unlike the live upsert it
	// does not overwrite the row's content with the fetched copy).
	keyed, err := harness.messages.GetMessageByRemote(
		context.Background(), googleEchoAccountID, googleEchoConversationID, permanentID,
	)
	if err != nil {
		t.Fatalf("re-keyed row: %v", err)
	}
	if keyed.Body != local.Body || keyed.OccurredAtMS != local.OccurredAtMS ||
		keyed.Direction != local.Direction || keyed.State != local.State {
		t.Fatalf("re-keyed row = %+v, want the local row's content %+v", keyed, local)
	}
	first := harness.counters.Snapshot(googleEchoAccountID)
	if first.Projected != 0 || first.Appended != 0 || first.HistoryAppended != 1 ||
		first.HistoryImported != 0 || first.EchoNotFound != 0 || first.EchoNoop != 0 ||
		first.EchoReconciled != 0 || first.EchoErrors != 0 || first.Quarantined != 0 {
		t.Fatalf("counters after history echo = %+v", first)
	}
	if got := harness.countInboxRows(t); got != 1 {
		t.Fatalf("inbox rows = %d, want 1", got)
	}

	// A later catch-up fetches the message again with different content.
	appendHistory(googleEchoMessage(permanentID, requestID, body+" (refetched)", true, googleEchoNow.Add(time.Minute)))
	harness.waitFor(t, "re-fetch skipped", func(snapshot CounterSnapshot) bool {
		return snapshot.HistoryExisting == 2
	})
	i01AssertNoPending(t, harness.messages)
	second := harness.counters.Snapshot(googleEchoAccountID)
	if second.EchoEnriched != 1 || second.EchoNoop != 0 || second.EchoNotFound != 0 ||
		second.EchoReconciled != 0 || second.EchoErrors != 0 || second.HistoryImported != 0 {
		t.Fatalf("counters after re-fetch = %+v, want no second echo observation", second)
	}
	harness.assertSinglePermanentMessage(t, submission.LocalMessageID, requestID, permanentID)
	if again, err := harness.messages.GetMessageByRemote(
		context.Background(), googleEchoAccountID, googleEchoConversationID, permanentID,
	); err != nil || again.Body != body {
		t.Fatalf("row after re-fetch = %+v, %v; want body %q", again, err, body)
	}
	if conversation, err := harness.store.GetConversation(googleEchoConversationID); err != nil || conversation.Title != "Echo test" {
		t.Fatalf("thread after history = %+v, %v; want the bound title unchanged", conversation, err)
	}
}

// ---------------------------------------------------------------------------
// (h) Idempotence (I2)

// Re-running the same catch-up adds no inbox rows and changes nothing.
func TestHistoryWorkerRepeatedCatchUpIsIdempotent(t *testing.T) {
	h := newHWTHarness(t, false)
	g1 := hwtConversation("hwt-h-g1", "Old friends", true, hwtKarl, hwtShoshana)
	h.liveConversation(t, g1)
	at := hwtStart.Add(-48 * time.Hour)
	existingIncoming := hwtMessage{
		id:           "hwt-h-1",
		conversation: "hwt-h-g1",
		body:         "live hello",
		from:         hwtKarl,
		at:           at,
		reactions:    []hwtReaction{{"👍", hwtShoshana}},
	}
	existingOutgoing := hwtMessage{id: "hwt-h-2", conversation: "hwt-h-g1", body: "live reply", at: at.Add(time.Minute)}
	h.liveMessage(t, existingIncoming.proto())
	h.liveMessage(t, existingOutgoing.proto())
	h.pump(t)

	g2 := hwtConversation("hwt-h-g2", "New circle", true, hwtAda, hwtBea)
	changedCopy := existingIncoming
	changedCopy.body = "fetched copy differs"
	changedCopy.reactions = nil
	missed := hwtMessage{
		id:           "hwt-h-3",
		conversation: "hwt-h-g1",
		body:         "missed while offline",
		from:         hwtShoshana,
		at:           at.Add(2 * time.Minute),
		media:        &gmproto.MediaContent{MediaID: "media-hwt-h", MimeType: "image/jpeg", Size: 9},
		reactions:    []hwtReaction{{"😂", hwtKarl}},
	}
	inNewGroup := hwtMessage{id: "hwt-h-4", conversation: "hwt-h-g2", body: "hi all", from: hwtAda, at: at.Add(3 * time.Minute)}
	sentInNewGroup := hwtMessage{id: "hwt-h-5", conversation: "hwt-h-g2", body: "hi Ada", at: at.Add(4 * time.Minute)}
	stub := hwtMessage{id: "hwt-h-6", conversation: "hwt-h-g1", from: hwtKarl, at: at.Add(5 * time.Minute)}
	contentDuplicate := existingIncoming
	contentDuplicate.id = "hwt-h-7"
	contentDuplicate.reactions = nil

	records := []bridge.RawIngressRecord{
		h.historyConversation(t, hwtConversation("hwt-h-g1", "Renamed in an old listing", true, hwtKarl, hwtShoshana)),
		h.historyConversation(t, g2),
		h.historyMessage(t, g1, changedCopy.proto()),
		h.historyMessage(t, g1, existingOutgoing.proto()), // byte-identical to the live frame
		h.historyMessage(t, g1, missed.proto()),
		h.historyMessage(t, g2, inNewGroup.proto()),
		h.historyMessage(t, g2, sentInNewGroup.proto()),
		h.historyMessage(t, g1, stub.proto()),
		h.historyMessage(t, g1, contentDuplicate.proto()),
	}
	h.pump(t)
	first := h.counts()
	// 9 records: one shares the live frame's dedupe key; it collapses onto
	// the live row and is never replayed. Inserted: missed, inNewGroup,
	// sentInNewGroup. Existing: changedCopy, contentDuplicate. Created: g2.
	// The stub decodes to no message.
	if first.HistoryAppended != 8 || first.HistoryDeduped != 1 ||
		first.HistoryImported != 3 || first.HistoryExisting != 2 ||
		first.HistoryConversations != 1 || first.EmptyStubsSkipped != 1 ||
		first.ReactionsApplied != 2 || first.ReactionsRemoved != 0 ||
		first.RemoteRebinds != 0 || first.Quarantined != 0 {
		t.Fatalf("first catch-up counters = %+v", first)
	}
	if got := h.message(t, "hwt-h-g1", "hwt-h-1").Body; got != "live hello" {
		t.Fatalf("existing body after first catch-up = %q", got)
	}
	if got := h.conversation(t, "hwt-h-g1").Title; got != "Old friends" {
		t.Fatalf("bound title after first catch-up = %q", got)
	}
	settled := h.state(t)
	inboxRows := i01QueryInt64(t, h.path, `SELECT COUNT(*) FROM inbox`)

	// The same catch-up an hour later: same protos, fresh receipt times.
	h.clock.Set(h.clock.Now().Add(time.Hour))
	for _, record := range records {
		record.ReceivedAt = h.tick()
		h.appendHistory(t, record)
	}
	h.pump(t)
	second := h.counts()

	if diff := hwtStateDiff(settled, h.state(t)); diff != "" {
		t.Fatalf("repeated catch-up changed the store:\n%s", diff)
	}
	if got := i01QueryInt64(t, h.path, `SELECT COUNT(*) FROM inbox`); got != inboxRows {
		t.Fatalf("inbox rows = %d after repeat, want %d", got, inboxRows)
	}
	if second.HistoryDeduped-first.HistoryDeduped != uint64(len(records)) ||
		second.HistoryAppended != first.HistoryAppended ||
		second.HistoryImported != first.HistoryImported ||
		second.HistoryConversations != first.HistoryConversations ||
		second.HistoryExisting != first.HistoryExisting || // deduplicated history is never replayed
		second.ReactionsApplied != first.ReactionsApplied ||
		second.ReactionsRemoved != first.ReactionsRemoved ||
		second.RemoteRebinds != 0 ||
		second.Quarantined != 0 {
		t.Fatalf("repeat counters first=%+v second=%+v", first, second)
	}
}

// ---------------------------------------------------------------------------
// (i) Registration

func TestHistoryWorkerRegistrationIsGoogleOnly(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "store.sqlite3"))
	if err != nil {
		t.Fatalf("sqlite.Open(): %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	messages := i01NewMessageRepository(t, store)
	reactions, err := sqlite.NewReactionRepository(store, func() time.Time { return i01TestTime })
	if err != nil {
		t.Fatalf("sqlite.NewReactionRepository(): %v", err)
	}
	decoder := i01DecoderFunc(func(context.Context, bridge.RawIngressRecord) ([]bridge.Event, error) {
		return nil, nil
	})
	config := func(registrations ...DecoderRegistration) WorkerConfig {
		return WorkerConfig{Store: store, Messages: messages, Reactions: reactions, Decoders: registrations}
	}

	for _, platform := range []bridge.Platform{bridge.PlatformWhatsApp, bridge.PlatformSignal} {
		t.Run(string(platform), func(t *testing.T) {
			const codec = "test.hwt.history"
			_, err := NewWorker(config(DecoderRegistration{Codec: codec, Platform: platform, Decoder: decoder, History: true}))
			want := fmt.Sprintf("history codec %q is only supported for %q", codec, bridge.PlatformGoogle)
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("NewWorker(History on %s) error = %v, want %q", platform, err, want)
			}
			// Control: the same registration without History is valid, so the
			// rejection is about history, not the platform or codec.
			if _, err := NewWorker(config(DecoderRegistration{Codec: codec, Platform: platform, Decoder: decoder})); err != nil {
				t.Fatalf("NewWorker(live %s) error = %v", platform, err)
			}
		})
	}

	worker, err := NewWorker(config(hwtRegistrations(&Counters{})...))
	if err != nil {
		t.Fatalf("NewWorker(Google pair): %v", err)
	}
	if !worker.decoders[GoogleHistoryCodec].history || worker.decoders[GoogleCodec].history {
		t.Fatalf("registered history flags: live=%v history=%v, want false/true",
			worker.decoders[GoogleCodec].history, worker.decoders[GoogleHistoryCodec].history)
	}
}

// ---------------------------------------------------------------------------
// (j) Property: I3 insert-only and I2 idempotence over random catch-ups

const (
	hwtPropBaseMS    = int64(1_780_000_000_000)
	hwtPropWindowMS  = int64(60_000)
	hwtPropMaxQueued = 48 // below the worker queue capacity, so no replay notification is dropped
)

// Distinct exact rosters, so no history-created group can match a live one
// by roster (that path is covered by the #176 tests, not by I3).
var (
	hwtPropRosters  = [][]string{{hwtKarl, hwtShoshana}, {hwtShoshana, hwtAda}, {hwtKarl, hwtAda}}
	hwtPropReactors = []string{hwtKarl, hwtShoshana, hwtAda}
	hwtPropEmoji    = []string{"👍", "❤️", "😂"}
)

type hwtPropReaction struct{ Emoji, Actor string }

type hwtPropVersion struct {
	Body      string
	AtMS      int64
	Outgoing  bool
	Sender    string
	Name      string
	ReplyTo   string
	MediaID   string
	MIME      string
	Reactions []hwtPropReaction
}

func (v hwtPropVersion) stub() bool {
	return v.Body == "" && v.MediaID == "" && len(v.Reactions) == 0
}

// insertable mirrors what the worker can store: v2 rejects a non-positive
// timestamp (intended divergence D2: legacy stores ts=0) and the decoder drops
// a contentless complete-status stub.
func (v hwtPropVersion) insertable() bool {
	return v.AtMS > 0 && !v.stub()
}

func (v hwtPropVersion) proto(id, conversation string) *gmproto.Message {
	spec := hwtMessage{
		id:           id,
		conversation: conversation,
		body:         v.Body,
		at:           time.UnixMilli(v.AtMS),
		reply:        v.ReplyTo,
	}
	if !v.Outgoing {
		spec.from = v.Sender
		spec.name = v.Name
	}
	if v.MediaID != "" {
		spec.media = &gmproto.MediaContent{
			MediaID:       v.MediaID,
			MediaName:     v.MediaID + ".bin",
			MimeType:      v.MIME,
			Size:          7,
			DecryptionKey: []byte{1, 2, 3},
		}
	}
	for _, reaction := range v.Reactions {
		spec.reactions = append(spec.reactions, hwtReaction{reaction.Emoji, reaction.Actor})
	}
	return spec.proto()
}

type hwtPropConversation struct {
	Wire   string
	Roster []string
	Live   bool // false: unknown to v2 until history creates it
}

type hwtPropMessage struct {
	ID           string
	Conversation int
	Delivered    bool // delivered live before the catch-up
	Live         hwtPropVersion
	Mutation     string // "", "deleted" or "edited", applied in SQL after live delivery
}

type hwtPropFrame struct {
	Message      int // index into Messages; -1 for a standalone conversation snapshot
	Conversation int
	Title        string // snapshot title (embedded or standalone)
	Version      hwtPropVersion
	Repeat       bool // exact re-delivery of an earlier frame
	PumpAfter    bool // let the worker run after this append
}

type hwtPropUniverse struct {
	Conversations []hwtPropConversation
	Messages      []hwtPropMessage
	Frames        []hwtPropFrame
}

func hwtPropNewVersion(r *rand.Rand, index, messageCount int, roster []string, allowInvalid bool) hwtPropVersion {
	version := hwtPropVersion{Outgoing: r.Intn(3) == 0}
	if !version.Outgoing {
		version.Sender = roster[r.Intn(len(roster))]
		version.Name = fmt.Sprintf("%s v%d", hwtNames[version.Sender], r.Intn(3))
	}
	// Each message ID owns a disjoint one-minute window, so versions of
	// different IDs never share the content-duplicate key (thread, ms, body,
	// sender); content duplicates have their own test.
	version.AtMS = hwtPropBaseMS + int64(index)*hwtPropWindowMS + 1 + r.Int63n(hwtPropWindowMS-1)
	version.Body = fmt.Sprintf("m%d says %d", index, r.Intn(1000))
	if r.Intn(4) == 0 {
		version.MediaID = fmt.Sprintf("media-%d-%d", index, r.Intn(100))
		version.MIME = []string{"image/jpeg", "video/mp4"}[r.Intn(2)]
	}
	if r.Intn(3) == 0 {
		version.ReplyTo = fmt.Sprintf("hwt-p-m%d", r.Intn(messageCount))
	}
	if r.Intn(3) == 0 {
		for _, actor := range r.Perm(len(hwtPropReactors))[:1+r.Intn(2)] {
			version.Reactions = append(version.Reactions, hwtPropReaction{
				Emoji: hwtPropEmoji[r.Intn(len(hwtPropEmoji))],
				Actor: hwtPropReactors[actor],
			})
		}
	}
	if allowInvalid {
		switch r.Intn(10) {
		case 0:
			version.AtMS = 0
		case 1:
			version.Body, version.MediaID, version.MIME, version.Reactions = "", "", "", nil
		case 2:
			version.Body = ""
		}
	}
	return version
}

func hwtPropTitle(r *rand.Rand, conversation int) string {
	return fmt.Sprintf("Group %d v%d", conversation, r.Intn(3))
}

func (hwtPropUniverse) Generate(r *rand.Rand, _ int) reflect.Value {
	var universe hwtPropUniverse
	conversationCount := 2 + r.Intn(2)
	for index := 0; index < conversationCount; index++ {
		universe.Conversations = append(universe.Conversations, hwtPropConversation{
			Wire:   fmt.Sprintf("hwt-p-c%d", index),
			Roster: hwtPropRosters[index],
			Live:   index < conversationCount-1 || r.Intn(2) == 0,
		})
	}
	messageCount := 1 + r.Intn(8)
	for index := 0; index < messageCount; index++ {
		message := hwtPropMessage{ID: fmt.Sprintf("hwt-p-m%d", index), Conversation: r.Intn(conversationCount)}
		if universe.Conversations[message.Conversation].Live && r.Intn(2) == 0 {
			message.Delivered = true
			message.Live = hwtPropNewVersion(r, index, messageCount, universe.Conversations[message.Conversation].Roster, false)
			message.Mutation = []string{"", "deleted", "edited"}[r.Intn(3)]
		}
		universe.Messages = append(universe.Messages, message)
	}

	var frames []hwtPropFrame
	for index, message := range universe.Messages {
		roster := universe.Conversations[message.Conversation].Roster
		for count := r.Intn(3); count > 0; count-- {
			version := hwtPropNewVersion(r, index, messageCount, roster, true)
			if message.Delivered && r.Intn(4) == 0 {
				version = message.Live // byte-identical to the live frame: dedupes onto its row
			}
			frames = append(frames, hwtPropFrame{
				Message:      index,
				Conversation: message.Conversation,
				Title:        hwtPropTitle(r, message.Conversation),
				Version:      version,
			})
		}
	}
	for index := range universe.Conversations {
		if r.Intn(2) == 0 {
			frames = append(frames, hwtPropFrame{Message: -1, Conversation: index, Title: hwtPropTitle(r, index)})
		}
	}
	r.Shuffle(len(frames), func(i, j int) { frames[i], frames[j] = frames[j], frames[i] })
	for count := r.Intn(4); count > 0 && len(frames) > 0; count-- {
		original := r.Intn(len(frames))
		repeat := frames[original]
		repeat.Repeat = true
		at := original + 1 + r.Intn(len(frames)-original)
		frames = append(frames[:at], append([]hwtPropFrame{repeat}, frames[at:]...)...)
	}
	for index := range frames {
		frames[index].PumpAfter = r.Intn(3) == 0
	}
	universe.Frames = frames
	return reflect.ValueOf(universe)
}

func (u hwtPropUniverse) record(t *testing.T, h *hwtHarness, frame hwtPropFrame) bridge.RawIngressRecord {
	t.Helper()
	conversation := u.Conversations[frame.Conversation]
	snapshot := hwtConversation(conversation.Wire, frame.Title, true, conversation.Roster...)
	if frame.Message < 0 {
		return h.historyConversation(t, snapshot)
	}
	return h.historyMessage(t, snapshot, frame.Version.proto(u.Messages[frame.Message].ID, conversation.Wire))
}

// hwtPropCheck runs one universe: live delivery, SQL mutation, a catch-up in
// random order with random worker interleaving, the model check, then the
// same catch-up again.
func hwtPropCheck(t *testing.T, u hwtPropUniverse) error {
	h := openHWTHarness(t, t.TempDir(), false)
	defer h.stop()
	var problems []string
	fail := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	// Live delivery, then deletes and edits that reached v2 afterwards.
	liveKeys := map[string]bool{}
	for index, conversation := range u.Conversations {
		if conversation.Live {
			title := fmt.Sprintf("Group %d", index)
			liveKeys[h.liveConversation(t, hwtConversation(conversation.Wire, title, true, conversation.Roster...)).DedupeKey] = true
		}
	}
	for _, message := range u.Messages {
		if message.Delivered {
			wire := u.Conversations[message.Conversation].Wire
			liveKeys[h.liveMessage(t, message.Live.proto(message.ID, wire)).DedupeKey] = true
		}
	}
	h.pump(t)
	for _, message := range u.Messages {
		if !message.Delivered || message.Mutation == "" {
			continue
		}
		stored := h.message(t, u.Conversations[message.Conversation].Wire, message.ID)
		switch message.Mutation {
		case "deleted":
			h.exec(t, `UPDATE messages SET state = 'deleted', updated_at_ms = updated_at_ms + 7 WHERE message_id = ?`, stored.MessageID)
		case "edited":
			h.exec(t, `UPDATE messages SET state = 'edited', body = body || ' (edited)', updated_at_ms = updated_at_ms + 7 WHERE message_id = ?`, stored.MessageID)
		}
	}

	preMessages := h.rowsByKey(t, `SELECT message_id, * FROM messages`)
	preAttachments := h.rowsByKey(t, `SELECT message_id || '/' || ordinal, * FROM message_attachments`)
	preReactions := h.rowsByKey(t, `SELECT message_id || '/' || reactor_key, * FROM reactions`)
	preFences := h.rowsByKey(t, `SELECT message_id, * FROM reaction_snapshot_fences`)
	const conversationColumns = `conversation_id, account_id, remote_conversation_id, kind, title,
		remote_revision, notification_mode, is_favorite, archived_at_ms, metadata_json,
		created_at_ms, updated_at_ms`
	preConversations := h.rowsByKey(t, `SELECT remote_conversation_id, `+conversationColumns+` FROM conversations`)
	preRosters := h.rowsByKey(t, `
		SELECT c.remote_conversation_id || '/' || p.identity_id, p.*
		FROM conversation_participants p JOIN conversations c ON c.conversation_id = p.conversation_id`)
	preRecency := h.rowsByKey(t, `SELECT remote_conversation_id, last_message_at_ms FROM conversations`)
	stored := map[string]bool{}
	for _, message := range u.Messages {
		if message.Delivered {
			stored[message.ID] = true
		}
	}
	if len(preMessages) != len(stored) {
		return fmt.Errorf("live phase stored %d messages, want %d", len(preMessages), len(stored))
	}

	// Model: the first insertable history version of an ID v2 lacks is
	// inserted; everything else is skipped. A thread v2 lacks is created by
	// the first frame that mentions it, with that frame's snapshot title.
	expected := map[int]hwtPropVersion{}
	createdTitle := map[int]string{}
	for _, frame := range u.Frames {
		if !u.Conversations[frame.Conversation].Live {
			if _, seen := createdTitle[frame.Conversation]; !seen {
				createdTitle[frame.Conversation] = frame.Title
			}
		}
		if frame.Message < 0 || stored[u.Messages[frame.Message].ID] {
			continue
		}
		if _, done := expected[frame.Message]; !done && frame.Version.insertable() {
			expected[frame.Message] = frame.Version
		}
	}

	// Catch-up, pass 1.
	before := h.counts()
	records := make([]bridge.RawIngressRecord, 0, len(u.Frames))
	queued := 0
	for _, frame := range u.Frames {
		records = append(records, u.record(t, h, frame))
		queued++
		if frame.PumpAfter || queued >= hwtPropMaxQueued {
			h.pump(t)
			queued = 0
		}
	}
	h.pump(t)
	first := h.counts()

	keys := map[string]bool{}
	for key := range liveKeys {
		keys[key] = true
	}
	wantAppended := uint64(0)
	for _, record := range records {
		if !keys[record.DedupeKey] {
			keys[record.DedupeKey] = true
			wantAppended++
		}
	}
	if got := first.HistoryAppended - before.HistoryAppended; got != wantAppended {
		fail("history_appended = %d, want %d", got, wantAppended)
	}
	if got := first.HistoryDeduped - before.HistoryDeduped; got != uint64(len(records))-wantAppended {
		fail("history_deduped = %d, want %d", got, uint64(len(records))-wantAppended)
	}
	if got := first.HistoryImported - before.HistoryImported; got != uint64(len(expected)) {
		fail("history_imported = %d, want %d", got, len(expected))
	}
	if first.Appended != before.Appended || first.Projected != before.Projected ||
		first.ReactionsOrphaned != before.ReactionsOrphaned || first.RemoteRebinds != 0 {
		fail("live counters moved: before=%+v after=%+v", before, first)
	}

	// I3: every pre-existing row, attachment, reaction and fence is
	// byte-identical (content columns and updated_at included).
	postMessages := h.rowsByKey(t, `SELECT message_id, * FROM messages`)
	for id, row := range preMessages {
		if postMessages[id] != row {
			fail("pre-existing message changed:\n  before %s\n  after  %s", row, postMessages[id])
		}
	}
	newIDs := map[string]bool{}
	for id := range postMessages {
		if _, existed := preMessages[id]; !existed {
			newIDs[id] = true
		}
	}
	for _, check := range []struct {
		name  string
		pre   map[string]string
		query string
	}{
		{"attachment", preAttachments, `SELECT message_id || '/' || ordinal, * FROM message_attachments`},
		{"reaction", preReactions, `SELECT message_id || '/' || reactor_key, * FROM reactions`},
		{"fence", preFences, `SELECT message_id, * FROM reaction_snapshot_fences`},
	} {
		post := h.rowsByKey(t, check.query)
		for key, row := range check.pre {
			if post[key] != row {
				fail("pre-existing %s %s changed:\n  before %s\n  after  %s", check.name, key, row, post[key])
			}
		}
		for key := range post {
			if _, existed := check.pre[key]; existed {
				continue
			}
			if messageID, _, _ := strings.Cut(key, "/"); !newIDs[messageID] {
				fail("new %s %s on a pre-existing message", check.name, key)
			}
		}
	}

	// Each expected ID is present exactly once, with its first history
	// version's content, in the thread its wire ID names.
	if len(postMessages) != len(preMessages)+len(expected) {
		fail("messages = %d, want %d pre-existing + %d inserted", len(postMessages), len(preMessages), len(expected))
	}
	for index, version := range expected {
		message := u.Messages[index]
		wire := u.Conversations[message.Conversation].Wire
		if got := i01QueryInt64(t, h.path, `SELECT COUNT(*) FROM messages WHERE remote_message_id = ?`, message.ID); got != 1 {
			fail("rows for %s = %d, want 1", message.ID, got)
			continue
		}
		row := h.message(t, wire, message.ID)
		if want := v2keys.DeriveID("message", i01AccountID, wire+"\x1f"+message.ID); row.MessageID != want || !newIDs[row.MessageID] {
			fail("%s message ID = %q, want new derived %q", message.ID, row.MessageID, want)
		}
		wantDirection := sqlite.MessageDirectionIncoming
		wantSender := version.Sender
		if version.Outgoing {
			wantDirection = sqlite.MessageDirectionOutgoing
			wantSender = ""
		}
		reply := ""
		if row.ReplyToRemoteID != nil {
			reply = *row.ReplyToRemoteID
		}
		if row.Body != version.Body || row.OccurredAtMS != version.AtMS || row.Direction != wantDirection ||
			row.State != sqlite.MessageStateActive || reply != version.ReplyTo ||
			h.identityCanonical(t, row.SenderIdentityID) != wantSender {
			fail("%s = %+v, want first history version %+v", message.ID, row, version)
		}
		attachments := h.rows(t, `SELECT remote_id, mime FROM message_attachments WHERE message_id = ? ORDER BY ordinal`, row.MessageID)
		wantAttachments := []string(nil)
		if version.MediaID != "" {
			wantAttachments = []string{fmt.Sprintf("remote_id=%q mime=%q", version.MediaID, version.MIME)}
		}
		if !reflect.DeepEqual(attachments, wantAttachments) {
			fail("%s attachments = %v, want %v", message.ID, attachments, wantAttachments)
		}
		gotReactions := map[string]string{}
		for _, reaction := range activeReactionRows(t, h.reactions, row.MessageID) {
			gotReactions[reaction.ReactorCanonical] = reaction.Emoji
		}
		wantReactions := map[string]string{}
		for _, reaction := range version.Reactions {
			wantReactions[reaction.Actor] = reaction.Emoji
		}
		if !reflect.DeepEqual(gotReactions, wantReactions) {
			fail("%s reactions = %v, want %v", message.ID, gotReactions, wantReactions)
		}
	}

	// Threads: live ones keep every column but recency, and their rosters;
	// history-only ones exist iff a frame mentioned them, created as the first
	// such frame's snapshot. Recency is the newest stored message.
	postConversations := h.rowsByKey(t, `SELECT remote_conversation_id, `+conversationColumns+` FROM conversations`)
	postRosters := h.rowsByKey(t, `
		SELECT c.remote_conversation_id || '/' || p.identity_id, p.*
		FROM conversation_participants p JOIN conversations c ON c.conversation_id = p.conversation_id`)
	postRecency := h.rowsByKey(t, `SELECT remote_conversation_id, last_message_at_ms FROM conversations`)
	if len(postConversations) != len(preConversations)+len(createdTitle) {
		fail("conversations = %d, want %d live + %d created", len(postConversations), len(preConversations), len(createdTitle))
	}
	for key, row := range preConversations {
		if postConversations[key] != row {
			fail("live thread %s changed:\n  before %s\n  after  %s", key, row, postConversations[key])
		}
	}
	for key, row := range preRosters {
		if postRosters[key] != row {
			fail("live roster entry %s changed:\n  before %s\n  after  %s", key, row, postRosters[key])
		}
	}
	for index, conversation := range u.Conversations {
		wantRecency := int64(0)
		if conversation.Live {
			wantRecency = h.thread(t, preRecency, conversation.Wire)
		}
		for messageIndex, version := range expected {
			if u.Messages[messageIndex].Conversation == index && version.AtMS > wantRecency {
				wantRecency = version.AtMS
			}
		}
		if conversation.Live {
			if got := h.thread(t, postRecency, conversation.Wire); got != wantRecency {
				fail("%s recency = %d, want %d", conversation.Wire, got, wantRecency)
			}
			continue
		}
		title, mentioned := createdTitle[index]
		created, err := h.store.GetConversationByRemote(i01AccountID, conversation.Wire)
		if !mentioned {
			if !errors.Is(err, sqlite.ErrNotFound) {
				fail("unmentioned %s lookup = %+v, %v; want ErrNotFound", conversation.Wire, created, err)
			}
			continue
		}
		if err != nil {
			fail("history-only %s: %v", conversation.Wire, err)
			continue
		}
		wantPeers := append([]string(nil), conversation.Roster...)
		sort.Strings(wantPeers)
		if created.Kind != sqlite.ConversationKindGroup || created.Title != title ||
			created.LastMessageAtMS != wantRecency ||
			!reflect.DeepEqual(h.peers(t, created.ConversationID), wantPeers) {
			fail("history-only %s = %+v peers %v, want group %q recency %d peers %v",
				conversation.Wire, created, h.peers(t, created.ConversationID), title, wantRecency, wantPeers)
		}
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "\n"))
	}

	// I2: the same catch-up again changes nothing and appends no inbox rows.
	settled := h.state(t)
	queued = 0
	for index, record := range records {
		record.ReceivedAt = h.tick()
		h.appendHistory(t, record)
		queued++
		if u.Frames[index].PumpAfter || queued >= hwtPropMaxQueued {
			h.pump(t)
			queued = 0
		}
	}
	h.pump(t)
	second := h.counts()
	if diff := hwtStateDiff(settled, h.state(t)); diff != "" {
		fail("repeated catch-up changed the store:\n%s", diff)
	}
	if second.HistoryDeduped-first.HistoryDeduped != uint64(len(records)) ||
		second.HistoryAppended != first.HistoryAppended ||
		second.HistoryImported != first.HistoryImported ||
		second.HistoryConversations != first.HistoryConversations {
		fail("repeat counters first=%+v second=%+v", first, second)
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "\n"))
	}
	return nil
}

// thread reads an integer column from a rowsByKey("remote_conversation_id,
// value") map.
func (h *hwtHarness) thread(t *testing.T, byWire map[string]string, wire string) int64 {
	t.Helper()
	row, ok := byWire[wire]
	if !ok {
		t.Fatalf("no conversation row for %q", wire)
	}
	_, value, _ := strings.Cut(row, "last_message_at_ms=")
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		t.Fatalf("parse recency %q: %v", row, err)
	}
	return parsed
}

// For random universes (2-3 threads, one sometimes unknown to v2; up to 8
// message IDs, some delivered live and then deleted or edited in SQL) and a
// random catch-up (new IDs, changed copies of stored IDs, byte-identical
// copies of live frames, stubs, ts=0, standalone snapshots, exact repeats, in
// random order with random worker interleaving): history never changes a
// pre-existing row (I3), inserts each new insertable ID exactly once with its
// first history version, and a repeated catch-up changes nothing (I2).
func TestHistoryWorkerPropertyInsertOnlyAndIdempotent(t *testing.T) {
	property := func(universe hwtPropUniverse) bool {
		if err := hwtPropCheck(t, universe); err != nil {
			t.Logf("counterexample %+v:\n%v", universe, err)
			return false
		}
		return true
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 40, Rand: rand.New(rand.NewSource(20261008))}); err != nil {
		t.Fatal(err)
	}
}
