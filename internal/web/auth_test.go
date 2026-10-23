package web

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/quick"
	"time"

	"github.com/maxghenis/openmessage/internal/db"
	"github.com/rs/zerolog"
)

func TestControlTokenMintLoadAndPermissions(t *testing.T) {
	dir := t.TempDir()
	first, err := NewControlAuth(dir, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	if len(first.token) != 64 {
		t.Fatalf("token length = %d, want 64 hex chars", len(first.token))
	}
	info, err := os.Stat(filepath.Join(dir, ControlTokenFile))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("mode = %o, want 600", got)
	}
	second, err := NewControlAuth(dir, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	if second.token != first.token {
		t.Fatal("token changed across restart")
	}
}

func TestControlTokenRegeneratesInvalidExistingFile(t *testing.T) {
	for _, contents := range []string{"", "abcd", "not-hex"} {
		t.Run(fmt.Sprintf("length-%d", len(contents)), func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, ControlTokenFile)
			if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
				t.Fatal(err)
			}
			var logs bytes.Buffer
			a, err := NewControlAuth(dir, zerolog.New(&logs))
			if err != nil {
				t.Fatal(err)
			}
			if decoded, err := hex.DecodeString(a.token); err != nil || len(decoded) != 32 {
				t.Fatalf("regenerated token = %q, decode error = %v", a.token, err)
			}
			if got := strings.Count(logs.String(), "Invalid control token was regenerated"); got != 1 {
				t.Fatalf("regeneration warnings = %d, want 1; %s", got, logs.String())
			}
		})
	}
}

func TestControlAuthRejectsMissingAndInvalidCredentialsBeforeDispatch(t *testing.T) {
	var logs bytes.Buffer
	a, err := NewControlAuth(t.TempDir(), zerolog.New(&logs))
	if err != nil {
		t.Fatal(err)
	}
	dispatched := 0
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		dispatched++
		w.WriteHeader(http.StatusNoContent)
	})
	h := ProtectLocalControl(a.Handler(next))
	tests := []struct {
		name     string
		apply    func(*http.Request)
		wantCode int
		wantWarn bool
	}{
		{"bearer", func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+a.token) }, http.StatusNoContent, false},
		{"bearer lowercase scheme", func(r *http.Request) { r.Header.Set("Authorization", "bearer "+a.token) }, http.StatusNoContent, false},
		{"cookie", func(r *http.Request) { r.AddCookie(&http.Cookie{Name: ControlCookieName, Value: a.token}) }, http.StatusNoContent, false},
		{"missing", func(*http.Request) {}, http.StatusUnauthorized, true},
		{"invalid bearer", func(r *http.Request) { r.Header.Set("Authorization", "Bearer wrong") }, http.StatusUnauthorized, true},
		{"invalid cookie", func(r *http.Request) { r.AddCookie(&http.Cookie{Name: ControlCookieName, Value: "wrong"}) }, http.StatusUnauthorized, true},
		{"token prefix", func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+a.token[:63]) }, http.StatusUnauthorized, true},
		{"basic scheme", func(r *http.Request) { r.Header.Set("Authorization", "Basic "+a.token) }, http.StatusUnauthorized, true},
		{"invalid bearer beats valid cookie", func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer wrong")
			r.AddCookie(&http.Cookie{Name: ControlCookieName, Value: a.token})
		}, http.StatusUnauthorized, true},
	}
	for i, tt := range tests {
		for _, path := range []string{"/api/status", "/api/conversations", "/api/send", "/api/v1/outbox/messages", "/mcp", "/mcp/sse", "/mcp/message"} {
			t.Run(tt.name+" "+path, func(t *testing.T) {
				logs.Reset()
				dispatched = 0
				r := httptest.NewRequest(http.MethodGet, "http://127.0.0.1"+path, nil)
				// A fresh remote per case so the once-per-remote warning fires.
				r.RemoteAddr = fmt.Sprintf("127.0.%d.%d:7007", i, len(path))
				tt.apply(r)
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				if w.Code != tt.wantCode {
					t.Fatalf("status = %d, want %d; body %s", w.Code, tt.wantCode, w.Body.String())
				}
				if wantDispatch := tt.wantCode != http.StatusUnauthorized; (dispatched == 1) != wantDispatch {
					t.Fatalf("dispatched = %d, want dispatch %t", dispatched, wantDispatch)
				}
				if tt.wantCode == http.StatusUnauthorized {
					if got := w.Header().Get("WWW-Authenticate"); got != `Bearer realm="openmessage"` {
						t.Fatalf("WWW-Authenticate = %q", got)
					}
					if strings.Contains(w.Body.String(), a.token) {
						t.Fatal("401 body leaks the control token")
					}
				}
				if got := strings.Contains(logs.String(), `"level":"warn"`); got != tt.wantWarn {
					t.Fatalf("warn = %t, want %t; %s", got, tt.wantWarn, logs.String())
				}
				if strings.Contains(logs.String(), a.token) {
					t.Fatal("log leaks the control token")
				}
			})
		}
	}
}

