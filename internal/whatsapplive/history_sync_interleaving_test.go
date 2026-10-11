package whatsapplive

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"

	"github.com/maxghenis/openmessage/internal/db"
)

// historyStep is one event in TestEveryHistorySyncInterleavingKeepsEveryChunk.
type historyStep byte

const (
	historyNotify      historyStep = 'N' // our phone sends a notification (media and inline alternate)
	historyPass        historyStep = 'P' // a pump pass whose effects all succeed
	historyTransient   historyStep = 'T' // a pass whose downloads fail transiently
	historyExpired     historyStep = 'E' // a pass whose downloads find the server copy gone
	historyGone        historyStep = 'G' // a pass whose downloads find the connection gone
	historyCrashFetch  historyStep = 'K' // the process ends in a pass's first download, and restarts
	historyCrashCommit historyStep = 'M' // the process ends right after a pass's first commit, and restarts
	historyCrashAck    historyStep = 'Q' // the process ends right after a pass's first receipt, and restarts
	historyGeneration  historyStep = 'R' // the observing generation retires, or the next one begins observing
	historyConnection  historyStep = 'D' // the connection drops, or comes back
	// historyLater (random runs only) lets an hour pass, so retries backed
	// off for up to an hour come due.
	historyLater historyStep = 'L'
)

var historySteps = []historyStep{
	historyNotify, historyPass, historyTransient, historyExpired, historyGone,
	historyCrashFetch, historyCrashCommit, historyCrashAck, historyGeneration, historyConnection,
}

// TestEveryHistorySyncInterleavingKeepsEveryChunk checks the history-sync
// queue's invariants exhaustively over every sequence of up to four events
// that delivers a notification,
// with a spool and a queue that take everything and ones that hold nothing
// (spool) or one notification (queue). After every step:
//
//   - a notification is queued whatever became of its own frame, unless the
//     queue is full, and reported handled only if it was queued (whatsmeow
//     redelivers one reported not handled);
//   - every notification reported handled is queued, or its chunk was
//     committed or is held in the spool, or its download failed in a way
//     that lets the pump drop it: nothing reported handled is lost;
//   - no receipt is sent and no server copy deleted before the chunk was
//     committed or held, except the receipt of a dropped notification
//     (checked by the fake effects as they run);
//   - a pass downloads notifications oldest first, each at most once;
//   - every commit of a chunk carries one dedupe key;
//   - the queue stays within its bound.
//
// Then the account connects, a generation that commits observes ingress,
// whatsmeow redelivers what was reported not handled, and the pump drains
// the queue. Every notification has been committed or dropped, each chunk
// committed under one key and acknowledged, the queue and
// the spool are empty, and the legacy store holds every chunk committed and
// never dropped.
func TestEveryHistorySyncInterleavingKeepsEveryChunk(t *testing.T) {
	database := openMemoryDB(t)

	// Every sequence that delivers at least one notification (one that
	// delivers none has nothing to lose). Under the race detector, up to
	// three events: see raceDetectorEnabled.
	maxLength, wantSequences := 4, 3730 // 10+100+1,000+10,000, less the 9+81+729+6,561 without a notification
	if raceDetectorEnabled {
		maxLength, wantSequences = 3, 291
	}
	var sequences [][]historyStep
	var extend func(prefix []historyStep)
	extend = func(prefix []historyStep) {
		if strings.ContainsRune(string(prefix), rune(historyNotify)) {
			sequences = append(sequences, append([]historyStep(nil), prefix...))
		}
		if len(prefix) == maxLength {
			return
		}
		for _, step := range historySteps {
			extend(append(prefix, step))
		}
	}
	extend(nil)

	bounds := []struct{ spool, queue int64 }{{16, 16}, {0, 16}, {16, 1}, {0, 1}}
	checked := 0
	for boundIndex, bound := range bounds {
		store, err := db.New(":memory:")
		if err != nil {
			t.Fatalf("db.New(): %v", err)
		}
		t.Cleanup(func() { _ = store.Close() })
		for index, sequence := range sequences {
			label := fmt.Sprintf("spool %d, queue %d, sequence %s", bound.spool, bound.queue, string(sequence))
			for _, table := range []string{"openmessage_whatsapp_ingress_spool", "openmessage_whatsapp_history_sync_pending"} {
				if _, err := database.Exec(`DROP TABLE IF EXISTS ` + table); err != nil {
					t.Fatalf("reset %s: %v", table, err)
				}
			}
			run := &historyRun{
				f:      newPumpFixtureWith(t, database, store, bound.spool, bound.queue),
				label:  label,
				bound:  bound.queue,
				prefix: fmt.Sprintf("b%d-%d-", boundIndex, index),
			}
			for step, event := range sequence {
				run.apply(step, event, bound.spool, bound.queue)
				run.check()
			}
			run.drain()
			checked++
		}
	}
	if len(sequences) != wantSequences || checked != len(bounds)*len(sequences) {
		t.Fatalf("checked %d interleavings of %d sequences, want %d x %d", checked, len(sequences), len(bounds), wantSequences)
	}
}

