package google

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"testing"
	"time"

	"go.mau.fi/mautrix-gmessages/pkg/libgm"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/events"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"

	"github.com/maxghenis/openmessage/internal/bridge"
)

// silenceClock is a clock the test moves by hand.
type silenceClock struct {
	mu  sync.Mutex
	now time.Time
}

func newSilenceClock() *silenceClock {
	return &silenceClock{now: time.Date(2026, 10, 20, 15, 55, 0, 0, time.UTC)}
}

func (c *silenceClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *silenceClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func unanswered(action gmproto.ActionType) error {
	return &libgm.UnansweredRequestError{Action: action, Reason: libgm.ErrPhoneNotResponding, Waited: 45 * time.Second}
}

func disconnected(action gmproto.ActionType) error {
	return &libgm.UnansweredRequestError{Action: action, Reason: libgm.ErrConnectionClosed}
}

// wantBackoff is the probe window after k consecutive unanswered requests,
// written out here rather than computed with phoneSilenceBackoff.
func wantBackoff(k int) time.Duration {
	table := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 5 * time.Minute}
	if k < 1 {
		k = 1
	}
	if k > len(table) {
		k = len(table)
	}
	return table[k-1]
}

func TestPhoneSilenceBackoffDoublesToFiveMinutes(t *testing.T) {
	for k := 0; k <= 100; k++ {
		if got, want := phoneSilenceBackoff(k), wantBackoff(k); got != want {
			t.Fatalf("phoneSilenceBackoff(%d) = %s, want %s", k, got, want)
		}
	}
}

// silentText is a text adapter whose phone leaves every conversation lookup
// unanswered, on a hand-moved clock.
func silentText(t *testing.T) (*Adapter, *fakeTextSendClient, *silenceClock) {
	t.Helper()
	fake := &fakeTextSendClient{conversationErr: unanswered(gmproto.ActionType_GET_CONVERSATION)}
	a := newTextSendTestAdapter(t, fake)
	clock := newSilenceClock()
	a.silence.now = clock.Now
	return a, fake, clock
}

func sendText(t *testing.T, a *Adapter, requestID string) error {
	t.Helper()
	_, err := a.SendText(context.Background(), bridge.TextRequest{
		Conversation: bridge.ConversationRef{RemoteID: "conversation-id"},
		Body:         "hello",
		RequestID:    requestID,
	})
	return err
}

func answerEverything(fake *fakeTextSendClient) {
	fake.conversationErr = nil
	fake.conversationResult = &gmproto.Conversation{}
	fake.sendResult = &gmproto.SendMessageResponse{Status: gmproto.SendMessageResponse_SUCCESS}
}

