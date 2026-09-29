package sqlite

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

// nearDuplicateRecorder is a NearDuplicateCheck matcher that records every
// candidate body it is shown, in order, and matches the configured bodies.
type nearDuplicateRecorder struct {
	seen    []string
	matches map[string]bool
}

func (r *nearDuplicateRecorder) check(sinceMS int64, limit int) *NearDuplicateCheck {
	return &NearDuplicateCheck{
		SinceMS: sinceMS,
		Limit:   limit,
		Matches: func(prior string) bool {
			r.seen = append(r.seen, prior)
			return r.matches[prior]
		},
	}
}

func openNearDuplicateTestRepository(t *testing.T, clock *outboxTestClock) (*Store, *OutboxRepository) {
	t.Helper()
	store, repository := openOutboxTestRepository(t, clock.Now)
	seedMessageIdentity(t, store, "identity-a", "account-a")
	seedMessageConversation(t, store, "conversation-a", "account-a")
	seedMessageConversation(t, store, "conversation-b", "account-a")
	return store, repository
}

func enqueueGuardedText(
	t *testing.T,
	repository *OutboxRepository,
	item NewOutboxItem,
	body string,
	check *NearDuplicateCheck,
) (OutboxItem, EnqueueDisposition, error) {
	t.Helper()
	return repository.EnqueueOutgoingMessageGuarded(
		context.Background(),
		item,
		outboxTestOutgoingMessage(item, body),
		check,
	)
}

// The check is consulted only when the idempotency key is new: a replay (or
// a conflicting reuse) of an existing key resolves before it.
func TestEnqueueGuardedRunsOnlyForNewIntents(t *testing.T) {
	clock := newOutboxTestClock(outboxTestTimeMS)
	store, repository := openNearDuplicateTestRepository(t, clock)

	prior := outboxTestItem("prior")
	mustEnqueueOutgoingOutbox(t, repository, prior, "prior body")

	recorder := &nearDuplicateRecorder{}
	fresh := outboxTestItem("fresh")
	row, disposition, err := enqueueGuardedText(t, repository, fresh, "fresh body", recorder.check(1, 8))
	if err != nil || disposition != EnqueueInserted || row.OutboxID != fresh.OutboxID {
		t.Fatalf("guarded new intent = (%+v, %q, %v), want inserted", row, disposition, err)
	}
	if !reflect.DeepEqual(recorder.seen, []string{"prior body"}) {
		t.Fatalf("new intent compared against %q, want only the prior (never itself)", recorder.seen)
	}

	// Replaying the new intent's key with a check that matches everything:
	// the check must not run, and the stored intent comes back.
	replayRecorder := &nearDuplicateRecorder{matches: map[string]bool{"prior body": true, "fresh body": true}}
	replay := fresh
	replay.OutboxID = "outbox-unused-replay"
	replay.LocalMessageID = "message-unused-replay"
	replay.TransportRequestID = "request-unused-replay"
	row, disposition, err = enqueueGuardedText(t, repository, replay, "fresh body", replayRecorder.check(1, 8))
	if err != nil || disposition != EnqueueExisting || row.OutboxID != fresh.OutboxID {
		t.Fatalf("guarded replay = (%+v, %q, %v), want the existing intent", row, disposition, err)
	}
	conflict := replay
	conflict.PayloadHash = "different-payload"
	if _, _, err := enqueueGuardedText(t, repository, conflict, "changed body", replayRecorder.check(1, 8)); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("guarded conflicting reuse error = %v, want ErrIdempotencyConflict", err)
	}
	if len(replayRecorder.seen) != 0 {
		t.Fatalf("existing-key enqueues consulted the check with %q", replayRecorder.seen)
	}

	// A nil check is exactly EnqueueOutgoingMessage.
	unguarded := outboxTestItem("unguarded")
	if _, disposition, err := enqueueGuardedText(t, repository, unguarded, "prior body", nil); err != nil || disposition != EnqueueInserted {
		t.Fatalf("nil check enqueue = (%q, %v), want inserted", disposition, err)
	}
	assertRowCount(t, store.db, "outbox", 3)
	assertRowCount(t, store.db, "messages", 3)
}

// A match refuses the whole enqueue: the inserted outbox row is rolled back,
// no message is written, and the key remains free.
func TestEnqueueGuardedRollsBackOnMatch(t *testing.T) {
	clock := newOutboxTestClock(outboxTestTimeMS)
	store, repository := openNearDuplicateTestRepository(t, clock)

	prior := outboxTestItem("prior")
	mustEnqueueOutgoingOutbox(t, repository, prior, "lunch tomorrow")
	clock.Set(outboxTestTimeMS + 1_500)

	recorder := &nearDuplicateRecorder{matches: map[string]bool{"lunch tomorrow": true}}
	blocked := outboxTestItem("blocked")
	_, _, err := enqueueGuardedText(t, repository, blocked, "lunch today", recorder.check(1, 8))
	if !errors.Is(err, ErrNearDuplicate) {
		t.Fatalf("matched enqueue error = %v, want ErrNearDuplicate", err)
	}
	var nearDuplicate *NearDuplicateError
	if !errors.As(err, &nearDuplicate) {
		t.Fatalf("error %v does not unwrap to *NearDuplicateError", err)
	}
	want := RecentTextIntent{
		OutboxID:       prior.OutboxID,
		IdempotencyKey: prior.IdempotencyKey,
		State:          OutboxQueued,
		Body:           "lunch tomorrow",
		CreatedAtMS:    outboxTestTimeMS,
	}
	if nearDuplicate.Prior != want {
		t.Fatalf("prior = %+v, want %+v", nearDuplicate.Prior, want)
	}
	assertRowCount(t, store.db, "outbox", 1)
	assertRowCount(t, store.db, "messages", 1)
	if _, err := repository.FindByID(context.Background(), blocked.OutboxID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("FindByID(refused) error = %v, want ErrNotFound", err)
	}

	row, disposition, err := enqueueGuardedText(t, repository, blocked, "lunch today", nil)
	if err != nil || disposition != EnqueueInserted || row.OutboxID != blocked.OutboxID {
		t.Fatalf("reusing the refused key = (%+v, %q, %v), want a fresh insert", row, disposition, err)
	}
}

