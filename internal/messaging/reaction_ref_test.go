package messaging

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

// notDispatchedReaction is what a transport answers when it cannot name the
// target yet (Signal: no sent timestamp): nothing was sent, retry later.
func notDispatchedReaction() sendStep {
	return sendStep{err: bridge.OpError{
		Class: bridge.FailureTransient, Operation: reactionOperation,
		Dispatch: bridge.DispatchNotCalled, Cause: errors.New("scripted: target has no transport timestamp"),
	}}
}

// A transport that needs to know whose message a reaction targets cannot read
// that off AuthorID: it is empty for this account's sends and for an incoming
// message stored with no sender alike.
func TestReactionTargetRefTellsAnOwnMessageFromASenderlessIncomingOne(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	senderless := mustProjectDispatchMessage(t, store, clock, sqlite.Message{
		MessageID:       "message-senderless-incoming",
		ConversationID:  "conversation-1",
		AccountID:       "account-1",
		RemoteMessageID: "1700000000123",
		Direction:       sqlite.MessageDirectionIncoming,
		Body:            "group message stored with no source",
		State:           sqlite.MessageStateActive,
		OccurredAtMS:    1_700_000_000_123,
	})
	text := &scriptedTextSender{steps: []sendStep{{result: bridge.SendResult{RemoteMessageID: "1700000000555"}}}}
	reactions := &scriptedReactionSender{steps: []sendStep{{}, {}}}
	registry := newScriptedRegistry("reaction-direction", text)
	registry.setReactionSender(reactions)
	registry.setAvailable(true)
	service := newMessagingTestService(t, store, registry, clock)
	own := mustSendText(t, service, SendTextCommand{
		CommonCommand: testCommonCommand("reaction-direction-own"),
		Body:          "own send",
	})
	if processed, err := service.DispatchDue(context.Background(), 1); err != nil || processed != 1 {
		t.Fatalf("DispatchDue(own send) = %d, %v; want 1, nil", processed, err)
	}

	for index, targetID := range []string{senderless.MessageID, own.LocalMessageID} {
		mustSendDispatchReaction(t, service, SendReactionCommand{
			CommonCommand:   testCommonCommand(fmt.Sprintf("reaction-direction-%d", index)),
			TargetMessageID: targetID,
			Emoji:           "👍",
		})
		clock.Advance(time.Millisecond)
	}
	if processed, err := service.DispatchDue(context.Background(), 2); err != nil || processed != 2 {
		t.Fatalf("DispatchDue(reactions) = %d, %v; want 2, nil", processed, err)
	}
	requests := reactions.snapshotRequests()
	if len(requests) != 2 {
		t.Fatalf("reaction requests = %+v, want two", requests)
	}
	wantIncoming := bridge.MessageRef{RemoteID: "1700000000123", SentAt: time.UnixMilli(1_700_000_000_123)}
	wantOwn := bridge.MessageRef{
		RemoteID: "1700000000555", Outgoing: true, SentAt: time.UnixMilli(messagingTestTime.UnixMilli()),
	}
	if !targetRefEqual(requests[0].Target, wantIncoming) {
		t.Fatalf("senderless incoming target = %+v, want %+v", requests[0].Target, wantIncoming)
	}
	if !targetRefEqual(requests[1].Target, wantOwn) {
		t.Fatalf("own target = %+v, want %+v", requests[1].Target, wantOwn)
	}
}

