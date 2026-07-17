package ingest

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/maxghenis/openmessage/internal/bridge"
	legacydb "github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/messaging"
	"github.com/maxghenis/openmessage/internal/migration"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
	"github.com/maxghenis/openmessage/internal/v2keys"
	"github.com/rs/zerolog"
)

const (
	signalDecoderAccountID = "signal-primary"
	signalSelf             = "+15551230000"
)

var signalDecoderReceivedAt = time.Date(2026, time.July, 17, 15, 30, 0, 0, time.UTC)

func TestSignalDecoderGoldenEnvelopes(t *testing.T) {
	const sourceACI = "11111111-2222-3333-4444-555555555555"
	directConversation := "signal:+15551234567"
	rawACIConversation := "signal:" + sourceACI
	groupConversation := "signal-group:group-token"
	directTimestamp := int64(1_700_000_000_123)
	groupTimestamp := int64(1_700_000_001_123)
	editTargetTimestamp := int64(1_700_000_000_123)
	sentTimestamp := int64(1_700_000_002_123)
	reactionTimestamp := int64(1_700_000_003_123)
	targetTimestamp := int64(1_700_000_000_999)

	groupRemoteRef := mustSignalJSON(t, signalDownloadOpaqueV1{
		Version:        1,
		Kind:           "remote",
		AttachmentID:   "att-42",
		ConversationID: groupConversation,
	})
	sentRemoteRef := mustSignalJSON(t, signalDownloadOpaqueV1{
		Version:        1,
		Kind:           "remote",
		AttachmentID:   "sent-att-7",
		ConversationID: groupConversation,
	})
	directSentRemoteRef := mustSignalJSON(t, signalDownloadOpaqueV1{
		Version:        1,
		Kind:           "remote",
		AttachmentID:   "sent-att-direct",
		ConversationID: directConversation,
	})
	directInboundRemoteRef := mustSignalJSON(t, signalDownloadOpaqueV1{
		Version:        1,
		Kind:           "remote",
		AttachmentID:   "inbound-att-direct",
		ConversationID: directConversation,
	})

	tests := []struct {
		name                string
		line                string
		resolvedSource      string
		resolvedDestination string
		want                []bridge.Event
	}{
		{
			name: "inbound direct prefers e164 source and displays body",
			line: `{"account":"+15551230000","envelope":{"source":"11111111-2222-3333-4444-555555555555","sourceNumber":"+15551234567","sourceName":"Taylor","timestamp":1700000000123,"dataMessage":{"timestamp":1700000000123,"message":"  hello from Signal  "}}}`,
			want: []bridge.Event{{
				Kind: bridge.EventMessage,
				Message: &bridge.MessageEvent{
					RemoteConversationID: directConversation,
					RemoteMessageID: v2keys.SignalIncomingSourceID(
						directConversation,
						"+15551234567",
						directTimestamp,
					),
					Sender:      bridge.IdentityRef{Raw: "+15551234567", Name: "Taylor"},
					Direction:   "incoming",
					Body:        "hello from Signal",
					Attachments: []bridge.Attachment{},
					OccurredAt:  time.UnixMilli(directTimestamp),
				},
			}},
		},
		{
			name: "inbound ACI group attachment uses raw id and placeholder",
			line: `{"account":"+15551230000","envelope":{"sourceServiceId":"11111111-2222-3333-4444-555555555555","sourceName":"ACI Taylor","timestamp":1700000001123,"dataMessage":{"timestamp":1700000001123,"groupInfo":{"groupId":"group-token","groupName":"Friends"},"attachments":[{"contentType":"video/mp4","id":"att-42","filename":"clip.mp4"}]}}}`,
			want: []bridge.Event{{
				Kind: bridge.EventMessage,
				Message: &bridge.MessageEvent{
					RemoteConversationID: groupConversation,
					RemoteMessageID: v2keys.SignalIncomingSourceID(
						groupConversation,
						"11111111-2222-3333-4444-555555555555",
						groupTimestamp,
					),
					Sender: bridge.IdentityRef{
						Raw:  "11111111-2222-3333-4444-555555555555",
						Name: "ACI Taylor",
					},
					Direction: "incoming",
					Body:      "[Video]",
					Attachments: []bridge.Attachment{{
						RemoteID:  "att-42",
						RemoteRef: groupRemoteRef,
						Filename:  "clip.mp4",
						MIME:      "video/mp4",
					}},
					OccurredAt: time.UnixMilli(groupTimestamp),
				},
			}},
		},
		{
			name: "inbound direct ACI falls back to raw identity",
			line: `{"account":"+15551230000","envelope":{"sourceServiceId":"11111111-2222-3333-4444-555555555555","sourceName":"ACI Taylor","timestamp":1700000000123,"dataMessage":{"timestamp":1700000000123,"message":"raw fallback"}}}`,
			want: []bridge.Event{{
				Kind: bridge.EventMessage,
				Message: &bridge.MessageEvent{
					RemoteConversationID: rawACIConversation,
					RemoteMessageID: v2keys.SignalIncomingSourceID(
						rawACIConversation,
						sourceACI,
						directTimestamp,
					),
					Sender:      bridge.IdentityRef{Raw: sourceACI, Name: "ACI Taylor"},
					Direction:   "incoming",
					Body:        "raw fallback",
					Attachments: []bridge.Attachment{},
					OccurredAt:  time.UnixMilli(directTimestamp),
				},
			}},
		},
		{
			name:           "inbound direct ACI capture resolution matches legacy ids",
			line:           `{"account":"+15551230000","envelope":{"sourceServiceId":"11111111-2222-3333-4444-555555555555","sourceName":"ACI Taylor","timestamp":1700000000123,"dataMessage":{"timestamp":1700000000123,"message":"resolved convergence","attachments":[{"contentType":"image/png","id":"inbound-att-direct","filename":"resolved-inbound.png"}]}}}`,
			resolvedSource: "+15551234567",
			want: []bridge.Event{{
				Kind: bridge.EventMessage,
				Message: &bridge.MessageEvent{
					RemoteConversationID: directConversation,
					RemoteMessageID: v2keys.SignalIncomingSourceID(
						directConversation,
						"+15551234567",
						directTimestamp,
					),
					Sender:    bridge.IdentityRef{Raw: "+15551234567", Name: "ACI Taylor"},
					Direction: "incoming",
					Body:      "resolved convergence",
					Attachments: []bridge.Attachment{{
						RemoteID:  "inbound-att-direct",
						RemoteRef: directInboundRemoteRef,
						Filename:  "resolved-inbound.png",
						MIME:      "image/png",
					}},
					OccurredAt: time.UnixMilli(directTimestamp),
				},
			}},
		},
		{
			name:           "inbound ACI group attachment uses captured resolution",
			line:           `{"account":"+15551230000","envelope":{"sourceServiceId":"11111111-2222-3333-4444-555555555555","sourceName":"ACI Taylor","timestamp":1700000001123,"dataMessage":{"timestamp":1700000001123,"groupInfo":{"groupId":"group-token","groupName":"Friends"},"attachments":[{"contentType":"video/mp4","id":"att-42","filename":"clip.mp4"}]}}}`,
			resolvedSource: "+15551234567",
			want: []bridge.Event{{
				Kind: bridge.EventMessage,
				Message: &bridge.MessageEvent{
					RemoteConversationID: groupConversation,
					RemoteMessageID: v2keys.SignalIncomingSourceID(
						groupConversation,
						"+15551234567",
						groupTimestamp,
					),
					Sender: bridge.IdentityRef{
						Raw:  "+15551234567",
						Name: "ACI Taylor",
					},
					Direction: "incoming",
					Body:      "[Video]",
					Attachments: []bridge.Attachment{{
						RemoteID:  "att-42",
						RemoteRef: groupRemoteRef,
						Filename:  "clip.mp4",
						MIME:      "video/mp4",
					}},
					OccurredAt: time.UnixMilli(groupTimestamp),
				},
			}},
		},
		{
			name: "inbound reaction",
			line: `{"account":"+15551230000","envelope":{"sourceNumber":"+15551234567","sourceName":"Taylor","timestamp":1700000003123,"dataMessage":{"timestamp":1700000003123,"reaction":{"emoji":"👍","target":{"timestamp":1700000000999,"authorNumber":"+15557654321"},"isRemove":true}}}}`,
			want: []bridge.Event{{
				Kind: bridge.EventReaction,
				Reaction: &bridge.ReactionEvent{
					RemoteConversationID: directConversation,
					TargetRemoteMessageID: v2keys.SignalIncomingSourceID(
						directConversation,
						"+15557654321",
						targetTimestamp,
					),
					Actor:      bridge.IdentityRef{Raw: "+15551234567", Name: "Taylor"},
					Emoji:      "👍",
					Action:     bridge.ReactionRemove,
					OccurredAt: time.UnixMilli(reactionTimestamp),
				},
			}},
		},
		{
			name: "incoming edit targets incoming sha1 key",
			line: `{"account":"+15551230000","envelope":{"sourceNumber":"+15551234567","sourceName":"Taylor","timestamp":1700000004123,"editMessage":{"targetSentTimestamp":1700000000123,"dataMessage":{"timestamp":1700000004123,"message":"edited body"}}}}`,
			want: []bridge.Event{{
				Kind: bridge.EventMessageMutation,
				MessageMutation: &bridge.MessageMutationEvent{
					RemoteConversationID: directConversation,
					RemoteMessageID: v2keys.SignalIncomingSourceID(
						directConversation,
						"+15551234567",
						editTargetTimestamp,
					),
					Kind:       "edit",
					Body:       "edited body",
					OccurredAt: time.UnixMilli(1_700_000_004_123),
				},
			}},
		},
		{
			name: "outgoing group sync uses timestamp key and self sender",
			line: `{"account":"+15551230000","envelope":{"sourceNumber":"+15551230000","timestamp":1700000002123,"syncMessage":{"sentMessage":{"timestamp":1700000002123,"groupInfo":{"groupId":"group-token"},"message":"sent body","attachments":[{"contentType":"image/jpeg","id":"sent-att-7","filename":"photo.jpg"}]}}}}`,
			want: []bridge.Event{{
				Kind: bridge.EventMessage,
				Message: &bridge.MessageEvent{
					RemoteConversationID: groupConversation,
					RemoteMessageID:      "1700000002123",
					Sender:               bridge.IdentityRef{Raw: signalSelf, IsSelf: true},
					Direction:            "outgoing",
					Body:                 "sent body",
					Attachments: []bridge.Attachment{{
						RemoteID:  "sent-att-7",
						RemoteRef: sentRemoteRef,
						Filename:  "photo.jpg",
						MIME:      "image/jpeg",
					}},
					OccurredAt: time.UnixMilli(sentTimestamp),
				},
			}},
		},
		{
			name:                "outgoing direct ACI uses captured destination for conversation and media ref",
			line:                `{"account":"+15551230000","envelope":{"sourceNumber":"+15551230000","timestamp":1700000002123,"syncMessage":{"sentMessage":{"timestamp":1700000002123,"destinationServiceId":"11111111-2222-3333-4444-555555555555","message":"resolved destination","attachments":[{"contentType":"image/png","id":"sent-att-direct","filename":"resolved.png"}]}}}}`,
			resolvedDestination: "+15551234567",
			want: []bridge.Event{{
				Kind: bridge.EventMessage,
				Message: &bridge.MessageEvent{
					RemoteConversationID: directConversation,
					RemoteMessageID:      "1700000002123",
					Sender:               bridge.IdentityRef{Raw: signalSelf, IsSelf: true},
					Direction:            "outgoing",
					Body:                 "resolved destination",
					Attachments: []bridge.Attachment{{
						RemoteID:  "sent-att-direct",
						RemoteRef: directSentRemoteRef,
						Filename:  "resolved.png",
						MIME:      "image/png",
					}},
					OccurredAt: time.UnixMilli(sentTimestamp),
				},
			}},
		},
		{
			name: "outgoing reaction targets timestamp-native self message",
			line: `{"account":"+15551230000","envelope":{"sourceNumber":"+15551230000","timestamp":1700000003123,"syncMessage":{"sentMessage":{"destinationNumber":"+15551234567","reaction":{"emoji":"🔥","targetSentTimestamp":1700000000999,"targetAuthorNumber":"+15551230000"}}}}}`,
			want: []bridge.Event{{
				Kind: bridge.EventReaction,
				Reaction: &bridge.ReactionEvent{
					RemoteConversationID:  directConversation,
					TargetRemoteMessageID: "1700000000999",
					Actor:                 bridge.IdentityRef{Raw: signalSelf, IsSelf: true},
					Emoji:                 "🔥",
					Action:                bridge.ReactionAdd,
					OccurredAt:            time.UnixMilli(reactionTimestamp),
				},
			}},
		},
		{
			name: "result-wrapped inbound envelope",
			line: `{"result":{"account":"+15551230000","envelope":{"sourceNumber":"+15551234567","sourceName":"Taylor","timestamp":1700000000123,"dataMessage":{"timestamp":1700000000123,"message":"wrapped"}}}}`,
			want: []bridge.Event{{
				Kind: bridge.EventMessage,
				Message: &bridge.MessageEvent{
					RemoteConversationID: directConversation,
					RemoteMessageID: v2keys.SignalIncomingSourceID(
						directConversation,
						"+15551234567",
						directTimestamp,
					),
					Sender:      bridge.IdentityRef{Raw: "+15551234567", Name: "Taylor"},
					Direction:   "incoming",
					Body:        "wrapped",
					Attachments: []bridge.Attachment{},
					OccurredAt:  time.UnixMilli(directTimestamp),
				},
			}},
		},
	}

	decoder := &SignalDecoder{}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			record, ephemeral, err := BuildSignalIngress(
				signalDecoderAccountID,
				9,
				signalSelf,
				[]byte(test.line),
				test.resolvedSource,
				test.resolvedDestination,
				signalDecoderReceivedAt,
			)
			if err != nil {
				t.Fatalf("BuildSignalIngress(): %v", err)
			}
			if ephemeral != nil || record == nil {
				t.Fatalf("BuildSignalIngress() = (%v, %+v), want durable record", record, ephemeral)
			}
			got, err := decoder.Decode(context.Background(), *record)
			if err != nil {
				t.Fatalf("Decode(): %v", err)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("Decode() =\n%#v\nwant\n%#v", got, test.want)
			}
		})
	}
}

