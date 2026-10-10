package sqlite

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"testing"
	"testing/quick"
	"time"

	"github.com/maxghenis/openmessage/internal/bridge"
)

const confirmReactionTimeMS int64 = 1_780_000_000_000

// confirmReactionFixture is an outbox with one reaction target and the lease
// steps a dispatcher takes before it may confirm.
type confirmReactionFixture struct {
	t         *testing.T
	clock     *outboxTestClock
	store     *Store
	outbox    *OutboxRepository
	reactions *ReactionRepository
	enqueued  int
	targetID  string
}

// retarget points the fixture at a fresh message, so one store can serve many
// independent histories.
func (f *confirmReactionFixture) retarget(messageID string) {
	f.t.Helper()
	seedOutboxTestMessage(f.t, f.store, messageID, "account-a", "conversation-a")
	f.targetID = messageID
}

func newConfirmReactionFixture(t *testing.T) *confirmReactionFixture {
	t.Helper()
	clock := newOutboxTestClock(confirmReactionTimeMS)
	store, outbox := openOutboxTestRepository(t, clock.Now)
	seedMessageConversation(t, store, "conversation-a", "account-a")
	seedOutboxTestMessage(t, store, "target-a", "account-a", "conversation-a")
	seedMessageIdentity(t, store, "identity-a", "account-a")
	seedMessageIdentity(t, store, "identity-other", "account-a")
	reactions, err := NewReactionRepository(store, clock.Now)
	if err != nil {
		t.Fatalf("NewReactionRepository(): %v", err)
	}
	return &confirmReactionFixture{
		t: t, clock: clock, store: store, outbox: outbox, reactions: reactions,
		targetID: "target-a",
	}
}

// called enqueues a reaction and takes it to the point where the transport
// has been called, which is where the dispatcher holds it when the transport
// answers.
func (f *confirmReactionFixture) called(emoji string, action bridge.ReactionAction) (outboxID, leaseToken string) {
	f.t.Helper()
	ctx := context.Background()
	f.enqueued++
	item := outboxTestReactionItem(fmt.Sprintf("reaction-%d", f.enqueued))
	if _, _, err := f.outbox.EnqueueReaction(ctx, item, OutboxReaction{
		TargetMessageID: f.targetID, Emoji: emoji, Action: string(action),
	}); err != nil {
		f.t.Fatalf("EnqueueReaction(): %v", err)
	}
	leased := mustLeaseOne(f.t, f.outbox, LeaseRequest{
		Owner: "dispatcher", Now: f.clock.Now(), Duration: time.Minute, Limit: 1,
	})
	if leased.OutboxID != item.OutboxID {
		f.t.Fatalf("leased %q, want the reaction just enqueued %q", leased.OutboxID, item.OutboxID)
	}
	leaseToken = mustLeaseToken(f.t, leased)
	if err := f.outbox.MarkTransportCalled(ctx, Attempt{
		OutboxID: item.OutboxID, LeaseToken: leaseToken,
		AttemptToken: item.OutboxID + ":" + leaseToken, StartedAt: f.clock.Now(),
	}); err != nil {
		f.t.Fatalf("MarkTransportCalled(): %v", err)
	}
	return item.OutboxID, leaseToken
}

func (f *confirmReactionFixture) state(outboxID string) OutboxState {
	f.t.Helper()
	item, err := f.outbox.FindByID(context.Background(), outboxID)
	if err != nil {
		f.t.Fatalf("FindByID(%q): %v", outboxID, err)
	}
	return item.State
}

