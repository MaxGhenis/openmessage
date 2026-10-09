package ingest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"go.mau.fi/mautrix-gmessages/pkg/libgm"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
	"google.golang.org/protobuf/proto"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/client"
	"github.com/maxghenis/openmessage/internal/db"
)

const (
	// GoogleCodec is the durable Google Messages protobuf envelope codec for
	// frames the live long-poll delivered.
	GoogleCodec = "google.protobuf"
	// GoogleHistoryCodec marks the same envelope for frames a catch-up fetched
	// on request (ListConversations / FetchMessages replies). The distinct codec
	// keeps fetched history out of anything that measures live delivery from
	// inbox rows (the silence and SMS-path monitors filter by exact codec), and
	// selects the worker's insert-only history semantics.
	GoogleHistoryCodec = "google.protobuf.history"
	// GoogleCodecVersion is the only envelope version understood by this decoder.
	GoogleCodecVersion uint32 = 1
)

const (
	googleFrameMessage      = "message"
	googleFrameConversation = "conversation"
)

// googleFrameEnvelope keeps the protobuf byte-preserving while making its
// concrete message type explicit. A non-nil IsOld makes the field appear for
// message frames even when false; conversation frames omit it.
//
// ConversationB64 is set only on history message frames: the conversation
// snapshot the message was fetched under. Carrying it in the same frame makes
// the worker apply the conversation before the message; as separate frames the
// two could be drained in either order (same-millisecond inbox rows sort by a
// random id), and a group message projected before its conversation is
// rerouted by sender into a member's 1:1 thread.
type googleFrameEnvelope struct {
	Kind            string `json:"kind"`
	ProtoB64        []byte `json:"proto_b64"`
	IsOld           *bool  `json:"is_old,omitempty"`
	ConversationB64 []byte `json:"conversation_b64,omitempty"`
}

// GoogleIngressDedupeKey is the inbox dedupe key of a Google frame: the kind,
// the remote ID, and a short hash of the protobuf. Live frames use the kinds
// "msg" and "conv"; history frames use "hmsg" and "hconv", so the two origins
// never collapse onto each other's rows. If they shared a key, a fetched copy
// that reached the inbox first would swallow a later byte-identical live push:
// the push would be replayed against an already-processed history row and
// dropped as a stale replay (losing, say, an attachment that only the live
// path records), and the live-delivery monitors would never see it.
func GoogleIngressDedupeKey(kind, remoteID string, protoBytes []byte) string {
	digest := sha256.Sum256(protoBytes)
	return fmt.Sprintf("%s:%s:%x", kind, remoteID, digest[:4])
}

// GoogleMessageRecord builds the durable ingress record the live tee appends
// for one pushed message.
func GoogleMessageRecord(
	accountID string,
	generation bridge.Generation,
	event *libgm.WrappedMessage,
	receivedAt time.Time,
) (bridge.RawIngressRecord, error) {
	payload, protoBytes, err := MarshalGoogleMessageFrame(event)
	if err != nil {
		return bridge.RawIngressRecord{}, err
	}
	return bridge.RawIngressRecord{
		AccountID:    accountID,
		Generation:   generation,
		DedupeKey:    GoogleIngressDedupeKey("msg", event.GetMessageID(), protoBytes),
		Codec:        GoogleCodec,
		CodecVersion: GoogleCodecVersion,
		ReceivedAt:   receivedAt,
		Payload:      payload,
	}, nil
}

// GoogleConversationRecord builds the durable ingress record the live tee
// appends for one pushed conversation snapshot.
func GoogleConversationRecord(
	accountID string,
	generation bridge.Generation,
	conversation *gmproto.Conversation,
	receivedAt time.Time,
) (bridge.RawIngressRecord, error) {
	return googleConversationRecord(accountID, generation, GoogleCodec, conversation, receivedAt)
}

// GoogleHistoryConversationRecord builds the history-codec record for one
// conversation a catch-up listed.
func GoogleHistoryConversationRecord(
	accountID string,
	generation bridge.Generation,
	conversation *gmproto.Conversation,
	receivedAt time.Time,
) (bridge.RawIngressRecord, error) {
	return googleConversationRecord(accountID, generation, GoogleHistoryCodec, conversation, receivedAt)
}

func googleConversationRecord(
	accountID string,
	generation bridge.Generation,
	codec string,
	conversation *gmproto.Conversation,
	receivedAt time.Time,
) (bridge.RawIngressRecord, error) {
	payload, protoBytes, err := MarshalGoogleConversationFrame(conversation)
	if err != nil {
		return bridge.RawIngressRecord{}, err
	}
	kind := "conv"
	if codec == GoogleHistoryCodec {
		kind = "hconv"
	}
	return bridge.RawIngressRecord{
		AccountID:    accountID,
		Generation:   generation,
		DedupeKey:    GoogleIngressDedupeKey(kind, conversation.GetConversationID(), protoBytes),
		Codec:        codec,
		CodecVersion: GoogleCodecVersion,
		ReceivedAt:   receivedAt,
		Payload:      payload,
	}, nil
}

