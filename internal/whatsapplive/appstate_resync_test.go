package whatsapplive_test

import (
	"context"
	"errors"
	"maps"
	"slices"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/proto/waServerSync"
	watypes "go.mau.fi/whatsmeow/types"
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

// interruptAppStateSync applies one patch list with a handler context that
// ends right after whatsmeow stores the collection's new version, so the
// value-MAC writes that follow are cancelled: what generation teardown did
// before the handler context was detached from it.
func (h *teardownHarness) interruptAppStateSync(ctx context.Context, list *appstate.PatchList) error {
	h.t.Helper()
	interrupted, cancel := context.WithCancel(ctx)
	defer cancel()
	original := h.cli.Store.AppState
	var once sync.Once
	h.cli.Store.AppState = &appStateStoreHook{AppStateStore: original, afterPutVersion: func() { once.Do(cancel) }}
	defer func() { h.cli.Store.AppState = original }()
	err := h.fetchAppState(interrupted, list)
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
	phone := h.newAppStatePhone()
	resyncs := phone.serveFullSyncs(t)
	contact := watypes.JID{User: "15550001111", Server: watypes.DefaultUserServer}

	if err := h.fetchAppState(ctx, phone.renameContact(t, contact, "Alice Phone")); err != nil {
		t.Fatalf("first contact-list patch: %v", err)
	}
	h.requireContactName(contact, "Alice Phone", "first patch")
	interruptedErr := h.interruptAppStateSync(ctx, phone.renameContact(t, contact, "Alice Moved"))

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
