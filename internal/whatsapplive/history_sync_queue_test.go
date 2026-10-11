package whatsapplive

import (
	"database/sql"
	"errors"
	"fmt"
	"math/rand"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	waE2E "go.mau.fi/whatsmeow/proto/waE2E"
	watypes "go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

// queueTestFarFuture is after every attempt the tests schedule, and still
// within UnixNano's range.
var queueTestFarFuture = time.Unix(1<<32, 0)

// openHistorySyncQueueForTest opens the queue on its own session database
// file through the same DSN and pool shape as initRuntimeState.
func openHistorySyncQueueForTest(t *testing.T, path string, maxRows, maxBytes int64) *historySyncQueue {
	t.Helper()
	database, err := sql.Open("sqlite", sessionStoreDSN(path))
	if err != nil {
		t.Fatalf("sql.Open(): %v", err)
	}
	database.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = database.Close() })
	queue, err := openHistorySyncQueue(database, maxRows, maxBytes)
	if err != nil {
		t.Fatalf("openHistorySyncQueue(): %v", err)
	}
	return queue
}

// queueTestNotification is a notification naming a chunk on the media
// server; its encoded size grows with padding.
func queueTestNotification(id string, padding int) *waE2E.HistorySyncNotification {
	return &waE2E.HistorySyncNotification{
		SyncType:          waE2E.HistorySyncType_RECENT.Enum(),
		ChunkOrder:        proto.Uint32(1),
		OriginalMessageID: proto.String(id),
		DirectPath:        proto.String("/v/t62.history/" + string(make([]byte, padding))),
		MediaKey:          []byte("media-key-" + id),
	}
}

func encodedSize(t *testing.T, notification *waE2E.HistorySyncNotification) int64 {
	t.Helper()
	encoded, err := proto.Marshal(notification)
	if err != nil {
		t.Fatalf("marshal notification: %v", err)
	}
	return int64(len(encoded))
}

// queuedIDs lists the queue's message ids oldest first, read back from the
// table.
func queuedIDs(t *testing.T, queue *historySyncQueue) []string {
	t.Helper()
	seqs, err := queue.due(queueTestFarFuture)
	if err != nil {
		t.Fatalf("due(): %v", err)
	}
	ids := []string{}
	for _, seq := range seqs {
		row, ok, err := queue.load(seq)
		if err != nil || !ok {
			t.Fatalf("load(%d) = %v, %v", seq, ok, err)
		}
		ids = append(ids, string(row.messageID))
	}
	return ids
}

func TestHistorySyncQueueStoresDedupesAndSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "whatsapp-session.db")
	queue := openHistorySyncQueueForTest(t, path, 16, 1<<20)
	received := time.Unix(1_775_006_000, 123_456_789)

	notifications := map[string]*waE2E.HistorySyncNotification{
		"A": queueTestNotification("A", 4),
		"B": {
			SyncType:                          waE2E.HistorySyncType_INITIAL_BOOTSTRAP.Enum(),
			InitialHistBootstrapInlinePayload: []byte("inline chunk"),
		},
		"C": queueTestNotification("C", 0),
	}
	for _, id := range []string{"A", "B", "C"} {
		if err := queue.enqueue(watypes.MessageID(id), "15550000000", notifications[id], received); err != nil {
			t.Fatalf("enqueue(%s): %v", id, err)
		}
	}
	// The same protocol message delivered again is the same row, whatever
	// it carries.
	if err := queue.enqueue("B", "15550000000", queueTestNotification("other", 9), received); err != nil {
		t.Fatalf("enqueue(B) again: %v", err)
	}
	if got := queuedIDs(t, queue); !reflect.DeepEqual(got, []string{"A", "B", "C"}) {
		t.Fatalf("queued = %v", got)
	}
	if queue.pending() != 3 {
		t.Fatalf("pending() = %d, want 3", queue.pending())
	}

	seqs, _ := queue.due(received)
	now := received.Add(time.Minute)
	if err := queue.markCommitted(seqs[0], now); err != nil {
		t.Fatalf("markCommitted(): %v", err)
	}
	if err := queue.deferAttempt(seqs[2], 3, now.Add(time.Hour), errors.New("timeout")); err != nil {
		t.Fatalf("deferAttempt(): %v", err)
	}
	// A deferred notification is not due until its next attempt.
	if due, _ := queue.due(now); !reflect.DeepEqual(due, seqs[:2]) {
		t.Fatalf("due before the retry = %v, want %v", due, seqs[:2])
	}
	if next, ok, err := queue.nextAttempt(now); err != nil || !ok || !next.Equal(now.Add(time.Hour)) {
		t.Fatalf("nextAttempt() = %v, %v, %v; want %v", next, ok, err, now.Add(time.Hour))
	}

	reopened := openHistorySyncQueueForTest(t, path, 16, 1<<20)
	if reopened.pending() != 3 {
		t.Fatalf("pending() after reopen = %d, want 3", reopened.pending())
	}
	for index, id := range []string{"A", "B", "C"} {
		row, ok, err := reopened.load(seqs[index])
		if err != nil || !ok {
			t.Fatalf("load(%s) = %v, %v", id, ok, err)
		}
		if string(row.messageID) != id || row.accountUser != "15550000000" || !row.receivedAt.Equal(received) ||
			!proto.Equal(row.notification, notifications[id]) {
			t.Fatalf("reloaded %s = %+v", id, row)
		}
		wantCommitted, wantAttempts := id == "A", 0
		if id == "C" {
			wantAttempts = 3
		}
		if row.committed != wantCommitted || row.attempts != wantAttempts {
			t.Fatalf("reloaded %s committed=%v attempts=%d, want %v and %d", id, row.committed, row.attempts, wantCommitted, wantAttempts)
		}
	}
	// An inline payload survives as inline: present, not merely empty.
	row, _, _ := reopened.load(seqs[1])
	if row.notification.InitialHistBootstrapInlinePayload == nil {
		t.Fatal("inline payload lost on reload")
	}

	for _, seq := range seqs {
		if err := reopened.remove(seq); err != nil {
			t.Fatalf("remove(): %v", err)
		}
	}
	if err := reopened.remove(seqs[0]); err != nil {
		t.Fatalf("remove() again: %v", err)
	}
	if reopened.pending() != 0 {
		t.Fatalf("pending() after removing everything = %d", reopened.pending())
	}
}

