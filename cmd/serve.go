package cmd

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/rs/zerolog"
	"golang.org/x/term"

	"github.com/maxghenis/openmessage/internal/app"
	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/importer"
	"github.com/maxghenis/openmessage/internal/notify"
	"github.com/maxghenis/openmessage/internal/tools"
	"github.com/maxghenis/openmessage/internal/web"
)

type serveOptions struct {
	demo bool
}

func RunServe(logger zerolog.Logger, args ...string) error {
	opts, err := parseServeOptions(args)
	if err != nil {
		return err
	}
	restoreEnv := configureServeEnv(opts)
	defer restoreEnv()

	a, err := app.New(logger)
	if err != nil {
		return fmt.Errorf("init app: %w", err)
	}
	defer a.Close()

	interactiveTerminal := term.IsTerminal(int(os.Stdin.Fd()))
	port := os.Getenv("OPENMESSAGES_PORT")
	if port == "" {
		port = "7007"
	}
	host := os.Getenv("OPENMESSAGES_HOST")
	if host == "" {
		host = "127.0.0.1"
	}
	listenAddr := net.JoinHostPort(host, port)
	baseURL := "http://" + net.JoinHostPort(publicHost(host), port)
	isDemo := app.DemoMode()

	events := web.NewEventBroker()
	isConnected := func() bool {
		if isDemo {
			return true
		}
		return a.AnyConnected()
	}
	publishOverallStatus := func() {
		events.PublishStatus(isConnected())
	}
	a.OnConversationsChange = events.PublishConversations
	a.OnMessagesChange = events.PublishMessages
	a.OnStatusChange = func(bool) {
		publishOverallStatus()
	}
	a.OnTypingChange = events.PublishTyping
	a.OnWhatsAppStatusChange = func() {
		publishOverallStatus()
	}
	a.OnSignalStatusChange = func() {
		publishOverallStatus()
	}
	identityName := app.LocalIdentityName()
	macNotifier := notify.NewMacOSNotifier(logger, macOSNotificationsEnabled(interactiveTerminal), baseURL, a.Store, identityName)
	if macNotifier.Enabled() {
		logger.Info().Msg("Native macOS notifications enabled for fresh inbound messages")
	}
	a.OnIncomingMessage = macNotifier.NotifyIncomingMessage

	// Connect to Google Messages (skip in demo mode)
	if !isDemo {
		if err := a.LoadAndConnect(); err != nil {
			logger.Warn().Err(err).Msg("Google Messages unavailable")
		} else {
			mode := startupBackfillMode()
			runShallowBackfill := func() {
				go func() {
					if err := a.Backfill(); err != nil {
						logger.Warn().Err(err).Msg("Backfill failed")
					}
				}()
			}
			switch mode {
			case "off":
				logger.Info().Msg("Startup backfill disabled")
			case "deep":
				if a.StartDeepBackfill() {
					logger.Info().Msg("Started deep startup backfill")
				}
			case "shallow":
				runShallowBackfill()
			default:
				smsCount, err := a.Store.MessageCount("sms")
				if err != nil {
					logger.Warn().Err(err).Msg("Failed to inspect local SMS cache; falling back to shallow backfill")
					runShallowBackfill()
				} else if smsCount == 0 {
					if a.StartDeepBackfill() {
						logger.Info().Msg("No cached SMS history found; started deep startup backfill")
					}
				} else {
					runShallowBackfill()
				}
			}
		}
	} else {
		logger.Info().Msg("Demo mode — skipping phone connection")
	}

	if !isDemo {
		if err := a.LoadAndConnectWhatsApp(); err != nil {
			logger.Warn().Err(err).Msg("WhatsApp live bridge unavailable")
		}
	} else {
		logger.Info().Msg("Demo mode — skipping WhatsApp live bridge")
	}

	if !isDemo {
		if err := a.LoadAndConnectSignal(); err != nil {
			logger.Warn().Err(err).Msg("Signal live bridge unavailable")
		}
	} else {
		logger.Info().Msg("Demo mode — skipping Signal live bridge")
	}

	if !isDemo {
		go func() {
			ticker := time.NewTicker(5 * time.Second)
			defer ticker.Stop()
			for range ticker.C {
				status := a.WhatsAppStatus()
				if !status.Paired || status.Connected || status.Pairing || status.Connecting {
					continue
				}
				if err := a.StartWhatsAppConnect(); err != nil {
					logger.Warn().Err(err).Msg("WhatsApp reconnect attempt failed")
				}
			}
		}()
	}

	if !isDemo {
		go func() {
			ticker := time.NewTicker(5 * time.Second)
			defer ticker.Stop()
			for range ticker.C {
				status := a.SignalStatus()
				if !status.Paired || status.Connected || status.Pairing || status.Connecting {
					continue
				}
				if err := a.StartSignalConnect(); err != nil {
					logger.Warn().Err(err).Msg("Signal reconnect attempt failed")
				}
			}
		}()
	}

	// Sync WhatsApp and iMessage periodically (every 30s, incremental)
	lastImportErr := map[string]string{}
	syncLocalPlatforms := func() {
		if app.Sandboxed() || isDemo {
			return
		}
		changed := false
		syncPlatform := func(platform, successMsg string, importFromDB func(*db.Store) (*importer.ImportResult, error)) {
			result, err := importFromDB(a.Store)
			if err != nil {
				logSyncError(logger, lastImportErr, platform, err)
				return
			}
			if result.MessagesImported == 0 {
				return
			}

			lastImportErr[platform] = ""
			changed = true
			logger.Info().
				Int("messages", result.MessagesImported).
				Int("conversations", result.ConversationsCreated).
				Msg(successMsg)
		}

		if !a.UsesWhatsAppLiveBridge() {
			syncPlatform("whatsapp", "WhatsApp sync complete", func(store *db.Store) (*importer.ImportResult, error) {
				return (&importer.WhatsAppNative{MyName: identityName}).ImportFromDB(store)
			})
		}
		if signalStatus := a.SignalStatus(); signalStatus.Paired {
			syncPlatform("signal", "Signal desktop sync complete", func(store *db.Store) (*importer.ImportResult, error) {
				return (&importer.SignalDesktop{
					MyName:    identityName,
					MyAddress: signalStatus.Account,
				}).ImportFromDB(store)
			})
		}
		syncPlatform("imessage", "iMessage sync complete", func(store *db.Store) (*importer.ImportResult, error) {
			return (&importer.IMessage{MyName: identityName}).ImportFromDB(store)
		})
		if changed {
			events.PublishConversations()
			events.PublishMessages("")
		}
	}

	// Run once immediately, then every 30 seconds
	go func() {
		syncLocalPlatforms()
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			syncLocalPlatforms()
		}
	}()

	// Create MCP server
	mcpSrv := mcpserver.NewMCPServer(
		"openmessage",
		"0.1.0",
		mcpserver.WithToolCapabilities(true),
	)
	tools.Register(mcpSrv, a)

	// Create SSE transport for MCP, mounted at /mcp/
	sseSrv := mcpserver.NewSSEServer(mcpSrv,
		mcpserver.WithBaseURL(baseURL),
		mcpserver.WithStaticBasePath("/mcp"),
	)

	googleStatus := func() any {
		if isDemo {
			return app.GoogleStatusSnapshot{Connected: true, Paired: true, NeedsPairing: false}
		}
		return a.GoogleStatus()
	}

	httpHandler := web.APIHandlerWithOptions(a.Store, nil, logger, sseSrv, web.APIOptions{
		Client:             a.GetClient,
		Events:             events,
		IdentityName:       identityName,
		IsConnected:        isConnected,
		GoogleStatus:       googleStatus,
		ReconnectGoogle:    a.ReconnectGoogleMessages,
		Unpair:             a.Unpair,
		WhatsAppStatus:     func() any { return a.WhatsAppStatus() },
		ConnectWhatsApp:    a.StartWhatsAppConnect,
		UnpairWhatsApp:     a.UnpairWhatsApp,
		SignalStatus:       func() any { return a.SignalStatus() },
		ConnectSignal:      a.StartSignalConnect,
		UnpairSignal:       a.UnpairSignal,
		LeaveWhatsAppGroup: a.LeaveWhatsAppGroup,
		WhatsAppQRCode: func() (any, error) {
			return a.WhatsAppQRCode()
		},
		SignalQRCode: func() (any, error) {
			return a.SignalQRCode()
		},
		SendWhatsAppText:      a.SendWhatsAppText,
		SendWhatsAppReaction:  a.SendWhatsAppReaction,
		SendSignalText:        a.SendSignalText,
		SendSignalMedia:       a.SendSignalMedia,
		SendSignalReaction:    a.SendSignalReaction,
		SendWhatsAppMedia:     a.SendWhatsAppMedia,
		WhatsAppAvatar:        a.WhatsAppAvatar,
		DownloadWhatsAppMedia: a.DownloadWhatsAppMedia,
		DownloadSignalMedia:   a.DownloadSignalMedia,
		StartDeepBackfill:     a.StartDeepBackfill,
		BackfillStatus:        func() any { return a.GetBackfillProgress() },
		BackfillPhone:         a.BackfillConversationByPhone,
	})
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", listenAddr, err)
	}
	go func() {
		logger.Info().Str("addr", listenAddr).Msg("Web UI available at " + baseURL)
		logger.Info().Str("addr", listenAddr).Msg("MCP SSE available at " + baseURL + "/mcp/sse")
		if err := http.Serve(ln, httpHandler); err != nil {
			logger.Error().Err(err).Msg("HTTP server error")
		}
	}()

	if !interactiveTerminal {
		go func() {
			logger.Info().Msg("Starting MCP stdio transport")
			if err := mcpserver.ServeStdio(mcpSrv); err != nil {
				logger.Warn().Err(err).Msg("MCP stdio server exited")
			}
		}()
	} else {
		logger.Debug().Msg("Skipping MCP stdio transport on interactive terminal")
	}

	// Block until signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
	logger.Info().Msg("Shutting down")
	return nil
}

