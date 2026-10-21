package messaging

import (
	"context"
	"fmt"
	"math/rand"
	"testing"
	"testing/quick"
	"time"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

func TestQueuedReactionSurvivesRestartAndMapsRequest(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	identityID := "identity-reaction-author"
	seedDispatchIdentity(t, store, identityID, "author@example.test", clock.Now())
	target := mustProjectDispatchMessage(t, store, clock, sqlite.Message{
		MessageID:        "message-reaction-target",
		ConversationID:   "conversation-1",
		AccountID:        "account-1",
		RemoteMessageID:  "remote-reaction-target",
		SenderIdentityID: &identityID,
		Direction:        sqlite.MessageDirectionIncoming,
		Body:             "target body is not copied",
		State:            sqlite.MessageStateActive,
		OccurredAtMS:     clock.Now().Add(-2 * time.Minute).UnixMilli(),
	})

	beforeRestart := newScriptedRegistry("reaction-before-restart", &scriptedTextSender{})
	beforeRestart.setReactionSender(&scriptedReactionSender{})
	first := newMessagingTestService(t, store, beforeRestart, clock)
	submission := mustSendDispatchReaction(t, first, SendReactionCommand{
		CommonCommand:   testCommonCommand("reaction-restart"),
		TargetMessageID: target.MessageID,
		Emoji:           "🎉",
		Action:          bridge.ReactionSwitch,
	})

	sender := &scriptedReactionSender{steps: []sendStep{{
		result: bridge.SendResult{RemoteMessageID: "remote-reaction-result"},
	}}}
	afterRestart := newScriptedRegistry("reaction-after-restart", &scriptedTextSender{})
	afterRestart.setReactionSender(sender)
	afterRestart.setAvailable(true)
	restarted := newMessagingTestService(t, store, afterRestart, clock)

	if before := mustDelivery(t, restarted, submission.OutboxID); before.State != OutboxQueued ||
		before.LocalMessageID != "" {
		t.Fatalf("restarted reaction delivery = %+v, want queued without local message", before)
	}
	if processed, err := restarted.DispatchDue(context.Background(), 4); err != nil || processed != 1 {
		t.Fatalf("DispatchDue(restarted reaction) = %d, %v; want 1, nil", processed, err)
	}
	if after := mustDelivery(t, restarted, submission.OutboxID); after.State != OutboxConfirmed ||
		after.RemoteMessageID != "remote-reaction-result" {
		t.Fatalf("reaction delivery after restart = %+v", after)
	}

	requests := sender.snapshotRequests()
	if len(requests) != 1 {
		t.Fatalf("reaction requests = %+v, want one", requests)
	}
	request := requests[0]
	outboxRow := mustOutboxItem(t, restarted, submission.OutboxID)
	if request.AccountID != "account-1" ||
		request.Conversation.RemoteID != "remote-conversation" ||
		request.Target.RemoteID != target.RemoteMessageID ||
		request.Target.AuthorID != "author@example.test" ||
		!request.Target.SentAt.Equal(time.UnixMilli(target.OccurredAtMS)) ||
		request.Target.Text != "" ||
		request.Emoji != "🎉" ||
		request.Action != bridge.ReactionSwitch ||
		request.RequestID != outboxRow.TransportRequestID {
		t.Fatalf("mapped reaction request = %+v", request)
	}
}

// A reaction to a message this account sent, which the platform has named,
// names it by that ID as this account's own: an empty author, outgoing, and
// not provisional.
func TestReactionToOwnOutgoingMessageMapsEmptyAuthor(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	sender := &scriptedReactionSender{steps: []sendStep{{
		result: bridge.SendResult{RemoteMessageID: "remote-own-reaction"},
	}}}
	registry := newScriptedRegistry("reaction-own-target", &scriptedTextSender{})
	registry.setReactionSender(sender)
	registry.setAvailable(true)
	service := newMessagingTestService(t, store, registry, clock)
	target := mustSentDispatchTarget(t, store, clock, "reaction-own-target")
	mustSendDispatchReaction(t, service, SendReactionCommand{
		CommonCommand:   testCommonCommand("reaction-own"),
		TargetMessageID: target.MessageID,
		Emoji:           "👍",
	})

	if processed, err := service.DispatchDue(context.Background(), 1); err != nil || processed != 1 {
		t.Fatalf("DispatchDue(own reaction) = %d, %v; want 1, nil", processed, err)
	}
	requests := sender.snapshotRequests()
	if len(requests) != 1 || requests[0].Target.RemoteID != target.RemoteMessageID ||
		requests[0].Target.AuthorID != "" || !requests[0].Target.Outgoing ||
		requests[0].Target.ProvisionalRemoteID {
		t.Fatalf("own-target reaction request = %+v, want the platform's ID, empty author, outgoing, not provisional", requests)
	}
}

// A reaction to a message this account has not sent yet is held before the
// transport call: the platform has no such message, so there is nothing to
// react to. It is tried again when the send is next due (here a scheduled
// send's time), and once the send has gone out the reaction goes out too,
// naming the message by its send's request ID, provisionally, until the echo
// renames it. A reaction to a received message is not held.
func TestReactionWaitsUncalledForItsTargetsSend(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	sender := &scriptedReactionSender{steps: []sendStep{{}, {}}}
	textSender := &scriptedTextSender{}
	registry := newScriptedRegistry("reaction-unsent-target", textSender)
	registry.setReactionSender(sender)
	registry.setAvailable(true)
	service := newMessagingTestService(t, store, registry, clock)

	scheduled := testCommonCommand("reaction-unsent-send")
	scheduled.NotBefore = clock.Now().Add(24 * time.Hour)
	target := mustSendText(t, service, SendTextCommand{CommonCommand: scheduled, Body: "not sent yet"})
	targetRow := mustOutboxItem(t, service, target.OutboxID)
	clock.Advance(time.Hour)
	held := mustSendDispatchReaction(t, service, SendReactionCommand{
		CommonCommand:   testCommonCommand("reaction-unsent"),
		TargetMessageID: target.LocalMessageID,
		Emoji:           "👍",
	})
	incoming := mustProjectDispatchMessage(t, store, clock, sqlite.Message{
		MessageID:       "message-unsent-incoming",
		ConversationID:  "conversation-1",
		AccountID:       "account-1",
		RemoteMessageID: targetRow.TransportRequestID + "-incoming",
		Direction:       sqlite.MessageDirectionIncoming,
		Body:            "received",
		State:           sqlite.MessageStateActive,
		OccurredAtMS:    clock.Now().Add(-time.Minute).UnixMilli(),
	})
	mustSendDispatchReaction(t, service, SendReactionCommand{
		CommonCommand:   testCommonCommand("reaction-unsent-incoming"),
		TargetMessageID: incoming.MessageID,
		Emoji:           "👍",
	})

	if processed, err := service.DispatchDue(context.Background(), 4); err != nil || processed != 2 {
		t.Fatalf("DispatchDue() = %d, %v; want the two reactions, nil", processed, err)
	}
	requests := sender.snapshotRequests()
	if len(requests) != 1 || requests[0].Target.RemoteID != incoming.RemoteMessageID ||
		requests[0].Target.Outgoing || requests[0].Target.ProvisionalRemoteID {
		t.Fatalf("reaction requests = %+v, want only the reaction to the received message, neither outgoing nor provisional", requests)
	}
	row := mustOutboxItem(t, service, held.OutboxID)
	if row.State != sqlite.OutboxNotDispatched ||
		row.ErrorClass == nil || *row.ErrorClass != string(bridge.FailureTransient) ||
		row.ErrorCode == nil || *row.ErrorCode != "reaction_target_unsent" ||
		row.TransportCalledAtMS != nil || row.AttemptCount != 0 {
		t.Fatalf("held reaction row = %+v, want not_dispatched transient reaction_target_unsent with no transport call", row)
	}
	if row.NextAttemptAtMS == nil || *row.NextAttemptAtMS != targetRow.ScheduledForMS+1 {
		t.Fatalf("held reaction next attempt = %v, want just after the send's scheduled time %d", row.NextAttemptAtMS, targetRow.ScheduledForMS)
	}
	if delivery := mustDelivery(t, service, held.OutboxID); delivery.State != OutboxNotDispatched ||
		delivery.ErrorCode != "reaction_target_unsent" {
		t.Fatalf("held reaction delivery = %+v, want not_dispatched reaction_target_unsent", delivery)
	}

	// Nothing is tried before the send is due.
	clock.Advance(22 * time.Hour)
	if processed, err := service.DispatchDue(context.Background(), 4); err != nil || processed != 0 {
		t.Fatalf("DispatchDue(before the send is due) = %d, %v; want 0, nil", processed, err)
	}

	// Once both are due the send goes out first, confirmed under its request
	// ID as a Google send is, and then the reaction names it provisionally.
	textSender.steps = []sendStep{{result: bridge.SendResult{RemoteMessageID: targetRow.TransportRequestID, EchoExpected: true}}}
	clock.Advance(time.Hour + time.Second)
	if processed, err := service.DispatchDue(context.Background(), 4); err != nil || processed != 2 {
		t.Fatalf("DispatchDue(at the send's time) = %d, %v; want the send and the reaction, nil", processed, err)
	}
	if sent := mustOutboxItem(t, service, target.OutboxID); sent.State != sqlite.OutboxConfirmed {
		t.Fatalf("send = %+v, want confirmed", sent)
	}
	requests = sender.snapshotRequests()
	if len(requests) != 2 || requests[1].Target.RemoteID != targetRow.TransportRequestID ||
		!requests[1].Target.Outgoing || !requests[1].Target.ProvisionalRemoteID {
		t.Fatalf("reaction requests = %+v, want a second naming the sent message's request ID, outgoing and provisional", requests)
	}
	if delivery := mustDelivery(t, service, held.OutboxID); delivery.State != OutboxConfirmed {
		t.Fatalf("reaction delivery after its target went out = %+v, want confirmed", delivery)
	}
}

// A held reaction is tried again just after its target's send is next due,
// and never sooner than the usual retry delay: a send in flight now, or due
// within the delay, gives the delay; a send retrying or scheduled later gives
// one millisecond after its next attempt or its scheduled time.
func TestHeldReactionRetriesWhenItsTargetsSendIsNextDue(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name     string
		schedule time.Duration
		// drive puts the send into its state and returns when the held
		// reaction should next be tried.
		drive func(t *testing.T, clock *manualClock, outbox *sqlite.OutboxRepository, row sqlite.OutboxItem) time.Time
	}{
		{name: "in flight", drive: func(t *testing.T, clock *manualClock, outbox *sqlite.OutboxRepository, row sqlite.OutboxItem) time.Time {
			mustLeaseSend(t, outbox, row, clock.Now())
			return clock.Now().Add(defaultRetryDelay)
		}},
		{name: "retrying in an hour", drive: func(t *testing.T, clock *manualClock, outbox *sqlite.OutboxRepository, row sqlite.OutboxItem) time.Time {
			retryAt := clock.Now().Add(time.Hour)
			if err := outbox.MarkNotDispatched(ctx, row.OutboxID, mustLeaseSend(t, outbox, row, clock.Now()), "rate_limited", "send_text", "scripted", retryAt); err != nil {
				t.Fatalf("MarkNotDispatched(): %v", err)
			}
			return retryAt.Add(time.Millisecond)
		}},
		{name: "retrying in a second", drive: func(t *testing.T, clock *manualClock, outbox *sqlite.OutboxRepository, row sqlite.OutboxItem) time.Time {
			if err := outbox.MarkNotDispatched(ctx, row.OutboxID, mustLeaseSend(t, outbox, row, clock.Now()), "transient", "send_text", "scripted", clock.Now().Add(time.Second)); err != nil {
				t.Fatalf("MarkNotDispatched(): %v", err)
			}
			return clock.Now().Add(defaultRetryDelay)
		}},
		{name: "scheduled for tomorrow", schedule: 24 * time.Hour, drive: func(_ *testing.T, clock *manualClock, _ *sqlite.OutboxRepository, _ sqlite.OutboxItem) time.Time {
			return clock.Now().Add(24*time.Hour + time.Millisecond)
		}},
		{name: "scheduled sooner than the delay", schedule: 3 * time.Second, drive: func(_ *testing.T, clock *manualClock, _ *sqlite.OutboxRepository, _ sqlite.OutboxItem) time.Time {
			return clock.Now().Add(defaultRetryDelay)
		}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			clock := newManualClock(messagingTestTime)
			store := openMessagingTestStore(t, clock.Now())
			sender := &scriptedReactionSender{}
			registry := newScriptedRegistry("reaction-held-retry", &scriptedTextSender{})
			registry.setReactionSender(sender)
			registry.setAvailable(true)
			service := newMessagingTestService(t, store, registry, clock)
			outbox, err := sqlite.NewOutboxRepository(store, clock.Now)
			if err != nil {
				t.Fatalf("NewOutboxRepository(): %v", err)
			}
			command := testCommonCommand("reaction-held-send")
			if test.schedule > 0 {
				command.NotBefore = clock.Now().Add(test.schedule)
			}
			target := mustSendText(t, service, SendTextCommand{CommonCommand: command, Body: "own message"})
			want := test.drive(t, clock, outbox, mustOutboxItem(t, service, target.OutboxID))
			held := mustSendDispatchReaction(t, service, SendReactionCommand{
				CommonCommand:   testCommonCommand("reaction-held"),
				TargetMessageID: target.LocalMessageID,
				Emoji:           "👍",
			})
			if processed, err := service.DispatchDue(ctx, 4); err != nil || processed != 1 {
				t.Fatalf("DispatchDue() = %d, %v; want the reaction alone, nil", processed, err)
			}
			if got := sender.requestCount(); got != 0 {
				t.Fatalf("reaction requests = %d, want the reaction held", got)
			}
			row := mustOutboxItem(t, service, held.OutboxID)
			if row.State != sqlite.OutboxNotDispatched || row.ErrorCode == nil || *row.ErrorCode != "reaction_target_unsent" ||
				row.NextAttemptAtMS == nil || *row.NextAttemptAtMS != want.UnixMilli() {
				t.Fatalf("held reaction row = %+v (next attempt %v), want not_dispatched reaction_target_unsent next tried at %d",
					row, row.NextAttemptAtMS, want.UnixMilli())
			}
		})
	}
}

