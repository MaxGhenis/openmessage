package db

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"testing/quick"
)

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
// holds an address when a participant entry that the owner rule doesn't
// count as the owner's holds it, or when an incoming message's sender number
// does. An entry holds it through a string element's text, or through an
// object's "number", "phone" or "email" value where that value is a JSON
// string; where a key repeats, its last value counts, as a generic decode
// keeps it. A value names the address when the two hold the same bytes once
// strings.TrimSpace has trimmed both, ASCII letters' case aside, so a NUL
// counts as any other byte does. The owner rule is searchModel's: an object
// that carries is_me or isMe as JSON true, and any entry, in any thread, on
// the phone line of a flagged entry whose number names one, as phonelinetest
// restates phoneline's rule. Lists read as modelEntries reads them. Names and
// titles never count, and a blank address matches nothing. Hits come newest
// first, then in conversation ID order, up to the limit.
type addressModel struct {
	conversations []*Conversation
	messages      []*Message
}

// addressHold says how a thread holds an address. hit: it holds it.
// senderOnly: only an incoming sender holds it. ownerOnly: only entries that
// the owner rule counts as the owner's hold it. ownerCopyOnly: so do some
// unflagged entries among them. containsOnly: it doesn't hold it, but some
// value, name or title contains it. nameOnly: it doesn't hold it, but a name
// or the title is it. otherCaseOnly: it doesn't hold it, but a value would
// be it if a non-ASCII letter's case didn't count. nulTwinOnly: it doesn't
// hold it, but a value as long as the address agrees with it up to a NUL
// that both hold, where SQLite's NOCASE stops comparing. earlierValueOnly:
// it doesn't hold it, but an earlier value of a repeated key of someone's is
// it. caseDiffers: it holds it only through values whose ASCII letters' case
// differs from the address's. padded: it holds it through a value with white
// space around it. hugeNumber: someone who holds it holds a number too large
// for a float64 too.
type addressHold struct {
	hit, senderOnly, ownerOnly, ownerCopyOnly, containsOnly, nameOnly             bool
	otherCaseOnly, nulTwinOnly, earlierValueOnly, caseDiffers, padded, hugeNumber bool
}

// nocaseAgreesUpToANUL reports whether a and b, two different texts of one
// length with their ASCII letters lowered, agree up to a NUL that both hold
// at the same place. SQLite's NOCASE stops comparing at a NUL and then
// compares lengths, so it takes such texts for one text.
func nocaseAgreesUpToANUL(a, b string) bool {
	if a == b || len(a) != len(b) {
		return false
	}
	cut := strings.IndexByte(a, 0)
	return cut >= 0 && b[cut] == 0 && a[:cut] == b[:cut]
}

// modelHoldsAHugeNumber reports whether entry holds a number too large for a
// float64.
func modelHoldsAHugeNumber(entry modelEntry) bool {
	for _, member := range entry.members {
		if number, ok := member.value.(json.Number); ok {
			if _, err := number.Float64(); err != nil {
				return true
			}
		}
	}
	return false
}

// modelAddressKey writes value as the model compares values: without the
// white space around it, as strings.TrimSpace trims it, and with its ASCII
// letters lowered. The model uses it instead of addressmatch.Key, so it stays
// apart from the code it checks.
func modelAddressKey(value string) string {
	return modelFoldASCII(strings.TrimSpace(value))
}

