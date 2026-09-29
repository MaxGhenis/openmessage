package sqlite

// The send-window gate at the transport boundary. LeaseDue refuses expired
// rows, but a leased batch is dispatched one item at a time, so a later
// item's window can close while an earlier item is still in its transport
// call. MarkTransportCalled therefore re-checks the window at the instant it
// commits the transport-call marker, and cancels the caller's own uncalled
// row as expired when the window has closed. These tests pin that gate:
//
//	I1 transport_called_at_ms commits at t only if expires_at_ms IS NULL or
//	   t < expires_at_ms.
//	I2 for a dispatching, uncalled row, exactly one outcome holds: nil (token
//	   matches, lease live, window open), ErrSendWindowExpired (token matches,
//	   window closed; row canceled/ttl, lease cleared, attempt_count
//	   unchanged), or ErrLeaseLost with the row unchanged.
//	I3 expiry never modifies a row that may have been sent: a called row, or
//	   one in uncertain/confirmed/store_failed/rejected.
//	I6 LeaseDue, MarkTransportCalled, and CancelExpired split time at the
//	   same instant (expires_at_ms <= now is expired).

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"testing"
	"testing/quick"
	"time"
)

func leaseOneForGate(t *testing.T, repository *OutboxRepository, now int64, duration time.Duration) Lease {
	t.Helper()
	leases, err := repository.LeaseDue(context.Background(), LeaseRequest{
		Owner:    "gate-test",
		Now:      time.UnixMilli(now),
		Duration: duration,
		Limit:    1,
	})
	if err != nil {
		t.Fatalf("LeaseDue(): %v", err)
	}
	if len(leases) != 1 {
		t.Fatalf("LeaseDue() leased %d rows, want 1", len(leases))
	}
	return leases[0]
}

func mustFindOutbox(t *testing.T, repository *OutboxRepository, id string) OutboxItem {
	t.Helper()
	row, err := repository.FindByID(context.Background(), id)
	if err != nil {
		t.Fatalf("FindByID(%s): %v", id, err)
	}
	return row
}

func gateAttempt(lease Lease) Attempt {
	return Attempt{
		OutboxID:             lease.OutboxID,
		LeaseToken:           *lease.LeaseToken,
		AttemptToken:         lease.OutboxID + ":" + *lease.LeaseToken,
		ConnectionGeneration: 1,
	}
}

func assertCanceledAtGate(t *testing.T, row OutboxItem, attemptsBefore int64) {
	t.Helper()
	if row.State != OutboxCanceled {
		t.Fatalf("state = %q, want canceled; row=%+v", row.State, row)
	}
	if row.ErrorClass == nil || *row.ErrorClass != TTLErrorClass ||
		row.ErrorCode == nil || *row.ErrorCode != TTLErrorCode {
		t.Fatalf("error class/code = %v/%v, want %q/%q", row.ErrorClass, row.ErrorCode, TTLErrorClass, TTLErrorCode)
	}
	if row.ErrorDetail == nil || *row.ErrorDetail != ttlErrorDetail {
		t.Fatalf("error_detail = %v, want %q", row.ErrorDetail, ttlErrorDetail)
	}
	if row.LeaseOwner != nil || row.LeaseToken != nil || row.LeaseExpiresAtMS != nil {
		t.Fatalf("lease not cleared: owner=%v token=%v expires=%v", row.LeaseOwner, row.LeaseToken, row.LeaseExpiresAtMS)
	}
	if row.TransportCalledAtMS != nil {
		t.Fatalf("transport_called_at_ms = %d, want NULL (never sent)", *row.TransportCalledAtMS)
	}
	if row.NextAttemptAtMS != nil {
		t.Fatalf("next_attempt_at_ms = %d, want NULL", *row.NextAttemptAtMS)
	}
	if row.AttemptCount != attemptsBefore {
		t.Fatalf("attempt_count = %d, want unchanged %d", row.AttemptCount, attemptsBefore)
	}
}

