package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/maxghenis/openmessage/internal/bridge"
)

const reactionMessageQueryBatchSize = 500

// ReactionApply is one per-reactor reaction mutation for a resolved message.
// OccurredAtMS orders delta mutations; SourceSeqMS carries the enclosing source
// frame's ordering value when one is available.
type ReactionApply struct {
	AccountID      string
	ConversationID string
	MessageID      string

	ReactorKey        string
	ReactorIdentityID *string
	ReactorIsSelf     bool
	ReactorLabel      string
	Emoji             string
	Action            bridge.ReactionAction
	OccurredAtMS      int64
	SourceSeqMS       int64
}

// ReactionSnapshotEntry is one active reactor in a full embedded reaction
// snapshot. Reactors absent from a newer snapshot are tombstoned.
type ReactionSnapshotEntry struct {
	ReactorKey        string
	ReactorIdentityID *string
	ReactorIsSelf     bool
	ReactorLabel      string
	Emoji             string
}

// ReactionRow is the active read shape consumed by the v2 DTO mapper.
type ReactionRow struct {
	MessageID        string
	Emoji            string
	ReactorIsSelf    bool
	ReactorCanonical string
	ReactorLabel     string
	OccurredAtMS     int64
}

// ReactionRepository owns the inbound reaction read model.
type ReactionRepository struct {
	store *Store
	now   func() time.Time
}

// NewReactionRepository creates a reaction repository. The clock is required
// so storage-owned creation and update timestamps are deterministic in tests.
func NewReactionRepository(
	store *Store,
	now func() time.Time,
) (*ReactionRepository, error) {
	if store == nil || store.db == nil {
		return nil, fmt.Errorf("create reaction repository: store is nil")
	}
	if now == nil {
		return nil, fmt.Errorf("create reaction repository: now function is nil")
	}
	return &ReactionRepository{store: store, now: now}, nil
}

// ApplyReaction applies one WhatsApp, Signal, or tapback delta. A newer
// removal preserves the previous emoji for audit; a removal received before
// any add inserts an empty-emoji tombstone that fences stale adds. Delta
// application neither reads nor writes the embedded-snapshot fence table.
func (r *ReactionRepository) ApplyReaction(
	ctx context.Context,
	reaction ReactionApply,
) (bool, error) {
	state := "active"
	emoji := reaction.Emoji
	switch reaction.Action {
	case bridge.ReactionAdd, bridge.ReactionSwitch:
	case bridge.ReactionRemove:
		state = "removed"
		// A remove with no prior row inserts an empty tombstone. The conflict
		// branch below deliberately retains reactions.emoji instead.
		emoji = ""
	default:
		return false, fmt.Errorf(
			"apply reaction (%q, %q): unsupported action %q",
			reaction.MessageID,
			reaction.ReactorKey,
			reaction.Action,
		)
	}

	nowMS, err := r.nowMS("apply reaction")
	if err != nil {
		return false, err
	}
	result, err := r.store.db.ExecContext(ctx, `
		INSERT INTO reactions (
			message_id,
			reactor_key,
			account_id,
			conversation_id,
			reactor_identity_id,
			reactor_is_self,
			reactor_label,
			emoji,
			state,
			occurred_at_ms,
			source_seq_ms,
			created_at_ms,
			updated_at_ms
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(message_id, reactor_key) DO UPDATE SET
			reactor_identity_id = excluded.reactor_identity_id,
			reactor_is_self = excluded.reactor_is_self,
			reactor_label = excluded.reactor_label,
			emoji = CASE
				WHEN excluded.state = 'removed' THEN reactions.emoji
				ELSE excluded.emoji
			END,
			state = excluded.state,
			occurred_at_ms = excluded.occurred_at_ms,
			source_seq_ms = excluded.source_seq_ms,
			updated_at_ms = excluded.updated_at_ms
		WHERE excluded.occurred_at_ms > reactions.occurred_at_ms
		   OR (
			excluded.occurred_at_ms = reactions.occurred_at_ms
			AND (
				reactions.reactor_identity_id IS NOT excluded.reactor_identity_id
				OR reactions.reactor_is_self IS NOT excluded.reactor_is_self
				OR reactions.reactor_label IS NOT excluded.reactor_label
				OR reactions.state IS NOT excluded.state
				OR (
					excluded.state = 'active'
					AND reactions.emoji IS NOT excluded.emoji
				)
				OR reactions.source_seq_ms IS NOT excluded.source_seq_ms
			)
		   )
	`,
		reaction.MessageID,
		reaction.ReactorKey,
		reaction.AccountID,
		reaction.ConversationID,
		reaction.ReactorIdentityID,
		reaction.ReactorIsSelf,
		reaction.ReactorLabel,
		emoji,
		state,
		reaction.OccurredAtMS,
		reaction.SourceSeqMS,
		nowMS,
		nowMS,
	)
	if err != nil {
		return false, fmt.Errorf(
			"apply reaction (%q, %q): %w",
			reaction.MessageID,
			reaction.ReactorKey,
			mapConstraintError(err),
		)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf(
			"apply reaction (%q, %q): read rows affected: %w",
			reaction.MessageID,
			reaction.ReactorKey,
			err,
		)
	}
	if affected != 0 && affected != 1 {
		return false, fmt.Errorf(
			"apply reaction (%q, %q): affected %d rows, want at most 1",
			reaction.MessageID,
			reaction.ReactorKey,
			affected,
		)
	}
	return affected == 1, nil
}

