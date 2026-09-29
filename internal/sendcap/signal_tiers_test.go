package sendcap

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"testing/quick"
	"time"

	"github.com/maxghenis/openmessage/internal/app"
	"github.com/maxghenis/openmessage/internal/signallive"
	"github.com/maxghenis/openmessage/internal/whatsapplive"
)

// signalPoisonFingerprint is signallive's (unexported) poison-envelope
// upgrade park fingerprint, spelled out so the tier tests cover every park
// the bridge can publish.
const signalPoisonFingerprint = "incoming_message_get_sender_content_null"

// parkFingerprints spans every park fingerprint the bridge publishes plus the
// empty and unrecognized values the fail-safe must absorb.
var parkFingerprints = []string{
	"",
	signallive.SignalAccountUnreadableFingerprint,
	signallive.SignalAccountInvalidFingerprint,
	signallive.SignalCLIVersionFingerprint,
	signalPoisonFingerprint,
	signallive.SignalParkUnspecifiedFingerprint,
	"garbage_fingerprint",
}

var sendPlatforms = []string{PlatformSMS, PlatformWhatsApp, PlatformSignal}

// TestComputeSignalTiers pins the three Signal park tiers (and their order)
// against the typed park fingerprint, including the reason text each tier
// owes the caller.
func TestComputeSignalTiers(t *testing.T) {
	retestMinutes := fmt.Sprintf("about every %d minutes", int(signallive.ParkRetestInterval/time.Minute))
	const upgradeDetail = "signal-cli 0.14.4 is below the required minimum 0.14.5; upgrade signal-cli to continue receiving messages"
	tests := []struct {
		name          string
		transportsOff bool
		adapterOff    bool
		signal        signallive.StatusSnapshot
		wantTier      Tier
		wantCondition Condition
		mustContain   []string
		mustNotHave   []string
	}{
		{
			name:     "healthy",
			signal:   signallive.StatusSnapshot{Paired: true, Connected: true},
			wantTier: TierAvailable,
		},
		{
			name:          "unpaired",
			signal:        signallive.StatusSnapshot{},
			wantTier:      TierUnavailable,
			wantCondition: ConditionNotPaired,
		},
		{
			name:          "adapter missing",
			adapterOff:    true,
			signal:        signallive.StatusSnapshot{Paired: true, Connected: true},
			wantTier:      TierUnavailable,
			wantCondition: ConditionAdapterMissing,
		},
		{
			name:          "disconnected",
			signal:        signallive.StatusSnapshot{Paired: true},
			wantTier:      TierQueueable,
			wantCondition: ConditionDisconnected,
		},
		{
			// StartPoller clears the park for the few seconds a retest
			// generation runs; that window reads as an ordinary reconnect.
			name:          "retest generation in flight",
			signal:        signallive.StatusSnapshot{Paired: true, Connecting: true},
			wantTier:      TierQueueable,
			wantCondition: ConditionDisconnected,
		},
		{
			name: "unreadable park is re-checked automatically",
			signal: signallive.StatusSnapshot{
				Paired: true, NeedsReauth: true,
				ParkFingerprint: signallive.SignalAccountUnreadableFingerprint,
				LastError:       "signal-cli cannot read the linked Signal account +15551230000 after 3 consecutive attempts; the paced park retest will keep re-probing it",
			},
			wantTier:      TierQueueable,
			wantCondition: ConditionAccountRecheck,
			mustContain: []string{
				"often transient",
				"re-link",
				"re-checks it automatically",
				retestMinutes,
				"waits in the outbox until Signal recovers",
				"send window closes if it has one",
			},
			mustNotHave: []string{"re-pair"},
		},
		{
			// A stale Connected flag must not make a parked account look
			// available.
			name: "unreadable park with a stale connected flag",
			signal: signallive.StatusSnapshot{
				Paired: true, Connected: true, NeedsReauth: true,
				ParkFingerprint: signallive.SignalAccountUnreadableFingerprint,
			},
			wantTier:      TierQueueable,
			wantCondition: ConditionAccountRecheck,
		},
		{
			name: "server-confirmed reauth needs a relink",
			signal: signallive.StatusSnapshot{
				Paired: true, NeedsReauth: true,
				ParkFingerprint: signallive.SignalAccountInvalidFingerprint,
			},
			wantTier:      TierUnavailable,
			wantCondition: ConditionRelinkRequired,
			mustContain:   []string{"re-link Signal from Platforms", "automatic reconnects stay parked"},
			mustNotHave:   []string{"automatically about"},
		},
		{
			// Fail-safe: a park that does not name its fingerprint is never
			// promised to heal (this was TestComputeSignalNeedsReauthIsHardUnavailable).
			name:          "reauth with an empty fingerprint is hard",
			signal:        signallive.StatusSnapshot{Paired: true, Connected: true, NeedsReauth: true},
			wantTier:      TierUnavailable,
			wantCondition: ConditionRelinkRequired,
		},
		{
			name: "reauth with an unrecognized fingerprint is hard",
			signal: signallive.StatusSnapshot{
				Paired: true, NeedsReauth: true, ParkFingerprint: "garbage_fingerprint",
			},
			wantTier:      TierUnavailable,
			wantCondition: ConditionRelinkRequired,
		},
		{
			name: "reauth with the unspecified placeholder is hard",
			signal: signallive.StatusSnapshot{
				Paired: true, NeedsReauth: true,
				ParkFingerprint: signallive.SignalParkUnspecifiedFingerprint,
			},
			wantTier:      TierUnavailable,
			wantCondition: ConditionRelinkRequired,
		},
		{
			name: "version gate needs an upgrade",
			signal: signallive.StatusSnapshot{
				Paired: true, UpgradeRequired: true,
				ParkFingerprint: signallive.SignalCLIVersionFingerprint,
				LastError:       upgradeDetail,
			},
			wantTier:      TierUnavailable,
			wantCondition: ConditionUpgradeRequired,
			mustContain:   []string{"upgrade signal-cli", "brew upgrade signal-cli", upgradeDetail, "reconnect Signal from Platforms or restart the app"},
			mustNotHave:   []string{"re-pair", "re-link"},
		},
		{
			name: "poison envelope needs an upgrade",
			signal: signallive.StatusSnapshot{
				Paired: true, UpgradeRequired: true, ParkFingerprint: signalPoisonFingerprint,
			},
			wantTier:      TierUnavailable,
			wantCondition: ConditionUpgradeRequired,
		},
		{
			name: "upgrade outranks a retested reauth park",
			signal: signallive.StatusSnapshot{
				Paired: true, UpgradeRequired: true, NeedsReauth: true,
				ParkFingerprint: signallive.SignalAccountUnreadableFingerprint,
			},
			wantTier:      TierUnavailable,
			wantCondition: ConditionUpgradeRequired,
		},
		{
			name: "upgrade with a stale connected flag is still hard",
			signal: signallive.StatusSnapshot{
				Paired: true, Connected: true, UpgradeRequired: true,
				ParkFingerprint: signallive.SignalCLIVersionFingerprint,
			},
			wantTier:      TierUnavailable,
			wantCondition: ConditionUpgradeRequired,
		},
		{
			name: "unpaired outranks an upgrade park",
			signal: signallive.StatusSnapshot{
				UpgradeRequired: true, ParkFingerprint: signallive.SignalCLIVersionFingerprint,
			},
			wantTier:      TierUnavailable,
			wantCondition: ConditionNotPaired,
		},
		{
			name:          "no transports outranks every park",
			transportsOff: true,
			signal: signallive.StatusSnapshot{
				Paired: true, NeedsReauth: true,
				ParkFingerprint: signallive.SignalAccountUnreadableFingerprint,
			},
			wantTier:      TierUnavailable,
			wantCondition: ConditionNoTransports,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			inputs := healthyInputs()
			inputs.TransportsEnabled = !test.transportsOff
			inputs.Signal = test.signal
			if test.adapterOff {
				inputs.AdapterTextSend = func(platform string) bool { return platform != PlatformSignal }
			}
			capability := Compute(inputs)[PlatformSignal]
			if got := TierOf(capability); got != test.wantTier {
				t.Fatalf("tier = %s, want %s (%+v)", got, test.wantTier, capability)
			}
			if capability.Condition != test.wantCondition {
				t.Fatalf("condition = %q, want %q (%+v)", capability.Condition, test.wantCondition, capability)
			}
			for _, fragment := range test.mustContain {
				if !strings.Contains(capability.Reason, fragment) {
					t.Fatalf("reason missing %q: %q", fragment, capability.Reason)
				}
			}
			for _, fragment := range test.mustNotHave {
				if strings.Contains(capability.Reason, fragment) {
					t.Fatalf("reason must not contain %q: %q", fragment, capability.Reason)
				}
			}
		})
	}
}

