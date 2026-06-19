package db

import "testing"

func TestValidateScheduleTime(t *testing.T) {
	now := int64(1_000_000_000_000)
	cases := []struct {
		name   string
		sendAt int64
		ok     bool
	}{
		{"comfortably future", now + 60_000, true},
		{"exactly now", now, false},
		{"in the past", now - 1, false},
		{"under the 10s floor", now + 5_000, false},
		{"just over the floor", now + 11_000, true},
		{"zero", 0, false},
		{"too far (over a year)", now + 400*24*60*60*1000, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateScheduleTime(tc.sendAt, now)
			if (err == nil) != tc.ok {
				t.Errorf("ValidateScheduleTime(%d) err=%v, want ok=%v", tc.sendAt, err, tc.ok)
			}
		})
	}
}

func TestScheduledMessageLifecycle(t *testing.T) {
	store := newTestStore(t)
	now := int64(1_700_000_000_000)

	m := &ScheduledMessage{
		ID: "s1", ConversationID: "c1", Body: "later text", SendAt: now + 60_000, CreatedAt: now,
	}
	if err := store.CreateScheduledMessage(m); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Lists for the conversation.
	list, err := store.ListScheduledMessages("c1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].ID != "s1" || list[0].Status != ScheduleStatusPending {
		t.Fatalf("list = %+v, want one pending s1", list)
	}

	// Not due yet.
	if due, _ := store.GetDueScheduledMessages(now); len(due) != 0 {
		t.Errorf("should not be due before send_at, got %d", len(due))
	}
	// Due once the time has passed.
	due, err := store.GetDueScheduledMessages(now + 60_001)
	if err != nil {
		t.Fatalf("getdue: %v", err)
	}
	if len(due) != 1 || due[0].ID != "s1" {
		t.Fatalf("due = %+v, want s1", due)
	}

	// Atomic claim: first claim succeeds, second fails (exactly-once).
	claimed, err := store.ClaimScheduledMessage("s1")
	if err != nil || !claimed {
		t.Fatalf("first claim should succeed, got claimed=%v err=%v", claimed, err)
	}
	claimedAgain, _ := store.ClaimScheduledMessage("s1")
	if claimedAgain {
		t.Errorf("second claim must fail (already sending)")
	}
	// A claimed (sending) message is no longer "due".
	if due, _ := store.GetDueScheduledMessages(now + 60_001); len(due) != 0 {
		t.Errorf("claimed message should not be due, got %d", len(due))
	}

	// Mark sent.
	if err := store.MarkScheduledMessageSent("s1", "msg-123"); err != nil {
		t.Fatalf("mark sent: %v", err)
	}
	got, _ := store.GetScheduledMessage("s1")
	if got == nil || got.Status != ScheduleStatusSent || got.SentMessageID != "msg-123" {
		t.Errorf("after sent = %+v", got)
	}
}

func TestScheduledMessageCancelAndFail(t *testing.T) {
	store := newTestStore(t)
	now := int64(1_700_000_000_000)
	mk := func(id string) {
		if err := store.CreateScheduledMessage(&ScheduledMessage{ID: id, ConversationID: "c1", Body: "x", SendAt: now + 60_000, CreatedAt: now}); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}

	// Cancel a pending message succeeds; cancelling again fails (not pending).
	mk("p1")
	if ok, err := store.CancelScheduledMessage("p1"); err != nil || !ok {
		t.Fatalf("cancel pending should succeed, ok=%v err=%v", ok, err)
	}
	if ok, _ := store.CancelScheduledMessage("p1"); ok {
		t.Errorf("cancelling an already-cancelled message must fail")
	}
	// A cancelled message never becomes due.
	if due, _ := store.GetDueScheduledMessages(now + 60_001); len(due) != 0 {
		t.Errorf("cancelled message should not be due")
	}

	// Failure path: claim, then mark failed with an error + attempt count.
	mk("f1")
	store.ClaimScheduledMessage("f1")
	if err := store.MarkScheduledMessageFailed("f1", "route unavailable"); err != nil {
		t.Fatalf("mark failed: %v", err)
	}
	got, _ := store.GetScheduledMessage("f1")
	if got.Status != ScheduleStatusFailed || got.Attempts != 1 || got.LastError != "route unavailable" {
		t.Errorf("after failure = %+v", got)
	}

	// Transient revert: claim, revert to pending — becomes due again.
	mk("r1")
	store.ClaimScheduledMessage("r1")
	if err := store.RevertScheduledMessageToPending("r1", "offline"); err != nil {
		t.Fatalf("revert: %v", err)
	}
	if due, _ := store.GetDueScheduledMessages(now + 60_001); len(due) != 1 || due[0].ID != "r1" {
		t.Errorf("reverted message should be due again, got %+v", due)
	}
}
