package sqlite

import (
	"context"
	"fmt"
	"math/rand"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/quick"
	"unicode"
)

// The trigram statements change how SQLite finds candidate rows, never which
// rows match: the LIKE still decides. The properties below hold for every
// input, not only where the index helps, so they draw text that probes where
// the trigram tokenizer and LIKE differ (non-ASCII case folding, the Kelvin
// sign, combining marks, emoji sequences, wildcards, backslashes, NULs and
// invalid UTF-8) and compare each search with the plain LIKE statement.

// searchTextPieces are the fragments random text is built from.
var searchTextPieces = []string{
	"a", "b", "c", "e", "o", "A", "B", "E", "the", "The", "THE", "lunch", "LUNCH",
	" ", " ", "  ", ".", "?", "'", `"`, "%", "_", `\`, "*", "-", "0", "1", "42", "+1555",
	"é", "É", "é", "ß", "ẞ", "ı", "İ", "K" /* Kelvin sign */, "k", "K", "Σ", "σ", "ς",
	"日本", "語", "😀", "👍🏽", "👨‍👩‍👧", "​",
	"\x00", "\xff", "\xc3",
}

// substringSearchQuickConfig runs the properties below over 30 random stores,
// or 8 under -race: the race build exists to find data races, the properties
// reach every path within a few stores, and the plain test run covers all 30.
func substringSearchQuickConfig() *quick.Config {
	if raceDetectorEnabled {
		return &quick.Config{MaxCount: 8}
	}
	return readPlanQuickConfig
}

func randomSearchText(r *rand.Rand, pieces int) string {
	var b strings.Builder
	for range pieces {
		piece := searchTextPieces[r.Intn(len(searchTextPieces))]
		// NUL and invalid UTF-8 stay rare, as they are in transport text.
		if (piece == "\x00" || piece == "\xff" || piece == "\xc3") && r.Intn(4) != 0 {
			piece = "x"
		}
		b.WriteString(piece)
	}
	return b.String()
}

// randomSearchQuery draws a query from the stored values (a byte range, which
// may split a UTF-8 sequence, with its case changed and wildcards inserted) or
// from scratch, including the empty and one- and two-character queries the
// index cannot narrow.
func randomSearchQuery(r *rand.Rand, values []string) string {
	if len(values) == 0 || r.Intn(5) == 0 {
		return randomSearchText(r, r.Intn(4))
	}
	value := values[r.Intn(len(values))]
	if value == "" {
		return value
	}
	start := r.Intn(len(value))
	end := start + r.Intn(len(value)-start+1)
	query := value[start:end]
	switch r.Intn(5) {
	case 0:
		query = strings.ToUpper(query)
	case 1:
		query = strings.ToLower(query)
	case 2:
		query = strings.Map(func(c rune) rune {
			if c < unicode.MaxASCII && r.Intn(2) == 0 {
				return unicode.SimpleFold(c)
			}
			return c
		}, query)
	case 3:
		if query != "" {
			at := r.Intn(len(query) + 1)
			query = query[:at] + []string{"%", "_"}[r.Intn(2)] + query[at:]
		}
	}
	return query
}

// Invariant: for every query and filter combination, SearchMessages' whole-scope
// statement returns exactly the messages, in exactly the order, of the plain
// LIKE statement; the repository method returns the same through every path
// its limits choose; and a recent window that reports itself complete holds
// that same answer.
func TestSearchMessagesMatchesLikeProperty(t *testing.T) {
	pathsSeen := map[searchPath]bool{}
	defer func() {
		for _, path := range []searchPath{searchPathRecentWindow, searchPathTrigramIndex, searchPathLike} {
			if !t.Failed() && !pathsSeen[path] {
				t.Errorf("no search took the %s path (saw %v); the property must reach every path", path, pathsSeen)
			}
		}
	}()
	property := func(c readPlanStore) bool {
		s := seedReadStore(t, c.Seed)
		r := rand.New(rand.NewSource(c.Seed ^ 0x7f75))
		var bodies []string
		for _, message := range s.messages {
			body := randomSearchText(r, r.Intn(12))
			bodies = append(bodies, body)
			mustExec(t, s.store.db, `UPDATE messages SET body = ? WHERE message_id = ?`, body, message.MessageID)
		}
		repository := mustMessageRepository(t, s.store, 100)
		pick := func(values []string) string { return values[r.Intn(len(values))] }
		for range 80 {
			filter := SearchQuery{Limit: 1 + r.Intn(12)}
			if r.Intn(4) == 0 {
				filter.AccountID = pick(append(slices.Clone(s.accounts), "account-none"))
			}
			if r.Intn(3) == 0 {
				filter.ConversationID = s.conversations[r.Intn(len(s.conversations))].ConversationID
			}
			if r.Intn(3) == 0 {
				filter.SenderCanonicalValue = pick(append(slices.Clone(s.phones), "uuid-1", "+19999999999"))
			}
			if r.Intn(3) == 0 {
				filter.SinceMS = int64(r.Intn(9))
			}
			if r.Intn(3) == 0 {
				filter.UntilMS = int64(r.Intn(9))
			}
			query := randomSearchQuery(r, bodies)
			normalized := filter
			if normalized.SinceMS > 0 && normalized.UntilMS > 0 && normalized.UntilMS < normalized.SinceMS {
				normalized.SinceMS, normalized.UntilMS = normalized.UntilMS, normalized.SinceMS
			}
			got := assertMessageSearchMatchesLike(t, s.store.db, query, normalized)
			// Windows from zero to past the store's size reach every path: the
			// window answers with limit matches or is the whole range, or the
			// whole scope is searched by the index or the LIKE.
			window := r.Intn(len(s.messages) + 3)
			messages, path, err := repository.searchMessages(context.Background(), query, filter, window)
			if err != nil {
				t.Fatalf("searchMessages(%q, %+v, %d): %v", query, filter, window, err)
			}
			pathsSeen[path] = true
			likeSQL, likeArgs := likeSearchMessagesStatement(query, normalized)
			want := queryMessages(t, s.store.db, likeSQL, likeArgs...)
			if !reflect.DeepEqual(messages, want) {
				t.Errorf("seed %d: searchMessages(%q, %+v, window %d) = %v, LIKE = %v", c.Seed, query, filter, window, messageIDs(messages), got)
				return false
			}
			// The recent window alone, when it claims to be complete, is the answer.
			recent, complete, err := repository.searchRecentMessages(context.Background(), query, normalized, window)
			if err != nil {
				t.Fatalf("searchRecentMessages(%q, %+v, %d): %v", query, normalized, window, err)
			}
			if complete && !reflect.DeepEqual(recent, want) {
				t.Errorf("seed %d: complete recent window %d for %q %+v = %v, want %v", c.Seed, window, query, normalized, messageIDs(recent), got)
				return false
			}
		}
		return true
	}
	if err := quick.Check(property, substringSearchQuickConfig()); err != nil {
		t.Fatal(err)
	}
}

// Invariant: for every query, SearchConversationsByName returns exactly the
// conversations, in exactly the order, of the escaped LIKE over titles,
// participant names, identity names and canonical values.
func TestSearchConversationsByNameMatchesLikeProperty(t *testing.T) {
	property := func(c readPlanStore) bool {
		s := seedReadStore(t, c.Seed)
		r := rand.New(rand.NewSource(c.Seed ^ 0xc0417))
		db := s.store.db
		var values []string
		for _, conversation := range s.conversations {
			title := randomSearchText(r, r.Intn(6))
			values = append(values, title)
			mustExec(t, db, `UPDATE conversations SET title = ? WHERE conversation_id = ?`, title, conversation.ConversationID)
		}
		rows, err := db.Query(`SELECT conversation_id, identity_id FROM conversation_participants`)
		if err != nil {
			t.Fatal(err)
		}
		var participants [][2]string
		for rows.Next() {
			var conversationID, identityID string
			if err := rows.Scan(&conversationID, &identityID); err != nil {
				t.Fatal(err)
			}
			participants = append(participants, [2]string{conversationID, identityID})
		}
		rows.Close()
		for _, participant := range participants {
			name := randomSearchText(r, r.Intn(5))
			values = append(values, name)
			mustExec(t, db, `UPDATE conversation_participants SET display_name = ? WHERE conversation_id = ? AND identity_id = ?`,
				name, participant[0], participant[1])
		}
		for _, identity := range s.identities {
			name := randomSearchText(r, r.Intn(5))
			// canonical_value stays unique per (account, kind) and non-blank.
			canonical := identity.CanonicalValue + randomSearchText(r, r.Intn(3)) + "#" + identity.IdentityID
			values = append(values, name, canonical)
			mustExec(t, db, `UPDATE identities SET display_name = ?, canonical_value = ? WHERE identity_id = ?`,
				name, canonical, identity.IdentityID)
		}
		for range 80 {
			query := randomSearchQuery(r, values)
			if strings.TrimSpace(query) == "" {
				continue // SearchConversationsByName returns nothing without running SQL
			}
			limit := 1 + r.Intn(12)
			want := assertConversationSearchMatchesLike(t, db, query, limit)
			got, err := s.store.SearchConversationsByName(query, limit)
			if err != nil {
				t.Fatalf("SearchConversationsByName(%q): %v", query, err)
			}
			if ids := conversationIDs(got); !slices.Equal(ids, want) {
				t.Errorf("seed %d: SearchConversationsByName(%q, %d) = %v, statement = %v", c.Seed, query, limit, ids, want)
				return false
			}
		}
		return true
	}
	if err := quick.Check(property, substringSearchQuickConfig()); err != nil {
		t.Fatal(err)
	}
}

// Invariant: after any sequence of writes, each trigram index equals its
// table and every search matches LIKE. The writes use each statement shape
// that can change an indexed row: inserts, upserts that change or keep the
// text, updates of the text and of other columns (a move between
// conversations), REPLACE (a delete and an insert), deletes, and deletes of a
// conversation that cascade to its messages and roster.
func TestSearchIndexesEqualTablesAfterRandomWritesProperty(t *testing.T) {
	property := func(c readPlanStore) bool {
		s := seedReadStore(t, c.Seed)
		r := rand.New(rand.NewSource(c.Seed ^ 0x3417e5))
		db := s.store.db
		// Read cursors and outbox rows point at messages; they play no part in
		// search, and clearing them lets any message or conversation be deleted.
		mustExec(t, db, `DELETE FROM read_cursors`)
		mustExec(t, db, `DELETE FROM outbox`)
		conversationIDs := func() []string {
			rows, err := db.Query(`SELECT conversation_id FROM conversations ORDER BY conversation_id`)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			var ids []string
			for rows.Next() {
				var id string
				if err := rows.Scan(&id); err != nil {
					t.Fatal(err)
				}
				ids = append(ids, id)
			}
			return ids
		}
		var texts []string
		text := func() string {
			value := randomSearchText(r, r.Intn(8))
			texts = append(texts, value)
			return value
		}
		next := 0
		for range 60 {
			conversations := conversationIDs()
			if len(conversations) == 0 {
				break
			}
			conversation := conversations[r.Intn(len(conversations))]
			var account string
			if err := db.QueryRow(`SELECT account_id FROM conversations WHERE conversation_id = ?`, conversation).Scan(&account); err != nil {
				t.Fatal(err)
			}
			next++
			messageID := fmt.Sprintf("written-%d", next)
			anyMessage := `(SELECT message_id FROM messages ORDER BY message_id LIMIT 1 OFFSET ?)`
			offset := r.Intn(8)
			op := r.Intn(11)
			switch op {
			case 0, 1:
				mustExec(t, db, `INSERT INTO messages (message_id, conversation_id, account_id, remote_message_id, direction, body, occurred_at_ms, created_at_ms, updated_at_ms)
					VALUES (?, ?, ?, ?, 'incoming', ?, ?, 1, 1)`, messageID, conversation, account, "remote-"+messageID, text(), 1+r.Intn(8))
			case 2:
				// The projection upsert: same natural key, new or unchanged body.
				body := text()
				if r.Intn(2) == 0 {
					body = "unchanged"
				}
				mustExec(t, db, `INSERT INTO messages (message_id, conversation_id, account_id, remote_message_id, direction, body, occurred_at_ms, created_at_ms, updated_at_ms)
					SELECT ?, conversation_id, account_id, remote_message_id, direction, ?, occurred_at_ms, 1, 1 FROM messages WHERE message_id = `+anyMessage+`
					ON CONFLICT(account_id, conversation_id, remote_message_id) DO UPDATE SET body = excluded.body
					WHERE messages.body IS NOT excluded.body`, messageID, body, offset)
			case 3:
				mustExec(t, db, `UPDATE messages SET body = ?, state = 'edited' WHERE message_id = `+anyMessage, text(), offset)
			case 4:
				mustExec(t, db, `UPDATE messages SET state = 'deleted', updated_at_ms = updated_at_ms + 1 WHERE message_id = `+anyMessage, offset)
			case 5:
				mustExec(t, db, `UPDATE messages SET conversation_id = ? WHERE message_id = `+anyMessage+` AND account_id = ?`, conversation, offset, account)
			case 6:
				mustExec(t, db, `REPLACE INTO messages (message_id, conversation_id, account_id, remote_message_id, direction, body, occurred_at_ms, created_at_ms, updated_at_ms)
					SELECT message_id, conversation_id, account_id, remote_message_id, direction, ?, occurred_at_ms, created_at_ms, updated_at_ms
					FROM messages WHERE message_id = `+anyMessage, text(), offset)
			case 7:
				mustExec(t, db, `DELETE FROM messages WHERE message_id = `+anyMessage, offset)
			case 8:
				mustExec(t, db, `UPDATE conversations SET title = ?, last_message_at_ms = last_message_at_ms + 1 WHERE conversation_id = ?`, text(), conversation)
				mustExec(t, db, `UPDATE conversation_participants SET display_name = ? WHERE conversation_id = ?`, text(), conversation)
			case 9:
				mustExec(t, db, `UPDATE identities SET display_name = ? WHERE account_id = ? AND is_self = 0`, text(), account)
			case 10:
				if r.Intn(3) == 0 {
					mustExec(t, db, `DELETE FROM conversations WHERE conversation_id = ?`, conversation)
				}
			}
			// Checking after every write names the statement that broke an index.
			if err := s.store.VerifySearchIndexes(context.Background()); err != nil {
				t.Errorf("seed %d: write %d (op %d) left an index unequal to its table: %v", c.Seed, next, op, err)
				return false
			}
		}
		for range 30 {
			query := randomSearchQuery(r, texts)
			assertMessageSearchMatchesLike(t, db, query, SearchQuery{Limit: 1 + r.Intn(20)})
			if strings.TrimSpace(query) != "" {
				assertConversationSearchMatchesLike(t, db, query, 1+r.Intn(20))
			}
		}
		return true
	}
	if err := quick.Check(property, substringSearchQuickConfig()); err != nil {
		t.Fatal(err)
	}
}
