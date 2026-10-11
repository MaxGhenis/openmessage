package ingest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/v2keys"
)

const (
	// SignalDesktopHistoryCodec is insert-only history read from Signal
	// Desktop's archive. It is never evidence of live delivery, and the
	// freshness and silence monitors do not count it.
	SignalDesktopHistoryCodec               = "signal.desktop.history"
	SignalDesktopHistoryCodecVersion uint32 = 1
	signalDesktopHistoryMaxMessages         = 100
	signalDesktopHistoryMaxReactions        = 64
)

var (
	signalDesktopE164 = regexp.MustCompile(`^\+[0-9]{6,15}$`)
	signalDesktopACI  = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	// Signal group IDs are base64 (standard alphabet, padded) as both
	// signal-cli and Signal Desktop write them.
	signalDesktopGroupID = regexp.MustCompile(`^[A-Za-z0-9+/]+={0,2}$`)
)

// SignalDesktopHistoryParticipant is one roster entry, already in the address
// form the live bridge keys that person by.
type SignalDesktopHistoryParticipant struct {
	Address string `json:"address"`
	Name    string `json:"name,omitempty"`
	IsSelf  bool   `json:"is_self,omitempty"`
}

// SignalDesktopHistoryAttachment carries what the body placeholder needs.
type SignalDesktopHistoryAttachment struct {
	ContentType string
	FileName    string
}

// SignalDesktopHistoryReaction is one current reaction on a captured message.
type SignalDesktopHistoryReaction struct {
	ActorAddress string
	ActorName    string
	IsSelf       bool
	Emoji        string
	TimestampMS  int64
}

// SignalDesktopHistoryMessage is one captured message. AuthorAddress is the
// sender for incoming messages and empty for outgoing ones.
type SignalDesktopHistoryMessage struct {
	AuthorAddress string
	AuthorName    string
	Outgoing      bool
	SentAtMS      int64
	Body          string
	Attachments   []SignalDesktopHistoryAttachment
	Sticker       bool
	Contact       bool
	Payment       bool
	Poll          bool
	Preview       bool
	ViewOnce      bool
	StoryReply    bool
	Reactions     []SignalDesktopHistoryReaction
}

// SignalDesktopHistorySnapshot is one chat and at most 100 of its messages.
// AccountAddress is the account's own E.164 from v2, never from signal-cli.
type SignalDesktopHistorySnapshot struct {
	AccountAddress       string
	RemoteConversationID string
	Title                string
	Participants         []SignalDesktopHistoryParticipant
	Messages             []SignalDesktopHistoryMessage
}

type signalDesktopHistoryEnvelope struct {
	Account        string                            `json:"account"`
	ConversationID string                            `json:"conversation_id"`
	Kind           string                            `json:"kind"`
	Title          string                            `json:"title,omitempty"`
	Participants   []SignalDesktopHistoryParticipant `json:"participants"`
	Lines          []signalDesktopHistoryLine        `json:"lines"`
}

// signalDesktopHistoryLine is a signal-cli receive line as the live bridge
// would have captured it, with the addresses it would have resolved.
type signalDesktopHistoryLine struct {
	Line                json.RawMessage `json:"line"`
	ResolvedSource      string          `json:"resolved_source,omitempty"`
	ResolvedDestination string          `json:"resolved_destination,omitempty"`
}

type sdLine struct {
	Envelope sdEnvelope `json:"envelope"`
}

type sdEnvelope struct {
	Source      string  `json:"source"`
	SourceName  string  `json:"sourceName,omitempty"`
	Timestamp   int64   `json:"timestamp"`
	DataMessage *sdData `json:"dataMessage,omitempty"`
	SyncMessage *sdSync `json:"syncMessage,omitempty"`
}

type sdSync struct {
	SentMessage *sdData `json:"sentMessage"`
}

