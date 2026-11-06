package db

import (
	"database/sql/driver"
	"encoding/json"
	"math/rand"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"testing/quick"
	"unicode/utf8"

	"modernc.org/sqlite"

	"github.com/maxghenis/openmessage/internal/addressmatch"
	"github.com/maxghenis/openmessage/internal/phoneline"
)

// batchPhones, batchAddresses and batchOddValues are what the threads of a
// random store hold. The phones include numbers that hold another's last ten
// digits inside them, share only its last seven, or are too short to name a
// line; the owner's number comes last. The addresses include values that hold
// another inside them, differ from it only in a non-ASCII letter's case or
// only after a NUL, or hold a byte that isn't UTF-8; the owner's address comes
// last. The odd values hold a line's digits but name no line, are blank, or
// carry a NUL after a number.
var (
	batchPhones    = []string{"12025550123", "13035550147", "15555550199", "447555012399", "4412025550123", "13035550123", "5550123", "550123", "12025550124", "15550009999"}
	batchAddresses = []string{"support@example.com", "other-support@example.com", "support@example.com.au", " support@example.com", "a&b@example.com", "élise@example.com", "Élise@example.com", "users/123", "users/1234", "abcdefab-abcd-abcd-abcd-abcdefabcdef", "72975", "nul\x00a@example.com", "nul\x00b@example.com", "bad\xff@example.com", "me@example.com"}
	batchOddValues = []string{"12025550123@s.whatsapp.net", "15555550199@lid", "alice12025550123@example.com", "users/12025550123", "+99999912025550123", "13035550147@c.us", "", " ", "\t", "+1 202 555 0123\x00x", "\x002025550123"}
)

// batchValue draws a value a participant entry or a sender may hold.
func batchValue(random *rand.Rand) string {
	switch random.Intn(10) {
	case 0, 1, 2, 3:
		return phoneLineForm(random, batchPhones[random.Intn(len(batchPhones))])
	case 4, 5, 6:
		return spaceAround(random, addressForm(random, batchAddresses[random.Intn(len(batchAddresses))]))
	default:
		return batchOddValues[random.Intn(len(batchOddValues))]
	}
}

