package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Cancel and RetryNotDispatched make the reactions held for that send
// (ReactionTargetUnsentCode) due 1 ms after now, in the same transaction, and
// touch nothing else: not a reaction held for another send, not one waiting
// for another reason, not one already due sooner, and nothing when the
// cancel or retry itself is refused.
func TestCancelAndRetryWakeOnlyTheReactionsHeldForThatSend(t *testing.T) {
	ctx := context.Background()
	const nowMS = int64(1_800_000_000_000)
	clock := newOutboxTestClock(nowMS)
	store, outbox := openOutboxTestRepository(t, clock.Now)
	seedMessageConversation(t, store, "conversation-a", "account-a")
	seedMessageIdentity(t, store, "identity-a", "account-a")

	// send-a is scheduled for tomorrow; send-b was tried and is rate limited
	// until tomorrow.
	tomorrowMS := nowMS + (24 * time.Hour).Milliseconds()
	sendA := outboxTestItem("send-a")
	sendA.ScheduledForMS = tomorrowMS
	mustEnqueueOutgoingOutbox(t, outbox, sendA, "scheduled")
	sendB := mustEnqueueOutgoingOutbox(t, outbox, outboxTestItem("send-b"), "rate limited")
	leased := mustLeaseOne(t, outbox, LeaseRequest{Owner: "dispatcher", Now: clock.Now(), Duration: time.Minute, Limit: 1})
	if leased.OutboxID != sendB.OutboxID {
		t.Fatalf("leased %q, want send-b", leased.OutboxID)
	}
	if err := outbox.MarkNotDispatched(ctx, sendB.OutboxID, mustLeaseToken(t, leased), "rate_limited", "send_text", "scripted",
		time.UnixMilli(tomorrowMS)); err != nil {
		t.Fatalf("MarkNotDispatched(send-b): %v", err)
	}
	// Each reaction waits until retryAtMS with code: held for its target's
	// send, or not_dispatched for another reason.
	reactions := []struct {
		id, target, code string
		retryAtMS        int64
	}{
		{id: "held-for-a", target: "message-send-a", code: ReactionTargetUnsentCode, retryAtMS: tomorrowMS + 1},
		{id: "other-code-for-a", target: "message-send-a", code: "send_reaction", retryAtMS: tomorrowMS + 1},
		// Due half an hour before the cancel below, so already due then.
		{id: "due-sooner-for-a", target: "message-send-a", code: ReactionTargetUnsentCode, retryAtMS: nowMS + (30 * time.Minute).Milliseconds()},
		{id: "held-for-b", target: "message-send-b", code: ReactionTargetUnsentCode, retryAtMS: tomorrowMS + 1},
	}
	for _, reaction := range reactions {
		item := outboxTestReactionItem(reaction.id)
		if _, _, err := outbox.EnqueueReaction(ctx, item, OutboxReaction{TargetMessageID: reaction.target, Emoji: "👍", Action: "add"}); err != nil {
			t.Fatalf("EnqueueReaction(%s): %v", reaction.id, err)
		}
		leased := mustLeaseOne(t, outbox, LeaseRequest{Owner: "dispatcher", Now: clock.Now(), Duration: time.Minute, Limit: 1})
		if leased.OutboxID != item.OutboxID {
			t.Fatalf("leased %q, want %q", leased.OutboxID, item.OutboxID)
		}
		if err := outbox.MarkNotDispatched(ctx, item.OutboxID, mustLeaseToken(t, leased), "transient", reaction.code, "scripted",
			time.UnixMilli(reaction.retryAtMS)); err != nil {
			t.Fatalf("MarkNotDispatched(%s): %v", reaction.id, err)
		}
	}
	next := func(id string) int64 {
		t.Helper()
		row, err := outbox.FindByID(ctx, "outbox-"+id)
		if err != nil || row.NextAttemptAtMS == nil {
			t.Fatalf("FindByID(%s) = %+v, %v; want a next attempt", id, row, err)
		}
		return *row.NextAttemptAtMS
	}
	want := func(when string, expect map[string]int64) {
		t.Helper()
		for id, wantMS := range expect {
			if got := next(id); got != wantMS {
				t.Fatalf("%s: %s next attempt = %d, want %d", when, id, got, wantMS)
			}
		}
	}

	cancelMS := nowMS + time.Hour.Milliseconds()
	clock.Set(cancelMS)
	if err := outbox.Cancel(ctx, sendA.OutboxID); err != nil {
		t.Fatalf("Cancel(send-a): %v", err)
	}
	want("after canceling send-a", map[string]int64{
		"held-for-a":       cancelMS + 1,
		"other-code-for-a": tomorrowMS + 1,
		"due-sooner-for-a": nowMS + (30 * time.Minute).Milliseconds(),
		"held-for-b":       tomorrowMS + 1,
	})

	// A refused cancel or retry wakes nothing.
	clock.Set(cancelMS + time.Second.Milliseconds())
	if err := outbox.Cancel(ctx, sendA.OutboxID); !errors.Is(err, ErrInvalidOutboxState) {
		t.Fatalf("Cancel(canceled send-a) = %v, want ErrInvalidOutboxState", err)
	}
	if err := outbox.RetryNotDispatched(ctx, sendA.OutboxID); !errors.Is(err, ErrInvalidOutboxState) {
		t.Fatalf("RetryNotDispatched(canceled send-a) = %v, want ErrInvalidOutboxState", err)
	}
	want("after the refused calls", map[string]int64{"held-for-a": cancelMS + 1, "held-for-b": tomorrowMS + 1})

	// Retrying send-b early wakes its reaction just after it.
	retryMS := cancelMS + time.Minute.Milliseconds()
	clock.Set(retryMS)
	if err := outbox.RetryNotDispatched(ctx, sendB.OutboxID); err != nil {
		t.Fatalf("RetryNotDispatched(send-b): %v", err)
	}
	want("after retrying send-b", map[string]int64{
		"send-b":           retryMS,
		"held-for-b":       retryMS + 1,
		"held-for-a":       cancelMS + 1,
		"other-code-for-a": tomorrowMS + 1,
	})
}
