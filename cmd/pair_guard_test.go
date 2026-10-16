package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"testing/quick"
	"time"

	"github.com/rs/zerolog"
)

// captureStdout runs fn with os.Stdout redirected and returns what it printed.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	done := make(chan string)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()
	defer func() { os.Stdout = old }()
	fn()
	w.Close()
	os.Stdout = old
	return <-done
}

// assertSwiftSafe checks a line the macOS PairingView parses: it turns any
// line containing "success" or "paired" into a pairing success, and any
// "https://" into a QR URL, so a failure message must contain none of them.
func assertSwiftSafe(t *testing.T, text string) {
	t.Helper()
	lower := strings.ToLower(text)
	for _, needle := range []string{"success", "paired", "https://"} {
		if strings.Contains(lower, needle) {
			t.Fatalf("%q contains %q, which the macOS pairing wrapper misreads", text, needle)
		}
	}
}

// fakeClock is a clock that only moves when sleep or a probe moves it.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 10, 16, 18, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

type probeOutcome struct {
	state pairBackendState
	err   error
}

type scriptedProbe struct {
	outcomes []probeOutcome
	calls    int
}

func (p *scriptedProbe) probe(context.Context, string) (pairBackendState, error) {
	i := min(p.calls, len(p.outcomes)-1)
	p.calls++
	return p.outcomes[i].state, p.outcomes[i].err
}

func holding(dir string) pairBackendState {
	return pairBackendState{Running: true, StatusKnown: true, DataDir: dir}
}

func TestPairBackendStateMayRewriteSession(t *testing.T) {
	dataDir := t.TempDir()
	otherDir := t.TempDir()
	linkDir := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dataDir, linkDir); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	cases := []struct {
		name  string
		state pairBackendState
		want  bool
	}{
		{name: "nothing listening", state: pairBackendState{}, want: false},
		{name: "answers without a status", state: pairBackendState{Running: true}, want: true},
		{name: "serves another data dir", state: holding(otherDir), want: false},
		{name: "serves this data dir with a session", state: holding(dataDir), want: true},
		{name: "serves this data dir through a symlink", state: holding(linkDir), want: true},
		{name: "serves this data dir with a trailing slash", state: holding(dataDir + string(filepath.Separator)), want: true},
		{name: "does not name its data dir", state: pairBackendState{Running: true, StatusKnown: true}, want: true},
		{name: "this data dir, no Google session (Docker first pair, in-app after unpair)", state: pairBackendState{Running: true, StatusKnown: true, DataDir: dataDir, NoGoogleSession: true}, want: false},
		{name: "no data dir named, no Google session", state: pairBackendState{Running: true, StatusKnown: true, NoGoogleSession: true}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.state.mayRewriteSession(dataDir); got != tc.want {
				t.Fatalf("mayRewriteSession(%+v) = %v, want %v", tc.state, got, tc.want)
			}
		})
	}
}

