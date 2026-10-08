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

type googlePullHealth struct {
	mu       sync.Mutex
	recorded bool
	snap     GooglePullHealthSnapshot
	counter  func() (int, error)
	now      func() time.Time
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
// empty legitimately.
func (a *App) recordGoogleListPull(trigger string, folder gmproto.ListConversationsRequest_Folder, firstPage bool, resp *gmproto.ListConversationsResponse, err error) {
	counted := firstPage && folder == gmproto.ListConversationsRequest_INBOX
	a.recordGooglePull(trigger, folder.String(), len(resp.GetConversations()), err, counted)
}

// recordGoogleLookupPull records a targeted conversation lookup, which should
// always return a conversation.
func (a *App) recordGoogleLookupPull(trigger string, conv *gmproto.Conversation, err error) {
	found := 0
	if conv != nil {
		found = 1
	}
	a.recordGooglePull(trigger, "", found, err, true)
}

func (a *App) recordGooglePull(trigger, folder string, count int, err error, counted bool) {
	outcome := classifyGooglePull(count, err)
	var payloadErr *libgm.ResponsePayloadError
	accountSwitch := errors.As(err, &payloadErr) && payloadErr.AccountSwitch
	dataless := outcome == GooglePullEmpty || outcome == GooglePullNoPayload

	// Count outside the lock: it reads a store.
	local := -1
	if counted && dataless {
		local = a.countGoogleConversations()
	}

	h := &a.googlePull
	h.mu.Lock()
	now := time.Now
	if h.now != nil {
		now = h.now
	}
	nowMS := now().UnixMilli()
	s := &h.snap
	h.recorded = true
	s.Threshold = googlePullEmptyThreshold
	s.LastAttemptMS = nowMS
	s.LastTrigger = trigger
	s.LastFolder = folder
	s.LastOutcome = outcome
	s.LastCount = count
	s.LastError = ""
	if err != nil {
		s.LastError = err.Error()
	}
	if outcome == GooglePullNoPayload {
		s.AccountSwitch = accountSwitch
	}
	raised := false
	if counted {
		switch {
		case outcome == GooglePullOK:
			s.ConsecutiveDataless = 0
			s.LastDataMS = nowMS
			s.AccountSwitch = false
			s.EmptyWithLocalHistory = false
		case dataless:
			s.ConsecutiveDataless++
			if local >= 0 {
				s.LocalConversations = local
			}
			was := s.EmptyWithLocalHistory
			s.EmptyWithLocalHistory = s.LocalConversations >= googlePullEmptyThreshold
			raised = s.EmptyWithLocalHistory && !was
		}
		// GooglePullError (transport, auth) is evidence of neither, so it
		// leaves the signal where it was.
	}
	snap := *s
	h.mu.Unlock()

	evt := a.Logger.Info()
	if dataless && counted {
		evt = a.Logger.Warn()
	}
	evt.Str("trigger", trigger).
		Str("folder", folder).
		Str("outcome", string(outcome)).
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
	if raised || (counted && outcome == GooglePullOK) {
		a.emitStatusChange(a.Connected.Load())
	}
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