// seedBatchStore writes random threads into store. Entries hold values under
// number, phone or email, as bare strings, as numbers that aren't text, or in
// nested arrays; some carry a blank number before a phone or an email; the
// owner's entries are flagged with JSON true under is_me or isMe, or with a
// string that isn't JSON true; a participants text may be blank, an object,
// broken JSON or JSON written by hand with a byte that isn't UTF-8, which Go's
// encoder would write as U+FFFD; incoming and outgoing messages carry senders;
// and threads often share a time.
func seedBatchStore(t *testing.T, random *rand.Rand, store *Store) {
	t.Helper()
	ownerPhone, ownerAddress := batchPhones[len(batchPhones)-1], batchAddresses[len(batchAddresses)-1]
	for index := 0; index < 4+random.Intn(12); index++ {
		id := "c" + strconv.Itoa(index)
		var entries []any
		for member := 0; member < 1+random.Intn(3); member++ {
			form := batchValue(random)
			person := "P" + strconv.Itoa(member)
			switch random.Intn(10) {
			case 0:
				entries = append(entries, form)
			case 1:
				entries = append(entries, map[string]any{"name": person, "phone": form})
			case 2:
				entries = append(entries, map[string]any{"name": person, "number": " ", "phone": form})
			case 3:
				entries = append(entries, map[string]any{"name": person, "number": form, "phone": batchValue(random)})
			case 4:
				entries = append(entries, map[string]any{"name": person, "email": form})
			case 5:
				entries = append(entries, map[string]any{"name": person, "number": " ", "email": form})
			case 6:
				number, _ := strconv.ParseInt(phoneline.Digits(form), 10, 64)
				entries = append(entries, map[string]any{"name": person, "number": number})
			case 7:
				entries = append(entries, []any{form})
			default:
				entries = append(entries, map[string]any{"name": person, "number": form})
			}
		}
		owner := phoneLineForm(random, ownerPhone)
		if random.Intn(2) == 0 {
			owner = addressForm(random, ownerAddress)
		}
		switch random.Intn(5) {
		case 0: // the owner isn't listed
		case 1:
			entries = append(entries, map[string]any{"name": "Me", "number": owner, "is_me": "true"})
		case 2:
			entries = append(entries, map[string]any{"name": "Me", "phone": owner, "isMe": true})
		default:
			entries = append(entries, map[string]any{"name": "Me", "number": owner, "is_me": true})
		}
		random.Shuffle(len(entries), func(i, j int) { entries[i], entries[j] = entries[j], entries[i] })
		participants, err := json.Marshal(entries)
		if err != nil {
			t.Fatal(err)
		}
		switch random.Intn(14) {
		case 0:
			participants = []byte("")
		case 1:
			participants = []byte(`{"number":"+12025550123"}`)
		case 2:
			participants = []byte(`[{"number":"+12025550123"`)
		case 3:
			participants = []byte("[{\"name\":\"P0\",\"number\":\"\u00a0BAD\xff@Example.com \"},\"nul\\u0000a@example.com\"]")
		}
		if err := store.UpsertConversation(&Conversation{
			ConversationID: id, Name: "Thread " + id, Participants: string(participants),
			LastMessageTS: int64(1_000_000 + 1000*random.Intn(6)), SourcePlatform: "sms",
		}); err != nil {
			t.Fatalf("seed conversation: %v", err)
		}
		for number := 0; number < random.Intn(4); number++ {
			sender := batchValue(random)
			fromMe := random.Intn(3) == 0
			if fromMe && random.Intn(2) == 0 {
				sender = owner
			}
			if err := store.UpsertMessage(&Message{
				MessageID: id + "-m" + strconv.Itoa(number), ConversationID: id, Body: "hello",
				TimestampMS: int64(1000 + number), SourcePlatform: "sms", SenderNumber: sender, IsFromMe: fromMe,
			}); err != nil {
				t.Fatalf("seed message: %v", err)
			}
		}
	}
}

// newestOfConversations is the reference for a batch: the conversations
// newest first, then in conversation ID order, each once, and at most limit
// unless limit is below 0.
func newestOfConversations(conversations []*Conversation, limit int) []*Conversation {
	sorted := slices.Clone(conversations)
	slices.SortStableFunc(sorted, func(a, b *Conversation) int {
		switch {
		case a.LastMessageTS > b.LastMessageTS:
			return -1
		case a.LastMessageTS < b.LastMessageTS:
			return 1
		}
		return strings.Compare(a.ConversationID, b.ConversationID)
	})
	newest := []*Conversation{}
	seen := map[string]bool{}
	for _, conversation := range sorted {
		if limit >= 0 && len(newest) == limit {
			break
		}
		if !seen[conversation.ConversationID] {
			seen[conversation.ConversationID] = true
			newest = append(newest, conversation)
		}
	}
	return newest
}

// article puts a or an before kind.
func article(kind string) string {
	if strings.HasPrefix(kind, "a") {
		return "an " + kind
	}
	return "a " + kind
}

// sameConversations reports whether a and b hold equal conversations in the
// same order; nil and empty count as equal.
func sameConversations(a, b []*Conversation) bool {
	if len(a) != len(b) {
		return false
	}
	for index := range a {
		if !reflect.DeepEqual(a[index], b[index]) {
			return false
		}
	}
	return true
}

// batchLine draws a line to ask for: the line of a phone the store may hold,
// or a string that is no Line, such as one with a NUL, letters or
// separators, or one too short to match.
func batchLine(random *rand.Rand) string {
	if random.Intn(4) == 0 {
		return []string{"", "5550123", "550123", "2025550123\x00junk", "\x002025550123", "abc5550123", "+1 (202) 555-0123", "99912025550123"}[random.Intn(8)]
	}
	return phoneline.Line(batchPhones[random.Intn(len(batchPhones))])
}

