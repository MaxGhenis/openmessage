package whatsapp

import (
	"context"
	"errors"
	"fmt"
	"testing"
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

func TestMarkReadSuccessMapsAllMessagesAndFirstAuthor(t *testing.T) {
	readAt := time.Date(2026, time.July, 15, 14, 4, 5, 0, time.UTC)
	var (
		gotConversationID string
		gotMessageIDs     []string
		gotSenderID       string
		gotReadAt         time.Time
		transportCalls    int
	)
	a := &Adapter{
		accountID:  "whatsapp-primary",
		mediaReady: func() bool { return true },
		markReadRequest: func(conversationID string, messageIDs []string, senderID string, timestamp time.Time) error {
			transportCalls++
			gotConversationID = conversationID
			gotMessageIDs = append([]string(nil), messageIDs...)
			gotSenderID = senderID
			gotReadAt = timestamp
			return nil
		},
	}

	err := a.MarkRead(context.Background(), bridge.ReadReceiptRequest{
		AccountID: "whatsapp-primary",
		Conversation: bridge.ConversationRef{
			RemoteID: "whatsapp:120363019999999999@g.us",
		},
		Messages: []bridge.MessageRef{
			{RemoteID: "message-one", AuthorID: "+15557654321"},
			{RemoteID: "message-two", AuthorID: "+15550000000"},
		},
		ReadAt: readAt,
	})
	if err != nil {
		t.Fatalf("MarkRead() error = %v", err)
	}
	if transportCalls != 1 {
		t.Fatalf("MarkRead transport calls = %d, want one", transportCalls)
	}
	if gotConversationID != "whatsapp:120363019999999999@g.us" {
		t.Fatalf("MarkReadRequest conversation = %q", gotConversationID)
	}
	if len(gotMessageIDs) != 2 || gotMessageIDs[0] != "message-one" || gotMessageIDs[1] != "message-two" {
		t.Fatalf("MarkReadRequest message IDs = %q, want both request IDs in order", gotMessageIDs)
	}
	if gotSenderID != "+15557654321" {
		t.Fatalf("MarkReadRequest sender = %q, want first message author", gotSenderID)
	}
	if !gotReadAt.Equal(readAt) {
		t.Fatalf("MarkReadRequest timestamp = %s, want %s", gotReadAt, readAt)
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
