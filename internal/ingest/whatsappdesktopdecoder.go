package ingest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/whatsappmedia"
	waE2E "go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

const (
	// WhatsAppDesktopHistoryCodec is insert-only desktop history, never evidence
	// of live delivery. It is deliberately separate from WhatsAppCodec.
	WhatsAppDesktopHistoryCodec               = "whatsapp.desktop.history"
	WhatsAppDesktopHistoryCodecVersion uint32 = 1
)

// WhatsAppDesktopHistoryParticipant has already been canonicalized at capture.
type WhatsAppDesktopHistoryParticipant struct {
	JID    string
	Name   string
	IsSelf bool
}

// WhatsAppDesktopHistoryMessage is capture data from a supported Core Data row.
// LocalMediaRef is present only after the reader checks the local file exists.
// Outgoing rows may have an empty SenderJID: the worker stores a null sender for
// outgoing messages and uses direction/IsSelf, just as on the live path.
type WhatsAppDesktopHistoryMessage struct {
	StanzaID      string
	SenderJID     string
	TimestampMS   int64
	IsFromMe      bool
	Type          int
	Text          string
	Caption       string
	FileName      string
	MIMEType      string
	LocalMediaRef string
	VoiceNote     bool
	Size          int64
}

// WhatsAppDesktopHistorySnapshot captures one chat and at most 100 messages.
// Every chunk includes the same complete roster; decoding never consults a
// live client, identity store, LID map or desktop database.
type WhatsAppDesktopHistorySnapshot struct {
	ChatJID      string
	Title        string
	Participants []WhatsAppDesktopHistoryParticipant
	Messages     []WhatsAppDesktopHistoryMessage
}

type whatsAppDesktopHistoryMessageEnvelope struct {
	whatsAppFrameEnvelope
	LocalMediaRef string `json:"local_media_ref,omitempty"`
	FileName      string `json:"file_name,omitempty"`
	MIMEType      string `json:"mime_type,omitempty"`
	Size          int64  `json:"size,omitempty"`
}

type whatsAppDesktopHistoryEnvelope struct {
	ChatJID      string                                  `json:"chat_jid"`
	Title        string                                  `json:"title"`
	Participants []WhatsAppDesktopHistoryParticipant     `json:"participants"`
	Messages     []whatsAppDesktopHistoryMessageEnvelope `json:"messages"`
}