func TestMarkTransportCalledCancelsExpiredLeasedRow(t *testing.T) {
	clock := newOutboxTestClock(outboxTestTimeMS)
	_, repository := openOutboxTestRepository(t, clock.Now)
	ctx := context.Background()

	item := outboxExpiryItem("gate-expired", outboxTestTimeMS+(10*time.Second).Milliseconds())
	mustEnqueueExpiry(t, repository, item)
	lease := leaseOneForGate(t, repository, outboxTestTimeMS, time.Minute)
	before := mustFindOutbox(t, repository, item.OutboxID)

	// 11s later: the lease is still live, but the 10s window has closed.
	clock.Set(outboxTestTimeMS + (11 * time.Second).Milliseconds())
	err := repository.MarkTransportCalled(ctx, gateAttempt(lease))
	if !errors.Is(err, ErrSendWindowExpired) {
		t.Fatalf("MarkTransportCalled(expired) = %v, want ErrSendWindowExpired", err)
	}
	after := mustFindOutbox(t, repository, item.OutboxID)
	assertCanceledAtGate(t, after, before.AttemptCount)
	if after.UpdatedAtMS != outboxTestTimeMS+(11*time.Second).Milliseconds() {
		t.Fatalf("updated_at_ms = %d, want the gate instant", after.UpdatedAtMS)
	}

	// A second call finds no lease to own and does not cancel again.
	clock.Set(outboxTestTimeMS + (12 * time.Second).Milliseconds())
	if err := repository.MarkTransportCalled(ctx, gateAttempt(lease)); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("second MarkTransportCalled = %v, want ErrLeaseLost", err)
	}
	if again := mustFindOutbox(t, repository, item.OutboxID); !reflect.DeepEqual(again, after) {
		t.Fatalf("second call modified the canceled row:\n before %+v\n after  %+v", after, again)
	}
}

func TestMarkTransportCalledAtExactExpiryIsExpired(t *testing.T) {
	clock := newOutboxTestClock(outboxTestTimeMS)
	_, repository := openOutboxTestRepository(t, clock.Now)
	expiresAt := outboxTestTimeMS + 5_000

	open := outboxExpiryItem("gate-last-open-ms", expiresAt)
	mustEnqueueExpiry(t, repository, open)
	openLease := leaseOneForGate(t, repository, outboxTestTimeMS, time.Minute)
	clock.Set(expiresAt - 1)
	if err := repository.MarkTransportCalled(context.Background(), gateAttempt(openLease)); err != nil {
		t.Fatalf("MarkTransportCalled(1ms before expiry) = %v, want nil", err)
	}
	marked := mustFindOutbox(t, repository, open.OutboxID)
	if marked.TransportCalledAtMS == nil || *marked.TransportCalledAtMS != expiresAt-1 || marked.AttemptCount != 1 {
		t.Fatalf("marked row = %+v, want transport_called_at_ms=%d attempt_count=1", marked, expiresAt-1)
	}

	clock.Set(outboxTestTimeMS)
	closed := outboxExpiryItem("gate-at-expiry", expiresAt)
	mustEnqueueExpiry(t, repository, closed)
	closedLease := leaseOneForGate(t, repository, outboxTestTimeMS, time.Minute)
	clock.Set(expiresAt)
	if err := repository.MarkTransportCalled(context.Background(), gateAttempt(closedLease)); !errors.Is(err, ErrSendWindowExpired) {
		t.Fatalf("MarkTransportCalled(at expiry) = %v, want ErrSendWindowExpired", err)
	}
	assertCanceledAtGate(t, mustFindOutbox(t, repository, closed.OutboxID), 0)
}

