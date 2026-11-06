package tools

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"testing/quick"
	"time"
	"unicode/utf8"

	"github.com/maxghenis/openmessage/internal/addressmatch"
	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/phoneline"
	"github.com/maxghenis/openmessage/internal/readsource"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
	"github.com/maxghenis/openmessage/internal/v2read"
)

// findRouteConversationsOneByOne is findRouteConversations as it stood
// before it batched its lookups (#267's head, d03c6a382), kept verbatim as
// the reference for the differential below: it searches reads once for each
// line and each address that people's cards and threads hold.
func findRouteConversationsOneByOne(reads readsource.ReadSource, people *db.Store, query string, limit int) ([]*db.Conversation, error) {
	if limit <= 0 {
		limit = 20
	}

	seen := make(map[string]*db.Conversation)
	addConversation := func(conv *db.Conversation) {
		if conv == nil {
			return
		}
		seen[conv.ConversationID] = conv
	}
	addConversations := func(convos []*db.Conversation) {
		for _, conv := range convos {
			addConversation(conv)
		}
	}
	// Only an exact ID names a conversation here. The v2 read source also
	// resolves a platform's thread ID (#155), but a stale Google thread
	// number can name another person's thread now (#177), and this tool says
	// where to send.
	if conv, err := reads.GetConversation(query); err == nil && conv != nil && conv.ConversationID == query {
		addConversation(conv)
	}

	// search finds text's conversations in reads. A phone number names its
	// line, which both stores match as send_message matches a recipient: by the
	// last ten digits of each value that reads as a number, or as a WhatsApp
	// JID of one, since v2 stores numbers in E.164 form while the legacy store
	// and address books keep them as people wrote them. A run of digits inside
	// another number names someone else (5550123 sits inside +447555012399),
	// and so do digits inside an email address, a Signal UUID or a name. Most
	// threads list the account's owner, and a thread that a query matches only
	// through the owner says nothing about whom to send to. Both stores'
	// searches skip the owner before their limit: v2's skips the owner's
	// identity, and the legacy store's skips participant entries with the
	// owner's flag and the owner's own messages.
	search := func(text string) ([]*db.Conversation, error) {
		if looksLikePhoneNumber(text) {
			return reads.SearchConversationsByPhoneLine(phoneline.Line(text), limit)
		}
		return reads.SearchConversationsByMetadata(text, limit)
	}
	convos, err := search(query)
	if err != nil {
		return nil, err
	}
	addConversations(convos)
	// The line search finds every thread on a number's line. An address-book
	// card or a participant whose number only contains the query's digits
	// names someone else, so a number goes no further.
	if looksLikePhoneNumber(query) {
		return newestRoutes(seen, limit), nil
	}

	// lookUp finds the threads of the person a number names. A phone number
	// names a line. Any other value names whoever holds it as a whole
	// address, with ASCII letters' case and the white space around it aside,
	// as addressmatch.Key writes it: the legacy Google Chat importer files an
	// email address as a number, SMS threads list a short code as one, and
	// another person's address or phone number, or a title, can hold either,
	// so a search for the text would route to them.
	// searched holds each line and address that lookUp has asked reads about,
	// so reads answer each lookup once. The address book and legacy threads
	// often hold one person's number, each in its own form, and every form
	// names one line. Two values share an address key exactly when the
	// stores' address searches can't tell them apart. The query arrives as
	// JSON text, so it holds valid UTF-8, and its own search matches every
	// thread that an address lookup of the query would, in the same order and
	// under the same limit. That lookup could add only threads that
	// newestRoutes drops, so it counts as done.
	searched := map[string]bool{"address:" + addressmatch.Key(query): true}
	lookUp := func(number string) {
		number = strings.TrimSpace(number)
		if number == "" {
			return
		}
		byLine := looksLikePhoneNumber(number)
		key := "address:" + addressmatch.Key(number)
		if byLine {
			key = "line:" + phoneline.Line(number)
		}
		if searched[key] {
			return
		}
		searched[key] = true
		var found []*db.Conversation
		var err error
		if byLine {
			found, err = reads.SearchConversationsByPhoneLine(phoneline.Line(number), limit)
		} else {
			found, err = reads.SearchConversationsByAddress(number, limit)
		}
		if err == nil {
			addConversations(found)
		}
	}

	contacts, err := people.ListContacts(query, limit)
	if err == nil {
		for _, contact := range contacts {
			if contact != nil {
				lookUp(contact.Number)
			}
		}
	}

	// A legacy thread can hold a name that reads lack, so its participants
	// count even when a title or a card matched: a title that holds a name
	// says nothing about where that person's own thread is.
	threadContacts, err := people.ListContactsFromConversations(query, limit)
	if err == nil {
		for _, contact := range threadContacts {
			if contact != nil {
				lookUp(contact.Number)
			}
		}
	}
	return newestRoutes(seen, limit), nil
}

