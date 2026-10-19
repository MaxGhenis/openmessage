package messaging

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"strings"
	"sync"
	"testing"
	"testing/quick"
	"time"

	"github.com/rs/zerolog"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
	"github.com/maxghenis/openmessage/internal/v2keys"
)

// I4: backoff is monotone, bounded in [base, 5 min], and never overflows.
func TestRetryBackoffScheduleAndBounds(t *testing.T) {
	want := []time.Duration{
		5 * time.Second,
		10 * time.Second,
		20 * time.Second,
		40 * time.Second,
		80 * time.Second,
		160 * time.Second,
		maxRetryBackoff,
		maxRetryBackoff,
	}
	for index, delay := range want {
		attempts := int64(index + 1)
		if got := retryBackoff(defaultRetryDelay, attempts); got != delay {
			t.Fatalf("retryBackoff(5s, %d) = %v, want %v", attempts, got, delay)
		}
	}
	// The default budget rejects on attempt 6, so the waits it ever uses are
	// the first five: about 2.6 minutes from the first consuming failure.
	var total time.Duration
	for attempts := int64(1); attempts < maxTransportAttempts; attempts++ {
		total += retryBackoff(defaultRetryDelay, attempts)
	}
	if total != 155*time.Second {
		t.Fatalf("total backoff before exhaustion = %v, want 155s", total)
	}

	now := messagingTestTime
	extremes := []int64{math.MinInt64, -1, 0, 1, 2, 7, 8, 63, 64, 65, math.MaxInt32, math.MaxInt64 - 1, math.MaxInt64}
	for attempts := int64(-3); attempts <= 300; attempts++ {
		extremes = append(extremes, attempts)
	}
	for _, attempts := range extremes {
		got := retryBackoff(defaultRetryDelay, attempts)
		if got < defaultRetryDelay || got > maxRetryBackoff {
			t.Fatalf("retryBackoff(5s, %d) = %v, outside [5s, 5m]", attempts, got)
		}
		if retryAt := now.Add(got); !retryAt.After(now) || retryAt.UnixMilli() <= 0 {
			t.Fatalf("retryBackoff(5s, %d) gives non-positive retry time %v", attempts, retryAt)
		}
		if attempts < math.MaxInt64 && retryBackoff(defaultRetryDelay, attempts+1) < got {
			t.Fatalf("retryBackoff(5s, %d) decreases at the next attempt", attempts)
		}
	}

	property := func(attempts int64, baseMS uint32) bool {
		base := time.Duration(baseMS%uint32(maxRetryBackoff.Milliseconds())+1) * time.Millisecond
		got := retryBackoff(base, attempts)
		if got < base || got > maxRetryBackoff {
			return false
		}
		if attempts < math.MaxInt64 && retryBackoff(base, attempts+1) < got {
			return false
		}
		return now.Add(got).After(now)
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 5000, Rand: rand.New(rand.NewSource(20261008))}); err != nil {
		t.Fatalf("retryBackoff property: %v", err)
	}

	// A base at or above the cap is used unchanged; a non-positive base falls
	// back to the default rather than producing a zero or negative wait.
	if got := retryBackoff(10*time.Minute, 4); got != 10*time.Minute {
		t.Fatalf("retryBackoff(10m, 4) = %v, want 10m", got)
	}
	if got := retryBackoff(0, 1); got != defaultRetryDelay {
		t.Fatalf("retryBackoff(0, 1) = %v, want %v", got, defaultRetryDelay)
	}
}

func TestBudgetExemptOnlyNotConnectedAndExpiredCredentials(t *testing.T) {
	tests := []struct {
		class       bridge.FailureClass
		fingerprint string
		want        bool
	}{
		{bridge.FailureTransient, "google_not_connected", true},
		{bridge.FailureTransient, "whatsapp_not_connected", true},
		{bridge.FailureTransient, "signal_not_connected", true},
		{bridge.FailureCredentialsExpired, "", true},
		{bridge.FailureCredentialsExpired, "google_auth_expired", true},
		{bridge.FailureTransient, "", false},
		{bridge.FailureTransient, "not_connected", false},
		{bridge.FailureTransient, "google_not_connected_yet", false},
		{bridge.FailureTransient, "google_conversation_not_found", false},
		{bridge.FailureTransient, bridge.FingerprintConversationMoved, false},
		{bridge.FailureRateLimited, "whatsapp_not_connected", false},
		{bridge.FailureRateLimited, "", false},
	}
	for _, test := range tests {
		got := budgetExempt(bridge.OpError{Class: test.class, Fingerprint: test.fingerprint})
		if got != test.want {
			t.Fatalf("budgetExempt(%s, %q) = %v, want %v", test.class, test.fingerprint, got, test.want)
		}
	}
}

