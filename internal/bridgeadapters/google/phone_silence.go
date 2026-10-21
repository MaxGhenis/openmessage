package google

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"go.mau.fi/mautrix-gmessages/pkg/libgm"

	"github.com/maxghenis/openmessage/internal/bridge"
)

// Requests the phone does not answer.
//
// libgm fails a request the phone leaves unanswered for client.RequestTimeout
// with libgm.ErrPhoneNotResponding. The outbox runs every intent, on every
// account, one at a time on one loop, so each such request holds every later
// send, reaction and read receipt, on every platform, for the whole timeout.
//
// A conversation lookup that times out is safe to retry: every Google send
// looks the conversation up before it sends anything. But the outbox retries a
// not-dispatched failure every 5 s with no cap, so while the phone stays silent
// one queued intent would hold the loop for 45 s of every 50, and each further
// queued Google intent would add its own 45 s to every round.
//
// phoneSilence spaces those holds out:
//
//   - After any send-path request goes unanswered, Google intents on this
//     account fail at once, before any request, until a probe window passes.
//     Each held intent is retried within phoneSilenceRecheck, which costs the
//     loop nothing, so it goes out soon after the phone is back.
//   - The first intent due after the window probes the phone. The window
//     doubles with each unanswered probe, from phoneSilenceBaseBackoff up to
//     phoneSilenceMaxBackoff, so a silent phone holds the loop for at most one
//     timeout per window.
//   - An unanswered lookup is retried a little after the window ends, so the
//     intents held meanwhile probe first and the probing rotates. If the phone
//     answered something else since that intent's previous unanswered lookup,
//     the lookup itself is what goes unanswered, so its own delay doubles too
//     (same schedule): it can't be the probe every time and hold the loop
//     while the phone answers everything else. While the phone is wholly
//     silent the account's window does the spacing, and each intent's own
//     delay stays at the base, so it goes out soon after the phone is back.
//   - Any answered request, or libgm reporting that the phone is responding
//     again (its pinger keeps probing in the background), reopens the gate.
const (
	phoneSilenceBaseBackoff = 30 * time.Second
	phoneSilenceMaxBackoff  = 5 * time.Minute
	phoneSilenceRecheck     = 30 * time.Second
	// phoneSilenceMaxIntents bounds the per-intent backoff table. Entries
	// leave it when their lookup is answered or fails another way; this only
	// matters for intents that stop being retried while still in it.
	phoneSilenceMaxIntents = 256
)

type phoneSilence struct {
	mu  sync.Mutex
	now func() time.Time // nil = time.Now

	// since is when the phone first left a request unanswered, zero while it
	// answers. until is when the next probe may go out, zero while open.
	since      time.Time
	until      time.Time
	unanswered int

	// answers counts answered requests and recovery signals, so an intent can
	// tell whether the phone answered anything since its last lookup.
	answers uint64
	// intents holds the lookups that went unanswered, by transport request ID.
	intents map[string]*silentIntent
}

type silentIntent struct {
	// unanswered counts this intent's unanswered lookups that each followed
	// an answer to some other request.
	unanswered int
	answers    uint64 // answers when its last lookup went unanswered
	lastAt     time.Time
}

// phoneSilenceBackoff is the wait after the given number of consecutive
// unanswered requests: phoneSilenceBaseBackoff doubled for each one after the
// first, at most phoneSilenceMaxBackoff.
func phoneSilenceBackoff(unanswered int) time.Duration {
	backoff := phoneSilenceBaseBackoff
	for i := 1; i < unanswered && backoff < phoneSilenceMaxBackoff; i++ {
		backoff *= 2
	}
	if backoff > phoneSilenceMaxBackoff {
		return phoneSilenceMaxBackoff
	}
	return backoff
}

func (p *phoneSilence) clock() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

// held reports whether Google requests are held, and if so since when the
// phone has been silent and when the held intent should be retried.
func (p *phoneSilence) held() (since, retryAt time.Time, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.clock()
	if p.until.IsZero() || !now.Before(p.until) {
		return time.Time{}, time.Time{}, false
	}
	retryAt = now.Add(phoneSilenceRecheck)
	if p.until.Before(retryAt) {
		retryAt = p.until
	}
	return p.since, retryAt, true
}

