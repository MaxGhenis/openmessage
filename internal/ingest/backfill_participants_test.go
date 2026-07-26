package ingest

import (
	"context"
	"fmt"
	"testing"

	"github.com/maxghenis/openmessage/internal/storage/sqlite"
	"github.com/maxghenis/openmessage/internal/v2keys"
)

// Conversations that predate peer-ensuring carry no roster at all. The daemon
// sweep links each one's peer from its own history, so threads projected before
// the fix stop reading as nameless rows (#155).
func TestBackfillDirectParticipantsLinksPeerFromInboundHistory(t *testing.T) {
	harness := newSignalWorkerHarness(t, "backfill-inbound.sqlite3")
	const remoteID = "signal:+15557770001"
	conversationID := v2keys.DeriveID("conversation", signalDecoderAccountID, remoteID)
	seedBackfillConversation(t, harness, sqlite.Conversation{
		ConversationID:       conversationID,
		AccountID:            signalDecoderAccountID,
		RemoteConversationID: remoteID,
		Kind:                 sqlite.ConversationKindDirect,
		NotificationMode:     sqlite.NotificationModeAll,
		LastMessageAtMS:      1700000000000,
	})
	identity := seedBackfillIdentity(t, harness, "identity-peer-1", "+15557770001", "Sender One")
	if err := harness.messages.ImportMessage(context.Background(), sqlite.MessageProjection{
		Message: sqlite.Message{
			MessageID:        "backfill-message-1",
			ConversationID:   conversationID,
			AccountID:        signalDecoderAccountID,
			RemoteMessageID:  "remote-backfill-1",
			SenderIdentityID: &identity.IdentityID,
			Direction:        sqlite.MessageDirectionIncoming,
			Body:             "from before the fix",
			State:            sqlite.MessageStateActive,
			OccurredAtMS:     1700000000000,
		},
	}); err != nil {
		t.Fatalf("ImportMessage(): %v", err)
	}

	report, err := harness.worker.BackfillDirectParticipants()
	if err != nil {
		t.Fatalf("BackfillDirectParticipants(): %v", err)
	}
	if report.Linked != 1 {
		t.Fatalf("report = %+v, want exactly one linked conversation", report)
	}
	participants, err := harness.store.ListParticipants(conversationID)
	if err != nil {
		t.Fatalf("ListParticipants(): %v", err)
	}
	if len(participants) != 1 || participants[0].IdentityID != identity.IdentityID {
		t.Fatalf("participants = %+v, want the inbound sender linked", participants)
	}

	// Idempotence: the sweep runs on every daemon start.
	second, err := harness.worker.BackfillDirectParticipants()
	if err != nil {
		t.Fatalf("second BackfillDirectParticipants(): %v", err)
	}
	if second.Scanned != 0 || second.Linked != 0 {
		t.Fatalf("second report = %+v, want nothing left to do", second)
	}
}

// A thread with no inbound message at all still has a peer: the one its remote
// conversation ID names.
func TestBackfillDirectParticipantsLinksPeerFromRemoteID(t *testing.T) {
	harness := newSignalWorkerHarness(t, "backfill-remote-id.sqlite3")
	const remoteID = "signal:+15557770002"
	conversationID := v2keys.DeriveID("conversation", signalDecoderAccountID, remoteID)
	seedBackfillConversation(t, harness, sqlite.Conversation{
		ConversationID:       conversationID,
		AccountID:            signalDecoderAccountID,
		RemoteConversationID: remoteID,
		Kind:                 sqlite.ConversationKindDirect,
		NotificationMode:     sqlite.NotificationModeAll,
		LastMessageAtMS:      1700000000000,
	})

	report, err := harness.worker.BackfillDirectParticipants()
	if err != nil {
		t.Fatalf("BackfillDirectParticipants(): %v", err)
	}
	if report.Linked != 1 {
		t.Fatalf("report = %+v, want the remote-ID peer linked", report)
	}
	participants, err := harness.store.ListParticipants(conversationID)
	if err != nil {
		t.Fatalf("ListParticipants(): %v", err)
	}
	if len(participants) != 1 {
		t.Fatalf("participants = %+v, want one", participants)
	}
	identity, err := harness.store.GetIdentity(participants[0].IdentityID)
	if err != nil {
		t.Fatalf("GetIdentity(): %v", err)
	}
	if identity.CanonicalValue != "+15557770002" {
		t.Fatalf("peer canonical = %q, want %q", identity.CanonicalValue, "+15557770002")
	}
}

