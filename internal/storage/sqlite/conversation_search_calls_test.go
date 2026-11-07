package sqlite

import (
	"database/sql"
	"database/sql/driver"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/maxghenis/openmessage/internal/addressmatch"
	"github.com/maxghenis/openmessage/internal/phoneline"
	modernsqlite "modernc.org/sqlite"
)

// The counted matchers test an identity by comparing text, or as
// om_address_match and om_phone_line_match do, and count their calls for each
// value they test. Each fixture here gives each identity a canonical value of
// its own, so a value's count is its identity's count.
const (
	countedIdentityMatch = "om_test_counted_identity_match"
	countedAddressMatch  = "om_test_counted_address_match"
	countedLineMatch     = "om_test_counted_phone_line_match"
)

var (
	countedIdentityMatchesMu sync.Mutex
	countedIdentityMatches   = map[string]int{}
)

func init() {
	modernsqlite.MustRegisterDeterministicScalarFunction(countedIdentityMatch, 2, func(_ *modernsqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		if countIdentityMatches(args[1:])[0] == countedText(args[0]) {
			return int64(1), nil
		}
		return int64(0), nil
	})
	modernsqlite.MustRegisterDeterministicScalarFunction(countedAddressMatch, -1, func(_ *modernsqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		key := countedText(args[0])
		for _, value := range countIdentityMatches(args[1:]) {
			if key != "" && addressmatch.Key(value) == key {
				return int64(1), nil
			}
		}
		return int64(0), nil
	})
	modernsqlite.MustRegisterDeterministicScalarFunction(countedLineMatch, -1, func(_ *modernsqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		if phoneline.MatchFirst(countedText(args[0]), countIdentityMatches(args[1:])...) {
			return int64(1), nil
		}
		return int64(0), nil
	})
}

// countIdentityMatches counts a call for each of values and returns their
// text.
func countIdentityMatches(values []driver.Value) []string {
	countedIdentityMatchesMu.Lock()
	defer countedIdentityMatchesMu.Unlock()
	texts := make([]string, len(values))
	for index, value := range values {
		texts[index] = countedText(value)
		countedIdentityMatches[texts[index]]++
	}
	return texts
}

// countedText is the text an SQL argument holds, or "" for anything else.
func countedText(value driver.Value) string {
	switch value := value.(type) {
	case string:
		return value
	case []byte:
		return string(value)
	}
	return ""
}

// countIdentityMatchCalls runs search and returns the counted matchers' calls
// for each value during it.
func countIdentityMatchCalls(search func()) map[string]int {
	countedIdentityMatchesMu.Lock()
	clear(countedIdentityMatches)
	countedIdentityMatchesMu.Unlock()
	search()
	countedIdentityMatchesMu.Lock()
	defer countedIdentityMatchesMu.Unlock()
	return maps.Clone(countedIdentityMatches)
}

// checkOneMatchPerIdentity reports each identity that search tested more than
// once.
func checkOneMatchPerIdentity(t *testing.T, search string, calls map[string]int) {
	t.Helper()
	for _, value := range slices.Sorted(maps.Keys(calls)) {
		if calls[value] > 1 {
			t.Errorf("%s called the matcher %d times for the identity %q, want at most once", search, calls[value], value)
		}
	}
}

const callCountAccount, callCountAt = "calls-account", int64(1000)

// openCallCountStore opens the store at path and closes it when t ends.
func openCallCountStore(t *testing.T, path string) *Store {
	t.Helper()
	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open(): %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close(): %v", err)
		}
	})
	return store
}

// seedCallCountAccount adds callCountAccount to store.
func seedCallCountAccount(t *testing.T, store *Store) {
	t.Helper()
	if err := store.UpsertAccount(Account{
		AccountID: callCountAccount, BridgeKey: "google_messages", DisplayName: callCountAccount, Mode: AccountModeLive, Enabled: true,
		ConfigJSON: `{}`, CreatedAtMS: callCountAt, UpdatedAtMS: callCountAt,
	}); err != nil {
		t.Fatalf("UpsertAccount(): %v", err)
	}
}

