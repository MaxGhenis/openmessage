package whatsapplive

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/whatsmeow"
	waBinary "go.mau.fi/whatsmeow/binary"
	wastore "go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	watypes "go.mau.fi/whatsmeow/types"
	waevents "go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"

	"github.com/maxghenis/openmessage/internal/db"
)

var (
	relinkTestJID      = watypes.JID{User: "15551230000", Device: 7, Server: watypes.DefaultUserServer}
	relinkTestOtherJID = watypes.JID{User: "15551230000", Device: 9, Server: watypes.DefaultUserServer}
)

const relinkTestReason401 = "401: logged out from another device"

func relinkTestSessionPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "whatsapp-session.db")
}

func openRelinkTestBridge(t *testing.T, sessionPath string) *Bridge {
	t.Helper()
	b, err := New(sessionPath, nil, zerolog.Nop(), Callbacks{})
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b
}

func reopenRelinkTestBridge(t *testing.T, b *Bridge, sessionPath string) *Bridge {
	t.Helper()
	if err := b.Close(); err != nil {
		t.Fatalf("Close(): %v", err)
	}
	return openRelinkTestBridge(t, sessionPath)
}

func assertLinkedDevices(t *testing.T, sessionPath string, want ...string) {
	t.Helper()
	got := LinkedDeviceJIDsForTest(t, sessionPath)
	if len(got) == 0 && len(want) == 0 {
		return
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("stored identities = %v, want %v", got, want)
	}
}

// heldIdentity is the identity the bridge's current client holds.
func heldIdentity(b *Bridge) linkedIdentity {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.identity
}

// storedIdentity reads one stored identity (JID and key fingerprint).
func storedIdentity(t *testing.T, sessionPath string, jid watypes.JID) linkedIdentity {
	t.Helper()
	container, err := sqlstore.New(context.Background(), "sqlite", sessionStoreDSN(sessionPath), waLog.Noop)
	if err != nil {
		t.Fatalf("sqlstore.New(): %v", err)
	}
	defer container.Close()
	device, err := container.GetDevice(context.Background(), jid)
	if err != nil {
		t.Fatalf("GetDevice(%s): %v", jid, err)
	}
	return identityOf(device)
}

// deleteStoredDevice removes a device row the way whatsmeow's Store.Delete does.
func deleteStoredDevice(t *testing.T, sessionPath string, jid watypes.JID) {
	t.Helper()
	database, err := sql.Open("sqlite", sessionStoreDSN(sessionPath))
	if err != nil {
		t.Fatalf("open session store: %v", err)
	}
	defer database.Close()
	if _, err := database.Exec(`DELETE FROM whatsmeow_device WHERE jid = ?`, jid.String()); err != nil {
		t.Fatalf("delete device %s: %v", jid, err)
	}
}

// failPurgeDeletes makes the purge's device delete fail until the returned
// function is called (or the test ends).
func failPurgeDeletes(t *testing.T) (restore func()) {
	t.Helper()
	original := deleteLoggedOutDevice
	deleteLoggedOutDevice = func(context.Context, *sqlstore.Container, *wastore.Device) error {
		return errors.New("injected: session store is locked")
	}
	var once sync.Once
	restore = func() { once.Do(func() { deleteLoggedOutDevice = original }) }
	t.Cleanup(restore)
	return restore
}

// failRecordWrites makes writes of the logout record fail until the returned
// function is called (or the test ends).
func failRecordWrites(t *testing.T) (restore func()) {
	t.Helper()
	original := writeLoggedOutState
	writeLoggedOutState = func(context.Context, *sql.DB, loggedOutState) error {
		return errors.New("injected: disk I/O error")
	}
	var once sync.Once
	restore = func() { once.Do(func() { writeLoggedOutState = original }) }
	t.Cleanup(restore)
	return restore
}

// The mechanism behind the 2026-08-20 state, in the pinned whatsmeow: on a
// logout it dispatches LoggedOut on a goroutine and then deletes the session
// with the context the node handler was given (the connection's event context,
// which is the one ConnectPaired passes). If that context has ended, the event
// still goes out but the identity stays. The lifecycle adapter cancels exactly
// that context on LoggedOut (see the adapter's
// TestLoggedOutCancelsTheContextGivenToConnect).
func TestWhatsmeowLogoutDeleteNeedsALiveEventContext(t *testing.T) {
	paths := []struct {
		name      string
		onConnect bool
		handle    func(*whatsmeow.Client, context.Context)
	}{
		{
			name:      "connect failure 401",
			onConnect: true,
			handle: func(cli *whatsmeow.Client, ctx context.Context) {
				cli.DangerousInternals().HandleConnectFailure(ctx, &waBinary.Node{
					Tag:   "failure",
					Attrs: waBinary.Attrs{"reason": "401"},
				})
			},
		},
		{
			name:      "device_removed stream error",
			onConnect: false,
			handle: func(cli *whatsmeow.Client, ctx context.Context) {
				cli.DangerousInternals().HandleStreamError(ctx, &waBinary.Node{
					Tag:   "stream:error",
					Attrs: waBinary.Attrs{"code": "401"},
					Content: []waBinary.Node{{
						Tag:   "conflict",
						Attrs: waBinary.Attrs{"type": "device_removed"},
					}},
				})
			},
		},
	}
	for _, path := range paths {
		for _, cancelled := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/context cancelled=%v", path.name, cancelled), func(t *testing.T) {
				sessionPath := relinkTestSessionPath(t)
				SeedLinkedDeviceForTest(t, sessionPath, relinkTestJID)
				container, err := sqlstore.New(context.Background(), "sqlite", sessionStoreDSN(sessionPath), waLog.Noop)
				if err != nil {
					t.Fatalf("sqlstore.New(): %v", err)
				}
				defer container.Close()
				device, err := container.GetFirstDevice(context.Background())
				if err != nil || device.ID == nil {
					t.Fatalf("GetFirstDevice() = %v, %v; want the seeded identity", device, err)
				}
				cli := whatsmeow.NewClient(device, waLog.Noop)
				events := make(chan *waevents.LoggedOut, 1)
				cli.AddEventHandler(func(raw any) {
					if evt, ok := raw.(*waevents.LoggedOut); ok {
						events <- evt
					}
				})

				ctx := context.Background()
				if cancelled {
					var cancel context.CancelFunc
					ctx, cancel = context.WithCancel(ctx)
					cancel()
				}
				path.handle(cli, ctx)

				select {
				case evt := <-events:
					if evt.OnConnect != path.onConnect || evt.Reason != waevents.ConnectFailureLoggedOut {
						t.Fatalf("LoggedOut = %+v, want OnConnect=%v Reason=401", evt, path.onConnect)
					}
					if got := evt.PermanentDisconnectDescription(); got != relinkTestReason401 {
						t.Fatalf("description = %q, want %q", got, relinkTestReason401)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("whatsmeow did not dispatch LoggedOut")
				}
				if cancelled {
					assertLinkedDevices(t, sessionPath, relinkTestJID.String())
				} else {
					assertLinkedDevices(t, sessionPath)
				}
			})
		}
	}
}