// historyRun is one interleaving and what the test knows about it.
type historyRun struct {
	f      *pumpFixture
	label  string
	bound  int64
	prefix string

	delivered []string
	// doomed notifications fail every download transiently, whatever the
	// pass, until they run out of attempts.
	doomed    map[string]bool
	inline    map[string]bool
	handled   map[string]bool
	seqOf     map[string]int64
	connected bool
}

func (r *historyRun) fail(format string, args ...any) {
	r.f.t.Helper()
	r.f.t.Fatalf("%s: %s (effects %v)", r.label, fmt.Sprintf(format, args...), r.f.effects())
}

func (r *historyRun) notify(id string) bool {
	room := int64(len(r.f.queued())) < r.bound
	handled := r.f.notify(id, r.inline[id])
	queued := false
	for _, queuedID := range r.f.queued() {
		queued = queued || queuedID == id
	}
	// The notification is queued whatever became of its own frame, and
	// reported handled only if it was queued.
	if handled && !queued {
		r.fail("notification %s reported handled but not queued", id)
	}
	if !queued && room {
		r.fail("notification %s refused while the queue had room", id)
	}
	if handled {
		r.handled[id] = true
	}
	if queued {
		var seq int64
		if err := r.f.database.QueryRow(
			`SELECT seq FROM openmessage_whatsapp_history_sync_pending WHERE message_id = ?`, id,
		).Scan(&seq); err != nil {
			r.fail("look up %s: %v", id, err)
		}
		r.seqOf[id] = seq
	}
	return handled
}

// pass runs one pass with the given download outcome and checks that it
// downloaded notifications oldest first, each at most once.
func (r *historyRun) pass(outcome error) bool {
	r.f.downloadOutcome = func(id string) error {
		if r.doomed[id] {
			return errFixtureTransient
		}
		return outcome
	}
	r.f.advance(time.Minute)
	before := len(r.f.effects())
	crashed := r.f.pass()
	var last int64
	for _, effect := range r.f.effects()[before:] {
		id, ok := strings.CutPrefix(effect, "download:")
		if !ok {
			continue
		}
		seq := r.seqOf[id]
		if seq <= last {
			r.fail("a pass downloaded %s (seq %d) after seq %d", id, seq, last)
		}
		last = seq
	}
	return crashed
}

func (r *historyRun) apply(step int, event historyStep, spoolFrames, queueRows int64) {
	if r.inline == nil {
		r.inline, r.handled, r.seqOf, r.connected = map[string]bool{}, map[string]bool{}, map[string]int64{}, true
	}
	if r.doomed == nil {
		r.doomed = map[string]bool{}
	}
	switch event {
	case historyNotify:
		id := fmt.Sprintf("%s%d", r.prefix, step)
		r.delivered = append(r.delivered, id)
		r.inline[id] = step%2 == 1
		r.notify(id)
	case historyPass:
		r.pass(nil)
	case historyTransient:
		r.pass(errFixtureTransient)
	case historyExpired:
		r.pass(whatsmeow.ErrMediaDownloadFailedWith404)
	case historyGone:
		r.pass(whatsmeow.ErrNotConnected)
	case historyCrashFetch, historyCrashCommit, historyCrashAck:
		r.f.crash = map[historyStep]string{
			historyCrashFetch:  "download:",
			historyCrashCommit: "commit:",
			historyCrashAck:    "receipt:",
		}[event]
		r.pass(nil)
		// The next run: the same database, a new bridge that connects and
		// whose first generation observes ingress (replaying the spool).
		r.f.crash = ""
		r.f.retire()
		r.f.start(spoolFrames, queueRows)
		r.connected = true
	case historyGeneration:
		if r.f.observing {
			r.f.retire()
		} else {
			r.f.observe(false)
		}
	case historyConnection:
		r.connected = !r.connected
		r.f.setConnected(r.connected)
	case historyLater:
		r.f.advance(time.Hour)
	}
}

func (r *historyRun) check() {
	queued := map[string]bool{}
	for _, id := range r.f.queued() {
		queued[id] = true
	}
	if int64(len(queued)) > r.bound {
		r.fail("queue holds %d notifications, bound %d", len(queued), r.bound)
	}
	r.f.mu.Lock()
	droppable := map[string]bool{}
	for id, ok := range r.f.droppable {
		droppable[id] = ok
	}
	keys := map[string][]string{}
	for id, k := range r.f.keys {
		keys[id] = append([]string(nil), k...)
	}
	r.f.mu.Unlock()
	for _, id := range r.delivered {
		if r.handled[id] && !queued[id] && !r.f.settled(id) && !droppable[id] {
			r.fail("notification %s was reported handled and then lost", id)
		}
		for _, key := range keys[id] {
			if key != keys[id][0] {
				r.fail("chunk %s committed under keys %q and %q", id, keys[id][0], key)
			}
		}
	}
	if r.f.t.Failed() {
		r.fail("an effect broke the ordering invariant")
	}
}

