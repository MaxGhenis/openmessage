package messaging

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

func TestSendAgainTextReconstructsAllowedStatesFromDurableMessage(t *testing.T) {
	for _, state := range []OutboxState{OutboxUncertain, OutboxRejected} {
		state := state
		t.Run(string(state), func(t *testing.T) {
			clock := newManualClock(messagingTestTime)
			store := openMessagingTestStore(t, clock.Now())
			const replyRemoteID = "remote-send-again-reply"
			const replyMessageID = "message-send-again-reply"
			seedMessagingMessage(t, store, clock, sqlite.Message{
				MessageID:       replyMessageID,
				ConversationID:  "conversation-1",
				AccountID:       "account-1",
				RemoteMessageID: replyRemoteID,
				Direction:       sqlite.MessageDirectionIncoming,
				Body:            "reply target",
				State:           sqlite.MessageStateActive,
				OccurredAtMS:    clock.Now().UnixMilli(),
			})

			sender := &scriptedTextSender{steps: []sendStep{sendAgainStepForState(state)}}
			registry := newScriptedRegistry("send-again-text", sender)
			registry.setAvailable(true)
			service := newMessagingTestService(t, store, registry, clock)
			original := mustSendText(t, service, SendTextCommand{
				CommonCommand:    testCommonCommand("key-send-again-text-" + string(state)),
				Body:             "durably reconstructed text",
				ReplyToMessageID: replyMessageID,
			})
			if processed, err := service.DispatchDue(context.Background(), 1); err != nil || processed != 1 {
				t.Fatalf("DispatchDue(%s) = %d, %v; want 1, nil", state, processed, err)
			}
			before := mustOutboxItem(t, service, original.OutboxID)
			if before.State != state {
				t.Fatalf("original state = %q, want %q", before.State, state)
			}
			clock.Advance(time.Second)

			changed := service.changeChannel()
			drainSendAgainWake(service)
			resent, err := service.SendAgain(
				context.Background(),
				original.OutboxID,
				"key-send-again-text-new-"+string(state),
			)
			if err != nil {
				t.Fatalf("SendAgain(%s): %v", state, err)
			}
			if resent.State != OutboxQueued || resent.OutboxID == original.OutboxID ||
				resent.LocalMessageID == original.LocalMessageID || resent.LocalMessageID == "" {
				t.Fatalf("resent submission = %+v, original = %+v", resent, original)
			}
			select {
			case <-changed:
			default:
				t.Fatal("SendAgain(inserted) did not signal a delivery change")
			}
			select {
			case <-service.wake:
			default:
				t.Fatal("SendAgain(inserted) did not wake the dispatcher")
			}

			after := mustOutboxItem(t, service, original.OutboxID)
			if !reflect.DeepEqual(after, before) {
				t.Fatalf("SendAgain mutated old row:\n before=%+v\n  after=%+v", before, after)
			}
			resentItem := mustOutboxItem(t, service, resent.OutboxID)
			if resentItem.State != sqlite.OutboxQueued || resentItem.SendAgainOfOutboxID == nil ||
				*resentItem.SendAgainOfOutboxID != original.OutboxID ||
				resentItem.TransportRequestID == before.TransportRequestID {
				t.Fatalf("resent outbox row = %+v", resentItem)
			}
			resentMessage, err := service.messages.GetMessage(context.Background(), resent.LocalMessageID)
			if err != nil {
				t.Fatalf("GetMessage(resent): %v", err)
			}
			if resentMessage.Body != "durably reconstructed text" ||
				resentMessage.ReplyToRemoteID == nil || *resentMessage.ReplyToRemoteID != replyRemoteID ||
				resentMessage.RemoteMessageID != resentItem.TransportRequestID {
				t.Fatalf("resent message = %+v", resentMessage)
			}
		})
	}
}

