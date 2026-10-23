package web

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
)

const (
	ControlTokenFile  = "control.token"
	ControlCookieName = "om_control"
	// AuthModeEnforce is the only mode: a protected request without a valid
	// control token is refused with 401 before it reaches any handler. The
	// accept-and-log stage (#144) that only warned has ended.
	AuthModeEnforce  = "enforce"
	maxWarnedRemotes = 1024
	// maxLoginBodyBytes bounds the token-paste form; a token is 64 hex chars.
	maxLoginBodyBytes = 4 << 10
	// LoginErrorQuery is the query parameter /auth/login adds to its redirect
	// back to the UI when the pasted token does not match.
	LoginErrorQuery = "auth_error"
	// BootstrapLinkPath mints a fresh single-use bootstrap link for an
	// already-authenticated native client (the macOS app), which loads it in
	// its web view so the page gets the cookie through a normal Set-Cookie.
	BootstrapLinkPath = "/api/auth/bootstrap-link"
	// mintedBootstrapTTL bounds how long a minted link stays usable; the app
	// loads it immediately.
	mintedBootstrapTTL = 2 * time.Minute
	// maxMintedBootstraps caps outstanding minted links.
	maxMintedBootstraps = 16
)

type ControlAuth struct {
	token             string
	bootstrap         string
	minted            map[string]time.Time
	now               func() time.Time
	logger            zerolog.Logger
	dataDir           string
	mu                sync.Mutex
	warned            map[string]struct{}
	warnedMapOverflow bool
}

func NewControlAuth(dataDir string, logger zerolog.Logger) (*ControlAuth, error) {
	absDataDir, err := filepath.Abs(dataDir)
	if err != nil {
		return nil, fmt.Errorf("resolve data directory: %w", err)
	}
	token, regenerated, err := loadOrMintControlToken(absDataDir)
	if err != nil {
		return nil, err
	}
	if regenerated {
		// Regeneration invalidates cookies containing the old token, which is
		// acceptable: a corrupt token cannot authenticate them anyway.
		logger.Warn().Str("path", filepath.Join(absDataDir, ControlTokenFile)).Msg("Invalid control token was regenerated")
	}
	bootstrap, err := randomHex(32)
	if err != nil {
		return nil, fmt.Errorf("mint bootstrap code: %w", err)
	}
	return &ControlAuth{token: token, bootstrap: bootstrap, minted: make(map[string]time.Time), now: time.Now, logger: logger, dataDir: absDataDir, warned: make(map[string]struct{})}, nil
}

func loadOrMintControlToken(dataDir string) (string, bool, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return "", false, fmt.Errorf("create data directory: %w", err)
	}
	path := filepath.Join(dataDir, ControlTokenFile)
	b, err := os.ReadFile(path)
	if err == nil {
		token := strings.TrimSpace(string(b))
		decoded, decodeErr := hex.DecodeString(token)
		if decodeErr == nil && len(decoded) == 32 {
			if chmodErr := os.Chmod(path, 0o600); chmodErr != nil {
				return "", false, fmt.Errorf("secure control token: %w", chmodErr)
			}
			return token, false, nil
		}
		token, mintErr := mintControlToken(path)
		if mintErr != nil {
			return "", false, mintErr
		}
		return token, true, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", false, fmt.Errorf("read control token: %w", err)
	}
	token, err := mintControlToken(path)
	return token, false, err
}

func mintControlToken(path string) (token string, err error) {
	token, err = randomHex(32)
	if err != nil {
		return "", fmt.Errorf("mint control token: %w", err)
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".control.token-*")
	if err != nil {
		return "", fmt.Errorf("create temporary control token: %w", err)
	}
	tempPath := f.Name()
	defer func() { _ = os.Remove(tempPath) }()
	if err = f.Chmod(0o600); err != nil {
		_ = f.Close()
		return "", fmt.Errorf("secure temporary control token: %w", err)
	}
	if _, err = f.WriteString(token + "\n"); err != nil {
		_ = f.Close()
		return "", fmt.Errorf("write temporary control token: %w", err)
	}
	if err = f.Close(); err != nil {
		return "", fmt.Errorf("close temporary control token: %w", err)
	}
	if err = os.Rename(tempPath, path); err != nil {
		return "", fmt.Errorf("install control token: %w", err)
	}
	return token, nil
}