// WhatsAppDesktopHistoryRecord preserves the live capture-time message shape
// and synthesizes only the protobuf fields needed by the existing body decoder.
// Its payload hash ignores receipt time and supervisor generation, so rescans
// dedupe even after daemon restarts.
func WhatsAppDesktopHistoryRecord(accountID string, generation bridge.Generation, snapshot WhatsAppDesktopHistorySnapshot, receivedAt time.Time) (bridge.RawIngressRecord, error) {
	chatJID, group, err := desktopHistoryChatJID(snapshot.ChatJID)
	if err != nil {
		return bridge.RawIngressRecord{}, err
	}
	if len(snapshot.Messages) > 100 {
		return bridge.RawIngressRecord{}, fmt.Errorf("WhatsApp desktop history frame exceeds 100 messages")
	}
	envelope := whatsAppDesktopHistoryEnvelope{
		ChatJID:      chatJID,
		Title:        snapshot.Title,
		Participants: append([]WhatsAppDesktopHistoryParticipant(nil), snapshot.Participants...),
		Messages:     make([]whatsAppDesktopHistoryMessageEnvelope, 0, len(snapshot.Messages)),
	}
	sort.Slice(envelope.Participants, func(i, j int) bool {
		a, b := envelope.Participants[i], envelope.Participants[j]
		if a.JID != b.JID {
			return a.JID < b.JID
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return !a.IsSelf && b.IsSelf
	})
	for _, captured := range snapshot.Messages {
		if strings.TrimSpace(captured.StanzaID) == "" || captured.TimestampMS <= 0 {
			return bridge.RawIngressRecord{}, fmt.Errorf("WhatsApp desktop history message lacks stanza ID or positive timestamp")
		}
		message, err := desktopHistoryMessageProto(captured)
		if err != nil {
			return bridge.RawIngressRecord{}, err
		}
		protoBytes, err := proto.Marshal(message)
		if err != nil {
			return bridge.RawIngressRecord{}, fmt.Errorf("encode WhatsApp desktop message: %w", err)
		}
		sender := captured.SenderJID
		if !captured.IsFromMe && !group {
			sender = chatJID
		}
		envelope.Messages = append(envelope.Messages, whatsAppDesktopHistoryMessageEnvelope{
			whatsAppFrameEnvelope: whatsAppFrameEnvelope{Kind: whatsAppFrameMessage, ChatJID: chatJID, SenderJID: sender, MessageID: captured.StanzaID, TimestampMS: captured.TimestampMS, IsFromMe: captured.IsFromMe, IsGroup: group, ProtoB64: protoBytes},
			LocalMediaRef:         captured.LocalMediaRef, FileName: captured.FileName, MIMEType: captured.MIMEType, Size: captured.Size,
		})
	}
	sort.Slice(envelope.Messages, func(i, j int) bool {
		if envelope.Messages[i].TimestampMS != envelope.Messages[j].TimestampMS {
			return envelope.Messages[i].TimestampMS < envelope.Messages[j].TimestampMS
		}
		return envelope.Messages[i].MessageID < envelope.Messages[j].MessageID
	})
	payload, err := json.Marshal(envelope)
	if err != nil {
		return bridge.RawIngressRecord{}, fmt.Errorf("encode WhatsApp desktop history: %w", err)
	}
	digest := sha256.Sum256(payload)
	return bridge.RawIngressRecord{AccountID: accountID, Generation: generation, DedupeKey: "wadesk:" + hex.EncodeToString(digest[:]), Codec: WhatsAppDesktopHistoryCodec, CodecVersion: WhatsAppDesktopHistoryCodecVersion, ReceivedAt: receivedAt, Payload: payload}, nil
}

func desktopHistoryMessageProto(message WhatsAppDesktopHistoryMessage) (*waE2E.Message, error) {
	switch message.Type {
	case 0, 7:
		return &waE2E.Message{Conversation: proto.String(message.Text)}, nil
	case 1:
		return &waE2E.Message{ImageMessage: &waE2E.ImageMessage{Caption: proto.String(message.Caption)}}, nil
	case 2, 11:
		return &waE2E.Message{VideoMessage: &waE2E.VideoMessage{Caption: proto.String(message.Caption), GifPlayback: proto.Bool(message.Type == 11)}}, nil
	case 3:
		return &waE2E.Message{AudioMessage: &waE2E.AudioMessage{PTT: proto.Bool(message.VoiceNote)}}, nil
	case 8:
		return &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{Caption: proto.String(message.Caption), FileName: proto.String(message.FileName)}}, nil
	case 15:
		return &waE2E.Message{StickerMessage: &waE2E.StickerMessage{}}, nil
	default:
		return nil, fmt.Errorf("unsupported WhatsApp desktop message type %d", message.Type)
	}
}

// desktopHistoryChatJID enforces I3 at the durable decoding boundary as well
// as at capture: a desktop frame can never mint a LID, status or broadcast chat.
func desktopHistoryChatJID(raw string) (string, bool, error) {
	jid, err := types.ParseJID(strings.TrimSpace(raw))
	if err != nil {
		return "", false, fmt.Errorf("invalid WhatsApp desktop chat JID")
	}
	jid = jid.ToNonAD()
	if jid.User == "0" || jid.IsBot() {
		return "", false, fmt.Errorf("unsupported WhatsApp desktop chat JID")
	}
	switch jid.Server {
	case types.DefaultUserServer:
		if allASCIIDigits(jid.User) {
			return jid.String(), false, nil
		}
	case types.GroupServer:
		if jid.User != "" {
			return jid.String(), true, nil
		}
	}
	return "", false, fmt.Errorf("unsupported WhatsApp desktop chat JID")
}

// WhatsAppDesktopHistoryDecoder is a store-free v1 desktop snapshot decoder.
type WhatsAppDesktopHistoryDecoder struct{}

var _ bridge.Decoder = (*WhatsAppDesktopHistoryDecoder)(nil)

