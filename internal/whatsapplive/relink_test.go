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
	"testing"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/whatsmeow"
	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/store/sqlstore"
	watypes "go.mau.fi/whatsmeow/types"
	waevents "go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
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
	assertLinkedDevices(t, sessionPath)

	b.mu.RLock()
	replaced := b.client != source && b.client.Store.ID == nil && b.identityJID.IsEmpty() && b.clientGeneration > generation
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

// A logout recorded by a process that exited before its purge (or whose purge
// failed) leaves the revoked identity on disk. It must read as unpaired, must
// never be dialed, and must not block pairing.
func TestRevokedIdentityLeftOnDiskIsRemovedBeforeDialOrPairing(t *testing.T) {
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
			b.recordLoggedOut(time.Now(), relinkTestReason401, relinkTestJID)
			b = reopenRelinkTestBridge(t, b, sessionPath)

			assertLinkedDevices(t, sessionPath, relinkTestJID.String())
			if status := b.Status(); status.Paired || !status.NeedsRelink {
				t.Fatalf("status with the revoked identity on disk = %+v, want unpaired and needs_relink", status)
			}
			StubConnectForTest(t, func(context.Context, *whatsmeow.Client) error {
				t.Error("dialed WhatsApp with a revoked identity")
				return nil
			})

			entry.enter(t, b)
			assertLinkedDevices(t, sessionPath)
		})
	}
}

