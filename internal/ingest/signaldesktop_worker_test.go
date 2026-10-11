package ingest

// Signal Desktop history invariants, end to end through the sink and worker:
//
//	S1 idempotence: feeding a frame N times leaves the rows of feeding it once.
//	S2 insert-only: existing messages, identities and participants are never
//	   rewritten; a conversation changes only by the monotone recency bump.
//	S3 order independence: any order of a capture's frames gives the same rows.
//	S4 live wins: a live frame for a message history inserted updates that row
//	   instead of adding one, and history after live is a no-op.
//	S5 roster: no thread is created for an ID whose roster an existing thread
//	   already answers to.
//	S6 reactions: history applies reactions only to messages it inserted.
//	S7 sends: a history row for a send the outbox has not confirmed merges into
//	   the local row on confirm, so concurrency with a send leaves one row.
//
// Feeder gating and capture belong to cmd and internal/signaldesktop. Every
// address, name and body is synthetic.

import (
	"context"
	"fmt"
	"math/rand"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
	"github.com/maxghenis/openmessage/internal/v2keys"
	"github.com/rs/zerolog"
)

type sdhHarness struct {
	*i01Harness
	now      time.Time
	reaction *sqlite.ReactionRepository
}

func newSDHHarness(t *testing.T) *sdhHarness {
	t.Helper()
	path := filepath.Join(t.TempDir(), "signal-desktop-history.sqlite3")
	store, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	base := time.UnixMilli(sdhBaseMS)
	if err := store.UpsertAccount(sqlite.Account{AccountID: sdhAccountID, BridgeKey: "signal_cli", DisplayName: "synthetic account", Mode: sqlite.AccountModeLive, Enabled: true, ConfigJSON: "{}", CreatedAtMS: base.UnixMilli(), UpdatedAtMS: base.UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	h := &sdhHarness{now: base}
	now := func() time.Time { return h.now }
	messages, err := sqlite.NewMessageRepository(store, now)
	if err != nil {
		t.Fatal(err)
	}
	reactions, err := sqlite.NewReactionRepository(store, now)
	if err != nil {
		t.Fatal(err)
	}
	counters := &Counters{}
	worker, err := NewWorker(WorkerConfig{
		Store: store, Messages: messages, Reactions: reactions, Counters: counters,
		Logger: zerolog.Nop(), Now: now,
		Decoders: []DecoderRegistration{
			{Codec: SignalJSONRPCCodec, Platform: bridge.PlatformSignal, Decoder: NewSignalDecoder()},
			NewSignalDesktopHistoryDecoderRegistration(),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	h.reaction = reactions
	h.i01Harness = &i01Harness{path: path, store: store, messages: messages, counters: counters, worker: worker, sink: i01NewSink(t, messages, worker, counters, "synthetic-signal-desktop-inbox")}
	return h
}

func (h *sdhHarness) append(t *testing.T, record bridge.RawIngressRecord, history bool) {
	t.Helper()
	var err error
	if history {
		err = h.sink.AppendHistoryIngress(context.Background(), record)
	} else {
		err = h.sink.AppendIngress(context.Background(), record)
	}
	if err != nil {
		t.Fatal(err)
	}
	for {
		select {
		case item := <-h.worker.work:
			if item.replay {
				h.worker.handleRecord(context.Background(), item.inboxID, item.record)
			}
			h.worker.drain(context.Background())
		default:
			i01AssertNoPending(t, h.messages)
			if c := h.counts(); c.Quarantined != 0 {
				t.Fatalf("quarantined=%d", c.Quarantined)
			}
			return
		}
	}
}

func (h *sdhHarness) live(t *testing.T, conversationID string, message SignalDesktopHistoryMessage) {
	t.Helper()
	line, source, destination := sdhLiveLine(sdhSelf, conversationID, message)
	record, ephemeral, err := BuildSignalIngress(sdhAccountID, 1, sdhSelf, line, source, destination, h.now)
	if err != nil || ephemeral != nil || record == nil {
		t.Fatalf("BuildSignalIngress: record=%v ephemeral=%v err=%v", record, ephemeral, err)
	}
	h.append(t, *record, false)
}

func (h *sdhHarness) counts() CounterSnapshot { return h.counters.Snapshot(sdhAccountID) }

func (h *sdhHarness) rows(t *testing.T, query string) []string {
	t.Helper()
	_, rows := hwtQueryRows(t, h.path, query)
	return rows
}

func (h *sdhHarness) state(t *testing.T) map[string][]string {
	t.Helper()
	state := map[string][]string{}
	for _, table := range []string{"conversations", "identities", "conversation_participants", "messages", "message_attachments", "reactions"} {
		state[table] = h.rows(t, "SELECT * FROM "+table+" ORDER BY 1, 2")
	}
	return state
}

func (h *sdhHarness) conversation(t *testing.T, remoteID string) sqlite.Conversation {
	t.Helper()
	conversation, err := h.store.GetConversationByRemote(sdhAccountID, remoteID)
	if err != nil {
		t.Fatalf("GetConversationByRemote(%s): %v", remoteID, err)
	}
	return conversation
}

func (h *sdhHarness) message(t *testing.T, conversationRemoteID, remoteMessageID string) sqlite.Message {
	t.Helper()
	conversation := h.conversation(t, conversationRemoteID)
	message, err := h.messages.GetMessageByRemote(context.Background(), sdhAccountID, conversation.ConversationID, remoteMessageID)
	if err != nil {
		t.Fatalf("GetMessageByRemote: %v", err)
	}
	return message
}

func sdhDirectSnapshot() SignalDesktopHistorySnapshot {
	return sdhDirect(sdhPeerACI,
		SignalDesktopHistoryMessage{AuthorAddress: sdhPeerACI, AuthorName: "Synthetic Peer", SentAtMS: sdhBaseMS + 1000, Body: "synthetic incoming"},
		SignalDesktopHistoryMessage{Outgoing: true, SentAtMS: sdhBaseMS + 2000, Body: "synthetic outgoing"},
	)
}

func TestSignalDesktopHistoryWorkerS1IdempotenceAndReplayNoOp(t *testing.T) {
	h := newSDHHarness(t)
	record := sdhRecord(t, sdhDirectSnapshot())
	h.append(t, record, true)
	before := h.state(t)
	first := h.counts()
	h.now = h.now.Add(time.Hour)
	for i := 0; i < 5; i++ {
		h.append(t, record, true)
	}
	if after := h.state(t); !reflect.DeepEqual(before, after) {
		t.Fatalf("replay changed rows: before=%v after=%v", before, after)
	}
	c := h.counts()
	if first.HistoryConversations != 1 || first.HistoryImported != 2 || c.HistoryImported != 2 ||
		c.HistoryDeduped != 5 || c.HistoryExisting != 10 || c.Appended != 0 || c.Projected != 0 || c.Imported != 0 {
		t.Fatalf("counts=%+v first=%+v", c, first)
	}
	if got := h.rows(t, "SELECT codec FROM inbox"); len(got) != 1 {
		t.Fatalf("inbox rows = %v, want one history frame", got)
	}
}

func TestSignalDesktopHistoryWorkerS2ExistingRowsRemainByteIdentical(t *testing.T) {
	h := newSDHHarness(t)
	snapshot := sdhDirectSnapshot()
	h.append(t, sdhRecord(t, snapshot), true)
	// The group's other member is known from live delivery, with a name.
	h.live(t, "signal-group:"+sdhGroupID, SignalDesktopHistoryMessage{AuthorAddress: sdhOtherACI, AuthorName: "Live Name", SentAtMS: sdhBaseMS + 100, Body: "synthetic live"})
	before := h.state(t)
	h.now = h.now.Add(time.Hour)
	changed := snapshot
	changed.Title = "changed title"
	changed.Participants = []SignalDesktopHistoryParticipant{{Address: sdhPeerACI, Name: "Changed Name"}}
	changed.Messages = []SignalDesktopHistoryMessage{
		{AuthorAddress: sdhPeerACI, AuthorName: "Changed Name", SentAtMS: sdhBaseMS + 1000, Body: "changed body"},
		{Outgoing: true, SentAtMS: sdhBaseMS + 2000, Body: "changed outgoing"},
	}
	h.append(t, sdhRecord(t, changed), true)
	if after := h.state(t); !reflect.DeepEqual(before, after) {
		t.Fatalf("history rewrote existing rows: before=%v after=%v", before, after)
	}
	if c := h.counts(); c.HistoryExisting != 2 || c.HistoryImported != 2 {
		t.Fatalf("counts=%+v", c)
	}

	// A history gap message whose sender, participants and reactors are all
	// known identities, under other names, rewrites none of them.
	group := sdhGroup(SignalDesktopHistoryMessage{AuthorAddress: sdhOtherACI, AuthorName: "History Name", SentAtMS: sdhBaseMS + 200, Body: "synthetic group gap",
		Reactions: []SignalDesktopHistoryReaction{{ActorAddress: sdhOtherACI, ActorName: "History Name", Emoji: "👍"}, {ActorAddress: sdhPeerACI, ActorName: "History Peer", Emoji: "❤️"}}})
	group.Participants = []SignalDesktopHistoryParticipant{{Address: sdhPeerACI, Name: "History Peer"}, {Address: sdhOtherACI, Name: "History Name"}}
	h.now = h.now.Add(time.Hour)
	h.append(t, sdhRecord(t, group), true)
	if afterGroup := h.state(t); !reflect.DeepEqual(before["identities"], afterGroup["identities"]) {
		t.Fatalf("history rewrote known identities:\nbefore=%v\nafter=%v", before["identities"], afterGroup["identities"])
	}
	if c := h.counts(); c.HistoryImported != 3 || c.ReactionsApplied != 2 {
		t.Fatalf("counts=%+v", c)
	}
	before = h.state(t)

	// A gap message inserts; the thread changes only by the recency bump.
	beforeConversation := h.conversation(t, snapshot.RemoteConversationID)
	changed.Messages = []SignalDesktopHistoryMessage{{AuthorAddress: sdhPeerACI, AuthorName: "Changed Name", SentAtMS: sdhBaseMS + 9000, Body: "synthetic gap"}}
	h.append(t, sdhRecord(t, changed), true)
	after := h.state(t)
	afterConversation := h.conversation(t, snapshot.RemoteConversationID)
	if afterConversation.LastMessageAtMS != sdhBaseMS+9000 {
		t.Fatalf("recency = %d, want the gap message's time", afterConversation.LastMessageAtMS)
	}
	afterConversation.LastMessageAtMS = beforeConversation.LastMessageAtMS
	if !reflect.DeepEqual(beforeConversation, afterConversation) {
		t.Fatalf("gap insertion rewrote the conversation: before=%+v after=%+v", beforeConversation, afterConversation)
	}
	for _, table := range []string{"identities", "conversation_participants"} {
		if !reflect.DeepEqual(before[table], after[table]) {
			t.Fatalf("gap insertion rewrote %s", table)
		}
	}
	if len(after["messages"]) != 5 {
		t.Fatalf("messages = %d, want 5", len(after["messages"]))
	}
	for _, row := range before["messages"] {
		found := false
		for _, candidate := range after["messages"] {
			found = found || candidate == row
		}
		if !found {
			t.Fatalf("existing message row changed: %s", row)
		}
	}
	// An older gap never moves recency backwards.
	changed.Messages = []SignalDesktopHistoryMessage{{AuthorAddress: sdhPeerACI, SentAtMS: sdhBaseMS + 500, Body: "synthetic older gap"}}
	h.append(t, sdhRecord(t, changed), true)
	if got := h.conversation(t, snapshot.RemoteConversationID).LastMessageAtMS; got != sdhBaseMS+9000 {
		t.Fatalf("older gap moved recency to %d", got)
	}
}

func TestSignalDesktopHistoryWorkerNewThreadsUseLiveIdentities(t *testing.T) {
	for _, tc := range []struct {
		name     string
		snapshot SignalDesktopHistorySnapshot
		kind     sqlite.ConversationKind
		peers    []string
	}{
		{name: "direct e164", snapshot: sdhDirect(sdhPeerE164, SignalDesktopHistoryMessage{AuthorAddress: sdhPeerE164, SentAtMS: sdhBaseMS + 1, Body: "synthetic"}), kind: sqlite.ConversationKindDirect, peers: []string{"e164:" + sdhPeerE164}},
		{name: "direct aci", snapshot: sdhDirect(sdhPeerACI, SignalDesktopHistoryMessage{AuthorAddress: sdhPeerACI, SentAtMS: sdhBaseMS + 1, Body: "synthetic"}), kind: sqlite.ConversationKindDirect, peers: []string{"signal_aci:" + sdhPeerACI}},
		{name: "group", snapshot: sdhGroup(SignalDesktopHistoryMessage{AuthorAddress: sdhOtherACI, SentAtMS: sdhBaseMS + 1, Body: "synthetic"}), kind: sqlite.ConversationKindGroup, peers: []string{"e164:" + sdhPeerE164, "signal_aci:" + sdhOtherACI}},
		{name: "note to self", snapshot: SignalDesktopHistorySnapshot{
			RemoteConversationID: "signal:" + sdhSelf,
			Participants:         []SignalDesktopHistoryParticipant{{Address: sdhSelf, IsSelf: true}},
			Messages:             []SignalDesktopHistoryMessage{{Outgoing: true, SentAtMS: sdhBaseMS + 1, Body: "synthetic note"}},
		}, kind: sqlite.ConversationKindDirect},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newSDHHarness(t)
			h.append(t, sdhRecord(t, tc.snapshot), true)
			conversation := h.conversation(t, tc.snapshot.RemoteConversationID)
			if conversation.Kind != tc.kind || conversation.Title != tc.snapshot.Title || conversation.LastMessageAtMS != sdhBaseMS+1 {
				t.Fatalf("conversation = %+v", conversation)
			}
			peers, err := h.store.ListConversationPeerIdentities(sdhAccountID, conversation.ConversationID)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, peer := range peers {
				got = append(got, string(peer.Kind)+":"+peer.CanonicalValue)
			}
			if fmt.Sprint(got) != fmt.Sprint(tc.peers) {
				t.Fatalf("peers = %v, want %v", got, tc.peers)
			}
			if c := h.counts(); c.HistoryConversations != 1 || c.HistoryImported != 1 || c.HistorySkipped != 0 {
				t.Fatalf("counts=%+v", c)
			}
		})
	}
}

func TestSignalDesktopHistoryWorkerS4LiveWinsWithoutDuplicate(t *testing.T) {
	for _, tc := range []struct {
		name     string
		snapshot SignalDesktopHistorySnapshot
		remoteID func(SignalDesktopHistorySnapshot) string
	}{
		{name: "direct incoming", snapshot: sdhDirect(sdhPeerACI, SignalDesktopHistoryMessage{AuthorAddress: sdhPeerACI, SentAtMS: sdhBaseMS + 1, Body: "synthetic history body"}),
			remoteID: func(s SignalDesktopHistorySnapshot) string {
				return v2keys.SignalIncomingSourceID(s.RemoteConversationID, sdhPeerACI, sdhBaseMS+1)
			}},
		{name: "direct outgoing", snapshot: sdhDirect(sdhPeerE164, SignalDesktopHistoryMessage{Outgoing: true, SentAtMS: sdhBaseMS + 1, Body: "synthetic history body"}),
			remoteID: func(SignalDesktopHistorySnapshot) string { return strconv.FormatInt(sdhBaseMS+1, 10) }},
		{name: "group incoming", snapshot: sdhGroup(SignalDesktopHistoryMessage{AuthorAddress: sdhOtherACI, SentAtMS: sdhBaseMS + 1, Body: "synthetic history body"}),
			remoteID: func(s SignalDesktopHistorySnapshot) string {
				return v2keys.SignalIncomingSourceID(s.RemoteConversationID, sdhOtherACI, sdhBaseMS+1)
			}},
	} {
		for _, liveFirst := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/live_first_%t", tc.name, liveFirst), func(t *testing.T) {
				h := newSDHHarness(t)
				liveMessage := tc.snapshot.Messages[0]
				liveMessage.Body = "synthetic live body"
				if liveFirst {
					h.live(t, tc.snapshot.RemoteConversationID, liveMessage)
					before := h.state(t)
					h.now = h.now.Add(time.Hour)
					h.append(t, sdhRecord(t, tc.snapshot), true)
					after := h.state(t)
					if !reflect.DeepEqual(before["messages"], after["messages"]) || !reflect.DeepEqual(before["identities"], after["identities"]) {
						t.Fatal("history changed live rows")
					}
					if h.counts().HistoryExisting != 1 || h.counts().HistoryImported != 0 {
						t.Fatalf("counts=%+v", h.counts())
					}
				} else {
					h.append(t, sdhRecord(t, tc.snapshot), true)
					h.now = h.now.Add(time.Hour)
					h.live(t, tc.snapshot.RemoteConversationID, liveMessage)
				}
				message := h.message(t, tc.snapshot.RemoteConversationID, tc.remoteID(tc.snapshot))
				if message.Body != "synthetic live body" {
					t.Fatalf("message body = %q, want the live body", message.Body)
				}
				if rows := h.rows(t, "SELECT * FROM messages"); len(rows) != 1 {
					t.Fatalf("messages = %d, want 1", len(rows))
				}
				if rows := h.rows(t, "SELECT * FROM conversations"); len(rows) != 1 {
					t.Fatalf("conversations = %d, want 1", len(rows))
				}
			})
		}
	}
}

