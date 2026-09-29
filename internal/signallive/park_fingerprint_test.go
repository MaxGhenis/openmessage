package signallive

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"testing"
	"testing/quick"
	"time"

	"github.com/rs/zerolog"
)

const parkTestAccount = "+15551230000"

// parkScenario drives one bridge into one terminal park through the real
// receive lifecycle (StartPoller), with signal-cli stubbed.
type parkScenario struct {
	name string
	// storedAccount writes accounts.json listing the account (the unreadable
	// streak's corroboration).
	storedAccount bool
	version       string
	listAccounts  func() ([]byte, error)
	receive       func() ([]byte, error)
	// generations is how many StartPoller runs it takes to park.
	generations int
	wantKind    PollerFailureKind
	wantFP      string
}

func parkScenarios() []parkScenario {
	listed := func() ([]byte, error) { return []byte(`[{"number":"` + parkTestAccount + `"}]`), nil }
	return []parkScenario{
		{
			name:         "version gate",
			version:      "signal-cli 0.14.4\n",
			listAccounts: listed,
			generations:  1,
			wantKind:     PollerFailureUpgrade,
			wantFP:       SignalCLIVersionFingerprint,
		},
		{
			name:         "poison envelope",
			version:      "signal-cli 0.14.5\n",
			listAccounts: listed,
			receive: func() ([]byte, error) {
				return []byte(realisticSignalGetSenderPoison), errors.New("exit status 1")
			},
			generations: 1,
			wantKind:    PollerFailureUpgrade,
			wantFP:      signalGetSenderPoisonFingerprint,
		},
		{
			name:         "receive account invalid",
			version:      "signal-cli 0.14.5\n",
			listAccounts: listed,
			receive: func() ([]byte, error) {
				return []byte("User " + parkTestAccount + " is not registered."), errors.New("exit status 1")
			},
			generations: 1,
			wantKind:    PollerFailureReauth,
			wantFP:      SignalAccountInvalidFingerprint,
		},
		{
			// No accounts.json: nothing on disk corroborates the link, so the
			// first exhausted probe parks immediately.
			name:    "probe account missing from disk",
			version: "signal-cli 0.14.5\n",
			listAccounts: func() ([]byte, error) {
				return []byte("User " + parkTestAccount + " is not registered."), errors.New("exit status 1")
			},
			generations: 1,
			wantKind:    PollerFailureReauth,
			wantFP:      SignalAccountInvalidFingerprint,
		},
		{
			name:          "unreadable streak",
			storedAccount: true,
			version:       "signal-cli 0.14.5\n",
			listAccounts:  func() ([]byte, error) { return []byte("[]"), nil },
			generations:   accountUnreadableStreakLimit,
			wantKind:      PollerFailureReauth,
			wantFP:        SignalAccountUnreadableFingerprint,
		},
	}
}

