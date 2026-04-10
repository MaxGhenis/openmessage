package app

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"

	"github.com/maxghenis/openmessage/internal/client"
	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/importer"
	"github.com/maxghenis/openmessage/internal/signallive"
	"github.com/maxghenis/openmessage/internal/whatsapplive"
)

// BackfillPhase represents the current phase of a deep backfill.
type BackfillPhase string

const (
	BackfillPhaseIdle     BackfillPhase = ""
	BackfillPhaseFolders  BackfillPhase = "folders"
	BackfillPhaseMessages BackfillPhase = "messages"
	BackfillPhaseContacts BackfillPhase = "contacts"
	BackfillPhaseDone     BackfillPhase = "done"
)

const maxErrorDetails = 100

// BackfillProgress tracks the current state of a deep backfill operation.
type BackfillProgress struct {
	mu                 sync.Mutex
	Running            bool          `json:"running"`
	Phase              BackfillPhase `json:"phase"`
	FoldersScanned     int           `json:"folders_scanned"`
	ConversationsFound int           `json:"conversations_found"`
	MessagesFound      int           `json:"messages_found"`
	ContactsChecked    int           `json:"contacts_checked"`
	Errors             int           `json:"errors"`
	ErrorDetails       []string      `json:"error_details,omitempty"`
}

// reset clears all fields for a fresh backfill run.
func (p *BackfillProgress) reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Running = true
	p.Phase = BackfillPhaseFolders
	p.FoldersScanned = 0
	p.ConversationsFound = 0
	p.MessagesFound = 0
	p.ContactsChecked = 0
	p.Errors = 0
	p.ErrorDetails = nil
}

// setPhase updates the current phase.
func (p *BackfillProgress) setPhase(phase BackfillPhase) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Phase = phase
}

// finish marks the backfill as complete.
func (p *BackfillProgress) finish() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Running = false
	p.Phase = BackfillPhaseDone
}

// addError increments the error count and optionally records a detail string.
func (p *BackfillProgress) addError(detail string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Errors++
	if detail != "" && len(p.ErrorDetails) < maxErrorDetails {
		p.ErrorDetails = append(p.ErrorDetails, detail)
	}
}

// add increments the given counters atomically.
func (p *BackfillProgress) add(conversations, messages, contacts, folders int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ConversationsFound += conversations
	p.MessagesFound += messages
	p.ContactsChecked += contacts
	p.FoldersScanned += folders
}

func (p *BackfillProgress) snapshot() BackfillProgress {
	p.mu.Lock()
	defer p.mu.Unlock()
	cp := *p
	if len(p.ErrorDetails) > 0 {
		cp.ErrorDetails = append([]string(nil), p.ErrorDetails...)
	}
	return cp
}

type App struct {
	clientMu               sync.RWMutex
	Client                 *client.Client
	Store                  *db.Store
	EventHandler           *client.EventHandler
	Logger                 zerolog.Logger
	DataDir                string
	SessionPath            string
	WhatsAppSessionPath    string
	SignalConfigPath       string
	Connected              atomic.Bool
	OnConversationsChange  func()
	OnIncomingMessage      func(*db.Message)
	OnMessagesChange       func(string)
	OnStatusChange         func(bool)
	OnTypingChange         func(conversationID, senderName, senderNumber string, typing bool)
	OnWhatsAppStatusChange func()
	OnSignalStatusChange   func()

	// gmClient is used by backfill methods. If nil, it's derived from Client.GM.
	// Set this field directly in tests to inject a mock.
	gmClient         GMClient
	BackfillProgress BackfillProgress
	backfillRunning  atomic.Bool
	reconcileRunning atomic.Bool
	whatsAppMu       sync.Mutex
	WhatsApp         *whatsapplive.Bridge
	signalMu         sync.Mutex
	Signal           *signallive.Bridge
	statusMu         sync.Mutex
	googleLastError  string
	tempDataDir      string
	pendingMediaMu   sync.Mutex
	pendingMedia     map[string]struct{}
}

type GoogleStatusSnapshot struct {
	Connected    bool   `json:"connected"`
	Paired       bool   `json:"paired"`
	NeedsPairing bool   `json:"needs_pairing"`
	LastError    string `json:"last_error,omitempty"`
}

