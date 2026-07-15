package signal

import (
	"bytes"
	"context"
	"errors"
	"io"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/signallive"
)

const signalAdapterTestTimeout = 2 * time.Second

var signalAdapterTestEpoch = time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)

func TestAdapterImplementsTextSender(t *testing.T) {
	var _ bridge.TextSender = (*Adapter)(nil)
}

func TestAdapterImplementsReactionSender(t *testing.T) {
	var _ bridge.ReactionSender = (*Adapter)(nil)
}

func TestAdapterDoesNotImplementReadReceiptSender(t *testing.T) {
	var adapter bridge.Adapter = (*Adapter)(nil)
	if _, ok := adapter.(bridge.ReadReceiptSender); ok {
		t.Fatal("Signal adapter unexpectedly implements bridge.ReadReceiptSender")
	}
}

func TestRegistryKeepsSignalReadReceiptsUndeclared(t *testing.T) {
	const accountID = "signal-primary"

	registry := bridge.NewRegistry()
	if err := registry.Register(New(accountID, nil)); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	if _, ok := registry.Snapshot(accountID); !ok {
		t.Fatal("registered Signal account was not discovered")
	}
	if got := registry.Capabilities(accountID).ReadReceipts; got {
		t.Fatal("Signal ReadReceipts capability = true, want false")
	}
}

func TestSendTextRequiresReadyRetainedBridge(t *testing.T) {
	tests := []struct {
		name   string
		poller poller
	}{
		{name: "nil retained bridge"},
		{name: "not connected", poller: newFakePoller()},
		{
			name: "connected without account",
			poller: &fakePoller{
				started: make(chan *fakePollerRun, 1),
				status:  signallive.StatusSnapshot{Connected: true},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			adapter := &Adapter{accountID: "signal-primary", poller: tc.poller}
			result, err := adapter.SendText(context.Background(), bridge.TextRequest{
				AccountID:    "signal-primary",
				Conversation: bridge.ConversationRef{RemoteID: "signal:+15551234567"},
				Body:         "hello",
			})
			if result != (bridge.SendResult{}) {
				t.Fatalf("SendText() result = %+v, want zero", result)
			}
			var operationError bridge.OpError
			if !errors.As(err, &operationError) {
				t.Fatalf("SendText() error = %v (%T), want bridge.OpError", err, err)
			}
			if operationError.Class != bridge.FailureTransient ||
				operationError.Dispatch != bridge.DispatchNotCalled ||
				operationError.Operation != "send_text" ||
				operationError.Fingerprint != "signal_not_connected" {
				t.Fatalf("SendText() OpError = %+v, want classified not-connected pre-call failure", operationError)
			}
			if fake, ok := tc.poller.(*fakePoller); ok && fake.textCallCount() != 0 {
				t.Fatalf("SendTextRequest calls = %d, want 0 while not ready", fake.textCallCount())
			}
		})
	}
}

func TestSendTextRejectsInvalidContextBeforeTransport(t *testing.T) {
	tests := []struct {
		name            string
		context         func() context.Context
		wantFingerprint string
	}{
		{
			name:            "nil context",
			context:         func() context.Context { return nil },
			wantFingerprint: "signal_send_text_context_missing",
		},
		{
			name: "done context",
			context: func() context.Context {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx
			},
			wantFingerprint: "signal_send_text_context_done",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			poller := newFakePoller()
			poller.status = signallive.StatusSnapshot{Connected: true, Account: "+15551230000"}
			adapter := &Adapter{accountID: "signal-primary", poller: poller}

			_, err := adapter.SendText(tc.context(), bridge.TextRequest{Body: "hello"})
			var operationError bridge.OpError
			if !errors.As(err, &operationError) ||
				operationError.Class != bridge.FailureTransient ||
				operationError.Dispatch != bridge.DispatchNotCalled ||
				operationError.Operation != "send_text" ||
				operationError.Fingerprint != tc.wantFingerprint {
				t.Fatalf("SendText() error = %v, want pre-call context failure %q", err, tc.wantFingerprint)
			}
			if got := poller.textCallCount(); got != 0 {
				t.Fatalf("SendTextRequest calls = %d, want 0 after context failure", got)
			}
		})
	}
}

func TestSendTextMapsRetainedTimestampAndRequest(t *testing.T) {
	poller := newFakePoller()
	poller.status = signallive.StatusSnapshot{
		Connected: true,
		Paired:    true,
		Account:   "+15551230000",
	}
	poller.textTimestamp = 1700000000123
	adapter := &Adapter{accountID: "signal-primary", poller: poller}

	result, err := adapter.SendText(context.Background(), bridge.TextRequest{
		AccountID:    "signal-primary",
		Conversation: bridge.ConversationRef{RemoteID: "signal:+15551234567"},
		Body:         "hello from durable Signal",
		ReplyTo:      &bridge.MessageRef{RemoteID: "signal:1700000000000"},
		RequestID:    "local-dedupe-only",
	})
	if err != nil {
		t.Fatalf("SendText() error = %v", err)
	}
	if result.RemoteMessageID != strconv.FormatInt(poller.textTimestamp, 10) ||
		result.EchoExpected || !result.AcceptedAt.IsZero() {
		t.Fatalf("SendText() result = %+v, want canonical timestamp ID without echo", result)
	}
	request := poller.lastTextRequest()
	if request.conversationID != "signal:+15551234567" ||
		request.body != "hello from durable Signal" ||
		request.replyToID != "signal:1700000000000" {
		t.Fatalf("retained SendTextRequest = %+v, want mapped TextRequest", request)
	}
}

func TestSendTextClassifiesPreCallAndCommandFailures(t *testing.T) {
	tests := []struct {
		name         string
		err          error
		wantDispatch bridge.DispatchCertainty
	}{
		{
			name:         "local validation",
			err:          errors.New("Signal message body is required"),
			wantDispatch: bridge.DispatchNotCalled,
		},
		{
			name: "signal-cli command",
			err:  signallive.NewCommandError("send Signal message: recipient is not registered"),
		},
		{
			name: "structured all-recipient failure",
			err: &fakeSignalSendNotDispatchedError{err: signallive.NewCommandError(
				"send Signal message: every recipient failed",
			)},
			wantDispatch: bridge.DispatchNotCalled,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			poller := newFakePoller()
			poller.status = signallive.StatusSnapshot{
				Connected: true,
				Paired:    true,
				Account:   "+15551230000",
			}
			poller.textErr = tc.err
			adapter := &Adapter{accountID: "signal-primary", poller: poller}

			_, err := adapter.SendText(context.Background(), bridge.TextRequest{
				AccountID:    "signal-primary",
				Conversation: bridge.ConversationRef{RemoteID: "signal:+15551234567"},
				Body:         "hello",
			})
			var operationError bridge.OpError
			if !errors.As(err, &operationError) {
				t.Fatalf("SendText() error = %v (%T), want bridge.OpError", err, err)
			}
			if operationError.Class != bridge.FailureTransient ||
				operationError.Dispatch != tc.wantDispatch ||
				operationError.Operation != "send_text" ||
				operationError.Fingerprint != "signal_send_text_failed" ||
				!errors.Is(operationError.Cause, tc.err) {
				t.Fatalf("SendText() OpError = %+v, want transient classified failure with dispatch %q", operationError, tc.wantDispatch)
			}
			if got := poller.appliedCount(); got != 0 {
				t.Fatalf("recipient/local send failure applied %d lifecycle transitions, want 0", got)
			}
		})
	}
}

