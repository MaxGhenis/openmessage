package messaging

// Near-duplicate guard: replay-first, atomicity, scope, and similarity.
//
// Invariants executed here:
//   - Replay-first: replaying a stored idempotency key with an identical
//     payload returns the stored intent (same OutboxID, Deduplicated) and never
//     ErrDuplicateSend, whatever else was submitted since.
//   - Conflict preserved: replaying a stored key with a different payload is
//     ErrIdempotencyConflict and writes nothing.
//   - Refusal is side-effect free: every ErrDuplicateSend leaves the outbox and
//     messages tables unchanged, and the refused key stays unused.
//   - Mutual exclusion: of concurrent guarded near-duplicate submissions with
//     new keys, exactly one is accepted, across goroutines and store handles.
//   - Scope: the guard runs iff GuardNearDuplicates && !Force; media and
//     SendAgain never run it.
//   - Candidate set: same account and conversation, text only, within the
//     window (inclusive), not rejected/canceled, newest duplicateCandidateLimit.
//   - Similarity: guardSimilarity is symmetric, in [0, 1], 1 on equal
//     non-empty normalized input, 0 on empty input, invariant under whitespace
//     and ASCII case; levenshtein is a metric within its length bounds; the
//     textsNearDuplicate length prefilter is exact.

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"testing/quick"
	"time"
	"unicode"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

// guardedCommonCommand is testCommonCommand opted into the near-duplicate
// guard, as every agent entry point (MCP, CLI) submits.
func guardedCommonCommand(key string) CommonCommand {
	command := testCommonCommand(key)
	command.GuardNearDuplicates = true
	return command
}

func guardedText(key, body string) SendTextCommand {
	return SendTextCommand{CommonCommand: guardedCommonCommand(key), Body: body}
}

func forcedText(key, body string) SendTextCommand {
	command := guardedText(key, body)
	command.Force = true
	return command
}

func requireDuplicateOf(t *testing.T, err error, priorOutboxID string) *DuplicateSendError {
	t.Helper()
	if !errors.Is(err, ErrDuplicateSend) {
		t.Fatalf("error = %v, want ErrDuplicateSend", err)
	}
	var duplicate *DuplicateSendError
	if !errors.As(err, &duplicate) {
		t.Fatalf("error %v does not unwrap to *DuplicateSendError", err)
	}
	if duplicate.PriorOutboxID != priorOutboxID {
		t.Fatalf("prior outbox = %q, want %q", duplicate.PriorOutboxID, priorOutboxID)
	}
	return duplicate
}

func requireReplayOf(t *testing.T, service *MessageService, command SendTextCommand, original Submission) {
	t.Helper()
	replay, err := service.SendText(context.Background(), command)
	if err != nil {
		t.Fatalf("replay of key %q: %v, want the original intent", command.IdempotencyKey, err)
	}
	if replay.OutboxID != original.OutboxID || replay.LocalMessageID != original.LocalMessageID || !replay.Deduplicated {
		t.Fatalf("replay = %+v, want deduplicated original %+v", replay, original)
	}
}

type sendRowCounts struct{ outbox, messages int }

func countSendRows(t *testing.T, raw *sql.DB) sendRowCounts {
	t.Helper()
	var counts sendRowCounts
	if err := raw.QueryRow(
		`SELECT (SELECT COUNT(*) FROM outbox), (SELECT COUNT(*) FROM messages)`,
	).Scan(&counts.outbox, &counts.messages); err != nil {
		t.Fatalf("count outbox and message rows: %v", err)
	}
	return counts
}

const (
	lunchTomorrow = "Lunch tomorrow at noon at Sfoglina?"
	lunchToday    = "Lunch today at noon at Sfoglina?"
)

// E1: a forced sibling under another key must not block the safe replay of
// the first key.
func TestSameKeyReplayReturnsOriginalDespiteForcedSibling(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	service := newMessagingTestService(t, store, newScriptedRegistry("dup-replay-forced", &scriptedTextSender{}), clock)

	first := mustSendText(t, service, guardedText("key-A", lunchTomorrow))
	mustSendText(t, service, forcedText("key-B", lunchTomorrow))
	clock.Advance(time.Second)

	requireReplayOf(t, service, guardedText("key-A", lunchTomorrow), first)
}

// E1c, minimized from a testing/quick counterexample: no force needed. A
// same-body send under a new key after the window closes is accepted, and the
// original key must still replay to the original intent.
func TestSameKeyReplayReturnsOriginalAfterLaterSameBodySend(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	service := newMessagingTestService(t, store, newScriptedRegistry("dup-replay-later", &scriptedTextSender{}), clock)

	const body = "Did you see the game last night?"
	first := mustSendText(t, service, guardedText("key-A", body))
	clock.Advance(DefaultDuplicateWindow + time.Minute)
	second := mustSendText(t, service, guardedText("key-B", body))
	if second.OutboxID == first.OutboxID || second.Deduplicated {
		t.Fatalf("second send = %+v, want a new intent outside the window", second)
	}
	clock.Advance(time.Second)

	requireReplayOf(t, service, guardedText("key-A", body), first)
}