func TestSignalDecoderSkipsNonProjectingEnvelopes(t *testing.T) {
	tests := []struct {
		name           string
		line           string
		resolvedSource string
		wantEvents     int
	}{
		{
			name: "from-me data",
			line: `{"account":"+15551230000","envelope":{"sourceNumber":"+15551230000","timestamp":1700000000123,"dataMessage":{"timestamp":1700000000123,"message":"duplicate sync path"}}}`,
		},
		{
			name:           "from-me ACI data resolved to account",
			line:           `{"account":"+15551230000","envelope":{"sourceServiceId":"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee","timestamp":1700000000123,"dataMessage":{"timestamp":1700000000123,"message":"duplicate ACI sync path"}}}`,
			resolvedSource: signalSelf,
		},
		{
			name: "receipt only",
			line: `{"account":"+15551230000","envelope":{"sourceNumber":"+15551234567","timestamp":1700000000123,"receiptMessage":{"when":1700000000123}}}`,
		},
		{
			name: "all nil",
			line: `{"account":"+15551230000","envelope":{"sourceNumber":"+15551234567","timestamp":1700000000123}}`,
		},
		{
			name: "missing inbound source",
			line: `{"account":"+15551230000","envelope":{"timestamp":1700000000123,"dataMessage":{"timestamp":1700000000123,"message":"quarantined by legacy"}}}`,
		},
		{
			name: "missing sent target",
			line: `{"account":"+15551230000","envelope":{"sourceNumber":"+15551230000","timestamp":1700000000123,"syncMessage":{"sentMessage":{"timestamp":1700000000123,"message":"no destination"}}}}`,
		},
		{
			name:       "incoming projection control",
			line:       `{"account":"+15551230000","envelope":{"sourceNumber":"+15551234567","timestamp":1700000000123,"dataMessage":{"timestamp":1700000000123,"message":"decoder is active"}}}`,
			wantEvents: 1,
		},
	}
	decoder := NewSignalDecoder()
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			record, ephemeral, err := BuildSignalIngress(
				signalDecoderAccountID,
				1,
				signalSelf,
				[]byte(test.line),
				test.resolvedSource,
				"",
				signalDecoderReceivedAt,
			)
			if err != nil {
				t.Fatalf("BuildSignalIngress(): %v", err)
			}
			if ephemeral != nil || record == nil {
				t.Fatalf("classification = (%v, %+v), want durable non-projecting record", record, ephemeral)
			}
			events, err := decoder.Decode(context.Background(), *record)
			if err != nil {
				t.Fatalf("Decode(): %v", err)
			}
			if len(events) != test.wantEvents {
				t.Fatalf("Decode() = %+v, want %d events", events, test.wantEvents)
			}
		})
	}
}