// ReactionSnapshotResult counts semantic changes made by a full snapshot.
type ReactionSnapshotResult struct {
	Applied int
	Removed int
}

// ReplaceEmbeddedReactions reconciles a Google embedded full snapshot in one
// transaction. Snapshot ordering is fenced by sourceSeqMS rather than the
// per-reaction occurrence timestamp used by ApplyReaction.
func (r *ReactionRepository) ReplaceEmbeddedReactions(
	ctx context.Context,
	messageID string,
	accountID string,
	conversationID string,
	entries []ReactionSnapshotEntry,
	sourceSeqMS int64,
) (ReactionSnapshotResult, error) {
	var changes ReactionSnapshotResult
	tx, err := r.store.db.BeginTx(ctx, nil)
	if err != nil {
		return changes, fmt.Errorf(
			"replace embedded reactions for message %q: begin transaction: %w",
			messageID,
			err,
		)
	}
	defer tx.Rollback()

	var storedSourceSeqMS sql.NullInt64
	if err := tx.QueryRowContext(ctx, `
		SELECT source_seq_ms
		FROM reaction_snapshot_fences
		WHERE message_id = ?
	`, messageID).Scan(&storedSourceSeqMS); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return changes, fmt.Errorf(
			"replace embedded reactions for message %q: read source fence: %w",
			messageID,
			err,
		)
	}
	if storedSourceSeqMS.Valid && sourceSeqMS < storedSourceSeqMS.Int64 {
		return changes, nil
	}

	nowMS, err := r.nowMS("replace embedded reactions")
	if err != nil {
		return changes, err
	}
	present := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		present[entry.ReactorKey] = struct{}{}
		var storedIdentity sql.NullString
		var storedIsSelf bool
		var storedLabel, storedEmoji, storedState string
		rowErr := tx.QueryRowContext(ctx, `
			SELECT reactor_identity_id, reactor_is_self, reactor_label, emoji, state
			FROM reactions
			WHERE message_id = ? AND reactor_key = ?
		`, messageID, entry.ReactorKey).Scan(
			&storedIdentity, &storedIsSelf, &storedLabel, &storedEmoji, &storedState,
		)
		if rowErr != nil && !errors.Is(rowErr, sql.ErrNoRows) {
			return changes, fmt.Errorf(
				"replace embedded reactions for message %q reactor %q: read semantic state: %w",
				messageID, entry.ReactorKey, rowErr,
			)
		}
		semanticChange := errors.Is(rowErr, sql.ErrNoRows) ||
			storedIdentity.Valid != (entry.ReactorIdentityID != nil) ||
			(storedIdentity.Valid && storedIdentity.String != *entry.ReactorIdentityID) ||
			storedIsSelf != entry.ReactorIsSelf || storedLabel != entry.ReactorLabel ||
			storedEmoji != entry.Emoji || storedState != "active"
		result, err := tx.ExecContext(ctx, `
			INSERT INTO reactions (
				message_id,
				reactor_key,
				account_id,
				conversation_id,
				reactor_identity_id,
				reactor_is_self,
				reactor_label,
				emoji,
				state,
				occurred_at_ms,
				source_seq_ms,
				created_at_ms,
				updated_at_ms
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'active', ?, ?, ?, ?)
			ON CONFLICT(message_id, reactor_key) DO UPDATE SET
				reactor_identity_id = excluded.reactor_identity_id,
				reactor_is_self = excluded.reactor_is_self,
				reactor_label = excluded.reactor_label,
				emoji = excluded.emoji,
				state = 'active',
				occurred_at_ms = excluded.occurred_at_ms,
				source_seq_ms = excluded.source_seq_ms,
				updated_at_ms = excluded.updated_at_ms
			WHERE excluded.source_seq_ms > reactions.source_seq_ms
			   OR (
				excluded.source_seq_ms = reactions.source_seq_ms
				AND (
					reactions.reactor_identity_id IS NOT excluded.reactor_identity_id
					OR reactions.reactor_is_self IS NOT excluded.reactor_is_self
					OR reactions.reactor_label IS NOT excluded.reactor_label
					OR reactions.emoji IS NOT excluded.emoji
					OR reactions.state IS NOT 'active'
				)
			   )
		`,
			messageID,
			entry.ReactorKey,
			accountID,
			conversationID,
			entry.ReactorIdentityID,
			entry.ReactorIsSelf,
			entry.ReactorLabel,
			entry.Emoji,
			nowMS,
			sourceSeqMS,
			nowMS,
			nowMS,
		)
		if err != nil {
			return changes, fmt.Errorf(
				"replace embedded reactions for message %q reactor %q: %w",
				messageID,
				entry.ReactorKey,
				mapConstraintError(err),
			)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return changes, fmt.Errorf(
				"replace embedded reactions for message %q reactor %q: read rows affected: %w",
				messageID,
				entry.ReactorKey,
				err,
			)
		}
		if affected != 0 && affected != 1 {
			return changes, fmt.Errorf(
				"replace embedded reactions for message %q reactor %q: affected %d rows, want at most 1",
				messageID,
				entry.ReactorKey,
				affected,
			)
		}
		if affected == 1 && semanticChange {
			changes.Applied++
		}
	}

	conditions := []string{"message_id = ?", "state = 'active'"}
	arguments := []any{nowMS, sourceSeqMS, nowMS, messageID}
	if len(present) > 0 {
		keys := make([]string, 0, len(present))
		for key := range present {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(keys)), ",")
		conditions = append(conditions, "reactor_key NOT IN ("+placeholders+")")
		for _, key := range keys {
			arguments = append(arguments, key)
		}
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE reactions
		SET
			state = 'removed',
			occurred_at_ms = ?,
			source_seq_ms = ?,
			updated_at_ms = ?
		WHERE `+strings.Join(conditions, " AND "), arguments...)
	if err != nil {
		return changes, fmt.Errorf(
			"replace embedded reactions for message %q: tombstone absent reactors: %w",
			messageID,
			mapConstraintError(err),
		)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return changes, fmt.Errorf(
			"replace embedded reactions for message %q: read tombstoned rows affected: %w",
			messageID,
			err,
		)
	}
	changes.Removed = int(affected)

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO reaction_snapshot_fences (message_id, source_seq_ms, updated_at_ms)
		VALUES (?, ?, ?)
		ON CONFLICT(message_id) DO UPDATE SET
			source_seq_ms = excluded.source_seq_ms,
			updated_at_ms = excluded.updated_at_ms
	`, messageID, sourceSeqMS, nowMS); err != nil {
		return changes, fmt.Errorf(
			"replace embedded reactions for message %q: write source fence: %w",
			messageID,
			mapConstraintError(err),
		)
	}

	if err := tx.Commit(); err != nil {
		return changes, fmt.Errorf(
			"replace embedded reactions for message %q: commit: %w",
			messageID,
			mapConstraintError(err),
		)
	}
	return changes, nil
}

