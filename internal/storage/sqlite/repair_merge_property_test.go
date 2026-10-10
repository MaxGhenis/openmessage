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
	Bodies          [repairPropertyMessages]int // body variant per message; deletes need equal content
	Sends           map[int]bool                // message -> its send is still open (else confirmed)
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
		Sends:        map[int]bool{},
		FutureOutbox: r.Intn(4) == 0,
	}
	for index := range scenario.Conversations {
		scenario.Conversations[index] = r.Intn(len(repairPropertyConversations))
		if r.Intn(4) == 0 {
			scenario.Bodies[index] = 1
		}
		if r.Intn(6) == 0 {
			scenario.Sends[index] = r.Intn(2) == 0
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
	ConversationID, Body string
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
				if send.LocalMessageID == step.MessageID && send.State != "confirmed" &&
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
				if send.LocalMessageID == duplicate && send.State != "confirmed" {
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
			if survivorRow.Body != duplicateRow.Body {
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
			mergedFence, fenced := int64(0), false
			for _, messageID := range []string{duplicate, survivor} {
				if fence, ok := state.Fences[messageID]; ok {
					mergedFence, fenced = max(mergedFence, fence.SourceSeqMS), true
				}
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
			// The newest snapshot didn't list a reactor still active below
			// the merged fence, so it is tombstoned.
			if fenced {
				for key, reaction := range state.Reactions {
					if reaction.MessageID == survivor && reaction.State == "active" && reaction.SourceSeqMS < mergedFence {
						reaction.State = "removed"
						reaction.OccurredAtMS = nowMS
						reaction.SourceSeqMS = mergedFence
						reaction.UpdatedAtMS = max(reaction.UpdatedAtMS, nowMS)
						state.Reactions[key] = reaction
						coverage["reaction tombstoned below the merged fence"]++
					}
				}
			}
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
			if send.State == "confirmed" {
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
		mustExec(t, store.db, `UPDATE messages SET body = ? WHERE message_id = ?`,
			"body-"+strconv.Itoa(scenario.Bodies[message]), repairPropertyMessageID(message))
	}
	for message, open := range scenario.Sends {
		state := "confirmed"
		if open {
			state = "queued"
		}
		id := "plan-send-" + strconv.Itoa(message)
		mustExec(t, store.db, `
			INSERT INTO outbox (
				outbox_id, account_id, conversation_id, kind, idempotency_key, payload_hash, operation,
				state, local_message_id, transport_request_id, scheduled_for_ms, created_at_ms, updated_at_ms
			) VALUES (?, 'account-a', ?, 'text', ?, ?, 'send_text', ?, ?, ?, ?, ?, ?)
		`, "outbox-"+id, conversationOf(message), "idempotency-"+id, "hash-"+id, state,
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
	query(`SELECT message_id, conversation_id, body FROM messages`, func(rows *sql.Rows) error {
		var messageID string
		var row repairMessageRow
		err := rows.Scan(&messageID, &row.ConversationID, &row.Body)
		state.Messages[messageID] = row
		return err
	})
	query(`SELECT outbox_id, local_message_id, conversation_id, state FROM outbox WHERE kind = 'text'`, func(rows *sql.Rows) error {
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
