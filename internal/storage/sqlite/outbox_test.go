package sqlite

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

const outboxTestTimeMS int64 = 2_000_000_000_000

func TestOutboxEnqueueIdempotencyAndConflict(t *testing.T) {
	clock := newOutboxTestClock(outboxTestTimeMS)
	store, repository := openOutboxTestRepository(t, clock.Now)
	ctx := context.Background()

	original := outboxTestItem("original")
	row, disposition, err := repository.Enqueue(ctx, original)
	if err != nil {
		t.Fatalf("Enqueue(original): %v", err)
	}
	if disposition != EnqueueInserted {
		t.Fatalf("Enqueue(original) disposition = %q, want %q", disposition, EnqueueInserted)
	}
	if row.OutboxID != original.OutboxID || row.State != OutboxQueued {
		t.Fatalf("Enqueue(original) row = %+v", row)
	}
	if row.CreatedAtMS != outboxTestTimeMS || row.ScheduledForMS != outboxTestTimeMS {
		t.Fatalf("Enqueue(original) timestamps = %+v", row)
	}

	clock.Set(outboxTestTimeMS + 500)
	duplicate := original
	duplicate.OutboxID = "outbox-duplicate"
	duplicate.TransportRequestID = "request-duplicate"
	duplicate.LocalMessageID = "message-duplicate"
	duplicateRow, disposition, err := repository.Enqueue(ctx, duplicate)
	if err != nil {
		t.Fatalf("Enqueue(duplicate): %v", err)
	}
	if disposition != EnqueueExisting {
		t.Fatalf("Enqueue(duplicate) disposition = %q, want %q", disposition, EnqueueExisting)
	}
	if duplicateRow.OutboxID != original.OutboxID || duplicateRow.CreatedAtMS != outboxTestTimeMS {
		t.Fatalf("Enqueue(duplicate) returned %+v, want original row", duplicateRow)
	}

	conflicts := []struct {
		name   string
		mutate func(*NewOutboxItem)
	}{
		{name: "operation", mutate: func(item *NewOutboxItem) { item.Operation = "edit_text" }},
		{name: "conversation", mutate: func(item *NewOutboxItem) { item.ConversationID = "conversation-b" }},
		{name: "payload hash", mutate: func(item *NewOutboxItem) { item.PayloadHash = "different-hash" }},
	}
	for i, test := range conflicts {
		t.Run(test.name, func(t *testing.T) {
			candidate := original
			candidate.OutboxID = fmt.Sprintf("outbox-conflict-%d", i)
			candidate.TransportRequestID = fmt.Sprintf("request-conflict-%d", i)
			test.mutate(&candidate)
			_, _, err := repository.Enqueue(ctx, candidate)
			if !errors.Is(err, ErrIdempotencyConflict) {
				t.Fatalf("Enqueue(conflict) error = %v, want ErrIdempotencyConflict", err)
			}
		})
	}
	assertRowCount(t, store.db, "outbox", 1)

	byRequest, err := repository.FindByTransportRequestID(ctx, original.AccountID, original.TransportRequestID)
	if err != nil {
		t.Fatalf("FindByTransportRequestID(): %v", err)
	}
	if byRequest.OutboxID != original.OutboxID {
		t.Fatalf("FindByTransportRequestID() ID = %q, want %q", byRequest.OutboxID, original.OutboxID)
	}
	if _, err := repository.FindByID(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("FindByID(missing) error = %v, want ErrNotFound", err)
	}
}

