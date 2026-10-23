package bridge

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"reflect"
	"sync"
	"testing"
	"testing/quick"
	"time"
)

// repairStep is what one credential-repair attempt does in a scripted episode.
type repairStep uint8

const (
	// repairStepTransient: the repairer fails with a recoverable error.
	repairStepTransient repairStep = iota
	// repairStepTerminal: the repairer fails with a terminal OpError.
	repairStepTerminal
	// repairStepIneffective: the repair succeeds, but the new generation is
	// rejected with the same expired-credentials failure.
	repairStepIneffective
	// repairStepHeals: the repair succeeds and the new generation comes up and
	// shows activity.
	repairStepHeals
	repairStepKinds
)

func (s repairStep) String() string {
	return [...]string{"transient", "terminal", "ineffective", "heals"}[s]
}

// repairEpisode is a quick-generated script of repair attempts.
type repairEpisode struct {
	Steps []repairStep
	Seed  int64
}

func (repairEpisode) Generate(r *rand.Rand, _ int) reflect.Value {
	n := 1 + r.Intn(12)
	steps := make([]repairStep, n)
	for i := range steps {
		// Weight transient failures up: they are the incident's case and the
		// one that used to park the account.
		switch roll := r.Intn(10); {
		case roll < 5:
			steps[i] = repairStepTransient
		case roll < 6:
			steps[i] = repairStepTerminal
		case roll < 8:
			steps[i] = repairStepIneffective
		default:
			steps[i] = repairStepHeals
		}
	}
	return reflect.ValueOf(repairEpisode{Steps: steps, Seed: r.Int63()})
}

// repairEpisodeModel is the expected outcome of an episode: which steps run
// before the episode ends, and how it ends.
type repairEpisodeModel struct {
	consumed    int    // steps the supervisor acts on
	starts      int    // lifecycle starts, including the first failed one
	final       State  // state after the last consumed step
	blockReason string // when final is StateBlocked
	// For a final repairing state: the newest attempt's outcome and the
	// episode's attempt count (an ineffective repair immediately starts the
	// next attempt, so it is still running).
	outcome  RepairOutcome
	attempts int
}

func modelRepairEpisode(steps []repairStep, maxSameFingerprint int) repairEpisodeModel {
	model := repairEpisodeModel{starts: 1}
	// The first generation's expiry counts toward the same-fingerprint circuit.
	rejected := 1
	for _, step := range steps {
		model.consumed++
		switch step {
		case repairStepTransient:
			model.final = StateRepairingCredentials
			model.outcome = RepairOutcomeRetrying
			model.attempts = model.consumed
		case repairStepTerminal:
			model.final = StateBlocked
			model.blockReason = BlockReasonRepairFailed
			return model
		case repairStepIneffective:
			model.starts++
			rejected++
			if rejected >= maxSameFingerprint {
				model.final = StateBlocked
				model.blockReason = BlockReasonRepeatedFailure
				return model
			}
			model.final = StateRepairingCredentials
			model.outcome = RepairOutcomeRunning
			model.attempts = model.consumed + 1
		case repairStepHeals:
			model.starts++
			model.final = StateOnline
			return model
		}
	}
	return model
}

// snapshotLog records every snapshot the observer sees.
type snapshotLog struct {
	mu        sync.Mutex
	snapshots []Snapshot
}

func (l *snapshotLog) observe(_, current Snapshot) {
	l.mu.Lock()
	l.snapshots = append(l.snapshots, current)
	l.mu.Unlock()
}

func (l *snapshotLog) all() []Snapshot {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]Snapshot(nil), l.snapshots...)
}

// seededRandom satisfies Random with arbitrary in-range draws.
type seededRandom struct {
	mu sync.Mutex
	r  *rand.Rand
}

func (s *seededRandom) Int63n(n int64) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.r.Int63n(n)
}

