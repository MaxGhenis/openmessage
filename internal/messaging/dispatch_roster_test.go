package messaging

import (
	"bytes"
	"context"
	"reflect"
	"testing"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

// seedRosterForDispatch gives the test conversation a peer, a departed member
// and this account's own row, and the account a second self identity.
func seedRosterForDispatch(t *testing.T, store *sqlite.Store) {
	t.Helper()
	nowMS := messagingTestTime.UnixMilli()
	identities := []sqlite.Identity{
		{IdentityID: "identity-self", Kind: "e164", CanonicalValue: "+15550100000", IsSelf: true},
		{IdentityID: "identity-self-legacy", Kind: "legacy_participant", CanonicalValue: "legacy-participant:me", IsSelf: true},
		{IdentityID: "identity-peer", Kind: "e164", CanonicalValue: "+15550100001"},
		{IdentityID: "identity-departed", Kind: "e164", CanonicalValue: "+15550100002"},
	}
	for _, identity := range identities {
		identity.AccountID = "account-1"
		identity.RawValue = identity.CanonicalValue
		identity.DisplayName = "Someone"
		identity.MetadataJSON = `{}`
		identity.CreatedAtMS, identity.UpdatedAtMS = nowMS, nowMS
		if err := store.UpsertIdentity(identity); err != nil {
			t.Fatalf("UpsertIdentity(%s): %v", identity.IdentityID, err)
		}
	}
	participant := func(id string, active bool) sqlite.ConversationParticipant {
		return sqlite.ConversationParticipant{
			AccountID: "account-1", ConversationID: "conversation-1", IdentityID: id,
			Role: sqlite.ParticipantRoleMember, IsActive: active,
		}
	}
	if err := store.ReplaceConversationParticipants("conversation-1", []sqlite.ConversationParticipant{
		participant("identity-self", true),
		participant("identity-peer", true),
		participant("identity-departed", false),
	}); err != nil {
		t.Fatalf("ReplaceConversationParticipants(): %v", err)
	}
}

// wantDispatchRosterRef is what the adapter must receive for the seeded
// conversation: its kind, its active peer, and every self identity, as
// natural keys only (no names or raw values).
var wantDispatchRosterRef = bridge.ConversationRef{
	RemoteID: "remote-conversation",
	Kind:     "direct",
	Peers:    []bridge.IdentityRef{{Kind: "e164", Canonical: "+15550100001"}},
	Self: []bridge.IdentityRef{
		{Kind: "e164", Canonical: "+15550100000", IsSelf: true},
		{Kind: "legacy_participant", Canonical: "legacy-participant:me", IsSelf: true},
	},
}

func TestDispatchPassesTheStoredRosterWithText(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	seedRosterForDispatch(t, store)
	sender := &scriptedTextSender{}
	registry := newScriptedRegistry("roster-platform", sender)
	registry.setAvailable(true)
	service := newMessagingTestService(t, store, registry, clock)
	mustSendText(t, service, SendTextCommand{CommonCommand: testCommonCommand("roster-text"), Body: "hello"})

	if processed, err := service.DispatchDue(context.Background(), 1); err != nil || processed != 1 {
		t.Fatalf("DispatchDue() = %d, %v", processed, err)
	}
	requests := sender.snapshotRequests()
	if len(requests) != 1 || !reflect.DeepEqual(requests[0].Conversation, wantDispatchRosterRef) {
		t.Fatalf("text requests = %+v, want one carrying %+v", requests, wantDispatchRosterRef)
	}
}

func TestDispatchPassesTheStoredRosterWithMedia(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	seedRosterForDispatch(t, store)
	blobs := newDispatchTestBlobStore(t)
	sender := &scriptedMediaSender{steps: []sendStep{{result: bridge.SendResult{RemoteMessageID: "remote-media"}}}}
	registry := newScriptedRegistry("roster-media", &scriptedTextSender{})
	registry.setMediaSender(sender)
	registry.setAvailable(true)
	service := newDispatchTestService(t, store, registry, blobs, clock)
	mustSendDispatchMedia(t, service, SendMediaCommand{
		CommonCommand: testCommonCommand("roster-media"),
		Content:       bytes.NewReader([]byte("image")),
		Filename:      "photo.png",
		MIME:          "image/png",
	})

	if processed, err := service.DispatchDue(context.Background(), 1); err != nil || processed != 1 {
		t.Fatalf("DispatchDue() = %d, %v", processed, err)
	}
	requests := sender.snapshotRequests()
	if len(requests) != 1 || !reflect.DeepEqual(requests[0].Conversation, wantDispatchRosterRef) {
		t.Fatalf("media requests = %d, want one carrying %+v", len(requests), wantDispatchRosterRef)
	}
}