// seedCallCountIdentity adds the identity id, with value as its canonical and
// raw value.
func seedCallCountIdentity(t *testing.T, store *Store, id, value string, self bool) {
	t.Helper()
	if err := store.UpsertIdentity(Identity{
		IdentityID: id, AccountID: callCountAccount, Kind: IdentityKind("username"),
		CanonicalValue: value, RawValue: value, IsSelf: self, MetadataJSON: `{}`, CreatedAtMS: callCountAt, UpdatedAtMS: callCountAt,
	}); err != nil {
		t.Fatalf("UpsertIdentity(%s): %v", id, err)
	}
}

// seedCallCountThread adds the group thread id, with the identities members
// in it.
func seedCallCountThread(t *testing.T, store *Store, id string, lastMessageAt int64, members ...string) {
	t.Helper()
	if err := store.UpsertConversation(Conversation{
		ConversationID: id, AccountID: callCountAccount, RemoteConversationID: "remote-" + id, Kind: ConversationKindGroup,
		NotificationMode: NotificationModeAll, LastMessageAtMS: lastMessageAt, MetadataJSON: `{}`, CreatedAtMS: callCountAt, UpdatedAtMS: callCountAt,
	}); err != nil {
		t.Fatalf("UpsertConversation(%s): %v", id, err)
	}
	participants := []ConversationParticipant{}
	for _, member := range members {
		participants = append(participants, ConversationParticipant{
			AccountID: callCountAccount, ConversationID: id, IdentityID: member, Role: ParticipantRoleMember, IsActive: true,
		})
	}
	if err := store.ReplaceConversationParticipants(id, participants); err != nil {
		t.Fatalf("ReplaceConversationParticipants(%s): %v", id, err)
	}
}

// Invariant: the phone line and address searches call the Go matcher at most
// once for each identity, however many threads it joins. Alice and Bob share
// 30 threads with the owner, Carol has one and Dan has none, so a plan that
// tests each participant row calls the matcher 61 times. The test counts the
// calls for each identity, so a plan that tests one identity twice fails even
// when it never tests another. The route tool runs these searches once for
// each number on an address-book card that a name finds.
func TestSearchConversationsByIdentityCallsTheMatcherOncePerIdentity(t *testing.T) {
	store := openCallCountStore(t, filepath.Join(t.TempDir(), "store.sqlite3"))
	seedCallCountAccount(t, store)
	for _, name := range []string{"me", "alice", "bob", "carol", "dan"} {
		seedCallCountIdentity(t, store, "identity-"+name, name, name == "me")
	}
	for index := 0; index < 30; index++ {
		seedCallCountThread(t, store, fmt.Sprintf("shared-%02d", index), callCountAt+int64(index), "identity-me", "identity-alice", "identity-bob")
	}
	seedCallCountThread(t, store, "carols", callCountAt+100, "identity-me", "identity-carol")

	for _, test := range []struct {
		value string
		limit int
		want  []string
	}{
		{"alice", 3, []string{"shared-29", "shared-28", "shared-27"}},
		{"bob", 2, []string{"shared-29", "shared-28"}},
		{"carol", 3, []string{"carols"}},
		{"dan", 3, []string{}},
		{"me", 3, []string{}},
		{"nobody", 3, []string{}},
	} {
		var conversations []Conversation
		var err error
		calls := countIdentityMatchCalls(func() {
			conversations, err = store.searchConversationsByIdentity(countedIdentityMatch+"(?, i.canonical_value)", test.value, test.limit)
		})
		if err != nil {
			t.Fatalf("searchConversationsByIdentity(%q): %v", test.value, err)
		}
		if got := conversationIDs(conversations); !slices.Equal(got, test.want) {
			t.Errorf("searchConversationsByIdentity(%q, %d) = %v, want %v", test.value, test.limit, got, test.want)
		}
		checkOneMatchPerIdentity(t, fmt.Sprintf("searchConversationsByIdentity(%q)", test.value), calls)
	}
}

