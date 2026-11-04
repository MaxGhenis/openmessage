package db

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/maxghenis/openmessage/internal/phoneline"
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
	// Most calls find nothing to merge, so look before beginning: a write
	// transaction starts with BEGIN IMMEDIATE and waits for any other
	// process's writer.
	var exists bool
	if err := s.db.QueryRow(`SELECT EXISTS (SELECT 1 FROM conversations WHERE conversation_id = ?)`, sourceID).Scan(&exists); err != nil || !exists {
		return err
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

	if _, err := s.deleteMessages(tx, `conversation_id = ?`, id); err != nil {
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
	tab, err := s.validateConversationTab(tab)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`UPDATE conversations SET tab = ? WHERE conversation_id = ?`, tab, id)
	return err
}

// SetConversationsTab moves multiple conversations into the given tab in one transaction.
func (s *Store) SetConversationsTab(ids []string, tab string) error {
	if len(ids) == 0 {
		return nil
	}
	tab, err := s.validateConversationTab(tab)
	if err != nil {
		return err
	}
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

func (s *Store) validateConversationTab(tab string) (string, error) {
	tab = strings.TrimSpace(tab)
	switch tab {
	case TabInbox, TabArchive:
		return tab, nil
	}
	var exists int
	err := s.db.QueryRow(`SELECT 1 FROM tabs WHERE tab_id = ?`, tab).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("unknown conversation tab %q", tab)
	}
	if err != nil {
		return "", err
	}
	return tab, nil
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

// SearchConversationsByMetadata matches a conversation by its name, a
// participant entry, or the sender of a message in it. A participant entry
// matches through its Texts, as owner.go's ParticipantEntries reads them:
// the text of its name, number, phone or email, or, for a bare string, the
// string; JSON keys, flags and IDs match nothing, and escapes are decoded.
// Most threads list the account's owner, so a participant match skips the
// owner's entries, or the owner's name or number would find every thread.
// The owner rule in owner.go names those entries: an object that carries
// is_me (or isMe), and any entry, in any thread, on the phone line of a
// flagged entry whose number reads as a phone number or a WhatsApp JID of
// one. Google's participant lists often repeat the owner without the flag.
// The owner's outgoing messages carry the owner's name and number too, so
// sender matches count incoming messages only. The SQL skips both before its
// LIMIT, so threads that match only through the owner never crowd out a
// match. Go reads every list, through the functions owner.go registers, so
// the search skips the entries send_message skips. The query matches as
// written, as in v2's search: % and _ match only themselves. LIKE folds the
// case of ASCII letters alone, and it reads a text only up to a NUL. Threads
// that share a time come in conversation ID order, as v2's do, so a limit
// keeps the same threads whichever store serves the reads.
func (s *Store) SearchConversationsByMetadata(query string, limit int) ([]*Conversation, error) {
	pattern := "%" + escapeLikePattern(query) + "%"
	rows, err := s.db.Query(`
		WITH `+sqlOwnerKeys()+`
		SELECT DISTINCT `+conversationColumns+`
		FROM conversations
		WHERE name LIKE ? ESCAPE '\'
			OR EXISTS (
				SELECT 1
				FROM `+sqlParticipantEntries("conversations.participants")+` AS p,
					json_each(p.value, '$.texts') AS entry_text
				WHERE NOT `+sqlEntryIsOwners+` AND entry_text.value LIKE ? ESCAPE '\'
			)
			OR conversation_id IN (
				SELECT DISTINCT conversation_id
				FROM messages
				WHERE is_from_me = 0 AND (sender_name LIKE ? ESCAPE '\' OR sender_number LIKE ? ESCAPE '\')
			)
			OR conversation_id IN (
				SELECT DISTINCT m.conversation_id
				FROM messages m
				JOIN contacts c ON c.number = m.sender_number
				WHERE m.is_from_me = 0 AND (c.name LIKE ? ESCAPE '\' OR c.number LIKE ? ESCAPE '\')
			)
		ORDER BY last_message_ts DESC, conversation_id
		LIMIT ?
	`, pattern, pattern, pattern, pattern, pattern, pattern, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanConversations(rows)
}

// SearchConversationsByPhoneLine returns the conversations with someone on
// the phone line that line, a phoneline.Line, names, newest first, then in
// conversation ID order, and at most limit. Someone is a participant entry
// that the owner rule in owner.go doesn't count as the owner's, through its
// number (or, without one, its phone) or, for a bare string, the string; or
// the sender of an incoming message, whom the roster may lack. The rule skips
// flagged entries and the unflagged copies of the owner that Google's lists
// hold, so the owner's own number finds no thread through its participants.
// A value counts only where it reads as a phone number or a WhatsApp JID of
// one, and then matches as send_message matches a recipient, through
// phoneline, so separators and a country prefix don't matter. Go reads every
// list, through the functions owner.go registers, and phoneline reads each
// value whole, NULs included. The match runs before the LIMIT, so numbers
// that only contain the line's digits never crowd out one that ends with
// them.
func (s *Store) SearchConversationsByPhoneLine(line string, limit int) ([]*Conversation, error) {
	rows, err := s.db.Query(phoneLineSearchSQL(phoneline.SQLMatch), line, line, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanConversations(rows)
}

// phoneLineSearchSQL is SearchConversationsByPhoneLine's query, calling the
// SQL function match to test a value against the line. It takes the line
// twice, then the limit. Each value reaches match as a BLOB, since the
// driver cuts a TEXT argument at its first NUL. A thread can hold thousands
// of messages from a few numbers, so match tests each distinct sender number
// once, in sender_numbers, and the messages from the numbers on the line
// then give their threads. SQLite pushes no WHERE term down into a
// MATERIALIZED CTE, so match never reaches the scan of messages.
func phoneLineSearchSQL(match string) string {
	return `
		WITH ` + sqlOwnerKeys() + `,
		sender_numbers AS MATERIALIZED (
			SELECT DISTINCT sender_number FROM messages
		)
		SELECT ` + conversationColumns + `
		FROM conversations
		WHERE EXISTS (
				SELECT 1
				FROM ` + sqlParticipantEntries("conversations.participants") + ` AS p
				WHERE NOT ` + sqlEntryIsOwners + ` AND ` + match + `(?, CAST(json_extract(p.value, '$.number') AS BLOB))
			)
			OR conversation_id IN (
				SELECT conversation_id
				FROM messages
				WHERE is_from_me = 0 AND sender_number IN (
					SELECT sender_number FROM sender_numbers WHERE ` + match + `(?, CAST(sender_number AS BLOB))
				)
			)
		ORDER BY last_message_ts DESC, conversation_id
		LIMIT ?
	`
}

// SearchConversationsByAddress returns the conversations with someone whose
// address is address as a whole, newest first, then in conversation ID
// order, and at most limit. Someone is a participant entry not flagged as
// the owner's, through the text of its number, phone or email, a bare
// string entry, or the sender of an incoming message, whom the roster may
// lack. The comparison ignores ASCII letters' case and nothing else, as
// SQLite's NOCASE does, so an email address matches however its letters are
// cased. A value that only contains address, a name, or a title matches
// nothing: the legacy Google Chat importer files an email address as a
// number, SMS threads list a short code as one, and another person's
// address or phone number can hold either. The match runs before the
// LIMIT. An empty address matches nothing.
func (s *Store) SearchConversationsByAddress(address string, limit int) ([]*Conversation, error) {
	if strings.TrimSpace(address) == "" || limit <= 0 {
		return []*Conversation{}, nil
	}
	rows, err := s.db.Query(addressSearchSQL, address, address, address, address, address, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanConversations(rows)
}

// addressSearchSQL is SearchConversationsByAddress's query. It takes the
// address five times, then the limit. NOCASE can't use the sender_number
// index, so the query compares each distinct sender number once, in
// sender_numbers, and then finds the matching numbers' messages through the
// index.
var addressSearchSQL = `
	WITH sender_numbers AS MATERIALIZED (
		SELECT DISTINCT sender_number FROM messages
	)
	SELECT ` + conversationColumns + `
	FROM conversations
	WHERE EXISTS (
			SELECT 1
			FROM ` + sqlRawParticipantEntries("conversations.participants") + ` AS p
			WHERE CASE p.type
				WHEN 'text' THEN p.value = ? COLLATE NOCASE
				WHEN 'object' THEN NOT ` + sqlOwnerFlag + ` AND (
					` + sqlEntryText("number") + ` = ? COLLATE NOCASE
					OR ` + sqlEntryText("phone") + ` = ? COLLATE NOCASE
					OR ` + sqlEntryText("email") + ` = ? COLLATE NOCASE
				)
				ELSE 0
			END
		)
		OR conversation_id IN (
			SELECT conversation_id
			FROM messages
			WHERE is_from_me = 0 AND sender_number IN (
				SELECT sender_number FROM sender_numbers WHERE sender_number = ? COLLATE NOCASE
			)
		)
	ORDER BY last_message_ts DESC, conversation_id
	LIMIT ?
`

// sqlOwnerFlag holds for an object entry that carries is_me or isMe. p is a
// row of sqlRawParticipantEntries. Only a JSON true counts,
// and json_type gives NULL for a missing key, hence IS.
const sqlOwnerFlag = `(json_type(p.value, '$.is_me') IS 'true' OR json_type(p.value, '$.isMe') IS 'true')`

// sqlEntryText is an object entry's text under key, or NULL when the key is
// missing or holds anything but text. p is a row of
// sqlRawParticipantEntries.
func sqlEntryText(key string) string {
	return `CASE json_type(p.value, '$.` + key + `') WHEN 'text' THEN json_extract(p.value, '$.` + key + `') END`
}

// sqlRawParticipantEntries is the json_each source for the elements of a
// participants column as SQLite's JSON reader reads them: the elements of a
// JSON array, and none for anything else. The JSON functions fail on
// malformed JSON, such as an empty string, and SQLite can evaluate both sides
// of an AND, so CASE guards them here and on p.value. Only the address
// search reads lists this way.
func sqlRawParticipantEntries(column string) string {
	return `json_each(CASE WHEN json_valid(` + column + `) THEN CASE json_type(` + column + `) WHEN 'array' THEN ` + column + ` ELSE '[]' END ELSE '[]' END)`
}

// sqlOwnerKeys is a common table expression for a WITH clause, owner_keys:
// a row for each of the phoneline.LineKeys of the owner's lines, as owner.go's
// OwnerLine gives them, from every conversation. SQLite fills a MATERIALIZED
// table once for each query.
func sqlOwnerKeys() string {
	return `owner_keys(owner_key) AS MATERIALIZED (
			SELECT DISTINCT line_key.value
			FROM conversations AS flagged, ` + sqlOwnerLineKeys("flagged.participants") + ` AS line_key
		)`
}

// sqlOwnerLineKeys is the json_each source for the keys of the owner's lines
// that the entries of a participants column give, as
// openmessage_owner_line_keys reads them.
func sqlOwnerLineKeys(column string) string {
	return `json_each(openmessage_owner_line_keys(CAST(` + column + ` AS BLOB)))`
}

// sqlEntryIsOwners holds for an entry that OwnerLines.Owner counts as the
// owner's, given the keys of the owner's lines in owner_keys (see
// sqlOwnerKeys): it carries the flag, or one of its number's keys names one
// of those lines. owner_keys doesn't depend on the entry, so SQLite reads it
// into an index once for each query. p is a row of sqlParticipantEntries.
const sqlEntryIsOwners = `(json_extract(p.value, '$.is_me') OR EXISTS (
	SELECT 1
	FROM json_each(p.value, '$.number_keys') AS number_key
	WHERE number_key.value IN (SELECT owner_key FROM owner_keys)
))`

// sqlParticipantEntries is the json_each source for the entries of a
// participants column: a row for each entry owner.go's ParticipantEntries
// gives, in order, whose value is the object openmessage_participant_entries
// writes for it. The column reaches Go whole, as a BLOB, so SQLite's JSON
// reader never reads it.
func sqlParticipantEntries(column string) string {
	return `json_each(openmessage_participant_entries(CAST(` + column + ` AS BLOB)))`
}

// escapeLikePattern makes user text match literally inside a LIKE pattern
// that declares '\' as its escape character.
func escapeLikePattern(text string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(text)
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
