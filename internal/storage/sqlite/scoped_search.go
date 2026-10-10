package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// A search narrowed by conversation or sender (indexBoundedSearch) reads that
// scope's index range with the LIKE. The trigram index returns matches from
// every conversation, so it costs time in proportion to the term's matches
// across the store, which a term common elsewhere makes far larger than the
// scope; but for a term rare in a long thread the LIKE reads the whole thread,
// which on a copy of the live store with ten times the history (one 127,000
// message thread) took 70 to 135 ms, where the index answered in 0.1 to 10 ms.
//
// searchScope chooses between them from counts it reads in the scope's
// indexes and the trigram index, never from timings, so a search always
// takes the same path. Every path applies the same LIKE and returns the
// LIKE's rows; the choice only changes the time taken.
//
// Measured on copies of the live store (72,000 messages, and the same
// conversations with ten times the history):
//   - reading a scope row and applying the LIKE: 0.3 to 1.2 us;
//   - counting a scope row in a covering index walk: 60 to 85 ns;
//   - the index's merge of the query's trigram postings: 0.1 to 1.3 ms on
//     the live copy, up to 11 ms at ten times the history. It grows with the
//     store, not with the scope, and long words and phrases made of common
//     trigrams cost the most. Counting the candidates with MATCH is the same
//     merge;
//   - then joining each candidate to its message and filtering it: 0.3 to
//     1.3 us, about one to two scope rows.
//
// So the index pays off only when the LIKE would still read many rows: more
// than one merge costs, and about twice the candidates. That many rows is
// scopeIndexMinRows, or 1/scopeIndexStoreShare of the store when larger. The
// index search (scopeIndexSearchStatement) then reads at most half as many
// candidates and gives up, for the LIKE, when there are more; it merges once,
// counting and searching in one statement.
const (
	// scopeIndexMinRows is the fewest rows the LIKE must still have to read
	// before searchScope tries the trigram index. One merge on the live
	// store's copy took up to 1.3 ms, the time it reads about 3,000 rows.
	scopeIndexMinRows = 3000
	// scopeIndexStoreShare scales that minimum with the store, as the merge
	// grows: 1/50 of the messages, 14,500 rows at ten times the live history,
	// where one merge took up to 10.8 ms and a row about 0.8 us.
	scopeIndexStoreShare = 50
	// scopeIndexRowsPerCandidate is how many scope rows the LIKE must have
	// left per trigram candidate the index search may read.
	scopeIndexRowsPerCandidate = 2
	// senderIndexMinimums is how many times the minimum a sender must hold
	// for the index to be tried. A conversation's window answers the terms
	// common in it before the index is considered; a sender has none, so each
	// of its common terms pays for an index search that gives up. With twice
	// the minimum to read, that is at most about half the LIKE's time.
	senderIndexMinimums = 2
	// conversationSearchWindow is how many of a conversation's newest
	// messages are read before the rest. It is smaller than
	// recentSearchWindow: a conversation search seeks its boundary every time,
	// and a smaller window leaves more of a mid-sized thread beyond it for the
	// index. Over the measured searches on the live store's copy, 500 took 13%
	// less time in total than 2,000; at ten times the history it took 3% more,
	// and 250 predicted too few remaining rows there to try the index at all.
	conversationSearchWindow = 500
)

// searchPlan holds the sizes SearchMessages plans with. Tests shrink them so
// small stores reach every path.
type searchPlan struct {
	// window is how many of a range's newest messages are read before the
	// rest of it (recentSearchWindow); conversationWindow is the same for a
	// conversation's range (conversationSearchWindow), and zero means window.
	window             int
	conversationWindow int
	// indexMinRows and indexStoreShare set the fewest rows a scope's LIKE
	// must have left before the trigram index is considered:
	// max(indexMinRows, messages/indexStoreShare).
	indexMinRows    int
	indexStoreShare int
	// indexRowsPerCandidate is scopeIndexRowsPerCandidate; zero means 2.
	indexRowsPerCandidate int
	// senderMinimums is senderIndexMinimums; zero means 1.
	senderMinimums int
}