func TestLogoutEpisodeKeepsItsFirstTimeAndReason(t *testing.T) {
	b := openRelinkTestBridge(t, relinkTestSessionPath(t))
	first := time.Unix(1_787_260_699, 0)
	b.recordLoggedOut(first, relinkTestReason401, relinkTestJID)
	b.recordLoggedOut(first.Add(time.Hour), "403: primary device was logged out", relinkTestJID)
	if got := b.loggedOutSnapshot(); !got.At.Equal(first) || got.Reason != relinkTestReason401 {
		t.Fatalf("episode after a repeat = %+v, want the first logout kept", got)
	}

	second := first.Add(48 * time.Hour)
	b.recordLoggedOut(second, "406: logged out for unknown reason", relinkTestOtherJID)
	if got := b.loggedOutSnapshot(); !got.At.Equal(second) || got.JID != relinkTestOtherJID.String() {
		t.Fatalf("episode after another identity's logout = %+v, want that logout", got)
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
	b = reopenRelinkTestBridge(t, b, sessionPath)
	if status := b.Status(); !status.Paired || status.NeedsRelink {
		t.Fatalf("status after restart = %+v, want the new link without needs_relink", status)
	}
	assertLinkedDevices(t, sessionPath, relinkTestOtherJID.String())
}

func TestUnpairEndsTheLogoutWithoutDialingTheRevokedIdentity(t *testing.T) {
	sessionPath := relinkTestSessionPath(t)
	SeedLinkedDeviceForTest(t, sessionPath, relinkTestJID)
	b := openRelinkTestBridge(t, sessionPath)
	// The purge has not run: the client still holds the revoked identity.
	b.recordLoggedOut(time.Now(), relinkTestReason401, relinkTestJID)

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
	assertLinkedDevices(t, sessionPath)
	b = reopenRelinkTestBridge(t, b, sessionPath)
	if b.Status().NeedsRelink {
		t.Fatal("needs_relink came back after a restart; Unpair() did not persist the clear")
	}
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
	if status := b.Status(); status.NeedsRelink || !status.Paired {
		t.Fatalf("status = %+v, want a retired generation's logout ignored", status)
	}
	assertLinkedDevices(t, sessionPath, relinkTestJID.String())
}

func TestPurgeRemovesOnlyTheRevokedIdentity(t *testing.T) {
	sessionPath := relinkTestSessionPath(t)
	SeedLinkedDeviceForTest(t, sessionPath, relinkTestJID)
	b := openRelinkTestBridge(t, sessionPath)
	b.recordLoggedOut(time.Now(), relinkTestReason401, relinkTestOtherJID)

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
	if status := b.Status(); !status.Paired || status.AccountJID != relinkTestJID.String() {
		t.Fatalf("status = %+v, want the unrevoked identity still paired", status)
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

	b.recordLoggedOut(time.Now(), relinkTestReason401, relinkTestJID)
	b = reopenRelinkTestBridge(t, b, sessionPath)
	if status := b.Status(); !status.NeedsRelink || status.RelinkReason != relinkTestReason401 {
		t.Fatalf("status after restart = %+v, want the logout recorded in the upgraded table", status)
	}
	if got := b.TemporaryBanDeadline(); !got.Equal(banUntil) {
		t.Fatalf("TemporaryBanDeadline() after restart = %v, want %v", got, banUntil)
	}
}

// TestRelinkStateMachineInvariants drives random sequences of lifecycle
// events, pairings, unpairs and restarts through a real Bridge and session
// store, checking after every step, against a model:
//
//	R1 needs_relink holds exactly while a logout of a linked identity is
//	   outstanding (no PairSuccess or Unpair since), and relink_reason and
//	   relink_since_ms stay those of the episode's first logout.
//	R2 needs_relink implies not paired.
//	R3 Status().Paired and PairedAccount().Paired agree, and are true exactly
//	   while a linked identity WhatsApp has not revoked is stored; account_jid
//	   names it.
//	R4 the session store holds exactly that identity: a logout leaves no
//	   revoked identity behind, and nothing else is ever deleted.
//	R5 R1-R4 hold across restarts.
func TestRelinkStateMachineInvariants(t *testing.T) {
	const (
		cases    = 40
		maxSteps = 12
	)
	type action int
	const (
		actLogout        action = iota // LoggedOut 401/403/406
		actLogoutFailure               // ConnectFailure with a logout reason
		actOtherEvent                  // lifecycle events that are not logouts
		actPair                        // a new link completes
		actUnpair                      // explicit unpair
		actRestart                     // close and reopen the bridge
		actionCount
	)
	logoutReasons := []waevents.ConnectFailureReason{
		waevents.ConnectFailureLoggedOut,
		waevents.ConnectFailureMainDeviceGone,
		waevents.ConnectFailureUnknownLogout,
	}
	otherEvents := []func() any{
		func() any { return &waevents.Disconnected{} },
		func() any { return &waevents.StreamReplaced{} },
		func() any { return &waevents.ClientOutdated{} },
		func() any { return &waevents.ManualLoginReconnect{} },
		func() any { return &waevents.KeepAliveTimeout{ErrorCount: 1} },
		func() any {
			return &waevents.TemporaryBan{Code: waevents.TempBanSentTooManySameMessage, Expire: time.Hour}
		},
		func() any { return &waevents.ConnectFailure{Reason: waevents.ConnectFailureGeneric} },
		func() any { return &waevents.ConnectFailure{Reason: waevents.ConnectFailureServiceUnavailable} },
	}

	// Unpair's remote logout attempt fails offline, so it falls back to the
	// local delete, the path that runs when the network is down.
	StubConnectForTest(t, func(context.Context, *whatsmeow.Client) error {
		return errors.New("offline")
	})

	type model struct {
		linked   string // identity WhatsApp still accepts, "" if none
		relink   bool
		reason   string
		sinceMS  int64 // captured on the step that opened the episode
		openedAt int64
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
		var m model
		if r.IntN(2) == 0 {
			jid := newJID()
			SeedLinkedDeviceForTest(t, sessionPath, jid)
			m.linked = jid.String()
		}
		b := openRelinkTestBridge(t, sessionPath)

		check := func(step string) {
			t.Helper()
			status := b.Status()
			// R1
			if status.NeedsRelink != m.relink {
				t.Fatalf("case %d %s: needs_relink = %v, model %v (status %+v)", c, step, status.NeedsRelink, m.relink, status)
			}
			if m.relink {
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
			if status.Paired != (m.linked != "") || status.AccountJID != m.linked {
				t.Fatalf("case %d %s: paired=%v account=%q, model linked %q", c, step, status.Paired, status.AccountJID, m.linked)
			}
			// R4
			var want []string
			if m.linked != "" {
				want = []string{m.linked}
			}
			if got := LinkedDeviceJIDsForTest(t, sessionPath); !reflect.DeepEqual(got, want) {
				t.Fatalf("case %d %s: stored identities %v, model %v", c, step, got, want)
			}
		}
		check("open")

		steps := 1 + r.IntN(maxSteps)
		for s := 0; s < steps; s++ {
			act := action(r.IntN(int(actionCount)))
			actionCounts[act]++
			step := fmt.Sprintf("step %d (action %d)", s, act)
			switch act {
			case actLogout, actLogoutFailure:
				reason := logoutReasons[r.IntN(len(logoutReasons))]
				var event any
				var description string
				if act == actLogout {
					evt := &waevents.LoggedOut{OnConnect: r.IntN(2) == 0, Reason: reason}
					event, description = evt, evt.PermanentDisconnectDescription()
				} else {
					evt := &waevents.ConnectFailure{Reason: reason}
					event, description = evt, evt.PermanentDisconnectDescription()
				}
				openedAt := time.Now().UnixMilli()
				b.handleEvent(event)
				if m.linked == "" {
					branches["logout with no link"]++
					break
				}
				branches["logout of a link"]++
				if m.relink {
					branches["logout while an episode is open"]++
				} else {
					m.relink, m.reason, m.sinceMS, m.openedAt = true, description, 0, openedAt
				}
				m.linked = ""
			case actOtherEvent:
				b.handleEvent(otherEvents[r.IntN(len(otherEvents))]())
				if m.linked != "" {
					branches["other event on a link"]++
				}
			case actPair:
				if m.linked != "" {
					branches["pair skipped: already linked"]++
					break
				}
				if m.relink {
					branches["relink after logout"]++
				} else {
					branches["first link"]++
				}
				jid := newJID()
				emulatePairingForTest(t, b, jid)
				m = model{linked: jid.String()}
			case actUnpair:
				if m.relink {
					branches["unpair during an episode"]++
				}
				if err := b.Unpair(); err != nil {
					t.Fatalf("case %d %s: Unpair(): %v", c, step, err)
				}
				m = model{}
			case actRestart:
				if m.relink {
					branches["restart during an episode"]++
				}
				b = reopenRelinkTestBridge(t, b, sessionPath)
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
		"other event on a link",
		"first link",
		"relink after logout",
		"pair skipped: already linked",
		"unpair during an episode",
		"restart during an episode",
	} {
		if branches[branch] == 0 {
			t.Fatalf("branch %q never ran (branches %v)", branch, branches)
		}
	}
	t.Logf("actions %v, branches %v", actionCounts, branches)
}
