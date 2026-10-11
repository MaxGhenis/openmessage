package sqlite

import (
	"context"
	"database/sql"
	"math/rand"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"testing/quick"
)

// Property tests for ApplyRepairPlan's move and delete steps against a
// reference interpreter over plain Go maps.

var (
	repairPropertyConversations = []string{"conversation-a", "conversation-b", "conversation-c"}
	repairPropertyDevices       = []string{"device-p0", "device-p1"}
)

const repairPropertyMessages = 6

func repairPropertyMessageID(index int) string {
	return "message-p" + strconv.Itoa(index)
}

// repairPlanScenario is a random message graph and a random plan over it.
type repairPlanScenario struct {
	Conversations   [repairPropertyMessages]int // conversation index per message
	Contents        [repairPropertyMessages]int // content variant per message; deletes need equal content
	Sends           map[int]repairSendSeed      // message -> the send naming it as its local copy
	Reactions       map[[2]int]echoMergeReactionSeed
	Fences          map[int]int64
	Attachments     map[[2]int]int // (message, ordinal) -> 1 pending, 2 downloaded
	Cursors         map[[2]int]int // (device, conversation) -> message
	ReactionIntents []int          // target message per intent
	ReceiptIntents  []int
	FutureOutbox    bool // some intents carry updated_at_ms after the repair clock
	Steps           []RepairStep
}

func (repairPlanScenario) Generate(r *rand.Rand, _ int) reflect.Value {
	scenario := repairPlanScenario{
		Reactions:    map[[2]int]echoMergeReactionSeed{},
		Fences:       map[int]int64{},
		Attachments:  map[[2]int]int{},
		Cursors:      map[[2]int]int{},
		Sends:        map[int]repairSendSeed{},
		FutureOutbox: r.Intn(4) == 0,
	}
	for index := range scenario.Conversations {
		scenario.Conversations[index] = r.Intn(len(repairPropertyConversations))
		if r.Intn(4) == 0 {
			scenario.Contents[index] = 1 + r.Intn(len(repairContentVariants)-1)
		}
		if r.Intn(6) == 0 {
			scenario.Sends[index] = repairSendSeed{
				State: repairSendStates[r.Intn(len(repairSendStates))],
				Kind:  []string{"text", "media"}[r.Intn(2)],
			}
		}
	}
	for message := range repairPropertyMessages {
		for reactor := range 3 {
			if r.Intn(5) >= 2 {
				continue
			}
			seed := echoMergeReactionSeed{
				Emoji:        []string{"a", "b", "c"}[r.Intn(3)],
				State:        "active",
				OccurredAtMS: outboxTestTimeMS - 800 + int64(r.Intn(3)),
				SourceSeqMS:  int64(r.Intn(3)),
				UpdatedAtMS:  outboxTestTimeMS - 900 + int64(r.Intn(3)),
			}
			if r.Intn(8) == 0 {
				seed.UpdatedAtMS = repairMergeNowMS + 5
			}
			if r.Intn(5) == 0 {
				seed.State = "removed"
				if r.Intn(2) == 0 {
					seed.Emoji = ""
				}
			}
			scenario.Reactions[[2]int{message, reactor}] = seed
		}
		if r.Intn(5) < 2 {
			scenario.Fences[message] = int64(r.Intn(4))
		}
		for ordinal := range 2 {
			if state := []int{0, 0, 1, 2}[r.Intn(4)]; state != 0 {
				scenario.Attachments[[2]int{message, ordinal}] = state
			}
		}
	}
	// Half the scenarios carry no cursors, so most plans can apply.
	if r.Intn(2) == 0 {
		for device := range repairPropertyDevices {
			for conversation := range repairPropertyConversations {
				var members []int
				for message, at := range scenario.Conversations {
					if at == conversation {
						members = append(members, message)
					}
				}
				if len(members) > 0 && r.Intn(3) == 0 {
					scenario.Cursors[[2]int{device, conversation}] = members[r.Intn(len(members))]
				}
			}
		}
	}
	for range r.Intn(4) {
		scenario.ReactionIntents = append(scenario.ReactionIntents, r.Intn(repairPropertyMessages))
	}
	for range r.Intn(3) {
		scenario.ReceiptIntents = append(scenario.ReceiptIntents, r.Intn(repairPropertyMessages))
	}
	for range 1 + r.Intn(5) {
		message := repairPropertyMessageID(r.Intn(repairPropertyMessages))
		if r.Intn(10) < 3 {
			target := repairPropertyConversations[r.Intn(len(repairPropertyConversations))]
			scenario.Steps = append(scenario.Steps, RepairStep{
				Op: "move", MessageID: message, TargetConversationID: target,
			})
			continue
		}
		survivor := repairPropertyMessageID(r.Intn(repairPropertyMessages))
		switch r.Intn(20) {
		case 0:
			survivor = ""
		case 1:
			survivor = "message-never-stored"
		}
		scenario.Steps = append(scenario.Steps, repairDeleteStep(message, survivor))
	}
	return reflect.ValueOf(scenario)
}