// I5/I8: a consuming failure backs off 5, 10, 20, 40, 80 s and the sixth
// rejects as retry_exhausted; nothing is sent again afterwards.
func TestRetryBudgetBacksOffThenRejectsOnTheCappedAttempt(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	failure := bridge.OpError{
		Class:       bridge.FailureTransient,
		Operation:   "send_text",
		Fingerprint: "google_conversation_not_found",
		Dispatch:    bridge.DispatchNotCalled,
		Cause:       errors.New("transport returned no conversation"),
	}
	steps := make([]sendStep, maxTransportAttempts)
	for index := range steps {
		steps[index] = sendStep{err: failure}
	}
	steps = append(steps, sendStep{result: bridge.SendResult{RemoteMessageID: "remote-sent-again"}})
	sender := &scriptedTextSender{steps: steps}
	registry := newScriptedRegistry("budget-cap", sender)
	registry.setAvailable(true)
	service := newMessagingTestService(t, store, registry, clock)
	submission := mustSendText(t, service, SendTextCommand{
		CommonCommand: testCommonCommand("budget-cap"),
		Body:          "stuck behind a phone that answers empty",
	})
	ctx := context.Background()
	wantDetail := "[google_conversation_not_found] send_text: transient: transport returned no conversation"

	for attempt := int64(1); attempt < maxTransportAttempts; attempt++ {
		if processed, err := service.DispatchDue(ctx, 1); err != nil || processed != 1 {
			t.Fatalf("DispatchDue(attempt %d) = %d, %v; want 1, nil", attempt, processed, err)
		}
		wait := retryBackoff(defaultRetryDelay, attempt)
		delivery := mustDelivery(t, service, submission.OutboxID)
		if delivery.State != OutboxNotDispatched || delivery.AttemptCount != attempt ||
			!delivery.NextAttemptAt.Equal(clock.Now().Add(wait)) ||
			delivery.ErrorClass != "transient" || delivery.ErrorCode != "send_text" ||
			delivery.ErrorDetail != wantDetail || delivery.RetryExhausted {
			t.Fatalf("delivery after attempt %d = %+v, want not_dispatched due in %v", attempt, delivery, wait)
		}
		clock.Advance(wait - time.Millisecond)
		if processed, err := service.DispatchDue(ctx, 1); err != nil || processed != 0 {
			t.Fatalf("DispatchDue(before backoff %d) = %d, %v; want 0, nil", attempt, processed, err)
		}
		clock.Advance(time.Millisecond)
	}
	if processed, err := service.DispatchDue(ctx, 1); err != nil || processed != 1 {
		t.Fatalf("DispatchDue(capped attempt) = %d, %v; want 1, nil", processed, err)
	}

	exhausted := mustDelivery(t, service, submission.OutboxID)
	wantExhausted := "retry budget exhausted after 6 attempts; last failure transient " +
		"[google_conversation_not_found]: send_text: transient: transport returned no conversation"
	if exhausted.State != OutboxRejected || exhausted.ErrorClass != sqlite.RetryExhaustedErrorClass ||
		exhausted.ErrorCode != "send_text" || exhausted.ErrorDetail != wantExhausted ||
		!exhausted.RetryExhausted || exhausted.AttemptCount != maxTransportAttempts ||
		!exhausted.NextAttemptAt.IsZero() {
		t.Fatalf("exhausted delivery = %+v", exhausted)
	}
	if got := sender.requestCount(); got != int(maxTransportAttempts) {
		t.Fatalf("transport calls = %d, want %d", got, maxTransportAttempts)
	}
	clock.Advance(time.Hour)
	if processed, err := service.DispatchDue(ctx, 1); err != nil || processed != 0 {
		t.Fatalf("DispatchDue(after exhaustion) = %d, %v; want 0, nil", processed, err)
	}
	if got := sender.requestCount(); got != int(maxTransportAttempts) {
		t.Fatalf("transport calls after exhaustion = %d, want %d", got, maxTransportAttempts)
	}

	// The tray keeps the exhausted send visible with the same verdict.
	pending, err := service.ListPending(ctx, ListPendingQuery{Limit: 10})
	if err != nil {
		t.Fatalf("ListPending(): %v", err)
	}
	if len(pending) != 1 || pending[0].OutboxID != submission.OutboxID ||
		pending[0].State != OutboxRejected || !pending[0].RetryExhausted ||
		pending[0].ErrorDetail != wantExhausted || pending[0].AttemptCount != maxTransportAttempts {
		t.Fatalf("ListPending() = %+v, want the exhausted send", pending)
	}

	// Send again starts a fresh budget under a new request ID.
	again, err := service.SendAgain(ctx, submission.OutboxID, "budget-cap-again")
	if err != nil {
		t.Fatalf("SendAgain(): %v", err)
	}
	if processed, err := service.DispatchDue(ctx, 1); err != nil || processed != 1 {
		t.Fatalf("DispatchDue(send again) = %d, %v; want 1, nil", processed, err)
	}
	if got := mustDelivery(t, service, again.OutboxID); got.State != OutboxConfirmed {
		t.Fatalf("sent-again delivery = %+v, want confirmed", got)
	}
	pending, err = service.ListPending(ctx, ListPendingQuery{Limit: 10})
	if err != nil {
		t.Fatalf("ListPending(after send again): %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("ListPending(after send again) = %+v, want the predecessor gone", pending)
	}
}

// I5: exempt failures keep the fixed cadence, leave attempt_count unchanged
// and never exhaust, however many there are.
func TestRetryBudgetExemptFailuresRefundAndNeverExhaust(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	exempt := []bridge.OpError{
		{Class: bridge.FailureTransient, Fingerprint: "google_not_connected"},
		{Class: bridge.FailureTransient, Fingerprint: "whatsapp_not_connected"},
		{Class: bridge.FailureTransient, Fingerprint: "signal_not_connected"},
		{Class: bridge.FailureCredentialsExpired, Fingerprint: "google_auth_expired"},
	}
	rounds := int(3 * maxTransportAttempts)
	var steps []sendStep
	for index := 0; index < rounds; index++ {
		failure := exempt[index%len(exempt)]
		failure.Operation = "send_text"
		failure.Dispatch = bridge.DispatchNotCalled
		failure.Cause = errors.New("offline")
		steps = append(steps, sendStep{err: failure})
	}
	consuming := bridge.OpError{
		Class:       bridge.FailureTransient,
		Operation:   "send_text",
		Fingerprint: "google_conversation_not_found",
		Dispatch:    bridge.DispatchNotCalled,
		Cause:       errors.New("no conversation"),
	}
	offline := exempt[0]
	offline.Operation = "send_text"
	offline.Dispatch = bridge.DispatchNotCalled
	offline.Cause = errors.New("offline")
	steps = append(steps,
		sendStep{err: consuming},
		sendStep{err: offline},
		sendStep{result: bridge.SendResult{RemoteMessageID: "remote-after-reconnect"}},
	)
	sender := &scriptedTextSender{steps: steps}
	registry := newScriptedRegistry("budget-exempt", sender)
	registry.setAvailable(true)
	service := newMessagingTestService(t, store, registry, clock)
	submission := mustSendText(t, service, SendTextCommand{
		CommonCommand: testCommonCommand("budget-exempt"),
		Body:          "composed while offline",
	})
	ctx := context.Background()

	for round := 0; round < rounds; round++ {
		if processed, err := service.DispatchDue(ctx, 1); err != nil || processed != 1 {
			t.Fatalf("DispatchDue(exempt %d) = %d, %v; want 1, nil", round, processed, err)
		}
		delivery := mustDelivery(t, service, submission.OutboxID)
		fingerprint := exempt[round%len(exempt)].Fingerprint
		if delivery.State != OutboxNotDispatched || delivery.AttemptCount != 0 ||
			!delivery.NextAttemptAt.Equal(clock.Now().Add(defaultRetryDelay)) ||
			!strings.HasPrefix(delivery.ErrorDetail, "["+fingerprint+"] ") || delivery.RetryExhausted {
			t.Fatalf("delivery after exempt failure %d = %+v", round, delivery)
		}
		clock.Advance(defaultRetryDelay)
	}

	if processed, err := service.DispatchDue(ctx, 1); err != nil || processed != 1 {
		t.Fatalf("DispatchDue(consuming) = %d, %v; want 1, nil", processed, err)
	}
	if row := mustOutboxItem(t, service, submission.OutboxID); row.AttemptCount != 1 ||
		row.NextAttemptAtMS == nil || *row.NextAttemptAtMS != clock.Now().Add(defaultRetryDelay).UnixMilli() {
		t.Fatalf("row after one consuming failure = %+v", row)
	}
	clock.Advance(defaultRetryDelay)
	if processed, err := service.DispatchDue(ctx, 1); err != nil || processed != 1 {
		t.Fatalf("DispatchDue(offline again) = %d, %v; want 1, nil", processed, err)
	}
	if row := mustOutboxItem(t, service, submission.OutboxID); row.AttemptCount != 1 {
		t.Fatalf("exempt failure after a consuming one changed attempt_count: %+v", row)
	}
	clock.Advance(defaultRetryDelay)
	if processed, err := service.DispatchDue(ctx, 1); err != nil || processed != 1 {
		t.Fatalf("DispatchDue(reconnected) = %d, %v; want 1, nil", processed, err)
	}
	if got := mustDelivery(t, service, submission.OutboxID); got.State != OutboxConfirmed ||
		got.RemoteMessageID != "remote-after-reconnect" {
		t.Fatalf("delivery after reconnect = %+v, want confirmed", got)
	}
	if got := sender.requestCount(); got != rounds+3 {
		t.Fatalf("transport calls = %d, want %d", got, rounds+3)
	}
}

// Terminal and uncertain outcomes keep the adapter fingerprint in
// error_detail too, so a reauth refusal (the phone switched to Google-account
// pairing) stays explainable and listed, but is never marked exhausted.
func TestRecordSendErrorKeepsTheFingerprintOnTerminalAndUncertainDetails(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	sender := &scriptedTextSender{steps: []sendStep{
		{err: bridge.OpError{
			Class:       bridge.FailureReauthRequired,
			Operation:   "send_text",
			Fingerprint: "google_account_pairing_switched",
			Dispatch:    bridge.DispatchNotCalled,
			Cause:       errors.New("the phone switched to Google-account pairing"),
		}},
		{err: bridge.OpError{
			Class:       bridge.FailureTransient,
			Operation:   "send_text",
			Fingerprint: "google_text_send_unknown_status",
			Dispatch:    bridge.DispatchUncertain,
			Cause:       errors.New("status UNKNOWN"),
		}},
	}}
	registry := newScriptedRegistry("fingerprint-detail", sender)
	registry.setAvailable(true)
	service := newMessagingTestService(t, store, registry, clock)
	refused := mustSendText(t, service, SendTextCommand{CommonCommand: testCommonCommand("refused"), Body: "refused"})
	unknown := mustSendText(t, service, SendTextCommand{CommonCommand: testCommonCommand("unknown"), Body: "unknown"})
	if processed, err := service.DispatchDue(context.Background(), 4); err != nil || processed != 2 {
		t.Fatalf("DispatchDue() = %d, %v; want 2, nil", processed, err)
	}
	if got := mustDelivery(t, service, refused.OutboxID); got.State != OutboxRejected ||
		got.ErrorClass != "reauth_required" || got.RetryExhausted ||
		got.ErrorDetail != "[google_account_pairing_switched] send_text: reauth_required: the phone switched to Google-account pairing" {
		t.Fatalf("refused delivery = %+v", got)
	}
	if got := mustDelivery(t, service, unknown.OutboxID); got.State != OutboxUncertain ||
		got.ErrorDetail != "[google_text_send_unknown_status] send_text: transient: status UNKNOWN" {
		t.Fatalf("unknown-status delivery = %+v", got)
	}
	pending, err := service.ListPending(context.Background(), ListPendingQuery{Limit: 10})
	if err != nil {
		t.Fatalf("ListPending(): %v", err)
	}
	listed := map[string]PendingDelivery{}
	for _, delivery := range pending {
		listed[delivery.OutboxID] = delivery
	}
	if got, ok := listed[refused.OutboxID]; !ok || got.RetryExhausted ||
		!strings.HasPrefix(got.ErrorDetail, "[google_account_pairing_switched] ") {
		t.Fatalf("listed refusal = %+v (present %v), want it shown and not exhausted", got, ok)
	}
}

// An adapter's RetryAt is a floor under the backoff, never a way to retry
// sooner; exempt failures keep honouring it too.
func TestRetryBudgetHonoursAdapterRetryAt(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	banEnds := clock.Now().Add(time.Hour)
	sender := &scriptedTextSender{steps: []sendStep{
		{err: bridge.OpError{
			Class:     bridge.FailureRateLimited,
			Operation: "send_text",
			RetryAt:   banEnds,
			Dispatch:  bridge.DispatchNotCalled,
			Cause:     errors.New("temporary ban"),
		}},
		{err: bridge.OpError{
			Class:     bridge.FailureTransient,
			Operation: "send_text",
			RetryAt:   banEnds.Add(time.Second),
			Dispatch:  bridge.DispatchNotCalled,
			Cause:     errors.New("adapter asks for one second"),
		}},
		{err: bridge.OpError{
			Class:       bridge.FailureCredentialsExpired,
			Operation:   "send_text",
			Fingerprint: "repairing",
			RetryAt:     banEnds.Add(2 * time.Hour),
			Dispatch:    bridge.DispatchNotCalled,
			Cause:       errors.New("repairing credentials"),
		}},
		// Dispatched at banEnds+2h: asks for one second, gets the 5 s cadence.
		{err: bridge.OpError{
			Class:       bridge.FailureTransient,
			Operation:   "send_text",
			Fingerprint: "google_not_connected",
			RetryAt:     banEnds.Add(2*time.Hour + time.Second),
			Dispatch:    bridge.DispatchNotCalled,
			Cause:       errors.New("offline"),
		}},
		// A RetryAt before the epoch must not reach MarkCalledNotDispatched,
		// which would refuse it and stop the dispatcher.
		{err: bridge.OpError{
			Class:       bridge.FailureCredentialsExpired,
			Operation:   "send_text",
			Fingerprint: "repairing",
			RetryAt:     time.UnixMilli(-1000),
			Dispatch:    bridge.DispatchNotCalled,
			Cause:       errors.New("repairing credentials"),
		}},
	}}
	registry := newScriptedRegistry("budget-retry-at", sender)
	registry.setAvailable(true)
	service := newMessagingTestService(t, store, registry, clock)
	submission := mustSendText(t, service, SendTextCommand{
		CommonCommand: testCommonCommand("budget-retry-at"),
		Body:          "after the ban",
	})
	ctx := context.Background()

	if processed, err := service.DispatchDue(ctx, 1); err != nil || processed != 1 {
		t.Fatalf("DispatchDue(rate limited) = %d, %v", processed, err)
	}
	if got := mustDelivery(t, service, submission.OutboxID); !got.NextAttemptAt.Equal(banEnds) ||
		got.AttemptCount != 1 {
		t.Fatalf("rate-limited delivery = %+v, want next attempt at the ban end", got)
	}
	clock.Advance(time.Hour)
	if processed, err := service.DispatchDue(ctx, 1); err != nil || processed != 1 {
		t.Fatalf("DispatchDue(short retry-at) = %d, %v", processed, err)
	}
	// The adapter asked for one second; the second consuming failure backs off 10 s.
	if got := mustDelivery(t, service, submission.OutboxID); !got.NextAttemptAt.Equal(clock.Now().Add(10*time.Second)) ||
		got.AttemptCount != 2 {
		t.Fatalf("delivery after short retry-at = %+v, want the 10 s backoff", got)
	}
	clock.Advance(10 * time.Second)
	if processed, err := service.DispatchDue(ctx, 1); err != nil || processed != 1 {
		t.Fatalf("DispatchDue(exempt retry-at) = %d, %v", processed, err)
	}
	if got := mustDelivery(t, service, submission.OutboxID); !got.NextAttemptAt.Equal(banEnds.Add(2*time.Hour)) ||
		got.AttemptCount != 2 {
		t.Fatalf("exempt delivery = %+v, want the adapter's retry-at and a refunded attempt", got)
	}
	// An adapter RetryAt never hurries an exempt retry below the fixed cadence.
	clock.Advance(banEnds.Add(2 * time.Hour).Sub(clock.Now()))
	for _, label := range []string{"earlier than the cadence", "before the epoch"} {
		if processed, err := service.DispatchDue(ctx, 1); err != nil || processed != 1 {
			t.Fatalf("DispatchDue(exempt retry-at %s) = %d, %v; want 1, nil", label, processed, err)
		}
		if got := mustDelivery(t, service, submission.OutboxID); got.State != OutboxNotDispatched ||
			!got.NextAttemptAt.Equal(clock.Now().Add(defaultRetryDelay)) || got.AttemptCount != 2 {
			t.Fatalf("exempt delivery with a retry-at %s = %+v, want the 5 s cadence", label, got)
		}
		clock.Advance(defaultRetryDelay)
	}
	if got := sender.requestCount(); got != 5 {
		t.Fatalf("transport calls = %d, want 5", got)
	}
}

// I6: the budget only ever rejects after DispatchNotCalled. Ambiguous
// outcomes stay uncertain even one attempt below the cap.
func TestRetryBudgetNeverRejectsAnUncertainOutcome(t *testing.T) {
	tests := []struct {
		name    string
		failure error
	}{
		{name: "explicitly uncertain", failure: bridge.OpError{
			Class: bridge.FailureTransient, Operation: "send_text", Dispatch: bridge.DispatchUncertain,
			Cause: errors.New("phone answered UNKNOWN"),
		}},
		{name: "retryable with unknown dispatch", failure: bridge.OpError{
			Class: bridge.FailureRateLimited, Operation: "send_text",
			Cause: errors.New("throttled mid-call"),
		}},
		{name: "unclassified timeout", failure: context.DeadlineExceeded},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clock := newManualClock(messagingTestTime)
			store, raw := openReconcileTestStore(t, clock.Now())
			sender := &scriptedTextSender{steps: []sendStep{{err: test.failure}}}
			registry := newScriptedRegistry("budget-uncertain", sender)
			registry.setAvailable(true)
			service := newMessagingTestService(t, store, registry, clock)
			submission := mustSendText(t, service, SendTextCommand{
				CommonCommand: testCommonCommand("budget-uncertain"),
				Body:          "maybe sent",
			})
			mustReconcileExecOne(t, raw,
				`UPDATE outbox SET attempt_count = ? WHERE outbox_id = ?`,
				maxTransportAttempts-1, submission.OutboxID)

			if processed, err := service.DispatchDue(context.Background(), 1); err != nil || processed != 1 {
				t.Fatalf("DispatchDue() = %d, %v; want 1, nil", processed, err)
			}
			got := mustDelivery(t, service, submission.OutboxID)
			if got.State != OutboxUncertain || got.RetryExhausted ||
				got.AttemptCount != maxTransportAttempts {
				t.Fatalf("delivery = %+v, want uncertain (never rejected)", got)
			}
		})
	}
}

