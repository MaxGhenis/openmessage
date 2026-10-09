package app

import (
	"errors"
	"sync"
	"time"

	"go.mau.fi/mautrix-gmessages/pkg/libgm"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
)

// Google catch-up pulls (conversation listings and targeted lookups) can come
// back with no data while push keeps working. On 2026-10-06/07 every
// ListConversations returned zero conversations with no error for a store
// holding ~1,000 Google threads, and deep backfill reported errors=0. The pull
// health recorder keeps each pull's outcome so /api/status says so instead of
// reporting a quiet success.

// googlePullEmptyThreshold is how many Google conversations the store must
// hold for a data-less INBOX pull to count as suspect. Below it, an empty
// inbox is plausible (a new or freshly cleared account).
const googlePullEmptyThreshold = 10

// GooglePullOutcome classifies one pull.
type GooglePullOutcome string

const (
	// GooglePullOK: the pull returned data.
	GooglePullOK GooglePullOutcome = "ok"
	// GooglePullEmpty: the phone answered with an empty result.
	GooglePullEmpty GooglePullOutcome = "empty"
	// GooglePullNoPayload: the phone answered only with frames that carry no
	// response payload (libgm.ErrNoResponsePayload).
	GooglePullNoPayload GooglePullOutcome = "no_payload"
	// GooglePullError: the pull failed for another reason (transport, auth).
	GooglePullError GooglePullOutcome = "error"
)

// GooglePullHealthSnapshot is the status view of recent Google pulls.
type GooglePullHealthSnapshot struct {
	LastAttemptMS int64             `json:"last_attempt_ms"`
	LastTrigger   string            `json:"last_trigger"`
	LastFolder    string            `json:"last_folder,omitempty"`
	LastOutcome   GooglePullOutcome `json:"last_outcome"`
	LastCount     int               `json:"last_count"`
	LastError     string            `json:"last_error,omitempty"`
	// LastDataMS is when a counted pull last returned data.
	LastDataMS int64 `json:"last_data_ms,omitempty"`
	// AccountSwitch is set when the latest payload-less answer carried the
	// phone's Google-account switch notice.
	AccountSwitch bool `json:"account_switch,omitempty"`
	// ConsecutiveDataless counts counted pulls in a row (first INBOX pages and
	// targeted lookups) that came back empty or payload-less.
	ConsecutiveDataless int `json:"consecutive_dataless"`
	// LocalConversations is the Google conversation count of the active store,
	// measured at the latest data-less counted pull.
	LocalConversations int `json:"local_conversations"`
	// EmptyWithLocalHistory is the health signal: the latest counted pull came
	// back with no data while the store held at least Threshold Google
	// conversations. The next counted pull that returns data clears it.
	EmptyWithLocalHistory bool `json:"empty_with_local_history"`
	Threshold             int  `json:"threshold"`
}

// googlePullDatalessCap bounds the data-less streak the recorder remembers;
// consecutive_dataless saturates there. Over the cap the oldest entry is
// evicted, which keeps the state independent of apply order. (A var so tests
// can exercise saturation cheaply.)
var googlePullDatalessCap = 1024

type googlePullHealth struct {
	mu       sync.Mutex
	recorded bool
	snap     GooglePullHealthSnapshot
	counter  func() (int, error)
	now      func() time.Time

	// Pulls are sequenced when their result arrives, before the (unlocked)
	// store count, and applied so that the final state depends only on that
	// order, not on which recorder finishes first: an older result can't
	// overwrite newer evidence or drop a concurrent data-less pull from the
	// streak.
	seq        uint64
	appliedSeq uint64 // newest pull reflected in the last_* fields
	countedSeq uint64 // newest counted pull with data or without
	dataSeq    uint64 // newest counted pull that returned data
	localSeq   uint64 // newest counted pull whose local count succeeded
	// newestDataless and newestAccountSwitch describe the countedSeq pull.
	newestDataless      bool
	newestAccountSwitch bool
	// dataless holds the sequence numbers of counted data-less pulls newer
	// than dataSeq; its length is the streak.
	dataless []uint64
}

type googlePullRecord struct {
	seq           uint64
	atMS          int64
	trigger       string
	folder        string
	count         int
	err           error
	outcome       GooglePullOutcome
	accountSwitch bool
	counted       bool
	local         int // -1 when not measured
}

