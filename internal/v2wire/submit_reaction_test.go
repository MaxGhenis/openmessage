package v2wire

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
	"github.com/maxghenis/openmessage/internal/messaging"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

// reactionSubmitFixture is a v2 store with three conversations on a reacting
// account and one on an account that cannot react, each holding one message.
type reactionSubmitFixture struct {
	t      *testing.T
	store  *sqlite.Store
	deps   NativeDeps
	outbox *sqlite.OutboxRepository
	keys   int
	// messages maps a message ID to the conversation it is in.
	messages map[string]sqlite.Conversation
}

func newReactionSubmitFixture(t *testing.T) *reactionSubmitFixture {
	t.Helper()
	store := openV2TestStore(t)
	registry := submitTestRegistry{caps: map[string]bridge.CapabilitySet{
		"account-signal": {Reactions: true},
		"account-google": {Reactions: true},
		"account-import": {},
	}}
	outbox, err := sqlite.NewOutboxRepository(store, time.Now)
	if err != nil {
		t.Fatalf("NewOutboxRepository(): %v", err)
	}
	fixture := &reactionSubmitFixture{
		t: t, store: store, outbox: outbox, messages: map[string]sqlite.Conversation{},
		deps: NativeDeps{V2: store, Service: newSubmitTestService(t, store, registry, nil), Registry: registry},
	}
	for _, seed := range []struct{ account, conversation, remote, message string }{
		{"account-signal", "v2-signal-direct", "signal:+15550001111", "message-signal-direct"},
		{"account-signal", "v2-signal-group", "signal-group:Z3JvdXA=", "message-signal-group"},
		{"account-google", "v2-google-thread", "1234", "message-google"},
		{"account-import", "v2-import-thread", "imessage:chat1", "message-import"},
	} {
		nowMS := time.Now().UnixMilli()
		if err := store.UpsertAccount(sqlite.Account{
			AccountID: seed.account, BridgeKey: "reaction-test", DisplayName: seed.account,
			Mode: sqlite.AccountModeLive, Enabled: true, ConfigJSON: "{}",
			CreatedAtMS: nowMS, UpdatedAtMS: nowMS,
		}); err != nil {
			t.Fatalf("UpsertAccount(%q): %v", seed.account, err)
		}
		conversation := sqlite.Conversation{
			ConversationID: seed.conversation, AccountID: seed.account, RemoteConversationID: seed.remote,
			Kind: sqlite.ConversationKindDirect, Title: seed.conversation,
			NotificationMode: sqlite.NotificationModeAll, MetadataJSON: "{}",
			CreatedAtMS: nowMS, UpdatedAtMS: nowMS,
		}
		if err := store.UpsertConversation(conversation); err != nil {
			t.Fatalf("UpsertConversation(%q): %v", seed.conversation, err)
		}
		projectV2TestMessage(t, store, sqlite.Message{
			MessageID: seed.message, ConversationID: seed.conversation, AccountID: seed.account,
			RemoteMessageID: "remote-" + seed.message, Direction: sqlite.MessageDirectionIncoming,
			Body: "react to this", State: sqlite.MessageStateActive, OccurredAtMS: 1_900_000_000_000,
		})
		fixture.messages[seed.message] = conversation
	}
	return fixture
}

func (f *reactionSubmitFixture) submit(input ReactionInput) (messaging.Submission, error) {
	if input.IdempotencyKey == "" {
		f.keys++
		input.IdempotencyKey = fmt.Sprintf("reaction-submit-%d", f.keys)
	}
	if input.Emoji == "" {
		input.Emoji = "👍"
	}
	return SubmitReactionV2(context.Background(), f.deps, input)
}

func (f *reactionSubmitFixture) queuedReactions() int {
	f.t.Helper()
	rows, err := f.outbox.ListPending(context.Background(), sqlite.ListPendingParams{Limit: 10_000})
	if err != nil {
		f.t.Fatalf("ListPending(): %v", err)
	}
	return len(rows)
}

func TestSubmitReactionV2TakesAccountAndConversationFromTheTarget(t *testing.T) {
	f := newReactionSubmitFixture(t)
	submission, err := f.submit(ReactionInput{
		MessageID: " message-signal-group ", Emoji: " ❤️ ", Action: "Switch", IdempotencyKey: "reaction-key",
	})
	if err != nil {
		t.Fatalf("SubmitReactionV2(): %v", err)
	}
	if submission.State != messaging.OutboxQueued || submission.LocalMessageID != "" || submission.Deduplicated {
		t.Fatalf("submission = %+v, want a new queued intent with no local message", submission)
	}
	item, err := f.outbox.FindByID(context.Background(), submission.OutboxID)
	if err != nil {
		t.Fatalf("FindByID(): %v", err)
	}
	reaction, err := f.outbox.GetOutboxReaction(context.Background(), submission.OutboxID)
	if err != nil {
		t.Fatalf("GetOutboxReaction(): %v", err)
	}
	if item.Kind != sqlite.OutboxKindReaction || item.AccountID != "account-signal" ||
		item.ConversationID != "v2-signal-group" || reaction.TargetMessageID != "message-signal-group" ||
		reaction.Emoji != "❤️" || reaction.Action != string(bridge.ReactionSwitch) {
		t.Fatalf("queued reaction = %+v / %+v", item, reaction)
	}

	replay, err := f.submit(ReactionInput{
		ConversationID: "v2-signal-group", MessageID: "message-signal-group",
		Emoji: "❤️", Action: "switch", IdempotencyKey: "reaction-key",
	})
	if err != nil || !replay.Deduplicated || replay.OutboxID != submission.OutboxID {
		t.Fatalf("replay with the same key = %+v, %v; want the first intent", replay, err)
	}
	if _, err := f.submit(ReactionInput{
		MessageID: "message-signal-group", Emoji: "👍", IdempotencyKey: "reaction-key",
	}); !errors.Is(err, messaging.ErrIdempotencyConflict) {
		t.Fatalf("same key, other reaction = %v, want ErrIdempotencyConflict", err)
	}
	if queued := f.queuedReactions(); queued != 1 {
		t.Fatalf("queued reactions = %d, want 1", queued)
	}
}

