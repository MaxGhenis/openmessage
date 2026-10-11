package importer

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/maxghenis/openmessage/internal/db"

	_ "modernc.org/sqlite"
)

// macOS Core Data epoch: 2001-01-01 00:00:00 UTC in Unix seconds.
const coreDataEpoch = 978307200

const (
	// DefaultIMessageWindow is how far back a poll re-reads message rows for
	// in-place changes. Messages allows an edit for 15 minutes and an unsend
	// for 2, and fixes an outgoing row's date seconds after inserting it.
	DefaultIMessageWindow = 24 * time.Hour
	// DefaultIMessageFullEvery is how often a reused importer re-reads all of
	// chat.db anyway, the backstop for anything the poll does not watch.
	DefaultIMessageFullEvery = 6 * time.Hour

	chatDBReadBatch = 500
)

// IMessage imports Messages' chat.db (needs Full Disk Access) into the legacy
// store.
//
// What an import makes the store hold is a function of chat.db alone, the
// same for a first import, a re-import and a poll:
//   - a conversation "imessage:<chat guid>" per chat;
//   - a message "imessage:<guid>" per row that has a body, is joined to a
//     chat, and is not a native reaction. A message joined to several chats
//     belongs to the one with the lowest ROWID. A row whose
//     associated_message_type is 0 is a message whatever its text says, so a
//     typed "Loved “hi”" stays text;
//   - each message's reactions, folded from every reaction row aimed at it
//     (associated_message_type 2000–2006 add, 3000–3006 remove;
//     associated_message_guid "p:N/<guid>", "bp:<guid>" or "<guid>") by
//     db.NativeReactionsJSON. Reaction rows get no row of their own; a copy an
//     older importer stored as text is deleted when it has no metadata.
//
// It writes only the columns it owns (see internal/db/imessage_sync.go) and
// only where they differ, so re-importing an unchanged chat.db commits
// nothing, and transcripts, media, reply ids and unread counts are never
// touched. Rows whose chat.db message was deleted stay in the store.
//
// One IMessage reused across calls polls: it keeps a read-only connection to
// chat.db and, from its last successful call, marks of what it read. A call
// returns at once when chat.db has not been written since. Otherwise it reads
// only rows added since the marks, rows dated within Window (in-place edits),
// and the small chat, handle and chat_handle_join tables, and recomputes what
// those rows affect. It reads everything again on its first call, when the
// file was replaced, when a row at or below a mark was deleted, when a chat's
// guid or a handle changed, and every FullEvery. A poll leaves the store as a
// full import would, except that an in-place edit to a row dated outside
// Window waits for the next full read. Call Close when done with it.
type IMessage struct {
	// DBPath is the path to chat.db. Defaults to ~/Library/Messages/chat.db.
	DBPath string
	// MyName is the display name for outgoing messages (default "Me").
	MyName string
	// Window defaults to DefaultIMessageWindow; FullEvery to
	// DefaultIMessageFullEvery (negative: never).
	Window    time.Duration
	FullEvery time.Duration

	mu         sync.Mutex
	src        *chatSource
	marks      *chatMarks
	lastFull   time.Time
	retryRows  map[int64]struct{}
	retryChats map[int64]struct{}

	now         func() time.Time // tests: the clock
	beforeApply func()           // tests: runs after chat.db is read, before the store is written
	last        pollStats        // tests: what the last call did
}

// pollStats records what one call did, for tests.
type pollStats struct {
	skipped    bool   // returned on an unchanged data_version
	full       bool   // read all of chat.db
	fullReason string // why
	rowsRead   int    // message rows loaded
}

// chatSource is the held read-only connection to chat.db.
type chatSource struct {
	db   *sql.DB
	conn *sql.Conn
	path string
	file os.FileInfo
	cols messageCols
}

// messageCols records which optional message columns this chat.db has.
type messageCols struct {
	assocType, assocGUID, assocEmoji bool
}

// chatMarks is what a successful call read, compared by the next one.
type chatMarks struct {
	dataVersion int64
	msgMax      int64
	msgMaxGUID  string
	msgCount    int64
	joinMax     int64
	joinMaxPair [2]int64
	joinCount   int64
	chatGUIDs   map[int64]string
	handles     map[int64]handleRow
}

type handleRow struct{ id, display string }

type chatRow struct {
	rowID       int64
	guid        string
	displayName string
	style       int64
}

type msgRow struct {
	rowID      int64
	guid       string
	body       string
	date       int64
	fromMe     bool
	handleID   int64
	assocType  int64
	assocGUID  string
	assocEmoji string
	owner      int64 // lowest joined chat ROWID; 0 when the row has no join
}

func (m *msgRow) isReaction() bool { return db.IsNativeTapbackType(m.assocType) }

