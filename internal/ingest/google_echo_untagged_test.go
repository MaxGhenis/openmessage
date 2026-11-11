package ingest

// Google echoes that carry no TmpID. On the live install the phone echoes
// every message sent from OpenMessage with an empty TmpID (raw inbox frames,
// 2026-10-16), so the TmpID correlation never ran and each send showed twice:
// the local row and the echo row. These tests drive the real service, worker
// and store, and check every pairing against the ground truth: each send must
// end bound to its own echo, and every text typed on the phone stays its own
// message.

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"reflect"
	"testing"
	"testing/quick"
	"time"

	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"

	"github.com/maxghenis/openmessage/internal/messaging"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
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

// phoneClockOffset puts the phone's clock well off this machine's, so no
// test can pass by pairing on the phone's stamp instead of the receipt time.
// The odd milliseconds keep a phone stamp from landing exactly on a local
// send's timestamp, which the store's same-millisecond content dedupe would
// (rightly) treat as one message.
const phoneClockOffset = -45123 * time.Millisecond

// echo delivers an outgoing frame with no TmpID that this machine receives
// at arrivedAt (the clock only moves forward). A reconcile error fails the
// test: a test must never pass because the matcher errored out.
func (h *googleEchoHarness) echo(t *testing.T, remoteID, body string, status gmproto.MessageStatusType, arrivedAt time.Time) {
	t.Helper()
	if arrivedAt.Before(h.clock.Now()) {
		t.Fatalf("echo arrives at %v, before the clock's %v", arrivedAt, h.clock.Now())
	}
	h.clock.Set(arrivedAt)
	h.appendMessage(t, untaggedEcho(remoteID, body, status, arrivedAt.Add(phoneClockOffset)))
	h.waitDrained(t)
	if errors := h.counters.Snapshot(googleEchoAccountID).EchoErrors; errors != 0 {
		t.Fatalf("echo reconcile failed %d times", errors)
	}
}

