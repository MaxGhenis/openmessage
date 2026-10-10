package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// MessageDirection is the persisted direction of a normalized message.
type MessageDirection string

const (
	MessageDirectionIncoming MessageDirection = "incoming"
	MessageDirectionOutgoing MessageDirection = "outgoing"
)

// MessageState is the current normalized state of a message.
type MessageState string

const (
	MessageStateActive  MessageState = "active"
	MessageStateEdited  MessageState = "edited"
	MessageStateDeleted MessageState = "deleted"
)

// InboxRecord is one durable, undecoded transport frame. ReceivedAtMS and
// ProcessedAtMS are populated on reads; AppendInbox stamps ReceivedAtMS with
// the repository's injected clock and always stores ProcessedAtMS as NULL.
type InboxRecord struct {
	InboxID       string
	AccountID     string
	Generation    int64
	DedupeKey     string
	Codec         string
	CodecVersion  int64
	ReceivedAtMS  int64
	Payload       []byte
	ProcessedAtMS *int64
}

// Message is one normalized projected message. CreatedAtMS and UpdatedAtMS are
// populated on reads; ProjectMessage stamps them with the injected clock.
type Message struct {
	MessageID        string
	ConversationID   string
	AccountID        string
	RemoteMessageID  string
	SenderIdentityID *string
	Direction        MessageDirection
	Body             string
	ReplyToRemoteID  *string
	State            MessageState
	OccurredAtMS     int64
	CreatedAtMS      int64
	UpdatedAtMS      int64
}

// SearchQuery narrows a bounded message-body LIKE query. Zero values leave a
// field unconstrained, and a non-positive Limit uses the legacy default of 20.
// SenderCanonicalValue corresponds to db.SearchFilter.Phone at the read seam.
type SearchQuery struct {
	AccountID            string
	ConversationID       string
	SenderCanonicalValue string
	SinceMS              int64
	UntilMS              int64
	Limit                int
}

// MessageProjection describes a normalized message and its attachments.
// ProjectMessage requires InboxID to identify the durable frame from which the
// message was decoded; ImportMessage ignores InboxID for historical imports.
type MessageProjection struct {
	InboxID     string
	Message     Message
	Attachments []MessageAttachment
}

// MessageRepository owns durable inbox ingestion and normalized projection.
type MessageRepository struct {
	store *Store
	now   func() time.Time
	// betweenSearchStatements, when a test sets it, runs between the
	// statements a conversation or sender search composes its answer from.
	betweenSearchStatements func()
}

// NewMessageRepository creates an inbox/message repository. now is required so
// all storage-owned timestamps are deterministic in tests.
func NewMessageRepository(
	store *Store,
	now func() time.Time,
) (*MessageRepository, error) {
	if store == nil || store.db == nil {
		return nil, fmt.Errorf("create message repository: store is nil")
	}
	if now == nil {
		return nil, fmt.Errorf("create message repository: now function is nil")
	}
	return &MessageRepository{store: store, now: now}, nil
}

// AppendInbox commits a raw transport frame before decoding. A nonempty
// account-scoped dedupe key returns the original inbox ID without changing the
// stored frame.
func (r *MessageRepository) AppendInbox(
	ctx context.Context,
	record InboxRecord,
) (string, error) {
	receivedAtMS, err := r.nowMS("append inbox")
	if err != nil {
		return "", err
	}

	result, err := r.store.db.ExecContext(ctx, `
		INSERT INTO inbox (
			inbox_id,
			account_id,
			generation,
			dedupe_key,
			codec,
			codec_version,
			received_at_ms,
			payload
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(account_id, dedupe_key) WHERE dedupe_key <> '' DO NOTHING
	`,
		record.InboxID,
		record.AccountID,
		record.Generation,
		record.DedupeKey,
		record.Codec,
		record.CodecVersion,
		receivedAtMS,
		record.Payload,
	)
	if err != nil {
		return "", invalidInboxConstraintError(err, record.InboxID)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return "", fmt.Errorf("append inbox %q: read rows affected: %w", record.InboxID, err)
	}
	if affected == 1 {
		return record.InboxID, nil
	}
	if affected != 0 || record.DedupeKey == "" {
		return "", fmt.Errorf(
			"append inbox %q: unexpected rows affected: %d",
			record.InboxID,
			affected,
		)
	}

	var existingID string
	if err := r.store.db.QueryRowContext(ctx, `
		SELECT inbox_id
		FROM inbox
		WHERE account_id = ? AND dedupe_key = ?
	`, record.AccountID, record.DedupeKey).Scan(&existingID); err != nil {
		return "", fmt.Errorf(
			"append inbox %q: read existing deduplicated row: %w",
			record.InboxID,
			err,
		)
	}
	return existingID, nil
}

// Unprocessed returns durable frames in receipt order. Multiple workers may
// observe the same row; ProjectMessage provides the idempotent serialization
// boundary.
func (r *MessageRepository) Unprocessed(ctx context.Context) ([]InboxRecord, error) {
	rows, err := r.store.db.QueryContext(ctx, `
		SELECT `+inboxColumns+`
		FROM inbox
		WHERE processed_at_ms IS NULL
		ORDER BY received_at_ms, inbox_id
	`)
	if err != nil {
		return nil, fmt.Errorf("list unprocessed inbox records: %w", err)
	}
	records, err := collectRows(rows, scanInboxRecord)
	if err != nil {
		return nil, fmt.Errorf("list unprocessed inbox records: %w", err)
	}
	return records, nil
}

