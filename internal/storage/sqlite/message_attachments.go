package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// MessageAttachment is one inbound attachment associated with a normalized
// message. AccountID is populated only by GetForDownload's messages join.
type MessageAttachment struct {
	MessageID string
	RemoteID  string
	Ordinal   int64
	RemoteRef []byte
	Filename  string
	MIME      string
	SizeBytes *int64
	State     string
	BlobHash  *string
	AccountID string
}

// MessageAttachmentRepository owns inbound attachment lifecycle state.
type MessageAttachmentRepository struct {
	store *Store
	now   func() time.Time
}

// NewMessageAttachmentRepository creates an inbound attachment repository.
func NewMessageAttachmentRepository(
	store *Store,
	now func() time.Time,
) (*MessageAttachmentRepository, error) {
	if store == nil || store.db == nil {
		return nil, fmt.Errorf("create message attachment repository: store is nil")
	}
	if now == nil {
		return nil, fmt.Errorf("create message attachment repository: now function is nil")
	}
	return &MessageAttachmentRepository{store: store, now: now}, nil
}

// RecordInboundAttachment inserts or refreshes one pending attachment inside
// the caller's projection transaction. A downloaded row is never modified.
func (r *MessageAttachmentRepository) RecordInboundAttachment(
	ctx context.Context,
	tx *sql.Tx,
	attachment MessageAttachment,
) error {
	if tx == nil {
		return fmt.Errorf("record inbound attachment: transaction is nil")
	}
	if strings.TrimSpace(attachment.MessageID) == "" {
		return fmt.Errorf("record inbound attachment: message ID is empty")
	}
	if attachment.Ordinal < 0 {
		return fmt.Errorf("record inbound attachment: ordinal is negative")
	}
	if attachment.SizeBytes != nil && *attachment.SizeBytes < 0 {
		return fmt.Errorf("record inbound attachment: size is negative")
	}
	if attachment.RemoteRef == nil {
		attachment.RemoteRef = []byte{}
	}
	if strings.TrimSpace(attachment.MIME) == "" {
		attachment.MIME = "application/octet-stream"
	}
	nowMS, err := r.nowMS("record inbound attachment")
	if err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO message_attachments (
			message_id,
			ordinal,
			remote_id,
			remote_ref,
			filename,
			mime,
			size_bytes,
			state,
			blob_hash,
			created_at_ms,
			updated_at_ms
		) VALUES (?, ?, ?, ?, ?, ?, ?, 'pending', NULL, ?, ?)
		ON CONFLICT(message_id, ordinal) DO UPDATE SET
			remote_id = excluded.remote_id,
			remote_ref = excluded.remote_ref,
			filename = excluded.filename,
			mime = excluded.mime,
			size_bytes = excluded.size_bytes,
			updated_at_ms = excluded.updated_at_ms
		WHERE message_attachments.blob_hash IS NULL
	`,
		attachment.MessageID,
		attachment.Ordinal,
		attachment.RemoteID,
		attachment.RemoteRef,
		attachment.Filename,
		attachment.MIME,
		attachment.SizeBytes,
		nowMS,
		nowMS,
	); err != nil {
		return fmt.Errorf(
			"record inbound attachment for message %q ordinal %d: %w",
			attachment.MessageID,
			attachment.Ordinal,
			err,
		)
	}
	return nil
}

// GetForDownload returns an attachment joined to its owning account. A missing
// row wraps database/sql.ErrNoRows.
func (r *MessageAttachmentRepository) GetForDownload(
	ctx context.Context,
	messageID string,
	ordinal int64,
) (MessageAttachment, error) {
	var (
		attachment MessageAttachment
		size       sql.NullInt64
		hash       sql.NullString
	)
	err := r.store.db.QueryRowContext(ctx, `
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
			m.account_id
		FROM message_attachments AS ma
		JOIN messages AS m ON m.message_id = ma.message_id
		WHERE ma.message_id = ? AND ma.ordinal = ?
	`, messageID, ordinal).Scan(
		&attachment.MessageID,
		&attachment.Ordinal,
		&attachment.RemoteID,
		&attachment.RemoteRef,
		&attachment.Filename,
		&attachment.MIME,
		&size,
		&attachment.State,
		&hash,
		&attachment.AccountID,
	)
	if err != nil {
		return MessageAttachment{}, fmt.Errorf(
			"get message attachment %q ordinal %d for download: %w",
			messageID,
			ordinal,
			err,
		)
	}
	if size.Valid {
		attachment.SizeBytes = &size.Int64
	}
	if hash.Valid {
		attachment.BlobHash = &hash.String
	}
	return attachment, nil
}

// MarkDownloaded atomically publishes a blob reference on a still-pending
// attachment. A missing or already-downloaded row is an idempotent no-op.
func (r *MessageAttachmentRepository) MarkDownloaded(
	ctx context.Context,
	messageID string,
	ordinal int64,
	blobHash string,
	size int64,
	mime string,
) error {
	if err := validateBlobHash(blobHash); err != nil {
		return fmt.Errorf("mark message attachment downloaded: %w", err)
	}
	if size < 0 {
		return fmt.Errorf("mark message attachment downloaded: size is negative")
	}
	if strings.TrimSpace(mime) == "" {
		return fmt.Errorf("mark message attachment downloaded: MIME type is empty")
	}
	nowMS, err := r.nowMS("mark message attachment downloaded")
	if err != nil {
		return err
	}
	result, err := r.store.db.ExecContext(ctx, `
		UPDATE message_attachments
		SET state = 'downloaded',
		    blob_hash = ?,
		    size_bytes = ?,
		    mime = ?,
		    last_error = NULL,
		    updated_at_ms = ?
		WHERE message_id = ? AND ordinal = ? AND state = 'pending'
	`, blobHash, size, mime, nowMS, messageID, ordinal)
	if err != nil {
		return fmt.Errorf(
			"mark message attachment %q ordinal %d downloaded: %w",
			messageID,
			ordinal,
			err,
		)
	}
	if _, err := result.RowsAffected(); err != nil {
		return fmt.Errorf(
			"mark message attachment %q ordinal %d downloaded: read rows affected: %w",
			messageID,
			ordinal,
			err,
		)
	}
	return nil
}

// MarkFailed records diagnostic text while leaving the attachment pending and
// retryable. A missing or downloaded row is a no-op.
func (r *MessageAttachmentRepository) MarkFailed(
	ctx context.Context,
	messageID string,
	ordinal int64,
	errText string,
) error {
	nowMS, err := r.nowMS("mark message attachment failed")
	if err != nil {
		return err
	}
	if _, err := r.store.db.ExecContext(ctx, `
		UPDATE message_attachments
		SET last_error = ?, updated_at_ms = ?
		WHERE message_id = ? AND ordinal = ? AND state = 'pending'
	`, errText, nowMS, messageID, ordinal); err != nil {
		return fmt.Errorf(
			"mark message attachment %q ordinal %d failed: %w",
			messageID,
			ordinal,
			err,
		)
	}
	return nil
}

func (r *MessageAttachmentRepository) nowMS(operation string) (int64, error) {
	nowMS := r.now().UnixMilli()
	if nowMS <= 0 {
		return 0, fmt.Errorf("%s: current Unix time is not positive", operation)
	}
	return nowMS, nil
}
