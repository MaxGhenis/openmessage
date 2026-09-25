package app

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestContactRefreshLoopRepeatsAndCancels(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	var calls atomic.Int32
	go func() {
		defer close(done)
		runContactRefreshLoop(ctx, time.Millisecond, func() {
			if calls.Add(1) == 3 {
				cancel()
			}
		})
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("worker did not stop")
	}
	if calls.Load() < 3 {
		t.Fatal("refresh did not repeat")
	}
}
func TestContactDirectoryWorkerClosesAndDoesNotRestart(t *testing.T) {
	t.Setenv("OPENMESSAGES_GOOGLE_CONTACTS_MCP_URL", "http://127.0.0.1:1/mcp")
	a := newTestApp(t, &mockGMClient{})
	a.StartContactDirectorySync()
	a.StartContactDirectorySync()
	a.stopContactDirectorySync()
	a.StartContactDirectorySync()
	if _, err := a.SyncGoogleContacts(); err == nil {
		t.Fatal("sync ran after shutdown")
	}
	if a.GetContactSyncStatus().Running {
		t.Fatal("worker left running")
	}
}
func TestContactRefreshInterval(t *testing.T) {
	for _, s := range []string{"0s", "-1m", "30s", "invalid", "25h"} {
		t.Setenv("OPENMESSAGES_CONTACTS_REFRESH_INTERVAL", s)
		if contactRefreshInterval() != 5*time.Minute {
			t.Fatal(s)
		}
	}
	t.Setenv("OPENMESSAGES_CONTACTS_REFRESH_INTERVAL", "2m")
	if contactRefreshInterval() != 2*time.Minute {
		t.Fatal("valid interval ignored")
	}
}

func TestContactDirectoryDisabledForNonLegacyDaemon(t *testing.T) {
	t.Setenv("OPENMESSAGES_GOOGLE_CONTACTS_MCP_URL", "http://127.0.0.1:1/mcp")
	a := newTestApp(t, &mockGMClient{})
	a.ContactDirectoryDisabled = true
	a.StartContactDirectorySync()
	if a.contactDirectory.cancel != nil {
		t.Fatal("started unsupported worker")
	}
	if _, err := a.SyncGoogleContacts(); err == nil {
		t.Fatal("wrote unsupported read store")
	}
	if a.GetContactSyncStatus().Source != "disabled" {
		t.Fatal("misleading status")
	}
}
