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
	// RemotePreview marks a RemoteRef that names a reduced copy (a
	// thumbnail) because the transport has not named the original
	// (bridge.Attachment.Preview).
	RemotePreview bool
	// BlobPreview marks downloaded bytes that are such a reduced copy. A row
	// whose BlobPreview is set while RemotePreview is not holds a thumbnail
	// and already knows where its original is (NeedsOriginal).
	BlobPreview bool
}

// NeedsOriginal reports a downloaded reduced copy whose reference now names
// the original, so the original can be downloaded in its place.
func (a MessageAttachment) NeedsOriginal() bool {
	return a.State == "downloaded" && a.BlobPreview && !a.RemotePreview && strings.TrimSpace(a.RemoteID) != ""
}

// AwaitsOriginal reports a downloaded reduced copy whose transport has not
// named the original yet, so it can be asked to (bridge.FullMediaRequester).
func (a MessageAttachment) AwaitsOriginal() bool {
	return a.State == "downloaded" && a.BlobPreview && a.RemotePreview
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

// RecordInboundAttachment inserts or refreshes one attachment inside the
// caller's projection transaction.
//
// A pending row takes the refresh, except in two cases, where it keeps its
// reference and the filename, MIME type and size that came with it:
//   - A refresh that names no remote media (an empty RemoteID). A transport
//     can follow the update that named an attachment with one that names
//     nothing (Google did, for 154 of the live install's media frames by
//     2026-10-24), and taking it would leave the row pointing nowhere for
//     good.
//   - A refresh naming a reduced copy (RemotePreview) when the row names the
//     original.
//
// A downloaded row is never modified, with one exception: a downloaded
// reduced copy (BlobPreview) takes a refresh that names the original, so the
// original can be downloaded in its place. It keeps its bytes, and their
// filename, MIME type and size, until then.
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
		attachment.RemotePreview = false
	}
	if strings.TrimSpace(attachment.MIME) == "" {
		attachment.MIME = "application/octet-stream"
	}
	nowMS, err := r.nowMS("record inbound attachment")
	if err != nil {
		return err
	}

	// takesReference is whether the refresh's reference replaces the stored
	// one. The WHERE clause admits a downloaded row only for a refresh that
	// names the original, for which it is always true.
	const takesReference = `(
		excluded.remote_id <> ''
		AND NOT (
			excluded.remote_preview = 1
			AND message_attachments.remote_id <> ''
			AND message_attachments.remote_preview = 0
		)
	)`
	// pending is whether the stored row has no bytes yet.
	const pending = `message_attachments.blob_hash IS NULL`
	// takesDescription is whether the refresh's filename, MIME type and size
	// replace the stored ones: on a pending row, together with its reference,
	// or while the row names no media at all.
	const takesDescription = `(` + pending + ` AND (` + takesReference + `
		OR message_attachments.remote_id = ''))`
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO message_attachments (
			message_id,
			ordinal,
			remote_id,
			remote_ref,
			remote_preview,
			filename,
			mime,
			size_bytes,
			state,
			blob_hash,
			last_error,
			created_at_ms,
			updated_at_ms
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'pending', NULL, ?, ?, ?)
		ON CONFLICT(message_id, ordinal) DO UPDATE SET
			remote_id = CASE WHEN `+takesReference+`
				THEN excluded.remote_id ELSE message_attachments.remote_id END,
			remote_ref = CASE WHEN `+takesReference+`
				THEN excluded.remote_ref ELSE message_attachments.remote_ref END,
			remote_preview = CASE WHEN `+takesReference+`
				THEN excluded.remote_preview ELSE message_attachments.remote_preview END,
			filename = CASE WHEN `+takesDescription+`
				THEN excluded.filename ELSE message_attachments.filename END,
			mime = CASE WHEN `+takesDescription+`
				THEN excluded.mime ELSE message_attachments.mime END,
			size_bytes = CASE WHEN `+takesDescription+`
				THEN excluded.size_bytes ELSE message_attachments.size_bytes END,
			last_error = CASE WHEN `+pending+`
				THEN COALESCE(excluded.last_error, message_attachments.last_error)
				ELSE message_attachments.last_error END,
			updated_at_ms = excluded.updated_at_ms
		WHERE `+pending+`
		   OR (
			message_attachments.blob_preview = 1
			AND excluded.remote_preview = 0
			AND excluded.remote_id <> ''
		   )
	`,
		attachment.MessageID,
		attachment.Ordinal,
		attachment.RemoteID,
		attachment.RemoteRef,
		attachment.RemotePreview,
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
			ma.remote_preview,
			ma.blob_preview,
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
		&attachment.RemotePreview,
		&attachment.BlobPreview,
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
// attachment. preview says the bytes are a reduced copy (the row's
// RemotePreview when its reference was read for the download). A missing or
// already-downloaded row is an idempotent no-op.
func (r *MessageAttachmentRepository) MarkDownloaded(
	ctx context.Context,
	messageID string,
	ordinal int64,
	blobHash string,
	size int64,
	mime string,
	preview bool,
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
		    blob_preview = ?,
		    size_bytes = ?,
		    mime = ?,
		    last_error = NULL,
		    updated_at_ms = ?
		WHERE message_id = ? AND ordinal = ? AND state = 'pending'
	`, blobHash, preview, size, mime, nowMS, messageID, ordinal)
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
// did. It corrects a wrong import and puts an original in place of its
// reduced copy; preview says whether the new bytes are themselves a reduced
// copy. The old blob stays in the BlobStore, which other rows may share.
func (r *MessageAttachmentRepository) ReplaceDownloaded(
	ctx context.Context,
	messageID string,
	ordinal int64,
	oldBlobHash string,
	blobHash string,
	size int64,
	mime string,
	preview bool,
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
		    blob_preview = ?,
		    size_bytes = ?,
		    mime = ?,
		    last_error = NULL,
		    updated_at_ms = ?
		WHERE message_id = ? AND ordinal = ? AND state = 'downloaded' AND blob_hash = ?
	`, blobHash, preview, size, mime, nowMS, messageID, ordinal, oldBlobHash)
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
