package ingest

// Tests for the drain's paging and retry backoff, and for the worker's
// change-guarded snapshot writes.
//
// Invariants:
//   - No frame is lost or applied twice: whatever transient failures a frame
//     meets, once they stop every frame is projected exactly once, and a frame
//     is decoded exactly once per attempt the backoff allows.
//   - Frames that never fail are applied in receipt order.
//   - A deferred frame is not read, decoded or applied again before its
//     backoff ends, and is retried when it ends without new traffic.
//   - A conversation snapshot identical to the stored state writes no row; a
//     changed one writes exactly the changed rows.
//   - An empty Google reaction snapshot still advances the fence, so an older
//     snapshot cannot bring its reactions back.

import (
	"context"
	"database/sql"
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

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/messaging"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
	"github.com/rs/zerolog"
)

const wvStartMS int64 = 1_900_000_000_000

// wvHarness drives a worker over scripted frames: each payload names the
// events it decodes to. Storage time and the retry clock are both fake.
type wvHarness struct {
	path     string
	store    *sqlite.Store
	messages *sqlite.MessageRepository
	worker   *Worker
	sink     *Sink
	counters *Counters
	nowMS    atomic.Int64
	retryMS  atomic.Int64
	nextID   atomic.Value

	mu        sync.Mutex
	frames    map[string][]bridge.Event
	decodes   map[string]int
	decodeLog []string
	failing   map[string]int // payload -> attempts still to fail
	current   string
}

