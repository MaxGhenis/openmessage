package db

import (
	"database/sql"
	"errors"
	"fmt"
)

// Scheduled-message statuses.
const (
	ScheduleStatusPending  = "pending"
	ScheduleStatusSending  = "sending"
	ScheduleStatusSent     = "sent"
	ScheduleStatusFailed   = "failed"
	ScheduleStatusCanceled = "canceled"
)

const (
	scheduleMinLeadMS  = 10_000                    // must be at least 10s out
	scheduleMaxAheadMS = 365 * 24 * 60 * 60 * 1000 // at most ~1 year out
)

// ScheduledMessage is a message queued to send at a future time.
type ScheduledMessage struct {
	ID             string `json:"id"`
	ConversationID string `json:"conversation_id"`
	Body           string `json:"body"`
	ReplyToID      string `json:"reply_to_id,omitempty"`
	SendAt         int64  `json:"send_at"`
	Status         string `json:"status"`
	Attempts       int    `json:"attempts"`
	LastError      string `json:"last_error,omitempty"`
	CreatedAt      int64  `json:"created_at"`
	SentMessageID  string `json:"sent_message_id,omitempty"`
	// Media fields. MediaData (the blob) is only set on create and loaded via
	// GetScheduledMediaData — it is never returned by Get/List/GetDue.
	MediaData     []byte `json:"-"`
	MediaFilename string `json:"media_filename,omitempty"`
	MediaMime     string `json:"media_mime,omitempty"`
}

// ValidateScheduleTime checks a requested send time against "now" (both epoch
// ms): it must be comfortably in the future and not absurdly far out.
func ValidateScheduleTime(sendAt, now int64) error {
	if sendAt <= 0 {
		return fmt.Errorf("a send time is required")
	}
	if sendAt < now+scheduleMinLeadMS {
		return fmt.Errorf("scheduled time must be in the future")
	}
	if sendAt > now+scheduleMaxAheadMS {
		return fmt.Errorf("scheduled time is too far in the future")
	}
	return nil
}

// scheduledColumns deliberately omits media_data so list/get/due queries never
// pull the blob; load it separately via GetScheduledMediaData.
const scheduledColumns = `id, conversation_id, body, reply_to_id, send_at, status, attempts, last_error, created_at, sent_message_id, media_filename, media_mime`

func scanScheduled(s interface{ Scan(...any) error }) (*ScheduledMessage, error) {
	m := &ScheduledMessage{}
	if err := s.Scan(&m.ID, &m.ConversationID, &m.Body, &m.ReplyToID, &m.SendAt, &m.Status, &m.Attempts, &m.LastError, &m.CreatedAt, &m.SentMessageID, &m.MediaFilename, &m.MediaMime); err != nil {
		return nil, err
	}
	return m, nil
}

