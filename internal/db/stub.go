package db

import "strings"

// pendingStatusMarkers indicate a message whose content may still arrive (media
// downloading, message sending, etc.). An empty body for one of these is a
// placeholder, not a stub, so it must be kept.
var pendingStatusMarkers = []string{
	"DOWNLOADING", "UPLOADING", "SENDING", "YET_TO_SEND",
	"PROCESSING", "VALIDATING", "RESENDING", "QUEUED",
}

// IsEmptyStubMessage reports whether a message is a contentless "stub": no body,
// no media, and no reactions, with a terminal/complete status. These show up as
// "Empty message" in the thread (and wrongly surface conversations) and arise
// when group activity leaks an empty message into a 1:1 thread. Placeholders
// (still downloading/sending), system tombstones, and unknown-status messages
// are NOT stubs.
func IsEmptyStubMessage(m *Message) bool {
	if m == nil {
		return false
	}
	if strings.TrimSpace(m.Body) != "" ||
		strings.TrimSpace(m.MediaID) != "" ||
		strings.TrimSpace(m.Reactions) != "" {
		return false
	}
	status := strings.ToUpper(strings.TrimSpace(m.Status))
	if status == "" || strings.HasPrefix(status, "TOMBSTONE") {
		return false
	}
	for _, marker := range pendingStatusMarkers {
		if strings.Contains(status, marker) {
			return false
		}
	}
	return true
}

// emptyStubSQLPredicate is the SQL form of IsEmptyStubMessage, used for the bulk
// repair. Keep it in sync with IsEmptyStubMessage / pendingStatusMarkers above.
const emptyStubSQLPredicate = `
	TRIM(body) = '' AND TRIM(COALESCE(media_id,'')) = '' AND TRIM(COALESCE(reactions,'')) = ''
	AND TRIM(status) != ''
	AND upper(status) NOT LIKE 'TOMBSTONE%'
	AND upper(status) NOT LIKE '%DOWNLOADING%' AND upper(status) NOT LIKE '%UPLOADING%'
	AND upper(status) NOT LIKE '%SENDING%' AND upper(status) NOT LIKE '%YET_TO_SEND%'
	AND upper(status) NOT LIKE '%PROCESSING%' AND upper(status) NOT LIKE '%VALIDATING%'
	AND upper(status) NOT LIKE '%RESENDING%' AND upper(status) NOT LIKE '%QUEUED%'
`

// RepairEmptyStubMessages deletes existing empty-stub messages (in a single
// batched statement) and recomputes the recency of conversations they were
// inflating. Conversations whose last_message_ts sits above the deleted stubs
// (e.g. legitimately ahead from metadata) are left alone. Returns the count.
func (s *Store) RepairEmptyStubMessages() (int, error) {
	// Affected conversations and the newest stub timestamp in each, BEFORE delete.
	rows, err := s.db.Query(`SELECT conversation_id, MAX(timestamp_ms) FROM messages WHERE ` + emptyStubSQLPredicate + ` GROUP BY conversation_id`)
	if err != nil {
		return 0, err
	}
	maxStubTS := map[string]int64{}
	for rows.Next() {
		var conv string
		var ts int64
		if err := rows.Scan(&conv, &ts); err != nil {
			rows.Close()
			return 0, err
		}
		maxStubTS[conv] = ts
	}
	rows.Close()
	if len(maxStubTS) == 0 {
		return 0, nil
	}

	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	res, err := s.deleteMessages(tx, emptyStubSQLPredicate)
	if err != nil {
		return 0, err
	}
	// Recompute recency only where a stub was at/above the current recency.
	for conv, stubTS := range maxStubTS {
		if _, err := tx.Exec(`
			UPDATE conversations
			SET last_message_ts = COALESCE((SELECT MAX(timestamp_ms) FROM messages WHERE conversation_id = ?), 0)
			WHERE conversation_id = ? AND last_message_ts <= ?
		`, conv, conv, stubTS); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}
