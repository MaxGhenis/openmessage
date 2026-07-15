package google

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/client"
)

func TestSendTextNotConnectedIsClassifiedPreCall(t *testing.T) {
	host := newTestApp(t)
	a := New("google-primary", host, func() bool { return true })
	a.newClient = func() (*client.Client, transportClient, error) {
		t.Fatal("SendText must not construct a Google transport")
		return nil, nil, nil
	}

	result, err := a.SendText(context.Background(), bridge.TextRequest{
		AccountID: "google-primary",
		Conversation: bridge.ConversationRef{
			RemoteID: "conversation-id",
		},
		Body:      "hello",
		RequestID: "request-id",
	})
	if result != (bridge.SendResult{}) {
		t.Fatalf("result = %+v, want zero value", result)
	}
	failure := requireTextOpError(t, err)
	if failure.Class != bridge.FailureTransient ||
		failure.Dispatch != bridge.DispatchNotCalled ||
		failure.Operation != "send_text" ||
		failure.Fingerprint != "google_not_connected" {
		t.Fatalf("failure = %+v, want transient pre-call google_not_connected", failure)
	}
}

func TestSendTextContextGuardsAreClassifiedPreCall(t *testing.T) {
	tests := []struct {
		name        string
		ctx         context.Context
		fingerprint string
	}{
		{
			name:        "nil",
			ctx:         nil,
			fingerprint: "google_text_context_invalid",
		},
		{
			name: "done",
			ctx: func() context.Context {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx
			}(),
			fingerprint: "google_text_context_done",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := &fakeTextSendClient{}
			a := newTextSendTestAdapter(t, fake)

			_, err := a.SendText(test.ctx, bridge.TextRequest{})
			failure := requireTextOpError(t, err)
			if failure.Class != bridge.FailureTransient ||
				failure.Dispatch != bridge.DispatchNotCalled ||
				failure.Operation != "send_text" ||
				failure.Fingerprint != test.fingerprint {
				t.Fatalf("failure = %+v, want transient pre-call %s", failure, test.fingerprint)
			}
			if fake.conversationCalls != 0 || fake.sendCalls != 0 {
				t.Fatalf("transport calls = (conversation %d, send %d), want (0, 0)",
					fake.conversationCalls, fake.sendCalls)
			}
		})
	}
}

func TestSendTextSuccessUsesStablePayloadAndMapsResult(t *testing.T) {
	sim := &gmproto.SIMPayload{Two: 7, SIMNumber: 2}
	conversation := &gmproto.Conversation{
		ConversationID: "remote-conversation",
		Participants: []*gmproto.Participant{{
			ID:         &gmproto.SmallInfo{Number: "+15551234567"},
			IsMe:       true,
			SimPayload: sim,
		}},
	}
	fake := &fakeTextSendClient{
		conversationResult: conversation,
		sendResult: &gmproto.SendMessageResponse{
			Status: gmproto.SendMessageResponse_SUCCESS,
		},
	}
	a := newTextSendTestAdapter(t, fake)

	result, err := a.SendText(context.Background(), bridge.TextRequest{
		AccountID: "google-primary",
		Conversation: bridge.ConversationRef{
			RemoteID: "remote-conversation",
		},
		Body:      "hello from the durable outbox",
		ReplyTo:   &bridge.MessageRef{RemoteID: "reply-id"},
		RequestID: "transport-request-id",
	})
	if err != nil {
		t.Fatalf("SendText() error = %v", err)
	}
	if result.RemoteMessageID != "transport-request-id" || !result.EchoExpected || !result.AcceptedAt.IsZero() {
		t.Fatalf("result = %+v, want stable TmpID, echo expected, and zero AcceptedAt", result)
	}
	if fake.conversationID != "remote-conversation" || fake.conversationCalls != 1 {
		t.Fatalf("GetConversation = (%q, %d calls), want (remote-conversation, 1 call)",
			fake.conversationID, fake.conversationCalls)
	}
	if fake.sendCalls != 1 || fake.sent == nil {
		t.Fatalf("SendMessage = (%d calls, payload %p), want one non-nil payload", fake.sendCalls, fake.sent)
	}
	payload := fake.sent
	if payload.GetTmpID() != "transport-request-id" ||
		payload.GetMessagePayload().GetTmpID() != "transport-request-id" ||
		payload.GetMessagePayload().GetTmpID2() != "transport-request-id" {
		t.Fatalf("payload TmpIDs = (%q, %q, %q), want transport-request-id in all positions",
			payload.GetTmpID(), payload.GetMessagePayload().GetTmpID(), payload.GetMessagePayload().GetTmpID2())
	}
	if payload.GetConversationID() != "remote-conversation" ||
		payload.GetMessagePayload().GetConversationID() != "remote-conversation" {
		t.Fatalf("payload conversation IDs = (%q, %q), want remote-conversation",
			payload.GetConversationID(), payload.GetMessagePayload().GetConversationID())
	}
	if payload.GetMessagePayload().GetParticipantID() != "+15551234567" || payload.GetSIMPayload() != sim {
		t.Fatalf("payload routing = participant %q SIM %p, want +15551234567 and %p",
			payload.GetMessagePayload().GetParticipantID(), payload.GetSIMPayload(), sim)
	}
	infos := payload.GetMessagePayload().GetMessageInfo()
	if len(infos) != 1 || infos[0].GetMessageContent().GetContent() != "hello from the durable outbox" {
		t.Fatalf("payload MessageInfo = %+v, want request body", infos)
	}
	if payload.GetReply().GetMessageID() != "reply-id" {
		t.Fatalf("payload reply ID = %q, want reply-id", payload.GetReply().GetMessageID())
	}
}

