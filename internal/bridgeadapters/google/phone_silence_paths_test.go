package google

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"

	"github.com/maxghenis/openmessage/internal/bridge"
)

// pathCounts is what one send through a path did on the transport.
type pathCounts struct{ lookups, uploads, sends int }

// sendPath runs one Google send kind that looks the conversation up first.
// Each call installs a fresh fake: a nil lookupErr answers the lookup, a nil
// sendErr answers the send. hook, if set, runs inside the lookup.
type sendPath struct {
	name string
	run  func(t *testing.T, a *Adapter, ctx context.Context, requestID string, lookupErr, sendErr error, hook func()) (error, pathCounts)
}

// hookedText, hookedReaction and hookedMedia run a hook inside the lookup.
type hookedText struct {
	fakeTextSendClient
	hook func()
}

func (h *hookedText) GetConversation(id string) (*gmproto.Conversation, error) {
	if h.hook != nil {
		h.hook()
	}
	return h.fakeTextSendClient.GetConversation(id)
}

type hookedReaction struct {
	fakeReactionSendClient
	hook func()
}

func (h *hookedReaction) GetConversation(id string) (*gmproto.Conversation, error) {
	if h.hook != nil {
		h.hook()
	}
	return h.fakeReactionSendClient.GetConversation(id)
}

type hookedMedia struct {
	fakeMediaSendClient
	hook       func()
	uploadHook func()
	lookups    int
}

func (h *hookedMedia) UploadMedia(data []byte, filename, mime string) (*gmproto.MediaContent, error) {
	if h.uploadHook != nil {
		h.uploadHook()
	}
	return h.fakeMediaSendClient.UploadMedia(data, filename, mime)
}

func (h *hookedMedia) GetConversation(id string) (*gmproto.Conversation, error) {
	h.lookups++
	if h.hook != nil {
		h.hook()
	}
	return h.fakeMediaSendClient.GetConversation(id)
}

func lookupResult(err error) *gmproto.Conversation {
	if err != nil {
		return nil
	}
	return &gmproto.Conversation{}
}

var sendPaths = []sendPath{
	{"text", func(t *testing.T, a *Adapter, ctx context.Context, requestID string, lookupErr, sendErr error, hook func()) (error, pathCounts) {
		fake := &hookedText{hook: hook}
		fake.conversationResult, fake.conversationErr = lookupResult(lookupErr), lookupErr
		fake.sendErr = sendErr
		if sendErr == nil {
			fake.sendResult = &gmproto.SendMessageResponse{Status: gmproto.SendMessageResponse_SUCCESS}
		}
		installTextSendClient(t, a.host.GetClient(), fake)
		_, err := a.SendText(ctx, bridge.TextRequest{
			Conversation: bridge.ConversationRef{RemoteID: "conversation-id"},
			Body:         "hello",
			RequestID:    requestID,
		})
		return err, pathCounts{lookups: fake.conversationCalls, sends: fake.sendCalls}
	}},
	{"reaction", func(t *testing.T, a *Adapter, ctx context.Context, requestID string, lookupErr, sendErr error, hook func()) (error, pathCounts) {
		fake := &hookedReaction{hook: hook}
		fake.conversationResult, fake.conversationErr = lookupResult(lookupErr), lookupErr
		fake.sendErr = sendErr
		if sendErr == nil {
			fake.sendResult = &gmproto.SendReactionResponse{Success: true}
		}
		installReactionSendClient(t, a.host.GetClient(), fake)
		_, err := a.SendReaction(ctx, bridge.ReactionRequest{
			Conversation: bridge.ConversationRef{RemoteID: "conversation-id"},
			Target:       bridge.MessageRef{RemoteID: "message-id"},
			Emoji:        "👍",
			Action:       bridge.ReactionAdd,
			RequestID:    requestID,
		})
		return err, pathCounts{lookups: fake.conversationCalls, sends: fake.sendCalls}
	}},
	{"media", func(t *testing.T, a *Adapter, ctx context.Context, requestID string, lookupErr, sendErr error, hook func()) (error, pathCounts) {
		fake := &hookedMedia{hook: hook}
		fake.uploadResult = &gmproto.MediaContent{MediaID: "media"}
		fake.conversationResult, fake.conversationErr = lookupResult(lookupErr), lookupErr
		if sendErr == nil {
			fake.sendResults = []*gmproto.SendMessageResponse{{Status: gmproto.SendMessageResponse_SUCCESS}}
		} else {
			fake.sendErrors = []error{sendErr}
		}
		installMediaSendClient(t, a.host.GetClient(), fake)
		_, err := a.SendMedia(ctx, bridge.MediaRequest{
			Conversation: bridge.ConversationRef{RemoteID: "conversation-id"},
			Reader:       strings.NewReader("img"),
			Size:         3,
			Filename:     "photo.jpg",
			MIME:         "image/jpeg",
			RequestID:    requestID,
		})
		return err, pathCounts{lookups: fake.lookups, uploads: fake.uploadCalls, sends: len(fake.sent)}
	}},
}

