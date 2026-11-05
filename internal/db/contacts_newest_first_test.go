package db

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"testing/quick"
)

// contactRow is one row of ListContactsFromConversations' query.
type contactRow struct {
	id, name, participants string
}

// contactRows runs one form of ListContactsFromConversations' query with the
// filter that the function would give it for query.
func contactRows(t *testing.T, store *Store, indexed bool, query string) []contactRow {
	t.Helper()
	filter := query
	if strings.ContainsRune(query, '�') {
		filter = ""
	}
	rows, err := store.db.Query(contactsFromConversationsSQL(indexed), filter, filter, filter)
	if err != nil {
		t.Fatalf("query (indexed %v): %v", indexed, err)
	}
	defer rows.Close()
	var got []contactRow
	for rows.Next() {
		var row contactRow
		if err := rows.Scan(&row.id, &row.name, &row.participants); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return got
}

// contactNames and contactNumbers fill the threads of a random store for
// ListContactsFromConversations: names that hold others inside them, names
// with letters outside ASCII in either case, a name with an ampersand,
// which Go's encoder writes as an escape, and the owner's name.
var (
	contactNames   = []string{"Ann", "Anna", "Ángel Ruiz", "ÁNGEL", "Bob & Co", "Zoë", "Me", ""}
	contactNumbers = []string{"+15555550100", "(202) 555-0123", "ann@example.com", "ANN@example.com", "72975", ""}
)

// seedContactThreads writes random threads into store. Participants come
// as JSON from Go's encoder, which writes & and < as escapes; as JSON
// written by hand, sometimes with a byte that isn't UTF-8; or as text that
// isn't a JSON array. A few threads list so many people that their
// participants overflow an index page. The owner's entries carry is_me or
// isMe, and threads often share a time.
func seedContactThreads(t *testing.T, random *rand.Rand, store *Store) {
	t.Helper()
	for index := 0; index < 3+random.Intn(25); index++ {
		id := fmt.Sprintf("c%02d", index)
		var entries []map[string]any
		members := 1 + random.Intn(3)
		if random.Intn(8) == 0 {
			members = 20 + random.Intn(140)
		}
		for member := 0; member < members; member++ {
			entry := map[string]any{"name": contactNames[random.Intn(len(contactNames))], "number": contactNumbers[random.Intn(len(contactNumbers))]}
			switch random.Intn(6) {
			case 0:
				entry["is_me"] = true
			case 1:
				entry["isMe"] = true
			}
			entries = append(entries, entry)
		}
		encoded, err := json.Marshal(entries)
		if err != nil {
			t.Fatal(err)
		}
		participants := string(encoded)
		switch random.Intn(10) {
		case 0:
			participants = `[{"name":"Ann` + "\xff" + `","number":"+15555550101"}]`
		case 1:
			participants = `[{"name":"Anna","number":"+15555550102"}]`
		case 2:
			participants = `{"name":"Ann"}`
		case 3:
			participants = ""
		}
		name := "Thread " + strconv.Itoa(index)
		if random.Intn(3) == 0 {
			name = contactNames[random.Intn(len(contactNames))]
		}
		if err := store.UpsertConversation(&Conversation{
			ConversationID: id, Name: name, Participants: participants,
			LastMessageTS: int64(1_000_000 + 1000*random.Intn(5)), SourcePlatform: "sms",
		}); err != nil {
			t.Fatalf("seed conversation: %v", err)
		}
	}
}

// contactQueries are what an autocomplete or the route tool asks
// ListContactsFromConversations for.
var contactQueries = []string{"", "a", "an", "Ann", "ann", "ÁNGEL", "Ángel", "ángel", "Me", "555", "+1", `\`, "&", "�", "\xff", "Bob & Co", "nobody", "zoë"}

// Differential, over random stores: ListContactsFromConversations' query
// returns the same rows in the same order whether it reads them through
// idx_conversations_newest_first or sorts them, and the function returns
// exactly the contacts it returned before it read the index: the same ones,
// in the same order, at most the limit. With the index dropped, the function
// runs the sorted query and returns them again, and every other read of the
// conversations returns what it returned with the index, since only a query
// that states the index's term may use a partial index.
func TestListContactsFromConversationsReadsTheIndexInTheSortedOrder(t *testing.T) {
	exercised := map[string]int{}
	property := func(seed int64) bool {
		random := rand.New(rand.NewSource(seed))
		store := newTestStore(t)
		seedContactThreads(t, random, store)
		seedBatchStore(t, random, store)

		type answer struct {
			contacts      []*Contact
			conversations []*Conversation
			metadata      []*Conversation
			lines         []*Conversation
			addresses     []*Conversation
		}
		limits := map[string]int{}
		read := func() map[string]answer {
			answers := map[string]answer{}
			for _, query := range contactQueries {
				limit := limits[query]
				contacts, err := store.ListContactsFromConversations(query, limit)
				if err != nil {
					t.Fatalf("ListContactsFromConversations(%q, %d): %v", query, limit, err)
				}
				conversations, err := store.ListConversations(limit)
				if err != nil {
					t.Fatalf("ListConversations(%d): %v", limit, err)
				}
				metadata, err := store.SearchConversationsByMetadata(query, limit)
				if err != nil {
					t.Fatalf("SearchConversationsByMetadata(%q, %d): %v", query, limit, err)
				}
				lines, err := store.SearchConversationsByPhoneLines([]string{"2025550123", "5555550100"}, limit)
				if err != nil {
					t.Fatalf("SearchConversationsByPhoneLines(): %v", err)
				}
				addresses, err := store.SearchConversationsByAddresses([]string{query, "ann@example.com"}, limit)
				if err != nil {
					t.Fatalf("SearchConversationsByAddresses(): %v", err)
				}
				answers[query] = answer{contacts, conversations, metadata, lines, addresses}
			}
			return answers
		}
		for _, query := range contactQueries {
			limits[query] = []int{0, 1, 2, 3, 5, 1000}[random.Intn(6)]
			sorted := contactRows(t, store, false, query)
			indexed := contactRows(t, store, true, query)
			if !reflect.DeepEqual(indexed, sorted) {
				t.Logf("seed %d: rows for %q through the index = %v, sorted = %v", seed, query, indexed, sorted)
				return false
			}
			contacts, err := store.ListContactsFromConversations(query, limits[query])
			if err != nil {
				t.Fatalf("ListContactsFromConversations(%q): %v", query, err)
			}
			if len(contacts) > 0 && contacts[len(contacts)-1].ContactID != sorted[len(sorted)-1].id {
				exercised["contacts that run out before the last row the prefilter passes"]++
			}
			if every, err := store.ListContactsFromConversations(query, 1000); err == nil && len(every) > limits[query] && limits[query] > 0 {
				exercised["a limit that cuts the contacts"]++
			}
			for index := 1; index < len(sorted); index++ {
				if sorted[index].id > sorted[index-1].id && strings.Contains(sorted[index].participants, `\`) == strings.Contains(sorted[index-1].participants, `\`) {
					exercised["rows that come in ID order"]++
					break
				}
			}
			if query != "" && !strings.ContainsRune(query, '�') {
				for _, row := range sorted {
					if strings.Contains(row.participants, `\`) && !strings.Contains(strings.ToLower(row.participants), strings.ToLower(query)) && !strings.Contains(strings.ToLower(row.name), strings.ToLower(query)) {
						exercised["a row that only the backslash passes"]++
						break
					}
				}
			}
		}
		before := read()
		if _, err := store.db.Exec(`DROP INDEX ` + newestFirstIndex); err != nil {
			t.Fatalf("drop the index: %v", err)
		}
		after := read()
		for _, query := range contactQueries {
			if !reflect.DeepEqual(before[query], after[query]) {
				t.Logf("seed %d: for %q (limit %d), reads with the index = %+v, without = %+v", seed, query, limits[query], before[query], after[query])
				return false
			}
		}
		return true
	}
	cases := 120
	if raceDetectorEnabled {
		cases = 25
	}
	if err := quick.Check(property, &quick.Config{MaxCount: cases, Rand: rand.New(rand.NewSource(20261104))}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		"contacts that run out before the last row the prefilter passes",
		"a limit that cuts the contacts",
		"rows that come in ID order",
		"a row that only the backslash passes",
	} {
		if exercised[path] == 0 {
			t.Errorf("the property never drew %s (drew %v)", path, exercised)
		}
	}
}

// queryPlan returns the details of query's plan.
func queryPlan(t *testing.T, store *Store, query string, args ...any) string {
	t.Helper()
	rows, err := store.db.Query("EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()
	var details []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		details = append(details, detail)
	}
	return strings.Join(details, "\n")
}

// ListContactsFromConversations reads conversations from the newest-first
// index alone, without sorting them or going back to the table, so it stops
// at its limit and a query that reads every row costs what a scan of the
// table does. The sorted form it falls back to sorts every row. migrate
// creates the index with its term, and no other query here may use it: each
// search's plan leaves it alone.
func TestListContactsFromConversationsPlanReadsTheNewestFirstIndex(t *testing.T) {
	store := newTestStore(t)
	var definition string
	if err := store.db.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'index' AND name = ?`, newestFirstIndex).Scan(&definition); err != nil {
		t.Fatalf("read the index: %v", err)
	}
	if !strings.Contains(definition, "(last_message_ts DESC, conversation_id, name, participants) WHERE "+newestFirstIndexTerm) {
		t.Errorf("migrate created %s as %q", newestFirstIndex, definition)
	}
	plan := queryPlan(t, store, contactsFromConversationsSQL(true), "ann", "ann", "ann")
	if !strings.Contains(plan, "USING COVERING INDEX "+newestFirstIndex) || strings.Contains(plan, "TEMP B-TREE") {
		t.Errorf("the indexed query's plan = %q, want a scan of %s alone with no sort", plan, newestFirstIndex)
	}
	if plan := queryPlan(t, store, contactsFromConversationsSQL(false), "ann", "ann", "ann"); !strings.Contains(plan, "TEMP B-TREE") {
		t.Errorf("the sorted query's plan = %q, want a sort", plan)
	}
	for name, query := range map[string]string{
		"phoneLineSearchSQL":   phoneLineSearchSQL("om_phone_line_match"),
		"phoneLinesSearchSQL":  phoneLinesSearchSQL("om_phone_line_keys"),
		"addressSearchSQL":     addressSearchSQL,
		"addressesSearchSQL":   addressesSearchSQL(2),
		"sorted contacts form": contactsFromConversationsSQL(false),
	} {
		count := strings.Count(query, "?")
		args := make([]any, count)
		for index := range args {
			args[index] = "x"
		}
		if plan := queryPlan(t, store, query, args...); strings.Contains(plan, newestFirstIndex) {
			t.Errorf("%s's plan uses %s: %q", name, newestFirstIndex, plan)
		}
	}
}