// TestStatusParkFingerprintMatchesExit is the differential check behind I8:
// for every park the receive lifecycle raises, the published
// status.park_fingerprint equals the PollerExit fingerprint the supervisor
// records, a retest generation in flight shows no park, and a later healthy
// generation clears it.
func TestStatusParkFingerprintMatchesExit(t *testing.T) {
	for _, scenario := range parkScenarios() {
		t.Run(scenario.name, func(t *testing.T) {
			shrinkAccountProbeRetryDelays(t)
			configDir := t.TempDir()
			if scenario.storedAccount {
				writeSignalAccountsFixture(t, configDir, parkTestAccount)
			}
			bridge := &Bridge{account: parkTestAccount, configDir: configDir, logger: zerolog.Nop()}

			// healthy flips the stubs to a working signal-cli; gateOverride
			// then holds the version probe so the next generation can be
			// observed in flight.
			healthy := make(chan struct{})
			var gateOverride chan struct{}
			isHealthy := func() bool {
				select {
				case <-healthy:
					return true
				default:
					return false
				}
			}
			installSignalGateStubs(t, bridge,
				func(ctx context.Context) ([]byte, error) {
					if isHealthy() {
						select {
						case <-gateOverride:
						case <-ctx.Done():
							return nil, ctx.Err()
						}
						return []byte("signal-cli 0.14.5\n"), nil
					}
					return []byte(scenario.version), nil
				},
				func(ctx context.Context, _ string, args ...string) ([]byte, error) {
					switch {
					case hasSignalCLIArg(args, "listAccounts"):
						if isHealthy() {
							return []byte(`[{"number":"` + parkTestAccount + `"}]`), nil
						}
						return scenario.listAccounts()
					case hasSignalCLIArg(args, "receive"):
						if isHealthy() || scenario.receive == nil {
							<-ctx.Done()
							return nil, ctx.Err()
						}
						return scenario.receive()
					default:
						return []byte("[]"), nil
					}
				},
			)

			var exit PollerExit
			for generation := 1; generation <= scenario.generations; generation++ {
				exit = runPollerGeneration(t, bridge, generation)
				status := bridge.Status()
				if generation < scenario.generations {
					if status.ParkFingerprint != "" || status.NeedsReauth || status.UpgradeRequired {
						t.Fatalf("generation %d is not a park but status = %+v", generation, status)
					}
					continue
				}
			}
			if exit.Kind != scenario.wantKind || exit.Fingerprint != scenario.wantFP {
				t.Fatalf("park exit = %+v, want %s/%s", exit, scenario.wantKind, scenario.wantFP)
			}
			status := bridge.Status()
			if status.ParkFingerprint != exit.Fingerprint {
				t.Fatalf("status.park_fingerprint = %q, want the exit fingerprint %q", status.ParkFingerprint, exit.Fingerprint)
			}
			if !status.NeedsReauth && !status.UpgradeRequired {
				t.Fatalf("parked status has no park flag: %+v", status)
			}
			if status.NeedsReauth && status.UpgradeRequired {
				t.Fatalf("parked status has both park flags: %+v", status)
			}
			if (exit.Kind == PollerFailureUpgrade) != status.UpgradeRequired {
				t.Fatalf("exit kind %s but upgrade_required=%v", exit.Kind, status.UpgradeRequired)
			}
			encoded, err := json.Marshal(status)
			if err != nil {
				t.Fatal(err)
			}
			if want := fmt.Sprintf(`"park_fingerprint":%q`, exit.Fingerprint); !bytes.Contains(encoded, []byte(want)) {
				t.Fatalf("status JSON %s missing %s", encoded, want)
			}

			// The next generation (the retest, or a manual reconnect) clears
			// the park while it runs: a retest in flight reads as an
			// ordinary reconnect, never as a stale park.
			gateOverride = make(chan struct{})
			close(healthy)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			run, err := bridge.StartPoller(ctx)
			if err != nil {
				t.Fatalf("healthy StartPoller(): %v", err)
			}
			inFlight := bridge.Status()
			if inFlight.ParkFingerprint != "" || inFlight.NeedsReauth || inFlight.UpgradeRequired || !inFlight.Connecting {
				t.Fatalf("generation in flight = %+v, want connecting with no park", inFlight)
			}
			close(gateOverride)
			select {
			case <-run.Ready():
			case <-time.After(3 * time.Second):
				t.Fatal("healthy generation did not become ready")
			}
			recovered := bridge.Status()
			if recovered.ParkFingerprint != "" || recovered.NeedsReauth || recovered.UpgradeRequired || !recovered.Connected {
				t.Fatalf("recovered status = %+v, want connected with no park", recovered)
			}
			cancel()
			select {
			case <-run.Done():
			case <-time.After(3 * time.Second):
				t.Fatal("healthy generation did not exit after cancel")
			}
		})
	}
}

func runPollerGeneration(t *testing.T, bridge *Bridge, generation int) PollerExit {
	t.Helper()
	run, err := bridge.StartPoller(context.Background())
	if err != nil {
		t.Fatalf("generation %d StartPoller(): %v", generation, err)
	}
	select {
	case exit := <-run.Done():
		return exit
	case <-time.After(5 * time.Second):
		t.Fatalf("generation %d did not exit", generation)
		return PollerExit{}
	}
}

// TestUnpairClearsParkFingerprint: unpair is one of the sites that clears
// both park flags, so it must clear the fingerprint with them.
func TestUnpairClearsParkFingerprint(t *testing.T) {
	bridge := &Bridge{account: parkTestAccount, configDir: t.TempDir(), logger: zerolog.Nop()}
	bridge.ApplyPollerFailure(PollerExit{
		Kind:        PollerFailureReauth,
		Fingerprint: SignalAccountUnreadableFingerprint,
		Err:         errors.New("unreadable"),
	})
	if got := bridge.Status().ParkFingerprint; got != SignalAccountUnreadableFingerprint {
		t.Fatalf("park_fingerprint before unpair = %q", got)
	}
	if err := bridge.UnpairContext(context.Background()); err != nil {
		t.Fatalf("UnpairContext(): %v", err)
	}
	if status := bridge.Status(); status.ParkFingerprint != "" || status.NeedsReauth || status.UpgradeRequired {
		t.Fatalf("status after unpair = %+v, want no park", status)
	}
}