// sendAt submits a text at createdAt and confirms it at sentAt (the moment
// the transport answered; equal unless the send was retried or scheduled).
func (h *googleEchoHarness) sendAt(t *testing.T, key, body string, createdAt, sentAt time.Time) messaging.Submission {
	t.Helper()
	h.clock.Set(createdAt)
	submission, _ := h.enqueue(t, key, body)
	h.clock.Set(sentAt)
	h.confirm(t, submission.OutboxID)
	return submission
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

// occurredAt returns the stored timestamp of the row holding remoteID.
func (h *googleEchoHarness) occurredAt(t *testing.T, remoteID string) int64 {
	t.Helper()
	return int64(countSQLiteRows(t, h.storePath,
		"SELECT occurred_at_ms FROM messages WHERE account_id = ? AND conversation_id = ? AND remote_message_id = ?",
		googleEchoAccountID, googleEchoConversationID, remoteID))
}

func (h *googleEchoHarness) delivery(t *testing.T, outboxID string) messaging.Delivery {
	t.Helper()
	delivery, err := h.service.Get(context.Background(), outboxID)
	if err != nil {
		t.Fatalf("MessageService.Get(): %v", err)
	}
	return delivery
}

func (h *googleEchoHarness) assertBound(t *testing.T, submission messaging.Submission, remoteID string) {
	t.Helper()
	ids := h.remoteIDs(t)
	if ids[submission.LocalMessageID] != remoteID {
		t.Fatalf("send %s holds %q, want its echo %q (rows %v)", submission.LocalMessageID, ids[submission.LocalMessageID], remoteID, ids)
	}
	if delivery := h.delivery(t, submission.OutboxID); delivery.State != messaging.OutboxConfirmed || delivery.RemoteMessageID != remoteID {
		t.Fatalf("delivery = %+v, want confirmed at %q", delivery, remoteID)
	}
}

const (
	complete  = gmproto.MessageStatusType_OUTGOING_COMPLETE
	delivered = gmproto.MessageStatusType_OUTGOING_DELIVERED
)

func TestGoogleEchoWithoutTmpIDMergesIntoTheSentMessage(t *testing.T) {
	h := newGoogleEchoHarness(t, nil)
	send := h.sendAt(t, "untagged", "Yes a few hours ago", googleEchoNow, googleEchoNow.Add(time.Second))
	// The live sequence: OUTGOING_COMPLETE received 0.8 s after the confirm,
	// then OUTGOING_DELIVERED for the same message ID.
	h.echo(t, "89087", "Yes a few hours ago", complete, googleEchoNow.Add(1800*time.Millisecond))
	h.echo(t, "89087", "Yes a few hours ago", delivered, googleEchoNow.Add(3*time.Second))

	h.assertBound(t, send, "89087")
	if got := h.countV2Messages(t); got != 1 {
		t.Fatalf("messages = %d, want one", got)
	}
	if snapshot := h.counters.Snapshot(googleEchoAccountID); snapshot.EchoContentMatched != 1 || snapshot.EchoErrors != 0 {
		t.Fatalf("counters = %+v, want one content match and no errors", snapshot)
	}
}

// Reviewer R2: every echo frame outruns the confirm, so the echo projects as
// its own row. The next frame with that body, even a text typed on the
// phone, must pair the send with its real echo row, not with itself.
func TestGoogleEchoBeforeConfirmMergesTheRealEchoNotALaterPhoneText(t *testing.T) {
	h := newGoogleEchoHarness(t, nil)
	submission, _ := h.enqueue(t, "early", "ok")
	h.echo(t, "97001", "ok", complete, googleEchoNow.Add(300*time.Millisecond))
	h.echo(t, "97001", "ok", delivered, googleEchoNow.Add(500*time.Millisecond))
	h.clock.Set(googleEchoNow.Add(time.Second))
	h.confirm(t, submission.OutboxID)
	if got := h.countV2Messages(t); got != 2 {
		t.Fatalf("messages after confirm = %d, want local plus echo", got)
	}

	h.echo(t, "97002", "ok", complete, googleEchoNow.Add(9*time.Second)) // typed on the phone
	h.assertBound(t, submission, "97001")
	if ids := h.remoteIDs(t); len(ids) != 2 {
		t.Fatalf("messages = %v, want the send and the phone's own text", ids)
	}
}

// Reviewer R1: a text typed on the phone a minute before an identical send
// must not be taken for the send's echo by the startup repair.
func TestRepairPrefersTheEchoOverAnEarlierPhoneText(t *testing.T) {
	h := newGoogleEchoHarnessWithObserver(t, nil, preFixObserver)
	h.echo(t, "96001", "Yes", complete, googleEchoNow) // typed on the phone
	send := h.sendAt(t, "send", "Yes", googleEchoNow.Add(time.Minute), googleEchoNow.Add(time.Minute+time.Second))
	h.echo(t, "96002", "Yes", complete, googleEchoNow.Add(time.Minute+1400*time.Millisecond))
	phoneAt := h.occurredAt(t, "96001")

	report, err := h.service.RepairUncorrelatedEchoDuplicates(context.Background())
	if err != nil || report.Merged != 1 {
		t.Fatalf("repair = %+v, %v; want one merge", report, err)
	}
	h.assertBound(t, send, "96002")
	if got := h.occurredAt(t, "96001"); got != phoneAt {
		t.Fatalf("phone text's time changed from %d to %d", phoneAt, got)
	}
	if got := h.countV2Messages(t); got != 2 {
		t.Fatalf("messages = %d, want the send and the phone text", got)
	}
}

// Reviewer R4: a scheduled or retried send goes out long after it was
// created. The window is measured from when it went out.
func TestGoogleEchoOfALateSendMerges(t *testing.T) {
	for _, viaRepair := range []bool{false, true} {
		t.Run(fmt.Sprintf("viaRepair=%v", viaRepair), func(t *testing.T) {
			var h *googleEchoHarness
			if viaRepair {
				h = newGoogleEchoHarnessWithObserver(t, nil, preFixObserver)
			} else {
				h = newGoogleEchoHarness(t, nil)
			}
			sentAt := googleEchoNow.Add(47 * time.Minute)
			send := h.sendAt(t, "late", "see you Saturday", googleEchoNow, sentAt)
			h.echo(t, "86953", "see you Saturday", delivered, sentAt.Add(500*time.Millisecond))
			if viaRepair {
				if _, err := h.service.RepairUncorrelatedEchoDuplicates(context.Background()); err != nil {
					t.Fatalf("repair: %v", err)
				}
			}
			h.assertBound(t, send, "86953")
		})
	}
}

// Reviewer R3: when an uncertain send is sent again, the resend owns the
// echo. The uncertain original must stay uncertain, not flip to delivered.
func TestGoogleEchoBelongsToTheResendNotTheUncertainOriginal(t *testing.T) {
	h := newGoogleEchoHarness(t, nil)
	h.sender.uncertain = true
	original, _ := h.enqueue(t, "original", "running late")
	if _, err := h.service.DispatchDue(context.Background(), 1); err != nil {
		t.Fatalf("DispatchDue(original): %v", err)
	}
	if state := h.delivery(t, original.OutboxID).State; state != messaging.OutboxUncertain {
		t.Fatalf("original state = %q, want uncertain", state)
	}
	h.sender.uncertain = false
	h.clock.Set(googleEchoNow.Add(30 * time.Second))
	resend, err := h.service.SendAgain(context.Background(), original.OutboxID, "resend-key")
	if err != nil {
		t.Fatalf("SendAgain(): %v", err)
	}
	h.confirm(t, resend.OutboxID)
	h.echo(t, "98001", "running late", complete, googleEchoNow.Add(30500*time.Millisecond))

	h.assertBound(t, resend, "98001")
	if delivery := h.delivery(t, original.OutboxID); delivery.State != messaging.OutboxUncertain {
		t.Fatalf("original = %+v, want it left uncertain", delivery)
	}
}

// Reviewer R6: Google reuses message IDs after a device ID-space reset. An ID
// already bound in another thread must not block this thread's merge.
func TestGoogleEchoWhoseIDIsBoundInAnotherThreadStillMerges(t *testing.T) {
	h := newGoogleEchoHarness(t, nil)
	nowMS := googleEchoNow.UnixMilli()
	if err := h.store.UpsertConversation(sqlite.Conversation{
		ConversationID:       "other-thread",
		AccountID:            googleEchoAccountID,
		RemoteConversationID: "other-remote",
		Kind:                 sqlite.ConversationKindDirect,
		Title:                "Other",
		NotificationMode:     sqlite.NotificationModeAll,
		MetadataJSON:         `{}`,
		CreatedAtMS:          nowMS,
		UpdatedAtMS:          nowMS,
	}); err != nil {
		t.Fatalf("UpsertConversation(other): %v", err)
	}
	other, err := h.service.SendText(context.Background(), messaging.SendTextCommand{
		CommonCommand: messaging.CommonCommand{AccountID: googleEchoAccountID, ConversationID: "other-thread", IdempotencyKey: "other"},
		Body:          "old",
	})
	if err != nil {
		t.Fatalf("SendText(other): %v", err)
	}
	h.confirm(t, other.OutboxID)
	outbox, err := sqlite.NewOutboxRepository(h.store, h.clock.Now)
	if err != nil {
		t.Fatalf("NewOutboxRepository(): %v", err)
	}
	item, err := outbox.FindByID(context.Background(), other.OutboxID)
	if err != nil {
		t.Fatalf("FindByID(): %v", err)
	}
	if _, err := outbox.ReconcileConfirm(context.Background(), sqlite.ReconcileRequest{
		AccountID: googleEchoAccountID, TransportRequestID: item.TransportRequestID, ResultRemoteID: "85896",
	}); err != nil {
		t.Fatalf("bind the other thread's send: %v", err)
	}

	send := h.sendAt(t, "here", "hello", googleEchoNow.Add(time.Minute), googleEchoNow.Add(time.Minute))
	h.echo(t, "85896", "hello", complete, googleEchoNow.Add(time.Minute+500*time.Millisecond))
	h.assertBound(t, send, "85896")
}

// Intents that are still in flight or never went out are never paired.
func TestGoogleEchoIgnoresSendsThatDidNotGoOut(t *testing.T) {
	h := newGoogleEchoHarness(t, nil)
	queued, requestID := h.enqueue(t, "queued", "hello")
	h.echo(t, "99001", "hello", complete, googleEchoNow.Add(time.Second))
	if ids := h.remoteIDs(t); len(ids) != 2 || ids[queued.LocalMessageID] != requestID {
		t.Fatalf("messages = %v, want the queued send left alone", ids)
	}
	if _, err := h.service.Cancel(context.Background(), queued.OutboxID); err != nil {
		t.Fatalf("Cancel(): %v", err)
	}
	h.echo(t, "99001", "hello", delivered, googleEchoNow.Add(time.Second))
	if delivery := h.delivery(t, queued.OutboxID); delivery.State != messaging.OutboxCanceled {
		t.Fatalf("canceled send = %+v, want it left canceled", delivery)
	}
	if report, err := h.service.RepairUncorrelatedEchoDuplicates(context.Background()); err != nil || report.Merged != 0 {
		t.Fatalf("repair = %+v, %v; want no merge into a canceled send", report, err)
	}
}

func TestGoogleEchoWithoutTmpIDLeavesUnrelatedMessagesAlone(t *testing.T) {
	t.Run("different body", func(t *testing.T) {
		h := newGoogleEchoHarness(t, nil)
		send := h.sendAt(t, "send", "hello", googleEchoNow, googleEchoNow)
		h.echo(t, "91000", "something else", complete, googleEchoNow.Add(500*time.Millisecond))
		assertUnpaired(t, h, send, "91000")
	})
	t.Run("received too long after the send went out", func(t *testing.T) {
		h := newGoogleEchoHarness(t, nil)
		send := h.sendAt(t, "send", "hello", googleEchoNow, googleEchoNow)
		h.echo(t, "91000", "hello", complete, googleEchoNow.Add(7*time.Second))
		assertUnpaired(t, h, send, "91000")
	})
	t.Run("received too long before the send went out", func(t *testing.T) {
		h := newGoogleEchoHarness(t, nil)
		// The send waits a minute (retrying); the phone text lands 6 s before
		// it finally goes out, and a status frame for it lands after.
		h.clock.Set(googleEchoNow.Add(-time.Minute))
		submission, _ := h.enqueue(t, "send", "hello")
		h.echo(t, "91000", "hello", complete, googleEchoNow.Add(-6*time.Second))
		h.clock.Set(googleEchoNow)
		h.confirm(t, submission.OutboxID)
		h.echo(t, "91000", "hello", delivered, googleEchoNow.Add(time.Second))
		assertUnpaired(t, h, submission, "91000")
	})
	t.Run("received a few seconds late still pairs", func(t *testing.T) {
		h := newGoogleEchoHarness(t, nil)
		send := h.sendAt(t, "send", "hello", googleEchoNow, googleEchoNow)
		h.echo(t, "91000", "hello", complete, googleEchoNow.Add(5*time.Second))
		h.assertBound(t, send, "91000")
	})
}

func assertUnpaired(t *testing.T, h *googleEchoHarness, send messaging.Submission, remoteID string) {
	t.Helper()
	if delivery := h.delivery(t, send.OutboxID); delivery.RemoteMessageID == remoteID {
		t.Fatalf("send paired with an unrelated message: %+v", delivery)
	}
	if got := h.countV2Messages(t); got != 2 {
		t.Fatalf("messages = %d, want the send and the phone's own message", got)
	}
	if snapshot := h.counters.Snapshot(googleEchoAccountID); snapshot.EchoContentMatched != 0 {
		t.Fatalf("counters = %+v, want no content match", snapshot)
	}
}

// When the resend's echo outruns the resend's own confirm, the uncertain
// original is the only unbound send with that body. It must not take the echo.
func TestGoogleEchoOutrunningAResendIsNotGivenToTheUncertainOriginal(t *testing.T) {
	h := newGoogleEchoHarness(t, nil)
	h.sender.uncertain = true
	original, _ := h.enqueue(t, "original", "running late")
	if _, err := h.service.DispatchDue(context.Background(), 1); err != nil {
		t.Fatalf("DispatchDue(original): %v", err)
	}
	h.sender.uncertain = false
	h.clock.Set(googleEchoNow.Add(time.Second))
	resend, err := h.service.SendAgain(context.Background(), original.OutboxID, "resend-key")
	if err != nil {
		t.Fatalf("SendAgain(): %v", err)
	}
	h.echo(t, "98002", "running late", complete, googleEchoNow.Add(1200*time.Millisecond))
	h.clock.Set(googleEchoNow.Add(1500 * time.Millisecond))
	h.confirm(t, resend.OutboxID)
	h.echo(t, "98002", "running late", delivered, googleEchoNow.Add(2*time.Second))

	h.assertBound(t, resend, "98002")
	if delivery := h.delivery(t, original.OutboxID); delivery.State != messaging.OutboxUncertain {
		t.Fatalf("original = %+v, want it left uncertain", delivery)
	}
}

// A canceled send of the same text, canceled just before the echo arrives,
// fits the echo's timing better than the real send. It must not absorb the
// pairing: the real send still takes its echo.
func TestGoogleEchoIsNotSpentOnACanceledSend(t *testing.T) {
	h := newGoogleEchoHarness(t, nil)
	send := h.sendAt(t, "send", "hello", googleEchoNow, googleEchoNow)
	h.clock.Set(googleEchoNow.Add(400 * time.Millisecond))
	canceled, _ := h.enqueue(t, "canceled", "hello")
	h.clock.Set(googleEchoNow.Add(500 * time.Millisecond))
	if _, err := h.service.Cancel(context.Background(), canceled.OutboxID); err != nil {
		t.Fatalf("Cancel(): %v", err)
	}
	h.echo(t, "99002", "hello", complete, googleEchoNow.Add(time.Second))
	h.assertBound(t, send, "99002")
}

func TestGoogleEchoWithoutTmpIDPairsRepeatedTextsWithTheirOwnEchoes(t *testing.T) {
	h := newGoogleEchoHarness(t, nil)
	first := h.sendAt(t, "first", "ok", googleEchoNow, googleEchoNow)
	// Each echo comes back once per status. The first message's DELIVERED
	// frame lands after the second send and must not claim it, which would
	// merge the two messages into one.
	h.echo(t, "93001", "ok", complete, googleEchoNow.Add(500*time.Millisecond))
	second := h.sendAt(t, "second", "ok", googleEchoNow.Add(time.Second), googleEchoNow.Add(time.Second))
	h.echo(t, "93001", "ok", delivered, googleEchoNow.Add(1200*time.Millisecond))
	h.echo(t, "93002", "ok", complete, googleEchoNow.Add(1500*time.Millisecond))
	h.echo(t, "93002", "ok", delivered, googleEchoNow.Add(2*time.Second))
	h.assertBound(t, first, "93001")
	h.assertBound(t, second, "93002")
	if got := h.countV2Messages(t); got != 2 {
		t.Fatalf("messages = %d, want two", got)
	}
}

func TestRepairUncorrelatedEchoDuplicatesIsIdempotent(t *testing.T) {
	h := newGoogleEchoHarnessWithObserver(t, nil, preFixObserver)
	a := h.sendAt(t, "a", "same text", googleEchoNow, googleEchoNow)
	h.echo(t, "94001", "same text", delivered, googleEchoNow.Add(500*time.Millisecond))
	b := h.sendAt(t, "b", "same text", googleEchoNow.Add(time.Minute), googleEchoNow.Add(time.Minute))
	h.echo(t, "94002", "same text", delivered, googleEchoNow.Add(time.Minute+500*time.Millisecond))
	h.echo(t, "94003", "same text", delivered, googleEchoNow.Add(time.Hour)) // typed on the phone
	if got := h.countV2Messages(t); got != 5 {
		t.Fatalf("messages before repair = %d, want 2 sends + 2 echoes + 1 phone text", got)
	}

	report, err := h.service.RepairUncorrelatedEchoDuplicates(context.Background())
	if err != nil || report.Merged != 2 || report.Failed != 0 {
		t.Fatalf("repair = %+v, %v; want 2 merged", report, err)
	}
	h.assertBound(t, a, "94001")
	h.assertBound(t, b, "94002")
	if got := h.countV2Messages(t); got != 3 {
		t.Fatalf("messages after repair = %d, want 2 sends + the phone text", got)
	}
	again, err := h.service.RepairUncorrelatedEchoDuplicates(context.Background())
	if err != nil || again.Groups != 0 || again.Merged != 0 {
		t.Fatalf("second repair = %+v, %v; want nothing left to do", again, err)
	}
}

// echoScenario interleaves sends from OpenMessage with texts typed on the
// phone, in time order, at least 2 s apart. Each send's echo is received
// 0.2-0.8 s after its confirm (as on the live store), or (outran) that long
// before it, with a DELIVERED re-delivery after the confirm.
//
// Closer than 2 s the input is ambiguous, not the rule: one send's echo can
// arrive late and the next one's early (outran), so the two echoes are seen
// in the opposite order to the sends. The pairing keeps pairs in send order,
// since one dispatcher sends one at a time, so it swaps the two identical
// texts; both messages stay (TestRepairKeepsBothMessagesWhenIdenticalEchoesCross).
// The pure pairing property (sqlite) documents the same limit.
type echoScenario struct {
	events []echoScenarioEvent
}

type echoScenarioEvent struct {
	fromApp bool
	outran  bool
	body    string
	at      time.Duration // after googleEchoNow
	lag     time.Duration // echo receipt relative to the confirm
}

func (echoScenario) Generate(r *rand.Rand, _ int) reflect.Value {
	n := 1 + r.Intn(7)
	var scenario echoScenario
	at := time.Duration(0)
	for i := 0; i < n; i++ {
		at += time.Duration(2+r.Intn(40)) * time.Second
		scenario.events = append(scenario.events, echoScenarioEvent{
			fromApp: r.Intn(3) != 0,
			outran:  r.Intn(4) == 0,
			body:    []string{"ok", "on my way"}[r.Intn(2)],
			at:      at,
			lag:     time.Duration(200+r.Intn(600)) * time.Millisecond,
		})
	}
	return reflect.ValueOf(scenario)
}

// runEchoScenario plays the scenario through the live correlation, or
// through a pre-fix build followed by the startup repair. It returns each
// send's final remote ID and the total message count.
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
		remoteID := fmt.Sprintf("95%03d", i)
		at := googleEchoNow.Add(event.at)
		if !event.fromApp {
			h.echo(t, remoteID, event.body, complete, at)
			continue
		}
		if event.outran {
			h.clock.Set(at.Add(-event.lag - 100*time.Millisecond))
			submission, _ := h.enqueue(t, fmt.Sprintf("send-%d", i), event.body)
			h.echo(t, remoteID, event.body, complete, at.Add(-event.lag))
			h.clock.Set(at)
			h.confirm(t, submission.OutboxID)
			h.echo(t, remoteID, event.body, delivered, at.Add(event.lag))
			sends[i] = submission
			continue
		}
		sends[i] = h.sendAt(t, fmt.Sprintf("send-%d", i), event.body, at, at)
		h.echo(t, remoteID, event.body, complete, at.Add(event.lag))
	}
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

