package tools

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"path/filepath"
	"testing"
	"testing/quick"

	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/localapi"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
	"github.com/maxghenis/openmessage/internal/v2read"
)

// connectedDaemon answers /api/status as a running app whose platforms are
// connected.
func connectedDaemon(t *testing.T) *localapi.Client {
	t.Helper()
	status := http.NewServeMux()
	status.HandleFunc("/api/status", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"connected": true})
	})
	return daemonClientFor(t, status)
}

// A group's roster can lag behind who writes in it. Here the Work group
// lists Bob and the owner, Sam Owner, but Sam Guest, whom the roster lacks,
// wrote in it too. A search for Sam matches Work only through Sam Guest's
// message, so Work routes, though the owner's own entry holds "Sam" as well.
// The owner's own name and number still route nowhere, though Work lists the
// owner and holds a message the owner sent.
func TestResolveContactRoutesKeepsAThreadThatOnlyAnIncomingSenderMatches(t *testing.T) {
	a := testApp(t)
	if err := a.Store.UpsertConversation(&db.Conversation{
		ConversationID: "work", Name: "Work", IsGroup: true, SourcePlatform: "sms", LastMessageTS: 10,
		Participants: `[{"name":"Bob Example","number":"+15555550199"},{"name":"Sam Owner","number":"+15550009999","is_me":true}]`,
	}); err != nil {
		t.Fatalf("seed Work: %v", err)
	}
	for _, message := range []*db.Message{
		{MessageID: "guest", ConversationID: "work", SenderName: "Sam Guest", SenderNumber: "+15555550177", Body: "running late", TimestampMS: 9, SourcePlatform: "sms"},
		{MessageID: "owner", ConversationID: "work", SenderName: "Sam Owner", SenderNumber: "+15550009999", IsFromMe: true, Body: "no rush", TimestampMS: 10, SourcePlatform: "sms"},
	} {
		if err := a.Store.UpsertMessage(message); err != nil {
			t.Fatalf("seed message %s: %v", message.MessageID, err)
		}
	}
	handler := resolveContactRoutesHandler(a, Options{Reads: a.Store, Daemon: connectedDaemon(t)})

	for _, query := range []string{"Sam", "Sam Guest", "Guest", "+15555550177"} {
		matches := routeMatches(t, callRouteTool(t, handler, map[string]any{"query": query}))
		if len(matches) != 1 || matches[0].PreferredReplyConversationID != "work" {
			t.Errorf("routes for %q = %+v, want one whose preferred route is Work", query, matches)
		}
	}
	for _, query := range append([]string{"Sam Owner", "Owner"}, routeNumberForms("+15550009999")...) {
		if matches := routeMatches(t, callRouteTool(t, handler, map[string]any{"query": query})); len(matches) != 0 {
			t.Errorf("routes for the owner's %q = %+v, want none", query, matches)
		}
	}
}

// seedOwnerOnlyThreads seeds people as decoys: count newer legacy threads, each
// listing one of them and the owner, then Sam Example's older thread. It
// returns Sam Example's thread ID in the store that options reads from.
func seedOwnerOnlyThreads(t *testing.T, legacy *db.Store, store *sqlite.Store, count int, namelessInV2 bool, legacyReads bool) string {
	t.Helper()
	people := make([]routeFixturePerson, 0, count+1)
	for index := 0; index < count; index++ {
		people = append(people, routeFixturePerson{
			Name: fmt.Sprintf("Lee %d", index), Number: fmt.Sprintf("+1555555%04d", 1000+index),
			LegacyThreadID: fmt.Sprintf("decoy-%d", index),
		})
	}
	people = append(people, routeFixturePerson{
		Name: "Sam Example", Number: "+15555550123", LegacyThreadID: "3100", RemoteThreadID: "3100", NamelessInV2: namelessInV2,
	})
	threads := seedRouteFixtureWithOwner(t, legacy, store, routeFixtureOwner{Name: "Sam Owner", Number: "+15550009999"}, people)
	if legacyReads {
		return "3100"
	}
	return threads["Sam Example"]
}