// specTier is the reference semantics (invariant I5 and the Google/WhatsApp
// switches) each platform's capability must match. Every branch reads only
// the platform's own inputs plus the process-wide flags, so agreement with it
// over the whole input space also proves isolation (I4). The Signal account
// recheck branch spells the retested fingerprint literally: widening
// signallive.ParkRetestedAutomatically must be a deliberate change here too.
func specTier(in Inputs, platform string) (Tier, Condition) {
	if !in.TransportsEnabled {
		return TierUnavailable, ConditionNoTransports
	}
	if in.AdapterTextSend != nil && !in.AdapterTextSend(platform) {
		return TierUnavailable, ConditionAdapterMissing
	}
	switch platform {
	case PlatformSMS:
		google := in.Google
		switch {
		case !google.Paired:
			return TierUnavailable, ConditionNotPaired
		case google.AuthExpired:
			return TierUnavailable, ConditionAuthExpired
		case google.NeedsRepair:
			return TierUnavailable, ConditionNeedsRepair
		case !google.Connected:
			return TierQueueable, ConditionDisconnected
		case !google.PhoneResponding:
			return TierQueueable, ConditionPhoneNotResponding
		}
	case PlatformWhatsApp:
		switch {
		case !in.WhatsApp.Paired:
			return TierUnavailable, ConditionNotPaired
		case !in.WhatsApp.Connected:
			return TierQueueable, ConditionDisconnected
		}
	case PlatformSignal:
		signal := in.Signal
		switch {
		case !signal.Paired:
			return TierUnavailable, ConditionNotPaired
		case signal.UpgradeRequired:
			return TierUnavailable, ConditionUpgradeRequired
		case signal.NeedsReauth && signal.ParkFingerprint == "signal_account_unreadable":
			return TierQueueable, ConditionAccountRecheck
		case signal.NeedsReauth:
			return TierUnavailable, ConditionRelinkRequired
		case !signal.Connected:
			return TierQueueable, ConditionDisconnected
		}
	}
	return TierAvailable, ""
}