// GoogleHistoryMessageRecord builds the history-codec record for one message a
// catch-up fetched from conversationID. A fetched message with no conversation
// ID of its own is attributed to conversationID (on a copy; the caller's proto
// is never mutated). conversation, when it is the snapshot of that same
// conversation, rides along in the frame; the dedupe key ("hmsg") covers only
// the message, so a re-fetch of an unchanged message dedupes onto its earlier
// history row even though the conversation snapshot moved on.
func GoogleHistoryMessageRecord(
	accountID string,
	generation bridge.Generation,
	conversationID string,
	conversation *gmproto.Conversation,
	message *gmproto.Message,
	receivedAt time.Time,
) (bridge.RawIngressRecord, error) {
	if message == nil {
		return bridge.RawIngressRecord{}, fmt.Errorf("marshal Google history message frame: message is nil")
	}
	conversationID = strings.TrimSpace(conversationID)
	if strings.TrimSpace(message.GetConversationID()) == "" {
		if conversationID == "" {
			return bridge.RawIngressRecord{}, fmt.Errorf(
				"marshal Google history message frame: message %q has no conversation ID",
				message.GetMessageID(),
			)
		}
		message = proto.Clone(message).(*gmproto.Message)
		message.ConversationID = conversationID
	}
	if conversation != nil && conversation.GetConversationID() != message.GetConversationID() {
		// A snapshot of some other thread proves nothing about this message's
		// thread; let the message route itself exactly as a live frame would.
		conversation = nil
	}
	payload, protoBytes, err := marshalGoogleMessageEnvelope(message, true, conversation)
	if err != nil {
		return bridge.RawIngressRecord{}, err
	}
	return bridge.RawIngressRecord{
		AccountID:    accountID,
		Generation:   generation,
		DedupeKey:    GoogleIngressDedupeKey("hmsg", message.GetMessageID(), protoBytes),
		Codec:        GoogleHistoryCodec,
		CodecVersion: GoogleCodecVersion,
		ReceivedAt:   receivedAt,
		Payload:      payload,
	}, nil
}

// MarshalGoogleMessageFrame serializes the exact v1 frame used by the Google
// adapter tee. protoBytes is returned separately for content-hash dedupe.
func MarshalGoogleMessageFrame(
	event *libgm.WrappedMessage,
) (payload, protoBytes []byte, err error) {
	if event == nil {
		return nil, nil, fmt.Errorf("marshal Google message frame: wrapped message is nil")
	}
	if event.Message == nil {
		return nil, nil, fmt.Errorf("marshal Google message frame: protobuf message is nil")
	}
	return marshalGoogleMessageEnvelope(event.Message, event.IsOld, nil)
}

func marshalGoogleMessageEnvelope(
	message *gmproto.Message,
	isOld bool,
	conversation *gmproto.Conversation,
) (payload, protoBytes []byte, err error) {
	protoBytes, err = proto.Marshal(message)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal Google message protobuf: %w", err)
	}
	envelope := googleFrameEnvelope{
		Kind:     googleFrameMessage,
		ProtoB64: protoBytes,
		IsOld:    &isOld,
	}
	if conversation != nil {
		envelope.ConversationB64, err = proto.Marshal(conversation)
		if err != nil {
			return nil, nil, fmt.Errorf("marshal Google message conversation protobuf: %w", err)
		}
	}
	payload, err = json.Marshal(envelope)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal Google message envelope: %w", err)
	}
	return payload, protoBytes, nil
}

// MarshalGoogleConversationFrame serializes the exact v1 frame used by the
// Google adapter tee. protoBytes is returned separately for content-hash dedupe.
func MarshalGoogleConversationFrame(
	conversation *gmproto.Conversation,
) (payload, protoBytes []byte, err error) {
	if conversation == nil {
		return nil, nil, fmt.Errorf("marshal Google conversation frame: conversation is nil")
	}
	protoBytes, err = proto.Marshal(conversation)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal Google conversation protobuf: %w", err)
	}
	payload, err = json.Marshal(googleFrameEnvelope{
		Kind:     googleFrameConversation,
		ProtoB64: protoBytes,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("marshal Google conversation envelope: %w", err)
	}
	return payload, protoBytes, nil
}

// GoogleDecoder maps durable Google protobuf envelopes to transport-neutral
// events. The zero value is usable; a configured Counters records Google-only
// skip/divergence classifications per account.
type GoogleDecoder struct {
	counters *Counters
}

