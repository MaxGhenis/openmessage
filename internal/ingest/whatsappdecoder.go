package ingest

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	waE2E "go.mau.fi/whatsmeow/proto/waE2E"
	waHistorySync "go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/whatsapplive"
)

const (
	// WhatsAppCodec is the durable capture-time WhatsApp event codec.
	WhatsAppCodec = whatsapplive.IngressCodec
	// WhatsAppCodecVersion is the only envelope version understood here.
	WhatsAppCodecVersion uint32 = whatsapplive.IngressCodecVersion
)

const (
	whatsAppFrameMessage     = "message"
	whatsAppFrameReceipt     = "receipt"
	whatsAppFrameHistorySync = "history_sync"
)

// whatsAppFrameEnvelope is the union of the three v1 JSON envelope shapes.
// Encoding lives at the capture boundary in whatsapplive; this type is only
// used to parse the durable bytes without consulting any live WhatsApp state.
type whatsAppFrameEnvelope struct {
	Kind        string            `json:"kind"`
	ChatJID     string            `json:"chat_jid"`
	SenderJID   string            `json:"sender_jid"`
	MessageID   string            `json:"message_id"`
	TimestampMS int64             `json:"timestamp_ms"`
	IsFromMe    bool              `json:"is_from_me"`
	IsGroup     bool              `json:"is_group"`
	PushName    string            `json:"push_name"`
	Mentions    map[string]string `json:"mentions"`
	ProtoB64    []byte            `json:"proto_b64"`
	ReceiptType string            `json:"receipt_type"`
	MessageIDs  []string          `json:"message_ids"`
}

// WhatsAppDecoderSnapshot exposes deterministic skip classifications that do
// not produce bridge events (and therefore cannot use the worker's event-drop
// counters). The snapshot is partitioned by account like the ingest counters.
type WhatsAppDecoderSnapshot struct {
	EmptyMessagesSkipped       uint64
	UnsupportedMessagesSkipped uint64
	EncryptedReactionsSkipped  uint64
	UnsupportedReceiptsSkipped uint64
	InvalidHistoryItemsSkipped uint64
}

type whatsAppDecoderCounters struct {
	emptyMessages       atomic.Uint64
	unsupportedMessages atomic.Uint64
	encryptedReactions  atomic.Uint64
	unsupportedReceipts atomic.Uint64
	invalidHistoryItems atomic.Uint64
}

// WhatsAppDecoder maps capture-enriched WhatsApp frames to normalized events.
// It is deliberately store-free: canonical chat JIDs and mention labels must
// already be present in the durable frame.
type WhatsAppDecoder struct {
	mu       sync.RWMutex
	accounts map[string]*whatsAppDecoderCounters
}

var _ bridge.Decoder = (*WhatsAppDecoder)(nil)

// NewWhatsAppDecoder returns a v1 decoder. Its result is safe for concurrent
// Snapshot calls while the single ingest worker decodes frames.
func NewWhatsAppDecoder() *WhatsAppDecoder { return &WhatsAppDecoder{} }

// NewWhatsAppDecoderRegistration is the self-contained I3 registration seam.
// I1 intentionally has no mutable/default registry, so the stack owner should
// append this value to WorkerConfig.Decoders at merge time.
func NewWhatsAppDecoderRegistration() DecoderRegistration {
	return DecoderRegistration{
		Codec:    WhatsAppCodec,
		Platform: bridge.PlatformWhatsApp,
		Decoder:  NewWhatsAppDecoder(),
	}
}

// Snapshot returns no-event decode classifications for one account.
func (d *WhatsAppDecoder) Snapshot(accountID string) WhatsAppDecoderSnapshot {
	if d == nil {
		return WhatsAppDecoderSnapshot{}
	}
	d.mu.RLock()
	counters := d.accounts[accountID]
	d.mu.RUnlock()
	if counters == nil {
		return WhatsAppDecoderSnapshot{}
	}
	return WhatsAppDecoderSnapshot{
		EmptyMessagesSkipped:       counters.emptyMessages.Load(),
		UnsupportedMessagesSkipped: counters.unsupportedMessages.Load(),
		EncryptedReactionsSkipped:  counters.encryptedReactions.Load(),
		UnsupportedReceiptsSkipped: counters.unsupportedReceipts.Load(),
		InvalidHistoryItemsSkipped: counters.invalidHistoryItems.Load(),
	}
}

