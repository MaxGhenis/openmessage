package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// MessageDirection is the persisted direction of a normalized message.
type MessageDirection string

const (
	MessageDirectionIncoming MessageDirection = "incoming"
	MessageDirectionOutgoing MessageDirection = "outgoing"
)

// MessageState is the current normalized state of a message.
type MessageState string

const (
	MessageStateActive  MessageState = "active"
	MessageStateEdited  MessageState = "edited"
	MessageStateDeleted MessageState = "deleted"
)

// InboxRecord is one durable, undecoded transport frame. ReceivedAtMS and
// ProcessedAtMS are populated on reads; AppendInbox stamps ReceivedAtMS with
// the repository's injected clock and always stores ProcessedAtMS as NULL.
type InboxRecord struct {
	InboxID       string
	AccountID     string
	Generation    int64
	DedupeKey     string
	Codec         string
	CodecVersion  int64
	ReceivedAtMS  int64
	Payload       []byte
	ProcessedAtMS *int64
}

// Message is one normalized projected message. CreatedAtMS and UpdatedAtMS are
// populated on reads; ProjectMessage stamps them with the injected clock.
type Message struct {
	MessageID        string
	ConversationID   string
	AccountID        string
	RemoteMessageID  string
	SenderIdentityID *string
	Direction        MessageDirection
	Body             string
	ReplyToRemoteID  *string
	State            MessageState
	OccurredAtMS     int64
	CreatedAtMS      int64
	UpdatedAtMS      int64
}

// MessageProjection couples a normalized message to the durable inbox frame
// from which it was decoded.
type MessageProjection struct {
	InboxID     string
	Message     Message
	Attachments []MessageAttachment
}

// MessageRepository owns durable inbox ingestion and normalized projection.
type MessageRepository struct {
	store *Store
	now   func() time.Time
}

// NewMessageRepository creates an inbox/message repository. now is required so
// all storage-owned timestamps are deterministic in tests.
func NewMessageRepository(
	store *Store,
	now func() time.Time,
) (*MessageRepository, error) {
	if store == nil || store.db == nil {
		return nil, fmt.Errorf("create message repository: store is nil")
	}
	if now == nil {
		return nil, fmt.Errorf("create message repository: now function is nil")
	}
	return &MessageRepository{store: store, now: now}, nil
}

// AppendInbox commits a raw transport frame before decoding. A nonempty
// account-scoped dedupe key returns the original inbox ID without changing the
// stored frame.
func (r *MessageRepository) AppendInbox(
	ctx context.Context,
	record InboxRecord,
) (string, error) {
	receivedAtMS, err := r.nowMS("append inbox")
	if err != nil {
		return "", err
	}

	result, err := r.store.db.ExecContext(ctx, `
		INSERT INTO inbox (
			inbox_id,
			account_id,
			generation,
			dedupe_key,
			codec,
			codec_version,
			received_at_ms,
			payload
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(account_id, dedupe_key) WHERE dedupe_key <> '' DO NOTHING
	`,
		record.InboxID,
		record.AccountID,
		record.Generation,
		record.DedupeKey,
		record.Codec,
		record.CodecVersion,
		receivedAtMS,
		record.Payload,
	)
	if err != nil {
		return "", invalidInboxConstraintError(err, record.InboxID)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return "", fmt.Errorf("append inbox %q: read rows affected: %w", record.InboxID, err)
	}
	if affected == 1 {
		return record.InboxID, nil
	}
	if affected != 0 || record.DedupeKey == "" {
		return "", fmt.Errorf(
			"append inbox %q: unexpected rows affected: %d",
			record.InboxID,
			affected,
		)
	}

	var existingID string
	if err := r.store.db.QueryRowContext(ctx, `
		SELECT inbox_id
		FROM inbox
		WHERE account_id = ? AND dedupe_key = ?
	`, record.AccountID, record.DedupeKey).Scan(&existingID); err != nil {
		return "", fmt.Errorf(
			"append inbox %q: read existing deduplicated row: %w",
			record.InboxID,
			err,
		)
	}
	return existingID, nil
}

