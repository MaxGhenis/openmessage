package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/quick"
)

// Invariant: for every LIKE pattern FTS5 answers from a trigram index,
// trigramMatchQuery's MATCH selects exactly the rows FTS5 reads as candidates
// for LIKE '%' || pattern || '%', and it returns an expression exactly when
// likePatternUsesTrigrams says FTS5 uses the index. The probe makes FTS5's
// candidates observable: the index holds the postings of random bodies, and
// then every row of the content table is rewritten to a body the pattern
// matches, so the LIKE that SQLite re-applies keeps every candidate and drops
// nothing else.
func TestTrigramMatchQuerySelectsFTS5LikeCandidates(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "probe.sqlite3"))
	if err != nil {
		t.Fatalf("sql.Open(): %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	for _, statement := range []string{
		`CREATE TABLE content (body TEXT NOT NULL) STRICT`,
		`CREATE VIRTUAL TABLE probe USING fts5(body, content = 'content', content_rowid = 'rowid',
			tokenize = 'trigram case_sensitive 0', detail = none, columnsize = 0)`,
	} {
		mustExec(t, db, statement)
	}
	r := rand.New(rand.NewSource(23))
	// Quotes and long runs exercise the expression's quoting and size.
	pieces := append(slices.Clone(searchTextPieces), `"`, `""`, `"a"`, "quayside", "QUAY", "the lunch")
	randomText := func(n int) string {
		var b strings.Builder
		for range n {
			b.WriteString(pieces[r.Intn(len(pieces))])
		}
		return b.String()
	}
	var bodies []string
	for i := range 400 {
		body := randomText(r.Intn(14))
		if i%50 == 0 {
			body = randomText(80 + r.Intn(80))
		}
		bodies = append(bodies, body)
		mustExec(t, db, `INSERT INTO content (rowid, body) VALUES (?, ?)`, i+1, body)
	}
	mustExec(t, db, `INSERT INTO probe (probe) VALUES ('rebuild')`)

	rowids := func(query string, args ...any) []int64 {
		t.Helper()
		rows, err := db.Query(query, args...)
		if err != nil {
			t.Fatalf("%s %q: %v", query, args, err)
		}
		defer rows.Close()
		var ids []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			ids = append(ids, id)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return ids
	}
	patterns := 3000
	if raceDetectorEnabled {
		patterns = 600
	}
	compared := 0
	for i := range patterns {
		var pattern string
		switch {
		case i%1000 == 0:
			// Hundreds of trigrams in one expression: no NUL to end it early.
			pattern = strings.ReplaceAll(randomText(300+r.Intn(200)), "\x00", "")
		case i%10 == 0:
			pattern = randomText(20 + r.Intn(60))
		case i%3 == 0:
			pattern = randomText(1 + r.Intn(5))
		default:
			pattern = randomSearchQuery(r, bodies)
		}
		expression, ok := trigramMatchQuery(pattern)
		if terms := strings.Count(expression, `" "`) + 1; i%1000 == 0 && terms < 150 {
			t.Fatalf("long pattern %d made only %d trigram terms", i, terms)
		}
		if ok != likePatternUsesTrigrams(pattern) {
			t.Fatalf("trigramMatchQuery(%q) ok = %v, likePatternUsesTrigrams = %v", pattern, ok, !ok)
		}
		if !ok {
			if expression != "" {
				t.Fatalf("trigramMatchQuery(%q) = %q without ok", pattern, expression)
			}
			continue
		}
		// A body the pattern matches: wildcards become ASCII letters, so no
		// two bytes join into a different UTF-8 sequence, and the pattern ends
		// at its first NUL as LIKE reads it.
		body := strings.NewReplacer("%", "z", "_", "x").Replace(pattern)
		if at := strings.IndexByte(body, 0); at >= 0 {
			body = body[:at]
		}
		var like int
		if err := db.QueryRow(`SELECT ? LIKE '%' || ? || '%'`, body, pattern).Scan(&like); err != nil || like != 1 {
			t.Fatalf("probe body %q LIKE %q = %d, %v; want 1", body, pattern, like, err)
		}
		mustExec(t, db, `UPDATE content SET body = ?`, body)
		want := rowids(`SELECT rowid FROM probe WHERE body LIKE '%' || ? || '%' ORDER BY rowid`, pattern)
		got := rowids(`SELECT rowid FROM probe WHERE probe MATCH ? ORDER BY rowid`, expression)
		if !slices.Equal(got, want) {
			t.Fatalf("pattern %q: MATCH %q selects %v, FTS5 reads %v for the LIKE", pattern, expression, got, want)
		}
		compared++
	}
	if compared < patterns/4 {
		t.Fatalf("compared only %d of %d patterns; the draw must mostly reach the index", compared, patterns)
	}
}

