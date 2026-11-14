package whatsapplive

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"math/rand"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/whatsmeow"
	waCommon "go.mau.fi/whatsmeow/proto/waCommon"
	waE2E "go.mau.fi/whatsmeow/proto/waE2E"
	waHistorySync "go.mau.fi/whatsmeow/proto/waHistorySync"
	wastore "go.mau.fi/whatsmeow/store"
	watypes "go.mau.fi/whatsmeow/types"
	waevents "go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"github.com/maxghenis/openmessage/internal/db"
)

var spoolTestEpoch = time.Unix(1_775_006_000, 123_456_789)

// openSpoolForTest opens the spool on its own session database file through
// the same DSN and pool shape as initRuntimeState.
func openSpoolForTest(t *testing.T, path string, maxFrames, maxBytes int64) *ingressSpool {
	t.Helper()
	database, err := sql.Open("sqlite", sessionStoreDSN(path))
	if err != nil {
		t.Fatalf("sql.Open(): %v", err)
	}
	database.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = database.Close() })
	spool, err := openIngressSpool(database, maxFrames, maxBytes)
	if err != nil {
		t.Fatalf("openIngressSpool(): %v", err)
	}
	return spool
}

func spoolTestFrame(key string, payload string) IngressFrame {
	return IngressFrame{DedupeKey: key, ReceivedAt: spoolTestEpoch, Payload: []byte(payload)}
}

// heldKeys lists the spool's frames oldest first, read back from the table.
func heldKeys(t *testing.T, spool *ingressSpool) []string {
	t.Helper()
	seqs, err := spool.sequence()
	if err != nil {
		t.Fatalf("sequence(): %v", err)
	}
	keys := []string{}
	for _, seq := range seqs {
		held, ok, err := spool.load(seq)
		if err != nil || !ok {
			t.Fatalf("load(%d) = %v, %v", seq, ok, err)
		}
		keys = append(keys, held.frame.DedupeKey)
	}
	return keys
}

func TestIngressSpoolHoldsReleasesAndSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "whatsapp-session.db")
	spool := openSpoolForTest(t, path, 16, 1<<20)

	frames := []IngressFrame{
		spoolTestFrame("hist:00000001", `{"kind":"history_sync"}`),
		spoolTestFrame("rcpt:00000002", `{"kind":"receipt"}`),
		spoolTestFrame("msg:3EB0:00000003", `{"kind":"message"}`),
	}
	frames[1].ReceivedAt = spoolTestEpoch.Add(time.Nanosecond)
	for index, frame := range frames {
		if err := spool.hold(fmt.Sprintf("kind-%d", index), frame); err != nil {
			t.Fatalf("hold(%s): %v", frame.DedupeKey, err)
		}
	}
	// A frame whose key is already held is the same inbox row: not stored
	// again, whatever its payload.
	if err := spool.hold("receipt", spoolTestFrame("rcpt:00000002", "different bytes")); err != nil {
		t.Fatalf("hold(duplicate key): %v", err)
	}
	if got := heldKeys(t, spool); !reflect.DeepEqual(got, []string{"hist:00000001", "rcpt:00000002", "msg:3EB0:00000003"}) {
		t.Fatalf("held keys = %v", got)
	}
	seqs, _ := spool.sequence()
	if err := spool.release(seqs[0]); err != nil {
		t.Fatalf("release(): %v", err)
	}
	if err := spool.release(seqs[0]); err != nil {
		t.Fatalf("release() again: %v", err)
	}
	if spool.heldFrames() != 2 {
		t.Fatalf("heldFrames() = %d, want 2", spool.heldFrames())
	}

	reopened := openSpoolForTest(t, path, 16, 1<<20)
	if reopened.heldFrames() != 2 {
		t.Fatalf("heldFrames() after reopen = %d, want 2", reopened.heldFrames())
	}
	seqs, _ = reopened.sequence()
	for index, seq := range seqs {
		held, ok, err := reopened.load(seq)
		if err != nil || !ok {
			t.Fatalf("load(%d) = %v, %v", seq, ok, err)
		}
		want := frames[index+1]
		if held.frame.DedupeKey != want.DedupeKey || !bytes.Equal(held.frame.Payload, want.Payload) ||
			!held.frame.ReceivedAt.Equal(want.ReceivedAt) || held.kind != fmt.Sprintf("kind-%d", index+1) {
			t.Fatalf("reloaded frame %d = %+v (kind %s), want %+v", index, held.frame, held.kind, want)
		}
	}
}