// batchAddress draws an address to ask for: any value a thread may hold, with
// its ASCII letters' case changed, or a blank one.
func batchAddress(random *rand.Rand) string {
	if random.Intn(8) == 0 {
		return []string{"", "  "}[random.Intn(2)]
	}
	return addressForm(random, batchValue(random))
}

// Differential, over random stores: SearchConversationsByPhoneLines and
// SearchConversationsByAddresses return exactly the newest of what
// SearchConversationsByPhoneLine or SearchConversationsByAddress returns for
// each value: the same conversations, newest first, then in conversation ID
// order, at most the limit, which each reads as its search for one value
// does. Every search for one value matches before its limit, so the newest
// of their hits are the newest threads on any value; so, as invariants, the
// batch matches each value as its own search does, under phoneline's rules
// for a line and addressmatch's key for a whole address, skips the flagged
// owner as that search does, applies the limit after matching, and keeps
// threads that share a time in ID order. Three more hold for every batch: the
// order of the values and repeats among them change nothing, and neither does
// how many addresses each query binds.
func TestBatchedSearchesReturnTheNewestOfEachValuesSearch(t *testing.T) {
	exercised := map[string]int{}
	property := func(seed int64) bool {
		random := rand.New(rand.NewSource(seed))
		store := newTestStore(t)
		seedBatchStore(t, random, store)
		stored, err := store.ListConversations(1000)
		if err != nil {
			t.Fatalf("list conversations: %v", err)
		}
		dump := func() {
			for _, conversation := range newestOfConversations(stored, -1) {
				t.Logf("  %s ts=%d: %q", conversation.ConversationID, conversation.LastMessageTS, conversation.Participants)
			}
			rows, err := store.db.Query(`SELECT conversation_id, sender_number, is_from_me FROM messages ORDER BY message_id`)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			for rows.Next() {
				var id, sender string
				var fromMe bool
				if err := rows.Scan(&id, &sender, &fromMe); err != nil {
					t.Fatal(err)
				}
				t.Logf("  message in %s from_me=%v sender=%q", id, fromMe, sender)
			}
		}
		at := map[string]int64{}
		for _, conversation := range stored {
			at[conversation.ConversationID] = conversation.LastMessageTS
		}
		// note records the paths a batch took, given each value's own hits
		// and the reference's answer.
		note := func(kind, kinds string, values []string, each [][]*Conversation, limit int, want []*Conversation) {
			withHits := 0
			var all []*Conversation
			for index, hits := range each {
				if len(hits) > 0 {
					withHits++
					if strings.Contains(values[index], "\x00") {
						exercised[article(kind)+" with a NUL that finds threads"]++
					}
					if strings.TrimSpace(values[index]) != values[index] {
						exercised[article(kind)+" with white space around it that finds threads"]++
					}
					if !utf8.ValidString(values[index]) {
						exercised[article(kind)+" with a byte that isn't UTF-8 that finds threads"]++
					}
				}
				all = append(all, hits...)
			}
			all = newestOfConversations(all, -1)
			if withHits >= 2 {
				exercised["a batch of "+kinds+" whose hits come from two or more of them"]++
			}
			if limit < 0 && len(want) > 0 {
				exercised["a batch of "+kinds+" with no limit"]++
			}
			if limit >= 1 && len(all) > limit {
				exercised["a limit that cuts a batch of "+kinds]++
				if at[all[limit].ConversationID] == at[all[limit-1].ConversationID] {
					exercised["threads that share a time across "+article(kind)+" batch's limit"]++
				}
			}
		}

		for round := 0; round < 4; round++ {
			limit := []int{-1, 0, 1, 2, 3, 5, 40}[random.Intn(7)]

			var lines []string
			for count := random.Intn(7); count > 0; count-- {
				lines = append(lines, batchLine(random))
			}
			var each [][]*Conversation
			var union []*Conversation
			for _, line := range lines {
				hits, err := store.SearchConversationsByPhoneLine(line, limit)
				if err != nil {
					t.Fatalf("SearchConversationsByPhoneLine(%q, %d): %v", line, limit, err)
				}
				each = append(each, hits)
				union = append(union, hits...)
			}
			want := newestOfConversations(union, limit)
			got, err := store.SearchConversationsByPhoneLines(lines, limit)
			if err != nil {
				t.Fatalf("SearchConversationsByPhoneLines(%q, %d): %v", lines, limit, err)
			}
			if !sameConversations(got, want) {
				dump()
				t.Logf("seed %d: lines %q (limit %d) = %v, each line's search gives %v", seed, lines, limit, conversationIDs(got), conversationIDs(want))
				return false
			}
			note("line", "lines", lines, each, limit, want)
			shuffled := slices.Clone(lines)
			for _, line := range lines {
				if random.Intn(2) == 0 {
					shuffled = append(shuffled, line)
				}
			}
			random.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
			if again, err := store.SearchConversationsByPhoneLines(shuffled, limit); err != nil || !sameConversations(again, got) {
				t.Logf("seed %d: lines %q (limit %d) = %v, but reordered and repeated as %q = %v (%v)", seed, lines, limit, conversationIDs(got), shuffled, conversationIDs(again), err)
				return false
			}

			var addresses []string
			for count := random.Intn(8); count > 0; count-- {
				addresses = append(addresses, batchAddress(random))
			}
			each, union = nil, nil
			for _, address := range addresses {
				hits, err := store.SearchConversationsByAddress(address, limit)
				if err != nil {
					t.Fatalf("SearchConversationsByAddress(%q, %d): %v", address, limit, err)
				}
				each = append(each, hits)
				union = append(union, hits...)
			}
			want = newestOfConversations(union, limit)
			got, err = store.SearchConversationsByAddresses(addresses, limit)
			if err != nil {
				t.Fatalf("SearchConversationsByAddresses(%q, %d): %v", addresses, limit, err)
			}
			if !sameConversations(got, want) {
				dump()
				t.Logf("seed %d: addresses %q (limit %d) = %v, each address's search gives %v", seed, addresses, limit, conversationIDs(got), conversationIDs(want))
				return false
			}
			note("address", "addresses", addresses, each, limit, want)
			for _, perQuery := range []int{1, 2, 3} {
				again, err := store.searchConversationsByAddresses(addresses, limit, perQuery)
				if err != nil || !sameConversations(again, got) {
					t.Logf("seed %d: addresses %q (limit %d) = %v, but %d to a query gives %v (%v)", seed, addresses, limit, conversationIDs(got), perQuery, conversationIDs(again), err)
					return false
				}
				if len(addressmatch.Keys(addresses)) > perQuery && len(got) > 0 {
					exercised["addresses split across queries"]++
				}
			}
			shuffled = slices.Clone(addresses)
			for _, address := range addresses {
				if random.Intn(2) == 0 {
					shuffled = append(shuffled, address)
				}
			}
			random.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
			if again, err := store.SearchConversationsByAddresses(shuffled, limit); err != nil || !sameConversations(again, got) {
				t.Logf("seed %d: addresses %q (limit %d) = %v, but reordered and repeated as %q = %v (%v)", seed, addresses, limit, conversationIDs(got), shuffled, conversationIDs(again), err)
				return false
			}
		}
		return true
	}
	cases := 150
	if raceDetectorEnabled {
		cases = 30
	}
	if err := quick.Check(property, &quick.Config{MaxCount: cases, Rand: rand.New(rand.NewSource(20261104))}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		"a batch of lines whose hits come from two or more of them",
		"a batch of addresses whose hits come from two or more of them",
		"a batch of lines with no limit",
		"a limit that cuts a batch of lines",
		"a limit that cuts a batch of addresses",
		"threads that share a time across a line batch's limit",
		"threads that share a time across an address batch's limit",
		"a line with a NUL that finds threads",
		"an address with a NUL that finds threads",
		"an address with white space around it that finds threads",
		"an address with a byte that isn't UTF-8 that finds threads",
		"addresses split across queries",
	} {
		if exercised[path] == 0 {
			t.Errorf("the property never drew %s (drew %v)", path, exercised)
		}
	}
}

