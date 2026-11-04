package db

import (
	"fmt"
	"testing"
)

// A participant entry is a JSON object, and only the text of its name,
// number, phone or email names anyone. Its keys, its flags and its IDs match
// nothing: the demo store names its owner Me, and "me" sits inside every
// "name" key. The search decodes JSON escapes, so R&D matches R\u0026D. A
// bare string entry still matches as written.
func TestSearchConversationsByMetadataMatchesEntryValuesNotKeys(t *testing.T) {
	store := newTestStore(t)
	seedSearchStore(t, store, []*Conversation{
		{ConversationID: "bob", Name: "Bob's thread", SourcePlatform: "sms", LastMessageTS: 40,
			Participants: `[{"name":"Bob Example","number":"+15555550123","id":"7","contact_id":"c9"},{"name":"Me","number":"+15550009999","is_me":true}]`},
		{ConversationID: "lab", Name: "Lab", SourcePlatform: "sms", LastMessageTS: 30,
			Participants: `[{"name":"R\u0026D Desk","phone":"+15555550188"},{"name":"Me","number":"+15550009999","is_me":true}]`},
		{ConversationID: "chat", Name: "Chat", SourcePlatform: "gchat", LastMessageTS: 20,
			Participants: `[{"name":"Dee Example","email":"dee@example.com"}]`},
		{ConversationID: "bare", Name: "Bare", SourcePlatform: "sms", LastMessageTS: 10,
			Participants: `["Carol Example",{"name":"Me","number":"+15550009999","is_me":true}]`},
	}, nil, nil)

	for _, query := range []string{"Me", "me", "name", "number", "phone", "email", "is_me", `":"`, "contact_id", "c9", "u0026"} {
		if got := metadataSearchIDs(t, store, query, 10); len(got) != 0 {
			t.Errorf("search %q = %v, want none: only keys, flags, IDs or the owner's entry hold it", query, got)
		}
	}
	for query, want := range map[string]string{
		"Bob": "[bob]", "+15555550123": "[bob]", "R&D": "[lab]", "+15555550188": "[lab]",
		"dee@example": "[chat]", "Carol": "[bare]", "Example": "[bob chat bare]",
	} {
		if got := fmt.Sprint(metadataSearchIDs(t, store, query, 10)); got != want {
			t.Errorf("search %q = %s, want %s", query, got, want)
		}
	}
}

// % and _ in a query match only themselves in the address book, as in the
// conversation searches.
func TestListContactsMatchesTheQueryLiterally(t *testing.T) {
	store := newTestStore(t)
	seedSearchStore(t, store, nil, nil, []*Contact{
		{ContactID: "anna", Name: "Anna Example", Number: "+12025550123"},
		{ContactID: "sale", Name: "50% Off", Number: "+15555550100"},
		{ContactID: "under", Name: "Under_Score", Number: "+15555550101"},
	})
	for query, want := range map[string]string{
		"%": "[sale]", "_": "[under]", "Ann_": "[]", "A%a": "[]", "50%": "[sale]",
		"r_S": "[under]", "Anna": "[anna]", "5550123": "[anna]", "": "[sale anna under]",
	} {
		contacts, err := store.ListContacts(query, 10)
		if err != nil {
			t.Fatalf("ListContacts(%q): %v", query, err)
		}
		ids := []string{}
		for _, contact := range contacts {
			ids = append(ids, contact.ContactID)
		}
		if got := fmt.Sprint(ids); got != want {
			t.Errorf("ListContacts(%q) = %s, want %s", query, got, want)
		}
	}
}