func TestOutboxEnqueueOutgoingMessageIsAtomicAndDeduplicated(t *testing.T) {
	clock := newOutboxTestClock(outboxTestTimeMS)
	store, repository := openOutboxTestRepository(t, clock.Now)
	seedMessageIdentity(t, store, "identity-a", "account-a")
	seedMessageConversation(t, store, "conversation-a", "account-a")
	ctx := context.Background()

	item := outboxTestItem("outgoing")
	message := outboxTestOutgoingMessage(item, "hello from the durable outbox")
	row, disposition, err := repository.EnqueueOutgoingMessage(ctx, item, message)
	if err != nil {
		t.Fatalf("EnqueueOutgoingMessage(): %v", err)
	}
	if disposition != EnqueueInserted || row.OutboxID != item.OutboxID {
		t.Fatalf("EnqueueOutgoingMessage() = (%+v, %q), want inserted", row, disposition)
	}
	messageRepository, err := NewMessageRepository(store, clock.Now)
	if err != nil {
		t.Fatalf("NewMessageRepository(): %v", err)
	}
	gotMessage, err := messageRepository.GetMessage(ctx, item.LocalMessageID)
	if err != nil {
		t.Fatalf("GetMessage(): %v", err)
	}
	if gotMessage.Body != message.Body || gotMessage.Direction != MessageDirectionOutgoing ||
		gotMessage.State != MessageStateActive || gotMessage.RemoteMessageID != message.RemoteMessageID ||
		gotMessage.SenderIdentityID == nil || *gotMessage.SenderIdentityID != "identity-a" {
		t.Fatalf("optimistic message = %+v, want %+v", gotMessage, message)
	}
	assertRowCount(t, store.db, "inbox", 0)

	clock.Set(outboxTestTimeMS + 500)
	duplicate := item
	duplicate.OutboxID = "outbox-unused-duplicate"
	duplicate.LocalMessageID = "message-unused-duplicate"
	duplicate.TransportRequestID = "request-unused-duplicate"
	duplicateRow, disposition, err := repository.EnqueueOutgoingMessage(ctx, duplicate, Message{})
	if err != nil {
		t.Fatalf("EnqueueOutgoingMessage(duplicate with invalid candidate): %v", err)
	}
	if disposition != EnqueueExisting || duplicateRow.OutboxID != item.OutboxID {
		t.Fatalf("duplicate result = (%+v, %q), want original", duplicateRow, disposition)
	}

	conflict := duplicate
	conflict.PayloadHash = "different-payload-hash"
	if _, _, err := repository.EnqueueOutgoingMessage(ctx, conflict, Message{}); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("EnqueueOutgoingMessage(conflict) error = %v, want ErrIdempotencyConflict", err)
	}

	generic := outboxTestItem("generic-collision")
	mustEnqueueOutbox(t, repository, generic)
	if _, _, err := repository.EnqueueOutgoingMessage(
		ctx,
		generic,
		outboxTestOutgoingMessage(generic, "missing optimistic pair"),
	); !errors.Is(err, ErrInvalidMessage) {
		t.Fatalf("EnqueueOutgoingMessage(generic collision) error = %v, want ErrInvalidMessage", err)
	}
	foreignPair := outboxTestItem("foreign-pair")
	if _, _, err := repository.EnqueueOutgoingMessage(
		ctx,
		foreignPair,
		outboxTestOutgoingMessage(foreignPair, "belongs to another request"),
	); err != nil {
		t.Fatalf("EnqueueOutgoingMessage(foreign pair): %v", err)
	}
	genericWithForeignMessage := outboxTestItem("generic-foreign-message")
	genericWithForeignMessage.LocalMessageID = foreignPair.LocalMessageID
	mustEnqueueOutbox(t, repository, genericWithForeignMessage)
	if _, _, err := repository.EnqueueOutgoingMessage(
		ctx,
		genericWithForeignMessage,
		outboxTestOutgoingMessage(genericWithForeignMessage, "candidate is not used"),
	); !errors.Is(err, ErrInvalidMessage) {
		t.Fatalf("EnqueueOutgoingMessage(foreign message collision) error = %v, want ErrInvalidMessage", err)
	}

	invalidItem := outboxTestItem("invalid-outgoing")
	invalidMessage := outboxTestOutgoingMessage(invalidItem, "must roll back")
	invalidMessage.State = MessageStateEdited
	if _, _, err := repository.EnqueueOutgoingMessage(ctx, invalidItem, invalidMessage); !errors.Is(err, ErrInvalidMessage) {
		t.Fatalf("EnqueueOutgoingMessage(invalid) error = %v, want ErrInvalidMessage", err)
	}
	assertRowCount(t, store.db, "outbox", 4)
	assertRowCount(t, store.db, "messages", 2)
}

