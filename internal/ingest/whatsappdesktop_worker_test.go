package ingest

// Desktop history invariants: I1 replay/idempotence, I2 insert-only existing
// rows, I3 PN/group remote conversation IDs, I4 stanza/timestamp/direction,
// I5 chunk order independence, and I6 later live messages own the same row.
// I7 feeder gating belongs to cmd. All fixtures and bodies are synthetic.

import (
	"context"
	"fmt"
	"math/rand"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
	"github.com/rs/zerolog"
	waE2E "go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"
)

type wadhHarness struct {
	*i01Harness
	now time.Time
}

func newWADHHarness(t *testing.T) *wadhHarness {
	t.Helper()
	path := filepath.Join(t.TempDir(), "desktop-history.sqlite3")
	store, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := store.UpsertAccount(sqlite.Account{AccountID: waWorkerAccountID, BridgeKey: "whatsmeow", DisplayName: "synthetic account", Mode: sqlite.AccountModeLive, Enabled: true, ConfigJSON: "{}", CreatedAtMS: waWorkerNow.UnixMilli(), UpdatedAtMS: waWorkerNow.UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	h := &wadhHarness{now: waWorkerNow}
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
	worker, err := NewWorker(WorkerConfig{Store: store, Messages: messages, Reactions: reactions, Counters: counters, Logger: zerolog.Nop(), Now: now, Decoders: []DecoderRegistration{NewWhatsAppDecoderRegistration(), NewWhatsAppDesktopHistoryDecoderRegistration()}})
	if err != nil {
		t.Fatal(err)
	}
	h.i01Harness = &i01Harness{path: path, store: store, messages: messages, counters: counters, worker: worker, sink: i01NewSink(t, messages, worker, counters, "synthetic-desktop-inbox")}
	return h
}

func (h *wadhHarness) append(t *testing.T, record bridge.RawIngressRecord, history bool) {
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

func (h *wadhHarness) counts() CounterSnapshot { return h.counters.Snapshot(waWorkerAccountID) }

func (h *wadhHarness) conversation(t *testing.T, jid string) sqlite.Conversation {
	t.Helper()
	conversation, err := h.store.GetConversationByRemote(waWorkerAccountID, "whatsapp:"+jid)
	if err != nil {
		t.Fatal(err)
	}
	return conversation
}

func (h *wadhHarness) message(t *testing.T, jid, id string) sqlite.Message {
	t.Helper()
	conversation := h.conversation(t, jid)
	message, err := h.messages.GetMessageByRemote(context.Background(), waWorkerAccountID, conversation.ConversationID, id)
	if err != nil {
		t.Fatal(err)
	}
	return message
}

func (h *wadhHarness) rows(t *testing.T, query string) []string {
	t.Helper()
	_, rows := hwtQueryRows(t, h.path, query)
	return rows
}

func (h *wadhHarness) state(t *testing.T) map[string][]string {
	t.Helper()
	state := map[string][]string{}
	for _, table := range []string{"conversations", "identities", "conversation_participants", "messages", "message_attachments"} {
		state[table] = h.rows(t, "SELECT * FROM "+table+" ORDER BY 1, 2")
	}
	return state
}

func wadhSnapshot(jid string) WhatsAppDesktopHistorySnapshot {
	return WhatsAppDesktopHistorySnapshot{ChatJID: jid, Title: "synthetic chat", Participants: []WhatsAppDesktopHistoryParticipant{{JID: jid}}, Messages: []WhatsAppDesktopHistoryMessage{{StanzaID: "synthetic-in", TimestampMS: 1780000001000, Type: 0, Text: "synthetic incoming"}, {StanzaID: "synthetic-out", TimestampMS: 1780000002000, Type: 0, Text: "synthetic outgoing", IsFromMe: true, SenderJID: "15555550999@s.whatsapp.net"}}}
}

func TestWhatsAppDesktopHistoryWorkerI1IdempotenceAndReplayNoOp(t *testing.T) {
	h := newWADHHarness(t)
	record := wadhRecord(t, wadhSnapshot(waCanonicalChatJID))
	h.append(t, record, true)
	before := h.state(t)
	first := h.counts()
	h.now = h.now.Add(time.Hour)
	for i := 0; i < 5; i++ {
		h.append(t, record, true)
	}
	if after := h.state(t); !reflect.DeepEqual(before, after) {
		t.Fatalf("replay changed projected rows: before=%v after=%v", before, after)
	}
	c := h.counts()
	if first.HistoryConversations != 1 || first.HistoryImported != 2 || c.HistoryImported != 2 || c.HistoryConversations != 1 || c.HistoryDeduped != 5 || c.HistoryExisting != 10 || c.Appended != 0 || c.Projected != 0 {
		t.Fatalf("counts=%+v first=%+v", c, first)
	}
}

func TestWhatsAppDesktopHistoryWorkerI2ExistingRowsRemainByteIdentical(t *testing.T) {
	h := newWADHHarness(t)
	snapshot := wadhSnapshot(waCanonicalChatJID)
	h.append(t, wadhRecord(t, snapshot), true)
	before := h.state(t)
	h.now = h.now.Add(time.Hour)
	snapshot.Title = "changed historical title"
	snapshot.Participants[0].Name = "changed historical name"
	snapshot.Messages[0].Text = "changed historical body"
	snapshot.Messages[0].TimestampMS += 100000
	snapshot.Messages[1].Text = "changed outgoing body"
	h.append(t, wadhRecord(t, snapshot), true)
	if after := h.state(t); !reflect.DeepEqual(before, after) {
		t.Fatalf("existing snapshot rewrote rows: before=%v after=%v", before, after)
	}
	if h.counts().HistoryExisting != 2 {
		t.Fatalf("counts=%+v", h.counts())
	}
	// A missing message may fill the gap while every existing identity,
	// participant and message remains frozen. The conversation changes only by
	// the monotone recency bump to the inserted message's time.
	beforeConversation := h.conversation(t, waCanonicalChatJID)
	snapshot.Messages = []WhatsAppDesktopHistoryMessage{{StanzaID: "new-gap", TimestampMS: 1780000010000, Type: 0, Text: "synthetic gap"}}
	h.append(t, wadhRecord(t, snapshot), true)
	after := h.state(t)
	afterConversation := h.conversation(t, waCanonicalChatJID)
	if afterConversation.LastMessageAtMS != 1780000010000 {
		t.Fatalf("gap insertion recency = %d, want the inserted message time", afterConversation.LastMessageAtMS)
	}
	afterConversation.LastMessageAtMS = beforeConversation.LastMessageAtMS
	if !reflect.DeepEqual(beforeConversation, afterConversation) {
		t.Fatalf("gap insertion rewrote the conversation beyond recency: before=%+v after=%+v", beforeConversation, afterConversation)
	}
	for _, table := range []string{"identities", "conversation_participants", "message_attachments"} {
		if !reflect.DeepEqual(before[table], after[table]) {
			t.Fatalf("gap insertion rewrote %s: before=%v after=%v", table, before[table], after[table])
		}
	}
	for _, id := range []string{"synthetic-in", "synthetic-out"} {
		beforeRow := []string{}
		for _, row := range before["messages"] {
			if strings.Contains(row, "remote_message_id=\""+id+"\" ") {
				beforeRow = append(beforeRow, row)
			}
		}
		afterRow := h.rows(t, fmt.Sprintf("SELECT * FROM messages WHERE remote_message_id='%s'", id))
		if !reflect.DeepEqual(beforeRow, afterRow) {
			t.Fatalf("gap insertion changed existing %s", id)
		}
	}
}

func TestWhatsAppDesktopHistoryWorkerI3I4NewChatAndMessageIdentity(t *testing.T) {
	for _, group := range []bool{false, true} {
		t.Run(fmt.Sprintf("group_%t", group), func(t *testing.T) {
			h := newWADHHarness(t)
			jid := waCanonicalChatJID
			snapshot := wadhSnapshot(jid)
			if group {
				jid = "123456789@g.us"
				snapshot.ChatJID = jid
				snapshot.Participants = []WhatsAppDesktopHistoryParticipant{{JID: waCanonicalChatJID}, {JID: "88888888@lid"}}
				snapshot.Messages[0].SenderJID = "88888888@lid"
			}
			h.append(t, wadhRecord(t, snapshot), true)
			conversation := h.conversation(t, jid)
			if conversation.RemoteConversationID != "whatsapp:"+jid || strings.Contains(conversation.RemoteConversationID, "@lid") || conversation.Title != snapshot.Title || conversation.LastMessageAtMS != 1780000002000 {
				t.Fatalf("conversation=%+v", conversation)
			}
			for _, captured := range snapshot.Messages {
				message := h.message(t, jid, captured.StanzaID)
				direction := sqlite.MessageDirectionIncoming
				if captured.IsFromMe {
					direction = sqlite.MessageDirectionOutgoing
				}
				if message.RemoteMessageID != captured.StanzaID || message.ConversationID != conversation.ConversationID || message.OccurredAtMS != captured.TimestampMS || message.Direction != direction {
					t.Fatalf("message=%+v capture=%+v", message, captured)
				}
			}
			if h.counts().HistoryConversations != 1 || h.counts().HistoryImported != 2 {
				t.Fatalf("counts=%+v", h.counts())
			}
		})
	}
}

func TestWhatsAppDesktopHistoryWorkerDistinctGroupJIDsWithSameRosterBothPlaced(t *testing.T) {
	// A WhatsApp group JID is the chat's identity, so a second group with the
	// same members is a second chat: both are minted, in either order.
	for _, reverse := range []bool{false, true} {
		t.Run(fmt.Sprintf("reverse_%t", reverse), func(t *testing.T) {
			h := newWADHHarness(t)
			first := WhatsAppDesktopHistorySnapshot{ChatJID: "123456789@g.us", Title: "synthetic first", Participants: []WhatsAppDesktopHistoryParticipant{{JID: waCanonicalChatJID}, {JID: waSenderJID}}, Messages: []WhatsAppDesktopHistoryMessage{{StanzaID: "first", SenderJID: waCanonicalChatJID, TimestampMS: 1780000001000, Type: 0, Text: "synthetic first"}}}
			second := first
			second.ChatJID = "987654321@g.us"
			second.Title = "synthetic second"
			second.Messages = []WhatsAppDesktopHistoryMessage{{StanzaID: "second", SenderJID: waSenderJID, TimestampMS: 1780000002000, Type: 0, Text: "synthetic second"}}
			order := []WhatsAppDesktopHistorySnapshot{first, second}
			if reverse {
				order = []WhatsAppDesktopHistorySnapshot{second, first}
			}
			for _, snapshot := range order {
				h.append(t, wadhRecord(t, snapshot), true)
			}
			h.message(t, "123456789@g.us", "first")
			h.message(t, "987654321@g.us", "second")
			if c := h.counts(); c.HistorySkipped != 0 || c.HistoryImported != 2 || c.HistoryConversations != 2 {
				t.Fatalf("counts=%+v", c)
			}
		})
	}
}

// wadhLive appends one live WhatsApp message frame through the real decoder.
func wadhLive(t *testing.T, h *wadhHarness, chatJID, senderJID, stanza string, fromMe bool, timestampMS int64) {
	t.Helper()
	envelope := waBaseMessageEnvelope()
	envelope.ChatJID = chatJID
	envelope.SenderJID = senderJID
	envelope.MessageID = stanza
	envelope.TimestampMS = timestampMS
	envelope.IsFromMe = fromMe
	envelope.IsGroup = strings.HasSuffix(chatJID, "@g.us")
	record, _ := waMessageRecord(t, envelope, &waE2E.Message{Conversation: proto.String("synthetic live " + stanza)})
	record.AccountID = waWorkerAccountID
	record.DedupeKey = "synthetic-live-" + stanza
	h.append(t, record, false)
}

func wadhPeerKinds(t *testing.T, h *wadhHarness, jid string) []string {
	t.Helper()
	conversation := h.conversation(t, jid)
	peers, err := h.store.ListConversationPeerIdentities(waWorkerAccountID, conversation.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, peer := range peers {
		kinds = append(kinds, string(peer.Kind))
	}
	return kinds
}

func TestWhatsAppDesktopHistoryWorkerFillsDirectThreadsWhosePeersAreAliased(t *testing.T) {
	// Live outgoing messages link the direct peer from the remote ID (a jid
	// identity) while live incoming senders are e164 identities, so a real
	// thread can carry {jid}, or {jid, e164}, for one person. Desktop history
	// names the e164 form. The bound remote JID decides placement, so the gap
	// must still fill.
	for _, withIncoming := range []bool{false, true} {
		t.Run(fmt.Sprintf("incoming_%t", withIncoming), func(t *testing.T) {
			h := newWADHHarness(t)
			wadhLive(t, h, waCanonicalChatJID, "", "live-out", true, 1780000001000)
			if withIncoming {
				wadhLive(t, h, waCanonicalChatJID, waCanonicalChatJID, "live-in", false, 1780000002000)
			}
			kinds := wadhPeerKinds(t, h, waCanonicalChatJID)
			if len(kinds) == 0 || (withIncoming && len(kinds) < 2) {
				t.Fatalf("fixture did not produce the aliased roster: peers %v", kinds)
			}
			snapshot := WhatsAppDesktopHistorySnapshot{ChatJID: waCanonicalChatJID, Title: "synthetic chat", Participants: []WhatsAppDesktopHistoryParticipant{{JID: waCanonicalChatJID}}, Messages: []WhatsAppDesktopHistoryMessage{
				{StanzaID: "gap-in", TimestampMS: 1780000010000, Type: 0, Text: "synthetic gap in"},
				{StanzaID: "gap-out", TimestampMS: 1780000011000, Type: 0, Text: "synthetic gap out", IsFromMe: true},
			}}
			h.append(t, wadhRecord(t, snapshot), true)
			h.message(t, waCanonicalChatJID, "gap-in")
			h.message(t, waCanonicalChatJID, "gap-out")
			if c := h.counts(); c.HistorySkipped != 0 || c.HistoryImported != 2 || c.HistoryConversations != 0 {
				t.Fatalf("counts=%+v", c)
			}
			if got := h.conversation(t, waCanonicalChatJID).LastMessageAtMS; got != 1780000011000 {
				t.Fatalf("recency = %d", got)
			}
		})
	}
}

func TestWhatsAppDesktopHistoryWorkerFillsBoundGroupWhoseMembersAreLIDs(t *testing.T) {
	// The live bridge can file group members under LID identities while
	// Desktop resolves the same people to phone numbers; with no overlapping
	// identity the Google-style roster check would call the binding stale.
	const group = "120363000000000001@g.us"
	h := newWADHHarness(t)
	wadhLive(t, h, group, "88888888@lid", "live-group", false, 1780000001000)
	snapshot := WhatsAppDesktopHistorySnapshot{ChatJID: group, Title: "synthetic group", Participants: []WhatsAppDesktopHistoryParticipant{{JID: waCanonicalChatJID}, {JID: waSenderJID}}, Messages: []WhatsAppDesktopHistoryMessage{{StanzaID: "gap-group", SenderJID: waSenderJID, TimestampMS: 1780000010000, Type: 0, Text: "synthetic group gap"}}}
	h.append(t, wadhRecord(t, snapshot), true)
	h.message(t, group, "gap-group")
	if c := h.counts(); c.HistorySkipped != 0 || c.HistoryImported != 1 {
		t.Fatalf("counts=%+v", c)
	}
}

func TestWhatsAppDesktopHistoryWorkerNeverDuplicatesAMessageHeldInAnotherThread(t *testing.T) {
	// A contact's renumbered or aliased chat maps to a different remote ID,
	// but its messages keep their stanza IDs. History must not copy them into
	// a second thread, and must not mint a thread that would only be empty.
	h := newWADHHarness(t)
	wadhLive(t, h, waCanonicalChatJID, waCanonicalChatJID, "held", false, 1780000001000)
	alias := WhatsAppDesktopHistorySnapshot{ChatJID: waSenderJID, Title: "synthetic alias", Participants: []WhatsAppDesktopHistoryParticipant{{JID: waSenderJID}}, Messages: []WhatsAppDesktopHistoryMessage{{StanzaID: "held", TimestampMS: 1780000001000, Type: 0, Text: "synthetic held"}}}
	h.append(t, wadhRecord(t, alias), true)
	if _, err := h.store.GetConversationByRemote(waWorkerAccountID, "whatsapp:"+waSenderJID); err == nil {
		t.Fatal("history minted a thread whose only message lives in another thread")
	}
	if c := h.counts(); c.HistoryExisting != 1 || c.HistoryImported != 0 || c.HistoryConversations != 0 {
		t.Fatalf("counts=%+v", c)
	}
	alias.Messages = append(alias.Messages, WhatsAppDesktopHistoryMessage{StanzaID: "new-after-renumber", TimestampMS: 1780000020000, Type: 0, Text: "synthetic new"})
	h.append(t, wadhRecord(t, alias), true)
	h.message(t, waSenderJID, "new-after-renumber")
	if rows := h.rows(t, "SELECT * FROM messages WHERE remote_message_id='held'"); len(rows) != 1 {
		t.Fatalf("held message rows = %d, want 1", len(rows))
	}
	if c := h.counts(); c.HistoryImported != 1 || c.HistoryConversations != 1 {
		t.Fatalf("counts=%+v", c)
	}
}

func TestWhatsAppDesktopHistoryWorkerSelfThreadKeepsSelfOutOfPeerRoster(t *testing.T) {
	h := newWADHHarness(t)
	snapshot := wadhSnapshot(waCanonicalChatJID)
	snapshot.Participants[0].IsSelf = true
	for index := range snapshot.Messages {
		snapshot.Messages[index].IsFromMe = true
		snapshot.Messages[index].SenderJID = waCanonicalChatJID
	}
	h.append(t, wadhRecord(t, snapshot), true)
	conversation := h.conversation(t, waCanonicalChatJID)
	peers, err := h.store.ListConversationPeerIdentities(waWorkerAccountID, conversation.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	if len(peers) != 0 {
		t.Fatalf("self-thread has non-self peers: %+v", peers)
	}
	identity, err := h.store.GetIdentityByCanonical(waWorkerAccountID, sqlite.IdentityKind("e164"), "+"+strings.Split(waCanonicalChatJID, "@")[0])
	if err != nil {
		t.Fatal(err)
	}
	if !identity.IsSelf {
		t.Fatalf("self-thread identity not marked self: %+v", identity)
	}
}

func TestWhatsAppDesktopHistoryWorkerI6LiveWinsWithoutDuplicate(t *testing.T) {
	for _, liveFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("live_first_%t", liveFirst), func(t *testing.T) {
			h := newWADHHarness(t)
			snapshot := wadhSnapshot(waCanonicalChatJID)
			snapshot.Messages = snapshot.Messages[:1]
			envelope := waBaseMessageEnvelope()
			envelope.ChatJID = waCanonicalChatJID
			envelope.SenderJID = waCanonicalChatJID
			envelope.MessageID = snapshot.Messages[0].StanzaID
			envelope.TimestampMS = snapshot.Messages[0].TimestampMS
			live, _ := waMessageRecord(t, envelope, &waE2E.Message{Conversation: proto.String("synthetic live body")})
			live.AccountID = waWorkerAccountID
			live.DedupeKey = "synthetic-live-stanza"
			if liveFirst {
				h.append(t, live, false)
				before := h.state(t)
				h.now = h.now.Add(time.Hour)
				h.append(t, wadhRecord(t, snapshot), true)
				if after := h.state(t); !reflect.DeepEqual(before, after) {
					t.Fatal("desktop changed live rows")
				}
			} else {
				h.append(t, wadhRecord(t, snapshot), true)
				h.now = h.now.Add(time.Hour)
				h.append(t, live, false)
			}
			message := h.message(t, waCanonicalChatJID, snapshot.Messages[0].StanzaID)
			if message.Body != "synthetic live body" || len(h.rows(t, "SELECT * FROM messages")) != 1 {
				t.Fatalf("message=%+v", message)
			}
			if liveFirst && h.counts().HistoryExisting != 1 {
				t.Fatalf("counts=%+v", h.counts())
			}
		})
	}
}

func TestWhatsAppDesktopHistoryWorkerI5ChunkOrderIndependenceProperty(t *testing.T) {
	random := rand.New(rand.NewSource(427101))
	categories := make([]int, 4)
	for caseIndex := 0; caseIndex < 24; caseIndex++ {
		state := random.Uint64()
		category := int((state >> 33) % 4)
		categories[category]++
		frames := []bridge.RawIngressRecord{}
		for chatIndex := 0; chatIndex < 2; chatIndex++ {
			jid := fmt.Sprintf("1555555010%d@s.whatsapp.net", chatIndex)
			participants := []WhatsAppDesktopHistoryParticipant{{JID: jid}}
			if category&1 != 0 {
				jid = fmt.Sprintf("12345678%d@g.us", chatIndex)
				participants = []WhatsAppDesktopHistoryParticipant{{JID: "15555550200@s.whatsapp.net"}, {JID: fmt.Sprintf("1555555010%d@s.whatsapp.net", chatIndex)}}
			}
			for chunk := 0; chunk < 2; chunk++ {
				fromMe := category&2 != 0 && chunk == 1
				snapshot := WhatsAppDesktopHistorySnapshot{ChatJID: jid, Title: fmt.Sprintf("synthetic chat %d", chatIndex), Participants: participants, Messages: []WhatsAppDesktopHistoryMessage{{StanzaID: fmt.Sprintf("synthetic-%d-%d-%d", caseIndex, chatIndex, chunk), SenderJID: participants[0].JID, TimestampMS: 1780000000000 + int64(chunk)*1000, IsFromMe: fromMe, Type: 0, Text: fmt.Sprintf("synthetic case %d chat %d chunk %d", caseIndex, chatIndex, chunk)}}}
				frames = append(frames, wadhRecord(t, snapshot))
			}
		}
		first := newWADHHarness(t)
		for _, record := range frames {
			first.append(t, record, true)
		}
		want := first.state(t)
		// Close before allocating the next case to keep file and SQLite memory
		// bounded; Cleanup safely closes an already closed database again.
		if err := first.store.Close(); err != nil {
			t.Fatal(err)
		}
		random.Shuffle(len(frames), func(i, j int) { frames[i], frames[j] = frames[j], frames[i] })
		second := newWADHHarness(t)
		for _, record := range frames {
			second.append(t, record, true)
		}
		if got := second.state(t); !reflect.DeepEqual(want, got) {
			t.Fatalf("case %d category %d is order-dependent: want=%v got=%v", caseIndex, category, want, got)
		}
		if c := second.counts(); c.HistoryConversations != 2 || c.HistoryImported != 4 {
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
