package tools

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/rs/zerolog"
	"go.mau.fi/mautrix-gmessages/pkg/libgm"

	"github.com/maxghenis/openmessage/internal/app"
	"github.com/maxghenis/openmessage/internal/client"
)

const (
	// pairingSessionTTL bounds how long a started pairing stays resumable.
	// Google expires the handshake on its own side too; this keeps abandoned
	// sessions (and their live clients) from accumulating.
	pairingSessionTTL = 10 * time.Minute
	// finishPairingTimeout bounds the blocking wait for the user's tap.
	finishPairingTimeout = 2 * time.Minute
)

// pendingPairing is a started-but-unconfirmed Gaia handshake.
//
// libgm's PairingSession holds an ECDSA private key and is bound to the client
// that created it, so unlike an opaque OTP challenge token it cannot be handed
// to the caller and passed back. We keep it server-side and give the caller an
// opaque handle instead.
type pendingPairing struct {
	cli     *client.Client
	session *libgm.PairingSession
	emoji   string
	started time.Time
}

type pairingRegistry struct {
	mu       sync.Mutex
	pending  map[string]*pendingPairing
	nowFn    func() time.Time
	newUUIDs func() string
}

func newPairingRegistry() *pairingRegistry {
	return &pairingRegistry{
		pending:  map[string]*pendingPairing{},
		nowFn:    time.Now,
		newUUIDs: func() string { return uuid.NewString() },
	}
}

var pairings = newPairingRegistry()

func (r *pairingRegistry) add(p *pendingPairing) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.evictExpiredLocked()
	handle := r.newUUIDs()
	r.pending[handle] = p
	return handle
}

// take removes and returns a pending pairing. Pairings are single-use: a
// failed finish leaves the libgm session spent, so the caller must start over.
func (r *pairingRegistry) take(handle string) (*pendingPairing, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.evictExpiredLocked()
	p, ok := r.pending[handle]
	if ok {
		delete(r.pending, handle)
	}
	return p, ok
}

func (r *pairingRegistry) evictExpiredLocked() {
	for handle, p := range r.pending {
		if r.nowFn().Sub(p.started) > pairingSessionTTL {
			p.cli.GM.Disconnect()
			delete(r.pending, handle)
		}
	}
}

// Indirected for tests.
var (
	startGaiaPairing = func(ctx context.Context, cli *client.Client, rawCookies string) (string, *libgm.PairingSession, error) {
		return client.StartGaiaPairing(ctx, cli, rawCookies)
	}
	finishGaiaPairing = func(ctx context.Context, cli *client.Client, session *libgm.PairingSession, sessionPath string) error {
		return client.FinishGaiaPairing(ctx, cli, session, sessionPath)
	}
	newPairingClient = func(logger zerolog.Logger) *client.Client {
		return client.NewForPairing(logger)
	}
)

func startGooglePairingTool() mcp.Tool {
	return mcp.NewTool("start_google_pairing",
		mcp.WithDescription(
			"First step of Google Messages pairing. Authenticates with Google Account cookies "+
				"copied from a browser and returns a confirmation emoji plus a `pairing_handle`. "+
				"This is the supported pairing method for headless installs, where the QR flow "+
				"(which needs an interactive terminal) is unavailable. "+
				"The caller must supply cookies from a browser already signed in to "+
				"messages.google.com — they cannot be obtained from this server. "+
				"Show the emoji to the user immediately: they tap it in Google Messages on their "+
				"phone, then you call `complete_google_pairing` with the handle. "+
				"The handle is valid for 10 minutes.",
		),
		mcp.WithString("cookies",
			mcp.Required(),
			mcp.Description(
				"Google cookies from browser devtools, in any of: a JSON object of "+
					"cookie name/value pairs, a full 'curl ...' command (Copy as cURL), "+
					"or a raw Cookie: header value.",
			),
		),
		mcp.WithReadOnlyHintAnnotation(false),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(false),
		mcp.WithOpenWorldHintAnnotation(true),
	)
}

func startGooglePairingHandler(a *app.App) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		cookies, err := req.RequireString("cookies")
		if err != nil {
			return errorResult(err.Error()), nil
		}
		if strings.TrimSpace(cookies) == "" {
			return errorResult("cookies must not be empty"), nil
		}

		cli := newPairingClient(a.Logger)
		emoji, session, err := startGaiaPairing(ctx, cli, cookies)
		if err != nil {
			cli.GM.Disconnect()
			return errorResult(fmt.Sprintf("start Google pairing: %v", err)), nil
		}

		handle := pairings.add(&pendingPairing{
			cli:     cli,
			session: session,
			emoji:   emoji,
			started: time.Now(),
		})

		text := fmt.Sprintf(
			"Google Messages is showing the emoji %s on your phone.\n\n"+
				"Tap %s in Google Messages (a prompt should appear; otherwise Settings > "+
				"Device pairing), then call complete_google_pairing with "+
				"pairing_handle=%q.",
			emoji, emoji, handle,
		)
		return structuredResult(map[string]any{
			"ok":             true,
			"emoji":          emoji,
			"pairing_handle": handle,
			"expires_in_s":   int(pairingSessionTTL.Seconds()),
		}, text), nil
	}
}

func completeGooglePairingTool() mcp.Tool {
	return mcp.NewTool("complete_google_pairing",
		mcp.WithDescription(
			"Second step of Google Messages pairing. Call after the user has tapped the emoji "+
				"returned by `start_google_pairing`. Waits up to 2 minutes for the tap to be "+
				"confirmed, then saves the session. "+
				"A pairing handle is single-use: if this fails, call `start_google_pairing` again "+
				"for a fresh emoji.",
		),
		mcp.WithString("pairing_handle",
			mcp.Required(),
			mcp.Description("The `pairing_handle` returned by start_google_pairing."),
		),
		mcp.WithReadOnlyHintAnnotation(false),
		mcp.WithDestructiveHintAnnotation(true),
		mcp.WithIdempotentHintAnnotation(false),
		mcp.WithOpenWorldHintAnnotation(true),
	)
}

func completeGooglePairingHandler(a *app.App) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		handle, err := req.RequireString("pairing_handle")
		if err != nil {
			return errorResult(err.Error()), nil
		}

		pending, ok := pairings.take(strings.TrimSpace(handle))
		if !ok {
			return errorResult(
				"unknown or expired pairing_handle — call start_google_pairing for a fresh emoji",
			), nil
		}
		defer pending.cli.GM.Disconnect()

		ctx, cancel := context.WithTimeout(ctx, finishPairingTimeout)
		defer cancel()

		if err := finishGaiaPairing(ctx, pending.cli, pending.session, a.SessionPath); err != nil {
			if ctx.Err() != nil {
				return errorResult(fmt.Sprintf(
					"Timed out after %s waiting for the tap to be confirmed. The emoji was %s. "+
						"Call start_google_pairing again for a fresh emoji.",
					finishPairingTimeout, pending.emoji,
				)), nil
			}
			return errorResult(fmt.Sprintf("complete Google pairing: %v", err)), nil
		}

		return structuredResult(map[string]any{
			"ok":           true,
			"session_path": a.SessionPath,
			"data_dir":     a.DataDir,
		}, fmt.Sprintf("Google Messages paired. Session saved to %s.", a.SessionPath)), nil
	}
}
