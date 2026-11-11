package db

// signalDuplicateRanks ranks the Signal rows that share a conversation, sender
// and timestamp, best first: most reactions, then longest body, then lowest
// message_id. Before v0.2.9 the Signal message_id hash included the body, so
// one message could be stored twice; (conversation, sender, timestamp) is its
// true identity.
const signalDuplicateRanks = `
	SELECT
		message_id,
		ROW_NUMBER() OVER (
			PARTITION BY conversation_id, sender_number, timestamp_ms
			ORDER BY length(reactions) DESC, length(body) DESC, message_id ASC
		) AS rn
	FROM messages
	WHERE source_platform = 'signal' AND timestamp_ms > 0`

// dedupeSignalMessages keeps the best-ranked row of each group of Signal rows
// that share a conversation, sender and timestamp, and deletes the rest with
// their search index entries. With nothing to delete, which is the usual case,
// it only reads: the Signal Desktop importer skips any message this would
// delete (see SignalDuplicateWouldBeDropped).
func (s *Store) dedupeSignalMessages() error {
	const duplicates = `message_id IN (SELECT message_id FROM (` + signalDuplicateRanks + `) WHERE rn > 1)`
	// Reads idx_messages_signal_identity in group order; ranking the rows, as
	// the delete below does, would sort every Signal message on every open.
	var found bool
	if err := s.db.QueryRow(`
		SELECT EXISTS (
			SELECT 1 FROM messages
			WHERE source_platform = 'signal' AND timestamp_ms > 0
			GROUP BY conversation_id, sender_number, timestamp_ms
			HAVING count(*) > 1
		)`).Scan(&found); err != nil {
		return err
	}
	if !found {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := s.deleteMessages(tx, duplicates); err != nil {
		return err
	}
	return tx.Commit()
}

// SignalDuplicateWouldBeDropped reports whether a new row for m would lose to
// a stored row in dedupeSignalMessages, which would delete it at the next
// store open. Callers check it before inserting a message under an ID the
// store doesn't hold yet. It compares lengths in SQL, as the ranking does, so
// both count characters the same way.
//
// The store still holds some Signal messages under IDs from before v0.2.9,
// whose hash included the body. The importer derives another ID for each, and
// when the stored row ranks first (often only on the message_id tie-break),
// the cleanup deletes the imported copy. Skipping such a copy keeps every
// periodic full rescan from inserting rows that the next store open deletes
// again.
func (s *Store) SignalDuplicateWouldBeDropped(m *Message) (bool, error) {
	if m.SourcePlatform != "signal" || m.TimestampMS <= 0 {
		return false, nil
	}
	var dropped bool
	err := s.db.QueryRow(`
		SELECT EXISTS (
			SELECT 1 FROM messages
			WHERE source_platform = 'signal'
				AND conversation_id = ? AND sender_number = ? AND timestamp_ms = ?
				AND message_id != ?
				AND (length(reactions) > length(?)
					OR (length(reactions) = length(?)
						AND (length(body) > length(?)
							OR (length(body) = length(?) AND message_id < ?))))
		)`,
		m.ConversationID, m.SenderNumber, m.TimestampMS,
		m.MessageID,
		m.Reactions, m.Reactions,
		m.Body, m.Body, m.MessageID,
	).Scan(&dropped)
	return dropped, err
}