// A lookup the phone did not answer sent nothing: it is retried, not marked
// uncertain, at the end of the probe window plus its own backoff. Until the
// window ends, every other Google intent fails at once without reaching the
// phone, so the dispatch loop is not held again.
func TestUnansweredLookupIsRetriedAndHoldsOtherGoogleIntents(t *testing.T) {
	a, text, clock := silentText(t)
	start := clock.Now()

	failure := requireTextOpError(t, sendText(t, a, "intent-1"))
	if failure.Class != bridge.FailureTransient || failure.Dispatch != bridge.DispatchNotCalled ||
		failure.Fingerprint != "google_conversation_get_unanswered" ||
		!errors.Is(failure.Cause, libgm.ErrPhoneNotResponding) {
		t.Fatalf("lookup failure = %+v, want a retryable not-dispatched unanswered lookup", failure)
	}
	if want := start.Add(30*time.Second + 30*time.Second); !failure.RetryAt.Equal(want) {
		t.Fatalf("RetryAt = %s, want the window (30s) plus the intent's backoff (30s): %s", failure.RetryAt, want)
	}
	if text.sendCalls != 0 {
		t.Fatalf("SendMessage calls = %d after an unanswered lookup, want 0", text.sendCalls)
	}

	// Every kind of Google intent is held while the window is open, without a
	// phone request and, for media, without reading or uploading the file.
	host := a.host.GetClient()
	reaction := &fakeReactionSendClient{conversationResult: &gmproto.Conversation{}}
	installReactionSendClient(t, host, reaction)
	read := &fakeReadSendClient{}
	installReadSendClient(t, host, read)
	media := &fakeMediaSendClient{}
	installMediaSendClient(t, host, media)

	clock.Advance(10 * time.Second)
	wantRetry := start.Add(30 * time.Second) // the window ends before a 30s recheck
	held := []struct {
		name string
		err  error
	}{
		{"text", sendText(t, a, "intent-2")},
		{"reaction", func() error {
			_, err := a.SendReaction(context.Background(), bridge.ReactionRequest{
				Conversation: bridge.ConversationRef{RemoteID: "conversation-id"},
				Target:       bridge.MessageRef{RemoteID: "message-id"},
				Emoji:        "👍",
				Action:       bridge.ReactionAdd,
				RequestID:    "intent-3",
			})
			return err
		}()},
		{"read", a.MarkRead(context.Background(), bridge.ReadReceiptRequest{
			Conversation: bridge.ConversationRef{RemoteID: "conversation-id"},
			Messages:     []bridge.MessageRef{{RemoteID: "message-id"}},
		})},
		{"media", func() error {
			_, err := a.SendMedia(context.Background(), bridge.MediaRequest{
				Conversation: bridge.ConversationRef{RemoteID: "conversation-id"},
				Reader:       panicReader{},
				Size:         3,
				Filename:     "photo.jpg",
				MIME:         "image/jpeg",
				RequestID:    "intent-4",
			})
			return err
		}()},
	}
	for _, h := range held {
		failure, ok := asOpError(h.err)
		if !ok || failure.Class != bridge.FailureTransient || failure.Dispatch != bridge.DispatchNotCalled ||
			failure.Fingerprint != "google_phone_not_answering" || !failure.RetryAt.Equal(wantRetry) {
			t.Fatalf("%s while the phone is silent = %v (%+v), want held until %s", h.name, h.err, failure, wantRetry)
		}
	}
	if text.conversationCalls != 1 || reaction.conversationCalls != 0 || read.calls != 0 || media.uploadCalls != 0 {
		t.Fatalf("phone requests while held: lookups text=%d reaction=%d, mark-read=%d, uploads=%d; want only the first lookup",
			text.conversationCalls, reaction.conversationCalls, read.calls, media.uploadCalls)
	}

	// Early in a long window, a held intent rechecks after 30s rather than
	// waiting out the window, so it goes soon after the phone is back.
	a.silence.recordUnanswered("")
	a.silence.recordUnanswered("") // window now 2 minutes from here
	clock.Advance(time.Second)
	failure = requireTextOpError(t, sendText(t, a, "intent-2"))
	if want := clock.Now().Add(30 * time.Second); !failure.RetryAt.Equal(want) {
		t.Fatalf("held RetryAt = %s, want the 30s recheck %s", failure.RetryAt, want)
	}

	// Once the window ends, the next intent probes; an answer reopens the gate.
	clock.Advance(3 * time.Minute)
	answerEverything(text)
	if err := sendText(t, a, "intent-2"); err != nil {
		t.Fatalf("send after the window = %v", err)
	}
	if err := sendText(t, a, "intent-5"); err != nil {
		t.Fatalf("send after the phone answered = %v", err)
	}
	if text.conversationCalls != 3 || text.sendCalls != 2 {
		t.Fatalf("lookups=%d sends=%d, want 3 and 2", text.conversationCalls, text.sendCalls)
	}
}

// While the phone stays silent, each probe it leaves unanswered doubles the
// window, up to 5 minutes; intents in between are held.
func TestUnansweredProbesBackOffToFiveMinutes(t *testing.T) {
	a, text, clock := silentText(t)
	for k := 1; k <= 7; k++ {
		probeAt := clock.Now()
		failure := requireTextOpError(t, sendText(t, a, fmt.Sprintf("probe-%d", k)))
		if failure.Fingerprint != "google_conversation_get_unanswered" {
			t.Fatalf("probe %d = %+v, want an unanswered lookup", k, failure)
		}
		until := probeAt.Add(wantBackoff(k))
		if want := until.Add(wantBackoff(1)); !failure.RetryAt.Equal(want) {
			t.Fatalf("probe %d RetryAt = %s, want %s", k, failure.RetryAt, want)
		}
		clock.Advance(wantBackoff(k) - time.Millisecond)
		if failure := requireTextOpError(t, sendText(t, a, "between")); failure.Fingerprint != "google_phone_not_answering" {
			t.Fatalf("intent just before window %d ends = %+v, want held", k, failure)
		}
		clock.Advance(time.Millisecond)
	}
	if text.conversationCalls != 7 {
		t.Fatalf("lookups = %d, want one per window (7)", text.conversationCalls)
	}
}