func TestControlAuthLeavesUnprotectedPathsOpen(t *testing.T) {
	a, err := NewControlAuth(t.TempDir(), zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	h := ProtectLocalControl(a.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })))
	// The UI shell and its static assets hold no messages; they must load so
	// the page can show how to authorize the browser.
	for _, path := range []string{"/", "/index.html", "/sw.js", "/manifest.webmanifest", "/favicon.svg", "/apix", "/mcpx"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "http://127.0.0.1"+path, nil))
		if w.Code != http.StatusNoContent {
			t.Fatalf("%s status = %d, want 204", path, w.Code)
		}
	}
}

// TestControlAuthDispatchIffExactToken is the invariant behind the 401: for
// any protected path and any Authorization/cookie pair, the request reaches
// the next handler if and only if it carries exactly the control token, as a
// Bearer credential or (with no Authorization header at all) as the cookie.
func TestControlAuthDispatchIffExactToken(t *testing.T) {
	a, err := NewControlAuth(t.TempDir(), zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	dispatched := false
	h := ProtectLocalControl(a.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		dispatched = true
		w.WriteHeader(http.StatusNoContent)
	})))
	protected := []string{"/api/status", "/api/conversations/x/messages", "/api/send", "/api/media/x", "/api/v1/outbox/messages", "/api/events", "/mcp", "/mcp/sse"}
	schemes := []string{"Bearer", "bearer", "BEARER", "Basic", "Token", ""}
	pick := func(seed uint8, real, other string) string {
		switch seed % 5 {
		case 0:
			return real
		case 1:
			return other
		case 2:
			return ""
		case 3:
			return real + "0"
		default:
			if len(real) > 1 {
				return real[1:]
			}
			return ""
		}
	}
	property := func(pathSeed, schemeSeed, authSeed, cookieSeed uint8, withAuth, withCookie bool, other string) bool {
		dispatched = false
		path := protected[int(pathSeed)%len(protected)]
		r := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7007"+path, nil)
		authValue := pick(authSeed, a.token, other)
		cookieValue := pick(cookieSeed, a.token, other)
		scheme := schemes[int(schemeSeed)%len(schemes)]
		if withAuth {
			r.Header.Set("Authorization", strings.TrimSpace(scheme+" "+authValue))
		}
		if withCookie {
			r.AddCookie(&http.Cookie{Name: ControlCookieName, Value: cookieValue})
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		authHeader := strings.TrimSpace(r.Header.Get("Authorization"))
		var want bool
		if authHeader != "" {
			fields := strings.Fields(authHeader)
			want = len(fields) == 2 && strings.EqualFold(fields[0], "bearer") && fields[1] == a.token
		} else {
			want = withCookie && cookieValue == a.token
		}
		if dispatched != want {
			t.Logf("path=%s auth=%q cookie=%t/%q dispatched=%t want=%t code=%d", path, authHeader, withCookie, cookieValue, dispatched, want, w.Code)
			return false
		}
		return want || w.Code == http.StatusUnauthorized
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 2000}); err != nil {
		t.Fatal(err)
	}
}