type repairSendSeed struct {
	State, Kind string
}

// repairSendStates are outbox states a seeded send can be in: three open
// (dispatch or reconcile still needs the local row) and the three closed.
var repairSendStates = []string{"queued", "uncertain", "store_failed", "confirmed", "rejected", "canceled"}

func repairSendIsOpen(state string) bool {
	return state == "queued" || state == "uncertain" || state == "store_failed"
}

// repairContentVariants each change one field of the planner's content key.
// Variant 0 is the base content.
var repairContentVariants = []string{
	``,
	`body = 'other body'`,
	`occurred_at_ms = occurred_at_ms + 1`,
	`direction = 'outgoing'`,
	`sender_identity_id = 'identity-a'`,
}

// repairPlanState is every row the move and delete steps touch.
type repairPlanState struct {
	Messages    map[string]repairMessageRow // message_id
	Sends       map[string]repairSendRow    // outbox_id of text sends
	Reactions   map[[2]string]echoMergeReactionRow
	Fences      map[string]echoMergeFenceRow
	Attachments map[echoMergeAttachmentKey]echoMergeAttachmentRow
	Cursors     map[[2]string]echoMergeCursorRow
	Intents     map[string]repairIntentRow // outbox_id
}

type repairMessageRow struct {
	ConversationID, Content string
}

type repairSendRow struct {
	LocalMessageID, ConversationID, State string
}

type repairIntentRow struct {
	Target, ConversationID string
	UpdatedAtMS            int64
}

func (state repairPlanState) clone() repairPlanState {
	clone := repairPlanState{
		Messages:    map[string]repairMessageRow{},
		Sends:       state.Sends,
		Reactions:   map[[2]string]echoMergeReactionRow{},
		Fences:      map[string]echoMergeFenceRow{},
		Attachments: map[echoMergeAttachmentKey]echoMergeAttachmentRow{},
		Cursors:     map[[2]string]echoMergeCursorRow{},
		Intents:     map[string]repairIntentRow{},
	}
	for key, value := range state.Messages {
		clone.Messages[key] = value
	}
	for key, value := range state.Reactions {
		clone.Reactions[key] = value
	}
	for key, value := range state.Fences {
		clone.Fences[key] = value
	}
	for key, value := range state.Attachments {
		clone.Attachments[key] = value
	}
	for key, value := range state.Cursors {
		clone.Cursors[key] = value
	}
	for key, value := range state.Intents {
		clone.Intents[key] = value
	}
	return clone
}