type sdData struct {
	Timestamp    int64             `json:"timestamp"`
	Message      string            `json:"message,omitempty"`
	Destination  string            `json:"destination,omitempty"`
	GroupInfo    *sdGroup          `json:"groupInfo,omitempty"`
	Attachments  []sdAttachment    `json:"attachments,omitempty"`
	Reaction     *sdReaction       `json:"reaction,omitempty"`
	ViewOnce     bool              `json:"viewOnce,omitempty"`
	Payment      json.RawMessage   `json:"payment,omitempty"`
	Previews     []json.RawMessage `json:"previews,omitempty"`
	Sticker      json.RawMessage   `json:"sticker,omitempty"`
	Contacts     []json.RawMessage `json:"contacts,omitempty"`
	PollCreate   json.RawMessage   `json:"pollCreate,omitempty"`
	StoryContext json.RawMessage   `json:"storyContext,omitempty"`
}

type sdGroup struct {
	GroupID string `json:"groupId"`
	Type    string `json:"type"`
}

// sdAttachment has no id: a Desktop attachment cannot be fetched through
// signal-cli, so the live decoder adds no attachment row for it, and the
// content type still picks the placeholder body.
type sdAttachment struct {
	ContentType string `json:"contentType"`
	Filename    string `json:"filename,omitempty"`
}

type sdReaction struct {
	Emoji               string `json:"emoji"`
	TargetAuthor        string `json:"targetAuthor"`
	TargetSentTimestamp int64  `json:"targetSentTimestamp"`
	IsRemove            bool   `json:"isRemove"`
}

var emptyJSONObject = json.RawMessage(`{}`)

// signalDesktopConversation validates a canonical Signal remote conversation
// ID and returns its peer address (direct) or group ID (group).
func signalDesktopConversation(remoteID string) (target string, group bool, err error) {
	remoteID = strings.TrimSpace(remoteID)
	switch {
	case strings.HasPrefix(remoteID, "signal-group:"):
		target = strings.TrimPrefix(remoteID, "signal-group:")
		if !signalDesktopGroupID.MatchString(target) {
			return "", false, fmt.Errorf("invalid Signal Desktop group ID")
		}
		return target, true, nil
	case strings.HasPrefix(remoteID, "signal:"):
		target = strings.TrimPrefix(remoteID, "signal:")
		if !signalDesktopAddress(target) {
			return "", false, fmt.Errorf("invalid Signal Desktop conversation address")
		}
		return target, false, nil
	}
	return "", false, fmt.Errorf("invalid Signal Desktop conversation ID")
}

func signalDesktopAddress(value string) bool {
	return signalDesktopE164.MatchString(value) || signalDesktopACI.MatchString(value)
}