// MarkInboxProcessed removes a durable frame from the worker's unprocessed
// queue without deleting its payload. Repeated calls, including calls for a
// row that is already processed or does not exist for the account, succeed.
func (r *MessageRepository) MarkInboxProcessed(
	ctx context.Context,
	inboxID string,
	accountID string,
) error {
	nowMS, err := r.nowMS("mark inbox processed")
	if err != nil {
		return err
	}
	return markInboxProcessed(ctx, r.store.db, inboxID, accountID, nowMS)
}

type inboxProcessExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func markInboxProcessed(
	ctx context.Context,
	execer inboxProcessExecer,
	inboxID string,
	accountID string,
	processedAtMS int64,
) error {
	_, err := execer.ExecContext(ctx, `
		UPDATE inbox
		SET processed_at_ms = ?
		WHERE inbox_id = ? AND account_id = ? AND processed_at_ms IS NULL
	`, processedAtMS, inboxID, accountID)
	if err == nil {
		return nil
	}
	if isSQLiteConstraint(err) {
		return fmt.Errorf(
			"mark inbox %q for account %q processed: %w: %w",
			inboxID,
			accountID,
			ErrInvalidInboxRecord,
			mapConstraintError(err),
		)
	}
	return fmt.Errorf(
		"mark inbox %q for account %q processed: %w",
		inboxID,
		accountID,
		err,
	)
}

// GetMessage returns the normalized message with messageID.
func (r *MessageRepository) GetMessage(
	ctx context.Context,
	messageID string,
) (Message, error) {
	message, err := scanMessage(r.store.db.QueryRowContext(ctx, `
		SELECT `+messageColumns+`
		FROM messages
		WHERE message_id = ?
	`, messageID))
	if errors.Is(err, sql.ErrNoRows) {
		return Message{}, notFound("message", messageID)
	}
	if err != nil {
		return Message{}, fmt.Errorf("get message %q: %w", messageID, err)
	}
	return message, nil
}

// GetMessageByRemote returns the normalized message selected by the same
// account-scoped natural key used by ProjectMessage. This is useful to recover
// the effective local message ID after a projection collides with a row that
// was already stored for the same remote message.
func (r *MessageRepository) GetMessageByRemote(
	ctx context.Context,
	accountID string,
	conversationID string,
	remoteMessageID string,
) (Message, error) {
	message, err := scanMessage(r.store.db.QueryRowContext(ctx, `
		SELECT `+messageColumns+`
		FROM messages
		WHERE account_id = ?
		  AND conversation_id = ?
		  AND remote_message_id = ?
	`, accountID, conversationID, remoteMessageID))
	if errors.Is(err, sql.ErrNoRows) {
		return Message{}, fmt.Errorf(
			"message for account %q, conversation %q, and remote ID %q: %w",
			accountID,
			conversationID,
			remoteMessageID,
			ErrNotFound,
		)
	}
	if err != nil {
		return Message{}, fmt.Errorf(
			"get message for account %q, conversation %q, and remote ID %q: %w",
			accountID,
			conversationID,
			remoteMessageID,
			err,
		)
	}
	return message, nil
}

// Keyset pages compare (occurred_at_ms, message_id) as one row value, which
// SQLite turns into a range on messages_conversation_time_idx and so reads only
// the page. The equivalent OR form,
// occurred_at_ms < ? OR (occurred_at_ms = ? AND message_id < ?), bounds only
// conversation_id: each page then walks every row between the conversation's
// newest message and the cursor, and walking a whole thread is quadratic.
// Both columns are NOT NULL, so the two forms select the same rows.
const (
	messagesBeforeCursorQuery = `
		SELECT ` + messageColumns + `
		FROM messages
		WHERE conversation_id = ?
		  AND (occurred_at_ms, message_id) < (?, ?)
		ORDER BY occurred_at_ms DESC, message_id DESC
		LIMIT ?
	`
	messagesAfterCursorQuery = `
		SELECT ` + messageColumns + `
		FROM messages
		WHERE conversation_id = ?
		  AND (occurred_at_ms, message_id) > (?, ?)
		ORDER BY occurred_at_ms ASC, message_id ASC
		LIMIT ?
	`
)

// ListMessagesByConversation returns a newest-first page. beforeMS == 0
// selects the latest page; otherwise beforeID is the deterministic tie cursor.
func (r *MessageRepository) ListMessagesByConversation(
	ctx context.Context,
	conversationID string,
	beforeMS int64,
	beforeID string,
	limit int,
) ([]Message, error) {
	if limit <= 0 {
		return []Message{}, nil
	}
	query := `
		SELECT ` + messageColumns + `
		FROM messages
		WHERE conversation_id = ?
		ORDER BY occurred_at_ms DESC, message_id DESC
		LIMIT ?
	`
	args := []any{conversationID, limit}
	if beforeMS > 0 {
		query = `
			SELECT ` + messageColumns + `
			FROM messages
			WHERE conversation_id = ? AND occurred_at_ms < ?
			ORDER BY occurred_at_ms DESC, message_id DESC
			LIMIT ?
		`
		args = []any{conversationID, beforeMS, limit}
		if beforeID != "" {
			query = messagesBeforeCursorQuery
			args = []any{conversationID, beforeMS, beforeID, limit}
		}
	}
	rows, err := r.store.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list messages for conversation %q: %w", conversationID, err)
	}
	messages, err := collectRows(rows, scanMessage)
	if err != nil {
		return nil, fmt.Errorf("list messages for conversation %q: %w", conversationID, err)
	}
	return messages, nil
}

