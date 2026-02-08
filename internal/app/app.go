package app

import (
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"

	"github.com/rs/zerolog"

	"github.com/maxghenis/openmessage/internal/client"
	"github.com/maxghenis/openmessage/internal/db"
)

type App struct {
	Client       *client.Client
	Store        *db.Store
	EventHandler *client.EventHandler
	Logger       zerolog.Logger
	DataDir      string
	SessionPath  string
	Connected    atomic.Bool
}

func DefaultDataDir() string {
	if dir := os.Getenv("OPENMESSAGES_DATA_DIR"); dir != "" {
		return dir
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "openmessage")
}

func New(logger zerolog.Logger) (*App, error) {
	dataDir := DefaultDataDir()
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}

	dbPath := filepath.Join(dataDir, "messages.db")
	store, err := db.New(dbPath)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}

	sessionPath := filepath.Join(dataDir, "session.json")

	app := &App{
		Store:       store,
		Logger:      logger,
		DataDir:     dataDir,
		SessionPath: sessionPath,
	}
	return app, nil
}

func (a *App) LoadAndConnect() error {
	sessionData, err := client.LoadSession(a.SessionPath)
	if err != nil {
		return fmt.Errorf("load session (run 'gmessages-mcp pair' first): %w", err)
	}

	cli, err := client.NewFromSession(sessionData, a.Logger)
	if err != nil {
		return fmt.Errorf("create client: %w", err)
	}
	a.Client = cli

	a.EventHandler = &client.EventHandler{
		Store:       a.Store,
		Logger:      a.Logger,
		SessionPath: a.SessionPath,
		Client:      cli,
		OnDisconnect: func() {
			a.Connected.Store(false)
			a.Logger.Warn().Msg("Disconnected from Google Messages")
		},
	}
	cli.GM.SetEventHandler(a.EventHandler.Handle)

	if err := cli.GM.Connect(); err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	a.Connected.Store(true)
	a.Logger.Info().Msg("Connected to Google Messages")
	return nil
}

// Unpair deletes the session file so the app can re-pair.
func (a *App) Unpair() error {
	a.Connected.Store(false)
	if a.Client != nil {
		a.Client.GM.Disconnect()
		a.Client = nil
	}
	if err := os.Remove(a.SessionPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove session: %w", err)
	}
	a.Logger.Info().Msg("Unpaired — session deleted")
	return nil
}

func (a *App) Close() {
	if a.Client != nil {
		a.Client.GM.Disconnect()
	}
	if a.Store != nil {
		a.Store.Close()
	}
}