// TestSendWindowBoundaryIsConsistentAcrossLeaseGateAndSweep executes I6
// exhaustively around the boundary: at every instant, a row is leasable and
// markable exactly when CancelExpired would leave it alone.
func TestSendWindowBoundaryIsConsistentAcrossLeaseGateAndSweep(t *testing.T) {
	const expiresAt = outboxTestTimeMS + 10_000
	for offset := int64(-3); offset <= 3; offset++ {
		t.Run(fmt.Sprintf("now=expires%+d", offset), func(t *testing.T) {
			now := expiresAt + offset
			windowOpen := now < expiresAt
			ctx := context.Background()

			// LeaseDue at now.
			leaseClock := newOutboxTestClock(now)
			_, leaseRepository := openOutboxTestRepository(t, leaseClock.Now)
			mustEnqueueExpiry(t, leaseRepository, outboxExpiryItem("boundary-lease", expiresAt))
			leases, err := leaseRepository.LeaseDue(ctx, LeaseRequest{
				Owner: "boundary", Now: time.UnixMilli(now), Duration: time.Minute, Limit: 1,
			})
			if err != nil {
				t.Fatalf("LeaseDue(): %v", err)
			}
			leased := len(leases) == 1

			// MarkTransportCalled at now for a row leased earlier.
			gateClock := newOutboxTestClock(outboxTestTimeMS)
			_, gateRepository := openOutboxTestRepository(t, gateClock.Now)
			mustEnqueueExpiry(t, gateRepository, outboxExpiryItem("boundary-gate", expiresAt))
			lease := leaseOneForGate(t, gateRepository, outboxTestTimeMS, time.Minute)
			gateClock.Set(now)
			gateErr := gateRepository.MarkTransportCalled(ctx, gateAttempt(lease))
			if gateErr != nil && !errors.Is(gateErr, ErrSendWindowExpired) {
				t.Fatalf("MarkTransportCalled() = %v, want nil or ErrSendWindowExpired", gateErr)
			}
			marked := gateErr == nil

			// CancelExpired at now.
			sweepClock := newOutboxTestClock(outboxTestTimeMS)
			_, sweepRepository := openOutboxTestRepository(t, sweepClock.Now)
			mustEnqueueExpiry(t, sweepRepository, outboxExpiryItem("boundary-sweep", expiresAt))
			canceled, err := sweepRepository.CancelExpired(ctx, time.UnixMilli(now))
			if err != nil {
				t.Fatalf("CancelExpired(): %v", err)
			}
			swept := len(canceled) == 1

			if leased != windowOpen || marked != windowOpen || swept != !windowOpen {
				t.Fatalf("at now-expires=%d: leased=%v marked=%v swept=%v; want leased=marked=%v swept=%v",
					offset, leased, marked, swept, windowOpen, !windowOpen)
			}
		})
	}
}

// gateCase is one generated MarkTransportCalled scenario. Offsets are
// milliseconds relative to the gate instant and are biased toward the -1/0/+1
// boundary so the property is not vacuous.
type gateCase struct {
	ExpiryOffset int64 // expires_at_ms - gate instant
	NoExpiry     bool
	LeaseOffset  int64 // lease_expires_at_ms - gate instant
	TokenMatches bool
	PreCalled    bool // the same lease already committed its marker
}

// gateLeadMS is how long before the gate instant the row is leased. Every
// offset is kept above -gateLeadMS so the row is leasable at lease time.
const gateLeadMS = int64(6_000)

func boundaryBiasedOffset(r *rand.Rand) int64 {
	switch r.Intn(4) {
	case 0:
		return int64(r.Intn(3)) - 1
	default:
		return int64(r.Intn(9_999)) - 4_999
	}
}

func (gateCase) Generate(r *rand.Rand, _ int) reflect.Value {
	return reflect.ValueOf(gateCase{
		ExpiryOffset: boundaryBiasedOffset(r),
		NoExpiry:     r.Intn(8) == 0,
		LeaseOffset:  boundaryBiasedOffset(r),
		TokenMatches: r.Intn(4) != 0,
		PreCalled:    r.Intn(6) == 0,
	})
}

