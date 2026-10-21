package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// IdentityUsage is what names one identity: the rows a rewrite moves.
type IdentityUsage struct {
	Messages      int64  `json:"messages"`
	Participants  int64  `json:"participants"`
	Reactions     int64  `json:"reactions"`
	PersonID      string `json:"person_id,omitempty"`
	PersonPrimary bool   `json:"person_primary,omitempty"`
	// ParticipantNames are the distinct display names its conversation rows
	// carry, and DirectTitles the distinct titles of the direct conversations
	// it belongs to.
	ParticipantNames []string `json:"participant_names,omitempty"`
	DirectTitles     []string `json:"direct_titles,omitempty"`
}

// IdentityUsage counts the rows that name identityID in accountID.
func (s *Store) IdentityUsage(accountID, identityID string) (IdentityUsage, error) {
	ctx := context.Background()
	var usage IdentityUsage
	for _, count := range []struct {
		target *int64
		query  string
		args   []any
	}{
		{&usage.Messages, `SELECT COUNT(*) FROM messages WHERE account_id = ? AND sender_identity_id = ?`, []any{accountID, identityID}},
		{&usage.Participants, `SELECT COUNT(*) FROM conversation_participants WHERE account_id = ? AND identity_id = ?`, []any{accountID, identityID}},
		{&usage.Reactions, `SELECT COUNT(*) FROM reactions WHERE account_id = ? AND (reactor_identity_id = ? OR reactor_key = ?)`, []any{accountID, identityID, identityID}},
	} {
		if err := s.db.QueryRowContext(ctx, count.query, count.args...).Scan(count.target); err != nil {
			return IdentityUsage{}, fmt.Errorf("count rows naming identity %q: %w", identityID, err)
		}
	}
	err := s.db.QueryRowContext(ctx, `
		SELECT person_id, is_primary FROM person_identities WHERE identity_id = ?
	`, identityID).Scan(&usage.PersonID, &usage.PersonPrimary)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return IdentityUsage{}, fmt.Errorf("read person link of identity %q: %w", identityID, err)
	}
	names, err := s.distinctStrings(ctx, `
		SELECT DISTINCT display_name FROM conversation_participants
		WHERE account_id = ? AND identity_id = ? AND display_name <> ''
		ORDER BY display_name
	`, accountID, identityID)
	if err != nil {
		return IdentityUsage{}, fmt.Errorf("read participant names of identity %q: %w", identityID, err)
	}
	usage.ParticipantNames = names
	titles, err := s.distinctStrings(ctx, `
		SELECT DISTINCT c.title FROM conversations c
		JOIN conversation_participants p ON p.conversation_id = c.conversation_id
		WHERE c.account_id = ? AND p.identity_id = ? AND c.kind = 'direct' AND c.title <> ''
		ORDER BY c.title
	`, accountID, identityID)
	if err != nil {
		return IdentityUsage{}, fmt.Errorf("read direct titles of identity %q: %w", identityID, err)
	}
	usage.DirectTitles = titles
	return usage, nil
}

func (s *Store) distinctStrings(ctx context.Context, query string, args ...any) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	return collectRows(rows, func(row rowScanner) (string, error) {
		var value string
		err := row.Scan(&value)
		return value, err
	})
}

// IdentityRewrite re-keys one identity: every row that names From comes to
// name the identity (account, ToKind, ToCanonicalValue), which is created
// with ToIdentityID when it does not exist yet, and From is deleted. RawNames
// are display names that only repeat the rewritten identifier (a LID's digits
// shown as a phone number, say): they are never carried, and are cleared from
// both identities, their participant rows and their direct conversations'
// titles.
type IdentityRewrite struct {
	FromIdentityID     string       `json:"from_identity_id"`
	FromKind           IdentityKind `json:"from_kind"`
	FromCanonicalValue string       `json:"from_canonical_value"`
	ToIdentityID       string       `json:"to_identity_id"`
	ToKind             IdentityKind `json:"to_kind"`
	ToCanonicalValue   string       `json:"to_canonical_value"`
	ToRawValue         string       `json:"to_raw_value"`
	RawNames           []string     `json:"raw_names,omitempty"`
}