func RunDemo(logger zerolog.Logger) error {
	return RunServe(logger, "--demo")
}

func parseServeOptions(args []string) (serveOptions, error) {
	opts := serveOptions{}
	for _, arg := range args {
		switch arg {
		case "--demo", "--mock":
			opts.demo = true
		case "":
		default:
			return serveOptions{}, fmt.Errorf("unknown serve option: %s", arg)
		}
	}
	return opts, nil
}

func configureServeEnv(opts serveOptions) func() {
	if !opts.demo {
		return func() {}
	}
	previous, hadPrevious := os.LookupEnv("OPENMESSAGES_DEMO")
	_ = os.Setenv("OPENMESSAGES_DEMO", "1")
	return func() {
		if hadPrevious {
			_ = os.Setenv("OPENMESSAGES_DEMO", previous)
			return
		}
		_ = os.Unsetenv("OPENMESSAGES_DEMO")
	}
}

// LogLevel returns the zerolog level based on OPENMESSAGES_LOG_LEVEL env var.
func LogLevel() zerolog.Level {
	switch os.Getenv("OPENMESSAGES_LOG_LEVEL") {
	case "debug":
		return zerolog.DebugLevel
	case "warn":
		return zerolog.WarnLevel
	case "error":
		return zerolog.ErrorLevel
	case "trace":
		return zerolog.TraceLevel
	default:
		return zerolog.InfoLevel
	}
}

