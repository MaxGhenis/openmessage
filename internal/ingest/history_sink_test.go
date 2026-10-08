package ingest

// Sink-level tests for AppendHistoryIngress: history frames are stored exactly
// like live frames but counted under history_appended/history_deduped, share
// the live dedupe-key space in both orders, and are validated identically.
// Reuses the i01 harness from sink_worker_test.go (i01SeedAccount,
// i01NewWorker, i01NewSink, i01IngressRecord, i01QueryInt64,
// i01OpenInspector, i01AccountID, i01TestTime). The worker is deliberately
// never started so the queued work item the sink hands it can be inspected.

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mau.fi/mautrix-gmessages/pkg/libgm"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

type hsinkClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *hsinkClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *hsinkClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type hsinkInboxRow struct {
	inboxID      string
	codec        string
	receivedAtMS int64
	payload      []byte
}

// hsinkNewHarness mirrors i01NewHarness but gives the message repository an
// advancing clock, so a dedupe that bumped the stored receipt time would show.
func hsinkNewHarness(t *testing.T) (*i01Harness, *hsinkClock) {
	t.Helper()
	clock := &hsinkClock{now: i01TestTime}
	path := filepath.Join(t.TempDir(), "store.sqlite3")
	store, err := sqlite.Open(path)
	if err != nil {
		t.Fatalf("sqlite.Open(): %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	i01SeedAccount(t, store)
	messages, err := sqlite.NewMessageRepository(store, clock.Now)
	if err != nil {
		t.Fatalf("sqlite.NewMessageRepository(): %v", err)
	}
	counters := &Counters{}
	decoder := i01DecoderFunc(func(context.Context, bridge.RawIngressRecord) ([]bridge.Event, error) {
		t.Error("worker decoded a frame; these sink tests never start the worker")
		return nil, nil
	})
	worker := i01NewWorker(t, store, messages, counters, decoder, nil)
	sink := i01NewSink(t, messages, worker, counters, "inbox-hsink")
	return &i01Harness{
		path:     path,
		store:    store,
		messages: messages,
		counters: counters,
		worker:   worker,
		sink:     sink,
	}, clock
}

func TestHistorySinkCountsHistoryApartFromLive(t *testing.T) {
	harness, clock := hsinkNewHarness(t)

	// The sink is discoverable as a HistoryIngressSink through the
	// ConnectionSink it is wired as.
	var connection bridge.ConnectionSink = harness.sink
	history, ok := connection.(bridge.HistoryIngressSink)
	if !ok {
		t.Fatal("*Sink does not implement bridge.HistoryIngressSink")
	}

	record := hsinkHistoryRecord(t, hsinkMessage("history-count", "first body"))
	if err := history.AppendHistoryIngress(context.Background(), record); err != nil {
		t.Fatalf("AppendHistoryIngress(new): %v", err)
	}
	hsinkAssertCounts(t, harness, hsinkCounts{historyAppended: 1})
	first := hsinkInbox(t, harness.path, record.DedupeKey)
	if first.inboxID != "inbox-hsink-0001" || first.codec != "google.protobuf.history" ||
		first.receivedAtMS != i01TestTime.UnixMilli() || string(first.payload) != string(record.Payload) {
		t.Fatalf("history inbox row = %+v", first)
	}
	item := hsinkTakeWork(t, harness.worker)
	if item.inboxID != first.inboxID || item.replay || item.record.Codec != GoogleHistoryCodec {
		t.Fatalf("queued work after new history append = %+v, want fresh history item for %q", item, first.inboxID)
	}

	clock.Advance(time.Minute)
	if err := history.AppendHistoryIngress(context.Background(), record); err != nil {
		t.Fatalf("AppendHistoryIngress(replay): %v", err)
	}
	hsinkAssertCounts(t, harness, hsinkCounts{historyAppended: 1, historyDeduped: 1})
	if got := i01QueryInt64(t, harness.path, `SELECT COUNT(*) FROM inbox`); got != 1 {
		t.Fatalf("inbox rows after history replay = %d, want 1", got)
	}
	if again := hsinkInbox(t, harness.path, record.DedupeKey); again.inboxID != first.inboxID ||
		again.receivedAtMS != first.receivedAtMS {
		t.Fatalf("history replay changed the stored row: %+v -> %+v", first, again)
	}
	// A deduplicated history append queues nothing: history is insert-only,
	// so a second pass over the same frame could add nothing.
	hsinkAssertNoWork(t, harness.worker)

	// A second, distinct history frame is a new append; the live counters stay
	// untouched throughout.
	if err := history.AppendHistoryIngress(context.Background(), hsinkHistoryRecord(t, hsinkMessage("history-count", "edited body"))); err != nil {
		t.Fatalf("AppendHistoryIngress(edited): %v", err)
	}
	hsinkAssertCounts(t, harness, hsinkCounts{historyAppended: 2, historyDeduped: 1})
	if got := i01QueryInt64(t, harness.path, `SELECT COUNT(*) FROM inbox`); got != 2 {
		t.Fatalf("inbox rows after distinct history frame = %d, want 2", got)
	}
	if other := harness.counters.Snapshot("some-other-account"); other != (CounterSnapshot{}) {
		t.Fatalf("history counters leaked to another account: %+v", other)
	}
}

func TestHistorySinkLiveThenHistoryDedupesOntoLiveRow(t *testing.T) {
	harness, clock := hsinkNewHarness(t)
	message := hsinkMessage("cross-origin-live-first", "same bytes")

	live := hsinkLiveRecord(t, message)
	if err := harness.sink.AppendIngress(context.Background(), live); err != nil {
		t.Fatalf("AppendIngress(live): %v", err)
	}
	liveRow := hsinkInbox(t, harness.path, live.DedupeKey)
	_ = hsinkTakeWork(t, harness.worker)

	// The history copy carries a conversation snapshot and is_old=true, so its
	// payload differs, but its dedupe key covers only the message proto.
	clock.Advance(time.Hour)
	history := hsinkHistoryRecordWithConversation(t, message)
	if history.DedupeKey != live.DedupeKey || string(history.Payload) == string(live.Payload) {
		t.Fatalf("fixture: history key %q payload-differs=%v, live key %q",
			history.DedupeKey, string(history.Payload) != string(live.Payload), live.DedupeKey)
	}
	if err := harness.sink.AppendHistoryIngress(context.Background(), history); err != nil {
		t.Fatalf("AppendHistoryIngress(after live): %v", err)
	}

	hsinkAssertCounts(t, harness, hsinkCounts{appended: 1, historyDeduped: 1})
	if got := i01QueryInt64(t, harness.path, `SELECT COUNT(*) FROM inbox`); got != 1 {
		t.Fatalf("inbox rows = %d, want the single live row", got)
	}
	after := hsinkInbox(t, harness.path, live.DedupeKey)
	// No receipt-time bump and no codec or payload rewrite: the live row is
	// exactly as the live channel left it (DESIGN decision 2).
	if after.inboxID != liveRow.inboxID || after.codec != GoogleCodec ||
		after.receivedAtMS != liveRow.receivedAtMS || string(after.payload) != string(live.Payload) {
		t.Fatalf("live row after history dedupe = %+v, want unchanged %+v", after, liveRow)
	}
	// No history replay may be queued against the live row: the worker would
	// apply history semantics to it and mark it processed, so a live update
	// still waiting in the inbox would never be applied.
	hsinkAssertNoWork(t, harness.worker)
}

func TestHistorySinkHistoryThenLiveDedupesOntoHistoryRow(t *testing.T) {
	harness, clock := hsinkNewHarness(t)
	message := hsinkMessage("cross-origin-history-first", "same bytes")

	history := hsinkHistoryRecordWithConversation(t, message)
	if err := harness.sink.AppendHistoryIngress(context.Background(), history); err != nil {
		t.Fatalf("AppendHistoryIngress: %v", err)
	}
	historyRow := hsinkInbox(t, harness.path, history.DedupeKey)
	_ = hsinkTakeWork(t, harness.worker)

	clock.Advance(time.Hour)
	live := hsinkLiveRecord(t, message)
	if err := harness.sink.AppendIngress(context.Background(), live); err != nil {
		t.Fatalf("AppendIngress(after history): %v", err)
	}

	// Specified behavior (DESIGN decision 2, shared keys): the later live push
	// counts as a live dedupe and adds no row. Known trade-off recorded in
	// reader-silence-190.md: the stored row keeps the history codec, so a
	// codec-filtered silence monitor does not see this live delivery.
	hsinkAssertCounts(t, harness, hsinkCounts{deduped: 1, historyAppended: 1})
	if got := i01QueryInt64(t, harness.path, `SELECT COUNT(*) FROM inbox`); got != 1 {
		t.Fatalf("inbox rows = %d, want the single history row", got)
	}
	after := hsinkInbox(t, harness.path, history.DedupeKey)
	if after.inboxID != historyRow.inboxID || after.codec != GoogleHistoryCodec ||
		after.receivedAtMS != historyRow.receivedAtMS || string(after.payload) != string(history.Payload) {
		t.Fatalf("history row after live dedupe = %+v, want unchanged %+v", after, historyRow)
	}
	if got := i01QueryInt64(t, harness.path, `SELECT COUNT(*) FROM inbox WHERE codec = ?`, GoogleCodec); got != 0 {
		t.Fatalf("live-codec inbox rows = %d, want 0 (live push collapsed onto the history row)", got)
	}
	item := hsinkTakeWork(t, harness.worker)
	if item.inboxID != historyRow.inboxID || !item.replay || item.record.Codec != GoogleCodec {
		t.Fatalf("queued work = %+v, want live replay against history row %q", item, historyRow.inboxID)
	}
}

func TestHistorySinkValidationAppliesToHistory(t *testing.T) {
	harness, _ := hsinkNewHarness(t)
	valid := hsinkHistoryRecord(t, hsinkMessage("history-validation", "body"))

	tests := []struct {
		name   string
		ctx    context.Context
		mutate func(*bridge.RawIngressRecord)
		want   string
	}{
		{name: "empty account", ctx: context.Background(), mutate: func(r *bridge.RawIngressRecord) { r.AccountID = "" }, want: "account ID is empty"},
		{name: "whitespace account", ctx: context.Background(), mutate: func(r *bridge.RawIngressRecord) { r.AccountID = " \t" }, want: "account ID is empty"},
		{name: "zero received time", ctx: context.Background(), mutate: func(r *bridge.RawIngressRecord) { r.ReceivedAt = time.Time{} }, want: "received time is not positive"},
		{name: "epoch received time", ctx: context.Background(), mutate: func(r *bridge.RawIngressRecord) { r.ReceivedAt = time.UnixMilli(0) }, want: "received time is not positive"},
		{name: "pre-epoch received time", ctx: context.Background(), mutate: func(r *bridge.RawIngressRecord) { r.ReceivedAt = time.UnixMilli(-1) }, want: "received time is not positive"},
		{name: "empty codec", ctx: context.Background(), mutate: func(r *bridge.RawIngressRecord) { r.Codec = "  " }, want: "codec is empty"},
		{name: "whitespace dedupe key", ctx: context.Background(), mutate: func(r *bridge.RawIngressRecord) { r.DedupeKey = "   " }, want: "dedupe key is whitespace"},
		{name: "nil context", ctx: nil, mutate: func(*bridge.RawIngressRecord) {}, want: "context is nil"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			record := valid
			test.mutate(&record)
			for _, entry := range []struct {
				name string
				call func(context.Context, bridge.RawIngressRecord) error
			}{
				{name: "AppendHistoryIngress", call: harness.sink.AppendHistoryIngress},
				{name: "AppendIngress", call: harness.sink.AppendIngress},
			} {
				err := entry.call(test.ctx, record)
				if err == nil || !strings.Contains(err.Error(), test.want) {
					t.Fatalf("%s(%s) error = %v, want %q", entry.name, test.name, err, test.want)
				}
			}
		})
	}

	// An unknown account fails at the inbox foreign key for history too.
	unknown := valid
	unknown.AccountID = "account-not-seeded"
	err := harness.sink.AppendHistoryIngress(context.Background(), unknown)
	if !errors.Is(err, sqlite.ErrOrphanInboxAccount) {
		t.Fatalf("AppendHistoryIngress(unknown account) error = %v, want ErrOrphanInboxAccount", err)
	}
	if snapshot := harness.counters.Snapshot("account-not-seeded"); snapshot.HistoryAppended != 0 || snapshot.HistoryDeduped != 0 {
		t.Fatalf("failed history append was counted: %+v", snapshot)
	}

	hsinkAssertCounts(t, harness, hsinkCounts{})
	if got := i01QueryInt64(t, harness.path, `SELECT COUNT(*) FROM inbox`); got != 0 {
		t.Fatalf("inbox rows after rejected appends = %d, want 0", got)
	}
	select {
	case item := <-harness.worker.work:
		t.Fatalf("rejected append queued work %+v", item)
	default:
	}

	// Control: the unmutated record is accepted.
	if err := harness.sink.AppendHistoryIngress(context.Background(), valid); err != nil {
		t.Fatalf("AppendHistoryIngress(valid control): %v", err)
	}
	hsinkAssertCounts(t, harness, hsinkCounts{historyAppended: 1})
}

type hsinkCounts struct {
	appended, deduped, historyAppended, historyDeduped uint64
}

func hsinkAssertCounts(t *testing.T, harness *i01Harness, want hsinkCounts) {
	t.Helper()
	snapshot := harness.counters.Snapshot(i01AccountID)
	got := hsinkCounts{
		appended:        snapshot.Appended,
		deduped:         snapshot.Deduped,
		historyAppended: snapshot.HistoryAppended,
		historyDeduped:  snapshot.HistoryDeduped,
	}
	if got != want {
		t.Fatalf("counters appended/deduped/history_appended/history_deduped = %+v, want %+v (snapshot %+v)",
			got, want, snapshot)
	}
	// The sink alone never projects or imports anything.
	if snapshot.Projected != 0 || snapshot.HistoryImported != 0 || snapshot.HistoryExisting != 0 ||
		snapshot.HistoryConversations != 0 || snapshot.AppendErrors != 0 {
		t.Fatalf("sink-only test advanced worker/error counters: %+v", snapshot)
	}
}

func hsinkMessage(messageID, body string) *gmproto.Message {
	return &gmproto.Message{
		MessageID:      messageID,
		ConversationID: i01RemoteConversationID,
		Timestamp:      i01TestTime.Add(-time.Minute).UnixMicro(),
		MessageStatus:  &gmproto.MessageStatus{Status: gmproto.MessageStatusType_INCOMING_COMPLETE},
		SenderParticipant: &gmproto.Participant{
			FullName: "Ada",
			ID:       &gmproto.SmallInfo{Number: "+15551234567"},
		},
		MessageInfo: []*gmproto.MessageInfo{{
			Data: &gmproto.MessageInfo_MessageContent{MessageContent: &gmproto.MessageContent{Content: body}},
		}},
	}
}

func hsinkHistoryRecord(t *testing.T, message *gmproto.Message) bridge.RawIngressRecord {
	t.Helper()
	record, err := GoogleHistoryMessageRecord(i01AccountID, 17, i01RemoteConversationID, nil, message, i01TestTime)
	if err != nil {
		t.Fatalf("GoogleHistoryMessageRecord: %v", err)
	}
	return record
}

func hsinkHistoryRecordWithConversation(t *testing.T, message *gmproto.Message) bridge.RawIngressRecord {
	t.Helper()
	record, err := GoogleHistoryMessageRecord(
		i01AccountID, 17, i01RemoteConversationID,
		&gmproto.Conversation{ConversationID: i01RemoteConversationID, Name: "Thread"},
		message, i01TestTime,
	)
	if err != nil {
		t.Fatalf("GoogleHistoryMessageRecord: %v", err)
	}
	return record
}

func hsinkLiveRecord(t *testing.T, message *gmproto.Message) bridge.RawIngressRecord {
	t.Helper()
	record, err := GoogleMessageRecord(i01AccountID, 17, &libgm.WrappedMessage{Message: message}, i01TestTime)
	if err != nil {
		t.Fatalf("GoogleMessageRecord: %v", err)
	}
	return record
}

func hsinkInbox(t *testing.T, path, dedupeKey string) hsinkInboxRow {
	t.Helper()
	database := i01OpenInspector(t, path)
	defer database.Close()
	var row hsinkInboxRow
	if err := database.QueryRow(`
		SELECT inbox_id, codec, received_at_ms, payload
		FROM inbox
		WHERE account_id = ? AND dedupe_key = ?
	`, i01AccountID, dedupeKey).Scan(&row.inboxID, &row.codec, &row.receivedAtMS, &row.payload); err != nil {
		t.Fatalf("read inbox row %q: %v", dedupeKey, err)
	}
	return row
}

func hsinkAssertNoWork(t *testing.T, worker *Worker) {
	t.Helper()
	select {
	case item := <-worker.work:
		t.Fatalf("sink queued %+v, want no work item", item)
	default:
	}
}

func hsinkTakeWork(t *testing.T, worker *Worker) workItem {
	t.Helper()
	select {
	case item := <-worker.work:
		return item
	default:
		t.Fatal("sink queued no work item")
		return workItem{}
	}
}
