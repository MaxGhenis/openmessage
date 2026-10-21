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
// one queued intent would hold the loop almost all the time, and each further
// queued Google intent would add its own timeout to every round.
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
//     while the phone answers everything else. After phoneSilenceMaxStuck
//     such lookups the intent is refused (nothing was sent), rather than
//     retried for ever. While the phone is wholly silent the account's window
//     does the spacing and each intent's own delay stays at the base, so it
//     goes out soon after the phone is back.
//   - An answer to a send-path request (lookup, send, reaction, read receipt),
//     libgm reporting that the phone is responding again (its pinger keeps
//     probing in the background), or a new Google generation reopens the gate.
const (
	phoneSilenceBaseBackoff = 30 * time.Second
	phoneSilenceMaxBackoff  = 5 * time.Minute
	phoneSilenceRecheck     = 30 * time.Second
	// phoneSilenceMaxStuck is how many times one intent's lookup may go
	// unanswered, the first time and then each time after the phone answered
	// something else, before the intent is refused.
	phoneSilenceMaxStuck = 6
	// phoneSilenceMaxIntents bounds the per-intent backoff table. Entries
	// leave it when their lookup is answered, fails another way or is
	// refused; this only matters for intents that stop being retried while
	// still in it.
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

	// answers counts answered send-path requests, so an intent can tell
	// whether the phone answered anything since its last lookup. Recovery
	// signals (a new generation, libgm's PhoneRespondingAgain) reopen the
	// gate but don't count: they don't show the phone answering a request,
	// so they must not push an intent toward refusal while the phone is off.
	answers uint64
	// intents holds the lookups that went unanswered, by transport request ID.
	intents map[string]*silentIntent
}

type silentIntent struct {
	// unanswered counts this intent's unanswered lookups: the first, and each
	// later one that followed an answer to some other request.
	unanswered int
	answers    uint64 // answers when its last lookup went unanswered
	lastAt     time.Time
}

