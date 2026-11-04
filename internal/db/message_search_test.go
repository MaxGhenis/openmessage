package db

import (
	"bytes"
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"testing/quick"
	"time"
	"unicode/utf8"

	"github.com/rs/zerolog"
)

// searchLetters are ASCII letters, which LIKE matches ignoring case, and
// letters with a case pair outside ASCII, which it matches only exactly while
// the trigram index folds them: É and é, the Kelvin sign and k, long s and s.
var searchLetters = []string{
	"a", "A", "b", "B", "e", "E", "k", "K", "n", "N", "s", "S", "i", "I",
	"é", "É", "ß", "\u1e9e", // capital sharp s
	"\u0131", "\u0130", // dotless i, capital I with a dot
	"\u212a", "\u017f", // the Kelvin sign, long s
	"σ", "Σ", "\u03c2", // final sigma
}

// searchOddAtoms are the other pieces of random bodies and queries: FTS5 query
// syntax, LIKE's wildcards, whitespace, NUL, multi-byte characters, a
// combining mark, and bytes that aren't valid UTF-8.
var searchOddAtoms = []string{
	"0", "5", " ", "\t", "\n",
	`"`, "*", "^", ":", "(", ")", "-", "+", "{", "}", ",", ".", "'", `\`,
	"AND", "OR", "NOT", "NEAR", "near",
	"%", "_",
	"\x00",
	"😀", "日", "\u0301", "\ufffd", // a combining acute accent, the replacement character
	"\xc3", "\xff", "\x80", "\xed\xa0\x80",
}

// searchQueries are queries that test one rule each: quoting, FTS5 syntax,
// wildcards, NUL, short runs and invalid UTF-8.
var searchQueries = []string{
	"", " ", "   ", `"`, `""`, `"""`, `a"b`, `"ab"`, `a""b`,
	"NEAR(a b)", "a AND b", "a OR b", "NOT a", "a*", "^ab", "body:abc", "message_id:a",
	"%", "_", "%%", "a%b", "abc%", "%abc", "ab_c", "a_bc_de", "abc_def",
	"\x00", "ab\x00c", "abc\x00", "\x00abc", "abc\x00def",
	"\xc3\xc3\xc3", "\xed\xa0\x80ab", "ÉÉÉ", "ééé", "KKK", "kkk", "\u212a\u212a\u212a", "SSS", "\u017f\u017f\u017f",
}

// caseSwaps maps each letter to its other case, ASCII and not.
var caseSwaps = map[string]string{
	"é": "É", "É": "é", "ß": "\u1e9e", "\u1e9e": "ß", "\u0131": "I", "\u0130": "i",
	"\u212a": "k", "\u017f": "s", "σ": "Σ", "Σ": "σ", "\u03c2": "Σ",
}

func swapCase(atom string) string {
	if swapped, ok := caseSwaps[atom]; ok {
		return swapped
	}
	if strings.ToLower(atom) != atom {
		return strings.ToLower(atom)
	}
	return strings.ToUpper(atom)
}

type searchCaseMessage struct {
	Atoms              []string
	Conv, Sender, Time int
}

type searchCaseQuery struct {
	Atoms  []string // the query is their concatenation
	Text   string   // the query when Atoms is nil
	Filter SearchFilter
}

func (q searchCaseQuery) query() string {
	if q.Atoms == nil {
		return q.Text
	}
	return strings.Join(q.Atoms, "")
}

// searchCase is a store of messages and the searches to run on it. With
// BigStore, the router believes the store holds a million messages, so it
// takes the index for a rare query, as it would on a large store.
type searchCase struct {
	Messages []searchCaseMessage
	Queries  []searchCaseQuery
	BigStore bool
}

var (
	searchConvs   = []string{"c0", "c1", "c2"}
	searchSenders = []string{"+15550001", "+15550002"}
	searchTimes   = []int64{0, 1000, 2000, 3000}
)

