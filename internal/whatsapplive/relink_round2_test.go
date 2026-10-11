package whatsapplive

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/whatsmeow"
	watypes "go.mau.fi/whatsmeow/types"
	waevents "go.mau.fi/whatsmeow/types/events"

	"github.com/maxghenis/openmessage/internal/db"
)

// Tests for the second review round of PR #232: deletes and clears other
// processes cannot defeat, logouts that overtake PairSuccess, callbacks from
// replaced clients, and stored replacements seen without a restart.

// openRelinkTestBridgeWithStore is openRelinkTestBridge with a message store,
// for tests whose paired Connected starts store reconciliation.
func openRelinkTestBridgeWithStore(t *testing.T, sessionPath string) *Bridge {
	t.Helper()
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
	b.ClaimLiveSession()
	return b
}

func currentGeneration(b *Bridge) (*whatsmeow.Client, uint64) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.client, b.clientGeneration
}

func refuseDials(t *testing.T) {
	t.Helper()
	StubConnectForTest(t, func(context.Context, *whatsmeow.Client) error {
		t.Error("dialed WhatsApp with a revoked or deleted identity")
		return nil
	})
}

// The purge checks the stored row's identity key and deletes in one
// statement that repeats the check, so a row another process replaced between
// the two (the same JID under new keys) survives.
func TestPurgeDeleteSparesARowReplacedMeanwhile(t *testing.T) {
	sessionPath := relinkTestSessionPath(t)
	SeedLinkedDeviceForTest(t, sessionPath, relinkTestJID)
	b := openRelinkTestBridge(t, sessionPath)
	restoreDeletes := failPurgeDeletes(t)
	b.handleEvent(&waevents.LoggedOut{Reason: waevents.ConnectFailureLoggedOut})
	restoreDeletes()
	revoked := readStoredIdentity(t, sessionPath, relinkTestJID)

	original := testHookBeforeRevokedDelete
	t.Cleanup(func() { testHookBeforeRevokedDelete = original })
	testHookBeforeRevokedDelete = func() {
		testHookBeforeRevokedDelete = nil
		deleteStoredDevice(t, sessionPath, relinkTestJID)
		SeedLinkedDeviceForTest(t, sessionPath, relinkTestJID)
	}
	b.mu.Lock()
	removed, err := b.purgeLoggedOutIdentityLocked()
	b.mu.Unlock()
	if err != nil || removed {
		t.Fatalf("purgeLoggedOutIdentityLocked() = %v, %v; want the replaced row spared", removed, err)
	}
	replacement := readStoredIdentity(t, sessionPath, relinkTestJID)
	if replacement.empty() || replacement.Key == revoked.Key {
		t.Fatalf("stored identity = %+v, want the replacement (not %+v)", replacement, revoked)
	}
	if status := b.Status(); !status.Paired || status.NeedsRelink {
		t.Fatalf("status = %+v, want the replacement paired and the record superseded", status)
	}
}

