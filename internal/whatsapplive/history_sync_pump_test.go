package whatsapplive

import (
	"bytes"
	"compress/zlib"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waCommon"
	waE2E "go.mau.fi/whatsmeow/proto/waE2E"
	waHistorySync "go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	wastore "go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	watypes "go.mau.fi/whatsmeow/types"
	waevents "go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"

	"github.com/maxghenis/openmessage/internal/db"
)

// pumpFixture is one process's bridge with a real ingress spool and
// history-sync queue on a session database that outlives it (the disk), an
// ingress observer the test controls (the generation), and fake WhatsApp
// effects. The effects check the pump's ordering invariant as they run: a
// notification's receipt and its server copy's deletion come only after its
// chunk was committed or held, except the receipt of a dropped notification.
type pumpFixture struct {
	t        *testing.T
	database *sql.DB
	store    *db.Store
	bridge   *Bridge
	own      watypes.JID

	mu  sync.Mutex
	now time.Time
	// log is every effect in order: "download:A", "commit:A", "hold:A",
	// "receipt:A", "delete:A".
	log []string
	// keys lists, per notification, the dedupe keys its chunk committed under.
	keys map[string][]string
	// dropReasons records, per notification, a download outcome after which
	// the pump may drop it (a permanent failure, or the attempt that
	// exhausts its retries).
	droppable map[string]bool
	failures  map[string]int // counted download failures per notification
	observing bool           // a generation observes ingress
	refusing  bool           // the observing generation refuses frames
	removeObs func()

	// download decides each download's outcome.
	downloadOutcome func(id string) error
	receiptOutcome  func(id string) error
	// crash, when set, ends the pass goroutine at the first effect it
	// prefixes (after the effect took place), the way the process ending
	// there would.
	crash string
}

var errFixtureTransient = errors.New("connection reset by peer")

func newPumpFixture(t *testing.T, database *sql.DB, spoolFrames, queueRows int64) *pumpFixture {
	t.Helper()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return newPumpFixtureWith(t, database, store, spoolFrames, queueRows)
}

func newPumpFixtureWith(t *testing.T, database *sql.DB, store *db.Store, spoolFrames, queueRows int64) *pumpFixture {
	t.Helper()
	f := &pumpFixture{
		t:               t,
		database:        database,
		store:           store,
		own:             watypes.NewJID("15550000000", watypes.DefaultUserServer),
		now:             spoolTestEpoch,
		keys:            map[string][]string{},
		droppable:       map[string]bool{},
		failures:        map[string]int{},
		downloadOutcome: func(string) error { return nil },
		receiptOutcome:  func(string) error { return nil },
	}
	f.start(spoolFrames, queueRows)
	return f
}

// start opens this process's bridge on the fixture's database: it reopens
// the spool and the queue, connects, and lets a generation observe ingress.
func (f *pumpFixture) start(spoolFrames, queueRows int64) {
	f.t.Helper()
	spool, err := openIngressSpool(f.database, spoolFrames, ingressSpoolMaxBytes)
	if err != nil {
		f.t.Fatalf("openIngressSpool(): %v", err)
	}
	spool.now = f.clock
	queue, err := openHistorySyncQueue(f.database, queueRows, historySyncQueueMaxBytes)
	if err != nil {
		f.t.Fatalf("openHistorySyncQueue(): %v", err)
	}
	pump := newHistorySyncPump(queue)
	pump.now = f.clock
	pump.download = f.download
	pump.sendReceipt = f.sendReceipt
	pump.deleteMedia = f.deleteMedia
	f.bridge = &Bridge{
		store:       f.store,
		client:      &whatsmeow.Client{Store: &wastore.Device{ID: &f.own}},
		spool:       spool,
		historySync: pump,
		connected:   true,
	}
	f.observe(false)
}

func (f *pumpFixture) clock() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *pumpFixture) advance(d time.Duration) {
	f.mu.Lock()
	f.now = f.now.Add(d)
	f.mu.Unlock()
}

// observe starts a generation (it replays the spool); refusing makes it
// refuse every frame, as a retiring one does.
func (f *pumpFixture) observe(refusing bool) {
	f.mu.Lock()
	f.observing = true
	f.refusing = refusing
	f.mu.Unlock()
	f.removeObs = f.bridge.ObserveIngress(f.observer, true)
}

