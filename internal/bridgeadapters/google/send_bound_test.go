package google

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"testing"
	"testing/quick"
	"time"

	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"

	"github.com/maxghenis/openmessage/internal/bridge"
)

// dispatcherLeaseTime mirrors messaging's defaultLeaseTime (service.go), the
// lease every outbox attempt runs under.
const dispatcherLeaseTime = 30 * time.Second

// leaseContentionMargin is the time a lease may lose between being granted
// and its transport call starting (a SQLite writer holding the database, as
// the PR #204 round-2 review measured at 4 s), which a worst-case attempt
// must still fit around.
const leaseContentionMargin = 7 * time.Second

// Design A5: a send attempt's libgm calls end within the dispatcher lease,
// even after the lease lost leaseContentionMargin before the call started.
func TestSendCallBoundsStayBelowDispatcherLease(t *testing.T) {
	if worst := googleSendAttemptTimeout + googleSendMinimumTimeout; worst+leaseContentionMargin > dispatcherLeaseTime {
		t.Fatalf("attempt budget %s + send minimum %s = %s, want it to fit the %s lease with %s to spare",
			googleSendAttemptTimeout, googleSendMinimumTimeout, worst, dispatcherLeaseTime, leaseContentionMargin)
	}
	if googleLookupTimeout <= 0 || googleSendMinimumTimeout <= 0 {
		t.Fatalf("bounds must be positive: lookup %s, send minimum %s", googleLookupTimeout, googleSendMinimumTimeout)
	}
	if googleLookupTimeout > googleSendAttemptTimeout-googleSendMinimumTimeout {
		t.Fatalf("lookup cap %s exceeds what the budget leaves before the send window", googleLookupTimeout)
	}
}

// Property (testing/quick) over any sequence of call durations, including
// calls that run into their deadline: one attempt (up to two lookups, the
// upload, the send and a caption) never waits longer than the attempt budget
// plus the send minimum, the pre-send calls never eat into the window kept
// for the send, and the send is never started with less than the minimum.
func TestCallBudgetBoundsEveryAttempt(t *testing.T) {
	config := &quick.Config{MaxCount: 5000, Rand: rand.New(rand.NewSource(11))}
	worst := googleSendAttemptTimeout + googleSendMinimumTimeout
	property := func(fractions [5]uint16, fallback, upload, caption bool, ctxMS uint16) bool {
		start := time.Unix(1_700_000_000, 0)
		clock := start
		ctx := context.Background()
		// Sometimes the caller's own deadline is the binding one.
		if ctxMS%3 == 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithDeadline(ctx, start.Add(time.Duration(ctxMS)*time.Millisecond))
			defer cancel()
		}
		budget := newCallBudgetAt(ctx, func() time.Time { return clock })
		// spend advances the clock by up to 1.5x limit and reports whether
		// the call answered before its deadline.
		spend := func(limit time.Duration, fraction uint16) bool {
			took := time.Duration(float64(limit) * 1.5 * float64(fraction) / float64(1<<16))
			if took >= limit {
				clock = clock.Add(limit)
				return false
			}
			clock = clock.Add(took)
			return true
		}

		capacities := []time.Duration{googleLookupTimeout} // GetConversation
		if fallback {
			capacities = append(capacities, googleLookupTimeout) // GetOrCreateConversation
		}
		if upload {
			capacities = append(capacities, 0) // UploadMedia: budget only
		}
		for index, capacity := range capacities {
			limit := budget.preSendLimit(capacity)
			if capacity > 0 && limit > capacity {
				return false
			}
			if limit <= 0 || !spend(limit, fractions[index]) {
				// Not started or unanswered: the attempt ends here.
				return clock.Sub(start) <= googleSendAttemptTimeout
			}
			// An answered pre-send call leaves the send window intact.
			if budget.deadline.Sub(clock) < googleSendMinimumTimeout {
				return false
			}
		}
		send := budget.sendLimit()
		if send < googleSendMinimumTimeout {
			return false
		}
		if !spend(send, fractions[3]) || !caption {
			return clock.Sub(start) <= worst
		}
		captionLimit := budget.sendLimit()
		if captionLimit < googleSendMinimumTimeout {
			return false
		}
		spend(captionLimit, fractions[4])
		return clock.Sub(start) <= worst
	}
	if err := quick.Check(property, config); err != nil {
		t.Fatalf("call budget property failed: %v", err)
	}
}

