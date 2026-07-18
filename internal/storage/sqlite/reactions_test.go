package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/maxghenis/openmessage/internal/bridge"
)

const reactionTestTimeMS int64 = 1_910_000_000_000

func TestNewReactionRepositoryRequiresStoreAndClock(t *testing.T) {
	if _, err := NewReactionRepository(nil, time.Now); err == nil {
		t.Fatal("NewReactionRepository(nil store) succeeded")
	}

	store, err := Open(filepath.Join(t.TempDir(), "store.sqlite3"))
	if err != nil {
		t.Fatalf("Open(): %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close(): %v", err)
		}
	})
	if _, err := NewReactionRepository(store, nil); err == nil {
		t.Fatal("NewReactionRepository(nil clock) succeeded")
	}
}

func TestApplyReactionAddSwitchRemoveIsIdempotentAndLastWriterWins(t *testing.T) {
	clock := newMessageTestClock(reactionTestTimeMS)
	store, repository := openReactionTestRepository(t, clock.Now)
	seedReactionGraph(t, store, "message-a")
	seedReactionIdentity(t, store, "identity-alice", "+15550000001")

	apply := ReactionApply{
		AccountID:         "account-a",
		ConversationID:    "conversation-a",
		MessageID:         "message-a",
		ReactorKey:        "identity-alice",
		ReactorIdentityID: pointer("identity-alice"),
		ReactorLabel:      "raw-alice",
		Emoji:             "👍",
		Action:            bridge.ReactionAdd,
		OccurredAtMS:      100,
		SourceSeqMS:       11,
	}
	applied, err := repository.ApplyReaction(context.Background(), apply)
	if err != nil {
		t.Fatalf("ApplyReaction(add): %v", err)
	}
	if !applied {
		t.Fatal("ApplyReaction(add) applied = false, want true")
	}
	row := readStoredReaction(t, store, apply.MessageID, apply.ReactorKey)
	if row.Emoji != "👍" || row.State != "active" || row.OccurredAtMS != 100 ||
		row.SourceSeqMS != 11 || row.CreatedAtMS != reactionTestTimeMS ||
		row.UpdatedAtMS != reactionTestTimeMS {
		t.Fatalf("stored add = %+v", row)
	}

	clock.Set(reactionTestTimeMS + 100)
	applied, err = repository.ApplyReaction(context.Background(), apply)
	if err != nil {
		t.Fatalf("ApplyReaction(duplicate add): %v", err)
	}
	if applied {
		t.Fatal("ApplyReaction(duplicate add) applied = true, want false")
	}
	row = readStoredReaction(t, store, apply.MessageID, apply.ReactorKey)
	if row.UpdatedAtMS != reactionTestTimeMS {
		t.Fatalf("duplicate add updated_at_ms = %d, want %d", row.UpdatedAtMS, reactionTestTimeMS)
	}

	clock.Set(reactionTestTimeMS + 200)
	switchReaction := apply
	switchReaction.Emoji = "❤️"
	switchReaction.Action = bridge.ReactionSwitch
	switchReaction.OccurredAtMS = 200
	switchReaction.SourceSeqMS = 12
	applied, err = repository.ApplyReaction(context.Background(), switchReaction)
	if err != nil {
		t.Fatalf("ApplyReaction(switch): %v", err)
	}
	if !applied {
		t.Fatal("ApplyReaction(switch) applied = false, want true")
	}
	row = readStoredReaction(t, store, apply.MessageID, apply.ReactorKey)
	if row.Emoji != "❤️" || row.State != "active" || row.OccurredAtMS != 200 ||
		row.UpdatedAtMS != reactionTestTimeMS+200 {
		t.Fatalf("stored switch = %+v", row)
	}

	clock.Set(reactionTestTimeMS + 300)
	remove := switchReaction
	remove.Action = bridge.ReactionRemove
	remove.Emoji = ""
	remove.OccurredAtMS = 300
	remove.SourceSeqMS = 13
	applied, err = repository.ApplyReaction(context.Background(), remove)
	if err != nil {
		t.Fatalf("ApplyReaction(remove): %v", err)
	}
	if !applied {
		t.Fatal("ApplyReaction(remove) applied = false, want true")
	}
	row = readStoredReaction(t, store, apply.MessageID, apply.ReactorKey)
	if row.State != "removed" || row.Emoji != "❤️" {
		t.Fatalf("stored removal state/emoji = (%q, %q), want removed with retained heart", row.State, row.Emoji)
	}
	if row.OccurredAtMS != 300 || row.UpdatedAtMS != reactionTestTimeMS+300 {
		t.Fatalf("stored removal = %+v", row)
	}

	clock.Set(reactionTestTimeMS + 400)
	staleAdd := apply
	staleAdd.Emoji = "😂"
	staleAdd.OccurredAtMS = 250
	staleAdd.SourceSeqMS = 99
	applied, err = repository.ApplyReaction(context.Background(), staleAdd)
	if err != nil {
		t.Fatalf("ApplyReaction(stale add): %v", err)
	}
	if applied {
		t.Fatal("ApplyReaction(stale add) applied = true, want false")
	}
	row = readStoredReaction(t, store, apply.MessageID, apply.ReactorKey)
	if row.State != "removed" || row.Emoji != "❤️" || row.OccurredAtMS != 300 {
		t.Fatalf("row after stale add = %+v, want retained tombstone", row)
	}
}

