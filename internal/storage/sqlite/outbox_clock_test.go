package sqlite

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/maxghenis/openmessage/internal/bridge"
)

// clockSkewFixture is an outbox whose clock can be set below the creation
// time of the rows it already holds. Two clock reads race that way: a writer
// reads the clock, another writer reads it later and commits first, and the
// first writer's statement then runs against a row created after its read.
// A wall clock stepped back does the same.
type clockSkewFixture struct {
	t      *testing.T
	ctx    context.Context
	clock  *outboxTestClock
	store  *Store
	outbox *OutboxRepository
}

func newClockSkewFixture(t *testing.T) *clockSkewFixture {
	t.Helper()
	clock := newOutboxTestClock(outboxTestTimeMS)
	store, outbox := openOutboxTestRepository(t, clock.Now)
	seedMessageConversation(t, store, "conversation-a", "account-a")
	seedMessageIdentity(t, store, "identity-a", "account-a")
	return &clockSkewFixture{t: t, ctx: context.Background(), clock: clock, store: store, outbox: outbox}
}

func (f *clockSkewFixture) row(outboxID string) OutboxItem {
	f.t.Helper()
	item, err := f.outbox.FindByID(f.ctx, outboxID)
	if err != nil {
		f.t.Fatalf("FindByID(%q): %v", outboxID, err)
	}
	return item
}

// leased enqueues a text intent and leases it at the clock's current time.
func (f *clockSkewFixture) leased(id string) (OutboxItem, string) {
	f.t.Helper()
	item := mustEnqueueOutgoingOutbox(f.t, f.outbox, outboxTestItem(id), "body "+id)
	lease := mustLeaseOne(f.t, f.outbox, LeaseRequest{
		Owner: "dispatcher", Now: f.clock.Now(), Duration: time.Minute, Limit: 1,
	})
	if lease.OutboxID != item.OutboxID {
		f.t.Fatalf("leased %q, want %q", lease.OutboxID, item.OutboxID)
	}
	return lease, mustLeaseToken(f.t, lease)
}

// called leases a text intent and crosses the transport-call boundary.
func (f *clockSkewFixture) called(id string) (OutboxItem, string) {
	f.t.Helper()
	item, token := f.leased(id)
	if err := f.outbox.MarkTransportCalled(f.ctx, Attempt{
		OutboxID: item.OutboxID, LeaseToken: token, StartedAt: f.clock.Now(),
	}); err != nil {
		f.t.Fatalf("MarkTransportCalled(): %v", err)
	}
	return item, token
}