// TestQuickMarkTransportCalledGateOutcomes executes I1 and I2 (and the
// called-row half of I3) over generated window/lease/token/called states.
// Before the gate existed, a closed window with a live lease returned nil and
// committed transport_called_at_ms at or after expires_at_ms.
func TestQuickMarkTransportCalledGateOutcomes(t *testing.T) {
	clock := newOutboxTestClock(outboxTestTimeMS)
	store, repository := openOutboxTestRepository(t, clock.Now)
	ctx := context.Background()
	gateAt := outboxTestTimeMS + gateLeadMS
	iteration := 0
	outcomes := map[string]int{}

	property := func(c gateCase) bool {
		iteration++
		if !c.NoExpiry && c.ExpiryOffset == 0 {
			outcomes["window closes exactly at the gate"]++
		}
		clock.Set(outboxTestTimeMS)
		item := outboxTestItem(fmt.Sprintf("gate-quick-%d", iteration))
		if !c.NoExpiry {
			item.ExpiresAtMS = gateAt + c.ExpiryOffset
		}
		mustEnqueueExpiry(t, repository, item)
		lease := leaseOneForGate(t, repository, outboxTestTimeMS, time.Duration(gateLeadMS+c.LeaseOffset)*time.Millisecond)
		// Retire the row afterwards so later iterations lease only their own.
		defer func() {
			if _, err := store.db.Exec(`
				UPDATE outbox
				SET state = 'canceled', lease_owner = NULL, lease_token = NULL,
					lease_expires_at_ms = NULL, transport_called_at_ms = NULL
				WHERE outbox_id = ?
			`, item.OutboxID); err != nil {
				t.Fatalf("retire %s: %v", item.OutboxID, err)
			}
		}()
		attempt := gateAttempt(lease)
		if c.PreCalled {
			if err := repository.MarkTransportCalled(ctx, attempt); err != nil {
				t.Logf("case %+v: pre-call MarkTransportCalled = %v", c, err)
				return false
			}
		}
		if !c.TokenMatches {
			attempt.LeaseToken = "not-the-lease-token"
		}
		before := mustFindOutbox(t, repository, item.OutboxID)

		clock.Set(gateAt)
		err := repository.MarkTransportCalled(ctx, attempt)
		after := mustFindOutbox(t, repository, item.OutboxID)

		windowOpen := c.NoExpiry || c.ExpiryOffset > 0
		leaseLive := c.LeaseOffset > 0
		owns := c.TokenMatches && !c.PreCalled
		wantMarked := owns && leaseLive && windowOpen
		wantExpired := owns && !windowOpen

		switch {
		case err == nil:
			outcomes["marked"]++
			ok := wantMarked &&
				after.State == OutboxDispatching &&
				after.TransportCalledAtMS != nil && *after.TransportCalledAtMS == gateAt &&
				(after.ExpiresAtMS == nil || *after.TransportCalledAtMS < *after.ExpiresAtMS) &&
				after.AttemptCount == before.AttemptCount+1
			if !ok {
				t.Logf("case %+v: nil outcome, want marked=%v; after=%+v", c, wantMarked, after)
			}
			return ok
		case errors.Is(err, ErrSendWindowExpired):
			outcomes["expired"]++
			if !leaseLive {
				outcomes["expired with the lease also expired"]++
			}
			ok := wantExpired &&
				after.State == OutboxCanceled &&
				after.ErrorClass != nil && *after.ErrorClass == TTLErrorClass &&
				after.ErrorCode != nil && *after.ErrorCode == TTLErrorCode &&
				after.LeaseToken == nil && after.LeaseExpiresAtMS == nil && after.LeaseOwner == nil &&
				after.TransportCalledAtMS == nil &&
				after.AttemptCount == before.AttemptCount
			if !ok {
				t.Logf("case %+v: expired outcome, want expired=%v; after=%+v", c, wantExpired, after)
			}
			return ok
		case errors.Is(err, ErrLeaseLost):
			outcomes["lease lost"]++
			if c.PreCalled && c.TokenMatches && !windowOpen {
				outcomes["called row past its window"]++
			}
			ok := !wantMarked && !wantExpired && reflect.DeepEqual(before, after)
			if !ok {
				t.Logf("case %+v: lease-lost outcome, want marked=%v expired=%v unchanged row;\n before %+v\n after  %+v",
					c, wantMarked, wantExpired, before, after)
			}
			return ok
		default:
			t.Logf("case %+v: unexpected error %v", c, err)
			return false
		}
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 400, Rand: rand.New(rand.NewSource(166_01))}); err != nil {
		t.Fatal(err)
	}
	for _, outcome := range []string{
		"marked", "expired", "lease lost", "window closes exactly at the gate",
		"expired with the lease also expired", "called row past its window",
	} {
		if outcomes[outcome] == 0 {
			t.Fatalf("generator never produced %q; outcomes=%v", outcome, outcomes)
		}
	}
}

// possiblySentState names a row state that expiry must never touch.
type possiblySentState int

const (
	sentDispatchingCalled possiblySentState = iota
	sentUncertain
	sentConfirmed
	sentStoreFailed
	sentRejectedAfterCall
	possiblySentStateCount
)

func (s possiblySentState) String() string {
	return [...]string{"dispatching-called", "uncertain", "confirmed", "store_failed", "rejected-after-call"}[s]
}