func newPathAdapter(t *testing.T) (*Adapter, *silenceClock) {
	t.Helper()
	a := newTextSendTestAdapter(t, &fakeTextSendClient{})
	clock := newSilenceClock()
	a.silence.now = clock.Now
	return a, clock
}

func windowLeft(a *Adapter) time.Duration {
	a.silence.mu.Lock()
	defer a.silence.mu.Unlock()
	if a.silence.until.IsZero() {
		return 0
	}
	return a.silence.until.Sub(a.silence.clock())
}

// Every send kind classifies a lookup the phone did not answer the same way:
// retryable, not dispatched, at the window's end plus the intent's own delay,
// with nothing uploaded or sent, and the gate closed for the others.
func TestEverySendPathClassifiesAnUnansweredLookup(t *testing.T) {
	for _, path := range sendPaths {
		t.Run(path.name, func(t *testing.T) {
			a, clock := newPathAdapter(t)
			err, counts := path.run(t, a, context.Background(), "intent", unanswered(gmproto.ActionType_GET_CONVERSATION), nil, nil)
			failure, ok := asOpError(err)
			if !ok || failure.Class != bridge.FailureTransient || failure.Dispatch != bridge.DispatchNotCalled ||
				failure.Fingerprint != "google_conversation_get_unanswered" ||
				!failure.RetryAt.Equal(clock.Now().Add(time.Minute)) {
				t.Fatalf("failure = %v (%+v), want retryable at +1m", err, failure)
			}
			if counts.lookups != 1 || counts.uploads != 0 || counts.sends != 0 {
				t.Fatalf("transport = %+v, want one lookup and nothing else", counts)
			}
			if _, _, held := a.silence.held(); !held {
				t.Fatal("an unanswered lookup left the gate open")
			}
		})
	}
}

// Every send kind tracks its own lookup: one that goes unanswered again after
// the phone answered something else doubles its own delay.
func TestEverySendPathBacksOffItsOwnLookup(t *testing.T) {
	for _, path := range sendPaths {
		t.Run(path.name, func(t *testing.T) {
			a, clock := newPathAdapter(t)
			path.run(t, a, context.Background(), "intent", unanswered(gmproto.ActionType_GET_CONVERSATION), nil, nil)
			a.phoneAnswered("another-intent")
			err, _ := path.run(t, a, context.Background(), "intent", unanswered(gmproto.ActionType_GET_CONVERSATION), nil, nil)
			failure, _ := asOpError(err)
			if want := clock.Now().Add(30*time.Second + time.Minute); !failure.RetryAt.Equal(want) {
				t.Fatalf("second unanswered lookup RetryAt = %s, want the window plus a doubled delay (%s)", failure.RetryAt, want)
			}
		})
	}
}