func TestBoundedCallReturnsResultTimeoutOrNotStarted(t *testing.T) {
	t.Run("answered", func(t *testing.T) {
		value, err := boundedCall(context.Background(), time.Minute, func() (string, error) {
			return "answer", nil
		})
		if value != "answer" || err != nil {
			t.Fatalf("boundedCall() = (%q, %v), want (answer, nil)", value, err)
		}
	})

	t.Run("call error", func(t *testing.T) {
		want := errors.New("transport failed")
		if _, err := boundedCall(context.Background(), time.Minute, func() (int, error) {
			return 0, want
		}); !errors.Is(err, want) {
			t.Fatalf("boundedCall() error = %v, want %v", err, want)
		}
	})

	t.Run("call error wrapping its own deadline", func(t *testing.T) {
		// An HTTP timeout inside libgm is a transport failure, not the
		// caller's context ending.
		httpTimeout := fmt.Errorf("finalize upload: %w", context.DeadlineExceeded)
		_, err := boundedCall(context.Background(), time.Minute, func() (int, error) {
			return 0, httpTimeout
		})
		if !errors.Is(err, httpTimeout) {
			t.Fatalf("boundedCall() error = %v, want the call's own error", err)
		}
		if _, unanswered := unansweredCallFailure(err, "send_media", "", "timeout", "context", bridge.DispatchNotCalled); unanswered {
			t.Fatal("a libgm deadline error was classified as an unanswered call")
		}
	})

	t.Run("panic becomes an error", func(t *testing.T) {
		_, err := boundedCall(context.Background(), time.Minute, func() (int, error) {
			panic("libgm exploded")
		})
		if err == nil || !strings.Contains(err.Error(), "libgm exploded") {
			t.Fatalf("boundedCall() error = %v, want the panic reported", err)
		}
		if _, unanswered := unansweredCallFailure(err, "send_text", "", "timeout", "context", bridge.DispatchUncertain); unanswered {
			t.Fatal("a panic was classified as an unanswered call; it must take the transport-error path")
		}
	})

	t.Run("no budget left", func(t *testing.T) {
		called := false
		_, err := boundedCall(context.Background(), 0, func() (int, error) {
			called = true
			return 0, nil
		})
		if called || !errors.Is(err, errCallNotStarted) {
			t.Fatalf("called = %v, error = %v; want not started", called, err)
		}
		// A request that never left is not dispatched even where an
		// unanswered one would be uncertain.
		failure, unanswered := unansweredCallFailure(err, "send_text", "send Google text", "google_text_send_timeout",
			"google_text_context_done", bridge.DispatchUncertain)
		if !unanswered || failure.Dispatch != bridge.DispatchNotCalled || failure.Fingerprint != "google_text_send_timeout" {
			t.Fatalf("failure = %+v, want not-dispatched google_text_send_timeout", failure)
		}
	})

	t.Run("context already ended", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		called := false
		_, err := boundedCall(ctx, time.Minute, func() (int, error) {
			called = true
			return 0, nil
		})
		if called || !errors.Is(err, errCallNotStarted) || !errors.Is(err, context.Canceled) {
			t.Fatalf("called = %v, error = %v; want not started because ctx ended", called, err)
		}
		failure, unanswered := unansweredCallFailure(err, "send_text", "send Google text", "google_text_send_timeout",
			"google_text_context_done", bridge.DispatchUncertain)
		if !unanswered || failure.Dispatch != bridge.DispatchNotCalled || failure.Fingerprint != "google_text_context_done" {
			t.Fatalf("failure = %+v, want not-dispatched google_text_context_done", failure)
		}
	})

	t.Run("deadline", func(t *testing.T) {
		release := make(chan struct{})
		defer close(release)
		started := time.Now()
		_, err := boundedCall(context.Background(), 50*time.Millisecond, func() (int, error) {
			<-release
			return 1, nil
		})
		var timeout *transportCallTimeoutError
		if !errors.As(err, &timeout) || timeout.waited != 50*time.Millisecond {
			t.Fatalf("boundedCall() error = %v, want a 50ms timeout", err)
		}
		if elapsed := time.Since(started); elapsed > 5*time.Second {
			t.Fatalf("boundedCall waited %s for a 50ms limit", elapsed)
		}
	})
}

