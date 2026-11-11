package whatsapp

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"testing"
	"testing/quick"
	"time"

	"go.mau.fi/whatsmeow"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/whatsapplive"
)

func TestAdapterImplementsReadReceiptSender(t *testing.T) {
	var _ bridge.ReadReceiptSender = (*Adapter)(nil)
}

func TestNewWiresMarkReadRequestSeam(t *testing.T) {
	a := New("whatsapp-primary", &whatsapplive.Bridge{})
	if a.markReadRequest == nil {
		t.Fatal("New() did not wire the retained MarkReadRequest method")
	}
}

func TestMarkReadRequiresReadyTransportWithoutLifecycleAction(t *testing.T) {
	var connectCalls, transportCalls int
	a := &Adapter{
		accountID: "whatsapp-primary",
		connect: func(context.Context) error {
			connectCalls++
			return nil
		},
		mediaReady: func() bool { return false },
		markReadRequest: func(string, []string, string, time.Time) error {
			transportCalls++
			return nil
		},
	}

	err := a.MarkRead(context.Background(), bridge.ReadReceiptRequest{})
	failure, ok := asOpError(err)
	if !ok {
		t.Fatalf("MarkRead() error = %T %v, want bridge.OpError", err, err)
	}
	if failure.Class != bridge.FailureTransient ||
		failure.Dispatch != bridge.DispatchNotCalled ||
		failure.Operation != "mark_read" ||
		failure.Fingerprint != "whatsapp_not_connected" ||
		!errors.Is(failure.Cause, whatsmeow.ErrNotConnected) {
		t.Fatalf("MarkRead() failure = %+v, want classified not-connected pre-call failure", failure)
	}
	if connectCalls != 0 {
		t.Fatalf("connect calls = %d, want zero", connectCalls)
	}
	if transportCalls != 0 {
		t.Fatalf("MarkRead transport calls = %d, want zero", transportCalls)
	}
}

func TestMarkReadContextGuardsRunBeforeTransport(t *testing.T) {
	tests := []struct {
		name            string
		ctx             context.Context
		wantFingerprint string
		wantCause       error
	}{
		{
			name:            "nil context",
			ctx:             nil,
			wantFingerprint: "whatsapp_mark_read_context_missing",
		},
		{
			name:            "done context",
			ctx:             canceledTextSendContext(),
			wantFingerprint: "whatsapp_mark_read_context_done",
			wantCause:       context.Canceled,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var transportCalls int
			a := &Adapter{
				mediaReady: func() bool { return true },
				markReadRequest: func(string, []string, string, time.Time) error {
					transportCalls++
					return nil
				},
			}

			err := a.MarkRead(test.ctx, bridge.ReadReceiptRequest{
				Messages: []bridge.MessageRef{{RemoteID: "message-one"}},
			})
			failure, ok := asOpError(err)
			if !ok || failure.Class != bridge.FailureTransient ||
				failure.Dispatch != bridge.DispatchNotCalled ||
				failure.Operation != "mark_read" ||
				failure.Fingerprint != test.wantFingerprint {
				t.Fatalf("MarkRead() error = %v, want context pre-call failure %q", err, test.wantFingerprint)
			}
			if test.wantCause != nil && !errors.Is(failure.Cause, test.wantCause) {
				t.Fatalf("MarkRead() cause = %v, want %v", failure.Cause, test.wantCause)
			}
			if transportCalls != 0 {
				t.Fatalf("MarkRead transport calls = %d, want zero", transportCalls)
			}
		})
	}
}

