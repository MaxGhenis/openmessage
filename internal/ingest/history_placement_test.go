package ingest

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

// History placement: a fetched frame never moves a thread binding. These tests
// pin the cases an independent review of PR #200 reproduced against the first
// version of the history worker, which ran the live #176 rebinding for
// history too, plus the retry and identity rules that follow from "history
// only fills gaps".

func hpQueryString(t *testing.T, h *hwtHarness, query string, args ...any) string {
	t.Helper()
	database := i01OpenInspector(t, h.path)
	defer database.Close()
	var value string
	if err := database.QueryRow(query, args...).Scan(&value); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return value
}

func (h *hwtHarness) bound(t *testing.T, remoteConversationID string) bool {
	t.Helper()
	_, err := h.store.GetConversationByRemote(i01AccountID, remoteConversationID)
	if err == nil {
		return true
	}
	if !errors.Is(err, sqlite.ErrNotFound) {
		t.Fatalf("GetConversationByRemote(%q): %v", remoteConversationID, err)
	}
	return false
}

func (h *hwtHarness) messageRows(t *testing.T, remoteMessageID string) int64 {
	t.Helper()
	return i01QueryInt64(t, h.path, `SELECT COUNT(*) FROM messages WHERE remote_message_id = ?`, remoteMessageID)
}

// The phone can hold two groups with the same members (an old one and the one
// in use). A fetched snapshot and message for the one v2 has never bound must
// not take the bound group's remote ID, title or roster (#176 would rebind by
// roster), and the bound group's next live message must still land in it.
func TestHistoryPlacementIdenticalRosterGroupIsNotRebound(t *testing.T) {
	h := newHWTHarness(t, false)
	family := hwtConversation("hp-family-live", "Family", true, hwtKarl, hwtShoshana)
	h.liveConversation(t, family)
	h.liveConversation(t, hwtConversation("hp-karl", "Karl", false, hwtKarl))
	h.pump(t)
	group := h.conversation(t, "hp-family-live")
	groupBefore := h.rows(t, `SELECT * FROM conversations WHERE conversation_id = ?`, group.ConversationID)
	rosterBefore := h.rows(t, `SELECT * FROM conversation_participants WHERE conversation_id = ? ORDER BY identity_id`, group.ConversationID)
	karlBefore := h.rows(t, `SELECT * FROM conversations WHERE remote_conversation_id = 'hp-karl'`)

	old := hwtConversation("hp-family-2019", "Family (2019)", true, hwtKarl, hwtShoshana)
	h.historyConversation(t, old)
	h.historyMessage(t, old, hwtMessage{
		id: "hp-old-1", conversation: "hp-family-2019", body: "from the old thread", from: hwtKarl,
		at: hwtStart.Add(-400 * 24 * time.Hour),
	}.proto())
	h.pump(t)

	counts := h.counts()
	if counts.RemoteRebinds != 0 || counts.HistoryConversations != 0 || counts.HistoryImported != 0 || counts.Quarantined != 0 {
		t.Fatalf("counters after ambiguous history = %+v, want no rebind, no thread, no import", counts)
	}
	// The standalone snapshot, the snapshot riding in the message frame, and
	// the message itself.
	if counts.HistorySkipped != 3 {
		t.Fatalf("history_skipped = %d, want 3", counts.HistorySkipped)
	}
	if h.bound(t, "hp-family-2019") {
		t.Fatal("the old thread's remote ID was bound; history must not bind by roster")
	}
	if got := h.rows(t, `SELECT * FROM conversations WHERE conversation_id = ?`, group.ConversationID); !sameRows(got, groupBefore) {
		t.Fatalf("bound group changed:\n before %v\n after  %v", groupBefore, got)
	}
	if got := h.rows(t, `SELECT * FROM conversation_participants WHERE conversation_id = ? ORDER BY identity_id`, group.ConversationID); !sameRows(got, rosterBefore) {
		t.Fatalf("bound group roster changed:\n before %v\n after  %v", rosterBefore, got)
	}
	if got := h.rows(t, `SELECT * FROM conversations WHERE remote_conversation_id = 'hp-karl'`); !sameRows(got, karlBefore) {
		t.Fatalf("Karl's 1:1 thread changed:\n before %v\n after  %v", karlBefore, got)
	}
	if n := h.messageRows(t, "hp-old-1"); n != 0 {
		t.Fatalf("the old thread's message was stored %d times; it has no thread v2 can place it in", n)
	}

	// The group in use keeps receiving its live messages.
	h.liveMessage(t, hwtMessage{
		id: "hp-live-1", conversation: "hp-family-live", body: "dinner?", from: hwtKarl, at: hwtStart,
	}.proto())
	h.pump(t)
	if got := h.message(t, "hp-family-live", "hp-live-1"); got.ConversationID != group.ConversationID {
		t.Fatalf("live group message filed in %q, want the Family group %q", got.ConversationID, group.ConversationID)
	}
	if after := h.counts(); after.RemoteRebinds != 0 {
		t.Fatalf("remote_rebinds = %d after the live message, want 0", after.RemoteRebinds)
	}
}

