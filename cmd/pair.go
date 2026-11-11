package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/mdp/qrterminal/v3"
	"github.com/rs/zerolog"
	"go.mau.fi/mautrix-gmessages/pkg/libgm"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/events"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
	utilcurl "go.mau.fi/util/curl"
	"golang.org/x/term"

	"github.com/maxghenis/openmessage/internal/app"
	"github.com/maxghenis/openmessage/internal/client"
	"github.com/maxghenis/openmessage/internal/googlecookies"
	"github.com/maxghenis/openmessage/internal/localapi"
)

const maxQRRefreshes = 5

type pairMode string

const (
	pairModeQR           pairMode = "qr"
	pairModeGoogle       pairMode = "google"
	pairModeGoogleChrome pairMode = "google-chrome"
)

// pairBackendExitWait is how long pair waits for a backend that holds this
// data directory's Google session to let go of it before refusing. The macOS
// app unpairs (POST /api/unpair deletes session.json) and terminates its
// backend, then spawns `pair` without waiting for the exit.
const pairBackendExitWait = 15 * time.Second

// pairProbeTimeout bounds one backend probe; the wait loop caps it further at
// the time left in pairBackendExitWait.
const pairProbeTimeout = 5 * time.Second

// pairDeps are RunPair's seams; tests replace them.
type pairDeps struct {
	probe      pairBackendProbe
	exitWait   time.Duration
	now        func() time.Time
	sleep      func(time.Duration)
	loadChrome func(context.Context, string) (map[string]string, error)
	pair       func(logger zerolog.Logger, mode pairMode, cookies map[string]string, sessionPath string) error
}

func defaultPairDeps() pairDeps {
	return pairDeps{
		probe:      defaultPairBackendProbe,
		exitWait:   pairBackendExitWait,
		now:        time.Now,
		sleep:      time.Sleep,
		loadChrome: googlecookies.Load,
		pair:       runPairing,
	}
}

func RunPair(logger zerolog.Logger, args ...string) error {
	return runPair(logger, args, defaultPairDeps())
}

func runPair(logger zerolog.Logger, args []string, deps pairDeps) error {
	dataDir := app.DefaultDataDir()
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}

	sessionPath := dataDir + "/session.json"
	mode, input, err := resolvePairMode(args)
	if err != nil {
		return err
	}

	ctx := context.Background()
	if err := waitForBackendToReleaseSession(ctx, dataDir, deps.probe, deps.exitWait, deps.now, deps.sleep); err != nil {
		return err
	}

	var cookies map[string]string
	switch mode {
	case pairModeGoogle:
		if cookies, err = parseGoogleCookiesInput(input); err != nil {
			return fmt.Errorf("parse Google cookies: %w", err)
		}
	case pairModeGoogleChrome:
		// Read right before pairing: Google rotates these cookies, and an
		// older extract has failed pairing with HTTP 401.
		if cookies, err = loadChromeCookiesForPairing(ctx, input, deps.loadChrome); err != nil {
			return err
		}
	}

	// Back up right before the phone is asked anything: a backup that fails
	// after the user confirmed would throw away a pairing the phone already
	// made.
	backup, err := backupReplacedSession(sessionPath, deps.now())
	if err != nil {
		return fmt.Errorf("back up %s before pairing: %w; fix its permissions or move it aside, then retry", sessionPath, err)
	}
	if backup != "" {
		fmt.Println("Current session backed up to", backup)
	}
	return deps.pair(logger, mode, cookies, sessionPath)
}

func runPairing(logger zerolog.Logger, mode pairMode, cookies map[string]string, sessionPath string) error {
	cli := client.NewForPairing(logger)
	defer cli.GM.Disconnect()
	if mode == pairModeQR {
		return runQRPairing(logger, cli, sessionPath)
	}
	return runGoogleAccountPairing(cli, sessionPath, cookies)
}

