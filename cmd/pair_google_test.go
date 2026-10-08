package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/maxghenis/openmessage/internal/client"
	"go.mau.fi/mautrix-gmessages/pkg/libgm"
)

type fakeGoogleAccountPairer struct {
	start  func(context.Context, context.Context) (string, *libgm.PairingSession, error)
	finish func(context.Context, *libgm.PairingSession) (string, error)
}

func (f fakeGoogleAccountPairer) StartGaiaPairing(ctx, bgCtx context.Context) (string, *libgm.PairingSession, error) {
	return f.start(ctx, bgCtx)
}

func (f fakeGoogleAccountPairer) FinishGaiaPairing(ctx context.Context, session *libgm.PairingSession) (string, error) {
	return f.finish(ctx, session)
}

func TestGoogleAccountPairingCancellationGates(t *testing.T) {
	for _, tc := range []struct {
		name                                    string
		wantStarts, wantFinishes, wantSnapshots int
	}{
		{"before_start", 0, 0, 0},
		{"startup_success", 1, 0, 0},
		{"finish_success", 1, 1, 0},
		{"session_snapshot", 1, 1, 1},
		{"not_canceled", 1, 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.name == "before_start" {
				cancel()
			}
			var starts, finishes, snapshots atomic.Int32
			pairing := &libgm.PairingSession{}
			pairer := fakeGoogleAccountPairer{
				start: func(callCtx, bgCtx context.Context) (string, *libgm.PairingSession, error) {
					starts.Add(1)
					if callCtx != ctx || bgCtx != ctx {
						t.Error("startup did not receive the cancellation context for both calls and long polling")
					}
					if tc.name == "startup_success" {
						cancel()
					}
					return "test-emoji", pairing, nil
				},
				finish: func(callCtx context.Context, got *libgm.PairingSession) (string, error) {
					finishes.Add(1)
					if callCtx != ctx || got != pairing {
						t.Error("finish did not receive the startup context and pairing session")
					}
					if tc.name == "finish_success" {
						cancel()
					}
					return "test-device", nil
				},
			}
			snapshot := func() (*client.SessionData, error) {
				snapshots.Add(1)
				if tc.name == "session_snapshot" {
					cancel()
				}
				return &client.SessionData{AuthDataJSON: json.RawMessage(`{"synthetic":true}`)}, nil
			}
			path := filepath.Join(t.TempDir(), "session.json")
			var output bytes.Buffer
			err := completeGoogleAccountPairing(ctx, pairer, snapshot, path, &output)
			if tc.name == "not_canceled" {
				if err != nil {
					t.Fatal(err)
				}
				if _, err := client.LoadSession(path); err != nil {
					t.Fatalf("successful pairing did not save a readable session: %v", err)
				}
				if !strings.Contains(output.String(), "Pairing successful!") {
					t.Errorf("successful pairing output missing: %s", &output)
				}
			} else {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("expected cancellation, got %v", err)
				}
				assertGooglePairingNotSaved(t, path, output.String())
			}
			if got := starts.Load(); got != int32(tc.wantStarts) {
				t.Errorf("startup calls = %d; want %d", got, tc.wantStarts)
			}
			if got := finishes.Load(); got != int32(tc.wantFinishes) {
				t.Errorf("finish calls = %d; want %d", got, tc.wantFinishes)
			}
			if got := snapshots.Load(); got != int32(tc.wantSnapshots) {
				t.Errorf("snapshot calls = %d; want %d", got, tc.wantSnapshots)
			}
		})
	}
}

func TestGoogleAccountPairingCancellationDoesNotWaitForStartup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan struct{})
	defer close(release)
	var finishes, snapshots atomic.Int32
	pairer := fakeGoogleAccountPairer{
		start: func(context.Context, context.Context) (string, *libgm.PairingSession, error) {
			close(started)
			<-release // Model upstream's non-context-aware initial long-poll wait.
			defer close(finished)
			return "late-emoji", &libgm.PairingSession{}, nil
		},
		finish: func(context.Context, *libgm.PairingSession) (string, error) {
			finishes.Add(1)
			return "unexpected-device", nil
		},
	}
	path := filepath.Join(t.TempDir(), "session.json")
	var output bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- completeGoogleAccountPairing(ctx, pairer, func() (*client.SessionData, error) {
			snapshots.Add(1)
			return &client.SessionData{}, nil
		}, path, &output)
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("startup did not begin")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected cancellation, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation remained blocked on startup")
	}
	assertGooglePairingNotSaved(t, path, output.String())
	if strings.Contains(output.String(), "EMOJI:") {
		t.Errorf("canceled startup displayed an emoji: %s", &output)
	}
	// Let the fake startup return after its result is no longer wanted. Its
	// result must not start the finish or session-save phases.
	release <- struct{}{}
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("late startup did not return")
	}
	if finishes.Load() != 0 || snapshots.Load() != 0 {
		t.Fatal("late startup continued into finish/session snapshot")
	}
	assertGooglePairingNotSaved(t, path, output.String())
}

func assertGooglePairingNotSaved(t *testing.T, path, output string) {
	t.Helper()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("canceled pairing created a session file: %v", err)
	}
	if strings.Contains(output, "Pairing successful!") || strings.Contains(output, "Session saved") {
		t.Errorf("canceled pairing reported success: %s", output)
	}
}
