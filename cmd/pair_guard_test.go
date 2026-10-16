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
	"strings"
	"testing"
	"testing/quick"
	"time"

	"github.com/maxghenis/openmessage/internal/client"
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

type probeOutcome struct {
	running bool
	served  string
	err     error
}

type scriptedProbe struct {
	outcomes []probeOutcome
	calls    int
}

func (p *scriptedProbe) probe(context.Context, string) (bool, string, error) {
	i := p.calls
	if i >= len(p.outcomes) {
		i = len(p.outcomes) - 1
	}
	p.calls++
	o := p.outcomes[i]
	return o.running, o.served, o.err
}

func TestWaitForBackendToReleaseSession(t *testing.T) {
	dataDir := t.TempDir()
	otherDir := t.TempDir()
	linkDir := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dataDir, linkDir); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	hung := errors.New("context deadline exceeded")
	const wait = 2 * time.Second // four 500 ms polls after the first probe

	cases := []struct {
		name       string
		outcomes   []probeOutcome
		wantErr    string
		wantProbes int
	}{
		{name: "no backend", outcomes: []probeOutcome{{}}, wantProbes: 1},
		{name: "backend serves another data dir", outcomes: []probeOutcome{{running: true, served: otherDir}}, wantProbes: 1},
		{name: "backend serves this data dir and stays up", outcomes: []probeOutcome{{running: true, served: dataDir}}, wantErr: "quit the OpenMessage app first", wantProbes: 5},
		{name: "backend names this data dir with a trailing slash", outcomes: []probeOutcome{{running: true, served: dataDir + string(filepath.Separator)}}, wantErr: "quit the OpenMessage app first", wantProbes: 5},
		{name: "backend names this data dir through a symlink", outcomes: []probeOutcome{{running: true, served: linkDir}}, wantErr: "quit the OpenMessage app first", wantProbes: 5},
		{name: "backend does not name its data dir", outcomes: []probeOutcome{{running: true}}, wantErr: "quit the OpenMessage app first", wantProbes: 5},
		{name: "hung backend", outcomes: []probeOutcome{{err: hung}}, wantErr: "cannot rule out a running OpenMessage backend", wantProbes: 5},
		{
			name:       "backend exits while waiting (the app's stop-then-pair race)",
			outcomes:   []probeOutcome{{running: true, served: dataDir}, {running: true, served: dataDir}, {}},
			wantProbes: 3,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			probe := &scriptedProbe{outcomes: tc.outcomes}
			var slept time.Duration
			var err error
			out := captureStdout(t, func() {
				err = waitForBackendToReleaseSession(context.Background(), dataDir, probe.probe, wait, func(d time.Duration) { slept += d })
			})
			if probe.calls != tc.wantProbes {
				t.Fatalf("probes = %d, want %d", probe.calls, tc.wantProbes)
			}
			if slept > wait {
				t.Fatalf("slept %v, more than the %v budget", slept, wait)
			}
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected refusal: %v", err)
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want it to contain %q", err, tc.wantErr)
				}
				assertSwiftSafe(t, err.Error())
			}
			assertSwiftSafe(t, out)
			if tc.wantProbes > 1 && !strings.Contains(out, "Waiting for the running OpenMessage backend to exit") {
				t.Fatalf("waited without saying so; stdout %q", out)
			}
		})
	}
}