var defaultSearchPlan = searchPlan{
	window:                recentSearchWindow,
	conversationWindow:    conversationSearchWindow,
	indexMinRows:          scopeIndexMinRows,
	indexStoreShare:       scopeIndexStoreShare,
	indexRowsPerCandidate: scopeIndexRowsPerCandidate,
	senderMinimums:        senderIndexMinimums,
}

const (
	// searchPathConversationWindow: a conversation's newest window messages
	// held limit matches.
	searchPathConversationWindow searchPath = "conversation window"
	// searchPathConversationRest: the conversation's newest window messages,
	// then the LIKE over the rest of it.
	searchPathConversationRest searchPath = "conversation window then rest"
	// searchPathScopeIndex: the trigram index's candidates, filtered to the
	// conversation or sender.
	searchPathScopeIndex searchPath = "scope index"
)

// searchQuerier runs a scoped search's statements: the store, or the read
// transaction searchScope composes an answer in.
type searchQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// searchScope answers a nonempty query that a conversation or sender bounds,
// for a normalized filter, when expression (trigramMatchQuery) narrows it.
//
// A conversation is read newest first and the LIKE stops at limit matches,
// so a term common in the thread is answered by its newest rows. searchScope
// therefore first reads the newest conversationWindow messages of the
// conversation's range (the rows at or above boundary, the window-th newest),
// and stops if they hold limit matches. Otherwise the rows below boundary
// remain. The window's match rate predicts how many of them the LIKE would
// read before it found the rest of its matches (expectedLikeRows); when that
// many rows remain (scopeIndexBound), the index search answers if it has few
// enough candidates; otherwise the LIKE continues below boundary.
//
// A sender's messages can come from several identities, whose rows the LIKE
// reads in full and sorts whatever the term (one identity's it reads in time
// order, stopping at limit matches). searchScope counts them and decides the
// same way, asking for senderIndexMinimums times the minimum.
//
// Either answer is put together from several statements: a conversation's
// window and rest, a sender's identities and their messages. They run in one
// read transaction, so a write between them cannot move a message across the
// boundary, or to another identity of the sender, to be returned twice or not
// at all.
func (r *MessageRepository) searchScope(
	ctx context.Context,
	query string,
	expression string,
	filter SearchQuery,
	plan searchPlan,
) ([]Message, searchPath, error) {
	tx, err := r.store.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, "", fmt.Errorf("search scope: begin: %w", err)
	}
	defer tx.Rollback()
	if filter.ConversationID == "" {
		return r.searchSender(ctx, tx, query, expression, filter, plan)
	}
	return r.searchConversation(ctx, tx, query, expression, filter, plan)
}

func (r *MessageRepository) searchSender(
	ctx context.Context,
	tx searchQuerier,
	query string,
	expression string,
	filter SearchQuery,
	plan searchPlan,
) ([]Message, searchPath, error) {
	// The sender's identities are read once and named in each statement:
	// identities has no index on canonical_value, so the filter's subquery
	// reads the whole table every time a statement runs it.
	senderIDs, err := senderIdentityIDs(ctx, tx, filter.SenderCanonicalValue)
	if err != nil {
		return nil, "", err
	}
	if r.betweenSearchStatements != nil {
		r.betweenSearchStatements()
	}
	if len(senderIDs) == 0 {
		// No identity holds the address, so no message has such a sender.
		return []Message{}, searchPathLike, nil
	}
	countStatement, countArgs := senderRangeCountStatement(filter, senderIDs)
	bound, err := scopeIndexBound(ctx, tx, -1, max(plan.senderMinimums, 1), plan, func(upTo int) (int, error) {
		return countSearchRows(ctx, tx, countStatement, append(countArgs, upTo)...)
	})
	if err != nil {
		return nil, "", err
	}
	if bound > 0 {
		messages, complete, err := scopeIndexSearch(ctx, tx, expression, query, filter, senderIDs, bound)
		if err != nil || complete {
			return messages, searchPathScopeIndex, err
		}
	}
	statement, args := senderLikeSearchStatement(query, filter, senderIDs)
	messages, err := querySearchMessages(ctx, tx, statement, args)
	return messages, searchPathLike, err
}