func TestTrigramMatchQueryQuotesEachTrigram(t *testing.T) {
	for _, tc := range []struct {
		pattern, want string
		ok            bool
	}{
		{"", "", false},
		{"ab", "", false},
		{"a%bc", "", false},
		{"ab\x00cd", "", false},
		{"abc", `"abc"`, true},
		{"abcd", `"abc" "bcd"`, true},
		{"ab%cde_fgh", `"cde" "fgh"`, true},
		{"abc\x00def", `"abc"`, true},
		{`a"b`, `"a""b"`, true},
		{`"""`, `""""""""`, true},
		{"日本語の", `"日本語" "本語の"`, true},
		// A lead byte takes the continuation bytes after it; any other byte
		// stands alone, as the tokenizer and LIKE read them.
		{"\xc3\xa9t\xc3", "\"\xc3\xa9t\xc3\"", true},
		{"a\x80bc", "\"a\x80b\" \"\x80bc\"", true},
		{"\x80\x80\x80", "", false},
	} {
		got, ok := trigramMatchQuery(tc.pattern)
		if got != tc.want || ok != tc.ok {
			t.Errorf("trigramMatchQuery(%q) = %q, %v; want %q, %v", tc.pattern, got, ok, tc.want, tc.ok)
		}
	}
}

// Invariant: cutting a conversation's range at any of its rows splits the
// LIKE's walk. The first limit matches at or above the cut, followed by the
// first limit-k of those below it (k the matches above), are the LIKE
// statement's rows, for every filter; and a cut past the range's last row is
// reported as missing.
func TestConversationSplitMatchesLikeProperty(t *testing.T) {
	ctx := context.Background()
	property := func(c readPlanStore) bool {
		s := seedReadStore(t, c.Seed)
		r := rand.New(rand.NewSource(c.Seed ^ 0x5b1d))
		var bodies []string
		for _, message := range s.messages {
			body := randomSearchText(r, r.Intn(8))
			bodies = append(bodies, body)
			mustExec(t, s.store.db, `UPDATE messages SET body = ? WHERE message_id = ?`, body, message.MessageID)
		}
		for range 20 {
			filter := SearchQuery{
				ConversationID: s.conversations[r.Intn(len(s.conversations))].ConversationID,
				Limit:          1 + r.Intn(8),
			}
			if r.Intn(4) == 0 {
				filter.AccountID = s.accounts[r.Intn(len(s.accounts))]
			}
			if r.Intn(4) == 0 {
				filter.SenderCanonicalValue = s.phones[r.Intn(len(s.phones))]
			}
			if r.Intn(3) == 0 {
				filter.SinceMS = int64(1 + r.Intn(4))
			}
			if r.Intn(3) == 0 {
				filter.UntilMS = int64(4 + r.Intn(5))
			}
			query := randomSearchQuery(r, bodies)
			likeSQL, likeArgs := likeSearchMessagesStatement(query, filter)
			want := queryMessages(t, s.store.db, likeSQL, likeArgs...)
			rangeConditions, rangeArgs := conversationRange(filter)
			var size int
			if err := s.store.db.QueryRow(`SELECT count(*) FROM messages AS m WHERE `+strings.Join(rangeConditions, " AND "), rangeArgs...).Scan(&size); err != nil {
				t.Fatal(err)
			}
			for window := 1; window <= size+1; window++ {
				boundary, found, err := conversationBoundary(ctx, s.store.db, filter, window)
				if err != nil {
					t.Fatalf("conversationBoundary(): %v", err)
				}
				if found != (window <= size) {
					t.Fatalf("seed %d: boundary of window %d in a range of %d found = %v", c.Seed, window, size, found)
				}
				if !found {
					continue
				}
				statement, args := conversationSplitSearchStatement(query, filter, boundary, true, filter.Limit)
				got := queryMessages(t, s.store.db, statement, args...)
				if len(got) < filter.Limit {
					statement, args = conversationSplitSearchStatement(query, filter, boundary, false, filter.Limit-len(got))
					got = append(got, queryMessages(t, s.store.db, statement, args...)...)
				}
				if !reflect.DeepEqual(got, want) {
					t.Errorf("seed %d: %q %+v cut at row %d (%+v) = %v, LIKE = %v", c.Seed, query, filter, window, boundary, messageIDs(got), messageIDs(want))
					return false
				}
				// The rows below the cut, counted from the index alone.
				countSQL, countArgs := conversationRestCountStatement(filter, boundary)
				var below int
				if err := s.store.db.QueryRow(countSQL, append(countArgs, size+1)...).Scan(&below); err != nil {
					t.Fatal(err)
				}
				if below != size-window {
					t.Errorf("seed %d: %+v rows below row %d of %d = %d, want %d", c.Seed, filter, window, size, below, size-window)
					return false
				}
			}
		}
		return true
	}
	if err := quick.Check(property, substringSearchQuickConfig()); err != nil {
		t.Fatal(err)
	}
}

