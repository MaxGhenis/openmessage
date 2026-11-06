package db

import (
	"encoding/json"
	"math/rand"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"testing/quick"
)

// foldASCIIForModel lowers ASCII letters and keeps every other rune, a NUL
// among them, as the address search compares values. The address model uses
// it instead of addressmatch.Key, so the model stays apart from the code it
// checks.
func foldASCIIForModel(s string) string {
	return strings.Map(func(r rune) rune {
		if 'A' <= r && r <= 'Z' {
			return r + 'a' - 'A'
		}
		return r
	}, s)
}

// addressForm writes value with the case of some of its ASCII letters
// flipped, as a transport or an importer might write an address.
func addressForm(random *rand.Rand, value string) string {
	b := []byte(value)
	for index, c := range b {
		if random.Intn(3) != 0 {
			continue
		}
		switch {
		case 'a' <= c && c <= 'z':
			b[index] = c - 'a' + 'A'
		case 'A' <= c && c <= 'Z':
			b[index] = c - 'A' + 'a'
		}
	}
	return string(b)
}

// spaceAround puts white space that strings.TrimSpace trims around value
// some of the time, as a hand-typed card or an importer might, with runes
// of one, two and three bytes.
func spaceAround(random *rand.Rand, value string) string {
	if random.Intn(4) != 0 {
		return value
	}
	spaces := []string{" ", "\t", "\n", "\u0085", "\u00a0", "\u2028", "\u3000"}
	pad := func() string {
		var b strings.Builder
		for count := random.Intn(3); count > 0; count-- {
			b.WriteString(spaces[random.Intn(len(spaces))])
		}
		return b.String()
	}
	return pad() + value + pad()
}

// addressModel is a reference for SearchConversationsByAddress. A thread
// holds an address when a participant entry without the owner's flag holds
// it as the text of its number, phone or email, when a bare string entry
// holds it, or when an incoming message's sender number does. A value names
// the address when the two hold the same bytes once strings.TrimSpace has
// trimmed both, ASCII letters' case aside. An entry carries the owner's flag
// when its is_me or isMe holds JSON true. Names and titles never count, and
// a blank address matches nothing. Hits come newest first, then in
// conversation ID order, up to the limit.
type addressModel struct {
	conversations []*Conversation
	messages      []*Message
}

// addressHold says how a thread holds an address. hit: it holds it.
// senderOnly: only an incoming sender holds it. ownerOnly: only entries with
// the owner's flag hold it. containsOnly: it doesn't hold it, but some
// value, name or title contains it. nameOnly: it doesn't hold it, but a name
// or the title is it. otherCaseOnly: it doesn't hold it, but a value would
// be it if a non-ASCII letter's case didn't count. caseDiffers: it holds it
// only through values whose ASCII letters' case differs from the address's.
// padded: it holds it through a value with white space around it.
type addressHold struct {
	hit, senderOnly, ownerOnly, containsOnly, nameOnly, otherCaseOnly, caseDiffers, padded bool
}