// A text send can fail on a recipient, an identity change, or the network;
// none indict the local account or widen ReportError's C6 matcher.
func TestSendTextDoesNotWidenGuardedLifecycleReporting(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{
			name: "unregistered recipient",
			err:  signallive.NewCommandError("send Signal message: user +15551234567 is not registered"),
		},
		{
			name: "untrusted identity",
			err:  signallive.NewCommandError("send Signal message: Untrusted identity key for +15551234567"),
		},
		{
			name: "network blip",
			err:  signallive.NewCommandError("send Signal message: Connection reset by peer"),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			poller := newFakePoller()
			poller.status = signallive.StatusSnapshot{
				Connected: true,
				Paired:    true,
				Account:   "+15551230000",
			}
			poller.textErr = tc.err
			adapter := &Adapter{accountID: "signal-primary", poller: poller}
			run, err := adapter.Start(context.Background(), bridge.StartRequest{
				AccountID:  "signal-primary",
				Generation: 1,
			}, nil)
			if err != nil {
				t.Fatalf("Start() error = %v", err)
			}
			receiveValue(t, poller.started, "retained poller start")
			t.Cleanup(func() { stopAdapterRun(t, run) })

			if _, err := adapter.SendText(context.Background(), bridge.TextRequest{
				Conversation: bridge.ConversationRef{RemoteID: "signal:+15551234567"},
				Body:         "hello",
			}); err == nil {
				t.Fatal("SendText() error = nil, want scripted command failure")
			}
			current := adapter.currentRun()
			if current == nil || !current.admitCallback() {
				t.Fatal("non-account text failure closed callback admission on the receive generation")
			}
			current.callbacks.Done()
			select {
			case terminal := <-run.Done():
				t.Fatalf("non-account text failure terminated receive generation: %v", terminal)
			default:
			}
			if got := poller.appliedCount(); got != 0 {
				t.Fatalf("non-account text failure applied %d lifecycle transitions, want 0", got)
			}
		})
	}
}

func TestSendTextRoutesLocalAccountFailureThroughGuardedLifecycleReporting(t *testing.T) {
	poller := newFakePoller()
	poller.status = signallive.StatusSnapshot{
		Connected: true,
		Paired:    true,
		Account:   "+15551230000",
	}
	poller.textErr = signallive.NewCommandError("send Signal message: authorization failed")
	adapter := &Adapter{accountID: "signal-primary", poller: poller}
	run, err := adapter.Start(context.Background(), bridge.StartRequest{
		AccountID:  "signal-primary",
		Generation: 1,
	}, nil)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	receiveValue(t, poller.started, "retained poller start")

	if _, err := adapter.SendText(context.Background(), bridge.TextRequest{
		Conversation: bridge.ConversationRef{RemoteID: "signal:+15551234567"},
		Body:         "hello",
	}); err == nil {
		t.Fatal("SendText() error = nil, want local-account command failure")
	}
	terminal := receiveValue(t, run.Done(), "guarded text-send terminal")
	assertOpError(t, terminal, bridge.FailureTransient, "signal_send_account_check")
	if got := poller.appliedCount(); got != 1 {
		t.Fatalf("local-account text failure status projections = %d, want one transient projection", got)
	}
}

func TestSendReactionRequiresReadyRetainedBridge(t *testing.T) {
	tests := []struct {
		name   string
		poller poller
	}{
		{name: "nil retained bridge"},
		{name: "not connected", poller: newFakePoller()},
		{
			name: "connected without account",
			poller: &fakePoller{
				started: make(chan *fakePollerRun, 1),
				status:  signallive.StatusSnapshot{Connected: true},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			adapter := &Adapter{accountID: "signal-primary", poller: tc.poller}
			result, err := adapter.SendReaction(context.Background(), bridge.ReactionRequest{
				AccountID:    "signal-primary",
				Conversation: bridge.ConversationRef{RemoteID: "signal:+15551234567"},
				Target:       bridge.MessageRef{RemoteID: "1700000000123"},
				Emoji:        "👍",
				Action:       bridge.ReactionAdd,
			})
			if result != (bridge.SendResult{}) {
				t.Fatalf("SendReaction() result = %+v, want zero", result)
			}
			var operationError bridge.OpError
			if !errors.As(err, &operationError) {
				t.Fatalf("SendReaction() error = %v (%T), want bridge.OpError", err, err)
			}
			if operationError.Class != bridge.FailureTransient ||
				operationError.Dispatch != bridge.DispatchNotCalled ||
				operationError.Operation != "send_reaction" ||
				operationError.Fingerprint != "signal_not_connected" {
				t.Fatalf("SendReaction() OpError = %+v, want classified not-connected pre-call failure", operationError)
			}
			if fake, ok := tc.poller.(*fakePoller); ok && fake.reactionCallCount() != 0 {
				t.Fatalf("SendReactionRequest calls = %d, want 0 while not ready", fake.reactionCallCount())
			}
		})
	}
}

func TestSendReactionRejectsInvalidContextBeforeTransport(t *testing.T) {
	tests := []struct {
		name            string
		context         func() context.Context
		wantFingerprint string
	}{
		{
			name:            "nil context",
			context:         func() context.Context { return nil },
			wantFingerprint: "signal_send_reaction_context_missing",
		},
		{
			name: "done context",
			context: func() context.Context {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx
			},
			wantFingerprint: "signal_send_reaction_context_done",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			poller := newFakePoller()
			poller.status = signallive.StatusSnapshot{Connected: true, Account: "+15551230000"}
			adapter := &Adapter{accountID: "signal-primary", poller: poller}

			_, err := adapter.SendReaction(tc.context(), bridge.ReactionRequest{})
			var operationError bridge.OpError
			if !errors.As(err, &operationError) ||
				operationError.Class != bridge.FailureTransient ||
				operationError.Dispatch != bridge.DispatchNotCalled ||
				operationError.Operation != "send_reaction" ||
				operationError.Fingerprint != tc.wantFingerprint {
				t.Fatalf("SendReaction() error = %v, want pre-call context failure %q", err, tc.wantFingerprint)
			}
			if got := poller.reactionCallCount(); got != 0 {
				t.Fatalf("SendReactionRequest calls = %d, want 0 after context failure", got)
			}
		})
	}
}

func TestSendReactionMapsAllActionsAndReturnsEmptyResult(t *testing.T) {
	for _, action := range []bridge.ReactionAction{
		bridge.ReactionAdd,
		bridge.ReactionRemove,
		bridge.ReactionSwitch,
	} {
		t.Run(string(action), func(t *testing.T) {
			poller := newFakePoller()
			poller.status = signallive.StatusSnapshot{
				Connected: true,
				Paired:    true,
				Account:   "+15551230000",
			}
			adapter := &Adapter{accountID: "signal-primary", poller: poller}

			result, err := adapter.SendReaction(context.Background(), bridge.ReactionRequest{
				AccountID:    "signal-primary",
				Conversation: bridge.ConversationRef{RemoteID: "signal-group:test-group"},
				Target: bridge.MessageRef{
					RemoteID: "1700000000123",
					AuthorID: "+15551234567",
				},
				Emoji:     "👍",
				Action:    action,
				RequestID: "local-dedupe-only",
			})
			if err != nil {
				t.Fatalf("SendReaction() error = %v", err)
			}
			if result != (bridge.SendResult{}) {
				t.Fatalf("SendReaction() result = %+v, want empty ConfirmWithoutResult mapping", result)
			}
			request := poller.lastReactionRequest()
			if request.conversationID != "signal-group:test-group" ||
				request.targetRemoteID != "1700000000123" ||
				request.targetAuthorID != "+15551234567" ||
				request.emoji != "👍" ||
				request.action != string(action) {
				t.Fatalf("retained SendReactionRequest = %+v, want mapped ReactionRequest", request)
			}
		})
	}
}