// TestOutboxWritesNeverStampUpdatedAtBelowCreatedAt runs every outbox write
// on a clock that trails the rows it touches, once for each of clockSkews:
// 1 s before they were created, and 1 s after creation but before their last
// update. Each write must succeed, change its row, and leave every row's
// updated_at_ms where it was rather than move it backward or below
// created_at_ms, which the CHECK from 0005_outbox.sql (and 0004 for messages,
// 0010 for reactions) refuses. Cases that need a lease taken before the row's
// creation time get it from LeaseDue on an early clock, which is the write
// the 2026-11-03 race failed.
func TestOutboxWritesNeverStampUpdatedAtBelowCreatedAt(t *testing.T) {
	const skew = time.Second
	before := time.UnixMilli(outboxTestTimeMS).Add(-skew)

	cases := []struct {
		name string
		// arrange runs at outboxTestTimeMS and returns the outbox row the
		// write changes.
		arrange func(f *clockSkewFixture) string
		// write runs on the early clock.
		write func(f *clockSkewFixture, outboxID string) error
	}{
		{
			name: "LeaseDue",
			arrange: func(f *clockSkewFixture) string {
				item := outboxTestItem("lease")
				item.ScheduledFor = before.Add(-skew)
				return mustEnqueueOutgoingOutbox(f.t, f.outbox, item, "body").OutboxID
			},
			write: func(f *clockSkewFixture, outboxID string) error {
				leases, err := f.outbox.LeaseDue(f.ctx, LeaseRequest{
					Owner: "dispatcher", Now: f.clock.Now(), Duration: time.Minute, Limit: 8,
				})
				if err == nil && (len(leases) != 1 || leases[0].OutboxID != outboxID) {
					f.t.Fatalf("LeaseDue() = %+v, want only %q", leases, outboxID)
				}
				return err
			},
		},
		{
			name: "MarkTransportCalled",
			arrange: func(f *clockSkewFixture) string {
				item, _ := f.leased("called")
				return item.OutboxID
			},
			write: func(f *clockSkewFixture, outboxID string) error {
				return f.outbox.MarkTransportCalled(f.ctx, Attempt{
					OutboxID: outboxID, LeaseToken: *f.row(outboxID).LeaseToken, StartedAt: f.clock.Now(),
				})
			},
		},
		{
			name: "MarkNotDispatched",
			arrange: func(f *clockSkewFixture) string {
				item, _ := f.leased("not-dispatched")
				return item.OutboxID
			},
			write: func(f *clockSkewFixture, outboxID string) error {
				return f.outbox.MarkNotDispatched(
					f.ctx, outboxID, *f.row(outboxID).LeaseToken, "offline", "not_connected", "skewed",
					time.UnixMilli(outboxTestTimeMS).Add(time.Minute),
				)
			},
		},
		{
			name: "ReleaseUnavailable",
			arrange: func(f *clockSkewFixture) string {
				item, _ := f.leased("release")
				return item.OutboxID
			},
			write: func(f *clockSkewFixture, outboxID string) error {
				return f.outbox.ReleaseUnavailable(f.ctx, outboxID, *f.row(outboxID).LeaseToken)
			},
		},
		{
			name: "Reject",
			arrange: func(f *clockSkewFixture) string {
				item, _ := f.leased("reject")
				return item.OutboxID
			},
			write: func(f *clockSkewFixture, outboxID string) error {
				return f.outbox.Reject(f.ctx, outboxID, *f.row(outboxID).LeaseToken, "unsupported", "send_text", "skewed")
			},
		},
		{
			name: "MarkUncertain",
			arrange: func(f *clockSkewFixture) string {
				item, _ := f.called("uncertain")
				return item.OutboxID
			},
			write: func(f *clockSkewFixture, outboxID string) error {
				return f.outbox.MarkUncertain(f.ctx, outboxID, *f.row(outboxID).LeaseToken, "transient", "timeout", "skewed")
			},
		},
		{
			name: "MarkCalledNotDispatched",
			arrange: func(f *clockSkewFixture) string {
				item, _ := f.called("called-not-dispatched")
				return item.OutboxID
			},
			write: func(f *clockSkewFixture, outboxID string) error {
				return f.outbox.MarkCalledNotDispatched(
					f.ctx, outboxID, *f.row(outboxID).LeaseToken, "transient", "send_text", "skewed",
					time.UnixMilli(outboxTestTimeMS).Add(time.Minute),
				)
			},
		},
		{
			name: "ConfirmWithoutResult",
			arrange: func(f *clockSkewFixture) string {
				item, _ := f.called("confirm-without-result")
				return item.OutboxID
			},
			write: func(f *clockSkewFixture, outboxID string) error {
				return f.outbox.ConfirmWithoutResult(f.ctx, outboxID, *f.row(outboxID).LeaseToken)
			},
		},
		{
			// Confirm also repoints the optimistic message, a messages write.
			name: "Confirm",
			arrange: func(f *clockSkewFixture) string {
				item, _ := f.called("confirm")
				return item.OutboxID
			},
			write: func(f *clockSkewFixture, outboxID string) error {
				return f.outbox.Confirm(f.ctx, Confirmation{
					OutboxID: outboxID, LeaseToken: *f.row(outboxID).LeaseToken, ResultRemoteID: "remote-real-confirm",
				})
			},
		},
		{
			name: "MarkStoreFailed",
			arrange: func(f *clockSkewFixture) string {
				item, _ := f.called("store-failed")
				return item.OutboxID
			},
			write: func(f *clockSkewFixture, outboxID string) error {
				return f.outbox.MarkStoreFailed(f.ctx, outboxID, *f.row(outboxID).LeaseToken, "remote-store-failed", "skewed")
			},
		},
		{
			name: "ReconcileConfirm uncertain",
			arrange: func(f *clockSkewFixture) string {
				item, token := f.called("reconcile-uncertain")
				if err := f.outbox.MarkUncertain(f.ctx, item.OutboxID, token, "transient", "timeout", "arrange"); err != nil {
					f.t.Fatalf("MarkUncertain(): %v", err)
				}
				return item.OutboxID
			},
			write: func(f *clockSkewFixture, outboxID string) error {
				row := f.row(outboxID)
				_, err := f.outbox.ReconcileConfirm(f.ctx, ReconcileRequest{
					AccountID: row.AccountID, TransportRequestID: row.TransportRequestID, ResultRemoteID: "remote-real-uncertain",
				})
				return err
			},
		},
		{
			name: "ReconcileConfirm enrich",
			arrange: func(f *clockSkewFixture) string {
				item, token := f.called("reconcile-enrich")
				if err := f.outbox.Confirm(f.ctx, Confirmation{
					OutboxID: item.OutboxID, LeaseToken: token, ResultRemoteID: item.TransportRequestID,
				}); err != nil {
					f.t.Fatalf("Confirm(placeholder): %v", err)
				}
				return item.OutboxID
			},
			write: func(f *clockSkewFixture, outboxID string) error {
				row := f.row(outboxID)
				outcome, err := f.outbox.ReconcileConfirm(f.ctx, ReconcileRequest{
					AccountID: row.AccountID, TransportRequestID: row.TransportRequestID, ResultRemoteID: "remote-real-enrich",
				})
				if err == nil && outcome != ReconcileOutcomeEnriched {
					f.t.Fatalf("ReconcileConfirm() outcome = %q, want enriched", outcome)
				}
				return err
			},
		},
		{
			name: "RetryNotDispatched",
			arrange: func(f *clockSkewFixture) string {
				item, token := f.leased("retry")
				if err := f.outbox.MarkNotDispatched(
					f.ctx, item.OutboxID, token, "offline", "not_connected", "arrange",
					time.UnixMilli(outboxTestTimeMS).Add(time.Hour),
				); err != nil {
					f.t.Fatalf("MarkNotDispatched(): %v", err)
				}
				return item.OutboxID
			},
			write: func(f *clockSkewFixture, outboxID string) error {
				return f.outbox.RetryNotDispatched(f.ctx, outboxID)
			},
		},
		{
			name: "Cancel",
			arrange: func(f *clockSkewFixture) string {
				return mustEnqueueOutgoingOutbox(f.t, f.outbox, outboxTestItem("cancel"), "body").OutboxID
			},
			write: func(f *clockSkewFixture, outboxID string) error {
				return f.outbox.Cancel(f.ctx, outboxID)
			},
		},
		{
			// The held reaction is the row the wake changes.
			name: "WakeReactionsHeldFor",
			arrange: func(f *clockSkewFixture) string {
				send := outboxTestItem("held-send")
				send.ScheduledFor = time.UnixMilli(outboxTestTimeMS).Add(time.Hour)
				mustEnqueueOutgoingOutbox(f.t, f.outbox, send, "body")
				reaction := outboxTestReactionItem("held-reaction")
				if _, _, err := f.outbox.EnqueueReaction(f.ctx, reaction, OutboxReaction{
					TargetMessageID: send.LocalMessageID, Emoji: "👍", Action: string(bridge.ReactionAdd),
				}); err != nil {
					f.t.Fatalf("EnqueueReaction(): %v", err)
				}
				lease := mustLeaseOne(f.t, f.outbox, LeaseRequest{
					Owner: "dispatcher", Now: f.clock.Now(), Duration: time.Minute, Limit: 8,
				})
				if err := f.outbox.MarkNotDispatched(
					f.ctx, lease.OutboxID, mustLeaseToken(f.t, lease), "transient", ReactionTargetUnsentCode, "arrange",
					time.UnixMilli(outboxTestTimeMS).Add(2*time.Hour),
				); err != nil {
					f.t.Fatalf("MarkNotDispatched(held reaction): %v", err)
				}
				return reaction.OutboxID
			},
			write: func(f *clockSkewFixture, outboxID string) error {
				woken, err := f.outbox.WakeReactionsHeldFor(f.ctx, "outbox-held-send")
				if err == nil && woken != 1 {
					f.t.Fatalf("WakeReactionsHeldFor() woke %d, want 1", woken)
				}
				return err
			},
		},
		{
			name: "RecoverExpiredLeases before the call",
			arrange: func(f *clockSkewFixture) string {
				item := outboxTestItem("recover-pre-call")
				item.ScheduledFor = before.Add(-time.Minute)
				return mustEnqueueOutgoingOutbox(f.t, f.outbox, item, "body").OutboxID
			},
			write: func(f *clockSkewFixture, outboxID string) error {
				if _, err := f.outbox.LeaseDue(f.ctx, LeaseRequest{
					Owner: "dispatcher", Now: before.Add(-time.Minute), Duration: time.Second, Limit: 1,
				}); err != nil {
					return err
				}
				recovered, err := f.outbox.RecoverExpiredLeases(f.ctx, f.clock.Now())
				if err == nil && recovered.NotDispatched != 1 {
					f.t.Fatalf("RecoverExpiredLeases() = %+v, want one not-dispatched row", recovered)
				}
				return err
			},
		},
		{
			name: "RecoverExpiredLeases after the call",
			arrange: func(f *clockSkewFixture) string {
				item := outboxTestItem("recover-post-call")
				item.ScheduledFor = before.Add(-time.Minute)
				return mustEnqueueOutgoingOutbox(f.t, f.outbox, item, "body").OutboxID
			},
			write: func(f *clockSkewFixture, outboxID string) error {
				writeAt := f.clock.Now()
				leaseAt := before.Add(-time.Minute)
				leases, err := f.outbox.LeaseDue(f.ctx, LeaseRequest{
					Owner: "dispatcher", Now: leaseAt, Duration: time.Second, Limit: 1,
				})
				if err != nil {
					return err
				}
				f.clock.Set(leaseAt.UnixMilli())
				if err := f.outbox.MarkTransportCalled(f.ctx, Attempt{
					OutboxID: outboxID, LeaseToken: *leases[0].LeaseToken, StartedAt: leaseAt,
				}); err != nil {
					return err
				}
				f.clock.Set(writeAt.UnixMilli())
				recovered, err := f.outbox.RecoverExpiredLeases(f.ctx, f.clock.Now())
				if err == nil && recovered.Uncertain != 1 {
					f.t.Fatalf("RecoverExpiredLeases() = %+v, want one uncertain row", recovered)
				}
				return err
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, writeTime := range clockSkews {
				t.Run(writeTime.name, func(t *testing.T) {
					f := newClockSkewFixture(t)
					outboxID := tc.arrange(f)
					rows := writeTime.prepare(t, f.store)
					was := f.row(outboxID)

					f.clock.Set(time.UnixMilli(outboxTestTimeMS).Add(writeTime.offset).UnixMilli())
					if err := tc.write(f, outboxID); err != nil {
						t.Fatalf("%s on a %s: %v", tc.name, writeTime.name, err)
					}

					got := f.row(outboxID)
					if got.UpdatedAtMS < was.UpdatedAtMS || got.UpdatedAtMS < got.CreatedAtMS {
						t.Fatalf(
							"updated_at_ms went from %d to %d (created_at_ms %d); want it never to fall",
							was.UpdatedAtMS, got.UpdatedAtMS, got.CreatedAtMS,
						)
					}
					unchanged := got
					unchanged.UpdatedAtMS = was.UpdatedAtMS
					if reflect.DeepEqual(unchanged, was) {
						t.Fatalf("%s left the outbox row as it was, so this case checks nothing", tc.name)
					}
					assertNoUpdatedAtFell(t, f.store, rows)
					assertNoRowUpdatedBeforeCreated(t, f.store)
				})
			}
		})
	}
}

