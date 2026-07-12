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
)