func TestConfirmReactionConfirmsAndRecordsTheOwnReactionTogether(t *testing.T) {
	f := newConfirmReactionFixture(t)
	outboxID, leaseToken := f.called("👍", bridge.ReactionAdd)
	acceptedAtMS := confirmReactionTimeMS + 250
	f.clock.Set(confirmReactionTimeMS + 900)

	applied, err := f.outbox.ConfirmReaction(context.Background(), ReactionConfirmation{
		OutboxID: outboxID, LeaseToken: leaseToken, OccurredAt: time.UnixMilli(acceptedAtMS),
	})
	if err != nil || !applied {
		t.Fatalf("ConfirmReaction() = %v, %v; want the read model changed", applied, err)
	}
	item, err := f.outbox.FindByID(context.Background(), outboxID)
	if err != nil {
		t.Fatalf("FindByID(): %v", err)
	}
	if item.State != OutboxConfirmed || item.ResultRemoteID != nil || item.LeaseToken != nil {
		t.Fatalf("confirmed reaction row = %+v, want confirmed with no result and no lease", item)
	}
	got := readStoredReaction(t, f.store, f.targetID, SelfReactorKey)
	want := storedReaction{
		MessageID: f.targetID, ReactorKey: SelfReactorKey, ReactorIsSelf: true, ReactorLabel: SelfReactorLabel,
		Emoji: "👍", State: "active", OccurredAtMS: acceptedAtMS, SourceSeqMS: acceptedAtMS,
		CreatedAtMS: confirmReactionTimeMS + 900, UpdatedAtMS: confirmReactionTimeMS + 900,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("own reaction row = %+v, want %+v", got, want)
	}

	// A remove through the same path tombstones that one row.
	removeID, removeToken := f.called("👍", bridge.ReactionRemove)
	if _, err := f.outbox.ConfirmReaction(context.Background(), ReactionConfirmation{
		OutboxID: removeID, LeaseToken: removeToken, ResultRemoteID: "remote-remove",
		OccurredAt: time.UnixMilli(acceptedAtMS + 1),
	}); err != nil {
		t.Fatalf("ConfirmReaction(remove): %v", err)
	}
	removed, err := f.outbox.FindByID(context.Background(), removeID)
	if err != nil || removed.State != OutboxConfirmed {
		t.Fatalf("removal row = %+v, %v; want confirmed", removed, err)
	}
	assertOutboxText(t, "result remote ID", removed.ResultRemoteID, "remote-remove")
	if row := readStoredReaction(t, f.store, f.targetID, SelfReactorKey); row.State != "removed" || row.Emoji != "👍" {
		t.Fatalf("own reaction row after remove = %+v, want the same row removed", row)
	}
	assertRowCount(t, f.store.db, "reactions", 1)
}

