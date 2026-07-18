package ingest

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	waCommon "go.mau.fi/whatsmeow/proto/waCommon"
	waE2E "go.mau.fi/whatsmeow/proto/waE2E"
	waHistorySync "go.mau.fi/whatsmeow/proto/waHistorySync"
	waWeb "go.mau.fi/whatsmeow/proto/waWeb"
	"google.golang.org/protobuf/proto"

	"github.com/maxghenis/openmessage/internal/bridge"
	whatsappadapter "github.com/maxghenis/openmessage/internal/bridgeadapters/whatsapp"
	legacydb "github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/migration"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
	"github.com/maxghenis/openmessage/internal/v2keys"
)

const (
	waDecoderAccountID = "whatsapp-decoder-account"
	waWorkerAccountID  = "whatsapp-worker-account"
	waCanonicalChatJID = "14155550100@s.whatsapp.net"
	waSenderJID        = "14155550200@s.whatsapp.net"
	waTimestampMS      = int64(1_752_777_123_456)
)

var waWorkerNow = time.Date(2026, 7, 17, 19, 30, 0, 0, time.UTC)

func TestWhatsAppDecoderRegistration(t *testing.T) {
	t.Parallel()

	registration := NewWhatsAppDecoderRegistration()
	if registration.Codec != WhatsAppCodec {
		t.Fatalf("registration codec = %q, want %q", registration.Codec, WhatsAppCodec)
	}
	if registration.Platform != bridge.PlatformWhatsApp {
		t.Fatalf("registration platform = %q, want %q", registration.Platform, bridge.PlatformWhatsApp)
	}
	if _, ok := registration.Decoder.(*WhatsAppDecoder); !ok {
		t.Fatalf("registration decoder type = %T, want *WhatsAppDecoder", registration.Decoder)
	}
}