// TestOutboxConfirmReactionOverAnOlderOwnReactionOnAnEarlyClock confirms a
// reaction whose own read-model row already exists, on a clock that trails
// that row, once for each of clockSkews. The confirm writes the reaction
// row's updated_at_ms from its clock read, so it must not move it backward or
// below the row's created_at_ms.
func TestOutboxConfirmReactionOverAnOlderOwnReactionOnAnEarlyClock(t *testing.T) {
	for _, writeTime := range clockSkews {
		t.Run(writeTime.name, func(t *testing.T) {
			f := newConfirmReactionFixture(t)
			ctx := context.Background()
			if _, err := f.reactions.ApplyReaction(ctx, ReactionApply{
				AccountID: "account-a", ConversationID: "conversation-a", MessageID: f.targetID,
				ReactorKey: SelfReactorKey, ReactorIsSelf: true, ReactorLabel: SelfReactorLabel,
				Emoji: "👍", Action: bridge.ReactionAdd,
				OccurredAtMS: confirmReactionTimeMS - 500, SourceSeqMS: confirmReactionTimeMS - 500,
			}); err != nil {
				t.Fatalf("ApplyReaction(earlier own reaction): %v", err)
			}
			outboxID, leaseToken := f.called("🎉", bridge.ReactionAdd)
			rows := writeTime.prepare(t, f.store)

			f.clock.Set(confirmReactionTimeMS + writeTime.offset.Milliseconds())
			applied, err := f.outbox.ConfirmReaction(ctx, ReactionConfirmation{
				OutboxID: outboxID, LeaseToken: leaseToken, OccurredAt: time.UnixMilli(confirmReactionTimeMS - 100),
			})
			if err != nil || !applied {
				t.Fatalf("ConfirmReaction() on a %s = %v, %v; want applied, nil", writeTime.name, applied, err)
			}
			if state := f.state(outboxID); state != OutboxConfirmed {
				t.Fatalf("state = %q, want confirmed", state)
			}
			if got := activeReactions(t, f.store, f.targetID); len(got) != 1 || got[0] != SelfReactorKey+" 🎉" {
				t.Fatalf("active reactions = %v, want the confirmed 🎉 alone", got)
			}
			assertNoUpdatedAtFell(t, f.store, rows)
			assertNoRowUpdatedBeforeCreated(t, f.store)
		})
	}
}