// A store whose index is missing, as one is when migrate fails to create
// it, still lists contacts, through the sorted query.
func TestListContactsFromConversationsSortsWithoutTheIndex(t *testing.T) {
	store := newTestStore(t)
	for index, conversation := range []*Conversation{
		{ConversationID: "old", Name: "Old", LastMessageTS: 1, Participants: `[{"name":"Ann Old","number":"+15555550100"}]`},
		{ConversationID: "new", Name: "New", LastMessageTS: 3, Participants: `[{"name":"Ann New","number":"+15555550101"}]`},
		{ConversationID: "tie-b", Name: "Tie", LastMessageTS: 2, Participants: `[{"name":"Ann B","number":"+15555550102"}]`},
		{ConversationID: "tie-a", Name: "Tie", LastMessageTS: 2, Participants: `[{"name":"Ann A","number":"+15555550103"}]`},
	} {
		if err := store.UpsertConversation(conversation); err != nil {
			t.Fatalf("seed %d: %v", index, err)
		}
	}
	want := []string{"Ann New", "Ann A", "Ann B"}
	for _, drop := range []bool{false, true} {
		if drop {
			if _, err := store.db.Exec(`DROP INDEX ` + newestFirstIndex); err != nil {
				t.Fatalf("drop the index: %v", err)
			}
		}
		contacts, err := store.ListContactsFromConversations("ann", 3)
		if err != nil {
			t.Fatalf("ListContactsFromConversations (index dropped %v): %v", drop, err)
		}
		var names []string
		for _, contact := range contacts {
			names = append(names, contact.Name)
		}
		if !reflect.DeepEqual(names, want) {
			t.Errorf("contacts for ann (index dropped %v) = %v, want %v", drop, names, want)
		}
	}
}
