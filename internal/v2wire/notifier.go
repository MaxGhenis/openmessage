package v2wire

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/rs/zerolog"
)

const (
	primaryNotifierFallbackInterval  = 5 * time.Second
	primaryNotifierClockPollInterval = time.Millisecond
)

// PrimaryNotifier turns coarse v2 state changes into the SSE events that ask
// the web client to refetch its message and conversation views.
//
// Every publish makes each open web client refetch its conversation list, its
// open thread, its drafts and the outbox, so the notifier publishes only for a
// change: a source firing, or a fallback tick that finds a revision moved. Idle
// stores publish nothing. Stream liveness is not the notifier's job; the
// /api/events handler sends its own heartbeat.
type PrimaryNotifier struct {
	Sources []func() <-chan struct{}
	// Revisions catch writes that fire no source, including writes from other
	// processes. Each fallback interval the notifier reads every revision and
	// publishes if any differs from the value read before the last publish.
	// A read error counts as moved. With no revisions the fallback is off and
	// the sources are the only trigger.
	Revisions []PrimaryNotifierRevision
	Events    EventPublisher
	Logger    zerolog.Logger
	Now       func() time.Time
	// FallbackInterval overrides the five-second fallback period when
	// positive.
	FallbackInterval time.Duration
}

// PrimaryNotifierRevision is a value that moves whenever the data behind it
// changes, such as a store's dataversion.Probe.
type PrimaryNotifierRevision struct {
	// Name identifies the revision in logs.
	Name string
	Read func(context.Context) (int64, error)
}

// Run publishes once immediately, then again after each change. Before every
// publish it snapshots all broadcast channels and revisions, so a change that
// lands during or after publication closes a held channel or moves a revision
// past its held value, and publishes again.
func (n *PrimaryNotifier) Run(ctx context.Context) error {
	if n == nil {
		return errors.New("run v2 primary notifier: notifier is nil")
	}
	if ctx == nil {
		return errors.New("run v2 primary notifier: context is nil")
	}
	if n.Events == nil {
		return errors.New("run v2 primary notifier: event publisher is nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	for i, revision := range n.Revisions {
		if revision.Read == nil {
			return fmt.Errorf("run v2 primary notifier: revision %d (%q) has no Read", i, revision.Name)
		}
	}

	var ticks <-chan time.Time
	if len(n.Revisions) > 0 {
		interval := n.FallbackInterval
		if interval <= 0 {
			interval = primaryNotifierFallbackInterval
		}
		fallback := newPrimaryNotifierFallback(n.Now, interval)
		defer fallback.stop()
		ticks = fallback.ticks
	}
	revisions := newPrimaryNotifierRevisions(n.Revisions, n.Logger)

	for {
		sourceChannels := snapshotPrimaryNotifierSources(n.Sources)
		published := revisions.current(ctx)
		if err := ctx.Err(); err != nil {
			return err
		}
		n.Events.PublishMessages("")
		n.Events.PublishConversations()

		if err := waitForPrimaryNotifierChange(ctx, sourceChannels, ticks, revisions, published); err != nil {
			return err
		}
	}
}

// waitForPrimaryNotifierChange returns when a held source channel delivers or
// a fallback tick finds a revision moved past published. Quiet ticks keep the
// same snapshot: taking a new one without publishing would drop a change that
// closed a held channel after the select chose the tick.
func waitForPrimaryNotifierChange(
	ctx context.Context,
	sourceChannels []<-chan struct{},
	ticks <-chan time.Time,
	revisions *primaryNotifierRevisions,
	published []primaryNotifierRevisionValue,
) error {
	cases := make([]reflect.SelectCase, 0, len(sourceChannels)+2)
	tickCase := reflect.SelectCase{Dir: reflect.SelectRecv}
	if ticks != nil {
		tickCase.Chan = reflect.ValueOf(ticks)
	}
	cases = append(cases,
		reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(ctx.Done())},
		tickCase,
	)
	for _, sourceChannel := range sourceChannels {
		cases = append(cases, reflect.SelectCase{
			Dir:  reflect.SelectRecv,
			Chan: reflect.ValueOf(sourceChannel),
		})
	}

	for {
		chosen, _, _ := reflect.Select(cases)
		switch chosen {
		case 0:
			return ctx.Err()
		case 1:
			current := revisions.current(ctx)
			if err := ctx.Err(); err != nil {
				return err
			}
			if primaryNotifierRevisionsMoved(current, published) {
				return nil
			}
		default:
			return nil
		}
	}
}

