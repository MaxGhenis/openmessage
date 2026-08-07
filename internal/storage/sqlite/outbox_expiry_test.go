package sqlite

// Expiry-column behavior at the storage layer: CancelExpired's state scope
// and the guarantee that post-transport states are never touched by the
// sweep. The dispatcher-level behavior (lease exclusion, never transmitting
// an expired intent) is covered in internal/messaging.

import (
	"context"
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
	leases, err := repository.LeaseDue(ctx, LeaseRequest{
		Owner:    "expiry-test",
		Now:      now,
		Duration: time.Minute,
		Limit:    10,
	})
	if err != nil {
		t.Fatalf("LeaseDue(): %v", err)
	}
	var crossedLease *OutboxItem
	for i := range leases {
		if leases[i].OutboxID == crossed.OutboxID {
			crossedLease = &leases[i].OutboxItem
		}
		if leases[i].OutboxID == expired.OutboxID {
			t.Fatal("LeaseDue leased an expired item")
		}
	}
	if crossedLease == nil {
		t.Fatalf("crossed item was not leased; got %d leases", len(leases))
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
	if _, err := repository.store.db.Exec(
		`UPDATE outbox SET expires_at_ms = ? WHERE outbox_id = ?`, pastExpiry, crossed.OutboxID,
	); err != nil {
		t.Fatalf("shrink window: %v", err)
	}

	// Release the other leased rows back to queued so the sweep sees the
	// realistic pre-dispatch states.
	for i := range leases {
		if leases[i].OutboxID == crossed.OutboxID {
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