// repairPlanModel is the reference semantics of ApplyRepairPlan's move and
// delete steps, written independently over repairPlanState. It returns the
// state after the plan and whether the plan applies; a plan that fails at any
// step leaves the state as it was. coverage counts the paths taken.
func repairPlanModel(before repairPlanState, steps []RepairStep, nowMS int64, coverage map[string]int) (repairPlanState, bool) {
	state := before.clone()
	mergedInto := map[string]string{}
	survivors := map[string]bool{} // messages a delete merged into
	refuse := func(reason string) (repairPlanState, bool) {
		coverage["refused: "+reason]++
		return before, false
	}
	for _, step := range steps {
		switch step.Op {
		case "move":
			message, exists := state.Messages[step.MessageID]
			if !exists {
				coverage["move of a missing message"]++
				continue
			}
			for _, cursor := range state.Cursors {
				if cursor.LastReadMessageID.String == step.MessageID && cursor.ConversationID != step.TargetConversationID {
					return refuse("move under a read cursor")
				}
			}
			for _, send := range state.Sends {
				if send.LocalMessageID == step.MessageID && repairSendIsOpen(send.State) &&
					send.ConversationID != step.TargetConversationID {
					return refuse("move under an open send")
				}
			}
			for key, reaction := range state.Reactions {
				if reaction.MessageID == step.MessageID {
					reaction.ConversationID = step.TargetConversationID
					reaction.UpdatedAtMS = max(reaction.UpdatedAtMS, nowMS)
					state.Reactions[key] = reaction
				}
			}
			for outboxID, intent := range state.Intents {
				if intent.Target == step.MessageID && intent.ConversationID != step.TargetConversationID {
					intent.ConversationID = step.TargetConversationID
					intent.UpdatedAtMS = max(intent.UpdatedAtMS, nowMS)
					state.Intents[outboxID] = intent
					coverage["intent follows a moved message"]++
				}
			}
			if message.ConversationID != step.TargetConversationID {
				coverage["move across conversations"]++
			}
			message.ConversationID = step.TargetConversationID
			state.Messages[step.MessageID] = message
		case "delete":
			duplicate := step.MessageID
			if step.SurvivorMessageID == "" {
				return refuse("no survivor")
			}
			duplicateRow, exists := state.Messages[duplicate]
			if !exists {
				coverage["delete of a missing duplicate"]++
				continue
			}
			for _, send := range state.Sends {
				if send.LocalMessageID == duplicate && repairSendIsOpen(send.State) {
					return refuse("delete under an open send")
				}
			}
			survivor := step.SurvivorMessageID
			for {
				next, merged := mergedInto[survivor]
				if !merged {
					break
				}
				survivor = next
				coverage["survivor chain hop"]++
			}
			if survivor == duplicate {
				return refuse("own survivor")
			}
			survivorRow, exists := state.Messages[survivor]
			if !exists {
				return refuse("survivor missing")
			}
			if survivorRow.Content != duplicateRow.Content {
				return refuse("content differs")
			}
			survivorConversation := survivorRow.ConversationID
			if survivorConversation != duplicateRow.ConversationID {
				for _, cursor := range state.Cursors {
					if cursor.LastReadMessageID.String == duplicate {
						return refuse("cursor can't follow")
					}
				}
				coverage["merge across conversations"]++
			} else {
				coverage["merge within a conversation"]++
			}
			for outboxID, intent := range state.Intents {
				if intent.Target != duplicate {
					continue
				}
				intent.Target = survivor
				if intent.ConversationID != survivorConversation {
					intent.ConversationID = survivorConversation
					intent.UpdatedAtMS = max(intent.UpdatedAtMS, nowMS)
					coverage["intent follows its target across conversations"]++
				}
				state.Intents[outboxID] = intent
			}
			for key, cursor := range state.Cursors {
				if cursor.LastReadMessageID.String == duplicate {
					cursor.LastReadMessageID.String = survivor
					cursor.UpdatedAtMS = max(cursor.UpdatedAtMS, nowMS)
					state.Cursors[key] = cursor
					coverage["cursor follows its message"]++
				}
			}
			// A fence on either row means the reactions come from embedded
			// snapshots, which order by source_seq_ms first; deltas order by
			// occurred_at_ms first.
			_, fenced := state.Fences[duplicate]
			if _, survivorFenced := state.Fences[survivor]; survivorFenced {
				fenced = true
			}
			rank := func(reaction echoMergeReactionRow) [2]int64 {
				if fenced {
					return [2]int64{reaction.SourceSeqMS, reaction.OccurredAtMS}
				}
				return [2]int64{reaction.OccurredAtMS, reaction.SourceSeqMS}
			}
			// Per reactor the newer row wins, and the survivor's on a tie.
			// Losing rows on the duplicate are deleted with it.
			for key, reaction := range state.Reactions {
				if reaction.MessageID != duplicate {
					continue
				}
				delete(state.Reactions, key)
				survivorKey := [2]string{survivor, reaction.ReactorKey}
				if existing, conflict := state.Reactions[survivorKey]; conflict {
					mine, theirs := rank(reaction), rank(existing)
					if mine[0] < theirs[0] || (mine[0] == theirs[0] && mine[1] <= theirs[1]) {
						coverage["reaction conflict: survivor's row stays"]++
						continue
					}
				}
				reaction.MessageID = survivor
				reaction.ConversationID = survivorConversation
				reaction.UpdatedAtMS = max(reaction.UpdatedAtMS, nowMS)
				state.Reactions[survivorKey] = reaction
			}
			survivors[survivor] = true
			if fence, ok := state.Fences[duplicate]; ok {
				delete(state.Fences, duplicate)
				existing, exists := state.Fences[survivor]
				switch {
				case !exists:
					state.Fences[survivor] = echoMergeFenceRow{survivor, fence.SourceSeqMS, nowMS}
				case fence.SourceSeqMS > existing.SourceSeqMS:
					state.Fences[survivor] = echoMergeFenceRow{survivor, fence.SourceSeqMS, max(existing.UpdatedAtMS, nowMS)}
				}
			}
			// Per ordinal, a downloaded survivor row stays; otherwise the
			// duplicate's row replaces it.
			for key, attachment := range state.Attachments {
				if key.MessageID != duplicate {
					continue
				}
				delete(state.Attachments, key)
				survivorKey := echoMergeAttachmentKey{survivor, key.Ordinal}
				if existing, exists := state.Attachments[survivorKey]; exists && existing.BlobHash.Valid {
					continue
				}
				attachment.echoMergeAttachmentKey = survivorKey
				attachment.UpdatedAtMS = max(attachment.UpdatedAtMS, nowMS)
				state.Attachments[survivorKey] = attachment
			}
			delete(state.Messages, duplicate)
			mergedInto[duplicate] = survivor
		}
	}
	// After every step, a reactor still active below a survivor's merged
	// fence is tombstoned: the newest snapshot didn't list it. Doing this
	// once, last, keeps a synthesized tombstone from outranking a later
	// duplicate's real row.
	for key, reaction := range state.Reactions {
		fence, fenced := state.Fences[reaction.MessageID]
		if survivors[reaction.MessageID] && fenced && reaction.State == "active" && reaction.SourceSeqMS < fence.SourceSeqMS {
			reaction.State = "removed"
			reaction.OccurredAtMS = inferredTombstoneOccurredAtMS
			reaction.SourceSeqMS = fence.SourceSeqMS
			reaction.UpdatedAtMS = max(reaction.UpdatedAtMS, nowMS)
			state.Reactions[key] = reaction
			coverage["reaction tombstoned below the merged fence"]++
		}
	}
	coverage["plan applied"]++
	return state, true
}

