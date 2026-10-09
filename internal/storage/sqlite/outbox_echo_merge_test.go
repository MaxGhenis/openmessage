package sqlite

// Tests for repointLocalMessage's echo-duplicate merge. When an echo frame
// projects a sent message as its own row before the outbox learns the real
// remote ID, Confirm, ReconcileConfirm and RepairStoreFailed delete that
// duplicate so the local row can take the remote ID. Before the merge, a
// read cursor, reaction intent or read-receipt intent pointing at the
// duplicate (NO ACTION foreign keys) made that DELETE fail and roll the whole
// confirmation back on every retry, and reactions, snapshot fences and
// attachments recorded on it (ON DELETE CASCADE) vanished silently.

import (
	"context"
	"database/sql"
	"errors"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"testing/quick"
	"time"

	"github.com/maxghenis/openmessage/internal/bridge"
)

const (
	echoMergeRealID    = "remote-echo-merge"
	echoMergeEchoID    = "message-echo-duplicate"
	echoMergeDeviceID  = "device-local"
	echoMergeBlobHash  = "cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd"
	echoMergeFenceSeq  = outboxTestTimeMS - 200
	echoMergeReadAtMS  = outboxTestTimeMS - 100
	echoMergeRepointMS = outboxTestTimeMS + 1_000
)

// echoMergePath drives one outbox intent up to the point where its local row
// must be repointed at realID, and returns the call that performs the repoint
// through one public entry point.
type echoMergePath struct {
	name    string
	prepare func(t *testing.T, repository *OutboxRepository, now time.Time, item NewOutboxItem, realID string) func() error
}

func echoMergePaths() []echoMergePath {
	ctx := context.Background()
	leaseCalled := func(t *testing.T, repository *OutboxRepository, now time.Time, item NewOutboxItem) string {
		t.Helper()
		token := mustLeaseToken(t, mustLeaseOne(t, repository, LeaseRequest{
			Owner: "worker", Now: now, Duration: time.Minute, Limit: 1,
		}))
		if err := repository.MarkTransportCalled(ctx, Attempt{OutboxID: item.OutboxID, LeaseToken: token}); err != nil {
			t.Fatalf("MarkTransportCalled(): %v", err)
		}
		return token
	}
	reconcile := func(repository *OutboxRepository, item NewOutboxItem, realID string, want ReconcileOutcome) func() error {
		return func() error {
			outcome, err := repository.ReconcileConfirm(ctx, ReconcileRequest{
				AccountID:          item.AccountID,
				TransportRequestID: item.TransportRequestID,
				ResultRemoteID:     realID,
			})
			if err != nil {
				return err
			}
			if outcome != want {
				return errors.New("ReconcileConfirm() outcome = " + string(outcome) + ", want " + string(want))
			}
			return nil
		}
	}
	return []echoMergePath{
		{
			name: "Confirm",
			prepare: func(t *testing.T, repository *OutboxRepository, now time.Time, item NewOutboxItem, realID string) func() error {
				token := leaseCalled(t, repository, now, item)
				return func() error {
					return repository.Confirm(ctx, Confirmation{
						OutboxID: item.OutboxID, LeaseToken: token, ResultRemoteID: realID,
					})
				}
			},
		},
		{
			name: "ReconcileConfirm uncertain",
			prepare: func(t *testing.T, repository *OutboxRepository, now time.Time, item NewOutboxItem, realID string) func() error {
				token := leaseCalled(t, repository, now, item)
				if err := repository.MarkUncertain(ctx, item.OutboxID, token, "timeout", "deadline", "outcome unknown"); err != nil {
					t.Fatalf("MarkUncertain(): %v", err)
				}
				return reconcile(repository, item, realID, ReconcileOutcomeReconciled)
			},
		},
		{
			// MessageService.RepairStoreFailed and the store_failed retry loop
			// both re-drive this ReconcileConfirm arm.
			name: "ReconcileConfirm store_failed",
			prepare: func(t *testing.T, repository *OutboxRepository, now time.Time, item NewOutboxItem, realID string) func() error {
				token := leaseCalled(t, repository, now, item)
				if err := repository.MarkStoreFailed(ctx, item.OutboxID, token, realID, "local write failed"); err != nil {
					t.Fatalf("MarkStoreFailed(): %v", err)
				}
				return reconcile(repository, item, realID, ReconcileOutcomeReconciled)
			},
		},
		{
			name: "ReconcileConfirm enrich",
			prepare: func(t *testing.T, repository *OutboxRepository, now time.Time, item NewOutboxItem, realID string) func() error {
				token := leaseCalled(t, repository, now, item)
				if err := repository.Confirm(ctx, Confirmation{
					OutboxID: item.OutboxID, LeaseToken: token, ResultRemoteID: item.TransportRequestID,
				}); err != nil {
					t.Fatalf("Confirm(provisional): %v", err)
				}
				return reconcile(repository, item, realID, ReconcileOutcomeEnriched)
			},
		},
	}
}

