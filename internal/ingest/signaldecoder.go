package ingest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/signallive"
	"github.com/maxghenis/openmessage/internal/v2keys"
)

const (
	SignalJSONRPCCodec        = "signal.jsonrpc"
	SignalJSONRPCCodecVersion = uint32(1)
	signalTypingLifetime      = 10 * time.Second
)

type signalJSONRPCFrame struct {
	Account             string          `json:"account"`
	Line                json.RawMessage `json:"line"`
	ResolvedSource      string          `json:"resolved_source"`
	ResolvedDestination string          `json:"resolved_destination"`
}

type signalDownloadOpaqueV1 struct {
	Version        int    `json:"v"`
	Kind           string `json:"kind"`
	AttachmentID   string `json:"att_id"`
	ConversationID string `json:"conversation_id"`
}

// SignalDecoder projects the byte-preserving signal.jsonrpc v1 codec into the
// normalized bridge event contract.
type SignalDecoder struct{}

var _ bridge.Decoder = (*SignalDecoder)(nil)

// NewSignalDecoder constructs the Signal durable-ingress decoder.
func NewSignalDecoder() *SignalDecoder {
	return &SignalDecoder{}
}

// BuildSignalIngress classifies a captured Signal line before durable append.
// Typing-only envelopes become ephemeral events; every other parsed envelope
// becomes one byte-preserving signal.jsonrpc record.
func BuildSignalIngress(
	accountID string,
	generation bridge.Generation,
	signalAccount string,
	line []byte,
	resolvedSource string,
	resolvedDestination string,
	receivedAt time.Time,
) (*bridge.RawIngressRecord, *bridge.EphemeralEvent, error) {
	resolvedAccount, env, err := signallive.ParseReceiveEnvelope(signalAccount, line)
	if err != nil {
		return nil, nil, fmt.Errorf("classify Signal ingress: %w", err)
	}
	resolvedAccount = strings.TrimSpace(resolvedAccount)
	if resolvedAccount == "" {
		return nil, nil, fmt.Errorf("classify Signal ingress: account is empty")
	}
	if signalTypingOnly(&env) {
		source := resolvedSignalAddress(resolvedSource, signallive.EnvelopeSource(&env))
		groupID := ""
		if env.TypingMessage.GroupInfo != nil {
			groupID = strings.TrimSpace(env.TypingMessage.GroupInfo.GroupID)
		}
		return nil, &bridge.EphemeralEvent{
			AccountID:  accountID,
			Generation: generation,
			Typing: &bridge.TypingEvent{
				RemoteConversationID: signallive.SignalConversationID(source, groupID),
				Actor: bridge.IdentityRef{
					Raw:  source,
					Name: strings.TrimSpace(env.SourceName),
				},
				Typing:    strings.EqualFold(strings.TrimSpace(env.TypingMessage.Action), "started"),
				ExpiresAt: receivedAt.Add(signalTypingLifetime),
			},
		}, nil
	}

	payload, err := marshalSignalJSONRPCFrame(
		resolvedAccount,
		line,
		resolvedSource,
		resolvedDestination,
	)
	if err != nil {
		return nil, nil, err
	}
	return &bridge.RawIngressRecord{
		AccountID:    accountID,
		Generation:   generation,
		DedupeKey:    signalDedupeKey(line),
		Codec:        SignalJSONRPCCodec,
		CodecVersion: SignalJSONRPCCodecVersion,
		ReceivedAt:   receivedAt,
		Payload:      payload,
	}, nil, nil
}

func signalTypingOnly(env *signallive.Envelope) bool {
	return env != nil && env.TypingMessage != nil && env.DataMessage == nil &&
		env.EditMessage == nil &&
		(env.SyncMessage == nil || env.SyncMessage.SentMessage == nil)
}

