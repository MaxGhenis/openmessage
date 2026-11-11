package messaging

import (
	"context"
	"fmt"
	"math/rand"
	"reflect"
	"testing"
	"testing/quick"
	"time"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

// reactionProjectionHarness runs reactions through the real service and
// dispatcher against a scripted transport, and reads back what a reader of
// the store sees.
type reactionProjectionHarness struct {
	t         *testing.T
	clock     *manualClock
	store     *sqlite.Store
	service   *MessageService
	sender    *scriptedReactionSender
	reactions *sqlite.ReactionRepository
	targets   int
	keys      int
}

const reactionProjectionOther = "identity-reaction-other"

func newReactionProjectionHarness(t *testing.T) *reactionProjectionHarness {
	t.Helper()
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	seedDispatchIdentity(t, store, "identity-reaction-author", "author@example.test", clock.Now())
	if err := store.UpsertIdentity(sqlite.Identity{
		IdentityID: reactionProjectionOther, AccountID: "account-1",
		Kind: sqlite.IdentityKind("test_address"), CanonicalValue: "other@example.test",
		RawValue: "other@example.test", MetadataJSON: `{}`,
		CreatedAtMS: clock.Now().UnixMilli(), UpdatedAtMS: clock.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("UpsertIdentity(other reactor): %v", err)
	}
	sender := &scriptedReactionSender{}
	registry := newScriptedRegistry("reaction-projection", &scriptedTextSender{})
	registry.setReactionSender(sender)
	registry.setAvailable(true)
	reactions, err := sqlite.NewReactionRepository(store, clock.Now)
	if err != nil {
		t.Fatalf("NewReactionRepository(): %v", err)
	}
	return &reactionProjectionHarness{
		t: t, clock: clock, store: store, sender: sender, reactions: reactions,
		service: newMessagingTestService(t, store, registry, clock),
	}
}

// target stores a fresh incoming message to react to.
func (h *reactionProjectionHarness) target() string {
	h.t.Helper()
	h.targets++
	author := "identity-reaction-author"
	message := mustProjectDispatchMessage(h.t, h.store, h.clock, sqlite.Message{
		MessageID:        fmt.Sprintf("message-reaction-projection-%d", h.targets),
		ConversationID:   "conversation-1",
		AccountID:        "account-1",
		RemoteMessageID:  fmt.Sprintf("remote-reaction-projection-%d", h.targets),
		SenderIdentityID: &author,
		Direction:        sqlite.MessageDirectionIncoming,
		Body:             "react to this",
		State:            sqlite.MessageStateActive,
		OccurredAtMS:     h.clock.Now().Add(-time.Minute).UnixMilli(),
	})
	return message.MessageID
}

// react submits a reaction and runs the dispatcher once with the transport
// scripted to answer as given.
func (h *reactionProjectionHarness) react(
	targetID, emoji string,
	action bridge.ReactionAction,
	answer sendStep,
) Submission {
	h.t.Helper()
	h.keys++
	h.sender.mu.Lock()
	h.sender.steps = append(h.sender.steps, answer)
	h.sender.mu.Unlock()
	submission := mustSendDispatchReaction(h.t, h.service, SendReactionCommand{
		CommonCommand:   testCommonCommand(fmt.Sprintf("reaction-projection-%d", h.keys)),
		TargetMessageID: targetID,
		Emoji:           emoji,
		Action:          action,
	})
	if processed, err := h.service.DispatchDue(context.Background(), 4); err != nil || processed != 1 {
		h.t.Fatalf("DispatchDue() = %d, %v; want exactly the reaction just queued", processed, err)
	}
	return submission
}

// visible returns the active reactions on a message as a reader gets them:
// reactor ("me" for this account, else the reactor's address) to emoji.
func (h *reactionProjectionHarness) visible(targetID string) map[string]string {
	h.t.Helper()
	rows, err := h.reactions.ReactionsForMessages(context.Background(), []string{targetID})
	if err != nil {
		h.t.Fatalf("ReactionsForMessages(): %v", err)
	}
	visible := map[string]string{}
	for _, row := range rows[targetID] {
		reactor := row.ReactorCanonical
		if row.ReactorIsSelf {
			reactor = sqlite.SelfReactorLabel
		}
		if _, duplicate := visible[reactor]; duplicate {
			h.t.Fatalf("reactor %q has more than one active reaction on %q: %+v", reactor, targetID, rows[targetID])
		}
		visible[reactor] = row.Emoji
	}
	return visible
}

func TestDeliveredReactionShowsAsThisAccountsOwn(t *testing.T) {
	h := newReactionProjectionHarness(t)
	target := h.target()

	// The transport says when it accepted the reaction; that time orders it.
	acceptedAt := h.clock.Now().Add(-3 * time.Second)
	added := h.react(target, "👍", bridge.ReactionAdd, sendStep{result: bridge.SendResult{AcceptedAt: acceptedAt}})
	if delivery := mustDelivery(t, h.service, added.OutboxID); delivery.State != OutboxConfirmed {
		t.Fatalf("delivery = %+v, want confirmed", delivery)
	}
	if got, want := h.visible(target), (map[string]string{"me": "👍"}); !reflect.DeepEqual(got, want) {
		t.Fatalf("visible reactions = %v, want %v", got, want)
	}
	rows, err := h.reactions.ReactionsForMessages(context.Background(), []string{target})
	if err != nil || rows[target][0].OccurredAtMS != acceptedAt.UnixMilli() {
		t.Fatalf("own reaction time = %+v, %v; want the transport's accepted time %d", rows[target], err, acceptedAt.UnixMilli())
	}

	// With no accepted time from the transport, the dispatcher's clock is it.
	h.clock.Advance(time.Second)
	h.react(target, "❤️", bridge.ReactionSwitch, sendStep{})
	if got, want := h.visible(target), (map[string]string{"me": "❤️"}); !reflect.DeepEqual(got, want) {
		t.Fatalf("visible reactions after switch = %v, want %v", got, want)
	}
	rows, err = h.reactions.ReactionsForMessages(context.Background(), []string{target})
	if err != nil || rows[target][0].OccurredAtMS != h.clock.Now().UnixMilli() {
		t.Fatalf("switched reaction time = %+v, %v; want the dispatcher's clock", rows[target], err)
	}

	h.clock.Advance(time.Second)
	h.react(target, "❤️", bridge.ReactionRemove, sendStep{})
	if got := h.visible(target); len(got) != 0 {
		t.Fatalf("visible reactions after remove = %v, want none", got)
	}
}

func TestUndeliveredReactionChangesNothingAReaderSees(t *testing.T) {
	for _, undelivered := range []struct {
		name   string
		answer sendStep
		want   OutboxState
	}{
		{
			name: "the platform is disconnected",
			answer: sendStep{err: bridge.OpError{
				Class: bridge.FailureTransient, Operation: "send_reaction", Dispatch: bridge.DispatchNotCalled,
			}},
			want: OutboxNotDispatched,
		},
		{
			name: "the platform refuses it",
			answer: sendStep{err: bridge.OpError{
				Class: bridge.FailureUnsupported, Operation: "send_reaction", Dispatch: bridge.DispatchNotCalled,
			}},
			want: OutboxRejected,
		},
		{
			name: "the transport call ends without an answer",
			answer: sendStep{err: bridge.OpError{
				Class: bridge.FailureTransient, Operation: "send_reaction", Dispatch: bridge.DispatchUncertain,
			}},
			want: OutboxUncertain,
		},
	} {
		t.Run(undelivered.name, func(t *testing.T) {
			h := newReactionProjectionHarness(t)
			target := h.target()
			// An earlier delivered reaction must survive the failed one.
			h.react(target, "👍", bridge.ReactionAdd, sendStep{})
			h.clock.Advance(time.Second)

			failed := h.react(target, "😂", bridge.ReactionSwitch, undelivered.answer)
			if delivery := mustDelivery(t, h.service, failed.OutboxID); delivery.State != undelivered.want {
				t.Fatalf("delivery = %+v, want %q", delivery, undelivered.want)
			}
			if got, want := h.visible(target), (map[string]string{"me": "👍"}); !reflect.DeepEqual(got, want) {
				t.Fatalf("visible reactions = %v, want the earlier reaction only %v", got, want)
			}
		})
	}

	t.Run("it is canceled before it is sent", func(t *testing.T) {
		h := newReactionProjectionHarness(t)
		target := h.target()
		submission := mustSendDispatchReaction(t, h.service, SendReactionCommand{
			CommonCommand:   testCommonCommand("reaction-projection-canceled"),
			TargetMessageID: target,
			Emoji:           "👍",
		})
		if _, err := h.service.Cancel(context.Background(), submission.OutboxID); err != nil {
			t.Fatalf("Cancel(): %v", err)
		}
		if processed, err := h.service.DispatchDue(context.Background(), 4); err != nil || processed != 0 {
			t.Fatalf("DispatchDue() = %d, %v; want nothing to dispatch", processed, err)
		}
		if got := h.visible(target); len(got) != 0 || h.sender.requestCount() != 0 {
			t.Fatalf("visible reactions = %v after %d transport calls, want none", got, h.sender.requestCount())
		}
	})
}

// reactionHistoryStep is one event in a message's reaction history.
type reactionHistoryStep struct {
	// Kind: 0 this account reacts and the transport accepts; 1 this account
	// reacts and the transport does not deliver; 2 the transport reports this
	// account's reaction from another device; 3 it reports another person's.
	Kind    int
	Emoji   string
	Action  bridge.ReactionAction
	Outcome int
	// Order is the reaction's place in time among the history's events. Every
	// step has a different one, and it is unrelated to the step's position,
	// so events are applied out of time order.
	Order int
}

type reactionHistory []reactionHistoryStep

func (reactionHistory) Generate(r *rand.Rand, _ int) reflect.Value {
	emoji := []string{"👍", "❤️", "😂"}
	actions := []bridge.ReactionAction{bridge.ReactionAdd, bridge.ReactionRemove, bridge.ReactionSwitch}
	history := make(reactionHistory, 1+r.Intn(10))
	order := r.Perm(len(history))
	for i := range history {
		history[i] = reactionHistoryStep{
			Kind:    r.Intn(4),
			Emoji:   emoji[r.Intn(len(emoji))],
			Action:  actions[r.Intn(len(actions))],
			Outcome: r.Intn(3),
			Order:   order[i],
		}
	}
	return reflect.ValueOf(history)
}

// TestReactionsAReaderSeesAreTheLatestDeliveredOnes states what the read
// model means once reactions go through the outbox. For any history of a
// message's reactions, applied in any order:
//
//   - each reactor shows at most one reaction;
//   - it is the latest, by the reaction's own time, of that reactor's
//     reactions that were delivered here (accepted by the transport, or
//     reported by it), and a latest "remove" shows nothing;
//   - a reaction the transport did not deliver changes nothing;
//   - every reaction this account submitted reaches the transport exactly
//     once and ends in the outbox state its answer calls for.
func TestReactionsAReaderSeesAreTheLatestDeliveredOnes(t *testing.T) {
	h := newReactionProjectionHarness(t)
	ctx := context.Background()
	undelivered := []struct {
		answer sendStep
		want   OutboxState
	}{
		{sendStep{err: bridge.OpError{Class: bridge.FailureUnsupported, Operation: "send_reaction", Dispatch: bridge.DispatchNotCalled}}, OutboxRejected},
		{sendStep{err: bridge.OpError{Class: bridge.FailureTransient, Operation: "send_reaction", Dispatch: bridge.DispatchUncertain}}, OutboxUncertain},
		{sendStep{err: bridge.OpError{Class: bridge.FailureTransient, Operation: "send_reaction", Dispatch: bridge.DispatchNotCalled}}, OutboxNotDispatched},
	}

	property := func(history reactionHistory) bool {
		target := h.target()
		base := h.clock.Now()
		type latest struct {
			order int
			emoji string
		}
		model := map[string]latest{}
		deliver := func(reactor string, step reactionHistoryStep) {
			if seen, ok := model[reactor]; ok && seen.order > step.Order {
				return
			}
			emoji := step.Emoji
			if step.Action == bridge.ReactionRemove {
				emoji = ""
			}
			model[reactor] = latest{order: step.Order, emoji: emoji}
		}
		calls := h.sender.requestCount()
		submitted := 0

		for index, step := range history {
			h.clock.Advance(time.Millisecond)
			at := base.Add(time.Duration(step.Order+1) * time.Hour)
			switch step.Kind {
			case 0:
				submission := h.react(target, step.Emoji, step.Action, sendStep{result: bridge.SendResult{AcceptedAt: at}})
				submitted++
				if state := mustDelivery(t, h.service, submission.OutboxID).State; state != OutboxConfirmed {
					t.Errorf("step %d: delivered reaction state = %q, want confirmed", index, state)
					return false
				}
				deliver("me", step)
			case 1:
				outcome := undelivered[step.Outcome]
				submission := h.react(target, step.Emoji, step.Action, outcome.answer)
				submitted++
				if state := mustDelivery(t, h.service, submission.OutboxID).State; state != outcome.want {
					t.Errorf("step %d: undelivered reaction state = %q, want %q", index, state, outcome.want)
					return false
				}
				if outcome.want == OutboxNotDispatched {
					// The dispatcher would retry it; take it out of play so the
					// transport sees each reaction of this history once.
					if _, err := h.service.Cancel(ctx, submission.OutboxID); err != nil {
						t.Errorf("step %d: Cancel(): %v", index, err)
						return false
					}
				}
			default:
				report := sqlite.ReactionApply{
					AccountID: "account-1", ConversationID: "conversation-1", MessageID: target,
					ReactorKey: sqlite.SelfReactorKey, ReactorIsSelf: true, ReactorLabel: sqlite.SelfReactorLabel,
					Emoji: step.Emoji, Action: step.Action,
					OccurredAtMS: at.UnixMilli(), SourceSeqMS: h.clock.Now().UnixMilli(),
				}
				reactor := "me"
				if step.Kind == 3 {
					other := reactionProjectionOther
					report.ReactorKey, report.ReactorIdentityID = other, &other
					report.ReactorIsSelf, report.ReactorLabel = false, ""
					reactor = "other@example.test"
				}
				if _, err := h.reactions.ApplyReaction(ctx, report); err != nil {
					t.Errorf("step %d: ApplyReaction(): %v", index, err)
					return false
				}
				deliver(reactor, step)
			}
		}

		want := map[string]string{}
		for reactor, seen := range model {
			if seen.emoji != "" {
				want[reactor] = seen.emoji
			}
		}
		if got := h.visible(target); !reflect.DeepEqual(got, want) {
			t.Errorf("history %+v\nvisible reactions = %v, want %v", history, got, want)
			return false
		}
		if got := h.sender.requestCount() - calls; got != submitted {
			t.Errorf("history %+v\ntransport calls = %d, want one per submitted reaction (%d)", history, got, submitted)
			return false
		}
		return true
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 150, Rand: rand.New(rand.NewSource(20261009))}); err != nil {
		t.Fatal(err)
	}
}