// A migrated outgoing row keyed by the legacy local alias is the same message
// as Desktop's sync transcript; history must not add a second row beside it.
func TestSignalDesktopHistoryWorkerOutgoingAliasIsExisting(t *testing.T) {
	h := newSDHHarness(t)
	snapshot := sdhDirect(sdhPeerE164, SignalDesktopHistoryMessage{AuthorAddress: sdhPeerE164, SentAtMS: sdhBaseMS + 1, Body: "synthetic first"})
	h.append(t, sdhRecord(t, snapshot), true)
	conversation := h.conversation(t, snapshot.RemoteConversationID)
	alias := v2keys.SignalLocalAlias(snapshot.RemoteConversationID, sdhBaseMS+2)
	if err := h.messages.ImportMessage(context.Background(), sqlite.MessageProjection{Message: sqlite.Message{
		MessageID:       v2keys.DeriveID("message", sdhAccountID, snapshot.RemoteConversationID+"\x1f"+alias),
		ConversationID:  conversation.ConversationID,
		AccountID:       sdhAccountID,
		RemoteMessageID: alias,
		Direction:       sqlite.MessageDirectionOutgoing,
		Body:            "synthetic migrated",
		State:           sqlite.MessageStateActive,
		OccurredAtMS:    sdhBaseMS + 2,
	}}); err != nil {
		t.Fatal(err)
	}
	before := h.state(t)
	snapshot.Messages = []SignalDesktopHistoryMessage{{Outgoing: true, SentAtMS: sdhBaseMS + 2, Body: "synthetic desktop copy"}}
	h.append(t, sdhRecord(t, snapshot), true)
	if after := h.state(t); !reflect.DeepEqual(before["messages"], after["messages"]) {
		t.Fatalf("history duplicated an aliased outgoing message")
	}
	if c := h.counts(); c.HistoryExisting != 1 || c.HistoryImported != 1 {
		t.Fatalf("counts=%+v", c)
	}
}

