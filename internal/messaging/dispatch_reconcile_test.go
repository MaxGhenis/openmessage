package messaging

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

func TestObserveTransportEchoConfirmsUncertainAndRepointsMessage(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	sender := &scriptedTextSender{steps: []sendStep{{result: bridge.SendResult{}}}}
	registry := newScriptedRegistry("reconcile-uncertain", sender)
	registry.setAvailable(true)
	service := newMessagingTestService(t, store, registry, clock)
	submission := mustSendText(t, service, SendTextCommand{
		CommonCommand: testCommonCommand("key-reconcile-uncertain"),
		Body:          "accept the later echo",
	})
	item := mustOutboxItem(t, service, submission.OutboxID)

	if processed, err := service.DispatchDue(context.Background(), 1); err != nil || processed != 1 {
		t.Fatalf("DispatchDue() = %d, %v; want 1, nil", processed, err)
	}
	if got := mustDelivery(t, service, submission.OutboxID); got.State != OutboxUncertain {
		t.Fatalf("delivery before echo = %+v, want uncertain", got)
	}
	if got := mustReconcileMessage(t, service, submission.LocalMessageID).RemoteMessageID; got != item.TransportRequestID {
		t.Fatalf("message remote ID before echo = %q, want placeholder %q", got, item.TransportRequestID)
	}

	const realID = "remote-reconciled"
	if _, err := service.ObserveTransportEcho(context.Background(), TransportEcho{
		AccountID:          item.AccountID,
		TransportRequestID: item.TransportRequestID,
		RemoteMessageID:    realID,
	}); err != nil {
		t.Fatalf("ObserveTransportEcho(): %v", err)
	}
	if got := mustDelivery(t, service, submission.OutboxID); got.State != OutboxConfirmed || got.RemoteMessageID != realID {
		t.Fatalf("delivery after echo = %+v, want confirmed at %q", got, realID)
	}
	message := mustReconcileMessage(t, service, submission.LocalMessageID)
	if message.MessageID != submission.LocalMessageID || message.RemoteMessageID != realID {
		t.Fatalf("repointed message = %+v, want stable local ID and remote ID %q", message, realID)
	}
}

func TestObserveTransportEchoCollisionKeepsOptimisticFKAnchor(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	sender := &scriptedTextSender{steps: []sendStep{{result: bridge.SendResult{}}}}
	registry := newScriptedRegistry("reconcile-collision", sender)
	registry.setReactionSender(&scriptedReactionSender{})
	registry.setAvailable(true)
	service := newMessagingTestService(t, store, registry, clock)
	submission := mustSendText(t, service, SendTextCommand{
		CommonCommand: testCommonCommand("key-reconcile-collision"),
		Body:          "keep this optimistic row",
	})
	item := mustOutboxItem(t, service, submission.OutboxID)

	const (
		echoMessageID = "echo-projected-duplicate"
		realID        = "remote-collision"
	)
	projectReconcileEchoMessage(t, store, clock, echoMessageID, realID)
	dependent := mustSendReaction(t, service, SendReactionCommand{
		CommonCommand: CommonCommand{
			AccountID:      item.AccountID,
			ConversationID: item.ConversationID,
			IdempotencyKey: "key-reconcile-dependent-on-m",
			NotBefore:      clock.Now().Add(time.Hour),
		},
		TargetMessageID: submission.LocalMessageID,
		Emoji:           "👍",
	})

	if processed, err := service.DispatchDue(context.Background(), 1); err != nil || processed != 1 {
		t.Fatalf("DispatchDue() = %d, %v; want 1, nil", processed, err)
	}
	if got := mustDelivery(t, service, submission.OutboxID).State; got != OutboxUncertain {
		t.Fatalf("delivery before echo = %q, want uncertain", got)
	}
	if _, err := service.ObserveTransportEcho(context.Background(), TransportEcho{
		AccountID:          item.AccountID,
		TransportRequestID: item.TransportRequestID,
		RemoteMessageID:    realID,
	}); err != nil {
		t.Fatalf("ObserveTransportEcho(): %v", err)
	}

	message := mustReconcileMessage(t, service, submission.LocalMessageID)
	if message.MessageID != submission.LocalMessageID || message.RemoteMessageID != realID {
		t.Fatalf("collision survivor = %+v, want optimistic message %q at %q", message, submission.LocalMessageID, realID)
	}
	if _, err := service.messages.GetMessage(context.Background(), echoMessageID); !errors.Is(err, sqlite.ErrNotFound) {
		t.Fatalf("GetMessage(echo duplicate) error = %v, want ErrNotFound", err)
	}
	reaction, err := service.outbox.GetOutboxReaction(context.Background(), dependent.OutboxID)
	if err != nil {
		t.Fatalf("GetOutboxReaction(dependent): %v", err)
	}
	if reaction.TargetMessageID != submission.LocalMessageID {
		t.Fatalf("dependent reaction target = %q, want surviving message %q", reaction.TargetMessageID, submission.LocalMessageID)
	}
}