// SetGoogleConversationCounter installs the active read source's Google
// conversation count (v2 on a v2-primary install). Without one, pull health
// counts the legacy store.
func (a *App) SetGoogleConversationCounter(count func() (int, error)) {
	a.googlePull.mu.Lock()
	a.googlePull.counter = count
	a.googlePull.mu.Unlock()
}

// GooglePullHealth returns the pull health snapshot, or nil before any pull.
func (a *App) GooglePullHealth() *GooglePullHealthSnapshot {
	a.googlePull.mu.Lock()
	defer a.googlePull.mu.Unlock()
	if !a.googlePull.recorded {
		return nil
	}
	snap := a.googlePull.snap
	return &snap
}

// IsGoogleAccountSwitchError reports whether err is a pull or lookup the phone
// answered with its Google-account switch notice instead of data (the phone
// switched to Google-account pairing; this session is a QR pairing).
func IsGoogleAccountSwitchError(err error) bool {
	var payloadErr *libgm.ResponsePayloadError
	return errors.As(err, &payloadErr) && payloadErr.AccountSwitch
}

func classifyGooglePull(count int, err error) GooglePullOutcome {
	switch {
	case errors.Is(err, libgm.ErrNoResponsePayload):
		return GooglePullNoPayload
	case err != nil:
		return GooglePullError
	case count == 0:
		return GooglePullEmpty
	default:
		return GooglePullOK
	}
}

// recordGoogleListPull records a ListConversations pull. Only first pages of
// INBOX count toward the health signal: other folders and later pages can be
// empty legitimately. clientToken identifies the client generation that made
// the pull; results from a retired client are dropped.
func (a *App) recordGoogleListPull(clientToken any, trigger string, folder gmproto.ListConversationsRequest_Folder, firstPage bool, resp *gmproto.ListConversationsResponse, err error) {
	counted := firstPage && folder == gmproto.ListConversationsRequest_INBOX
	a.recordGooglePull(clientToken, trigger, folder.String(), len(resp.GetConversations()), err, counted)
}

// recordGoogleLookupPull records a targeted conversation lookup, which should
// always return a conversation.
func (a *App) recordGoogleLookupPull(clientToken any, trigger string, conv *gmproto.Conversation, err error) {
	found := 0
	if conv != nil {
		found = 1
	}
	a.recordGooglePull(clientToken, trigger, "", found, err, true)
}

func (a *App) recordGooglePull(clientToken any, trigger, folder string, count int, err error, counted bool) {
	if clientToken != nil && !a.backfillClientStillCurrent(clientToken) {
		// A retired client's late answer (e.g. a payload-less expiry after a
		// re-pair) says nothing about the current session.
		a.Logger.Debug().Str("trigger", trigger).AnErr("error", err).Msg("Ignoring Google pull outcome from a retired client")
		return
	}
	rec := googlePullRecord{
		trigger: trigger,
		folder:  folder,
		count:   count,
		err:     err,
		outcome: classifyGooglePull(count, err),
		counted: counted,
		local:   -1,
	}
	rec.accountSwitch = IsGoogleAccountSwitchError(err)
	dataless := rec.outcome == GooglePullEmpty || rec.outcome == GooglePullNoPayload

	h := &a.googlePull
	h.mu.Lock()
	h.seq++
	rec.seq = h.seq
	now := time.Now
	if h.now != nil {
		now = h.now
	}
	rec.atMS = now().UnixMilli()
	h.mu.Unlock()

	// Count outside the lock: it reads a store.
	if counted && dataless {
		rec.local = a.countGoogleConversations()
	}

	var (
		snap            GooglePullHealthSnapshot
		raised, cleared bool
	)
	if !a.whileGoogleClientCurrent(clientToken, func() { snap, raised, cleared = h.apply(rec) }) {
		// The client was replaced while this pull's local count ran.
		a.Logger.Debug().Str("trigger", trigger).AnErr("error", err).Msg("Ignoring Google pull outcome from a retired client")
		return
	}

	evt := a.Logger.Info()
	if dataless && counted {
		evt = a.Logger.Warn()
	}
	evt.Str("trigger", trigger).
		Str("folder", folder).
		Str("outcome", string(rec.outcome)).
		Int("count", count).
		Int("consecutive_dataless", snap.ConsecutiveDataless).
		Int("local_conversations", snap.LocalConversations).
		Bool("empty_with_local_history", snap.EmptyWithLocalHistory).
		Bool("account_switch", snap.AccountSwitch).
		AnErr("error", err).
		Msg("Google pull outcome")
	if raised {
		a.Logger.Error().
			Int("local_conversations", snap.LocalConversations).
			Int("consecutive_dataless", snap.ConsecutiveDataless).
			Bool("account_switch", snap.AccountSwitch).
			Msg("Google pulls return no data while the store holds this account's conversations; catch-up is not working")
	}
	if raised || cleared {
		a.emitStatusChange(a.Connected.Load())
	}
}

