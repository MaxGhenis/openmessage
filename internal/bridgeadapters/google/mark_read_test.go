package google

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/client"
)

var _ bridge.ReadReceiptSender = (*Adapter)(nil)

func TestMarkReadNotConnectedDoesNotReachClientOrTransport(t *testing.T) {
	host := newTestApp(t)
	legacy := newLegacyClient(t)
	generation := host.BeginGoogleGeneration(legacy)
	t.Cleanup(generation.Release)
	fake := &fakeReadSendClient{}
	seam := installReadSendClient(t, legacy, fake)
	a := New("google-primary", host, func() bool { return true })
	a.newClient = func() (*client.Client, transportClient, error) {
		t.Fatal("MarkRead must not construct a Google transport")
		return nil, nil, nil
	}

	err := a.MarkRead(context.Background(), bridge.ReadReceiptRequest{
		AccountID: "google-primary",
		Conversation: bridge.ConversationRef{
			RemoteID: "conversation-id",
		},
		Messages: []bridge.MessageRef{{RemoteID: "message-id"}},
	})
	failure := requireMarkReadOpError(t, err)
	if failure.Class != bridge.FailureTransient ||
		failure.Dispatch != bridge.DispatchNotCalled ||
		failure.Operation != "mark_read" ||
		failure.Fingerprint != "google_not_connected" {
		t.Fatalf("failure = %+v, want transient pre-call google_not_connected", failure)
	}
	if seam.calls != 0 || fake.calls != 0 {
		t.Fatalf("seam/transport calls = (%d, %d), want (0, 0)", seam.calls, fake.calls)
	}
}

func TestMarkReadContextGuardsDoNotReachTransport(t *testing.T) {
	doneCtx, cancel := context.WithCancel(context.Background())
	cancel()
	tests := []struct {
		name string
		ctx  context.Context
	}{
		{name: "nil", ctx: nil},
		{name: "done", ctx: doneCtx},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := &fakeReadSendClient{}
			a := newReadSendTestAdapter(t, fake)

			err := a.MarkRead(test.ctx, bridge.ReadReceiptRequest{
				Conversation: bridge.ConversationRef{RemoteID: "conversation-id"},
				Messages:     []bridge.MessageRef{{RemoteID: "message-id"}},
			})
			failure := requireMarkReadOpError(t, err)
			if failure.Class != bridge.FailureTransient ||
				failure.Dispatch != bridge.DispatchNotCalled ||
				failure.Operation != "mark_read" {
				t.Fatalf("failure = %+v, want transient pre-call mark_read", failure)
			}
			if fake.calls != 0 {
				t.Fatalf("MarkRead transport calls = %d, want 0", fake.calls)
			}
		})
	}
}

func TestMarkReadEmptyMessagesIsClassifiedPreCall(t *testing.T) {
	fake := &fakeReadSendClient{}
	a := newReadSendTestAdapter(t, fake)

	err := a.MarkRead(context.Background(), bridge.ReadReceiptRequest{
		Conversation: bridge.ConversationRef{RemoteID: "conversation-id"},
	})
	failure := requireMarkReadOpError(t, err)
	if failure.Class != bridge.FailureTransient ||
		failure.Dispatch != bridge.DispatchNotCalled ||
		failure.Operation != "mark_read" {
		t.Fatalf("failure = %+v, want transient pre-call mark_read", failure)
	}
	if fake.calls != 0 {
		t.Fatalf("MarkRead transport calls = %d, want 0", fake.calls)
	}
}

