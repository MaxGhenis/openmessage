package whatsapplive

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	waE2E "go.mau.fi/whatsmeow/proto/waE2E"
	watypes "go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

// The history-sync queue holds our phone's history-sync notifications until
// the chunk each one names is committed to v2 ingest or held in the ingress
// spool. A notification is a small protocol message naming one chunk of
// message history, carried inline or stored on WhatsApp's media server.
//
// whatsmeow's default handling keeps a notification only in memory
// (handleProtocolMessage): it queues it on a channel, sends the history-sync
// receipt, and downloads and dispatches the chunk later from a background
// loop. By then the bridge has reported the notification's own message
// handled, so whatsmeow has cleared its buffered plaintext and acknowledged
// the stanza, and nothing delivers the notification again. A process that
// ends in between loses the chunk. The loop also drops a notification whose
// download fails once.
//
// So the bridge downloads history itself (ManualHistorySyncDownload, with
// DisableManualHistorySyncReceipt). It stores each notification here before
// it reports the protocol message handled, and the history-sync pump
// (history_sync_pump.go) downloads the chunk, commits or holds it, and only
// then sends the history-sync receipt, deletes the server copy, and removes
// the row. The table lives in the WhatsApp session database next to the
// ingress spool and is bounded by row count and notification bytes; a
// notification that would take it past either bound is refused.
const historySyncQueueSchema = `
CREATE TABLE IF NOT EXISTS openmessage_whatsapp_history_sync_pending (
    seq INTEGER PRIMARY KEY AUTOINCREMENT,
    message_id TEXT NOT NULL UNIQUE,
    account_user TEXT NOT NULL,
    notification BLOB NOT NULL,
    notification_bytes INTEGER NOT NULL,
    received_at_ns INTEGER NOT NULL,
    attempts INTEGER NOT NULL DEFAULT 0,
    next_attempt_at_ns INTEGER NOT NULL DEFAULT 0,
    committed_at_ns INTEGER NOT NULL DEFAULT 0,
    last_error TEXT NOT NULL DEFAULT ''
);
`

const (
	// historySyncQueueMaxRows and historySyncQueueMaxBytes bound the queue. A
	// notification is a few hundred bytes unless it carries its chunk inline,
	// and the pump drains the queue while the account is connected, so it
	// fills only while downloads keep failing or v2 keeps refusing chunks.
	historySyncQueueMaxRows  = 4096
	historySyncQueueMaxBytes = 64 << 20
)

var (
	errHistorySyncQueueFull        = errors.New("whatsapp history-sync queue is full")
	errHistorySyncQueueUnavailable = errors.New("whatsapp history-sync queue is unavailable")
)

type historySyncQueue struct {
	db       *sql.DB
	maxRows  int64
	maxBytes int64

	// queued counts the table's rows so a pass over an empty queue costs one
	// atomic load. It is loaded from the table at open and moves only with a
	// row this process inserted or deleted.
	queued atomic.Int64
}

// queuedHistorySync is one stored notification.
type queuedHistorySync struct {
	seq          int64
	messageID    watypes.MessageID
	accountUser  string
	notification *waE2E.HistorySyncNotification
	receivedAt   time.Time
	attempts     int
	committed    bool
}

func openHistorySyncQueue(database *sql.DB, maxRows int64, maxBytes int64) (*historySyncQueue, error) {
	if database == nil {
		return nil, errHistorySyncQueueUnavailable
	}
	if _, err := database.ExecContext(context.Background(), historySyncQueueSchema); err != nil {
		return nil, fmt.Errorf("initialize WhatsApp history-sync queue: %w", err)
	}
	queue := &historySyncQueue{db: database, maxRows: maxRows, maxBytes: maxBytes}
	var queued int64
	if err := database.QueryRowContext(
		context.Background(),
		`SELECT COUNT(*) FROM openmessage_whatsapp_history_sync_pending`,
	).Scan(&queued); err != nil {
		return nil, fmt.Errorf("count WhatsApp history-sync queue: %w", err)
	}
	queue.queued.Store(queued)
	return queue, nil
}

// pending reports how many notifications the queue holds.
func (q *historySyncQueue) pending() int64 {
	if q == nil {
		return 0
	}
	return q.queued.Load()
}

