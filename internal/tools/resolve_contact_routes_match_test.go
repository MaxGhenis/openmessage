package tools

import (
	"fmt"
	"path/filepath"
	"slices"
	"testing"

	"github.com/maxghenis/openmessage/internal/app"
	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/readsource"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
	"github.com/maxghenis/openmessage/internal/v2read"
)

// routeStoreModes are the stores that can serve the route tool's reads.
var routeStoreModes = []string{"legacy", "v2"}

func openRouteV2Store(t *testing.T) *sqlite.Store {
	t.Helper()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "v2.sqlite3"))
	if err != nil {
		t.Fatalf("sqlite.Open(): %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// routeReadsFor returns the reads that mode serves and the route tool's
// options for them.
func routeReadsFor(t *testing.T, a *app.App, store *sqlite.Store, mode string) (readsource.ReadSource, Options) {
	t.Helper()
	if mode == "legacy" {
		return a.Store, Options{Reads: a.Store, Daemon: connectedDaemon(t)}
	}
	reads := v2read.New(store)
	return reads, Options{Reads: reads, V2Primary: true, Daemon: connectedDaemon(t)}
}

func routeIDs(convos []*db.Conversation) []string {
	ids := make([]string, 0, len(convos))
	for _, conv := range convos {
		ids = append(ids, conv.ConversationID)
	}
	return ids
}

// A participant entry is a JSON object, and its keys name no one. The
// default fixture owner is named Me, as the demo store's owner is, and "me"
// sits inside every entry's "name" key, so a search for Me must match no
// one's entry through that key. Names and numbers still match.
func TestResolveContactRoutesMatchesParticipantValuesNotJSONKeys(t *testing.T) {
	for _, mode := range routeStoreModes {
		t.Run(mode, func(t *testing.T) {
			a := testApp(t)
			store := openRouteV2Store(t)
			threads := seedRouteFixture(t, a.Store, store, []routeFixturePerson{
				{Name: "Bob Example", Number: "+15555550123", LegacyThreadID: "bob", RemoteThreadID: "bob"},
			})
			reads, _ := routeReadsFor(t, a, store, mode)
			bob := threads["Bob Example"]
			if mode == "legacy" {
				bob = "bob"
			}
			for _, query := range []string{"Me", "me", "name", "number", "is_me", `"name"`} {
				got, err := findRouteConversations(reads, a.Store, query, 80)
				if err != nil {
					t.Fatalf("findRouteConversations(%q): %v", query, err)
				}
				if len(got) != 0 {
					t.Errorf("routes for %q = %v, want none: only a JSON key or the owner's entry holds it", query, routeIDs(got))
				}
			}
			for _, query := range []string{"Bob", "Example", "+15555550123"} {
				got, err := findRouteConversations(reads, a.Store, query, 80)
				if err != nil {
					t.Fatalf("findRouteConversations(%q): %v", query, err)
				}
				if len(got) != 1 || got[0].ConversationID != bob {
					t.Errorf("routes for %q = %v, want Bob's thread %q only", query, routeIDs(got), bob)
				}
			}
		})
	}
}

// A number names a phone line by its last ten digits, as send_message reads
// a recipient, and never by a run of digits inside another number. 5550123
// ends Alice's number but sits inside Bob's newer one, so it routes to Alice
// only, the thread send_message picks, and at limit 1 it still prefers her.
func TestResolveContactRoutesMatchesANumberByItsLineNotAsASubstring(t *testing.T) {
	for _, mode := range routeStoreModes {
		t.Run(mode, func(t *testing.T) {
			a := testApp(t)
			store := openRouteV2Store(t)
			threads := seedRouteFixture(t, a.Store, store, []routeFixturePerson{
				{Name: "Bob Example", Number: "+447555012399", LegacyThreadID: "bob", RemoteThreadID: "bob"},
				{Name: "Alice Example", Number: "+12025550123", LegacyThreadID: "alice", RemoteThreadID: "alice"},
			})
			reads, options := routeReadsFor(t, a, store, mode)
			alice := threads["Alice Example"]
			if mode == "legacy" {
				alice = "alice"
			}
			for _, query := range []string{"5550123", "555-0123", "+12025550123", "(202) 555-0123"} {
				direct, err := findDirectSMSConversation(reads, query)
				if err != nil || direct == nil || direct.ConversationID != alice {
					t.Fatalf("send_message's thread for %q = %+v (%v), want Alice's %q", query, direct, err, alice)
				}
				got, err := findRouteConversations(reads, a.Store, query, 80)
				if err != nil {
					t.Fatalf("findRouteConversations(%q): %v", query, err)
				}
				if len(got) != 1 || got[0].ConversationID != alice {
					t.Errorf("routes for %q = %v, want Alice's thread %q only", query, routeIDs(got), alice)
				}
				matches := routeMatches(t, callRouteTool(t, resolveContactRoutesHandler(a, options), map[string]any{"query": query, "limit": 1}))
				if len(matches) != 1 || matches[0].PreferredReplyConversationID != alice {
					t.Errorf("limit-1 routes for %q = %+v, want one that prefers Alice's thread %q", query, matches, alice)
				}
			}
		})
	}
}

// The owner's own card can hold part of the owner's number. That fragment
// names a line only where a number ends with it, so the owner's name doesn't
// route to Bob, whose number holds the fragment mid-number.
func TestResolveContactRoutesDoesNotRouteTheOwnerThroughAFragmentOfTheirNumber(t *testing.T) {
	for _, mode := range routeStoreModes {
		t.Run(mode, func(t *testing.T) {
			a := testApp(t)
			store := openRouteV2Store(t)
			seedRouteFixtureWithOwner(t, a.Store, store,
				routeFixtureOwner{Name: "Sam Owner", Number: "+12025550123", InAddressBook: true},
				[]routeFixturePerson{{Name: "Bob Example", Number: "+447555012399", LegacyThreadID: "bob", RemoteThreadID: "bob"}})
			if err := a.Store.UpsertContact(&db.Contact{ContactID: "contact-owner", Name: "Sam Owner", Number: "5550123"}); err != nil {
				t.Fatalf("store the owner's card: %v", err)
			}
			reads, _ := routeReadsFor(t, a, store, mode)
			for _, query := range []string{"Sam Owner", "Owner"} {
				got, err := findRouteConversations(reads, a.Store, query, 80)
				if err != nil {
					t.Fatalf("findRouteConversations(%q): %v", query, err)
				}
				if len(got) != 0 {
					t.Errorf("routes for the owner's %q = %v, want none", query, routeIDs(got))
				}
			}
		})
	}
}

// Numbers that hold a query's digits mid-number must not use up a limit
// ahead of a number that ends with them. Nine newer people have 555-0123's
// digits inside their numbers, and Alice's number ends with them. At limit 1,
// whose search limit is 8, the tool still finds Alice, the thread
// send_message picks.
func TestResolveContactRoutesFindsALineBehindNumbersThatOnlyContainItsDigits(t *testing.T) {
	for _, mode := range routeStoreModes {
		t.Run(mode, func(t *testing.T) {
			a := testApp(t)
			store := openRouteV2Store(t)
			people := make([]routeFixturePerson, 0, 10)
			for index := 0; index < 9; index++ {
				people = append(people, routeFixturePerson{
					Name: fmt.Sprintf("Bob %d", index), Number: fmt.Sprintf("+44755501239%d", index),
					LegacyThreadID: fmt.Sprintf("bob-%d", index), RemoteThreadID: fmt.Sprintf("bob-%d", index),
				})
			}
			people = append(people, routeFixturePerson{Name: "Alice Example", Number: "+12025550123", LegacyThreadID: "alice", RemoteThreadID: "alice"})
			threads := seedRouteFixture(t, a.Store, store, people)
			reads, options := routeReadsFor(t, a, store, mode)
			alice := threads["Alice Example"]
			if mode == "legacy" {
				alice = "alice"
			}
			direct, err := findDirectSMSConversation(reads, "555-0123")
			if err != nil || direct == nil || direct.ConversationID != alice {
				t.Fatalf("send_message's thread = %+v (%v), want Alice's %q", direct, err, alice)
			}
			got, err := findRouteConversations(reads, a.Store, "555-0123", 8)
			if err != nil {
				t.Fatalf("findRouteConversations(): %v", err)
			}
			if len(got) != 1 || got[0].ConversationID != alice {
				t.Errorf("routes = %v, want Alice's thread %q only", routeIDs(got), alice)
			}
			matches := routeMatches(t, callRouteTool(t, resolveContactRoutesHandler(a, options), map[string]any{"query": "555-0123", "limit": 1}))
			if len(matches) != 1 || matches[0].PreferredReplyConversationID != alice {
				t.Errorf("limit-1 routes = %+v, want one that prefers Alice's thread %q", matches, alice)
			}
		})
	}
}

// % and _ in a query match only themselves in the address book, as in both
// stores' conversation searches. Anna's card holds neither, so no query made
// of them routes to her.
func TestResolveContactRoutesMatchesTheAddressBookLiterally(t *testing.T) {
	for _, mode := range routeStoreModes {
		t.Run(mode, func(t *testing.T) {
			a := testApp(t)
			store := openRouteV2Store(t)
			threads := seedRouteFixture(t, a.Store, store, []routeFixturePerson{
				{Name: "Anna Example", Number: "+12025550123", LegacyThreadID: "anna", RemoteThreadID: "anna", InAddressBook: true, NamelessInV2: true},
			})
			reads, _ := routeReadsFor(t, a, store, mode)
			anna := threads["Anna Example"]
			if mode == "legacy" {
				anna = "anna"
			}
			for _, query := range []string{"%", "_", "Ann_", "%nna", "A%a"} {
				got, err := findRouteConversations(reads, a.Store, query, 80)
				if err != nil {
					t.Fatalf("findRouteConversations(%q): %v", query, err)
				}
				if len(got) != 0 {
					t.Errorf("routes for %q = %v, want none", query, routeIDs(got))
				}
			}
			got, err := findRouteConversations(reads, a.Store, "Anna", 80)
			if err != nil {
				t.Fatalf("findRouteConversations(Anna): %v", err)
			}
			if len(got) != 1 || got[0].ConversationID != anna {
				t.Errorf("routes for Anna = %v, want her thread %q only", routeIDs(got), anna)
			}
		})
	}
}

// A group's roster can lag behind who writes in it. Carol, whom Work's roster
// lacks, wrote in it, so her number in any written form finds Work. The same
// ten digits inside another sender's longer number find nothing, and neither
// does the owner's number, though the owner wrote in Work.
func TestResolveContactRoutesFindsAnIncomingSenderByAnyFormOfTheirNumber(t *testing.T) {
	a := testApp(t)
	for _, conv := range []*db.Conversation{
		{ConversationID: "work", Name: "Work", IsGroup: true, SourcePlatform: "sms", LastMessageTS: 10,
			Participants: `[{"name":"Bob Example","number":"+15555550199"},{"name":"Sam Owner","number":"+15550009999","is_me":true}]`},
		{ConversationID: "club", Name: "Club", IsGroup: true, SourcePlatform: "sms", LastMessageTS: 20,
			Participants: `[{"name":"Dee Example","number":"+15555550188"},{"name":"Sam Owner","number":"+15550009999","is_me":true}]`},
	} {
		if err := a.Store.UpsertConversation(conv); err != nil {
			t.Fatalf("seed %s: %v", conv.ConversationID, err)
		}
	}
	for _, message := range []*db.Message{
		{MessageID: "carol", ConversationID: "work", SenderName: "Carol Guest", SenderNumber: "+13035550147", Body: "on my way", TimestampMS: 9, SourcePlatform: "sms"},
		{MessageID: "owner", ConversationID: "work", SenderName: "Sam Owner", SenderNumber: "+15550009999", IsFromMe: true, Body: "see you", TimestampMS: 10, SourcePlatform: "sms"},
		// 3035550147 sits inside this number but doesn't end it.
		{MessageID: "decoy", ConversationID: "club", SenderName: "Eve Guest", SenderNumber: "+44130355501479", Body: "hello", TimestampMS: 19, SourcePlatform: "sms"},
	} {
		if err := a.Store.UpsertMessage(message); err != nil {
			t.Fatalf("seed message %s: %v", message.MessageID, err)
		}
	}
	for _, query := range routeNumberForms("+13035550147") {
		got, err := findRouteConversations(a.Store, a.Store, query, 80)
		if err != nil {
			t.Fatalf("findRouteConversations(%q): %v", query, err)
		}
		if len(got) != 1 || got[0].ConversationID != "work" {
			t.Errorf("routes for Carol's %q = %v, want Work only", query, routeIDs(got))
		}
	}
	for _, query := range routeNumberForms("+15550009999") {
		got, err := findRouteConversations(a.Store, a.Store, query, 80)
		if err != nil {
			t.Fatalf("findRouteConversations(%q): %v", query, err)
		}
		if len(got) != 0 {
			t.Errorf("routes for the owner's %q = %v, want none", query, routeIDs(got))
		}
	}
}

// The legacy store keeps a number as the address book wrote it, separators
// included, and a query in any written form still finds it by its line.
func TestResolveContactRoutesFindsALegacyNumberWrittenWithSeparators(t *testing.T) {
	a := testApp(t)
	store := openRouteV2Store(t)
	seedRouteFixture(t, a.Store, store, []routeFixturePerson{
		{Name: "Alice Example", Number: "+12025550123", LegacyNumber: "+1 (202) 555-0123", LegacyThreadID: "alice"},
		{Name: "Bob Example", Number: "+447555012399", LegacyNumber: "+44 7555 012399", LegacyThreadID: "bob"},
	})
	for _, query := range routeNumberForms("+12025550123") {
		direct, err := findDirectSMSConversation(a.Store, query)
		if err != nil || direct == nil || direct.ConversationID != "alice" {
			t.Fatalf("send_message's thread for %q = %+v (%v), want Alice's", query, direct, err)
		}
		got, err := findRouteConversations(a.Store, a.Store, query, 80)
		if err != nil {
			t.Fatalf("findRouteConversations(%q): %v", query, err)
		}
		if len(got) != 1 || got[0].ConversationID != "alice" {
			t.Errorf("routes for %q = %v, want Alice's thread only", query, routeIDs(got))
		}
	}
}

// A title that holds a name says nothing about where that person's own
// thread is. Only a legacy thread knows Alice Chen's name, and v2 files her
// thread under her number alone, while a newer group in both stores is
// titled "Alice Chen dinner plans". A search for her name finds her thread
// beside the group, on either store: a title that matches doesn't stop the
// tool from reading the names in legacy threads.
func TestResolveContactRoutesFindsALegacyOnlyNameBesideATitleThatHoldsIt(t *testing.T) {
	for _, mode := range routeStoreModes {
		t.Run(mode, func(t *testing.T) {
			a := testApp(t)
			store := openRouteV2Store(t)
			threads := seedRouteFixture(t, a.Store, store, []routeFixturePerson{
				{Name: "Alice Chen", Number: "+12025550123", LegacyThreadID: "alice", RemoteThreadID: "alice", NamelessInV2: true},
			})
			seedTitledRouteGroup(t, a.Store, store, defaultRouteFixtureOwner, "dinner", "Alice Chen dinner plans")
			reads, options := routeReadsFor(t, a, store, mode)
			alice := threads["Alice Chen"]
			if mode == "legacy" {
				alice = "alice"
			}
			want := []string{"dinner", alice}
			for _, query := range []string{"Alice Chen", "alice", "Chen"} {
				got, err := findRouteConversations(reads, a.Store, query, 80)
				if err != nil {
					t.Fatalf("findRouteConversations(%q): %v", query, err)
				}
				if !slices.Equal(routeIDs(got), want) {
					t.Errorf("routes for %q = %v, want %v: the group, then Alice's thread", query, routeIDs(got), want)
				}
				matches := routeMatches(t, callRouteTool(t, resolveContactRoutesHandler(a, options), map[string]any{"query": query}))
				preferred := make([]string, 0, len(matches))
				for _, match := range matches {
					preferred = append(preferred, match.PreferredReplyConversationID)
				}
				if !slices.Equal(preferred, want) {
					t.Errorf("preferred routes for %q = %v, want %v; matches=%+v", query, preferred, want, matches)
				}
			}
		})
	}
}

// countingRouteReads counts the searches that the route tool asks the
// serving store for, by line and by text.
type countingRouteReads struct {
	readsource.ReadSource
	lines map[string]int
	texts map[string]int
}

func newCountingRouteReads(reads readsource.ReadSource) *countingRouteReads {
	return &countingRouteReads{ReadSource: reads, lines: map[string]int{}, texts: map[string]int{}}
}

func (r *countingRouteReads) SearchConversationsByPhoneLine(line string, limit int) ([]*db.Conversation, error) {
	r.lines[line]++
	return r.ReadSource.SearchConversationsByPhoneLine(line, limit)
}

func (r *countingRouteReads) SearchConversationsByMetadata(query string, limit int) ([]*db.Conversation, error) {
	r.texts[query]++
	return r.ReadSource.SearchConversationsByMetadata(query, limit)
}

// Two address-book cards and a legacy thread hold Alice's number, in two
// written forms that name one line. Two Google Chat threads list her email
// address, where the legacy importer files it as her number, under two names.
// On either store, the tool asks the serving store about her line once and
// about her email address once when the query names her, and about the
// address once when the query is the address itself.
func TestResolveContactRoutesSearchesEachLineOnce(t *testing.T) {
	for _, mode := range routeStoreModes {
		t.Run(mode, func(t *testing.T) {
			a := testApp(t)
			store := openRouteV2Store(t)
			threads := seedRouteFixture(t, a.Store, store, []routeFixturePerson{
				{Name: "Alice Chen", Number: "+12025550123", LegacyNumber: "(202) 555-0123", LegacyThreadID: "alice", RemoteThreadID: "alice", InAddressBook: true},
			})
			if err := a.Store.UpsertContact(&db.Contact{ContactID: "contact-alice-work", Name: "Alice Chen (work)", Number: "+1 202-555-0123"}); err != nil {
				t.Fatalf("seed Alice's work card: %v", err)
			}
			for _, conv := range []*db.Conversation{
				{ConversationID: "lunch", Name: "Lunch", SourcePlatform: "gchat", LastMessageTS: 1,
					Participants: `[{"name":"Alice Chen","number":"alice.chen@example.com"}]`},
				{ConversationID: "books", Name: "Books", SourcePlatform: "gchat", LastMessageTS: 2,
					Participants: `[{"name":"Alice C.","number":"alice.chen@example.com"}]`},
			} {
				if err := a.Store.UpsertConversation(conv); err != nil {
					t.Fatalf("seed %s: %v", conv.ConversationID, err)
				}
			}
			reads, _ := routeReadsFor(t, a, store, mode)
			alice := threads["Alice Chen"]
			if mode == "legacy" {
				alice = "alice"
			}
			counting := newCountingRouteReads(reads)
			got, err := findRouteConversations(counting, a.Store, "Alice", 80)
			if err != nil {
				t.Fatalf("findRouteConversations(Alice): %v", err)
			}
			if !slices.Contains(routeIDs(got), alice) {
				t.Errorf("routes for Alice = %v, want her thread %q among them", routeIDs(got), alice)
			}
			if counting.lines["2025550123"] != 1 || counting.texts["alice.chen@example.com"] != 1 {
				t.Errorf("line searches %v, text searches %v; want one search of Alice's line and one of her email address", counting.lines, counting.texts)
			}
			for line, count := range counting.lines {
				if count != 1 {
					t.Errorf("searches of line %s = %d, want 1", line, count)
				}
			}
			for text, count := range counting.texts {
				if count != 1 {
					t.Errorf("searches of %q = %d, want 1", text, count)
				}
			}

			counting = newCountingRouteReads(reads)
			if _, err := findRouteConversations(counting, a.Store, "alice.chen@example.com", 80); err != nil {
				t.Fatalf("findRouteConversations(alice.chen@example.com): %v", err)
			}
			if len(counting.lines) != 0 || len(counting.texts) != 1 || counting.texts["alice.chen@example.com"] != 1 {
				t.Errorf("for her email address: line searches %v, text searches %v; want one search of the address and no line search", counting.lines, counting.texts)
			}
		})
	}
}