// ListMessagesByConversationAfter returns an oldest-first page after the
// supplied timestamp and optional deterministic tie cursor.
func (r *MessageRepository) ListMessagesByConversationAfter(
	ctx context.Context,
	conversationID string,
	afterMS int64,
	afterID string,
	limit int,
) ([]Message, error) {
	if limit <= 0 {
		return []Message{}, nil
	}
	query := `
		SELECT ` + messageColumns + `
		FROM messages
		WHERE conversation_id = ? AND occurred_at_ms > ?
		ORDER BY occurred_at_ms ASC, message_id ASC
		LIMIT ?
	`
	args := []any{conversationID, afterMS, limit}
	if afterID != "" {
		query = messagesAfterCursorQuery
		args = []any{conversationID, afterMS, afterID, limit}
	}
	rows, err := r.store.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list messages after cursor for conversation %q: %w", conversationID, err)
	}
	messages, err := collectRows(rows, scanMessage)
	if err != nil {
		return nil, fmt.Errorf("list messages after cursor for conversation %q: %w", conversationID, err)
	}
	return messages, nil
}

// ListMessagesAroundMessage returns the anchor and its neighboring messages in
// chronological order. Missing anchors and cross-conversation anchors wrap
// ErrNotFound.
func (r *MessageRepository) ListMessagesAroundMessage(
	ctx context.Context,
	conversationID string,
	messageID string,
	before int,
	after int,
) ([]Message, error) {
	if before < 0 {
		before = 0
	}
	if after < 0 {
		after = 0
	}
	if before == 0 {
		before = 40
	}
	if after == 0 {
		after = 40
	}

	anchor, err := r.GetMessage(ctx, messageID)
	if err != nil {
		return nil, err
	}
	if anchor.ConversationID != conversationID {
		return nil, fmt.Errorf("message %q in conversation %q: %w", messageID, conversationID, ErrNotFound)
	}

	beforeRows, err := r.store.db.QueryContext(
		ctx, messagesBeforeCursorQuery,
		conversationID, anchor.OccurredAtMS, anchor.MessageID, before,
	)
	if err != nil {
		return nil, fmt.Errorf("list messages before %q: %w", messageID, err)
	}
	beforeMessages, err := collectRows(beforeRows, scanMessage)
	if err != nil {
		return nil, fmt.Errorf("list messages before %q: %w", messageID, err)
	}

	afterRows, err := r.store.db.QueryContext(
		ctx, messagesAfterCursorQuery,
		conversationID, anchor.OccurredAtMS, anchor.MessageID, after,
	)
	if err != nil {
		return nil, fmt.Errorf("list messages after %q: %w", messageID, err)
	}
	afterMessages, err := collectRows(afterRows, scanMessage)
	if err != nil {
		return nil, fmt.Errorf("list messages after %q: %w", messageID, err)
	}

	result := make([]Message, 0, len(beforeMessages)+1+len(afterMessages))
	for i := len(beforeMessages) - 1; i >= 0; i-- {
		result = append(result, beforeMessages[i])
	}
	result = append(result, anchor)
	result = append(result, afterMessages...)
	return result, nil
}

// SearchMessages returns the messages whose body matches LIKE '%' || query ||
// '%' (R5 substring semantics: ASCII case-insensitive, with '%' and '_' in
// query acting as wildcards), newest first with message ID as a deterministic
// tie-breaker. Relevance ranking is intentionally not offered.
//
// A search narrowed by conversation or sender (indexBoundedSearch) reads that
// scope's index range with the LIKE, or, when the query has a literal run of
// three characters and the LIKE would read many rows of a long thread or a
// prolific sender, the trigram index's candidates in that scope
// (searchScope). Any other nonempty query first reads the newest messages,
// within the date range if any, stopping once it has limit matches
// (searchRecentMessages). That answers it whenever those messages hold limit
// matches, which covers common terms that the trigram index would have to
// visit in full, or are the whole range. Otherwise the whole scope is
// searched: from the trigram index when the query has a literal run of three
// characters, by the LIKE alone when it has none. Every path applies the same
// LIKE and returns the same rows (substring_search.go).
func (r *MessageRepository) SearchMessages(
	ctx context.Context,
	query string,
	filter SearchQuery,
) ([]Message, error) {
	messages, _, err := r.searchMessages(ctx, query, filter, defaultSearchPlan)
	return messages, err
}

// recentSearchWindow is how many of the newest messages SearchMessages reads
// before searching every conversation. On copies of the live store, reading
// them cost about 0.5 to 1 us a message, stopping at limit matches, and the
// trigram index cost about 2 to 3 us a candidate: 2,000 messages cost about as
// much as one lookup of a medium-frequency term (1-2 ms), and a term in more
// than limit/2,000 of them (1.5% for the UI's 30) is answered there without
// visiting its other matches.
const recentSearchWindow = 2000

// searchPath names the path that answered a search, for tests.
type searchPath string

const (
	searchPathRecentWindow searchPath = "recent window"
	searchPathTrigramIndex searchPath = "trigram index"
	searchPathLike         searchPath = "like"
)

// searchMessages is SearchMessages with its plan as a parameter, so tests can
// reach every path on small stores, and reports the path that answered.
func (r *MessageRepository) searchMessages(
	ctx context.Context,
	query string,
	filter SearchQuery,
	plan searchPlan,
) ([]Message, searchPath, error) {
	if filter.Limit <= 0 {
		filter.Limit = 20
	}
	if filter.SinceMS > 0 && filter.UntilMS > 0 && filter.UntilMS < filter.SinceMS {
		filter.SinceMS, filter.UntilMS = filter.UntilMS, filter.SinceMS
	}

	if query != "" && indexBoundedSearch(filter) {
		if expression, ok := trigramMatchQuery(query); ok {
			return r.searchScope(ctx, query, expression, filter, plan)
		}
	}
	if query != "" && !indexBoundedSearch(filter) {
		messages, complete, err := r.searchRecentMessages(ctx, query, filter, plan.window)
		if err != nil {
			return nil, "", err
		}
		if complete {
			return messages, searchPathRecentWindow, nil
		}
	}
	path := searchPathLike
	if usesTrigramIndex(query, filter) {
		path = searchPathTrigramIndex
	}
	statement, args := searchMessagesStatement(query, filter)
	messages, err := r.queryMessages(ctx, statement, args)
	return messages, path, err
}

