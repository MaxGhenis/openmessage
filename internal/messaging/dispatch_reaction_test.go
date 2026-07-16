package messaging

import (
	"context"
	"testing"
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
	target := mustCanceledDispatchTarget(t, service, "reaction-own-target")
	targetRow := mustOutboxItem(t, service, target.OutboxID)
	mustSendDispatchReaction(t, service, SendReactionCommand{
		CommonCommand:   testCommonCommand("reaction-own"),
		TargetMessageID: target.LocalMessageID,
		Emoji:           "👍",
	})

	if processed, err := service.DispatchDue(context.Background(), 1); err != nil || processed != 1 {
		t.Fatalf("DispatchDue(own reaction) = %d, %v; want 1, nil", processed, err)
	}
	requests := sender.snapshotRequests()
	if len(requests) != 1 || requests[0].Target.RemoteID != targetRow.TransportRequestID ||
		requests[0].Target.AuthorID != "" {
		t.Fatalf("own-target reaction request = %+v, want request ID target and empty author", requests)
	}
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
	target := mustCanceledDispatchTarget(t, service, "reaction-dedup-target")
	command := SendReactionCommand{
		CommonCommand:   testCommonCommand("reaction-dedup"),
		TargetMessageID: target.LocalMessageID,
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
	target := mustCanceledDispatchTarget(t, service, "reaction-toggle-target")
	mustSendDispatchReaction(t, service, SendReactionCommand{
		CommonCommand:   testCommonCommand("reaction-toggle-add"),
		TargetMessageID: target.LocalMessageID,
		Emoji:           "👍",
		Action:          bridge.ReactionAdd,
	})
	clock.Advance(time.Millisecond)
	mustSendDispatchReaction(t, service, SendReactionCommand{
		CommonCommand:   testCommonCommand("reaction-toggle-remove"),
		TargetMessageID: target.LocalMessageID,
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
	target := mustCanceledDispatchTarget(t, service, "reaction-idless-target")
	submission := mustSendDispatchReaction(t, service, SendReactionCommand{
		CommonCommand:   testCommonCommand("reaction-idless-success"),
		TargetMessageID: target.LocalMessageID,
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
	target := mustCanceledDispatchTarget(t, service, "reaction-id-target")
	submission := mustSendDispatchReaction(t, service, SendReactionCommand{
		CommonCommand:   testCommonCommand("reaction-id-success"),
		TargetMessageID: target.LocalMessageID,
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
	target := mustCanceledDispatchTarget(t, service, "reaction-timeout-target")
	submission := mustSendDispatchReaction(t, service, SendReactionCommand{
		CommonCommand:   testCommonCommand("reaction-timeout"),
		TargetMessageID: target.LocalMessageID,
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