func TestOutboxRepointMergesEchoDuplicateIntoLocalRow(t *testing.T) {
	for _, path := range echoMergePaths() {
		for _, withReferences := range []bool{false, true} {
			name := path.name + "/cascade children only"
			if withReferences {
				name = path.name + "/with NO ACTION references"
			}
			t.Run(name, func(t *testing.T) {
				clock := newOutboxTestClock(outboxTestTimeMS)
				store, repository := openOutboxTestRepository(t, clock.Now)
				seedEchoMergeGraph(t, store)
				ctx := context.Background()

				item := outboxTestItem("echo-merge")
				mustEnqueueOutgoingOutbox(t, repository, item, "optimistic body")
				repoint := path.prepare(t, repository, clock.Now(), item, echoMergeRealID)

				seedEchoDuplicate(t, store, echoMergeEchoID, item, echoMergeRealID)
				seedEchoCascadeChildren(t, store, echoMergeEchoID)
				var reactionIntent, receiptIntent NewOutboxItem
				if withReferences {
					reactionIntent, receiptIntent = seedEchoNoActionReferences(t, store, repository, echoMergeEchoID)
				}

				clock.Set(echoMergeRepointMS)
				if err := repoint(); err != nil {
					t.Fatalf("repoint through %s: %v", path.name, err)
				}

				messages := mustMessageRepository(t, store, echoMergeRepointMS)
				survivor, err := messages.GetMessage(ctx, item.LocalMessageID)
				if err != nil {
					t.Fatalf("GetMessage(local survivor): %v", err)
				}
				if survivor.RemoteMessageID != echoMergeRealID {
					t.Fatalf("local survivor remote ID = %q, want %q", survivor.RemoteMessageID, echoMergeRealID)
				}
				if _, err := messages.GetMessage(ctx, echoMergeEchoID); !errors.Is(err, ErrNotFound) {
					t.Fatalf("GetMessage(echo duplicate) error = %v, want ErrNotFound", err)
				}

				// Cascade children moved onto the survivor instead of vanishing.
				assertEchoMergedReaction(t, store, item.LocalMessageID, "reactor-delta", "thumbs-up", "active")
				assertEchoMergedReaction(t, store, item.LocalMessageID, "reactor-snapshot", "heart", "active")
				assertReactionFence(t, store, item.LocalMessageID, echoMergeFenceSeq)
				attachment, err := mustEchoMergeAttachments(t, store).GetForDownload(ctx, item.LocalMessageID, 0)
				if err != nil {
					t.Fatalf("GetForDownload(survivor attachment): %v", err)
				}
				if attachment.State != "downloaded" || attachment.BlobHash == nil || *attachment.BlobHash != echoMergeBlobHash {
					t.Fatalf("survivor attachment = %+v, want the echo's downloaded blob", attachment)
				}

				if withReferences {
					cursor, err := store.GetReadCursor(echoMergeDeviceID, item.ConversationID)
					if err != nil {
						t.Fatalf("GetReadCursor(): %v", err)
					}
					if cursor.LastReadMessageID == nil || *cursor.LastReadMessageID != item.LocalMessageID ||
						cursor.LastReadAtMS != echoMergeReadAtMS {
						t.Fatalf("read cursor = %+v, want it on the survivor at %d", cursor, echoMergeReadAtMS)
					}
					reaction, err := repository.GetOutboxReaction(ctx, reactionIntent.OutboxID)
					if err != nil {
						t.Fatalf("GetOutboxReaction(): %v", err)
					}
					if reaction.TargetMessageID != item.LocalMessageID {
						t.Fatalf("reaction intent target = %q, want survivor %q", reaction.TargetMessageID, item.LocalMessageID)
					}
					receipt, err := repository.GetOutboxReadReceipt(ctx, receiptIntent.OutboxID)
					if err != nil {
						t.Fatalf("GetOutboxReadReceipt(): %v", err)
					}
					if receipt.LastReadMessageID != item.LocalMessageID {
						t.Fatalf("read-receipt intent target = %q, want survivor %q", receipt.LastReadMessageID, item.LocalMessageID)
					}
				}

				assertNoMessageReferences(t, store, echoMergeEchoID)
				assertForeignKeyCheckClean(t, store.db)

				confirmed, err := repository.FindByID(ctx, item.OutboxID)
				if err != nil {
					t.Fatalf("FindByID(): %v", err)
				}
				if confirmed.State != OutboxConfirmed || confirmed.ResultRemoteID == nil ||
					*confirmed.ResultRemoteID != echoMergeRealID {
					t.Fatalf("confirmed outbox row = %+v", confirmed)
				}
			})
		}
	}
}

func seedEchoMergeGraph(t *testing.T, store *Store) {
	t.Helper()
	seedMessageIdentity(t, store, "identity-a", "account-a")
	seedMessageConversation(t, store, "conversation-a", "account-a")
	seedOutboxTestDevice(t, store, echoMergeDeviceID, "account-a")
}

// seedEchoDuplicate inserts the row an echo frame projects for a sent message
// whose outbox intent has not yet recorded realID.
func seedEchoDuplicate(t *testing.T, store *Store, messageID string, item NewOutboxItem, realID string) {
	t.Helper()
	mustExec(t, store.db, `
		INSERT INTO messages (
			message_id, conversation_id, account_id, remote_message_id,
			sender_identity_id, direction, body, reply_to_remote_id, state,
			occurred_at_ms, created_at_ms, updated_at_ms
		) VALUES (?, ?, ?, ?, NULL, 'outgoing', 'optimistic body', NULL, 'active', ?, ?, ?)
	`,
		messageID,
		item.ConversationID,
		item.AccountID,
		realID,
		outboxTestTimeMS-500,
		outboxTestTimeMS-500,
		outboxTestTimeMS-500,
	)
}