func newWVHarness(t *testing.T) *wvHarness {
	t.Helper()
	h := &wvHarness{
		path:    filepath.Join(t.TempDir(), "store.sqlite3"),
		frames:  make(map[string][]bridge.Event),
		decodes: make(map[string]int),
		failing: make(map[string]int),
	}
	h.nowMS.Store(wvStartMS)
	h.retryMS.Store(wvStartMS)
	h.nextID.Store("")
	store, err := sqlite.Open(h.path)
	if err != nil {
		t.Fatalf("sqlite.Open(): %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	h.store = store
	i01SeedAccount(t, store)
	now := func() time.Time { return time.UnixMilli(h.nowMS.Load()) }
	h.messages, err = sqlite.NewMessageRepository(store, now)
	if err != nil {
		t.Fatal(err)
	}
	reactions, err := sqlite.NewReactionRepository(store, now)
	if err != nil {
		t.Fatal(err)
	}
	h.counters = &Counters{}
	h.worker, err = NewWorker(WorkerConfig{
		Store:     store,
		Messages:  h.messages,
		Reactions: reactions,
		Counters:  h.counters,
		Logger:    zerolog.Nop(),
		Now:       now,
		Decoders: []DecoderRegistration{{
			Codec:    i01Codec,
			Platform: bridge.PlatformGoogle,
			Decoder:  i01DecoderFunc(h.decode),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	h.worker.retryNow = func() time.Time { return time.UnixMilli(h.retryMS.Load()) }
	busy := hpTransientError(t)
	h.worker.fault = func(point string) error {
		if point != "conversation-upserted" {
			return nil
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.failing[h.current] > 0 {
			h.failing[h.current]--
			return busy
		}
		return nil
	}
	h.sink, err = NewSink(SinkConfig{
		Messages: h.messages,
		Worker:   h.worker,
		Counters: h.counters,
		IDs: messaging.IDSourceFunc(func() (string, error) {
			return h.nextID.Load().(string), nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *wvHarness) decode(_ context.Context, record bridge.RawIngressRecord) ([]bridge.Event, error) {
	key := string(record.Payload)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.current = key
	h.decodes[key]++
	h.decodeLog = append(h.decodeLog, key)
	events, ok := h.frames[key]
	if !ok {
		return nil, fmt.Errorf("unscripted payload %q", key)
	}
	return events, nil
}

// add appends a frame at the current storage time without draining.
func (h *wvHarness) add(t *testing.T, inboxID, payload string, events []bridge.Event) {
	t.Helper()
	h.mu.Lock()
	h.frames[payload] = events
	h.mu.Unlock()
	h.nextID.Store(inboxID)
	record := i01IngressRecord("dedupe-"+inboxID, []byte(payload))
	record.ReceivedAt = time.UnixMilli(h.nowMS.Load())
	if err := h.sink.AppendIngress(context.Background(), record); err != nil {
		t.Fatalf("AppendIngress(%s): %v", inboxID, err)
	}
	// The worker is not running in these tests; discard the notification.
	select {
	case <-h.worker.work:
	default:
	}
}

func (h *wvHarness) failAttempts(payload string, attempts int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.failing[payload] = attempts
}

func (h *wvHarness) decodeCount(payload string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.decodes[payload]
}

func (h *wvHarness) pending(t *testing.T) int {
	t.Helper()
	records, err := h.messages.Unprocessed(context.Background())
	if err != nil {
		t.Fatalf("Unprocessed(): %v", err)
	}
	return len(records)
}

// wvFrame is a Google-style frame: a snapshot of its own thread plus one
// incoming message, so it can be failed at the conversation write.
func wvFrame(index int) []bridge.Event {
	remote := fmt.Sprintf("thread-%d", index)
	peer := bridge.IdentityRef{Raw: fmt.Sprintf("+1555%07d", index), Name: fmt.Sprintf("Peer %d", index)}
	return []bridge.Event{
		{
			Kind: bridge.EventConversation,
			Conversation: &bridge.ConversationEvent{
				RemoteConversationID: remote,
				Kind:                 "direct",
				Title:                peer.Name,
				Participants:         []bridge.Participant{{Identity: peer, Role: "member", Active: true}},
			},
		},
		{
			Kind: bridge.EventMessage,
			Message: &bridge.MessageEvent{
				RemoteConversationID: remote,
				RemoteMessageID:      fmt.Sprintf("message-%d", index),
				Sender:               peer,
				Direction:            "incoming",
				Body:                 fmt.Sprintf("body %d", index),
				OccurredAt:           time.UnixMilli(wvStartMS - 1000 + int64(index)),
			},
		},
	}
}

func TestDrainPagesBacklogInReceiptOrder(t *testing.T) {
	h := newWVHarness(t)
	rng := rand.New(rand.NewSource(20261009))
	total := 2*drainPageSize + 7
	type position struct {
		receivedAtMS int64
		inboxID      string
		payload      string
	}
	positions := make([]position, 0, total)
	for index := 0; index < total; index++ {
		// Ties on receipt time, broken by inbox ID, which is not append order.
		h.nowMS.Store(wvStartMS + int64(index/3))
		inboxID := fmt.Sprintf("inbox-%06d-%d", rng.Intn(1_000_000), index)
		payload := fmt.Sprintf("frame-%d", index)
		h.add(t, inboxID, payload, wvFrame(index))
		positions = append(positions, position{h.nowMS.Load(), inboxID, payload})
	}
	sort.Slice(positions, func(i, j int) bool {
		if positions[i].receivedAtMS != positions[j].receivedAtMS {
			return positions[i].receivedAtMS < positions[j].receivedAtMS
		}
		return positions[i].inboxID < positions[j].inboxID
	})

	h.worker.drain(context.Background())
	if got := h.pending(t); got != 0 {
		t.Fatalf("unprocessed after one drain = %d, want 0", got)
	}
	if len(h.decodeLog) != total {
		t.Fatalf("decodes = %d, want %d (one per frame)", len(h.decodeLog), total)
	}
	for index, want := range positions {
		if h.decodeLog[index] != want.payload {
			t.Fatalf("decode %d = %s, want %s (receipt order)", index, h.decodeLog[index], want.payload)
		}
	}
	if snapshot := h.counters.Snapshot(i01AccountID); snapshot.Projected != uint64(total) || snapshot.Quarantined != 0 {
		t.Fatalf("counters = %+v, want %d projected and none quarantined", snapshot, total)
	}
}

func TestDrainBacksOffFrameThatExhaustedRetries(t *testing.T) {
	h := newWVHarness(t)
	ctx := context.Background()
	h.add(t, "inbox-1", "stuck", wvFrame(1))
	h.nowMS.Add(1)
	h.add(t, "inbox-2", "second", wvFrame(2))
	// Two rounds of transient retries fail, then the frame applies.
	h.failAttempts("stuck", 2*(maxTransientRetries+1))

	h.worker.drain(ctx)
	attempts := maxTransientRetries + 1
	if got := h.decodeCount("stuck"); got != attempts {
		t.Fatalf("stuck frame decodes after first drain = %d, want %d", got, attempts)
	}
	if got := h.pending(t); got != 1 {
		t.Fatalf("unprocessed after first drain = %d, want only the stuck frame", got)
	}
	if got := h.counters.Snapshot(i01AccountID).Deferred; got != 1 {
		t.Fatalf("deferred = %d, want 1", got)
	}

	// New traffic before the backoff ends drains past the stuck frame
	// without reading it again.
	h.retryMS.Add(int64(retryBaseDelay/time.Millisecond) / 2)
	h.nowMS.Add(1)
	h.add(t, "inbox-3", "third", wvFrame(3))
	h.worker.drain(ctx)
	if got := h.decodeCount("stuck"); got != attempts {
		t.Fatalf("stuck frame decoded during its backoff: %d decodes, want %d", got, attempts)
	}
	if got := h.decodeCount("third"); got != 1 {
		t.Fatalf("third frame decodes = %d, want 1", got)
	}

	// The backoff ends: the frame is retried, fails again, and waits twice
	// as long.
	h.retryMS.Store(wvStartMS + int64(retryBaseDelay/time.Millisecond))
	h.worker.drain(ctx)
	if got := h.decodeCount("stuck"); got != 2*attempts {
		t.Fatalf("stuck frame decodes after its backoff = %d, want %d", got, 2*attempts)
	}
	retry := h.worker.retries["inbox-1"]
	if want := time.UnixMilli(h.retryMS.Load()).Add(2 * retryBaseDelay); retry.failures != 2 || !retry.notBefore.Equal(want) {
		t.Fatalf("backoff after second failure = %+v, want 2 failures until %s", retry, want)
	}
	h.retryMS.Add(int64(retryBaseDelay/time.Millisecond) * 3 / 2)
	h.worker.drain(ctx)
	if got := h.decodeCount("stuck"); got != 2*attempts {
		t.Fatalf("stuck frame decoded during its doubled backoff: %d", got)
	}

	h.retryMS.Add(int64(retryBaseDelay / time.Millisecond))
	h.worker.drain(ctx)
	if got := h.decodeCount("stuck"); got != 2*attempts+1 {
		t.Fatalf("stuck frame decodes after recovery = %d, want %d", got, 2*attempts+1)
	}
	if got := h.pending(t); got != 0 {
		t.Fatalf("unprocessed after recovery = %d, want 0", got)
	}
	if len(h.worker.retries) != 0 {
		t.Fatalf("retries after recovery = %+v, want none", h.worker.retries)
	}
	snapshot := h.counters.Snapshot(i01AccountID)
	if snapshot.Deferred != 2 || snapshot.Quarantined != 0 || snapshot.Projected != 3 {
		t.Fatalf("counters = %+v, want 2 deferred, 0 quarantined, 3 projected", snapshot)
	}
	for index := 1; index <= 3; index++ {
		conversation, err := h.store.GetConversationByRemote(i01AccountID, fmt.Sprintf("thread-%d", index))
		if err != nil {
			t.Fatalf("thread %d: %v", index, err)
		}
		if _, err := h.messages.GetMessageByRemote(ctx, i01AccountID, conversation.ConversationID, fmt.Sprintf("message-%d", index)); err != nil {
			t.Fatalf("message %d: %v", index, err)
		}
	}
}

func TestDrainBackoffIsCappedAndForgetsFramesProcessedElsewhere(t *testing.T) {
	h := newWVHarness(t)
	ctx := context.Background()
	h.add(t, "inbox-1", "stuck", wvFrame(1))
	h.failAttempts("stuck", 1<<20)
	for round := 0; round < 12; round++ {
		h.worker.drain(ctx)
		retry, waiting := h.worker.retries["inbox-1"]
		if !waiting {
			t.Fatalf("round %d: stuck frame has no backoff", round)
		}
		delay := retry.notBefore.Sub(time.UnixMilli(h.retryMS.Load()))
		want := min(retryBaseDelay<<round, retryMaxDelay)
		if delay != want {
			t.Fatalf("round %d: backoff %s, want %s", round, delay, want)
		}
		h.retryMS.Store(retry.notBefore.UnixMilli())
	}

	// Processed by someone else (a replay, another worker): the next full
	// pass drops its backoff.
	h.nowMS.Add(1)
	if err := h.messages.MarkInboxProcessed(ctx, "inbox-1", i01AccountID); err != nil {
		t.Fatal(err)
	}
	h.worker.drain(ctx)
	if len(h.worker.retries) != 0 {
		t.Fatalf("retries = %+v, want the processed frame forgotten", h.worker.retries)
	}
}

// Run wakes for a deferred frame's retry by itself: no new frame arrives.
func TestRunRetriesDeferredFrameWithoutNewTraffic(t *testing.T) {
	h := newWVHarness(t)
	h.worker.retryNow = time.Now
	h.worker.retryBase = 20 * time.Millisecond
	h.worker.retryMax = 40 * time.Millisecond
	h.failAttempts("stuck", maxTransientRetries+1)
	h.add(t, "inbox-1", "stuck", wvFrame(1))
	i01StartWorker(t, h.worker)
	i01WaitFor(t, "deferred frame retried without new traffic", func() bool {
		records, err := h.messages.Unprocessed(context.Background())
		return err == nil && len(records) == 0
	})
	if got := h.decodeCount("stuck"); got != maxTransientRetries+2 {
		t.Fatalf("stuck frame decodes = %d, want %d", got, maxTransientRetries+2)
	}
	if snapshot := h.counters.Snapshot(i01AccountID); snapshot.Deferred != 1 || snapshot.Projected != 1 {
		t.Fatalf("counters = %+v, want 1 deferred and 1 projected", snapshot)
	}
}

func TestDrainNeverLosesOrRepeatsFramesProperty(t *testing.T) {
	property := func(seed int64) bool {
		rng := rand.New(rand.NewSource(seed))
		h := newWVHarness(t)
		ctx := context.Background()
		total := 1 + rng.Intn(30)
		failRounds := make([]int, total)
		appended := 0
		for appended < total || rng.Intn(4) != 0 {
			switch step := rng.Intn(6); {
			case step <= 2 && appended < total:
				h.nowMS.Add(1)
				payload := fmt.Sprintf("frame-%d", appended)
				if rng.Intn(3) == 0 {
					failRounds[appended] = 1 + rng.Intn(3)
					h.failAttempts(payload, failRounds[appended]*(maxTransientRetries+1))
				}
				h.add(t, fmt.Sprintf("inbox-%03d", appended), payload, wvFrame(appended))
				appended++
			case step <= 4:
				h.worker.drain(ctx)
			default:
				h.retryMS.Add(int64(rng.Intn(3000)))
			}
		}
		for round := 0; round < 40 && h.pending(t) > 0; round++ {
			h.retryMS.Add(int64(retryMaxDelay / time.Millisecond))
			h.worker.drain(ctx)
		}
		if got := h.pending(t); got != 0 {
			t.Logf("seed %d: %d frames never processed", seed, got)
			return false
		}
		snapshot := h.counters.Snapshot(i01AccountID)
		if snapshot.Projected != uint64(total) || snapshot.Quarantined != 0 {
			t.Logf("seed %d: counters %+v, want %d projected", seed, snapshot, total)
			return false
		}
		var messages int
		inspector := i01OpenInspector(t, h.path)
		defer inspector.Close()
		if err := inspector.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&messages); err != nil || messages != total {
			t.Logf("seed %d: %d messages (%v), want %d", seed, messages, err, total)
			return false
		}
		lastDecode := make(map[string]int)
		for index, payload := range h.decodeLog {
			lastDecode[payload] = index
		}
		previous := -1
		for index := 0; index < total; index++ {
			payload := fmt.Sprintf("frame-%d", index)
			if got, want := h.decodeCount(payload), failRounds[index]*(maxTransientRetries+1)+1; got != want {
				t.Logf("seed %d: %s decoded %d times, want %d", seed, payload, got, want)
				return false
			}
			if failRounds[index] == 0 {
				if lastDecode[payload] < previous {
					t.Logf("seed %d: %s applied out of receipt order", seed, payload)
					return false
				}
				previous = lastDecode[payload]
			}
		}
		return true
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 15, Rand: rand.New(rand.NewSource(20261015))}); err != nil {
		t.Fatal(err)
	}
}

// installIngestWriteCounter counts row writes per table through triggers in
// the test store, so a test can assert what a frame wrote.
func installIngestWriteCounter(t *testing.T, path string, tables ...string) *sql.DB {
	t.Helper()
	inspector := i01OpenInspector(t, path)
	t.Cleanup(func() { _ = inspector.Close() })
	statements := []string{`CREATE TABLE test_row_writes (table_name TEXT PRIMARY KEY, writes INTEGER NOT NULL)`}
	for _, table := range tables {
		statements = append(statements, fmt.Sprintf(`INSERT INTO test_row_writes VALUES ('%s', 0)`, table))
		for _, event := range []string{"INSERT", "UPDATE", "DELETE"} {
			statements = append(statements, fmt.Sprintf(`
				CREATE TRIGGER test_count_%s_%s AFTER %s ON %s
				BEGIN
					UPDATE test_row_writes SET writes = writes + 1 WHERE table_name = '%s';
				END`, table, strings.ToLower(event), event, table, table))
		}
	}
	for _, statement := range statements {
		if _, err := inspector.Exec(statement); err != nil {
			t.Fatalf("install write counter: %v", err)
		}
	}
	return inspector
}

func ingestRowWrites(t *testing.T, inspector *sql.DB) map[string]int64 {
	t.Helper()
	rows, err := inspector.Query(`SELECT table_name, writes FROM test_row_writes`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	writes := make(map[string]int64)
	for rows.Next() {
		var table string
		var count int64
		if err := rows.Scan(&table, &count); err != nil {
			t.Fatal(err)
		}
		writes[table] = count
	}
	return writes
}

func resetIngestRowWrites(t *testing.T, inspector *sql.DB) {
	t.Helper()
	if _, err := inspector.Exec(`UPDATE test_row_writes SET writes = 0`); err != nil {
		t.Fatal(err)
	}
}

func dumpIngestTable(t *testing.T, inspector *sql.DB, query string) []string {
	t.Helper()
	rows, err := inspector.Query(query)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	columns, _ := rows.Columns()
	var dumped []string
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for index := range values {
			pointers[index] = &values[index]
		}
		if err := rows.Scan(pointers...); err != nil {
			t.Fatal(err)
		}
		dumped = append(dumped, fmt.Sprint(values...))
	}
	return dumped
}

type wvMember struct {
	number string
	name   string
	role   string
}

func wvGroupSnapshot(title string, members []wvMember) []bridge.Event {
	participants := []bridge.Participant{{
		Identity: bridge.IdentityRef{Raw: "+15550009999", Name: "Me", IsSelf: true},
		Role:     "member",
		Active:   true,
	}}
	for _, member := range members {
		participants = append(participants, bridge.Participant{
			Identity: bridge.IdentityRef{Raw: member.number, Name: member.name},
			Role:     member.role,
			Active:   true,
		})
	}
	return []bridge.Event{{
		Kind: bridge.EventConversation,
		Conversation: &bridge.ConversationEvent{
			RemoteConversationID: "group-thread",
			Kind:                 "group",
			Title:                title,
			Participants:         participants,
		},
	}}
}

func wvMembers(count int) []wvMember {
	members := make([]wvMember, 0, count)
	for index := 0; index < count; index++ {
		members = append(members, wvMember{
			number: fmt.Sprintf("+1555100%04d", index),
			name:   fmt.Sprintf("Member %d", index),
			role:   "member",
		})
	}
	return members
}

func TestRepeatedGroupSnapshotWritesNothing(t *testing.T) {
	h := newWVHarness(t)
	ctx := context.Background()
	inspector := installIngestWriteCounter(t, h.path, "identities", "conversations", "conversation_participants", "inbox")
	members := wvMembers(20)
	h.add(t, "inbox-1", "snapshot-1", wvGroupSnapshot("Group", members))
	h.worker.drain(ctx)
	first := ingestRowWrites(t, inspector)
	if first["identities"] != 21 || first["conversations"] != 1 || first["conversation_participants"] != 21 {
		t.Fatalf("first snapshot writes = %v, want 21 identities, 1 conversation, 21 participants", first)
	}
	const (
		identitiesQuery   = `SELECT * FROM identities ORDER BY identity_id`
		conversationQuery = `SELECT * FROM conversations ORDER BY conversation_id`
		participantsQuery = `SELECT * FROM conversation_participants ORDER BY conversation_id, identity_id`
	)
	identities := dumpIngestTable(t, inspector, identitiesQuery)
	conversations := dumpIngestTable(t, inspector, conversationQuery)
	participants := dumpIngestTable(t, inspector, participantsQuery)

	// The same snapshot pushed again, later, as a new frame.
	resetIngestRowWrites(t, inspector)
	h.nowMS.Add(60_000)
	h.add(t, "inbox-2", "snapshot-2", wvGroupSnapshot("Group", members))
	h.worker.drain(ctx)
	if got := ingestRowWrites(t, inspector); got["identities"] != 0 || got["conversations"] != 0 || got["conversation_participants"] != 0 || got["inbox"] != 2 {
		t.Fatalf("repeated snapshot writes = %v, want only the inbox insert and its processed mark", got)
	}
	for name, check := range map[string]struct {
		query string
		want  []string
	}{
		"identities":                {identitiesQuery, identities},
		"conversations":             {conversationQuery, conversations},
		"conversation_participants": {participantsQuery, participants},
	} {
		if got := dumpIngestTable(t, inspector, check.query); strings.Join(got, "\n") != strings.Join(check.want, "\n") {
			t.Fatalf("%s changed under a repeated snapshot", name)
		}
	}
	if h.pending(t) != 0 {
		t.Fatal("repeated snapshot left unprocessed")
	}
}

func TestChangedGroupSnapshotWritesOnlyTheDifference(t *testing.T) {
	h := newWVHarness(t)
	ctx := context.Background()
	inspector := installIngestWriteCounter(t, h.path, "identities", "conversations", "conversation_participants")
	members := wvMembers(20)
	h.add(t, "inbox-1", "snapshot-1", wvGroupSnapshot("Group", members))
	h.worker.drain(ctx)

	// Rename one member (identity and roster row), drop one, add one, and
	// retitle the group.
	changed := append([]wvMember(nil), members...)
	changed[3].name = "Renamed"
	changed = append(changed[:7], changed[8:]...)
	changed = append(changed, wvMember{number: "+15552000000", name: "Newcomer", role: "admin"})
	resetIngestRowWrites(t, inspector)
	h.nowMS.Add(60_000)
	h.add(t, "inbox-2", "snapshot-2", wvGroupSnapshot("Group renamed", changed))
	h.worker.drain(ctx)
	if got := ingestRowWrites(t, inspector); got["identities"] != 2 || got["conversations"] != 1 || got["conversation_participants"] != 3 {
		t.Fatalf("changed snapshot writes = %v, want 2 identities (1 new, 1 renamed), 1 conversation, 3 participants", got)
	}
	conversation, err := h.store.GetConversationByRemote(i01AccountID, "group-thread")
	if err != nil {
		t.Fatal(err)
	}
	if conversation.Title != "Group renamed" || conversation.UpdatedAtMS != h.nowMS.Load() {
		t.Fatalf("conversation = %+v, want the new title at %d", conversation, h.nowMS.Load())
	}
	assertWVRoster(t, h, conversation.ConversationID, changed)

	// A roster-only change still moves the conversation's update time, which
	// the id-space repair's clobber detection lists by.
	resetIngestRowWrites(t, inspector)
	h.nowMS.Add(60_000)
	rosterOnly := changed[1:]
	h.add(t, "inbox-3", "snapshot-3", wvGroupSnapshot("Group renamed", rosterOnly))
	h.worker.drain(ctx)
	if got := ingestRowWrites(t, inspector); got["identities"] != 0 || got["conversations"] != 1 || got["conversation_participants"] != 1 {
		t.Fatalf("roster-only snapshot writes = %v, want 1 participant delete and the conversation touch", got)
	}
	touched, err := h.store.ListConversationsUpdatedSince(i01AccountID, h.nowMS.Load())
	if err != nil {
		t.Fatal(err)
	}
	if len(touched) != 1 || touched[0].ConversationID != conversation.ConversationID {
		t.Fatalf("conversations updated since the roster change = %+v, want the group", touched)
	}
	assertWVRoster(t, h, conversation.ConversationID, rosterOnly)
}

func assertWVRoster(t *testing.T, h *wvHarness, conversationID string, members []wvMember) {
	t.Helper()
	participants, err := h.store.ListParticipants(conversationID)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]wvMember{"+15550009999": {number: "+15550009999", name: "Me", role: "member"}}
	for _, member := range members {
		want[member.number] = member
	}
	if len(participants) != len(want) {
		t.Fatalf("roster has %d rows, want %d", len(participants), len(want))
	}
	for _, participant := range participants {
		identity, err := h.store.GetIdentity(participant.IdentityID)
		if err != nil {
			t.Fatal(err)
		}
		member, ok := want[identity.CanonicalValue]
		if !ok || participant.DisplayName != member.name || string(participant.Role) != member.role || identity.DisplayName != member.name {
			t.Fatalf("roster row %+v (identity %+v) does not match the snapshot", participant, identity)
		}
	}
}

// Google message frames carry their reactions as a full snapshot fenced by
// receipt time. An empty snapshot must still advance the fence: here the
// newer, reaction-free frame lands first, and the older frame carrying a
// reaction (as a frame retried after a transient failure would) must not add
// it.
func TestOlderGoogleReactionSnapshotCannotOverrideNewerEmptyOne(t *testing.T) {
	h := newWVHarness(t)
	ctx := context.Background()
	self := bridge.IdentityRef{Raw: "+15550009999", IsSelf: true}
	frame := func(reacted bool) []bridge.Event {
		events := []bridge.Event{{
			Kind: bridge.EventMessage,
			Message: &bridge.MessageEvent{
				RemoteConversationID: "thread-reactions",
				RemoteMessageID:      "message-reactions",
				Sender:               bridge.IdentityRef{Raw: "+15551230000", Name: "Peer"},
				Direction:            "incoming",
				Body:                 "react to me",
				OccurredAt:           time.UnixMilli(wvStartMS - 5000),
			},
		}}
		if reacted {
			events = append(events, bridge.Event{
				Kind: bridge.EventReaction,
				Reaction: &bridge.ReactionEvent{
					RemoteConversationID:  "thread-reactions",
					TargetRemoteMessageID: "message-reactions",
					Actor:                 self,
					Emoji:                 "👍",
					Action:                bridge.ReactionAdd,
					OccurredAt:            time.UnixMilli(wvStartMS - 4000),
				},
			})
		}
		return events
	}
	activeReactions := func() int {
		inspector := i01OpenInspector(t, h.path)
		defer inspector.Close()
		var count int
		if err := inspector.QueryRow(`SELECT COUNT(*) FROM reactions WHERE state = 'active'`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}

	h.nowMS.Store(wvStartMS + 200)
	h.add(t, "inbox-newer-empty", "newer-empty", frame(false))
	h.worker.drain(ctx)
	h.nowMS.Store(wvStartMS + 100)
	h.add(t, "inbox-older-reacted", "older-reacted", frame(true))
	h.worker.drain(ctx)
	if got := activeReactions(); got != 0 {
		t.Fatalf("active reactions after an older snapshot = %d, want 0 (fenced by the newer empty one)", got)
	}
	h.nowMS.Store(wvStartMS + 300)
	h.add(t, "inbox-reacted", "reacted", frame(true))
	h.worker.drain(ctx)
	if got := activeReactions(); got != 1 {
		t.Fatalf("active reactions after a newer snapshot = %d, want 1", got)
	}
	h.nowMS.Store(wvStartMS + 400)
	h.add(t, "inbox-emptied", "emptied", frame(false))
	h.worker.drain(ctx)
	if got := activeReactions(); got != 0 {
		t.Fatalf("active reactions after a newer empty snapshot = %d, want 0", got)
	}
	if h.pending(t) != 0 {
		t.Fatal("reaction frames left unprocessed")
	}
}

// resolveIdentityFor writes an identity only when the reference changes it:
// the stored row always equals the merge rule applied to every reference so
// far (raw value replaced, a non-empty name replaces the old one, the self
// flag only turns on; history's create-only resolution never rewrites), and
// its update time is the time of the last change.
func TestResolveIdentityWritesOnlyChangesProperty(t *testing.T) {
	type modelIdentity struct {
		raw, name             string
		isSelf                bool
		createdAtMS, updateMS int64
	}
	property := func(seed int64) bool {
		rng := rand.New(rand.NewSource(seed))
		h := newWVHarness(t)
		model := make(map[string]*modelIdentity)
		for step := 0; step < 30; step++ {
			h.nowMS.Add(int64(rng.Intn(3)))
			nowMS := h.nowMS.Load()
			digits := pickWV(rng, "15550000001", "15550000002")
			raw := pickWV(rng, "+"+digits, "+"+digits[:1]+" "+digits[1:4]+"-"+digits[4:7]+"-"+digits[7:], " +"+digits+" ")
			name := pickWV(rng, "", "", "Ann", " Bob ")
			isSelf := rng.Intn(5) == 0
			createOnly := rng.Intn(4) == 0
			got, err := h.worker.resolveIdentityFor(i01AccountID, bridge.PlatformGoogle, bridge.IdentityRef{Raw: raw, Name: name, IsSelf: isSelf}, createOnly)
			if err != nil {
				t.Logf("seed %d step %d: %v", seed, step, err)
				return false
			}
			canonical := "+" + digits
			want := model[canonical]
			trimmedRaw, trimmedName := strings.TrimSpace(raw), strings.TrimSpace(name)
			switch {
			case want == nil:
				want = &modelIdentity{raw: trimmedRaw, name: trimmedName, isSelf: isSelf, createdAtMS: nowMS, updateMS: nowMS}
				model[canonical] = want
			case !createOnly:
				merged := *want
				merged.raw = trimmedRaw
				if trimmedName != "" {
					merged.name = trimmedName
				}
				merged.isSelf = want.isSelf || isSelf
				if merged.raw != want.raw || merged.name != want.name || merged.isSelf != want.isSelf {
					merged.updateMS = nowMS
				}
				*want = merged
			}
			stored, err := h.store.GetIdentityByCanonical(i01AccountID, sqlite.IdentityKind("e164"), canonical)
			if err != nil {
				t.Logf("seed %d step %d: %v", seed, step, err)
				return false
			}
			if stored.RawValue != want.raw || stored.DisplayName != want.name || stored.IsSelf != want.isSelf ||
				stored.CreatedAtMS != want.createdAtMS || stored.UpdatedAtMS != want.updateMS {
				t.Logf("seed %d step %d: stored %+v, model %+v", seed, step, stored, *want)
				return false
			}
			if got.IdentityID != stored.IdentityID || got.RawValue != stored.RawValue ||
				got.DisplayName != stored.DisplayName || got.IsSelf != stored.IsSelf {
				t.Logf("seed %d step %d: returned %+v, stored %+v", seed, step, got, stored)
				return false
			}
		}
		return true
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 15, Rand: rand.New(rand.NewSource(20261016))}); err != nil {
		t.Fatal(err)
	}
}

// refreshConversation writes a conversation snapshot only where it differs:
// after every snapshot the thread's kind, title, revision and roster equal the
// snapshot's, and its update time is the time of the last snapshot that
// changed any of them (its row or its roster).
func TestRefreshConversationWritesOnlyChangesProperty(t *testing.T) {
	type rosterEntry struct {
		name, role string
		active     bool
	}
	property := func(seed int64) bool {
		rng := rand.New(rand.NewSource(seed))
		h := newWVHarness(t)
		pool := []string{"+15550000001", "+15550000002", "+15550000003", "+15550000004"}
		var lastKind, lastTitle, lastRevision string
		var lastRoster map[string]rosterEntry
		var changedAtMS int64
		for step := 0; step < 25; step++ {
			h.nowMS.Add(int64(rng.Intn(3)))
			nowMS := h.nowMS.Load()
			event := bridge.ConversationEvent{
				RemoteConversationID: "whatsapp:thread@g.us",
				Kind:                 pickWV(rng, "", "group", "group", "direct"),
				Title:                pickWV(rng, "T1", "T1", "T2"),
				RemoteRevision:       pickWV(rng, "", "v1", "v1", " v2 "),
			}
			roster := make(map[string]rosterEntry)
			for _, number := range pool {
				if rng.Intn(3) == 0 {
					continue
				}
				entry := rosterEntry{name: pickWV(rng, "", "Ann", "Ann", "Bob"), role: pickWV(rng, "member", "member", "admin"), active: rng.Intn(6) != 0}
				roster[number] = entry
				event.Participants = append(event.Participants, bridge.Participant{
					Identity: bridge.IdentityRef{Raw: number, Name: entry.name},
					Role:     entry.role,
					Active:   entry.active,
				})
			}
			if _, err := h.worker.refreshConversation(i01AccountID, bridge.PlatformWhatsApp, event, true); err != nil {
				t.Logf("seed %d step %d: %v", seed, step, err)
				return false
			}
			kind := lastKind
			switch {
			case event.Kind != "":
				kind = event.Kind
			case kind == "":
				kind = "direct"
			}
			revision := strings.TrimSpace(event.RemoteRevision)
			if step == 0 || kind != lastKind || event.Title != lastTitle || revision != lastRevision || !reflect.DeepEqual(roster, lastRoster) {
				changedAtMS = nowMS
			}
			lastKind, lastTitle, lastRevision, lastRoster = kind, event.Title, revision, roster

			conversation, err := h.store.GetConversationByRemote(i01AccountID, "whatsapp:thread@g.us")
			if err != nil {
				t.Logf("seed %d step %d: %v", seed, step, err)
				return false
			}
			storedRevision := ""
			if conversation.RemoteRevision != nil {
				storedRevision = *conversation.RemoteRevision
			}
			if string(conversation.Kind) != kind || conversation.Title != event.Title || storedRevision != revision || conversation.UpdatedAtMS != changedAtMS {
				t.Logf("seed %d step %d: conversation %+v, want kind %s title %s revision %q updated %d",
					seed, step, conversation, kind, event.Title, revision, changedAtMS)
				return false
			}
			participants, err := h.store.ListParticipants(conversation.ConversationID)
			if err != nil {
				t.Logf("seed %d step %d: %v", seed, step, err)
				return false
			}
			stored := make(map[string]rosterEntry, len(participants))
			for _, participant := range participants {
				identity, err := h.store.GetIdentity(participant.IdentityID)
				if err != nil {
					t.Logf("seed %d step %d: %v", seed, step, err)
					return false
				}
				stored[identity.CanonicalValue] = rosterEntry{name: participant.DisplayName, role: string(participant.Role), active: participant.IsActive}
			}
			if !reflect.DeepEqual(stored, roster) {
				t.Logf("seed %d step %d: roster %+v, want %+v", seed, step, stored, roster)
				return false
			}
		}
		return true
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 15, Rand: rand.New(rand.NewSource(20261017))}); err != nil {
		t.Fatal(err)
	}
}

func pickWV(rng *rand.Rand, values ...string) string {
	return values[rng.Intn(len(values))]
}