// Invariant: the bound holds under statistics that would lead SQLite to index
// identities if the query let it. The store never runs ANALYZE, but anyone
// who opens its file can, and can write sqlite_stat1 by hand. With the
// statistics below, SQLite plans the identities query through an automatic
// index unless the query reads identities NOT INDEXED, and that plan calls the
// matcher twice for the identity that matches. The last check shows that
// these statistics still lead to that plan.
func TestSearchConversationsByIdentityCallsTheMatcherOncePerIdentityUnderSkewedStatistics(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.sqlite3")
	store := openCallCountStore(t, path)
	seedCallCountAccount(t, store)
	// 31 people and the owner, whose identity comes last. Even-numbered people
	// have an email address and odd-numbered ones a phone number. Person 0 and
	// the owner share 20 threads.
	const people = 32
	value := func(person int) string {
		if person%2 == 0 {
			return fmt.Sprintf("person%d@example.com", person)
		}
		return fmt.Sprintf("+1202%07d", person)
	}
	for person := 0; person < people; person++ {
		seedCallCountIdentity(t, store, fmt.Sprintf("identity-%02d", person), value(person), person == people-1)
	}
	threads := []string{}
	for index := 0; index < 20; index++ {
		id := fmt.Sprintf("thread-%02d", index)
		seedCallCountThread(t, store, id, callCountAt+int64(index), "identity-00", fmt.Sprintf("identity-%02d", people-1))
		threads = append([]string{id}, threads...)
	}
	for _, statement := range []string{
		`ANALYZE`,
		`UPDATE sqlite_stat1 SET stat = '1000000 1000000 1' WHERE idx = 'conversation_participants_identity_idx'`,
		`UPDATE sqlite_stat1 SET stat = '1 1' WHERE idx = 'sqlite_autoindex_conversations_1'`,
	} {
		result, err := store.db.Exec(statement)
		if err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
		if changed, err := result.RowsAffected(); strings.HasPrefix(statement, "UPDATE") && (err != nil || changed != 1) {
			t.Fatalf("%s changed %d rows (%v), want 1", statement, changed, err)
		}
	}
	// Every connection of a store opened after this plans with the
	// statistics above.
	if err := store.Close(); err != nil {
		t.Fatalf("Close(): %v", err)
	}
	store = openCallCountStore(t, path)

	for _, search := range []struct {
		name, match, value string
		want               []string
	}{
		{"address", strings.Replace(addressmatch.SQL("?", "i.canonical_value"), addressmatch.SQLMatch, countedAddressMatch, 1), addressmatch.Key(value(0)), threads},
		{"phone line", strings.Replace(phoneline.SQL("?", "i.canonical_value"), phoneline.SQLMatch, countedLineMatch, 1), phoneline.Line(value(1)), []string{}},
	} {
		var conversations []Conversation
		var err error
		calls := countIdentityMatchCalls(func() {
			conversations, err = store.searchConversationsByIdentity(search.match, search.value, 80)
		})
		if err != nil {
			t.Fatalf("%s search: %v", search.name, err)
		}
		if got := conversationIDs(conversations); !slices.Equal(got, search.want) {
			t.Errorf("%s search for %q = %v, want %v", search.name, search.value, got, search.want)
		}
		checkOneMatchPerIdentity(t, search.name+" search", calls)

		// Without NOT INDEXED, the same query must still test some identity
		// twice. When it stops, this test no longer covers that plan, and its
		// statistics need a change.
		query := identitySearchSQL(search.match)
		indexable := strings.Replace(query, "identities i NOT INDEXED", "identities i", 1)
		if indexable == query {
			t.Errorf("searchConversationsByIdentity no longer reads identities NOT INDEXED")
			continue
		}
		calls = countIdentityMatchCalls(func() {
			var rows *sql.Rows
			if rows, err = store.db.Query(indexable, search.value, 80); err == nil {
				_, err = collectRows(rows, scanConversation)
			}
		})
		if err != nil {
			t.Fatalf("%s search without NOT INDEXED: %v", search.name, err)
		}
		most := 0
		for _, count := range calls {
			most = max(most, count)
		}
		if most < 2 {
			t.Errorf("the %s search without NOT INDEXED called the matcher at most %d times for each identity, want twice for one: these statistics no longer make SQLite index identities", search.name, most)
		}
	}
}