// Candidates are the newest limit text intents in the same account and
// conversation created at or after SinceMS, other than rejected or canceled
// ones, in created_at DESC, outbox_id DESC order.
func TestEnqueueGuardedExcludesRejectedCanceledSelfAndOtherConversation(t *testing.T) {
	clock := newOutboxTestClock(outboxTestTimeMS)
	store, repository := openNearDuplicateTestRepository(t, clock)
	ctx := context.Background()

	at := func(offsetMS int64) { clock.Set(outboxTestTimeMS + offsetMS) }
	text := func(id, conversationID, body string) NewOutboxItem {
		item := outboxTestItem(id)
		item.ConversationID = conversationID
		mustEnqueueOutgoingOutbox(t, repository, item, body)
		return item
	}
	setState := func(item NewOutboxItem, state OutboxState) {
		t.Helper()
		if _, err := store.db.Exec(`UPDATE outbox SET state = ? WHERE outbox_id = ?`, state, item.OutboxID); err != nil {
			t.Fatalf("set %s state: %v", item.OutboxID, err)
		}
	}

	at(0)
	text("too-old", "conversation-a", "too old")
	at(1_000)
	text("at-boundary", "conversation-a", "at boundary")
	at(2_000)
	setState(text("rejected", "conversation-a", "rejected"), OutboxRejected)
	setState(text("canceled", "conversation-a", "canceled"), OutboxCanceled)
	setState(text("uncertain", "conversation-a", "uncertain"), OutboxUncertain)
	setState(text("confirmed", "conversation-a", "confirmed"), OutboxConfirmed)
	text("other-conversation", "conversation-b", "other conversation")
	media := outboxTestMediaItem("media")
	if _, _, err := repository.EnqueueOutgoingMediaMessage(
		ctx,
		media,
		outboxTestOutgoingMessage(media, "media caption"),
		outboxTestAttachment(),
	); err != nil {
		t.Fatalf("enqueue media: %v", err)
	}
	at(3_000)
	text("newest-b", "conversation-a", "newest b")
	text("newest-a", "conversation-a", "newest a")

	recorder := &nearDuplicateRecorder{}
	if _, _, err := enqueueGuardedText(t, repository, outboxTestItem("subject"), "subject", recorder.check(outboxTestTimeMS+1_000, 8)); err != nil {
		t.Fatalf("guarded enqueue: %v", err)
	}
	want := []string{"newest b", "newest a", "uncertain", "confirmed", "at boundary"}
	if !reflect.DeepEqual(recorder.seen, want) {
		t.Fatalf("candidates = %q, want %q", recorder.seen, want)
	}

	limited := &nearDuplicateRecorder{}
	if _, _, err := enqueueGuardedText(t, repository, outboxTestItem("subject-limited"), "subject", limited.check(outboxTestTimeMS+1_000, 2)); err != nil {
		t.Fatalf("guarded enqueue with limit: %v", err)
	}
	// The accepted "subject" row now shares created_at 3000 with the two
	// "newest" rows; ties break by outbox_id DESC ("outbox-subject" first),
	// and the new row itself is never a candidate.
	if !reflect.DeepEqual(limited.seen, []string{"subject", "newest b"}) {
		t.Fatalf("limited candidates = %q, want the newest two", limited.seen)
	}
}

func TestEnqueueGuardedRejectsInvalidCheck(t *testing.T) {
	clock := newOutboxTestClock(outboxTestTimeMS)
	store, repository := openNearDuplicateTestRepository(t, clock)
	never := func(string) bool { return true }
	tests := []struct {
		name  string
		check *NearDuplicateCheck
	}{
		{name: "zero limit", check: &NearDuplicateCheck{SinceMS: 1, Limit: 0, Matches: never}},
		{name: "unbounded window", check: &NearDuplicateCheck{SinceMS: 0, Limit: 8, Matches: never}},
		{name: "nil matcher", check: &NearDuplicateCheck{SinceMS: 1, Limit: 8}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, _, err := enqueueGuardedText(t, repository, outboxTestItem("invalid"), "body", test.check); err == nil {
				t.Fatal("invalid check accepted")
			}
		})
	}
	media := outboxTestMediaItem("media-check")
	message := outboxTestOutgoingMessage(media, "caption")
	if err := validateNearDuplicateCheck(media, &message, &NearDuplicateCheck{SinceMS: 1, Limit: 8, Matches: never}); err == nil {
		t.Fatal("near-duplicate check accepted a media intent")
	}
	if err := validateNearDuplicateCheck(outboxTestItem("no-message"), nil, &NearDuplicateCheck{SinceMS: 1, Limit: 8, Matches: never}); err == nil {
		t.Fatal("near-duplicate check accepted an intent without a message")
	}
	assertRowCount(t, store.db, "outbox", 0)
	assertRowCount(t, store.db, "messages", 0)
}
