package cmd

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/maxghenis/openmessage/internal/db"
)

// testReclaimStep is small, so even the smaller stores the race job builds
// hold several steps of free pages.
const testReclaimStep = 16 << 10

// testReclaimer has small thresholds, so a test store exercises several
// steps.
func testReclaimer(store *db.Store, logs *bytes.Buffer) *legacyFreePageReclaimer {
	r := newLegacyFreePageReclaimer(store, zerolog.New(logs))
	r.minFree = 4 * testReclaimStep
	r.step = testReclaimStep
	r.pause = 0
	r.warnFree = 4 * testReclaimStep
	return r
}

func newIncrementalStoreWithFreePages(t *testing.T) *db.Store {
	t.Helper()
	store, err := db.New(filepath.Join(t.TempDir(), legacyDatabaseName))
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	t.Cleanup(func() { store.Close() })
	saveAndThinMessages(t, store, thinnedStoreMessages(), 1500, 10)
	return store
}

func TestLegacyFreePageReclaimerReturnsEveryFreePageAndKeepsEveryRow(t *testing.T) {
	ctx := context.Background()
	store := newIncrementalStoreWithFreePages(t)
	before, err := store.StorageStats(ctx)
	if err != nil {
		t.Fatalf("StorageStats(): %v", err)
	}
	if before.AutoVacuum != db.AutoVacuumIncremental || before.FreeBytes() < 4*testReclaimStep {
		t.Fatalf("setup: %+v, want an incremental store with several steps of free pages", before)
	}
	digest, err := store.ContentDigest(ctx)
	if err != nil {
		t.Fatalf("ContentDigest(): %v", err)
	}

	var logs bytes.Buffer
	freed, err := testReclaimer(store, &logs).pass(ctx)
	if err != nil {
		t.Fatalf("pass(): %v", err)
	}
	after, err := store.StorageStats(ctx)
	if err != nil {
		t.Fatalf("StorageStats(): %v", err)
	}
	if freed != before.FreeBytes() || after.FreePages != 0 {
		t.Errorf("pass freed %d of %d free bytes, %d free pages left", freed, before.FreeBytes(), after.FreePages)
	}
	if after.PageCount > before.PageCount-before.FreePages {
		t.Errorf("pages %d -> %d with %d free; the file should lose at least the free pages", before.PageCount, after.PageCount, before.FreePages)
	}
	if got, err := store.ContentDigest(ctx); err != nil || len(db.ChangedTables(digest, got)) != 0 {
		t.Errorf("tables changed: %v (err %v)", db.ChangedTables(digest, got), err)
	}
	if !strings.Contains(logs.String(), "freed_bytes") {
		t.Errorf("pass did not log what it returned: %s", logs.String())
	}
}

func TestLegacyFreePageReclaimerLeavesASmallFreelist(t *testing.T) {
	ctx := context.Background()
	store := newIncrementalStoreWithFreePages(t)
	before, err := store.StorageStats(ctx)
	if err != nil {
		t.Fatalf("StorageStats(): %v", err)
	}
	var logs bytes.Buffer
	r := testReclaimer(store, &logs)
	r.minFree = before.FreeBytes() + 1
	if freed, err := r.pass(ctx); err != nil || freed != 0 {
		t.Fatalf("pass() = %d, %v; want nothing under the threshold", freed, err)
	}
	if after, err := store.StorageStats(ctx); err != nil || after != before {
		t.Fatalf("stats %+v -> %+v (err %v)", before, after, err)
	}
}

// A store without auto-vacuum can't hand pages back in steps, so the
// reclaimer says once how to compact it and changes nothing.
func TestLegacyFreePageReclaimerSaysOnceToCompactAStoreWithoutAutoVacuum(t *testing.T) {
	ctx := context.Background()
	dataDir, _ := buildBloatedLegacyStore(t)
	store, err := db.New(filepath.Join(dataDir, legacyDatabaseName))
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	defer store.Close()
	before, err := store.StorageStats(ctx)
	if err != nil {
		t.Fatalf("StorageStats(): %v", err)
	}
	var logs bytes.Buffer
	r := testReclaimer(store, &logs)
	if before.FreeBytes() < r.warnFree {
		t.Fatalf("setup: %d free bytes, want at least the %d that draw the warning", before.FreeBytes(), r.warnFree)
	}
	for i := 0; i < 2; i++ {
		if freed, err := r.pass(ctx); err != nil || freed != 0 {
			t.Fatalf("pass %d = %d, %v; want nothing freed", i, freed, err)
		}
	}
	if got := strings.Count(logs.String(), "openmessage compact --apply"); got != 1 {
		t.Errorf("warned %d times, want once:\n%s", got, logs.String())
	}
	if after, err := store.StorageStats(ctx); err != nil || after != before {
		t.Fatalf("stats %+v -> %+v (err %v)", before, after, err)
	}
}

func TestLegacyFreePageReclaimerRunsPassesUntilCancelled(t *testing.T) {
	store := newIncrementalStoreWithFreePages(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var logs bytes.Buffer
	r := testReclaimer(store, &logs)
	go func() {
		r.run(ctx, time.Millisecond, time.Millisecond)
		close(done)
	}()
	deadline := time.Now().Add(30 * time.Second)
	for {
		stats, err := store.StorageStats(context.Background())
		if err != nil {
			t.Fatalf("StorageStats(): %v", err)
		}
		if stats.FreePages == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("free pages still %d after 30 s of passes", stats.FreePages)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("run did not return after its context ended")
	}
}