// Invariant: the sender count is the number of rows the sender's LIKE reads:
// the sender's messages in the date range, whatever else the filter names.
func TestSenderRangeCountCountsTheSendersMessages(t *testing.T) {
	s := seedReadStore(t, 31)
	for _, phone := range append(slices.Clone(s.phones), "uuid-1", "+19999999999") {
		for _, dates := range [][2]int64{{0, 0}, {2, 0}, {0, 5}, {2, 5}} {
			filter := SearchQuery{SenderCanonicalValue: phone, SinceMS: dates[0], UntilMS: dates[1]}
			ids, err := senderIdentityIDs(context.Background(), s.store.db, phone)
			if err != nil {
				t.Fatal(err)
			}
			var got int
			// An address no identity holds is answered without a count.
			if len(ids) > 0 {
				statement, args := senderRangeCountStatement(filter, ids)
				if err := s.store.db.QueryRow(statement, append(args, 1000)...).Scan(&got); err != nil {
					t.Fatal(err)
				}
			}
			want := 0
			for _, message := range s.messages {
				if message.SenderIdentityID == nil || (dates[0] > 0 && message.OccurredAtMS < dates[0]) || (dates[1] > 0 && message.OccurredAtMS > dates[1]) {
					continue
				}
				for _, identity := range s.identities {
					if identity.IdentityID == *message.SenderIdentityID && identity.CanonicalValue == phone {
						want++
					}
				}
			}
			if got != want {
				t.Errorf("sender %s dates %v: count = %d, want %d", phone, dates, got, want)
			}
		}
	}
}

func TestExpectedLikeRows(t *testing.T) {
	for _, tc := range []struct{ limit, matched, window, want int }{
		{30, 0, 2000, 30 * 2001},
		{50, 0, 2000, 50 * 2001},
		{30, 1, 2000, (29*2001 + 1) / 2}, // ceil(29*2001/2)
		{30, 29, 2000, 67},               // ceil(1*2001/30)
		{5, 0, 20, 105},
		{1, 0, 1, 2},
	} {
		if got := expectedLikeRows(tc.limit, tc.matched, tc.window); got != tc.want {
			t.Errorf("expectedLikeRows(%d, %d, %d) = %d, want %d", tc.limit, tc.matched, tc.window, got, tc.want)
		}
	}
	// It never grows as the window finds more matches, and never drops below
	// the matches still missing times the window's rows per match.
	property := func(limit, matched, window uint16) bool {
		l, w := 1+int(limit%600), 1+int(window%5000)
		m := int(matched) % l
		got := expectedLikeRows(l, m, w)
		if m+1 < l && expectedLikeRows(l, m+1, w) > got {
			return false
		}
		return int64(got)*int64(m+1) >= int64(l-m)*int64(w+1)
	}
	if err := quick.Check(property, nil); err != nil {
		t.Fatal(err)
	}
}