// E1b: a SendAgain successor (never guarded, new key) must not block the
// replay of the predecessor's key.
func TestSameKeyReplayReturnsOriginalAfterSendAgainSibling(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	sender := &scriptedTextSender{steps: []sendStep{sendAgainStepForState(OutboxUncertain)}}
	registry := newScriptedRegistry("dup-replay-send-again", sender)
	registry.setAvailable(true)
	service := newMessagingTestService(t, store, registry, clock)

	first := mustSendText(t, service, guardedText("key-A", lunchTomorrow))
	if processed, err := service.DispatchDue(context.Background(), 1); err != nil || processed != 1 {
		t.Fatalf("DispatchDue() = %d, %v; want 1, nil", processed, err)
	}
	if state := mustDelivery(t, service, first.OutboxID).State; state != OutboxUncertain {
		t.Fatalf("first state = %q, want uncertain", state)
	}
	if _, err := service.SendAgain(context.Background(), first.OutboxID, "key-C"); err != nil {
		t.Fatalf("SendAgain(): %v", err)
	}
	clock.Advance(time.Second)

	requireReplayOf(t, service, guardedText("key-A", lunchTomorrow), first)
}

// Conflict preserved: a stored key replayed with a different body conflicts,
// even when that body is a near-duplicate of another recent intent. The key
// is resolved before the guard is ever consulted.
func TestSameKeyReplayWithDifferentBodyConflictsNotGuard(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store, raw := openReconcileTestStore(t, clock.Now())
	service := newMessagingTestService(t, store, newScriptedRegistry("dup-replay-conflict", &scriptedTextSender{}), clock)

	mustSendText(t, service, guardedText("key-A", lunchTomorrow))
	mustSendText(t, service, forcedText("key-B", lunchToday))
	before := countSendRows(t, raw)

	_, err := service.SendText(context.Background(), guardedText("key-A", "Lunch today at noon at Sfoglina!"))
	if !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("changed-body replay error = %v, want ErrIdempotencyConflict", err)
	}
	if errors.Is(err, ErrDuplicateSend) {
		t.Fatalf("changed-body replay reported a near-duplicate: %v", err)
	}
	if after := countSendRows(t, raw); after != before {
		t.Fatalf("conflicting replay wrote rows: before %+v, after %+v", before, after)
	}
}

// Refusal is side-effect free: nothing is written and the refused key stays
// unused, so a forced resubmission may reuse it.
func TestGuardRefusalLeavesNoRows(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store, raw := openReconcileTestStore(t, clock.Now())
	service := newMessagingTestService(t, store, newScriptedRegistry("dup-refusal-rows", &scriptedTextSender{}), clock)

	first := mustSendText(t, service, guardedText("key-A", lunchTomorrow))
	clock.Advance(2 * time.Minute)
	before := countSendRows(t, raw)

	for attempt := 0; attempt < 2; attempt++ {
		_, err := service.SendText(context.Background(), guardedText("key-B", lunchToday))
		duplicate := requireDuplicateOf(t, err, first.OutboxID)
		if duplicate.PriorAgeMS != (2 * time.Minute).Milliseconds() {
			t.Fatalf("prior age = %dms, want %dms", duplicate.PriorAgeMS, (2 * time.Minute).Milliseconds())
		}
		if duplicate.PriorIdempotencyKey != "key-A" || duplicate.PriorState != OutboxQueued {
			t.Fatalf("duplicate = %+v, want key-A queued", duplicate)
		}
		if after := countSendRows(t, raw); after != before {
			t.Fatalf("refusal %d wrote rows: before %+v, after %+v", attempt, before, after)
		}
	}

	forced, err := service.SendText(context.Background(), forcedText("key-B", lunchToday))
	if err != nil {
		t.Fatalf("forced resubmission with the refused key: %v", err)
	}
	if forced.Deduplicated || forced.OutboxID == first.OutboxID {
		t.Fatalf("forced resubmission = %+v, want a new intent (the refused key was never used)", forced)
	}
	if after := countSendRows(t, raw); after.outbox != before.outbox+1 || after.messages != before.messages+1 {
		t.Fatalf("forced resubmission rows: before %+v, after %+v", before, after)
	}
}

func TestGuardOffNeverBlocks(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	service := newMessagingTestService(t, store, newScriptedRegistry("dup-off", &scriptedTextSender{}), clock)

	// testCommonCommand leaves GuardNearDuplicates at its zero value. Every
	// production text entry point now sets it (d472 Variant B guards all HTTP
	// text submissions), but the service must still honor the zero value.
	for i := 0; i < 3; i++ {
		mustSendText(t, service, SendTextCommand{
			CommonCommand: testCommonCommand(fmt.Sprintf("key-off-%d", i)),
			Body:          lunchTomorrow,
		})
	}
	// A guarded submission still sees those unguarded intents as candidates.
	_, err := service.SendText(context.Background(), guardedText("key-guarded", lunchToday))
	if !errors.Is(err, ErrDuplicateSend) {
		t.Fatalf("guarded send after unguarded repeats = %v, want ErrDuplicateSend", err)
	}
}

