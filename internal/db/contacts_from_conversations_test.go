package db

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"sort"
	"strconv"
	"strings"
	"testing"
	"testing/quick"
)

// conversationContactKeys renders contacts as ID/name/number triples, for
// comparison.
func conversationContactKeys(contacts []*Contact) []string {
	keys := []string{}
	for _, contact := range contacts {
		keys = append(keys, contact.ContactID+"/"+contact.Name+"/"+contact.Number)
	}
	return keys
}

// Most threads list the account's owner, so a name the owner shares, here
// Sam, appears in all of them. ListContactsFromConversations reads threads
// newest first until it has limit contacts, so the 40 newer threads that
// list only Lee and the owner can't hide Sam Example's older thread. It
// never returns an entry flagged as the owner's, whether by is_me or isMe.
func TestListContactsFromConversationsReadsPastThreadsThatListOnlyTheOwner(t *testing.T) {
	store := newTestStore(t)
	conversations := []*Conversation{{
		ConversationID: "sam", Name: "Sam Example", LastMessageTS: 1000, SourcePlatform: "sms",
		Participants: `[{"name":"Sam Example","number":"+15555550123"},{"name":"Sam Owner","number":"+15550009999","is_me":true}]`,
	}}
	for index := 0; index < 40; index++ {
		flag := "is_me"
		if index%2 == 1 {
			flag = "isMe"
		}
		conversations = append(conversations, &Conversation{
			ConversationID: fmt.Sprintf("lee-%d", index), Name: fmt.Sprintf("Lee %d", index),
			LastMessageTS: int64(2000 + index), SourcePlatform: "sms",
			Participants: fmt.Sprintf(`[{"name":"Lee %d","number":"+1555555%04d"},{"name":"Sam Owner","number":"+15550009999","%s":true}]`, index, 1000+index, flag),
		})
	}
	seedSearchStore(t, store, conversations, nil, nil)

	got, err := store.ListContactsFromConversations("Sam", 1)
	if err != nil {
		t.Fatal(err)
	}
	if keys := conversationContactKeys(got); fmt.Sprint(keys) != "[sam/Sam Example/+15555550123]" {
		t.Errorf("contacts for %q, limit 1 = %v, want Sam Example's entry", "Sam", keys)
	}
	for _, query := range []string{"Sam Owner", "Owner", "+15550009999"} {
		got, err := store.ListContactsFromConversations(query, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Errorf("contacts for the owner's %q = %v, want none", query, conversationContactKeys(got))
		}
	}
}

// A thread whose participants don't decode offers its title instead, and
// those titles count toward the limit like any other contact. A limit below
// one returns nothing.
func TestListContactsFromConversationsKeepsToTheLimitWhenParticipantsDontDecode(t *testing.T) {
	store := newTestStore(t)
	var conversations []*Conversation
	for index, participants := range []string{"", "not json", `["Bob Example"]`} {
		conversations = append(conversations, &Conversation{
			ConversationID: fmt.Sprintf("group-%d", index), Name: fmt.Sprintf("Group %d", index),
			LastMessageTS: int64(100 + index), SourcePlatform: "sms", Participants: participants,
		})
	}
	seedSearchStore(t, store, conversations, nil, nil)

	got, err := store.ListContactsFromConversations("Group", 2)
	if err != nil {
		t.Fatal(err)
	}
	if keys := conversationContactKeys(got); fmt.Sprint(keys) != "[group-2/Group 2/ group-1/Group 1/]" {
		t.Errorf("contacts for %q, limit 2 = %v, want the two newest titles", "Group", keys)
	}
	for _, limit := range []int{0, -1} {
		got, err := store.ListContactsFromConversations("Group", limit)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Errorf("contacts for %q, limit %d = %v, want none", "Group", limit, conversationContactKeys(got))
		}
	}
}

// conversationContactsModel is a reference for ListContactsFromConversations.
// Threads go newest first. A thread whose participants don't decode as a list
// of entries offers its title; otherwise each entry not flagged as the
// owner's (is_me or isMe) offers its name, or its number when it has no name,
// together with its number. An offer counts when the query, ASCII case aside,
// appears in the name offered or in the number, and only the first offer of
// each name and number counts. The result is the first limit offers.
func conversationContactsModel(conversations []*Conversation, query string, limit int) []*Contact {
	holds := func(text string) bool {
		return strings.Contains(strings.ToLower(text), strings.ToLower(query))
	}
	ordered := append([]*Conversation(nil), conversations...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].LastMessageTS > ordered[j].LastMessageTS })
	seen := map[string]bool{}
	var offers []*Contact
	for _, conversation := range ordered {
		var entries []struct {
			Name      string `json:"name"`
			Number    string `json:"number"`
			IsMe      bool   `json:"is_me"`
			IsMeCamel bool   `json:"isMe"`
		}
		if json.Unmarshal([]byte(conversation.Participants), &entries) != nil {
			if conversation.Name != "" && !seen[conversation.Name] && holds(conversation.Name) {
				seen[conversation.Name] = true
				offers = append(offers, &Contact{ContactID: conversation.ConversationID, Name: conversation.Name})
			}
			continue
		}
		for _, entry := range entries {
			name := entry.Name
			if name == "" {
				name = entry.Number
			}
			key := name + "|" + entry.Number
			if entry.IsMe || entry.IsMeCamel || name == "" || seen[key] || !(holds(name) || holds(entry.Number)) {
				continue
			}
			seen[key] = true
			offers = append(offers, &Contact{ContactID: conversation.ConversationID, Name: name, Number: entry.Number})
		}
	}
	if len(offers) > limit {
		offers = offers[:limit]
	}
	return offers
}