func TestMarkReadRejectsEmptyMessagesBeforeTransport(t *testing.T) {
	var transportCalls int
	a := &Adapter{
		mediaReady: func() bool { return true },
		markReadRequest: func(string, []string, string, time.Time) error {
			transportCalls++
			return nil
		},
	}

	err := a.MarkRead(context.Background(), bridge.ReadReceiptRequest{})
	failure, ok := asOpError(err)
	if !ok || failure.Class != bridge.FailureTransient ||
		failure.Dispatch != bridge.DispatchNotCalled ||
		failure.Operation != "mark_read" ||
		failure.Fingerprint != "whatsapp_mark_read_no_messages" ||
		failure.Cause == nil {
		t.Fatalf("MarkRead(empty Messages) error = %v, want no-messages pre-call failure", err)
	}
	if transportCalls != 0 {
		t.Fatalf("MarkRead transport calls = %d, want zero", transportCalls)
	}
}

// readReceiptCall is one MarkReadRequest call the adapter made.
type readReceiptCall struct {
	conversationID string
	messageIDs     []string
	senderID       string
	readAt         time.Time
}

// recordingReadAdapter is a ready adapter whose MarkReadRequest records each
// call and answers it with the next error in errs (nil once they run out).
func recordingReadAdapter(calls *[]readReceiptCall, errs ...error) *Adapter {
	return &Adapter{
		accountID:  "whatsapp-primary",
		mediaReady: func() bool { return true },
		markReadRequest: func(conversationID string, messageIDs []string, senderID string, timestamp time.Time) error {
			*calls = append(*calls, readReceiptCall{
				conversationID: conversationID,
				messageIDs:     append([]string(nil), messageIDs...),
				senderID:       senderID,
				readAt:         timestamp,
			})
			if len(errs) == 0 {
				return nil
			}
			err := errs[0]
			errs = errs[1:]
			return err
		},
	}
}

