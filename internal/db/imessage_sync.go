package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// The iMessage importer (internal/importer/imessage.go) owns the rows whose
// message_id or conversation_id begins "imessage:", and on them only these
// columns:
//   - messages: conversation_id, sender_name, sender_number, body,
//     timestamp_ms, status, is_from_me, source_platform, source_id, reactions;
//   - conversations: name, is_group, participants, last_message_ts,
//     source_platform.
//
// Everything else on those rows (transcripts, media, decryption keys, reply
// ids, mentions_me; unread counts, favorites, tabs, notification modes,
// display protocols) belongs to other writers, and the methods here never
// change it. They write only what differs, so applying a state the store
// already holds commits nothing.

const (
	iMessageIDPrefix      = "imessage:"
	iMessageIDPrefixLimit = "imessage;" // the first string past every "imessage:..." id
	iMessageStatus        = "delivered"
	iMessagePlatform      = "imessage"

	// iMessageApplyBatch bounds the rows one write transaction touches, so a
	// large import never holds the store's single connection for long.
	iMessageApplyBatch = 500
	// iMessageReadBatch bounds the ids bound into one SELECT ... IN (...).
	iMessageReadBatch = 500
)

// IMessageRow is the importer-owned part of one messages row. Status is
// always "delivered" and SourcePlatform always "imessage".
type IMessageRow struct {
	MessageID      string
	ConversationID string
	SenderName     string
	SenderNumber   string
	Body           string
	TimestampMS    int64
	IsFromMe       bool
	SourceID       string
	Reactions      string
}

// IMessageStoredRow is an IMessageRow as the store holds it, with the two
// owned columns the desired state fixes and whether the row carries data
// another writer added.
type IMessageStoredRow struct {
	IMessageRow
	Status         string
	SourcePlatform string
	// HasMetadata: a transcript, media id, decryption key or reply id.
	HasMetadata bool
}

// SameOwned reports whether the stored row already holds every owned column
// of want except reactions.
func (r *IMessageStoredRow) SameOwned(want *IMessageRow) bool {
	return r.ConversationID == want.ConversationID &&
		r.SenderName == want.SenderName &&
		r.SenderNumber == want.SenderNumber &&
		r.Body == want.Body &&
		r.TimestampMS == want.TimestampMS &&
		r.IsFromMe == want.IsFromMe &&
		r.SourceID == want.SourceID &&
		r.Status == iMessageStatus &&
		r.SourcePlatform == iMessagePlatform
}

// IMessageConversation is the importer-owned part of one conversations row.
type IMessageConversation struct {
	ConversationID string
	Name           string
	IsGroup        bool
	Participants   string
	LastMessageTS  int64
	SourcePlatform string // read side only; writes always set "imessage"
}

// IMessageConversationChange is a conversation's desired state. With
// SetLastMessageTS false, last_message_ts is left as stored: the importer
// recomputes a chat's newest message only when its messages changed or the
// conversation is not stored. Such a change cannot insert (see
// ErrIMessageConversationVanished).
type IMessageConversationChange struct {
	IMessageConversation
	SetLastMessageTS bool
}

// IMessageChanges is one batch of desired state for ApplyIMessageChanges.
type IMessageChanges struct {
	Conversations []IMessageConversationChange
	Messages      []IMessageRow
	// StaleIDs are rows an older importer stored for chat.db rows that are
	// native reactions. Each is deleted only if it is still an imessage row
	// with no metadata (see HasMetadata); otherwise it is left alone.
	StaleIDs []string
}

// IMessageApplyResult counts what ApplyIMessageChanges wrote. Failed maps
// each message or conversation id whose write failed to its error; ids not
// in it either were written or already matched.
type IMessageApplyResult struct {
	ConversationsCreated int
	ConversationsUpdated int
	MessagesInserted     int
	MessagesUpdated      int // owned columns, reactions, or both
	StaleDeleted         int
	Failed               map[string]error
}

// Written is the number of message rows inserted, changed or deleted.
func (r IMessageApplyResult) Written() int {
	return r.MessagesInserted + r.MessagesUpdated + r.StaleDeleted
}

// ErrIMessageConversationVanished fails a conversation change that left
// last_message_ts unset (the caller saw the row stored) when the row is gone
// by the time it is applied.
var ErrIMessageConversationVanished = errors.New("imessage conversation deleted since it was read")

