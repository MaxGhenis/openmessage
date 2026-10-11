package whatsapplive

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
)

type connectContextTestKey struct{}

// TestConnectPairedDetachesTheHandlerContextFromTeardown pins the context a
// lifecycle connection hands whatsmeow: whatsmeow runs the connection's node
// handlers with it, so ending the generation (cancelling the caller's
// context) after the connection is up must not cancel it. It keeps the
// caller's values.
func TestConnectPairedDetachesTheHandlerContextFromTeardown(t *testing.T) {
	bridge := newPairedBridgeForUnpairTest(t)
	original := connectClientContext
	t.Cleanup(func() { connectClientContext = original })
	var handlerCtx context.Context
	connectClientContext = func(ctx context.Context, _ *whatsmeow.Client) error {
		handlerCtx = ctx
		return nil
	}

	runCtx, cancelRun := context.WithCancel(context.WithValue(context.Background(), connectContextTestKey{}, "run value"))
	if err := bridge.ConnectPaired(runCtx); err != nil {
		t.Fatalf("ConnectPaired(): %v", err)
	}
	cancelRun()

	select {
	case <-handlerCtx.Done():
		t.Fatalf("the handler context ended with the generation: %v", handlerCtx.Err())
	case <-time.After(50 * time.Millisecond):
	}
	if got := handlerCtx.Value(connectContextTestKey{}); got != "run value" {
		t.Fatalf("handler context value = %v, want the caller's", got)
	}
}

// TestConnectPairedStillCancelsTheDial pins the other half: while whatsmeow is
// dialing and handshaking, ending the caller's context still aborts the
// connect.
func TestConnectPairedStillCancelsTheDial(t *testing.T) {
	bridge := newPairedBridgeForUnpairTest(t)
	original := connectClientContext
	t.Cleanup(func() { connectClientContext = original })
	dialing := make(chan context.Context, 1)
	connectClientContext = func(ctx context.Context, _ *whatsmeow.Client) error {
		dialing <- ctx
		<-ctx.Done()
		return ctx.Err()
	}

	runCtx, cancelRun := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- bridge.ConnectPaired(runCtx) }()
	var handlerCtx context.Context
	select {
	case handlerCtx = <-dialing:
	case <-time.After(5 * time.Second):
		t.Fatal("ConnectPaired never dialed")
	}
	cancelRun()

	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("ConnectPaired() = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ending the caller's context did not abort the dial")
	}
	if handlerCtx.Err() == nil {
		t.Fatal("a connect aborted mid-dial left its handler context live")
	}
}

// TestConnectPairedReleasesTheHandlerContextOfAFailedDial: a failed dial keeps
// no handlers, so its context is released at once.
func TestConnectPairedReleasesTheHandlerContextOfAFailedDial(t *testing.T) {
	bridge := newPairedBridgeForUnpairTest(t)
	original := connectClientContext
	t.Cleanup(func() { connectClientContext = original })
	dialErr := errors.New("dial failed")
	var handlerCtx context.Context
	connectClientContext = func(ctx context.Context, _ *whatsmeow.Client) error {
		handlerCtx = ctx
		return dialErr
	}

	if err := bridge.ConnectPaired(context.Background()); !errors.Is(err, dialErr) {
		t.Fatalf("ConnectPaired() = %v, want the dial error", err)
	}
	if handlerCtx.Err() == nil {
		t.Fatal("a failed dial left its handler context live")
	}
}

// TestConnectPairedReportsTheCallersReasonForAnAbortedDial: the dial only sees
// the handler context's cancellation, so the caller's own reason (a deadline)
// is wrapped into the error.
func TestConnectPairedReportsTheCallersReasonForAnAbortedDial(t *testing.T) {
	bridge := newPairedBridgeForUnpairTest(t)
	original := connectClientContext
	t.Cleanup(func() { connectClientContext = original })
	connectClientContext = func(ctx context.Context, _ *whatsmeow.Client) error {
		<-ctx.Done()
		return ctx.Err()
	}

	runCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := bridge.ConnectPaired(runCtx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ConnectPaired() = %v, want it to wrap context.DeadlineExceeded", err)
	}
}