// enqueue stores a notification durably. A notification whose message id the
// queue already holds is not stored again: it is the same protocol message,
// delivered twice. One that would take the queue past either bound is refused
// with errHistorySyncQueueFull.
func (q *historySyncQueue) enqueue(
	messageID watypes.MessageID,
	accountUser string,
	notification *waE2E.HistorySyncNotification,
	receivedAt time.Time,
) error {
	if q == nil || q.db == nil {
		return errHistorySyncQueueUnavailable
	}
	if strings.TrimSpace(string(messageID)) == "" {
		return errors.New("whatsapp history-sync queue: notification has no message id")
	}
	if notification == nil {
		return errors.New("whatsapp history-sync queue: no notification")
	}
	encoded, err := proto.Marshal(notification)
	if err != nil {
		return fmt.Errorf("encode WhatsApp history-sync notification: %w", err)
	}
	size := int64(len(encoded))
	if size > q.maxBytes {
		return fmt.Errorf("%w: a %d-byte notification exceeds the %d-byte bound", errHistorySyncQueueFull, size, q.maxBytes)
	}
	ctx := context.Background()
	// One statement, so the bound check and the insert are atomic.
	result, err := q.db.ExecContext(ctx, `
		INSERT INTO openmessage_whatsapp_history_sync_pending
		    (message_id, account_user, notification, notification_bytes, received_at_ns)
		SELECT ?, ?, ?, ?, ?
		WHERE (SELECT COUNT(*) FROM openmessage_whatsapp_history_sync_pending) < ?
		  AND (SELECT COALESCE(SUM(notification_bytes), 0) FROM openmessage_whatsapp_history_sync_pending) + ? <= ?
		ON CONFLICT(message_id) DO NOTHING`,
		string(messageID), accountUser, encoded, size, receivedAt.UnixNano(),
		q.maxRows, size, q.maxBytes,
	)
	if err != nil {
		return fmt.Errorf("queue WhatsApp history-sync notification: %w", err)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("queue WhatsApp history-sync notification: read rows affected: %w", err)
	}
	if inserted == 1 {
		q.queued.Add(1)
		return nil
	}
	var rows, bytes int64
	var already bool
	if err := q.db.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(SUM(notification_bytes), 0),
		       COALESCE(MAX(message_id = ?), 0)
		FROM openmessage_whatsapp_history_sync_pending`,
		string(messageID),
	).Scan(&rows, &bytes, &already); err != nil {
		return fmt.Errorf("queue WhatsApp history-sync notification: %w", err)
	}
	if already {
		return nil
	}
	return fmt.Errorf("%w: holding %d notifications and %d bytes (bounds %d and %d), refusing %d more bytes",
		errHistorySyncQueueFull, rows, bytes, q.maxRows, q.maxBytes, size)
}

// due lists the sequence numbers of the notifications whose next attempt is
// at or before now, oldest first.
func (q *historySyncQueue) due(now time.Time) ([]int64, error) {
	if q == nil || q.db == nil {
		return nil, errHistorySyncQueueUnavailable
	}
	rows, err := q.db.QueryContext(context.Background(), `
		SELECT seq FROM openmessage_whatsapp_history_sync_pending
		WHERE next_attempt_at_ns <= ? ORDER BY seq`,
		now.UnixNano())
	if err != nil {
		return nil, fmt.Errorf("list WhatsApp history-sync queue: %w", err)
	}
	defer rows.Close()
	var seqs []int64
	for rows.Next() {
		var seq int64
		if err := rows.Scan(&seq); err != nil {
			return nil, fmt.Errorf("list WhatsApp history-sync queue: %w", err)
		}
		seqs = append(seqs, seq)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list WhatsApp history-sync queue: %w", err)
	}
	return seqs, nil
}

// nextAttempt returns the earliest next attempt after now among the queued
// notifications, and false when none is scheduled after now.
func (q *historySyncQueue) nextAttempt(now time.Time) (time.Time, bool, error) {
	if q == nil || q.db == nil {
		return time.Time{}, false, errHistorySyncQueueUnavailable
	}
	var next sql.NullInt64
	if err := q.db.QueryRowContext(context.Background(), `
		SELECT MIN(next_attempt_at_ns) FROM openmessage_whatsapp_history_sync_pending
		WHERE next_attempt_at_ns > ?`,
		now.UnixNano(),
	).Scan(&next); err != nil {
		return time.Time{}, false, fmt.Errorf("read WhatsApp history-sync queue: %w", err)
	}
	if !next.Valid {
		return time.Time{}, false, nil
	}
	return time.Unix(0, next.Int64), true, nil
}

// errHistorySyncRowUnreadable reports a stored notification that no longer
// decodes; the pump drops it.
var errHistorySyncRowUnreadable = errors.New("stored WhatsApp history-sync notification does not decode")

// load returns one queued notification; ok is false when it is no longer
// queued. A row whose notification does not decode is returned with
// errHistorySyncRowUnreadable and its seq, so the caller can remove it.
func (q *historySyncQueue) load(seq int64) (queuedHistorySync, bool, error) {
	if q == nil || q.db == nil {
		return queuedHistorySync{}, false, errHistorySyncQueueUnavailable
	}
	row := queuedHistorySync{seq: seq}
	var messageID string
	var encoded []byte
	var receivedAtNS, committedAtNS int64
	err := q.db.QueryRowContext(context.Background(), `
		SELECT message_id, account_user, notification, received_at_ns, attempts, committed_at_ns
		FROM openmessage_whatsapp_history_sync_pending WHERE seq = ?`,
		seq,
	).Scan(&messageID, &row.accountUser, &encoded, &receivedAtNS, &row.attempts, &committedAtNS)
	if errors.Is(err, sql.ErrNoRows) {
		return queuedHistorySync{}, false, nil
	}
	if err != nil {
		return queuedHistorySync{}, false, fmt.Errorf("load WhatsApp history-sync notification %d: %w", seq, err)
	}
	row.messageID = watypes.MessageID(messageID)
	row.receivedAt = time.Unix(0, receivedAtNS)
	row.committed = committedAtNS != 0
	row.notification = &waE2E.HistorySyncNotification{}
	if err := proto.Unmarshal(encoded, row.notification); err != nil {
		return row, true, fmt.Errorf("%w: notification %d: %w", errHistorySyncRowUnreadable, seq, err)
	}
	return row, true, nil
}

// markCommitted records that a notification's chunk was committed or held, so
// a later pass sends its receipt without downloading it again.
func (q *historySyncQueue) markCommitted(seq int64, at time.Time) error {
	if q == nil || q.db == nil {
		return errHistorySyncQueueUnavailable
	}
	if _, err := q.db.ExecContext(context.Background(), `
		UPDATE openmessage_whatsapp_history_sync_pending
		SET committed_at_ns = ?, next_attempt_at_ns = 0, last_error = ''
		WHERE seq = ?`,
		at.UnixNano(), seq,
	); err != nil {
		return fmt.Errorf("mark WhatsApp history-sync notification %d committed: %w", seq, err)
	}
	return nil
}

// deferAttempt schedules a notification's next attempt and records why this one
// failed and how many attempts have counted.
func (q *historySyncQueue) deferAttempt(seq int64, attempts int, next time.Time, reason error) error {
	if q == nil || q.db == nil {
		return errHistorySyncQueueUnavailable
	}
	message := ""
	if reason != nil {
		message = reason.Error()
	}
	if _, err := q.db.ExecContext(context.Background(), `
		UPDATE openmessage_whatsapp_history_sync_pending
		SET attempts = ?, next_attempt_at_ns = ?, last_error = ?
		WHERE seq = ?`,
		attempts, next.UnixNano(), message, seq,
	); err != nil {
		return fmt.Errorf("defer WhatsApp history-sync notification %d: %w", seq, err)
	}
	return nil
}

// remove deletes a notification the pump is done with.
func (q *historySyncQueue) remove(seq int64) error {
	if q == nil || q.db == nil {
		return errHistorySyncQueueUnavailable
	}
	result, err := q.db.ExecContext(context.Background(),
		`DELETE FROM openmessage_whatsapp_history_sync_pending WHERE seq = ?`, seq)
	if err != nil {
		return fmt.Errorf("remove WhatsApp history-sync notification %d: %w", seq, err)
	}
	if deleted, err := result.RowsAffected(); err == nil && deleted == 1 {
		q.queued.Add(-1)
	}
	return nil
}