// TestUncorrelatedEchoPairsEverySendWithItsOwnEchoProperty: for any
// scenario, through the live path and through the pre-fix path plus repair,
//   - every send ends bound to its own echo (ground truth, not just a
//     consistent answer), and
//   - every message, sent from the app or typed on the phone, is one row.
func TestUncorrelatedEchoPairsEverySendWithItsOwnEchoProperty(t *testing.T) {
	property := func(scenario echoScenario) bool {
		for _, viaRepair := range []bool{false, true} {
			bound, rows := runEchoScenario(t, scenario, viaRepair)
			if rows != len(scenario.events) {
				t.Logf("viaRepair=%v: rows = %d, want %d (%+v)", viaRepair, rows, len(scenario.events), scenario.events)
				return false
			}
			for i, remote := range bound {
				if want := fmt.Sprintf("95%03d", i); remote != want {
					t.Logf("viaRepair=%v: send %d bound to %q, want its own echo %q (%+v)", viaRepair, i, remote, want, scenario.events)
					return false
				}
			}
		}
		return true
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 30}); err != nil {
		t.Fatal(err)
	}
}

// echoStamped delivers an outgoing frame received at arrivedAt whose phone
// stamp is stamp, such as an old message arriving now (a history catch-up,
// or a live re-delivery when a reaction on it changes).
func (h *googleEchoHarness) echoStamped(t *testing.T, remoteID, body string, status gmproto.MessageStatusType, arrivedAt, stamp time.Time) {
	t.Helper()
	h.clock.Set(arrivedAt)
	h.appendMessage(t, untaggedEcho(remoteID, body, status, stamp))
	h.waitDrained(t)
	if errors := h.counters.Snapshot(googleEchoAccountID).EchoErrors; errors != 0 {
		t.Fatalf("echo reconcile failed %d times", errors)
	}
}

