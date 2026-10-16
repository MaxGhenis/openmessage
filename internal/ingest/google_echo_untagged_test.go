package ingest

// Google echoes that carry no TmpID. On the live install the phone echoes
// every message sent from OpenMessage with an empty TmpID (raw inbox frames,
// 2026-10-16), so the TmpID correlation never ran and each send showed twice:
// the local row and the echo row. These tests drive the real service, worker
// and store.

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"reflect"
	"sort"
	"testing"
	"testing/quick"
	"time"

	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"

	"github.com/maxghenis/openmessage/internal/messaging"
)

// transportEchoOnly hides ObserveUncorrelatedEcho, standing in for a build
// that predates content correlation, so tests can create the duplicates the
// startup repair has to clean up.
type transportEchoOnly struct{ EchoObserver }

func preFixObserver(service *messaging.MessageService) EchoObserver {
	return transportEchoOnly{service}
}

func untaggedEcho(remoteID, body string, status gmproto.MessageStatusType, occurredAt time.Time) *gmproto.Message {
	message := googleEchoMessage(remoteID, "", body, true, occurredAt)
	message.MessageStatus = &gmproto.MessageStatus{Status: status}
	return message
}

func (h *googleEchoHarness) waitDrained(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if countSQLiteRows(t, h.storePath, "SELECT COUNT(*) FROM inbox WHERE processed_at_ms IS NULL") == 0 {
			return
		}
		select {
		case err := <-h.done:
			t.Fatalf("Worker.Run() stopped while draining: %v", err)
		default:
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("inbox not drained; counters=%+v", h.counters.Snapshot(googleEchoAccountID))
}

// sendAt submits and confirms a text with the service clock at at, so its
// outbox row is created then.
func (h *googleEchoHarness) sendAt(t *testing.T, key, body string, at time.Time) (messaging.Submission, string) {
	t.Helper()
	h.clock.Set(at)
	return h.enqueueAndConfirm(t, key, body)
}

// remoteIDs maps every message row in the test conversation to its remote ID.
func (h *googleEchoHarness) remoteIDs(t *testing.T) map[string]string {
	t.Helper()
	inspection, err := sql.Open("sqlite", h.storePath)
	if err != nil {
		t.Fatalf("sql.Open(inspect): %v", err)
	}
	defer inspection.Close()
	rows, err := inspection.Query(
		"SELECT message_id, remote_message_id FROM messages WHERE account_id = ? AND conversation_id = ?",
		googleEchoAccountID, googleEchoConversationID,
	)
	if err != nil {
		t.Fatalf("query messages: %v", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var messageID, remoteID string
		if err := rows.Scan(&messageID, &remoteID); err != nil {
			t.Fatalf("scan message: %v", err)
		}
		out[messageID] = remoteID
	}
	return out
}

func (h *googleEchoHarness) deliveryRemoteID(t *testing.T, outboxID string) string {
	t.Helper()
	delivery, err := h.service.Get(context.Background(), outboxID)
	if err != nil {
		t.Fatalf("MessageService.Get(): %v", err)
	}
	if delivery.State != messaging.OutboxConfirmed {
		t.Fatalf("delivery %q state = %q, want confirmed", outboxID, delivery.State)
	}
	return delivery.RemoteMessageID
}

func TestGoogleEchoWithoutTmpIDMergesIntoTheSentMessage(t *testing.T) {
	h := newGoogleEchoHarness(t, nil)
	submission, requestID := h.sendAt(t, "untagged", "Yes a few hours ago", googleEchoNow)

	// The live sequence: OUTGOING_COMPLETE about a second after the send,
	// then OUTGOING_DELIVERED for the same message ID.
	h.appendMessage(t, untaggedEcho("89087", "Yes a few hours ago", gmproto.MessageStatusType_OUTGOING_COMPLETE, googleEchoNow.Add(time.Second)))
	h.appendMessage(t, untaggedEcho("89087", "Yes a few hours ago", gmproto.MessageStatusType_OUTGOING_DELIVERED, googleEchoNow.Add(time.Second)))
	h.waitDrained(t)

	h.assertSinglePermanentMessage(t, submission.LocalMessageID, requestID, "89087")
	if got := h.deliveryRemoteID(t, submission.OutboxID); got != "89087" {
		t.Fatalf("delivery remote ID = %q, want the echo's", got)
	}
	if snapshot := h.counters.Snapshot(googleEchoAccountID); snapshot.EchoContentMatched != 1 || snapshot.EchoErrors != 0 {
		t.Fatalf("counters = %+v, want one content match and no errors", snapshot)
	}
}

// An echo that outruns the dispatcher's confirm projects as its own row (the
// intent is not sendable-confirmed yet); the next status frame for the same
// message merges it.
func TestGoogleEchoWithoutTmpIDBeforeConfirmConvergesOnTheNextStatusFrame(t *testing.T) {
	h := newGoogleEchoHarness(t, nil)
	submission, requestID := h.enqueue(t, "early", "echo outran confirm")
	h.appendMessage(t, untaggedEcho("90001", "echo outran confirm", gmproto.MessageStatusType_OUTGOING_COMPLETE, googleEchoNow.Add(time.Second)))
	h.waitDrained(t)
	if got := h.countV2Messages(t); got != 2 {
		t.Fatalf("messages before confirm = %d, want local plus echo", got)
	}

	h.confirm(t, submission.OutboxID)
	h.appendMessage(t, untaggedEcho("90001", "echo outran confirm", gmproto.MessageStatusType_OUTGOING_DELIVERED, googleEchoNow.Add(time.Second)))
	h.waitDrained(t)
	h.assertSinglePermanentMessage(t, submission.LocalMessageID, requestID, "90001")
}

func TestGoogleEchoWithoutTmpIDLeavesUnrelatedMessagesAlone(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		occurredAt time.Time
	}{
		{name: "different body", body: "something else", occurredAt: googleEchoNow.Add(time.Second)},
		{name: "stamped too long after the send", body: "hello", occurredAt: googleEchoNow.Add(31 * time.Minute)},
		{name: "stamped too long before the send", body: "hello", occurredAt: googleEchoNow.Add(-3 * time.Minute)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newGoogleEchoHarness(t, nil)
			submission, requestID := h.sendAt(t, "send", "hello", googleEchoNow)
			h.appendMessage(t, untaggedEcho("91000", tc.body, gmproto.MessageStatusType_OUTGOING_COMPLETE, tc.occurredAt))
			h.waitDrained(t)

			ids := h.remoteIDs(t)
			if len(ids) != 2 || ids[submission.LocalMessageID] != requestID {
				t.Fatalf("messages = %v, want the unbound send plus the phone's own message", ids)
			}
			if got := h.deliveryRemoteID(t, submission.OutboxID); got != requestID {
				t.Fatalf("delivery remote ID = %q, want it still on the request ID", got)
			}
			if snapshot := h.counters.Snapshot(googleEchoAccountID); snapshot.EchoContentMatched != 0 {
				t.Fatalf("counters = %+v, want no content match", snapshot)
			}
		})
	}
}

