package whatsapplive_test

import (
	"context"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/whatsmeow"
	waBinary "go.mau.fi/whatsmeow/binary"
	watypes "go.mau.fi/whatsmeow/types"
	waevents "go.mau.fi/whatsmeow/types/events"

	"github.com/maxghenis/openmessage/internal/bridge"
	whatsappadapter "github.com/maxghenis/openmessage/internal/bridgeadapters/whatsapp"
	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/whatsapplive"
)

const lifecycleTestTimeout = 5 * time.Second

type lifecycleTestClock struct{}

func (lifecycleTestClock) Now() time.Time { return time.Now() }

func (lifecycleTestClock) NewTimer(delay time.Duration) bridge.Timer {
	return lifecycleTestTimer{timer: time.NewTimer(delay)}
}

type lifecycleTestTimer struct{ timer *time.Timer }

func (t lifecycleTestTimer) C() <-chan time.Time { return t.timer.C }
func (t lifecycleTestTimer) Stop() bool          { return t.timer.Stop() }

type lifecycleTestRandom struct{}

func (lifecycleTestRandom) Int63n(n int64) int64 { return n / 2 }

func awaitLifecycle(t *testing.T, description string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(lifecycleTestTimeout)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", description)
		}
		runtime.Gosched()
		time.Sleep(time.Millisecond)
	}
}

// TestRemoteLogoutLeavesTheAccountReadyToRelink replays 2026-08-20 through the
// real bridge, lifecycle adapter, supervisor and whatsmeow's own 401 handling,
// with only the dial stubbed. Whether whatsmeow's delete loses to the
// adapter's cancel (forced here by handing it an ended context, the shape that
// left the revoked identity behind) or races it, the account must end blocked
// for reauthentication with needs_relink set, not paired, with no identity
// stored, and able to start QR pairing at once, without an unpair.
func TestRemoteLogoutLeavesTheAccountReadyToRelink(t *testing.T) {
	for _, test := range []struct {
		name        string
		deleteLoses bool
	}{
		{name: "whatsmeow's delete loses to the adapter's cancel", deleteLoses: true},
		{name: "whatsmeow's delete races the adapter's cancel", deleteLoses: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			sessionPath := filepath.Join(dir, "whatsapp-session.db")
			jid := watypes.JID{User: "15551230000", Device: 7, Server: watypes.DefaultUserServer}
			whatsapplive.SeedLinkedDeviceForTest(t, sessionPath, jid)
			store, err := db.New(filepath.Join(dir, "messages.db"))
			if err != nil {
				t.Fatalf("db.New(): %v", err)
			}
			t.Cleanup(func() { _ = store.Close() })
			host, err := whatsapplive.New(sessionPath, store, zerolog.Nop(), whatsapplive.Callbacks{})
			if err != nil {
				t.Fatalf("whatsapplive.New(): %v", err)
			}
			t.Cleanup(func() { _ = host.Close() })

			dialed := make(chan context.Context, 1)
			whatsapplive.StubConnectForTest(t, func(ctx context.Context, _ *whatsmeow.Client) error {
				dialed <- ctx
				return nil
			})
			adapter := whatsappadapter.New("whatsapp-primary", host)
			supervisor, err := bridge.NewSupervisor(
				"whatsapp-primary",
				bridge.PlatformWhatsApp,
				adapter,
				bridge.Policy{
					ConnectTimeout:     time.Minute,
					ProbeEvery:         time.Hour,
					ProbeTimeout:       time.Minute,
					LivenessTimeout:    time.Hour,
					MinBackoff:         time.Second,
					MaxBackoff:         time.Minute,
					MaxSameFingerprint: 3,
				},
				lifecycleTestClock{},
				lifecycleTestRandom{},
			)
			if err != nil {
				t.Fatalf("NewSupervisor(): %v", err)
			}
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), lifecycleTestTimeout)
				defer cancel()
				_ = supervisor.Stop(ctx)
			})

			startCtx, cancelStart := context.WithTimeout(context.Background(), lifecycleTestTimeout)
			defer cancelStart()
			if err := supervisor.Start(startCtx, bridge.StartRequest{}); err != nil {
				t.Fatalf("Supervisor.Start(): %v", err)
			}
			var eventCtx context.Context
			select {
			case eventCtx = <-dialed:
			case <-time.After(lifecycleTestTimeout):
				t.Fatal("the supervisor never dialed the linked identity")
			}
			cli := host.WhatsmeowClientForTest()
			cli.DangerousInternals().DispatchEvent(&waevents.Connected{})
			awaitLifecycle(t, "the generation to come online", func() bool {
				return supervisor.Snapshot().State == bridge.StateOnline
			})

			if test.deleteLoses {
				ended, cancel := context.WithCancel(eventCtx)
				cancel()
				eventCtx = ended
			}
			// whatsmeow's handling of <failure reason="401">: dispatch LoggedOut on
			// a goroutine, then delete the session with the event context.
			cli.DangerousInternals().HandleConnectFailure(eventCtx, &waBinary.Node{
				Tag:   "failure",
				Attrs: waBinary.Attrs{"reason": "401"},
			})

			awaitLifecycle(t, "the logout to settle", func() bool {
				snapshot := supervisor.Snapshot()
				status := host.Status()
				return snapshot.State == bridge.StateBlocked &&
					status.NeedsRelink && !status.Paired &&
					len(whatsapplive.LinkedDeviceJIDsForTest(t, sessionPath)) == 0
			})
			snapshot := supervisor.Snapshot()
			if snapshot.ErrorClass != bridge.FailureReauthRequired || snapshot.ErrorFingerprint != "whatsapp_logged_out" {
				t.Fatalf("supervisor = %+v, want reauth_required / whatsapp_logged_out", snapshot)
			}
			status := host.Status()
			if status.RelinkReason != "401: logged out from another device" || status.RelinkSinceMS == 0 {
				t.Fatalf("status = %+v, want the 401 reason and episode start", status)
			}
			// cmd's supervisor control reconnects a blocked account that reads as
			// unpaired by starting a fresh pairing; the bridge must accept one.
			if host.PairedAccount().Paired {
				t.Fatal("PairedAccount() still reports the revoked identity")
			}
			if _, err := host.PrepareQRPairing(context.Background()); err != nil {
				t.Fatalf("PrepareQRPairing() after the logout: %v", err)
			}
		})
	}
}