// Property (repair plan model): for any message graph and any sequence of
// move and delete steps, ApplyRepairPlan either commits exactly the rows
// repairPlanModel derives, or fails and leaves every row as it was. The model
// refuses a delete whose survivor holds other content, so a deleted message's
// content always survives. After a committed plan, no row references a
// deleted message, foreign_key_check is clean, cursors and intents are
// conserved, every intent's target is in the intent's own conversation, and
// every open send still has its local message in its own conversation.
func TestApplyRepairPlanMatchesModelProperty(t *testing.T) {
	coverage := map[string]int{}
	clock := newOutboxTestClock(outboxTestTimeMS)
	store, repository := openOutboxTestRepository(t, clock.Now)
	seedMessageIdentity(t, store, "identity-a", "account-a")
	for _, conversation := range repairPropertyConversations {
		seedMessageConversation(t, store, conversation, "account-a")
	}
	for _, device := range repairPropertyDevices {
		seedOutboxTestDevice(t, store, device, "account-a")
	}
	ctx := context.Background()
	property := func(scenario repairPlanScenario) bool {
		for _, statement := range []string{`DELETE FROM outbox`, `DELETE FROM read_cursors`, `DELETE FROM messages`} {
			if _, err := store.db.Exec(statement); err != nil {
				t.Errorf("reset (%s): %v", statement, err)
				return false
			}
		}
		seedRepairPlanScenario(t, store, repository, scenario)
		before := readRepairPlanState(t, store)

		err := store.ApplyRepairPlan(ctx, "account-a", scenario.Steps, repairMergeNowMS)
		after := readRepairPlanState(t, store)
		want, applies := repairPlanModel(before, scenario.Steps, repairMergeNowMS, coverage)
		if applies != (err == nil) {
			t.Errorf("scenario %+v: ApplyRepairPlan() error = %v, model applies = %v", scenario, err, applies)
			return false
		}
		if !reflect.DeepEqual(after, want) {
			t.Errorf("scenario %+v (error %v): rows differ from the model\n got %+v\nwant %+v", scenario, err, after, want)
			return false
		}
		if err != nil {
			return true
		}
		for messageID := range before.Messages {
			if _, kept := after.Messages[messageID]; !kept {
				assertNoMessageReferences(t, store, messageID)
			}
		}
		assertForeignKeyCheckClean(t, store.db)
		if len(after.Cursors) != len(before.Cursors) || len(after.Intents) != len(before.Intents) {
			t.Errorf("cursor/intent counts changed: %d->%d, %d->%d",
				len(before.Cursors), len(after.Cursors), len(before.Intents), len(after.Intents))
		}
		for outboxID, intent := range after.Intents {
			if after.Messages[intent.Target].ConversationID != intent.ConversationID {
				t.Errorf("intent %s targets %s in %s, outside its conversation %s",
					outboxID, intent.Target, after.Messages[intent.Target].ConversationID, intent.ConversationID)
			}
		}
		for outboxID, send := range after.Sends {
			if !repairSendIsOpen(send.State) {
				continue
			}
			if row, kept := after.Messages[send.LocalMessageID]; !kept || row.ConversationID != send.ConversationID {
				t.Errorf("open send %s lost its local message %s in %s", outboxID, send.LocalMessageID, send.ConversationID)
			}
		}
		return !t.Failed()
	}
	if err := quick.Check(property, repairPropertyConfig(t, 400, 120)); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"plan applied", "merge within a conversation", "merge across conversations",
		"survivor chain hop", "intent follows its target across conversations",
		"intent follows a moved message", "cursor follows its message", "move across conversations",
		"delete of a missing duplicate", "move of a missing message",
		"refused: no survivor", "refused: own survivor", "refused: survivor missing",
		"refused: cursor can't follow", "refused: move under a read cursor",
		"refused: move under an open send", "refused: delete under an open send", "refused: content differs",
		"reaction conflict: survivor's row stays", "reaction tombstoned below the merged fence",
	} {
		if coverage[name] == 0 {
			t.Errorf("generator never produced %q; coverage = %v", name, coverage)
		}
	}
}