// CreateScheduledMessage stores a new pending scheduled message.
func (s *Store) CreateScheduledMessage(m *ScheduledMessage) error {
	if m.Status == "" {
		m.Status = ScheduleStatusPending
	}
	var mediaData any
	if len(m.MediaData) > 0 {
		mediaData = m.MediaData
	}
	_, err := s.db.Exec(`
		INSERT INTO scheduled_messages (id, conversation_id, body, reply_to_id, send_at, status, attempts, last_error, created_at, sent_message_id, media_data, media_filename, media_mime)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, m.ID, m.ConversationID, m.Body, m.ReplyToID, m.SendAt, m.Status, m.Attempts, m.LastError, m.CreatedAt, m.SentMessageID, mediaData, m.MediaFilename, m.MediaMime)
	return err
}

// GetScheduledMediaData loads just the media blob for a scheduled message. The
// scheduler calls this at send time so the blob never travels through the
// list/get paths. Returns (nil, nil) when there is no media.
func (s *Store) GetScheduledMediaData(id string) ([]byte, error) {
	var data []byte
	err := s.db.QueryRow(`SELECT media_data FROM scheduled_messages WHERE id = ?`, id).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return data, err
}

// GetScheduledMessage returns one by ID, or (nil, nil) if not found.
func (s *Store) GetScheduledMessage(id string) (*ScheduledMessage, error) {
	row := s.db.QueryRow(`SELECT `+scheduledColumns+` FROM scheduled_messages WHERE id = ?`, id)
	m, err := scanScheduled(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return m, err
}

// ListScheduledMessages returns the active (pending/sending/failed) scheduled
// messages for a conversation, oldest send time first. Sent/canceled ones are
// excluded — they're done.
func (s *Store) ListScheduledMessages(conversationID string) ([]*ScheduledMessage, error) {
	rows, err := s.db.Query(`
		SELECT `+scheduledColumns+` FROM scheduled_messages
		WHERE conversation_id = ? AND status IN (?, ?, ?)
		ORDER BY send_at ASC
	`, conversationID, ScheduleStatusPending, ScheduleStatusSending, ScheduleStatusFailed)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectScheduled(rows)
}

// GetDueScheduledMessages returns pending messages whose time has arrived.
func (s *Store) GetDueScheduledMessages(now int64) ([]*ScheduledMessage, error) {
	rows, err := s.db.Query(`
		SELECT `+scheduledColumns+` FROM scheduled_messages
		WHERE status = ? AND send_at <= ?
		ORDER BY send_at ASC
	`, ScheduleStatusPending, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectScheduled(rows)
}

func collectScheduled(rows *sql.Rows) ([]*ScheduledMessage, error) {
	var out []*ScheduledMessage
	for rows.Next() {
		m, err := scanScheduled(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ClaimScheduledMessage atomically transitions pending -> sending. It returns
// true only for the caller that won the claim, guaranteeing exactly-once sends.
func (s *Store) ClaimScheduledMessage(id string) (bool, error) {
	res, err := s.db.Exec(`UPDATE scheduled_messages SET status = ? WHERE id = ? AND status = ?`,
		ScheduleStatusSending, id, ScheduleStatusPending)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// MarkScheduledMessageSent records a successful send.
func (s *Store) MarkScheduledMessageSent(id, sentMessageID string) error {
	_, err := s.db.Exec(`UPDATE scheduled_messages SET status = ?, sent_message_id = ?, last_error = '' WHERE id = ?`,
		ScheduleStatusSent, sentMessageID, id)
	return err
}

// MarkScheduledMessageFailed records a permanent failure (and counts the attempt).
func (s *Store) MarkScheduledMessageFailed(id, errMsg string) error {
	_, err := s.db.Exec(`UPDATE scheduled_messages SET status = ?, last_error = ?, attempts = attempts + 1 WHERE id = ?`,
		ScheduleStatusFailed, errMsg, id)
	return err
}

// RevertScheduledMessageToPending puts a claimed message back to pending so it
// is retried later (e.g. the platform was offline). Counts the attempt.
func (s *Store) RevertScheduledMessageToPending(id, errMsg string) error {
	_, err := s.db.Exec(`UPDATE scheduled_messages SET status = ?, last_error = ?, attempts = attempts + 1 WHERE id = ?`,
		ScheduleStatusPending, errMsg, id)
	return err
}

// CancelScheduledMessage cancels a still-pending message. Returns false if it
// wasn't pending (already sending/sent/canceled).
func (s *Store) CancelScheduledMessage(id string) (bool, error) {
	res, err := s.db.Exec(`UPDATE scheduled_messages SET status = ? WHERE id = ? AND status = ?`,
		ScheduleStatusCanceled, id, ScheduleStatusPending)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// DeleteScheduledMessage removes a scheduled-message row entirely.
func (s *Store) DeleteScheduledMessage(id string) error {
	_, err := s.db.Exec(`DELETE FROM scheduled_messages WHERE id = ?`, id)
	return err
}