func TestSendAgainMediaReusesDurableAttachmentWithoutBlobStore(t *testing.T) {
	for _, state := range []OutboxState{OutboxUncertain, OutboxRejected} {
		state := state
		t.Run(string(state), func(t *testing.T) {
			clock := newManualClock(messagingTestTime)
			store := openMessagingTestStore(t, clock.Now())
			replyMessageID := "message-send-again-media-reply-" + string(state)
			replyRemoteID := "remote-send-again-media-reply-" + string(state)
			seedMessagingMessage(t, store, clock, sqlite.Message{
				MessageID:       replyMessageID,
				ConversationID:  "conversation-1",
				AccountID:       "account-1",
				RemoteMessageID: replyRemoteID,
				Direction:       sqlite.MessageDirectionIncoming,
				Body:            "media reply target",
				State:           sqlite.MessageStateActive,
				OccurredAtMS:    clock.Now().UnixMilli(),
			})
			blobs, blobRoot := newMessagingTestBlobStore(t)
			sender := &scriptedMediaSender{steps: []sendStep{sendAgainStepForState(state)}}
			registry := newScriptedRegistry("send-again-media", nil)
			registry.setMediaSender(sender)
			registry.setAvailable(true)
			service := newMessagingTestServiceWithBlobs(t, store, registry, blobs, clock)
			content := []byte("send-again must reuse this content-addressed blob")
			original := mustSendMedia(t, service, SendMediaCommand{
				CommonCommand:    testCommonCommand("key-send-again-media-" + string(state)),
				Content:          bytes.NewReader(content),
				Filename:         "preserved.bin",
				MIME:             "application/x-send-again",
				Caption:          "durably reconstructed caption",
				ReplyToMessageID: replyMessageID,
			})
			if processed, err := service.DispatchDue(context.Background(), 1); err != nil || processed != 1 {
				t.Fatalf("DispatchDue(%s) = %d, %v; want 1, nil", state, processed, err)
			}
			before := mustOutboxItem(t, service, original.OutboxID)
			beforeAttachment, err := service.outbox.GetOutboxAttachment(context.Background(), original.OutboxID)
			if err != nil {
				t.Fatalf("GetOutboxAttachment(original): %v", err)
			}
			beforeFiles := messagingBlobFiles(t, blobRoot)
			if len(beforeFiles) != 1 {
				t.Fatalf("blob files before SendAgain = %v, want one", beforeFiles)
			}
			clock.Advance(time.Second)

			// A nil blob store makes any Put, Open, or existence pre-check fail. The
			// durable attachment carrier is sufficient for SendAgain.
			service.blobs = nil
			resent, err := service.SendAgain(
				context.Background(),
				original.OutboxID,
				"key-send-again-media-new-"+string(state),
			)
			if err != nil {
				t.Fatalf("SendAgain(%s): %v", state, err)
			}
			if resent.State != OutboxQueued || resent.OutboxID == original.OutboxID ||
				resent.LocalMessageID == original.LocalMessageID || resent.LocalMessageID == "" {
				t.Fatalf("resent submission = %+v, original = %+v", resent, original)
			}
			if after := mustOutboxItem(t, service, original.OutboxID); !reflect.DeepEqual(after, before) {
				t.Fatalf("SendAgain mutated old media row:\n before=%+v\n  after=%+v", before, after)
			}
			resentItem := mustOutboxItem(t, service, resent.OutboxID)
			if resentItem.SendAgainOfOutboxID == nil || *resentItem.SendAgainOfOutboxID != original.OutboxID ||
				resentItem.TransportRequestID == before.TransportRequestID {
				t.Fatalf("resent media outbox row = %+v", resentItem)
			}
			resentAttachment, err := service.outbox.GetOutboxAttachment(context.Background(), resent.OutboxID)
			if err != nil {
				t.Fatalf("GetOutboxAttachment(resent): %v", err)
			}
			if resentAttachment.BlobHash != beforeAttachment.BlobHash ||
				resentAttachment.SizeBytes != beforeAttachment.SizeBytes ||
				resentAttachment.MIME != beforeAttachment.MIME ||
				resentAttachment.Filename != beforeAttachment.Filename {
				t.Fatalf("resent attachment = %+v, want metadata from %+v", resentAttachment, beforeAttachment)
			}
			if afterFiles := messagingBlobFiles(t, blobRoot); !reflect.DeepEqual(afterFiles, beforeFiles) {
				t.Fatalf("blob files after SendAgain = %v, want unchanged %v", afterFiles, beforeFiles)
			}
			resentMessage, err := service.messages.GetMessage(context.Background(), resent.LocalMessageID)
			if err != nil {
				t.Fatalf("GetMessage(resent media): %v", err)
			}
			if resentMessage.Body != "durably reconstructed caption" ||
				resentMessage.ReplyToRemoteID == nil || *resentMessage.ReplyToRemoteID != replyRemoteID ||
				resentMessage.RemoteMessageID != resentItem.TransportRequestID {
				t.Fatalf("resent media message = %+v", resentMessage)
			}
		})
	}
}