// The same for 1:1 threads: a second thread with the same person (say an SMS
// and an RCS thread) must not take the bound thread's remote ID.
func TestHistoryPlacementSamePeerDirectThreadIsNotRebound(t *testing.T) {
	h := newHWTHarness(t, false)
	h.liveConversation(t, hwtConversation("hp-ada-rcs", "Ada", false, hwtAda))
	h.pump(t)
	thread := h.conversation(t, "hp-ada-rcs")
	before := h.rows(t, `SELECT * FROM conversations WHERE conversation_id = ?`, thread.ConversationID)

	sms := hwtConversation("hp-ada-sms", "Ada (SMS)", false, hwtAda)
	h.historyMessage(t, sms, hwtMessage{
		id: "hp-sms-1", conversation: "hp-ada-sms", body: "old sms", from: hwtAda, at: hwtStart.Add(-time.Hour),
	}.proto())
	h.historyMessage(t, sms, hwtMessage{
		id: "hp-sms-2", conversation: "hp-ada-sms", body: "old sms reply", at: hwtStart.Add(-30 * time.Minute),
	}.proto())
	h.pump(t)

	counts := h.counts()
	if counts.RemoteRebinds != 0 || counts.HistoryImported != 0 || counts.HistoryConversations != 0 {
		t.Fatalf("counters = %+v, want nothing rebound, created or imported", counts)
	}
	if h.bound(t, "hp-ada-sms") {
		t.Fatal("the second thread's remote ID was bound")
	}
	if got := h.rows(t, `SELECT * FROM conversations WHERE conversation_id = ?`, thread.ConversationID); !sameRows(got, before) {
		t.Fatalf("bound 1:1 thread changed:\n before %v\n after  %v", before, got)
	}
	if n := h.messageRows(t, "hp-sms-1") + h.messageRows(t, "hp-sms-2"); n != 0 {
		t.Fatalf("%d messages of the unbound thread were stored", n)
	}

	// An outgoing live message on the bound ID still lands in that thread.
	h.liveMessage(t, hwtMessage{id: "hp-rcs-out", conversation: "hp-ada-rcs", body: "on my way", at: hwtStart}.proto())
	h.pump(t)
	if got := h.message(t, "hp-ada-rcs", "hp-rcs-out"); got.ConversationID != thread.ConversationID {
		t.Fatalf("outgoing live message filed in %q, want %q", got.ConversationID, thread.ConversationID)
	}
}