// TestScopeIndexBoundReadsWithinItsBounds checks that deciding reads no more
// than it must: nothing when the LIKE expects to read fewer than the minimum
// rows, and rows only up to the minimum; and that the bound is half of it.
func TestScopeIndexBoundReadsWithinItsBounds(t *testing.T) {
	ctx := context.Background()
	store := openRepositoryTestStore(t)
	seedReadStoreInto(t, store, 7)
	plan := searchPlan{indexMinRows: 10}
	var counted []int
	countRows := func(rows int) func(int) (int, error) {
		return func(upTo int) (int, error) {
			counted = append(counted, upTo)
			return min(rows, upTo), nil
		}
	}
	if bound, err := scopeIndexBound(ctx, store.db, 9, 1, plan, countRows(1000)); err != nil || bound != 0 || len(counted) != 0 {
		t.Fatalf("expected 9 < 10 rows: bound %d, err %v, counted %v; want 0 and no count", bound, err, counted)
	}
	if bound, err := scopeIndexBound(ctx, store.db, -1, 1, plan, countRows(9)); err != nil || bound != 0 || !slices.Equal(counted, []int{10}) {
		t.Fatalf("9 rows left: bound %d, err %v, counted %v; want 0 after one count up to 10", bound, err, counted)
	}
	if bound, err := scopeIndexBound(ctx, store.db, 10, 1, plan, countRows(1000)); err != nil || bound != 5 {
		t.Fatalf("10 expected and 10 left: bound %d, err %v; want 5", bound, err)
	}
	// With two minimums asked for, 19 rows are too few and 20 enough, and the
	// bound stays half of one minimum.
	counted = nil
	if bound, err := scopeIndexBound(ctx, store.db, -1, 2, plan, countRows(19)); err != nil || bound != 0 || !slices.Equal(counted, []int{20}) {
		t.Fatalf("19 rows against two minimums: bound %d, err %v, counted %v; want 0 after one count up to 20", bound, err, counted)
	}
	if bound, err := scopeIndexBound(ctx, store.db, -1, 2, plan, countRows(20)); err != nil || bound != 5 {
		t.Fatalf("20 rows against two minimums: bound %d, err %v; want 5", bound, err)
	}
	if bound, err := scopeIndexBound(ctx, store.db, -1, 1, searchPlan{indexMinRows: 1}, countRows(1000)); err != nil || bound != 0 {
		t.Fatalf("a minimum of one row: bound %d, err %v; want 0 (no candidate fits)", bound, err)
	}
	var messages int64
	if err := store.db.QueryRow(`SELECT max(rowid) FROM messages`).Scan(&messages); err != nil {
		t.Fatal(err)
	}
	if bound, err := scopeIndexBound(ctx, store.db, -1, 1, searchPlan{indexMinRows: 2, indexStoreShare: 1}, countRows(1<<30)); err != nil || bound != max(1, int(messages)/2) {
		t.Fatalf("a minimum of the store's size %d: bound %d, err %v", messages, bound, err)
	}
	failing := func(int) (int, error) { return 0, errors.New("count failed") }
	if _, err := scopeIndexBound(ctx, store.db, -1, 1, plan, failing); err == nil || !strings.Contains(err.Error(), "count failed") {
		t.Fatalf("a failing count = %v; want it reported", err)
	}
	if bound, err := scopeIndexBound(ctx, openRepositoryTestStore(t).db, -1, 1, searchPlan{indexMinRows: 4, indexStoreShare: 50}, countRows(1000)); err != nil || bound != 2 {
		t.Fatalf("an empty store: bound %d, err %v; want the minimum's 2", bound, err)
	}
}

// Invariant: the index search gives up exactly when the index holds at least
// bound candidates, and otherwise returns the LIKE's rows, including none.
func TestScopeIndexSearchGivesUpAtItsBound(t *testing.T) {
	ctx := context.Background()
	store := openRepositoryTestStore(t)
	s := seedReadStoreInto(t, store, 41)
	mustExec(t, store.db, `UPDATE messages SET body = CASE WHEN rowid % 3 = 0 THEN 'lunch at the quay' ELSE 'quayside walk' END`)
	for _, query := range []string{"quay", "lunch", "absent-term", "%quay%"} {
		expression, _ := trigramMatchQuery(query)
		var candidates int
		if err := store.db.QueryRow(`SELECT count(*) FROM messages_fts WHERE messages_fts MATCH ?`, expression).Scan(&candidates); err != nil {
			t.Fatal(err)
		}
		for _, conversation := range s.conversations {
			filter := SearchQuery{ConversationID: conversation.ConversationID, Limit: 3}
			likeSQL, likeArgs := likeSearchMessagesStatement(query, filter)
			want := queryMessages(t, store.db, likeSQL, likeArgs...)
			for _, bound := range []int{max(1, candidates-1), max(1, candidates), candidates + 1} {
				got, complete, err := scopeIndexSearch(ctx, store.db, expression, query, filter, nil, bound)
				if err != nil {
					t.Fatal(err)
				}
				if complete != (candidates < bound) {
					t.Fatalf("%q with %d candidates and bound %d: complete = %v", query, candidates, bound, complete)
				}
				if complete && !reflect.DeepEqual(got, want) {
					t.Fatalf("%q in %s: %v, LIKE = %v", query, conversation.ConversationID, messageIDs(got), messageIDs(want))
				}
				if !complete && got != nil {
					t.Fatalf("%q gave up but returned %v", query, messageIDs(got))
				}
				// Giving up happens in the statement, before any lookup: it
				// returns only the count, never a message from a truncated list.
				statement, args := scopeIndexSearchStatement(expression, query, filter, nil, bound)
				answer, err := store.db.Query(statement, args...)
				if err != nil {
					t.Fatal(err)
				}
				var rows, found int
				for answer.Next() {
					columns := make([]any, 14)
					for i := range columns {
						columns[i] = new(any)
					}
					if err := answer.Scan(columns...); err != nil {
						t.Fatal(err)
					}
					rows++
					if *(columns[1].(*any)) != int64(0) {
						found++
					}
				}
				if err := answer.Close(); err != nil {
					t.Fatal(err)
				}
				if !complete && (rows != 1 || found != 0) {
					t.Fatalf("%q with %d candidates and bound %d: the statement returned %d rows, %d messages; want the count alone", query, candidates, bound, rows, found)
				}
			}
		}
	}
}