func TestBuildSignalIngressTypingOnlyIsEphemeral(t *testing.T) {
	line := []byte(`{"account":"+15551230000","envelope":{"sourceNumber":"+15551234567","sourceName":"Taylor","timestamp":1700000000123,"typingMessage":{"action":"started","groupInfo":{"groupId":"typing-group"}}}}`)
	record, ephemeral, err := BuildSignalIngress(
		signalDecoderAccountID,
		77,
		signalSelf,
		line,
		"",
		"",
		signalDecoderReceivedAt,
	)
	if err != nil {
		t.Fatalf("BuildSignalIngress(): %v", err)
	}
	if record != nil || ephemeral == nil || ephemeral.Typing == nil {
		t.Fatalf("classification = (%+v, %+v), want typing-only ephemeral", record, ephemeral)
	}
	if ephemeral.AccountID != signalDecoderAccountID || ephemeral.Generation != 77 ||
		ephemeral.Typing.RemoteConversationID != "signal-group:typing-group" ||
		ephemeral.Typing.Actor.Raw != "+15551234567" ||
		ephemeral.Typing.Actor.Name != "Taylor" || !ephemeral.Typing.Typing ||
		!ephemeral.Typing.ExpiresAt.Equal(signalDecoderReceivedAt.Add(signalTypingLifetime)) {
		t.Fatalf("ephemeral = %+v, want fully mapped typing event", ephemeral)
	}
}

