package ingest_test

// Tests for the Google history codec (google.protobuf.history): the decoder's
// acceptance and rejection rules for history frames and their embedded
// conversation snapshot, and the history record builders' codec, dedupe-key
// and clone-on-fill contracts. Helpers reused from googledecoder_test.go:
// textInfo, assertJSONString, assertEnvelopeProto.

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"testing/quick"
	"time"

	"go.mau.fi/mautrix-gmessages/pkg/libgm"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
	"google.golang.org/protobuf/proto"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/ingest"
)

const (
	hdecAccountID                    = "account-history-decoder"
	hdecGeneration bridge.Generation = 41
)

var hdecReceivedAt = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

// hdecEnvelope mirrors the v1 Google envelope JSON so tests can hand-craft
// frames the record builders refuse to produce (mismatched or misplaced
// conversation snapshots).
type hdecEnvelope struct {
	Kind            string `json:"kind"`
	ProtoB64        []byte `json:"proto_b64"`
	IsOld           *bool  `json:"is_old,omitempty"`
	ConversationB64 []byte `json:"conversation_b64,omitempty"`
}

func TestHistoryDecoderAcceptsHistoryCodecVersionOneOnly(t *testing.T) {
	t.Parallel()

	message := hdecIncomingMessage("history-accept-1", "conversation-accept", "fetched body")
	record, err := ingest.GoogleHistoryMessageRecord(
		hdecAccountID, hdecGeneration, "conversation-accept", nil, message, hdecReceivedAt,
	)
	if err != nil {
		t.Fatalf("GoogleHistoryMessageRecord: %v", err)
	}
	if ingest.GoogleHistoryCodec != "google.protobuf.history" {
		t.Fatalf("GoogleHistoryCodec = %q, want the durable name %q", ingest.GoogleHistoryCodec, "google.protobuf.history")
	}
	if record.Codec != ingest.GoogleHistoryCodec {
		t.Fatalf("history record codec = %q, want %q", record.Codec, ingest.GoogleHistoryCodec)
	}
	if record.CodecVersion != 1 || ingest.GoogleCodecVersion != 1 {
		t.Fatalf("history record codec version = %d (GoogleCodecVersion %d), want 1",
			record.CodecVersion, ingest.GoogleCodecVersion)
	}

	events, err := ingest.NewGoogleDecoder(nil).Decode(context.Background(), record)
	if err != nil {
		t.Fatalf("Decode(history v1): %v", err)
	}
	if len(events) != 1 || events[0].Kind != bridge.EventMessage || events[0].Message == nil {
		t.Fatalf("history v1 events = %+v, want exactly one MessageEvent", events)
	}
	got := events[0].Message
	if got.RemoteMessageID != "history-accept-1" || got.RemoteConversationID != "conversation-accept" ||
		got.Body != "fetched body" || got.Direction != "incoming" {
		t.Fatalf("history message event = %+v", got)
	}

	for _, test := range []struct {
		name    string
		codec   string
		version uint32
	}{
		{name: "history codec version 0", codec: ingest.GoogleHistoryCodec, version: 0},
		{name: "history codec version 2", codec: ingest.GoogleHistoryCodec, version: 2},
		{name: "unrelated codec", codec: "whatsapp.event", version: 1},
		{name: "history codec with suffix", codec: ingest.GoogleHistoryCodec + ".v2", version: 1},
		{name: "history codec prefix only", codec: "google.protobuf.hist", version: 1},
		{name: "history codec different case", codec: strings.ToUpper(ingest.GoogleHistoryCodec), version: 1},
		{name: "history codec padded", codec: " " + ingest.GoogleHistoryCodec, version: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			rejected := record
			rejected.Codec = test.codec
			rejected.CodecVersion = test.version
			if events, err := ingest.NewGoogleDecoder(nil).Decode(context.Background(), rejected); err == nil {
				t.Fatalf("Decode(codec %q v%d) = %+v, nil error; want rejection", test.codec, test.version, events)
			}
		})
	}
}

func TestHistoryDecoderEmbeddedConversationLeadsMessageAndReactions(t *testing.T) {
	t.Parallel()

	conversation := hdecGroupConversation("conversation-group-1")
	message := hdecIncomingMessage("history-group-message", "conversation-group-1", "hello group")
	message.Reactions = []*gmproto.ReactionEntry{{
		Data:           &gmproto.ReactionData{Unicode: "👍"},
		ParticipantIDs: []string{"participant-a", "participant-b"},
	}}
	record, err := ingest.GoogleHistoryMessageRecord(
		hdecAccountID, hdecGeneration, "conversation-group-1", conversation, message, hdecReceivedAt,
	)
	if err != nil {
		t.Fatalf("GoogleHistoryMessageRecord: %v", err)
	}
	events, err := ingest.NewGoogleDecoder(nil).Decode(context.Background(), record)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}

	wantKinds := []bridge.EventKind{
		bridge.EventConversation, bridge.EventMessage, bridge.EventReaction, bridge.EventReaction,
	}
	if got := hdecKinds(events); !reflect.DeepEqual(got, wantKinds) {
		t.Fatalf("event kinds = %v, want %v (conversation strictly first)", got, wantKinds)
	}
	gotConversation := events[0].Conversation
	if gotConversation == nil {
		t.Fatal("leading event has nil Conversation")
	}
	if gotConversation.RemoteConversationID != "conversation-group-1" ||
		gotConversation.Kind != "group" || gotConversation.Title != "Study group" {
		t.Fatalf("leading conversation = %+v", gotConversation)
	}
	wantParticipants := []bridge.Participant{
		{
			Identity: bridge.IdentityRef{Raw: "+15551234567", Name: "Ada Lovelace"},
			Role:     "member",
			Active:   true,
		},
		{
			Identity: bridge.IdentityRef{Raw: "+15550001111", Name: "Me", IsSelf: true},
			Role:     "member",
			Active:   true,
		},
	}
	if !reflect.DeepEqual(gotConversation.Participants, wantParticipants) {
		t.Fatalf("participants = %+v, want %+v", gotConversation.Participants, wantParticipants)
	}

	// Differential: the embedded snapshot maps exactly like the same snapshot
	// delivered as a standalone live conversation frame.
	liveConversation, err := ingest.GoogleConversationRecord(hdecAccountID, hdecGeneration, conversation, hdecReceivedAt)
	if err != nil {
		t.Fatalf("GoogleConversationRecord: %v", err)
	}
	liveConversationEvents, err := ingest.NewGoogleDecoder(nil).Decode(context.Background(), liveConversation)
	if err != nil {
		t.Fatalf("Decode(live conversation): %v", err)
	}
	if len(liveConversationEvents) != 1 || !reflect.DeepEqual(liveConversationEvents[0], events[0]) {
		t.Fatalf("embedded conversation event = %+v, live conversation frame = %+v", events[0], liveConversationEvents)
	}

	if events[1].Message.RemoteMessageID != "history-group-message" ||
		events[1].Message.RemoteConversationID != "conversation-group-1" ||
		events[1].Message.Body != "hello group" {
		t.Fatalf("message event = %+v", events[1].Message)
	}
	for index, actor := range []string{"participant-a", "participant-b"} {
		reaction := events[2+index].Reaction
		if reaction == nil || reaction.TargetRemoteMessageID != "history-group-message" ||
			reaction.Emoji != "👍" || reaction.Actor.Raw != actor || reaction.Action != bridge.ReactionAdd {
			t.Fatalf("reaction %d = %+v", index, reaction)
		}
	}
}

