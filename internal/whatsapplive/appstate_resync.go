package whatsapplive

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/appstate"
	waevents "go.mau.fi/whatsmeow/types/events"
)

// An app-state collection whose stored state disagrees with the server's
// fails every later patch that touches an affected entry with
// "failed to verify patch vN: mismatching LTHash". The usual cause is a
// version stored without the value MACs of the patch that advanced it:
// whatsmeow writes the version first and the MACs in separate statements
// (appstate/decode.go storeMACs), so anything that interrupts it in between
// (generation teardown cancelling the handler context before #236, a crash, a
// failed write) leaves the collection that way. whatsmeow dispatches
// AppStateSyncError for the failure and keeps fetching from the stored
// version, so the collection stays stuck. For critical_unblock_low that means
// the contact names OpenMessage reads from whatsmeow's contact store stop
// updating.
//
// The way out is a full resync of the collection: whatsmeow's FetchAppState
// with fullSync deletes the stored version (which cascades its value MACs) and
// applies the server's snapshot. With EmitAppStateEventsOnFullSync off (the
// default), a snapshot of critical_unblock_low bulk-writes every contact's
// name (PutAllContactNames). The appStateResyncer below starts one when the
// error says the stored state disagrees, at most one at a time per collection
// and within a budget, so a collection that keeps failing cannot loop.

// fetchAppStateFullSync resyncs one app-state collection from scratch.
var fetchAppStateFullSync = func(ctx context.Context, cli *whatsmeow.Client, name appstate.WAPatchName) error {
	return cli.FetchAppState(ctx, name, true, false)
}

// appStateResyncPolicy bounds the full resyncs started for one collection.
type appStateResyncPolicy struct {
	// cooldown is the least time between the starts of two resyncs.
	cooldown time.Duration
	// maxStarts is how many resyncs may start within any window.
	maxStarts int
	window    time.Duration
}

// appStateResyncLimits allows a collection three full resyncs a day, ten
// minutes apart. A collection corrupted once needs one; one that still fails
// after three is not a local-state problem a fourth snapshot would fix.
var appStateResyncLimits = appStateResyncPolicy{cooldown: 10 * time.Minute, maxStarts: 3, window: 24 * time.Hour}

var appStateResyncNow = time.Now

type appStateResyncDecision string

const (
	appStateResyncStart       appStateResyncDecision = "start"
	appStateResyncInFlight    appStateResyncDecision = "in_flight"
	appStateResyncCoolingDown appStateResyncDecision = "cooling_down"
	appStateResyncCapped      appStateResyncDecision = "capped"
)

// appStateResyncBudget is one collection's resync history.
type appStateResyncBudget struct {
	inFlight bool
	// starts holds the start times of the resyncs inside the policy window,
	// oldest first. lastStart is kept apart because the cooldown can outlast
	// the window.
	starts    []time.Time
	lastStart time.Time
	// cappedWarned is set once the cap has been logged at warning level, and
	// cleared by the next start.
	cappedWarned bool
}

// admit decides whether a resync may start at now and, if so, records it as
// started and in flight.
func (p appStateResyncPolicy) admit(budget *appStateResyncBudget, now time.Time) appStateResyncDecision {
	if budget.inFlight {
		return appStateResyncInFlight
	}
	inWindow := budget.starts[:0]
	for _, start := range budget.starts {
		if now.Sub(start) < p.window {
			inWindow = append(inWindow, start)
		}
	}
	budget.starts = inWindow
	if len(budget.starts) >= p.maxStarts {
		return appStateResyncCapped
	}
	if !budget.lastStart.IsZero() && now.Sub(budget.lastStart) < p.cooldown {
		return appStateResyncCoolingDown
	}
	budget.inFlight = true
	budget.starts = append(budget.starts, now)
	budget.lastStart = now
	return appStateResyncStart
}

// nextStart is the earliest time a resync refused by admit could start,
// assuming none is in flight then.
func (p appStateResyncPolicy) nextStart(budget *appStateResyncBudget) time.Time {
	next := budget.lastStart.Add(p.cooldown)
	if p.maxStarts > 0 && len(budget.starts) >= p.maxStarts {
		if leaves := budget.starts[len(budget.starts)-p.maxStarts].Add(p.window); leaves.After(next) {
			next = leaves
		}
	}
	return next
}

// needsFullResync reports whether an app-state decode error means the stored
// collection disagrees with the server's, which only a full resync repairs.
// The pinned whatsmeow only warns about a missing previous value MAC and lets
// the LTHash check fail instead; ErrMissingPreviousSetValueOperation is
// matched too in case a later whatsmeow returns it. The other decode errors
// (a missing key, a patch, content or index MAC mismatch) are about the patch
// itself, and refetching the collection would not change them.
func needsFullResync(err error) bool {
	return errors.Is(err, appstate.ErrMismatchingLTHash) ||
		errors.Is(err, appstate.ErrMissingPreviousSetValueOperation)
}

// appStateResyncer starts the full resyncs of one whatsmeow client's
// collections. Invariants:
//   - at most one resync per collection is in flight;
//   - for each collection, at most policy.maxStarts resyncs start within any
//     policy.window, each at least policy.cooldown after the one before;
//   - handle never blocks the event goroutine.
type appStateResyncer struct {
	cli    *whatsmeow.Client
	logger zerolog.Logger
	// ctx ends when the client is retired (stop); a resync started after
	// that is refused, and one in flight is cancelled.
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu      sync.Mutex
	budgets map[appstate.WAPatchName]*appStateResyncBudget
}