func TestSendReactionClassifiesPreCallAndCommandFailures(t *testing.T) {
	tests := []struct {
		name         string
		err          error
		wantDispatch bridge.DispatchCertainty
	}{
		{
			name:         "local validation",
			err:          errors.New("signal target message is required"),
			wantDispatch: bridge.DispatchNotCalled,
		},
		{
			name: "signal-cli command",
			err:  signallive.NewCommandError("send Signal reaction: transport failed"),
		},
		{
			name: "proven not dispatched",
			err: &fakeSignalSendNotDispatchedError{err: signallive.NewCommandError(
				"send Signal reaction: not dispatched",
			)},
			wantDispatch: bridge.DispatchNotCalled,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			poller := newFakePoller()
			poller.status = signallive.StatusSnapshot{
				Connected: true,
				Paired:    true,
				Account:   "+15551230000",
			}
			poller.reactionErr = tc.err
			adapter := &Adapter{accountID: "signal-primary", poller: poller}

			_, err := adapter.SendReaction(context.Background(), bridge.ReactionRequest{})
			var operationError bridge.OpError
			if !errors.As(err, &operationError) {
				t.Fatalf("SendReaction() error = %v (%T), want bridge.OpError", err, err)
			}
			if operationError.Class != bridge.FailureTransient ||
				operationError.Dispatch != tc.wantDispatch ||
				operationError.Operation != "send_reaction" ||
				operationError.Fingerprint != "signal_reaction_failed" ||
				!errors.Is(operationError.Cause, tc.err) {
				t.Fatalf("SendReaction() OpError = %+v, want transient classified failure with dispatch %q", operationError, tc.wantDispatch)
			}
			if got := poller.appliedCount(); got != 0 {
				t.Fatalf("recipient/local reaction failure applied %d lifecycle transitions, want 0", got)
			}
		})
	}
}

func TestSendReactionDoesNotWidenGuardedLifecycleReporting(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{
			name: "unregistered recipient",
			err:  signallive.NewCommandError("send Signal reaction: user +15551234567 is not registered"),
		},
		{
			name: "untrusted identity",
			err:  signallive.NewCommandError("send Signal reaction: Untrusted identity key for +15551234567"),
		},
		{
			name: "network blip",
			err:  signallive.NewCommandError("send Signal reaction: Connection reset by peer"),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			poller := newFakePoller()
			poller.status = signallive.StatusSnapshot{
				Connected: true,
				Paired:    true,
				Account:   "+15551230000",
			}
			poller.reactionErr = tc.err
			adapter := &Adapter{accountID: "signal-primary", poller: poller}
			run, err := adapter.Start(context.Background(), bridge.StartRequest{
				AccountID:  "signal-primary",
				Generation: 1,
			}, nil)
			if err != nil {
				t.Fatalf("Start() error = %v", err)
			}
			receiveValue(t, poller.started, "retained poller start")
			t.Cleanup(func() { stopAdapterRun(t, run) })

			if _, err := adapter.SendReaction(context.Background(), bridge.ReactionRequest{}); err == nil {
				t.Fatal("SendReaction() error = nil, want scripted command failure")
			}
			current := adapter.currentRun()
			if current == nil || !current.admitCallback() {
				t.Fatal("non-account reaction failure closed callback admission on the receive generation")
			}
			current.callbacks.Done()
			select {
			case terminal := <-run.Done():
				t.Fatalf("non-account reaction failure terminated receive generation: %v", terminal)
			default:
			}
			if got := poller.appliedCount(); got != 0 {
				t.Fatalf("non-account reaction failure applied %d lifecycle transitions, want 0", got)
			}
		})
	}
}

func TestSendReactionRoutesLocalAccountFailureThroughGuardedLifecycleReporting(t *testing.T) {
	poller := newFakePoller()
	poller.status = signallive.StatusSnapshot{
		Connected: true,
		Paired:    true,
		Account:   "+15551230000",
	}
	poller.reactionErr = signallive.NewCommandError("send Signal reaction: authorization failed")
	adapter := &Adapter{accountID: "signal-primary", poller: poller}
	run, err := adapter.Start(context.Background(), bridge.StartRequest{
		AccountID:  "signal-primary",
		Generation: 1,
	}, nil)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	receiveValue(t, poller.started, "retained poller start")

	if _, err := adapter.SendReaction(context.Background(), bridge.ReactionRequest{}); err == nil {
		t.Fatal("SendReaction() error = nil, want local-account command failure")
	}
	terminal := receiveValue(t, run.Done(), "guarded reaction-send terminal")
	assertOpError(t, terminal, bridge.FailureTransient, "signal_send_account_check")
	if got := poller.appliedCount(); got != 1 {
		t.Fatalf("local-account reaction failure status projections = %d, want one transient projection", got)
	}
}

func TestSendMediaRequiresReadyRetainedBridge(t *testing.T) {
	tests := []struct {
		name   string
		poller poller
	}{
		{name: "nil retained bridge"},
		{name: "not connected", poller: newFakePoller()},
		{
			name: "connected without account",
			poller: &fakePoller{
				started: make(chan *fakePollerRun, 1),
				status:  signallive.StatusSnapshot{Connected: true},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			adapter := &Adapter{accountID: "signal-primary", poller: tc.poller}
			result, err := adapter.SendMedia(context.Background(), bridge.MediaRequest{
				AccountID:    "signal-primary",
				Conversation: bridge.ConversationRef{RemoteID: "signal:+15551234567"},
				Reader:       strings.NewReader("media"),
				Size:         int64(len("media")),
			})
			if result != (bridge.SendResult{}) {
				t.Fatalf("SendMedia() result = %+v, want zero", result)
			}
			var operationError bridge.OpError
			if !errors.As(err, &operationError) {
				t.Fatalf("SendMedia() error = %v (%T), want bridge.OpError", err, err)
			}
			if operationError.Class != bridge.FailureTransient ||
				operationError.Dispatch != bridge.DispatchNotCalled ||
				operationError.Operation != "send_media" ||
				operationError.Fingerprint != "signal_not_connected" {
				t.Fatalf("SendMedia() OpError = %+v, want classified not-connected pre-call failure", operationError)
			}
			if fake, ok := tc.poller.(*fakePoller); ok && fake.mediaCallCount() != 0 {
				t.Fatalf("SendMediaRequest calls = %d, want 0 while not ready", fake.mediaCallCount())
			}
		})
	}
}

func TestSendMediaMapsRetainedTimestampAndRequest(t *testing.T) {
	poller := newFakePoller()
	poller.status = signallive.StatusSnapshot{
		Connected: true,
		Paired:    true,
		Account:   "+15551230000",
	}
	poller.mediaTimestamp = 1700000000123
	adapter := &Adapter{accountID: "signal-primary", poller: poller}
	content := []byte("signal-media-content")

	result, err := adapter.SendMedia(context.Background(), bridge.MediaRequest{
		AccountID:    "signal-primary",
		Conversation: bridge.ConversationRef{RemoteID: "signal:+15551234567"},
		Reader:       bytes.NewReader(content),
		Size:         int64(len(content)),
		Filename:     "photo.png",
		MIME:         "image/png",
		Caption:      "signal photo",
		ReplyTo:      &bridge.MessageRef{RemoteID: "signal:1700000000000"},
		RequestID:    "local-dedupe-only",
	})
	if err != nil {
		t.Fatalf("SendMedia() error = %v", err)
	}
	if result.RemoteMessageID != strconv.FormatInt(poller.mediaTimestamp, 10) ||
		result.EchoExpected || !result.AcceptedAt.IsZero() {
		t.Fatalf("SendMedia() result = %+v, want canonical timestamp ID without echo", result)
	}
	request := poller.lastMediaRequest()
	if request.conversationID != "signal:+15551234567" ||
		request.size != int64(len(content)) ||
		request.filename != "photo.png" ||
		request.mime != "image/png" ||
		request.caption != "signal photo" ||
		request.replyToID != "signal:1700000000000" ||
		!bytes.Equal(request.content, content) {
		t.Fatalf("retained SendMediaRequest = %+v, want mapped MediaRequest", request)
	}
}

