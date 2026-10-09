package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// FirstAttachmentMIME reports the MIME type of a message's first attachment.
// It reads the lowest-ordinal message_attachments row, which ingested and
// migrated messages carry. An outgoing media message sent through the outbox
// has no such row; its attachment is the one its newest outbox intent was
// submitted with. ok is false when the message has neither.
func (r *MessageRepository) FirstAttachmentMIME(
	ctx context.Context,
	messageID string,
) (mime string, ok bool, err error) {
	err = r.store.db.QueryRowContext(ctx, `
		SELECT mime
		FROM message_attachments
		WHERE message_id = ?
		ORDER BY ordinal
		LIMIT 1
	`, messageID).Scan(&mime)
	if err == nil {
		return mime, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", false, fmt.Errorf("first attachment of message %q: %w", messageID, err)
	}

	err = r.store.db.QueryRowContext(ctx, `
		SELECT oa.mime
		FROM outbox AS o
		JOIN outbox_attachments AS oa ON oa.outbox_id = o.outbox_id
		WHERE o.local_message_id = ?
		ORDER BY o.created_at_ms DESC, o.outbox_id DESC, oa.ordinal
		LIMIT 1
	`, messageID).Scan(&mime)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("first outbox attachment of message %q: %w", messageID, err)
	}
	return mime, true, nil
}