// TestSupervisorRepairEpisodeProperties drives random repair episodes through
// the real supervisor and checks, for every episode:
//
//   - a generation starts only after a successful repair (atomic repair);
//   - a recoverable repair failure never parks the account: it is retried no
//     sooner than the floor and no later than max(MaxBackoff, floor);
//   - the account parks only on a terminal repair error or on the
//     same-fingerprint circuit of rejected repairs, and nothing restarts it;
//   - repeated recoverable failures followed by a working repair heal;
//   - BlockReason is set exactly while the state is blocked;
//   - RepairAttempts counts the episode's attempts and resets once the healed
//     generation shows activity.
func TestSupervisorRepairEpisodeProperties(t *testing.T) {
	policy := supervisorTestPolicy()
	policy.MaxSameFingerprint = 3
	policy.RepairRetryFloor = 7 * time.Second
	floor := policy.RepairRetryFloor
	ceiling := policy.MaxBackoff
	expired := OpError{
		Class:       FailureCredentialsExpired,
		Operation:   "listen",
		Fingerprint: "google_auth_expired",
		Cause:       errors.New("HTTP 401: invalid authentication credentials"),
	}
	terminal := OpError{
		Class:       FailureUpgradeRequired,
		Operation:   "refresh",
		Fingerprint: "refresh_unavailable",
		Cause:       errors.New("no refresh mechanism"),
	}

	exercised := map[string]int{}
	property := func(episode repairEpisode) bool {
		model := modelRepairEpisode(episode.Steps, policy.MaxSameFingerprint)
		scripts := []supervisorStartScript{{err: expired}}
		runs := map[int]*supervisorTestRun{}
		for _, step := range episode.Steps[:model.consumed] {
			switch step {
			case repairStepIneffective:
				scripts = append(scripts, supervisorStartScript{err: expired})
			case repairStepHeals:
				run := newSupervisorTestRun()
				runs[len(scripts)] = run
				scripts = append(scripts, supervisorStartScript{run: run})
			}
		}
		clock := newSupervisorManualClock(supervisorTestEpoch)
		lifecycle := &supervisorTestLifecycle{scripts: scripts}
		repairer := newSupervisorControlledRepairer()
		observed := &snapshotLog{}
		supervisor, err := NewSupervisor(
			"account-1",
			Platform("test"),
			lifecycle,
			policy,
			clock,
			&seededRandom{r: rand.New(rand.NewSource(episode.Seed))},
			WithCredentialRepairer(repairer),
			WithSnapshotObserver(observed.observe),
		)
		if err != nil {
			t.Fatalf("NewSupervisor() error = %v", err)
		}
		defer func() {
			ctx, cancel := context.WithTimeout(context.Background(), supervisorTestTimeout)
			defer cancel()
			if err := supervisor.Stop(ctx); err != nil {
				t.Errorf("Supervisor.Stop() error = %v", err)
			}
		}()
		fail := func(format string, args ...any) bool {
			t.Logf("episode %v: %s", episode.Steps, fmt.Sprintf(format, args...))
			return false
		}

		if err := supervisorStart(t, supervisor, StartRequest{}); err == nil {
			return fail("Start() error = nil, want the expired-credentials failure")
		}
		starts := 1
		consecutive := 0
		for index, step := range episode.Steps[:model.consumed] {
			attempt := receiveSupervisorValue(t, repairer.started, "repair attempt start")
			if attempt.failure.Class != FailureCredentialsExpired {
				return fail("step %d repaired %+v, want the expired-credentials trigger", index, attempt.failure)
			}
			if got := lifecycle.CallCount(); got != starts {
				return fail("step %d: %d starts during a repair, want %d", index, got, starts)
			}
			switch step {
			case repairStepTransient:
				exercised["transient"]++
				sendSupervisorValue(t, repairer.responses, fmt.Errorf("chrome cookie copy %d failed", index), "transient failure")
				waiting := awaitSupervisorSnapshot(t, supervisor, "retry scheduled", func(snapshot Snapshot) bool {
					return snapshot.LastRepairOutcome == RepairOutcomeRetrying
				})
				if waiting.State != StateRepairingCredentials {
					return fail("step %d: state %q after a recoverable failure", index, waiting.State)
				}
				consecutive++
				if waiting.RepairFailures != consecutive {
					return fail("step %d: RepairFailures = %d, want %d consecutive", index, waiting.RepairFailures, consecutive)
				}
				wait := waiting.RetryAt.Sub(waiting.LastRepairFinishedAt)
				if wait < floor || wait > ceiling {
					return fail("step %d: retry wait %s outside [%s, %s]", index, wait, floor, ceiling)
				}
				clock.Advance(waiting.RetryAt.Sub(clock.Now()) - time.Nanosecond)
				supervisorSync(t, supervisor)
				select {
				case early := <-repairer.started:
					return fail("step %d: repair retried before its backoff: %+v", index, early)
				default:
				}
				if index == model.consumed-1 {
					break
				}
				clock.Advance(time.Nanosecond)
			case repairStepTerminal:
				exercised["terminal"]++
				sendSupervisorValue(t, repairer.responses, error(terminal), "terminal failure")
			case repairStepIneffective:
				exercised["ineffective"]++
				sendSupervisorValue(t, repairer.responses, error(nil), "ineffective repair")
				starts++
				consecutive = 0
				awaitSupervisorStartCount(t, lifecycle, starts)
			case repairStepHeals:
				exercised["heals"]++
				sendSupervisorValue(t, repairer.responses, error(nil), "healing repair")
				starts++
				calls := awaitSupervisorStartCount(t, lifecycle, starts)
				runs[starts-1].MarkReady()
				online := awaitSupervisorSnapshot(t, supervisor, "healed online", func(snapshot Snapshot) bool {
					return snapshot.State == StateOnline
				})
				if online.LastRepairOutcome != RepairOutcomeSucceeded || online.LastRepairError != "" {
					return fail("healed snapshot %+v, want a recorded success", online)
				}
				calls[starts-1].sink.Beat(online.Generation, clock.Now(), "activity")
				awaitSupervisorSnapshot(t, supervisor, "episode reset by activity", func(snapshot Snapshot) bool {
					return snapshot.RepairAttempts == 0
				})
			}
		}

		var final Snapshot
		switch model.final {
		case StateBlocked:
			final = awaitSupervisorSnapshot(t, supervisor, "blocked", func(snapshot Snapshot) bool {
				return snapshot.State == StateBlocked
			})
			if final.BlockReason != model.blockReason {
				return fail("block reason %q, want %q", final.BlockReason, model.blockReason)
			}
			exercised["blocked:"+final.BlockReason]++
			clock.Advance(24 * time.Hour)
			supervisorSync(t, supervisor)
			select {
			case late := <-repairer.started:
				return fail("a blocked account repaired again on its own: %+v", late)
			default:
			}
			// Only a user action leaves Blocked: it repairs again from a fresh
			// episode, and the block reason goes with the blocked state.
			if err := supervisorRetryBlocked(t, supervisor); err != nil {
				return fail("RetryBlocked() error = %v", err)
			}
			receiveSupervisorValue(t, repairer.started, "repair after a user retry")
			retried := supervisor.Snapshot()
			if retried.State != StateRepairingCredentials || retried.BlockReason != "" || retried.RepairAttempts != 1 {
				return fail("after RetryBlocked: %+v, want a fresh repair with no block reason", retried)
			}
			sendSupervisorValue(t, repairer.responses, error(terminal), "park again")
			awaitSupervisorSnapshot(t, supervisor, "parked again", func(snapshot Snapshot) bool {
				return snapshot.State == StateBlocked
			})

		case StateRepairingCredentials:
			final = awaitSupervisorSnapshot(t, supervisor, "still repairing", func(snapshot Snapshot) bool {
				return snapshot.LastRepairOutcome == model.outcome && snapshot.RepairAttempts == model.attempts
			})
			if final.State != StateRepairingCredentials {
				return fail("final state %q, want repairing_credentials", final.State)
			}
		case StateOnline:
			final = supervisor.Snapshot()
			if final.State != StateOnline {
				return fail("final state %q, want online", final.State)
			}
			exercised["healed"]++
		}
		supervisorSync(t, supervisor)
		if got := lifecycle.CallCount(); got != model.starts {
			return fail("lifecycle starts = %d, want %d", got, model.starts)
		}

		// Whole-history invariants over every published transition.
		attempts := 0
		var lastFailureAt time.Time
		for _, snapshot := range observed.all() {
			if (snapshot.State == StateBlocked) != (snapshot.BlockReason != "") {
				return fail("snapshot %+v: BlockReason must be set exactly while blocked", snapshot)
			}
			if snapshot.LastRepairOutcome == RepairOutcomeRunning && snapshot.RepairAttempts > attempts {
				attempts = snapshot.RepairAttempts
				if !lastFailureAt.IsZero() && snapshot.LastRepairStartedAt.Sub(lastFailureAt) < floor {
					return fail("attempt %d started %s after a failure, under the %s floor",
						attempts, snapshot.LastRepairStartedAt.Sub(lastFailureAt), floor)
				}
				lastFailureAt = time.Time{}
			}
			if snapshot.LastRepairOutcome == RepairOutcomeRetrying {
				lastFailureAt = snapshot.LastRepairFinishedAt
			}
			if snapshot.State == StateBlocked && snapshot.LastRepairOutcome == RepairOutcomeRetrying {
				return fail("parked after a recoverable failure: %+v", snapshot)
			}
		}
		wantAttempts := model.consumed
		if model.final == StateRepairingCredentials {
			wantAttempts = model.attempts
		}
		if model.final != StateOnline && final.RepairAttempts != wantAttempts {
			return fail("RepairAttempts = %d, want %d", final.RepairAttempts, wantAttempts)
		}
		return true
	}

	if err := quick.Check(property, &quick.Config{
		MaxCount: 250,
		Rand:     rand.New(rand.NewSource(20261022)),
	}); err != nil {
		t.Fatalf("repair episode property: %v", err)
	}
	for _, branch := range []string{"transient", "terminal", "ineffective", "heals", "healed",
		"blocked:" + BlockReasonRepairFailed, "blocked:" + BlockReasonRepeatedFailure} {
		if exercised[branch] == 0 {
			t.Fatalf("property never exercised %q (coverage %v)", branch, exercised)
		}
	}
}