func (r *MessageRepository) queryMessages(ctx context.Context, statement string, args []any) ([]Message, error) {
	return querySearchMessages(ctx, r.store.db, statement, args)
}

func querySearchMessages(ctx context.Context, q searchQuerier, statement string, args []any) ([]Message, error) {
	rows, err := q.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, fmt.Errorf("search messages: %w", err)
	}
	messages, err := collectRows(rows, scanMessage)
	if err != nil {
		return nil, fmt.Errorf("search messages: %w", err)
	}
	return messages, nil
}

// searchRecentMessages applies the search to a window: the newest window
// messages, within the date range if any, read from messages_time_idx. The
// other filters and the LIKE then apply to the window's rows, read newest
// first until limit match. The window lists the range in result order, so the
// scope's rows within it are the scope's newest rows. complete reports that
// the result is the whole answer: either it holds limit matches, which are then
// the scope's newest limit matches; or the range held fewer than window rows,
// so the window was all of it.
func (r *MessageRepository) searchRecentMessages(
	ctx context.Context,
	query string,
	filter SearchQuery,
	window int,
) (messages []Message, complete bool, err error) {
	statement, args := recentSearchMessagesStatement(query, filter, window)
	messages, err = r.queryMessages(ctx, statement, args)
	if err != nil {
		return nil, false, fmt.Errorf("recent window: %w", err)
	}
	if len(messages) >= filter.Limit {
		return messages, true, nil
	}
	statement, args = recentWindowCountStatement(filter, window)
	var inRange int
	if err := r.store.db.QueryRowContext(ctx, statement, args...).Scan(&inRange); err != nil {
		return nil, false, fmt.Errorf("count recent search window: %w", err)
	}
	return messages, inRange < window, nil
}

// recentSearchMessagesStatement reads the window (recentWindowRange) and
// applies the other filters and the LIKE to its rows. SearchMessages sends
// conversation and sender searches to their own indexes instead, but the
// window applies those filters too, so it is exact for every filter.
func recentSearchMessagesStatement(query string, filter SearchQuery, window int) (string, []any) {
	from, conditions, args := recentWindowRange(filter)
	args = append(args, window)
	var outer []string
	if filter.AccountID != "" {
		outer = append(outer, "account_id = ?")
		args = append(args, filter.AccountID)
	}
	if filter.ConversationID != "" {
		outer = append(outer, "conversation_id = ?")
		args = append(args, filter.ConversationID)
	}
	if filter.SenderCanonicalValue != "" {
		outer = append(outer, "sender_identity_id IN ("+senderIdentityIDsQuery+")")
		args = append(args, filter.SenderCanonicalValue)
	}
	outer = append(outer, "body LIKE '%' || ? || '%'")
	args = append(args, query, filter.Limit)
	statement := `
		SELECT ` + messageColumns + `
		FROM (
			SELECT ` + prefixedMessageColumns("m") + `
			FROM ` + from + `
			` + whereClause(conditions) + `
			ORDER BY m.occurred_at_ms DESC, m.message_id DESC
			LIMIT ?
		)
		` + whereClause(outer) + `
		ORDER BY occurred_at_ms DESC, message_id DESC
		LIMIT ?
	`
	return statement, args
}

// recentWindowCountStatement counts the rows of the window's range, up to
// upTo, by walking the range's index.
func recentWindowCountStatement(filter SearchQuery, upTo int) (string, []any) {
	from, conditions, args := recentWindowRange(filter)
	args = append(args, upTo)
	statement := `
		SELECT count(*) FROM (
			SELECT 1
			FROM ` + from + `
			` + whereClause(conditions) + `
			LIMIT ?
		)
	`
	return statement, args
}

// recentWindowRange names the index range a window reads: messages_time_idx,
// bounded by the date range. Every condition is a bound on that index, so the
// window never reads more than window index entries. The other filters stay
// outside: inside, the planner would have to read rows until window of them
// matched, all of a small account's history and beyond.
func recentWindowRange(filter SearchQuery) (from string, conditions []string, args []any) {
	from = "messages AS m INDEXED BY messages_time_idx"
	if filter.SinceMS > 0 {
		conditions = append(conditions, "m.occurred_at_ms >= ?")
		args = append(args, filter.SinceMS)
	}
	if filter.UntilMS > 0 {
		conditions = append(conditions, "m.occurred_at_ms <= ?")
		args = append(args, filter.UntilMS)
	}
	return from, conditions, args
}

func whereClause(conditions []string) string {
	if len(conditions) == 0 {
		return ""
	}
	return "WHERE " + strings.Join(conditions, " AND ")
}

// indexBoundedSearch reports a search narrowed by conversation or sender. Its
// LIKE reads only the messages of that conversation's or sender's index range,
// and stops at limit matches when the range is in recency order (a
// conversation's is). The trigram index, which returns matches from every
// conversation, loses to that bound for a term common elsewhere but rare in
// this scope (one lookup of a word in 28% of messages took 45 ms on the live
// store, whose busiest thread the LIKE reads in 8 ms), and wins only when the
// scope is long and the term rare; searchScope chooses between them by
// counting both.
func indexBoundedSearch(filter SearchQuery) bool {
	return filter.ConversationID != "" || filter.SenderCanonicalValue != ""
}