func TestSendTextConversationFailuresAreClassifiedNotCalled(t *testing.T) {
	tests := []struct {
		name        string
		fake        *fakeTextSendClient
		wantClass   bridge.FailureClass
		fingerprint string
	}{
		{
			name: "transport error",
			fake: &fakeTextSendClient{
				conversationErr: errors.New("conversation unavailable"),
			},
			wantClass:   bridge.FailureTransient,
			fingerprint: "google_conversation_get_failed",
		},
		{
			name:        "nil conversation",
			fake:        &fakeTextSendClient{},
			wantClass:   bridge.FailureTransient,
			fingerprint: "google_conversation_get_failed",
		},
		{
			name: "auth expiry",
			fake: &fakeTextSendClient{
				conversationErr: errors.New("HTTP 401: invalid authentication credentials"),
			},
			wantClass:   bridge.FailureCredentialsExpired,
			fingerprint: "google_auth_expired",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			a := newTextSendTestAdapter(t, test.fake)
			_, err := a.SendText(context.Background(), bridge.TextRequest{
				Conversation: bridge.ConversationRef{RemoteID: "conversation-id"},
				Body:         "hello",
				RequestID:    "request-id",
			})
			failure := requireTextOpError(t, err)
			if failure.Class != test.wantClass || failure.Dispatch != bridge.DispatchNotCalled ||
				failure.Operation != "send_text" || failure.Fingerprint != test.fingerprint {
				t.Fatalf("failure = %+v, want class %q pre-call %s", failure, test.wantClass, test.fingerprint)
			}
			if test.fake.sendCalls != 0 {
				t.Fatalf("SendMessage calls = %d, want 0", test.fake.sendCalls)
			}
		})
	}
}

func TestSendTextSendFailuresAreClassifiedUncertain(t *testing.T) {
	tests := []struct {
		name string
		fake *fakeTextSendClient
	}{
		{
			name: "transport error",
			fake: &fakeTextSendClient{
				conversationResult: &gmproto.Conversation{},
				sendErr:            errors.New("transport timeout"),
			},
		},
		{
			name: "nil response",
			fake: &fakeTextSendClient{
				conversationResult: &gmproto.Conversation{},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			a := newTextSendTestAdapter(t, test.fake)
			_, err := a.SendText(context.Background(), bridge.TextRequest{
				Conversation: bridge.ConversationRef{RemoteID: "conversation-id"},
				Body:         "hello",
				RequestID:    "request-id",
			})
			failure := requireTextOpError(t, err)
			if failure.Class != bridge.FailureTransient || failure.Dispatch != "" ||
				failure.Operation != "send_text" || failure.Fingerprint != "google_text_send_failed" {
				t.Fatalf("failure = %+v, want ambiguous transient google_text_send_failed", failure)
			}
			if test.fake.sendCalls != 1 {
				t.Fatalf("SendMessage calls = %d, want 1", test.fake.sendCalls)
			}
		})
	}
}

func TestSendTextTransientAndRejectedFailuresDoNotRetireGeneration(t *testing.T) {
	tests := []struct {
		name        string
		fake        *fakeTextSendClient
		dispatch    bridge.DispatchCertainty
		fingerprint string
	}{
		{
			name: "ambiguous transport error",
			fake: &fakeTextSendClient{
				conversationResult: &gmproto.Conversation{},
				sendErr:            errors.New("transport timeout"),
			},
			dispatch:    "",
			fingerprint: "google_text_send_failed",
		},
		{
			name: "nil response",
			fake: &fakeTextSendClient{
				conversationResult: &gmproto.Conversation{},
			},
			dispatch:    "",
			fingerprint: "google_text_send_failed",
		},
		{
			name: "rejected status",
			fake: &fakeTextSendClient{
				conversationResult: &gmproto.Conversation{},
				sendResult: &gmproto.SendMessageResponse{
					Status: gmproto.SendMessageResponse_UNKNOWN,
				},
			},
			dispatch:    bridge.DispatchNotCalled,
			fingerprint: "google_text_send_rejected",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			host := newTestApp(t)
			transport := &fakeTransport{}
			a := newTestAdapter(t, host, transport)
			run, err := a.Start(context.Background(), bridge.StartRequest{
				AccountID:  "google-primary",
				Generation: 1,
			}, nil)
			if err != nil {
				t.Fatalf("Start() error = %v", err)
			}
			transport.emit(&gmproto.Conversation{ConversationID: "ready"})
			<-run.Ready()

			installTextSendClient(t, host.GetClient(), test.fake)
			_, err = a.SendText(context.Background(), bridge.TextRequest{
				Conversation: bridge.ConversationRef{RemoteID: "conversation-id"},
				Body:         "hello",
				RequestID:    "request-id",
			})
			failure := requireTextOpError(t, err)
			if failure.Class != bridge.FailureTransient || failure.Dispatch != test.dispatch ||
				failure.Operation != "send_text" || failure.Fingerprint != test.fingerprint {
				t.Fatalf("failure = %+v, want transient %q dispatch %q", failure, test.fingerprint, test.dispatch)
			}
			if !host.Connected.Load() {
				t.Fatal("text send failure marked the connected receive generation lost")
			}
			select {
			case terminal := <-run.Done():
				t.Fatalf("text send failure retired the receive generation with %v", terminal)
			default:
			}
			if host.GoogleStatus().NeedsRepair {
				t.Fatal("text send failure parked the Google lifecycle")
			}
		})
	}
}