func TestControlLoginSetsCookieOnlyForTheExactToken(t *testing.T) {
	a, err := NewControlAuth(t.TempDir(), zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	h := ProtectLocalControl(a.Handler(http.NotFoundHandler()))
	login := func(token, origin, host string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7007/auth/login", strings.NewReader(url.Values{"token": {token}}.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		if host != "" {
			r.Host = host
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}

	ok := login(" "+a.token+"\n", "http://127.0.0.1:7007", "")
	if ok.Code != http.StatusSeeOther || ok.Header().Get("Location") != "/" {
		t.Fatalf("valid login = %d location %q", ok.Code, ok.Header().Get("Location"))
	}
	cookies := ok.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != ControlCookieName || cookies[0].Value != a.token || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode || cookies[0].Path != "/" {
		t.Fatalf("cookie = %+v", cookies)
	}
	if nullOrigin := login(a.token, "null", ""); nullOrigin.Code != http.StatusSeeOther || len(nullOrigin.Result().Cookies()) != 1 {
		t.Fatalf("Origin: null login = %d cookies %+v; a no-referrer form post sends a null origin", nullOrigin.Code, nullOrigin.Result().Cookies())
	}

	wrong := login("0"+a.token[1:], "http://127.0.0.1:7007", "")
	if wrong.Code != http.StatusSeeOther || wrong.Header().Get("Location") != "/?"+LoginErrorQuery+"=invalid_token" {
		t.Fatalf("wrong token = %d location %q", wrong.Code, wrong.Header().Get("Location"))
	}
	if len(wrong.Result().Cookies()) != 0 {
		t.Fatalf("wrong token set cookies %+v", wrong.Result().Cookies())
	}

	if cross := login(a.token, "http://evil.example", ""); cross.Code != http.StatusForbidden || len(cross.Result().Cookies()) != 0 {
		t.Fatalf("cross-origin login = %d cookies %+v", cross.Code, cross.Result().Cookies())
	}
	if otherPort := login(a.token, "http://127.0.0.1:9999", ""); otherPort.Code != http.StatusForbidden {
		t.Fatalf("other-port origin login = %d, want 403", otherPort.Code)
	}
	if rebound := login(a.token, "", "evil.example:7007"); rebound.Code != http.StatusNotFound || len(rebound.Result().Cookies()) != 0 {
		t.Fatalf("non-loopback Host login = %d, want 404", rebound.Code)
	}

	get := httptest.NewRecorder()
	h.ServeHTTP(get, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7007/auth/login?token="+a.token, nil))
	if get.Code != http.StatusMethodNotAllowed || len(get.Result().Cookies()) != 0 {
		t.Fatalf("GET login = %d; the token must never travel in a URL", get.Code)
	}

	huge := login(strings.Repeat("a", maxLoginBodyBytes+1), "", "")
	if huge.Code != http.StatusBadRequest {
		t.Fatalf("oversized login = %d, want 400", huge.Code)
	}
}

func TestControlCookieFromLoginAuthenticatesAPI(t *testing.T) {
	a, err := NewControlAuth(t.TempDir(), zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	h := ProtectLocalControl(a.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })))
	r := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7007/auth/login", strings.NewReader("token="+a.token))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	api := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7007/api/conversations", nil)
	for _, c := range w.Result().Cookies() {
		api.AddCookie(c)
	}
	got := httptest.NewRecorder()
	h.ServeHTTP(got, api)
	if got.Code != http.StatusNoContent {
		t.Fatalf("API with login cookie = %d", got.Code)
	}
}

func TestControlAuthWarnsOncePerRemoteHost(t *testing.T) {
	var logs bytes.Buffer
	a, err := NewControlAuth(t.TempDir(), zerolog.New(&logs))
	if err != nil {
		t.Fatal(err)
	}
	h := a.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	for _, remote := range []string{"192.0.2.10:7001", "192.0.2.10:7002"} {
		r := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/status", nil)
		r.RemoteAddr = remote
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", w.Code)
		}
	}
	if got := strings.Count(logs.String(), `"level":"warn"`); got != 1 {
		t.Fatalf("warnings = %d, want 1; %s", got, logs.String())
	}
}

