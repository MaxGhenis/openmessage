package cmd

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"
	"time"

	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/rs/zerolog"
	"golang.org/x/term"

	"github.com/maxghenis/openmessage/internal/app"
	"github.com/maxghenis/openmessage/internal/bridge"
	googleadapter "github.com/maxghenis/openmessage/internal/bridgeadapters/google"
	signaladapter "github.com/maxghenis/openmessage/internal/bridgeadapters/signal"
	whatsappadapter "github.com/maxghenis/openmessage/internal/bridgeadapters/whatsapp"
	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/googlecookies"
	"github.com/maxghenis/openmessage/internal/importer"
	"github.com/maxghenis/openmessage/internal/ingest"
	"github.com/maxghenis/openmessage/internal/notify"
	"github.com/maxghenis/openmessage/internal/telemetry"
	"github.com/maxghenis/openmessage/internal/tools"
	"github.com/maxghenis/openmessage/internal/web"
	"github.com/maxghenis/openmessage/internal/whatsapplive"
)

type serveOptions struct {
	demo     bool
	web      bool
	mcpSSE   bool
	mcpStdio bool
}

func mcpV2Dependencies(stack *v2Stack) []*tools.V2Dependencies {
	if stack == nil {
		return nil
	}
	return []*tools.V2Dependencies{{
		Enabled:  true,
		Service:  stack.Service,
		V2Store:  stack.Store,
		Registry: stack.Registry,
	}}
}

func v2SendWebOptions(stack *v2Stack, enabled bool) *web.V2Options {
	if stack == nil || !enabled {
		return nil
	}
	return &web.V2Options{
		Service:  stack.Service,
		Media:    stack.Media,
		V2Store:  stack.Store,
		Blobs:    stack.Blobs,
		Registry: stack.Registry,
	}
}

