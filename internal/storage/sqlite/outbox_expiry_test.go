package sqlite

// Expiry-column behavior at the storage layer: CancelExpired's state scope
// and the guarantee that post-transport states are never touched by the
// sweep. The transport-boundary gate (MarkTransportCalled canceling a leased,
// uncalled row whose window closed) is covered in outbox_gate_test.go; the
// dispatcher-level guarantee (lease exclusion plus that gate, so an expired
// intent is never handed to the transport) is covered in internal/messaging.

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

func outboxExpiryItem(id string, expiresAtMS int64) NewOutboxItem {
	item := outboxTestItem(id)
	item.ExpiresAtMS = expiresAtMS
	return item
}

func mustEnqueueExpiry(t *testing.T, repository *OutboxRepository, item NewOutboxItem) OutboxItem {
	t.Helper()
	row, disposition, err := repository.Enqueue(context.Background(), item)
	if err != nil || disposition != EnqueueInserted {
		t.Fatalf("Enqueue(%s) = %v, %v", item.OutboxID, disposition, err)
	}
	return row
}

func TestExpiryColumnRoundTrip(t *testing.T) {
	clock := newOutboxTestClock(outboxTestTimeMS)
	_, repository := openOutboxTestRepository(t, clock.Now)

	expiry := outboxTestTimeMS + (10 * time.Minute).Milliseconds()
	row := mustEnqueueExpiry(t, repository, outboxExpiryItem("expiry-roundtrip", expiry))
	if row.ExpiresAtMS == nil || *row.ExpiresAtMS != expiry {
		t.Fatalf("expires_at_ms = %v, want %d", row.ExpiresAtMS, expiry)
	}

	unbounded := mustEnqueueExpiry(t, repository, outboxExpiryItem("expiry-unbounded", 0))
	if unbounded.ExpiresAtMS != nil {
		t.Fatalf("unbounded expires_at_ms = %v, want nil", *unbounded.ExpiresAtMS)
	}
}

