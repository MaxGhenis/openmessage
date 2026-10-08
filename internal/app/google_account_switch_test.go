package app

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/events"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
	"google.golang.org/protobuf/proto"

	"github.com/maxghenis/openmessage/internal/client"
)

const switchTestAccount = "owner@example.com"

// accountSwitchTestApp is a paired App (a session file exists) that counts
// status emissions.
func accountSwitchTestApp(t *testing.T) (*App, *atomic.Int32) {
	t.Helper()
	t.Setenv("OPENMESSAGE_GOOGLE_AVATAR_SYNC", "0")
	sessionPath := filepath.Join(t.TempDir(), "session.json")
	if err := client.SaveSession(sessionPath, &client.SessionData{AuthDataJSON: []byte(`{}`)}); err != nil {
		t.Fatalf("SaveSession(): %v", err)
	}
	var emits atomic.Int32
	a := &App{SessionPath: sessionPath, Logger: zerolog.Nop()}
	a.OnStatusChange = func(bool) { emits.Add(1) }
	return a, &emits
}

func qrSessionClient(t *testing.T, browserID string) *client.Client {
	t.Helper()
	cli, err := client.NewFromSession(
		&client.SessionData{AuthDataJSON: []byte(`{"browser":{"sourceID":"` + browserID + `"}}`)},
		zerolog.Nop(),
	)
	if err != nil {
		t.Fatalf("NewFromSession(): %v", err)
	}
	if cli.GM.AuthData.IsGoogleAccount() {
		t.Fatal("QR fixture is a Google-account session")
	}
	return cli
}

func googleAccountSessionClient(t *testing.T) *client.Client {
	t.Helper()
	cli, err := client.NewFromSession(
		&client.SessionData{AuthDataJSON: []byte(`{"dest_reg_id":"6f1c2c1e-6b0a-4d4a-9a55-0d1b5b2f9c11"}`)},
		zerolog.Nop(),
	)
	if err != nil {
		t.Fatalf("NewFromSession(): %v", err)
	}
	if !cli.GM.AuthData.IsGoogleAccount() {
		t.Fatal("Google-account fixture is a QR session")
	}
	return cli
}

func fakeAccountChange(account string) *events.AccountChange {
	return &events.AccountChange{
		AccountChangeOrSomethingEvent: &gmproto.AccountChangeOrSomethingEvent{Account: account},
		IsFake:                        true,
	}
}

func realAccountChange(account string, enabled bool) *events.AccountChange {
	return &events.AccountChange{
		AccountChangeOrSomethingEvent: &gmproto.AccountChangeOrSomethingEvent{
			Account: account,
			Enabled: enabled,
		},
	}
}

func TestGoogleAccountChangeRaisesFlagOnRisingEdgeOnly(t *testing.T) {
	a, emits := accountSwitchTestApp(t)
	generation := a.BeginGoogleGeneration(qrSessionClient(t, "browser-a"))
	t.Cleanup(generation.Release)
	if !generation.Ready() {
		t.Fatal("generation did not become ready")
	}
	connected := a.Connected.Load()
	emits.Store(0)

	before := time.Now().UnixMilli()
	generation.Handler.Handle(fakeAccountChange(switchTestAccount))
	after := time.Now().UnixMilli()

	status := a.GoogleStatus()
	if !status.AccountSwitched || status.SwitchedAccount != switchTestAccount {
		t.Fatalf("status = %+v, want account switch for %s", status, switchTestAccount)
	}
	if status.AccountSwitchedAtMS < before || status.AccountSwitchedAtMS > after {
		t.Fatalf("AccountSwitchedAtMS = %d, want within [%d, %d]", status.AccountSwitchedAtMS, before, after)
	}
	if emits.Load() != 1 {
		t.Fatalf("status emissions = %d, want 1 on the rising edge", emits.Load())
	}
	// I3: the account switch never changes Connected or sets needs_repair.
	if a.Connected.Load() != connected || status.NeedsRepair || a.googleNeedsRepair.Load() {
		t.Fatalf("account switch changed connection state: connected %v -> %v, needs_repair %v",
			connected, a.Connected.Load(), status.NeedsRepair)
	}
	if switched, account := a.GoogleAccountSwitch(); !switched || account != switchTestAccount {
		t.Fatalf("GoogleAccountSwitch() = (%v, %q), want (true, %s)", switched, account, switchTestAccount)
	}

	// The phone repeats the container; that is not a new edge.
	firstAt := status.AccountSwitchedAtMS
	generation.Handler.Handle(fakeAccountChange(switchTestAccount))
	a.NoteGoogleAccountSwitch(generation.Client, switchTestAccount)
	if emits.Load() != 1 {
		t.Fatalf("status emissions after repeats = %d, want 1", emits.Load())
	}
	if got := a.GoogleStatus().AccountSwitchedAtMS; got != firstAt {
		t.Fatalf("AccountSwitchedAtMS moved from %d to %d on a repeat", firstAt, got)
	}

	// A real event that turns Google-account pairing off clears it.
	generation.Handler.Handle(realAccountChange(switchTestAccount, false))
	if a.GoogleStatus().AccountSwitched {
		t.Fatal("a non-fake Enabled=false event did not clear the account switch")
	}
	if emits.Load() != 2 {
		t.Fatalf("status emissions after clearing = %d, want 2", emits.Load())
	}
	a.ClearGoogleAccountSwitch()
	if emits.Load() != 2 {
		t.Fatalf("clearing an unset flag emitted status (%d emissions, want 2)", emits.Load())
	}
}