// seedEchoCascadeChildren records an inbound delta reaction, an embedded
// reaction snapshot (with its fence) and a downloaded attachment on messageID.
func seedEchoCascadeChildren(t *testing.T, store *Store, messageID string) {
	t.Helper()
	ctx := context.Background()
	reactions, err := NewReactionRepository(store, func() time.Time { return time.UnixMilli(outboxTestTimeMS - 300) })
	if err != nil {
		t.Fatalf("NewReactionRepository(): %v", err)
	}
	if _, err := reactions.ReplaceEmbeddedReactions(ctx, messageID, "account-a", "conversation-a",
		[]ReactionSnapshotEntry{{ReactorKey: "reactor-snapshot", ReactorLabel: "peer", Emoji: "heart"}},
		echoMergeFenceSeq,
	); err != nil {
		t.Fatalf("ReplaceEmbeddedReactions(): %v", err)
	}
	if applied, err := reactions.ApplyReaction(ctx, ReactionApply{
		AccountID:      "account-a",
		ConversationID: "conversation-a",
		MessageID:      messageID,
		ReactorKey:     "reactor-delta",
		ReactorLabel:   "peer",
		Emoji:          "thumbs-up",
		Action:         bridge.ReactionAdd,
		OccurredAtMS:   outboxTestTimeMS - 250,
	}); err != nil || !applied {
		t.Fatalf("ApplyReaction() = (%v, %v), want applied", applied, err)
	}

	attachments := mustEchoMergeAttachments(t, store)
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin attachment seed: %v", err)
	}
	if err := attachments.RecordInboundAttachment(ctx, tx, MessageAttachment{
		MessageID: messageID, Ordinal: 0, RemoteID: "media-0", MIME: "image/png",
	}); err != nil {
		_ = tx.Rollback()
		t.Fatalf("RecordInboundAttachment(): %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit attachment seed: %v", err)
	}
	if err := attachments.MarkDownloaded(ctx, messageID, 0, echoMergeBlobHash, 42, "image/png"); err != nil {
		t.Fatalf("MarkDownloaded(): %v", err)
	}
}

// seedEchoNoActionReferences points the local device's read cursor, a queued
// reaction intent and a queued read-receipt intent at messageID. All three
// reference messages with NO ACTION, so they block a plain DELETE.
func seedEchoNoActionReferences(
	t *testing.T,
	store *Store,
	repository *OutboxRepository,
	messageID string,
) (NewOutboxItem, NewOutboxItem) {
	t.Helper()
	ctx := context.Background()
	reactionIntent := outboxTestReactionItem("echo-merge-reaction")
	if _, _, err := repository.EnqueueReaction(ctx, reactionIntent, OutboxReaction{
		TargetMessageID: messageID, Emoji: "thumbs-up", Action: "add",
	}); err != nil {
		t.Fatalf("EnqueueReaction(): %v", err)
	}
	receiptIntent := outboxTestReadItem("echo-merge-read")
	target := messageID
	if _, _, err := repository.EnqueueReadReceipt(ctx, receiptIntent, OutboxReadReceipt{
		DeviceID: echoMergeDeviceID, LastReadMessageID: messageID, ReadAtMS: echoMergeReadAtMS,
	}, ReadCursor{
		AccountID:         "account-a",
		DeviceID:          echoMergeDeviceID,
		ConversationID:    "conversation-a",
		LastReadMessageID: &target,
		LastReadAtMS:      echoMergeReadAtMS,
		UpdatedAtMS:       echoMergeReadAtMS,
	}); err != nil {
		t.Fatalf("EnqueueReadReceipt(): %v", err)
	}
	cursor, err := store.GetReadCursor(echoMergeDeviceID, "conversation-a")
	if err != nil || cursor.LastReadMessageID == nil || *cursor.LastReadMessageID != messageID {
		t.Fatalf("seeded read cursor = (%+v, %v), want it on %q", cursor, err, messageID)
	}
	return reactionIntent, receiptIntent
}

func mustEchoMergeAttachments(t *testing.T, store *Store) *MessageAttachmentRepository {
	t.Helper()
	attachments, err := NewMessageAttachmentRepository(store, func() time.Time {
		return time.UnixMilli(outboxTestTimeMS - 300)
	})
	if err != nil {
		t.Fatalf("NewMessageAttachmentRepository(): %v", err)
	}
	return attachments
}

func assertEchoMergedReaction(t *testing.T, store *Store, messageID, reactorKey, emoji, state string) {
	t.Helper()
	var gotEmoji, gotState string
	err := store.db.QueryRow(`
		SELECT emoji, state FROM reactions WHERE message_id = ? AND reactor_key = ?
	`, messageID, reactorKey).Scan(&gotEmoji, &gotState)
	if errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("reaction (%q, %q) is missing", messageID, reactorKey)
	}
	if err != nil {
		t.Fatalf("read reaction (%q, %q): %v", messageID, reactorKey, err)
	}
	if gotEmoji != emoji || gotState != state {
		t.Fatalf("reaction (%q, %q) = {%q %q}, want {%q %q}", messageID, reactorKey, gotEmoji, gotState, emoji, state)
	}
}

func assertReactionFence(t *testing.T, store *Store, messageID string, want int64) {
	t.Helper()
	var got int64
	err := store.db.QueryRow(`
		SELECT source_seq_ms FROM reaction_snapshot_fences WHERE message_id = ?
	`, messageID).Scan(&got)
	if errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("reaction snapshot fence for %q is missing", messageID)
	}
	if err != nil {
		t.Fatalf("read reaction snapshot fence for %q: %v", messageID, err)
	}
	if got != want {
		t.Fatalf("reaction snapshot fence for %q = %d, want %d", messageID, got, want)
	}
}

// assertNoMessageReferences fails if any table that references messages still
// names messageID.
func assertNoMessageReferences(t *testing.T, store *Store, messageID string) {
	t.Helper()
	for _, reference := range []struct{ table, column string }{
		{"read_cursors", "last_read_message_id"},
		{"outbox_reactions", "target_message_id"},
		{"outbox_read_receipts", "last_read_message_id"},
		{"reactions", "message_id"},
		{"reaction_snapshot_fences", "message_id"},
		{"message_attachments", "message_id"},
	} {
		var count int
		if err := store.db.QueryRow(
			"SELECT COUNT(*) FROM "+reference.table+" WHERE "+reference.column+" = ?",
			messageID,
		).Scan(&count); err != nil {
			t.Fatalf("count %s references: %v", reference.table, err)
		}
		if count != 0 {
			t.Errorf("%s.%s still references %q in %d rows", reference.table, reference.column, messageID, count)
		}
	}
}