// retire ends the observing generation; frames go to the spool.
func (f *pumpFixture) retire() {
	if f.removeObs != nil {
		f.removeObs()
		f.removeObs = nil
	}
	f.mu.Lock()
	f.observing = false
	f.mu.Unlock()
}

func (f *pumpFixture) setConnected(connected bool) {
	f.bridge.mu.Lock()
	f.bridge.connected = connected
	f.bridge.mu.Unlock()
}

// pumpChunk is the chunk the fake download returns for notification id: one
// message whose id names the notification.
func pumpChunk(id string) *waHistorySync.HistorySync {
	return &waHistorySync.HistorySync{
		SyncType: waHistorySync.HistorySync_RECENT.Enum(),
		Conversations: []*waHistorySync.Conversation{{
			ID: proto.String("15551234567@s.whatsapp.net"),
			Messages: []*waHistorySync.HistorySyncMsg{{
				Message: &waWeb.WebMessageInfo{
					Key: &waCommon.MessageKey{
						RemoteJID: proto.String("15551234567@s.whatsapp.net"),
						FromMe:    proto.Bool(false),
						ID:        proto.String("chunk-" + id),
					},
					Message:          &waE2E.Message{Conversation: proto.String("history " + id)},
					MessageTimestamp: proto.Uint64(1_775_006_000),
				},
			}},
		}},
	}
}

// chunkIDs reads the notification ids out of a history-sync frame.
func chunkIDs(t *testing.T, payload []byte) []string {
	t.Helper()
	var envelope whatsappHistorySyncIngressEnvelope
	if err := json.Unmarshal(payload, &envelope); err != nil {
		t.Fatalf("decode history frame: %v", err)
	}
	if envelope.Kind != "history_sync" {
		return nil
	}
	var chunk waHistorySync.HistorySync
	if err := proto.Unmarshal(envelope.ProtoB64, &chunk); err != nil {
		t.Fatalf("decode history chunk: %v", err)
	}
	var ids []string
	for _, conversation := range chunk.GetConversations() {
		for _, item := range conversation.GetMessages() {
			ids = append(ids, strings.TrimPrefix(item.GetMessage().GetKey().GetID(), "chunk-"))
		}
	}
	return ids
}

func (f *pumpFixture) record(event string) {
	f.mu.Lock()
	f.log = append(f.log, event)
	crash := f.crash != "" && strings.HasPrefix(event, f.crash)
	if crash {
		f.crash = ""
	}
	f.mu.Unlock()
	if crash {
		runtime.Goexit()
	}
}

func (f *pumpFixture) observer(frame IngressFrame) error {
	f.mu.Lock()
	refusing := f.refusing
	f.mu.Unlock()
	if refusing {
		return errors.New("generation no longer admits ingress")
	}
	for _, id := range chunkIDs(f.t, frame.Payload) {
		if !frame.Replayed {
			f.t.Errorf("history chunk %s committed as connection activity", id)
		}
		f.mu.Lock()
		f.keys[id] = append(f.keys[id], frame.DedupeKey)
		f.mu.Unlock()
		f.record("commit:" + id)
	}
	return nil
}

func (f *pumpFixture) download(_ context.Context, _ *whatsmeow.Client, notification *waE2E.HistorySyncNotification) (*waHistorySync.HistorySync, error) {
	id := notification.GetOriginalMessageID()
	f.record("download:" + id)
	err := f.downloadOutcome(id)
	if err == nil {
		return pumpChunk(id), nil
	}
	f.mu.Lock()
	if !historySyncConnectionGone(context.Background(), err) {
		f.failures[id]++
		if historySyncDownloadPermanent(err) || f.failures[id] >= historySyncMaxAttempts {
			f.droppable[id] = true
		}
	}
	f.mu.Unlock()
	return nil, err
}

// settled reports whether notification id's chunk was committed or is held.
func (f *pumpFixture) settled(id string) bool {
	f.mu.Lock()
	committed := len(f.keys[id]) > 0
	f.mu.Unlock()
	return committed || f.spoolHolds(id)
}

