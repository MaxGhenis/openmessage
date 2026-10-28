package whatsapplive

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// The ingress spool holds frames the bridge captured while a lifecycle adapter
// owns ingress but no generation committed them: the frame arrived after one
// generation stopped observing ingress and before the next started, or the
// observing generation was retiring (admission closed, stale generation) or
// its append failed. whatsmeow does not deliver those frames again. It
// dispatches a downloaded history-sync chunk from a background loop that
// ignores the handler's result and then deletes the server copy, and it
// acknowledges a receipt before dispatching it. So the bridge keeps each such
// frame here, durably, and replays it to the next generation that observes
// ingress (ObserveIngress), oldest first.
//
// The spool lives in the WhatsApp session database next to whatsmeow's own
// state. It is bounded by frame count and payload bytes; a frame that would
// take it past either bound is refused.
const ingressSpoolSchema = `
CREATE TABLE IF NOT EXISTS openmessage_whatsapp_ingress_spool (
    seq INTEGER PRIMARY KEY AUTOINCREMENT,
    dedupe_key TEXT NOT NULL UNIQUE,
    kind TEXT NOT NULL,
    received_at_ns INTEGER NOT NULL,
    spooled_at_ns INTEGER NOT NULL,
    payload_bytes INTEGER NOT NULL,
    payload BLOB NOT NULL
);
`

const (
	// ingressSpoolMaxFrames and ingressSpoolMaxBytes bound the spool. Frames
	// reach it only while generations change, when the socket is closed and
	// only in-flight handlers and whatsmeow's history-sync queue (32
	// notifications) still produce frames, so a gap fills a small fraction of
	// either bound. A spool that stays full means v2 has refused appends for
	// a long time.
	ingressSpoolMaxFrames = 4096
	ingressSpoolMaxBytes  = 256 << 20

	// ingressSpoolRetryInterval spaces the retries a commit on the same
	// generation triggers after a replay left frames held, so a frame the
	// generation keeps refusing is not reloaded on every message.
	ingressSpoolRetryInterval = 30 * time.Second
)

var (
	errIngressSpoolFull        = errors.New("whatsapp ingress spool is full")
	errIngressSpoolUnavailable = errors.New("whatsapp ingress spool is unavailable")
)

type ingressSpool struct {
	db        *sql.DB
	maxFrames int64
	maxBytes  int64
	now       func() time.Time

	// held counts the spool's rows so the hot path can skip a replay with one
	// atomic load. It is loaded from the table at open and moves only with a
	// row this process inserted or deleted.
	held atomic.Int64

	// replayMu admits one replay at a time, so frames replay oldest first.
	replayMu sync.Mutex
	// retryAfter (guarded by replayMu) defers commit-triggered retries after
	// a replay that left frames held.
	retryAfter time.Time
}

type spooledFrame struct {
	seq   int64
	kind  string
	frame IngressFrame
}

func openIngressSpool(database *sql.DB, maxFrames int64, maxBytes int64) (*ingressSpool, error) {
	if database == nil {
		return nil, errIngressSpoolUnavailable
	}
	if _, err := database.ExecContext(context.Background(), ingressSpoolSchema); err != nil {
		return nil, fmt.Errorf("initialize WhatsApp ingress spool: %w", err)
	}
	spool := &ingressSpool{db: database, maxFrames: maxFrames, maxBytes: maxBytes, now: time.Now}
	var held int64
	if err := database.QueryRowContext(
		context.Background(),
		`SELECT COUNT(*) FROM openmessage_whatsapp_ingress_spool`,
	).Scan(&held); err != nil {
		return nil, fmt.Errorf("count WhatsApp ingress spool: %w", err)
	}
	spool.held.Store(held)
	return spool, nil
}

// heldFrames reports how many frames the spool holds.
func (s *ingressSpool) heldFrames() int64 {
	if s == nil {
		return 0
	}
	return s.held.Load()
}

