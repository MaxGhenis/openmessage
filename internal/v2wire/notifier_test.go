package v2wire

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

var notifierTestStart = time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)

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

// An idle store must not publish: every publish costs each open web client a
// refetch of its conversation list, open thread and outbox.
func TestPrimaryNotifierIdleTickDoesNotPublish(t *testing.T) {
	clock := newNotifierTestClock(notifierTestStart)
	revision := newNotifierTestRevision(7)
	events := newNotifierTestEvents()
	notifier := &PrimaryNotifier{
		Sources:   []func() <-chan struct{}{newNotifierTestSource().Changes},
		Revisions: notifierTestRevisions(revision),
		Events:    events,
		Logger:    zerolog.Nop(),
		Now:       clock.Now,
	}

	cancel, done := runNotifierTest(t, notifier)
	events.waitForPairs(t, 1)
	revision.waitForReads(t, 1) // read before the initial publish

	for tick := 1; tick <= 3; tick++ {
		clock.Advance(primaryNotifierFallbackInterval)
		revision.waitForReads(t, 1+tick) // the tick ran and found nothing new
		events.requireStablePairs(t, 1)
	}

	cancel()
	requireNotifierStopped(t, done, context.Canceled)
}

func TestPrimaryNotifierSourceChangePublishesWithoutTick(t *testing.T) {
	clock := newNotifierTestClock(notifierTestStart)
	source := newNotifierTestSource()
	revision := newNotifierTestRevision(7)
	events := newNotifierTestEvents()
	notifier := &PrimaryNotifier{
		Sources:   []func() <-chan struct{}{source.Changes},
		Revisions: notifierTestRevisions(revision),
		Events:    events,
		Logger:    zerolog.Nop(),
		Now:       clock.Now,
	}

	cancel, done := runNotifierTest(t, notifier)
	events.waitForPairs(t, 1)

	// A source fires after its write commits, so the revision has moved too;
	// the publish must not wait for a tick either way.
	revision.Set(8)
	source.Fire()
	events.waitForPairs(t, 2)
	events.requireStablePairs(t, 2)

	// That publish re-read the revision, so the next tick is quiet.
	reads := revision.Reads()
	clock.Advance(primaryNotifierFallbackInterval)
	revision.waitForReads(t, reads+1)
	events.requireStablePairs(t, 2)

	cancel()
	requireNotifierStopped(t, done, context.Canceled)
}

// A commit that fires no source still reaches the UI on the next tick.
func TestPrimaryNotifierPublishesWhenRevisionMovesWithoutSource(t *testing.T) {
	clock := newNotifierTestClock(notifierTestStart)
	revision := newNotifierTestRevision(7)
	events := newNotifierTestEvents()
	notifier := &PrimaryNotifier{
		Sources:   []func() <-chan struct{}{newNotifierTestSource().Changes},
		Revisions: notifierTestRevisions(revision),
		Events:    events,
		Logger:    zerolog.Nop(),
		Now:       clock.Now,
	}

	cancel, done := runNotifierTest(t, notifier)
	events.waitForPairs(t, 1)

	revision.Set(8)
	events.requireStablePairs(t, 1) // nothing until the fallback interval passes
	clock.Advance(primaryNotifierFallbackInterval - time.Nanosecond)
	events.requireStablePairs(t, 1)
	clock.Advance(time.Nanosecond)
	events.waitForPairs(t, 2)
	events.requireStablePairs(t, 2)

	// A large clock jump is one fallback wake, not one per skipped interval.
	revision.Set(9)
	clock.Advance(5 * primaryNotifierFallbackInterval)
	events.waitForPairs(t, 3)
	events.requireStablePairs(t, 3)

	cancel()
	requireNotifierStopped(t, done, context.Canceled)
}