func (f *pumpFixture) spoolHolds(id string) bool {
	seqs, err := f.bridge.spool.sequence()
	if err != nil {
		f.t.Fatalf("spool sequence(): %v", err)
	}
	for _, seq := range seqs {
		held, ok, err := f.bridge.spool.load(seq)
		if err != nil || !ok {
			continue
		}
		for _, heldID := range chunkIDs(f.t, held.frame.Payload) {
			if heldID == id {
				return true
			}
		}
	}
	return false
}

func (f *pumpFixture) sendReceipt(_ context.Context, _ *whatsmeow.Client, messageID watypes.MessageID) error {
	id := string(messageID)
	f.mu.Lock()
	droppable := f.droppable[id]
	f.mu.Unlock()
	if !f.settled(id) && !droppable {
		f.t.Errorf("receipt for %s sent before its chunk was committed or held", id)
	}
	if err := f.receiptOutcome(id); err != nil {
		return err
	}
	f.record("receipt:" + id)
	return nil
}

func (f *pumpFixture) deleteMedia(_ context.Context, _ *whatsmeow.Client, notification *waE2E.HistorySyncNotification) error {
	id := notification.GetOriginalMessageID()
	if !f.settled(id) {
		f.t.Errorf("server copy of %s deleted before its chunk was committed or held", id)
	}
	f.record("delete:" + id)
	return nil
}

// notify delivers our phone's notification for id through the bridge's
// handler and returns whether the bridge reported it handled.
func (f *pumpFixture) notify(id string, inline bool) bool {
	notification := &waE2E.HistorySyncNotification{
		SyncType:          waE2E.HistorySyncType_RECENT.Enum(),
		OriginalMessageID: proto.String(id),
	}
	if inline {
		notification.InitialHistBootstrapInlinePayload = []byte("inline " + id)
	} else {
		notification.DirectPath = proto.String("/v/t62.history/" + id)
	}
	message := &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
		Type:                    waE2E.ProtocolMessage_HISTORY_SYNC_NOTIFICATION.Enum(),
		HistorySyncNotification: notification,
	}}
	return f.bridge.handleClientEvent(nil, 0, &waevents.Message{
		Info: watypes.MessageInfo{
			MessageSource: watypes.MessageSource{Chat: f.own, Sender: f.own, IsFromMe: true},
			ID:            watypes.MessageID(id),
			Timestamp:     time.UnixMilli(1775006000000),
		},
		Message:    message,
		RawMessage: message,
	})
}

// pass runs one pump pass on its own goroutine and reports whether it ended
// by crashing.
func (f *pumpFixture) pass() (crashed bool) {
	done := make(chan bool)
	go func() {
		finished := false
		defer func() { done <- !finished }()
		f.bridge.pumpHistorySync(context.Background())
		finished = true
	}()
	return <-done
}

func (f *pumpFixture) queued() []string {
	f.t.Helper()
	return queuedIDs(f.t, f.bridge.historySync.queue)
}

func (f *pumpFixture) effects() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.log...)
}

func TestHistorySyncPumpCommitsThenAcknowledges(t *testing.T) {
	f := newPumpFixture(t, openMemoryDB(t), 16, 16)
	if !f.notify("media", false) || !f.notify("inline", true) {
		t.Fatal("a queued notification was reported not handled")
	}
	if got := f.queued(); !reflect.DeepEqual(got, []string{"media", "inline"}) {
		t.Fatalf("queued = %v", got)
	}
	if f.pass() {
		t.Fatal("pass crashed")
	}
	want := []string{
		"download:media", "commit:media", "receipt:media", "delete:media",
		// An inline chunk has no server copy to delete.
		"download:inline", "commit:inline", "receipt:inline",
	}
	if got := f.effects(); !reflect.DeepEqual(got, want) {
		t.Fatalf("effects = %v, want %v", got, want)
	}
	if got := f.queued(); len(got) != 0 {
		t.Fatalf("queue still holds %v", got)
	}
	for _, id := range []string{"media", "inline"} {
		if stored, err := f.store.GetMessageByID("whatsapp:chunk-" + id); err != nil || stored == nil {
			t.Fatalf("legacy store lacks %s's chunk (%v)", id, err)
		}
	}
}

func openMemoryDB(t *testing.T) *sql.DB {
	t.Helper()
	database, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open(): %v", err)
	}
	// One connection keeps the in-memory database alive: it is the disk that
	// outlives each simulated process.
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	database.SetConnMaxLifetime(0)
	t.Cleanup(func() { _ = database.Close() })
	return database
}