// wellFormed checks invariants I1 and I2 on one capability.
func wellFormed(capability Capability) error {
	switch {
	case capability.Available:
		if capability.Queueable || capability.Reason != "" || capability.Condition != "" {
			return fmt.Errorf("available capability carries queueable/reason/condition: %+v", capability)
		}
		return nil
	case capability.Reason == "" || capability.Condition == "":
		return fmt.Errorf("non-available capability without reason or condition: %+v", capability)
	}
	tier, known := ConditionTier(capability.Condition)
	if !known {
		return fmt.Errorf("condition %q missing from the tier table", capability.Condition)
	}
	if tier != TierOf(capability) {
		return fmt.Errorf("condition %q implies %s but the capability is %s: %+v", capability.Condition, tier, TierOf(capability), capability)
	}
	return nil
}

func checkComputeAgainstSpec(in Inputs) error {
	capabilities := Compute(in)
	if len(capabilities) != len(sendPlatforms) {
		return fmt.Errorf("capability keys = %d, want exactly %v", len(capabilities), sendPlatforms)
	}
	for _, platform := range sendPlatforms {
		capability, ok := capabilities[platform]
		if !ok {
			return fmt.Errorf("capability map missing %s", platform)
		}
		if err := wellFormed(capability); err != nil {
			return fmt.Errorf("%s: %w", platform, err)
		}
		wantTier, wantCondition := specTier(in, platform)
		if TierOf(capability) != wantTier || capability.Condition != wantCondition {
			return fmt.Errorf("%s = %s/%q, spec says %s/%q (%+v)", platform, TierOf(capability), capability.Condition, wantTier, wantCondition, capability)
		}
	}
	// I7 lockstep: "re-checked automatically" iff the supervisor's retest
	// predicate accepts this park.
	signal := capabilities[PlatformSignal]
	recheck := signal.Condition == ConditionAccountRecheck
	retested := in.Signal.NeedsReauth && signallive.ParkRetestedAutomatically(in.Signal.ParkFingerprint)
	if recheck && !retested {
		return fmt.Errorf("account_recheck for a park the supervisor never retests: %+v", in.Signal)
	}
	if retested && !recheck && signal.Condition == ConditionRelinkRequired {
		return fmt.Errorf("a retested park was reported as relink_required: %+v", in.Signal)
	}
	if signal.Available && (!in.Signal.Paired || !in.Signal.Connected || in.Signal.NeedsReauth || in.Signal.UpgradeRequired) {
		return fmt.Errorf("signal available without a healthy link: %+v", in.Signal)
	}
	return nil
}