// searchMessagesStatement builds the SQL that searches a filter's whole
// scope, for an already normalized filter. A query the trigram index can
// narrow reads its candidates from messages_fts
// (trigramSearchMessagesStatement) unless a conversation or sender index
// bounds the search (indexBoundedSearch); any other query, and the empty one,
// uses likeSearchMessagesStatement. Both select the rows whose body matches the
// LIKE, in the same order.
func searchMessagesStatement(query string, filter SearchQuery) (string, []any) {
	if usesTrigramIndex(query, filter) {
		return trigramSearchMessagesStatement(query, filter)
	}
	return likeSearchMessagesStatement(query, filter)
}

func usesTrigramIndex(query string, filter SearchQuery) bool {
	return likePatternUsesTrigrams(query) && !indexBoundedSearch(filter)
}

// trigramSearchMessagesStatement puts the substring LIKE on messages_fts.body,
// which SQLite hands to the trigram index: the index returns the rowids of the
// messages holding every trigram of the query's literal runs, SQLite applies
// the LIKE to each of those bodies, and the filters and recency order apply to
// the joined messages rows.
//
// CROSS JOIN keeps messages_fts the outer loop. Given a filter on messages
// (an account), the planner otherwise may read those messages first and probe
// the index once per row, running the whole trigram query for each: 7 to 34
// seconds for one account's 72k messages, against 0.2 to 4 ms this way.
func trigramSearchMessagesStatement(query string, filter SearchQuery) (string, []any) {
	conditions, args := searchMessageFilters(filter)
	conditions = append([]string{"f.body LIKE '%' || ? || '%'"}, conditions...)
	args = append([]any{query}, args...)
	args = append(args, filter.Limit)
	statement := `
		SELECT ` + prefixedMessageColumns("m") + `
		FROM messages_fts AS f
		CROSS JOIN messages AS m ON m.rowid = f.rowid
		WHERE ` + strings.Join(conditions, " AND ") + `
		ORDER BY m.occurred_at_ms DESC, m.message_id DESC
		LIMIT ?
	`
	return statement, args
}

// likeSearchMessagesStatement matches the LIKE against messages directly. It
// serves the empty query (a listing), queries without a literal run of three
// characters, which the trigram index cannot narrow, and searches a
// conversation or sender index bounds (indexBoundedSearch).
func likeSearchMessagesStatement(query string, filter SearchQuery) (string, []any) {
	conditions, args := searchMessageFilters(filter)
	conditions = append([]string{"m.body LIKE '%' || ? || '%'"}, conditions...)
	args = append([]any{query}, args...)
	args = append(args, filter.Limit)

	// A listing with no substring walks messages_time_idx from the newest end
	// (of the window, if any) and stops after limit rows; filtered by account
	// it reads that account's rows inside the window. A substring search that
	// no conversation or sender bounds must not use an index: for a rare or
	// absent term the planner would walk messages_time_idx and fetch each row
	// out of rowid order, several times slower than one sequential scan. Pin
	// that scan (with its top-N sort) for the short queries the trigram index
	// cannot serve.
	from := "messages AS m"
	if query != "" && filter.ConversationID == "" && filter.SenderCanonicalValue == "" {
		from = "messages AS m NOT INDEXED"
	}
	statement := `
		SELECT ` + prefixedMessageColumns("m") + `
		FROM ` + from + `
		WHERE ` + strings.Join(conditions, " AND ") + `
		ORDER BY m.occurred_at_ms DESC, m.message_id DESC
		LIMIT ?
	`
	return statement, args
}

// searchMessageFilters returns SearchMessages' filter conditions on messages
// AS m, with their arguments in order.
//
// The sender filter selects the sender's identity IDs in a subquery so the
// planner can seek messages_sender_time_idx per identity. Joining identities
// and filtering on i.canonical_value instead walked that whole index, every
// attributed message, because no identities index leads with canonical_value.
// The subquery needs no account_id match: the composite foreign key
// messages(account_id, sender_identity_id) -> identities(account_id,
// identity_id) already makes a sender identity belong to the message's account.
func searchMessageFilters(filter SearchQuery) ([]string, []any) {
	var conditions []string
	var args []any
	if filter.AccountID != "" {
		// direction's CHECK allows exactly these two values, so the IN changes
		// no result; it lets messages_account_direction_time_idx bound a date
		// window per direction instead of reading the account's every row.
		conditions = append(conditions, "m.account_id = ? AND m.direction IN ('incoming', 'outgoing')")
		args = append(args, filter.AccountID)
	}
	if filter.ConversationID != "" {
		conditions = append(conditions, "m.conversation_id = ?")
		args = append(args, filter.ConversationID)
	}
	if filter.SenderCanonicalValue != "" {
		conditions = append(conditions, "m.sender_identity_id IN ("+senderIdentityIDsQuery+")")
		args = append(args, filter.SenderCanonicalValue)
	}
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

// senderIdentityIDsQuery selects the identities holding a canonical address,
// for the sender filter.
const senderIdentityIDsQuery = `SELECT identity_id FROM identities WHERE canonical_value = ?`

// ImportMessage atomically upserts a historical normalized message by remote
// ID and records its attachments without requiring or modifying an inbox row.
func (r *MessageRepository) ImportMessage(
	ctx context.Context,
	projection MessageProjection,
) error {
	message := projection.Message
	tx, err := r.store.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("import message %q: begin transaction: %w", message.MessageID, err)
	}
	defer tx.Rollback()

	nowMS, err := r.nowMS("import message")
	if err != nil {
		return err
	}
	if err := r.upsertMessage(ctx, tx, message, nowMS); err != nil {
		return err
	}
	attachmentMessageID := message.MessageID
	if len(projection.Attachments) > 0 {
		// upsertMessage preserves the first local message_id on a natural-key
		// conflict. Resolve that effective parent inside this transaction so a
		// repeated import cannot point attachments at a discarded ID.
		if err := tx.QueryRowContext(ctx, `
			SELECT message_id
			FROM messages
			WHERE account_id = ?
			  AND conversation_id = ?
			  AND remote_message_id = ?
		`, message.AccountID, message.ConversationID, message.RemoteMessageID).Scan(
			&attachmentMessageID,
		); err != nil {
			return fmt.Errorf(
				"import message %q: resolve attachment parent: %w",
				message.MessageID,
				err,
			)
		}
	}
	attachmentRepository := &MessageAttachmentRepository{store: r.store, now: r.now}
	for _, attachment := range projection.Attachments {
		// Historical bridge attachments do not carry a trusted v2 message ID.
		// Bind every row to the parent selected by this import.
		attachment.MessageID = attachmentMessageID
		if err := attachmentRepository.RecordInboundAttachment(ctx, tx, attachment); err != nil {
			return fmt.Errorf(
				"import message %q: record attachment ordinal %d: %w",
				message.MessageID,
				attachment.Ordinal,
				err,
			)
		}
	}

	if err := tx.Commit(); err != nil {
		if isSQLiteConstraint(err) {
			return invalidMessageConstraintError(nil, err, "commit message import")
		}
		return fmt.Errorf("import message %q: commit: %w", message.MessageID, err)
	}
	return nil
}