func randomSearchAtom(r *rand.Rand) string {
	if r.Intn(2) == 0 {
		return searchLetters[r.Intn(len(searchLetters))]
	}
	return searchOddAtoms[r.Intn(len(searchOddAtoms))]
}

func randomSearchFilter(r *rand.Rand) SearchFilter {
	var f SearchFilter
	if r.Intn(4) == 0 {
		f.Phone = searchSenders[r.Intn(len(searchSenders))]
	}
	if r.Intn(4) == 0 {
		f.ConversationID = searchConvs[r.Intn(len(searchConvs))]
	}
	if r.Intn(4) == 0 {
		f.SinceMS = searchTimes[r.Intn(len(searchTimes))]
	}
	if r.Intn(4) == 0 {
		f.UntilMS = searchTimes[r.Intn(len(searchTimes))]
	}
	f.Limit = r.Intn(13) // 0 takes the default
	return f
}

func (searchCase) Generate(r *rand.Rand, _ int) reflect.Value {
	c := searchCase{Messages: make([]searchCaseMessage, r.Intn(25)), BigStore: r.Intn(2) == 0}
	for i := range c.Messages {
		atoms := make([]string, r.Intn(11))
		for j := range atoms {
			atoms[j] = randomSearchAtom(r)
		}
		c.Messages[i] = searchCaseMessage{Atoms: atoms, Conv: r.Intn(len(searchConvs)), Sender: r.Intn(len(searchSenders)), Time: r.Intn(len(searchTimes))}
	}
	c.Queries = make([]searchCaseQuery, 8)
	for i := range c.Queries {
		q := searchCaseQuery{Filter: randomSearchFilter(r)}
		switch kind := r.Intn(10); {
		case kind < 4 && len(c.Messages) > 0:
			// A piece of a stored body, some letters in the other case.
			body := c.Messages[r.Intn(len(c.Messages))].Atoms
			if len(body) == 0 {
				q.Atoms = []string{}
				break
			}
			start := r.Intn(len(body))
			end := start + 1 + r.Intn(min(6, len(body)-start))
			q.Atoms = append([]string{}, body[start:end]...)
			for j := range q.Atoms {
				if r.Intn(3) == 0 {
					q.Atoms[j] = swapCase(q.Atoms[j])
				}
			}
		case kind < 7:
			q.Atoms = make([]string, r.Intn(6))
			for j := range q.Atoms {
				q.Atoms[j] = randomSearchAtom(r)
			}
		default:
			q.Text = searchQueries[r.Intn(len(searchQueries))]
		}
		c.Queries[i] = q
	}
	return reflect.ValueOf(c)
}

// normalizedSearchFilter applies SearchMessagesFiltered's defaults, for calls
// that go straight to one search path.
func normalizedSearchFilter(f SearchFilter) SearchFilter {
	if f.Limit <= 0 {
		f.Limit = 20
	}
	if f.SinceMS > 0 && f.UntilMS > 0 && f.UntilMS < f.SinceMS {
		f.SinceMS, f.UntilMS = f.UntilMS, f.SinceMS
	}
	return f
}

func messageIDs(msgs []*Message) []string {
	ids := make([]string, len(msgs))
	for i, m := range msgs {
		ids[i] = m.MessageID
	}
	return ids
}

// queryIDs returns the message_id of every row query returns.
func queryIDs(t testing.TB, s *Store, query string, args ...any) (map[string]bool, error) {
	t.Helper()
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids[id] = true
	}
	return ids, rows.Err()
}

// emptyStore deletes every message and its index entry, and forgets the
// counts the search router read.
func emptyStore(t testing.TB, s *Store) {
	t.Helper()
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatalf("Begin(): %v", err)
	}
	if _, err := s.deleteMessages(tx, "1 = 1"); err != nil {
		tx.Rollback()
		t.Fatalf("empty the store: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit(): %v", err)
	}
	s.countsReadAt = time.Time{}
}