// TestWaitForBackendToReleaseSessionProperty: for any probe sequence, pairing
// proceeds exactly when some probe inside the wait budget reported no conflict,
// it stops probing at the first such report, and it never sleeps past the
// budget.
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
		firstClear := -1
		for i, k := range kinds {
			switch k % 5 {
			case 0:
				outcomes[i] = probeOutcome{}
			case 1:
				outcomes[i] = probeOutcome{running: true, served: otherDir}
			case 2:
				outcomes[i] = probeOutcome{running: true, served: dataDir}
			case 3:
				outcomes[i] = probeOutcome{running: true}
			case 4:
				outcomes[i] = probeOutcome{err: errors.New("timeout")}
			}
		}
		// The scripted probe repeats its last outcome, so extend the
		// expectation the same way.
		for i := 0; i < maxProbes; i++ {
			o := outcomes[min(i, len(outcomes)-1)]
			if o.err == nil && (!o.running || o.served == otherDir) {
				firstClear = i
				break
			}
		}
		probe := &scriptedProbe{outcomes: outcomes}
		var slept time.Duration
		var err error
		captureStdout(t, func() {
			err = waitForBackendToReleaseSession(context.Background(), dataDir, probe.probe, wait, func(d time.Duration) { slept += d })
		})
		if slept > wait {
			return false
		}
		if firstClear >= 0 {
			return err == nil && probe.calls == firstClear+1
		}
		return err != nil && probe.calls == maxProbes
	}
	if err := quick.Check(check, &quick.Config{MaxCount: 500}); err != nil {
		t.Fatal(err)
	}
}

func TestDefaultPairBackendProbe(t *testing.T) {
	dataDir := t.TempDir()

	t.Run("refused connection is no backend", func(t *testing.T) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		_, port, _ := net.SplitHostPort(listener.Addr().String())
		listener.Close()
		t.Setenv("OPENMESSAGES_HOST", "127.0.0.1")
		t.Setenv("OPENMESSAGES_PORT", port)
		running, served, err := defaultPairBackendProbe(context.Background(), dataDir)
		if err != nil || running || served != "" {
			t.Fatalf("probe = (%v, %q, %v), want (false, \"\", nil)", running, served, err)
		}
	})

	t.Run("status names the served data dir", func(t *testing.T) {
		var sawToken string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sawToken = r.Header.Get("Authorization")
			_ = json.NewEncoder(w).Encode(map[string]any{"auth": map[string]any{"data_dir": dataDir}})
		}))
		defer server.Close()
		if err := os.WriteFile(filepath.Join(dataDir, "control.token"), []byte("tok\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		host, port, _ := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
		t.Setenv("OPENMESSAGES_HOST", host)
		t.Setenv("OPENMESSAGES_PORT", port)
		running, served, err := defaultPairBackendProbe(context.Background(), dataDir)
		if err != nil || !running || served != dataDir {
			t.Fatalf("probe = (%v, %q, %v), want (true, %q, nil)", running, served, err, dataDir)
		}
		if sawToken != "Bearer tok" {
			t.Fatalf("status probe sent Authorization %q, want the data dir's control token", sawToken)
		}
	})

	t.Run("an answer without a status document is a running backend of unknown dir", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "nope", http.StatusUnauthorized)
		}))
		defer server.Close()
		host, port, _ := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
		t.Setenv("OPENMESSAGES_HOST", host)
		t.Setenv("OPENMESSAGES_PORT", port)
		running, served, err := defaultPairBackendProbe(context.Background(), dataDir)
		if err != nil || !running || served != "" {
			t.Fatalf("probe = (%v, %q, %v), want (true, \"\", nil)", running, served, err)
		}
	})
}

func TestResolvePairModeGoogleChrome(t *testing.T) {
	t.Setenv("OPENMESSAGE_CHROME_PROFILE", "/profiles/Default")

	mode, profile, err := resolvePairMode([]string{"--google-chrome"})
	if err != nil || mode != pairModeGoogleChrome || profile != "/profiles/Default" {
		t.Fatalf("default profile: (%q, %q, %v)", mode, profile, err)
	}
	mode, profile, err = resolvePairMode([]string{"--google-chrome", " /profiles/Profile 1 "})
	if err != nil || mode != pairModeGoogleChrome || profile != "/profiles/Profile 1" {
		t.Fatalf("explicit profile: (%q, %q, %v)", mode, profile, err)
	}
}

