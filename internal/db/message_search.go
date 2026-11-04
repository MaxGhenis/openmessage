package db

import (
	"fmt"
	"math"
	"strings"
	"time"
)

func (s *Store) SearchMessages(query, phoneNumber string, limit int) ([]*Message, error) {
	return s.SearchMessagesFiltered(query, SearchFilter{Phone: phoneNumber, Limit: limit})
}

// SearchMessagesFiltered returns the messages that f keeps and whose body
// matches `body LIKE '%' || query || '%'`, newest first with message_id
// breaking ties, at most f.Limit of them. It answers from the messages_fts
// trigram index when searchIndexPays expects the index to cost less than a
// LIKE scan of messages, and from the scan otherwise. Both return the same
// rows (see searchMessagesIndexed). An index error is logged and the scan
// answers instead, so a broken index slows search down but changes no result.
func (s *Store) SearchMessagesFiltered(query string, f SearchFilter) ([]*Message, error) {
	if f.Limit <= 0 {
		f.Limit = 20
	}
	if f.SinceMS > 0 && f.UntilMS > 0 && f.UntilMS < f.SinceMS {
		f.SinceMS, f.UntilMS = f.UntilMS, f.SinceMS
	}
	if s.ftsEnabled {
		pays, err := s.searchIndexPays(query, f)
		if err != nil {
			s.logger.Error().Err(err).Msg("Message search index probe failed; searching with a LIKE scan")
		} else if pays {
			msgs, err := s.searchMessagesIndexed(query, f)
			if err == nil {
				return msgs, nil
			}
			s.logger.Error().Err(err).Msg("Message search index query failed; searching with a LIKE scan")
		}
	}
	return s.searchMessagesLike(query, f)
}

// searchIndexCandidateCost is about how many rows a LIKE scan reads in the
// time the index path spends on one candidate: finding it in the index,
// checking LIKE on its index copy, fetching its message by message_id and
// sorting it. On an 80,000-message store a candidate costs about 7 µs and a
// scanned row about 0.45 µs.
const searchIndexCandidateCost = 16

// searchIndexTrigramCost is about how many rows a LIKE scan reads in the time
// the index path spends on one trigram of the query, however few candidates it
// yields: FTS5 merges each trigram's postings, once in the probe and again in
// the answer. On 80,000 messages the merges for "passport", 6 trigrams, take
// about 1.4 ms, and those for 64 copies of it about 78 ms.
const searchIndexTrigramCost = 200

// searchCountsTTL is how long the search router trusts searchCounts. It needs
// only the message count's rough size, and counting 80,000 messages takes
// about 1 ms.
const searchCountsTTL = 10 * time.Minute

// searchCounts is what the search router knows about the whole store.
type searchCounts struct {
	messages int
	// indexInSync reports whether the index holds one entry per message, as
	// every write path keeps it and as New makes it. A count that differs
	// means some writer, such as an older build, changed messages without
	// the index since this store opened.
	indexInSync bool
}

