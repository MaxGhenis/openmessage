package db

import (
	"database/sql"
	"fmt"
	"strings"
)

// conversationColumns is the canonical column list for SELECT queries on conversations.
const conversationColumns = `conversation_id, name, is_group, participants, last_message_ts, unread_count, source_platform, display_protocol, is_favorite, notification_mode, tab`

const (
	NotificationModeAll      = "all"
	NotificationModeMentions = "mentions"
	NotificationModeMuted    = "muted"
)

func explicitNotificationMode(mode string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "":
		return "", false
	case NotificationModeAll:
		return NotificationModeAll, true
	case "mention", NotificationModeMentions:
		return NotificationModeMentions, true
	case "mute", "muted", "none", "off":
		return NotificationModeMuted, true
	default:
		return NotificationModeAll, true
	}
}

func normalizeStoredNotificationMode(mode string) string {
	normalized, ok := explicitNotificationMode(mode)
	if !ok {
		return NotificationModeAll
	}
	return normalized
}

func parseNotificationMode(mode string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case NotificationModeAll:
		return NotificationModeAll, nil
	case NotificationModeMentions:
		return NotificationModeMentions, nil
	case NotificationModeMuted:
		return NotificationModeMuted, nil
	default:
		return "", fmt.Errorf("invalid notification mode %q", mode)
	}
}

func (s *Store) UpsertConversation(c *Conversation) error {
	if c.SourcePlatform == "" {
		c.SourcePlatform = "sms"
	}
	c.DisplayProtocol = normalizeDisplayProtocol(c.DisplayProtocol)
	notificationMode, hasNotificationMode := explicitNotificationMode(c.NotificationMode)
	_, err := s.db.Exec(`
			INSERT INTO conversations (conversation_id, name, is_group, participants, last_message_ts, unread_count, source_platform, display_protocol, is_favorite, notification_mode)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, COALESCE(NULLIF(?, ''), 'all'))
			ON CONFLICT(conversation_id) DO UPDATE SET
				name=excluded.name,
				is_group=excluded.is_group,
				participants=excluded.participants,
				last_message_ts=excluded.last_message_ts,
				unread_count=excluded.unread_count,
				source_platform=excluded.source_platform,
				display_protocol=CASE WHEN excluded.display_protocol != '' THEN excluded.display_protocol ELSE conversations.display_protocol END,
				is_favorite=CASE WHEN excluded.is_favorite THEN 1 ELSE conversations.is_favorite END,
				notification_mode=CASE WHEN ? != '' THEN ? ELSE conversations.notification_mode END
		`, c.ConversationID, c.Name, c.IsGroup, c.Participants, c.LastMessageTS, c.UnreadCount, c.SourcePlatform, c.DisplayProtocol, c.IsFavorite, notificationMode, maybeNotificationModeArg(hasNotificationMode, notificationMode), maybeNotificationModeArg(hasNotificationMode, notificationMode))
	return err
}

func (s *Store) GetConversation(id string) (*Conversation, error) {
	c := &Conversation{}
	err := s.db.QueryRow(`
		SELECT `+conversationColumns+`
		FROM conversations WHERE conversation_id = ?
		`, id).Scan(&c.ConversationID, &c.Name, &c.IsGroup, &c.Participants, &c.LastMessageTS, &c.UnreadCount, &c.SourcePlatform, &c.DisplayProtocol, &c.IsFavorite, &c.NotificationMode, &c.Tab)
	if err != nil {
		return nil, err
	}
	c.DisplayProtocol = normalizeDisplayProtocol(c.DisplayProtocol)
	c.NotificationMode = normalizeStoredNotificationMode(c.NotificationMode)
	return c, nil
}

func (s *Store) UpdateConversationTimestamp(id string, ts int64) error {
	_, err := s.db.Exec(`UPDATE conversations SET last_message_ts = ? WHERE conversation_id = ?`, ts, id)
	return err
}

func (s *Store) BumpConversationTimestamp(id string, ts int64) error {
	_, err := s.db.Exec(`
		UPDATE conversations
		SET last_message_ts = CASE
			WHEN last_message_ts < ? THEN ?
			ELSE last_message_ts
		END
		WHERE conversation_id = ?
	`, ts, ts, id)
	return err
}