// ApplyIdentityRewrites applies rewrites in order inside one transaction. A
// rewrite whose From identity is gone is a no-op, so applying a plan again
// changes nothing; a From identity that no longer has the kind and value the
// plan read, or a self identity on either side, fails the whole plan. What
// moves, and how a collision with the target resolves:
//   - messages: sender_identity_id is repointed.
//   - conversation_participants: a row is repointed; when the target is
//     already in that conversation the two rows merge into the target's
//     (active wins, then the target; a blank display name is filled; the
//     earlier join time is kept).
//   - reactions: a reaction keyed by From is re-keyed to the target; when the
//     target also reacted to that message the newer row stays
//     (mergeDuplicateRows' rule: (occurred_at_ms, source_seq_ms) for deltas,
//     (source_seq_ms, occurred_at_ms) under a snapshot fence; the target's on
//     an exact tie). A row keyed otherwise keeps its key.
//   - person_identities: the link moves to the target when the target has
//     none; when both link to one person the target's link stays (and
//     becomes primary if From's was); links to two people fail the plan,
//     because joining them is not the repair's to decide.
//
// The foreign keys make deleting From fail, and the plan roll back, if any
// row still names it.
func (s *Store) ApplyIdentityRewrites(ctx context.Context, accountID string, rewrites []IdentityRewrite, nowMS int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("apply identity rewrites: begin: %w", err)
	}
	defer tx.Rollback()
	for index, rewrite := range rewrites {
		if err := applyIdentityRewrite(ctx, tx, accountID, rewrite, nowMS); err != nil {
			return fmt.Errorf("apply identity rewrites: rewrite %d (%s %s -> %s %s): %w",
				index, rewrite.FromKind, rewrite.FromCanonicalValue, rewrite.ToKind, rewrite.ToCanonicalValue, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("apply identity rewrites: commit: %w", err)
	}
	return nil
}

func applyIdentityRewrite(ctx context.Context, tx *sql.Tx, accountID string, rewrite IdentityRewrite, nowMS int64) error {
	var from Identity
	err := tx.QueryRowContext(ctx, `
		SELECT kind, canonical_value, display_name, is_self, metadata_json, created_at_ms
		FROM identities WHERE account_id = ? AND identity_id = ?
	`, accountID, rewrite.FromIdentityID).Scan(
		&from.Kind, &from.CanonicalValue, &from.DisplayName, &from.IsSelf, &from.MetadataJSON, &from.CreatedAtMS,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read identity %q: %w", rewrite.FromIdentityID, err)
	}
	if from.Kind != rewrite.FromKind || from.CanonicalValue != rewrite.FromCanonicalValue {
		return fmt.Errorf("identity %q is now %s %q, not the %s %q the plan read",
			rewrite.FromIdentityID, from.Kind, from.CanonicalValue, rewrite.FromKind, rewrite.FromCanonicalValue)
	}
	if from.IsSelf {
		return fmt.Errorf("identity %q is this account's own", rewrite.FromIdentityID)
	}
	if rewrite.ToKind == from.Kind && rewrite.ToCanonicalValue == from.CanonicalValue {
		return fmt.Errorf("identity %q would be rewritten to itself", rewrite.FromIdentityID)
	}
	raw := make(map[string]struct{}, len(rewrite.RawNames))
	for _, name := range rewrite.RawNames {
		raw[name] = struct{}{}
	}
	carriedName := strings.TrimSpace(from.DisplayName)
	if isRawName(raw, from.DisplayName) {
		carriedName = ""
	}

	var toID, toName string
	var toSelf bool
	err = tx.QueryRowContext(ctx, `
		SELECT identity_id, display_name, is_self FROM identities
		WHERE account_id = ? AND kind = ? AND canonical_value = ?
	`, accountID, rewrite.ToKind, rewrite.ToCanonicalValue).Scan(&toID, &toName, &toSelf)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		toID = rewrite.ToIdentityID
		if strings.TrimSpace(toID) == "" {
			return errors.New("the plan names no identity id for the target")
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO identities (
				identity_id, account_id, kind, canonical_value, raw_value, display_name,
				is_self, metadata_json, created_at_ms, updated_at_ms
			) VALUES (?, ?, ?, ?, ?, ?, 0, ?, ?, ?)
		`, toID, accountID, rewrite.ToKind, rewrite.ToCanonicalValue, rewrite.ToRawValue, carriedName,
			from.MetadataJSON, from.CreatedAtMS, max(nowMS, from.CreatedAtMS)); err != nil {
			return fmt.Errorf("create target identity: %w", mapConstraintError(err))
		}
	case err != nil:
		return fmt.Errorf("read target identity: %w", err)
	case toSelf:
		return fmt.Errorf("target identity %q is this account's own", toID)
	case isRawName(raw, toName) || (strings.TrimSpace(toName) == "" && carriedName != ""):
		if _, err := tx.ExecContext(ctx, `
			UPDATE identities SET display_name = ?, updated_at_ms = MAX(updated_at_ms, ?)
			WHERE identity_id = ?
		`, carriedName, nowMS, toID); err != nil {
			return fmt.Errorf("name target identity: %w", err)
		}
	}
	if toID == rewrite.FromIdentityID {
		return fmt.Errorf("target identity %q is the identity being rewritten", toID)
	}

	if err := moveIdentityPersonLink(ctx, tx, rewrite.FromIdentityID, toID); err != nil {
		return err
	}

	arguments := []any{
		sql.Named("account_id", accountID),
		sql.Named("from_id", rewrite.FromIdentityID),
		sql.Named("to_id", toID),
		sql.Named("now_ms", nowMS),
	}
	rawList := "''"
	if len(rewrite.RawNames) > 0 {
		placeholders := make([]string, len(rewrite.RawNames))
		for i, name := range rewrite.RawNames {
			placeholders[i] = fmt.Sprintf(":raw_%d", i)
			arguments = append(arguments, sql.Named(fmt.Sprintf("raw_%d", i), name))
		}
		rawList = strings.Join(placeholders, ", ")
	}
	newerUnderFence := `(old.source_seq_ms, old.occurred_at_ms) > (reactions.source_seq_ms, reactions.occurred_at_ms)`
	newerDelta := `(old.occurred_at_ms, old.source_seq_ms) > (reactions.occurred_at_ms, reactions.source_seq_ms)`
	fenced := `EXISTS (SELECT 1 FROM reaction_snapshot_fences f WHERE f.message_id = reactions.message_id)`
	steps := []struct {
		name  string
		query string
	}{
		{
			name: "clear raw direct-conversation titles",
			query: `
				UPDATE conversations
				SET title = '', updated_at_ms = MAX(updated_at_ms, :now_ms)
				WHERE account_id = :account_id AND kind = 'direct' AND title IN (` + rawList + `)
				  AND conversation_id IN (
					SELECT conversation_id FROM conversation_participants
					WHERE account_id = :account_id AND identity_id IN (:from_id, :to_id)
				  )
			`,
		},
		{
			name: "clear raw participant names",
			query: `
				UPDATE conversation_participants SET display_name = ''
				WHERE account_id = :account_id AND identity_id IN (:from_id, :to_id)
				  AND display_name IN (` + rawList + `)
			`,
		},
		{
			name: "merge participant rows into the target's",
			query: `
				UPDATE conversation_participants AS target
				SET role = CASE WHEN target.is_active >= old.is_active THEN target.role ELSE old.role END,
				    left_at_ms = CASE WHEN target.is_active >= old.is_active THEN target.left_at_ms ELSE old.left_at_ms END,
				    is_active = MAX(target.is_active, old.is_active),
				    display_name = CASE WHEN target.display_name = '' THEN old.display_name ELSE target.display_name END,
				    joined_at_ms = CASE
				        WHEN target.joined_at_ms IS NULL THEN old.joined_at_ms
				        WHEN old.joined_at_ms IS NULL THEN target.joined_at_ms
				        ELSE MIN(target.joined_at_ms, old.joined_at_ms)
				    END
				FROM conversation_participants AS old
				WHERE old.account_id = :account_id AND old.identity_id = :from_id
				  AND target.conversation_id = old.conversation_id AND target.identity_id = :to_id
			`,
		},
		{
			name: "drop merged participant rows",
			query: `
				DELETE FROM conversation_participants
				WHERE account_id = :account_id AND identity_id = :from_id
				  AND EXISTS (
					SELECT 1 FROM conversation_participants AS target
					WHERE target.conversation_id = conversation_participants.conversation_id
					  AND target.identity_id = :to_id
				  )
			`,
		},
		{
			name: "repoint participant rows",
			query: `
				UPDATE conversation_participants SET identity_id = :to_id
				WHERE account_id = :account_id AND identity_id = :from_id
			`,
		},
		{
			name: "repoint message senders",
			query: `
				UPDATE messages
				SET sender_identity_id = :to_id, updated_at_ms = MAX(updated_at_ms, :now_ms)
				WHERE account_id = :account_id AND sender_identity_id = :from_id
			`,
		},
		{
			name: "drop target reactions the rewritten identity's supersede",
			query: `
				DELETE FROM reactions
				WHERE account_id = :account_id AND reactor_key = :to_id
				  AND EXISTS (
					SELECT 1 FROM reactions AS old
					WHERE old.message_id = reactions.message_id AND old.reactor_key = :from_id
					  AND CASE WHEN ` + fenced + ` THEN ` + newerUnderFence + ` ELSE ` + newerDelta + ` END
				  )
			`,
		},
		{
			name: "drop superseded reactions of the rewritten identity",
			query: `
				DELETE FROM reactions
				WHERE account_id = :account_id AND reactor_key = :from_id
				  AND EXISTS (
					SELECT 1 FROM reactions AS target
					WHERE target.message_id = reactions.message_id AND target.reactor_key = :to_id
				  )
			`,
		},
		{
			name: "re-key reactions",
			query: `
				UPDATE reactions
				SET reactor_key = :to_id, reactor_identity_id = :to_id,
				    updated_at_ms = MAX(updated_at_ms, :now_ms)
				WHERE account_id = :account_id AND reactor_key = :from_id
			`,
		},
		{
			// Live ingest keys a person's reaction by their identity id; a row
			// keyed otherwise keeps its key and only names the target.
			name: "repoint reactions keyed otherwise",
			query: `
				UPDATE reactions
				SET reactor_identity_id = :to_id, updated_at_ms = MAX(updated_at_ms, :now_ms)
				WHERE account_id = :account_id AND reactor_identity_id = :from_id
			`,
		},
		{
			name: "delete the rewritten identity",
			query: `
				DELETE FROM identities WHERE account_id = :account_id AND identity_id = :from_id
			`,
		},
	}
	for _, step := range steps {
		if _, err := tx.ExecContext(ctx, step.query, arguments...); err != nil {
			return fmt.Errorf("%s: %w", step.name, mapConstraintError(err))
		}
	}
	return nil
}

func isRawName(raw map[string]struct{}, name string) bool {
	_, ok := raw[name]
	return ok
}

// moveIdentityPersonLink carries From's person link to the target (see
// ApplyIdentityRewrites).
func moveIdentityPersonLink(ctx context.Context, tx *sql.Tx, fromID, toID string) error {
	var fromPerson string
	var fromPrimary bool
	err := tx.QueryRowContext(ctx, `
		SELECT person_id, is_primary FROM person_identities WHERE identity_id = ?
	`, fromID).Scan(&fromPerson, &fromPrimary)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read person link: %w", err)
	}
	var toPerson string
	var toPrimary bool
	err = tx.QueryRowContext(ctx, `
		SELECT person_id, is_primary FROM person_identities WHERE identity_id = ?
	`, toID).Scan(&toPerson, &toPrimary)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if _, err := tx.ExecContext(ctx, `
			UPDATE person_identities SET identity_id = ? WHERE identity_id = ?
		`, toID, fromID); err != nil {
			return fmt.Errorf("move person link: %w", err)
		}
		return nil
	case err != nil:
		return fmt.Errorf("read target person link: %w", err)
	case toPerson != fromPerson:
		return fmt.Errorf("identity %q links to person %q and its target %q to person %q", fromID, fromPerson, toID, toPerson)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM person_identities WHERE identity_id = ?`, fromID); err != nil {
		return fmt.Errorf("drop merged person link: %w", err)
	}
	if fromPrimary && !toPrimary {
		if _, err := tx.ExecContext(ctx, `
			UPDATE person_identities SET is_primary = 1 WHERE identity_id = ?
		`, toID); err != nil {
			return fmt.Errorf("keep the person's primary identity: %w", err)
		}
	}
	return nil
}