func TestApplyReactionRemoveBeforeAddCreatesEmptyTombstone(t *testing.T) {
	store, repository := openReactionTestRepository(
		t,
		func() time.Time { return time.UnixMilli(reactionTestTimeMS) },
	)
	seedReactionGraph(t, store, "message-a")

	remove := ReactionApply{
		AccountID:      "account-a",
		ConversationID: "conversation-a",
		MessageID:      "message-a",
		ReactorKey:     "raw:unknown",
		ReactorLabel:   "unknown",
		Action:         bridge.ReactionRemove,
		OccurredAtMS:   100,
	}
	applied, err := repository.ApplyReaction(context.Background(), remove)
	if err != nil {
		t.Fatalf("ApplyReaction(remove before add): %v", err)
	}
	if !applied {
		t.Fatal("ApplyReaction(remove before add) applied = false, want true")
	}
	row := readStoredReaction(t, store, remove.MessageID, remove.ReactorKey)
	if row.State != "removed" || row.Emoji != "" {
		t.Fatalf("remove-before-add row state/emoji = (%q, %q), want (removed, empty)", row.State, row.Emoji)
	}

	lateAdd := remove
	lateAdd.Action = bridge.ReactionAdd
	lateAdd.Emoji = "👍"
	lateAdd.OccurredAtMS = 50
	applied, err = repository.ApplyReaction(context.Background(), lateAdd)
	if err != nil {
		t.Fatalf("ApplyReaction(late earlier add): %v", err)
	}
	if applied {
		t.Fatal("ApplyReaction(late earlier add) applied = true, want false")
	}
	row = readStoredReaction(t, store, remove.MessageID, remove.ReactorKey)
	if row.State != "removed" || row.Emoji != "" || row.OccurredAtMS != 100 {
		t.Fatalf("row after late earlier add = %+v, want empty tombstone", row)
	}

	newAdd := lateAdd
	newAdd.OccurredAtMS = 101
	applied, err = repository.ApplyReaction(context.Background(), newAdd)
	if err != nil {
		t.Fatalf("ApplyReaction(newer add): %v", err)
	}
	if !applied {
		t.Fatal("ApplyReaction(newer add) applied = false, want true")
	}
	row = readStoredReaction(t, store, remove.MessageID, remove.ReactorKey)
	if row.State != "active" || row.Emoji != "👍" {
		t.Fatalf("row after newer add = %+v, want active thumbs-up", row)
	}
}