// mustLeaseSend leases the send alone and returns its lease token.
func mustLeaseSend(t *testing.T, outbox *sqlite.OutboxRepository, row sqlite.OutboxItem, now time.Time) string {
	t.Helper()
	leased, err := outbox.LeaseDue(context.Background(), sqlite.LeaseRequest{
		Owner: "test", Now: now, Duration: 96 * time.Hour, Limit: 1,
	})
	if err != nil || len(leased) != 1 || leased[0].OutboxID != row.OutboxID || leased[0].LeaseToken == nil {
		t.Fatalf("LeaseDue() = %+v, %v; want the send", leased, err)
	}
	return *leased[0].LeaseToken
}

func TestDeclaredButUnwiredReactionReturnsToQueuedWithoutAttempt(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	registry := newScriptedRegistry("reaction-unwired", &scriptedTextSender{})
	registry.setReactionSender(&scriptedReactionSender{})
	service := newMessagingTestService(t, store, registry, clock)
	target := mustCanceledDispatchTarget(t, service, "reaction-unwired-target")
	submission := mustSendDispatchReaction(t, service, SendReactionCommand{
		CommonCommand:   testCommonCommand("reaction-unwired"),
		TargetMessageID: target.LocalMessageID,
		Emoji:           "👍",
	})
	registry.setReactionSender(nil)
	registry.setReactionsDeclared(true)
	registry.setAvailable(true)

	if processed, err := service.DispatchDue(context.Background(), 1); err != nil || processed != 1 {
		t.Fatalf("DispatchDue(unwired reaction) = %d, %v; want 1, nil", processed, err)
	}
	assertQueuedWithoutAttempt(t, service, submission.OutboxID)
}