func TestWhatsAppDecoderMessageFrameTable(t *testing.T) {
	t.Parallel()

	plainEnvelope := waBaseMessageEnvelope()
	plainEnvelope.MessageID = "plain-message"
	plainEnvelope.Mentions = map[string]string{
		"14155550300@s.whatsapp.net": "Ada Lovelace",
	}
	plainMessage := &waE2E.Message{
		ExtendedTextMessage: &waE2E.ExtendedTextMessage{
			Text: proto.String("hello @14155550300"),
			ContextInfo: &waE2E.ContextInfo{
				StanzaID:     proto.String("reply-stanza"),
				MentionedJID: []string{"14155550300@s.whatsapp.net"},
			},
		},
	}

	mediaEnvelope := waBaseMessageEnvelope()
	mediaEnvelope.MessageID = "media-message"
	mediaEnvelope.IsFromMe = true
	mediaEnvelope.SenderJID = "14155550999:7@s.whatsapp.net"
	mediaMessage := &waE2E.Message{
		ImageMessage: &waE2E.ImageMessage{
			Caption:       proto.String("captured photo"),
			Mimetype:      proto.String("image/jpeg"),
			DirectPath:    proto.String("/mms/image/decoder-fixture"),
			FileSHA256:    []byte{0x01, 0x02, 0x03},
			FileEncSHA256: []byte{0x04, 0x05, 0x06},
			MediaKey:      []byte{0x07, 0x08, 0x09},
			FileLength:    proto.Uint64(321),
		},
	}

	reactionEnvelope := waBaseMessageEnvelope()
	reactionEnvelope.MessageID = "reaction-envelope"
	reactionAdd := &waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{
		Key:  &waCommon.MessageKey{ID: proto.String("reaction-target")},
		Text: proto.String("👍"),
	}}
	reactionRemove := &waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{
		Key:  &waCommon.MessageKey{ID: proto.String("reaction-target")},
		Text: proto.String(""),
	}}

	tests := []struct {
		name     string
		envelope whatsAppFrameEnvelope
		message  *waE2E.Message
		check    func(*testing.T, bridge.Event)
	}{
		{
			name:     "plain with canonical chat, reply, and captured mention",
			envelope: plainEnvelope,
			message:  plainMessage,
			check: func(t *testing.T, event bridge.Event) {
				t.Helper()
				if event.Kind != bridge.EventMessage || event.Message == nil {
					t.Fatalf("event = %+v, want MessageEvent", event)
				}
				message := event.Message
				if message.RemoteConversationID != "whatsapp:"+waCanonicalChatJID {
					t.Fatalf("remote conversation ID = %q, want capture-time JID byte-for-byte", message.RemoteConversationID)
				}
				if message.RemoteMessageID != "plain-message" || message.ClientRequestID != "" {
					t.Fatalf("message IDs = (%q, %q), want (plain-message, empty client request)", message.RemoteMessageID, message.ClientRequestID)
				}
				if message.Direction != "incoming" || message.Body != "hello @~Ada Lovelace" {
					t.Fatalf("message direction/body = (%q, %q), want incoming pure mention formatting", message.Direction, message.Body)
				}
				if message.ReplyToRemoteID != "reply-stanza" {
					t.Fatalf("reply ID = %q, want reply-stanza", message.ReplyToRemoteID)
				}
				if message.Sender.Raw != "+14155550200" || message.Sender.Name != "Grace Hopper" || message.Sender.IsSelf {
					t.Fatalf("sender = %+v, want captured PN identity", message.Sender)
				}
				if !message.OccurredAt.Equal(time.UnixMilli(waTimestampMS)) {
					t.Fatalf("occurred at = %v, want %v", message.OccurredAt, time.UnixMilli(waTimestampMS))
				}
			},
		},
		{
			name:     "media packs remote download reference",
			envelope: mediaEnvelope,
			message:  mediaMessage,
			check: func(t *testing.T, event bridge.Event) {
				t.Helper()
				if event.Kind != bridge.EventMessage || event.Message == nil {
					t.Fatalf("event = %+v, want MessageEvent", event)
				}
				message := event.Message
				if message.Direction != "outgoing" || !message.Sender.IsSelf || message.ClientRequestID != "" {
					t.Fatalf("outgoing media routing = direction %q sender %+v client_request_id %q", message.Direction, message.Sender, message.ClientRequestID)
				}
				if message.Body != "captured photo" || len(message.Attachments) != 1 {
					t.Fatalf("media message = body %q attachments %+v", message.Body, message.Attachments)
				}
				attachment := message.Attachments[0]
				if attachment.RemoteID != "/mms/image/decoder-fixture" || attachment.MIME != "image/jpeg" || attachment.Size != 321 {
					t.Fatalf("attachment metadata = %+v", attachment)
				}
				if err := whatsappadapter.ValidateDownloadOpaque(attachment.RemoteRef); err != nil {
					t.Fatalf("ValidateDownloadOpaque(): %v; raw=%s", err, attachment.RemoteRef)
				}
				var packed struct {
					Version       int    `json:"v"`
					Kind          string `json:"kind"`
					DirectPath    string `json:"direct_path"`
					FileSHA256    string `json:"file_sha256"`
					FileEncSHA256 string `json:"file_enc_sha256"`
					MediaKey      string `json:"media_key"`
					FileLength    uint64 `json:"file_length"`
				}
				if err := json.Unmarshal(attachment.RemoteRef, &packed); err != nil {
					t.Fatalf("unmarshal packed media reference: %v", err)
				}
				if packed.Version != 1 || packed.Kind != "remote" ||
					packed.DirectPath != "/mms/image/decoder-fixture" ||
					packed.FileSHA256 != "010203" || packed.FileEncSHA256 != "040506" ||
					packed.MediaKey != "070809" || packed.FileLength != 321 {
					t.Fatalf("packed media reference = %+v", packed)
				}
			},
		},
		{
			name:     "reaction add",
			envelope: reactionEnvelope,
			message:  reactionAdd,
			check: func(t *testing.T, event bridge.Event) {
				t.Helper()
				if event.Kind != bridge.EventReaction || event.Reaction == nil {
					t.Fatalf("event = %+v, want ReactionEvent", event)
				}
				reaction := event.Reaction
				if reaction.RemoteConversationID != "whatsapp:"+waCanonicalChatJID ||
					reaction.TargetRemoteMessageID != "reaction-target" ||
					reaction.Emoji != "👍" || reaction.Action != bridge.ReactionAdd {
					t.Fatalf("reaction add = %+v", reaction)
				}
			},
		},
		{
			name:     "reaction remove",
			envelope: reactionEnvelope,
			message:  reactionRemove,
			check: func(t *testing.T, event bridge.Event) {
				t.Helper()
				if event.Kind != bridge.EventReaction || event.Reaction == nil {
					t.Fatalf("event = %+v, want ReactionEvent", event)
				}
				if event.Reaction.Action != bridge.ReactionRemove || event.Reaction.Emoji != "" {
					t.Fatalf("reaction remove = %+v", event.Reaction)
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			decoder := NewWhatsAppDecoder()
			record, _ := waMessageRecord(t, test.envelope, test.message)
			events, err := decoder.Decode(context.Background(), record)
			if err != nil {
				t.Fatalf("Decode(): %v", err)
			}
			if len(events) != 1 {
				t.Fatalf("Decode() event count = %d, want 1: %+v", len(events), events)
			}
			test.check(t, events[0])
		})
	}
}

func TestFormatWhatsAppMentionedBodyIsPureAndDeterministic(t *testing.T) {
	t.Parallel()

	mentions := map[string]string{
		"123@s.whatsapp.net":   "@~Short Name",
		"12345@s.whatsapp.net": "Long Name",
	}
	wantMentions := map[string]string{
		"123@s.whatsapp.net":   "@~Short Name",
		"12345@s.whatsapp.net": "Long Name",
	}
	const body = "@12345 ping @123 and leave @999 alone"
	const want = "@~Long Name ping @~Short Name and leave @999 alone"

	for attempt := 0; attempt < 2; attempt++ {
		if got := FormatWhatsAppMentionedBody(body, mentions); got != want {
			t.Fatalf("FormatWhatsAppMentionedBody() = %q, want %q", got, want)
		}
	}
	if !reflect.DeepEqual(mentions, wantMentions) {
		t.Fatalf("mention map mutated: got %+v, want %+v", mentions, wantMentions)
	}
}