func TestApplyReactionRejectsUnsupportedActionAndMapsConstraints(t *testing.T) {
	store, repository := openReactionTestRepository(
		t,
		func() time.Time { return time.UnixMilli(reactionTestTimeMS) },
	)
	seedReactionGraph(t, store, "message-a")

	base := ReactionApply{
		AccountID:      "account-a",
		ConversationID: "conversation-a",
		MessageID:      "message-a",
		ReactorKey:     "self",
		ReactorIsSelf:  true,
		Emoji:          "👍",
		OccurredAtMS:   100,
	}
	invalid := base
	invalid.Action = bridge.ReactionAction("toggle")
	if _, err := repository.ApplyReaction(context.Background(), invalid); err == nil {
		t.Fatal("ApplyReaction(unsupported action) succeeded")
	}

	emptyEmoji := base
	emptyEmoji.Action = bridge.ReactionAdd
	emptyEmoji.Emoji = ""
	_, err := repository.ApplyReaction(context.Background(), emptyEmoji)
	if !errors.Is(err, ErrConstraintViolation) {
		t.Fatalf("ApplyReaction(empty active emoji) error = %v, want ErrConstraintViolation", err)
	}

	orphan := base
	orphan.Action = bridge.ReactionAdd
	orphan.MessageID = "message-missing"
	_, err = repository.ApplyReaction(context.Background(), orphan)
	if !errors.Is(err, ErrConstraintViolation) {
		t.Fatalf("ApplyReaction(orphan message) error = %v, want ErrConstraintViolation", err)
	}
	assertRowCount(t, store.db, "reactions", 0)
}

func TestReplaceEmbeddedReactionsReconcilesAbsenceAndRejectsStaleSnapshots(t *testing.T) {
	clock := newMessageTestClock(reactionTestTimeMS)
	store, repository := openReactionTestRepository(t, clock.Now)
	seedReactionGraph(t, store, "message-a")
	seedReactionIdentity(t, store, "identity-alice", "+15550000001")
	seedReactionIdentity(t, store, "identity-bob", "+15550000002")

	initial := []ReactionSnapshotEntry{
		{
			ReactorKey:        "identity-alice",
			ReactorIdentityID: pointer("identity-alice"),
			ReactorLabel:      "alice",
			Emoji:             "👍",
		},
		{
			ReactorKey:        "identity-bob",
			ReactorIdentityID: pointer("identity-bob"),
			ReactorLabel:      "bob",
			Emoji:             "👍",
		},
		{
			ReactorKey:    "self",
			ReactorIsSelf: true,
			ReactorLabel:  "me",
			Emoji:         "❤️",
		},
	}
	changes, err := repository.ReplaceEmbeddedReactions(
		context.Background(), "message-a", "account-a", "conversation-a", initial, 1_000,
	)
	if err != nil {
		t.Fatalf("ReplaceEmbeddedReactions(initial): %v", err)
	}
	if changes.Applied != 3 || changes.Removed != 0 {
		t.Fatalf("ReplaceEmbeddedReactions(initial) changes = %+v, want applied 3", changes)
	}
	assertReactionStates(t, store, "message-a", map[string]storedReaction{
		"identity-alice": {Emoji: "👍", State: "active", SourceSeqMS: 1_000},
		"identity-bob":   {Emoji: "👍", State: "active", SourceSeqMS: 1_000},
		"self":           {Emoji: "❤️", State: "active", SourceSeqMS: 1_000},
	})

	changes, err = repository.ReplaceEmbeddedReactions(
		context.Background(), "message-a", "account-a", "conversation-a", initial, 1_100,
	)
	if err != nil {
		t.Fatalf("ReplaceEmbeddedReactions(idempotent replay): %v", err)
	}
	if changes != (ReactionSnapshotResult{}) {
		t.Fatalf("ReplaceEmbeddedReactions(higher-seq identical replay) changes = %+v, want zero", changes)
	}

	clock.Set(reactionTestTimeMS + 100)
	current := initial[:1]
	changes, err = repository.ReplaceEmbeddedReactions(
		context.Background(), "message-a", "account-a", "conversation-a", current, 2_000,
	)
	if err != nil {
		t.Fatalf("ReplaceEmbeddedReactions(removal by absence): %v", err)
	}
	if changes.Applied != 0 || changes.Removed != 2 {
		t.Fatalf("ReplaceEmbeddedReactions(removal by absence) changes = %+v, want removed 2", changes)
	}
	assertReactionStates(t, store, "message-a", map[string]storedReaction{
		"identity-alice": {Emoji: "👍", State: "active", SourceSeqMS: 2_000},
		// R1: snapshot tombstones retain the stored emoji for audit.
		"identity-bob": {Emoji: "👍", State: "removed", SourceSeqMS: 2_000},
		"self":         {Emoji: "❤️", State: "removed", SourceSeqMS: 2_000},
	})

	stale := []ReactionSnapshotEntry{
		{
			ReactorKey:        "identity-bob",
			ReactorIdentityID: pointer("identity-bob"),
			Emoji:             "😂",
		},
	}
	changes, err = repository.ReplaceEmbeddedReactions(
		context.Background(), "message-a", "account-a", "conversation-a", stale, 1_500,
	)
	if err != nil {
		t.Fatalf("ReplaceEmbeddedReactions(stale): %v", err)
	}
	if changes != (ReactionSnapshotResult{}) {
		t.Fatalf("ReplaceEmbeddedReactions(stale) changes = %+v, want zero", changes)
	}
	assertReactionStates(t, store, "message-a", map[string]storedReaction{
		"identity-alice": {Emoji: "👍", State: "active", SourceSeqMS: 2_000},
		"identity-bob":   {Emoji: "👍", State: "removed", SourceSeqMS: 2_000},
		"self":           {Emoji: "❤️", State: "removed", SourceSeqMS: 2_000},
	})
}

