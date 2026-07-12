package app

import (
	"os"

	"github.com/maxghenis/openmessage/internal/client"
)

// GoogleGeneration binds the legacy App callbacks and client pointer to one
// supervisor-owned Google connection generation. Every mutating method first
// verifies that the generation is still current.
type GoogleGeneration struct {
	app     *App
	Client  *client.Client
	Handler *client.EventHandler
}

// BeginGoogleGeneration installs a fresh legacy client for the unchanged
// send/read/media paths and builds a generation-local event handler. Callers
// must capture Handler directly; a libgm callback must never look up
// App.EventHandler after it has been installed.
func (a *App) BeginGoogleGeneration(cli *client.Client) *GoogleGeneration {
	generation := &GoogleGeneration{app: a, Client: cli}
	generation.Handler = &client.EventHandler{
		Store:       a.Store,
		Logger:      a.Logger,
		SessionPath: a.SessionPath,
		Client:      cli,
		OnConversationsChange: func() {
			a.emitConversationsChange()
		},
		OnIncomingMessage: a.OnIncomingMessage,
		OnPendingMedia: func(conversationID, messageID string) {
			a.StartPendingMediaRefresh(conversationID, messageID)
		},
		OnMessagesChange: func(conversationID string) {
			a.emitMessagesChange(conversationID)
		},
		OnTypingChange:           a.OnTypingChange,
		OnGoogleAvatarCandidates: a.QueueGoogleAvatarCandidates,
		OnRealtimeGapRecovered: func(reason string) {
			a.StartRecentReconcile(reason)
		},
		OnPhoneRespondingChange: generation.PhoneResponding,
		OnConnectionLost:        generation.ConnectionLost,
		OnSessionInvalid:        generation.SessionInvalid,
	}

	a.clientMu.Lock()
	a.Client = cli
	a.EventHandler = generation.Handler
	a.googleGeneration = generation
	a.Connected.Store(false)
	a.clientMu.Unlock()
	return generation
}

func (g *GoogleGeneration) current() bool {
	if g == nil || g.app == nil {
		return false
	}
	g.app.clientMu.RLock()
	current := g.Client != nil && g.app.googleGeneration == g && g.app.Client == g.Client
	g.app.clientMu.RUnlock()
	return current
}

// Ready publishes compatibility status only after the adapter has observed a
// real long-poll event. The Connected bit remains a UI/send-path projection;
// it is never used by the supervisor as liveness evidence.
func (g *GoogleGeneration) Ready() bool {
	if !g.current() {
		return false
	}
	a := g.app
	a.ClearGoogleRepairFlag()
	a.googleAuthExpired.Store(false)
	a.RecordGooglePhoneResponding(true)
	a.Connected.Store(true)
	a.setGoogleLastError("")
	a.emitStatusChange(true)
	a.Logger.Info().Msg("Connected to Google Messages")
	a.StartGoogleContactSync()
	return true
}

func (g *GoogleGeneration) PhoneResponding(responding bool) {
	if !g.current() {
		return
	}
	a := g.app
	a.RecordGooglePhoneResponding(responding)
	if !responding {
		a.setGoogleLastError("Your phone isn't responding to OpenMessage right now; make sure it's on and online.")
	} else {
		a.clearGoogleLastErrorIf("Your phone isn't responding to OpenMessage right now; make sure it's on and online.")
	}
	a.emitStatusChange(a.Connected.Load())
}

func (g *GoogleGeneration) ConnectionLost() {
	if !g.current() {
		return
	}
	a := g.app
	a.Connected.Store(false)
	a.setGoogleLastError("Google Messages connection lost; reconnecting…")
	a.emitStatusChange(false)
	a.Logger.Warn().Msg("Google Messages connection lost; will attempt to reconnect")
}

func (g *GoogleGeneration) AuthExpired(err error) {
	if !g.current() || !IsGoogleAuthExpiredError(err) {
		return
	}
	a := g.app
	a.Connected.Store(false)
	a.googleAuthExpired.Store(true)
	a.setGoogleLastError(googleAuthExpiredStatusMessage)
	a.emitStatusChange(false)
	a.Logger.Warn().Err(err).Msg("Google auth expired; marking disconnected")
}

func (g *GoogleGeneration) SessionInvalid() {
	if !g.current() {
		return
	}
	a := g.app
	a.Connected.Store(false)
	a.clientMu.Lock()
	if a.googleGeneration == g && a.Client == g.Client {
		a.Client = nil
		a.EventHandler = nil
	}
	a.clientMu.Unlock()
	if err := os.Remove(a.SessionPath); err != nil && !os.IsNotExist(err) {
		a.Logger.Warn().Err(err).Msg("Failed to remove invalidated Google Messages session")
	}
	a.setGoogleLastError("Google Messages session invalidated; pair again")
	a.emitStatusChange(false)
	a.Logger.Warn().Msg("Disconnected from Google Messages")
}

func (g *GoogleGeneration) SyncInterrupted() {
	if !g.current() {
		return
	}
	a := g.app
	a.Connected.Store(false)
	a.setGoogleLastError("Google Messages sync interrupted; reconnecting…")
	a.emitStatusChange(false)
}

// Release clears the legacy client only if this is still the installed
// generation, so a delayed Stop from generation N cannot clear generation N+1.
func (g *GoogleGeneration) Release() {
	if g == nil || g.app == nil {
		return
	}
	a := g.app
	released := false
	a.clientMu.Lock()
	if a.googleGeneration == g {
		if a.Client == g.Client {
			a.Client = nil
		}
		if a.EventHandler == g.Handler {
			a.EventHandler = nil
		}
		a.googleGeneration = nil
		released = true
	}
	a.clientMu.Unlock()
	if released && a.Connected.Swap(false) {
		a.emitStatusChange(false)
	}
}
