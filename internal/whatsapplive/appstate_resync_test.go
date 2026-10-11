package whatsapplive_test

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math/rand"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/proto/waServerSync"
	"go.mau.fi/whatsmeow/store"
	watypes "go.mau.fi/whatsmeow/types"
	waevents "go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"github.com/maxghenis/openmessage/internal/whatsapplive"
)

// These tests corrupt an app-state collection the way generation teardown did
// before the handler context was detached from it (PR #236): the collection's
// new version is stored and the patch's value MACs are not. Every later patch
// touching an affected entry then fails with a mismatching LTHash, and
// whatsmeow dispatches AppStateSyncError but never resyncs on its own. The
// tests check that OpenMessage answers that error with one full resync of the
// collection, which restores the phone's current contact names (whatsmeow's
// contact store, which OpenMessage reads for display names) and lets later
// patches apply.
//
// The only stub is the network: a full resync here resets the stored version
// and applies the phone's current snapshot through whatsmeow's real decoder,
// where production calls whatsmeow's FetchAppState(fullSync), which does the
// same after fetching the snapshot from the server.

// snapshot is the server's snapshot of the phone's collection: the current
// value of every index, with the MAC of the current state.
func (p *appStatePhone) snapshot() *appstate.PatchList {
	p.mu.Lock()
	defer p.mu.Unlock()
	records := make([]*waServerSync.SyncdRecord, 0, len(p.records))
	for _, indexMAC := range slices.Sorted(maps.Keys(p.records)) {
		records = append(records, p.records[indexMAC])
	}
	return &appstate.PatchList{
		Name: appstate.WAPatchCriticalUnblockLow,
		Snapshot: &waServerSync.SyncdSnapshot{
			Version: &waServerSync.SyncdVersion{Version: proto.Uint64(p.state.Version)},
			Records: records,
			Mac:     p.snapshotMAC,
			KeyID:   &waServerSync.KeyId{ID: p.keyID},
		},
	}
}

type appStateResync struct {
	name appstate.WAPatchName
	err  error
}

// serveFullSyncs makes every full resync OpenMessage starts fetch this
// phone's snapshot, and reports each finished resync on the returned channel.
// A full resync is what whatsmeow's FetchAppState(ctx, name, true, false)
// does once the snapshot arrives: reset the stored version, which cascades the
// value MACs, then apply the snapshot with app-state events off
// (EmitAppStateEventsOnFullSync is false), which bulk-writes the contact
// names (PutAllContactNames).
func (p *appStatePhone) serveFullSyncs(t *testing.T) <-chan appStateResync {
	t.Helper()
	done := make(chan appStateResync, 16)
	whatsapplive.StubAppStateFullSyncForTest(t, func(ctx context.Context, cli *whatsmeow.Client, name appstate.WAPatchName) error {
		err := cli.Store.AppState.DeleteAppStateVersion(ctx, string(name))
		if err == nil {
			_, err = cli.DangerousInternals().ApplyAppStatePatches(ctx, name, appstate.HashState{}, p.snapshot(), true, nil)
		}
		done <- appStateResync{name: name, err: err}
		return err
	})
	return done
}

// fetchAppState applies one fetched patch list the way whatsmeow's
// fetchAppState does for a notification (appstate.go): decode it against the
// stored state with applyAppStatePatches, which dispatches AppStateSyncError
// to the bridge's event handler when the patches fail to verify, then
// dispatch the collected events.
func (h *teardownHarness) fetchAppState(ctx context.Context, list *appstate.PatchList) error {
	version, hash, err := h.cli.Store.AppState.GetAppStateVersion(ctx, string(list.Name))
	if err != nil {
		return err
	}
	var events []any
	if _, err := h.cli.DangerousInternals().ApplyAppStatePatches(ctx, list.Name, appstate.HashState{Version: version, Hash: hash}, list, false, &events); err != nil {
		return err
	}
	for _, evt := range events {
		h.cli.DangerousInternals().DispatchEvent(evt)
	}
	return nil
}

