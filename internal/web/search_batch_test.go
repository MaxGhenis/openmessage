package web

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"
	"testing/quick"

	"github.com/rs/zerolog"

	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/readsource"
	"github.com/maxghenis/openmessage/internal/storage/sqlite/sqlitetest"
	"github.com/maxghenis/openmessage/internal/v2read"
)

// referenceMergeSearchResults is mergeSearchResults as it stood before lookups
// were memoized and batched (origin/main 19e35d9), kept verbatim as the
// differential oracle: one GetConversation per message hit, checked against
// seen only afterwards, and one GetMessagesByConversation(id, 1) per
// conversation-only hit.
func referenceMergeSearchResults(reads readsource.ReadSource, identityStore *db.Store, msgs []*db.Message, convos []*db.Conversation, limit int) []SearchResult {
	results := make([]SearchResult, 0, limit)
	seen := make(map[string]struct{}, limit)
	var identityIndex map[string]unifiedConversationIdentity
	if identityStore != nil {
		identityIndex = loadUnifiedIdentityIndex(identityStore)
	}

	appendResult := func(result SearchResult) {
		if _, ok := seen[result.ConversationID]; ok {
			return
		}
		seen[result.ConversationID] = struct{}{}
		results = append(results, result)
	}

	for _, msg := range msgs {
		conv, err := reads.GetConversation(msg.ConversationID)
		if err != nil || conv == nil {
			continue
		}
		appendResult(searchResultForConversation(conv, msg.TimestampMS, searchPreviewForMessage(msg), identityIndex))
	}

	for _, conv := range convos {
		if _, ok := seen[conv.ConversationID]; ok {
			continue
		}
		preview := ""
		msgs, err := reads.GetMessagesByConversation(conv.ConversationID, 1)
		if err == nil && len(msgs) > 0 {
			preview = searchPreviewForMessage(msgs[0])
		}
		appendResult(searchResultForConversation(conv, conv.LastMessageTS, preview, identityIndex))
	}

	sort.Slice(results, func(i, j int) bool {
		if results[i].LastMessageTS != results[j].LastMessageTS {
			return results[i].LastMessageTS > results[j].LastMessageTS
		}
		return results[i].ConversationID < results[j].ConversationID
	})
	if len(results) > limit {
		results = results[:limit]
	}
	return results
}

// plainReads hides every method but ReadSource's, so mergeSearchResults takes
// its memoized per-ID path instead of the batch prefetch.
type plainReads struct{ readsource.ReadSource }

func TestMergeSearchResultsMatchesPerHitReferenceProperty(t *testing.T) {
	count := 30
	if testing.Short() {
		count = 6
	}
	config := &quick.Config{MaxCount: count, Rand: rand.New(rand.NewSource(20261009))}
	property := func(seed int64) bool {
		rs := sqlitetest.BuildRandomStore(t, seed, sqlitetest.DenseShape)
		source := v2read.New(rs.Store)
		rng := rand.New(rand.NewSource(seed))
		ok := true
		check := func(label string, msgs []*db.Message, convos []*db.Conversation, limit int) {
			want, err := json.Marshal(referenceMergeSearchResults(source, nil, msgs, convos, limit))
			if err != nil {
				t.Fatal(err)
			}
			for name, reads := range map[string]readsource.ReadSource{"batched": source, "memoized": plainReads{source}} {
				got, err := json.Marshal(mergeSearchResults(reads, nil, msgs, convos, limit))
				if err != nil {
					t.Fatal(err)
				}
				if string(got) != string(want) {
					t.Errorf("seed %d %s (%s):\n got: %s\nwant: %s", seed, label, name, got, want)
					ok = false
				}
			}
		}
		for _, term := range rs.SearchTerms {
			for _, limit := range []int{1, 5, 50, 500} {
				msgs, err := source.SearchMessagesFiltered(term, db.SearchFilter{Limit: limit})
				if err != nil {
					t.Fatalf("SearchMessagesFiltered(%q): %v", term, err)
				}
				convos, err := source.SearchConversationsByMetadata(term, limit)
				if err != nil {
					convos = nil
				}
				check(fmt.Sprintf("term %q limit %d", term, limit), msgs, convos, limit)
			}
		}

		// Hits whose conversation IDs are not plain v2 IDs, repeated IDs, and
		// conversation hits that are also message hits: the per-ID fallback
		// and the memo must answer exactly as the per-hit reads did.
		all, err := source.ListConversations(1 << 20)
		if err != nil {
			t.Fatal(err)
		}
		var msgs []*db.Message
		for _, conversationID := range rs.ConversationIDs {
			page, err := source.GetMessagesByConversation(conversationID, 3)
			if err != nil {
				t.Fatal(err)
			}
			msgs = append(msgs, page...)
		}
		aliases := append([]string{"missing", "", " "}, rs.RemoteIDs...)
		for i := 0; i < len(msgs); i++ {
			if rng.Intn(4) == 0 && len(aliases) > 0 {
				clone := *msgs[i]
				clone.ConversationID = aliases[rng.Intn(len(aliases))]
				if rng.Intn(3) == 0 {
					clone.ConversationID = " " + clone.ConversationID
				}
				msgs = append(msgs, &clone)
			}
		}
		rng.Shuffle(len(msgs), func(i, j int) { msgs[i], msgs[j] = msgs[j], msgs[i] })
		rng.Shuffle(len(all), func(i, j int) { all[i], all[j] = all[j], all[i] })
		for _, limit := range []int{1, 7, 500} {
			check(fmt.Sprintf("synthetic hits limit %d", limit), msgs, all, limit)
		}
		return ok
	}
	if err := quick.Check(property, config); err != nil {
		t.Fatal(err)
	}
}

// /api/conversations and /api/search cost a fixed number of v2 statements
// however many conversations they return or hits they merge.
func TestConversationListAndSearchStatementsDoNotGrowWithRows(t *testing.T) {
	legacy, err := db.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = legacy.Close() })

	measure := func(conversations int) map[string]int {
		built := sqlitetest.BuildUniformStore(t, sqlitetest.Uniform{
			Accounts: 2, Conversations: conversations, Participants: 5, MessagesPer: 2,
		})
		handler := APIHandlerWithOptions(legacy, nil, zerolog.Nop(), nil, APIOptions{
			Reads:     v2read.New(built.Store),
			V2Primary: true,
		})
		counts := map[string]int{}
		for _, path := range []string{
			"/api/conversations?limit=200",
			"/api/search?q=needle&limit=200",
			"/api/search?q=Person&limit=200",
			"/api/search/messages?q=needle&limit=200",
			"/api/conversations/" + built.ConversationIDs[0] + "/messages?limit=100",
		} {
			built.Counter.Reset()
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "http://127.0.0.1"+path, nil))
			if recorder.Code != http.StatusOK {
				t.Fatalf("GET %s: %d %s", path, recorder.Code, recorder.Body.String())
			}
			counts[path[:min(len(path), 40)]] = built.Counter.Count()
		}
		return counts
	}
	small, large := measure(4), measure(150)
	t.Logf("v2 statements per request, 8 vs 300 conversations: %v vs %v", small, large)
	for path, count := range large {
		// A page with no outgoing message (or no attributed sender) skips
		// that batch, so the larger store can come in under the smaller.
		if count > small[path] {
			t.Errorf("GET %s: %d statements for 8 conversations, %d for 300", path, small[path], count)
		}
		if count > 12 {
			t.Errorf("GET %s: %d statements, want <= 12", path, count)
		}
	}
}