// Reviewer H1: last month's identical text arrives right after a send goes
// out. It is first seen now but stamped a month ago, so it is not an echo;
// the send waits for its real echo.
func TestGoogleEchoIgnoresAnOldMessageArrivingNow(t *testing.T) {
	h := newGoogleEchoHarness(t, nil)
	send := h.sendAt(t, "send", "ok", googleEchoNow, googleEchoNow)
	lastMonth := googleEchoNow.Add(-30 * 24 * time.Hour)
	h.echoStamped(t, "50001", "ok", delivered, googleEchoNow.Add(300*time.Millisecond), lastMonth)
	h.echo(t, "97001", "ok", complete, googleEchoNow.Add(600*time.Millisecond))

	h.assertBound(t, send, "97001")
	if got := h.occurredAt(t, "50001"); got != lastMonth.UnixMilli() {
		t.Fatalf("last month's message is stamped %d, want %d", got, lastMonth.UnixMilli())
	}
}

// Reviewer H2: the send's echo was missed and an old identical text was
// stored a second after the send went out. The repair must not merge it.
func TestRepairIgnoresAnOldMessageStoredNearASend(t *testing.T) {
	h := newGoogleEchoHarnessWithObserver(t, nil, preFixObserver)
	send := h.sendAt(t, "send", "ok", googleEchoNow, googleEchoNow)
	lastMonth := googleEchoNow.Add(-30 * 24 * time.Hour)
	h.echoStamped(t, "50002", "ok", delivered, googleEchoNow.Add(time.Second), lastMonth)

	report, err := h.service.RepairUncorrelatedEchoDuplicates(context.Background())
	if err != nil || report.Merged != 0 {
		t.Fatalf("repair = %+v, %v; want no merge", report, err)
	}
	if delivery := h.delivery(t, send.OutboxID); delivery.RemoteMessageID == "50002" {
		t.Fatalf("send took last month's message: %+v", delivery)
	}
	if got := h.occurredAt(t, "50002"); got != lastMonth.UnixMilli() {
		t.Fatalf("last month's message is stamped %d, want %d", got, lastMonth.UnixMilli())
	}
}

