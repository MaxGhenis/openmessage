package v2read

import (
	"context"
	"fmt"
	"sort"

	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

// LatestMessagesByConversation is GetMessagesByConversation(id, 1) for many
// conversations in a fixed number of statements, keyed by the requested ID:
// each value holds the conversation's newest message, or is empty when it has
// none. Like GetConversationsByID it answers only IDs that name a v2
// conversation directly; legacy remote IDs, unknown IDs and untrimmed IDs are
// absent, and callers resolve those with GetMessagesByConversation.
func (s *Source) LatestMessagesByConversation(ids []string) (map[string][]*db.Message, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	ctx := context.Background()
	conversations, err := s.store.ConversationsByID(directConversationIDs(ids))
	if err != nil {
		return nil, err
	}
	result := make(map[string][]*db.Message, len(conversations))
	if len(conversations) == 0 {
		return result, nil
	}
	conversationIDs := make([]string, 0, len(conversations))
	for conversationID := range conversations {
		conversationIDs = append(conversationIDs, conversationID)
	}
	sort.Strings(conversationIDs)
	latest, err := s.messages.LatestMessagesForConversations(ctx, conversationIDs)
	if err != nil {
		return nil, err
	}
	rows := make([]sqlite.Message, 0, len(latest))
	for _, conversationID := range conversationIDs {
		if message, ok := latest[conversationID]; ok {
			rows = append(rows, message)
		}
	}
	// Each message maps from its own rows alone, so one page of every
	// conversation's latest message maps exactly as one page per conversation.
	mapped, err := s.mapMessages(rows)
	if err != nil {
		return nil, fmt.Errorf("latest messages: %w", err)
	}
	for _, conversationID := range conversationIDs {
		result[conversationID] = []*db.Message{}
	}
	for _, message := range mapped {
		result[message.ConversationID] = []*db.Message{message}
	}
	return result, nil
}