func TestSubmitReactionV2RefusesWhatItCannotPlace(t *testing.T) {
	for _, refused := range []struct {
		name  string
		input ReactionInput
		want  error
	}{
		{"no message", ReactionInput{MessageID: "  "}, messaging.ErrInvalidCommand},
		{"an action that is not a reaction action", ReactionInput{MessageID: "message-google", Action: "toggle"}, messaging.ErrInvalidCommand},
		{"a message this store does not hold", ReactionInput{MessageID: "signal:1700000001000"}, ErrReactionTargetUnavailable},
		{"a message in another conversation", ReactionInput{MessageID: "message-google", ConversationID: "v2-signal-direct"}, ErrReactionTargetUnavailable},
		{"another conversation's remote ID", ReactionInput{MessageID: "message-signal-group", ConversationID: "signal:+15550001111"}, ErrReactionTargetUnavailable},
		{"an account with no reaction sender", ReactionInput{MessageID: "message-import"}, ErrPlatformNotSendable},
	} {
		t.Run(refused.name, func(t *testing.T) {
			f := newReactionSubmitFixture(t)
			if _, err := f.submit(refused.input); !errors.Is(err, refused.want) {
				t.Fatalf("SubmitReactionV2() = %v, want %v", err, refused.want)
			}
			if queued := f.queuedReactions(); queued != 0 {
				t.Fatalf("a refused reaction left %d outbox rows", queued)
			}
		})
	}
}

// reactionAddress is a reaction as a caller might address it: one of the
// fixture's messages, and a conversation key that may or may not be its own.
type reactionAddress struct {
	MessageID       string
	ConversationKey string
}

func (reactionAddress) Generate(r *rand.Rand, _ int) reflect.Value {
	messages := []string{"message-signal-direct", "message-signal-group", "message-google"}
	keys := []string{
		"", "  ",
		"v2-signal-direct", " v2-signal-direct\t", "v2-signal-group", "v2-google-thread", "v2-import-thread",
		"signal:+15550001111", "signal: +15550001111 ", " signal:+15550001111", "signal:+15550002222",
		"signal-group:Z3JvdXA=", "signal-group: Z3JvdXA= ", "signal-group:b3RoZXI=",
		"1234", " 1234 ", "12345", "imessage:chat1", "V2-SIGNAL-DIRECT", "remote-v2-signal-direct",
	}
	return reflect.ValueOf(reactionAddress{
		MessageID:       messages[r.Intn(len(messages))],
		ConversationKey: keys[r.Intn(len(keys))],
	})
}

// TestSubmitReactionV2QueuesOnlyInTheTargetsConversation is the routing
// invariant: however a reaction is addressed, it is queued if and only if the
// conversation key is empty or names the target's own conversation (by v2 ID
// or by remote ID, whitespace aside), and a queued reaction always sits under
// the target's account and conversation. A refused one queues nothing.
func TestSubmitReactionV2QueuesOnlyInTheTargetsConversation(t *testing.T) {
	f := newReactionSubmitFixture(t)
	// The acceptable keys, written out per conversation rather than derived
	// with the code under test.
	names := map[string]map[string]bool{
		"v2-signal-direct": {"": true, "  ": true, "v2-signal-direct": true, " v2-signal-direct\t": true,
			"signal:+15550001111": true, "signal: +15550001111 ": true, " signal:+15550001111": true},
		"v2-signal-group": {"": true, "  ": true, "v2-signal-group": true,
			"signal-group:Z3JvdXA=": true, "signal-group: Z3JvdXA= ": true},
		"v2-google-thread": {"": true, "  ": true, "v2-google-thread": true, "1234": true, " 1234 ": true},
	}
	property := func(address reactionAddress) bool {
		conversation := f.messages[address.MessageID]
		before := f.queuedReactions()
		submission, err := f.submit(ReactionInput{MessageID: address.MessageID, ConversationID: address.ConversationKey})
		if !names[conversation.ConversationID][address.ConversationKey] {
			if !errors.Is(err, ErrReactionTargetUnavailable) || f.queuedReactions() != before {
				t.Errorf("%+v: err = %v with %d new rows, want refused with none", address, err, f.queuedReactions()-before)
				return false
			}
			return true
		}
		if err != nil {
			t.Errorf("%+v: SubmitReactionV2() = %v, want queued", address, err)
			return false
		}
		item, err := f.outbox.FindByID(context.Background(), submission.OutboxID)
		if err != nil || item.AccountID != conversation.AccountID || item.ConversationID != conversation.ConversationID {
			t.Errorf("%+v: queued under %q/%q (%v), want %q/%q", address,
				item.AccountID, item.ConversationID, err, conversation.AccountID, conversation.ConversationID)
			return false
		}
		return f.queuedReactions() == before+1
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 300, Rand: rand.New(rand.NewSource(20261009))}); err != nil {
		t.Fatal(err)
	}
}