// SignalDesktopHistoryRecord turns a snapshot into one history frame. Each
// message becomes the signal-cli receive line the live bridge would have
// captured (a data message, or a sync transcript for an outgoing one), so the
// unchanged live decoder derives its IDs and body. The payload is sorted and
// its hash is the dedupe key, so a rescan of unchanged history is a replay.
func SignalDesktopHistoryRecord(accountID string, generation bridge.Generation, snapshot SignalDesktopHistorySnapshot, receivedAt time.Time) (bridge.RawIngressRecord, error) {
	account := strings.TrimSpace(snapshot.AccountAddress)
	if !signalDesktopE164.MatchString(account) {
		return bridge.RawIngressRecord{}, fmt.Errorf("Signal Desktop history needs the account's own E.164 number")
	}
	remoteID := strings.TrimSpace(snapshot.RemoteConversationID)
	target, group, err := signalDesktopConversation(remoteID)
	if err != nil {
		return bridge.RawIngressRecord{}, err
	}
	if len(snapshot.Messages) > signalDesktopHistoryMaxMessages {
		return bridge.RawIngressRecord{}, fmt.Errorf("Signal Desktop history frame exceeds %d messages", signalDesktopHistoryMaxMessages)
	}
	envelope := signalDesktopHistoryEnvelope{
		Account:        account,
		ConversationID: remoteID,
		Kind:           "direct",
		Title:          strings.TrimSpace(snapshot.Title),
		Participants:   make([]SignalDesktopHistoryParticipant, 0, len(snapshot.Participants)),
	}
	if group {
		envelope.Kind = "group"
	}
	for _, participant := range snapshot.Participants {
		participant.Address = strings.TrimSpace(participant.Address)
		participant.Name = strings.TrimSpace(participant.Name)
		if !signalDesktopAddress(participant.Address) {
			return bridge.RawIngressRecord{}, fmt.Errorf("invalid Signal Desktop participant address")
		}
		envelope.Participants = append(envelope.Participants, participant)
	}
	sort.Slice(envelope.Participants, func(i, j int) bool {
		a, b := envelope.Participants[i], envelope.Participants[j]
		if a.Address != b.Address {
			return a.Address < b.Address
		}
		return a.Name < b.Name
	})
	messages := append([]SignalDesktopHistoryMessage(nil), snapshot.Messages...)
	sort.SliceStable(messages, func(i, j int) bool {
		a, b := messages[i], messages[j]
		if a.SentAtMS != b.SentAtMS {
			return a.SentAtMS < b.SentAtMS
		}
		if a.Outgoing != b.Outgoing {
			return !a.Outgoing
		}
		return a.AuthorAddress < b.AuthorAddress
	})
	var groupInfo *sdGroup
	destination := ""
	if group {
		groupInfo = &sdGroup{GroupID: target, Type: "DELIVER"}
	} else {
		destination = target
	}
	for _, message := range messages {
		if message.SentAtMS <= 0 {
			return bridge.RawIngressRecord{}, fmt.Errorf("Signal Desktop history message lacks a positive sent time")
		}
		author := strings.TrimSpace(message.AuthorAddress)
		if message.Outgoing {
			author = account
		} else if !signalDesktopAddress(author) {
			return bridge.RawIngressRecord{}, fmt.Errorf("Signal Desktop history message lacks a valid author")
		} else if !group && author != target {
			return bridge.RawIngressRecord{}, fmt.Errorf("Signal Desktop 1:1 message author is not the conversation's peer")
		}
		data := signalDesktopData(message, groupInfo)
		line := sdLine{Envelope: sdEnvelope{Source: author, Timestamp: message.SentAtMS}}
		resolved := signalDesktopHistoryLine{}
		if message.Outgoing {
			data.Destination = destination
			line.Envelope.SyncMessage = &sdSync{SentMessage: data}
			resolved.ResolvedDestination = destination
		} else {
			line.Envelope.SourceName = strings.TrimSpace(message.AuthorName)
			line.Envelope.DataMessage = data
			resolved.ResolvedSource = author
		}
		encoded, err := json.Marshal(line)
		if err != nil {
			return bridge.RawIngressRecord{}, fmt.Errorf("encode Signal Desktop history line: %w", err)
		}
		resolved.Line = encoded
		envelope.Lines = append(envelope.Lines, resolved)

		reactions := append([]SignalDesktopHistoryReaction(nil), message.Reactions...)
		if len(reactions) > signalDesktopHistoryMaxReactions {
			return bridge.RawIngressRecord{}, fmt.Errorf("Signal Desktop history message has too many reactions")
		}
		sort.SliceStable(reactions, func(i, j int) bool {
			if reactions[i].IsSelf != reactions[j].IsSelf {
				return reactions[i].IsSelf
			}
			return reactions[i].ActorAddress < reactions[j].ActorAddress
		})
		for _, reaction := range reactions {
			reactionLine, err := signalDesktopReactionLine(account, author, destination, groupInfo, message.SentAtMS, reaction)
			if err != nil {
				return bridge.RawIngressRecord{}, err
			}
			envelope.Lines = append(envelope.Lines, reactionLine)
		}
	}
	payload, err := json.Marshal(envelope)
	if err != nil {
		return bridge.RawIngressRecord{}, fmt.Errorf("encode Signal Desktop history: %w", err)
	}
	digest := sha256.Sum256(payload)
	return bridge.RawIngressRecord{
		AccountID:    accountID,
		Generation:   generation,
		DedupeKey:    "sigdesk:" + hex.EncodeToString(digest[:]),
		Codec:        SignalDesktopHistoryCodec,
		CodecVersion: SignalDesktopHistoryCodecVersion,
		ReceivedAt:   receivedAt,
		Payload:      payload,
	}, nil
}

