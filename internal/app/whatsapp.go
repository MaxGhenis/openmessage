package app

import (
	"fmt"

	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/whatsapplive"
)

func (a *App) ensureWhatsApp() (*whatsapplive.Bridge, error) {
	a.whatsAppMu.Lock()
	defer a.whatsAppMu.Unlock()

	if a.WhatsApp != nil {
		return a.WhatsApp, nil
	}

	bridge, err := whatsapplive.New(a.WhatsAppSessionPath, a.Store, a.Logger, whatsapplive.Callbacks{
		OnConversationsChange: a.emitConversationsChange,
		OnIncomingMessage:     a.OnIncomingMessage,
		OnMessagesChange:      a.emitMessagesChange,
		OnStatusChange: func() {
			if a.OnWhatsAppStatusChange != nil {
				a.OnWhatsAppStatusChange()
			}
		},
		OnTypingChange: a.OnTypingChange,
	})
	if err != nil {
		return nil, err
	}

	a.WhatsApp = bridge
	return bridge, nil
}

func (a *App) GetWhatsApp() *whatsapplive.Bridge {
	a.whatsAppMu.Lock()
	defer a.whatsAppMu.Unlock()
	return a.WhatsApp
}

func (a *App) LoadAndConnectWhatsApp() error {
	bridge, err := a.ensureWhatsApp()
	if err != nil {
		return fmt.Errorf("init WhatsApp bridge: %w", err)
	}
	if err := bridge.ConnectIfPaired(); err != nil {
		return fmt.Errorf("connect WhatsApp bridge: %w", err)
	}
	return nil
}

func (a *App) StartWhatsAppConnect() error {
	bridge, err := a.ensureWhatsApp()
	if err != nil {
		return fmt.Errorf("init WhatsApp bridge: %w", err)
	}
	if err := bridge.Connect(); err != nil {
		return fmt.Errorf("connect WhatsApp bridge: %w", err)
	}
	return nil
}

func (a *App) UnpairWhatsApp() error {
	bridge, err := a.ensureWhatsApp()
	if err != nil {
		return fmt.Errorf("init WhatsApp bridge: %w", err)
	}
	if err := bridge.Unpair(); err != nil {
		return fmt.Errorf("unpair WhatsApp bridge: %w", err)
	}
	return nil
}

func (a *App) WhatsAppStatus() whatsapplive.StatusSnapshot {
	bridge, err := a.ensureWhatsApp()
	if err != nil {
		a.Logger.Warn().Err(err).Msg("Failed to initialize WhatsApp bridge for status")
		return whatsapplive.StatusSnapshot{LastError: err.Error()}
	}
	return bridge.Status()
}

func (a *App) WhatsAppQRCode() (whatsapplive.QRSnapshot, error) {
	bridge, err := a.ensureWhatsApp()
	if err != nil {
		return whatsapplive.QRSnapshot{}, fmt.Errorf("init WhatsApp bridge: %w", err)
	}
	return bridge.QRCode()
}

func (a *App) UsesWhatsAppLiveBridge() bool {
	bridge, err := a.ensureWhatsApp()
	if err != nil {
		return false
	}
	return bridge.UsesLiveSession()
}

func (a *App) SendWhatsAppText(conversationID, body, replyToID string) (*db.Message, error) {
	bridge, err := a.ensureWhatsApp()
	if err != nil {
		return nil, fmt.Errorf("init WhatsApp bridge: %w", err)
	}
	return bridge.SendText(conversationID, body, replyToID)
}

func (a *App) SendWhatsAppMedia(conversationID string, data []byte, filename, mime, caption, replyToID string) (*db.Message, error) {
	bridge, err := a.ensureWhatsApp()
	if err != nil {
		return nil, fmt.Errorf("init WhatsApp bridge: %w", err)
	}
	return bridge.SendMedia(conversationID, data, filename, mime, caption, replyToID)
}

func (a *App) SendWhatsAppReaction(conversationID, messageID, emoji, action string) error {
	bridge, err := a.ensureWhatsApp()
	if err != nil {
		return fmt.Errorf("init WhatsApp bridge: %w", err)
	}
	return bridge.SendReaction(conversationID, messageID, emoji, action)
}

func (a *App) LeaveWhatsAppGroup(conversationID string) error {
	bridge, err := a.ensureWhatsApp()
	if err != nil {
		return fmt.Errorf("init WhatsApp bridge: %w", err)
	}
	if err := bridge.LeaveGroup(conversationID); err != nil {
		return fmt.Errorf("leave WhatsApp group: %w", err)
	}
	return nil
}

func (a *App) DownloadWhatsAppMedia(msg *db.Message) ([]byte, string, error) {
	bridge, err := a.ensureWhatsApp()
	if err != nil {
		return nil, "", fmt.Errorf("init WhatsApp bridge: %w", err)
	}
	return bridge.DownloadStoredMedia(msg)
}

func (a *App) WhatsAppAvatar(conversationID string) ([]byte, string, error) {
	bridge, err := a.ensureWhatsApp()
	if err != nil {
		return nil, "", fmt.Errorf("init WhatsApp bridge: %w", err)
	}
	return bridge.ProfilePhoto(conversationID)
}