func TestReplaceEmbeddedReactionsEmptyFirstSnapshotFencesOlderNonEmptySnapshot(t *testing.T) {
	store, repository := openReactionTestRepository(
		t, func() time.Time { return time.UnixMilli(reactionTestTimeMS) },
	)
	seedReactionGraph(t, store, "message-a")

	changes, err := repository.ReplaceEmbeddedReactions(
		context.Background(), "message-a", "account-a", "conversation-a", nil, 200,
	)
	if err != nil {
		t.Fatalf("ReplaceEmbeddedReactions(empty seq 200): %v", err)
	}
	if changes != (ReactionSnapshotResult{}) {
		t.Fatalf("ReplaceEmbeddedReactions(empty seq 200) changes = %+v, want zero", changes)
	}

	changes, err = repository.ReplaceEmbeddedReactions(
		context.Background(), "message-a", "account-a", "conversation-a",
		[]ReactionSnapshotEntry{{ReactorKey: "self", ReactorIsSelf: true, Emoji: "👍"}}, 100,
	)
	if err != nil {
		t.Fatalf("ReplaceEmbeddedReactions(non-empty seq 100): %v", err)
	}
	if changes != (ReactionSnapshotResult{}) {
		t.Fatalf("ReplaceEmbeddedReactions(non-empty seq 100) changes = %+v, want zero", changes)
	}
	assertRowCount(t, store.db, "reactions", 0)
	assertReactionSnapshotFence(t, store, "message-a", 200)
}