// InsertHistoricalMessage inserts a message recovered from fetched history,
// with its attachments, only when no row already holds its primary key or its
// (account, conversation, remote message) natural key. It never updates an
// existing row: ProjectMessage and ImportMessage overwrite body, state,
// occurrence time, sender and reply target unconditionally, which would let a
// fetched copy resurrect a deleted row, revert an edit, or regress newer
// content. inserted reports whether this call created the row; attachments are
// recorded only then. Like ImportMessage it does not touch the inbox.
func (r *MessageRepository) InsertHistoricalMessage(
	ctx context.Context,
	projection MessageProjection,
) (inserted bool, err error) {
	message := projection.Message
	tx, err := r.store.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("insert historical message %q: begin transaction: %w", message.MessageID, err)
	}
	defer tx.Rollback()

	nowMS, err := r.nowMS("insert historical message")
	if err != nil {
		return false, err
	}
	result, err := tx.ExecContext(ctx, `
		INSERT INTO messages (
			message_id,
			conversation_id,
			account_id,
			remote_message_id,
			sender_identity_id,
			direction,
			body,
			reply_to_remote_id,
			state,
			occurred_at_ms,
			created_at_ms,
			updated_at_ms
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT DO NOTHING
	`,
		message.MessageID,
		message.ConversationID,
		message.AccountID,
		message.RemoteMessageID,
		message.SenderIdentityID,
		message.Direction,
		message.Body,
		message.ReplyToRemoteID,
		message.State,
		message.OccurredAtMS,
		nowMS,
		nowMS,
	)
	if err != nil {
		return false, r.mapMessageWriteError(ctx, tx, message, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("insert historical message %q: read rows affected: %w", message.MessageID, err)
	}
	if affected == 0 {
		return false, nil
	}
	attachmentRepository := &MessageAttachmentRepository{store: r.store, now: r.now}
	for _, attachment := range projection.Attachments {
		attachment.MessageID = message.MessageID
		if err := attachmentRepository.RecordInboundAttachment(ctx, tx, attachment); err != nil {
			return false, fmt.Errorf(
				"insert historical message %q: record attachment ordinal %d: %w",
				message.MessageID,
				attachment.Ordinal,
				err,
			)
		}
	}
	if err := tx.Commit(); err != nil {
		if isSQLiteConstraint(err) {
			return false, invalidMessageConstraintError(nil, err, "commit historical message insert")
		}
		return false, fmt.Errorf("insert historical message %q: commit: %w", message.MessageID, err)
	}
	return true, nil
}

