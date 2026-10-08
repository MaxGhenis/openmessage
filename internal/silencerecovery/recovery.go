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
// channel delivered something after it, the Recoverer runs one guarded window
// backfill from just before the silence began, so the messages the phone
// created during it reach both stores. Its state lives in a small JSON file,
// so each silence is recovered at most once across restarts.
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
	// file left in this state by a crash reads back as pending.
	StateRunning = "running"
	// StateRecovered: a window backfill returned data for the window.
	StateRecovered = "recovered"
	// StateGaveUp: every allowed attempt failed; Notice says what to do.
	StateGaveUp = "gave_up"
	// StateWindowTooLarge: the window exceeded Config.MaxWindow, so nothing
	// was fetched automatically; Notice says what to do.
	StateWindowTooLarge = "window_too_large"
)

// Attempt outcomes.
const (
	// OutcomeRecovered: the run listed conversations and fetched the window
	// without errors.
	OutcomeRecovered = "recovered"
	// OutcomeNothingInWindow: listings returned data, but no conversation had
	// a message inside the window, so nothing was missing.
	OutcomeNothingInWindow = "nothing_in_window"
	// OutcomeEmpty: the pull path returned no data (the 2026-10-07 defect):
	// listings came back empty, pull health flagged a data-less pull, or an
	// in-window conversation's fetch returned no messages.
	OutcomeEmpty = "empty"
	// OutcomePartial: data came back, but some listings or fetches failed or
	// some history did not reach v2.
	OutcomePartial = "partial"
	// OutcomeAborted: the run stopped early (client change, disconnect, or
	// Google rejected the session).
	OutcomeAborted = "aborted"
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
	// the phone pushes when it resumes lands first and the startup or
	// reconnect catch-ups finish.
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
	// Backoff[i] is the wait after failed attempt i+1. When it runs out the
	// episode is given up, so at most len(Backoff)+1 attempts run.
	Backoff []time.Duration
	// PersistEvery bounds how stale the persisted watermark may get. Episode
	// changes are always persisted at once.
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
	// EmptyConversations counts in-window conversations whose fetch returned
	// no message although their last message is inside the window.
	EmptyConversations int
	// Errors counts failed listings and fetches.
	Errors int
	// HistoryTeed and HistoryTeeFailed count hand-offs to v2 ingest.
	HistoryTeed      int
	HistoryTeeFailed int
}

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

// PullHealth is the part of the Google pull health snapshot the Recoverer
// reads after a run.
type PullHealth struct {
	// ConsecutiveDataless counts counted pulls in a row that returned no data.
	ConsecutiveDataless int
	// EmptyWithLocalHistory is set while the latest counted pull returned no
	// data although the store holds this account's conversations.
	EmptyWithLocalHistory bool
}

// PullHealthFunc returns the current pull health, or nil before any pull.
type PullHealthFunc func() *PullHealth

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
	PullDataless       bool   `json:"pull_dataless"`
}

// Episode is one stalled silence and its recovery.
type Episode struct {
	// LastEventMS is the last activity before the silence. It identifies
	// the episode: each one is recovered at most once.
	LastEventMS int64 `json:"last_event_ms"`
	// EndedAtMS is the first activity after the silence.
	EndedAtMS int64 `json:"ended_at_ms"`
	// SilentMS is EndedAtMS minus LastEventMS.
	SilentMS int64 `json:"silent_ms"`
	// Rule is the stall rule that flagged it.
	Rule         string `json:"rule"`
	DetectedAtMS int64  `json:"detected_at_ms"`
	// SinceMS is the window start: LastEventMS minus Config.Margin, or
	// earlier when a later episode was merged in.
	SinceMS       int64    `json:"since_ms"`
	State         string   `json:"state"`
	Attempts      int      `json:"attempts"`
	NextAttemptMS int64    `json:"next_attempt_ms,omitempty"`
	Waiting       string   `json:"waiting,omitempty"`
	LastAttempt   *Attempt `json:"last_attempt,omitempty"`
	// Covers lists later episodes (by LastEventMS) that ended while this one
	// was still owed; its window includes theirs.
	Covers       []int64 `json:"covers,omitempty"`
	Notice       string  `json:"notice,omitempty"`
	FinishedAtMS int64   `json:"finished_at_ms,omitempty"`
}

// State is the persisted state.
type State struct {
	Version int `json:"version"`
	// WatermarkMS is the newest activity time already scanned for silences.
	WatermarkMS int64     `json:"watermark_ms"`
	Flagged     *Flag     `json:"flagged,omitempty"`
	Pending     *Episode  `json:"pending,omitempty"`
	History     []Episode `json:"history,omitempty"`
}

