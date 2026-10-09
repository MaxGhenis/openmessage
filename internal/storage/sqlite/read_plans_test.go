package sqlite

import (
	"database/sql"
	"slices"
	"strings"
	"testing"
)

// explainPlanLines returns the EXPLAIN QUERY PLAN detail lines for query with
// args bound, in plan order. Bound values matter: a partial index is usable
// only when the query's WHERE implies the index's WHERE.
func explainPlanLines(t *testing.T, db *sql.DB, query string, args ...any) []string {
	t.Helper()
	rows, err := db.Query("EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatalf("EXPLAIN QUERY PLAN: %v\n%s", err, query)
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatalf("scan plan row: %v", err)
		}
		lines = append(lines, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read plan rows: %v", err)
	}
	return lines
}

// planScans reports the plan lines that read a whole table or a whole index:
// "SCAN t" or "SCAN t USING [COVERING] INDEX i". Scans of a subquery or CTE
// result ("SCAN (subquery-1)", "SCAN CONSTANT ROW") read rows already bounded
// by their own plan lines and are not counted.
func planScans(plan []string, table string) []string {
	var scans []string
	for _, line := range plan {
		if line == "SCAN "+table || strings.HasPrefix(line, "SCAN "+table+" ") {
			scans = append(scans, line)
		}
	}
	return scans
}