func (m *msgRow) importable() bool { return !m.isReaction() && m.body != "" && m.owner != 0 }

// Close releases the chat.db connection. The IMessage can be used again; its
// next call reads everything.
func (im *IMessage) Close() error {
	im.mu.Lock()
	defer im.mu.Unlock()
	im.marks = nil
	return im.closeSource()
}

func (im *IMessage) closeSource() error {
	if im.src == nil {
		return nil
	}
	err := im.src.conn.Close()
	if cerr := im.src.db.Close(); err == nil {
		err = cerr
	}
	im.src = nil
	return err
}

func (im *IMessage) clock() time.Time {
	if im.now != nil {
		return im.now()
	}
	return time.Now()
}

func (im *IMessage) window() time.Duration {
	if im.Window > 0 {
		return im.Window
	}
	return DefaultIMessageWindow
}

func (im *IMessage) fullEvery() time.Duration {
	if im.FullEvery != 0 {
		return im.FullEvery
	}
	return DefaultIMessageFullEvery
}

func (im *IMessage) myName() string {
	if im.MyName != "" {
		return im.MyName
	}
	return "Me"
}

func (im *IMessage) path() string {
	if im.DBPath != "" {
		return im.DBPath
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "Messages", "chat.db")
}

// openSource (re)opens chat.db when nothing is open or the path now names a
// different file, and reports whether it did.
func (im *IMessage) openSource() (bool, error) {
	path := im.path()
	info, err := os.Stat(path)
	if err != nil {
		return false, fmt.Errorf("open iMessage db: %w", err)
	}
	if im.src != nil && im.src.path == path && os.SameFile(im.src.file, info) {
		return false, nil
	}
	if err := im.closeSource(); err != nil {
		return false, err
	}
	conn, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return false, fmt.Errorf("open iMessage db: %w", err)
	}
	conn.SetMaxOpenConns(1)
	held, err := conn.Conn(context.Background())
	if err != nil {
		conn.Close()
		return false, fmt.Errorf("open iMessage db: %w", err)
	}
	src := &chatSource{db: conn, conn: held, path: path, file: info}
	if src.cols, err = readMessageCols(held); err != nil {
		held.Close()
		conn.Close()
		return false, fmt.Errorf("read iMessage schema: %w", err)
	}
	im.src = src
	return true, nil
}

func readMessageCols(conn *sql.Conn) (messageCols, error) {
	rows, err := conn.QueryContext(context.Background(), `PRAGMA table_info(message)`)
	if err != nil {
		return messageCols{}, err
	}
	defer rows.Close()
	var cols messageCols
	seen := false
	for rows.Next() {
		var cid, notNull, pk int
		var name, typ string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			return cols, err
		}
		seen = true
		switch name {
		case "associated_message_type":
			cols.assocType = true
		case "associated_message_guid":
			cols.assocGUID = true
		case "associated_message_emoji":
			cols.assocEmoji = true
		}
	}
	if err := rows.Err(); err != nil {
		return cols, err
	}
	if !seen {
		return cols, errors.New("chat.db has no message table")
	}
	return cols, nil
}

// ImportFromDB imports chat.db into store; see IMessage.
func (im *IMessage) ImportFromDB(store *db.Store) (*ImportResult, error) {
	im.mu.Lock()
	defer im.mu.Unlock()
	im.last = pollStats{}
	now := im.clock()

	reopened, err := im.openSource()
	if err != nil {
		return nil, err
	}
	var dataVersion int64
	if err := im.src.conn.QueryRowContext(context.Background(), `PRAGMA data_version`).Scan(&dataVersion); err != nil {
		return nil, fmt.Errorf("read iMessage data_version: %w", err)
	}

	fullReason := ""
	switch {
	case im.marks == nil:
		fullReason = "first read"
	case reopened:
		fullReason = "chat.db replaced"
	case im.fullEvery() > 0 && now.Sub(im.lastFull) >= im.fullEvery():
		fullReason = "periodic"
	}
	if fullReason == "" && dataVersion == im.marks.dataVersion && len(im.retryRows) == 0 && len(im.retryChats) == 0 {
		im.last.skipped = true
		return &ImportResult{}, nil
	}

	plan, err := im.read(store, dataVersion, fullReason, now)
	if err != nil {
		return nil, err
	}
	if im.beforeApply != nil {
		im.beforeApply()
	}
	applied, applyErr := store.ApplyIMessageChanges(plan.changes)

	// The chat.db read succeeded, so the marks advance. Rows whose write
	// failed are read again by the next call.
	im.marks = plan.marks
	if plan.full {
		im.lastFull = now
	}
	im.retryRows, im.retryChats = map[int64]struct{}{}, map[int64]struct{}{}
	result := &ImportResult{
		ConversationsCreated: applied.ConversationsCreated,
		MessagesImported:     applied.Written(),
		MessagesDuplicate:    plan.unchanged,
	}
	failedIDs := make([]string, 0, len(applied.Failed))
	for id := range applied.Failed {
		failedIDs = append(failedIDs, id)
	}
	sort.Strings(failedIDs)
	for _, id := range failedIDs {
		if rowID, ok := plan.rowOf[id]; ok {
			im.retryRows[rowID] = struct{}{}
		}
		if chatID, ok := plan.chatOf[id]; ok {
			im.retryChats[chatID] = struct{}{}
		}
		result.Errors = append(result.Errors, fmt.Sprintf("%s: %v", id, applied.Failed[id]))
	}
	if applyErr != nil {
		return result, fmt.Errorf("write iMessage rows: %w", applyErr)
	}
	return result, nil
}

