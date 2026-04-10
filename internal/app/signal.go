package app

import (
	"fmt"

	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/signallive"
)

func (a *App) ensureSignal() (*signallive.Bridge, error) {
	a.signalMu.Lock()
	defer a.signalMu.Unlock()

	if a.Signal != nil {
		return a.Signal, nil
	}

	bridge, err := signallive.New(a.SignalConfigPath, a.Store, a.Logger, signallive.Callbacks{
		OnConversationsChange: a.emitConversationsChange,
		OnIncomingMessage:     a.OnIncomingMessage,
		OnMessagesChange:      a.emitMessagesChange,
		OnStatusChange: func() {
			if a.OnSignalStatusChange != nil {
				a.OnSignalStatusChange()
			}
		},
		OnTypingChange: a.OnTypingChange,
	})
	if err != nil {
		return nil, err
	}

	a.Signal = bridge
	return bridge, nil
}

func (a *App) GetSignal() *signallive.Bridge {
	a.signalMu.Lock()
	defer a.signalMu.Unlock()
	return a.Signal
}

func (a *App) LoadAndConnectSignal() error {
	bridge, err := a.ensureSignal()
	if err != nil {
		return fmt.Errorf("init Signal bridge: %w", err)
	}
	if err := bridge.ConnectIfPaired(); err != nil {
		return fmt.Errorf("connect Signal bridge: %w", err)
	}
	return nil
}

func (a *App) StartSignalConnect() error {
	bridge, err := a.ensureSignal()
	if err != nil {
		return fmt.Errorf("init Signal bridge: %w", err)
	}
	if err := bridge.Connect(); err != nil {
		return fmt.Errorf("connect Signal bridge: %w", err)
	}
	return nil
}

func (a *App) UnpairSignal() error {
	bridge, err := a.ensureSignal()
	if err != nil {
		return fmt.Errorf("init Signal bridge: %w", err)
	}
	if err := bridge.Unpair(); err != nil {
		return fmt.Errorf("unpair Signal bridge: %w", err)
	}
	return nil
}

func (a *App) SignalStatus() signallive.StatusSnapshot {
	bridge, err := a.ensureSignal()
	if err != nil {
		a.Logger.Warn().Err(err).Msg("Failed to initialize Signal bridge for status")
		return signallive.StatusSnapshot{LastError: err.Error()}
	}
	return bridge.Status()
}

func (a *App) SignalQRCode() (signallive.QRSnapshot, error) {
	bridge, err := a.ensureSignal()
	if err != nil {
		return signallive.QRSnapshot{}, fmt.Errorf("init Signal bridge: %w", err)
	}
	return bridge.QRCode()
}

func (a *App) SendSignalText(conversationID, body, replyToID string) (*db.Message, error) {
	bridge, err := a.ensureSignal()
	if err != nil {
		return nil, fmt.Errorf("init Signal bridge: %w", err)
	}
	return bridge.SendText(conversationID, body, replyToID)
}

func (a *App) SendSignalMedia(conversationID string, data []byte, filename, mime, caption, replyToID string) (*db.Message, error) {
	bridge, err := a.ensureSignal()
	if err != nil {
		return nil, fmt.Errorf("init Signal bridge: %w", err)
	}
	return bridge.SendMedia(conversationID, data, filename, mime, caption, replyToID)
}

func (a *App) SendSignalReaction(conversationID, messageID, emoji, action string) error {
	bridge, err := a.ensureSignal()
	if err != nil {
		return fmt.Errorf("init Signal bridge: %w", err)
	}
	return bridge.SendReaction(conversationID, messageID, emoji, action)
}

func (a *App) DownloadSignalMedia(msg *db.Message) ([]byte, string, error) {
	bridge, err := a.ensureSignal()
	if err != nil {
		return nil, "", fmt.Errorf("init Signal bridge: %w", err)
	}
	return bridge.DownloadMedia(msg)
}
