package v2read

import (
	"testing"

	"github.com/maxghenis/openmessage/internal/storage/sqlite"
	"github.com/maxghenis/openmessage/internal/v2keys"
)

// The live regression (#155): before cutover every reader addressed the Signal
// 1:1 thread as "signal:+15550000002". Cutover re-keys the conversation to a
// derived hash, so unless remote IDs still resolve, that stored key reads as an
// empty thread the moment a restart flips the app to v2-primary.
func TestGetMessagesByConversationResolvesLegacySignalRemoteID(t *testing.T) {
	store, messages, source := openSourceTestStore(t)
	seedSourceAccount(t, store, "signal-primary", "signal_cli")
	const remoteID = "signal:+15550000002"
	conversationID := v2keys.DeriveID("conversation", "signal-primary", remoteID)
	seedSourceConversation(t, store, sqlite.Conversation{
		ConversationID:       conversationID,
		AccountID:            "signal-primary",
		RemoteConversationID: remoteID,
		Kind:                 sqlite.ConversationKindDirect,
		NotificationMode:     sqlite.NotificationModeAll,
		LastMessageAtMS:      400,
	})
	importSourceMessage(t, messages, sqlite.Message{
		MessageID:       "message-signal",
		ConversationID:  conversationID,
		AccountID:       "signal-primary",
		RemoteMessageID: "remote-signal",
		Direction:       sqlite.MessageDirectionIncoming,
		Body:            "reachable by legacy id",
		State:           sqlite.MessageStateActive,
		OccurredAtMS:    400,
	})

	byLegacyID, err := source.GetMessagesByConversation(remoteID, 10)
	if err != nil {
		t.Fatalf("GetMessagesByConversation(%q): %v", remoteID, err)
	}
	assertMessageIDs(t, byLegacyID, "message-signal")

	byV2ID, err := source.GetMessagesByConversation(conversationID, 10)
	if err != nil {
		t.Fatalf("GetMessagesByConversation(%q): %v", conversationID, err)
	}
	assertMessageIDs(t, byV2ID, "message-signal")

	conversation, err := source.GetConversation(remoteID)
	if err != nil {
		t.Fatalf("GetConversation(%q): %v", remoteID, err)
	}
	// The DTO always carries the canonical v2 id, whichever key was asked for.
	if conversation.ConversationID != conversationID {
		t.Fatalf("GetConversation(%q).ConversationID = %q, want %q",
			remoteID, conversation.ConversationID, conversationID)
	}
}

func TestResolveConversationIDLeavesUnknownAndAmbiguousKeysUsable(t *testing.T) {
	store, _, source := openSourceTestStore(t)
	seedSourceAccount(t, store, "signal-primary", "signal_cli")

	// An unknown key must come back unchanged so callers keep their existing
	// empty-result / not-found behavior instead of resolving to some other row.
	if got := source.resolveConversationID("signal:+15550009999"); got != "signal:+15550009999" {
		t.Fatalf("unknown key resolved to %q, want it unchanged", got)
	}
	if got := source.resolveConversationID(""); got != "" {
		t.Fatalf("empty key resolved to %q, want empty", got)
	}

	// A v2 primary key that also happens to exist wins over any remote lookup.
	const remoteID = "signal-group:AAAA="
	conversationID := v2keys.DeriveID("conversation", "signal-primary", remoteID)
	seedSourceConversation(t, store, sqlite.Conversation{
		ConversationID:       conversationID,
		AccountID:            "signal-primary",
		RemoteConversationID: remoteID,
		Kind:                 sqlite.ConversationKindGroup,
		NotificationMode:     sqlite.NotificationModeAll,
		LastMessageAtMS:      10,
	})
	if got := source.resolveConversationID(conversationID); got != conversationID {
		t.Fatalf("v2 id resolved to %q, want %q", got, conversationID)
	}
	if got := source.resolveConversationID(remoteID); got != conversationID {
		t.Fatalf("group remote id resolved to %q, want %q", got, conversationID)
	}
}

// When the same remote ID exists under two accounts, resolution must be
// deterministic (most recently active wins) rather than map-iteration luck.
func TestResolveConversationIDPrefersMostRecentAcrossAccounts(t *testing.T) {
	store, _, source := openSourceTestStore(t)
	seedSourceAccount(t, store, "signal-primary", "signal_cli")
	seedSourceAccount(t, store, "google-primary", "google_messages")
	const remoteID = "shared-remote-thread"

	stale := v2keys.DeriveID("conversation", "google-primary", remoteID)
	seedSourceConversation(t, store, sqlite.Conversation{
		ConversationID:       stale,
		AccountID:            "google-primary",
		RemoteConversationID: remoteID,
		Kind:                 sqlite.ConversationKindDirect,
		NotificationMode:     sqlite.NotificationModeAll,
		LastMessageAtMS:      100,
	})
	fresh := v2keys.DeriveID("conversation", "signal-primary", remoteID)
	seedSourceConversation(t, store, sqlite.Conversation{
		ConversationID:       fresh,
		AccountID:            "signal-primary",
		RemoteConversationID: remoteID,
		Kind:                 sqlite.ConversationKindDirect,
		NotificationMode:     sqlite.NotificationModeAll,
		LastMessageAtMS:      900,
	})

	for i := 0; i < 5; i++ {
		if got := source.resolveConversationID(remoteID); got != fresh {
			t.Fatalf("attempt %d resolved to %q, want the most recent %q", i, got, fresh)
		}
	}
}