func TestMarkReadSuccessMapsAllMessagesOfOneSender(t *testing.T) {
	readAt := time.Date(2026, time.July, 15, 14, 4, 5, 0, time.UTC)
	var calls []readReceiptCall
	err := recordingReadAdapter(&calls).MarkRead(context.Background(), bridge.ReadReceiptRequest{
		AccountID: "whatsapp-primary",
		Conversation: bridge.ConversationRef{
			RemoteID: "whatsapp:120363019999999999@g.us",
		},
		Messages: []bridge.MessageRef{
			{RemoteID: "message-one", AuthorID: "+15557654321"},
			{RemoteID: "message-two", AuthorID: "+15557654321"},
		},
		ReadAt: readAt,
	})
	if err != nil {
		t.Fatalf("MarkRead() error = %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("MarkRead transport calls = %d, want one", len(calls))
	}
	call := calls[0]
	if call.conversationID != "whatsapp:120363019999999999@g.us" {
		t.Fatalf("MarkReadRequest conversation = %q", call.conversationID)
	}
	if len(call.messageIDs) != 2 || call.messageIDs[0] != "message-one" || call.messageIDs[1] != "message-two" {
		t.Fatalf("MarkReadRequest message IDs = %q, want both request IDs in order", call.messageIDs)
	}
	if call.senderID != "+15557654321" {
		t.Fatalf("MarkReadRequest sender = %q, want the messages' author", call.senderID)
	}
	if !call.readAt.Equal(readAt) {
		t.Fatalf("MarkReadRequest timestamp = %s, want %s", call.readAt, readAt)
	}
}

// whatsmeow's MarkRead names one sender for every message a receipt covers,
// so a request for messages from different senders sends one receipt per
// sender. Before, one receipt named the first message's author for all of
// them.
func TestMarkReadSendsOneReceiptPerSender(t *testing.T) {
	readAt := time.Date(2026, time.July, 15, 14, 4, 5, 0, time.UTC)
	var calls []readReceiptCall
	err := recordingReadAdapter(&calls).MarkRead(context.Background(), bridge.ReadReceiptRequest{
		Conversation: bridge.ConversationRef{RemoteID: "whatsapp:120363019999999999@g.us"},
		Messages: []bridge.MessageRef{
			{RemoteID: "message-one", AuthorID: "+15557654321"},
			{RemoteID: "message-two", AuthorID: "98765432109876@lid"},
			{RemoteID: "message-three", AuthorID: "+15557654321"},
			{RemoteID: "message-four", AuthorID: ""},
		},
		ReadAt: readAt,
	})
	if err != nil {
		t.Fatalf("MarkRead() error = %v", err)
	}
	want := []readReceiptCall{
		{senderID: "+15557654321", messageIDs: []string{"message-one", "message-three"}},
		{senderID: "98765432109876@lid", messageIDs: []string{"message-two"}},
		{senderID: "", messageIDs: []string{"message-four"}},
	}
	if len(calls) != len(want) {
		t.Fatalf("MarkRead transport calls = %+v, want %d", calls, len(want))
	}
	for i, call := range calls {
		if call.senderID != want[i].senderID || strings.Join(call.messageIDs, ",") != strings.Join(want[i].messageIDs, ",") {
			t.Fatalf("receipt %d = %q for %q, want %q for %q", i, call.senderID, call.messageIDs, want[i].senderID, want[i].messageIDs)
		}
		if call.conversationID != "whatsapp:120363019999999999@g.us" || !call.readAt.Equal(readAt) {
			t.Fatalf("receipt %d = %+v, want the request's conversation and read time", i, call)
		}
	}
}

// A failed receipt ends the request and leaves it retryable; the retry sends
// the receipts before it again, which is harmless.
func TestMarkReadStopsAtTheFirstFailedReceipt(t *testing.T) {
	cause := errors.New("read receipt acknowledgement lost")
	var calls []readReceiptCall
	err := recordingReadAdapter(&calls, nil, cause).MarkRead(context.Background(), bridge.ReadReceiptRequest{
		Messages: []bridge.MessageRef{
			{RemoteID: "message-one", AuthorID: "+15557654321"},
			{RemoteID: "message-two", AuthorID: "+15550000000"},
			{RemoteID: "message-three", AuthorID: "+15551111111"},
		},
	})
	failure, ok := asOpError(err)
	if !ok || failure.Class != bridge.FailureTransient ||
		failure.Dispatch != bridge.DispatchNotCalled ||
		failure.Fingerprint != "whatsapp_mark_read_failed" ||
		!errors.Is(failure.Cause, cause) {
		t.Fatalf("MarkRead() error = %v, want a retryable not-dispatched read failure", err)
	}
	if len(calls) != 2 {
		t.Fatalf("MarkRead transport calls = %d, want two (none after the failure)", len(calls))
	}
}

// A group receipt whose stored sender is neither a user's JID nor a phone
// number is refused by the retained core before anything is sent, and
// retrying cannot name it either, so the adapter reports it as unsupported:
// the dispatcher rejects the row and keeps the read cursor.
func TestMarkReadRefusesAGroupReceiptWhoseSenderIsNotAJIDOrNumber(t *testing.T) {
	for _, test := range []struct {
		name    string
		request func(string, []string, string, time.Time) error
	}{
		{
			name: "retained core",
			// The refusal comes before the transport is needed.
			request: (&whatsapplive.Bridge{}).MarkReadRequest,
		},
		{
			name: "wrapped refusal",
			request: func(string, []string, string, time.Time) error {
				return fmt.Errorf("mark read: %w", whatsapplive.ErrReadReceiptSenderUnknown)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			a := &Adapter{mediaReady: func() bool { return true }, markReadRequest: test.request}
			err := a.MarkRead(context.Background(), bridge.ReadReceiptRequest{
				Conversation: bridge.ConversationRef{RemoteID: "whatsapp:120363019999999999@g.us"},
				Messages:     []bridge.MessageRef{{RemoteID: "message-one", AuthorID: "Me"}},
			})
			failure, ok := asOpError(err)
			if !ok || failure.Class != bridge.FailureUnsupported ||
				failure.Dispatch != bridge.DispatchNotCalled ||
				failure.Operation != "mark_read" ||
				failure.Fingerprint != "whatsapp_read_receipt_sender_invalid" ||
				!errors.Is(failure.Cause, whatsapplive.ErrReadReceiptSenderUnknown) {
				t.Fatalf("MarkRead() error = %v, want the unsupported unparseable-sender refusal", err)
			}
		})
	}
}

// A refused sender does not hold up the others: their receipts are still
// sent, and the refusal is reported (as unsupported) once they are, whichever
// batch it came from.
func TestMarkReadSendsTheOtherSendersReceiptsPastARefusedOne(t *testing.T) {
	refusal := fmt.Errorf("mark read: %w", whatsapplive.ErrReadReceiptSenderUnknown)
	for refusedAt := 0; refusedAt < 3; refusedAt++ {
		t.Run(strconv.Itoa(refusedAt), func(t *testing.T) {
			errs := make([]error, 3)
			errs[refusedAt] = refusal
			var calls []readReceiptCall
			err := recordingReadAdapter(&calls, errs...).MarkRead(context.Background(), bridge.ReadReceiptRequest{
				Conversation: bridge.ConversationRef{RemoteID: "whatsapp:120363019999999999@g.us"},
				Messages: []bridge.MessageRef{
					{RemoteID: "message-one", AuthorID: "+15557654321"},
					{RemoteID: "message-two", AuthorID: "+15550000000"},
					{RemoteID: "message-three", AuthorID: "+15551111111"},
				},
			})
			if len(calls) != 3 {
				t.Fatalf("MarkRead transport calls = %d, want all three senders", len(calls))
			}
			failure, ok := asOpError(err)
			if !ok || failure.Class != bridge.FailureUnsupported ||
				failure.Dispatch != bridge.DispatchNotCalled ||
				failure.Fingerprint != "whatsapp_read_receipt_sender_invalid" ||
				!errors.Is(failure.Cause, whatsapplive.ErrReadReceiptSenderUnknown) {
				t.Fatalf("MarkRead() error = %v, want the unsupported refusal", err)
			}
		})
	}
}

// Once the request's context ends (callTransport abandons a call that
// outlives its bound), no further receipt is sent.
func TestMarkReadSendsNoFurtherReceiptAfterItsContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls int
	a := &Adapter{
		mediaReady: func() bool { return true },
		markReadRequest: func(string, []string, string, time.Time) error {
			calls++
			cancel()
			return nil
		},
	}
	err := a.MarkRead(ctx, bridge.ReadReceiptRequest{
		Messages: []bridge.MessageRef{
			{RemoteID: "message-one", AuthorID: "+15557654321"},
			{RemoteID: "message-two", AuthorID: "+15550000000"},
		},
	})
	if calls != 1 {
		t.Fatalf("MarkRead transport calls = %d, want one (none after the context ended)", calls)
	}
	failure, ok := asOpError(err)
	if !ok || failure.Fingerprint != "whatsapp_mark_read_context_done" ||
		failure.Dispatch != bridge.DispatchNotCalled || !errors.Is(failure.Cause, context.Canceled) {
		t.Fatalf("MarkRead() error = %v, want the context-done failure", err)
	}
}

