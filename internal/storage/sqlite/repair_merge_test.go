package sqlite

// Tests for ApplyRepairPlan's delete and move steps. A repair delete removes
// a content duplicate that a Google id-space reset projected, and a repair
// move re-files a misrouted message. Before the merge, a reaction or
// read-receipt intent on a planned-delete duplicate (NO ACTION foreign keys)
// made the whole plan fail with FOREIGN KEY constraint failed on every re-run,
// and reactions, snapshot fences and attachments recorded on it (ON DELETE
// CASCADE) were silently deleted instead of moving to the surviving copy.

import (
	"context"
	"strings"
	"testing"
)

const (
	repairMergeDuplicateID = "message-repair-duplicate"
	repairMergeSurvivorID  = "message-repair-survivor"
	repairMergeBystanderID = "message-repair-bystander"
	repairMergeNowMS       = outboxTestTimeMS + 5_000
)

func openRepairMergeStore(t *testing.T) (*Store, *OutboxRepository) {
	t.Helper()
	store, repository := openOutboxTestRepository(t, newOutboxTestClock(outboxTestTimeMS).Now)
	seedEchoMergeGraph(t, store)
	seedMessageConversation(t, store, "conversation-b", "account-a")
	return store, repository
}

// seedRepairMergeIntents queues a reaction intent and a read-receipt intent
// on messageID, both in conversation-a. Enqueueing the receipt also moves the
// local device's read cursor onto messageID.
func seedRepairMergeIntents(
	t *testing.T,
	store *Store,
	repository *OutboxRepository,
	messageID string,
) (NewOutboxItem, NewOutboxItem) {
	t.Helper()
	return seedEchoNoActionReferences(t, store, repository, messageID)
}

func applyRepairPlan(t *testing.T, store *Store, steps ...RepairStep) error {
	t.Helper()
	return store.ApplyRepairPlan(context.Background(), "account-a", steps, repairMergeNowMS)
}

func repairDeleteStep(messageID, survivorID string) RepairStep {
	return RepairStep{
		Op:                "delete",
		MessageID:         messageID,
		SurvivorMessageID: survivorID,
		Reason:            "duplicate of " + survivorID,
	}
}

func assertOutboxConversation(t *testing.T, repository *OutboxRepository, outboxID, want string) {
	t.Helper()
	item, err := repository.FindByID(context.Background(), outboxID)
	if err != nil {
		t.Fatalf("FindByID(%q): %v", outboxID, err)
	}
	if item.ConversationID != want {
		t.Fatalf("outbox %q conversation = %q, want %q", outboxID, item.ConversationID, want)
	}
}

func assertReactionConversation(t *testing.T, store *Store, messageID, reactorKey, want string) {
	t.Helper()
	var got string
	if err := store.db.QueryRow(`
		SELECT conversation_id FROM reactions WHERE message_id = ? AND reactor_key = ?
	`, messageID, reactorKey).Scan(&got); err != nil {
		t.Fatalf("read reaction (%q, %q) conversation: %v", messageID, reactorKey, err)
	}
	if got != want {
		t.Fatalf("reaction (%q, %q) conversation = %q, want %q", messageID, reactorKey, got, want)
	}
}