// SendAgain is a deliberate resend and is never guarded, even when a
// near-identical guarded intent is recent; its successor is itself a
// candidate for later guarded sends.
func TestSendAgainBypassesGuard(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	sender := &scriptedTextSender{steps: []sendStep{sendAgainStepForState(OutboxUncertain)}}
	registry := newScriptedRegistry("dup-send-again", sender)
	registry.setAvailable(true)
	service := newMessagingTestService(t, store, registry, clock)

	first := mustSendText(t, service, guardedText("key-original", lunchTomorrow))
	if processed, err := service.DispatchDue(context.Background(), 1); err != nil || processed != 1 {
		t.Fatalf("DispatchDue() = %d, %v; want 1, nil", processed, err)
	}
	clock.Advance(time.Second)
	resent, err := service.SendAgain(context.Background(), first.OutboxID, "key-send-again")
	if err != nil {
		t.Fatalf("SendAgain next to a near-duplicate prior: %v", err)
	}
	clock.Advance(time.Second)
	_, err = service.SendText(context.Background(), guardedText("key-third", lunchTomorrow))
	requireDuplicateOf(t, err, resent.OutboxID)
}

// Media is never guarded, and media captions are never candidates for a
// guarded text.
func TestMediaNeverGuarded(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	blobs, _ := newMessagingTestBlobStore(t)
	registry := newScriptedRegistry("dup-media", &scriptedTextSender{})
	registry.setMediaSender(&scriptedMediaSender{})
	service := newMessagingTestServiceWithBlobs(t, store, registry, blobs, clock)

	media := func(key string) SendMediaCommand {
		return SendMediaCommand{
			CommonCommand: guardedCommonCommand(key),
			Content:       bytes.NewReader([]byte("photo bytes")),
			Filename:      "photo.jpg",
			MIME:          "image/jpeg",
			Caption:       lunchTomorrow,
		}
	}
	mustSendMedia(t, service, media("key-media-1"))
	mustSendText(t, service, guardedText("key-text-1", lunchTomorrow))
	mustSendMedia(t, service, media("key-media-2"))
}

func TestDuplicateSendErrorClampsFutureStampedPrior(t *testing.T) {
	err := duplicateSendError(sqlite.RecentTextIntent{
		OutboxID:    "outbox-skewed",
		State:       OutboxQueued,
		CreatedAtMS: messagingTestTime.Add(time.Second).UnixMilli(),
	}, messagingTestTime)
	if err.PriorAgeMS != 0 {
		t.Fatalf("prior age = %d, want 0 for a prior stamped after now", err.PriorAgeMS)
	}
}

// barrierClock releases Now callers in cohorts of parties once armed.
// SendText reads the clock exactly twice per submission: its submission time,
// then enqueue's timestamp immediately before BEGIN. With one goroutine per
// party, the second cohort therefore releases every goroutine at the
// transaction boundary together, after each has finished all of its
// pre-transaction work. (Before the guard moved into the enqueue
// transaction, that pre-transaction work included the guard's candidate
// read, so every party passed the guard.)
type barrierClock struct {
	now     time.Time
	parties int
	timeout time.Duration

	mu       sync.Mutex
	armed    bool
	calls    int
	waiting  int
	release  chan struct{}
	timeouts int
}

func (c *barrierClock) arm() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.armed = true
	c.calls = 0
	c.waiting = 0
	c.release = make(chan struct{})
}

func (c *barrierClock) Now() time.Time {
	c.mu.Lock()
	c.calls++
	if !c.armed {
		c.mu.Unlock()
		return c.now
	}
	release := c.release
	c.waiting++
	if c.waiting == c.parties {
		c.waiting = 0
		c.release = make(chan struct{})
		close(release)
		c.mu.Unlock()
		return c.now
	}
	c.mu.Unlock()
	select {
	case <-release:
	case <-time.After(c.timeout):
		c.mu.Lock()
		c.timeouts++
		c.mu.Unlock()
	}
	return c.now
}

func (c *barrierClock) NewTimer(delay time.Duration) Timer {
	return systemTimer{Timer: time.NewTimer(delay)}
}

func (c *barrierClock) stats() (calls, timeouts int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls, c.timeouts
}