// TestHistorySyncPumpFailureClasses checks what one failed step does to a
// notification: whether it stays queued, how many attempts count, whether it
// is dropped (with the receipt whatsmeow's default handling would have sent),
// and whether the pass goes on to the next notification.
func TestHistorySyncPumpFailureClasses(t *testing.T) {
	cases := []struct {
		name        string
		download    error
		receipt     error
		wantQueued  bool
		wantAttempt int
		wantEffects []string // for notification "a"; "b" is queued behind it
		wantBNext   bool     // the pass went on to "b"
	}{
		{
			name:        "transient download failure is retried later",
			download:    errFixtureTransient,
			wantQueued:  true,
			wantAttempt: 1,
			wantEffects: []string{"download:a"},
			wantBNext:   true,
		},
		{
			name:        "expired server copy is dropped with its receipt",
			download:    whatsmeow.ErrMediaDownloadFailedWith404,
			wantEffects: []string{"download:a", "receipt:a"},
			wantBNext:   true,
		},
		{
			name:        "deleted server copy is dropped with its receipt",
			download:    whatsmeow.ErrMediaDownloadFailedWith410,
			wantEffects: []string{"download:a", "receipt:a"},
			wantBNext:   true,
		},
		{
			name:        "notification naming no chunk is dropped",
			download:    whatsmeow.ErrNoURLPresent,
			wantEffects: []string{"download:a", "receipt:a"},
			wantBNext:   true,
		},
		{
			name:        "unparseable chunk is dropped",
			download:    fmt.Errorf("%w: failed to decompress: unexpected EOF", errHistorySyncUnreadable),
			wantEffects: []string{"download:a", "receipt:a"},
			wantBNext:   true,
		},
		{
			name:        "lost connection counts no attempt and stops the pass",
			download:    fmt.Errorf("failed to refresh media connections: %w", whatsmeow.ErrIQDisconnected),
			wantQueued:  true,
			wantEffects: []string{"download:a"},
		},
		{
			name:        "not connected counts no attempt and stops the pass",
			download:    whatsmeow.ErrNotConnected,
			wantQueued:  true,
			wantEffects: []string{"download:a"},
		},
		{
			name:        "receipt failure keeps the committed notification",
			receipt:     errFixtureTransient,
			wantQueued:  true,
			wantAttempt: 1,
			wantEffects: []string{"download:a", "commit:a"},
			wantBNext:   true,
		},
		{
			name:        "receipt on a lost connection stops the pass",
			receipt:     whatsmeow.ErrNotConnected,
			wantQueued:  true,
			wantEffects: []string{"download:a", "commit:a"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newPumpFixture(t, openMemoryDB(t), 16, 16)
			f.downloadOutcome = func(id string) error {
				if id == "a" {
					return tc.download
				}
				return nil
			}
			f.receiptOutcome = func(id string) error {
				if id == "a" {
					return tc.receipt
				}
				return nil
			}
			f.notify("a", false)
			f.notify("b", false)
			f.pass()

			var aEffects []string
			bTouched := false
			for _, effect := range f.effects() {
				if strings.HasSuffix(effect, ":a") {
					aEffects = append(aEffects, effect)
				} else {
					bTouched = true
				}
			}
			if !reflect.DeepEqual(aEffects, tc.wantEffects) {
				t.Fatalf("effects on a = %v, want %v", aEffects, tc.wantEffects)
			}
			if bTouched != tc.wantBNext {
				t.Fatalf("pass reached b = %v, want %v (effects %v)", bTouched, tc.wantBNext, f.effects())
			}
			queued := f.queued()
			if (len(queued) > 0 && queued[0] == "a") != tc.wantQueued {
				t.Fatalf("a queued = %v, want %v", queued, tc.wantQueued)
			}
			if !tc.wantQueued {
				return
			}
			seqs, _ := f.bridge.historySync.queue.due(queueTestFarFuture)
			row, _, _ := f.bridge.historySync.queue.load(seqs[0])
			if row.attempts != tc.wantAttempt {
				t.Fatalf("a attempts = %d, want %d", row.attempts, tc.wantAttempt)
			}
			if tc.receipt != nil && !row.committed {
				t.Fatal("a committed chunk whose receipt failed is not marked committed")
			}
			// A committed notification is not downloaded again.
			f.receiptOutcome = func(string) error { return nil }
			f.downloadOutcome = func(string) error { return nil }
			f.advance(time.Hour)
			f.pass()
			if got := f.queued(); len(got) != 0 {
				t.Fatalf("queue holds %v after a clean pass", got)
			}
			downloads := 0
			for _, effect := range f.effects() {
				if effect == "download:a" {
					downloads++
				}
			}
			if wantDownloads := map[bool]int{true: 1, false: 2}[tc.receipt != nil]; downloads != wantDownloads {
				t.Fatalf("a downloaded %d times, want %d (effects %v)", downloads, wantDownloads, f.effects())
			}
		})
	}
}

