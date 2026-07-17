package ingest_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"go.mau.fi/mautrix-gmessages/pkg/libgm"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
	"google.golang.org/protobuf/proto"

	"github.com/maxghenis/openmessage/internal/bridge"
	googleadapter "github.com/maxghenis/openmessage/internal/bridgeadapters/google"
	"github.com/maxghenis/openmessage/internal/ingest"
)

func TestGoogleFrameMarshalEnvelopeShapes(t *testing.T) {
	t.Parallel()

	message := &gmproto.Message{MessageID: "message-1", ConversationID: "conversation-1"}
	payload, protoBytes, err := ingest.MarshalGoogleMessageFrame(&libgm.WrappedMessage{
		Message: message,
		IsOld:   false,
	})
	if err != nil {
		t.Fatalf("MarshalGoogleMessageFrame: %v", err)
	}
	wantProto, err := proto.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(protoBytes, wantProto) {
		t.Fatalf("message proto bytes = %x, want %x", protoBytes, wantProto)
	}
	var messageEnvelope map[string]json.RawMessage
	if err := json.Unmarshal(payload, &messageEnvelope); err != nil {
		t.Fatalf("decode message envelope: %v", err)
	}
	assertJSONString(t, messageEnvelope["kind"], "message")
	assertEnvelopeProto(t, messageEnvelope["proto_b64"], wantProto)
	var isOld bool
	if raw, ok := messageEnvelope["is_old"]; !ok {
		t.Fatal("message envelope omitted is_old=false")
	} else if err := json.Unmarshal(raw, &isOld); err != nil {
		t.Fatalf("decode message is_old: %v", err)
	} else if isOld {
		t.Fatal("message envelope is_old = true, want false")
	}

	conversation := &gmproto.Conversation{ConversationID: "conversation-2", Name: "Thread"}
	payload, protoBytes, err = ingest.MarshalGoogleConversationFrame(conversation)
	if err != nil {
		t.Fatalf("MarshalGoogleConversationFrame: %v", err)
	}
	wantProto, err = proto.Marshal(conversation)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(protoBytes, wantProto) {
		t.Fatalf("conversation proto bytes = %x, want %x", protoBytes, wantProto)
	}
	var conversationEnvelope map[string]json.RawMessage
	if err := json.Unmarshal(payload, &conversationEnvelope); err != nil {
		t.Fatalf("decode conversation envelope: %v", err)
	}
	assertJSONString(t, conversationEnvelope["kind"], "conversation")
	assertEnvelopeProto(t, conversationEnvelope["proto_b64"], wantProto)
	if _, ok := conversationEnvelope["is_old"]; ok {
		t.Fatal("conversation envelope unexpectedly contains is_old")
	}
}

