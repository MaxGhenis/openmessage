package whatsapp

import (
	"context"
	"testing"

	waevents "go.mau.fi/whatsmeow/types/events"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/whatsapplive"
)

// One half of the 2026-08-20 mechanism. whatsmeow deletes a logged-out session
// with the event context of the connection, which is the context Start hands
// to connect, and it dispatches LoggedOut before it starts that delete. The
// adapter cancels the context as soon as LoggedOut reaches it, so whatsmeow's
// delete can be interrupted. whatsapplive therefore removes the revoked
// identity itself (whatsapplive's TestWhatsmeowLogoutDeleteNeedsALiveEventContext
// pins the other half).
func TestLoggedOutCancelsTheContextGivenToConnect(t *testing.T) {
	clock := newManualClock(whatsappAdapterTestEpoch)
	client := newFakeLifecycleClient()
	a := newTestAdapter(t, client, clock.Now)
	connected := make(chan context.Context, 1)
	a.connect = func(ctx context.Context) error {
		connected <- ctx
		return nil
	}
	supervisor := newTestSupervisor(t, a, clock)
	startSupervisor(t, supervisor)
	connectCtx := receiveValue(t, connected, "the connect context")
	if err := connectCtx.Err(); err != nil {
		t.Fatalf("connect context ended before any event: %v", err)
	}

	client.emit(a, whatsapplive.LifecycleEvent{
		Raw:    &waevents.LoggedOut{Reason: waevents.ConnectFailureLoggedOut},
		Paired: true,
		At:     clock.Now(),
	})

	awaitCondition(t, "LoggedOut to cancel the connect context", func() bool {
		return connectCtx.Err() != nil
	})
	awaitSnapshot(t, supervisor, "the reauthentication block", func(snapshot bridge.Snapshot) bool {
		return snapshot.State == bridge.StateBlocked &&
			snapshot.ErrorClass == bridge.FailureReauthRequired &&
			snapshot.ErrorFingerprint == "whatsapp_logged_out"
	})
}