func TestLogoutReportsNeedsRelinkAndRemovesTheRevokedIdentity(t *testing.T) {
	sessionPath := relinkTestSessionPath(t)
	SeedLinkedDeviceForTest(t, sessionPath, relinkTestJID)
	b := openRelinkTestBridge(t, sessionPath)
	if status := b.Status(); !status.Paired || status.NeedsRelink {
		t.Fatalf("status before logout = %+v, want paired without needs_relink", status)
	}

	b.mu.RLock()
	source, generation := b.client, b.clientGeneration
	b.mu.RUnlock()

	var observed bool
	var statusAtObserver StatusSnapshot
	var storedAtObserver []string
	b.ObserveLifecycle(func(event LifecycleEvent) {
		if _, ok := event.Raw.(*waevents.LoggedOut); !ok {
			return
		}
		observed = true
		statusAtObserver = b.Status()
		storedAtObserver = LinkedDeviceJIDsForTest(t, sessionPath)
	})

	before := time.Now()
	// whatsmeow's own delete lost its race: the event arrives while the
	// revoked identity is still stored and still held by the client.
	b.handleClientEvent(source, generation, &waevents.LoggedOut{
		OnConnect: true,
		Reason:    waevents.ConnectFailureLoggedOut,
	})
	after := time.Now()

	if !observed {
		t.Fatal("the supervisor observer never saw the LoggedOut")
	}
	// The observer runs before the purge, and already sees the logout.
	if !statusAtObserver.NeedsRelink || statusAtObserver.Paired {
		t.Fatalf("status seen by the observer = %+v, want needs_relink and not paired", statusAtObserver)
	}
	if !reflect.DeepEqual(storedAtObserver, []string{relinkTestJID.String()}) {
		t.Fatalf("stored identities when the observer ran = %v, want the revoked one still present", storedAtObserver)
	}

	status := b.Status()
	if status.Paired || status.AccountJID != "" {
		t.Fatalf("status = %+v, want unpaired with no account", status)
	}
	if !status.NeedsRelink || status.RelinkReason != relinkTestReason401 {
		t.Fatalf("status = %+v, want needs_relink with reason %q", status, relinkTestReason401)
	}
	if status.RelinkSinceMS < before.UnixMilli() || status.RelinkSinceMS > after.UnixMilli() {
		t.Fatalf("relink_since_ms = %d, want within [%d, %d]", status.RelinkSinceMS, before.UnixMilli(), after.UnixMilli())
	}
	if status.LastError != relinkTestReason401 {
		t.Fatalf("last_error = %q, want %q", status.LastError, relinkTestReason401)
	}
	if b.PairedAccount().Paired {
		t.Fatal("PairedAccount() still reports the revoked identity")
	}
	if !b.UsesLiveSession() {
		t.Fatal("UsesLiveSession() = false during a logout; the WhatsApp Desktop importer would take over")
	}
	assertLinkedDevices(t, sessionPath)

	b.mu.RLock()
	replaced := b.client != source && b.client.Store.ID == nil && b.identity.empty() && b.clientGeneration > generation
	b.mu.RUnlock()
	if !replaced {
		t.Fatal("the client holding the revoked identity was not replaced by a fresh unpaired one")
	}

	encoded, err := json.Marshal(status)
	if err != nil {
		t.Fatalf("json.Marshal(status): %v", err)
	}
	for _, fragment := range []string{
		`"paired":false`,
		`"needs_relink":true`,
		`"relink_reason":"` + relinkTestReason401 + `"`,
		fmt.Sprintf(`"relink_since_ms":%d`, status.RelinkSinceMS),
	} {
		if !strings.Contains(string(encoded), fragment) {
			t.Fatalf("status JSON %s lacks %s", encoded, fragment)
		}
	}

	t.Run("survives a restart", func(t *testing.T) {
		restarted := reopenRelinkTestBridge(t, b, sessionPath)
		got := restarted.Status()
		if got.Paired || !got.NeedsRelink || got.RelinkReason != status.RelinkReason || got.RelinkSinceMS != status.RelinkSinceMS {
			t.Fatalf("status after restart = %+v, want the same episode %+v", got, status)
		}
		if restarted.PairedAccount().Paired {
			t.Fatal("PairedAccount() after restart reports paired")
		}
	})
}

func TestStatusAlwaysCarriesNeedsRelink(t *testing.T) {
	encoded, err := json.Marshal(StatusSnapshot{})
	if err != nil {
		t.Fatalf("json.Marshal(): %v", err)
	}
	if !strings.Contains(string(encoded), `"needs_relink":false`) {
		t.Fatalf("status JSON %s omits needs_relink; clients use its presence to tell old backends apart", encoded)
	}
	if strings.Contains(string(encoded), "relink_reason") || strings.Contains(string(encoded), "relink_since_ms") {
		t.Fatalf("status JSON %s carries empty relink details", encoded)
	}
}

// A revoked identity left on disk (its purge failed, or the process died
// before it ran) must read as unpaired, must never be dialed, and must not
// block pairing. Building the bridge removes it; if that fails too, every
// connect and pair entry point does.
func TestRevokedIdentityLeftOnDiskIsRemovedBeforeDialOrPairing(t *testing.T) {
	t.Run("at startup", func(t *testing.T) {
		sessionPath := relinkTestSessionPath(t)
		SeedLinkedDeviceForTest(t, sessionPath, relinkTestJID)
		b := openRelinkTestBridge(t, sessionPath)
		b.recordLoggedOut(time.Now(), relinkTestReason401, heldIdentity(b))
		b = reopenRelinkTestBridge(t, b, sessionPath)
		assertLinkedDevices(t, sessionPath)
		if status := b.Status(); status.Paired || !status.NeedsRelink {
			t.Fatalf("status after startup = %+v, want unpaired with needs_relink", status)
		}
		if !heldIdentity(b).empty() {
			t.Fatal("the rebuilt client still holds the revoked identity")
		}
	})

	entryPoints := []struct {
		name  string
		enter func(*testing.T, *Bridge)
	}{
		{
			name: "ConnectPaired",
			enter: func(t *testing.T, b *Bridge) {
				if err := b.ConnectPaired(context.Background()); !errors.Is(err, whatsmeow.ErrNotLoggedIn) {
					t.Fatalf("ConnectPaired() error = %v, want ErrNotLoggedIn", err)
				}
			},
		},
		{
			name: "PrepareQRPairing",
			enter: func(t *testing.T, b *Bridge) {
				if _, err := b.PrepareQRPairing(context.Background()); err != nil {
					t.Fatalf("PrepareQRPairing() error = %v, want QR pairing to start", err)
				}
				if status := b.Status(); !status.Pairing || !status.NeedsRelink {
					t.Fatalf("status while relinking = %+v, want pairing with needs_relink still set", status)
				}
			},
		},
	}
	for _, entry := range entryPoints {
		t.Run(entry.name, func(t *testing.T) {
			sessionPath := relinkTestSessionPath(t)
			SeedLinkedDeviceForTest(t, sessionPath, relinkTestJID)
			b := openRelinkTestBridge(t, sessionPath)
			b.recordLoggedOut(time.Now(), relinkTestReason401, heldIdentity(b))
			restoreDeletes := failPurgeDeletes(t)
			b = reopenRelinkTestBridge(t, b, sessionPath)

			assertLinkedDevices(t, sessionPath, relinkTestJID.String())
			if status := b.Status(); status.Paired || !status.NeedsRelink {
				t.Fatalf("status with the revoked identity on disk = %+v, want unpaired and needs_relink", status)
			}
			StubConnectForTest(t, func(context.Context, *whatsmeow.Client) error {
				t.Error("dialed WhatsApp with a revoked identity")
				return nil
			})
			restoreDeletes()

			entry.enter(t, b)
			assertLinkedDevices(t, sessionPath)
		})
	}
}