func TestSendAgainIdempotencyUsesNewKeyAndLinksEachDeliberateIntent(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	sender := &scriptedTextSender{steps: []sendStep{{result: bridge.SendResult{}}}}
	registry := newScriptedRegistry("send-again-idempotency", sender)
	registry.setAvailable(true)
	service := newMessagingTestService(t, store, registry, clock)
	original := mustSendText(t, service, SendTextCommand{
		CommonCommand: testCommonCommand("key-send-again-original"),
		Body:          "repeat only when explicitly requested",
	})
	if processed, err := service.DispatchDue(context.Background(), 1); err != nil || processed != 1 {
		t.Fatalf("DispatchDue() = %d, %v; want 1, nil", processed, err)
	}

	first, err := service.SendAgain(context.Background(), original.OutboxID, "key-send-again-first")
	if err != nil {
		t.Fatalf("SendAgain(first): %v", err)
	}
	replay, err := service.SendAgain(context.Background(), original.OutboxID, "key-send-again-first")
	if err != nil {
		t.Fatalf("SendAgain(replay): %v", err)
	}
	assertDeduplicatedSubmission(t, first, replay)
	second, err := service.SendAgain(context.Background(), original.OutboxID, "key-send-again-second")
	if err != nil {
		t.Fatalf("SendAgain(second key): %v", err)
	}
	if second.OutboxID == first.OutboxID || second.LocalMessageID == first.LocalMessageID {
		t.Fatalf("different keys returned same identity: first=%+v second=%+v", first, second)
	}
	for _, submission := range []Submission{first, second} {
		item := mustOutboxItem(t, service, submission.OutboxID)
		if item.SendAgainOfOutboxID == nil || *item.SendAgainOfOutboxID != original.OutboxID {
			t.Fatalf("linked row = %+v, want predecessor %q", item, original.OutboxID)
		}
	}
	if got := mustOutboxItem(t, service, original.OutboxID).State; got != sqlite.OutboxUncertain {
		t.Fatalf("original state = %q, want uncertain", got)
	}
}