// clampTables are the tables whose CHECK requires updated_at_ms >=
// created_at_ms.
var clampTables = []string{
	"accounts", "devices", "identities", "people", "conversations",
	"messages", "outbox", "message_attachments", "reactions",
}

// clockSkew is when a write under test reads its clock, relative to the rows
// it changes.
type clockSkew struct {
	name string
	// offset is the write's time minus the changed rows' created_at_ms.
	offset time.Duration
	// lastUpdate, when positive, moves every row's updated_at_ms to
	// created_at_ms+lastUpdate before the write, as a later write would leave
	// it.
	lastUpdate time.Duration
}

// clockSkews are the two ways a write's time can trail the row it changes.
// Before creation, a plain write fails the CHECK. After creation but before
// the last update, a write clamped only to created_at_ms
// (MAX(created_at_ms, ?)) satisfies the CHECK and still moves updated_at_ms
// backward.
var clockSkews = []clockSkew{
	{name: "clock before the row was created", offset: -time.Second},
	{name: "clock after creation, before the last update", offset: time.Second, lastUpdate: 2 * time.Second},
}

// prepare gives store's rows their last update, if the skew has one, and
// returns each row's updated_at_ms for assertNoUpdatedAtFell.
func (s clockSkew) prepare(t *testing.T, store *Store) map[string]int64 {
	t.Helper()
	if s.lastUpdate > 0 {
		for _, table := range clampTables {
			if _, err := store.db.Exec(
				`UPDATE `+table+` SET updated_at_ms = MAX(updated_at_ms, created_at_ms + ?)`,
				s.lastUpdate.Milliseconds(),
			); err != nil {
				t.Fatalf("move %s updated_at_ms past created_at_ms: %v", table, err)
			}
		}
	}
	return updatedAtByRow(t, store)
}