func TestReplaceEmbeddedReactionsNewerEmptySnapshotRejectsOlderReplay(t *testing.T) {
	store, repository := openReactionTestRepository(
		t, func() time.Time { return time.UnixMilli(reactionTestTimeMS) },
	)
	seedReactionGraph(t, store, "message-a")
	entries := []ReactionSnapshotEntry{{ReactorKey: "self", ReactorIsSelf: true, Emoji: "👍"}}
	if _, err := repository.ReplaceEmbeddedReactions(
		context.Background(), "message-a", "account-a", "conversation-a", entries, 100,
	); err != nil {
		t.Fatalf("ReplaceEmbeddedReactions(non-empty seq 100): %v", err)
	}
	if _, err := repository.ReplaceEmbeddedReactions(
		context.Background(), "message-a", "account-a", "conversation-a", nil, 200,
	); err != nil {
		t.Fatalf("ReplaceEmbeddedReactions(empty seq 200): %v", err)
	}
	row := readStoredReaction(t, store, "message-a", "self")
	if row.State != "removed" || row.SourceSeqMS != 200 {
		t.Fatalf("row after empty snapshot = %+v, want removed at seq 200", row)
	}
	assertReactionSnapshotFence(t, store, "message-a", 200)

	changes, err := repository.ReplaceEmbeddedReactions(
		context.Background(), "message-a", "account-a", "conversation-a", entries, 100,
	)
	if err != nil {
		t.Fatalf("ReplaceEmbeddedReactions(older replay): %v", err)
	}
	if changes != (ReactionSnapshotResult{}) {
		t.Fatalf("ReplaceEmbeddedReactions(older replay) changes = %+v, want zero", changes)
	}
	row = readStoredReaction(t, store, "message-a", "self")
	if row.State != "removed" || row.SourceSeqMS != 200 {
		t.Fatalf("row after older replay = %+v, want retained seq-200 tombstone", row)
	}
}

func TestReplaceEmbeddedReactionsRollsBackAtomically(t *testing.T) {
	store, repository := openReactionTestRepository(
		t,
		func() time.Time { return time.UnixMilli(reactionTestTimeMS) },
	)
	seedReactionGraph(t, store, "message-a")
	if _, err := repository.ReplaceEmbeddedReactions(
		context.Background(), "message-a", "account-a", "conversation-a", nil, 500,
	); err != nil {
		t.Fatalf("ReplaceEmbeddedReactions(initial fence): %v", err)
	}

	entries := []ReactionSnapshotEntry{
		{ReactorKey: "self", ReactorIsSelf: true, Emoji: "👍"},
		{ReactorKey: "invalid", Emoji: ""},
	}
	_, err := repository.ReplaceEmbeddedReactions(
		context.Background(), "message-a", "account-a", "conversation-a", entries, 1_000,
	)
	if !errors.Is(err, ErrConstraintViolation) {
		t.Fatalf("ReplaceEmbeddedReactions(invalid entry) error = %v, want ErrConstraintViolation", err)
	}
	assertRowCount(t, store.db, "reactions", 0)
	assertReactionSnapshotFence(t, store, "message-a", 500)
}

func TestSeedReactionUsesExplicitTransactionAndKeepsFirstRow(t *testing.T) {
	clock := newMessageTestClock(reactionTestTimeMS)
	store, repository := openReactionTestRepository(t, clock.Now)
	seedReactionGraph(t, store, "message-a")

	seed := ReactionApply{
		AccountID:      "account-a",
		ConversationID: "conversation-a",
		MessageID:      "message-a",
		ReactorKey:     "self",
		ReactorIsSelf:  true,
		ReactorLabel:   "me",
		Emoji:          "👍",
		// SeedReaction deliberately ignores Action and SourceSeqMS: migration
		// rows are always active with source_seq_ms=0.
		Action:       bridge.ReactionRemove,
		OccurredAtMS: 123,
		SourceSeqMS:  999,
	}
	if err := repository.SeedReaction(context.Background(), nil, seed); err == nil {
		t.Fatal("SeedReaction(nil transaction) succeeded")
	}

	tx, err := store.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("BeginTx(): %v", err)
	}
	if err := repository.SeedReaction(context.Background(), tx, seed); err != nil {
		_ = tx.Rollback()
		t.Fatalf("SeedReaction(first): %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit(): %v", err)
	}

	clock.Set(reactionTestTimeMS + 100)
	duplicate := seed
	duplicate.Emoji = "❤️"
	duplicate.OccurredAtMS = 456
	tx, err = store.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("BeginTx(duplicate): %v", err)
	}
	if err := repository.SeedReaction(context.Background(), tx, duplicate); err != nil {
		_ = tx.Rollback()
		t.Fatalf("SeedReaction(duplicate): %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit(duplicate): %v", err)
	}

	row := readStoredReaction(t, store, seed.MessageID, seed.ReactorKey)
	if row.State != "active" || row.Emoji != "👍" || row.OccurredAtMS != 123 ||
		row.SourceSeqMS != 0 || row.CreatedAtMS != reactionTestTimeMS ||
		row.UpdatedAtMS != reactionTestTimeMS {
		t.Fatalf("seeded row = %+v", row)
	}
}