// IMessageRowsByID returns the stored rows among ids, keyed by message_id.
func (s *Store) IMessageRowsByID(ids []string) (map[string]*IMessageStoredRow, error) {
	out := make(map[string]*IMessageStoredRow, len(ids))
	for start := 0; start < len(ids); start += iMessageReadBatch {
		end := min(start+iMessageReadBatch, len(ids))
		chunk := ids[start:end]
		args := make([]any, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}
		if err := s.scanIMessageRows(out, `WHERE message_id IN (`+placeholders(len(chunk))+`)`, args...); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// AllIMessageRows returns every stored row whose id begins "imessage:".
func (s *Store) AllIMessageRows() (map[string]*IMessageStoredRow, error) {
	out := map[string]*IMessageStoredRow{}
	if err := s.scanIMessageRows(out, `WHERE message_id >= ? AND message_id < ?`, iMessageIDPrefix, iMessageIDPrefixLimit); err != nil {
		return nil, err
	}
	return out, nil
}

const iMessageStoredColumns = `message_id, conversation_id, sender_name, sender_number, body, timestamp_ms,
	is_from_me, source_id, reactions, status, source_platform,
	(transcript != '' OR media_id != '' OR decryption_key != '' OR reply_to_id != '')`

func scanIMessageStoredRow(scan func(...any) error) (*IMessageStoredRow, error) {
	r := &IMessageStoredRow{}
	err := scan(&r.MessageID, &r.ConversationID, &r.SenderName, &r.SenderNumber, &r.Body, &r.TimestampMS,
		&r.IsFromMe, &r.SourceID, &r.Reactions, &r.Status, &r.SourcePlatform, &r.HasMetadata)
	return r, err
}

func (s *Store) scanIMessageRows(out map[string]*IMessageStoredRow, where string, args ...any) error {
	rows, err := s.db.Query(`SELECT `+iMessageStoredColumns+` FROM messages `+where, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		r, err := scanIMessageStoredRow(rows.Scan)
		if err != nil {
			return err
		}
		out[r.MessageID] = r
	}
	return rows.Err()
}

// IMessageConversations returns every stored conversation whose id begins
// "imessage:", keyed by id.
func (s *Store) IMessageConversations() (map[string]*IMessageConversation, error) {
	rows, err := s.db.Query(`
		SELECT conversation_id, name, is_group, participants, last_message_ts, source_platform
		FROM conversations WHERE conversation_id >= ? AND conversation_id < ?`,
		iMessageIDPrefix, iMessageIDPrefixLimit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]*IMessageConversation{}
	for rows.Next() {
		c := &IMessageConversation{}
		if err := rows.Scan(&c.ConversationID, &c.Name, &c.IsGroup, &c.Participants, &c.LastMessageTS, &c.SourcePlatform); err != nil {
			return nil, err
		}
		out[c.ConversationID] = c
	}
	return out, rows.Err()
}

// ApplyIMessageChanges brings the given rows to their desired state.
//
// It writes in BEGIN IMMEDIATE transactions of at most iMessageApplyBatch
// rows, conversations first. Taking the write lock up front means a
// concurrent writer's commit can never leave this transaction on a stale
// snapshot (SQLITE_BUSY_SNAPSHOT); the lock is waited for under the store's
// busy timeout instead. Inside, each row is read again and only owned
// columns that still differ are written, so a change another writer made
// since the caller read the store is neither lost nor blindly undone.
//
// The search index is touched only for rows inserted, deleted or whose body
// changed: one DELETE ... IN (...) per transaction, then the inserts.
// Reaction-only changes never touch it. Reactions of rows that already
// existed are written through UpdateMessageReactions after their
// transaction commits; a new row is inserted with its reactions.
//
// A row whose statement fails (a constraint, say) is recorded in Failed and
// the rest of its transaction proceeds. A transaction that cannot start or
// commit fails every row in it and stops the apply; that error is returned.
func (s *Store) ApplyIMessageChanges(ch IMessageChanges) (IMessageApplyResult, error) {
	res := IMessageApplyResult{Failed: map[string]error{}}
	type item struct {
		conv  *IMessageConversationChange
		msg   *IMessageRow
		stale string
	}
	items := make([]item, 0, len(ch.Conversations)+len(ch.Messages)+len(ch.StaleIDs))
	for i := range ch.Conversations {
		items = append(items, item{conv: &ch.Conversations[i]})
	}
	for i := range ch.Messages {
		items = append(items, item{msg: &ch.Messages[i]})
	}
	for _, id := range ch.StaleIDs {
		items = append(items, item{stale: id})
	}
	itemID := func(it item) string {
		switch {
		case it.conv != nil:
			return it.conv.ConversationID
		case it.msg != nil:
			return it.msg.MessageID
		default:
			return it.stale
		}
	}

	ctx := context.Background()
	for start := 0; start < len(items); start += iMessageApplyBatch {
		batch := items[start:min(start+iMessageApplyBatch, len(items))]
		var reactions []IMessageRow
		var batchRes IMessageApplyResult
		failed := map[string]error{}
		err := s.withImmediateTx(ctx, func(conn *sql.Conn) error {
			var ftsDelete []string
			ftsInsert := map[string]string{}
			for _, it := range batch {
				var err error
				switch {
				case it.conv != nil:
					err = applyIMessageConversation(ctx, conn, it.conv, &batchRes)
				case it.msg != nil:
					var fts, reaction bool
					fts, reaction, err = applyIMessageRow(ctx, conn, it.msg, &batchRes)
					if err == nil && fts {
						ftsDelete = append(ftsDelete, it.msg.MessageID)
						ftsInsert[it.msg.MessageID] = it.msg.Body
					}
					if err == nil && reaction {
						reactions = append(reactions, *it.msg)
					}
				default:
					var deleted bool
					deleted, err = deleteStaleIMessageRow(ctx, conn, it.stale)
					if err == nil && deleted {
						batchRes.StaleDeleted++
						ftsDelete = append(ftsDelete, it.stale)
					}
				}
				if err != nil {
					failed[itemID(it)] = err
				}
			}
			if !s.ftsEnabled || len(ftsDelete) == 0 {
				return nil
			}
			for i := 0; i < len(ftsDelete); i += iMessageReadBatch {
				chunk := ftsDelete[i:min(i+iMessageReadBatch, len(ftsDelete))]
				args := make([]any, len(chunk))
				for j, id := range chunk {
					args[j] = id
				}
				if _, err := conn.ExecContext(ctx, `DELETE FROM messages_fts WHERE message_id IN (`+placeholders(len(chunk))+`)`, args...); err != nil {
					return fmt.Errorf("search index: %w", err)
				}
			}
			for _, id := range ftsDelete {
				body, ok := ftsInsert[id]
				if !ok {
					continue
				}
				if _, err := conn.ExecContext(ctx, `INSERT INTO messages_fts(message_id, body) VALUES (?, ?)`, id, body); err != nil {
					return fmt.Errorf("search index: %w", err)
				}
			}
			return nil
		})
		if err != nil {
			// Nothing in this batch was written, and nothing after it is
			// tried: every one of those rows is failed.
			for _, it := range items[start:] {
				res.Failed[itemID(it)] = err
			}
			return res, err
		}
		res.ConversationsCreated += batchRes.ConversationsCreated
		res.ConversationsUpdated += batchRes.ConversationsUpdated
		res.MessagesInserted += batchRes.MessagesInserted
		res.MessagesUpdated += batchRes.MessagesUpdated
		res.StaleDeleted += batchRes.StaleDeleted
		for id, err := range failed {
			res.Failed[id] = err
		}
		for _, r := range reactions {
			if err := s.UpdateMessageReactions(r.MessageID, r.Reactions); err != nil {
				res.Failed[r.MessageID] = err
				continue
			}
		}
	}
	return res, nil
}

// withImmediateTx runs fn inside BEGIN IMMEDIATE on the store's connection
// and commits, or rolls back when fn or the commit fails.
func (s *Store) withImmediateTx(ctx context.Context, fn func(*sql.Conn) error) (err error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			if _, rbErr := conn.ExecContext(ctx, `ROLLBACK`); rbErr != nil && err == nil {
				err = rbErr
			}
		}
	}()
	if err := fn(conn); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return err
	}
	committed = true
	return nil
}