func marshalSignalJSONRPCFrame(
	account string,
	line []byte,
	resolvedSource string,
	resolvedDestination string,
) ([]byte, error) {
	if !json.Valid(line) {
		return nil, fmt.Errorf("encode Signal ingress: line is not valid JSON")
	}
	encodedAccount, err := json.Marshal(account)
	if err != nil {
		return nil, fmt.Errorf("encode Signal ingress account: %w", err)
	}
	encodedSource, err := json.Marshal(strings.TrimSpace(resolvedSource))
	if err != nil {
		return nil, fmt.Errorf("encode Signal ingress resolved source: %w", err)
	}
	encodedDestination, err := json.Marshal(strings.TrimSpace(resolvedDestination))
	if err != nil {
		return nil, fmt.Errorf("encode Signal ingress resolved destination: %w", err)
	}
	payload := make([]byte, 0, len(encodedAccount)+len(line)+len(encodedSource)+len(encodedDestination)+64)
	payload = append(payload, `{"account":`...)
	payload = append(payload, encodedAccount...)
	payload = append(payload, `,"line":`...)
	payload = append(payload, line...)
	payload = append(payload, `,"resolved_source":`...)
	payload = append(payload, encodedSource...)
	payload = append(payload, `,"resolved_destination":`...)
	payload = append(payload, encodedDestination...)
	payload = append(payload, '}')
	return payload, nil
}

func signalDedupeKey(line []byte) string {
	var probe struct {
		Envelope signallive.Envelope `json:"envelope"`
		Result   *struct {
			Envelope signallive.Envelope `json:"envelope"`
		} `json:"result"`
	}
	if err := json.Unmarshal(line, &probe); err == nil {
		envelope := probe.Envelope
		if envelope.Timestamp == 0 && strings.TrimSpace(signallive.EnvelopeSource(&envelope)) == "" && probe.Result != nil {
			envelope = probe.Result.Envelope
		}
		if source := strings.TrimSpace(signallive.EnvelopeSource(&envelope)); source != "" && envelope.Timestamp != 0 {
			// Dedupe deliberately uses the raw envelope address. Contact resolution
			// can drift between live delivery and WAL recovery; the raw key absorbs
			// an exact replay before that drift can mint a divergent projection.
			return "env:" + source + ":" + strconv.FormatInt(envelope.Timestamp, 10)
		}
	}
	sum := sha256.Sum256(line)
	return "line:" + hex.EncodeToString(sum[:8])
}

func resolvedSignalAddress(resolved string, raw string) string {
	if resolved = strings.TrimSpace(resolved); resolved != "" {
		return resolved
	}
	return strings.TrimSpace(raw)
}

func (d *SignalDecoder) Decode(
	_ context.Context,
	record bridge.RawIngressRecord,
) ([]bridge.Event, error) {
	if record.Codec != SignalJSONRPCCodec {
		return nil, fmt.Errorf("decode Signal ingress: codec %q is not %q", record.Codec, SignalJSONRPCCodec)
	}
	if record.CodecVersion != SignalJSONRPCCodecVersion {
		return nil, fmt.Errorf("decode Signal ingress: unsupported codec version %d", record.CodecVersion)
	}
	var frame signalJSONRPCFrame
	if err := json.Unmarshal(record.Payload, &frame); err != nil {
		return nil, fmt.Errorf("decode Signal ingress frame: %w", err)
	}
	frame.Account = strings.TrimSpace(frame.Account)
	if frame.Account == "" {
		return nil, fmt.Errorf("decode Signal ingress frame: account is empty")
	}
	if len(frame.Line) == 0 {
		return nil, fmt.Errorf("decode Signal ingress frame: line is missing")
	}
	account, env, err := signallive.ParseReceiveEnvelope(frame.Account, frame.Line)
	if err != nil {
		return nil, fmt.Errorf("decode Signal receive line: %w", err)
	}
	account = strings.TrimSpace(account)
	if account == "" {
		account = frame.Account
	}
	source := resolvedSignalAddress(frame.ResolvedSource, signallive.EnvelopeSource(&env))
	destination := ""
	if env.SyncMessage != nil {
		destination = resolvedSignalAddress(
			frame.ResolvedDestination,
			signallive.SentTarget(env.SyncMessage.SentMessage),
		)
	}

	events := make([]bridge.Event, 0, 3)
	if env.EditMessage != nil {
		event, err := decodeSignalEdit(account, &env, source)
		if err != nil {
			return nil, err
		}
		if event != nil {
			events = append(events, *event)
		}
	}
	if env.DataMessage != nil {
		event, err := decodeSignalData(account, &env, source)
		if err != nil {
			return nil, err
		}
		if event != nil {
			events = append(events, *event)
		}
	}
	if env.SyncMessage != nil && env.SyncMessage.SentMessage != nil {
		event, err := decodeSignalSent(account, &env, destination)
		if err != nil {
			return nil, err
		}
		if event != nil {
			events = append(events, *event)
		}
	}
	return events, nil
}