// appStateInterrupter wraps the app-state store for the whole test (so a
// resync running concurrently never sees the store swapped) and, when armed,
// cancels a context right after whatsmeow stores a collection's new version.
type appStateInterrupter struct {
	store.AppStateStore
	armed atomic.Pointer[context.CancelFunc]
}

func (i *appStateInterrupter) PutAppStateVersion(ctx context.Context, name string, version uint64, hash [128]byte) error {
	err := i.AppStateStore.PutAppStateVersion(ctx, name, version, hash)
	if cancel := i.armed.Swap(nil); err == nil && cancel != nil {
		(*cancel)()
	}
	return err
}

func (h *teardownHarness) installAppStateInterrupter() *appStateInterrupter {
	interrupter := &appStateInterrupter{AppStateStore: h.cli.Store.AppState}
	h.cli.Store.AppState = interrupter
	return interrupter
}

// interruptAppStateSync applies one patch list with a handler context that
// ends right after whatsmeow stores the collection's new version, so the
// value-MAC writes that follow are cancelled: what generation teardown did
// before the handler context was detached from it. The patch fails either
// way: on the cancelled MAC writes, or earlier on a mismatching LTHash when
// the collection is already stuck.
func (h *teardownHarness) interruptAppStateSync(interrupter *appStateInterrupter, ctx context.Context, list *appstate.PatchList) error {
	h.t.Helper()
	interrupted, cancel := context.WithCancel(ctx)
	defer cancel()
	interrupter.armed.Store(&cancel)
	err := h.fetchAppState(interrupted, list)
	interrupter.armed.Store(nil)
	if err == nil {
		h.t.Fatal("the interrupted patch applied; the test no longer corrupts the collection")
	}
	return err
}

func (h *teardownHarness) requireContactName(contact watypes.JID, want, why string) {
	h.t.Helper()
	info, err := h.cli.Store.Contacts.GetContact(context.Background(), contact)
	if err != nil {
		h.t.Fatalf("GetContact(%s): %v", contact, err)
	}
	if info.FullName != want {
		h.t.Fatalf("contact store name of %s = %q, want %q (%s)", contact, info.FullName, want, why)
	}
	if got := whatsapplive.ContactDisplayNameForTest(h.host, contact); got != want {
		h.t.Fatalf("OpenMessage display name of %s = %q, want %q (%s)", contact, got, want, why)
	}
}

func awaitResync(t *testing.T, resyncs <-chan appStateResync, why string) appStateResync {
	t.Helper()
	select {
	case resync := <-resyncs:
		return resync
	case <-time.After(teardownTestTimeout):
		t.Fatalf("OpenMessage never resynced the collection: %s", why)
		return appStateResync{}
	}
}

// TestStuckContactListRecoversAfterAppStateSyncError corrupts the contact
// list the way the old teardown did and checks that OpenMessage recovers: the
// next patch touching the corrupted contact fails to verify, whatsmeow
// dispatches AppStateSyncError, OpenMessage resyncs the collection, the
// contact store shows the phone's current name, and the patch after that
// applies.
func TestStuckContactListRecoversAfterAppStateSyncError(t *testing.T) {
	h := newTeardownHarness(t, false)
	ctx, _ := h.awaitGeneration()
	interrupter := h.installAppStateInterrupter()
	phone := h.newAppStatePhone()
	resyncs := phone.serveFullSyncs(t)
	contact := watypes.JID{User: "15550001111", Server: watypes.DefaultUserServer}

	if err := h.fetchAppState(ctx, phone.renameContact(t, contact, "Alice Phone")); err != nil {
		t.Fatalf("first contact-list patch: %v", err)
	}
	h.requireContactName(contact, "Alice Phone", "first patch")
	interruptedErr := h.interruptAppStateSync(interrupter, ctx, phone.renameContact(t, contact, "Alice Moved"))
	if errors.Is(interruptedErr, appstate.ErrMismatchingLTHash) {
		t.Fatalf("the interrupted patch failed to verify instead of being interrupted: %v", interruptedErr)
	}

	stuckErr := h.fetchAppState(ctx, phone.renameContact(t, contact, "Alice Renamed"))
	if !errors.Is(stuckErr, appstate.ErrMismatchingLTHash) {
		t.Fatalf("patch after the interrupted one = %v, want a mismatching LTHash (interrupted sync: %v)", stuckErr, interruptedErr)
	}
	resync := awaitResync(t, resyncs, "after an AppStateSyncError with a mismatching LTHash")
	if resync.name != appstate.WAPatchCriticalUnblockLow || resync.err != nil {
		t.Fatalf("resync = %s, %v; want %s without error", resync.name, resync.err, appstate.WAPatchCriticalUnblockLow)
	}
	h.requireContactName(contact, "Alice Renamed", "the full resync restores the phone's current name")

	if err := h.fetchAppState(ctx, phone.renameContact(t, contact, "Alice Latest")); err != nil {
		t.Fatalf("the contact-list patch after the resync failed: %v", err)
	}
	h.requireContactName(contact, "Alice Latest", "the patch after the resync")
}