func TestObserveTransportEchoEnrichesPlaceholderConfirmation(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	sender := &scriptedTextSender{}
	registry := newScriptedRegistry("reconcile-enrich", sender)
	registry.setAvailable(true)
	service := newMessagingTestService(t, store, registry, clock)
	submission := mustSendText(t, service, SendTextCommand{
		CommonCommand: testCommonCommand("key-reconcile-enrich"),
		Body:          "confirm provisionally",
	})
	item := mustOutboxItem(t, service, submission.OutboxID)
	setReconcileTextSteps(sender, sendStep{result: bridge.SendResult{RemoteMessageID: item.TransportRequestID}})

	if processed, err := service.DispatchDue(context.Background(), 1); err != nil || processed != 1 {
		t.Fatalf("DispatchDue() = %d, %v; want 1, nil", processed, err)
	}
	if got := mustDelivery(t, service, submission.OutboxID); got.State != OutboxConfirmed || got.RemoteMessageID != item.TransportRequestID {
		t.Fatalf("provisional delivery = %+v, want confirmed at placeholder", got)
	}
	if got := mustReconcileMessage(t, service, submission.LocalMessageID).RemoteMessageID; got != item.TransportRequestID {
		t.Fatalf("message remote ID before enrichment = %q, want %q", got, item.TransportRequestID)
	}

	const permanentID = "remote-permanent"
	if _, err := service.ObserveTransportEcho(context.Background(), TransportEcho{
		AccountID:          item.AccountID,
		TransportRequestID: item.TransportRequestID,
		RemoteMessageID:    permanentID,
	}); err != nil {
		t.Fatalf("ObserveTransportEcho(): %v", err)
	}
	if got := mustDelivery(t, service, submission.OutboxID); got.State != OutboxConfirmed || got.RemoteMessageID != permanentID {
		t.Fatalf("enriched delivery = %+v, want confirmed at %q", got, permanentID)
	}
	if got := mustReconcileMessage(t, service, submission.LocalMessageID).RemoteMessageID; got != permanentID {
		t.Fatalf("enriched message remote ID = %q, want %q", got, permanentID)
	}
}