// countedPhoneLineKeys is phoneline.SQLKeys under another name, and
// countedPhoneLineKeysCalls counts its calls.
const countedPhoneLineKeys = "om_test_counted_phone_line_keys"

var countedPhoneLineKeysCalls atomic.Int64

// countedPhoneLineKeysSQL writes a call of countedPhoneLineKeys, casting each
// argument to BLOB as phoneline.KeysSQL does.
func countedPhoneLineKeysSQL(args ...string) string {
	cast := make([]string, len(args))
	for index, arg := range args {
		cast[index] = "CAST(" + arg + " AS BLOB)"
	}
	return countedPhoneLineKeys + "(" + strings.Join(cast, ", ") + ")"
}

func init() {
	sqlite.MustRegisterDeterministicScalarFunction(countedPhoneLineKeys, -1, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		countedPhoneLineKeysCalls.Add(1)
		for _, arg := range args {
			var value string
			switch arg := arg.(type) {
			case string:
				value = arg
			case []byte:
				value = string(arg)
			}
			if strings.TrimSpace(value) == "" {
				continue
			}
			keys := phoneline.ValueKeys(value)
			if len(keys) == 0 {
				return nil, nil
			}
			encoded, err := json.Marshal(keys)
			return string(encoded), err
		}
		return nil, nil
	})
}