// Mutual exclusion, deterministic: all parties reach BEGIN together.
func TestConcurrentNearDuplicateSubmissions(t *testing.T) {
	const parties = 8
	clock := &barrierClock{now: messagingTestTime, parties: parties, timeout: 10 * time.Second}
	store, raw := openReconcileTestStore(t, messagingTestTime)
	service := newMessagingTestService(t, store, newScriptedRegistry("dup-concurrent", &scriptedTextSender{}), clock)
	clock.arm()

	type outcome struct {
		submission Submission
		err        error
	}
	outcomes := make([]outcome, parties)
	var group sync.WaitGroup
	for party := 0; party < parties; party++ {
		group.Add(1)
		go func(party int) {
			defer group.Done()
			submission, err := service.SendText(
				context.Background(),
				guardedText(fmt.Sprintf("key-concurrent-%d", party), lunchTomorrow),
			)
			outcomes[party] = outcome{submission: submission, err: err}
		}(party)
	}
	group.Wait()

	calls, timeouts := clock.stats()
	if timeouts != 0 || calls != 2*parties {
		t.Fatalf("barrier drift: clock calls = %d (want %d), timeouts = %d (want 0); SendText's clock reads changed, so this test no longer aligns the parties at BEGIN", calls, 2*parties, timeouts)
	}
	var winners []Submission
	var refusals []*DuplicateSendError
	for party, result := range outcomes {
		var duplicate *DuplicateSendError
		switch {
		case result.err == nil:
			winners = append(winners, result.submission)
		case errors.As(result.err, &duplicate):
			refusals = append(refusals, duplicate)
		default:
			t.Fatalf("party %d: unexpected error %v", party, result.err)
		}
	}
	if len(winners) != 1 {
		t.Fatalf("accepted %d of %d concurrent near-duplicates, want exactly 1", len(winners), parties)
	}
	for _, refusal := range refusals {
		if refusal.PriorOutboxID != winners[0].OutboxID {
			t.Fatalf("refusal names %q, want the accepted intent %q", refusal.PriorOutboxID, winners[0].OutboxID)
		}
	}
	if counts := countSendRows(t, raw); counts.outbox != 1 || counts.messages != 1 {
		t.Fatalf("rows after the race = %+v, want exactly one outbox row and one message", counts)
	}
}

type prefixedIDs struct {
	prefix string
	mu     sync.Mutex
	next   int
}

func (s *prefixedIDs) NewID() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next++
	return fmt.Sprintf("%s-%04d", s.prefix, s.next), nil
}

// Mutual exclusion across processes: two independent store handles (the
// in-process stand-in for two serve processes sharing one store file) race a
// near-duplicate each round.
func TestConcurrentNearDuplicatesAcrossStoreHandles(t *testing.T) {
	const rounds = 40
	path := filepath.Join(t.TempDir(), "shared.sqlite")
	clock := newManualClock(messagingTestTime)
	open := func(prefix string) (*sqlite.Store, *MessageService) {
		store, err := sqlite.Open(path)
		if err != nil {
			t.Fatalf("sqlite.Open(%s): %v", prefix, err)
		}
		t.Cleanup(func() { _ = store.Close() })
		service, err := NewMessageService(store, newScriptedRegistry(bridge.Platform("dup-"+prefix), &scriptedTextSender{}), nil, clock, &prefixedIDs{prefix: prefix})
		if err != nil {
			t.Fatalf("NewMessageService(%s): %v", prefix, err)
		}
		return store, service
	}
	storeA, serviceA := open("a")
	_, serviceB := open("b")
	seedMessagingStore(t, storeA, clock.Now())

	bothAccepted := 0
	for round := 0; round < rounds; round++ {
		// Each round starts past the previous round's window.
		clock.Advance(DefaultDuplicateWindow + time.Minute)
		start := make(chan struct{})
		errs := make([]error, 2)
		var group sync.WaitGroup
		for index, service := range []*MessageService{serviceA, serviceB} {
			group.Add(1)
			go func(index int, service *MessageService) {
				defer group.Done()
				<-start
				_, errs[index] = service.SendText(
					context.Background(),
					guardedText(fmt.Sprintf("key-round-%02d-%d", round, index), lunchTomorrow),
				)
			}(index, service)
		}
		close(start)
		group.Wait()
		accepted := 0
		for index, err := range errs {
			switch {
			case err == nil:
				accepted++
			case errors.Is(err, ErrDuplicateSend):
			default:
				t.Fatalf("round %d handle %d: unexpected error %v", round, index, err)
			}
		}
		if accepted == 2 {
			bothAccepted++
		}
		if accepted == 0 {
			t.Fatalf("round %d accepted neither submission", round)
		}
	}
	if bothAccepted != 0 {
		t.Fatalf("both handles accepted a near-duplicate in %d/%d rounds, want 0", bothAccepted, rounds)
	}
}

// --- Arbitrary-history properties -------------------------------------------

var guardBodyPool = []string{
	lunchTomorrow,
	lunchToday,
	"lunch  TOMORROW at noon at sfoglina?",
	"Did you see the game last night?",
	"Did you see the game last night??",
	"ok",
	"ok!",
	"Running 5 minutes late, sorry",
	"Running 10 minutes late, sorry",
	"there at 7",
	"there at 8",
}