// readPlan is the desired state an import computed, still to be applied.
type readPlan struct {
	full      bool
	marks     *chatMarks
	changes   db.IMessageChanges
	unchanged int              // importable messages the store already holds as wanted
	rowOf     map[string]int64 // message or stale id → chat.db ROWID
	chatOf    map[string]int64 // conversation id → chat ROWID
}

// read computes, from one chat.db snapshot, everything this call must write.
// fullReason "" means a poll, which may still turn into a full read.
func (im *IMessage) read(store *db.Store, dataVersion int64, fullReason string, now time.Time) (*readPlan, error) {
	ctx := context.Background()
	tx, err := im.src.conn.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("read iMessage db: %w", err)
	}
	defer tx.Rollback()
	r := &snapshotReader{ctx: ctx, tx: tx, cols: im.src.cols}

	marks := &chatMarks{dataVersion: dataVersion}
	if err := r.readMarks(marks); err != nil {
		return nil, err
	}
	chats, err := r.readChats()
	if err != nil {
		return nil, err
	}
	handles, err := r.readHandles()
	if err != nil {
		return nil, err
	}
	participants, err := r.readParticipants(handles)
	if err != nil {
		return nil, err
	}
	marks.chatGUIDs = make(map[int64]string, len(chats))
	for _, c := range chats {
		marks.chatGUIDs[c.rowID] = c.guid
	}
	marks.handles = handles

	var newRows []int64
	var newJoins [][3]int64 // rowid, chat_id, message_id
	if fullReason == "" {
		prev := im.marks
		if newRows, err = r.rowIDs(`SELECT ROWID FROM message WHERE ROWID > ? AND ROWID <= ?`, prev.msgMax, marks.msgMax); err != nil {
			return nil, err
		}
		if newJoins, err = r.joinsAbove(prev.joinMax, marks.joinMax); err != nil {
			return nil, err
		}
		atPrev, err := r.atMarks(prev)
		if err != nil {
			return nil, err
		}
		fullReason = prev.fullReadNeeded(marks, atPrev, len(newRows), len(newJoins))
	}

	plan := &readPlan{marks: marks, rowOf: map[string]int64{}, chatOf: map[string]int64{}}
	chatByID := make(map[int64]*chatRow, len(chats))
	for i := range chats {
		chatByID[chats[i].rowID] = &chats[i]
		plan.chatOf["imessage:"+chats[i].guid] = chats[i].rowID
	}

	storedConvs, err := store.IMessageConversations()
	if err != nil {
		return nil, fmt.Errorf("read stored iMessage conversations: %w", err)
	}

	// rows: the message rows to reconcile. lastTSChats: chats whose newest
	// message must be recomputed.
	rows := map[int64]*msgRow{}
	lastTSChats := map[int64]struct{}{}
	if fullReason != "" {
		plan.full = true
		im.last.full, im.last.fullReason = true, fullReason
		all, err := r.loadRows("", nil)
		if err != nil {
			return nil, err
		}
		for _, m := range all {
			rows[m.rowID] = m
		}
		for _, c := range chats {
			lastTSChats[c.rowID] = struct{}{}
		}
	} else {
		changed := map[int64]struct{}{}
		for _, id := range newRows {
			changed[id] = struct{}{}
		}
		for _, j := range newJoins {
			changed[j[2]] = struct{}{}
			lastTSChats[j[1]] = struct{}{}
		}
		windowStart := (now.Add(-im.window()).Unix() - coreDataEpoch) * 1_000_000_000
		windowRows, err := r.rowIDs(`SELECT ROWID FROM message WHERE date >= ?`, windowStart)
		if err != nil {
			return nil, err
		}
		for _, id := range windowRows {
			changed[id] = struct{}{}
		}
		for id := range im.retryRows {
			changed[id] = struct{}{}
		}
		for id := range im.retryChats {
			lastTSChats[id] = struct{}{}
		}
		// A handle or chat row can land after rows that name it (sync order is
		// not insertion order): re-read the messages of every handle and chat
		// this poll sees for the first time.
		var newHandles, newChats []int64
		for id := range handles {
			if _, ok := im.marks.handles[id]; !ok {
				newHandles = append(newHandles, id)
			}
		}
		for _, c := range chats {
			_, known := im.marks.chatGUIDs[c.rowID]
			_, stored := storedConvs["imessage:"+c.guid]
			// A chat whose conversation another writer deleted from the store
			// (deleting a conversation deletes its messages) is restored whole,
			// as a full read would.
			if !known || !stored {
				newChats = append(newChats, c.rowID)
				lastTSChats[c.rowID] = struct{}{}
			}
		}
		named, err := r.rowIDsIn(`SELECT ROWID FROM message WHERE handle_id IN`, newHandles)
		if err != nil {
			return nil, err
		}
		inNewChats, err := r.rowIDsIn(`SELECT message_id FROM chat_message_join WHERE chat_id IN`, newChats)
		if err != nil {
			return nil, err
		}
		for _, id := range append(named, inNewChats...) {
			changed[id] = struct{}{}
		}
		joined, err := r.chatsOfMessages(keys(changed))
		if err != nil {
			return nil, err
		}
		for _, id := range joined {
			lastTSChats[id] = struct{}{}
		}
		loaded, err := r.loadRows("WHERE m.ROWID IN", int64Args(keys(changed)))
		if err != nil {
			return nil, err
		}
		for _, m := range loaded {
			rows[m.rowID] = m
		}
		// A reaction changes its target: reconcile the target too.
		var targets []string
		for _, m := range loaded {
			if m.isReaction() {
				if g := reactionTarget(m.assocGUID); g != "" {
					targets = append(targets, g)
				}
			}
		}
		targetRows, err := r.loadRows("WHERE m.guid IN", stringArgs(targets))
		if err != nil {
			return nil, err
		}
		for _, m := range targetRows {
			rows[m.rowID] = m
		}
	}
	im.last.rowsRead = len(rows)

	// Reactions for every importable message being reconciled.
	targetSet := map[string]struct{}{}
	for _, m := range rows {
		if m.importable() {
			targetSet[m.guid] = struct{}{}
		}
	}
	events, err := r.reactionsFor(targetSet, plan.full, handles)
	if err != nil {
		return nil, err
	}

	// The store as it is now, to diff against.
	var stored map[string]*db.IMessageStoredRow
	if plan.full {
		stored, err = store.AllIMessageRows()
	} else {
		ids := make([]string, 0, len(rows))
		for _, m := range rows {
			ids = append(ids, "imessage:"+m.guid)
		}
		stored, err = store.IMessageRowsByID(ids)
	}
	if err != nil {
		return nil, fmt.Errorf("read stored iMessage rows: %w", err)
	}
	for _, c := range chats {
		if _, ok := storedConvs["imessage:"+c.guid]; !ok {
			lastTSChats[c.rowID] = struct{}{}
		}
	}
	lastTS, err := r.lastMessageDates(keys(lastTSChats), plan.full)
	if err != nil {
		return nil, err
	}

	// Conversations: every chat's name, group flag and participants; the
	// newest message only where it was recomputed.
	for _, c := range chats {
		id := "imessage:" + c.guid
		want := db.IMessageConversationChange{IMessageConversation: db.IMessageConversation{
			ConversationID: id,
			Name:           c.displayName,
			IsGroup:        c.style == 43, // iMessage group chat style
			Participants:   participantsJSON(participants[c.rowID]),
		}}
		if want.Name == "" {
			want.Name = derivedChatName(participants[c.rowID])
		}
		if _, ok := lastTSChats[c.rowID]; ok {
			want.SetLastMessageTS = true
			want.LastMessageTS = coreDataToMS(lastTS[c.rowID])
		}
		have := storedConvs[id]
		if have != nil && have.Name == want.Name && have.IsGroup == want.IsGroup &&
			have.Participants == want.Participants && have.SourcePlatform == "imessage" &&
			(!want.SetLastMessageTS || have.LastMessageTS == want.LastMessageTS) {
			continue
		}
		plan.changes.Conversations = append(plan.changes.Conversations, want)
	}

	// Messages, in ROWID order so writes are deterministic.
	rowIDs := keys(rowsAsSet(rows))
	myName := im.myName()
	for _, rowID := range rowIDs {
		m := rows[rowID]
		id := "imessage:" + m.guid
		plan.rowOf[id] = m.rowID
		if m.isReaction() {
			if have := stored[id]; have != nil && have.SourcePlatform == "imessage" && !have.HasMetadata {
				plan.changes.StaleIDs = append(plan.changes.StaleIDs, id)
			}
			continue
		}
		if !m.importable() {
			continue
		}
		owner := chatByID[m.owner]
		if owner == nil {
			continue
		}
		want := db.IMessageRow{
			MessageID:      id,
			ConversationID: "imessage:" + owner.guid,
			Body:           m.body,
			TimestampMS:    coreDataToMS(m.date),
			IsFromMe:       m.fromMe,
			SourceID:       m.guid,
			Reactions:      db.NativeReactionsJSON(events[m.guid]),
		}
		if m.fromMe {
			want.SenderName = myName
		} else if h, ok := handles[m.handleID]; ok {
			want.SenderName, want.SenderNumber = h.display, h.id
		}
		if have := stored[id]; have != nil && have.SameOwned(&want) && have.Reactions == want.Reactions {
			plan.unchanged++
			continue
		}
		plan.changes.Messages = append(plan.changes.Messages, want)
	}
	return plan, nil
}