// TestReadReceiptBatchesProperty checks readReceiptBatches over random
// requests: each batch has one sender and no two share one, batches follow the
// order each sender first appears, and together they hold every message ID
// exactly once, in request order within a sender.
func TestReadReceiptBatchesProperty(t *testing.T) {
	senders := []string{"", "+15557654321", "98765432109876@lid", "+15550000000"}
	property := func(picks []uint8) bool {
		messages := make([]bridge.MessageRef, len(picks))
		for i, pick := range picks {
			messages[i] = bridge.MessageRef{RemoteID: "message-" + strconv.Itoa(i), AuthorID: senders[int(pick)%len(senders)]}
		}
		batches := readReceiptBatches(messages)

		var firstSeen []string
		seen := map[string]bool{}
		wantIDs := map[string][]string{}
		for _, message := range messages {
			if !seen[message.AuthorID] {
				seen[message.AuthorID] = true
				firstSeen = append(firstSeen, message.AuthorID)
			}
			wantIDs[message.AuthorID] = append(wantIDs[message.AuthorID], message.RemoteID)
		}
		if len(batches) != len(firstSeen) {
			return false
		}
		total := 0
		for i, batch := range batches {
			if batch.authorID != firstSeen[i] || strings.Join(batch.messageIDs, ",") != strings.Join(wantIDs[batch.authorID], ",") {
				return false
			}
			total += len(batch.messageIDs)
		}
		return total == len(messages)
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 2000, Rand: rand.New(rand.NewSource(20261021))}); err != nil {
		t.Fatal(err)
	}
}