// A source change that lands while the notifier is publishing must cause one
// more publish: the refetch that publish triggers may already have run.
func TestPrimaryNotifierSourceChangeDuringPublishIsNotLost(t *testing.T) {
	source := newNotifierTestSource()
	events := newNotifierTestEvents()
	events.onPublishMessages = func(call int) {
		if call == 1 {
			source.Fire()
		}
	}
	notifier := &PrimaryNotifier{
		Sources:   []func() <-chan struct{}{source.Changes},
		Revisions: notifierTestRevisions(newNotifierTestRevision(7)),
		Events:    events,
		Logger:    zerolog.Nop(),
		Now:       newNotifierTestClock(notifierTestStart).Now,
	}

	cancel, done := runNotifierTest(t, notifier)
	events.waitForPairs(t, 2)
	events.requireStablePairs(t, 2)

	cancel()
	requireNotifierStopped(t, done, context.Canceled)
}

// Likewise for a commit that fires no source: the revision is read before the
// publish, so the next tick sees it move.
func TestPrimaryNotifierRevisionChangeDuringPublishIsNotLost(t *testing.T) {
	clock := newNotifierTestClock(notifierTestStart)
	revision := newNotifierTestRevision(7)
	events := newNotifierTestEvents()
	events.onPublishMessages = func(call int) {
		if call == 1 {
			revision.Set(8)
		}
	}
	notifier := &PrimaryNotifier{
		Revisions: notifierTestRevisions(revision),
		Events:    events,
		Logger:    zerolog.Nop(),
		Now:       clock.Now,
	}

	cancel, done := runNotifierTest(t, notifier)
	events.waitForPairs(t, 1)
	clock.Advance(primaryNotifierFallbackInterval)
	events.waitForPairs(t, 2)
	events.requireStablePairs(t, 2)

	cancel()
	requireNotifierStopped(t, done, context.Canceled)
}

// A source change that lands while a quiet tick is reading the revision must
// still publish. The notifier keeps its snapshot across quiet ticks; a fresh
// snapshot would hold the replacement channel and miss this change.
func TestPrimaryNotifierSourceChangeDuringQuietTickIsNotLost(t *testing.T) {
	clock := newNotifierTestClock(notifierTestStart)
	source := newNotifierTestSource()
	revision := newNotifierTestRevision(7)
	revision.onRead = func(call int) {
		if call == 2 { // the first tick's read
			source.Fire()
		}
	}
	events := newNotifierTestEvents()
	notifier := &PrimaryNotifier{
		Sources:   []func() <-chan struct{}{source.Changes},
		Revisions: notifierTestRevisions(revision),
		Events:    events,
		Logger:    zerolog.Nop(),
		Now:       clock.Now,
	}

	cancel, done := runNotifierTest(t, notifier)
	events.waitForPairs(t, 1)
	clock.Advance(primaryNotifierFallbackInterval)
	events.waitForPairs(t, 2)
	events.requireStablePairs(t, 2)

	cancel()
	requireNotifierStopped(t, done, context.Canceled)
}

// A failed revision read cannot rule a change out, so it publishes; once reads
// work again the notifier goes quiet. Each transition logs once.
func TestPrimaryNotifierRevisionErrorsFailOpen(t *testing.T) {
	clock := newNotifierTestClock(notifierTestStart)
	revision := newNotifierTestRevision(7)
	events := newNotifierTestEvents()
	var logs notifierTestLog
	notifier := &PrimaryNotifier{
		Revisions: notifierTestRevisions(revision),
		Events:    events,
		Logger:    zerolog.New(&logs),
		Now:       clock.Now,
	}

	cancel, done := runNotifierTest(t, notifier)
	events.waitForPairs(t, 1)

	// Two failing ticks: each publishes, and the publish's own read fails too.
	revision.FailReads(4)
	clock.Advance(primaryNotifierFallbackInterval)
	events.waitForPairs(t, 2)
	clock.Advance(primaryNotifierFallbackInterval)
	events.waitForPairs(t, 3)

	// The next tick reads successfully, but the value held from the last
	// publish is unknown, so it still publishes once.
	clock.Advance(primaryNotifierFallbackInterval)
	events.waitForPairs(t, 4)
	events.requireStablePairs(t, 4)

	// Now the held value is known and unchanged: quiet again.
	reads := revision.Reads()
	clock.Advance(primaryNotifierFallbackInterval)
	revision.waitForReads(t, reads+1)
	events.requireStablePairs(t, 4)

	cancel()
	requireNotifierStopped(t, done, context.Canceled)
	if got := logs.count("cannot read a store revision"); got != 1 {
		t.Fatalf("revision failure warnings = %d, want 1:\n%s", got, logs.String())
	}
	if got := logs.count("reads the store revision again"); got != 1 {
		t.Fatalf("revision recovery notices = %d, want 1:\n%s", got, logs.String())
	}
}

