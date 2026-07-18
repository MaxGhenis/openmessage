package ingest

import (
	"context"
	"testing"

	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

func activeReactionRows(
	t *testing.T,
	repository *sqlite.ReactionRepository,
	messageID string,
) []sqlite.ReactionRow {
	t.Helper()
	byMessage, err := repository.ReactionsForMessages(
		context.Background(),
		[]string{messageID},
	)
	if err != nil {
		t.Fatalf("ReactionsForMessages(%q): %v", messageID, err)
	}
	return byMessage[messageID]
}

func assertSingleActiveReaction(
	t *testing.T,
	repository *sqlite.ReactionRepository,
	messageID string,
	emoji string,
	canonical string,
	isSelf bool,
) {
	t.Helper()
	rows := activeReactionRows(t, repository, messageID)
	if len(rows) != 1 {
		t.Fatalf("active reactions for %q = %+v, want one", messageID, rows)
	}
	row := rows[0]
	if row.Emoji != emoji || row.ReactorCanonical != canonical || row.ReactorIsSelf != isSelf {
		t.Fatalf(
			"active reaction for %q = %+v, want emoji=%q canonical=%q self=%v",
			messageID,
			row,
			emoji,
			canonical,
			isSelf,
		)
	}
}

func assertNoActiveReactions(
	t *testing.T,
	repository *sqlite.ReactionRepository,
	messageID string,
) {
	t.Helper()
	if rows := activeReactionRows(t, repository, messageID); len(rows) != 0 {
		t.Fatalf("active reactions for %q = %+v, want none", messageID, rows)
	}
}

func assertReactionByCanonical(
	t *testing.T,
	rows []sqlite.ReactionRow,
	want map[string]string,
) {
	t.Helper()
	got := make(map[string]string, len(rows))
	for _, row := range rows {
		if row.ReactorIsSelf {
			t.Fatalf("unexpected self-key row in canonical reaction set: %+v", row)
		}
		got[row.ReactorCanonical] = row.Emoji
	}
	if len(got) != len(want) {
		t.Fatalf("reaction canonical map = %+v, want %+v", got, want)
	}
	for canonical, emoji := range want {
		if got[canonical] != emoji {
			t.Fatalf("reaction canonical map = %+v, want %+v", got, want)
		}
	}
}
