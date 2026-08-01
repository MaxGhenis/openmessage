package ingest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/v2keys"
)

// Regression for the 2026-08-01 live quarantine: Google conversation snapshots
// listed the account's own number twice in three group rosters — once as the
// self entry, once as a plain member (same role, same active state). The
// worker must treat state-agreeing duplicates as benign (keep the first entry,
// OR-merge IsSelf) and apply the snapshot; before the fix the whole snapshot
// quarantined and those conversations' roster/title updates were dropped
// permanently. Duplicates that disagree on membership state remain an error.
func TestWorkerConversationSnapshotDedupesDuplicateParticipants(t *testing.T) {
	const remoteID = "1690"
	events := func(participants []bridge.Participant) []bridge.Event {
		return []bridge.Event{{
			Kind: bridge.EventConversation,
			Conversation: &bridge.ConversationEvent{
				RemoteConversationID: remoteID,
				Kind:                 "group",
				Title:                "Group with duplicated self",
				RemoteRevision:       "revision-1",
				Participants:         participants,
			},
		}}
	}
	member := bridge.Participant{
		Identity: bridge.IdentityRef{Raw: "+15551112222", Name: "Member"},
		Role:     "member",
		Active:   true,
	}
	selfEntry := bridge.Participant{
		Identity: bridge.IdentityRef{Raw: "+16506303657", Name: "Self primary", IsSelf: true},
		Role:     "member",
		Active:   true,
	}
	plainDuplicate := bridge.Participant{
		Identity: bridge.IdentityRef{Raw: "+16506303657", Name: "Self duplicate"},
		Role:     "member",
		Active:   true,
	}

	tests := []struct {
		name         string
		participants []bridge.Participant
	}{
		// The exact live shape: self entry first, plain duplicate later.
		{"self entry first", []bridge.Participant{selfEntry, member, plainDuplicate}},
		// Reversed order must still surface IsSelf on the kept entry.
		{"plain entry first", []bridge.Participant{plainDuplicate, member, selfEntry}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			harness := newWorkerPathHarness(t, bridge.PlatformGoogle, map[string][]bridge.Event{
				"snapshot": events(test.participants),
			})

			// process fails the test on any apply error, so reaching the
			// assertions proves the duplicated roster no longer poisons the
			// snapshot.
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
			for _, row := range participants {
				if row.Role != "member" || !row.IsActive {
					t.Fatalf("participant row = %+v, want role=member active=true", row)
				}
			}

			identities, err := harness.store.ListIdentities(workerPathAccountID)
			if err != nil {
				t.Fatalf("ListIdentities(): %v", err)
			}
			var self *[3]string
			for _, identity := range identities {
				if identity.CanonicalValue == "+16506303657" {
					isSelf := "false"
					if identity.IsSelf {
						isSelf = "true"
					}
					self = &[3]string{identity.DisplayName, isSelf, identity.RawValue}
				}
			}
			if self == nil {
				t.Fatal("deduped identity not found")
			}
			// First occurrence's display name wins; IsSelf is OR-merged, so
			// it must be true regardless of which entry came first.
			wantName := test.participants[0].Identity.Name
			if self[0] != wantName || self[1] != "true" {
				t.Fatalf("deduped identity name=%q isSelf=%s, want name=%q isSelf=true",
					self[0], self[1], wantName)
			}
		})
	}
}

// Duplicates that disagree on membership state (role or active) are ambiguous
// — refusing the snapshot (quarantine) is still the contract there.
func TestWorkerConversationSnapshotConflictingDuplicateStillQuarantines(t *testing.T) {
	departed := bridge.Participant{
		Identity: bridge.IdentityRef{Raw: "+16506303657", Name: "Self departed"},
		Role:     "member",
		Active:   false,
	}
	active := bridge.Participant{
		Identity: bridge.IdentityRef{Raw: "+16506303657", Name: "Self active", IsSelf: true},
		Role:     "member",
		Active:   true,
	}
	harness := newWorkerPathHarness(t, bridge.PlatformGoogle, map[string][]bridge.Event{
		"conflict": {{
			Kind: bridge.EventConversation,
			Conversation: &bridge.ConversationEvent{
				RemoteConversationID: "1691",
				Kind:                 "group",
				Title:                "Conflicting duplicate",
				RemoteRevision:       "revision-1",
				Participants:         []bridge.Participant{departed, active},
			},
		}},
	})

	record := bridge.RawIngressRecord{
		AccountID:    workerPathAccountID,
		Generation:   1,
		DedupeKey:    "conflict-dedupe",
		Codec:        workerPathCodec,
		CodecVersion: 1,
		ReceivedAt:   time.UnixMilli(workerPathNowMS),
		Payload:      []byte("conflict"),
	}
	_, err := harness.worker.processRecordResult(context.Background(), "conflict-inbox", record)
	var quarantined quarantineError
	if !errors.As(err, &quarantined) {
		t.Fatalf("processRecordResult() error = %v, want quarantine classification", err)
	}
}