// searchIndexPays reports whether the index answers query under f faster than
// a LIKE scan, counting both costs in rows the scan reads. The scan reads
// messages newest first and stops at f.Limit matches. When n of the store's N
// messages match, spread over time, it reads about Limit·N/n rows (all N when
// n is at most Limit), and never more than the F rows that f keeps. The index
// path merges the postings of the query's t trigrams and then reads all n
// candidates, whatever the limit. So the index pays when
//
//	searchIndexTrigramCost·t + searchIndexCandidateCost·n < min(N, Limit·N/n, F)
//
// A common word is the case the index loses: on 80,000 messages, the 50
// newest of 36,000 that contain "the" take the scan 0.2 ms and the index
// path 290 ms. A rare one is the case it wins: 17 matches take the scan, which
// reads every row to find them, 38 ms, and the index path 1 ms.
//
// The probe counts the index rows the LIKE pattern selects (searchIndexMatch)
// and stops one past the most candidates for which the index can still pay.
// It never decides a result, only which path finds it. A query goes to the
// scan without a probe when the index can't look it up, when it has more than
// searchIndexMaxPhrases phrases, when its pattern is longer than LIKE accepts,
// and while the index is out of step with messages, which would make the index
// path miss a message the scan finds. It also skips the probe when the store,
// or the filter, keeps no more than twice as many messages as the merges cost:
// the probe pays for one of the two merges, so a probe that sends the search
// to the scan costs at most about a quarter of scanning them all.
func (s *Store) searchIndexPays(query string, f SearchFilter) (bool, error) {
	match, phrases, trigrams := searchIndexMatch(query)
	if phrases == 0 || phrases > searchIndexMaxPhrases || len(searchLikePattern(query)) > likePatternMaxBytes {
		return false, nil
	}
	counts, err := s.searchedCounts()
	if err != nil {
		return false, err
	}
	total, mergeCost := counts.messages, searchIndexTrigramCost*trigrams
	if !counts.indexInSync || total <= 2*mergeCost {
		return false, nil
	}
	// Most conversations and senders hold few messages, so a filter is
	// counted before the probe.
	conditions, _ := searchFilterConditions(f, "")
	filtered := len(conditions) > 0
	if filtered {
		if kept, err := s.countSearchFilterKeeps(f, 2*mergeCost); err != nil || kept <= 2*mergeCost {
			return false, err
		}
	}
	// candidate cost·n < Limit·N/n
	most := int(math.Sqrt(float64(f.Limit) * float64(total) / searchIndexCandidateCost))
	var candidates int
	if err := s.db.QueryRow(
		`SELECT count(*) FROM (SELECT 1 FROM messages_fts WHERE messages_fts MATCH ? LIMIT ?)`,
		match, most+1,
	).Scan(&candidates); err != nil {
		return false, fmt.Errorf("count search index candidates: %w", err)
	}
	if candidates > most {
		return false, nil
	}
	indexCost := mergeCost + searchIndexCandidateCost*candidates
	scanCost := total
	if candidates > f.Limit {
		scanCost = f.Limit * total / candidates
	}
	if indexCost >= scanCost {
		return false, nil
	}
	if !filtered {
		return true, nil
	}
	// index cost < F
	kept, err := s.countSearchFilterKeeps(f, indexCost)
	return kept > indexCost, err
}

// countSearchFilterKeeps counts the messages f keeps, stopping one past most.
func (s *Store) countSearchFilterKeeps(f SearchFilter, most int) (int, error) {
	conditions, args := searchFilterConditions(f, "")
	var kept int
	if err := s.db.QueryRow(
		`SELECT count(*) FROM (SELECT 1 FROM messages WHERE `+strings.Join(conditions, " AND ")+` LIMIT ?)`,
		append(args, most+1)...,
	).Scan(&kept); err != nil {
		return 0, fmt.Errorf("count messages the search filter keeps: %w", err)
	}
	return kept, nil
}

// searchedCounts returns the store's searchCounts, read at most
// searchCountsTTL ago. FTS5 keeps one messages_fts_docsize row per index
// entry, and counting those takes 0.05 ms on 80,000 messages, where counting
// messages_fts reads every entry and takes 45 ms.
func (s *Store) searchedCounts() (searchCounts, error) {
	s.countsMu.Lock()
	defer s.countsMu.Unlock()
	if !s.countsReadAt.IsZero() && time.Since(s.countsReadAt) < searchCountsTTL {
		return s.counts, nil
	}
	var messages, entries int
	if err := s.db.QueryRow(
		`SELECT (SELECT count(*) FROM messages), (SELECT count(*) FROM messages_fts_docsize)`,
	).Scan(&messages, &entries); err != nil {
		return searchCounts{}, fmt.Errorf("count messages and search index entries: %w", err)
	}
	s.counts = searchCounts{messages: messages, indexInSync: entries == messages}
	s.countsReadAt = time.Now()
	return s.counts, nil
}

// likePatternMaxBytes is the longest pattern SQLite's LIKE accepts
// (SQLITE_MAX_LIKE_PATTERN_LENGTH). LIKE fails a longer one with "LIKE or
// GLOB pattern too complex", but only when it compares a row, so an index
// that finds no candidate would return no rows where the scan fails.
const likePatternMaxBytes = 50_000

// searchIndexMaxPhrases is the most phrases the router lets FTS5 AND. FTS5
// parses an AND of n phrases in about n² time, which no count sees: on 80,000
// messages the index path takes about 1 s for a query of 1,000 runs between
// wildcards, and the scan 70 ms.
const searchIndexMaxPhrases = 64