func TestPrimaryNotifierFailedInitialRevisionPublishesOnFirstTick(t *testing.T) {
	clock := newNotifierTestClock(notifierTestStart)
	revision := newNotifierTestRevision(7)
	revision.FailReads(1)
	events := newNotifierTestEvents()
	notifier := &PrimaryNotifier{
		Revisions: notifierTestRevisions(revision),
		Events:    events,
		Logger:    zerolog.Nop(),
		Now:       clock.Now,
	}

	cancel, done := runNotifierTest(t, notifier)
	events.waitForPairs(t, 1)
	clock.Advance(primaryNotifierFallbackInterval)
	events.waitForPairs(t, 2)
	events.requireStablePairs(t, 2)

	cancel()
	requireNotifierStopped(t, done, context.Canceled)
}

func TestPrimaryNotifierWithoutRevisionPublishesOnlyForSources(t *testing.T) {
	clock := newNotifierTestClock(notifierTestStart)
	source := newNotifierTestSource()
	events := newNotifierTestEvents()
	notifier := &PrimaryNotifier{
		Sources: []func() <-chan struct{}{source.Changes},
		Events:  events,
		Logger:  zerolog.Nop(),
		Now:     clock.Now,
	}

	cancel, done := runNotifierTest(t, notifier)
	events.waitForPairs(t, 1)
	clock.Advance(10 * primaryNotifierFallbackInterval)
	events.requireStablePairs(t, 1)

	source.Fire()
	events.waitForPairs(t, 2)
	events.requireStablePairs(t, 2)

	cancel()
	requireNotifierStopped(t, done, context.Canceled)
}

func TestPrimaryNotifierFallbackIntervalOverride(t *testing.T) {
	clock := newNotifierTestClock(notifierTestStart)
	revision := newNotifierTestRevision(7)
	events := newNotifierTestEvents()
	notifier := &PrimaryNotifier{
		Revisions:        notifierTestRevisions(revision),
		Events:           events,
		Logger:           zerolog.Nop(),
		Now:              clock.Now,
		FallbackInterval: time.Second,
	}

	cancel, done := runNotifierTest(t, notifier)
	events.waitForPairs(t, 1)
	revision.Set(8)
	clock.Advance(time.Second)
	events.waitForPairs(t, 2)

	cancel()
	requireNotifierStopped(t, done, context.Canceled)
}

// Production watches two stores (v2 and legacy). A commit to either one
// publishes on the next tick; a tick with neither moved stays quiet.
func TestPrimaryNotifierPublishesWhenAnyRevisionMoves(t *testing.T) {
	clock := newNotifierTestClock(notifierTestStart)
	v2 := newNotifierTestRevision(7)
	legacy := newNotifierTestRevision(40)
	events := newNotifierTestEvents()
	notifier := &PrimaryNotifier{
		Revisions: notifierTestRevisions(v2, legacy),
		Events:    events,
		Logger:    zerolog.Nop(),
		Now:       clock.Now,
	}

	cancel, done := runNotifierTest(t, notifier)
	events.waitForPairs(t, 1)

	legacy.Set(41)
	clock.Advance(primaryNotifierFallbackInterval)
	events.waitForPairs(t, 2)
	events.requireStablePairs(t, 2)

	v2.Set(8)
	clock.Advance(primaryNotifierFallbackInterval)
	events.waitForPairs(t, 3)
	events.requireStablePairs(t, 3)

	reads := legacy.Reads()
	clock.Advance(primaryNotifierFallbackInterval)
	legacy.waitForReads(t, reads+1)
	events.requireStablePairs(t, 3)

	cancel()
	requireNotifierStopped(t, done, context.Canceled)
}