// adapterMasks is nil (no v2 send stack) plus every subset of the three
// platforms.
func adapterMasks() []func(string) bool {
	masks := []func(string) bool{nil}
	for mask := 0; mask < 8; mask++ {
		mask := mask
		masks = append(masks, func(platform string) bool {
			for index, candidate := range sendPlatforms {
				if candidate == platform {
					return mask&(1<<index) != 0
				}
			}
			return false
		})
	}
	return masks
}

func bit(value, index int) bool { return value&(1<<index) != 0 }

// TestExhaustiveComputeInvariants enumerates every combination of the
// boolean inputs, every park fingerprint, and every adapter mask (516,096
// inputs) and checks I1 (well-formed tiers), I2 (condition/tier table), I3
// (exact key set), I4+I5 (agreement with the per-platform spec), and I7
// (account_recheck iff the supervisor retests the park).
func TestExhaustiveComputeInvariants(t *testing.T) {
	masks := adapterMasks()
	checked := 0
	for transports := 0; transports < 2; transports++ {
		for google := 0; google < 1<<5; google++ {
			for whatsApp := 0; whatsApp < 1<<2; whatsApp++ {
				for signal := 0; signal < 1<<5; signal++ {
					for _, fingerprint := range parkFingerprints {
						for _, mask := range masks {
							in := Inputs{
								TransportsEnabled: transports == 1,
								Google: app.GoogleStatusSnapshot{
									Connected:       bit(google, 0),
									Paired:          bit(google, 1),
									PhoneResponding: bit(google, 2),
									AuthExpired:     bit(google, 3),
									NeedsRepair:     bit(google, 4),
								},
								WhatsApp: whatsapplive.StatusSnapshot{
									Connected: bit(whatsApp, 0),
									Paired:    bit(whatsApp, 1),
								},
								Signal: signallive.StatusSnapshot{
									Connected:       bit(signal, 0),
									Connecting:      bit(signal, 1),
									Paired:          bit(signal, 2),
									NeedsReauth:     bit(signal, 3),
									UpgradeRequired: bit(signal, 4),
									ParkFingerprint: fingerprint,
								},
								AdapterTextSend: mask,
							}
							if err := checkComputeAgainstSpec(in); err != nil {
								t.Fatalf("input %+v: %v", in, err)
							}
							checked++
						}
					}
				}
			}
		}
	}
	if want := 2 * 32 * 4 * 32 * len(parkFingerprints) * len(masks); checked != want {
		t.Fatalf("checked %d inputs, want %d", checked, want)
	}
}

func tierRank(tier Tier) int {
	switch tier {
	case TierAvailable:
		return 2
	case TierQueueable:
		return 1
	default:
		return 0
	}
}

// TestExhaustiveSignalFaultMonotone (I6): over every Signal-relevant input,
// switching on a fault never raises the Signal tier (hard < queueable <
// available).
func TestExhaustiveSignalFaultMonotone(t *testing.T) {
	for transports := 0; transports < 2; transports++ {
		for signal := 0; signal < 1<<5; signal++ {
			for _, fingerprint := range parkFingerprints {
				for adapter := 0; adapter < 2; adapter++ {
					in := healthyInputs()
					in.TransportsEnabled = transports == 1
					in.Signal = signallive.StatusSnapshot{
						Connected:       bit(signal, 0),
						Connecting:      bit(signal, 1),
						Paired:          bit(signal, 2),
						NeedsReauth:     bit(signal, 3),
						UpgradeRequired: bit(signal, 4),
						ParkFingerprint: fingerprint,
					}
					if adapter == 0 {
						in.AdapterTextSend = func(platform string) bool { return platform != PlatformSignal }
					}
					if err := checkSignalFaultMonotone(in); err != nil {
						t.Fatal(err)
					}
				}
			}
		}
	}
}