func assertMessageExists(t *testing.T, store *Store, messageID string, want bool) {
	t.Helper()
	var count int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE message_id = ?`, messageID).Scan(&count); err != nil {
		t.Fatalf("count message %q: %v", messageID, err)
	}
	if (count == 1) != want {
		t.Fatalf("message %q exists = %v, want %v", messageID, count == 1, want)
	}
}

func TestApplyRepairPlanDeleteMergesDuplicateIntoSurvivor(t *testing.T) {
	for _, survivorConversation := range []string{"conversation-a", "conversation-b"} {
		for _, withReferences := range []bool{false, true} {
			name := "survivor in the same conversation"
			if survivorConversation != "conversation-a" {
				name = "survivor in another conversation"
			}
			if withReferences {
				name += "/with NO ACTION references"
			} else {
				name += "/cascade children only"
			}
			t.Run(name, func(t *testing.T) {
				store, repository := openRepairMergeStore(t)
				ctx := context.Background()
				seedOutboxTestMessage(t, store, repairMergeDuplicateID, "account-a", "conversation-a")
				seedOutboxTestMessage(t, store, repairMergeSurvivorID, "account-a", survivorConversation)
				seedOutboxTestMessage(t, store, repairMergeBystanderID, "account-a", "conversation-a")
				seedEchoCascadeChildren(t, store, repairMergeDuplicateID)
				var reactionIntent, receiptIntent NewOutboxItem
				if withReferences {
					reactionIntent, receiptIntent = seedRepairMergeIntents(t, store, repository, repairMergeDuplicateID)
				}
				if withReferences && survivorConversation != "conversation-a" {
					// A cursor can't follow its message into another conversation
					// (its foreign key is composite), so the planner never deletes
					// a cross-conversation duplicate a cursor names. The receipt
					// intent outlives the cursor once the device reads further.
					bystander := repairMergeBystanderID
					mustRepositoryWrite(t, "advance cursor", store.UpsertReadCursor(ReadCursor{
						AccountID: "account-a", DeviceID: echoMergeDeviceID, ConversationID: "conversation-a",
						LastReadMessageID: &bystander, LastReadAtMS: echoMergeReadAtMS + 1, UpdatedAtMS: echoMergeReadAtMS + 1,
					}))
				}

				if err := applyRepairPlan(t, store, repairDeleteStep(repairMergeDuplicateID, repairMergeSurvivorID)); err != nil {
					t.Fatalf("ApplyRepairPlan(): %v", err)
				}

				assertMessageExists(t, store, repairMergeDuplicateID, false)
				assertMessageExists(t, store, repairMergeSurvivorID, true)
				assertEchoMergedReaction(t, store, repairMergeSurvivorID, "reactor-delta", "thumbs-up", "active")
				assertEchoMergedReaction(t, store, repairMergeSurvivorID, "reactor-snapshot", "heart", "active")
				assertReactionConversation(t, store, repairMergeSurvivorID, "reactor-delta", survivorConversation)
				assertReactionConversation(t, store, repairMergeSurvivorID, "reactor-snapshot", survivorConversation)
				assertReactionFence(t, store, repairMergeSurvivorID, echoMergeFenceSeq)
				attachment, err := mustEchoMergeAttachments(t, store).GetForDownload(ctx, repairMergeSurvivorID, 0)
				if err != nil {
					t.Fatalf("GetForDownload(survivor attachment): %v", err)
				}
				if attachment.State != "downloaded" || attachment.BlobHash == nil || *attachment.BlobHash != echoMergeBlobHash {
					t.Fatalf("survivor attachment = %+v, want the duplicate's downloaded blob", attachment)
				}

				if withReferences {
					reaction, err := repository.GetOutboxReaction(ctx, reactionIntent.OutboxID)
					if err != nil || reaction.TargetMessageID != repairMergeSurvivorID {
						t.Fatalf("reaction intent = (%+v, %v), want it on the survivor", reaction, err)
					}
					receipt, err := repository.GetOutboxReadReceipt(ctx, receiptIntent.OutboxID)
					if err != nil || receipt.LastReadMessageID != repairMergeSurvivorID {
						t.Fatalf("read-receipt intent = (%+v, %v), want it on the survivor", receipt, err)
					}
					// The dispatcher refuses an intent whose target is outside the
					// intent's conversation, so the intents follow their target.
					assertOutboxConversation(t, repository, reactionIntent.OutboxID, survivorConversation)
					assertOutboxConversation(t, repository, receiptIntent.OutboxID, survivorConversation)

					cursor, err := store.GetReadCursor(echoMergeDeviceID, "conversation-a")
					if err != nil {
						t.Fatalf("GetReadCursor(): %v", err)
					}
					wantCursor := repairMergeSurvivorID
					if survivorConversation != "conversation-a" {
						wantCursor = repairMergeBystanderID
					}
					if cursor.LastReadMessageID == nil || *cursor.LastReadMessageID != wantCursor {
						t.Fatalf("read cursor = %+v, want it on %q", cursor, wantCursor)
					}
				}

				assertNoMessageReferences(t, store, repairMergeDuplicateID)
				assertForeignKeyCheckClean(t, store.db)
			})
		}
	}
}

func TestApplyRepairPlanDeleteRefusesCursorThatCannotFollow(t *testing.T) {
	store, repository := openRepairMergeStore(t)
	seedOutboxTestMessage(t, store, repairMergeDuplicateID, "account-a", "conversation-a")
	seedOutboxTestMessage(t, store, repairMergeSurvivorID, "account-a", "conversation-b")
	seedEchoCascadeChildren(t, store, repairMergeDuplicateID)
	seedRepairMergeIntents(t, store, repository, repairMergeDuplicateID)

	err := applyRepairPlan(t, store, repairDeleteStep(repairMergeDuplicateID, repairMergeSurvivorID))
	if err == nil || !strings.Contains(err.Error(), "read cursor") {
		t.Fatalf("ApplyRepairPlan() error = %v, want a refusal naming the read cursor", err)
	}
	// The plan rolled back: the duplicate and everything on it are intact.
	assertMessageExists(t, store, repairMergeDuplicateID, true)
	assertEchoMergedReaction(t, store, repairMergeDuplicateID, "reactor-delta", "thumbs-up", "active")
	assertReactionFence(t, store, repairMergeDuplicateID, echoMergeFenceSeq)
	assertRowCount(t, store.db, "message_attachments", 1)
	assertForeignKeyCheckClean(t, store.db)
}

func TestApplyRepairPlanDeleteFollowsSurvivorChain(t *testing.T) {
	for _, order := range []string{"survivor deleted first", "survivor deleted later"} {
		t.Run(order, func(t *testing.T) {
			store, repository := openRepairMergeStore(t)
			ctx := context.Background()
			const root = "message-repair-root"
			seedOutboxTestMessage(t, store, repairMergeDuplicateID, "account-a", "conversation-a")
			seedOutboxTestMessage(t, store, repairMergeSurvivorID, "account-a", "conversation-a")
			seedOutboxTestMessage(t, store, root, "account-a", "conversation-b")
			seedEchoCascadeChildren(t, store, repairMergeDuplicateID)
			reactionIntent := outboxTestReactionItem("repair-chain-reaction")
			if _, _, err := repository.EnqueueReaction(ctx, reactionIntent, OutboxReaction{
				TargetMessageID: repairMergeDuplicateID, Emoji: "thumbs-up", Action: "add",
			}); err != nil {
				t.Fatalf("EnqueueReaction(): %v", err)
			}

			steps := []RepairStep{
				repairDeleteStep(repairMergeSurvivorID, root),
				repairDeleteStep(repairMergeDuplicateID, repairMergeSurvivorID),
			}
			if order == "survivor deleted later" {
				steps[0], steps[1] = steps[1], steps[0]
			}
			if err := applyRepairPlan(t, store, steps...); err != nil {
				t.Fatalf("ApplyRepairPlan(): %v", err)
			}

			assertRowCount(t, store.db, "messages", 1)
			assertEchoMergedReaction(t, store, root, "reactor-delta", "thumbs-up", "active")
			assertReactionConversation(t, store, root, "reactor-delta", "conversation-b")
			assertReactionFence(t, store, root, echoMergeFenceSeq)
			reaction, err := repository.GetOutboxReaction(ctx, reactionIntent.OutboxID)
			if err != nil || reaction.TargetMessageID != root {
				t.Fatalf("reaction intent = (%+v, %v), want it on the root survivor", reaction, err)
			}
			assertOutboxConversation(t, repository, reactionIntent.OutboxID, "conversation-b")
			assertNoMessageReferences(t, store, repairMergeDuplicateID)
			assertNoMessageReferences(t, store, repairMergeSurvivorID)
			assertForeignKeyCheckClean(t, store.db)
		})
	}
}

func TestApplyRepairPlanDeleteRejectsInvalidSurvivor(t *testing.T) {
	cases := []struct {
		name     string
		steps    []RepairStep
		wantText string
	}{
		{
			name:     "no survivor",
			steps:    []RepairStep{{Op: "delete", MessageID: repairMergeDuplicateID}},
			wantText: "names no survivor",
		},
		{
			name:     "survivor is the duplicate",
			steps:    []RepairStep{repairDeleteStep(repairMergeDuplicateID, repairMergeDuplicateID)},
			wantText: "is its own survivor",
		},
		{
			name:     "survivor missing",
			steps:    []RepairStep{repairDeleteStep(repairMergeDuplicateID, "message-never-stored")},
			wantText: "survivor",
		},
		{
			name:     "survivor in another account",
			steps:    []RepairStep{repairDeleteStep(repairMergeDuplicateID, "message-other-account")},
			wantText: "account",
		},
		{
			name: "survivor chain loops back",
			steps: []RepairStep{
				repairDeleteStep(repairMergeSurvivorID, repairMergeDuplicateID),
				repairDeleteStep(repairMergeDuplicateID, repairMergeSurvivorID),
			},
			wantText: "is its own survivor",
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			store, _ := openRepairMergeStore(t)
			seedMessageAccount(t, store, "account-b", "test")
			seedMessageConversation(t, store, "conversation-other-account", "account-b")
			seedOutboxTestMessage(t, store, "message-other-account", "account-b", "conversation-other-account")
			seedOutboxTestMessage(t, store, repairMergeDuplicateID, "account-a", "conversation-a")
			seedOutboxTestMessage(t, store, repairMergeSurvivorID, "account-a", "conversation-a")
			seedEchoCascadeChildren(t, store, repairMergeDuplicateID)

			err := applyRepairPlan(t, store, test.steps...)
			if err == nil || !strings.Contains(err.Error(), test.wantText) {
				t.Fatalf("ApplyRepairPlan() error = %v, want one containing %q", err, test.wantText)
			}
			assertMessageExists(t, store, repairMergeDuplicateID, true)
			assertMessageExists(t, store, repairMergeSurvivorID, true)
			assertEchoMergedReaction(t, store, repairMergeDuplicateID, "reactor-delta", "thumbs-up", "active")
			assertRowCount(t, store.db, "message_attachments", 1)
		})
	}
}

func TestApplyRepairPlanDeleteOfMissingDuplicateIsNoOp(t *testing.T) {
	store, _ := openRepairMergeStore(t)
	seedOutboxTestMessage(t, store, repairMergeSurvivorID, "account-a", "conversation-a")
	if err := applyRepairPlan(t, store, repairDeleteStep(repairMergeDuplicateID, repairMergeSurvivorID)); err != nil {
		t.Fatalf("ApplyRepairPlan(): %v", err)
	}
	assertMessageExists(t, store, repairMergeSurvivorID, true)
}

func TestApplyRepairPlanMoveCarriesIntentsAndReactions(t *testing.T) {
	store, repository := openRepairMergeStore(t)
	ctx := context.Background()
	seedOutboxTestMessage(t, store, repairMergeDuplicateID, "account-a", "conversation-a")
	seedOutboxTestMessage(t, store, repairMergeBystanderID, "account-a", "conversation-a")
	seedEchoCascadeChildren(t, store, repairMergeDuplicateID)
	reactionIntent, receiptIntent := seedRepairMergeIntents(t, store, repository, repairMergeDuplicateID)
	bystander := repairMergeBystanderID
	mustRepositoryWrite(t, "advance cursor", store.UpsertReadCursor(ReadCursor{
		AccountID: "account-a", DeviceID: echoMergeDeviceID, ConversationID: "conversation-a",
		LastReadMessageID: &bystander, LastReadAtMS: echoMergeReadAtMS + 1, UpdatedAtMS: echoMergeReadAtMS + 1,
	}))

	if err := applyRepairPlan(t, store, RepairStep{
		Op: "move", MessageID: repairMergeDuplicateID, ConversationID: "conversation-a",
		TargetConversationID: "conversation-b",
	}); err != nil {
		t.Fatalf("ApplyRepairPlan(move): %v", err)
	}
	message, err := mustMessageRepository(t, store, repairMergeNowMS).GetMessage(ctx, repairMergeDuplicateID)
	if err != nil || message.ConversationID != "conversation-b" {
		t.Fatalf("moved message = (%+v, %v), want it in conversation-b", message, err)
	}
	assertReactionConversation(t, store, repairMergeDuplicateID, "reactor-delta", "conversation-b")
	assertOutboxConversation(t, repository, reactionIntent.OutboxID, "conversation-b")
	assertOutboxConversation(t, repository, receiptIntent.OutboxID, "conversation-b")
	assertForeignKeyCheckClean(t, store.db)
}

func TestApplyRepairPlanMoveRefusesMessageACursorNames(t *testing.T) {
	store, repository := openRepairMergeStore(t)
	seedOutboxTestMessage(t, store, repairMergeDuplicateID, "account-a", "conversation-a")
	seedRepairMergeIntents(t, store, repository, repairMergeDuplicateID)

	err := applyRepairPlan(t, store, RepairStep{
		Op: "move", MessageID: repairMergeDuplicateID, ConversationID: "conversation-a",
		TargetConversationID: "conversation-b",
	})
	if err == nil || !strings.Contains(err.Error(), "read cursor") {
		t.Fatalf("ApplyRepairPlan(move) error = %v, want a refusal naming the read cursor", err)
	}
	assertMessageExists(t, store, repairMergeDuplicateID, true)
	assertForeignKeyCheckClean(t, store.db)
}
