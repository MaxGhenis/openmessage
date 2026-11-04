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

// seedSearchStore writes a search fixture into store.
func seedSearchStore(t *testing.T, store *Store, conversations []*Conversation, messages []*Message, contacts []*Contact) {
	t.Helper()
	for _, conversation := range conversations {
		if err := store.UpsertConversation(conversation); err != nil {
			t.Fatalf("seed conversation %s: %v", conversation.ConversationID, err)
		}
	}
	for _, message := range messages {
		if err := store.UpsertMessage(message); err != nil {
			t.Fatalf("seed message %s: %v", message.MessageID, err)
		}
	}
	for _, contact := range contacts {
		if err := store.UpsertContact(contact); err != nil {
			t.Fatalf("seed contact %s: %v", contact.ContactID, err)
		}
	}
}

// metadataSearchIDs returns the IDs SearchConversationsByMetadata finds, in
// its order.
func metadataSearchIDs(t *testing.T, store *Store, query string, limit int) []string {
	t.Helper()
	got, err := store.SearchConversationsByMetadata(query, limit)
	if err != nil {
		t.Fatalf("search %q: %v", query, err)
	}
	ids := []string{}
	for _, conversation := range got {
		ids = append(ids, conversation.ConversationID)
	}
	return ids
}

// The legacy store lists the account's owner in most threads, flagged is_me
// (or isMe), and keeps the owner's name and number on the owner's outgoing
// messages. A thread that matches a query only through those says nothing
// about whom the query names, so the search skips them before its limit
// applies. Here the 30 newest threads list only Lee and the owner, and a
// search for Sam, a name the owner shares, still finds the three older
// threads that match through someone else. Work matches only through an
// incoming message from Sam Guest, whom its roster lacks, or through Sam
// Guest's contact card. Participants that aren't a JSON array match nothing,
// and % and _ match only themselves.
func TestSearchConversationsByMetadataSkipsEntriesFlaggedAsTheOwners(t *testing.T) {
	store := newTestStore(t)
	owner := `{"name":"Sam Owner","number":"+15550009999","is_me":true}`
	conversations := []*Conversation{
		{ConversationID: "sam", Name: "Sam Example", LastMessageTS: 1000, SourcePlatform: "sms",
			Participants: `[{"name":"Sam Example","number":"+15555550123"},` + owner + `]`},
		{ConversationID: "work", Name: "Work", IsGroup: true, LastMessageTS: 900, SourcePlatform: "sms",
			Participants: `[{"name":"Bob Example","number":"+15555550199"},` + owner + `]`},
		{ConversationID: "dana", Name: "Dana Example", LastMessageTS: 800, SourcePlatform: "signal",
			Participants: `[{"name":"Dana Example","phone":"+15555550166"},{"name":"Sam Owner","phone":"+15550009999","isMe":true}]`},
		{ConversationID: "deals", Name: "50% off", IsGroup: true, LastMessageTS: 700, SourcePlatform: "sms",
			Participants: `[{"name":"Ed_Short","number":"56789"},` + owner + `]`},
		{ConversationID: "picnic", Name: "Picnic", IsGroup: true, LastMessageTS: 600, SourcePlatform: "whatsapp",
			Participants: `["Sam Lee","Bob Example"]`},
		{ConversationID: "broken", Name: "Broken", LastMessageTS: 500, SourcePlatform: "sms",
			Participants: `not json: Sam Owner, Sam Lee`},
	}
	for index := 0; index < 30; index++ {
		conversations = append(conversations, &Conversation{
			ConversationID: fmt.Sprintf("lee-%d", index), Name: fmt.Sprintf("Lee %d", index),
			LastMessageTS: int64(2000 + index), SourcePlatform: "sms",
			Participants: fmt.Sprintf(`[{"name":"Lee %d","number":"+1555555%04d"},%s]`, index, 1000+index, owner),
		})
	}
	messages := []*Message{
		{MessageID: "work-guest", ConversationID: "work", SenderName: "Sam Guest", SenderNumber: "+15555550177", Body: "running late", TimestampMS: 900, SourcePlatform: "sms"},
		{MessageID: "work-owner", ConversationID: "work", SenderName: "Sam Owner", SenderNumber: "+15550009999", IsFromMe: true, Body: "no rush", TimestampMS: 899, SourcePlatform: "sms"},
		{MessageID: "dana-owner", ConversationID: "dana", SenderName: "Sam Owner", SenderNumber: "+15550009999", IsFromMe: true, Body: "hi", TimestampMS: 800, SourcePlatform: "signal"},
	}
	contacts := []*Contact{
		{ContactID: "card-owner", Name: "Sam Owner", Number: "+15550009999"},
		{ContactID: "card-guest", Name: "Sammy G", Number: "+15555550177"},
	}
	seedSearchStore(t, store, conversations, messages, contacts)

	for query, want := range map[string][]string{
		"Sam":          {"sam", "work", "picnic"},
		"Sam Example":  {"sam"},
		"Guest":        {"work"},
		"Sammy":        {"work"},
		"+15555550177": {"work"},
		"Bob":          {"work", "picnic"},
		"Dana":         {"dana"},
		"Sam Owner":    {},
		"Owner":        {},
		"+15550009999": {},
		"5550009999":   {},
		"is_me":        {},
		"isMe":         {},
		"%":            {"deals"},
		"_":            {"deals"},
		"Ed_":          {"deals"},
		"E_":           {},
		"S%m":          {},
		"not json":     {},
	} {
		if got := metadataSearchIDs(t, store, query, 10); fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("search %q = %v, want %v", query, got, want)
		}
	}
	if got := metadataSearchIDs(t, store, "Sam", 1); fmt.Sprint(got) != "[sam]" {
		t.Errorf("search %q with limit 1 = %v, want [sam]", "Sam", got)
	}
}

