package ingest

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"go.mau.fi/mautrix-gmessages/pkg/libgm"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
	"google.golang.org/protobuf/proto"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/client"
	"github.com/maxghenis/openmessage/internal/db"
)

const (
	// GoogleCodec is the durable Google Messages protobuf envelope codec.
	GoogleCodec = "google.protobuf"
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
type googleFrameEnvelope struct {
	Kind     string `json:"kind"`
	ProtoB64 []byte `json:"proto_b64"`
	IsOld    *bool  `json:"is_old,omitempty"`
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
	protoBytes, err = proto.Marshal(event.Message)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal Google message protobuf: %w", err)
	}
	isOld := event.IsOld
	payload, err = json.Marshal(googleFrameEnvelope{
		Kind:     googleFrameMessage,
		ProtoB64: protoBytes,
		IsOld:    &isOld,
	})
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
	if record.Codec != GoogleCodec {
		return nil, fmt.Errorf("decode Google ingress: codec %q is not %q", record.Codec, GoogleCodec)
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

	switch envelope.Kind {
	case googleFrameMessage:
		if envelope.IsOld == nil {
			return nil, fmt.Errorf("decode Google ingress envelope: message is_old is missing")
		}
		var message gmproto.Message
		if err := proto.Unmarshal(envelope.ProtoB64, &message); err != nil {
			return nil, fmt.Errorf("decode Google message protobuf: %w", err)
		}
		return d.decodeMessage(record.AccountID, &message)
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