// While the purge cannot delete the revoked identity, the client keeps
// holding it, so later logouts of that identity arrive too. They must keep
// the first logout's episode.
func TestRepeatedLogoutWhileThePurgeFailsKeepsTheEpisode(t *testing.T) {
	sessionPath := relinkTestSessionPath(t)
	SeedLinkedDeviceForTest(t, sessionPath, relinkTestJID)
	b := openRelinkTestBridge(t, sessionPath)
	restoreDeletes := failPurgeDeletes(t)

	b.handleEvent(&waevents.LoggedOut{OnConnect: true, Reason: waevents.ConnectFailureLoggedOut})
	first := b.Status()
	if first.Paired || !first.NeedsRelink || first.RelinkReason != relinkTestReason401 {
		t.Fatalf("status after the first logout = %+v, want unpaired with needs_relink (401)", first)
	}
	assertLinkedDevices(t, sessionPath, relinkTestJID.String())
	if heldIdentity(b).empty() {
		t.Fatal("a failed purge replaced the client anyway")
	}

	time.Sleep(2 * time.Millisecond)
	b.handleEvent(&waevents.LoggedOut{OnConnect: true, Reason: waevents.ConnectFailureMainDeviceGone})
	second := b.Status()
	if second.RelinkReason != first.RelinkReason || second.RelinkSinceMS != first.RelinkSinceMS {
		t.Fatalf("repeat logout moved the episode from %+v to %+v", first, second)
	}

	restoreDeletes()
	StubConnectForTest(t, func(context.Context, *whatsmeow.Client) error {
		t.Error("dialed WhatsApp with a revoked identity")
		return nil
	})
	if err := b.ConnectPaired(context.Background()); !errors.Is(err, whatsmeow.ErrNotLoggedIn) {
		t.Fatalf("ConnectPaired() error = %v, want ErrNotLoggedIn", err)
	}
	assertLinkedDevices(t, sessionPath)
	if got := b.Status(); got.RelinkSinceMS != first.RelinkSinceMS || !got.NeedsRelink {
		t.Fatalf("status after the purge = %+v, want the same episode", got)
	}
}

func TestLogoutEpisodeKeepsItsFirstTimeAndReason(t *testing.T) {
	b := openRelinkTestBridge(t, relinkTestSessionPath(t))
	revoked := linkedIdentity{JID: relinkTestJID, Key: "key-1"}
	first := time.Unix(1_787_260_699, 0)
	b.recordLoggedOut(first, relinkTestReason401, revoked)
	b.recordLoggedOut(first.Add(time.Hour), "403: primary device was logged out", revoked)
	if got := b.loggedOutSnapshot(); !got.At.Equal(first) || got.Reason != relinkTestReason401 {
		t.Fatalf("episode after a repeat = %+v, want the first logout kept", got)
	}

	// The same JID with a new identity key is a new link, so its logout opens
	// a new episode.
	relinked := linkedIdentity{JID: relinkTestJID, Key: "key-2"}
	second := first.Add(48 * time.Hour)
	b.recordLoggedOut(second, "406: logged out for unknown reason", relinked)
	if got := b.loggedOutSnapshot(); !got.At.Equal(second) || got.Key != "key-2" {
		t.Fatalf("episode after the relinked identity's logout = %+v, want that logout", got)
	}
}

func TestNewLinkEndsTheLogout(t *testing.T) {
	sessionPath := relinkTestSessionPath(t)
	SeedLinkedDeviceForTest(t, sessionPath, relinkTestJID)
	b := openRelinkTestBridge(t, sessionPath)
	b.handleEvent(&waevents.LoggedOut{Reason: waevents.ConnectFailureLoggedOut})
	if !b.Status().NeedsRelink {
		t.Fatal("logout did not set needs_relink")
	}

	emulatePairingForTest(t, b, relinkTestOtherJID)
	status := b.Status()
	if !status.Paired || status.NeedsRelink || status.AccountJID != relinkTestOtherJID.String() {
		t.Fatalf("status after relinking = %+v, want paired as %s without needs_relink", status, relinkTestOtherJID)
	}
	if got, want := heldIdentity(b), storedIdentity(t, sessionPath, relinkTestOtherJID); got != want {
		t.Fatalf("held identity after PairSuccess = %+v, want the stored one %+v", got, want)
	}
	b = reopenRelinkTestBridge(t, b, sessionPath)
	if status := b.Status(); !status.Paired || status.NeedsRelink {
		t.Fatalf("status after restart = %+v, want the new link without needs_relink", status)
	}
	assertLinkedDevices(t, sessionPath, relinkTestOtherJID.String())
}