func TestReactionsForMessagesReturnsStableActiveRowsForLegacyAggregation(t *testing.T) {
	store, repository := openReactionTestRepository(
		t,
		func() time.Time { return time.UnixMilli(reactionTestTimeMS) },
	)
	seedReactionGraph(t, store, "message-a")
	seedReactionMessage(t, store, "message-b")
	seedReactionIdentity(t, store, "identity-alice", "+15550000001")
	seedReactionIdentity(t, store, "identity-bob", "+15550000002")

	for _, reaction := range []ReactionApply{
		{
			AccountID: "account-a", ConversationID: "conversation-a", MessageID: "message-a",
			ReactorKey: "identity-bob", ReactorIdentityID: pointer("identity-bob"),
			ReactorLabel: "bob raw", Emoji: "👍", Action: bridge.ReactionAdd, OccurredAtMS: 100,
		},
		{
			AccountID: "account-a", ConversationID: "conversation-a", MessageID: "message-a",
			ReactorKey: "identity-alice", ReactorIdentityID: pointer("identity-alice"),
			ReactorLabel: "alice raw", Emoji: "👍", Action: bridge.ReactionAdd, OccurredAtMS: 100,
		},
		{
			AccountID: "account-a", ConversationID: "conversation-a", MessageID: "message-b",
			ReactorKey: "self", ReactorIsSelf: true, ReactorLabel: "me",
			Emoji: "❤️", Action: bridge.ReactionAdd, OccurredAtMS: 200,
		},
		{
			AccountID: "account-a", ConversationID: "conversation-a", MessageID: "message-b",
			ReactorKey: "raw:mystery", ReactorLabel: "Mystery",
			Emoji: "❤️", Action: bridge.ReactionAdd, OccurredAtMS: 201,
		},
		{
			AccountID: "account-a", ConversationID: "conversation-a", MessageID: "message-b",
			ReactorKey: "raw:removed", ReactorLabel: "Removed",
			Action: bridge.ReactionRemove, OccurredAtMS: 202,
		},
	} {
		if _, err := repository.ApplyReaction(context.Background(), reaction); err != nil {
			t.Fatalf("ApplyReaction(%s/%s): %v", reaction.MessageID, reaction.ReactorKey, err)
		}
	}

	messageIDs := make([]string, 0, reactionMessageQueryBatchSize+4)
	messageIDs = append(messageIDs, "message-b")
	for index := 0; index < reactionMessageQueryBatchSize+1; index++ {
		messageIDs = append(messageIDs, fmt.Sprintf("missing-%04d", index))
	}
	messageIDs = append(messageIDs, "message-a", "message-b")

	got, err := repository.ReactionsForMessages(context.Background(), messageIDs)
	if err != nil {
		t.Fatalf("ReactionsForMessages(): %v", err)
	}
	want := map[string][]ReactionRow{
		"message-a": {
			{
				MessageID: "message-a", Emoji: "👍", ReactorCanonical: "+15550000001",
				ReactorLabel: "alice raw", OccurredAtMS: 100,
			},
			{
				MessageID: "message-a", Emoji: "👍", ReactorCanonical: "+15550000002",
				ReactorLabel: "bob raw", OccurredAtMS: 100,
			},
		},
		"message-b": {
			{
				MessageID: "message-b", Emoji: "❤️", ReactorIsSelf: true,
				ReactorLabel: "me", OccurredAtMS: 200,
			},
			{
				MessageID: "message-b", Emoji: "❤️", ReactorLabel: "Mystery",
				OccurredAtMS: 201,
			},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ReactionsForMessages() = %#v, want %#v", got, want)
	}

	again, err := repository.ReactionsForMessages(context.Background(), messageIDs)
	if err != nil {
		t.Fatalf("ReactionsForMessages(second call): %v", err)
	}
	if !reflect.DeepEqual(again, got) {
		t.Fatalf("second ReactionsForMessages() = %#v, first %#v", again, got)
	}

	assertLegacyReactionJSON(t, got["message-a"], `[{"emoji":"👍","count":2,"actors":["+15550000001","+15550000002"]}]`)
	assertLegacyReactionJSON(t, got["message-b"], `[{"emoji":"❤️","count":2,"actors":["me","Mystery"]}]`)

	empty, err := repository.ReactionsForMessages(context.Background(), nil)
	if err != nil {
		t.Fatalf("ReactionsForMessages(nil): %v", err)
	}
	if empty == nil || len(empty) != 0 {
		t.Fatalf("ReactionsForMessages(nil) = %#v, want non-nil empty map", empty)
	}
}

func TestReactionRowsCascadeWithMessage(t *testing.T) {
	store, repository := openReactionTestRepository(
		t,
		func() time.Time { return time.UnixMilli(reactionTestTimeMS) },
	)
	seedReactionGraph(t, store, "message-a")
	if _, err := repository.ApplyReaction(context.Background(), ReactionApply{
		AccountID: "account-a", ConversationID: "conversation-a", MessageID: "message-a",
		ReactorKey: "self", ReactorIsSelf: true, Emoji: "👍",
		Action: bridge.ReactionAdd, OccurredAtMS: 100,
	}); err != nil {
		t.Fatalf("ApplyReaction(): %v", err)
	}
	if _, err := repository.ReplaceEmbeddedReactions(
		context.Background(), "message-a", "account-a", "conversation-a", nil, 200,
	); err != nil {
		t.Fatalf("ReplaceEmbeddedReactions(): %v", err)
	}
	assertReactionSnapshotFence(t, store, "message-a", 200)
	if _, err := store.db.Exec(`DELETE FROM messages WHERE message_id = ?`, "message-a"); err != nil {
		t.Fatalf("delete message: %v", err)
	}
	assertRowCount(t, store.db, "reactions", 0)
	assertRowCount(t, store.db, "reaction_snapshot_fences", 0)
}

type storedReaction struct {
	MessageID         string
	ReactorKey        string
	ReactorIdentityID *string
	ReactorIsSelf     bool
	ReactorLabel      string
	Emoji             string
	State             string
	OccurredAtMS      int64
	SourceSeqMS       int64
	CreatedAtMS       int64
	UpdatedAtMS       int64
}

func assertReactionSnapshotFence(t *testing.T, store *Store, messageID string, want int64) {
	t.Helper()
	var got int64
	if err := store.db.QueryRow(`
		SELECT source_seq_ms FROM reaction_snapshot_fences WHERE message_id = ?
	`, messageID).Scan(&got); err != nil {
		t.Fatalf("read reaction snapshot fence for %q: %v", messageID, err)
	}
	if got != want {
		t.Fatalf("reaction snapshot fence for %q = %d, want %d", messageID, got, want)
	}
}

func openReactionTestRepository(
	t *testing.T,
	now func() time.Time,
) (*Store, *ReactionRepository) {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "store.sqlite3"))
	if err != nil {
		t.Fatalf("Open(): %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close(): %v", err)
		}
	})
	repository, err := NewReactionRepository(store, now)
	if err != nil {
		t.Fatalf("NewReactionRepository(): %v", err)
	}
	return store, repository
}