func seedRepairPlanScenario(t *testing.T, store *Store, repository *OutboxRepository, scenario repairPlanScenario) {
	t.Helper()
	ctx := context.Background()
	conversationOf := func(message int) string {
		return repairPropertyConversations[scenario.Conversations[message]]
	}
	for message := range repairPropertyMessages {
		seedOutboxTestMessage(t, store, repairPropertyMessageID(message), "account-a", conversationOf(message))
		if variant := repairContentVariants[scenario.Contents[message]]; variant != "" {
			mustExec(t, store.db, `UPDATE messages SET `+variant+` WHERE message_id = ?`, repairPropertyMessageID(message))
		}
	}
	for message, send := range scenario.Sends {
		id := "plan-send-" + strconv.Itoa(message)
		mustExec(t, store.db, `
			INSERT INTO outbox (
				outbox_id, account_id, conversation_id, kind, idempotency_key, payload_hash, operation,
				state, local_message_id, transport_request_id, scheduled_for_ms, created_at_ms, updated_at_ms
			) VALUES (?, 'account-a', ?, ?, ?, ?, 'send', ?, ?, ?, ?, ?, ?)
		`, "outbox-"+id, conversationOf(message), send.Kind, "idempotency-"+id, "hash-"+id, send.State,
			repairPropertyMessageID(message), "request-"+id, outboxTestTimeMS, outboxTestTimeMS, outboxTestTimeMS)
	}
	for key, seed := range scenario.Reactions {
		messageID := repairPropertyMessageID(key[0])
		mustExec(t, store.db, `
			INSERT INTO reactions (
				message_id, reactor_key, account_id, conversation_id, reactor_identity_id,
				reactor_is_self, reactor_label, emoji, state, occurred_at_ms, source_seq_ms,
				created_at_ms, updated_at_ms
			) VALUES (?, ?, 'account-a', ?, NULL, 0, ?, ?, ?, ?, ?, ?, ?)
		`, messageID, "r"+strconv.Itoa(key[1]), conversationOf(key[0]), "label-"+messageID, seed.Emoji, seed.State,
			seed.OccurredAtMS, seed.SourceSeqMS, outboxTestTimeMS-950, seed.UpdatedAtMS)
	}
	for message, seq := range scenario.Fences {
		mustExec(t, store.db, `
			INSERT INTO reaction_snapshot_fences (message_id, source_seq_ms, updated_at_ms) VALUES (?, ?, ?)
		`, repairPropertyMessageID(message), seq, outboxTestTimeMS-700)
	}
	for key, state := range scenario.Attachments {
		messageID := repairPropertyMessageID(key[0])
		var blobHash any
		stateName := "pending"
		if state == 2 {
			stateName = "downloaded"
			blobHash = strings.Repeat(strconv.Itoa(key[0]), 63) + strconv.Itoa(key[1])
		}
		mustExec(t, store.db, `
			INSERT INTO message_attachments (
				message_id, ordinal, remote_id, remote_ref, filename, mime, size_bytes,
				state, blob_hash, last_error, created_at_ms, updated_at_ms
			) VALUES (?, ?, ?, x'', ?, 'image/png', 10, ?, ?, NULL, ?, ?)
		`, messageID, key[1], "remote-"+messageID, "file-"+messageID, stateName, blobHash,
			outboxTestTimeMS-600, outboxTestTimeMS-600+int64(key[1]))
	}
	for index, target := range scenario.ReactionIntents {
		intent := outboxTestReactionItem("plan-reaction-" + strconv.Itoa(index))
		intent.ConversationID = conversationOf(target)
		if _, _, err := repository.EnqueueReaction(ctx, intent, OutboxReaction{
			TargetMessageID: repairPropertyMessageID(target), Emoji: "a", Action: "add",
		}); err != nil {
			t.Fatalf("EnqueueReaction(): %v", err)
		}
	}
	for index, target := range scenario.ReceiptIntents {
		intent := outboxTestReadItem("plan-read-" + strconv.Itoa(index))
		intent.ConversationID = conversationOf(target)
		messageID := repairPropertyMessageID(target)
		if _, _, err := repository.EnqueueReadReceipt(ctx, intent, OutboxReadReceipt{
			DeviceID: repairPropertyDevices[0], LastReadMessageID: messageID, ReadAtMS: outboxTestTimeMS - 300,
		}, ReadCursor{
			AccountID: "account-a", DeviceID: repairPropertyDevices[0], ConversationID: intent.ConversationID,
			LastReadMessageID: &messageID, LastReadAtMS: outboxTestTimeMS - 300, UpdatedAtMS: outboxTestTimeMS - 300,
		}); err != nil {
			t.Fatalf("EnqueueReadReceipt(): %v", err)
		}
	}
	if scenario.FutureOutbox {
		mustExec(t, store.db, `UPDATE outbox SET updated_at_ms = ? WHERE outbox_id LIKE '%-0'`, repairMergeNowMS+5)
	}
	// Enqueueing a receipt moved a cursor; the scenario's cursors replace them.
	mustExec(t, store.db, `DELETE FROM read_cursors`)
	for key, message := range scenario.Cursors {
		messageID := repairPropertyMessageID(message)
		mustRepositoryWrite(t, "seed cursor", store.UpsertReadCursor(ReadCursor{
			AccountID: "account-a", DeviceID: repairPropertyDevices[key[0]], ConversationID: repairPropertyConversations[key[1]],
			LastReadMessageID: &messageID, LastReadAtMS: outboxTestTimeMS - 400, UpdatedAtMS: outboxTestTimeMS - 400 + int64(key[0]),
		}))
	}
}