func assertForeignKeyCheckClean(t *testing.T, db *sql.DB) {
	t.Helper()
	rows, err := db.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatalf("PRAGMA foreign_key_check: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var table, parent string
		var rowID sql.NullInt64
		var foreignKeyID int
		if err := rows.Scan(&table, &rowID, &parent, &foreignKeyID); err != nil {
			t.Fatalf("scan foreign_key_check: %v", err)
		}
		t.Errorf("foreign_key_check violation: table=%q rowid=%v parent=%q fk=%d", table, rowID, parent, foreignKeyID)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate foreign_key_check: %v", err)
	}
}

func TestOutboxRepointKeepsEchoRowWhenLocalRowIsGone(t *testing.T) {
	clock := newOutboxTestClock(outboxTestTimeMS)
	store, repository := openOutboxTestRepository(t, clock.Now)
	seedEchoMergeGraph(t, store)
	ctx := context.Background()

	item := outboxTestItem("echo-merge-orphan")
	mustEnqueueOutgoingOutbox(t, repository, item, "optimistic body")
	repoint := echoMergePaths()[0].prepare(t, repository, clock.Now(), item, echoMergeRealID)
	seedEchoDuplicate(t, store, echoMergeEchoID, item, echoMergeRealID)
	seedEchoCascadeChildren(t, store, echoMergeEchoID)
	// outbox.local_message_id has no foreign key, so the optimistic row can
	// disappear (for example, a repair delete) while the intent is in flight.
	mustExec(t, store.db, `DELETE FROM messages WHERE message_id = ?`, item.LocalMessageID)

	clock.Set(echoMergeRepointMS)
	if err := repoint(); err != nil {
		t.Fatalf("Confirm(): %v", err)
	}
	echo, err := mustMessageRepository(t, store, echoMergeRepointMS).GetMessage(ctx, echoMergeEchoID)
	if err != nil {
		t.Fatalf("GetMessage(echo): %v; the only copy of the sent message was deleted", err)
	}
	if echo.RemoteMessageID != echoMergeRealID {
		t.Fatalf("echo remote ID = %q, want %q", echo.RemoteMessageID, echoMergeRealID)
	}
	assertEchoMergedReaction(t, store, echoMergeEchoID, "reactor-delta", "thumbs-up", "active")
	assertReactionFence(t, store, echoMergeEchoID, echoMergeFenceSeq)
}

func TestOutboxRepointMergesInLocalRowsCurrentConversation(t *testing.T) {
	clock := newOutboxTestClock(outboxTestTimeMS)
	store, repository := openOutboxTestRepository(t, clock.Now)
	seedEchoMergeGraph(t, store)
	seedMessageConversation(t, store, "conversation-b", "account-a")
	ctx := context.Background()

	item := outboxTestItem("echo-merge-moved")
	mustEnqueueOutgoingOutbox(t, repository, item, "optimistic body")
	repoint := echoMergePaths()[0].prepare(t, repository, clock.Now(), item, echoMergeRealID)
	// A repair move took the optimistic row to conversation-b, where the echo
	// then landed; the outbox row still names conversation-a.
	mustExec(t, store.db, `UPDATE messages SET conversation_id = 'conversation-b' WHERE message_id = ?`, item.LocalMessageID)
	moved := item
	moved.ConversationID = "conversation-b"
	seedEchoDuplicate(t, store, echoMergeEchoID, moved, echoMergeRealID)
	target := echoMergeEchoID
	mustRepositoryWrite(t, "seed cursor", store.UpsertReadCursor(ReadCursor{
		AccountID:         "account-a",
		DeviceID:          echoMergeDeviceID,
		ConversationID:    "conversation-b",
		LastReadMessageID: &target,
		LastReadAtMS:      echoMergeReadAtMS,
		UpdatedAtMS:       echoMergeReadAtMS,
	}))

	clock.Set(echoMergeRepointMS)
	if err := repoint(); err != nil {
		t.Fatalf("Confirm(): %v", err)
	}
	survivor, err := mustMessageRepository(t, store, echoMergeRepointMS).GetMessage(ctx, item.LocalMessageID)
	if err != nil {
		t.Fatalf("GetMessage(local survivor): %v", err)
	}
	if survivor.ConversationID != "conversation-b" || survivor.RemoteMessageID != echoMergeRealID {
		t.Fatalf("local survivor = %+v, want conversation-b holding %q", survivor, echoMergeRealID)
	}
	cursor, err := store.GetReadCursor(echoMergeDeviceID, "conversation-b")
	if err != nil {
		t.Fatalf("GetReadCursor(): %v", err)
	}
	if cursor.LastReadMessageID == nil || *cursor.LastReadMessageID != item.LocalMessageID {
		t.Fatalf("read cursor = %+v, want it on the survivor", cursor)
	}
	assertNoMessageReferences(t, store, echoMergeEchoID)
	assertRowCount(t, store.db, "messages", 1)
}

// echoMergeScenario is a random set of rows hanging off the echo duplicate,
// the local survivor and an unrelated bystander message, plus the entry point
// that performs the repoint.
type echoMergeScenario struct {
	Path            int
	Reactions       map[string][2]*echoMergeReactionSeed // [survivor, duplicate] per reactor key
	Fences          [2]*int64                            // [survivor, duplicate] source_seq_ms
	Attachments     [3][2]int                            // per ordinal, [survivor, duplicate]: 0 absent, 1 pending, 2 downloaded
	Cursors         [3]int                               // per device: 0 none, 1 duplicate, 2 survivor, 3 bystander
	ReactionIntents []int                                // target per intent: 1 duplicate, 2 survivor, 3 bystander
	ReceiptIntents  []int
	FutureUpdatedAt bool // some seeded rows carry updated_at_ms after the repoint clock
}