// The index path, the router and the LIKE scan return the same messages in
// the same order, for any store whose index holds one entry per message with
// its stored body (what every write path keeps, see
// TestSearchIndexMatchesMessagesOverRandomWrites), any query and any filter.
// The probe's query never fails to parse and selects every message the LIKE
// pattern matches.
func TestSearchPathsAgreeWithLikeOverRandomStores(t *testing.T) {
	store := newTestStore(t)
	if !store.ftsEnabled {
		t.Skip("fts index not enabled in this sqlite build")
	}
	seen := map[string]int{}
	property := func(c searchCase) bool {
		emptyStore(t, store)
		for i, m := range c.Messages {
			msg := &Message{
				MessageID:      fmt.Sprintf("m%02d", i),
				ConversationID: searchConvs[m.Conv],
				SenderNumber:   searchSenders[m.Sender],
				Body:           strings.Join(m.Atoms, ""),
				TimestampMS:    searchTimes[m.Time],
			}
			if err := store.UpsertMessage(msg); err != nil {
				t.Fatalf("UpsertMessage(%q): %v", msg.Body, err)
			}
		}
		if c.BigStore {
			store.counts = searchCounts{messages: 1_000_000, indexInSync: true}
			store.countsReadAt = time.Now()
		}
		for _, sq := range c.Queries {
			query, f := sq.query(), normalizedSearchFilter(sq.Filter)
			like, err := store.searchMessagesLike(query, f)
			if err != nil {
				t.Fatalf("searchMessagesLike(%q, %+v): %v", query, f, err)
			}
			indexed, err := store.searchMessagesIndexed(query, f)
			if err != nil {
				t.Logf("searchMessagesIndexed(%q, %+v): %v", query, f, err)
				return false
			}
			if !reflect.DeepEqual(indexed, like) {
				t.Logf("query %q, filter %+v: index path %q, LIKE %q", query, f, messageIDs(indexed), messageIDs(like))
				return false
			}
			pays, err := store.searchIndexPays(query, f)
			if err != nil {
				t.Logf("searchIndexPays(%q, %+v): %v", query, f, err)
				return false
			}
			routed, err := store.SearchMessagesFiltered(query, sq.Filter)
			if err != nil {
				t.Fatalf("SearchMessagesFiltered(%q, %+v): %v", query, sq.Filter, err)
			}
			if !reflect.DeepEqual(routed, like) {
				t.Logf("query %q, filter %+v: router %q, LIKE %q", query, sq.Filter, messageIDs(routed), messageIDs(like))
				return false
			}

			matches, err := queryIDs(t, store, `SELECT message_id FROM messages WHERE body LIKE ?`, searchLikePattern(query))
			if err != nil {
				t.Fatalf("LIKE %q: %v", query, err)
			}
			if match, _, _ := searchIndexMatch(query); match != "" {
				candidates, err := queryIDs(t, store, `SELECT message_id FROM messages_fts WHERE messages_fts MATCH ?`, match)
				if err != nil {
					t.Logf("query %q: probe %q: %v", query, match, err)
					return false
				}
				for id := range matches {
					if !candidates[id] {
						t.Logf("query %q: probe %q misses %s, which LIKE matches", query, match, id)
						return false
					}
				}
				seen["indexable query"]++
				if len(candidates) > len(matches) {
					seen["probe finds rows LIKE drops"]++
				}
			} else {
				seen["query the index can't narrow"]++
			}
			if pays {
				seen["router takes the index"]++
			} else {
				seen["router takes the scan"]++
			}
			if len(like) > 0 {
				seen["a match"]++
			}
			if len(like) == f.Limit {
				seen["limit cuts the matches"]++
			}
			if f.Phone != "" || f.ConversationID != "" || f.SinceMS > 0 || f.UntilMS > 0 {
				seen["filtered"]++
			}
			for _, r := range []struct{ name, chars string }{
				{"query with a double quote", `"`},
				{"query with a LIKE wildcard", "%_"},
				{"query with NUL", "\x00"},
				{"query with FTS5 syntax", "*^:()+-"},
			} {
				if strings.ContainsAny(query, r.chars) {
					seen[r.name]++
				}
			}
			for _, kw := range []string{"AND", "OR", "NOT", "NEAR"} {
				if strings.Contains(query, kw) {
					seen["query with an FTS5 keyword"]++
				}
			}
			if !utf8.ValidString(query) {
				seen["query with invalid UTF-8"]++
			}
			if len(like) > 0 && hasASCIICaseVariant(query, like) {
				seen["match in the other ASCII case"]++
			}
		}
		return true
	}
	cases := 500
	if raceDetectorEnabled {
		cases = 100
	}
	if err := quick.Check(property, &quick.Config{MaxCount: cases, Rand: rand.New(rand.NewSource(20261103))}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"indexable query", "probe finds rows LIKE drops", "query the index can't narrow",
		"router takes the index", "router takes the scan", "a match", "limit cuts the matches", "filtered",
		"query with a double quote", "query with a LIKE wildcard", "query with NUL", "query with FTS5 syntax",
		"query with an FTS5 keyword", "query with invalid UTF-8", "match in the other ASCII case",
	} {
		if seen[want] == 0 {
			t.Errorf("no search drew a %s", want)
		}
	}
	t.Logf("searches drawn: %v", seen)
}