// Any answer from the phone reopens the gate and resets the backoff, so the
// next unanswered request opens a 30s window again, not a doubled one.
func TestAnsweredRequestResetsTheBackoff(t *testing.T) {
	a, text, clock := silentText(t)
	requireTextOpError(t, sendText(t, a, "a"))
	clock.Advance(30 * time.Second)
	requireTextOpError(t, sendText(t, a, "b")) // second window: 1 minute
	clock.Advance(time.Minute)
	answerEverything(text)
	if err := sendText(t, a, "c"); err != nil {
		t.Fatal(err)
	}
	text.conversationErr = unanswered(gmproto.ActionType_GET_CONVERSATION)
	failure := requireTextOpError(t, sendText(t, a, "d"))
	if want := clock.Now().Add(30*time.Second + 30*time.Second); !failure.RetryAt.Equal(want) {
		t.Fatalf("RetryAt after an answer = %s, want a fresh 30s window plus 30s: %s", failure.RetryAt, want)
	}
}

// stubbornLookups answers every conversation lookup except those for one
// conversation, which the phone never answers.
type stubbornLookups struct {
	fakeTextSendClient
	stuck string
}

func (s *stubbornLookups) GetConversation(conversationID string) (*gmproto.Conversation, error) {
	s.conversationCalls++
	if conversationID == s.stuck {
		return nil, unanswered(gmproto.ActionType_GET_CONVERSATION)
	}
	return &gmproto.Conversation{}, nil
}

// A lookup the phone never answers, while it answers everything else, backs
// off on its own schedule past the account's window. Other intents, due when
// the window ends, probe first and get through, so the stuck one costs the
// loop one timeout per its own growing interval and starves nobody.
func TestStuckLookupBacksOffPastTheWindowAndStarvesNobody(t *testing.T) {
	fake := &stubbornLookups{stuck: "stuck-conversation"}
	fake.sendResult = &gmproto.SendMessageResponse{Status: gmproto.SendMessageResponse_SUCCESS}
	a := newTextSendTestAdapter(t, &fake.fakeTextSendClient)
	installTextSendClient(t, a.host.GetClient(), fake)
	clock := newSilenceClock()
	a.silence.now = clock.Now

	send := func(requestID, conversation string) error {
		_, err := a.SendText(context.Background(), bridge.TextRequest{
			Conversation: bridge.ConversationRef{RemoteID: conversation},
			Body:         "hello",
			RequestID:    requestID,
		})
		return err
	}
	var lastGap time.Duration
	for round := 1; round <= 6; round++ {
		failure := requireTextOpError(t, send("stuck", "stuck-conversation"))
		windowEnds := clock.Now().Add(30 * time.Second) // reset by the answers in between
		if !failure.RetryAt.After(windowEnds) {
			t.Fatalf("round %d: stuck RetryAt %s is not past the window %s", round, failure.RetryAt, windowEnds)
		}
		if want := windowEnds.Add(wantBackoff(round)); !failure.RetryAt.Equal(want) {
			t.Fatalf("round %d: stuck RetryAt = %s, want %s", round, failure.RetryAt, want)
		}
		gap := failure.RetryAt.Sub(clock.Now())
		if gap < lastGap {
			t.Fatalf("round %d: stuck intent's interval shrank from %s to %s", round, lastGap, gap)
		}
		lastGap = gap
		// The others wait out the window, then get through.
		clock.Advance(30 * time.Second)
		if err := send(fmt.Sprintf("other-%d", round), "healthy-conversation"); err != nil {
			t.Fatalf("round %d: healthy send after the window = %v", round, err)
		}
		clock.Advance(failure.RetryAt.Sub(clock.Now()))
	}
}