type echoMergeReactionSeed struct {
	Emoji        string
	State        string
	OccurredAtMS int64
	SourceSeqMS  int64
	UpdatedAtMS  int64
}

func (echoMergeScenario) Generate(r *rand.Rand, _ int) reflect.Value {
	scenario := echoMergeScenario{
		Path:            r.Intn(len(echoMergePaths())),
		Reactions:       map[string][2]*echoMergeReactionSeed{},
		FutureUpdatedAt: r.Intn(4) == 0,
	}
	updatedAt := func() int64 {
		if scenario.FutureUpdatedAt && r.Intn(3) == 0 {
			return echoMergeRepointMS + 5
		}
		return outboxTestTimeMS - 900 + int64(r.Intn(3))
	}
	reaction := func() *echoMergeReactionSeed {
		seed := &echoMergeReactionSeed{
			Emoji:        []string{"a", "b", "c"}[r.Intn(3)],
			State:        "active",
			OccurredAtMS: outboxTestTimeMS - 800 + int64(r.Intn(3)),
			SourceSeqMS:  int64(r.Intn(3)),
			UpdatedAtMS:  updatedAt(),
		}
		if r.Intn(5) == 0 {
			seed.State = "removed"
			if r.Intn(2) == 0 {
				seed.Emoji = ""
			}
		}
		return seed
	}
	for _, key := range []string{"r0", "r1", "r2", "r3"} {
		var pair [2]*echoMergeReactionSeed
		if r.Intn(2) == 0 {
			pair[0] = reaction()
		}
		if r.Intn(5) < 3 {
			pair[1] = reaction()
		}
		if pair[0] != nil || pair[1] != nil {
			scenario.Reactions[key] = pair
		}
	}
	for side, percent := range [2]int{40, 60} {
		if r.Intn(100) < percent {
			seq := int64(r.Intn(4))
			scenario.Fences[side] = &seq
		}
	}
	for ordinal := range scenario.Attachments {
		scenario.Attachments[ordinal] = [2]int{
			[]int{0, 0, 1, 2}[r.Intn(4)],
			[]int{0, 1, 2}[r.Intn(3)],
		}
	}
	for device := range scenario.Cursors {
		scenario.Cursors[device] = []int{0, 1, 1, 2, 3}[r.Intn(5)]
	}
	for range r.Intn(4) {
		scenario.ReactionIntents = append(scenario.ReactionIntents, 1+r.Intn(3))
	}
	for range r.Intn(3) {
		scenario.ReceiptIntents = append(scenario.ReceiptIntents, 1+r.Intn(3))
	}
	return reflect.ValueOf(scenario)
}

// echoMergeSnapshot holds every row of the tables the merge touches, keyed so
// that a model can rewrite them.
type echoMergeSnapshot struct {
	Reactions   map[[2]string]echoMergeReactionRow // (message_id, reactor_key)
	Fences      map[string]echoMergeFenceRow       // message_id
	Attachments map[echoMergeAttachmentKey]echoMergeAttachmentRow
	Cursors     map[[2]string]echoMergeCursorRow // (device_id, conversation_id)
	Intents     map[string]string                // outbox_id -> target message (reactions and receipts)
}

type echoMergeReactionRow struct {
	MessageID, ReactorKey, AccountID, ConversationID, Label, Emoji, State string
	IsSelf                                                                bool
	OccurredAtMS, SourceSeqMS, CreatedAtMS, UpdatedAtMS                   int64
}

type echoMergeFenceRow struct {
	MessageID                string
	SourceSeqMS, UpdatedAtMS int64
}

type echoMergeAttachmentKey struct {
	MessageID string
	Ordinal   int64
}

type echoMergeAttachmentRow struct {
	echoMergeAttachmentKey
	RemoteID, Filename, MIME, State string
	BlobHash                        sql.NullString
	SizeBytes                       sql.NullInt64
	CreatedAtMS, UpdatedAtMS        int64
}

type echoMergeCursorRow struct {
	AccountID, DeviceID, ConversationID string
	LastReadMessageID                   sql.NullString
	LastReadAtMS, UpdatedAtMS           int64
}