// A link stored after the logout supersedes the record even when its
// PairSuccess never reached the bridge (the process died between whatsmeow
// saving the pairing and dispatching the event), and it is never deleted,
// even when WhatsApp gave it the revoked identity's JID.
func TestInterruptedRelinkSupersedesTheLogout(t *testing.T) {
	t.Run("new JID", func(t *testing.T) {
		sessionPath := relinkTestSessionPath(t)
		SeedLinkedDeviceForTest(t, sessionPath, relinkTestJID)
		b := openRelinkTestBridge(t, sessionPath)
		b.handleEvent(&waevents.LoggedOut{Reason: waevents.ConnectFailureLoggedOut})
		assertLinkedDevices(t, sessionPath)

		SeedLinkedDeviceForTest(t, sessionPath, relinkTestOtherJID)
		// Before any reconciliation the stored link already reads as paired.
		if status := b.Status(); !status.Paired || status.NeedsRelink {
			t.Fatalf("status with a newer link stored = %+v, want paired without needs_relink", status)
		}
		b = reopenRelinkTestBridge(t, b, sessionPath)
		if status := b.Status(); !status.Paired || status.NeedsRelink || status.AccountJID != relinkTestOtherJID.String() {
			t.Fatalf("status after restart = %+v, want the newer link", status)
		}
		if b.loggedOutSnapshot().active() {
			t.Fatal("the superseded record is still held")
		}
		b = reopenRelinkTestBridge(t, b, sessionPath)
		if b.loggedOutSnapshot().active() {
			t.Fatal("the superseded record came back after another restart")
		}
		assertLinkedDevices(t, sessionPath, relinkTestOtherJID.String())
	})

	t.Run("same JID, new keys", func(t *testing.T) {
		sessionPath := relinkTestSessionPath(t)
		SeedLinkedDeviceForTest(t, sessionPath, relinkTestJID)
		b := openRelinkTestBridge(t, sessionPath)
		revoked := heldIdentity(b)
		restoreDeletes := failPurgeDeletes(t)
		b.handleEvent(&waevents.LoggedOut{Reason: waevents.ConnectFailureLoggedOut})
		restoreDeletes()
		if err := b.Close(); err != nil {
			t.Fatalf("Close(): %v", err)
		}
		// WhatsApp relinked this install under the same JID: new keys.
		deleteStoredDevice(t, sessionPath, relinkTestJID)
		SeedLinkedDeviceForTest(t, sessionPath, relinkTestJID)
		relinked := storedIdentity(t, sessionPath, relinkTestJID)
		if relinked.Key == revoked.Key {
			t.Fatal("the seeded relink reused the revoked identity key")
		}

		b = openRelinkTestBridge(t, sessionPath)
		assertLinkedDevices(t, sessionPath, relinkTestJID.String())
		if got := storedIdentity(t, sessionPath, relinkTestJID); got != relinked {
			t.Fatalf("stored identity = %+v, want the relinked one %+v kept", got, relinked)
		}
		if status := b.Status(); !status.Paired || status.NeedsRelink {
			t.Fatalf("status = %+v, want the relinked identity paired without needs_relink", status)
		}
		if b.loggedOutSnapshot().active() {
			t.Fatal("the superseded record is still held")
		}
	})

	t.Run("revoked and newer rows both stored", func(t *testing.T) {
		sessionPath := relinkTestSessionPath(t)
		SeedLinkedDeviceForTest(t, sessionPath, relinkTestJID)
		b := openRelinkTestBridge(t, sessionPath)
		restoreDeletes := failPurgeDeletes(t)
		b.handleEvent(&waevents.LoggedOut{Reason: waevents.ConnectFailureLoggedOut})
		SeedLinkedDeviceForTest(t, sessionPath, relinkTestOtherJID)
		restoreDeletes()
		assertLinkedDevices(t, sessionPath, relinkTestJID.String(), relinkTestOtherJID.String())

		b = reopenRelinkTestBridge(t, b, sessionPath)
		assertLinkedDevices(t, sessionPath, relinkTestOtherJID.String())
		if status := b.Status(); !status.Paired || status.NeedsRelink || status.AccountJID != relinkTestOtherJID.String() {
			t.Fatalf("status = %+v, want only the newer link, paired", status)
		}
		if got := heldIdentity(b); got.JID != relinkTestOtherJID {
			t.Fatalf("held identity = %+v, want the newer link", got)
		}
	})
}

// whatsmeow does not order PairSuccess before the connection that follows a
// pairing. A paired Connected whose PairSuccess has not arrived adopts the
// identity whatsmeow installed: the adapter's pairer can finish on it, and it
// supersedes an outstanding logout.
func TestConnectedAdoptsAnIdentityWhosePairSuccessIsMissing(t *testing.T) {
	sessionPath := relinkTestSessionPath(t)
	SeedLinkedDeviceForTest(t, sessionPath, relinkTestJID)
	// A paired Connected starts store reconciliation, so this bridge needs a
	// message store.
	store, err := db.New(filepath.Join(filepath.Dir(sessionPath), "messages.db"))
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	b, err := New(sessionPath, store, zerolog.Nop(), Callbacks{})
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	b.handleEvent(&waevents.LoggedOut{Reason: waevents.ConnectFailureLoggedOut})
	if !heldIdentity(b).empty() || !b.Status().NeedsRelink {
		t.Fatal("logout did not leave an unpaired client with needs_relink")
	}

	var connectedPaired []bool
	b.ObserveLifecycle(func(event LifecycleEvent) {
		if _, ok := event.Raw.(*waevents.Connected); ok {
			connectedPaired = append(connectedPaired, event.Paired)
		}
	})
	// What a pairing does to the client before PairSuccess is dispatched.
	b.mu.Lock()
	device := b.client.Store
	device.ID = &relinkTestOtherJID
	device.Account = testADVIdentity()
	err = device.Save(context.Background())
	b.mu.Unlock()
	if err != nil {
		t.Fatalf("save paired device: %v", err)
	}
	b.handleEvent(&waevents.Connected{})

	if want := storedIdentity(t, sessionPath, relinkTestOtherJID); heldIdentity(b) != want {
		t.Fatalf("held identity = %+v, want the adopted %+v", heldIdentity(b), want)
	}
	if !reflect.DeepEqual(connectedPaired, []bool{true}) {
		t.Fatalf("Connected reached the observer with Paired = %v, want [true]", connectedPaired)
	}
	status := b.Status()
	if !status.Paired || status.NeedsRelink || status.AccountJID != relinkTestOtherJID.String() {
		t.Fatalf("status = %+v, want the adopted link paired without needs_relink", status)
	}
	if b.loggedOutSnapshot().active() {
		t.Fatal("the adopted link did not end the logout record")
	}
}

func TestUnpairEndsTheLogoutWithoutDialingTheRevokedIdentity(t *testing.T) {
	sessionPath := relinkTestSessionPath(t)
	SeedLinkedDeviceForTest(t, sessionPath, relinkTestJID)
	b := openRelinkTestBridge(t, sessionPath)
	// The purge has not run: the client still holds the revoked identity.
	b.recordLoggedOut(time.Now(), relinkTestReason401, heldIdentity(b))

	StubConnectForTest(t, func(context.Context, *whatsmeow.Client) error {
		t.Error("Unpair dialed WhatsApp with a revoked identity")
		return nil
	})
	originalLogout := logoutClient
	t.Cleanup(func() { logoutClient = originalLogout })
	logoutClient = func(context.Context, *whatsmeow.Client) error {
		t.Error("Unpair sent a logout for a revoked identity")
		return nil
	}

	if err := b.Unpair(); err != nil {
		t.Fatalf("Unpair(): %v", err)
	}
	if status := b.Status(); status.Paired || status.NeedsRelink {
		t.Fatalf("status after Unpair() = %+v, want unpaired without needs_relink", status)
	}
	if b.UsesLiveSession() {
		t.Fatal("UsesLiveSession() = true after a deliberate unpair")
	}
	assertLinkedDevices(t, sessionPath)
	b = reopenRelinkTestBridge(t, b, sessionPath)
	if b.Status().NeedsRelink {
		t.Fatal("needs_relink came back after a restart; Unpair() did not persist the clear")
	}
}

// A logout callback that passed the first generation check and then lost the
// race to a deliberate unpair must not bring the record back, revoke a link
// made right after the unpair, or touch the replacement client.
func TestLogoutCallbackRacingUnpairLeavesNoRecord(t *testing.T) {
	t.Run("unpair", func(t *testing.T) { testLogoutCallbackRacingUnpair(t, false) })
	t.Run("unpair then a new link", func(t *testing.T) { testLogoutCallbackRacingUnpair(t, true) })
}

