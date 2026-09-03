package v2read

import (
	"context"
	"errors"
	"sort"

	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

// GetMessagesByConversations returns the newest limit messages across the
// given conversations, ordered by timestamp ascending — the same contract as
// the legacy store's batch query.
func (s *Source) GetMessagesByConversations(
	conversationIDs []string,
	limit int,
) ([]*db.Message, error) {
	return s.batchMessages(conversationIDs, 0, 0, limit)
}

// GetMessagesByConversationsRange returns the newest limit messages across
// the given conversations with afterMS <= timestamp <= beforeMS (each bound
// applied only when > 0), ordered by timestamp ascending.
func (s *Source) GetMessagesByConversationsRange(
	conversationIDs []string,
	afterMS, beforeMS int64,
	limit int,
) ([]*db.Message, error) {
	return s.batchMessages(conversationIDs, afterMS, beforeMS, limit)
}

func (s *Source) batchMessages(
	conversationIDs []string,
	afterMS, beforeMS int64,
	limit int,
) ([]*db.Message, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	if len(conversationIDs) == 0 || limit <= 0 {
		return nil, nil
	}

	resolved := make([]string, 0, len(conversationIDs))
	seen := make(map[string]struct{}, len(conversationIDs))
	for _, id := range conversationIDs {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		resolved = append(resolved, id)
	}

	// Prune to the newest limit after every conversation so the working set
	// never exceeds 2×limit however many conversations match.
	var merged []sqlite.Message
	for _, conversationID := range resolved {
		rows, err := s.conversationMessagesInRange(conversationID, afterMS, beforeMS, limit)
		if err != nil {
			return nil, err
		}
		merged = keepNewest(append(merged, rows...), limit)
	}

	// Present ascending.
	for i, j := 0, len(merged)-1; i < j; i, j = i+1, j-1 {
		merged[i], merged[j] = merged[j], merged[i]
	}
	return s.mapMessages(merged)
}

// keepNewest orders messages newest-first by (occurred_at_ms, message_id) —
// the legacy store's tiebreak — and truncates to limit.
func keepNewest(messages []sqlite.Message, limit int) []sqlite.Message {
	sort.Slice(messages, func(i, j int) bool {
		if messages[i].OccurredAtMS != messages[j].OccurredAtMS {
			return messages[i].OccurredAtMS > messages[j].OccurredAtMS
		}
		return messages[i].MessageID > messages[j].MessageID
	})
	if len(messages) > limit {
		messages = messages[:limit]
	}
	return messages
}

// conversationMessagesInRange collects up to limit newest-first rows for one
// conversation, bounded by the optional afterMS/beforeMS range. The upper
// bound rides the repository's exclusive before-cursor (beforeMS+1 makes it
// inclusive); the lower bound is enforced while walking newest-first pages.
func (s *Source) conversationMessagesInRange(
	conversationID string,
	afterMS, beforeMS int64,
	limit int,
) ([]sqlite.Message, error) {
	var cursorMS int64
	var cursorID string
	if beforeMS > 0 {
		cursorMS = beforeMS + 1
	}
	collected := make([]sqlite.Message, 0, min(limit, sourceMessagePageSize))
	for len(collected) < limit {
		pageSize := min(limit-len(collected), sourceMessagePageSize)
		page, err := s.messages.ListMessagesByConversation(
			context.Background(), conversationID, cursorMS, cursorID, pageSize,
		)
		if err != nil {
			return nil, err
		}
		for _, message := range page {
			if afterMS > 0 && message.OccurredAtMS < afterMS {
				return collected, nil
			}
			collected = append(collected, message)
		}
		if len(page) < pageSize {
			return collected, nil
		}
		last := page[len(page)-1]
		if last.OccurredAtMS == cursorMS && last.MessageID == cursorID {
			return nil, errors.New("v2 read message pagination did not advance")
		}
		cursorMS = last.OccurredAtMS
		cursorID = last.MessageID
	}
	return collected, nil
}