// A reaction that is not confirmed must leave no trace in the read model, and
// a confirmation that cannot be recorded must not confirm: the two writes are
// one transaction.
func TestConfirmReactionIsAllOrNothing(t *testing.T) {
	t.Run("a lost lease writes nothing", func(t *testing.T) {
		f := newConfirmReactionFixture(t)
		outboxID, _ := f.called("👍", bridge.ReactionAdd)
		_, err := f.outbox.ConfirmReaction(context.Background(), ReactionConfirmation{
			OutboxID: outboxID, LeaseToken: "someone-elses-lease", OccurredAt: f.clock.Now(),
		})
		if !errors.Is(err, ErrLeaseLost) {
			t.Fatalf("ConfirmReaction(wrong lease) = %v, want ErrLeaseLost", err)
		}
		if state := f.state(outboxID); state != OutboxDispatching {
			t.Fatalf("state = %q, want still dispatching", state)
		}
		assertRowCount(t, f.store.db, "reactions", 0)
	})

	t.Run("a reaction the transport was never called for cannot be confirmed", func(t *testing.T) {
		f := newConfirmReactionFixture(t)
		ctx := context.Background()
		item := outboxTestReactionItem("uncalled")
		if _, _, err := f.outbox.EnqueueReaction(ctx, item, OutboxReaction{
			TargetMessageID: f.targetID, Emoji: "👍", Action: "add",
		}); err != nil {
			t.Fatalf("EnqueueReaction(): %v", err)
		}
		leased := mustLeaseOne(t, f.outbox, LeaseRequest{
			Owner: "dispatcher", Now: f.clock.Now(), Duration: time.Minute, Limit: 1,
		})
		if _, err := f.outbox.ConfirmReaction(ctx, ReactionConfirmation{
			OutboxID: item.OutboxID, LeaseToken: mustLeaseToken(t, leased), OccurredAt: f.clock.Now(),
		}); !errors.Is(err, ErrLeaseLost) {
			t.Fatalf("ConfirmReaction(uncalled) = %v, want ErrLeaseLost", err)
		}
		assertRowCount(t, f.store.db, "reactions", 0)
	})

	t.Run("a failed read-model write rolls the confirmation back", func(t *testing.T) {
		f := newConfirmReactionFixture(t)
		outboxID, leaseToken := f.called("👍", bridge.ReactionAdd)
		// The stored action is corrupted after enqueue, so the outbox update
		// succeeds and the read-model write is the step that fails.
		mustExec(t, f.store.db, `PRAGMA ignore_check_constraints = ON`)
		mustExec(t, f.store.db, `UPDATE outbox_reactions SET action = 'explode' WHERE outbox_id = ?`, outboxID)
		mustExec(t, f.store.db, `PRAGMA ignore_check_constraints = OFF`)

		if _, err := f.outbox.ConfirmReaction(context.Background(), ReactionConfirmation{
			OutboxID: outboxID, LeaseToken: leaseToken, OccurredAt: f.clock.Now(),
		}); err == nil {
			t.Fatal("ConfirmReaction(corrupt action) succeeded")
		}
		if state := f.state(outboxID); state != OutboxDispatching {
			t.Fatalf("state = %q, want still dispatching after the rollback", state)
		}
		assertRowCount(t, f.store.db, "reactions", 0)
	})

	t.Run("only a reaction row can be confirmed this way", func(t *testing.T) {
		f := newConfirmReactionFixture(t)
		ctx := context.Background()
		item := outboxTestItem("text")
		if _, _, err := f.outbox.EnqueueOutgoingMessage(ctx, item, outboxTestOutgoingMessage(item, "text")); err != nil {
			t.Fatalf("EnqueueOutgoingMessage(): %v", err)
		}
		leased := mustLeaseOne(t, f.outbox, LeaseRequest{
			Owner: "dispatcher", Now: f.clock.Now(), Duration: time.Minute, Limit: 1,
		})
		leaseToken := mustLeaseToken(t, leased)
		if err := f.outbox.MarkTransportCalled(ctx, Attempt{
			OutboxID: item.OutboxID, LeaseToken: leaseToken, AttemptToken: "attempt", StartedAt: f.clock.Now(),
		}); err != nil {
			t.Fatalf("MarkTransportCalled(): %v", err)
		}
		if _, err := f.outbox.ConfirmReaction(ctx, ReactionConfirmation{
			OutboxID: item.OutboxID, LeaseToken: leaseToken, OccurredAt: f.clock.Now(),
		}); !errors.Is(err, ErrLeaseLost) {
			t.Fatalf("ConfirmReaction(text row) = %v, want ErrLeaseLost", err)
		}
		if state := f.state(item.OutboxID); state != OutboxDispatching {
			t.Fatalf("state = %q, want the text row untouched", state)
		}
	})

	t.Run("an occurrence time is required", func(t *testing.T) {
		f := newConfirmReactionFixture(t)
		outboxID, leaseToken := f.called("👍", bridge.ReactionAdd)
		if _, err := f.outbox.ConfirmReaction(context.Background(), ReactionConfirmation{
			OutboxID: outboxID, LeaseToken: leaseToken,
		}); err == nil {
			t.Fatal("ConfirmReaction(zero time) succeeded")
		}
		if state := f.state(outboxID); state != OutboxDispatching {
			t.Fatalf("state = %q, want still dispatching", state)
		}
	})
}

