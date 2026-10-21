package signallive

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/quick"
	"time"

	"github.com/rs/zerolog"

	"github.com/maxghenis/openmessage/internal/db"
)

const failedReceiveAccount = "+15551230000"

// signalCLIShutdownLine is what signal-cli prints when the receive deadline
// fires and configureSignalCancel sends SIGTERM (captured live 2026-10-21).
const signalCLIShutdownLine = "INFO  Shutdown - Received SIGTERM signal, shutting down ..."

var errSignalCLIExit = errors.New("exit status 1")

func failedReceiveEnvelope(sender string, ts int64, body string) string {
	return fmt.Sprintf(
		`{"account":%q,"envelope":{"source":%q,"sourceName":"Taylor","timestamp":%d,"dataMessage":{"timestamp":%d,"message":%q}}}`,
		failedReceiveAccount, sender, ts, ts, body,
	)
}

// newFailedReceiveBridge wires a bridge to a real store and swaps runSignalCLI
// for receive, which answers the receive calls the loop makes.
func newFailedReceiveBridge(t *testing.T, receive func(call int32) ([]byte, error)) (*Bridge, *db.Store, *atomic.Int32) {
	t.Helper()
	store, err := db.New(filepath.Join(t.TempDir(), "messages.db"))
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	bridge := &Bridge{store: store, logger: zerolog.Nop(), configDir: t.TempDir()}

	originalRun := runSignalCLI
	originalVersionProbe := probeSignalCLIVersion
	t.Cleanup(func() {
		_ = bridge.Close()
		bridge.commandMu.Lock()
		runSignalCLI = originalRun
		bridge.commandMu.Unlock()
		probeSignalCLIVersion = originalVersionProbe
		store.Close()
	})
	probeSignalCLIVersion = func(context.Context) ([]byte, error) {
		return []byte("signal-cli 0.14.9"), nil
	}

	var calls atomic.Int32
	runSignalCLI = func(ctx context.Context, configDir string, args ...string) ([]byte, error) {
		joined := " " + strings.Join(args, " ") + " "
		switch {
		case strings.Contains(joined, " listAccounts "):
			return []byte(`[{"number":"` + failedReceiveAccount + `"}]`), nil
		case strings.Contains(joined, " receive "):
			return receive(calls.Add(1))
		}
		return []byte{}, nil
	}
	return bridge, store, &calls
}

func signalBodies(t *testing.T, store *db.Store, sender string) []string {
	t.Helper()
	msgs, err := store.GetMessagesByConversation("signal:"+sender, 100)
	if err != nil {
		t.Fatalf("GetMessagesByConversation(): %v", err)
	}
	bodies := make([]string, 0, len(msgs))
	for _, msg := range msgs {
		bodies = append(bodies, msg.Body)
	}
	slices.Sort(bodies)
	return bodies
}

// A receive call that runs past its deadline is killed after signal-cli has
// already printed, and acknowledged to Signal's server, the envelopes it
// received. Those envelopes must be kept: the server will never deliver them
// to this device again.
func TestStartReceiveLoopKeepsEnvelopesFromTimedOutReceive(t *testing.T) {
	const sender = "+15551234567"
	envelope := failedReceiveEnvelope(sender, 1700000000123, "sent just before the deadline")
	bridge, store, calls := newFailedReceiveBridge(t, func(call int32) ([]byte, error) {
		if call == 1 {
			return []byte(envelope + "\n" + signalCLIShutdownLine + "\n"), context.DeadlineExceeded
		}
		return []byte{}, context.DeadlineExceeded
	})

	var observed atomic.Int32
	unregister := bridge.ObserveIngress(func(string, []byte, string, string) { observed.Add(1) })
	defer unregister()

	go bridge.startReceiveLoop(failedReceiveAccount, false)
	waitForCondition(t, 2*time.Second, func() bool { return calls.Load() >= 3 })

	if got := signalBodies(t, store, sender); !slices.Equal(got, []string{"sent just before the deadline"}) {
		t.Fatalf("stored bodies = %q, want the envelope printed before the deadline", got)
	}
	if observed.Load() != 1 {
		t.Fatalf("ingress observer saw %d envelopes, want 1", observed.Load())
	}
	if status := bridge.Status(); !status.Connected || status.LastError != "" {
		t.Fatalf("a deadline hit while delivering messages is not a failure; status = %+v", status)
	}
}