// Differential, over random stores: ListContactsFromConversations returns
// what conversationContactsModel returns. Up to 90 of the newest threads list
// only Lee and the owner, flagged by is_me or isMe, so they can outnumber 20
// times the limit; the older threads list people in shared threads, entries
// without a name, the owner or not, and some have participants that don't
// decode. So, as invariants, no limit hides an older match behind threads
// that match only through the owner, the result never holds the owner's
// flagged entries, and each name and number comes back once.
func TestListContactsFromConversationsAgreesWithTheModel(t *testing.T) {
	people := []struct{ name, number string }{
		{"Sam Example", "+15555550100"},
		{"Bob Example", "+15555550101"},
		{"", "+15555550102"},
		{"Carol Sammy", "+15555550103"},
	}
	exercised := map[string]int{}

	property := func(seed int64) bool {
		random := rand.New(rand.NewSource(seed))
		store := newTestStore(t)
		count := 2 + random.Intn(7)
		decoys := random.Intn(91)
		for index := 0; index < count+decoys; index++ {
			id := "c" + strconv.Itoa(index)
			var entries []map[string]any
			title := "Lee " + id
			lastMessageTS := int64(3_000_000 + index)
			if index < count {
				for _, member := range random.Perm(len(people))[:1+random.Intn(3)] {
					entries = append(entries, map[string]any{"name": people[member].name, "number": people[member].number})
				}
				title = []string{"Group " + id, "Sam's people", people[0].name}[random.Intn(3)]
				lastMessageTS = int64(1_000_000 + 1000*random.Intn(1000) + index)
			} else {
				entries = append(entries, map[string]any{"name": title, "number": fmt.Sprintf("+1555555%04d", 1000+index)})
			}
			switch random.Intn(3) {
			case 0:
				entries = append(entries, map[string]any{"name": "Sam Owner", "number": "+15550009999", "is_me": true})
			case 1:
				entries = append(entries, map[string]any{"name": "Me", "number": "+15550009999", "isMe": true})
			}
			random.Shuffle(len(entries), func(i, j int) { entries[i], entries[j] = entries[j], entries[i] })
			participants, err := json.Marshal(entries)
			if err != nil {
				t.Fatal(err)
			}
			if index < count && random.Intn(6) == 0 {
				participants = []byte(`["Sam Example","Bob Example"]`)
			}
			if err := store.UpsertConversation(&Conversation{
				ConversationID: id, Name: title, IsGroup: len(entries) > 2, Participants: string(participants),
				LastMessageTS: lastMessageTS, SourcePlatform: "sms",
			}); err != nil {
				t.Fatalf("seed conversation: %v", err)
			}
		}
		stored, err := store.ListConversations(1000)
		if err != nil {
			t.Fatalf("list conversations: %v", err)
		}

		for _, query := range []string{"", "Sam", "Owner", "Sam Owner", "Me", "Example", "555", "+1555555", "Lee", "Carol", "Group", "zzz"} {
			limit := 1 + random.Intn(4)
			want := conversationContactKeys(conversationContactsModel(stored, query, limit))
			contacts, err := store.ListContactsFromConversations(query, limit)
			if err != nil {
				t.Fatalf("list contacts %q: %v", query, err)
			}
			if got := conversationContactKeys(contacts); fmt.Sprint(got) != fmt.Sprint(want) {
				t.Logf("seed %d: contacts for %q (limit %d) = %v, model says %v", seed, query, limit, got, want)
				return false
			}
			if len(want) > 0 {
				exercised["a query with contacts"]++
				// Lee's entries and numbers match these, so for them the
				// newest threads hold matches of their own.
				matchesLee := map[string]bool{"": true, "Lee": true, "555": true, "+1555555": true}[query]
				if !matchesLee && decoys > 20*limit {
					exercised["a match behind more owner-only threads than 20 times the limit"]++
				}
			}
		}
		return true
	}
	cases := 120
	if raceDetectorEnabled {
		cases = 30
	}
	if err := quick.Check(property, &quick.Config{MaxCount: cases, Rand: rand.New(rand.NewSource(20261103))}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"a query with contacts", "a match behind more owner-only threads than 20 times the limit"} {
		if exercised[path] == 0 {
			t.Errorf("the property never drew %s; raise MaxCount (drew %v)", path, exercised)
		}
	}
}