func TestOutboxLeaseDueRespectsScheduleRetryAndLiveLease(t *testing.T) {
	clock := newOutboxTestClock(outboxTestTimeMS)
	_, repository := openOutboxTestRepository(t, clock.Now)
	ctx := context.Background()
	now := clock.Now()

	live := outboxTestItem("live")
	live.ScheduledFor = now.Add(-time.Second)
	mustEnqueueOutbox(t, repository, live)
	liveLease := mustLeaseOne(t, repository, LeaseRequest{
		Owner: "worker-live", Now: now, Duration: 10 * time.Second, Limit: 1,
	})

	retry := outboxTestItem("retry")
	mustEnqueueOutbox(t, repository, retry)
	retryLease := mustLeaseOne(t, repository, LeaseRequest{
		Owner: "worker-retry", Now: now, Duration: 10 * time.Second, Limit: 1,
	})
	if err := repository.MarkNotDispatched(
		ctx,
		retry.OutboxID,
		mustLeaseToken(t, retryLease),
		"offline",
		"not_connected",
		"bridge is offline",
		now.Add(5*time.Second),
	); err != nil {
		t.Fatalf("MarkNotDispatched(): %v", err)
	}

	due := outboxTestItem("due")
	mustEnqueueOutbox(t, repository, due)
	future := outboxTestItem("future")
	future.ScheduledFor = now.Add(10 * time.Second)
	mustEnqueueOutbox(t, repository, future)

	leases, err := repository.LeaseDue(ctx, LeaseRequest{
		Owner: "worker-due", Now: now, Duration: 3 * time.Second, Limit: 10,
	})
	if err != nil {
		t.Fatalf("LeaseDue(): %v", err)
	}
	if len(leases) != 1 || leases[0].OutboxID != due.OutboxID {
		t.Fatalf("LeaseDue() leases = %+v, want only %q", leases, due.OutboxID)
	}
	lease := leases[0]
	if lease.LeaseOwner == nil || *lease.LeaseOwner != "worker-due" ||
		lease.LeaseToken == nil || *lease.LeaseToken == "" ||
		lease.LeaseExpiresAtMS == nil || *lease.LeaseExpiresAtMS != now.Add(3*time.Second).UnixMilli() {
		t.Fatalf("claimed lease fields = %+v", lease.OutboxItem)
	}

	again, err := repository.LeaseDue(ctx, LeaseRequest{
		Owner: "worker-other", Now: now, Duration: time.Second, Limit: 10,
	})
	if err != nil {
		t.Fatalf("LeaseDue(again): %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("LeaseDue(again) = %+v, want no live/scheduled/retry leases", again)
	}
	if got, err := repository.FindByID(ctx, live.OutboxID); err != nil {
		t.Fatalf("FindByID(live): %v", err)
	} else if got.State != OutboxDispatching || got.LeaseToken == nil || *got.LeaseToken != mustLeaseToken(t, liveLease) {
		t.Fatalf("live row changed unexpectedly: %+v", got)
	}
}

func TestOutboxPreCallExpiredLeaseRetriesAndStaleTokenLoses(t *testing.T) {
	clock := newOutboxTestClock(outboxTestTimeMS)
	_, repository := openOutboxTestRepository(t, clock.Now)
	ctx := context.Background()
	item := outboxTestItem("pre-call")
	mustEnqueueOutbox(t, repository, item)
	first := mustLeaseOne(t, repository, LeaseRequest{
		Owner: "worker-a", Now: clock.Now(), Duration: time.Second, Limit: 1,
	})
	firstToken := mustLeaseToken(t, first)

	clock.Set(outboxTestTimeMS + 1_000)
	recovered, err := repository.RecoverExpiredLeases(ctx, clock.Now())
	if err != nil {
		t.Fatalf("RecoverExpiredLeases(): %v", err)
	}
	if recovered.NotDispatched != 1 || recovered.Uncertain != 0 {
		t.Fatalf("RecoverExpiredLeases() = %+v", recovered)
	}
	retryable, err := repository.FindByID(ctx, item.OutboxID)
	if err != nil {
		t.Fatalf("FindByID(recovered): %v", err)
	}
	if retryable.State != OutboxNotDispatched || retryable.NextAttemptAtMS == nil ||
		*retryable.NextAttemptAtMS != clock.Now().UnixMilli() || retryable.LeaseToken != nil {
		t.Fatalf("pre-call recovered row = %+v", retryable)
	}

	second := mustLeaseOne(t, repository, LeaseRequest{
		Owner: "worker-b", Now: clock.Now(), Duration: time.Second, Limit: 1,
	})
	secondToken := mustLeaseToken(t, second)
	if secondToken == firstToken {
		t.Fatal("re-leased row reused its stale lease token")
	}
	err = repository.MarkNotDispatched(
		ctx,
		item.OutboxID,
		firstToken,
		"late",
		"stale_worker",
		"late result",
		clock.Now().Add(time.Second),
	)
	if !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("late MarkNotDispatched() error = %v, want ErrLeaseLost", err)
	}
	stillLeased, err := repository.FindByID(ctx, item.OutboxID)
	if err != nil {
		t.Fatalf("FindByID(re-leased): %v", err)
	}
	if stillLeased.State != OutboxDispatching || stillLeased.LeaseToken == nil || *stillLeased.LeaseToken != secondToken {
		t.Fatalf("stale mutation changed re-leased row: %+v", stillLeased)
	}
}

