package sendcap

import (
	"testing"

	"github.com/maxghenis/openmessage/internal/app"
	"github.com/maxghenis/openmessage/internal/signallive"
	"github.com/maxghenis/openmessage/internal/whatsapplive"
)

func healthyInputs() Inputs {
	return Inputs{
		TransportsEnabled: true,
		Google:            app.GoogleStatusSnapshot{Connected: true, Paired: true, PhoneResponding: true},
		WhatsApp:          whatsapplive.StatusSnapshot{Connected: true, Paired: true},
		Signal:            signallive.StatusSnapshot{Connected: true, Paired: true},
	}
}

func TestComputeAllHealthy(t *testing.T) {
	capabilities := Compute(healthyInputs())
	for _, platform := range []string{PlatformSMS, PlatformWhatsApp, PlatformSignal} {
		capability := capabilities[platform]
		if !capability.Available || capability.Reason != "" {
			t.Fatalf("%s = %+v, want available with no reason", platform, capability)
		}
	}
}

func TestComputeTransportsDisabledBlocksEverything(t *testing.T) {
	inputs := healthyInputs()
	inputs.TransportsEnabled = false
	for platform, capability := range Compute(inputs) {
		if capability.Available || capability.Queueable || capability.Reason == "" {
			t.Fatalf("%s = %+v, want hard-unavailable with reason", platform, capability)
		}
	}
}

func TestComputeTiersSelfHealingVersusHardOutages(t *testing.T) {
	// Disconnected transports are queueable: the durable outbox exists for
	// exactly this, and TTL bounds the staleness.
	inputs := healthyInputs()
	inputs.Google.Connected = false
	inputs.WhatsApp.Connected = false
	inputs.Signal.Connected = false
	capabilities := Compute(inputs)
	for _, platform := range []string{PlatformSMS, PlatformWhatsApp, PlatformSignal} {
		capability := capabilities[platform]
		if capability.Available || !capability.Queueable {
			t.Fatalf("%s disconnected = %+v, want unavailable but queueable", platform, capability)
		}
	}

	// Unpaired platforms are hard-unavailable: nothing will self-heal.
	inputs = healthyInputs()
	inputs.Google.Paired = false
	inputs.WhatsApp.Paired = false
	inputs.Signal.Paired = false
	capabilities = Compute(inputs)
	for _, platform := range []string{PlatformSMS, PlatformWhatsApp, PlatformSignal} {
		capability := capabilities[platform]
		if capability.Available || capability.Queueable {
			t.Fatalf("%s unpaired = %+v, want hard-unavailable", platform, capability)
		}
	}
}

func TestComputeGoogleDegradedStates(t *testing.T) {
	inputs := healthyInputs()
	inputs.Google.NeedsRepair = true
	if capability := Compute(inputs)[PlatformSMS]; capability.Available || capability.Queueable {
		t.Fatalf("needs_repair = %+v, want hard-unavailable (sends keep failing)", capability)
	}

	inputs = healthyInputs()
	inputs.Google.AuthExpired = true
	if capability := Compute(inputs)[PlatformSMS]; capability.Available || capability.Queueable {
		t.Fatalf("auth_expired = %+v, want hard-unavailable", capability)
	}

	// PhoneResponding=false is the incident mechanism: Google can accept a
	// send and hold it until the phone comes back. Queueable, with the
	// warning surfaced.
	inputs = healthyInputs()
	inputs.Google.PhoneResponding = false
	capability := Compute(inputs)[PlatformSMS]
	if capability.Available || !capability.Queueable || capability.Reason == "" {
		t.Fatalf("phone_not_responding = %+v, want queueable with reason", capability)
	}
}

func TestComputeAdapterMissingIsHardUnavailable(t *testing.T) {
	inputs := healthyInputs()
	inputs.AdapterTextSend = func(platform string) bool { return platform != PlatformWhatsApp }
	capabilities := Compute(inputs)
	if capability := capabilities[PlatformWhatsApp]; capability.Available || capability.Queueable {
		t.Fatalf("adapter-missing whatsapp = %+v, want hard-unavailable (receive-only)", capability)
	}
	if !capabilities[PlatformSMS].Available || !capabilities[PlatformSignal].Available {
		t.Fatalf("other platforms affected: %+v", capabilities)
	}
}

func TestComputeSignalNeedsReauthIsHardUnavailable(t *testing.T) {
	inputs := healthyInputs()
	inputs.Signal.NeedsReauth = true
	if capability := Compute(inputs)[PlatformSignal]; capability.Available || capability.Queueable {
		t.Fatalf("needs_reauth = %+v, want hard-unavailable", capability)
	}
}