// libgm's pinger reporting the phone back reopens the gate at once.
func TestPhoneRespondingAgainReopensTheGate(t *testing.T) {
	host := newTestApp(t)
	fake := &fakeTransport{}
	a := newTestAdapter(t, host, fake)
	run, err := a.Start(context.Background(), bridge.StartRequest{AccountID: "google-primary", Generation: 1}, &recordingSink{})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { stopRun(t, run) })
	clock := newSilenceClock()
	a.silence.now = clock.Now

	a.silence.recordUnanswered("")
	a.silence.recordUnanswered("")
	fake.emit(&events.PhoneNotResponding{})
	if _, _, held := a.silence.held(); !held {
		t.Fatal("precondition: the gate should be closed")
	}
	fake.emit(&events.PhoneRespondingAgain{})
	if _, _, held := a.silence.held(); held {
		t.Fatal("PhoneRespondingAgain left Google intents held")
	}
	// The backoff restarted too.
	if until := a.silence.recordUnanswered(""); !until.Equal(clock.Now().Add(30 * time.Second)) {
		t.Fatalf("window after recovery ends %s, want 30s", until.Sub(clock.Now()))
	}
}

// A send, reaction, media send or caption that went unanswered, or was still
// waiting when the client disconnected, may still be acted on by the phone:
// it is uncertain and never sent again. Only the unanswered one closes the
// gate; a disconnect says nothing about the phone.
func TestUnansweredSendsAreUncertain(t *testing.T) {
	type sendCase struct {
		name string
		run  func(t *testing.T, a *Adapter, sendErr error) error
	}
	cases := []sendCase{
		{"text", func(t *testing.T, a *Adapter, sendErr error) error {
			installTextSendClient(t, a.host.GetClient(), &fakeTextSendClient{conversationResult: &gmproto.Conversation{}, sendErr: sendErr})
			return sendText(t, a, "intent")
		}},
		{"reaction", func(t *testing.T, a *Adapter, sendErr error) error {
			installReactionSendClient(t, a.host.GetClient(), &fakeReactionSendClient{conversationResult: &gmproto.Conversation{}, sendErr: sendErr})
			_, err := a.SendReaction(context.Background(), bridge.ReactionRequest{
				Conversation: bridge.ConversationRef{RemoteID: "conversation-id"},
				Target:       bridge.MessageRef{RemoteID: "message-id"},
				Emoji:        "👍",
				Action:       bridge.ReactionAdd,
				RequestID:    "intent",
			})
			return err
		}},
		{"media", func(t *testing.T, a *Adapter, sendErr error) error {
			installMediaSendClient(t, a.host.GetClient(), &fakeMediaSendClient{
				uploadResult:       &gmproto.MediaContent{MediaID: "media"},
				conversationResult: &gmproto.Conversation{},
				sendErrors:         []error{sendErr},
			})
			_, err := a.SendMedia(context.Background(), bridge.MediaRequest{
				Conversation: bridge.ConversationRef{RemoteID: "conversation-id"},
				Reader:       bytes.NewReader([]byte("img")),
				Size:         3,
				Filename:     "photo.jpg",
				MIME:         "image/jpeg",
				RequestID:    "intent",
			})
			return err
		}},
		{"caption", func(t *testing.T, a *Adapter, sendErr error) error {
			installMediaSendClient(t, a.host.GetClient(), &fakeMediaSendClient{
				uploadResult:       &gmproto.MediaContent{MediaID: "media"},
				conversationResult: &gmproto.Conversation{},
				sendResults:        []*gmproto.SendMessageResponse{{Status: gmproto.SendMessageResponse_SUCCESS}},
				sendErrors:         []error{nil, sendErr},
			})
			_, err := a.SendMedia(context.Background(), bridge.MediaRequest{
				Conversation: bridge.ConversationRef{RemoteID: "conversation-id"},
				Reader:       bytes.NewReader([]byte("img")),
				Size:         3,
				Filename:     "photo.jpg",
				MIME:         "image/jpeg",
				Caption:      "look",
				RequestID:    "intent",
			})
			return err
		}},
	}
	for _, c := range cases {
		for _, reason := range []error{libgm.ErrPhoneNotResponding, libgm.ErrConnectionClosed} {
			t.Run(c.name+"/"+reason.Error(), func(t *testing.T) {
				a := newTextSendTestAdapter(t, &fakeTextSendClient{})
				clock := newSilenceClock()
				a.silence.now = clock.Now
				err := c.run(t, a, &libgm.UnansweredRequestError{Action: gmproto.ActionType_SEND_MESSAGE, Reason: reason})
				failure, ok := asOpError(err)
				if !ok || failure.Class != bridge.FailureTransient || failure.Dispatch != bridge.DispatchUncertain ||
					!errors.Is(failure.Cause, reason) {
					t.Fatalf("failure = %v (%+v), want transient and uncertain, wrapping %v", err, failure, reason)
				}
				_, _, held := a.silence.held()
				if want := errors.Is(reason, libgm.ErrPhoneNotResponding); held != want {
					t.Fatalf("gate closed = %t after %v, want %t", held, reason, want)
				}
			})
		}
	}
}

