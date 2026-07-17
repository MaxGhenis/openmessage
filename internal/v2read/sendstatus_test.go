package v2read

import (
	"context"
	"testing"
	"time"

	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

// An outgoing v2 message must carry its durable send state on the read DTO, so
// the composer can render it honestly after a refetch. queued/uncertain read as
// "sending" (never "failed"), confirmed as "sent", rejected as "failed", and a
// message with no outbox row carries no status.
func TestV2ReadOutgoingMessageStatusReflectsOutbox(t *testing.T) {
	store, messages, source := openSourceTestStore(t)
	seedSourceAccount(t, store, "acct", "google_messages")
	seedSourceConversation(t, store, sqlite.Conversation{
		ConversationID:       "conv",
		AccountID:            "acct",
		RemoteConversationID: "remote-conv",
		Kind:                 sqlite.ConversationKindDirect,
		Title:                "Thread",
		NotificationMode:     sqlite.NotificationModeAll,
	})
	outbox, err := sqlite.NewOutboxRepository(store, func() time.Time { return time.UnixMilli(sourceTestTimeMS) })
	if err != nil {
		t.Fatalf("NewOutboxRepository(): %v", err)
	}
	ctx := context.Background()

	// enqueue creates the optimistic outgoing message + a queued outbox row.
	enqueue := func(t *testing.T, id string) sqlite.OutboxItem {
		t.Helper()
		item, disp, err := outbox.EnqueueOutgoingMessage(ctx, sqlite.NewOutboxItem{
			OutboxID:           "outbox-" + id,
			AccountID:          "acct",
			ConversationID:     "conv",
			Kind:               sqlite.OutboxKindText,
			IdempotencyKey:     "key-" + id,
			PayloadHash:        "hash-" + id,
			Operation:          "send_text",
			LocalMessageID:     id,
			TransportRequestID: "req-" + id,
		}, sqlite.Message{
			MessageID:       id,
			ConversationID:  "conv",
			AccountID:       "acct",
			RemoteMessageID: "req-" + id,
			Direction:       sqlite.MessageDirectionOutgoing,
			Body:            "body-" + id,
			State:           sqlite.MessageStateActive,
			OccurredAtMS:    sourceTestTimeMS,
		})
		if err != nil || disp != sqlite.EnqueueInserted {
			t.Fatalf("enqueue %q: disp=%q err=%v", id, disp, err)
		}
		return item
	}
	lease := func(t *testing.T, id string) sqlite.Lease {
		t.Helper()
		leases, err := outbox.LeaseDue(ctx, sqlite.LeaseRequest{
			Owner:    "test",
			Now:      time.UnixMilli(sourceTestTimeMS),
			Duration: time.Minute,
			Limit:    10,
		})
		if err != nil {
			t.Fatalf("LeaseDue(): %v", err)
		}
		for _, l := range leases {
			if l.OutboxID == "outbox-"+id {
				return l
			}
		}
		t.Fatalf("no lease for %q", id)
		return sqlite.Lease{}
	}
	statusFor := func(t *testing.T, id string) string {
		t.Helper()
		msgs, err := source.GetMessagesByConversation("conv", 100)
		if err != nil {
			t.Fatalf("GetMessagesByConversation(): %v", err)
		}
		for _, m := range msgs {
			if m.MessageID == id {
				return m.Status
			}
		}
		t.Fatalf("message %q not found", id)
		return ""
	}

	// queued → sending
	enqueue(t, "queued-msg")
	if got := statusFor(t, "queued-msg"); got != db.OutgoingSendStatusSending {
		t.Fatalf("queued status = %q, want sending", got)
	}

	// confirmed → sent
	enqueue(t, "confirmed-msg")
	cl := lease(t, "confirmed-msg")
	if cl.LeaseToken == nil {
		t.Fatal("confirmed lease has no token")
	}
	if err := outbox.MarkTransportCalled(ctx, sqlite.Attempt{
		OutboxID:   "outbox-confirmed-msg",
		LeaseToken: *cl.LeaseToken,
	}); err != nil {
		t.Fatalf("MarkTransportCalled(): %v", err)
	}
	if err := outbox.Confirm(ctx, sqlite.Confirmation{
		OutboxID:       "outbox-confirmed-msg",
		LeaseToken:     *cl.LeaseToken,
		ResultRemoteID: "permanent-confirmed",
	}); err != nil {
		t.Fatalf("Confirm(): %v", err)
	}
	if got := statusFor(t, "confirmed-msg"); got != db.OutgoingSendStatusSent {
		t.Fatalf("confirmed status = %q, want sent", got)
	}

	// rejected → failed
	enqueue(t, "rejected-msg")
	rl := lease(t, "rejected-msg")
	if err := outbox.Reject(ctx, "outbox-rejected-msg", *rl.LeaseToken, "unsupported", "no_route", "nope"); err != nil {
		t.Fatalf("Reject(): %v", err)
	}
	if got := statusFor(t, "rejected-msg"); got != db.OutgoingSendStatusFailed {
		t.Fatalf("rejected status = %q, want failed", got)
	}

	// uncertain → sending (never failed: an ambiguous send is not a failure)
	enqueue(t, "uncertain-msg")
	ul := lease(t, "uncertain-msg")
	if err := outbox.MarkTransportCalled(ctx, sqlite.Attempt{
		OutboxID:   "outbox-uncertain-msg",
		LeaseToken: *ul.LeaseToken,
	}); err != nil {
		t.Fatalf("MarkTransportCalled(uncertain): %v", err)
	}
	if err := outbox.MarkUncertain(ctx, "outbox-uncertain-msg", *ul.LeaseToken, "transient", "timeout", "unknown"); err != nil {
		t.Fatalf("MarkUncertain(): %v", err)
	}
	if got := statusFor(t, "uncertain-msg"); got != db.OutgoingSendStatusSending {
		t.Fatalf("uncertain status = %q, want sending (never failed)", got)
	}

	// A received message (no outbox row) carries no status.
	importSourceMessage(t, messages, sqlite.Message{
		MessageID:       "incoming-msg",
		ConversationID:  "conv",
		AccountID:       "acct",
		RemoteMessageID: "remote-incoming",
		Direction:       sqlite.MessageDirectionIncoming,
		Body:            "hi",
		State:           sqlite.MessageStateActive,
		OccurredAtMS:    sourceTestTimeMS,
	})
	if got := statusFor(t, "incoming-msg"); got != "" {
		t.Fatalf("incoming status = %q, want empty", got)
	}
}