// I7: a row already at or over the cap (the live incident's row was past
// 1900) is rejected before any transport is acquired, for every kind.
func TestRetryBudgetRejectsOverCapRowsBeforeCallingTheTransport(t *testing.T) {
	kinds := []struct {
		name      string
		operation string
		submit    func(t *testing.T, service *MessageService, store *sqlite.Store, clock *manualClock) Submission
	}{
		{name: "text", operation: textOperation, submit: func(t *testing.T, service *MessageService, _ *sqlite.Store, _ *manualClock) Submission {
			return mustSendText(t, service, SendTextCommand{
				CommonCommand: testCommonCommand("over-cap-text"),
				Body:          "must never fire late",
			})
		}},
		{name: "media", operation: mediaOperation, submit: func(t *testing.T, service *MessageService, _ *sqlite.Store, _ *manualClock) Submission {
			return mustSendDispatchMedia(t, service, SendMediaCommand{
				CommonCommand: testCommonCommand("over-cap-media"),
				Content:       bytes.NewReader([]byte("late media")),
				Filename:      "late.bin",
				MIME:          "application/octet-stream",
			})
		}},
		{name: "reaction", operation: reactionOperation, submit: func(t *testing.T, service *MessageService, store *sqlite.Store, clock *manualClock) Submission {
			target := seedBudgetTarget(t, store, clock)
			return mustSendDispatchReaction(t, service, SendReactionCommand{
				CommonCommand:   testCommonCommand("over-cap-reaction"),
				TargetMessageID: target.MessageID,
				Emoji:           "👍",
			})
		}},
		{name: "read", operation: readOperation, submit: func(t *testing.T, service *MessageService, store *sqlite.Store, clock *manualClock) Submission {
			target := seedBudgetTarget(t, store, clock)
			seedDispatchDevice(t, store, "device-over-cap", clock.Now())
			return mustMarkDispatchRead(t, service, MarkReadCommand{
				CommonCommand:     testCommonCommand("over-cap-read"),
				DeviceID:          "device-over-cap",
				LastReadMessageID: target.MessageID,
			})
		}},
	}
	for _, kind := range kinds {
		for _, seeded := range []int64{maxTransportAttempts, 100, 1959} {
			t.Run(fmt.Sprintf("%s at %d", kind.name, seeded), func(t *testing.T) {
				clock := newManualClock(messagingTestTime)
				store, raw := openReconcileTestStore(t, clock.Now())
				counter := &budgetCallCounter{}
				registry := newScriptedRegistry("over-cap", counter)
				registry.setMediaSender(counter)
				registry.setReactionSender(counter)
				registry.setReadReceiptSender(counter)
				registry.setAvailable(true)
				service := newMessagingTestService(t, store, registry, clock)
				submission := kind.submit(t, service, store, clock)
				mustReconcileExecOne(t, raw,
					`UPDATE outbox SET attempt_count = ? WHERE outbox_id = ?`,
					seeded, submission.OutboxID)

				if processed, err := service.DispatchDue(context.Background(), 4); err != nil || processed != 1 {
					t.Fatalf("DispatchDue() = %d, %v; want 1, nil", processed, err)
				}
				got := mustDelivery(t, service, submission.OutboxID)
				wantDetail := fmt.Sprintf("retry budget already exhausted (%d attempts); not attempted again", seeded)
				if got.State != OutboxRejected || got.ErrorClass != sqlite.RetryExhaustedErrorClass ||
					got.ErrorCode != kind.operation || got.ErrorDetail != wantDetail ||
					got.AttemptCount != seeded || got.RetryExhausted != true {
					t.Fatalf("over-cap delivery = %+v", got)
				}
				if calls := counter.count(); calls != 0 {
					t.Fatalf("transport calls = %d, want 0", calls)
				}
			})
		}
	}

	// One below the cap still gets its last attempt.
	clock := newManualClock(messagingTestTime)
	store, raw := openReconcileTestStore(t, clock.Now())
	counter := &budgetCallCounter{}
	registry := newScriptedRegistry("below-cap", counter)
	registry.setAvailable(true)
	service := newMessagingTestService(t, store, registry, clock)
	submission := mustSendText(t, service, SendTextCommand{
		CommonCommand: testCommonCommand("below-cap"),
		Body:          "last chance",
	})
	mustReconcileExecOne(t, raw,
		`UPDATE outbox SET attempt_count = ? WHERE outbox_id = ?`,
		maxTransportAttempts-1, submission.OutboxID)
	if processed, err := service.DispatchDue(context.Background(), 1); err != nil || processed != 1 {
		t.Fatalf("DispatchDue(below cap) = %d, %v; want 1, nil", processed, err)
	}
	if calls := counter.count(); calls != 1 {
		t.Fatalf("transport calls below the cap = %d, want 1", calls)
	}
	if got := mustDelivery(t, service, submission.OutboxID); got.State != OutboxConfirmed {
		t.Fatalf("below-cap delivery = %+v, want confirmed", got)
	}
}