func TestSendMediaClassifiesPreCallAndCommandFailures(t *testing.T) {
	tests := []struct {
		name         string
		err          error
		wantDispatch bridge.DispatchCertainty
	}{
		{
			name:         "local attachment preparation",
			err:          errors.New("write Signal attachment temp file: disk full"),
			wantDispatch: bridge.DispatchNotCalled,
		},
		{
			name: "signal-cli command",
			err:  signallive.NewCommandError("send Signal media: recipient is not registered"),
		},
		{
			name: "structured all-recipient failure",
			err: &fakeSignalSendNotDispatchedError{err: signallive.NewCommandError(
				"send Signal media: every recipient failed",
			)},
			wantDispatch: bridge.DispatchNotCalled,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			poller := newFakePoller()
			poller.status = signallive.StatusSnapshot{
				Connected: true,
				Paired:    true,
				Account:   "+15551230000",
			}
			poller.mediaErr = tc.err
			adapter := &Adapter{accountID: "signal-primary", poller: poller}

			_, err := adapter.SendMedia(context.Background(), bridge.MediaRequest{
				AccountID:    "signal-primary",
				Conversation: bridge.ConversationRef{RemoteID: "signal:+15551234567"},
				Reader:       strings.NewReader("media"),
				Size:         int64(len("media")),
			})
			var operationError bridge.OpError
			if !errors.As(err, &operationError) {
				t.Fatalf("SendMedia() error = %v (%T), want bridge.OpError", err, err)
			}
			if operationError.Class != bridge.FailureTransient ||
				operationError.Dispatch != tc.wantDispatch ||
				operationError.Operation != "send_media" ||
				operationError.Fingerprint != "signal_send_media_failed" ||
				!errors.Is(operationError.Cause, tc.err) {
				t.Fatalf("SendMedia() OpError = %+v, want transient classified failure with dispatch %q", operationError, tc.wantDispatch)
			}
			if got := poller.appliedCount(); got != 0 {
				t.Fatalf("recipient/local send failure applied %d lifecycle transitions, want 0", got)
			}
		})
	}
}

func TestSendMediaRoutesCommandFailuresThroughGuardedLifecycleReporting(t *testing.T) {
	t.Run("recipient failure leaves receive generation running", func(t *testing.T) {
		poller := newFakePoller()
		poller.status = signallive.StatusSnapshot{
			Connected: true,
			Paired:    true,
			Account:   "+15551230000",
		}
		poller.mediaErr = &fakeSignalSendNotDispatchedError{err: signallive.NewCommandError(
			"send Signal media: user +15551234567 is not registered",
		)}
		adapter := &Adapter{accountID: "signal-primary", poller: poller}
		run, err := adapter.Start(context.Background(), bridge.StartRequest{
			AccountID:  "signal-primary",
			Generation: 1,
		}, nil)
		if err != nil {
			t.Fatalf("Start() error = %v", err)
		}
		receiveValue(t, poller.started, "retained poller start")
		t.Cleanup(func() { stopAdapterRun(t, run) })

		if _, err := adapter.SendMedia(context.Background(), bridge.MediaRequest{
			Conversation: bridge.ConversationRef{RemoteID: "signal:+15551234567"},
			Reader:       strings.NewReader("media"),
			Size:         int64(len("media")),
		}); err == nil {
			t.Fatal("SendMedia() error = nil, want recipient command failure")
		}
		select {
		case terminal := <-run.Done():
			t.Fatalf("recipient media failure terminated receive generation: %v", terminal)
		default:
		}
		if got := poller.appliedCount(); got != 0 {
			t.Fatalf("recipient media failure applied %d lifecycle transitions, want 0", got)
		}
	})

	t.Run("local account failure retires only transiently", func(t *testing.T) {
		poller := newFakePoller()
		poller.status = signallive.StatusSnapshot{
			Connected: true,
			Paired:    true,
			Account:   "+15551230000",
		}
		poller.mediaErr = signallive.NewCommandError("send Signal media: authorization failed")
		adapter := &Adapter{accountID: "signal-primary", poller: poller}
		run, err := adapter.Start(context.Background(), bridge.StartRequest{
			AccountID:  "signal-primary",
			Generation: 1,
		}, nil)
		if err != nil {
			t.Fatalf("Start() error = %v", err)
		}
		receiveValue(t, poller.started, "retained poller start")

		if _, err := adapter.SendMedia(context.Background(), bridge.MediaRequest{
			Conversation: bridge.ConversationRef{RemoteID: "signal:+15551234567"},
			Reader:       strings.NewReader("media"),
			Size:         int64(len("media")),
		}); err == nil {
			t.Fatal("SendMedia() error = nil, want local-account command failure")
		}
		terminal := receiveValue(t, run.Done(), "guarded media-send terminal")
		assertOpError(t, terminal, bridge.FailureTransient, "signal_send_account_check")
		if got := poller.appliedCount(); got != 1 {
			t.Fatalf("local-account media failure status projections = %d, want one transient projection", got)
		}
	})
}

func TestRunTranslatesRetainedPollerSignals(t *testing.T) {
	poller := newFakePoller()
	adapter := &Adapter{accountID: "signal-primary", poller: poller}
	sink := &recordingSink{beats: make(chan recordedBeat, 1)}

	run, err := adapter.Start(context.Background(), bridge.StartRequest{
		AccountID:  "signal-primary",
		Generation: 7,
	}, sink)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { stopAdapterRun(t, run) })
	retained := receiveValue(t, poller.started, "retained poller start")

	assertChannelOpen(t, run.Ready(), "before retained poller readiness")
	retained.markReady()
	select {
	case <-run.Ready():
	case <-time.After(signalAdapterTestTimeout):
		t.Fatal("adapter Ready did not follow retained poller Ready")
	}

	activity := signallive.PollerActivity{
		At:     time.Now().Add(time.Hour),
		Detail: "receive_batch",
	}
	retained.emitActivity(activity)
	beat := receiveValue(t, sink.beats, "retained poller activity beat")
	if beat.generation != 7 || !beat.at.Equal(activity.At) || beat.detail != activity.Detail {
		t.Fatalf("Beat = %+v, want generation 7 and activity %+v", beat, activity)
	}

	probeContext, probeCancel := context.WithTimeout(context.Background(), signalAdapterTestTimeout)
	defer probeCancel()
	liveness, err := run.Probe(probeContext)
	if err != nil {
		t.Fatalf("Probe() error = %v", err)
	}
	if !liveness.AliveAt.Equal(activity.At) || liveness.Detail != activity.Detail {
		t.Fatalf("Probe() = %+v, want activity %+v", liveness, activity)
	}

	retained.complete(signallive.PollerExit{
		Kind:        signallive.PollerFailureTransient,
		Operation:   "receive",
		Fingerprint: signallive.SignalReceiveFailureFingerprint,
		Err:         errors.New("receive failed repeatedly"),
	})
	terminal := receiveValue(t, run.Done(), "adapter terminal result")
	assertOpError(t, terminal, bridge.FailureTransient, signallive.SignalReceiveFailureFingerprint)
	if _, ok := <-run.Done(); ok {
		t.Fatal("adapter Done remained open after its one terminal result")
	}
	if got := retained.stopCount(); got != 1 {
		t.Fatalf("retained poller join calls after natural exit = %d, want 1", got)
	}
}