// The SendAgain link is part of an intent's identity: reusing one idempotency
// key against two different predecessors whose payloads hash identically must
// conflict loudly, never silently return the first successor.
func TestSendAgainSameKeyAcrossDifferentPredecessorsConflicts(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	sender := &scriptedTextSender{steps: []sendStep{
		{result: bridge.SendResult{}},
		{result: bridge.SendResult{}},
	}}
	registry := newScriptedRegistry("send-again-cross-predecessor", sender)
	registry.setAvailable(true)
	service := newMessagingTestService(t, store, registry, clock)

	const identicalBody = "identical payload either way"
	first := mustSendText(t, service, SendTextCommand{
		CommonCommand: testCommonCommand("key-cross-predecessor-one"),
		Body:          identicalBody,
	})
	second := mustSendText(t, service, SendTextCommand{
		CommonCommand: testCommonCommand("key-cross-predecessor-two"),
		Body:          identicalBody,
	})
	if processed, err := service.DispatchDue(context.Background(), 2); err != nil || processed != 2 {
		t.Fatalf("DispatchDue() = %d, %v; want 2, nil", processed, err)
	}

	const sharedKey = "key-cross-predecessor-resend"
	if _, err := service.SendAgain(context.Background(), first.OutboxID, sharedKey); err != nil {
		t.Fatalf("SendAgain(first predecessor): %v", err)
	}
	if _, err := service.SendAgain(context.Background(), second.OutboxID, sharedKey); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("SendAgain(second predecessor, shared key) error = %v, want ErrIdempotencyConflict", err)
	}
}

// A soft-deleted predecessor message must not be resent verbatim (mirrors
// SendReaction's deleted-target refusal).
func TestSendAgainRejectsDeletedPredecessorMessage(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store, raw := openReconcileTestStore(t, clock.Now())
	sender := &scriptedTextSender{steps: []sendStep{{result: bridge.SendResult{}}}}
	registry := newScriptedRegistry("send-again-deleted-predecessor", sender)
	registry.setAvailable(true)
	service := newMessagingTestService(t, store, registry, clock)
	original := mustSendText(t, service, SendTextCommand{
		CommonCommand: testCommonCommand("key-send-again-deleted"),
		Body:          "soon to be deleted",
	})
	if processed, err := service.DispatchDue(context.Background(), 1); err != nil || processed != 1 {
		t.Fatalf("DispatchDue() = %d, %v; want 1, nil", processed, err)
	}
	mustReconcileExecOne(t, raw, `
		UPDATE messages SET state = 'deleted' WHERE message_id = ?
	`, original.LocalMessageID)

	if _, err := service.SendAgain(context.Background(), original.OutboxID, "key-send-again-deleted-resend"); !errors.Is(err, ErrInvalidCommand) {
		t.Fatalf("SendAgain(deleted predecessor) error = %v, want ErrInvalidCommand", err)
	}
}

func TestSendAgainRejectsInvalidNewIdempotencyKeys(t *testing.T) {
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	sender := &scriptedTextSender{steps: []sendStep{{result: bridge.SendResult{}}}}
	registry := newScriptedRegistry("send-again-key-guard", sender)
	registry.setAvailable(true)
	service := newMessagingTestService(t, store, registry, clock)
	const originalKey = "key-send-again-key-guard"
	original := mustSendText(t, service, SendTextCommand{
		CommonCommand: testCommonCommand(originalKey),
		Body:          "guard the new key",
	})
	if processed, err := service.DispatchDue(context.Background(), 1); err != nil || processed != 1 {
		t.Fatalf("DispatchDue() = %d, %v; want 1, nil", processed, err)
	}

	for _, newKey := range []string{"", "   ", originalKey} {
		if _, err := service.SendAgain(context.Background(), original.OutboxID, newKey); !errors.Is(err, ErrInvalidCommand) {
			t.Errorf("SendAgain(new key %q) error = %v, want ErrInvalidCommand", newKey, err)
		}
	}
}

func TestSendAgainStrictStateGuard(t *testing.T) {
	states := []OutboxState{
		OutboxConfirmed,
		OutboxQueued,
		OutboxDispatching,
		OutboxNotDispatched,
		OutboxCanceled,
		OutboxStoreFailed,
	}
	for _, state := range states {
		state := state
		t.Run(string(state), func(t *testing.T) {
			service, submission := sendAgainTextFixtureInState(t, state)
			if _, err := service.SendAgain(
				context.Background(),
				submission.OutboxID,
				"key-send-again-invalid-state-"+string(state),
			); !errors.Is(err, ErrInvalidState) {
				t.Fatalf("SendAgain(%s) error = %v, want ErrInvalidState", state, err)
			}
		})
	}
}