func TestIngressSpoolRefusesFramesWithoutIdentity(t *testing.T) {
	spool := openSpoolForTest(t, filepath.Join(t.TempDir(), "whatsapp-session.db"), 16, 1<<20)
	if err := spool.hold("message", IngressFrame{DedupeKey: "  ", ReceivedAt: spoolTestEpoch}); err == nil {
		t.Fatal("hold() accepted a frame with no dedupe key")
	}
	if err := spool.hold("message", IngressFrame{DedupeKey: "msg:x:1"}); err == nil {
		t.Fatal("hold() accepted a frame with no receive time")
	}
	var nilSpool *ingressSpool
	if err := nilSpool.hold("message", spoolTestFrame("msg:x:1", "")); !errors.Is(err, errIngressSpoolUnavailable) {
		t.Fatalf("nil spool hold() = %v, want errIngressSpoolUnavailable", err)
	}
	if nilSpool.heldFrames() != 0 {
		t.Fatal("nil spool reports held frames")
	}
}

// TestIngressSpoolMatchesModel runs random hold/release/reopen sequences (200,
// or 40 under -race) against a model of the spool: an ordered list of held
// keys and sizes. After every step the spool holds exactly the model's frames
// in the model's order, its count matches the table, and it stays within both
// bounds; hold succeeds exactly when the key is already held or the frame
// fits.
func TestIngressSpoolMatchesModel(t *testing.T) {
	const maxFrames, maxBytes = 4, 12
	random := rand.New(rand.NewSource(20261011))
	trials := 200
	if raceDetectorEnabled {
		trials = 40
	}
	for trial := 0; trial < trials; trial++ {
		path := filepath.Join(t.TempDir(), fmt.Sprintf("spool-%d.db", trial))
		spool := openSpoolForTest(t, path, maxFrames, maxBytes)
		type entry struct {
			key  string
			size int
		}
		var model []entry
		modelBytes := func() int {
			total := 0
			for _, e := range model {
				total += e.size
			}
			return total
		}
		for step := 0; step < 30; step++ {
			switch op := random.Intn(10); {
			case op < 6:
				key := fmt.Sprintf("k%d", random.Intn(8))
				size := random.Intn(7)
				err := spool.hold("message", spoolTestFrame(key, string(bytes.Repeat([]byte{'x'}, size))))
				already := false
				for _, e := range model {
					already = already || e.key == key
				}
				fits := len(model) < maxFrames && modelBytes()+size <= maxBytes
				switch {
				case already:
					if err != nil {
						t.Fatalf("trial %d step %d: hold(held key %s) = %v, want nil", trial, step, key, err)
					}
				case fits:
					if err != nil {
						t.Fatalf("trial %d step %d: hold(%s, %d bytes) = %v, want nil", trial, step, key, size, err)
					}
					model = append(model, entry{key: key, size: size})
				default:
					if !errors.Is(err, errIngressSpoolFull) {
						t.Fatalf("trial %d step %d: hold(%s, %d bytes) over the bounds = %v, want errIngressSpoolFull", trial, step, key, size, err)
					}
				}
			case op < 9:
				if len(model) == 0 {
					continue
				}
				index := random.Intn(len(model))
				seqs, err := spool.sequence()
				if err != nil {
					t.Fatalf("sequence(): %v", err)
				}
				if err := spool.release(seqs[index]); err != nil {
					t.Fatalf("release(): %v", err)
				}
				model = append(model[:index], model[index+1:]...)
			default:
				spool = openSpoolForTest(t, path, maxFrames, maxBytes)
			}

			want := []string{}
			for _, e := range model {
				want = append(want, e.key)
			}
			if got := heldKeys(t, spool); !reflect.DeepEqual(got, want) {
				t.Fatalf("trial %d step %d: held keys = %v, want %v", trial, step, got, want)
			}
			var count, total int64
			if err := spool.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(LENGTH(payload)), 0) FROM openmessage_whatsapp_ingress_spool`).Scan(&count, &total); err != nil {
				t.Fatalf("count spool: %v", err)
			}
			if spool.heldFrames() != count || count > maxFrames || total > maxBytes || int(total) != modelBytes() {
				t.Fatalf("trial %d step %d: heldFrames %d, table %d frames / %d bytes, model %d bytes, bounds %d / %d",
					trial, step, spool.heldFrames(), count, total, modelBytes(), maxFrames, maxBytes)
			}
		}
	}
}

// spoolTestBridge is a bridge with a real spool on a temporary session
// database and a controllable clock, set up like TestHandledStatusTable's.
func spoolTestBridge(t *testing.T, maxFrames int64) (*Bridge, *db.Store, *time.Time) {
	t.Helper()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	own := watypes.NewJID("15550000000", watypes.DefaultUserServer)
	spool := openSpoolForTest(t, filepath.Join(t.TempDir(), "whatsapp-session.db"), maxFrames, ingressSpoolMaxBytes)
	now := spoolTestEpoch
	spool.now = func() time.Time { return now }
	bridge := &Bridge{store: store, client: &whatsmeow.Client{Store: &wastore.Device{ID: &own}}, spool: spool}
	return bridge, store, &now
}

func spoolTestEvent(kind string, id string) any {
	chat := watypes.NewJID("15551234567", watypes.DefaultUserServer)
	switch kind {
	case "message":
		return &waevents.Message{
			Info: watypes.MessageInfo{
				MessageSource: watypes.MessageSource{Chat: chat, Sender: chat},
				ID:            watypes.MessageID(id),
				Timestamp:     time.UnixMilli(1775006000000),
			},
			Message: &waE2E.Message{Conversation: proto.String("spool " + id)},
		}
	case "receipt":
		return &waevents.Receipt{
			MessageSource: watypes.MessageSource{Chat: chat, Sender: chat},
			MessageIDs:    []watypes.MessageID{watypes.MessageID(id)},
			Timestamp:     time.UnixMilli(1775006000000),
			Type:          watypes.ReceiptTypeRead,
		}
	case "history_sync":
		return &waevents.HistorySync{Data: &waHistorySync.HistorySync{
			SyncType: waHistorySync.HistorySync_RECENT.Enum(),
			Conversations: []*waHistorySync.Conversation{{
				ID: proto.String(id + "@s.whatsapp.net"),
			}},
		}}
	case "chat_presence":
		return &waevents.ChatPresence{MessageSource: watypes.MessageSource{Chat: chat, Sender: chat}}
	}
	panic("unknown event kind " + kind)
}

// TestHandledStatusTableWithSpool extends TestHandledStatusTable (which runs
// without a spool, as a bridge whose runtime database is closed does) with a
// spool that takes frames and one that is full. The rule: a frame a durable
// observer owns but no generation commits is held; a message is reported
// handled iff it was committed or held; receipts and history chunks are
// always reported handled (whatsmeow ignores the status for them). A
// legacy-only (non-durable) observer never holds anything. The legacy store
// takes every message on its first delivery.
func TestHandledStatusTableWithSpool(t *testing.T) {
	outcomes := []ingressObserverOutcome{observerNeverUsed, observerCommits, observerRejects, observerPanics, observerRemoved}
	kinds := []string{"message", "receipt", "history_sync", "chat_presence"}
	for _, durable := range []bool{true, false} {
		for _, full := range []bool{false, true} {
			for _, outcome := range outcomes {
				for _, kind := range kinds {
					if outcome == observerNeverUsed && !durable {
						continue // durability is a property of an observer
					}
					name := fmt.Sprintf("durable=%v/spool full=%v/%s/%s", durable, full, outcome, kind)
					t.Run(name, func(t *testing.T) {
						maxFrames := int64(16)
						if full {
							maxFrames = 0
						}
						bridge, store, _ := spoolTestBridge(t, maxFrames)
						observed := 0
						switch outcome {
						case observerCommits:
							bridge.ObserveIngress(func(IngressFrame) error { observed++; return nil }, durable)
						case observerRejects:
							bridge.ObserveIngress(func(IngressFrame) error { observed++; return errors.New("stale generation") }, durable)
						case observerPanics:
							bridge.ObserveIngress(func(IngressFrame) error { observed++; panic("observer bug") }, durable)
						case observerRemoved:
							bridge.ObserveIngress(func(IngressFrame) error { observed++; return nil }, durable)()
						}

						handled := bridge.handleClientEvent(nil, 0, spoolTestEvent(kind, "status-table"))

						captured := kind != "chat_presence"
						wantObserved := 0
						if captured && (outcome == observerCommits || outcome == observerRejects || outcome == observerPanics) {
							wantObserved = 1
						}
						if observed != wantObserved {
							t.Fatalf("observer saw %d frames, want %d", observed, wantObserved)
						}
						missed := captured && durable && (outcome == observerRejects || outcome == observerRemoved)
						wantHeld := int64(0)
						if missed && !full {
							wantHeld = 1
						}
						if got := bridge.spool.heldFrames(); got != wantHeld {
							t.Fatalf("spool holds %d frames, want %d", got, wantHeld)
						}
						wantHandled := !(kind == "message" && missed && full)
						if handled != wantHandled {
							t.Fatalf("handled = %v, want %v", handled, wantHandled)
						}
						if kind != "message" {
							return
						}
						stored, err := store.GetMessageByID("whatsapp:status-table")
						if err != nil {
							t.Fatalf("GetMessageByID(): %v", err)
						}
						if stored == nil {
							t.Fatal("legacy store lacks the message after its first delivery")
						}
					})
				}
			}
		}
	}
}

// TestLegacyOnlyObserverNeitherHoldsNorReplays: on a legacy-only install (no
// v2 sink) the observer commits nothing, so a frame it refuses is not held,
// and frames held under an earlier durable observer stay held rather than
// being handed to an observer that would drop them.
func TestLegacyOnlyObserverNeitherHoldsNorReplays(t *testing.T) {
	bridge, _, _ := spoolTestBridge(t, 16)
	bridge.ObserveIngress(func(IngressFrame) error { return nil }, true)()
	if _, err := bridge.commitIngress("receipt", spoolTestFrame("rcpt:held", "held")); err != nil {
		t.Fatalf("commitIngress in a durable gap: %v", err)
	}
	legacy := &replayRecorder{reject: func(IngressFrame) error { return errors.New("no v2 sink") }}
	bridge.ObserveIngress(legacy.observe, false)
	if held, err := bridge.commitIngress("receipt", spoolTestFrame("rcpt:legacy", "legacy")); err != nil || held {
		t.Fatalf("commitIngress to a legacy-only observer = held %v, %v; want reported committed (dropped)", held, err)
	}
	if got := heldKeys(t, bridge.spool); !reflect.DeepEqual(got, []string{"rcpt:held"}) {
		t.Fatalf("held = %v, want only the frame from the durable gap", got)
	}
	durable := &replayRecorder{}
	bridge.ObserveIngress(durable.observe, true)
	if got := durable.keys(); !reflect.DeepEqual(got, []string{"rcpt:held"}) {
		t.Fatalf("durable observer replayed %v, want the held frame", got)
	}
}

type replayRecorder struct {
	mu     sync.Mutex
	frames []IngressFrame
	reject func(IngressFrame) error
}

func (r *replayRecorder) observe(frame IngressFrame) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.reject != nil {
		if err := r.reject(frame); err != nil {
			return err
		}
	}
	r.frames = append(r.frames, frame)
	return nil
}

func (r *replayRecorder) keys() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	keys := []string{}
	for _, frame := range r.frames {
		keys = append(keys, frame.DedupeKey)
	}
	return keys
}

func (r *replayRecorder) replayedFlags() []bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	flags := []bool{}
	for _, frame := range r.frames {
		flags = append(flags, frame.Replayed)
	}
	return flags
}

// TestReplayIsOrderedResumableAndRateLimited holds three frames in a gap and
// replays them: oldest first, as Replayed frames, skipping a frame the
// observer refuses without blocking the ones behind it. The refused frame is
// retried when a later commit on the same generation comes at least
// ingressSpoolRetryInterval after the replay, and by the next observer.
func TestReplayIsOrderedResumableAndRateLimited(t *testing.T) {
	bridge, _, now := spoolTestBridge(t, 16)
	bridge.ObserveIngress(func(IngressFrame) error { return nil }, true)() // an adapter owns ingress; no generation observes
	for _, key := range []string{"one", "two", "three"} {
		if _, err := bridge.commitIngress("receipt", spoolTestFrame("rcpt:"+key, key)); err != nil {
			t.Fatalf("commitIngress(%s) in the gap: %v", key, err)
		}
	}
	if bridge.spool.heldFrames() != 3 {
		t.Fatalf("held = %d, want 3", bridge.spool.heldFrames())
	}

	refused := false
	recorder := &replayRecorder{reject: func(frame IngressFrame) error {
		if frame.DedupeKey == "rcpt:two" && !refused {
			refused = true
			return errors.New("append failed once")
		}
		return nil
	}}
	bridge.ObserveIngress(recorder.observe, true)
	if got := recorder.keys(); !reflect.DeepEqual(got, []string{"rcpt:one", "rcpt:three"}) {
		t.Fatalf("replayed = %v, want one and three in order", got)
	}
	if got := heldKeys(t, bridge.spool); !reflect.DeepEqual(got, []string{"rcpt:two"}) {
		t.Fatalf("held after replay = %v, want only two", got)
	}

	// A commit inside the retry interval does not replay.
	*now = now.Add(ingressSpoolRetryInterval - time.Nanosecond)
	if _, err := bridge.commitIngress("receipt", spoolTestFrame("rcpt:four", "four")); err != nil {
		t.Fatalf("commitIngress(four): %v", err)
	}
	if got := recorder.keys(); !reflect.DeepEqual(got, []string{"rcpt:one", "rcpt:three", "rcpt:four"}) {
		t.Fatalf("commits = %v, want no retry inside the interval", got)
	}
	// One after it does, and the retried frame is marked Replayed.
	*now = now.Add(time.Nanosecond)
	if _, err := bridge.commitIngress("receipt", spoolTestFrame("rcpt:five", "five")); err != nil {
		t.Fatalf("commitIngress(five): %v", err)
	}
	if got := recorder.keys(); !reflect.DeepEqual(got, []string{"rcpt:one", "rcpt:three", "rcpt:four", "rcpt:five", "rcpt:two"}) {
		t.Fatalf("commits = %v, want two retried after five", got)
	}
	if got := recorder.replayedFlags(); !reflect.DeepEqual(got, []bool{true, true, false, false, true}) {
		t.Fatalf("Replayed flags = %v", got)
	}
	if bridge.spool.heldFrames() != 0 {
		t.Fatalf("held = %d after the retry, want 0", bridge.spool.heldFrames())
	}

	// A new observer finds nothing to replay.
	next := &replayRecorder{}
	bridge.ObserveIngress(next.observe, true)
	if got := next.keys(); len(got) != 0 {
		t.Fatalf("an empty spool replayed %v", got)
	}
}

// TestFrameHeldAfterANewerObserverReplayedIsReplayedAtOnce covers the race
// between a retiring generation and its successor: the retiring generation
// refuses a frame after the next one installed its observer and replayed the
// spool. The frame is held after that replay, so the bridge replays it to the
// new observer at once instead of leaving it for the generation after.
func TestFrameHeldAfterANewerObserverReplayedIsReplayedAtOnce(t *testing.T) {
	bridge, _, _ := spoolTestBridge(t, 16)
	next := &replayRecorder{}
	bridge.ObserveIngress(func(IngressFrame) error {
		bridge.ObserveIngress(next.observe, true)
		return errors.New("generation no longer admits ingress")
	}, true)
	handled := bridge.handleClientEvent(nil, 0, spoolTestEvent("message", "raced"))
	if !handled {
		t.Fatal("a held message was reported not handled")
	}
	if got := next.keys(); len(got) != 1 || got[0][:len("msg:raced:")] != "msg:raced:" {
		t.Fatalf("new observer received %v, want the raced message", got)
	}
	if got := next.replayedFlags(); !reflect.DeepEqual(got, []bool{true}) {
		t.Fatalf("Replayed flags = %v, want [true]", got)
	}
	if bridge.spool.heldFrames() != 0 {
		t.Fatalf("held = %d, want 0", bridge.spool.heldFrames())
	}
}

// TestHeldFramesSurviveARestart holds a message in one bridge process and
// replays it in the next: the spool is in the session database, so a frame
// held before the app quit reaches the first generation of the next run.
func TestHeldFramesSurviveARestart(t *testing.T) {
	dir := t.TempDir()
	sessionPath := filepath.Join(dir, "whatsapp-session.db")
	store, err := db.New(filepath.Join(dir, "messages.db"))
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	first, err := New(sessionPath, store, zerolog.Nop(), Callbacks{})
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	first.ObserveIngress(func(IngressFrame) error { return nil }, true)()
	if !first.handleClientEvent(nil, 0, spoolTestEvent("message", "restart")) {
		t.Fatal("a held message was reported not handled")
	}
	if got := first.ingressSpool().heldFrames(); got != 1 {
		t.Fatalf("held = %d, want 1", got)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close(): %v", err)
	}

	second, err := New(sessionPath, store, zerolog.Nop(), Callbacks{})
	if err != nil {
		t.Fatalf("New() after restart: %v", err)
	}
	t.Cleanup(func() { _ = second.Close() })
	recorder := &replayRecorder{}
	second.ObserveIngress(recorder.observe, true)
	if got := recorder.keys(); len(got) != 1 || got[0][:len("msg:restart:")] != "msg:restart:" {
		t.Fatalf("replayed after restart = %v, want the held message", got)
	}
	if got := second.ingressSpool().heldFrames(); got != 0 {
		t.Fatalf("held after replay = %d, want 0", got)
	}
}

// TestRedeliveryOfAHeldMessageDoesNotUndoLegacyEdits: a held message is in
// the legacy store ahead of v2, as one left for redelivery is. If whatsmeow
// redelivers it anyway (its clear of the plaintext failed), the redelivery
// must not write the original body back over an edit applied in between,
// whether the spool holds it again or a generation commits it.
func TestRedeliveryOfAHeldMessageDoesNotUndoLegacyEdits(t *testing.T) {
	chat := watypes.NewJID("15551234567", watypes.DefaultUserServer)
	original := statusTestMessage(chat, "held-original", &waE2E.Message{Conversation: proto.String("original text")})
	edit := statusTestMessage(chat, "held-edit", &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
		Type:          waE2E.ProtocolMessage_MESSAGE_EDIT.Enum(),
		Key:           &waCommon.MessageKey{ID: proto.String("held-original")},
		EditedMessage: &waE2E.Message{Conversation: proto.String("edited text")},
	}})

	bridge, store, _ := spoolTestBridge(t, 16)
	bridge.ObserveIngress(func(IngressFrame) error { return nil }, true)() // durable, now gone
	if !bridge.handleClientEvent(nil, 0, original) {
		t.Fatal("a held message was reported not handled")
	}
	bridge.handleClientEvent(nil, 0, edit)

	if !bridge.handleClientEvent(nil, 0, original) { // redelivery, held again
		t.Fatal("the held redelivery was reported not handled")
	}
	recorder := &replayRecorder{}
	bridge.ObserveIngress(recorder.observe, true)
	if !bridge.handleClientEvent(nil, 0, original) { // redelivery, committed
		t.Fatal("the committed redelivery was reported not handled")
	}
	if stored, err := store.GetMessageByID("whatsapp:held-original"); err != nil || stored == nil || stored.Body != "edited text" {
		t.Fatalf("legacy row after the redeliveries = %+v, %v; want the edit kept", stored, err)
	}
	keys := recorder.keys()
	if len(keys) < 2 || keys[0] != keys[len(keys)-1] {
		t.Fatalf("committed keys = %v, want the replayed frame and the redelivery under one key", keys)
	}
}