func TestCancelExpiredScope(t *testing.T) {
	clock := newOutboxTestClock(outboxTestTimeMS)
	_, repository := openOutboxTestRepository(t, clock.Now)
	ctx := context.Background()
	now := time.UnixMilli(outboxTestTimeMS)

	pastExpiry := outboxTestTimeMS - time.Minute.Milliseconds()
	futureExpiry := outboxTestTimeMS + time.Hour.Milliseconds()

	expired := mustEnqueueExpiry(t, repository, outboxExpiryItem("expired", pastExpiry))
	fresh := mustEnqueueExpiry(t, repository, outboxExpiryItem("fresh", futureExpiry))
	unbounded := mustEnqueueExpiry(t, repository, outboxExpiryItem("no-ttl", 0))

	// An item that already crossed the transport boundary must be left alone
	// even once its window closes: lease it, mark the transport called, and
	// record uncertain — then shrink its window into the past.
	crossed := mustEnqueueExpiry(t, repository, outboxExpiryItem("crossed", futureExpiry))
	// Two rows still held by a live dispatch lease whose windows then close:
	// one before its transport call, one after. The sweep must leave both to
	// their lease owner.
	leasedUncalled := mustEnqueueExpiry(t, repository, outboxExpiryItem("leased-uncalled", futureExpiry))
	leasedCalled := mustEnqueueExpiry(t, repository, outboxExpiryItem("leased-called", futureExpiry))
	leases, err := repository.LeaseDue(ctx, LeaseRequest{
		Owner:    "expiry-test",
		Now:      now,
		Duration: time.Minute,
		Limit:    10,
	})
	if err != nil {
		t.Fatalf("LeaseDue(): %v", err)
	}
	var crossedLease, uncalledLease, calledLease *OutboxItem
	for i := range leases {
		switch leases[i].OutboxID {
		case crossed.OutboxID:
			crossedLease = &leases[i].OutboxItem
		case leasedUncalled.OutboxID:
			uncalledLease = &leases[i].OutboxItem
		case leasedCalled.OutboxID:
			calledLease = &leases[i].OutboxItem
		case expired.OutboxID:
			t.Fatal("LeaseDue leased an expired item")
		}
	}
	if crossedLease == nil || uncalledLease == nil || calledLease == nil {
		t.Fatalf("crossed/leased-uncalled/leased-called were not all leased; got %d leases", len(leases))
	}
	calledAttempt := Attempt{
		OutboxID:             leasedCalled.OutboxID,
		LeaseToken:           *calledLease.LeaseToken,
		AttemptToken:         leasedCalled.OutboxID + ":attempt",
		ConnectionGeneration: 1,
		StartedAt:            now,
	}
	if err := repository.MarkTransportCalled(ctx, calledAttempt); err != nil {
		t.Fatalf("MarkTransportCalled(leased-called): %v", err)
	}
	if err := repository.MarkTransportCalled(ctx, Attempt{
		OutboxID:             crossed.OutboxID,
		LeaseToken:           *crossedLease.LeaseToken,
		AttemptToken:         crossed.OutboxID + ":attempt",
		ConnectionGeneration: 1,
		StartedAt:            now,
	}); err != nil {
		t.Fatalf("MarkTransportCalled(): %v", err)
	}
	if err := repository.MarkUncertain(ctx, crossed.OutboxID, *crossedLease.LeaseToken, "unknown", "timeout", "test"); err != nil {
		t.Fatalf("MarkUncertain(): %v", err)
	}
	for _, id := range []string{crossed.OutboxID, leasedUncalled.OutboxID, leasedCalled.OutboxID} {
		if _, err := repository.store.db.Exec(
			`UPDATE outbox SET expires_at_ms = ? WHERE outbox_id = ?`, pastExpiry, id,
		); err != nil {
			t.Fatalf("shrink window of %s: %v", id, err)
		}
	}
	leasedUncalledBefore, err := repository.FindByID(ctx, leasedUncalled.OutboxID)
	if err != nil {
		t.Fatalf("FindByID(leased-uncalled): %v", err)
	}
	leasedCalledBefore, err := repository.FindByID(ctx, leasedCalled.OutboxID)
	if err != nil {
		t.Fatalf("FindByID(leased-called): %v", err)
	}

	// Release the other leased rows back to queued so the sweep sees the
	// realistic pre-dispatch states.
	for i := range leases {
		switch leases[i].OutboxID {
		case crossed.OutboxID, leasedUncalled.OutboxID, leasedCalled.OutboxID:
			continue
		}
		if err := repository.ReleaseUnavailable(ctx, leases[i].OutboxID, *leases[i].LeaseToken); err != nil {
			t.Fatalf("ReleaseUnavailable(%s): %v", leases[i].OutboxID, err)
		}
	}

	canceledIDs, err := repository.CancelExpired(ctx, now)
	if err != nil {
		t.Fatalf("CancelExpired(): %v", err)
	}
	if len(canceledIDs) != 1 || canceledIDs[0] != expired.OutboxID {
		t.Fatalf("canceled = %v, want exactly [%s]", canceledIDs, expired.OutboxID)
	}

	assertState := func(id string, want OutboxState) {
		t.Helper()
		item, err := repository.FindByID(ctx, id)
		if err != nil {
			t.Fatalf("FindByID(%s): %v", id, err)
		}
		if item.State != want {
			t.Fatalf("%s state = %q, want %q", id, item.State, want)
		}
	}
	assertState(expired.OutboxID, OutboxCanceled)
	assertState(fresh.OutboxID, OutboxQueued)
	assertState(unbounded.OutboxID, OutboxQueued)
	assertState(crossed.OutboxID, OutboxUncertain)

	// Both live-lease rows are untouched by the sweep, down to every field.
	for _, want := range []OutboxItem{leasedUncalledBefore, leasedCalledBefore} {
		got, err := repository.FindByID(ctx, want.OutboxID)
		if err != nil {
			t.Fatalf("FindByID(%s): %v", want.OutboxID, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("sweep modified live-lease row %s:\n before %+v\n after  %+v", want.OutboxID, want, got)
		}
	}
	// The lease owner cancels the uncalled one at its transport boundary
	// instead; the called one may have been sent and stays with its owner.
	if err := repository.MarkTransportCalled(ctx, Attempt{
		OutboxID:   leasedUncalled.OutboxID,
		LeaseToken: *uncalledLease.LeaseToken,
	}); !errors.Is(err, ErrSendWindowExpired) {
		t.Fatalf("MarkTransportCalled(leased-uncalled, window closed) = %v, want ErrSendWindowExpired", err)
	}
	assertState(leasedUncalled.OutboxID, OutboxCanceled)
	if err := repository.MarkTransportCalled(ctx, calledAttempt); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("MarkTransportCalled(leased-called again) = %v, want ErrLeaseLost", err)
	}
	assertState(leasedCalled.OutboxID, OutboxDispatching)

	// The swept row carries the TTL markers so readers report "expired
	// unsent" rather than a bare cancellation.
	swept, err := repository.FindByID(ctx, expired.OutboxID)
	if err != nil {
		t.Fatalf("FindByID(swept): %v", err)
	}
	if swept.ErrorClass == nil || *swept.ErrorClass != TTLErrorClass {
		t.Fatalf("error_class = %v, want %q", swept.ErrorClass, TTLErrorClass)
	}
	if swept.ErrorCode == nil || *swept.ErrorCode != TTLErrorCode {
		t.Fatalf("error_code = %v, want %q", swept.ErrorCode, TTLErrorCode)
	}

	// Idempotent: a second sweep finds nothing.
	again, err := repository.CancelExpired(ctx, now)
	if err != nil {
		t.Fatalf("CancelExpired(again): %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("second sweep canceled %v, want nothing", again)
	}
}