func TestGoogleEchoWithoutTmpIDFromThePhoneProjectsNormally(t *testing.T) {
	h := newGoogleEchoHarness(t, nil)
	h.appendMessage(t, untaggedEcho("92000", "typed on the phone", gmproto.MessageStatusType_OUTGOING_COMPLETE, googleEchoNow))
	h.waitDrained(t)
	if ids := h.remoteIDs(t); len(ids) != 1 {
		t.Fatalf("messages = %v, want the one phone message", ids)
	}
	if snapshot := h.counters.Snapshot(googleEchoAccountID); snapshot.EchoContentMatched != 0 || snapshot.EchoErrors != 0 {
		t.Fatalf("counters = %+v", snapshot)
	}
}

func TestGoogleEchoWithoutTmpIDPairsRepeatedTextsOldestFirst(t *testing.T) {
	h := newGoogleEchoHarness(t, nil)
	first, _ := h.sendAt(t, "first", "ok", googleEchoNow)
	second, _ := h.sendAt(t, "second", "ok", googleEchoNow.Add(5*time.Second))
	// Each echo comes back once per status. The first message's DELIVERED
	// frame lands before the second message's first echo and must not claim
	// the second send, which would merge the two messages into one.
	h.appendMessage(t, untaggedEcho("93001", "ok", gmproto.MessageStatusType_OUTGOING_COMPLETE, googleEchoNow.Add(time.Second)))
	h.appendMessage(t, untaggedEcho("93001", "ok", gmproto.MessageStatusType_OUTGOING_DELIVERED, googleEchoNow.Add(time.Second)))
	h.appendMessage(t, untaggedEcho("93002", "ok", gmproto.MessageStatusType_OUTGOING_COMPLETE, googleEchoNow.Add(6*time.Second)))
	h.appendMessage(t, untaggedEcho("93002", "ok", gmproto.MessageStatusType_OUTGOING_DELIVERED, googleEchoNow.Add(6*time.Second)))
	h.waitDrained(t)

	ids := h.remoteIDs(t)
	if len(ids) != 2 || ids[first.LocalMessageID] != "93001" || ids[second.LocalMessageID] != "93002" {
		t.Fatalf("messages = %v, want first->93001 and second->93002", ids)
	}
}