func TestWhatsAppDecoderMutationFrameTable(t *testing.T) {
	t.Parallel()

	base := waBaseMessageEnvelope()
	base.Mentions = map[string]string{"14155550300@s.whatsapp.net": "Katherine Johnson"}
	tests := []struct {
		name       string
		message    *waE2E.Message
		wantKind   string
		wantTarget string
		wantBody   string
	}{
		{
			name: "edit",
			message: &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
				Key:  &waCommon.MessageKey{ID: proto.String("edit-target")},
				Type: waE2E.ProtocolMessage_MESSAGE_EDIT.Enum(),
				EditedMessage: &waE2E.Message{
					ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String("edited for @14155550300")},
				},
			}},
			wantKind:   "edit",
			wantTarget: "edit-target",
			wantBody:   "edited for @~Katherine Johnson",
		},
		{
			name: "delete",
			message: &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
				Key:  &waCommon.MessageKey{ID: proto.String("delete-target")},
				Type: waE2E.ProtocolMessage_REVOKE.Enum(),
			}},
			wantKind:   "delete",
			wantTarget: "delete-target",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			envelope := base
			envelope.MessageID = test.name + "-envelope"
			record, _ := waMessageRecord(t, envelope, test.message)
			events, err := NewWhatsAppDecoder().Decode(context.Background(), record)
			if err != nil {
				t.Fatalf("Decode(): %v", err)
			}
			if len(events) != 1 || events[0].Kind != bridge.EventMessageMutation || events[0].MessageMutation == nil {
				t.Fatalf("events = %+v, want one MessageMutationEvent", events)
			}
			mutation := events[0].MessageMutation
			if mutation.RemoteConversationID != "whatsapp:"+waCanonicalChatJID ||
				mutation.RemoteMessageID != test.wantTarget || mutation.Kind != test.wantKind ||
				mutation.Body != test.wantBody || !mutation.OccurredAt.Equal(time.UnixMilli(waTimestampMS)) {
				t.Fatalf("mutation = %+v", mutation)
			}
		})
	}
}

func TestWhatsAppDecoderSkipClassifications(t *testing.T) {
	t.Parallel()

	decoder := NewWhatsAppDecoder()
	tests := []struct {
		name    string
		message *waE2E.Message
	}{
		{
			name: "encrypted reaction",
			message: &waE2E.Message{EncReactionMessage: &waE2E.EncReactionMessage{
				TargetMessageKey: &waCommon.MessageKey{ID: proto.String("encrypted-target")},
				EncPayload:       []byte{1, 2, 3},
				EncIV:            []byte{4, 5, 6},
			}},
		},
		{
			name: "unsupported",
			message: &waE2E.Message{SendPaymentMessage: &waE2E.SendPaymentMessage{
				TransactionData: proto.String("opaque-payment-data"),
			}},
		},
		{name: "empty", message: &waE2E.Message{}},
	}

	for _, test := range tests {
		envelope := waBaseMessageEnvelope()
		envelope.MessageID = "skip-" + test.name
		record, _ := waMessageRecord(t, envelope, test.message)
		events, err := decoder.Decode(context.Background(), record)
		if err != nil {
			t.Fatalf("Decode(%s): %v", test.name, err)
		}
		if len(events) != 0 {
			t.Fatalf("Decode(%s) events = %+v, want none", test.name, events)
		}
	}

	want := WhatsAppDecoderSnapshot{
		EmptyMessagesSkipped:       1,
		UnsupportedMessagesSkipped: 1,
		EncryptedReactionsSkipped:  1,
	}
	if got := decoder.Snapshot(waDecoderAccountID); got != want {
		t.Fatalf("Snapshot() = %+v, want %+v", got, want)
	}
	if got := decoder.Snapshot("another-account"); got != (WhatsAppDecoderSnapshot{}) {
		t.Fatalf("Snapshot(other account) = %+v, want zero", got)
	}
}

func TestWhatsAppDecoderReceiptFrameTable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		receiptType string
		wantStatus  string
		wantSelf    bool
	}{
		{name: "read self", receiptType: "read_self", wantStatus: "read", wantSelf: true},
		{name: "sender linked device", receiptType: "sender", wantStatus: "delivered", wantSelf: true},
		{name: "read by other", receiptType: "read", wantStatus: "read", wantSelf: false},
		{name: "server error", receiptType: "server_error", wantStatus: "failed", wantSelf: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			envelope := whatsAppFrameEnvelope{
				Kind:        whatsAppFrameReceipt,
				ChatJID:     waCanonicalChatJID,
				SenderJID:   waSenderJID,
				TimestampMS: waTimestampMS,
				ReceiptType: test.receiptType,
				MessageIDs:  []string{" first-message ", "second-message"},
			}
			events, err := NewWhatsAppDecoder().Decode(
				context.Background(),
				waRecord(waJSON(t, envelope)),
			)
			if err != nil {
				t.Fatalf("Decode(): %v", err)
			}
			if len(events) != 1 || events[0].Kind != bridge.EventReceipt || events[0].Receipt == nil {
				t.Fatalf("events = %+v, want one ReceiptEvent", events)
			}
			receipt := events[0].Receipt
			if receipt.RemoteConversationID != "whatsapp:"+waCanonicalChatJID ||
				receipt.Status != test.wantStatus || receipt.Actor.IsSelf != test.wantSelf ||
				receipt.Actor.Raw != "+14155550200" ||
				!reflect.DeepEqual(receipt.RemoteMessageIDs, []string{"first-message", "second-message"}) ||
				!receipt.OccurredAt.Equal(time.UnixMilli(waTimestampMS)) {
				t.Fatalf("receipt = %+v", receipt)
			}
		})
	}

	decoder := NewWhatsAppDecoder()
	unsupported := whatsAppFrameEnvelope{
		Kind:        whatsAppFrameReceipt,
		ChatJID:     waCanonicalChatJID,
		SenderJID:   waSenderJID,
		TimestampMS: waTimestampMS,
		ReceiptType: "typing",
		MessageIDs:  []string{"ignored"},
	}
	events, err := decoder.Decode(context.Background(), waRecord(waJSON(t, unsupported)))
	if err != nil || len(events) != 0 {
		t.Fatalf("unsupported receipt = events %+v error %v, want skipped", events, err)
	}
	if got := decoder.Snapshot(waDecoderAccountID).UnsupportedReceiptsSkipped; got != 1 {
		t.Fatalf("unsupported receipt count = %d, want 1", got)
	}
}