// A fetched incoming message whose sender is not the bound 1:1 thread's peer
// is the #176 id-collision signature. The live path reroutes it and moves the
// binding; history leaves both alone and skips the message.
func TestHistoryPlacementSenderContradictingBoundThreadIsSkipped(t *testing.T) {
	h := newHWTHarness(t, false)
	h.liveConversation(t, hwtConversation("hp-collide", "Ada", false, hwtAda))
	h.pump(t)
	thread := h.conversation(t, "hp-collide")
	conversationsBefore := h.rows(t, `SELECT * FROM conversations ORDER BY conversation_id`)

	h.historyMessage(t, nil, hwtMessage{
		id: "hp-collide-1", conversation: "hp-collide", body: "who is this", from: hwtBea, at: hwtStart.Add(-time.Hour),
	}.proto())
	h.pump(t)

	counts := h.counts()
	if counts.HistorySkipped != 1 || counts.HistoryImported != 0 || counts.RemoteRebinds != 0 || counts.Quarantined != 0 {
		t.Fatalf("counters = %+v, want one skip and no rebind", counts)
	}
	if got := h.rows(t, `SELECT * FROM conversations ORDER BY conversation_id`); !sameRows(got, conversationsBefore) {
		t.Fatalf("conversations changed:\n before %v\n after  %v", conversationsBefore, got)
	}
	if got := h.conversation(t, "hp-collide"); got.ConversationID != thread.ConversationID {
		t.Fatalf("remote ID now bound to %q, want %q", got.ConversationID, thread.ConversationID)
	}
	if n := h.messageRows(t, "hp-collide-1"); n != 0 {
		t.Fatalf("contradicting message stored %d times", n)
	}

	// The peer's own fetched message in the same thread is placed.
	h.historyMessage(t, nil, hwtMessage{
		id: "hp-collide-2", conversation: "hp-collide", body: "hi", from: hwtAda, at: hwtStart.Add(-30 * time.Minute),
	}.proto())
	h.pump(t)
	if got := h.message(t, "hp-collide", "hp-collide-2"); got.ConversationID != thread.ConversationID || got.Body != "hi" {
		t.Fatalf("peer's history message = %+v, want it in the bound thread", got)
	}
}

// A message fetched without a snapshot for a thread v2 has never bound is
// skipped without minting a thread (its kind and roster are unknown). When a
// live frame later binds the thread, fetching the same history again places
// the message: the re-fetch dedupes onto the stored history row and replays it.
func TestHistoryPlacementSkippedMessageIsPlacedOnceLiveBindsThread(t *testing.T) {
	h := newHWTHarness(t, false)
	message := hwtMessage{
		id: "hp-late-1", conversation: "hp-late", body: "sent during the stall", from: hwtKarl, at: hwtStart.Add(-2 * time.Hour),
	}.proto()
	record := h.historyMessage(t, nil, message)
	h.pump(t)
	first := h.counts()
	if first.HistorySkipped != 1 || first.HistoryImported != 0 || first.HistoryConversations != 0 {
		t.Fatalf("counters after unbound history = %+v, want one skip", first)
	}
	if h.bound(t, "hp-late") {
		t.Fatal("a thread was minted for a snapshot-less history message")
	}
	if codec, processed := h.inboxRow(t, record.DedupeKey); codec != GoogleHistoryCodec || !processed {
		t.Fatalf("skipped history row = (%q, processed %v), want a processed history row", codec, processed)
	}

	h.liveConversation(t, hwtConversation("hp-late", "Weekend", true, hwtKarl, hwtShoshana))
	h.pump(t)
	group := h.conversation(t, "hp-late")

	record.ReceivedAt = h.tick()
	h.appendHistory(t, record)
	h.pump(t)
	second := h.counts()
	if second.HistoryDeduped-first.HistoryDeduped != 1 || second.HistoryAppended != first.HistoryAppended ||
		second.HistoryImported != 1 || second.HistorySkipped != first.HistorySkipped {
		t.Fatalf("counters after re-fetch = %+v (before %+v), want one dedupe and one import", second, first)
	}
	if got := h.message(t, "hp-late", "hp-late-1"); got.ConversationID != group.ConversationID || got.Body != "sent during the stall" {
		t.Fatalf("re-fetched message = %+v, want it in the Weekend group", got)
	}
}