// hasASCIICaseVariant reports whether some message in msgs contains query only
// with ASCII letters in the other case.
func hasASCIICaseVariant(query string, msgs []*Message) bool {
	if query == "" || strings.ContainsAny(query, "%_\x00") {
		return false
	}
	for _, m := range msgs {
		if !strings.Contains(m.Body, query) {
			return true
		}
	}
	return false
}

// Each rule the index path must follow to return LIKE's rows, on one store.
// LIKE ignores case for ASCII letters only, and its % and _ stay wildcards.
func TestSearchIndexPathFollowsLike(t *testing.T) {
	store := newTestStore(t)
	if !store.ftsEnabled {
		t.Skip("fts index not enabled in this sqlite build")
	}
	bodies := map[string]string{
		"ascii":   "Dinner at Nine",
		"accent":  "école",
		"ACCENT":  "ÉCOLE",
		"quote":   `she said "hi there"`,
		"syntax":  "NEAR(a b) AND c* ^d body:e",
		"percent": "50% off today",
		"under":   "file_name.txt",
		"kelvin":  "300 \u212a",
		"emoji":   "party 😀😀😀 time",
		"spaces":  "a   gap",
		"nul":     "before\x00after",
	}
	for id, body := range bodies {
		if err := store.UpsertMessage(&Message{MessageID: id, ConversationID: "c1", Body: body, TimestampMS: 1000}); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	for _, tc := range []struct {
		query string
		want  []string
	}{
		{"DINNER", []string{"ascii"}},          // ASCII letters match in either case
		{"nine", []string{"ascii"}},            // ...
		{"éco", []string{"accent"}},            // é matches only é
		{"ÉCO", []string{"ACCENT"}},            // and É only É
		{"cole", []string{"accent", "ACCENT"}}, // the ASCII rest matches either case
		{`"hi`, []string{"quote"}},             // a double quote is a character
		{`said "hi there"`, []string{"quote"}}, // ...even around a phrase
		{"NEAR(a b)", []string{"syntax"}},      // FTS5 syntax is text
		{"c* ^d", []string{"syntax"}},          // ...
		{"body:e", []string{"syntax"}},         // ...a column filter too
		{"50%", []string{"percent"}},           // % stays a wildcard: "%50%%"
		{"50%today", []string{"percent"}},      // ...so this matches "50% off today"
		{"e_n", []string{"under"}},             // _ matches any one character
		{"file_name", []string{"under"}},       // ...including _
		{"300 k", nil},                         // the Kelvin sign isn't k to LIKE
		{"😀😀", []string{"emoji"}},              // multi-byte characters
		{"   ", []string{"spaces"}},            // three spaces
		{"after", nil},                         // LIKE stops reading a body at NUL
		{"fore", []string{"nul"}},              // ...but reads up to it
		{"", nil},                              // the empty query lists every message; checked below
	} {
		f := normalizedSearchFilter(SearchFilter{Limit: 50})
		like, err := store.searchMessagesLike(tc.query, f)
		if err != nil {
			t.Fatalf("searchMessagesLike(%q): %v", tc.query, err)
		}
		indexed, err := store.searchMessagesIndexed(tc.query, f)
		if err != nil {
			t.Fatalf("searchMessagesIndexed(%q): %v", tc.query, err)
		}
		if !reflect.DeepEqual(indexed, like) {
			t.Errorf("query %q: index path %q, LIKE %q", tc.query, messageIDs(indexed), messageIDs(like))
		}
		if tc.query == "" {
			if len(like) != len(bodies) {
				t.Errorf("empty query: LIKE found %d messages, want all %d", len(like), len(bodies))
			}
			continue
		}
		got := messageIDs(like)
		if len(got) == 0 {
			got = nil
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("query %q: LIKE found %q, want %q", tc.query, got, tc.want)
		}
	}
}

func TestSearchIndexMatchBuildsFTS5sPhrases(t *testing.T) {
	for query, want := range map[string]string{
		"":                       "",
		"ab":                     "",
		"abc":                    `"abc"`,
		"a b":                    `"a b"`,
		`a"b`:                    `"a""b"`,
		`""`:                     "",
		`"""`:                    `""""""""`,
		"NEAR(a b)":              `"NEAR(a b)"`,
		"ab%cde_fg":              `"cde"`,
		"abc%def":                `"abc" "def"`,
		"%%abc__":                `"abc"`,
		"abc\x00def":             `"abc"`,
		"ab\x00cde":              "",
		"日本":                     "",
		"日本語":                    `"日本語"`,
		"é\u0301":                "",
		"\xc3\xc3\xc3":           "\"\xc3\xc3\xc3\"",
		"\x80\x80ab":             "",                           // a continuation byte starts no character
		"\xe6\x97\xa5\x80\x80ab": "\"\xe6\x97\xa5\x80\x80ab\"", // 日, a, b
	} {
		if got, _, _ := searchIndexMatch(query); got != want {
			t.Errorf("searchIndexMatch(%q) = %q, want %q", query, got, want)
		}
	}
	for query, want := range map[string][2]int{ // phrases, trigrams
		"": {0, 0}, "ab": {0, 0}, "abc": {1, 1}, "abcdef": {1, 4}, `"""`: {1, 1}, "日本語": {1, 1},
		"abc%def": {2, 2}, "ab%cde_fg": {1, 1}, "ab\x00cde": {0, 0}, "abcd\x00ef": {1, 2},
		strings.Repeat("abc%", 70): {70, 70},
	} {
		if _, phrases, trigrams := searchIndexMatch(query); phrases != want[0] || trigrams != want[1] {
			t.Errorf("searchIndexMatch(%q) counts %d phrases of %d trigrams, want %d of %d", query, phrases, trigrams, want[0], want[1])
		}
	}
}

func TestQualifiedMessageColumnsQualifiesEveryColumn(t *testing.T) {
	plain := strings.Split(messageColumns, ", ")
	qualified := strings.Split(qualifiedMessageColumns("m"), ", ")
	if len(qualified) != len(plain) {
		t.Fatalf("qualifiedMessageColumns gives %d columns, messageColumns has %d", len(qualified), len(plain))
	}
	for i, column := range qualified {
		if column != "m."+plain[i] {
			t.Errorf("column %d = %q, want %q", i, column, "m."+plain[i])
		}
	}
}

// routeStore holds 4,000 messages that all contain "text", 3 that contain
// "zebra", a conversation of 3,000 messages and one of 5.
func routeStore(t *testing.T) *Store {
	t.Helper()
	store := newTestStore(t)
	if !store.ftsEnabled {
		t.Skip("fts index not enabled in this sqlite build")
	}
	tx, err := store.db.Begin()
	if err != nil {
		t.Fatalf("Begin(): %v", err)
	}
	for i := 0; i < 4000; i++ {
		conv := "big"
		switch {
		case i < 5:
			conv = "tiny"
		case i >= 3005:
			conv = fmt.Sprintf("c%d", i%50)
		}
		body := fmt.Sprintf("filler text number %d", i)
		if i == 1 || i == 1000 || i == 3999 {
			body += " zebra"
		}
		if err := store.upsertMessageAndIndexTx(tx, &Message{MessageID: fmt.Sprintf("m%04d", i), ConversationID: conv, Body: body, TimestampMS: int64(i)}); err != nil {
			tx.Rollback()
			t.Fatalf("seed: %v", err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit(): %v", err)
	}
	return store
}

// The router scans a store of too few messages for the index to pay, and a store
// whose index holds a different number of entries than it has messages,
// since the index path misses a message without an entry.
func TestSearchScansSmallStoresAndAnIndexOutOfStep(t *testing.T) {
	t.Run("small store", func(t *testing.T) {
		// "zebra" holds 3 trigrams; scanning this many messages costs about
		// twice as much as merging their postings.
		n := 2 * 3 * searchIndexTrigramCost
		store := newTestStore(t)
		for i := 0; i < n; i++ {
			if err := store.UpsertMessage(&Message{MessageID: fmt.Sprintf("m%04d", i), ConversationID: "c1", Body: fmt.Sprintf("filler %d", i), TimestampMS: int64(i)}); err != nil {
				t.Fatalf("seed: %v", err)
			}
		}
		if pays, err := store.searchIndexPays("zebra", normalizedSearchFilter(SearchFilter{})); err != nil || pays {
			t.Errorf("searchIndexPays on %d messages = %v, %v; want the scan", n, pays, err)
		}
	})
	t.Run("index out of step", func(t *testing.T) {
		store := routeStore(t)
		// What a build that doesn't keep the index in step can write.
		if _, err := store.db.Exec(`INSERT INTO messages (message_id, conversation_id, body, timestamp_ms) VALUES ('unindexed', 'c1', 'a zebra the index never saw', 5000)`); err != nil {
			t.Fatalf("insert a message without an index entry: %v", err)
		}
		f := normalizedSearchFilter(SearchFilter{})
		indexed, err := store.searchMessagesIndexed("zebra", f)
		if err != nil {
			t.Fatalf("searchMessagesIndexed(): %v", err)
		}
		if want := []string{"m3999", "m1000", "m0001"}; !reflect.DeepEqual(messageIDs(indexed), want) {
			t.Fatalf("searchMessagesIndexed() = %q, want %q, without the unindexed message", messageIDs(indexed), want)
		}
		if pays, err := store.searchIndexPays("zebra", f); err != nil || pays {
			t.Errorf("searchIndexPays() = %v, %v; want the scan", pays, err)
		}
		got, err := store.SearchMessagesFiltered("zebra", SearchFilter{})
		if err != nil {
			t.Fatalf("SearchMessagesFiltered(): %v", err)
		}
		if want := []string{"unindexed", "m3999", "m1000", "m0001"}; !reflect.DeepEqual(messageIDs(got), want) {
			t.Errorf("SearchMessagesFiltered() = %q, want %q", messageIDs(got), want)
		}
	})
}

// The router takes the index for a rare word and the scan for a common one,
// a query too short to look up, and a filter that keeps fewer messages than
// the index path would spend on its merges and candidates. Every search
// returns LIKE's rows.
func TestSearchRoutesByCost(t *testing.T) {
	store := routeStore(t)
	for _, tc := range []struct {
		query string
		f     SearchFilter
		index bool
		want  int
	}{
		{"zebra", SearchFilter{}, true, 3},
		{"ZEBRA", SearchFilter{}, true, 3},
		{"zebra", SearchFilter{ConversationID: "big"}, true, 1},
		{"zebra", SearchFilter{ConversationID: "tiny"}, false, 1},
		{"zebra", SearchFilter{SinceMS: 1, UntilMS: 3}, false, 1},
		{"text", SearchFilter{}, false, 20},
		{"text", SearchFilter{ConversationID: "tiny"}, false, 5},
		{"ze", SearchFilter{}, false, 3},
		{"number 3999", SearchFilter{}, true, 1},
		{"quagga", SearchFilter{}, true, 0},
		{"no such words", SearchFilter{}, false, 0}, // 11 trigrams cost more than half a scan
	} {
		f := normalizedSearchFilter(tc.f)
		pays, err := store.searchIndexPays(tc.query, f)
		if err != nil {
			t.Fatalf("searchIndexPays(%q, %+v): %v", tc.query, tc.f, err)
		}
		if pays != tc.index {
			t.Errorf("searchIndexPays(%q, %+v) = %v, want %v", tc.query, tc.f, pays, tc.index)
		}
		got, err := store.SearchMessagesFiltered(tc.query, tc.f)
		if err != nil {
			t.Fatalf("SearchMessagesFiltered(%q, %+v): %v", tc.query, tc.f, err)
		}
		like, err := store.searchMessagesLike(tc.query, f)
		if err != nil {
			t.Fatalf("searchMessagesLike(%q, %+v): %v", tc.query, tc.f, err)
		}
		if !reflect.DeepEqual(got, like) || len(got) != tc.want {
			t.Errorf("SearchMessagesFiltered(%q, %+v) = %q, want LIKE's %d: %q", tc.query, tc.f, messageIDs(got), tc.want, messageIDs(like))
		}
	}
}

// The router scans a query of more phrases than it lets FTS5 AND, even on a
// store large enough for their merges to pay.
func TestSearchCapsThePhrasesFTS5ANDs(t *testing.T) {
	store := routeStore(t)
	store.counts = searchCounts{messages: 1_000_000_000, indexInSync: true}
	store.countsReadAt = time.Now()
	for _, tc := range []struct {
		phrases int
		index   bool
	}{
		{searchIndexMaxPhrases, true},
		{searchIndexMaxPhrases + 1, false},
	} {
		query := strings.Repeat("zebra%", tc.phrases)
		f := normalizedSearchFilter(SearchFilter{})
		if pays, err := store.searchIndexPays(query, f); err != nil || pays != tc.index {
			t.Errorf("searchIndexPays(%d phrases) = %v, %v; want %v", tc.phrases, pays, err, tc.index)
		}
		got, err := store.SearchMessagesFiltered(query, SearchFilter{})
		if err != nil {
			t.Fatalf("SearchMessagesFiltered(%d phrases): %v", tc.phrases, err)
		}
		if len(got) != 0 {
			t.Errorf("SearchMessagesFiltered(%d phrases) = %q, want none: no body holds zebra twice", tc.phrases, messageIDs(got))
		}
	}
}

// LIKE fails a pattern longer than likePatternMaxBytes only when it compares a
// row, so the index path, which finds no candidate for this one, would return
// no rows. The router sends it to the scan, and the search fails as the scan
// does. One byte shorter, both answer.
func TestSearchFailsAPatternTooLongForLikeAsTheScanDoes(t *testing.T) {
	store := routeStore(t)
	for _, tc := range []struct {
		query string
		fails bool
	}{
		{strings.Repeat("q", likePatternMaxBytes-1), true},
		{strings.Repeat("q", likePatternMaxBytes-2), false},
	} {
		_, likeErr := store.searchMessagesLike(tc.query, normalizedSearchFilter(SearchFilter{}))
		got, err := store.SearchMessagesFiltered(tc.query, SearchFilter{})
		switch {
		case (likeErr != nil) != tc.fails:
			t.Errorf("%d-byte query: LIKE error %v, want failure %v", len(tc.query), likeErr, tc.fails)
		case tc.fails && (err == nil || err.Error() != likeErr.Error()):
			t.Errorf("%d-byte query: SearchMessagesFiltered error %v, want LIKE's %v", len(tc.query), err, likeErr)
		case !tc.fails && (err != nil || len(got) != 0):
			t.Errorf("%d-byte query: SearchMessagesFiltered = %d messages, %v; want none and no error", len(tc.query), len(got), err)
		}
	}
}

// An index that fails a query is logged, and the scan answers, so the search
// returns LIKE's rows and no error.
func TestSearchLogsIndexErrorsAndScans(t *testing.T) {
	for _, tc := range []struct {
		name, breakIndex, logged string
	}{
		{"index gone", `DROP TABLE messages_fts`, "Message search index probe failed"},
		{"index without message IDs", `DROP TABLE messages_fts; CREATE VIRTUAL TABLE messages_fts USING fts5(body, tokenize='trigram'); INSERT INTO messages_fts(body) SELECT body FROM messages`, "Message search index query failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := routeStore(t)
			var logs bytes.Buffer
			store.SetLogger(zerolog.New(&logs))
			if _, err := store.db.Exec(tc.breakIndex); err != nil {
				t.Fatalf("break the index: %v", err)
			}
			got, err := store.SearchMessagesFiltered("zebra", SearchFilter{})
			if err != nil {
				t.Fatalf("SearchMessagesFiltered(): %v", err)
			}
			if want := []string{"m3999", "m1000", "m0001"}; !reflect.DeepEqual(messageIDs(got), want) {
				t.Errorf("SearchMessagesFiltered() = %q, want %q", messageIDs(got), want)
			}
			if !strings.Contains(logs.String(), tc.logged) || !strings.Contains(logs.String(), `"level":"error"`) {
				t.Errorf("log = %q, want an error %q", logs.String(), tc.logged)
			}
		})
	}
}

// Intended difference: a message stored without a message_id (SQLite lets a
// TEXT PRIMARY KEY hold NULL) has no ID to join its index entry by, so the
// index path never returns it. The scan returns it and then fails to read its
// NULL ID, so on such a store the index path answers where the scan errors.
func TestSearchIndexPathSkipsAMessageWithoutAnID(t *testing.T) {
	store := routeStore(t)
	if _, err := store.db.Exec(`INSERT INTO messages (message_id, conversation_id, body, timestamp_ms) VALUES (NULL, 'c1', 'a zebra without an id', 5000)`); err != nil {
		t.Fatalf("insert a message without an ID: %v", err)
	}
	if _, err := store.db.Exec(`INSERT INTO messages_fts (message_id, body) VALUES (NULL, 'a zebra without an id')`); err != nil {
		t.Fatalf("index it: %v", err)
	}
	f := normalizedSearchFilter(SearchFilter{})
	if _, err := store.searchMessagesLike("zebra", f); err == nil {
		t.Errorf("searchMessagesLike(zebra) read a NULL message_id without an error")
	}
	got, err := store.searchMessagesIndexed("zebra", f)
	if err != nil {
		t.Fatalf("searchMessagesIndexed(zebra): %v", err)
	}
	if want := []string{"m3999", "m1000", "m0001"}; !reflect.DeepEqual(messageIDs(got), want) {
		t.Errorf("searchMessagesIndexed(zebra) = %q, want %q", messageIDs(got), want)
	}
}

// BenchmarkSearchPaths times each search path alone, and the probe that picks
// between them, on BenchmarkSearchLargeStore's searches. The scan for a word
// in no message reads all 80,000 messages, and the index path for a word in
// 4,000 reads 4,000 candidates, so the two give searchIndexCandidateCost.
func BenchmarkSearchPaths(b *testing.B) {
	runSearchBenchmarks(b, func(b *testing.B, store *Store, query string, f SearchFilter) {
		for _, path := range []struct {
			name string
			run  func() error
		}{
			{"scan", func() error { _, err := store.searchMessagesLike(query, f); return err }},
			{"index", func() error { _, err := store.searchMessagesIndexed(query, f); return err }},
			{"probe", func() error { _, err := store.searchIndexPays(query, f); return err }},
		} {
			b.Run(path.name, func(b *testing.B) {
				for i := 0; i < b.N; i++ {
					if err := path.run(); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	})
}
