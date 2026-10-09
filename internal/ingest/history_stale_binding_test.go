package ingest

import (
	"context"
	"database/sql"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// Second-round review of PR #200: history must not trust a binding the
// phone's own snapshot contradicts, must finish a frame across a transient
// storage error through the real retry wiring, must not touch the outbox for
// a message it skips, and must not rewrite known identities while minting.

// After a phone swap or restore, a remote ID can still be bound in v2 to the
// thread it named on the old phone. A fetched snapshot that names a different
// thread under that ID must stop the frame from filing anything there,
// incoming or outgoing. (The first revision filed both into the stale thread.)
func TestHistoryStaleBindingGroupIsNotFiledInto(t *testing.T) {
	h := newHWTHarness(t, false)
	family := hwtConversation("zz-42", "Family", true, hwtKarl, hwtShoshana)
	h.liveConversation(t, family)
	h.liveMessage(t, hwtMessage{id: "zz-42-old", conversation: "zz-42", body: "dinner?", from: hwtKarl, at: hwtStart.Add(-48 * time.Hour)}.proto())
	h.pump(t)
	group := h.conversation(t, "zz-42")
	groupBefore := h.rows(t, `SELECT * FROM conversations WHERE conversation_id = ?`, group.ConversationID)
	messagesBefore := h.rows(t, `SELECT * FROM messages ORDER BY message_id`)

	// On the phone, zz-42 is now Bea's 1:1 thread.
	bea := hwtConversation("zz-42", "Bea", false, hwtBea)
	h.historyMessage(t, bea, hwtMessage{id: "zz-42-1", conversation: "zz-42", body: "hi from Bea", from: hwtBea, at: hwtStart.Add(-time.Hour)}.proto())
	h.historyMessage(t, bea, hwtMessage{id: "zz-42-2", conversation: "zz-42", body: "see you at 6", at: hwtStart.Add(-30 * time.Minute)}.proto())
	h.pump(t)

	counts := h.counts()
	if counts.HistoryImported != 0 || counts.RemoteRebinds != 0 || counts.Quarantined != 0 {
		t.Fatalf("counters = %+v, want nothing imported or rebound", counts)
	}
	// Each frame: its snapshot and its message.
	if counts.HistorySkipped != 4 {
		t.Fatalf("history_skipped = %d, want 4", counts.HistorySkipped)
	}
	if got := h.rows(t, `SELECT * FROM messages ORDER BY message_id`); !sameRows(got, messagesBefore) {
		t.Fatalf("messages changed:\n before %v\n after  %v", messagesBefore, got)
	}
	if got := h.rows(t, `SELECT * FROM conversations WHERE conversation_id = ?`, group.ConversationID); !sameRows(got, groupBefore) {
		t.Fatalf("the Family group changed:\n before %v\n after  %v", groupBefore, got)
	}
}

// The 1:1 variant: zz-7 is bound to Ada's thread but is Bea's on the phone.
// The outgoing message, which no sender check can catch, must not land in
// Ada's thread.
func TestHistoryStaleBindingDirectThreadGetsNoOutgoingMessage(t *testing.T) {
	h := newHWTHarness(t, false)
	h.liveConversation(t, hwtConversation("zz-7", "Ada", false, hwtAda))
	h.pump(t)
	ada := h.conversation(t, "zz-7")

	bea := hwtConversation("zz-7", "Bea", false, hwtBea)
	h.historyMessage(t, bea, hwtMessage{id: "zz-7-out", conversation: "zz-7", body: "hey Bea, see you at 6", at: hwtStart.Add(-time.Hour)}.proto())
	h.pump(t)

	if n := i01QueryInt64(t, h.path, `SELECT COUNT(*) FROM messages WHERE conversation_id = ?`, ada.ConversationID); n != 0 {
		t.Fatalf("Ada's thread holds %d messages, want none", n)
	}
	if counts := h.counts(); counts.HistorySkipped != 2 || counts.HistoryImported != 0 {
		t.Fatalf("counters = %+v, want the snapshot and the message skipped", counts)
	}
}

// What the contradiction check must still let through: a group whose
// membership grew (group rosters only contradict when fully disjoint), and a
// bound thread with no known peers (which proves nothing).
func TestHistoryConsistentSnapshotStillImports(t *testing.T) {
	t.Run("group membership grew", func(t *testing.T) {
		h := newHWTHarness(t, false)
		h.liveConversation(t, hwtConversation("hp-grew", "Family", true, hwtKarl, hwtShoshana))
		h.pump(t)
		grown := hwtConversation("hp-grew", "Family", true, hwtKarl, hwtShoshana, hwtBea)
		h.historyMessage(t, grown, hwtMessage{id: "hp-grew-1", conversation: "hp-grew", body: "welcome Bea", from: hwtBea, at: hwtStart.Add(-time.Hour)}.proto())
		h.pump(t)
		if counts := h.counts(); counts.HistoryImported != 1 || counts.HistorySkipped != 0 {
			t.Fatalf("counters = %+v, want the message imported", counts)
		}
		if got := h.message(t, "hp-grew", "hp-grew-1").Body; got != "welcome Bea" {
			t.Fatalf("imported body = %q", got)
		}
	})

	t.Run("bound thread with no known peers", func(t *testing.T) {
		h := newHWTHarness(t, false)
		// An outgoing live message mints a direct thread with no peers.
		h.liveMessage(t, hwtMessage{id: "hp-nopeer-0", conversation: "hp-nopeer", body: "sent first", at: hwtStart.Add(-2 * time.Hour)}.proto())
		h.pump(t)
		if peers := h.peers(t, h.conversation(t, "hp-nopeer").ConversationID); len(peers) != 0 {
			t.Fatalf("fixture: the minted thread has peers %v", peers)
		}
		snapshot := hwtConversation("hp-nopeer", "Ada", false, hwtAda)
		h.historyMessage(t, snapshot, hwtMessage{id: "hp-nopeer-1", conversation: "hp-nopeer", body: "reply", from: hwtAda, at: hwtStart.Add(-time.Hour)}.proto())
		h.pump(t)
		if counts := h.counts(); counts.HistoryImported != 1 || counts.HistorySkipped != 0 {
			t.Fatalf("counters = %+v, want the message imported", counts)
		}
	})
}

// A thread v2 knows only from a live message frame is stored as direct with
// the sender as its peer, a default the live conversation event corrects
// later. A group snapshot contradicts it, so history waits: the frame is
// skipped, and once the live event has corrected the thread, fetching the
// same history again files the message.
func TestHistoryKindMismatchWaitsForTheLiveCorrection(t *testing.T) {
	h := newHWTHarness(t, false)
	h.liveMessage(t, hwtMessage{id: "hp-kind-0", conversation: "hp-kind", body: "first", from: hwtKarl, at: hwtStart.Add(-2 * time.Hour)}.proto())
	h.pump(t)
	if got := h.conversation(t, "hp-kind").Kind; got != "direct" {
		t.Fatalf("fixture: message-minted thread kind = %q, want direct", got)
	}

	group := hwtConversation("hp-kind", "Weekend", true, hwtKarl, hwtShoshana)
	record := h.historyMessage(t, group, hwtMessage{id: "hp-kind-1", conversation: "hp-kind", body: "fetched", from: hwtShoshana, at: hwtStart.Add(-time.Hour)}.proto())
	h.pump(t)
	if counts := h.counts(); counts.HistoryImported != 0 || counts.HistorySkipped != 2 {
		t.Fatalf("counters = %+v, want the frame skipped", counts)
	}

	h.liveConversation(t, group)
	h.pump(t)
	record.ReceivedAt = h.tick()
	h.appendHistory(t, record)
	h.pump(t)
	if counts := h.counts(); counts.HistoryImported != 1 {
		t.Fatalf("counters after the live correction and a re-fetch = %+v, want the message imported", counts)
	}
	if got := h.message(t, "hp-kind", "hp-kind-1").Body; got != "fetched" {
		t.Fatalf("imported body = %q", got)
	}
}

// A skipped outgoing message never reaches the outbox echo observer: the echo
// is observed only once the message is known to be placeable.
func TestHistoryUnplaceableOutgoingMessageNeverReachesTheOutbox(t *testing.T) {
	h := newHWTHarness(t, false)
	h.historyMessage(t, nil, hwtMessage{
		id: "hp-echo-1", conversation: "hp-echo-unbound", body: "sent during the stall",
		tmpID: "tmp-hp-echo-1", at: hwtStart.Add(-time.Hour),
	}.proto())
	h.pump(t)
	if counts := h.counts(); counts.HistorySkipped != 1 || counts.HistoryImported != 0 {
		t.Fatalf("counters = %+v, want the message skipped", counts)
	}
	if got := h.echoCalls(); got != 0 {
		t.Fatalf("echo observer calls = %d, want 0 for a skipped message", got)
	}

	// Placeable once its thread is bound: then the echo is observed.
	h.liveConversation(t, hwtConversation("hp-echo-unbound", "Ada", false, hwtAda))
	h.pump(t)
	h.historyMessage(t, nil, hwtMessage{
		id: "hp-echo-2", conversation: "hp-echo-unbound", body: "and another",
		tmpID: "tmp-hp-echo-2", at: hwtStart.Add(-30 * time.Minute),
	}.proto())
	h.pump(t)
	if got := h.echoCalls(); got != 1 {
		t.Fatalf("echo observer calls = %d, want 1 for the placed message", got)
	}
}

// Minting a thread from a snapshot, and checking its roster, never rewrites a
// contact v2 already knows, even when the snapshot carries another name.
func TestHistoryMintNeverRewritesKnownIdentity(t *testing.T) {
	h := newHWTHarness(t, false)
	h.liveConversation(t, hwtConversation("hp-known", "Karl", false, hwtKarl))
	h.pump(t)
	const karlRow = `SELECT display_name || '|' || raw_value || '|' || updated_at_ms FROM identities WHERE canonical_value = ?`
	before := hpQueryString(t, h, karlRow, hwtKarl)

	renamed := hwtConversation("hp-new-group", "Book club", true, hwtKarl, hwtAda)
	for _, participant := range renamed.Participants {
		if participant.GetID().GetNumber() == hwtKarl {
			participant.FullName = "Karl (old phonebook)"
		}
	}
	h.historyConversation(t, renamed)
	h.pump(t)
	if counts := h.counts(); counts.HistoryConversations != 1 {
		t.Fatalf("counters = %+v, want the group minted", counts)
	}
	if after := hpQueryString(t, h, karlRow, hwtKarl); after != before {
		t.Fatalf("known identity rewritten while minting: %q -> %q", before, after)
	}

	// The roster check on a bound thread only looks identities up.
	rebadged := hwtConversation("hp-known", "Karl", false, hwtKarl)
	rebadged.Participants[1].FullName = "Karl (old phonebook)"
	h.historyConversation(t, rebadged)
	h.pump(t)
	if after := hpQueryString(t, h, karlRow, hwtKarl); after != before {
		t.Fatalf("known identity rewritten by the roster check: %q -> %q", before, after)
	}
}

// hpTransientError returns a real SQLITE_BUSY error, the kind the worker
// retries, by contending for a write lock held by another connection.
func hpTransientError(t *testing.T) error {
	t.Helper()
	dsn := "file:" + filepath.Join(t.TempDir(), "busy.sqlite3") + "?_pragma=busy_timeout(0)"
	locker, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("sql.Open(locker): %v", err)
	}
	t.Cleanup(func() { _ = locker.Close() })
	contender, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("sql.Open(contender): %v", err)
	}
	t.Cleanup(func() { _ = contender.Close() })
	if _, err := locker.Exec(`CREATE TABLE busy_probe (value INTEGER)`); err != nil {
		t.Fatalf("create busy probe: %v", err)
	}
	connection, err := locker.Conn(context.Background())
	if err != nil {
		t.Fatalf("locker.Conn(): %v", err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	if _, err := connection.ExecContext(context.Background(), `BEGIN IMMEDIATE`); err != nil {
		t.Fatalf("BEGIN IMMEDIATE: %v", err)
	}
	_, busy := contender.Exec(`INSERT INTO busy_probe (value) VALUES (1)`)
	_, _ = connection.ExecContext(context.Background(), `ROLLBACK`)
	if busy == nil || !isTransientDBError(busy) {
		t.Fatalf("contending insert error = %v, want a transient SQLite error", busy)
	}
	return busy
}

// failOnce makes the worker fail once at point with a transient error.
func failOnce(h *hwtHarness, point string, transient error) *int {
	var mu sync.Mutex
	fired := 0
	h.worker.fault = func(at string) error {
		if at != point {
			return nil
		}
		mu.Lock()
		defer mu.Unlock()
		if fired > 0 {
			return nil
		}
		fired++
		return transient
	}
	return &fired
}

// Through the real retry wiring (handleRecord): a transient error after the
// history insert is retried, and the retry finishes the message's reactions
// instead of finding its own row and skipping the frame.
func TestHistoryRetryThroughHandleRecordFinishesReactions(t *testing.T) {
	h := newHWTHarness(t, false)
	group := hwtConversation("hp-rr", "Family", true, hwtKarl, hwtShoshana)
	h.liveConversation(t, group)
	h.pump(t)
	fired := failOnce(h, "history-message-inserted", hpTransientError(t))

	h.historyMessage(t, group, hwtMessage{
		id: "hp-rr-1", conversation: "hp-rr", body: "with a reaction", from: hwtKarl,
		at: hwtStart.Add(-time.Hour), reactions: []hwtReaction{{"👍", hwtShoshana}},
	}.proto())
	h.pump(t)

	if *fired != 1 {
		t.Fatalf("fault fired %d times, want once", *fired)
	}
	stored := h.message(t, "hp-rr", "hp-rr-1")
	if n := i01QueryInt64(t, h.path, `SELECT COUNT(*) FROM reactions WHERE message_id = ?`, stored.MessageID); n != 1 {
		t.Fatalf("reactions = %d after the retried frame, want 1", n)
	}
	if counts := h.counts(); counts.HistoryImported != 1 || counts.HistoryExisting != 0 || counts.Quarantined != 0 {
		t.Fatalf("counters = %+v, want one import and no existing/quarantine", counts)
	}
}

// The same for a minted thread: a transient error after the thread row
// commits is retried (not counted as an unusable snapshot), and the retry
// writes the roster instead of finding its own row bound and stopping.
func TestHistoryRetryThroughHandleRecordFinishesTheRoster(t *testing.T) {
	h := newHWTHarness(t, false)
	fired := failOnce(h, "conversation-upserted", hpTransientError(t))

	snapshot := hwtConversation("hp-rt", "New circle", true, hwtAda, hwtBea)
	h.historyMessage(t, snapshot, hwtMessage{
		id: "hp-rt-1", conversation: "hp-rt", body: "hello circle", from: hwtAda, at: hwtStart.Add(-time.Hour),
	}.proto())
	h.pump(t)

	if *fired != 1 {
		t.Fatalf("fault fired %d times, want once", *fired)
	}
	thread := h.conversation(t, "hp-rt")
	if peers := h.peers(t, thread.ConversationID); len(peers) != 2 || !containsAll(peers, []string{hwtAda, hwtBea}) {
		t.Fatalf("roster after the retried mint = %v, want Ada and Bea", peers)
	}
	if got := h.message(t, "hp-rt", "hp-rt-1"); got.ConversationID != thread.ConversationID {
		t.Fatalf("message filed in %q, want the minted thread", got.ConversationID)
	}
	if counts := h.counts(); counts.HistoryConversations != 1 || counts.HistorySkipped != 0 || counts.HistoryImported != 1 {
		t.Fatalf("counters = %+v, want one thread, one import, nothing skipped", counts)
	}
}
