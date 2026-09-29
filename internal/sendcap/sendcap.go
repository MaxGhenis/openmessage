// Package sendcap computes per-platform SEND capability: whether a send
// submitted right now is expected to reach the transport promptly, with the
// reason when it is not. It is deliberately stricter than "connected" — a
// paired-but-dark platform still accepts sends into the durable outbox,
// where they wait, which is exactly what a caller must know before
// submitting a time-sensitive message (2026-08-05: a send reported as
// accepted flushed ~15 hours later and double-texted the recipient).
//
// Every non-available capability names a typed Condition, and a static
// table maps each Condition to exactly one tier (queueable or unavailable),
// so the reason text, the tier, and the condition can never disagree.
// Signal's parks split three ways on the bridge's typed park fingerprint:
// an upgrade park and a server-confirmed (or unrecognized) reauth park are
// hard-down, while the ambiguous signal_account_unreadable park — the one
// the supervisor re-probes on its own — is queueable.
//
// The daemon publishes this as the /api/status "send" block, and the
// transportless MCP client enforces it before submitting; keeping the
// computation and the Classify classifier here keeps the two surfaces
// answering identically.
package sendcap

import (
	"fmt"
	"strings"
	"time"

	"github.com/maxghenis/openmessage/internal/app"
	"github.com/maxghenis/openmessage/internal/signallive"
	"github.com/maxghenis/openmessage/internal/whatsapplive"
)

// Platform keys of the capability map. "sms" covers Google Messages
// (SMS/RCS); RCS-vs-SMS is not distinguishable at this layer and is
// deliberately not guessed.
const (
	PlatformSMS      = "sms"
	PlatformWhatsApp = "whatsapp"
	PlatformSignal   = "signal"
)

// Tier is the coarse send state a caller branches on. Compute only ever
// yields the first three; Classify adds TierUnknown for a capability the
// caller could not obtain (a daemon older than the send block, or a platform
// missing from it).
type Tier string

const (
	TierAvailable   Tier = "available"
	TierQueueable   Tier = "queueable"
	TierUnavailable Tier = "unavailable"
	TierUnknown     Tier = "unknown"
)

// Condition names why a platform is not available. It is advisory for
// clients — the tier always derives from Available/Queueable — but Compute
// sets it on every non-available result and conditionTiers pins it to the
// tier it implies.
type Condition string

const (
	// ConditionNoTransports: this process holds no live platform connections.
	ConditionNoTransports Condition = "no_transports"
	// ConditionAdapterMissing: the v2 send stack has no text-send adapter
	// for the platform in this run (receive-only).
	ConditionAdapterMissing Condition = "adapter_missing"
	// ConditionNotPaired: the platform has no linked account.
	ConditionNotPaired Condition = "not_paired"
	// ConditionAuthExpired: Google rejected the session cookies.
	ConditionAuthExpired Condition = "auth_expired"
	// ConditionNeedsRepair: Google reports connected but consecutive sends
	// failed.
	ConditionNeedsRepair Condition = "needs_repair"
	// ConditionDisconnected: a paired transport is briefly down.
	ConditionDisconnected Condition = "disconnected"
	// ConditionPhoneNotResponding: Google may hold a send until the paired
	// phone comes back.
	ConditionPhoneNotResponding Condition = "phone_not_responding"
	// ConditionAccountRecheck: a Signal reauth park that the supervisor
	// re-probes automatically (signallive.ParkRetestedAutomatically). Its
	// evidence is ambiguous: often transient, possibly a real unlink.
	ConditionAccountRecheck Condition = "account_recheck"
	// ConditionRelinkRequired: any other Signal reauth park, including an
	// empty or unrecognized park fingerprint (fail-safe). Nothing retries it.
	ConditionRelinkRequired Condition = "relink_required"
	// ConditionUpgradeRequired: Signal is parked until signal-cli is
	// upgraded (version gate or the known poison-envelope crash).
	ConditionUpgradeRequired Condition = "upgrade_required"
)

