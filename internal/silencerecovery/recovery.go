// Package silencerecovery re-fetches the Google messages a stalled phone did
// not relay, once the stall ends.
//
// On 2026-10-06 the phone stopped relaying Google Messages for 38 hours while
// the long-poll stayed up. The silence check in internal/freshness flags such a
// stall in /api/status (stale_reason "silent"), but nothing fetched what the
// phone had created meanwhile: when push resumed, the newest stored message sat
// above the hole, so the recent reconcile stopped short, and the window had to
// be re-fetched by hand with POST /api/backfill {"since": ...}.
//
// A Recoverer watches the same activity clock the status payload judges
// silence by. When a silence that was stalled ends, which means the live
// channel delivered something after it (on a daemon whose readers use the
// legacy store, a catch-up that stores a newer message counts too), the
// Recoverer runs one guarded window
// backfill from just before the silence began, so the messages the phone
// created during it reach both stores. Its state lives in a small JSON file,
// so each silence gets one recovery across restarts; only a run cut off before
// its result was saved runs again.
package silencerecovery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/maxghenis/openmessage/internal/freshness"
)

// StateFileName is the state file's name inside the data directory.
const StateFileName = "google-silence-recovery.json"

// stateVersion is the state file's format version.
const stateVersion = 1

// Episode states.
const (
	// StatePending: the silence ended and a window backfill is owed.
	StatePending = "pending"
	// StateRunning: a window backfill for the episode is running. A state
	// file left in this state was cut off by a quit or crash; the next tick
	// counts it as a crashed attempt.
	StateRunning = "running"
	// StateRecovered: a window backfill returned data for the window, or
	// showed that no conversation had a message inside it. Earlier empty or
	// aborted attempts may already have fetched part of it.
	StateRecovered = "recovered"
	// StateGaveUp: len(Config.Backoff)+1 attempts were partial or crashed;
	// Notice says what to do.
	StateGaveUp = "gave_up"
	// StateWindowTooLarge: the window exceeded Config.MaxWindow, so nothing
	// was fetched automatically; Notice says what to do.
	StateWindowTooLarge = "window_too_large"
	// StateUnjudged: the gap's baseline could not be read until its window
	// exceeded Config.MaxWindow, so nobody knows whether it was stalled;
	// Notice says what to do.
	StateUnjudged = "unjudged"
)

// Attempt outcomes.
const (
	// OutcomeRecovered: the run listed conversations and fetched the window
	// without errors.
	OutcomeRecovered = "recovered"
	// OutcomeNothingInWindow: listings returned data, but no conversation had
	// a message inside the window, so nothing was missing.
	OutcomeNothingInWindow = "nothing_in_window"
	// OutcomeEmpty: the pull path answered with nothing (the 2026-10-07
	// defect): the run's first INBOX listing came back without a payload,
	// every listing came back empty without an error, or every in-window
	// conversation fetched nothing.
	OutcomeEmpty = "empty"
	// OutcomePartial: listings, fetches or store writes failed, the INBOX
	// listing came back empty while other folders listed conversations, an
	// in-window conversation fetched nothing while others worked, or, when
	// readers use v2, history did not reach it.
	OutcomePartial = "partial"
	// OutcomeAborted: the run stopped early because the client changed or
	// disconnected, or Google rejected the session.
	OutcomeAborted = "aborted"
	// OutcomeCrashed: the run panicked, or the daemon stopped during it.
	OutcomeCrashed = "crashed"
)

// Config tunes the Recoverer.
type Config struct {
	// Platform is the activity platform key to watch ("google").
	Platform string
	// Silence is the stall rule. It must match what /api/status uses so
	// "flagged" means the same thing in both places.
	Silence freshness.SilenceConfig
	// Location is the local zone the silence profile is built in.
	Location *time.Location
	// Interval is how often the activity clock is checked.
	Interval time.Duration
	// Settle delays the first attempt after a silence ends, so the backlog
	// the phone pushes when it resumes, and any reconnect catch-up, usually
	// land first. Nothing waits for them: the reconnect reconcile does not
	// take the backfill guard and may run alongside; a backfill that holds
	// the guard makes the attempt retry after BusyRetry.
	Settle time.Duration
	// Margin moves the window start this far before the silence's last
	// event, for clock skew between the receipt clock and message times.
	Margin time.Duration
	// MaxWindow bounds the window (now minus its start) an automatic run may
	// fetch. A longer window gets a notice instead.
	MaxWindow time.Duration
	// BusyRetry is how soon to try again when another backfill holds the
	// guard or the client went away between the readiness check and the run.
	BusyRetry time.Duration
	// Backoff[i] is the wait after attempt i+1 when it did not recover the
	// silence; later attempts wait the last entry. An episode is given up
	// after len(Backoff)+1 partial or crashed attempts. Empty attempts (the
	// pull path returned no data) and aborted ones (the connection changed)
	// never give up: the episode keeps trying until its window exceeds
	// MaxWindow, so it recovers on its own once the pull path works again.
	Backoff []time.Duration
	// PersistEvery bounds how stale the persisted watermark may get. New,
	// started, finished and merged episodes are persisted at once; failure
	// counts of unjudged gaps and the record of a run cut off by a crash
	// ride this periodic save.
	PersistEvery time.Duration
	// HistoryLimit is how many finished episodes the state file keeps.
	HistoryLimit int
	// RequireV2 makes a run that fetched messages but handed none to v2
	// ingest count as partial. Set it when readers use v2.
	RequireV2 bool
}

// DefaultConfig returns the production configuration for Google.
func DefaultConfig() Config {
	return Config{
		Platform:     "google",
		Silence:      freshness.DefaultSilenceConfig,
		Location:     time.Local,
		Interval:     time.Minute,
		Settle:       2 * time.Minute,
		Margin:       time.Hour,
		MaxWindow:    7 * 24 * time.Hour,
		BusyRetry:    2 * time.Minute,
		Backoff:      []time.Duration{5 * time.Minute, 15 * time.Minute, 30 * time.Minute, time.Hour, 2 * time.Hour, 4 * time.Hour},
		PersistEvery: 5 * time.Minute,
		HistoryLimit: 20,
	}
}

// RunResult is one window backfill run as the Runner reports it.
type RunResult struct {
	// Connected is false when no Google client was connected, so nothing ran.
	Connected bool
	// Aborted is set when the run stopped early.
	Aborted bool
	// Listed counts the distinct conversations the folder listings returned.
	Listed int
	// Conversations counts the listed conversations inside the window.
	Conversations int
	// Messages counts the messages fetched from them.
	Messages int
	// EmptyConversations counts in-window conversations whose fetch
	// succeeded with no message although their last message is inside the
	// window.
	EmptyConversations int
	// Errors counts failed listings, fetches and legacy-store writes.
	Errors int
	// HistoryTeed and HistoryTeeFailed count hand-offs to v2 ingest.
	HistoryTeed      int
	HistoryTeeFailed int
	// InboxOutcome is how the run's first INBOX listing went, classified the
	// way Google pull health classifies a counted pull: InboxOK, InboxEmpty,
	// InboxNoPayload or InboxError, or "" when the run never listed INBOX.
	InboxOutcome string
}