// The live store holds Signal groups mislabeled as direct (only the Google
// decoder emitted conversation kinds, so every Signal/WhatsApp group minted
// from a message frame defaulted to direct). The sweep must correct the kind
// and NOT invent a peer — a group has no single counterparty, and attaching one
// would fabricate a 1:1 out of whoever spoke first.
func TestBackfillDirectParticipantsFixesMislabeledGroupWithoutInventingPeer(t *testing.T) {
	harness := newSignalWorkerHarness(t, "backfill-mislabeled-group.sqlite3")
	const remoteID = "signal-group:h1wtWjqi48wR9p16z4QkwBDswcy6RgSH88TepsyOT00="
	conversationID := v2keys.DeriveID("conversation", signalDecoderAccountID, remoteID)
	seedBackfillConversation(t, harness, sqlite.Conversation{
		ConversationID:       conversationID,
		AccountID:            signalDecoderAccountID,
		RemoteConversationID: remoteID,
		Kind:                 sqlite.ConversationKindDirect,
		NotificationMode:     sqlite.NotificationModeAll,
		LastMessageAtMS:      1700000000000,
	})
	identity := seedBackfillIdentity(t, harness, "identity-group-speaker", "+15557770003", "Group Speaker")
	if err := harness.messages.ImportMessage(context.Background(), sqlite.MessageProjection{
		Message: sqlite.Message{
			MessageID:        "backfill-group-message",
			ConversationID:   conversationID,
			AccountID:        signalDecoderAccountID,
			RemoteMessageID:  "remote-group-1",
			SenderIdentityID: &identity.IdentityID,
			Direction:        sqlite.MessageDirectionIncoming,
			Body:             "spoke first in the group",
			State:            sqlite.MessageStateActive,
			OccurredAtMS:     1700000000000,
		},
	}); err != nil {
		t.Fatalf("ImportMessage(): %v", err)
	}

	report, err := harness.worker.BackfillDirectParticipants()
	if err != nil {
		t.Fatalf("BackfillDirectParticipants(): %v", err)
	}
	if report.KindsFixed != 1 {
		t.Fatalf("report = %+v, want the mislabeled group kind corrected", report)
	}
	if report.Linked != 0 {
		t.Fatalf("report = %+v, want no peer invented for a group", report)
	}
	conversation, err := harness.store.GetConversation(conversationID)
	if err != nil {
		t.Fatalf("GetConversation(): %v", err)
	}
	if conversation.Kind != sqlite.ConversationKindGroup {
		t.Fatalf("kind = %q, want group", conversation.Kind)
	}
	participants, err := harness.store.ListParticipants(conversationID)
	if err != nil {
		t.Fatalf("ListParticipants(): %v", err)
	}
	if len(participants) != 0 {
		t.Fatalf("participants = %+v, want none for a corrected group", participants)
	}
}

// A Google thread id names no party, so an unattributed Google conversation is
// reported unresolved rather than given a guessed peer.
func TestBackfillDirectParticipantsLeavesOpaqueGoogleThreadsAlone(t *testing.T) {
	harness := newSignalWorkerHarness(t, "backfill-google-opaque.sqlite3")
	if err := harness.store.UpsertAccount(sqlite.Account{
		AccountID:   "google-primary",
		BridgeKey:   "google_messages",
		DisplayName: "Google",
		Mode:        sqlite.AccountModeLive,
		Enabled:     true,
		ConfigJSON:  `{}`,
		CreatedAtMS: signalDecoderReceivedAt.UnixMilli(),
		UpdatedAtMS: signalDecoderReceivedAt.UnixMilli(),
	}); err != nil {
		t.Fatalf("UpsertAccount(): %v", err)
	}
	conversationID := v2keys.DeriveID("conversation", "google-primary", "3031")
	seedBackfillConversation(t, harness, sqlite.Conversation{
		ConversationID:       conversationID,
		AccountID:            "google-primary",
		RemoteConversationID: "3031",
		Kind:                 sqlite.ConversationKindDirect,
		NotificationMode:     sqlite.NotificationModeAll,
		LastMessageAtMS:      1700000000000,
	})

	report, err := harness.worker.BackfillDirectParticipants()
	if err != nil {
		t.Fatalf("BackfillDirectParticipants(): %v", err)
	}
	if report.Linked != 0 || report.Unresolved != 1 || report.KindsFixed != 0 {
		t.Fatalf("report = %+v, want the opaque thread reported unresolved and untouched", report)
	}
	participants, err := harness.store.ListParticipants(conversationID)
	if err != nil {
		t.Fatalf("ListParticipants(): %v", err)
	}
	if len(participants) != 0 {
		t.Fatalf("participants = %+v, want none guessed for an opaque thread id", participants)
	}
}

func seedBackfillConversation(
	t *testing.T,
	harness *signalWorkerHarness,
	conversation sqlite.Conversation,
) {
	t.Helper()
	if conversation.MetadataJSON == "" {
		conversation.MetadataJSON = `{}`
	}
	if conversation.CreatedAtMS == 0 {
		conversation.CreatedAtMS = signalDecoderReceivedAt.UnixMilli()
	}
	if conversation.UpdatedAtMS == 0 {
		conversation.UpdatedAtMS = conversation.CreatedAtMS
	}
	if err := harness.store.UpsertConversation(conversation); err != nil {
		t.Fatalf("UpsertConversation(%q): %v", conversation.ConversationID, err)
	}
}