// TestScopedSearchPlansReadTheirIndexes pins the plans of the statements a
// conversation or sender search runs, on a store without statistics as in
// production: the boundary and the counts read only the scope's index; the two
// parts of a conversation walk messages_conversation_time_idx from the
// boundary, never past it, with no sort; and the index search reads the
// trigram postings first and joins each candidate by rowid.
func TestScopedSearchPlansReadTheirIndexes(t *testing.T) {
	ctx := context.Background()
	store := openRepositoryTestStore(t)
	db := store.db
	boundary := conversationKey{occurredAtMS: 5, messageID: "message-05"}
	sender := []string{"LIST SUBQUERY 1", "SCAN identities", "CREATE BLOOM FILTER"}
	for _, tc := range []struct {
		name                   string
		filter                 SearchQuery
		boundary, above, below []string
		restCount              []string
		extra                  []string
	}{
		{
			name:      "conversation",
			filter:    SearchQuery{ConversationID: "conversation-a", Limit: 30},
			boundary:  []string{"SEARCH m USING COVERING INDEX messages_conversation_time_idx (conversation_id=?)"},
			above:     []string{"SEARCH m USING INDEX messages_conversation_time_idx (conversation_id=? AND (occurred_at_ms,message_id)>(?,?))"},
			below:     []string{"SEARCH m USING INDEX messages_conversation_time_idx (conversation_id=? AND (occurred_at_ms,message_id)<(?,?))"},
			restCount: []string{"CO-ROUTINE (subquery-1)", "SEARCH m USING COVERING INDEX messages_conversation_time_idx (conversation_id=? AND (occurred_at_ms,message_id)<(?,?))", "SCAN (subquery-1)"},
		},
		{
			// Each part is bounded by the boundary and the date it does not imply.
			name:      "conversation in a date range",
			filter:    SearchQuery{ConversationID: "conversation-a", SinceMS: 1, UntilMS: 9, Limit: 30},
			boundary:  []string{"SEARCH m USING COVERING INDEX messages_conversation_time_idx (conversation_id=? AND occurred_at_ms>? AND occurred_at_ms<?)"},
			above:     []string{"SEARCH m USING INDEX messages_conversation_time_idx (conversation_id=? AND (occurred_at_ms,message_id)>(?,?) AND occurred_at_ms<?)"},
			below:     []string{"SEARCH m USING INDEX messages_conversation_time_idx (conversation_id=? AND occurred_at_ms>? AND (occurred_at_ms,message_id)<(?,?))"},
			restCount: []string{"CO-ROUTINE (subquery-1)", "SEARCH m USING COVERING INDEX messages_conversation_time_idx (conversation_id=? AND occurred_at_ms>? AND (occurred_at_ms,message_id)<(?,?))", "SCAN (subquery-1)"},
		},
		{
			// A sender filter applies to the conversation's rows.
			name:      "conversation and sender",
			filter:    SearchQuery{ConversationID: "conversation-a", SenderCanonicalValue: "+15550000001", Limit: 30},
			boundary:  []string{"SEARCH m USING COVERING INDEX messages_conversation_time_idx (conversation_id=?)"},
			above:     append([]string{"SEARCH m USING INDEX messages_conversation_time_idx (conversation_id=? AND (occurred_at_ms,message_id)>(?,?))"}, sender...),
			below:     append([]string{"SEARCH m USING INDEX messages_conversation_time_idx (conversation_id=? AND (occurred_at_ms,message_id)<(?,?))"}, sender...),
			restCount: []string{"CO-ROUTINE (subquery-1)", "SEARCH m USING COVERING INDEX messages_conversation_time_idx (conversation_id=? AND (occurred_at_ms,message_id)<(?,?))", "SCAN (subquery-1)"},
			extra:     sender,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conditions, args := conversationRange(tc.filter)
			query := `SELECT m.occurred_at_ms, m.message_id FROM messages AS m INDEXED BY messages_conversation_time_idx WHERE ` +
				strings.Join(conditions, " AND ") + ` ORDER BY m.occurred_at_ms DESC, m.message_id DESC LIMIT 1 OFFSET ?`
			if plan := explainPlanLines(t, db, query, append(args, 1)...); !slices.Equal(plan, tc.boundary) {
				t.Fatalf("boundary plan = %q, want %q", plan, tc.boundary)
			}
			// The pinned statement is the one conversationBoundary runs.
			if _, _, err := conversationBoundary(ctx, store.db, tc.filter, 1); err != nil {
				t.Fatalf("conversationBoundary(): %v", err)
			}
			statement, args := conversationSplitSearchStatement("term", tc.filter, boundary, true, 30)
			if plan := explainPlanLines(t, db, statement, args...); !slices.Equal(plan, tc.above) {
				t.Fatalf("upper part plan = %q, want %q", plan, tc.above)
			}
			statement, args = conversationSplitSearchStatement("term", tc.filter, boundary, false, 30)
			if plan := explainPlanLines(t, db, statement, args...); !slices.Equal(plan, tc.below) {
				t.Fatalf("lower part plan = %q, want %q", plan, tc.below)
			}
			statement, args = conversationRestCountStatement(tc.filter, boundary)
			if plan := explainPlanLines(t, db, statement, append(args, 10)...); !slices.Equal(plan, tc.restCount) {
				t.Fatalf("rest count plan = %q, want %q", plan, tc.restCount)
			}
			assertScopeIndexSearchPlan(t, db, tc.filter, nil, renumberSubquery(tc.extra, 4))
		})
	}
	// A sender's statements name its identities by ID: none reads identities,
	// the count reads only messages_sender_time_idx, and the LIKE seeks it once
	// per identity.
	ids := []string{"identity-a", "identity-b"}
	for _, tc := range []struct {
		name        string
		filter      SearchQuery
		count, like []string
	}{
		{"sender", SearchQuery{SenderCanonicalValue: "+15550000001", Limit: 30},
			[]string{"CO-ROUTINE (subquery-1)", "SEARCH m USING COVERING INDEX messages_sender_time_idx (sender_identity_id=?)", "SCAN (subquery-1)"},
			[]string{"SEARCH m USING INDEX messages_sender_time_idx (sender_identity_id=?)", "USE TEMP B-TREE FOR ORDER BY"}},
		{"sender in a date range", SearchQuery{SenderCanonicalValue: "+15550000001", SinceMS: 1, UntilMS: 9, Limit: 30},
			[]string{"CO-ROUTINE (subquery-1)", "SEARCH m USING COVERING INDEX messages_sender_time_idx (sender_identity_id=? AND occurred_at_ms>? AND occurred_at_ms<?)", "SCAN (subquery-1)"},
			[]string{"SEARCH m USING INDEX messages_sender_time_idx (sender_identity_id=? AND occurred_at_ms>? AND occurred_at_ms<?)", "USE TEMP B-TREE FOR ORDER BY"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			statement, args := senderRangeCountStatement(tc.filter, ids)
			if plan := explainPlanLines(t, db, statement, append(args, 10)...); !slices.Equal(plan, tc.count) {
				t.Fatalf("sender count plan = %q, want %q", plan, tc.count)
			}
			statement, args = senderLikeSearchStatement("term", tc.filter, ids)
			if plan := explainPlanLines(t, db, statement, args...); !slices.Equal(plan, tc.like) {
				t.Fatalf("sender LIKE plan = %q, want %q", plan, tc.like)
			}
			assertScopeIndexSearchPlan(t, db, tc.filter, ids, nil)
		})
	}
	// One identity's messages come out of the index in time order, so only
	// ties are sorted and the LIKE stops at limit matches.
	t.Run("sender with one identity", func(t *testing.T) {
		statement, args := senderLikeSearchStatement("term", SearchQuery{SenderCanonicalValue: "+15550000001", Limit: 30}, ids[:1])
		want := []string{"SEARCH m USING INDEX messages_sender_time_idx (sender_identity_id=?)", "USE TEMP B-TREE FOR LAST TERM OF ORDER BY"}
		if plan := explainPlanLines(t, db, statement, args...); !slices.Equal(plan, want) {
			t.Fatalf("one-identity sender LIKE plan = %q, want %q", plan, want)
		}
	})
}

