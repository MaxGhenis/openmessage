package google

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	gmproto "go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/messaging"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

// reactionDispatchRegistry leases one adapter's reaction sender to the
// dispatcher, the way the account registry does for a connected account.
type reactionDispatchRegistry struct {
	accountID string
	sender    bridge.ReactionSender
}

func (r reactionDispatchRegistry) Snapshot(string) (bridge.Snapshot, bool) {
	return bridge.Snapshot{}, false
}

func (r reactionDispatchRegistry) Acquire(
	ctx context.Context,
	accountID string,
	capability bridge.Capability,
) (*bridge.DispatchLease, error) {
	if accountID != r.accountID || capability != bridge.CapabilityReactions {
		return nil, bridge.ErrCapabilityUnavailable
	}
	return &bridge.DispatchLease{
		AccountID: accountID, Platform: bridge.PlatformGoogle, Generation: 1, Ctx: ctx, Reaction: r.sender,
	}, nil
}

func (r reactionDispatchRegistry) Capabilities(accountID string) bridge.CapabilitySet {
	return bridge.CapabilitySet{Reactions: accountID == r.accountID}
}

// reactionDispatchClock is a wall clock the test moves by hand.
type reactionDispatchClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *reactionDispatchClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *reactionDispatchClock) Advance(by time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(by)
}

func (c *reactionDispatchClock) NewTimer(d time.Duration) messaging.Timer {
	return messaging.SystemClock{}.NewTimer(d)
}

// A reaction the phone refuses (SendReactionResponse success: false) must end
// rejected, the state /api/react answers 502 for, and must not be sent again.
// This runs the real Google adapter's classification through the real
// dispatcher and outbox; only libgm's transport is faked. Before, the refusal
// was classified transient and not dispatched, so the row went back to
// not_dispatched, /api/react answered 202 "keeps trying", and the dispatcher
// sent it again every 5 s with no cap.
func TestGoogleRefusedReactionIsRejectedAndNotRetried(t *testing.T) {
	ctx := context.Background()
	clock := &reactionDispatchClock{now: time.UnixMilli(1_900_000_000_000)}
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "v2.sqlite3"))
	if err != nil {
		t.Fatalf("sqlite.Open(): %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const accountID = "google-primary"
	nowMS := clock.Now().UnixMilli()
	if err := store.UpsertAccount(sqlite.Account{
		AccountID: accountID, BridgeKey: "google", DisplayName: "Google Messages",
		Mode: sqlite.AccountModeLive, Enabled: true, ConfigJSON: "{}",
		CreatedAtMS: nowMS, UpdatedAtMS: nowMS,
	}); err != nil {
		t.Fatalf("UpsertAccount(): %v", err)
	}
	if err := store.UpsertConversation(sqlite.Conversation{
		ConversationID: "v2-google-thread", AccountID: accountID, RemoteConversationID: "conversation-id",
		Kind: sqlite.ConversationKindDirect, Title: "thread",
		NotificationMode: sqlite.NotificationModeAll, MetadataJSON: "{}",
		CreatedAtMS: nowMS, UpdatedAtMS: nowMS,
	}); err != nil {
		t.Fatalf("UpsertConversation(): %v", err)
	}
	messages, err := sqlite.NewMessageRepository(store, clock.Now)
	if err != nil {
		t.Fatalf("NewMessageRepository(): %v", err)
	}
	if _, err := messages.AppendInbox(ctx, sqlite.InboxRecord{
		InboxID: "inbox-target", AccountID: accountID, Generation: 1,
		DedupeKey: "inbox-target", Codec: "test", CodecVersion: 1, Payload: []byte("test"),
	}); err != nil {
		t.Fatalf("AppendInbox(): %v", err)
	}
	if err := messages.ProjectMessage(ctx, sqlite.MessageProjection{
		InboxID: "inbox-target",
		Message: sqlite.Message{
			MessageID: "v2-target", ConversationID: "v2-google-thread", AccountID: accountID,
			RemoteMessageID: "target-id", Direction: sqlite.MessageDirectionIncoming,
			Body: "react to this", State: sqlite.MessageStateActive, OccurredAtMS: nowMS - 60_000,
		},
	}); err != nil {
		t.Fatalf("ProjectMessage(): %v", err)
	}

	fake := &fakeReactionSendClient{
		conversationResult: &gmproto.Conversation{},
		sendResult:         &gmproto.SendReactionResponse{Success: false},
	}
	adapter := newReactionSendTestAdapter(t, fake)
	service, err := messaging.NewMessageService(
		store,
		reactionDispatchRegistry{accountID: accountID, sender: adapter},
		nil,
		clock,
		messaging.CryptoIDSource{},
	)
	if err != nil {
		t.Fatalf("NewMessageService(): %v", err)
	}

	submission, err := service.SendReaction(ctx, messaging.SendReactionCommand{
		CommonCommand: messaging.CommonCommand{
			AccountID: accountID, ConversationID: "v2-google-thread", IdempotencyKey: "refused-reaction",
		},
		TargetMessageID: "v2-target",
		Emoji:           "👍",
		Action:          bridge.ReactionAdd,
	})
	if err != nil {
		t.Fatalf("SendReaction(): %v", err)
	}
	if processed, err := service.DispatchDue(ctx, 4); err != nil || processed != 1 {
		t.Fatalf("DispatchDue() = %d, %v; want 1, nil", processed, err)
	}
	if fake.sendCalls != 1 || fake.sent == nil {
		t.Fatalf("libgm SendReaction calls = %d, want 1", fake.sendCalls)
	}
	delivery, err := service.Get(ctx, submission.OutboxID)
	if err != nil {
		t.Fatalf("Get(): %v", err)
	}
	if delivery.State != messaging.OutboxRejected ||
		delivery.ErrorClass != string(bridge.FailureUnsupported) || delivery.ErrorCode != "send_reaction" {
		t.Fatalf("delivery = %+v, want rejected unsupported send_reaction", delivery)
	}

	// Well past the retry delay, nothing is due and nothing is sent again.
	clock.Advance(time.Hour)
	if processed, err := service.DispatchDue(ctx, 4); err != nil || processed != 0 {
		t.Fatalf("DispatchDue(an hour later) = %d, %v; want 0, nil", processed, err)
	}
	if fake.sendCalls != 1 {
		t.Fatalf("libgm SendReaction calls = %d after an hour, want the refusal never resent", fake.sendCalls)
	}

	// A refused reaction is not shown as this account's own.
	reactions, err := sqlite.NewReactionRepository(store, clock.Now)
	if err != nil {
		t.Fatalf("NewReactionRepository(): %v", err)
	}
	shown, err := reactions.ReactionsForMessages(ctx, []string{"v2-target"})
	if err != nil {
		t.Fatalf("ReactionsForMessages(): %v", err)
	}
	if len(shown["v2-target"]) != 0 {
		t.Fatalf("target reactions = %+v, want none for a refused reaction", shown["v2-target"])
	}
}