// TestHistorySyncPumpRetriesWithBackoffThenDrops fails one notification's
// download every time: it is retried 30 seconds, then 1, 2, 4, 8, 16 and 32
// minutes, then hourly after each failure, and dropped (with its receipt) on
// the tenth. Passes before an attempt is due leave it alone.
func TestHistorySyncPumpRetriesWithBackoffThenDrops(t *testing.T) {
	f := newPumpFixture(t, openMemoryDB(t), 16, 16)
	f.downloadOutcome = func(string) error { return errFixtureTransient }
	f.notify("a", false)
	wantDelays := []time.Duration{
		30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute,
		16 * time.Minute, 32 * time.Minute, time.Hour, time.Hour,
	}
	for attempt := 1; attempt <= historySyncMaxAttempts; attempt++ {
		f.pass()
		if attempt == historySyncMaxAttempts {
			break
		}
		next, ok, err := f.bridge.historySync.queue.nextAttempt(f.clock())
		if err != nil || !ok {
			t.Fatalf("attempt %d: nextAttempt() = %v, %v", attempt, ok, err)
		}
		if delay := next.Sub(f.clock()); delay != wantDelays[attempt-1] {
			t.Fatalf("attempt %d: next attempt in %v, want %v", attempt, delay, wantDelays[attempt-1])
		}
		f.advance(wantDelays[attempt-1] - time.Nanosecond)
		f.pass() // not due yet
		f.advance(time.Nanosecond)
	}
	downloads, receipts := 0, 0
	for _, effect := range f.effects() {
		switch effect {
		case "download:a":
			downloads++
		case "receipt:a":
			receipts++
		}
	}
	if downloads != historySyncMaxAttempts || receipts != 1 {
		t.Fatalf("downloads %d, receipts %d; want %d and 1", downloads, receipts, historySyncMaxAttempts)
	}
	if got := f.queued(); len(got) != 0 {
		t.Fatalf("queue still holds %v after the last attempt", got)
	}
}

// TestHistorySyncPumpWaitsUnderBackPressure: while v2 refuses frames and the
// spool is full, a downloaded chunk is neither committed nor held. Its
// notification stays queued with no attempt counted and no receipt sent. The
// legacy store takes the chunk on the first attempt, as it takes a message on
// its first delivery, and the later attempts and the eventual commit do not
// write it again: a revoke applied in between survives (legacyAhead).
func TestHistorySyncPumpWaitsUnderBackPressure(t *testing.T) {
	f := newPumpFixture(t, openMemoryDB(t), 0, 16)
	f.notify("a", false)
	f.retire()
	f.observe(true)
	f.pass()
	if stored, _ := f.store.GetMessageByID("whatsapp:chunk-a"); stored == nil {
		t.Fatal("legacy store lacks the chunk after its first attempt")
	}
	if err := f.store.DeleteMessageByID("whatsapp:chunk-a"); err != nil {
		t.Fatalf("revoke the legacy row: %v", err)
	}
	for round := 0; round < historySyncMaxAttempts+2; round++ {
		f.advance(time.Hour)
		f.pass()
	}
	seqs, _ := f.bridge.historySync.queue.due(queueTestFarFuture)
	if len(seqs) != 1 {
		t.Fatalf("queue holds %d notifications, want 1", len(seqs))
	}
	row, _, _ := f.bridge.historySync.queue.load(seqs[0])
	if row.attempts != 0 || row.committed {
		t.Fatalf("attempts %d committed %v under back-pressure, want 0 and false", row.attempts, row.committed)
	}
	for _, effect := range f.effects() {
		if !strings.HasPrefix(effect, "download:") {
			t.Fatalf("effect %s under back-pressure", effect)
		}
	}

	f.retire()
	f.observe(false)
	f.advance(time.Hour)
	f.pass()
	if got := f.queued(); len(got) != 0 {
		t.Fatalf("queue holds %v once v2 takes frames", got)
	}
	if stored, _ := f.store.GetMessageByID("whatsapp:chunk-a"); stored != nil {
		t.Fatal("a retry rewrote the legacy row the revoke deleted")
	}
}