func (m addressModel) holds(conversation *Conversation, address string) addressHold {
	address = strings.TrimSpace(address)
	want := foldASCIIForModel(address)
	var held addressHold
	byEntry, byOwner, bySender, exact := false, false, false, false
	// is reports whether value is the address, and notes how a value that
	// isn't the address relates to it.
	is := func(value string) bool {
		value = strings.TrimSpace(value)
		folded := foldASCIIForModel(value)
		switch {
		case folded == want:
			return true
		case strings.Contains(folded, want):
			held.containsOnly = true
		case strings.EqualFold(value, address):
			held.otherCaseOnly = true
		}
		return false
	}
	// ties records value, which is the address, as tying the thread to it.
	ties := func(value string) {
		if strings.TrimSpace(value) == address {
			exact = true
		}
		if strings.TrimSpace(value) != value {
			held.padded = true
		}
	}
	if is(conversation.Name) {
		held.nameOnly = true
	}
	var elements []json.RawMessage
	if json.Unmarshal([]byte(conversation.Participants), &elements) == nil {
		for _, element := range elements {
			var object map[string]any
			if json.Unmarshal(element, &object) == nil && object != nil {
				if name, ok := object["name"].(string); ok && is(name) {
					held.nameOnly = true
				}
				var found []string
				for _, key := range []string{"number", "phone", "email"} {
					if value, ok := object[key].(string); ok && is(value) {
						found = append(found, value)
					}
				}
				if len(found) == 0 {
					continue
				}
				if object["is_me"] == true || object["isMe"] == true {
					byOwner = true
					continue
				}
				byEntry = true
				for _, value := range found {
					ties(value)
				}
				continue
			}
			var text string
			if json.Unmarshal(element, &text) == nil && is(text) {
				byEntry = true
				ties(text)
			}
		}
	}
	for _, message := range m.messages {
		if message.ConversationID != conversation.ConversationID {
			continue
		}
		if is(message.SenderName) {
			held.nameOnly = true
		}
		if is(message.SenderNumber) && !message.IsFromMe {
			bySender = true
			ties(message.SenderNumber)
		}
	}
	held.hit = byEntry || bySender
	held.senderOnly = bySender && !byEntry
	held.ownerOnly = byOwner && !held.hit
	held.containsOnly = held.containsOnly && !held.hit
	held.nameOnly = held.nameOnly && !held.hit
	held.otherCaseOnly = held.otherCaseOnly && !held.hit
	held.caseDiffers = held.hit && !exact
	held.padded = held.padded && held.hit
	return held
}

func (m addressModel) search(address string, limit int) []string {
	ids := []string{}
	if strings.TrimSpace(address) == "" || limit <= 0 {
		return ids
	}
	for _, conversation := range m.conversations {
		if len(ids) == limit {
			break
		}
		if m.holds(conversation, address).hit {
			ids = append(ids, conversation.ConversationID)
		}
	}
	return ids
}

// newestIDs returns the IDs in ids that order puts first, at most limit.
// order lists conversations newest first, then in conversation ID order.
func newestIDs(order []*Conversation, ids []string, limit int) []string {
	newest := []string{}
	for _, conversation := range order {
		if len(newest) == limit {
			break
		}
		if slices.Contains(ids, conversation.ConversationID) {
			newest = append(newest, conversation.ConversationID)
		}
	}
	return newest
}

func conversationIDs(conversations []*Conversation) []string {
	ids := []string{}
	for _, conversation := range conversations {
		ids = append(ids, conversation.ConversationID)
	}
	return ids
}