func decodeSignalData(
	account string,
	env *signallive.Envelope,
	source string,
) (*bridge.Event, error) {
	message := env.DataMessage
	source = strings.TrimSpace(source)
	if source == "" {
		return nil, nil
	}
	groupID := ""
	if message.GroupInfo != nil {
		groupID = strings.TrimSpace(message.GroupInfo.GroupID)
	}
	conversationID := signallive.SignalConversationID(source, groupID)
	if message.Reaction != nil {
		occurredAt := env.Timestamp
		if occurredAt == 0 {
			occurredAt = message.Timestamp
		}
		return decodeSignalReaction(account, conversationID, source, strings.TrimSpace(env.SourceName), message.Reaction, occurredAt, false), nil
	}
	if signallive.AddressesMatch(source, account) {
		return nil, nil
	}
	timestamp := message.Timestamp
	if timestamp == 0 {
		timestamp = env.Timestamp
	}
	if timestamp <= 0 {
		return nil, fmt.Errorf("decode Signal data message: timestamp is not positive")
	}
	attachments, err := signalAttachments(conversationID, message.Attachments)
	if err != nil {
		return nil, err
	}
	return &bridge.Event{
		Kind: bridge.EventMessage,
		Message: &bridge.MessageEvent{
			RemoteConversationID: conversationID,
			RemoteMessageID:      v2keys.SignalIncomingSourceID(conversationID, source, timestamp),
			Sender: bridge.IdentityRef{
				Raw:  source,
				Name: strings.TrimSpace(env.SourceName),
			},
			Direction:   "incoming",
			Body:        message.DisplayBody(),
			Attachments: attachments,
			OccurredAt:  time.UnixMilli(timestamp),
		},
	}, nil
}

func decodeSignalEdit(
	account string,
	env *signallive.Envelope,
	source string,
) (*bridge.Event, error) {
	edit := env.EditMessage
	if edit.DataMessage == nil {
		return nil, nil
	}
	source = strings.TrimSpace(source)
	if source == "" || signallive.AddressesMatch(source, account) {
		return nil, nil
	}
	groupID := ""
	if edit.DataMessage.GroupInfo != nil {
		groupID = strings.TrimSpace(edit.DataMessage.GroupInfo.GroupID)
	}
	conversationID := signallive.SignalConversationID(source, groupID)
	if edit.TargetSentTimestamp <= 0 {
		return nil, fmt.Errorf("decode Signal edit message: target timestamp is not positive")
	}
	occurredAt := edit.DataMessage.Timestamp
	if occurredAt == 0 {
		occurredAt = env.Timestamp
	}
	return &bridge.Event{
		Kind: bridge.EventMessageMutation,
		MessageMutation: &bridge.MessageMutationEvent{
			RemoteConversationID: conversationID,
			RemoteMessageID: v2keys.SignalIncomingSourceID(
				conversationID,
				source,
				edit.TargetSentTimestamp,
			),
			Kind:       "edit",
			Body:       edit.DataMessage.DisplayBody(),
			OccurredAt: time.UnixMilli(occurredAt),
		},
	}, nil
}

