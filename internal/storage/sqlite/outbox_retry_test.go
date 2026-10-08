package sqlite

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
)

func TestOutboxMarkCalledNotDispatchedRefundingAttemptGivesBackTheAttempt(t *testing.T) {
	clock := newOutboxTestClock(outboxTestTimeMS)
	store, repository := openOutboxTestRepository(t, clock.Now)
	ctx := context.Background()
	item := outboxTestItem("refund")
	mustEnqueueOutbox(t, repository, item)
	lease := mustLeaseOne(t, repository, LeaseRequest{
		Owner: "worker", Now: clock.Now(), Duration: time.Minute, Limit: 1,
	})
	token := mustLeaseToken(t, lease)
	retryAt := clock.Now().Add(5 * time.Second)

	// Same call-marker predicate as MarkCalledNotDispatched: a pre-call lease
	// must use MarkNotDispatched instead.
	if err := repository.MarkCalledNotDispatchedRefundingAttempt(
		ctx, item.OutboxID, token, "transient", "send_text", "offline", retryAt,
	); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("refund before the transport call error = %v, want ErrLeaseLost", err)
	}
	if err := repository.MarkTransportCalled(ctx, Attempt{OutboxID: item.OutboxID, LeaseToken: token}); err != nil {
		t.Fatalf("MarkTransportCalled(): %v", err)
	}
	if err := repository.MarkCalledNotDispatchedRefundingAttempt(
		ctx, item.OutboxID, "stale-token", "transient", "send_text", "offline", retryAt,
	); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("refund with a stale lease token error = %v, want ErrLeaseLost", err)
	}
	if err := repository.MarkCalledNotDispatchedRefundingAttempt(
		ctx, item.OutboxID, token, "transient", "send_text", "offline", time.UnixMilli(0),
	); err == nil {
		t.Fatal("refund with a non-positive retry time succeeded")
	}
	if err := repository.MarkCalledNotDispatchedRefundingAttempt(
		ctx, item.OutboxID, token, "transient", "send_text", "[google_not_connected] offline", retryAt,
	); err != nil {
		t.Fatalf("MarkCalledNotDispatchedRefundingAttempt(): %v", err)
	}
	row, err := repository.FindByID(ctx, item.OutboxID)
	if err != nil {
		t.Fatalf("FindByID(): %v", err)
	}
	if row.State != OutboxNotDispatched || row.AttemptCount != 0 || row.LeaseToken != nil ||
		row.TransportCalledAtMS != nil || row.NextAttemptAtMS == nil ||
		*row.NextAttemptAtMS != retryAt.UnixMilli() ||
		row.TransportRequestID != item.TransportRequestID {
		t.Fatalf("refunded row = %+v", row)
	}
	assertOutboxText(t, "error class", row.ErrorClass, "transient")
	assertOutboxText(t, "error code", row.ErrorCode, "send_text")
	assertOutboxText(t, "error detail", row.ErrorDetail, "[google_not_connected] offline")

	// The refund never drives attempt_count below zero (its CHECK would
	// otherwise fail the whole transition).
	clock.Set(retryAt.UnixMilli())
	second := mustLeaseOne(t, repository, LeaseRequest{
		Owner: "worker", Now: clock.Now(), Duration: time.Minute, Limit: 1,
	})
	secondToken := mustLeaseToken(t, second)
	if err := repository.MarkTransportCalled(ctx, Attempt{OutboxID: item.OutboxID, LeaseToken: secondToken}); err != nil {
		t.Fatalf("MarkTransportCalled(second): %v", err)
	}
	mustExec(t, store.db, `UPDATE outbox SET attempt_count = 0 WHERE outbox_id = ?`, item.OutboxID)
	if err := repository.MarkCalledNotDispatchedRefundingAttempt(
		ctx, item.OutboxID, secondToken, "credentials_expired", "send_text", "repairing", retryAt.Add(time.Second),
	); err != nil {
		t.Fatalf("MarkCalledNotDispatchedRefundingAttempt(at zero): %v", err)
	}
	if row, err := repository.FindByID(ctx, item.OutboxID); err != nil {
		t.Fatalf("FindByID(at zero): %v", err)
	} else if row.State != OutboxNotDispatched || row.AttemptCount != 0 {
		t.Fatalf("refund at zero = %+v, want not_dispatched with attempt_count 0", row)
	}
}