func DefaultDataDir() string {
	if dir := os.Getenv("OPENMESSAGES_DATA_DIR"); dir != "" {
		return dir
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "openmessage")
}

func DemoMode() bool {
	value := strings.TrimSpace(os.Getenv("OPENMESSAGES_DEMO"))
	if value == "" {
		return false
	}
	switch strings.ToLower(value) {
	case "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

func New(logger zerolog.Logger) (*App, error) {
	dataDir := DefaultDataDir()
	tempDataDir := ""
	if DemoMode() {
		tmpDir, err := os.MkdirTemp("", "openmessage-demo-*")
		if err != nil {
			return nil, fmt.Errorf("create temp dir: %w", err)
		}
		dataDir = tmpDir
		tempDataDir = tmpDir
	}
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		if tempDataDir != "" {
			_ = os.RemoveAll(tempDataDir)
		}
		return nil, fmt.Errorf("create data dir: %w", err)
	}

	dbPath := filepath.Join(dataDir, "messages.db")
	store, err := db.New(dbPath)
	if err != nil {
		if tempDataDir != "" {
			_ = os.RemoveAll(tempDataDir)
		}
		return nil, fmt.Errorf("open db: %w", err)
	}
	if report, err := store.RepairLegacyArtifacts(); err != nil {
		logger.Warn().Err(err).Msg("Failed to repair legacy message artifacts")
	} else {
		if report.DeletedWhatsAppReactionPlaceholders > 0 {
			logger.Info().
				Int("deleted", report.DeletedWhatsAppReactionPlaceholders).
				Msg("Removed legacy WhatsApp reaction placeholder rows")
		}
		if report.DeletedSignalReactionPlaceholders > 0 {
			logger.Info().
				Int("deleted", report.DeletedSignalReactionPlaceholders).
				Msg("Removed legacy Signal reaction placeholder rows")
		}
		if report.RemainingWhatsAppMediaPlaceholders > 0 {
			logger.Info().
				Int("count", report.RemainingWhatsAppMediaPlaceholders).
				Msg("Legacy WhatsApp media placeholders remain without downloadable metadata")
		}
		if report.FixedGoogleOutgoingAttributionRows > 0 {
			logger.Info().
				Int("fixed", report.FixedGoogleOutgoingAttributionRows).
				Msg("Repaired legacy Google Messages outgoing attribution rows")
		}
	}
	if !Sandboxed() {
		if mediaRepair, err := (&importer.WhatsAppNative{}).RepairLegacyMediaPlaceholders(store); err != nil {
			logger.Warn().Err(err).Msg("Failed to repair legacy WhatsApp media placeholders")
		} else if mediaRepair.MessagesRepaired > 0 {
			logger.Info().
				Int("repaired", mediaRepair.MessagesRepaired).
				Int("skipped", mediaRepair.MessagesSkipped).
				Msg("Repaired legacy WhatsApp media placeholders from local desktop store")
		}
	}

	// Seed demo data
	if DemoMode() {
		if err := store.SeedDemo(); err != nil {
			store.Close()
			if tempDataDir != "" {
				_ = os.RemoveAll(tempDataDir)
			}
			return nil, fmt.Errorf("seed demo data: %w", err)
		}
		logger.Info().
			Str("data_dir", dataDir).
			Str("db", dbPath).
			Msg("Demo mode — using isolated fake data")
	}

	sessionPath := filepath.Join(dataDir, "session.json")
	whatsAppSessionPath := filepath.Join(dataDir, "whatsapp-session.db")
	signalConfigPath := filepath.Join(dataDir, "signal-cli")

	app := &App{
		Store:               store,
		Logger:              logger,
		DataDir:             dataDir,
		SessionPath:         sessionPath,
		WhatsAppSessionPath: whatsAppSessionPath,
		SignalConfigPath:    signalConfigPath,
		tempDataDir:         tempDataDir,
	}
	return app, nil
}

func LocalIdentityName() string {
	if name := os.Getenv("OPENMESSAGES_MY_NAME"); name != "" {
		return name
	}
	if currentUser, err := user.Current(); err == nil {
		if currentUser.Name != "" {
			return currentUser.Name
		}
		if currentUser.Username != "" {
			return currentUser.Username
		}
	}
	return "Me"
}

func Sandboxed() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("OPENMESSAGES_APP_SANDBOX")), "1") ||
		strings.EqualFold(strings.TrimSpace(os.Getenv("OPENMESSAGES_APP_SANDBOX")), "true")
}

