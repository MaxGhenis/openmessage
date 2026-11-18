package telemetry

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDisabledClientDoesNothing(t *testing.T) {
	t.Parallel()

	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
	}))
	defer srv.Close()

	c := New(t.TempDir(), "test", false)
	c.SetEndpoint(srv.URL)
	c.MaybeSend(context.Background(), PlatformStatus{GoogleMessages: true})

	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Fatalf("disabled client made %d HTTP calls; want 0", got)
	}
}

func TestEnabledClientSendsOnce(t *testing.T) {
	t.Parallel()

	var calls int32
	var captured payload
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &captured)
	}))
	defer srv.Close()

	c := New(t.TempDir(), "v0.2.0", true)
	c.SetEndpoint(srv.URL)
	c.MaybeSend(context.Background(), PlatformStatus{GoogleMessages: true, WhatsApp: true})

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected 1 call, got %d", got)
	}
	if captured.Version != "v0.2.0" {
		t.Fatalf("version = %q; want v0.2.0", captured.Version)
	}
	if !captured.Platforms.GoogleMessages || !captured.Platforms.WhatsApp || captured.Platforms.Signal {
		t.Fatalf("platforms not propagated correctly: %+v", captured.Platforms)
	}
	if len(captured.InstallID) != 32 {
		t.Fatalf("install id wrong length: %q", captured.InstallID)
	}
}

func TestRespectsHeartbeatPeriod(t *testing.T) {
	t.Parallel()

	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
	}))
	defer srv.Close()

	c := New(t.TempDir(), "test", true)
	c.SetEndpoint(srv.URL)

	for i := 0; i < 5; i++ {
		c.MaybeSend(context.Background(), PlatformStatus{})
	}

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected exactly 1 call within period, got %d", got)
	}
}

func TestInstallIDIsStable(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	c := New(dir, "test", true)

	id1, err := c.installID()
	if err != nil {
		t.Fatalf("first install id: %v", err)
	}
	id2, err := c.installID()
	if err != nil {
		t.Fatalf("second install id: %v", err)
	}
	if id1 != id2 {
		t.Fatalf("install id changed: %q vs %q", id1, id2)
	}
	if len(id1) != 32 {
		t.Fatalf("install id length = %d; want 32", len(id1))
	}
}

// TestInstallIDStableAcrossLaunches models two launches the way serve does
// them, a new Client each time on the same data dir: the second launch's
// install ID is the one the first launch sent and saved in telemetry-id.
func TestInstallIDStableAcrossLaunches(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var sent []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p payload
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &p); err != nil {
			t.Errorf("decode heartbeat: %v", err)
			return
		}
		mu.Lock()
		sent = append(sent, p.InstallID)
		mu.Unlock()
	}))
	defer srv.Close()

	dir := t.TempDir()
	first := New(dir, "test", true)
	first.SetEndpoint(srv.URL)
	first.MaybeSend(context.Background(), PlatformStatus{})

	mu.Lock()
	defer mu.Unlock()
	if len(sent) != 1 || len(sent[0]) != 32 {
		t.Fatalf("first launch sent %q; want one 32-character install id", sent)
	}

	second := New(dir, "test", true)
	secondID, err := second.installID()
	if err != nil {
		t.Fatalf("second launch install id: %v", err)
	}
	if secondID != sent[0] {
		t.Fatalf("install id changed between launches: %q vs %q", sent[0], secondID)
	}
	saved, err := os.ReadFile(filepath.Join(dir, "telemetry-id"))
	if err != nil {
		t.Fatalf("read telemetry-id: %v", err)
	}
	if string(saved) != sent[0] {
		t.Fatalf("telemetry-id holds %q; first launch sent %q", saved, sent[0])
	}
}

// TestEachLaunchSendsAgain pins the package comment's claim that the 24-hour
// guard doesn't survive a relaunch: two Clients on one data dir, back to back,
// both send. If lastSent is ever persisted, update that comment with this test.
func TestEachLaunchSendsAgain(t *testing.T) {
	t.Parallel()

	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
	}))
	defer srv.Close()

	dir := t.TempDir()
	for launch := 0; launch < 2; launch++ {
		c := New(dir, "test", true)
		c.SetEndpoint(srv.URL)
		c.MaybeSend(context.Background(), PlatformStatus{})
	}

	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("two launches sent %d heartbeats; want 2", got)
	}
}

// TestUnsavableInstallIDSendsNothing checks that a heartbeat never carries an
// ID that couldn't be saved: when the data dir can't be created or
// telemetry-id can't be written, MaybeSend sends nothing.
func TestUnsavableInstallIDSendsNothing(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		dataDir func(t *testing.T) string
	}{
		{
			// A regular file where the data dir should be: MkdirAll fails.
			name: "data dir can't be created",
			dataDir: func(t *testing.T) string {
				path := filepath.Join(t.TempDir(), "not-a-dir")
				if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
					t.Fatal(err)
				}
				return path
			},
		},
		{
			// A directory named telemetry-id: MkdirAll succeeds, but reading
			// and writing the file both fail.
			name: "telemetry-id can't be written",
			dataDir: func(t *testing.T) string {
				dir := t.TempDir()
				if err := os.Mkdir(filepath.Join(dir, "telemetry-id"), 0o700); err != nil {
					t.Fatal(err)
				}
				return dir
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var calls int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&calls, 1)
			}))
			defer srv.Close()

			c := New(tc.dataDir(t), "test", true)
			c.SetEndpoint(srv.URL)
			c.MaybeSend(context.Background(), PlatformStatus{})

			if got := atomic.LoadInt32(&calls); got != 0 {
				t.Fatalf("made %d HTTP calls with an unsavable install id; want 0", got)
			}
		})
	}
}

func TestNetworkErrorIsSilent(t *testing.T) {
	t.Parallel()

	c := New(t.TempDir(), "test", true)
	c.SetEndpoint("http://127.0.0.1:1") // closed port
	c.httpc = &http.Client{Timeout: 50 * time.Millisecond}

	// Should not panic or block.
	done := make(chan struct{})
	go func() {
		c.MaybeSend(context.Background(), PlatformStatus{})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("MaybeSend blocked too long on network failure")
	}
}
