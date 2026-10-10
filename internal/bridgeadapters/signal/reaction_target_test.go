package signal

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/signallive"
)

func TestReactionTargetCarriesTheFieldsThatNameAMessage(t *testing.T) {
	ref := bridge.MessageRef{
		RemoteID:       "f48818f15483f503bb133d92360ca8e2fbd8287e",
		AuthorID:       "+15551234567",
		Outgoing:       true,
		SentAt:         time.UnixMilli(1_700_000_000_123),
		Text:           "reply-only field",
		HasAttachment:  true,
		AttachmentMIME: "image/jpeg",
	}
	got := reflect.ValueOf(ReactionTarget(ref))
	source := reflect.ValueOf(ref)
	// Every ReactionTarget field must be the MessageRef field of the same
	// name, so a field added to it cannot be left unfilled here silently.
	var names []string
	for index := 0; index < got.NumField(); index++ {
		name := got.Type().Field(index).Name
		names = append(names, name)
		field := source.FieldByName(name)
		if !field.IsValid() {
			t.Fatalf("bridge.MessageRef has no %s field for signallive.ReactionTarget", name)
		}
		if !reflect.DeepEqual(got.Field(index).Interface(), field.Interface()) {
			t.Fatalf("ReactionTarget().%s = %v, want %v", name, got.Field(index).Interface(), field.Interface())
		}
		if field.IsZero() {
			t.Fatalf("fixture leaves MessageRef.%s zero; set it so the copy is checked", name)
		}
	}
	// Signal names a reaction's target by author and sent timestamp. Outgoing
	// says whose message it is, and RemoteID is the timestamp of this
	// account's own sends.
	if want := []string{"RemoteID", "AuthorID", "Outgoing", "SentAt"}; !slices.Equal(names, want) {
		t.Fatalf("signallive.ReactionTarget fields = %v, want %v", names, want)
	}
}

func TestSendReactionHandsTheDispatchersDescriptionToThePoller(t *testing.T) {
	tests := []struct {
		name   string
		target bridge.MessageRef
	}{
		{
			name: "incoming message",
			target: bridge.MessageRef{
				RemoteID: "f48818f15483f503bb133d92360ca8e2fbd8287e",
				AuthorID: "9f4b50e3-ebf2-413c-a856-161756a6161a",
				SentAt:   time.UnixMilli(1_700_000_000_123),
			},
		},
		{
			name:   "this account's confirmed send",
			target: bridge.MessageRef{RemoteID: "1700000000555", Outgoing: true, SentAt: time.UnixMilli(1_700_000_000_000)},
		},
		{
			name: "this account's send still on its request ID",
			target: bridge.MessageRef{
				RemoteID: "0f1e2d3c4b5a69788796a5b4c3d2e1f0", Outgoing: true, SentAt: time.UnixMilli(1_700_000_000_000),
			},
		},
		{
			name:   "incoming message with no stored sender",
			target: bridge.MessageRef{RemoteID: "1700000000123", SentAt: time.UnixMilli(1_700_000_000_123)},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			poller := newFakePoller()
			poller.status = signallive.StatusSnapshot{Connected: true, Paired: true, Account: "+15551230000"}
			adapter := &Adapter{accountID: "signal-primary", poller: poller}
			if _, err := adapter.SendReaction(context.Background(), bridge.ReactionRequest{
				AccountID:    "signal-primary",
				Conversation: bridge.ConversationRef{RemoteID: "signal:+15551234567"},
				Target:       tc.target,
				Emoji:        "👍",
				Action:       bridge.ReactionAdd,
			}); err != nil {
				t.Fatalf("SendReaction(): %v", err)
			}
			want := signallive.ReactionTarget{
				RemoteID: tc.target.RemoteID, AuthorID: tc.target.AuthorID,
				Outgoing: tc.target.Outgoing, SentAt: tc.target.SentAt,
			}
			if got := poller.lastReactionRequest().target; got != want {
				t.Fatalf("reaction target handed to signallive = %+v, want %+v", got, want)
			}
		})
	}
}

// A target signallive cannot name fails before signal-cli runs: this account's
// send that has no Signal timestamp yet, or an incoming message with no stored
// sender. The adapter must report that as not dispatched, so the outbox
// retries instead of marking the reaction uncertain.
func TestSendReactionReportsATargetItCannotNameAsNotDispatched(t *testing.T) {
	tests := []struct {
		name   string
		target bridge.MessageRef
	}{
		{
			name: "this account's send still on its request ID",
			target: bridge.MessageRef{
				RemoteID: "0f1e2d3c4b5a69788796a5b4c3d2e1f0", Outgoing: true, SentAt: time.UnixMilli(1_700_000_000_000),
			},
		},
		{
			name:   "incoming message with no stored sender",
			target: bridge.MessageRef{RemoteID: "1700000000123", SentAt: time.UnixMilli(1_700_000_000_123)},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, targetErr := signallive.ReactionTargetArgs(ReactionTarget(tc.target), "+15551230000")
			if targetErr == nil {
				t.Fatal("ReactionTargetArgs() succeeded, want a target it cannot name")
			}
			poller := newFakePoller()
			poller.status = signallive.StatusSnapshot{Connected: true, Paired: true, Account: "+15551230000"}
			poller.reactionErr = targetErr
			adapter := &Adapter{accountID: "signal-primary", poller: poller}

			_, err := adapter.SendReaction(context.Background(), bridge.ReactionRequest{
				AccountID:    "signal-primary",
				Conversation: bridge.ConversationRef{RemoteID: "signal:+15551234567"},
				Target:       tc.target,
				Emoji:        "👍",
			})
			var operationError bridge.OpError
			if !errors.As(err, &operationError) ||
				operationError.Class != bridge.FailureTransient ||
				operationError.Dispatch != bridge.DispatchNotCalled ||
				!errors.Is(operationError.Cause, targetErr) {
				t.Fatalf("SendReaction() error = %+v, want a retryable not-dispatched failure caused by %v", err, targetErr)
			}
			if got := poller.appliedCount(); got != 0 {
				t.Fatalf("a target it cannot name applied %d lifecycle transitions, want 0", got)
			}
		})
	}
}