// rowsAtMarks is what a snapshot holds at the previous marks' positions.
type rowsAtMarks struct {
	msgGUID  string   // "" when the row is gone
	joinPair [2]int64 // zero when the row is gone
}

// fullReadNeeded reports why a poll must read everything ("" when it need
// not), comparing the previous marks with the snapshot's. Rows only ever
// appear above a mark (message ROWIDs never repeat; a join rowid repeats
// only after the highest one is deleted, which changes the row at the
// mark), so a count that is not the old count plus the rows above the mark
// means rows at or below it were deleted.
func (prev *chatMarks) fullReadNeeded(cur *chatMarks, at rowsAtMarks, rowsAbove, joinsAbove int) string {
	switch {
	case cur.msgMax < prev.msgMax:
		return "message rows end below the mark"
	case prev.msgMax > 0 && at.msgGUID != prev.msgMaxGUID:
		return "the message at the mark changed"
	case cur.msgCount != prev.msgCount+int64(rowsAbove):
		return "message rows deleted"
	case cur.joinMax < prev.joinMax:
		return "chat_message_join rows end below the mark"
	case prev.joinMax > 0 && at.joinPair != prev.joinMaxPair:
		return "the chat_message_join row at the mark changed"
	case cur.joinCount != prev.joinCount+int64(joinsAbove):
		return "chat_message_join rows deleted"
	}
	for rowID, guid := range prev.chatGUIDs {
		if now, ok := cur.chatGUIDs[rowID]; ok && now != guid {
			return "a chat's guid changed"
		}
	}
	for rowID, h := range prev.handles {
		if now, ok := cur.handles[rowID]; ok && now != h {
			return "a handle changed"
		}
	}
	return ""
}

