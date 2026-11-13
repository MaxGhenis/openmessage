package tools

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
	"github.com/maxghenis/openmessage/internal/v2keys"
)

// seedLegacyAddressThread writes a legacy thread with title as its name, at
// time at. Its roster lists entries, then the default fixture owner, whose
// entry carries the is_me flag.
func seedLegacyAddressThread(t *testing.T, legacy *db.Store, id, title, platform string, at int64, entries ...any) {
	t.Helper()
	participants, err := json.Marshal(append(entries, map[string]any{
		"name": defaultRouteFixtureOwner.Name, "number": defaultRouteFixtureOwner.Number, "is_me": true,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if err := legacy.UpsertConversation(&db.Conversation{
		ConversationID: id, Name: title, Participants: string(participants),
		LastMessageTS: at, SourcePlatform: platform,
	}); err != nil {
		t.Fatalf("seed legacy conversation %q: %v", id, err)
	}
}

// seedV2AddressThread writes a v2 thread at time at between the default
// fixture owner and someone who holds each of values, and returns its
// conversation ID. A value with an @ makes an email identity and any other
// a username, and the helper stores each as given, as v2keys.IdentityKey
// would store a value without white space around it. Threads that list one
// value share its identity. Seed the fixture first: it writes the account
// and the owner's identity.
func seedV2AddressThread(t *testing.T, store *sqlite.Store, id string, at int64, values ...string) string {
	t.Helper()
	now := time.Now().UnixMilli()
	conversationID := v2keys.DeriveID("conversation", routeFixtureAccount, "address-thread:"+id)
	if err := store.UpsertConversation(sqlite.Conversation{
		ConversationID: conversationID, AccountID: routeFixtureAccount,
		RemoteConversationID: id, Kind: sqlite.ConversationKindDirect,
		NotificationMode: sqlite.NotificationModeAll, MetadataJSON: "{}",
		LastMessageAtMS: at, CreatedAtMS: now, UpdatedAtMS: now,
	}); err != nil {
		t.Fatalf("UpsertConversation(%s): %v", id, err)
	}
	participants := []sqlite.ConversationParticipant{{
		AccountID: routeFixtureAccount, ConversationID: conversationID, IdentityID: "identity-self",
		Role: sqlite.ParticipantRoleMember, IsActive: true,
	}}
	for _, value := range values {
		kind := "username"
		if strings.Contains(value, "@") {
			kind = "email"
		}
		identityID := v2keys.DeriveID("identity", routeFixtureAccount, kind+"\x1f"+value)
		if err := store.UpsertIdentity(sqlite.Identity{
			IdentityID: identityID, AccountID: routeFixtureAccount, Kind: sqlite.IdentityKind(kind),
			CanonicalValue: value, RawValue: value, MetadataJSON: "{}", CreatedAtMS: now, UpdatedAtMS: now,
		}); err != nil {
			t.Fatalf("UpsertIdentity(%s): %v", value, err)
		}
		participants = append(participants, sqlite.ConversationParticipant{
			AccountID: routeFixtureAccount, ConversationID: conversationID, IdentityID: identityID,
			Role: sqlite.ParticipantRoleMember, IsActive: true,
		})
	}
	if err := store.ReplaceConversationParticipants(conversationID, participants); err != nil {
		t.Fatalf("ReplaceConversationParticipants(%s): %v", id, err)
	}
	return conversationID
}

// Only Alice's address-book card holds their name, and its number holds their
// email address with white space around it. Their legacy thread, whose title
// reads Support, lists the address with other white space around it and no
// name; v2 holds it trimmed, as v2keys.IdentityKey stores it. A search for
// their name finds their thread on either store: the tool and the lookup
// compare addresses by key, and a key drops what strings.TrimSpace trims.
func TestResolveContactRoutesFindsAnAddressWhateverWhiteSpaceSurroundsIt(t *testing.T) {
	for _, mode := range routeStoreModes {
		t.Run(mode, func(t *testing.T) {
			a := testApp(t)
			store := openRouteV2Store(t)
			seedRouteFixture(t, a.Store, store, nil)
			if err := a.Store.UpsertContact(&db.Contact{ContactID: "contact-alice", Name: "Alice Chen", Number: " support@example.com\t"}); err != nil {
				t.Fatalf("seed Alice's card: %v", err)
			}
			seedLegacyAddressThread(t, a.Store, "alice", "Support", "sms", 2000, map[string]any{"number": "\u3000support@example.com\u00a0"})
			want := seedV2AddressThread(t, store, "alice", 2000, "support@example.com")
			if mode == "legacy" {
				want = "alice"
			}
			reads, options := routeReadsFor(t, a, store, mode)
			got, err := findRouteConversations(reads, a.Store, "Alice Chen", 80)
			if err != nil {
				t.Fatalf("findRouteConversations(Alice Chen): %v", err)
			}
			if !slices.Equal(routeIDs(got), []string{want}) {
				t.Errorf("routes for Alice Chen = %v, want only their thread %q", routeIDs(got), want)
			}
			matches := routeMatches(t, callRouteTool(t, resolveContactRoutesHandler(a, options), map[string]any{"query": "Alice Chen"}))
			if len(matches) != 1 || matches[0].PreferredReplyConversationID != want {
				t.Errorf("matches for Alice Chen = %+v, want one, preferring %q", matches, want)
			}
		})
	}
}

// A query with white space around it finds what the trimmed query finds. The
// card's name holds the padded text and its number holds the trimmed address,
// so the card leads to the address that the query's own search covers, and
// only a thread's roster holds that address.
func TestFindRouteConversationsSearchesForTheTrimmedQuery(t *testing.T) {
	const address = "support@example.com"
	const query = " " + address + " "
	for _, mode := range routeStoreModes {
		t.Run(mode, func(t *testing.T) {
			a := testApp(t)
			store := openRouteV2Store(t)
			seedRouteFixture(t, a.Store, store, nil)
			if err := a.Store.UpsertContact(&db.Contact{ContactID: "contact-support", Name: query, Number: address}); err != nil {
				t.Fatalf("seed the card: %v", err)
			}
			seedLegacyAddressThread(t, a.Store, "support", "Help desk", "sms", 2000, map[string]any{"number": address})
			want := seedV2AddressThread(t, store, "support", 2000, address)
			if mode == "legacy" {
				want = "support"
			}
			reads, _ := routeReadsFor(t, a, store, mode)
			got, err := findRouteConversations(reads, a.Store, query, 80)
			if err != nil {
				t.Fatalf("findRouteConversations(%q): %v", query, err)
			}
			if !slices.Equal(routeIDs(got), []string{want}) {
				t.Errorf("routes for %q = %v, want only the thread %q", query, routeIDs(got), want)
			}
		})
	}
}

// Elise's two address-book cards write their email address two ways that
// differ in the case of a letter outside ASCII, which the lookup tells
// apart, and a legacy thread lists each way; v2 holds the address one way,
// in lower case, as v2keys.IdentityKey writes an email address, so only the
// lower-case card finds the v2 thread. Dana's card holds one address that two
// of their threads list, an SMS thread and a Google Chat thread. No thread
// lists either name. A search for each name finds every thread of that person
// on either store: the tool looks up each way of writing an address that
// differs beyond ASCII letters' case on its own, and keeps every thread a
// lookup returns, up to its limit.
func TestResolveContactRoutesFindsEveryThreadOfEachAddressItLooksUp(t *testing.T) {
	upper, lower := "\u00c9lise@example.com", "\u00e9lise@example.com"
	for _, mode := range routeStoreModes {
		t.Run(mode, func(t *testing.T) {
			a := testApp(t)
			store := openRouteV2Store(t)
			seedRouteFixture(t, a.Store, store, nil)
			for _, card := range []*db.Contact{
				{ContactID: "contact-elise", Name: "Elise Martin", Number: upper},
				{ContactID: "contact-elise-work", Name: "Elise Martin (work)", Number: lower},
				{ContactID: "contact-dana", Name: "Dana Lee", Number: "dana@example.com"},
			} {
				if err := a.Store.UpsertContact(card); err != nil {
					t.Fatalf("seed %s: %v", card.ContactID, err)
				}
			}
			seedLegacyAddressThread(t, a.Store, "elise-upper", "Upper", "gchat", 4000, map[string]any{"number": upper})
			seedLegacyAddressThread(t, a.Store, "elise-lower", "Lower", "gchat", 3000, map[string]any{"number": lower})
			seedLegacyAddressThread(t, a.Store, "dana-sms", "Texts", "sms", 2000, map[string]any{"number": "dana@example.com"})
			seedLegacyAddressThread(t, a.Store, "dana-chat", "Chat", "gchat", 1000, "dana@example.com")
			elise := seedV2AddressThread(t, store, "elise", 3000, lower)
			danaSMS := seedV2AddressThread(t, store, "dana-sms", 2000, "dana@example.com")
			danaChat := seedV2AddressThread(t, store, "dana-chat", 1000, "dana@example.com")
			want := map[string][]string{"Elise Martin": {elise}, "Dana Lee": {danaSMS, danaChat}}
			if mode == "legacy" {
				want = map[string][]string{"Elise Martin": {"elise-upper", "elise-lower"}, "Dana Lee": {"dana-sms", "dana-chat"}}
			}
			reads, _ := routeReadsFor(t, a, store, mode)
			for _, name := range []string{"Elise Martin", "Dana Lee"} {
				got, err := findRouteConversations(reads, a.Store, name, 80)
				if err != nil {
					t.Fatalf("findRouteConversations(%s): %v", name, err)
				}
				if !slices.Equal(routeIDs(got), want[name]) {
					t.Errorf("routes for %s = %v, want %v", name, routeIDs(got), want[name])
				}
			}
		})
	}
}

// Ann's and Bob's address-book cards hold email addresses that differ only
// after a NUL byte, and each store holds a thread with each of them. A
// search for each name finds only that person's thread: the lookup compares
// every byte of an address, the bytes after a NUL among them.
func TestResolveContactRoutesTellsApartAddressesThatDifferAfterANUL(t *testing.T) {
	ann, bob := "ann\x00a@example.com", "ann\x00b@example.com"
	for _, mode := range routeStoreModes {
		t.Run(mode, func(t *testing.T) {
			a := testApp(t)
			store := openRouteV2Store(t)
			seedRouteFixture(t, a.Store, store, nil)
			for _, card := range []*db.Contact{
				{ContactID: "contact-ann", Name: "Ann Lee", Number: ann},
				{ContactID: "contact-bob", Name: "Bob Ray", Number: bob},
			} {
				if err := a.Store.UpsertContact(card); err != nil {
					t.Fatalf("seed %s: %v", card.ContactID, err)
				}
			}
			seedLegacyAddressThread(t, a.Store, "ann", "A1", "sms", 1000, map[string]any{"number": ann})
			seedLegacyAddressThread(t, a.Store, "bob", "B1", "sms", 2000, map[string]any{"number": bob})
			want := map[string]string{
				"Ann Lee": seedV2AddressThread(t, store, "ann", 1000, ann),
				"Bob Ray": seedV2AddressThread(t, store, "bob", 2000, bob),
			}
			if mode == "legacy" {
				want = map[string]string{"Ann Lee": "ann", "Bob Ray": "bob"}
			}
			reads, options := routeReadsFor(t, a, store, mode)
			for _, name := range []string{"Ann Lee", "Bob Ray"} {
				got, err := findRouteConversations(reads, a.Store, name, 80)
				if err != nil {
					t.Fatalf("findRouteConversations(%s): %v", name, err)
				}
				if !slices.Equal(routeIDs(got), []string{want[name]}) {
					t.Errorf("routes for %s = %v, want only their thread %q", name, routeIDs(got), want[name])
				}
				matches := routeMatches(t, callRouteTool(t, resolveContactRoutesHandler(a, options), map[string]any{"query": name}))
				if len(matches) != 1 || matches[0].PreferredReplyConversationID != want[name] {
					t.Errorf("matches for %s = %+v, want one, preferring %q", name, matches, want[name])
				}
			}
		})
	}
}

// Ann's address-book card holds one email address, and a legacy thread lists
// their name beside a second address that differs from the first only after a
// NUL byte. A search for their name looks up both addresses and finds the
// threads of each, on either store: the tool keys each lookup by every byte
// of an address, as the stores compare addresses.
func TestResolveContactRoutesLooksUpTwoAddressesThatDifferAfterANUL(t *testing.T) {
	first, second := "ann\x00a@example.com", "ann\x00b@example.com"
	for _, mode := range routeStoreModes {
		t.Run(mode, func(t *testing.T) {
			a := testApp(t)
			store := openRouteV2Store(t)
			seedRouteFixture(t, a.Store, store, nil)
			if err := a.Store.UpsertContact(&db.Contact{ContactID: "contact-ann", Name: "Ann Lee", Number: first}); err != nil {
				t.Fatalf("seed Ann's card: %v", err)
			}
			seedLegacyAddressThread(t, a.Store, "ann-first", "A1", "sms", 1000, map[string]any{"number": first})
			seedLegacyAddressThread(t, a.Store, "ann-named", "A2", "sms", 2000, map[string]any{"name": "Ann Lee", "number": second})
			seedLegacyAddressThread(t, a.Store, "ann-second", "A3", "sms", 3000, map[string]any{"number": second})
			want := []string{
				seedV2AddressThread(t, store, "ann-second", 3000, second),
				seedV2AddressThread(t, store, "ann-first", 1000, first),
			}
			if mode == "legacy" {
				want = []string{"ann-second", "ann-named", "ann-first"}
			}
			reads, _ := routeReadsFor(t, a, store, mode)
			got, err := findRouteConversations(reads, a.Store, "Ann Lee", 80)
			if err != nil {
				t.Fatalf("findRouteConversations(Ann Lee): %v", err)
			}
			if !slices.Equal(routeIDs(got), want) {
				t.Errorf("routes for Ann Lee = %v, want %v", routeIDs(got), want)
			}
		})
	}
}