// searchIndexMatch returns the FTS5 query that selects the index rows FTS5
// reads for `body LIKE '%' || query || '%'`, how many phrases it ANDs and how
// many trigrams they hold. It builds the query as FTS5 builds it for that LIKE
// (sqlite3Fts5ExprPattern): each run of 3 or more characters between the
// wildcards % and _ becomes a phrase, its double quotes doubled, and the
// phrases are ANDed. Inside a phrase every other character is literal, FTS5
// syntax such as *, ^, : and NEAR included. A pattern with no run that long
// gives no phrase: the trigram index can't narrow it. SQLite reads a text
// argument up to its first NUL, so the runs stop there.
func searchIndexMatch(query string) (match string, phrases, trigrams int) {
	if nul := strings.IndexByte(query, 0); nul >= 0 {
		query = query[:nul]
	}
	var quoted []string
	for _, run := range strings.FieldsFunc(query, func(r rune) bool { return r == '%' || r == '_' }) {
		if n := fts5CharCount(run); n >= 3 {
			quoted = append(quoted, `"`+strings.ReplaceAll(run, `"`, `""`)+`"`)
			trigrams += n - 2
		}
	}
	return strings.Join(quoted, " "), len(quoted), trigrams
}

// fts5CharCount counts characters as FTS5 does when it decides whether a run
// is long enough to look up (fts5ExprCountChar): every byte that doesn't
// continue a UTF-8 sequence starts one.
func fts5CharCount(s string) int {
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i]&0xC0 != 0x80 {
			n++
		}
	}
	return n
}

// searchMessagesIndexed answers a search from the trigram index. FTS5 serves
// `body LIKE ?` on messages_fts from the index (EXPLAIN shows INDEX 0:L1), and
// SQLite checks the LIKE again on every row FTS5 returns, so the subquery holds
// exactly the index entries whose body matches. The trigram index folds case
// across Unicode and LIKE only for ASCII, so "É" finds "é" in the index, and
// the check then drops the row, as a scan would never match it. Every match is
// a candidate, so the result is the scan's.
//
// The outer LIKE checks the message's own body, so an index entry whose body
// differs from its message's can't add a row, and DISTINCT keeps a duplicated
// entry from repeating one. CROSS JOIN keeps the index the outer loop: with a
// conversation or sender filter, the planner would otherwise read every
// message the filter keeps, however few candidates the index holds.
func (s *Store) searchMessagesIndexed(query string, f SearchFilter) ([]*Message, error) {
	pattern := searchLikePattern(query)
	conditions, args := searchFilterConditions(f, "m.")
	q := `SELECT ` + qualifiedMessageColumns("m") + `
		FROM (SELECT DISTINCT message_id FROM messages_fts WHERE body LIKE ?) f
		CROSS JOIN messages m ON m.message_id = f.message_id
		WHERE ` + strings.Join(append([]string{"m.body LIKE ?"}, conditions...), " AND ") + `
		ORDER BY m.timestamp_ms DESC, m.message_id DESC
		LIMIT ?`
	args = append(append([]any{pattern, pattern}, args...), f.Limit)

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMessages(rows)
}

func (s *Store) searchMessagesLike(query string, f SearchFilter) ([]*Message, error) {
	conditions, args := searchFilterConditions(f, "")
	q := `SELECT ` + messageColumns + ` FROM messages WHERE ` +
		strings.Join(append([]string{"body LIKE ?"}, conditions...), " AND ") +
		` ORDER BY timestamp_ms DESC, message_id DESC LIMIT ?`
	args = append(append([]any{searchLikePattern(query)}, args...), f.Limit)

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMessages(rows)
}

// searchLikePattern is the LIKE pattern for a body that contains query. Both
// search paths match it, % and _ in the query included.
func searchLikePattern(query string) string {
	return "%" + query + "%"
}

// searchFilterConditions returns f's conditions on messages, each column
// prefixed with prefix, and their arguments.
func searchFilterConditions(f SearchFilter, prefix string) ([]string, []any) {
	var conditions []string
	var args []any
	if f.Phone != "" {
		conditions = append(conditions, prefix+"sender_number = ?")
		args = append(args, f.Phone)
	}
	if f.ConversationID != "" {
		conditions = append(conditions, prefix+"conversation_id = ?")
		args = append(args, f.ConversationID)
	}
	if f.SinceMS > 0 {
		conditions = append(conditions, prefix+"timestamp_ms >= ?")
		args = append(args, f.SinceMS)
	}
	if f.UntilMS > 0 {
		conditions = append(conditions, prefix+"timestamp_ms <= ?")
		args = append(args, f.UntilMS)
	}
	return conditions, args
}

// qualifiedMessageColumns returns messageColumns with each column qualified
// by alias, for a query that joins messages to a table that shares column
// names with it: messages_fts has message_id and body. Prefixing only the
// list's first column left body ambiguous, so the index query failed to
// prepare from March to November 2026 and every search fell back to a scan.
func qualifiedMessageColumns(alias string) string {
	columns := strings.Split(messageColumns, ", ")
	for i, column := range columns {
		columns[i] = alias + "." + column
	}
	return strings.Join(columns, ", ")
}