func TestObserveTransportEchoNoOpsNeverCreateOrCorrupt(t *testing.T) {
	t.Run("duplicate foreign and orphan", func(t *testing.T) {
		clock := newManualClock(messagingTestTime)
		store := openMessagingTestStore(t, clock.Now())
		sender := &scriptedTextSender{}
		registry := newScriptedRegistry("reconcile-noop", sender)
		registry.setAvailable(true)
		service := newMessagingTestService(t, store, registry, clock)
		submission := mustSendText(t, service, SendTextCommand{
			CommonCommand: testCommonCommand("key-reconcile-noop"),
			Body:          "already authoritative",
		})
		item := mustOutboxItem(t, service, submission.OutboxID)
		const authoritativeID = "remote-authoritative"
		setReconcileTextSteps(sender, sendStep{result: bridge.SendResult{RemoteMessageID: authoritativeID}})
		if processed, err := service.DispatchDue(context.Background(), 1); err != nil || processed != 1 {
			t.Fatalf("DispatchDue() = %d, %v; want 1, nil", processed, err)
		}

		for _, echo := range []TransportEcho{
			{AccountID: item.AccountID, TransportRequestID: item.TransportRequestID, RemoteMessageID: authoritativeID},
			{AccountID: item.AccountID, TransportRequestID: item.TransportRequestID, RemoteMessageID: "remote-foreign"},
			{AccountID: item.AccountID, TransportRequestID: "missing-request", RemoteMessageID: "remote-orphan"},
		} {
			if _, err := service.ObserveTransportEcho(context.Background(), echo); err != nil {
				t.Fatalf("ObserveTransportEcho(%+v): %v", echo, err)
			}
		}
		if _, err := service.outbox.FindByTransportRequestID(context.Background(), item.AccountID, "missing-request"); !errors.Is(err, sqlite.ErrNotFound) {
			t.Fatalf("FindByTransportRequestID(orphan) error = %v, want ErrNotFound", err)
		}
		if got := mustDelivery(t, service, submission.OutboxID); got.State != OutboxConfirmed || got.RemoteMessageID != authoritativeID {
			t.Fatalf("delivery after no-op echoes = %+v, want original confirmation", got)
		}
		if got := mustReconcileMessage(t, service, submission.LocalMessageID).RemoteMessageID; got != authoritativeID {
			t.Fatalf("message remote ID after foreign echo = %q, want %q", got, authoritativeID)
		}
	})

	t.Run("rejected stays rejected", func(t *testing.T) {
		clock := newManualClock(messagingTestTime)
		store := openMessagingTestStore(t, clock.Now())
		sender := &scriptedTextSender{steps: []sendStep{{err: bridge.OpError{
			Class:     bridge.FailureUnsupported,
			Operation: "send_text",
			Dispatch:  bridge.DispatchNotCalled,
			Cause:     errors.New("terminal rejection"),
		}}}}
		registry := newScriptedRegistry("reconcile-rejected", sender)
		registry.setAvailable(true)
		service := newMessagingTestService(t, store, registry, clock)
		submission := mustSendText(t, service, SendTextCommand{
			CommonCommand: testCommonCommand("key-reconcile-rejected"),
			Body:          "terminal",
		})
		item := mustOutboxItem(t, service, submission.OutboxID)
		if processed, err := service.DispatchDue(context.Background(), 1); err != nil || processed != 1 {
			t.Fatalf("DispatchDue() = %d, %v; want 1, nil", processed, err)
		}
		if got := mustDelivery(t, service, submission.OutboxID).State; got != OutboxRejected {
			t.Fatalf("delivery before echo = %q, want rejected", got)
		}
		if _, err := service.ObserveTransportEcho(context.Background(), TransportEcho{
			AccountID: item.AccountID, TransportRequestID: item.TransportRequestID, RemoteMessageID: "remote-rejected",
		}); err != nil {
			t.Fatalf("ObserveTransportEcho(rejected): %v", err)
		}
		if got := mustDelivery(t, service, submission.OutboxID); got.State != OutboxRejected || got.RemoteMessageID != "" {
			t.Fatalf("rejected delivery after echo = %+v", got)
		}
		if got := mustReconcileMessage(t, service, submission.LocalMessageID).RemoteMessageID; got != item.TransportRequestID {
			t.Fatalf("rejected message remote ID = %q, want placeholder %q", got, item.TransportRequestID)
		}
	})

	t.Run("canceled stays canceled", func(t *testing.T) {
		clock := newManualClock(messagingTestTime)
		store := openMessagingTestStore(t, clock.Now())
		registry := newScriptedRegistry("reconcile-canceled", &scriptedTextSender{})
		service := newMessagingTestService(t, store, registry, clock)
		command := SendTextCommand{
			CommonCommand: testCommonCommand("key-reconcile-canceled"),
			Body:          "cancel first",
		}
		command.NotBefore = clock.Now().Add(time.Hour)
		submission := mustSendText(t, service, command)
		item := mustOutboxItem(t, service, submission.OutboxID)
		if _, err := service.Cancel(context.Background(), submission.OutboxID); err != nil {
			t.Fatalf("Cancel(): %v", err)
		}
		if _, err := service.ObserveTransportEcho(context.Background(), TransportEcho{
			AccountID: item.AccountID, TransportRequestID: item.TransportRequestID, RemoteMessageID: "remote-canceled",
		}); err != nil {
			t.Fatalf("ObserveTransportEcho(canceled): %v", err)
		}
		if got := mustDelivery(t, service, submission.OutboxID); got.State != OutboxCanceled || got.RemoteMessageID != "" {
			t.Fatalf("canceled delivery after echo = %+v", got)
		}
		if got := mustReconcileMessage(t, service, submission.LocalMessageID).RemoteMessageID; got != item.TransportRequestID {
			t.Fatalf("canceled message remote ID = %q, want placeholder %q", got, item.TransportRequestID)
		}
	})
}

