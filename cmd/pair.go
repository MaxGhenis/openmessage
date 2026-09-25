package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
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
)

const maxQRRefreshes = 5

type pairMode string

const (
	pairModeQR     pairMode = "qr"
	pairModeGoogle pairMode = "google"
)

func RunPair(logger zerolog.Logger, args ...string) error {
	dataDir := app.DefaultDataDir()
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}

	sessionPath := dataDir + "/session.json"
	mode, googleInput, err := resolvePairMode(args)
	if err != nil {
		return err
	}

	cli := client.NewForPairing(logger)
	defer cli.GM.Disconnect()

	if mode == pairModeGoogle {
		return runGoogleAccountPairing(cli, sessionPath, googleInput)
	}

	return runQRPairing(logger, cli, sessionPath)
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
	qrURL, err := cli.GM.StartLogin(context.Background())
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
	default:
		return "", "", fmt.Errorf("unknown pair option: %s", args[0])
	}
}

func runGoogleAccountPairing(cli *client.Client, sessionPath, rawInput string) error {
	cookies, err := parseGoogleCookiesInput(rawInput)
	if err != nil {
		return fmt.Errorf("parse Google cookies: %w", err)
	}
	cli.GM.AuthData.Cookies = cookies

	fmt.Println("Starting Google account pairing...")
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return completeGoogleAccountPairing(ctx, cli.GM, cli.SessionData, sessionPath, os.Stdout)
}

type googleAccountPairer interface {
	StartGaiaPairing(context.Context, context.Context) (string, *libgm.PairingSession, error)
	FinishGaiaPairing(context.Context, *libgm.PairingSession) (string, error)
}

func completeGoogleAccountPairing(ctx context.Context, pairer googleAccountPairer, sessionData func() (*client.SessionData, error), sessionPath string, output io.Writer) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("start google account pairing: %w", err)
	}
	// Use the split API so this one-shot command can save the session before
	// disconnecting. DoGaiaPairing also starts an asynchronous reconnect.
	// Upstream startup can wait for the first long poll without honoring ctx.
	// Do not let that wait trap the CLI's signal handler. A buffered result lets
	// a late startup return without blocking or continuing into Finish/save.
	// This is one-shot CLI code: an upstream waiter that never returns is only
	// released by process exit, so this wrapper must not be used by the daemon.
	type startResult struct {
		emoji   string
		pairing *libgm.PairingSession
		err     error
	}
	started := make(chan startResult, 1)
	go func() {
		emoji, pairing, err := pairer.StartGaiaPairing(ctx, ctx)
		started <- startResult{emoji, pairing, err}
	}()
	var result startResult
	select {
	case <-ctx.Done():
		return fmt.Errorf("start google account pairing: %w", ctx.Err())
	case result = <-started:
	}
	// Cancellation wins even if both the context and result became ready.
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("start google account pairing: %w", err)
	}
	if result.err != nil {
		return fmt.Errorf("start google account pairing: %w", result.err)
	}
	fmt.Fprintln(output, "EMOJI:", result.emoji)
	fmt.Fprintln(output, "Tap this emoji in Google Messages on your phone.")
	_, err := pairer.FinishGaiaPairing(ctx, result.pairing)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("google account pairing: %w", ctxErr)
	}
	if err != nil {
		return fmt.Errorf("google account pairing: %w", err)
	}

	data, err := sessionData()
	if err != nil {
		return fmt.Errorf("get session data: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("google account pairing: %w", err)
	}
	if err := client.SaveSession(sessionPath, data); err != nil {
		return fmt.Errorf("save session: %w", err)
	}
	fmt.Fprintln(output, "\nPairing successful!")
	fmt.Fprintln(output, "Session saved to", sessionPath)
	fmt.Fprintln(output, "You can now run: openmessage serve")
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