func TestLoadChromeCookiesForPairingNeverPrintsValues(t *testing.T) {
	secretValues := map[string]string{
		"SID": "sid-SECRET-1", "HSID": "hsid-SECRET-2", "SSID": "ssid-SECRET-3",
		"APISID": "apisid-SECRET-4", "SAPISID": "sapisid-SECRET-5",
	}
	var gotProfile string
	var cookies map[string]string
	var err error
	out := captureStdout(t, func() {
		cookies, err = loadChromeCookiesForPairing(context.Background(), "/p/Default", func(_ context.Context, profile string) (map[string]string, error) {
			gotProfile = profile
			return secretValues, nil
		})
	})
	if err != nil || gotProfile != "/p/Default" || len(cookies) != len(secretValues) {
		t.Fatalf("load = (%d cookies, %v), profile %q", len(cookies), err, gotProfile)
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

func TestSavePairedSessionBacksUpTheSessionItReplaces(t *testing.T) {
	dir := t.TempDir()
	sessionPath := filepath.Join(dir, "session.json")
	now := time.Date(2026, 10, 16, 15, 4, 5, 0, time.Local)
	first := &client.SessionData{AuthDataJSON: json.RawMessage(`{"generation":1}`)}
	second := &client.SessionData{AuthDataJSON: json.RawMessage(`{"generation":2}`)}
	third := &client.SessionData{AuthDataJSON: json.RawMessage(`{"generation":3}`)}

	// A first pairing has nothing to back up.
	out := captureStdout(t, func() {
		if err := savePairedSession(sessionPath, first, now); err != nil {
			t.Fatalf("first save: %v", err)
		}
	})
	if strings.Contains(out, "backed up") {
		t.Fatalf("first save claimed a backup: %q", out)
	}
	firstRaw, err := os.ReadFile(sessionPath)
	if err != nil {
		t.Fatal(err)
	}

	// A re-pair keeps the replaced session, byte for byte, owner-only.
	captureStdout(t, func() {
		if err := savePairedSession(sessionPath, second, now); err != nil {
			t.Fatalf("second save: %v", err)
		}
	})
	backup := sessionPath + ".bak-20261016-150405"
	assertFile(t, backup, firstRaw)

	// Two re-pairs in the same second never overwrite an earlier backup.
	secondRaw, _ := os.ReadFile(sessionPath)
	captureStdout(t, func() {
		if err := savePairedSession(sessionPath, third, now); err != nil {
			t.Fatalf("third save: %v", err)
		}
	})
	assertFile(t, backup, firstRaw)
	assertFile(t, backup+"-2", secondRaw)

	loaded, err := client.LoadSession(sessionPath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	var auth struct{ Generation int }
	if err := json.Unmarshal(loaded.AuthDataJSON, &auth); err != nil || auth.Generation != 3 {
		t.Fatalf("session.json holds %s, want the newest pairing", loaded.AuthDataJSON)
	}
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

func TestWithGaiaConfirmTimeout(t *testing.T) {
	t.Run("an unanswered prompt is canceled at the timeout", func(t *testing.T) {
		var sawErr error
		err := withGaiaConfirmTimeout(context.Background(), 20*time.Millisecond, func(ctx context.Context) error {
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

	t.Run("a confirmation inside the window passes through", func(t *testing.T) {
		if err := withGaiaConfirmTimeout(context.Background(), time.Minute, func(context.Context) error { return nil }); err != nil {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("a pairing error before the timeout is not called a timeout", func(t *testing.T) {
		wrong := errors.New("incorrect emoji")
		err := withGaiaConfirmTimeout(context.Background(), time.Minute, func(context.Context) error { return wrong })
		if err != wrong {
			t.Fatalf("err = %v, want the pairing error unchanged", err)
		}
	})

	t.Run("an outer cancel (Ctrl-C) is not called a timeout", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := withGaiaConfirmTimeout(ctx, time.Minute, func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		})
		if !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "did not confirm") {
			t.Fatalf("err = %v, want a plain cancel", err)
		}
	})
}
