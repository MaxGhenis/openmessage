package sqlite

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Substring search over the v2 store has LIKE semantics: '%' || q || '%'
// matches a value containing q, case-insensitively for ASCII letters only.
// Migration 0012 (substring_search) adds an FTS5 trigram index for every
// searched column, and the searches put their LIKE on the index's column, so
// SQLite reads candidate rows from the trigram postings and then applies the
// LIKE to each one. The index only narrows; the LIKE decides. Every query
// therefore returns the rows the plain LIKE would, in the same order, and the
// statements without the index remain for the patterns it cannot narrow.

// likePatternUsesTrigrams reports whether FTS5 can answer pattern, a LIKE
// pattern without an ESCAPE clause, from a trigram index rather than by
// reading every row. It mirrors sqlite3Fts5ExprPattern: the pattern is cut at
// its wildcards ('%' and '_') and at its first NUL, and the index is used when
// some piece holds at least three characters, counted as the bytes that do
// not continue a UTF-8 sequence. A disagreement could only cost speed: FTS5
// falls back to visiting every row itself, and the LIKE still decides.
func likePatternUsesTrigrams(pattern string) bool {
	run := 0
	for i := 0; i < len(pattern); i++ {
		switch b := pattern[i]; {
		case b == 0:
			return false
		case b == '%' || b == '_':
			run = 0
		case b&0xC0 != 0x80:
			run++
			if run >= 3 {
				return true
			}
		}
	}
	return false
}

// searchIndexes are the FTS5 trigram tables migration 0012 maintains, each
// over the table named in its content= option.
var searchIndexes = []string{
	"messages_fts",
	"conversations_fts",
	"conversation_participants_fts",
	"identities_fts",
}

// ErrSearchIndexMismatch reports a trigram index whose postings differ from
// its table's rows. Searches would then miss rows the index lacks.
var ErrSearchIndexMismatch = errors.New("sqlite search index does not match its table")

// VerifySearchIndexes checks that each trigram index holds exactly the
// postings of its table's current rows, using FTS5's integrity-check with rank
// 1, which compares the index against the external content table. PRAGMA
// integrity_check does not make this comparison. The check reads every row of
// each indexed table and, being an FTS5 command, runs as a write statement.
// The triggers of migration 0012 keep the indexes equal to their tables, so a
// mismatch means a write bypassed them or the file is damaged; running
// INSERT INTO <index>(<index>) VALUES ('rebuild') with the app stopped
// rebuilds an index from its table.
func (s *Store) VerifySearchIndexes(ctx context.Context) error {
	for _, index := range searchIndexes {
		_, err := s.db.ExecContext(ctx, fmt.Sprintf(
			`INSERT INTO %[1]s (%[1]s, rank) VALUES ('integrity-check', 1)`, index,
		))
		if err == nil {
			continue
		}
		if isSQLiteErrorCode(err, sqliteCorruptVTabCode) {
			return fmt.Errorf("%w: %s: %w", ErrSearchIndexMismatch, index, err)
		}
		return fmt.Errorf("verify search index %s: %w", index, err)
	}
	return nil
}

// trigramCandidatePattern turns user text that an escaped LIKE matches
// literally into an unescaped pattern matching a superset of the same values:
// each literal '%' or '_' becomes the one-character wildcard '_'. SQLite passes
// a LIKE to a virtual table only without an ESCAPE clause, so this is the form
// that can reach the trigram index; the escaped LIKE then trims its candidates.
// A backslash is an ordinary character in both patterns.
func trigramCandidatePattern(text string) string {
	return strings.ReplaceAll(text, "%", "_")
}
