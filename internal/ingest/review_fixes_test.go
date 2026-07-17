package ingest

// Pins for the adversarial-review fixes: echo reconciliation is best-effort
// (a reconcile fault never blocks projecting the inbound message), echo
// counters mirror the outcomes the real MessageService now surfaces, message
// frames advance conversation recency monotonically without re-upserting the
// row, and a self receipt that outruns its conversation or device drops
// benignly instead of quarantining.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/messaging"
)

func TestWorkerEchoErrorStillProjectsInbound(t *testing.T) {
	const (
		clientRequestID = "client-request-echo-error"
		remoteMessageID = "remote-echo-error"
	)
	recorder := &i01EchoRecorder{
		err: errors.New("observe transport echo: reconcile confirm affected 0 rows, want 1"),
	}
	decoder := i01DecoderFunc(func(
		_ context.Context,
		_ bridge.RawIngressRecord,
	) ([]bridge.Event, error) {
		return i01OutgoingMessageEvents(remoteMessageID, "still delivered", clientRequestID), nil
	})
	harness := i01NewHarness(t, decoder, recorder)
	i01StartWorker(t, harness.worker)

	i01MustAppend(t, harness.sink, i01IngressRecord(
		"echo-error:remote-echo-error",
		[]byte("fake-decoder-echo-error-frame"),
	))
	i01WaitFor(t, "projection despite echo error", func() bool {
		snapshot := harness.counters.Snapshot(i01AccountID)
		return snapshot.Projected == 1 && snapshot.EchoErrors == 1
	})

	snapshot := harness.counters.Snapshot(i01AccountID)
	if snapshot.Quarantined != 0 || snapshot.EchoReconciled != 0 || snapshot.EchoNotFound != 0 {
		t.Fatalf("echo-error counters = %+v", snapshot)
	}
	if calls := recorder.snapshot(); len(calls) != 1 {
		t.Fatalf("ObserveTransportEcho calls = %d, want 1", len(calls))
	}
	message, err := i01GetMessage(harness.messages, remoteMessageID)
	if err != nil {
		t.Fatalf("GetMessageByRemote() after echo error: %v", err)
	}
	if message.Body != "still delivered" {
		t.Fatalf("projected body = %q, want still delivered", message.Body)
	}
	i01AssertNoPending(t, harness.messages)
}

func TestWorkerEchoOutcomeCountersMirrorService(t *testing.T) {
	tests := []struct {
		name    string
		outcome messaging.EchoOutcome
		read    func(CounterSnapshot) uint64
	}{
		{name: "reconciled", outcome: messaging.EchoReconciled, read: func(s CounterSnapshot) uint64 { return s.EchoReconciled }},
		{name: "enriched", outcome: messaging.EchoEnriched, read: func(s CounterSnapshot) uint64 { return s.EchoEnriched }},
		{name: "noop", outcome: messaging.EchoNoop, read: func(s CounterSnapshot) uint64 { return s.EchoNoop }},
		{name: "notfound", outcome: messaging.EchoNotFound, read: func(s CounterSnapshot) uint64 { return s.EchoNotFound }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := &i01EchoRecorder{outcome: test.outcome}
			decoder := i01DecoderFunc(func(
				_ context.Context,
				_ bridge.RawIngressRecord,
			) ([]bridge.Event, error) {
				return i01OutgoingMessageEvents(
					"remote-outcome-"+test.name,
					"outcome body",
					"client-request-"+test.name,
				), nil
			})
			harness := i01NewHarness(t, decoder, recorder)
			i01StartWorker(t, harness.worker)

			i01MustAppend(t, harness.sink, i01IngressRecord(
				"echo-outcome:"+test.name,
				[]byte("fake-decoder-outcome-"+test.name),
			))
			i01WaitFor(t, "projection with outcome "+test.name, func() bool {
				return harness.counters.Snapshot(i01AccountID).Projected == 1
			})

			snapshot := harness.counters.Snapshot(i01AccountID)
			if got := test.read(snapshot); got != 1 {
				t.Fatalf("%s counter = %d, want 1; snapshot=%+v", test.name, got, snapshot)
			}
			total := snapshot.EchoReconciled + snapshot.EchoEnriched +
				snapshot.EchoNoop + snapshot.EchoNotFound + snapshot.EchoErrors
			if total != 1 {
				t.Fatalf("echo counters sum = %d, want exactly 1; snapshot=%+v", total, snapshot)
			}
		})
	}
}