func TestSendAgainStrictKindGuard(t *testing.T) {
	t.Run("reaction uncertain", func(t *testing.T) {
		clock := newManualClock(messagingTestTime)
		store := openMessagingTestStore(t, clock.Now())
		target := seedSendAgainTarget(t, store, clock, "reaction")
		sender := &scriptedReactionSender{steps: []sendStep{{err: sendAgainUncertainError("reaction")}}}
		registry := newScriptedRegistry("send-again-reaction", nil)
		registry.setReactionSender(sender)
		registry.setAvailable(true)
		service := newMessagingTestService(t, store, registry, clock)
		submission := mustSendReaction(t, service, SendReactionCommand{
			CommonCommand:   testCommonCommand("key-send-again-reaction"),
			TargetMessageID: target,
			Emoji:           "👍",
		})
		if processed, err := service.DispatchDue(context.Background(), 1); err != nil || processed != 1 {
			t.Fatalf("DispatchDue() = %d, %v; want 1, nil", processed, err)
		}
		if got := mustOutboxItem(t, service, submission.OutboxID).State; got != sqlite.OutboxUncertain {
			t.Fatalf("reaction state = %q, want uncertain", got)
		}
		if _, err := service.SendAgain(context.Background(), submission.OutboxID, "new-reaction-key"); !errors.Is(err, ErrInvalidState) {
			t.Fatalf("SendAgain(reaction) error = %v, want ErrInvalidState", err)
		}
	})

	t.Run("read rejected", func(t *testing.T) {
		clock := newManualClock(messagingTestTime)
		store := openMessagingTestStore(t, clock.Now())
		target := seedSendAgainTarget(t, store, clock, "read")
		seedMessagingDevice(t, store, "send-again-device", "account-1", clock.Now())
		sender := &scriptedReadReceiptSender{steps: []readReceiptStep{{err: sendAgainRejectedError("read")}}}
		registry := newScriptedRegistry("send-again-read", nil)
		registry.setReadReceiptSender(sender)
		registry.setAvailable(true)
		service := newMessagingTestService(t, store, registry, clock)
		submission := mustMarkRead(t, service, MarkReadCommand{
			CommonCommand:     testCommonCommand("key-send-again-read"),
			DeviceID:          "send-again-device",
			LastReadMessageID: target,
		})
		if processed, err := service.DispatchDue(context.Background(), 1); err != nil || processed != 1 {
			t.Fatalf("DispatchDue() = %d, %v; want 1, nil", processed, err)
		}
		if got := mustOutboxItem(t, service, submission.OutboxID).State; got != sqlite.OutboxRejected {
			t.Fatalf("read state = %q, want rejected", got)
		}
		if _, err := service.SendAgain(context.Background(), submission.OutboxID, "new-read-key"); !errors.Is(err, ErrInvalidState) {
			t.Fatalf("SendAgain(read) error = %v, want ErrInvalidState", err)
		}
	})
}

func sendAgainStepForState(state OutboxState) sendStep {
	if state == OutboxRejected {
		return sendStep{err: sendAgainRejectedError("message")}
	}
	return sendStep{result: bridge.SendResult{}}
}

func sendAgainRejectedError(operation string) error {
	return bridge.OpError{
		Class:     bridge.FailureUnsupported,
		Operation: operation,
		Dispatch:  bridge.DispatchNotCalled,
		Cause:     errors.New("terminal send-again fixture failure"),
	}
}

func sendAgainUncertainError(operation string) error {
	return bridge.OpError{
		Class:     bridge.FailureTransient,
		Operation: operation,
		Dispatch:  bridge.DispatchUncertain,
		Cause:     context.DeadlineExceeded,
	}
}

