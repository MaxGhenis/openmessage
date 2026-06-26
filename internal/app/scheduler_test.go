package app

import (
	"fmt"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/maxghenis/openmessage/internal/db"
)

func newSchedulerTestApp(t *testing.T) *App {
	t.Helper()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.UpsertConversation(&db.Conversation{ConversationID: "c1", SourcePlatform: "sms"}); err != nil {
		t.Fatalf("conv: %v", err)
	}
	a := &App{Store: store, Logger: zerolog.Nop()}
	a.Connected.Store(true) // SMS route "ready"
	return a
}

func seedDue(t *testing.T, a *App, id string) {
	t.Helper()
	now := time.Now().UnixMilli()
	if err := a.Store.CreateScheduledMessage(&db.ScheduledMessage{
		ID: id, ConversationID: "c1", Body: "scheduled hi", SendAt: now - 1000,
		Status: db.ScheduleStatusPending, CreatedAt: now,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func TestRecordScheduledSendPersists(t *testing.T) {
	a := newSchedulerTestApp(t)
	msg := &db.Message{
		MessageID:      "wa-sched-1",
		ConversationID: "whatsapp:123@g.us",
		Body:           "scheduled hi",
		IsFromMe:       true,
		TimestampMS:    1000,
		Status:         "OUTGOING_SENT",
	}
	got, err := a.recordScheduledSend(msg, nil)
	if err != nil {
		t.Fatalf("recordScheduledSend: %v", err)
	}
	if got != msg {
		t.Fatal("expected the sent message to be returned")
	}
	// The core of B1: a WhatsApp/Signal scheduled send must be persisted
	// locally, or it never appears in the user's own thread.
	stored, err := a.Store.GetMessageByID("wa-sched-1")
	if err != nil {
		t.Fatalf("GetMessageByID: %v", err)
	}
	if stored == nil {
		t.Fatal("recordScheduledSend must persist the message locally")
	}
	// A send error passes through and persists nothing.
	if _, err := a.recordScheduledSend(nil, fmt.Errorf("send failed")); err == nil {
		t.Fatal("expected the send error to pass through")
	}
}

func TestProcessDueScheduledMessages_Sends(t *testing.T) {
	a := newSchedulerTestApp(t)
	seedDue(t, a, "s1")
	var gotBody string
	a.sendTextOverride = func(cid, body, rt string) (*db.Message, error) {
		gotBody = body
		return &db.Message{MessageID: "m99"}, nil
	}

	a.processDueScheduledMessages()

	if gotBody != "scheduled hi" {
		t.Errorf("send called with body=%q, want %q", gotBody, "scheduled hi")
	}
	got, _ := a.Store.GetScheduledMessage("s1")
	if got.Status != db.ScheduleStatusSent || got.SentMessageID != "m99" {
		t.Errorf("after send = %+v, want sent with m99", got)
	}
}

func TestProcessDueScheduledMessages_OfflineRetries(t *testing.T) {
	a := newSchedulerTestApp(t)
	a.Connected.Store(false) // SMS route NOT ready
	seedDue(t, a, "s1")
	called := false
	a.sendTextOverride = func(cid, body, rt string) (*db.Message, error) {
		called = true
		return &db.Message{MessageID: "x"}, nil
	}

	a.processDueScheduledMessages()

	if called {
		t.Error("send must not be attempted while the route is offline")
	}
	got, _ := a.Store.GetScheduledMessage("s1")
	if got.Status != db.ScheduleStatusPending || got.Attempts != 1 {
		t.Errorf("offline message should revert to pending (attempt 1), got %+v", got)
	}
}

func TestProcessDueScheduledMessages_FailsAfterMaxAttempts(t *testing.T) {
	a := newSchedulerTestApp(t)
	seedDue(t, a, "s1")
	a.sendTextOverride = func(cid, body, rt string) (*db.Message, error) {
		return nil, fmt.Errorf("send rejected")
	}

	// Each tick claims, fails, and either retries or (eventually) marks failed.
	for i := 0; i < scheduleMaxAttempts+1; i++ {
		a.processDueScheduledMessages()
	}

	got, _ := a.Store.GetScheduledMessage("s1")
	if got.Status != db.ScheduleStatusFailed {
		t.Errorf("after exhausting retries, status = %q, want failed", got.Status)
	}
	if got.LastError == "" {
		t.Errorf("expected a recorded error")
	}
}