func TestSendTextAuthExpiryStillReportsToLifecycle(t *testing.T) {
	host := newTestApp(t)
	transport := &fakeTransport{}
	a := newTestAdapter(t, host, transport)
	run, err := a.Start(context.Background(), bridge.StartRequest{
		AccountID:  "google-primary",
		Generation: 1,
	}, nil)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	transport.emit(&gmproto.Conversation{ConversationID: "ready"})
	<-run.Ready()

	fake := &fakeTextSendClient{
		conversationResult: &gmproto.Conversation{},
		sendErr:            errors.New("google returned http 401 for send"),
	}
	installTextSendClient(t, host.GetClient(), fake)

	_, err = a.SendText(context.Background(), bridge.TextRequest{
		Conversation: bridge.ConversationRef{RemoteID: "conversation-id"},
		Body:         "hello",
		RequestID:    "request-id",
	})
	failure := requireTextOpError(t, err)
	if failure.Fingerprint != "google_auth_expired" {
		t.Fatalf("failure = %+v, want google_auth_expired classification", failure)
	}
	select {
	case terminal := <-run.Done():
		terminalFailure, ok := asOpError(terminal)
		if !ok || (terminalFailure.Class != bridge.FailureCredentialsExpired &&
			terminalFailure.Class != bridge.FailureUpgradeRequired) {
			t.Fatalf("terminal = %v, want credentials_expired/upgrade_required", terminal)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("auth-expired text send failure did not reach the lifecycle owner")
	}
}

func TestSendReactionNotConnectedIsClassifiedPreCall(t *testing.T) {
	host := newTestApp(t)
	legacy := newLegacyClient(t)
	generation := host.BeginGoogleGeneration(legacy)
	t.Cleanup(generation.Release)
	fake := &fakeReactionSendClient{}
	installReactionSendClient(t, legacy, fake)
	a := New("google-primary", host, func() bool { return true })
	a.newClient = func() (*client.Client, transportClient, error) {
		t.Fatal("SendReaction must not construct a Google transport")
		return nil, nil, nil
	}

	result, err := a.SendReaction(context.Background(), bridge.ReactionRequest{
		AccountID: "google-primary",
		Conversation: bridge.ConversationRef{
			RemoteID: "conversation-id",
		},
		Target: bridge.MessageRef{RemoteID: "target-id"},
		Emoji:  "👍",
		Action: bridge.ReactionAdd,
	})
	if result != (bridge.SendResult{}) {
		t.Fatalf("result = %+v, want zero value", result)
	}
	failure := requireReactionOpError(t, err)
	if failure.Class != bridge.FailureTransient ||
		failure.Dispatch != bridge.DispatchNotCalled ||
		failure.Operation != "send_reaction" ||
		failure.Fingerprint != "google_not_connected" {
		t.Fatalf("failure = %+v, want transient pre-call google_not_connected", failure)
	}
	if fake.conversationCalls != 0 || fake.sendCalls != 0 {
		t.Fatalf("transport calls = (conversation %d, send %d), want (0, 0)",
			fake.conversationCalls, fake.sendCalls)
	}
}

func TestSendReactionContextGuardsAreClassifiedPreCall(t *testing.T) {
	tests := []struct {
		name        string
		ctx         context.Context
		fingerprint string
	}{
		{
			name:        "nil",
			ctx:         nil,
			fingerprint: "google_reaction_context_invalid",
		},
		{
			name: "done",
			ctx: func() context.Context {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx
			}(),
			fingerprint: "google_reaction_context_done",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := &fakeReactionSendClient{}
			a := newReactionSendTestAdapter(t, fake)

			_, err := a.SendReaction(test.ctx, bridge.ReactionRequest{})
			failure := requireReactionOpError(t, err)
			if failure.Class != bridge.FailureTransient ||
				failure.Dispatch != bridge.DispatchNotCalled ||
				failure.Operation != "send_reaction" ||
				failure.Fingerprint != test.fingerprint {
				t.Fatalf("failure = %+v, want transient pre-call %s", failure, test.fingerprint)
			}
			if fake.conversationCalls != 0 || fake.sendCalls != 0 {
				t.Fatalf("transport calls = (conversation %d, send %d), want (0, 0)",
					fake.conversationCalls, fake.sendCalls)
			}
		})
	}
}

func TestSendReactionSuccessMapsNativeActionsAndReturnsEmptyResult(t *testing.T) {
	tests := []struct {
		name       string
		action     bridge.ReactionAction
		wantAction gmproto.SendReactionRequest_Action
	}{
		{name: "add", action: bridge.ReactionAdd, wantAction: gmproto.SendReactionRequest_ADD},
		{name: "remove", action: bridge.ReactionRemove, wantAction: gmproto.SendReactionRequest_REMOVE},
		{name: "switch", action: bridge.ReactionSwitch, wantAction: gmproto.SendReactionRequest_SWITCH},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			sim := &gmproto.SIMPayload{Two: 7, SIMNumber: 2}
			fake := &fakeReactionSendClient{
				conversationResult: &gmproto.Conversation{
					ConversationID: "remote-conversation",
					Participants: []*gmproto.Participant{{
						ID:         &gmproto.SmallInfo{Number: "+15551234567"},
						IsMe:       true,
						SimPayload: sim,
					}},
				},
				sendResult: &gmproto.SendReactionResponse{Success: true},
			}
			a := newReactionSendTestAdapter(t, fake)

			result, err := a.SendReaction(context.Background(), bridge.ReactionRequest{
				AccountID: "google-primary",
				Conversation: bridge.ConversationRef{
					RemoteID: "remote-conversation",
				},
				// Empty AuthorID is the cross-platform self-target representation.
				// Google routes reactions by remote message ID and SIM, so it must
				// neither reject nor invent an author identifier for this case.
				Target: bridge.MessageRef{RemoteID: "target-message-id", AuthorID: ""},
				Emoji:  "🫡",
				Action: test.action,
			})
			if err != nil {
				t.Fatalf("SendReaction() error = %v", err)
			}
			if result != (bridge.SendResult{}) {
				t.Fatalf("result = %+v, want empty result for ConfirmWithoutResult", result)
			}
			if fake.conversationCalls != 1 || fake.conversationID != "remote-conversation" {
				t.Fatalf("GetConversation = (%d calls, %q), want (1, remote-conversation)",
					fake.conversationCalls, fake.conversationID)
			}
			if fake.sendCalls != 1 || fake.sent == nil {
				t.Fatalf("SendReaction = (%d calls, payload %p), want one non-nil payload",
					fake.sendCalls, fake.sent)
			}
			if fake.sent.GetMessageID() != "target-message-id" ||
				fake.sent.GetReactionData().GetUnicode() != "🫡" ||
				fake.sent.GetAction() != test.wantAction ||
				fake.sent.GetSIMPayload() != sim {
				t.Fatalf("payload = %+v, want target/emoji/action %s/original SIM", fake.sent, test.wantAction)
			}
		})
	}
}

func TestSendReactionConversationFailuresAreClassifiedNotCalled(t *testing.T) {
	tests := []struct {
		name string
		fake *fakeReactionSendClient
	}{
		{
			name: "transport error",
			fake: &fakeReactionSendClient{
				conversationErr: errors.New("conversation unavailable"),
			},
		},
		{
			name: "nil conversation",
			fake: &fakeReactionSendClient{},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			a := newReactionSendTestAdapter(t, test.fake)
			_, err := a.SendReaction(context.Background(), bridge.ReactionRequest{
				Conversation: bridge.ConversationRef{RemoteID: "conversation-id"},
				Target:       bridge.MessageRef{RemoteID: "target-id"},
				Emoji:        "👍",
				Action:       bridge.ReactionAdd,
			})
			failure := requireReactionOpError(t, err)
			if failure.Class != bridge.FailureTransient || failure.Dispatch != bridge.DispatchNotCalled ||
				failure.Operation != "send_reaction" || failure.Fingerprint != "google_conversation_get_failed" {
				t.Fatalf("failure = %+v, want transient pre-call google_conversation_get_failed", failure)
			}
			if test.fake.sendCalls != 0 {
				t.Fatalf("SendReaction calls = %d, want 0", test.fake.sendCalls)
			}
		})
	}
}