// echoMergeModel is the reference semantics of mergeEchoDuplicate followed by
// the duplicate's DELETE, written independently over plain Go maps.
func echoMergeModel(before echoMergeSnapshot, duplicateID, survivorID string, nowMS int64) echoMergeSnapshot {
	after := echoMergeSnapshot{
		Reactions:   map[[2]string]echoMergeReactionRow{},
		Fences:      map[string]echoMergeFenceRow{},
		Attachments: map[echoMergeAttachmentKey]echoMergeAttachmentRow{},
		Cursors:     map[[2]string]echoMergeCursorRow{},
		Intents:     map[string]string{},
	}
	for key, row := range before.Reactions {
		switch row.MessageID {
		case survivorID:
			duplicate, conflict := before.Reactions[[2]string{duplicateID, row.ReactorKey}]
			if !conflict || !(duplicate.OccurredAtMS > row.OccurredAtMS ||
				(duplicate.OccurredAtMS == row.OccurredAtMS && duplicate.SourceSeqMS > row.SourceSeqMS)) {
				after.Reactions[key] = row
			}
		case duplicateID:
			survivor, conflict := before.Reactions[[2]string{survivorID, row.ReactorKey}]
			if !conflict || row.OccurredAtMS > survivor.OccurredAtMS ||
				(row.OccurredAtMS == survivor.OccurredAtMS && row.SourceSeqMS > survivor.SourceSeqMS) {
				row.MessageID = survivorID
				row.UpdatedAtMS = max(row.UpdatedAtMS, nowMS)
				after.Reactions[[2]string{survivorID, row.ReactorKey}] = row
			}
		default:
			after.Reactions[key] = row
		}
	}
	for messageID, fence := range before.Fences {
		if messageID != duplicateID {
			after.Fences[messageID] = fence
		}
	}
	if duplicate, ok := before.Fences[duplicateID]; ok {
		survivor, exists := before.Fences[survivorID]
		switch {
		case !exists:
			after.Fences[survivorID] = echoMergeFenceRow{survivorID, duplicate.SourceSeqMS, nowMS}
		case duplicate.SourceSeqMS > survivor.SourceSeqMS:
			after.Fences[survivorID] = echoMergeFenceRow{survivorID, duplicate.SourceSeqMS, max(survivor.UpdatedAtMS, nowMS)}
		}
	}
	for key, row := range before.Attachments {
		switch key.MessageID {
		case survivorID:
			_, covered := before.Attachments[echoMergeAttachmentKey{duplicateID, key.Ordinal}]
			if !covered || row.BlobHash.Valid {
				after.Attachments[key] = row
			}
		case duplicateID:
			survivor, exists := before.Attachments[echoMergeAttachmentKey{survivorID, key.Ordinal}]
			if !exists || !survivor.BlobHash.Valid {
				row.MessageID = survivorID
				row.UpdatedAtMS = max(row.UpdatedAtMS, nowMS)
				after.Attachments[row.echoMergeAttachmentKey] = row
			}
		default:
			after.Attachments[key] = row
		}
	}
	for key, cursor := range before.Cursors {
		if cursor.LastReadMessageID.Valid && cursor.LastReadMessageID.String == duplicateID {
			cursor.LastReadMessageID.String = survivorID
			cursor.UpdatedAtMS = max(cursor.UpdatedAtMS, nowMS)
		}
		after.Cursors[key] = cursor
	}
	for outboxID, target := range before.Intents {
		if target == duplicateID {
			target = survivorID
		}
		after.Intents[outboxID] = target
	}
	return after
}

// Property (merge model): for any rows on the echo duplicate, the survivor and
// a bystander, and through every entry point, the repoint commits; afterwards
// no row references the duplicate, foreign_key_check is clean, and every row
// of every touched table equals what echoMergeModel derives from the rows
// before the repoint. Read-cursor, intent and reaction counts are conserved
// except for reaction rows the model resolves a conflict away from.
func TestOutboxRepointEchoMergeMatchesModelProperty(t *testing.T) {
	coverage := map[string]int{}
	clock := newOutboxTestClock(outboxTestTimeMS)
	store, repository := openOutboxTestRepository(t, clock.Now)
	seedEchoMergeGraph(t, store)
	seedMessageConversation(t, store, "conversation-b", "account-a")
	ctx := context.Background()
	property := func(scenario echoMergeScenario) bool {
		// Reset to the seeded graph. Deleting outbox cascades to its intent
		// carriers, and messages to reactions, fences and attachments.
		for _, statement := range []string{
			`DELETE FROM outbox`,
			`DELETE FROM read_cursors`,
			`DELETE FROM messages`,
		} {
			if _, err := store.db.Exec(statement); err != nil {
				t.Errorf("reset (%s): %v", statement, err)
				return false
			}
		}
		clock.Set(outboxTestTimeMS)

		item := outboxTestItem("echo-merge-property")
		mustEnqueueOutgoingOutbox(t, repository, item, "optimistic body")
		path := echoMergePaths()[scenario.Path]
		repoint := path.prepare(t, repository, clock.Now(), item, echoMergeRealID)
		survivorID := item.LocalMessageID
		const bystanderID = "message-bystander"
		seedEchoDuplicate(t, store, echoMergeEchoID, item, echoMergeRealID)
		seedOutboxTestMessage(t, store, bystanderID, "account-a", "conversation-a")
		seedOutboxTestMessage(t, store, "message-other-conversation", "account-a", "conversation-b")
		seedEchoMergeScenario(t, store, repository, scenario, survivorID, bystanderID)
		recordEchoMergeCoverage(coverage, scenario)

		before := readEchoMergeSnapshot(t, store)
		clock.Set(echoMergeRepointMS)
		if err := repoint(); err != nil {
			t.Errorf("scenario %+v: repoint through %s: %v", scenario, path.name, err)
			return false
		}
		after := readEchoMergeSnapshot(t, store)
		want := echoMergeModel(before, echoMergeEchoID, survivorID, echoMergeRepointMS)
		if !reflect.DeepEqual(after, want) {
			t.Errorf("scenario %+v via %s: rows differ from the merge model\n got %+v\nwant %+v", scenario, path.name, after, want)
			return false
		}
		if len(after.Cursors) != len(before.Cursors) || len(after.Intents) != len(before.Intents) {
			t.Errorf("cursor/intent counts changed: %d->%d, %d->%d", len(before.Cursors), len(after.Cursors), len(before.Intents), len(after.Intents))
			return false
		}
		survivor, err := mustMessageRepository(t, store, echoMergeRepointMS).GetMessage(ctx, survivorID)
		if err != nil || survivor.RemoteMessageID != echoMergeRealID {
			t.Errorf("survivor = (%+v, %v), want it holding %q", survivor, err, echoMergeRealID)
			return false
		}
		assertRowCount(t, store.db, "messages", 3)
		assertNoMessageReferences(t, store, echoMergeEchoID)
		assertForeignKeyCheckClean(t, store.db)
		return !t.Failed()
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 150, Rand: rand.New(rand.NewSource(20261009))}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"reaction conflict: duplicate wins", "reaction conflict: survivor wins", "reaction conflict: exact tie",
		"fence on both", "fence on duplicate only", "attachment conflict: survivor downloaded",
		"attachment conflict: survivor pending", "cursor on duplicate", "reaction intent on duplicate",
		"receipt intent on duplicate", "updated_at after repoint clock",
		"path 0", "path 1", "path 2", "path 3",
	} {
		if coverage[name] == 0 {
			t.Errorf("generator never produced %q; coverage = %v", name, coverage)
		}
	}
}