// snapshotReader runs every chat.db query of one call inside one read
// transaction, so they all see the same snapshot.
type snapshotReader struct {
	ctx  context.Context
	tx   *sql.Tx
	cols messageCols
}

// atMarks reads what this snapshot holds where prev's marks point.
func (r *snapshotReader) atMarks(prev *chatMarks) (rowsAtMarks, error) {
	var at rowsAtMarks
	if prev.msgMax > 0 {
		err := r.tx.QueryRowContext(r.ctx, `SELECT guid FROM message WHERE ROWID = ?`, prev.msgMax).Scan(&at.msgGUID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return at, fmt.Errorf("read iMessage marks: %w", err)
		}
	}
	if prev.joinMax > 0 {
		err := r.tx.QueryRowContext(r.ctx, `SELECT chat_id, message_id FROM chat_message_join WHERE rowid = ?`, prev.joinMax).Scan(&at.joinPair[0], &at.joinPair[1])
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return at, fmt.Errorf("read iMessage marks: %w", err)
		}
	}
	return at, nil
}

// readMarks fills the snapshot's own marks.
func (r *snapshotReader) readMarks(m *chatMarks) error {
	if err := r.tx.QueryRowContext(r.ctx, `SELECT COALESCE(MAX(ROWID), 0), COUNT(*) FROM message`).Scan(&m.msgMax, &m.msgCount); err != nil {
		return fmt.Errorf("read iMessage marks: %w", err)
	}
	if m.msgMax > 0 {
		if err := r.tx.QueryRowContext(r.ctx, `SELECT guid FROM message WHERE ROWID = ?`, m.msgMax).Scan(&m.msgMaxGUID); err != nil {
			return fmt.Errorf("read iMessage marks: %w", err)
		}
	}
	if err := r.tx.QueryRowContext(r.ctx, `SELECT COALESCE(MAX(rowid), 0), COUNT(*) FROM chat_message_join`).Scan(&m.joinMax, &m.joinCount); err != nil {
		return fmt.Errorf("read iMessage marks: %w", err)
	}
	if m.joinMax > 0 {
		if err := r.tx.QueryRowContext(r.ctx, `SELECT chat_id, message_id FROM chat_message_join WHERE rowid = ?`, m.joinMax).Scan(&m.joinMaxPair[0], &m.joinMaxPair[1]); err != nil {
			return fmt.Errorf("read iMessage marks: %w", err)
		}
	}
	return nil
}