func TestSendReactionSendFailureIsClassifiedUncertain(t *testing.T) {
	fake := &fakeReactionSendClient{
		conversationResult: &gmproto.Conversation{},
		sendErr:            errors.New("transport timeout"),
	}
	a := newReactionSendTestAdapter(t, fake)

	_, err := a.SendReaction(context.Background(), bridge.ReactionRequest{
		Conversation: bridge.ConversationRef{RemoteID: "conversation-id"},
		Target:       bridge.MessageRef{RemoteID: "target-id"},
		Emoji:        "👍",
		Action:       bridge.ReactionAdd,
	})
	failure := requireReactionOpError(t, err)
	if failure.Class != bridge.FailureTransient || failure.Dispatch != "" ||
		failure.Operation != "send_reaction" || failure.Fingerprint != "google_reaction_send_failed" {
		t.Fatalf("failure = %+v, want ambiguous transient google_reaction_send_failed", failure)
	}
	if fake.sendCalls != 1 {
		t.Fatalf("SendReaction calls = %d, want 1", fake.sendCalls)
	}
}

func TestSendReactionRejectedIsClassifiedNotCalled(t *testing.T) {
	tests := []struct {
		name     string
		response *gmproto.SendReactionResponse
	}{
		{name: "success false", response: &gmproto.SendReactionResponse{}},
		{name: "nil response", response: nil},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := &fakeReactionSendClient{
				conversationResult: &gmproto.Conversation{},
				sendResult:         test.response,
			}
			a := newReactionSendTestAdapter(t, fake)

			_, err := a.SendReaction(context.Background(), bridge.ReactionRequest{
				Conversation: bridge.ConversationRef{RemoteID: "conversation-id"},
				Target:       bridge.MessageRef{RemoteID: "target-id"},
				Emoji:        "👍",
				Action:       bridge.ReactionAdd,
			})
			failure := requireReactionOpError(t, err)
			if failure.Class != bridge.FailureTransient || failure.Dispatch != bridge.DispatchNotCalled ||
				failure.Operation != "send_reaction" || failure.Fingerprint != "google_reaction_rejected" {
				t.Fatalf("failure = %+v, want transient pre-call google_reaction_rejected", failure)
			}
		})
	}
}

func TestSendReactionTransientAndRejectedFailuresDoNotRetireGeneration(t *testing.T) {
	tests := []struct {
		name        string
		fake        *fakeReactionSendClient
		dispatch    bridge.DispatchCertainty
		fingerprint string
	}{
		{
			name: "ambiguous transport error",
			fake: &fakeReactionSendClient{
				conversationResult: &gmproto.Conversation{},
				sendErr:            errors.New("transport timeout"),
			},
			dispatch:    "",
			fingerprint: "google_reaction_send_failed",
		},
		{
			name: "rejected response",
			fake: &fakeReactionSendClient{
				conversationResult: &gmproto.Conversation{},
				sendResult:         &gmproto.SendReactionResponse{},
			},
			dispatch:    bridge.DispatchNotCalled,
			fingerprint: "google_reaction_rejected",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			host := newTestApp(t)
			transport := &fakeTransport{}
			a := newTestAdapter(t, host, transport)
			run, err := a.Start(context.Background(), bridge.StartRequest{
				AccountID:  "google-primary",
				Generation: 1,
			}, nil)
			if err != nil {
				t.Fatalf("Start() error = %v", err)
			}
			t.Cleanup(func() { stopRun(t, run) })
			transport.emit(&gmproto.Conversation{ConversationID: "ready"})
			<-run.Ready()

			installReactionSendClient(t, host.GetClient(), test.fake)
			_, err = a.SendReaction(context.Background(), bridge.ReactionRequest{
				Conversation: bridge.ConversationRef{RemoteID: "conversation-id"},
				Target:       bridge.MessageRef{RemoteID: "target-id"},
				Emoji:        "👍",
				Action:       bridge.ReactionAdd,
			})
			failure := requireReactionOpError(t, err)
			if failure.Class != bridge.FailureTransient || failure.Dispatch != test.dispatch ||
				failure.Operation != "send_reaction" || failure.Fingerprint != test.fingerprint {
				t.Fatalf("failure = %+v, want transient %q dispatch %q", failure, test.fingerprint, test.dispatch)
			}
			if !host.Connected.Load() {
				t.Fatal("reaction failure marked the connected receive generation lost")
			}
			select {
			case terminal := <-run.Done():
				t.Fatalf("reaction failure retired the receive generation with %v", terminal)
			default:
			}
			if host.GoogleStatus().NeedsRepair {
				t.Fatal("reaction failure parked the Google lifecycle")
			}
		})
	}
}