// applyIMessageConversation inserts or updates one conversation's owned
// columns.
func applyIMessageConversation(ctx context.Context, conn *sql.Conn, want *IMessageConversationChange, res *IMessageApplyResult) error {
	if !strings.HasPrefix(want.ConversationID, iMessageIDPrefix) {
		return fmt.Errorf("not an imessage conversation: %q", want.ConversationID)
	}
	var have IMessageConversation
	err := conn.QueryRowContext(ctx, `
		SELECT name, is_group, participants, last_message_ts, source_platform
		FROM conversations WHERE conversation_id = ?`, want.ConversationID).
		Scan(&have.Name, &have.IsGroup, &have.Participants, &have.LastMessageTS, &have.SourcePlatform)
	if errors.Is(err, sql.ErrNoRows) {
		if !want.SetLastMessageTS {
			// The caller saw this conversation stored and did not compute its
			// newest message; another writer deleted it since. Inserting it
			// with last_message_ts 0 would be wrong, so fail it for a retry.
			return ErrIMessageConversationVanished
		}
		if _, err := conn.ExecContext(ctx, `
			INSERT INTO conversations (conversation_id, name, is_group, participants, last_message_ts, source_platform)
			VALUES (?, ?, ?, ?, ?, ?)`,
			want.ConversationID, want.Name, want.IsGroup, want.Participants, want.LastMessageTS, iMessagePlatform); err != nil {
			return err
		}
		res.ConversationsCreated++
		return nil
	}
	if err != nil {
		return err
	}
	var sets []string
	var args []any
	if have.Name != want.Name {
		sets, args = append(sets, "name = ?"), append(args, want.Name)
	}
	if have.IsGroup != want.IsGroup {
		sets, args = append(sets, "is_group = ?"), append(args, want.IsGroup)
	}
	if have.Participants != want.Participants {
		sets, args = append(sets, "participants = ?"), append(args, want.Participants)
	}
	if want.SetLastMessageTS && have.LastMessageTS != want.LastMessageTS {
		sets, args = append(sets, "last_message_ts = ?"), append(args, want.LastMessageTS)
	}
	if have.SourcePlatform != iMessagePlatform {
		sets, args = append(sets, "source_platform = ?"), append(args, iMessagePlatform)
	}
	if len(sets) == 0 {
		return nil
	}
	args = append(args, want.ConversationID)
	if _, err := conn.ExecContext(ctx, `UPDATE conversations SET `+strings.Join(sets, ", ")+` WHERE conversation_id = ?`, args...); err != nil {
		return err
	}
	res.ConversationsUpdated++
	return nil
}