// renumberSubquery renames the sender's identity list to the number SQLite
// gives it inside the index search, after its own subqueries.
func renumberSubquery(lines []string, n int) []string {
	out := slices.Clone(lines)
	for i, line := range out {
		if strings.HasPrefix(line, "LIST SUBQUERY ") {
			out[i] = fmt.Sprintf("LIST SUBQUERY %d", n)
		}
	}
	return out
}

// assertScopeIndexSearchPlan pins the index search's plan: the candidates
// come from messages_fts alone and are materialized and counted once; each is
// joined to its message by rowid, with filter's extra lines (the identity
// list of a sender selected by subquery); and only the found rows are sorted.
// No plan line reads messages whole.
func assertScopeIndexSearchPlan(t *testing.T, db *sql.DB, filter SearchQuery, senderIDs []string, extra []string) {
	t.Helper()
	statement, args := scopeIndexSearchStatement(`"ter" "erm"`, "term", filter, senderIDs, 10)
	want := append(append([]string{
		"MATERIALIZE candidate_count",
		"MATERIALIZE candidates",
		"SCAN messages_fts VIRTUAL TABLE INDEX 0:M1",
		"SCAN candidates",
		"MATERIALIZE found",
		"SCAN c",
		"SCALAR SUBQUERY 3",
		"SCAN candidate_count",
		"SEARCH m USING INTEGER PRIMARY KEY (rowid=?)",
	}, extra...), "USE TEMP B-TREE FOR ORDER BY", "SCAN candidate_count", "SCAN found LEFT-JOIN", "USE TEMP B-TREE FOR ORDER BY")
	if plan := explainPlanLines(t, db, statement, args...); !slices.Equal(plan, want) {
		t.Fatalf("index search plan = %q, want %q", plan, want)
	}
}

