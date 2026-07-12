package bridgeadapters_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/bridgeadapters"
)

func TestLegacyAdaptersRegisterIdentityAndDeclaredCapabilities(t *testing.T) {
	registry := bridge.NewRegistry()

	tests := []struct {
		name          string
		accountID     string
		platform      bridge.Platform
		adapter       bridge.Adapter
		wantPairPhone bool
	}{
		{
			name:          "google",
			accountID:     "google-primary",
			platform:      bridge.PlatformGoogle,
			adapter:       bridgeadapters.NewGoogle("google-primary", nil),
			wantPairPhone: false,
		},
		{
			name:          "whatsapp",
			accountID:     "whatsapp-primary",
			platform:      bridge.PlatformWhatsApp,
			adapter:       bridgeadapters.NewWhatsApp("whatsapp-primary", nil),
			wantPairPhone: true,
		},
		{
			name:          "signal",
			accountID:     "signal-primary",
			platform:      bridge.PlatformSignal,
			adapter:       bridgeadapters.NewSignal("signal-primary", nil),
			wantPairPhone: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cleanCapabilities := []struct {
				name        string
				implemented bool
			}{
				{"ConversationStarter", implements[bridge.ConversationStarter](tt.adapter)},
				{"TextSender", implements[bridge.TextSender](tt.adapter)},
				{"MediaSender", implements[bridge.MediaSender](tt.adapter)},
				{"ReactionSender", implements[bridge.ReactionSender](tt.adapter)},
				{"ReadReceiptSender", implements[bridge.ReadReceiptSender](tt.adapter)},
				{"MediaDownloader", implements[bridge.MediaDownloader](tt.adapter)},
				{"Pairer", implements[bridge.Pairer](tt.adapter)},
			}
			for _, capability := range cleanCapabilities {
				if capability.implemented {
					t.Fatalf("legacy adapter unexpectedly implements clean %s interface", capability.name)
				}
			}
			if err := registry.Register(tt.adapter); err != nil {
				t.Fatalf("Register() error = %v", err)
			}

			snapshot, ok := registry.Snapshot(tt.accountID)
			if !ok {
				t.Fatal("Snapshot() found no registered adapter")
			}
			if snapshot.AccountID != tt.accountID || snapshot.Platform != tt.platform {
				t.Fatalf("Snapshot() = %+v, want account %q platform %q", snapshot, tt.accountID, tt.platform)
			}

			wantCapabilities := bridge.CapabilitySet{
				StartConversation: true,
				TextSend:          true,
				MediaSend:         true,
				Reactions:         true,
				ReadReceipts:      true,
				PairQR:            true,
				PairPhone:         tt.wantPairPhone,
				MediaDownload:     true,
			}
			if got := registry.Capabilities(tt.accountID); !reflect.DeepEqual(got, wantCapabilities) {
				t.Fatalf("Capabilities() = %+v, want %+v", got, wantCapabilities)
			}

			_, err := registry.Acquire(context.Background(), tt.accountID, bridge.CapabilityTextSend)
			if !errors.Is(err, bridge.ErrCapabilityUnavailable) {
				t.Fatalf("Acquire(text) error = %v, want ErrCapabilityUnavailable", err)
			}

			_, err = tt.adapter.Start(context.Background(), bridge.StartRequest{AccountID: tt.accountID}, nil)
			var opErr bridge.OpError
			if !errors.As(err, &opErr) || opErr.Class != bridge.FailureUnsupported {
				t.Fatalf("Start() error = %v, want unsupported OpError", err)
			}
		})
	}
}

func implements[T any](value any) bool {
	_, ok := value.(T)
	return ok
}