// A conversation_moved refusal rebinds the conversation and retries at once
// under the new remote ID, displacing any stale holder of that ID.
func TestRetryBudgetConversationMovedRebindsAndRetriesUnderTheNewID(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	seedBudgetConversation(t, store, clock, "conversation-holder", "remote-conversation-new", sqlite.ConversationKindDirect)
	sender := &scriptedTextSender{steps: []sendStep{
		{err: movedFailure("remote-conversation", "remote-conversation-new")},
		{result: bridge.SendResult{RemoteMessageID: "remote-sent-after-move"}},
	}}
	registry := newScriptedRegistry("budget-moved", sender)
	registry.setAvailable(true)
	service := newMessagingTestService(t, store, registry, clock)
	submission := mustSendText(t, service, SendTextCommand{
		CommonCommand: testCommonCommand("budget-moved"),
		Body:          "after the phone re-keyed the thread",
	})
	ctx := context.Background()

	if processed, err := service.DispatchDue(ctx, 1); err != nil || processed != 1 {
		t.Fatalf("DispatchDue(moved) = %d, %v; want 1, nil", processed, err)
	}
	moved := mustDelivery(t, service, submission.OutboxID)
	if moved.State != OutboxNotDispatched || moved.AttemptCount != 1 ||
		!moved.NextAttemptAt.Equal(clock.Now()) ||
		!strings.HasPrefix(moved.ErrorDetail, "[conversation_moved] ") ||
		!strings.Contains(moved.ErrorDetail, `rebound the conversation to remote ID "remote-conversation-new"`) {
		t.Fatalf("delivery after move = %+v, want an immediate retry", moved)
	}
	conversation, err := store.GetConversation("conversation-1")
	if err != nil {
		t.Fatalf("GetConversation(): %v", err)
	}
	if conversation.RemoteConversationID != "remote-conversation-new" {
		t.Fatalf("conversation binding = %q, want the moved-to ID", conversation.RemoteConversationID)
	}
	holder, err := store.GetConversation("conversation-holder")
	if err != nil {
		t.Fatalf("GetConversation(holder): %v", err)
	}
	if !strings.HasPrefix(holder.RemoteConversationID, sqlite.DisplacedRemoteIDPrefix) {
		t.Fatalf("stale holder binding = %q, want displaced", holder.RemoteConversationID)
	}

	// Immediately due: no clock advance.
	if processed, err := service.DispatchDue(ctx, 1); err != nil || processed != 1 {
		t.Fatalf("DispatchDue(after rebind) = %d, %v; want 1, nil", processed, err)
	}
	requests := sender.snapshotRequests()
	if len(requests) != 2 || requests[0].Conversation.RemoteID != "remote-conversation" ||
		requests[1].Conversation.RemoteID != "remote-conversation-new" ||
		requests[0].RequestID != requests[1].RequestID {
		t.Fatalf("requests = %+v, want the retry under the new ID with the same request ID", requests)
	}
	if got := mustDelivery(t, service, submission.OutboxID); got.State != OutboxConfirmed {
		t.Fatalf("delivery after rebind = %+v, want confirmed", got)
	}
}