// useShortCallBounds shrinks the bounds so tests can run into them.
func useShortCallBounds(t *testing.T) {
	t.Helper()
	attempt, minimum, lookup := googleSendAttemptTimeout, googleSendMinimumTimeout, googleLookupTimeout
	googleSendAttemptTimeout = 600 * time.Millisecond
	googleSendMinimumTimeout = 150 * time.Millisecond
	googleLookupTimeout = 200 * time.Millisecond
	t.Cleanup(func() {
		googleSendAttemptTimeout, googleSendMinimumTimeout, googleLookupTimeout = attempt, minimum, lookup
	})
}

// boundedCallSlack is how long past its bound a call may take to return on a
// loaded test machine.
const boundedCallSlack = 5 * time.Second

// Design A5 and invariant I3: whichever libgm request goes unanswered, the
// adapter stops waiting at its bound, reports not dispatched for requests
// that send nothing (lookups, the upload, an idempotent read receipt) and
// uncertain for requests the phone may have acted on (the send, the
// reaction), never sends after an unanswered lookup, and never touches the
// receive generation. A late answer changes nothing.
func TestUnansweredLibgmRequestsAreBounded(t *testing.T) {
	tests := []struct {
		op          string
		blockOn     string
		blockSend   int
		ref         bridge.ConversationRef
		fingerprint string
		dispatch    bridge.DispatchCertainty
		wantCalls   []string
	}{
		{
			op: "text", blockOn: "get_conversation", ref: directTextRequest().Conversation,
			fingerprint: fingerprintConversationGetTimeout, dispatch: bridge.DispatchNotCalled,
			wantCalls: []string{"get_conversation"},
		},
		{
			op: "text", blockOn: "get_or_create", ref: directTextRequest().Conversation,
			fingerprint: fingerprintConversationGetTimeout, dispatch: bridge.DispatchNotCalled,
			wantCalls: []string{"get_conversation", "get_or_create"},
		},
		{
			op: "text", blockOn: "send", ref: directTextRequest().Conversation,
			fingerprint: "google_text_send_timeout", dispatch: bridge.DispatchUncertain,
			wantCalls: []string{"get_conversation", "send"},
		},
		{
			op: "media", blockOn: "get_conversation", ref: directTextRequest().Conversation,
			fingerprint: fingerprintConversationGetTimeout, dispatch: bridge.DispatchNotCalled,
			wantCalls: []string{"get_conversation"},
		},
		{
			op: "media", blockOn: "upload", ref: directTextRequest().Conversation,
			fingerprint: "google_media_upload_timeout", dispatch: bridge.DispatchNotCalled,
			wantCalls: []string{"get_conversation", "upload"},
		},
		{
			op: "media", blockOn: "send", ref: directTextRequest().Conversation,
			fingerprint: "google_media_send_timeout", dispatch: bridge.DispatchUncertain,
			wantCalls: []string{"get_conversation", "upload", "send"},
		},
		{
			// The media already went out, so the caption stays ambiguous.
			op: "media", blockOn: "send", blockSend: 1, ref: directTextRequest().Conversation,
			fingerprint: "google_caption_send_timeout", dispatch: "",
			wantCalls: []string{"get_conversation", "upload", "send", "send"},
		},
		{
			op: "reaction", blockOn: "get_conversation", ref: directTextRequest().Conversation,
			fingerprint: fingerprintConversationGetTimeout, dispatch: bridge.DispatchNotCalled,
			wantCalls: []string{"get_conversation"},
		},
		{
			op: "reaction", blockOn: "send_reaction", ref: directTextRequest().Conversation,
			fingerprint: "google_reaction_send_timeout", dispatch: bridge.DispatchUncertain,
			wantCalls: []string{"get_conversation", "send_reaction"},
		},
		{
			op: "read", blockOn: "mark_read", ref: directTextRequest().Conversation,
			fingerprint: "google_mark_read_timeout", dispatch: bridge.DispatchNotCalled,
			wantCalls: []string{"mark_read"},
		},
	}
	for _, test := range tests {
		name := test.op + "/" + test.blockOn
		if test.blockSend > 0 {
			name += "/caption"
		}
		t.Run(name, func(t *testing.T) {
			useShortCallBounds(t)
			h := startSendRun(t, newLegacyClient)
			fake := newBlockingSendTransport(t, test.blockOn, test.blockSend)
			if test.blockOn == "get_or_create" {
				fake.conversation = nil
			}
			started := time.Now()
			err := fake.run(t, h, test.op, context.Background(), test.ref)
			elapsed := time.Since(started)

			failure, ok := asOpError(err)
			if !ok {
				t.Fatalf("error = %T %v, want bridge.OpError", err, err)
			}
			if failure.Class != bridge.FailureTransient || failure.Fingerprint != test.fingerprint ||
				failure.Dispatch != test.dispatch {
				t.Fatalf("failure = %+v, want transient %s dispatch %q", failure, test.fingerprint, test.dispatch)
			}
			var timeout *transportCallTimeoutError
			if !errors.As(failure.Cause, &timeout) || !strings.Contains(err.Error(), "did not answer within") {
				t.Fatalf("cause = %v, want the call timeout", failure.Cause)
			}
			if limit := googleSendAttemptTimeout + googleSendMinimumTimeout + boundedCallSlack; elapsed > limit {
				t.Fatalf("unanswered %s held the caller %s, want at most %s", test.blockOn, elapsed, limit)
			}
			select {
			case <-fake.entered:
			default:
				t.Fatalf("%s was never called", test.blockOn)
			}
			h.assertGenerationIntact(t, "unanswered "+test.blockOn)

			// The late answer is discarded: nothing else is called.
			fake.unblock()
			<-fake.returned
			if got := fake.callLog(); strings.Join(got, ",") != strings.Join(test.wantCalls, ",") {
				t.Fatalf("calls = %v, want %v", got, test.wantCalls)
			}
			if h.host.GoogleStatus().AccountSwitched {
				t.Fatal("an unanswered request raised the account switch")
			}
		})
	}
}