func TestSignalDesktopHistoryWorkerS5NoThreadForAnAnsweredRoster(t *testing.T) {
	t.Run("group", func(t *testing.T) {
		h := newSDHHarness(t)
		original := sdhGroup(SignalDesktopHistoryMessage{AuthorAddress: sdhOtherACI, SentAtMS: sdhBaseMS + 1, Body: "synthetic"})
		h.append(t, sdhRecord(t, original), true)
		before := h.state(t)
		other := original
		other.RemoteConversationID = "signal-group:b3RoZXItc3ludGhldGljLWdyb3VwLWlkLWZvci10ZXN0cw=="
		other.Messages = []SignalDesktopHistoryMessage{{AuthorAddress: sdhOtherACI, SentAtMS: sdhBaseMS + 2, Body: "synthetic other"}}
		h.append(t, sdhRecord(t, other), true)
		if after := h.state(t); !reflect.DeepEqual(before, after) {
			t.Fatal("history created a thread for a roster an existing thread answers to")
		}
		if c := h.counts(); c.HistorySkipped != 2 || c.HistoryConversations != 1 {
			t.Fatalf("counts=%+v", c)
		}
	})
	t.Run("direct", func(t *testing.T) {
		h := newSDHHarness(t)
		// A thread bound under another ID whose peer is this person, as a
		// migration can leave, then Desktop history for the canonical ID.
		first := sdhDirect(sdhPeerACI, SignalDesktopHistoryMessage{AuthorAddress: sdhPeerACI, SentAtMS: sdhBaseMS + 1, Body: "synthetic"})
		nowMS := h.now.UnixMilli()
		identityID := v2keys.DeriveID("identity", sdhAccountID, "signal_aci\x1f"+sdhPeerACI)
		if err := h.store.UpsertIdentity(sqlite.Identity{IdentityID: identityID, AccountID: sdhAccountID, Kind: "signal_aci", CanonicalValue: sdhPeerACI, RawValue: sdhPeerACI, MetadataJSON: "{}", CreatedAtMS: nowMS, UpdatedAtMS: nowMS}); err != nil {
			t.Fatal(err)
		}
		legacy := sqlite.Conversation{ConversationID: "conversation-legacy-synthetic", AccountID: sdhAccountID, RemoteConversationID: "signal:legacy-synthetic-thread", Kind: sqlite.ConversationKindDirect, NotificationMode: sqlite.NotificationModeAll, MetadataJSON: "{}", CreatedAtMS: nowMS, UpdatedAtMS: nowMS}
		if err := h.store.UpsertConversation(legacy); err != nil {
			t.Fatal(err)
		}
		if err := h.store.ReplaceConversationParticipants(legacy.ConversationID, []sqlite.ConversationParticipant{{AccountID: sdhAccountID, ConversationID: legacy.ConversationID, IdentityID: identityID, Role: sqlite.ParticipantRoleMember, IsActive: true}}); err != nil {
			t.Fatal(err)
		}
		before := h.state(t)
		h.append(t, sdhRecord(t, first), true)
		if after := h.state(t); !reflect.DeepEqual(before, after) {
			t.Fatal("history created a second thread for the same peer")
		}
		if c := h.counts(); c.HistorySkipped != 2 || c.HistoryConversations != 0 {
			t.Fatalf("counts=%+v", c)
		}
	})
}