// Most legacy threads list the owner, so a name the owner shares, here Sam,
// appears in hundreds of threads only through the owner's entry. Those
// threads must not use up a search's limit ahead of an older thread that
// matches through someone else: behind 161 newer threads that list only Lee
// and the owner, Sam with limit 1 still finds Sam Example, on either store.
// v2 holds no name for Sam here, so on v2 the tool finds Sam through the
// legacy store's participants and then searches v2 for Sam's number.
func TestResolveContactRoutesFindsAPersonBehindThreadsThatListOnlyTheOwner(t *testing.T) {
	for _, mode := range []string{"legacy", "v2-primary"} {
		t.Run(mode, func(t *testing.T) {
			a := testApp(t)
			store, err := sqlite.Open(filepath.Join(t.TempDir(), "v2.sqlite3"))
			if err != nil {
				t.Fatalf("sqlite.Open(): %v", err)
			}
			defer store.Close()
			want := seedOwnerOnlyThreads(t, a.Store, store, 161, true, mode == "legacy")
			options := Options{Reads: v2read.New(store), V2Primary: true, Daemon: connectedDaemon(t)}
			if mode == "legacy" {
				options = Options{Reads: a.Store, Daemon: connectedDaemon(t)}
			}
			handler := resolveContactRoutesHandler(a, options)

			for _, query := range []string{"Sam", "Sam Example"} {
				matches := routeMatches(t, callRouteTool(t, handler, map[string]any{"query": query, "limit": 1}))
				if len(matches) != 1 || matches[0].PreferredReplyConversationID != want || matches[0].RouteCount != 1 {
					t.Errorf("routes for %q, limit 1 = %+v, want one match with one route, Sam Example's thread %q", query, matches, want)
				}
			}
		})
	}
}

// Property, over random stores and limits: however many newer threads list
// only someone else and the owner, a person who shares the owner's first
// name still gets one match, their own thread, on either store, whether v2
// knows their name or only the legacy store does. The decoys cross every
// limit the tool's searches apply: 8 times the limit for the searches and
// 160 times the limit for the contacts it reads from legacy threads.
func TestResolveContactRoutesThreadsThatListOnlyTheOwnerNeverHideAPerson(t *testing.T) {
	daemon := connectedDaemon(t)
	exercised := map[string]int{}

	property := func(seed int64) bool {
		random := rand.New(rand.NewSource(seed))
		count := random.Intn(500)
		limit := 1 + random.Intn(3)
		legacyReads := random.Intn(2) == 0
		namelessInV2 := random.Intn(2) == 0

		a := testApp(t)
		store, err := sqlite.Open(filepath.Join(t.TempDir(), "v2.sqlite3"))
		if err != nil {
			t.Fatalf("sqlite.Open(): %v", err)
		}
		defer store.Close()
		want := seedOwnerOnlyThreads(t, a.Store, store, count, namelessInV2, legacyReads)
		options := Options{Reads: v2read.New(store), V2Primary: true, Daemon: daemon}
		if legacyReads {
			options = Options{Reads: a.Store, Daemon: daemon}
		}
		handler := resolveContactRoutesHandler(a, options)

		for _, query := range []string{"Sam", "Sam Example", "+1 (555) 555-0123"} {
			matches := routeMatches(t, callRouteTool(t, handler, map[string]any{"query": query, "limit": limit}))
			if len(matches) != 1 || matches[0].PreferredReplyConversationID != want || matches[0].RouteCount != 1 {
				t.Logf("seed %d: %d decoys, limit %d, legacy reads %v, nameless in v2 %v: routes for %q = %+v, want one match with one route, %q",
					seed, count, limit, legacyReads, namelessInV2, query, matches, want)
				return false
			}
		}
		switch {
		case legacyReads && count > 8*limit:
			exercised["legacy reads behind more decoys than the search limit"]++
		case !legacyReads && namelessInV2 && count > 160*limit:
			exercised["v2 reads, a legacy-only name, behind more decoys than the contacts limit"]++
		}
		return true
	}
	cases := 24
	if raceDetectorEnabled {
		cases = 8
	}
	if err := quick.Check(property, &quick.Config{MaxCount: cases, Rand: rand.New(rand.NewSource(20261103))}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		"legacy reads behind more decoys than the search limit",
		"v2 reads, a legacy-only name, behind more decoys than the contacts limit",
	} {
		if exercised[path] == 0 {
			t.Errorf("the property never drew %s; raise MaxCount (drew %v)", path, exercised)
		}
	}
}
