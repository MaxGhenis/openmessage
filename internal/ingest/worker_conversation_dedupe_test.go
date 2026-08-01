package ingest

import (
	"testing"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/v2keys"
)

// Regression for the 2026-08-01 live quarantine: Google conversation snapshots
// can list the same participant identity twice (the account's own number
// duplicated in group rosters). The worker must keep the first entry and apply
// the snapshot instead of quarantining it, which permanently dropped those
// conversations' roster and title updates.
func TestWorkerConversationSnapshotDedupesDuplicateParticipants(t *testing.T) {
	const remoteID = "1690"
	harness := newWorkerPathHarness(t, bridge.PlatformGoogle, map[string][]bridge.Event{
		"snapshot": {{
			Kind: bridge.EventConversation,
			Conversation: &bridge.ConversationEvent{
				RemoteConversationID: remoteID,
				Kind:                 "group",
				Title:                "Group with duplicated self",
				RemoteRevision:       "revision-1",
				Participants: []bridge.Participant{
					{
						Identity: bridge.IdentityRef{Raw: "+16506303657", Name: "Self primary"},
						Role:     "member",
						Active:   true,
					},
					{
						Identity: bridge.IdentityRef{Raw: "+15551112222", Name: "Member"},
						Role:     "member",
						Active:   true,
					},
					{
						Identity: bridge.IdentityRef{Raw: "+16506303657", Name: "Self duplicate"},
						Role:     "member",
						Active:   true,
					},
				},
			},
		}},
	})

	// process fails the test on any quarantine, so reaching the assertions
	// already proves the duplicated roster no longer poisons the snapshot.
	harness.process(t, "snapshot")

	normalized := v2keys.NormalizeRemoteConversationID(
		string(bridge.PlatformGoogle),
		remoteID,
	)
	conversation, err := harness.store.GetConversationByRemote(workerPathAccountID, normalized)
	if err != nil {
		t.Fatalf("GetConversationByRemote(): %v", err)
	}
	if conversation.Title != "Group with duplicated self" {
		t.Fatalf("conversation title = %q, want snapshot title applied", conversation.Title)
	}

	participants, err := harness.store.ListParticipants(conversation.ConversationID)
	if err != nil {
		t.Fatalf("ListParticipants(): %v", err)
	}
	if len(participants) != 2 {
		t.Fatalf("participants after dedupe = %d (%+v), want 2", len(participants), participants)
	}

	identities, err := harness.store.ListIdentities(workerPathAccountID)
	if err != nil {
		t.Fatalf("ListIdentities(): %v", err)
	}
	var selfName string
	for _, identity := range identities {
		if identity.CanonicalValue == "+16506303657" {
			selfName = identity.DisplayName
		}
	}
	if selfName != "Self primary" {
		t.Fatalf("deduped identity display name = %q, want first occurrence kept", selfName)
	}
}
