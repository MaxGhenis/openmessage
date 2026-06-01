package db

import "database/sql"

type LegacyRepairReport struct {
	DeletedWhatsAppReactionPlaceholders int
	DeletedWhatsAppUnsupportedRows      int
	DeletedSignalReactionPlaceholders   int
	FixedSignalBlankMessages            int
	RemainingWhatsAppMediaPlaceholders  int
	FixedGoogleOutgoingAttributionRows  int
}

const legacyWhatsAppReactionPlaceholderWhere = `
	source_platform = 'whatsapp'
	AND body = '[Reaction]'
	AND IFNULL(media_id, '') = ''
	AND IFNULL(mime_type, '') = ''
	AND IFNULL(reactions, '') = ''
	AND IFNULL(reply_to_id, '') = ''
	AND IFNULL(source_id, '') != ''
`

const legacyWhatsAppUnsupportedPlaceholderWhere = `
	source_platform = 'whatsapp'
	AND body = '[Unsupported message]'
	AND IFNULL(media_id, '') = ''
	AND IFNULL(mime_type, '') = ''
	AND IFNULL(reactions, '') = ''
	AND IFNULL(reply_to_id, '') = ''
	AND IFNULL(source_id, '') != ''
`

const legacySignalReactionPlaceholderWhere = `
	source_platform = 'signal'
	AND body = '[Reaction]'
	AND IFNULL(media_id, '') = ''
	AND IFNULL(mime_type, '') = ''
	AND IFNULL(reactions, '') = ''
	AND IFNULL(reply_to_id, '') = ''
	AND IFNULL(source_id, '') != ''
`

const legacySignalBlankMessageWhere = `
	source_platform = 'signal'
	AND IFNULL(TRIM(body), '') = ''
	AND IFNULL(media_id, '') = ''
	AND IFNULL(mime_type, '') = ''
	AND IFNULL(reactions, '') = ''
	AND IFNULL(reply_to_id, '') = ''
`

const repairedSignalBlankMessageBody = "[Unsupported Signal message]"