// Unprocessed returns durable frames in receipt order. Multiple workers may
// observe the same row; ProjectMessage provides the idempotent serialization
// boundary.
func (r *MessageRepository) Unprocessed(ctx context.Context) ([]InboxRecord, error) {
	rows, err := r.store.db.QueryContext(ctx, `
		SELECT `+inboxColumns+`
		FROM inbox
		WHERE processed_at_ms IS NULL
		ORDER BY received_at_ms, inbox_id
	`)
	if err != nil {
		return nil, fmt.Errorf("list unprocessed inbox records: %w", err)
	}
	records, err := collectRows(rows, scanInboxRecord)
	if err != nil {
		return nil, fmt.Errorf("list unprocessed inbox records: %w", err)
	}
	return records, nil
}

// GetMessage returns the normalized message with messageID.
func (r *MessageRepository) GetMessage(
	ctx context.Context,
	messageID string,
) (Message, error) {
	message, err := scanMessage(r.store.db.QueryRowContext(ctx, `
		SELECT `+messageColumns+`
		FROM messages
		WHERE message_id = ?
	`, messageID))
	if errors.Is(err, sql.ErrNoRows) {
		return Message{}, notFound("message", messageID)
	}
	if err != nil {
		return Message{}, fmt.Errorf("get message %q: %w", messageID, err)
	}
	return message, nil
}

// GetMessageByRemote returns the normalized message selected by the same
// account-scoped natural key used by ProjectMessage. This is useful to recover
// the effective local message ID after a projection collides with a row that
// was already stored for the same remote message.
func (r *MessageRepository) GetMessageByRemote(
	ctx context.Context,
	accountID string,
	conversationID string,
	remoteMessageID string,
) (Message, error) {
	message, err := scanMessage(r.store.db.QueryRowContext(ctx, `
		SELECT `+messageColumns+`
		FROM messages
		WHERE account_id = ?
		  AND conversation_id = ?
		  AND remote_message_id = ?
	`, accountID, conversationID, remoteMessageID))
	if errors.Is(err, sql.ErrNoRows) {
		return Message{}, fmt.Errorf(
			"message for account %q, conversation %q, and remote ID %q: %w",
			accountID,
			conversationID,
			remoteMessageID,
			ErrNotFound,
		)
	}
	if err != nil {
		return Message{}, fmt.Errorf(
			"get message for account %q, conversation %q, and remote ID %q: %w",
			accountID,
			conversationID,
			remoteMessageID,
			err,
		)
	}
	return message, nil
}