// holds says how conversation holds address, given the owner's phone lines,
// which modelOwnerLines gathers from every conversation.
func (m addressModel) holds(conversation *Conversation, address string, lines []string) addressHold {
	address = strings.TrimSpace(address)
	want := modelFoldASCII(address)
	var held addressHold
	byEntry, byOwner, byOwnerCopy, bySender, exact := false, false, false, false, false
	// is reports whether value is the address, and notes how a value that
	// isn't the address relates to it.
	is := func(value string) bool {
		value = strings.TrimSpace(value)
		folded := modelFoldASCII(value)
		switch {
		case folded == want:
			return true
		case strings.Contains(folded, want):
			held.containsOnly = true
		case strings.EqualFold(value, address):
			held.otherCaseOnly = true
		case nocaseAgreesUpToANUL(folded, want):
			held.nulTwinOnly = true
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
	for _, entry := range modelEntries(conversation.Participants) {
		var found []string
		if entry.isString {
			if is(entry.text) {
				found = append(found, entry.text)
			}
		} else {
			if name, ok := entry.object["name"].(string); ok && is(name) {
				held.nameOnly = true
			}
			for _, key := range []string{"number", "phone", "email"} {
				if value, ok := entry.object[key].(string); ok && is(value) {
					found = append(found, value)
				}
			}
		}
		owners := modelOwners(entry, lines)
		if !owners && !entry.isString {
			for _, member := range entry.members {
				switch member.key {
				case "number", "phone", "email":
					value, isText := member.value.(string)
					last, lastIsText := entry.object[member.key].(string)
					if isText && modelAddressKey(value) == want && (!lastIsText || modelAddressKey(last) != want) {
						held.earlierValueOnly = true
					}
				}
			}
		}
		if len(found) == 0 {
			continue
		}
		if owners {
			byOwner = true
			if !modelFlagged(entry) {
				byOwnerCopy = true
			}
			continue
		}
		byEntry = true
		if modelHoldsAHugeNumber(entry) {
			held.hugeNumber = true
		}
		for _, value := range found {
			ties(value)
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
	held.ownerCopyOnly = byOwnerCopy && !held.hit
	held.containsOnly = held.containsOnly && !held.hit
	held.nameOnly = held.nameOnly && !held.hit
	held.otherCaseOnly = held.otherCaseOnly && !held.hit
	held.nulTwinOnly = held.nulTwinOnly && !held.hit
	held.earlierValueOnly = held.earlierValueOnly && !held.hit
	held.caseDiffers = held.hit && !exact
	held.padded = held.padded && held.hit
	return held
}

func (m addressModel) search(address string, limit int) []string {
	ids := []string{}
	if strings.TrimSpace(address) == "" || limit <= 0 {
		return ids
	}
	lines := modelOwnerLines(m.conversations)
	for _, conversation := range m.conversations {
		if len(ids) == limit {
			break
		}
		if m.holds(conversation, address, lines).hit {
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

func addressSearchIDs(t *testing.T, store *Store, address string, limit int) string {
	t.Helper()
	got, err := store.SearchConversationsByAddress(address, limit)
	if err != nil {
		t.Fatalf("SearchConversationsByAddress(%q, %d): %v", address, limit, err)
	}
	return fmt.Sprint(conversationIDs(got))
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

// Most threads list the account's owner, and Google's lists often repeat
// the owner without the flag, so an address that only the owner's entries
// hold finds no thread. The owner rule in owner.go names those entries, as
// it does for send_message and the other legacy searches: an object that
// carries is_me or isMe, and any entry on the phone line of a flagged entry
// whose number names one. Here an unflagged copy lists the owner's number
// beside the owner's email address, so neither value finds its thread, and
// nor does a bare string that holds the owner's number. A WhatsApp LID names
// no line, so the entry that holds one is another person's, and another
// person's address still finds their thread.
func TestSearchConversationsByAddressSkipsTheOwnersParticipantEntries(t *testing.T) {
	store := newTestStore(t)
	seedConversations(t, store,
		&Conversation{ConversationID: "flagged", Name: "Flagged", SourcePlatform: "gchat", LastMessageTS: 1000,
			Participants: `[{"name":"Sam Owner","number":"+15550009999","email":"sam@example.com","is_me":true},{"name":"Bob","email":"bob@example.com"}]`},
		&Conversation{ConversationID: "copy", Name: "Copy", SourcePlatform: "gchat", LastMessageTS: 2000,
			Participants: `[{"name":"Sam Owner","number":"+1 (555) 000-9999","email":"sam@example.com"},{"name":"Carol","email":"carol@example.com"}]`},
		&Conversation{ConversationID: "bare", Name: "Bare", SourcePlatform: "sms", LastMessageTS: 3000,
			Participants: `["555-000-9999","55501"]`},
		&Conversation{ConversationID: "lid", Name: "Lid", SourcePlatform: "whatsapp", LastMessageTS: 4000,
			Participants: `[{"name":"Sam","number":"15550009999@lid"}]`},
	)
	for _, test := range []struct{ address, want string }{
		{"sam@example.com", "[]"},
		{"SAM@example.com", "[]"},
		{"+1 (555) 000-9999", "[]"},
		{"555-000-9999", "[]"},
		{"+15550009999", "[]"},
		{"bob@example.com", "[flagged]"},
		{"carol@example.com", "[copy]"},
		{"55501", "[bare]"},
		{"15550009999@lid", "[lid]"},
	} {
		if got := addressSearchIDs(t, store, test.address, 10); got != test.want {
			t.Errorf("SearchConversationsByAddress(%q) = %v, want %v", test.address, got, test.want)
		}
	}
}

// The address search reads participant lists through owner.go, as
// send_message and the other legacy searches do. SQLite's JSON reader stops
// at 1,000 levels of nesting, reads TEXT only up to a NUL, and takes a
// repeated key's first value, so it would read no one from the first list
// below, read Nell from the second, and take Rae's old address and Flo's
// first flag. Go reads 10,000 levels deep, reads no entry from a list with
// bytes after a NUL, and takes a repeated key's last value. The search
// compares addressmatch keys byte by byte, where SQLite's NOCASE stops at a
// NUL and then compares only lengths, so it would take
// "val@example.com\u0000b" for Val's "val@example.com\u0000a".
func TestSearchConversationsByAddressReadsListsAsSendMessageDoes(t *testing.T) {
	store := newTestStore(t)
	seedConversations(t, store,
		&Conversation{ConversationID: "deep", Name: "Deep", SourcePlatform: "gchat", LastMessageTS: 1000,
			Participants: `[` + ownerRuleNested(1001) + `,{"name":"Dee","email":"dee@example.com"}]`},
		&Conversation{ConversationID: "after-nul", Name: "After NUL", SourcePlatform: "gchat", LastMessageTS: 2000,
			Participants: "[{\"name\":\"Nell\",\"email\":\"nell@example.com\"}]\x00garbage"},
		&Conversation{ConversationID: "repeated", Name: "Repeated", SourcePlatform: "gchat", LastMessageTS: 3000,
			Participants: `[{"name":"Rae","email":"old@example.com","email":"rae@example.com"}]`},
		&Conversation{ConversationID: "flag-repeated", Name: "Flag repeated", SourcePlatform: "gchat", LastMessageTS: 4000,
			Participants: `[{"is_me":true,"name":"Flo","email":"flo@example.com","is_me":false}]`},
		&Conversation{ConversationID: "nul-value", Name: "NUL value", SourcePlatform: "gchat", LastMessageTS: 5000,
			Participants: `[{"name":"Val","email":"val@example.com\u0000a"}]`},
		&Conversation{ConversationID: "huge", Name: "Huge", SourcePlatform: "gchat", LastMessageTS: 6000,
			Participants: `[{"id":1e400,"name":"Hugh","email":"hugh@example.com"}]`},
		&Conversation{ConversationID: "escaped-key", Name: "Escaped key", SourcePlatform: "gchat", LastMessageTS: 7000,
			Participants: `[{"name":"Esme","\u0065mail":"esme@example.com"}]`},
	)
	for _, test := range []struct{ address, want string }{
		{"dee@example.com", "[deep]"},
		{"nell@example.com", "[]"},
		{"rae@example.com", "[repeated]"},
		{"old@example.com", "[]"},
		{"flo@example.com", "[flag-repeated]"},
		{"val@example.com\x00a", "[nul-value]"},
		{"VAL@example.com\x00A", "[nul-value]"},
		{"val@example.com\x00b", "[]"},
		{"val@example.com", "[]"},
		{"hugh@example.com", "[huge]"},
		{"esme@example.com", "[escaped-key]"},
	} {
		if got := addressSearchIDs(t, store, test.address, 10); got != test.want {
			t.Errorf("SearchConversationsByAddress(%q) = %v, want %v", test.address, got, test.want)
		}
	}
}

// Differential, over random stores: SearchConversationsByAddress returns
// exactly what addressModel returns, in the same order. Addresses come with
// their ASCII letters in any case and often with white space around them.
// Decoys hold a target inside a longer value, beside a space inside it or a
// character that only looks like white space, before a NUL, or with a
// non-ASCII letter in another case, some differ from a target only after a
// NUL, and some values are blank; names, senders' names and titles hold
// targets too; some entries carry a value under phone or email, a number that
// isn't text, a nested array, a repeated key whose values differ, or a number
// too large for a float64; JSON escapes encode some values; some lists nest
// an element deeper than SQLite's JSON reader goes, or carry bytes after a
// NUL, where SQLite's reader stops and Go's doesn't; the owner's entries carry
// the owner's number and email address, flagged, unflagged or with a flag that
// isn't JSON true, bare strings hold the owner's number, and the owner sends
// messages too; and threads often share a time. So, as invariants, a thread
// holds an address only through someone whose number, phone, email or bare
// entry is that address as a whole, up to ASCII letters' case and the white
// space around each and with every byte after a NUL counting, and who isn't
// the owner, as the owner rule in owner.go counts the owner's entries for
// send_message; an incoming sender whom the roster lacks counts; the search
// reads every list as send_message does; the limit applies after matching, so
// threads whose values only contain the address never crowd out one that holds
// it; and threads that share a time come in conversation ID order. Two more
// hold for every query: any way of writing its ASCII letters' case, with any
// white space around it, finds the same threads, and the metadata search for
// the same text without the white space around it, at the same limit, returns
// every thread the address search returns, or newer ones in its place, so the
// route tool loses nothing when it counts the query, which its handler trims,
// as an address it has looked up.
func TestSearchConversationsByAddressAgreesWithTheAddressModel(t *testing.T) {
	targets := []string{"support@example.com", "users/123", "abcdefab-abcd-abcd-abcd-abcdefabcdef", "a&b@example.com", "élise@example.com", "a\x00c@example.com"}
	ownerEmail := "me@example.com"
	ownerNumbers := []string{"+15550009999", "+1 (555) 000-9999", "555-000-9999", "15550009999@s.whatsapp.net"}
	// The last two decoys are blank: a blank address matches nothing, not
	// even a blank value. "a\x00b@example.com" agrees with a target up to the
	// NUL both hold.
	decoys := []string{"other-support@example.com", "support@example.com.au", "\u200bsupport@example.com", "support@example.com\ufeff", "a\x00b@example.com", "support @example.com", "users/1234", "xusers/123", "Élise@example.com", "a&b@example.co", "a\x00c@example.com\x00", "support@example.com\x00x", "", "  "}
	exercised := map[string]int{}

	property := func(seed int64) bool {
		random := rand.New(rand.NewSource(seed))
		store := newTestStore(t)
		model := addressModel{}
		values := append(append(append([]string{ownerEmail}, ownerNumbers...), targets...), decoys...)
		value := func() string { return spaceAround(random, addressForm(random, values[random.Intn(len(values))])) }
		quote := func(text string) string { return ownerRuleQuote(text, random.Intn(3) == 0) }
		// beforeNUL holds the list before the NUL of each list that carries
		// bytes after one, and deep each list that nests an element deeper
		// than SQLite's JSON reader goes.
		beforeNUL, deep := map[string]string{}, map[string]bool{}

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
				switch random.Intn(11) {
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
				case 6:
					key := []string{"number", "phone", "email"}[random.Intn(3)]
					entries = append(entries, json.RawMessage(`{"name":`+quote(person)+`,"`+key+`":`+quote(form)+`,"`+key+`":`+quote(value())+`}`))
					exercised["an entry with a repeated key"]++
				case 7:
					entries = append(entries, map[string]any{"name": person, "email": form, "id": json.RawMessage([]string{"1e400", "-1e400", "1E+999"}[random.Intn(3)])})
					exercised["an entry with a number too large for a float64"]++
				default:
					entries = append(entries, map[string]any{"name": person, "number": form})
				}
			}
			ownerNumber := func() string { return spaceAround(random, ownerNumbers[random.Intn(len(ownerNumbers))]) }
			ownerForm := func() string { return spaceAround(random, addressForm(random, ownerEmail)) }
			switch random.Intn(7) {
			case 0: // the owner isn't listed
			case 1:
				entries = append(entries, map[string]any{"name": "Me", "number": ownerNumber(), "email": ownerForm(), "is_me": "true"})
				exercised["an owner's entry whose flag isn't JSON true"]++
			case 2:
				entries = append(entries, map[string]any{"name": "Me", "email": ownerForm(), "isMe": true})
			case 3:
				entries = append(entries, map[string]any{"name": "Me", "number": ownerNumber(), "email": ownerForm()})
				exercised["an unflagged copy of the owner"]++
			case 4:
				entries = append(entries, ownerNumber())
				exercised["a bare string that holds the owner's number"]++
			default:
				entries = append(entries, map[string]any{"name": "Me", "number": ownerNumber(), "email": ownerForm(), "is_me": true})
			}
			random.Shuffle(len(entries), func(i, j int) { entries[i], entries[j] = entries[j], entries[i] })
			// json.Marshal writes & as \u0026 and a NUL as \u0000, so the
			// store must decode escapes.
			participants, err := json.Marshal(entries)
			if err != nil {
				t.Fatal(err)
			}
			switch random.Intn(14) {
			case 0:
				participants = []byte("")
			case 1:
				participants = []byte(`{"number":"` + targets[0] + `"}`)
			case 2:
				participants = append([]byte("["+ownerRuleNested(1000+random.Intn(500))+","), participants[1:]...)
				deep[id] = true
				exercised["a list nested deeper than SQLite reads"]++
			case 3:
				beforeNUL[id] = string(participants)
				participants = append(participants, "\x00garbage"...)
				exercised["a list with bytes after a NUL"]++
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
		lines := modelOwnerLines(stored)
		dump := func() {
			for _, conversation := range stored {
				t.Logf("  %s ts=%d name=%q: %q", conversation.ConversationID, conversation.LastMessageTS, conversation.Name, conversation.Participants)
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

		addresses := append(append([]string{ownerEmail, ownerNumbers[1], "", "  "}, targets...), decoys[:5]...)
		for _, address := range addresses {
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
			if strings.TrimSpace(address) == "" {
				continue
			}
			oldest := int64(-1)
			if len(want) > 0 {
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
				for _, conversation := range stored {
					if conversation.ConversationID == want[len(want)-1] {
						oldest = conversation.LastMessageTS
					}
				}
			}
			for _, conversation := range stored {
				held := model.holds(conversation, address, lines)
				for path, drawn := range map[string]bool{
					"a hit through an incoming sender alone":                                                        held.senderOnly,
					"a thread that holds the address only through the owner's entries":                              held.ownerOnly,
					"a thread that holds the address only through the owner's entries, an unflagged one among them": held.ownerCopyOnly,
					"a hit whose ASCII letters' case differs from the query's":                                      held.caseDiffers,
					"a thread whose name or title alone is the address":                                             held.nameOnly,
					"a thread whose values would hold it but for a non-ASCII letter's case":                         held.otherCaseOnly,
					"a thread whose value agrees with the address up to a NUL, but not after it":                    held.nulTwinOnly,
					"a thread that an earlier value of a repeated key would put on the address":                     held.earlierValueOnly,
					"a hit through someone who holds a number too large for a float64":                              held.hit && held.hugeNumber,
					"a hit through a value with a NUL":                                                              held.hit && strings.ContainsRune(address, 0),
					"a hit in a list nested deeper than SQLite reads":                                               held.hit && !held.senderOnly && deep[conversation.ConversationID],
					"a hit behind a newer thread whose values only contain the address":                             held.containsOnly && oldest >= 0 && conversation.LastMessageTS > oldest,
					"a hit through a value with white space around it":                                              held.padded,
				} {
					if drawn {
						exercised[path]++
					}
				}
				if held.hit && strings.Contains(address, "&") {
					exercised["a hit through a JSON escape"]++
				}
				if prefix, ok := beforeNUL[conversation.ConversationID]; ok && !held.hit {
					// SQLite's reader takes the list as what comes before its NUL.
					cut := *conversation
					cut.ConversationID, cut.Participants = "\x00"+cut.ConversationID, prefix
					if model.holds(&cut, address, lines).hit {
						exercised["a thread whose list holds the address only before bytes after a NUL"]++
					}
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
		"a thread that holds the address only through the owner's entries",
		"a thread that holds the address only through the owner's entries, an unflagged one among them",
		"a hit whose ASCII letters' case differs from the query's",
		"a thread whose name or title alone is the address",
		"a thread whose values would hold it but for a non-ASCII letter's case",
		"a thread whose value agrees with the address up to a NUL, but not after it",
		"a thread that an earlier value of a repeated key would put on the address",
		"a hit through someone who holds a number too large for a float64",
		"a hit through a value with a NUL",
		"a hit in a list nested deeper than SQLite reads",
		"a hit behind a newer thread whose values only contain the address",
		"a hit through a JSON escape",
		"a thread whose list holds the address only before bytes after a NUL",
		"an entry with a repeated key",
		"an entry with a number too large for a float64",
		"a list nested deeper than SQLite reads",
		"a list with bytes after a NUL",
		"an owner's entry whose flag isn't JSON true",
		"an unflagged copy of the owner",
		"a bare string that holds the owner's number",
		"a hit through a value with white space around it",
		"an address with white space around it that has hits",
	} {
		if exercised[path] == 0 {
			t.Errorf("the property never drew %s; raise MaxCount (drew %v)", path, exercised)
		}
	}
}
