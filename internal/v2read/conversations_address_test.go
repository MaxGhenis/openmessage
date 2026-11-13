package v2read

import (
	"fmt"
	"math/rand"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"testing/quick"

	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
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

// flipASCIICase writes value with the case of some of its ASCII letters
// flipped.
func flipASCIICase(random *rand.Rand, value string) string {
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
// some of the time, with runes of one, two and three bytes.
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

func sourceConversationIDs(conversations []*db.Conversation) []string {
	ids := []string{}
	for _, conversation := range conversations {
		ids = append(ids, conversation.ConversationID)
	}
	return ids
}

func TestSearchConversationsByAddressMatchesTheWholeAddress(t *testing.T) {
	store, _, source := openSourceTestStore(t)
	seedSourceAccount(t, store, "address-account", "google_messages")
	identities := map[string]string{}
	// v2keys.IdentityKey trims a value before it stores it, so only a
	// writer that skips it stores the padded value here; the lookup trims
	// stored values all the same.
	for index, identity := range []struct {
		value, raw, kind, name string
		self                   bool
	}{
		{"support@example.com", "", "email", "Alice", false},
		{"SUPPORT@example.com", "", "email", "Alice (work)", false},
		{"other-support@example.com", "", "email", "Other", false},
		{"+15555550100", "", "e164", "support@example.com", false},
		{"me@example.com", "", "email", "Me", true},
		{"users/1234", "", "username", "User", false},
		{"élise@example.com", "", "email", "Élise", false},
		{"  support@example.com\u3000", "", "email", "Padded", false},
		{"canonical@example.com", "raw@example.com", "email", "Canonical", false},
		{"\u200bsupport@example.com", "", "email", "Invisible", false},
		{"support@example.com\x00x", "", "email", "Cut", false},
		{"a\x00c@example.com", "", "email", "Inside", false},
	} {
		id := "identity-" + strconv.Itoa(index)
		raw := identity.raw
		if raw == "" {
			raw = identity.value
		}
		if err := store.UpsertIdentity(sqlite.Identity{
			IdentityID: id, AccountID: "address-account", Kind: sqlite.IdentityKind(identity.kind),
			CanonicalValue: identity.value, RawValue: raw, DisplayName: identity.name,
			IsSelf: identity.self, MetadataJSON: `{}`, CreatedAtMS: sourceTestTimeMS, UpdatedAtMS: sourceTestTimeMS,
		}); err != nil {
			t.Fatalf("UpsertIdentity(%s): %v", identity.value, err)
		}
		identities[identity.value] = id
	}
	for index, conversation := range []struct {
		id, title string
		members   []string
		left      bool
	}{
		{"alice", "Alice", []string{"support@example.com", "me@example.com"}, false},
		{"alice-work", "Work", []string{"SUPPORT@example.com"}, false},
		{"other", "Other", []string{"other-support@example.com", "me@example.com"}, false},
		{"titled", "Ask support@example.com", []string{"+15555550100"}, false},
		{"owner", "Notes", []string{"me@example.com"}, false},
		{"user", "User", []string{"users/1234"}, false},
		{"accented", "Élise", []string{"élise@example.com"}, false},
		{"padded", "Padded", []string{"  support@example.com\u3000"}, false},
		{"canonical", "Canonical", []string{"canonical@example.com"}, false},
		{"left", "Former", []string{"support@example.com"}, true},
		{"invisible", "Invisible", []string{"\u200bsupport@example.com"}, false},
		{"cut", "Cut", []string{"support@example.com\x00x"}, false},
		{"inside", "Inside", []string{"a\x00c@example.com"}, false},
	} {
		seedSourceConversation(t, store, sqlite.Conversation{
			ConversationID: conversation.id, AccountID: "address-account", RemoteConversationID: "remote-" + conversation.id,
			Kind: sqlite.ConversationKindGroup, Title: conversation.title,
			NotificationMode: sqlite.NotificationModeAll, LastMessageAtMS: int64(1000 + 10*index),
		})
		var participants []sqlite.ConversationParticipant
		for _, member := range conversation.members {
			participants = append(participants, sqlite.ConversationParticipant{
				AccountID: "address-account", ConversationID: conversation.id, IdentityID: identities[member],
				Role: sqlite.ParticipantRoleMember, DisplayName: "support@example.com", IsActive: !conversation.left,
			})
		}
		if err := store.ReplaceConversationParticipants(conversation.id, participants); err != nil {
			t.Fatalf("ReplaceConversationParticipants(%s): %v", conversation.id, err)
		}
	}
	// A participant who has left still counts, as in the metadata search.
	hits := []string{"left", "padded", "alice-work", "alice"}
	for _, test := range []struct {
		address string
		limit   int
		want    []string
	}{
		{"support@example.com", 20, hits},
		{"Support@Example.Com", 20, hits},
		{"\u00a0support@example.com\t", 20, hits},
		{"support@example.com", 1, []string{"left"}},
		{"canonical@example.com", 20, []string{"canonical"}},
		// A NUL counts as any other byte, so a value that one ends early
		// matches nothing, and neither does one that differs only after it.
		{"a\x00c@example.com", 20, []string{"inside"}},
		{"A\x00C@EXAMPLE.COM ", 20, []string{"inside"}},
		{"a\x00b@example.com", 20, []string{}},
		{"a", 20, []string{}},
		{"raw@example.com", 20, []string{}},
		{"example.com", 20, []string{}},
		{"me@example.com", 20, []string{}},
		{"users/123", 20, []string{}},
		{"Élise@example.com", 20, []string{}},
		{"élise@EXAMPLE.com", 20, []string{"accented"}},
		{"", 20, []string{}},
		{"   ", 20, []string{}},
		{"\u3000", 20, []string{}},
		{"support@example.com", 0, []string{}},
		{"support@example.com", -1, []string{}},
	} {
		got, err := source.SearchConversationsByAddress(test.address, test.limit)
		if err != nil {
			t.Fatalf("SearchConversationsByAddress(%q, %d): %v", test.address, test.limit, err)
		}
		if ids := sourceConversationIDs(got); !slices.Equal(ids, test.want) {
			t.Errorf("SearchConversationsByAddress(%q, %d) = %v, want %v", test.address, test.limit, ids, test.want)
		}
	}
}

// Differential, over random v2 stores: SearchConversationsByAddress returns,
// in order, the conversations with a participant other than the self identity
// whose canonical value holds the same bytes as the address once
// strings.TrimSpace has trimmed both, ASCII letters' case aside, newest
// first, then in conversation ID order, and at most the limit. Identities of
// several kinds hold the targets, with their ASCII letters in any case, and
// some hold one with white space around it, as a writer that skips
// v2keys.IdentityKey might store it; decoys hold a target inside a longer
// value, beside a character that only looks like white space or before a NUL,
// or differ from one only in a non-ASCII letter's case or after a NUL; raw
// values often differ from canonical ones; some participants have left their
// conversations; identities' and participants' display names and titles hold
// targets too; the owner takes part in most conversations, and an identity
// that isn't the owner's holds the owner's address in another case; and
// conversations often share a time. So, as invariants, the self identity
// holds no address, raw values, display names and titles count for nothing, a
// participant who has left counts as the metadata search counts one, values
// that only contain the address never crowd out one that is it, and threads
// that share a time come in ID order. Two more hold for every query: any way
// of writing its ASCII letters' case, with any white space around it, finds
// the same threads, and SearchConversationsByMetadata for the same text
// without the white space around it, at the same limit, returns every thread
// the address search returns, or newer ones in its place, so the route tool
// loses nothing when it counts the query, which its handler trims, as an
// address it has looked up.
func TestSearchConversationsByAddressAgreesWithTheAddressModel(t *testing.T) {
	owner := "me@example.com"
	kinds := map[string]string{
		"support@example.com":                    "email",
		"SUPPORT@example.com":                    "email",
		"Support@Example.com":                    "email",
		" support@example.com\u3000":             "email",
		"\u200bsupport@example.com":              "email",
		"other-support@example.com":              "email",
		"support@example.com.au":                 "email",
		"ME@example.com":                         "email",
		"users/123":                              "username",
		"Users/123":                              "username",
		"\tusers/123\u00a0":                      "username",
		"users/1234":                             "username",
		"abcdefab-abcd-abcd-abcd-abcdefabcdef":   "signal_aci",
		"ABCDEFAB-abcd-abcd-abcd-abcdefabcdef":   "signal_aci",
		"élise@example.com":                      "email",
		"Élise@example.com":                      "email",
		"+12025550123":                           "e164",
		"legacy-participant:support@example.com": "legacy_participant",
		"a\x00c@example.com":                     "email",
		"A\x00C@example.com":                     "email",
		"a\x00b@example.com":                     "email",
		"support@example.com\x00x":               "email",
	}
	members := []string{}
	for value := range kinds {
		members = append(members, value)
	}
	sort.Strings(members)
	queries := []string{owner, "", "  ", "support@example.com", "users/123", "abcdefab-abcd-abcd-abcd-abcdefabcdef", "élise@example.com", "example.com", "+12025550123", "a\x00c@example.com"}
	exercised := map[string]int{}

	property := func(seed int64) bool {
		random := rand.New(rand.NewSource(seed))
		store, _, source := openSourceTestStore(t)
		seedSourceAccount(t, store, "address-account", "google_messages")
		identities := map[string]string{}
		for index, value := range append([]string{owner}, members...) {
			kind := kinds[value]
			if value == owner {
				kind = "email"
			}
			name := "Person " + strconv.Itoa(index)
			if random.Intn(4) == 0 {
				name = flipASCIICase(random, queries[3+random.Intn(len(queries)-3)])
			}
			id := "identity-" + strconv.Itoa(index)
			// The raw value is what a transport sent; only the canonical
			// value counts.
			raw := value
			if random.Intn(3) == 0 {
				raw = "raw-" + strconv.Itoa(index) + "@example.org"
			}
			if err := store.UpsertIdentity(sqlite.Identity{
				IdentityID: id, AccountID: "address-account", Kind: sqlite.IdentityKind(kind),
				CanonicalValue: value, RawValue: raw, DisplayName: name,
				IsSelf: value == owner, MetadataJSON: `{}`, CreatedAtMS: sourceTestTimeMS, UpdatedAtMS: sourceTestTimeMS,
			}); err != nil {
				t.Fatalf("UpsertIdentity(%s): %v", value, err)
			}
			identities[value] = id
		}
		type thread struct {
			id, title string
			at        int64
			members   []string
			left      map[string]bool
		}
		var threads []thread
		for index := 0; index < 3+random.Intn(10); index++ {
			conversation := thread{id: fmt.Sprintf("conversation-%02d", index), at: int64(1000 + 10*random.Intn(6)), title: "Thread " + strconv.Itoa(index)}
			switch random.Intn(5) {
			case 0:
				conversation.title = "Ask " + flipASCIICase(random, queries[3+random.Intn(len(queries)-3)])
			case 1:
				conversation.title = flipASCIICase(random, queries[3+random.Intn(len(queries)-3)])
			}
			for _, member := range random.Perm(len(members))[:random.Intn(4)] {
				conversation.members = append(conversation.members, members[member])
			}
			if random.Intn(4) != 0 {
				conversation.members = append(conversation.members, owner)
			}
			seedSourceConversation(t, store, sqlite.Conversation{
				ConversationID: conversation.id, AccountID: "address-account", RemoteConversationID: "remote-" + conversation.id,
				Kind: sqlite.ConversationKindGroup, Title: conversation.title,
				NotificationMode: sqlite.NotificationModeAll, LastMessageAtMS: conversation.at,
			})
			var participants []sqlite.ConversationParticipant
			conversation.left = map[string]bool{}
			for _, member := range conversation.members {
				participant := sqlite.ConversationParticipant{
					AccountID: "address-account", ConversationID: conversation.id, IdentityID: identities[member],
					Role: sqlite.ParticipantRoleMember, IsActive: random.Intn(4) != 0,
				}
				conversation.left[member] = !participant.IsActive
				if random.Intn(4) == 0 {
					participant.DisplayName = flipASCIICase(random, queries[3+random.Intn(len(queries)-3)])
				}
				participants = append(participants, participant)
			}
			if err := store.ReplaceConversationParticipants(conversation.id, participants); err != nil {
				t.Fatalf("ReplaceConversationParticipants(%s): %v", conversation.id, err)
			}
			threads = append(threads, conversation)
		}
		sort.Slice(threads, func(i, j int) bool {
			if threads[i].at != threads[j].at {
				return threads[i].at > threads[j].at
			}
			return threads[i].id < threads[j].id
		})
		dump := func() {
			for _, conversation := range threads {
				t.Logf("  %s at=%d title=%q: %v", conversation.id, conversation.at, conversation.title, conversation.members)
			}
		}
		search := func(address string, limit int) []string {
			got, err := source.SearchConversationsByAddress(address, limit)
			if err != nil {
				t.Fatalf("SearchConversationsByAddress(%q): %v", address, err)
			}
			return sourceConversationIDs(got)
		}
		newest := func(ids []string, limit int) []string {
			kept := []string{}
			for _, conversation := range threads {
				if len(kept) < limit && slices.Contains(ids, conversation.id) {
					kept = append(kept, conversation.id)
				}
			}
			return kept
		}

		for _, address := range queries {
			if random.Intn(2) == 0 {
				address = flipASCIICase(random, address)
			}
			address = spaceAround(random, address)
			text := strings.TrimSpace(address)
			limit := 1 + random.Intn(5)
			want := []string{}
			folded := foldASCIIForModel(text)
			crowded := false
			for _, conversation := range threads {
				hit, exact, selfOnly, otherCase, containsOnly, padded, left := false, false, false, false, false, false, false
				for _, member := range conversation.members {
					value := strings.TrimSpace(member)
					switch {
					case foldASCIIForModel(value) == folded && member == owner:
						selfOnly = true
					case foldASCIIForModel(value) == folded:
						hit = true
						exact = exact || value == text
						padded = padded || value != member
						left = left || conversation.left[member]
					case strings.Contains(foldASCIIForModel(value), folded):
						containsOnly = true
					case strings.EqualFold(value, text):
						otherCase = true
					}
				}
				if text == "" || !hit {
					if selfOnly && text != "" {
						exercised["a thread that only the self identity ties to the address"]++
					}
					if otherCase {
						exercised["a thread whose values would hold it but for a non-ASCII letter's case"]++
					}
					if containsOnly && len(want) < limit && strings.TrimSpace(address) != "" {
						crowded = true
					}
					if strings.Contains(foldASCIIForModel(conversation.title), folded) && strings.TrimSpace(address) != "" {
						exercised["a thread whose title alone holds the address"]++
					}
					continue
				}
				if len(want) < limit {
					if crowded {
						exercised["a hit behind a newer thread whose values only contain the address"]++
					}
					if !exact {
						exercised["a hit whose ASCII letters' case differs from the query's"]++
					}
					if padded {
						exercised["a hit through a value with white space around it"]++
					}
					if left {
						exercised["a hit through a participant who has left"]++
					}
					want = append(want, conversation.id)
				}
			}
			got := search(address, limit)
			if !slices.Equal(got, want) {
				dump()
				t.Logf("seed %d: address %q (limit %d) = %v, model says %v", seed, address, limit, got, want)
				return false
			}
			variant := spaceAround(random, flipASCIICase(random, address))
			if again := search(variant, limit); !slices.Equal(again, got) {
				dump()
				t.Logf("seed %d: address %q (limit %d) = %v, but %q = %v", seed, address, limit, got, variant, again)
				return false
			}
			metadata, err := source.SearchConversationsByMetadata(text, limit)
			if err != nil {
				t.Fatalf("SearchConversationsByMetadata(%q): %v", text, err)
			}
			byText := sourceConversationIDs(metadata)
			if merged := newest(append(slices.Clone(byText), got...), limit); !slices.Equal(merged, byText) {
				dump()
				t.Logf("seed %d: address %q (limit %d) = %v, which the metadata search's %v for %q lacks room for (merged %v)", seed, address, limit, got, byText, text, merged)
				return false
			}
			if len(want) > 0 {
				exercised["an address with hits"]++
				if !slices.Equal(byText, got) {
					exercised["a metadata search that returns other threads"]++
				}
				if text != address {
					exercised["an address with white space around it that has hits"]++
				}
				if strings.Contains(address, "\x00") {
					exercised["an address with a NUL in it that has hits"]++
				}
			}
		}
		return true
	}
	cases := 60
	if raceDetectorEnabled {
		cases = 15
	}
	if err := quick.Check(property, &quick.Config{MaxCount: cases, Rand: rand.New(rand.NewSource(20261104))}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		"an address with hits",
		"a metadata search that returns other threads",
		"a thread that only the self identity ties to the address",
		"a thread whose values would hold it but for a non-ASCII letter's case",
		"a thread whose title alone holds the address",
		"a hit behind a newer thread whose values only contain the address",
		"a hit whose ASCII letters' case differs from the query's",
		"a hit through a value with white space around it",
		"a hit through a participant who has left",
		"an address with white space around it that has hits",
		"an address with a NUL in it that has hits",
	} {
		if exercised[path] == 0 {
			t.Errorf("the property never drew %s; raise MaxCount (drew %v)", path, exercised)
		}
	}
}