func TestObserveTransportEchoDuringDispatchingDefersToLeaseOwner(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	const realID = "remote-lease-result"
	sender := &scriptedTextSender{steps: []sendStep{{result: bridge.SendResult{RemoteMessageID: realID}}}}
	registry := newScriptedRegistry("reconcile-dispatching", sender)
	registry.setAvailable(true)
	service := newMessagingTestService(t, store, registry, clock)
	submission := mustSendText(t, service, SendTextCommand{
		CommonCommand: testCommonCommand("key-reconcile-dispatching"),
		Body:          "lease owner wins",
	})
	item := mustOutboxItem(t, service, submission.OutboxID)

	var (
		echoErr       error
		during        Delivery
		duringErr     error
		duringMessage sqlite.Message
		messageErr    error
	)
	sender.onSend = func() {
		_, echoErr = service.ObserveTransportEcho(context.Background(), TransportEcho{
			AccountID: item.AccountID, TransportRequestID: item.TransportRequestID, RemoteMessageID: realID,
		})
		during, duringErr = service.Get(context.Background(), submission.OutboxID)
		duringMessage, messageErr = service.messages.GetMessage(context.Background(), submission.LocalMessageID)
	}
	if processed, err := service.DispatchDue(context.Background(), 1); err != nil || processed != 1 {
		t.Fatalf("DispatchDue() = %d, %v; want 1, nil", processed, err)
	}
	if echoErr != nil || duringErr != nil || messageErr != nil {
		t.Fatalf("during-send echo/get errors = %v / %v / %v", echoErr, duringErr, messageErr)
	}
	if during.State != OutboxDispatching || during.RemoteMessageID != "" {
		t.Fatalf("delivery during transport call = %+v, want unchanged dispatching", during)
	}
	if duringMessage.RemoteMessageID != item.TransportRequestID {
		t.Fatalf("message during transport call = %q, want placeholder %q", duringMessage.RemoteMessageID, item.TransportRequestID)
	}
	if got := mustDelivery(t, service, submission.OutboxID); got.State != OutboxConfirmed || got.RemoteMessageID != realID {
		t.Fatalf("delivery after lease confirmation = %+v, want confirmed at %q", got, realID)
	}
	if got := mustReconcileMessage(t, service, submission.LocalMessageID).RemoteMessageID; got != realID {
		t.Fatalf("message after lease confirmation = %q, want %q", got, realID)
	}
	if _, err := service.ObserveTransportEcho(context.Background(), TransportEcho{
		AccountID: item.AccountID, TransportRequestID: item.TransportRequestID, RemoteMessageID: realID,
	}); err != nil {
		t.Fatalf("ObserveTransportEcho(replay): %v", err)
	}
	if got := mustDelivery(t, service, submission.OutboxID); got.State != OutboxConfirmed || got.RemoteMessageID != realID {
		t.Fatalf("delivery after echo replay = %+v", got)
	}
}