func TestSendReactionAuthExpiryStillReportsToLifecycle(t *testing.T) {
	host := newTestApp(t)
	transport := &fakeTransport{}
	a := newTestAdapter(t, host, transport)
	run, err := a.Start(context.Background(), bridge.StartRequest{
		AccountID:  "google-primary",
		Generation: 1,
	}, nil)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	transport.emit(&gmproto.Conversation{ConversationID: "ready"})
	<-run.Ready()

	fake := &fakeReactionSendClient{
		conversationResult: &gmproto.Conversation{},
		sendErr:            errors.New("google returned http 401 for reaction send"),
	}
	installReactionSendClient(t, host.GetClient(), fake)

	_, err = a.SendReaction(context.Background(), bridge.ReactionRequest{
		Conversation: bridge.ConversationRef{RemoteID: "conversation-id"},
		Target:       bridge.MessageRef{RemoteID: "target-id"},
		Emoji:        "👍",
		Action:       bridge.ReactionAdd,
	})
	failure := requireReactionOpError(t, err)
	if failure.Fingerprint != "google_auth_expired" || failure.Dispatch != "" {
		t.Fatalf("failure = %+v, want ambiguous google_auth_expired classification", failure)
	}
	select {
	case terminal := <-run.Done():
		terminalFailure, ok := asOpError(terminal)
		if !ok || (terminalFailure.Class != bridge.FailureCredentialsExpired &&
			terminalFailure.Class != bridge.FailureUpgradeRequired) {
			t.Fatalf("terminal = %v, want credentials_expired/upgrade_required", terminal)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("auth-expired reaction failure did not reach the lifecycle owner")
	}
}

func TestSendMediaNotConnectedIsClassifiedPreCall(t *testing.T) {
	host := newTestApp(t)
	a := New("google-primary", host, func() bool { return true })
	a.newClient = func() (*client.Client, transportClient, error) {
		t.Fatal("SendMedia must not construct a Google transport")
		return nil, nil, nil
	}

	result, err := a.SendMedia(context.Background(), bridge.MediaRequest{
		AccountID: "google-primary",
		Reader:    panicReader{},
		Size:      1,
	})
	if result != (bridge.SendResult{}) {
		t.Fatalf("result = %+v, want zero value", result)
	}
	failure := requireMediaOpError(t, err)
	if failure.Class != bridge.FailureTransient ||
		failure.Dispatch != bridge.DispatchNotCalled ||
		failure.Operation != "send_media" ||
		failure.Fingerprint != "google_not_connected" {
		t.Fatalf("failure = %+v, want transient pre-call google_not_connected", failure)
	}
}

func TestSendMediaInstalledClientBeforeReadyIsClassifiedPreCall(t *testing.T) {
	host := newTestApp(t)
	legacy := newLegacyClient(t)
	generation := host.BeginGoogleGeneration(legacy)
	t.Cleanup(generation.Release)
	fake := &fakeMediaSendClient{}
	installMediaSendClient(t, legacy, fake)
	a := New("google-primary", host, func() bool { return true })

	_, err := a.SendMedia(context.Background(), bridge.MediaRequest{
		Reader: strings.NewReader("x"),
		Size:   1,
	})
	failure := requireMediaOpError(t, err)
	if failure.Class != bridge.FailureTransient ||
		failure.Dispatch != bridge.DispatchNotCalled ||
		failure.Operation != "send_media" ||
		failure.Fingerprint != "google_not_connected" {
		t.Fatalf("failure = %+v, want transient pre-call google_not_connected", failure)
	}
	if fake.uploadCalls != 0 {
		t.Fatalf("UploadMedia calls = %d, want 0 before lifecycle readiness", fake.uploadCalls)
	}
}

func TestSendMediaSuccessUsesStablePayloadAndMapsResult(t *testing.T) {
	media := &gmproto.MediaContent{
		Format:    gmproto.MediaFormats_IMAGE_PNG,
		MediaID:   "uploaded-media-id",
		MediaName: "photo.png",
		MimeType:  "image/png",
		Size:      4,
	}
	sim := &gmproto.SIMPayload{Two: 7, SIMNumber: 2}
	conversation := &gmproto.Conversation{
		ConversationID: "remote-conversation",
		Participants: []*gmproto.Participant{{
			ID:         &gmproto.SmallInfo{Number: "+15551234567"},
			IsMe:       true,
			SimPayload: sim,
		}},
	}
	fake := &fakeMediaSendClient{
		uploadResult:       media,
		conversationResult: conversation,
		sendResults: []*gmproto.SendMessageResponse{{
			Status: gmproto.SendMessageResponse_SUCCESS,
		}},
	}
	a := newMediaSendTestAdapter(t, fake)

	result, err := a.SendMedia(context.Background(), bridge.MediaRequest{
		AccountID: "google-primary",
		Conversation: bridge.ConversationRef{
			RemoteID: "remote-conversation",
		},
		Reader:    bytes.NewBufferString("data"),
		Size:      4,
		Filename:  "photo.png",
		MIME:      "image/png",
		RequestID: "transport-request-id",
	})
	if err != nil {
		t.Fatalf("SendMedia() error = %v", err)
	}
	if result.RemoteMessageID != "transport-request-id" || !result.EchoExpected || !result.AcceptedAt.IsZero() {
		t.Fatalf("result = %+v, want stable TmpID, echo expected, and zero AcceptedAt", result)
	}
	if got := string(fake.uploadData); got != "data" {
		t.Fatalf("UploadMedia data = %q, want data", got)
	}
	if fake.uploadFilename != "photo.png" || fake.uploadMIME != "image/png" {
		t.Fatalf("UploadMedia metadata = (%q, %q), want (photo.png, image/png)", fake.uploadFilename, fake.uploadMIME)
	}
	if fake.conversationID != "remote-conversation" {
		t.Fatalf("GetConversation ID = %q, want remote-conversation", fake.conversationID)
	}
	if len(fake.sent) != 1 {
		t.Fatalf("SendMessage calls = %d, want 1", len(fake.sent))
	}
	payload := fake.sent[0]
	if payload.GetTmpID() != "transport-request-id" ||
		payload.GetMessagePayload().GetTmpID() != "transport-request-id" ||
		payload.GetMessagePayload().GetTmpID2() != "transport-request-id" {
		t.Fatalf("payload TmpIDs = (%q, %q, %q), want transport-request-id in all positions",
			payload.GetTmpID(), payload.GetMessagePayload().GetTmpID(), payload.GetMessagePayload().GetTmpID2())
	}
	if payload.GetMessagePayload().GetParticipantID() != "+15551234567" || payload.GetSIMPayload() != sim {
		t.Fatalf("payload routing = participant %q SIM %p, want +15551234567 and %p",
			payload.GetMessagePayload().GetParticipantID(), payload.GetSIMPayload(), sim)
	}
	infos := payload.GetMessagePayload().GetMessageInfo()
	if len(infos) != 1 || infos[0].GetMediaContent() != media {
		t.Fatalf("media payload = %+v, want uploaded media pointer %p", infos, media)
	}
}

func TestSendMediaCaptionUsesStableFollowUpTextPayload(t *testing.T) {
	sim := &gmproto.SIMPayload{Two: 1, SIMNumber: 1}
	fake := &fakeMediaSendClient{
		uploadResult: &gmproto.MediaContent{MediaID: "media-id"},
		conversationResult: &gmproto.Conversation{Participants: []*gmproto.Participant{{
			ID:         &gmproto.SmallInfo{Number: "+15557654321"},
			IsMe:       true,
			SimPayload: sim,
		}}},
		sendResults: []*gmproto.SendMessageResponse{
			{Status: gmproto.SendMessageResponse_SUCCESS},
			{Status: gmproto.SendMessageResponse_SUCCESS},
		},
	}
	a := newMediaSendTestAdapter(t, fake)

	result, err := a.SendMedia(context.Background(), bridge.MediaRequest{
		Conversation: bridge.ConversationRef{RemoteID: "conversation-id"},
		Reader:       strings.NewReader("x"),
		Size:         1,
		Caption:      "  a useful caption  ",
		ReplyTo:      &bridge.MessageRef{RemoteID: "reply-id"},
		RequestID:    "media-request-id",
	})
	if err != nil {
		t.Fatalf("SendMedia() error = %v", err)
	}
	if result.RemoteMessageID != "media-request-id" {
		t.Fatalf("RemoteMessageID = %q, want media-request-id", result.RemoteMessageID)
	}
	if len(fake.sent) != 2 {
		t.Fatalf("SendMessage calls = %d, want media plus caption", len(fake.sent))
	}
	caption := fake.sent[1]
	if caption.GetTmpID() != "media-request-id:caption" ||
		caption.GetMessagePayload().GetTmpID() != "media-request-id:caption" ||
		caption.GetMessagePayload().GetTmpID2() != "media-request-id:caption" {
		t.Fatalf("caption TmpIDs = (%q, %q, %q), want stable :caption suffix",
			caption.GetTmpID(), caption.GetMessagePayload().GetTmpID(), caption.GetMessagePayload().GetTmpID2())
	}
	infos := caption.GetMessagePayload().GetMessageInfo()
	if len(infos) != 1 || infos[0].GetMessageContent().GetContent() != "a useful caption" {
		t.Fatalf("caption MessageInfo = %+v, want trimmed caption", infos)
	}
	if caption.GetReply().GetMessageID() != "reply-id" {
		t.Fatalf("caption reply ID = %q, want reply-id", caption.GetReply().GetMessageID())
	}
}

func TestSendMediaBoundsAndLengthChecksReaderBeforeUpload(t *testing.T) {
	tests := []struct {
		name        string
		reader      io.Reader
		size        int64
		fingerprint string
	}{
		{
			name:        "reader error",
			reader:      errorReader{err: errors.New("blob read failed")},
			size:        4,
			fingerprint: "google_media_read_failed",
		},
		{
			name:        "short reader",
			reader:      strings.NewReader("abc"),
			size:        4,
			fingerprint: "google_media_size_mismatch",
		},
		{
			name:        "long reader",
			reader:      strings.NewReader("abcde"),
			size:        4,
			fingerprint: "google_media_size_mismatch",
		},
		{
			name:        "nil reader",
			reader:      nil,
			size:        4,
			fingerprint: "google_media_read_failed",
		},
		{
			name:        "negative size",
			reader:      strings.NewReader(""),
			size:        -1,
			fingerprint: "google_media_size_mismatch",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := &fakeMediaSendClient{}
			a := newMediaSendTestAdapter(t, fake)
			_, err := a.SendMedia(context.Background(), bridge.MediaRequest{
				Reader: test.reader,
				Size:   test.size,
			})
			failure := requireMediaOpError(t, err)
			if failure.Class != bridge.FailureTransient || failure.Dispatch != bridge.DispatchNotCalled ||
				failure.Operation != "send_media" || failure.Fingerprint != test.fingerprint {
				t.Fatalf("failure = %+v, want transient pre-call %s", failure, test.fingerprint)
			}
			if fake.uploadCalls != 0 {
				t.Fatalf("UploadMedia calls = %d, want 0", fake.uploadCalls)
			}
		})
	}

	reader := strings.NewReader("abcdef")
	fake := &fakeMediaSendClient{}
	a := newMediaSendTestAdapter(t, fake)
	_, _ = a.SendMedia(context.Background(), bridge.MediaRequest{Reader: reader, Size: 3})
	if reader.Len() != 2 {
		t.Fatalf("reader remaining bytes = %d, want 2 after bounded Size+1 slurp", reader.Len())
	}
}

