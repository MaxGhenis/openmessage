package web

import "sync"

const (
	EventTypeConversations = "conversations"
	EventTypeDrafts        = "drafts"
	EventTypeHeartbeat     = "heartbeat"
	EventTypeMessages      = "messages"
	EventTypeStatus        = "status"
	EventTypeTyping        = "typing"

	subscriberChannelBuffer = 8
	subscriberMaxPending    = 128
	maxEventSubscribers     = 32
)

// StreamEvent is a small invalidation payload pushed to connected browsers.
type StreamEvent struct {
	Type           string `json:"type"`
	ConversationID string `json:"conversation_id,omitempty"`
	Connected      *bool  `json:"connected,omitempty"`
	SenderName     string `json:"sender_name,omitempty"`
	SenderNumber   string `json:"sender_number,omitempty"`
	Timestamp      int64  `json:"timestamp,omitempty"`
	Typing         *bool  `json:"typing,omitempty"`
}

type eventSubscriber struct {
	out    chan StreamEvent
	notify chan struct{}
	done   chan struct{}

	mu      sync.Mutex
	order   []string
	pending map[string]StreamEvent
}

func newEventSubscriber() *eventSubscriber {
	sub := &eventSubscriber{
		out:     make(chan StreamEvent, subscriberChannelBuffer),
		notify:  make(chan struct{}, 1),
		done:    make(chan struct{}),
		pending: make(map[string]StreamEvent),
	}
	go sub.run()
	return sub
}

func (s *eventSubscriber) run() {
	for {
		select {
		case <-s.notify:
			for {
				evt, ok := s.next()
				if !ok {
					break
				}
				select {
				case s.out <- evt:
				case <-s.done:
					return
				}
			}
		case <-s.done:
			return
		}
	}
}

func (s *eventSubscriber) enqueue(evt StreamEvent) {
	key := streamEventKey(evt)

	s.mu.Lock()
	if _, exists := s.pending[key]; !exists {
		s.order = append(s.order, key)
	}
	s.pending[key] = evt
	if len(s.order) > subscriberMaxPending {
		s.collapseLocked(evt)
	}
	s.mu.Unlock()

	select {
	case s.notify <- struct{}{}:
	default:
	}
}

func (s *eventSubscriber) next() (StreamEvent, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.order) == 0 {
		return StreamEvent{}, false
	}
	key := s.order[0]
	s.order = s.order[1:]
	evt := s.pending[key]
	delete(s.pending, key)
	return evt, true
}

func (s *eventSubscriber) collapseLocked(incoming StreamEvent) {
	statusEvt, hasStatus := s.pending[EventTypeStatus]
	if incoming.Type == EventTypeStatus {
		statusEvt = incoming
		hasStatus = true
	}

	s.order = s.order[:0]
	s.pending = make(map[string]StreamEvent)
	if hasStatus {
		s.pending[EventTypeStatus] = statusEvt
		s.order = append(s.order, EventTypeStatus)
	}
	s.pending[EventTypeConversations] = StreamEvent{Type: EventTypeConversations}
	s.pending[streamEventKey(StreamEvent{Type: EventTypeMessages})] = StreamEvent{Type: EventTypeMessages}
	s.pending[streamEventKey(StreamEvent{Type: EventTypeDrafts})] = StreamEvent{Type: EventTypeDrafts}
	s.order = append(s.order, EventTypeConversations, streamEventKey(StreamEvent{Type: EventTypeMessages}), streamEventKey(StreamEvent{Type: EventTypeDrafts}))
}

func (s *eventSubscriber) close() {
	close(s.done)
}

func streamEventKey(evt StreamEvent) string {
	switch evt.Type {
	case EventTypeMessages, EventTypeDrafts, EventTypeTyping:
		return evt.Type + ":" + evt.ConversationID
	default:
		return evt.Type
	}
}

// EventBroker fans out lightweight invalidation events to SSE subscribers.
type EventBroker struct {
	mu          sync.RWMutex
	nextID      int
	subscribers map[int]*eventSubscriber
}

func NewEventBroker() *EventBroker {
	return &EventBroker{
		subscribers: make(map[int]*eventSubscriber),
	}
}

func (b *EventBroker) Subscribe() (int, <-chan StreamEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.subscribeLocked()
}

func (b *EventBroker) TrySubscribe() (int, <-chan StreamEvent, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.subscribers) >= maxEventSubscribers {
		return 0, nil, false
	}

	id, ch := b.subscribeLocked()
	return id, ch, true
}

func (b *EventBroker) subscribeLocked() (int, <-chan StreamEvent) {
	id := b.nextID
	b.nextID++
	sub := newEventSubscriber()
	b.subscribers[id] = sub
	return id, sub.out
}

func (b *EventBroker) Unsubscribe(id int) {
	b.mu.Lock()
	sub := b.subscribers[id]
	delete(b.subscribers, id)
	b.mu.Unlock()
	if sub != nil {
		sub.close()
	}
}

func (b *EventBroker) Publish(evt StreamEvent) {
	b.mu.RLock()
	subs := make([]*eventSubscriber, 0, len(b.subscribers))
	for _, sub := range b.subscribers {
		subs = append(subs, sub)
	}
	b.mu.RUnlock()

	for _, sub := range subs {
		sub.enqueue(evt)
	}
}

func (b *EventBroker) PublishConversations() {
	if b == nil {
		return
	}
	b.Publish(StreamEvent{Type: EventTypeConversations})
}

func (b *EventBroker) PublishDrafts(conversationID string) {
	if b == nil {
		return
	}
	b.Publish(StreamEvent{
		Type:           EventTypeDrafts,
		ConversationID: conversationID,
	})
}

func (b *EventBroker) PublishMessages(conversationID string) {
	if b == nil {
		return
	}
	b.Publish(StreamEvent{
		Type:           EventTypeMessages,
		ConversationID: conversationID,
	})
}

func (b *EventBroker) PublishStatus(connected bool) {
	if b == nil {
		return
	}
	b.Publish(StreamEvent{
		Type:      EventTypeStatus,
		Connected: &connected,
	})
}

func (b *EventBroker) PublishTyping(conversationID, senderName, senderNumber string, typing bool) {
	if b == nil {
		return
	}
	b.Publish(StreamEvent{
		Type:           EventTypeTyping,
		ConversationID: conversationID,
		SenderName:     senderName,
		SenderNumber:   senderNumber,
		Typing:         &typing,
	})
}