// resyncClock is the clock the resync budget reads, advanced by the test.
type resyncClock struct{ now atomic.Int64 }

func newResyncClock(t *testing.T) *resyncClock {
	clock := &resyncClock{}
	clock.now.Store(time.Unix(1_760_000_000, 0).UnixNano())
	whatsapplive.SetAppStateResyncClockForTest(t, func() time.Time { return time.Unix(0, clock.now.Load()) })
	return clock
}

func (c *resyncClock) advance(d time.Duration) { c.now.Add(int64(d)) }

func mismatchingLTHash(name appstate.WAPatchName) *waevents.AppStateSyncError {
	return &waevents.AppStateSyncError{
		Name:  name,
		Error: fmt.Errorf("failed to decode app state %s patches: %w", name, fmt.Errorf("failed to verify patch v7: %w", appstate.ErrMismatchingLTHash)),
	}
}

func (h *teardownHarness) awaitNoResyncInFlight() {
	h.t.Helper()
	if !awaitTeardownCondition(h.t, "the app state resyncs to finish", func() bool {
		return whatsapplive.AppStateResyncsInFlightForTest(h.host) == 0
	}) {
		h.t.FailNow()
	}
}

// requireResyncs checks which resyncs started since the last check. The
// bridge decides synchronously, inside the dispatch, whether to start one, so
// once DispatchEvent has returned, a started resync's call is already on its
// way; the short wait only covers the goroutine being scheduled.
func requireResyncs(t *testing.T, calls <-chan appstate.WAPatchName, want ...appstate.WAPatchName) {
	t.Helper()
	var got []appstate.WAPatchName
	for range want {
		select {
		case name := <-calls:
			got = append(got, name)
		case <-time.After(teardownTestTimeout):
			t.Fatalf("resyncs started = %v, want %v", got, want)
		}
	}
	select {
	case name := <-calls:
		t.Fatalf("resyncs started = %v, then %s; want only %v", got, name, want)
	case <-time.After(50 * time.Millisecond):
	}
	slices.Sort(got)
	sorted := slices.Clone(want)
	slices.Sort(sorted)
	if !slices.Equal(got, sorted) {
		t.Fatalf("resyncs started = %v, want %v", got, want)
	}
}