func TestRepairUncorrelatedEchoDuplicatesMergesPreFixDuplicates(t *testing.T) {
	h := newGoogleEchoHarnessWithObserver(t, nil, preFixObserver)
	a1, _ := h.sendAt(t, "a1", "same text", googleEchoNow)
	b, _ := h.sendAt(t, "b", "other text", googleEchoNow.Add(10*time.Second))
	a2, _ := h.sendAt(t, "a2", "same text", googleEchoNow.Add(20*time.Second))
	for _, echo := range []*gmproto.Message{
		untaggedEcho("94001", "same text", gmproto.MessageStatusType_OUTGOING_DELIVERED, googleEchoNow.Add(time.Second)),
		untaggedEcho("94002", "other text", gmproto.MessageStatusType_OUTGOING_DELIVERED, googleEchoNow.Add(11*time.Second)),
		untaggedEcho("94003", "same text", gmproto.MessageStatusType_OUTGOING_DELIVERED, googleEchoNow.Add(21*time.Second)),
		// The same words typed on the phone an hour later stay their own message.
		untaggedEcho("94004", "same text", gmproto.MessageStatusType_OUTGOING_DELIVERED, googleEchoNow.Add(time.Hour)),
	} {
		h.appendMessage(t, echo)
	}
	h.waitDrained(t)
	if got := h.countV2Messages(t); got != 7 {
		t.Fatalf("messages before repair = %d, want 3 sends + 3 echo duplicates + 1 phone message", got)
	}

	report, err := h.service.RepairUncorrelatedEchoDuplicates(context.Background())
	if err != nil {
		t.Fatalf("RepairUncorrelatedEchoDuplicates(): %v", err)
	}
	if report.Unbound != 3 || report.Merged != 3 {
		t.Fatalf("report = %+v, want 3 unbound, 3 merged", report)
	}
	ids := h.remoteIDs(t)
	want := map[string]string{a1.LocalMessageID: "94001", b.LocalMessageID: "94002", a2.LocalMessageID: "94003"}
	if len(ids) != 4 {
		t.Fatalf("messages after repair = %v, want 3 sends + the phone message", ids)
	}
	for local, remote := range want {
		if ids[local] != remote {
			t.Fatalf("messages after repair = %v, want %s->%s", ids, local, remote)
		}
	}
	for _, submission := range []messaging.Submission{a1, b, a2} {
		if got := h.deliveryRemoteID(t, submission.OutboxID); got != want[submission.LocalMessageID] {
			t.Fatalf("delivery %s remote = %q, want %q", submission.OutboxID, got, want[submission.LocalMessageID])
		}
	}

	again, err := h.service.RepairUncorrelatedEchoDuplicates(context.Background())
	if err != nil || again.Unbound != 0 || again.Merged != 0 {
		t.Fatalf("second repair = %+v, %v; want nothing left to do", again, err)
	}
}

