// Package readsource defines the canonical conversation and message read seam.
package readsource

import "github.com/maxghenis/openmessage/internal/db"

// ReadSource is the subset of the legacy store consumed by canonical read
// surfaces. Its signatures intentionally match *db.Store so legacy-primary
// callers use the existing store without an adapter.
type ReadSource interface {
	ListConversations(limit int) ([]*db.Conversation, error)
	GetConversation(id string) (*db.Conversation, error)
	GetMessagesByConversation(conversationID string, limit int) ([]*db.Message, error)
	GetMessagesByConversationBefore(conversationID string, beforeMS int64, beforeID string, limit int) ([]*db.Message, error)
	GetMessagesByConversationAfter(conversationID string, afterMS int64, afterID string, limit int) ([]*db.Message, error)
	GetMessagesAroundMessage(conversationID, messageID string, before, after int) ([]*db.Message, error)
	GetMessagesByConversations(conversationIDs []string, limit int) ([]*db.Message, error)
	GetMessagesByConversationsRange(conversationIDs []string, afterMS, beforeMS int64, limit int) ([]*db.Message, error)
	SearchMessagesFiltered(query string, filter db.SearchFilter) ([]*db.Message, error)
	PlatformStats() ([]db.PlatformStat, error)
	MessageCount(sourcePlatform string) (int, error)
	ConversationCount(sourcePlatform string) (int, error)
	LatestTimestamp(sourcePlatform string) (int64, error)
	LatestConversationPreviews(ids []string) (map[string]string, error)
}

var _ ReadSource = (*db.Store)(nil)