// guardAdvances biases clock steps toward the window boundary: exactly the
// window is still inside it (created_at >= now - window), one millisecond more
// is outside.
var guardAdvances = []time.Duration{
	0, 0, 0,
	time.Millisecond,
	time.Minute,
	5 * time.Minute,
	DefaultDuplicateWindow,
	DefaultDuplicateWindow + time.Millisecond,
	11 * time.Minute,
}

type guardOpKind uint8

const (
	guardOpSend guardOpKind = iota
	guardOpReplay
	guardOpCancel
)

type guardOp struct {
	Kind    guardOpKind
	Body    int
	Guard   bool
	Force   bool
	Conv2   bool
	Advance time.Duration
	Target  int
}

type guardHistory struct{ Ops []guardOp }

func (guardHistory) Generate(r *rand.Rand, _ int) reflect.Value {
	ops := make([]guardOp, 1+r.Intn(12))
	for i := range ops {
		// Half the bodies come from the three-way "lunch" near-duplicate
		// cluster so refusals are common, not incidental.
		body := r.Intn(len(guardBodyPool))
		if r.Intn(2) == 0 {
			body = r.Intn(3)
		}
		op := guardOp{
			Body:    body,
			Guard:   r.Intn(4) != 0,
			Force:   r.Intn(4) == 0,
			Conv2:   r.Intn(3) == 0,
			Advance: guardAdvances[r.Intn(len(guardAdvances))],
			Target:  r.Intn(16),
		}
		switch roll := r.Intn(20); {
		case roll < 3:
			op.Kind = guardOpReplay
		case roll < 5:
			op.Kind = guardOpCancel
		}
		ops[i] = op
	}
	return reflect.ValueOf(guardHistory{Ops: ops})
}

type guardIntent struct {
	outboxID       string
	localMessageID string
	key            string
	conversationID string
	body           string
	createdMS      int64
	canceled       bool
}

// guardHistoryEnv runs many histories against one store; each history gets
// its own two conversations, so histories cannot see each other's intents.
type guardHistoryEnv struct {
	t       *testing.T
	clock   *manualClock
	store   *sqlite.Store
	raw     *sql.DB
	service *MessageService
	runs    int
}

func newGuardHistoryEnv(t *testing.T) *guardHistoryEnv {
	t.Helper()
	clock := newManualClock(messagingTestTime)
	store, raw := openReconcileTestStore(t, clock.Now())
	service := newMessagingTestService(t, store, newScriptedRegistry("dup-history", &scriptedTextSender{}), clock)
	return &guardHistoryEnv{t: t, clock: clock, store: store, raw: raw, service: service}
}

type guardHistoryResult struct {
	accepted       []guardIntent
	refusals       int
	guardedAccepts int
	// violations by invariant; each property test asserts its own.
	replay    []string
	model     []string
	invariant []string
}

func (r *guardHistoryResult) failure(kind *[]string, format string, args ...any) {
	*kind = append(*kind, fmt.Sprintf(format, args...))
}

// modelFirstMatch is the reference guard decision: the first match, in
// candidate order, among the newest duplicateCandidateLimit live intents in
// the conversation created at or after max(1, now - window). It uses
// guardSimilarity directly (no prefilter) as the similarity oracle.
func modelFirstMatch(intents []guardIntent, conversationID, body string, now time.Time) (guardIntent, bool) {
	sinceMS := now.Add(-DefaultDuplicateWindow).UnixMilli()
	if sinceMS < 1 {
		sinceMS = 1
	}
	var candidates []guardIntent
	for _, intent := range intents {
		if intent.conversationID == conversationID && !intent.canceled && intent.createdMS >= sinceMS {
			candidates = append(candidates, intent)
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].createdMS != candidates[j].createdMS {
			return candidates[i].createdMS > candidates[j].createdMS
		}
		return candidates[i].outboxID > candidates[j].outboxID
	})
	if len(candidates) > duplicateCandidateLimit {
		candidates = candidates[:duplicateCandidateLimit]
	}
	for _, candidate := range candidates {
		if guardSimilarity(body, candidate.body) >= defaultDuplicateThreshold {
			return candidate, true
		}
	}
	return guardIntent{}, false
}

// dbCandidatesMatch reads the committed store (not the model) and reports any
// live text intent in the new intent's candidate set that is a near-duplicate
// of body.
func (env *guardHistoryEnv) dbCandidatesMatch(conversationID, body, excludeOutboxID string, now time.Time) []string {
	sinceMS := now.Add(-DefaultDuplicateWindow).UnixMilli()
	rows, err := env.raw.Query(`
		SELECT o.outbox_id, COALESCE(m.body, '')
		FROM outbox o
		LEFT JOIN messages m ON m.message_id = o.local_message_id
		WHERE o.account_id = 'account-1'
		  AND o.conversation_id = ?
		  AND o.kind = 'text'
		  AND o.created_at_ms >= ?
		  AND o.state NOT IN ('rejected', 'canceled')
		  AND o.outbox_id <> ?
		ORDER BY o.created_at_ms DESC, o.outbox_id DESC
		LIMIT ?
	`, conversationID, max(sinceMS, 1), excludeOutboxID, duplicateCandidateLimit)
	if err != nil {
		env.t.Fatalf("read candidates: %v", err)
	}
	defer rows.Close()
	var matches []string
	for rows.Next() {
		var outboxID, priorBody string
		if err := rows.Scan(&outboxID, &priorBody); err != nil {
			env.t.Fatalf("scan candidate: %v", err)
		}
		if guardSimilarity(body, priorBody) >= defaultDuplicateThreshold {
			matches = append(matches, outboxID)
		}
	}
	if err := rows.Err(); err != nil {
		env.t.Fatalf("iterate candidates: %v", err)
	}
	return matches
}

