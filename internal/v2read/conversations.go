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

// GetConversation returns one v2 conversation as the canonical legacy DTO.
func (s *Source) GetConversation(id string) (*db.Conversation, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	conversation, err := s.store.GetConversation(id)
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
