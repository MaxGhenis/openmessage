package messaging

import (
	"context"
	"testing"
	"time"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

func TestQueuedReadSurvivesRestartAndMapsRequest(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	seedDispatchDevice(t, store, "device-read-restart", clock.Now())
	identityID := "identity-read-author"
	seedDispatchIdentity(t, store, identityID, "reader-target-author@example.test", clock.Now())
	target := mustProjectDispatchMessage(t, store, clock, sqlite.Message{
		MessageID:        "message-read-target",
		ConversationID:   "conversation-1",
		AccountID:        "account-1",
		RemoteMessageID:  "remote-read-target",
		SenderIdentityID: &identityID,
		Direction:        sqlite.MessageDirectionIncoming,
		Body:             "read target body is not copied",
		State:            sqlite.MessageStateActive,
		OccurredAtMS:     clock.Now().Add(-3 * time.Minute).UnixMilli(),
	})
	readAt := clock.Now().Add(-time.Minute)

	beforeRestart := newScriptedRegistry("read-before-restart", &scriptedTextSender{})
	beforeRestart.setReadReceiptSender(&scriptedReadReceiptSender{})
	first := newMessagingTestService(t, store, beforeRestart, clock)
	submission := mustMarkDispatchRead(t, first, MarkReadCommand{
		CommonCommand:     testCommonCommand("read-restart"),
		DeviceID:          "device-read-restart",
		LastReadMessageID: target.MessageID,
		LastReadAt:        readAt,
	})

	sender := &scriptedReadReceiptSender{steps: []readReceiptStep{{}}}
	afterRestart := newScriptedRegistry("read-after-restart", &scriptedTextSender{})
	afterRestart.setReadReceiptSender(sender)
	afterRestart.setAvailable(true)
	restarted := newMessagingTestService(t, store, afterRestart, clock)

	if before := mustDelivery(t, restarted, submission.OutboxID); before.State != OutboxQueued ||
		before.LocalMessageID != "" {
		t.Fatalf("restarted read delivery = %+v, want queued without local message", before)
	}
	if processed, err := restarted.DispatchDue(context.Background(), 4); err != nil || processed != 1 {
		t.Fatalf("DispatchDue(restarted read) = %d, %v; want 1, nil", processed, err)
	}
	if after := mustDelivery(t, restarted, submission.OutboxID); after.State != OutboxConfirmed ||
		after.RemoteMessageID != "" {
		t.Fatalf("read delivery after restart = %+v", after)
	}

	requests := sender.snapshotRequests()
	if len(requests) != 1 {
		t.Fatalf("read requests = %+v, want one", requests)
	}
	request := requests[0]
	if request.AccountID != "account-1" ||
		request.Conversation.RemoteID != "remote-conversation" ||
		len(request.Messages) != 1 ||
		request.Messages[0].RemoteID != target.RemoteMessageID ||
		request.Messages[0].AuthorID != "reader-target-author@example.test" ||
		!request.Messages[0].SentAt.Equal(time.UnixMilli(target.OccurredAtMS)) ||
		request.Messages[0].Text != "" ||
		!request.ReadAt.Equal(readAt) {
		t.Fatalf("mapped read request = %+v", request)
	}
}

func TestDeclaredButUnwiredReadReturnsToQueuedWithoutAttempt(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	seedDispatchDevice(t, store, "device-read-unwired", clock.Now())
	registry := newScriptedRegistry("read-unwired", &scriptedTextSender{})
	registry.setReadReceiptSender(&scriptedReadReceiptSender{})
	service := newMessagingTestService(t, store, registry, clock)
	target := mustCanceledDispatchTarget(t, service, "read-unwired-target")
	submission := mustMarkDispatchRead(t, service, MarkReadCommand{
		CommonCommand:     testCommonCommand("read-unwired"),
		DeviceID:          "device-read-unwired",
		LastReadMessageID: target.LocalMessageID,
	})
	registry.setReadReceiptSender(nil)
	registry.setReadReceiptsDeclared(true)
	registry.setAvailable(true)

	if processed, err := service.DispatchDue(context.Background(), 1); err != nil || processed != 1 {
		t.Fatalf("DispatchDue(unwired read) = %d, %v; want 1, nil", processed, err)
	}
	assertQueuedWithoutAttempt(t, service, submission.OutboxID)
}

func TestUnavailableReadAccountReturnsToQueuedWithoutAttempt(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	seedDispatchDevice(t, store, "device-read-unavailable", clock.Now())
	sender := &scriptedReadReceiptSender{}
	registry := newScriptedRegistry("read-account-unavailable", &scriptedTextSender{})
	registry.setReadReceiptSender(sender)
	service := newMessagingTestService(t, store, registry, clock)
	target := mustCanceledDispatchTarget(t, service, "read-unavailable-target")
	submission := mustMarkDispatchRead(t, service, MarkReadCommand{
		CommonCommand:     testCommonCommand("read-account-unavailable"),
		DeviceID:          "device-read-unavailable",
		LastReadMessageID: target.LocalMessageID,
	})

	if processed, err := service.DispatchDue(context.Background(), 1); err != nil || processed != 1 {
		t.Fatalf("DispatchDue(unavailable read account) = %d, %v; want 1, nil", processed, err)
	}
	assertQueuedWithoutAttempt(t, service, submission.OutboxID)
	if got := sender.requestCount(); got != 0 {
		t.Fatalf("unavailable read send count = %d, want 0", got)
	}
}

func TestReadLeaseWithoutSenderReturnsToQueuedWithoutAttempt(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	seedDispatchDevice(t, store, "device-read-nil-lease", clock.Now())
	baseRegistry := newScriptedRegistry("read-nil-lease", &scriptedTextSender{})
	baseRegistry.setReadReceiptSender(&scriptedReadReceiptSender{})
	baseRegistry.setAvailable(true)
	service := newMessagingTestService(
		t,
		store,
		nilCapabilityLeaseRegistry{
			scriptedRegistry: baseRegistry,
			capability:       bridge.CapabilityReadReceipts,
		},
		clock,
	)
	target := mustCanceledDispatchTarget(t, service, "read-nil-target")
	submission := mustMarkDispatchRead(t, service, MarkReadCommand{
		CommonCommand:     testCommonCommand("read-nil-lease"),
		DeviceID:          "device-read-nil-lease",
		LastReadMessageID: target.LocalMessageID,
	})

	if processed, err := service.DispatchDue(context.Background(), 1); err != nil || processed != 1 {
		t.Fatalf("DispatchDue(nil read sender) = %d, %v; want 1, nil", processed, err)
	}
	assertQueuedWithoutAttempt(t, service, submission.OutboxID)
}

func TestUndeclaredReadCapabilityIsRejected(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	seedDispatchDevice(t, store, "device-read-undeclared", clock.Now())
	sender := &scriptedReadReceiptSender{}
	registry := newScriptedRegistry("read-undeclared", &scriptedTextSender{})
	registry.setReadReceiptSender(sender)
	service := newMessagingTestService(t, store, registry, clock)
	target := mustCanceledDispatchTarget(t, service, "read-undeclared-target")
	submission := mustMarkDispatchRead(t, service, MarkReadCommand{
		CommonCommand:     testCommonCommand("read-undeclared"),
		DeviceID:          "device-read-undeclared",
		LastReadMessageID: target.LocalMessageID,
	})
	registry.setReadReceiptSender(nil)
	registry.setReadReceiptsDeclared(false)
	registry.setAvailable(true)

	if processed, err := service.DispatchDue(context.Background(), 1); err != nil || processed != 1 {
		t.Fatalf("DispatchDue(undeclared read) = %d, %v; want 1, nil", processed, err)
	}
	delivery := mustDelivery(t, service, submission.OutboxID)
	if delivery.State != OutboxRejected ||
		delivery.ErrorClass != string(bridge.FailureUnsupported) ||
		delivery.ErrorCode != "read_receipts_unsupported" {
		t.Fatalf("undeclared read delivery = %+v, want rejected/unsupported", delivery)
	}
	if row := mustOutboxItem(t, service, submission.OutboxID); row.AttemptCount != 0 {
		t.Fatalf("undeclared read attempt count = %d, want 0", row.AttemptCount)
	}
	if got := sender.requestCount(); got != 0 {
		t.Fatalf("undeclared read send count = %d, want 0", got)
	}
}

func TestDuplicateReadReturnsSameSubmissionAndDispatchesOnce(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	seedDispatchDevice(t, store, "device-read-dedup", clock.Now())
	sender := &scriptedReadReceiptSender{}
	registry := newScriptedRegistry("read-dedup", &scriptedTextSender{})
	registry.setReadReceiptSender(sender)
	registry.setAvailable(true)
	service := newMessagingTestService(t, store, registry, clock)
	target := mustCanceledDispatchTarget(t, service, "read-dedup-target")
	command := MarkReadCommand{
		CommonCommand:     testCommonCommand("read-dedup"),
		DeviceID:          "device-read-dedup",
		LastReadMessageID: target.LocalMessageID,
		LastReadAt:        clock.Now(),
	}

	first := mustMarkDispatchRead(t, service, command)
	command.LastReadAt = clock.Now().Add(time.Minute)
	second := mustMarkDispatchRead(t, service, command)
	assertDeduplicatedSubmission(t, first, second)
	if processed, err := service.DispatchDue(context.Background(), 4); err != nil || processed != 1 {
		t.Fatalf("DispatchDue(duplicate read) = %d, %v; want 1, nil", processed, err)
	}
	if got := sender.requestCount(); got != 1 {
		t.Fatalf("duplicate read send count = %d, want 1", got)
	}
	requests := sender.snapshotRequests()
	if len(requests) != 1 || !requests[0].ReadAt.Equal(messagingTestTime) {
		t.Fatalf("deduplicated read request = %+v, want original ReadAt", requests)
	}
}

func TestReadConfirmHasNoRemoteMessageID(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	seedDispatchDevice(t, store, "device-read-confirm", clock.Now())
	sender := &scriptedReadReceiptSender{}
	registry := newScriptedRegistry("read-confirm", &scriptedTextSender{})
	registry.setReadReceiptSender(sender)
	registry.setAvailable(true)
	service := newMessagingTestService(t, store, registry, clock)
	target := mustCanceledDispatchTarget(t, service, "read-confirm-target")
	submission := mustMarkDispatchRead(t, service, MarkReadCommand{
		CommonCommand:     testCommonCommand("read-confirm"),
		DeviceID:          "device-read-confirm",
		LastReadMessageID: target.LocalMessageID,
	})

	if processed, err := service.DispatchDue(context.Background(), 1); err != nil || processed != 1 {
		t.Fatalf("DispatchDue(read confirm) = %d, %v; want 1, nil", processed, err)
	}
	delivery, err := service.Wait(context.Background(), submission.OutboxID)
	if err != nil {
		t.Fatalf("Wait(confirmed read): %v", err)
	}
	row := mustOutboxItem(t, service, submission.OutboxID)
	if delivery.State != OutboxConfirmed || delivery.RemoteMessageID != "" ||
		row.ResultRemoteID != nil {
		t.Fatalf("confirmed read delivery/row = %+v / %+v, want confirmed with NULL result", delivery, row)
	}
}

func TestPostCallReadTimeoutBecomesUncertainAndIsNotRedispatched(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	seedDispatchDevice(t, store, "device-read-timeout", clock.Now())
	sender := &scriptedReadReceiptSender{steps: []readReceiptStep{{err: bridge.OpError{
		Class:     bridge.FailureTransient,
		Operation: readOperation,
		Dispatch:  bridge.DispatchUncertain,
		Cause:     context.DeadlineExceeded,
	}}}}
	registry := newScriptedRegistry("read-timeout", &scriptedTextSender{})
	registry.setReadReceiptSender(sender)
	registry.setAvailable(true)
	service := newMessagingTestService(t, store, registry, clock)
	target := mustCanceledDispatchTarget(t, service, "read-timeout-target")
	submission := mustMarkDispatchRead(t, service, MarkReadCommand{
		CommonCommand:     testCommonCommand("read-timeout"),
		DeviceID:          "device-read-timeout",
		LastReadMessageID: target.LocalMessageID,
	})

	if processed, err := service.DispatchDue(context.Background(), 1); err != nil || processed != 1 {
		t.Fatalf("DispatchDue(read timeout) = %d, %v; want 1, nil", processed, err)
	}
	delivery := mustDelivery(t, service, submission.OutboxID)
	if delivery.State != OutboxUncertain || delivery.ErrorCode != readOperation || delivery.Warning == "" {
		t.Fatalf("read timeout delivery = %+v, want uncertain", delivery)
	}
	if processed, err := service.DispatchDue(context.Background(), 1); err != nil || processed != 0 {
		t.Fatalf("DispatchDue(after read timeout) = %d, %v; want 0, nil", processed, err)
	}
	if got := sender.requestCount(); got != 1 {
		t.Fatalf("read timeout send count = %d, want 1", got)
	}
}

func TestReadCursorPersistsThroughTerminalDispatchFailure(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	seedDispatchDevice(t, store, "device-read-terminal", clock.Now())
	sender := &scriptedReadReceiptSender{steps: []readReceiptStep{{err: bridge.OpError{
		Class:     bridge.FailureUnsupported,
		Operation: readOperation,
		Dispatch:  bridge.DispatchNotCalled,
		Cause:     context.Canceled,
	}}}}
	registry := newScriptedRegistry("read-terminal", &scriptedTextSender{})
	registry.setReadReceiptSender(sender)
	registry.setAvailable(true)
	service := newMessagingTestService(t, store, registry, clock)
	target := mustCanceledDispatchTarget(t, service, "read-terminal-target")
	readAt := clock.Now().Add(-time.Second)
	submission := mustMarkDispatchRead(t, service, MarkReadCommand{
		CommonCommand:     testCommonCommand("read-terminal"),
		DeviceID:          "device-read-terminal",
		LastReadMessageID: target.LocalMessageID,
		LastReadAt:        readAt,
	})

	before, err := store.GetReadCursor("device-read-terminal", "conversation-1")
	if err != nil {
		t.Fatalf("GetReadCursor(before dispatch): %v", err)
	}
	if processed, err := service.DispatchDue(context.Background(), 1); err != nil || processed != 1 {
		t.Fatalf("DispatchDue(terminal read) = %d, %v; want 1, nil", processed, err)
	}
	delivery := mustDelivery(t, service, submission.OutboxID)
	after, err := store.GetReadCursor("device-read-terminal", "conversation-1")
	if err != nil {
		t.Fatalf("GetReadCursor(after dispatch): %v", err)
	}
	if delivery.State != OutboxRejected || before.LastReadMessageID == nil ||
		after.LastReadMessageID == nil || *before.LastReadMessageID != target.LocalMessageID ||
		*after.LastReadMessageID != target.LocalMessageID ||
		before.LastReadAtMS != readAt.UnixMilli() || after.LastReadAtMS != readAt.UnixMilli() {
		t.Fatalf("terminal read delivery/cursors = %+v / %+v / %+v", delivery, before, after)
	}
}

func TestReactionAndReadExpiredLeaseRecoveryClassifiesPreAndPostCall(t *testing.T) {
	t.Run("reaction", func(t *testing.T) {
		clock := newManualClock(messagingTestTime)
		store := openMessagingTestStore(t, clock.Now())
		registry := newScriptedRegistry("reaction-recovery", &scriptedTextSender{})
		registry.setReactionSender(&scriptedReactionSender{})
		service := newMessagingTestService(t, store, registry, clock)
		target := mustCanceledDispatchTarget(t, service, "reaction-recovery-target")
		first := mustSendDispatchReaction(t, service, SendReactionCommand{
			CommonCommand:   testCommonCommand("reaction-recovery-pre"),
			TargetMessageID: target.LocalMessageID,
			Emoji:           "👍",
		})
		clock.Advance(time.Millisecond)
		second := mustSendDispatchReaction(t, service, SendReactionCommand{
			CommonCommand:   testCommonCommand("reaction-recovery-post"),
			TargetMessageID: target.LocalMessageID,
			Emoji:           "👎",
		})
		assertExpiredDispatchLeaseRecovery(t, service, clock, first.OutboxID, second.OutboxID)
	})

	t.Run("read", func(t *testing.T) {
		clock := newManualClock(messagingTestTime)
		store := openMessagingTestStore(t, clock.Now())
		seedDispatchDevice(t, store, "device-read-recovery", clock.Now())
		registry := newScriptedRegistry("read-recovery", &scriptedTextSender{})
		registry.setReadReceiptSender(&scriptedReadReceiptSender{})
		service := newMessagingTestService(t, store, registry, clock)
		target := mustCanceledDispatchTarget(t, service, "read-recovery-target")
		first := mustMarkDispatchRead(t, service, MarkReadCommand{
			CommonCommand:     testCommonCommand("read-recovery-pre"),
			DeviceID:          "device-read-recovery",
			LastReadMessageID: target.LocalMessageID,
		})
		clock.Advance(time.Millisecond)
		second := mustMarkDispatchRead(t, service, MarkReadCommand{
			CommonCommand:     testCommonCommand("read-recovery-post"),
			DeviceID:          "device-read-recovery",
			LastReadMessageID: target.LocalMessageID,
		})
		assertExpiredDispatchLeaseRecovery(t, service, clock, first.OutboxID, second.OutboxID)
	})
}

func assertExpiredDispatchLeaseRecovery(
	t *testing.T,
	service *MessageService,
	clock *manualClock,
	preCallID, postCallID string,
) {
	t.Helper()
	leases, err := service.outbox.LeaseDue(context.Background(), sqlite.LeaseRequest{
		Owner:    "dispatch-recovery-test",
		Now:      clock.Now(),
		Duration: defaultLeaseTime,
		Limit:    2,
	})
	if err != nil {
		t.Fatalf("LeaseDue(recovery): %v", err)
	}
	if len(leases) != 2 {
		t.Fatalf("recovery leases = %d, want 2", len(leases))
	}
	var postCallLease *sqlite.Lease
	for index := range leases {
		if leases[index].OutboxID == postCallID {
			postCallLease = &leases[index]
		}
	}
	if postCallLease == nil || postCallLease.LeaseToken == nil {
		t.Fatalf("post-call lease = %+v, want active lease", postCallLease)
	}
	if err := service.outbox.MarkTransportCalled(context.Background(), sqlite.Attempt{
		OutboxID:             postCallID,
		LeaseToken:           *postCallLease.LeaseToken,
		AttemptToken:         "dispatch-recovery-attempt",
		ConnectionGeneration: 7,
		StartedAt:            clock.Now(),
	}); err != nil {
		t.Fatalf("MarkTransportCalled(recovery): %v", err)
	}

	clock.Advance(defaultLeaseTime + time.Second)
	recovered, err := service.outbox.RecoverExpiredLeases(context.Background(), clock.Now())
	if err != nil {
		t.Fatalf("RecoverExpiredLeases(): %v", err)
	}
	if recovered.NotDispatched != 1 || recovered.Uncertain != 1 {
		t.Fatalf("RecoverExpiredLeases() = %+v, want one pre-call and one post-call", recovered)
	}
	preCall := mustOutboxItem(t, service, preCallID)
	postCall := mustOutboxItem(t, service, postCallID)
	if preCall.State != sqlite.OutboxNotDispatched || preCall.AttemptCount != 0 ||
		postCall.State != sqlite.OutboxUncertain || postCall.AttemptCount != 1 {
		t.Fatalf("recovered pre/post rows = %+v / %+v", preCall, postCall)
	}
}

func seedDispatchDevice(t *testing.T, store *sqlite.Store, deviceID string, now time.Time) {
	t.Helper()
	if err := store.UpsertDevice(sqlite.Device{
		DeviceID:    deviceID,
		AccountID:   "account-1",
		Kind:        sqlite.DeviceKindLocalInstallation,
		DisplayName: "Dispatch test device",
		State:       sqlite.DeviceStateActive,
		IsCurrent:   true,
		CreatedAtMS: now.UnixMilli(),
		UpdatedAtMS: now.UnixMilli(),
	}); err != nil {
		t.Fatalf("UpsertDevice(dispatch): %v", err)
	}
}

func mustMarkDispatchRead(
	t *testing.T,
	service *MessageService,
	command MarkReadCommand,
) Submission {
	t.Helper()
	submission, err := service.MarkRead(context.Background(), command)
	if err != nil {
		t.Fatalf("MarkRead(): %v", err)
	}
	return submission
}