// The batched line search calls the Go keys function once for each
// participant entry and once for each distinct sender number at most,
// however many lines it holds and however many messages each sender sent: a
// name can find 160 numbers, and a search for each read every entry and
// sender again.
func TestSearchConversationsByPhoneLinesReadsEachValueOnce(t *testing.T) {
	store := newTestStore(t)
	conversations := []*Conversation{
		{ConversationID: "work", Name: "Work", IsGroup: true, SourcePlatform: "sms", LastMessageTS: 20,
			Participants: `[{"name":"Bob Example","number":"+15555550199"},{"name":"Dee Example","number":"+15555550188"},{"name":"Me","number":"+15550009999","is_me":true}]`},
		{ConversationID: "club", Name: "Club", IsGroup: true, SourcePlatform: "sms", LastMessageTS: 10,
			Participants: `[{"name":"Eve Example","number":"+15555550177"}]`},
	}
	var messages []*Message
	for index := 0; index < 900; index++ {
		messages = append(messages, &Message{
			MessageID: "m" + strconv.Itoa(index), Body: "hello", TimestampMS: int64(index + 1), SourcePlatform: "sms",
			ConversationID: []string{"work", "club", "work"}[index%3],
			SenderNumber:   []string{"+12025550123", "+13035550147", "+15550009999"}[index%3],
			IsFromMe:       index%3 == 2,
		})
	}
	seedSearchStore(t, store, conversations, messages, nil)

	lines := []string{"2025550123"}
	for index := 0; len(lines) < 160; index++ {
		lines = append(lines, phoneline.Line("1"+strconv.Itoa(4000000000+index)))
	}
	countedPhoneLineKeysCalls.Store(0)
	rows, err := store.db.Query(phoneLinesSearchSQL(countedPhoneLineKeysSQL), phoneline.KeysOfLines(lines), 10)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	found, err := scanConversations(rows)
	rows.Close()
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if ids := conversationIDs(found); !slices.Equal(ids, []string{"work"}) {
		t.Errorf("%d lines with 2025550123 = %v, want Work, where its number wrote", len(lines), ids)
	}
	const entries, senderNumbers = 4, 3
	if calls := countedPhoneLineKeysCalls.Load(); calls > entries+senderNumbers {
		t.Errorf("the search called the keys function %d times for %d lines, %d entries and %d sender numbers in %d messages", calls, len(lines), entries, senderNumbers, len(messages))
	}
	real, err := store.SearchConversationsByPhoneLines(lines, 10)
	if err != nil || !sameConversations(real, found) {
		t.Errorf("SearchConversationsByPhoneLines = %v (%v), but the counted query found %v", conversationIDs(real), err, conversationIDs(found))
	}
}

