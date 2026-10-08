package ingest

import (
	"context"
	"testing"
	"time"
)

// A contentless re-delivery of a stored message with no MessageStatus is an
// empty stub (legacy records the status as "unknown"). The decoder must drop
// it: projecting it would overwrite the stored row's body with "".
func TestGoogleNilStatusContentlessRedeliveryKeepsStoredBody(t *testing.T) {
	harness := newGoogleEchoHarness(t, nil)
	const remoteID = "google-nil-status-redelivery"
	occurredAt := googleEchoNow.Add(time.Minute)
	harness.appendMessage(t, googleEchoMessage(remoteID, "", "hello", false, occurredAt))
	harness.waitFor(t, "first projection", func(snapshot CounterSnapshot) bool {
		return snapshot.Projected == 1
	})

	redelivery := googleEchoMessage(remoteID, "", "", false, occurredAt)
	redelivery.MessageStatus = nil
	redelivery.MessageInfo = nil
	harness.appendMessage(t, redelivery)
	harness.waitFor(t, "contentless redelivery handled", func(snapshot CounterSnapshot) bool {
		return snapshot.EmptyStubsSkipped == 1 || snapshot.Projected == 2
	})

	stored, err := harness.messages.GetMessageByRemote(
		context.Background(),
		googleEchoAccountID,
		googleEchoConversationID,
		remoteID,
	)
	if err != nil {
		t.Fatalf("GetMessageByRemote(): %v", err)
	}
	if stored.Body != "hello" {
		t.Fatalf("stored body = %q, want %q", stored.Body, "hello")
	}
	snapshot := harness.counters.Snapshot(googleEchoAccountID)
	if snapshot.EmptyStubsSkipped != 1 || snapshot.Projected != 1 {
		t.Fatalf("counters = %+v, want the redelivery skipped as an empty stub", snapshot)
	}
}

// A message with no MessageStatus that carries content is not a stub and is
// projected, the same as legacy stores it with status "unknown".
func TestGoogleNilStatusMessageWithContentIsProjected(t *testing.T) {
	harness := newGoogleEchoHarness(t, nil)
	const remoteID = "google-nil-status-content"
	message := googleEchoMessage(remoteID, "", "still here", false, googleEchoNow.Add(time.Minute))
	message.MessageStatus = nil
	harness.appendMessage(t, message)
	harness.waitFor(t, "projection", func(snapshot CounterSnapshot) bool {
		return snapshot.Projected == 1
	})
	stored, err := harness.messages.GetMessageByRemote(
		context.Background(),
		googleEchoAccountID,
		googleEchoConversationID,
		remoteID,
	)
	if err != nil {
		t.Fatalf("GetMessageByRemote(): %v", err)
	}
	if stored.Body != "still here" {
		t.Fatalf("stored body = %q, want %q", stored.Body, "still here")
	}
}