func TestMarkReadSuccessUsesConversationAndLastMessageAsCursor(t *testing.T) {
	fake := &fakeReadSendClient{}
	a := newReadSendTestAdapter(t, fake)

	err := a.MarkRead(context.Background(), bridge.ReadReceiptRequest{
		AccountID: "google-primary",
		Conversation: bridge.ConversationRef{
			RemoteID: "remote-conversation-id",
		},
		Messages: []bridge.MessageRef{
			{RemoteID: "older-message-id", AuthorID: "older-author"},
			{RemoteID: "read-cursor-message-id", AuthorID: "cursor-author"},
		},
		ReadAt: time.Date(2026, time.July, 15, 13, 14, 15, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("MarkRead() error = %v", err)
	}
	if fake.calls != 1 {
		t.Fatalf("MarkRead transport calls = %d, want 1", fake.calls)
	}
	if fake.conversationID != "remote-conversation-id" ||
		fake.messageID != "read-cursor-message-id" {
		t.Fatalf(
			"MarkRead transport args = (%q, %q), want (remote-conversation-id, read-cursor-message-id)",
			fake.conversationID,
			fake.messageID,
		)
	}
}

func TestMarkReadTransientTransportFailureIsRetryableNotUncertain(t *testing.T) {
	fake := &fakeReadSendClient{err: errors.New("transport timeout")}
	a := newReadSendTestAdapter(t, fake)

	err := a.MarkRead(context.Background(), bridge.ReadReceiptRequest{
		Conversation: bridge.ConversationRef{RemoteID: "conversation-id"},
		Messages:     []bridge.MessageRef{{RemoteID: "message-id"}},
	})
	failure := requireMarkReadOpError(t, err)
	if failure.Class != bridge.FailureTransient ||
		failure.Operation != "mark_read" ||
		failure.Fingerprint != "google_mark_read_failed" ||
		failure.Dispatch != bridge.DispatchNotCalled {
		t.Fatalf(
			"failure = %+v, want transient google_mark_read_failed with DispatchNotCalled (never uncertain)",
			failure,
		)
	}
	if failure.Dispatch == bridge.DispatchUncertain {
		t.Fatalf("failure dispatch = %q, idempotent read receipt must never be uncertain", failure.Dispatch)
	}
	if fake.calls != 1 {
		t.Fatalf("MarkRead transport calls = %d, want 1", fake.calls)
	}
}

func TestMarkReadTransientFailureDoesNotRetireGeneration(t *testing.T) {
	host := newTestApp(t)
	lifecycleTransport := &fakeTransport{}
	a := newTestAdapter(t, host, lifecycleTransport)
	run, err := a.Start(context.Background(), bridge.StartRequest{
		AccountID:  "google-primary",
		Generation: 1,
	}, nil)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { stopRun(t, run) })
	lifecycleTransport.emit(&gmproto.Conversation{ConversationID: "ready"})
	<-run.Ready()

	fake := &fakeReadSendClient{err: errors.New("transport timeout")}
	installReadSendClient(t, host.GetClient(), fake)
	err = a.MarkRead(context.Background(), bridge.ReadReceiptRequest{
		Conversation: bridge.ConversationRef{RemoteID: "conversation-id"},
		Messages:     []bridge.MessageRef{{RemoteID: "message-id"}},
	})
	failure := requireMarkReadOpError(t, err)
	if failure.Class != bridge.FailureTransient ||
		failure.Operation != "mark_read" ||
		failure.Fingerprint != "google_mark_read_failed" ||
		failure.Dispatch != bridge.DispatchNotCalled {
		t.Fatalf("failure = %+v, want retryable google_mark_read_failed", failure)
	}
	if !host.Connected.Load() {
		t.Fatal("transient read failure marked the connected receive generation lost")
	}
	select {
	case terminal := <-run.Done():
		t.Fatalf("transient read failure retired the receive generation with %v", terminal)
	default:
	}
	if host.GoogleStatus().NeedsRepair {
		t.Fatal("transient read failure parked the Google lifecycle")
	}
}

func TestMarkReadAuthExpiryReportsToLifecycleAndRemainsRetryable(t *testing.T) {
	host := newTestApp(t)
	lifecycleTransport := &fakeTransport{}
	a := newTestAdapter(t, host, lifecycleTransport)
	run, err := a.Start(context.Background(), bridge.StartRequest{
		AccountID:  "google-primary",
		Generation: 1,
	}, nil)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	lifecycleTransport.emit(&gmproto.Conversation{ConversationID: "ready"})
	<-run.Ready()

	fake := &fakeReadSendClient{err: errors.New("google returned HTTP 401 for MarkRead")}
	installReadSendClient(t, host.GetClient(), fake)
	err = a.MarkRead(context.Background(), bridge.ReadReceiptRequest{
		Conversation: bridge.ConversationRef{RemoteID: "conversation-id"},
		Messages:     []bridge.MessageRef{{RemoteID: "message-id"}},
	})
	failure := requireMarkReadOpError(t, err)
	if failure.Class != bridge.FailureCredentialsExpired ||
		failure.Fingerprint != "google_auth_expired" ||
		failure.Dispatch != bridge.DispatchNotCalled {
		t.Fatalf(
			"failure = %+v, want credentials-expired google_auth_expired retaining DispatchNotCalled",
			failure,
		)
	}

	select {
	case terminal := <-run.Done():
		terminalFailure, ok := asOpError(terminal)
		if !ok || (terminalFailure.Class != bridge.FailureCredentialsExpired &&
			terminalFailure.Class != bridge.FailureUpgradeRequired) {
			t.Fatalf("terminal = %v, want credentials_expired/upgrade_required", terminal)
		}
		if terminalFailure.Dispatch != bridge.DispatchNotCalled {
			t.Fatalf(
				"terminal dispatch = %q, want retained DispatchNotCalled",
				terminalFailure.Dispatch,
			)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("auth-expired read failure did not reach the lifecycle owner")
	}
}

func newReadSendTestAdapter(t *testing.T, fake *fakeReadSendClient) *Adapter {
	t.Helper()
	host := newTestApp(t)
	legacy := newLegacyClient(t)
	generation := host.BeginGoogleGeneration(legacy)
	t.Cleanup(generation.Release)
	host.Connected.Store(true)
	installReadSendClient(t, legacy, fake)
	return New("google-primary", host, func() bool { return true })
}

type readSendClientSeamProbe struct {
	calls int
}

func installReadSendClient(
	t *testing.T,
	want *client.Client,
	fake readSendClient,
) *readSendClientSeamProbe {
	t.Helper()
	probe := &readSendClientSeamProbe{}
	previous := readSendClientFor
	readSendClientFor = func(got *client.Client) readSendClient {
		probe.calls++
		if got != want {
			t.Fatalf("read client source = %p, want App.GetClient() %p", got, want)
		}
		return fake
	}
	t.Cleanup(func() { readSendClientFor = previous })
	return probe
}

func requireMarkReadOpError(t *testing.T, err error) bridge.OpError {
	t.Helper()
	if err == nil {
		t.Fatal("MarkRead() error = nil, want classified failure")
	}
	failure, ok := asOpError(err)
	if !ok {
		t.Fatalf("MarkRead() error = %T %v, want bridge.OpError", err, err)
	}
	return failure
}

type fakeReadSendClient struct {
	err error

	calls          int
	conversationID string
	messageID      string
}

func (f *fakeReadSendClient) MarkRead(conversationID, messageID string) error {
	f.calls++
	f.conversationID = conversationID
	f.messageID = messageID
	return f.err
}
