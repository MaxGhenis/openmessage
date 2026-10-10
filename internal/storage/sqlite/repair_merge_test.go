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
	"time"
)

const (
	repairMergeDuplicateID = "message-repair-duplicate"
	repairMergeSurvivorID  = "message-repair-survivor"
	repairMergeBystanderID = "message-repair-bystander"
	repairMergeNowMS       = outboxTestTimeMS + 5_000
	repairMergeReactorA    = "repair-reactor-a"
	repairMergeReactorB    = "repair-reactor-b"
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

// seedRepairMergeChildren records an embedded reaction snapshot (two reactors
// and its fence) and a downloaded attachment on messageID, which must be in
// conversation-a.
func seedRepairMergeChildren(t *testing.T, store *Store, messageID string) {
	t.Helper()
	ctx := context.Background()
	reactions, err := NewReactionRepository(store, func() time.Time { return time.UnixMilli(outboxTestTimeMS - 300) })
	if err != nil {
		t.Fatalf("NewReactionRepository(): %v", err)
	}
	if _, err := reactions.ReplaceEmbeddedReactions(ctx, messageID, "account-a", "conversation-a",
		[]ReactionSnapshotEntry{
			{ReactorKey: repairMergeReactorA, ReactorLabel: "peer", Emoji: "heart"},
			{ReactorKey: repairMergeReactorB, ReactorIsSelf: true, Emoji: "thumbs-up"},
		},
		echoMergeFenceSeq,
	); err != nil {
		t.Fatalf("ReplaceEmbeddedReactions(): %v", err)
	}
	mustExec(t, store.db, `
		INSERT INTO message_attachments (
			message_id, ordinal, remote_id, remote_ref, filename, mime, size_bytes,
			state, blob_hash, last_error, created_at_ms, updated_at_ms
		) VALUES (?, 0, 'media-0', x'', 'photo.png', 'image/png', 42, 'downloaded', ?, NULL, ?, ?)
	`, messageID, echoMergeBlobHash, outboxTestTimeMS-300, outboxTestTimeMS-300)
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
				seedRepairMergeChildren(t, store, repairMergeDuplicateID)
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
				assertEchoMergedReaction(t, store, repairMergeSurvivorID, repairMergeReactorB, "thumbs-up", "active")
				assertEchoMergedReaction(t, store, repairMergeSurvivorID, repairMergeReactorA, "heart", "active")
				assertReactionConversation(t, store, repairMergeSurvivorID, repairMergeReactorB, survivorConversation)
				assertReactionConversation(t, store, repairMergeSurvivorID, repairMergeReactorA, survivorConversation)
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
	seedRepairMergeChildren(t, store, repairMergeDuplicateID)
	seedRepairMergeIntents(t, store, repository, repairMergeDuplicateID)

	err := applyRepairPlan(t, store, repairDeleteStep(repairMergeDuplicateID, repairMergeSurvivorID))
	if err == nil || !strings.Contains(err.Error(), "read cursor") {
		t.Fatalf("ApplyRepairPlan() error = %v, want a refusal naming the read cursor", err)
	}
	// The plan rolled back: the duplicate and everything on it are intact.
	assertMessageExists(t, store, repairMergeDuplicateID, true)
	assertEchoMergedReaction(t, store, repairMergeDuplicateID, repairMergeReactorB, "thumbs-up", "active")
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
			seedRepairMergeChildren(t, store, repairMergeDuplicateID)
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
			assertEchoMergedReaction(t, store, root, repairMergeReactorB, "thumbs-up", "active")
			assertReactionConversation(t, store, root, repairMergeReactorB, "conversation-b")
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
			name:     "survivor holds other content",
			steps:    []RepairStep{repairDeleteStep(repairMergeDuplicateID, "message-other-content")},
			wantText: "different content",
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
			seedOutboxTestMessage(t, store, "message-other-content", "account-a", "conversation-a")
			mustExec(t, store.db, `UPDATE messages SET body = 'other text' WHERE message_id = 'message-other-content'`)
			seedOutboxTestMessage(t, store, repairMergeDuplicateID, "account-a", "conversation-a")
			seedOutboxTestMessage(t, store, repairMergeSurvivorID, "account-a", "conversation-a")
			seedRepairMergeChildren(t, store, repairMergeDuplicateID)

			err := applyRepairPlan(t, store, test.steps...)
			if err == nil || !strings.Contains(err.Error(), test.wantText) {
				t.Fatalf("ApplyRepairPlan() error = %v, want one containing %q", err, test.wantText)
			}
			assertMessageExists(t, store, repairMergeDuplicateID, true)
			assertMessageExists(t, store, repairMergeSurvivorID, true)
			assertEchoMergedReaction(t, store, repairMergeDuplicateID, repairMergeReactorB, "thumbs-up", "active")
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
	seedRepairMergeChildren(t, store, repairMergeDuplicateID)
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
	assertReactionConversation(t, store, repairMergeDuplicateID, repairMergeReactorB, "conversation-b")
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

// A send that hasn't finished reads its local message inside its own
// conversation at dispatch, so a repair must neither delete that row nor move
// it elsewhere. Once the send is confirmed, rejected or canceled the row is
// ordinary history.
func TestApplyRepairPlanRefusesMessagesAnOpenSendNames(t *testing.T) {
	for _, test := range []struct {
		name  string
		state string
		step  RepairStep
		want  string
	}{
		{name: "delete under a queued send", state: "queued", step: repairDeleteStep("message-repair-send", repairMergeSurvivorID), want: "unfinished send"},
		{name: "move under an uncertain send", state: "uncertain", step: RepairStep{Op: "move", MessageID: "message-repair-send", TargetConversationID: "conversation-b"}, want: "unfinished send"},
		{name: "move within the send's conversation", state: "queued", step: RepairStep{Op: "move", MessageID: "message-repair-send", TargetConversationID: "conversation-a"}},
		{name: "delete after the send was canceled", state: "canceled", step: repairDeleteStep("message-repair-send", repairMergeSurvivorID)},
		{name: "move after the send was confirmed", state: "confirmed", step: RepairStep{Op: "move", MessageID: "message-repair-send", TargetConversationID: "conversation-b"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, repository := openRepairMergeStore(t)
			item := outboxTestItem("repair-send")
			mustEnqueueOutgoingOutbox(t, repository, item, "optimistic body")
			// The survivor restates the optimistic row exactly.
			mustExec(t, store.db, `
				INSERT INTO messages (
					message_id, conversation_id, account_id, remote_message_id, sender_identity_id, direction,
					body, reply_to_remote_id, state, occurred_at_ms, created_at_ms, updated_at_ms
				)
				SELECT ?, conversation_id, account_id, 'remote-survivor', sender_identity_id, direction,
					body, NULL, 'active', occurred_at_ms, created_at_ms, updated_at_ms
				FROM messages WHERE message_id = ?
			`, repairMergeSurvivorID, item.LocalMessageID)
			mustExec(t, store.db, `UPDATE outbox SET state = ? WHERE outbox_id = ?`, test.state, item.OutboxID)

			err := applyRepairPlan(t, store, test.step)
			if test.want == "" {
				if err != nil {
					t.Fatalf("ApplyRepairPlan(): %v", err)
				}
				assertForeignKeyCheckClean(t, store.db)
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ApplyRepairPlan() error = %v, want one containing %q", err, test.want)
			}
			message, err := mustMessageRepository(t, store, repairMergeNowMS).GetMessage(context.Background(), item.LocalMessageID)
			if err != nil || message.ConversationID != item.ConversationID {
				t.Fatalf("optimistic row = (%+v, %v), want it unchanged in %s", message, err, item.ConversationID)
			}
		})
	}
}