// A bridge in a process that does not own the live session (serve
// --no-transports sharing the store) never deletes an identity or writes the
// record, and its status follows the owner's writes without a restart.
func TestObservingProcessFollowsTheOwnerAndWritesNothing(t *testing.T) {
	sessionPath := relinkTestSessionPath(t)
	SeedLinkedDeviceForTest(t, sessionPath, relinkTestJID)
	owner := openRelinkTestBridge(t, sessionPath)
	restoreDeletes := failPurgeDeletes(t)
	owner.handleEvent(&waevents.LoggedOut{Reason: waevents.ConnectFailureLoggedOut})
	restoreDeletes()
	episode := storedRecord(t, sessionPath)
	if !episode.active() {
		t.Fatal("the owner did not store the logout")
	}

	observer := openObservingTestBridge(t, sessionPath)
	assertLinkedDevices(t, sessionPath, relinkTestJID.String())
	if got := storedRecord(t, sessionPath); !got.same(episode) {
		t.Fatalf("stored record after the observer opened = %+v, want %+v untouched", got, episode)
	}
	if status := observer.Status(); status.Paired || !status.NeedsRelink {
		t.Fatalf("observer status = %+v, want the revoked identity unpaired with needs_relink", status)
	}

	refuseDials(t)
	_ = owner.ConnectPaired(context.Background()) // the owner's purge
	assertLinkedDevices(t, sessionPath)
	if status := observer.Status(); status.Paired || !status.NeedsRelink {
		t.Fatalf("observer status after the purge = %+v, want needs_relink", status)
	}

	emulatePairingForTest(t, owner, relinkTestOtherJID)
	if status := observer.Status(); !status.Paired || status.NeedsRelink || status.AccountJID != relinkTestOtherJID.String() {
		t.Fatalf("observer status after the owner relinked = %+v, want the new link", status)
	}

	owner.handleEvent(&waevents.LoggedOut{OnConnect: true, Reason: waevents.ConnectFailureMainDeviceGone})
	if status := observer.Status(); status.Paired || !status.NeedsRelink || status.RelinkReason != "403: primary device was logged out" {
		t.Fatalf("observer status after the new link's logout = %+v, want that episode", status)
	}
}

// A clear is a compare-and-swap against the episode this process last
// stored: it never erases a different episode stored meanwhile, which it
// adopts instead.
func TestClearNeverErasesAnotherEpisode(t *testing.T) {
	sessionPath := relinkTestSessionPath(t)
	SeedLinkedDeviceForTest(t, sessionPath, relinkTestJID)
	b := openRelinkTestBridge(t, sessionPath)
	b.handleEvent(&waevents.LoggedOut{Reason: waevents.ConnectFailureLoggedOut})

	other := loggedOutState{
		At:     time.Now().Add(time.Minute).Round(0),
		Reason: "406: logged out for unknown reason",
		JID:    relinkTestOtherJID.String(),
		Key:    "another-identity-key",
	}
	database, err := sql.Open("sqlite", sessionStoreDSN(sessionPath))
	if err != nil {
		t.Fatalf("open session store: %v", err)
	}
	defer database.Close()
	if err := writeLoggedOutRecord(context.Background(), database, other); err != nil {
		t.Fatalf("store another episode: %v", err)
	}

	if err := b.clearLoggedOut(); err == nil {
		t.Fatal("clearLoggedOut() reported success over another episode")
	}
	if got := storedRecord(t, sessionPath); !got.same(other) {
		t.Fatalf("stored record = %+v, want the other episode %+v kept", got, other)
	}
	if got := b.loggedOutSnapshot(); !got.same(other) {
		t.Fatalf("record in memory = %+v, want the other episode adopted", got)
	}
}

// whatsmeow saves a pairing and dispatches PairSuccess from its own
// goroutine, independently of LoggedOut. A logout that overtakes PairSuccess
// must not leave a revoked or deleted identity reading as paired, whether
// whatsmeow's own delete then failed, both deletes failed, or whatsmeow's
// delete succeeded.
func TestLogoutBeforePairSuccessIsNotUndone(t *testing.T) {
	for _, test := range []struct {
		name            string
		whatsmeowDelete bool // whatsmeow's own delete succeeds before the event
		purgeFails      bool
		wantRelink      bool
	}{
		{name: "whatsmeow's delete failed", wantRelink: true},
		{name: "both deletes failed", purgeFails: true, wantRelink: true},
		{name: "whatsmeow's delete succeeded", whatsmeowDelete: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			sessionPath := relinkTestSessionPath(t)
			b := openRelinkTestBridge(t, sessionPath)
			source, generation := currentGeneration(b)
			device := emulatePairingSaveForTest(t, b, relinkTestOtherJID)
			if test.whatsmeowDelete {
				if err := device.Delete(context.Background()); err != nil {
					t.Fatalf("device.Delete(): %v", err)
				}
			}
			var restoreDeletes func()
			if test.purgeFails {
				restoreDeletes = failPurgeDeletes(t)
			}

			b.handleClientEvent(source, generation, &waevents.LoggedOut{Reason: waevents.ConnectFailureLoggedOut})
			b.handleClientEvent(source, generation, &waevents.PairSuccess{ID: relinkTestOtherJID})

			status := b.Status()
			if status.Paired || status.NeedsRelink != test.wantRelink {
				t.Fatalf("status = %+v, want unpaired with needs_relink=%v", status, test.wantRelink)
			}
			if !heldIdentity(b).empty() {
				t.Fatalf("held identity = %+v, want none installed", heldIdentity(b))
			}
			if restoreDeletes != nil {
				restoreDeletes()
			}
			refuseDials(t)
			if err := b.ConnectPaired(context.Background()); !errors.Is(err, whatsmeow.ErrNotLoggedIn) {
				t.Fatalf("ConnectPaired() error = %v, want ErrNotLoggedIn", err)
			}
			assertLinkedDevices(t, sessionPath)
		})
	}
}