// The input CI drew on 2026-10-19 (Go Race on #227): two identical sends a
// second apart whose echoes are seen in the opposite order. Which echo is
// whose cannot be told from the data, so this pins only what must hold:
// every message stays its own row, and each send binds to one of the two
// echoes, never to a phone text.
func TestRepairKeepsBothMessagesWhenIdenticalEchoesCross(t *testing.T) {
	scenario := echoScenario{events: []echoScenarioEvent{
		{fromApp: false, outran: true, body: "ok", at: 25 * time.Second, lag: 468 * time.Millisecond},
		{fromApp: false, body: "ok", at: 37 * time.Second, lag: 605 * time.Millisecond},
		{fromApp: false, body: "ok", at: 41 * time.Second, lag: 366 * time.Millisecond},
		{fromApp: true, body: "on my way", at: 69 * time.Second, lag: 293 * time.Millisecond},
		{fromApp: false, body: "ok", at: 94 * time.Second, lag: 439 * time.Millisecond},
		{fromApp: true, body: "on my way", at: 111 * time.Second, lag: 768 * time.Millisecond},
		{fromApp: true, outran: true, body: "on my way", at: 112 * time.Second, lag: 259 * time.Millisecond},
	}}
	for _, viaRepair := range []bool{false, true} {
		bound, rows := runEchoScenario(t, scenario, viaRepair)
		if rows != len(scenario.events) {
			t.Fatalf("viaRepair=%v: rows = %d, want every message kept (%d)", viaRepair, rows, len(scenario.events))
		}
		if bound[3] != "95003" {
			t.Fatalf("viaRepair=%v: send 3 bound to %q, want its own echo", viaRepair, bound[3])
		}
		crossing := map[string]bool{"95005": true, "95006": true}
		if !crossing[bound[5]] || !crossing[bound[6]] || bound[5] == bound[6] {
			t.Fatalf("viaRepair=%v: sends 5 and 6 bound to %q and %q, want the two crossing echoes", viaRepair, bound[5], bound[6])
		}
	}
}