// A move whose rebind is refused is an ordinary consuming failure; a move on
// the capped attempt still rebinds but exhausts.
func TestRetryBudgetConversationMovedRebindFailureAndCap(t *testing.T) {
	t.Run("binding changed since the attempt", func(t *testing.T) {
		clock := newManualClock(messagingTestTime)
		store := openMessagingTestStore(t, clock.Now())
		sender := &scriptedTextSender{steps: []sendStep{
			{err: movedFailure("remote-somewhere-else", "remote-conversation-new")},
		}}
		registry := newScriptedRegistry("budget-moved-stale", sender)
		registry.setAvailable(true)
		service := newMessagingTestService(t, store, registry, clock)
		submission := mustSendText(t, service, SendTextCommand{
			CommonCommand: testCommonCommand("budget-moved-stale"),
			Body:          "stale move",
		})
		if processed, err := service.DispatchDue(context.Background(), 1); err != nil || processed != 1 {
			t.Fatalf("DispatchDue() = %d, %v; want 1, nil", processed, err)
		}
		got := mustDelivery(t, service, submission.OutboxID)
		if got.State != OutboxNotDispatched || got.AttemptCount != 1 ||
			!got.NextAttemptAt.Equal(clock.Now().Add(defaultRetryDelay)) ||
			!strings.Contains(got.ErrorDetail, `; rebind failed: reassign remote conversation ID "remote-conversation-new": conversation "conversation-1" is bound to "remote-conversation", not "remote-somewhere-else"`) {
			t.Fatalf("delivery = %+v, want a consuming backoff failure naming the refused rebind", got)
		}
		if conversation, err := store.GetConversation("conversation-1"); err != nil ||
			conversation.RemoteConversationID != "remote-conversation" {
			t.Fatalf("conversation = %+v, %v; want the binding untouched", conversation, err)
		}
	})

	t.Run("empty moved-to ID", func(t *testing.T) {
		clock := newManualClock(messagingTestTime)
		store := openMessagingTestStore(t, clock.Now())
		sender := &scriptedTextSender{steps: []sendStep{{err: movedFailure("remote-conversation", " ")}}}
		registry := newScriptedRegistry("budget-moved-empty", sender)
		registry.setAvailable(true)
		service := newMessagingTestService(t, store, registry, clock)
		submission := mustSendText(t, service, SendTextCommand{
			CommonCommand: testCommonCommand("budget-moved-empty"),
			Body:          "empty move",
		})
		if processed, err := service.DispatchDue(context.Background(), 1); err != nil || processed != 1 {
			t.Fatalf("DispatchDue() = %d, %v; want 1, nil", processed, err)
		}
		if got := mustDelivery(t, service, submission.OutboxID); got.State != OutboxNotDispatched ||
			!got.NextAttemptAt.Equal(clock.Now().Add(defaultRetryDelay)) ||
			!strings.Contains(got.ErrorDetail, "rebind failed") {
			t.Fatalf("delivery = %+v, want a consuming failure", got)
		}
	})

	t.Run("move on the capped attempt", func(t *testing.T) {
		clock := newManualClock(messagingTestTime)
		store, raw := openReconcileTestStore(t, clock.Now())
		sender := &scriptedTextSender{steps: []sendStep{
			{err: movedFailure("remote-conversation", "remote-conversation-new")},
		}}
		registry := newScriptedRegistry("budget-moved-cap", sender)
		registry.setAvailable(true)
		service := newMessagingTestService(t, store, registry, clock)
		submission := mustSendText(t, service, SendTextCommand{
			CommonCommand: testCommonCommand("budget-moved-cap"),
			Body:          "flip-flopping phone",
		})
		mustReconcileExecOne(t, raw,
			`UPDATE outbox SET attempt_count = ? WHERE outbox_id = ?`,
			maxTransportAttempts-1, submission.OutboxID)
		if processed, err := service.DispatchDue(context.Background(), 1); err != nil || processed != 1 {
			t.Fatalf("DispatchDue() = %d, %v; want 1, nil", processed, err)
		}
		got := mustDelivery(t, service, submission.OutboxID)
		if got.State != OutboxRejected || !got.RetryExhausted ||
			!strings.HasPrefix(got.ErrorDetail, "retry budget exhausted after 6 attempts; last failure transient [conversation_moved]: ") ||
			!strings.Contains(got.ErrorDetail, "rebound the conversation") {
			t.Fatalf("delivery = %+v, want exhausted after the rebind", got)
		}
		if conversation, err := store.GetConversation("conversation-1"); err != nil ||
			conversation.RemoteConversationID != "remote-conversation-new" {
			t.Fatalf("conversation = %+v, %v; want the rebind kept", conversation, err)
		}
	})
}

// Both rebind outcomes are logged through the logger the caller's context
// carries, with the IDs needed to trace the move.
func TestRetryBudgetConversationMovedRebindIsLogged(t *testing.T) {
	tests := []struct {
		name      string
		from      string
		wantLevel string
		wantMsg   string
		wantError string
	}{
		{
			name:      "rebound",
			from:      "remote-conversation",
			wantLevel: "info",
			wantMsg:   "Outbox rebound a moved conversation before sending",
		},
		{
			name:      "refused",
			from:      "remote-somewhere-else",
			wantLevel: "warn",
			wantMsg:   "Outbox could not rebind a moved conversation",
			wantError: `reassign remote conversation ID "remote-conversation-new": conversation "conversation-1" is bound to "remote-conversation", not "remote-somewhere-else": conversation is bound to a different remote ID`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clock := newManualClock(messagingTestTime)
			store := openMessagingTestStore(t, clock.Now())
			sender := &scriptedTextSender{steps: []sendStep{
				{err: movedFailure(test.from, "remote-conversation-new")},
			}}
			registry := newScriptedRegistry("budget-moved-log", sender)
			registry.setAvailable(true)
			service := newMessagingTestService(t, store, registry, clock)
			submission := mustSendText(t, service, SendTextCommand{
				CommonCommand: testCommonCommand("budget-moved-log"),
				Body:          "logged move",
			})
			var output bytes.Buffer
			ctx := zerolog.New(&output).WithContext(context.Background())
			if processed, err := service.DispatchDue(ctx, 1); err != nil || processed != 1 {
				t.Fatalf("DispatchDue() = %d, %v; want 1, nil", processed, err)
			}

			lines := strings.Split(strings.TrimSpace(output.String()), "\n")
			if len(lines) != 1 {
				t.Fatalf("log lines = %q, want exactly one", output.String())
			}
			var entry map[string]any
			if err := json.Unmarshal([]byte(lines[0]), &entry); err != nil {
				t.Fatalf("decode log line %q: %v", lines[0], err)
			}
			want := map[string]any{
				"level":           test.wantLevel,
				"message":         test.wantMsg,
				"outbox_id":       submission.OutboxID,
				"account_id":      "account-1",
				"conversation_id": "conversation-1",
				"from_remote_id":  test.from,
				"to_remote_id":    "remote-conversation-new",
				"attempt":         float64(1),
			}
			if test.wantError != "" {
				want["error"] = test.wantError
			}
			for key, value := range want {
				if entry[key] != value {
					t.Fatalf("log %q = %v, want %v (entry %v)", key, entry[key], value, entry)
				}
			}
			if _, ok := entry["error"]; ok != (test.wantError != "") {
				t.Fatalf("log entry %v: error field present = %v", entry, ok)
			}
		})
	}
}