// ProjectMessage atomically upserts a normalized message by remote ID and
// marks its source inbox row processed. A replay of an already-processed inbox
// row returns without writing either timestamp.
func (r *MessageRepository) ProjectMessage(
	ctx context.Context,
	projection MessageProjection,
) error {
	tx, err := r.store.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("project message %q: begin transaction: %w", projection.Message.MessageID, err)
	}
	defer tx.Rollback()

	var (
		inboxAccountID string
		receivedAtMS   int64
		processedAtMS  sql.NullInt64
	)
	if err := tx.QueryRowContext(ctx, `
		SELECT account_id, received_at_ms, processed_at_ms
		FROM inbox
		WHERE inbox_id = ?
	`, projection.InboxID).Scan(
		&inboxAccountID,
		&receivedAtMS,
		&processedAtMS,
	); errors.Is(err, sql.ErrNoRows) {
		return invalidMessageError(
			orphanMessageError(ErrOrphanMessageInbox),
			"source inbox %q does not exist",
			projection.InboxID,
		)
	} else if err != nil {
		return fmt.Errorf(
			"project message %q: read inbox %q: %w",
			projection.Message.MessageID,
			projection.InboxID,
			err,
		)
	}
	if inboxAccountID != projection.Message.AccountID {
		return invalidMessageError(
			ErrCrossAccountMessage,
			"source inbox %q account %q does not match message account %q",
			projection.InboxID,
			inboxAccountID,
			projection.Message.AccountID,
		)
	}
	message := projection.Message
	if processedAtMS.Valid {
		existing, err := scanMessage(tx.QueryRowContext(ctx, `
			SELECT `+messageColumns+`
			FROM messages
			WHERE account_id = ?
			  AND conversation_id = ?
			  AND remote_message_id = ?
		`,
			message.AccountID,
			message.ConversationID,
			message.RemoteMessageID,
		))
		naturalKeyExists := true
		if errors.Is(err, sql.ErrNoRows) {
			naturalKeyExists = false
		} else if err != nil {
			return fmt.Errorf(
				"project message %q: read processed inbox natural key: %w",
				message.MessageID,
				err,
			)
		}
		// Execute the same SQLite write inside the transaction that will be
		// rolled back. This keeps SQLite authoritative for orphan/cross-account
		// validation even when the source inbox has already been processed.
		if err := r.upsertMessage(ctx, tx, message, processedAtMS.Int64); err != nil {
			return err
		}
		if !naturalKeyExists || !sameProjectedMessage(existing, message) {
			return fmt.Errorf(
				"%w: %w: processed inbox %q does not match message natural key/content (%q, %q, %q)",
				ErrInvalidMessage,
				ErrInboxProjectionConflict,
				projection.InboxID,
				message.AccountID,
				message.ConversationID,
				message.RemoteMessageID,
			)
		}
		return nil
	}

	nowMS, err := r.nowMS("project message")
	if err != nil {
		return err
	}
	if err := r.upsertMessage(ctx, tx, message, nowMS); err != nil {
		return err
	}
	attachmentMessageID := message.MessageID
	if len(projection.Attachments) > 0 {
		// upsertMessage preserves the first local message_id on a natural-key
		// conflict. Resolve that effective parent inside this transaction so a
		// duplicate inbound frame cannot point attachments at a discarded ID.
		if err := tx.QueryRowContext(ctx, `
			SELECT message_id
			FROM messages
			WHERE account_id = ?
			  AND conversation_id = ?
			  AND remote_message_id = ?
		`, message.AccountID, message.ConversationID, message.RemoteMessageID).Scan(
			&attachmentMessageID,
		); err != nil {
			return fmt.Errorf(
				"project message %q: resolve attachment parent: %w",
				message.MessageID,
				err,
			)
		}
	}
	attachmentRepository := &MessageAttachmentRepository{store: r.store, now: r.now}
	for _, attachment := range projection.Attachments {
		// Normalized bridge attachments do not carry a local message ID. Bind
		// every row to the parent selected by this projection rather than
		// trusting caller-supplied attachment ownership.
		attachment.MessageID = attachmentMessageID
		if err := attachmentRepository.RecordInboundAttachment(ctx, tx, attachment); err != nil {
			return fmt.Errorf(
				"project message %q: record attachment ordinal %d: %w",
				message.MessageID,
				attachment.Ordinal,
				err,
			)
		}
	}

	result, err := tx.ExecContext(ctx, `
		UPDATE inbox
		SET processed_at_ms = ?
		WHERE inbox_id = ? AND account_id = ? AND processed_at_ms IS NULL
	`, nowMS, projection.InboxID, message.AccountID)
	if err != nil {
		if isSQLiteConstraint(err) {
			return invalidMessageConstraintError(
				nil,
				err,
				"mark source inbox %q processed at %d after receipt at %d",
				projection.InboxID,
				nowMS,
				receivedAtMS,
			)
		}
		return fmt.Errorf(
			"project message %q: mark inbox %q processed: %w",
			message.MessageID,
			projection.InboxID,
			err,
		)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf(
			"project message %q: read processed inbox rows affected: %w",
			message.MessageID,
			err,
		)
	}
	if affected != 1 {
		return fmt.Errorf(
			"project message %q: mark inbox %q processed: affected %d rows, want 1",
			message.MessageID,
			projection.InboxID,
			affected,
		)
	}

	if err := tx.Commit(); err != nil {
		if isSQLiteConstraint(err) {
			return invalidMessageConstraintError(nil, err, "commit message projection")
		}
		return fmt.Errorf("project message %q: commit: %w", message.MessageID, err)
	}
	return nil
}

