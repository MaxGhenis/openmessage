package story

import (
	"strings"
	"testing"

	"github.com/maxghenis/openmessage/internal/db"
)

func TestRelationshipSummary_SystemMessagesOnlyIsNoCommunication(t *testing.T) {
	// All RCS system banners — not real conversation.
	msgs := []*db.Message{
		{Body: "RCS chat with Robert", TimestampMS: 1000},
		{Body: "This RCS chat is now end-to-end encrypted", TimestampMS: 2000},
		{Body: "You created this RCS group chat", TimestampMS: 3000},
		{Body: "", TimestampMS: 4000}, // contentless stub
	}
	got := RelationshipSummary(msgs, "Robert", nil)
	if !strings.Contains(strings.ToLower(got), "no communication") {
		t.Errorf("expected a 'no communication' summary, got: %q", got)
	}
}

func TestRelationshipSummary_RealMessagesProduceSummary(t *testing.T) {
	msgs := []*db.Message{
		{Body: "RCS chat with Robert", TimestampMS: 1000}, // filtered out
		{Body: "hey are we still on for friday", IsFromMe: true, TimestampMS: 2000},
		{Body: "yes! see you then", SenderName: "Robert", TimestampMS: 3000},
	}
	got := RelationshipSummary(msgs, "Robert", nil)
	if strings.Contains(strings.ToLower(got), "no communication") {
		t.Errorf("expected a real summary, got no-communication: %q", got)
	}
	if !strings.Contains(got, "2 messages") {
		t.Errorf("expected 2 real messages counted, got: %q", got)
	}
}

func TestFilterRealMessages(t *testing.T) {
	msgs := []*db.Message{
		{Body: "RCS chat with Abby"},
		{Body: "real text"},
		{Body: ""},                 // contentless
		{Body: "", MediaID: "m1"},  // media counts as real
	}
	got := FilterRealMessages(msgs)
	if len(got) != 2 {
		t.Fatalf("FilterRealMessages = %d, want 2 (real text + media)", len(got))
	}
}