func (d *WhatsAppDecoder) account(accountID string) *whatsAppDecoderCounters {
	d.mu.RLock()
	counters := d.accounts[accountID]
	d.mu.RUnlock()
	if counters != nil {
		return counters
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.accounts == nil {
		d.accounts = make(map[string]*whatsAppDecoderCounters)
	}
	if counters = d.accounts[accountID]; counters == nil {
		counters = &whatsAppDecoderCounters{}
		d.accounts[accountID] = counters
	}
	return counters
}

// Decode parses one capture-time envelope without reaching into the live
// whatsmeow client, its contact store, or its LID-to-PN mapping.
func (d *WhatsAppDecoder) Decode(
	ctx context.Context,
	record bridge.RawIngressRecord,
) ([]bridge.Event, error) {
	if ctx == nil {
		return nil, fmt.Errorf("decode WhatsApp ingress: context is nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if record.Codec != WhatsAppCodec {
		return nil, fmt.Errorf(
			"decode WhatsApp ingress: codec %q is not %q",
			record.Codec,
			WhatsAppCodec,
		)
	}
	if record.CodecVersion != WhatsAppCodecVersion {
		return nil, fmt.Errorf(
			"decode WhatsApp ingress: codec version %d is unsupported",
			record.CodecVersion,
		)
	}

	var envelope whatsAppFrameEnvelope
	if err := json.Unmarshal(record.Payload, &envelope); err != nil {
		return nil, fmt.Errorf("decode WhatsApp ingress envelope: %w", err)
	}
	switch envelope.Kind {
	case whatsAppFrameMessage:
		return d.decodeMessage(record.AccountID, envelope)
	case whatsAppFrameReceipt:
		return d.decodeReceipt(record.AccountID, envelope)
	case whatsAppFrameHistorySync:
		return d.decodeHistorySync(record.AccountID, envelope.ProtoB64)
	default:
		return nil, fmt.Errorf(
			"decode WhatsApp ingress envelope: kind %q is unsupported",
			envelope.Kind,
		)
	}
}

func (d *WhatsAppDecoder) decodeMessage(
	accountID string,
	envelope whatsAppFrameEnvelope,
) ([]bridge.Event, error) {
	if strings.TrimSpace(envelope.ChatJID) == "" {
		return nil, fmt.Errorf("decode WhatsApp message: chat_jid is empty")
	}
	if strings.TrimSpace(envelope.MessageID) == "" {
		return nil, fmt.Errorf("decode WhatsApp message: message_id is empty")
	}
	if envelope.TimestampMS <= 0 {
		return nil, fmt.Errorf("decode WhatsApp message: timestamp_ms is not positive")
	}

	var message waE2E.Message
	if err := proto.Unmarshal(envelope.ProtoB64, &message); err != nil {
		return nil, fmt.Errorf("decode WhatsApp message protobuf: %w", err)
	}
	return d.decodeMessageProto(accountID, envelope, &message, false)
}

func (d *WhatsAppDecoder) decodeMessageProto(
	accountID string,
	envelope whatsAppFrameEnvelope,
	message *waE2E.Message,
	history bool,
) ([]bridge.Event, error) {
	conversationID := whatsAppRemoteConversationID(envelope.ChatJID)
	occurredAt := time.UnixMilli(envelope.TimestampMS)
	sender := whatsAppIdentity(envelope.SenderJID, envelope.PushName, envelope.IsFromMe)

	if reaction := whatsapplive.ExtractReactionMessage(message); reaction != nil {
		targetID := strings.TrimSpace(reaction.GetKey().GetID())
		if targetID == "" {
			d.account(accountID).unsupportedMessages.Add(1)
			return nil, nil
		}
		action := bridge.ReactionAdd
		emoji := strings.TrimSpace(reaction.GetText())
		if emoji == "" {
			action = bridge.ReactionRemove
		}
		return []bridge.Event{{
			Kind: bridge.EventReaction,
			Reaction: &bridge.ReactionEvent{
				RemoteConversationID:  conversationID,
				TargetRemoteMessageID: targetID,
				Actor:                 sender,
				Emoji:                 emoji,
				Action:                action,
				OccurredAt:            occurredAt,
			},
		}}, nil
	}

	if protocol := whatsapplive.ExtractProtocolMessage(message); protocol != nil {
		key := protocol.GetKey()
		targetID := strings.TrimSpace(key.GetID())
		switch protocol.GetType() {
		case waE2E.ProtocolMessage_MESSAGE_EDIT:
			if targetID == "" || protocol.GetEditedMessage() == nil {
				d.account(accountID).unsupportedMessages.Add(1)
				return nil, nil
			}
			body := whatsapplive.ExtractMessageBody(protocol.GetEditedMessage())
			body = FormatWhatsAppMentionedBody(body, envelope.Mentions)
			if body == "" || body == "[Unsupported message]" {
				d.account(accountID).unsupportedMessages.Add(1)
				return nil, nil
			}
			return []bridge.Event{{
				Kind: bridge.EventMessageMutation,
				MessageMutation: &bridge.MessageMutationEvent{
					RemoteConversationID: conversationID,
					RemoteMessageID:      targetID,
					Kind:                 "edit",
					Body:                 body,
					OccurredAt:           occurredAt,
				},
			}}, nil
		case waE2E.ProtocolMessage_REVOKE:
			// REVOKE is the enum zero value. As in the legacy handler, a real
			// target key is required before interpreting it as a deletion.
			if targetID == "" {
				d.account(accountID).unsupportedMessages.Add(1)
				return nil, nil
			}
			return []bridge.Event{{
				Kind: bridge.EventMessageMutation,
				MessageMutation: &bridge.MessageMutationEvent{
					RemoteConversationID: conversationID,
					RemoteMessageID:      targetID,
					Kind:                 "delete",
					OccurredAt:           occurredAt,
				},
			}}, nil
		default:
			d.account(accountID).unsupportedMessages.Add(1)
			return nil, nil
		}
	}

	if whatsapplive.HasEncryptedReactionMessage(message) {
		d.account(accountID).encryptedReactions.Add(1)
		return nil, nil
	}

	body := whatsapplive.ExtractMessageBody(message)
	switch body {
	case "":
		d.account(accountID).emptyMessages.Add(1)
		return nil, nil
	case "[Unsupported message]":
		d.account(accountID).unsupportedMessages.Add(1)
		return nil, nil
	}
	body = FormatWhatsAppMentionedBody(body, envelope.Mentions)
	attachments, err := whatsAppAttachments(message)
	if err != nil {
		return nil, err
	}
	direction := "incoming"
	if envelope.IsFromMe {
		direction = "outgoing"
	}
	event := bridge.Event{
		Kind: bridge.EventMessage,
		Message: &bridge.MessageEvent{
			RemoteConversationID: conversationID,
			RemoteMessageID:      strings.TrimSpace(envelope.MessageID),
			ClientRequestID:      "",
			Sender:               sender,
			Direction:            direction,
			Body:                 body,
			Attachments:          attachments,
			ReplyToRemoteID: strings.TrimPrefix(
				whatsapplive.ExtractReplyToID(message),
				"whatsapp:",
			),
			OccurredAt: occurredAt,
		},
	}
	if history && event.Message.RemoteMessageID == "" {
		d.account(accountID).invalidHistoryItems.Add(1)
		return nil, nil
	}
	return []bridge.Event{event}, nil
}

func (d *WhatsAppDecoder) decodeReceipt(
	accountID string,
	envelope whatsAppFrameEnvelope,
) ([]bridge.Event, error) {
	if strings.TrimSpace(envelope.ChatJID) == "" {
		return nil, fmt.Errorf("decode WhatsApp receipt: chat_jid is empty")
	}
	if envelope.TimestampMS <= 0 {
		return nil, fmt.Errorf("decode WhatsApp receipt: timestamp_ms is not positive")
	}
	status, self, ok := whatsAppReceiptStatus(envelope.ReceiptType)
	if !ok || len(envelope.MessageIDs) == 0 {
		d.account(accountID).unsupportedReceipts.Add(1)
		return nil, nil
	}
	messageIDs := make([]string, 0, len(envelope.MessageIDs))
	for _, messageID := range envelope.MessageIDs {
		if messageID = strings.TrimSpace(messageID); messageID != "" {
			messageIDs = append(messageIDs, messageID)
		}
	}
	if len(messageIDs) == 0 {
		d.account(accountID).unsupportedReceipts.Add(1)
		return nil, nil
	}
	actor := whatsAppIdentity(envelope.SenderJID, "", self)
	return []bridge.Event{{
		Kind: bridge.EventReceipt,
		Receipt: &bridge.ReceiptEvent{
			RemoteConversationID: whatsAppRemoteConversationID(envelope.ChatJID),
			RemoteMessageIDs:     messageIDs,
			Actor:                actor,
			Status:               status,
			OccurredAt:           time.UnixMilli(envelope.TimestampMS),
		},
	}}, nil
}

func (d *WhatsAppDecoder) decodeHistorySync(
	accountID string,
	protoBytes []byte,
) ([]bridge.Event, error) {
	var history waHistorySync.HistorySync
	if err := proto.Unmarshal(protoBytes, &history); err != nil {
		return nil, fmt.Errorf("decode WhatsApp history sync protobuf: %w", err)
	}
	aliases := whatsAppHistoryAliases(&history)
	pushNames := whatsAppHistoryPushNames(&history, aliases)
	var events []bridge.Event
	for _, conversation := range history.GetConversations() {
		if conversation == nil {
			d.account(accountID).invalidHistoryItems.Add(1)
			continue
		}
		chatJID := canonicalHistoryJID(conversation.GetID(), conversation.GetPnJID(), aliases)
		if chatJID == "" {
			d.account(accountID).invalidHistoryItems.Add(1)
			continue
		}
		for _, item := range conversation.GetMessages() {
			webMessage := item.GetMessage()
			if webMessage == nil || webMessage.GetMessage() == nil {
				d.account(accountID).invalidHistoryItems.Add(1)
				continue
			}
			key := webMessage.GetKey()
			messageID := strings.TrimSpace(key.GetID())
			timestamp := int64(webMessage.GetMessageTimestamp())
			if messageID == "" || timestamp <= 0 {
				d.account(accountID).invalidHistoryItems.Add(1)
				continue
			}
			// The enclosing conversation is authoritative here: it carries the
			// capture-time PN alias (`pnJID`) that a message key containing the
			// older LID form may not. Re-preferring key.remoteJID would undo that
			// canonicalization inside the pure decoder.
			messageChatJID := chatJID
			senderJID := strings.TrimSpace(webMessage.GetParticipant())
			if senderJID == "" {
				senderJID = strings.TrimSpace(key.GetParticipant())
			}
			if senderJID == "" && !key.GetFromMe() {
				senderJID = messageChatJID
			}
			senderJID = canonicalHistoryJID(senderJID, "", aliases)
			pushName := strings.TrimSpace(webMessage.GetPushName())
			if pushName == "" {
				pushName = pushNames[senderJID]
			}
			mentions := whatsAppHistoryMentions(webMessage.GetMessage(), aliases, pushNames)
			envelope := whatsAppFrameEnvelope{
				Kind:        whatsAppFrameMessage,
				ChatJID:     messageChatJID,
				SenderJID:   senderJID,
				MessageID:   messageID,
				TimestampMS: timestamp * 1000,
				IsFromMe:    key.GetFromMe(),
				IsGroup:     whatsAppJIDServer(messageChatJID) == types.GroupServer,
				PushName:    pushName,
				Mentions:    mentions,
			}
			decoded, err := d.decodeMessageProto(
				accountID,
				envelope,
				webMessage.GetMessage(),
				true,
			)
			if err != nil {
				return nil, err
			}
			// HistorySync is an import of message rows. Reactions and mutations
			// inside the snapshot do not have an ordered target projection here.
			for _, event := range decoded {
				if event.Kind == bridge.EventMessage {
					events = append(events, event)
				}
			}
		}
	}
	return events, nil
}

func whatsAppAttachments(message *waE2E.Message) ([]bridge.Attachment, error) {
	mediaRef, mediaKey, mimeType, ok := whatsapplive.ExtractStoredMediaRef(message)
	if !ok {
		return nil, nil
	}
	opaque, err := json.Marshal(struct {
		Version       int    `json:"v"`
		Kind          string `json:"kind"`
		DirectPath    string `json:"direct_path"`
		FileSHA256    string `json:"file_sha256"`
		FileEncSHA256 string `json:"file_enc_sha256"`
		MediaKey      string `json:"media_key"`
		FileLength    uint64 `json:"file_length"`
	}{
		Version:       1,
		Kind:          "remote",
		DirectPath:    mediaRef.DirectPath,
		FileSHA256:    mediaRef.FileSHA256,
		FileEncSHA256: mediaRef.FileEncSHA256,
		MediaKey:      hex.EncodeToString(mediaKey),
		FileLength:    mediaRef.FileLength,
	})
	if err != nil {
		return nil, fmt.Errorf("pack WhatsApp media reference: %w", err)
	}
	return []bridge.Attachment{{
		RemoteID:  mediaRef.DirectPath,
		RemoteRef: opaque,
		MIME:      strings.TrimSpace(mimeType),
		Size:      int64(mediaRef.FileLength),
	}}, nil
}

func whatsAppRemoteConversationID(chatJID string) string {
	return "whatsapp:" + strings.TrimSpace(chatJID)
}

func whatsAppIdentity(jid, name string, self bool) bridge.IdentityRef {
	jid = strings.TrimSpace(jid)
	raw := jid
	if parsed, err := types.ParseJID(jid); err == nil {
		parsed = parsed.ToNonAD()
		if parsed.Server == types.DefaultUserServer && allASCIIDigits(parsed.User) {
			raw = "+" + parsed.User
		} else {
			raw = parsed.String()
		}
	}
	return bridge.IdentityRef{
		Raw:    raw,
		Name:   strings.TrimSpace(name),
		IsSelf: self,
	}
}

func allASCIIDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

func whatsAppReceiptStatus(receiptType string) (status string, self bool, ok bool) {
	switch strings.ToLower(strings.TrimSpace(receiptType)) {
	case "delivered":
		return "delivered", false, true
	case "sender":
		// WhatsApp emits sender receipts from another linked device owned by
		// the current account. This is the specified sender-is-own case.
		return "delivered", true, true
	case "read":
		return "read", false, true
	case "read_self":
		return "read", true, true
	case "played":
		return "read", false, true
	case "played_self":
		return "read", true, true
	case "server_error":
		return "failed", false, true
	default:
		return "", false, false
	}
}

// FormatWhatsAppMentionedBody applies only the pure half of mention
// formatting. Capture has already resolved each JID to a display label.
func FormatWhatsAppMentionedBody(body string, mentions map[string]string) string {
	body = strings.TrimSpace(body)
	if body == "" || len(mentions) == 0 {
		return body
	}
	type replacement struct {
		token string
		value string
	}
	seen := make(map[string]struct{}, len(mentions))
	replacements := make([]replacement, 0, len(mentions))
	for rawJID, display := range mentions {
		display = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(display), "@~"))
		if display == "" {
			continue
		}
		jid := strings.TrimSpace(rawJID)
		if parsed, err := types.ParseJID(jid); err == nil {
			jid = strings.TrimSpace(parsed.ToNonAD().User)
		} else if index := strings.IndexByte(jid, '@'); index >= 0 {
			jid = strings.TrimSpace(jid[:index])
		}
		jid = strings.TrimPrefix(jid, "@")
		if jid == "" {
			continue
		}
		token := "@" + jid
		value := "@~" + display
		if token == value {
			continue
		}
		if _, duplicate := seen[token]; duplicate {
			continue
		}
		seen[token] = struct{}{}
		replacements = append(replacements, replacement{token: token, value: value})
	}
	sort.Slice(replacements, func(i, j int) bool {
		if len(replacements[i].token) != len(replacements[j].token) {
			return len(replacements[i].token) > len(replacements[j].token)
		}
		return replacements[i].token < replacements[j].token
	})
	for _, replacement := range replacements {
		body = strings.ReplaceAll(body, replacement.token, replacement.value)
	}
	return body
}