// A conversation's window and rest are separate statements. searchScope
// reads them in one read transaction, so the answer is the LIKE's at one
// snapshot even when a write moves a message across the cut between them;
// without the transaction, the moved message comes back twice.
func TestConversationSearchReadsOneSnapshot(t *testing.T) {
	ctx := context.Background()
	store := openRepositoryTestStore(t)
	repository := mustMessageRepository(t, store, 100)
	db := store.db
	mustExec(t, db, `INSERT INTO accounts (account_id, bridge_key, created_at_ms, updated_at_ms) VALUES ('account-a', 'signal_cli', 1, 1)`)
	mustExec(t, db, `INSERT INTO conversations (conversation_id, account_id, remote_conversation_id, kind, created_at_ms, updated_at_ms) VALUES ('conversation-a', 'account-a', 'remote-a', 'direct', 1, 1)`)
	for i := 1; i <= 10; i++ {
		mustExec(t, db, `INSERT INTO messages (message_id, conversation_id, account_id, remote_message_id, direction, body, occurred_at_ms, created_at_ms, updated_at_ms)
			VALUES (?, 'conversation-a', 'account-a', ?, 'incoming', 'lunch at the quay', ?, 1, 1)`, fmt.Sprintf("message-%02d", i), fmt.Sprintf("remote-%d", i), 100+i)
	}
	filter := SearchQuery{ConversationID: "conversation-a", Limit: 20}
	likeSQL, likeArgs := likeSearchMessagesStatement("quay", filter)
	before := queryMessages(t, db, likeSQL, likeArgs...)
	// A newest-3 window, and the index never tried: the rest answers. Between
	// the window and the rest, the newest message moves below the cut.
	plan := searchPlan{window: 3, indexMinRows: 1000}
	repository.betweenSearchStatements = func() {
		mustExec(t, db, `UPDATE messages SET occurred_at_ms = 50 WHERE message_id = 'message-10'`)
	}
	got, path, err := repository.searchMessages(ctx, "quay", filter, plan)
	if err != nil || path != searchPathConversationRest {
		t.Fatalf("searchMessages() path %s, err %v", path, err)
	}
	if !reflect.DeepEqual(got, before) {
		t.Fatalf("searchMessages() = %v, want the LIKE's rows before the write %v", messageIDs(got), messageIDs(before))
	}

	// The same statements outside a transaction see two different stores.
	mustExec(t, db, `UPDATE messages SET occurred_at_ms = 110 WHERE message_id = 'message-10'`)
	expression, _ := trigramMatchQuery("quay")
	got, _, err = repository.searchConversation(ctx, db, "quay", expression, filter, plan)
	if err != nil {
		t.Fatal(err)
	}
	if ids := messageIDs(got); len(ids) != 11 || ids[0] != "message-10" || ids[10] != "message-10" {
		t.Fatalf("without a transaction = %v; want message-10 first and again last, which is why searchScope opens one", ids)
	}
}