func TestSignalJSONRPCCodecPreservesLineBytesAndDedupeKeys(t *testing.T) {
	line := []byte(`{ "account" : "+15551230000", "envelope" : { "source" : "+15551234567", "timestamp" : 1700000000123 } }`)
	record, ephemeral, err := BuildSignalIngress(
		signalDecoderAccountID,
		3,
		signalSelf,
		line,
		"+15551234567",
		"+15550009999",
		signalDecoderReceivedAt,
	)
	if err != nil {
		t.Fatalf("BuildSignalIngress(): %v", err)
	}
	if ephemeral != nil || record == nil {
		t.Fatalf("classification = (%+v, %+v), want durable", record, ephemeral)
	}
	var frame signalJSONRPCFrame
	if err := json.Unmarshal(record.Payload, &frame); err != nil {
		t.Fatalf("json.Unmarshal(codec frame): %v", err)
	}
	if frame.Account != signalSelf || !reflect.DeepEqual([]byte(frame.Line), line) {
		t.Fatalf("codec frame = account %q line %q, want byte-preserved %q", frame.Account, frame.Line, line)
	}
	if frame.ResolvedSource != "+15551234567" || frame.ResolvedDestination != "+15550009999" {
		t.Fatalf("codec resolutions = (%q, %q), want capture-time values", frame.ResolvedSource, frame.ResolvedDestination)
	}
	if record.DedupeKey != "env:+15551234567:1700000000123" {
		t.Fatalf("DedupeKey = %q, want envelope source/timestamp", record.DedupeKey)
	}
	resultWrapped := []byte(`{"result":{"account":"+15551230000","envelope":{"source":"+15557654321","timestamp":1700000000456}}}`)
	if got := signalDedupeKey(resultWrapped); got != "env:+15557654321:1700000000456" {
		t.Fatalf("signalDedupeKey(result wrapper) = %q, want result envelope key", got)
	}
	aciOnly := []byte(`{"account":"+15551230000","envelope":{"sourceServiceId":"11111111-2222-3333-4444-555555555555","timestamp":1700000000789}}`)
	if got := signalDedupeKey(aciOnly); got != "env:11111111-2222-3333-4444-555555555555:1700000000789" {
		t.Fatalf("signalDedupeKey(ACI-only) = %q, want raw envelope key", got)
	}

	malformed := []byte("not-json with exact bytes")
	sum := sha256.Sum256(malformed)
	wantFallback := "line:" + hex.EncodeToString(sum[:8])
	if got := signalDedupeKey(malformed); got != wantFallback {
		t.Fatalf("signalDedupeKey(malformed) = %q, want %q", got, wantFallback)
	}
	sourceless := []byte(`{"envelope":{"timestamp":1700000000123}}`)
	sourcelessSum := sha256.Sum256(sourceless)
	sourcelessFallback := "line:" + hex.EncodeToString(sourcelessSum[:8])
	if got := signalDedupeKey(sourceless); got != sourcelessFallback {
		t.Fatalf("signalDedupeKey(sourceless) = %q, want %q", got, sourcelessFallback)
	}
}

