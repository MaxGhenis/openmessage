// Package migration transforms the legacy OpenMessage store into the merged
// v2 SQLite schema. It is intentionally one-shot and never opens the legacy
// database through internal/db, whose constructor performs writes.
package migration

import "errors"

const (
	// ReportFormat is the machine-readable integrity report schema version.
	ReportFormat = 1
	// SampleMessagesPerPlatform is the deterministic spot-check sample size.
	SampleMessagesPerPlatform = 5
)

var (
	// ErrSource marks an unsupported, ambiguous, or unreadable legacy source.
	ErrSource = errors.New("legacy migration source error")
	// ErrTransform marks a failure while constructing the staged v2 store.
	ErrTransform = errors.New("legacy migration transform error")
	// ErrValidation marks a completed transform that failed integrity gates.
	ErrValidation = errors.New("legacy migration validation error")
)

// Report is the pre-registered cutover evidence emitted by migrate and
// migrate --check. Maps have stable JSON ordering under encoding/json.
type Report struct {
	Format            int                               `json:"format"`
	OK                bool                              `json:"ok"`
	Check             bool                              `json:"check"`
	Source            SourceReport                      `json:"source"`
	Target            TargetReport                      `json:"target"`
	TableCounts       map[string]TableReconciliation    `json:"table_counts"`
	PlatformCounts    map[string]PlatformReconciliation `json:"platform_counts"`
	Identities        IdentityReport                    `json:"identities"`
	Schedule          ScheduleReport                    `json:"schedule"`
	ReadState         ReadStateReport                   `json:"read_state"`
	Reactions         ReactionReport                    `json:"reactions"`
	Dropped           DroppedDimensions                 `json:"dropped_dimensions"`
	SignalLocalRows   int64                             `json:"signal_local_rows"`
	Media             MediaReport                       `json:"media"`
	MessageCollisions []MessageCollision                `json:"message_collisions"`
	SampledHashes     []SampledHash                     `json:"sampled_hashes"`
	Validation        ValidationReport                  `json:"validation"`
	Warnings          []string                          `json:"warnings"`
}

type ReactionReport struct {
	MessagesWithReactions     int64 `json:"messages_with_reactions"`
	MessagesSeeded            int64 `json:"messages_seeded"`
	MessagesFullyDeduplicated int64 `json:"messages_fully_deduplicated"`
	RowsPlannable             int64 `json:"rows_plannable"`
	RowsSeeded                int64 `json:"rows_seeded"`
	ActorlessCountCollapsed   int64 `json:"actorless_count_collapsed"`
	ActorCountSurplusDropped  int64 `json:"actor_count_surplus_dropped"`
	DuplicateRowsDropped      int64 `json:"duplicate_rows_dropped"`
	SeedConflicts             int64 `json:"seed_conflicts"`
}

type SourceReport struct {
	Path              string               `json:"path"`
	DatabasePath      string               `json:"database_path"`
	SchemaFingerprint string               `json:"schema_fingerprint"`
	SchemaUserVersion int64                `json:"schema_user_version"`
	QuickCheck        string               `json:"quick_check"`
	SHA256Before      string               `json:"sha256_before"`
	SHA256After       string               `json:"sha256_after"`
	Unchanged         bool                 `json:"unchanged"`
	Files             []SourceFileEvidence `json:"files"`
}

// SourceFileEvidence records content and mutation-relevant metadata for the
// SQLite database family. WAL and shared-memory sidecars are part of the
// source evidence because a stopped legacy WAL store may retain both.
type SourceFileEvidence struct {
	Role               string `json:"role"`
	Path               string `json:"path"`
	ExistsBefore       bool   `json:"exists_before"`
	ExistsAfter        bool   `json:"exists_after"`
	SizeBefore         int64  `json:"size_before"`
	SizeAfter          int64  `json:"size_after"`
	SHA256Before       string `json:"sha256_before,omitempty"`
	SHA256After        string `json:"sha256_after,omitempty"`
	ModeBefore         string `json:"mode_before,omitempty"`
	ModeAfter          string `json:"mode_after,omitempty"`
	ModifiedAtNSBefore int64  `json:"modified_at_ns_before,omitempty"`
	ModifiedAtNSAfter  int64  `json:"modified_at_ns_after,omitempty"`
	Unchanged          bool   `json:"unchanged"`
}

type TargetReport struct {
	Path               string           `json:"path"`
	DatabasePath       string           `json:"database_path"`
	StoreInstanceID    string           `json:"store_instance_id"`
	SchemaVersion      int64            `json:"schema_version"`
	MigrationChecksums []string         `json:"migration_checksums"`
	Counts             map[string]int64 `json:"counts"`
	Published          bool             `json:"published"`
}

type TableReconciliation struct {
	Legacy              int64   `json:"legacy"`
	V2                  int64   `json:"v2"`
	Reconciled          int64   `json:"reconciled"`
	ReconciliationRatio float64 `json:"reconciliation_ratio"`
	Disposition         string  `json:"disposition,omitempty"`
}

type PlatformReconciliation struct {
	AccountID           string  `json:"account_id"`
	Legacy              int64   `json:"legacy"`
	V2                  int64   `json:"v2"`
	ReconciliationRatio float64 `json:"reconciliation_ratio"`
}

type IdentityReport struct {
	Created             int64            `json:"created"`
	PeopleCreated       int64            `json:"people_created"`
	LinksByProvenance   map[string]int64 `json:"links_by_provenance"`
	UnunifiedIdentities int64            `json:"ununified_identities"`
}