func seedEchoMergeScenario(
	t *testing.T,
	store *Store,
	repository *OutboxRepository,
	scenario echoMergeScenario,
	survivorID, bystanderID string,
) {
	t.Helper()
	ctx := context.Background()
	sides := [2]string{survivorID, echoMergeEchoID}
	insertReaction := func(messageID, reactorKey string, seed echoMergeReactionSeed) {
		mustExec(t, store.db, `
			INSERT INTO reactions (
				message_id, reactor_key, account_id, conversation_id, reactor_identity_id,
				reactor_is_self, reactor_label, emoji, state, occurred_at_ms, source_seq_ms,
				created_at_ms, updated_at_ms
			) VALUES (?, ?, 'account-a', 'conversation-a', NULL, 0, ?, ?, ?, ?, ?, ?, ?)
		`, messageID, reactorKey, "label-"+messageID, seed.Emoji, seed.State, seed.OccurredAtMS,
			seed.SourceSeqMS, outboxTestTimeMS-950, seed.UpdatedAtMS)
	}
	for reactorKey, pair := range scenario.Reactions {
		for side, seed := range pair {
			if seed != nil {
				insertReaction(sides[side], reactorKey, *seed)
			}
		}
	}
	insertFence := func(messageID string, seq int64) {
		mustExec(t, store.db, `
			INSERT INTO reaction_snapshot_fences (message_id, source_seq_ms, updated_at_ms)
			VALUES (?, ?, ?)
		`, messageID, seq, outboxTestTimeMS-700)
	}
	for side, seq := range scenario.Fences {
		if seq != nil {
			insertFence(sides[side], *seq)
		}
	}
	insertAttachment := func(messageID string, ordinal, state int) {
		var blobHash any
		stateName := "pending"
		if state == 2 {
			stateName = "downloaded"
			blobHash = strings.Repeat(string("0123456789abcdef"[len(messageID)%16]), 63) + string("0123456789abcdef"[ordinal])
		}
		mustExec(t, store.db, `
			INSERT INTO message_attachments (
				message_id, ordinal, remote_id, remote_ref, filename, mime, size_bytes,
				state, blob_hash, last_error, created_at_ms, updated_at_ms
			) VALUES (?, ?, ?, x'', ?, 'image/png', 10, ?, ?, NULL, ?, ?)
		`, messageID, ordinal, "remote-"+messageID, "file-"+messageID, stateName, blobHash,
			outboxTestTimeMS-600, outboxTestTimeMS-600+int64(ordinal))
	}
	for ordinal, pair := range scenario.Attachments {
		for side, state := range pair {
			if state != 0 {
				insertAttachment(sides[side], ordinal, state)
			}
		}
	}
	// The bystander's rows must come through untouched.
	insertReaction(bystanderID, "r0", echoMergeReactionSeed{Emoji: "a", State: "active", OccurredAtMS: outboxTestTimeMS - 10, UpdatedAtMS: outboxTestTimeMS - 10})
	insertFence(bystanderID, 7)
	insertAttachment(bystanderID, 0, 2)

	targets := map[int]string{1: echoMergeEchoID, 2: survivorID, 3: bystanderID}
	for device, target := range scenario.Cursors {
		deviceID := "device-" + string(rune('0'+device))
		seedOutboxTestDevice(t, store, deviceID, "account-a")
		if target == 0 {
			continue
		}
		messageID := targets[target]
		updatedAtMS := outboxTestTimeMS - 400 + int64(device)
		if scenario.FutureUpdatedAt && device == 0 {
			updatedAtMS = echoMergeRepointMS + 5
		}
		mustRepositoryWrite(t, "seed cursor", store.UpsertReadCursor(ReadCursor{
			AccountID: "account-a", DeviceID: deviceID, ConversationID: "conversation-a",
			LastReadMessageID: &messageID, LastReadAtMS: outboxTestTimeMS - 400, UpdatedAtMS: updatedAtMS,
		}))
	}
	otherMessage := "message-other-conversation"
	mustRepositoryWrite(t, "seed other-conversation cursor", store.UpsertReadCursor(ReadCursor{
		AccountID: "account-a", DeviceID: "device-0", ConversationID: "conversation-b",
		LastReadMessageID: &otherMessage, LastReadAtMS: outboxTestTimeMS - 400, UpdatedAtMS: outboxTestTimeMS - 400,
	}))
	for index, target := range scenario.ReactionIntents {
		intent := outboxTestReactionItem("property-reaction-" + string(rune('a'+index)))
		if _, _, err := repository.EnqueueReaction(ctx, intent, OutboxReaction{
			TargetMessageID: targets[target], Emoji: "a", Action: "add",
		}); err != nil {
			t.Fatalf("EnqueueReaction(): %v", err)
		}
	}
	seedOutboxTestDevice(t, store, "device-receipts", "account-a")
	for index, target := range scenario.ReceiptIntents {
		intent := outboxTestReadItem("property-read-" + string(rune('a'+index)))
		messageID := targets[target]
		readAtMS := outboxTestTimeMS - 300 + int64(index)
		if _, _, err := repository.EnqueueReadReceipt(ctx, intent, OutboxReadReceipt{
			DeviceID: "device-receipts", LastReadMessageID: messageID, ReadAtMS: readAtMS,
		}, ReadCursor{
			AccountID: "account-a", DeviceID: "device-receipts", ConversationID: "conversation-a",
			LastReadMessageID: &messageID, LastReadAtMS: readAtMS, UpdatedAtMS: readAtMS,
		}); err != nil {
			t.Fatalf("EnqueueReadReceipt(): %v", err)
		}
	}
}