func TestAdapterStartIsSingleFlight(t *testing.T) {
	poller := newFakePoller()
	poller.startEntered = make(chan struct{}, 1)
	poller.startRelease = make(chan struct{})
	adapter := &Adapter{accountID: "signal-primary", poller: poller}

	type startResult struct {
		run bridge.Run
		err error
	}
	firstResult := make(chan startResult, 1)
	go func() {
		run, err := adapter.Start(context.Background(), bridge.StartRequest{
			AccountID:  "signal-primary",
			Generation: 1,
		}, nil)
		firstResult <- startResult{run: run, err: err}
	}()
	receiveValue(t, poller.startEntered, "first StartPoller admission")

	second, err := adapter.Start(context.Background(), bridge.StartRequest{
		AccountID:  "signal-primary",
		Generation: 2,
	}, nil)
	if second != nil {
		t.Fatalf("overlapping Start() run = %T, want nil", second)
	}
	assertOpError(t, err, bridge.FailureMisconfigured, "overlapping_signal_generation")
	if got := poller.startCount(); got != 1 {
		t.Fatalf("StartPoller calls while first start is admitted = %d, want 1", got)
	}

	close(poller.startRelease)
	first := receiveValue(t, firstResult, "first adapter Start result")
	if first.err != nil {
		t.Fatalf("first Start() error = %v", first.err)
	}
	if first.run == nil {
		t.Fatal("first Start() run = nil")
	}
	stopAdapterRun(t, first.run)
}

func TestAdapterStartDoesNotWaitForReady(t *testing.T) {
	poller := newFakePoller()
	adapter := &Adapter{accountID: "signal-primary", poller: poller}

	returned := make(chan bridge.Run, 1)
	errorsReturned := make(chan error, 1)
	go func() {
		run, err := adapter.Start(context.Background(), bridge.StartRequest{
			AccountID:  "signal-primary",
			Generation: 1,
		}, nil)
		if err != nil {
			errorsReturned <- err
			return
		}
		returned <- run
	}()

	var run bridge.Run
	select {
	case err := <-errorsReturned:
		t.Fatalf("Start() error = %v", err)
	case run = <-returned:
	case <-time.After(signalAdapterTestTimeout):
		t.Fatal("Start blocked waiting for retained poller Ready")
	}
	assertChannelOpen(t, run.Ready(), "after non-blocking Start return")
	stopAdapterRun(t, run)
}

func TestPollerTerminalOutranksRacingTransientSendFailure(t *testing.T) {
	poller := newFakePoller()
	adapter := &Adapter{accountID: "signal-primary", poller: poller}
	run, err := adapter.Start(context.Background(), bridge.StartRequest{
		AccountID:  "signal-primary",
		Generation: 1,
	}, nil)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	retained := receiveValue(t, poller.started, "retained poller start")
	retained.setStopExit(signallive.PollerExit{
		Kind:        signallive.PollerFailureUpgrade,
		Operation:   "receive",
		Fingerprint: "incoming_message_get_sender_content_null",
		Err:         errors.New("signal-cli poison envelope"),
	})

	if reported := adapter.ReportError(signallive.NewCommandError("send failed: authorization failed")); !reported {
		t.Fatal("ReportError() rejected an admitted local-account send failure")
	}
	terminal := receiveValue(t, run.Done(), "arbitrated terminal result")
	assertOpError(
		t,
		terminal,
		bridge.FailureUpgradeRequired,
		"incoming_message_get_sender_content_null",
	)
	if got := poller.appliedCount(); got != 0 {
		t.Fatalf("send failure status projections after poller terminal won = %d, want 0", got)
	}
}

func TestStrongestAlreadyAdmittedSendFailureWins(t *testing.T) {
	poller := newFakePoller()
	adapter := &Adapter{accountID: "signal-primary", poller: poller}
	started, err := adapter.Start(context.Background(), bridge.StartRequest{
		AccountID:  "signal-primary",
		Generation: 1,
	}, nil)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	receiveValue(t, poller.started, "retained poller start")
	r := started.(*run)
	if !r.admitCallback() || !r.admitCallback() {
		t.Fatal("failed to admit send callbacks before terminal selection")
	}
	transientExit := signallive.PollerExit{
		Kind:        signallive.PollerFailureTransient,
		Operation:   "send",
		Fingerprint: "signal_send_failed",
		Err:         errors.New("temporary send failure"),
	}
	reauthExit := signallive.PollerExit{
		Kind:        signallive.PollerFailureReauth,
		Operation:   "send",
		Fingerprint: signallive.SignalAccountInvalidFingerprint,
		Err:         errors.New("account is not registered"),
	}
	r.requestFinish(terminalCandidate{
		err:      classifyPollerExit(transientExit),
		sendExit: &transientExit,
	})
	r.requestFinish(terminalCandidate{
		err:      classifyPollerExit(reauthExit),
		sendExit: &reauthExit,
	})
	r.callbacks.Done()
	r.callbacks.Done()

	terminal := receiveValue(t, started.Done(), "strongest send terminal result")
	assertOpError(
		t,
		terminal,
		bridge.FailureReauthRequired,
		signallive.SignalAccountInvalidFingerprint,
	)
	if got := poller.appliedCount(); got != 1 {
		t.Fatalf("winning send status projections = %d, want 1", got)
	}
}

// A send can fail for many reasons that do NOT indict the local Signal account:
// an unregistered *recipient* (whose "not registered" text is identical to an
// unregistered local account), an untrusted-identity change, a group-permission
// denial, a rate/proof challenge, or a plain network blip. None of these may
// retire the healthy receive generation — the account-scoped receive probe owns
// account validity. Regression for the C6 D1/D2 send-classification defect.
func TestReportErrorIgnoresRecipientAndNonAccountSendFailures(t *testing.T) {
	cases := []struct {
		name string
		text string
	}{
		{"unregistered recipient", "send failed: Untrusted or not registered: user +15551230000 is not registered"},
		{"untrusted identity", "send failed: Untrusted identity key for +15551230000"},
		{"group permission", "send failed: User is not authorized to send to this group"},
		{"rate limited", "send failed: Rate limit challenge required (proof of work)"},
		{"network blip", "send failed: Connection reset by peer"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			poller := newFakePoller()
			adapter := &Adapter{accountID: "signal-primary", poller: poller}
			run, err := adapter.Start(context.Background(), bridge.StartRequest{
				AccountID:  "signal-primary",
				Generation: 1,
			}, nil)
			if err != nil {
				t.Fatalf("Start() error = %v", err)
			}
			receiveValue(t, poller.started, "retained poller start")
			t.Cleanup(func() { stopAdapterRun(t, run) })

			if reported := adapter.ReportError(signallive.NewCommandError(tc.text)); reported {
				t.Fatalf("ReportError(%q) acted on a non-account send failure", tc.text)
			}
			select {
			case terminal := <-run.Done():
				t.Fatalf("ignored send failure produced terminal %v", terminal)
			default:
			}
			if got := poller.appliedCount(); got != 0 {
				t.Fatalf("status projections after ignored send failure = %d, want 0", got)
			}
		})
	}
}