// TestHistorySyncPumpWaitsForAConnection: nothing is downloaded, committed or
// acknowledged while the account is not connected.
func TestHistorySyncPumpWaitsForAConnection(t *testing.T) {
	f := newPumpFixture(t, openMemoryDB(t), 16, 16)
	f.notify("a", true)
	f.setConnected(false)
	f.pass()
	if got := f.effects(); len(got) != 0 {
		t.Fatalf("effects while disconnected: %v", got)
	}
	f.setConnected(true)
	f.pass()
	if got := f.effects(); !reflect.DeepEqual(got, []string{"download:a", "commit:a", "receipt:a"}) {
		t.Fatalf("effects once connected = %v", got)
	}
}

// TestHistorySyncPumpDropsAnotherAccountsNotification: after the account is
// unpaired and another one paired, a notification the first account received
// is removed without a download or a receipt from the second.
func TestHistorySyncPumpDropsAnotherAccountsNotification(t *testing.T) {
	f := newPumpFixture(t, openMemoryDB(t), 16, 16)
	f.notify("a", false)
	other := watypes.NewJID("15559999999", watypes.DefaultUserServer)
	f.bridge.mu.Lock()
	f.bridge.client = &whatsmeow.Client{Store: &wastore.Device{ID: &other}}
	f.bridge.mu.Unlock()
	f.notify("b", false)
	f.pass()
	if got := f.effects(); !reflect.DeepEqual(got, []string{"download:b", "commit:b", "receipt:b", "delete:b"}) {
		t.Fatalf("effects = %v, want only b's", got)
	}
	if got := f.queued(); len(got) != 0 {
		t.Fatalf("queue holds %v", got)
	}
}

// TestHistorySyncQueueFullLeavesTheNotificationForRedelivery: a notification
// the queue cannot take is reported not handled (whatsmeow keeps its
// plaintext and the server redelivers it), and the redelivery is queued once
// there is room.
func TestHistorySyncQueueFullLeavesTheNotificationForRedelivery(t *testing.T) {
	f := newPumpFixture(t, openMemoryDB(t), 16, 1)
	f.setConnected(false)
	if !f.notify("a", false) {
		t.Fatal("first notification not handled")
	}
	if f.notify("b", false) {
		t.Fatal("a notification the full queue refused was reported handled")
	}
	if got := f.queued(); !reflect.DeepEqual(got, []string{"a"}) {
		t.Fatalf("queued = %v, want only a", got)
	}
	f.setConnected(true)
	f.pass()
	if !f.notify("b", false) {
		t.Fatal("redelivered notification not handled once the queue drained")
	}
	f.pass()
	if got := f.effects(); !reflect.DeepEqual(got, []string{
		"download:a", "commit:a", "receipt:a", "delete:a",
		"download:b", "commit:b", "receipt:b", "delete:b",
	}) {
		t.Fatalf("effects = %v", got)
	}
}