func TestWorkerMessageFrameAdvancesRecencyMonotonically(t *testing.T) {
	const (
		remoteConversationID   = "google-recency-thread"
		existingConversationID = "recency-conversation-pk"
		existingTitle          = "Recency title survives"
	)
	newerAtMS := workerPathNowMS - 500
	olderAtMS := workerPathNowMS - 8_000
	harness := newWorkerPathHarness(t, bridge.PlatformGoogle, map[string][]bridge.Event{
		"newer": {{
			Kind: bridge.EventMessage,
			Message: &bridge.MessageEvent{
				RemoteConversationID: remoteConversationID,
				RemoteMessageID:      "recency-message-newer",
				Sender:               bridge.IdentityRef{IsSelf: true},
				Direction:            "outgoing",
				Body:                 "newer message",
				OccurredAt:           time.UnixMilli(newerAtMS),
			},
		}},
		"older": {{
			Kind: bridge.EventMessage,
			Message: &bridge.MessageEvent{
				RemoteConversationID: remoteConversationID,
				RemoteMessageID:      "recency-message-older",
				Sender:               bridge.IdentityRef{IsSelf: true},
				Direction:            "outgoing",
				Body:                 "older message",
				OccurredAt:           time.UnixMilli(olderAtMS),
			},
		}},
	})
	seeded := workerPathSeedConversation(
		t,
		harness.store,
		existingConversationID,
		remoteConversationID,
		existingTitle,
	)

	harness.process(t, "newer")
	afterNewer, err := harness.store.GetConversationByRemote(workerPathAccountID, remoteConversationID)
	if err != nil {
		t.Fatalf("GetConversationByRemote() after newer: %v", err)
	}
	if afterNewer.LastMessageAtMS != newerAtMS {
		t.Fatalf("last_message_at_ms after newer = %d, want %d", afterNewer.LastMessageAtMS, newerAtMS)
	}

	harness.process(t, "older")
	afterOlder, err := harness.store.GetConversationByRemote(workerPathAccountID, remoteConversationID)
	if err != nil {
		t.Fatalf("GetConversationByRemote() after older: %v", err)
	}
	if afterOlder.LastMessageAtMS != newerAtMS {
		t.Fatalf("last_message_at_ms after older = %d, want it to stay %d", afterOlder.LastMessageAtMS, newerAtMS)
	}
	if afterOlder.ConversationID != seeded.ConversationID ||
		afterOlder.Title != seeded.Title ||
		afterOlder.Kind != seeded.Kind ||
		afterOlder.CreatedAtMS != seeded.CreatedAtMS {
		t.Fatalf("recency bump disturbed the conversation row: %+v, want fields of %+v", afterOlder, seeded)
	}
	workerPathAssertAllProcessed(t, harness.messages)
}

func TestWorkerSelfReceiptBeforeConversationIsBenign(t *testing.T) {
	harness := newWorkerPathHarness(t, bridge.PlatformGoogle, map[string][]bridge.Event{
		"orphan-receipt": {workerPathReceiptEvent(
			"google-unknown-thread",
			"unknown-message",
			workerPathNowMS-100,
		)},
	})

	harness.process(t, "orphan-receipt")

	snapshot := harness.worker.Counters().Snapshot(workerPathAccountID)
	if snapshot.ReceiptsDropped != 1 || snapshot.ReceiptsSelf != 0 || snapshot.Quarantined != 0 {
		t.Fatalf("orphan self-receipt counters = %+v, want dropped=1 self=0 quarantined=0", snapshot)
	}
	workerPathAssertAllProcessed(t, harness.messages)
}

// Delivered-class self receipts acknowledge outgoing delivery; they say
// nothing about what the local user has read, so they must not advance the
// read cursor.
func TestWorkerDeliveredSelfReceiptDoesNotAdvanceReadCursor(t *testing.T) {
	harness := newWorkerPathHarness(t, bridge.PlatformWhatsApp, map[string][]bridge.Event{
		"delivered-self": {{
			Kind: bridge.EventReceipt,
			Receipt: &bridge.ReceiptEvent{
				RemoteConversationID: "whatsapp:15551234567@s.whatsapp.net",
				RemoteMessageIDs:     []string{"delivered-ack"},
				Actor:                bridge.IdentityRef{IsSelf: true},
				Status:               "delivered",
				OccurredAt:           time.UnixMilli(workerPathNowMS - 50),
			},
		}},
	})

	harness.process(t, "delivered-self")

	snapshot := harness.worker.Counters().Snapshot(workerPathAccountID)
	if snapshot.ReceiptsDropped != 1 || snapshot.ReceiptsSelf != 0 || snapshot.Quarantined != 0 {
		t.Fatalf("delivered self-receipt counters = %+v, want dropped=1 self=0", snapshot)
	}
	workerPathAssertAllProcessed(t, harness.messages)
}

func TestSinkRecordIngressErrorCountsPerAccount(t *testing.T) {
	counters := &Counters{}
	sink := &Sink{counters: counters}
	sink.RecordIngressError("account-err")
	sink.RecordIngressError("account-err")
	if got := counters.Snapshot("account-err").AppendErrors; got != 2 {
		t.Fatalf("AppendErrors = %d, want 2", got)
	}
	if got := counters.Snapshot("account-other").AppendErrors; got != 0 {
		t.Fatalf("other-account AppendErrors = %d, want 0", got)
	}
}