func NewWhatsAppDesktopHistoryDecoder() *WhatsAppDesktopHistoryDecoder {
	return &WhatsAppDesktopHistoryDecoder{}
}

func NewWhatsAppDesktopHistoryDecoderRegistration() DecoderRegistration {
	return DecoderRegistration{Codec: WhatsAppDesktopHistoryCodec, Platform: bridge.PlatformWhatsApp, Decoder: NewWhatsAppDesktopHistoryDecoder(), History: true}
}

func (*WhatsAppDesktopHistoryDecoder) Decode(ctx context.Context, record bridge.RawIngressRecord) ([]bridge.Event, error) {
	if ctx == nil {
		return nil, fmt.Errorf("decode WhatsApp desktop history: context is nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if record.Codec != WhatsAppDesktopHistoryCodec || record.CodecVersion != WhatsAppDesktopHistoryCodecVersion {
		return nil, fmt.Errorf("decode WhatsApp desktop history: unsupported codec or version")
	}
	var envelope whatsAppDesktopHistoryEnvelope
	if err := json.Unmarshal(record.Payload, &envelope); err != nil {
		return nil, fmt.Errorf("decode WhatsApp desktop history: %w", err)
	}
	chatJID, group, err := desktopHistoryChatJID(envelope.ChatJID)
	if err != nil {
		return nil, err
	}
	if len(envelope.Messages) > 100 {
		return nil, fmt.Errorf("WhatsApp desktop history frame exceeds 100 messages")
	}
	kind := "direct"
	if group {
		kind = "group"
	}
	conversation := bridge.ConversationEvent{RemoteConversationID: whatsAppRemoteConversationID(chatJID), Kind: kind, Title: envelope.Title}
	for _, participant := range envelope.Participants {
		if strings.TrimSpace(participant.JID) == "" {
			continue
		}
		conversation.Participants = append(conversation.Participants, bridge.Participant{Identity: whatsAppIdentity(participant.JID, participant.Name, participant.IsSelf), Role: "member", Active: true})
	}
	if !group && len(conversation.Participants) == 0 {
		conversation.Participants = []bridge.Participant{{Identity: whatsAppIdentity(chatJID, "", false), Role: "member", Active: true}}
	}
	events := []bridge.Event{{Kind: bridge.EventConversation, Conversation: &conversation}}
	live := NewWhatsAppDecoder()
	for _, message := range envelope.Messages {
		if message.ChatJID != chatJID || message.IsGroup != group || message.Kind != whatsAppFrameMessage {
			return nil, fmt.Errorf("WhatsApp desktop history message does not match its chat snapshot")
		}
		decoded, err := live.decodeMessage(record.AccountID, message.whatsAppFrameEnvelope)
		if err != nil {
			return nil, err
		}
		for _, event := range decoded {
			if event.Kind != bridge.EventMessage {
				return nil, fmt.Errorf("WhatsApp desktop history contains a non-message event")
			}
			// The live decoder derives a remote attachment from any media
			// protobuf, but a desktop row carries no direct path or media key.
			// Only a local file the reader verified becomes an attachment;
			// otherwise the placeholder body stands alone.
			event.Message.Attachments = nil
			if message.LocalMediaRef != "" {
				path, err := whatsappmedia.DecodeLocalMediaRef(message.LocalMediaRef)
				if err != nil || strings.TrimSpace(path) == "" {
					return nil, fmt.Errorf("invalid WhatsApp desktop local media reference")
				}
				if _, err := whatsappmedia.ResolveLocalMediaPath("desktop-history-validation-root", message.LocalMediaRef); err != nil {
					return nil, err
				}
				opaque, err := json.Marshal(struct {
					V        int    `json:"v"`
					Kind     string `json:"kind"`
					LocalRef string `json:"local_ref"`
				}{1, "local", message.LocalMediaRef})
				if err != nil {
					return nil, err
				}
				event.Message.Attachments = []bridge.Attachment{{RemoteID: message.LocalMediaRef, RemoteRef: opaque, Filename: message.FileName, MIME: message.MIMEType, Size: message.Size}}
			}
			events = append(events, event)
		}
	}
	return events, nil
}
