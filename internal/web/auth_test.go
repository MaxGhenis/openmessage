package web

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

func TestControlAuthAcceptAndLogClassifications(t *testing.T) {
	var logs bytes.Buffer
	a, err := NewControlAuth(t.TempDir(), zerolog.New(&logs))
	if err != nil {
		t.Fatal(err)
	}
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	h := ProtectLocalControl(a.Handler(next))
	tests := []struct {
		name     string
		apply    func(*http.Request)
		wantWarn bool
	}{
		{"bearer", func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+a.token) }, false},
		{"cookie", func(r *http.Request) { r.AddCookie(&http.Cookie{Name: ControlCookieName, Value: a.token}) }, false},
		{"missing", func(*http.Request) {}, true},
		{"invalid", func(r *http.Request) { r.Header.Set("Authorization", "Bearer wrong") }, true},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logs.Reset()
			r := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/status", nil)
			r.RemoteAddr = "127.0.0." + string(rune('1'+i)) + ":7007"
			tt.apply(r)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != http.StatusNoContent {
				t.Fatalf("status = %d, accept-and-log must pass", w.Code)
			}
			if got := strings.Contains(logs.String(), `"level":"warn"`); got != tt.wantWarn {
				t.Fatalf("warn = %t, want %t; %s", got, tt.wantWarn, logs.String())
			}
		})
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
		h.ServeHTTP(httptest.NewRecorder(), r)
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
		a.warnOnce(r, "missing", "")
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

func TestStatusReportsAcceptAndLogAuth(t *testing.T) {
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
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/status", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	auth, ok := body["auth"].(map[string]any)
	if !ok || auth["mode"] != AuthModeAcceptAndLog || auth["token_present"] != true || auth["data_dir"] != a.dataDir {
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
