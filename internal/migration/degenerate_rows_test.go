package migration

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// A real legacy store carries rows the clean-slate schema cannot represent:
// NULL message ids, messages whose conversation was deleted, placeholder rows
// with no timestamp, platform-inconsistent rows, empty or malformed
// participants JSON, and unified contacts whose identifiers are not the
// expected array (all observed in a production store during a cutover
// rehearsal). A single such row previously aborted the entire migration; each
// class must instead drop with an exact count while everything representable
// migrates and every integrity gate still passes.
func TestTransformDropsDegenerateLegacyRowsWithCounts(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	fixture := buildMigrationFixture(t, root)

	writer, err := sql.Open("sqlite", fixture.sourcePath)
	mustNoError(t, err)
	writer.SetMaxOpenConns(1)
	exec := func(query string, args ...any) {
		t.Helper()
		_, execErr := writer.Exec(query, args...)
		mustNoError(t, execErr)
	}

	// Message with a NULL primary key (empty-body placeholder).
	exec(`INSERT INTO messages (message_id, conversation_id, body, timestamp_ms, source_platform)
	      VALUES (NULL, 'sms-conversation', '', 0, 'sms')`)
	// Message referencing a conversation that no longer exists.
	exec(`INSERT INTO messages (message_id, conversation_id, body, timestamp_ms, source_platform)
	      VALUES ('degenerate-orphan', 'deleted-conversation', 'orphan body', ?, 'sms')`, fixtureBaseMS+50_000)
	// Message whose platform disagrees with its conversation's platform.
	exec(`INSERT INTO messages (message_id, conversation_id, body, timestamp_ms, source_platform)
	      VALUES ('degenerate-mismatch', 'sms-conversation', 'mismatch body', ?, 'signal')`, fixtureBaseMS+51_000)
	// Message with a valid id but a non-positive timestamp.
	exec(`INSERT INTO messages (message_id, conversation_id, body, timestamp_ms, source_platform)
	      VALUES ('degenerate-zero-ts', 'sms-conversation', 'no time', 0, 'sms')`)
	// A malformed reactions payload degrades only reactions; its message moves.
	exec(`INSERT INTO messages (message_id, conversation_id, body, timestamp_ms, source_platform, reactions)
	      VALUES ('degenerate-bad-reactions', 'sms-conversation', 'valid message', ?, 'sms', '{oops')`, fixtureBaseMS+51_500)
	// Conversation with empty-string participants (must migrate, no roster,
	// silently) and one with malformed participants JSON (must migrate,
	// counted).
	exec(`INSERT INTO conversations (conversation_id, name, participants, last_message_ts, source_platform)
	      VALUES ('degenerate-empty-participants', 'Empty roster', '', ?, 'sms')`, fixtureBaseMS+52_000)
	exec(`INSERT INTO conversations (conversation_id, name, participants, last_message_ts, source_platform)
	      VALUES ('degenerate-bad-participants', 'Bad roster', '{"truncated', ?, 'sms')`, fixtureBaseMS+53_000)
	// Unified contact whose identifiers column is a bare number, not an array.
	exec(`INSERT INTO unified_contacts (unified_id, display_name, identifiers)
	      VALUES ('degenerate-unified', 'Bare Number Person', '36040')`)
	mustNoError(t, writer.Close())

	report, err := Transform(context.Background(), Options{
		SourcePath:      fixture.sourcePath,
		TempStorePath:   filepath.Join(root, "store.sqlite3.tmp"),
		TempBlobPath:    filepath.Join(root, "blobs.tmp"),
		TargetPath:      filepath.Join(root, "canonical"),
		TargetStorePath: filepath.Join(root, "canonical", "store.sqlite3"),
		Check:           true,
	})
	mustNoError(t, err)

	if !report.Validation.Passed || !report.Validation.CountsMatched {
		t.Fatalf("validation = %+v, want passed with matched counts", report.Validation)
	}
	dropped := report.Dropped
	if dropped.MalformedMessages != 1 || dropped.OrphanedMessages != 1 ||
		dropped.PlatformMismatchMessages != 1 || dropped.NonPositiveTimestampMessages != 1 ||
		dropped.MalformedParticipants != 1 || dropped.UnmappableUnifiedContacts != 1 ||
		dropped.MalformedReactions != 1 {
		t.Fatalf("dropped dimensions = %+v, want exactly one of each degenerate class", dropped)
	}

	// Both degenerate conversations still migrated (empty roster is fine).
	conversations := report.TableCounts["conversations"]
	if conversations.Legacy != conversations.V2 {
		t.Fatalf("conversations legacy=%d v2=%d, want the degenerate-roster rows migrated", conversations.Legacy, conversations.V2)
	}
	// No degenerate message reached the v2 store.
	messages := report.TableCounts["messages"]
	if messages.Legacy != messages.V2 {
		t.Fatalf("messages legacy=%d v2=%d, want dropped rows excluded from both sides of the reconciliation", messages.Legacy, messages.V2)
	}

	warned := strings.Join(report.Warnings, "\n")
	for _, fragment := range []string{
		"malformed legacy messages",
		"referencing a missing conversation",
		"platform disagreed",
		"non-positive timestamp",
		"unparseable participants",
		"unmappable identifiers",
		"malformed reactions JSON",
	} {
		if !strings.Contains(warned, fragment) {
			t.Fatalf("warnings missing %q:\n%s", fragment, warned)
		}
	}
}
