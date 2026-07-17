package ingest

import (
	"sync"
	"sync/atomic"
)

// CounterSnapshot is the externally consumable, point-in-time view of one
// account's ingest activity. Echo outcomes mirror messaging.EchoOutcome;
// EchoErrors counts reconcile faults that were absorbed without blocking
// projection of the inbound message.
type CounterSnapshot struct {
	Appended          uint64 `json:"appended"`
	Deduped           uint64 `json:"deduped"`
	DecodedEvents     uint64 `json:"decoded_events"`
	Projected         uint64 `json:"projected"`
	Imported          uint64 `json:"imported"`
	Mutations         uint64 `json:"mutations"`
	ReactionsDropped  uint64 `json:"reactions_dropped"`
	TapbackMessages   uint64 `json:"tapback_messages"`
	EmptyStubsSkipped uint64 `json:"empty_stubs_skipped"`
	ReceiptsSelf      uint64 `json:"receipts_self"`
	ReceiptsDropped   uint64 `json:"receipts_dropped"`
	AppendErrors      uint64 `json:"append_errors"`
	Quarantined       uint64 `json:"quarantined"`
	StaleReplays      uint64 `json:"stale_replays"`
	EchoReconciled    uint64 `json:"echo_reconciled"`
	EchoEnriched      uint64 `json:"echo_enriched"`
	EchoNoop          uint64 `json:"echo_noop"`
	EchoNotFound      uint64 `json:"echo_notfound"`
	EchoErrors        uint64 `json:"echo_errors"`
	Ephemeral         uint64 `json:"ephemeral"`
}

type accountCounters struct {
	appended          atomic.Uint64
	deduped           atomic.Uint64
	decodedEvents     atomic.Uint64
	projected         atomic.Uint64
	imported          atomic.Uint64
	mutations         atomic.Uint64
	reactionsDropped  atomic.Uint64
	tapbackMessages   atomic.Uint64
	emptyStubsSkipped atomic.Uint64
	receiptsSelf      atomic.Uint64
	receiptsDropped   atomic.Uint64
	appendErrors      atomic.Uint64
	quarantined       atomic.Uint64
	staleReplays      atomic.Uint64
	echoReconciled    atomic.Uint64
	echoEnriched      atomic.Uint64
	echoNoop          atomic.Uint64
	echoNotFound      atomic.Uint64
	echoErrors        atomic.Uint64
	ephemeral         atomic.Uint64
}

// Counters owns atomic ingest counters partitioned by account. Its zero value
// is ready for use.
type Counters struct {
	mu       sync.RWMutex
	accounts map[string]*accountCounters
}

func (c *Counters) account(accountID string) *accountCounters {
	c.mu.RLock()
	counters := c.accounts[accountID]
	c.mu.RUnlock()
	if counters != nil {
		return counters
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.accounts == nil {
		c.accounts = make(map[string]*accountCounters)
	}
	if counters = c.accounts[accountID]; counters == nil {
		counters = &accountCounters{}
		c.accounts[accountID] = counters
	}
	return counters
}

// Snapshot returns one account's counters without creating an entry for an
// account that has not observed ingest activity.
func (c *Counters) Snapshot(accountID string) CounterSnapshot {
	if c == nil {
		return CounterSnapshot{}
	}
	c.mu.RLock()
	counters := c.accounts[accountID]
	c.mu.RUnlock()
	return snapshotCounters(counters)
}

// PerAccount returns independent snapshots for every observed account.
func (c *Counters) PerAccount() map[string]CounterSnapshot {
	result := make(map[string]CounterSnapshot)
	if c == nil {
		return result
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	for accountID, counters := range c.accounts {
		result[accountID] = snapshotCounters(counters)
	}
	return result
}

func snapshotCounters(c *accountCounters) CounterSnapshot {
	if c == nil {
		return CounterSnapshot{}
	}
	return CounterSnapshot{
		Appended:          c.appended.Load(),
		Deduped:           c.deduped.Load(),
		DecodedEvents:     c.decodedEvents.Load(),
		Projected:         c.projected.Load(),
		Imported:          c.imported.Load(),
		Mutations:         c.mutations.Load(),
		ReactionsDropped:  c.reactionsDropped.Load(),
		TapbackMessages:   c.tapbackMessages.Load(),
		EmptyStubsSkipped: c.emptyStubsSkipped.Load(),
		ReceiptsSelf:      c.receiptsSelf.Load(),
		ReceiptsDropped:   c.receiptsDropped.Load(),
		AppendErrors:     c.appendErrors.Load(),
		Quarantined:       c.quarantined.Load(),
		StaleReplays:      c.staleReplays.Load(),
		EchoReconciled:    c.echoReconciled.Load(),
		EchoEnriched:      c.echoEnriched.Load(),
		EchoNoop:          c.echoNoop.Load(),
		EchoNotFound:      c.echoNotFound.Load(),
		EchoErrors:        c.echoErrors.Load(),
		Ephemeral:         c.ephemeral.Load(),
	}
}
