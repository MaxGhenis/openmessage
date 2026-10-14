package whatsapplive

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"runtime"
	"sync"
	"testing"
	"testing/quick"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/appstate"
	waevents "go.mau.fi/whatsmeow/types/events"
)

// referenceResyncDecision is the resync budget stated over a collection's
// whole start history, with nothing pruned: no start while one is in flight,
// fewer than maxStarts starts within the window ending now, and none within
// the cooldown.
func referenceResyncDecision(p appStateResyncPolicy, history []time.Time, inFlight bool, now time.Time) appStateResyncDecision {
	if inFlight {
		return appStateResyncInFlight
	}
	inWindow := 0
	for _, start := range history {
		if now.Sub(start) < p.window {
			inWindow++
		}
	}
	if inWindow >= p.maxStarts {
		return appStateResyncCapped
	}
	if len(history) > 0 && now.Sub(history[len(history)-1]) < p.cooldown {
		return appStateResyncCoolingDown
	}
	return appStateResyncStart
}

// TestAppStateResyncBudgetMatchesTheReferenceModel drives the budget with
// random policies and random sequences of errors, finished resyncs and clock
// advances (including advances that land exactly on a cooldown or window
// boundary). Every decision must match the reference model, and the starts
// must satisfy the invariants: at most one in flight, at least the cooldown
// apart, at most maxStarts within any window. When a resync is refused while
// none is in flight, nextStart must be the first moment admit starts one.
func TestAppStateResyncBudgetMatchesTheReferenceModel(t *testing.T) {
	durations := []time.Duration{0, time.Nanosecond, time.Minute, 10 * time.Minute, time.Hour, 24 * time.Hour}
	pick := func(rng *rand.Rand) time.Duration {
		if rng.Intn(3) == 0 {
			return time.Duration(rng.Int63n(int64(30 * time.Hour)))
		}
		return durations[rng.Intn(len(durations))]
	}
	property := func(seed int64) bool {
		rng := rand.New(rand.NewSource(seed))
		policy := appStateResyncPolicy{cooldown: pick(rng), maxStarts: rng.Intn(5), window: pick(rng)}
		budget := &appStateResyncBudget{}
		var history []time.Time
		inFlight := false
		now := time.Unix(1_760_000_000, 0)
		for step := 0; step < 200; step++ {
			switch rng.Intn(4) {
			case 0:
				// Advance to a boundary the decision depends on, or by a
				// random step. The clock never goes back: the bridge reads
				// time.Now, whose monotonic reading only moves forward.
				next := now.Add(pick(rng))
				switch {
				case len(history) > 0 && rng.Intn(2) == 0:
					next = history[len(history)-1].Add(policy.cooldown)
				case len(history) > 0 && rng.Intn(2) == 0:
					next = history[rng.Intn(len(history))].Add(policy.window)
				}
				if next.After(now) {
					now = next
				}
			case 1:
				// The resync in flight returns.
				budget.inFlight = false
				inFlight = false
			default:
				want := referenceResyncDecision(policy, history, inFlight, now)
				got := policy.admit(budget, now)
				if got != want {
					t.Logf("seed %d step %d: policy %+v, history %v, in flight %v, now %v: admit = %s, reference = %s", seed, step, policy, history, inFlight, now, got, want)
					return false
				}
				if got == appStateResyncStart {
					if inFlight {
						t.Logf("seed %d step %d: a second resync started while one was in flight", seed, step)
						return false
					}
					history = append(history, now)
					inFlight = true
				} else if !inFlight && policy.maxStarts > 0 {
					// A refusal only prunes the window, so budget is what
					// the bridge logs nextStart from.
					next := policy.nextStart(budget)
					early := cloneResyncBudget(budget)
					onTime := cloneResyncBudget(budget)
					if !next.After(now) {
						t.Logf("seed %d step %d: policy %+v, history %v: refused at %v with nextStart %v", seed, step, policy, history, now, next)
						return false
					}
					if policy.admit(&early, next.Add(-time.Nanosecond)) == appStateResyncStart {
						t.Logf("seed %d step %d: policy %+v, history %v: a resync starts before nextStart %v", seed, step, policy, history, next)
						return false
					}
					if policy.admit(&onTime, next) != appStateResyncStart {
						t.Logf("seed %d step %d: policy %+v, history %v: no resync starts at nextStart %v", seed, step, policy, history, next)
						return false
					}
				}
			}
		}
		for i, start := range history {
			if i > 0 && start.Sub(history[i-1]) < policy.cooldown {
				t.Logf("seed %d: starts %v and %v are closer than the cooldown %v", seed, history[i-1], start, policy.cooldown)
				return false
			}
			inWindow := 0
			for _, earlier := range history[:i+1] {
				if start.Sub(earlier) < policy.window {
					inWindow++
				}
			}
			if inWindow > policy.maxStarts {
				t.Logf("seed %d: %d starts within the window ending %v, cap %d", seed, inWindow, start, policy.maxStarts)
				return false
			}
		}
		return true
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 2000, Rand: rand.New(rand.NewSource(238))}); err != nil {
		t.Fatal(err)
	}
}