func signalFaults() map[string]func(*Inputs) {
	return map[string]func(*Inputs){
		"upgrade_required": func(in *Inputs) { in.Signal.UpgradeRequired = true },
		"unpaired":         func(in *Inputs) { in.Signal.Paired = false },
		"adapter_missing": func(in *Inputs) {
			in.AdapterTextSend = func(platform string) bool { return platform != PlatformSignal }
		},
		"no_transports": func(in *Inputs) { in.TransportsEnabled = false },
		"needs_reauth":  func(in *Inputs) { in.Signal.NeedsReauth = true },
		"disconnected":  func(in *Inputs) { in.Signal.Connected = false },
		"unretested_fingerprint": func(in *Inputs) {
			if in.Signal.ParkFingerprint == signallive.SignalAccountUnreadableFingerprint {
				in.Signal.ParkFingerprint = signallive.SignalAccountInvalidFingerprint
			}
		},
	}
}

func checkSignalFaultMonotone(in Inputs) error {
	before := TierOf(Compute(in)[PlatformSignal])
	for name, apply := range signalFaults() {
		faulted := in
		apply(&faulted)
		after := TierOf(Compute(faulted)[PlatformSignal])
		if tierRank(after) > tierRank(before) {
			return fmt.Errorf("fault %s raised signal from %s to %s (input %+v)", name, before, after, in.Signal)
		}
	}
	return nil
}

// quickInputs is a testing/quick generator for Inputs. quick cannot generate
// func fields, and uniform random strings would almost never hit a real
// park fingerprint, so the fingerprint is drawn from the known set most of
// the time and the adapter view from nil or a random mask.
type quickInputs struct{ in Inputs }

func (quickInputs) Generate(r *rand.Rand, _ int) reflect.Value {
	coin := func() bool { return r.Intn(2) == 0 }
	fingerprint := parkFingerprints[r.Intn(len(parkFingerprints))]
	if r.Intn(5) == 0 {
		fingerprint = fmt.Sprintf("random_%x", r.Int63())
	}
	lastErrors := []string{"", "signal-cli 0.14.4 is below the required minimum 0.14.5", "exit status 1"}
	in := Inputs{
		TransportsEnabled: r.Intn(4) != 0,
		Google: app.GoogleStatusSnapshot{
			Connected: coin(), Paired: coin(), PhoneResponding: coin(),
			AuthExpired: coin(), NeedsRepair: coin(),
		},
		WhatsApp: whatsapplive.StatusSnapshot{Connected: coin(), Paired: coin()},
		Signal: signallive.StatusSnapshot{
			Connected: coin(), Connecting: coin(), Paired: coin(),
			NeedsReauth: coin(), UpgradeRequired: coin(),
			ParkFingerprint: fingerprint,
			LastError:       lastErrors[r.Intn(len(lastErrors))],
		},
	}
	if coin() {
		mask := r.Intn(8)
		in.AdapterTextSend = func(platform string) bool {
			for index, candidate := range sendPlatforms {
				if candidate == platform {
					return mask&(1<<index) != 0
				}
			}
			return false
		}
	}
	return reflect.ValueOf(quickInputs{in: in})
}

func quickConfig(seed int64) *quick.Config {
	return &quick.Config{MaxCount: 10000, Rand: rand.New(rand.NewSource(seed))}
}

// TestQuickComputeTierInvariants: I1, I2, I3 (including determinism), I5, I7,
// and "Signal available implies a healthy link" on random inputs, including
// random fingerprints and LastError text the exhaustive test does not vary.
func TestQuickComputeTierInvariants(t *testing.T) {
	property := func(generated quickInputs) bool {
		if err := checkComputeAgainstSpec(generated.in); err != nil {
			t.Log(err)
			return false
		}
		if !reflect.DeepEqual(Compute(generated.in), Compute(generated.in)) {
			t.Logf("Compute is not deterministic for %+v", generated.in)
			return false
		}
		return true
	}
	if err := quick.Check(property, quickConfig(0x5e4dca9)); err != nil {
		t.Fatal(err)
	}
}

