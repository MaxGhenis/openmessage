package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

const (
	// ReceiveAttachmentErrorPrefix starts the last_error a projection records
	// when the transport reported, with the message itself, that it failed
	// to fetch the attachment. Download attempts keep that text rather than
	// overwriting it with their own, usually less informative, failure.
	ReceiveAttachmentErrorPrefix = "receive: "
	// UnavailableAttachmentErrorPrefix starts the last_error of an attachment
	// a download attempt found its transport has no copy of. The mark stands
	// for UnavailableAttachmentRecheckAfter (from updated_at_ms): meanwhile
	// the media service answers from the row and reads show the attachment
	// as unavailable. After that, reads show it as downloadable again and the
	// next download asks the transport once more, because a copy can come
	// back (a re-linked Signal's attachment store restored from a backup).
	// The row stays pending (the state column allows only pending and
	// downloaded), so an import from elsewhere can fill it.
	UnavailableAttachmentErrorPrefix = "unavailable: "
)

// UnavailableAttachmentRecheckAfter is how long an unavailable mark stands.
// It bounds the transport calls a repeatedly rendered attachment costs while
// still noticing a copy that came back.
const UnavailableAttachmentRecheckAfter = time.Hour

// UnavailableReason reports why a pending attachment cannot be downloaded
// through its transport, once a download attempt has found it has no copy.
// A receive-time report alone does not count: the attempt confirms it. ok is
// false for an attachment that is downloaded or may still download.
func (a MessageAttachment) UnavailableReason() (reason string, ok bool) {
	if a.State != "pending" || a.LastError == nil {
		return "", false
	}
	rest, found := strings.CutPrefix(*a.LastError, UnavailableAttachmentErrorPrefix)
	if !found {
		return "", false
	}
	return strings.TrimSpace(rest), true
}

// UnavailableReasonAt is UnavailableReason for a mark still standing at now
// (younger than UnavailableAttachmentRecheckAfter; UpdatedAtMS must be read).
// A mark past it reads as not unavailable, so a reader fetches the
// attachment and that fetch re-checks the transport.
func (a MessageAttachment) UnavailableReasonAt(now time.Time) (reason string, ok bool) {
	reason, ok = a.UnavailableReason()
	if !ok || now.Sub(time.UnixMilli(a.UpdatedAtMS)) >= UnavailableAttachmentRecheckAfter {
		return "", false
	}
	return reason, true
}

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
	// LastError is diagnostic text from the projection or the latest failed
	// download. RecordInboundAttachment only writes a non-nil value, so a
	// replay that carries none keeps what is stored.
	LastError *string
	AccountID string
	// UpdatedAtMS and CreatedAtMS are read by GetForDownload only.
	UpdatedAtMS int64
	CreatedAtMS int64
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
//
// A refresh that names no remote media (an empty RemoteID) keeps the
// reference already stored, and the filename, MIME type and size that came
// with it: a transport can follow the update that named an attachment with
// one that names nothing (Google did, for 154 of the live install's media
// frames by 2026-10-24), and taking it would leave the row pointing nowhere
// for good.
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
	if strings.TrimSpace(attachment.RemoteID) == "" {
		attachment.RemoteID = ""
	}
	if strings.TrimSpace(attachment.MIME) == "" {
		attachment.MIME = "application/octet-stream"
	}
	nowMS, err := r.nowMS("record inbound attachment")
	if err != nil {
		return err
	}

	// keepsReference is whether the stored row names media and the refresh
	// does not, so the row keeps its reference and the description that came
	// with it.
	const keepsReference = `(excluded.remote_id = '' AND message_attachments.remote_id <> '')`
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
			last_error,
			created_at_ms,
			updated_at_ms
		) VALUES (?, ?, ?, ?, ?, ?, ?, 'pending', NULL, ?, ?, ?)
		ON CONFLICT(message_id, ordinal) DO UPDATE SET
			remote_id = CASE WHEN excluded.remote_id = ''
				THEN message_attachments.remote_id ELSE excluded.remote_id END,
			remote_ref = CASE WHEN excluded.remote_id = ''
				THEN message_attachments.remote_ref ELSE excluded.remote_ref END,
			filename = CASE WHEN `+keepsReference+`
				THEN message_attachments.filename ELSE excluded.filename END,
			mime = CASE WHEN `+keepsReference+`
				THEN message_attachments.mime ELSE excluded.mime END,
			size_bytes = CASE WHEN `+keepsReference+`
				THEN message_attachments.size_bytes ELSE excluded.size_bytes END,
			last_error = COALESCE(excluded.last_error, message_attachments.last_error),
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
		attachment.LastError,
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
		lastError  sql.NullString
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
			ma.last_error,
			ma.updated_at_ms,
			ma.created_at_ms,
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
		&lastError,
		&attachment.UpdatedAtMS,
		&attachment.CreatedAtMS,
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
	if lastError.Valid {
		attachment.LastError = &lastError.String
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

// ReplaceDownloaded points a downloaded attachment at different bytes, only
// while it still holds oldBlobHash (compare-and-swap), and reports whether it
// did. It exists to correct a wrong import; the old blob stays in the
// BlobStore, which other rows may share.
func (r *MessageAttachmentRepository) ReplaceDownloaded(
	ctx context.Context,
	messageID string,
	ordinal int64,
	oldBlobHash string,
	blobHash string,
	size int64,
	mime string,
) (bool, error) {
	if err := validateBlobHash(blobHash); err != nil {
		return false, fmt.Errorf("replace message attachment: %w", err)
	}
	if size < 0 {
		return false, fmt.Errorf("replace message attachment: size is negative")
	}
	if strings.TrimSpace(mime) == "" {
		return false, fmt.Errorf("replace message attachment: MIME type is empty")
	}
	nowMS, err := r.nowMS("replace message attachment")
	if err != nil {
		return false, err
	}
	result, err := r.store.db.ExecContext(ctx, `
		UPDATE message_attachments
		SET blob_hash = ?,
		    size_bytes = ?,
		    mime = ?,
		    last_error = NULL,
		    updated_at_ms = ?
		WHERE message_id = ? AND ordinal = ? AND state = 'downloaded' AND blob_hash = ?
	`, blobHash, size, mime, nowMS, messageID, ordinal, oldBlobHash)
	if err != nil {
		return false, fmt.Errorf("replace message attachment %q ordinal %d: %w", messageID, ordinal, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("replace message attachment %q ordinal %d: read rows affected: %w", messageID, ordinal, err)
	}
	return affected == 1, nil
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