// Every send kind counts its answered lookup as the phone answering: when the
// send that follows goes unanswered, its window starts over at 30 s instead of
// continuing the old backoff.
func TestEverySendPathAnsweredLookupResetsTheBackoff(t *testing.T) {
	for _, path := range sendPaths {
		t.Run(path.name, func(t *testing.T) {
			a, clock := newPathAdapter(t)
			a.silence.recordUnanswered("")
			a.silence.recordUnanswered("") // second window: 1 minute
			clock.Advance(time.Minute)
			err, counts := path.run(t, a, context.Background(), "intent", nil, unanswered(gmproto.ActionType_SEND_MESSAGE), nil)
			if failure, _ := asOpError(err); failure.Dispatch != bridge.DispatchUncertain {
				t.Fatalf("unanswered send = %v, want uncertain", err)
			}
			if counts.sends != 1 {
				t.Fatalf("transport = %+v, want the send made", counts)
			}
			if left := windowLeft(a); left != 30*time.Second {
				t.Fatalf("window after an answered lookup and an unanswered send = %s, want 30s", left)
			}
		})
	}
}

// A lookup that fails for another reason clears the intent's own backoff, so
// a later unanswered lookup starts from the base again.
func TestLookupFailingOtherwiseClearsItsBackoff(t *testing.T) {
	path := sendPaths[0]
	a, clock := newPathAdapter(t)
	path.run(t, a, context.Background(), "intent", unanswered(gmproto.ActionType_GET_CONVERSATION), nil, nil)
	a.phoneAnswered("another-intent")
	if err, _ := path.run(t, a, context.Background(), "intent", errors.New("connection reset"), nil, nil); err == nil {
		t.Fatal("want the lookup failure")
	}
	a.phoneAnswered("another-intent")
	err, _ := path.run(t, a, context.Background(), "intent", unanswered(gmproto.ActionType_GET_CONVERSATION), nil, nil)
	failure, _ := asOpError(err)
	if want := clock.Now().Add(time.Minute); !failure.RetryAt.Equal(want) {
		t.Fatalf("RetryAt = %s, want a fresh 30s + 30s (%s)", failure.RetryAt, want)
	}
}

// If the dispatcher gives up on the call while the lookup runs (its context
// is canceled on abandon), nothing has been sent and nothing is: no upload,
// no send.
func TestSendStopsWhenTheDispatcherGaveUpDuringTheLookup(t *testing.T) {
	for _, path := range sendPaths {
		t.Run(path.name, func(t *testing.T) {
			a, _ := newPathAdapter(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			err, counts := path.run(t, a, ctx, "intent", nil, nil, cancel)
			failure, ok := asOpError(err)
			if !ok || failure.Dispatch != bridge.DispatchNotCalled || !errors.Is(failure.Cause, context.Canceled) {
				t.Fatalf("failure = %v (%+v), want not dispatched because the call was abandoned", err, failure)
			}
			if counts.lookups != 1 || counts.uploads != 0 || counts.sends != 0 {
				t.Fatalf("transport = %+v, want the lookup only", counts)
			}
		})
	}
}

// A new Google generation brings a new client, which may reach a phone the
// old one could not, so it reopens the gate rather than holding its first
// sends for the rest of the old window.
func TestNewGenerationReopensTheGate(t *testing.T) {
	host := newTestApp(t)
	a := newTestAdapter(t, host, &fakeTransport{})
	a.silence.now = newSilenceClock().Now
	a.silence.recordUnanswered("")
	if _, _, held := a.silence.held(); !held {
		t.Fatal("precondition: the gate should be closed")
	}
	run, err := a.Start(context.Background(), bridge.StartRequest{AccountID: "google-primary", Generation: 1}, &recordingSink{})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { stopRun(t, run) })
	if _, _, held := a.silence.held(); held {
		t.Fatal("a new generation left Google intents held")
	}
}