// Inbox listing outcomes, as Google pull health names them.
const (
	InboxOK        = "ok"
	InboxEmpty     = "empty"
	InboxNoPayload = "no_payload"
	InboxError     = "error"
)

// Runner runs window backfills.
type Runner interface {
	// Ready reports whether Google can serve a window backfill now. When it
	// cannot, reason names why ("disconnected", "phone_not_responding", ...).
	// The Recoverer waits rather than reconnecting or re-pairing.
	Ready() (ready bool, reason string)
	// RunWindowBackfill runs a guarded window backfill from since on the
	// calling goroutine. started is false, and nothing ran, when another
	// backfill holds the guard.
	RunWindowBackfill(since time.Time) (result RunResult, started bool)
}

// Flag records that the current silence has been judged stalled.
type Flag struct {
	LastEventMS int64  `json:"last_event_ms"`
	FlaggedAtMS int64  `json:"flagged_at_ms"`
	Rule        string `json:"rule"`
}

// Attempt is one window backfill attempt for an episode.
type Attempt struct {
	StartedAtMS        int64  `json:"started_at_ms"`
	FinishedAtMS       int64  `json:"finished_at_ms"`
	Outcome            string `json:"outcome"`
	Detail             string `json:"detail,omitempty"`
	Listed             int    `json:"listed"`
	Conversations      int    `json:"conversations"`
	Messages           int    `json:"messages"`
	EmptyConversations int    `json:"empty_conversations"`
	Errors             int    `json:"errors"`
	HistoryTeed        int    `json:"history_teed"`
	HistoryTeeFailed   int    `json:"history_tee_failed"`
	InboxOutcome       string `json:"inbox_outcome,omitempty"`
}

// Episode is one stalled silence and its recovery.
type Episode struct {
	// LastEventMS is the last activity before the silence. It identifies
	// the episode: each silence is queued once and gets one recovery.
	LastEventMS int64 `json:"last_event_ms"`
	// EndedAtMS is the first activity after the silence.
	EndedAtMS int64 `json:"ended_at_ms"`
	// SilentMS is EndedAtMS minus LastEventMS.
	SilentMS int64 `json:"silent_ms"`
	// Rule is the stall rule that flagged it.
	Rule         string `json:"rule"`
	DetectedAtMS int64  `json:"detected_at_ms"`
	// SinceMS is the window start: LastEventMS minus Config.Margin. The
	// episode is always owned by the earliest silence it covers (a silence
	// judged late can take over and move the start back), so every covered
	// silence's window is inside.
	SinceMS  int64  `json:"since_ms"`
	State    string `json:"state"`
	Attempts int    `json:"attempts"`
	// Failures counts attempts that were partial or crashed. Empty and
	// aborted attempts don't count: they keep probing until MaxWindow.
	Failures      int   `json:"failures,omitempty"`
	NextAttemptMS int64 `json:"next_attempt_ms,omitempty"`
	// RunningSinceMS is when the attempt now running started; zero otherwise.
	RunningSinceMS int64    `json:"running_since_ms,omitempty"`
	Waiting        string   `json:"waiting,omitempty"`
	LastAttempt    *Attempt `json:"last_attempt,omitempty"`
	// Covers lists later silences that ended while this one was still owed.
	// This episode's window includes theirs. If it is finished without a
	// fetch (window too large), they are owed again on their own windows.
	Covers       []Covered `json:"covers,omitempty"`
	Notice       string    `json:"notice,omitempty"`
	FinishedAtMS int64     `json:"finished_at_ms,omitempty"`
}

// Covered is a later silence merged into a pending episode.
type Covered struct {
	LastEventMS int64  `json:"last_event_ms"`
	EndedAtMS   int64  `json:"ended_at_ms"`
	Rule        string `json:"rule"`
}

// State is the persisted state.
type State struct {
	Version int `json:"version"`
	// WatermarkMS is the newest activity time already scanned for silences.
	WatermarkMS int64     `json:"watermark_ms"`
	Flagged     *Flag     `json:"flagged,omitempty"`
	Pending     *Episode  `json:"pending,omitempty"`
	History     []Episode `json:"history,omitempty"`
	// Unjudged holds gaps long enough to be stalled whose baseline could not
	// be read. Each tick retries up to maxUnjudgedPerTick of them, longest
	// untried first, until a verdict comes in or their window exceeds
	// MaxWindow.
	Unjudged []Gap `json:"unjudged,omitempty"`
}

// Gap is a silence between two activity times that still needs a verdict.
type Gap struct {
	LastEventMS int64  `json:"last_event_ms"`
	EndedAtMS   int64  `json:"ended_at_ms"`
	Failures    int    `json:"failures"`
	LastError   string `json:"last_error,omitempty"`
}

// Recoverer watches a platform's activity clock and recovers each stalled
// silence once it ends. Tick is the whole state machine; Start drives it.
type Recoverer struct {
	cfg      Config
	source   freshness.ActivitySource
	runner   Runner
	path     string
	logger   zerolog.Logger
	now      func() time.Time
	minStall time.Duration

	tickMu sync.Mutex // serializes Tick

	mu          sync.Mutex // guards the fields below
	state       State
	dirty       bool
	persistedAt time.Time
	loadErr     string
	saveErr     string
	activityErr string
	baselineErr string
	// readOnly is set when an unreadable state file could not be preserved;
	// saving then would destroy it, so nothing is saved.
	readOnly bool

	// baselines caches baseline events by last-event ms; only Tick uses it.
	baselines map[int64][]time.Time
}

// NoticeTTL is how long a finished episode's notice stays in the status. The
// history keeps such episodes this long even past Config.HistoryLimit.
const NoticeTTL = 14 * 24 * time.Hour

// maxUnjudgedPerTick bounds the baseline reads retrying unjudged gaps in one
// tick.
const maxUnjudgedPerTick = 3

// New builds a Recoverer whose state lives at statePath (empty keeps it in
// memory only). It loads any existing state. An unreadable file is logged,
// reported in Snapshot.StateLoadError and moved or copied aside; if neither
// works it is left alone and nothing is saved over it.
func New(
	cfg Config,
	source freshness.ActivitySource,
	runner Runner,
	statePath string,
	logger zerolog.Logger,
) *Recoverer {
	if cfg.Location == nil {
		cfg.Location = time.Local
	}
	r := &Recoverer{
		cfg:       cfg,
		source:    source,
		runner:    runner,
		path:      statePath,
		logger:    logger.With().Str("component", "silence_recovery").Str("platform", cfg.Platform).Logger(),
		now:       time.Now,
		minStall:  MinStallDuration(cfg.Silence),
		baselines: map[int64][]time.Time{},
	}
	r.state = State{Version: stateVersion}
	if statePath != "" {
		state, err := loadState(statePath)
		switch {
		case err == nil:
			r.state = state
		case errors.Is(err, os.ErrNotExist):
		default:
			// Keep the unreadable file for inspection (a newer version's
			// state survives a downgrade this way) and start fresh. If it
			// can't be moved or copied aside, never save over it.
			aside, preserveErr := preserveUnreadable(statePath, time.Now())
			if preserveErr == nil {
				r.loadErr = fmt.Sprintf("%v (moved to %s)", err, filepath.Base(aside))
			} else {
				r.readOnly = true
				r.loadErr = fmt.Sprintf("%v (could not preserve it: %v; the state file is left untouched and nothing is saved)", err, preserveErr)
			}
			r.logger.Error().Err(err).Str("path", statePath).Msg("Silence recovery: state file unreadable; starting fresh")
		}
	}
	if p := r.state.Pending; p != nil && p.State != StatePending && p.State != StateRunning {
		// Only pending and running episodes are ever saved as pending; move
		// anything else to the history so it can't absorb later silences.
		r.finishLocked(time.UnixMilli(p.FinishedAtMS))
		r.dirty = true
	}
	return r
}