// B3: a direct conversation with exactly one canonical E.164 peer gives text
// and media requests its number; everything else leaves it empty.
func TestDispatchConversationRefCarriesKindAndDirectPeerNumber(t *testing.T) {
	tests := []struct {
		name       string
		kind       sqlite.ConversationKind
		peers      []sqlite.Identity
		wantNumber string
	}{
		{name: "direct e164 peer", kind: sqlite.ConversationKindDirect,
			peers: []sqlite.Identity{budgetIdentity("peer-1", e164IdentityKind, "+15551234567")}, wantNumber: "+15551234567"},
		{name: "direct without participants", kind: sqlite.ConversationKindDirect},
		{name: "direct email peer", kind: sqlite.ConversationKindDirect,
			peers: []sqlite.Identity{budgetIdentity("peer-1", sqlite.IdentityKind("email"), "peer@example.test")}},
		{name: "direct non-canonical number", kind: sqlite.ConversationKindDirect,
			peers: []sqlite.Identity{budgetIdentity("peer-1", e164IdentityKind, "+1 555 123 4567")}},
		{name: "direct with two peers", kind: sqlite.ConversationKindDirect,
			peers: []sqlite.Identity{
				budgetIdentity("peer-1", e164IdentityKind, "+15551234567"),
				budgetIdentity("peer-2", e164IdentityKind, "+15557654321"),
			}},
		{name: "group with one e164 peer", kind: sqlite.ConversationKindGroup,
			peers: []sqlite.Identity{budgetIdentity("peer-1", e164IdentityKind, "+15551234567")}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clock := newManualClock(messagingTestTime)
			store := openMessagingTestStore(t, clock.Now())
			seedBudgetConversation(t, store, clock, "conversation-1", "remote-conversation", test.kind)
			self := budgetIdentity("self", e164IdentityKind, "+15550000000")
			self.IsSelf = true
			seedBudgetParticipants(t, store, clock, "conversation-1", append([]sqlite.Identity{self}, test.peers...))

			text := &scriptedTextSender{}
			media := &scriptedMediaSender{}
			registry := newScriptedRegistry("conversation-ref", text)
			registry.setMediaSender(media)
			registry.setAvailable(true)
			service := newMessagingTestService(t, store, registry, clock)
			mustSendText(t, service, SendTextCommand{CommonCommand: testCommonCommand("ref-text"), Body: "hi"})
			mustSendDispatchMedia(t, service, SendMediaCommand{
				CommonCommand: testCommonCommand("ref-media"),
				Content:       bytes.NewReader([]byte("media")),
				Filename:      "ref.bin",
				MIME:          "application/octet-stream",
			})
			if processed, err := service.DispatchDue(context.Background(), 4); err != nil || processed != 2 {
				t.Fatalf("DispatchDue() = %d, %v; want 2, nil", processed, err)
			}
			want := bridge.ConversationRef{
				RemoteID:         "remote-conversation",
				Kind:             string(test.kind),
				DirectPeerNumber: test.wantNumber,
			}
			if requests := text.snapshotRequests(); len(requests) != 1 || requests[0].Conversation != want {
				t.Fatalf("text requests = %+v, want conversation %+v", requests, want)
			}
			if requests := media.snapshotRequests(); len(requests) != 1 || requests[0].Conversation != want {
				t.Fatalf("media requests = %+v, want conversation %+v", requests, want)
			}
		})
	}

	// Reactions and read receipts carry the kind but never the number: no
	// adapter re-resolves those by number.
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	seedBudgetParticipants(t, store, clock, "conversation-1", []sqlite.Identity{
		budgetIdentity("peer-1", e164IdentityKind, "+15551234567"),
	})
	target := seedBudgetTarget(t, store, clock)
	seedDispatchDevice(t, store, "device-ref", clock.Now())
	reaction := &scriptedReactionSender{steps: []sendStep{{}}}
	read := &scriptedReadReceiptSender{steps: []readReceiptStep{{}}}
	registry := newScriptedRegistry("conversation-ref-other", &scriptedTextSender{})
	registry.setReactionSender(reaction)
	registry.setReadReceiptSender(read)
	registry.setAvailable(true)
	service := newMessagingTestService(t, store, registry, clock)
	mustSendDispatchReaction(t, service, SendReactionCommand{
		CommonCommand: testCommonCommand("ref-reaction"), TargetMessageID: target.MessageID, Emoji: "👍",
	})
	mustMarkDispatchRead(t, service, MarkReadCommand{
		CommonCommand: testCommonCommand("ref-read"), DeviceID: "device-ref", LastReadMessageID: target.MessageID,
	})
	if processed, err := service.DispatchDue(context.Background(), 4); err != nil || processed != 2 {
		t.Fatalf("DispatchDue(reaction, read) = %d, %v; want 2, nil", processed, err)
	}
	want := bridge.ConversationRef{RemoteID: "remote-conversation", Kind: "direct"}
	if requests := reaction.snapshotRequests(); len(requests) != 1 || requests[0].Conversation != want {
		t.Fatalf("reaction requests = %+v, want conversation %+v", requests, want)
	}
	if requests := read.snapshotRequests(); len(requests) != 1 || requests[0].Conversation != want {
		t.Fatalf("read requests = %+v, want conversation %+v", requests, want)
	}
}

// The E.164 kind constant must match what live ingest mints for a phone number.
func TestE164IdentityKindMatchesIngestKeys(t *testing.T) {
	for _, raw := range []string{"+15551234567", " +1 (555) 123-4567 ", "+447700900123"} {
		key, err := v2keys.IdentityKey("google-primary", "sms", raw)
		if err != nil {
			t.Fatalf("IdentityKey(%q): %v", raw, err)
		}
		if sqlite.IdentityKind(key.Kind) != e164IdentityKind || !canonicalE164(key.Canonical) {
			t.Fatalf("IdentityKey(%q) = %+v, want kind %q and a canonical number", raw, key, e164IdentityKind)
		}
	}
	for _, value := range []string{"", "+", "15551234567", "+1555-123", "+1234567890123456", "++15551234567"} {
		if canonicalE164(value) {
			t.Fatalf("canonicalE164(%q) = true", value)
		}
	}
}

// I8: RetryExhausted is true iff the state is rejected and the class is
// retry_exhausted; NextAttemptAt is only reported for not_dispatched.
func TestDeliveryFromItemRetryExhaustedAndNextAttemptExhaustive(t *testing.T) {
	states := []OutboxState{
		OutboxQueued, OutboxDispatching, OutboxNotDispatched, OutboxUncertain,
		OutboxConfirmed, OutboxStoreFailed, OutboxRejected, OutboxCanceled,
	}
	classes := []*string{
		nil,
		stringPointer(""),
		stringPointer("transient"),
		stringPointer("reauth_required"),
		stringPointer("permanent"),
		stringPointer("ttl"),
		stringPointer(sqlite.RetryExhaustedErrorClass),
	}
	nextAttemptMS := messagingTestTime.Add(time.Minute).UnixMilli()
	for _, state := range states {
		for _, class := range classes {
			for _, next := range []*int64{nil, &nextAttemptMS} {
				detail := "why"
				item := sqlite.OutboxItem{
					OutboxID:        "outbox",
					State:           state,
					ErrorClass:      class,
					ErrorDetail:     &detail,
					AttemptCount:    3,
					NextAttemptAtMS: next,
				}
				delivery := deliveryFromItem(item)
				wantExhausted := state == OutboxRejected && class != nil && *class == sqlite.RetryExhaustedErrorClass
				if delivery.RetryExhausted != wantExhausted {
					t.Fatalf("deliveryFromItem(%s, %v).RetryExhausted = %v", state, stringValue(class), delivery.RetryExhausted)
				}
				if retryExhausted(state, stringValue(class)) != wantExhausted {
					t.Fatalf("retryExhausted(%s, %v) disagrees with Delivery", state, stringValue(class))
				}
				wantNext := state == OutboxNotDispatched && next != nil
				if delivery.NextAttemptAt.IsZero() == wantNext ||
					(wantNext && !delivery.NextAttemptAt.Equal(time.UnixMilli(nextAttemptMS))) {
					t.Fatalf("deliveryFromItem(%s, next %v).NextAttemptAt = %v", state, next, delivery.NextAttemptAt)
				}
				if delivery.ErrorDetail != "why" || delivery.AttemptCount != 3 {
					t.Fatalf("deliveryFromItem(%s) = %+v, want detail and attempts", state, delivery)
				}
			}
		}
	}
}

type budgetStepKind uint8

const (
	budgetConsumingTransient budgetStepKind = iota
	budgetConsumingRateLimited
	budgetConsumingRetryAt
	budgetConsumingMoved
	budgetExemptNotConnected
	budgetExemptCredentials
	budgetUncertain
	budgetUnknownDispatch
	budgetSuccess
	budgetStepKinds
)

