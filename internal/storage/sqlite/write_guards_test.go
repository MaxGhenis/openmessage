package sqlite

// Tests for the ingest write guards: the change-guarded identity and
// conversation upserts, the diffing roster sync, the recency bump and the
// reaction fence write. Each is compared with the statement it replaced, kept
// here verbatim, over random operation sequences (differential properties):
// every column but the update time must match after every step, and the
// guarded update time must be the time of the row's last content change where
// the replaced statement stamped every refresh.
//
// Invariants:
//   - Content: for every operation sequence, identities, conversations,
//     conversation_participants, reactions and the fence's sequence equal what
//     the replaced statements produce, and errors fall in the same classes.
//   - Update time: a guarded row's updated_at_ms is the time of its last
//     content change (created_at_ms if it never changed); it never exceeds the
//     replaced statement's value.
//   - No-op: a write that changes nothing touches no row.
//   - Paging: walking UnprocessedAfter from the zero cursor visits every
//     unprocessed frame exactly once, in (received_at_ms, inbox_id) order.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"testing/quick"
	"time"
)

const guardTestTimeMS int64 = 1_900_000_000_000

// legacyUpsertIdentitySQL is UpsertIdentity's statement before the guard.
const legacyUpsertIdentitySQL = `
		INSERT INTO identities (
			identity_id,
			account_id,
			kind,
			canonical_value,
			raw_value,
			display_name,
			is_self,
			metadata_json,
			created_at_ms,
			updated_at_ms
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(account_id, kind, canonical_value) DO UPDATE SET
			raw_value = excluded.raw_value,
			display_name = excluded.display_name,
			is_self = excluded.is_self,
			metadata_json = excluded.metadata_json,
			updated_at_ms = excluded.updated_at_ms
	`

// legacyUpsertConversationSQL is UpsertConversation's statement before the
// guard.
const legacyUpsertConversationSQL = `
		INSERT INTO conversations (
			conversation_id,
			account_id,
			remote_conversation_id,
			kind,
			title,
			remote_revision,
			notification_mode,
			is_favorite,
			archived_at_ms,
			last_message_at_ms,
			metadata_json,
			created_at_ms,
			updated_at_ms
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(account_id, remote_conversation_id) DO UPDATE SET
			kind = excluded.kind,
			title = excluded.title,
			remote_revision = excluded.remote_revision,
			notification_mode = excluded.notification_mode,
			is_favorite = excluded.is_favorite,
			archived_at_ms = excluded.archived_at_ms,
			last_message_at_ms = excluded.last_message_at_ms,
			metadata_json = excluded.metadata_json,
			updated_at_ms = excluded.updated_at_ms
	`

// legacyBumpConversationRecencySQL is BumpConversationRecency's statement
// before the guard.
const legacyBumpConversationRecencySQL = `UPDATE conversations
		 SET last_message_at_ms = MAX(COALESCE(last_message_at_ms, 0), ?)
		 WHERE conversation_id = ?`

// legacyReplaceConversationParticipants is ReplaceConversationParticipants
// before it diffed the roster (origin/main 19e35d9), verbatim but for the
// receiver and name: delete every row, then insert the list.
func legacyReplaceConversationParticipants(
	s *Store,
	conversationID string,
	participants []ConversationParticipant,
) error {
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("replace participants for conversation %q: begin transaction: %w", conversationID, err)
	}
	defer tx.Rollback()

	var conversationAccountID string
	if err := tx.QueryRowContext(
		ctx,
		`SELECT account_id FROM conversations WHERE conversation_id = ?`,
		conversationID,
	).Scan(&conversationAccountID); errors.Is(err, sql.ErrNoRows) {
		return notFound("conversation", conversationID)
	} else if err != nil {
		return fmt.Errorf("replace participants for conversation %q: read account: %w", conversationID, err)
	}

	if _, err := tx.ExecContext(
		ctx,
		`DELETE FROM conversation_participants WHERE conversation_id = ?`,
		conversationID,
	); err != nil {
		return fmt.Errorf("replace participants for conversation %q: delete existing rows: %w", conversationID, err)
	}

	for i, participant := range participants {
		if participant.ConversationID != "" && participant.ConversationID != conversationID {
			return invalidParticipantError(
				nil,
				"participant %d names conversation %q while replacing %q",
				i,
				participant.ConversationID,
				conversationID,
			)
		}

		accountID := participant.AccountID
		if accountID == "" {
			accountID = conversationAccountID
		}
		if accountID != conversationAccountID {
			return invalidParticipantError(
				ErrCrossAccountParticipant,
				"participant %d account %q does not match conversation account %q",
				i,
				accountID,
				conversationAccountID,
			)
		}

		if _, err := tx.ExecContext(ctx, `
			INSERT INTO conversation_participants (
				account_id,
				conversation_id,
				identity_id,
				role,
				display_name,
				is_active,
				joined_at_ms,
				left_at_ms
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		`,
			accountID,
			conversationID,
			participant.IdentityID,
			participant.Role,
			participant.DisplayName,
			participant.IsActive,
			participant.JoinedAtMS,
			participant.LeftAtMS,
		); err != nil {
			if isSQLiteErrorCode(err, sqliteConstraintForeignKeyCode) {
				specific, classifyErr := classifyParticipantForeignKey(
					ctx,
					tx,
					conversationAccountID,
					participant.IdentityID,
				)
				if classifyErr != nil {
					return fmt.Errorf(
						"replace participants for conversation %q: classify participant %d constraint: %w",
						conversationID,
						i,
						classifyErr,
					)
				}
				return invalidParticipantConstraintError(
					specific,
					err,
					"insert participant %d identity %q",
					i,
					participant.IdentityID,
				)
			}
			if isSQLiteConstraint(err) {
				return invalidParticipantConstraintError(
					nil,
					err,
					"insert participant %d identity %q",
					i,
					participant.IdentityID,
				)
			}
			return fmt.Errorf(
				"replace participants for conversation %q: insert participant %d: %w",
				conversationID,
				i,
				err,
			)
		}
	}

	if err := tx.Commit(); err != nil {
		if isSQLiteErrorCode(err, sqliteConstraintForeignKeyCode) {
			return fmt.Errorf(
				"%w: %w: commit participant replacement: %w",
				ErrInvalidConversationParticipant,
				ErrConstraintViolation,
				err,
			)
		}
		return fmt.Errorf("replace participants for conversation %q: commit: %w", conversationID, err)
	}
	return nil
}