func cloneResyncBudget(budget *appStateResyncBudget) appStateResyncBudget {
	clone := *budget
	clone.starts = append([]time.Time(nil), budget.starts...)
	return clone
}

func TestNeedsFullResyncOnlyForStateMismatches(t *testing.T) {
	wrapPatch := func(err error) error {
		return fmt.Errorf("failed to decode app state critical_unblock_low patches: %w", fmt.Errorf("failed to verify patch v7: %w", err))
	}
	for _, test := range []struct {
		err  error
		want bool
	}{
		{wrapPatch(appstate.ErrMismatchingLTHash), true},
		{fmt.Errorf("failed to verify snapshot: %w", fmt.Errorf("failed to verify patch v9: %w", appstate.ErrMismatchingLTHash)), true},
		{fmt.Errorf("failed to update state hash: %w", appstate.ErrMissingPreviousSetValueOperation), true},
		{wrapPatch(appstate.ErrMismatchingPatchMAC), false},
		{wrapPatch(appstate.ErrMismatchingContentMAC), false},
		{wrapPatch(appstate.ErrMismatchingIndexMAC), false},
		{wrapPatch(appstate.ErrKeyNotFound), false},
		{wrapPatch(context.Canceled), false},
		{errors.New("failed to update state hash: failed to get value MAC of previous SET operation: database is locked"), false},
		{nil, false},
	} {
		if got := needsFullResync(test.err); got != test.want {
			t.Errorf("needsFullResync(%v) = %v, want %v", test.err, got, test.want)
		}
	}
}

// TestStoppedResyncerStartsNothing checks a retired client's resyncer: an
// error after stop starts no resync, and stop cancels the one in flight.
func TestStoppedResyncerStartsNothing(t *testing.T) {
	calls := make(chan context.Context, 4)
	original := fetchAppStateFullSync
	fetchAppStateFullSync = func(ctx context.Context, _ *whatsmeow.Client, _ appstate.WAPatchName) error {
		calls <- ctx
		<-ctx.Done()
		return ctx.Err()
	}
	t.Cleanup(func() { fetchAppStateFullSync = original })
	mismatch := func(name appstate.WAPatchName) *waevents.AppStateSyncError {
		return &waevents.AppStateSyncError{Name: name, Error: fmt.Errorf("failed to verify patch v3: %w", appstate.ErrMismatchingLTHash)}
	}

	resync := newAppStateResyncer(nil, zerolog.Nop())
	handled := make(chan struct{})
	go func() {
		resync.handle(mismatch(appstate.WAPatchCriticalUnblockLow))
		close(handled)
	}()
	select {
	case <-handled:
	case <-time.After(5 * time.Second):
		t.Fatal("handle waited for the resync")
	}
	var running context.Context
	select {
	case running = <-calls:
	case <-time.After(5 * time.Second):
		t.Fatal("no resync started")
	}
	resync.stop()
	if !resync.wait(5 * time.Second) {
		t.Fatal("the resync in flight did not return after stop")
	}
	if !errors.Is(running.Err(), context.Canceled) {
		t.Fatalf("resync context after stop = %v, want cancelled", running.Err())
	}
	resync.handle(mismatch(appstate.WAPatchRegularHigh))
	if resync.inFlight() != 0 {
		t.Fatalf("a stopped resyncer has %d resyncs in flight", resync.inFlight())
	}
	select {
	case <-calls:
		t.Fatal("a stopped resyncer started a resync")
	default:
	}
}

