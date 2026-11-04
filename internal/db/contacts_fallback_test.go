package db

import (
	"encoding/json"
	"fmt"
	"testing"
)

// The contacts listed in legacy threads match by what a participant entry
// says, not by its JSON: Go writes &, < and > as \u0026, \u003c and \u003e,
// and a quote as \". A name or number matches where it holds the query as
// written, ASCII case aside, as the other searches match: SQLite's lower()
// folds only ASCII letters, so Ángel finds Ángel Ruiz and so does ÁNGEL, but
// ángel doesn't. The owner's flagged entry names no contact.
func TestListContactsFromConversationsMatchesWhatAnEntrySays(t *testing.T) {
	store := newTestStore(t)
	participants, err := json.Marshal([]map[string]any{
		{"name": "R&D Desk", "number": "+15555550188"},
		{"name": "<Ops> Team", "number": "+15555550177"},
		{"name": `Bob "Builder"`, "number": "+15555550166"},
		{"name": "Ángel Ruiz", "number": "+15555550155"},
		{"name": "Me", "number": "+15550009999", "is_me": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	seedSearchStore(t, store, []*Conversation{
		{ConversationID: "lab", Name: "Lab", SourcePlatform: "sms", LastMessageTS: 10, Participants: string(participants)},
	}, nil, nil)
	for query, want := range map[string]string{
		"R&D": "[R&D Desk]", "r&d desk": "[R&D Desk]", "<ops>": "[<Ops> Team]", `"Builder"`: `[Bob "Builder"]`,
		"Ángel": "[Ángel Ruiz]", "ÁNGEL": "[Ángel Ruiz]", "ángel": "[]", "5550188": "[R&D Desk]",
		"Me": "[]", "+15550009999": "[]", `\u0026`: "[]", "u003c": "[]",
	} {
		contacts, err := store.ListContactsFromConversations(query, 10)
		if err != nil {
			t.Fatalf("ListContactsFromConversations(%q): %v", query, err)
		}
		names := []string{}
		for _, contact := range contacts {
			names = append(names, contact.Name)
		}
		if got := fmt.Sprint(names); got != want {
			t.Errorf("ListContactsFromConversations(%q) = %s, want %s", query, got, want)
		}
	}
}