// pairBackendState is what a probe of the local API port found.
type pairBackendState struct {
	Running bool
	// StatusKnown is set when the backend answered with a status document.
	StatusKnown bool
	// DataDir is the data directory the backend serves, when it says.
	DataDir string
	// NoGoogleSession is set only when the status says google.paired,
	// google.connected and (where reported) google.client_loaded are all
	// false. paired=false alone only means session.json is gone; a client
	// still loaded rewrites that file on its next token refresh or cookie
	// rotation. Unpair (the macOS app's re-pair flow) drops the client, and a
	// first pairing in Docker never had one.
	NoGoogleSession bool
}

// mayRewriteSession reports whether this backend could overwrite dataDir's
// session.json: it rewrites the file when it refreshes tokens and rotates
// cookies, and it keeps using its old session until it restarts. Anything
// that answers without a usable status is assumed to.
func (s pairBackendState) mayRewriteSession(dataDir string) bool {
	switch {
	case !s.Running:
		return false
	case !s.StatusKnown:
		return true
	case s.DataDir != "" && !samePath(s.DataDir, dataDir):
		return false
	default:
		return !s.NoGoogleSession
	}
}

type pairBackendProbe func(ctx context.Context, dataDir string) (pairBackendState, error)

func defaultPairBackendProbe(ctx context.Context, dataDir string) (pairBackendState, error) {
	probeURL := defaultBackendProbeURL()
	running, err := probeBackend(ctx, &http.Client{}, probeURL)
	if err != nil || !running {
		return pairBackendState{Running: running}, err
	}
	// The control token goes only to a backend on this machine.
	token := ""
	if pairProbeHostIsLocal() {
		token = localapi.LoadControlToken(dataDir)
	}
	raw, err := localapi.NewClient(strings.TrimSuffix(probeURL, "/api/status"), token).RawStatus(ctx)
	if err != nil {
		return pairBackendState{Running: true}, nil
	}
	state := pairBackendState{Running: true, StatusKnown: true}
	if auth, ok := raw["auth"].(map[string]any); ok {
		if dir, ok := auth["data_dir"].(string); ok {
			state.DataDir = strings.TrimSpace(dir)
		}
	}
	if google, ok := raw["google"].(map[string]any); ok {
		paired, pairedOK := google["paired"].(bool)
		connected, connectedOK := google["connected"].(bool)
		loaded, loadedOK := google["client_loaded"].(bool)
		// Daemons that predate client_loaded don't report it.
		state.NoGoogleSession = pairedOK && !paired && connectedOK && !connected && (!loadedOK || !loaded)
	}
	return state, nil
}

func pairProbeHostIsLocal() bool {
	host := strings.TrimSpace(os.Getenv("OPENMESSAGES_HOST"))
	if host == "" || strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && (ip.IsLoopback() || ip.IsUnspecified())
}

// waitForBackendToReleaseSession refuses to pair while a backend that may
// rewrite dataDir's session.json answers on the API port (see
// mayRewriteSession), after waiting up to wait for it to stop. A probe that
// fails other than by a refused connection (a hung backend) also counts as
// one. wait bounds wall time: each probe's timeout is capped at what is left.
func waitForBackendToReleaseSession(
	ctx context.Context,
	dataDir string,
	probe pairBackendProbe,
	wait time.Duration,
	now func() time.Time,
	sleep func(time.Duration),
) error {
	const poll = 500 * time.Millisecond
	deadline := now().Add(wait)
	announced := false
	for {
		timeout := min(pairProbeTimeout, max(deadline.Sub(now()), poll))
		probeCtx, cancel := context.WithTimeout(ctx, timeout)
		state, err := probe(probeCtx, dataDir)
		cancel()
		if err == nil && !state.mayRewriteSession(dataDir) {
			return nil
		}
		left := deadline.Sub(now())
		if left <= 0 {
			if err != nil {
				return fmt.Errorf("cannot rule out an OpenMessage backend at %s that holds the Google session in %s (%v); stop it and retry", defaultBackendProbeURL(), dataDir, err)
			}
			return fmt.Errorf("an OpenMessage backend at %s holds the Google session in %s; stop it first (macOS app: osascript -e 'quit app \"OpenMessage\"'; Docker: stop the container, then docker compose run --rm openmessage pair), because it rewrites session.json and would overwrite the new pairing", defaultBackendProbeURL(), dataDir)
		}
		if !announced {
			fmt.Printf("Waiting for the OpenMessage backend at %s to stop...\n", defaultBackendProbeURL())
			announced = true
		}
		sleep(min(poll, left))
	}
}

