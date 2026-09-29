package sqlite

import (
	"errors"
	"fmt"
)

var (
	// ErrNotFound means the requested repository row does not exist.
	ErrNotFound = errors.New("sqlite repository row not found")

	// ErrConstraintViolation is the common sentinel for a write that cannot
	// satisfy the SQLite schema constraints.
	ErrConstraintViolation = errors.New("sqlite constraint violation")

	// ErrDuplicateIdentityLink means an identity already belongs to a person.
	ErrDuplicateIdentityLink = errors.New("identity is already linked to a person")

	// ErrInvalidConversationParticipant identifies participant rows that cannot
	// satisfy the conversation/identity ownership constraints.
	ErrInvalidConversationParticipant = errors.New("invalid conversation participant")

	// ErrCrossAccountParticipant means a participant, identity, and conversation
	// do not all belong to the same account.
	ErrCrossAccountParticipant = errors.New("cross-account conversation participant")

	// ErrOrphanParticipantIdentity means a participant references an identity
	// that does not exist.
	ErrOrphanParticipantIdentity = errors.New("orphan conversation participant identity")

	// ErrInvalidInboxRecord identifies an inbox row that cannot satisfy the
	// durable ingress schema.
	ErrInvalidInboxRecord = errors.New("invalid inbox record")

	// ErrOrphanInboxAccount means an inbox row references an account that does
	// not exist.
	ErrOrphanInboxAccount = errors.New("orphan inbox account")

	// ErrInvalidMessage identifies a normalized message that cannot satisfy the
	// message schema or its projection relationship to an inbox row.
	ErrInvalidMessage = errors.New("invalid message")

	// ErrCrossAccountMessage means a message, its inbox record, conversation,
	// and sender identity do not all belong to the same account.
	ErrCrossAccountMessage = errors.New("cross-account message")

	// ErrOrphanMessage is the common sentinel for a message that references a
	// missing inbox row, conversation, or sender identity.
	ErrOrphanMessage = errors.New("orphan message")

	// ErrOrphanMessageInbox identifies a missing source inbox row.
	ErrOrphanMessageInbox = errors.New("orphan message inbox")

	// ErrOrphanMessageConversation identifies a missing message conversation.
	ErrOrphanMessageConversation = errors.New("orphan message conversation")

	// ErrOrphanMessageIdentity identifies a missing sender identity.
	ErrOrphanMessageIdentity = errors.New("orphan message sender identity")

	// ErrInboxProjectionConflict means an already-processed inbox row was
	// replayed with different message identity or normalized content.
	ErrInboxProjectionConflict = errors.New("inbox projection conflict")

	// ErrIdempotencyConflict means an existing account-scoped idempotency key
	// names a different outbound intent.
	ErrIdempotencyConflict = errors.New("outbox idempotency conflict")

	// ErrLeaseLost means an outbox mutation no longer owns the row's active
	// dispatch lease.
	ErrLeaseLost = errors.New("outbox lease lost")

	// ErrInvalidOutboxState means an outbox operation is not allowed from the
	// row's current delivery state.
	ErrInvalidOutboxState = errors.New("invalid outbox state transition")

	// ErrStoreMissing means OpenReadOnly found no regular file at the store
	// path. A read-only client never creates a store; the running app
	// provisions it.
	ErrStoreMissing = errors.New("sqlite store does not exist")

	// ErrSchemaNewer means the store's migration ledger runs past the newest
	// migration this build embeds: a newer binary migrated it.
	ErrSchemaNewer = errors.New("sqlite store schema is newer than this build supports")

	// ErrSchemaTooOld means the store is a valid prefix of this build's
	// migrations but older than MinClientReadSchemaVersion, so the client
	// read inventory would fail against it.
	ErrSchemaTooOld = errors.New("sqlite store schema is older than read-only clients support")

	// ErrLedgerMismatch means the store's migration ledger, user_version, or
	// application_id is not a state any build of this lineage writes: a
	// missing or non-contiguous ledger, or a renamed or edited migration.
	ErrLedgerMismatch = errors.New("sqlite store migration ledger does not match this build")

	// ErrReadOnlyAttach means SQLite could not open the store read-only at all,
	// typically because the store's directory is not writable and the WAL
	// index (-shm) does not exist yet.
	ErrReadOnlyAttach = errors.New("cannot attach to sqlite store read-only")
)

// schemaError tags a schema-compatibility failure with its class sentinel
// (ErrSchemaNewer, ErrSchemaTooOld, ErrLedgerMismatch) while keeping the exact
// message text the migrating open has always reported.
type schemaError struct {
	class error
	err   error
}

func (e *schemaError) Error() string { return e.err.Error() }

func (e *schemaError) Unwrap() []error { return []error{e.class, e.err} }

func schemaErrorf(class error, format string, args ...any) error {
	return &schemaError{class: class, err: fmt.Errorf(format, args...)}
}

// IsReadOnlyError reports whether err is SQLite refusing a write because the
// connection or database is read-only (SQLITE_READONLY or one of its extended
// codes). Every mutating call on a store from OpenReadOnly fails this way.
func IsReadOnlyError(err error) bool {
	code, ok := sqliteErrorCode(err)
	return ok && code&0xff == sqliteReadOnlyCode
}