func TestSearchConversationsByAddressMatchesTheWholeAddress(t *testing.T) {
	store := newTestStore(t)
	for _, conversation := range []*Conversation{
		{ConversationID: "alice", Name: "Alice", LastMessageTS: 1, Participants: `[{"name":"Alice","number":"support@example.com"}]`},
		{ConversationID: "alice-email", Name: "Alice", LastMessageTS: 2, Participants: `[{"name":"Alice C.","email":"Support@Example.COM"}]`},
		{ConversationID: "bare", Name: "Bare", LastMessageTS: 3, Participants: `["support@example.com"]`},
		{ConversationID: "escaped", Name: "Escaped", LastMessageTS: 4, Participants: `[{"number":"support\u0040example.com"}]`},
		{ConversationID: "sender", Name: "Sender", LastMessageTS: 5, Participants: `[]`},
		{ConversationID: "sent", Name: "Sent", LastMessageTS: 6, Participants: `[]`},
		{ConversationID: "owner", Name: "Owner", LastMessageTS: 7, Participants: `[{"name":"Me","number":"support@example.com","is_me":true}]`},
		{ConversationID: "other", Name: "Other", LastMessageTS: 8, Participants: `[{"name":"Other","number":"other-support@example.com"}]`},
		{ConversationID: "longer", Name: "Longer", LastMessageTS: 9, Participants: `[{"name":"Longer","email":"support@example.com.au"}]`},
		{ConversationID: "titled", Name: "Ask support@example.com", LastMessageTS: 10, Participants: `[{"name":"Guest","number":"+15555550100"}]`},
		{ConversationID: "named", Name: "Named", LastMessageTS: 11, Participants: `[{"name":"support@example.com","number":"+15555550101"}]`},
		{ConversationID: "user", Name: "User", LastMessageTS: 12, Participants: `[{"name":"U","number":"users/1234"}]`},
		{ConversationID: "accented", Name: "Accented", LastMessageTS: 13, Participants: `[{"name":"E","email":"élise@example.com"}]`},
		{ConversationID: "blank", Name: "Blank", LastMessageTS: 14, Participants: `[{"name":"Blank","number":"","email":"   "},""]`},
		{ConversationID: "padded", Name: "Padded", LastMessageTS: 15, Participants: `[{"name":"P","number":" support@example.com\t"}]`},
		{ConversationID: "padded-wide", Name: "Wide", LastMessageTS: 16, Participants: `[{"name":"W","email":"\u00a0Support@Example.com\u3000"}]`},
		{ConversationID: "padded-bare", Name: "Padded bare", LastMessageTS: 17, Participants: `["\nsupport@example.com\u2028"]`},
		{ConversationID: "padded-sender", Name: "Padded sender", LastMessageTS: 18, Participants: `[]`},
		{ConversationID: "inner-space", Name: "Inner", LastMessageTS: 19, Participants: `[{"name":"I","number":"support @example.com"}]`},
		{ConversationID: "invisible", Name: "Invisible", LastMessageTS: 20, Participants: `[{"name":"Z","number":"\u200bsupport@example.com"},"support@example.com\ufeff"]`},
		{ConversationID: "short-code", Name: "Pharmacy", LastMessageTS: 21, Participants: `[{"name":"Pharmacy","number":"55501"}]`},
		{ConversationID: "numeric", Name: "Numeric", LastMessageTS: 22, Participants: `[{"name":"N","number":55501}]`},
		{ConversationID: "nul-cut", Name: "Cut", LastMessageTS: 23, Participants: `[{"name":"C","number":"support@example.com\u0000x"},"support@example.com\u0000y"]`},
		{ConversationID: "nul-inside", Name: "Inside", LastMessageTS: 24, Participants: `[{"name":"N","email":"a\u0000c@example.com"}]`},
	} {
		conversation.SourcePlatform = "gchat"
		if err := store.UpsertConversation(conversation); err != nil {
			t.Fatalf("seed %s: %v", conversation.ConversationID, err)
		}
	}
	for _, message := range []*Message{
		{MessageID: "from-them", ConversationID: "sender", SenderNumber: "SUPPORT@example.com", Body: "hi", TimestampMS: 5, SourcePlatform: "gchat"},
		{MessageID: "from-me", ConversationID: "sent", SenderNumber: "support@example.com", Body: "hi", TimestampMS: 6, SourcePlatform: "gchat", IsFromMe: true},
		{MessageID: "from-nobody", ConversationID: "blank", SenderNumber: "", Body: "hi", TimestampMS: 14, SourcePlatform: "gchat"},
		{MessageID: "from-them-padded", ConversationID: "padded-sender", SenderNumber: "\u0085support@example.com ", Body: "hi", TimestampMS: 18, SourcePlatform: "gchat"},
		{MessageID: "from-them-cut", ConversationID: "nul-cut", SenderNumber: "support@example.com\x00z", Body: "hi", TimestampMS: 23, SourcePlatform: "gchat"},
	} {
		if err := store.UpsertMessage(message); err != nil {
			t.Fatalf("seed %s: %v", message.MessageID, err)
		}
	}
	hits := []string{"padded-sender", "padded-bare", "padded-wide", "padded", "sender", "escaped", "bare", "alice-email", "alice"}
	for _, test := range []struct {
		address string
		limit   int
		want    []string
	}{
		{"support@example.com", 20, hits},
		{"SUPPORT@EXAMPLE.COM", 20, hits},
		{"\u3000support@example.com\n", 20, hits},
		{"support@example.com", 1, []string{"padded-sender"}},
		{"55501", 20, []string{"short-code"}},
		// A NUL counts as any other byte, so a value that one ends early
		// matches nothing, and neither does one that differs only after it.
		{"a\x00c@example.com", 20, []string{"nul-inside"}},
		{"A\x00C@EXAMPLE.COM ", 20, []string{"nul-inside"}},
		{"a\x00b@example.com", 20, []string{}},
		{"a", 20, []string{}},
		{"example.com", 20, []string{}},
		{"users/123", 20, []string{}},
		{"Élise@example.com", 20, []string{}},
		{"élise@EXAMPLE.com", 20, []string{"accented"}},
		{"", 20, []string{}},
		{"   ", 20, []string{}},
		{"\u3000\u00a0", 20, []string{}},
		{"support@example.com", 0, []string{}},
		{"support@example.com", -1, []string{}},
	} {
		got, err := store.SearchConversationsByAddress(test.address, test.limit)
		if err != nil {
			t.Fatalf("SearchConversationsByAddress(%q, %d): %v", test.address, test.limit, err)
		}
		if ids := conversationIDs(got); !slices.Equal(ids, test.want) {
			t.Errorf("SearchConversationsByAddress(%q, %d) = %v, want %v", test.address, test.limit, ids, test.want)
		}
	}
}