// legacyReplaceEmbeddedReactions is ReplaceEmbeddedReactions as it stood
// before the fence write was guarded (origin/main 19e35d9), verbatim but for
// this comment and the name: the reference the guarded version is compared to.
func legacyReplaceEmbeddedReactions(
	r *ReactionRepository,
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

func openGuardTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "store.sqlite3"))
	if err != nil {
		t.Fatalf("Open(): %v", err)
	}
	return store
}

func closeGuardTestStore(t *testing.T, store *Store) {
	t.Helper()
	if err := store.Close(); err != nil {
		t.Errorf("Close(): %v", err)
	}
}

// guardErrorClasses are the sentinels a caller can branch on; two errors are
// equivalent when they agree on every one of them.
var guardErrorClasses = []error{
	ErrNotFound,
	ErrConstraintViolation,
	ErrInvalidConversationParticipant,
	ErrCrossAccountParticipant,
	ErrOrphanParticipantIdentity,
}

func sameErrorClass(left, right error) bool {
	if (left == nil) != (right == nil) {
		return false
	}
	for _, class := range guardErrorClasses {
		if errors.Is(left, class) != errors.Is(right, class) {
			return false
		}
	}
	return true
}

// dumpTable returns every row of table as strings, ordered by every column,
// with the named columns left out.
func dumpTable(t *testing.T, store *Store, table string, omit ...string) []string {
	t.Helper()
	skip := make(map[string]bool, len(omit))
	for _, column := range omit {
		skip[column] = true
	}
	rows, err := store.db.Query(`SELECT name FROM pragma_table_info(?) ORDER BY cid`, table)
	if err != nil {
		t.Fatalf("columns of %s: %v", table, err)
	}
	var columns []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan column of %s: %v", table, err)
		}
		if !skip[name] {
			columns = append(columns, name)
		}
	}
	_ = rows.Close()
	selected := strings.Join(columns, ", ")
	dataRows, err := store.db.Query(`SELECT ` + selected + ` FROM ` + table + ` ORDER BY ` + selected)
	if err != nil {
		t.Fatalf("dump %s: %v", table, err)
	}
	defer dataRows.Close()
	var dumped []string
	for dataRows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for index := range values {
			pointers[index] = &values[index]
		}
		if err := dataRows.Scan(pointers...); err != nil {
			t.Fatalf("scan %s: %v", table, err)
		}
		var line strings.Builder
		for index, value := range values {
			if raw, ok := value.([]byte); ok {
				value = string(raw)
			}
			fmt.Fprintf(&line, "%s=%v;", columns[index], value)
		}
		dumped = append(dumped, line.String())
	}
	if err := dataRows.Err(); err != nil {
		t.Fatalf("iterate %s: %v", table, err)
	}
	return dumped
}

func updatedAtByKey(t *testing.T, store *Store, query string) map[string]int64 {
	t.Helper()
	rows, err := store.db.Query(query)
	if err != nil {
		t.Fatalf("read update times: %v", err)
	}
	defer rows.Close()
	result := make(map[string]int64)
	for rows.Next() {
		var key string
		var updatedAtMS int64
		if err := rows.Scan(&key, &updatedAtMS); err != nil {
			t.Fatalf("scan update time: %v", err)
		}
		result[key] = updatedAtMS
	}
	return result
}

// installWriteCounter makes every row insert, update and delete on tables
// count into test_row_writes, so a test can assert that a no-op wrote nothing.
func installWriteCounter(t *testing.T, store *Store, tables ...string) {
	t.Helper()
	statements := []string{`CREATE TABLE test_row_writes (table_name TEXT PRIMARY KEY, writes INTEGER NOT NULL)`}
	for _, table := range tables {
		statements = append(statements, fmt.Sprintf(`INSERT INTO test_row_writes VALUES ('%s', 0)`, table))
		for _, event := range []string{"INSERT", "UPDATE", "DELETE"} {
			statements = append(statements, fmt.Sprintf(`
				CREATE TRIGGER test_count_%s_%s AFTER %s ON %s
				BEGIN
					UPDATE test_row_writes SET writes = writes + 1 WHERE table_name = '%s';
				END`, table, strings.ToLower(event), event, table, table))
		}
	}
	for _, statement := range statements {
		if _, err := store.db.Exec(statement); err != nil {
			t.Fatalf("install write counter: %v", err)
		}
	}
}

func rowWrites(t *testing.T, store *Store, table string) int64 {
	t.Helper()
	var writes int64
	if err := store.db.QueryRow(`SELECT writes FROM test_row_writes WHERE table_name = ?`, table).Scan(&writes); err != nil {
		t.Fatalf("read row writes for %s: %v", table, err)
	}
	return writes
}

func pickString(rng *rand.Rand, values ...string) string {
	return values[rng.Intn(len(values))]
}

func optionalInt(rng *rand.Rand, values ...int64) *int64 {
	index := rng.Intn(len(values) + 1)
	if index == len(values) {
		return nil
	}
	value := values[index]
	return &value
}

func TestUpsertIdentityMatchesUnguardedUpsertProperty(t *testing.T) {
	property := func(seed int64) bool {
		rng := rand.New(rand.NewSource(seed))
		guarded, reference := openGuardTestStore(t), openGuardTestStore(t)
		defer closeGuardTestStore(t, guarded)
		defer closeGuardTestStore(t, reference)
		for _, store := range []*Store{guarded, reference} {
			mustRepositoryWrite(t, "seed account", store.UpsertAccount(repositoryTestAccount("account-a")))
		}

		lastChange := make(map[string]int64)
		nowMS := guardTestTimeMS
		for step := 0; step < 30; step++ {
			nowMS += int64(rng.Intn(3))
			kind := pickString(rng, "phone", "email")
			canonical := pickString(rng, "a", "b", "c")
			identity := Identity{
				IdentityID:     "identity-" + kind + "-" + canonical,
				AccountID:      "account-a",
				Kind:           IdentityKind(kind),
				CanonicalValue: canonical,
				RawValue:       pickString(rng, "raw-1", "raw-2", "raw-2", "  "),
				DisplayName:    pickString(rng, "", "Ann", "Bob"),
				IsSelf:         rng.Intn(4) == 0,
				MetadataJSON:   pickString(rng, `{}`, `{}`, `{"k":1}`, `not json`),
				CreatedAtMS:    nowMS,
				UpdatedAtMS:    nowMS,
			}
			before := dumpTable(t, reference, "identities", "updated_at_ms")
			guardedErr := guarded.UpsertIdentity(identity)
			_, referenceErr := reference.db.Exec(legacyUpsertIdentitySQL,
				identity.IdentityID, identity.AccountID, identity.Kind, identity.CanonicalValue,
				identity.RawValue, identity.DisplayName, identity.IsSelf, identity.MetadataJSON,
				identity.CreatedAtMS, identity.UpdatedAtMS)
			if (guardedErr == nil) != (referenceErr == nil) {
				t.Logf("seed %d step %d: errors differ: guarded %v, reference %v", seed, step, guardedErr, referenceErr)
				return false
			}
			after := dumpTable(t, reference, "identities", "updated_at_ms")
			if referenceErr == nil && !reflect.DeepEqual(before, after) {
				lastChange[identity.IdentityID] = nowMS
			}
			if got := dumpTable(t, guarded, "identities", "updated_at_ms"); !reflect.DeepEqual(got, after) {
				t.Logf("seed %d step %d: content differs:\n%v\n%v", seed, step, got, after)
				return false
			}
			guardedTimes := updatedAtByKey(t, guarded, `SELECT identity_id, updated_at_ms FROM identities`)
			referenceTimes := updatedAtByKey(t, reference, `SELECT identity_id, updated_at_ms FROM identities`)
			for id, updatedAtMS := range guardedTimes {
				if updatedAtMS != lastChange[id] || updatedAtMS > referenceTimes[id] {
					t.Logf("seed %d step %d: %s updated_at %d, last change %d, reference %d",
						seed, step, id, updatedAtMS, lastChange[id], referenceTimes[id])
					return false
				}
			}
		}
		return true
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 25, Rand: rand.New(rand.NewSource(20261009))}); err != nil {
		t.Fatal(err)
	}
}

