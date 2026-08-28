// Package sendcap computes per-platform SEND capability: whether a send
// submitted right now is expected to reach the transport promptly, with the
// reason when it is not. It is deliberately stricter than "connected" — a
// paired-but-dark platform still accepts sends into the durable outbox,
// where they wait, which is exactly what a caller must know before
// submitting a time-sensitive message (2026-08-05: a send reported as
// accepted flushed ~15 hours later and double-texted the recipient).
//
// The daemon publishes this as the /api/status "send" block, and the
// transportless MCP client enforces it before submitting; keeping the
// computation here keeps the two surfaces answering identically.
package sendcap

import (
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

// Capability reports one platform's send path.
//
// Available=false splits into two tiers:
//   - Queueable=true: a self-healing outage (transport briefly disconnected,
//     phone not responding). A durable send submitted now is accepted,
//     reported truthfully as queued/not-transmitted, and transmits when the
//     platform recovers — or cancels at its TTL. Send paths allow these.
//   - Queueable=false: the platform cannot send and will not recover on its
//     own (not paired, adapter unregistered, auth revoked). Send paths
//     refuse these outright rather than queueing into a black hole.
type Capability struct {
	Available bool   `json:"available"`
	Queueable bool   `json:"queueable,omitempty"`
	Reason    string `json:"reason,omitempty"`
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

// Compute builds the capability map for the three send platforms.
func Compute(in Inputs) map[string]Capability {
	capabilities := make(map[string]Capability, 3)
	if !in.TransportsEnabled {
		off := Capability{
			Reason: "this process holds no live platform connections and cannot send on any platform",
		}
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
	adapterMissing := Capability{
		Reason: "the platform adapter is not registered with the v2 send stack in this run (receive-only); sends on this platform fail rather than queue",
	}

	switch {
	case !adapterSendable(PlatformSMS):
		capabilities[PlatformSMS] = adapterMissing
	case !in.Google.Paired:
		capabilities[PlatformSMS] = Capability{Reason: "google messages is not paired"}
	case in.Google.AuthExpired:
		capabilities[PlatformSMS] = Capability{Reason: "google messages session cookies were rejected; re-pair or wait for automatic repair"}
	case in.Google.NeedsRepair:
		capabilities[PlatformSMS] = Capability{Reason: "google messages reports connected but consecutive sends have failed; the phone has likely unlinked this device"}
	case !in.Google.Connected:
		capabilities[PlatformSMS] = Capability{Queueable: true, Reason: "google messages is disconnected; a send submitted now would wait in the outbox until it reconnects"}
	case !in.Google.PhoneResponding:
		capabilities[PlatformSMS] = Capability{Queueable: true, Reason: "the paired phone is not responding; google may accept a send and hold it until the phone comes back"}
	default:
		capabilities[PlatformSMS] = Capability{Available: true}
	}

	switch {
	case !adapterSendable(PlatformWhatsApp):
		capabilities[PlatformWhatsApp] = adapterMissing
	case !in.WhatsApp.Paired:
		capabilities[PlatformWhatsApp] = Capability{Reason: "whatsapp is not paired"}
	case !in.WhatsApp.Connected:
		capabilities[PlatformWhatsApp] = Capability{Queueable: true, Reason: "whatsapp is disconnected; a send submitted now would wait in the outbox until it reconnects"}
	default:
		capabilities[PlatformWhatsApp] = Capability{Available: true}
	}

	switch {
	case !adapterSendable(PlatformSignal):
		capabilities[PlatformSignal] = adapterMissing
	case !in.Signal.Paired:
		capabilities[PlatformSignal] = Capability{Reason: "signal is not paired"}
	case in.Signal.NeedsReauth:
		capabilities[PlatformSignal] = Capability{Reason: "signal reports the linked account is no longer authorized; re-pair from Platforms"}
	case !in.Signal.Connected:
		capabilities[PlatformSignal] = Capability{Queueable: true, Reason: "signal is disconnected; a send submitted now would wait in the outbox until it reconnects"}
	default:
		capabilities[PlatformSignal] = Capability{Available: true}
	}

	return capabilities
}