// TestAppStateResyncRunsOffTheEventGoroutineOncePerCollection dispatches
// AppStateSyncError through whatsmeow's event dispatch to the bridge and
// checks the resync invariants end to end: the handler returns while the
// resync runs (whatsmeow dispatches the error while holding the app-state
// lock a resync takes, so a resync on the event goroutine would deadlock);
// one resync per collection is in flight; and starts honor the cooldown and
// the cap per window.
func TestAppStateResyncRunsOffTheEventGoroutineOncePerCollection(t *testing.T) {
	h := newTeardownHarness(t, false)
	h.awaitGeneration()
	clock := newResyncClock(t)
	whatsapplive.SetAppStateResyncLimitsForTest(t, time.Minute, 2, time.Hour)
	release := make(chan struct{})
	calls := make(chan appstate.WAPatchName, 16)
	whatsapplive.StubAppStateFullSyncForTest(t, func(ctx context.Context, _ *whatsmeow.Client, name appstate.WAPatchName) error {
		calls <- name
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	dispatch := func(name appstate.WAPatchName) {
		returned := make(chan struct{})
		go func() {
			h.cli.DangerousInternals().DispatchEvent(mismatchingLTHash(name))
			close(returned)
		}()
		select {
		case <-returned:
		case <-time.After(teardownTestTimeout):
			t.Fatalf("dispatching AppStateSyncError for %s blocked the event goroutine", name)
		}
	}
	contacts, regular := appstate.WAPatchCriticalUnblockLow, appstate.WAPatchRegularHigh

	dispatch(contacts) // returns while the resync is held (t0)
	requireResyncs(t, calls, contacts)
	clock.advance(time.Minute)
	dispatch(contacts)
	requireResyncs(t, calls) // past the cooldown, but one is in flight
	dispatch(regular)
	requireResyncs(t, calls, regular) // collections are independent
	if inFlight := whatsapplive.AppStateResyncsInFlightForTest(h.host); inFlight != 2 {
		t.Fatalf("resyncs in flight = %d, want 2", inFlight)
	}
	close(release)
	h.awaitNoResyncInFlight()

	dispatch(contacts)
	requireResyncs(t, calls, contacts) // second start of the window (t0+1m)
	h.awaitNoResyncInFlight()
	clock.advance(30 * time.Second)
	dispatch(contacts)
	requireResyncs(t, calls) // cooling down
	clock.advance(30 * time.Second)
	dispatch(contacts)
	requireResyncs(t, calls) // capped: two starts in the last hour
	clock.advance(time.Hour - 2*time.Minute - time.Nanosecond)
	dispatch(contacts)
	requireResyncs(t, calls) // still capped a nanosecond before t0 leaves the window
	clock.advance(time.Nanosecond)
	dispatch(contacts)
	requireResyncs(t, calls, contacts) // t0 left the window
	h.awaitNoResyncInFlight()
}

// TestAppStateResyncOnlyAnswersStateMismatches dispatches the decode errors
// whatsmeow can report and checks that only a disagreeing stored state starts
// a resync.
func TestAppStateResyncOnlyAnswersStateMismatches(t *testing.T) {
	h := newTeardownHarness(t, false)
	h.awaitGeneration()
	newResyncClock(t)
	calls := make(chan appstate.WAPatchName, 16)
	whatsapplive.StubAppStateFullSyncForTest(t, func(_ context.Context, _ *whatsmeow.Client, name appstate.WAPatchName) error {
		calls <- name
		return nil
	})
	verify := func(err error) error { return fmt.Errorf("failed to verify patch v4: %w", err) }
	for _, test := range []struct {
		name   appstate.WAPatchName
		err    error
		resync bool
	}{
		{appstate.WAPatchCriticalUnblockLow, verify(appstate.ErrMismatchingLTHash), true},
		{appstate.WAPatchCriticalBlock, fmt.Errorf("failed to verify snapshot: %w", verify(appstate.ErrMismatchingLTHash)), true},
		{appstate.WAPatchRegular, fmt.Errorf("failed to update state hash: %w", appstate.ErrMissingPreviousSetValueOperation), true},
		{appstate.WAPatchRegularHigh, verify(appstate.ErrMismatchingPatchMAC), false},
		{appstate.WAPatchRegularHigh, fmt.Errorf("failed to verify mutation #1: %w", appstate.ErrMismatchingContentMAC), false},
		{appstate.WAPatchRegularHigh, fmt.Errorf("failed to verify mutation #1: %w", appstate.ErrMismatchingIndexMAC), false},
		{appstate.WAPatchRegularLow, fmt.Errorf("failed to get key: %w", appstate.ErrKeyNotFound), false},
		{appstate.WAPatchRegularLow, context.Canceled, false},
	} {
		h.cli.DangerousInternals().DispatchEvent(&waevents.AppStateSyncError{Name: test.name, Error: test.err})
		if test.resync {
			requireResyncs(t, calls, test.name)
		} else {
			requireResyncs(t, calls)
		}
	}
	h.awaitNoResyncInFlight()
}

// TestFullResyncRestoresTheCurrentContactNames is a property test over random
// contact-list histories. The phone renames random contacts; a random quarter
// of the patches are interrupted between the version and MAC writes. Every
// patch that fails to verify must touch a contact an interruption corrupted,
// and must be answered by a resync after which the contact store holds the
// phone's current name for every contact, and the next patch applies.
func TestFullResyncRestoresTheCurrentContactNames(t *testing.T) {
	for seed := int64(1); seed <= 4; seed++ {
		t.Run(fmt.Sprintf("seed %d", seed), func(t *testing.T) { testFullResyncRestoresTheCurrentContactNames(t, seed) })
	}
}

func testFullResyncRestoresTheCurrentContactNames(t *testing.T, seed int64) {
	rng := rand.New(rand.NewSource(seed))
	h := newTeardownHarness(t, false)
	ctx, _ := h.awaitGeneration()
	clock := newResyncClock(t)
	whatsapplive.SetAppStateResyncLimitsForTest(t, time.Minute, 1000, time.Hour)
	interrupter := h.installAppStateInterrupter()
	phone := h.newAppStatePhone()
	resyncs := phone.serveFullSyncs(t)
	contacts := make([]watypes.JID, 4)
	for i := range contacts {
		contacts[i] = watypes.JID{User: fmt.Sprintf("1555000%04d", i), Server: watypes.DefaultUserServer}
	}
	current := make(map[watypes.JID]string) // the phone's names
	corrupted := make(map[watypes.JID]bool)
	resynced := 0
	requireAllCurrent := func(why string) {
		t.Helper()
		for _, contact := range slices.SortedFunc(maps.Keys(current), func(a, b watypes.JID) int { return strings.Compare(a.User, b.User) }) {
			h.requireContactName(contact, current[contact], why)
		}
	}
	step := func(contact watypes.JID, name string, interrupt bool) {
		t.Helper()
		list := phone.renameContact(t, contact, name)
		current[contact] = name
		var err error
		if interrupt {
			err = h.interruptAppStateSync(interrupter, ctx, list)
		} else {
			err = h.fetchAppState(ctx, list)
		}
		switch {
		case errors.Is(err, appstate.ErrMismatchingLTHash):
			if !corrupted[contact] {
				t.Fatalf("patch renaming %s to %q failed to verify, but no interruption corrupted it: %v", contact, name, err)
			}
			resync := awaitResync(t, resyncs, fmt.Sprintf("after renaming %s to %q failed to verify", contact, name))
			if resync.err != nil {
				t.Fatalf("resync: %v", resync.err)
			}
			h.awaitNoResyncInFlight()
			resynced++
			clear(corrupted)
			requireAllCurrent(fmt.Sprintf("after the resync answering the rename of %s to %q", contact, name))
		case err != nil:
			if !interrupt {
				t.Fatalf("patch renaming %s to %q: %v", contact, name, err)
			}
			corrupted[contact] = true
		default:
			h.requireContactName(contact, name, "the patch applied")
		}
		clock.advance(time.Minute)
	}
	for i := 0; i < 30; i++ {
		contact := contacts[rng.Intn(len(contacts))]
		step(contact, fmt.Sprintf("%s v%d", contact.User, i), rng.Intn(4) == 0)
	}
	// Touch every contact still corrupted, so its next patch fails and is
	// resynced.
	for _, contact := range contacts {
		if corrupted[contact] {
			step(contact, contact.User+" final", false)
		}
	}
	if len(corrupted) != 0 {
		t.Fatalf("contacts still corrupted after touching them: %v", corrupted)
	}
	requireAllCurrent("end of the history")
	select {
	case resync := <-resyncs:
		t.Fatalf("an extra resync of %s ran", resync.name)
	default:
	}
	t.Logf("seed %d: %d resyncs", seed, resynced)
}

// TestProductionFullSyncResetsTheStoredVersion runs the full resync
// production uses on a client with no socket. whatsmeow's FetchAppState with
// fullSync deletes the stored version, which cascades its value MACs, before
// it asks the server for the snapshot; an incremental fetch never does. So
// the deleted version shows production passes fullSync, and that a resync cut
// off before the snapshot arrives leaves version 0, which whatsmeow's next
// fetch of the collection turns into a full sync.
func TestProductionFullSyncResetsTheStoredVersion(t *testing.T) {
	h := newTeardownHarness(t, false)
	ctx, _ := h.awaitGeneration()
	phone := h.newAppStatePhone()
	contact := watypes.JID{User: "15550001111", Server: watypes.DefaultUserServer}
	if err := h.fetchAppState(ctx, phone.renameContact(t, contact, "Alice Phone")); err != nil {
		t.Fatalf("contact-list patch: %v", err)
	}
	name := string(appstate.WAPatchCriticalUnblockLow)
	var indexMAC []byte
	for key := range phone.records {
		indexMAC = []byte(key)
	}
	if version, _, err := h.cli.Store.AppState.GetAppStateVersion(ctx, name); err != nil || version != 1 {
		t.Fatalf("stored version = %d, %v; want 1", version, err)
	}
	if mac, err := h.cli.Store.AppState.GetAppStateMutationMAC(ctx, name, indexMAC); err != nil || mac == nil {
		t.Fatalf("stored value MAC = %x, %v; want one", mac, err)
	}

	err := whatsapplive.ProductionAppStateFullSyncForTest(ctx, h.cli, appstate.WAPatchCriticalUnblockLow)
	if !errors.Is(err, whatsmeow.ErrNotConnected) {
		t.Fatalf("full resync without a socket = %v, want ErrNotConnected", err)
	}
	if version, _, err := h.cli.Store.AppState.GetAppStateVersion(ctx, name); err != nil || version != 0 {
		t.Fatalf("stored version after the full resync = %d, %v; want 0 (deleted)", version, err)
	}
	if mac, err := h.cli.Store.AppState.GetAppStateMutationMAC(ctx, name, indexMAC); err != nil || mac != nil {
		t.Fatalf("stored value MAC after the full resync = %x, %v; want none", mac, err)
	}
}

// TestCloseCancelsAnAppStateResyncInFlight checks that closing the bridge
// cancels a running resync and waits for it, and that a retired client's
// error starts nothing.
func TestCloseCancelsAnAppStateResyncInFlight(t *testing.T) {
	h := newTeardownHarness(t, false)
	h.awaitGeneration()
	newResyncClock(t)
	var started atomic.Int32
	ended := make(chan error, 4)
	whatsapplive.StubAppStateFullSyncForTest(t, func(ctx context.Context, _ *whatsmeow.Client, _ appstate.WAPatchName) error {
		started.Add(1)
		<-ctx.Done()
		// A cancelled resync can take a moment to unwind (an interrupted
		// statement, a dispatch); Close must wait for it.
		time.Sleep(200 * time.Millisecond)
		ended <- ctx.Err()
		return ctx.Err()
	})
	h.cli.DangerousInternals().DispatchEvent(mismatchingLTHash(appstate.WAPatchCriticalUnblockLow))
	if !awaitTeardownCondition(t, "the resync to start", func() bool { return started.Load() == 1 }) {
		t.FailNow()
	}
	if err := h.host.Close(); err != nil {
		t.Fatalf("Close(): %v", err)
	}
	select {
	case err := <-ended:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("resync context after Close = %v, want cancelled", err)
		}
	default:
		t.Fatal("Close returned before the resync it cancelled")
	}
	h.cli.DangerousInternals().DispatchEvent(mismatchingLTHash(appstate.WAPatchRegularHigh))
	time.Sleep(50 * time.Millisecond)
	if n := started.Load(); n != 1 {
		t.Fatalf("resyncs started = %d after Close, want 1", n)
	}
}