// A message can move to another conversation after its reaction was queued.
// The read-model row must follow the message, or its conversation foreign key
// would name a thread the message is no longer in.
func TestConfirmReactionRecordsUnderTheTargetsCurrentConversation(t *testing.T) {
	f := newConfirmReactionFixture(t)
	outboxID, leaseToken := f.called("👍", bridge.ReactionAdd)
	seedMessageConversation(t, f.store, "conversation-b", "account-a")
	mustExec(t, f.store.db, `UPDATE messages SET conversation_id = 'conversation-b' WHERE message_id = ?`, f.targetID)

	if _, err := f.outbox.ConfirmReaction(context.Background(), ReactionConfirmation{
		OutboxID: outboxID, LeaseToken: leaseToken, OccurredAt: f.clock.Now(),
	}); err != nil {
		t.Fatalf("ConfirmReaction(): %v", err)
	}
	var conversationID string
	if err := f.store.db.QueryRow(
		`SELECT conversation_id FROM reactions WHERE message_id = ? AND reactor_key = ?`,
		f.targetID, SelfReactorKey,
	).Scan(&conversationID); err != nil || conversationID != "conversation-b" {
		t.Fatalf("own reaction conversation = %q, %v; want the message's current one", conversationID, err)
	}
}

// reactionScriptStep is one event in the life of a message's reactions.
type reactionScriptStep struct {
	// Kind: 0 this account's reaction, delivered through the outbox; 1 this
	// account's reaction, not delivered; 2 a transport report of this
	// account's reaction (made on another device); 3 another person's.
	Kind   int
	Emoji  string
	Action bridge.ReactionAction
	// AtMS is the reaction's own time and SeqMS the frame time a transport
	// report carries. Both come from a few values, so ties are common.
	AtMS  int64
	SeqMS int64
	// Outcome picks how an undelivered reaction ended.
	Outcome int
}

type reactionScript []reactionScriptStep

func (reactionScript) Generate(r *rand.Rand, _ int) reflect.Value {
	emoji := []string{"👍", "❤️", "😂"}
	actions := []bridge.ReactionAction{bridge.ReactionAdd, bridge.ReactionRemove, bridge.ReactionSwitch}
	script := make(reactionScript, 1+r.Intn(14))
	for i := range script {
		script[i] = reactionScriptStep{
			Kind:    r.Intn(4),
			Emoji:   emoji[r.Intn(len(emoji))],
			Action:  actions[r.Intn(len(actions))],
			AtMS:    confirmReactionTimeMS + int64(r.Intn(6)),
			SeqMS:   confirmReactionTimeMS + int64(r.Intn(6)),
			Outcome: r.Intn(3),
		}
	}
	return reflect.ValueOf(script)
}

// storedReactionsFor returns every reaction row of a message, removed ones
// included, without the storage timestamps that record when a row was written.
func storedReactionsFor(t *testing.T, store *Store, messageID string) []storedReaction {
	t.Helper()
	rows, err := store.db.Query(`SELECT reactor_key FROM reactions WHERE message_id = ? ORDER BY reactor_key`, messageID)
	if err != nil {
		t.Fatalf("list reactions: %v", err)
	}
	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			t.Fatalf("scan reaction key: %v", err)
		}
		keys = append(keys, key)
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("close reactions: %v", err)
	}
	stored := make([]storedReaction, 0, len(keys))
	for _, key := range keys {
		stored = append(stored, readStoredReaction(t, store, messageID, key))
	}
	return stored
}