func TestHistoryDecoderRejectsConversationForAnotherThread(t *testing.T) {
	t.Parallel()

	message := hdecIncomingMessage("history-mismatch", "conversation-a", "body")
	for _, test := range []struct {
		name           string
		conversationID string
	}{
		{name: "different thread", conversationID: "conversation-b"},
		{name: "prefix of message thread", conversationID: "conversation-"},
		{name: "padded copy of message thread", conversationID: " conversation-a"},
		{name: "empty conversation id with other fields", conversationID: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			conversation := hdecGroupConversation(test.conversationID)
			payload := hdecMessagePayload(t, message, true, conversation)
			record := hdecRecord(ingest.GoogleHistoryCodec, payload)
			events, err := ingest.NewGoogleDecoder(nil).Decode(context.Background(), record)
			if err == nil {
				t.Fatalf("Decode(conversation %q for message thread %q) = %+v, nil error",
					test.conversationID, message.GetConversationID(), events)
			}
			if events != nil {
				t.Fatalf("rejected decode returned events %+v, want nil", events)
			}
			if !strings.Contains(err.Error(), "does not match message conversation") {
				t.Fatalf("error = %v, want conversation mismatch", err)
			}
		})
	}

	// Control: the same hand-crafted envelope with the matching thread decodes.
	payload := hdecMessagePayload(t, message, true, hdecGroupConversation("conversation-a"))
	events, err := ingest.NewGoogleDecoder(nil).Decode(context.Background(), hdecRecord(ingest.GoogleHistoryCodec, payload))
	if err != nil {
		t.Fatalf("control Decode(matching conversation): %v", err)
	}
	if got := hdecKinds(events); !reflect.DeepEqual(got, []bridge.EventKind{bridge.EventConversation, bridge.EventMessage}) {
		t.Fatalf("control event kinds = %v", got)
	}
}

func TestHistoryDecoderRejectsConversationOnLiveCodec(t *testing.T) {
	t.Parallel()

	message := hdecIncomingMessage("live-with-snapshot", "conversation-live", "body")
	record, err := ingest.GoogleHistoryMessageRecord(
		hdecAccountID, hdecGeneration, "conversation-live",
		hdecGroupConversation("conversation-live"), message, hdecReceivedAt,
	)
	if err != nil {
		t.Fatalf("GoogleHistoryMessageRecord: %v", err)
	}
	// The payload carries conversation_b64; relabelled as a live frame it must
	// be refused rather than silently applying a snapshot under live semantics.
	live := record
	live.Codec = ingest.GoogleCodec
	if events, err := ingest.NewGoogleDecoder(nil).Decode(context.Background(), live); err == nil {
		t.Fatalf("Decode(live codec with conversation_b64) = %+v, nil error", events)
	} else if !strings.Contains(err.Error(), "only valid on history message frames") {
		t.Fatalf("error = %v, want history-only conversation_b64 rejection", err)
	}
	// is_old=false on the hand-crafted live frame changes nothing.
	payload := hdecMessagePayload(t, message, false, hdecGroupConversation("conversation-live"))
	if events, err := ingest.NewGoogleDecoder(nil).Decode(context.Background(), hdecRecord(ingest.GoogleCodec, payload)); err == nil {
		t.Fatalf("Decode(live is_old=false with conversation_b64) = %+v, nil error", events)
	}

	// Control: the untouched history record decodes.
	if _, err := ingest.NewGoogleDecoder(nil).Decode(context.Background(), record); err != nil {
		t.Fatalf("control Decode(history): %v", err)
	}
}

