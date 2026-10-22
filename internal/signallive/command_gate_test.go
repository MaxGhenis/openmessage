package signallive

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// The receive loop takes the gate again as soon as it lets go. A send that
// was waiting must get it first, or a polling loop could starve sends.
func TestCommandGateHandsOffToTheWaiterBeforeTheHolderRetakesIt(t *testing.T) {
	var gate commandGate
	gate.Lock()
	acquired := make(chan bool, 1)
	go func() { acquired <- gate.lockWithin(5 * time.Second) }()
	time.Sleep(250 * time.Millisecond) // let the waiter queue

	gate.Unlock()
	retaken := make(chan struct{})
	go func() {
		gate.Lock()
		close(retaken)
	}()
	if !<-acquired {
		t.Fatal("the waiting lockWithin gave up although the gate was released")
	}
	select {
	case <-retaken:
		t.Fatal("the holder retook the gate while the waiter held it")
	case <-time.After(50 * time.Millisecond):
	}
	gate.Unlock()
	<-retaken
	gate.Unlock()
}

func TestCommandGateLockWithinGivesUpAndLeavesTheGateUsable(t *testing.T) {
	var gate commandGate
	gate.Lock()
	started := time.Now()
	if gate.lockWithin(80 * time.Millisecond) {
		t.Fatal("lockWithin took a held gate")
	}
	if waited := time.Since(started); waited < 80*time.Millisecond {
		t.Fatalf("lockWithin gave up after %s, before its wait", waited)
	}
	gate.Unlock()
	if !gate.lockWithin(time.Millisecond) {
		t.Fatal("lockWithin could not take a free gate")
	}
	gate.Unlock()
}

func TestCommandGateUnlockOfAnUnlockedGatePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("Unlock of an unlocked gate did not panic")
		}
	}()
	var gate commandGate
	gate.Unlock()
}

const gateTestRecipient = "+15557654321"

// sendThroughGate runs each send path the dispatcher and the web API use.
func sendThroughGate(bridge *Bridge) map[string]error {
	_, textErr := bridge.SendTextRequest("signal:"+gateTestRecipient, "hi", ReplyTarget{})
	_, mediaErr := bridge.SendMediaRequest("signal:"+gateTestRecipient, bytes.NewReader([]byte("jpeg")), 4, "a.jpg", "image/jpeg", "", ReplyTarget{})
	// An own confirmed send: its remote ID is the Signal timestamp.
	reactionErr := bridge.SendReactionRequest("signal:"+gateTestRecipient, ReactionTarget{
		RemoteID: "1700000000555", Outgoing: true, SentAt: time.UnixMilli(1700000000000),
	}, "👍", "add")
	_, legacyTextErr := bridge.SendText("signal:"+gateTestRecipient, "hi", "")
	// Quoting an author known only by an ACI the contact cache lacks makes
	// the send refresh contacts first; that refresh must give up too.
	_, quoteErr := bridge.SendTextRequest("signal:"+gateTestRecipient, "re", ReplyTarget{
		RemoteID: "1700000000999", AuthorID: "8a3c1f2e-5b6d-4e7f-9a0b-1c2d3e4f5a6b", SentAt: time.UnixMilli(1700000000999), Text: "orig",
	})
	return map[string]error{
		"text": textErr, "media": mediaErr, "reaction": reactionErr,
		"legacy text": legacyTextErr, "quoted text": quoteErr,
	}
}

func newGateTestBridge(t *testing.T, run func(ctx context.Context, args ...string) ([]byte, error)) *Bridge {
	t.Helper()
	original := runSignalCLI
	t.Cleanup(func() { runSignalCLI = original })
	runSignalCLI = func(ctx context.Context, _ string, args ...string) ([]byte, error) {
		return run(ctx, args...)
	}
	return &Bridge{account: "+15550000000", connected: true, configDir: t.TempDir(), logger: zerolog.Nop()}
}

// While a receive call is drained to finish a download, which can take
// minutes, a send must fail at once as not dispatched: the dispatcher sends
// every platform's messages one at a time and retries a not-dispatched
// send. Waiting would stall it; failing as a command error would mark a
// message that never reached signal-cli uncertain.
func TestSendsGiveUpAsNotDispatchedWhileAReceiveIsDrained(t *testing.T) {
	var calls atomic.Int32
	bridge := newGateTestBridge(t, func(context.Context, ...string) ([]byte, error) {
		calls.Add(1)
		return nil, errors.New("signal-cli must not run")
	})
	bridge.commandMu.Lock() // the drained receive call
	defer bridge.commandMu.Unlock()
	bridge.receiveDraining.Store(true)

	started := time.Now()
	for path, err := range sendThroughGate(bridge) {
		if !IsSendNotDispatchedError(err) || !IsCommandError(err) || !strings.Contains(err.Error(), "attachment download") {
			t.Errorf("%s send = %v, want a not-dispatched command error naming the drain", path, err)
		}
	}
	if waited := time.Since(started); waited > time.Second {
		t.Fatalf("sends took %s to give up behind a drain, want at once", waited)
	}
	if calls.Load() != 0 {
		t.Fatalf("signal-cli ran %d times behind a drain", calls.Load())
	}
	if _, err := bridge.downloadSignalAttachment("+15550000000", "abc.jpg", gateTestRecipient, false); err == nil || !strings.Contains(err.Error(), "attachment download") {
		t.Fatalf("attachment fetch behind a drain = %v, want it to give up", err)
	}
}

func TestSendsGiveUpAsNotDispatchedWhenTheGateStaysTaken(t *testing.T) {
	original := requestLockWait
	requestLockWait = 60 * time.Millisecond
	t.Cleanup(func() { requestLockWait = original })
	bridge := newGateTestBridge(t, func(context.Context, ...string) ([]byte, error) {
		return nil, errors.New("signal-cli must not run")
	})
	bridge.commandMu.Lock() // a stuck call
	defer bridge.commandMu.Unlock()

	for path, err := range sendThroughGate(bridge) {
		if !IsSendNotDispatchedError(err) || !strings.Contains(err.Error(), "stayed busy") {
			t.Errorf("%s send = %v, want not dispatched after requestLockWait", path, err)
		}
	}
}

// A send's own timeout starts once it holds the gate, so time spent waiting
// behind a receive call can't leave it too little to finish (a send cut off
// mid-run is uncertain).
func TestSendTimeoutStartsOnceTheSendHoldsTheGate(t *testing.T) {
	var budget atomic.Int64
	bridge := newGateTestBridge(t, func(ctx context.Context, _ ...string) ([]byte, error) {
		deadline, _ := ctx.Deadline()
		budget.Store(int64(time.Until(deadline)))
		return []byte(`{"timestamp":1700000000123,"results":[{"recipientAddress":{"number":"` + gateTestRecipient + `"},"type":"SUCCESS"}]}`), nil
	})
	bridge.commandMu.Lock()
	released := make(chan struct{})
	go func() {
		time.Sleep(400 * time.Millisecond)
		bridge.commandMu.Unlock()
		close(released)
	}()
	if _, err := bridge.SendTextRequest("signal:"+gateTestRecipient, "hi", ReplyTarget{}); err != nil {
		t.Fatalf("SendTextRequest(): %v", err)
	}
	<-released
	if got := time.Duration(budget.Load()); got < sendTimeout-200*time.Millisecond {
		t.Fatalf("send ran with %s of its %s timeout left after waiting for the gate", got, sendTimeout)
	}
}