func seedReactionGraph(t *testing.T, store *Store, messageID string) {
	t.Helper()
	seedMessageAccount(t, store, "account-a", "signal")
	seedMessageConversation(t, store, "conversation-a", "account-a")
	seedReactionMessage(t, store, messageID)
}

func seedReactionMessage(t *testing.T, store *Store, messageID string) {
	t.Helper()
	repository := mustMessageRepository(t, store, reactionTestTimeMS)
	message := messageTestMessage(
		messageID,
		"conversation-a",
		"account-a",
		"remote-"+messageID,
		nil,
	)
	if err := repository.ImportMessage(context.Background(), MessageProjection{Message: message}); err != nil {
		t.Fatalf("seed ImportMessage(%q): %v", messageID, err)
	}
}

func seedReactionIdentity(t *testing.T, store *Store, identityID, canonical string) {
	t.Helper()
	identity := repositoryTestIdentity(identityID, "account-a", canonical)
	identity.Kind = IdentityKind("e164")
	identity.RawValue = canonical
	mustRepositoryWrite(t, "seed reaction identity", store.UpsertIdentity(identity))
}

func readStoredReaction(
	t *testing.T,
	store *Store,
	messageID string,
	reactorKey string,
) storedReaction {
	t.Helper()
	var (
		row        storedReaction
		identityID sql.NullString
	)
	err := store.db.QueryRow(`
		SELECT
			message_id,
			reactor_key,
			reactor_identity_id,
			reactor_is_self,
			reactor_label,
			emoji,
			state,
			occurred_at_ms,
			source_seq_ms,
			created_at_ms,
			updated_at_ms
		FROM reactions
		WHERE message_id = ? AND reactor_key = ?
	`, messageID, reactorKey).Scan(
		&row.MessageID,
		&row.ReactorKey,
		&identityID,
		&row.ReactorIsSelf,
		&row.ReactorLabel,
		&row.Emoji,
		&row.State,
		&row.OccurredAtMS,
		&row.SourceSeqMS,
		&row.CreatedAtMS,
		&row.UpdatedAtMS,
	)
	if err != nil {
		t.Fatalf("read reaction (%q, %q): %v", messageID, reactorKey, err)
	}
	if identityID.Valid {
		row.ReactorIdentityID = pointer(identityID.String)
	}
	return row
}