func TestUnavailableReactionAccountReturnsToQueuedWithoutAttempt(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	sender := &scriptedReactionSender{}
	registry := newScriptedRegistry("reaction-account-unavailable", &scriptedTextSender{})
	registry.setReactionSender(sender)
	service := newMessagingTestService(t, store, registry, clock)
	target := mustCanceledDispatchTarget(t, service, "reaction-unavailable-target")
	submission := mustSendDispatchReaction(t, service, SendReactionCommand{
		CommonCommand:   testCommonCommand("reaction-account-unavailable"),
		TargetMessageID: target.LocalMessageID,
		Emoji:           "👍",
	})

	if processed, err := service.DispatchDue(context.Background(), 1); err != nil || processed != 1 {
		t.Fatalf("DispatchDue(unavailable reaction account) = %d, %v; want 1, nil", processed, err)
	}
	assertQueuedWithoutAttempt(t, service, submission.OutboxID)
	if got := sender.requestCount(); got != 0 {
		t.Fatalf("unavailable reaction send count = %d, want 0", got)
	}
}

func TestReactionLeaseWithoutSenderReturnsToQueuedWithoutAttempt(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	baseRegistry := newScriptedRegistry("reaction-nil-lease", &scriptedTextSender{})
	baseRegistry.setReactionSender(&scriptedReactionSender{})
	baseRegistry.setAvailable(true)
	service := newMessagingTestService(
		t,
		store,
		nilCapabilityLeaseRegistry{scriptedRegistry: baseRegistry, capability: bridge.CapabilityReactions},
		clock,
	)
	target := mustCanceledDispatchTarget(t, service, "reaction-nil-target")
	submission := mustSendDispatchReaction(t, service, SendReactionCommand{
		CommonCommand:   testCommonCommand("reaction-nil-lease"),
		TargetMessageID: target.LocalMessageID,
		Emoji:           "👍",
	})

	if processed, err := service.DispatchDue(context.Background(), 1); err != nil || processed != 1 {
		t.Fatalf("DispatchDue(nil reaction sender) = %d, %v; want 1, nil", processed, err)
	}
	assertQueuedWithoutAttempt(t, service, submission.OutboxID)
}

func TestUndeclaredReactionCapabilityIsRejected(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	sender := &scriptedReactionSender{}
	registry := newScriptedRegistry("reaction-undeclared", &scriptedTextSender{})
	registry.setReactionSender(sender)
	service := newMessagingTestService(t, store, registry, clock)
	target := mustCanceledDispatchTarget(t, service, "reaction-undeclared-target")
	submission := mustSendDispatchReaction(t, service, SendReactionCommand{
		CommonCommand:   testCommonCommand("reaction-undeclared"),
		TargetMessageID: target.LocalMessageID,
		Emoji:           "👍",
	})
	registry.setReactionSender(nil)
	registry.setReactionsDeclared(false)
	registry.setAvailable(true)

	if processed, err := service.DispatchDue(context.Background(), 1); err != nil || processed != 1 {
		t.Fatalf("DispatchDue(undeclared reaction) = %d, %v; want 1, nil", processed, err)
	}
	delivery := mustDelivery(t, service, submission.OutboxID)
	if delivery.State != OutboxRejected ||
		delivery.ErrorClass != string(bridge.FailureUnsupported) ||
		delivery.ErrorCode != "reactions_unsupported" {
		t.Fatalf("undeclared reaction delivery = %+v, want rejected/unsupported", delivery)
	}
	if row := mustOutboxItem(t, service, submission.OutboxID); row.AttemptCount != 0 {
		t.Fatalf("undeclared reaction attempt count = %d, want 0", row.AttemptCount)
	}
	if got := sender.requestCount(); got != 0 {
		t.Fatalf("undeclared reaction send count = %d, want 0", got)
	}
}

func TestDuplicateReactionReturnsSameSubmissionAndDispatchesOnce(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	sender := &scriptedReactionSender{steps: []sendStep{{
		result: bridge.SendResult{RemoteMessageID: "remote-deduplicated-reaction"},
	}}}
	registry := newScriptedRegistry("reaction-dedup", &scriptedTextSender{})
	registry.setReactionSender(sender)
	registry.setAvailable(true)
	service := newMessagingTestService(t, store, registry, clock)
	target := mustSentDispatchTarget(t, store, clock, "reaction-dedup-target")
	command := SendReactionCommand{
		CommonCommand:   testCommonCommand("reaction-dedup"),
		TargetMessageID: target.MessageID,
		Emoji:           "👍",
		Action:          bridge.ReactionAdd,
	}

	first := mustSendDispatchReaction(t, service, command)
	second := mustSendDispatchReaction(t, service, command)
	assertDeduplicatedSubmission(t, first, second)
	if processed, err := service.DispatchDue(context.Background(), 4); err != nil || processed != 1 {
		t.Fatalf("DispatchDue(duplicate reaction) = %d, %v; want 1, nil", processed, err)
	}
	if got := sender.requestCount(); got != 1 {
		t.Fatalf("duplicate reaction send count = %d, want 1", got)
	}
}

func TestReactionToggleDispatchesAddThenRemove(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	sender := &scriptedReactionSender{steps: []sendStep{
		{result: bridge.SendResult{RemoteMessageID: "remote-reaction-add"}},
		{result: bridge.SendResult{RemoteMessageID: "remote-reaction-remove"}},
	}}
	registry := newScriptedRegistry("reaction-toggle", &scriptedTextSender{})
	registry.setReactionSender(sender)
	registry.setAvailable(true)
	service := newMessagingTestService(t, store, registry, clock)
	target := mustSentDispatchTarget(t, store, clock, "reaction-toggle-target")
	mustSendDispatchReaction(t, service, SendReactionCommand{
		CommonCommand:   testCommonCommand("reaction-toggle-add"),
		TargetMessageID: target.MessageID,
		Emoji:           "👍",
		Action:          bridge.ReactionAdd,
	})
	clock.Advance(time.Millisecond)
	mustSendDispatchReaction(t, service, SendReactionCommand{
		CommonCommand:   testCommonCommand("reaction-toggle-remove"),
		TargetMessageID: target.MessageID,
		Emoji:           "👍",
		Action:          bridge.ReactionRemove,
	})

	if processed, err := service.DispatchDue(context.Background(), 2); err != nil || processed != 2 {
		t.Fatalf("DispatchDue(reaction toggle) = %d, %v; want 2, nil", processed, err)
	}
	requests := sender.snapshotRequests()
	if len(requests) != 2 || requests[0].Action != bridge.ReactionAdd ||
		requests[1].Action != bridge.ReactionRemove {
		t.Fatalf("reaction toggle requests = %+v, want add then remove", requests)
	}
}

func TestReactionSuccessWithoutRemoteMessageIDConfirms(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	sender := &scriptedReactionSender{steps: []sendStep{{}}}
	registry := newScriptedRegistry("reaction-idless-success", &scriptedTextSender{})
	registry.setReactionSender(sender)
	registry.setAvailable(true)
	service := newMessagingTestService(t, store, registry, clock)
	target := mustSentDispatchTarget(t, store, clock, "reaction-idless-target")
	submission := mustSendDispatchReaction(t, service, SendReactionCommand{
		CommonCommand:   testCommonCommand("reaction-idless-success"),
		TargetMessageID: target.MessageID,
		Emoji:           "👍",
	})

	if processed, err := service.DispatchDue(context.Background(), 1); err != nil || processed != 1 {
		t.Fatalf("DispatchDue(id-less reaction) = %d, %v; want 1, nil", processed, err)
	}
	delivery := mustDelivery(t, service, submission.OutboxID)
	row := mustOutboxItem(t, service, submission.OutboxID)
	if delivery.State != OutboxConfirmed || delivery.RemoteMessageID != "" ||
		row.ResultRemoteID != nil {
		t.Fatalf("id-less reaction delivery/row = %+v / %+v, want confirmed with NULL result", delivery, row)
	}
}

func TestReactionSuccessWithRemoteMessageIDConfirms(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	sender := &scriptedReactionSender{steps: []sendStep{{
		result: bridge.SendResult{RemoteMessageID: "remote-reaction-confirmed"},
	}}}
	registry := newScriptedRegistry("reaction-id-success", &scriptedTextSender{})
	registry.setReactionSender(sender)
	registry.setAvailable(true)
	service := newMessagingTestService(t, store, registry, clock)
	target := mustSentDispatchTarget(t, store, clock, "reaction-id-target")
	submission := mustSendDispatchReaction(t, service, SendReactionCommand{
		CommonCommand:   testCommonCommand("reaction-id-success"),
		TargetMessageID: target.MessageID,
		Emoji:           "👍",
	})

	if processed, err := service.DispatchDue(context.Background(), 1); err != nil || processed != 1 {
		t.Fatalf("DispatchDue(reaction with ID) = %d, %v; want 1, nil", processed, err)
	}
	if delivery := mustDelivery(t, service, submission.OutboxID); delivery.State != OutboxConfirmed ||
		delivery.RemoteMessageID != "remote-reaction-confirmed" {
		t.Fatalf("reaction delivery = %+v, want confirmed with remote ID", delivery)
	}
}

