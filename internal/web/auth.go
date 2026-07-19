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

	"github.com/rs/zerolog"
)

const (
	ControlTokenFile     = "control.token"
	ControlCookieName    = "om_control"
	AuthModeAcceptAndLog = "accept-and-log"
	maxWarnedRemotes     = 1024
)

type ControlAuth struct {
	token             string
	bootstrap         string
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
	return &ControlAuth{token: token, bootstrap: bootstrap, logger: logger, dataDir: absDataDir, warned: make(map[string]struct{})}, nil
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

func (a *ControlAuth) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/auth/bootstrap" {
			if _, ok := localRequestOrigin(r); !ok {
				http.NotFound(w, r)
				return
			}
			a.handleBootstrap(w, r)
			return
		}
		if isProtectedLocalPath(r.URL.Path) {
			classification := a.classify(r)
			fetchSite := strings.TrimSpace(r.Header.Get("Sec-Fetch-Site"))
			if classification == "missing" || classification == "invalid" || (fetchSite != "" && fetchSite != "same-origin" && fetchSite != "same-site" && fetchSite != "none") {
				a.warnOnce(r, classification, fetchSite)
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

func (a *ControlAuth) warnOnce(r *http.Request, classification, fetchSite string) {
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
	a.logger.Warn().Str("remote", key).Str("auth", classification).Str("sec_fetch_site", fetchSite).Msg("Local control request is not authenticated; allowed during accept-and-log rollout")
}

func (a *ControlAuth) handleBootstrap(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		httpError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	code := r.URL.Query().Get("t")
	a.mu.Lock()
	valid := a.bootstrap != "" && secureEqual(code, a.bootstrap)
	if valid {
		a.bootstrap = ""
	}
	a.mu.Unlock()
	if !valid {
		httpError(w, "bootstrap code is invalid or already used", http.StatusGone)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: ControlCookieName, Value: a.token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
	http.Redirect(w, r, "/", http.StatusFound)
}

func (a *ControlAuth) Status() map[string]any {
	if a == nil {
		return map[string]any{"token_present": false, "mode": AuthModeAcceptAndLog}
	}
	return map[string]any{"token_present": a.token != "", "mode": AuthModeAcceptAndLog, "data_dir": a.dataDir}
}