// routeBatchFirstNames and routeBatchLastNames name the people of a random
// route store. First names repeat across people, and some hold others
// inside them (Alex and Alexa, Ann and Anna), so a first name finds several
// people, as a common first name does in a live address book.
var (
	routeBatchFirstNames = []string{"Alex", "Alexa", "Sam", "Ann", "Anna", "Dee"}
	routeBatchLastNames  = []string{"Kim", "Ruiz", "Chen", "Okafor", "Smith", "Lee", "Park"}
)

// routeBatchOwner is the owner of every random route store.
const (
	routeBatchOwnerName   = "Me"
	routeBatchOwnerNumber = "+15550009999"
	routeBatchOwnerEmail  = "me@example.com"
)

// routeBatchValue is a value a person holds and the kind v2 gives it.
type routeBatchValue struct {
	value, kind string
}

// routeBatchPerson is one person of a random route store.
type routeBatchPerson struct {
	name   string
	values []routeBatchValue
}

// routeBatchPhoneForm writes an E.164 number as an address book or a
// transport might: as it is, national with separators, or as a WhatsApp
// JID.
func routeBatchPhoneForm(random *rand.Rand, e164 string) string {
	digits := phoneline.Digits(e164)
	switch random.Intn(4) {
	case 0:
		return "(" + digits[len(digits)-10:len(digits)-7] + ") " + digits[len(digits)-7:len(digits)-4] + "-" + digits[len(digits)-4:]
	case 1:
		return digits[:len(digits)-10] + " " + digits[len(digits)-10:len(digits)-7] + "." + digits[len(digits)-7:len(digits)-4] + "." + digits[len(digits)-4:]
	case 2:
		return digits + "@s.whatsapp.net"
	}
	return e164
}