func TestGoogleDecoderMapsMessageAndReactions(t *testing.T) {
	t.Parallel()

	const timestampUS = int64(1_701_234_567_890_123)
	message := &gmproto.Message{
		MessageID:      "message-1",
		ConversationID: "conversation-1",
		Timestamp:      timestampUS,
		MessageStatus: &gmproto.MessageStatus{
			Status: gmproto.MessageStatusType_INCOMING_COMPLETE,
		},
		SenderParticipant: &gmproto.Participant{
			FullName: "Ada Lovelace",
			ID:       &gmproto.SmallInfo{Number: "+15551234567"},
		},
		MessageInfo: []*gmproto.MessageInfo{
			{
				Data: &gmproto.MessageInfo_MessageContent{
					MessageContent: &gmproto.MessageContent{Content: "photo caption"},
				},
			},
			{
				Data: &gmproto.MessageInfo_MediaContent{
					MediaContent: &gmproto.MediaContent{
						MediaID:       "media-1",
						MediaName:     "photo.jpg",
						MimeType:      "image/jpeg",
						Size:          321,
						DecryptionKey: []byte{0xde, 0xad, 0xbe, 0xef},
					},
				},
			},
		},
		ReplyMessage: &gmproto.ReplyMessage{MessageID: "message-0"},
		Reactions: []*gmproto.ReactionEntry{
			{
				Data:           &gmproto.ReactionData{Unicode: "😂"},
				ParticipantIDs: []string{"participant-a", "participant-b"},
			},
			{
				Data: &gmproto.ReactionData{Unicode: "❤️"},
			},
		},
	}

	events := decodeGoogleMessage(t, ingest.NewGoogleDecoder(nil), "account-1", message, false)
	if len(events) != 4 {
		t.Fatalf("event count = %d, want message + three per-actor reactions", len(events))
	}
	if events[0].Kind != bridge.EventMessage || events[0].Message == nil {
		t.Fatalf("first event = %+v, want MessageEvent", events[0])
	}
	got := events[0].Message
	wantOccurredAt := time.UnixMilli(timestampUS / 1000)
	if got.RemoteConversationID != "conversation-1" || got.RemoteMessageID != "message-1" ||
		got.ClientRequestID != "" || got.Direction != "incoming" || got.Body != "photo caption" ||
		got.ReplyToRemoteID != "message-0" || !got.OccurredAt.Equal(wantOccurredAt) {
		t.Fatalf("message event = %+v", got)
	}
	if got.Sender.Raw != "+15551234567" || got.Sender.Name != "Ada Lovelace" || got.Sender.IsSelf {
		t.Fatalf("sender = %+v", got.Sender)
	}
	if len(got.Attachments) != 1 {
		t.Fatalf("attachments = %+v, want one", got.Attachments)
	}
	attachment := got.Attachments[0]
	if attachment.RemoteID != "media-1" || attachment.Filename != "photo.jpg" ||
		attachment.MIME != "image/jpeg" || attachment.Size != 321 {
		t.Fatalf("attachment = %+v", attachment)
	}
	if err := googleadapter.ValidateDownloadOpaque(attachment.RemoteRef); err != nil {
		t.Fatalf("ValidateDownloadOpaque: %v; opaque=%s", err, attachment.RemoteRef)
	}
	var opaque struct {
		Version       int    `json:"v"`
		MediaID       string `json:"media_id"`
		DecryptionKey string `json:"decryption_key"`
	}
	if err := json.Unmarshal(attachment.RemoteRef, &opaque); err != nil {
		t.Fatalf("decode attachment opaque: %v", err)
	}
	if opaque.Version != 1 || opaque.MediaID != "media-1" ||
		opaque.DecryptionKey != hex.EncodeToString([]byte{0xde, 0xad, 0xbe, 0xef}) {
		t.Fatalf("attachment opaque = %+v", opaque)
	}

	wantReactions := []struct {
		emoji string
		actor string
	}{
		{emoji: "😂", actor: "participant-a"},
		{emoji: "😂", actor: "participant-b"},
		{emoji: "❤️", actor: ""},
	}
	for index, want := range wantReactions {
		event := events[index+1]
		if event.Kind != bridge.EventReaction || event.Reaction == nil {
			t.Fatalf("event %d = %+v, want ReactionEvent", index+1, event)
		}
		reaction := event.Reaction
		if reaction.RemoteConversationID != "conversation-1" ||
			reaction.TargetRemoteMessageID != "message-1" ||
			reaction.Emoji != want.emoji || reaction.Actor.Raw != want.actor ||
			reaction.Action != bridge.ReactionAdd || !reaction.OccurredAt.Equal(wantOccurredAt) {
			t.Fatalf("reaction %d = %+v", index, reaction)
		}
	}
	for _, event := range events {
		if event.Kind == bridge.EventReceipt || event.Receipt != nil {
			t.Fatalf("Google decoder emitted forbidden receipt event: %+v", event)
		}
	}
}