func (env *guardHistoryEnv) replay(result *guardHistoryResult, intent guardIntent, when string) {
	before := countSendRows(env.t, env.raw)
	env.replayOnce(result, intent, when)
	if after := countSendRows(env.t, env.raw); after != before {
		result.failure(&result.replay, "%s replay of %s wrote rows: %+v -> %+v", when, intent.key, before, after)
	}
}

func (env *guardHistoryEnv) replayOnce(result *guardHistoryResult, intent guardIntent, when string) {
	submission, err := env.service.SendText(context.Background(), SendTextCommand{
		CommonCommand: CommonCommand{
			AccountID:           "account-1",
			ConversationID:      intent.conversationID,
			IdempotencyKey:      intent.key,
			GuardNearDuplicates: true,
		},
		Body: intent.body,
	})
	switch {
	case err != nil:
		result.failure(&result.replay, "%s replay of %s refused: %v", when, intent.key, err)
	case submission.OutboxID != intent.outboxID || !submission.Deduplicated:
		result.failure(&result.replay, "%s replay of %s = %+v, want deduplicated %s", when, intent.key, submission, intent.outboxID)
	}
}

func (env *guardHistoryEnv) run(history guardHistory) guardHistoryResult {
	env.runs++
	prefix := fmt.Sprintf("h%04d", env.runs)
	conversations := [2]string{prefix + "-conversation-1", prefix + "-conversation-2"}
	for _, conversationID := range conversations {
		seedConversation(env.t, env.store, "account-1", conversationID, env.clock.Now())
	}
	ctx := context.Background()
	var result guardHistoryResult
	for index, op := range history.Ops {
		env.clock.Advance(op.Advance)
		now := env.clock.Now()
		if op.Kind == guardOpReplay && len(result.accepted) > 0 {
			env.replay(&result, result.accepted[op.Target%len(result.accepted)], fmt.Sprintf("op %d", index))
			continue
		}
		if op.Kind == guardOpCancel && len(result.accepted) > 0 {
			target := &result.accepted[op.Target%len(result.accepted)]
			if !target.canceled {
				if _, err := env.service.Cancel(ctx, target.outboxID); err != nil {
					env.t.Fatalf("cancel %s: %v", target.outboxID, err)
				}
				target.canceled = true
			}
			continue
		}

		conversationID := conversations[0]
		if op.Conv2 {
			conversationID = conversations[1]
		}
		body := guardBodyPool[op.Body]
		key := fmt.Sprintf("%s-key-%02d", prefix, index)
		guarded := op.Guard && !op.Force
		prior, wantRefusal := modelFirstMatch(result.accepted, conversationID, body, now)
		wantRefusal = wantRefusal && guarded

		before := countSendRows(env.t, env.raw)
		submission, err := env.service.SendText(ctx, SendTextCommand{
			CommonCommand: CommonCommand{
				AccountID:           "account-1",
				ConversationID:      conversationID,
				IdempotencyKey:      key,
				GuardNearDuplicates: op.Guard,
				Force:               op.Force,
			},
			Body: body,
		})
		after := countSendRows(env.t, env.raw)
		var duplicate *DuplicateSendError
		switch {
		case errors.As(err, &duplicate):
			result.refusals++
			if after != before {
				result.failure(&result.replay, "op %d refusal wrote rows: %+v -> %+v", index, before, after)
			}
			switch {
			case !wantRefusal:
				result.failure(&result.model, "op %d (%q guard=%v force=%v) refused by %s; model accepts", index, body, op.Guard, op.Force, duplicate.PriorOutboxID)
			case duplicate.PriorOutboxID != prior.outboxID:
				result.failure(&result.model, "op %d refused by %s; model names %s", index, duplicate.PriorOutboxID, prior.outboxID)
			case duplicate.PriorAgeMS != now.UnixMilli()-prior.createdMS:
				result.failure(&result.model, "op %d prior age %d; model %d", index, duplicate.PriorAgeMS, now.UnixMilli()-prior.createdMS)
			}
		case err != nil:
			env.t.Fatalf("op %d: unexpected SendText error %v", index, err)
		default:
			if wantRefusal {
				result.failure(&result.model, "op %d (%q) accepted; model refuses as a duplicate of %s", index, body, prior.outboxID)
			}
			if submission.Deduplicated || after.outbox != before.outbox+1 {
				result.failure(&result.model, "op %d new key %s = %+v, rows %+v -> %+v; want one new intent", index, key, submission, before, after)
			}
			if guarded {
				result.guardedAccepts++
				if matches := env.dbCandidatesMatch(conversationID, body, submission.OutboxID, now); len(matches) > 0 {
					result.failure(&result.invariant, "op %d accepted guarded %q beside near-duplicate candidates %v", index, body, matches)
				}
			}
			result.accepted = append(result.accepted, guardIntent{
				outboxID:       submission.OutboxID,
				localMessageID: submission.LocalMessageID,
				key:            key,
				conversationID: conversationID,
				body:           body,
				createdMS:      now.UnixMilli(),
			})
		}
	}

	// Final pass, a second later: every accepted key replays to its intent,
	// and a changed payload under that key conflicts; the pass writes nothing.
	env.clock.Advance(time.Second)
	before := countSendRows(env.t, env.raw)
	for _, intent := range result.accepted {
		env.replayOnce(&result, intent, "final")
		changed := guardBodyPool[0]
		if changed == intent.body {
			changed = guardBodyPool[1]
		}
		_, err := env.service.SendText(ctx, SendTextCommand{
			CommonCommand: CommonCommand{
				AccountID:           "account-1",
				ConversationID:      intent.conversationID,
				IdempotencyKey:      intent.key,
				GuardNearDuplicates: true,
			},
			Body: changed,
		})
		if !errors.Is(err, ErrIdempotencyConflict) || errors.Is(err, ErrDuplicateSend) {
			result.failure(&result.replay, "changed-payload replay of %s = %v, want ErrIdempotencyConflict", intent.key, err)
		}
	}
	if after := countSendRows(env.t, env.raw); after != before {
		result.failure(&result.replay, "final replays wrote rows: %+v -> %+v", before, after)
	}
	return result
}