// conditionTiers is the single condition-to-tier table. Every Condition
// constant has exactly one entry (pinned by a test that reads the constant
// list from source).
var conditionTiers = map[Condition]Tier{
	ConditionNoTransports:       TierUnavailable,
	ConditionAdapterMissing:     TierUnavailable,
	ConditionNotPaired:          TierUnavailable,
	ConditionAuthExpired:        TierUnavailable,
	ConditionNeedsRepair:        TierUnavailable,
	ConditionDisconnected:       TierQueueable,
	ConditionPhoneNotResponding: TierQueueable,
	ConditionAccountRecheck:     TierQueueable,
	ConditionRelinkRequired:     TierUnavailable,
	ConditionUpgradeRequired:    TierUnavailable,
}

// ConditionTier reports the tier a Condition implies; false for a condition
// this build does not know (for example one published by a newer daemon).
func ConditionTier(condition Condition) (Tier, bool) {
	tier, ok := conditionTiers[condition]
	return tier, ok
}

// Capability reports one platform's send path.
//
// Available=false splits into two tiers:
//   - Queueable=true: a self-healing outage (transport briefly disconnected,
//     phone not responding, or the Signal account_recheck park the app
//     re-probes on its own). A durable send submitted now is accepted,
//     reported truthfully as queued/not-transmitted, and transmits when the
//     platform recovers — or cancels at its TTL, when it has one. Send paths
//     allow these.
//   - Queueable=false: the platform cannot send and will not recover on its
//     own (no transports, not paired, adapter unregistered, Google auth
//     expired or needing repair, a Signal relink_required or
//     upgrade_required park). Send paths refuse these outright rather than
//     queueing into a black hole.
//
// Condition names the specific cause on every non-available result; it is
// empty exactly when Available is true.
type Capability struct {
	Available bool      `json:"available"`
	Queueable bool      `json:"queueable,omitempty"`
	Reason    string    `json:"reason,omitempty"`
	Condition Condition `json:"condition,omitempty"`
}

// TierOf derives the tier from Available/Queueable alone — the fields every
// daemon version publishes — so a client never has to trust Condition.
func TierOf(capability Capability) Tier {
	switch {
	case capability.Available:
		return TierAvailable
	case capability.Queueable:
		return TierQueueable
	default:
		return TierUnavailable
	}
}

// Classify is the one classifier behind send-time enforcement and route
// discovery. known=false (no capability map, or the platform is missing from
// it) is TierUnknown: unknown is not unavailable, so enforcement lets it
// through and route discovery must not call it available either.
func Classify(capability Capability, known bool) Tier {
	if !known {
		return TierUnknown
	}
	return TierOf(capability)
}

// Inputs are the live transport snapshots plus the optional v2 send-stack
// adapter view.
type Inputs struct {
	// TransportsEnabled is false in daemon shapes that hold no live platform
	// connections; nothing can send from such a process.
	TransportsEnabled bool

	Google   app.GoogleStatusSnapshot
	WhatsApp whatsapplive.StatusSnapshot
	Signal   signallive.StatusSnapshot

	// AdapterTextSend reports whether the v2 send stack has a registered
	// adapter with text-send capability for the platform key. Nil means no
	// v2 send stack is active (legacy direct-transport sends).
	AdapterTextSend func(platform string) bool
}

func queueable(condition Condition, reason string) Capability {
	return Capability{Queueable: true, Reason: reason, Condition: condition}
}

func unavailable(condition Condition, reason string) Capability {
	return Capability{Reason: reason, Condition: condition}
}

// signalAccountRecheckReason explains the queueable Signal park. It must stay
// true for both readings of the ambiguous evidence (a transient account-check
// failure or a real unlink), and it must not promise a send window: UI sends
// carry none.
func signalAccountRecheckReason() string {
	return fmt.Sprintf(
		"signal-cli could not read the linked Signal account on several consecutive attempts; this is often transient, but it can also mean Signal unlinked this device, which only a re-link from Platforms fixes. The app re-checks it automatically about every %d minutes; a send submitted now waits in the outbox until Signal recovers, or until its send window closes if it has one",
		int(signallive.ParkRetestInterval/time.Minute),
	)
}

const signalRelinkRequiredReason = "signal reports the linked account is no longer registered or authorized, and automatic reconnects stay parked; re-link Signal from Platforms (unpairing deletes signal-cli's local data, so back it up first)"

