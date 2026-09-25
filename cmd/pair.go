package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/mdp/qrterminal/v3"
	"github.com/rs/zerolog"
	"go.mau.fi/mautrix-gmessages/pkg/libgm"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/events"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
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

// parseGoogleCookiesInput is retained as a thin alias so the cookie-parsing
// tests keep exercising the CLI's entry point; the implementation now lives in
// internal/client so the MCP pairing tool shares it.
func parseGoogleCookiesInput(raw string) (map[string]string, error) {
	return client.ParseGoogleCookiesInput(raw)
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
	fmt.Println("Starting Google account pairing...")
	err := client.PairWithGoogleCookies(context.Background(), cli, sessionPath, rawInput, func(emoji string) {
		fmt.Println("EMOJI:", emoji)
		fmt.Println("Tap this emoji in Google Messages on your phone.")
	})
	if err != nil {
		return err
	}
	fmt.Println("\nPairing successful!")
	fmt.Println("Session saved to", sessionPath)
	fmt.Println("You can now run: openmessage serve")
	return nil
}

// Ensure PairCallback type matches what libgm expects
var _ = (*libgm.Client)(nil)
