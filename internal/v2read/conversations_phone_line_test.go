package v2read

import (
	"fmt"
	"math/rand"
	"sort"
	"strconv"
	"strings"
	"testing"
	"testing/quick"

	"github.com/maxghenis/openmessage/internal/phoneline"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

// Differential, over random v2 stores: SearchConversationsByPhoneLine
// returns, in order, the conversations with a participant other than the
// self identity whose canonical value phoneline.Match puts on the line,
// newest first and at most the limit. Decoys hold a target's digits inside a
// longer number or share only its last seven, and the owner takes part in
// most conversations. So, as invariants, the owner is on no line, and
// threads whose numbers only contain the digits never crowd out one on the
// line.
func TestSearchConversationsByPhoneLineAgreesWithTheLineModel(t *testing.T) {
	numbers := []string{"+12025550123", "+13035550147", "+447555012399", "+4412025550123", "+13035550123", "+12025550124"}
	owner := "+15550009999"
	exercised := map[string]int{}

	property := func(seed int64) bool {
		random := rand.New(rand.NewSource(seed))
		store, _, source := openSourceTestStore(t)
		seedSourceAccount(t, store, "line-account", "google_messages")
		identities := map[string]string{}
		for index, number := range append([]string{owner}, numbers...) {
			id := "identity-" + strconv.Itoa(index)
			if err := store.UpsertIdentity(sqlite.Identity{
				IdentityID: id, AccountID: "line-account", Kind: sqlite.IdentityKind("e164"),
				CanonicalValue: number, RawValue: number, DisplayName: "Person " + strconv.Itoa(index),
				IsSelf: number == owner, MetadataJSON: `{}`, CreatedAtMS: sourceTestTimeMS, UpdatedAtMS: sourceTestTimeMS,
			}); err != nil {
				t.Fatalf("UpsertIdentity(%s): %v", number, err)
			}
			identities[number] = id
		}
		type thread struct {
			id      string
			at      int64
			members []string
		}
		var threads []thread
		for index := 0; index < 3+random.Intn(10); index++ {
			conversation := thread{id: fmt.Sprintf("conversation-%02d", index), at: int64(1000 + 10*random.Intn(100) + index)}
			for _, member := range random.Perm(len(numbers))[:random.Intn(3)] {
				conversation.members = append(conversation.members, numbers[member])
			}
			if random.Intn(4) != 0 {
				conversation.members = append(conversation.members, owner)
			}
			seedSourceConversation(t, store, sqlite.Conversation{
				ConversationID: conversation.id, AccountID: "line-account", RemoteConversationID: "remote-" + conversation.id,
				Kind: sqlite.ConversationKindGroup, Title: "Thread " + strconv.Itoa(index),
				NotificationMode: sqlite.NotificationModeAll, LastMessageAtMS: conversation.at,
			})
			var participants []sqlite.ConversationParticipant
			for _, member := range conversation.members {
				participants = append(participants, sqlite.ConversationParticipant{
					AccountID: "line-account", ConversationID: conversation.id, IdentityID: identities[member],
					Role: sqlite.ParticipantRoleMember, IsActive: true,
				})
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

		for _, number := range append(append([]string{owner, "5550123"}, numbers...), "550123") {
			line := phoneline.Line(number)
			limit := 1 + random.Intn(5)
			want := []string{}
			crowded := false
			for _, conversation := range threads {
				onLine := false
				for _, member := range conversation.members {
					if member != owner && phoneline.Match(line, member) {
						onLine = true
					}
				}
				if !onLine {
					for _, member := range conversation.members {
						if len(want) < limit && len(line) >= 7 && phoneline.Digits(member) != "" &&
							!phoneline.Match(line, member) && containsDigits(member, line) {
							crowded = true
						}
					}
					continue
				}
				if len(want) < limit {
					if crowded {
						exercised["a hit behind a newer thread whose numbers only contain the line's digits"]++
					}
					want = append(want, conversation.id)
				}
			}
			got, err := source.SearchConversationsByPhoneLine(line, limit)
			if err != nil {
				t.Fatalf("SearchConversationsByPhoneLine(%q): %v", line, err)
			}
			gotIDs := []string{}
			for _, conversation := range got {
				gotIDs = append(gotIDs, conversation.ConversationID)
			}
			if fmt.Sprint(gotIDs) != fmt.Sprint(want) {
				for _, conversation := range threads {
					t.Logf("  %s at=%d: %v", conversation.id, conversation.at, conversation.members)
				}
				t.Logf("seed %d: line %q (limit %d) = %v, model says %v", seed, line, limit, gotIDs, want)
				return false
			}
			if len(want) > 0 {
				exercised["a line with hits"]++
			}
		}
		return true
	}
	cases := 60
	if raceDetectorEnabled {
		cases = 15
	}
	if err := quick.Check(property, &quick.Config{MaxCount: cases, Rand: rand.New(rand.NewSource(20261103))}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"a line with hits", "a hit behind a newer thread whose numbers only contain the line's digits"} {
		if exercised[path] == 0 {
			t.Errorf("the property never drew %s; raise MaxCount (drew %v)", path, exercised)
		}
	}
}

func containsDigits(number, line string) bool {
	return strings.Contains(phoneline.Digits(number), line)
}