func v2IngestCountersProvider(stack *v2Stack) func() map[string]ingest.CounterSnapshot {
	if stack == nil {
		return nil
	}
	return func() map[string]ingest.CounterSnapshot {
		return stack.Counters.PerAccount()
	}
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
	previousUmask := syscall.Umask(0o077)
	defer syscall.Umask(previousUmask)

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
	v2Send := v2SendEnabled()
	v2Ingest := v2IngestEnabled()

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
	a.OnStatusChange = func(bool) { publishOverallStatus() }
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

	var googleLifecycle *googleadapter.Adapter
	var googleSupervisor *bridge.Supervisor
	var googleControl *googleSupervisorControl
	var whatsappLifecycle *whatsappadapter.Adapter
	var whatsappSupervisor *bridge.Supervisor
	var whatsappControl *whatsappSupervisorControl
	var signalLifecycle *signaladapter.Adapter
	var signalControl *signalSupervisorControl
	var stack *v2Stack
	var stopV2Stack func()

	// Google has one lifecycle owner. Transient retry, liveness probes,
	// credential repair, and terminal parking all flow through this supervisor;
	// the legacy Connected flag remains a UI projection only.
	if !isDemo {
		googleLifecycle = googleadapter.New(googleAccountID, a, canRefreshGoogleCookies)
		a.SetGoogleLifecycleNotifier(googleLifecycle)
		if v2Send || v2Ingest {
			stack, err = newV2Stack(v2StackDeps{
				Logger:  logger,
				DataDir: app.DefaultDataDir(),
				Google:  googleLifecycle,
			})
			if err != nil {
				return fmt.Errorf("init v2 stack: %w", err)
			}
			stopV2Stack = func() {
				if err := stack.Store.Close(); err != nil {
					logger.Warn().Err(err).Msg("Failed to close unstarted v2 message store")
				}
			}
			defer func() { stopV2Stack() }()
		} else {
			logger.Info().Msg("V2 stack disabled (flags off)")
		}
		repairer := newGoogleCredentialRepairer(
			a.SessionPath,
			canRefreshGoogleCookies,
			refreshGoogleSessionCookies,
			a.FlagGoogleNeedsRepair,
			a.ClearGoogleRepairFlag,
		)
		newGoogleSupervisor := func() (*bridge.Supervisor, error) {
			supervisorOptions := []bridge.SupervisorOption{
				bridge.WithCredentialRepairer(repairer),
			}
			if stack != nil {
				supervisorOptions = append(supervisorOptions, bridge.WithConnectionSink(stack.Sink))
			}
			return bridge.NewSupervisor(
				googleAccountID,
				bridge.PlatformGoogle,
				googleLifecycle,
				googleSupervisorPolicy(),
				googleWallClock{},
				googleRandom{},
				supervisorOptions...,
			)
		}
		googleSupervisor, err = newGoogleSupervisor()
		if err != nil {
			return fmt.Errorf("init Google Messages supervisor: %w", err)
		}
		googleControl = newGoogleSupervisorControl(googleSupervisor, a.SessionPath, newGoogleSupervisor)
		googleStartupCtx, cancelGoogleStartup := context.WithCancel(context.Background())
		var googleStartupWG sync.WaitGroup
		defer func() {
			cancelGoogleStartup()
			ctx, cancel := context.WithTimeout(context.Background(), googleSupervisorStopTimeout)
			defer cancel()
			if err := googleControl.Stop(ctx); err != nil {
				logger.Warn().Err(err).Msg("Failed to stop Google Messages supervisor cleanly")
			}
			googleStartupWG.Wait()
		}()

		if a.GooglePaired() {
			googleStartupWG.Add(1)
			go func(supervisor *bridge.Supervisor) {
				defer googleStartupWG.Done()
				ctx, cancel := context.WithTimeout(googleStartupCtx, googleSupervisorPolicy().ConnectTimeout)
				defer cancel()
				startErr := supervisor.Start(ctx, bridge.StartRequest{})
				waitErr := waitForGoogleOnline(ctx, supervisor)
				if waitErr != nil {
					logger.Warn().Err(errors.Join(startErr, waitErr)).Msg("Google Messages unavailable")
					return
				}
				select {
				case <-ctx.Done():
					return
				default:
				}
				startGoogleBackfill(a, logger)
			}(googleSupervisor)
		}
	} else {
		logger.Info().Msg("Demo mode — skipping phone connection")
	}

	if !isDemo {
		whatsappBridge, initialized := initializeWhatsAppForServe(logger, a.InitializeWhatsApp)
		if initialized {
			whatsappLifecycle = whatsappadapter.New(whatsappAccountID, whatsappBridge)
			a.SetWhatsAppLifecycleNotifier(whatsappLifecycle)
			if stack != nil {
				if err := stack.RegisterAdapter(whatsappLifecycle); err != nil {
					return fmt.Errorf("register WhatsApp adapter with v2 stack: %w", err)
				}
			}
			newWhatsAppSupervisor := func(initial bridge.State) (*bridge.Supervisor, error) {
				supervisorOptions := []bridge.SupervisorOption{
					bridge.WithPairer(whatsappLifecycle),
					bridge.WithInitialState(initial),
				}
				if stack != nil {
					supervisorOptions = append(supervisorOptions, bridge.WithConnectionSink(stack.Sink))
				}
				return bridge.NewSupervisor(
					whatsappAccountID,
					bridge.PlatformWhatsApp,
					whatsappLifecycle,
					whatsappSupervisorPolicy(),
					whatsappWallClock{},
					whatsappRandom{},
					supervisorOptions...,
				)
			}
			initialState := bridge.StateUnpaired
			if whatsappBridge.PairedAccount().Paired {
				initialState = bridge.StateStopped
			}
			whatsappSupervisor, err = newWhatsAppSupervisor(initialState)
			if err != nil {
				return fmt.Errorf("init WhatsApp supervisor: %w", err)
			}
			whatsappControl = newWhatsAppSupervisorControl(
				whatsappSupervisor,
				newWhatsAppSupervisor,
				func() bool { return whatsappBridge.PairedAccount().Paired },
			)
			whatsappStartupCtx, cancelWhatsAppStartup := context.WithCancel(context.Background())
			var whatsappStartupWG sync.WaitGroup
			defer func() {
				cancelWhatsAppStartup()
				ctx, cancel := context.WithTimeout(context.Background(), whatsappSupervisorStopTimeout)
				defer cancel()
				if err := whatsappControl.Stop(ctx); err != nil {
					logger.Warn().Err(err).Msg("WhatsApp supervisor stop timed out; waiting for generation teardown")
					// App.Close runs after this defer. Wait without a deadline so the
					// whatsmeow session store is never closed under a live generation.
					_ = whatsappControl.Stop(context.Background())
				}
				whatsappStartupWG.Wait()
			}()

			if initialState == bridge.StateStopped {
				whatsappStartupWG.Add(1)
				go func(supervisor *bridge.Supervisor) {
					defer whatsappStartupWG.Done()
					ctx, cancel := context.WithTimeout(whatsappStartupCtx, whatsappSupervisorPolicy().ConnectTimeout)
					defer cancel()
					if err := supervisor.Start(ctx, bridge.StartRequest{}); err != nil {
						logger.Warn().Err(err).Msg("WhatsApp live bridge unavailable")
					}
				}(whatsappSupervisor)
			}
		}
	} else {
		logger.Info().Msg("Demo mode — skipping WhatsApp live bridge")
	}

	if !isDemo {
		signalBridge, err := a.EnsureSignal()
		if err != nil {
			logger.Warn().Err(err).Msg("Signal live bridge unavailable")
		} else {
			signalLifecycle = signaladapter.New(signalAccountID, signalBridge)
			a.SetSignalLifecycleNotifier(signalLifecycle)
			if stack != nil {
				if err := stack.RegisterAdapter(signalLifecycle); err != nil {
					return fmt.Errorf("register Signal adapter with v2 stack: %w", err)
				}
			}
			newSignalSupervisor := func() (*bridge.Supervisor, error) {
				var supervisorOptions []bridge.SupervisorOption
				if stack != nil {
					supervisorOptions = append(supervisorOptions, bridge.WithConnectionSink(stack.Sink))
				}
				return bridge.NewSupervisor(
					signalAccountID,
					bridge.PlatformSignal,
					signalLifecycle,
					signalSupervisorPolicy(),
					signalWallClock{},
					signalRandom{},
					supervisorOptions...,
				)
			}
			signalSupervisor, err := newSignalSupervisor()
			if err != nil {
				return fmt.Errorf("init Signal supervisor: %w", err)
			}
			signalControl = newSignalSupervisorControl(
				signalSupervisor,
				newSignalSupervisor,
				signalLifecycle.InputFingerprint,
			)
			signalStartupCtx, cancelSignalStartup := context.WithCancel(context.Background())
			var signalStartupWG sync.WaitGroup
			defer func() {
				cancelSignalStartup()
				ctx, cancel := context.WithTimeout(context.Background(), signalSupervisorStopTimeout)
				defer cancel()
				if err := signalControl.Stop(ctx); err != nil {
					logger.Warn().Err(err).Msg("Failed to stop Signal supervisor cleanly")
				}
				signalStartupWG.Wait()
			}()

			if signalBridge.Status().Paired {
				signalStartupWG.Add(1)
				go func() {
					defer signalStartupWG.Done()
					if err := signalSupervisor.Start(signalStartupCtx, bridge.StartRequest{}); err != nil &&
						!errors.Is(err, context.Canceled) &&
						!errors.Is(err, bridge.ErrSupervisorStopped) {
						logger.Warn().Err(err).Msg("Signal live bridge unavailable")
					}
				}()
			}
		}
	} else {
		logger.Info().Msg("Demo mode — skipping Signal live bridge")
	}

	if stack != nil {
		stopV2Stack = stack.Start(context.Background(), a.Store, events)
		logger.Info().
			Bool("send", v2Send).
			Bool("ingest", v2Ingest).
			Msg("V2 stack enabled")
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
	v2SendStack := stack
	if !v2Send {
		v2SendStack = nil
	}
	tools.Register(mcpSrv, a, mcpV2Dependencies(v2SendStack)...)

	var mcpHTTPHandler http.Handler
	if opts.mcpSSE {
		mcpHTTPHandler = newMCPHTTPHandler(mcpSrv, baseURL)
	}

	googleStatus := func() any {
		if isDemo {
			return app.GoogleStatusSnapshot{Connected: true, Paired: true, NeedsPairing: false, PhoneResponding: true}
		}
		return a.GoogleStatus()
	}
	recordGoogleSend := a.RecordGoogleSendOutcome
	recordGoogleSendError := a.RecordGoogleSendError
	markGoogleAuthExpired := a.HandleGoogleAuthExpiredError
	reconnectGoogle := a.ReconnectGoogleMessages
	unpairGoogle := a.Unpair
	if googleControl != nil {
		reconnectGoogle = googleControl.Reconnect
		unpairGoogle = func() error {
			return googleControl.StopAndUnpair(a.Unpair)
		}
	}
	connectWhatsApp := a.StartWhatsAppConnect
	pairWhatsAppPhone := a.PairWhatsAppPhone
	unpairWhatsApp := a.UnpairWhatsApp
	if whatsappControl != nil {
		connectWhatsApp = whatsappControl.Reconnect
		pairWhatsAppPhone = whatsappControl.PairPhone
		unpairWhatsApp = func() error {
			return whatsappControl.StopAndUnpair(a.UnpairWhatsApp)
		}
	}
	var connectSignal func() error
	var unpairSignal func() error
	if signalControl != nil {
		connectSignal = signalControl.Connect
		unpairSignal = func() error {
			return signalControl.StopAndUnpair(signalLifecycle.Unpair)
		}
	}

	// Background loop that sends due scheduled ("send later") messages.
	a.StartScheduler()

	v2Options := v2SendWebOptions(stack, v2Send)
	v2IngestCounters := v2IngestCountersProvider(stack)

	httpEnabled := opts.web || opts.mcpSSE
	if httpEnabled {
		httpHandler := http.Handler(nil)
		if opts.web {
			httpHandler = web.APIHandlerWithOptions(a.Store, nil, logger, mcpHTTPHandler, web.APIOptions{
				V2:                    v2Options,
				V2IngestCounters:      v2IngestCounters,
				Client:                a.GetClient,
				Events:                events,
				IdentityName:          identityName,
				IsConnected:           isConnected,
				GoogleStatus:          googleStatus,
				RecordGoogleSend:      recordGoogleSend,
				RecordGoogleSendError: recordGoogleSendError,
				GooglePhoneResponding: a.GooglePhoneResponding,
				MarkGoogleAuthExpired: markGoogleAuthExpired,
				ReconnectGoogle:       reconnectGoogle,
				Unpair:                unpairGoogle,
				WhatsAppStatus:        func() any { return a.WhatsAppStatus() },
				ConnectWhatsApp:       connectWhatsApp,
				PairWhatsAppPhone:     pairWhatsAppPhone,
				UnpairWhatsApp:        unpairWhatsApp,
				SignalStatus:          func() any { return a.SignalStatus() },
				ConnectSignal:         connectSignal,
				ReplaySignalRecovery:  a.ReplaySignalRecoveryQueue,
				UnpairSignal:          unpairSignal,
				LeaveWhatsAppGroup:    a.LeaveWhatsAppGroup,
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
				SyncGoogleContacts:    a.SyncGoogleContacts,
			})
		} else {
			httpHandler = web.ProtectLocalControl(mcpHTTPHandler)
		}

		ln, err := net.Listen("tcp", listenAddr)
		if err != nil {
			return fmt.Errorf("listen on %s: %w", listenAddr, err)
		}
		srv := &http.Server{
			Handler:           httpHandler,
			ReadHeaderTimeout: 10 * time.Second,
			IdleTimeout:       120 * time.Second,
			MaxHeaderBytes:    1 << 20,
		}
		go func() {
			if opts.web {
				logger.Info().Str("addr", listenAddr).Msg("Web UI available at " + baseURL)
			}
			if opts.mcpSSE {
				logger.Info().Str("addr", listenAddr).Msg("MCP SSE available at " + baseURL + "/mcp/sse")
			}
			if err := srv.Serve(ln); err != nil {
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

func initializeWhatsAppForServe(
	logger zerolog.Logger,
	initialize func() (*whatsapplive.Bridge, error),
) (*whatsapplive.Bridge, bool) {
	whatsappBridge, err := initialize()
	if err != nil {
		logger.Warn().Err(err).Msg("WhatsApp live bridge unavailable")
		return nil, false
	}
	return whatsappBridge, true
}

func newMCPHTTPHandler(mcpSrv *mcpserver.MCPServer, baseURL string) http.Handler {
	streamableSrv := mcpserver.NewStreamableHTTPServer(mcpSrv, mcpserver.WithEndpointPath("/mcp"))
	sseSrv := mcpserver.NewSSEServer(mcpSrv,
		mcpserver.WithBaseURL(baseURL),
		mcpserver.WithStaticBasePath("/mcp"),
	)
	mux := http.NewServeMux()
	mux.Handle("/mcp", streamableSrv)
	mux.Handle("/mcp/", sseSrv)
	return mux
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
	opts := serveOptions{web: true}
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

const googleCookieRefreshTimeout = 20 * time.Second

// canRefreshGoogleCookies reports whether an expired Google session can be
// recovered automatically — either via a configured external refresh script or
// the built-in native refresh (macOS with a Chrome profile). When neither is
// available, the adapter classifies auth expiry as a blocked condition and the
// existing needs_repair status prompts a manual re-pair instead of spinning.
func canRefreshGoogleCookies() bool {
	if strings.TrimSpace(os.Getenv("OPENMESSAGE_COOKIE_REFRESH_SCRIPT")) != "" {
		return true
	}
	return googlecookies.NativeSupported()
}

// refreshGoogleSessionCookies rewrites the Google cookies in sessionPath. It
// prefers an explicitly configured OPENMESSAGE_COOKIE_REFRESH_SCRIPT (so the
// operator can override the mechanism), otherwise falls back to the built-in
// native refresh. The supervisor starts a new generation only after this
// function succeeds.
var refreshGoogleSessionCookies = func(ctx context.Context, sessionPath string) error {
	script := strings.TrimSpace(os.Getenv("OPENMESSAGE_COOKIE_REFRESH_SCRIPT"))
	if script != "" {
		if _, err := os.Stat(script); err != nil {
			return fmt.Errorf("refresh script unavailable at %s: %w", script, err)
		}
		cmd := exec.CommandContext(ctx, script, "--quiet", "--no-backup")
		cmd.Env = os.Environ()
		output, err := cmd.CombinedOutput()
		if ctx.Err() != nil {
			return fmt.Errorf("refresh Google cookies timed out after %s", googleCookieRefreshTimeout)
		}
		if err != nil {
			detail := strings.TrimSpace(string(output))
			if detail == "" {
				return fmt.Errorf("refresh Google cookies: %w", err)
			}
			return fmt.Errorf("refresh Google cookies: %w: %s", err, detail)
		}
		return nil
	}

	if !googlecookies.NativeSupported() {
		return nil
	}
	if err := googlecookies.Refresh(ctx, googlecookies.DefaultChromeProfile(), sessionPath); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("refresh Google cookies timed out after %s", googleCookieRefreshTimeout)
		}
		return fmt.Errorf("refresh Google cookies: %w", err)
	}
	return nil
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