// An unambiguous local-account send failure ("authorization failed" / "invalid
// account") is worth a nudge, but only as a *transient* bounce — never reauth.
// The next generation's account-scoped probe, not this send's text, is what
// parks reauth, so a send can never by itself drive the bridge to Blocked.
func TestReportErrorTransientlyReportsLocalAccountSendFailure(t *testing.T) {
	for _, text := range []string{
		"send failed: authorization failed",
		"send failed: invalid account",
	} {
		t.Run(text, func(t *testing.T) {
			poller := newFakePoller()
			adapter := &Adapter{accountID: "signal-primary", poller: poller}
			run, err := adapter.Start(context.Background(), bridge.StartRequest{
				AccountID:  "signal-primary",
				Generation: 1,
			}, nil)
			if err != nil {
				t.Fatalf("Start() error = %v", err)
			}
			receiveValue(t, poller.started, "retained poller start")
			t.Cleanup(func() { stopAdapterRun(t, run) })

			if reported := adapter.ReportError(signallive.NewCommandError(text)); !reported {
				t.Fatalf("ReportError(%q) ignored a local-account send failure", text)
			}
			// FailureTransient + signal_send_account_check IS the proof it is not
			// reauth/signal_account_invalid — the probe, not the send, owns reauth.
			terminal := receiveValue(t, run.Done(), "local-account send terminal")
			assertOpError(t, terminal, bridge.FailureTransient, "signal_send_account_check")
		})
	}
}

// End to end: texting a number that is not on Signal must not disturb the live
// receive loop. The supervised generation stays Online — no reconnect, no
// projection, and never Blocked or needs-reauth.
func TestSupervisorKeepsReceivingThroughUnregisteredRecipientSend(t *testing.T) {
	poller := newFakePoller()
	adapter := &Adapter{accountID: "signal-primary", poller: poller}
	clock := newManualClock(signalAdapterTestEpoch)
	supervisor := newSignalTestSupervisor(t, adapter, signalSupervisorTestPolicy(), clock)

	startSupervisor(t, supervisor)
	readyNextGeneration(t, supervisor, poller, 1)

	if reported := adapter.ReportError(signallive.NewCommandError(
		"send failed: user +15551230000 is not registered",
	)); reported {
		t.Fatal("ReportError() acted on an unregistered-recipient send failure")
	}

	syncSupervisor(t, supervisor)
	if got := poller.startCount(); got != 1 {
		t.Fatalf("StartPoller calls after recipient send failure = %d, want 1", got)
	}
	if got := poller.appliedCount(); got != 0 {
		t.Fatalf("status projections after recipient send failure = %d, want 0", got)
	}
	snapshot := supervisor.Snapshot()
	if snapshot.State != bridge.StateOnline || snapshot.Generation != 1 {
		t.Fatalf("snapshot after recipient send failure = %+v, want online generation 1", snapshot)
	}
	if snapshot.ErrorClass == bridge.FailureReauthRequired ||
		snapshot.ErrorFingerprint == signallive.SignalAccountInvalidFingerprint {
		t.Fatalf("recipient send failure produced reauth: %+v", snapshot)
	}
}

func TestSupervisorReceiveFailureRetriesBeyondFingerprintThreshold(t *testing.T) {
	poller := newFakePoller()
	adapter := &Adapter{accountID: "signal-primary", poller: poller}
	clock := newManualClock(signalAdapterTestEpoch)
	policy := signalSupervisorTestPolicy()
	policy.MaxSameFingerprint = 2
	supervisor := newSignalTestSupervisor(t, adapter, policy, clock)

	startSupervisor(t, supervisor)
	retained := readyNextGeneration(t, supervisor, poller, 1)
	for generation := bridge.Generation(1); generation <= 3; generation++ {
		retained.complete(signallive.PollerExit{
			Kind:        signallive.PollerFailureTransient,
			Operation:   "receive",
			Fingerprint: signallive.SignalReceiveFailureFingerprint,
			Err:         errors.New("signal-cli receive failed repeatedly"),
		})
		backoff := awaitSnapshot(t, supervisor, "transient receive backoff", func(snapshot bridge.Snapshot) bool {
			return snapshot.Generation == generation && snapshot.State == bridge.StateBackoff
		})
		if backoff.ErrorClass != bridge.FailureTransient ||
			backoff.ErrorFingerprint != signallive.SignalReceiveFailureFingerprint {
			t.Fatalf("generation %d backoff = %+v", generation, backoff)
		}
		clock.Advance(backoff.RetryAt.Sub(clock.Now()))
		retained = readyNextGeneration(t, supervisor, poller, generation+1)
	}

	if got := poller.startCount(); got != 4 {
		t.Fatalf("StartPoller calls after three identical transient failures = %d, want 4", got)
	}
	if snapshot := supervisor.Snapshot(); snapshot.State != bridge.StateOnline || snapshot.Generation != 4 {
		t.Fatalf("snapshot after transient retries = %+v, want online generation 4", snapshot)
	}
}

func TestSupervisorParksTerminalSignalFailuresWithoutReconnectChurn(t *testing.T) {
	tests := []struct {
		name        string
		exit        signallive.PollerExit
		wantClass   bridge.FailureClass
		fingerprint string
	}{
		{
			name: "account invalid requires re-pair",
			exit: signallive.PollerExit{
				Kind:        signallive.PollerFailureReauth,
				Operation:   "probe_account",
				Fingerprint: signallive.SignalAccountInvalidFingerprint,
				Err:         errors.New("account is not registered"),
			},
			wantClass:   bridge.FailureReauthRequired,
			fingerprint: signallive.SignalAccountInvalidFingerprint,
		},
		{
			name: "old signal-cli version",
			exit: signallive.PollerExit{
				Kind:        signallive.PollerFailureUpgrade,
				Operation:   "version_gate",
				Fingerprint: signallive.SignalCLIVersionFingerprint,
				Err:         errors.New("signal-cli is below the minimum version"),
			},
			wantClass:   bridge.FailureUpgradeRequired,
			fingerprint: signallive.SignalCLIVersionFingerprint,
		},
		{
			name: "getSender poison circuit",
			exit: signallive.PollerExit{
				Kind:        signallive.PollerFailureUpgrade,
				Operation:   "receive",
				Fingerprint: "incoming_message_get_sender_content_null",
				Err:         errors.New("IncomingMessageHandler.getSender content is null"),
			},
			wantClass:   bridge.FailureUpgradeRequired,
			fingerprint: "incoming_message_get_sender_content_null",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			poller := newFakePoller()
			adapter := &Adapter{accountID: "signal-primary", poller: poller}
			clock := newManualClock(signalAdapterTestEpoch)
			supervisor := newSignalTestSupervisor(t, adapter, signalSupervisorTestPolicy(), clock)

			startSupervisor(t, supervisor)
			retained := readyNextGeneration(t, supervisor, poller, 1)
			retained.complete(test.exit)
			blocked := awaitSnapshot(t, supervisor, "blocked terminal failure", func(snapshot bridge.Snapshot) bool {
				return snapshot.State == bridge.StateBlocked
			})
			if blocked.ErrorClass != test.wantClass || blocked.ErrorFingerprint != test.fingerprint {
				t.Fatalf("blocked snapshot = %+v, want class %q fingerprint %q", blocked, test.wantClass, test.fingerprint)
			}

			clock.Advance(7 * 24 * time.Hour)
			syncSupervisor(t, supervisor)
			if got := poller.startCount(); got != 1 {
				t.Fatalf("StartPoller calls after a week parked = %d, want 1", got)
			}
			if snapshot := supervisor.Snapshot(); snapshot.State != bridge.StateBlocked || snapshot.Generation != 1 {
				t.Fatalf("snapshot after parked clock advance = %+v", snapshot)
			}
		})
	}
}