func TestUpsertConversationMatchesUnguardedUpsertProperty(t *testing.T) {
	property := func(seed int64) bool {
		rng := rand.New(rand.NewSource(seed))
		guarded, reference := openGuardTestStore(t), openGuardTestStore(t)
		defer closeGuardTestStore(t, guarded)
		defer closeGuardTestStore(t, reference)
		for _, store := range []*Store{guarded, reference} {
			mustRepositoryWrite(t, "seed account", store.UpsertAccount(repositoryTestAccount("account-a")))
		}

		lastChange := make(map[string]int64)
		nowMS := guardTestTimeMS
		for step := 0; step < 30; step++ {
			nowMS += int64(rng.Intn(3))
			remoteID := pickString(rng, "r1", "r2", "r3")
			conversationID := "conversation-" + remoteID
			if rng.Intn(6) == 0 {
				// A conflicting insert under another primary key keeps the
				// stored row's ID.
				conversationID += "-other"
			}
			var revision *string
			if value := pickString(rng, "", "v1", "v2"); value != "" {
				revision = &value
			}
			conversation := Conversation{
				ConversationID:       conversationID,
				AccountID:            "account-a",
				RemoteConversationID: remoteID,
				Kind:                 ConversationKind(pickString(rng, "direct", "direct", "group", "bogus")),
				Title:                pickString(rng, "", "T1", "T2"),
				RemoteRevision:       revision,
				NotificationMode:     NotificationMode(pickString(rng, "all", "all", "muted")),
				IsFavorite:           rng.Intn(4) == 0,
				ArchivedAtMS:         optionalInt(rng, 0, 7),
				LastMessageAtMS:      int64(rng.Intn(3) * 10),
				MetadataJSON:         pickString(rng, `{}`, `{}`, `{"x":1}`),
				CreatedAtMS:          nowMS,
				UpdatedAtMS:          nowMS,
			}
			before := dumpTable(t, reference, "conversations", "updated_at_ms")
			guardedErr := guarded.UpsertConversation(conversation)
			_, referenceErr := reference.db.Exec(legacyUpsertConversationSQL,
				conversation.ConversationID, conversation.AccountID, conversation.RemoteConversationID,
				conversation.Kind, conversation.Title, conversation.RemoteRevision,
				conversation.NotificationMode, conversation.IsFavorite, conversation.ArchivedAtMS,
				conversation.LastMessageAtMS, conversation.MetadataJSON,
				conversation.CreatedAtMS, conversation.UpdatedAtMS)
			if (guardedErr == nil) != (referenceErr == nil) {
				t.Logf("seed %d step %d: errors differ: guarded %v, reference %v", seed, step, guardedErr, referenceErr)
				return false
			}
			after := dumpTable(t, reference, "conversations", "updated_at_ms")
			if referenceErr == nil && !reflect.DeepEqual(before, after) {
				lastChange[remoteID] = nowMS
			}
			if got := dumpTable(t, guarded, "conversations", "updated_at_ms"); !reflect.DeepEqual(got, after) {
				t.Logf("seed %d step %d: content differs:\n%v\n%v", seed, step, got, after)
				return false
			}
			guardedTimes := updatedAtByKey(t, guarded, `SELECT remote_conversation_id, updated_at_ms FROM conversations`)
			referenceTimes := updatedAtByKey(t, reference, `SELECT remote_conversation_id, updated_at_ms FROM conversations`)
			for remote, updatedAtMS := range guardedTimes {
				if updatedAtMS != lastChange[remote] || updatedAtMS > referenceTimes[remote] {
					t.Logf("seed %d step %d: %s updated_at %d, last change %d, reference %d",
						seed, step, remote, updatedAtMS, lastChange[remote], referenceTimes[remote])
					return false
				}
			}
		}
		return true
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 25, Rand: rand.New(rand.NewSource(20261010))}); err != nil {
		t.Fatal(err)
	}
}

