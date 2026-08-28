package messaging

// TTL and near-duplicate guard behavior for durable sends, added after the
// 2026-08-05 incident: an overnight-queued send flushed ~15 hours later,
// seconds behind a manually retried near-duplicate, double-texting the
// recipient. The TTL bounds how stale a queued send may get; the guard blocks
// the accidental near-duplicate resubmission that completed the double-text.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

func ttlCommand(key string, ttl time.Duration) SendTextCommand {
	command := testCommonCommand(key)
	command.TTL = ttl
	return SendTextCommand{CommonCommand: command, Body: "time-sensitive lunch plan"}
}

func TestSendTextTTLStampsExpiry(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	registry := newScriptedRegistry("ttl-stamp", &scriptedTextSender{})
	service := newMessagingTestService(t, store, registry, clock)

	submission := mustSendText(t, service, ttlCommand("key-ttl-stamp", 10*time.Minute))
	wantExpiry := messagingTestTime.Add(10 * time.Minute)
	if !submission.ExpiresAt.Equal(wantExpiry) {
		t.Fatalf("submission expiry = %v, want %v", submission.ExpiresAt, wantExpiry)
	}
	delivery := mustDelivery(t, service, submission.OutboxID)
	if !delivery.ExpiresAt.Equal(wantExpiry) {
		t.Fatalf("delivery expiry = %v, want %v", delivery.ExpiresAt, wantExpiry)
	}

	// No TTL means no expiry.
	command := testCommonCommand("key-ttl-none")
	unbounded := mustSendText(t, service, SendTextCommand{CommonCommand: command, Body: "no window"})
	if !unbounded.ExpiresAt.IsZero() {
		t.Fatalf("unbounded submission expiry = %v, want zero", unbounded.ExpiresAt)
	}
}

func TestScheduledSendTTLCountsFromSchedule(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	registry := newScriptedRegistry("ttl-scheduled", &scriptedTextSender{})
	service := newMessagingTestService(t, store, registry, clock)

	command := ttlCommand("key-ttl-scheduled", 10*time.Minute)
	command.NotBefore = messagingTestTime.Add(24 * time.Hour)
	submission := mustSendText(t, service, command)

	// The window opens at the scheduled time, so a tomorrow-send with a
	// 10-minute TTL is not born expired.
	wantExpiry := command.NotBefore.Add(10 * time.Minute)
	if !submission.ExpiresAt.Equal(wantExpiry) {
		t.Fatalf("scheduled expiry = %v, want %v", submission.ExpiresAt, wantExpiry)
	}
}

func TestExpiredQueuedSendIsCanceledAndNeverDispatched(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	sender := &scriptedTextSender{steps: []sendStep{{result: bridge.SendResult{RemoteMessageID: "remote-late"}}}}
	registry := newScriptedRegistry("ttl-expire", sender)
	// The transport stays unavailable while the message waits — the incident
	// shape: nothing dispatches overnight.
	registry.setAvailable(false)
	service := newMessagingTestService(t, store, registry, clock)

	submission := mustSendText(t, service, ttlCommand("key-ttl-expire", 10*time.Minute))
	// The unavailable transport releases the lease untouched: the item is
	// processed (lease handled) but the transport is never called and the
	// intent stays queued.
	if _, err := service.DispatchDue(context.Background(), 8); err != nil {
		t.Fatalf("DispatchDue(unavailable): %v", err)
	}
	if got := sender.requestCount(); got != 0 {
		t.Fatalf("transport called %d times while unavailable, want 0", got)
	}
	if got := mustDelivery(t, service, submission.OutboxID).State; got != OutboxQueued {
		t.Fatalf("state before expiry = %q, want queued", got)
	}

	// 15 hours later the transport comes back — the incident's overnight gap.
	clock.Advance(15 * time.Hour)
	registry.setAvailable(true)

	if err := service.cancelExpiredDue(context.Background()); err != nil {
		t.Fatalf("cancelExpiredDue(): %v", err)
	}
	delivery := mustDelivery(t, service, submission.OutboxID)
	if delivery.State != OutboxCanceled {
		t.Fatalf("state after expiry sweep = %q, want canceled", delivery.State)
	}
	if !delivery.Expired() {
		t.Fatalf("delivery.Expired() = false, want true; delivery=%+v", delivery)
	}

	// Even without the sweep having run first, the dispatcher must never
	// lease an expired intent: the transport is called zero times.
	if processed, err := service.DispatchDue(context.Background(), 8); err != nil || processed != 0 {
		t.Fatalf("DispatchDue(after expiry) = %d, %v; want 0, nil", processed, err)
	}
	if got := sender.requestCount(); got != 0 {
		t.Fatalf("transport called %d times for an expired send, want 0", got)
	}

	// Wait reports the canceled outcome instead of blocking.
	waited, err := service.Wait(context.Background(), submission.OutboxID)
	if err != nil {
		t.Fatalf("Wait(expired): %v", err)
	}
	if waited.State != OutboxCanceled {
		t.Fatalf("Wait state = %q, want canceled", waited.State)
	}
}

