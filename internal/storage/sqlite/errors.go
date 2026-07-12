package sqlite

import "errors"

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
)