// I5/I6/I9 as a property over arbitrary failure sequences: at most
// maxTransportAttempts consuming transport calls; rejection happens exactly on
// the consuming call that reaches the cap and only after DispatchNotCalled;
// exempt failures never change attempt_count; every attempt after a move
// carries the rebound remote ID.
func TestRetryBudgetInvariantsHoldForAnyFailureSequence(t *testing.T) {
	notConnected := []string{"google_not_connected", "whatsapp_not_connected", "signal_not_connected"}
	property := func(raw []uint8) bool {
		if len(raw) > 3*int(maxTransportAttempts) {
			raw = raw[:3*int(maxTransportAttempts)]
		}
		clock := newManualClock(messagingTestTime)
		store := openMessagingTestStore(t, clock.Now())
		sender := &budgetFuncSender{}
		registry := newScriptedRegistry("budget-property", sender)
		registry.setAvailable(true)
		service := newMessagingTestService(t, store, registry, clock)
		submission := mustSendText(t, service, SendTextCommand{
			CommonCommand: testCommonCommand("budget-property"),
			Body:          "property",
		})
		ctx := context.Background()
		consumed := int64(0)
		moves := 0

		for index, value := range raw {
			kind := budgetStepKind(value % uint8(budgetStepKinds))
			before := mustOutboxItem(t, service, submission.OutboxID)
			if before.State != OutboxQueued && before.State != OutboxNotDispatched {
				// Terminal or uncertain: nothing may be sent again.
				clock.Advance(time.Hour)
				calls := sender.count()
				if processed, err := service.DispatchDue(ctx, 1); err != nil || processed != 0 || sender.count() != calls {
					t.Logf("step %d: settled row %q was dispatched again", index, before.State)
					return false
				}
				continue
			}
			if before.NextAttemptAtMS != nil {
				if wait := *before.NextAttemptAtMS - clock.Now().UnixMilli(); wait > 0 {
					clock.Advance(time.Duration(wait) * time.Millisecond)
				}
			}
			conversation, err := store.GetConversation("conversation-1")
			if err != nil {
				t.Logf("GetConversation(): %v", err)
				return false
			}
			now := clock.Now()
			var failure error
			var result bridge.SendResult
			switch kind {
			case budgetConsumingTransient:
				failure = bridge.OpError{Class: bridge.FailureTransient, Fingerprint: "google_conversation_not_found"}
			case budgetConsumingRateLimited:
				failure = bridge.OpError{Class: bridge.FailureRateLimited}
			case budgetConsumingRetryAt:
				failure = bridge.OpError{Class: bridge.FailureTransient, RetryAt: now.Add(time.Hour)}
			case budgetConsumingMoved:
				moves++
				failure = movedFailure(conversation.RemoteConversationID, fmt.Sprintf("remote-moved-%d", moves))
			case budgetExemptNotConnected:
				failure = bridge.OpError{Class: bridge.FailureTransient, Fingerprint: notConnected[index%len(notConnected)]}
			case budgetExemptCredentials:
				failure = bridge.OpError{Class: bridge.FailureCredentialsExpired}
			case budgetUncertain:
				failure = bridge.OpError{Class: bridge.FailureTransient, Dispatch: bridge.DispatchUncertain}
			case budgetUnknownDispatch:
				failure = bridge.OpError{Class: bridge.FailureTransient}
			case budgetSuccess:
				result = bridge.SendResult{RemoteMessageID: "remote-property-sent"}
			}
			if opErr, ok := failure.(bridge.OpError); ok {
				opErr.Operation = "send_text"
				if opErr.Dispatch == "" && kind != budgetUnknownDispatch {
					opErr.Dispatch = bridge.DispatchNotCalled
				}
				if opErr.Cause == nil {
					opErr.Cause = errors.New("scripted")
				}
				failure = opErr
			}
			sender.next(result, failure)

			calls := sender.count()
			if processed, err := service.DispatchDue(ctx, 1); err != nil || processed != 1 {
				t.Logf("step %d (%d): DispatchDue = %d, %v", index, kind, processed, err)
				return false
			}
			if sender.count() != calls+1 {
				t.Logf("step %d (%d): transport not called", index, kind)
				return false
			}
			if got := sender.lastRequest().Conversation.RemoteID; got != conversation.RemoteConversationID {
				t.Logf("step %d: request remote ID %q, conversation bound to %q", index, got, conversation.RemoteConversationID)
				return false
			}
			after := mustOutboxItem(t, service, submission.OutboxID)
			switch kind {
			case budgetConsumingTransient, budgetConsumingRateLimited, budgetConsumingRetryAt, budgetConsumingMoved:
				consumed++
				if consumed > maxTransportAttempts {
					t.Logf("step %d: %d consuming calls exceed the cap", index, consumed)
					return false
				}
				if after.AttemptCount != before.AttemptCount+1 {
					t.Logf("step %d: consuming failure moved attempts %d -> %d", index, before.AttemptCount, after.AttemptCount)
					return false
				}
				if consumed == maxTransportAttempts {
					if after.State != OutboxRejected || stringValue(after.ErrorClass) != sqlite.RetryExhaustedErrorClass {
						t.Logf("step %d: capped consuming failure left %+v", index, after)
						return false
					}
					continue
				}
				if after.State != OutboxNotDispatched || after.NextAttemptAtMS == nil {
					t.Logf("step %d: consuming failure left %+v", index, after)
					return false
				}
				wantNext := now.Add(retryBackoff(defaultRetryDelay, after.AttemptCount))
				switch kind {
				case budgetConsumingRetryAt:
					wantNext = now.Add(time.Hour)
				case budgetConsumingMoved:
					wantNext = now
				}
				if *after.NextAttemptAtMS != wantNext.UnixMilli() {
					t.Logf("step %d (%d): next attempt %d, want %d", index, kind, *after.NextAttemptAtMS, wantNext.UnixMilli())
					return false
				}
			case budgetExemptNotConnected, budgetExemptCredentials:
				if after.State != OutboxNotDispatched || after.AttemptCount != before.AttemptCount ||
					after.NextAttemptAtMS == nil || *after.NextAttemptAtMS != now.Add(defaultRetryDelay).UnixMilli() {
					t.Logf("step %d: exempt failure left %+v (before %d attempts)", index, after, before.AttemptCount)
					return false
				}
			case budgetUncertain, budgetUnknownDispatch:
				if after.State != OutboxUncertain {
					t.Logf("step %d: ambiguous outcome left %+v", index, after)
					return false
				}
			case budgetSuccess:
				if after.State != OutboxConfirmed {
					t.Logf("step %d: success left %+v", index, after)
					return false
				}
			}
		}
		final := mustOutboxItem(t, service, submission.OutboxID)
		if final.State == OutboxRejected && consumed != maxTransportAttempts {
			t.Logf("rejected after %d consuming calls", consumed)
			return false
		}
		return final.AttemptCount <= maxTransportAttempts
	}
	config := &quick.Config{MaxCount: 80, Rand: rand.New(rand.NewSource(20261007))}
	if err := quick.Check(property, config); err != nil {
		t.Fatalf("retry budget property: %v", err)
	}
	// The sequences that matter most, pinned explicitly.
	pinned := [][]uint8{
		repeatBudgetStep(budgetConsumingTransient, int(maxTransportAttempts)+3),
		repeatBudgetStep(budgetExemptNotConnected, 4*int(maxTransportAttempts)),
		repeatBudgetStep(budgetConsumingMoved, int(maxTransportAttempts)+1),
		append(repeatBudgetStep(budgetConsumingTransient, int(maxTransportAttempts)-1), uint8(budgetUncertain), uint8(budgetSuccess)),
	}
	for _, sequence := range pinned {
		if !property(sequence) {
			t.Fatalf("retry budget property fails for %v", sequence)
		}
	}
}

func repeatBudgetStep(kind budgetStepKind, count int) []uint8 {
	steps := make([]uint8, count)
	for index := range steps {
		steps[index] = uint8(kind)
	}
	return steps
}

func movedFailure(from, to string) bridge.OpError {
	return bridge.OpError{
		Class:       bridge.FailureTransient,
		Operation:   "send_text",
		Fingerprint: bridge.FingerprintConversationMoved,
		Dispatch:    bridge.DispatchNotCalled,
		Cause:       &bridge.ConversationMovedError{FromRemoteID: from, ToRemoteID: to},
	}
}

func stringPointer(value string) *string { return &value }