// gaiaCancelGrace is how long pair waits for libgm to return once a Google
// pairing is canceled. Not every libgm wait honours the context: the
// long-poll connect wait in StartGaiaPairing is a bare sync.WaitGroup, and
// its HTTP requests use context.TODO, so a cancel alone can leave pair
// blocked. After the grace, pair returns and the process exits.
const gaiaCancelGrace = 30 * time.Second

// withGaiaConfirmTimeout runs a Google-account pairing and gives up when the
// phone has not confirmed within timeout. It cancels rather than letting a
// deadline expire because libgm sends the phone a pairing cancel only for
// context.Canceled.
func withGaiaConfirmTimeout(ctx context.Context, timeout, grace time.Duration, pair func(context.Context) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var timedOut atomic.Bool
	timer := time.AfterFunc(timeout, func() {
		timedOut.Store(true)
		cancel()
	})
	defer timer.Stop()
	done := make(chan error, 1)
	go func() { done <- pair(ctx) }()
	var err error
	select {
	case err = <-done:
	case <-ctx.Done():
		select {
		case err = <-done:
		case <-time.After(grace):
			err = fmt.Errorf("pairing did not stop within %v of being canceled: %w", grace, ctx.Err())
		}
	}
	if err != nil && timedOut.Load() {
		return fmt.Errorf("the phone did not confirm within %v; run pair again when you can tap the emoji in Google Messages: %w", timeout, err)
	}
	return err
}

// loadChromeCookiesForPairing reads the Google account cookies of a local
// Chrome profile, the same cookies the backend's self-heal refreshes the
// session from. It prints how many it read, never their values.
func loadChromeCookiesForPairing(ctx context.Context, profile string, load func(context.Context, string) (map[string]string, error)) (map[string]string, error) {
	cookies, err := load(ctx, profile)
	if err != nil {
		return nil, fmt.Errorf("read Google cookies from Chrome profile %s: %w; sign that profile in to the Google account your phone's Google Messages uses, or pass another profile", profile, err)
	}
	fmt.Printf("Read %d Google cookies from Chrome profile %s\n", len(cookies), profile)
	return cookies, nil
}

// backupReplacedSession copies an existing session.json to
// session.json.bak-<time> (never overwriting an earlier backup), so a re-pair
// can be rolled back by moving that file back. It returns "" when there is no
// session to back up.
func backupReplacedSession(sessionPath string, now time.Time) (string, error) {
	raw, err := os.ReadFile(sessionPath)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	base := sessionPath + ".bak-" + now.Format("20060102-150405")
	for i := 1; i <= 100; i++ {
		path := base
		if i > 1 {
			path = fmt.Sprintf("%s-%d", base, i)
		}
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		_, writeErr := file.Write(raw)
		syncErr := file.Sync()
		closeErr := file.Close()
		if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
			_ = os.Remove(path)
			return "", err
		}
		return path, nil
	}
	return "", fmt.Errorf("no free backup name for %s", base)
}