func TestHistorySyncQueueRefusesWhatItCannotKeep(t *testing.T) {
	queue := openHistorySyncQueueForTest(t, filepath.Join(t.TempDir(), "whatsapp-session.db"), 16, 1<<20)
	received := time.Unix(1_775_006_000, 0)
	if err := queue.enqueue("  ", "15550000000", queueTestNotification("x", 0), received); err == nil {
		t.Fatal("enqueue() accepted a notification with no message id")
	}
	if err := queue.enqueue("x", "15550000000", nil, received); err == nil {
		t.Fatal("enqueue() accepted no notification")
	}
	var nilQueue *historySyncQueue
	if err := nilQueue.enqueue("x", "15550000000", queueTestNotification("x", 0), received); !errors.Is(err, errHistorySyncQueueUnavailable) {
		t.Fatalf("nil queue enqueue() = %v, want errHistorySyncQueueUnavailable", err)
	}
	if nilQueue.pending() != 0 {
		t.Fatal("nil queue reports pending notifications")
	}
	if queue.pending() != 0 {
		t.Fatalf("refused notifications were stored: %d", queue.pending())
	}
}

func TestHistorySyncQueueDropsARowThatNoLongerDecodes(t *testing.T) {
	queue := openHistorySyncQueueForTest(t, filepath.Join(t.TempDir(), "whatsapp-session.db"), 16, 1<<20)
	if err := queue.enqueue("bad", "15550000000", queueTestNotification("bad", 0), time.Unix(1, 0)); err != nil {
		t.Fatalf("enqueue(): %v", err)
	}
	if _, err := queue.db.Exec(`UPDATE openmessage_whatsapp_history_sync_pending SET notification = x'ffff'`); err != nil {
		t.Fatalf("corrupt the row: %v", err)
	}
	seqs, _ := queue.due(time.Unix(2, 0))
	row, ok, err := queue.load(seqs[0])
	if !ok || !errors.Is(err, errHistorySyncRowUnreadable) || row.seq != seqs[0] || row.messageID != "bad" {
		t.Fatalf("load(corrupt) = %+v, %v, %v; want the row's identity and errHistorySyncRowUnreadable", row, ok, err)
	}
}