func TestNewSignalDecoder(t *testing.T) {
	if decoder := NewSignalDecoder(); decoder == nil {
		t.Fatal("NewSignalDecoder() returned nil")
	}
}

func TestSignalWALReplayDedupeThroughRealWorker(t *testing.T) {
	harness := newSignalWorkerHarness(t, "dedupe.sqlite3")
	line := []byte(`{"account":"+15551230000","envelope":{"sourceNumber":"+15551234567","source":"+15551234567","sourceName":"Taylor","timestamp":1700000000123,"dataMessage":{"timestamp":1700000000123,"message":"live then WAL replay"}}}`)
	record := mustBuildSignalRecord(t, line)
	if err := harness.sink.AppendIngress(context.Background(), record); err != nil {
		t.Fatalf("AppendIngress(live): %v", err)
	}
	if err := harness.sink.AppendIngress(context.Background(), record); err != nil {
		t.Fatalf("AppendIngress(WAL replay): %v", err)
	}
	harness.start(t)

	conversationID := v2keys.DeriveID("conversation", signalDecoderAccountID, "signal:+15551234567")
	remoteMessageID := v2keys.SignalIncomingSourceID(
		"signal:+15551234567",
		"+15551234567",
		1_700_000_000_123,
	)
	waitSignalCondition(t, "deduplicated Signal projection", func() bool {
		message, err := harness.messages.GetMessageByRemote(
			context.Background(),
			signalDecoderAccountID,
			conversationID,
			remoteMessageID,
		)
		return err == nil && message.Body == "live then WAL replay" &&
			harness.counters.Snapshot(signalDecoderAccountID).Deduped == 1
	})
	if got := countSignalRows(t, harness.path, "inbox"); got != 1 {
		t.Fatalf("inbox rows = %d, want one exact replay frame", got)
	}
	if got := countSignalRows(t, harness.path, "messages"); got != 1 {
		t.Fatalf("message rows = %d, want one projected message", got)
	}
}

func TestSignalResolutionDriftReplayDedupeThroughRealWorker(t *testing.T) {
	harness := newSignalWorkerHarness(t, "resolution-drift.sqlite3")
	line := []byte(`{"account":"+15551230000","envelope":{"sourceServiceId":"11111111-2222-3333-4444-555555555555","timestamp":1700000000123,"dataMessage":{"timestamp":1700000000123,"message":"stable raw replay"}}}`)
	first, firstEphemeral, err := BuildSignalIngress(
		signalDecoderAccountID,
		1,
		signalSelf,
		line,
		"+15551234567",
		"",
		signalDecoderReceivedAt,
	)
	if err != nil || first == nil || firstEphemeral != nil {
		t.Fatalf("BuildSignalIngress(first resolution) = (%+v, %+v, %v), want durable", first, firstEphemeral, err)
	}
	second, secondEphemeral, err := BuildSignalIngress(
		signalDecoderAccountID,
		1,
		signalSelf,
		line,
		"+15557654321",
		"",
		signalDecoderReceivedAt,
	)
	if err != nil || second == nil || secondEphemeral != nil {
		t.Fatalf("BuildSignalIngress(drifted resolution) = (%+v, %+v, %v), want durable", second, secondEphemeral, err)
	}
	if first.DedupeKey != second.DedupeKey || reflect.DeepEqual(first.Payload, second.Payload) {
		t.Fatalf("drifted records = keys (%q, %q), payload equality %v; want stable raw key and distinct frames", first.DedupeKey, second.DedupeKey, reflect.DeepEqual(first.Payload, second.Payload))
	}
	if err := harness.sink.AppendIngress(context.Background(), *first); err != nil {
		t.Fatalf("AppendIngress(first): %v", err)
	}
	if err := harness.sink.AppendIngress(context.Background(), *second); err != nil {
		t.Fatalf("AppendIngress(drifted replay): %v", err)
	}
	if got := countSignalRows(t, harness.path, "inbox"); got != 1 {
		t.Fatalf("inbox rows = %d, want one raw-keyed frame after resolution drift", got)
	}
}