func readRepairPlanState(t *testing.T, store *Store) repairPlanState {
	t.Helper()
	snapshot := readEchoMergeSnapshot(t, store)
	state := repairPlanState{
		Messages:    map[string]repairMessageRow{},
		Sends:       map[string]repairSendRow{},
		Reactions:   snapshot.Reactions,
		Fences:      snapshot.Fences,
		Attachments: snapshot.Attachments,
		Cursors:     snapshot.Cursors,
		Intents:     map[string]repairIntentRow{},
	}
	query := func(statement string, each func(*sql.Rows) error) {
		rows, err := store.db.Query(statement)
		if err != nil {
			t.Fatalf("snapshot %q: %v", statement, err)
		}
		defer rows.Close()
		for rows.Next() {
			if err := each(rows); err != nil {
				t.Fatalf("scan snapshot %q: %v", statement, err)
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("iterate snapshot %q: %v", statement, err)
		}
	}
	query(`SELECT message_id, conversation_id,
			direction || '|' || COALESCE(sender_identity_id, '') || '|' || occurred_at_ms || '|' || body
		FROM messages`, func(rows *sql.Rows) error {
		var messageID string
		var row repairMessageRow
		err := rows.Scan(&messageID, &row.ConversationID, &row.Content)
		state.Messages[messageID] = row
		return err
	})
	query(`SELECT outbox_id, local_message_id, conversation_id, state FROM outbox WHERE kind IN ('text', 'media')`, func(rows *sql.Rows) error {
		var outboxID string
		var send repairSendRow
		err := rows.Scan(&outboxID, &send.LocalMessageID, &send.ConversationID, &send.State)
		state.Sends[outboxID] = send
		return err
	})
	query(`
		SELECT o.outbox_id, COALESCE(r.target_message_id, p.last_read_message_id), o.conversation_id, o.updated_at_ms
		FROM outbox AS o
		LEFT JOIN outbox_reactions AS r ON r.outbox_id = o.outbox_id
		LEFT JOIN outbox_read_receipts AS p ON p.outbox_id = o.outbox_id
		WHERE o.kind IN ('reaction', 'read')
	`, func(rows *sql.Rows) error {
		var outboxID string
		var intent repairIntentRow
		err := rows.Scan(&outboxID, &intent.Target, &intent.ConversationID, &intent.UpdatedAtMS)
		state.Intents[outboxID] = intent
		return err
	})
	return state
}

// repairPropertyConfig returns the cases and seed for a repair property test:
// fewer cases under -race, where the pure-Go SQLite engine runs about tenfold
// slower. OPENMESSAGE_PROPERTY_CASES and OPENMESSAGE_PROPERTY_SEED override
// both, so a sweep over seeds needs no rebuild.
func repairPropertyConfig(t *testing.T, cases, raceCases int) *quick.Config {
	t.Helper()
	if raceDetectorEnabled {
		cases = raceCases
	}
	seed := int64(20261010)
	for name, target := range map[string]func(int64){
		"OPENMESSAGE_PROPERTY_CASES": func(value int64) { cases = int(value) },
		"OPENMESSAGE_PROPERTY_SEED":  func(value int64) { seed = value },
	} {
		raw := os.Getenv(name)
		if raw == "" {
			continue
		}
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || value <= 0 {
			t.Fatalf("%s = %q, want a positive integer", name, raw)
		}
		target(value)
	}
	return &quick.Config{MaxCount: cases, Rand: rand.New(rand.NewSource(seed))}
}

// repairSnapshotScenario is one message stored as several copies, each as a
// Google embedded snapshot leaves it: a fence, the reactors that snapshot
// listed active at the fence's sequence, and tombstones at or below it. Copy
// 0 is the survivor; Order is the order the other copies are merged in, and
// the first Split of them are merged by an earlier repair.
type repairSnapshotScenario struct {
	Copies []repairSnapshotCopy
	Order  []int
	Split  int
}

type repairSnapshotCopy struct {
	Fence     *int64        // nil: the copy never received a snapshot
	Reactions map[int]bool  // reactor -> active (else removed)
	RemovedAt map[int]int64 // removed reactor -> its source_seq_ms, at most Fence
	Emoji     map[int]string
}

func (repairSnapshotScenario) Generate(r *rand.Rand, _ int) reflect.Value {
	var scenario repairSnapshotScenario
	for range 2 + r.Intn(4) {
		snapshot := repairSnapshotCopy{Reactions: map[int]bool{}, RemovedAt: map[int]int64{}, Emoji: map[int]string{}}
		if r.Intn(5) != 0 {
			fence := int64(1 + r.Intn(3))
			snapshot.Fence = &fence
			for reactor := range 3 {
				switch r.Intn(3) {
				case 0:
					snapshot.Reactions[reactor] = true
				case 1:
					snapshot.Reactions[reactor] = false
					snapshot.RemovedAt[reactor] = 1 + int64(r.Intn(int(fence)))
				}
				snapshot.Emoji[reactor] = []string{"a", "b", "c"}[r.Intn(3)]
			}
		}
		scenario.Copies = append(scenario.Copies, snapshot)
	}
	scenario.Order = r.Perm(len(scenario.Copies) - 1)
	scenario.Split = r.Intn(len(scenario.Order) + 1)
	return reflect.ValueOf(scenario)
}

// Property (order independence): however many snapshot-fenced copies of one
// message are merged into a survivor, in whatever order and whether by one
// repair or two, the survivor ends with what the copies' snapshots say taken
// together. With F the largest fence, each reactor keeps its row with the
// largest (source_seq_ms, occurred_at_ms) among all copies, and a reactor
// still active below F is tombstoned at F, because no snapshot at F listed
// it. Across two repairs a removed reactor's row may be the first repair's
// inferred tombstone instead of the row that removed it, so then only its
// state is compared. So a reactor a
// snapshot at F lists stays active, unless another copy holds a tombstone for
// it written later at F. The expectation is computed from the original rows,
// not by replaying merges.
func TestApplyRepairPlanReactionMergeIsOrderIndependentProperty(t *testing.T) {
	coverage := map[string]int{}
	store, _ := openOutboxTestRepository(t, newOutboxTestClock(outboxTestTimeMS).Now)
	seedMessageIdentity(t, store, "identity-a", "account-a")
	seedMessageConversation(t, store, "conversation-a", "account-a")
	type row struct {
		State, Emoji              string
		SourceSeqMS, OccurredAtMS int64
	}
	property := func(scenario repairSnapshotScenario) bool {
		mustExec(t, store.db, `DELETE FROM messages`)
		want := map[string]row{}
		var mergedFence int64
		fenced := false
		occurredAtMS := outboxTestTimeMS - 900
		for index, snapshot := range scenario.Copies {
			messageID := repairPropertyMessageID(index)
			seedOutboxTestMessage(t, store, messageID, "account-a", "conversation-a")
			if snapshot.Fence == nil {
				continue
			}
			fenced, mergedFence = true, max(mergedFence, *snapshot.Fence)
			mustExec(t, store.db, `
				INSERT INTO reaction_snapshot_fences (message_id, source_seq_ms, updated_at_ms) VALUES (?, ?, ?)
			`, messageID, *snapshot.Fence, outboxTestTimeMS-700)
			for reactor, active := range snapshot.Reactions {
				// Every row gets its own write time, as snapshots do.
				occurredAtMS++
				seeded := row{State: "active", Emoji: snapshot.Emoji[reactor], SourceSeqMS: *snapshot.Fence, OccurredAtMS: occurredAtMS}
				if !active {
					seeded.State, seeded.SourceSeqMS = "removed", snapshot.RemovedAt[reactor]
				}
				reactorKey := "r" + strconv.Itoa(reactor)
				mustExec(t, store.db, `
					INSERT INTO reactions (
						message_id, reactor_key, account_id, conversation_id, reactor_identity_id,
						reactor_is_self, reactor_label, emoji, state, occurred_at_ms, source_seq_ms,
						created_at_ms, updated_at_ms
					) VALUES (?, ?, 'account-a', 'conversation-a', NULL, 0, 'peer', ?, ?, ?, ?, ?, ?)
				`, messageID, reactorKey, seeded.Emoji, seeded.State, seeded.OccurredAtMS, seeded.SourceSeqMS,
					outboxTestTimeMS-950, outboxTestTimeMS-950)
				if best, seen := want[reactorKey]; !seen || seeded.SourceSeqMS > best.SourceSeqMS ||
					(seeded.SourceSeqMS == best.SourceSeqMS && seeded.OccurredAtMS > best.OccurredAtMS) {
					want[reactorKey] = seeded
				}
			}
		}
		for reactorKey, best := range want {
			if best.State == "active" && best.SourceSeqMS < mergedFence {
				best.State, best.SourceSeqMS, best.OccurredAtMS = "removed", mergedFence, inferredTombstoneOccurredAtMS
				want[reactorKey] = best
				coverage["reactor tombstoned below the largest fence"]++
			} else if best.State == "active" {
				coverage["reactor active at the largest fence"]++
			}
		}

		survivorID := repairPropertyMessageID(0)
		steps := make([]RepairStep, 0, len(scenario.Order))
		for _, index := range scenario.Order {
			steps = append(steps, repairDeleteStep(repairPropertyMessageID(index+1), survivorID))
		}
		if len(steps) > 1 {
			coverage["several copies merged"]++
		}
		twoRepairs := scenario.Split > 0 && scenario.Split < len(steps)
		plans := [][]RepairStep{steps}
		if twoRepairs {
			plans = [][]RepairStep{steps[:scenario.Split], steps[scenario.Split:]}
			coverage["merged by two repairs"]++
		}
		for index, plan := range plans {
			// The second repair runs later than the first.
			if err := store.ApplyRepairPlan(context.Background(), "account-a", plan, repairMergeNowMS+int64(1000*index)); err != nil {
				t.Errorf("scenario %+v: ApplyRepairPlan(): %v", scenario, err)
				return false
			}
		}

		got := map[string]row{}
		rows, err := store.db.Query(`
			SELECT reactor_key, state, emoji, source_seq_ms, occurred_at_ms FROM reactions WHERE message_id = ?
		`, survivorID)
		if err != nil {
			t.Fatalf("read survivor reactions: %v", err)
		}
		defer rows.Close()
		for rows.Next() {
			var reactorKey string
			var read row
			if err := rows.Scan(&reactorKey, &read.State, &read.Emoji, &read.SourceSeqMS, &read.OccurredAtMS); err != nil {
				t.Fatalf("scan survivor reaction: %v", err)
			}
			got[reactorKey] = read
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("iterate survivor reactions: %v", err)
		}
		if twoRepairs {
			for _, reactions := range []map[string]row{got, want} {
				for reactorKey, reaction := range reactions {
					if reaction.State == "removed" {
						reactions[reactorKey] = row{State: "removed"}
					}
				}
			}
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("scenario %+v: survivor reactions\n got %+v\nwant %+v", scenario, got, want)
			return false
		}
		assertRowCount(t, store.db, "messages", 1)
		assertRowCount(t, store.db, "reactions", len(want))
		if fenced {
			assertReactionFence(t, store, survivorID, mergedFence)
		} else {
			assertRowCount(t, store.db, "reaction_snapshot_fences", 0)
		}
		return !t.Failed()
	}
	if err := quick.Check(property, repairPropertyConfig(t, 400, 120)); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"several copies merged", "merged by two repairs",
		"reactor tombstoned below the largest fence", "reactor active at the largest fence",
	} {
		if coverage[name] == 0 {
			t.Errorf("generator never produced %q; coverage = %v", name, coverage)
		}
	}
}