func whatsAppHistoryAliases(history *waHistorySync.HistorySync) map[string]string {
	aliases := make(map[string]string)
	for _, mapping := range history.GetPhoneNumberToLidMappings() {
		if mapping == nil {
			continue
		}
		lid := strings.TrimSpace(mapping.GetLidJID())
		pn := strings.TrimSpace(mapping.GetPnJID())
		if lid != "" && pn != "" {
			aliases[lid] = pn
		}
	}
	return aliases
}

func whatsAppHistoryPushNames(
	history *waHistorySync.HistorySync,
	aliases map[string]string,
) map[string]string {
	names := make(map[string]string)
	for _, item := range history.GetPushnames() {
		if item == nil {
			continue
		}
		id := canonicalHistoryJID(item.GetID(), "", aliases)
		name := strings.TrimSpace(item.GetPushname())
		if id != "" && name != "" {
			names[id] = name
		}
	}
	return names
}

func canonicalHistoryJID(raw, pn string, aliases map[string]string) string {
	raw = strings.TrimSpace(raw)
	pn = strings.TrimSpace(pn)
	if pn != "" && (raw == "" || whatsAppJIDServer(raw) == types.HiddenUserServer) {
		raw = pn
	}
	if canonical := strings.TrimSpace(aliases[raw]); canonical != "" {
		raw = canonical
	}
	if parsed, err := types.ParseJID(raw); err == nil {
		return parsed.ToNonAD().String()
	}
	return raw
}

func whatsAppJIDServer(raw string) string {
	parsed, err := types.ParseJID(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	return parsed.Server
}

func whatsAppHistoryMentions(
	message *waE2E.Message,
	aliases map[string]string,
	pushNames map[string]string,
) map[string]string {
	mentioned := whatsapplive.ExtractMentionedJIDs(message)
	if len(mentioned) == 0 {
		return nil
	}
	result := make(map[string]string)
	for _, raw := range mentioned {
		canonical := canonicalHistoryJID(raw, "", aliases)
		name := strings.TrimSpace(pushNames[canonical])
		if name == "" {
			continue
		}
		result[strings.TrimSpace(raw)] = name
		result[canonical] = name
	}
	return result
}