// TestHistorySyncPumpStopsWithoutCountingAnAttempt: Close cancels a pass
// mid-download; the notification keeps its attempts, and Close returns.
func TestHistorySyncPumpStopsWithoutCountingAnAttempt(t *testing.T) {
	f := newPumpFixture(t, openMemoryDB(t), 16, 16)
	pump := f.bridge.historySync
	started := make(chan struct{})
	pump.download = func(ctx context.Context, _ *whatsmeow.Client, _ *waE2E.HistorySyncNotification) (*waHistorySync.HistorySync, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	f.notify("a", false)
	f.bridge.startHistorySyncPump()
	<-started
	stopped := make(chan struct{})
	go func() {
		f.bridge.stopHistorySyncPump()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("stopping the pump hung on a download")
	}
	seqs, _ := pump.queue.due(queueTestFarFuture)
	row, _, _ := pump.queue.load(seqs[0])
	if row.attempts != 0 || row.committed {
		t.Fatalf("attempts %d committed %v after a cancelled download, want 0 and false", row.attempts, row.committed)
	}
}

// TestDownloadHistorySyncChunkClassifiesFailures runs the production download
// against a real whatsmeow client: an inline chunk parses, an inline chunk
// that does not decompress is permanent, and a notification with neither an
// inline chunk nor a direct path is permanent.
func TestDownloadHistorySyncChunkClassifiesFailures(t *testing.T) {
	own := watypes.NewJID("15550000000", watypes.DefaultUserServer)
	database := openSessionStoreForTest(t)
	device := database.NewDevice()
	device.ID = &own
	cli := whatsmeow.NewClient(device, nil)

	raw, err := proto.Marshal(pumpChunk("inline"))
	if err != nil {
		t.Fatalf("marshal chunk: %v", err)
	}
	var compressed bytes.Buffer
	writer := zlib.NewWriter(&compressed)
	_, _ = writer.Write(raw)
	_ = writer.Close()

	chunk, err := downloadHistorySyncChunk(context.Background(), cli, &waE2E.HistorySyncNotification{
		InitialHistBootstrapInlinePayload: compressed.Bytes(),
	})
	if err != nil || len(chunk.GetConversations()) != 1 {
		t.Fatalf("inline chunk = %v, %v", chunk, err)
	}
	_, err = downloadHistorySyncChunk(context.Background(), cli, &waE2E.HistorySyncNotification{
		InitialHistBootstrapInlinePayload: []byte("not zlib"),
	})
	if !historySyncDownloadPermanent(err) || !errors.Is(err, errHistorySyncUnreadable) {
		t.Fatalf("corrupt inline chunk = %v, want a permanent errHistorySyncUnreadable", err)
	}
	_, err = downloadHistorySyncChunk(context.Background(), cli, &waE2E.HistorySyncNotification{})
	if !historySyncDownloadPermanent(err) || !errors.Is(err, whatsmeow.ErrNoURLPresent) {
		t.Fatalf("notification naming no chunk = %v, want a permanent ErrNoURLPresent", err)
	}
}

func TestHistorySyncNotificationMirrorsWhatsmeow(t *testing.T) {
	own := watypes.NewJID("15550000000", watypes.DefaultUserServer)
	notification := &waE2E.HistorySyncNotification{SyncType: waE2E.HistorySyncType_RECENT.Enum()}
	protocol := &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{HistorySyncNotification: notification}}
	event := func(fromMe bool, raw *waE2E.Message) *waevents.Message {
		return &waevents.Message{
			Info:       watypes.MessageInfo{MessageSource: watypes.MessageSource{Chat: own, Sender: own, IsFromMe: fromMe}},
			RawMessage: raw,
		}
	}
	cases := []struct {
		name string
		evt  *waevents.Message
		want bool
	}{
		{"from our phone", event(true, protocol), true},
		{"in a device-sent envelope", event(true, &waE2E.Message{DeviceSentMessage: &waE2E.DeviceSentMessage{Message: protocol}}), true},
		// whatsmeow handles history sync only from our own account.
		{"from someone else", event(false, protocol), false},
		// processProtocolParts unwraps only the device-sent envelope.
		{"in an ephemeral envelope", event(true, &waE2E.Message{EphemeralMessage: &waE2E.FutureProofMessage{Message: protocol}}), false},
		{"no notification", event(true, &waE2E.Message{Conversation: proto.String("hi")}), false},
		{"nil event", nil, false},
	}
	for _, tc := range cases {
		if got := historySyncNotification(tc.evt) != nil; got != tc.want {
			t.Errorf("%s: notification found = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func openSessionStoreForTest(t *testing.T) *sqlstore.Container {
	t.Helper()
	container, err := sqlstore.New(context.Background(), "sqlite", sessionStoreDSN(filepath.Join(t.TempDir(), "whatsapp-session.db")), waLog.Noop)
	if err != nil {
		t.Fatalf("sqlstore.New(): %v", err)
	}
	t.Cleanup(func() { _ = container.Close() })
	return container
}