// metadataSearchModel is a reference for the legacy store's
// SearchConversationsByMetadata. A thread matches when the query, as
// written and ASCII case aside, appears in its title, in the decoded text of
// the name, number, phone or email of a participant entry not flagged as the
// owner's, in a bare string entry, or in the name or number of an incoming
// message's sender or of a contact card with that sender's number. An entry
// is flagged when its is_me or isMe holds JSON true. Participants that
// aren't a JSON array offer no entries. Hits come newest first, up to the
// limit.
type metadataSearchModel struct {
	conversations []*Conversation
	messages      []*Message
	contacts      []*Contact
}

// participantEntryTexts returns the texts that each participant entry offers
// the search, and whether the entry is flagged as the owner's. An object
// offers the decoded text of its name, number, phone and email; a string
// offers itself; anything else offers nothing.
func participantEntryTexts(participants string) (texts [][]string, flagged []bool) {
	var elements []json.RawMessage
	if json.Unmarshal([]byte(participants), &elements) != nil {
		return nil, nil
	}
	for _, element := range elements {
		var object map[string]any
		if json.Unmarshal(element, &object) == nil && object != nil {
			var values []string
			for _, key := range []string{"name", "number", "phone", "email"} {
				if value, ok := object[key].(string); ok {
					values = append(values, value)
				}
			}
			texts = append(texts, values)
			flagged = append(flagged, object["is_me"] == true || object["isMe"] == true)
			continue
		}
		var text string
		if json.Unmarshal(element, &text) == nil {
			texts = append(texts, []string{text})
		} else {
			texts = append(texts, nil)
		}
		flagged = append(flagged, false)
	}
	return texts, flagged
}

// match reports how conversation matches query: through someone else (its
// title, an entry not flagged as the owner's, an incoming sender or their
// card), through an incoming sender or card alone, or only through entries
// flagged as the owner's.
func (m metadataSearchModel) match(conversation *Conversation, query string) (hit, senderOnly, ownerOnly bool) {
	holds := func(text string) bool {
		return strings.Contains(strings.ToLower(text), strings.ToLower(query))
	}
	byText := holds(conversation.Name)
	byOwner := false
	texts, flagged := participantEntryTexts(conversation.Participants)
	for index, entry := range texts {
		for _, text := range entry {
			if !holds(text) {
				continue
			}
			if flagged[index] {
				byOwner = true
			} else {
				byText = true
			}
		}
	}
	bySender := false
	for _, message := range m.messages {
		if message.ConversationID != conversation.ConversationID || message.IsFromMe {
			continue
		}
		bySender = bySender || holds(message.SenderName) || holds(message.SenderNumber)
		for _, contact := range m.contacts {
			if contact.Number == message.SenderNumber {
				bySender = bySender || holds(contact.Name) || holds(contact.Number)
			}
		}
	}
	hit = byText || bySender
	return hit, bySender && !byText, byOwner && !hit
}

