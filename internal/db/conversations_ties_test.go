package db

import (
	"fmt"
	"testing"
)

// Threads whose last messages share a time come in conversation ID order,
// as v2's searches and the route tool order them, so a limit keeps the same
// threads whichever store serves the reads. The test writes the threads in
// reverse ID order, so the order the store keeps them in is no help.
func TestLegacySearchesBreakTiesByConversationID(t *testing.T) {
	store := newTestStore(t)
	var conversations []*Conversation
	for _, id := range []string{"c", "b", "a"} {
		conversations = append(conversations, &Conversation{
			ConversationID: id, Name: "Example " + id, SourcePlatform: "sms", LastMessageTS: 50,
			Participants: `[{"name":"Pat ` + id + `","number":"+12025550123"}]`,
		})
	}
	seedSearchStore(t, store, conversations, nil, nil)
	for _, limit := range []int{1, 2, 3} {
		want := fmt.Sprint([]string{"a", "b", "c"}[:limit])
		if got := fmt.Sprint(metadataSearchIDs(t, store, "Example", limit)); got != want {
			t.Errorf("SearchConversationsByMetadata(Example, %d) = %s, want %s", limit, got, want)
		}
		byLine, err := store.SearchConversationsByPhoneLine("2025550123", limit)
		if err != nil {
			t.Fatalf("SearchConversationsByPhoneLine(): %v", err)
		}
		ids := []string{}
		for _, conversation := range byLine {
			ids = append(ids, conversation.ConversationID)
		}
		if got := fmt.Sprint(ids); got != want {
			t.Errorf("SearchConversationsByPhoneLine(2025550123, %d) = %s, want %s", limit, got, want)
		}
		contacts, err := store.ListContactsFromConversations("Pat", limit)
		if err != nil {
			t.Fatalf("ListContactsFromConversations(): %v", err)
		}
		names := []string{}
		for _, contact := range contacts {
			names = append(names, contact.ContactID)
		}
		if got := fmt.Sprint(names); got != want {
			t.Errorf("ListContactsFromConversations(Pat, %d) came from threads %s, want %s", limit, got, want)
		}
	}
}
