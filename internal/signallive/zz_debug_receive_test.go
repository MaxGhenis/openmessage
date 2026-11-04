package signallive

// TEMPORARY timing probe; not for commit.

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestZZDebugReceiveScriptTiming(t *testing.T) {
	if os.Getenv("ZZ_DEBUG_RECV") == "" {
		t.Skip("probe")
	}
	const sender = "+15550001111"
	script := []scriptedReceiveCall{
		{Bodies: nil, Noise: true, Outcome: 0},
		{Bodies: nil, Noise: true, Outcome: 1},
		{Bodies: []string{"message 000", "message 001"}, Noise: true, Outcome: 0},
		{Bodies: nil, Noise: true, Outcome: 2},
		{Bodies: []string{"message 002"}, Noise: true, Outcome: 1},
		{Bodies: []string{"message 003"}, Noise: true, Outcome: 1},
	}
	var mu sync.Mutex
	var marks []string
	start := time.Now()
	mark := func(s string) {
		mu.Lock()
		marks = append(marks, fmt.Sprintf("%7.3fs %s", time.Since(start).Seconds(), s))
		mu.Unlock()
	}
	done := make(chan struct{})
	var once sync.Once
	bridge, _, _ := newFailedReceiveBridge(t, func(ctx context.Context, call int32) ([]byte, error) {
		index := int(call) - 1
		mark(fmt.Sprintf("receive call %d", call))
		if index >= len(script) {
			once.Do(func() { close(done) })
			return []byte{}, nil
		}
		s := script[index]
		var lines []string
		for i, body := range s.Bodies {
			lines = append(lines, failedReceiveEnvelope(sender, 1700000000000+int64(index*10+i), body))
		}
		if s.Noise {
			lines = append(lines, "WARN  AccountHelper - Messages have been last received 8 days ago.")
		}
		out := strings.Join(lines, "\n")
		switch s.Outcome {
		case 1:
			return runIntoDeadline(ctx, out)
		case 2:
			return []byte(out + "\nERROR App - Unexpected error"), errSignalCLIExit
		}
		return []byte(out), nil
	})
	original := runSignalCLI
	runSignalCLI = func(ctx context.Context, configDir string, args ...string) ([]byte, error) {
		mark("runSignalCLI " + strings.Join(args, " "))
		return original(ctx, configDir, args...)
	}
	mark("start loop")
	go bridge.startReceiveLoop(failedReceiveAccount, false)
	select {
	case <-done:
		mark("done")
	case <-time.After(60 * time.Second):
		mark("gave up")
	}
	mu.Lock()
	t.Logf("ZZRECV\n%s", strings.Join(marks, "\n"))
	mu.Unlock()
}