func (a *App) GetClient() *client.Client {
	a.clientMu.RLock()
	defer a.clientMu.RUnlock()
	return a.Client
}

func (a *App) setClient(cli *client.Client) {
	a.clientMu.Lock()
	defer a.clientMu.Unlock()
	a.Client = cli
}

func (a *App) LoadAndConnect() error {
	sessionData, err := client.LoadSession(a.SessionPath)
	if err != nil {
		a.setGoogleLastError(err.Error())
		return fmt.Errorf("load session (run 'gmessages-mcp pair' first): %w", err)
	}

	cli, err := client.NewFromSession(sessionData, a.Logger)
	if err != nil {
		a.setGoogleLastError(err.Error())
		return fmt.Errorf("create client: %w", err)
	}
	a.setClient(cli)

	a.EventHandler = &client.EventHandler{
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
		OnTypingChange: a.OnTypingChange,
		OnRealtimeGapRecovered: func(reason string) {
			a.StartRecentReconcile(reason)
		},
		OnDisconnect: func() {
			a.Connected.Store(false)
			a.setClient(nil)
			if err := os.Remove(a.SessionPath); err != nil && !os.IsNotExist(err) {
				a.Logger.Warn().Err(err).Msg("Failed to remove invalidated Google Messages session")
			}
			a.setGoogleLastError("Google Messages session invalidated; pair again")
			a.emitStatusChange(false)
			a.Logger.Warn().Msg("Disconnected from Google Messages")
		},
	}
	cli.GM.SetEventHandler(a.EventHandler.Handle)

	if err := cli.GM.Connect(); err != nil {
		a.setGoogleLastError(err.Error())
		return fmt.Errorf("connect: %w", err)
	}
	a.Connected.Store(true)
	a.setGoogleLastError("")
	a.emitStatusChange(true)
	a.Logger.Info().Msg("Connected to Google Messages")
	return nil
}

// Unpair deletes the session file so the app can re-pair.
func (a *App) Unpair() error {
	a.Connected.Store(false)
	a.setGoogleLastError("")
	a.emitStatusChange(false)
	if cli := a.GetClient(); cli != nil {
		cli.GM.Disconnect()
		a.setClient(nil)
	}
	if err := os.Remove(a.SessionPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove session: %w", err)
	}
	a.Logger.Info().Msg("Unpaired — session deleted")
	return nil
}

// getGMClient returns the GMClient for backfill operations.
// Uses the injected mock if set, otherwise wraps the real libgm client.
func (a *App) getGMClient() GMClient {
	if a.gmClient != nil {
		return a.gmClient
	}
	if cli := a.GetClient(); cli != nil {
		return newRealGMClient(cli.GM)
	}
	return nil
}

func (a *App) currentBackfillClient() (GMClient, any) {
	if a.gmClient != nil {
		return a.gmClient, a.gmClient
	}
	if cli := a.GetClient(); cli != nil {
		return newRealGMClient(cli.GM), cli.GM
	}
	return nil, nil
}

func (a *App) backfillClientStillCurrent(token any) bool {
	if token == nil {
		return false
	}
	if a.gmClient != nil {
		return a.gmClient == token
	}
	if cli := a.GetClient(); cli != nil {
		return cli.GM == token
	}
	return false
}

func (a *App) StartDeepBackfill() bool {
	if !a.beginBackfill() {
		return false
	}
	go a.deepBackfill()
	return true
}

func (a *App) StartRecentReconcile(reason string) bool {
	if a.backfillRunning.Load() || !a.reconcileRunning.CompareAndSwap(false, true) {
		return false
	}
	go a.reconcileRecentConversations(reason)
	return true
}