func TestWhatsAppDecoderHistorySyncTwoMessages(t *testing.T) {
	t.Parallel()

	record := waHistoryRecord(t, waHistoryFixture())
	events, err := NewWhatsAppDecoder().Decode(context.Background(), record)
	if err != nil {
		t.Fatalf("Decode(): %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("history event count = %d, want 2: %+v", len(events), events)
	}
	wantIDs := []string{"history-first", "history-second"}
	wantBodies := []string{"first historical body", "second historical body"}
	wantDirections := []string{"incoming", "outgoing"}
	for index, event := range events {
		if event.Kind != bridge.EventMessage || event.Message == nil {
			t.Fatalf("history event %d = %+v, want MessageEvent", index, event)
		}
		message := event.Message
		if message.RemoteConversationID != "whatsapp:"+waCanonicalChatJID ||
			message.RemoteMessageID != wantIDs[index] || message.Body != wantBodies[index] ||
			message.Direction != wantDirections[index] || message.ClientRequestID != "" {
			t.Fatalf("history message %d = %+v", index, message)
		}
		wantTime := time.UnixMilli(int64(1_700_000_001+index) * 1000)
		if !message.OccurredAt.Equal(wantTime) {
			t.Fatalf("history message %d occurred at = %v, want %v", index, message.OccurredAt, wantTime)
		}
	}
}

func TestWhatsAppHistorySyncWorkerProjectsFirstAndImportsSecond(t *testing.T) {
	harness := newWhatsAppWorkerHarness(t)
	record := waHistoryRecord(t, waHistoryFixture())
	record.AccountID = waWorkerAccountID
	record.DedupeKey = "hist:" + waHash8(record.Payload)
	record.ReceivedAt = waWorkerNow

	if err := harness.sink.AppendIngress(context.Background(), record); err != nil {
		t.Fatalf("AppendIngress(): %v", err)
	}
	i01WaitFor(t, "WhatsApp history projection/import", func() bool {
		snapshot := harness.counters.Snapshot(waWorkerAccountID)
		return snapshot.Projected == 1 && snapshot.Imported == 1 && snapshot.DecodedEvents == 2
	})

	if got := i01QueryInt64(t, harness.path, `SELECT COUNT(*) FROM inbox`); got != 1 {
		t.Fatalf("history inbox rows = %d, want 1", got)
	}
	if got := i01QueryInt64(t, harness.path, `SELECT COUNT(*) FROM messages`); got != 2 {
		t.Fatalf("history message rows = %d, want 2", got)
	}
	conversation, err := harness.store.GetConversationByRemote(
		waWorkerAccountID,
		"whatsapp:"+waCanonicalChatJID,
	)
	if err != nil {
		t.Fatalf("GetConversationByRemote(): %v", err)
	}
	for _, remoteID := range []string{"history-first", "history-second"} {
		if _, err := harness.messages.GetMessageByRemote(
			context.Background(),
			waWorkerAccountID,
			conversation.ConversationID,
			remoteID,
		); err != nil {
			t.Errorf("GetMessageByRemote(%q): %v", remoteID, err)
		}
	}
	i01AssertNoPending(t, harness.messages)
}

func TestWhatsAppReactionDeltasApplySwitchRemoveAndOrphanThroughWorker(t *testing.T) {
	harness := newWhatsAppWorkerHarness(t)
	appendFrame := func(
		label string,
		envelope whatsAppFrameEnvelope,
		message *waE2E.Message,
	) bridge.RawIngressRecord {
		t.Helper()
		record, protoBytes := waMessageRecord(t, envelope, message)
		record.AccountID = waWorkerAccountID
		record.DedupeKey = "reaction:" + label + ":" + waHash8(protoBytes)
		record.ReceivedAt = waWorkerNow.Add(time.Duration(envelope.TimestampMS-waTimestampMS) * time.Millisecond)
		if err := harness.sink.AppendIngress(context.Background(), record); err != nil {
			t.Fatalf("AppendIngress(%s): %v", label, err)
		}
		return record
	}

	targetEnvelope := waBaseMessageEnvelope()
	targetEnvelope.MessageID = "reaction-target"
	appendFrame(
		"target",
		targetEnvelope,
		&waE2E.Message{Conversation: proto.String("react to this")},
	)
	i01WaitFor(t, "WhatsApp reaction target projection", func() bool {
		return harness.counters.Snapshot(waWorkerAccountID).Projected == 1
	})
	conversation, err := harness.store.GetConversationByRemote(
		waWorkerAccountID,
		"whatsapp:"+waCanonicalChatJID,
	)
	if err != nil {
		t.Fatalf("GetConversationByRemote(): %v", err)
	}
	target, err := harness.messages.GetMessageByRemote(
		context.Background(),
		waWorkerAccountID,
		conversation.ConversationID,
		"reaction-target",
	)
	if err != nil {
		t.Fatalf("GetMessageByRemote(reaction target): %v", err)
	}

	reactionFrame := func(
		label string,
		targetID string,
		emoji string,
		occurredAtMS int64,
		fromMe bool,
	) bridge.RawIngressRecord {
		envelope := waBaseMessageEnvelope()
		envelope.MessageID = "reaction-envelope-" + label
		envelope.TimestampMS = occurredAtMS
		envelope.IsFromMe = fromMe
		return appendFrame(label, envelope, &waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{
			Key:  &waCommon.MessageKey{ID: proto.String(targetID)},
			Text: proto.String(emoji),
		}})
	}

	changes := harness.worker.Changes()
	reactionFrame("add", "reaction-target", "👍", waTimestampMS+100, false)
	i01WaitFor(t, "WhatsApp reaction add", func() bool {
		return harness.counters.Snapshot(waWorkerAccountID).ReactionsApplied == 1
	})
	assertWorkerChangeClosed(t, changes, "WhatsApp reaction add")
	assertSingleActiveReaction(t, harness.reactions, target.MessageID, "👍", "+14155550200", false)

	// WhatsApp represents a switch as another add delta by the same reactor.
	reactionFrame("switch", "reaction-target", "❤️", waTimestampMS+200, false)
	i01WaitFor(t, "WhatsApp reaction switch", func() bool {
		return harness.counters.Snapshot(waWorkerAccountID).ReactionsApplied == 2
	})
	assertSingleActiveReaction(t, harness.reactions, target.MessageID, "❤️", "+14155550200", false)

	reactionFrame("remove", "reaction-target", "", waTimestampMS+300, false)
	i01WaitFor(t, "WhatsApp reaction removal", func() bool {
		return harness.counters.Snapshot(waWorkerAccountID).ReactionsRemoved == 1
	})
	assertNoActiveReactions(t, harness.reactions, target.MessageID)
	if got := i01QueryInt64(t, harness.path, `
		SELECT COUNT(*)
		FROM reactions
		WHERE message_id = ? AND state = 'removed' AND emoji = '❤️'
	`, target.MessageID); got != 1 {
		t.Fatalf("retained WhatsApp tombstones = %d, want 1", got)
	}

	// A late WAL-drain add that predates the removal must not resurrect it.
	reactionFrame("stale-add", "reaction-target", "😂", waTimestampMS+250, false)
	i01WaitFor(t, "WhatsApp stale reaction frame processing", func() bool {
		pending, err := harness.messages.Unprocessed(context.Background())
		return err == nil && len(pending) == 0
	})
	assertNoActiveReactions(t, harness.reactions, target.MessageID)
	snapshot := harness.counters.Snapshot(waWorkerAccountID)
	if snapshot.ReactionsApplied != 2 || snapshot.ReactionsRemoved != 1 {
		t.Fatalf("counters after stale reaction = %+v, want applied=2 removed=1", snapshot)
	}

	selfRecord := reactionFrame("self", "reaction-target", "🔥", waTimestampMS+400, true)
	i01WaitFor(t, "WhatsApp self-reaction echo", func() bool {
		return harness.counters.Snapshot(waWorkerAccountID).ReactionsApplied == 3
	})
	assertSingleActiveReaction(t, harness.reactions, target.MessageID, "🔥", "", true)
	if err := harness.sink.AppendIngress(context.Background(), selfRecord); err != nil {
		t.Fatalf("AppendIngress(self reaction replay): %v", err)
	}
	i01WaitFor(t, "WhatsApp self-reaction replay", func() bool {
		snapshot := harness.counters.Snapshot(waWorkerAccountID)
		return snapshot.Deduped == 1 && snapshot.DecodedEvents == 7
	})
	assertSingleActiveReaction(t, harness.reactions, target.MessageID, "🔥", "", true)

	reactionFrame("orphan", "missing-reaction-target", "👍", waTimestampMS+500, false)
	i01WaitFor(t, "WhatsApp orphan reaction", func() bool {
		return harness.counters.Snapshot(waWorkerAccountID).ReactionsOrphaned == 1
	})
	snapshot = harness.counters.Snapshot(waWorkerAccountID)
	if snapshot.Quarantined != 0 {
		t.Fatalf("orphan reaction counters = %+v, want no quarantine", snapshot)
	}
	i01AssertNoPending(t, harness.messages)
}