type ScheduleReport struct {
	LegacyTotal        int64            `json:"legacy_total"`
	ByStatus           map[string]int64 `json:"by_status"`
	PendingToOutbox    int64            `json:"pending_to_outbox"`
	PendingMedia       int64            `json:"pending_media"`
	SkippedSent        int64            `json:"skipped_sent"`
	SkippedCanceled    int64            `json:"skipped_canceled"`
	SkippedFailed      int64            `json:"skipped_failed"`
	AmbiguousSending   int64            `json:"ambiguous_sending"`
	OptimisticOnlyRows int64            `json:"optimistic_only_rows"`
}

type ReadStateReport struct {
	LossyWarning                 string `json:"lossy_warning"`
	ConversationsWithUnreadCount int64  `json:"conversations_with_unread_count"`
	CursorsCreated               int64  `json:"cursors_created"`
}

type DroppedDimensions struct {
	ReactionsBearingMessages  int64 `json:"reactions_bearing_messages"`
	TranscriptBearingMessages int64 `json:"transcript_bearing_messages"`
	ContactAvatars            int64 `json:"contact_avatars"`
	Drafts                    int64 `json:"drafts"`
	ContactMetaCRM            int64 `json:"contact_meta_crm"`
	OutgoingSendKeys          int64 `json:"outgoing_send_keys"`
	Tabs                      int64 `json:"tabs"`
	// MalformedMessages counts legacy message rows with no message_id — no
	// stable identity or content to migrate, dropped rather than aborting.
	MalformedMessages int64 `json:"malformed_messages"`
	// OrphanedMessages counts messages whose conversation row no longer exists
	// (already unreachable in the legacy app); dropped with a count.
	OrphanedMessages int64 `json:"orphaned_messages"`
	// PlatformMismatchMessages counts messages whose platform disagrees with
	// their conversation's platform; dropped with a count.
	PlatformMismatchMessages int64 `json:"platform_mismatch_messages"`
	// NonPositiveTimestampMessages counts messages with a non-positive
	// timestamp_ms (placeholder rows with no usable time); dropped with a count.
	NonPositiveTimestampMessages int64 `json:"non_positive_timestamp_messages"`
	// UnmappableMessages counts messages that map to an empty remote id;
	// dropped with a count.
	UnmappableMessages int64 `json:"unmappable_messages"`
	// MalformedParticipants counts conversations whose participants JSON could
	// not be parsed; the conversation migrated with no participant roster.
	MalformedParticipants int64 `json:"malformed_participants"`
	// UnmappableUnifiedContacts counts unified contacts whose identifiers were
	// empty or not the expected JSON array; dropped with a count.
	UnmappableUnifiedContacts int64 `json:"unmappable_unified_contacts"`
	// MalformedReactions counts reaction-bearing messages whose JSON could not
	// be decoded or contained no usable emoji entries. The message still moves.
	MalformedReactions int64 `json:"malformed_reactions"`
	// UnmappableReactions counts reaction-bearing messages with reactor tokens
	// that cannot be converted to the platform's live-ingest identity key.
	UnmappableReactions               int64 `json:"unmappable_reactions"`
	ReactionMessagesDroppedWithParent int64 `json:"reaction_messages_dropped_with_parent"`
}

type MediaReport struct {
	LegacyAttachments             int64 `json:"legacy_attachments"`
	PendingAttachments            int64 `json:"pending_attachments"`
	WhatsAppUnresolvable          int64 `json:"whatsapp_unresolvable"`
	SignalUnresolvable            int64 `json:"signal_unresolvable"`
	SignalLocalAttachments        int64 `json:"signal_local_attachments"`
	UnsupportedArchiveAttachments int64 `json:"unsupported_archive_attachments"`
	ScheduledBlobsVerified        int64 `json:"scheduled_blobs_verified"`
}

type MessageCollision struct {
	AccountID        string   `json:"account_id"`
	ConversationID   string   `json:"legacy_conversation_id"`
	RemoteMessageID  string   `json:"remote_message_id"`
	LegacyMessageIDs []string `json:"legacy_message_ids"`
}

type SampledHash struct {
	Platform        string `json:"platform"`
	LegacyMessageID string `json:"legacy_message_id"`
	V2MessageID     string `json:"v2_message_id"`
	ExpectedSHA256  string `json:"expected_sha256"`
	ActualSHA256    string `json:"actual_sha256"`
	Matched         bool   `json:"matched"`
}

type ValidationReport struct {
	QuickCheck           string                `json:"quick_check"`
	ForeignKeyViolations []ForeignKeyViolation `json:"foreign_key_check"`
	Orphans              OrphanReport          `json:"orphans"`
	CountsMatched        bool                  `json:"counts_matched"`
	SampledHashesMatched bool                  `json:"sampled_hashes_matched"`
	BlobReferencesValid  bool                  `json:"blob_references_valid"`
	SourceUnchanged      bool                  `json:"source_unchanged"`
	Passed               bool                  `json:"passed"`
}

type ForeignKeyViolation struct {
	Table      string `json:"table"`
	RowID      *int64 `json:"rowid,omitempty"`
	Parent     string `json:"parent"`
	Constraint int64  `json:"constraint"`
}

type OrphanReport struct {
	MessageConversations     int64 `json:"message_conversations"`
	MessageSenders           int64 `json:"message_senders"`
	ConversationParticipants int64 `json:"conversation_participants"`
	MessageAttachments       int64 `json:"message_attachments"`
	OutboxAccounts           int64 `json:"outbox_accounts"`
	OutboxConversations      int64 `json:"outbox_conversations"`
	OutboxAttachments        int64 `json:"outbox_attachments"`
	ReadCursorMessages       int64 `json:"read_cursor_messages"`
}

func reconciliationRatio(reconciled, legacy int64) float64 {
	if legacy == 0 {
		return 1
	}
	return float64(reconciled) / float64(legacy)
}