// apply folds one sequenced pull into the health state. The result depends
// only on the set of records and their sequence numbers, not on the order in
// which apply is called.
func (h *googlePullHealth) apply(rec googlePullRecord) (snap GooglePullHealthSnapshot, raised, cleared bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := &h.snap
	was := s.EmptyWithLocalHistory
	h.recorded = true
	s.Threshold = googlePullEmptyThreshold

	if rec.seq > h.appliedSeq {
		h.appliedSeq = rec.seq
		s.LastAttemptMS = rec.atMS
		s.LastTrigger = rec.trigger
		s.LastFolder = rec.folder
		s.LastOutcome = rec.outcome
		s.LastCount = rec.count
		s.LastError = ""
		if rec.err != nil {
			s.LastError = rec.err.Error()
		}
	}

	dataless := rec.outcome == GooglePullEmpty || rec.outcome == GooglePullNoPayload
	// Transport and auth errors are evidence of neither data nor its absence,
	// so they never move the signal or the streak.
	if rec.counted && (dataless || rec.outcome == GooglePullOK) {
		if rec.outcome == GooglePullOK {
			if rec.seq > h.dataSeq {
				h.dataSeq = rec.seq
				s.LastDataMS = rec.atMS
			}
			kept := h.dataless[:0]
			for _, q := range h.dataless {
				if q > rec.seq {
					kept = append(kept, q)
				}
			}
			h.dataless = kept
		} else if rec.seq > h.dataSeq {
			h.dataless = append(h.dataless, rec.seq)
			if len(h.dataless) > googlePullDatalessCap {
				// Keep the newest: drop the smallest sequence number, so
				// the retained set is the same whatever the apply order.
				oldest := 0
				for i, q := range h.dataless {
					if q < h.dataless[oldest] {
						oldest = i
					}
				}
				h.dataless = append(h.dataless[:oldest], h.dataless[oldest+1:]...)
			}
		}
		if dataless && rec.local >= 0 && rec.seq > h.localSeq {
			h.localSeq = rec.seq
			s.LocalConversations = rec.local
		}
		if rec.seq > h.countedSeq {
			h.countedSeq = rec.seq
			h.newestDataless = dataless
			h.newestAccountSwitch = dataless && rec.accountSwitch
		}
	}
	s.ConsecutiveDataless = len(h.dataless)
	s.AccountSwitch = h.newestAccountSwitch
	s.EmptyWithLocalHistory = h.newestDataless && s.LocalConversations >= googlePullEmptyThreshold
	return *s, s.EmptyWithLocalHistory && !was, was && !s.EmptyWithLocalHistory
}

// whileGoogleClientCurrent runs fn only if clientToken (nil = any) still
// identifies the current Google client, holding the client lock so a
// replacement can't land between the check and fn. Every path that replaces
// a.Client does so under clientMu.
func (a *App) whileGoogleClientCurrent(clientToken any, fn func()) bool {
	if clientToken == nil {
		fn()
		return true
	}
	if a.gmClient != nil {
		if a.gmClient != clientToken {
			return false
		}
		fn()
		return true
	}
	a.clientMu.RLock()
	defer a.clientMu.RUnlock()
	if a.Client == nil || a.Client.GM != clientToken {
		return false
	}
	fn()
	return true
}

func (a *App) countGoogleConversations() int {
	a.googlePull.mu.Lock()
	counter := a.googlePull.counter
	a.googlePull.mu.Unlock()
	if counter == nil {
		if a.Store == nil {
			return -1
		}
		counter = func() (int, error) { return a.Store.ConversationCount("sms") }
	}
	n, err := counter()
	if err != nil {
		a.Logger.Warn().Err(err).Msg("Pull health: count local Google conversations failed")
		return -1
	}
	return n
}