// TestSupervisorRepairRetryDelayBounds checks the backoff draw itself over
// arbitrary policies, failure counts, and random draws: the wait is never
// under the floor or over max(MaxBackoff, floor), and the window it draws
// from doubles per consecutive failure.
func TestSupervisorRepairRetryDelayBounds(t *testing.T) {
	property := func(minSeconds, maxExtra, floorSeconds uint16, failures uint8, draw int64) bool {
		minimum := time.Duration(minSeconds%600+1) * time.Second
		policy := supervisorTestPolicy()
		policy.MinBackoff = minimum
		policy.MaxBackoff = minimum + time.Duration(maxExtra%3600)*time.Second
		policy.RepairRetryFloor = time.Duration(floorSeconds%900) * time.Second
		floor := policy.RepairRetryFloor
		if floor == 0 {
			floor = policy.MinBackoff
		}
		ceiling := policy.MaxBackoff
		if ceiling < floor {
			ceiling = floor
		}
		random := &fixedDrawRandom{draw: draw}
		loop := supervisorLoop{supervisor: &Supervisor{policy: policy, random: random}}
		loop.snapshot.RepairFailures = int(failures%40) + 1
		delay, err := loop.repairRetryDelay()
		if err != nil {
			t.Logf("repairRetryDelay() error = %v", err)
			return false
		}
		if delay < floor || delay > ceiling {
			t.Logf("delay %s outside [%s, %s] for policy %+v failures %d", delay, floor, ceiling, policy, loop.snapshot.RepairFailures)
			return false
		}
		// The window doubles per consecutive failure: floor * 2^(failures-1),
		// capped at the ceiling, computed here in closed form rather than with
		// the production helper. A jittered draw must come from exactly
		// [0, window-floor); a window no wider than the floor draws nothing
		// and waits the floor.
		window := ceiling
		if doubled := float64(floor) * math.Pow(2, float64(loop.snapshot.RepairFailures-1)); doubled < float64(ceiling) {
			window = floor << (loop.snapshot.RepairFailures - 1)
		}
		if window <= floor {
			if random.bound != 0 || delay != floor {
				t.Logf("window %s ≤ floor %s, but drew [0, %s) and waited %s", window, floor, time.Duration(random.bound), delay)
				return false
			}
			return true
		}
		if random.bound != int64(window-floor) {
			t.Logf("failures %d: drew from [0, %s), want [0, %s)", loop.snapshot.RepairFailures, time.Duration(random.bound), window-floor)
			return false
		}
		return true
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 2000, Rand: rand.New(rand.NewSource(1022))}); err != nil {
		t.Fatalf("repair retry delay property: %v", err)
	}
}