func TestWaitForBackendToReleaseSession(t *testing.T) {
	dataDir := t.TempDir()
	hung := errors.New("context deadline exceeded")
	const wait = 2 * time.Second // four 500 ms polls after the first probe

	cases := []struct {
		name       string
		outcomes   []probeOutcome
		wantErr    string
		wantProbes int
	}{
		{name: "no backend", outcomes: []probeOutcome{{}}, wantProbes: 1},
		{name: "backend without a Google session", outcomes: []probeOutcome{{state: pairBackendState{Running: true, StatusKnown: true, DataDir: dataDir, NoGoogleSession: true}}}, wantProbes: 1},
		{name: "backend holding this session stays up", outcomes: []probeOutcome{{state: holding(dataDir)}}, wantErr: "stop it first", wantProbes: 5},
		{name: "hung backend", outcomes: []probeOutcome{{err: hung}}, wantErr: "cannot rule out", wantProbes: 5},
		{
			name:       "backend exits while waiting (the app's stop-then-pair race)",
			outcomes:   []probeOutcome{{state: holding(dataDir)}, {state: holding(dataDir)}, {}},
			wantProbes: 3,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clock := newFakeClock()
			start := clock.Now()
			probe := &scriptedProbe{outcomes: tc.outcomes}
			var err error
			out := captureStdout(t, func() {
				err = waitForBackendToReleaseSession(context.Background(), dataDir, probe.probe, wait, clock.Now, clock.Advance)
			})
			if probe.calls != tc.wantProbes {
				t.Fatalf("probes = %d, want %d", probe.calls, tc.wantProbes)
			}
			elapsed := clock.Now().Sub(start)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected refusal: %v", err)
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want it to contain %q", err, tc.wantErr)
				}
				// A refusal waits out the whole budget, no more and no less.
				if elapsed != wait {
					t.Fatalf("waited %v before refusing, want exactly %v", elapsed, wait)
				}
				assertSwiftSafe(t, err.Error())
				if strings.Contains(err.Error(), "osascript") && !strings.Contains(err.Error(), "docker compose run") {
					t.Fatalf("refusal %q gives only macOS advice", err)
				}
			}
			assertSwiftSafe(t, out)
			if tc.wantProbes > 1 && !strings.Contains(out, "Waiting for the OpenMessage backend") {
				t.Fatalf("waited without saying so; stdout %q", out)
			}
		})
	}
}

// A backend that accepts connections and never answers makes every probe run
// to its timeout. The wait still ends on time: each probe's timeout is capped
// at what is left of the budget.
func TestWaitForBackendToReleaseSessionBoundsWallTimeAgainstAHungBackend(t *testing.T) {
	dataDir := t.TempDir()
	clock := newFakeClock()
	start := clock.Now()
	var timeouts []time.Duration
	probe := func(ctx context.Context, _ string) (pairBackendState, error) {
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Fatal("probe context has no deadline")
		}
		timeout := time.Until(deadline).Round(100 * time.Millisecond)
		timeouts = append(timeouts, timeout)
		clock.Advance(timeout)
		return pairBackendState{}, context.DeadlineExceeded
	}
	var err error
	captureStdout(t, func() {
		err = waitForBackendToReleaseSession(context.Background(), dataDir, probe, pairBackendExitWait, clock.Now, clock.Advance)
	})
	if err == nil || !strings.Contains(err.Error(), "cannot rule out") {
		t.Fatalf("error = %v, want a refusal", err)
	}
	if elapsed := clock.Now().Sub(start); elapsed > pairBackendExitWait+time.Second {
		t.Fatalf("hung backend kept pair waiting %v (probe timeouts %v), want about %v", elapsed, timeouts, pairBackendExitWait)
	}
	for _, timeout := range timeouts {
		if timeout > pairProbeTimeout {
			t.Fatalf("probe timeout %v exceeds %v", timeout, pairProbeTimeout)
		}
	}
}

// TestWaitForBackendToReleaseSessionProperty: for any probe sequence, pairing
// proceeds exactly when some probe inside the wait budget reported a backend
// that cannot rewrite the session, it stops probing at the first such
// report, and a refusal waits out exactly the budget.
func TestWaitForBackendToReleaseSessionProperty(t *testing.T) {
	dataDir := t.TempDir()
	otherDir := t.TempDir()
	const wait = 3 * time.Second
	const maxProbes = 7 // first probe + 3s / 500ms

	check := func(kinds []uint8) bool {
		if len(kinds) == 0 {
			kinds = []uint8{0}
		}
		outcomes := make([]probeOutcome, len(kinds))
		for i, k := range kinds {
			switch k % 6 {
			case 0:
				outcomes[i] = probeOutcome{}
			case 1:
				outcomes[i] = probeOutcome{state: holding(otherDir)}
			case 2:
				outcomes[i] = probeOutcome{state: holding(dataDir)}
			case 3:
				outcomes[i] = probeOutcome{state: pairBackendState{Running: true}}
			case 4:
				outcomes[i] = probeOutcome{err: errors.New("timeout")}
			case 5:
				outcomes[i] = probeOutcome{state: pairBackendState{Running: true, StatusKnown: true, DataDir: dataDir, NoGoogleSession: true}}
			}
		}
		firstClear := -1
		for i := 0; i < maxProbes; i++ {
			o := outcomes[min(i, len(outcomes)-1)]
			if o.err == nil && !o.state.mayRewriteSession(dataDir) {
				firstClear = i
				break
			}
		}
		clock := newFakeClock()
		start := clock.Now()
		probe := &scriptedProbe{outcomes: outcomes}
		var err error
		captureStdout(t, func() {
			err = waitForBackendToReleaseSession(context.Background(), dataDir, probe.probe, wait, clock.Now, clock.Advance)
		})
		if firstClear >= 0 {
			return err == nil && probe.calls == firstClear+1
		}
		return err != nil && probe.calls == maxProbes && clock.Now().Sub(start) == wait
	}
	if err := quick.Check(check, &quick.Config{MaxCount: 500}); err != nil {
		t.Fatal(err)
	}
}