// MinStallDuration is the shortest silence cfg can judge stalled. The
// expected-activity score never exceeds the silence's length in hours, so a
// silence shorter than ExpectedActiveHoursLimit hours, MaxSilence and
// LongSilence (each when enabled) is never stalled. It returns MaxInt64 when
// no rule is enabled.
func MinStallDuration(cfg freshness.SilenceConfig) time.Duration {
	shortest := time.Duration(math.MaxInt64)
	if cfg.ExpectedActiveHoursLimit > 0 {
		limit := cfg.ExpectedActiveHoursLimit * float64(time.Hour)
		if limit < float64(math.MaxInt64) {
			shortest = time.Duration(limit)
		}
	}
	if cfg.MaxSilence > 0 && cfg.MaxSilence < shortest {
		shortest = cfg.MaxSilence
	}
	if cfg.LongSilence > 0 && cfg.LongSilence < shortest {
		shortest = cfg.LongSilence
	}
	return shortest
}

// Start runs Tick every Config.Interval until ctx ends. A panicking tick is
// logged and the loop continues.
func (r *Recoverer) Start(ctx context.Context) {
	interval := r.cfg.Interval
	if interval <= 0 {
		interval = time.Minute
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			r.safeTick(ctx)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

func (r *Recoverer) safeTick(ctx context.Context) {
	defer func() {
		if recovered := recover(); recovered != nil {
			r.logger.Error().
				Interface("panic", recovered).
				Bytes("stack", debug.Stack()).
				Msg("Silence recovery: tick panicked; will retry next interval")
		}
	}()
	r.Tick(ctx)
}

// activityQueryTimeout bounds each activity query.
const activityQueryTimeout = 10 * time.Second

// Tick observes the activity clock once, records a silence that is stalled
// or has just ended, and runs a due window backfill. A run blocks Tick until it
// finishes.
func (r *Recoverer) Tick(ctx context.Context) {
	r.tickMu.Lock()
	defer r.tickMu.Unlock()

	r.retryUnjudged(ctx, r.now())
	r.observe(ctx, r.now())
	if ctx.Err() == nil {
		if plan := r.beginAttempt(r.now()); plan != nil {
			r.runAttempt(plan)
		}
	}
	r.persistIfDue(r.now())
}

// runAttempt runs a planned attempt and records its result. A run that
// panics is recorded as a crashed attempt before the panic continues, so the
// episode is retried on the backoff schedule instead of staying "running"
// until the next restart.
func (r *Recoverer) runAttempt(plan *attemptPlan) {
	returned := false
	defer func() {
		if !returned {
			r.finishCrashed(plan.key, plan.startedAt.UnixMilli(), "the run panicked", r.now())
		}
	}()
	result, started := r.runner.RunWindowBackfill(plan.since)
	returned = true
	r.finishAttempt(plan, result, started, r.now())
}

// detected is a stalled silence found by observe.
type detected struct {
	last time.Time
	end  time.Time
	rule string
}

// observe advances the watermark over new activity, recording each stalled
// gap it crosses as an episode, or, when nothing new arrived, flags the
// current silence once it is stalled.
func (r *Recoverer) observe(ctx context.Context, now time.Time) {
	if r.source == nil {
		return
	}
	latestByPlatform, err := r.queryLatest(ctx)
	if err != nil {
		r.noteActivityError(err)
		return
	}
	r.noteActivityError(nil)
	latest := latestByPlatform[r.cfg.Platform]
	if latest.IsZero() {
		return
	}
	r.mu.Lock()
	watermarkMS := r.state.WatermarkMS
	flagged := r.state.Flagged
	r.mu.Unlock()

	if watermarkMS <= 0 {
		// First run: nothing before now was watched, so start here.
		r.mu.Lock()
		r.state.WatermarkMS = latest.UnixMilli()
		r.dirty = true
		r.mu.Unlock()
		return
	}
	watermark := time.UnixMilli(watermarkMS)
	flaggedHere := flagged != nil && flagged.LastEventMS == watermarkMS

	if latest.UnixMilli() <= watermarkMS {
		// Still quiet since the watermark: flag the silence once it stalls.
		if flaggedHere || now.Sub(watermark) < r.minStall {
			return
		}
		verdict, err := r.judge(ctx, watermark, now)
		if err != nil {
			// Without its baseline the silence can't be flagged before the
			// floor rule; say so. Its end is judged again when traffic resumes.
			r.noteBaselineError(fmt.Errorf("baseline for the silence after %s: %w", watermark.Format(time.RFC3339), err))
		}
		if !verdict.Stalled {
			return
		}
		r.mu.Lock()
		r.state.Flagged = &Flag{LastEventMS: watermarkMS, FlaggedAtMS: now.UnixMilli(), Rule: verdict.Rule}
		r.mu.Unlock()
		r.save(now)
		r.logger.Info().
			Time("last_event", watermark).
			Dur("silent", verdict.Silence).
			Str("rule", verdict.Rule).
			Msg("Silence recovery: silence flagged; a window backfill will run once activity resumes")
		return
	}

	if !flaggedHere && latest.Sub(watermark) < r.minStall {
		// No gap inside a span shorter than minStall can be stalled: move on
		// without reading the events.
		r.mu.Lock()
		r.state.Flagged = nil
		r.state.WatermarkMS = latest.UnixMilli()
		r.dirty = true
		r.mu.Unlock()
		return
	}

	// Walk every gap since the watermark.
	events, err := r.queryBetween(ctx, watermark.Add(time.Millisecond), latest)
	if err != nil {
		r.noteActivityError(err)
		return
	}
	if len(events) == 0 || events[len(events)-1].Before(latest) {
		events = append(events, latest)
	}
	var found []detected
	var unjudged []Gap
	prev := watermark
	for _, event := range events {
		if !event.After(prev) {
			continue
		}
		switch {
		case prev.Equal(watermark) && flaggedHere:
			// Flagged while it lasted: no need to judge it again.
			found = append(found, detected{last: prev, end: event, rule: flagged.Rule})
		case event.Sub(prev) >= r.minStall:
			verdict, err := r.judge(ctx, prev, event)
			switch {
			case verdict.Stalled:
				found = append(found, detected{last: prev, end: event, rule: verdict.Rule})
			case err != nil:
				// Not stalled without its baseline, which can't be read: keep
				// the gap and judge it again each tick (retryUnjudged).
				unjudged = append(unjudged, Gap{LastEventMS: prev.UnixMilli(), EndedAtMS: event.UnixMilli(), Failures: 1, LastError: err.Error()})
				r.noteBaselineError(fmt.Errorf("baseline for the silence after %s: %w", prev.Format(time.RFC3339), err))
			}
		}
		prev = event
	}

	r.mu.Lock()
	for _, episode := range found {
		r.addEpisodeLocked(episode, now)
	}
	for _, gap := range unjudged {
		if !r.knownLocked(gap.LastEventMS) {
			r.state.Unjudged = append(r.state.Unjudged, gap)
		}
	}
	r.state.Flagged = nil
	r.state.WatermarkMS = latest.UnixMilli()
	r.dirty = true
	r.mu.Unlock()
	if len(found) > 0 || len(unjudged) > 0 {
		r.save(now)
	}
	r.pruneBaselines(latest.UnixMilli())
}

// retryUnjudged judges again the gaps whose baseline could not be read, up to
// maxUnjudgedPerTick a tick, longest untried first. A verdict settles a gap:
// a stalled one becomes an episode, any other is dropped. A gap whose window
// has passed MaxWindow gets one last read, outside the cap; if that fails too
// it is finished with a notice, since nothing would be fetched for it anyway.
func (r *Recoverer) retryUnjudged(ctx context.Context, now time.Time) {
	r.mu.Lock()
	gaps := append([]Gap(nil), r.state.Unjudged...)
	r.mu.Unlock()
	if len(gaps) == 0 || r.source == nil {
		return
	}
	var keep, tried []Gap
	var settled []detected
	var expired []Gap
	reads := 0
	for _, gap := range gaps {
		last, end := time.UnixMilli(gap.LastEventMS), time.UnixMilli(gap.EndedAtMS)
		expiring := r.cfg.MaxWindow > 0 && now.Sub(last.Add(-r.cfg.Margin)) > r.cfg.MaxWindow
		if !expiring && reads >= maxUnjudgedPerTick {
			keep = append(keep, gap)
			continue
		}
		// A gap about to expire gets one last read, outside the cap: a
		// verdict now beats a notice that it could not be judged.
		reads++
		verdict, err := r.judge(ctx, last, end)
		switch {
		case verdict.Stalled:
			settled = append(settled, detected{last: last, end: end, rule: verdict.Rule})
		case err != nil:
			gap.Failures++
			gap.LastError = err.Error()
			r.noteBaselineError(fmt.Errorf("baseline for the silence after %s: %w", last.Format(time.RFC3339), err))
			if expiring {
				expired = append(expired, gap)
			} else {
				tried = append(tried, gap)
			}
		default:
			// Judged and not stalled: nothing is owed.
		}
	}
	r.mu.Lock()
	// Gaps tried this tick go to the back, so a few that keep failing can't
	// starve the rest of their reads.
	r.state.Unjudged = append(keep, tried...)
	remaining := len(r.state.Unjudged)
	for _, found := range settled {
		r.addEpisodeLocked(found, now)
	}
	for _, gap := range expired {
		r.finishUnjudgedLocked(gap, now)
	}
	r.dirty = true
	r.mu.Unlock()
	// A settled or expired gap is saved at once; failure counts ride the
	// periodic save.
	if len(settled) > 0 || len(expired) > 0 || remaining < len(gaps) {
		r.save(now)
	}
	for _, gap := range gaps {
		delete(r.baselines, gap.LastEventMS)
	}
}

// finishUnjudgedLocked records a gap that could never be judged in the
// history, with a notice: its window is now too large to fetch automatically.
func (r *Recoverer) finishUnjudgedLocked(gap Gap, now time.Time) {
	since := time.UnixMilli(gap.LastEventMS).Add(-r.cfg.Margin)
	episode := Episode{
		LastEventMS:  gap.LastEventMS,
		EndedAtMS:    gap.EndedAtMS,
		SilentMS:     gap.EndedAtMS - gap.LastEventMS,
		DetectedAtMS: now.UnixMilli(),
		SinceMS:      since.UnixMilli(),
		State:        StateUnjudged,
		Notice: fmt.Sprintf(
			"The Google silence from %s to %s could not be judged: its activity baseline failed to load %d times (last: %s), and its window from %s is now longer than the %s automatic limit. If messages from then are missing, run POST /api/backfill {\"since\": %q} by hand.",
			formatTime(time.UnixMilli(gap.LastEventMS), r.cfg.Location),
			formatTime(time.UnixMilli(gap.EndedAtMS), r.cfg.Location),
			gap.Failures, gap.LastError,
			formatTime(since, r.cfg.Location),
			formatDuration(r.cfg.MaxWindow),
			since.In(r.cfg.Location).Format(time.RFC3339),
		),
		FinishedAtMS: now.UnixMilli(),
	}
	r.appendHistoryLocked(episode, now)
	r.logger.Error().Msg("Silence recovery: " + episode.Notice)
}

func (r *Recoverer) queryLatest(ctx context.Context) (map[string]time.Time, error) {
	queryCtx, cancel := context.WithTimeout(ctx, activityQueryTimeout)
	defer cancel()
	return r.source.Latest(queryCtx)
}

func (r *Recoverer) queryBetween(ctx context.Context, from, to time.Time) ([]time.Time, error) {
	queryCtx, cancel := context.WithTimeout(ctx, activityQueryTimeout)
	defer cancel()
	return r.source.Between(queryCtx, r.cfg.Platform, from, to)
}

// judge evaluates the silence from last to at against last's baseline. When
// the baseline can't be read it judges without one, as /api/status does, so
// only the floor rule can fire, and returns the read error.
func (r *Recoverer) judge(ctx context.Context, last, at time.Time) (freshness.SilenceVerdict, error) {
	key := last.UnixMilli()
	baseline, ok := r.baselines[key]
	var readErr error
	if !ok {
		from, to := freshness.BaselineRange(last, r.cfg.Location, r.cfg.Silence)
		events, err := r.queryBetween(ctx, from, to)
		if err != nil {
			readErr = err
		} else {
			baseline = events
			r.baselines[key] = events
			r.mu.Lock()
			r.baselineErr = ""
			r.mu.Unlock()
		}
	}
	return freshness.EvaluateSilence(last, baseline, at, r.cfg.Location, r.cfg.Silence), readErr
}

// pruneBaselines drops cached baselines for silences behind the watermark.
func (r *Recoverer) pruneBaselines(watermarkMS int64) {
	for key := range r.baselines {
		if key < watermarkMS {
			delete(r.baselines, key)
		}
	}
}

// noteBaselineError records a failed baseline read; a successful one clears
// it (judge).
func (r *Recoverer) noteBaselineError(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.baselineErr == "" {
		r.logger.Warn().Err(err).Msg("Silence recovery: baseline read failed; will retry")
	}
	r.baselineErr = err.Error()
}

func (r *Recoverer) noteActivityError(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err == nil {
		r.activityErr = ""
		return
	}
	if r.activityErr == "" {
		r.logger.Warn().Err(err).Msg("Silence recovery: activity query failed; will retry")
	}
	r.activityErr = err.Error()
}

// addEpisodeLocked records a stalled silence that ended. Each silence is
// identified by its last event: one already pending, covered or finished is
// ignored. A silence that ends while another is still owed joins that one's
// window rather than queueing a second run.
func (r *Recoverer) addEpisodeLocked(found detected, now time.Time) {
	key := found.last.UnixMilli()
	if r.knownLocked(key) {
		return
	}
	sinceMS := found.last.Add(-r.cfg.Margin).UnixMilli()
	if pending := r.state.Pending; pending != nil {
		joined := Covered{LastEventMS: key, EndedAtMS: found.end.UnixMilli(), Rule: found.rule}
		if key < pending.LastEventMS {
			// An earlier silence, judged late (its baseline could not be read
			// at first): it becomes the owner, and the window reaches back to
			// it. The pending silence and its covers join it.
			pending.Covers = append([]Covered{{
				LastEventMS: pending.LastEventMS,
				EndedAtMS:   pending.EndedAtMS,
				Rule:        pending.Rule,
			}}, pending.Covers...)
			pending.LastEventMS = key
			pending.EndedAtMS = joined.EndedAtMS
			pending.SilentMS = joined.EndedAtMS - key
			pending.Rule = found.rule
			pending.SinceMS = sinceMS
		} else {
			pending.Covers = append(pending.Covers, joined)
		}
		// The next attempt comes once every silence the window owns has
		// settled: sooner than a backoff would allow (there is new data to
		// fetch), but never before the latest of them settles.
		latestEnd := pending.EndedAtMS
		for _, c := range pending.Covers {
			if c.EndedAtMS > latestEnd {
				latestEnd = c.EndedAtMS
			}
		}
		next := time.UnixMilli(latestEnd).Add(r.cfg.Settle)
		if next.Before(now) {
			next = now
		}
		pending.NextAttemptMS = next.UnixMilli()
		r.logger.Info().
			Time("last_event", found.last).
			Time("ended", found.end).
			Int64("owner_last_event_ms", pending.LastEventMS).
			Time("since", time.UnixMilli(pending.SinceMS)).
			Msg("Silence recovery: another stalled silence ended; it joins the pending window backfill")
		return
	}
	next := found.end.Add(r.cfg.Settle)
	if next.Before(now) {
		next = now
	}
	r.state.Pending = &Episode{
		LastEventMS:   key,
		EndedAtMS:     found.end.UnixMilli(),
		SilentMS:      found.end.Sub(found.last).Milliseconds(),
		Rule:          found.rule,
		DetectedAtMS:  now.UnixMilli(),
		SinceMS:       sinceMS,
		State:         StatePending,
		NextAttemptMS: next.UnixMilli(),
	}
	r.logger.Info().
		Time("last_event", found.last).
		Time("ended", found.end).
		Dur("silent", found.end.Sub(found.last)).
		Str("rule", found.rule).
		Time("since", time.UnixMilli(sinceMS)).
		Time("first_attempt", next).
		Msg("Silence recovery: stalled silence ended; window backfill scheduled")
}

func (r *Recoverer) knownLocked(key int64) bool {
	if pending := r.state.Pending; pending != nil {
		if pending.LastEventMS == key || covers(pending.Covers, key) {
			return true
		}
	}
	for _, episode := range r.state.History {
		if episode.LastEventMS == key || covers(episode.Covers, key) {
			return true
		}
	}
	for _, gap := range r.state.Unjudged {
		if gap.LastEventMS == key {
			return true
		}
	}
	return false
}

func covers(covered []Covered, key int64) bool {
	for _, c := range covered {
		if c.LastEventMS == key {
			return true
		}
	}
	return false
}

// requeueLocked owes the covered silences again after the episode covering
// them finished without fetching anything: the earliest becomes pending, and
// it covers the rest. A window too large for it is too large for them only if
// it is too large on its own, which beginAttempt checks again.
func (r *Recoverer) requeueLocked(later []Covered, now time.Time) {
	if len(later) == 0 {
		return
	}
	sort.Slice(later, func(i, j int) bool { return later[i].LastEventMS < later[j].LastEventMS })
	first := later[0]
	next := time.UnixMilli(first.EndedAtMS).Add(r.cfg.Settle)
	if next.Before(now) {
		next = now
	}
	r.state.Pending = &Episode{
		LastEventMS:   first.LastEventMS,
		EndedAtMS:     first.EndedAtMS,
		SilentMS:      first.EndedAtMS - first.LastEventMS,
		Rule:          first.Rule,
		DetectedAtMS:  now.UnixMilli(),
		SinceMS:       time.UnixMilli(first.LastEventMS).Add(-r.cfg.Margin).UnixMilli(),
		State:         StatePending,
		NextAttemptMS: next.UnixMilli(),
		Covers:        append([]Covered(nil), later[1:]...),
	}
	r.logger.Info().
		Time("last_event", time.UnixMilli(first.LastEventMS)).
		Msg("Silence recovery: a silence the finished window covered is owed again on its own window")
}

// attemptPlan is an attempt beginAttempt started.
type attemptPlan struct {
	key       int64
	since     time.Time
	startedAt time.Time
}

// beginAttempt returns the attempt to run now, if one is due, after marking
// the episode running and persisting that. It finalizes an episode whose
// window is too large, and leaves one waiting while Google is not ready.
func (r *Recoverer) beginAttempt(now time.Time) *attemptPlan {
	r.mu.Lock()
	r.recoverInterruptedLocked(now)
	pending := r.state.Pending
	if pending == nil || pending.State != StatePending || now.UnixMilli() < pending.NextAttemptMS {
		r.mu.Unlock()
		return nil
	}
	since := time.UnixMilli(pending.SinceMS)
	if r.cfg.MaxWindow > 0 && now.Sub(since) > r.cfg.MaxWindow {
		pending.State = StateWindowTooLarge
		pending.Notice = fmt.Sprintf(
			"The Google silence from %s to %s needs a window backfill from %s, longer than the %s automatic limit, so nothing was re-fetched. Run it by hand: POST /api/backfill {\"since\": %q}.",
			formatTime(time.UnixMilli(pending.LastEventMS), r.cfg.Location),
			formatTime(time.UnixMilli(pending.EndedAtMS), r.cfg.Location),
			formatTime(since, r.cfg.Location),
			formatDuration(r.cfg.MaxWindow),
			since.In(r.cfg.Location).Format(time.RFC3339),
		)
		if last := pending.LastAttempt; last != nil {
			pending.Notice = fmt.Sprintf(
				"The Google silence from %s to %s was not recovered: %d attempts failed (last %s: %s), and its window from %s is now longer than the %s automatic limit. Run it by hand: POST /api/backfill {\"since\": %q}.",
				formatTime(time.UnixMilli(pending.LastEventMS), r.cfg.Location),
				formatTime(time.UnixMilli(pending.EndedAtMS), r.cfg.Location),
				pending.Attempts, last.Outcome, last.Detail,
				formatTime(since, r.cfg.Location),
				formatDuration(r.cfg.MaxWindow),
				since.In(r.cfg.Location).Format(time.RFC3339),
			)
		}
		episode := *pending
		later := pending.Covers
		pending.Covers = nil
		r.finishLocked(now)
		r.requeueLocked(later, now)
		r.mu.Unlock()
		r.save(now)
		r.logger.Error().
			Int64("last_event_ms", episode.LastEventMS).
			Time("since", since).
			Dur("window", now.Sub(since)).
			Msg("Silence recovery: " + episode.Notice)
		return nil
	}
	r.mu.Unlock()

	// Ask outside the lock: Ready reads app state.
	ready, reason := true, ""
	if r.runner != nil {
		ready, reason = r.runner.Ready()
	} else {
		ready, reason = false, "no_runner"
	}

	r.mu.Lock()
	pending = r.state.Pending
	if pending == nil || pending.State != StatePending {
		r.mu.Unlock()
		return nil
	}
	if !ready {
		changed := pending.Waiting != reason
		pending.Waiting = reason
		r.mu.Unlock()
		if changed {
			r.save(now)
			r.logger.Info().Str("reason", reason).Msg("Silence recovery: window backfill owed; waiting for Google")
		}
		return nil
	}
	pending.Waiting = ""
	pending.State = StateRunning
	pending.RunningSinceMS = now.UnixMilli()
	pending.Attempts++
	plan := &attemptPlan{key: pending.LastEventMS, since: since, startedAt: now}
	attempt := pending.Attempts
	r.mu.Unlock()
	if !r.save(now) {
		// Without the running state on disk, a run that killed the daemon
		// would not count against the episode, and could crash-loop it. Wait
		// until the state can be saved.
		r.mu.Lock()
		if p := r.state.Pending; p != nil && p.LastEventMS == plan.key {
			p.State = StatePending
			p.RunningSinceMS = 0
			p.Attempts--
			p.Waiting = "state_not_saved"
			p.NextAttemptMS = now.Add(r.cfg.BusyRetry).UnixMilli()
		}
		r.mu.Unlock()
		r.logger.Warn().Msg("Silence recovery: the state file can't be saved; not starting a window backfill until it can")
		return nil
	}
	r.logger.Info().
		Time("since", since).
		Int("attempt", attempt).
		Msg("Silence recovery: starting window backfill")
	return plan
}

// finishAttempt records an attempt's result and schedules what follows.
func (r *Recoverer) finishAttempt(plan *attemptPlan, result RunResult, started bool, now time.Time) {
	r.mu.Lock()
	pending := r.state.Pending
	if pending == nil || pending.LastEventMS != plan.key {
		r.mu.Unlock()
		return
	}
	pending.RunningSinceMS = 0
	if !started || !result.Connected {
		// Nothing ran: another backfill held the guard, or the client went
		// away after the readiness check. Not an attempt.
		pending.Attempts--
		pending.State = StatePending
		pending.NextAttemptMS = now.Add(r.cfg.BusyRetry).UnixMilli()
		if !started {
			pending.Waiting = "backfill_busy"
		} else {
			pending.Waiting = "disconnected"
		}
		waiting := pending.Waiting
		r.mu.Unlock()
		r.save(now)
		r.logger.Info().Str("reason", waiting).Dur("retry_in", r.cfg.BusyRetry).Msg("Silence recovery: window backfill did not run; will retry")
		return
	}
	outcome, detail := Classify(result, r.cfg.RequireV2)
	pending.LastAttempt = &Attempt{
		StartedAtMS:        plan.startedAt.UnixMilli(),
		FinishedAtMS:       now.UnixMilli(),
		Outcome:            outcome,
		Detail:             detail,
		Listed:             result.Listed,
		Conversations:      result.Conversations,
		Messages:           result.Messages,
		EmptyConversations: result.EmptyConversations,
		Errors:             result.Errors,
		HistoryTeed:        result.HistoryTeed,
		HistoryTeeFailed:   result.HistoryTeeFailed,
		InboxOutcome:       result.InboxOutcome,
	}
	attempts := pending.Attempts
	since := time.UnixMilli(pending.SinceMS)
	event := r.logger.Info()
	message := "Silence recovery: window backfill recovered the silence"
	switch outcome {
	case OutcomeRecovered, OutcomeNothingInWindow:
		pending.State = StateRecovered
		if outcome == OutcomeNothingInWindow {
			message = "Silence recovery: window backfill found no conversation inside the window; nothing was missing"
		}
		r.finishLocked(now)
	case OutcomeEmpty, OutcomeAborted:
		// The pull path answered with nothing, or the connection changed
		// under the run: both pass. Keep probing (an empty listing costs three
		// calls) until the window outgrows MaxWindow.
		wait := r.backoffAfter(attempts)
		pending.State = StatePending
		pending.NextAttemptMS = now.Add(wait).UnixMilli()
		event = r.logger.Warn().Dur("retry_in", wait)
		message = "Silence recovery: window backfill got nothing it could use; will try again"
	default:
		pending.Failures++
		if pending.Failures > len(r.cfg.Backoff) {
			pending.State = StateGaveUp
			pending.Notice = r.giveUpNotice(pending, detail)
			event = r.logger.Error()
			message = "Silence recovery: " + pending.Notice
			r.finishLocked(now)
			break
		}
		wait := r.backoffAfter(attempts)
		pending.State = StatePending
		pending.NextAttemptMS = now.Add(wait).UnixMilli()
		event = r.logger.Warn().Dur("retry_in", wait)
		message = "Silence recovery: window backfill did not recover the silence; will retry"
	}
	r.mu.Unlock()
	r.save(now)
	event.
		Time("since", since).
		Int("attempt", attempts).
		Str("outcome", outcome).
		Str("detail", detail).
		Str("inbox_outcome", result.InboxOutcome).
		Int("listed", result.Listed).
		Int("conversations", result.Conversations).
		Int("messages", result.Messages).
		Int("empty_conversations", result.EmptyConversations).
		Int("errors", result.Errors).
		Int("history_teed", result.HistoryTeed).
		Int("history_tee_failed", result.HistoryTeeFailed).
		Msg(message)
}

// finishCrashed records an attempt that panicked in this process. It counts
// as a failure and backs off, so a run that keeps crashing is given up
// instead of looping. A run cut off when the daemon stopped is recorded the
// same way on the next tick (recoverInterruptedLocked).
func (r *Recoverer) finishCrashed(key, startedMS int64, detail string, now time.Time) {
	r.mu.Lock()
	r.crashedLocked(key, startedMS, detail, now)
	r.mu.Unlock()
	r.save(now)
}

func (r *Recoverer) crashedLocked(key, startedMS int64, detail string, now time.Time) {
	pending := r.state.Pending
	if pending == nil || pending.LastEventMS != key {
		return
	}
	pending.State = StatePending
	pending.RunningSinceMS = 0
	pending.Failures++
	pending.LastAttempt = &Attempt{
		StartedAtMS:  startedMS,
		FinishedAtMS: now.UnixMilli(),
		Outcome:      OutcomeCrashed,
		Detail:       detail,
	}
	r.dirty = true
	r.logger.Warn().
		Int64("last_event_ms", pending.LastEventMS).
		Int("attempts", pending.Attempts).
		Int("failures", pending.Failures).
		Str("detail", detail).
		Msg("Silence recovery: a window backfill crashed")
	if pending.Failures > len(r.cfg.Backoff) {
		pending.State = StateGaveUp
		pending.Notice = r.giveUpNotice(pending, detail)
		r.logger.Error().Msg("Silence recovery: " + pending.Notice)
		r.finishLocked(now)
		return
	}
	pending.NextAttemptMS = now.Add(r.backoffAfter(pending.Attempts)).UnixMilli()
}

// recoverInterruptedLocked handles a run the previous process was in the
// middle of when it stopped: the state file still says running. Runs happen
// only inside Tick, so a running episode seen here is always such a leftover.
func (r *Recoverer) recoverInterruptedLocked(now time.Time) {
	pending := r.state.Pending
	if pending == nil || pending.State != StateRunning {
		return
	}
	r.crashedLocked(pending.LastEventMS, pending.RunningSinceMS, "the daemon stopped during the run", now)
}

// backoffAfter is the wait after the given number of attempts.
func (r *Recoverer) backoffAfter(attempts int) time.Duration {
	if len(r.cfg.Backoff) == 0 {
		return r.cfg.BusyRetry
	}
	i := attempts - 1
	if i < 0 {
		i = 0
	}
	if i >= len(r.cfg.Backoff) {
		i = len(r.cfg.Backoff) - 1
	}
	return r.cfg.Backoff[i]
}

func (r *Recoverer) giveUpNotice(episode *Episode, detail string) string {
	return fmt.Sprintf(
		"Automatic recovery of the Google silence from %s to %s gave up after %d partial or crashed attempts (last: %s). Once that is fixed, run POST /api/backfill {\"since\": %q} by hand.",
		formatTime(time.UnixMilli(episode.LastEventMS), r.cfg.Location),
		formatTime(time.UnixMilli(episode.EndedAtMS), r.cfg.Location),
		episode.Failures,
		detail,
		time.UnixMilli(episode.SinceMS).In(r.cfg.Location).Format(time.RFC3339),
	)
}

// finishLocked moves the pending episode into the history.
func (r *Recoverer) finishLocked(now time.Time) {
	pending := r.state.Pending
	if pending == nil {
		return
	}
	pending.FinishedAtMS = now.UnixMilli()
	pending.NextAttemptMS = 0
	pending.Waiting = ""
	pending.RunningSinceMS = 0
	r.state.Pending = nil
	r.appendHistoryLocked(*pending, now)
}

// appendHistoryLocked adds a finished episode to the front of the history
// and trims it to Config.HistoryLimit, keeping older episodes whose notice is
// still shown (NoticeTTL) so a burst of later episodes can't hide it.
func (r *Recoverer) appendHistoryLocked(episode Episode, now time.Time) {
	history := append([]Episode{episode}, r.state.History...)
	limit := r.cfg.HistoryLimit
	if limit > 0 && len(history) > limit {
		kept := append([]Episode(nil), history[:limit]...)
		for _, older := range history[limit:] {
			if noticeActive(older, now) {
				kept = append(kept, older)
			}
		}
		history = kept
	}
	r.state.History = history
}

// noticeActive reports whether a finished episode's notice still shows.
func noticeActive(episode Episode, now time.Time) bool {
	switch episode.State {
	case StateGaveUp, StateWindowTooLarge, StateUnjudged:
	default:
		return false
	}
	return episode.Notice != "" && episode.FinishedAtMS >= now.Add(-NoticeTTL).UnixMilli()
}

// Classify judges a run that started and had a client. A run counts as
// recovered only when the pull path demonstrably returned data. It is empty
// when the phone answered with nothing (the 2026-10-07 defect): the run's own
// first INBOX listing came back empty or without a payload (the counted pull
// Google pull health records, read off this run alone), the listings returned
// nothing without an error, or every in-window conversation fetched nothing.
// Failures and lost writes make it partial.
func Classify(result RunResult, requireV2 bool) (string, string) {
	switch {
	case result.Aborted:
		return OutcomeAborted, "the run stopped early: the client changed or disconnected, or Google rejected the session"
	case result.InboxOutcome == InboxNoPayload:
		return OutcomeEmpty, "the phone answered the INBOX listing without a payload"
	case result.InboxOutcome == InboxEmpty && result.Listed == 0 && result.Errors == 0:
		return OutcomeEmpty, "the phone listed no conversations in any folder; its request/response calls return no data"
	case result.InboxOutcome == InboxEmpty:
		// Other folders answered with data, so the pull path works; the inbox
		// may really be empty, or its listing broken. Bounded, not probed.
		return OutcomePartial, fmt.Sprintf("the INBOX listing came back empty while other folders listed %d conversations; if the inbox really is empty, nothing was missed", result.Listed)
	case result.InboxOutcome == InboxError:
		return OutcomePartial, fmt.Sprintf("the INBOX listing failed (%d errors in all)", result.Errors)
	case result.Listed == 0 && result.Errors == 0:
		return OutcomeEmpty, "the phone listed no conversations at all; its request/response calls return no data"
	case result.Listed == 0:
		return OutcomePartial, fmt.Sprintf("the conversation listings failed (%d errors)", result.Errors)
	case result.Conversations > 0 && result.Messages == 0 && result.Errors == 0:
		return OutcomeEmpty, fmt.Sprintf("all %d in-window conversations fetched no messages", result.Conversations)
	case result.Errors > 0 || (requireV2 && result.HistoryTeeFailed > 0) || result.EmptyConversations > 0:
		return OutcomePartial, fmt.Sprintf(
			"%d listing, fetch or store errors, %d in-window conversations fetched nothing, %d hand-offs to v2 failed",
			result.Errors, result.EmptyConversations, result.HistoryTeeFailed)
	case requireV2 && result.Messages > 0 && result.HistoryTeed == 0:
		return OutcomePartial, "fetched messages did not reach v2 ingest"
	case result.Conversations == 0:
		return OutcomeNothingInWindow, "no conversation had a message inside the window"
	default:
		return OutcomeRecovered, fmt.Sprintf("%d conversations, %d messages", result.Conversations, result.Messages)
	}
}

// Snapshot is the status view of the Recoverer.
type Snapshot struct {
	Enabled     bool   `json:"enabled"`
	Source      string `json:"source,omitempty"`
	WatermarkMS int64  `json:"watermark_ms"`
	// Flagged is set while the current silence is stalled: a window backfill
	// will run once activity resumes.
	Flagged *Flag    `json:"flagged,omitempty"`
	Pending *Episode `json:"pending,omitempty"`
	// Unjudged lists gaps whose baseline could not be read yet.
	Unjudged []Gap `json:"unjudged,omitempty"`
	// Last is the most recently finished episode.
	Last *Episode `json:"last,omitempty"`
	// Notice is the newest notice of a finished episode that needs a person
	// (gave up, window too large, or never judged) within NoticeTTL; Notices
	// lists them all, newest first.
	Notice      string   `json:"notice,omitempty"`
	Notices     []string `json:"notices,omitempty"`
	RequireV2   bool     `json:"require_v2"`
	MarginMS    int64    `json:"margin_ms"`
	MaxWindowMS int64    `json:"max_window_ms"`
	SettleMS    int64    `json:"settle_ms"`
	// MaxFailedAttempts is how many partial or crashed attempts an episode
	// gets before it is given up; empty and aborted attempts keep trying
	// until MaxWindow.
	MaxFailedAttempts int    `json:"max_failed_attempts"`
	StatePath         string `json:"state_path,omitempty"`
	// StateError is the latest failure to save the state file.
	StateError string `json:"state_error,omitempty"`
	// StateLoadError is why the state file could not be read at startup. The
	// file was moved or copied aside and the Recoverer started fresh, or, if
	// neither worked, it was left in place and nothing is saved.
	StateLoadError string `json:"state_load_error,omitempty"`
	// ActivityError is the latest failure to read the activity clock, until
	// a read succeeds.
	ActivityError string `json:"activity_error,omitempty"`
	// BaselineError is the latest failure to read a silence's baseline, until
	// a baseline read succeeds.
	BaselineError string `json:"baseline_error,omitempty"`
}

// Snapshot returns the current status view.
func (r *Recoverer) Snapshot() Snapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	snap := Snapshot{
		Enabled:           true,
		WatermarkMS:       r.state.WatermarkMS,
		MarginMS:          r.cfg.Margin.Milliseconds(),
		MaxWindowMS:       r.cfg.MaxWindow.Milliseconds(),
		SettleMS:          r.cfg.Settle.Milliseconds(),
		MaxFailedAttempts: len(r.cfg.Backoff) + 1,
		StatePath:         r.path,
		StateError:        r.saveErr,
		StateLoadError:    r.loadErr,
		ActivityError:     r.activityErr,
		BaselineError:     r.baselineErr,
		RequireV2:         r.cfg.RequireV2,
	}
	if r.source != nil {
		snap.Source = r.source.Name()
	}
	if r.state.Flagged != nil {
		flag := *r.state.Flagged
		snap.Flagged = &flag
	}
	if r.state.Pending != nil {
		snap.Pending = copyEpisode(r.state.Pending)
	}
	if len(r.state.History) > 0 {
		snap.Last = copyEpisode(&r.state.History[0])
	}
	snap.Unjudged = append([]Gap(nil), r.state.Unjudged...)
	now := r.now()
	for _, episode := range r.state.History {
		if noticeActive(episode, now) {
			snap.Notices = append(snap.Notices, episode.Notice)
		}
	}
	if len(snap.Notices) > 0 {
		snap.Notice = snap.Notices[0]
	}
	return snap
}

// State returns a copy of the persisted state.
func (r *Recoverer) State() State {
	r.mu.Lock()
	defer r.mu.Unlock()
	return copyState(r.state)
}

func copyEpisode(episode *Episode) *Episode {
	cp := *episode
	if episode.LastAttempt != nil {
		attempt := *episode.LastAttempt
		cp.LastAttempt = &attempt
	}
	cp.Covers = append([]Covered(nil), episode.Covers...)
	return &cp
}

func copyState(state State) State {
	cp := state
	if state.Flagged != nil {
		flag := *state.Flagged
		cp.Flagged = &flag
	}
	if state.Pending != nil {
		cp.Pending = copyEpisode(state.Pending)
	}
	cp.Unjudged = append([]Gap(nil), state.Unjudged...)
	cp.History = make([]Episode, len(state.History))
	for i := range state.History {
		cp.History[i] = *copyEpisode(&state.History[i])
	}
	return cp
}

// persistIfDue saves a moved watermark at most every PersistEvery. A lost
// watermark only means the next start rescans a few minutes of activity, and
// rescanning finds the same episodes, which are already known.
func (r *Recoverer) persistIfDue(now time.Time) {
	r.mu.Lock()
	due := r.dirty && (r.persistedAt.IsZero() || now.Sub(r.persistedAt) >= r.cfg.PersistEvery)
	r.mu.Unlock()
	if due {
		r.save(now)
	}
}

// save writes the state file now.
// It reports whether the state is now durable: true in memory-only mode,
// false when the save failed or the file is left alone (read-only).
func (r *Recoverer) save(now time.Time) bool {
	r.mu.Lock()
	state := copyState(r.state)
	readOnly := r.readOnly
	r.mu.Unlock()
	if r.path == "" || readOnly {
		r.mu.Lock()
		r.dirty = false
		r.persistedAt = now
		r.mu.Unlock()
		return r.path == ""
	}
	err := saveState(r.path, state)
	r.mu.Lock()
	defer r.mu.Unlock()
	if err != nil {
		if r.saveErr == "" {
			r.logger.Error().Err(err).Str("path", r.path).Msg("Silence recovery: could not save state")
		}
		r.saveErr = err.Error()
		// Retry on the next tick, not only when something else changes.
		r.dirty = true
		r.persistedAt = time.Time{}
		return false
	}
	r.saveErr = ""
	r.dirty = false
	r.persistedAt = now
	return true
}

// renameUnreadable moves an unreadable state file aside; tests replace it to
// exercise the copy fallback.
var renameUnreadable = os.Rename

// preserveUnreadable moves an unreadable state file to a name of its own
// (path.unreadable-<unix ms>), or copies it there when it can't be moved, so
// the fresh state saved next never overwrites it. It returns the new name.
func preserveUnreadable(path string, now time.Time) (string, error) {
	aside := fmt.Sprintf("%s.unreadable-%d", path, now.UnixMilli())
	for i := 1; ; i++ {
		_, err := os.Lstat(aside)
		if errors.Is(err, os.ErrNotExist) {
			break
		}
		if err != nil {
			return "", err
		}
		aside = fmt.Sprintf("%s.unreadable-%d-%d", path, now.UnixMilli(), i)
	}
	if err := renameUnreadable(path, aside); err == nil {
		return aside, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(aside, data, 0o600); err != nil {
		return "", err
	}
	return aside, nil
}

func loadState(path string) (State, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return State{}, err
	}
	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return State{}, fmt.Errorf("decode %s: %w", filepath.Base(path), err)
	}
	if state.Version != stateVersion {
		return State{}, fmt.Errorf("%s has version %d, want %d", filepath.Base(path), state.Version, stateVersion)
	}
	sort.SliceStable(state.History, func(i, j int) bool {
		return state.History[i].FinishedAtMS > state.History[j].FinishedAtMS
	})
	return state, nil
}

// syncStateFile flushes the temporary state file before it replaces the old
// one. Tests that save thousands of times swap it out: on macOS it is a full
// device flush.
var syncStateFile = (*os.File).Sync

// saveState writes state atomically: a synced temporary file renamed over
// the old one, so a crash leaves either the old state or the new.
func saveState(path string, state State) error {
	state.Version = stateVersion
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := syncStateFile(tmp); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	// Make the rename itself durable; best effort, as some filesystems
	// refuse to sync a directory.
	if dirFile, err := os.Open(dir); err == nil {
		_ = syncStateFile(dirFile)
		dirFile.Close()
	}
	return nil
}

func formatTime(t time.Time, loc *time.Location) string {
	if loc == nil {
		loc = time.Local
	}
	return t.In(loc).Format("2006-01-02 15:04 MST")
}

func formatDuration(d time.Duration) string {
	if d%(24*time.Hour) == 0 {
		days := int(d / (24 * time.Hour))
		if days == 1 {
			return "1-day"
		}
		return fmt.Sprintf("%d-day", days)
	}
	return strings.TrimSuffix(d.String(), "0s")
}
