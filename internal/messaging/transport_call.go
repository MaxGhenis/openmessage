package messaging

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

// Bounds on one transport call. The dispatch loop runs every call
// synchronously on one goroutine, so a call that never returns stops every
// later intent, on every account, until the app restarts. On 2026-10-20 a
// Google text send got no answer at 15:55 and, with libgm then waiting for the
// phone forever, froze all sending, reactions and read receipts for three and
// a half hours. A call past its bound is abandoned and its intent becomes
// uncertain: the request may have reached the platform, so nothing sends it
// again automatically.
//
// libgm now gives up on an unanswered request after client.RequestTimeout,
// inside this bound, so the Google adapter classifies most such failures
// itself (a conversation lookup that sent nothing stays retryable). This
// bound remains the backstop for what that can't cover: a stalled HTTP send,
// a Google send whose two requests together run past it, and every other
// platform.
const (
	defaultTransportCallTimeout = 60 * time.Second
	// A media call uploads the attachment first. libgm bounds each of its two
	// upload requests at 2 minutes and WhatsApp its upload at 90 s, so
	// 5 minutes leaves room for the send itself.
	defaultMediaTransportCallTimeout = 5 * time.Minute
	// defaultTransportShutdownGrace is how long a canceled call may take to
	// return its own result before shutdown abandons it.
	defaultTransportShutdownGrace = 2 * time.Second
)

const (
	transportTimeoutOperation    = "transport_timeout"
	transportTimeoutFingerprint  = "transport_call_timeout"
	transportShutdownFingerprint = "transport_call_abandoned_at_shutdown"

	abandonReasonTimeout  = "timeout"
	abandonReasonShutdown = "shutdown"
)

// DispatchHealth reports the dispatch loop's transport calls, so a slow or
// abandoned call is visible in /api/status instead of looking like an idle
// outbox.
type DispatchHealth struct {
	// InFlight is the call the dispatch loop is waiting on, if any.
	InFlight *InFlightTransportCall `json:"in_flight,omitempty"`
	// AbandonedCalls counts calls given up on since the service started.
	AbandonedCalls int64 `json:"abandoned_calls"`
	// AbandonedStillRunning counts abandoned calls that have not returned.
	AbandonedStillRunning int64 `json:"abandoned_still_running"`
	// LastAbandoned is the most recent abandoned call, if any.
	LastAbandoned *AbandonedTransportCall `json:"last_abandoned,omitempty"`
}

// InFlightTransportCall describes the transport call in progress.
type InFlightTransportCall struct {
	OutboxID    string `json:"outbox_id"`
	AccountID   string `json:"account_id"`
	Operation   string `json:"operation"`
	StartedAtMS int64  `json:"started_at_ms"`
	ElapsedMS   int64  `json:"elapsed_ms"`
	TimeoutMS   int64  `json:"timeout_ms"`
}

// AbandonedTransportCall describes a call the dispatch loop stopped waiting
// for.
type AbandonedTransportCall struct {
	OutboxID      string `json:"outbox_id"`
	AccountID     string `json:"account_id"`
	Operation     string `json:"operation"`
	StartedAtMS   int64  `json:"started_at_ms"`
	AbandonedAtMS int64  `json:"abandoned_at_ms"`
	// Reason is "timeout" or "shutdown".
	Reason string `json:"reason"`
}

// transportCalls is the bookkeeping behind DispatchHealth. One lock orders a
// call's return against its abandonment, so a result that races the deadline
// is either delivered or counted as still running, never both.
type transportCalls struct {
	mu            sync.Mutex
	inFlight      *transportCallRecord
	abandoned     int64
	stillRunning  int64
	lastAbandoned *AbandonedTransportCall
}

type transportCallRecord struct {
	call      InFlightTransportCall
	returned  bool
	abandoned bool
}

func (t *transportCalls) begin(call InFlightTransportCall) *transportCallRecord {
	record := &transportCallRecord{call: call}
	t.mu.Lock()
	t.inFlight = record
	t.mu.Unlock()
	return record
}

// finish runs on the call's goroutine when the transport returns. deliver
// hands the result to a waiter that has not abandoned the call.
func (t *transportCalls) finish(record *transportCallRecord, deliver func()) {
	t.mu.Lock()
	defer t.mu.Unlock()
	record.returned = true
	if record.abandoned {
		t.stillRunning--
		return
	}
	deliver()
}

// settle clears a call whose result the waiter received.
func (t *transportCalls) settle(record *transportCallRecord) {
	t.mu.Lock()
	if t.inFlight == record {
		t.inFlight = nil
	}
	t.mu.Unlock()
}