// SeedReaction inserts one active migration row inside the caller's write
// transaction. The first row for a (message, reactor) pair wins, normalizing
// legacy multi-emoji duplicates to the one-reaction-per-person model. Seeds
// neither read nor write the embedded-snapshot fence table.
func (r *ReactionRepository) SeedReaction(
	ctx context.Context,
	tx *sql.Tx,
	reaction ReactionApply,
) error {
	if tx == nil {
		return fmt.Errorf(
			"seed reaction (%q, %q): transaction is nil",
			reaction.MessageID,
			reaction.ReactorKey,
		)
	}
	nowMS, err := r.nowMS("seed reaction")
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO reactions (
			message_id,
			reactor_key,
			account_id,
			conversation_id,
			reactor_identity_id,
			reactor_is_self,
			reactor_label,
			emoji,
			state,
			occurred_at_ms,
			source_seq_ms,
			created_at_ms,
			updated_at_ms
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'active', ?, 0, ?, ?)
		ON CONFLICT(message_id, reactor_key) DO NOTHING
	`,
		reaction.MessageID,
		reaction.ReactorKey,
		reaction.AccountID,
		reaction.ConversationID,
		reaction.ReactorIdentityID,
		reaction.ReactorIsSelf,
		reaction.ReactorLabel,
		reaction.Emoji,
		reaction.OccurredAtMS,
		nowMS,
		nowMS,
	); err != nil {
		return fmt.Errorf(
			"seed reaction (%q, %q): %w",
			reaction.MessageID,
			reaction.ReactorKey,
			mapConstraintError(err),
		)
	}
	return nil
}

// ReactionsForMessages loads active reactions for a batch of messages in a
// stable order suitable for deterministic legacy-shape aggregation.
func (r *ReactionRepository) ReactionsForMessages(
	ctx context.Context,
	messageIDs []string,
) (map[string][]ReactionRow, error) {
	result := make(map[string][]ReactionRow)
	if len(messageIDs) == 0 {
		return result, nil
	}

	unique := make([]string, 0, len(messageIDs))
	seen := make(map[string]struct{}, len(messageIDs))
	for _, messageID := range messageIDs {
		if _, exists := seen[messageID]; exists {
			continue
		}
		seen[messageID] = struct{}{}
		unique = append(unique, messageID)
	}
	for start := 0; start < len(unique); start += reactionMessageQueryBatchSize {
		end := min(start+reactionMessageQueryBatchSize, len(unique))
		batch := unique[start:end]
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(batch)), ",")
		arguments := make([]any, len(batch))
		for index, messageID := range batch {
			arguments[index] = messageID
		}

		rows, err := r.store.db.QueryContext(ctx, `
			SELECT
				r.message_id,
				r.emoji,
				r.reactor_is_self,
				COALESCE(i.canonical_value, ''),
				r.reactor_label,
				r.occurred_at_ms
			FROM reactions AS r
			LEFT JOIN identities AS i
			  ON i.identity_id = r.reactor_identity_id
			WHERE r.message_id IN (`+placeholders+`)
			  AND r.state = 'active'
			ORDER BY r.message_id, r.occurred_at_ms, r.reactor_key
		`, arguments...)
		if err != nil {
			return nil, fmt.Errorf("list reactions for messages: %w", err)
		}
		for rows.Next() {
			var row ReactionRow
			if err := rows.Scan(
				&row.MessageID,
				&row.Emoji,
				&row.ReactorIsSelf,
				&row.ReactorCanonical,
				&row.ReactorLabel,
				&row.OccurredAtMS,
			); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("scan reaction for messages: %w", err)
			}
			result[row.MessageID] = append(result[row.MessageID], row)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("iterate reactions for messages: %w", err)
		}
		if err := rows.Close(); err != nil {
			return nil, fmt.Errorf("close reactions for messages: %w", err)
		}
	}
	return result, nil
}

func (r *ReactionRepository) nowMS(operation string) (int64, error) {
	nowMS := r.now().UnixMilli()
	if nowMS <= 0 {
		return 0, fmt.Errorf("%s: current Unix time is not positive", operation)
	}
	return nowMS, nil
}