// switched := Enabled || IsFake, the upstream mautrix-gmessages rule.
func TestGoogleAccountChangeFollowsUpstreamSwitchRule(t *testing.T) {
	for _, test := range []struct {
		enabled, fake, want bool
	}{
		{enabled: false, fake: false, want: false},
		{enabled: true, fake: false, want: true},
		{enabled: false, fake: true, want: true},
		{enabled: true, fake: true, want: true},
	} {
		a, _ := accountSwitchTestApp(t)
		generation := a.BeginGoogleGeneration(qrSessionClient(t, "browser-a"))
		generation.Handler.Handle(&events.AccountChange{
			AccountChangeOrSomethingEvent: &gmproto.AccountChangeOrSomethingEvent{
				Account: switchTestAccount,
				Enabled: test.enabled,
			},
			IsFake: test.fake,
		})
		if got := a.GoogleStatus().AccountSwitched; got != test.want {
			t.Fatalf("enabled=%v fake=%v: AccountSwitched = %v, want %v", test.enabled, test.fake, got, test.want)
		}
		generation.Release()
	}
}

// I3: the flag is never set for a Google-account session, from either source.
func TestGoogleAccountSessionNeverRaisesAccountSwitch(t *testing.T) {
	a, emits := accountSwitchTestApp(t)
	cli := googleAccountSessionClient(t)
	generation := a.BeginGoogleGeneration(cli)
	t.Cleanup(generation.Release)
	emits.Store(0)

	generation.Handler.Handle(fakeAccountChange(switchTestAccount))
	generation.Handler.Handle(realAccountChange(switchTestAccount, true))
	a.NoteGoogleAccountSwitch(cli, switchTestAccount)

	if switched, _ := a.GoogleAccountSwitch(); switched || a.GoogleStatus().AccountSwitched {
		t.Fatal("a Google-account session raised the account switch")
	}
	if emits.Load() != 0 {
		t.Fatalf("status emissions = %d, want 0", emits.Load())
	}
	// Missing auth data never raises it either.
	a.NoteGoogleAccountSwitch(nil, switchTestAccount)
	a.NoteGoogleAccountSwitch(&client.Client{}, switchTestAccount)
	if switched, _ := a.GoogleAccountSwitch(); switched {
		t.Fatal("a client without auth data raised the account switch")
	}
}

func TestStaleGoogleGenerationAccountChangeIsIgnored(t *testing.T) {
	a, _ := accountSwitchTestApp(t)
	stale := a.BeginGoogleGeneration(qrSessionClient(t, "browser-a"))
	current := a.BeginGoogleGeneration(qrSessionClient(t, "browser-a"))
	t.Cleanup(current.Release)

	stale.Handler.Handle(fakeAccountChange(switchTestAccount))
	if a.GoogleStatus().AccountSwitched {
		t.Fatal("a stale generation's AccountChange raised the account switch")
	}
	current.Handler.Handle(fakeAccountChange(switchTestAccount))
	if !a.GoogleStatus().AccountSwitched {
		t.Fatal("the current generation's AccountChange was ignored")
	}
}

