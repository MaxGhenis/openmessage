package google

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	gmproto "go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/messaging"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

// reactionDispatchRegistry leases one adapter's text and reaction senders to
// the dispatcher, the way the account registry does for a connected account.
type reactionDispatchRegistry struct {
	accountID string
	adapter   *Adapter
}

func (r reactionDispatchRegistry) Snapshot(string) (bridge.Snapshot, bool) {
	return bridge.Snapshot{}, false
}

func (r reactionDispatchRegistry) Acquire(
	ctx context.Context,
	accountID string,
	capability bridge.Capability,
) (*bridge.DispatchLease, error) {
	if accountID != r.accountID {
		return nil, bridge.ErrCapabilityUnavailable
	}
	lease := &bridge.DispatchLease{AccountID: accountID, Platform: bridge.PlatformGoogle, Generation: 1, Ctx: ctx}
	switch capability {
	case bridge.CapabilityReactions:
		lease.Reaction = r.adapter
	case bridge.CapabilityTextSend:
		lease.Text = r.adapter
	default:
		return nil, bridge.ErrCapabilityUnavailable
	}
	return lease, nil
}

func (r reactionDispatchRegistry) Capabilities(accountID string) bridge.CapabilitySet {
	return bridge.CapabilitySet{Reactions: accountID == r.accountID, TextSend: accountID == r.accountID}
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

const (
	reactionDispatchAccountID      = "google-primary"
	reactionDispatchConversationID = "v2-google-thread"
)

// reactionDispatchFixture is a v2 store with one Google thread holding one
// incoming message, served by the real Google adapter (only libgm's transport
// is faked) through the real MessageService dispatcher.
type reactionDispatchFixture struct {
	t        *testing.T
	ctx      context.Context
	clock    *reactionDispatchClock
	store    *sqlite.Store
	service  *messaging.MessageService
	outbox   *sqlite.OutboxRepository
	reaction *fakeReactionSendClient
	text     *fakeTextSendClient
	keys     int
}

func newReactionDispatchFixture(t *testing.T) *reactionDispatchFixture {
	t.Helper()
	ctx := context.Background()
	clock := &reactionDispatchClock{now: time.UnixMilli(1_900_000_000_000)}
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "v2.sqlite3"))
	if err != nil {
		t.Fatalf("sqlite.Open(): %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	nowMS := clock.Now().UnixMilli()
	if err := store.UpsertAccount(sqlite.Account{
		AccountID: reactionDispatchAccountID, BridgeKey: "google", DisplayName: "Google Messages",
		Mode: sqlite.AccountModeLive, Enabled: true, ConfigJSON: "{}",
		CreatedAtMS: nowMS, UpdatedAtMS: nowMS,
	}); err != nil {
		t.Fatalf("UpsertAccount(): %v", err)
	}
	if err := store.UpsertConversation(sqlite.Conversation{
		ConversationID: reactionDispatchConversationID, AccountID: reactionDispatchAccountID,
		RemoteConversationID: "conversation-id", Kind: sqlite.ConversationKindDirect, Title: "thread",
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
		InboxID: "inbox-target", AccountID: reactionDispatchAccountID, Generation: 1,
		DedupeKey: "inbox-target", Codec: "test", CodecVersion: 1, Payload: []byte("test"),
	}); err != nil {
		t.Fatalf("AppendInbox(): %v", err)
	}
	if err := messages.ProjectMessage(ctx, sqlite.MessageProjection{
		InboxID: "inbox-target",
		Message: sqlite.Message{
			MessageID: "v2-target", ConversationID: reactionDispatchConversationID,
			AccountID: reactionDispatchAccountID, RemoteMessageID: "target-id",
			Direction: sqlite.MessageDirectionIncoming, Body: "react to this",
			State: sqlite.MessageStateActive, OccurredAtMS: nowMS - 60_000,
		},
	}); err != nil {
		t.Fatalf("ProjectMessage(): %v", err)
	}

	reaction := &fakeReactionSendClient{
		conversationResult: &gmproto.Conversation{},
		sendResult:         &gmproto.SendReactionResponse{Success: false},
	}
	text := &fakeTextSendClient{
		conversationResult: &gmproto.Conversation{},
		sendResult:         &gmproto.SendMessageResponse{Status: gmproto.SendMessageResponse_SUCCESS},
	}
	adapter := newReactionSendTestAdapter(t, reaction)
	installTextSendClient(t, adapter.host.GetClient(), text)
	service, err := messaging.NewMessageService(
		store,
		reactionDispatchRegistry{accountID: reactionDispatchAccountID, adapter: adapter},
		nil,
		clock,
		messaging.CryptoIDSource{},
	)
	if err != nil {
		t.Fatalf("NewMessageService(): %v", err)
	}
	outbox, err := sqlite.NewOutboxRepository(store, clock.Now)
	if err != nil {
		t.Fatalf("NewOutboxRepository(): %v", err)
	}
	return &reactionDispatchFixture{
		t: t, ctx: ctx, clock: clock, store: store, service: service, outbox: outbox,
		reaction: reaction, text: text,
	}
}

func (f *reactionDispatchFixture) common() messaging.CommonCommand {
	f.keys++
	return messaging.CommonCommand{
		AccountID:      reactionDispatchAccountID,
		ConversationID: reactionDispatchConversationID,
		IdempotencyKey: fmt.Sprintf("reaction-dispatch-%d", f.keys),
	}
}

func (f *reactionDispatchFixture) react(targetMessageID string) messaging.Submission {
	f.t.Helper()
	submission, err := f.service.SendReaction(f.ctx, messaging.SendReactionCommand{
		CommonCommand:   f.common(),
		TargetMessageID: targetMessageID,
		Emoji:           "👍",
		Action:          bridge.ReactionAdd,
	})
	if err != nil {
		f.t.Fatalf("SendReaction(%q): %v", targetMessageID, err)
	}
	return submission
}

func (f *reactionDispatchFixture) dispatch(want int) {
	f.t.Helper()
	if processed, err := f.service.DispatchDue(f.ctx, 4); err != nil || processed != want {
		f.t.Fatalf("DispatchDue() = %d, %v; want %d, nil", processed, err, want)
	}
}

func (f *reactionDispatchFixture) delivery(outboxID string) messaging.Delivery {
	f.t.Helper()
	delivery, err := f.service.Get(f.ctx, outboxID)
	if err != nil {
		f.t.Fatalf("Get(%q): %v", outboxID, err)
	}
	return delivery
}

// A reaction the phone refuses (SendReactionResponse success: false) must end
// rejected, the state /api/react answers 502 for, and must not be sent again.
// Before, the refusal was classified transient and not dispatched, so the row
// went back to not_dispatched, /api/react answered 202 "keeps trying", and the
// dispatcher sent it again every 5 s with no cap.
func TestGoogleRefusedReactionIsRejectedAndNotRetried(t *testing.T) {
	f := newReactionDispatchFixture(t)
	submission := f.react("v2-target")
	f.dispatch(1)
	if f.reaction.sendCalls != 1 || f.reaction.sent == nil {
		t.Fatalf("libgm SendReaction calls = %d, want 1", f.reaction.sendCalls)
	}
	if delivery := f.delivery(submission.OutboxID); delivery.State != messaging.OutboxRejected ||
		delivery.ErrorClass != string(bridge.FailureUnsupported) || delivery.ErrorCode != "send_reaction" {
		t.Fatalf("delivery = %+v, want rejected unsupported send_reaction", delivery)
	}

	// Well past the retry delay, nothing is due and nothing is sent again.
	f.clock.Advance(time.Hour)
	f.dispatch(0)
	if f.reaction.sendCalls != 1 {
		t.Fatalf("libgm SendReaction calls = %d after an hour, want the refusal never resent", f.reaction.sendCalls)
	}

	// A refused reaction is not shown as this account's own.
	reactions, err := sqlite.NewReactionRepository(f.store, f.clock.Now)
	if err != nil {
		t.Fatalf("NewReactionRepository(): %v", err)
	}
	shown, err := reactions.ReactionsForMessages(f.ctx, []string{"v2-target"})
	if err != nil {
		t.Fatalf("ReactionsForMessages(): %v", err)
	}
	if len(shown["v2-target"]) != 0 {
		t.Fatalf("target reactions = %+v, want none for a refused reaction", shown["v2-target"])
	}
}

// A message this account just sent is named by its send's request ID (Google's
// TmpID) until the phone's echo brings the phone's own ID. A refusal of a
// reaction to it then says nothing final: the retry names the phone's ID. So
// that refusal is retried, and the reaction goes out once the echo arrives.
// A refusal of a reaction to the same message once it has its own ID, or to a
// message whose send was canceled and so never gets one, is final.
func TestGoogleReactionToOwnMessageRetriesUntilItsEchoNamesIt(t *testing.T) {
	f := newReactionDispatchFixture(t)
	sent, err := f.service.SendText(f.ctx, messaging.SendTextCommand{CommonCommand: f.common(), Body: "my message"})
	if err != nil {
		t.Fatalf("SendText(): %v", err)
	}
	f.dispatch(1)
	sendRow, err := f.outbox.FindByID(f.ctx, sent.OutboxID)
	if err != nil {
		t.Fatalf("FindByID(text): %v", err)
	}
	if sendRow.State != sqlite.OutboxConfirmed || f.text.sent.GetTmpID() != sendRow.TransportRequestID {
		t.Fatalf("text send = %+v with TmpID %q, want confirmed under its request ID", sendRow, f.text.sent.GetTmpID())
	}

	reaction := f.react(sent.LocalMessageID)
	f.dispatch(1)
	if got := f.reaction.sent.GetReactionData(); f.reaction.sendCalls != 1 || f.reaction.sent.GetMessageID() != sendRow.TransportRequestID {
		t.Fatalf("first reaction attempt = %d calls naming %q (%v), want one naming the request ID %q",
			f.reaction.sendCalls, f.reaction.sent.GetMessageID(), got, sendRow.TransportRequestID)
	}
	if delivery := f.delivery(reaction.OutboxID); delivery.State != messaging.OutboxNotDispatched ||
		delivery.ErrorClass != string(bridge.FailureTransient) {
		t.Fatalf("delivery after a refusal of a provisionally named target = %+v, want not_dispatched transient", delivery)
	}

	// The phone's echo names the message by its own ID; the phone now accepts.
	if _, err := f.service.ObserveTransportEcho(f.ctx, messaging.TransportEcho{
		AccountID: reactionDispatchAccountID, TransportRequestID: sendRow.TransportRequestID, RemoteMessageID: "phone-message-id",
	}); err != nil {
		t.Fatalf("ObserveTransportEcho(): %v", err)
	}
	f.reaction.sendResult = &gmproto.SendReactionResponse{Success: true}
	f.clock.Advance(time.Minute)
	f.dispatch(1)
	if f.reaction.sendCalls != 2 || f.reaction.sent.GetMessageID() != "phone-message-id" {
		t.Fatalf("retry = %d calls naming %q, want a second naming phone-message-id", f.reaction.sendCalls, f.reaction.sent.GetMessageID())
	}
	if delivery := f.delivery(reaction.OutboxID); delivery.State != messaging.OutboxConfirmed {
		t.Fatalf("delivery after the echo = %+v, want confirmed", delivery)
	}

	// Named by its own ID, the same message's refusal is final.
	f.reaction.sendResult = &gmproto.SendReactionResponse{Success: false}
	again := f.react(sent.LocalMessageID)
	f.dispatch(1)
	if delivery := f.delivery(again.OutboxID); delivery.State != messaging.OutboxRejected {
		t.Fatalf("delivery of a refused reaction to a reconciled own message = %+v, want rejected", delivery)
	}

	// A canceled send never gets the phone's ID, so a refusal is final too.
	later := f.common()
	later.NotBefore = f.clock.Now().Add(24 * time.Hour)
	canceled, err := f.service.SendText(f.ctx, messaging.SendTextCommand{CommonCommand: later, Body: "never sent"})
	if err != nil {
		t.Fatalf("SendText(scheduled): %v", err)
	}
	if _, err := f.service.Cancel(f.ctx, canceled.OutboxID); err != nil {
		t.Fatalf("Cancel(): %v", err)
	}
	toCanceled := f.react(canceled.LocalMessageID)
	f.dispatch(1)
	if delivery := f.delivery(toCanceled.OutboxID); delivery.State != messaging.OutboxRejected {
		t.Fatalf("delivery of a refused reaction to a canceled send = %+v, want rejected", delivery)
	}
}

// A reaction to a message this account has scheduled but not sent asks the
// phone nothing: the phone has no such message, so the reaction waits for the
// send. Before, it was sent every 5 s naming the send's TmpID, the phone
// refused it, and the refusal was retried until someone canceled the reaction.
// Once the send goes out the reaction follows it.
func TestGoogleReactionToAScheduledSendWaitsWithoutCallingThePhone(t *testing.T) {
	f := newReactionDispatchFixture(t)
	later := f.common()
	later.NotBefore = f.clock.Now().Add(time.Hour)
	scheduled, err := f.service.SendText(f.ctx, messaging.SendTextCommand{CommonCommand: later, Body: "sent in an hour"})
	if err != nil {
		t.Fatalf("SendText(scheduled): %v", err)
	}
	reaction := f.react(scheduled.LocalMessageID)
	f.dispatch(1)
	for range 3 {
		if f.reaction.conversationCalls != 0 || f.reaction.sendCalls != 0 {
			t.Fatalf("phone calls for a reaction to an unsent message = %d conversation, %d reaction; want none",
				f.reaction.conversationCalls, f.reaction.sendCalls)
		}
		if delivery := f.delivery(reaction.OutboxID); delivery.State != messaging.OutboxNotDispatched ||
			delivery.ErrorCode != "reaction_target_unsent" {
			t.Fatalf("delivery = %+v, want not_dispatched reaction_target_unsent", delivery)
		}
		// The hold waits for the send, so nothing is due again before it.
		f.clock.Advance(20 * time.Minute)
		if f.clock.Now().Before(later.NotBefore) {
			f.dispatch(0)
		}
	}

	// The send is due an hour after it was queued, and the held reaction
	// just after it. A dispatcher that reaches both sends the text first
	// (confirmed under its TmpID) and then the reaction, naming that TmpID.
	f.reaction.sendResult = &gmproto.SendReactionResponse{Success: true}
	f.clock.Advance(time.Second)
	f.dispatch(2)
	sendRow, err := f.outbox.FindByID(f.ctx, scheduled.OutboxID)
	if err != nil {
		t.Fatalf("FindByID(text): %v", err)
	}
	if sendRow.State != sqlite.OutboxConfirmed || f.reaction.sendCalls != 1 ||
		f.reaction.sent.GetMessageID() != sendRow.TransportRequestID {
		t.Fatalf("after the send went out: send %+v, %d reaction calls naming %q; want confirmed and one naming %q",
			sendRow, f.reaction.sendCalls, f.reaction.sent.GetMessageID(), sendRow.TransportRequestID)
	}
	if delivery := f.delivery(reaction.OutboxID); delivery.State != messaging.OutboxConfirmed {
		t.Fatalf("delivery after its target went out = %+v, want confirmed", delivery)
	}
}

// A reaction to a message this account sent whose echo never arrives (on the
// live install no echo carries its TmpID, and only a text's echo pairs by
// content, within seconds) keeps that TmpID for good. The phone's refusal is
// retried while the echo could still rename the message, and is final once
// provisionalTargetWindow (one minute) has passed since the send went out.
// Before, it was retried every 5 s with no end.
func TestGoogleRefusalOfAnUnpairedOwnMessageIsFinalAfterTheEchoWindow(t *testing.T) {
	f := newReactionDispatchFixture(t)
	sent, err := f.service.SendText(f.ctx, messaging.SendTextCommand{CommonCommand: f.common(), Body: "no echo will pair"})
	if err != nil {
		t.Fatalf("SendText(): %v", err)
	}
	f.dispatch(1)
	sendRow, err := f.outbox.FindByID(f.ctx, sent.OutboxID)
	if err != nil {
		t.Fatalf("FindByID(text): %v", err)
	}
	if sendRow.State != sqlite.OutboxConfirmed {
		t.Fatalf("send = %+v, want confirmed under its request ID", sendRow)
	}
	wentOut := time.UnixMilli(sendRow.UpdatedAtMS)

	reaction := f.react(sent.LocalMessageID)
	var attempts []time.Time
	for range 100 {
		before := f.reaction.sendCalls
		if processed, err := f.service.DispatchDue(f.ctx, 4); err != nil || processed > 1 {
			t.Fatalf("DispatchDue() = %d, %v; want at most the reaction", processed, err)
		}
		if f.reaction.sendCalls > before {
			attempts = append(attempts, f.clock.Now())
		}
		if f.delivery(reaction.OutboxID).State == messaging.OutboxRejected {
			break
		}
		f.clock.Advance(time.Second)
	}
	delivery := f.delivery(reaction.OutboxID)
	if delivery.State != messaging.OutboxRejected || delivery.ErrorClass != string(bridge.FailureUnsupported) {
		t.Fatalf("delivery after %d refusals = %+v, want rejected unsupported", len(attempts), delivery)
	}
	last := attempts[len(attempts)-1]
	if last.Before(wentOut.Add(time.Minute)) {
		t.Fatalf("final refusal at %s after the send went out, want it no sooner than the one-minute window", last.Sub(wentOut))
	}
	for _, at := range attempts[:len(attempts)-1] {
		if !at.Before(wentOut.Add(time.Minute)) {
			t.Fatalf("a refusal at %s after the send went out was retried, want every refusal past the window final", at.Sub(wentOut))
		}
	}
	// One attempt per 5 s retry inside the window, and the one that ends it.
	if len(attempts) > 13 {
		t.Fatalf("phone asked %d times, want at most 13 (one minute at one per 5 s, plus the last)", len(attempts))
	}
	if f.reaction.sent.GetMessageID() != sendRow.TransportRequestID {
		t.Fatalf("refused reaction named %q, want the send's TmpID %q", f.reaction.sent.GetMessageID(), sendRow.TransportRequestID)
	}

	// Nothing is sent again.
	f.clock.Advance(time.Hour)
	f.dispatch(0)
	if got := f.reaction.sendCalls; got != len(attempts) {
		t.Fatalf("phone asked %d times after the rejection, want %d", got, len(attempts))
	}
}