// Direct conversations carry no title of their own. Legacy named them after the
// remote peer; v2 reads must too, or 1:1 threads list as blank rows that no
// human or agent can pick out (#155).
func TestMapConversationNamesDirectThreadAfterPeer(t *testing.T) {
	store, _, source := openSourceTestStore(t)
	seedSourceAccount(t, store, "signal-primary", "signal_cli")
	const remoteID = "signal:+15550000003"
	conversationID := v2keys.DeriveID("conversation", "signal-primary", remoteID)
	seedSourceConversation(t, store, sqlite.Conversation{
		ConversationID:       conversationID,
		AccountID:            "signal-primary",
		RemoteConversationID: remoteID,
		Kind:                 sqlite.ConversationKindDirect,
		NotificationMode:     sqlite.NotificationModeAll,
		LastMessageAtMS:      500,
	})

	// No participants yet: the pre-fix shape. Name is empty, but reads still work.
	conversation, err := source.GetConversation(conversationID)
	if err != nil {
		t.Fatalf("GetConversation(): %v", err)
	}
	if conversation.Name != "" {
		t.Fatalf("nameless direct conversation Name = %q, want empty", conversation.Name)
	}

	self := sqlite.Identity{
		IdentityID:     "identity-self",
		AccountID:      "signal-primary",
		Kind:           sqlite.IdentityKind("e164"),
		CanonicalValue: "+15550000001",
		RawValue:       "+15550000001",
		DisplayName:    "Me",
		IsSelf:         true,
		MetadataJSON:   `{}`,
		CreatedAtMS:    sourceTestTimeMS,
		UpdatedAtMS:    sourceTestTimeMS,
	}
	peer := sqlite.Identity{
		IdentityID:     "identity-peer",
		AccountID:      "signal-primary",
		Kind:           sqlite.IdentityKind("e164"),
		CanonicalValue: "+15550000003",
		RawValue:       "+15550000003",
		DisplayName:    "Peer Person",
		MetadataJSON:   `{}`,
		CreatedAtMS:    sourceTestTimeMS,
		UpdatedAtMS:    sourceTestTimeMS,
	}
	for _, identity := range []sqlite.Identity{self, peer} {
		if err := store.UpsertIdentity(identity); err != nil {
			t.Fatalf("UpsertIdentity(%q): %v", identity.IdentityID, err)
		}
	}
	// Self first, so a naive "first participant" rule would name the thread "Me".
	if err := store.ReplaceConversationParticipants(conversationID, []sqlite.ConversationParticipant{
		{
			AccountID:      "signal-primary",
			ConversationID: conversationID,
			IdentityID:     self.IdentityID,
			Role:           sqlite.ParticipantRoleMember,
			IsActive:       true,
		},
		{
			AccountID:      "signal-primary",
			ConversationID: conversationID,
			IdentityID:     peer.IdentityID,
			Role:           sqlite.ParticipantRoleMember,
			IsActive:       true,
		},
	}); err != nil {
		t.Fatalf("ReplaceConversationParticipants(): %v", err)
	}

	named, err := source.GetConversation(conversationID)
	if err != nil {
		t.Fatalf("GetConversation() after participants: %v", err)
	}
	if named.Name != "Peer Person" {
		t.Fatalf("direct conversation Name = %q, want %q", named.Name, "Peer Person")
	}
}

// An explicit group title always wins; peer naming is a direct-only fallback.
func TestMapConversationKeepsExplicitTitle(t *testing.T) {
	store, _, source := openSourceTestStore(t)
	seedSourceAccount(t, store, "signal-primary", "signal_cli")
	const remoteID = "signal-group:BBBB="
	conversationID := v2keys.DeriveID("conversation", "signal-primary", remoteID)
	seedSourceConversation(t, store, sqlite.Conversation{
		ConversationID:       conversationID,
		AccountID:            "signal-primary",
		RemoteConversationID: remoteID,
		Kind:                 sqlite.ConversationKindGroup,
		Title:                "Real Group Title",
		NotificationMode:     sqlite.NotificationModeAll,
		LastMessageAtMS:      500,
	})
	conversation, err := source.GetConversation(conversationID)
	if err != nil {
		t.Fatalf("GetConversation(): %v", err)
	}
	if conversation.Name != "Real Group Title" {
		t.Fatalf("group Name = %q, want %q", conversation.Name, "Real Group Title")
	}
}

// Peer naming falls back to the address when the peer has no display name, so a
// thread is at least addressable by number instead of blank.
func TestDirectPeerNameFallsBackToAddress(t *testing.T) {
	if got := directPeerName([]participantInfo{
		{dto: participantDTO{Name: "Me", Number: "+15550000001"}, isSelf: true},
		{dto: participantDTO{Name: "", Number: "+15550000004"}},
	}); got != "+15550000004" {
		t.Fatalf("directPeerName() = %q, want the peer address", got)
	}
	if got := directPeerName([]participantInfo{
		{dto: participantDTO{Name: "Me", Number: "+15550000001"}, isSelf: true},
	}); got != "" {
		t.Fatalf("directPeerName() with only self = %q, want empty", got)
	}
}