func TestGoogleDecoderOutgoingClientRequestMapping(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		status        gmproto.MessageStatusType
		senderIsSelf  bool
		tmpID         string
		messageID     string
		wantDirection string
		wantRequestID string
	}{
		{
			name:          "outgoing status fallback correlates distinct tmp id",
			status:        gmproto.MessageStatusType_OUTGOING_COMPLETE,
			tmpID:         "request-1",
			messageID:     "permanent-1",
			wantDirection: "outgoing",
			wantRequestID: "request-1",
		},
		{
			name:          "self participant correlates distinct tmp id",
			status:        gmproto.MessageStatusType_INCOMING_COMPLETE,
			senderIsSelf:  true,
			tmpID:         "request-2",
			messageID:     "permanent-2",
			wantDirection: "outgoing",
			wantRequestID: "request-2",
		},
		{
			name:          "tmp id equal to message id does not correlate",
			status:        gmproto.MessageStatusType_OUTGOING_COMPLETE,
			tmpID:         "same-id",
			messageID:     "same-id",
			wantDirection: "outgoing",
		},
		{
			name:          "incoming tmp id does not correlate",
			status:        gmproto.MessageStatusType_INCOMING_COMPLETE,
			tmpID:         "phone-tmp",
			messageID:     "incoming-1",
			wantDirection: "incoming",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			message := &gmproto.Message{
				MessageID:      test.messageID,
				ConversationID: "conversation-1",
				Timestamp:      1_700_000_000_000_000,
				TmpID:          test.tmpID,
				MessageStatus:  &gmproto.MessageStatus{Status: test.status},
				SenderParticipant: &gmproto.Participant{
					IsMe:     test.senderIsSelf,
					FullName: "Me",
					ID:       &gmproto.SmallInfo{Number: "+15550001111"},
				},
				MessageInfo: textInfo("hello"),
			}
			events := decodeGoogleMessage(t, ingest.NewGoogleDecoder(nil), "account-1", message, false)
			if len(events) != 1 || events[0].Message == nil {
				t.Fatalf("events = %+v, want one MessageEvent", events)
			}
			got := events[0].Message
			if got.Direction != test.wantDirection || got.ClientRequestID != test.wantRequestID {
				t.Fatalf("direction/request = %q/%q, want %q/%q", got.Direction, got.ClientRequestID, test.wantDirection, test.wantRequestID)
			}
			if got.Sender.IsSelf != (test.wantDirection == "outgoing") {
				t.Fatalf("sender IsSelf = %v, direction = %q", got.Sender.IsSelf, got.Direction)
			}
		})
	}
}

func TestGoogleDecoderTapbackPreservesMessageAndCountsDivergence(t *testing.T) {
	t.Parallel()

	var counters ingest.Counters
	decoder := ingest.NewGoogleDecoder(&counters)
	message := &gmproto.Message{
		MessageID:      "tapback-message",
		ConversationID: "conversation-1",
		Timestamp:      1_700_000_000_000_000,
		MessageStatus:  &gmproto.MessageStatus{Status: gmproto.MessageStatusType_INCOMING_COMPLETE},
		SenderParticipant: &gmproto.Participant{
			FullName: "Ada",
			ID:       &gmproto.SmallInfo{Number: "+15551234567"},
		},
		MessageInfo: textInfo(`Removed a like from "hello"`),
	}

	events := decodeGoogleMessage(t, decoder, "account-1", message, false)
	if len(events) != 2 || events[0].Message == nil || events[1].Reaction == nil {
		t.Fatalf("events = %+v, want preserved message plus tapback reaction", events)
	}
	if events[0].Message.Body != `Removed a like from "hello"` {
		t.Fatalf("preserved message body = %q", events[0].Message.Body)
	}
	reaction := events[1].Reaction
	if reaction.TargetRemoteMessageID != "" || reaction.Emoji != "👍" ||
		reaction.Action != bridge.ReactionRemove || reaction.Actor.Raw != "+15551234567" ||
		reaction.Actor.Name != "Ada" || reaction.Actor.IsSelf {
		t.Fatalf("tapback reaction = %+v", reaction)
	}
	if got := counters.Snapshot("account-1").TapbackMessages; got != 1 {
		t.Fatalf("tapback_messages = %d, want 1", got)
	}
}