func decodeSignalSent(
	account string,
	env *signallive.Envelope,
	target string,
) (*bridge.Event, error) {
	sent := env.SyncMessage.SentMessage
	// Sent-message edits are retained by the legacy path but have no specified
	// v2 remote-id mapping in this slice; keep the durable frame and emit no
	// projection until that mapping is defined.
	if sent.EditMessage != nil {
		return nil, nil
	}
	groupID := ""
	if sent.GroupInfo != nil {
		groupID = strings.TrimSpace(sent.GroupInfo.GroupID)
	}
	target = strings.TrimSpace(target)
	if target == "" && groupID == "" {
		return nil, nil
	}
	conversationID := signallive.SignalConversationID(target, groupID)
	if sent.Reaction != nil {
		occurredAt := env.Timestamp
		if occurredAt == 0 {
			occurredAt = sent.Timestamp
		}
		return decodeSignalReaction(account, conversationID, account, "", sent.Reaction, occurredAt, true), nil
	}
	timestamp := sent.Timestamp
	if timestamp == 0 {
		timestamp = env.Timestamp
	}
	if timestamp <= 0 {
		return nil, fmt.Errorf("decode Signal sent message: timestamp is not positive")
	}
	attachments, err := signalAttachments(conversationID, sent.Attachments)
	if err != nil {
		return nil, err
	}
	return &bridge.Event{
		Kind: bridge.EventMessage,
		Message: &bridge.MessageEvent{
			RemoteConversationID: conversationID,
			RemoteMessageID:      strconv.FormatInt(timestamp, 10),
			Sender:               bridge.IdentityRef{Raw: account, IsSelf: true},
			Direction:            "outgoing",
			Body:                 sent.DisplayBody(),
			Attachments:          attachments,
			OccurredAt:           time.UnixMilli(timestamp),
		},
	}, nil
}

func decodeSignalReaction(
	account string,
	conversationID string,
	actor string,
	actorName string,
	reaction *signallive.Reaction,
	timestamp int64,
	actorIsSelf bool,
) *bridge.Event {
	targetTimestamp := signallive.ReactionTargetTimestamp(reaction)
	if targetTimestamp <= 0 {
		return nil
	}
	targetAuthor := strings.TrimSpace(signallive.ReactionTargetAuthor(reaction, account))
	targetRemoteID := v2keys.SignalIncomingSourceID(conversationID, targetAuthor, targetTimestamp)
	if signallive.AddressesMatch(targetAuthor, account) {
		targetRemoteID = strconv.FormatInt(targetTimestamp, 10)
	}
	action := bridge.ReactionAdd
	if reaction.IsRemove {
		action = bridge.ReactionRemove
	}
	return &bridge.Event{
		Kind: bridge.EventReaction,
		Reaction: &bridge.ReactionEvent{
			RemoteConversationID:  conversationID,
			TargetRemoteMessageID: targetRemoteID,
			Actor: bridge.IdentityRef{
				Raw:    strings.TrimSpace(actor),
				Name:   strings.TrimSpace(actorName),
				IsSelf: actorIsSelf,
			},
			Emoji:      strings.TrimSpace(reaction.Emoji),
			Action:     action,
			OccurredAt: time.UnixMilli(timestamp),
		},
	}
}

func signalAttachments(
	conversationID string,
	attachments []signallive.Attachment,
) ([]bridge.Attachment, error) {
	result := make([]bridge.Attachment, 0, len(attachments))
	for _, attachment := range attachments {
		attachmentID := strings.TrimSpace(attachment.ID)
		if attachmentID == "" {
			continue
		}
		remoteRef, err := json.Marshal(signalDownloadOpaqueV1{
			Version:        1,
			Kind:           "remote",
			AttachmentID:   attachmentID,
			ConversationID: conversationID,
		})
		if err != nil {
			return nil, fmt.Errorf("encode Signal attachment %q: %w", attachmentID, err)
		}
		result = append(result, bridge.Attachment{
			RemoteID:  attachmentID,
			RemoteRef: remoteRef,
			Filename:  strings.TrimSpace(attachment.Filename),
			MIME:      strings.TrimSpace(attachment.ContentType),
		})
	}
	return result, nil
}