func signalDesktopData(message SignalDesktopHistoryMessage, groupInfo *sdGroup) *sdData {
	data := &sdData{
		Timestamp: message.SentAtMS,
		Message:   message.Body,
		GroupInfo: groupInfo,
		ViewOnce:  message.ViewOnce,
	}
	for _, attachment := range message.Attachments {
		data.Attachments = append(data.Attachments, sdAttachment{
			ContentType: strings.TrimSpace(attachment.ContentType),
			Filename:    strings.TrimSpace(attachment.FileName),
		})
	}
	if message.Sticker {
		data.Sticker = emptyJSONObject
	}
	if message.Contact {
		data.Contacts = []json.RawMessage{emptyJSONObject}
	}
	if message.Payment {
		data.Payment = emptyJSONObject
	}
	if message.Poll {
		data.PollCreate = emptyJSONObject
	}
	if message.Preview {
		data.Previews = []json.RawMessage{emptyJSONObject}
	}
	if message.StoryReply {
		data.StoryContext = emptyJSONObject
	}
	return data
}

// signalDesktopReactionLine is the live line for a reaction someone holds on
// a captured message: a data message from the reactor, or a sync transcript
// for the account's own reaction.
func signalDesktopReactionLine(account, targetAuthor, destination string, groupInfo *sdGroup, targetTimestamp int64, reaction SignalDesktopHistoryReaction) (signalDesktopHistoryLine, error) {
	emoji := strings.TrimSpace(reaction.Emoji)
	if emoji == "" {
		return signalDesktopHistoryLine{}, fmt.Errorf("Signal Desktop history reaction has no emoji")
	}
	at := reaction.TimestampMS
	if at <= 0 {
		at = targetTimestamp
	}
	data := &sdData{
		Timestamp: at,
		GroupInfo: groupInfo,
		Reaction:  &sdReaction{Emoji: emoji, TargetAuthor: targetAuthor, TargetSentTimestamp: targetTimestamp},
	}
	line := sdLine{Envelope: sdEnvelope{Timestamp: at}}
	resolved := signalDesktopHistoryLine{}
	if reaction.IsSelf {
		line.Envelope.Source = account
		data.Destination = destination
		line.Envelope.SyncMessage = &sdSync{SentMessage: data}
		resolved.ResolvedDestination = destination
	} else {
		actor := strings.TrimSpace(reaction.ActorAddress)
		if !signalDesktopAddress(actor) {
			return signalDesktopHistoryLine{}, fmt.Errorf("Signal Desktop history reaction lacks a valid actor")
		}
		if groupInfo == nil && actor != destination {
			return signalDesktopHistoryLine{}, fmt.Errorf("Signal Desktop 1:1 reaction actor is not the conversation's peer")
		}
		line.Envelope.Source = actor
		line.Envelope.SourceName = strings.TrimSpace(reaction.ActorName)
		line.Envelope.DataMessage = data
		resolved.ResolvedSource = actor
	}
	encoded, err := json.Marshal(line)
	if err != nil {
		return signalDesktopHistoryLine{}, fmt.Errorf("encode Signal Desktop history reaction: %w", err)
	}
	resolved.Line = encoded
	return resolved, nil
}

// SignalDesktopHistoryDecoder decodes signal.desktop.history v1 frames. It is
// store-free: every line goes through the live SignalDecoder, and the frame's
// conversation snapshot leads, so the worker's history rules decide placement.
type SignalDesktopHistoryDecoder struct{}

var _ bridge.Decoder = (*SignalDesktopHistoryDecoder)(nil)

func NewSignalDesktopHistoryDecoder() *SignalDesktopHistoryDecoder {
	return &SignalDesktopHistoryDecoder{}
}