func TestSignalRecoveryGuardFrameRemainsDurableThroughRealWorker(t *testing.T) {
	harness := newSignalWorkerHarness(t, "recovery-guard.sqlite3")
	harness.start(t)
	line := []byte(`{"account":"+15551230000","envelope":{"timestamp":1700000000123,"dataMessage":{"timestamp":1700000000123,"message":"legacy quarantines missing source"}}}`)
	record := mustBuildSignalRecord(t, line)
	if err := harness.sink.AppendIngress(context.Background(), record); err != nil {
		t.Fatalf("AppendIngress(): %v", err)
	}
	waitSignalCondition(t, "non-projecting recovery frame processing", func() bool {
		unprocessed, err := harness.messages.Unprocessed(context.Background())
		return err == nil && len(unprocessed) == 0
	})
	if got := countSignalRows(t, harness.path, "inbox"); got != 1 {
		t.Fatalf("inbox rows = %d, want one durable recovery-guard frame", got)
	}
	if got := countSignalRows(t, harness.path, "messages"); got != 0 {
		t.Fatalf("message rows = %d, want no projection for missing source", got)
	}
	snapshot := harness.counters.Snapshot(signalDecoderAccountID)
	if snapshot.Appended != 1 || snapshot.DecodedEvents != 0 ||
		snapshot.Projected != 0 || snapshot.Quarantined != 0 {
		t.Fatalf("recovery frame counters = %+v, want durable no-event processing", snapshot)
	}
}

func TestSignalOutgoingAliasConvergesThroughRealDecoderAndWorker(t *testing.T) {
	const (
		remoteConversationID = "signal:+16505550100"
		conversationID       = "migrated-signal-conversation"
		timestamp            = int64(1_700_000_006_000)
		migratedMessageID    = "migrated-signal-message"
	)
	harness := newSignalWorkerHarness(t, "alias.sqlite3")
	seedSignalConversation(t, harness.store, conversationID, remoteConversationID)
	alias := v2keys.SignalLocalAlias(remoteConversationID, timestamp)
	seedSignalMessage(
		t,
		harness.messages,
		migratedMessageID,
		conversationID,
		alias,
		"migrated body",
		timestamp,
	)
	harness.start(t)

	line := []byte(`{"account":"+15551230000","envelope":{"sourceNumber":"+15551230000","timestamp":1700000006000,"syncMessage":{"sentMessage":{"timestamp":1700000006000,"destinationServiceId":"7a81fd95-20f1-4437-86e2-d5c93ba18851","message":"outgoing sync replay"}}}}`)
	record := mustBuildSignalRecordWithResolutions(t, line, "", "+16505550100")
	if err := harness.sink.AppendIngress(context.Background(), record); err != nil {
		t.Fatalf("AppendIngress(): %v", err)
	}
	waitSignalCondition(t, "Signal outgoing alias convergence", func() bool {
		message, err := harness.messages.GetMessageByRemote(
			context.Background(), signalDecoderAccountID, conversationID, alias,
		)
		return err == nil && message.Body == "outgoing sync replay"
	})

	message, err := harness.messages.GetMessageByRemote(
		context.Background(), signalDecoderAccountID, conversationID, alias,
	)
	if err != nil {
		t.Fatalf("GetMessageByRemote(alias): %v", err)
	}
	if message.MessageID != migratedMessageID || message.RemoteMessageID != alias {
		t.Fatalf("converged message = %+v, want migrated PK and alias", message)
	}
	if _, err := harness.messages.GetMessageByRemote(
		context.Background(), signalDecoderAccountID, conversationID, "1700000006000",
	); !errors.Is(err, sqlite.ErrNotFound) {
		t.Fatalf("timestamp lookup error = %v, want ErrNotFound", err)
	}
	if got := countSignalRows(t, harness.path, "messages"); got != 1 {
		t.Fatalf("message rows = %d, want one converged alias row", got)
	}
}