func (s *Store) MergeConversationIDs(sourceID, targetID string) error {
	if sourceID == "" || targetID == "" || sourceID == targetID {
		return nil
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	source, err := getConversationTx(tx, sourceID)
	if err != nil {
		return err
	}
	if source == nil {
		return tx.Commit()
	}
	target, err := getConversationTx(tx, targetID)
	if err != nil {
		return err
	}

	if _, err := tx.Exec(`UPDATE messages SET conversation_id = ? WHERE conversation_id = ?`, targetID, sourceID); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE drafts SET conversation_id = ? WHERE conversation_id = ?`, targetID, sourceID); err != nil {
		return err
	}

	merged := mergeConversationRecords(source, target, targetID)
	if err := upsertConversationTx(tx, merged); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM conversations WHERE conversation_id = ?`, sourceID); err != nil {
		return err
	}

	return tx.Commit()
}

func (s *Store) DeleteConversation(id string) error {
	if strings.TrimSpace(id) == "" {
		return nil
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM messages WHERE conversation_id = ?`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM drafts WHERE conversation_id = ?`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM conversations WHERE conversation_id = ?`, id); err != nil {
		return err
	}

	return tx.Commit()
}

func (s *Store) MarkConversationRead(id string) error {
	_, err := s.db.Exec(`UPDATE conversations SET unread_count = 0 WHERE conversation_id = ?`, id)
	return err
}

func (s *Store) SetConversationNotificationMode(id, mode string) error {
	normalized, err := parseNotificationMode(mode)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`UPDATE conversations SET notification_mode = ? WHERE conversation_id = ?`, normalized, id)
	return err
}

// SetConversationFavorite stores the local favorite state for a thread.
func (s *Store) SetConversationFavorite(id string, favorite bool) error {
	if strings.TrimSpace(id) == "" {
		return sql.ErrNoRows
	}
	result, err := s.db.Exec(`UPDATE conversations SET is_favorite = ? WHERE conversation_id = ?`, favorite, id)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func normalizeDisplayProtocol(protocol string) string {
	switch strings.ToUpper(strings.TrimSpace(protocol)) {
	case "RCS":
		return "RCS"
	case "TEXT", "SMS", "SMS/MMS":
		return "Text"
	default:
		return ""
	}
}

func (s *Store) SetConversationDisplayProtocol(id, protocol string) error {
	normalized := normalizeDisplayProtocol(protocol)
	if strings.TrimSpace(id) == "" || normalized == "" {
		return nil
	}
	_, err := s.db.Exec(`UPDATE conversations SET display_protocol = ? WHERE conversation_id = ?`, normalized, id)
	return err
}

// Built-in tab identifiers. "" is the implicit Recent (inbox) tab.
const (
	TabInbox   = ""        // Recent threads
	TabArchive = "archive" // Archived threads
)

// SetConversationTab moves a single conversation into the given tab.
// An empty tab returns it to Recent (inbox).
func (s *Store) SetConversationTab(id, tab string) error {
	_, err := s.db.Exec(`UPDATE conversations SET tab = ? WHERE conversation_id = ?`, strings.TrimSpace(tab), id)
	return err
}

// SetConversationsTab moves multiple conversations into the given tab in one transaction.
func (s *Store) SetConversationsTab(ids []string, tab string) error {
	if len(ids) == 0 {
		return nil
	}
	tab = strings.TrimSpace(tab)
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	stmt, err := tx.Prepare(`UPDATE conversations SET tab = ? WHERE conversation_id = ?`)
	if err != nil {
		tx.Rollback()
		return err
	}
	defer stmt.Close()
	for _, id := range ids {
		if _, err := stmt.Exec(tab, id); err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) ListConversations(limit int) ([]*Conversation, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := s.db.Query(`
		WITH recent AS (
			SELECT conversation_id
			FROM conversations
			ORDER BY last_message_ts DESC
			LIMIT ?
		)
		SELECT `+conversationColumns+`
		FROM conversations
		WHERE conversation_id IN (SELECT conversation_id FROM recent)
			OR is_favorite = 1
		ORDER BY last_message_ts DESC
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanConversations(rows)
}

// ConversationCount returns the total number of conversations, optionally filtered by source platform.
func (s *Store) ConversationCount(sourcePlatform string) (int, error) {
	var count int
	var err error
	if sourcePlatform != "" {
		err = s.db.QueryRow(`SELECT COUNT(*) FROM conversations WHERE source_platform = ?`, sourcePlatform).Scan(&count)
	} else {
		err = s.db.QueryRow(`SELECT COUNT(*) FROM conversations`).Scan(&count)
	}
	return count, err
}

// ListConversationsByPlatform lists conversations filtered by source platform.
func (s *Store) ListConversationsByPlatform(platform string, limit int) ([]*Conversation, error) {
	rows, err := s.db.Query(`
		SELECT `+conversationColumns+`
		FROM conversations
		WHERE source_platform = ?
		ORDER BY last_message_ts DESC
		LIMIT ?
	`, platform, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanConversations(rows)
}

func (s *Store) SearchConversationsByMetadata(query string, limit int) ([]*Conversation, error) {
	rows, err := s.db.Query(`
		SELECT DISTINCT `+conversationColumns+`
		FROM conversations
		WHERE name LIKE ?
			OR participants LIKE ?
			OR conversation_id IN (
				SELECT DISTINCT conversation_id
				FROM messages
				WHERE sender_name LIKE ? OR sender_number LIKE ?
			)
			OR conversation_id IN (
				SELECT DISTINCT m.conversation_id
				FROM messages m
				JOIN contacts c ON c.number = m.sender_number
				WHERE c.name LIKE ? OR c.number LIKE ?
			)
		ORDER BY last_message_ts DESC
		LIMIT ?
	`, "%"+query+"%", "%"+query+"%", "%"+query+"%", "%"+query+"%", "%"+query+"%", "%"+query+"%", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanConversations(rows)
}

func scanConversations(rows interface {
	Next() bool
	Scan(...any) error
	Err() error
}) ([]*Conversation, error) {
	var convs []*Conversation
	for rows.Next() {
		c := &Conversation{}
		if err := rows.Scan(&c.ConversationID, &c.Name, &c.IsGroup, &c.Participants, &c.LastMessageTS, &c.UnreadCount, &c.SourcePlatform, &c.DisplayProtocol, &c.IsFavorite, &c.NotificationMode, &c.Tab); err != nil {
			return nil, err
		}
		c.DisplayProtocol = normalizeDisplayProtocol(c.DisplayProtocol)
		c.NotificationMode = normalizeStoredNotificationMode(c.NotificationMode)
		convs = append(convs, c)
	}
	return convs, rows.Err()
}

func getConversationTx(tx *sql.Tx, id string) (*Conversation, error) {
	c := &Conversation{}
	err := tx.QueryRow(`
		SELECT `+conversationColumns+`
		FROM conversations WHERE conversation_id = ?
	`, id).Scan(&c.ConversationID, &c.Name, &c.IsGroup, &c.Participants, &c.LastMessageTS, &c.UnreadCount, &c.SourcePlatform, &c.DisplayProtocol, &c.IsFavorite, &c.NotificationMode, &c.Tab)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	c.DisplayProtocol = normalizeDisplayProtocol(c.DisplayProtocol)
	c.NotificationMode = normalizeStoredNotificationMode(c.NotificationMode)
	return c, nil
}

func upsertConversationTx(tx *sql.Tx, c *Conversation) error {
	if c.SourcePlatform == "" {
		c.SourcePlatform = "sms"
	}
	c.DisplayProtocol = normalizeDisplayProtocol(c.DisplayProtocol)
	notificationMode, hasNotificationMode := explicitNotificationMode(c.NotificationMode)
	_, err := tx.Exec(`
			INSERT INTO conversations (conversation_id, name, is_group, participants, last_message_ts, unread_count, source_platform, display_protocol, is_favorite, notification_mode)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, COALESCE(NULLIF(?, ''), 'all'))
			ON CONFLICT(conversation_id) DO UPDATE SET
				name=excluded.name,
				is_group=excluded.is_group,
				participants=excluded.participants,
				last_message_ts=excluded.last_message_ts,
				unread_count=excluded.unread_count,
				source_platform=excluded.source_platform,
				display_protocol=CASE WHEN excluded.display_protocol != '' THEN excluded.display_protocol ELSE conversations.display_protocol END,
				is_favorite=CASE WHEN excluded.is_favorite THEN 1 ELSE conversations.is_favorite END,
				notification_mode=CASE WHEN ? != '' THEN ? ELSE conversations.notification_mode END
		`, c.ConversationID, c.Name, c.IsGroup, c.Participants, c.LastMessageTS, c.UnreadCount, c.SourcePlatform, c.DisplayProtocol, c.IsFavorite, notificationMode, maybeNotificationModeArg(hasNotificationMode, notificationMode), maybeNotificationModeArg(hasNotificationMode, notificationMode))
	return err
}

func mergeConversationRecords(source, target *Conversation, targetID string) *Conversation {
	if target == nil {
		merged := *source
		merged.ConversationID = targetID
		merged.DisplayProtocol = normalizeDisplayProtocol(merged.DisplayProtocol)
		merged.NotificationMode = normalizeStoredNotificationMode(merged.NotificationMode)
		return &merged
	}

	merged := *target
	if merged.Name == "" {
		merged.Name = source.Name
	}
	merged.IsGroup = merged.IsGroup || source.IsGroup
	if merged.Participants == "" || merged.Participants == "[]" {
		merged.Participants = source.Participants
	}
	if source.LastMessageTS > merged.LastMessageTS {
		merged.LastMessageTS = source.LastMessageTS
	}
	if source.UnreadCount > merged.UnreadCount {
		merged.UnreadCount = source.UnreadCount
	}
	merged.IsFavorite = merged.IsFavorite || source.IsFavorite
	if merged.SourcePlatform == "" {
		merged.SourcePlatform = source.SourcePlatform
	}
	if merged.DisplayProtocol == "" {
		merged.DisplayProtocol = normalizeDisplayProtocol(source.DisplayProtocol)
	} else {
		merged.DisplayProtocol = normalizeDisplayProtocol(merged.DisplayProtocol)
	}
	if normalizeStoredNotificationMode(merged.NotificationMode) == NotificationModeAll && normalizeStoredNotificationMode(source.NotificationMode) != NotificationModeAll {
		merged.NotificationMode = normalizeStoredNotificationMode(source.NotificationMode)
	} else {
		merged.NotificationMode = normalizeStoredNotificationMode(merged.NotificationMode)
	}
	return &merged
}

func maybeNotificationModeArg(hasMode bool, mode string) string {
	if !hasMode {
		return ""
	}
	return mode
}

// ApplyConversationSnapshot upserts a conversation from a platform-state
// snapshot (a Google Messages conversation event or a backfill page) while
// guarding local state against stale snapshots:
//
//   - last_message_ts never moves backward, matching the recency rule used
//     for messages (AdvanceConversationRecency).
//   - A snapshot may set the unread flag only when it carries something
//     newer than what's stored. The phone re-sends conversation state on
//     unrelated events, and a snapshot that is no newer than local state
//     must not resurrect unread on a conversation already read locally.
//     Clearing unread (the user read it on the phone) is always honored.
//
// Live-transport handlers that compute unread deltas themselves (WhatsApp,
// Signal) should keep using UpsertConversation directly.
func (s *Store) ApplyConversationSnapshot(c *Conversation) error {
	existing, err := s.GetConversation(c.ConversationID)
	if err == nil && existing != nil {
		snapshotTS := c.LastMessageTS
		if snapshotTS < existing.LastMessageTS {
			c.LastMessageTS = existing.LastMessageTS
		}
		if c.UnreadCount > 0 && existing.UnreadCount == 0 && snapshotTS <= existing.LastMessageTS {
			c.UnreadCount = 0
		}
	}
	return s.UpsertConversation(c)
}