func testLogoutCallbackRacingUnpair(t *testing.T, relink bool) {
	sessionPath := relinkTestSessionPath(t)
	SeedLinkedDeviceForTest(t, sessionPath, relinkTestJID)
	b := openRelinkTestBridge(t, sessionPath)
	b.mu.RLock()
	source, generation := b.client, b.clientGeneration
	b.mu.RUnlock()
	StubConnectForTest(t, func(context.Context, *whatsmeow.Client) error {
		return errors.New("offline")
	})

	original := testHookBeforeLogoutRecord
	t.Cleanup(func() { testHookBeforeLogoutRecord = original })
	var unpairErr error
	testHookBeforeLogoutRecord = func() {
		testHookBeforeLogoutRecord = nil
		unpairErr = b.Unpair()
		if relink {
			emulatePairingForTest(t, b, relinkTestOtherJID)
		}
	}
	b.handleClientEvent(source, generation, &waevents.LoggedOut{Reason: waevents.ConnectFailureLoggedOut})
	if unpairErr != nil {
		t.Fatalf("Unpair(): %v", unpairErr)
	}

	status := b.Status()
	if status.NeedsRelink || status.Paired != relink || status.LastError != "" {
		t.Fatalf("status = %+v, want the deliberate unpair (and any new link) intact: no needs_relink, no logout error", status)
	}
	if relink {
		assertLinkedDevices(t, sessionPath, relinkTestOtherJID.String())
	} else {
		assertLinkedDevices(t, sessionPath)
	}
	b = reopenRelinkTestBridge(t, b, sessionPath)
	if b.Status().NeedsRelink {
		t.Fatal("the retired callback persisted a logout record")
	}
}

// A disconnect from a client that was replaced while the event was in flight
// must not mark its successor disconnected or give it the old error.
func TestRetiredDisconnectLeavesTheSuccessorAlone(t *testing.T) {
	sessionPath := relinkTestSessionPath(t)
	SeedLinkedDeviceForTest(t, sessionPath, relinkTestJID)
	b := openRelinkTestBridge(t, sessionPath)
	b.mu.Lock()
	retired, generation := b.client, b.clientGeneration
	if err := b.resetClientLocked(); err != nil {
		b.mu.Unlock()
		t.Fatalf("resetClientLocked(): %v", err)
	}
	b.connected, b.connecting = true, true
	b.mu.Unlock()

	b.handleDisconnected(retired, generation, "stream replaced")

	b.mu.RLock()
	connected, connecting, lastError := b.connected, b.connecting, b.lastError
	b.mu.RUnlock()
	if !connected || !connecting || lastError != "" {
		t.Fatalf("successor state = connected %v, connecting %v, last_error %q; want it untouched", connected, connecting, lastError)
	}
}

// whatsmeow's Store.Delete clears the live device's ID from its own goroutine,
// holding no lock the bridge holds. Status, PairedAccount, UsesLiveSession,
// DisconnectTransport and the lifecycle event path read the bridge's own
// record instead, so they never touch that field. Under -race (CI's Go Race
// job) any read of it here is reported: the write below has no
// happens-before edge to the calls.
func TestBridgeNeverReadsTheLiveDeviceID(t *testing.T) {
	sessionPath := relinkTestSessionPath(t)
	SeedLinkedDeviceForTest(t, sessionPath, relinkTestJID)
	b := openRelinkTestBridge(t, sessionPath)
	b.recordLoggedOut(time.Now(), relinkTestReason401, heldIdentity(b))
	b.mu.RLock()
	device := b.client.Store
	b.mu.RUnlock()

	written := make(chan struct{})
	go func() {
		device.ID = nil // what whatsmeow's Store.Delete does
		close(written)
	}()
	status := b.Status()
	_ = b.PairedAccount()
	_ = b.UsesLiveSession()
	b.handleEvent(&waevents.Disconnected{})
	b.DisconnectTransport()
	<-written

	if status.Paired || !status.NeedsRelink {
		t.Fatalf("status = %+v, want the revoked identity unpaired with needs_relink", status)
	}
}

// Failed writes of the record are retried, and a restart reconciles a clear
// that never landed once a newer link is stored.
func TestFailedLogoutRecordWritesAreRetried(t *testing.T) {
	t.Run("record", func(t *testing.T) {
		sessionPath := relinkTestSessionPath(t)
		SeedLinkedDeviceForTest(t, sessionPath, relinkTestJID)
		b := openRelinkTestBridge(t, sessionPath)
		restoreWrites := failRecordWrites(t)
		b.handleEvent(&waevents.LoggedOut{Reason: waevents.ConnectFailureLoggedOut})
		if !b.Status().NeedsRelink {
			t.Fatal("a failed write hid the logout in memory too")
		}
		restoreWrites()
		_ = b.ConnectPaired(context.Background()) // entry-point purge retries the write
		b = reopenRelinkTestBridge(t, b, sessionPath)
		if status := b.Status(); !status.NeedsRelink || status.RelinkReason != relinkTestReason401 {
			t.Fatalf("status after restart = %+v, want the retried record", status)
		}
	})

	t.Run("clear on unpair", func(t *testing.T) {
		sessionPath := relinkTestSessionPath(t)
		SeedLinkedDeviceForTest(t, sessionPath, relinkTestJID)
		b := openRelinkTestBridge(t, sessionPath)
		b.handleEvent(&waevents.LoggedOut{Reason: waevents.ConnectFailureLoggedOut})
		restoreWrites := failRecordWrites(t)
		err := b.Unpair()
		if err == nil || !strings.Contains(err.Error(), "unpaired, but") {
			t.Fatalf("Unpair() error = %v, want the unwritten clear reported", err)
		}
		if b.Status().NeedsRelink {
			t.Fatal("needs_relink still set in memory after the unpair")
		}
		restoreWrites()
		_ = b.ConnectPaired(context.Background()) // entry-point purge retries the write
		b = reopenRelinkTestBridge(t, b, sessionPath)
		if b.Status().NeedsRelink {
			t.Fatal("the retried clear did not land")
		}
	})

	t.Run("clear on a new link, then a restart", func(t *testing.T) {
		sessionPath := relinkTestSessionPath(t)
		SeedLinkedDeviceForTest(t, sessionPath, relinkTestJID)
		b := openRelinkTestBridge(t, sessionPath)
		b.handleEvent(&waevents.LoggedOut{Reason: waevents.ConnectFailureLoggedOut})
		restoreWrites := failRecordWrites(t)
		emulatePairingForTest(t, b, relinkTestOtherJID)
		restoreWrites()
		b = reopenRelinkTestBridge(t, b, sessionPath)
		if status := b.Status(); !status.Paired || status.NeedsRelink {
			t.Fatalf("status after restart = %+v, want the new link without needs_relink", status)
		}
		if b.loggedOutSnapshot().active() {
			t.Fatal("startup did not drop the record the new link superseded")
		}
	})
}

