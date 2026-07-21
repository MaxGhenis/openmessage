package ingest

import (
	"context"
	"testing"
	"time"

	"github.com/maxghenis/openmessage/internal/bridge"
)

func TestWorkerChangesBroadcastAfterProjectedDrain(t *testing.T) {
	decoder := i01DecoderFunc(func(
		_ context.Context,
		_ bridge.RawIngressRecord,
	) ([]bridge.Event, error) {
		return i01OutgoingMessageEvents("changes-projected", "projected body", ""), nil
	})
	harness := i01NewHarness(t, decoder, nil)
	changes := harness.worker.Changes()

	i01MustAppend(t, harness.sink, i01IngressRecord(
		"changes:projected",
		[]byte("projected"),
	))
	harness.worker.drain(context.Background())

	message, err := i01GetMessage(harness.messages, "changes-projected")
	if err != nil {
		t.Fatalf("GetMessageByRemote() after drain: %v", err)
	}
	if message.Body != "projected body" {
		t.Fatalf("projected message body = %q, want projected body", message.Body)
	}
	assertWorkerChangeClosed(t, changes, "projected drain")

	next := harness.worker.Changes()
	if next == changes {
		t.Fatal("Changes() did not replace the closed broadcast channel")
	}
	assertWorkerChangeOpen(t, next, "replacement channel")
}

func TestWorkerChangesDoesNotBroadcastForQuarantinedOrNoopDrain(t *testing.T) {
	tests := []struct {
		name   string
		record bridge.RawIngressRecord
	}{
		{
			name: "quarantined",
			record: func() bridge.RawIngressRecord {
				record := i01IngressRecord(
					"changes:quarantined",
					[]byte("quarantined"),
				)
				record.Codec = "changes.unknown"
				return record
			}(),
		},
		{
			name: "no events",
			record: i01IngressRecord(
				"changes:no-events",
				[]byte("no-events"),
			),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decoder := i01DecoderFunc(func(
				_ context.Context,
				_ bridge.RawIngressRecord,
			) ([]bridge.Event, error) {
				return nil, nil
			})
			harness := i01NewHarness(t, decoder, nil)
			changes := harness.worker.Changes()

			i01MustAppend(t, harness.sink, test.record)
			harness.worker.drain(context.Background())

			i01AssertNoPending(t, harness.messages)
			assertWorkerChangeOpen(t, changes, test.name+" drain")
		})
	}
}

func assertWorkerChangeClosed(t *testing.T, changes <-chan struct{}, description string) {
	t.Helper()
	// The worker broadcasts after the drain pass that advanced the counters a
	// caller waited on, so closure is guaranteed but not yet observable; block
	// rather than snapshot.
	select {
	case <-changes:
	case <-time.After(10 * time.Second):
		t.Fatalf("Changes() channel remained open after %s", description)
	}
}

func assertWorkerChangeOpen(t *testing.T, changes <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-changes:
		t.Fatalf("Changes() channel closed after %s", description)
	default:
	}
}