func TestPostCallReactionTimeoutBecomesUncertainAndIsNotRedispatched(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	sender := &scriptedReactionSender{steps: []sendStep{{err: bridge.OpError{
		Class:     bridge.FailureTransient,
		Operation: reactionOperation,
		Dispatch:  bridge.DispatchUncertain,
		Cause:     context.DeadlineExceeded,
	}}}}
	registry := newScriptedRegistry("reaction-timeout", &scriptedTextSender{})
	registry.setReactionSender(sender)
	registry.setAvailable(true)
	service := newMessagingTestService(t, store, registry, clock)
	target := mustSentDispatchTarget(t, store, clock, "reaction-timeout-target")
	submission := mustSendDispatchReaction(t, service, SendReactionCommand{
		CommonCommand:   testCommonCommand("reaction-timeout"),
		TargetMessageID: target.MessageID,
		Emoji:           "👍",
	})

	if processed, err := service.DispatchDue(context.Background(), 1); err != nil || processed != 1 {
		t.Fatalf("DispatchDue(reaction timeout) = %d, %v; want 1, nil", processed, err)
	}
	delivery := mustDelivery(t, service, submission.OutboxID)
	if delivery.State != OutboxUncertain || delivery.ErrorCode != reactionOperation ||
		delivery.Warning == "" {
		t.Fatalf("reaction timeout delivery = %+v, want uncertain", delivery)
	}
	if processed, err := service.DispatchDue(context.Background(), 1); err != nil || processed != 0 {
		t.Fatalf("DispatchDue(after reaction timeout) = %d, %v; want 0, nil", processed, err)
	}
	if got := sender.requestCount(); got != 1 {
		t.Fatalf("reaction timeout send count = %d, want 1", got)
	}
}

type nilCapabilityLeaseRegistry struct {
	*scriptedRegistry
	capability bridge.Capability
}