// A non-timeout failure (signal-cli exits non-zero) still delivers whatever it
// printed first, and still counts toward the reconnect limit.
func TestStartReceiveLoopKeepsEnvelopesFromFailedReceiveAndStillCountsFailure(t *testing.T) {
	const sender = "+15557654321"
	bridge, store, _ := newFailedReceiveBridge(t, func(call int32) ([]byte, error) {
		body := fmt.Sprintf("printed before failure %d", call)
		out := failedReceiveEnvelope(sender, 1700000000000+int64(call), body) +
			"\nERROR App - Unexpected error: connection reset\n"
		return []byte(out), errSignalCLIExit
	})

	go bridge.startReceiveLoop(failedReceiveAccount, false)
	waitForCondition(t, 2*time.Second, func() bool {
		status := bridge.Status()
		return !status.Connected && !status.Connecting && strings.Contains(status.LastError, "connection reset")
	})

	want := make([]string, 0, receiveFailureLimit)
	for call := 1; call <= receiveFailureLimit; call++ {
		want = append(want, fmt.Sprintf("printed before failure %d", call))
	}
	slices.Sort(want)
	if got := signalBodies(t, store, sender); !slices.Equal(got, want) {
		t.Fatalf("stored bodies = %q, want every envelope printed by the %d failed calls", got, receiveFailureLimit)
	}
}

// receiveCallScript is one generated sequence of receive results: each call
// prints some envelopes and log noise, then ends in one of the outcomes the
// loop distinguishes.
type receiveCallScript struct {
	Calls []scriptedReceiveCall
}

type scriptedReceiveCall struct {
	Bodies  []string
	Noise   bool
	Outcome int // 0 success, 1 deadline exceeded, 2 non-zero exit
}

func (receiveCallScript) Generate(r *rand.Rand, _ int) reflect.Value {
	calls := make([]scriptedReceiveCall, 1+r.Intn(8))
	next := 0
	for i := range calls {
		bodies := make([]string, r.Intn(3))
		for j := range bodies {
			bodies[j] = fmt.Sprintf("message %03d", next)
			next++
		}
		calls[i] = scriptedReceiveCall{Bodies: bodies, Noise: r.Intn(2) == 0, Outcome: r.Intn(3)}
	}
	return reflect.ValueOf(receiveCallScript{Calls: calls})
}

// Invariant: every envelope signal-cli prints in a receive call the loop
// makes is stored exactly once, whatever the call's exit status. Calls the
// loop never makes (it reconnects after receiveFailureLimit consecutive
// failures) are excluded, since their envelopes were never printed.
func TestStartReceiveLoopStoresEveryPrintedEnvelopeProperty(t *testing.T) {
	const sender = "+15550001111"
	property := func(script receiveCallScript) bool {
		t.Run("", func(t *testing.T) {
			var mu sync.Mutex
			var printed []string
			done := make(chan struct{})
			var doneOnce sync.Once
			bridge, store, _ := newFailedReceiveBridge(t, func(call int32) ([]byte, error) {
				index := int(call) - 1
				if index >= len(script.Calls) {
					doneOnce.Do(func() { close(done) })
					return []byte{}, context.DeadlineExceeded
				}
				scripted := script.Calls[index]
				var lines []string
				for i, body := range scripted.Bodies {
					lines = append(lines, failedReceiveEnvelope(sender, 1700000000000+int64(index*10+i), body))
					mu.Lock()
					printed = append(printed, body)
					mu.Unlock()
				}
				if scripted.Noise {
					lines = append(lines, "WARN  AccountHelper - Messages have been last received 8 days ago.")
				}
				out := []byte(strings.Join(lines, "\n"))
				switch scripted.Outcome {
				case 1:
					return append(out, []byte("\n"+signalCLIShutdownLine)...), context.DeadlineExceeded
				case 2:
					return append(out, []byte("\nERROR App - Unexpected error")...), errSignalCLIExit
				}
				return out, nil
			})
			go bridge.startReceiveLoop(failedReceiveAccount, false)
			// The loop either runs past the end of the script or gives up
			// after receiveFailureLimit consecutive failures and disconnects.
			waitForCondition(t, 5*time.Second, func() bool {
				select {
				case <-done:
					return true
				default:
				}
				status := bridge.Status()
				return !status.Connected && !status.Connecting && status.LastError != ""
			})
			mu.Lock()
			want := slices.Clone(printed)
			mu.Unlock()
			slices.Sort(want)
			if got := signalBodies(t, store, sender); !slices.Equal(got, want) {
				t.Errorf("stored %q, want every printed envelope %q (script %+v)", got, want, script.Calls)
			}
		})
		return !t.Failed()
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 15}); err != nil {
		t.Fatal(err)
	}
}
