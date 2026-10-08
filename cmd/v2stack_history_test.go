package cmd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/mautrix-gmessages/pkg/libgm"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"

	"github.com/maxghenis/openmessage/internal/bridge"
	googleadapter "github.com/maxghenis/openmessage/internal/bridgeadapters/google"
	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/ingest"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
	"github.com/maxghenis/openmessage/internal/v2read"
	"github.com/maxghenis/openmessage/internal/web"
)

// The production sink handed to the Google supervisor (serve.go passes
// stack.Sink to bridge.WithConnectionSink) must implement the history
// extension; generationSink.AppendHistoryIngress type-asserts it and fails
// with ErrHistoryIngressMissing otherwise.
var _ bridge.HistoryIngressSink = (*ingest.Sink)(nil)

const (
	v2StackHistoryThread = "v2stack-history-thread"
	v2StackHistoryPeer   = "+15550700001"
	v2StackHistorySelf   = "+15550700000"
	v2StackHistoryTitle  = "History Peer"
)

func v2StackHistoryConversation(title string) *gmproto.Conversation {
	return &gmproto.Conversation{
		ConversationID: v2StackHistoryThread,
		Name:           title,
		Participants: []*gmproto.Participant{
			{FullName: "History Peer", ID: &gmproto.SmallInfo{Number: v2StackHistoryPeer}},
			{FullName: "Me", IsMe: true, ID: &gmproto.SmallInfo{Number: v2StackHistorySelf}},
		},
	}
}

// v2StackHistoryMessage is an incoming text from the thread's peer. An empty
// body yields an empty stub (status set, no content).
func v2StackHistoryMessage(remoteID, body string, atMS int64) *gmproto.Message {
	message := &gmproto.Message{
		MessageID:      remoteID,
		ConversationID: v2StackHistoryThread,
		Timestamp:      atMS * 1000,
		SenderParticipant: &gmproto.Participant{
			FullName: "History Peer",
			ID:       &gmproto.SmallInfo{Number: v2StackHistoryPeer},
		},
		MessageStatus: &gmproto.MessageStatus{Status: gmproto.MessageStatusType_INCOMING_COMPLETE},
	}
	if body != "" {
		message.MessageInfo = []*gmproto.MessageInfo{{
			Data: &gmproto.MessageInfo_MessageContent{
				MessageContent: &gmproto.MessageContent{Content: body},
			},
		}}
	}
	return message
}

type v2StackHistoryHarness struct {
	stack    *v2Stack
	legacy   *db.Store
	messages *sqlite.MessageRepository
}

// startV2StackHistoryHarness builds the production stack exactly as
// TestV2StackFreshGoogleIngressReachesWorker does (v2stack_test.go) and starts
// its runners. Cleanup stops the stack (closing the v2 store) before closing
// the legacy store.
func startV2StackHistoryHarness(t *testing.T, v2Primary bool) *v2StackHistoryHarness {
	t.Helper()
	stack, err := newV2Stack(v2StackDeps{
		Logger:  zerolog.Nop(),
		DataDir: t.TempDir(),
		Google:  googleadapter.New(googleAccountID, nil, func() bool { return false }),
	})
	if err != nil {
		t.Fatalf("newV2Stack(): %v", err)
	}
	legacy, err := db.New(filepath.Join(t.TempDir(), "legacy.sqlite3"))
	if err != nil {
		_ = stack.Store.Close()
		t.Fatalf("db.New(): %v", err)
	}
	t.Cleanup(func() { _ = legacy.Close() })
	startLegacy := legacy
	if v2Primary {
		// v2-primary runs no legacy projector (TestV2PrimaryStackRuns...).
		startLegacy = nil
	}
	stop := stack.Start(context.Background(), startLegacy, web.NewEventBroker(), v2Primary)
	t.Cleanup(stop)
	messages, err := sqlite.NewMessageRepository(stack.Store, time.Now)
	if err != nil {
		t.Fatalf("NewMessageRepository(): %v", err)
	}
	return &v2StackHistoryHarness{stack: stack, legacy: legacy, messages: messages}
}

