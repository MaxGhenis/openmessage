package google

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"testing/quick"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/mautrix-gmessages/pkg/libgm"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
	"google.golang.org/protobuf/proto"

	"github.com/maxghenis/openmessage/internal/app"
	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/ingest"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

// Tests for the Google adapter run as its generation's catch-up history
// ingress (app.GoogleHistoryIngress, google.go AppendHistoryConversation /
// AppendHistoryMessage / appendHistory). Catch-ups in internal/app capture the
// run with the client (beginGoogleCatchUp) and cannot be driven from this
// package, so these tests call the run's history methods directly, the way
// googleCatchUp.storeConversation / storeMessage do.

// historyTeeSink is the live recordingSink from google_test.go (AppendIngress,
// EmitEphemeral, Beat) plus a bridge.HistoryIngressSink path recorded apart, so
// each test can tell which path a frame took.
type historyTeeSink struct {
	recordingSink

	historyMu  sync.Mutex
	history    []bridge.RawIngressRecord
	historyErr error
}

func (s *historyTeeSink) AppendHistoryIngress(_ context.Context, record bridge.RawIngressRecord) error {
	s.historyMu.Lock()
	defer s.historyMu.Unlock()
	record.Payload = bytes.Clone(record.Payload)
	s.history = append(s.history, record)
	return s.historyErr
}

func (s *historyTeeSink) historyRecords() []bridge.RawIngressRecord {
	s.historyMu.Lock()
	defer s.historyMu.Unlock()
	records := make([]bridge.RawIngressRecord, len(s.history))
	for i, record := range s.history {
		record.Payload = bytes.Clone(record.Payload)
		records[i] = record
	}
	return records
}

var (
	_ bridge.ConnectionSink     = (*historyTeeSink)(nil)
	_ bridge.HistoryIngressSink = (*historyTeeSink)(nil)
)

// startHistoryTeeRun starts one generation on the fake transport and returns
// the run both as the bridge.Run the supervisor sees and as the
// app.GoogleHistoryIngress catch-ups hand fetched history to.
func startHistoryTeeRun(
	t *testing.T,
	sink bridge.ConnectionSink,
	generation bridge.Generation,
) (bridge.Run, app.GoogleHistoryIngress, *fakeTransport) {
	t.Helper()
	host := newTestApp(t)
	fake := &fakeTransport{}
	a := newTestAdapter(t, host, fake)
	started, err := a.Start(context.Background(), bridge.StartRequest{
		AccountID:  "google-primary",
		Generation: generation,
	}, sink)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { stopRun(t, started) })
	if _, ok := started.(*run); !ok {
		t.Fatalf("Start() returned %T, want *run", started)
	}
	history, ok := started.(app.GoogleHistoryIngress)
	if !ok {
		t.Fatalf("*run %T does not implement app.GoogleHistoryIngress", started)
	}
	if host.GetClient() == nil {
		t.Fatal("Start() did not install the generation's legacy client in App")
	}
	return started, history, fake
}

// historyTeeEnvelope mirrors the googleFrameEnvelope JSON (googledecoder.go).
type historyTeeEnvelope struct {
	Kind         string `json:"kind"`
	Proto        []byte `json:"proto_b64"`
	IsOld        *bool  `json:"is_old"`
	Conversation []byte `json:"conversation_b64"`
}

