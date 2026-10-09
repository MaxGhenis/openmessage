package v2read

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"

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
	return s.mapConversations(conversations, accounts)
}

// ListPlatformConversations returns the conversations whose SourcePlatform is
// platform, newest first with conversation ID as the tie-breaker, at most
// limit: exactly the first limit rows of ListConversations(math.MaxInt) whose
// SourcePlatform equals platform. Only the accounts of that platform are read,
// and only the returned rows are mapped.
func (s *Source) ListPlatformConversations(platform string, limit int) ([]*db.Conversation, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		return []*db.Conversation{}, nil
	}
	accounts, err := s.store.ListAccounts()
	if err != nil {
		return nil, fmt.Errorf("list %s conversations: %w", platform, err)
	}
	var conversations []sqlite.Conversation
	for _, account := range accounts {
		if platformForBridgeKey(account.BridgeKey) != platform {
			continue
		}
		accountConversations, err := s.store.ListConversationsByRecency(account.AccountID)
		if err != nil {
			return nil, fmt.Errorf("list %s conversations: %w", platform, err)
		}
		conversations = append(conversations, accountConversations...)
	}
	// ListConversationsByRecencyAllAccounts' order.
	sort.Slice(conversations, func(i, j int) bool {
		if conversations[i].LastMessageAtMS != conversations[j].LastMessageAtMS {
			return conversations[i].LastMessageAtMS > conversations[j].LastMessageAtMS
		}
		return conversations[i].ConversationID < conversations[j].ConversationID
	})
	if len(conversations) > limit {
		conversations = conversations[:limit]
	}
	return s.mapConversations(conversations, indexAccounts(accounts))
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
	if len(conversations) == 0 {
		return []*db.Conversation{}, nil
	}
	accounts, err := s.accountIndex()
	if err != nil {
		return nil, fmt.Errorf("search conversations: %w", err)
	}
	return s.mapConversations(conversations, accounts)
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
	mapped, err := s.mapConversations([]sqlite.Conversation{conversation}, accounts)
	if err != nil {
		return nil, err
	}
	return mapped[0], nil
}

// GetConversationsByID is GetConversation for many IDs in a fixed number of
// statements, keyed by the requested ID. It answers only IDs that name a v2
// conversation directly (as stored: no surrounding whitespace), for which
// GetConversation returns the same DTO; legacy remote IDs, unknown IDs and
// untrimmed IDs are absent, and callers resolve those with GetConversation.
func (s *Source) GetConversationsByID(ids []string) (map[string]*db.Conversation, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	rows, err := s.store.ConversationsByID(directConversationIDs(ids))
	if err != nil {
		return nil, err
	}
	result := make(map[string]*db.Conversation, len(rows))
	if len(rows) == 0 {
		return result, nil
	}
	conversations := make([]sqlite.Conversation, 0, len(rows))
	for _, conversation := range rows {
		conversations = append(conversations, conversation)
	}
	// Map in a fixed order so an unmappable row always reports the same error.
	sort.Slice(conversations, func(i, j int) bool {
		return conversations[i].ConversationID < conversations[j].ConversationID
	})
	accounts, err := s.accountIndex()
	if err != nil {
		return nil, fmt.Errorf("get conversations: %w", err)
	}
	mapped, err := s.mapConversations(conversations, accounts)
	if err != nil {
		return nil, err
	}
	for _, conversation := range mapped {
		result[conversation.ConversationID] = conversation
	}
	return result, nil
}

// directConversationIDs keeps the IDs that resolveConversationID passes
// through unchanged when they exist: non-empty and already trimmed.
func directConversationIDs(ids []string) []string {
	direct := make([]string, 0, len(ids))
	for _, id := range ids {
		if id != "" && strings.TrimSpace(id) == id {
			direct = append(direct, id)
		}
	}
	return direct
}