func seedBackfillIdentity(
	t *testing.T,
	harness *signalWorkerHarness,
	identityID, canonical, displayName string,
) sqlite.Identity {
	t.Helper()
	identity := sqlite.Identity{
		IdentityID:     identityID,
		AccountID:      signalDecoderAccountID,
		Kind:           sqlite.IdentityKind("e164"),
		CanonicalValue: canonical,
		RawValue:       canonical,
		DisplayName:    displayName,
		MetadataJSON:   `{}`,
		CreatedAtMS:    signalDecoderReceivedAt.UnixMilli(),
		UpdatedAtMS:    signalDecoderReceivedAt.UnixMilli(),
	}
	if err := harness.store.UpsertIdentity(identity); err != nil {
		t.Fatalf("UpsertIdentity(%q): %v", identityID, err)
	}
	return identity
}

// Google thread ids are opaque about shape, so a multi-sender thread must not be
// given a single "peer" — that would render a group as a 1:1 with whoever spoke
// most recently. Verified against a real store: every bare Google thread there
// had exactly one inbound sender, so this guard is what keeps the one that
// doesn't from being mislabeled.
func TestBackfillDirectParticipantsSkipsMultiSenderOpaqueThread(t *testing.T) {
	harness := newSignalWorkerHarness(t, "backfill-multi-sender.sqlite3")
	if err := harness.store.UpsertAccount(sqlite.Account{
		AccountID:   "google-primary",
		BridgeKey:   "google_messages",
		DisplayName: "Google",
		Mode:        sqlite.AccountModeLive,
		Enabled:     true,
		ConfigJSON:  `{}`,
		CreatedAtMS: signalDecoderReceivedAt.UnixMilli(),
		UpdatedAtMS: signalDecoderReceivedAt.UnixMilli(),
	}); err != nil {
		t.Fatalf("UpsertAccount(): %v", err)
	}
	conversationID := v2keys.DeriveID("conversation", "google-primary", "4242")
	seedBackfillConversation(t, harness, sqlite.Conversation{
		ConversationID:       conversationID,
		AccountID:            "google-primary",
		RemoteConversationID: "4242",
		Kind:                 sqlite.ConversationKindDirect,
		NotificationMode:     sqlite.NotificationModeAll,
		LastMessageAtMS:      1700000000000,
	})
	for index, sender := range []struct{ id, number, name string }{
		{"identity-rcs-1", "+15558880001", "First Sender"},
		{"identity-rcs-2", "+15558880002", "Second Sender"},
	} {
		identity := sqlite.Identity{
			IdentityID:     sender.id,
			AccountID:      "google-primary",
			Kind:           sqlite.IdentityKind("e164"),
			CanonicalValue: sender.number,
			RawValue:       sender.number,
			DisplayName:    sender.name,
			MetadataJSON:   `{}`,
			CreatedAtMS:    signalDecoderReceivedAt.UnixMilli(),
			UpdatedAtMS:    signalDecoderReceivedAt.UnixMilli(),
		}
		if err := harness.store.UpsertIdentity(identity); err != nil {
			t.Fatalf("UpsertIdentity(%q): %v", sender.id, err)
		}
		if err := harness.messages.ImportMessage(context.Background(), sqlite.MessageProjection{
			Message: sqlite.Message{
				MessageID:        fmt.Sprintf("multi-sender-%d", index),
				ConversationID:   conversationID,
				AccountID:        "google-primary",
				RemoteMessageID:  fmt.Sprintf("remote-multi-%d", index),
				SenderIdentityID: &identity.IdentityID,
				Direction:        sqlite.MessageDirectionIncoming,
				Body:             "group traffic",
				State:            sqlite.MessageStateActive,
				OccurredAtMS:     1700000000000 + int64(index),
			},
		}); err != nil {
			t.Fatalf("ImportMessage(%d): %v", index, err)
		}
	}

	report, err := harness.worker.BackfillDirectParticipants()
	if err != nil {
		t.Fatalf("BackfillDirectParticipants(): %v", err)
	}
	if report.Linked != 0 || report.Unresolved != 1 {
		t.Fatalf("report = %+v, want the multi-sender opaque thread left unresolved", report)
	}
	participants, err := harness.store.ListParticipants(conversationID)
	if err != nil {
		t.Fatalf("ListParticipants(): %v", err)
	}
	if len(participants) != 0 {
		t.Fatalf("participants = %+v, want none picked from a multi-sender thread", participants)
	}
}
