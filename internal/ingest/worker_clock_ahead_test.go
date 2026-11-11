package ingest

import (
	"testing"
	"time"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/v2keys"
)

// Regression for issue #169: a message stamped ahead of the local clock mints
// its conversation with created_at_ms at that later time, and a snapshot
// processed afterwards stamped updated_at_ms with the local time, below it.
// The CHECK refused the write and the worker quarantined the snapshot, so its
// title and roster never landed. UpsertConversation now keeps the larger
// updated_at_ms, and the snapshot applies.
func TestWorkerAppliesASnapshotAfterAMessageStampedAheadOfTheLocalClock(t *testing.T) {
	const remoteID = "issue-169"
	ahead := time.UnixMilli(workerPathNowMS).Add(time.Hour)
	contact := bridge.IdentityRef{Raw: "+1 (555) 123-4567", Name: "Phone clock ahead"}
	harness := newWorkerPathHarness(t, bridge.PlatformGoogle, map[string][]bridge.Event{
		"message": {{
			Kind: bridge.EventMessage,
			Message: &bridge.MessageEvent{
				RemoteConversationID: remoteID,
				RemoteMessageID:      "stamped-ahead",
				Sender:               contact,
				Direction:            "incoming",
				Body:                 "sent from a clock an hour ahead",
				OccurredAt:           ahead,
			},
		}},
		"snapshot": {{
			Kind: bridge.EventConversation,
			Conversation: &bridge.ConversationEvent{
				RemoteConversationID: remoteID,
				Kind:                 "direct",
				Title:                "Title from the snapshot",
				RemoteRevision:       "revision-1",
				Participants: []bridge.Participant{{
					Identity: contact,
					Role:     "member",
					Active:   true,
				}},
			},
		}},
	})

	// process fails the test on any apply error, a quarantine included.
	harness.process(t, "message")
	harness.process(t, "snapshot")

	conversation, err := harness.store.GetConversationByRemote(
		workerPathAccountID,
		v2keys.NormalizeRemoteConversationID(string(bridge.PlatformGoogle), remoteID),
	)
	if err != nil {
		t.Fatalf("GetConversationByRemote(): %v", err)
	}
	if conversation.Title != "Title from the snapshot" {
		t.Fatalf("conversation title = %q, want the snapshot's", conversation.Title)
	}
	if conversation.CreatedAtMS != ahead.UnixMilli() || conversation.UpdatedAtMS < conversation.CreatedAtMS {
		t.Fatalf(
			"conversation created_at_ms = %d, updated_at_ms = %d; want created at the message's time %d and updated no earlier",
			conversation.CreatedAtMS,
			conversation.UpdatedAtMS,
			ahead.UnixMilli(),
		)
	}
}