// applyIMessageRow inserts or updates one message's owned columns. fts
// reports that its search entry must be replaced (new row or new body);
// reaction that its stored reactions differ and must be written after the
// transaction commits.
func applyIMessageRow(ctx context.Context, conn *sql.Conn, want *IMessageRow, res *IMessageApplyResult) (fts, reaction bool, err error) {
	if !strings.HasPrefix(want.MessageID, iMessageIDPrefix) || !strings.HasPrefix(want.ConversationID, iMessageIDPrefix) {
		return false, false, fmt.Errorf("not an imessage row: %q in %q", want.MessageID, want.ConversationID)
	}
	have, err := scanIMessageStoredRow(conn.QueryRowContext(ctx,
		`SELECT `+iMessageStoredColumns+` FROM messages WHERE message_id = ?`, want.MessageID).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := conn.ExecContext(ctx, `
			INSERT INTO messages (message_id, conversation_id, sender_name, sender_number, body, timestamp_ms,
				status, is_from_me, reactions, source_platform, source_id)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			want.MessageID, want.ConversationID, want.SenderName, want.SenderNumber, want.Body, want.TimestampMS,
			iMessageStatus, want.IsFromMe, want.Reactions, iMessagePlatform, want.SourceID); err != nil {
			return false, false, err
		}
		res.MessagesInserted++
		return true, false, nil
	}
	if err != nil {
		return false, false, err
	}
	var sets []string
	var args []any
	add := func(col string, v any) { sets, args = append(sets, col+" = ?"), append(args, v) }
	if have.ConversationID != want.ConversationID {
		add("conversation_id", want.ConversationID)
	}
	if have.SenderName != want.SenderName {
		add("sender_name", want.SenderName)
	}
	if have.SenderNumber != want.SenderNumber {
		add("sender_number", want.SenderNumber)
	}
	if have.Body != want.Body {
		add("body", want.Body)
	}
	if have.TimestampMS != want.TimestampMS {
		add("timestamp_ms", want.TimestampMS)
	}
	if have.IsFromMe != want.IsFromMe {
		add("is_from_me", want.IsFromMe)
	}
	if have.SourceID != want.SourceID {
		add("source_id", want.SourceID)
	}
	if have.Status != iMessageStatus {
		add("status", iMessageStatus)
	}
	if have.SourcePlatform != iMessagePlatform {
		add("source_platform", iMessagePlatform)
	}
	reaction = have.Reactions != want.Reactions
	if len(sets) > 0 {
		args = append(args, want.MessageID)
		if _, err := conn.ExecContext(ctx, `UPDATE messages SET `+strings.Join(sets, ", ")+` WHERE message_id = ?`, args...); err != nil {
			return false, false, err
		}
	}
	if len(sets) > 0 || reaction {
		res.MessagesUpdated++
	}
	return have.Body != want.Body, reaction, nil
}

// deleteStaleIMessageRow deletes id when it is an imessage row without
// metadata, and reports whether it did.
func deleteStaleIMessageRow(ctx context.Context, conn *sql.Conn, id string) (bool, error) {
	if !strings.HasPrefix(id, iMessageIDPrefix) {
		return false, fmt.Errorf("not an imessage row: %q", id)
	}
	result, err := conn.ExecContext(ctx, `
		DELETE FROM messages
		WHERE message_id = ? AND source_platform = ?
			AND transcript = '' AND media_id = '' AND decryption_key = '' AND reply_to_id = ''`,
		id, iMessagePlatform)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n > 0, err
}

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.Repeat("?, ", n-1) + "?"
}