func TestWhatsAppMigratedReactionsConvergeWithLiveDecoderAndWorker(t *testing.T) {
	const (
		accountID          = "whatsapp-primary"
		conversationRemote = "whatsapp:" + waCanonicalChatJID
		contactTarget      = "wa-contact-target"
		selfTarget         = "wa-self-target"
		ownJID             = "14155550300@s.whatsapp.net"
	)
	root := t.TempDir()
	legacyPath := filepath.Join(root, "legacy.sqlite3")
	legacy, err := legacydb.New(legacyPath)
	if err != nil {
		t.Fatalf("legacydb.New(): %v", err)
	}
	if err := legacy.UpsertConversation(&legacydb.Conversation{ConversationID: conversationRemote, Name: "WA convergence", Participants: `[]`, LastMessageTS: waTimestampMS, SourcePlatform: "whatsapp"}); err != nil {
		t.Fatal(err)
	}
	for _, message := range []*legacydb.Message{
		{MessageID: "whatsapp:" + contactTarget, ConversationID: conversationRemote, SenderName: "Grace", SenderNumber: waSenderJID, Body: "contact target", TimestampMS: waTimestampMS, SourcePlatform: "whatsapp", Reactions: `[{"emoji":"👍","count":1,"actors":["` + waSenderJID + `"]}]`},
		{MessageID: "whatsapp:" + selfTarget, ConversationID: conversationRemote, SenderName: "Grace", SenderNumber: waSenderJID, Body: "self target", TimestampMS: waTimestampMS + 1, SourcePlatform: "whatsapp", Reactions: `[{"emoji":"🔥","count":1,"actors":["` + ownJID + `"]}]`},
		{MessageID: "whatsapp:own-number-evidence", ConversationID: conversationRemote, SenderName: "Me", SenderNumber: "+14155550300", Body: "own number evidence", TimestampMS: waTimestampMS - 1, IsFromMe: true, SourcePlatform: "whatsapp"},
	} {
		if err := legacy.UpsertMessage(message); err != nil {
			t.Fatal(err)
		}
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	staged := filepath.Join(root, "staged.sqlite3")
	report, err := migration.Transform(context.Background(), migration.Options{SourcePath: legacyPath, TempStorePath: staged, TempBlobPath: filepath.Join(root, "blobs"), TargetPath: filepath.Join(root, "target"), TargetStorePath: filepath.Join(root, "target", "store.sqlite3"), Check: true})
	if err != nil {
		t.Fatalf("migration.Transform(): %v (report=%+v)", err, report)
	}
	store, err := sqlite.Open(staged)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	messages, err := sqlite.NewMessageRepository(store, func() time.Time { return waWorkerNow })
	if err != nil {
		t.Fatal(err)
	}
	harness := newWhatsAppWorkerHarnessForStore(t, staged, store, messages)

	conversation, err := store.GetConversationByRemote(accountID, conversationRemote)
	if err != nil {
		t.Fatal(err)
	}
	messageID := func(remote string) string {
		return v2keys.DeriveID("message", accountID, conversationRemote+"\x1f"+remote)
	}
	contactKey := v2keys.DeriveID("identity", accountID, "e164\x1f+14155550200")
	assertReactionDBRow(t, staged, messageID(contactTarget), contactKey, 0, "", "👍", 1)
	assertReactionDBRow(t, staged, messageID(selfTarget), "self", 1, "me", "🔥", 1)

	appendReaction := func(label, target, emoji, sender string, fromMe bool, at int64) {
		envelope := waBaseMessageEnvelope()
		envelope.ChatJID, envelope.SenderJID, envelope.MessageID = waCanonicalChatJID, sender, "migration-reaction-"+label
		envelope.TimestampMS, envelope.IsFromMe = at, fromMe
		record, bytes := waMessageRecord(t, envelope, &waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{Key: &waCommon.MessageKey{ID: proto.String(target)}, Text: proto.String(emoji)}})
		record.AccountID, record.DedupeKey, record.ReceivedAt = accountID, "migration-reaction:"+label+":"+waHash8(bytes), waWorkerNow
		if err := harness.sink.AppendIngress(context.Background(), record); err != nil {
			t.Fatal(err)
		}
	}
	appendReaction("contact", contactTarget, "❤️", waSenderJID, false, waTimestampMS+100)
	appendReaction("self", selfTarget, "😂", ownJID, true, waTimestampMS+101)
	i01WaitFor(t, "migrated WhatsApp reactions applied", func() bool { return harness.counters.Snapshot(accountID).ReactionsApplied == 2 })
	assertReactionDBRow(t, staged, messageID(contactTarget), contactKey, 0, "", "❤️", 1)
	assertReactionDBRow(t, staged, messageID(selfTarget), "self", 1, "me", "😂", 1)
	_ = conversation
}