func TestOutboxPostCallExpiredLeaseBecomesUncertain(t *testing.T) {
	clock := newOutboxTestClock(outboxTestTimeMS)
	_, repository := openOutboxTestRepository(t, clock.Now)
	ctx := context.Background()
	item := outboxTestItem("post-call")
	mustEnqueueOutbox(t, repository, item)
	lease := mustLeaseOne(t, repository, LeaseRequest{
		Owner: "worker", Now: clock.Now(), Duration: time.Second, Limit: 1,
	})
	if err := repository.MarkTransportCalled(ctx, Attempt{
		OutboxID: item.OutboxID, LeaseToken: mustLeaseToken(t, lease),
	}); err != nil {
		t.Fatalf("MarkTransportCalled(): %v", err)
	}
	called, err := repository.FindByID(ctx, item.OutboxID)
	if err != nil {
		t.Fatalf("FindByID(called): %v", err)
	}
	if called.AttemptCount != 1 || called.TransportCalledAtMS == nil {
		t.Fatalf("called row = %+v", called)
	}

	clock.Set(outboxTestTimeMS + 1_000)
	recovered, err := repository.RecoverExpiredLeases(ctx, clock.Now())
	if err != nil {
		t.Fatalf("RecoverExpiredLeases(): %v", err)
	}
	if recovered.NotDispatched != 0 || recovered.Uncertain != 1 {
		t.Fatalf("RecoverExpiredLeases() = %+v", recovered)
	}
	uncertain, err := repository.FindByID(ctx, item.OutboxID)
	if err != nil {
		t.Fatalf("FindByID(uncertain): %v", err)
	}
	if uncertain.State != OutboxUncertain || uncertain.NextAttemptAtMS != nil || uncertain.LeaseToken != nil {
		t.Fatalf("post-call recovered row = %+v", uncertain)
	}
	leases, err := repository.LeaseDue(ctx, LeaseRequest{
		Owner: "worker-later", Now: clock.Now().Add(time.Hour), Duration: time.Second, Limit: 10,
	})
	if err != nil {
		t.Fatalf("LeaseDue(uncertain): %v", err)
	}
	if len(leases) != 0 {
		t.Fatalf("LeaseDue(uncertain) = %+v, want no automatic retry", leases)
	}
}

func TestOutboxLateConfirmationWinsBeforeRecovery(t *testing.T) {
	clock := newOutboxTestClock(outboxTestTimeMS)
	_, repository := openOutboxTestRepository(t, clock.Now)
	ctx := context.Background()
	item := mustEnqueueOutbox(t, repository, outboxTestItem("late-confirmation"))
	lease := mustLeaseOne(t, repository, LeaseRequest{
		Owner: "worker", Now: clock.Now(), Duration: time.Second, Limit: 1,
	})
	token := mustLeaseToken(t, lease)
	if err := repository.MarkTransportCalled(ctx, Attempt{
		OutboxID: item.OutboxID, LeaseToken: token,
	}); err != nil {
		t.Fatalf("MarkTransportCalled(): %v", err)
	}

	// Expiry makes the row recoverable, but recovery has not fenced this token
	// yet. A definitive result and recovery serialize; the first commit wins.
	clock.Set(outboxTestTimeMS + 1_000)
	if err := repository.Confirm(ctx, Confirmation{
		OutboxID: item.OutboxID, LeaseToken: token, ResultRemoteID: "remote-late",
	}); err != nil {
		t.Fatalf("Confirm(after expiry, before recovery): %v", err)
	}
	recovered, err := repository.RecoverExpiredLeases(ctx, clock.Now())
	if err != nil {
		t.Fatalf("RecoverExpiredLeases(): %v", err)
	}
	if recovered != (RecoveredLeases{}) {
		t.Fatalf("RecoverExpiredLeases() = %+v, want no recovered terminal row", recovered)
	}
	confirmed, err := repository.FindByID(ctx, item.OutboxID)
	if err != nil {
		t.Fatalf("FindByID(): %v", err)
	}
	if confirmed.State != OutboxConfirmed || confirmed.ResultRemoteID == nil || *confirmed.ResultRemoteID != "remote-late" {
		t.Fatalf("late confirmed row = %+v", confirmed)
	}
}

func TestOutboxConcurrentLeaseDueClaimsEachRowOnce(t *testing.T) {
	clock := newOutboxTestClock(outboxTestTimeMS)
	_, repository := openOutboxTestRepository(t, clock.Now)
	const itemCount = 12
	for i := range itemCount {
		mustEnqueueOutbox(t, repository, outboxTestItem(fmt.Sprintf("concurrent-%02d", i)))
	}

	type leaseResult struct {
		leases []Lease
		err    error
	}
	start := make(chan struct{})
	results := make(chan leaseResult, 2)
	for _, owner := range []string{"worker-a", "worker-b"} {
		go func(owner string) {
			<-start
			leases, err := repository.LeaseDue(context.Background(), LeaseRequest{
				Owner: owner, Now: clock.Now(), Duration: time.Minute, Limit: itemCount,
			})
			results <- leaseResult{leases: leases, err: err}
		}(owner)
	}
	close(start)

	seenIDs := make(map[string]string, itemCount)
	for range 2 {
		result := <-results
		if result.err != nil {
			t.Fatalf("concurrent LeaseDue(): %v", result.err)
		}
		for _, lease := range result.leases {
			token := mustLeaseToken(t, lease.OutboxItem)
			if previous, exists := seenIDs[lease.OutboxID]; exists {
				t.Fatalf("outbox item %q leased twice with tokens %q and %q", lease.OutboxID, previous, token)
			}
			seenIDs[lease.OutboxID] = token
		}
	}
	if len(seenIDs) != itemCount {
		t.Fatalf("concurrent LeaseDue() claimed %d unique rows, want %d", len(seenIDs), itemCount)
	}
}