// A phone in Google-account pairing answers with its account notice before
// (or instead of) a real reply: the switch raised while the lookup is
// outstanding turns the unanswered lookup into the terminal refusal, as for
// any non-auth lookup failure (design A2 step 1).
func TestUnansweredLookupAfterAccountSwitchIsRefused(t *testing.T) {
	useShortCallBounds(t)
	h := startSendRun(t, newLegacyClient)
	fake := newBlockingSendTransport(t, "get_conversation", 0)
	fake.beforeBlock = h.emitAccountChange
	err := fake.run(t, h, "text", context.Background(), directTextRequest().Conversation)
	failure, ok := asOpError(err)
	if !ok {
		t.Fatalf("error = %T %v, want bridge.OpError", err, err)
	}
	requireAccountSwitchRefusal(t, failure, "send_text")
	h.assertGenerationIntact(t, "refused unanswered lookup")
	fake.unblock()
	<-fake.returned
	if calls := fake.callLog(); len(calls) != 1 {
		t.Fatalf("calls = %v, want only the lookup", calls)
	}
}

// The caller's context ends while a request is outstanding (daemon shutdown):
// the adapter returns at once, not dispatched for a lookup and uncertain for
// the send.
func TestContextEndingDuringUnansweredRequest(t *testing.T) {
	for _, test := range []struct {
		blockOn     string
		fingerprint string
		dispatch    bridge.DispatchCertainty
	}{
		{blockOn: "get_conversation", fingerprint: "google_text_context_done", dispatch: bridge.DispatchNotCalled},
		{blockOn: "send", fingerprint: "google_text_context_done", dispatch: bridge.DispatchUncertain},
	} {
		t.Run(test.blockOn, func(t *testing.T) {
			h := startSendRun(t, newLegacyClient)
			fake := newBlockingSendTransport(t, test.blockOn, 0)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			fake.beforeBlock = cancel
			started := time.Now()
			err := fake.run(t, h, "text", ctx, directTextRequest().Conversation)
			// Ignoring ctx would hold the caller until a call bound instead.
			if elapsed := time.Since(started); elapsed >= googleLookupTimeout {
				t.Fatalf("cancellation took %s to end the send", elapsed)
			}
			failure, ok := asOpError(err)
			if !ok || failure.Fingerprint != test.fingerprint || failure.Dispatch != test.dispatch ||
				!errors.Is(failure.Cause, context.Canceled) {
				t.Fatalf("error = %v (%+v), want %s dispatch %q from the canceled context",
					err, failure, test.fingerprint, test.dispatch)
			}
			h.assertGenerationIntact(t, "canceled "+test.blockOn)
		})
	}
}