func TestSignalDesktopHistoryWorkerS6ReactionsOnlyOnInsertedMessages(t *testing.T) {
	h := newSDHHarness(t)
	group := sdhGroup()
	// The live channel delivered one message, with no reactions yet.
	liveMessage := SignalDesktopHistoryMessage{AuthorAddress: sdhOtherACI, SentAtMS: sdhBaseMS + 1, Body: "synthetic live"}
	h.live(t, group.RemoteConversationID, liveMessage)
	// History carries that message with a reaction, and a gap message with two.
	liveMessage.Reactions = []SignalDesktopHistoryReaction{{ActorAddress: sdhPeerE164, Emoji: "👍"}}
	group.Messages = []SignalDesktopHistoryMessage{
		liveMessage,
		{AuthorAddress: sdhPeerE164, SentAtMS: sdhBaseMS + 2, Body: "synthetic gap", Reactions: []SignalDesktopHistoryReaction{
			{ActorAddress: sdhOtherACI, Emoji: "😂", TimestampMS: sdhBaseMS + 3},
			{IsSelf: true, Emoji: "❤️", TimestampMS: sdhBaseMS + 4},
		}},
	}
	h.append(t, sdhRecord(t, group), true)
	liveRow := h.message(t, group.RemoteConversationID, v2keys.SignalIncomingSourceID(group.RemoteConversationID, sdhOtherACI, sdhBaseMS+1))
	gapRow := h.message(t, group.RemoteConversationID, v2keys.SignalIncomingSourceID(group.RemoteConversationID, sdhPeerE164, sdhBaseMS+2))
	if rows := h.rows(t, "SELECT * FROM reactions WHERE message_id='"+liveRow.MessageID+"'"); len(rows) != 0 {
		t.Fatalf("history added reactions to a live message: %v", rows)
	}
	if rows := h.rows(t, "SELECT * FROM reactions WHERE message_id='"+gapRow.MessageID+"' AND state='active'"); len(rows) != 2 {
		t.Fatalf("gap message reactions = %v, want 2", rows)
	}
	before := h.state(t)
	h.append(t, sdhRecord(t, group), true)
	if after := h.state(t); !reflect.DeepEqual(before, after) {
		t.Fatal("replaying reactions changed rows")
	}
}