// countedAddressKey is addressmatch.SQLKey under another name, and
// countedAddressKeyCalls counts its calls.
const countedAddressKey = "om_test_counted_address_key"

var countedAddressKeyCalls atomic.Int64

// countedAddressKeySQL writes a call of countedAddressKey, casting its
// argument to BLOB as addressmatch.KeySQL does.
func countedAddressKeySQL(value string) string {
	return countedAddressKey + "(CAST(" + value + " AS BLOB))"
}

func init() {
	sqlite.MustRegisterDeterministicScalarFunction(countedAddressKey, 1, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		countedAddressKeyCalls.Add(1)
		value, ok := args[0].([]byte)
		if !ok {
			return nil, nil
		}
		key := addressmatch.Key(string(value))
		if key == "" {
			return nil, nil
		}
		return []byte(key), nil
	})
}

// The batched address search calls the Go key function up to three times for
// each participant entry, for its number, phone and email, and once for each
// distinct sender number at most, however many addresses it holds and however
// many messages each sender sent: a name can find 160 values, and a search for
// each read every entry and sender again.
func TestSearchConversationsByAddressesReadsEachValueOnce(t *testing.T) {
	store := newTestStore(t)
	conversations := []*Conversation{
		{ConversationID: "work", Name: "Work", IsGroup: true, SourcePlatform: "sms", LastMessageTS: 20,
			Participants: `[{"name":"Bob Example","number":"bob@example.com"},{"name":"Dee Example","email":"dee@example.com"},{"name":"Me","number":"me@example.com","is_me":true}]`},
		{ConversationID: "club", Name: "Club", IsGroup: true, SourcePlatform: "sms", LastMessageTS: 10,
			Participants: `[{"name":"Eve Example","number":"eve@example.com"}]`},
		{ConversationID: "desk", Name: "Desk", SourcePlatform: "sms", LastMessageTS: 5, Participants: `[]`},
	}
	var messages []*Message
	for index := 0; index < 900; index++ {
		messages = append(messages, &Message{
			MessageID: "m" + strconv.Itoa(index), Body: "hello", TimestampMS: int64(index + 1), SourcePlatform: "sms",
			ConversationID: []string{"desk", "club", "work"}[index%3],
			SenderNumber:   []string{"sam@example.com", "kim@example.com", "me@example.com"}[index%3],
			IsFromMe:       index%3 == 2,
		})
	}
	seedSearchStore(t, store, conversations, messages, nil)

	addresses := []string{"Bob@Example.com", "SAM@example.com "}
	for index := 0; len(addresses) < 160; index++ {
		addresses = append(addresses, "person"+strconv.Itoa(index)+"@example.com")
	}
	keys := addressmatch.Keys(addresses)
	args := make([]any, 0, len(keys)+1)
	for _, key := range keys {
		args = append(args, []byte(key))
	}
	countedAddressKeyCalls.Store(0)
	rows, err := store.db.Query(addressesSearchSQL(len(keys), countedAddressKeySQL), append(args, 10)...)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	found, err := scanConversations(rows)
	rows.Close()
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if ids := conversationIDs(found); !slices.Equal(ids, []string{"work", "desk"}) {
		t.Errorf("%d addresses with bob@example.com and sam@example.com = %v, want Work, which lists Bob, and Desk, where Sam wrote", len(addresses), ids)
	}
	const entries, senderNumbers = 4, 3
	if calls := countedAddressKeyCalls.Load(); calls > 3*entries+senderNumbers {
		t.Errorf("the search called the key function %d times for %d addresses, %d entries and %d sender numbers in %d messages", calls, len(addresses), entries, senderNumbers, len(messages))
	}
	real, err := store.SearchConversationsByAddresses(addresses, 10)
	if err != nil || !sameConversations(real, found) {
		t.Errorf("SearchConversationsByAddresses = %v (%v), but the counted query found %v", conversationIDs(real), err, conversationIDs(found))
	}
}