// TestReadQueryPlansStayOffWholeTableScans pins the plans the 2026-10-09 audit
// moved off whole-table reads. The store never runs ANALYZE, so these are the
// default-statistics plans that production uses; an empty store plans exactly
// like a large one without sqlite_stat1.
func TestReadQueryPlansStayOffWholeTableScans(t *testing.T) {
	store := openRepositoryTestStore(t)
	db := store.db

	type planCase struct {
		name  string
		query string
		args  []any
		// want lists lines the plan must contain; the scanned tables must not
		// be read whole.
		want      []string
		notScan   []string
		wantExact bool
	}
	const (
		conv    = "conversation-a"
		account = "account-a"
	)
	search := func(term string, filter SearchQuery) (string, []any) {
		filter.Limit = max(filter.Limit, 1)
		return searchMessagesStatement(term, filter)
	}
	senderSQL, senderArgs := search("", SearchQuery{SenderCanonicalValue: "+15550000001"})
	senderWindowSQL, senderWindowArgs := search("x", SearchQuery{SenderCanonicalValue: "+15550000001", SinceMS: 1, UntilMS: 2})
	listSQL, listArgs := search("", SearchQuery{})
	accountWindowSQL, accountWindowArgs := search("", SearchQuery{AccountID: account, SinceMS: 1, UntilMS: 2})
	accountListSQL, accountListArgs := search("", SearchQuery{AccountID: account})
	listWindowSQL, listWindowArgs := search("", SearchQuery{SinceMS: 1, UntilMS: 2})
	conversationSQL, conversationArgs := search("x", SearchQuery{ConversationID: conv})

	cases := []planCase{
		{
			name:      "page before a keyset cursor",
			query:     messagesBeforeCursorQuery,
			args:      []any{conv, int64(10), "m", 50},
			want:      []string{"SEARCH messages USING INDEX messages_conversation_time_idx (conversation_id=? AND (occurred_at_ms,message_id)<(?,?))"},
			wantExact: true,
		},
		{
			name:      "page after a keyset cursor",
			query:     messagesAfterCursorQuery,
			args:      []any{conv, int64(10), "m", 50},
			want:      []string{"SEARCH messages USING INDEX messages_conversation_time_idx (conversation_id=? AND (occurred_at_ms,message_id)>(?,?))"},
			wantExact: true,
		},
		{
			name:    "search by sender seeks the sender index per identity",
			query:   senderSQL,
			args:    senderArgs,
			want:    []string{"SEARCH m USING INDEX messages_sender_time_idx (sender_identity_id=?)"},
			notScan: []string{"m", "messages"},
		},
		{
			name:    "search by sender inside a window",
			query:   senderWindowSQL,
			args:    senderWindowArgs,
			want:    []string{"SEARCH m USING INDEX messages_sender_time_idx (sender_identity_id=? AND occurred_at_ms>? AND occurred_at_ms<?)"},
			notScan: []string{"m", "messages"},
		},
		{
			name:  "newest messages across conversations read the time index from its end",
			query: listSQL,
			args:  listArgs,
			// A SCAN of the index in order, stopped by LIMIT: no sort, no table scan.
			want:      []string{"SCAN m USING INDEX messages_time_idx"},
			wantExact: true,
		},
		{
			name:      "newest messages inside a window",
			query:     listWindowSQL,
			args:      listWindowArgs,
			want:      []string{"SEARCH m USING INDEX messages_time_idx (occurred_at_ms>? AND occurred_at_ms<?)"},
			wantExact: true,
		},
		{
			name:    "account listing inside a window seeks each direction's window",
			query:   accountWindowSQL,
			args:    accountWindowArgs,
			want:    []string{"SEARCH m USING INDEX messages_account_direction_time_idx (account_id=? AND direction=? AND occurred_at_ms>? AND occurred_at_ms<?)"},
			notScan: []string{"m", "messages"},
		},
		{
			name:    "account listing stays inside the account's index range",
			query:   accountListSQL,
			args:    accountListArgs,
			want:    []string{"SEARCH m USING INDEX messages_account_direction_time_idx (account_id=? AND direction=?)"},
			notScan: []string{"m", "messages"},
		},
		{
			name:      "search inside one conversation",
			query:     conversationSQL,
			args:      conversationArgs,
			want:      []string{"SEARCH m USING INDEX messages_conversation_time_idx (conversation_id=?)"},
			wantExact: true,
		},
		{
			name:  "conversation roster starts from participants",
			query: conversationPeerIdentitiesQuery,
			args:  []any{account, conv},
			want: []string{
				"SEARCH p USING INDEX sqlite_autoindex_conversation_participants_1 (conversation_id=?)",
				"SEARCH i USING INDEX sqlite_autoindex_identities_3 (account_id=? AND identity_id=?)",
			},
			notScan: []string{"i", "p"},
		},
		{
			name:  "direct thread by sole peer starts from the peer's participant rows",
			query: directConversationBySolePeerQuery,
			args:  []any{account, "identity-a", account, "identity-a"},
			want: []string{
				"SEARCH c USING INDEX sqlite_autoindex_conversations_3 (account_id=? AND conversation_id=?)",
				"SEARCH p USING INDEX conversation_participants_identity_idx (identity_id=?)",
			},
			notScan: []string{"c", "p"},
		},
		{
			name:  "group candidates come from one member's participant rows",
			query: groupConversationsWithMemberQuery,
			args:  []any{account, "identity-a", account},
			want: []string{
				"SEARCH c USING INDEX sqlite_autoindex_conversations_3 (account_id=? AND conversation_id=?)",
				"SEARCH p USING INDEX conversation_participants_identity_idx (identity_id=?)",
			},
			notScan: []string{"c", "p"},
		},
		{
			name:  "cross-account recency list reads both arms of the recency index",
			query: conversationsByRecencyQuery,
			args:  []any{200, 200, 200},
			want: []string{
				"SEARCH conversations USING INDEX conversations_recency_idx (archived_at_ms=?)",
				"SEARCH conversations USING INDEX conversations_recency_idx (archived_at_ms>?)",
			},
			notScan: []string{"conversations"},
		},

		{
			name:    "message count stays inside one account's index range",
			query:   countMessagesQuery,
			args:    []any{account},
			want:    []string{"SEARCH messages USING COVERING INDEX messages_account_direction_time_idx (account_id=?)"},
			notScan: []string{"messages"},
		},
		{
			name:    "conversation count stays inside one account's index range",
			query:   countConversationsQuery,
			args:    []any{account},
			want:    []string{"SEARCH conversations USING COVERING INDEX sqlite_autoindex_conversations_3 (account_id=?)"},
			notScan: []string{"conversations"},
		},
		{
			name:      "outbox state for a rendered outgoing message",
			query:     latestOutboxStateQuery,
			args:      []any{account, "message-a"},
			want:      []string{"SEARCH outbox USING INDEX outbox_local_message_idx (account_id=? AND local_message_id=?)"},
			wantExact: true,
		},
		{
			name:    "read-cursor check seeks the message's conversation",
			query:   messageHasReadCursorQuery,
			args:    []any{"message-a", "message-a"},
			want:    []string{"SEARCH read_cursors USING INDEX read_cursors_conversation_idx (conversation_id=?)"},
			notScan: []string{"read_cursors", "messages"},
		},
	}
	t.Run("content duplicate check seeks the occurrence millisecond", func(t *testing.T) {
		plan := explainPlanLines(t, db, messageContentDuplicateQuery,
			account, conv, int64(10), "incoming", "body", nil, "remote-a")
		if scans := planScans(plan, "messages"); len(scans) > 0 || len(plan) == 0 ||
			!strings.HasPrefix(plan[0], "SEARCH messages USING INDEX ") ||
			!strings.Contains(plan[0], "occurred_at_ms=?") {
			t.Fatalf("plan = %q, want a SEARCH bounded by occurred_at_ms=?", plan)
		}
	})
	t.Run("newest message times seek both directions", func(t *testing.T) {
		plan := explainPlanLines(t, db, latestMessageTimesQuery, account, account)
		seek := "SEARCH messages USING COVERING INDEX messages_account_direction_time_idx (account_id=? AND direction=?)"
		seeks := 0
		for _, line := range plan {
			if line == seek {
				seeks++
			}
		}
		// One seek per direction. Dropping either subquery's direction term
		// keeps the MAX right but walks the account's whole range: still a
		// SEARCH, so count the seeks rather than look for a SCAN.
		if seeks != 2 || len(planScans(plan, "messages")) > 0 {
			t.Fatalf("plan = %q, want exactly two %q lines", plan, seek)
		}
	})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan := explainPlanLines(t, db, tc.query, tc.args...)
			for _, want := range tc.want {
				if !slices.Contains(plan, want) {
					t.Fatalf("plan = %q\nwant a line %q", plan, want)
				}
			}
			if tc.wantExact && len(plan) != len(tc.want) {
				t.Fatalf("plan = %q\nwant only %q (no scan, no sort)", plan, tc.want)
			}
			for _, table := range tc.notScan {
				if scans := planScans(plan, table); len(scans) > 0 {
					t.Fatalf("plan = %q\nreads %s whole: %q", plan, table, scans)
				}
			}
		})
	}
}

// TestUnscopedSubstringSearchKeepsSequentialScan pins the one deliberate scan:
// a substring search over every conversation reads messages once in rowid order
// and keeps the newest rows in a top-N sort. With messages_time_idx available
// the planner would otherwise walk that index and fetch each row out of order,
// which for a rare or absent term costs several times the scan (3.4 s against
// 0.45 s on a 2.2M-message store). Full-text search is the fix for this one.
func TestUnscopedSubstringSearchKeepsSequentialScan(t *testing.T) {
	store := openRepositoryTestStore(t)
	for _, filter := range []SearchQuery{
		{Limit: 30},
		{SinceMS: 1, UntilMS: 2, Limit: 30},
		{AccountID: "account-a", Limit: 30},
	} {
		query, args := searchMessagesStatement("term", filter)
		plan := explainPlanLines(t, store.db, query, args...)
		want := []string{"SCAN m", "USE TEMP B-TREE FOR ORDER BY"}
		if !slices.Equal(plan, want) {
			t.Fatalf("filter %+v: plan = %q, want %q", filter, plan, want)
		}
	}
}