func TestParkRetestedAutomaticallyOnlyAcceptsTheUnreadablePark(t *testing.T) {
	for _, fingerprint := range []string{
		"",
		SignalCLIVersionFingerprint,
		SignalAccountInvalidFingerprint,
		SignalReceiveFailureFingerprint,
		SignalAccountProbeFingerprint,
		SignalReceivePanicFingerprint,
		SignalPairingIncompleteFingerprint,
		SignalAccountProbeEmptyFingerprint,
		SignalParkUnspecifiedFingerprint,
		signalGetSenderPoisonFingerprint,
		"signal_poller_stopped",
		"x",
	} {
		if ParkRetestedAutomatically(fingerprint) {
			t.Fatalf("ParkRetestedAutomatically(%q) = true; only the ambiguous unreadable park may self-retest", fingerprint)
		}
	}
	if !ParkRetestedAutomatically(SignalAccountUnreadableFingerprint) {
		t.Fatal("the unreadable park must self-retest")
	}
}

// parkModel is the reference semantics of ApplyPollerFailure's park flags.
type parkModel struct {
	needsReauth     bool
	upgradeRequired bool
	fingerprint     string
}

func (m parkModel) apply(exit PollerExit) parkModel {
	if exit.Err == nil ||
		(m.upgradeRequired && exit.Kind != PollerFailureUpgrade) ||
		(m.needsReauth && exit.Kind == PollerFailureTransient) {
		return m
	}
	normalized := exit.Fingerprint
	if normalized == "" {
		normalized = SignalParkUnspecifiedFingerprint
	}
	switch exit.Kind {
	case PollerFailureReauth:
		return parkModel{needsReauth: true, fingerprint: normalized}
	case PollerFailureUpgrade:
		return parkModel{upgradeRequired: true, fingerprint: normalized}
	case PollerFailureTransient:
		return parkModel{}
	}
	return m
}

type quickExitSequence []PollerExit

func (quickExitSequence) Generate(r *rand.Rand, _ int) reflect.Value {
	kinds := []PollerFailureKind{"", PollerFailureTransient, PollerFailureReauth, PollerFailureUpgrade, PollerFailureUnpaired}
	fingerprints := []string{"", SignalAccountUnreadableFingerprint, SignalAccountInvalidFingerprint, SignalCLIVersionFingerprint, "x"}
	sequence := make(quickExitSequence, 1+r.Intn(20))
	for index := range sequence {
		exit := PollerExit{
			Kind:        kinds[r.Intn(len(kinds))],
			Fingerprint: fingerprints[r.Intn(len(fingerprints))],
		}
		if r.Intn(6) != 0 {
			exit.Err = errors.New("poller failure")
		}
		sequence[index] = exit
	}
	return reflect.ValueOf(sequence)
}

// TestQuickApplyPollerFailureParkInvariant (I8): across arbitrary exit
// sequences, the published park fingerprint is non-empty exactly when a park
// flag is set, the two flags never coexist, and the projection matches the
// reference model step for step.
//
// Deliberately NOT asserted: "a self-retesting fingerprint implies
// needs_reauth". The generator pairs kinds and fingerprints freely, and the
// minimized counterexample [upgrade_required/signal_account_unreadable]
// publishes upgrade_required with that fingerprint. No park site emits that
// pairing, and it stays safe if one ever does: sendcap ranks UpgradeRequired
// above the account_recheck tier (hard), and the supervisor retest requires
// the reauth_required class.
func TestQuickApplyPollerFailureParkInvariant(t *testing.T) {
	property := func(sequence quickExitSequence) bool {
		bridge := &Bridge{account: parkTestAccount, configDir: t.TempDir(), logger: zerolog.Nop()}
		model := parkModel{}
		for step, exit := range sequence {
			bridge.ApplyPollerFailure(exit)
			model = model.apply(exit)
			status := bridge.Status()
			got := parkModel{
				needsReauth:     status.NeedsReauth,
				upgradeRequired: status.UpgradeRequired,
				fingerprint:     status.ParkFingerprint,
			}
			switch {
			case got != model:
				t.Logf("step %d exit %+v: status %+v, model %+v", step, exit, got, model)
				return false
			case (status.ParkFingerprint != "") != (status.NeedsReauth || status.UpgradeRequired):
				t.Logf("step %d: park fingerprint %q disagrees with flags %+v", step, status.ParkFingerprint, status)
				return false
			case status.NeedsReauth && status.UpgradeRequired:
				t.Logf("step %d: both park flags set", step)
				return false
			}
		}
		return true
	}
	config := &quick.Config{MaxCount: 2000, Rand: rand.New(rand.NewSource(0x9a4c))}
	if err := quick.Check(property, config); err != nil {
		t.Fatal(err)
	}
}
