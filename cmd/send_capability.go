package cmd

import (
	"github.com/maxghenis/openmessage/internal/app"
	"github.com/maxghenis/openmessage/internal/sendcap"
	"github.com/maxghenis/openmessage/internal/web"
)

// sendCapabilityProvider builds the /api/status "send" block from the live
// transport snapshots and, when the v2 send stack is active, the adapter
// registry. See internal/sendcap for the semantics.
func sendCapabilityProvider(
	a *app.App,
	stack *v2Stack,
	transports bool,
) func() map[string]web.SendPlatformCapability {
	accountForPlatform := map[string]string{
		sendcap.PlatformSMS:      googleAccountID,
		sendcap.PlatformWhatsApp: whatsappAccountID,
		sendcap.PlatformSignal:   signalAccountID,
	}
	return func() map[string]web.SendPlatformCapability {
		inputs := sendcap.Inputs{
			TransportsEnabled: transports,
			Google:            a.GoogleStatus(),
			WhatsApp:          a.WhatsAppStatus(),
			Signal:            a.SignalStatus(),
		}
		if stack != nil {
			inputs.AdapterTextSend = func(platform string) bool {
				accountID, ok := accountForPlatform[platform]
				if !ok {
					return false
				}
				return stack.Registry.Capabilities(accountID).TextSend
			}
		}
		return sendcap.Compute(inputs)
	}
}