func assertReactionStates(
	t *testing.T,
	store *Store,
	messageID string,
	want map[string]storedReaction,
) {
	t.Helper()
	for reactorKey, expected := range want {
		got := readStoredReaction(t, store, messageID, reactorKey)
		if got.Emoji != expected.Emoji || got.State != expected.State ||
			got.SourceSeqMS != expected.SourceSeqMS {
			t.Fatalf(
				"reaction %q = {emoji:%q state:%q source_seq:%d}, want {emoji:%q state:%q source_seq:%d}",
				reactorKey,
				got.Emoji,
				got.State,
				got.SourceSeqMS,
				expected.Emoji,
				expected.State,
				expected.SourceSeqMS,
			)
		}
	}
	assertRowCount(t, store.db, "reactions", len(want))
}

type legacyReactionShape struct {
	Emoji  string   `json:"emoji"`
	Count  int      `json:"count"`
	Actors []string `json:"actors,omitempty"`
}

func assertLegacyReactionJSON(t *testing.T, rows []ReactionRow, want string) {
	t.Helper()
	grouped := make([]legacyReactionShape, 0)
	byEmoji := make(map[string]int)
	for _, row := range rows {
		index, exists := byEmoji[row.Emoji]
		if !exists {
			index = len(grouped)
			byEmoji[row.Emoji] = index
			grouped = append(grouped, legacyReactionShape{Emoji: row.Emoji})
		}
		actor := row.ReactorLabel
		if row.ReactorCanonical != "" {
			actor = row.ReactorCanonical
		}
		if row.ReactorIsSelf {
			actor = "me"
		}
		grouped[index].Count++
		grouped[index].Actors = append(grouped[index].Actors, actor)
	}
	encoded, err := json.Marshal(grouped)
	if err != nil {
		t.Fatalf("json.Marshal(legacy reactions): %v", err)
	}
	if string(encoded) != want {
		t.Fatalf("legacy reaction JSON = %s, want %s", encoded, want)
	}
}