// One failing revision fails open on its own; the other's state is unaffected.
func TestPrimaryNotifierOneFailingRevisionFailsOpen(t *testing.T) {
	clock := newNotifierTestClock(notifierTestStart)
	v2 := newNotifierTestRevision(7)
	legacy := newNotifierTestRevision(40)
	events := newNotifierTestEvents()
	notifier := &PrimaryNotifier{
		Revisions: notifierTestRevisions(v2, legacy),
		Events:    events,
		Logger:    zerolog.Nop(),
		Now:       clock.Now,
	}

	cancel, done := runNotifierTest(t, notifier)
	events.waitForPairs(t, 1)

	legacy.FailReads(1)
	clock.Advance(primaryNotifierFallbackInterval)
	events.waitForPairs(t, 2)
	events.requireStablePairs(t, 2)

	reads := v2.Reads()
	clock.Advance(primaryNotifierFallbackInterval)
	v2.waitForReads(t, reads+1)
	events.requireStablePairs(t, 2)

	cancel()
	requireNotifierStopped(t, done, context.Canceled)
}

func TestPrimaryNotifierRejectsRevisionWithoutRead(t *testing.T) {
	notifier := &PrimaryNotifier{
		Revisions: []PrimaryNotifierRevision{{Name: "broken"}},
		Events:    newNotifierTestEvents(),
		Logger:    zerolog.Nop(),
	}
	err := notifier.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), `"broken"`) {
		t.Fatalf("Run() error = %v, want a missing-Read error naming the revision", err)
	}
}

func TestPrimaryNotifierContextCancellationReturnsPromptly(t *testing.T) {
	clock := newNotifierTestClock(notifierTestStart)
	events := newNotifierTestEvents()
	notifier := &PrimaryNotifier{
		Sources:   []func() <-chan struct{}{newNotifierTestSource().Changes},
		Revisions: notifierTestRevisions(newNotifierTestRevision(7)),
		Events:    events,
		Logger:    zerolog.Nop(),
		Now:       clock.Now,
	}

	cancel, done := runNotifierTest(t, notifier)
	events.waitForPairs(t, 1)
	cancel()
	requireNotifierStopped(t, done, context.Canceled)
}

// TestPrimaryNotifierMatchesModel drives the notifier through random sequences
// of source fires, unsignalled commits (revision moves) and clock advances,
// and checks it against a model of the contract after every step:
//   - a source fire publishes exactly once, at once;
//   - a tick publishes exactly once if the revision moved since the value
//     read before the last publish, and otherwise reads it and stays quiet;
//   - nothing else publishes, and every publish and tick reads the revision
//     exactly once.
func TestPrimaryNotifierMatchesModel(t *testing.T) {
	const interval = primaryNotifierFallbackInterval
	advances := []time.Duration{interval / 2, interval - time.Nanosecond, interval, 3 * interval}
	for seed := int64(1); seed <= 8; seed++ {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			random := rand.New(rand.NewSource(seed))
			clock := newNotifierTestClock(notifierTestStart)
			sources := []*notifierTestSource{newNotifierTestSource(), newNotifierTestSource()}
			revision := newNotifierTestRevision(0)
			events := newNotifierTestEvents()
			notifier := &PrimaryNotifier{
				Sources:   []func() <-chan struct{}{sources[0].Changes, sources[1].Changes},
				Revisions: notifierTestRevisions(revision),
				Events:    events,
				Logger:    zerolog.Nop(),
				Now:       clock.Now,
			}
			cancel, done := runNotifierTest(t, notifier)

			publishes, reads := 1, 1
			sinceTick := time.Duration(0)
			moved := false
			events.waitForPairs(t, publishes)
			revision.waitForReads(t, reads)

			var trace []string
			for step := 0; step < 12; step++ {
				switch random.Intn(3) {
				case 0:
					i := random.Intn(len(sources))
					trace = append(trace, fmt.Sprintf("fire source %d", i))
					sources[i].Fire()
					publishes++
					reads++
					moved = false
				case 1:
					trace = append(trace, "unsignalled commit")
					revision.Set(revision.Value() + 1)
					moved = true
				default:
					delta := advances[random.Intn(len(advances))]
					trace = append(trace, fmt.Sprintf("advance %s", delta))
					clock.Advance(delta)
					sinceTick += delta
					if sinceTick >= interval {
						sinceTick = 0
						reads++
						if moved {
							publishes++
							reads++
							moved = false
						}
					}
				}
				t.Logf("step %d: %s -> want %d publishes, %d reads", step, trace[len(trace)-1], publishes, reads)
				events.waitForPairs(t, publishes)
				revision.waitForReads(t, reads)
				events.requireStablePairs(t, publishes)
				if got := revision.Reads(); got != reads {
					t.Fatalf("revision reads = %d, want %d after %v", got, reads, trace)
				}
			}

			cancel()
			requireNotifierStopped(t, done, context.Canceled)
		})
	}
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