// The phone may not resend its account container on reconnect, so a new
// generation of the same session keeps the flag; a new pairing does not.
func TestGoogleAccountSwitchSurvivesReconnectButNotNewPairing(t *testing.T) {
	t.Run("same session reconnects", func(t *testing.T) {
		a, _ := accountSwitchTestApp(t)
		first := a.BeginGoogleGeneration(qrSessionClient(t, "browser-a"))
		first.Handler.Handle(fakeAccountChange(switchTestAccount))
		first.Release()
		if !a.GoogleStatus().AccountSwitched {
			t.Fatal("releasing the generation cleared the account switch")
		}
		second := a.BeginGoogleGeneration(qrSessionClient(t, "browser-a"))
		t.Cleanup(second.Release)
		if !second.Ready() {
			t.Fatal("second generation did not become ready")
		}
		if !a.GoogleStatus().AccountSwitched {
			t.Fatal("reconnecting the same session cleared the account switch")
		}
	})

	t.Run("new QR pairing", func(t *testing.T) {
		a, _ := accountSwitchTestApp(t)
		first := a.BeginGoogleGeneration(qrSessionClient(t, "browser-a"))
		first.Handler.Handle(fakeAccountChange(switchTestAccount))
		first.Release()
		second := a.BeginGoogleGeneration(qrSessionClient(t, "browser-b"))
		if a.GoogleStatus().AccountSwitched {
			t.Fatal("a new pairing inherited another session's account switch")
		}
		// Installing the new session forgets the old report outright, so it
		// cannot resurface while no client is installed between generations.
		second.Release()
		if switched, _ := a.GoogleAccountSwitch(); switched || a.GoogleStatus().AccountSwitched {
			t.Fatal("the old session's account switch resurfaced after the new pairing's generation ended")
		}
	})

	t.Run("new Google-account pairing", func(t *testing.T) {
		a, _ := accountSwitchTestApp(t)
		first := a.BeginGoogleGeneration(qrSessionClient(t, "browser-a"))
		first.Handler.Handle(fakeAccountChange(switchTestAccount))
		first.Release()
		second := a.BeginGoogleGeneration(googleAccountSessionClient(t))
		t.Cleanup(second.Release)
		if a.GoogleStatus().AccountSwitched {
			t.Fatal("a Google-account pairing inherited the QR session's account switch")
		}
	})

	t.Run("installed client is another session", func(t *testing.T) {
		a, _ := accountSwitchTestApp(t)
		generation := a.BeginGoogleGeneration(qrSessionClient(t, "browser-a"))
		generation.Handler.Handle(fakeAccountChange(switchTestAccount))
		// Any install path that bypasses BeginGoogleGeneration still never
		// reports another session's switch.
		a.setClient(qrSessionClient(t, "browser-b"))
		if switched, _ := a.GoogleAccountSwitch(); switched || a.GoogleStatus().AccountSwitched {
			t.Fatal("the switch was reported for a different installed session")
		}
	})
}

// A send that started on a replaced session can answer after another session
// was installed. Its report must not replace the installed session's state,
// while a report from a reconnect of the same session still counts.
func TestLateSendResponseFromReplacedSessionIsIgnored(t *testing.T) {
	a, _ := accountSwitchTestApp(t)
	old := qrSessionClient(t, "browser-a")
	first := a.BeginGoogleGeneration(old)
	first.Release()
	current := a.BeginGoogleGeneration(qrSessionClient(t, "browser-b"))
	t.Cleanup(current.Release)
	current.Handler.Handle(fakeAccountChange("current@example.com"))

	a.NoteGoogleAccountSwitch(old, "stale@example.com")
	if switched, account := a.GoogleAccountSwitch(); !switched || account != "current@example.com" {
		t.Fatalf("GoogleAccountSwitch() = (%v, %q), want the installed session's report kept", switched, account)
	}

	// Another client object for the installed session (a reconnect) is the
	// same session, so its report is applied.
	a.ClearGoogleAccountSwitch()
	a.NoteGoogleAccountSwitch(qrSessionClient(t, "browser-b"), "current@example.com")
	if switched, _ := a.GoogleAccountSwitch(); !switched {
		t.Fatal("a report from another client of the installed session was ignored")
	}
}