// A reaction to this account's own send that the transport has not accepted
// carries the send's request ID. Once the send is confirmed, a retry carries
// the transport's ID for it.
func TestReactionToOwnPendingSendFollowsItsConfirmation(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	text := &scriptedTextSender{steps: []sendStep{{result: bridge.SendResult{RemoteMessageID: "1700000000555"}}}}
	reactions := &scriptedReactionSender{steps: []sendStep{notDispatchedReaction(), {}}}
	registry := newScriptedRegistry("reaction-own-pending", text)
	registry.setReactionSender(reactions)
	registry.setAvailable(true)
	service := newMessagingTestService(t, store, registry, clock)

	// The reaction is due now; its target is scheduled a little later, so the
	// first reaction attempt finds a message the transport has not accepted.
	ownCommand := testCommonCommand("reaction-own-pending-send")
	ownCommand.NotBefore = clock.Now().Add(2 * time.Second)
	own := mustSendText(t, service, SendTextCommand{CommonCommand: ownCommand, Body: "scheduled"})
	ownRow := mustOutboxItem(t, service, own.OutboxID)
	reaction := mustSendDispatchReaction(t, service, SendReactionCommand{
		CommonCommand:   testCommonCommand("reaction-own-pending"),
		TargetMessageID: own.LocalMessageID,
		Emoji:           "👍",
	})

	if processed, err := service.DispatchDue(context.Background(), 4); err != nil || processed != 1 {
		t.Fatalf("DispatchDue(reaction before its target) = %d, %v; want 1, nil", processed, err)
	}
	submittedAt := time.UnixMilli(messagingTestTime.UnixMilli())
	requests := reactions.snapshotRequests()
	wantPending := bridge.MessageRef{RemoteID: ownRow.TransportRequestID, Outgoing: true, SentAt: submittedAt}
	if len(requests) != 1 || !targetRefEqual(requests[0].Target, wantPending) {
		t.Fatalf("reaction target while the send was pending = %+v, want %+v", requests, wantPending)
	}
	if row := mustOutboxItem(t, service, reaction.OutboxID); row.State != sqlite.OutboxNotDispatched {
		t.Fatalf("reaction row = %+v, want not_dispatched awaiting its retry", row)
	}

	clock.Advance(3 * time.Second)
	if processed, err := service.DispatchDue(context.Background(), 4); err != nil || processed != 1 {
		t.Fatalf("DispatchDue(own send) = %d, %v; want 1, nil", processed, err)
	}
	clock.Advance(defaultRetryDelay)
	if processed, err := service.DispatchDue(context.Background(), 4); err != nil || processed != 1 {
		t.Fatalf("DispatchDue(reaction retry) = %d, %v; want 1, nil", processed, err)
	}
	requests = reactions.snapshotRequests()
	wantConfirmed := bridge.MessageRef{RemoteID: "1700000000555", Outgoing: true, SentAt: submittedAt}
	if len(requests) != 2 || !targetRefEqual(requests[1].Target, wantConfirmed) {
		t.Fatalf("reaction target after the send confirmed = %+v, want %+v", requests, wantConfirmed)
	}
	if delivery := mustDelivery(t, service, reaction.OutboxID); delivery.State != OutboxConfirmed {
		t.Fatalf("reaction delivery = %+v, want confirmed", delivery)
	}
}

func TestDescribedReplyRefStatesWhoSentTheQuotedMessage(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	incoming := projectReplyTarget(t, store, clock, sqlite.Message{
		MessageID:       "message-quoted-senderless",
		ConversationID:  "conversation-1",
		AccountID:       "account-1",
		RemoteMessageID: "4f2b7c9d0e1a3b5c7d9e1f2a4b6c8d0e2f4a6b8c",
		Direction:       sqlite.MessageDirectionIncoming,
		Body:            "incoming",
		State:           sqlite.MessageStateActive,
		OccurredAtMS:    1_700_000_000_123,
	})
	text := &scriptedTextSender{steps: []sendStep{
		{result: bridge.SendResult{RemoteMessageID: "1700000000555"}},
		{result: bridge.SendResult{RemoteMessageID: "1700000000556"}},
		{result: bridge.SendResult{RemoteMessageID: "1700000000557"}},
	}}
	registry := newScriptedRegistry("reply-direction", text)
	registry.setAvailable(true)
	service := newMessagingTestService(t, store, registry, clock)
	own := mustSendText(t, service, SendTextCommand{
		CommonCommand: testCommonCommand("reply-direction-own"),
		Body:          "own send",
	})
	if processed, err := service.DispatchDue(context.Background(), 1); err != nil || processed != 1 {
		t.Fatalf("DispatchDue(own send) = %d, %v; want 1, nil", processed, err)
	}
	for index, quotedID := range []string{incoming.MessageID, own.LocalMessageID} {
		clock.Advance(time.Millisecond)
		mustSendText(t, service, SendTextCommand{
			CommonCommand:    testCommonCommand(fmt.Sprintf("reply-direction-%d", index)),
			Body:             "reply",
			ReplyToMessageID: quotedID,
		})
	}
	if processed, err := service.DispatchDue(context.Background(), 2); err != nil || processed != 2 {
		t.Fatalf("DispatchDue(replies) = %d, %v; want 2, nil", processed, err)
	}
	requests := text.snapshotRequests()
	if len(requests) != 3 || requests[1].ReplyTo == nil || requests[2].ReplyTo == nil {
		t.Fatalf("text requests = %+v, want the own send and two replies", requests)
	}
	if requests[1].ReplyTo.Outgoing || requests[1].ReplyTo.AuthorID != "" {
		t.Fatalf("reply ref to a senderless incoming message = %+v, want no author and not outgoing", requests[1].ReplyTo)
	}
	if !requests[2].ReplyTo.Outgoing || requests[2].ReplyTo.AuthorID != "" {
		t.Fatalf("reply ref to an own send = %+v, want outgoing", requests[2].ReplyTo)
	}
}