func TestGoogleDecoderEmptyStubPolicy(t *testing.T) {
	t.Parallel()

	var counters ingest.Counters
	decoder := ingest.NewGoogleDecoder(&counters)
	terminalStub := &gmproto.Message{
		MessageID:      "empty-terminal",
		ConversationID: "conversation-1",
		Timestamp:      1_700_000_000_000_000,
		MessageStatus:  &gmproto.MessageStatus{Status: gmproto.MessageStatusType_INCOMING_COMPLETE},
	}
	if events := decodeGoogleMessage(t, decoder, "account-1", terminalStub, false); len(events) != 0 {
		t.Fatalf("terminal stub events = %+v, want none", events)
	}

	pendingPlaceholder := proto.Clone(terminalStub).(*gmproto.Message)
	pendingPlaceholder.MessageID = "empty-pending"
	pendingPlaceholder.MessageStatus.Status = gmproto.MessageStatusType_INCOMING_AUTO_DOWNLOADING
	events := decodeGoogleMessage(t, decoder, "account-1", pendingPlaceholder, false)
	if len(events) != 1 || events[0].Message == nil {
		t.Fatalf("pending placeholder events = %+v, want one MessageEvent", events)
	}
	if got := counters.Snapshot("account-1").EmptyStubsSkipped; got != 1 {
		t.Fatalf("empty_stubs_skipped = %d, want 1", got)
	}
}

