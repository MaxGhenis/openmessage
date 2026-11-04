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

// foldASCIIForModel lowers ASCII letters and keeps every other rune, as
// SQLite's NOCASE compares text.
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
	for index, identity := range []struct {
		value, kind, name string
		self              bool
	}{
		{"support@example.com", "email", "Alice", false},
		{"SUPPORT@example.com", "email", "Alice (work)", false},
		{"other-support@example.com", "email", "Other", false},
		{"+15555550100", "e164", "support@example.com", false},
		{"me@example.com", "email", "Me", true},
		{"users/1234", "username", "User", false},
		{"élise@example.com", "email", "Élise", false},
	} {
		id := "identity-" + strconv.Itoa(index)
		if err := store.UpsertIdentity(sqlite.Identity{
			IdentityID: id, AccountID: "address-account", Kind: sqlite.IdentityKind(identity.kind),
			CanonicalValue: identity.value, RawValue: identity.value, DisplayName: identity.name,
			IsSelf: identity.self, MetadataJSON: `{}`, CreatedAtMS: sourceTestTimeMS, UpdatedAtMS: sourceTestTimeMS,
		}); err != nil {
			t.Fatalf("UpsertIdentity(%s): %v", identity.value, err)
		}
		identities[identity.value] = id
	}
	for index, conversation := range []struct {
		id, title string
		members   []string
	}{
		{"alice", "Alice", []string{"support@example.com", "me@example.com"}},
		{"alice-work", "Work", []string{"SUPPORT@example.com"}},
		{"other", "Other", []string{"other-support@example.com", "me@example.com"}},
		{"titled", "Ask support@example.com", []string{"+15555550100"}},
		{"owner", "Notes", []string{"me@example.com"}},
		{"user", "User", []string{"users/1234"}},
		{"accented", "Élise", []string{"élise@example.com"}},
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
				Role: sqlite.ParticipantRoleMember, DisplayName: "support@example.com", IsActive: true,
			})
		}
		if err := store.ReplaceConversationParticipants(conversation.id, participants); err != nil {
			t.Fatalf("ReplaceConversationParticipants(%s): %v", conversation.id, err)
		}
	}
	for _, test := range []struct {
		address string
		limit   int
		want    []string
	}{
		{"support@example.com", 20, []string{"alice-work", "alice"}},
		{"Support@Example.Com", 20, []string{"alice-work", "alice"}},
		{"support@example.com", 1, []string{"alice-work"}},
		{"example.com", 20, []string{}},
		{"me@example.com", 20, []string{}},
		{"users/123", 20, []string{}},
		{"Élise@example.com", 20, []string{}},
		{"élise@EXAMPLE.com", 20, []string{"accented"}},
		{"", 20, []string{}},
		{"   ", 20, []string{}},
		{"support@example.com", 0, []string{}},
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
// in order, the conversations with a participant other than the self
// identity whose canonical value is the address once ASCII letters are
// lowered on both sides, newest first, then in conversation ID order, and at
// most the limit. Identities of several kinds hold the targets, with their
// ASCII letters in any case; decoys hold a target inside a longer value or
// differ from one only in a non-ASCII letter's case; identities' and
// participants' display names and titles hold targets too; the owner takes
// part in most conversations, and an identity that isn't the owner's holds
// the owner's address in another case; and conversations often share a time.
// So, as invariants, the self identity holds no address, display names and
// titles count for nothing, values that only contain the address never crowd
// out one that is it, and threads that share a time come in ID order. Two
// more hold for every query: any way of writing its ASCII letters' case
// finds the same threads, and SearchConversationsByMetadata for the same text
// at the same limit returns every thread the address search returns, or
// newer ones in its place, so the route tool loses nothing when it counts the
// query itself as an address it has looked up.
func TestSearchConversationsByAddressAgreesWithTheAddressModel(t *testing.T) {
	owner := "me@example.com"
	kinds := map[string]string{
		"support@example.com":                    "email",
		"SUPPORT@example.com":                    "email",
		"Support@Example.com":                    "email",
		"other-support@example.com":              "email",
		"support@example.com.au":                 "email",
		"ME@example.com":                         "email",
		"users/123":                              "username",
		"Users/123":                              "username",
		"users/1234":                             "username",
		"abcdefab-abcd-abcd-abcd-abcdefabcdef":   "signal_aci",
		"ABCDEFAB-abcd-abcd-abcd-abcdefabcdef":   "signal_aci",
		"élise@example.com":                      "email",
		"Élise@example.com":                      "email",
		"+12025550123":                           "e164",
		"legacy-participant:support@example.com": "legacy_participant",
	}
	members := []string{}
	for value := range kinds {
		members = append(members, value)
	}
	sort.Strings(members)
	queries := []string{owner, "", "  ", "support@example.com", "users/123", "abcdefab-abcd-abcd-abcd-abcdefabcdef", "élise@example.com", "example.com", "+12025550123"}
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
			if err := store.UpsertIdentity(sqlite.Identity{
				IdentityID: id, AccountID: "address-account", Kind: sqlite.IdentityKind(kind),
				CanonicalValue: value, RawValue: value, DisplayName: name,
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
			for _, member := range conversation.members {
				participant := sqlite.ConversationParticipant{
					AccountID: "address-account", ConversationID: conversation.id, IdentityID: identities[member],
					Role: sqlite.ParticipantRoleMember, IsActive: true,
				}
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
			limit := 1 + random.Intn(5)
			want := []string{}
			folded := foldASCIIForModel(address)
			crowded := false
			for _, conversation := range threads {
				hit, exact, selfOnly, otherCase, containsOnly := false, false, false, false, false
				for _, member := range conversation.members {
					switch {
					case foldASCIIForModel(member) == folded && member == owner:
						selfOnly = true
					case foldASCIIForModel(member) == folded:
						hit = true
						exact = exact || member == address
					case strings.Contains(foldASCIIForModel(member), folded):
						containsOnly = true
					case strings.EqualFold(member, address):
						otherCase = true
					}
				}
				if strings.TrimSpace(address) == "" || !hit {
					if selfOnly && strings.TrimSpace(address) != "" {
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
					want = append(want, conversation.id)
				}
			}
			got := search(address, limit)
			if !slices.Equal(got, want) {
				dump()
				t.Logf("seed %d: address %q (limit %d) = %v, model says %v", seed, address, limit, got, want)
				return false
			}
			variant := flipASCIICase(random, address)
			if again := search(variant, limit); !slices.Equal(again, got) {
				dump()
				t.Logf("seed %d: address %q (limit %d) = %v, but %q = %v", seed, address, limit, got, variant, again)
				return false
			}
			metadata, err := source.SearchConversationsByMetadata(address, limit)
			if err != nil {
				t.Fatalf("SearchConversationsByMetadata(%q): %v", address, err)
			}
			byText := sourceConversationIDs(metadata)
			if merged := newest(append(slices.Clone(byText), got...), limit); !slices.Equal(merged, byText) {
				dump()
				t.Logf("seed %d: address %q (limit %d) = %v, which the metadata search's %v lacks room for (merged %v)", seed, address, limit, got, byText, merged)
				return false
			}
			if len(want) > 0 {
				exercised["an address with hits"]++
				if !slices.Equal(byText, got) {
					exercised["a metadata search that returns other threads"]++
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
	} {
		if exercised[path] == 0 {
			t.Errorf("the property never drew %s; raise MaxCount (drew %v)", path, exercised)
		}
	}
}