var pendingMediaRefreshSchedule = []time.Duration{
	0,
	2 * time.Second,
	6 * time.Second,
	15 * time.Second,
}

func (a *App) StartPendingMediaRefresh(conversationID, messageID string) bool {
	conversationID = strings.TrimSpace(conversationID)
	messageID = strings.TrimSpace(messageID)
	if conversationID == "" || messageID == "" || a.backfillRunning.Load() {
		return false
	}

	key := conversationID + "|" + messageID
	a.pendingMediaMu.Lock()
	if a.pendingMedia == nil {
		a.pendingMedia = make(map[string]struct{})
	}
	if _, exists := a.pendingMedia[key]; exists {
		a.pendingMediaMu.Unlock()
		return false
	}
	a.pendingMedia[key] = struct{}{}
	a.pendingMediaMu.Unlock()

	go func() {
		defer func() {
			a.pendingMediaMu.Lock()
			delete(a.pendingMedia, key)
			a.pendingMediaMu.Unlock()
		}()
		a.refreshPendingMediaMessageWithSchedule(conversationID, messageID, pendingMediaRefreshSchedule)
	}()
	return true
}

func (a *App) GooglePaired() bool {
	_, err := os.Stat(a.SessionPath)
	return err == nil
}

func (a *App) GoogleStatus() GoogleStatusSnapshot {
	a.statusMu.Lock()
	lastError := a.googleLastError
	a.statusMu.Unlock()
	return GoogleStatusSnapshot{
		Connected:    a.Connected.Load(),
		Paired:       a.GooglePaired(),
		NeedsPairing: !a.Connected.Load() && !a.GooglePaired(),
		LastError:    lastError,
	}
}

func (a *App) AnyConnected() bool {
	if a.Connected.Load() {
		return true
	}
	if a.WhatsAppStatus().Connected {
		return true
	}
	if a.SignalStatus().Connected {
		return true
	}
	return false
}

func (a *App) ReconnectGoogleMessages() error {
	if a.Connected.Load() && a.GetClient() != nil {
		a.setGoogleLastError("")
		return nil
	}
	if cli := a.GetClient(); cli != nil {
		cli.GM.Disconnect()
		a.setClient(nil)
	}
	return a.LoadAndConnect()
}

func (a *App) setGoogleLastError(message string) {
	a.statusMu.Lock()
	defer a.statusMu.Unlock()
	a.googleLastError = strings.TrimSpace(message)
}

func (a *App) beginBackfill() bool {
	return a.backfillRunning.CompareAndSwap(false, true)
}

func (a *App) endBackfill() {
	a.backfillRunning.Store(false)
}

func (a *App) emitConversationsChange() {
	if a.OnConversationsChange != nil {
		a.OnConversationsChange()
	}
}

func (a *App) emitMessagesChange(conversationID string) {
	if a.OnMessagesChange != nil {
		a.OnMessagesChange(conversationID)
	}
}

func (a *App) emitStatusChange(connected bool) {
	if a.OnStatusChange != nil {
		a.OnStatusChange(connected)
	}
}

func (a *App) IsDeepBackfillRunning() bool {
	return a.backfillRunning.Load()
}

// GetBackfillProgress returns a snapshot of the current backfill progress.
func (a *App) GetBackfillProgress() BackfillProgress {
	return a.BackfillProgress.snapshot()
}

func (a *App) Close() {
	if cli := a.GetClient(); cli != nil {
		cli.GM.Disconnect()
	}
	if signal := a.GetSignal(); signal != nil {
		if err := signal.Close(); err != nil {
			a.Logger.Warn().Err(err).Msg("Failed to close Signal bridge")
		}
	}
	if wa := a.GetWhatsApp(); wa != nil {
		if err := wa.Close(); err != nil {
			a.Logger.Warn().Err(err).Msg("Failed to close WhatsApp bridge")
		}
	}
	if a.Store != nil {
		a.Store.Close()
	}
	if a.tempDataDir != "" {
		if err := os.RemoveAll(a.tempDataDir); err != nil {
			a.Logger.Warn().Err(err).Str("dir", a.tempDataDir).Msg("Failed to remove demo temp data dir")
		}
	}
}