// silenceRecord is what recordUnanswered decided.
type silenceRecord struct {
	// Since is when this silence began, Until when its window ends, and
	// Unanswered the number of consecutive unanswered requests.
	Since      time.Time
	Until      time.Time
	Unanswered int
	// RetryAt is when the intent may retry: Until for a request recorded
	// without an intent, later for a lookup, zero when Stuck.
	RetryAt time.Time
	// Stuck reports a lookup refused after phoneSilenceMaxStuck tries.
	Stuck bool
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

// recordUnanswered closes the gate after a request the phone did not answer.
// A lookup passes its intent's transport request ID and is given a retry time
// past the window by the intent's own delay, or is refused as stuck. A request
// that will not be retried (a send, a read receipt) passes "".
func (p *phoneSilence) recordUnanswered(requestID string) silenceRecord {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.clock()
	if p.since.IsZero() {
		p.since = now
	}
	p.unanswered++
	p.until = now.Add(phoneSilenceBackoff(p.unanswered))
	record := silenceRecord{Since: p.since, Until: p.until, Unanswered: p.unanswered, RetryAt: p.until}
	if requestID == "" {
		return record
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
	if intent.unanswered >= phoneSilenceMaxStuck {
		delete(p.intents, requestID)
		record.Stuck = true
		record.RetryAt = time.Time{}
		return record
	}
	intent.answers = p.answers
	intent.lastAt = now
	record.RetryAt = p.until.Add(phoneSilenceBackoff(intent.unanswered))
	return record
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

// recordAnswered reopens the gate after the phone answered a request, and
// reports whether the phone had been silent until then. A lookup passes its
// intent's transport request ID, which also clears that intent's own backoff.
func (p *phoneSilence) recordAnswered(requestID string) (wasSilent bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.intents, requestID)
	p.answers++
	return p.reopenLocked()
}

// forget clears an intent's own backoff after its lookup failed for a reason
// other than silence; the gate is unchanged.
func (p *phoneSilence) forget(requestID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.intents, requestID)
}

// reopen opens the gate when libgm reports the phone responding again or a
// new generation starts, and reports whether the phone had been silent. Each
// intent keeps its own backoff (its retry is already scheduled), and its stuck
// count doesn't grow: only an answered request shows the phone answering.
func (p *phoneSilence) reopen() (wasSilent bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.reopenLocked()
}

func (p *phoneSilence) reopenLocked() bool {
	wasSilent := p.unanswered > 0
	p.since = time.Time{}
	p.until = time.Time{}
	p.unanswered = 0
	return wasSilent
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
			"google_phone_not_answering: the phone has not answered since %s; held until %s",
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

// phoneUnanswered records a send-path request the phone did not answer and
// logs the hold it starts (once per probe window: requests in between are
// held before they reach the phone).
func (a *Adapter) phoneUnanswered(operation, requestID string) silenceRecord {
	record := a.silence.recordUnanswered(requestID)
	if a.host != nil {
		a.host.Logger.Warn().
			Str("operation", operation).
			Int("unanswered", record.Unanswered).
			Time("silent_since", record.Since).
			Time("held_until", record.Until).
			Bool("lookup_refused", record.Stuck).
			Msg("Google Messages phone did not answer; holding Google sends until the next probe")
	}
	return record
}

// phoneAnswered records an answered send-path request and logs the end of a
// silence.
func (a *Adapter) phoneAnswered(requestID string) {
	if a.silence.recordAnswered(requestID) && a.host != nil {
		a.host.Logger.Info().Msg("Google Messages phone answered again; releasing held Google sends")
	}
}

// phoneRecovered reopens the gate on a recovery signal, logging the end of a
// silence.
func (a *Adapter) phoneRecovered(reason string) {
	if a.silence.reopen() && a.host != nil {
		a.host.Logger.Info().Str("reason", reason).Msg("Google Messages phone may be back; releasing held Google sends")
	}
}

// classifyLookup records what a failed conversation lookup says about the
// phone and adjusts its failure. A lookup the phone did not answer sent
// nothing, so it stays retryable (DispatchNotCalled), at the time the silence
// backoff allows. One that keeps going unanswered while the phone answers
// other requests is refused, still as not sent.
func (a *Adapter) classifyLookup(failure bridge.OpError, err error, requestID string) bridge.OpError {
	if !errors.Is(err, libgm.ErrPhoneNotResponding) {
		a.silence.forget(requestID)
		return failure
	}
	record := a.phoneUnanswered(failure.Operation, requestID)
	failure.Dispatch = bridge.DispatchNotCalled
	if record.Stuck {
		failure.Class = bridge.FailureUnsupported
		failure.Fingerprint = "google_conversation_get_never_answered"
		failure.Cause = fmt.Errorf(
			"google_conversation_get_never_answered: the phone answered other requests but left this conversation's lookup unanswered %d times; nothing was sent: %w",
			phoneSilenceMaxStuck, failure.Cause,
		)
		return failure
	}
	failure.Fingerprint = "google_conversation_get_unanswered"
	failure.Cause = fmt.Errorf("google_conversation_get_unanswered: %w", failure.Cause)
	failure.RetryAt = record.RetryAt
	return failure
}

// classifyUnansweredSend marks a send the phone did not answer, or that was
// still waiting when the client disconnected, as uncertain: the server had
// accepted the request, so the phone may still act on it, and it must not be
// sent again. A send the phone did not answer also closes the gate.
func (a *Adapter) classifyUnansweredSend(failure bridge.OpError, err error) bridge.OpError {
	if errors.Is(err, libgm.ErrPhoneNotResponding) {
		a.phoneUnanswered(failure.Operation, "")
		failure.Dispatch = bridge.DispatchUncertain
	} else if errors.Is(err, libgm.ErrConnectionClosed) {
		failure.Dispatch = bridge.DispatchUncertain
	}
	return failure
}