// fixedDrawRandom maps one arbitrary draw into the requested range.
type fixedDrawRandom struct {
	draw  int64
	bound int64
}

func (r *fixedDrawRandom) Int63n(n int64) int64 {
	r.bound = n
	value := r.draw % n
	if value < 0 {
		value += n
	}
	return value
}

func TestSupervisorRetryCredentialRepairNowExpeditesOnlyAWaitingRetry(t *testing.T) {
	clock := newSupervisorManualClock(supervisorTestEpoch)
	runTwo := newSupervisorTestRun()
	expired := OpError{Class: FailureCredentialsExpired, Operation: "listen", Fingerprint: "google_auth_expired"}
	lifecycle := &supervisorTestLifecycle{scripts: []supervisorStartScript{{err: expired}, {run: runTwo}}}
	repairer := newSupervisorControlledRepairer()
	supervisor := newTestSupervisor(
		t,
		lifecycle,
		supervisorTestPolicy(),
		clock,
		&supervisorScriptedRandom{},
		WithCredentialRepairer(repairer),
	)
	retryNow := func() error {
		ctx, cancel := supervisorTestContext(t)
		defer cancel()
		return supervisor.RetryCredentialRepairNow(ctx)
	}

	if err := retryNow(); !errors.Is(err, ErrNoRepairScheduled) {
		t.Fatalf("RetryCredentialRepairNow() while stopped = %v, want ErrNoRepairScheduled", err)
	}
	if err := supervisorStart(t, supervisor, StartRequest{}); err == nil {
		t.Fatal("Start() error = nil, want the expired-credentials failure")
	}
	receiveSupervisorValue(t, repairer.started, "first repair")
	if err := retryNow(); !errors.Is(err, ErrSupervisorBusy) {
		t.Fatalf("RetryCredentialRepairNow() during a repair = %v, want ErrSupervisorBusy", err)
	}
	sendSupervisorValue(t, repairer.responses, errors.New("keychain locked"), "failed repair")
	awaitSupervisorSnapshot(t, supervisor, "retry scheduled", func(snapshot Snapshot) bool {
		return snapshot.LastRepairOutcome == RepairOutcomeRetrying
	})

	if err := retryNow(); err != nil {
		t.Fatalf("RetryCredentialRepairNow() while waiting = %v", err)
	}
	receiveSupervisorValue(t, repairer.started, "expedited repair")
	if got := supervisor.Snapshot(); got.RepairAttempts != 2 || !got.RetryAt.IsZero() {
		t.Fatalf("expedited snapshot = %+v, want attempt 2 and no pending retry", got)
	}
	sendSupervisorValue(t, repairer.responses, error(nil), "repair succeeds")
	awaitSupervisorStartCount(t, lifecycle, 2)
}