// hold stores frame durably. A frame whose dedupe key the spool already holds
// is not stored again: both commit as the same inbox row. A frame that would
// take the spool past either bound is refused with errIngressSpoolFull.
func (s *ingressSpool) hold(kind string, frame IngressFrame) error {
	if s == nil || s.db == nil {
		return errIngressSpoolUnavailable
	}
	if strings.TrimSpace(frame.DedupeKey) == "" {
		return errors.New("whatsapp ingress spool: frame has no dedupe key")
	}
	if frame.ReceivedAt.IsZero() {
		return errors.New("whatsapp ingress spool: frame has no receive time")
	}
	size := int64(len(frame.Payload))
	if size > s.maxBytes {
		return fmt.Errorf("%w: a %d-byte frame exceeds the %d-byte bound", errIngressSpoolFull, size, s.maxBytes)
	}
	payload := frame.Payload
	if payload == nil {
		payload = []byte{}
	}
	ctx := context.Background()
	// One statement, so the bound check and the insert are atomic.
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO openmessage_whatsapp_ingress_spool
		    (dedupe_key, kind, received_at_ns, spooled_at_ns, payload_bytes, payload)
		SELECT ?, ?, ?, ?, ?, ?
		WHERE (SELECT COUNT(*) FROM openmessage_whatsapp_ingress_spool) < ?
		  AND (SELECT COALESCE(SUM(payload_bytes), 0) FROM openmessage_whatsapp_ingress_spool) + ? <= ?
		ON CONFLICT(dedupe_key) DO NOTHING`,
		frame.DedupeKey, kind, frame.ReceivedAt.UnixNano(), s.now().UnixNano(), size, payload,
		s.maxFrames, size, s.maxBytes,
	)
	if err != nil {
		return fmt.Errorf("hold WhatsApp ingress frame: %w", err)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("hold WhatsApp ingress frame: read rows affected: %w", err)
	}
	if inserted == 1 {
		s.held.Add(1)
		return nil
	}
	var frames, bytes int64
	var already bool
	if err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(SUM(payload_bytes), 0),
		       COALESCE(MAX(dedupe_key = ?), 0)
		FROM openmessage_whatsapp_ingress_spool`,
		frame.DedupeKey,
	).Scan(&frames, &bytes, &already); err != nil {
		return fmt.Errorf("hold WhatsApp ingress frame: %w", err)
	}
	if already {
		return nil
	}
	return fmt.Errorf("%w: holding %d frames and %d bytes (bounds %d and %d), refusing %d more bytes",
		errIngressSpoolFull, frames, bytes, s.maxFrames, s.maxBytes, size)
}

// sequence lists the held frames' sequence numbers, oldest first.
func (s *ingressSpool) sequence() ([]int64, error) {
	if s == nil || s.db == nil {
		return nil, errIngressSpoolUnavailable
	}
	rows, err := s.db.QueryContext(context.Background(),
		`SELECT seq FROM openmessage_whatsapp_ingress_spool ORDER BY seq`)
	if err != nil {
		return nil, fmt.Errorf("list WhatsApp ingress spool: %w", err)
	}
	defer rows.Close()
	var seqs []int64
	for rows.Next() {
		var seq int64
		if err := rows.Scan(&seq); err != nil {
			return nil, fmt.Errorf("list WhatsApp ingress spool: %w", err)
		}
		seqs = append(seqs, seq)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list WhatsApp ingress spool: %w", err)
	}
	return seqs, nil
}

// load returns one held frame; ok is false when it is no longer held.
func (s *ingressSpool) load(seq int64) (held spooledFrame, ok bool, err error) {
	if s == nil || s.db == nil {
		return spooledFrame{}, false, errIngressSpoolUnavailable
	}
	var receivedAtNS int64
	held.seq = seq
	err = s.db.QueryRowContext(context.Background(), `
		SELECT dedupe_key, kind, received_at_ns, payload
		FROM openmessage_whatsapp_ingress_spool WHERE seq = ?`,
		seq,
	).Scan(&held.frame.DedupeKey, &held.kind, &receivedAtNS, &held.frame.Payload)
	if errors.Is(err, sql.ErrNoRows) {
		return spooledFrame{}, false, nil
	}
	if err != nil {
		return spooledFrame{}, false, fmt.Errorf("load WhatsApp ingress frame %d: %w", seq, err)
	}
	held.frame.ReceivedAt = time.Unix(0, receivedAtNS)
	return held, true, nil
}

// release deletes a held frame once a generation committed it.
func (s *ingressSpool) release(seq int64) error {
	if s == nil || s.db == nil {
		return errIngressSpoolUnavailable
	}
	result, err := s.db.ExecContext(context.Background(),
		`DELETE FROM openmessage_whatsapp_ingress_spool WHERE seq = ?`, seq)
	if err != nil {
		return fmt.Errorf("release WhatsApp ingress frame %d: %w", seq, err)
	}
	if deleted, err := result.RowsAffected(); err == nil && deleted == 1 {
		s.held.Add(-1)
	}
	return nil
}