func (s *Store) RepairLegacyArtifacts() (LegacyRepairReport, error) {
	report := LegacyRepairReport{}

	tx, err := s.db.Begin()
	if err != nil {
		return report, err
	}
	defer tx.Rollback()

	affectedConversationIDs, err := selectStringColumnTx(tx, `
		SELECT DISTINCT conversation_id
		FROM messages
		WHERE `+legacyWhatsAppReactionPlaceholderWhere)
	if err != nil {
		return report, err
	}
	unsupportedConversationIDs, err := selectStringColumnTx(tx, `
		SELECT DISTINCT conversation_id
		FROM messages
		WHERE `+legacyWhatsAppUnsupportedPlaceholderWhere)
	if err != nil {
		return report, err
	}
	affectedConversationIDs = append(affectedConversationIDs, unsupportedConversationIDs...)
	signalConversationIDs, err := selectStringColumnTx(tx, `
		SELECT DISTINCT conversation_id
		FROM messages
		WHERE `+legacySignalReactionPlaceholderWhere)
	if err != nil {
		return report, err
	}
	affectedConversationIDs = append(affectedConversationIDs, signalConversationIDs...)

	result, err := tx.Exec(`
		DELETE FROM messages
		WHERE ` + legacyWhatsAppReactionPlaceholderWhere)
	if err != nil {
		return report, err
	}
	if deleted, err := result.RowsAffected(); err == nil {
		report.DeletedWhatsAppReactionPlaceholders = int(deleted)
	}

	unsupportedResult, err := tx.Exec(`
		DELETE FROM messages
		WHERE ` + legacyWhatsAppUnsupportedPlaceholderWhere)
	if err != nil {
		return report, err
	}
	if deleted, err := unsupportedResult.RowsAffected(); err == nil {
		report.DeletedWhatsAppUnsupportedRows = int(deleted)
	}

	signalResult, err := tx.Exec(`
		DELETE FROM messages
		WHERE ` + legacySignalReactionPlaceholderWhere)
	if err != nil {
		return report, err
	}
	if deleted, err := signalResult.RowsAffected(); err == nil {
		report.DeletedSignalReactionPlaceholders = int(deleted)
	}

	fixSignalBlankResult, err := tx.Exec(`
		UPDATE messages
		SET body = ?
		WHERE `+legacySignalBlankMessageWhere, repairedSignalBlankMessageBody)
	if err != nil {
		return report, err
	}
	if repaired, err := fixSignalBlankResult.RowsAffected(); err == nil {
		report.FixedSignalBlankMessages = int(repaired)
	}

	selfSenderName, selfSenderNumber, err := mostCommonOutgoingSMSSenderTx(tx)
	if err != nil {
		return report, err
	}
	fixResult, err := tx.Exec(`
		UPDATE messages
		SET
			is_from_me = 1,
			source_platform = CASE
				WHEN IFNULL(source_platform, '') = '' THEN 'sms'
				ELSE source_platform
			END,
			sender_name = CASE
				WHEN IFNULL(sender_name, '') = '' THEN ?
				ELSE sender_name
			END,
			sender_number = CASE
				WHEN IFNULL(sender_number, '') = '' THEN ?
				ELSE sender_number
			END
		WHERE conversation_id IN (
			SELECT conversation_id
			FROM conversations
			WHERE IFNULL(source_platform, '') = 'sms'
		)
			AND IFNULL(source_platform, '') IN ('', 'sms')
			AND is_from_me = 0
			AND status LIKE 'OUTGOING%'
	`, selfSenderName, selfSenderNumber)
	if err != nil {
		return report, err
	}
	if repaired, err := fixResult.RowsAffected(); err == nil {
		report.FixedGoogleOutgoingAttributionRows = int(repaired)
	}

	for _, conversationID := range affectedConversationIDs {
		if _, err := tx.Exec(`
			UPDATE conversations
			SET last_message_ts = COALESCE((
				SELECT MAX(timestamp_ms)
				FROM messages
				WHERE conversation_id = ?
			), 0)
			WHERE conversation_id = ?
		`, conversationID, conversationID); err != nil {
			return report, err
		}
	}

	if err := tx.Commit(); err != nil {
		return report, err
	}

	if (report.DeletedWhatsAppReactionPlaceholders > 0 ||
		report.DeletedWhatsAppUnsupportedRows > 0 ||
		report.DeletedSignalReactionPlaceholders > 0 ||
		report.FixedSignalBlankMessages > 0) && s.ftsEnabled {
		// Repair rewrites message bodies in place (blank → placeholder), which
		// doesn't change the row count, so the count-guarded rebuildFTS would
		// skip it. Force a full repopulation to pick up the new bodies.
		if err := s.forceRebuildFTS(); err != nil {
			return report, err
		}
	}

	if err := s.db.QueryRow(`
		SELECT COUNT(*)
		FROM messages
		WHERE source_platform = 'whatsapp'
			AND body IN ('[Photo]', '[Video]', '[Audio]', '[Voice note]')
			AND IFNULL(media_id, '') = ''
	`).Scan(&report.RemainingWhatsAppMediaPlaceholders); err != nil {
		return report, err
	}

	return report, nil
}

func mostCommonOutgoingSMSSenderTx(tx *sql.Tx) (string, string, error) {
	row := tx.QueryRow(`
		SELECT sender_name, sender_number
		FROM messages
		WHERE source_platform = 'sms'
			AND is_from_me = 1
			AND IFNULL(sender_name, '') != ''
			AND IFNULL(sender_number, '') != ''
		GROUP BY sender_name, sender_number
		ORDER BY COUNT(*) DESC
		LIMIT 1
	`)
	var name string
	var number string
	switch err := row.Scan(&name, &number); err {
	case nil:
		return name, number, nil
	case sql.ErrNoRows:
		return "", "", nil
	default:
		return "", "", err
	}
}

func selectStringColumnTx(tx *sql.Tx, query string, args ...any) ([]string, error) {
	rows, err := tx.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var values []string
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, rows.Err()
}