func TestSendMediaPreDispatchTransportFailuresAreClassifiedNotCalled(t *testing.T) {
	tests := []struct {
		name        string
		configure   func(*fakeMediaSendClient)
		wantClass   bridge.FailureClass
		fingerprint string
	}{
		{
			name: "upload",
			configure: func(fake *fakeMediaSendClient) {
				fake.uploadErr = errors.New("upload unavailable")
			},
			wantClass:   bridge.FailureTransient,
			fingerprint: "google_media_upload_failed",
		},
		{
			name:        "nil upload result",
			configure:   func(*fakeMediaSendClient) {},
			wantClass:   bridge.FailureTransient,
			fingerprint: "google_media_upload_failed",
		},
		{
			name: "nil conversation result",
			configure: func(fake *fakeMediaSendClient) {
				fake.uploadResult = &gmproto.MediaContent{MediaID: "media-id"}
			},
			wantClass:   bridge.FailureTransient,
			fingerprint: "google_conversation_get_failed",
		},
		{
			name: "conversation auth expiry",
			configure: func(fake *fakeMediaSendClient) {
				fake.uploadResult = &gmproto.MediaContent{MediaID: "media-id"}
				fake.conversationErr = errors.New("HTTP 401: invalid authentication credentials")
			},
			wantClass:   bridge.FailureCredentialsExpired,
			fingerprint: "google_auth_expired",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := &fakeMediaSendClient{}
			test.configure(fake)
			a := newMediaSendTestAdapter(t, fake)
			_, err := a.SendMedia(context.Background(), bridge.MediaRequest{
				Conversation: bridge.ConversationRef{RemoteID: "conversation-id"},
				Reader:       strings.NewReader("x"),
				Size:         1,
			})
			failure := requireMediaOpError(t, err)
			if failure.Class != test.wantClass || failure.Dispatch != bridge.DispatchNotCalled ||
				failure.Operation != "send_media" || failure.Fingerprint != test.fingerprint {
				t.Fatalf("failure = %+v, want class %q pre-call %s", failure, test.wantClass, test.fingerprint)
			}
			if len(fake.sent) != 0 {
				t.Fatalf("SendMessage calls = %d, want 0", len(fake.sent))
			}
		})
	}
}

