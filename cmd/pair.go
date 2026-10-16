package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
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

// pairBackendExitWait is how long pair waits for a backend serving the same
// data directory to exit before refusing. The macOS app terminates its backend
// and spawns `pair` without waiting for the exit, so a short wait keeps the
// in-app flow working while a backend that stays up is still refused.
const pairBackendExitWait = 15 * time.Second

func RunPair(logger zerolog.Logger, args ...string) error {
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
	if err := waitForBackendToReleaseSession(ctx, dataDir, defaultPairBackendProbe, pairBackendExitWait, time.Sleep); err != nil {
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
		if cookies, err = loadChromeCookiesForPairing(ctx, input, googlecookies.Load); err != nil {
			return err
		}
	}

	cli := client.NewForPairing(logger)
	defer cli.GM.Disconnect()

	if mode == pairModeQR {
		return runQRPairing(logger, cli, sessionPath)
	}
	return runGoogleAccountPairing(cli, sessionPath, cookies)
}

// pairBackendProbe reports whether an OpenMessage backend answers on the
// local API port and, when its status says so, which data directory it serves.
type pairBackendProbe func(ctx context.Context, dataDir string) (running bool, servedDataDir string, err error)

func defaultPairBackendProbe(ctx context.Context, dataDir string) (bool, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	probeURL := defaultBackendProbeURL()
	running, err := probeBackend(ctx, &http.Client{}, probeURL)
	if err != nil || !running {
		return running, "", err
	}
	daemon := localapi.NewClient(strings.TrimSuffix(probeURL, "/api/status"), localapi.LoadControlToken(dataDir))
	status, _, err := daemon.Status(ctx)
	if err != nil {
		// Something answers but does not say which data directory it serves.
		return true, "", nil
	}
	return true, strings.TrimSpace(status.Auth.DataDir), nil
}

// waitForBackendToReleaseSession refuses to pair while a running backend
// serves dataDir. That backend owns session.json: it rewrites the file when it
// refreshes tokens and rotates cookies, so it would overwrite the new pairing,
// and it keeps using its old session until it restarts. A backend that names a
// different data directory is no conflict. One that answers without naming
// its directory is treated as a conflict, and so is a probe that fails for any
// reason other than a refused connection (a hung backend).
func waitForBackendToReleaseSession(ctx context.Context, dataDir string, probe pairBackendProbe, wait time.Duration, sleep func(time.Duration)) error {
	const poll = 500 * time.Millisecond
	announced := false
	for waited := time.Duration(0); ; waited += poll {
		running, served, err := probe(ctx, dataDir)
		if err == nil && (!running || (served != "" && !samePath(served, dataDir))) {
			return nil
		}
		if waited >= wait {
			if err != nil {
				return fmt.Errorf("cannot rule out a running OpenMessage backend at %s: %w; quit the OpenMessage app and retry", defaultBackendProbeURL(), err)
			}
			return fmt.Errorf("an OpenMessage backend at %s is running on %s; quit the OpenMessage app first (osascript -e 'quit app \"OpenMessage\"'), because it rewrites session.json and would overwrite the new pairing", defaultBackendProbeURL(), dataDir)
		}
		if !announced {
			fmt.Println("Waiting for the running OpenMessage backend to exit...")
			announced = true
		}
		sleep(poll)
	}
}

// withGaiaConfirmTimeout runs a Google-account pairing and gives up when the
// phone has not confirmed within timeout. It cancels (rather than lets a
// deadline expire) because libgm sends the phone a pairing cancel only for
// context.Canceled, which dismisses the stale prompt there.
func withGaiaConfirmTimeout(ctx context.Context, timeout time.Duration, pair func(context.Context) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var timedOut atomic.Bool
	timer := time.AfterFunc(timeout, func() {
		timedOut.Store(true)
		cancel()
	})
	defer timer.Stop()
	err := pair(ctx)
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
		return nil, fmt.Errorf("read Google cookies from Chrome profile %s: %w; sign that profile in to the Google account your phone's Google Messages uses, or pass another profile directory", profile, err)
	}
	fmt.Printf("Read %d Google cookies from Chrome profile %s\n", len(cookies), profile)
	return cookies, nil
}

// savePairedSession writes a new pairing to sessionPath. A session it
// replaces is first copied to session.json.bak-<time>, so a re-pair can be
// rolled back by moving that file back.
func savePairedSession(sessionPath string, data *client.SessionData, now time.Time) error {
	backup, err := backupReplacedSession(sessionPath, now)
	if err != nil {
		return fmt.Errorf("back up the session being replaced: %w", err)
	}
	if backup != "" {
		fmt.Println("Previous session backed up to", backup)
	}
	return client.SaveSession(sessionPath, data)
}

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
		if err := savePairedSession(sessionPath, sessionData, time.Now()); err != nil {
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
		profile := googlecookies.DefaultChromeProfile()
		if len(args) >= 2 && strings.TrimSpace(args[1]) != "" {
			profile = strings.TrimSpace(args[1])
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

func runGoogleAccountPairing(cli *client.Client, sessionPath string, cookies map[string]string) error {
	cli.GM.AuthData.Cookies = cookies

	fmt.Println("Starting Google account pairing...")
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err := withGaiaConfirmTimeout(ctx, gaiaConfirmTimeout, func(ctx context.Context) error {
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
	if err := savePairedSession(sessionPath, sessionData, time.Now()); err != nil {
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