func randomHex(bytes int) (string, error) {
	b := make([]byte, bytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func (a *ControlAuth) BootstrapURL(baseURL string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.bootstrap == "" {
		return ""
	}
	return strings.TrimRight(baseURL, "/") + "/auth/bootstrap?t=" + a.bootstrap
}

// Handler authenticates local control requests. Every protected path (/api/*
// and /mcp*) needs the control token, as an Authorization: Bearer header
// (native clients, the CLI, MCP clients) or the om_control cookie (the web UI,
// which gets it from the single-use bootstrap link, the token-paste form, or
// the macOS app injecting it). Anything else gets 401 before dispatch.
func (a *ControlAuth) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/bootstrap":
			if _, ok := localRequestOrigin(r); !ok {
				http.NotFound(w, r)
				return
			}
			a.handleBootstrap(w, r)
			return
		case "/auth/login":
			if _, ok := localRequestOrigin(r); !ok {
				http.NotFound(w, r)
				return
			}
			a.handleLogin(w, r)
			return
		}
		if isProtectedLocalPath(r.URL.Path) {
			classification := a.classify(r)
			fetchSite := strings.TrimSpace(r.Header.Get("Sec-Fetch-Site"))
			if classification == "missing" || classification == "invalid" {
				a.warnOnce(r, classification, fetchSite, "Rejected local control request without a valid control token")
				w.Header().Set("WWW-Authenticate", `Bearer realm="openmessage"`)
				httpError(w, "unauthorized: send the control token as Authorization: Bearer <token> (the token is the control.token file in the OpenMessage data directory), or open the web UI through its authorization link", http.StatusUnauthorized)
				return
			}
			if fetchSite != "" && fetchSite != "same-origin" && fetchSite != "same-site" && fetchSite != "none" {
				a.warnOnce(r, classification, fetchSite, "Authenticated local control request has an unexpected Sec-Fetch-Site")
			}
			if r.URL.Path == BootstrapLinkPath {
				a.handleMintBootstrapLink(w, r)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (a *ControlAuth) classify(r *http.Request) string {
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if auth != "" {
		parts := strings.Fields(auth)
		if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") && secureEqual(parts[1], a.token) {
			return "bearer-ok"
		}
		return "invalid"
	}
	cookie, err := r.Cookie(ControlCookieName)
	if err == nil {
		if secureEqual(cookie.Value, a.token) {
			return "cookie-ok"
		}
		return "invalid"
	}
	return "missing"
}

func secureEqual(got, want string) bool {
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func (a *ControlAuth) warnOnce(r *http.Request, classification, fetchSite, message string) {
	key := r.RemoteAddr
	if host, _, err := net.SplitHostPort(key); err == nil {
		key = host
	}
	if key == "" {
		key = "unknown"
	}
	a.mu.Lock()
	if _, ok := a.warned[key]; ok {
		a.mu.Unlock()
		return
	}
	overflow := false
	if len(a.warned) >= maxWarnedRemotes {
		clear(a.warned)
		if !a.warnedMapOverflow {
			a.warnedMapOverflow = true
			overflow = true
		}
	}
	a.warned[key] = struct{}{}
	a.mu.Unlock()
	if overflow {
		a.logger.Warn().Int("limit", maxWarnedRemotes).Msg("Cleared full unauthenticated remote warning cache")
	}
	a.logger.Warn().Str("remote", key).Str("auth", classification).Str("sec_fetch_site", fetchSite).Str("path", r.URL.Path).Msg(message)
}

func (a *ControlAuth) handleBootstrap(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		httpError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !a.consumeBootstrapCode(r.URL.Query().Get("t")) {
		httpError(w, "bootstrap code is invalid or already used", http.StatusGone)
		return
	}
	a.setControlCookie(w)
	http.Redirect(w, r, "/", http.StatusFound)
}

// consumeBootstrapCode reports whether code is the startup code or an
// unexpired minted one, and spends it. Every outstanding code is compared in
// constant time.
func (a *ControlAuth) consumeBootstrapCode(code string) bool {
	if code == "" {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.bootstrap != "" && secureEqual(code, a.bootstrap) {
		a.bootstrap = ""
		return true
	}
	now := a.now()
	matched := ""
	for minted, expires := range a.minted {
		if !now.Before(expires) {
			delete(a.minted, minted)
			continue
		}
		if secureEqual(code, minted) {
			matched = minted
		}
	}
	if matched == "" {
		return false
	}
	delete(a.minted, matched)
	return true
}

// MintBootstrapCode returns a fresh single-use bootstrap code that expires
// after mintedBootstrapTTL. At most maxMintedBootstraps are outstanding; a
// new one evicts the one closest to expiring.
func (a *ControlAuth) MintBootstrapCode() (string, error) {
	code, err := randomHex(32)
	if err != nil {
		return "", fmt.Errorf("mint bootstrap code: %w", err)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	for minted, expires := range a.minted {
		if !now.Before(expires) {
			delete(a.minted, minted)
		}
	}
	for len(a.minted) >= maxMintedBootstraps {
		oldest, oldestExpiry := "", time.Time{}
		for minted, expires := range a.minted {
			if oldest == "" || expires.Before(oldestExpiry) {
				oldest, oldestExpiry = minted, expires
			}
		}
		delete(a.minted, oldest)
	}
	a.minted[code] = now.Add(mintedBootstrapTTL)
	return code, nil
}

func (a *ControlAuth) handleMintBootstrapLink(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		httpError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	code, err := a.MintBootstrapCode()
	if err != nil {
		httpError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, map[string]any{"path": "/auth/bootstrap?t=" + code, "expires_in_seconds": int(mintedBootstrapTTL / time.Second)})
}

// handleLogin is the fallback for a browser that missed the single-use
// bootstrap link (it is printed once per daemon start): the user pastes the
// control token, which is already the capability, and gets the same cookie.
// A cross-origin page cannot use it to log the browser in, and a wrong token
// changes nothing.
func (a *ControlAuth) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		httpError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if origin := strings.TrimSpace(r.Header.Get("Origin")); origin != "" && origin != "null" {
		reqOrigin, _ := localRequestOrigin(r)
		if headerOrigin, ok := parseLocalOrigin(origin); !ok || headerOrigin != reqOrigin {
			httpError(w, "forbidden: cross-origin login", http.StatusForbidden)
			return
		}
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxLoginBodyBytes)
	if err := r.ParseForm(); err != nil {
		httpError(w, "invalid login form", http.StatusBadRequest)
		return
	}
	if !secureEqual(strings.TrimSpace(r.PostForm.Get("token")), a.token) {
		a.warnOnce(r, "invalid", strings.TrimSpace(r.Header.Get("Sec-Fetch-Site")), "Rejected web UI login with a wrong control token")
		http.Redirect(w, r, "/?"+LoginErrorQuery+"=invalid_token", http.StatusSeeOther)
		return
	}
	a.setControlCookie(w)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (a *ControlAuth) setControlCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: ControlCookieName, Value: a.token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
}

func (a *ControlAuth) Status() map[string]any {
	if a == nil {
		return map[string]any{"token_present": false, "mode": AuthModeEnforce}
	}
	return map[string]any{"token_present": a.token != "", "mode": AuthModeEnforce, "data_dir": a.dataDir}
}