// Recoverer watches a platform's activity clock and recovers each stalled
// silence once it ends. Tick is the whole state machine; Start drives it.
type Recoverer struct {
	cfg      Config
	source   freshness.ActivitySource
	runner   Runner
	pull     PullHealthFunc
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

	// baselines caches baseline events by last-event ms; only Tick uses it.
	baselines map[int64][]time.Time
}

// New builds a Recoverer whose state lives at statePath (empty keeps it in
// memory only). It loads any existing state; an unreadable file is logged and
// replaced on the next save.
func New(
	cfg Config,
	source freshness.ActivitySource,
	runner Runner,
	pull PullHealthFunc,
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
		pull:      pull,
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
			r.loadErr = err.Error()
			r.logger.Error().Err(err).Str("path", statePath).Msg("Silence recovery: state file unreadable; starting fresh")
		}
	}
	if p := r.state.Pending; p != nil && p.State == StateRunning {
		// A crash or quit interrupted the run. Its attempt stays counted, so a
		// run that kills the daemon cannot retry forever.
		p.State = StatePending
		r.dirty = true
		r.logger.Warn().
			Int64("last_event_ms", p.LastEventMS).
			Int("attempts", p.Attempts).
			Msg("Silence recovery: a window backfill was interrupted by a restart; it will run again")
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

// activityQueryTimeout bounds the activity queries of one tick.
const activityQueryTimeout = 10 * time.Second

// Tick observes the activity clock once, records a silence that is stalled
// or has just ended, and runs a due window backfill. A run blocks Tick until it
// finishes.
func (r *Recoverer) Tick(ctx context.Context) {
	r.tickMu.Lock()
	defer r.tickMu.Unlock()

	queryCtx, cancel := context.WithTimeout(ctx, activityQueryTimeout)
	r.observe(queryCtx, r.now())
	cancel()

	if plan := r.beginAttempt(r.now()); plan != nil {
		r.runAttempt(plan)
	}
	r.persistIfDue(r.now())
}

// runAttempt runs a planned attempt and records its result. A run that
// panics is recorded as an aborted attempt before the panic continues, so the
// episode is retried on the backoff schedule instead of staying "running"
// until the next restart.
func (r *Recoverer) runAttempt(plan *attemptPlan) {
	finished := false
	defer func() {
		if !finished {
			r.finishAttempt(plan, RunResult{Connected: true, Aborted: true}, true, nil, r.now())
		}
	}()
	result, started := r.runner.RunWindowBackfill(plan.since)
	var health *PullHealth
	if r.pull != nil {
		health = r.pull()
	}
	r.finishAttempt(plan, result, started, health, r.now())
	finished = true
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
	latestByPlatform, err := r.source.Latest(ctx)
	if err != nil {
		r.noteActivityError(err)
		return
	}
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

	if latest.UnixMilli() <= watermarkMS {
		// Still quiet since the watermark: flag the silence once it stalls.
		if now.Sub(watermark) < r.minStall {
			return
		}
		if flagged != nil && flagged.LastEventMS == watermarkMS {
			return
		}
		verdict := r.judge(ctx, watermark, now)
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

	// New activity: walk every gap since the watermark.
	events, err := r.source.Between(ctx, r.cfg.Platform, watermark.Add(time.Millisecond), latest)
	if err != nil {
		r.noteActivityError(err)
		return
	}
	r.noteActivityError(nil)
	if len(events) == 0 || events[len(events)-1].Before(latest) {
		events = append(events, latest)
	}
	var found []detected
	prev := watermark
	var firstAfter time.Time
	for _, event := range events {
		if !event.After(prev) {
			continue
		}
		if firstAfter.IsZero() {
			firstAfter = event
		}
		if event.Sub(prev) >= r.minStall {
			if verdict := r.judge(ctx, prev, event); verdict.Stalled {
				found = append(found, detected{last: prev, end: event, rule: verdict.Rule})
			}
		}
		prev = event
	}
	// A silence flagged while it lasted stays flagged: honour it even if
	// judging it at its end disagrees (its baseline could not be read).
	if flagged != nil && flagged.LastEventMS == watermarkMS && !firstAfter.IsZero() &&
		(len(found) == 0 || found[0].last.UnixMilli() != watermarkMS) {
		found = append([]detected{{last: watermark, end: firstAfter, rule: flagged.Rule}}, found...)
	}

	r.mu.Lock()
	for _, episode := range found {
		r.addEpisodeLocked(episode, now)
	}
	r.state.Flagged = nil
	r.state.WatermarkMS = latest.UnixMilli()
	r.dirty = true
	r.mu.Unlock()
	if len(found) > 0 {
		r.save(now)
	}
	r.pruneBaselines(latest.UnixMilli())
}

// judge evaluates the silence from last to at against last's baseline. When
// the baseline can't be read it judges without one, as /api/status does, so
// only the floor rule can fire.
func (r *Recoverer) judge(ctx context.Context, last, at time.Time) freshness.SilenceVerdict {
	key := last.UnixMilli()
	baseline, ok := r.baselines[key]
	if !ok {
		from, to := freshness.BaselineRange(last, r.cfg.Location, r.cfg.Silence)
		events, err := r.source.Between(ctx, r.cfg.Platform, from, to)
		if err != nil {
			r.logger.Warn().Err(err).Time("last_event", last).Msg("Silence recovery: baseline unavailable; judging without one")
		} else {
			baseline = events
			r.baselines[key] = events
		}
	}
	return freshness.EvaluateSilence(last, baseline, at, r.cfg.Location, r.cfg.Silence)
}

// pruneBaselines drops cached baselines for silences behind the watermark.
func (r *Recoverer) pruneBaselines(watermarkMS int64) {
	for key := range r.baselines {
		if key < watermarkMS {
			delete(r.baselines, key)
		}
	}
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
	next := found.end.Add(r.cfg.Settle)
	if next.Before(now) {
		next = now
	}
	if pending := r.state.Pending; pending != nil {
		pending.Covers = append(pending.Covers, key)
		if sinceMS < pending.SinceMS {
			pending.SinceMS = sinceMS
		}
		if pending.NextAttemptMS > next.UnixMilli() {
			pending.NextAttemptMS = next.UnixMilli()
		}
		r.logger.Info().
			Time("last_event", found.last).
			Time("ended", found.end).
			Int64("pending_last_event_ms", pending.LastEventMS).
			Msg("Silence recovery: another stalled silence ended; it joins the pending window backfill")
		return
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
		if pending.LastEventMS == key || containsKey(pending.Covers, key) {
			return true
		}
	}
	for _, episode := range r.state.History {
		if episode.LastEventMS == key || containsKey(episode.Covers, key) {
			return true
		}
	}
	return false
}

func containsKey(keys []int64, key int64) bool {
	for _, k := range keys {
		if k == key {
			return true
		}
	}
	return false
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
		episode := *pending
		r.finishLocked(now)
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
	pending.Attempts++
	plan := &attemptPlan{key: pending.LastEventMS, since: since, startedAt: now}
	attempt := pending.Attempts
	r.mu.Unlock()
	r.save(now)
	r.logger.Info().
		Time("since", since).
		Int("attempt", attempt).
		Msg("Silence recovery: starting window backfill")
	return plan
}

// finishAttempt records an attempt's result and schedules what follows.
func (r *Recoverer) finishAttempt(plan *attemptPlan, result RunResult, started bool, health *PullHealth, now time.Time) {
	r.mu.Lock()
	pending := r.state.Pending
	if pending == nil || pending.LastEventMS != plan.key {
		r.mu.Unlock()
		return
	}
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
	outcome, detail := Classify(result, health, r.cfg.RequireV2)
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
		PullDataless:       pullDataless(health),
	}
	attempts := pending.Attempts
	since := time.UnixMilli(pending.SinceMS)
	event := r.logger.Info()
	message := "Silence recovery: window backfill recovered the silence"
	switch {
	case outcome == OutcomeRecovered || outcome == OutcomeNothingInWindow:
		pending.State = StateRecovered
		if outcome == OutcomeNothingInWindow {
			message = "Silence recovery: window backfill found no conversation inside the window; nothing was missing"
		}
		r.finishLocked(now)
	case attempts > len(r.cfg.Backoff):
		pending.State = StateGaveUp
		pending.Notice = fmt.Sprintf(
			"Automatic recovery of the Google silence from %s to %s gave up after %d attempts (last: %s). Once that is fixed, run POST /api/backfill {\"since\": %q} by hand.",
			formatTime(time.UnixMilli(pending.LastEventMS), r.cfg.Location),
			formatTime(time.UnixMilli(pending.EndedAtMS), r.cfg.Location),
			attempts,
			detail,
			since.In(r.cfg.Location).Format(time.RFC3339),
		)
		event = r.logger.Error()
		message = "Silence recovery: " + pending.Notice
		r.finishLocked(now)
	default:
		wait := r.cfg.Backoff[attempts-1]
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
		Int("listed", result.Listed).
		Int("conversations", result.Conversations).
		Int("messages", result.Messages).
		Int("empty_conversations", result.EmptyConversations).
		Int("errors", result.Errors).
		Int("history_teed", result.HistoryTeed).
		Int("history_tee_failed", result.HistoryTeeFailed).
		Msg(message)
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
	r.state.History = append([]Episode{*pending}, r.state.History...)
	if limit := r.cfg.HistoryLimit; limit > 0 && len(r.state.History) > limit {
		r.state.History = r.state.History[:limit]
	}
	r.state.Pending = nil
}

func pullDataless(health *PullHealth) bool {
	return health != nil && (health.EmptyWithLocalHistory || health.ConsecutiveDataless > 0)
}

// Classify judges a run that started and had a client. A run counts as
// recovered only when the pull path demonstrably returned data: an empty
// listing, a data-less pull recorded in pull health, or an in-window
// conversation that fetched nothing all mean the 2026-10-07 empty-response
// defect, not an empty window. health is read after the run; the run's own
// INBOX listing is a counted pull that resets pull health when it returns
// data, so a data-less signal after the run means the run's pulls came back
// empty (or another pull since did).
func Classify(result RunResult, health *PullHealth, requireV2 bool) (string, string) {
	switch {
	case result.Aborted:
		return OutcomeAborted, "the run stopped early: the client changed or disconnected, or Google rejected the session"
	case result.Listed == 0:
		return OutcomeEmpty, "the phone listed no conversations at all; its request/response calls return no data"
	case pullDataless(health):
		return OutcomeEmpty, "pull health reports data-less Google pulls"
	case result.EmptyConversations > 0:
		return OutcomeEmpty, fmt.Sprintf("%d in-window conversations returned no messages", result.EmptyConversations)
	case result.Errors > 0 || result.HistoryTeeFailed > 0:
		return OutcomePartial, fmt.Sprintf("%d listing or fetch errors, %d hand-offs to v2 failed", result.Errors, result.HistoryTeeFailed)
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
	// Last is the most recently finished episode.
	Last *Episode `json:"last,omitempty"`
	// Notice repeats Last's notice when it gave up or its window was too
	// large: a recovery that needs a person.
	Notice      string `json:"notice,omitempty"`
	MarginMS    int64  `json:"margin_ms"`
	MaxWindowMS int64  `json:"max_window_ms"`
	SettleMS    int64  `json:"settle_ms"`
	MaxAttempts int    `json:"max_attempts"`
	StatePath   string `json:"state_path,omitempty"`
	StateError  string `json:"state_error,omitempty"`
	// ActivityError is the latest activity query failure, until one succeeds.
	ActivityError string `json:"activity_error,omitempty"`
}

// Snapshot returns the current status view.
func (r *Recoverer) Snapshot() Snapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	snap := Snapshot{
		Enabled:       true,
		WatermarkMS:   r.state.WatermarkMS,
		MarginMS:      r.cfg.Margin.Milliseconds(),
		MaxWindowMS:   r.cfg.MaxWindow.Milliseconds(),
		SettleMS:      r.cfg.Settle.Milliseconds(),
		MaxAttempts:   len(r.cfg.Backoff) + 1,
		StatePath:     r.path,
		ActivityError: r.activityErr,
	}
	if r.source != nil {
		snap.Source = r.source.Name()
	}
	switch {
	case r.saveErr != "":
		snap.StateError = r.saveErr
	case r.loadErr != "":
		snap.StateError = r.loadErr
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
		if snap.Last.State == StateGaveUp || snap.Last.State == StateWindowTooLarge {
			snap.Notice = snap.Last.Notice
		}
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
	cp.Covers = append([]int64(nil), episode.Covers...)
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
func (r *Recoverer) save(now time.Time) {
	r.mu.Lock()
	state := copyState(r.state)
	r.mu.Unlock()
	if r.path == "" {
		r.mu.Lock()
		r.dirty = false
		r.persistedAt = now
		r.mu.Unlock()
		return
	}
	err := saveState(r.path, state)
	r.mu.Lock()
	defer r.mu.Unlock()
	if err != nil {
		if r.saveErr == "" {
			r.logger.Error().Err(err).Str("path", r.path).Msg("Silence recovery: could not save state")
		}
		r.saveErr = err.Error()
		return
	}
	r.saveErr = ""
	r.loadErr = ""
	r.dirty = false
	r.persistedAt = now
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
	return os.Rename(tmpPath, path)
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