// routeBatchFlipCase writes value with the case of some of its ASCII letters
// flipped.
func routeBatchFlipCase(random *rand.Rand, value string) string {
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

// routeBatchWritten writes a value as the legacy store or an address book
// keeps it: a phone number in some written form, any other value with its
// ASCII letters' case changed and, some of the time, white space around it.
func routeBatchWritten(random *rand.Rand, value routeBatchValue) string {
	if value.kind == "e164" {
		return routeBatchPhoneForm(random, value.value)
	}
	written := routeBatchFlipCase(random, value.value)
	if random.Intn(4) == 0 {
		spaces := []string{"", " ", "\t", "\u0085", "\u00a0", "\u2028", "\u3000"}
		written = spaces[random.Intn(len(spaces))] + written + spaces[random.Intn(len(spaces))]
	}
	return written
}

// seedRouteBatchStores writes random people into the legacy store, which
// serves as people and as legacy reads, and into the v2 store. People hold
// phone numbers, some shared and some that hold another's digits inside
// them, email addresses, some that differ only after a NUL or hold a byte that
// isn't UTF-8, an SMS short code, a Signal UUID or a WhatsApp LID.
// The address book files cards for some of their values, in written forms;
// legacy threads list people with the owner flagged, carry titles that hold
// names, and have incoming senders, including some whom no roster lists and
// the owner's own messages; v2 threads list people's identities and, most of
// the time, the owner's; and threads in both stores often share a time.
func seedRouteBatchStores(t *testing.T, random *rand.Rand, legacy *db.Store, store *sqlite.Store) []routeBatchPerson {
	t.Helper()
	sharedPhones := []string{"+12025550123", "+13035550147", "+447555012399", "+12025550124"}
	var people []routeBatchPerson
	for index := 0; index < 3+random.Intn(10); index++ {
		first := routeBatchFirstNames[random.Intn(len(routeBatchFirstNames))]
		last := routeBatchLastNames[random.Intn(len(routeBatchLastNames))]
		person := routeBatchPerson{name: first + " " + last}
		for count := 1 + random.Intn(3); count > 0; count-- {
			switch random.Intn(10) {
			case 8:
				person.values = append(person.values, routeBatchValue{[]string{"nul\x00a@example.com", "nul\x00b@example.com", "bad\xff@example.com"}[random.Intn(3)], "email"})
			case 0, 1, 2:
				person.values = append(person.values, routeBatchValue{fmt.Sprintf("+1202555%04d", random.Intn(40)), "e164"})
			case 3:
				person.values = append(person.values, routeBatchValue{sharedPhones[random.Intn(len(sharedPhones))], "e164"})
			case 4, 5:
				person.values = append(person.values, routeBatchValue{strings.ToLower(first+"."+last) + strconv.Itoa(random.Intn(3)) + "@example.com", "email"})
			case 6:
				person.values = append(person.values, routeBatchValue{[]string{"72975", "33733"}[random.Intn(2)], "phone"})
			case 7:
				person.values = append(person.values, routeBatchValue{fmt.Sprintf("abcdefab-abcd-abcd-abcd-%012d", random.Intn(4)), "signal_aci"})
			default:
				person.values = append(person.values, routeBatchValue{fmt.Sprintf("%d@lid", 15555550100+random.Intn(4)), "whatsapp_lid"})
			}
		}
		people = append(people, person)
	}

	for index, person := range people {
		for _, value := range person.values {
			if random.Intn(2) == 0 {
				name := person.name
				if random.Intn(4) == 0 {
					name += " (work)"
				}
				if err := legacy.UpsertContact(&db.Contact{ContactID: fmt.Sprintf("card-%d-%s", index, value.value), Name: name, Number: routeBatchWritten(random, value)}); err != nil {
					t.Fatalf("seed card: %v", err)
				}
			}
		}
	}
	if random.Intn(3) == 0 {
		if err := legacy.UpsertContact(&db.Contact{ContactID: "card-owner", Name: routeBatchOwnerName, Number: routeBatchOwnerNumber}); err != nil {
			t.Fatalf("seed the owner's card: %v", err)
		}
	}

	now := time.Now().UnixMilli()
	if err := store.UpsertAccount(sqlite.Account{
		AccountID: routeFixtureAccount, BridgeKey: "google_messages", DisplayName: "Google Messages",
		Mode: sqlite.AccountModeLive, Enabled: true, ConfigJSON: "{}", CreatedAtMS: now, UpdatedAtMS: now,
	}); err != nil {
		t.Fatalf("UpsertAccount(): %v", err)
	}
	identities := map[string]string{}
	identity := func(value routeBatchValue, name string, self bool) string {
		key := value.kind + "|" + value.value
		if id, ok := identities[key]; ok {
			return id
		}
		id := fmt.Sprintf("identity-%03d", len(identities))
		if err := store.UpsertIdentity(sqlite.Identity{
			IdentityID: id, AccountID: routeFixtureAccount, Kind: sqlite.IdentityKind(value.kind),
			CanonicalValue: value.value, RawValue: value.value, DisplayName: name, IsSelf: self,
			MetadataJSON: "{}", CreatedAtMS: now, UpdatedAtMS: now,
		}); err != nil {
			t.Fatalf("UpsertIdentity(%q): %v", value.value, err)
		}
		identities[key] = id
		return id
	}
	selfID := identity(routeBatchValue{routeBatchOwnerNumber, "e164"}, routeBatchOwnerName, true)
	identity(routeBatchValue{routeBatchOwnerEmail, "email"}, routeBatchOwnerName, true)

	for index := 0; index < 3+random.Intn(12); index++ {
		var members []routeBatchPerson
		for _, member := range random.Perm(len(people))[:1+random.Intn(min(3, len(people)))] {
			members = append(members, people[member])
		}
		at := now - int64(1000*random.Intn(6))
		title := ""
		switch random.Intn(4) {
		case 0:
			title = "Dinner with " + members[0].name[:strings.Index(members[0].name, " ")]
		case 1:
			title = fmt.Sprintf("Group %d", index)
		}

		var entries []map[string]any
		for _, member := range members {
			value := member.values[random.Intn(len(member.values))]
			entries = append(entries, map[string]any{"name": member.name, "number": routeBatchWritten(random, value)})
		}
		entries = append(entries, map[string]any{"name": routeBatchOwnerName, "number": routeBatchOwnerNumber, "is_me": true})
		participants, err := json.Marshal(entries)
		if err != nil {
			t.Fatal(err)
		}
		legacyID := fmt.Sprintf("legacy-%02d", index)
		name := title
		if name == "" {
			name = members[0].name
		}
		if err := legacy.UpsertConversation(&db.Conversation{
			ConversationID: legacyID, Name: name, IsGroup: len(members) > 1, Participants: string(participants),
			LastMessageTS: at, SourcePlatform: "sms",
		}); err != nil {
			t.Fatalf("seed legacy thread: %v", err)
		}
		for number := 0; number < random.Intn(3); number++ {
			sender := members[random.Intn(len(members))]
			message := &db.Message{
				MessageID: fmt.Sprintf("%s-m%d", legacyID, number), ConversationID: legacyID, Body: "hello",
				TimestampMS: at - int64(number), SourcePlatform: "sms",
				SenderName: sender.name, SenderNumber: routeBatchWritten(random, sender.values[random.Intn(len(sender.values))]),
			}
			switch random.Intn(4) {
			case 0:
				message.IsFromMe, message.SenderName, message.SenderNumber = true, routeBatchOwnerName, routeBatchOwnerNumber
			case 1:
				stranger := people[random.Intn(len(people))]
				message.SenderName, message.SenderNumber = stranger.name, routeBatchWritten(random, stranger.values[0])
			}
			if err := legacy.UpsertMessage(message); err != nil {
				t.Fatalf("seed legacy message: %v", err)
			}
		}

		v2ID := fmt.Sprintf("v2-%02d", index)
		kind := sqlite.ConversationKindDirect
		if len(members) > 1 {
			kind = sqlite.ConversationKindGroup
		}
		if err := store.UpsertConversation(sqlite.Conversation{
			ConversationID: v2ID, AccountID: routeFixtureAccount, RemoteConversationID: "remote-" + v2ID,
			Kind: kind, Title: title, NotificationMode: sqlite.NotificationModeAll, MetadataJSON: "{}",
			LastMessageAtMS: at, CreatedAtMS: now, UpdatedAtMS: now,
		}); err != nil {
			t.Fatalf("UpsertConversation(%s): %v", v2ID, err)
		}
		var rows []sqlite.ConversationParticipant
		seen := map[string]bool{}
		for _, member := range members {
			name := member.name
			if random.Intn(4) == 0 {
				name = ""
			}
			id := identity(member.values[random.Intn(len(member.values))], name, false)
			if seen[id] {
				continue
			}
			seen[id] = true
			rows = append(rows, sqlite.ConversationParticipant{AccountID: routeFixtureAccount, ConversationID: v2ID, IdentityID: id, Role: sqlite.ParticipantRoleMember, IsActive: true})
		}
		if random.Intn(4) != 0 {
			rows = append(rows, sqlite.ConversationParticipant{AccountID: routeFixtureAccount, ConversationID: v2ID, IdentityID: selfID, Role: sqlite.ParticipantRoleMember, IsActive: true})
		}
		if err := store.ReplaceConversationParticipants(v2ID, rows); err != nil {
			t.Fatalf("ReplaceConversationParticipants(%s): %v", v2ID, err)
		}
	}
	return people
}

// failingBatchRouteReads fails the batched searches that its flags name, as
// a store that can't run them would, and passes every other read through.
type failingBatchRouteReads struct {
	readsource.ReadSource
	failLines, failAddresses bool
}

func (r *failingBatchRouteReads) SearchConversationsByPhoneLines(lines []string, limit int) ([]*db.Conversation, error) {
	if r.failLines {
		return nil, errors.New("the batched line search failed")
	}
	return r.ReadSource.SearchConversationsByPhoneLines(lines, limit)
}

func (r *failingBatchRouteReads) SearchConversationsByAddresses(addresses []string, limit int) ([]*db.Conversation, error) {
	if r.failAddresses {
		return nil, errors.New("the batched address search failed")
	}
	return r.ReadSource.SearchConversationsByAddresses(addresses, limit)
}

// mapKeys returns counts' keys.
func mapKeys(counts map[string]int) []string {
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	return keys
}

// sameRoutes reports whether a and b hold equal conversations in the same
// order.
func sameRoutes(a, b []*db.Conversation) bool {
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

// Differential, over random stores on both serving stores:
// findRouteConversations, which asks reads once for all of a name's lines
// and once for all its addresses, returns exactly what
// findRouteConversationsOneByOne returns by searching for each value: the
// same conversations, with the same fields, in the same order. So the
// batches keep every rule that each value's search applies, phoneline's for
// a line and addressmatch's key for a whole address, the owner skipped as each
// store skips it, matching before the limit, and newest first with ties in
// conversation ID order. It returns the same when a batch fails and the
// tool falls back to a search for each value. Two counts hold too: the tool
// asks reads for each line and each address once, and runs one query of
// each kind at most, a batch for two or more values and the search for a
// lone one.
func TestFindRouteConversationsBatchesAgreeWithASearchForEachValue(t *testing.T) {
	exercised := map[string]int{}
	property := func(seed int64) bool {
		random := rand.New(rand.NewSource(seed))
		a := testApp(t)
		store := openRouteV2Store(t)
		people := seedRouteBatchStores(t, random, a.Store, store)

		queries := []string{"Alex", "Ann", "alex", "Sam Lee", "Dee", "Nobody", routeBatchOwnerName, "a", "72975", "555", "(202) 555-0123", "+1 202 555 0001"}
		for _, person := range people[:min(2, len(people))] {
			queries = append(queries, person.name, routeBatchWritten(random, person.values[0]))
		}
		for _, mode := range routeStoreModes {
			var reads readsource.ReadSource = a.Store
			if mode == "v2" {
				reads = v2read.New(store)
			}
			for _, query := range queries {
				limit := []int{1, 2, 3, 5, 8, 80}[random.Intn(6)]
				want, err := findRouteConversationsOneByOne(reads, a.Store, query, limit)
				if err != nil {
					t.Fatalf("%s: findRouteConversationsOneByOne(%q, %d): %v", mode, query, limit, err)
				}
				counting := newCountingRouteReads(reads)
				got, err := findRouteConversations(counting, a.Store, query, limit)
				if err != nil {
					t.Fatalf("%s: findRouteConversations(%q, %d): %v", mode, query, limit, err)
				}
				if !sameRoutes(got, want) {
					t.Logf("seed %d, %s: routes for %q (limit %d) = %v, a search for each value gives %v", seed, mode, query, limit, routeIDs(got), routeIDs(want))
					return false
				}
				for value, count := range counting.lines {
					if count != 1 {
						t.Logf("seed %d, %s: for %q, asked reads for line %q %d times", seed, mode, query, value, count)
						return false
					}
				}
				for value, count := range counting.addresses {
					if count != 1 {
						t.Logf("seed %d, %s: for %q, asked reads for address %q %d times", seed, mode, query, value, count)
						return false
					}
				}
				if counting.queries["lines"]+counting.queries["line"] > 1 || counting.queries["addresses"]+counting.queries["address"] > 1 ||
					(counting.queries["lines"] == 1) != (len(counting.lines) > 1) || (counting.queries["addresses"] == 1) != (len(counting.addresses) > 1) {
					t.Logf("seed %d, %s: for %q, queries %v for %d lines and %d addresses", seed, mode, query, counting.queries, len(counting.lines), len(counting.addresses))
					return false
				}

				failing := &failingBatchRouteReads{ReadSource: reads, failLines: random.Intn(2) == 0, failAddresses: random.Intn(2) == 0}
				fallback, err := findRouteConversations(failing, a.Store, query, limit)
				if err != nil {
					t.Fatalf("%s: findRouteConversations(%q, %d) with failing batches: %v", mode, query, limit, err)
				}
				if !sameRoutes(fallback, want) {
					t.Logf("seed %d, %s: with batches failing (lines %v, addresses %v), routes for %q (limit %d) = %v, a search for each value gives %v", seed, mode, failing.failLines, failing.failAddresses, query, limit, routeIDs(fallback), routeIDs(want))
					return false
				}

				if counting.queries["lines"] == 1 && counting.queries["addresses"] == 1 {
					exercised[mode+": a query that asks for both batches"]++
				}
				if len(counting.lines) >= 2 && counting.queries["lines"] == 1 {
					exercised[mode+": a batch of two or more lines"]++
				}
				if len(counting.addresses) >= 2 && counting.queries["addresses"] == 1 {
					exercised[mode+": a batch of two or more addresses"]++
				}
				if !looksLikePhoneNumber(query) && len(counting.lines) == 1 && counting.queries["line"] == 1 {
					exercised[mode+": a lone line searched alone"]++
				}
				if len(counting.addresses) == 1 && counting.queries["address"] == 1 {
					exercised[mode+": a lone address searched alone"]++
				}
				for value := range counting.addresses {
					if strings.Contains(value, "\x00") {
						exercised[mode+": an address with a NUL to look up"]++
					}
					if !utf8.ValidString(value) {
						exercised[mode+": an address with a byte that isn't UTF-8 to look up"]++
					}
				}
				if len(addressmatch.Keys(append(mapKeys(counting.addresses), query))) < len(counting.addresses)+1 {
					t.Logf("seed %d, %s: for %q, asked reads for addresses that share a key: %q", seed, mode, query, mapKeys(counting.addresses))
					return false
				}
				if all, err := findRouteConversationsOneByOne(reads, a.Store, query, 1000); err == nil && len(all) > limit && len(want) == limit {
					exercised[mode+": a limit that cuts the routes"]++
				}
				if (failing.failLines && counting.queries["lines"] == 1) || (failing.failAddresses && counting.queries["addresses"] == 1) {
					exercised[mode+": a fallback to a search for each value"]++
				}
			}
		}
		return true
	}
	cases := 60
	if raceDetectorEnabled {
		cases = 12
	}
	if err := quick.Check(property, &quick.Config{MaxCount: cases, Rand: rand.New(rand.NewSource(20261104))}); err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, mode := range routeStoreModes {
		for _, path := range []string{"a query that asks for both batches", "a batch of two or more lines", "a batch of two or more addresses", "a lone line searched alone", "a lone address searched alone", "a limit that cuts the routes", "a fallback to a search for each value", "an address with a NUL to look up", "an address with a byte that isn't UTF-8 to look up"} {
			paths = append(paths, mode+": "+path)
		}
	}
	sort.Strings(paths)
	for _, path := range paths {
		if exercised[path] == 0 {
			t.Errorf("the property never drew %s (drew %v)", path, exercised)
		}
	}
}