// Every guarded column, changed on its own (NULL transitions included),
// still writes the row and moves its update time; the same values write
// nothing. Random sequences reach single-column changes only sometimes, so
// this pins each guard term directly.
func TestUpsertGuardsWriteEveryChangedColumn(t *testing.T) {
	store := openGuardTestStore(t)
	defer closeGuardTestStore(t, store)
	mustRepositoryWrite(t, "seed account", store.UpsertAccount(repositoryTestAccount("account-a")))
	installWriteCounter(t, store, "identities", "conversations")

	identity := repositoryTestIdentity("identity-a", "account-a", "+15550000001")
	mustRepositoryWrite(t, "seed identity", store.UpsertIdentity(identity))
	identityChanges := map[string]func(*Identity){
		"raw_value":      func(i *Identity) { i.RawValue = "raw-changed" },
		"display_name":   func(i *Identity) { i.DisplayName = "Named" },
		"is_self":        func(i *Identity) { i.IsSelf = !i.IsSelf },
		"metadata_json":  func(i *Identity) { i.MetadataJSON = `{"changed":true}` },
		"display_name()": func(i *Identity) { i.DisplayName = "" },
	}
	nowMS := repositoryTestTimeMS
	for _, column := range []string{"raw_value", "display_name", "is_self", "metadata_json", "display_name()"} {
		nowMS += 10
		before := rowWrites(t, store, "identities")
		identity.UpdatedAtMS = nowMS
		mustRepositoryWrite(t, "UpsertIdentity(same)", store.UpsertIdentity(identity))
		if got := rowWrites(t, store, "identities") - before; got != 0 {
			t.Fatalf("unchanged identity before %s change wrote %d rows", column, got)
		}
		identityChanges[column](&identity)
		mustRepositoryWrite(t, "UpsertIdentity("+column+")", store.UpsertIdentity(identity))
		stored, err := store.GetIdentity("identity-a")
		mustRepositoryRead(t, "GetIdentity", err)
		if got := rowWrites(t, store, "identities") - before; got != 1 || stored.UpdatedAtMS != nowMS {
			t.Fatalf("identity %s change wrote %d rows, updated_at %d; want 1 row at %d", column, got, stored.UpdatedAtMS, nowMS)
		}
		want := identity
		want.CreatedAtMS = repositoryTestTimeMS
		if stored != want {
			t.Fatalf("identity after %s change = %+v, want %+v", column, stored, want)
		}
	}

	revision, other := "v1", "v2"
	archived, archivedLater := int64(0), int64(7)
	conversation := Conversation{
		ConversationID:       "conversation-a",
		AccountID:            "account-a",
		RemoteConversationID: "remote-a",
		Kind:                 ConversationKindDirect,
		NotificationMode:     NotificationModeAll,
		MetadataJSON:         `{}`,
		CreatedAtMS:          repositoryTestTimeMS,
		UpdatedAtMS:          repositoryTestTimeMS,
	}
	mustRepositoryWrite(t, "seed conversation", store.UpsertConversation(conversation))
	conversationChanges := []struct {
		column string
		change func(*Conversation)
	}{
		{"kind", func(c *Conversation) { c.Kind = ConversationKindGroup }},
		{"title", func(c *Conversation) { c.Title = "Title" }},
		{"remote_revision (NULL to value)", func(c *Conversation) { c.RemoteRevision = &revision }},
		{"remote_revision (value to value)", func(c *Conversation) { c.RemoteRevision = &other }},
		{"remote_revision (value to NULL)", func(c *Conversation) { c.RemoteRevision = nil }},
		{"notification_mode", func(c *Conversation) { c.NotificationMode = NotificationModeMuted }},
		{"is_favorite", func(c *Conversation) { c.IsFavorite = true }},
		{"archived_at_ms (NULL to 0)", func(c *Conversation) { c.ArchivedAtMS = &archived }},
		{"archived_at_ms (0 to 7)", func(c *Conversation) { c.ArchivedAtMS = &archivedLater }},
		{"archived_at_ms (7 to NULL)", func(c *Conversation) { c.ArchivedAtMS = nil }},
		{"last_message_at_ms", func(c *Conversation) { c.LastMessageAtMS = 42 }},
		{"metadata_json", func(c *Conversation) { c.MetadataJSON = `{"changed":true}` }},
	}
	for _, step := range conversationChanges {
		nowMS += 10
		before := rowWrites(t, store, "conversations")
		conversation.UpdatedAtMS = nowMS
		mustRepositoryWrite(t, "UpsertConversation(same)", store.UpsertConversation(conversation))
		if got := rowWrites(t, store, "conversations") - before; got != 0 {
			t.Fatalf("unchanged conversation before %s change wrote %d rows", step.column, got)
		}
		step.change(&conversation)
		mustRepositoryWrite(t, "UpsertConversation("+step.column+")", store.UpsertConversation(conversation))
		stored, err := store.GetConversation("conversation-a")
		mustRepositoryRead(t, "GetConversation", err)
		if got := rowWrites(t, store, "conversations") - before; got != 1 || stored.UpdatedAtMS != nowMS {
			t.Fatalf("conversation %s change wrote %d rows, updated_at %d; want 1 row at %d", step.column, got, stored.UpdatedAtMS, nowMS)
		}
		if !reflect.DeepEqual(stored, conversation) {
			t.Fatalf("conversation after %s change = %+v, want %+v", step.column, stored, conversation)
		}
	}
}

// seedRosterGraph seeds two accounts, their identities and conversations for
// the roster properties.
func seedRosterGraph(t *testing.T, store *Store) {
	t.Helper()
	for _, accountID := range []string{"account-a", "account-b"} {
		mustRepositoryWrite(t, "seed account", store.UpsertAccount(repositoryTestAccount(accountID)))
	}
	for index := 1; index <= 5; index++ {
		id := fmt.Sprintf("id-%d", index)
		mustRepositoryWrite(t, "seed identity", store.UpsertIdentity(repositoryTestIdentity(id, "account-a", id)))
	}
	mustRepositoryWrite(t, "seed identity", store.UpsertIdentity(repositoryTestIdentity("idb-1", "account-b", "idb-1")))
	for _, conversation := range []struct{ id, account string }{
		{"conversation-a", "account-a"},
		{"conversation-b", "account-a"},
		{"conversation-x", "account-b"},
	} {
		mustRepositoryWrite(t, "seed conversation", store.UpsertConversation(Conversation{
			ConversationID:       conversation.id,
			AccountID:            conversation.account,
			RemoteConversationID: "remote-" + conversation.id,
			Kind:                 ConversationKindGroup,
			NotificationMode:     NotificationModeAll,
			MetadataJSON:         `{}`,
			CreatedAtMS:          guardTestTimeMS,
			UpdatedAtMS:          guardTestTimeMS,
		}))
	}
}

func randomRoster(rng *rand.Rand, target string) []ConversationParticipant {
	count := rng.Intn(6)
	roster := make([]ConversationParticipant, 0, count)
	for index := 0; index < count; index++ {
		participant := ConversationParticipant{
			IdentityID:  pickString(rng, "id-1", "id-2", "id-3", "id-4", "id-5", "id-1", "id-2"),
			Role:        ParticipantRole(pickString(rng, "member", "member", "admin", "owner", "unknown")),
			DisplayName: pickString(rng, "", "", "A", "B"),
			IsActive:    rng.Intn(5) != 0,
			JoinedAtMS:  optionalInt(rng, 0, 5),
			LeftAtMS:    optionalInt(rng, 9),
		}
		switch rng.Intn(40) {
		case 0:
			participant.IdentityID = "idb-1" // another account's identity
		case 1:
			participant.IdentityID = "id-missing" // orphan
		case 2:
			participant.Role = "boss" // CHECK violation
		case 3:
			negative := int64(-1)
			participant.JoinedAtMS = &negative // CHECK violation
		case 4:
			participant.AccountID = "account-b" // contradicts the conversation
		case 5:
			participant.ConversationID = "conversation-elsewhere"
		case 6:
			participant.AccountID = "account-a"
			participant.ConversationID = target
		}
		roster = append(roster, participant)
	}
	return roster
}