// libgm delivers roster entries with no number. One must not make the whole
// snapshot unusable (the first version quarantined the frame, message
// included): the thread is minted from the participants that have an address,
// and the message is placed in it.
func TestHistoryPlacementParticipantWithoutNumberIsLeftOffTheRoster(t *testing.T) {
	h := newHWTHarness(t, false)
	snapshot := hwtConversation("hp-nonumber", "Soccer parents", true, hwtAda, hwtBea)
	snapshot.Participants = append(snapshot.Participants, &gmproto.Participant{FullName: "No Number"})
	h.historyMessage(t, snapshot, hwtMessage{
		id: "hp-nonumber-1", conversation: "hp-nonumber", body: "practice moved", from: hwtAda, at: hwtStart.Add(-time.Hour),
	}.proto())
	h.pump(t)

	counts := h.counts()
	if counts.Quarantined != 0 || counts.HistoryConversations != 1 || counts.HistoryImported != 1 || counts.HistorySkipped != 0 {
		t.Fatalf("counters = %+v, want one thread and one message, nothing quarantined", counts)
	}
	group := h.conversation(t, "hp-nonumber")
	if group.Kind != sqlite.ConversationKindGroup || group.Title != "Soccer parents" {
		t.Fatalf("minted thread = %+v", group)
	}
	want := []string{hwtAda, hwtBea}
	if got := h.peers(t, group.ConversationID); len(got) != 2 || !containsAll(got, want) {
		t.Fatalf("minted roster peers = %v, want %v", got, want)
	}
	if got := h.message(t, "hp-nonumber", "hp-nonumber-1"); got.ConversationID != group.ConversationID {
		t.Fatalf("message filed in %q, want the minted group", got.ConversationID)
	}
}

// Regression (review of PR #200). v2 holds a message in state A. A catch-up
// fetches state B (an edit plus the hydrated attachment) before the phone
// pushes it; history skips it because the message exists. The phone then
// pushes state B live with byte-identical bytes. The push must be applied:
// with a shared dedupe key it collapsed onto the history row, was replayed
// against an already-processed inbox row and dropped as a stale replay, so v2
// kept the placeholder forever.
func TestHistoryPlacementLivePushAfterFetchedCopyIsApplied(t *testing.T) {
	h := newHWTHarness(t, false)
	group := hwtConversation("hp-hydrate", "Family", true, hwtKarl, hwtShoshana)
	h.liveConversation(t, group)
	at := hwtStart.Add(-time.Hour)
	stateA := hwtMessage{id: "hp-hydrate-1", conversation: "hp-hydrate", body: "Tap to download from phone", from: hwtKarl, at: at}
	h.liveMessage(t, stateA.proto())
	h.pump(t)

	stateB := stateA
	stateB.body = "the photo caption"
	stateB.media = &gmproto.MediaContent{MediaID: "media-hp-hydrate", MimeType: "image/jpeg", Size: 12}
	h.historyMessage(t, group, stateB.proto())
	h.pump(t)
	afterHistory := h.counts()
	if afterHistory.HistoryExisting != 1 || afterHistory.HistoryImported != 0 {
		t.Fatalf("counters after fetched copy = %+v, want it skipped as existing", afterHistory)
	}
	if got := h.message(t, "hp-hydrate", "hp-hydrate-1").Body; got != "Tap to download from phone" {
		t.Fatalf("history rewrote the existing body to %q", got)
	}

	h.liveMessage(t, stateB.proto())
	h.pump(t)
	afterLive := h.counts()
	if afterLive.StaleReplays != 0 || afterLive.Deduped != 0 || afterLive.Appended-afterHistory.Appended != 1 ||
		afterLive.Projected-afterHistory.Projected != 1 {
		t.Fatalf("counters after the live push = %+v (before %+v), want it appended and projected", afterLive, afterHistory)
	}
	stored := h.message(t, "hp-hydrate", "hp-hydrate-1")
	if stored.Body != "the photo caption" {
		t.Fatalf("body after the live push = %q, want the pushed state", stored.Body)
	}
	if n := i01QueryInt64(t, h.path, `SELECT COUNT(*) FROM message_attachments WHERE message_id = ? AND remote_id = 'media-hp-hydrate'`, stored.MessageID); n != 1 {
		t.Fatalf("attachments after the live push = %d, want the hydrated one", n)
	}
}