func TestDefaultPairBackendProbe(t *testing.T) {
	dataDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dataDir, "control.token"), []byte("tok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	serve := func(t *testing.T, host string, handler http.HandlerFunc) {
		t.Helper()
		server := httptest.NewServer(handler)
		t.Cleanup(server.Close)
		_, port, _ := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
		t.Setenv("OPENMESSAGES_HOST", host)
		t.Setenv("OPENMESSAGES_PORT", port)
	}
	status := func(paired bool) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"auth":   map[string]any{"data_dir": dataDir},
				"google": map[string]any{"paired": paired, "connected": paired},
			})
		}
	}

	t.Run("refused connection is no backend", func(t *testing.T) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		_, port, _ := net.SplitHostPort(listener.Addr().String())
		listener.Close()
		t.Setenv("OPENMESSAGES_HOST", "127.0.0.1")
		t.Setenv("OPENMESSAGES_PORT", port)
		state, err := defaultPairBackendProbe(context.Background(), dataDir)
		if err != nil || state.Running {
			t.Fatalf("probe = (%+v, %v), want not running", state, err)
		}
	})

	t.Run("a backend holding the Google session", func(t *testing.T) {
		var sawToken string
		serve(t, "127.0.0.1", func(w http.ResponseWriter, r *http.Request) {
			sawToken = r.Header.Get("Authorization")
			status(true)(w, r)
		})
		state, err := defaultPairBackendProbe(context.Background(), dataDir)
		if err != nil || !state.StatusKnown || state.DataDir != dataDir || state.NoGoogleSession || !state.mayRewriteSession(dataDir) {
			t.Fatalf("probe = (%+v, %v), want a backend holding this session", state, err)
		}
		if sawToken != "Bearer tok" {
			t.Fatalf("local status probe sent Authorization %q, want the data dir's control token", sawToken)
		}
	})

	t.Run("an unpaired backend on the same data dir (Docker first pairing)", func(t *testing.T) {
		serve(t, "0.0.0.0", status(false))
		state, err := defaultPairBackendProbe(context.Background(), dataDir)
		if err != nil || !state.NoGoogleSession || state.mayRewriteSession(dataDir) {
			t.Fatalf("probe = (%+v, %v), want a backend with no Google session", state, err)
		}
	})

	t.Run("an answer without a status document", func(t *testing.T) {
		serve(t, "127.0.0.1", func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "nope", http.StatusUnauthorized)
		})
		state, err := defaultPairBackendProbe(context.Background(), dataDir)
		if err != nil || !state.Running || state.StatusKnown || !state.mayRewriteSession(dataDir) {
			t.Fatalf("probe = (%+v, %v), want a running backend of unknown state", state, err)
		}
	})
}

