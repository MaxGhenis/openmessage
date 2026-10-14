package ingest

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/messaging"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

// I9: when the phone files a direct thread under a new remote conversation
// ID, the dispatcher rebinds the local conversation before sending, so the
// phone's echo of the sent message lands in the same local conversation and
// reconciles onto the optimistic row instead of projecting a second message.
func TestGoogleEchoAfterConversationMovedRebindConvergesToOneMessage(t *testing.T) {
	const movedRemoteID = "google-remote-conversation-moved"
	harness := newGoogleEchoHarness(t, nil)
	sender := &movingGoogleEchoSender{clock: harness.clock, to: movedRemoteID}
	// A second dispatcher over the same store carries the scripted move; the
	// harness's worker still reconciles echoes through the shared outbox.
	dispatcher, err := messaging.NewMessageService(
		harness.store,
		&googleEchoRegistry{sender: sender},
		harness.blobs,
		harness.clock,
		&googleEchoIDs{prefix: "rebind"},
	)
	if err != nil {
		t.Fatalf("NewMessageService(dispatcher): %v", err)
	}
	ctx := context.Background()
	submission, err := dispatcher.SendText(ctx, messaging.SendTextCommand{
		CommonCommand: messaging.CommonCommand{
			AccountID:      googleEchoAccountID,
			ConversationID: googleEchoConversationID,
			IdempotencyKey: "google-echo-rebind",
		},
		Body: "sent after the thread moved",
	})
	if err != nil {
		t.Fatalf("SendText(): %v", err)
	}

	if processed, err := dispatcher.DispatchDue(ctx, 1); err != nil || processed != 1 {
		t.Fatalf("DispatchDue(moved) = %d, %v; want 1, nil", processed, err)
	}
	conversation, err := harness.store.GetConversation(googleEchoConversationID)
	if err != nil {
		t.Fatalf("GetConversation(): %v", err)
	}
	if conversation.RemoteConversationID != movedRemoteID {
		t.Fatalf("conversation binding = %q, want %q", conversation.RemoteConversationID, movedRemoteID)
	}
	if processed, err := dispatcher.DispatchDue(ctx, 1); err != nil || processed != 1 {
		t.Fatalf("DispatchDue(after rebind) = %d, %v; want 1, nil", processed, err)
	}
	delivery, err := dispatcher.Get(ctx, submission.OutboxID)
	if err != nil {
		t.Fatalf("Get(): %v", err)
	}
	requests := sender.snapshot()
	if delivery.State != messaging.OutboxConfirmed || len(requests) != 2 ||
		requests[0].Conversation.RemoteID != googleEchoRemoteConversationID ||
		requests[1].Conversation.RemoteID != movedRemoteID ||
		requests[0].RequestID != requests[1].RequestID {
		t.Fatalf("delivery %+v after requests %+v, want one send under the moved ID", delivery, requests)
	}
	requestID := requests[1].RequestID

	const permanentID = "google-permanent-after-move"
	echo := googleEchoMessage(
		permanentID,
		requestID,
		"sent after the thread moved",
		true,
		googleEchoNow.Add(time.Minute),
	)
	echo.ConversationID = movedRemoteID
	harness.appendMessage(t, echo)
	harness.waitFor(t, "echo under the moved conversation ID", func(snapshot CounterSnapshot) bool {
		return snapshot.EchoEnriched == 1 && snapshot.Projected == 1
	})

	harness.assertSinglePermanentMessage(t, submission.LocalMessageID, requestID, permanentID)
	if got := countSQLiteRows(t, harness.storePath,
		"SELECT COUNT(*) FROM conversations WHERE account_id = ?", googleEchoAccountID); got != 1 {
		t.Fatalf("conversations after echo = %d, want the one rebound conversation", got)
	}
	message, err := harness.messages.GetMessageByRemote(ctx, googleEchoAccountID, googleEchoConversationID, permanentID)
	if err != nil {
		t.Fatalf("GetMessageByRemote(): %v", err)
	}
	if message.Direction != sqlite.MessageDirectionOutgoing {
		t.Fatalf("reconciled message = %+v, want the outgoing optimistic row", message)
	}
}

// movingGoogleEchoSender refuses the first send as conversation_moved (as the
// Google adapter does after a by-number lookup resolves a different thread)
// and accepts every later one with a provisional request-ID result.
type movingGoogleEchoSender struct {
	clock *googleEchoClock
	to    string

	mu       sync.Mutex
	requests []bridge.TextRequest
}

func (s *movingGoogleEchoSender) SendText(
	_ context.Context,
	request bridge.TextRequest,
) (bridge.SendResult, error) {
	s.mu.Lock()
	s.requests = append(s.requests, request)
	first := len(s.requests) == 1
	s.mu.Unlock()
	if first {
		return bridge.SendResult{}, bridge.OpError{
			Class:       bridge.FailureTransient,
			Operation:   "send_text",
			Fingerprint: bridge.FingerprintConversationMoved,
			Dispatch:    bridge.DispatchNotCalled,
			Cause: &bridge.ConversationMovedError{
				FromRemoteID: request.Conversation.RemoteID,
				ToRemoteID:   s.to,
			},
		}
	}
	if request.Conversation.RemoteID != s.to {
		return bridge.SendResult{}, errors.New("sent under a stale conversation ID")
	}
	return bridge.SendResult{
		RemoteMessageID: request.RequestID,
		AcceptedAt:      s.clock.Now(),
		EchoExpected:    true,
	}, nil
}

func (s *movingGoogleEchoSender) snapshot() []bridge.TextRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]bridge.TextRequest(nil), s.requests...)
}