func TestOutboxTerminalAndUncertainTransitions(t *testing.T) {
	tests := []struct {
		name       string
		wantState  OutboxState
		transition func(context.Context, *OutboxRepository, OutboxItem, string) error
		assert     func(*testing.T, OutboxItem)
	}{
		{
			name: "confirm", wantState: OutboxConfirmed,
			transition: func(ctx context.Context, repository *OutboxRepository, item OutboxItem, token string) error {
				return repository.Confirm(ctx, Confirmation{
					OutboxID: item.OutboxID, LeaseToken: token, ResultRemoteID: "remote-confirmed",
				})
			},
			assert: func(t *testing.T, item OutboxItem) {
				if item.ResultRemoteID == nil || *item.ResultRemoteID != "remote-confirmed" {
					t.Fatalf("confirmed result = %+v", item.ResultRemoteID)
				}
			},
		},
		{
			name: "reject", wantState: OutboxRejected,
			transition: func(ctx context.Context, repository *OutboxRepository, item OutboxItem, token string) error {
				return repository.Reject(ctx, item.OutboxID, token, "permanent", "blocked", "recipient blocked")
			},
			assert: func(t *testing.T, item OutboxItem) {
				assertOutboxText(t, "error class", item.ErrorClass, "permanent")
				assertOutboxText(t, "error code", item.ErrorCode, "blocked")
				assertOutboxText(t, "error detail", item.ErrorDetail, "recipient blocked")
			},
		},
		{
			name: "store failed", wantState: OutboxStoreFailed,
			transition: func(ctx context.Context, repository *OutboxRepository, item OutboxItem, token string) error {
				return repository.MarkStoreFailed(ctx, item.OutboxID, token, "remote-accepted", "disk full")
			},
			assert: func(t *testing.T, item OutboxItem) {
				assertOutboxText(t, "result remote ID", item.ResultRemoteID, "remote-accepted")
				assertOutboxText(t, "error detail", item.ErrorDetail, "disk full")
			},
		},
		{
			name: "uncertain", wantState: OutboxUncertain,
			transition: func(ctx context.Context, repository *OutboxRepository, item OutboxItem, token string) error {
				return repository.MarkUncertain(ctx, item.OutboxID, token, "timeout", "deadline", "outcome unknown")
			},
			assert: func(t *testing.T, item OutboxItem) {
				assertOutboxText(t, "error class", item.ErrorClass, "timeout")
				if item.NextAttemptAtMS != nil {
					t.Fatalf("uncertain next attempt = %v, want nil", item.NextAttemptAtMS)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clock := newOutboxTestClock(outboxTestTimeMS)
			_, repository := openOutboxTestRepository(t, clock.Now)
			ctx := context.Background()
			input := outboxTestItem(stringsForOutboxID(test.name))
			item := mustEnqueueOutbox(t, repository, input)
			lease := mustLeaseOne(t, repository, LeaseRequest{
				Owner: "worker", Now: clock.Now(), Duration: time.Minute, Limit: 1,
			})
			token := mustLeaseToken(t, lease)
			if err := repository.MarkTransportCalled(ctx, Attempt{
				OutboxID: item.OutboxID, LeaseToken: token,
			}); err != nil {
				t.Fatalf("MarkTransportCalled(): %v", err)
			}
			if err := test.transition(ctx, repository, item, token); err != nil {
				t.Fatalf("transition: %v", err)
			}
			got, err := repository.FindByID(ctx, item.OutboxID)
			if err != nil {
				t.Fatalf("FindByID(): %v", err)
			}
			if got.State != test.wantState || got.LeaseToken != nil || got.LeaseOwner != nil || got.LeaseExpiresAtMS != nil {
				t.Fatalf("terminal row = %+v, want state %q and no lease", got, test.wantState)
			}
			test.assert(t, got)
		})
	}
}

func TestOutboxPhaseTransitionsRejectWrongSideOfCallBoundary(t *testing.T) {
	clock := newOutboxTestClock(outboxTestTimeMS)
	_, repository := openOutboxTestRepository(t, clock.Now)
	ctx := context.Background()
	item := mustEnqueueOutbox(t, repository, outboxTestItem("phase"))
	lease := mustLeaseOne(t, repository, LeaseRequest{
		Owner: "worker", Now: clock.Now(), Duration: time.Minute, Limit: 1,
	})
	token := mustLeaseToken(t, lease)
	if err := repository.MarkUncertain(ctx, item.OutboxID, token, "", "", ""); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("pre-call MarkUncertain() error = %v, want ErrLeaseLost", err)
	}
	if err := repository.MarkTransportCalled(ctx, Attempt{OutboxID: item.OutboxID, LeaseToken: token}); err != nil {
		t.Fatalf("MarkTransportCalled(): %v", err)
	}
	if err := repository.MarkNotDispatched(
		ctx, item.OutboxID, token, "", "", "", clock.Now().Add(time.Second),
	); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("post-call MarkNotDispatched() error = %v, want ErrLeaseLost", err)
	}
}

