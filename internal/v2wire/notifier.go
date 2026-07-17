package v2wire

import (
	"context"
	"errors"
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
type PrimaryNotifier struct {
	Sources []func() <-chan struct{}
	Events  EventPublisher
	Logger  zerolog.Logger
	Now     func() time.Time
}

// Run publishes once immediately, then again after any source change or the
// fallback interval. Each loop snapshots all broadcast channels before
// publishing so changes concurrent with publication remain observable.
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

	fallback := newPrimaryNotifierFallback(n.Now)
	defer fallback.stop()

	for {
		sourceChannels := snapshotPrimaryNotifierSources(n.Sources)
		n.Events.PublishMessages("")
		n.Events.PublishConversations()

		cases := make([]reflect.SelectCase, 0, len(sourceChannels)+2)
		cases = append(cases,
			reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(ctx.Done())},
			reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(fallback.ticks)},
		)
		for _, sourceChannel := range sourceChannels {
			cases = append(cases, reflect.SelectCase{
				Dir:  reflect.SelectRecv,
				Chan: reflect.ValueOf(sourceChannel),
			})
		}

		chosen, _, _ := reflect.Select(cases)
		if chosen == 0 {
			return ctx.Err()
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

type primaryNotifierFallback struct {
	ticks <-chan time.Time
	stop  func()
}

func newPrimaryNotifierFallback(now func() time.Time) primaryNotifierFallback {
	if now == nil {
		ticker := time.NewTicker(primaryNotifierFallbackInterval)
		return primaryNotifierFallback{ticks: ticker.C, stop: ticker.Stop}
	}

	// A time-reading function cannot directly wake a select. In tests, poll
	// the supplied logical clock on a short real ticker and emit at most one
	// wake for any forward jump. Production leaves Now nil and uses the exact
	// five-second ticker path above.
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
				if current.Sub(lastTick) < primaryNotifierFallbackInterval {
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
