package v2read

import (
	"context"

	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

// SearchMessagesFiltered performs a bounded LIKE substring scan. R5 preserves
// substring matching and deterministic recency order; FTS relevance ranking is
// explicitly deferred to S8.
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