func TestGoogleDecoderMapsConversationSnapshot(t *testing.T) {
	t.Parallel()

	conversation := &gmproto.Conversation{
		ConversationID: "conversation-1",
		Name:           "Study group",
		IsGroupChat:    true,
		Participants: []*gmproto.Participant{
			{
				FullName: "Ada Lovelace",
				ID:       &gmproto.SmallInfo{Number: "+15551234567"},
			},
			{
				FullName:        "Me",
				IsMe:            true,
				ID:              &gmproto.SmallInfo{},
				FormattedNumber: "+15550001111",
			},
		},
	}
	payload, _, err := ingest.MarshalGoogleConversationFrame(conversation)
	if err != nil {
		t.Fatal(err)
	}
	events, err := ingest.NewGoogleDecoder(nil).Decode(context.Background(), bridge.RawIngressRecord{
		AccountID:    "account-1",
		Codec:        ingest.GoogleCodec,
		CodecVersion: ingest.GoogleCodecVersion,
		Payload:      payload,
	})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(events) != 1 || events[0].Kind != bridge.EventConversation || events[0].Conversation == nil {
		t.Fatalf("events = %+v, want one ConversationEvent", events)
	}
	got := events[0].Conversation
	if got.RemoteConversationID != "conversation-1" || got.Kind != "group" || got.Title != "Study group" {
		t.Fatalf("conversation = %+v", got)
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
	if !reflect.DeepEqual(got.Participants, wantParticipants) {
		t.Fatalf("participants = %+v, want %+v", got.Participants, wantParticipants)
	}
}

func TestGoogleDecoderRejectsMalformedOrUnsupportedFrames(t *testing.T) {
	t.Parallel()

	decoder := ingest.NewGoogleDecoder(nil)
	tests := []struct {
		name   string
		record bridge.RawIngressRecord
		ctx    context.Context
	}{
		{
			name: "wrong codec",
			record: bridge.RawIngressRecord{
				Codec: "other", CodecVersion: ingest.GoogleCodecVersion, Payload: []byte(`{}`),
			},
			ctx: context.Background(),
		},
		{
			name: "wrong version",
			record: bridge.RawIngressRecord{
				Codec: ingest.GoogleCodec, CodecVersion: 2, Payload: []byte(`{}`),
			},
			ctx: context.Background(),
		},
		{
			name: "malformed json",
			record: bridge.RawIngressRecord{
				Codec: ingest.GoogleCodec, CodecVersion: ingest.GoogleCodecVersion, Payload: []byte(`{`),
			},
			ctx: context.Background(),
		},
		{
			name: "empty proto",
			record: bridge.RawIngressRecord{
				Codec: ingest.GoogleCodec, CodecVersion: ingest.GoogleCodecVersion,
				Payload: []byte(`{"kind":"message","proto_b64":""}`),
			},
			ctx: context.Background(),
		},
		{
			name: "message missing is_old",
			record: bridge.RawIngressRecord{
				Codec: ingest.GoogleCodec, CodecVersion: ingest.GoogleCodecVersion,
				Payload: []byte(`{"kind":"message","proto_b64":"AQ=="}`),
			},
			ctx: context.Background(),
		},
		{
			name: "unknown kind",
			record: bridge.RawIngressRecord{
				Codec: ingest.GoogleCodec, CodecVersion: ingest.GoogleCodecVersion,
				Payload: []byte(`{"kind":"other","proto_b64":"AQ=="}`),
			},
			ctx: context.Background(),
		},
		{
			// is_old present so decode reaches proto.Unmarshal and fails there,
			// rather than short-circuiting on the missing-field guard.
			name: "invalid protobuf",
			record: bridge.RawIngressRecord{
				Codec: ingest.GoogleCodec, CodecVersion: ingest.GoogleCodecVersion,
				Payload: []byte(`{"kind":"message","is_old":false,"proto_b64":"/w=="}`),
			},
			ctx: context.Background(),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if events, err := decoder.Decode(test.ctx, test.record); err == nil {
				t.Fatalf("Decode() = %+v, nil error", events)
			}
		})
	}
	if _, err := decoder.Decode(nil, bridge.RawIngressRecord{}); err == nil {
		t.Fatal("Decode(nil context) returned nil error")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := decoder.Decode(canceled, bridge.RawIngressRecord{}); err == nil {
		t.Fatal("Decode(canceled context) returned nil error")
	}
}

func TestGoogleAttachmentRemoteRefRoundTrip(t *testing.T) {
	t.Parallel()

	message := &gmproto.Message{
		MessageID:      "media-message",
		ConversationID: "conversation-1",
		Timestamp:      1_700_000_000_000_000,
		MessageStatus:  &gmproto.MessageStatus{Status: gmproto.MessageStatusType_INCOMING_COMPLETE},
		MessageInfo: []*gmproto.MessageInfo{{
			Data: &gmproto.MessageInfo_MediaContent{MediaContent: &gmproto.MediaContent{
				MediaID: "media-id", DecryptionKey: []byte{0x01, 0x02}, MimeType: "image/jpeg",
			}},
		}},
	}
	events := decodeGoogleMessage(t, ingest.NewGoogleDecoder(nil), "account-1", message, false)
	if len(events) != 1 || events[0].Message == nil || len(events[0].Message.Attachments) != 1 {
		t.Fatalf("events = %+v", events)
	}
	if err := googleadapter.ValidateDownloadOpaque(events[0].Message.Attachments[0].RemoteRef); err != nil {
		t.Fatalf("packed RemoteRef failed Google adapter validation: %v", err)
	}
}

func decodeGoogleMessage(
	t *testing.T,
	decoder *ingest.GoogleDecoder,
	accountID string,
	message *gmproto.Message,
	isOld bool,
) []bridge.Event {
	t.Helper()
	payload, _, err := ingest.MarshalGoogleMessageFrame(&libgm.WrappedMessage{
		Message: message,
		IsOld:   isOld,
	})
	if err != nil {
		t.Fatalf("MarshalGoogleMessageFrame: %v", err)
	}
	events, err := decoder.Decode(context.Background(), bridge.RawIngressRecord{
		AccountID:    accountID,
		Codec:        ingest.GoogleCodec,
		CodecVersion: ingest.GoogleCodecVersion,
		Payload:      payload,
	})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	return events
}

func textInfo(body string) []*gmproto.MessageInfo {
	return []*gmproto.MessageInfo{{
		Data: &gmproto.MessageInfo_MessageContent{
			MessageContent: &gmproto.MessageContent{Content: body},
		},
	}}
}

func assertJSONString(t *testing.T, raw json.RawMessage, want string) {
	t.Helper()
	var got string
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode JSON string: %v", err)
	}
	if got != want {
		t.Fatalf("JSON string = %q, want %q", got, want)
	}
}

func assertEnvelopeProto(t *testing.T, raw json.RawMessage, want []byte) {
	t.Helper()
	var encoded string
	if err := json.Unmarshal(raw, &encoded); err != nil {
		t.Fatalf("decode proto_b64 string: %v", err)
	}
	got, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("decode proto_b64: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("decoded proto_b64 = %x, want %x", got, want)
	}
}
