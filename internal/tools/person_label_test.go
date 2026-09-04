package tools

import (
	"testing"

	"github.com/maxghenis/openmessage/internal/db"
)

func TestPersonConversationLabel(t *testing.T) {
	const roster = `[{"name":"Me","number":"+15550009999","is_me":true},{"name":"Alice","number":"+15550000001"},{"name":"","number":"+15550000002"},{"name":"Carol","number":"+15550000003"},{"name":"Dan","number":"+15550000004"}]`
	for name, tc := range map[string]struct {
		conv *db.Conversation
		want string
	}{
		"nil":                 {conv: nil, want: ""},
		"title wins":          {conv: &db.Conversation{ConversationID: "c1", Name: " Titled ", IsGroup: true, Participants: roster}, want: "Titled"},
		"direct: peer name":   {conv: &db.Conversation{ConversationID: "c2", Participants: `[{"name":"Me","number":"+15550009999","is_me":true},{"name":"Alice","number":"+15550000001"}]`}, want: "Alice"},
		"direct: peer number": {conv: &db.Conversation{ConversationID: "c3", Participants: `[{"name":"","number":"+15550000002"},{"name":"Me","number":"+15550009999","is_me":true}]`}, want: "+15550000002"},
		"direct: only self":   {conv: &db.Conversation{ConversationID: "c4", Participants: `[{"name":"Me","number":"+15550009999","is_me":true}]`}, want: "c4"},
		"direct: no roster":   {conv: &db.Conversation{ConversationID: "c5", Participants: ""}, want: "c5"},
		// A titleless group is never presented as one member's 1:1 thread.
		"group: members":   {conv: &db.Conversation{ConversationID: "g1", IsGroup: true, Participants: roster}, want: "Group: Alice, +15550000002, Carol +1 more"},
		"group: no roster": {conv: &db.Conversation{ConversationID: "g2", IsGroup: true, Participants: `[]`}, want: "Group g2"},
		"group: only self": {conv: &db.Conversation{ConversationID: "g3", IsGroup: true, Participants: `[{"name":"Me","is_me":true}]`}, want: "Group g3"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := personConversationLabel(tc.conv); got != tc.want {
				t.Fatalf("label = %q, want %q", got, tc.want)
			}
		})
	}
}
