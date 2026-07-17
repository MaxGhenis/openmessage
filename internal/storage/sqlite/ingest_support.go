package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ApplyMessageMutation updates an existing normalized message and marks the
// mutation's durable source frame processed in the same transaction. updated
// reports whether the target message existed. A missing target is a benign
// no-op, but the source frame is still marked processed.
func (r *MessageRepository) ApplyMessageMutation(
	ctx context.Context,
	accountID string,
	conversationID string,
	remoteMessageID string,
	kind string,
	body string,
	inboxID string,
) (updated bool, err error) {
	if kind != "edit" && kind != "delete" {
		return false, fmt.Errorf("apply message mutation: unsupported kind %q", kind)
	}

	tx, err := r.store.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf(
			"apply %s mutation to message (%q, %q, %q): begin transaction: %w",
			kind,
			accountID,
			conversationID,
			remoteMessageID,
			err,
		)
	}
	defer tx.Rollback()

	var inboxAccountID string
	if err := tx.QueryRowContext(ctx, `
		SELECT account_id
		FROM inbox
		WHERE inbox_id = ?
	`, inboxID).Scan(&inboxAccountID); errors.Is(err, sql.ErrNoRows) {
		return false, invalidMessageError(
			orphanMessageError(ErrOrphanMessageInbox),
			"mutation source inbox %q does not exist",
			inboxID,
		)
	} else if err != nil {
		return false, fmt.Errorf(
			"apply %s mutation to message (%q, %q, %q): read inbox %q: %w",
			kind,
			accountID,
			conversationID,
			remoteMessageID,
			inboxID,
			err,
		)
	}
	if inboxAccountID != accountID {
		return false, invalidMessageError(
			ErrCrossAccountMessage,
			"mutation source inbox %q account %q does not match message account %q",
			inboxID,
			inboxAccountID,
			accountID,
		)
	}

	nowMS, err := r.nowMS("apply message mutation")
	if err != nil {
		return false, err
	}

	var result sql.Result
	switch kind {
	case "edit":
		result, err = tx.ExecContext(ctx, `
			UPDATE messages
			SET body = ?, state = 'edited', updated_at_ms = ?
			WHERE account_id = ?
			  AND conversation_id = ?
			  AND remote_message_id = ?
		`, body, nowMS, accountID, conversationID, remoteMessageID)
	case "delete":
		result, err = tx.ExecContext(ctx, `
			UPDATE messages
			SET state = 'deleted', updated_at_ms = ?
			WHERE account_id = ?
			  AND conversation_id = ?
			  AND remote_message_id = ?
		`, nowMS, accountID, conversationID, remoteMessageID)
	}
	if err != nil {
		if isSQLiteConstraint(err) {
			return false, invalidMessageConstraintError(
				nil,
				err,
				"apply %s mutation to message (%q, %q, %q)",
				kind,
				accountID,
				conversationID,
				remoteMessageID,
			)
		}
		return false, fmt.Errorf(
			"apply %s mutation to message (%q, %q, %q): %w",
			kind,
			accountID,
			conversationID,
			remoteMessageID,
			err,
		)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf(
			"apply %s mutation to message (%q, %q, %q): read rows affected: %w",
			kind,
			accountID,
			conversationID,
			remoteMessageID,
			err,
		)
	}
	if affected != 0 && affected != 1 {
		return false, fmt.Errorf(
			"apply %s mutation to message (%q, %q, %q): affected %d rows, want at most 1",
			kind,
			accountID,
			conversationID,
			remoteMessageID,
			affected,
		)
	}

	if err := markInboxProcessed(ctx, tx, inboxID, accountID, nowMS); err != nil {
		return false, fmt.Errorf(
			"apply %s mutation to message (%q, %q, %q): %w",
			kind,
			accountID,
			conversationID,
			remoteMessageID,
			err,
		)
	}
	if err := tx.Commit(); err != nil {
		if isSQLiteConstraint(err) {
			return false, invalidMessageConstraintError(
				nil,
				err,
				"commit %s mutation to message (%q, %q, %q)",
				kind,
				accountID,
				conversationID,
				remoteMessageID,
			)
		}
		return false, fmt.Errorf(
			"apply %s mutation to message (%q, %q, %q): commit: %w",
			kind,
			accountID,
			conversationID,
			remoteMessageID,
			err,
		)
	}
	return affected == 1, nil
}

// GetLocalInstallationDevice resolves the account's persisted local device by
// its natural role instead of assuming a particular derived device ID.
func (s *Store) GetLocalInstallationDevice(
	ctx context.Context,
	accountID string,
) (Device, error) {
	device, err := scanDevice(s.db.QueryRowContext(ctx, `
		SELECT `+deviceColumns+`
		FROM devices
		WHERE account_id = ? AND kind = 'local_installation'
		ORDER BY is_current DESC, device_id
		LIMIT 1
	`, accountID))
	if errors.Is(err, sql.ErrNoRows) {
		return Device{}, fmt.Errorf(
			"local installation device for account %q: %w",
			accountID,
			ErrNotFound,
		)
	}
	if err != nil {
		return Device{}, fmt.Errorf(
			"get local installation device for account %q: %w",
			accountID,
			err,
		)
	}
	return device, nil
}

// BumpConversationRecency advances a conversation's last_message_at_ms to at
// least atMS. Message frames never re-upsert existing conversation rows (their
// titles and kinds are transport-refresh territory), so ingest recency flows
// through this targeted monotone update instead. A missing conversation is a
// no-op.
func (s *Store) BumpConversationRecency(conversationID string, atMS int64) error {
	if atMS <= 0 {
		return fmt.Errorf("bump conversation recency %q: time %d is not positive", conversationID, atMS)
	}
	_, err := s.db.Exec(
		`UPDATE conversations
		 SET last_message_at_ms = MAX(COALESCE(last_message_at_ms, 0), ?)
		 WHERE conversation_id = ?`,
		atMS,
		conversationID,
	)
	if err != nil {
		return fmt.Errorf("bump conversation recency %q: %w", conversationID, err)
	}
	return nil
}