func (m metadataSearchModel) search(query string, limit int) []string {
	var hits []*Conversation
	for _, conversation := range m.conversations {
		if hit, _, _ := m.match(conversation, query); hit {
			hits = append(hits, conversation)
		}
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].LastMessageTS > hits[j].LastMessageTS })
	ids := []string{}
	for _, conversation := range hits {
		if len(ids) == limit {
			break
		}
		ids = append(ids, conversation.ConversationID)
	}
	return ids
}

// Differential, over random stores: SearchConversationsByMetadata returns
// exactly what metadataSearchModel returns, in the same order. The stores
// list the owner flagged with is_me or isMe, or not at all, beside people
// who share words of the owner's name, and they add up to 15 newer threads
// that list only Lee and the owner. Some entries carry is_me false or the
// string "true", which flag nothing; some participants are empty, malformed
// or a list of bare names; titles and names hold % and _. Messages come from
// the owner, from people in the roster, or from Sam Guest, whom no roster
// lists, and the owner and Sam Guest can have contact cards. So, as
// invariants, no query finds a thread through an entry flagged as the
// owner's or through the owner's own messages; titles, other entries,
// incoming senders and their cards still match, before the limit applies;
// and % and _ match only themselves. The owner always appears flagged here:
// the search makes no claim about an unflagged copy of the owner.
func TestSearchConversationsByMetadataAgreesWithTheFlaggedOwnerModel(t *testing.T) {
	people := []struct{ name, number string }{
		{"Sam Example", "+15555550100"},
		{"Bob Example", "+15555550101"},
		{"Carol Sammy", "+15555550102"},
		{"Ed_Short", "56789"},
		{"Fay 100%", "+15555550104"},
		{"Gus R&D", "+15555550105"},
	}
	guest := struct{ name, number string }{"Sam Guest", "+15555550177"}
	owner := struct{ name, number string }{"Sam Owner", "+15550009999"}
	exercised := map[string]int{}

	property := func(seed int64) bool {
		random := rand.New(rand.NewSource(seed))
		store := newTestStore(t)
		model := metadataSearchModel{}

		count := 3 + random.Intn(6)
		decoys := random.Intn(16)
		for index := 0; index < count+decoys; index++ {
			id := "c" + strconv.Itoa(index)
			var entries []map[string]any
			var title string
			if index < count {
				for _, member := range random.Perm(len(people))[:1+random.Intn(3)] {
					entry := map[string]any{"name": people[member].name, "number": people[member].number}
					switch random.Intn(7) {
					case 0:
						entry["is_me"] = false
					case 1:
						entry["is_me"] = "true"
					case 2:
						entry["id"] = strconv.Itoa(member)
					case 3:
						entry["contact_id"] = "card-" + strconv.Itoa(member)
						entry["email"] = strings.ToLower(strings.Fields(people[member].name)[0]) + "@example.com"
					}
					entries = append(entries, entry)
				}
				title = entries[0]["name"].(string)
				if random.Intn(3) == 0 {
					title = []string{"Group " + id, "Sam's people", "100% club", "Book_club"}[random.Intn(4)]
				}
			} else {
				title = "Lee " + id
				entries = append(entries, map[string]any{"name": title, "number": fmt.Sprintf("+1555555%04d", 1000+index)})
			}
			switch random.Intn(4) {
			case 0: // the owner isn't listed
			case 1, 2:
				entries = append(entries, map[string]any{"name": owner.name, "number": owner.number, "is_me": true})
			default:
				entries = append(entries, map[string]any{"name": "Me", "phone": owner.number, "isMe": true})
			}
			random.Shuffle(len(entries), func(i, j int) { entries[i], entries[j] = entries[j], entries[i] })
			participants, err := json.Marshal(entries)
			if err != nil {
				t.Fatal(err)
			}
			if index < count {
				switch random.Intn(10) {
				case 0:
					participants = []byte("")
				case 1:
					participants = []byte("not json: " + owner.name)
				case 2:
					participants = []byte(`["` + owner.name + `","Bob Example"]`)
				}
			}
			lastMessageTS := int64(1_000_000 + 1000*random.Intn(1000) + index)
			if index >= count {
				lastMessageTS = int64(3_000_000 + index)
			}
			if err := store.UpsertConversation(&Conversation{
				ConversationID: id, Name: title, IsGroup: len(entries) > 2, Participants: string(participants),
				LastMessageTS: lastMessageTS, SourcePlatform: "sms",
			}); err != nil {
				t.Fatalf("seed conversation: %v", err)
			}
			for number := 0; number < random.Intn(3); number++ {
				message := &Message{
					MessageID: id + "-m" + strconv.Itoa(number), ConversationID: id, Body: "hello",
					TimestampMS: int64(1000 + number), SourcePlatform: "sms",
				}
				switch random.Intn(3) {
				case 0:
					message.IsFromMe, message.SenderName, message.SenderNumber = true, owner.name, owner.number
				case 1:
					message.SenderName, message.SenderNumber = guest.name, guest.number
				default:
					sender := people[random.Intn(len(people))]
					message.SenderName, message.SenderNumber = sender.name, sender.number
				}
				if err := store.UpsertMessage(message); err != nil {
					t.Fatalf("seed message: %v", err)
				}
				model.messages = append(model.messages, message)
			}
		}
		model.contacts = []*Contact{{ContactID: "card-owner", Name: owner.name, Number: owner.number}}
		if random.Intn(2) == 0 {
			model.contacts = append(model.contacts, &Contact{ContactID: "card-guest", Name: "Sammy G", Number: guest.number})
		}
		for _, contact := range model.contacts {
			if err := store.UpsertContact(contact); err != nil {
				t.Fatalf("seed contact: %v", err)
			}
		}
		stored, err := store.ListConversations(1000)
		if err != nil {
			t.Fatalf("list conversations: %v", err)
		}
		model.conversations = stored

		for _, query := range []string{
			owner.name, "Sam", "Owner", "Me", "is_me", "isMe", "true", "Sammy", "Guest", "Bob", "Carol",
			"Lee", "Group", "club", "_", "%", "Ed_", "100%", "E_", "S%m", "not json", "+1555555", "56789",
			owner.number, "5550009999", guest.number, "name", "number", "phone", "email", `":"`,
			"contact_id", "card-", "R&D", "u0026", "@example.com", "bob@",
		} {
			limit := 1 + random.Intn(10)
			want := model.search(query, limit)
			got := metadataSearchIDs(t, store, query, limit)
			if fmt.Sprint(got) != fmt.Sprint(want) {
				for _, conversation := range stored {
					t.Logf("  %s %q ts=%d: %s", conversation.ConversationID, conversation.Name, conversation.LastMessageTS, conversation.Participants)
				}
				t.Logf("seed %d: search %q (limit %d) = %v, model says %v", seed, query, limit, got, want)
				return false
			}
			if len(want) == 0 {
				continue
			}
			exercised["a query with hits"]++
			oldest := int64(-1)
			for _, conversation := range stored {
				if conversation.ConversationID == want[len(want)-1] {
					oldest = conversation.LastMessageTS
				}
			}
			for _, conversation := range stored {
				_, senderOnly, ownerOnly := model.match(conversation, query)
				if senderOnly {
					exercised["a hit through an incoming sender or card alone"]++
				}
				if ownerOnly && conversation.LastMessageTS > oldest {
					exercised["a hit behind a newer thread that matches only through the owner"]++
				}
			}
		}
		return true
	}
	cases := 200
	if raceDetectorEnabled {
		cases = 40
	}
	if err := quick.Check(property, &quick.Config{MaxCount: cases, Rand: rand.New(rand.NewSource(20261103))}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		"a query with hits",
		"a hit through an incoming sender or card alone",
		"a hit behind a newer thread that matches only through the owner",
	} {
		if exercised[path] == 0 {
			t.Errorf("the property never drew %s; raise MaxCount (drew %v)", path, exercised)
		}
	}
}