func TestSupervisorLeavesExpiredPairingUnpairedWithoutReconnectChurn(t *testing.T) {
	poller := newFakePoller()
	adapter := &Adapter{accountID: "signal-primary", poller: poller}
	clock := newManualClock(signalAdapterTestEpoch)
	supervisor := newSignalTestSupervisor(t, adapter, signalSupervisorTestPolicy(), clock)

	startSupervisor(t, supervisor)
	retained := receiveValue(t, poller.started, "pairing poller start")
	retained.complete(signallive.PollerExit{
		Kind:        signallive.PollerFailureUnpaired,
		Operation:   "pair",
		Fingerprint: signallive.SignalPairingIncompleteFingerprint,
		Err:         errors.New("Signal link QR expired"),
	})
	unpaired := awaitSnapshot(t, supervisor, "unpaired pairing expiry", func(snapshot bridge.Snapshot) bool {
		return snapshot.State == bridge.StateUnpaired
	})
	if unpaired.ErrorClass != bridge.FailureUnpaired ||
		unpaired.ErrorFingerprint != signallive.SignalPairingIncompleteFingerprint ||
		!unpaired.RetryAt.IsZero() {
		t.Fatalf("unpaired snapshot = %+v", unpaired)
	}

	clock.Advance(7 * 24 * time.Hour)
	syncSupervisor(t, supervisor)
	if got := poller.startCount(); got != 1 {
		t.Fatalf("StartPoller calls after a week unpaired = %d, want 1", got)
	}
	if snapshot := supervisor.Snapshot(); snapshot.State != bridge.StateUnpaired {
		t.Fatalf("snapshot after unpaired clock advance = %+v", snapshot)
	}
}

type fakePoller struct {
	mu             sync.Mutex
	starts         int
	runs           []*fakePollerRun
	started        chan *fakePollerRun
	startEntered   chan struct{}
	startRelease   chan struct{}
	applied        []signallive.PollerExit
	fingerprint    string
	status         signallive.StatusSnapshot
	textCalls      []fakeTextRequest
	textTimestamp  int64
	textErr        error
	reactionCalls  []fakeReactionRequest
	reactionErr    error
	mediaCalls     []fakeMediaRequest
	mediaTimestamp int64
	mediaErr       error
}

type fakeTextRequest struct {
	conversationID string
	body           string
	replyToID      string
}

type fakeReactionRequest struct {
	conversationID string
	targetRemoteID string
	targetAuthorID string
	emoji          string
	action         string
}

type fakeMediaRequest struct {
	conversationID string
	content        []byte
	size           int64
	filename       string
	mime           string
	caption        string
	replyToID      string
}

type fakeSignalSendNotDispatchedError struct {
	err error
}

func (e *fakeSignalSendNotDispatchedError) Error() string { return e.err.Error() }

func (e *fakeSignalSendNotDispatchedError) Unwrap() error { return e.err }

func (*fakeSignalSendNotDispatchedError) SignalSendNotDispatched() {}

func newFakePoller() *fakePoller {
	return &fakePoller{started: make(chan *fakePollerRun, 16)}
}

func (p *fakePoller) StartPoller(ctx context.Context) (signallive.PollerRun, error) {
	p.mu.Lock()
	p.starts++
	entered := p.startEntered
	release := p.startRelease
	p.mu.Unlock()
	if entered != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
	}
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	run := newFakePollerRun()
	p.mu.Lock()
	p.runs = append(p.runs, run)
	p.mu.Unlock()
	select {
	case p.started <- run:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return run, nil
}

func (p *fakePoller) Status() signallive.StatusSnapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.status
}

func (p *fakePoller) SendTextRequest(conversationID, body, replyToID string) (int64, error) {
	p.mu.Lock()
	p.textCalls = append(p.textCalls, fakeTextRequest{
		conversationID: conversationID,
		body:           body,
		replyToID:      replyToID,
	})
	timestamp, err := p.textTimestamp, p.textErr
	p.mu.Unlock()
	return timestamp, err
}

func (p *fakePoller) textCallCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.textCalls)
}

func (p *fakePoller) lastTextRequest() fakeTextRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.textCalls) == 0 {
		return fakeTextRequest{}
	}
	return p.textCalls[len(p.textCalls)-1]
}

func (p *fakePoller) SendReactionRequest(
	conversationID, targetRemoteID, targetAuthorID, emoji, action string,
) error {
	p.mu.Lock()
	p.reactionCalls = append(p.reactionCalls, fakeReactionRequest{
		conversationID: conversationID,
		targetRemoteID: targetRemoteID,
		targetAuthorID: targetAuthorID,
		emoji:          emoji,
		action:         action,
	})
	err := p.reactionErr
	p.mu.Unlock()
	return err
}

func (p *fakePoller) reactionCallCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.reactionCalls)
}

func (p *fakePoller) lastReactionRequest() fakeReactionRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.reactionCalls) == 0 {
		return fakeReactionRequest{}
	}
	return p.reactionCalls[len(p.reactionCalls)-1]
}

func (p *fakePoller) SendMediaRequest(
	conversationID string,
	content io.Reader,
	size int64,
	filename, mime, caption, replyToID string,
) (int64, error) {
	data, readErr := io.ReadAll(content)
	p.mu.Lock()
	p.mediaCalls = append(p.mediaCalls, fakeMediaRequest{
		conversationID: conversationID,
		content:        data,
		size:           size,
		filename:       filename,
		mime:           mime,
		caption:        caption,
		replyToID:      replyToID,
	})
	timestamp, sendErr := p.mediaTimestamp, p.mediaErr
	p.mu.Unlock()
	if readErr != nil {
		return 0, readErr
	}
	return timestamp, sendErr
}

func (p *fakePoller) mediaCallCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.mediaCalls)
}

func (p *fakePoller) lastMediaRequest() fakeMediaRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.mediaCalls) == 0 {
		return fakeMediaRequest{}
	}
	return p.mediaCalls[len(p.mediaCalls)-1]
}

func (p *fakePoller) ApplyPollerFailure(exit signallive.PollerExit) {
	p.mu.Lock()
	p.applied = append(p.applied, exit)
	p.mu.Unlock()
}

func (p *fakePoller) appliedCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.applied)
}

func (p *fakePoller) InputFingerprint() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.fingerprint
}

func (*fakePoller) UnpairContext(context.Context) error { return nil }

func (p *fakePoller) startCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.starts
}

type fakePollerRun struct {
	ready      chan struct{}
	activity   chan signallive.PollerActivity
	done       chan signallive.PollerExit
	stopped    chan struct{}
	readyOnce  sync.Once
	finishOnce sync.Once
	mu         sync.Mutex
	stops      int
	stopExit   signallive.PollerExit
}

func newFakePollerRun() *fakePollerRun {
	return &fakePollerRun{
		ready:    make(chan struct{}),
		activity: make(chan signallive.PollerActivity, 4),
		done:     make(chan signallive.PollerExit, 1),
		stopped:  make(chan struct{}),
	}
}

func (r *fakePollerRun) Ready() <-chan struct{} { return r.ready }

func (r *fakePollerRun) Activity() <-chan signallive.PollerActivity { return r.activity }

func (r *fakePollerRun) Done() <-chan signallive.PollerExit { return r.done }