func TestSupervisorSnapshotObserverPanicDoesNotStopTheLoop(t *testing.T) {
	clock := newSupervisorManualClock(supervisorTestEpoch)
	run := newSupervisorTestRun()
	lifecycle := &supervisorTestLifecycle{scripts: []supervisorStartScript{{run: run}}}
	supervisor := newTestSupervisor(
		t,
		lifecycle,
		supervisorTestPolicy(),
		clock,
		&supervisorScriptedRandom{},
		WithSnapshotObserver(func(_, _ Snapshot) { panic("observer bug") }),
	)
	if err := supervisorStart(t, supervisor, StartRequest{}); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	run.MarkReady()
	awaitSupervisorSnapshot(t, supervisor, "online despite a panicking observer", func(snapshot Snapshot) bool {
		return snapshot.State == StateOnline
	})
}

func TestRepairErrorTextIsBoundedValidUTF8(t *testing.T) {
	long := errors.New(repeatRune('é', maxRepairErrorText))
	text := repairErrorText(long)
	if len(text) > maxRepairErrorText+len("…") {
		t.Fatalf("len = %d, want at most %d", len(text), maxRepairErrorText+len("…"))
	}
	for i, r := range text {
		if r == '�' {
			t.Fatalf("invalid rune at %d in truncated text", i)
		}
	}
}

func repeatRune(r rune, n int) string {
	out := make([]rune, n)
	for i := range out {
		out[i] = r
	}
	return string(out)
}