func TestOutboxListPendingShowsActionableRejectedRowsOnly(t *testing.T) {
	clock := newOutboxTestClock(outboxTestTimeMS)
	store, repository := openOutboxTestRepository(t, clock.Now)
	ctx := context.Background()
	start := clock.Now()
	seedMessageIdentity(t, store, "identity-a", "account-a")
	seedMessageConversation(t, store, "conversation-a", "account-a")
	seedOutboxTestDevice(t, store, "device-a", "account-a")
	seedOutboxTestMessage(t, store, "target", "account-a", "conversation-a")
	seedOutboxTestMessage(t, store, "target-read", "account-a", "conversation-a")

	reject := func(item NewOutboxItem, class string) {
		t.Helper()
		lease := mustLeaseOne(t, repository, LeaseRequest{
			Owner: "worker-" + item.OutboxID, Now: clock.Now(), Duration: time.Minute, Limit: 1,
		})
		if lease.OutboxID != item.OutboxID {
			t.Fatalf("leased %q, want %q", lease.OutboxID, item.OutboxID)
		}
		if err := repository.Reject(ctx, item.OutboxID, mustLeaseToken(t, lease), class, item.Operation, "gave up"); err != nil {
			t.Fatalf("Reject(%q): %v", item.OutboxID, err)
		}
	}
	rejectText := func(id, class string) NewOutboxItem {
		t.Helper()
		item := outboxTestItem(id)
		mustEnqueueOutgoingOutbox(t, repository, item, id+" body")
		reject(item, class)
		return item
	}

	// Rejected an hour before the window closes on the listing time, and
	// rejected exactly one window before it (excluded: the window is strict).
	clock.Set(start.UnixMilli())
	boundary := rejectText("rejected-at-boundary", RetryExhaustedErrorClass)
	clock.Set(start.Add(time.Hour).UnixMilli())
	inside := rejectText("rejected-inside-window", RetryExhaustedErrorClass)

	clock.Set(start.Add(2 * time.Hour).UnixMilli())
	exhaustedText := rejectText("exhausted-text", RetryExhaustedErrorClass)
	reauthText := rejectText("reauth-text", "reauth_required")
	rejectText("permanent-text", "permanent")
	rejectText("misconfigured-text", "misconfigured")
	rejectText("unsupported-text", "unsupported")
	rejectText("unpaired-text", "unpaired")

	media := outboxTestMediaItem("exhausted-media")
	if _, _, err := repository.EnqueueOutgoingMediaMessage(
		ctx,
		media,
		outboxTestOutgoingMessage(media, "media caption"),
		outboxTestAttachment(),
	); err != nil {
		t.Fatalf("EnqueueOutgoingMediaMessage(): %v", err)
	}
	reject(media, RetryExhaustedErrorClass)

	reaction := outboxTestReactionItem("exhausted-reaction")
	if _, _, err := repository.EnqueueReaction(ctx, reaction, OutboxReaction{
		TargetMessageID: "target", Emoji: "👍", Action: "add",
	}); err != nil {
		t.Fatalf("EnqueueReaction(): %v", err)
	}
	reject(reaction, RetryExhaustedErrorClass)

	read := outboxTestReadItem("exhausted-read")
	targetID := "target-read"
	if _, _, err := repository.EnqueueReadReceipt(
		ctx,
		read,
		OutboxReadReceipt{DeviceID: "device-a", LastReadMessageID: targetID, ReadAtMS: clock.Now().UnixMilli()},
		ReadCursor{
			AccountID:         "account-a",
			DeviceID:          "device-a",
			ConversationID:    "conversation-a",
			LastReadMessageID: &targetID,
			LastReadAtMS:      clock.Now().UnixMilli(),
			UpdatedAtMS:       clock.Now().UnixMilli(),
		},
	); err != nil {
		t.Fatalf("EnqueueReadReceipt(): %v", err)
	}
	reject(read, RetryExhaustedErrorClass)

	// An exhausted row the user already sent again leaves the tray; the
	// successor (queued) shows instead.
	resent := rejectText("exhausted-resent", RetryExhaustedErrorClass)
	successor := outboxTestItem("exhausted-resent-successor")
	successor.SendAgainOfOutboxID = resent.OutboxID
	successor.ScheduledFor = clock.Now().Add(time.Hour)
	mustEnqueueOutgoingOutbox(t, repository, successor, "sent again")

	canceled := outboxTestItem("canceled")
	mustEnqueueOutbox(t, repository, canceled)
	if err := repository.Cancel(ctx, canceled.OutboxID); err != nil {
		t.Fatalf("Cancel(): %v", err)
	}

	clock.Set(start.Add(TrayRejectedWindow).UnixMilli())
	rows, err := repository.ListPending(ctx, ListPendingParams{Limit: 50})
	if err != nil {
		t.Fatalf("ListPending(): %v", err)
	}
	got := make([]string, len(rows))
	for i, row := range rows {
		got[i] = row.OutboxID
	}
	// Due order: rejected rows sort by scheduled_for_ms (their next attempt is
	// NULL), then created_at_ms, then outbox_id.
	want := []string{
		inside.OutboxID,
		media.OutboxID,
		exhaustedText.OutboxID,
		reauthText.OutboxID,
		successor.OutboxID,
	}
	if !slices.Equal(got, want) {
		t.Fatalf("ListPending() IDs = %v, want %v (boundary row %q must age out)", got, want, boundary.OutboxID)
	}
	for _, row := range rows[:4] {
		if row.State != OutboxRejected {
			t.Fatalf("listed rejected row = %+v", row)
		}
	}
	if rows[1].MediaFile == nil || *rows[1].MediaFile != outboxTestAttachment().Filename {
		t.Fatalf("listed exhausted media row = %+v, want its attachment summary", rows[1])
	}
	if rows[4].State != OutboxQueued {
		t.Fatalf("listed send-again successor = %+v, want queued", rows[4])
	}

	// Scope filters still apply to the rejected rows.
	scoped, err := repository.ListPending(ctx, ListPendingParams{
		AccountID: "account-a", ConversationID: "conversation-elsewhere", Limit: 50,
	})
	if err != nil {
		t.Fatalf("ListPending(other conversation): %v", err)
	}
	if len(scoped) != 0 {
		t.Fatalf("ListPending(other conversation) = %+v, want none", scoped)
	}
	otherAccount, err := repository.ListPending(ctx, ListPendingParams{AccountID: "account-b", Limit: 50})
	if err != nil {
		t.Fatalf("ListPending(other account): %v", err)
	}
	if len(otherAccount) != 0 {
		t.Fatalf("ListPending(other account) = %+v, want none", otherAccount)
	}

	// One millisecond earlier the boundary row was still inside the window.
	clock.Set(start.Add(TrayRejectedWindow).UnixMilli() - 1)
	earlier, err := repository.ListPending(ctx, ListPendingParams{Limit: 50})
	if err != nil {
		t.Fatalf("ListPending(before boundary): %v", err)
	}
	if len(earlier) != len(want)+1 || earlier[0].OutboxID != boundary.OutboxID {
		t.Fatalf("ListPending(before boundary) = %d rows starting %+v, want the boundary row first", len(earlier), earlier[0])
	}
}