// S7: Desktop can hold the sync transcript of a send before the outbox has
// confirmed it (only if the feed runs while signal-cli sends). The confirm
// merges that row into the local one, as it does for an early echo (#218).
func TestSignalDesktopHistoryWorkerS7HistoryBeforeConfirmMergesIntoLocalRow(t *testing.T) {
	ctx := context.Background()
	h := newSDHHarness(t)
	snapshot := sdhDirect(sdhPeerE164, SignalDesktopHistoryMessage{AuthorAddress: sdhPeerE164, SentAtMS: sdhBaseMS + 1, Body: "synthetic"})
	h.append(t, sdhRecord(t, snapshot), true)
	conversation := h.conversation(t, snapshot.RemoteConversationID)
	outbox, err := sqlite.NewOutboxRepository(h.store, func() time.Time { return h.now })
	if err != nil {
		t.Fatal(err)
	}
	sentAt := sdhBaseMS + 50
	item := sqlite.NewOutboxItem{
		OutboxID: "outbox-synthetic", AccountID: sdhAccountID, ConversationID: conversation.ConversationID,
		Kind: sqlite.OutboxKindText, IdempotencyKey: "idempotency-synthetic", PayloadHash: "payload-synthetic",
		Operation: "send_text", LocalMessageID: "message-local-synthetic", TransportRequestID: "request-synthetic",
	}
	if _, _, err := outbox.EnqueueOutgoingMessage(ctx, item, sqlite.Message{
		MessageID: item.LocalMessageID, ConversationID: conversation.ConversationID, AccountID: sdhAccountID,
		RemoteMessageID: item.TransportRequestID, Direction: sqlite.MessageDirectionOutgoing,
		Body: "synthetic send", State: sqlite.MessageStateActive, OccurredAtMS: sentAt,
	}); err != nil {
		t.Fatal(err)
	}
	leases, err := outbox.LeaseDue(ctx, sqlite.LeaseRequest{Owner: "synthetic", Now: h.now, Duration: time.Minute, Limit: 1})
	if err != nil || len(leases) != 1 || leases[0].LeaseToken == nil {
		t.Fatalf("LeaseDue: %v %v", leases, err)
	}
	token := *leases[0].LeaseToken
	if err := outbox.MarkTransportCalled(ctx, sqlite.Attempt{OutboxID: item.OutboxID, LeaseToken: token}); err != nil {
		t.Fatal(err)
	}
	// Desktop already has the transcript; the feed inserts it.
	snapshot.Messages = []SignalDesktopHistoryMessage{{Outgoing: true, SentAtMS: sentAt, Body: "synthetic send"}}
	h.append(t, sdhRecord(t, snapshot), true)
	if c := h.counts(); c.HistoryImported != 2 {
		t.Fatalf("counts=%+v", c)
	}
	if err := outbox.Confirm(ctx, sqlite.Confirmation{OutboxID: item.OutboxID, LeaseToken: token, ResultRemoteID: strconv.FormatInt(sentAt, 10)}); err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	rows := h.rows(t, "SELECT message_id, remote_message_id FROM messages WHERE direction='outgoing'")
	if len(rows) != 1 {
		t.Fatalf("outgoing rows = %v, want the local row only", rows)
	}
	local, err := h.messages.GetMessage(ctx, item.LocalMessageID)
	if err != nil || local.RemoteMessageID != strconv.FormatInt(sentAt, 10) {
		t.Fatalf("local row = %+v err=%v", local, err)
	}
	// And afterwards history is a no-op for it.
	h.append(t, sdhRecord(t, snapshot), true)
	if rows := h.rows(t, "SELECT * FROM messages WHERE direction='outgoing'"); len(rows) != 1 {
		t.Fatalf("history re-added a confirmed send: %v", rows)
	}
}