// TestHistorySyncQueueMatchesModel runs random enqueue / commit / defer /
// remove / reopen sequences against a model of the queue: an ordered list of
// (id, size, committed, attempts, next attempt). After every step the queue
// holds exactly the model's notifications in the model's order with the
// model's state, its count matches the table, it stays within both bounds,
// due lists exactly the model's due rows, and enqueue succeeds exactly when
// the id is already queued or the notification fits.
func TestHistorySyncQueueMatchesModel(t *testing.T) {
	const maxRows = 4
	random := rand.New(rand.NewSource(20261011))
	maxBytes := int64(0)
	for padding := 0; padding < 6; padding++ {
		maxBytes = max(maxBytes, encodedSize(t, queueTestNotification("k0", padding*8)))
	}
	maxBytes = maxBytes*2 + 10 // room for two of the largest, so the byte bound binds too
	type entry struct {
		id        string
		size      int64
		committed bool
		attempts  int
		next      int64
	}
	for trial := 0; trial < 200; trial++ {
		path := filepath.Join(t.TempDir(), fmt.Sprintf("queue-%d.db", trial))
		queue := openHistorySyncQueueForTest(t, path, maxRows, maxBytes)
		var model []entry
		modelBytes := func() int64 {
			var total int64
			for _, e := range model {
				total += e.size
			}
			return total
		}
		clock := int64(1_000)
		for step := 0; step < 30; step++ {
			label := fmt.Sprintf("trial %d step %d", trial, step)
			switch op := random.Intn(12); {
			case op < 5:
				id := fmt.Sprintf("k%d", random.Intn(8))
				padding := random.Intn(6)
				notification := queueTestNotification(id, padding*8)
				size := encodedSize(t, notification)
				err := queue.enqueue(watypes.MessageID(id), "15550000000", notification, time.Unix(0, clock))
				already := false
				for _, e := range model {
					already = already || e.id == id
				}
				fits := len(model) < maxRows && modelBytes()+size <= maxBytes
				switch {
				case already:
					if err != nil {
						t.Fatalf("%s: enqueue(queued id %s) = %v, want nil", label, id, err)
					}
				case fits:
					if err != nil {
						t.Fatalf("%s: enqueue(%s, %d bytes) = %v, want nil", label, id, size, err)
					}
					model = append(model, entry{id: id, size: size})
				default:
					if !errors.Is(err, errHistorySyncQueueFull) {
						t.Fatalf("%s: enqueue(%s, %d bytes) over the bounds = %v, want errHistorySyncQueueFull", label, id, size, err)
					}
				}
			case op < 10:
				if len(model) == 0 {
					continue
				}
				index := random.Intn(len(model))
				seqs, err := queue.due(queueTestFarFuture)
				if err != nil {
					t.Fatalf("due(): %v", err)
				}
				switch op {
				case 5, 6:
					if err := queue.remove(seqs[index]); err != nil {
						t.Fatalf("remove(): %v", err)
					}
					model = append(model[:index], model[index+1:]...)
				case 7:
					if err := queue.markCommitted(seqs[index], time.Unix(0, clock)); err != nil {
						t.Fatalf("markCommitted(): %v", err)
					}
					model[index].committed = true
					model[index].next = 0
				default:
					attempts := model[index].attempts + 1
					next := clock + int64(random.Intn(3))*100
					if err := queue.deferAttempt(seqs[index], attempts, time.Unix(0, next), errors.New("failed")); err != nil {
						t.Fatalf("deferAttempt(): %v", err)
					}
					model[index].attempts = attempts
					model[index].next = next
				}
			case op < 11:
				clock += 100
			default:
				queue = openHistorySyncQueueForTest(t, path, maxRows, maxBytes)
			}

			want := []string{}
			wantDue := []string{}
			for _, e := range model {
				want = append(want, e.id)
				if e.next <= clock {
					wantDue = append(wantDue, e.id)
				}
			}
			if got := queuedIDs(t, queue); !reflect.DeepEqual(got, want) {
				t.Fatalf("%s: queued = %v, want %v", label, got, want)
			}
			dueSeqs, err := queue.due(time.Unix(0, clock))
			if err != nil {
				t.Fatalf("due(): %v", err)
			}
			gotDue := []string{}
			for _, seq := range dueSeqs {
				row, _, _ := queue.load(seq)
				gotDue = append(gotDue, string(row.messageID))
			}
			if !reflect.DeepEqual(gotDue, wantDue) {
				t.Fatalf("%s: due = %v, want %v", label, gotDue, wantDue)
			}
			allSeqs, _ := queue.due(queueTestFarFuture)
			for index, seq := range allSeqs {
				row, _, _ := queue.load(seq)
				if row.committed != model[index].committed || row.attempts != model[index].attempts {
					t.Fatalf("%s: %s committed=%v attempts=%d, model %+v", label, row.messageID, row.committed, row.attempts, model[index])
				}
			}
			var count, total int64
			if err := queue.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(LENGTH(notification)), 0) FROM openmessage_whatsapp_history_sync_pending`).Scan(&count, &total); err != nil {
				t.Fatalf("count queue: %v", err)
			}
			if queue.pending() != count || count > maxRows || total > maxBytes || total != modelBytes() {
				t.Fatalf("%s: pending %d, table %d rows / %d bytes, model %d bytes, bounds %d / %d",
					label, queue.pending(), count, total, modelBytes(), maxRows, maxBytes)
			}
		}
	}
}
