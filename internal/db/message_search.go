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

// messageCountTTL is how long the search router trusts a message count. It
// needs only the count's rough size, and counting 80,000 messages takes about
// 3 ms.
const messageCountTTL = 10 * time.Minute

// searchIndexPays reports whether the index answers query under f faster than
// a LIKE scan. The scan reads messages newest first and stops at f.Limit
// matches. When n of the store's N messages match, spread over time, it reads
// about Limit·N/n rows, and never more than the F rows that f keeps. The index
// path reads all n candidates, whatever the limit. So the index pays when
//
//	searchIndexCandidateCost·n < min(F, Limit·N/n)
//
// A common word is the case the index loses: on 80,000 messages, the 50
// newest of 36,000 that contain "the" take the scan 0.2 ms and the index
// path 290 ms. A rare one is the case it wins: 17 matches take the scan, which
// reads every row to find them, 38 ms, and the index path 1 ms.
//
// The probe counts the index rows the LIKE pattern selects (searchIndexMatch)
// and stops one past the most candidates for which the index can still pay.
// It never decides a result, only which path finds it.
func (s *Store) searchIndexPays(query string, f SearchFilter) (bool, error) {
	match := searchIndexMatch(query)
	if match == "" {
		return false, nil
	}
	total, err := s.searchedMessageCount()
	if err != nil {
		return false, err
	}
	// cost·n < Limit·N/n
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
	// cost·n < F
	budget := searchIndexCandidateCost * candidates
	conditions, args := searchFilterConditions(f, "")
	if len(conditions) == 0 {
		return budget < total, nil
	}
	var kept int
	if err := s.db.QueryRow(
		`SELECT count(*) FROM (SELECT 1 FROM messages WHERE `+strings.Join(conditions, " AND ")+` LIMIT ?)`,
		append(args, budget+1)...,
	).Scan(&kept); err != nil {
		return false, fmt.Errorf("count messages the search filter keeps: %w", err)
	}
	return kept > budget, nil
}

// searchedMessageCount returns the number of messages, counted at most
// messageCountTTL ago.
func (s *Store) searchedMessageCount() (int, error) {
	s.messageCountMu.Lock()
	defer s.messageCountMu.Unlock()
	if !s.messageCountReadAt.IsZero() && time.Since(s.messageCountReadAt) < messageCountTTL {
		return s.messageCount, nil
	}
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM messages`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count messages: %w", err)
	}
	s.messageCount, s.messageCountReadAt = n, time.Now()
	return n, nil
}

// searchIndexMatch returns the FTS5 query that selects the index rows FTS5
// reads for `body LIKE '%' || query || '%'`. It builds the query as FTS5
// builds it for that LIKE (sqlite3Fts5ExprPattern): each run of 3 or more
// characters between the wildcards % and _ becomes a phrase, its double
// quotes doubled, and the phrases are ANDed. Inside a phrase every other
// character is literal, FTS5 syntax such as *, ^, : and NEAR included. A
// pattern with no run that long gives "": the trigram index can't narrow it.
// SQLite reads a text argument up to its first NUL, so the runs stop there.
func searchIndexMatch(query string) string {
	if nul := strings.IndexByte(query, 0); nul >= 0 {
		query = query[:nul]
	}
	var phrases []string
	for _, run := range strings.FieldsFunc(query, func(r rune) bool { return r == '%' || r == '_' }) {
		if fts5CharCount(run) >= 3 {
			phrases = append(phrases, `"`+strings.ReplaceAll(run, `"`, `""`)+`"`)
		}
	}
	return strings.Join(phrases, " ")
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
