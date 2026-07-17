package messaging

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// A canceled context must not turn a raced lease transaction into a hard
// dispatch fault. database/sql auto-rolls-back a transaction whose context is
// canceled, so LeaseDue's commit fails; because no lease was handed out, the
// dispatcher must report the cancellation itself and Run must treat it as a
// clean stop. Regression for the shutdown flake in DispatchDue's lease path.
func TestDispatchDueCanceledContextIsCleanStopNotLeaseFault(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	service := newMessagingTestService(
		t,
		store,
		newScriptedRegistry("lease-cancel", &scriptedTextSender{}),
		clock,
	)

	// A due, queued intent exists, so LeaseDue would otherwise open its
	// transaction and claim it.
	mustSendText(t, service, SendTextCommand{
		CommonCommand: testCommonCommand("lease-cancel-key"),
		Body:          "durably queued before shutdown",
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	processed, err := service.DispatchDue(ctx, service.batchLimit)
	if processed != 0 {
		t.Fatalf("DispatchDue processed = %d, want 0 under a canceled context", processed)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("DispatchDue error = %v, want context.Canceled", err)
	}
	// The cancellation must surface as itself, not be reframed as a lease-path
	// storage fault. This is what the guard changes: without it, the error is
	// "lease due messages: ...: context canceled" (a raced commit reads as a
	// dispatch failure); with it, the bare cancellation.
	if strings.Contains(err.Error(), "lease due") {
		t.Fatalf("DispatchDue error = %q, want the bare cancellation, not a lease-path fault", err)
	}

	// Run wraps the same path; it too must exit cleanly, and Run's own wrapper
	// ("run message service:") is the only framing permitted.
	runErr := service.Run(ctx)
	if !errors.Is(runErr, context.Canceled) {
		t.Fatalf("Run error = %v, want context.Canceled", runErr)
	}
	if strings.Contains(runErr.Error(), "lease due") {
		t.Fatalf("Run error = %q, want a clean stop, not a lease-path fault", runErr)
	}
}