func TestSyncConversationParticipantsMatchesDeleteAndReinsertProperty(t *testing.T) {
	property := func(seed int64) bool {
		rng := rand.New(rand.NewSource(seed))
		guarded, reference := openGuardTestStore(t), openGuardTestStore(t)
		defer closeGuardTestStore(t, guarded)
		defer closeGuardTestStore(t, reference)
		seedRosterGraph(t, guarded)
		seedRosterGraph(t, reference)

		touchedAt := map[string]int64{
			"conversation-a": guardTestTimeMS,
			"conversation-b": guardTestTimeMS,
			"conversation-x": guardTestTimeMS,
		}
		nowMS := guardTestTimeMS
		for step := 0; step < 25; step++ {
			nowMS += int64(rng.Intn(3))
			target := pickString(rng, "conversation-a", "conversation-a", "conversation-b", "conversation-x", "conversation-missing")
			roster := randomRoster(rng, target)
			if rng.Intn(4) == 0 {
				// Re-send the stored roster, the common no-op.
				stored, err := reference.ListParticipants(target)
				if err == nil {
					roster = stored
				}
			}
			before := dumpTable(t, reference, "conversation_participants")
			changed, guardedErr := guarded.SyncConversationParticipants(target, roster, nowMS)
			referenceErr := legacyReplaceConversationParticipants(reference, target, roster)
			if !sameErrorClass(guardedErr, referenceErr) {
				t.Logf("seed %d step %d: errors differ: guarded %v, reference %v", seed, step, guardedErr, referenceErr)
				return false
			}
			after := dumpTable(t, reference, "conversation_participants")
			if got := dumpTable(t, guarded, "conversation_participants"); !reflect.DeepEqual(got, after) {
				t.Logf("seed %d step %d: rosters differ:\n%v\n%v", seed, step, got, after)
				return false
			}
			rosterChanged := !reflect.DeepEqual(before, after)
			if changed != rosterChanged {
				t.Logf("seed %d step %d: changed = %v, roster changed = %v", seed, step, changed, rosterChanged)
				return false
			}
			if changed && touchedAt[target] < nowMS {
				touchedAt[target] = nowMS
			}
			if got, want := dumpTable(t, guarded, "conversations", "updated_at_ms"), dumpTable(t, reference, "conversations", "updated_at_ms"); !reflect.DeepEqual(got, want) {
				t.Logf("seed %d step %d: conversations differ:\n%v\n%v", seed, step, got, want)
				return false
			}
			times := updatedAtByKey(t, guarded, `SELECT conversation_id, updated_at_ms FROM conversations`)
			for id, updatedAtMS := range times {
				if updatedAtMS != touchedAt[id] {
					t.Logf("seed %d step %d: %s updated_at %d, want %d", seed, step, id, updatedAtMS, touchedAt[id])
					return false
				}
			}
		}
		return true
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 30, Rand: rand.New(rand.NewSource(20261011))}); err != nil {
		t.Fatal(err)
	}
}

func TestReplaceEmbeddedReactionsMatchesUnguardedFenceProperty(t *testing.T) {
	property := func(seed int64) bool {
		rng := rand.New(rand.NewSource(seed))
		clock := newMessageTestClock(guardTestTimeMS)
		guardedStore, guarded := openReactionTestRepository(t, clock.Now)
		referenceStore, reference := openReactionTestRepository(t, clock.Now)
		for _, store := range []*Store{guardedStore, referenceStore} {
			seedReactionGraph(t, store, "message-a")
			seedReactionIdentity(t, store, "identity-1", "+15550000001")
			seedReactionIdentity(t, store, "identity-2", "+15550000002")
		}
		identity1, identity2 := "identity-1", "identity-2"
		reactors := []ReactionSnapshotEntry{
			{ReactorKey: "self", ReactorIsSelf: true, ReactorLabel: "me"},
			{ReactorKey: identity1, ReactorIdentityID: &identity1},
			{ReactorKey: identity2, ReactorIdentityID: &identity2},
			{ReactorKey: "anon:👍"},
		}

		fenceAdvancedAt := int64(0)
		for step := 0; step < 25; step++ {
			clock.Set(guardTestTimeMS + int64(step))
			entries := make([]ReactionSnapshotEntry, 0, len(reactors))
			for _, reactor := range reactors {
				if rng.Intn(2) == 0 {
					entry := reactor
					entry.Emoji = pickString(rng, "👍", "❤️")
					entries = append(entries, entry)
				}
			}
			sequence := int64(100 + 50*rng.Intn(5))
			beforeFence := readFenceSequence(t, referenceStore, "message-a")
			guardedResult, guardedErr := guarded.ReplaceEmbeddedReactions(
				context.Background(), "message-a", "account-a", "conversation-a", entries, sequence)
			referenceResult, referenceErr := legacyReplaceEmbeddedReactions(
				reference, context.Background(), "message-a", "account-a", "conversation-a", entries, sequence)
			if guardedErr != nil || referenceErr != nil {
				t.Logf("seed %d step %d: errors: guarded %v, reference %v", seed, step, guardedErr, referenceErr)
				return false
			}
			if guardedResult != referenceResult {
				t.Logf("seed %d step %d: results differ: %+v vs %+v", seed, step, guardedResult, referenceResult)
				return false
			}
			if got, want := dumpTable(t, guardedStore, "reactions"), dumpTable(t, referenceStore, "reactions"); !reflect.DeepEqual(got, want) {
				t.Logf("seed %d step %d: reactions differ:\n%v\n%v", seed, step, got, want)
				return false
			}
			afterFence := readFenceSequence(t, referenceStore, "message-a")
			if afterFence != beforeFence {
				fenceAdvancedAt = clock.Now().UnixMilli()
			}
			if got := readFenceSequence(t, guardedStore, "message-a"); got != afterFence {
				t.Logf("seed %d step %d: fence %d, reference %d", seed, step, got, afterFence)
				return false
			}
			if afterFence >= 0 {
				if got := readFenceUpdatedAt(t, guardedStore, "message-a"); got != fenceAdvancedAt {
					t.Logf("seed %d step %d: fence updated_at %d, want %d", seed, step, got, fenceAdvancedAt)
					return false
				}
			}
		}
		return true
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 25, Rand: rand.New(rand.NewSource(20261012))}); err != nil {
		t.Fatal(err)
	}
}

