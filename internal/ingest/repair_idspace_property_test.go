package ingest

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"testing/quick"
	"time"

	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

// Property tests for the id-space repair over random damage: re-served
// duplicates, misfiled rows, and remote ids that collide across the two
// device id spaces, with reactions, snapshot fences, attachments, intents and
// read cursors scattered over every row.

var (
	repairPropertyPeers  = []string{"+15550000001", "+15550000002", "+15550000003"}
	repairPropertyBodies = []string{"hi", "lunch?", "see you"}
)

// repairRowSeed is one stored message. Conversation 0-2 is peer i's thread,
// 3 a peerless thread minted in the window. Sender -1 is outgoing.
type repairRowSeed struct {
	Conversation, Sender, Body, Occurred, Remote int
}

type repairDamageScenario struct {
	Older, Window   []repairRowSeed
	Reactions       map[[2]int][2]int64 // (row, reactor) -> (occurred offset, source seq)
	Fences          map[int]int64
	Attachments     map[int]bool // row -> downloaded
	ReactionIntents []int
	ReceiptIntents  []int
	Cursors         map[int]int // conversation -> row
}

func (repairDamageScenario) Generate(r *rand.Rand, _ int) reflect.Value {
	scenario := repairDamageScenario{
		Reactions:   map[[2]int][2]int64{},
		Fences:      map[int]int64{},
		Attachments: map[int]bool{},
		Cursors:     map[int]int{},
	}
	// Google message ids are the phone's row ids. Older rows come from the
	// old phone and window rows from the new one, so an id is unique within
	// each space, and both draw from one small range so that they collide
	// across spaces, as re-keyed ids do. Within one conversation the stored
	// natural key keeps ids unique across spaces too.
	used := map[[2]int]bool{} // (space, remote) and (-1-conversation, remote)
	remote := func(space, conversation int) int {
		free := func(candidate int) bool {
			if used[[2]int{space, candidate}] || used[[2]int{-1 - conversation, candidate}] {
				return false
			}
			used[[2]int{space, candidate}] = true
			used[[2]int{-1 - conversation, candidate}] = true
			return true
		}
		for range 8 {
			if candidate := 1 + r.Intn(6); free(candidate) {
				return candidate
			}
		}
		for candidate := 100; ; candidate++ {
			if free(candidate) {
				return candidate
			}
		}
	}
	for conversation := range repairPropertyPeers {
		for range 1 + r.Intn(2) {
			sender := conversation
			if r.Intn(4) == 0 {
				sender = -1
			}
			scenario.Older = append(scenario.Older, repairRowSeed{
				Conversation: conversation, Sender: sender, Body: r.Intn(len(repairPropertyBodies)),
				Occurred: r.Intn(3), Remote: remote(0, conversation),
			})
		}
	}
	for range 1 + r.Intn(6) {
		conversation := r.Intn(4)
		row := repairRowSeed{Conversation: conversation, Body: r.Intn(len(repairPropertyBodies)), Occurred: r.Intn(3)}
		switch roll := r.Intn(10); {
		case roll < 6 && conversation < 3:
			row.Sender = conversation
		case roll < 9:
			row.Sender = r.Intn(len(repairPropertyPeers))
		default:
			row.Sender = -1
		}
		if r.Intn(2) == 0 {
			// A re-served copy of a stored message.
			all := append(append([]repairRowSeed{}, scenario.Older...), scenario.Window...)
			source := all[r.Intn(len(all))]
			row.Sender, row.Body, row.Occurred = source.Sender, source.Body, source.Occurred
		}
		row.Remote = remote(1, conversation)
		scenario.Window = append(scenario.Window, row)
	}
	rows := len(scenario.Older) + len(scenario.Window)
	for row := range rows {
		for reactor := range 2 {
			if r.Intn(4) == 0 {
				scenario.Reactions[[2]int{row, reactor}] = [2]int64{int64(r.Intn(3)), int64(r.Intn(3))}
			}
		}
		if r.Intn(5) == 0 {
			scenario.Fences[row] = int64(r.Intn(4))
		}
		if r.Intn(5) == 0 {
			scenario.Attachments[row] = r.Intn(2) == 0
		}
	}
	for range r.Intn(4) {
		scenario.ReactionIntents = append(scenario.ReactionIntents, r.Intn(rows))
	}
	for range r.Intn(3) {
		scenario.ReceiptIntents = append(scenario.ReceiptIntents, r.Intn(rows))
	}
	all := append(append([]repairRowSeed{}, scenario.Older...), scenario.Window...)
	for conversation := range 4 {
		if r.Intn(3) != 0 {
			continue
		}
		var members []int
		for index, row := range all {
			if row.Conversation == conversation {
				members = append(members, index)
			}
		}
		if len(members) > 0 {
			scenario.Cursors[conversation] = members[r.Intn(len(members))]
		}
	}
	return reflect.ValueOf(scenario)
}

