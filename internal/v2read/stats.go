package v2read

import (
	"context"
	"sort"
	"strings"

	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

// MessageCount returns all message rows or those whose account maps to the
// requested legacy source platform.
func (s *Source) MessageCount(sourcePlatform string) (int, error) {
	if err := s.ready(); err != nil {
		return 0, err
	}
	accounts, err := s.store.ListAccounts()
	if err != nil {
		return 0, err
	}
	count := 0
	for _, account := range accounts {
		if sourcePlatform != "" && platformForBridgeKey(account.BridgeKey) != sourcePlatform {
			continue
		}
		conversations, err := s.store.ListConversationsByRecency(account.AccountID)
		if err != nil {
			return 0, err
		}
		for _, conversation := range conversations {
			if err := s.walkConversationMessages(conversation.ConversationID, func(sqlite.Message) {
				count++
			}); err != nil {
				return 0, err
			}
		}
	}
	return count, nil
}

// ConversationCount returns all conversations or those mapped to one legacy
// source platform.
func (s *Source) ConversationCount(sourcePlatform string) (int, error) {
	if err := s.ready(); err != nil {
		return 0, err
	}
	accounts, err := s.store.ListAccounts()
	if err != nil {
		return 0, err
	}
	count := 0
	for _, account := range accounts {
		if sourcePlatform != "" && platformForBridgeKey(account.BridgeKey) != sourcePlatform {
			continue
		}
		conversations, err := s.store.ListConversationsByRecency(account.AccountID)
		if err != nil {
			return 0, err
		}
		count += len(conversations)
	}
	return count, nil
}

// LatestTimestamp returns the newest message timestamp for sourcePlatform.
func (s *Source) LatestTimestamp(sourcePlatform string) (int64, error) {
	if err := s.ready(); err != nil {
		return 0, err
	}
	accounts, err := s.store.ListAccounts()
	if err != nil {
		return 0, err
	}
	var latest int64
	for _, account := range accounts {
		if platformForBridgeKey(account.BridgeKey) != sourcePlatform {
			continue
		}
		conversations, err := s.store.ListConversationsByRecency(account.AccountID)
		if err != nil {
			return 0, err
		}
		for _, conversation := range conversations {
			messages, err := s.messages.ListMessagesByConversation(
				context.Background(), conversation.ConversationID, 0, "", 1,
			)
			if err != nil {
				return 0, err
			}
			if len(messages) > 0 && messages[0].OccurredAtMS > latest {
				latest = messages[0].OccurredAtMS
			}
		}
	}
	return latest, nil
}

// PlatformStats summarizes v2 messages under their legacy platform labels.
func (s *Source) PlatformStats() ([]db.PlatformStat, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	accounts, err := s.store.ListAccounts()
	if err != nil {
		return nil, err
	}
	byPlatform := make(map[string]*db.PlatformStat)
	for _, account := range accounts {
		platform := platformForBridgeKey(account.BridgeKey)
		if strings.TrimSpace(platform) == "" {
			platform = "unknown"
		}
		conversations, err := s.store.ListConversationsByRecency(account.AccountID)
		if err != nil {
			return nil, err
		}
		for _, conversation := range conversations {
			if err := s.walkConversationMessages(conversation.ConversationID, func(message sqlite.Message) {
				stat := byPlatform[platform]
				if stat == nil {
					stat = &db.PlatformStat{Platform: platform}
					byPlatform[platform] = stat
				}
				stat.Count++
				if message.OccurredAtMS > stat.LatestMS {
					stat.LatestMS = message.OccurredAtMS
				}
				if message.Direction == sqlite.MessageDirectionIncoming &&
					message.OccurredAtMS > stat.LatestRecvMS {
					stat.LatestRecvMS = message.OccurredAtMS
				}
			}); err != nil {
				return nil, err
			}
		}
	}
	stats := make([]db.PlatformStat, 0, len(byPlatform))
	for _, stat := range byPlatform {
		stats = append(stats, *stat)
	}
	sort.Slice(stats, func(i, j int) bool {
		if stats[i].LatestMS != stats[j].LatestMS {
			return stats[i].LatestMS > stats[j].LatestMS
		}
		return stats[i].Platform < stats[j].Platform
	})
	return stats, nil
}

// LatestConversationPreviews formats each requested conversation's latest v2
// message exactly like the legacy store's preview formatter.
func (s *Source) LatestConversationPreviews(ids []string) (map[string]string, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	unique := make([]string, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		unique = append(unique, id)
	}
	previews := make(map[string]string, len(unique))
	for _, conversationID := range unique {
		messages, err := s.messages.ListMessagesByConversation(
			context.Background(), conversationID, 0, "", 1,
		)
		if err != nil {
			return nil, err
		}
		if len(messages) == 0 {
			continue
		}
		message := messages[0]
		attachment, hasAttachment, err := s.messageAttachment(
			context.Background(), message.MessageID,
		)
		if err != nil {
			return nil, err
		}
		mediaID := ""
		mimeType := ""
		if hasAttachment {
			mediaID = "v2msg:" + message.MessageID + ":0"
			mimeType = attachment.MIME
		}
		previews[conversationID] = formatLastMessagePreview(
			message.Body,
			mediaID,
			mimeType,
			message.Direction == sqlite.MessageDirectionOutgoing,
		)
	}
	return previews, nil
}

func formatLastMessagePreview(body, mediaID, mimeType string, isFromMe bool) string {
	const previewRuneLimit = 120

	preview := strings.Join(strings.Fields(body), " ")
	if preview == "" && strings.TrimSpace(mediaID) != "" {
		switch {
		case strings.HasPrefix(strings.ToLower(mimeType), "image/"):
			preview = "Photo"
		case strings.HasPrefix(strings.ToLower(mimeType), "video/"):
			preview = "Video"
		case strings.HasPrefix(strings.ToLower(mimeType), "audio/"):
			preview = "Audio"
		default:
			preview = "Attachment"
		}
	}
	if preview == "" {
		return ""
	}
	if isFromMe {
		preview = "You: " + preview
	}
	return truncateRunes(preview, previewRuneLimit)
}

func truncateRunes(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit-1]) + "…"
}