func TestOnlyLogoutsRequestRelink(t *testing.T) {
	tests := []struct {
		name   string
		event  any
		reason string // empty: not a logout
	}{
		{"LoggedOut 401 on connect", &waevents.LoggedOut{OnConnect: true, Reason: waevents.ConnectFailureLoggedOut}, relinkTestReason401},
		{"LoggedOut 401 device removed", &waevents.LoggedOut{Reason: waevents.ConnectFailureLoggedOut}, relinkTestReason401},
		{"LoggedOut 403", &waevents.LoggedOut{OnConnect: true, Reason: waevents.ConnectFailureMainDeviceGone}, "403: primary device was logged out"},
		{"LoggedOut 406", &waevents.LoggedOut{OnConnect: true, Reason: waevents.ConnectFailureUnknownLogout}, "406: logged out for unknown reason"},
		{"ConnectFailure 403", &waevents.ConnectFailure{Reason: waevents.ConnectFailureMainDeviceGone}, "connect failure: 403: primary device was logged out"},
		{"ConnectFailure 400", &waevents.ConnectFailure{Reason: waevents.ConnectFailureGeneric}, ""},
		{"ConnectFailure 500", &waevents.ConnectFailure{Reason: waevents.ConnectFailureInternalServerError}, ""},
		{"Disconnected", &waevents.Disconnected{}, ""},
		{"StreamReplaced", &waevents.StreamReplaced{}, ""},
		{"ClientOutdated", &waevents.ClientOutdated{}, ""},
		{"TemporaryBan", &waevents.TemporaryBan{Code: waevents.TempBanSentTooManySameMessage, Expire: time.Hour}, ""},
		{"ManualLoginReconnect", &waevents.ManualLoginReconnect{}, ""},
		{"CATRefreshError", &waevents.CATRefreshError{Error: errors.New("refresh failed")}, ""},
		{"KeepAliveTimeout", &waevents.KeepAliveTimeout{ErrorCount: 3}, ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			sessionPath := relinkTestSessionPath(t)
			SeedLinkedDeviceForTest(t, sessionPath, relinkTestJID)
			b := openRelinkTestBridge(t, sessionPath)
			b.handleEvent(test.event)
			status := b.Status()
			if test.reason == "" {
				if status.NeedsRelink || !status.Paired {
					t.Fatalf("status = %+v, want the link kept and no needs_relink", status)
				}
				assertLinkedDevices(t, sessionPath, relinkTestJID.String())
				return
			}
			if !status.NeedsRelink || status.RelinkReason != test.reason || status.Paired {
				t.Fatalf("status = %+v, want needs_relink (%q) and unpaired", status, test.reason)
			}
			assertLinkedDevices(t, sessionPath)
		})
	}
}

func TestLogoutOnAPairingTransportIsNotARelink(t *testing.T) {
	b := openRelinkTestBridge(t, relinkTestSessionPath(t))
	b.handleEvent(&waevents.LoggedOut{OnConnect: true, Reason: waevents.ConnectFailureLoggedOut})
	if status := b.Status(); status.NeedsRelink {
		t.Fatalf("status = %+v; a never-linked install has no link to lose", status)
	}
}

func TestLogoutFromARetiredClientIsIgnored(t *testing.T) {
	sessionPath := relinkTestSessionPath(t)
	SeedLinkedDeviceForTest(t, sessionPath, relinkTestJID)
	b := openRelinkTestBridge(t, sessionPath)
	b.mu.Lock()
	retired, generation := b.client, b.clientGeneration
	if err := b.resetClientLocked(); err != nil {
		b.mu.Unlock()
		t.Fatalf("resetClientLocked(): %v", err)
	}
	b.mu.Unlock()

	b.handleClientEvent(retired, generation, &waevents.LoggedOut{Reason: waevents.ConnectFailureLoggedOut})
	if status := b.Status(); status.NeedsRelink || !status.Paired || status.LastError != "" {
		t.Fatalf("status = %+v, want a retired generation's logout ignored", status)
	}
	assertLinkedDevices(t, sessionPath, relinkTestJID.String())
}

// A record that names another identity never deletes the stored one: not a
// different JID, and not the same JID under new keys. Such a record is stale
// (the stored link superseded it), so the purge drops it.
func TestPurgeRemovesOnlyTheRevokedIdentity(t *testing.T) {
	for _, test := range []struct {
		name    string
		revoked func(stored linkedIdentity) linkedIdentity
	}{
		{"different JID", func(stored linkedIdentity) linkedIdentity {
			return linkedIdentity{JID: relinkTestOtherJID, Key: stored.Key}
		}},
		{"same JID, other keys", func(stored linkedIdentity) linkedIdentity {
			return linkedIdentity{JID: stored.JID, Key: "an-older-identity-key"}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			sessionPath := relinkTestSessionPath(t)
			SeedLinkedDeviceForTest(t, sessionPath, relinkTestJID)
			b := openRelinkTestBridge(t, sessionPath)
			stored := heldIdentity(b)
			b.recordLoggedOut(time.Now(), relinkTestReason401, test.revoked(stored))
			if status := b.Status(); !status.Paired || status.NeedsRelink {
				t.Fatalf("status = %+v, want the stored link paired and the record superseded", status)
			}

			b.mu.Lock()
			client := b.client
			removed, err := b.purgeLoggedOutIdentityLocked()
			kept := b.client == client
			b.mu.Unlock()
			if err != nil || removed {
				t.Fatalf("purgeLoggedOutIdentityLocked() = %v, %v; want nothing removed", removed, err)
			}
			if !kept {
				t.Fatal("the purge replaced a client that holds a different identity")
			}
			assertLinkedDevices(t, sessionPath, relinkTestJID.String())
			if b.loggedOutSnapshot().active() {
				t.Fatal("the purge kept a record the stored link superseded")
			}
		})
	}
}

func TestRuntimeStateGainsLogoutColumnsOnAnOlderTable(t *testing.T) {
	sessionPath := relinkTestSessionPath(t)
	banUntil := time.Now().Add(time.Hour).Round(0)
	database, err := sql.Open("sqlite", sessionStoreDSN(sessionPath))
	if err != nil {
		t.Fatalf("sql.Open(): %v", err)
	}
	// The table as the first release created it.
	if _, err := database.Exec(`
		CREATE TABLE openmessage_whatsapp_runtime_state (
		    singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
		    blocked_until_ns INTEGER NOT NULL DEFAULT 0
		)
	`); err != nil {
		t.Fatalf("create the older table: %v", err)
	}
	if _, err := database.Exec(
		`INSERT INTO openmessage_whatsapp_runtime_state (singleton, blocked_until_ns) VALUES (1, ?)`,
		banUntil.UnixNano(),
	); err != nil {
		t.Fatalf("seed the older table: %v", err)
	}
	if err := database.Close(); err != nil {
		t.Fatalf("Close(): %v", err)
	}

	b := openRelinkTestBridge(t, sessionPath)
	if got := b.TemporaryBanDeadline(); !got.Equal(banUntil) {
		t.Fatalf("TemporaryBanDeadline() = %v, want %v kept through the upgrade", got, banUntil)
	}
	if b.Status().NeedsRelink {
		t.Fatal("an upgraded table reports a logout it never recorded")
	}
	b.runtimeMu.Lock()
	columns, err := runtimeStateColumnNames(context.Background(), b.runtimeDB)
	if err == nil {
		// A second pass finds every column present and changes nothing.
		err = ensureRuntimeStateColumns(context.Background(), b.runtimeDB)
	}
	b.runtimeMu.Unlock()
	if err != nil {
		t.Fatalf("runtime state columns: %v", err)
	}
	for _, column := range whatsappRuntimeStateColumns {
		if !columns[column.name] {
			t.Fatalf("column %s missing after the upgrade (have %v)", column.name, columns)
		}
	}

	b.recordLoggedOut(time.Now(), relinkTestReason401, linkedIdentity{JID: relinkTestJID, Key: "key-1"})
	b = reopenRelinkTestBridge(t, b, sessionPath)
	if status := b.Status(); !status.NeedsRelink || status.RelinkReason != relinkTestReason401 {
		t.Fatalf("status after restart = %+v, want the logout recorded in the upgraded table", status)
	}
	if got := b.loggedOutSnapshot(); got.Key != "key-1" {
		t.Fatalf("record after restart = %+v, want its identity key kept", got)
	}
	if got := b.TemporaryBanDeadline(); !got.Equal(banUntil) {
		t.Fatalf("TemporaryBanDeadline() after restart = %v, want %v", got, banUntil)
	}
}