// A transient media send failure must never retire the receive generation (the
// C4/C5/C6 lesson): the outbox retries every few seconds, and reporting each
// attempt would bounce a healthy Google connection indefinitely — the
// over-reconnect throttle vector the runbook warns about.
func TestSendMediaTransientFailuresDoNotRetireGeneration(t *testing.T) {
	tests := []struct {
		name string
		fake *fakeMediaSendClient
	}{
		{
			name: "ambiguous transport error",
			fake: &fakeMediaSendClient{
				uploadResult:       &gmproto.MediaContent{MediaID: "media-id"},
				conversationResult: &gmproto.Conversation{},
				sendErrors:         []error{errors.New("transport timeout")},
			},
		},
		{
			name: "nil send response",
			fake: &fakeMediaSendClient{
				uploadResult:       &gmproto.MediaContent{MediaID: "media-id"},
				conversationResult: &gmproto.Conversation{},
				sendResults:        []*gmproto.SendMessageResponse{nil},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			host := newTestApp(t)
			transport := &fakeTransport{}
			a := newTestAdapter(t, host, transport)
			run, err := a.Start(context.Background(), bridge.StartRequest{
				AccountID:  "google-primary",
				Generation: 1,
			}, nil)
			if err != nil {
				t.Fatalf("Start() error = %v", err)
			}
			transport.emit(&gmproto.Conversation{ConversationID: "ready"})
			<-run.Ready()

			installMediaSendClient(t, host.GetClient(), test.fake)

			_, err = a.SendMedia(context.Background(), bridge.MediaRequest{
				Conversation: bridge.ConversationRef{RemoteID: "conversation-id"},
				Reader:       strings.NewReader("x"),
				Size:         1,
				RequestID:    "request-id",
			})
			failure := requireMediaOpError(t, err)
			if failure.Class != bridge.FailureTransient || failure.Dispatch != "" ||
				failure.Operation != "send_media" || failure.Fingerprint != "google_media_send_failed" {
				t.Fatalf("failure = %+v, want ambiguous transient google_media_send_failed", failure)
			}
			select {
			case terminal := <-run.Done():
				t.Fatalf("transient media send failure retired the receive generation with %v", terminal)
			default:
			}
			if host.GoogleStatus().NeedsRepair {
				t.Fatal("transient media send failure parked the Google lifecycle")
			}
		})
	}
}

// An auth-indicting media send failure must still reach the lifecycle owner so
// the supervisor can route to credential repair — the C4 notify-on-auth-expiry
// contract, preserved from every send path.
func TestSendMediaAuthExpiryStillReportsToLifecycle(t *testing.T) {
	host := newTestApp(t)
	transport := &fakeTransport{}
	a := newTestAdapter(t, host, transport)
	run, err := a.Start(context.Background(), bridge.StartRequest{
		AccountID:  "google-primary",
		Generation: 1,
	}, nil)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	transport.emit(&gmproto.Conversation{ConversationID: "ready"})
	<-run.Ready()

	fake := &fakeMediaSendClient{
		uploadResult:       &gmproto.MediaContent{MediaID: "media-id"},
		conversationResult: &gmproto.Conversation{},
		sendErrors:         []error{errors.New("google returned http 401 for send")},
	}
	installMediaSendClient(t, host.GetClient(), fake)

	_, err = a.SendMedia(context.Background(), bridge.MediaRequest{
		Conversation: bridge.ConversationRef{RemoteID: "conversation-id"},
		Reader:       strings.NewReader("x"),
		Size:         1,
		RequestID:    "request-id",
	})
	failure := requireMediaOpError(t, err)
	if failure.Fingerprint != "google_auth_expired" {
		t.Fatalf("failure = %+v, want google_auth_expired classification", failure)
	}
	select {
	case terminal := <-run.Done():
		terminalFailure, ok := asOpError(terminal)
		if !ok || (terminalFailure.Class != bridge.FailureCredentialsExpired &&
			terminalFailure.Class != bridge.FailureUpgradeRequired) {
			t.Fatalf("terminal = %v, want credentials_expired/upgrade_required", terminal)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("auth-expired media send failure did not reach the lifecycle owner")
	}
}

func TestSendMediaRejectedStatusIsClassifiedNotCalled(t *testing.T) {
	fake := &fakeMediaSendClient{
		uploadResult:       &gmproto.MediaContent{MediaID: "media-id"},
		conversationResult: &gmproto.Conversation{},
		sendResults: []*gmproto.SendMessageResponse{{
			Status: gmproto.SendMessageResponse_UNKNOWN,
		}},
	}
	a := newMediaSendTestAdapter(t, fake)

	_, err := a.SendMedia(context.Background(), bridge.MediaRequest{
		Conversation: bridge.ConversationRef{RemoteID: "conversation-id"},
		Reader:       strings.NewReader("x"),
		Size:         1,
		RequestID:    "request-id",
	})
	failure := requireMediaOpError(t, err)
	if failure.Class != bridge.FailureTransient || failure.Dispatch != bridge.DispatchNotCalled ||
		failure.Operation != "send_media" || failure.Fingerprint != "google_media_send_rejected" {
		t.Fatalf("failure = %+v, want transient pre-call google_media_send_rejected", failure)
	}
}

func TestSendMediaCaptionFailureRemainsAmbiguousAfterMediaSuccess(t *testing.T) {
	tests := []struct {
		name        string
		sendResults []*gmproto.SendMessageResponse
		sendErrors  []error
		fingerprint string
	}{
		{
			name: "send error",
			sendResults: []*gmproto.SendMessageResponse{
				{Status: gmproto.SendMessageResponse_SUCCESS},
				nil,
			},
			sendErrors:  []error{nil, errors.New("caption timeout")},
			fingerprint: "google_caption_send_failed",
		},
		{
			name: "rejected status",
			sendResults: []*gmproto.SendMessageResponse{
				{Status: gmproto.SendMessageResponse_SUCCESS},
				{Status: gmproto.SendMessageResponse_UNKNOWN},
			},
			fingerprint: "google_caption_send_rejected",
		},
		{
			name: "nil response",
			sendResults: []*gmproto.SendMessageResponse{
				{Status: gmproto.SendMessageResponse_SUCCESS},
				nil,
			},
			fingerprint: "google_caption_send_failed",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := &fakeMediaSendClient{
				uploadResult:       &gmproto.MediaContent{MediaID: "media-id"},
				conversationResult: &gmproto.Conversation{},
				sendResults:        test.sendResults,
				sendErrors:         test.sendErrors,
			}
			a := newMediaSendTestAdapter(t, fake)
			_, err := a.SendMedia(context.Background(), bridge.MediaRequest{
				Conversation: bridge.ConversationRef{RemoteID: "conversation-id"},
				Reader:       strings.NewReader("x"),
				Size:         1,
				Caption:      "caption",
				RequestID:    "request-id",
			})
			failure := requireMediaOpError(t, err)
			if failure.Class != bridge.FailureTransient || failure.Dispatch != "" ||
				failure.Operation != "send_media" || failure.Fingerprint != test.fingerprint {
				t.Fatalf("failure = %+v, want ambiguous transient %s", failure, test.fingerprint)
			}
			if len(fake.sent) != 2 {
				t.Fatalf("SendMessage calls = %d, want media then caption", len(fake.sent))
			}
		})
	}
}