// ProjectMessage atomically upserts a normalized message by remote ID and
// marks its source inbox row processed. A replay of an already-processed inbox
// row returns without writing either timestamp.
func (r *MessageRepository) ProjectMessage(
	ctx context.Context,
	projection MessageProjection,
) error {
	tx, err := r.store.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("project message %q: begin transaction: %w", projection.Message.MessageID, err)
	}
	defer tx.Rollback()

	var (
		inboxAccountID string
		receivedAtMS   int64
		processedAtMS  sql.NullInt64
	)
	if err := tx.QueryRowContext(ctx, `
		SELECT account_id, received_at_ms, processed_at_ms
		FROM inbox
		WHERE inbox_id = ?
	`, projection.InboxID).Scan(
		&inboxAccountID,
		&receivedAtMS,
		&processedAtMS,
	); errors.Is(err, sql.ErrNoRows) {
		return invalidMessageError(
			orphanMessageError(ErrOrphanMessageInbox),
			"source inbox %q does not exist",
			projection.InboxID,
		)
	} else if err != nil {
		return fmt.Errorf(
			"project message %q: read inbox %q: %w",
			projection.Message.MessageID,
			projection.InboxID,
			err,
		)
	}
	if inboxAccountID != projection.Message.AccountID {
		return invalidMessageError(
			ErrCrossAccountMessage,
			"source inbox %q account %q does not match message account %q",
			projection.InboxID,
			inboxAccountID,
			projection.Message.AccountID,
		)
	}
	message := projection.Message
	if processedAtMS.Valid {
		existing, err := scanMessage(tx.QueryRowContext(ctx, `
			SELECT `+messageColumns+`
			FROM messages
			WHERE account_id = ?
			  AND conversation_id = ?
			  AND remote_message_id = ?
		`,
			message.AccountID,
			message.ConversationID,
			message.RemoteMessageID,
		))
		naturalKeyExists := true
		if errors.Is(err, sql.ErrNoRows) {
			naturalKeyExists = false
		} else if err != nil {
			return fmt.Errorf(
				"project message %q: read processed inbox natural key: %w",
				message.MessageID,
				err,
			)
		}
		// Execute the same SQLite write inside the transaction that will be
		// rolled back. This keeps SQLite authoritative for orphan/cross-account
		// validation even when the source inbox has already been processed.
		if err := r.upsertMessage(ctx, tx, message, processedAtMS.Int64); err != nil {
			return err
		}
		if !naturalKeyExists || !sameProjectedMessage(existing, message) {
			return fmt.Errorf(
				"%w: %w: processed inbox %q does not match message natural key/content (%q, %q, %q)",
				ErrInvalidMessage,
				ErrInboxProjectionConflict,
				projection.InboxID,
				message.AccountID,
				message.ConversationID,
				message.RemoteMessageID,
			)
		}
		return nil
	}

	nowMS, err := r.nowMS("project message")
	if err != nil {
		return err
	}
	if err := r.upsertMessage(ctx, tx, message, nowMS); err != nil {
		return err
	}
	attachmentMessageID := message.MessageID
	if len(projection.Attachments) > 0 {
		// upsertMessage preserves the first local message_id on a natural-key
		// conflict. Resolve that effective parent inside this transaction so a
		// duplicate inbound frame cannot point attachments at a discarded ID.
		if err := tx.QueryRowContext(ctx, `
			SELECT message_id
			FROM messages
			WHERE account_id = ?
			  AND conversation_id = ?
			  AND remote_message_id = ?
		`, message.AccountID, message.ConversationID, message.RemoteMessageID).Scan(
			&attachmentMessageID,
		); err != nil {
			return fmt.Errorf(
				"project message %q: resolve attachment parent: %w",
				message.MessageID,
				err,
			)
		}
	}
	attachmentRepository := &MessageAttachmentRepository{store: r.store, now: r.now}
	for _, attachment := range projection.Attachments {
		// Normalized bridge attachments do not carry a local message ID. Bind
		// every row to the parent selected by this projection rather than
		// trusting caller-supplied attachment ownership.
		attachment.MessageID = attachmentMessageID
		if err := attachmentRepository.RecordInboundAttachment(ctx, tx, attachment); err != nil {
			return fmt.Errorf(
				"project message %q: record attachment ordinal %d: %w",
				message.MessageID,
				attachment.Ordinal,
				err,
			)
		}
	}

	result, err := tx.ExecContext(ctx, `
		UPDATE inbox
		SET processed_at_ms = ?
		WHERE inbox_id = ? AND account_id = ? AND processed_at_ms IS NULL
	`, nowMS, projection.InboxID, message.AccountID)
	if err != nil {
		if isSQLiteConstraint(err) {
			return invalidMessageConstraintError(
				nil,
				err,
				"mark source inbox %q processed at %d after receipt at %d",
				projection.InboxID,
				nowMS,
				receivedAtMS,
			)
		}
		return fmt.Errorf(
			"project message %q: mark inbox %q processed: %w",
			message.MessageID,
			projection.InboxID,
			err,
		)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf(
			"project message %q: read processed inbox rows affected: %w",
			message.MessageID,
			err,
		)
	}
	if affected != 1 {
		return fmt.Errorf(
			"project message %q: mark inbox %q processed: affected %d rows, want 1",
			message.MessageID,
			projection.InboxID,
			affected,
		)
	}

	if err := tx.Commit(); err != nil {
		if isSQLiteConstraint(err) {
			return invalidMessageConstraintError(nil, err, "commit message projection")
		}
		return fmt.Errorf("project message %q: commit: %w", message.MessageID, err)
	}
	return nil
}