func TestOutboxReleaseUnavailableReturnsLeaseToQueuedWithoutAttempt(t *testing.T) {
	clock := newOutboxTestClock(outboxTestTimeMS)
	_, repository := openOutboxTestRepository(t, clock.Now)
	ctx := context.Background()
	item := outboxTestItem("unavailable")
	mustEnqueueOutbox(t, repository, item)
	lease := mustLeaseOne(t, repository, LeaseRequest{
		Owner: "worker", Now: clock.Now(), Duration: time.Minute, Limit: 1,
	})
	token := mustLeaseToken(t, lease)
	if err := repository.ReleaseUnavailable(ctx, item.OutboxID, "stale-token"); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("ReleaseUnavailable(stale token) error = %v, want ErrLeaseLost", err)
	}
	if err := repository.ReleaseUnavailable(ctx, item.OutboxID, token); err != nil {
		t.Fatalf("ReleaseUnavailable(): %v", err)
	}
	queued, err := repository.FindByID(ctx, item.OutboxID)
	if err != nil {
		t.Fatalf("FindByID(): %v", err)
	}
	if queued.State != OutboxQueued || queued.AttemptCount != 0 || queued.LeaseToken != nil ||
		queued.NextAttemptAtMS != nil || queued.TransportCalledAtMS != nil ||
		queued.TransportRequestID != item.TransportRequestID {
		t.Fatalf("released unavailable row = %+v", queued)
	}
}

func TestOutboxMarkCalledNotDispatchedRecordsExplicitSafeRetry(t *testing.T) {
	clock := newOutboxTestClock(outboxTestTimeMS)
	_, repository := openOutboxTestRepository(t, clock.Now)
	ctx := context.Background()
	item := outboxTestItem("called-not-dispatched")
	mustEnqueueOutbox(t, repository, item)
	lease := mustLeaseOne(t, repository, LeaseRequest{
		Owner: "worker", Now: clock.Now(), Duration: time.Minute, Limit: 1,
	})
	token := mustLeaseToken(t, lease)
	if err := repository.MarkCalledNotDispatched(
		ctx, item.OutboxID, token, "transient", "preflight", "not sent", clock.Now().Add(time.Minute),
	); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("MarkCalledNotDispatched(pre-call) error = %v, want ErrLeaseLost", err)
	}
	if err := repository.MarkTransportCalled(ctx, Attempt{OutboxID: item.OutboxID, LeaseToken: token}); err != nil {
		t.Fatalf("MarkTransportCalled(): %v", err)
	}
	retryAt := clock.Now().Add(time.Minute)
	if err := repository.MarkCalledNotDispatched(
		ctx, item.OutboxID, token, "transient", "preflight", "transport proved no dispatch", retryAt,
	); err != nil {
		t.Fatalf("MarkCalledNotDispatched(): %v", err)
	}
	notDispatched, err := repository.FindByID(ctx, item.OutboxID)
	if err != nil {
		t.Fatalf("FindByID(): %v", err)
	}
	if notDispatched.State != OutboxNotDispatched || notDispatched.TransportCalledAtMS != nil ||
		notDispatched.NextAttemptAtMS == nil || *notDispatched.NextAttemptAtMS != retryAt.UnixMilli() ||
		notDispatched.TransportRequestID != item.TransportRequestID || notDispatched.AttemptCount != 1 {
		t.Fatalf("called-not-dispatched row = %+v", notDispatched)
	}
	assertOutboxText(t, "error class", notDispatched.ErrorClass, "transient")
	assertOutboxText(t, "error code", notDispatched.ErrorCode, "preflight")
}