func (r nilCapabilityLeaseRegistry) Acquire(
	ctx context.Context,
	accountID string,
	capability bridge.Capability,
) (*bridge.DispatchLease, error) {
	if capability != r.capability {
		return r.scriptedRegistry.Acquire(ctx, accountID, capability)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &bridge.DispatchLease{
		AccountID:  accountID,
		Platform:   "nil-capability-lease",
		Generation: 7,
		Ctx:        ctx,
	}, nil
}

// mustSentDispatchTarget stores a message this account sent that the
// platform has already named (as a message sent from the phone is), so a
// reaction to it goes to the transport.
func mustSentDispatchTarget(t *testing.T, store *sqlite.Store, clock Clock, key string) sqlite.Message {
	t.Helper()
	return mustProjectDispatchMessage(t, store, clock, sqlite.Message{
		MessageID:       "message-" + key,
		ConversationID:  "conversation-1",
		AccountID:       "account-1",
		RemoteMessageID: "platform-" + key,
		Direction:       sqlite.MessageDirectionOutgoing,
		Body:            "dispatch target",
		State:           sqlite.MessageStateActive,
		OccurredAtMS:    clock.Now().Add(-time.Minute).UnixMilli(),
	})
}

// mustCanceledDispatchTarget queues a send and cancels it. A reaction to its
// message is rejected before the transport call (the message was never
// sent), so it serves tests that stop before the target is loaded.
func mustCanceledDispatchTarget(
	t *testing.T,
	service *MessageService,
	key string,
) Submission {
	t.Helper()
	target := mustSendText(t, service, SendTextCommand{
		CommonCommand: testCommonCommand(key),
		Body:          "dispatch target",
	})
	if _, err := service.Cancel(context.Background(), target.OutboxID); err != nil {
		t.Fatalf("Cancel(dispatch target): %v", err)
	}
	return target
}

func mustSendDispatchReaction(
	t *testing.T,
	service *MessageService,
	command SendReactionCommand,
) Submission {
	t.Helper()
	submission, err := service.SendReaction(context.Background(), command)
	if err != nil {
		t.Fatalf("SendReaction(): %v", err)
	}
	return submission
}

func assertQueuedWithoutAttempt(t *testing.T, service *MessageService, outboxID string) {
	t.Helper()
	row := mustOutboxItem(t, service, outboxID)
	if row.State != sqlite.OutboxQueued || row.AttemptCount != 0 || row.NextAttemptAtMS != nil {
		t.Fatalf("outbox row = %+v, want queued/attempt=0/no next-attempt", row)
	}
}

func seedDispatchIdentity(
	t *testing.T,
	store *sqlite.Store,
	identityID, canonicalValue string,
	now time.Time,
) {
	t.Helper()
	if err := store.UpsertIdentity(sqlite.Identity{
		IdentityID:     identityID,
		AccountID:      "account-1",
		Kind:           sqlite.IdentityKind("test_address"),
		CanonicalValue: canonicalValue,
		RawValue:       canonicalValue,
		DisplayName:    "Dispatch Author",
		MetadataJSON:   `{}`,
		CreatedAtMS:    now.UnixMilli(),
		UpdatedAtMS:    now.UnixMilli(),
	}); err != nil {
		t.Fatalf("UpsertIdentity(dispatch author): %v", err)
	}
}

func mustProjectDispatchMessage(
	t *testing.T,
	store *sqlite.Store,
	clock Clock,
	message sqlite.Message,
) sqlite.Message {
	t.Helper()
	repository, err := sqlite.NewMessageRepository(store, clock.Now)
	if err != nil {
		t.Fatalf("NewMessageRepository(dispatch target): %v", err)
	}
	inboxID := "inbox-" + message.MessageID
	if _, err := repository.AppendInbox(context.Background(), sqlite.InboxRecord{
		InboxID:      inboxID,
		AccountID:    message.AccountID,
		Generation:   1,
		DedupeKey:    "event-" + message.MessageID,
		Codec:        "dispatch-test",
		CodecVersion: 1,
		Payload:      []byte("dispatch target"),
	}); err != nil {
		t.Fatalf("AppendInbox(dispatch target): %v", err)
	}
	if err := repository.ProjectMessage(context.Background(), sqlite.MessageProjection{
		InboxID: inboxID,
		Message: message,
	}); err != nil {
		t.Fatalf("ProjectMessage(dispatch target): %v", err)
	}
	projected, err := repository.GetMessage(context.Background(), message.MessageID)
	if err != nil {
		t.Fatalf("GetMessage(dispatch target): %v", err)
	}
	return projected
}

// TestTargetSendProgressForEveryStateOfItsSend pins, for each outbox state of
// the send that named an own message, what a reaction to the message does:
// it is held before the transport call while the send has not gone out,
// rejected before it when the send never will (canceled, rejected), and
// otherwise sent, carrying MessageRef.ProvisionalRemoteID while the send may
// still give the message the platform's own ID: never once that is done
// (renamed to the platform's ID), and never for a message that row did not
// send. Every state that went out is checked at the moment it did, inside the
// window; TestProvisionalTargetWindowStartsWhenTheSendWentOut checks its end.
func TestTargetSendProgressForEveryStateOfItsSend(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name            string
		state           sqlite.OutboxState
		drive           func(t *testing.T, outbox *sqlite.OutboxRepository, row sqlite.OutboxItem, lease func() string)
		wantProgress    sendProgress // 0: not named by its send's request ID
		wantProvisional bool
	}{
		{name: "queued", state: sqlite.OutboxQueued, wantProgress: sendUnsent, wantProvisional: true},
		{name: "dispatching", state: sqlite.OutboxDispatching, wantProgress: sendUnsent, wantProvisional: true, drive: func(_ *testing.T, _ *sqlite.OutboxRepository, _ sqlite.OutboxItem, lease func() string) {
			lease()
		}},
		{name: "not_dispatched", state: sqlite.OutboxNotDispatched, wantProgress: sendUnsent, wantProvisional: true, drive: func(t *testing.T, outbox *sqlite.OutboxRepository, row sqlite.OutboxItem, lease func() string) {
			if err := outbox.MarkNotDispatched(ctx, row.OutboxID, lease(), "transient", "send_text", "scripted", messagingTestTime.Add(72*time.Hour)); err != nil {
				t.Fatalf("MarkNotDispatched(): %v", err)
			}
		}},
		{name: "uncertain", state: sqlite.OutboxUncertain, wantProgress: sendWentOut, wantProvisional: true, drive: func(t *testing.T, outbox *sqlite.OutboxRepository, row sqlite.OutboxItem, lease func() string) {
			token := mustMarkCalled(t, outbox, row, lease)
			if err := outbox.MarkUncertain(ctx, row.OutboxID, token, "transient", "send_text", "scripted"); err != nil {
				t.Fatalf("MarkUncertain(): %v", err)
			}
		}},
		{name: "store_failed", state: sqlite.OutboxStoreFailed, wantProgress: sendWentOut, wantProvisional: true, drive: func(t *testing.T, outbox *sqlite.OutboxRepository, row sqlite.OutboxItem, lease func() string) {
			token := mustMarkCalled(t, outbox, row, lease)
			if err := outbox.MarkStoreFailed(ctx, row.OutboxID, token, row.TransportRequestID, "scripted"); err != nil {
				t.Fatalf("MarkStoreFailed(): %v", err)
			}
		}},
		{name: "confirmed, waiting for its echo", state: sqlite.OutboxConfirmed, wantProgress: sendWentOut, wantProvisional: true, drive: func(t *testing.T, outbox *sqlite.OutboxRepository, row sqlite.OutboxItem, lease func() string) {
			token := mustMarkCalled(t, outbox, row, lease)
			if err := outbox.Confirm(ctx, sqlite.Confirmation{OutboxID: row.OutboxID, LeaseToken: token, ResultRemoteID: row.TransportRequestID}); err != nil {
				t.Fatalf("Confirm(request ID): %v", err)
			}
		}},
		{name: "confirmed under the platform's ID", state: sqlite.OutboxConfirmed, drive: func(t *testing.T, outbox *sqlite.OutboxRepository, row sqlite.OutboxItem, lease func() string) {
			token := mustMarkCalled(t, outbox, row, lease)
			if err := outbox.Confirm(ctx, sqlite.Confirmation{OutboxID: row.OutboxID, LeaseToken: token, ResultRemoteID: "platform-message-id"}); err != nil {
				t.Fatalf("Confirm(platform ID): %v", err)
			}
		}},
		{name: "rejected", state: sqlite.OutboxRejected, wantProgress: sendNeverSent, drive: func(t *testing.T, outbox *sqlite.OutboxRepository, row sqlite.OutboxItem, lease func() string) {
			if err := outbox.Reject(ctx, row.OutboxID, lease(), "unsupported", "send_text", "scripted"); err != nil {
				t.Fatalf("Reject(): %v", err)
			}
		}},
		{name: "canceled", state: sqlite.OutboxCanceled, wantProgress: sendNeverSent, drive: func(t *testing.T, outbox *sqlite.OutboxRepository, row sqlite.OutboxItem, _ func() string) {
			if err := outbox.Cancel(ctx, row.OutboxID); err != nil {
				t.Fatalf("Cancel(): %v", err)
			}
		}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			clock := newManualClock(messagingTestTime)
			store := openMessagingTestStore(t, clock.Now())
			sender := &scriptedReactionSender{}
			registry := newScriptedRegistry("provisional-target", &scriptedTextSender{})
			registry.setReactionSender(sender)
			registry.setAvailable(true)
			service := newMessagingTestService(t, store, registry, clock)
			outbox, err := sqlite.NewOutboxRepository(store, clock.Now)
			if err != nil {
				t.Fatalf("NewOutboxRepository(): %v", err)
			}
			scheduled := testCommonCommand("provisional-send")
			scheduled.NotBefore = clock.Now().Add(24 * time.Hour)
			sent := mustSendText(t, service, SendTextCommand{CommonCommand: scheduled, Body: "own message"})
			row := mustOutboxItem(t, service, sent.OutboxID)
			lease := func() string {
				t.Helper()
				return mustLeaseSend(t, outbox, row, clock.Now().Add(48*time.Hour))
			}
			if test.drive != nil {
				test.drive(t, outbox, row, lease)
			}
			if state := mustOutboxItem(t, service, sent.OutboxID).State; string(state) != string(test.state) {
				t.Fatalf("send state = %q, want %q", state, test.state)
			}
			target, err := service.messages.GetMessage(ctx, sent.LocalMessageID)
			if err != nil {
				t.Fatalf("GetMessage(own message): %v", err)
			}
			send, provisional, err := service.targetSendProgress(ctx, target)
			var progress sendProgress
			if send != nil {
				progress = send.progress
				if send.row.OutboxID != sent.OutboxID {
					t.Fatalf("targetSendProgress(%s) found send %q, want %q", test.name, send.row.OutboxID, sent.OutboxID)
				}
			}
			if err != nil || progress != test.wantProgress || provisional != test.wantProvisional {
				t.Fatalf("targetSendProgress(%s, remote %q) = progress %d, provisional %t, %v; want %d, %t",
					test.name, target.RemoteMessageID, progress, provisional, err, test.wantProgress, test.wantProvisional)
			}

			// The dispatcher acts on it: a reaction is held uncalled while
			// the send has not gone out, rejected uncalled when it never
			// will, and otherwise sent, marked provisional exactly when the
			// rule says so.
			reaction := mustSendDispatchReaction(t, service, SendReactionCommand{
				CommonCommand:   testCommonCommand("provisional-reaction"),
				TargetMessageID: sent.LocalMessageID,
				Emoji:           "👍",
			})
			if processed, err := service.DispatchDue(ctx, 4); err != nil || processed != 1 {
				t.Fatalf("DispatchDue() = %d, %v; want the reaction alone, nil", processed, err)
			}
			requests := sender.snapshotRequests()
			reactionRow := mustOutboxItem(t, service, reaction.OutboxID)
			switch test.wantProgress {
			case sendUnsent:
				if len(requests) != 0 || reactionRow.State != sqlite.OutboxNotDispatched ||
					reactionRow.ErrorCode == nil || *reactionRow.ErrorCode != "reaction_target_unsent" || reactionRow.AttemptCount != 0 {
					t.Fatalf("reaction to a %s send: requests %+v, row %+v; want it held uncalled (reaction_target_unsent)", test.name, requests, reactionRow)
				}
				return
			case sendNeverSent:
				if len(requests) != 0 || reactionRow.State != sqlite.OutboxRejected ||
					reactionRow.ErrorClass == nil || *reactionRow.ErrorClass != string(bridge.FailureUnsupported) ||
					reactionRow.ErrorCode == nil || *reactionRow.ErrorCode != "reaction_target_never_sent" || reactionRow.AttemptCount != 0 {
					t.Fatalf("reaction to a %s send: requests %+v, row %+v; want it rejected uncalled (reaction_target_never_sent)", test.name, requests, reactionRow)
				}
				return
			}
			if len(requests) != 1 || requests[0].Target.RemoteID != target.RemoteMessageID ||
				requests[0].Target.ProvisionalRemoteID != test.wantProvisional {
				t.Fatalf("reaction to a %s send: requests %+v; want one naming %q with provisional %t",
					test.name, requests, target.RemoteMessageID, test.wantProvisional)
			}
		})
	}

	// An outbox row whose request ID a message carries but which did not
	// send that message (here a reaction's own row, which has no local
	// message) never makes it provisional, outgoing or received.
	t.Run("a row that did not send the message", func(t *testing.T) {
		clock := newManualClock(messagingTestTime)
		store := openMessagingTestStore(t, clock.Now())
		registry := newScriptedRegistry("provisional-other-row", &scriptedTextSender{})
		registry.setReactionSender(&scriptedReactionSender{})
		service := newMessagingTestService(t, store, registry, clock)
		target := mustCanceledDispatchTarget(t, service, "provisional-other-target")
		for _, direction := range []sqlite.MessageDirection{sqlite.MessageDirectionOutgoing, sqlite.MessageDirectionIncoming} {
			reaction := mustSendDispatchReaction(t, service, SendReactionCommand{
				CommonCommand:   testCommonCommand("provisional-other-" + string(direction)),
				TargetMessageID: target.LocalMessageID,
				Emoji:           "👍",
			})
			reactionRow := mustOutboxItem(t, service, reaction.OutboxID)
			if reactionRow.State != sqlite.OutboxQueued || reactionRow.LocalMessageID != nil {
				t.Fatalf("reaction row = %+v, want a queued row with no local message", reactionRow)
			}
			other := mustProjectDispatchMessage(t, store, clock, sqlite.Message{
				MessageID:       "message-under-another-rows-id-" + string(direction),
				ConversationID:  "conversation-1",
				AccountID:       "account-1",
				RemoteMessageID: reactionRow.TransportRequestID,
				Direction:       direction,
				Body:            "a message under another row's request ID",
				State:           sqlite.MessageStateActive,
				OccurredAtMS:    clock.Now().Add(-time.Minute).UnixMilli(),
			})
			if send, provisional, err := service.targetSendProgress(ctx, other); err != nil || send != nil || provisional {
				t.Fatalf("targetSendProgress(%s message under a reaction row's ID) = %+v, provisional %t, %v; want no send, not provisional",
					direction, send, provisional, err)
			}
		}
	})
}