func TestStoreFailedRepointRepair(t *testing.T) {
	t.Run("explicit repair", func(t *testing.T) {
		clock := newManualClock(messagingTestTime)
		fixture := newStoreFailedRepointFixture(t, clock)
		fixture.removeBlocker(t)

		delivery, err := fixture.service.RepairStoreFailed(context.Background(), fixture.submission.OutboxID)
		if err != nil {
			t.Fatalf("RepairStoreFailed(): %v", err)
		}
		fixture.assertRepaired(t, delivery)

		// The public repair API is state-guarded. A second call is idempotent in
		// effect: it reports ErrInvalidState and leaves the confirmed row intact.
		if _, err := fixture.service.RepairStoreFailed(context.Background(), fixture.submission.OutboxID); !errors.Is(err, ErrInvalidState) {
			t.Fatalf("RepairStoreFailed(second) error = %v, want ErrInvalidState", err)
		}
		fixture.assertRepaired(t, mustDelivery(t, fixture.service, fixture.submission.OutboxID))
	})

	t.Run("Run tolerates blocked row then sweeps it", func(t *testing.T) {
		clock := newNotifyingReconcileClock(messagingTestTime)
		fixture := newStoreFailedRepointFixture(t, clock)
		fixture.service.pollDelay = time.Hour
		drainReconcileWake(fixture.service)

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- fixture.service.Run(ctx) }()
		defer func() {
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Errorf("Run() error = %v, want context.Canceled", err)
				}
			case <-time.After(2 * time.Second):
				t.Error("Run() did not stop after cancellation")
			}
		}()

		select {
		case <-clock.timerCreated:
			// Reaching the poll timer proves the first, still-blocked repair
			// attempt was contained instead of terminating Run.
		case err := <-done:
			t.Fatalf("Run() returned on unrepairable row: %v", err)
		case <-time.After(2 * time.Second):
			t.Fatal("Run() did not finish its first bounded repair pass")
		}
		if got := mustDelivery(t, fixture.service, fixture.submission.OutboxID).State; got != OutboxStoreFailed {
			t.Fatalf("blocked sweep state = %q, want store_failed", got)
		}

		fixture.removeBlocker(t)
		fixture.service.signalWake()
		select {
		case <-clock.timerCreated:
			// A new timer is created only after the woken loop has run its
			// bounded repair and dispatch passes.
		case err := <-done:
			t.Fatalf("Run() returned during repair sweep: %v", err)
		case <-time.After(2 * time.Second):
			t.Fatal("Run() did not finish its unblocked repair pass")
		}
		fixture.assertRepaired(t, mustDelivery(t, fixture.service, fixture.submission.OutboxID))
	})
}

// An idempotent SendText replay must keep working AFTER the repoint — the
// exact case the idempotency key exists for: delivery succeeds, the API
// response is lost, the client retries the same key. Pre-fix, pair validation
// still demanded remote_message_id == transport_request_id, which stops
// holding once a Signal-style Confirm (real id differs from the request id)
// repoints the message, so the retry spuriously failed instead of returning
// the original Submission.
func TestSendTextReplayAfterConfirmRepointReturnsOriginalSubmission(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	sender := &scriptedTextSender{}
	registry := newScriptedRegistry("replay-after-repoint", sender)
	registry.setAvailable(true)
	service := newMessagingTestService(t, store, registry, clock)
	command := SendTextCommand{
		CommonCommand: testCommonCommand("key-replay-after-repoint"),
		Body:          "deliver then replay",
	}
	submission := mustSendText(t, service, command)
	item := mustOutboxItem(t, service, submission.OutboxID)

	// Signal-style: the transport's real id differs from the request id, so
	// Confirm repoints the optimistic message at confirm time.
	const realID = "remote-signal-timestamp"
	setReconcileTextSteps(sender, sendStep{result: bridge.SendResult{RemoteMessageID: realID}})
	if processed, err := service.DispatchDue(context.Background(), 1); err != nil || processed != 1 {
		t.Fatalf("DispatchDue() = %d, %v; want 1, nil", processed, err)
	}
	if got := mustReconcileMessage(t, service, submission.LocalMessageID).RemoteMessageID; got != realID {
		t.Fatalf("message remote ID after confirm = %q, want repointed %q", got, realID)
	}

	replay := mustSendText(t, service, command)
	if replay.OutboxID != submission.OutboxID || replay.LocalMessageID != submission.LocalMessageID {
		t.Fatalf("replay = %+v, want original submission %+v", replay, submission)
	}
	if got := sender.requestCount(); got != 1 {
		t.Fatalf("transport sends after replay = %d, want exactly 1", got)
	}
	if got := mustDelivery(t, service, submission.OutboxID); got.State != OutboxConfirmed ||
		got.RemoteMessageID != realID {
		t.Fatalf("delivery after replay = %+v, want confirmed at %q", got, realID)
	}
	if item.TransportRequestID == realID {
		t.Fatal("test fixture degenerate: real id must differ from the request id")
	}
}