func budgetIdentity(id string, kind sqlite.IdentityKind, canonical string) sqlite.Identity {
	return sqlite.Identity{
		IdentityID:     id,
		AccountID:      "account-1",
		Kind:           kind,
		CanonicalValue: canonical,
		RawValue:       canonical,
		DisplayName:    id,
		MetadataJSON:   `{}`,
	}
}

func seedBudgetConversation(
	t *testing.T,
	store *sqlite.Store,
	clock Clock,
	conversationID, remoteID string,
	kind sqlite.ConversationKind,
) {
	t.Helper()
	nowMS := clock.Now().UnixMilli()
	if err := store.UpsertConversation(sqlite.Conversation{
		ConversationID:       conversationID,
		AccountID:            "account-1",
		RemoteConversationID: remoteID,
		Kind:                 kind,
		Title:                conversationID,
		NotificationMode:     sqlite.NotificationModeAll,
		MetadataJSON:         `{}`,
		CreatedAtMS:          nowMS,
		UpdatedAtMS:          nowMS,
	}); err != nil {
		t.Fatalf("UpsertConversation(%q): %v", conversationID, err)
	}
}

func seedBudgetParticipants(
	t *testing.T,
	store *sqlite.Store,
	clock Clock,
	conversationID string,
	identities []sqlite.Identity,
) {
	t.Helper()
	nowMS := clock.Now().UnixMilli()
	participants := make([]sqlite.ConversationParticipant, 0, len(identities))
	for _, identity := range identities {
		identity.CreatedAtMS = nowMS
		identity.UpdatedAtMS = nowMS
		if err := store.UpsertIdentity(identity); err != nil {
			t.Fatalf("UpsertIdentity(%q): %v", identity.IdentityID, err)
		}
		participants = append(participants, sqlite.ConversationParticipant{
			AccountID:      "account-1",
			ConversationID: conversationID,
			IdentityID:     identity.IdentityID,
			Role:           sqlite.ParticipantRoleMember,
			IsActive:       true,
		})
	}
	if err := store.ReplaceConversationParticipants(conversationID, participants); err != nil {
		t.Fatalf("ReplaceConversationParticipants(%q): %v", conversationID, err)
	}
}

func seedBudgetTarget(t *testing.T, store *sqlite.Store, clock *manualClock) sqlite.Message {
	t.Helper()
	identityID := "identity-budget-target-author"
	seedDispatchIdentity(t, store, identityID, "author@example.test", clock.Now())
	return mustProjectDispatchMessage(t, store, clock, sqlite.Message{
		MessageID:        "message-budget-target",
		ConversationID:   "conversation-1",
		AccountID:        "account-1",
		RemoteMessageID:  "remote-budget-target",
		SenderIdentityID: &identityID,
		Direction:        sqlite.MessageDirectionIncoming,
		Body:             "target",
		State:            sqlite.MessageStateActive,
		OccurredAtMS:     clock.Now().Add(-time.Minute).UnixMilli(),
	})
}

// budgetCallCounter answers every transport capability with success and
// counts the calls.
type budgetCallCounter struct {
	mu    sync.Mutex
	calls int
}

func (c *budgetCallCounter) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func (c *budgetCallCounter) call() {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
}

func (c *budgetCallCounter) SendText(_ context.Context, request bridge.TextRequest) (bridge.SendResult, error) {
	c.call()
	return bridge.SendResult{RemoteMessageID: "remote-" + request.RequestID}, nil
}

func (c *budgetCallCounter) SendMedia(_ context.Context, request bridge.MediaRequest) (bridge.SendResult, error) {
	c.call()
	return bridge.SendResult{RemoteMessageID: "remote-" + request.RequestID}, nil
}

func (c *budgetCallCounter) SendReaction(context.Context, bridge.ReactionRequest) (bridge.SendResult, error) {
	c.call()
	return bridge.SendResult{}, nil
}

func (c *budgetCallCounter) MarkRead(context.Context, bridge.ReadReceiptRequest) error {
	c.call()
	return nil
}

// budgetFuncSender answers each SendText with the outcome queued by next.
type budgetFuncSender struct {
	mu       sync.Mutex
	result   bridge.SendResult
	err      error
	requests []bridge.TextRequest
}

func (s *budgetFuncSender) next(result bridge.SendResult, err error) {
	s.mu.Lock()
	s.result, s.err = result, err
	s.mu.Unlock()
}

func (s *budgetFuncSender) SendText(_ context.Context, request bridge.TextRequest) (bridge.SendResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, request)
	return s.result, s.err
}

func (s *budgetFuncSender) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.requests)
}

func (s *budgetFuncSender) lastRequest() bridge.TextRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests[len(s.requests)-1]
}

// Rows leased together are dispatched one at a time; a row whose lease the
// rows before it used up is returned to the queue untouched (no transport
// call, no attempt) and dispatched on a fresh lease next, so no transport call
// starts with too little lease left (PR #204 review).
func TestDispatchReleasesRowsWhoseLeaseASlowCallUsedUp(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	sender := &scriptedTextSender{steps: []sendStep{
		{result: bridge.SendResult{RemoteMessageID: "remote-first"}},
		{result: bridge.SendResult{RemoteMessageID: "remote-second"}},
	}}
	registry := newScriptedRegistry("lease-fresh", sender)
	registry.setAvailable(true)
	service := newMessagingTestService(t, store, registry, clock)
	first := mustSendText(t, service, SendTextCommand{CommonCommand: testCommonCommand("lease-first"), Body: "first"})
	second := mustSendText(t, service, SendTextCommand{CommonCommand: testCommonCommand("lease-second"), Body: "second"})
	// The first send is slow: it uses a third of the lease.
	sender.onSend = func() {
		if sender.requestCount() == 1 {
			clock.Advance(defaultLeaseTime / 3)
		}
	}
	ctx := context.Background()

	if processed, err := service.DispatchDue(ctx, 2); err != nil || processed != 1 {
		t.Fatalf("DispatchDue(batch) = %d, %v; want 1 dispatched, nil", processed, err)
	}
	if got := sender.requestCount(); got != 1 {
		t.Fatalf("transport calls = %d, want only the first row's", got)
	}
	if delivery := mustDelivery(t, service, first.OutboxID); delivery.State != OutboxConfirmed {
		t.Fatalf("first delivery = %+v, want confirmed", delivery)
	}
	released := mustOutboxItem(t, service, second.OutboxID)
	if released.State != sqlite.OutboxQueued || released.AttemptCount != 0 || released.LeaseToken != nil {
		t.Fatalf("second row = %+v, want queued again with no attempt spent", released)
	}

	if processed, err := service.DispatchDue(ctx, 2); err != nil || processed != 1 {
		t.Fatalf("DispatchDue(fresh lease) = %d, %v; want 1, nil", processed, err)
	}
	if got := sender.requestCount(); got != 2 {
		t.Fatalf("transport calls = %d, want the second row dispatched on its fresh lease", got)
	}
	if delivery := mustDelivery(t, service, second.OutboxID); delivery.State != OutboxConfirmed {
		t.Fatalf("second delivery = %+v, want confirmed", delivery)
	}
}

func TestLeaseFreshForDispatch(t *testing.T) {
	now := messagingTestTime
	expiring := func(in time.Duration) sqlite.Lease {
		ms := now.Add(in).UnixMilli()
		return sqlite.Lease{OutboxItem: sqlite.OutboxItem{LeaseExpiresAtMS: &ms}}
	}
	for _, test := range []struct {
		remaining time.Duration
		want      bool
	}{
		{defaultLeaseTime, true},
		{27 * time.Second, true},
		{27*time.Second - time.Millisecond, false},
		{time.Second, false},
		{-time.Second, false},
	} {
		if got := leaseFreshForDispatch(expiring(test.remaining), now, defaultLeaseTime); got != test.want {
			t.Fatalf("leaseFreshForDispatch(%v left of %v) = %v, want %v", test.remaining, defaultLeaseTime, got, test.want)
		}
	}
	if !leaseFreshForDispatch(sqlite.Lease{}, now, defaultLeaseTime) {
		t.Fatal("a lease without an expiry was treated as used up")
	}
}