// TestRetiredClientErrorNeverResyncsItsReplacement checks that an
// AppStateSyncError reaches only the resyncer of the client that reported it.
// handleClientEvent checks the source client before applying an event, and
// the client can be replaced between that check and the resyncer lookup; the
// lookup must not then hand the retired client's error to the replacement.
// (Found by the PR #238 review, which raced the two.)
func TestRetiredClientErrorNeverResyncsItsReplacement(t *testing.T) {
	var mu sync.Mutex
	resynced := make(map[*whatsmeow.Client]int)
	original := fetchAppStateFullSync
	fetchAppStateFullSync = func(_ context.Context, cli *whatsmeow.Client, _ appstate.WAPatchName) error {
		mu.Lock()
		resynced[cli]++
		mu.Unlock()
		return nil
	}
	t.Cleanup(func() { fetchAppStateFullSync = original })
	evt := &waevents.AppStateSyncError{Name: appstate.WAPatchCriticalUnblockLow, Error: fmt.Errorf("failed to verify patch v3: %w", appstate.ErrMismatchingLTHash)}
	retired, replacement := &whatsmeow.Client{}, &whatsmeow.Client{}

	t.Run("replaced before the lookup", func(t *testing.T) {
		b := &Bridge{logger: zerolog.Nop(), client: replacement, clientGeneration: 2, appStateResync: newAppStateResyncer(replacement, zerolog.Nop())}
		b.handleAppStateSyncError(retired, 1, evt)
		b.handleAppStateSyncError(replacement, 1, evt) // same client, stale generation
		b.appStateResync.stop()
		b.appStateResync.wait(5 * time.Second)
		mu.Lock()
		defer mu.Unlock()
		if resynced[replacement] != 0 {
			t.Fatalf("a retired client's error resynced its replacement %d time(s)", resynced[replacement])
		}
	})

	t.Run("replaced concurrently", func(t *testing.T) {
		b := &Bridge{logger: zerolog.Nop()}
		for generation := uint64(1); generation <= 2000; generation++ {
			retiredResync := newAppStateResyncer(retired, zerolog.Nop())
			replacementResync := newAppStateResyncer(replacement, zerolog.Nop())
			b.mu.Lock()
			b.client, b.clientGeneration, b.appStateResync = retired, generation, retiredResync
			b.mu.Unlock()
			start := make(chan struct{})
			var wg sync.WaitGroup
			wg.Add(2)
			go func() {
				defer wg.Done()
				<-start
				b.handleClientEvent(retired, generation, evt)
			}()
			go func() {
				defer wg.Done()
				<-start
				runtime.Gosched()
				b.mu.Lock()
				retiredResync.stop()
				b.client, b.clientGeneration, b.appStateResync = replacement, generation+1, replacementResync
				b.mu.Unlock()
			}()
			close(start)
			wg.Wait()
			replacementResync.stop()
			if !retiredResync.wait(5*time.Second) || !replacementResync.wait(5*time.Second) {
				t.Fatal("a resync did not return")
			}
		}
		mu.Lock()
		defer mu.Unlock()
		if resynced[replacement] != 0 {
			t.Fatalf("retired clients' errors resynced the replacement %d time(s)", resynced[replacement])
		}
	})
}