func TestHistoryDecoderRejectsConversationOnConversationFrame(t *testing.T) {
	t.Parallel()

	conversation := hdecGroupConversation("conversation-frame")
	protoBytes, err := proto.Marshal(conversation)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(hdecEnvelope{
		Kind:            "conversation",
		ProtoB64:        protoBytes,
		ConversationB64: protoBytes,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, codec := range []string{ingest.GoogleHistoryCodec, ingest.GoogleCodec} {
		if events, err := ingest.NewGoogleDecoder(nil).Decode(context.Background(), hdecRecord(codec, payload)); err == nil {
			t.Fatalf("Decode(%s conversation frame with conversation_b64) = %+v, nil error", codec, events)
		} else if !strings.Contains(err.Error(), "only valid on history message frames") {
			t.Fatalf("Decode(%s) error = %v, want history-message-only rejection", codec, err)
		}
	}

	// Control: a plain history conversation frame decodes to exactly one
	// conversation event.
	record, err := ingest.GoogleHistoryConversationRecord(hdecAccountID, hdecGeneration, conversation, hdecReceivedAt)
	if err != nil {
		t.Fatalf("GoogleHistoryConversationRecord: %v", err)
	}
	events, err := ingest.NewGoogleDecoder(nil).Decode(context.Background(), record)
	if err != nil {
		t.Fatalf("control Decode(history conversation): %v", err)
	}
	if len(events) != 1 || events[0].Kind != bridge.EventConversation ||
		events[0].Conversation.RemoteConversationID != "conversation-frame" {
		t.Fatalf("control events = %+v", events)
	}
}

func TestHistoryDecoderRejectsMalformedEmbeddedConversation(t *testing.T) {
	t.Parallel()

	message := hdecIncomingMessage("history-bad-snapshot", "conversation-a", "body")
	protoBytes, err := proto.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	isOld := true
	payload, err := json.Marshal(hdecEnvelope{
		Kind:            "message",
		ProtoB64:        protoBytes,
		IsOld:           &isOld,
		ConversationB64: []byte{0xff},
	})
	if err != nil {
		t.Fatal(err)
	}
	if events, err := ingest.NewGoogleDecoder(nil).Decode(context.Background(), hdecRecord(ingest.GoogleHistoryCodec, payload)); err == nil {
		t.Fatalf("Decode(invalid conversation_b64) = %+v, nil error", events)
	}
}

func TestHistoryDecoderEmptyStubStillYieldsConversation(t *testing.T) {
	t.Parallel()

	stub := &gmproto.Message{
		MessageID:      "history-empty-stub",
		ConversationID: "conversation-stub",
		Timestamp:      1_700_000_000_000_000,
		MessageStatus:  &gmproto.MessageStatus{Status: gmproto.MessageStatusType_INCOMING_COMPLETE},
	}

	var counters ingest.Counters
	decoder := ingest.NewGoogleDecoder(&counters)
	withConversation, err := ingest.GoogleHistoryMessageRecord(
		hdecAccountID, hdecGeneration, "conversation-stub",
		hdecGroupConversation("conversation-stub"), stub, hdecReceivedAt,
	)
	if err != nil {
		t.Fatalf("GoogleHistoryMessageRecord(with conversation): %v", err)
	}
	events, err := decoder.Decode(context.Background(), withConversation)
	if err != nil {
		t.Fatalf("Decode(stub with conversation): %v", err)
	}
	if len(events) != 1 || events[0].Kind != bridge.EventConversation ||
		events[0].Conversation.RemoteConversationID != "conversation-stub" ||
		events[0].Conversation.Kind != "group" {
		t.Fatalf("stub-with-conversation events = %+v, want only the conversation event", events)
	}
	if got := counters.Snapshot(hdecAccountID).EmptyStubsSkipped; got != 1 {
		t.Fatalf("empty_stubs_skipped after stub with conversation = %d, want 1", got)
	}

	withoutConversation, err := ingest.GoogleHistoryMessageRecord(
		hdecAccountID, hdecGeneration, "conversation-stub", nil, stub, hdecReceivedAt,
	)
	if err != nil {
		t.Fatalf("GoogleHistoryMessageRecord(without conversation): %v", err)
	}
	events, err = decoder.Decode(context.Background(), withoutConversation)
	if err != nil {
		t.Fatalf("Decode(stub without conversation): %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("stub-without-conversation events = %+v, want none", events)
	}
	if got := counters.Snapshot(hdecAccountID).EmptyStubsSkipped; got != 2 {
		t.Fatalf("empty_stubs_skipped after both stubs = %d, want 2", got)
	}
}

func TestHistoryDecoderEnvelopeShape(t *testing.T) {
	t.Parallel()

	conversation := hdecGroupConversation("conversation-shape")
	message := hdecIncomingMessage("history-shape", "conversation-shape", "body")
	record, err := ingest.GoogleHistoryMessageRecord(
		hdecAccountID, hdecGeneration, "conversation-shape", conversation, message, hdecReceivedAt,
	)
	if err != nil {
		t.Fatalf("GoogleHistoryMessageRecord: %v", err)
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(record.Payload, &envelope); err != nil {
		t.Fatalf("decode history envelope: %v", err)
	}
	assertJSONString(t, envelope["kind"], "message")
	wantMessage, err := proto.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	assertEnvelopeProto(t, envelope["proto_b64"], wantMessage)
	wantConversation, err := proto.Marshal(conversation)
	if err != nil {
		t.Fatal(err)
	}
	assertEnvelopeProto(t, envelope["conversation_b64"], wantConversation)
	var isOld bool
	if raw, ok := envelope["is_old"]; !ok {
		t.Fatal("history message envelope omitted is_old")
	} else if err := json.Unmarshal(raw, &isOld); err != nil {
		t.Fatalf("decode is_old: %v", err)
	} else if !isOld {
		t.Fatal("history message envelope is_old = false, want true (fetched history is never a fresh push)")
	}
	if len(envelope) != 4 {
		t.Fatalf("history message envelope keys = %v, want exactly kind/proto_b64/is_old/conversation_b64", hdecKeys(envelope))
	}

	// Without a snapshot the history frame still says is_old=true and has no
	// conversation_b64 key at all.
	bare, err := ingest.GoogleHistoryMessageRecord(
		hdecAccountID, hdecGeneration, "conversation-shape", nil, message, hdecReceivedAt,
	)
	if err != nil {
		t.Fatalf("GoogleHistoryMessageRecord(bare): %v", err)
	}
	envelope = nil
	if err := json.Unmarshal(bare.Payload, &envelope); err != nil {
		t.Fatalf("decode bare history envelope: %v", err)
	}
	if _, ok := envelope["conversation_b64"]; ok {
		t.Fatal("bare history envelope contains conversation_b64")
	}
	if err := json.Unmarshal(envelope["is_old"], &isOld); err != nil || !isOld {
		t.Fatalf("bare history envelope is_old = %s (err %v), want true", envelope["is_old"], err)
	}

	// History conversation frames have the live conversation shape.
	conversationRecord, err := ingest.GoogleHistoryConversationRecord(hdecAccountID, hdecGeneration, conversation, hdecReceivedAt)
	if err != nil {
		t.Fatalf("GoogleHistoryConversationRecord: %v", err)
	}
	envelope = nil
	if err := json.Unmarshal(conversationRecord.Payload, &envelope); err != nil {
		t.Fatalf("decode history conversation envelope: %v", err)
	}
	assertJSONString(t, envelope["kind"], "conversation")
	assertEnvelopeProto(t, envelope["proto_b64"], wantConversation)
	if len(envelope) != 2 {
		t.Fatalf("history conversation envelope keys = %v, want exactly kind/proto_b64", hdecKeys(envelope))
	}
}

// History keys live in their own namespace ("hmsg"): equal across re-fetches
// of the same bytes, never equal to the live key for those bytes. If the two
// origins shared a key, a fetched copy that reached the inbox first would
// swallow a later byte-identical live push (it would be replayed against an
// already-processed history row and dropped as a stale replay).
func TestHistoryRecordMessageKeyIsDisjointFromLiveKey(t *testing.T) {
	t.Parallel()

	message := hdecIncomingMessage("shared-key-message", "conversation-shared", "same bytes")
	message.MessageInfo = append(message.MessageInfo, &gmproto.MessageInfo{
		Data: &gmproto.MessageInfo_MediaContent{MediaContent: &gmproto.MediaContent{
			MediaID: "media-shared", MimeType: "image/png", DecryptionKey: []byte{1, 2, 3},
		}},
	})
	protoBytes, err := proto.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	wantKey := ingest.GoogleIngressDedupeKey("hmsg", "shared-key-message", protoBytes)
	if !strings.HasPrefix(wantKey, "hmsg:shared-key-message:") || len(wantKey) != len("hmsg:shared-key-message:")+8 {
		t.Fatalf("GoogleIngressDedupeKey = %q, want hmsg:<id>:<8 hex chars>", wantKey)
	}
	liveKey := ingest.GoogleIngressDedupeKey("msg", "shared-key-message", protoBytes)

	history, err := ingest.GoogleHistoryMessageRecord(
		hdecAccountID, hdecGeneration, "conversation-shared",
		hdecGroupConversation("conversation-shared"), message, hdecReceivedAt,
	)
	if err != nil {
		t.Fatalf("GoogleHistoryMessageRecord: %v", err)
	}
	if history.AccountID != hdecAccountID || history.Generation != hdecGeneration ||
		history.Codec != ingest.GoogleHistoryCodec || history.CodecVersion != ingest.GoogleCodecVersion ||
		!history.ReceivedAt.Equal(hdecReceivedAt) {
		t.Fatalf("history record identity = %+v", history)
	}
	if history.DedupeKey != wantKey {
		t.Fatalf("history dedupe key = %q, want %q", history.DedupeKey, wantKey)
	}

	// The live tee's record for the identical proto, pushed fresh or replayed
	// (is_old), keeps the live key, which the history key never equals.
	for _, isOld := range []bool{false, true} {
		live, err := ingest.GoogleMessageRecord(
			hdecAccountID, hdecGeneration,
			&libgm.WrappedMessage{Message: message, IsOld: isOld},
			hdecReceivedAt,
		)
		if err != nil {
			t.Fatalf("GoogleMessageRecord(is_old=%v): %v", isOld, err)
		}
		if live.Codec != ingest.GoogleCodec {
			t.Fatalf("live record codec = %q, want %q", live.Codec, ingest.GoogleCodec)
		}
		if live.DedupeKey != liveKey {
			t.Fatalf("live(is_old=%v) key = %q, want %q", isOld, live.DedupeKey, liveKey)
		}
		if live.DedupeKey == history.DedupeKey {
			t.Fatalf("live(is_old=%v) key %q equals the history key; the origins must not share inbox rows", isOld, live.DedupeKey)
		}
		if string(live.Payload) == string(history.Payload) {
			t.Fatal("live and history payloads are identical; history should carry is_old=true and the snapshot")
		}
	}

	// A one-field content change moves the key (dedupe is content-sensitive).
	edited := proto.Clone(message).(*gmproto.Message)
	edited.MessageInfo[0] = textInfo("different bytes")[0]
	editedRecord, err := ingest.GoogleHistoryMessageRecord(
		hdecAccountID, hdecGeneration, "conversation-shared", nil, edited, hdecReceivedAt,
	)
	if err != nil {
		t.Fatalf("GoogleHistoryMessageRecord(edited): %v", err)
	}
	if editedRecord.DedupeKey == history.DedupeKey {
		t.Fatalf("edited message reused key %q", history.DedupeKey)
	}
}

func TestHistoryRecordFillsMissingConversationIDOnClone(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name          string
		ownID         string
		param         string
		wantEffective string
	}{
		{name: "empty own id", ownID: "", param: "conversation-fill", wantEffective: "conversation-fill"},
		{name: "whitespace own id", ownID: "  \t", param: "conversation-fill", wantEffective: "conversation-fill"},
		{name: "padded parameter is trimmed", ownID: "", param: "  conversation-fill \n", wantEffective: "conversation-fill"},
		{name: "own id wins over parameter", ownID: "conversation-own", param: "conversation-fetched-under", wantEffective: "conversation-own"},
	} {
		t.Run(test.name, func(t *testing.T) {
			message := hdecIncomingMessage("fill-message", test.ownID, "body")
			before := proto.Clone(message).(*gmproto.Message)

			record, err := ingest.GoogleHistoryMessageRecord(
				hdecAccountID, hdecGeneration, test.param, nil, message, hdecReceivedAt,
			)
			if err != nil {
				t.Fatalf("GoogleHistoryMessageRecord: %v", err)
			}
			if !proto.Equal(message, before) || message.GetConversationID() != test.ownID {
				t.Fatalf("caller's proto mutated: conversation ID now %q, want %q", message.GetConversationID(), test.ownID)
			}

			var envelope hdecEnvelope
			if err := json.Unmarshal(record.Payload, &envelope); err != nil {
				t.Fatalf("decode envelope: %v", err)
			}
			var framed gmproto.Message
			if err := proto.Unmarshal(envelope.ProtoB64, &framed); err != nil {
				t.Fatalf("decode framed proto: %v", err)
			}
			if framed.GetConversationID() != test.wantEffective {
				t.Fatalf("framed conversation ID = %q, want %q", framed.GetConversationID(), test.wantEffective)
			}
			filled := proto.Clone(before).(*gmproto.Message)
			filled.ConversationID = test.wantEffective
			if !proto.Equal(&framed, filled) {
				t.Fatalf("framed proto = %v, want caller's proto with only the conversation ID filled (%v)", &framed, filled)
			}
			filledBytes, err := proto.Marshal(filled)
			if err != nil {
				t.Fatal(err)
			}
			if want := ingest.GoogleIngressDedupeKey("hmsg", "fill-message", filledBytes); record.DedupeKey != want {
				t.Fatalf("dedupe key = %q, want key of the filled proto %q", record.DedupeKey, want)
			}
			if strings.TrimSpace(test.ownID) == "" {
				originalBytes, err := proto.Marshal(before)
				if err != nil {
					t.Fatal(err)
				}
				if unfilled := ingest.GoogleIngressDedupeKey("msg", "fill-message", originalBytes); record.DedupeKey == unfilled {
					t.Fatalf("dedupe key %q equals the unfilled proto's key; it must reflect the filled clone", unfilled)
				}
			}

			events, err := ingest.NewGoogleDecoder(nil).Decode(context.Background(), record)
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			if len(events) != 1 || events[0].Message.RemoteConversationID != test.wantEffective {
				t.Fatalf("decoded events = %+v, want message in %q", events, test.wantEffective)
			}
		})
	}
}

func TestHistoryRecordRejectsMessageWithoutAnyConversationID(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name  string
		ownID string
		param string
	}{
		{name: "both empty", ownID: "", param: ""},
		{name: "both whitespace", ownID: " ", param: "\t \n"},
		{name: "empty own id, whitespace parameter", ownID: "", param: "   "},
	} {
		t.Run(test.name, func(t *testing.T) {
			message := hdecIncomingMessage("orphan-message", test.ownID, "body")
			before := proto.Clone(message).(*gmproto.Message)
			record, err := ingest.GoogleHistoryMessageRecord(
				hdecAccountID, hdecGeneration, test.param, nil, message, hdecReceivedAt,
			)
			if err == nil {
				t.Fatalf("GoogleHistoryMessageRecord = %+v, nil error; want missing-conversation error", record)
			}
			if !strings.Contains(err.Error(), "no conversation ID") {
				t.Fatalf("error = %v, want missing conversation ID", err)
			}
			if !reflect.DeepEqual(record, bridge.RawIngressRecord{}) {
				t.Fatalf("failed builder returned non-zero record %+v", record)
			}
			if !proto.Equal(message, before) {
				t.Fatal("failed builder mutated the caller's proto")
			}
		})
	}
	if _, err := ingest.GoogleHistoryMessageRecord(
		hdecAccountID, hdecGeneration, "conversation", nil, nil, hdecReceivedAt,
	); err == nil {
		t.Fatal("GoogleHistoryMessageRecord(nil message) returned nil error")
	}
}

func TestHistoryRecordDropsSnapshotOfAnotherConversation(t *testing.T) {
	t.Parallel()

	message := hdecIncomingMessage("foreign-snapshot", "conversation-a", "body")
	bare, err := ingest.GoogleHistoryMessageRecord(
		hdecAccountID, hdecGeneration, "conversation-a", nil, message, hdecReceivedAt,
	)
	if err != nil {
		t.Fatalf("GoogleHistoryMessageRecord(bare): %v", err)
	}
	for _, foreignID := range []string{"conversation-b", "", "conversation-a "} {
		foreign, err := ingest.GoogleHistoryMessageRecord(
			hdecAccountID, hdecGeneration, "conversation-a",
			hdecGroupConversation(foreignID), message, hdecReceivedAt,
		)
		if err != nil {
			t.Fatalf("GoogleHistoryMessageRecord(foreign %q): %v", foreignID, err)
		}
		if string(foreign.Payload) != string(bare.Payload) {
			t.Fatalf("foreign snapshot %q changed the payload:\n got %s\nwant %s", foreignID, foreign.Payload, bare.Payload)
		}
		if foreign.DedupeKey != bare.DedupeKey {
			t.Fatalf("foreign snapshot %q changed the key: %q vs %q", foreignID, foreign.DedupeKey, bare.DedupeKey)
		}
		events, err := ingest.NewGoogleDecoder(nil).Decode(context.Background(), foreign)
		if err != nil {
			t.Fatalf("Decode(foreign %q): %v", foreignID, err)
		}
		if got := hdecKinds(events); !reflect.DeepEqual(got, []bridge.EventKind{bridge.EventMessage}) {
			t.Fatalf("foreign snapshot %q event kinds = %v, want only the message", foreignID, got)
		}
	}

	// A snapshot matching the conversation ID the builder filled in is kept.
	unattributed := hdecIncomingMessage("filled-snapshot", "", "body")
	filled, err := ingest.GoogleHistoryMessageRecord(
		hdecAccountID, hdecGeneration, "conversation-filled",
		hdecGroupConversation("conversation-filled"), unattributed, hdecReceivedAt,
	)
	if err != nil {
		t.Fatalf("GoogleHistoryMessageRecord(filled): %v", err)
	}
	events, err := ingest.NewGoogleDecoder(nil).Decode(context.Background(), filled)
	if err != nil {
		t.Fatalf("Decode(filled): %v", err)
	}
	if got := hdecKinds(events); !reflect.DeepEqual(got, []bridge.EventKind{bridge.EventConversation, bridge.EventMessage}) ||
		events[0].Conversation.RemoteConversationID != "conversation-filled" {
		t.Fatalf("filled-conversation events = %+v", events)
	}
}

func TestHistoryRecordDedupeKeyIgnoresConversationSnapshot(t *testing.T) {
	t.Parallel()

	message := hdecIncomingMessage("snapshot-independent", "conversation-a", "body")
	older := hdecGroupConversation("conversation-a")
	newer := proto.Clone(older).(*gmproto.Conversation)
	newer.Name = "Renamed group"
	newer.Participants = append(newer.Participants, &gmproto.Participant{
		FullName: "Grace Hopper",
		ID:       &gmproto.SmallInfo{Number: "+15559998888"},
	})

	var keys, payloads []string
	for _, conversation := range []*gmproto.Conversation{nil, older, newer} {
		record, err := ingest.GoogleHistoryMessageRecord(
			hdecAccountID, hdecGeneration, "conversation-a", conversation, message, hdecReceivedAt,
		)
		if err != nil {
			t.Fatalf("GoogleHistoryMessageRecord: %v", err)
		}
		keys = append(keys, record.DedupeKey)
		payloads = append(payloads, string(record.Payload))
	}
	if keys[0] != keys[1] || keys[1] != keys[2] {
		t.Fatalf("dedupe keys across snapshots = %q, want all equal", keys)
	}
	if payloads[0] == payloads[1] || payloads[1] == payloads[2] || payloads[0] == payloads[2] {
		t.Fatal("payloads should differ across snapshots (only the key ignores the snapshot)")
	}
}

func TestHistoryRecordConversationUsesHistoryCodecAndOwnKey(t *testing.T) {
	t.Parallel()

	conversation := hdecGroupConversation("conversation-key")
	protoBytes, err := proto.Marshal(conversation)
	if err != nil {
		t.Fatal(err)
	}
	history, err := ingest.GoogleHistoryConversationRecord(hdecAccountID, hdecGeneration, conversation, hdecReceivedAt)
	if err != nil {
		t.Fatalf("GoogleHistoryConversationRecord: %v", err)
	}
	live, err := ingest.GoogleConversationRecord(hdecAccountID, hdecGeneration, conversation, hdecReceivedAt)
	if err != nil {
		t.Fatalf("GoogleConversationRecord: %v", err)
	}
	wantHistory := ingest.GoogleIngressDedupeKey("hconv", "conversation-key", protoBytes)
	wantLive := ingest.GoogleIngressDedupeKey("conv", "conversation-key", protoBytes)
	if history.DedupeKey != wantHistory || live.DedupeKey != wantLive || wantHistory == wantLive {
		t.Fatalf("keys history=%q live=%q, want %q and %q (disjoint)", history.DedupeKey, live.DedupeKey, wantHistory, wantLive)
	}
	if history.Codec != ingest.GoogleHistoryCodec || live.Codec != ingest.GoogleCodec {
		t.Fatalf("codecs history=%q live=%q", history.Codec, live.Codec)
	}
	if history.AccountID != hdecAccountID || history.Generation != hdecGeneration ||
		history.CodecVersion != ingest.GoogleCodecVersion || !history.ReceivedAt.Equal(hdecReceivedAt) {
		t.Fatalf("history conversation record identity = %+v", history)
	}
	if string(history.Payload) != string(live.Payload) {
		t.Fatalf("history conversation payload differs from live:\n%s\n%s", history.Payload, live.Payload)
	}
	if _, err := ingest.GoogleHistoryConversationRecord(hdecAccountID, hdecGeneration, nil, hdecReceivedAt); err == nil {
		t.Fatal("GoogleHistoryConversationRecord(nil) returned nil error")
	}
}

// hdecHistoryCase is one generated fetched message plus the arguments a
// catch-up would pass alongside it.
type hdecHistoryCase struct {
	Message           *gmproto.Message
	ConversationParam string
	Conversation      *gmproto.Conversation
	EffectiveConvID   string
	WantConversation  bool
	LiveIsOld         bool
}

func (hdecHistoryCase) Generate(r *rand.Rand, _ int) reflect.Value {
	effective := "c-" + hdecRandomToken(r, 1, 10)
	c := hdecHistoryCase{EffectiveConvID: effective, ConversationParam: effective, LiveIsOld: r.Intn(2) == 0}
	ownConversationID := effective
	switch r.Intn(8) {
	case 0, 1:
		// Fetched without its own conversation ID: the builder must fill it.
		ownConversationID = ""
		if r.Intn(2) == 0 {
			ownConversationID = strings.Repeat(" ", 1+r.Intn(2))
		}
		if r.Intn(2) == 0 {
			c.ConversationParam = "  " + effective + "\t"
		}
	case 2:
		// The message's own ID wins over the thread it was fetched under.
		c.ConversationParam = effective + "-fetched-under"
	}

	message := &gmproto.Message{
		MessageID:      "m-" + hdecRandomToken(r, 1, 12),
		ConversationID: ownConversationID,
		Timestamp:      hdecRandomTimestamp(r),
	}
	if r.Intn(4) == 0 {
		message.TmpID = "tmp-" + hdecRandomToken(r, 1, 6)
		if r.Intn(3) == 0 {
			message.TmpID = message.MessageID
		}
	}
	statuses := []gmproto.MessageStatusType{
		gmproto.MessageStatusType_INCOMING_COMPLETE,
		gmproto.MessageStatusType_OUTGOING_COMPLETE,
		gmproto.MessageStatusType_OUTGOING_DELIVERED,
		gmproto.MessageStatusType_INCOMING_AUTO_DOWNLOADING,
		gmproto.MessageStatusType_MESSAGE_DELETED,
	}
	if r.Intn(5) != 0 {
		message.MessageStatus = &gmproto.MessageStatus{Status: statuses[r.Intn(len(statuses))]}
	}
	if r.Intn(5) != 0 {
		message.SenderParticipant = hdecRandomParticipant(r)
	}
	var infos []*gmproto.MessageInfo
	if body, ok := hdecRandomBody(r); ok {
		infos = append(infos, textInfo(body)...)
	}
	if r.Intn(3) == 0 {
		media := &gmproto.MediaContent{
			MediaID:       "media-" + hdecRandomToken(r, 1, 8),
			MediaName:     hdecRandomToken(r, 0, 6) + ".bin",
			MimeType:      []string{"image/jpeg", "video/mp4", ""}[r.Intn(3)],
			Size:          r.Int63n(1 << 20),
			DecryptionKey: []byte(hdecRandomToken(r, 0, 8)),
		}
		infos = append(infos, &gmproto.MessageInfo{Data: &gmproto.MessageInfo_MediaContent{MediaContent: media}})
	}
	message.MessageInfo = infos
	if r.Intn(4) == 0 {
		message.ReplyMessage = &gmproto.ReplyMessage{MessageID: "reply-" + hdecRandomToken(r, 1, 6)}
	}
	if r.Intn(4) == 0 {
		for range 1 + r.Intn(2) {
			entry := &gmproto.ReactionEntry{Data: &gmproto.ReactionData{Unicode: []string{"👍", "❤️", "😂"}[r.Intn(3)]}}
			for range r.Intn(3) {
				entry.ParticipantIDs = append(entry.ParticipantIDs, "participant-"+hdecRandomToken(r, 1, 4))
			}
			message.Reactions = append(message.Reactions, entry)
		}
	}
	c.Message = message

	switch r.Intn(3) {
	case 0:
		// No snapshot.
	case 1:
		c.Conversation = hdecRandomConversation(r, effective)
		c.WantConversation = true
	case 2:
		c.Conversation = hdecRandomConversation(r, effective+"-other")
	}
	return reflect.ValueOf(c)
}

// Differential property: a history frame decodes to exactly the events the
// equivalent live frame decodes to, preceded by one conversation event iff a
// snapshot of the message's own thread was supplied.
func TestHistoryRecordDecodeMatchesLiveProperty(t *testing.T) {
	t.Parallel()

	// Class counts guard against a vacuous run: each interesting shape must be
	// generated at least once under the fixed seed.
	seen := map[string]int{}
	property := func(c hdecHistoryCase) bool {
		if strings.TrimSpace(c.Message.GetConversationID()) == "" {
			seen["filled conversation id"]++
		}
		if c.Conversation != nil && !c.WantConversation {
			seen["foreign snapshot"]++
		}
		before := proto.Clone(c.Message).(*gmproto.Message)
		history, err := ingest.GoogleHistoryMessageRecord(
			hdecAccountID, hdecGeneration, c.ConversationParam, c.Conversation, c.Message, hdecReceivedAt,
		)
		if err != nil {
			t.Errorf("GoogleHistoryMessageRecord(%v): %v", c.Message, err)
			return false
		}
		if !proto.Equal(c.Message, before) {
			t.Errorf("builder mutated caller proto %v", before)
			return false
		}
		liveMessage := proto.Clone(c.Message).(*gmproto.Message)
		liveMessage.ConversationID = c.EffectiveConvID
		live, err := ingest.GoogleMessageRecord(
			hdecAccountID, hdecGeneration,
			&libgm.WrappedMessage{Message: liveMessage, IsOld: c.LiveIsOld},
			hdecReceivedAt,
		)
		if err != nil {
			t.Errorf("GoogleMessageRecord: %v", err)
			return false
		}
		// Same content hash, disjoint namespaces.
		if history.DedupeKey != "h"+live.DedupeKey {
			t.Errorf("history key %q, want \"h\"+live key %q for %v", history.DedupeKey, live.DedupeKey, liveMessage)
			return false
		}

		var historyCounters, liveCounters ingest.Counters
		historyEvents, historyErr := ingest.NewGoogleDecoder(&historyCounters).Decode(context.Background(), history)
		liveEvents, liveErr := ingest.NewGoogleDecoder(&liveCounters).Decode(context.Background(), live)
		if (historyErr == nil) != (liveErr == nil) {
			t.Errorf("decode errors diverge: history=%v live=%v for %v", historyErr, liveErr, liveMessage)
			return false
		}
		if historyErr != nil {
			// Both rejected the same message; nothing further to compare.
			seen["both rejected"]++
			return true
		}
		switch {
		case len(liveEvents) == 0 && c.WantConversation:
			seen["stub with snapshot"]++
		case len(liveEvents) == 0:
			seen["stub without snapshot"]++
		case c.WantConversation:
			seen["message with snapshot"]++
		}
		for _, event := range liveEvents {
			if event.Kind == bridge.EventReaction {
				seen["reaction"]++
				break
			}
		}

		conversationEvents := 0
		for _, event := range historyEvents {
			if event.Kind == bridge.EventConversation {
				conversationEvents++
			}
		}
		wantConversationEvents := 0
		if c.WantConversation {
			wantConversationEvents = 1
		}
		if conversationEvents != wantConversationEvents {
			t.Errorf("conversation events = %d, want %d (snapshot %v, effective %q)",
				conversationEvents, wantConversationEvents, c.Conversation, c.EffectiveConvID)
			return false
		}
		messageEvents := historyEvents
		if c.WantConversation {
			if historyEvents[0].Kind != bridge.EventConversation {
				t.Errorf("first history event kind = %v, want conversation", historyEvents[0].Kind)
				return false
			}
			liveConversation, err := ingest.GoogleConversationRecord(hdecAccountID, hdecGeneration, c.Conversation, hdecReceivedAt)
			if err != nil {
				t.Errorf("GoogleConversationRecord: %v", err)
				return false
			}
			wantConversation, err := ingest.NewGoogleDecoder(nil).Decode(context.Background(), liveConversation)
			if err != nil || len(wantConversation) != 1 || !reflect.DeepEqual(historyEvents[0], wantConversation[0]) {
				t.Errorf("embedded conversation event %+v != live conversation decode %+v (err %v)",
					historyEvents[0], wantConversation, err)
				return false
			}
			messageEvents = historyEvents[1:]
		}
		// Compare element-wise: slicing off the conversation leaves an empty
		// non-nil slice where the live decode of a stub returns nil.
		if len(messageEvents) != len(liveEvents) ||
			(len(liveEvents) > 0 && !reflect.DeepEqual(messageEvents, liveEvents)) {
			t.Errorf("history message events differ from live:\nhistory %s\n   live %s\nmessage %v",
				hdecDescribe(messageEvents), hdecDescribe(liveEvents), liveMessage)
			return false
		}
		if len(liveEvents) > 0 && liveEvents[0].Kind == bridge.EventMessage {
			got, want := messageEvents[0].Message, liveEvents[0].Message
			if got.RemoteConversationID != c.EffectiveConvID ||
				got.RemoteConversationID != want.RemoteConversationID ||
				got.RemoteMessageID != want.RemoteMessageID || got.Body != want.Body ||
				!got.OccurredAt.Equal(want.OccurredAt) || got.Direction != want.Direction {
				t.Errorf("message event fields: history %+v live %+v", got, want)
				return false
			}
		}
		if historyCounters.Snapshot(hdecAccountID) != liveCounters.Snapshot(hdecAccountID) {
			t.Errorf("decoder counters diverge: history %+v live %+v",
				historyCounters.Snapshot(hdecAccountID), liveCounters.Snapshot(hdecAccountID))
			return false
		}
		return true
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 500, Rand: rand.New(rand.NewSource(20261008))}); err != nil {
		t.Fatal(err)
	}
	for _, class := range []string{
		"filled conversation id", "foreign snapshot", "stub with snapshot",
		"stub without snapshot", "message with snapshot", "reaction",
	} {
		if seen[class] == 0 {
			t.Errorf("generator never produced class %q; seen = %v", class, seen)
		}
	}
	t.Logf("generated classes: %v", seen)
}

func hdecIncomingMessage(messageID, conversationID, body string) *gmproto.Message {
	return &gmproto.Message{
		MessageID:      messageID,
		ConversationID: conversationID,
		Timestamp:      1_701_234_567_890_123,
		MessageStatus:  &gmproto.MessageStatus{Status: gmproto.MessageStatusType_INCOMING_COMPLETE},
		SenderParticipant: &gmproto.Participant{
			FullName: "Ada Lovelace",
			ID:       &gmproto.SmallInfo{Number: "+15551234567"},
		},
		MessageInfo: textInfo(body),
	}
}

func hdecGroupConversation(conversationID string) *gmproto.Conversation {
	return &gmproto.Conversation{
		ConversationID: conversationID,
		Name:           "Study group",
		IsGroupChat:    true,
		Participants: []*gmproto.Participant{
			{FullName: "Ada Lovelace", ID: &gmproto.SmallInfo{Number: "+15551234567"}},
			{FullName: "Me", IsMe: true, ID: &gmproto.SmallInfo{}, FormattedNumber: "+15550001111"},
		},
	}
}

func hdecMessagePayload(
	t *testing.T,
	message *gmproto.Message,
	isOld bool,
	conversation *gmproto.Conversation,
) []byte {
	t.Helper()
	messageBytes, err := proto.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	envelope := hdecEnvelope{Kind: "message", ProtoB64: messageBytes, IsOld: &isOld}
	if conversation != nil {
		if envelope.ConversationB64, err = proto.Marshal(conversation); err != nil {
			t.Fatal(err)
		}
		if len(envelope.ConversationB64) == 0 {
			t.Fatal("test snapshot marshals to zero bytes and would be omitted")
		}
	}
	payload, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func hdecRecord(codec string, payload []byte) bridge.RawIngressRecord {
	return bridge.RawIngressRecord{
		AccountID:    hdecAccountID,
		Generation:   hdecGeneration,
		Codec:        codec,
		CodecVersion: ingest.GoogleCodecVersion,
		ReceivedAt:   hdecReceivedAt,
		Payload:      payload,
	}
}

func hdecKinds(events []bridge.Event) []bridge.EventKind {
	kinds := make([]bridge.EventKind, 0, len(events))
	for _, event := range events {
		kinds = append(kinds, event.Kind)
	}
	return kinds
}

func hdecKeys(envelope map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(envelope))
	for key := range envelope {
		keys = append(keys, key)
	}
	return keys
}

func hdecDescribe(events []bridge.Event) string {
	parts := make([]string, 0, len(events))
	for _, event := range events {
		switch {
		case event.Message != nil:
			parts = append(parts, fmt.Sprintf("message%+v", *event.Message))
		case event.Reaction != nil:
			parts = append(parts, fmt.Sprintf("reaction%+v", *event.Reaction))
		case event.Conversation != nil:
			parts = append(parts, fmt.Sprintf("conversation%+v", *event.Conversation))
		default:
			parts = append(parts, fmt.Sprintf("%+v", event))
		}
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

const hdecAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789-_:"

func hdecRandomToken(r *rand.Rand, minLength, maxLength int) string {
	length := minLength + r.Intn(maxLength-minLength+1)
	var builder strings.Builder
	for range length {
		builder.WriteByte(hdecAlphabet[r.Intn(len(hdecAlphabet))])
	}
	return builder.String()
}

func hdecRandomTimestamp(r *rand.Rand) int64 {
	switch r.Intn(10) {
	case 0:
		return 0
	case 1:
		return -r.Int63n(1_000_000_000_000)
	case 2:
		return r.Int63n(1000) // sub-millisecond microsecond values
	default:
		return 1_500_000_000_000_000 + r.Int63n(500_000_000_000_000)
	}
}

func hdecRandomBody(r *rand.Rand) (string, bool) {
	switch r.Intn(7) {
	case 0:
		return "", false
	case 1:
		return "", true
	case 2:
		return "  \n ", true
	case 3:
		return "héllo wörld ✓ " + hdecRandomToken(r, 0, 8), true
	case 4:
		return []string{`Loved "see you soon"`, `Removed a like from "hello"`, `Laughed at "ok"`}[r.Intn(3)], true
	default:
		return hdecRandomToken(r, 1, 40), true
	}
}

func hdecRandomParticipant(r *rand.Rand) *gmproto.Participant {
	participant := &gmproto.Participant{IsMe: r.Intn(4) == 0}
	if r.Intn(3) != 0 {
		participant.FullName = "Name " + hdecRandomToken(r, 1, 6)
	} else if r.Intn(2) == 0 {
		participant.FirstName = "First " + hdecRandomToken(r, 1, 4)
	}
	number := fmt.Sprintf("+1555%07d", r.Intn(10_000_000))
	switch r.Intn(4) {
	case 0:
		participant.ID = &gmproto.SmallInfo{Number: number}
	case 1:
		participant.ID = &gmproto.SmallInfo{}
		participant.FormattedNumber = "(555) " + number[5:8] + "-" + number[8:]
	case 2:
		participant.FormattedNumber = number
	}
	return participant
}

func hdecRandomConversation(r *rand.Rand, conversationID string) *gmproto.Conversation {
	conversation := &gmproto.Conversation{
		ConversationID: conversationID,
		Name:           hdecRandomToken(r, 0, 12),
		IsGroupChat:    r.Intn(2) == 0,
	}
	for range r.Intn(4) {
		conversation.Participants = append(conversation.Participants, hdecRandomParticipant(r))
	}
	return conversation
}