func guardQuickConfig(seed int64, count int) *quick.Config {
	return &quick.Config{MaxCount: count, Rand: rand.New(rand.NewSource(seed))}
}

// guardHistoryChecks is how many random histories each history property
// runs. The race detector slows the pure-Go SQLite driver by more than an
// order of magnitude, so the race build (which CI also runs) checks a smaller
// prefix of the same deterministic sequence; the concurrency tests above are
// what the race job exists for.
func guardHistoryChecks() int {
	if raceDetectorEnabled {
		return 30
	}
	return 200
}

// Replay-first, conflict preserved, and side-effect-free refusal over
// arbitrary recent histories (the head of #166 failed this; see E1/E1c).
func TestQuickIdempotentReplayWithArbitraryRecentHistory(t *testing.T) {
	env := newGuardHistoryEnv(t)
	var failures []string
	property := func(history guardHistory) bool {
		failures = env.run(history).replay
		return len(failures) == 0
	}
	if err := quick.Check(property, guardQuickConfig(20260929, guardHistoryChecks())); err != nil {
		t.Fatalf("%v\nviolations: %s", err, strings.Join(failures, "\n  "))
	}
}

// Differential: the transactional guard decides exactly like the reference
// model (same accept/refuse, same named prior, same age), and every accepted
// guarded intent has no near-duplicate among its committed candidates.
func TestQuickGuardDecisionsMatchReferenceModel(t *testing.T) {
	env := newGuardHistoryEnv(t)
	var failures []string
	refusals, guardedAccepts := 0, 0
	property := func(history guardHistory) bool {
		result := env.run(history)
		refusals += result.refusals
		guardedAccepts += result.guardedAccepts
		failures = append(result.model, result.invariant...)
		return len(failures) == 0
	}
	if err := quick.Check(property, guardQuickConfig(20260930, guardHistoryChecks())); err != nil {
		t.Fatalf("%v\nviolations: %s", err, strings.Join(failures, "\n  "))
	}
	// Guard against a vacuous generator: both outcomes must be common (at
	// least one of each per ten histories on average).
	if minimum := guardHistoryChecks() / 10; refusals < minimum || guardedAccepts < minimum {
		t.Fatalf("vacuous generator: %d refusals, %d guarded acceptances over %d histories", refusals, guardedAccepts, guardHistoryChecks())
	}
	t.Logf("%d histories: %d refusals, %d guarded acceptances", guardHistoryChecks(), refusals, guardedAccepts)
}

// --- Similarity properties --------------------------------------------------

var guardRuneAlphabet = []rune("aAbBcCdD eé\tß\n😀İ?! ")

type guardText string

func (guardText) Generate(r *rand.Rand, _ int) reflect.Value {
	length := r.Intn(12)
	switch r.Intn(20) {
	case 0, 1:
		length = 0
	case 2:
		// Long enough that the normalized body still exceeds the compare cap
		// after whitespace collapsing, so truncation is exercised.
		length = duplicateCompareMaxRunes + 150 + r.Intn(100)
	}
	runes := make([]rune, length)
	for i := range runes {
		runes[i] = guardRuneAlphabet[r.Intn(len(guardRuneAlphabet))]
	}
	return reflect.ValueOf(guardText(runes))
}