// TestQuickSignalInputsIsolated (I4): each platform's snapshot affects only
// its own entry.
func TestQuickSignalInputsIsolated(t *testing.T) {
	property := func(base, other quickInputs) bool {
		original := Compute(base.in)
		swaps := []struct {
			name      string
			apply     func(*Inputs)
			unchanged []string
		}{
			{"signal", func(in *Inputs) { in.Signal = other.in.Signal }, []string{PlatformSMS, PlatformWhatsApp}},
			{"google", func(in *Inputs) { in.Google = other.in.Google }, []string{PlatformWhatsApp, PlatformSignal}},
			{"whatsapp", func(in *Inputs) { in.WhatsApp = other.in.WhatsApp }, []string{PlatformSMS, PlatformSignal}},
		}
		for _, swap := range swaps {
			changed := base.in
			swap.apply(&changed)
			swapped := Compute(changed)
			for _, platform := range swap.unchanged {
				if !reflect.DeepEqual(original[platform], swapped[platform]) {
					t.Logf("replacing %s changed %s: %+v -> %+v", swap.name, platform, original[platform], swapped[platform])
					return false
				}
			}
		}
		return true
	}
	if err := quick.Check(property, quickConfig(0x150a7ed)); err != nil {
		t.Fatal(err)
	}
}

// TestQuickSignalFaultMonotone (I6) on random inputs, including random
// fingerprints.
func TestQuickSignalFaultMonotone(t *testing.T) {
	property := func(generated quickInputs) bool {
		if err := checkSignalFaultMonotone(generated.in); err != nil {
			t.Log(err)
			return false
		}
		return true
	}
	if err := quick.Check(property, quickConfig(0x3070707)); err != nil {
		t.Fatal(err)
	}
}

// TestConditionTierTableTotal reads every Condition constant from the
// package source and checks the tier table covers each exactly once, never
// with an available or unknown tier (a Condition only ever explains a
// non-available result).
func TestConditionTierTableTotal(t *testing.T) {
	fileSet := token.NewFileSet()
	file, err := parser.ParseFile(fileSet, "sendcap.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var declared []Condition
	for _, decl := range file.Decls {
		general, ok := decl.(*ast.GenDecl)
		if !ok || general.Tok != token.CONST {
			continue
		}
		for _, spec := range general.Specs {
			value := spec.(*ast.ValueSpec)
			ident, ok := value.Type.(*ast.Ident)
			if !ok || ident.Name != "Condition" {
				continue
			}
			for _, literal := range value.Values {
				basic, ok := literal.(*ast.BasicLit)
				if !ok || basic.Kind != token.STRING {
					t.Fatalf("Condition constant with a non-literal value: %#v", literal)
				}
				declared = append(declared, Condition(strings.Trim(basic.Value, `"`)))
			}
		}
	}
	if len(declared) == 0 {
		t.Fatal("found no Condition constants in sendcap.go")
	}
	seen := map[Condition]bool{}
	for _, condition := range declared {
		if seen[condition] {
			t.Fatalf("duplicate Condition value %q", condition)
		}
		seen[condition] = true
		tier, ok := ConditionTier(condition)
		if !ok {
			t.Fatalf("Condition %q has no tier", condition)
		}
		if tier != TierQueueable && tier != TierUnavailable {
			t.Fatalf("Condition %q maps to %s; a condition only explains queueable or unavailable", condition, tier)
		}
	}
	if len(conditionTiers) != len(declared) {
		t.Fatalf("tier table has %d entries for %d declared conditions", len(conditionTiers), len(declared))
	}
}

func TestClassifyUnknownIsNeverUnavailable(t *testing.T) {
	for _, capability := range []Capability{
		{},
		{Available: true},
		{Queueable: true, Reason: "r"},
		{Reason: "r", Condition: ConditionRelinkRequired},
	} {
		if got := Classify(capability, false); got != TierUnknown {
			t.Fatalf("Classify(%+v, unknown) = %s, want unknown", capability, got)
		}
		if got := Classify(capability, true); got != TierOf(capability) {
			t.Fatalf("Classify(%+v, known) = %s, want %s", capability, got, TierOf(capability))
		}
	}
	// A malformed daemon entry cannot be both: Available wins, matching the
	// pre-condition enforcement (available passed before queueable was read).
	if got := TierOf(Capability{Available: true, Queueable: true}); got != TierAvailable {
		t.Fatalf("available+queueable = %s, want available", got)
	}
}