func newTextSendTestAdapter(t *testing.T, fake *fakeTextSendClient) *Adapter {
	t.Helper()
	host := newTestApp(t)
	legacy := newLegacyClient(t)
	generation := host.BeginGoogleGeneration(legacy)
	t.Cleanup(generation.Release)
	host.Connected.Store(true)
	installTextSendClient(t, legacy, fake)
	return New("google-primary", host, func() bool { return true })
}

func installTextSendClient(t *testing.T, want *client.Client, fake textSendClient) {
	t.Helper()
	previous := textSendClientFor
	textSendClientFor = func(got *client.Client) textSendClient {
		if got != want {
			t.Fatalf("text client source = %p, want App.GetClient() %p", got, want)
		}
		return fake
	}
	t.Cleanup(func() { textSendClientFor = previous })
}

func requireTextOpError(t *testing.T, err error) bridge.OpError {
	t.Helper()
	if err == nil {
		t.Fatal("SendText() error = nil, want classified failure")
	}
	failure, ok := asOpError(err)
	if !ok {
		t.Fatalf("SendText() error = %T %v, want bridge.OpError", err, err)
	}
	return failure
}

type fakeTextSendClient struct {
	conversationResult *gmproto.Conversation
	conversationErr    error
	sendResult         *gmproto.SendMessageResponse
	sendErr            error

	conversationCalls int
	conversationID    string
	sendCalls         int
	sent              *gmproto.SendMessageRequest
}

func (f *fakeTextSendClient) GetConversation(conversationID string) (*gmproto.Conversation, error) {
	f.conversationCalls++
	f.conversationID = conversationID
	return f.conversationResult, f.conversationErr
}

func (f *fakeTextSendClient) SendMessage(payload *gmproto.SendMessageRequest) (*gmproto.SendMessageResponse, error) {
	f.sendCalls++
	f.sent = payload
	return f.sendResult, f.sendErr
}

func newReactionSendTestAdapter(t *testing.T, fake *fakeReactionSendClient) *Adapter {
	t.Helper()
	host := newTestApp(t)
	legacy := newLegacyClient(t)
	generation := host.BeginGoogleGeneration(legacy)
	t.Cleanup(generation.Release)
	host.Connected.Store(true)
	installReactionSendClient(t, legacy, fake)
	return New("google-primary", host, func() bool { return true })
}

func installReactionSendClient(t *testing.T, want *client.Client, fake reactionSendClient) {
	t.Helper()
	previous := reactionSendClientFor
	reactionSendClientFor = func(got *client.Client) reactionSendClient {
		if got != want {
			t.Fatalf("reaction client source = %p, want App.GetClient() %p", got, want)
		}
		return fake
	}
	t.Cleanup(func() { reactionSendClientFor = previous })
}

func requireReactionOpError(t *testing.T, err error) bridge.OpError {
	t.Helper()
	if err == nil {
		t.Fatal("SendReaction() error = nil, want classified failure")
	}
	failure, ok := asOpError(err)
	if !ok {
		t.Fatalf("SendReaction() error = %T %v, want bridge.OpError", err, err)
	}
	return failure
}

type fakeReactionSendClient struct {
	conversationResult *gmproto.Conversation
	conversationErr    error
	sendResult         *gmproto.SendReactionResponse
	sendErr            error

	conversationCalls int
	conversationID    string
	sendCalls         int
	sent              *gmproto.SendReactionRequest
}

func (f *fakeReactionSendClient) GetConversation(conversationID string) (*gmproto.Conversation, error) {
	f.conversationCalls++
	f.conversationID = conversationID
	return f.conversationResult, f.conversationErr
}

func (f *fakeReactionSendClient) SendReaction(payload *gmproto.SendReactionRequest) (*gmproto.SendReactionResponse, error) {
	f.sendCalls++
	f.sent = payload
	return f.sendResult, f.sendErr
}

func newMediaSendTestAdapter(t *testing.T, fake *fakeMediaSendClient) *Adapter {
	t.Helper()
	host := newTestApp(t)
	legacy := newLegacyClient(t)
	generation := host.BeginGoogleGeneration(legacy)
	t.Cleanup(generation.Release)
	host.Connected.Store(true)
	installMediaSendClient(t, legacy, fake)
	return New("google-primary", host, func() bool { return true })
}

func installMediaSendClient(t *testing.T, want *client.Client, fake mediaSendClient) {
	t.Helper()
	previous := mediaSendClientFor
	mediaSendClientFor = func(got *client.Client) mediaSendClient {
		if got != want {
			t.Fatalf("media client source = %p, want App.GetClient() %p", got, want)
		}
		return fake
	}
	t.Cleanup(func() { mediaSendClientFor = previous })
}

func requireMediaOpError(t *testing.T, err error) bridge.OpError {
	t.Helper()
	if err == nil {
		t.Fatal("SendMedia() error = nil, want classified failure")
	}
	failure, ok := asOpError(err)
	if !ok {
		t.Fatalf("SendMedia() error = %T %v, want bridge.OpError", err, err)
	}
	return failure
}

type fakeMediaSendClient struct {
	uploadResult       *gmproto.MediaContent
	uploadErr          error
	conversationResult *gmproto.Conversation
	conversationErr    error
	sendResults        []*gmproto.SendMessageResponse
	sendErrors         []error

	uploadCalls    int
	uploadData     []byte
	uploadFilename string
	uploadMIME     string
	conversationID string
	sent           []*gmproto.SendMessageRequest
}

func (f *fakeMediaSendClient) UploadMedia(data []byte, filename, mime string) (*gmproto.MediaContent, error) {
	f.uploadCalls++
	f.uploadData = append([]byte(nil), data...)
	f.uploadFilename = filename
	f.uploadMIME = mime
	return f.uploadResult, f.uploadErr
}

func (f *fakeMediaSendClient) GetConversation(conversationID string) (*gmproto.Conversation, error) {
	f.conversationID = conversationID
	return f.conversationResult, f.conversationErr
}

func (f *fakeMediaSendClient) SendMessage(payload *gmproto.SendMessageRequest) (*gmproto.SendMessageResponse, error) {
	f.sent = append(f.sent, payload)
	index := len(f.sent) - 1
	var result *gmproto.SendMessageResponse
	if index < len(f.sendResults) {
		result = f.sendResults[index]
	}
	var err error
	if index < len(f.sendErrors) {
		err = f.sendErrors[index]
	}
	return result, err
}

type panicReader struct{}

func (panicReader) Read([]byte) (int, error) { panic("reader used before readiness check") }

type errorReader struct{ err error }

func (r errorReader) Read([]byte) (int, error) { return 0, r.err }
