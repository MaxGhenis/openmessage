package v2wire

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

func TestPrimaryNotifierPublishesForEitherSource(t *testing.T) {
	sourceA := newNotifierTestSource()
	sourceB := newNotifierTestSource()
	events := newNotifierTestEvents()
	notifier := &PrimaryNotifier{
		Sources: []func() <-chan struct{}{sourceA.Changes, sourceB.Changes},
		Events:  events,
		Logger:  zerolog.Nop(),
	}

	cancel, done := runNotifierTest(t, notifier)
	events.waitForPairs(t, 1) // The notifier publishes once before its first wait.

	sourceA.Fire()
	events.waitForPairs(t, 2)
	events.requireStablePairs(t, 2)

	sourceB.Fire()
	events.waitForPairs(t, 3)
	events.requireStablePairs(t, 3)

	cancel()
	requireNotifierStopped(t, done, context.Canceled)
	events.requireEmptyConversationIDs(t)
}

func TestPrimaryNotifierCoalescesSimultaneousSources(t *testing.T) {
	sourceA := newNotifierTestSource()
	sourceB := newNotifierTestSource()
	events := newNotifierTestEvents()
	notifier := &PrimaryNotifier{
		Sources: []func() <-chan struct{}{sourceA.Changes, sourceB.Changes},
		Events:  events,
		Logger:  zerolog.Nop(),
	}

	cancel, done := runNotifierTest(t, notifier)
	events.waitForPairs(t, 1)

	// Close both snapshotted channels while holding both source locks. Even if
	// the notifier wakes after the first close, it cannot take a new snapshot
	// until both channels have been closed and replaced.
	fireNotifierTestSourcesTogether(sourceA, sourceB)
	events.waitForPairs(t, 2)
	events.requireStablePairs(t, 2)

	cancel()
	requireNotifierStopped(t, done, context.Canceled)
}

func TestPrimaryNotifierFallbackUsesClock(t *testing.T) {
	clock := newNotifierTestClock(time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC))
	events := newNotifierTestEvents()
	notifier := &PrimaryNotifier{
		Events: events,
		Logger: zerolog.Nop(),
		Now:    clock.Now,
	}

	cancel, done := runNotifierTest(t, notifier)
	events.waitForPairs(t, 1)

	clock.Advance(primaryNotifierFallbackInterval - time.Nanosecond)
	events.requireStablePairs(t, 1)
	clock.Advance(time.Nanosecond)
	events.waitForPairs(t, 2)

	// A large clock jump is one fallback wake, not one publish per skipped
	// interval.
	clock.Advance(5 * primaryNotifierFallbackInterval)
	events.waitForPairs(t, 3)
	events.requireStablePairs(t, 3)

	cancel()
	requireNotifierStopped(t, done, context.Canceled)
}

func TestPrimaryNotifierContextCancellationReturnsPromptly(t *testing.T) {
	clock := newNotifierTestClock(time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC))
	events := newNotifierTestEvents()
	notifier := &PrimaryNotifier{
		Sources: []func() <-chan struct{}{newNotifierTestSource().Changes},
		Events:  events,
		Logger:  zerolog.Nop(),
		Now:     clock.Now,
	}

	cancel, done := runNotifierTest(t, notifier)
	events.waitForPairs(t, 1)
	cancel()
	requireNotifierStopped(t, done, context.Canceled)
}

func runNotifierTest(t *testing.T, notifier *PrimaryNotifier) (context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() {
		done <- notifier.Run(ctx)
	}()
	return cancel, done
}

func requireNotifierStopped(t *testing.T, done <-chan error, want error) {
	t.Helper()
	select {
	case err := <-done:
		if !errors.Is(err, want) {
			t.Fatalf("Run() error = %v, want %v", err, want)
		}
	case <-time.After(time.Second):
		t.Fatal("Run() did not return promptly")
	}
}

type notifierTestSource struct {
	mu      sync.Mutex
	changed chan struct{}
}

func newNotifierTestSource() *notifierTestSource {
	return &notifierTestSource{changed: make(chan struct{})}
}

func (s *notifierTestSource) Changes() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.changed
}

func (s *notifierTestSource) Fire() {
	s.mu.Lock()
	close(s.changed)
	s.changed = make(chan struct{})
	s.mu.Unlock()
}

func fireNotifierTestSourcesTogether(a, b *notifierTestSource) {
	a.mu.Lock()
	b.mu.Lock()
	close(a.changed)
	close(b.changed)
	a.changed = make(chan struct{})
	b.changed = make(chan struct{})
	b.mu.Unlock()
	a.mu.Unlock()
}

type notifierTestEvents struct {
	mu                sync.Mutex
	messages          int
	conversations     int
	conversationIDs   []string
	completedPairWake chan struct{}
}

func newNotifierTestEvents() *notifierTestEvents {
	return &notifierTestEvents{completedPairWake: make(chan struct{})}
}

func (e *notifierTestEvents) PublishMessages(conversationID string) {
	e.mu.Lock()
	e.messages++
	e.conversationIDs = append(e.conversationIDs, conversationID)
	e.mu.Unlock()
}

func (e *notifierTestEvents) PublishConversations() {
	e.mu.Lock()
	e.conversations++
	close(e.completedPairWake)
	e.completedPairWake = make(chan struct{})
	e.mu.Unlock()
}

func (e *notifierTestEvents) waitForPairs(t *testing.T, want int) {
	t.Helper()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for {
		messages, conversations, wake := e.snapshot()
		// PublishMessages and PublishConversations are two interface calls, so
		// observing one in-progress pair here is valid. Completed pairs must
		// remain balanced.
		if messages < conversations || messages > conversations+1 {
			t.Fatalf("invalid publish counts = messages %d, conversations %d", messages, conversations)
		}
		if conversations >= want {
			if conversations != want || messages != want {
				t.Fatalf("publish counts = messages %d, conversations %d; want %d each", messages, conversations, want)
			}
			return
		}
		select {
		case <-wake:
		case <-deadline.C:
			t.Fatalf("timed out waiting for %d publish pairs; got %d", want, conversations)
		}
	}
}

func (e *notifierTestEvents) requireStablePairs(t *testing.T, want int) {
	t.Helper()
	messages, conversations, wake := e.snapshot()
	if messages != want || conversations != want {
		t.Fatalf("publish counts = messages %d, conversations %d; want %d each", messages, conversations, want)
	}
	timer := time.NewTimer(25 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-wake:
		messages, conversations, _ = e.snapshot()
		t.Fatalf("unexpected extra publish: messages %d, conversations %d", messages, conversations)
	case <-timer.C:
		messages, conversations, _ = e.snapshot()
		if messages != want || conversations != want {
			t.Fatalf("publish counts became messages %d, conversations %d; want %d each", messages, conversations, want)
		}
	}
}

func (e *notifierTestEvents) requireEmptyConversationIDs(t *testing.T) {
	t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	for i, conversationID := range e.conversationIDs {
		if conversationID != "" {
			t.Fatalf("PublishMessages call %d conversation ID = %q, want empty", i, conversationID)
		}
	}
}

func (e *notifierTestEvents) snapshot() (int, int, <-chan struct{}) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.messages, e.conversations, e.completedPairWake
}

type notifierTestClock struct {
	mu  sync.Mutex
	now time.Time
}

func newNotifierTestClock(now time.Time) *notifierTestClock {
	return &notifierTestClock{now: now}
}

func (c *notifierTestClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *notifierTestClock) Advance(delta time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(delta)
	c.mu.Unlock()
}