func startupBackfillMode() string {
	mode := strings.ToLower(os.Getenv("OPENMESSAGES_STARTUP_BACKFILL"))
	switch mode {
	case "off", "shallow", "deep":
		return mode
	default:
		return "auto"
	}
}

func macOSNotificationsEnabled(interactive bool) bool {
	mode := strings.ToLower(strings.TrimSpace(os.Getenv("OPENMESSAGES_MACOS_NOTIFICATIONS")))
	switch mode {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}

	if !interactive {
		return false
	}
	return strings.EqualFold(runtimeGOOS(), "darwin")
}

func publicHost(host string) string {
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		return "localhost"
	default:
		return host
	}
}

var runtimeGOOS = func() string {
	return runtime.GOOS
}

func logSyncError(logger zerolog.Logger, lastImportErr map[string]string, platform string, err error) {
	if err == nil {
		lastImportErr[platform] = ""
		return
	}
	msg := err.Error()
	if lastImportErr[platform] == msg {
		return
	}
	lastImportErr[platform] = msg

	lowerMsg := strings.ToLower(msg)
	event := logger.Warn().Err(err).Str("platform", platform)
	if strings.Contains(lowerMsg, "not found") {
		event = logger.Debug().Err(err).Str("platform", platform)
	}
	event.Msg("Local platform sync unavailable")
}