func assertReactionDBRow(t *testing.T, path, messageID, key string, isSelf int, label, emoji string, count int) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var gotCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM reactions WHERE message_id = ? AND reactor_key = ?`, messageID, key).Scan(&gotCount); err != nil {
		t.Fatal(err)
	}
	if gotCount != count {
		t.Fatalf("reaction (%s, %s) rows = %d, want %d", messageID, key, gotCount, count)
	}
	var total int
	if err := db.QueryRow(`SELECT COUNT(*) FROM reactions WHERE message_id = ?`, messageID).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != count {
		t.Fatalf("all reaction rows for message %s = %d, want exactly %d converged row", messageID, total, count)
	}
	var gotSelf int
	var gotLabel, gotEmoji string
	if err := db.QueryRow(`SELECT reactor_is_self, reactor_label, emoji FROM reactions WHERE message_id = ? AND reactor_key = ?`, messageID, key).Scan(&gotSelf, &gotLabel, &gotEmoji); err != nil {
		t.Fatal(err)
	}
	if gotSelf != isSelf || gotLabel != label || gotEmoji != emoji {
		t.Fatalf("reaction row = self=%d label=%q emoji=%q, want %d/%q/%q", gotSelf, gotLabel, gotEmoji, isSelf, label, emoji)
	}
}

func TestWhatsAppIngressDedupeUsesMessageIDAndProtoHash(t *testing.T) {
	harness := newWhatsAppWorkerHarness(t)
	envelope := waBaseMessageEnvelope()
	envelope.MessageID = "dedupe-message"
	firstRecord, firstProto := waMessageRecord(
		t,
		envelope,
		&waE2E.Message{Conversation: proto.String("body-a")},
	)
	firstRecord.AccountID = waWorkerAccountID
	firstRecord.ReceivedAt = waWorkerNow
	firstRecord.DedupeKey = "msg:" + envelope.MessageID + ":" + waHash8(firstProto)

	if err := harness.sink.AppendIngress(context.Background(), firstRecord); err != nil {
		t.Fatalf("AppendIngress(first): %v", err)
	}
	i01WaitFor(t, "first WhatsApp message projection", func() bool {
		return harness.counters.Snapshot(waWorkerAccountID).Projected == 1
	})
	if err := harness.sink.AppendIngress(context.Background(), firstRecord); err != nil {
		t.Fatalf("AppendIngress(exact replay): %v", err)
	}
	i01WaitFor(t, "exact WhatsApp replay", func() bool {
		snapshot := harness.counters.Snapshot(waWorkerAccountID)
		return snapshot.Deduped == 1 && snapshot.Projected == 2
	})

	changedRecord, changedProto := waMessageRecord(
		t,
		envelope,
		&waE2E.Message{Conversation: proto.String("body-b")},
	)
	changedRecord.AccountID = waWorkerAccountID
	changedRecord.ReceivedAt = waWorkerNow
	changedRecord.DedupeKey = "msg:" + envelope.MessageID + ":" + waHash8(changedProto)
	if firstRecord.DedupeKey == changedRecord.DedupeKey {
		t.Fatalf("changed protobuf unexpectedly produced the same dedupe key %q", firstRecord.DedupeKey)
	}
	if err := harness.sink.AppendIngress(context.Background(), changedRecord); err != nil {
		t.Fatalf("AppendIngress(changed proto): %v", err)
	}
	i01WaitFor(t, "changed WhatsApp content projection", func() bool {
		snapshot := harness.counters.Snapshot(waWorkerAccountID)
		return snapshot.Appended == 2 && snapshot.Projected == 3
	})

	if got := i01QueryInt64(t, harness.path, `SELECT COUNT(*) FROM inbox`); got != 2 {
		t.Fatalf("inbox rows = %d, want 2 content-distinct rows", got)
	}
	if got := i01QueryInt64(t, harness.path, `SELECT COUNT(*) FROM messages`); got != 1 {
		t.Fatalf("message rows = %d, want one natural-key row", got)
	}
	conversation, err := harness.store.GetConversationByRemote(
		waWorkerAccountID,
		"whatsapp:"+waCanonicalChatJID,
	)
	if err != nil {
		t.Fatalf("GetConversationByRemote(): %v", err)
	}
	message, err := harness.messages.GetMessageByRemote(
		context.Background(),
		waWorkerAccountID,
		conversation.ConversationID,
		envelope.MessageID,
	)
	if err != nil {
		t.Fatalf("GetMessageByRemote(): %v", err)
	}
	if message.Body != "body-b" {
		t.Fatalf("message body = %q, want body-b", message.Body)
	}
	snapshot := harness.counters.Snapshot(waWorkerAccountID)
	if snapshot.Appended != 2 || snapshot.Deduped != 1 {
		t.Fatalf("dedupe counters = %+v, want appended=2 deduped=1", snapshot)
	}
	i01AssertNoPending(t, harness.messages)
}

func TestWhatsAppDecoderRejectsMalformedBoundaries(t *testing.T) {
	t.Parallel()

	validEnvelope := whatsAppFrameEnvelope{
		Kind:        whatsAppFrameReceipt,
		ChatJID:     waCanonicalChatJID,
		SenderJID:   waSenderJID,
		TimestampMS: waTimestampMS,
		ReceiptType: "read",
		MessageIDs:  []string{"message"},
	}
	wrongCodec := waRecord(waJSON(t, validEnvelope))
	wrongCodec.Codec = "whatsapp.other"
	wrongVersion := waRecord(waJSON(t, validEnvelope))
	wrongVersion.CodecVersion = WhatsAppCodecVersion + 1
	invalidProtoEnvelope := waBaseMessageEnvelope()
	invalidProtoEnvelope.MessageID = "invalid-proto"
	invalidProtoEnvelope.ProtoB64 = []byte{0xff}

	tests := []struct {
		name        string
		record      bridge.RawIngressRecord
		wantInError string
	}{
		{name: "codec", record: wrongCodec, wantInError: "codec"},
		{name: "version", record: wrongVersion, wantInError: "version"},
		{name: "JSON payload", record: waRecord([]byte(`{"kind":`)), wantInError: "envelope"},
		{name: "protobuf payload", record: waRecord(waJSON(t, invalidProtoEnvelope)), wantInError: "protobuf"},
		{name: "unknown frame kind", record: waRecord(waJSON(t, whatsAppFrameEnvelope{Kind: "future"})), wantInError: "unsupported"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := NewWhatsAppDecoder().Decode(context.Background(), test.record)
			if err == nil {
				t.Fatal("Decode() error = nil, want malformed-frame error")
			}
			if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(test.wantInError)) {
				t.Fatalf("Decode() error = %q, want substring %q", err, test.wantInError)
			}
		})
	}
}

type whatsAppWorkerHarness struct {
	path      string
	store     *sqlite.Store
	messages  *sqlite.MessageRepository
	reactions *sqlite.ReactionRepository
	counters  *Counters
	worker    *Worker
	sink      *Sink
}

func newWhatsAppWorkerHarness(t *testing.T) *whatsAppWorkerHarness {
	t.Helper()
	path := filepath.Join(t.TempDir(), "whatsapp-ingest.sqlite3")
	store, err := sqlite.Open(path)
	if err != nil {
		t.Fatalf("sqlite.Open(): %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Store.Close(): %v", err)
		}
	})
	if err := store.UpsertAccount(sqlite.Account{
		AccountID:   waWorkerAccountID,
		BridgeKey:   "whatsmeow",
		DisplayName: "WhatsApp ingest decoder test",
		Mode:        sqlite.AccountModeLive,
		Enabled:     true,
		ConfigJSON:  `{}`,
		CreatedAtMS: waWorkerNow.UnixMilli(),
		UpdatedAtMS: waWorkerNow.UnixMilli(),
	}); err != nil {
		t.Fatalf("UpsertAccount(): %v", err)
	}
	now := func() time.Time { return waWorkerNow }
	messages, err := sqlite.NewMessageRepository(store, now)
	if err != nil {
		t.Fatalf("NewMessageRepository(): %v", err)
	}
	return newWhatsAppWorkerHarnessForStore(t, path, store, messages)
}

func newWhatsAppWorkerHarnessForStore(t *testing.T, path string, store *sqlite.Store, messages *sqlite.MessageRepository) *whatsAppWorkerHarness {
	t.Helper()
	now := func() time.Time { return waWorkerNow }
	reactions, err := sqlite.NewReactionRepository(store, now)
	if err != nil {
		t.Fatalf("NewReactionRepository(): %v", err)
	}
	counters := &Counters{}
	worker, err := NewWorker(WorkerConfig{
		Store:     store,
		Messages:  messages,
		Reactions: reactions,
		Counters:  counters,
		Logger:    zerolog.Nop(),
		Now:       now,
		Decoders:  []DecoderRegistration{NewWhatsAppDecoderRegistration()},
	})
	if err != nil {
		t.Fatalf("NewWorker(): %v", err)
	}
	sink := i01NewSink(t, messages, worker, counters, "whatsapp-inbox")
	i01StartWorker(t, worker)
	return &whatsAppWorkerHarness{
		path:      path,
		store:     store,
		messages:  messages,
		reactions: reactions,
		counters:  counters,
		worker:    worker,
		sink:      sink,
	}
}

func waBaseMessageEnvelope() whatsAppFrameEnvelope {
	return whatsAppFrameEnvelope{
		Kind:        whatsAppFrameMessage,
		ChatJID:     waCanonicalChatJID,
		SenderJID:   waSenderJID,
		MessageID:   "message",
		TimestampMS: waTimestampMS,
		PushName:    "Grace Hopper",
	}
}

func waMessageRecord(
	t *testing.T,
	envelope whatsAppFrameEnvelope,
	message *waE2E.Message,
) (bridge.RawIngressRecord, []byte) {
	t.Helper()
	protoBytes := waProto(t, message)
	envelope.Kind = whatsAppFrameMessage
	envelope.ProtoB64 = protoBytes
	return waRecord(waJSON(t, envelope)), protoBytes
}

func waHistoryRecord(t *testing.T, history *waHistorySync.HistorySync) bridge.RawIngressRecord {
	t.Helper()
	return waRecord(waJSON(t, whatsAppFrameEnvelope{
		Kind:     whatsAppFrameHistorySync,
		ProtoB64: waProto(t, history),
	}))
}

func waRecord(payload []byte) bridge.RawIngressRecord {
	return bridge.RawIngressRecord{
		AccountID:    waDecoderAccountID,
		Generation:   7,
		Codec:        WhatsAppCodec,
		CodecVersion: WhatsAppCodecVersion,
		ReceivedAt:   time.UnixMilli(waTimestampMS),
		Payload:      payload,
	}
}

func waJSON(t *testing.T, value any) []byte {
	t.Helper()
	payload, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("json.Marshal(%T): %v", value, err)
	}
	return payload
}

func waProto(t *testing.T, value proto.Message) []byte {
	t.Helper()
	payload, err := proto.Marshal(value)
	if err != nil {
		t.Fatalf("proto.Marshal(%T): %v", value, err)
	}
	return payload
}

func waHash8(payload []byte) string {
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:4])
}

func waHistoryFixture() *waHistorySync.HistorySync {
	message := func(
		id string,
		body string,
		timestamp uint64,
		fromMe bool,
		participant string,
	) *waHistorySync.HistorySyncMsg {
		return &waHistorySync.HistorySyncMsg{Message: &waWeb.WebMessageInfo{
			Key: &waCommon.MessageKey{
				RemoteJID: proto.String(waCanonicalChatJID),
				FromMe:    proto.Bool(fromMe),
				ID:        proto.String(id),
			},
			Message:          &waE2E.Message{Conversation: proto.String(body)},
			MessageTimestamp: proto.Uint64(timestamp),
			Participant:      proto.String(participant),
			PushName:         proto.String("Historical Sender"),
		}}
	}
	return &waHistorySync.HistorySync{
		SyncType: waHistorySync.HistorySync_INITIAL_BOOTSTRAP.Enum(),
		Conversations: []*waHistorySync.Conversation{{
			ID: proto.String(waCanonicalChatJID),
			Messages: []*waHistorySync.HistorySyncMsg{
				message("history-first", "first historical body", 1_700_000_001, false, waSenderJID),
				message("history-second", "second historical body", 1_700_000_002, true, "14155550999@s.whatsapp.net"),
			},
		}},
	}
}
