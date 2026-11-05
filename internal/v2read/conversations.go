package v2read

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

// ListConversations returns v2 conversations in cross-account recency order.
func (s *Source) ListConversations(limit int) ([]*db.Conversation, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	conversations, err := s.store.ListConversationsByRecencyAllAccounts(limit)
	if err != nil {
		return nil, err
	}
	accounts, err := s.accountIndex()
	if err != nil {
		return nil, fmt.Errorf("list conversations: %w", err)
	}
	mapped := make([]*db.Conversation, 0, len(conversations))
	for _, conversation := range conversations {
		dto, err := s.mapConversation(conversation, accounts)
		if err != nil {
			return nil, err
		}
		mapped = append(mapped, dto)
	}
	return mapped, nil
}

// SearchConversationsByMetadata is the v2 counterpart of the legacy metadata
// search: a bounded title/participant substring match in SQL, mapping only the
// hits (at most limit) rather than the whole conversation list.
func (s *Source) SearchConversationsByMetadata(query string, limit int) ([]*db.Conversation, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	conversations, err := s.store.SearchConversationsByName(query, limit)
	if err != nil {
		return nil, err
	}
	return s.mapSearchHits(conversations)
}

// SearchConversationsByPhoneLine runs the v2 store's phone line search, the
// legacy one's counterpart. It returns the conversations with a participant
// other than the owner on the line that line, a phoneline.Line, names, and
// the store matches in SQL before the limit.
func (s *Source) SearchConversationsByPhoneLine(line string, limit int) ([]*db.Conversation, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	conversations, err := s.store.SearchConversationsByPhoneLine(line, limit)
	if err != nil {
		return nil, err
	}
	return s.mapSearchHits(conversations)
}

// SearchConversationsByAddress runs the v2 store's address search, the
// legacy one's counterpart. It returns the conversations with a participant
// other than the owner whose canonical value shares address's addressmatch
// key, and the store matches in SQL before the limit.
func (s *Source) SearchConversationsByAddress(address string, limit int) ([]*db.Conversation, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	conversations, err := s.store.SearchConversationsByAddress(address, limit)
	if err != nil {
		return nil, err
	}
	return s.mapSearchHits(conversations)
}

// mapSearchHits maps a search's hits, and only those, to the canonical
// legacy DTO.
func (s *Source) mapSearchHits(conversations []sqlite.Conversation) ([]*db.Conversation, error) {
	if len(conversations) == 0 {
		return []*db.Conversation{}, nil
	}
	accounts, err := s.accountIndex()
	if err != nil {
		return nil, fmt.Errorf("search conversations: %w", err)
	}
	mapped := make([]*db.Conversation, 0, len(conversations))
	for _, conversation := range conversations {
		dto, err := s.mapConversation(conversation, accounts)
		if err != nil {
			return nil, err
		}
		mapped = append(mapped, dto)
	}
	return mapped, nil
}

// GetConversation returns one v2 conversation as the canonical legacy DTO.
// The id may be a v2 conversation ID or a legacy remote conversation ID; the
// returned DTO always carries the canonical v2 ID.
func (s *Source) GetConversation(id string) (*db.Conversation, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	conversation, err := s.store.GetConversation(s.resolveConversationID(id))
	if errors.Is(err, sqlite.ErrNotFound) {
		return nil, sql.ErrNoRows
	}
	if err != nil {
		return nil, err
	}
	accounts, err := s.accountIndex()
	if err != nil {
		return nil, fmt.Errorf("get conversation %q: %w", id, err)
	}
	return s.mapConversation(conversation, accounts)
}