func recordEchoMergeCoverage(coverage map[string]int, scenario echoMergeScenario) {
	coverage["path "+string(rune('0'+scenario.Path))]++
	for _, pair := range scenario.Reactions {
		survivor, duplicate := pair[0], pair[1]
		if survivor == nil || duplicate == nil {
			continue
		}
		switch {
		case duplicate.OccurredAtMS > survivor.OccurredAtMS ||
			(duplicate.OccurredAtMS == survivor.OccurredAtMS && duplicate.SourceSeqMS > survivor.SourceSeqMS):
			coverage["reaction conflict: duplicate wins"]++
		case duplicate.OccurredAtMS == survivor.OccurredAtMS && duplicate.SourceSeqMS == survivor.SourceSeqMS:
			coverage["reaction conflict: exact tie"]++
		default:
			coverage["reaction conflict: survivor wins"]++
		}
	}
	switch {
	case scenario.Fences[0] != nil && scenario.Fences[1] != nil:
		coverage["fence on both"]++
	case scenario.Fences[1] != nil:
		coverage["fence on duplicate only"]++
	}
	for _, pair := range scenario.Attachments {
		if pair[1] == 0 {
			continue
		}
		switch pair[0] {
		case 2:
			coverage["attachment conflict: survivor downloaded"]++
		case 1:
			coverage["attachment conflict: survivor pending"]++
		}
	}
	for _, target := range scenario.Cursors {
		if target == 1 {
			coverage["cursor on duplicate"]++
		}
	}
	for _, target := range scenario.ReactionIntents {
		if target == 1 {
			coverage["reaction intent on duplicate"]++
		}
	}
	for _, target := range scenario.ReceiptIntents {
		if target == 1 {
			coverage["receipt intent on duplicate"]++
		}
	}
	if scenario.FutureUpdatedAt {
		coverage["updated_at after repoint clock"]++
	}
}

func readEchoMergeSnapshot(t *testing.T, store *Store) echoMergeSnapshot {
	t.Helper()
	snapshot := echoMergeSnapshot{
		Reactions:   map[[2]string]echoMergeReactionRow{},
		Fences:      map[string]echoMergeFenceRow{},
		Attachments: map[echoMergeAttachmentKey]echoMergeAttachmentRow{},
		Cursors:     map[[2]string]echoMergeCursorRow{},
		Intents:     map[string]string{},
	}
	scan := func(query string, each func(*sql.Rows) error) {
		rows, err := store.db.Query(query)
		if err != nil {
			t.Fatalf("snapshot %q: %v", query, err)
		}
		defer rows.Close()
		for rows.Next() {
			if err := each(rows); err != nil {
				t.Fatalf("scan snapshot %q: %v", query, err)
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("iterate snapshot %q: %v", query, err)
		}
	}
	scan(`SELECT message_id, reactor_key, account_id, conversation_id, reactor_label, emoji, state,
			reactor_is_self, occurred_at_ms, source_seq_ms, created_at_ms, updated_at_ms
		FROM reactions WHERE reactor_identity_id IS NULL`, func(rows *sql.Rows) error {
		var row echoMergeReactionRow
		err := rows.Scan(&row.MessageID, &row.ReactorKey, &row.AccountID, &row.ConversationID, &row.Label,
			&row.Emoji, &row.State, &row.IsSelf, &row.OccurredAtMS, &row.SourceSeqMS, &row.CreatedAtMS, &row.UpdatedAtMS)
		snapshot.Reactions[[2]string{row.MessageID, row.ReactorKey}] = row
		return err
	})
	assertRowCount(t, store.db, "reactions", len(snapshot.Reactions))
	scan(`SELECT message_id, source_seq_ms, updated_at_ms FROM reaction_snapshot_fences`, func(rows *sql.Rows) error {
		var row echoMergeFenceRow
		err := rows.Scan(&row.MessageID, &row.SourceSeqMS, &row.UpdatedAtMS)
		snapshot.Fences[row.MessageID] = row
		return err
	})
	scan(`SELECT message_id, ordinal, remote_id, filename, mime, state, blob_hash, size_bytes,
			created_at_ms, updated_at_ms
		FROM message_attachments`, func(rows *sql.Rows) error {
		var row echoMergeAttachmentRow
		err := rows.Scan(&row.MessageID, &row.Ordinal, &row.RemoteID, &row.Filename, &row.MIME, &row.State,
			&row.BlobHash, &row.SizeBytes, &row.CreatedAtMS, &row.UpdatedAtMS)
		snapshot.Attachments[row.echoMergeAttachmentKey] = row
		return err
	})
	scan(`SELECT account_id, device_id, conversation_id, last_read_message_id, last_read_at_ms, updated_at_ms
		FROM read_cursors`, func(rows *sql.Rows) error {
		var row echoMergeCursorRow
		err := rows.Scan(&row.AccountID, &row.DeviceID, &row.ConversationID, &row.LastReadMessageID,
			&row.LastReadAtMS, &row.UpdatedAtMS)
		snapshot.Cursors[[2]string{row.DeviceID, row.ConversationID}] = row
		return err
	})
	scan(`SELECT outbox_id, target_message_id FROM outbox_reactions
		UNION ALL
		SELECT outbox_id, last_read_message_id FROM outbox_read_receipts`, func(rows *sql.Rows) error {
		var outboxID, target string
		err := rows.Scan(&outboxID, &target)
		snapshot.Intents[outboxID] = target
		return err
	})
	return snapshot
}