func TestSignalMigrationOutputConvergesWithLiveDecoder(t *testing.T) {
	const (
		remoteConversationID = "signal:+16505550100"
		timestamp            = int64(1_700_000_006_000)
		legacyMessageID      = "signal:local:053fa0da59a9c2dee79cc0a2b18e4599ad080bf8"
	)
	root := t.TempDir()
	legacyPath := filepath.Join(root, "legacy.sqlite3")
	legacy, err := legacydb.New(legacyPath)
	if err != nil {
		t.Fatalf("legacydb.New(): %v", err)
	}
	if err := legacy.UpsertConversation(&legacydb.Conversation{
		ConversationID: remoteConversationID,
		Name:           "Signal fixture",
		Participants:   `[]`,
		LastMessageTS:  timestamp,
		SourcePlatform: "signal",
	}); err != nil {
		t.Fatalf("UpsertConversation(): %v", err)
	}
	if err := legacy.UpsertMessage(&legacydb.Message{
		MessageID:      legacyMessageID,
		ConversationID: remoteConversationID,
		SenderName:     "Me",
		Body:           "migration output seed",
		TimestampMS:    timestamp,
		Status:         "sent",
		IsFromMe:       true,
		SourcePlatform: "signal",
	}); err != nil {
		t.Fatalf("UpsertMessage(): %v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("legacy.Close(): %v", err)
	}

	stagedPath := filepath.Join(root, "staged.sqlite3")
	report, err := migration.Transform(context.Background(), migration.Options{
		SourcePath:      legacyPath,
		TempStorePath:   stagedPath,
		TempBlobPath:    filepath.Join(root, "staged-blobs"),
		TargetPath:      filepath.Join(root, "canonical"),
		TargetStorePath: filepath.Join(root, "canonical", "store.sqlite3"),
		Check:           true,
	})
	if err != nil {
		t.Fatalf("migration.Transform(): %v (report=%+v)", err, report)
	}
	if !report.OK || report.SignalLocalRows != 1 {
		t.Fatalf("migration report = %+v, want one Signal local row", report)
	}

	store, err := sqlite.Open(stagedPath)
	if err != nil {
		t.Fatalf("sqlite.Open(staged): %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close staged store: %v", err)
		}
	})
	conversation, err := store.GetConversationByRemote(signalDecoderAccountID, remoteConversationID)
	if err != nil {
		t.Fatalf("GetConversationByRemote(migrated): %v", err)
	}
	messages, err := sqlite.NewMessageRepository(store, func() time.Time { return signalDecoderReceivedAt })
	if err != nil {
		t.Fatalf("NewMessageRepository(): %v", err)
	}
	migratedRemoteID, migratedMessageID := migratedSignalMessageKeys(
		t,
		stagedPath,
		conversation.ConversationID,
	)
	if !strings.HasPrefix(migratedRemoteID, "local:") {
		t.Fatalf("migration remote_message_id = %q, want local alias", migratedRemoteID)
	}

	harness := newSignalWorkerHarnessForStore(t, stagedPath, store, messages)
	harness.start(t)
	line := []byte(`{"account":"+15551230000","envelope":{"sourceNumber":"+15551230000","timestamp":1700000006000,"syncMessage":{"sentMessage":{"timestamp":1700000006000,"destinationNumber":"+16505550100","message":"live decoder replay"}}}}`)
	record := mustBuildSignalRecord(t, line)
	if err := harness.sink.AppendIngress(context.Background(), record); err != nil {
		t.Fatalf("AppendIngress(live replay): %v", err)
	}
	waitSignalCondition(t, "migration-to-live convergence", func() bool {
		message, err := messages.GetMessageByRemote(
			context.Background(),
			signalDecoderAccountID,
			conversation.ConversationID,
			migratedRemoteID,
		)
		return err == nil && message.Body == "live decoder replay"
	})

	message, err := messages.GetMessageByRemote(
		context.Background(),
		signalDecoderAccountID,
		conversation.ConversationID,
		migratedRemoteID,
	)
	if err != nil {
		t.Fatalf("GetMessageByRemote(migration alias): %v", err)
	}
	if message.MessageID != migratedMessageID {
		t.Fatalf("message PK = %q, want migration output PK %q", message.MessageID, migratedMessageID)
	}
	if _, err := messages.GetMessageByRemote(
		context.Background(),
		signalDecoderAccountID,
		conversation.ConversationID,
		"1700000006000",
	); !errors.Is(err, sqlite.ErrNotFound) {
		t.Fatalf("timestamp lookup error = %v, want ErrNotFound", err)
	}
	if got := countSignalRows(t, stagedPath, "messages"); got != 1 {
		t.Fatalf("migrated store message rows = %d, want exactly one", got)
	}
}

type signalWorkerHarness struct {
	path     string
	store    *sqlite.Store
	messages *sqlite.MessageRepository
	counters *Counters
	worker   *Worker
	sink     *Sink
	started  bool
}

func newSignalWorkerHarness(t *testing.T, filename string) *signalWorkerHarness {
	t.Helper()
	path := filepath.Join(t.TempDir(), filename)
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
		AccountID:   signalDecoderAccountID,
		BridgeKey:   "signal_cli",
		DisplayName: "Signal",
		Mode:        sqlite.AccountModeLive,
		Enabled:     true,
		ConfigJSON:  `{}`,
		CreatedAtMS: signalDecoderReceivedAt.UnixMilli(),
		UpdatedAtMS: signalDecoderReceivedAt.UnixMilli(),
	}); err != nil {
		t.Fatalf("UpsertAccount(): %v", err)
	}
	messages, err := sqlite.NewMessageRepository(store, func() time.Time { return signalDecoderReceivedAt })
	if err != nil {
		t.Fatalf("NewMessageRepository(): %v", err)
	}
	return newSignalWorkerHarnessForStore(t, path, store, messages)
}