func TestLeaseDueSkipsExpiredEvenWithoutSweep(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	sender := &scriptedTextSender{steps: []sendStep{{result: bridge.SendResult{RemoteMessageID: "remote-race"}}}}
	registry := newScriptedRegistry("ttl-lease-race", sender)
	registry.setAvailable(true)
	service := newMessagingTestService(t, store, registry, clock)

	submission := mustSendText(t, service, ttlCommand("key-ttl-lease-race", time.Minute))
	clock.Advance(2 * time.Minute)

	// DispatchDue without a prior expiry sweep: the lease query itself must
	// exclude the expired row, so a scheduling race can never transmit stale.
	if processed, err := service.DispatchDue(context.Background(), 8); err != nil || processed != 0 {
		t.Fatalf("DispatchDue(expired, no sweep) = %d, %v; want 0, nil", processed, err)
	}
	if got := sender.requestCount(); got != 0 {
		t.Fatalf("transport called %d times, want 0", got)
	}
	if got := mustDelivery(t, service, submission.OutboxID).State; got != OutboxQueued {
		t.Fatalf("state = %q, want still queued until the sweep cancels it", got)
	}
}

func TestNearDuplicateSendBlockedThenForced(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	registry := newScriptedRegistry("dup-guard", &scriptedTextSender{steps: []sendStep{
		{result: bridge.SendResult{RemoteMessageID: "remote-first"}},
		{result: bridge.SendResult{RemoteMessageID: "remote-forced"}},
	}})
	registry.setAvailable(true)
	service := newMessagingTestService(t, store, registry, clock)

	first := mustSendText(t, service, SendTextCommand{
		CommonCommand: testCommonCommand("key-dup-first"),
		Body:          "Lunch tomorrow at noon at Sfoglina?",
	})

	// The incident retry: near-identical body, new idempotency key, minutes
	// later. Must be blocked with the prior intent named.
	clock.Advance(2 * time.Minute)
	_, err := service.SendText(context.Background(), SendTextCommand{
		CommonCommand: testCommonCommand("key-dup-second"),
		Body:          "Lunch today at noon at Sfoglina?",
	})
	if !errors.Is(err, ErrDuplicateSend) {
		t.Fatalf("near-duplicate error = %v, want ErrDuplicateSend", err)
	}
	var duplicate *DuplicateSendError
	if !errors.As(err, &duplicate) {
		t.Fatalf("error %v does not unwrap to *DuplicateSendError", err)
	}
	if duplicate.PriorOutboxID != first.OutboxID {
		t.Fatalf("prior outbox = %q, want %q", duplicate.PriorOutboxID, first.OutboxID)
	}
	if duplicate.PriorState != OutboxQueued {
		t.Fatalf("prior state = %q, want queued", duplicate.PriorState)
	}

	// Force is the explicit override for a deliberate repeat.
	forced := testCommonCommand("key-dup-forced")
	forced.Force = true
	mustSendText(t, service, SendTextCommand{
		CommonCommand: forced,
		Body:          "Lunch today at noon at Sfoglina?",
	})
}