func (r *MessageRepository) upsertMessage(
	ctx context.Context,
	tx *sql.Tx,
	message Message,
	nowMS int64,
) error {
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO messages (
			message_id,
			conversation_id,
			account_id,
			remote_message_id,
			sender_identity_id,
			direction,
			body,
			reply_to_remote_id,
			state,
			occurred_at_ms,
			created_at_ms,
			updated_at_ms
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(account_id, conversation_id, remote_message_id) DO UPDATE SET
			sender_identity_id = excluded.sender_identity_id,
			direction = excluded.direction,
			body = excluded.body,
			reply_to_remote_id = excluded.reply_to_remote_id,
			state = excluded.state,
			occurred_at_ms = excluded.occurred_at_ms,
			updated_at_ms = excluded.updated_at_ms
		WHERE messages.sender_identity_id IS NOT excluded.sender_identity_id
		   OR messages.direction IS NOT excluded.direction
		   OR messages.body IS NOT excluded.body
		   OR messages.reply_to_remote_id IS NOT excluded.reply_to_remote_id
		   OR messages.state IS NOT excluded.state
		   OR messages.occurred_at_ms IS NOT excluded.occurred_at_ms
	`,
		message.MessageID,
		message.ConversationID,
		message.AccountID,
		message.RemoteMessageID,
		message.SenderIdentityID,
		message.Direction,
		message.Body,
		message.ReplyToRemoteID,
		message.State,
		message.OccurredAtMS,
		nowMS,
		nowMS,
	); err != nil {
		return r.mapMessageWriteError(ctx, tx, message, err)
	}
	return nil
}

const inboxColumns = `
	inbox_id,
	account_id,
	generation,
	dedupe_key,
	codec,
	codec_version,
	received_at_ms,
	payload,
	processed_at_ms`

func scanInboxRecord(row rowScanner) (InboxRecord, error) {
	var record InboxRecord
	err := row.Scan(
		&record.InboxID,
		&record.AccountID,
		&record.Generation,
		&record.DedupeKey,
		&record.Codec,
		&record.CodecVersion,
		&record.ReceivedAtMS,
		&record.Payload,
		&record.ProcessedAtMS,
	)
	return record, err
}

const messageColumns = `
	message_id,
	conversation_id,
	account_id,
	remote_message_id,
	sender_identity_id,
	direction,
	body,
	reply_to_remote_id,
	state,
	occurred_at_ms,
	created_at_ms,
	updated_at_ms`

func scanMessage(row rowScanner) (Message, error) {
	var message Message
	err := row.Scan(
		&message.MessageID,
		&message.ConversationID,
		&message.AccountID,
		&message.RemoteMessageID,
		&message.SenderIdentityID,
		&message.Direction,
		&message.Body,
		&message.ReplyToRemoteID,
		&message.State,
		&message.OccurredAtMS,
		&message.CreatedAtMS,
		&message.UpdatedAtMS,
	)
	return message, err
}

func sameProjectedMessage(existing Message, candidate Message) bool {
	return optionalStringEqual(existing.SenderIdentityID, candidate.SenderIdentityID) &&
		existing.Direction == candidate.Direction &&
		existing.Body == candidate.Body &&
		optionalStringEqual(existing.ReplyToRemoteID, candidate.ReplyToRemoteID) &&
		existing.State == candidate.State &&
		existing.OccurredAtMS == candidate.OccurredAtMS
}

func optionalStringEqual(left, right *string) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func (r *MessageRepository) nowMS(operation string) (int64, error) {
	nowMS := r.now().UnixMilli()
	if nowMS <= 0 {
		return 0, fmt.Errorf("%s: current Unix time is not positive", operation)
	}
	return nowMS, nil
}

func invalidInboxConstraintError(cause error, inboxID string) error {
	if !isSQLiteConstraint(cause) {
		return fmt.Errorf("append inbox %q: %w", inboxID, cause)
	}
	if isSQLiteErrorCode(cause, sqliteConstraintForeignKeyCode) {
		return fmt.Errorf(
			"%w: %w: %w: append inbox %q: %w",
			ErrInvalidInboxRecord,
			ErrConstraintViolation,
			ErrOrphanInboxAccount,
			inboxID,
			cause,
		)
	}
	return fmt.Errorf(
		"%w: %w: append inbox %q: %w",
		ErrInvalidInboxRecord,
		ErrConstraintViolation,
		inboxID,
		cause,
	)
}

func (r *MessageRepository) mapMessageWriteError(
	ctx context.Context,
	tx *sql.Tx,
	message Message,
	cause error,
) error {
	if !isSQLiteConstraint(cause) {
		return fmt.Errorf("project message %q: upsert: %w", message.MessageID, cause)
	}
	var specific error
	if isSQLiteErrorCode(cause, sqliteConstraintForeignKeyCode) {
		var err error
		specific, err = classifyMessageForeignKey(ctx, tx, message)
		if err != nil {
			return fmt.Errorf(
				"project message %q: classify foreign key constraint: %w",
				message.MessageID,
				err,
			)
		}
	}
	return invalidMessageConstraintError(
		specific,
		cause,
		"upsert message %q",
		message.MessageID,
	)
}

func classifyMessageForeignKey(
	ctx context.Context,
	tx *sql.Tx,
	message Message,
) (error, error) {
	var conversationAccountID string
	err := tx.QueryRowContext(ctx, `
		SELECT account_id
		FROM conversations
		WHERE conversation_id = ?
	`, message.ConversationID).Scan(&conversationAccountID)
	if errors.Is(err, sql.ErrNoRows) {
		return orphanMessageError(ErrOrphanMessageConversation), nil
	}
	if err != nil {
		return nil, err
	}
	if conversationAccountID != message.AccountID {
		return ErrCrossAccountMessage, nil
	}

	if message.SenderIdentityID == nil {
		return nil, nil
	}
	var identityAccountID string
	err = tx.QueryRowContext(ctx, `
		SELECT account_id
		FROM identities
		WHERE identity_id = ?
	`, *message.SenderIdentityID).Scan(&identityAccountID)
	if errors.Is(err, sql.ErrNoRows) {
		return orphanMessageError(ErrOrphanMessageIdentity), nil
	}
	if err != nil {
		return nil, err
	}
	if identityAccountID != message.AccountID {
		return ErrCrossAccountMessage, nil
	}
	return nil, nil
}

func orphanMessageError(specific error) error {
	return fmt.Errorf("%w: %w", ErrOrphanMessage, specific)
}

func invalidMessageConstraintError(
	specific error,
	cause error,
	format string,
	args ...any,
) error {
	detail := fmt.Sprintf(format, args...)
	if specific == nil {
		return fmt.Errorf(
			"%w: %w: %s: %w",
			ErrInvalidMessage,
			ErrConstraintViolation,
			detail,
			cause,
		)
	}
	return fmt.Errorf(
		"%w: %w: %w: %s: %w",
		ErrInvalidMessage,
		ErrConstraintViolation,
		specific,
		detail,
		cause,
	)
}

func invalidMessageError(specific error, format string, args ...any) error {
	detail := fmt.Sprintf(format, args...)
	return fmt.Errorf(
		"%w: %w: %w: %s",
		ErrInvalidMessage,
		ErrConstraintViolation,
		specific,
		detail,
	)
}