type possiblySentCase struct {
	State         possiblySentState
	ExpiryOffset  int64 // expires_at_ms after shrinking, relative to the probe instant
	ProbeAfterMS  int64 // probe instant relative to the transport call
	ReuseOldToken bool
}

func (possiblySentCase) Generate(r *rand.Rand, _ int) reflect.Value {
	return reflect.ValueOf(possiblySentCase{
		State:         possiblySentState(r.Intn(int(possiblySentStateCount))),
		ExpiryOffset:  -int64(r.Intn(5_000)),
		ProbeAfterMS:  int64(r.Intn(90_000)),
		ReuseOldToken: r.Intn(2) == 0,
	})
}

// TestQuickExpiryNeverTouchesPossiblySentRows executes I3: once a row's
// transport call has been committed, neither the sweep nor the gate's cancel
// branch modifies it, whatever its window, the clock, or the token presented
// (including the original lease token).
func TestQuickExpiryNeverTouchesPossiblySentRows(t *testing.T) {
	clock := newOutboxTestClock(outboxTestTimeMS)
	store, repository := openOutboxTestRepository(t, clock.Now)
	ctx := context.Background()
	iteration := 0

	property := func(c possiblySentCase) bool {
		iteration++
		clock.Set(outboxTestTimeMS)
		item := outboxExpiryItem(fmt.Sprintf("sent-%d", iteration), outboxTestTimeMS+time.Hour.Milliseconds())
		mustEnqueueExpiry(t, repository, item)
		lease := leaseOneForGate(t, repository, outboxTestTimeMS, 30*time.Second)
		defer func() {
			if _, err := store.db.Exec(`
				UPDATE outbox
				SET state = 'canceled', lease_owner = NULL, lease_token = NULL,
					lease_expires_at_ms = NULL, transport_called_at_ms = NULL
				WHERE outbox_id = ?
			`, item.OutboxID); err != nil {
				t.Fatalf("retire %s: %v", item.OutboxID, err)
			}
		}()
		attempt := gateAttempt(lease)
		if err := repository.MarkTransportCalled(ctx, attempt); err != nil {
			t.Fatalf("MarkTransportCalled(open window): %v", err)
		}
		var err error
		switch c.State {
		case sentDispatchingCalled:
		case sentUncertain:
			err = repository.MarkUncertain(ctx, item.OutboxID, attempt.LeaseToken, "unknown", "timeout", "test")
		case sentConfirmed:
			err = repository.ConfirmWithoutResult(ctx, item.OutboxID, attempt.LeaseToken)
		case sentStoreFailed:
			err = repository.MarkStoreFailed(ctx, item.OutboxID, attempt.LeaseToken, "remote-result", "test")
		case sentRejectedAfterCall:
			err = repository.Reject(ctx, item.OutboxID, attempt.LeaseToken, "unsupported", "test", "test")
		}
		if err != nil {
			t.Fatalf("drive %s: %v", c.State, err)
		}
		probeAt := outboxTestTimeMS + c.ProbeAfterMS
		if _, err := store.db.Exec(
			`UPDATE outbox SET expires_at_ms = ? WHERE outbox_id = ?`, probeAt+c.ExpiryOffset, item.OutboxID,
		); err != nil {
			t.Fatalf("shrink window: %v", err)
		}
		before := mustFindOutbox(t, repository, item.OutboxID)

		clock.Set(probeAt)
		if _, err := repository.CancelExpired(ctx, time.UnixMilli(probeAt)); err != nil {
			t.Fatalf("CancelExpired(): %v", err)
		}
		if !c.ReuseOldToken {
			attempt.LeaseToken = "some-other-token"
		}
		gateErr := repository.MarkTransportCalled(ctx, attempt)
		after := mustFindOutbox(t, repository, item.OutboxID)
		if errors.Is(gateErr, ErrSendWindowExpired) || gateErr == nil {
			t.Logf("case %+v (%s): gate returned %v for a possibly-sent row", c, c.State, gateErr)
			return false
		}
		if !reflect.DeepEqual(before, after) {
			t.Logf("case %+v (%s): row modified:\n before %+v\n after  %+v", c, c.State, before, after)
			return false
		}
		return true
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 200, Rand: rand.New(rand.NewSource(166_03))}); err != nil {
		t.Fatal(err)
	}
}