// echoScenario is a random interleaving of sends from OpenMessage and texts
// typed on the phone, every one echoed with no TmpID, all inside the match
// window and arriving in timestamp order.
type echoScenario struct {
	events []echoScenarioEvent
}

type echoScenarioEvent struct {
	fromApp bool
	body    string
	at      time.Duration // after googleEchoNow
}

func (echoScenario) Generate(r *rand.Rand, _ int) reflect.Value {
	n := 1 + r.Intn(6)
	var scenario echoScenario
	at := time.Duration(0)
	for i := 0; i < n; i++ {
		at += time.Duration(1+r.Intn(20)) * time.Second
		scenario.events = append(scenario.events, echoScenarioEvent{
			fromApp: r.Intn(3) != 0,
			body:    []string{"ok", "on my way"}[r.Intn(2)],
			at:      at,
		})
	}
	return reflect.ValueOf(scenario)
}

// runEchoScenario plays the scenario through either the live correlation or
// a pre-fix build followed by the startup repair, and returns each send's
// final remote ID plus the total message count.
func runEchoScenario(t *testing.T, scenario echoScenario, viaRepair bool) (map[int]string, int) {
	t.Helper()
	var h *googleEchoHarness
	if viaRepair {
		h = newGoogleEchoHarnessWithObserver(t, nil, preFixObserver)
	} else {
		h = newGoogleEchoHarness(t, nil)
	}
	sends := map[int]messaging.Submission{}
	for i, event := range scenario.events {
		if event.fromApp {
			submission, _ := h.sendAt(t, fmt.Sprintf("send-%d", i), event.body, googleEchoNow.Add(event.at))
			sends[i] = submission
		}
	}
	for i, event := range scenario.events {
		// An app send's echo is stamped a moment after the send.
		h.appendMessage(t, untaggedEcho(fmt.Sprintf("95%03d", i), event.body, gmproto.MessageStatusType_OUTGOING_COMPLETE, googleEchoNow.Add(event.at+500*time.Millisecond)))
	}
	h.waitDrained(t)
	if viaRepair {
		if _, err := h.service.RepairUncorrelatedEchoDuplicates(context.Background()); err != nil {
			t.Fatalf("RepairUncorrelatedEchoDuplicates(): %v", err)
		}
	}
	ids := h.remoteIDs(t)
	bound := map[int]string{}
	for i, submission := range sends {
		bound[i] = ids[submission.LocalMessageID]
	}
	return bound, len(ids)
}

// TestUncorrelatedEchoLiveAndRepairAgreeProperty: for any scenario,
//   - every message, sent from the app or typed on the phone, is exactly one row;
//   - every app send ends bound to a real remote ID, and no remote ID binds twice;
//   - the live correlation and the pre-fix-plus-repair path reach the same pairing.
func TestUncorrelatedEchoLiveAndRepairAgreeProperty(t *testing.T) {
	property := func(scenario echoScenario) bool {
		live, liveRows := runEchoScenario(t, scenario, false)
		repaired, repairedRows := runEchoScenario(t, scenario, true)
		if liveRows != len(scenario.events) || repairedRows != len(scenario.events) {
			t.Logf("rows live=%d repaired=%d, want %d", liveRows, repairedRows, len(scenario.events))
			return false
		}
		seen := map[string]bool{}
		for _, i := range sortedKeys(live) {
			remote := live[i]
			if len(remote) != 5 || seen[remote] {
				t.Logf("send %d bound to %q (seen=%v)", i, remote, seen[remote])
				return false
			}
			seen[remote] = true
			if repaired[i] != remote {
				t.Logf("send %d: live %q, repaired %q", i, remote, repaired[i])
				return false
			}
		}
		return true
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 25}); err != nil {
		t.Fatal(err)
	}
}

func sortedKeys(m map[int]string) []int {
	keys := make([]int, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	return keys
}