// senderIdentityIDs returns the identities holding a canonical address
// (senderIdentityIDsQuery).
func senderIdentityIDs(ctx context.Context, q searchQuerier, canonicalValue string) ([]string, error) {
	rows, err := q.QueryContext(ctx, senderIdentityIDsQuery, canonicalValue)
	if err != nil {
		return nil, fmt.Errorf("resolve sender identities: %w", err)
	}
	ids, err := collectRows(rows, func(row rowScanner) (string, error) {
		var id string
		err := row.Scan(&id)
		return id, err
	})
	if err != nil {
		return nil, fmt.Errorf("resolve sender identities: %w", err)
	}
	return ids, nil
}

// resolvedSenderFilters is searchMessageFilters with the sender's identities
// named by ID (senderIdentityIDs) rather than selected by the subquery.
func resolvedSenderFilters(filter SearchQuery, senderIDs []string) ([]string, []any) {
	others := filter
	others.SenderCanonicalValue = ""
	conditions, args := searchMessageFilters(others)
	senderArgs := make([]any, len(senderIDs))
	for i, id := range senderIDs {
		senderArgs[i] = id
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?, ", len(senderIDs)), ", ")
	return append([]string{"m.sender_identity_id IN (" + placeholders + ")"}, conditions...), append(senderArgs, args...)
}

// senderLikeSearchStatement is likeSearchMessagesStatement for a sender whose
// identities are named by ID: the same rows, without reading identities again.
func senderLikeSearchStatement(query string, filter SearchQuery, senderIDs []string) (string, []any) {
	conditions, args := resolvedSenderFilters(filter, senderIDs)
	conditions = append(conditions, "m.body LIKE '%' || ? || '%'")
	args = append(args, query, filter.Limit)
	statement := `
		SELECT ` + prefixedMessageColumns("m") + `
		FROM messages AS m
		WHERE ` + strings.Join(conditions, " AND ") + `
		ORDER BY m.occurred_at_ms DESC, m.message_id DESC
		LIMIT ?
	`
	return statement, args
}

func (r *MessageRepository) searchConversation(
	ctx context.Context,
	tx searchQuerier,
	query string,
	expression string,
	filter SearchQuery,
	plan searchPlan,
) ([]Message, searchPath, error) {
	window := plan.conversationWindow
	if window == 0 {
		window = plan.window
	}
	window = max(window, 1)
	boundary, found, err := conversationBoundary(ctx, tx, filter, window)
	if err != nil {
		return nil, "", err
	}
	if !found {
		// Fewer than window rows: the LIKE reads them all in any case.
		statement, args := likeSearchMessagesStatement(query, filter)
		messages, err := querySearchMessages(ctx, tx, statement, args)
		return messages, searchPathLike, err
	}
	statement, args := conversationSplitSearchStatement(query, filter, boundary, true, filter.Limit)
	matches, err := querySearchMessages(ctx, tx, statement, args)
	if err != nil {
		return nil, "", fmt.Errorf("conversation window: %w", err)
	}
	if len(matches) >= filter.Limit {
		return matches, searchPathConversationWindow, nil
	}
	if r.betweenSearchStatements != nil {
		r.betweenSearchStatements()
	}
	countStatement, countArgs := conversationRestCountStatement(filter, boundary)
	expected := expectedLikeRows(filter.Limit, len(matches), window)
	bound, err := scopeIndexBound(ctx, tx, expected, 1, plan, func(upTo int) (int, error) {
		return countSearchRows(ctx, tx, countStatement, append(countArgs, upTo)...)
	})
	if err != nil {
		return nil, "", err
	}
	if bound > 0 {
		messages, complete, err := scopeIndexSearch(ctx, tx, expression, query, filter, nil, bound)
		if err != nil || complete {
			return messages, searchPathScopeIndex, err
		}
	}
	statement, args = conversationSplitSearchStatement(query, filter, boundary, false, filter.Limit-len(matches))
	rest, err := querySearchMessages(ctx, tx, statement, args)
	if err != nil {
		return nil, "", fmt.Errorf("conversation rest: %w", err)
	}
	return append(matches, rest...), searchPathConversationRest, nil
}

// scopeIndexBound returns how many trigram candidates the index search may
// read for a scoped search, or 0 when the LIKE should answer it. expected is
// how many rows the LIKE is expected to read before it finds its matches, or
// negative when it reads every row of the scope (a sender's); countRows(upTo)
// counts the rows it has left, up to upTo. The index is tried when the LIKE
// has at least minimums times minRows left (plan), with a bound of
// minRows/scopeIndexRowsPerCandidate candidates. The count stops there, so
// deciding reads at most that many index entries.
func scopeIndexBound(
	ctx context.Context,
	q searchQuerier,
	expected int,
	minimums int,
	plan searchPlan,
	countRows func(upTo int) (int, error),
) (int, error) {
	minRows := plan.indexMinRows
	if plan.indexStoreShare > 0 {
		var maxRowID sql.NullInt64
		if err := q.QueryRowContext(ctx, `SELECT max(rowid) FROM messages`).Scan(&maxRowID); err != nil {
			return 0, fmt.Errorf("size the store for search: %w", err)
		}
		minRows = max(minRows, int(maxRowID.Int64/int64(plan.indexStoreShare)))
	}
	rowsPerCandidate := plan.indexRowsPerCandidate
	if rowsPerCandidate <= 0 {
		rowsPerCandidate = 2
	}
	bound := minRows / rowsPerCandidate
	needed := minRows * minimums
	if bound < 1 || (expected >= 0 && expected < needed) {
		return 0, nil
	}
	rows, err := countRows(needed)
	if err != nil {
		return 0, fmt.Errorf("count scope rows: %w", err)
	}
	if rows < needed {
		return 0, nil
	}
	return bound, nil
}

func countSearchRows(ctx context.Context, q searchQuerier, statement string, args ...any) (int, error) {
	var n int
	if err := q.QueryRowContext(ctx, statement, args...).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// expectedLikeRows estimates how many more rows a newest-first LIKE reads to
// find limit matches when its first window rows held matched of them: the
// window's match rate, (matched+1)/(window+1) so that a window without
// matches still predicts a finite walk, applied to the matches still missing.
func expectedLikeRows(limit, matched, window int) int {
	missing := int64(limit - matched)
	rows := (missing*int64(window+1) + int64(matched)) / int64(matched+1)
	return int(min(rows, int64(1)<<40))
}

// scopeIndexSearchStatement reads at most bound of the trigram index's
// candidates for expression, counts them, and, when there are fewer than
// bound (all of them), keeps those in the filter's scope whose body matches
// the LIKE, newest first. Each row starts with the candidate count and
// whether it holds a message: when none matches, or there were bound or more
// candidates, one row carries the count alone.
//
// The MATCH selects the rows FTS5 reads for the LIKE on its own column
// (trigramMatchQuery) without reading a message, so counting the candidates
// reads only the index (about 0.1 us each in the probes), and a candidate
// from another conversation costs one lookup by rowid rather than a fetch of
// its body through the index. The candidates are materialized once, so the
// postings are merged once whether the search goes on or gives up; with too
// many, the guard on the count stops it before any lookup (the statement
// then costs what counting alone does). senderIDs, when not nil, names the
// sender filter's identities (resolvedSenderFilters).
func scopeIndexSearchStatement(expression, query string, filter SearchQuery, senderIDs []string, bound int) (string, []any) {
	conditions, filterArgs := searchMessageFilters(filter)
	if senderIDs != nil {
		conditions, filterArgs = resolvedSenderFilters(filter, senderIDs)
	}
	conditions = append(append([]string{"(SELECT n FROM candidate_count) < ?"}, conditions...),
		"m.body LIKE '%' || ? || '%'")
	args := append(append([]any{expression, bound, bound}, filterArgs...), query, filter.Limit)
	statement := `
		WITH candidates(id) AS MATERIALIZED (
			SELECT rowid FROM messages_fts WHERE messages_fts MATCH ? LIMIT ?
		),
		candidate_count(n) AS MATERIALIZED (SELECT count(*) FROM candidates)
		SELECT
			candidate_count.n,
			found.message_id IS NOT NULL,
			coalesce(found.message_id, ''),
			coalesce(found.conversation_id, ''),
			coalesce(found.account_id, ''),
			coalesce(found.remote_message_id, ''),
			found.sender_identity_id,
			coalesce(found.direction, ''),
			coalesce(found.body, ''),
			found.reply_to_remote_id,
			coalesce(found.state, ''),
			coalesce(found.occurred_at_ms, 0),
			coalesce(found.created_at_ms, 0),
			coalesce(found.updated_at_ms, 0)
		FROM candidate_count
		LEFT JOIN (
			SELECT ` + prefixedMessageColumns("m") + `
			FROM candidates AS c
			CROSS JOIN messages AS m ON m.rowid = c.id
			WHERE ` + strings.Join(conditions, " AND ") + `
			ORDER BY m.occurred_at_ms DESC, m.message_id DESC
			LIMIT ?
		) AS found
		ORDER BY found.occurred_at_ms DESC, found.message_id DESC
	`
	return statement, args
}

// scopeIndexSearch runs scopeIndexSearchStatement. complete is false, with no
// messages, when the index held bound or more candidates.
func scopeIndexSearch(
	ctx context.Context,
	q searchQuerier,
	expression string,
	query string,
	filter SearchQuery,
	senderIDs []string,
	bound int,
) (messages []Message, complete bool, err error) {
	statement, args := scopeIndexSearchStatement(expression, query, filter, senderIDs, bound)
	rows, err := q.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, false, fmt.Errorf("scope index search: %w", err)
	}
	defer rows.Close()
	// Like collectRows, an answer without matches is empty, not nil.
	messages = []Message{}
	var candidates int
	for rows.Next() {
		var found bool
		message, err := scanMessage(countedRow{rows: rows, prefix: []any{&candidates, &found}})
		if err != nil {
			return nil, false, fmt.Errorf("scope index search: %w", err)
		}
		if found {
			messages = append(messages, message)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("scope index search: %w", err)
	}
	if candidates >= bound {
		return nil, false, nil
	}
	return messages, true, nil
}

// countedRow scans a row's leading columns into prefix and the rest into the
// destinations its caller passes.
type countedRow struct {
	rows   *sql.Rows
	prefix []any
}

func (r countedRow) Scan(dest ...any) error {
	return r.rows.Scan(append(append([]any{}, r.prefix...), dest...)...)
}

// conversationKey is a message's position in messages_conversation_time_idx.
type conversationKey struct {
	occurredAtMS int64
	messageID    string
}

// conversationRange returns the conversation and date bounds of a filter's
// range in messages_conversation_time_idx, the only conditions the index
// itself answers.
func conversationRange(filter SearchQuery) (conditions []string, args []any) {
	conditions = append(conditions, "m.conversation_id = ?")
	args = append(args, filter.ConversationID)
	if filter.SinceMS > 0 {
		conditions = append(conditions, "m.occurred_at_ms >= ?")
		args = append(args, filter.SinceMS)
	}
	if filter.UntilMS > 0 {
		conditions = append(conditions, "m.occurred_at_ms <= ?")
		args = append(args, filter.UntilMS)
	}
	return conditions, args
}

// conversationBoundary returns the key of the window-th newest row of the
// filter's conversation range, read from the index alone, or found false when
// the range holds fewer rows.
func conversationBoundary(
	ctx context.Context,
	q searchQuerier,
	filter SearchQuery,
	window int,
) (conversationKey, bool, error) {
	conditions, args := conversationRange(filter)
	args = append(args, window-1)
	var key conversationKey
	err := q.QueryRowContext(ctx, `
		SELECT m.occurred_at_ms, m.message_id
		FROM messages AS m INDEXED BY messages_conversation_time_idx
		WHERE `+strings.Join(conditions, " AND ")+`
		ORDER BY m.occurred_at_ms DESC, m.message_id DESC
		LIMIT 1 OFFSET ?
	`, args...).Scan(&key.occurredAtMS, &key.messageID)
	if errors.Is(err, sql.ErrNoRows) {
		return conversationKey{}, false, nil
	}
	if err != nil {
		return conversationKey{}, false, fmt.Errorf("conversation window boundary: %w", err)
	}
	return key, true, nil
}

// conversationSplitSearchStatement is likeSearchMessagesStatement for a
// conversation with its range cut at boundary: the rows at or above it (the
// newest ones, when above is true) or the rows below it. The two parts list
// the whole range in its order, so the first limit matches of the upper part
// followed by the first ones of the lower part are the LIKE's matches.
// INDEXED BY keeps the walk on messages_conversation_time_idx, newest first
// and stopping at limit matches, whatever other filters apply.
func conversationSplitSearchStatement(
	query string,
	filter SearchQuery,
	boundary conversationKey,
	above bool,
	limit int,
) (string, []any) {
	conditions, args := conversationSplitRange(filter, boundary, above)
	// The account and sender filters apply to the rows the range yields.
	others := filter
	others.ConversationID, others.SinceMS, others.UntilMS = "", 0, 0
	otherConditions, otherArgs := searchMessageFilters(others)
	conditions = append(append(conditions, otherConditions...), "m.body LIKE '%' || ? || '%'")
	args = append(append(args, otherArgs...), query, limit)
	statement := `
		SELECT ` + prefixedMessageColumns("m") + `
		FROM messages AS m INDEXED BY messages_conversation_time_idx
		WHERE ` + strings.Join(conditions, " AND ") + `
		ORDER BY m.occurred_at_ms DESC, m.message_id DESC
		LIMIT ?
	`
	return statement, args
}

// conversationSplitRange bounds the part of a filter's conversation range at
// or above boundary, or below it. The boundary row lies in the range, so the
// upper part already starts after since and the lower part ends before until;
// leaving out the bound the boundary implies lets SQLite seek the index with
// the boundary itself, where both bounds on occurred_at_ms would make it seek
// with the dates and only filter by the boundary, reading index entries past
// it to the end of the range.
func conversationSplitRange(filter SearchQuery, boundary conversationKey, above bool) ([]string, []any) {
	conditions := []string{"m.conversation_id = ?"}
	args := []any{filter.ConversationID}
	if above {
		conditions = append(conditions, "(m.occurred_at_ms, m.message_id) >= (?, ?)")
		args = append(args, boundary.occurredAtMS, boundary.messageID)
		if filter.UntilMS > 0 {
			conditions = append(conditions, "m.occurred_at_ms <= ?")
			args = append(args, filter.UntilMS)
		}
		return conditions, args
	}
	conditions = append(conditions, "(m.occurred_at_ms, m.message_id) < (?, ?)")
	args = append(args, boundary.occurredAtMS, boundary.messageID)
	if filter.SinceMS > 0 {
		conditions = append(conditions, "m.occurred_at_ms >= ?")
		args = append(args, filter.SinceMS)
	}
	return conditions, args
}

// conversationRestCountStatement counts the rows of a conversation range
// below boundary, up to a bound appended as the last argument, from the index
// alone.
func conversationRestCountStatement(filter SearchQuery, boundary conversationKey) (string, []any) {
	conditions, args := conversationSplitRange(filter, boundary, false)
	statement := `
		SELECT count(*) FROM (
			SELECT 1
			FROM messages AS m INDEXED BY messages_conversation_time_idx
			WHERE ` + strings.Join(conditions, " AND ") + `
			LIMIT ?
		)
	`
	return statement, args
}

// senderRangeCountStatement counts the messages of the sender's identities
// within the filter's date range, up to a bound appended as the last
// argument, from messages_sender_time_idx alone: the rows the sender's LIKE
// reads. senderIDs must not be empty: the index holds only messages with a
// sender, which an empty list does not let SQLite conclude.
func senderRangeCountStatement(filter SearchQuery, senderIDs []string) (string, []any) {
	conditions, args := resolvedSenderFilters(SearchQuery{SinceMS: filter.SinceMS, UntilMS: filter.UntilMS}, senderIDs)
	statement := `
		SELECT count(*) FROM (
			SELECT 1
			FROM messages AS m INDEXED BY messages_sender_time_idx
			WHERE ` + strings.Join(conditions, " AND ") + `
			LIMIT ?
		)
	`
	return statement, args
}