func TestSignalDesktopHistoryWorkerS3OrderIndependenceProperty(t *testing.T) {
	random := rand.New(rand.NewSource(20261011))
	categories := make([]int, 8)
	for caseIndex := 0; caseIndex < 24; caseIndex++ {
		category := int((random.Uint64() >> 33) % 8)
		categories[category]++
		group := category&1 != 0
		withOutgoing := category&2 != 0
		withReactions := category&4 != 0
		var frames []bridge.RawIngressRecord
		for chat := 0; chat < 2; chat++ {
			peer := fmt.Sprintf("+1555555020%d", chat)
			snapshot := sdhDirect(peer)
			if group {
				snapshot = SignalDesktopHistorySnapshot{
					RemoteConversationID: fmt.Sprintf("signal-group:c3ludGhldGljLWdyb3Vw%dA==", chat),
					Title:                fmt.Sprintf("Synthetic Group %d", chat),
					Participants:         []SignalDesktopHistoryParticipant{{Address: peer}, {Address: sdhOtherACI}},
				}
			}
			for chunk := 0; chunk < 2; chunk++ {
				message := SignalDesktopHistoryMessage{AuthorAddress: peer, SentAtMS: sdhBaseMS + int64(chunk*1000+chat), Body: fmt.Sprintf("synthetic %d/%d/%d", caseIndex, chat, chunk)}
				if withOutgoing && chunk == 1 {
					message = SignalDesktopHistoryMessage{Outgoing: true, SentAtMS: message.SentAtMS, Body: message.Body}
				}
				if withReactions {
					message.Reactions = []SignalDesktopHistoryReaction{{IsSelf: true, Emoji: "👍"}, {ActorAddress: peer, Emoji: "❤️"}}
				}
				frame := snapshot
				frame.Messages = []SignalDesktopHistoryMessage{message}
				frames = append(frames, sdhRecord(t, frame))
			}
		}
		first := newSDHHarness(t)
		for _, record := range frames {
			first.append(t, record, true)
		}
		want := first.state(t)
		if err := first.store.Close(); err != nil {
			t.Fatal(err)
		}
		random.Shuffle(len(frames), func(i, j int) { frames[i], frames[j] = frames[j], frames[i] })
		second := newSDHHarness(t)
		for _, record := range frames {
			second.append(t, record, true)
		}
		if got := second.state(t); !reflect.DeepEqual(want, got) {
			t.Fatalf("case %d category %d is order-dependent:\nwant=%v\ngot=%v", caseIndex, category, want, got)
		}
		if c := second.counts(); c.HistoryConversations != 2 || c.HistoryImported != 4 || c.HistorySkipped != 0 {
			t.Fatalf("case %d counts=%+v", caseIndex, c)
		}
		before := second.state(t)
		for _, record := range frames {
			second.append(t, record, true)
		}
		if after := second.state(t); !reflect.DeepEqual(before, after) {
			t.Fatalf("case %d replay changed state", caseIndex)
		}
		if err := second.store.Close(); err != nil {
			t.Fatal(err)
		}
	}
	for category, count := range categories {
		if count == 0 {
			t.Fatalf("category %d never exercised", category)
		}
	}
}
