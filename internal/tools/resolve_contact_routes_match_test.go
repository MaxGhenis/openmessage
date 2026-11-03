package tools

import (
	"fmt"
	"path/filepath"
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
