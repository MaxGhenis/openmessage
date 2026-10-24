package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// AccountAttachment is one attachment of an account's message, with the
// remote IDs that name the message on its transport.
type AccountAttachment struct {
	MessageAttachment
	RemoteConversationID string
	RemoteMessageID      string
	OccurredAtMS         int64
}

// ListAccountAttachments returns every attachment of accountID's messages,
// ordered by message and ordinal.
func (r *MessageAttachmentRepository) ListAccountAttachments(
	ctx context.Context,
	accountID string,
) ([]AccountAttachment, error) {
	rows, err := r.store.db.QueryContext(ctx, `
		SELECT
			ma.message_id,
			ma.ordinal,
			ma.remote_id,
			ma.remote_ref,
			ma.filename,
			ma.mime,
			ma.size_bytes,
			ma.state,
			ma.blob_hash,
			ma.last_error,
			ma.updated_at_ms,
			ma.created_at_ms,
			ma.remote_preview,
			ma.blob_preview,
			m.account_id,
			c.remote_conversation_id,
			m.remote_message_id,
			m.occurred_at_ms
		FROM message_attachments AS ma
		JOIN messages AS m ON m.message_id = ma.message_id
		JOIN conversations AS c
			ON c.account_id = m.account_id AND c.conversation_id = m.conversation_id
		WHERE m.account_id = ?
		ORDER BY ma.message_id, ma.ordinal
	`, accountID)
	if err != nil {
		return nil, fmt.Errorf("list attachments of account %q: %w", accountID, err)
	}
	attachments, err := collectRows(rows, func(scanner rowScanner) (AccountAttachment, error) {
		var (
			attachment AccountAttachment
			size       sql.NullInt64
			hash       sql.NullString
			lastError  sql.NullString
		)
		err := scanner.Scan(
			&attachment.MessageID,
			&attachment.Ordinal,
			&attachment.RemoteID,
			&attachment.RemoteRef,
			&attachment.Filename,
			&attachment.MIME,
			&size,
			&attachment.State,
			&hash,
			&lastError,
			&attachment.UpdatedAtMS,
			&attachment.CreatedAtMS,
			&attachment.RemotePreview,
			&attachment.BlobPreview,
			&attachment.AccountID,
			&attachment.RemoteConversationID,
			&attachment.RemoteMessageID,
			&attachment.OccurredAtMS,
		)
		if size.Valid {
			attachment.SizeBytes = &size.Int64
		}
		if hash.Valid {
			attachment.BlobHash = &hash.String
		}
		if lastError.Valid {
			attachment.LastError = &lastError.String
		}
		return attachment, err
	})
	if err != nil {
		return nil, fmt.Errorf("list attachments of account %q: %w", accountID, err)
	}
	return attachments, nil
}

// AttachmentReferenceRepair rewrites one attachment's remote reference, and
// the preview marks, as a repair planned from its transport's raw frames.
// It applies only while the row is as it was when the repair was planned:
// the same reference, state and bytes.
type AttachmentReferenceRepair struct {
	MessageID string
	Ordinal   int64

	ExpectRemoteID  string
	ExpectRemoteRef []byte
	ExpectBlobHash  *string

	RemoteID      string
	RemoteRef     []byte
	RemotePreview bool
	// BlobPreview marks the row's downloaded bytes as a reduced copy. It
	// must be false for a pending row.
	BlobPreview bool
	// Filename, MIME and SizeBytes replace the description of a pending
	// row, whose description belongs to its reference; a downloaded row
	// keeps the description of its bytes.
	Filename  string
	MIME      string
	SizeBytes *int64
}

// ApplyAttachmentReferenceRepairs applies repairs in one transaction and
// returns how many applied. A repair whose row has changed since it was
// planned is skipped, not an error. A pending row whose reference changes
// loses its last_error, which described the reference it had.
func (r *MessageAttachmentRepository) ApplyAttachmentReferenceRepairs(
	ctx context.Context,
	repairs []AttachmentReferenceRepair,
) (int, error) {
	nowMS, err := r.nowMS("repair attachment references")
	if err != nil {
		return 0, err
	}
	for _, repair := range repairs {
		if strings.TrimSpace(repair.RemoteID) == "" {
			return 0, fmt.Errorf("repair attachment %q ordinal %d: the new remote ID is empty", repair.MessageID, repair.Ordinal)
		}
		if repair.ExpectBlobHash == nil && repair.BlobPreview {
			return 0, fmt.Errorf("repair attachment %q ordinal %d: a pending row cannot hold a reduced copy", repair.MessageID, repair.Ordinal)
		}
	}
	tx, err := r.store.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("repair attachment references: begin: %w", err)
	}
	defer tx.Rollback()
	applied := 0
	for _, repair := range repairs {
		expectRef := repair.ExpectRemoteRef
		if expectRef == nil {
			expectRef = []byte{}
		}
		newRef := repair.RemoteRef
		if newRef == nil {
			newRef = []byte{}
		}
		var result sql.Result
		if repair.ExpectBlobHash == nil {
			mime := repair.MIME
			if strings.TrimSpace(mime) == "" {
				mime = "application/octet-stream"
			}
			clearError := repair.RemoteID != repair.ExpectRemoteID || !bytes.Equal(newRef, expectRef)
			result, err = tx.ExecContext(ctx, `
				UPDATE message_attachments
				SET remote_id = ?, remote_ref = ?, remote_preview = ?,
				    filename = ?, mime = ?, size_bytes = ?,
				    last_error = CASE WHEN ? THEN NULL ELSE last_error END,
				    updated_at_ms = ?
				WHERE message_id = ? AND ordinal = ?
				  AND state = 'pending' AND blob_hash IS NULL
				  AND remote_id = ? AND remote_ref = ?
			`, repair.RemoteID, newRef, repair.RemotePreview,
				repair.Filename, mime, repair.SizeBytes,
				clearError, nowMS,
				repair.MessageID, repair.Ordinal, repair.ExpectRemoteID, expectRef)
		} else {
			result, err = tx.ExecContext(ctx, `
				UPDATE message_attachments
				SET remote_id = ?, remote_ref = ?, remote_preview = ?, blob_preview = ?,
				    updated_at_ms = ?
				WHERE message_id = ? AND ordinal = ?
				  AND state = 'downloaded' AND blob_hash = ?
				  AND remote_id = ? AND remote_ref = ?
			`, repair.RemoteID, newRef, repair.RemotePreview, repair.BlobPreview,
				nowMS,
				repair.MessageID, repair.Ordinal, *repair.ExpectBlobHash, repair.ExpectRemoteID, expectRef)
		}
		if err != nil {
			return 0, fmt.Errorf("repair attachment %q ordinal %d: %w", repair.MessageID, repair.Ordinal, err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return 0, fmt.Errorf("repair attachment %q ordinal %d: rows affected: %w", repair.MessageID, repair.Ordinal, err)
		}
		applied += int(affected)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("repair attachment references: commit: %w", err)
	}
	return applied, nil
}
