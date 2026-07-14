package sqlite

import (
	"context"
	"fmt"
	"strings"
	"time"
)

const attachmentHashQueryBatchSize = 500

// Attachment describes a logical attachment and its content-addressed blob.
// CreatedAtMS is populated by GetAttachment; RecordAttachment stamps the row
// using the repository's injected clock.
type Attachment struct {
	AttachmentID string
	BlobHash     string
	Size         int64
	MIME         string
	Filename     string
	CreatedAtMS  int64
}

// AttachmentRepository records logical references to content-addressed blobs.
type AttachmentRepository struct {
	store *Store
	now   func() time.Time
}

// NewAttachmentRepository creates an attachment repository. now is required so
// tests and callers can control persisted timestamps.
func NewAttachmentRepository(
	store *Store,
	now func() time.Time,
) (*AttachmentRepository, error) {
	if store == nil || store.db == nil {
		return nil, fmt.Errorf("create attachment repository: store is nil")
	}
	if now == nil {
		return nil, fmt.Errorf("create attachment repository: now function is nil")
	}
	return &AttachmentRepository{store: store, now: now}, nil
}

// RecordAttachment records one logical attachment. Multiple attachment IDs may
// reference the same blob hash.
func (r *AttachmentRepository) RecordAttachment(
	ctx context.Context,
	attachment Attachment,
) error {
	if err := validateAttachment(attachment); err != nil {
		return fmt.Errorf("record attachment: %w", err)
	}
	createdAtMS := r.now().UnixMilli()
	if createdAtMS <= 0 {
		return fmt.Errorf("record attachment: current Unix time is not positive")
	}

	if _, err := r.store.db.ExecContext(ctx, `
		INSERT INTO attachments (
			attachment_id,
			blob_hash,
			size,
			mime,
			filename,
			created_at_ms
		) VALUES (?, ?, ?, ?, ?, ?)
	`,
		attachment.AttachmentID,
		attachment.BlobHash,
		attachment.Size,
		attachment.MIME,
		attachment.Filename,
		createdAtMS,
	); err != nil {
		return fmt.Errorf("record attachment %q: %w", attachment.AttachmentID, err)
	}
	return nil
}

// GetAttachment returns the attachment with attachmentID. A missing row wraps
// database/sql.ErrNoRows so callers can use errors.Is.
func (r *AttachmentRepository) GetAttachment(
	ctx context.Context,
	attachmentID string,
) (Attachment, error) {
	if strings.TrimSpace(attachmentID) == "" {
		return Attachment{}, fmt.Errorf("get attachment: attachment ID is empty")
	}

	var attachment Attachment
	if err := r.store.db.QueryRowContext(ctx, `
		SELECT attachment_id, blob_hash, size, mime, filename, created_at_ms
		FROM attachments
		WHERE attachment_id = ?
	`, attachmentID).Scan(
		&attachment.AttachmentID,
		&attachment.BlobHash,
		&attachment.Size,
		&attachment.MIME,
		&attachment.Filename,
		&attachment.CreatedAtMS,
	); err != nil {
		return Attachment{}, fmt.Errorf("get attachment %q: %w", attachmentID, err)
	}
	return attachment, nil
}

// ListUnreferencedBlobHashes returns the valid candidate hashes not referenced
// by an attachment. It preserves first-seen candidate order and removes
// duplicate candidates. Candidates are supplied by the blob store because this
// repository intentionally does not duplicate the filesystem inventory.
func (r *AttachmentRepository) ListUnreferencedBlobHashes(
	ctx context.Context,
	candidates []string,
) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("list unreferenced blob hashes: %w", err)
	}

	unique := make([]string, 0, len(candidates))
	seen := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		if err := validateBlobHash(candidate); err != nil {
			return nil, fmt.Errorf("list unreferenced blob hashes: %w", err)
		}
		if _, exists := seen[candidate]; exists {
			continue
		}
		seen[candidate] = struct{}{}
		unique = append(unique, candidate)
	}
	if len(unique) == 0 {
		return nil, nil
	}

	referenced := make(map[string]struct{}, len(unique))
	for start := 0; start < len(unique); start += attachmentHashQueryBatchSize {
		end := min(start+attachmentHashQueryBatchSize, len(unique))
		batch := unique[start:end]
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(batch)), ",")
		arguments := make([]any, len(batch))
		for i, hash := range batch {
			arguments[i] = hash
		}

		rows, err := r.store.db.QueryContext(ctx, `
			WITH referenced (blob_hash) AS (
				SELECT blob_hash FROM attachments
				UNION
				SELECT blob_hash FROM outbox_attachments
				UNION
				SELECT blob_hash FROM message_attachments WHERE blob_hash IS NOT NULL
			)
			SELECT blob_hash
			FROM referenced
			WHERE blob_hash IN (`+placeholders+`)
		`, arguments...)
		if err != nil {
			return nil, fmt.Errorf("list referenced blob hashes: %w", err)
		}
		for rows.Next() {
			var hash string
			if err := rows.Scan(&hash); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("scan referenced blob hash: %w", err)
			}
			referenced[hash] = struct{}{}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("iterate referenced blob hashes: %w", err)
		}
		if err := rows.Close(); err != nil {
			return nil, fmt.Errorf("close referenced blob hashes: %w", err)
		}
	}

	unreferenced := make([]string, 0, len(unique)-len(referenced))
	for _, candidate := range unique {
		if _, exists := referenced[candidate]; !exists {
			unreferenced = append(unreferenced, candidate)
		}
	}
	return unreferenced, nil
}

func validateAttachment(attachment Attachment) error {
	if strings.TrimSpace(attachment.AttachmentID) == "" {
		return fmt.Errorf("attachment ID is empty")
	}
	if err := validateBlobHash(attachment.BlobHash); err != nil {
		return err
	}
	if attachment.Size < 0 {
		return fmt.Errorf("attachment size is negative")
	}
	if strings.TrimSpace(attachment.MIME) == "" {
		return fmt.Errorf("attachment MIME type is empty")
	}
	return nil
}

func validateBlobHash(hash string) error {
	if len(hash) != 64 {
		return fmt.Errorf("blob hash %q has length %d, want 64", hash, len(hash))
	}
	for _, value := range []byte(hash) {
		if (value < '0' || value > '9') && (value < 'a' || value > 'f') {
			return fmt.Errorf("blob hash %q is not lowercase hexadecimal SHA-256", hash)
		}
	}
	return nil
}