// NewSignalDesktopHistoryDecoderRegistration registers the codec as history
// for the Signal live account.
func NewSignalDesktopHistoryDecoderRegistration() DecoderRegistration {
	return DecoderRegistration{
		Codec:    SignalDesktopHistoryCodec,
		Platform: bridge.PlatformSignal,
		Decoder:  NewSignalDesktopHistoryDecoder(),
		History:  true,
	}
}

func (*SignalDesktopHistoryDecoder) Decode(ctx context.Context, record bridge.RawIngressRecord) ([]bridge.Event, error) {
	if ctx == nil {
		return nil, fmt.Errorf("decode Signal Desktop history: context is nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if record.Codec != SignalDesktopHistoryCodec || record.CodecVersion != SignalDesktopHistoryCodecVersion {
		return nil, fmt.Errorf("decode Signal Desktop history: unsupported codec or version")
	}
	var envelope signalDesktopHistoryEnvelope
	if err := json.Unmarshal(record.Payload, &envelope); err != nil {
		return nil, fmt.Errorf("decode Signal Desktop history: %w", err)
	}
	if !signalDesktopE164.MatchString(envelope.Account) {
		return nil, fmt.Errorf("decode Signal Desktop history: account is not an E.164 number")
	}
	target, group, err := signalDesktopConversation(envelope.ConversationID)
	if err != nil {
		return nil, err
	}
	kind := "direct"
	if group {
		kind = "group"
	}
	if envelope.Kind != kind {
		return nil, fmt.Errorf("decode Signal Desktop history: kind does not match the conversation ID")
	}
	conversationID := v2keys.NormalizeRemoteConversationID("signal", envelope.ConversationID)
	conversation := bridge.ConversationEvent{RemoteConversationID: conversationID, Kind: kind, Title: envelope.Title}
	for _, participant := range envelope.Participants {
		if !signalDesktopAddress(participant.Address) {
			return nil, fmt.Errorf("decode Signal Desktop history: invalid participant address")
		}
		conversation.Participants = append(conversation.Participants, bridge.Participant{
			Identity: bridge.IdentityRef{Raw: participant.Address, Name: participant.Name, IsSelf: participant.IsSelf},
			Role:     "member",
			Active:   true,
		})
	}
	if !group && len(conversation.Participants) == 0 {
		conversation.Participants = []bridge.Participant{{
			Identity: bridge.IdentityRef{Raw: target, IsSelf: target == envelope.Account},
			Role:     "member",
			Active:   true,
		}}
	}
	events := []bridge.Event{{Kind: bridge.EventConversation, Conversation: &conversation}}
	live := NewSignalDecoder()
	messages := 0
	for _, line := range envelope.Lines {
		payload, err := marshalSignalJSONRPCFrame(envelope.Account, line.Line, line.ResolvedSource, line.ResolvedDestination)
		if err != nil {
			return nil, err
		}
		decoded, err := live.Decode(ctx, bridge.RawIngressRecord{
			AccountID:    record.AccountID,
			Generation:   record.Generation,
			Codec:        SignalJSONRPCCodec,
			CodecVersion: SignalJSONRPCCodecVersion,
			ReceivedAt:   record.ReceivedAt,
			Payload:      payload,
		})
		if err != nil {
			return nil, err
		}
		for _, event := range decoded {
			var eventConversation string
			switch event.Kind {
			case bridge.EventMessage:
				messages++
				eventConversation = event.Message.RemoteConversationID
			case bridge.EventReaction:
				eventConversation = event.Reaction.RemoteConversationID
			default:
				return nil, fmt.Errorf("decode Signal Desktop history: unexpected %s event", event.Kind)
			}
			if v2keys.NormalizeRemoteConversationID("signal", eventConversation) != conversationID {
				return nil, fmt.Errorf("decode Signal Desktop history: a line does not belong to the frame's conversation")
			}
			events = append(events, event)
		}
	}
	if messages > signalDesktopHistoryMaxMessages {
		return nil, fmt.Errorf("Signal Desktop history frame exceeds %d messages", signalDesktopHistoryMaxMessages)
	}
	return events, nil
}
