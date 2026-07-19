package web

import (
	"fmt"
	"testing"
	"time"
)

func collectBrokerEvents(ch <-chan StreamEvent, quiet time.Duration) []StreamEvent {
	events := make([]StreamEvent, 0)
	timer := time.NewTimer(quiet)
	defer timer.Stop()

	for {
		select {
		case evt := <-ch:
			events = append(events, evt)
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(quiet)
		case <-timer.C:
			return events
		}
	}
}

func TestEventSubscriberCollapsePreservesLatestStatusAndGlobalRefreshes(t *testing.T) {
	connected := false
	sub := &eventSubscriber{
		pending: map[string]StreamEvent{
			EventTypeStatus: {Type: EventTypeStatus, Connected: &connected},
			streamEventKey(StreamEvent{Type: EventTypeMessages, ConversationID: "c1"}): {Type: EventTypeMessages, ConversationID: "c1"},
			streamEventKey(StreamEvent{Type: EventTypeDrafts, ConversationID: "c2"}):   {Type: EventTypeDrafts, ConversationID: "c2"},
		},
		order: []string{
			EventTypeStatus,
			streamEventKey(StreamEvent{Type: EventTypeMessages, ConversationID: "c1"}),
			streamEventKey(StreamEvent{Type: EventTypeDrafts, ConversationID: "c2"}),
		},
	}

	sub.collapseLocked(StreamEvent{Type: EventTypeMessages, ConversationID: "c9"})

	if len(sub.order) != 4 {
		t.Fatalf("collapsed order length = %d, want 4", len(sub.order))
	}
	if sub.order[0] != EventTypeStatus {
		t.Fatalf("first collapsed event key = %q, want %q", sub.order[0], EventTypeStatus)
	}
	if evt, ok := sub.pending[EventTypeStatus]; !ok || evt.Connected == nil || *evt.Connected {
		t.Fatalf("collapsed status event = %+v, want connected=false", evt)
	}
	if evt, ok := sub.pending[EventTypeConversations]; !ok || evt.Type != EventTypeConversations {
		t.Fatalf("collapsed conversations event missing: %+v", evt)
	}
	if evt, ok := sub.pending[streamEventKey(StreamEvent{Type: EventTypeMessages})]; !ok || evt.ConversationID != "" {
		t.Fatalf("collapsed global messages event = %+v, want empty conversation", evt)
	}
	if evt, ok := sub.pending[streamEventKey(StreamEvent{Type: EventTypeDrafts})]; !ok || evt.ConversationID != "" {
		t.Fatalf("collapsed global drafts event = %+v, want empty conversation", evt)
	}
}

func TestEventBrokerRejectsNewestSubscriberAtLimit(t *testing.T) {
	b := NewEventBroker()
	ids := make([]int, 0, maxEventSubscribers)
	for i := 0; i < maxEventSubscribers; i++ {
		id, _, ok := b.TrySubscribe()
		if !ok {
			t.Fatalf("subscriber %d rejected before limit", i)
		}
		ids = append(ids, id)
	}
	if _, _, ok := b.TrySubscribe(); ok {
		t.Fatal("subscriber above limit accepted")
	}
	b.Unsubscribe(ids[0])
	if _, _, ok := b.TrySubscribe(); !ok {
		t.Fatal("subscriber rejected after slot released")
	}
	for _, id := range ids[1:] {
		b.Unsubscribe(id)
	}
}

func TestEventBrokerOverflowFallsBackToGlobalRefresh(t *testing.T) {
	broker := NewEventBroker()
	id, ch := broker.Subscribe()
	defer broker.Unsubscribe(id)

	for i := 0; i < subscriberMaxPending+32; i++ {
		broker.PublishMessages(fmt.Sprintf("c-%d", i))
	}

	events := collectBrokerEvents(ch, 100*time.Millisecond)
	if len(events) == 0 {
		t.Fatal("expected at least one event from broker")
	}

	var (
		sawGlobalConversations bool
		sawGlobalMessages      bool
		sawGlobalDrafts        bool
	)
	for _, evt := range events {
		if evt.Type == EventTypeConversations {
			sawGlobalConversations = true
		}
		if evt.Type == EventTypeMessages && evt.ConversationID == "" {
			sawGlobalMessages = true
		}
		if evt.Type == EventTypeDrafts && evt.ConversationID == "" {
			sawGlobalDrafts = true
		}
	}

	if !sawGlobalConversations || !sawGlobalMessages || !sawGlobalDrafts {
		t.Fatalf("overflow events = %+v, want global conversations/messages/drafts refreshes", events)
	}
}
