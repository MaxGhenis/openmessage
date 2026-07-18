package ingest

import (
	"sync"
	"testing"
)

func TestCountersAreAtomicAndAccountScoped(t *testing.T) {
	t.Parallel()

	var counters Counters
	const (
		workers    = 8
		increments = 250
	)
	var wait sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for index := 0; index < increments; index++ {
				account := counters.account("account-a")
				account.appended.Add(1)
				account.decodedEvents.Add(2)
			}
		}()
	}
	wait.Wait()
	accountBInternal := counters.account("account-b")
	accountBInternal.ephemeral.Add(1)
	accountBInternal.tapbackMessages.Add(2)
	accountBInternal.emptyStubsSkipped.Add(3)
	accountBInternal.reactionsApplied.Add(4)
	accountBInternal.reactionsRemoved.Add(5)
	accountBInternal.reactionsOrphaned.Add(6)

	want := uint64(workers * increments)
	accountA := counters.Snapshot("account-a")
	if accountA.Appended != want || accountA.DecodedEvents != 2*want {
		t.Fatalf("account-a snapshot = %+v, want appended=%d decoded_events=%d", accountA, want, 2*want)
	}
	accountB := counters.Snapshot("account-b")
	if accountB.Ephemeral != 1 || accountB.TapbackMessages != 2 || accountB.EmptyStubsSkipped != 3 ||
		accountB.ReactionsApplied != 4 || accountB.ReactionsRemoved != 5 || accountB.ReactionsOrphaned != 6 ||
		accountB.Appended != 0 {
		t.Fatalf("account-b snapshot = %+v, want additive ingest counters", accountB)
	}
	if missing := counters.Snapshot("missing"); missing != (CounterSnapshot{}) {
		t.Fatalf("missing account snapshot = %+v, want zero value", missing)
	}
	perAccount := counters.PerAccount()
	if len(perAccount) != 2 || perAccount["account-a"] != accountA || perAccount["account-b"] != accountB {
		t.Fatalf("PerAccount() = %+v, want independent account snapshots", perAccount)
	}
}