// TestReactionTargetRefInvariants checks, over seeded random conversations,
// the target ref of every reaction the dispatcher maps:
//
//   - The ref names the target message's current remote ID: its outbox
//     request ID until the transport confirms the send, then the transport's.
//   - Outgoing is set exactly for a message this account sent, whether or not
//     the stored message names a sender.
//   - AuthorID is the sender's canonical value, "" when the message names
//     none, and SentAt is the occurred time.
//   - The reply-only fields (Text and the attachment fields) stay empty.
//   - The mapping is deterministic: a retried attempt carries the same ref.
func TestReactionTargetRefInvariants(t *testing.T) {
	for seed := int64(1); seed <= 40; seed++ {
		seed := seed
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			checkReactionTargetRefInvariants(t, seed)
		})
	}
}

type reactionRefCase struct {
	name     string
	targetID string // local message ID the reaction targets
	want     bridge.MessageRef
}

func checkReactionTargetRefInvariants(t *testing.T, seed int64) {
	random := rand.New(rand.NewSource(seed))
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	text := &scriptedTextSender{}
	media := &scriptedMediaSender{}
	reactions := &scriptedReactionSender{}
	registry := newScriptedRegistry(bridge.Platform(fmt.Sprintf("reaction-invariants-%d", seed)), text)
	registry.setMediaSender(media)
	registry.setReactionSender(reactions)
	registry.setAvailable(true)
	service := newMessagingTestService(t, store, registry, clock)

	var cases []reactionRefCase
	identityCount := 1 + random.Intn(3)
	for index := 0; index < identityCount; index++ {
		seedDispatchIdentity(t, store, fmt.Sprintf("identity-%d", index), fmt.Sprintf("+1555000%04d", index), clock.Now())
	}
	incomingCount := 1 + random.Intn(4)
	for index := 0; index < incomingCount; index++ {
		message := sqlite.Message{
			MessageID:      fmt.Sprintf("message-incoming-%d", index),
			ConversationID: "conversation-1",
			AccountID:      "account-1",
			Direction:      sqlite.MessageDirectionIncoming,
			Body:           "incoming body is not copied",
			State:          sqlite.MessageStateActive,
			OccurredAtMS:   1_700_000_000_000 + random.Int63n(1_000_000_000),
		}
		// Incoming IDs are SHA-1s from the live decoder and can be decimal
		// for a migrated row.
		if random.Intn(3) == 0 {
			message.RemoteMessageID = fmt.Sprintf("%d", 1_600_000_000_000+int64(index))
		} else {
			message.RemoteMessageID = fmt.Sprintf("%040x", random.Uint64())
		}
		author := ""
		if random.Intn(3) != 0 {
			identity := random.Intn(identityCount)
			identityID := fmt.Sprintf("identity-%d", identity)
			message.SenderIdentityID = &identityID
			author = fmt.Sprintf("+1555000%04d", identity)
		}
		var attachmentMIMEs []string
		if random.Intn(2) == 0 {
			attachmentMIMEs = []string{"image/jpeg"}
		}
		projectReplyTarget(t, store, clock, message, attachmentMIMEs...)
		cases = append(cases, reactionRefCase{
			name: message.MessageID, targetID: message.MessageID,
			want: bridge.MessageRef{
				RemoteID: message.RemoteMessageID, AuthorID: author, SentAt: time.UnixMilli(message.OccurredAtMS),
			},
		})
	}

	outgoingCount := 1 + random.Intn(5)
	for index := 0; index < outgoingCount; index++ {
		occurredMS := clock.Now().UnixMilli()
		// 0: confirmed by the transport, 1: canceled before sending,
		// 2: scheduled far in the future, so still unsent.
		fate := random.Intn(3)
		command := testCommonCommand(fmt.Sprintf("outgoing-%d", index))
		if fate == 2 {
			command.NotBefore = clock.Now().Add(1000 * time.Hour)
		}
		isMedia := random.Intn(3) == 0
		var submission Submission
		if isMedia {
			submission = mustSendMedia(t, service, SendMediaCommand{
				CommonCommand: command,
				Content:       bytes.NewReader([]byte(fmt.Sprintf("bytes-%d", index))),
				Filename:      "attachment.bin",
				MIME:          "image/png",
			})
		} else {
			submission = mustSendText(t, service, SendTextCommand{CommonCommand: command, Body: "outgoing text"})
		}
		row := mustOutboxItem(t, service, submission.OutboxID)
		wantRemote := row.TransportRequestID
		switch fate {
		case 0:
			wantRemote = fmt.Sprintf("%d", 1_700_000_000_000+int64(index))
			step := sendStep{result: bridge.SendResult{RemoteMessageID: wantRemote}}
			if isMedia {
				media.mu.Lock()
				media.steps = append(media.steps, step)
				media.mu.Unlock()
			} else {
				text.mu.Lock()
				text.steps = append(text.steps, step)
				text.mu.Unlock()
			}
			if processed, err := service.DispatchDue(context.Background(), 1); err != nil || processed != 1 {
				t.Fatalf("seed %d: DispatchDue(outgoing %d) = %d, %v", seed, index, processed, err)
			}
		case 1:
			if _, err := service.Cancel(context.Background(), submission.OutboxID); err != nil {
				t.Fatalf("seed %d: Cancel(outgoing %d): %v", seed, index, err)
			}
		}
		cases = append(cases, reactionRefCase{
			name: submission.LocalMessageID, targetID: submission.LocalMessageID,
			want: bridge.MessageRef{RemoteID: wantRemote, Outgoing: true, SentAt: time.UnixMilli(occurredMS)},
		})
		clock.Advance(time.Second)
	}

	random.Shuffle(len(cases), func(i, j int) { cases[i], cases[j] = cases[j], cases[i] })
	for index, tc := range cases {
		// The first attempt fails before dispatch, so the target is mapped twice.
		reactions.mu.Lock()
		reactions.steps = append(reactions.steps, notDispatchedReaction(), sendStep{})
		before := len(reactions.requests)
		reactions.mu.Unlock()
		mustSendDispatchReaction(t, service, SendReactionCommand{
			CommonCommand:   testCommonCommand(fmt.Sprintf("reaction-%d", index)),
			TargetMessageID: tc.targetID,
			Emoji:           "👍",
		})
		for attempt := 0; attempt < 2; attempt++ {
			if processed, err := service.DispatchDue(context.Background(), 1); err != nil || processed != 1 {
				t.Fatalf("seed %d case %s attempt %d: DispatchDue = %d, %v", seed, tc.name, attempt, processed, err)
			}
			clock.Advance(defaultRetryDelay + time.Second)
		}
		requests := reactions.snapshotRequests()[before:]
		if len(requests) != 2 {
			t.Fatalf("seed %d case %s: reaction attempts = %d, want 2", seed, tc.name, len(requests))
		}
		if !targetRefEqual(requests[0].Target, requests[1].Target) {
			t.Fatalf("seed %d case %s: retried target %+v != first %+v", seed, tc.name, requests[1].Target, requests[0].Target)
		}
		if got := requests[1].Target; !targetRefEqual(got, tc.want) {
			t.Fatalf("seed %d case %s: target = %+v, want %+v", seed, tc.name, got, tc.want)
		}
	}
}

// targetRefEqual compares every field of two refs, the direction included.
func targetRefEqual(got, want bridge.MessageRef) bool {
	return got.Outgoing == want.Outgoing && messageRefEqual(&got, &want)
}