// A batch compares what the search for one address compares: every byte of a
// value's key. Each row names the threads that its addresses must find: white
// space around a value or an address and its ASCII letters' case don't matter,
// but a non-ASCII letter's case, a byte after a NUL and a byte that isn't UTF-8
// do, and a value that only holds an address matches nothing. The reads run
// through entries and senders, so both arms stay under test.
func TestSearchConversationsByAddressesComparesEveryByteOfTheKey(t *testing.T) {
	store := newTestStore(t)
	conversations := []*Conversation{
		{ConversationID: "padded", Name: "Padded", SourcePlatform: "sms", LastMessageTS: 90,
			Participants: `[{"name":"Sue","number":"\u00a0Support@Example.COM\t"}]`},
		{ConversationID: "holds-it", Name: "Holds it", SourcePlatform: "sms", LastMessageTS: 85,
			Participants: `[{"name":"Other","number":"other-support@example.com"}]`},
		{ConversationID: "nul-a", Name: "NUL a", SourcePlatform: "sms", LastMessageTS: 80,
			Participants: `[{"name":"A","email":"NUL\u0000a@example.com"}]`},
		{ConversationID: "nul-b", Name: "NUL b", SourcePlatform: "sms", LastMessageTS: 70,
			Participants: `["nul\u0000b@example.com"]`},
		{ConversationID: "raw-entry", Name: "Raw entry", SourcePlatform: "sms", LastMessageTS: 60,
			Participants: "[{\"name\":\"Raw\",\"phone\":\" RAW\xff@example.com\"}]"},
		{ConversationID: "raw-sender", Name: "Raw sender", SourcePlatform: "sms", LastMessageTS: 50, Participants: `[]`},
		{ConversationID: "accent", Name: "Accent", SourcePlatform: "sms", LastMessageTS: 40,
			Participants: `[{"name":"Élise","number":"Élise@example.com"}]`},
		{ConversationID: "owner", Name: "Owner", SourcePlatform: "sms", LastMessageTS: 30,
			Participants: `[{"name":"Me","number":"me@example.com","is_me":true}]`},
	}
	messages := []*Message{
		{MessageID: "raw", ConversationID: "raw-sender", Body: "hello", TimestampMS: 1, SourcePlatform: "sms", SenderNumber: "\u3000Sender\xfe@example.com"},
		{MessageID: "from-me", ConversationID: "owner", Body: "hello", TimestampMS: 2, SourcePlatform: "sms", SenderNumber: "me@example.com", IsFromMe: true},
	}
	seedSearchStore(t, store, conversations, messages, nil)
	for _, c := range []struct {
		addresses []string
		want      []string
	}{
		{[]string{"support@example.com", "nobody@example.com"}, []string{"padded"}},
		{[]string{"\tSUPPORT@example.com\u2003", " support@example.com"}, []string{"padded"}},
		{[]string{"example.com", "support", "nobody@example.com"}, []string{}},
		{[]string{"nul\x00a@example.com", "nobody@example.com"}, []string{"nul-a"}},
		{[]string{"NUL\x00B@example.com", "nobody@example.com"}, []string{"nul-b"}},
		{[]string{"nul\x00a@example.com", "nul\x00b@example.com"}, []string{"nul-a", "nul-b"}},
		{[]string{"nul", "nul\x00", "nul\x00c@example.com"}, []string{}},
		{[]string{"raw\xff@example.com", "nobody@example.com"}, []string{"raw-entry"}},
		{[]string{"raw\xfe@example.com", "raw\ufffd@example.com"}, []string{}},
		{[]string{"sender\xfe@example.com", "nobody@example.com"}, []string{"raw-sender"}},
		{[]string{"ÉLISE@EXAMPLE.com", "nobody@example.com"}, []string{"accent"}},
		{[]string{"élise@example.com", "éLISE@example.com"}, []string{}},
		{[]string{"me@example.com", "ME@example.com "}, []string{}},
		{[]string{"", " ", "\u00a0"}, []string{}},
	} {
		var union []*Conversation
		for _, address := range c.addresses {
			hits, err := store.SearchConversationsByAddress(address, 10)
			if err != nil {
				t.Fatalf("SearchConversationsByAddress(%q): %v", address, err)
			}
			union = append(union, hits...)
		}
		if ids := conversationIDs(newestOfConversations(union, 10)); !slices.Equal(ids, c.want) {
			t.Errorf("the searches for each of %q find %v, want %v", c.addresses, ids, c.want)
		}
		got, err := store.SearchConversationsByAddresses(c.addresses, 10)
		if err != nil {
			t.Fatalf("SearchConversationsByAddresses(%q): %v", c.addresses, err)
		}
		if ids := conversationIDs(got); !slices.Equal(ids, c.want) {
			t.Errorf("SearchConversationsByAddresses(%q) = %v, want %v", c.addresses, ids, c.want)
		}
	}
}