// recordUnanswered closes the gate after a request the phone did not answer
// and returns when the next request may go out. A lookup passes its intent's
// transport request ID, and gets back a retry time past the account's window
// by the intent's own delay. A request that will not be retried (a send, a
// read receipt) passes "".
func (p *phoneSilence) recordUnanswered(requestID string) time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.clock()
	if p.since.IsZero() {
		p.since = now
	}
	p.unanswered++
	p.until = now.Add(phoneSilenceBackoff(p.unanswered))
	if requestID == "" {
		return p.until
	}
	if p.intents == nil {
		p.intents = make(map[string]*silentIntent)
	}
	intent := p.intents[requestID]
	switch {
	case intent == nil:
		p.evictOldestIntentLocked()
		intent = &silentIntent{unanswered: 1}
		p.intents[requestID] = intent
	case p.answers != intent.answers:
		intent.unanswered++
	}
	intent.answers = p.answers
	intent.lastAt = now
	return p.until.Add(phoneSilenceBackoff(intent.unanswered))
}

func (p *phoneSilence) evictOldestIntentLocked() {
	if len(p.intents) < phoneSilenceMaxIntents {
		return
	}
	var oldestID string
	var oldest time.Time
	for id, intent := range p.intents {
		if oldestID == "" || intent.lastAt.Before(oldest) {
			oldestID, oldest = id, intent.lastAt
		}
	}
	delete(p.intents, oldestID)
}

// recordAnswered reopens the gate after the phone answered a request. A
// lookup passes its intent's transport request ID, which also clears that
// intent's own backoff.
func (p *phoneSilence) recordAnswered(requestID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reopenLocked()
	delete(p.intents, requestID)
}

// forget clears an intent's own backoff after its lookup failed for a reason
// other than silence; the gate is unchanged.
func (p *phoneSilence) forget(requestID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.intents, requestID)
}

// reopen opens the gate when libgm reports the phone responding again. Each
// intent keeps its own backoff: its retry is already scheduled.
func (p *phoneSilence) reopen() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reopenLocked()
}

func (p *phoneSilence) reopenLocked() {
	p.since = time.Time{}
	p.until = time.Time{}
	p.unanswered = 0
	p.answers++
}

// phoneHeldError is the failure for an intent held while the phone is silent:
// nothing was sent, so it is retried, at retryAt.
func phoneHeldError(operation string, since, retryAt time.Time) bridge.OpError {
	return bridge.OpError{
		Class:       bridge.FailureTransient,
		Operation:   operation,
		RetryAt:     retryAt,
		Fingerprint: "google_phone_not_answering",
		Dispatch:    bridge.DispatchNotCalled,
		Cause: fmt.Errorf(
			"Google Messages: the phone has not answered since %s; held until %s",
			since.UTC().Format(time.RFC3339),
			retryAt.UTC().Format(time.RFC3339),
		),
	}
}

// holdIfPhoneSilent returns the failure for an intent that must wait while the
// phone is silent, or nil when it may go out.
func (a *Adapter) holdIfPhoneSilent(operation string) *bridge.OpError {
	since, retryAt, held := a.silence.held()
	if !held {
		return nil
	}
	failure := phoneHeldError(operation, since, retryAt)
	return &failure
}

// classifyLookup records what a failed conversation lookup says about the
// phone and adjusts its failure. A lookup the phone did not answer sent
// nothing, so it stays retryable (DispatchNotCalled), at the time the silence
// backoff allows.
func (a *Adapter) classifyLookup(failure bridge.OpError, err error, requestID string) bridge.OpError {
	if !errors.Is(err, libgm.ErrPhoneNotResponding) {
		a.silence.forget(requestID)
		return failure
	}
	failure.Fingerprint = "google_conversation_get_unanswered"
	failure.Dispatch = bridge.DispatchNotCalled
	failure.RetryAt = a.silence.recordUnanswered(requestID)
	return failure
}

// classifyUnansweredSend marks a send the phone did not answer, or that was
// still waiting when the client disconnected, as uncertain: the server had
// accepted the request, so the phone may still act on it, and it must not be
// sent again. A send the phone did not answer also closes the gate.
func (a *Adapter) classifyUnansweredSend(failure bridge.OpError, err error) bridge.OpError {
	if errors.Is(err, libgm.ErrPhoneNotResponding) {
		a.silence.recordUnanswered("")
		failure.Dispatch = bridge.DispatchUncertain
	} else if errors.Is(err, libgm.ErrConnectionClosed) {
		failure.Dispatch = bridge.DispatchUncertain
	}
	return failure
}
