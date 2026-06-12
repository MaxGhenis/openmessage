package cmd

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
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
	"github.com/maxghenis/openmessage/internal/telemetry"
	"github.com/maxghenis/openmessage/internal/tools"
	"github.com/maxghenis/openmessage/internal/web"
)

type serveOptions struct {
	demo     bool
	web      bool
	mcpSSE   bool
	mcpStdio bool
}

// buildVersion is the version string baked in at build time. SetVersion is
// called from main() with the value of main.version (set via -ldflags).
var buildVersion = "dev"

// SetVersion records the build-time version string for use in telemetry, MCP
// server identification, etc.
func SetVersion(v string) {
	if v != "" {
		buildVersion = v
	}
}

// Version returns the build-time version string. Defaults to "dev".
func Version() string {
	return buildVersion
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
				// Skip reconnect when the account needs manual re-pairing.
				// Hammering signal-cli every 5s with a known-bad account
				// wastes resources and produces noise in the logs. The UI
				// will show the needs_reauth state so the user knows to
				// open Platforms and re-pair.
				if status.NeedsReauth {
					continue
				}
				if err := a.StartSignalConnect(); err != nil {
					logger.Warn().Err(err).Msg("Signal reconnect attempt failed")
				}
			}
		}()
	}

	// Google Messages reconnect watchdog. libgm has no auto-reconnect and the
	// long-poll goroutine can die (panic, ping failure, transient fatal error)
	// while the session stays valid. Without this, SMS silently freezes with
	// Connected=true forever. Only reconnect a paired-but-disconnected session;
	// skip when it genuinely needs re-pairing (session deleted) so we don't
	// hammer a known-bad state.
	if !isDemo {
		go func() {
			ticker := time.NewTicker(15 * time.Second)
			defer ticker.Stop()
			for range ticker.C {
				g := a.GoogleStatus()
				if !g.Paired || g.Connected || g.NeedsPairing {
					continue
				}
				logger.Info().Msg("Google Messages disconnected — attempting reconnect")
				if err := a.ReconnectGoogleMessages(); err != nil {
					logger.Warn().Err(err).Msg("Google Messages reconnect attempt failed")
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
		if iMessageSyncSupported() {
			syncPlatform("imessage", "iMessage sync complete", func(store *db.Store) (*importer.ImportResult, error) {
				return (&importer.IMessage{MyName: identityName}).ImportFromDB(store)
			})
		}
		if changed {
			events.PublishConversations()
			events.PublishMessages("")
		}
	}

	// Run once immediately, then every 30 seconds. Each tick is wrapped in a
	// recover() so a panic from one bad row (corrupt iMessage chat.db entry,
	// nil map in an importer, etc.) can't take down the entire backend — we
	// just log, skip this tick, and try again next interval.
	safeSync := func() {
		defer func() {
			if r := recover(); r != nil {
				logger.Error().
					Interface("panic", r).
					Bytes("stack", debug.Stack()).
					Msg("local platform sync panicked; skipping this tick")
			}
		}()
		syncLocalPlatforms()
	}
	go func() {
		safeSync()
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			safeSync()
		}
	}()

	// Belt-and-suspenders: periodically force a full Signal Desktop rescan
	// so any drift that slipped past both the live signal-cli WebSocket AND
	// the 30-second incremental window is eventually recovered. The
	// incremental window is narrow by design (fast ticks), so rare
	// out-of-window losses depend on this wider sweep. 30 minutes balances
	// recovery lag against the cost of a full scan (~1–2s on a typical
	// archive of a few thousand rows).
	safeFullSignalSync := func() {
		defer func() {
			if r := recover(); r != nil {
				logger.Error().
					Interface("panic", r).
					Bytes("stack", debug.Stack()).
					Msg("periodic Signal full rescan panicked; will retry next interval")
			}
		}()
		if !a.SignalStatus().Paired {
			return
		}
		identityName := strings.TrimSpace(os.Getenv("OPENMESSAGES_MY_NAME"))
		imp := &importer.SignalDesktop{
			MyName:    identityName,
			MyAddress: a.SignalStatus().Account,
			SinceMS:   -1, // explicit full scan, ignore incremental window
		}
		result, err := imp.ImportFromDB(a.Store)
		if err != nil {
			logger.Debug().Err(err).Msg("Periodic Signal full rescan failed")
			return
		}
		if result.MessagesImported > 0 {
			logger.Info().
				Int("recovered", result.MessagesImported).
				Msg("Periodic Signal full rescan recovered drifted messages")
			events.PublishConversations()
			events.PublishMessages("")
		}
	}
	go func() {
		// First full rescan 2 minutes after startup so a crash-restart
		// cycle doesn't hammer the DB immediately, then every 30 minutes.
		time.Sleep(2 * time.Minute)
		safeFullSignalSync()
		ticker := time.NewTicker(30 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			safeFullSignalSync()
		}
	}()

	// Create MCP server
	mcpSrv := mcpserver.NewMCPServer(
		"openmessage",
		buildVersion,
		mcpserver.WithToolCapabilities(true),
	)
	tools.Register(mcpSrv, a)

	var sseSrv http.Handler
	if opts.mcpSSE {
		sseSrv = mcpserver.NewSSEServer(mcpSrv,
			mcpserver.WithBaseURL(baseURL),
			mcpserver.WithStaticBasePath("/mcp"),
		)
	}

	googleStatus := func() any {
		if isDemo {
			return app.GoogleStatusSnapshot{Connected: true, Paired: true, NeedsPairing: false}
		}
		return a.GoogleStatus()
	}

	httpEnabled := opts.web || opts.mcpSSE
	if httpEnabled {
		httpHandler := http.Handler(nil)
		if opts.web {
			httpHandler = web.APIHandlerWithOptions(a.Store, nil, logger, sseSrv, web.APIOptions{
				Client:               a.GetClient,
				Events:               events,
				IdentityName:         identityName,
				IsConnected:          isConnected,
				GoogleStatus:         googleStatus,
				ReconnectGoogle:      a.ReconnectGoogleMessages,
				Unpair:               a.Unpair,
				WhatsAppStatus:       func() any { return a.WhatsAppStatus() },
				ConnectWhatsApp:      a.StartWhatsAppConnect,
				UnpairWhatsApp:       a.UnpairWhatsApp,
				SignalStatus:         func() any { return a.SignalStatus() },
				ConnectSignal:        a.StartSignalConnect,
				ReplaySignalRecovery: a.ReplaySignalRecoveryQueue,
				UnpairSignal:         a.UnpairSignal,
				LeaveWhatsAppGroup:   a.LeaveWhatsAppGroup,
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
		} else {
			mux := http.NewServeMux()
			if sseSrv != nil {
				mux.Handle("/mcp/", sseSrv)
			}
			httpHandler = mux
		}

		ln, err := net.Listen("tcp", listenAddr)
		if err != nil {
			return fmt.Errorf("listen on %s: %w", listenAddr, err)
		}
		go func() {
			if opts.web {
				logger.Info().Str("addr", listenAddr).Msg("Web UI available at " + baseURL)
			}
			if opts.mcpSSE {
				logger.Info().Str("addr", listenAddr).Msg("MCP SSE available at " + baseURL + "/mcp/sse")
			}
			if err := http.Serve(ln, httpHandler); err != nil {
				logger.Error().Err(err).Msg("HTTP server error")
			}
		}()
	}

	if opts.mcpStdio {
		if !httpEnabled {
			logger.Info().Msg("Starting MCP stdio transport")
			return mcpserver.ServeStdio(mcpSrv)
		}
		go func() {
			logger.Info().Msg("Starting MCP stdio transport")
			if err := mcpserver.ServeStdio(mcpSrv); err != nil {
				logger.Warn().Err(err).Msg("MCP stdio server exited")
			}
		}()
	}

	// Send anonymous heartbeat (opt-in only, off by default).
	// Enable with `OPENMESSAGE_TELEMETRY=1`. Skipped in demo mode.
	if !isDemo && os.Getenv("OPENMESSAGE_TELEMETRY") == "1" {
		go func() {
			tc := telemetry.New(app.DefaultDataDir(), buildVersion, true)
			ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
			defer cancel()
			tc.MaybeSend(ctx, telemetrySnapshot(a, googleStatus))
		}()
	}

	// Block until signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
	logger.Info().Msg("Shutting down")
	return nil
}

// telemetrySnapshot extracts the minimal pairing-status data sent in heartbeats.
// No conversation contents, contact info, or anything user-identifying.
func telemetrySnapshot(a *app.App, googleStatus func() any) telemetry.PlatformStatus {
	status := telemetry.PlatformStatus{}
	if g, ok := googleStatus().(app.GoogleStatusSnapshot); ok {
		status.GoogleMessages = g.Connected || g.Paired
	}
	if w := a.WhatsAppStatus(); w.Connected || w.Paired {
		status.WhatsApp = true
	}
	if s := a.SignalStatus(); s.Connected || s.Paired {
		status.Signal = true
	}
	return status
}

func RunDemo(logger zerolog.Logger) error {
	return RunServe(logger, "--demo")
}

func parseServeOptions(args []string) (serveOptions, error) {
	opts := serveOptions{
		web:    true,
		mcpSSE: true,
	}
	transportFlagsSeen := false
	enableExplicitTransportMode := func() {
		if transportFlagsSeen {
			return
		}
		transportFlagsSeen = true
		opts.web = false
		opts.mcpSSE = false
		opts.mcpStdio = false
	}
	for _, arg := range args {
		switch arg {
		case "--demo":
			opts.demo = true
		case "--web":
			enableExplicitTransportMode()
			opts.web = true
		case "--no-web":
			enableExplicitTransportMode()
			opts.web = false
		case "--mcp-sse":
			enableExplicitTransportMode()
			opts.mcpSSE = true
		case "--no-mcp-sse":
			enableExplicitTransportMode()
			opts.mcpSSE = false
		case "--mcp-stdio":
			enableExplicitTransportMode()
			opts.mcpStdio = true
		case "--no-mcp-stdio":
			enableExplicitTransportMode()
			opts.mcpStdio = false
		case "":
		default:
			return serveOptions{}, fmt.Errorf("unknown serve option: %s", arg)
		}
	}
	if !opts.web && !opts.mcpSSE && !opts.mcpStdio {
		return serveOptions{}, fmt.Errorf("serve requires at least one enabled transport: web, mcp-sse, or mcp-stdio")
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
	return isDarwin()
}

func iMessageSyncSupported() bool {
	return isDarwin()
}

func isDarwin() bool {
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