func signalUpgradeRequiredReason(lastError string) string {
	detail := ""
	if lastError = strings.TrimSpace(lastError); lastError != "" {
		detail = " (" + lastError + ")"
	}
	return "signal is parked until signal-cli is upgraded" + detail +
		"; upgrade signal-cli (for a Homebrew install: brew upgrade signal-cli), then reconnect Signal from Platforms or restart the app. Automatic reconnects stay parked until then"
}

// Compute builds the capability map for the three send platforms.
func Compute(in Inputs) map[string]Capability {
	capabilities := make(map[string]Capability, 3)
	if !in.TransportsEnabled {
		off := unavailable(ConditionNoTransports,
			"this process holds no live platform connections and cannot send on any platform")
		capabilities[PlatformSMS] = off
		capabilities[PlatformWhatsApp] = off
		capabilities[PlatformSignal] = off
		return capabilities
	}

	adapterSendable := func(platform string) bool {
		if in.AdapterTextSend == nil {
			return true
		}
		return in.AdapterTextSend(platform)
	}
	adapterMissing := unavailable(ConditionAdapterMissing,
		"the platform adapter is not registered with the v2 send stack in this run (receive-only); sends on this platform fail rather than queue")

	switch {
	case !adapterSendable(PlatformSMS):
		capabilities[PlatformSMS] = adapterMissing
	case !in.Google.Paired:
		capabilities[PlatformSMS] = unavailable(ConditionNotPaired, "google messages is not paired")
	case in.Google.AuthExpired:
		capabilities[PlatformSMS] = unavailable(ConditionAuthExpired, "google messages session cookies were rejected; re-pair or wait for automatic repair")
	case in.Google.NeedsRepair:
		capabilities[PlatformSMS] = unavailable(ConditionNeedsRepair, "google messages reports connected but consecutive sends have failed; the phone has likely unlinked this device")
	case !in.Google.Connected:
		capabilities[PlatformSMS] = queueable(ConditionDisconnected, "google messages is disconnected; a send submitted now would wait in the outbox until it reconnects")
	case !in.Google.PhoneResponding:
		capabilities[PlatformSMS] = queueable(ConditionPhoneNotResponding, "the paired phone is not responding; google may accept a send and hold it until the phone comes back")
	default:
		capabilities[PlatformSMS] = Capability{Available: true}
	}

	switch {
	case !adapterSendable(PlatformWhatsApp):
		capabilities[PlatformWhatsApp] = adapterMissing
	case !in.WhatsApp.Paired:
		capabilities[PlatformWhatsApp] = unavailable(ConditionNotPaired, "whatsapp is not paired")
	case !in.WhatsApp.Connected:
		capabilities[PlatformWhatsApp] = queueable(ConditionDisconnected, "whatsapp is disconnected; a send submitted now would wait in the outbox until it reconnects")
	default:
		capabilities[PlatformWhatsApp] = Capability{Available: true}
	}

	// Signal's order matters: an upgrade park outranks a reauth park (the
	// bridge never raises both, but a status must still classify), and only a
	// reauth park whose fingerprint the supervisor retests on its own is
	// queueable. An empty or unrecognized fingerprint falls to the hard
	// relink tier — fail-safe, never "it will heal".
	switch {
	case !adapterSendable(PlatformSignal):
		capabilities[PlatformSignal] = adapterMissing
	case !in.Signal.Paired:
		capabilities[PlatformSignal] = unavailable(ConditionNotPaired, "signal is not paired")
	case in.Signal.UpgradeRequired:
		capabilities[PlatformSignal] = unavailable(ConditionUpgradeRequired, signalUpgradeRequiredReason(in.Signal.LastError))
	case in.Signal.NeedsReauth && signallive.ParkRetestedAutomatically(in.Signal.ParkFingerprint):
		capabilities[PlatformSignal] = queueable(ConditionAccountRecheck, signalAccountRecheckReason())
	case in.Signal.NeedsReauth:
		capabilities[PlatformSignal] = unavailable(ConditionRelinkRequired, signalRelinkRequiredReason)
	case !in.Signal.Connected:
		capabilities[PlatformSignal] = queueable(ConditionDisconnected, "signal is disconnected; a send submitted now would wait in the outbox until it reconnects")
	default:
		capabilities[PlatformSignal] = Capability{Available: true}
	}

	return capabilities
}