// blockingSendTransport implements every send seam. The call named blockOn
// (for "send", the blockSend-th SendMessage) waits until unblock, the way the
// pinned libgm waits for a phone that never answers; every other call
// answers at once with a usable result.
type blockingSendTransport struct {
	blockOn     string
	blockSend   int
	beforeBlock func()

	conversation *gmproto.Conversation
	resolved     *gmproto.GetOrCreateConversationResponse

	release  chan struct{}
	entered  chan struct{}
	returned chan struct{}
	once     sync.Once

	mu    sync.Mutex
	calls []string
	sends int
}

func newBlockingSendTransport(t *testing.T, blockOn string, blockSend int) *blockingSendTransport {
	t.Helper()
	f := &blockingSendTransport{
		blockOn:      blockOn,
		blockSend:    blockSend,
		conversation: directConversation(testRemoteID, testPeerNumber),
		resolved: &gmproto.GetOrCreateConversationResponse{
			Conversation: directConversation(testRemoteID, testPeerNumber),
		},
		release:  make(chan struct{}),
		entered:  make(chan struct{}),
		returned: make(chan struct{}),
	}
	// Never leave the blocked goroutine behind the test.
	t.Cleanup(f.unblock)
	return f
}

func (f *blockingSendTransport) unblock() { f.once.Do(func() { close(f.release) }) }

func (f *blockingSendTransport) callLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *blockingSendTransport) wait(call string) {
	f.mu.Lock()
	f.calls = append(f.calls, call)
	block := call == f.blockOn
	if call == "send" {
		block = block && f.sends == f.blockSend
		f.sends++
	}
	f.mu.Unlock()
	if !block {
		return
	}
	if f.beforeBlock != nil {
		f.beforeBlock()
	}
	close(f.entered)
	<-f.release
	close(f.returned)
}

func (f *blockingSendTransport) GetConversation(string) (*gmproto.Conversation, error) {
	f.wait("get_conversation")
	return f.conversation, nil
}

func (f *blockingSendTransport) GetOrCreateConversation(
	*gmproto.GetOrCreateConversationRequest,
) (*gmproto.GetOrCreateConversationResponse, error) {
	f.wait("get_or_create")
	return f.resolved, nil
}

func (f *blockingSendTransport) UploadMedia([]byte, string, string) (*gmproto.MediaContent, error) {
	f.wait("upload")
	return &gmproto.MediaContent{MediaID: "media-id"}, nil
}

func (f *blockingSendTransport) SendMessage(*gmproto.SendMessageRequest) (*gmproto.SendMessageResponse, error) {
	f.wait("send")
	return &gmproto.SendMessageResponse{Status: gmproto.SendMessageResponse_SUCCESS}, nil
}

func (f *blockingSendTransport) SendReaction(*gmproto.SendReactionRequest) (*gmproto.SendReactionResponse, error) {
	f.wait("send_reaction")
	return &gmproto.SendReactionResponse{Success: true}, nil
}

func (f *blockingSendTransport) MarkRead(string, string) error {
	f.wait("mark_read")
	return nil
}

// run installs f as the op's transport on h's live generation and performs
// one send of that kind.
func (f *blockingSendTransport) run(
	t *testing.T,
	h *sendRunHarness,
	op string,
	ctx context.Context,
	ref bridge.ConversationRef,
) error {
	t.Helper()
	cli := h.host.GetClient()
	var err error
	switch op {
	case "text":
		installTextSendClient(t, cli, f)
		_, err = h.adapter.SendText(ctx, bridge.TextRequest{Conversation: ref, Body: "hello", RequestID: "request-id"})
	case "media":
		installMediaSendClient(t, cli, f)
		_, err = h.adapter.SendMedia(ctx, bridge.MediaRequest{
			Conversation: ref,
			Reader:       strings.NewReader("x"),
			Size:         1,
			Caption:      "caption",
			RequestID:    "request-id",
		})
	case "reaction":
		installReactionSendClient(t, cli, f)
		_, err = h.adapter.SendReaction(ctx, bridge.ReactionRequest{
			Conversation: ref,
			Target:       bridge.MessageRef{RemoteID: "target-id"},
			Emoji:        "👍",
			Action:       bridge.ReactionAdd,
		})
	case "read":
		installReadSendClient(t, cli, f)
		err = h.adapter.MarkRead(ctx, bridge.ReadReceiptRequest{
			Conversation: ref,
			Messages:     []bridge.MessageRef{{RemoteID: "message-id"}},
		})
	default:
		t.Fatalf("unknown op %q", op)
	}
	return err
}