func TestGoogleAccountSwitchClearedByUnpairAndSessionInvalid(t *testing.T) {
	t.Run("unpair", func(t *testing.T) {
		a, _ := accountSwitchTestApp(t)
		generation := a.BeginGoogleGeneration(qrSessionClient(t, "browser-a"))
		generation.Handler.Handle(fakeAccountChange(switchTestAccount))
		generation.Release()
		if err := a.Unpair(); err != nil {
			t.Fatalf("Unpair(): %v", err)
		}
		if switched, _ := a.GoogleAccountSwitch(); switched {
			t.Fatal("Unpair did not clear the account switch")
		}
	})

	t.Run("session invalid", func(t *testing.T) {
		a, _ := accountSwitchTestApp(t)
		generation := a.BeginGoogleGeneration(qrSessionClient(t, "browser-a"))
		t.Cleanup(generation.Release)
		generation.Handler.Handle(fakeAccountChange(switchTestAccount))
		generation.Handler.Handle(&events.GaiaLoggedOut{})
		if switched, _ := a.GoogleAccountSwitch(); switched {
			t.Fatal("an invalidated session kept the account switch")
		}
	})

	t.Run("status hides it once unpaired", func(t *testing.T) {
		a, _ := accountSwitchTestApp(t)
		generation := a.BeginGoogleGeneration(qrSessionClient(t, "browser-a"))
		t.Cleanup(generation.Release)
		generation.Handler.Handle(fakeAccountChange(switchTestAccount))
		if err := os.Remove(a.SessionPath); err != nil {
			t.Fatal(err)
		}
		status := a.GoogleStatus()
		if status.AccountSwitched || status.SwitchedAccount != "" || status.AccountSwitchedAtMS != 0 {
			t.Fatalf("unpaired status = %+v, want no account switch fields", status)
		}
	})
}

// The legacy (non-v2) LoadAndConnect path wires the same callback, guarded
// against a replaced client.
func TestLegacyGoogleCallbackAppliesAccountChange(t *testing.T) {
	a := newGoogleCallbackTestApp(t)
	a.Connected.Store(true)
	a.EventHandler.Handle(fakeAccountChange(switchTestAccount))
	if !a.GoogleStatus().AccountSwitched {
		t.Fatal("legacy handler ignored AccountChange")
	}
	if !a.Connected.Load() || a.GoogleStatus().NeedsRepair {
		t.Fatal("legacy AccountChange changed Connected or set needs_repair")
	}

	// A replaced client's late event is ignored.
	a.ClearGoogleAccountSwitch()
	staleHandler := a.EventHandler
	a.setClient(qrSessionClient(t, ""))
	staleHandler.Handle(fakeAccountChange(switchTestAccount))
	if switched, _ := a.GoogleAccountSwitch(); switched {
		t.Fatal("a replaced legacy client's AccountChange raised the account switch")
	}
}

// End to end through the pinned libgm: a GET_UPDATES frame whose only payload
// is the encrypted account container (the shape seen at daemon start in the
// incident) reaches the installed callback as AccountChange{IsFake: true}.
func TestLibgmAccountContainerFrameRaisesAccountSwitch(t *testing.T) {
	a := newGoogleCallbackTestApp(t)
	a.Connected.Store(true)
	cli := a.GetClient()

	container, err := proto.Marshal(&gmproto.EncryptedData2Container{
		AccountChange: &gmproto.AccountChangeOrSomethingEvent{Account: switchTestAccount},
	})
	if err != nil {
		t.Fatalf("marshal account container: %v", err)
	}
	encrypted, err := cli.GM.AuthData.RequestCrypto.Encrypt(container)
	if err != nil {
		t.Fatalf("encrypt account container: %v", err)
	}
	messageData, err := proto.Marshal(&gmproto.RPCMessageData{
		Action:         gmproto.ActionType_GET_UPDATES,
		EncryptedData2: encrypted,
	})
	if err != nil {
		t.Fatalf("marshal frame: %v", err)
	}
	cli.GM.HandleRPCMsg(&gmproto.IncomingRPCMessage{
		ResponseID:  "account-container-fixture",
		BugleRoute:  gmproto.BugleRoute_DataEvent,
		MessageData: messageData,
	})

	status := a.GoogleStatus()
	if !status.AccountSwitched || status.SwitchedAccount != switchTestAccount {
		t.Fatalf("status = %+v, want account switch for %s", status, switchTestAccount)
	}
	if !a.Connected.Load() || status.NeedsRepair {
		t.Fatal("the account container changed Connected or set needs_repair")
	}
}
