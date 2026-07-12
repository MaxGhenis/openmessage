package bridge

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

func TestEventPayloadInvariant(t *testing.T) {
	tests := []struct {
		name  string
		event Event
	}{
		{
			name:  "conversation",
			event: Event{Kind: EventConversation, Conversation: &ConversationEvent{}},
		},
		{
			name:  "message",
			event: Event{Kind: EventMessage, Message: &MessageEvent{}},
		},
		{
			name:  "message mutation",
			event: Event{Kind: EventMessageMutation, MessageMutation: &MessageMutationEvent{}},
		},
		{
			name:  "reaction",
			event: Event{Kind: EventReaction, Reaction: &ReactionEvent{}},
		},
		{
			name:  "receipt",
			event: Event{Kind: EventReceipt, Receipt: &ReceiptEvent{}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !validEvent(tt.event) {
				t.Fatalf("validEvent(%+v) = false, want true", tt.event)
			}
		})
	}

	invalid := []Event{
		{Kind: EventMessage},
		{Kind: EventMessage, Conversation: &ConversationEvent{}},
		{Kind: EventMessage, Message: &MessageEvent{}, Receipt: &ReceiptEvent{}},
	}
	for _, event := range invalid {
		if validEvent(event) {
			t.Fatalf("validEvent(%+v) = true, want false", event)
		}
	}
}

func validEvent(event Event) bool {
	payloadCount := 0
	for _, present := range []bool{
		event.Conversation != nil,
		event.Message != nil,
		event.MessageMutation != nil,
		event.Reaction != nil,
		event.Receipt != nil,
	} {
		if present {
			payloadCount++
		}
	}

	if payloadCount != 1 {
		return false
	}

	switch event.Kind {
	case EventConversation:
		return event.Conversation != nil
	case EventMessage:
		return event.Message != nil
	case EventMessageMutation:
		return event.MessageMutation != nil
	case EventReaction:
		return event.Reaction != nil
	case EventReceipt:
		return event.Receipt != nil
	default:
		return false
	}
}

func TestOpErrorError(t *testing.T) {
	tests := []struct {
		name string
		err  OpError
		want string
	}{
		{
			name: "with cause",
			err: OpError{
				Class:     FailureRateLimited,
				Operation: "send_text",
				Cause:     errors.New("server busy"),
			},
			want: "send_text: rate_limited: server busy",
		},
		{
			name: "without cause",
			err: OpError{
				Class:     FailureUnsupported,
				Operation: "send_media",
			},
			want: "send_media: unsupported",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.err.Error(); got != tt.want {
				t.Fatalf("Error() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCapabilitySetJSONRoundTrip(t *testing.T) {
	want := CapabilitySet{
		StartConversation: true,
		TextSend:          true,
		MediaSend:         true,
		Reactions:         true,
		ReadReceipts:      true,
		PairQR:            true,
		PairPhone:         true,
		MediaDownload:     true,
	}

	encoded, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}

	const wantJSON = `{"start_conversation":true,"text_send":true,"media_send":true,"reactions":true,"read_receipts":true,"pair_qr":true,"pair_phone":true,"media_download":true}`
	if string(encoded) != wantJSON {
		t.Fatalf("json.Marshal() = %s, want %s", encoded, wantJSON)
	}

	var got CapabilitySet
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip = %+v, want %+v", got, want)
	}
}