// A media send whose call the dispatcher abandons during the upload stops
// before sending, too.
func TestMediaSendStopsWhenTheDispatcherGaveUpDuringTheUpload(t *testing.T) {
	a, _ := newPathAdapter(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fake := &hookedMedia{uploadHook: cancel}
	fake.uploadResult = &gmproto.MediaContent{MediaID: "media"}
	fake.conversationResult = &gmproto.Conversation{}
	fake.sendResults = []*gmproto.SendMessageResponse{{Status: gmproto.SendMessageResponse_SUCCESS}}
	installMediaSendClient(t, a.host.GetClient(), fake)
	_, err := a.SendMedia(ctx, bridge.MediaRequest{
		Conversation: bridge.ConversationRef{RemoteID: "conversation-id"},
		Reader:       strings.NewReader("img"),
		Size:         3,
		RequestID:    "intent",
	})
	failure, ok := asOpError(err)
	if !ok || failure.Dispatch != bridge.DispatchNotCalled || !errors.Is(failure.Cause, context.Canceled) {
		t.Fatalf("failure = %v (%+v), want not dispatched because the call was abandoned", err, failure)
	}
	if fake.uploadCalls != 1 || len(fake.sent) != 0 {
		t.Fatalf("uploads=%d sends=%d, want the upload and no send", fake.uploadCalls, len(fake.sent))
	}
}

// Recovery signals reopen the gate but are not answers: with the phone off,
// a queued text keeps waiting through reconnects and pinger recoveries, never
// refused as a lookup the phone singles out.
func TestRecoverySignalsNeverRefuseALookup(t *testing.T) {
	for _, signal := range []string{"new_generation", "phone_responding_again"} {
		t.Run(signal, func(t *testing.T) {
			a, text, clock := silentText(t)
			for probe := 1; probe <= 3*phoneSilenceMaxStuck; probe++ {
				failure := requireTextOpError(t, sendText(t, a, "intent"))
				if failure.Class != bridge.FailureTransient || failure.Fingerprint != "google_conversation_get_unanswered" {
					t.Fatalf("probe %d with the phone off = %+v, want it still retryable", probe, failure)
				}
				if want := clock.Now().Add(30*time.Second + 30*time.Second); !failure.RetryAt.Equal(want) {
					t.Fatalf("probe %d RetryAt = %s, want the base window and delay (%s)", probe, failure.RetryAt, want)
				}
				a.phoneRecovered(signal)
				clock.Advance(failure.RetryAt.Sub(clock.Now()))
			}
			if text.sendCalls != 0 {
				t.Fatalf("SendMessage calls = %d, want 0", text.sendCalls)
			}
		})
	}
}

// The log lines the runbook points operators to: a warning when a hold
// starts, with when it ends, and an info line when the phone answers again
// or libgm reports it back.
func TestHoldIsLogged(t *testing.T) {
	a, text, clock := silentText(t)
	var logs bytes.Buffer
	a.host.Logger = zerolog.New(&logs)
	requireTextOpError(t, sendText(t, a, "intent"))
	if out := logs.String(); !strings.Contains(out, `"level":"warn"`) ||
		!strings.Contains(out, "Google Messages phone did not answer; holding Google sends until the next probe") ||
		!strings.Contains(out, `"held_until"`) || !strings.Contains(out, `"operation":"send_text"`) {
		t.Fatalf("hold start log = %s", out)
	}
	logs.Reset()
	clock.Advance(30 * time.Second)
	answerEverything(text)
	if err := sendText(t, a, "intent"); err != nil {
		t.Fatal(err)
	}
	if out := logs.String(); !strings.Contains(out, "Google Messages phone answered again; releasing held Google sends") {
		t.Fatalf("hold end log = %s", out)
	}
	logs.Reset()
	a.silence.recordUnanswered("")
	a.phoneRecovered("phone_responding_again")
	if out := logs.String(); !strings.Contains(out, "Google Messages phone may be back; releasing held Google sends") ||
		!strings.Contains(out, `"reason":"phone_responding_again"`) {
		t.Fatalf("recovery log = %s", out)
	}
}