func (r *snapshotReader) readChats() ([]chatRow, error) {
	rows, err := r.tx.QueryContext(r.ctx, `
		SELECT ROWID, guid, COALESCE(display_name, ''), COALESCE(style, 0) FROM chat ORDER BY ROWID`)
	if err != nil {
		return nil, fmt.Errorf("load chats: %w", err)
	}
	defer rows.Close()
	var chats []chatRow
	for rows.Next() {
		var c chatRow
		if err := rows.Scan(&c.rowID, &c.guid, &c.displayName, &c.style); err != nil {
			return nil, fmt.Errorf("load chats: %w", err)
		}
		chats = append(chats, c)
	}
	return chats, rows.Err()
}

func (r *snapshotReader) readHandles() (map[int64]handleRow, error) {
	rows, err := r.tx.QueryContext(r.ctx, `SELECT ROWID, COALESCE(id, ''), COALESCE(uncanonicalized_id, id, '') FROM handle`)
	if err != nil {
		return nil, fmt.Errorf("load handles: %w", err)
	}
	defer rows.Close()
	out := map[int64]handleRow{}
	for rows.Next() {
		var id int64
		var h handleRow
		if err := rows.Scan(&id, &h.id, &h.display); err != nil {
			return nil, fmt.Errorf("load handles: %w", err)
		}
		out[id] = h
	}
	return out, rows.Err()
}

// readParticipants returns each chat's handles in handle ROWID order.
func (r *snapshotReader) readParticipants(handles map[int64]handleRow) (map[int64][]handleRow, error) {
	rows, err := r.tx.QueryContext(r.ctx, `
		SELECT chat_id, handle_id FROM chat_handle_join ORDER BY chat_id, handle_id`)
	if err != nil {
		return nil, fmt.Errorf("load chat participants: %w", err)
	}
	defer rows.Close()
	out := map[int64][]handleRow{}
	for rows.Next() {
		var chatID, handleID int64
		if err := rows.Scan(&chatID, &handleID); err != nil {
			return nil, fmt.Errorf("load chat participants: %w", err)
		}
		if h, ok := handles[handleID]; ok {
			out[chatID] = append(out[chatID], h)
		}
	}
	return out, rows.Err()
}