// respace rejoins a body's fields with arbitrary whitespace runs and flips
// ASCII letter case: normalization must erase both.
func respace(r *rand.Rand, value string) string {
	spaces := []string{" ", "  ", "\t", "\n", " \t\n "}
	var builder strings.Builder
	builder.WriteString(spaces[r.Intn(len(spaces))])
	for i, field := range strings.Fields(value) {
		if i > 0 {
			builder.WriteString(spaces[r.Intn(len(spaces))])
		}
		for _, char := range field {
			if char < unicode.MaxASCII && unicode.IsLetter(char) && r.Intn(2) == 0 {
				if unicode.IsUpper(char) {
					char = unicode.ToLower(char)
				} else {
					char = unicode.ToUpper(char)
				}
			}
			builder.WriteRune(char)
		}
	}
	builder.WriteString(spaces[r.Intn(len(spaces))])
	return builder.String()
}

func TestQuickGuardSimilarityProperties(t *testing.T) {
	perturb := rand.New(rand.NewSource(7))
	property := func(a, b guardText) bool {
		x, y := string(a), string(b)
		sim := guardSimilarity(x, y)
		if sim != guardSimilarity(y, x) {
			t.Logf("asymmetric: %q %q", x, y)
			return false
		}
		if sim < 0 || sim > 1 || math.IsNaN(sim) {
			t.Logf("out of bounds %v: %q %q", sim, x, y)
			return false
		}
		emptyX, emptyY := normalizeGuardText(x) == "", normalizeGuardText(y) == ""
		if (emptyX || emptyY) && sim != 0 {
			t.Logf("empty input scored %v: %q %q", sim, x, y)
			return false
		}
		if !emptyX && guardSimilarity(x, x) != 1 {
			t.Logf("identity failed for %q", x)
			return false
		}
		if got := guardSimilarity(respace(perturb, x), y); got != sim {
			t.Logf("whitespace/case variance: %v vs %v for %q %q", got, sim, x, y)
			return false
		}
		// The prefilter is exact: textsNearDuplicate is the similarity
		// comparison, at the default, at random thresholds, and exactly at
		// and just past the pair's own score.
		thresholds := []float64{defaultDuplicateThreshold, perturb.Float64() + math.SmallestNonzeroFloat64, 1}
		if sim > 0 {
			thresholds = append(thresholds, sim, math.Nextafter(sim, 2))
		}
		for _, threshold := range thresholds {
			if threshold <= 0 || threshold > 1 {
				continue
			}
			if got, want := textsNearDuplicate(x, y, threshold), sim >= threshold; got != want {
				t.Logf("prefilter mismatch at %v: textsNearDuplicate=%v similarity=%v for %q %q", threshold, got, sim, x, y)
				return false
			}
		}
		return true
	}
	if err := quick.Check(property, guardQuickConfig(11, 2000)); err != nil {
		t.Fatal(err)
	}
}

func TestQuickLevenshteinIsBoundedMetric(t *testing.T) {
	property := func(a, b, c guardText) bool {
		ra, rb, rc := truncateGuardRunes(string(a)), truncateGuardRunes(string(b)), truncateGuardRunes(string(c))
		dab, dba := levenshtein(ra, rb), levenshtein(rb, ra)
		gap := len(ra) - len(rb)
		if gap < 0 {
			gap = -gap
		}
		return dab == dba &&
			levenshtein(ra, ra) == 0 &&
			gap <= dab && dab <= max(len(ra), len(rb)) &&
			levenshtein(ra, rc) <= dab+levenshtein(rb, rc)
	}
	if err := quick.Check(property, guardQuickConfig(13, 300)); err != nil {
		t.Fatal(err)
	}
}

func TestGuardSimilarityFixtures(t *testing.T) {
	longPrefix := strings.Repeat("ab ", duplicateCompareMaxRunes/3+1)
	tests := []struct {
		name string
		a, b string
		want float64
	}{
		{name: "unicode case folds", a: "Café CRÈME", b: "café crème", want: 1},
		{name: "emoji identical", a: "see you 😀", b: "see  you 😀", want: 1},
		{name: "emoji differs by one rune", a: "ok 😀", b: "ok 😢", want: 0.75},
		{name: "short repeat", a: "ok", b: "ok!", want: 1 - 1.0/3},
		{name: "empty never matches", a: " \t\n", b: " ", want: 0},
		// Intended: only the first duplicateCompareMaxRunes normalized runes
		// are compared, so long bodies sharing that prefix score 1.
		{name: "long identical prefix", a: longPrefix + "tail one", b: longPrefix + "a completely different ending", want: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := guardSimilarity(test.a, test.b); math.Abs(got-test.want) > 1e-12 {
				t.Fatalf("guardSimilarity(%q, %q) = %v, want %v", test.a, test.b, got, test.want)
			}
		})
	}
}