var _ bridge.Decoder = (*GoogleDecoder)(nil)

// NewGoogleDecoder constructs the v1 Google protobuf decoder.
func NewGoogleDecoder(counters *Counters) *GoogleDecoder {
	return &GoogleDecoder{counters: counters}
}

// Decode parses one v1 envelope and applies only store-free mappings.
func (d *GoogleDecoder) Decode(
	ctx context.Context,
	record bridge.RawIngressRecord,
) ([]bridge.Event, error) {
	if ctx == nil {
		return nil, fmt.Errorf("decode Google ingress: context is nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	history := record.Codec == GoogleHistoryCodec
	if record.Codec != GoogleCodec && !history {
		return nil, fmt.Errorf(
			"decode Google ingress: codec %q is not %q or %q",
			record.Codec,
			GoogleCodec,
			GoogleHistoryCodec,
		)
	}
	if record.CodecVersion != GoogleCodecVersion {
		return nil, fmt.Errorf(
			"decode Google ingress: codec version %d is unsupported",
			record.CodecVersion,
		)
	}

	var envelope googleFrameEnvelope
	if err := json.Unmarshal(record.Payload, &envelope); err != nil {
		return nil, fmt.Errorf("decode Google ingress envelope: %w", err)
	}
	if len(envelope.ProtoB64) == 0 {
		return nil, fmt.Errorf("decode Google ingress envelope: proto_b64 is empty")
	}

	if len(envelope.ConversationB64) > 0 && (!history || envelope.Kind != googleFrameMessage) {
		return nil, fmt.Errorf(
			"decode Google ingress envelope: conversation_b64 is only valid on history message frames",
		)
	}

	switch envelope.Kind {
	case googleFrameMessage:
		if envelope.IsOld == nil {
			return nil, fmt.Errorf("decode Google ingress envelope: message is_old is missing")
		}
		var message gmproto.Message
		if err := proto.Unmarshal(envelope.ProtoB64, &message); err != nil {
			return nil, fmt.Errorf("decode Google message protobuf: %w", err)
		}
		events, err := d.decodeMessage(record.AccountID, &message)
		if err != nil || len(envelope.ConversationB64) == 0 {
			return events, err
		}
		var conversation gmproto.Conversation
		if err := proto.Unmarshal(envelope.ConversationB64, &conversation); err != nil {
			return nil, fmt.Errorf("decode Google message conversation protobuf: %w", err)
		}
		if conversation.GetConversationID() != message.GetConversationID() {
			return nil, fmt.Errorf(
				"decode Google ingress envelope: conversation %q does not match message conversation %q",
				conversation.GetConversationID(),
				message.GetConversationID(),
			)
		}
		// The conversation leads even when the message itself is an empty stub:
		// the snapshot is still the phone's view of a thread v2 may lack.
		return append([]bridge.Event{googleConversationEvent(&conversation)}, events...), nil
	case googleFrameConversation:
		var conversation gmproto.Conversation
		if err := proto.Unmarshal(envelope.ProtoB64, &conversation); err != nil {
			return nil, fmt.Errorf("decode Google conversation protobuf: %w", err)
		}
		return []bridge.Event{googleConversationEvent(&conversation)}, nil
	default:
		return nil, fmt.Errorf("decode Google ingress envelope: kind %q is unsupported", envelope.Kind)
	}
}

func (d *GoogleDecoder) decodeMessage(
	accountID string,
	message *gmproto.Message,
) ([]bridge.Event, error) {
	body := client.ExtractMessageBody(message)
	media := client.ExtractMediaInfo(message)
	reactions := client.ExtractReactions(message)
	replyToRemoteID := client.ExtractReplyToID(message)
	senderName, senderNumber := client.ExtractSenderInfo(message)
	fromMe := client.MessageIsFromMe(message)

	if googleMessageIsEmptyStub(message, body, media, reactions) {
		if d != nil && d.counters != nil {
			d.counters.account(accountID).emptyStubsSkipped.Add(1)
		}
		return nil, nil
	}

	sender := bridge.IdentityRef{
		Raw:    senderNumber,
		Name:   senderName,
		IsSelf: fromMe,
	}
	direction := "incoming"
	if fromMe {
		direction = "outgoing"
	}
	clientRequestID := ""
	if tmpID := message.GetTmpID(); fromMe && tmpID != "" && tmpID != message.GetMessageID() {
		clientRequestID = tmpID
	}
	occurredAt := time.UnixMilli(message.GetTimestamp() / 1000)
	attachments, err := googleAttachments(media)
	if err != nil {
		return nil, err
	}

	events := []bridge.Event{{
		Kind: bridge.EventMessage,
		Message: &bridge.MessageEvent{
			RemoteConversationID: message.GetConversationID(),
			RemoteMessageID:      message.GetMessageID(),
			ClientRequestID:      clientRequestID,
			Sender:               sender,
			Direction:            direction,
			Body:                 body,
			Attachments:          attachments,
			ReplyToRemoteID:      replyToRemoteID,
			OccurredAt:           occurredAt,
		},
	}}

	for _, reaction := range reactions {
		if len(reaction.Actors) == 0 {
			events = append(events, googleReactionEvent(
				message,
				reaction.Emoji,
				bridge.IdentityRef{},
				bridge.ReactionAdd,
			))
			continue
		}
		for _, actor := range reaction.Actors {
			events = append(events, googleReactionEvent(
				message,
				reaction.Emoji,
				bridge.IdentityRef{Raw: actor},
				bridge.ReactionAdd,
			))
		}
	}

	// Tapbacks arrive as standalone SMS/RCS bodies. Keep the MessageEvent and
	// also surface the reaction. Resolving the quoted text to a target ID is
	// store-dependent, so its target remains empty until the deferred reaction
	// read model can perform that lookup.
	if tapback, ok := db.ParseTapback(body); ok {
		action := bridge.ReactionAdd
		if tapback.Remove {
			action = bridge.ReactionRemove
		}
		tapbackEvent := googleReactionEvent(message, tapback.Emoji, sender, action)
		tapbackEvent.Reaction.TargetRemoteMessageID = ""
		events = append(events, tapbackEvent)
		if d != nil && d.counters != nil {
			d.counters.account(accountID).tapbackMessages.Add(1)
		}
	}

	return events, nil
}

func googleConversationEvent(conversation *gmproto.Conversation) bridge.Event {
	kind := "direct"
	if conversation.GetIsGroupChat() {
		kind = "group"
	}
	participants := make([]bridge.Participant, 0, len(conversation.GetParticipants()))
	for _, participant := range conversation.GetParticipants() {
		if participant == nil {
			continue
		}
		number := ""
		if id := participant.GetID(); id != nil {
			number = id.GetNumber()
		}
		if number == "" {
			number = participant.GetFormattedNumber()
		}
		participants = append(participants, bridge.Participant{
			Identity: bridge.IdentityRef{
				Raw:    number,
				Name:   participant.GetFullName(),
				IsSelf: participant.GetIsMe(),
			},
			Role:   "member",
			Active: true,
		})
	}

	return bridge.Event{
		Kind: bridge.EventConversation,
		Conversation: &bridge.ConversationEvent{
			RemoteConversationID: conversation.GetConversationID(),
			Kind:                 kind,
			Title:                conversation.GetName(),
			Participants:         participants,
		},
	}
}

func googleReactionEvent(
	message *gmproto.Message,
	emoji string,
	actor bridge.IdentityRef,
	action bridge.ReactionAction,
) bridge.Event {
	return bridge.Event{
		Kind: bridge.EventReaction,
		Reaction: &bridge.ReactionEvent{
			RemoteConversationID:  message.GetConversationID(),
			TargetRemoteMessageID: message.GetMessageID(),
			Actor:                 actor,
			Emoji:                 emoji,
			Action:                action,
			OccurredAt:            time.UnixMilli(message.GetTimestamp() / 1000),
		},
	}
}

func googleMessageIsEmptyStub(
	message *gmproto.Message,
	body string,
	media *client.MediaInfo,
	reactions []client.Reaction,
) bool {
	status := ""
	if messageStatus := message.GetMessageStatus(); messageStatus != nil {
		status = messageStatus.GetStatus().String()
	}
	mediaID := ""
	if media != nil {
		mediaID = media.MediaID
	}
	reactionMarker := ""
	if len(reactions) > 0 {
		reactionMarker = "present"
	}
	return db.IsEmptyStubMessage(&db.Message{
		Body:      body,
		MediaID:   mediaID,
		Reactions: reactionMarker,
		Status:    status,
	})
}

func googleAttachments(media *client.MediaInfo) ([]bridge.Attachment, error) {
	if media == nil {
		return nil, nil
	}
	remoteRef, err := json.Marshal(struct {
		Version       int    `json:"v"`
		MediaID       string `json:"media_id"`
		DecryptionKey string `json:"decryption_key"`
	}{
		Version:       1,
		MediaID:       media.MediaID,
		DecryptionKey: hex.EncodeToString(media.DecryptionKey),
	})
	if err != nil {
		return nil, fmt.Errorf("pack Google attachment %q: %w", media.MediaID, err)
	}
	return []bridge.Attachment{{
		RemoteID:  media.MediaID,
		RemoteRef: remoteRef,
		Filename:  media.MediaName,
		MIME:      media.MimeType,
		Size:      media.Size,
	}}, nil
}