func newSignalWorkerHarnessForStore(
	t *testing.T,
	path string,
	store *sqlite.Store,
	messages *sqlite.MessageRepository,
) *signalWorkerHarness {
	t.Helper()
	counters := &Counters{}
	worker, err := NewWorker(WorkerConfig{
		Store:    store,
		Messages: messages,
		Counters: counters,
		Logger:   zerolog.Nop(),
		Now:      func() time.Time { return signalDecoderReceivedAt },
		Decoders: []DecoderRegistration{{
			Codec:    SignalJSONRPCCodec,
			Platform: bridge.PlatformSignal,
			Decoder:  NewSignalDecoder(),
		}},
	})
	if err != nil {
		t.Fatalf("NewWorker(): %v", err)
	}
	var nextID atomic.Uint64
	sink, err := NewSink(SinkConfig{
		Messages: messages,
		Worker:   worker,
		Counters: counters,
		IDs: messaging.IDSourceFunc(func() (string, error) {
			return fmt.Sprintf("signal-inbox-%04d", nextID.Add(1)), nil
		}),
	})
	if err != nil {
		t.Fatalf("NewSink(): %v", err)
	}
	return &signalWorkerHarness{
		path: path, store: store, messages: messages,
		counters: counters, worker: worker, sink: sink,
	}
}

func (h *signalWorkerHarness) start(t *testing.T) {
	t.Helper()
	if h.started {
		t.Fatal("signal worker harness started twice")
	}
	h.started = true
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.worker.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Worker.Run(): %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("Worker.Run() did not stop")
		}
	})
}

func mustBuildSignalRecord(t *testing.T, line []byte) bridge.RawIngressRecord {
	t.Helper()
	return mustBuildSignalRecordWithResolutions(t, line, "", "")
}

func mustBuildSignalRecordWithResolutions(
	t *testing.T,
	line []byte,
	resolvedSource string,
	resolvedDestination string,
) bridge.RawIngressRecord {
	t.Helper()
	record, ephemeral, err := BuildSignalIngress(
		signalDecoderAccountID,
		1,
		signalSelf,
		line,
		resolvedSource,
		resolvedDestination,
		signalDecoderReceivedAt,
	)
	if err != nil {
		t.Fatalf("BuildSignalIngress(): %v", err)
	}
	if record == nil || ephemeral != nil {
		t.Fatalf("BuildSignalIngress() = (%+v, %+v), want durable", record, ephemeral)
	}
	return *record
}

func mustSignalJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("json.Marshal(): %v", err)
	}
	return raw
}

func seedSignalConversation(
	t *testing.T,
	store *sqlite.Store,
	conversationID string,
	remoteConversationID string,
) {
	t.Helper()
	if err := store.UpsertConversation(sqlite.Conversation{
		ConversationID:       conversationID,
		AccountID:            signalDecoderAccountID,
		RemoteConversationID: remoteConversationID,
		Kind:                 sqlite.ConversationKindDirect,
		Title:                "Signal fixture",
		NotificationMode:     sqlite.NotificationModeAll,
		LastMessageAtMS:      signalDecoderReceivedAt.UnixMilli(),
		MetadataJSON:         `{}`,
		CreatedAtMS:          signalDecoderReceivedAt.UnixMilli(),
		UpdatedAtMS:          signalDecoderReceivedAt.UnixMilli(),
	}); err != nil {
		t.Fatalf("UpsertConversation(): %v", err)
	}
}

func seedSignalMessage(
	t *testing.T,
	messages *sqlite.MessageRepository,
	messageID string,
	conversationID string,
	remoteMessageID string,
	body string,
	occurredAtMS int64,
) {
	t.Helper()
	if err := messages.ImportMessage(context.Background(), sqlite.MessageProjection{
		Message: sqlite.Message{
			MessageID:       messageID,
			ConversationID:  conversationID,
			AccountID:       signalDecoderAccountID,
			RemoteMessageID: remoteMessageID,
			Direction:       sqlite.MessageDirectionOutgoing,
			Body:            body,
			State:           sqlite.MessageStateActive,
			OccurredAtMS:    occurredAtMS,
		},
	}); err != nil {
		t.Fatalf("ImportMessage(): %v", err)
	}
}

func waitSignalCondition(t *testing.T, description string, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ready() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", description)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func countSignalRows(t *testing.T, path, table string) int {
	t.Helper()
	if table != "inbox" && table != "messages" {
		t.Fatalf("unsupported count table %q", table)
	}
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open(%s): %v", table, err)
	}
	defer database.Close()
	var count int
	query := "SELECT COUNT(*) FROM " + table + " WHERE account_id = ?"
	if err := database.QueryRow(query, signalDecoderAccountID).Scan(&count); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return count
}

func migratedSignalMessageKeys(
	t *testing.T,
	path string,
	conversationID string,
) (remoteMessageID string, messageID string) {
	t.Helper()
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open(migrated): %v", err)
	}
	defer database.Close()
	if err := database.QueryRow(`
		SELECT remote_message_id, message_id
		FROM messages
		WHERE account_id = ? AND conversation_id = ?
	`, signalDecoderAccountID, conversationID).Scan(&remoteMessageID, &messageID); err != nil {
		t.Fatalf("query migrated Signal message: %v", err)
	}
	return remoteMessageID, messageID
}