// A read receipt the phone did not answer may have been applied, and
// retrying it would hold the loop for another timeout, so it is uncertain. A
// receipt cut off by a disconnect costs nothing to repeat, so it stays
// retryable like any other failed receipt.
func TestMarkReadUnansweredIsUncertainAndDisconnectIsRetried(t *testing.T) {
	for _, c := range []struct {
		err      error
		dispatch bridge.DispatchCertainty
		gate     bool
	}{
		{unanswered(gmproto.ActionType_MESSAGE_READ), bridge.DispatchUncertain, true},
		{disconnected(gmproto.ActionType_MESSAGE_READ), bridge.DispatchNotCalled, false},
	} {
		t.Run(c.err.Error(), func(t *testing.T) {
			fake := &fakeReadSendClient{err: c.err}
			a := newReadSendTestAdapter(t, fake)
			a.silence.now = newSilenceClock().Now
			failure := requireMarkReadOpError(t, a.MarkRead(context.Background(), bridge.ReadReceiptRequest{
				Conversation: bridge.ConversationRef{RemoteID: "conversation-id"},
				Messages:     []bridge.MessageRef{{RemoteID: "message-id"}},
			}))
			if failure.Class != bridge.FailureTransient || failure.Dispatch != c.dispatch || !errors.Is(failure.Cause, c.err) {
				t.Fatalf("failure = %+v, want transient with dispatch %q", failure, c.dispatch)
			}
			if _, _, held := a.silence.held(); held != c.gate {
				t.Fatalf("gate closed = %t, want %t", held, c.gate)
			}
		})
	}
}

// A lookup cut off by a disconnect sent nothing: it is retried on the usual
// schedule and closes no gate, since a reconnect, not the phone, ends it.
func TestLookupCutOffByDisconnectIsRetriedWithoutHolding(t *testing.T) {
	fake := &fakeTextSendClient{conversationErr: disconnected(gmproto.ActionType_GET_CONVERSATION)}
	a := newTextSendTestAdapter(t, fake)
	a.silence.now = newSilenceClock().Now
	failure := requireTextOpError(t, sendText(t, a, "intent"))
	if failure.Dispatch != bridge.DispatchNotCalled || !failure.RetryAt.IsZero() ||
		failure.Fingerprint != "google_conversation_get_failed" {
		t.Fatalf("failure = %+v, want a not-dispatched lookup failure on the default retry", failure)
	}
	if _, _, held := a.silence.held(); held {
		t.Fatal("a disconnect closed the gate")
	}
}

// The per-intent backoff table is bounded: an intent that stops being retried
// while in it is eventually evicted, oldest first.
func TestSilentIntentTableIsBounded(t *testing.T) {
	clock := newSilenceClock()
	p := &phoneSilence{now: clock.Now}
	for i := 0; i < phoneSilenceMaxIntents+50; i++ {
		p.recordUnanswered(fmt.Sprintf("intent-%d", i))
		clock.Advance(time.Millisecond)
	}
	if len(p.intents) != phoneSilenceMaxIntents {
		t.Fatalf("table holds %d intents, want %d", len(p.intents), phoneSilenceMaxIntents)
	}
	for i := 0; i < 50; i++ {
		if _, ok := p.intents[fmt.Sprintf("intent-%d", i)]; ok {
			t.Fatalf("intent-%d, among the oldest, was not evicted", i)
		}
	}
	if _, ok := p.intents[fmt.Sprintf("intent-%d", phoneSilenceMaxIntents+49)]; !ok {
		t.Fatal("the newest intent was evicted")
	}
}