func (r *fakePollerRun) Stop(ctx context.Context) error {
	r.mu.Lock()
	r.stops++
	exit := r.stopExit
	r.mu.Unlock()
	r.complete(exit)
	select {
	case <-r.stopped:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *fakePollerRun) setStopExit(exit signallive.PollerExit) {
	r.mu.Lock()
	r.stopExit = exit
	r.mu.Unlock()
}

func (r *fakePollerRun) markReady() {
	r.readyOnce.Do(func() { close(r.ready) })
}

func (r *fakePollerRun) emitActivity(activity signallive.PollerActivity) {
	r.activity <- activity
}

func (r *fakePollerRun) complete(exit signallive.PollerExit) {
	r.finishOnce.Do(func() {
		r.done <- exit
		close(r.done)
		close(r.activity)
		close(r.stopped)
	})
}

func (r *fakePollerRun) stopCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stops
}

type recordedBeat struct {
	generation bridge.Generation
	at         time.Time
	detail     string
}

type recordingSink struct {
	beats chan recordedBeat
}

func (*recordingSink) AppendIngress(context.Context, bridge.RawIngressRecord) error { return nil }

func (*recordingSink) EmitEphemeral(context.Context, bridge.EphemeralEvent) error { return nil }

func (s *recordingSink) Beat(generation bridge.Generation, at time.Time, detail string) {
	s.beats <- recordedBeat{generation: generation, at: at, detail: detail}
}

type manualClock struct {
	mu     sync.Mutex
	now    time.Time
	timers map[*manualTimer]struct{}
}

func newManualClock(now time.Time) *manualClock {
	return &manualClock{now: now, timers: make(map[*manualTimer]struct{})}
}

func (c *manualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *manualClock) NewTimer(delay time.Duration) bridge.Timer {
	if delay < 0 {
		delay = 0
	}
	c.mu.Lock()
	timer := &manualTimer{
		clock:    c,
		deadline: c.now.Add(delay),
		channel:  make(chan time.Time, 1),
		active:   delay != 0,
	}
	if timer.active {
		c.timers[timer] = struct{}{}
	}
	now := c.now
	c.mu.Unlock()
	if delay == 0 {
		timer.channel <- now
	}
	return timer
}

func (c *manualClock) Advance(delta time.Duration) {
	if delta < 0 {
		panic("manual clock cannot move backward")
	}
	c.mu.Lock()
	c.now = c.now.Add(delta)
	now := c.now
	var due []*manualTimer
	for timer := range c.timers {
		if !now.Before(timer.deadline) {
			timer.active = false
			delete(c.timers, timer)
			due = append(due, timer)
		}
	}
	c.mu.Unlock()
	for _, timer := range due {
		timer.channel <- now
	}
}

type manualTimer struct {
	clock    *manualClock
	deadline time.Time
	channel  chan time.Time
	active   bool
}

func (t *manualTimer) C() <-chan time.Time { return t.channel }

func (t *manualTimer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	if !t.active {
		return false
	}
	t.active = false
	delete(t.clock.timers, t)
	return true
}

type midpointRandom struct{}

func (midpointRandom) Int63n(bound int64) int64 { return bound / 2 }

func signalSupervisorTestPolicy() bridge.Policy {
	return bridge.Policy{
		ConnectTimeout:     time.Hour,
		ProbeEvery:         time.Hour,
		ProbeTimeout:       time.Hour,
		LivenessTimeout:    time.Hour,
		MinBackoff:         10 * time.Second,
		MaxBackoff:         time.Minute,
		MaxSameFingerprint: 3,
	}
}

func newSignalTestSupervisor(
	t *testing.T,
	adapter *Adapter,
	policy bridge.Policy,
	clock *manualClock,
) *bridge.Supervisor {
	t.Helper()
	supervisor, err := bridge.NewSupervisor(
		"signal-primary",
		bridge.PlatformSignal,
		adapter,
		policy,
		clock,
		midpointRandom{},
	)
	if err != nil {
		t.Fatalf("NewSupervisor() error = %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), signalAdapterTestTimeout)
		defer cancel()
		if err := supervisor.Stop(ctx); err != nil {
			t.Errorf("Supervisor.Stop() cleanup error = %v", err)
		}
	})
	return supervisor
}

func startSupervisor(t *testing.T, supervisor *bridge.Supervisor) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), signalAdapterTestTimeout)
	defer cancel()
	if err := supervisor.Start(ctx, bridge.StartRequest{}); err != nil {
		t.Fatalf("Supervisor.Start() error = %v", err)
	}
}

func syncSupervisor(t *testing.T, supervisor *bridge.Supervisor) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), signalAdapterTestTimeout)
	defer cancel()
	if err := supervisor.Sync(ctx); err != nil {
		t.Fatalf("Supervisor.Sync() error = %v", err)
	}
}

func readyNextGeneration(
	t *testing.T,
	supervisor *bridge.Supervisor,
	poller *fakePoller,
	generation bridge.Generation,
) *fakePollerRun {
	t.Helper()
	retained := receiveValue(t, poller.started, "retained poller generation start")
	retained.markReady()
	awaitSnapshot(t, supervisor, "online generation", func(snapshot bridge.Snapshot) bool {
		return snapshot.State == bridge.StateOnline && snapshot.Generation == generation
	})
	return retained
}

func awaitSnapshot(
	t *testing.T,
	supervisor *bridge.Supervisor,
	description string,
	predicate func(bridge.Snapshot) bool,
) bridge.Snapshot {
	t.Helper()
	deadline := time.NewTimer(signalAdapterTestTimeout)
	defer deadline.Stop()
	for {
		snapshot := supervisor.Snapshot()
		if predicate(snapshot) {
			return snapshot
		}
		select {
		case <-deadline.C:
			t.Fatalf("timed out waiting for %s; last snapshot: %+v", description, snapshot)
		default:
			runtime.Gosched()
		}
	}
}

func assertChannelOpen(t *testing.T, channel <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-channel:
		t.Fatalf("channel closed %s", description)
	default:
	}
}

func assertOpError(
	t *testing.T,
	err error,
	wantClass bridge.FailureClass,
	wantFingerprint string,
) {
	t.Helper()
	var operationError bridge.OpError
	if !errors.As(err, &operationError) {
		t.Fatalf("error = %v (%T), want bridge.OpError", err, err)
	}
	if operationError.Class != wantClass || operationError.Fingerprint != wantFingerprint {
		t.Fatalf("OpError = %+v, want class %q fingerprint %q", operationError, wantClass, wantFingerprint)
	}
}

func stopAdapterRun(t *testing.T, run bridge.Run) {
	t.Helper()
	if run == nil {
		return
	}
	select {
	case <-run.Done():
		return
	default:
	}
	ctx, cancel := context.WithTimeout(context.Background(), signalAdapterTestTimeout)
	defer cancel()
	if err := run.Stop(ctx); err != nil {
		t.Fatalf("Run.Stop() error = %v", err)
	}
}

func receiveValue[T any](t *testing.T, channel <-chan T, description string) T {
	t.Helper()
	select {
	case value := <-channel:
		return value
	case <-time.After(signalAdapterTestTimeout):
		var zero T
		t.Fatalf("timed out waiting for %s", description)
		return zero
	}
}

var (
	_ poller                = (*fakePoller)(nil)
	_ signallive.PollerRun  = (*fakePollerRun)(nil)
	_ bridge.ConnectionSink = (*recordingSink)(nil)
	_ bridge.Clock          = (*manualClock)(nil)
	_ bridge.Timer          = (*manualTimer)(nil)
	_ bridge.Random         = midpointRandom{}
)