func snapshotPrimaryNotifierSources(sources []func() <-chan struct{}) []<-chan struct{} {
	channels := make([]<-chan struct{}, 0, len(sources))
	for _, source := range sources {
		if source != nil {
			channels = append(channels, source())
		}
	}
	return channels
}

// primaryNotifierRevisionValue is one revision read; known is false when the
// read failed.
type primaryNotifierRevisionValue struct {
	value int64
	known bool
}

// primaryNotifierRevisionsMoved fails open: a failed read on either side
// cannot rule a change out.
func primaryNotifierRevisionsMoved(current, published []primaryNotifierRevisionValue) bool {
	if len(current) != len(published) {
		return true
	}
	for i := range current {
		if !current[i].known || !published[i].known || current[i].value != published[i].value {
			return true
		}
	}
	return false
}

// primaryNotifierRevisions reads every revision and logs only when one starts
// or stops failing, so a broken probe costs one warning rather than one per
// tick.
type primaryNotifierRevisions struct {
	revisions []PrimaryNotifierRevision
	failing   []bool
	logger    zerolog.Logger
}

func newPrimaryNotifierRevisions(revisions []PrimaryNotifierRevision, logger zerolog.Logger) *primaryNotifierRevisions {
	return &primaryNotifierRevisions{
		revisions: revisions,
		failing:   make([]bool, len(revisions)),
		logger:    logger,
	}
}

func (r *primaryNotifierRevisions) current(ctx context.Context) []primaryNotifierRevisionValue {
	values := make([]primaryNotifierRevisionValue, len(r.revisions))
	for i, revision := range r.revisions {
		value, err := revision.Read(ctx)
		if err != nil {
			if ctx.Err() == nil && !r.failing[i] {
				r.failing[i] = true
				r.logger.Warn().Err(err).Str("revision", revision.Name).Msg(
					"V2 primary notifier cannot read a store revision; publishing on every fallback tick until it can",
				)
			}
			continue
		}
		if r.failing[i] {
			r.failing[i] = false
			r.logger.Info().Str("revision", revision.Name).Msg("V2 primary notifier reads the store revision again")
		}
		values[i] = primaryNotifierRevisionValue{value: value, known: true}
	}
	return values
}

type primaryNotifierFallback struct {
	ticks <-chan time.Time
	stop  func()
}

func newPrimaryNotifierFallback(now func() time.Time, interval time.Duration) primaryNotifierFallback {
	if now == nil {
		ticker := time.NewTicker(interval)
		return primaryNotifierFallback{ticks: ticker.C, stop: ticker.Stop}
	}

	// A time-reading function cannot directly wake a select. In tests, poll
	// the supplied logical clock on a short real ticker and emit at most one
	// wake for any forward jump. Production leaves Now nil and uses the real
	// ticker path above.
	ticks := make(chan time.Time, 1)
	stop := make(chan struct{})
	stopped := make(chan struct{})
	lastTick := now()
	go func() {
		defer close(stopped)
		poller := time.NewTicker(primaryNotifierClockPollInterval)
		defer poller.Stop()
		for {
			select {
			case <-stop:
				return
			case <-poller.C:
				current := now()
				if current.Before(lastTick) {
					lastTick = current
					continue
				}
				if current.Sub(lastTick) < interval {
					continue
				}
				lastTick = current
				select {
				case ticks <- current:
				default:
				}
			}
		}
	}()
	return primaryNotifierFallback{
		ticks: ticks,
		stop: func() {
			close(stop)
			<-stopped
		},
	}
}