func sendAgainTextFixtureInState(t *testing.T, state OutboxState) (*MessageService, Submission) {
	t.Helper()
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	var step sendStep
	switch state {
	case OutboxConfirmed:
		step.result = bridge.SendResult{RemoteMessageID: "remote-send-again-confirmed"}
	case OutboxNotDispatched:
		step.err = bridge.OpError{
			Class:     bridge.FailureTransient,
			Operation: "send_text",
			Dispatch:  bridge.DispatchNotCalled,
			Cause:     errors.New("safe pre-call failure"),
		}
	}
	sender := &scriptedTextSender{steps: []sendStep{step}}
	registry := newScriptedRegistry("send-again-state-guard", sender)
	registry.setAvailable(true)
	service := newMessagingTestService(t, store, registry, clock)
	command := SendTextCommand{
		CommonCommand: testCommonCommand("key-send-again-state-" + string(state)),
		Body:          "state guard",
	}
	if state == OutboxQueued || state == OutboxCanceled {
		command.NotBefore = clock.Now().Add(time.Hour)
	}
	submission := mustSendText(t, service, command)

	switch state {
	case OutboxQueued:
	case OutboxCanceled:
		if _, err := service.Cancel(context.Background(), submission.OutboxID); err != nil {
			t.Fatalf("Cancel(): %v", err)
		}
	case OutboxDispatching:
		leases, err := service.outbox.LeaseDue(context.Background(), sqlite.LeaseRequest{
			Owner:    "send-again-test",
			Now:      clock.Now(),
			Duration: time.Minute,
			Limit:    1,
		})
		if err != nil || len(leases) != 1 {
			t.Fatalf("LeaseDue() = %+v, %v; want one lease", leases, err)
		}
	case OutboxStoreFailed:
		leases, err := service.outbox.LeaseDue(context.Background(), sqlite.LeaseRequest{
			Owner:    "send-again-test",
			Now:      clock.Now(),
			Duration: time.Minute,
			Limit:    1,
		})
		if err != nil || len(leases) != 1 || leases[0].LeaseToken == nil {
			t.Fatalf("LeaseDue() = %+v, %v; want one tokened lease", leases, err)
		}
		lease := leases[0]
		if err := service.outbox.MarkTransportCalled(context.Background(), sqlite.Attempt{
			OutboxID:     submission.OutboxID,
			LeaseToken:   *lease.LeaseToken,
			AttemptToken: "send-again-store-failed-attempt",
			StartedAt:    clock.Now(),
		}); err != nil {
			t.Fatalf("MarkTransportCalled(): %v", err)
		}
		if err := service.outbox.MarkStoreFailed(
			context.Background(),
			submission.OutboxID,
			*lease.LeaseToken,
			"remote-send-again-store-failed",
			"local projection failed",
		); err != nil {
			t.Fatalf("MarkStoreFailed(): %v", err)
		}
	default:
		if processed, err := service.DispatchDue(context.Background(), 1); err != nil || processed != 1 {
			t.Fatalf("DispatchDue(%s) = %d, %v; want 1, nil", state, processed, err)
		}
	}
	if got := mustOutboxItem(t, service, submission.OutboxID).State; got != state {
		t.Fatalf("fixture state = %q, want %q", got, state)
	}
	return service, submission
}

func seedSendAgainTarget(t *testing.T, store *sqlite.Store, clock Clock, suffix string) string {
	t.Helper()
	messageID := "message-send-again-target-" + suffix
	return seedMessagingMessage(t, store, clock, sqlite.Message{
		MessageID:       messageID,
		ConversationID:  "conversation-1",
		AccountID:       "account-1",
		RemoteMessageID: "remote-send-again-target-" + suffix,
		Direction:       sqlite.MessageDirectionIncoming,
		Body:            "send-again target",
		State:           sqlite.MessageStateActive,
		OccurredAtMS:    clock.Now().UnixMilli(),
	})
}

func drainSendAgainWake(service *MessageService) {
	select {
	case <-service.wake:
	default:
	}
}