// A PairSuccess or Connected from a client replaced while the event was in
// flight must not end the successor's own pairing attempt.
func TestRetiredPairSuccessAndConnectedLeaveANewPairingAlone(t *testing.T) {
	for _, event := range []struct {
		name string
		raw  any
	}{
		{"PairSuccess", &waevents.PairSuccess{ID: relinkTestJID}},
		{"Connected", &waevents.Connected{}},
	} {
		t.Run(event.name, func(t *testing.T) {
			sessionPath := relinkTestSessionPath(t)
			b := openRelinkTestBridgeWithStore(t, sessionPath)
			StubConnectForTest(t, func(context.Context, *whatsmeow.Client) error {
				return errors.New("offline")
			})
			source, generation := currentGeneration(b)
			emulatePairingSaveForTest(t, b, relinkTestJID)

			original := testHookAfterAdmission
			t.Cleanup(func() { testHookAfterAdmission = original })
			testHookAfterAdmission = func(any) {
				testHookAfterAdmission = nil
				if err := b.Unpair(); err != nil {
					t.Errorf("Unpair(): %v", err)
				}
				if _, err := b.PrepareQRPairing(context.Background()); err != nil {
					t.Errorf("PrepareQRPairing(): %v", err)
				}
			}
			b.handleClientEvent(source, generation, event.raw)

			if status := b.Status(); !status.Pairing || !status.Connecting || status.Paired {
				t.Fatalf("status = %+v, want the successor's pairing attempt still running", status)
			}
		})
	}
}

// A PairSuccess or Connected from a client replaced while the event was in
// flight (an unpair, possibly followed at once by a new link) must not
// install its identity on the successor or mark the successor connected.
func TestRetiredPairSuccessAndConnectedAreIgnored(t *testing.T) {
	for _, event := range []struct {
		name string
		raw  any
	}{
		{"PairSuccess", &waevents.PairSuccess{ID: relinkTestJID}},
		{"Connected", &waevents.Connected{}},
	} {
		for _, relink := range []bool{false, true} {
			name := event.name + "/unpair"
			if relink {
				name += " then a new link"
			}
			t.Run(name, func(t *testing.T) {
				sessionPath := relinkTestSessionPath(t)
				b := openRelinkTestBridgeWithStore(t, sessionPath)
				StubConnectForTest(t, func(context.Context, *whatsmeow.Client) error {
					return errors.New("offline")
				})
				source, generation := currentGeneration(b)
				emulatePairingSaveForTest(t, b, relinkTestJID)

				original := testHookAfterAdmission
				t.Cleanup(func() { testHookAfterAdmission = original })
				testHookAfterAdmission = func(any) {
					testHookAfterAdmission = nil
					if err := b.Unpair(); err != nil {
						t.Errorf("Unpair(): %v", err)
					}
					if relink {
						emulatePairingForTest(t, b, relinkTestOtherJID)
					}
				}
				b.handleClientEvent(source, generation, event.raw)

				status := b.Status()
				if relink {
					if !status.Paired || status.AccountJID != relinkTestOtherJID.String() || heldIdentity(b).JID != relinkTestOtherJID {
						t.Fatalf("status = %+v (held %+v), want only the new link", status, heldIdentity(b))
					}
				} else if status.Paired || !heldIdentity(b).empty() {
					t.Fatalf("status = %+v (held %+v), want the unpair intact", status, heldIdentity(b))
				}
				if status.Connected || b.connectedForTest() {
					t.Fatal("a retired client's event marked its successor connected")
				}
			})
		}
	}
}