func TestOutboxRetryNotDispatchedMakesRowDueWithSameRequestID(t *testing.T) {
	clock := newOutboxTestClock(outboxTestTimeMS)
	_, repository := openOutboxTestRepository(t, clock.Now)
	ctx := context.Background()
	item := outboxTestItem("manual-retry")
	mustEnqueueOutbox(t, repository, item)
	lease := mustLeaseOne(t, repository, LeaseRequest{
		Owner: "worker", Now: clock.Now(), Duration: time.Minute, Limit: 1,
	})
	if err := repository.MarkNotDispatched(
		ctx, item.OutboxID, mustLeaseToken(t, lease), "transient", "offline", "retry later", clock.Now().Add(time.Hour),
	); err != nil {
		t.Fatalf("MarkNotDispatched(): %v", err)
	}
	clock.Set(outboxTestTimeMS + 1_000)
	if err := repository.RetryNotDispatched(ctx, item.OutboxID); err != nil {
		t.Fatalf("RetryNotDispatched(): %v", err)
	}
	retryable, err := repository.FindByID(ctx, item.OutboxID)
	if err != nil {
		t.Fatalf("FindByID(): %v", err)
	}
	if retryable.State != OutboxNotDispatched || retryable.NextAttemptAtMS == nil ||
		*retryable.NextAttemptAtMS != clock.Now().UnixMilli() ||
		retryable.TransportRequestID != item.TransportRequestID || retryable.ErrorClass == nil ||
		*retryable.ErrorClass != "transient" {
		t.Fatalf("manual retry row = %+v", retryable)
	}
	queuedItem := outboxTestItem("manual-retry-wrong-state")
	mustEnqueueOutbox(t, repository, queuedItem)
	if err := repository.RetryNotDispatched(ctx, queuedItem.OutboxID); !errors.Is(err, ErrInvalidOutboxState) {
		t.Fatalf("RetryNotDispatched(queued) error = %v, want ErrInvalidOutboxState", err)
	}
	if err := repository.RetryNotDispatched(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("RetryNotDispatched(missing) error = %v, want ErrNotFound", err)
	}
}

func TestOutboxCancelAllowsOnlyQueuedOrNotDispatched(t *testing.T) {
	clock := newOutboxTestClock(outboxTestTimeMS)
	_, repository := openOutboxTestRepository(t, clock.Now)
	ctx := context.Background()

	queuedItem := outboxTestItem("cancel-queued")
	mustEnqueueOutbox(t, repository, queuedItem)
	if err := repository.Cancel(ctx, queuedItem.OutboxID); err != nil {
		t.Fatalf("Cancel(queued): %v", err)
	}
	canceled, err := repository.FindByID(ctx, queuedItem.OutboxID)
	if err != nil {
		t.Fatalf("FindByID(canceled): %v", err)
	}
	if canceled.State != OutboxCanceled || canceled.NextAttemptAtMS != nil {
		t.Fatalf("canceled queued row = %+v", canceled)
	}
	if err := repository.Cancel(ctx, queuedItem.OutboxID); !errors.Is(err, ErrInvalidOutboxState) {
		t.Fatalf("Cancel(canceled) error = %v, want ErrInvalidOutboxState", err)
	}

	notDispatchedItem := outboxTestItem("cancel-not-dispatched")
	mustEnqueueOutbox(t, repository, notDispatchedItem)
	lease := mustLeaseOne(t, repository, LeaseRequest{
		Owner: "worker", Now: clock.Now(), Duration: time.Minute, Limit: 1,
	})
	if err := repository.MarkNotDispatched(
		ctx, notDispatchedItem.OutboxID, mustLeaseToken(t, lease), "transient", "offline", "retry later", clock.Now().Add(time.Hour),
	); err != nil {
		t.Fatalf("MarkNotDispatched(): %v", err)
	}
	if err := repository.Cancel(ctx, notDispatchedItem.OutboxID); err != nil {
		t.Fatalf("Cancel(not dispatched): %v", err)
	}

	dispatchingItem := outboxTestItem("cancel-dispatching")
	mustEnqueueOutbox(t, repository, dispatchingItem)
	lease = mustLeaseOne(t, repository, LeaseRequest{
		Owner: "worker", Now: clock.Now(), Duration: time.Minute, Limit: 1,
	})
	if lease.OutboxID != dispatchingItem.OutboxID {
		t.Fatalf("LeaseDue(cancel dispatching) = %+v, want %q", lease, dispatchingItem.OutboxID)
	}
	if err := repository.Cancel(ctx, dispatchingItem.OutboxID); !errors.Is(err, ErrInvalidOutboxState) {
		t.Fatalf("Cancel(dispatching) error = %v, want ErrInvalidOutboxState", err)
	}
	if err := repository.Cancel(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Cancel(missing) error = %v, want ErrNotFound", err)
	}
}