func TestMarkReadTransientTransportFailuresAreAlwaysRetryable(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{name: "plain transport error", err: errors.New("read receipt acknowledgement lost")},
		{name: "disconnect at transport boundary", err: fmt.Errorf("mark WhatsApp messages read: %w", whatsmeow.ErrNotConnected)},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			a := &Adapter{
				mediaReady: func() bool { return true },
				markReadRequest: func(string, []string, string, time.Time) error {
					return test.err
				},
			}

			err := a.MarkRead(context.Background(), bridge.ReadReceiptRequest{
				Messages: []bridge.MessageRef{{RemoteID: "message-one"}},
			})
			failure, ok := asOpError(err)
			if !ok || failure.Class != bridge.FailureTransient ||
				failure.Dispatch != bridge.DispatchNotCalled ||
				failure.Dispatch == bridge.DispatchUncertain ||
				failure.Operation != "mark_read" ||
				failure.Fingerprint != "whatsapp_mark_read_failed" ||
				!errors.Is(failure.Cause, test.err) {
				t.Fatalf("MarkRead() error = %v, want retryable not-dispatched read failure", err)
			}
		})
	}
}

func TestMarkReadPreservesAuthenticationClassification(t *testing.T) {
	want := whatsmeow.ErrNotLoggedIn
	a := &Adapter{
		mediaReady: func() bool { return true },
		markReadRequest: func(string, []string, string, time.Time) error {
			return want
		},
	}

	err := a.MarkRead(context.Background(), bridge.ReadReceiptRequest{
		Messages: []bridge.MessageRef{{RemoteID: "message-one"}},
	})
	failure, ok := asOpError(err)
	if !ok || failure.Class != bridge.FailureReauthRequired ||
		failure.Operation != "mark_read" ||
		failure.Fingerprint != "whatsapp_session_invalid" ||
		!errors.Is(failure.Cause, want) {
		t.Fatalf("MarkRead() error = %v, want WhatsApp reauth classification", err)
	}
}

// The adapter shim must not forward either retryable read failures or terminal
// authentication failures into the receive lifecycle. The retained core owns
// its guarded connection-error report at the actual transport boundary.
func TestMarkReadFailureDoesNotRetireReceiveGeneration(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		wantClass bridge.FailureClass
	}{
		{
			name:      "transient transport failure",
			err:       errors.New("read receipt failed"),
			wantClass: bridge.FailureTransient,
		},
		{
			name:      "authentication failure",
			err:       whatsmeow.ErrNotLoggedIn,
			wantClass: bridge.FailureReauthRequired,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ready := make(chan struct{})
			close(ready)
			r := &run{
				ready:     ready,
				finish:    make(chan error, 1),
				admitting: true,
			}
			a := &Adapter{
				current:    r,
				mediaReady: func() bool { return true },
				markReadRequest: func(string, []string, string, time.Time) error {
					return test.err
				},
			}

			err := a.MarkRead(context.Background(), bridge.ReadReceiptRequest{
				Messages: []bridge.MessageRef{{RemoteID: "message-one"}},
			})
			failure, ok := asOpError(err)
			if !ok || failure.Class != test.wantClass || !errors.Is(failure.Cause, test.err) {
				t.Fatalf("MarkRead() error = %v, want class %q with transport cause", err, test.wantClass)
			}
			select {
			case reported := <-r.finish:
				t.Fatalf("MarkRead() retired the receive generation with %v", reported)
			default:
			}
		})
	}
}