func TestControlAuthBoundsWarnedRemoteMap(t *testing.T) {
	a, err := NewControlAuth(t.TempDir(), zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i <= maxWarnedRemotes; i++ {
		r := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/status", nil)
		r.RemoteAddr = fmt.Sprintf("remote-%d", i)
		a.warnOnce(r, "missing", "", "test")
	}
	if got := len(a.warned); got != 1 {
		t.Fatalf("warned map length after overflow = %d, want 1", got)
	}
	if !a.warnedMapOverflow {
		t.Fatal("warned map overflow was not recorded")
	}
}

func TestControlBootstrapSetsStrictCookieAndIsSingleUse(t *testing.T) {
	a, err := NewControlAuth(t.TempDir(), zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	url := a.BootstrapURL("http://127.0.0.1:7007")
	h := a.Handler(http.NotFoundHandler())
	first := httptest.NewRecorder()
	h.ServeHTTP(first, httptest.NewRequest(http.MethodGet, url, nil))
	if first.Code != http.StatusFound || first.Header().Get("Location") != "/" {
		t.Fatalf("first = %d location %q", first.Code, first.Header().Get("Location"))
	}
	cookies := first.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != ControlCookieName || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode || cookies[0].Secure || cookies[0].Path != "/" {
		t.Fatalf("cookie = %+v", cookies)
	}
	second := httptest.NewRecorder()
	h.ServeHTTP(second, httptest.NewRequest(http.MethodGet, url, nil))
	if second.Code != http.StatusGone {
		t.Fatalf("second status = %d, want 410", second.Code)
	}
}

func TestControlBootstrapRejectsNonLoopbackHostWithoutConsumingCode(t *testing.T) {
	a, err := NewControlAuth(t.TempDir(), zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	url := a.BootstrapURL("http://127.0.0.1:7007")
	h := a.Handler(http.NotFoundHandler())
	nonLoopback := httptest.NewRequest(http.MethodGet, url, nil)
	nonLoopback.Host = "example.com"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, nonLoopback)
	if w.Code != http.StatusNotFound {
		t.Fatalf("non-loopback status = %d, want 404", w.Code)
	}
	valid := httptest.NewRecorder()
	h.ServeHTTP(valid, httptest.NewRequest(http.MethodGet, url, nil))
	if valid.Code != http.StatusFound {
		t.Fatalf("loopback status after rejection = %d, want 302", valid.Code)
	}
}

func TestStatusReportsEnforcedAuth(t *testing.T) {
	store, err := db.New(filepath.Join(t.TempDir(), "messages.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	a, err := NewControlAuth(t.TempDir(), zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	h := APIHandlerWithOptions(store, nil, zerolog.Nop(), nil, APIOptions{Auth: a})
	unauthenticated := httptest.NewRecorder()
	h.ServeHTTP(unauthenticated, httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/status", nil))
	if unauthenticated.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want 401", unauthenticated.Code)
	}
	if strings.Contains(unauthenticated.Body.String(), a.dataDir) {
		t.Fatal("401 body names the data directory")
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/status", nil)
	r.Header.Set("Authorization", "Bearer "+a.token)
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	auth, ok := body["auth"].(map[string]any)
	if !ok || auth["mode"] != AuthModeEnforce || auth["token_present"] != true || auth["data_dir"] != a.dataDir {
		t.Fatalf("auth = %#v", body["auth"])
	}
	if got := w.Header().Get("Cross-Origin-Resource-Policy"); got != "same-origin" {
		t.Fatalf("CORP = %q", got)
	}
}

func TestEventStreamRejectsNewestSubscriberAtLimit(t *testing.T) {
	store, err := db.New(filepath.Join(t.TempDir(), "messages.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	events := NewEventBroker()
	for i := 0; i < maxEventSubscribers; i++ {
		if _, _, ok := events.TrySubscribe(); !ok {
			t.Fatalf("subscriber %d rejected", i)
		}
	}
	h := APIHandlerWithOptions(store, nil, zerolog.Nop(), nil, APIOptions{Events: events})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/events", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
}

func mintBootstrapLink(t *testing.T, h http.Handler, token string) (int, string) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7007"+BootstrapLinkPath, nil)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var body struct {
		Path string `json:"path"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	return w.Code, body.Path
}

func useBootstrapPath(h http.Handler, path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7007"+path, nil))
	return w
}

func TestMintedBootstrapLinkNeedsTheTokenAndWorksOnce(t *testing.T) {
	a, err := NewControlAuth(t.TempDir(), zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	dispatched := false
	h := ProtectLocalControl(a.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { dispatched = true })))

	if code, path := mintBootstrapLink(t, h, ""); code != http.StatusUnauthorized || path != "" {
		t.Fatalf("unauthenticated mint = %d %q, want 401", code, path)
	}
	get := httptest.NewRecorder()
	getReq := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7007"+BootstrapLinkPath, nil)
	getReq.Header.Set("Authorization", "Bearer "+a.token)
	h.ServeHTTP(get, getReq)
	if get.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET mint = %d, want 405", get.Code)
	}

	code, path := mintBootstrapLink(t, h, a.token)
	if code != http.StatusOK || !strings.HasPrefix(path, "/auth/bootstrap?t=") {
		t.Fatalf("mint = %d %q", code, path)
	}
	if strings.Contains(path, a.token) {
		t.Fatal("minted link carries the control token")
	}
	first := useBootstrapPath(h, path)
	if first.Code != http.StatusFound || len(first.Result().Cookies()) != 1 || first.Result().Cookies()[0].Value != a.token {
		t.Fatalf("minted link = %d cookies %+v", first.Code, first.Result().Cookies())
	}
	if again := useBootstrapPath(h, path); again.Code != http.StatusGone {
		t.Fatalf("reused minted link = %d, want 410", again.Code)
	}
	// Minting leaves the startup link alone.
	if startup := useBootstrapPath(h, strings.TrimPrefix(a.BootstrapURL("http://127.0.0.1:7007"), "http://127.0.0.1:7007")); startup.Code != http.StatusFound {
		t.Fatalf("startup link after a mint = %d, want 302", startup.Code)
	}
	if dispatched {
		t.Fatal("mint or bootstrap reached the API handler")
	}
}

func TestMintedBootstrapLinkExpiresAndIsBounded(t *testing.T) {
	a, err := NewControlAuth(t.TempDir(), zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Unix(1_800_000_000, 0)
	a.now = func() time.Time { return clock }
	h := a.Handler(http.NotFoundHandler())

	expiring, err := a.MintBootstrapCode()
	if err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(mintedBootstrapTTL)
	if w := useBootstrapPath(h, "/auth/bootstrap?t="+expiring); w.Code != http.StatusGone {
		t.Fatalf("expired minted link = %d, want 410", w.Code)
	}

	codes := make([]string, 0, maxMintedBootstraps+1)
	for i := 0; i <= maxMintedBootstraps; i++ {
		clock = clock.Add(time.Second)
		code, err := a.MintBootstrapCode()
		if err != nil {
			t.Fatal(err)
		}
		codes = append(codes, code)
	}
	if len(a.minted) != maxMintedBootstraps {
		t.Fatalf("outstanding minted codes = %d, want %d", len(a.minted), maxMintedBootstraps)
	}
	if w := useBootstrapPath(h, "/auth/bootstrap?t="+codes[0]); w.Code != http.StatusGone {
		t.Fatalf("evicted oldest code = %d, want 410", w.Code)
	}
	for _, code := range codes[1:] {
		if w := useBootstrapPath(h, "/auth/bootstrap?t="+code); w.Code != http.StatusFound {
			t.Fatalf("outstanding code = %d, want 302", w.Code)
		}
	}
}

// TestBootstrapCodesAreSpentAtMostOnce: across any interleaving of mints,
// clock moves and redemption attempts, each code sets the cookie at most
// once, never after its expiry, and an unknown code never does.
func TestBootstrapCodesAreSpentAtMostOnce(t *testing.T) {
	property := func(ops []uint8) bool {
		a, err := NewControlAuth(t.TempDir(), zerolog.Nop())
		if err != nil {
			t.Fatal(err)
		}
		clock := time.Unix(1_800_000_000, 0)
		a.now = func() time.Time { return clock }
		type issued struct {
			code    string
			expires time.Time
			spent   bool
		}
		var codes []*issued
		startup := &issued{code: a.bootstrap, expires: time.Unix(1<<40, 0)}
		codes = append(codes, startup)
		for _, op := range ops {
			switch op % 4 {
			case 0:
				code, err := a.MintBootstrapCode()
				if err != nil {
					t.Fatal(err)
				}
				codes = append(codes, &issued{code: code, expires: clock.Add(mintedBootstrapTTL)})
			case 1:
				clock = clock.Add(time.Duration(op) * time.Second)
			case 2:
				if a.consumeBootstrapCode(strings.Repeat("f", 64)) {
					return false
				}
			default:
				target := codes[int(op/4)%len(codes)]
				got := a.consumeBootstrapCode(target.code)
				if got && (target.spent || !clock.Before(target.expires)) {
					return false
				}
				if got {
					target.spent = true
				}
			}
		}
		return true
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 300}); err != nil {
		t.Fatal(err)
	}
}