func (r *snapshotReader) rowIDs(query string, args ...any) ([]int64, error) {
	rows, err := r.tx.QueryContext(r.ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read iMessage rows: %w", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("read iMessage rows: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (r *snapshotReader) joinsAbove(after, upTo int64) ([][3]int64, error) {
	rows, err := r.tx.QueryContext(r.ctx, `
		SELECT rowid, chat_id, message_id FROM chat_message_join WHERE rowid > ? AND rowid <= ?`, after, upTo)
	if err != nil {
		return nil, fmt.Errorf("read chat_message_join: %w", err)
	}
	defer rows.Close()
	var out [][3]int64
	for rows.Next() {
		var j [3]int64
		if err := rows.Scan(&j[0], &j[1], &j[2]); err != nil {
			return nil, fmt.Errorf("read chat_message_join: %w", err)
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// chatsOfMessages returns every chat any of the messages is joined to.
func (r *snapshotReader) chatsOfMessages(rowIDs []int64) ([]int64, error) {
	return r.rowIDsIn(`SELECT DISTINCT chat_id FROM chat_message_join WHERE message_id IN`, rowIDs)
}

// rowIDsIn runs query ("... IN") over ids in batches.
func (r *snapshotReader) rowIDsIn(query string, ids []int64) ([]int64, error) {
	var out []int64
	for start := 0; start < len(ids); start += chatDBReadBatch {
		chunk := ids[start:min(start+chatDBReadBatch, len(ids))]
		got, err := r.rowIDs(query+` (`+qmarks(len(chunk))+`)`, int64Args(chunk)...)
		if err != nil {
			return nil, err
		}
		out = append(out, got...)
	}
	return out, nil
}

func (r *snapshotReader) messageSelect() string {
	assocType, assocGUID, assocEmoji := "0", "''", "''"
	if r.cols.assocType {
		assocType = "COALESCE(m.associated_message_type, 0)"
	}
	if r.cols.assocGUID {
		assocGUID = "COALESCE(m.associated_message_guid, '')"
	}
	if r.cols.assocEmoji {
		assocEmoji = "COALESCE(m.associated_message_emoji, '')"
	}
	return `SELECT m.ROWID, m.guid, m.text, m.attributedBody, COALESCE(m.date, 0), COALESCE(m.is_from_me, 0),
			COALESCE(m.handle_id, 0), ` + assocType + `, ` + assocGUID + `, ` + assocEmoji + `,
			COALESCE((SELECT MIN(j.chat_id) FROM chat_message_join j JOIN chat c ON c.ROWID = j.chat_id
				WHERE j.message_id = m.ROWID), 0)
		FROM message m `
}

// loadRows loads message rows: all of them when where is "", else those
// matching where ("WHERE m.ROWID IN" or "WHERE m.guid IN") one of args.
func (r *snapshotReader) loadRows(where string, args []any) ([]*msgRow, error) {
	if where == "" {
		return r.scanRows(r.messageSelect())
	}
	var out []*msgRow
	for start := 0; start < len(args); start += chatDBReadBatch {
		chunk := args[start:min(start+chatDBReadBatch, len(args))]
		got, err := r.scanRows(r.messageSelect()+where+` (`+qmarks(len(chunk))+`)`, chunk...)
		if err != nil {
			return nil, err
		}
		out = append(out, got...)
	}
	return out, nil
}

func (r *snapshotReader) scanRows(query string, args ...any) ([]*msgRow, error) {
	rows, err := r.tx.QueryContext(r.ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("load messages: %w", err)
	}
	defer rows.Close()
	var out []*msgRow
	for rows.Next() {
		m := &msgRow{}
		var text sql.NullString
		var attributedBody []byte
		if err := rows.Scan(&m.rowID, &m.guid, &text, &attributedBody, &m.date, &m.fromMe,
			&m.handleID, &m.assocType, &m.assocGUID, &m.assocEmoji, &m.owner); err != nil {
			return nil, fmt.Errorf("load messages: %w", err)
		}
		if !m.isReaction() {
			// Modern Messages leaves text NULL and keeps the body in
			// attributedBody (an NSAttributedString archive).
			m.body = text.String
			if m.body == "" && len(attributedBody) > 0 {
				m.body = decodeAttributedBody(attributedBody)
			}
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// reactionsFor returns, for each target guid, every reaction row aimed at it.
// A full read loads every association row; a poll first scans the guids
// alone (the associated_message_guid index covers them) and loads only rows
// aimed at a target in the set.
func (r *snapshotReader) reactionsFor(targets map[string]struct{}, full bool, handles map[int64]handleRow) (map[string][]db.NativeReaction, error) {
	out := map[string][]db.NativeReaction{}
	if len(targets) == 0 || !r.cols.assocGUID || !r.cols.assocType {
		return out, nil
	}
	var rows []*msgRow
	var err error
	if full {
		rows, err = r.scanRows(r.messageSelect() + `WHERE m.associated_message_guid IS NOT NULL`)
	} else {
		var ids []int64
		guidRows, qerr := r.tx.QueryContext(r.ctx, `SELECT ROWID, associated_message_guid FROM message WHERE associated_message_guid IS NOT NULL`)
		if qerr != nil {
			return nil, fmt.Errorf("load reactions: %w", qerr)
		}
		for guidRows.Next() {
			var id int64
			var guid string
			if err := guidRows.Scan(&id, &guid); err != nil {
				guidRows.Close()
				return nil, fmt.Errorf("load reactions: %w", err)
			}
			if _, ok := targets[reactionTarget(guid)]; ok {
				ids = append(ids, id)
			}
		}
		if err := guidRows.Err(); err != nil {
			guidRows.Close()
			return nil, fmt.Errorf("load reactions: %w", err)
		}
		guidRows.Close()
		rows, err = r.loadRows("WHERE m.ROWID IN", int64Args(ids))
	}
	if err != nil {
		return nil, err
	}
	for _, m := range rows {
		target := reactionTarget(m.assocGUID)
		if _, ok := targets[target]; !ok {
			continue
		}
		kind, add, emoji, ok := db.NativeTapbackKind(m.assocType, m.assocEmoji)
		if !ok {
			continue
		}
		actor := "me"
		if !m.fromMe {
			h := handles[m.handleID]
			actor = h.id
			if actor == "" {
				actor = h.display
			}
			if actor == "" {
				actor = "unknown"
			}
		}
		out[target] = append(out[target], db.NativeReaction{
			Date: m.date, RowID: m.rowID, Add: add, Kind: kind, Emoji: emoji, Actor: actor,
		})
	}
	return out, nil
}

// lastMessageDates returns MAX(message.date) over each chat's joins (any
// message, with or without text). Chats without one are absent.
func (r *snapshotReader) lastMessageDates(chatIDs []int64, full bool) (map[int64]int64, error) {
	out := map[int64]int64{}
	scan := func(query string, args ...any) error {
		rows, err := r.tx.QueryContext(r.ctx, query, args...)
		if err != nil {
			return fmt.Errorf("read chat dates: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var chatID, date int64
			if err := rows.Scan(&chatID, &date); err != nil {
				return fmt.Errorf("read chat dates: %w", err)
			}
			out[chatID] = date
		}
		return rows.Err()
	}
	const base = `SELECT j.chat_id, COALESCE(MAX(m.date), 0) FROM chat_message_join j JOIN message m ON m.ROWID = j.message_id `
	if full {
		return out, scan(base + `GROUP BY j.chat_id`)
	}
	for start := 0; start < len(chatIDs); start += chatDBReadBatch {
		chunk := chatIDs[start:min(start+chatDBReadBatch, len(chatIDs))]
		if err := scan(base+`WHERE j.chat_id IN (`+qmarks(len(chunk))+`) GROUP BY j.chat_id`, int64Args(chunk)...); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// reactionTarget is the message guid a reaction row's associated_message_guid
// names: "p:<part>/<guid>", "bp:<guid>", or a bare "<guid>".
func reactionTarget(assoc string) string {
	switch {
	case strings.HasPrefix(assoc, "p:"):
		if i := strings.IndexByte(assoc, '/'); i >= 0 {
			return assoc[i+1:]
		}
		return ""
	case strings.HasPrefix(assoc, "bp:"):
		return assoc[len("bp:"):]
	default:
		return assoc
	}
}

func participantsJSON(handles []handleRow) string {
	var participants []map[string]string
	for _, h := range handles {
		participants = append(participants, map[string]string{"name": h.display, "number": h.id})
	}
	b, _ := json.Marshal(participants)
	return string(b)
}

func derivedChatName(handles []handleRow) string {
	var names []string
	for _, h := range handles {
		if h.display != "" {
			names = append(names, h.display)
		} else if h.id != "" {
			names = append(names, h.id)
		}
	}
	return strings.Join(names, ", ")
}

func keys(set map[int64]struct{}) []int64 {
	out := make([]int64, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func rowsAsSet(rows map[int64]*msgRow) map[int64]struct{} {
	out := make(map[int64]struct{}, len(rows))
	for k := range rows {
		out[k] = struct{}{}
	}
	return out
}

func int64Args(ids []int64) []any {
	out := make([]any, len(ids))
	for i, id := range ids {
		out[i] = id
	}
	return out
}

func stringArgs(ids []string) []any {
	out := make([]any, len(ids))
	for i, id := range ids {
		out[i] = id
	}
	return out
}

func qmarks(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.Repeat("?, ", n-1) + "?"
}

// decodeAttributedBody best-effort extracts the plain message text from an
// iMessage `attributedBody` blob (an NSArchiver "streamtyped" archive of an
// NSAttributedString). The text is stored as an NSString whose UTF-8 bytes
// follow the "NSString" class marker, a few control bytes, a '+' (0x2B), and a
// length. The length uses the streamtyped variable-length integer encoding.
// Returns "" if the structure isn't recognized or the result isn't valid UTF-8
// (so a parse failure degrades to skipping, never to garbage).
func decodeAttributedBody(blob []byte) string {
	idx := bytes.Index(blob, []byte("NSString"))
	if idx < 0 {
		return ""
	}
	rest := blob[idx+len("NSString"):]
	plus := bytes.IndexByte(rest, '+')
	if plus < 0 || plus+1 >= len(rest) {
		return ""
	}
	rest = rest[plus+1:]

	var length int
	switch {
	case rest[0] < 0x80:
		length = int(rest[0])
		rest = rest[1:]
	case rest[0] == 0x81: // next 2 bytes, little-endian
		if len(rest) < 3 {
			return ""
		}
		length = int(binary.LittleEndian.Uint16(rest[1:3]))
		rest = rest[3:]
	case rest[0] == 0x82: // next 4 bytes, little-endian
		if len(rest) < 5 {
			return ""
		}
		length = int(binary.LittleEndian.Uint32(rest[1:5]))
		rest = rest[5:]
	default:
		return ""
	}
	if length <= 0 || length > len(rest) {
		return ""
	}
	text := string(rest[:length])
	if !utf8.ValidString(text) {
		return ""
	}
	return text
}

// coreDataToMS converts a macOS Core Data timestamp (nanoseconds since 2001-01-01) to milliseconds since Unix epoch.
func coreDataToMS(date int64) int64 {
	if date == 0 {
		return 0
	}
	secs := date / 1_000_000_000
	return (secs + coreDataEpoch) * 1000
}