// The Google-style variant: Confirm records the placeholder, the later echo
// repoints to the permanent id — and an idempotent replay after the echo must
// still return the original Submission.
func TestSendTextReplayAfterEchoRepointReturnsOriginalSubmission(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	sender := &scriptedTextSender{}
	registry := newScriptedRegistry("replay-after-echo", sender)
	registry.setAvailable(true)
	service := newMessagingTestService(t, store, registry, clock)
	command := SendTextCommand{
		CommonCommand: testCommonCommand("key-replay-after-echo"),
		Body:          "confirm provisionally then echo then replay",
	}
	submission := mustSendText(t, service, command)
	item := mustOutboxItem(t, service, submission.OutboxID)
	setReconcileTextSteps(sender, sendStep{result: bridge.SendResult{RemoteMessageID: item.TransportRequestID}})
	if processed, err := service.DispatchDue(context.Background(), 1); err != nil || processed != 1 {
		t.Fatalf("DispatchDue() = %d, %v; want 1, nil", processed, err)
	}

	const permanentID = "remote-permanent-echo"
	if _, err := service.ObserveTransportEcho(context.Background(), TransportEcho{
		AccountID:          item.AccountID,
		TransportRequestID: item.TransportRequestID,
		RemoteMessageID:    permanentID,
	}); err != nil {
		t.Fatalf("ObserveTransportEcho(): %v", err)
	}
	if got := mustReconcileMessage(t, service, submission.LocalMessageID).RemoteMessageID; got != permanentID {
		t.Fatalf("message remote ID after echo = %q, want %q", got, permanentID)
	}

	replay := mustSendText(t, service, command)
	if replay.OutboxID != submission.OutboxID || replay.LocalMessageID != submission.LocalMessageID {
		t.Fatalf("replay = %+v, want original submission %+v", replay, submission)
	}
	if got := sender.requestCount(); got != 1 {
		t.Fatalf("transport sends after replay = %d, want exactly 1", got)
	}
}

func TestUncertainCannotReenterDispatchWhenArtificiallyArmed(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store, raw := openReconcileTestStore(t, clock.Now())
	sender := &scriptedTextSender{steps: []sendStep{{result: bridge.SendResult{}}}}
	registry := newScriptedRegistry("reconcile-unleaseable", sender)
	registry.setAvailable(true)
	service := newMessagingTestService(t, store, registry, clock)
	submission := mustSendText(t, service, SendTextCommand{
		CommonCommand: testCommonCommand("key-reconcile-unleaseable"),
		Body:          "never send twice",
	})

	if processed, err := service.DispatchDue(context.Background(), 1); err != nil || processed != 1 {
		t.Fatalf("DispatchDue(first) = %d, %v; want 1, nil", processed, err)
	}
	row := mustOutboxItem(t, service, submission.OutboxID)
	if row.State != sqlite.OutboxUncertain || row.NextAttemptAtMS != nil {
		t.Fatalf("uncertain row = %+v, want no next attempt", row)
	}
	if got := sender.requestCount(); got != 1 {
		t.Fatalf("send count before artificial arm = %d, want 1", got)
	}

	dueAt := clock.Now().Add(time.Minute)
	mustReconcileExecOne(t, raw, `
		UPDATE outbox
		SET next_attempt_at_ms = ?
		WHERE outbox_id = ? AND state = 'uncertain'
	`, dueAt.UnixMilli(), submission.OutboxID)
	clock.Advance(2 * time.Minute)
	if processed, err := service.DispatchDue(context.Background(), 8); err != nil || processed != 0 {
		t.Fatalf("DispatchDue(artificially armed uncertain) = %d, %v; want 0, nil", processed, err)
	}
	if got := sender.requestCount(); got != 1 {
		t.Fatalf("send count after artificial arm = %d, want exactly 1", got)
	}
	if got := mustDelivery(t, service, submission.OutboxID).State; got != OutboxUncertain {
		t.Fatalf("state after artificial arm = %q, want uncertain", got)
	}
}