func TestNearDuplicateGuardScope(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	registry := newScriptedRegistry("dup-scope", &scriptedTextSender{})
	service := newMessagingTestService(t, store, registry, clock)

	seedConversation(t, store, "account-1", "conversation-2", clock.Now())

	mustSendText(t, service, SendTextCommand{
		CommonCommand: testCommonCommand("key-scope-first"),
		Body:          "Lunch tomorrow at noon at Sfoglina?",
	})

	// A different message to the same conversation passes.
	mustSendText(t, service, SendTextCommand{
		CommonCommand: testCommonCommand("key-scope-different"),
		Body:          "Completely unrelated: did you see the game?",
	})

	// The same message to a DIFFERENT conversation passes.
	other := testCommonCommand("key-scope-other-conversation")
	other.ConversationID = "conversation-2"
	mustSendText(t, service, SendTextCommand{
		CommonCommand: other,
		Body:          "Lunch tomorrow at noon at Sfoglina?",
	})

	// Short conversational repeats pass ("ok" / "ok!").
	mustSendText(t, service, SendTextCommand{
		CommonCommand: testCommonCommand("key-scope-ok-1"),
		Body:          "ok",
	})
	mustSendText(t, service, SendTextCommand{
		CommonCommand: testCommonCommand("key-scope-ok-2"),
		Body:          "ok!",
	})

	// Outside the window, the same body passes again.
	clock.Advance(11 * time.Minute)
	mustSendText(t, service, SendTextCommand{
		CommonCommand: testCommonCommand("key-scope-after-window"),
		Body:          "Lunch tomorrow at noon at Sfoglina?",
	})
}

func TestSameKeyReplayBypassesDuplicateGuard(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	registry := newScriptedRegistry("dup-replay", &scriptedTextSender{})
	service := newMessagingTestService(t, store, registry, clock)

	first := mustSendText(t, service, SendTextCommand{
		CommonCommand: testCommonCommand("key-replay"),
		Body:          "exact same send, lost response",
	})
	// Replaying with the SAME idempotency key is the documented safe retry
	// and must reach enqueue-level deduplication, not the guard.
	replay := mustSendText(t, service, SendTextCommand{
		CommonCommand: testCommonCommand("key-replay"),
		Body:          "exact same send, lost response",
	})
	if replay.OutboxID != first.OutboxID || !replay.Deduplicated {
		t.Fatalf("replay = %+v, want deduplicated original %q", replay, first.OutboxID)
	}
}

func TestTextsNearDuplicateThreshold(t *testing.T) {
	tests := []struct {
		name string
		a, b string
		want bool
	}{
		{name: "identical", a: "see you soon", b: "see you soon", want: true},
		{name: "case and whitespace", a: "See  you soon", b: "see you SOON", want: true},
		{name: "one word swapped", a: "Lunch tomorrow at noon at Sfoglina?", b: "Lunch today at noon at Sfoglina?", want: true},
		{name: "different messages", a: "Lunch tomorrow?", b: "Did you see the game last night?", want: false},
		{name: "short repeats differ", a: "ok", b: "ok!", want: false},
		{name: "empty never matches", a: "", b: "", want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := textsNearDuplicate(test.a, test.b, defaultDuplicateThreshold); got != test.want {
				t.Fatalf("textsNearDuplicate(%q, %q) = %v, want %v", test.a, test.b, got, test.want)
			}
		})
	}
}

// seedConversation adds a second conversation for cross-conversation guard
// scope checks, mirroring seedMessagingStore's shape.
func seedConversation(t *testing.T, store *sqlite.Store, accountID, conversationID string, now time.Time) {
	t.Helper()
	if err := store.UpsertConversation(sqlite.Conversation{
		ConversationID:       conversationID,
		AccountID:            accountID,
		RemoteConversationID: "remote-" + conversationID,
		Kind:                 sqlite.ConversationKindDirect,
		Title:                "Second conversation",
		NotificationMode:     sqlite.NotificationModeAll,
		MetadataJSON:         `{}`,
		CreatedAtMS:          now.UnixMilli(),
		UpdatedAtMS:          now.UnixMilli(),
	}); err != nil {
		t.Fatalf("seed conversation %q: %v", conversationID, err)
	}
}