func mustMarkCalled(t *testing.T, outbox *sqlite.OutboxRepository, row sqlite.OutboxItem, lease func() string) string {
	t.Helper()
	token := lease()
	if err := outbox.MarkTransportCalled(context.Background(), sqlite.Attempt{
		OutboxID: row.OutboxID, LeaseToken: token, AttemptToken: row.OutboxID + ":" + token, StartedAt: messagingTestTime,
	}); err != nil {
		t.Fatalf("MarkTransportCalled(): %v", err)
	}
	return token
}

// A sent message still named by its send's request ID counts as awaiting its
// echo for provisionalTargetWindow after the send went out (the send row's
// updated_at_ms, when the dispatcher recorded the transport's answer), not
// after it was submitted, and not after that: a reaction one millisecond
// before the window ends is provisional, one at its end is not.
func TestProvisionalTargetWindowStartsWhenTheSendWentOut(t *testing.T) {
	ctx := context.Background()
	if provisionalTargetWindow != time.Minute {
		t.Fatalf("provisionalTargetWindow = %s, want the echo pairing horizon of one minute", provisionalTargetWindow)
	}
	wentOut := map[string]func(t *testing.T, outbox *sqlite.OutboxRepository, row sqlite.OutboxItem, token string){
		"confirmed, waiting for its echo": func(t *testing.T, outbox *sqlite.OutboxRepository, row sqlite.OutboxItem, token string) {
			if err := outbox.Confirm(ctx, sqlite.Confirmation{OutboxID: row.OutboxID, LeaseToken: token, ResultRemoteID: row.TransportRequestID}); err != nil {
				t.Fatalf("Confirm(): %v", err)
			}
		},
		"uncertain": func(t *testing.T, outbox *sqlite.OutboxRepository, row sqlite.OutboxItem, token string) {
			if err := outbox.MarkUncertain(ctx, row.OutboxID, token, "transient", "send_text", "scripted"); err != nil {
				t.Fatalf("MarkUncertain(): %v", err)
			}
		},
		"store_failed": func(t *testing.T, outbox *sqlite.OutboxRepository, row sqlite.OutboxItem, token string) {
			if err := outbox.MarkStoreFailed(ctx, row.OutboxID, token, row.TransportRequestID, "scripted"); err != nil {
				t.Fatalf("MarkStoreFailed(): %v", err)
			}
		},
	}
	for name, settle := range wentOut {
		t.Run(name, func(t *testing.T) {
			clock := newManualClock(messagingTestTime)
			store := openMessagingTestStore(t, clock.Now())
			sender := &scriptedReactionSender{}
			registry := newScriptedRegistry("provisional-window", &scriptedTextSender{})
			registry.setReactionSender(sender)
			registry.setAvailable(true)
			service := newMessagingTestService(t, store, registry, clock)
			outbox, err := sqlite.NewOutboxRepository(store, clock.Now)
			if err != nil {
				t.Fatalf("NewOutboxRepository(): %v", err)
			}
			sent := mustSendText(t, service, SendTextCommand{CommonCommand: testCommonCommand("window-send"), Body: "own message"})
			// The send goes out ten minutes after it was submitted, as a
			// retried or scheduled send does.
			clock.Advance(10 * time.Minute)
			row := mustOutboxItem(t, service, sent.OutboxID)
			settle(t, outbox, row, mustMarkCalledAt(t, outbox, row, clock.Now()))
			wentOutAt := time.UnixMilli(mustOutboxItem(t, service, sent.OutboxID).UpdatedAtMS)
			if !wentOutAt.Equal(clock.Now()) {
				t.Fatalf("send updated at %s, want when it went out, %s", wentOutAt, clock.Now())
			}

			for i, at := range []struct {
				offset          time.Duration
				wantProvisional bool
			}{
				{offset: 0, wantProvisional: true},
				{offset: provisionalTargetWindow - time.Millisecond, wantProvisional: true},
				{offset: provisionalTargetWindow, wantProvisional: false},
				{offset: time.Hour, wantProvisional: false},
			} {
				clock.Advance(wentOutAt.Add(at.offset).Sub(clock.Now()))
				mustSendDispatchReaction(t, service, SendReactionCommand{
					CommonCommand:   testCommonCommand(fmt.Sprintf("window-reaction-%d", i)),
					TargetMessageID: sent.LocalMessageID,
					Emoji:           "👍",
				})
				if processed, err := service.DispatchDue(ctx, 4); err != nil || processed != 1 {
					t.Fatalf("DispatchDue(+%s) = %d, %v; want the reaction, nil", at.offset, processed, err)
				}
				requests := sender.snapshotRequests()
				if len(requests) != i+1 || requests[i].Target.RemoteID != row.TransportRequestID ||
					requests[i].Target.ProvisionalRemoteID != at.wantProvisional {
					t.Fatalf("reaction %s after the send went out: requests %+v; want the %dth naming %q with provisional %t",
						at.offset, requests, i+1, row.TransportRequestID, at.wantProvisional)
				}
			}
		})
	}
}

// mustMarkCalledAt leases the send alone and marks its transport call.
func mustMarkCalledAt(t *testing.T, outbox *sqlite.OutboxRepository, row sqlite.OutboxItem, now time.Time) string {
	t.Helper()
	token := mustLeaseSend(t, outbox, row, now)
	if err := outbox.MarkTransportCalled(context.Background(), sqlite.Attempt{
		OutboxID: row.OutboxID, LeaseToken: token, AttemptToken: row.OutboxID + ":" + token, StartedAt: now,
	}); err != nil {
		t.Fatalf("MarkTransportCalled(): %v", err)
	}
	return token
}