// The batches take as many values as a caller gives them. Address keys bind one
// value each, so a query holds the cap's worth at most, and past the cap each
// group runs as its own query; line keys bind as one JSON array. This runs the
// real cap through real bound values, in one query and across groups: the tool
// sends far fewer, but a store's search shouldn't hold a limit of its own.
func TestBatchesTakeThousandsOfValues(t *testing.T) {
	store := newTestStore(t)
	total := maxAddressesPerQuery*5/2 + 1
	addresses, lines := make([]string, total), make([]string, total)
	for index := range addresses {
		addresses[index] = "person" + strconv.Itoa(index) + "@example.com"
		lines[index] = "20255" + strconv.Itoa(10000+index)
	}
	entry := func(index int) string {
		return `[{"name":"P","number":"` + addresses[index] + `"},{"name":"Q","number":"+1` + lines[index] + `"}]`
	}
	seedSearchStore(t, store, []*Conversation{
		{ConversationID: "other", Name: "Other", SourcePlatform: "sms", LastMessageTS: 40, Participants: `[{"name":"O","number":"nobody@example.com"},{"name":"R","number":"+13035550147"}]`},
		{ConversationID: "first", Name: "First", SourcePlatform: "sms", LastMessageTS: 30, Participants: entry(0)},
		{ConversationID: "middle", Name: "Middle", SourcePlatform: "sms", LastMessageTS: 20, Participants: entry(total / 2)},
		{ConversationID: "last", Name: "Last", SourcePlatform: "sms", LastMessageTS: 10, Participants: entry(total - 1)},
	}, nil, nil)
	for _, c := range []struct {
		count, limit int
		want         []string
	}{
		{maxAddressesPerQuery, 10, []string{"first"}},
		{maxAddressesPerQuery + 1, 10, []string{"first"}},
		{total, 10, []string{"first", "middle", "last"}},
		{total, 2, []string{"first", "middle"}},
		{total, 1, []string{"first"}},
	} {
		byAddress, err := store.SearchConversationsByAddresses(addresses[:c.count], c.limit)
		if err != nil {
			t.Fatalf("%d addresses at limit %d: %v", c.count, c.limit, err)
		}
		if ids := conversationIDs(byAddress); !slices.Equal(ids, c.want) {
			t.Errorf("%d addresses at limit %d = %v, want %v", c.count, c.limit, ids, c.want)
		}
		byLine, err := store.SearchConversationsByPhoneLines(lines[:c.count], c.limit)
		if err != nil {
			t.Fatalf("%d lines at limit %d: %v", c.count, c.limit, err)
		}
		if ids := conversationIDs(byLine); !slices.Equal(ids, c.want) {
			t.Errorf("%d lines at limit %d = %v, want %v", c.count, c.limit, ids, c.want)
		}
	}
}
