package v2read

import (
	"context"

	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

// SearchMessagesFiltered returns the newest messages whose body contains query
// under SQLite LIKE semantics (R5: substring, ASCII case-insensitive, '%' and
// '_' as wildcards) in deterministic recency order. The store reads candidates
// from its trigram index or its newest messages; the LIKE decides every match
// (sqlite.MessageRepository.SearchMessages).
func (s *Source) SearchMessagesFiltered(
	query string,
	filter db.SearchFilter,
) ([]*db.Message, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	messages, err := s.messages.SearchMessages(context.Background(), query, sqlite.SearchQuery{
		ConversationID:       filter.ConversationID,
		SenderCanonicalValue: filter.Phone,
		SinceMS:              filter.SinceMS,
		UntilMS:              filter.UntilMS,
		Limit:                filter.Limit,
	})
	if err != nil {
		return nil, err
	}
	return s.mapMessages(messages)
}