// TestReactionToAnOwnMessageFollowsItsSend is the property behind the hold,
// the never-sent rejection and the window, against a reference model: for any
// state of the send that named an own message, any delay between submitting
// the send and its last change, and any time after that change, a reaction to
// the message
//
//   - is never handed to the transport while the send has not gone out, and
//     is next tried just after the send is next due (but not within the retry
//     delay); a dispatcher that then reaches both sends the message first and
//     the reaction right after it, provisionally;
//   - is rejected without a transport call when the send was canceled or
//     rejected;
//   - otherwise goes out, marked provisional exactly when the message still
//     carries the request ID of a send that went out less than
//     provisionalTargetWindow ago.
func TestReactionToAnOwnMessageFollowsItsSend(t *testing.T) {
	ctx := context.Background()
	states := []string{
		"queued", "dispatching", "not_dispatched",
		"uncertain", "store_failed", "confirmed under its request ID",
		"confirmed under the platform's ID", "rejected", "canceled",
	}
	window := provisionalTargetWindow.Milliseconds()
	property := func(seed int64) bool {
		r := rand.New(rand.NewSource(seed))
		state := states[r.Intn(len(states))]
		// How long after the send's last change the reaction is tried,
		// often right at the window's end.
		var afterMS int64
		switch r.Intn(4) {
		case 0:
			afterMS = window - 1 + r.Int63n(3)
		default:
			afterMS = r.Int63n(3 * window)
		}
		// How long the send waited between submission and its last change.
		waited := time.Duration(r.Int63n(int64(10 * time.Minute)))

		clock := newManualClock(messagingTestTime)
		store := openMessagingTestStore(t, clock.Now())
		sender := &scriptedReactionSender{}
		text := &scriptedTextSender{}
		registry := newScriptedRegistry("reaction-follows-send", text)
		registry.setReactionSender(sender)
		registry.setAvailable(true)
		service := newMessagingTestService(t, store, registry, clock)
		outbox, err := sqlite.NewOutboxRepository(store, clock.Now)
		if err != nil {
			t.Errorf("seed %d NewOutboxRepository(): %v", seed, err)
			return false
		}
		t0 := clock.Now().Add(waited)
		attemptAt := t0.Add(time.Duration(afterMS) * time.Millisecond)
		command := testCommonCommand("follows-send")
		// A send that has not gone out must not be due when the reaction
		// is tried, or the dispatcher would send it first.
		var scheduledFor time.Time
		if state == "queued" {
			scheduledFor = attemptAt.Add(time.Millisecond + time.Duration(r.Int63n(int64(48*time.Hour))))
			command.NotBefore = scheduledFor
		}
		sent := mustSendText(t, service, SendTextCommand{CommonCommand: command, Body: "own message"})
		row := mustOutboxItem(t, service, sent.OutboxID)
		clock.Advance(waited)
		var retryAt time.Time
		switch state {
		case "queued":
		case "dispatching":
			mustLeaseSend(t, outbox, row, t0)
		case "not_dispatched":
			retryAt = attemptAt.Add(time.Millisecond + time.Duration(r.Int63n(int64(2*time.Hour))))
			err = outbox.MarkNotDispatched(ctx, row.OutboxID, mustLeaseSend(t, outbox, row, t0), "transient", "send_text", "scripted", retryAt)
		case "uncertain":
			err = outbox.MarkUncertain(ctx, row.OutboxID, mustMarkCalledAt(t, outbox, row, t0), "transient", "send_text", "scripted")
		case "store_failed":
			err = outbox.MarkStoreFailed(ctx, row.OutboxID, mustMarkCalledAt(t, outbox, row, t0), row.TransportRequestID, "scripted")
		case "confirmed under its request ID":
			err = outbox.Confirm(ctx, sqlite.Confirmation{OutboxID: row.OutboxID, LeaseToken: mustMarkCalledAt(t, outbox, row, t0), ResultRemoteID: row.TransportRequestID})
		case "confirmed under the platform's ID":
			err = outbox.Confirm(ctx, sqlite.Confirmation{OutboxID: row.OutboxID, LeaseToken: mustMarkCalledAt(t, outbox, row, t0), ResultRemoteID: "platform-message-id"})
		case "rejected":
			err = outbox.Reject(ctx, row.OutboxID, mustLeaseSend(t, outbox, row, t0), "unsupported", "send_text", "scripted")
		case "canceled":
			err = outbox.Cancel(ctx, row.OutboxID)
		}
		if err != nil {
			t.Errorf("seed %d drive send to %s: %v", seed, state, err)
			return false
		}

		clock.Advance(attemptAt.Sub(clock.Now()))
		reaction := mustSendDispatchReaction(t, service, SendReactionCommand{
			CommonCommand:   testCommonCommand("follows-reaction"),
			TargetMessageID: sent.LocalMessageID,
			Emoji:           "👍",
		})
		if processed, err := service.DispatchDue(ctx, 4); err != nil || processed != 1 {
			t.Errorf("seed %d (%s, +%d ms) DispatchDue() = %d, %v; want the reaction alone", seed, state, afterMS, processed, err)
			return false
		}

		// The reference model.
		requests := sender.snapshotRequests()
		reactionRow := mustOutboxItem(t, service, reaction.OutboxID)
		switch state {
		case "queued", "dispatching", "not_dispatched":
			wantNext := attemptAt.Add(defaultRetryDelay)
			for _, due := range []time.Time{scheduledFor, retryAt} {
				if after := due.Add(time.Millisecond); after.After(wantNext) {
					wantNext = after
				}
			}
			if len(requests) != 0 || reactionRow.State != sqlite.OutboxNotDispatched || reactionRow.AttemptCount != 0 ||
				reactionRow.TransportCalledAtMS != nil || reactionRow.ErrorCode == nil || *reactionRow.ErrorCode != "reaction_target_unsent" ||
				reactionRow.NextAttemptAtMS == nil || *reactionRow.NextAttemptAtMS != wantNext.UnixMilli() {
				t.Errorf("seed %d (%s, +%d ms): requests %+v, reaction row %+v; want it held uncalled until %d",
					seed, state, afterMS, requests, reactionRow, wantNext.UnixMilli())
				return false
			}
			if state == "dispatching" {
				// The send's lease outlives the test; nothing more is due.
				return true
			}
			// When the reaction is next due the send is due too. The send
			// goes out first, confirmed under its request ID as a Google
			// send is, and the reaction follows it, provisionally.
			text.mu.Lock()
			text.steps = []sendStep{{result: bridge.SendResult{RemoteMessageID: row.TransportRequestID, EchoExpected: true}}}
			text.mu.Unlock()
			clock.Advance(wantNext.Sub(clock.Now()))
			if processed, err := service.DispatchDue(ctx, 4); err != nil || processed != 2 {
				t.Errorf("seed %d (%s): DispatchDue(when both are due) = %d, %v; want the send and the reaction", seed, state, processed, err)
				return false
			}
			requests = sender.snapshotRequests()
			if sendRow := mustOutboxItem(t, service, sent.OutboxID); sendRow.State != sqlite.OutboxConfirmed ||
				len(requests) != 1 || requests[0].Target.RemoteID != row.TransportRequestID || !requests[0].Target.ProvisionalRemoteID {
				t.Errorf("seed %d (%s): send %s, requests %+v; want the send confirmed and then one provisional reaction",
					seed, state, sendRow.State, requests)
				return false
			}
			return true
		case "rejected", "canceled":
			if len(requests) != 0 || reactionRow.State != sqlite.OutboxRejected || reactionRow.AttemptCount != 0 ||
				reactionRow.ErrorCode == nil || *reactionRow.ErrorCode != "reaction_target_never_sent" {
				t.Errorf("seed %d (%s): requests %+v, reaction row %+v; want it rejected uncalled (reaction_target_never_sent)",
					seed, state, requests, reactionRow)
				return false
			}
			return true
		}
		wantRemoteID := row.TransportRequestID
		if state == "confirmed under the platform's ID" {
			wantRemoteID = "platform-message-id"
		}
		wentOut := state != "confirmed under the platform's ID"
		wantProvisional := wentOut && afterMS < window
		if len(requests) != 1 || requests[0].Target.RemoteID != wantRemoteID ||
			requests[0].Target.ProvisionalRemoteID != wantProvisional {
			t.Errorf("seed %d (%s, +%d ms): requests %+v; want one naming %q with provisional %t",
				seed, state, afterMS, requests, wantRemoteID, wantProvisional)
			return false
		}
		return true
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 150, Rand: rand.New(rand.NewSource(20261021))}); err != nil {
		t.Fatal(err)
	}
}

// Canceling the send a reaction is held for makes the reaction due at once,
// and it is rejected without a transport call: the message was never sent.
// Without the wake it would wait for the canceled send's scheduled time.
func TestCancelingAHeldReactionsSendRejectsItAtOnce(t *testing.T) {
	ctx := context.Background()
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	sender := &scriptedReactionSender{}
	registry := newScriptedRegistry("reaction-canceled-send", &scriptedTextSender{})
	registry.setReactionSender(sender)
	registry.setAvailable(true)
	service := newMessagingTestService(t, store, registry, clock)
	scheduled := testCommonCommand("canceled-send")
	scheduled.NotBefore = clock.Now().Add(24 * time.Hour)
	target := mustSendText(t, service, SendTextCommand{CommonCommand: scheduled, Body: "never sent"})
	held := mustSendDispatchReaction(t, service, SendReactionCommand{
		CommonCommand:   testCommonCommand("canceled-send-reaction"),
		TargetMessageID: target.LocalMessageID,
		Emoji:           "👍",
	})
	if processed, err := service.DispatchDue(ctx, 4); err != nil || processed != 1 {
		t.Fatalf("DispatchDue(hold) = %d, %v; want the reaction held", processed, err)
	}

	clock.Advance(time.Hour)
	select {
	case <-service.wake:
	default:
	}
	if _, err := service.Cancel(ctx, target.OutboxID); err != nil {
		t.Fatalf("Cancel(send): %v", err)
	}
	// The running dispatcher is woken, so it need not sleep until the
	// canceled send's old time.
	select {
	case <-service.wake:
	default:
		t.Fatalf("Cancel(send) did not wake the dispatcher")
	}
	clock.Advance(time.Millisecond)
	if processed, err := service.DispatchDue(ctx, 4); err != nil || processed != 1 {
		t.Fatalf("DispatchDue(after the cancel) = %d, %v; want the reaction at once", processed, err)
	}
	row := mustOutboxItem(t, service, held.OutboxID)
	if sender.requestCount() != 0 || row.State != sqlite.OutboxRejected || row.AttemptCount != 0 ||
		row.ErrorCode == nil || *row.ErrorCode != "reaction_target_never_sent" {
		t.Fatalf("reaction after its send was canceled: %d requests, row %+v; want it rejected uncalled (reaction_target_never_sent)",
			sender.requestCount(), row)
	}
}