func TestOutboxMigrationIsChecksummedAndStrict(t *testing.T) {
	store, _ := openOutboxTestRepository(t, func() time.Time {
		return time.UnixMilli(outboxTestTimeMS)
	})
	if len(embeddedMigrations) != 5 {
		t.Fatalf("embedded migrations = %d, want 5", len(embeddedMigrations))
	}
	assertPragmaInt(t, store.db, "user_version", 5)
	ledger := readLedgerRow(t, store.db, 5)
	if ledger.name != "outbox" {
		t.Fatalf("migration 0005 name = %q, want outbox", ledger.name)
	}
	if ledger.checksum != embeddedMigrations[4].checksumSHA256 {
		t.Fatalf("migration 0005 checksum = %q, want %q", ledger.checksum, embeddedMigrations[4].checksumSHA256)
	}
	var strict int
	if err := store.db.QueryRow(`
		SELECT strict
		FROM pragma_table_list
		WHERE schema = 'main' AND name = 'outbox'
	`).Scan(&strict); err != nil {
		t.Fatalf("read outbox STRICT flag: %v", err)
	}
	if strict != 1 {
		t.Fatalf("outbox strict = %d, want 1", strict)
	}
}

type outboxTestClock struct {
	mu    sync.Mutex
	nowMS int64
}

func newOutboxTestClock(nowMS int64) *outboxTestClock {
	return &outboxTestClock{nowMS: nowMS}
}

func (c *outboxTestClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.UnixMilli(c.nowMS)
}

func (c *outboxTestClock) Set(nowMS int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nowMS = nowMS
}

func openOutboxTestRepository(
	t *testing.T,
	now func() time.Time,
) (*Store, *OutboxRepository) {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "store.sqlite3"))
	if err != nil {
		t.Fatalf("Open(): %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close(): %v", err)
		}
	})
	seedMessageAccount(t, store, "account-a", "test")
	repository, err := NewOutboxRepository(store, now)
	if err != nil {
		t.Fatalf("NewOutboxRepository(): %v", err)
	}
	return store, repository
}

func outboxTestItem(id string) NewOutboxItem {
	return NewOutboxItem{
		OutboxID:           "outbox-" + id,
		AccountID:          "account-a",
		ConversationID:     "conversation-a",
		Kind:               OutboxKindText,
		IdempotencyKey:     "idempotency-" + id,
		PayloadHash:        "payload-hash-" + id,
		Operation:          "send_text",
		LocalMessageID:     "message-" + id,
		TransportRequestID: "request-" + id,
	}
}

func outboxTestOutgoingMessage(item NewOutboxItem, body string) Message {
	senderIdentityID := "identity-a"
	return Message{
		MessageID:        item.LocalMessageID,
		ConversationID:   item.ConversationID,
		AccountID:        item.AccountID,
		RemoteMessageID:  item.TransportRequestID,
		SenderIdentityID: &senderIdentityID,
		Direction:        MessageDirectionOutgoing,
		Body:             body,
		State:            MessageStateActive,
		OccurredAtMS:     outboxTestTimeMS,
	}
}

func mustEnqueueOutbox(
	t *testing.T,
	repository *OutboxRepository,
	item NewOutboxItem,
) OutboxItem {
	t.Helper()
	row, disposition, err := repository.Enqueue(context.Background(), item)
	if err != nil {
		t.Fatalf("Enqueue(%q): %v", item.OutboxID, err)
	}
	if disposition != EnqueueInserted {
		t.Fatalf("Enqueue(%q) disposition = %q, want %q", item.OutboxID, disposition, EnqueueInserted)
	}
	return row
}

func mustLeaseOne(
	t *testing.T,
	repository *OutboxRepository,
	req LeaseRequest,
) OutboxItem {
	t.Helper()
	leases, err := repository.LeaseDue(context.Background(), req)
	if err != nil {
		t.Fatalf("LeaseDue(): %v", err)
	}
	if len(leases) != 1 {
		t.Fatalf("LeaseDue() returned %d leases, want 1: %+v", len(leases), leases)
	}
	return leases[0].OutboxItem
}

func mustLeaseToken(t *testing.T, item OutboxItem) string {
	t.Helper()
	if item.LeaseToken == nil || *item.LeaseToken == "" {
		t.Fatalf("outbox item %q has no lease token: %+v", item.OutboxID, item)
	}
	return *item.LeaseToken
}

func assertOutboxText(t *testing.T, field string, got *string, want string) {
	t.Helper()
	if got == nil || *got != want {
		t.Fatalf("%s = %v, want %q", field, got, want)
	}
}

func stringsForOutboxID(value string) string {
	result := make([]byte, 0, len(value))
	for i := range len(value) {
		if value[i] == ' ' {
			result = append(result, '-')
			continue
		}
		result = append(result, value[i])
	}
	return string(result)
}