// abandon gives up on a call unless it already returned, in which case its
// result is waiting and abandon reports false.
func (t *transportCalls) abandon(record *transportCallRecord, reason string, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if record.returned {
		return false
	}
	record.abandoned = true
	t.abandoned++
	t.stillRunning++
	t.lastAbandoned = &AbandonedTransportCall{
		OutboxID:      record.call.OutboxID,
		AccountID:     record.call.AccountID,
		Operation:     record.call.Operation,
		StartedAtMS:   record.call.StartedAtMS,
		AbandonedAtMS: now.UnixMilli(),
		Reason:        reason,
	}
	if t.inFlight == record {
		t.inFlight = nil
	}
	return true
}

func (t *transportCalls) health(now time.Time) DispatchHealth {
	t.mu.Lock()
	defer t.mu.Unlock()
	health := DispatchHealth{
		AbandonedCalls:        t.abandoned,
		AbandonedStillRunning: t.stillRunning,
	}
	if t.inFlight != nil {
		call := t.inFlight.call
		call.ElapsedMS = max(now.UnixMilli()-call.StartedAtMS, 0)
		health.InFlight = &call
	}
	if t.lastAbandoned != nil {
		last := *t.lastAbandoned
		health.LastAbandoned = &last
	}
	return health
}

// DispatchHealth snapshots the dispatch loop's transport calls.
func (s *MessageService) DispatchHealth() DispatchHealth {
	return s.transport.health(s.clock.Now())
}

type transportOutcome[T any] struct {
	value T
	err   error
}

// callTransport runs one transport call with a hard bound. The call gets a
// context canceled when the bound passes, but a transport that ignores its
// context (libgm) keeps its goroutine until it returns; the dispatch loop does
// not wait for it. An abandoned call returns an uncertain OpError, because the
// request may already have reached the platform. That holds even when the hang
// came before the request left: Google's SendText first asks the phone for the
// conversation, and a phone that never answers hangs there. Retrying such an
// intent would let it hold the loop for another full bound each time.
//
// call runs on its own goroutine and must release anything it uses (the
// dispatch lease, a media reader) itself, since an abandoned call can outlive
// the dispatch path that started it.
func callTransport[T any](
	s *MessageService,
	ctx context.Context,
	item sqlite.OutboxItem,
	operation string,
	timeout time.Duration,
	call func(context.Context) (T, error),
) (T, error) {
	if timeout <= 0 {
		timeout = defaultTransportCallTimeout
	}
	grace := s.transportShutdownGrace
	if grace <= 0 {
		grace = defaultTransportShutdownGrace
	}
	callCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	started := s.clock.Now()
	record := s.transport.begin(InFlightTransportCall{
		OutboxID:    item.OutboxID,
		AccountID:   item.AccountID,
		Operation:   operation,
		StartedAtMS: started.UnixMilli(),
		TimeoutMS:   timeout.Milliseconds(),
	})
	done := make(chan transportOutcome[T], 1)
	go func() {
		value, err := call(callCtx)
		s.transport.finish(record, func() {
			done <- transportOutcome[T]{value: value, err: err}
		})
	}()

	deadline := s.clock.NewTimer(timeout)
	defer deadline.Stop()
	reason := abandonReasonTimeout
	select {
	case outcome := <-done:
		s.transport.settle(record)
		return outcome.value, outcome.err
	case <-deadline.C():
	case <-ctx.Done():
		// A context-aware transport returns its own, better classified result
		// once canceled; give it a moment before abandoning it.
		graceTimer := s.clock.NewTimer(grace)
		select {
		case outcome := <-done:
			graceTimer.Stop()
			s.transport.settle(record)
			return outcome.value, outcome.err
		case <-graceTimer.C():
			reason = abandonReasonShutdown
		case <-deadline.C():
		}
		graceTimer.Stop()
	}

	if !s.transport.abandon(record, reason, s.clock.Now()) {
		outcome := <-done
		s.transport.settle(record)
		return outcome.value, outcome.err
	}
	var zero T
	if reason == abandonReasonShutdown {
		return zero, bridge.OpError{
			Class:       bridge.FailureTransient,
			Operation:   transportTimeoutOperation,
			Fingerprint: transportShutdownFingerprint,
			Dispatch:    bridge.DispatchUncertain,
			Cause:       fmt.Errorf("%s: abandoned at shutdown: %w", operation, ctx.Err()),
		}
	}
	return zero, bridge.OpError{
		Class:       bridge.FailureTransient,
		Operation:   transportTimeoutOperation,
		Fingerprint: transportTimeoutFingerprint,
		Dispatch:    bridge.DispatchUncertain,
		Cause: fmt.Errorf(
			"%s: the platform did not answer within %s; the message may still arrive",
			operation,
			timeout,
		),
	}
}