type storeFailedRepointFixture struct {
	service            *MessageService
	raw                *sql.DB
	submission         Submission
	transportRequestID string
	realID             string
	echoMessageID      string
	blockerOutboxID    string
}

func newStoreFailedRepointFixture(t *testing.T, clock Clock) storeFailedRepointFixture {
	t.Helper()
	store, raw := openReconcileTestStore(t, clock.Now())
	const realID = "remote-store-failed"
	sender := &scriptedTextSender{steps: []sendStep{{result: bridge.SendResult{RemoteMessageID: realID}}}}
	registry := newScriptedRegistry("reconcile-store-failed", sender)
	registry.setReactionSender(&scriptedReactionSender{})
	registry.setAvailable(true)
	service := newMessagingTestService(t, store, registry, clock)
	submission := mustSendText(t, service, SendTextCommand{
		CommonCommand: testCommonCommand("key-reconcile-store-failed"),
		Body:          "remote accepted this",
	})
	item := mustOutboxItem(t, service, submission.OutboxID)
	const echoMessageID = "echo-store-failed-collision"
	projectReconcileEchoMessage(t, store, clock, echoMessageID, realID)
	blocker := mustSendReaction(t, service, SendReactionCommand{
		CommonCommand: CommonCommand{
			AccountID:      item.AccountID,
			ConversationID: item.ConversationID,
			IdempotencyKey: "key-reconcile-store-failed-blocker",
			NotBefore:      clock.Now().Add(time.Hour),
		},
		TargetMessageID: echoMessageID,
		Emoji:           "🔒",
	})

	if processed, err := service.DispatchDue(context.Background(), 1); err != nil || processed != 1 {
		t.Fatalf("DispatchDue(store failure) = %d, %v; want 1, nil", processed, err)
	}
	delivery := mustDelivery(t, service, submission.OutboxID)
	if delivery.State != OutboxStoreFailed || delivery.RemoteMessageID != realID {
		t.Fatalf("delivery after failed repoint = %+v, want store_failed at %q", delivery, realID)
	}
	if got := mustReconcileMessage(t, service, submission.LocalMessageID).RemoteMessageID; got != item.TransportRequestID {
		t.Fatalf("optimistic message after rolled-back Confirm = %q, want %q", got, item.TransportRequestID)
	}
	if got := mustReconcileMessage(t, service, echoMessageID).RemoteMessageID; got != realID {
		t.Fatalf("colliding echo after rolled-back Confirm = %q, want %q", got, realID)
	}
	if got := sender.requestCount(); got != 1 {
		t.Fatalf("send count after store failure = %d, want 1", got)
	}

	return storeFailedRepointFixture{
		service:            service,
		raw:                raw,
		submission:         submission,
		transportRequestID: item.TransportRequestID,
		realID:             realID,
		echoMessageID:      echoMessageID,
		blockerOutboxID:    blocker.OutboxID,
	}
}

func (f storeFailedRepointFixture) removeBlocker(t *testing.T) {
	t.Helper()
	mustReconcileExecOne(t, f.raw, `DELETE FROM outbox WHERE outbox_id = ?`, f.blockerOutboxID)
	if _, err := f.service.outbox.GetOutboxReaction(context.Background(), f.blockerOutboxID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetOutboxReaction(removed blocker) error = %v, want sql.ErrNoRows", err)
	}
}