// Invariants, over random schedules of answered and unanswered requests,
// recovery signals and the passage of time, checked against a model kept by
// the test:
//
//  1. Requests are held exactly while the window is open, and a held
//     intent's retry time is after now and no later than the window's end or
//     the 30s recheck, whichever is first.
//  2. After the k-th unanswered request with no answer or recovery since,
//     the window lasts wantBackoff(k).
//  3. An unanswered lookup is retried strictly after the window ends, by its
//     own backoff, whose step counts only the unanswered lookups that
//     followed an answer or recovery since the intent's previous one.
//  4. A caller that only reaches the phone when not held (the adapter's
//     contract) leaves unanswered requests at least the window apart while
//     the phone stays silent. This is the bound on how often the dispatch
//     loop is held.
//  5. The intent table never exceeds its bound.
func TestPhoneSilenceInvariantsOverRandomSchedules(t *testing.T) {
	rng := rand.New(rand.NewSource(20261021))
	intents := []string{"", "a", "b", "c"}
	for script := 0; script < 3000; script++ {
		clock := newSilenceClock()
		p := &phoneSilence{now: clock.Now}
		var (
			modelUntil      time.Time
			modelCount      int
			modelIntent     = map[string]int{}
			answeredSince   = map[string]bool{} // an answer or recovery since the intent's last unanswered lookup
			lastUnanswered  time.Time
			lastWindow      time.Duration
			silentSinceLast bool
		)
		answer := func() {
			for id := range answeredSince {
				answeredSince[id] = true
			}
		}
		for step := 0; step < 40; step++ {
			now := clock.Now()
			_, retryAt, held := p.held()
			wantHeld := !modelUntil.IsZero() && now.Before(modelUntil)
			if held != wantHeld {
				t.Fatalf("script %d step %d: held=%t, want %t", script, step, held, wantHeld)
			}
			if held {
				limit := now.Add(30 * time.Second)
				if modelUntil.Before(limit) {
					limit = modelUntil
				}
				if !retryAt.After(now) || retryAt.After(limit) {
					t.Fatalf("script %d step %d: held retry %s outside (%s, %s]", script, step, retryAt, now, limit)
				}
			}
			switch op := rng.Intn(10); {
			case op < 4: // a well-behaved caller reaches the phone only when not held
				if held {
					break
				}
				id := intents[rng.Intn(len(intents))]
				if rng.Intn(3) == 0 { // answered
					p.recordAnswered(id)
					modelUntil, modelCount = time.Time{}, 0
					delete(modelIntent, id)
					delete(answeredSince, id)
					answer()
					silentSinceLast = false
					break
				}
				if silentSinceLast {
					if gap := now.Sub(lastUnanswered); gap < lastWindow {
						t.Fatalf("script %d step %d: unanswered requests %s apart, inside the %s window", script, step, gap, lastWindow)
					}
				}
				got := p.recordUnanswered(id)
				modelCount++
				modelUntil = now.Add(wantBackoff(modelCount))
				lastUnanswered, lastWindow, silentSinceLast = now, wantBackoff(modelCount), true
				if id == "" {
					if !got.Equal(modelUntil) {
						t.Fatalf("script %d step %d: window ends %s, want %s", script, step, got, modelUntil)
					}
					break
				}
				if modelIntent[id] == 0 || answeredSince[id] {
					modelIntent[id]++
				}
				answeredSince[id] = false
				if want := modelUntil.Add(wantBackoff(modelIntent[id])); !got.Equal(want) || !got.After(modelUntil) {
					t.Fatalf("script %d step %d: lookup %q retry %s, want %s (after the window %s)", script, step, id, got, want, modelUntil)
				}
			case op < 5:
				p.reopen()
				modelUntil, modelCount = time.Time{}, 0
				answer()
				silentSinceLast = false
			case op < 6:
				id := intents[1+rng.Intn(len(intents)-1)]
				p.forget(id)
				delete(modelIntent, id)
				delete(answeredSince, id)
			default:
				clock.Advance(time.Duration(rng.Intn(400_000)) * time.Millisecond)
			}
			if len(p.intents) > phoneSilenceMaxIntents {
				t.Fatalf("script %d step %d: %d intents tracked", script, step, len(p.intents))
			}
		}
	}
}