// Retrying the send a reaction is held for, before the send's own next
// attempt, makes the reaction due just after it: the send goes out first and
// the reaction follows, instead of waiting for the send's old retry time.
func TestRetryingAHeldReactionsSendEarlyBringsTheReactionWithIt(t *testing.T) {
	ctx := context.Background()
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	sender := &scriptedReactionSender{}
	text := &scriptedTextSender{}
	registry := newScriptedRegistry("reaction-retried-send", text)
	registry.setReactionSender(sender)
	registry.setAvailable(true)
	service := newMessagingTestService(t, store, registry, clock)
	outbox, err := sqlite.NewOutboxRepository(store, clock.Now)
	if err != nil {
		t.Fatalf("NewOutboxRepository(): %v", err)
	}
	target := mustSendText(t, service, SendTextCommand{CommonCommand: testCommonCommand("retried-send"), Body: "rate limited"})
	row := mustOutboxItem(t, service, target.OutboxID)
	if err := outbox.MarkNotDispatched(ctx, row.OutboxID, mustLeaseSend(t, outbox, row, clock.Now()),
		"rate_limited", "send_text", "scripted", clock.Now().Add(time.Hour)); err != nil {
		t.Fatalf("MarkNotDispatched(): %v", err)
	}
	held := mustSendDispatchReaction(t, service, SendReactionCommand{
		CommonCommand:   testCommonCommand("retried-send-reaction"),
		TargetMessageID: target.LocalMessageID,
		Emoji:           "👍",
	})
	if processed, err := service.DispatchDue(ctx, 4); err != nil || processed != 1 {
		t.Fatalf("DispatchDue(hold) = %d, %v; want the reaction held", processed, err)
	}
	if next := mustOutboxItem(t, service, held.OutboxID).NextAttemptAtMS; next == nil || *next != clock.Now().Add(time.Hour).UnixMilli()+1 {
		t.Fatalf("held reaction next attempt = %v, want just after the send's retry in an hour", next)
	}

	clock.Advance(time.Minute)
	if _, err := service.RetryNotDispatched(ctx, target.OutboxID); err != nil {
		t.Fatalf("RetryNotDispatched(send): %v", err)
	}
	text.steps = []sendStep{{result: bridge.SendResult{RemoteMessageID: "platform-retried"}}}
	clock.Advance(time.Millisecond)
	if processed, err := service.DispatchDue(ctx, 4); err != nil || processed != 2 {
		t.Fatalf("DispatchDue(after the retry) = %d, %v; want the send and then the reaction", processed, err)
	}
	requests := sender.snapshotRequests()
	if len(requests) != 1 || requests[0].Target.RemoteID != "platform-retried" {
		t.Fatalf("reaction requests = %+v, want one naming the sent message's platform ID", requests)
	}
}

// A cancel or early retry of the send that lands between the dispatcher
// reading a reaction's target and recording the hold finds the reaction still
// dispatching, so its own wake passes the reaction by. The dispatcher reads
// the send again after the hold and, finding it changed, makes the reaction
// due at once instead of at the send's old time.
func TestASendChangedWhileItsReactionIsBeingHeldWakesTheReaction(t *testing.T) {
	cases := []struct {
		name string
		// change lands in the gap; then is what the reaction does when it
		// is next due, a millisecond later.
		change func(t *testing.T, service *MessageService, send Submission)
		then   func(t *testing.T, service *MessageService, sender *scriptedReactionSender, held Submission)
	}{
		{
			name: "canceled",
			change: func(t *testing.T, service *MessageService, send Submission) {
				if _, err := service.Cancel(context.Background(), send.OutboxID); err != nil {
					t.Fatalf("Cancel(send): %v", err)
				}
			},
			then: func(t *testing.T, service *MessageService, sender *scriptedReactionSender, held Submission) {
				if processed, err := service.DispatchDue(context.Background(), 4); err != nil || processed != 1 {
					t.Fatalf("DispatchDue() = %d, %v; want the reaction", processed, err)
				}
				if row := mustOutboxItem(t, service, held.OutboxID); sender.requestCount() != 0 || row.State != sqlite.OutboxRejected ||
					row.ErrorCode == nil || *row.ErrorCode != "reaction_target_never_sent" {
					t.Fatalf("reaction after its send was canceled in the gap = %+v, want rejected uncalled", row)
				}
			},
		},
		{
			name: "retried early",
			change: func(t *testing.T, service *MessageService, send Submission) {
				if _, err := service.RetryNotDispatched(context.Background(), send.OutboxID); err != nil {
					t.Fatalf("RetryNotDispatched(send): %v", err)
				}
			},
			then: func(t *testing.T, service *MessageService, sender *scriptedReactionSender, held Submission) {
				if processed, err := service.DispatchDue(context.Background(), 4); err != nil || processed != 2 {
					t.Fatalf("DispatchDue() = %d, %v; want the send and then the reaction", processed, err)
				}
				if requests := sender.snapshotRequests(); len(requests) != 1 || requests[0].Target.RemoteID != "platform-gap" {
					t.Fatalf("reaction requests = %+v, want one naming the sent message", requests)
				}
			},
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			clock := newManualClock(messagingTestTime)
			store := openMessagingTestStore(t, clock.Now())
			sender := &scriptedReactionSender{}
			text := &scriptedTextSender{}
			registry := newScriptedRegistry("reaction-gap", text)
			registry.setReactionSender(sender)
			registry.setAvailable(true)
			service := newMessagingTestService(t, store, registry, clock)
			outbox, err := sqlite.NewOutboxRepository(store, clock.Now)
			if err != nil {
				t.Fatalf("NewOutboxRepository(): %v", err)
			}
			// The send is retrying in an hour, so it can be canceled or
			// retried early.
			send := mustSendText(t, service, SendTextCommand{CommonCommand: testCommonCommand("gap-send"), Body: "rate limited"})
			row := mustOutboxItem(t, service, send.OutboxID)
			if err := outbox.MarkNotDispatched(ctx, row.OutboxID, mustLeaseSend(t, outbox, row, clock.Now()),
				"rate_limited", "send_text", "scripted", clock.Now().Add(time.Hour)); err != nil {
				t.Fatalf("MarkNotDispatched(): %v", err)
			}
			held := mustSendDispatchReaction(t, service, SendReactionCommand{
				CommonCommand:   testCommonCommand("gap-reaction"),
				TargetMessageID: send.LocalMessageID,
				Emoji:           "👍",
			})
			clock.Advance(time.Second)
			afterReactionTargetRead = func() {
				afterReactionTargetRead = func() {}
				test.change(t, service, send)
			}
			t.Cleanup(func() { afterReactionTargetRead = func() {} })
			if processed, err := service.DispatchDue(ctx, 4); err != nil || processed != 1 {
				t.Fatalf("DispatchDue(hold) = %d, %v; want the reaction held", processed, err)
			}
			if next := mustOutboxItem(t, service, held.OutboxID).NextAttemptAtMS; next == nil || *next != clock.Now().UnixMilli()+1 {
				t.Fatalf("held reaction next attempt = %v, want 1 ms from now (%d), not the send's old time", next, clock.Now().UnixMilli()+1)
			}
			text.steps = []sendStep{{result: bridge.SendResult{RemoteMessageID: "platform-gap"}}}
			clock.Advance(time.Millisecond)
			test.then(t, service, sender, held)
		})
	}
}