func (r *historyRun) drain() {
	f := r.f
	f.receiptOutcome = func(string) error { return nil }
	if !r.connected {
		r.connected = true
		f.setConnected(true)
	}
	if !f.observing {
		f.observe(false)
	}
	// Each round lets every deferred attempt come due. A doomed notification
	// needs historySyncMaxAttempts rounds to run out, and a redelivery the
	// full queue refused waits for one to leave, so the drain runs until the
	// queue empties; the cap only catches a queue that never does.
	for round := 0; f.bridge.historySync.queue.pending() > 0 || len(r.handled) < len(r.delivered); round++ {
		if round == 100*historySyncMaxAttempts {
			r.fail("the queue never drained: %v", f.queued())
		}
		f.advance(2 * time.Hour)
		r.pass(nil)
		// whatsmeow redelivers what was reported not handled.
		for _, id := range r.delivered {
			if !r.handled[id] {
				r.notify(id)
			}
		}
	}
	r.check()
	if got := f.queued(); len(got) != 0 {
		r.fail("queue still holds %v after draining", got)
	}
	if held := f.bridge.spool.heldFrames(); held != 0 {
		r.fail("spool still holds %d frames after draining", held)
	}
	effects := f.effects()
	for _, id := range r.delivered {
		if !r.handled[id] {
			r.fail("notification %s was never handled", id)
		}
		f.mu.Lock()
		committed, dropped := len(f.keys[id]) > 0, f.droppable[id]
		f.mu.Unlock()
		if !committed && !dropped {
			r.fail("chunk %s was neither committed nor dropped", id)
		}
		if !committed {
			continue
		}
		// The fake receipt checked, when it was sent, that the chunk was
		// committed or held by then.
		acknowledged := false
		for _, effect := range effects {
			acknowledged = acknowledged || effect == "receipt:"+id
		}
		if !acknowledged {
			r.fail("chunk %s was committed but never acknowledged", id)
		}
		// The legacy store takes a chunk after its commit. A process that
		// ended between the two, and whose next download found the server
		// copy gone, leaves the chunk in v2 only.
		if stored, err := f.store.GetMessageByID("whatsapp:chunk-" + id); !dropped && (err != nil || stored == nil) {
			r.fail("legacy store lacks chunk %s (%v)", id, err)
		}
	}
}

// TestRandomHistorySyncRunsKeepEveryChunk runs longer random sequences of the
// same events than the exhaustive test reaches, with a queue of three: hours
// passing, notifications whose downloads always fail until they run out of
// attempts, several crashes and restarts, and a queue that fills and refuses
// notifications until it drains. The same invariants hold after every step
// and after the drain.
func TestRandomHistorySyncRunsKeepEveryChunk(t *testing.T) {
	database := openMemoryDB(t)
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	random := rand.New(rand.NewSource(20261011))
	trials, steps := 300, 40
	if raceDetectorEnabled {
		trials = 40
	}
	exhausted := 0
	for trial := 0; trial < trials; trial++ {
		for _, table := range []string{"openmessage_whatsapp_ingress_spool", "openmessage_whatsapp_history_sync_pending"} {
			if _, err := database.Exec(`DROP TABLE IF EXISTS ` + table); err != nil {
				t.Fatalf("reset %s: %v", table, err)
			}
		}
		spoolFrames := []int64{16, 0}[random.Intn(2)]
		const queueRows = 3
		var sequence []byte
		run := &historyRun{
			f:      newPumpFixtureWith(t, database, store, spoolFrames, queueRows),
			bound:  queueRows,
			prefix: fmt.Sprintf("r%d-", trial),
			doomed: map[string]bool{},
		}
		for step := 0; step < steps; step++ {
			if random.Intn(5) == 0 {
				run.doomed[fmt.Sprintf("%s%d", run.prefix, step)] = true
			}
			// Notifications and failing passes are likelier than the rest,
			// so queues fill and attempts run out.
			var event historyStep
			switch roll := random.Intn(20); {
			case roll < 4:
				event = historyNotify
			case roll < 9:
				event = historyTransient
			case roll < 12:
				event = historyLater
			default:
				event = historySteps[random.Intn(len(historySteps))]
			}
			sequence = append(sequence, byte(event))
			run.label = fmt.Sprintf("trial %d (spool %d), sequence %s", trial, spoolFrames, sequence)
			run.apply(step, event, spoolFrames, queueRows)
			run.check()
		}
		run.drain()
		run.f.mu.Lock()
		for _, failures := range run.f.failures {
			if failures >= historySyncMaxAttempts {
				exhausted++
			}
		}
		run.f.mu.Unlock()
	}
	if exhausted == 0 && !raceDetectorEnabled {
		t.Fatal("no notification ran out of attempts; the generator no longer reaches exhaustion")
	}
}