func runQRPairing(logger zerolog.Logger, cli *client.Client, sessionPath string) error {
	var pairDone sync.WaitGroup
	pairDone.Add(1)
	var pairErr error

	pairCB := func(data *gmproto.PairedData) {
		defer pairDone.Done()

		logger.Info().
			Str("phone_id", data.GetMobile().GetSourceID()).
			Msg("Pairing successful!")

		sessionData, err := cli.SessionData()
		if err != nil {
			pairErr = fmt.Errorf("get session data: %w", err)
			return
		}
		if err := client.SaveSession(sessionPath, sessionData); err != nil {
			pairErr = fmt.Errorf("save session: %w", err)
			return
		}
		fmt.Println("\nSession saved to", sessionPath)
		fmt.Println("You can now run: openmessage serve")
	}
	cli.GM.PairCallback.Store(&pairCB)

	// Handle Ctrl+C
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Println("\nAborted.")
		cli.GM.Disconnect()
		os.Exit(1)
	}()

	// Start login - shows first QR code
	qrURL, err := cli.GM.StartLogin()
	if err != nil {
		return fmt.Errorf("start login: %w", err)
	}
	displayQR(qrURL)

	// Auto-refresh QR codes
	go func() {
		for i := 0; i < maxQRRefreshes; i++ {
			time.Sleep(30 * time.Second)
			newURL, err := cli.GM.RefreshPhoneRelay()
			if err != nil {
				logger.Warn().Err(err).Msg("Failed to refresh QR code")
				return
			}
			fmt.Println("\n--- QR code refreshed ---")
			displayQR(newURL)
		}
	}()

	// Also set event handler for non-pair events during pairing
	cli.GM.SetEventHandler(func(evt any) {
		switch evt := evt.(type) {
		case *events.ListenFatalError:
			logger.Error().Err(evt.Error).Msg("Fatal error during pairing")
		case *events.PairSuccessful:
			// Handled by PairCallback
		default:
			logger.Debug().Type("type", evt).Msg("Event during pairing")
		}
	})

	pairDone.Wait()
	return pairErr
}

func displayQR(url string) {
	fmt.Println("\nScan this QR code with Google Messages:")
	fmt.Println("(Settings > Device pairing > Pair a device)")
	fmt.Println()
	qrterminal.GenerateHalfBlock(url, qrterminal.L, os.Stdout)
	fmt.Println()
	fmt.Println("URL:", url)
}

func resolvePairMode(args []string) (pairMode, string, error) {
	if len(args) == 0 {
		return pairModeQR, "", nil
	}
	switch args[0] {
	case "--google", "google":
		if term.IsTerminal(int(os.Stdin.Fd())) {
			fmt.Println("Paste a Google cookie JSON object or a cURL command copied from browser devtools, then press Ctrl-D:")
		}
		input, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", "", fmt.Errorf("read Google cookies from stdin: %w", err)
		}
		return pairModeGoogle, string(input), nil
	case "--google-stdin":
		input, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", "", fmt.Errorf("read Google cookies from stdin: %w", err)
		}
		return pairModeGoogle, string(input), nil
	case "--google-file":
		if len(args) < 2 {
			return "", "", fmt.Errorf("--google-file requires a path")
		}
		b, err := os.ReadFile(args[1])
		if err != nil {
			return "", "", fmt.Errorf("read Google cookies file: %w", err)
		}
		return pairModeGoogle, string(b), nil
	case "--google-chrome":
		if runtime.GOOS != "darwin" {
			return "", "", fmt.Errorf("--google-chrome reads Chrome's cookies through the macOS keychain and works only on macOS; use --google-file or --google-stdin")
		}
		profile := googlecookies.DefaultChromeProfile()
		if len(args) >= 2 && strings.TrimSpace(args[1]) != "" {
			profile = chromeProfileDir(strings.TrimSpace(args[1]), profile)
		}
		if profile == "" {
			return "", "", fmt.Errorf("--google-chrome: no Chrome profile directory; pass one or set OPENMESSAGE_CHROME_PROFILE")
		}
		return pairModeGoogleChrome, profile, nil
	default:
		return "", "", fmt.Errorf("unknown pair option: %s", args[0])
	}
}