// Connected adopts a link whose PairSuccess has not arrived from the store,
// never from the live device (whose ID whatsmeow clears from its own
// goroutine), and never a link a logout revoked.
func TestConnectedAdoptsFromTheStoreNotTheLiveDevice(t *testing.T) {
	t.Run("live ID cleared, store still holds the link", func(t *testing.T) {
		sessionPath := relinkTestSessionPath(t)
		b := openRelinkTestBridgeWithStore(t, sessionPath)
		device := emulatePairingSaveForTest(t, b, relinkTestOtherJID)
		b.mu.Lock()
		device.ID = nil
		b.mu.Unlock()
		b.handleEvent(&waevents.Connected{})
		if got, want := heldIdentity(b), readStoredIdentity(t, sessionPath, relinkTestOtherJID); got != want {
			t.Fatalf("held identity = %+v, want the stored link %+v", got, want)
		}
	})
	t.Run("revoked link is not adopted", func(t *testing.T) {
		sessionPath := relinkTestSessionPath(t)
		b := openRelinkTestBridgeWithStore(t, sessionPath)
		source, generation := currentGeneration(b)
		emulatePairingSaveForTest(t, b, relinkTestOtherJID)
		restoreDeletes := failPurgeDeletes(t)
		b.handleClientEvent(source, generation, &waevents.LoggedOut{Reason: waevents.ConnectFailureLoggedOut})
		b.handleClientEvent(source, generation, &waevents.Connected{})
		restoreDeletes()
		if !heldIdentity(b).empty() {
			t.Fatalf("held identity = %+v, want the revoked link not adopted", heldIdentity(b))
		}
		if status := b.Status(); status.Paired || !status.NeedsRelink || status.Connected {
			t.Fatalf("status = %+v, want unpaired, not connected, needs_relink", status)
		}
	})
}

// A process holding a revoked identity sees a replacement another process
// stored, without a restart: a different JID, or the same JID under new keys.
func TestStoredReplacementIsSeenWithoutARestart(t *testing.T) {
	for _, test := range []struct {
		name string
		jid  watypes.JID
	}{
		{"different JID", relinkTestOtherJID},
		{"same JID, new keys", relinkTestJID},
	} {
		t.Run(test.name, func(t *testing.T) {
			sessionPath := relinkTestSessionPath(t)
			SeedLinkedDeviceForTest(t, sessionPath, relinkTestJID)
			b := openRelinkTestBridge(t, sessionPath)
			restoreDeletes := failPurgeDeletes(t)
			b.handleEvent(&waevents.LoggedOut{Reason: waevents.ConnectFailureLoggedOut})
			restoreDeletes()
			if status := b.Status(); status.Paired || !status.NeedsRelink {
				t.Fatalf("status = %+v, want the revoked identity unpaired with needs_relink", status)
			}

			deleteStoredDevice(t, sessionPath, relinkTestJID)
			SeedLinkedDeviceForTest(t, sessionPath, test.jid)
			status := b.Status()
			if !status.Paired || status.NeedsRelink || status.AccountJID != test.jid.String() {
				t.Fatalf("status = %+v, want the stored replacement paired without needs_relink", status)
			}
			if account := b.PairedAccount(); !account.Paired || account.JID != test.jid.String() {
				t.Fatalf("PairedAccount() = %+v, want the replacement", account)
			}
		})
	}
}

func (b *Bridge) connectedForTest() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.connected
}