func readFenceSequence(t *testing.T, store *Store, messageID string) int64 {
	t.Helper()
	var sequence int64
	err := store.db.QueryRow(`SELECT source_seq_ms FROM reaction_snapshot_fences WHERE message_id = ?`, messageID).Scan(&sequence)
	if errors.Is(err, sql.ErrNoRows) {
		return -1
	}
	if err != nil {
		t.Fatalf("read fence: %v", err)
	}
	return sequence
}

func readFenceUpdatedAt(t *testing.T, store *Store, messageID string) int64 {
	t.Helper()
	var updatedAtMS int64
	if err := store.db.QueryRow(`SELECT updated_at_ms FROM reaction_snapshot_fences WHERE message_id = ?`, messageID).Scan(&updatedAtMS); err != nil {
		t.Fatalf("read fence update time: %v", err)
	}
	return updatedAtMS
}

// An empty snapshot's fence write is not a no-op: on a message that has never
// had a reaction, it is the only thing that stops an older snapshot carrying
// reactions (a frame retried after a transient failure) from resurrecting
// them. Dropping that write would leave the stale reaction active.
func TestEmptySnapshotFenceIsLoadBearingWithoutReactions(t *testing.T) {
	clock := newMessageTestClock(guardTestTimeMS)
	store, repository := openReactionTestRepository(t, clock.Now)
	seedReactionGraph(t, store, "message-a")
	seedReactionMessage(t, store, "message-b")
	stale := []ReactionSnapshotEntry{{ReactorKey: "self", ReactorIsSelf: true, ReactorLabel: "me", Emoji: "👍"}}

	for _, messageID := range []string{"message-a", "message-b"} {
		if _, err := repository.ReplaceEmbeddedReactions(
			context.Background(), messageID, "account-a", "conversation-a", nil, 200,
		); err != nil {
			t.Fatalf("empty snapshot for %s: %v", messageID, err)
		}
		assertReactionSnapshotFence(t, store, messageID, 200)
	}
	// Simulate skipping the empty snapshot's fence write on message-b.
	if _, err := store.db.Exec(`DELETE FROM reaction_snapshot_fences WHERE message_id = 'message-b'`); err != nil {
		t.Fatalf("drop fence: %v", err)
	}
	for _, messageID := range []string{"message-a", "message-b"} {
		if _, err := repository.ReplaceEmbeddedReactions(
			context.Background(), messageID, "account-a", "conversation-a", stale, 100,
		); err != nil {
			t.Fatalf("older snapshot for %s: %v", messageID, err)
		}
	}
	active := func(messageID string) int {
		var count int
		if err := store.db.QueryRow(`SELECT COUNT(*) FROM reactions WHERE message_id = ? AND state = 'active'`, messageID).Scan(&count); err != nil {
			t.Fatalf("count reactions: %v", err)
		}
		return count
	}
	if got := active("message-a"); got != 0 {
		t.Fatalf("message-a active reactions = %d, want 0: the empty snapshot's fence must reject the older one", got)
	}
	if got := active("message-b"); got != 1 {
		t.Fatalf("message-b active reactions = %d, want 1: without the fence the stale reaction comes back", got)
	}
}

func TestBumpConversationRecencyMatchesMaxAndWritesOnlyWhenNewerProperty(t *testing.T) {
	property := func(seed int64) bool {
		rng := rand.New(rand.NewSource(seed))
		guarded, reference := openGuardTestStore(t), openGuardTestStore(t)
		defer closeGuardTestStore(t, guarded)
		defer closeGuardTestStore(t, reference)
		seedRosterGraph(t, guarded)
		seedRosterGraph(t, reference)
		installWriteCounter(t, guarded, "conversations")
		for step := 0; step < 20; step++ {
			conversationID := pickString(rng, "conversation-a", "conversation-b", "conversation-missing")
			atMS := int64(1 + rng.Intn(5)*10)
			var current sql.NullInt64
			_ = reference.db.QueryRow(`SELECT last_message_at_ms FROM conversations WHERE conversation_id = ?`, conversationID).Scan(&current)
			writesBefore := rowWrites(t, guarded, "conversations")
			if err := guarded.BumpConversationRecency(conversationID, atMS); err != nil {
				t.Logf("guarded bump: %v", err)
				return false
			}
			if _, err := reference.db.Exec(legacyBumpConversationRecencySQL, atMS, conversationID); err != nil {
				t.Logf("reference bump: %v", err)
				return false
			}
			wantWrites := int64(0)
			if current.Valid && current.Int64 < atMS {
				wantWrites = 1
			}
			if got := rowWrites(t, guarded, "conversations") - writesBefore; got != wantWrites {
				t.Logf("seed %d step %d: bump to %d over %v wrote %d rows, want %d", seed, step, atMS, current, got, wantWrites)
				return false
			}
			if got, want := dumpTable(t, guarded, "conversations"), dumpTable(t, reference, "conversations"); !reflect.DeepEqual(got, want) {
				t.Logf("seed %d step %d: conversations differ:\n%v\n%v", seed, step, got, want)
				return false
			}
		}
		return true
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 20, Rand: rand.New(rand.NewSource(20261013))}); err != nil {
		t.Fatal(err)
	}
}