// gaiaConfirmTimeout bounds the wait for the emoji tap. libgm sends the
// finish request with a 300 s TTL and then waits for the phone's answer with
// no deadline of its own, so an unanswered prompt blocked `pair` for good
// (2026-10-16: still waiting after 10 minutes) while the app stayed quit.
const gaiaConfirmTimeout = 5 * time.Minute

// pairSignalContext is canceled by the first Ctrl-C or SIGTERM. That signal
// also restores the default action, so a second one ends the process even if
// libgm is stuck in a wait that ignores the context.
func pairSignalContext() (context.Context, context.CancelFunc) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ctx.Done()
		stop()
	}()
	return ctx, stop
}

// chromeProfileDir resolves a --google-chrome argument. A bare profile name
// such as "Profile 1" names a directory beside the default profile; anything
// with a path separator is a path.
func chromeProfileDir(arg, defaultProfile string) string {
	if strings.ContainsRune(arg, filepath.Separator) || defaultProfile == "" {
		return arg
	}
	return filepath.Join(filepath.Dir(defaultProfile), arg)
}

func runGoogleAccountPairing(cli *client.Client, sessionPath string, cookies map[string]string) error {
	cli.GM.AuthData.Cookies = cookies

	fmt.Println("Starting Google account pairing...")
	ctx, stop := pairSignalContext()
	defer stop()
	err := withGaiaConfirmTimeout(ctx, gaiaConfirmTimeout, gaiaCancelGrace, func(ctx context.Context) error {
		return cli.GM.DoGaiaPairing(ctx, func(emoji string) {
			fmt.Println("EMOJI:", emoji)
			fmt.Println("Tap this emoji in Google Messages on your phone.")
		})
	})
	if err != nil {
		return fmt.Errorf("google account pairing: %w", err)
	}

	sessionData, err := cli.SessionData()
	if err != nil {
		return fmt.Errorf("get session data: %w", err)
	}
	if err := client.SaveSession(sessionPath, sessionData); err != nil {
		return fmt.Errorf("save session: %w", err)
	}
	fmt.Println("\nPairing successful!")
	fmt.Println("Session saved to", sessionPath)
	fmt.Println("You can now run: openmessage serve")
	return nil
}

func parseGoogleCookiesInput(raw string) (map[string]string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, fmt.Errorf("no cookies provided")
	}

	var cookieMap map[string]string
	if strings.HasPrefix(trimmed, "{") {
		if err := json.Unmarshal([]byte(trimmed), &cookieMap); err == nil && len(cookieMap) > 0 {
			return cookieMap, nil
		}
	}

	if strings.HasPrefix(trimmed, "curl ") {
		parsed, err := utilcurl.Parse(trimmed)
		if err != nil {
			return nil, fmt.Errorf("parse cURL command: %w", err)
		}
		if cookies, err := parseCookieHeader(parsed.Header.Get("Cookie")); err == nil {
			return cookies, nil
		}
		return nil, fmt.Errorf("cURL command did not include a Cookie header")
	}

	return parseCookieHeader(trimmed)
}

func parseCookieHeader(raw string) (map[string]string, error) {
	header := strings.TrimSpace(raw)
	if header == "" {
		return nil, fmt.Errorf("no cookie header provided")
	}
	if strings.HasPrefix(strings.ToLower(header), "cookie:") {
		header = strings.TrimSpace(header[len("cookie:"):])
	}
	req := &http.Request{Header: make(http.Header)}
	req.Header.Set("Cookie", header)
	parsed := map[string]string{}
	for _, cookie := range req.Cookies() {
		name := strings.TrimSpace(cookie.Name)
		if name == "" {
			continue
		}
		parsed[name] = cookie.Value
	}
	if len(parsed) == 0 {
		return nil, fmt.Errorf("no cookies found")
	}
	return parsed, nil
}

// Ensure PairCallback type matches what libgm expects
var _ = (*libgm.Client)(nil)