// repairDamageState is every row the repair may rewrite.
type repairDamageState struct {
	Messages    map[string]repairMessageRow
	Reactions   map[[2]string][3]any // (message, reactor) -> (occurred, seq, conversation)
	Fences      map[string]int64
	Attachments map[string]string // message -> state (ordinal 0)
	Intents     map[string][2]string
	Cursors     map[string]string // conversation -> message
}

type repairMessageRow struct {
	ConversationID, Content string
}

func readRepairDamageState(t *testing.T, path string) repairDamageState {
	t.Helper()
	database := i01OpenInspector(t, path)
	defer database.Close()
	state := repairDamageState{
		Messages:    map[string]repairMessageRow{},
		Reactions:   map[[2]string][3]any{},
		Fences:      map[string]int64{},
		Attachments: map[string]string{},
		Intents:     map[string][2]string{},
		Cursors:     map[string]string{},
	}
	query := func(statement string, each func(*sql.Rows) error) {
		rows, err := database.Query(statement)
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
	query(`SELECT message_id, conversation_id, direction || '|' || COALESCE(sender_identity_id, '') || '|' ||
			occurred_at_ms || '|' || body FROM messages`, func(rows *sql.Rows) error {
		var id string
		var row repairMessageRow
		err := rows.Scan(&id, &row.ConversationID, &row.Content)
		state.Messages[id] = row
		return err
	})
	query(`SELECT message_id, reactor_key, occurred_at_ms, source_seq_ms, conversation_id FROM reactions`, func(rows *sql.Rows) error {
		var message, reactor, conversation string
		var occurred, seq int64
		err := rows.Scan(&message, &reactor, &occurred, &seq, &conversation)
		state.Reactions[[2]string{message, reactor}] = [3]any{occurred, seq, conversation}
		return err
	})
	query(`SELECT message_id, source_seq_ms FROM reaction_snapshot_fences`, func(rows *sql.Rows) error {
		var message string
		var seq int64
		err := rows.Scan(&message, &seq)
		state.Fences[message] = seq
		return err
	})
	query(`SELECT message_id, state FROM message_attachments`, func(rows *sql.Rows) error {
		var message, attachmentState string
		err := rows.Scan(&message, &attachmentState)
		state.Attachments[message] = attachmentState
		return err
	})
	query(`SELECT o.outbox_id, COALESCE(r.target_message_id, p.last_read_message_id), o.conversation_id
		FROM outbox AS o
		LEFT JOIN outbox_reactions AS r ON r.outbox_id = o.outbox_id
		LEFT JOIN outbox_read_receipts AS p ON p.outbox_id = o.outbox_id`, func(rows *sql.Rows) error {
		var outboxID, target, conversation string
		err := rows.Scan(&outboxID, &target, &conversation)
		state.Intents[outboxID] = [2]string{target, conversation}
		return err
	})
	query(`SELECT conversation_id, last_read_message_id FROM read_cursors`, func(rows *sql.Rows) error {
		var conversation, message string
		err := rows.Scan(&conversation, &message)
		state.Cursors[conversation] = message
		return err
	})
	return state
}

// resetRepairDamage clears what a scenario seeds and a repair writes, keeping
// the account, identities and device. The inspector connection doesn't
// enforce foreign keys, so each table is cleared explicitly.
func resetRepairDamage(t *testing.T, path string) {
	t.Helper()
	database := i01OpenInspector(t, path)
	defer database.Close()
	for _, table := range []string{
		"outbox_reactions", "outbox_read_receipts", "outbox", "read_cursors", "reactions",
		"reaction_snapshot_fences", "message_attachments", "messages", "conversation_participants", "conversations",
	} {
		if _, err := database.Exec("DELETE FROM " + table); err != nil {
			t.Fatalf("reset %s: %v", table, err)
		}
	}
}

func seedRepairDamage(t *testing.T, fixture repairMergeFixture, scenario repairDamageScenario) {
	t.Helper()
	harness := fixture.harness
	before := repairWindowMS - 10_000_000
	inWindow := repairWindowMS + 5_000
	self := repairSeedIdentity(t, harness, idsSelfNumber, "Max Ghenis", true)
	peers := make([]sqlite.Identity, len(repairPropertyPeers))
	threads := make([]sqlite.Conversation, 0, 4)
	for index, number := range repairPropertyPeers {
		peers[index] = repairSeedIdentity(t, harness, number, "", false)
		threads = append(threads, repairSeedConversation(t, harness, strconv.Itoa(1000+index), "peer "+strconv.Itoa(index),
			sqlite.ConversationKindDirect, before, peers[index], self))
	}
	threads = append(threads, repairSeedConversation(t, harness, "2000", "", sqlite.ConversationKindDirect, inWindow))

	rows := make([]sqlite.Message, 0, len(scenario.Older)+len(scenario.Window))
	seed := func(row repairRowSeed, createdMS int64) {
		var sender *sqlite.Identity
		if row.Sender >= 0 {
			sender = &peers[row.Sender]
		}
		occurredMS := before - 5_000_000 + int64(row.Occurred)*1_000
		rows = append(rows, repairSeedMessage(t, harness, threads[row.Conversation], strconv.Itoa(row.Remote), sender,
			repairPropertyBodies[row.Body], occurredMS, createdMS))
	}
	for index, row := range scenario.Older {
		seed(row, before+int64(index))
	}
	for index, row := range scenario.Window {
		seed(row, inWindow+int64(index))
	}

	database := i01OpenInspector(t, harness.path)
	defer database.Close()
	exec := func(statement string, arguments ...any) {
		if _, err := database.Exec(statement, arguments...); err != nil {
			t.Fatalf("seed (%s): %v", statement, err)
		}
	}
	nowMS := i01TestTime.UnixMilli()
	for key, value := range scenario.Reactions {
		message := rows[key[0]]
		exec(`INSERT INTO reactions (
				message_id, reactor_key, account_id, conversation_id, reactor_identity_id, reactor_is_self,
				reactor_label, emoji, state, occurred_at_ms, source_seq_ms, created_at_ms, updated_at_ms
			) VALUES (?, ?, ?, ?, NULL, 0, 'peer', '👍', 'active', ?, ?, ?, ?)`,
			message.MessageID, "reactor-"+strconv.Itoa(key[1]), i01AccountID, message.ConversationID,
			nowMS+value[0], value[1], nowMS, nowMS)
	}
	for row, seq := range scenario.Fences {
		exec(`INSERT INTO reaction_snapshot_fences (message_id, source_seq_ms, updated_at_ms) VALUES (?, ?, ?)`,
			rows[row].MessageID, seq, nowMS)
	}
	for row, downloaded := range scenario.Attachments {
		state, hash := "pending", any(nil)
		if downloaded {
			state, hash = "downloaded", strings.Repeat("ab", 32)
		}
		exec(`INSERT INTO message_attachments (
				message_id, ordinal, remote_id, remote_ref, filename, mime, size_bytes,
				state, blob_hash, last_error, created_at_ms, updated_at_ms
			) VALUES (?, 0, 'media', x'', 'photo.png', 'image/png', 42, ?, ?, NULL, ?, ?)`,
			rows[row].MessageID, state, hash, nowMS, nowMS)
	}
	for index, row := range scenario.ReactionIntents {
		fixture.enqueueReaction(t, "property-react-"+strconv.Itoa(index), rows[row])
	}
	for index, row := range scenario.ReceiptIntents {
		fixture.enqueueReadReceipt(t, "property-read-"+strconv.Itoa(index), rows[row], nowMS+int64(index))
	}
	// Enqueueing a receipt moved a cursor; the scenario's cursors replace them.
	exec(`DELETE FROM read_cursors`)
	for _, row := range scenario.Cursors {
		fixture.setCursor(t, rows[row], nowMS)
	}
}

// Property (repair invariants): for any damage, the plan applies; afterwards
// foreign_key_check is clean, and with root(m) the survivor a delete step
// names for m (m itself when kept):
//   - a delete step's survivor is never deleted, and holds the deleted row's
//     exact content; kept rows keep their content and only deleted rows go;
//   - every intent targets root(its old target), inside its own conversation;
//   - reactions group by (root, reactor): one row each, carrying the group's
//     latest (occurred_at_ms, source_seq_ms), in root's conversation;
//   - fences keep the group's largest source_seq_ms; attachments one row per
//     root, downloaded if any in the group was;
//   - every cursor survives, on root(its old message);
//   - no conversation holds two copies of a window row's content;
//   - planning again finds nothing to do unless the first plan left a row in
//     place.
func TestGoogleIDSpaceRepairInvariantsProperty(t *testing.T) {
	coverage := map[string]int{}
	ctx := context.Background()
	fixture := newRepairMergeFixture(t)
	harness := fixture.harness
	property := func(scenario repairDamageScenario) bool {
		resetRepairDamage(t, harness.path)
		seedRepairDamage(t, fixture, scenario)
		before := readRepairDamageState(t, harness.path)
		windowRows := map[string]bool{}
		for _, row := range harness.mustListWindowRows(t) {
			windowRows[row] = true
		}
		options := IDSpaceRepairOptions{AccountID: i01AccountID, SinceMS: repairWindowMS, Now: func() time.Time { return i01TestTime }}
		report, err := PlanGoogleIDSpaceRepair(ctx, harness.store, harness.messages, options)
		if err != nil {
			t.Errorf("scenario %+v: plan: %v", scenario, err)
			return false
		}
		if err := ApplyGoogleIDSpaceRepair(ctx, harness.store, report, i01TestTime); err != nil {
			t.Errorf("scenario %+v: apply: %v\nplan %+v", scenario, err, report.Steps)
			return false
		}
		after := readRepairDamageState(t, harness.path)
		fixture.assertForeignKeysClean(t)
		fail := func(format string, arguments ...any) bool {
			t.Errorf("scenario %+v: %s\nsteps %+v", scenario, fmt.Sprintf(format, arguments...), report.Steps)
			return false
		}

		survivorOf := map[string]string{}
		for _, step := range report.Steps {
			if step.Op != "delete" {
				continue
			}
			survivorOf[step.MessageID] = step.SurvivorMessageID
			coverage["delete"]++
			// "duplicate of <id>..." names the row the planner matched; the
			// step's survivor differs when that row is itself deleted.
			if fields := strings.Fields(step.Reason); len(fields) > 2 && fields[2] != "content" &&
				strings.TrimSuffix(fields[2], ",") != step.SurvivorMessageID {
				coverage["delete through a survivor chain"]++
			}
			if strings.Contains(step.Reason, "same remote id") {
				coverage["remote id taken: same content"]++
			}
			if before.Messages[step.MessageID].ConversationID != before.Messages[step.SurvivorMessageID].ConversationID {
				coverage["delete across conversations"]++
			}
		}
		for _, group := range report.Groups {
			if strings.Contains(group.Detail, "for other content") {
				coverage["remote id taken: other content"]++
			}
		}
		root := func(messageID string) string {
			if survivor, deleted := survivorOf[messageID]; deleted {
				return survivor
			}
			return messageID
		}

		for messageID, row := range before.Messages {
			survivor, deleted := survivorOf[messageID]
			kept, exists := after.Messages[messageID]
			switch {
			case deleted && exists:
				return fail("deleted message %s still exists", messageID)
			case deleted:
				if _, alsoDeleted := survivorOf[survivor]; alsoDeleted {
					return fail("survivor %s of %s is itself deleted", survivor, messageID)
				}
				if after.Messages[survivor].Content != row.Content {
					return fail("survivor %s holds %q, deleted %s held %q", survivor, after.Messages[survivor].Content, messageID, row.Content)
				}
			case !exists:
				return fail("message %s vanished without a delete step", messageID)
			case kept.Content != row.Content:
				return fail("message %s content changed %q -> %q", messageID, row.Content, kept.Content)
			}
		}
		if len(after.Messages) != len(before.Messages)-len(survivorOf) {
			return fail("messages %d -> %d with %d deletes", len(before.Messages), len(after.Messages), len(survivorOf))
		}

		if len(after.Intents) != len(before.Intents) {
			return fail("intents %d -> %d", len(before.Intents), len(after.Intents))
		}
		for outboxID, intent := range before.Intents {
			got := after.Intents[outboxID]
			if got[0] != root(intent[0]) || got[1] != after.Messages[got[0]].ConversationID {
				return fail("intent %s = %v, want target %s in its conversation", outboxID, got, root(intent[0]))
			}
			if got[1] != intent[1] {
				coverage["intent changed conversation"]++
			}
			if root(intent[0]) != intent[0] {
				coverage["intent on a deleted duplicate"]++
			}
		}

		wantReactions := map[[2]string][3]any{}
		for key, value := range before.Reactions {
			grouped := [2]string{root(key[0]), key[1]}
			current, seen := wantReactions[grouped]
			if seen {
				coverage["reaction groups merged"]++
			}
			if !seen || value[0].(int64) > current[0].(int64) ||
				(value[0].(int64) == current[0].(int64) && value[1].(int64) > current[1].(int64)) {
				current = value
			}
			current[2] = after.Messages[grouped[0]].ConversationID
			wantReactions[grouped] = current
		}
		if !reflect.DeepEqual(after.Reactions, wantReactions) {
			return fail("reactions\n got %v\nwant %v", after.Reactions, wantReactions)
		}
		wantFences := map[string]int64{}
		for message, seq := range before.Fences {
			if current, seen := wantFences[root(message)]; !seen || seq > current {
				wantFences[root(message)] = seq
			}
		}
		if !reflect.DeepEqual(after.Fences, wantFences) {
			return fail("fences\n got %v\nwant %v", after.Fences, wantFences)
		}
		wantAttachments := map[string]string{}
		for message, attachmentState := range before.Attachments {
			if wantAttachments[root(message)] != "downloaded" {
				wantAttachments[root(message)] = attachmentState
			}
		}
		if !reflect.DeepEqual(after.Attachments, wantAttachments) {
			return fail("attachments\n got %v\nwant %v", after.Attachments, wantAttachments)
		}
		wantCursors := map[string]string{}
		for conversation, message := range before.Cursors {
			wantCursors[conversation] = root(message)
			if root(message) != message {
				coverage["cursor on a deleted duplicate"]++
			}
		}
		if !reflect.DeepEqual(after.Cursors, wantCursors) {
			return fail("cursors\n got %v\nwant %v", after.Cursors, wantCursors)
		}

		again, err := PlanGoogleIDSpaceRepair(ctx, harness.store, harness.messages, options)
		if err != nil {
			return fail("second plan: %v", err)
		}
		// Duplicates are merged on the first pass: no conversation still
		// holds two copies of a window row's content.
		copies := map[[2]string]string{}
		for messageID, row := range after.Messages {
			key := [2]string{row.ConversationID, row.Content}
			if strings.HasSuffix(row.Content, "|") {
				continue // blank body: the repair never matches on content
			}
			if other, seen := copies[key]; seen && (windowRows[messageID] || windowRows[other]) {
				return fail("conversation %s still holds %s and %s with content %q", row.ConversationID, other, messageID, row.Content)
			}
			copies[key] = messageID
		}
		// A row left in place (a cursor names it, or the target holds its
		// remote id) can leave its thread with senders that disagree, so the
		// next plan may route the thread again. Otherwise it finds nothing.
		leftInPlace := false
		for _, group := range report.Groups {
			leftInPlace = leftInPlace || strings.Contains(group.Detail, "left message")
		}
		if !leftInPlace && again.Moves+again.Deletes != 0 {
			return fail("second plan moves %d and deletes %d: %+v", again.Moves, again.Deletes, again.Steps)
		}
		coverage["plan applied"]++
		return true
	}
	cases := 200
	if raceDetectorEnabled {
		cases = 40
	}
	if err := quick.Check(property, &quick.Config{MaxCount: cases, Rand: rand.New(rand.NewSource(20261010))}); err != nil {
		t.Fatal(err)
	}
	t.Logf("coverage: %v", coverage)
	for _, name := range []string{
		// Survivor chains need three rows to line up; TestGoogleIDSpaceRepairResolvesSurvivorChains
		// covers them deterministically.
		"plan applied", "delete", "delete across conversations",
		"remote id taken: same content", "remote id taken: other content",
		"intent changed conversation", "intent on a deleted duplicate", "reaction groups merged",
		"cursor on a deleted duplicate",
	} {
		if coverage[name] == 0 {
			t.Errorf("generator never produced %q; coverage = %v", name, coverage)
		}
	}
}

func (h *i01Harness) mustListWindowRows(t *testing.T) []string {
	t.Helper()
	rows, err := h.store.ListMessagesCreatedSince(i01AccountID, repairWindowMS)
	if err != nil {
		t.Fatalf("ListMessagesCreatedSince(): %v", err)
	}
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.MessageID)
	}
	return ids
}