// History creates an identity v2 lacks but never rewrites one it has: a
// fetched copy's sender name must not replace the stored contact name or move
// the identity's update time, even for a message it inserts.
func TestHistoryPlacementNeverRewritesKnownIdentity(t *testing.T) {
	h := newHWTHarness(t, false)
	group := hwtConversation("hp-identity", "Family", true, hwtKarl)
	h.liveConversation(t, group)
	h.pump(t)
	const karlRow = `SELECT display_name || '|' || raw_value || '|' || updated_at_ms FROM identities WHERE canonical_value = ?`
	before := hpQueryString(t, h, karlRow, hwtKarl)

	h.historyMessage(t, group, hwtMessage{
		id: "hp-identity-1", conversation: "hp-identity", body: "hello", from: hwtKarl, name: "Karl (old phonebook)",
		at: hwtStart.Add(-time.Hour), reactions: []hwtReaction{{"👍", hwtKarl}},
	}.proto())
	h.pump(t)
	if counts := h.counts(); counts.HistoryImported != 1 || counts.ReactionsApplied != 1 {
		t.Fatalf("counters = %+v, want the message and its reaction imported", counts)
	}
	if after := hpQueryString(t, h, karlRow, hwtKarl); after != before {
		t.Fatalf("known identity rewritten by history: %q -> %q", before, after)
	}

	// An unknown sender is created, with the fetched name.
	h.historyMessage(t, group, hwtMessage{
		id: "hp-identity-2", conversation: "hp-identity", body: "hi all", from: hwtBea, at: hwtStart.Add(-30 * time.Minute),
	}.proto())
	h.pump(t)
	if got := hpQueryString(t, h, `SELECT display_name FROM identities WHERE canonical_value = ?`, hwtBea); got != hwtNames[hwtBea] {
		t.Fatalf("new identity display name = %q, want %q", got, hwtNames[hwtBea])
	}
}

// A message v2 holds under its wire identity is skipped before projection even
// after its remote conversation ID has been re-bound to another thread, so the
// re-fetch reaches neither the outbox echo observer nor the placement logic.
func TestHistoryPlacementSkipsMessageWhoseThreadWasRebound(t *testing.T) {
	h := newHWTHarness(t, false)
	h.liveConversation(t, hwtConversation("hp-moved", "Ada", false, hwtAda))
	sent := hwtMessage{id: "hp-moved-1", conversation: "hp-moved", body: "sent", tmpID: "tmp-hp-moved", at: hwtStart.Add(-time.Hour)}
	h.liveMessage(t, sent.proto())
	h.pump(t)
	original := h.conversation(t, "hp-moved")
	echoesBefore := h.echoCalls()

	// The remote ID now names a different thread (as after an id-space reset).
	h.exec(t, `UPDATE conversations SET remote_conversation_id = 'hp-moved-old' WHERE conversation_id = ?`, original.ConversationID)
	h.liveConversation(t, hwtConversation("hp-moved", "Bea", false, hwtBea))
	h.pump(t)
	if rebound := h.conversation(t, "hp-moved"); rebound.ConversationID == original.ConversationID {
		t.Fatal("fixture: the remote ID is still bound to the original thread")
	}

	h.historyMessage(t, nil, sent.proto())
	h.pump(t)
	counts := h.counts()
	if counts.HistoryExisting != 1 || counts.HistoryImported != 0 || counts.HistorySkipped != 0 {
		t.Fatalf("counters = %+v, want the re-fetched message counted as existing", counts)
	}
	if got := h.echoCalls(); got != echoesBefore {
		t.Fatalf("echo observer calls = %d, want %d (an existing message must not reach the outbox)", got, echoesBefore)
	}
	if n := h.messageRows(t, "hp-moved-1"); n != 1 {
		t.Fatalf("message rows = %d, want the single original", n)
	}
}