func TestUnchangedWritesTouchNoRows(t *testing.T) {
	store := openGuardTestStore(t)
	defer closeGuardTestStore(t, store)
	seedRosterGraph(t, store)
	clock := newMessageTestClock(guardTestTimeMS)
	reactions, err := NewReactionRepository(store, clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	messages := mustMessageRepository(t, store, guardTestTimeMS)
	message := messageTestMessage("message-a", "conversation-a", "account-a", "remote-message-a", nil)
	if err := messages.ImportMessage(context.Background(), MessageProjection{Message: message}); err != nil {
		t.Fatalf("ImportMessage(): %v", err)
	}
	identity, err := store.GetIdentity("id-1")
	if err != nil {
		t.Fatal(err)
	}
	conversation, err := store.GetConversation("conversation-a")
	if err != nil {
		t.Fatal(err)
	}
	roster := []ConversationParticipant{
		{IdentityID: "id-1", Role: ParticipantRoleMember, DisplayName: "A", IsActive: true},
		{IdentityID: "id-2", Role: ParticipantRoleAdmin, IsActive: true},
	}
	if err := store.ReplaceConversationParticipants("conversation-a", roster); err != nil {
		t.Fatal(err)
	}
	if err := store.BumpConversationRecency("conversation-a", 500); err != nil {
		t.Fatal(err)
	}
	conversation.LastMessageAtMS = 500
	if _, err := reactions.ReplaceEmbeddedReactions(context.Background(), "message-a", "account-a", "conversation-a", nil, 300); err != nil {
		t.Fatal(err)
	}
	tables := []string{"identities", "conversations", "conversation_participants", "reactions", "reaction_snapshot_fences"}
	installWriteCounter(t, store, tables...)

	clock.Set(guardTestTimeMS + 1000)
	identity.UpdatedAtMS = guardTestTimeMS + 1000
	conversation.UpdatedAtMS = guardTestTimeMS + 1000
	mustRepositoryWrite(t, "UpsertIdentity(unchanged)", store.UpsertIdentity(identity))
	mustRepositoryWrite(t, "UpsertConversation(unchanged)", store.UpsertConversation(conversation))
	changed, err := store.SyncConversationParticipants("conversation-a", roster, guardTestTimeMS+1000)
	mustRepositoryWrite(t, "SyncConversationParticipants(unchanged)", err)
	if changed {
		t.Fatal("SyncConversationParticipants(unchanged) reported a change")
	}
	mustRepositoryWrite(t, "ReplaceConversationParticipants(unchanged)", store.ReplaceConversationParticipants("conversation-a", roster))
	mustRepositoryWrite(t, "BumpConversationRecency(older)", store.BumpConversationRecency("conversation-a", 400))
	mustRepositoryWrite(t, "BumpConversationRecency(same)", store.BumpConversationRecency("conversation-a", 500))
	if _, err := reactions.ReplaceEmbeddedReactions(context.Background(), "message-a", "account-a", "conversation-a", nil, 300); err != nil {
		t.Fatal(err)
	}
	if _, err := reactions.ReplaceEmbeddedReactions(context.Background(), "message-a", "account-a", "conversation-a", nil, 200); err != nil {
		t.Fatal(err)
	}
	for _, table := range tables {
		if got := rowWrites(t, store, table); got != 0 {
			t.Errorf("%s: %d row writes for unchanged input, want 0", table, got)
		}
	}

	// A newer empty snapshot is a change: the fence advances.
	if _, err := reactions.ReplaceEmbeddedReactions(context.Background(), "message-a", "account-a", "conversation-a", nil, 301); err != nil {
		t.Fatal(err)
	}
	if got := rowWrites(t, store, "reaction_snapshot_fences"); got != 1 {
		t.Fatalf("newer empty snapshot fence writes = %d, want 1", got)
	}
	assertReactionSnapshotFence(t, store, "message-a", 301)
}

// A list naming one identity twice is rejected exactly as delete-and-reinsert
// rejects it (the repeat's insert hits the primary key), including when the
// stored roster has as many rows and every listed row matches one of them,
// the case the lock-free fast path must not accept.
func TestSyncConversationParticipantsRejectsRepeatedIdentity(t *testing.T) {
	for _, stored := range [][]string{{"id-1"}, {"id-1", "id-2"}} {
		t.Run(strings.Join(stored, "+"), func(t *testing.T) {
			guarded, reference := openGuardTestStore(t), openGuardTestStore(t)
			defer closeGuardTestStore(t, guarded)
			defer closeGuardTestStore(t, reference)
			var roster []ConversationParticipant
			for _, id := range stored {
				roster = append(roster, ConversationParticipant{IdentityID: id, Role: ParticipantRoleMember, IsActive: true})
			}
			for _, store := range []*Store{guarded, reference} {
				seedRosterGraph(t, store)
				mustRepositoryWrite(t, "seed roster", store.ReplaceConversationParticipants("conversation-a", roster))
			}
			repeated := []ConversationParticipant{roster[0], roster[0]}
			changed, guardedErr := guarded.SyncConversationParticipants("conversation-a", repeated, guardTestTimeMS+1)
			referenceErr := legacyReplaceConversationParticipants(reference, "conversation-a", repeated)
			if referenceErr == nil || !errors.Is(referenceErr, ErrInvalidConversationParticipant) || !errors.Is(referenceErr, ErrConstraintViolation) {
				t.Fatalf("reference error = %v, want an invalid-participant constraint violation", referenceErr)
			}
			if changed || !sameErrorClass(guardedErr, referenceErr) {
				t.Fatalf("SyncConversationParticipants(repeat) = (%v, %v), want (false, %v)", changed, guardedErr, referenceErr)
			}
			if got, want := dumpTable(t, guarded, "conversation_participants"), dumpTable(t, reference, "conversation_participants"); !reflect.DeepEqual(got, want) {
				t.Fatalf("rosters differ after the rejected repeat:\n%v\n%v", got, want)
			}
		})
	}
}

// An unchanged roster is confirmed without the write lock, so it does not
// wait behind another writer.
func TestSyncConversationParticipantsUnchangedRosterTakesNoWriteLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.sqlite3")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open(): %v", err)
	}
	defer closeGuardTestStore(t, store)
	seedRosterGraph(t, store)
	roster := []ConversationParticipant{{IdentityID: "id-1", Role: ParticipantRoleMember, IsActive: true}}
	if err := store.ReplaceConversationParticipants("conversation-a", roster); err != nil {
		t.Fatal(err)
	}

	locker, err := sql.Open("sqlite", storeDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	defer locker.Close()
	connection, err := locker.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if _, err := connection.ExecContext(context.Background(), `BEGIN IMMEDIATE`); err != nil {
		t.Fatalf("BEGIN IMMEDIATE: %v", err)
	}
	defer connection.ExecContext(context.Background(), `ROLLBACK`)

	started := time.Now()
	changed, err := store.SyncConversationParticipants("conversation-a", roster, guardTestTimeMS+1)
	if err != nil || changed {
		t.Fatalf("SyncConversationParticipants(unchanged) under a held write lock = (%v, %v), want (false, nil)", changed, err)
	}
	// A sync that took the lock would wait out the 5 s busy timeout.
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("unchanged roster sync waited %s behind the write lock", elapsed)
	}
}