func decodeHistoryTeeEnvelope(t *testing.T, payload []byte) (historyTeeEnvelope, map[string]json.RawMessage) {
	t.Helper()
	var envelope historyTeeEnvelope
	if err := json.Unmarshal(payload, &envelope); err != nil {
		t.Fatalf("decode envelope %s: %v", payload, err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil {
		t.Fatalf("decode envelope fields %s: %v", payload, err)
	}
	return envelope, fields
}

func historyTeeKey(t *testing.T, kind, remoteID string, m proto.Message) string {
	t.Helper()
	protoBytes, err := proto.Marshal(m)
	if err != nil {
		t.Fatalf("proto.Marshal(): %v", err)
	}
	digest := sha256.Sum256(protoBytes)
	return kind + ":" + remoteID + ":" + hex.EncodeToString(digest[:4])
}

func historyTeeMessage(messageID, conversationID, body string) *gmproto.Message {
	return &gmproto.Message{
		MessageID:      messageID,
		ConversationID: conversationID,
		Timestamp:      1_750_000_000_000_000,
		MessageStatus:  &gmproto.MessageStatus{Status: gmproto.MessageStatusType_INCOMING_COMPLETE},
		SenderParticipant: &gmproto.Participant{
			FullName: "Ada",
			ID:       &gmproto.SmallInfo{Number: "+15551234567"},
		},
		MessageInfo: []*gmproto.MessageInfo{{
			Data: &gmproto.MessageInfo_MessageContent{
				MessageContent: &gmproto.MessageContent{Content: body},
			},
		}},
	}
}

func historyTeeGroup(conversationID string) *gmproto.Conversation {
	return &gmproto.Conversation{
		ConversationID: conversationID,
		Name:           "Family",
		IsGroupChat:    true,
		Participants: []*gmproto.Participant{
			{FullName: "Ada", ID: &gmproto.SmallInfo{Number: "+15551234567"}},
			{FullName: "Grace", ID: &gmproto.SmallInfo{Number: "+15557654321"}},
		},
	}
}

// AppendHistoryMessage commits exactly one history frame through the sink's
// history path: history codec, the StartRequest's account and generation,
// is_old true, the conversation snapshot embedded, and the same dedupe key the
// live tee derives for the same protobuf. Nothing goes through AppendIngress.
func TestHistoryMessageAppendsOneHistoryFrameKeyedLikeLive(t *testing.T) {
	sink := &historyTeeSink{}
	_, history, fake := startHistoryTeeRun(t, sink, 31)

	message := historyTeeMessage("history-message-1", "history-conversation", "fetched body")
	conversation := historyTeeGroup("history-conversation")
	messageBefore := proto.Clone(message)
	conversationBefore := proto.Clone(conversation)

	before := time.Now()
	if err := history.AppendHistoryMessage(context.Background(), "history-conversation", conversation, message); err != nil {
		t.Fatalf("AppendHistoryMessage() error = %v", err)
	}
	after := time.Now()

	if live := sink.ingressRecords(); len(live) != 0 {
		t.Fatalf("history message reached AppendIngress %d times, want 0", len(live))
	}
	records := sink.historyRecords()
	if len(records) != 1 {
		t.Fatalf("AppendHistoryIngress calls = %d, want 1", len(records))
	}
	record := records[0]
	if record.AccountID != "google-primary" || record.Generation != 31 {
		t.Fatalf("history frame ownership = (%q, %d), want (google-primary, 31)", record.AccountID, record.Generation)
	}
	if ingest.GoogleHistoryCodec != "google.protobuf.history" || ingest.GoogleCodecVersion != 1 {
		t.Fatalf("history codec constants = %q v%d, want google.protobuf.history v1",
			ingest.GoogleHistoryCodec, ingest.GoogleCodecVersion)
	}
	if record.Codec != ingest.GoogleHistoryCodec || record.CodecVersion != ingest.GoogleCodecVersion {
		t.Fatalf("history codec = %q v%d, want google.protobuf.history v1", record.Codec, record.CodecVersion)
	}
	if record.ReceivedAt.Before(before) || record.ReceivedAt.After(after) {
		t.Fatalf("ReceivedAt = %s, want the hand-off time in [%s, %s]", record.ReceivedAt, before, after)
	}

	envelope, fields := decodeHistoryTeeEnvelope(t, record.Payload)
	if envelope.Kind != "message" {
		t.Fatalf("envelope kind = %q, want message", envelope.Kind)
	}
	if envelope.IsOld == nil || !*envelope.IsOld {
		t.Fatalf("history envelope is_old = %v, want true", envelope.IsOld)
	}
	if _, present := fields["conversation_b64"]; !present || len(envelope.Conversation) == 0 {
		t.Fatalf("history message envelope lacks conversation_b64: %s", record.Payload)
	}
	var gotMessage gmproto.Message
	if err := proto.Unmarshal(envelope.Proto, &gotMessage); err != nil {
		t.Fatalf("unmarshal history message proto: %v", err)
	}
	if !proto.Equal(&gotMessage, message) {
		t.Fatalf("history message proto = %v, want %v", &gotMessage, message)
	}
	var gotConversation gmproto.Conversation
	if err := proto.Unmarshal(envelope.Conversation, &gotConversation); err != nil {
		t.Fatalf("unmarshal embedded conversation proto: %v", err)
	}
	if !proto.Equal(&gotConversation, conversation) {
		t.Fatalf("embedded conversation = %v, want %v", &gotConversation, conversation)
	}
	if !proto.Equal(message, messageBefore) || !proto.Equal(conversation, conversationBefore) {
		t.Fatal("AppendHistoryMessage mutated the caller's protobufs")
	}

	wantKey := historyTeeKey(t, "hmsg", "history-message-1", message)
	if record.DedupeKey != wantKey ||
		record.DedupeKey != ingest.GoogleIngressDedupeKey("hmsg", "history-message-1", envelope.Proto) {
		t.Fatalf("history dedupe key = %q, want %q", record.DedupeKey, wantKey)
	}

	// The live tee for the same protobuf uses the live key namespace: the two
	// origins never share an inbox row, so a fetched copy cannot swallow a
	// later live push of the same bytes. Both keys cover only the message
	// protobuf, not is_old or the embedded conversation.
	fake.emit(&libgm.WrappedMessage{Message: message, IsOld: false})
	live := sink.ingressRecords()
	if len(live) != 1 {
		t.Fatalf("live AppendIngress calls = %d, want 1", len(live))
	}
	if live[0].Codec != ingest.GoogleCodec {
		t.Fatalf("live codec = %q, want %q", live[0].Codec, ingest.GoogleCodec)
	}
	if live[0].DedupeKey != historyTeeKey(t, "msg", "history-message-1", message) || live[0].DedupeKey == record.DedupeKey {
		t.Fatalf("live key %q, history key %q: want the live msg: key, distinct from the history key", live[0].DedupeKey, record.DedupeKey)
	}
	if bytes.Equal(live[0].Payload, record.Payload) {
		t.Fatal("live and history payloads are identical; history must carry is_old=true and the conversation")
	}
	if got := len(sink.historyRecords()); got != 1 {
		t.Fatalf("live emit used the history path: history appends = %d, want 1", got)
	}
}

// The conversation snapshot rides along only when it is the message's own
// conversation; a message with no conversation ID of its own is attributed to
// the conversation it was fetched from (on a copy); frames that cannot be
// built are not committed at all.
func TestHistoryMessageConversationSnapshotRules(t *testing.T) {
	t.Run("nil snapshot is omitted", func(t *testing.T) {
		sink := &historyTeeSink{}
		_, history, _ := startHistoryTeeRun(t, sink, 5)
		message := historyTeeMessage("m-nil", "c-1", "body")
		if err := history.AppendHistoryMessage(context.Background(), "c-1", nil, message); err != nil {
			t.Fatalf("AppendHistoryMessage() error = %v", err)
		}
		records := sink.historyRecords()
		if len(records) != 1 {
			t.Fatalf("history appends = %d, want 1", len(records))
		}
		envelope, fields := decodeHistoryTeeEnvelope(t, records[0].Payload)
		if _, present := fields["conversation_b64"]; present {
			t.Fatalf("nil snapshot produced conversation_b64: %s", records[0].Payload)
		}
		if envelope.IsOld == nil || !*envelope.IsOld {
			t.Fatalf("is_old = %v, want true", envelope.IsOld)
		}
	})

	t.Run("snapshot of another thread is dropped", func(t *testing.T) {
		sink := &historyTeeSink{}
		_, history, _ := startHistoryTeeRun(t, sink, 5)
		message := historyTeeMessage("m-other", "c-1", "body")
		if err := history.AppendHistoryMessage(context.Background(), "c-1", historyTeeGroup("c-2"), message); err != nil {
			t.Fatalf("AppendHistoryMessage() error = %v", err)
		}
		records := sink.historyRecords()
		if len(records) != 1 {
			t.Fatalf("history appends = %d, want 1 (the message itself still commits)", len(records))
		}
		if _, fields := decodeHistoryTeeEnvelope(t, records[0].Payload); fields["conversation_b64"] != nil {
			t.Fatalf("foreign snapshot was embedded: %s", records[0].Payload)
		}
		if records[0].DedupeKey != historyTeeKey(t, "hmsg", "m-other", message) {
			t.Fatalf("dedupe key = %q, want key over the unchanged message", records[0].DedupeKey)
		}
	})

	t.Run("message without conversation id is attributed on a copy", func(t *testing.T) {
		sink := &historyTeeSink{}
		_, history, _ := startHistoryTeeRun(t, sink, 5)
		message := historyTeeMessage("m-orphan", "", "body")
		conversation := historyTeeGroup("fetched-from")
		if err := history.AppendHistoryMessage(context.Background(), " fetched-from ", conversation, message); err != nil {
			t.Fatalf("AppendHistoryMessage() error = %v", err)
		}
		if message.GetConversationID() != "" {
			t.Fatalf("caller's message was mutated: ConversationID = %q", message.GetConversationID())
		}
		records := sink.historyRecords()
		if len(records) != 1 {
			t.Fatalf("history appends = %d, want 1", len(records))
		}
		envelope, fields := decodeHistoryTeeEnvelope(t, records[0].Payload)
		var got gmproto.Message
		if err := proto.Unmarshal(envelope.Proto, &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if got.GetConversationID() != "fetched-from" {
			t.Fatalf("framed ConversationID = %q, want fetched-from (trimmed)", got.GetConversationID())
		}
		if fields["conversation_b64"] == nil {
			t.Fatal("snapshot of the attributed conversation was not embedded")
		}
		attributed := proto.Clone(message).(*gmproto.Message)
		attributed.ConversationID = "fetched-from"
		// Intended divergence: the key covers the attributed bytes, so it
		// differs from a key over the raw fetched protobuf.
		if records[0].DedupeKey != historyTeeKey(t, "hmsg", "m-orphan", attributed) {
			t.Fatalf("dedupe key = %q, want key over the attributed copy", records[0].DedupeKey)
		}
		if records[0].DedupeKey == historyTeeKey(t, "hmsg", "m-orphan", message) {
			t.Fatal("dedupe key ignored the attribution")
		}
	})

	t.Run("unattributable or nil message is not committed", func(t *testing.T) {
		sink := &historyTeeSink{}
		_, history, _ := startHistoryTeeRun(t, sink, 5)
		if err := history.AppendHistoryMessage(context.Background(), "  ", nil, historyTeeMessage("m-x", "", "body")); err == nil {
			t.Fatal("AppendHistoryMessage() without any conversation ID error = nil")
		}
		if err := history.AppendHistoryMessage(context.Background(), "c-1", nil, nil); err == nil {
			t.Fatal("AppendHistoryMessage(nil message) error = nil")
		}
		if err := history.AppendHistoryConversation(context.Background(), nil); err == nil {
			t.Fatal("AppendHistoryConversation(nil) error = nil")
		}
		if got := len(sink.historyRecords()); got != 0 {
			t.Fatalf("unbuildable frames reached AppendHistoryIngress %d times", got)
		}
		if got := len(sink.ingressRecords()); got != 0 {
			t.Fatalf("unbuildable frames reached AppendIngress %d times", got)
		}
	})
}

// AppendHistoryConversation commits a history conversation frame (no is_old,
// no embedded conversation) keyed exactly like the live tee's frame for the
// same snapshot.
func TestHistoryConversationAppendsHistoryConversationFrame(t *testing.T) {
	sink := &historyTeeSink{}
	_, history, fake := startHistoryTeeRun(t, sink, 17)

	conversation := historyTeeGroup("listed-conversation")
	if err := history.AppendHistoryConversation(context.Background(), conversation); err != nil {
		t.Fatalf("AppendHistoryConversation() error = %v", err)
	}
	if live := sink.ingressRecords(); len(live) != 0 {
		t.Fatalf("history conversation reached AppendIngress %d times", len(live))
	}
	records := sink.historyRecords()
	if len(records) != 1 {
		t.Fatalf("AppendHistoryIngress calls = %d, want 1", len(records))
	}
	record := records[0]
	if record.AccountID != "google-primary" || record.Generation != 17 ||
		record.Codec != ingest.GoogleHistoryCodec || record.CodecVersion != 1 || record.ReceivedAt.IsZero() {
		t.Fatalf("history conversation record = %+v", record)
	}
	envelope, fields := decodeHistoryTeeEnvelope(t, record.Payload)
	if envelope.Kind != "conversation" {
		t.Fatalf("envelope kind = %q, want conversation", envelope.Kind)
	}
	if _, present := fields["is_old"]; present {
		t.Fatalf("conversation envelope included is_old: %s", record.Payload)
	}
	if _, present := fields["conversation_b64"]; present {
		t.Fatalf("conversation envelope included conversation_b64: %s", record.Payload)
	}
	var got gmproto.Conversation
	if err := proto.Unmarshal(envelope.Proto, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !proto.Equal(&got, conversation) {
		t.Fatalf("history conversation proto = %v, want %v", &got, conversation)
	}
	if want := historyTeeKey(t, "hconv", "listed-conversation", conversation); record.DedupeKey != want {
		t.Fatalf("history conversation key = %q, want %q", record.DedupeKey, want)
	}

	fake.emit(conversation)
	live := sink.ingressRecords()
	if len(live) != 1 || live[0].Codec != ingest.GoogleCodec {
		t.Fatalf("live conversation records = %+v, want one google.protobuf frame", live)
	}
	if live[0].DedupeKey != historyTeeKey(t, "conv", "listed-conversation", conversation) || live[0].DedupeKey == record.DedupeKey {
		t.Fatalf("live conversation key %q, history key %q: want the live conv: key, distinct from the history key", live[0].DedupeKey, record.DedupeKey)
	}
	// Codec and key namespace differ: the conversation envelope is the same bytes.
	if !bytes.Equal(live[0].Payload, record.Payload) {
		t.Fatalf("conversation payloads differ:\n live    %s\n history %s", live[0].Payload, record.Payload)
	}
	if got := len(sink.historyRecords()); got != 1 {
		t.Fatalf("live emit used the history path: history appends = %d", got)
	}
}

// After the run stops (admission closed), both hand-offs fail closed with an
// error wrapping app.ErrGoogleHistoryClosed and nothing reaches either path.
func TestHistoryAppendsAfterStopFailClosed(t *testing.T) {
	sink := &historyTeeSink{}
	started, history, _ := startHistoryTeeRun(t, sink, 8)
	stopRun(t, started)

	errs := map[string]error{
		"message": history.AppendHistoryMessage(
			context.Background(),
			"c-1",
			historyTeeGroup("c-1"),
			historyTeeMessage("late", "c-1", "late body"),
		),
		"conversation": history.AppendHistoryConversation(context.Background(), historyTeeGroup("c-1")),
	}
	for name, err := range errs {
		if !errors.Is(err, app.ErrGoogleHistoryClosed) {
			t.Fatalf("%s after Stop error = %v, want app.ErrGoogleHistoryClosed", name, err)
		}
		if errors.Is(err, bridge.ErrStaleGeneration) {
			t.Fatalf("%s after Stop reached the sink fence: %v", name, err)
		}
	}
	if got := len(sink.historyRecords()); got != 0 {
		t.Fatalf("history committed after Stop: %d records", got)
	}
	if got := len(sink.ingressRecords()); got != 0 {
		t.Fatalf("live path used after Stop: %d records", got)
	}
}

// A connection sink with no history path fails with
// bridge.ErrHistoryIngressMissing and never falls back to AppendIngress, which
// would make fetched history look like live delivery. Intended: the error is
// not ErrGoogleHistoryClosed, because the generation is still open.
func TestHistorySinkWithoutHistoryPathReportsMissing(t *testing.T) {
	sink := &recordingSink{} // google_test.go: AppendIngress only
	_, history, _ := startHistoryTeeRun(t, sink, 3)

	errs := map[string]error{
		"message": history.AppendHistoryMessage(
			context.Background(),
			"c-1",
			nil,
			historyTeeMessage("m-1", "c-1", "body"),
		),
		"conversation": history.AppendHistoryConversation(context.Background(), historyTeeGroup("c-1")),
	}
	for name, err := range errs {
		if !errors.Is(err, bridge.ErrHistoryIngressMissing) {
			t.Fatalf("%s error = %v, want bridge.ErrHistoryIngressMissing", name, err)
		}
		if errors.Is(err, app.ErrGoogleHistoryClosed) {
			t.Fatalf("%s error %v claims the generation closed", name, err)
		}
	}
	if got := len(sink.ingressRecords()); got != 0 {
		t.Fatalf("history fell back to AppendIngress %d times", got)
	}
}

// A sink fence rejection (the supervisor retired the generation before the run
// closed admission) is reported as ErrGoogleHistoryClosed so the catch-up
// stops handing history to this generation, and still matches
// bridge.ErrStaleGeneration. Contrast: any other commit error passes through
// without claiming the generation closed.
func TestHistoryStaleGenerationIsReportedAsClosed(t *testing.T) {
	for _, tc := range []struct {
		name         string
		sinkErr      error
		wantClosed   bool
		wantDisabled bool
	}{
		{name: "stale generation", sinkErr: bridge.ErrStaleGeneration, wantClosed: true},
		// No v2 ingest is running (legacy-only install): the catch-up must be
		// able to tell, so it stops offering without counting a failure.
		{name: "no ingest sink", sinkErr: bridge.ErrHistoryIngressDisabled, wantDisabled: true},
		{name: "other commit error", sinkErr: errors.New("inbox unavailable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sink := &historyTeeSink{historyErr: tc.sinkErr}
			_, history, _ := startHistoryTeeRun(t, sink, 11)
			errs := []error{
				history.AppendHistoryMessage(context.Background(), "c-1", nil, historyTeeMessage("m-1", "c-1", "body")),
				history.AppendHistoryConversation(context.Background(), historyTeeGroup("c-1")),
			}
			for i, err := range errs {
				if !errors.Is(err, tc.sinkErr) {
					t.Fatalf("call %d error = %v, want it to wrap %v", i, err, tc.sinkErr)
				}
				if got := errors.Is(err, app.ErrGoogleHistoryClosed); got != tc.wantClosed {
					t.Fatalf("call %d errors.Is(ErrGoogleHistoryClosed) = %v, want %v (err %v)", i, got, tc.wantClosed, err)
				}
				if got := errors.Is(err, app.ErrGoogleHistoryDisabled); got != tc.wantDisabled {
					t.Fatalf("call %d errors.Is(ErrGoogleHistoryDisabled) = %v, want %v (err %v)", i, got, tc.wantDisabled, err)
				}
			}
			if got := len(sink.historyRecords()); got != 2 {
				t.Fatalf("history appends offered to the sink = %d, want 2", got)
			}
		})
	}
}

// The live tee is unchanged: pushed frames, including is_old replays from
// libgm's skipCount (push deliveries, DESIGN.md "Silence choice"), go through
// AppendIngress with the live codec and never through the history path.
func TestHistoryLiveTeeStillUsesLiveCodecAndPath(t *testing.T) {
	sink := &historyTeeSink{}
	_, _, fake := startHistoryTeeRun(t, sink, 6)

	message := historyTeeMessage("live-1", "live-conversation", "pushed")
	fake.emit(&libgm.WrappedMessage{Message: message, IsOld: true})
	fake.emit(historyTeeGroup("live-conversation"))

	if ingest.GoogleCodec != "google.protobuf" {
		t.Fatalf("live codec constant = %q, want google.protobuf", ingest.GoogleCodec)
	}
	if got := len(sink.historyRecords()); got != 0 {
		t.Fatalf("live frames used the history path %d times", got)
	}
	live := sink.ingressRecords()
	if len(live) != 2 {
		t.Fatalf("live AppendIngress calls = %d, want 2", len(live))
	}
	for i, record := range live {
		if record.Codec != ingest.GoogleCodec || record.Generation != 6 {
			t.Fatalf("live record %d = codec %q generation %d, want google.protobuf generation 6", i, record.Codec, record.Generation)
		}
		if _, fields := decodeHistoryTeeEnvelope(t, record.Payload); fields["conversation_b64"] != nil {
			t.Fatalf("live record %d carries conversation_b64: %s", i, record.Payload)
		}
	}
	envelope, _ := decodeHistoryTeeEnvelope(t, live[0].Payload)
	if envelope.Kind != "message" || envelope.IsOld == nil || !*envelope.IsOld {
		t.Fatalf("live is_old replay envelope = kind %q is_old %v, want message/true", envelope.Kind, envelope.IsOld)
	}
}

// historyTeeV2 is a real v2 ingest stack (shape copied from
// TestIngressTeeThroughRealSinkDedupesExactFrames) with both Google codecs
// registered as in cmd/v2stack.go: the live codec and the history codec with
// History: true.
type historyTeeV2 struct {
	sink      *ingest.Sink
	counters  *ingest.Counters
	storePath string
	db        *sql.DB
}

func newHistoryTeeV2(t *testing.T) *historyTeeV2 {
	t.Helper()
	storePath := filepath.Join(t.TempDir(), "v2.sqlite3")
	store, err := sqlite.Open(storePath)
	if err != nil {
		t.Fatalf("sqlite.Open(): %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now
	nowMS := now().UnixMilli()
	if err := store.UpsertAccount(sqlite.Account{
		AccountID:   "google-primary",
		BridgeKey:   "google_messages",
		DisplayName: "Google Messages",
		Mode:        sqlite.AccountModeLive,
		Enabled:     true,
		ConfigJSON:  "{}",
		CreatedAtMS: nowMS,
		UpdatedAtMS: nowMS,
	}); err != nil {
		t.Fatalf("UpsertAccount(): %v", err)
	}
	messages, err := sqlite.NewMessageRepository(store, now)
	if err != nil {
		t.Fatalf("NewMessageRepository(): %v", err)
	}
	reactions, err := sqlite.NewReactionRepository(store, now)
	if err != nil {
		t.Fatalf("NewReactionRepository(): %v", err)
	}
	counters := &ingest.Counters{}
	worker, err := ingest.NewWorker(ingest.WorkerConfig{
		Store:     store,
		Messages:  messages,
		Reactions: reactions,
		Counters:  counters,
		Logger:    zerolog.Nop(),
		Decoders: []ingest.DecoderRegistration{
			{
				Codec:    ingest.GoogleCodec,
				Platform: bridge.PlatformGoogle,
				Decoder:  ingest.NewGoogleDecoder(counters),
			},
			{
				Codec:    ingest.GoogleHistoryCodec,
				Platform: bridge.PlatformGoogle,
				Decoder:  ingest.NewGoogleDecoder(counters),
				History:  true,
			},
		},
	})
	if err != nil {
		t.Fatalf("NewWorker(): %v", err)
	}
	sink, err := ingest.NewSink(ingest.SinkConfig{
		Messages: messages,
		Worker:   worker,
		Counters: counters,
	})
	if err != nil {
		t.Fatalf("NewSink(): %v", err)
	}
	workerCtx, cancelWorker := context.WithCancel(context.Background())
	workerDone := make(chan error, 1)
	go func() { workerDone <- worker.Run(workerCtx) }()
	t.Cleanup(func() {
		cancelWorker()
		select {
		case runErr := <-workerDone:
			if runErr != nil {
				t.Errorf("Worker.Run(): %v", runErr)
			}
		case <-time.After(time.Second):
			t.Error("Worker.Run() did not stop")
		}
	})
	inspection, err := sql.Open("sqlite", storePath)
	if err != nil {
		t.Fatalf("sql.Open(): %v", err)
	}
	t.Cleanup(func() { _ = inspection.Close() })
	return &historyTeeV2{sink: sink, counters: counters, storePath: storePath, db: inspection}
}

func (v *historyTeeV2) snapshot() ingest.CounterSnapshot {
	return v.counters.Snapshot("google-primary")
}

func (v *historyTeeV2) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var got int
	if err := v.db.QueryRow(query, args...).Scan(&got); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return got
}

// await polls until the counters satisfy done and no inbox row is
// unprocessed, then returns the final counters.
func (v *historyTeeV2) await(t *testing.T, description string, done func(ingest.CounterSnapshot) bool) ingest.CounterSnapshot {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		snapshot := v.snapshot()
		if done(snapshot) &&
			v.count(t, "SELECT COUNT(*) FROM inbox WHERE processed_at_ms IS NULL") == 0 {
			return snapshot
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s; counters %+v", description, snapshot)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

type historyTeeStoredMessage struct {
	conversationRemoteID string
	conversationKind     string
	conversationTitle    string
	direction            string
	body                 string
	occurredAtMS         int64
	updatedAtMS          int64
}

func (v *historyTeeV2) storedMessage(t *testing.T, remoteMessageID string) historyTeeStoredMessage {
	t.Helper()
	var got historyTeeStoredMessage
	if err := v.db.QueryRow(`
		SELECT c.remote_conversation_id, c.kind, c.title, m.direction, m.body, m.occurred_at_ms, m.updated_at_ms
		FROM messages m JOIN conversations c ON c.conversation_id = m.conversation_id
		WHERE m.remote_message_id = ?`, remoteMessageID).Scan(
		&got.conversationRemoteID,
		&got.conversationKind,
		&got.conversationTitle,
		&got.direction,
		&got.body,
		&got.occurredAtMS,
		&got.updatedAtMS,
	); err != nil {
		t.Fatalf("read v2 message %q: %v", remoteMessageID, err)
	}
	return got
}

// End to end through a real ingest.Sink and Worker: a history message handed
// to the run lands in v2 messages inside the group its embedded snapshot
// created, counted only under the history counters (history_appended,
// history_imported, history_conversations; appended/deduped/projected stay 0,
// DESIGN.md decision 6), in an inbox row with the history codec that the
// worker marked processed. Re-handing the same message (I2) dedupes onto that
// row and changes nothing in v2.
func TestHistoryMessageThroughRealSinkImportsIntoV2(t *testing.T) {
	v2 := newHistoryTeeV2(t)
	_, history, _ := startHistoryTeeRun(t, v2.sink, 41)

	message := historyTeeMessage("e2e-history-message", "e2e-group", "fetched while the push channel was stalled")
	conversation := historyTeeGroup("e2e-group")
	if err := history.AppendHistoryMessage(context.Background(), "e2e-group", conversation, message); err != nil {
		t.Fatalf("AppendHistoryMessage() error = %v", err)
	}

	got := v2.await(t, "history import", func(s ingest.CounterSnapshot) bool { return s.HistoryImported == 1 })
	if got.HistoryAppended != 1 || got.HistoryImported != 1 || got.HistoryConversations != 1 ||
		got.HistoryDeduped != 0 || got.HistoryExisting != 0 {
		t.Fatalf("history counters = %+v, want appended=1 imported=1 conversations=1", got)
	}
	if got.Appended != 0 || got.Deduped != 0 || got.Projected != 0 || got.Quarantined != 0 {
		t.Fatalf("live counters moved for a history frame: %+v", got)
	}
	if n := v2.count(t, "SELECT COUNT(*) FROM inbox"); n != 1 {
		t.Fatalf("inbox rows = %d, want 1", n)
	}
	if n := v2.count(t, "SELECT COUNT(*) FROM inbox WHERE codec = ? AND generation = 41 AND processed_at_ms IS NOT NULL",
		ingest.GoogleHistoryCodec); n != 1 {
		t.Fatalf("processed generation-41 history-codec inbox rows = %d, want 1", n)
	}
	if n := v2.count(t, "SELECT COUNT(*) FROM messages"); n != 1 {
		t.Fatalf("v2 messages = %d, want 1", n)
	}
	stored := v2.storedMessage(t, "e2e-history-message")
	want := historyTeeStoredMessage{
		conversationRemoteID: "e2e-group",
		conversationKind:     "group",
		conversationTitle:    "Family",
		direction:            "incoming",
		body:                 "fetched while the push channel was stalled",
		occurredAtMS:         message.GetTimestamp() / 1000,
		updatedAtMS:          stored.updatedAtMS,
	}
	if stored != want {
		t.Fatalf("v2 message = %+v, want %+v", stored, want)
	}

	// I2: the same hand-off again dedupes onto its history row; the replay
	// re-evaluates the frame and finds the message already stored.
	if err := history.AppendHistoryMessage(context.Background(), "e2e-group", conversation, message); err != nil {
		t.Fatalf("second AppendHistoryMessage() error = %v", err)
	}
	again := v2.await(t, "history replay", func(s ingest.CounterSnapshot) bool { return s.HistoryExisting == 1 })
	if again.HistoryAppended != 1 || again.HistoryDeduped != 1 || again.HistoryImported != 1 ||
		again.HistoryConversations != 1 || again.HistorySkipped != 0 || again.Appended != 0 || again.Deduped != 0 {
		t.Fatalf("counters after re-hand-off = %+v", again)
	}
	if n := v2.count(t, "SELECT COUNT(*) FROM inbox"); n != 1 {
		t.Fatalf("inbox rows after re-hand-off = %d, want 1", n)
	}
	if n := v2.count(t, "SELECT COUNT(*) FROM messages"); n != 1 {
		t.Fatalf("v2 messages after re-hand-off = %d, want 1", n)
	}
	if after := v2.storedMessage(t, "e2e-history-message"); after != stored {
		t.Fatalf("re-hand-off changed the v2 row:\n got  %+v\n want %+v", after, stored)
	}
}

// End to end: a fetched copy byte-identical to a frame the live channel
// already delivered gets its own history inbox row (the origins never share a
// row), is skipped by the worker as existing, and leaves the live row, the
// live counters and the live-projected message exactly as they were. Its
// group snapshot does not rewrite the thread the live frames created.
func TestHistoryCopyOfLiveFrameLeavesLiveRowAndMessageAlone(t *testing.T) {
	v2 := newHistoryTeeV2(t)
	_, history, fake := startHistoryTeeRun(t, v2.sink, 42)

	// The live channel delivers the thread's snapshot and then the message.
	fake.emit(historyTeeGroup("pushed-conversation"))
	message := historyTeeMessage("pushed-then-fetched", "pushed-conversation", "pushed first")
	fake.emit(&libgm.WrappedMessage{Message: message})
	live := v2.await(t, "live projection", func(s ingest.CounterSnapshot) bool { return s.Projected == 1 })
	if live.Appended != 2 || live.HistoryAppended != 0 {
		t.Fatalf("counters after live frames = %+v", live)
	}
	var liveInboxID string
	var receivedBefore int64
	if err := v2.db.QueryRow("SELECT inbox_id, received_at_ms FROM inbox WHERE dedupe_key LIKE 'msg:%'").Scan(&liveInboxID, &receivedBefore); err != nil {
		t.Fatalf("read live inbox row: %v", err)
	}
	stored := v2.storedMessage(t, "pushed-then-fetched")

	if err := history.AppendHistoryMessage(
		context.Background(),
		"pushed-conversation",
		historyTeeGroup("pushed-conversation"),
		message,
	); err != nil {
		t.Fatalf("AppendHistoryMessage() error = %v", err)
	}
	got := v2.await(t, "history copy skipped as existing", func(s ingest.CounterSnapshot) bool { return s.HistoryExisting == 1 })
	if got.HistoryAppended != 1 || got.HistoryDeduped != 0 || got.HistoryImported != 0 ||
		got.HistoryConversations != 0 || got.HistorySkipped != 0 ||
		got.Appended != live.Appended || got.Deduped != live.Deduped || got.Projected != live.Projected ||
		got.StaleReplays != 0 {
		t.Fatalf("counters after fetched copy = %+v (live %+v)", got, live)
	}
	if n := v2.count(t, "SELECT COUNT(*) FROM inbox"); n != 3 {
		t.Fatalf("inbox rows = %d, want the two live rows and the history row", n)
	}
	var codec string
	var receivedAfter int64
	if err := v2.db.QueryRow("SELECT codec, received_at_ms FROM inbox WHERE inbox_id = ?", liveInboxID).Scan(&codec, &receivedAfter); err != nil {
		t.Fatalf("read live inbox row: %v", err)
	}
	if codec != ingest.GoogleCodec || receivedAfter != receivedBefore {
		t.Fatalf("live inbox row = (%q, %d), want unchanged (%q, %d)", codec, receivedAfter, ingest.GoogleCodec, receivedBefore)
	}
	if n := v2.count(t, "SELECT COUNT(*) FROM inbox WHERE codec = '"+ingest.GoogleHistoryCodec+"'"); n != 1 {
		t.Fatalf("history-codec inbox rows = %d, want 1", n)
	}
	if after := v2.storedMessage(t, "pushed-then-fetched"); after != stored {
		t.Fatalf("fetched copy changed the live-projected row:\n got  %+v\n want %+v", after, stored)
	}
}

// Stop joins an in-flight history hand-off (the run admits it like a libgm
// callback), so nothing can commit after the generation is retired, and a
// hand-off attempted after Stop fails closed.
func TestHistoryStopJoinsInFlightHandOff(t *testing.T) {
	sink := &historyBlockingSink{entered: make(chan struct{}), release: make(chan struct{})}
	started, history, _ := startHistoryTeeRun(t, sink, 5)

	handOff := make(chan error, 1)
	go func() {
		handOff <- history.AppendHistoryMessage(context.Background(), "c-join", nil, historyTeeMessage("m-join", "c-join", "in flight"))
	}()
	select {
	case <-sink.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("history hand-off never reached the sink")
	}

	stopped := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		stopped <- started.Stop(ctx)
	}()
	select {
	case err := <-stopped:
		t.Fatalf("Stop() returned (%v) while a history hand-off was still committing", err)
	case <-time.After(200 * time.Millisecond):
	}

	close(sink.release)
	select {
	case err := <-handOff:
		if err != nil {
			t.Fatalf("in-flight hand-off error = %v, want it to finish", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("in-flight hand-off did not finish after release")
	}
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatalf("Stop() error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Stop() did not return after the hand-off finished")
	}

	err := history.AppendHistoryMessage(context.Background(), "c-join", nil, historyTeeMessage("m-late", "c-join", "too late"))
	if !errors.Is(err, app.ErrGoogleHistoryClosed) {
		t.Fatalf("hand-off after Stop error = %v, want ErrGoogleHistoryClosed", err)
	}
	if got := sink.appends.Load(); got != 1 {
		t.Fatalf("history appends that reached the sink = %d, want only the in-flight one", got)
	}
}

// historyBlockingSink blocks its first history append until released.
type historyBlockingSink struct {
	recordingSink
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	appends atomic.Int64
}

func (s *historyBlockingSink) AppendHistoryIngress(context.Context, bridge.RawIngressRecord) error {
	s.appends.Add(1)
	s.once.Do(func() { close(s.entered) })
	<-s.release
	return nil
}

// historyTeeCase is a random fetched message: random IDs and body, an
// incoming or outgoing status, an optional sender, an optional snapshot of its
// own conversation, and a random is_old for the live copy.
type historyTeeCase struct {
	Message      *gmproto.Message
	Conversation *gmproto.Conversation
	LiveIsOld    bool
}

func (historyTeeCase) Generate(r *rand.Rand, _ int) reflect.Value {
	word := func(n int) string {
		letters := make([]byte, 1+r.Intn(n))
		for i := range letters {
			letters[i] = "abcdefghijklmnopqrstuvwxyz0123456789-"[r.Intn(37)]
		}
		return string(letters)
	}
	statuses := []gmproto.MessageStatusType{
		gmproto.MessageStatusType_INCOMING_COMPLETE,
		gmproto.MessageStatusType_OUTGOING_COMPLETE,
		gmproto.MessageStatusType_OUTGOING_DELIVERED,
		gmproto.MessageStatusType_OUTGOING_DISPLAYED,
	}
	conversationID := "c-" + word(12)
	message := &gmproto.Message{
		MessageID:      fmt.Sprintf("%d", 1+r.Int63n(1_000_000)),
		ConversationID: conversationID,
		Timestamp:      1 + r.Int63n(2_000_000_000_000_000),
		MessageStatus:  &gmproto.MessageStatus{Status: statuses[r.Intn(len(statuses))], SubCode: int64(r.Intn(3))},
		MessageInfo: []*gmproto.MessageInfo{{
			Data: &gmproto.MessageInfo_MessageContent{
				MessageContent: &gmproto.MessageContent{Content: word(40)},
			},
		}},
	}
	if r.Intn(2) == 0 {
		message.SenderParticipant = &gmproto.Participant{
			FullName: word(10),
			ID:       &gmproto.SmallInfo{Number: fmt.Sprintf("+1555%07d", r.Intn(10_000_000))},
		}
	}
	var conversation *gmproto.Conversation
	if r.Intn(2) == 0 {
		conversation = &gmproto.Conversation{
			ConversationID: conversationID,
			Name:           word(16),
			IsGroupChat:    r.Intn(2) == 0,
		}
	}
	return reflect.ValueOf(historyTeeCase{Message: message, Conversation: conversation, LiveIsOld: r.Intn(2) == 0})
}

// Property: for any fetched message that carries its own conversation ID, the
// run's history frame and the live tee's frame for the same protobuf never
// share a dedupe key (hmsg: vs msg: over the same bytes), while the history
// frame always uses the history codec and
// is_old=true and embeds exactly the matching snapshot, and the live frame
// keeps the live codec and its own is_old.
func TestHistoryPropertyHistoryKeyIsDisjointFromLiveKey(t *testing.T) {
	sink := &historyTeeSink{}
	_, history, fake := startHistoryTeeRun(t, sink, 99)

	property := func(c historyTeeCase) bool {
		historyBefore, liveBefore := len(sink.historyRecords()), len(sink.ingressRecords())
		if err := history.AppendHistoryMessage(context.Background(), c.Message.GetConversationID(), c.Conversation, c.Message); err != nil {
			t.Logf("AppendHistoryMessage() error = %v", err)
			return false
		}
		fake.emit(&libgm.WrappedMessage{Message: c.Message, IsOld: c.LiveIsOld})
		histories, lives := sink.historyRecords(), sink.ingressRecords()
		if len(histories) != historyBefore+1 || len(lives) != liveBefore+1 {
			t.Logf("appends: history %d->%d live %d->%d", historyBefore, len(histories), liveBefore, len(lives))
			return false
		}
		h, l := histories[len(histories)-1], lives[len(lives)-1]
		if h.DedupeKey == l.DedupeKey ||
			h.DedupeKey != historyTeeKey(t, "hmsg", c.Message.GetMessageID(), c.Message) ||
			l.DedupeKey != historyTeeKey(t, "msg", c.Message.GetMessageID(), c.Message) {
			t.Logf("keys: history %q live %q", h.DedupeKey, l.DedupeKey)
			return false
		}
		if h.Codec != ingest.GoogleHistoryCodec || l.Codec != ingest.GoogleCodec || h.Generation != 99 || l.Generation != 99 {
			return false
		}
		he, _ := decodeHistoryTeeEnvelope(t, h.Payload)
		le, _ := decodeHistoryTeeEnvelope(t, l.Payload)
		if he.IsOld == nil || !*he.IsOld || le.IsOld == nil || *le.IsOld != c.LiveIsOld || len(le.Conversation) != 0 {
			return false
		}
		if c.Conversation == nil {
			return len(he.Conversation) == 0
		}
		var embedded gmproto.Conversation
		if err := proto.Unmarshal(he.Conversation, &embedded); err != nil {
			return false
		}
		return proto.Equal(&embedded, c.Conversation)
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 100, Rand: rand.New(rand.NewSource(20261008))}); err != nil {
		t.Fatalf("history/live key property: %v", err)
	}
}