func TestPairProbeHostIsLocal(t *testing.T) {
	for host, want := range map[string]bool{
		"": true, "localhost": true, "127.0.0.1": true, "::1": true, "[::1]": true,
		"0.0.0.0": true, "::": true, "192.168.1.5": false, "example.com": false,
	} {
		t.Setenv("OPENMESSAGES_HOST", host)
		if got := pairProbeHostIsLocal(); got != want {
			t.Errorf("pairProbeHostIsLocal(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestResolvePairModeGoogleChrome(t *testing.T) {
	if runtime.GOOS != "darwin" {
		_, _, err := resolvePairMode([]string{"--google-chrome"})
		if err == nil || !strings.Contains(err.Error(), "only on macOS") {
			t.Fatalf("err = %v, want a macOS-only refusal", err)
		}
		return
	}
	t.Setenv("OPENMESSAGE_CHROME_PROFILE", "/profiles/Default")
	cases := []struct {
		args []string
		want string
	}{
		{args: []string{"--google-chrome"}, want: "/profiles/Default"},
		{args: []string{"--google-chrome", " /elsewhere/Profile 2 "}, want: "/elsewhere/Profile 2"},
		{args: []string{"--google-chrome", "Profile 1"}, want: "/profiles/Profile 1"},
	}
	for _, tc := range cases {
		mode, profile, err := resolvePairMode(tc.args)
		if err != nil || mode != pairModeGoogleChrome || profile != tc.want {
			t.Fatalf("resolvePairMode(%q) = (%q, %q, %v), want profile %q", tc.args, mode, profile, err, tc.want)
		}
	}
}

func TestLoadChromeCookiesForPairingNeverPrintsValues(t *testing.T) {
	secretValues := map[string]string{
		"SID": "sid-SECRET-1", "HSID": "hsid-SECRET-2", "SSID": "ssid-SECRET-3",
		"APISID": "apisid-SECRET-4", "SAPISID": "sapisid-SECRET-5",
	}
	var cookies map[string]string
	var err error
	out := captureStdout(t, func() {
		cookies, err = loadChromeCookiesForPairing(context.Background(), "/p/Default", func(context.Context, string) (map[string]string, error) {
			return secretValues, nil
		})
	})
	if err != nil || len(cookies) != len(secretValues) {
		t.Fatalf("load = (%d cookies, %v)", len(cookies), err)
	}
	if !strings.Contains(out, "Read 5 Google cookies from Chrome profile /p/Default") {
		t.Fatalf("stdout %q does not report the count", out)
	}
	for _, v := range secretValues {
		if strings.Contains(out, v) {
			t.Fatalf("stdout leaks a cookie value: %q", out)
		}
	}
	assertSwiftSafe(t, out)

	_, err = loadChromeCookiesForPairing(context.Background(), "/p/Default", func(context.Context, string) (map[string]string, error) {
		return nil, errors.New("missing required cookies: .google.com:SID")
	})
	if err == nil || !strings.Contains(err.Error(), "missing required cookies") || !strings.Contains(err.Error(), "/p/Default") {
		t.Fatalf("load error = %v, want the cause and the profile", err)
	}
	assertSwiftSafe(t, err.Error())
}

func TestBackupReplacedSession(t *testing.T) {
	dir := t.TempDir()
	sessionPath := filepath.Join(dir, "session.json")
	now := time.Date(2026, 10, 16, 15, 4, 5, 0, time.Local)

	if backup, err := backupReplacedSession(sessionPath, now); err != nil || backup != "" {
		t.Fatalf("no session: backup = (%q, %v), want nothing", backup, err)
	}
	if err := os.WriteFile(sessionPath, []byte(`{"generation":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := backupReplacedSession(sessionPath, now)
	if err != nil || first != sessionPath+".bak-20261016-150405" {
		t.Fatalf("first backup = (%q, %v)", first, err)
	}
	assertFile(t, first, []byte(`{"generation":1}`))
	if err := os.WriteFile(sessionPath, []byte(`{"generation":2}`), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := backupReplacedSession(sessionPath, now)
	if err != nil || second != first+"-2" {
		t.Fatalf("same-second backup = (%q, %v), want a new name", second, err)
	}
	assertFile(t, first, []byte(`{"generation":1}`))
	assertFile(t, second, []byte(`{"generation":2}`))
}

func assertFile(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s = %q, want %q", path, got, want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("%s mode %v, want 0600", path, info.Mode().Perm())
	}
}

// runPairHarness drives runPair with every seam replaced.
type runPairHarness struct {
	dataDir string
	clock   *fakeClock
	state   pairBackendState
	cookies map[string]string
	pairs   int
	// backupsAtPair is what backups existed when the phone would be asked.
	backupsAtPair []string
	pairedMode    pairMode
	pairedCookies map[string]string
}

func newRunPairHarness(t *testing.T) *runPairHarness {
	t.Helper()
	h := &runPairHarness{dataDir: t.TempDir(), clock: newFakeClock()}
	t.Setenv("OPENMESSAGES_DATA_DIR", h.dataDir)
	t.Setenv("OPENMESSAGE_CHROME_PROFILE", "/profiles/Default")
	return h
}

func (h *runPairHarness) run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	deps := pairDeps{
		probe:    func(context.Context, string) (pairBackendState, error) { return h.state, nil },
		exitWait: time.Second,
		now:      h.clock.Now,
		sleep:    h.clock.Advance,
		loadChrome: func(context.Context, string) (map[string]string, error) {
			return h.cookies, nil
		},
		pair: func(_ zerolog.Logger, mode pairMode, cookies map[string]string, sessionPath string) error {
			h.pairs++
			h.pairedMode = mode
			h.pairedCookies = cookies
			matches, _ := filepath.Glob(sessionPath + ".bak-*")
			h.backupsAtPair = matches
			return nil
		},
	}
	var err error
	out := captureStdout(t, func() { err = runPair(zerolog.Nop(), args, deps) })
	return out, err
}

func TestRunPairRefusesWhileABackendHoldsTheSession(t *testing.T) {
	h := newRunPairHarness(t)
	h.state = holding(h.dataDir)
	if err := os.WriteFile(filepath.Join(h.dataDir, "session.json"), []byte(`{"old":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := h.run(t)
	if err == nil || !strings.Contains(err.Error(), "stop it first") {
		t.Fatalf("err = %v, want a refusal", err)
	}
	if h.pairs != 0 {
		t.Fatal("paired despite a backend holding the session")
	}
	if matches, _ := filepath.Glob(filepath.Join(h.dataDir, "session.json.bak-*")); len(matches) != 0 {
		t.Fatalf("a refused pair left backups %v", matches)
	}
	assertSwiftSafe(t, out)
	assertSwiftSafe(t, err.Error())
}

func TestRunPairProceedsWhenTheBackendHoldsNoGoogleSession(t *testing.T) {
	h := newRunPairHarness(t)
	h.state = pairBackendState{Running: true, StatusKnown: true, DataDir: h.dataDir, NoGoogleSession: true}
	if _, err := h.run(t); err != nil || h.pairs != 1 || h.pairedMode != pairModeQR {
		t.Fatalf("err = %v, pairs = %d, mode = %q; want one QR pairing", err, h.pairs, h.pairedMode)
	}
}

func TestRunPairBacksUpTheSessionBeforeAskingThePhone(t *testing.T) {
	h := newRunPairHarness(t)
	sessionPath := filepath.Join(h.dataDir, "session.json")
	if err := os.WriteFile(sessionPath, []byte(`{"old":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := h.run(t)
	if err != nil || h.pairs != 1 {
		t.Fatalf("err = %v, pairs = %d", err, h.pairs)
	}
	if len(h.backupsAtPair) != 1 {
		t.Fatalf("backups when the phone was asked = %v, want one", h.backupsAtPair)
	}
	assertFile(t, h.backupsAtPair[0], []byte(`{"old":true}`))
	if !strings.Contains(out, "Current session backed up to") {
		t.Fatalf("stdout %q does not name the backup", out)
	}
	assertSwiftSafe(t, out)
}

func TestRunPairStopsBeforeThePhoneWhenTheSessionCannotBeBackedUp(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads mode-000 files")
	}
	h := newRunPairHarness(t)
	sessionPath := filepath.Join(h.dataDir, "session.json")
	if err := os.WriteFile(sessionPath, []byte(`{"old":true}`), 0o000); err != nil {
		t.Fatal(err)
	}
	_, err := h.run(t)
	if err == nil || !strings.Contains(err.Error(), "before pairing") {
		t.Fatalf("err = %v, want a refusal before pairing", err)
	}
	if h.pairs != 0 {
		t.Fatal("asked the phone although the old session could not be backed up")
	}
	assertSwiftSafe(t, err.Error())
}

func TestRunPairGoogleChromeHandsTheCookiesToThePairing(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("--google-chrome is macOS-only")
	}
	h := newRunPairHarness(t)
	h.cookies = map[string]string{"SID": "sid-SECRET", "SAPISID": "sapisid-SECRET"}
	out, err := h.run(t, "--google-chrome")
	if err != nil || h.pairs != 1 || h.pairedMode != pairModeGoogleChrome || h.pairedCookies["SID"] != "sid-SECRET" {
		t.Fatalf("err = %v, pairs = %d, mode = %q, cookies = %d", err, h.pairs, h.pairedMode, len(h.pairedCookies))
	}
	if strings.Contains(out, "SECRET") {
		t.Fatalf("stdout leaks a cookie value: %q", out)
	}
}

func TestWithGaiaConfirmTimeout(t *testing.T) {
	t.Run("an unanswered prompt is canceled at the timeout", func(t *testing.T) {
		var sawErr error
		err := withGaiaConfirmTimeout(context.Background(), 20*time.Millisecond, time.Minute, func(ctx context.Context) error {
			<-ctx.Done()
			sawErr = ctx.Err()
			return fmt.Errorf("failed to send client finish: %w", ctx.Err())
		})
		// libgm sends the phone a pairing cancel only for context.Canceled,
		// so the timeout must cancel rather than let a deadline expire.
		if !errors.Is(sawErr, context.Canceled) {
			t.Fatalf("pairing saw %v, want context.Canceled", sawErr)
		}
		if err == nil || !strings.Contains(err.Error(), "did not confirm within 20ms") || !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want the timeout explained and the cause kept", err)
		}
		assertSwiftSafe(t, err.Error())
	})

	t.Run("a pairing stuck in a wait that ignores the context is abandoned after the grace", func(t *testing.T) {
		block := make(chan struct{})
		defer close(block)
		start := time.Now()
		err := withGaiaConfirmTimeout(context.Background(), 20*time.Millisecond, 50*time.Millisecond, func(context.Context) error {
			<-block // like libgm's long-poll connect wait
			return nil
		})
		if err == nil || !strings.Contains(err.Error(), "did not stop within 50ms") || !strings.Contains(err.Error(), "did not confirm") {
			t.Fatalf("error = %v, want the stuck pairing abandoned", err)
		}
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Fatalf("returned after %v", elapsed)
		}
		assertSwiftSafe(t, err.Error())
	})

	t.Run("a confirmation inside the window passes through", func(t *testing.T) {
		if err := withGaiaConfirmTimeout(context.Background(), time.Minute, time.Minute, func(context.Context) error { return nil }); err != nil {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("a pairing error before the timeout is not called a timeout", func(t *testing.T) {
		wrong := errors.New("incorrect emoji")
		err := withGaiaConfirmTimeout(context.Background(), time.Minute, time.Minute, func(context.Context) error { return wrong })
		if err != wrong {
			t.Fatalf("err = %v, want the pairing error unchanged", err)
		}
	})

	t.Run("an outer cancel (Ctrl-C) is not called a timeout", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := withGaiaConfirmTimeout(ctx, time.Minute, time.Minute, func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		})
		if !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "did not confirm") {
			t.Fatalf("err = %v, want a plain cancel", err)
		}
	})
}