func TestUnprocessedAfterVisitsEveryUnprocessedFrameOnceProperty(t *testing.T) {
	property := func(seed int64) bool {
		rng := rand.New(rand.NewSource(seed))
		clock := newMessageTestClock(guardTestTimeMS)
		store, repository := openMessageTestRepository(t, clock.Now)
		seedMessageAccount(t, store, "account-a", "signal")

		type frame struct {
			cursor    InboxCursor
			processed bool
		}
		frames := make(map[string]*frame)
		appendFrame := func() {
			inboxID := fmt.Sprintf("inbox-%04d-%d", rng.Intn(10000), len(frames))
			clock.Set(guardTestTimeMS + int64(rng.Intn(6)))
			record := messageTestInbox(inboxID, "account-a", "", []byte(inboxID))
			if _, err := repository.AppendInbox(context.Background(), record); err != nil {
				t.Fatalf("AppendInbox(): %v", err)
			}
			frames[inboxID] = &frame{cursor: InboxCursor{ReceivedAtMS: clock.Now().UnixMilli(), InboxID: inboxID}}
		}
		process := func(inboxID string) {
			clock.Set(guardTestTimeMS + 100)
			if err := repository.MarkInboxProcessed(context.Background(), inboxID, "account-a"); err != nil {
				t.Fatalf("MarkInboxProcessed(): %v", err)
			}
			frames[inboxID].processed = true
		}
		for index := rng.Intn(40); index >= 0; index-- {
			appendFrame()
		}
		for inboxID := range frames {
			if rng.Intn(3) == 0 {
				process(inboxID)
			}
		}

		// Walk with a random page size, processing and appending frames
		// mid-walk the way a drain does.
		limit := 1 + rng.Intn(7)
		var cursor InboxCursor
		visited := make(map[string]int)
		var order []InboxCursor
		unprocessedAtStart := make(map[string]bool)
		for inboxID, f := range frames {
			if !f.processed {
				unprocessedAtStart[inboxID] = true
			}
		}
		for {
			page, err := repository.UnprocessedAfter(context.Background(), cursor, limit)
			if err != nil {
				t.Fatalf("UnprocessedAfter(): %v", err)
			}
			// The page is a snapshot: every position must be unprocessed and
			// in order when it is listed.
			previous := cursor
			for _, position := range page {
				f := frames[position.InboxID]
				if f == nil || f.processed || f.cursor != position {
					t.Logf("seed %d: listed %+v, which is processed or unknown", seed, position)
					return false
				}
				if !lessInboxCursor(previous, position) {
					t.Logf("seed %d: position %+v does not follow %+v", seed, position, previous)
					return false
				}
				previous = position
			}
			for _, position := range page {
				cursor = position
				visited[position.InboxID]++
				order = append(order, position)
				if rng.Intn(2) == 0 {
					process(position.InboxID)
				}
				if rng.Intn(8) == 0 {
					appendFrame()
				}
				// Occasionally another worker processes a frame ahead.
				if rng.Intn(10) == 0 {
					for inboxID, other := range frames {
						if !other.processed && lessInboxCursor(cursor, other.cursor) {
							process(inboxID)
							delete(unprocessedAtStart, inboxID)
							break
						}
					}
				}
			}
			if len(page) < limit {
				break
			}
		}
		for inboxID := range unprocessedAtStart {
			if visited[inboxID] != 1 {
				t.Logf("seed %d: frame %s visited %d times, want once", seed, inboxID, visited[inboxID])
				return false
			}
		}
		for inboxID, count := range visited {
			if count != 1 {
				t.Logf("seed %d: frame %s visited %d times", seed, inboxID, count)
				return false
			}
		}
		if !sort.SliceIsSorted(order, func(i, j int) bool { return lessInboxCursor(order[i], order[j]) }) {
			t.Logf("seed %d: walk out of receipt order: %+v", seed, order)
			return false
		}
		return true
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 40, Rand: rand.New(rand.NewSource(20261014))}); err != nil {
		t.Fatal(err)
	}
}

func lessInboxCursor(left, right InboxCursor) bool {
	if left.ReceivedAtMS != right.ReceivedAtMS {
		return left.ReceivedAtMS < right.ReceivedAtMS
	}
	return left.InboxID < right.InboxID
}

func TestUnprocessedRecordSkipsProcessedAndMissingFrames(t *testing.T) {
	clock := newMessageTestClock(guardTestTimeMS)
	store, repository := openMessageTestRepository(t, clock.Now)
	seedMessageAccount(t, store, "account-a", "signal")
	record := messageTestInbox("inbox-a", "account-a", "dedupe-a", []byte("payload-a"))
	if _, err := repository.AppendInbox(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	got, ok, err := repository.UnprocessedRecord(context.Background(), "inbox-a")
	if err != nil || !ok {
		t.Fatalf("UnprocessedRecord(unprocessed) = (%v, %v)", ok, err)
	}
	if got.InboxID != "inbox-a" || string(got.Payload) != "payload-a" || got.ReceivedAtMS != guardTestTimeMS || got.ProcessedAtMS != nil {
		t.Fatalf("UnprocessedRecord() = %+v", got)
	}
	if err := repository.MarkInboxProcessed(context.Background(), "inbox-a", "account-a"); err != nil {
		t.Fatal(err)
	}
	for _, inboxID := range []string{"inbox-a", "inbox-missing"} {
		if _, ok, err := repository.UnprocessedRecord(context.Background(), inboxID); err != nil || ok {
			t.Fatalf("UnprocessedRecord(%s) = (%v, %v), want (false, nil)", inboxID, ok, err)
		}
	}
}

// The drain's two inbox reads stay on indexes: the position page is a range
// seek on the partial unprocessed index (SQLite visits each listed row only to
// check processed_at_ms; a partial index never covers its own WHERE column),
// and the frame load is a primary key seek.
func TestInboxDrainQueriesStayOnIndexes(t *testing.T) {
	store := openGuardTestStore(t)
	defer closeGuardTestStore(t, store)
	plan := func(query string, args ...any) string {
		rows, err := store.db.Query(`EXPLAIN QUERY PLAN `+query, args...)
		if err != nil {
			t.Fatalf("EXPLAIN %s: %v", query, err)
		}
		defer rows.Close()
		var details []string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			details = append(details, detail)
		}
		return strings.Join(details, " | ")
	}
	page := plan(`
		SELECT received_at_ms, inbox_id
		FROM inbox
		WHERE processed_at_ms IS NULL
		  AND (received_at_ms, inbox_id) > (?, ?)
		ORDER BY received_at_ms, inbox_id
		LIMIT ?`, 0, "", 10)
	if want := "SEARCH inbox USING INDEX inbox_unprocessed_idx ((received_at_ms,inbox_id)>(?,?))"; page != want {
		t.Fatalf("UnprocessedAfter plan = %q, want %q", page, want)
	}
	load := plan(`SELECT `+inboxColumns+` FROM inbox WHERE inbox_id = ? AND processed_at_ms IS NULL`, "x")
	if want := "SEARCH inbox USING INDEX sqlite_autoindex_inbox_1 (inbox_id=?)"; load != want {
		t.Fatalf("UnprocessedRecord plan = %q, want %q", load, want)
	}
}