// History's writes are separate commits. If a transient storage error hits
// after the message insert, the retry must finish the reactions, not find its
// own row and skip the frame. The same holds for a thread whose row committed
// before its roster.
func TestHistoryPlacementRetryFinishesWhatAnEarlierAttemptCommitted(t *testing.T) {
	ctx := context.Background()

	t.Run("message then reactions", func(t *testing.T) {
		for _, withProgress := range []bool{true, false} {
			h := newHWTHarness(t, false)
			group := hwtConversation("hp-retry", "Family", true, hwtKarl, hwtShoshana)
			h.liveConversation(t, group)
			h.pump(t)
			record := h.historyMessage(t, group, hwtMessage{
				id: "hp-retry-1", conversation: "hp-retry", body: "with a reaction", from: hwtKarl,
				at: hwtStart.Add(-time.Hour), reactions: []hwtReaction{{"👍", hwtShoshana}},
			}.proto())
			inboxID := hpQueryString(t, h, `SELECT inbox_id FROM inbox WHERE dedupe_key = ?`, record.DedupeKey)
			record.ReceivedAt = h.clock.Now()

			// Attempt 1 gets as far as the message insert.
			events, err := NewGoogleDecoder(h.counters).Decode(ctx, record)
			if err != nil {
				t.Fatalf("Decode(): %v", err)
			}
			var event *bridge.MessageEvent
			for index := range events {
				if events[index].Kind == bridge.EventMessage {
					event = events[index].Message
				}
			}
			if event == nil {
				t.Fatal("fixture: the frame decoded to no message")
			}
			progress := &historyFrameProgress{}
			if _, inserted, err := h.worker.historyMessage(ctx, i01AccountID, bridge.PlatformGoogle, inboxID, *event, true, progress); err != nil || !inserted {
				t.Fatalf("attempt 1 historyMessage() = (inserted %v, %v), want the insert", inserted, err)
			}

			// Attempt 2 runs the whole frame again.
			retry := progress
			if !withProgress {
				retry = nil // what a retry would see without the progress record
			}
			if _, err := h.worker.processRecordAttempt(ctx, inboxID, record, retry); err != nil {
				t.Fatalf("attempt 2: %v", err)
			}
			stored := h.message(t, "hp-retry", "hp-retry-1")
			reactions := i01QueryInt64(t, h.path, `SELECT COUNT(*) FROM reactions WHERE message_id = ?`, stored.MessageID)
			counts := h.counts()
			if withProgress {
				if reactions != 1 || counts.HistoryImported != 1 || counts.HistoryExisting != 0 {
					t.Fatalf("with progress: reactions=%d counters=%+v, want the reaction applied and one import", reactions, counts)
				}
			} else if reactions != 0 || counts.HistoryExisting != 1 {
				// The contrast that makes the progress record necessary.
				t.Fatalf("without progress: reactions=%d counters=%+v, want the retry to skip its own row", reactions, counts)
			}
		}
	})

	t.Run("thread row then roster", func(t *testing.T) {
		for _, withProgress := range []bool{true, false} {
			h := newHWTHarness(t, false)
			snapshot := hwtConversation("hp-retry-thread", "New circle", true, hwtAda, hwtBea)
			event := googleConversationEvent(snapshot)

			// Attempt 1 committed the thread row and failed before its roster.
			progress := &historyFrameProgress{}
			now := h.clock.Now().UnixMilli()
			if err := h.store.UpsertConversation(sqlite.Conversation{
				ConversationID:       "hp-retry-thread-row",
				AccountID:            i01AccountID,
				RemoteConversationID: "hp-retry-thread",
				Kind:                 sqlite.ConversationKindGroup,
				Title:                "New circle",
				NotificationMode:     sqlite.NotificationModeAll,
				MetadataJSON:         "{}",
				CreatedAtMS:          now,
				UpdatedAtMS:          now,
			}); err != nil {
				t.Fatalf("UpsertConversation(): %v", err)
			}
			progress.setConversation("hp-retry-thread", false)

			retry := progress
			if !withProgress {
				retry = nil
			}
			created, err := h.worker.applyHistoryConversation(i01AccountID, bridge.PlatformGoogle, *event.Conversation, retry)
			if err != nil {
				t.Fatalf("attempt 2 applyHistoryConversation(): %v", err)
			}
			peers := h.peers(t, "hp-retry-thread-row")
			if withProgress {
				if !created || len(peers) != 2 {
					t.Fatalf("with progress: created=%v peers=%v, want the roster finished", created, peers)
				}
				// A third attempt finds it finished and writes nothing more.
				if again, err := h.worker.applyHistoryConversation(i01AccountID, bridge.PlatformGoogle, *event.Conversation, retry); err != nil || again {
					t.Fatalf("attempt 3 = (created %v, %v), want it to recognise the finished thread", again, err)
				}
			} else if created || len(peers) != 0 {
				t.Fatalf("without progress: created=%v peers=%v, want the bound thread left as it is", created, peers)
			}
		}
	})
}

func sameRows(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for index := range a {
		if a[index] != b[index] {
			return false
		}
	}
	return true
}

func containsAll(have, want []string) bool {
	set := make(map[string]struct{}, len(have))
	for _, value := range have {
		set[value] = struct{}{}
	}
	for _, value := range want {
		if _, ok := set[value]; !ok {
			return false
		}
	}
	return true
}