// Status and PairedAccount read the bridge's own record of the identity, so
// they stay coherent (never paired and needs_relink at once) and race-free
// while whatsmeow's real 401 handling deletes the device and clears its ID
// from another goroutine. Run it under -race.
func TestStatusStaysCoherentWhileWhatsmeowDeletesTheDevice(t *testing.T) {
	for i := 0; i < 20; i++ {
		sessionPath := relinkTestSessionPath(t)
		SeedLinkedDeviceForTest(t, sessionPath, relinkTestJID)
		b := openRelinkTestBridge(t, sessionPath)
		cli := b.WhatsmeowClientForTest()

		var stop atomic.Bool
		var readers sync.WaitGroup
		var violation atomic.Value
		for r := 0; r < 2; r++ {
			readers.Add(1)
			go func() {
				defer readers.Done()
				for !stop.Load() {
					status := b.Status()
					if status.Paired && status.NeedsRelink {
						violation.Store(fmt.Sprintf("%+v", status))
					}
					_ = b.PairedAccount()
					_ = b.UsesLiveSession()
				}
			}()
		}
		// A live context, so whatsmeow's own delete runs (and clears the
		// device's ID) while the bridge handles the LoggedOut it dispatched.
		cli.DangerousInternals().HandleConnectFailure(context.Background(), &waBinary.Node{
			Tag:   "failure",
			Attrs: waBinary.Attrs{"reason": "401"},
		})
		deadline := time.Now().Add(5 * time.Second)
		for !(b.Status().NeedsRelink && heldIdentity(b).empty()) {
			if time.Now().After(deadline) {
				t.Fatalf("round %d: the logout never settled (status %+v)", i, b.Status())
			}
			time.Sleep(time.Millisecond)
		}
		stop.Store(true)
		readers.Wait()
		if v := violation.Load(); v != nil {
			t.Fatalf("round %d: one status reported paired and needs_relink together: %s", i, v)
		}
		assertLinkedDevices(t, sessionPath)
		if err := b.Close(); err != nil {
			t.Fatalf("Close(): %v", err)
		}
	}
}