var errNotifierTestRevision = errors.New("revision unavailable")

func notifierTestRevisions(revisions ...*notifierTestRevision) []PrimaryNotifierRevision {
	out := make([]PrimaryNotifierRevision, len(revisions))
	for i, revision := range revisions {
		out[i] = PrimaryNotifierRevision{Name: fmt.Sprintf("test-%d", i), Read: revision.Read}
	}
	return out
}

// notifierTestRevision stands in for the store's data_version probe.
type notifierTestRevision struct {
	mu       sync.Mutex
	value    int64
	reads    int
	failNext int
	readWake chan struct{}
	// onRead runs inside Read after it is counted and before it returns;
	// set it before the notifier starts.
	onRead func(call int)
}

func newNotifierTestRevision(value int64) *notifierTestRevision {
	return &notifierTestRevision{value: value, readWake: make(chan struct{})}
}

func (r *notifierTestRevision) Read(context.Context) (int64, error) {
	r.mu.Lock()
	r.reads++
	call := r.reads
	value := r.value
	var err error
	if r.failNext > 0 {
		r.failNext--
		err = errNotifierTestRevision
	}
	close(r.readWake)
	r.readWake = make(chan struct{})
	r.mu.Unlock()
	if r.onRead != nil {
		r.onRead(call)
	}
	if err != nil {
		return 0, err
	}
	return value, nil
}

func (r *notifierTestRevision) Set(value int64) {
	r.mu.Lock()
	r.value = value
	r.mu.Unlock()
}

func (r *notifierTestRevision) Value() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.value
}

func (r *notifierTestRevision) FailReads(n int) {
	r.mu.Lock()
	r.failNext = n
	r.mu.Unlock()
}

func (r *notifierTestRevision) Reads() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reads
}

func (r *notifierTestRevision) waitForReads(t *testing.T, want int) {
	t.Helper()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for {
		r.mu.Lock()
		reads, wake := r.reads, r.readWake
		r.mu.Unlock()
		if reads >= want {
			return
		}
		select {
		case <-wake:
		case <-deadline.C:
			t.Fatalf("timed out waiting for %d revision reads; got %d", want, reads)
		}
	}
}

type notifierTestEvents struct {
	mu                sync.Mutex
	messages          int
	conversations     int
	conversationIDs   []string
	completedPairWake chan struct{}
	// onPublishMessages runs inside PublishMessages after it is counted; set
	// it before the notifier starts.
	onPublishMessages func(call int)
}

func newNotifierTestEvents() *notifierTestEvents {
	return &notifierTestEvents{completedPairWake: make(chan struct{})}
}

func (e *notifierTestEvents) PublishMessages(conversationID string) {
	e.mu.Lock()
	e.messages++
	call := e.messages
	e.conversationIDs = append(e.conversationIDs, conversationID)
	e.mu.Unlock()
	if e.onPublishMessages != nil {
		e.onPublishMessages(call)
	}
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

// notifierTestLog is a goroutine-safe zerolog sink.
type notifierTestLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *notifierTestLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *notifierTestLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func (l *notifierTestLog) count(substring string) int {
	return strings.Count(l.String(), substring)
}