func newAppStateResyncer(cli *whatsmeow.Client, logger zerolog.Logger) *appStateResyncer {
	ctx, cancel := context.WithCancel(context.Background())
	return &appStateResyncer{
		cli:     cli,
		logger:  logger,
		ctx:     ctx,
		cancel:  cancel,
		budgets: make(map[appstate.WAPatchName]*appStateResyncBudget),
	}
}

// handle answers one AppStateSyncError. It returns at once: whatsmeow
// dispatches the error from inside fetchAppState, which holds the client's
// app-state lock that FetchAppState takes again, so a resync run here would
// deadlock. The resync runs on its own goroutine and starts once the failed
// fetch has released the lock.
//
// The resync context is not the handler's. A generation teardown does not
// cancel it (cancelling between whatsmeow's version and MAC writes is how a
// collection gets stuck); only retiring the client does. If the connection
// drops before the snapshot arrives, whatsmeow has already deleted the stored
// version, and its next fetch of the collection is a full sync.
func (r *appStateResyncer) handle(evt *waevents.AppStateSyncError) {
	if r == nil || evt == nil {
		return
	}
	log := r.logger.With().Str("collection", string(evt.Name)).Bool("full_sync", evt.FullSync).Logger()
	if !needsFullResync(evt.Error) {
		log.Debug().Err(evt.Error).Msg("WhatsApp app state sync failed for a reason a full resync does not fix")
		return
	}
	now := appStateResyncNow()
	policy := appStateResyncLimits

	r.mu.Lock()
	if r.ctx.Err() != nil {
		r.mu.Unlock()
		return
	}
	budget := r.budgets[evt.Name]
	if budget == nil {
		budget = &appStateResyncBudget{}
		r.budgets[evt.Name] = budget
	}
	decision := policy.admit(budget, now)
	attempt := len(budget.starts)
	warnCapped := false
	switch decision {
	case appStateResyncStart:
		budget.cappedWarned = false
		r.wg.Add(1)
	case appStateResyncCapped:
		warnCapped = !budget.cappedWarned
		budget.cappedWarned = true
	}
	var nextStart time.Time
	if decision == appStateResyncCapped || decision == appStateResyncCoolingDown {
		nextStart = policy.nextStart(budget)
	}
	r.mu.Unlock()

	switch decision {
	case appStateResyncStart:
		log.Warn().Err(evt.Error).Int("attempt", attempt).Int("max_attempts", policy.maxStarts).Dur("window", policy.window).
			Msg("WhatsApp app state collection disagrees with the server; starting a full resync")
		go r.run(evt.Name, attempt, log)
	case appStateResyncCapped:
		event := log.Debug()
		if warnCapped {
			event = log.Warn()
		}
		event.Err(evt.Error).Int("max_attempts", policy.maxStarts).Dur("window", policy.window).Time("next_resync_after", nextStart).
			Msg("WhatsApp app state collection still disagrees with the server; full resync budget used up")
	case appStateResyncCoolingDown:
		log.Debug().Err(evt.Error).Time("next_resync_after", nextStart).Msg("WhatsApp app state collection disagrees with the server; full resync cooling down")
	case appStateResyncInFlight:
		log.Debug().Err(evt.Error).Msg("WhatsApp app state collection disagrees with the server; full resync already running")
	}
}

func (r *appStateResyncer) run(name appstate.WAPatchName, attempt int, log zerolog.Logger) {
	defer r.wg.Done()
	started := time.Now()
	err := fetchAppStateFullSync(r.ctx, r.cli, name)
	r.mu.Lock()
	r.budgets[name].inFlight = false
	r.mu.Unlock()
	if err != nil {
		log.Warn().Err(err).Int("attempt", attempt).Dur("elapsed", time.Since(started)).Msg("WhatsApp app state full resync failed")
		return
	}
	log.Info().Int("attempt", attempt).Dur("elapsed", time.Since(started)).Msg("WhatsApp app state full resync completed")
}

// stop refuses new resyncs and cancels the ones in flight. It does not wait
// for them: a resync dispatches whatsmeow events, which take the bridge lock
// that stop's callers hold. wait does that afterwards.
func (r *appStateResyncer) stop() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.cancel()
	r.mu.Unlock()
}

// wait waits up to timeout for the resyncs in flight to return. Call it after
// stop, without holding the bridge lock.
func (r *appStateResyncer) wait(timeout time.Duration) bool {
	if r == nil {
		return true
	}
	done := make(chan struct{})
	go func() {
		r.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// inFlight counts the collections with a resync in flight.
func (r *appStateResyncer) inFlight() int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	count := 0
	for _, budget := range r.budgets {
		if budget.inFlight {
			count++
		}
	}
	return count
}

// appStateResyncStopTimeout bounds how long Close waits for a cancelled
// resync to return.
const appStateResyncStopTimeout = 5 * time.Second

// handleAppStateSyncError hands whatsmeow's app-state decode failure to the
// current client's resyncer. handleClientEvent has already dropped events from
// a retired client.
func (b *Bridge) handleAppStateSyncError(evt *waevents.AppStateSyncError) {
	b.mu.RLock()
	resync := b.appStateResync
	b.mu.RUnlock()
	resync.handle(evt)
}
