// Package bridgeadapters contains composition-layer adapters around the
// existing DB-coupled transports. The clean bridge package intentionally does
// not import this package or any legacy transport/database type.
package bridgeadapters

import (
	"context"
	"errors"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/client"
	"github.com/maxghenis/openmessage/internal/signallive"
	"github.com/maxghenis/openmessage/internal/whatsapplive"
)

// GoogleAdapter registers the current Google Messages client at the lifecycle
// and capability-metadata boundary. C4 will adapt its lifecycle and clean
// transport operations; C2 deliberately does not delegate legacy sends.
type GoogleAdapter struct {
	accountID string
	legacy    *client.Client
}

func NewGoogle(accountID string, legacy *client.Client) *GoogleAdapter {
	return &GoogleAdapter{accountID: accountID, legacy: legacy}
}

func (a *GoogleAdapter) AccountID() string {
	return a.accountID
}

func (a *GoogleAdapter) Platform() bridge.Platform {
	return bridge.PlatformGoogle
}

func (a *GoogleAdapter) DeclaredCapabilities() bridge.CapabilitySet {
	return legacyCapabilities(false)
}

func (a *GoogleAdapter) Start(
	_ context.Context,
	_ bridge.StartRequest,
	_ bridge.ConnectionSink,
) (bridge.Run, error) {
	return nil, unsupportedLegacyLifecycle(bridge.PlatformGoogle)
}

// WhatsAppAdapter registers the current whatsapplive bridge without adapting
// its DB-returning send methods to the clean transport interfaces.
type WhatsAppAdapter struct {
	accountID string
	legacy    *whatsapplive.Bridge
}

func NewWhatsApp(accountID string, legacy *whatsapplive.Bridge) *WhatsAppAdapter {
	return &WhatsAppAdapter{accountID: accountID, legacy: legacy}
}

func (a *WhatsAppAdapter) AccountID() string {
	return a.accountID
}

func (a *WhatsAppAdapter) Platform() bridge.Platform {
	return bridge.PlatformWhatsApp
}

func (a *WhatsAppAdapter) DeclaredCapabilities() bridge.CapabilitySet {
	return legacyCapabilities(true)
}

func (a *WhatsAppAdapter) Start(
	_ context.Context,
	_ bridge.StartRequest,
	_ bridge.ConnectionSink,
) (bridge.Run, error) {
	return nil, unsupportedLegacyLifecycle(bridge.PlatformWhatsApp)
}

// SignalAdapter registers the current signallive bridge without adapting its
// DB-returning send methods to the clean transport interfaces.
type SignalAdapter struct {
	accountID string
	legacy    *signallive.Bridge
}

func NewSignal(accountID string, legacy *signallive.Bridge) *SignalAdapter {
	return &SignalAdapter{accountID: accountID, legacy: legacy}
}

func (a *SignalAdapter) AccountID() string {
	return a.accountID
}

func (a *SignalAdapter) Platform() bridge.Platform {
	return bridge.PlatformSignal
}

func (a *SignalAdapter) DeclaredCapabilities() bridge.CapabilitySet {
	return legacyCapabilities(false)
}

func (a *SignalAdapter) Start(
	_ context.Context,
	_ bridge.StartRequest,
	_ bridge.ConnectionSink,
) (bridge.Run, error) {
	return nil, unsupportedLegacyLifecycle(bridge.PlatformSignal)
}

func legacyCapabilities(pairPhone bool) bridge.CapabilitySet {
	return bridge.CapabilitySet{
		StartConversation: true,
		TextSend:          true,
		MediaSend:         true,
		Reactions:         true,
		ReadReceipts:      true,
		PairQR:            true,
		PairPhone:         pairPhone,
		MediaDownload:     true,
	}
}

func unsupportedLegacyLifecycle(platform bridge.Platform) error {
	return bridge.OpError{
		Class:     bridge.FailureUnsupported,
		Operation: "start_legacy_" + string(platform) + "_lifecycle",
		Cause: errors.New(
			"legacy lifecycle is metadata-only until its generation-owned adapter is implemented",
		),
	}
}

var (
	_ bridge.Adapter            = (*GoogleAdapter)(nil)
	_ bridge.CapabilityDeclarer = (*GoogleAdapter)(nil)
	_ bridge.Adapter            = (*WhatsAppAdapter)(nil)
	_ bridge.CapabilityDeclarer = (*WhatsAppAdapter)(nil)
	_ bridge.Adapter            = (*SignalAdapter)(nil)
	_ bridge.CapabilityDeclarer = (*SignalAdapter)(nil)
)