func (r *MessageRepository) upsertMessage(
	ctx context.Context,
	tx *sql.Tx,
	message Message,
	nowMS int64,
) error {
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO messages (
			message_id,
			conversation_id,
			account_id,
			remote_message_id,
			sender_identity_id,
			direction,
			body,
			reply_to_remote_id,
			state,
			occurred_at_ms,
			created_at_ms,
			updated_at_ms
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(account_id, conversation_id, remote_message_id) DO UPDATE SET
			sender_identity_id = excluded.sender_identity_id,
			direction = excluded.direction,
			body = excluded.body,
			reply_to_remote_id = excluded.reply_to_remote_id,
			state = excluded.state,
			occurred_at_ms = excluded.occurred_at_ms,
			updated_at_ms = excluded.updated_at_ms
		WHERE messages.sender_identity_id IS NOT excluded.sender_identity_id
		   OR messages.direction IS NOT excluded.direction
		   OR messages.body IS NOT excluded.body
		   OR messages.reply_to_remote_id IS NOT excluded.reply_to_remote_id
		   OR messages.state IS NOT excluded.state
		   OR messages.occurred_at_ms IS NOT excluded.occurred_at_ms
	`,
		message.MessageID,
		message.ConversationID,
		message.AccountID,
		message.RemoteMessageID,
		message.SenderIdentityID,
		message.Direction,
		message.Body,
		message.ReplyToRemoteID,
		message.State,
		message.OccurredAtMS,
		nowMS,
		nowMS,
	); err != nil {
		return r.mapMessageWriteError(ctx, tx, message, err)
	}
	return nil
}

const inboxColumns = `
	inbox_id,
	account_id,
	generation,
	dedupe_key,
	codec,
	codec_version,
	received_at_ms,
	payload,
	processed_at_ms`

func scanInboxRecord(row rowScanner) (InboxRecord, error) {
	var record InboxRecord
	err := row.Scan(
		&record.InboxID,
		&record.AccountID,
		&record.Generation,
		&record.DedupeKey,
		&record.Codec,
		&record.CodecVersion,
		&record.ReceivedAtMS,
		&record.Payload,
		&record.ProcessedAtMS,
	)
	return record, err
}

const messageColumns = `
	message_id,
	conversation_id,
	account_id,
	remote_message_id,
	sender_identity_id,
	direction,
	body,
	reply_to_remote_id,
	state,
	occurred_at_ms,
	created_at_ms,
	updated_at_ms`

func prefixedMessageColumns(alias string) string {
	return alias + `.message_id,
	` + alias + `.conversation_id,
	` + alias + `.account_id,
	` + alias + `.remote_message_id,
	` + alias + `.sender_identity_id,
	` + alias + `.direction,
	` + alias + `.body,
	` + alias + `.reply_to_remote_id,
	` + alias + `.state,
	` + alias + `.occurred_at_ms,
	` + alias + `.created_at_ms,
	` + alias + `.updated_at_ms`
}

func scanMessage(row rowScanner) (Message, error) {
	var message Message
	err := row.Scan(
		&message.MessageID,
		&message.ConversationID,
		&message.AccountID,
		&message.RemoteMessageID,
		&message.SenderIdentityID,
		&message.Direction,
		&message.Body,
		&message.ReplyToRemoteID,
		&message.State,
		&message.OccurredAtMS,
		&message.CreatedAtMS,
		&message.UpdatedAtMS,
	)
	return message, err
}

func sameProjectedMessage(existing Message, candidate Message) bool {
	return optionalStringEqual(existing.SenderIdentityID, candidate.SenderIdentityID) &&
		existing.Direction == candidate.Direction &&
		existing.Body == candidate.Body &&
		optionalStringEqual(existing.ReplyToRemoteID, candidate.ReplyToRemoteID) &&
		existing.State == candidate.State &&
		existing.OccurredAtMS == candidate.OccurredAtMS
}

func optionalStringEqual(left, right *string) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func (r *MessageRepository) nowMS(operation string) (int64, error) {
	nowMS := r.now().UnixMilli()
	if nowMS <= 0 {
		return 0, fmt.Errorf("%s: current Unix time is not positive", operation)
	}
	return nowMS, nil
}

func invalidInboxConstraintError(cause error, inboxID string) error {
	if !isSQLiteConstraint(cause) {
		return fmt.Errorf("append inbox %q: %w", inboxID, cause)
	}
	if isSQLiteErrorCode(cause, sqliteConstraintForeignKeyCode) {
		return fmt.Errorf(
			"%w: %w: %w: append inbox %q: %w",
			ErrInvalidInboxRecord,
			ErrConstraintViolation,
			ErrOrphanInboxAccount,
			inboxID,
			cause,
		)
	}
	return fmt.Errorf(
		"%w: %w: append inbox %q: %w",
		ErrInvalidInboxRecord,
		ErrConstraintViolation,
		inboxID,
		cause,
	)
}

func (r *MessageRepository) mapMessageWriteError(
	ctx context.Context,
	tx *sql.Tx,
	message Message,
	cause error,
) error {
	if !isSQLiteConstraint(cause) {
		return fmt.Errorf("project message %q: upsert: %w", message.MessageID, cause)
	}
	var specific error
	if isSQLiteErrorCode(cause, sqliteConstraintForeignKeyCode) {
		var err error
		specific, err = classifyMessageForeignKey(ctx, tx, message)
		if err != nil {
			return fmt.Errorf(
				"project message %q: classify foreign key constraint: %w",
				message.MessageID,
				err,
			)
		}
	}
	return invalidMessageConstraintError(
		specific,
		cause,
		"upsert message %q",
		message.MessageID,
	)
}

func classifyMessageForeignKey(
	ctx context.Context,
	tx *sql.Tx,
	message Message,
) (error, error) {
	var conversationAccountID string
	err := tx.QueryRowContext(ctx, `
		SELECT account_id
		FROM conversations
		WHERE conversation_id = ?
	`, message.ConversationID).Scan(&conversationAccountID)
	if errors.Is(err, sql.ErrNoRows) {
		return orphanMessageError(ErrOrphanMessageConversation), nil
	}
	if err != nil {
		return nil, err
	}
	if conversationAccountID != message.AccountID {
		return ErrCrossAccountMessage, nil
	}

	if message.SenderIdentityID == nil {
		return nil, nil
	}
	var identityAccountID string
	err = tx.QueryRowContext(ctx, `
		SELECT account_id
		FROM identities
		WHERE identity_id = ?
	`, *message.SenderIdentityID).Scan(&identityAccountID)
	if errors.Is(err, sql.ErrNoRows) {
		return orphanMessageError(ErrOrphanMessageIdentity), nil
	}
	if err != nil {
		return nil, err
	}
	if identityAccountID != message.AccountID {
		return ErrCrossAccountMessage, nil
	}
	return nil, nil
}

func orphanMessageError(specific error) error {
	return fmt.Errorf("%w: %w", ErrOrphanMessage, specific)
}

func invalidMessageConstraintError(
	specific error,
	cause error,
	format string,
	args ...any,
) error {
	detail := fmt.Sprintf(format, args...)
	if specific == nil {
		return fmt.Errorf(
			"%w: %w: %s: %w",
			ErrInvalidMessage,
			ErrConstraintViolation,
			detail,
			cause,
		)
	}
	return fmt.Errorf(
		"%w: %w: %w: %s: %w",
		ErrInvalidMessage,
		ErrConstraintViolation,
		specific,
		detail,
		cause,
	)
}

func invalidMessageError(specific error, format string, args ...any) error {
	detail := fmt.Sprintf(format, args...)
	return fmt.Errorf(
		"%w: %w: %w: %s",
		ErrInvalidMessage,
		ErrConstraintViolation,
		specific,
		detail,
	)
}
