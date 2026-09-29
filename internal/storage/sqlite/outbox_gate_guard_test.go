package sqlite

// Cross-area pin between the send-window gate (MarkTransportCalled cancels a
// leased, uncalled row whose window closed) and the enqueue-time
// near-duplicate check (candidates exclude rejected and canceled rows):
//
//   - a text canceled at the gate was provably never sent, so it must stop
//     blocking a guarded resend of the same body;
//   - a text that crossed the boundary (transport_called_at_ms committed) may
//     have been sent, so it must keep blocking a guarded near-duplicate even
//     after its window closes, because nothing expires a called row.

import (
	"context"
	"errors"
	"testing"
	"time"
)

func enqueueWindowedText(
	t *testing.T,
	repository *OutboxRepository,
	id string,
	body string,
	window time.Duration,
) OutboxItem {
	t.Helper()
	item := outboxTestItem(id)
	item.ExpiresAtMS = outboxTestTimeMS + window.Milliseconds()
	return mustEnqueueOutgoingOutbox(t, repository, item, body)
}

func TestGateExpiredTextStopsBlockingGuardedResend(t *testing.T) {
	clock := newOutboxTestClock(outboxTestTimeMS)
	_, repository := openNearDuplicateTestRepository(t, clock)
	ctx := context.Background()

	const body = "lunch tomorrow at noon at Sfoglina?"
	prior := enqueueWindowedText(t, repository, "gate-expired-prior", body, 10*time.Second)
	lease := leaseOneForGate(t, repository, outboxTestTimeMS, time.Minute)
	if lease.OutboxID != prior.OutboxID {
		t.Fatalf("leased %q, want %q", lease.OutboxID, prior.OutboxID)
	}

	// The window closes while the row is leased but before its transport
	// call: the gate cancels it as expired, so it was never sent.
	clock.Set(outboxTestTimeMS + (11 * time.Second).Milliseconds())
	if err := repository.MarkTransportCalled(ctx, gateAttempt(lease)); !errors.Is(err, ErrSendWindowExpired) {
		t.Fatalf("MarkTransportCalled(expired) = %v, want ErrSendWindowExpired", err)
	}
	assertCanceledAtGate(t, mustFindOutbox(t, repository, prior.OutboxID), prior.AttemptCount)

	// A guarded resend of the identical body is a new intent the guard must
	// accept: the only candidate was proven unsent.
	recorder := &nearDuplicateRecorder{matches: map[string]bool{body: true}}
	resend := outboxTestItem("gate-expired-resend")
	row, disposition, err := enqueueGuardedText(t, repository, resend, body, recorder.check(1, 8))
	if err != nil || disposition != EnqueueInserted || row.OutboxID != resend.OutboxID {
		t.Fatalf("guarded resend after gate expiry = (%+v, %q, %v), want inserted", row, disposition, err)
	}
	if len(recorder.seen) != 0 {
		t.Fatalf("guard compared against %q, want no candidates (the gate-canceled row is not one)", recorder.seen)
	}
}

func TestCalledTextKeepsBlockingGuardedResendAfterItsWindow(t *testing.T) {
	clock := newOutboxTestClock(outboxTestTimeMS)
	_, repository := openNearDuplicateTestRepository(t, clock)
	ctx := context.Background()

	const body = "dinner at seven at the usual place"
	prior := enqueueWindowedText(t, repository, "called-prior", body, 10*time.Second)
	lease := leaseOneForGate(t, repository, outboxTestTimeMS, time.Minute)

	// The transport call starts inside the window.
	clock.Set(outboxTestTimeMS + (1 * time.Second).Milliseconds())
	if err := repository.MarkTransportCalled(ctx, gateAttempt(lease)); err != nil {
		t.Fatalf("MarkTransportCalled(open window) = %v, want nil", err)
	}

	// Past the window: neither the sweep nor the gate may expire a called
	// row, so it still may have reached the recipient.
	clock.Set(outboxTestTimeMS + (11 * time.Second).Milliseconds())
	canceled, err := repository.CancelExpired(ctx, clock.Now())
	if err != nil {
		t.Fatalf("CancelExpired(): %v", err)
	}
	if len(canceled) != 0 {
		t.Fatalf("CancelExpired() canceled %v, want nothing (the only row was handed to the transport)", canceled)
	}
	called := mustFindOutbox(t, repository, prior.OutboxID)
	if called.State != OutboxDispatching || called.TransportCalledAtMS == nil {
		t.Fatalf("called row = state %q transport_called_at_ms %v, want dispatching with the marker", called.State, called.TransportCalledAtMS)
	}

	recorder := &nearDuplicateRecorder{matches: map[string]bool{body: true}}
	resend := outboxTestItem("called-resend")
	_, _, err = enqueueGuardedText(t, repository, resend, body, recorder.check(1, 8))
	var nearDuplicate *NearDuplicateError
	if !errors.As(err, &nearDuplicate) {
		t.Fatalf("guarded resend after a called send = %v, want *NearDuplicateError", err)
	}
	if nearDuplicate.Prior.OutboxID != prior.OutboxID || nearDuplicate.Prior.State != OutboxDispatching {
		t.Fatalf("prior = %+v, want %q in state %q", nearDuplicate.Prior, prior.OutboxID, OutboxDispatching)
	}
	if _, err := repository.FindByID(ctx, resend.OutboxID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("FindByID(refused resend) error = %v, want ErrNotFound", err)
	}
}