// TestConfirmedOwnReactionIsStoredAsItsEchoWouldBe is the differential check
// between the two writers of an own reaction. For any history of a message's
// reactions, a store where this account's delivered reactions were confirmed
// by the outbox holds exactly the rows of a store where the transport
// reported each of them instead, and a reaction that was not delivered
// (rejected, left uncertain, or put back for retry) leaves no row at all.
func TestConfirmedOwnReactionIsStoredAsItsEchoWouldBe(t *testing.T) {
	// Two stores serve every case; each case gets a message of its own in
	// both, because opening a store costs far more than a case does.
	viaOutbox := newConfirmReactionFixture(t)
	viaEcho := newConfirmReactionFixture(t)
	ctx := context.Background()
	cases := 0
	property := func(script reactionScript) bool {
		cases++
		messageID := fmt.Sprintf("target-case-%d", cases)
		viaOutbox.retarget(messageID)
		viaEcho.retarget(messageID)
		for index, step := range script {
			nowMS := confirmReactionTimeMS + 100 + int64(index)
			viaOutbox.clock.Set(nowMS)
			viaEcho.clock.Set(nowMS)
			self := ReactionApply{
				AccountID: "account-a", ConversationID: "conversation-a", MessageID: messageID,
				ReactorKey: SelfReactorKey, ReactorIsSelf: true, ReactorLabel: SelfReactorLabel,
				Emoji: step.Emoji, Action: step.Action, OccurredAtMS: step.AtMS, SourceSeqMS: step.AtMS,
			}
			switch step.Kind {
			case 0:
				outboxID, leaseToken := viaOutbox.called(step.Emoji, step.Action)
				if _, err := viaOutbox.outbox.ConfirmReaction(ctx, ReactionConfirmation{
					OutboxID: outboxID, LeaseToken: leaseToken, OccurredAt: time.UnixMilli(step.AtMS),
				}); err != nil {
					t.Errorf("step %d ConfirmReaction(): %v", index, err)
					return false
				}
				if state := viaOutbox.state(outboxID); state != OutboxConfirmed {
					t.Errorf("step %d state = %q, want confirmed", index, state)
					return false
				}
				if _, err := viaEcho.reactions.ApplyReaction(ctx, self); err != nil {
					t.Errorf("step %d ApplyReaction(echo of own): %v", index, err)
					return false
				}
			case 1:
				outboxID, leaseToken := viaOutbox.called(step.Emoji, step.Action)
				var err error
				want := OutboxRejected
				switch step.Outcome {
				case 0:
					err = viaOutbox.outbox.Reject(ctx, outboxID, leaseToken, "unsupported", "send_reaction", "scripted")
				case 1:
					want = OutboxUncertain
					err = viaOutbox.outbox.MarkUncertain(ctx, outboxID, leaseToken, "transient", "send_reaction", "scripted")
				default:
					want = OutboxNotDispatched
					err = viaOutbox.outbox.MarkCalledNotDispatched(
						ctx, outboxID, leaseToken, "transient", "send_reaction", "scripted",
						time.UnixMilli(nowMS).Add(24*time.Hour),
					)
				}
				if err != nil {
					t.Errorf("step %d undelivered outcome %d: %v", index, step.Outcome, err)
					return false
				}
				if state := viaOutbox.state(outboxID); state != want {
					t.Errorf("step %d state = %q, want %q", index, state, want)
					return false
				}
			default:
				report := self
				report.SourceSeqMS = step.SeqMS
				if step.Kind == 3 {
					report.ReactorKey, report.ReactorIsSelf, report.ReactorLabel = "identity-other", false, ""
					report.ReactorIdentityID = pointer("identity-other")
				}
				for _, fixture := range []*confirmReactionFixture{viaOutbox, viaEcho} {
					if _, err := fixture.reactions.ApplyReaction(ctx, report); err != nil {
						t.Errorf("step %d ApplyReaction(report): %v", index, err)
						return false
					}
				}
			}
		}
		got := storedReactionsFor(t, viaOutbox.store, messageID)
		want := storedReactionsFor(t, viaEcho.store, messageID)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("script %+v\nvia the outbox: %+v\nvia echoes:     %+v", script, got, want)
			return false
		}
		return true
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 150, Rand: rand.New(rand.NewSource(20261009))}); err != nil {
		t.Fatal(err)
	}
}