// waitCounters waits until the Google account's counters equal want exactly
// and the inbox has no unprocessed row, so each step's effects are complete
// before the next step's frame is appended.
func (h *v2StackHistoryHarness) waitCounters(t *testing.T, step string, want ingest.CounterSnapshot) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := h.stack.Counters.Snapshot(googleAccountID)
		pending, err := h.messages.Unprocessed(context.Background())
		if err != nil {
			t.Fatalf("%s: Unprocessed(): %v", step, err)
		}
		if got == want && len(pending) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: counters did not settle (unprocessed=%d)\n got: %+v\nwant: %+v", step, len(pending), got, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (h *v2StackHistoryHarness) conversation(t *testing.T) sqlite.Conversation {
	t.Helper()
	conversation, err := h.stack.Store.GetConversationByRemote(googleAccountID, v2StackHistoryThread)
	if err != nil {
		t.Fatalf("GetConversationByRemote(%q): %v", v2StackHistoryThread, err)
	}
	return conversation
}

func (h *v2StackHistoryHarness) message(t *testing.T, conversationID, remoteID string) sqlite.Message {
	t.Helper()
	message, err := h.messages.GetMessageByRemote(context.Background(), googleAccountID, conversationID, remoteID)
	if err != nil {
		t.Fatalf("GetMessageByRemote(%q): %v", remoteID, err)
	}
	return message
}