// updatedAtByRow maps each row of clampTables, named by its table, rowid and
// created_at_ms, to its updated_at_ms.
func updatedAtByRow(t *testing.T, store *Store) map[string]int64 {
	t.Helper()
	byRow := make(map[string]int64)
	for _, table := range clampTables {
		rows, err := store.db.Query(`SELECT rowid, created_at_ms, updated_at_ms FROM ` + table)
		if err != nil {
			t.Fatalf("read %s updated_at_ms: %v", table, err)
		}
		for rows.Next() {
			var rowID, createdAtMS, updatedAtMS int64
			if err := rows.Scan(&rowID, &createdAtMS, &updatedAtMS); err != nil {
				rows.Close()
				t.Fatalf("scan %s updated_at_ms: %v", table, err)
			}
			byRow[fmt.Sprintf("%s row %d created at %d", table, rowID, createdAtMS)] = updatedAtMS
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			t.Fatalf("read %s updated_at_ms: %v", table, err)
		}
	}
	return byRow
}

// assertNoUpdatedAtFell checks that no row in before, from updatedAtByRow,
// now has a lower updated_at_ms. It skips rows deleted since.
func assertNoUpdatedAtFell(t *testing.T, store *Store, before map[string]int64) {
	t.Helper()
	for row, updatedAtMS := range updatedAtByRow(t, store) {
		if was, ok := before[row]; ok && updatedAtMS < was {
			t.Fatalf("%s: updated_at_ms fell from %d to %d", row, was, updatedAtMS)
		}
	}
}

// assertNoRowUpdatedBeforeCreated checks, across every table that carries
// the updated_at_ms >= created_at_ms CHECK, what the CHECK itself enforces on
// each write. It catches a store opened without the CHECK (an older schema)
// and documents which tables the invariant covers.
func assertNoRowUpdatedBeforeCreated(t *testing.T, store *Store) {
	t.Helper()
	for _, table := range clampTables {
		var count int
		if err := store.db.QueryRow(
			`SELECT COUNT(*) FROM ` + table + ` WHERE updated_at_ms < created_at_ms`,
		).Scan(&count); err != nil {
			t.Fatalf("count %s rows updated before they were created: %v", table, err)
		}
		if count != 0 {
			t.Fatalf("%d %s rows have updated_at_ms below created_at_ms", count, table)
		}
	}
}