// A sender's identities and their messages are read by separate statements
// too. In searchScope's transaction the answer is the LIKE's at one snapshot
// even when a message moves to another identity of the same address between
// them; without it, that message is in neither the identities read before nor
// the messages read after.
func TestSenderSearchReadsOneSnapshot(t *testing.T) {
	ctx := context.Background()
	store := openRepositoryTestStore(t)
	repository := mustMessageRepository(t, store, 100)
	db := store.db
	mustExec(t, db, `INSERT INTO accounts (account_id, bridge_key, created_at_ms, updated_at_ms) VALUES ('account-a', 'signal_cli', 1, 1)`)
	mustExec(t, db, `INSERT INTO identities (identity_id, account_id, kind, canonical_value, raw_value, created_at_ms, updated_at_ms) VALUES ('identity-a', 'account-a', 'phone', '+15550000001', '+15550000001', 1, 1)`)
	mustExec(t, db, `INSERT INTO conversations (conversation_id, account_id, remote_conversation_id, kind, created_at_ms, updated_at_ms) VALUES ('conversation-a', 'account-a', 'remote-a', 'direct', 1, 1)`)
	for i := 1; i <= 6; i++ {
		mustExec(t, db, `INSERT INTO messages (message_id, conversation_id, account_id, remote_message_id, sender_identity_id, direction, body, occurred_at_ms, created_at_ms, updated_at_ms)
			VALUES (?, 'conversation-a', 'account-a', ?, 'identity-a', 'incoming', 'lunch at the quay', ?, 1, 1)`, fmt.Sprintf("message-%02d", i), fmt.Sprintf("remote-%d", i), 100+i)
	}
	filter := SearchQuery{SenderCanonicalValue: "+15550000001", Limit: 20}
	likeSQL, likeArgs := likeSearchMessagesStatement("quay", filter)
	before := queryMessages(t, db, likeSQL, likeArgs...)
	// After the identities are read, the address gains a second identity and
	// the newest message becomes its.
	reattribute := func() {
		mustExec(t, db, `INSERT INTO identities (identity_id, account_id, kind, canonical_value, raw_value, created_at_ms, updated_at_ms) VALUES ('identity-b', 'account-a', 'e164', '+15550000001', '+15550000001', 1, 1)`)
		mustExec(t, db, `UPDATE messages SET sender_identity_id = 'identity-b' WHERE message_id = 'message-06'`)
	}
	repository.betweenSearchStatements = reattribute
	got, path, err := repository.searchMessages(ctx, "quay", filter, searchPlan{indexMinRows: 1000})
	if err != nil || path != searchPathLike {
		t.Fatalf("searchMessages() path %s, err %v", path, err)
	}
	if !reflect.DeepEqual(got, before) {
		t.Fatalf("searchMessages() = %v, want the LIKE's rows before the write %v", messageIDs(got), messageIDs(before))
	}

	// The same statements outside a transaction lose the moved message, which
	// the LIKE finds both before and after the write.
	mustExec(t, db, `UPDATE messages SET sender_identity_id = 'identity-a' WHERE message_id = 'message-06'`)
	mustExec(t, db, `DELETE FROM identities WHERE identity_id = 'identity-b'`)
	expression, _ := trigramMatchQuery("quay")
	got, _, err = repository.searchSender(ctx, db, "quay", expression, filter, searchPlan{indexMinRows: 1000})
	if err != nil {
		t.Fatal(err)
	}
	after := queryMessages(t, db, likeSQL, likeArgs...)
	if len(before) != 6 || len(after) != 6 || len(got) != 5 {
		t.Fatalf("without a transaction = %v (LIKE before %d rows, after %d); want the moved message missing, which is why searchScope opens one",
			messageIDs(got), len(before), len(after))
	}
}