func (f storeFailedRepointFixture) assertRepaired(t *testing.T, delivery Delivery) {
	t.Helper()
	if delivery.State != OutboxConfirmed || delivery.RemoteMessageID != f.realID {
		t.Fatalf("repaired delivery = %+v, want confirmed at %q", delivery, f.realID)
	}
	message := mustReconcileMessage(t, f.service, f.submission.LocalMessageID)
	if message.MessageID != f.submission.LocalMessageID || message.RemoteMessageID != f.realID {
		t.Fatalf("repaired optimistic message = %+v", message)
	}
	if _, err := f.service.messages.GetMessage(context.Background(), f.echoMessageID); !errors.Is(err, sqlite.ErrNotFound) {
		t.Fatalf("GetMessage(repaired echo duplicate) error = %v, want ErrNotFound", err)
	}
}

func openReconcileTestStore(t *testing.T, now time.Time) (*sqlite.Store, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "message.sqlite")
	store, err := sqlite.Open(path)
	if err != nil {
		t.Fatalf("sqlite.Open(): %v", err)
	}
	query := make(url.Values)
	query.Add("_pragma", "busy_timeout(5000)")
	query.Add("_pragma", "foreign_keys(ON)")
	raw, err := sql.Open("sqlite", (&url.URL{
		Scheme:   "file",
		Path:     filepath.ToSlash(path),
		RawQuery: query.Encode(),
	}).String())
	if err != nil {
		_ = store.Close()
		t.Fatalf("sql.Open(raw test database): %v", err)
	}
	raw.SetMaxOpenConns(1)
	raw.SetMaxIdleConns(1)
	if err := raw.PingContext(context.Background()); err != nil {
		_ = raw.Close()
		_ = store.Close()
		t.Fatalf("ping raw test database: %v", err)
	}
	t.Cleanup(func() {
		_ = raw.Close()
		_ = store.Close()
	})
	seedMessagingStore(t, store, now)
	return store, raw
}

func projectReconcileEchoMessage(
	t *testing.T,
	store *sqlite.Store,
	clock Clock,
	messageID, remoteID string,
) {
	t.Helper()
	seedMessagingMessage(t, store, clock, sqlite.Message{
		MessageID:       messageID,
		ConversationID:  "conversation-1",
		AccountID:       "account-1",
		RemoteMessageID: remoteID,
		Direction:       sqlite.MessageDirectionOutgoing,
		Body:            "projected own-message echo",
		State:           sqlite.MessageStateActive,
		OccurredAtMS:    clock.Now().UnixMilli(),
	})
}

func mustReconcileMessage(t *testing.T, service *MessageService, messageID string) sqlite.Message {
	t.Helper()
	message, err := service.messages.GetMessage(context.Background(), messageID)
	if err != nil {
		t.Fatalf("GetMessage(%q): %v", messageID, err)
	}
	return message
}

func mustReconcileExecOne(t *testing.T, db *sql.DB, statement string, args ...any) {
	t.Helper()
	result, err := db.ExecContext(context.Background(), statement, args...)
	if err != nil {
		t.Fatalf("test database mutation: %v", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		t.Fatalf("test database mutation RowsAffected(): %v", err)
	}
	if affected != 1 {
		t.Fatalf("test database mutation affected %d rows, want 1", affected)
	}
}

func setReconcileTextSteps(sender *scriptedTextSender, steps ...sendStep) {
	sender.mu.Lock()
	sender.steps = append([]sendStep(nil), steps...)
	sender.mu.Unlock()
}

type notifyingReconcileClock struct {
	*manualClock
	timerCreated chan struct{}
}

func newNotifyingReconcileClock(now time.Time) *notifyingReconcileClock {
	return &notifyingReconcileClock{
		manualClock:  newManualClock(now),
		timerCreated: make(chan struct{}, 4),
	}
}

func (c *notifyingReconcileClock) NewTimer(delay time.Duration) Timer {
	select {
	case c.timerCreated <- struct{}{}:
	default:
	}
	return c.manualClock.NewTimer(delay)
}

func drainReconcileWake(service *MessageService) {
	for {
		select {
		case <-service.wake:
		default:
			return
		}
	}
}