// Differential, over random stores: SearchConversationsByAddress returns
// exactly what addressModel returns, in the same order. Addresses come with
// their ASCII letters in any case and often with white space around them.
// Decoys hold a target inside a longer value, beside a space inside it or a
// character that only looks like white space, before a NUL, or with a
// non-ASCII letter in another case, one differs from a target only after a
// NUL, and some values are blank; names, senders' names and titles hold
// targets too; some entries carry a value under phone or email, a number that
// isn't text, or a nested array; JSON escapes encode some values; the owner's
// entries carry the owner's address flagged, unflagged or with a flag that
// isn't JSON true, and the owner sends messages too; and threads often share
// a time. So, as invariants, a thread holds an address only through someone
// other than the flagged owner whose number, phone, email or bare entry is
// that address as a whole, up to ASCII letters' case and the white space
// around each, an incoming sender whom the roster lacks counts, the limit
// applies after matching, so threads whose values only contain the address
// never crowd out one that holds it, and threads that share a time come in
// conversation ID order. Two more hold for every query: any way of writing
// its ASCII letters' case, with any white space around it, finds the same
// threads, and the metadata search for the same text without the white space
// around it, at the same limit, returns every thread the address search
// returns, or newer ones in its place, so the route tool loses nothing when
// it counts the query, which its handler trims, as an address it has looked
// up.
func TestSearchConversationsByAddressAgreesWithTheAddressModel(t *testing.T) {
	targets := []string{"support@example.com", "users/123", "abcdefab-abcd-abcd-abcd-abcdefabcdef", "a&b@example.com", "élise@example.com", "a\x00c@example.com"}
	owner := "me@example.com"
	// The last two decoys are blank: a blank address matches nothing, not
	// even a blank value.
	decoys := []string{"other-support@example.com", "support@example.com.au", "\u200bsupport@example.com", "support@example.com\ufeff", "support @example.com", "users/1234", "xusers/123", "Élise@example.com", "a&b@example.co", "a\x00b@example.com", "support@example.com\x00x", "", "  "}
	exercised := map[string]int{}

	property := func(seed int64) bool {
		random := rand.New(rand.NewSource(seed))
		store := newTestStore(t)
		model := addressModel{}
		values := append(append([]string{owner}, targets...), decoys...)
		value := func() string { return spaceAround(random, addressForm(random, values[random.Intn(len(values))])) }

		count := 4 + random.Intn(10)
		for index := 0; index < count; index++ {
			id := "c" + strconv.Itoa(index)
			name := "Thread " + id
			switch random.Intn(5) {
			case 0:
				name = "Ask " + value()
			case 1:
				name = value()
			}
			var entries []any
			for member := 0; member < 1+random.Intn(3); member++ {
				form := value()
				person := "P" + strconv.Itoa(member)
				switch random.Intn(9) {
				case 0:
					entries = append(entries, form)
				case 1:
					entries = append(entries, map[string]any{"name": person, "phone": form})
				case 2:
					entries = append(entries, map[string]any{"name": person, "number": " ", "email": form})
				case 3:
					entries = append(entries, map[string]any{"name": form, "number": "+1555555" + strconv.Itoa(1000+member)})
				case 4:
					entries = append(entries, map[string]any{"name": person, "number": 12025550123})
				case 5:
					entries = append(entries, []any{form})
				default:
					entries = append(entries, map[string]any{"name": person, "number": form})
				}
			}
			ownerForm := spaceAround(random, addressForm(random, owner))
			switch random.Intn(5) {
			case 0: // the owner isn't listed
			case 1:
				entries = append(entries, map[string]any{"name": "Me", "number": ownerForm, "is_me": "true"})
			case 2:
				entries = append(entries, map[string]any{"name": "Me", "email": ownerForm, "isMe": true})
			default:
				entries = append(entries, map[string]any{"name": "Me", "number": ownerForm, "is_me": true})
			}
			random.Shuffle(len(entries), func(i, j int) { entries[i], entries[j] = entries[j], entries[i] })
			// json.Marshal writes & as \u0026, so the store must decode escapes.
			participants, err := json.Marshal(entries)
			if err != nil {
				t.Fatal(err)
			}
			switch random.Intn(12) {
			case 0:
				participants = []byte("")
			case 1:
				participants = []byte(`{"number":"` + targets[0] + `"}`)
			}
			if err := store.UpsertConversation(&Conversation{
				ConversationID: id, Name: name, Participants: string(participants),
				LastMessageTS: int64(1_000_000 + 1000*random.Intn(6)), SourcePlatform: "gchat",
			}); err != nil {
				t.Fatalf("seed conversation: %v", err)
			}
			for number := 0; number < random.Intn(3); number++ {
				message := &Message{
					MessageID: id + "-m" + strconv.Itoa(number), ConversationID: id, Body: "hello",
					TimestampMS: int64(1000 + number), SourcePlatform: "gchat",
					SenderNumber: value(),
				}
				if random.Intn(4) == 0 {
					message.SenderName = value()
				}
				if random.Intn(3) == 0 {
					message.IsFromMe = true
				}
				if err := store.UpsertMessage(message); err != nil {
					t.Fatalf("seed message: %v", err)
				}
				model.messages = append(model.messages, message)
			}
		}
		stored, err := store.ListConversations(1000)
		if err != nil {
			t.Fatalf("list conversations: %v", err)
		}
		sort.SliceStable(stored, func(i, j int) bool {
			if stored[i].LastMessageTS != stored[j].LastMessageTS {
				return stored[i].LastMessageTS > stored[j].LastMessageTS
			}
			return stored[i].ConversationID < stored[j].ConversationID
		})
		model.conversations = stored
		dump := func() {
			for _, conversation := range stored {
				t.Logf("  %s ts=%d name=%q: %s", conversation.ConversationID, conversation.LastMessageTS, conversation.Name, conversation.Participants)
			}
			for _, message := range model.messages {
				t.Logf("  message %s in %s from_me=%v sender=%q name=%q", message.MessageID, message.ConversationID, message.IsFromMe, message.SenderNumber, message.SenderName)
			}
		}
		search := func(address string, limit int) []string {
			got, err := store.SearchConversationsByAddress(address, limit)
			if err != nil {
				t.Fatalf("SearchConversationsByAddress(%q): %v", address, err)
			}
			return conversationIDs(got)
		}

		for _, address := range append(append([]string{owner, "", "  "}, targets...), decoys[:4]...) {
			if random.Intn(2) == 0 {
				address = addressForm(random, address)
			}
			address = spaceAround(random, address)
			limit := 1 + random.Intn(6)
			want := model.search(address, limit)
			got := search(address, limit)
			if !slices.Equal(got, want) {
				dump()
				t.Logf("seed %d: address %q (limit %d) = %v, model says %v", seed, address, limit, got, want)
				return false
			}
			variant := spaceAround(random, addressForm(random, address))
			if again := search(variant, limit); !slices.Equal(again, got) {
				dump()
				t.Logf("seed %d: address %q (limit %d) = %v, but %q = %v", seed, address, limit, got, variant, again)
				return false
			}
			text := strings.TrimSpace(address)
			metadata, err := store.SearchConversationsByMetadata(text, limit)
			if err != nil {
				t.Fatalf("SearchConversationsByMetadata(%q): %v", text, err)
			}
			byText := conversationIDs(metadata)
			if merged := newestIDs(stored, append(slices.Clone(byText), got...), limit); !slices.Equal(merged, byText) {
				dump()
				t.Logf("seed %d: address %q (limit %d) = %v, which the metadata search's %v for %q lacks room for (merged %v)", seed, address, limit, got, byText, text, merged)
				return false
			}
			if len(want) == 0 {
				continue
			}
			exercised["an address with hits"]++
			if text != address {
				exercised["an address with white space around it that has hits"]++
			}
			if strings.Contains(address, "\x00") {
				exercised["an address with a NUL in it that has hits"]++
			}
			if all := model.search(address, len(stored)); len(all) > len(want) {
				at := map[string]int64{}
				for _, conversation := range stored {
					at[conversation.ConversationID] = conversation.LastMessageTS
				}
				if at[all[len(want)]] == at[want[len(want)-1]] {
					exercised["a tie at the limit"]++
				}
			}
			if !slices.Equal(byText, got) {
				exercised["a metadata search that returns other threads"]++
			}
			oldest := int64(-1)
			for _, conversation := range stored {
				if conversation.ConversationID == want[len(want)-1] {
					oldest = conversation.LastMessageTS
				}
			}
			for _, conversation := range stored {
				held := model.holds(conversation, address)
				for path, drawn := range map[string]bool{
					"a hit through an incoming sender alone":                                held.senderOnly,
					"a thread that holds the address only through the flagged owner":        held.ownerOnly,
					"a hit whose ASCII letters' case differs from the query's":              held.caseDiffers,
					"a thread whose name or title alone is the address":                     held.nameOnly,
					"a thread whose values would hold it but for a non-ASCII letter's case": held.otherCaseOnly,
					"a hit behind a newer thread whose values only contain the address":     held.containsOnly && conversation.LastMessageTS > oldest,
					"a hit through a value with white space around it":                      held.padded,
				} {
					if drawn {
						exercised[path]++
					}
				}
				if held.hit && !held.senderOnly && strings.Contains(address, "&") {
					exercised["a hit through a JSON escape"]++
				}
			}
		}
		return true
	}
	cases := 200
	if raceDetectorEnabled {
		cases = 40
	}
	if err := quick.Check(property, &quick.Config{MaxCount: cases, Rand: rand.New(rand.NewSource(20261104))}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		"an address with hits",
		"an address with a NUL in it that has hits",
		"a tie at the limit",
		"a metadata search that returns other threads",
		"a hit through an incoming sender alone",
		"a thread that holds the address only through the flagged owner",
		"a hit whose ASCII letters' case differs from the query's",
		"a thread whose name or title alone is the address",
		"a thread whose values would hold it but for a non-ASCII letter's case",
		"a hit behind a newer thread whose values only contain the address",
		"a hit through a JSON escape",
		"a hit through a value with white space around it",
		"an address with white space around it that has hits",
	} {
		if exercised[path] == 0 {
			t.Errorf("the property never drew %s; raise MaxCount (drew %v)", path, exercised)
		}
	}
}
