package ingest

import (
	"context"
	"testing"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

// The live regression (#155): a Signal 1:1 thread only ever arrives as message
// frames — the decoder emits no ConversationEvent for it — so the projected
// conversation had zero participants and no title, and read surfaces rendered
// it as a nameless row with an empty roster.
func TestSignalDirectMessageProjectsPeerParticipant(t *testing.T) {
	harness := newSignalWorkerHarness(t, "direct-participants.sqlite3")
	harness.start(t)
	record := mustBuildSignalRecord(t, []byte(
		`{"account":"+15551230000","envelope":{"sourceNumber":"+15551234567","sourceName":"Taylor",`+
			`"timestamp":1700000000123,"dataMessage":{"message":"first message in a new 1:1"}}}`,
	))
	if err := harness.sink.AppendIngress(context.Background(), record); err != nil {
		t.Fatalf("AppendIngress(): %v", err)
	}
	waitSignalCondition(t, "Signal direct message projected", func() bool {
		pending, err := harness.messages.Unprocessed(context.Background())
		return err == nil && len(pending) == 0
	})

	conversation, err := harness.store.GetConversationByRemote(
		signalDecoderAccountID, "signal:+15551234567",
	)
	if err != nil {
		t.Fatalf("GetConversationByRemote(): %v", err)
	}
	if conversation.Kind != sqlite.ConversationKindDirect {
		t.Fatalf("conversation kind = %q, want direct", conversation.Kind)
	}
	participants, err := harness.store.ListParticipants(conversation.ConversationID)
	if err != nil {
		t.Fatalf("ListParticipants(): %v", err)
	}
	if len(participants) != 1 {
		t.Fatalf("participants = %+v, want exactly the remote peer", participants)
	}
	identity, err := harness.store.GetIdentity(participants[0].IdentityID)
	if err != nil {
		t.Fatalf("GetIdentity(): %v", err)
	}
	if identity.CanonicalValue != "+15551234567" {
		t.Fatalf("peer canonical = %q, want %q", identity.CanonicalValue, "+15551234567")
	}
	if identity.IsSelf {
		t.Fatal("peer identity is flagged self")
	}

	// A second message in the same thread must not duplicate the link.
	second := mustBuildSignalRecord(t, []byte(
		`{"account":"+15551230000","envelope":{"sourceNumber":"+15551234567","sourceName":"Taylor",`+
			`"timestamp":1700000000456,"dataMessage":{"message":"second message"}}}`,
	))
	if err := harness.sink.AppendIngress(context.Background(), second); err != nil {
		t.Fatalf("AppendIngress() second: %v", err)
	}
	waitSignalCondition(t, "second Signal direct message projected", func() bool {
		pending, err := harness.messages.Unprocessed(context.Background())
		return err == nil && len(pending) == 0
	})
	participants, err = harness.store.ListParticipants(conversation.ConversationID)
	if err != nil {
		t.Fatalf("ListParticipants() after second message: %v", err)
	}
	if len(participants) != 1 {
		t.Fatalf("participants after second message = %+v, want no duplicate", participants)
	}
}

// An outgoing-only thread (sync from another device, or a message we sent first)
// carries no inbound sender, so the peer has to come from the remote
// conversation ID.
func TestSignalOutgoingOnlyThreadProjectsPeerFromRemoteID(t *testing.T) {
	harness := newSignalWorkerHarness(t, "outgoing-participants.sqlite3")
	harness.start(t)
	record := mustBuildSignalRecord(t, []byte(
		`{"account":"+15551230000","envelope":{"sourceNumber":"+15551230000","timestamp":1700000001000,`+
			`"syncMessage":{"sentMessage":{"destinationNumber":"+15559998888","timestamp":1700000001000,`+
			`"message":"I messaged them first"}}}}`,
	))
	if err := harness.sink.AppendIngress(context.Background(), record); err != nil {
		t.Fatalf("AppendIngress(): %v", err)
	}
	waitSignalCondition(t, "Signal outgoing message projected", func() bool {
		pending, err := harness.messages.Unprocessed(context.Background())
		return err == nil && len(pending) == 0
	})

	conversation, err := harness.store.GetConversationByRemote(
		signalDecoderAccountID, "signal:+15559998888",
	)
	if err != nil {
		t.Fatalf("GetConversationByRemote(): %v", err)
	}
	participants, err := harness.store.ListParticipants(conversation.ConversationID)
	if err != nil {
		t.Fatalf("ListParticipants(): %v", err)
	}
	if len(participants) != 1 {
		t.Fatalf("participants = %+v, want the peer derived from the remote ID", participants)
	}
	identity, err := harness.store.GetIdentity(participants[0].IdentityID)
	if err != nil {
		t.Fatalf("GetIdentity(): %v", err)
	}
	if identity.CanonicalValue != "+15559998888" {
		t.Fatalf("peer canonical = %q, want %q", identity.CanonicalValue, "+15559998888")
	}
}

// Group threads get their roster from ConversationEvent frames; the direct-peer
// path must never invent a participant for them.
func TestSignalGroupMessageDoesNotSynthesizeParticipant(t *testing.T) {
	harness := newSignalWorkerHarness(t, "group-participants.sqlite3")
	harness.start(t)
	record := mustBuildSignalRecord(t, []byte(
		`{"account":"+15551230000","envelope":{"sourceNumber":"+15551234567","sourceName":"Taylor",`+
			`"timestamp":1700000002000,"dataMessage":{"message":"group hello",`+
			`"groupInfo":{"groupId":"participant-test-group"}}}}`,
	))
	if err := harness.sink.AppendIngress(context.Background(), record); err != nil {
		t.Fatalf("AppendIngress(): %v", err)
	}
	waitSignalCondition(t, "Signal group message projected", func() bool {
		pending, err := harness.messages.Unprocessed(context.Background())
		return err == nil && len(pending) == 0
	})

	conversation, err := harness.store.GetConversationByRemote(
		signalDecoderAccountID, "signal-group:participant-test-group",
	)
	if err != nil {
		t.Fatalf("GetConversationByRemote(): %v", err)
	}
	if conversation.Kind != sqlite.ConversationKindGroup {
		t.Fatalf("conversation kind = %q, want group", conversation.Kind)
	}
	participants, err := harness.store.ListParticipants(conversation.ConversationID)
	if err != nil {
		t.Fatalf("ListParticipants(): %v", err)
	}
	if len(participants) != 0 {
		t.Fatalf("group participants = %+v, want none synthesized from a message frame", participants)
	}
}

func TestDirectPeerAddressOnlyAcceptsAddressShapedRemoteIDs(t *testing.T) {
	for _, test := range []struct {
		name     string
		platform bridge.Platform
		remoteID string
		want     string
	}{
		{"signal direct", bridge.PlatformSignal, "signal:+15551234567", "+15551234567"},
		{"signal ACI direct", bridge.PlatformSignal,
			"signal:8f31e572-719f-4e4a-a77c-d5f4b8607ce6", "8f31e572-719f-4e4a-a77c-d5f4b8607ce6"},
		{"signal group", bridge.PlatformSignal, "signal-group:AAAA=", ""},
		{"whatsapp direct", bridge.PlatformWhatsApp,
			"whatsapp:15551234567@s.whatsapp.net", "15551234567@s.whatsapp.net"},
		{"whatsapp group", bridge.PlatformWhatsApp, "whatsapp:1203630000@g.us", ""},
		{"whatsapp broadcast", bridge.PlatformWhatsApp, "whatsapp:status@broadcast", ""},
		// Google Messages thread ids name no addressable party: guessing one
		// would attach a fabricated peer to a real thread.
		{"google opaque thread", bridge.PlatformGoogle, "3031", ""},
		{"empty", bridge.PlatformSignal, "   ", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := directPeerAddress(test.platform, test.remoteID); got != test.want {
				t.Fatalf("directPeerAddress(%q, %q) = %q, want %q",
					test.platform, test.remoteID, got, test.want)
			}
		})
	}
}

func TestPlatformForBridgeKeyMapsStoredAccounts(t *testing.T) {
	for _, test := range []struct {
		bridgeKey string
		want      bridge.Platform
	}{
		{"signal_cli", bridge.PlatformSignal},
		{"whatsmeow", bridge.PlatformWhatsApp},
		{"google_messages", bridge.PlatformGoogle},
		{"gchat", bridge.Platform("gchat")},
	} {
		t.Run(test.bridgeKey, func(t *testing.T) {
			if got := platformForBridgeKey(test.bridgeKey); got != test.want {
				t.Fatalf("platformForBridgeKey(%q) = %q, want %q", test.bridgeKey, got, test.want)
			}
		})
	}
}