// TestV2StackHistoryFrameImportsThroughProductionWiring drives newV2Stack's
// own worker and sink: the google.protobuf.history codec is registered with
// History semantics (insert-only, history_* counters, appended/projected
// untouched), live Google frames still project normally next to it, and the
// counters reach /api/status with every history_* key.
func TestV2StackHistoryFrameImportsThroughProductionWiring(t *testing.T) {
	for _, mode := range []struct {
		name      string
		v2Primary bool
	}{
		{name: "legacy-primary", v2Primary: false},
		{name: "v2-primary", v2Primary: true},
	} {
		t.Run(mode.name, func(t *testing.T) {
			h := startV2StackHistoryHarness(t, mode.v2Primary)
			ctx := context.Background()
			baseMS := time.Now().Add(-48 * time.Hour).Truncate(time.Second).UnixMilli()
			want := ingest.CounterSnapshot{}

			// Step 1: a fetched message of a thread v2 has never seen, carrying
			// its conversation snapshot (DESIGN decision 3).
			historyBody := "fetched while the push channel stalled"
			historyMessage := v2StackHistoryMessage("hist-1", historyBody, baseMS)
			historyRecord, err := ingest.GoogleHistoryMessageRecord(
				googleAccountID, 1, v2StackHistoryThread,
				v2StackHistoryConversation(v2StackHistoryTitle), historyMessage, time.Now(),
			)
			if err != nil {
				t.Fatalf("GoogleHistoryMessageRecord(): %v", err)
			}
			if historyRecord.Codec != ingest.GoogleHistoryCodec || historyRecord.Codec == ingest.GoogleCodec {
				t.Fatalf("history record codec = %q, want %q", historyRecord.Codec, ingest.GoogleHistoryCodec)
			}
			if err := h.stack.Sink.AppendHistoryIngress(ctx, historyRecord); err != nil {
				t.Fatalf("AppendHistoryIngress(): %v", err)
			}
			// Intended: history counts only under history_*; appended and
			// projected keep meaning "the live channel delivered" (decision 6).
			want.HistoryAppended = 1
			want.DecodedEvents = 2 // conversation snapshot + message
			want.HistoryConversations = 1
			want.HistoryImported = 1
			h.waitCounters(t, "history import", want)

			conversation := h.conversation(t)
			if conversation.Title != v2StackHistoryTitle || conversation.Kind != sqlite.ConversationKindDirect {
				t.Fatalf("history-created conversation = %+v, want direct %q", conversation, v2StackHistoryTitle)
			}
			imported := h.message(t, conversation.ConversationID, "hist-1")
			if imported.Body != historyBody || imported.OccurredAtMS != baseMS ||
				imported.Direction != sqlite.MessageDirectionIncoming || imported.State != sqlite.MessageStateActive {
				t.Fatalf("imported history message = %+v", imported)
			}

			// What a v2-primary reader sees, addressed by the Google thread id.
			reads := v2read.New(h.stack.Store)
			visible, err := reads.GetMessagesByConversation(v2StackHistoryThread, 10)
			if err != nil {
				t.Fatalf("v2read GetMessagesByConversation(): %v", err)
			}
			if len(visible) != 1 || visible[0].SourceID != "hist-1" || visible[0].Body != historyBody ||
				visible[0].TimestampMS != baseMS || visible[0].IsFromMe || visible[0].SenderNumber != v2StackHistoryPeer {
				t.Fatalf("v2 reader view after history import = %+v", visible)
			}

			// Step 2: a live frame in the same stack still projects normally.
			liveBody := "delivered live"
			liveMessage := v2StackHistoryMessage("live-1", liveBody, baseMS+60_000)
			liveRecord, err := ingest.GoogleMessageRecord(
				googleAccountID, 1, &libgm.WrappedMessage{Message: liveMessage}, time.Now(),
			)
			if err != nil {
				t.Fatalf("GoogleMessageRecord(): %v", err)
			}
			if liveRecord.Codec != ingest.GoogleCodec {
				t.Fatalf("live record codec = %q, want %q", liveRecord.Codec, ingest.GoogleCodec)
			}
			if err := h.stack.Sink.AppendIngress(ctx, liveRecord); err != nil {
				t.Fatalf("AppendIngress(live): %v", err)
			}
			want.Appended = 1
			want.DecodedEvents = 3
			want.Projected = 1
			h.waitCounters(t, "live projection", want)
			live := h.message(t, conversation.ConversationID, "live-1")
			if live.Body != liveBody || live.OccurredAtMS != baseMS+60_000 {
				t.Fatalf("live message = %+v", live)
			}

			// Step 3: a stale fetched copy of the live message (different body)
			// under a renamed snapshot. History is insert-only (I3): the live
			// row and the bound thread's title stay as they were.
			staleRecord, err := ingest.GoogleHistoryMessageRecord(
				googleAccountID, 1, v2StackHistoryThread,
				v2StackHistoryConversation("Renamed by a stale snapshot"),
				v2StackHistoryMessage("live-1", "stale fetched body", baseMS+60_000),
				time.Now(),
			)
			if err != nil {
				t.Fatalf("GoogleHistoryMessageRecord(stale): %v", err)
			}
			if err := h.stack.Sink.AppendHistoryIngress(ctx, staleRecord); err != nil {
				t.Fatalf("AppendHistoryIngress(stale): %v", err)
			}
			want.HistoryAppended = 2
			want.DecodedEvents = 5
			want.HistoryExisting = 1
			h.waitCounters(t, "stale history refetch", want)
			if got := h.message(t, conversation.ConversationID, "live-1"); !reflect.DeepEqual(got, live) {
				t.Fatalf("history refetch modified the live row\n got: %+v\nwant: %+v", got, live)
			}
			if got := h.conversation(t); got.Title != v2StackHistoryTitle {
				t.Fatalf("history snapshot retitled a bound thread to %q", got.Title)
			}

			// Step 4: the identical history frame again (I2). It dedupes onto
			// its inbox row and is not replayed: history is insert-only, so a
			// second pass could add nothing, and the worker never runs.
			if err := h.stack.Sink.AppendHistoryIngress(ctx, historyRecord); err != nil {
				t.Fatalf("AppendHistoryIngress(repeat): %v", err)
			}
			want.HistoryDeduped = 1
			h.waitCounters(t, "repeat history frame", want)
			if got := h.message(t, conversation.ConversationID, "hist-1"); !reflect.DeepEqual(got, imported) {
				t.Fatalf("repeat history frame modified the row\n got: %+v\nwant: %+v", got, imported)
			}

			// Step 5: an empty-stub history frame is skipped by the history
			// registration's decoder, which shares the stack's counters.
			stubRecord, err := ingest.GoogleHistoryMessageRecord(
				googleAccountID, 1, v2StackHistoryThread, nil,
				v2StackHistoryMessage("hist-stub", "", baseMS+120_000), time.Now(),
			)
			if err != nil {
				t.Fatalf("GoogleHistoryMessageRecord(stub): %v", err)
			}
			if err := h.stack.Sink.AppendHistoryIngress(ctx, stubRecord); err != nil {
				t.Fatalf("AppendHistoryIngress(stub): %v", err)
			}
			want.HistoryAppended = 3
			want.EmptyStubsSkipped = 1
			h.waitCounters(t, "history empty stub", want)

			visible, err = reads.GetMessagesByConversation(v2StackHistoryThread, 10)
			if err != nil {
				t.Fatalf("v2read GetMessagesByConversation(): %v", err)
			}
			var sourceIDs []string
			for _, message := range visible {
				sourceIDs = append(sourceIDs, message.SourceID)
			}
			if !reflect.DeepEqual(sourceIDs, []string{"live-1", "hist-1"}) {
				t.Fatalf("v2 reader message ids = %v, want [live-1 hist-1] newest first", sourceIDs)
			}

			// The counters reach /api/status through the production provider.
			handler := web.APIHandlerWithOptions(h.legacy, nil, zerolog.Nop(), nil, web.APIOptions{
				V2IngestCounters: v2IngestCountersProvider(h.stack),
			})
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/status", nil))
			if recorder.Code != http.StatusOK {
				t.Fatalf("/api/status = %d: %s", recorder.Code, recorder.Body.String())
			}
			var status struct {
				V2Ingest struct {
					Enabled    bool                                  `json:"enabled"`
					PerAccount map[string]map[string]json.RawMessage `json:"per_account"`
				} `json:"v2_ingest"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &status); err != nil {
				t.Fatalf("decode /api/status: %v", err)
			}
			account, ok := status.V2Ingest.PerAccount[googleAccountID]
			if !status.V2Ingest.Enabled || !ok {
				t.Fatalf("/api/status v2_ingest = %+v, want enabled with %q", status.V2Ingest, googleAccountID)
			}
			for key, wantValue := range map[string]uint64{
				"history_appended":      want.HistoryAppended,
				"history_deduped":       want.HistoryDeduped,
				"history_imported":      want.HistoryImported,
				"history_existing":      want.HistoryExisting,
				"history_conversations": want.HistoryConversations,
				"appended":              want.Appended,
				"deduped":               want.Deduped,
				"projected":             want.Projected,
				"empty_stubs_skipped":   want.EmptyStubsSkipped,
			} {
				raw, present := account[key]
				if !present {
					t.Errorf("/api/status per_account[%q] lacks %q", googleAccountID, key)
					continue
				}
				var got uint64
				if err := json.Unmarshal(raw, &got); err != nil || got != wantValue {
					t.Errorf("/api/status %s = %s (%v), want %d", key, raw, err, wantValue)
				}
			}
		})
	}
}

// TestV2StackHistoryCounterJSONKeys pins the wire names of the history
// counters: present even at zero (no omitempty), so status consumers can rely
// on the keys, and distinct from the live keys.
func TestV2StackHistoryCounterJSONKeys(t *testing.T) {
	encoded, err := json.Marshal(ingest.CounterSnapshot{
		HistoryAppended:      1,
		HistoryDeduped:       2,
		HistoryImported:      3,
		HistoryExisting:      4,
		HistoryConversations: 5,
	})
	if err != nil {
		t.Fatalf("marshal CounterSnapshot: %v", err)
	}
	var fields map[string]uint64
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatalf("unmarshal CounterSnapshot: %v", err)
	}
	for key, want := range map[string]uint64{
		"history_appended":      1,
		"history_deduped":       2,
		"history_imported":      3,
		"history_existing":      4,
		"history_conversations": 5,
		"appended":              0,
		"deduped":               0,
		"projected":             0,
	} {
		got, ok := fields[key]
		if !ok || got != want {
			t.Errorf("CounterSnapshot JSON %q = %d (present %t), want %d", key, got, ok, want)
		}
	}

	zero, err := json.Marshal(ingest.CounterSnapshot{})
	if err != nil {
		t.Fatalf("marshal zero CounterSnapshot: %v", err)
	}
	var zeroFields map[string]uint64
	if err := json.Unmarshal(zero, &zeroFields); err != nil {
		t.Fatalf("unmarshal zero CounterSnapshot: %v", err)
	}
	for _, key := range []string{"history_appended", "history_deduped", "history_imported", "history_existing", "history_conversations"} {
		if value, ok := zeroFields[key]; !ok || value != 0 {
			t.Errorf("zero CounterSnapshot JSON %q = %d (present %t), want present 0", key, value, ok)
		}
	}
}
