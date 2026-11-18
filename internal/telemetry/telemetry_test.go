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
// them: a new Client each time, on the same data dir. Both must send the same
// install ID, the one saved in telemetry-id. The 24-hour limit doesn't carry
// over between Clients, so each launch sends.
func TestInstallIDStableAcrossLaunches(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var ids []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p payload
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &p); err != nil {
			t.Errorf("decode heartbeat: %v", err)
			return
		}
		mu.Lock()
		ids = append(ids, p.InstallID)
		mu.Unlock()
	}))
	defer srv.Close()

	dir := t.TempDir()
	for launch := 0; launch < 2; launch++ {
		c := New(dir, "test", true)
		c.SetEndpoint(srv.URL)
		c.MaybeSend(context.Background(), PlatformStatus{})
	}

	mu.Lock()
	defer mu.Unlock()
	if len(ids) != 2 {
		t.Fatalf("got %d heartbeats from two launches; want 2", len(ids))
	}
	if ids[0] != ids[1] {
		t.Fatalf("install id changed between launches: %q vs %q", ids[0], ids[1])
	}
	saved, err := os.ReadFile(filepath.Join(dir, "telemetry-id"))
	if err != nil {
		t.Fatalf("read telemetry-id: %v", err)
	}
	if string(saved) != ids[0] {
		t.Fatalf("telemetry-id holds %q; heartbeats sent %q", saved, ids[0])
	}
}

// TestUnsavableInstallIDSendsNothing checks that a heartbeat never carries an
// ID that couldn't be saved: with a data dir that can't hold telemetry-id,
// MaybeSend sends nothing.
func TestUnsavableInstallIDSendsNothing(t *testing.T) {
	t.Parallel()

	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
	}))
	defer srv.Close()

	// A regular file where the data dir should be: MkdirAll fails.
	notADir := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(notADir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := New(notADir, "test", true)
	c.SetEndpoint(srv.URL)
	c.MaybeSend(context.Background(), PlatformStatus{})

	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Fatalf("made %d HTTP calls with an unsavable install id; want 0", got)
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