// TestRelinkStateMachineInvariants drives random sequences of lifecycle
// events, failed purges, pairings, interrupted pairings, unpairs, connect
// attempts and restarts through a real Bridge and session store, checking
// after every step against a model that holds at most one stored identity:
//
//	R1 needs_relink holds exactly while a logout of a linked identity is
//	   outstanding and no newer link is stored (no PairSuccess, unpair or
//	   superseding link since), and relink_reason and relink_since_ms stay
//	   those of the episode's first logout, through repeated logouts.
//	R2 needs_relink implies not paired.
//	R3 Status().Paired and PairedAccount().Paired agree, and are true exactly
//	   while a linked identity WhatsApp has not revoked is stored or held;
//	   account_jid names it.
//	R4 the session store holds exactly the model's identities: a successful
//	   purge leaves no revoked identity, a failed one leaves it in place, and
//	   no other identity is ever deleted, including a relink that reuses the
//	   revoked JID under new keys.
//	R5 R1-R4 hold across restarts, including a restart that interrupts a
//	   pairing before its PairSuccess.
//
// Multiple stored identities and failed record writes are covered by the
// deterministic tests above.
func TestRelinkStateMachineInvariants(t *testing.T) {
	const (
		cases    = 60
		maxSteps = 14
	)
	type action int
	const (
		actLogout          action = iota // LoggedOut or a logout ConnectFailure
		actOtherEvent                    // lifecycle events that are not logouts
		actPair                          // a pairing completes (PairSuccess)
		actInterruptedPair               // a pairing is saved, then the process dies
		actUnpair                        // explicit unpair
		actConnect                       // a connect attempt (entry-point purge)
		actRestart                       // close and reopen the bridge
		actionCount
	)
	logoutEvents := []func() (any, string){
		func() (any, string) {
			evt := &waevents.LoggedOut{OnConnect: true, Reason: waevents.ConnectFailureLoggedOut}
			return evt, evt.PermanentDisconnectDescription()
		},
		func() (any, string) {
			evt := &waevents.LoggedOut{Reason: waevents.ConnectFailureLoggedOut}
			return evt, evt.PermanentDisconnectDescription()
		},
		func() (any, string) {
			evt := &waevents.LoggedOut{OnConnect: true, Reason: waevents.ConnectFailureMainDeviceGone}
			return evt, evt.PermanentDisconnectDescription()
		},
		func() (any, string) {
			evt := &waevents.ConnectFailure{Reason: waevents.ConnectFailureUnknownLogout}
			return evt, evt.PermanentDisconnectDescription()
		},
	}
	otherEvents := []func() any{
		func() any { return &waevents.Disconnected{} },
		func() any { return &waevents.StreamReplaced{} },
		func() any { return &waevents.ClientOutdated{} },
		func() any { return &waevents.ManualLoginReconnect{} },
		func() any { return &waevents.KeepAliveTimeout{ErrorCount: 1} },
		func() any { return &waevents.ConnectFailure{Reason: waevents.ConnectFailureGeneric} },
		func() any { return &waevents.ConnectFailure{Reason: waevents.ConnectFailureServiceUnavailable} },
	}

	// Dials fail offline: Unpair falls back to the local delete, and a
	// connect attempt changes nothing but last_error.
	StubConnectForTest(t, func(context.Context, *whatsmeow.Client) error {
		return errors.New("offline")
	})

	type model struct {
		stored   linkedIdentity // the one stored identity, if any
		held     linkedIdentity // the identity the client holds
		marker   linkedIdentity // the identity an outstanding logout revoked
		reason   string
		sinceMS  int64 // captured on the step that opened the episode
		openedAt int64
	}
	// reconcile mirrors purgeLoggedOutIdentityLocked with deletes working.
	reconcile := func(m *model) {
		if m.marker.empty() {
			return
		}
		if m.stored == m.marker {
			m.stored = linkedIdentity{}
		}
		if m.held == m.marker {
			m.held = m.stored
		}
		linked := m.held
		if linked.empty() {
			linked = m.stored
		}
		if !linked.empty() && linked != m.marker {
			m.marker, m.reason, m.sinceMS = linkedIdentity{}, "", 0
		}
	}
	actionCounts := make([]int, actionCount)
	branches := map[string]int{}

	for c := 0; c < cases; c++ {
		r := rand.New(rand.NewPCG(uint64(c)+1, 0x9e3779b97f4a7c15))
		sessionPath := relinkTestSessionPath(t)
		nextDevice := uint16(1)
		newJID := func() watypes.JID {
			jid := watypes.JID{User: "15551230000", Device: nextDevice, Server: watypes.DefaultUserServer}
			nextDevice++
			return jid
		}
		// pairingJID is new, or (half the time after a logout) the
		// revoked JID, as WhatsApp may hand out again.
		pairingJID := func(m model) watypes.JID {
			if !m.marker.empty() && r.IntN(2) == 0 {
				branches["relink reuses the revoked JID"]++
				return m.marker.JID
			}
			return newJID()
		}
		var m model
		if r.IntN(2) == 0 {
			jid := newJID()
			SeedLinkedDeviceForTest(t, sessionPath, jid)
			m.stored = storedIdentity(t, sessionPath, jid)
			m.held = m.stored
		}
		b := openRelinkTestBridge(t, sessionPath)

		check := func(step string) {
			t.Helper()
			status := b.Status()
			linked := m.held
			if linked.empty() {
				linked = m.stored
			}
			wantPaired := !linked.empty() && linked != m.marker
			wantRelink := !m.marker.empty() && (linked.empty() || linked == m.marker)
			// R1
			if status.NeedsRelink != wantRelink {
				t.Fatalf("case %d %s: needs_relink = %v, model %v (status %+v, model %+v)", c, step, status.NeedsRelink, wantRelink, status, m)
			}
			if wantRelink {
				if status.RelinkReason != m.reason {
					t.Fatalf("case %d %s: relink_reason = %q, model %q", c, step, status.RelinkReason, m.reason)
				}
				if m.sinceMS == 0 {
					if status.RelinkSinceMS < m.openedAt {
						t.Fatalf("case %d %s: relink_since_ms = %d precedes the logout at %d", c, step, status.RelinkSinceMS, m.openedAt)
					}
					m.sinceMS = status.RelinkSinceMS
				} else if status.RelinkSinceMS != m.sinceMS {
					t.Fatalf("case %d %s: relink_since_ms moved from %d to %d within one episode", c, step, m.sinceMS, status.RelinkSinceMS)
				}
			} else if status.RelinkReason != "" || status.RelinkSinceMS != 0 {
				t.Fatalf("case %d %s: relink details %q/%d without needs_relink", c, step, status.RelinkReason, status.RelinkSinceMS)
			}
			// R2
			if status.NeedsRelink && status.Paired {
				t.Fatalf("case %d %s: needs_relink and paired together: %+v", c, step, status)
			}
			// R3
			if account := b.PairedAccount(); account.Paired != status.Paired {
				t.Fatalf("case %d %s: PairedAccount().Paired = %v, Status().Paired = %v", c, step, account.Paired, status.Paired)
			}
			wantAccount := ""
			if wantPaired {
				wantAccount = linked.JID.String()
			}
			if status.Paired != wantPaired || status.AccountJID != wantAccount {
				t.Fatalf("case %d %s: paired=%v account=%q, model paired=%v account=%q (model %+v)", c, step, status.Paired, status.AccountJID, wantPaired, wantAccount, m)
			}
			// R4
			var want []string
			if !m.stored.empty() {
				want = []string{m.stored.JID.String()}
				if got := storedIdentity(t, sessionPath, m.stored.JID); got != m.stored {
					t.Fatalf("case %d %s: stored identity %+v, model %+v", c, step, got, m.stored)
				}
			}
			if got := LinkedDeviceJIDsForTest(t, sessionPath); !reflect.DeepEqual(got, want) {
				t.Fatalf("case %d %s: stored identities %v, model %v", c, step, got, want)
			}
			if got := heldIdentity(b); got != m.held {
				t.Fatalf("case %d %s: held identity %+v, model %+v", c, step, got, m.held)
			}
		}
		check("open")

		steps := 1 + r.IntN(maxSteps)
		for s := 0; s < steps; s++ {
			act := action(r.IntN(int(actionCount)))
			actionCounts[act]++
			step := fmt.Sprintf("step %d (action %d)", s, act)
			switch act {
			case actLogout:
				event, description := logoutEvents[r.IntN(len(logoutEvents))]()
				purgeFails := r.IntN(2) == 0
				var restore func()
				if purgeFails {
					restore = failPurgeDeletes(t)
				}
				openedAt := time.Now().UnixMilli()
				b.handleEvent(event)
				if restore != nil {
					restore()
				}
				if m.held.empty() {
					branches["logout with no link"]++
					break
				}
				if m.marker == m.held {
					branches["repeat logout of a revoked identity"]++
				} else {
					branches["logout of a link"]++
					m.marker, m.reason, m.sinceMS, m.openedAt = m.held, description, 0, openedAt
				}
				if purgeFails {
					branches["purge failed"]++
					break
				}
				reconcile(&m)
			case actOtherEvent:
				b.handleEvent(otherEvents[r.IntN(len(otherEvents))]())
				if !m.held.empty() {
					branches["other event on a link"]++
				}
			case actPair, actInterruptedPair:
				if !m.held.empty() || !m.stored.empty() {
					branches["pair skipped: a link is held or stored"]++
					break
				}
				jid := pairingJID(m)
				if act == actPair {
					if m.marker.empty() {
						branches["first link"]++
					} else {
						branches["relink after logout"]++
					}
					emulatePairingForTest(t, b, jid)
					m.stored = storedIdentity(t, sessionPath, jid)
					m.held = m.stored
					m.marker, m.reason, m.sinceMS = linkedIdentity{}, "", 0
					break
				}
				branches["pairing interrupted by a restart"]++
				SeedLinkedDeviceForTest(t, sessionPath, jid)
				m.stored = storedIdentity(t, sessionPath, jid)
				b = reopenRelinkTestBridge(t, b, sessionPath)
				m.held = m.stored
				reconcile(&m)
			case actUnpair:
				if !m.marker.empty() {
					branches["unpair during an episode"]++
				}
				if err := b.Unpair(); err != nil {
					t.Fatalf("case %d %s: Unpair(): %v", c, step, err)
				}
				m = model{}
			case actConnect:
				if !m.marker.empty() && m.stored == m.marker {
					branches["connect purges a revoked identity"]++
				}
				_ = b.ConnectPaired(context.Background())
				reconcile(&m)
			case actRestart:
				if !m.marker.empty() {
					branches["restart during an episode"]++
				}
				if !m.marker.empty() && m.stored == m.marker {
					branches["restart purges a revoked identity"]++
				}
				b = reopenRelinkTestBridge(t, b, sessionPath)
				m.held = m.stored
				reconcile(&m)
			}
			check(step)
		}
		if err := b.Close(); err != nil {
			t.Fatalf("case %d: Close(): %v", c, err)
		}
	}

	for act, count := range actionCounts {
		if count < 15 {
			t.Fatalf("action %d ran %d times; the generator no longer exercises it (counts %v)", act, count, actionCounts)
		}
	}
	for _, branch := range []string{
		"logout of a link",
		"logout with no link",
		"repeat logout of a revoked identity",
		"purge failed",
		"other event on a link",
		"first link",
		"relink after logout",
		"relink reuses the revoked JID",
		"pairing interrupted by a restart",
		"pair skipped: a link is held or stored",
		"unpair during an episode",
		"connect purges a revoked identity",
		"restart during an episode",
		"restart purges a revoked identity",
	} {
		if branches[branch] == 0 {
			t.Fatalf("branch %q never ran (branches %v)", branch, branches)
		}
	}
	t.Logf("actions %v, branches %v", actionCounts, branches)
}
