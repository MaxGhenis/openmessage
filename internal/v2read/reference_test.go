package v2read

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

// referenceSource is the v2 read mapping as it stood before rosters, senders,
// attachments, send states and previews were batched (origin/main 19e35d9:
// internal/v2read/map.go, conversations.go, stats.go, messages.go, batch.go,
// search.go). Its bodies are kept verbatim, issuing one single-row read per
// participant, message and conversation, as the oracle the differential tests
// compare the batched Source against. The list queries are unchanged, so the
// read paths differ only in mapping.
type referenceSource struct{ s *Source }

// --- conversations.go ---

func (r referenceSource) ListConversations(limit int) ([]*db.Conversation, error) {
	s := r.s
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
		dto, err := r.mapConversation(conversation, accounts)
		if err != nil {
			return nil, err
		}
		mapped = append(mapped, dto)
	}
	return mapped, nil
}

func (r referenceSource) SearchConversationsByMetadata(query string, limit int) ([]*db.Conversation, error) {
	s := r.s
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
	mapped := make([]*db.Conversation, 0, len(conversations))
	for _, conversation := range conversations {
		dto, err := r.mapConversation(conversation, accounts)
		if err != nil {
			return nil, err
		}
		mapped = append(mapped, dto)
	}
	return mapped, nil
}

func (r referenceSource) GetConversation(id string) (*db.Conversation, error) {
	s := r.s
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
	return r.mapConversation(conversation, accounts)
}

// ListPlatformConversations' oracle is the filter internal/tools ran before
// it existed: map every conversation, keep the first limit on the platform.
func (r referenceSource) ListPlatformConversations(platform string, limit int) ([]*db.Conversation, error) {
	if limit <= 0 {
		return []*db.Conversation{}, nil
	}
	candidates, err := r.ListConversations(math.MaxInt)
	if err != nil {
		return nil, err
	}
	conversations := []*db.Conversation{}
	for _, conversation := range candidates {
		if conversation.SourcePlatform == platform {
			conversations = append(conversations, conversation)
			if len(conversations) >= limit {
				break
			}
		}
	}
	return conversations, nil
}

// --- map.go ---

func (r referenceSource) mapConversation(
	conversation sqlite.Conversation,
	accounts map[string]sqlite.Account,
) (*db.Conversation, error) {
	account, ok := accounts[conversation.AccountID]
	if !ok {
		return nil, fmt.Errorf(
			"map conversation %q: account %q is missing",
			conversation.ConversationID,
			conversation.AccountID,
		)
	}
	infos, err := r.participantInfos(conversation.ConversationID)
	if err != nil {
		return nil, err
	}
	participants, err := participantsJSON(conversation.ConversationID, infos)
	if err != nil {
		return nil, err
	}
	name := conversation.Title
	if name == "" && conversation.Kind != sqlite.ConversationKindGroup {
		// Direct conversations have no title of their own; the legacy DTO
		// names them after the remote peer, so v2 reads must too or 1:1
		// threads render blank and unfindable in every conversation list.
		name = directPeerName(infos)
	}
	return &db.Conversation{
		ConversationID:   conversation.ConversationID,
		Name:             name,
		IsGroup:          conversation.Kind == sqlite.ConversationKindGroup,
		Participants:     participants,
		LastMessageTS:    conversation.LastMessageAtMS,
		UnreadCount:      0,
		SourcePlatform:   platformForBridgeKey(account.BridgeKey),
		IsFavorite:       conversation.IsFavorite,
		NotificationMode: string(conversation.NotificationMode),
	}, nil
}

func (r referenceSource) participantInfos(conversationID string) ([]participantInfo, error) {
	s := r.s
	participants, err := s.store.ListParticipants(conversationID)
	if err != nil {
		return nil, fmt.Errorf("map conversation %q participants: %w", conversationID, err)
	}
	infos := make([]participantInfo, 0, len(participants))
	for _, participant := range participants {
		identity, err := s.store.GetIdentity(participant.IdentityID)
		if err != nil {
			return nil, fmt.Errorf(
				"map conversation %q participant %q: %w",
				conversationID,
				participant.IdentityID,
				err,
			)
		}
		name := strings.TrimSpace(participant.DisplayName)
		if name == "" {
			name = identity.DisplayName
		}
		infos = append(infos, participantInfo{
			dto: participantDTO{
				Name:   name,
				Number: identity.CanonicalValue,
				IsMe:   identity.IsSelf,
			},
			isSelf: identity.IsSelf,
		})
	}
	return infos, nil
}

func (r referenceSource) mapMessages(
	messages []sqlite.Message,
) ([]*db.Message, error) {
	s := r.s
	accounts, err := s.accountIndex()
	if err != nil {
		return nil, fmt.Errorf("map messages: %w", err)
	}
	messageIDs := make([]string, 0, len(messages))
	for _, message := range messages {
		messageIDs = append(messageIDs, message.MessageID)
	}
	reactions, err := s.reactions.ReactionsForMessages(context.Background(), messageIDs)
	if err != nil {
		return nil, fmt.Errorf("map messages: load reactions: %w", err)
	}
	mapped := make([]*db.Message, 0, len(messages))
	for _, message := range messages {
		dto, err := r.mapMessage(message, accounts, reactions[message.MessageID])
		if err != nil {
			return nil, err
		}
		mapped = append(mapped, dto)
	}
	return mapped, nil
}

func (r referenceSource) mapMessage(
	message sqlite.Message,
	accounts map[string]sqlite.Account,
	reactionRows []sqlite.ReactionRow,
) (*db.Message, error) {
	s := r.s
	account, ok := accounts[message.AccountID]
	if !ok {
		return nil, fmt.Errorf(
			"map message %q: account %q is missing",
			message.MessageID,
			message.AccountID,
		)
	}
	dto := &db.Message{
		MessageID:      message.MessageID,
		ConversationID: message.ConversationID,
		Body:           message.Body,
		TimestampMS:    message.OccurredAtMS,
		IsFromMe:       message.Direction == sqlite.MessageDirectionOutgoing,
		SourcePlatform: platformForBridgeKey(account.BridgeKey),
		SourceID:       message.RemoteMessageID,
	}
	reactionJSON, err := mapReactions(reactionRows)
	if err != nil {
		return nil, fmt.Errorf("map message %q reactions: %w", message.MessageID, err)
	}
	dto.Reactions = reactionJSON
	if message.SenderIdentityID != nil {
		identity, err := s.store.GetIdentity(*message.SenderIdentityID)
		if err != nil {
			return nil, fmt.Errorf(
				"map message %q sender %q: %w",
				message.MessageID,
				*message.SenderIdentityID,
				err,
			)
		}
		dto.SenderNumber = identity.CanonicalValue
		dto.SenderName = identity.DisplayName
	}
	if message.ReplyToRemoteID != nil {
		dto.ReplyToID = *message.ReplyToRemoteID
	}
	if dto.IsFromMe {
		status, err := r.sendStatusForMessage(message)
		if err != nil {
			return nil, err
		}
		dto.Status = status
	}
	attachment, ok, err := r.messageAttachment(context.Background(), message.MessageID)
	if err != nil {
		return nil, err
	}
	if ok {
		dto.MediaID = fmt.Sprintf("v2msg:%s:%d", message.MessageID, attachment.Ordinal)
		dto.MimeType = attachment.MIME
	}
	return dto, nil
}

func (r referenceSource) messageAttachment(
	ctx context.Context,
	messageID string,
) (sqlite.MessageAttachment, bool, error) {
	// R2 historical migration and every current decoder number attachments from
	// zero, so ordinal zero is the canonical legacy MediaID representative.
	attachment, err := r.s.attachments.GetForDownload(ctx, messageID, 0)
	if errors.Is(err, sql.ErrNoRows) {
		return sqlite.MessageAttachment{}, false, nil
	}
	if err != nil {
		return sqlite.MessageAttachment{}, false, fmt.Errorf(
			"map message %q attachment: %w",
			messageID,
			err,
		)
	}
	return attachment, true, nil
}

func (r referenceSource) sendStatusForMessage(message sqlite.Message) (string, error) {
	if strings.TrimSpace(message.MessageID) == "" {
		return "", nil
	}
	state, ok, err := r.s.outbox.LatestStateForLocalMessage(
		context.Background(),
		message.AccountID,
		message.MessageID,
	)
	if err != nil {
		return "", fmt.Errorf("map message %q send status: %w", message.MessageID, err)
	}
	if !ok {
		return "", nil
	}
	switch state {
	case sqlite.OutboxQueued, sqlite.OutboxDispatching, sqlite.OutboxNotDispatched, sqlite.OutboxUncertain:
		return db.OutgoingSendStatusSending, nil
	case sqlite.OutboxConfirmed, sqlite.OutboxStoreFailed:
		return db.OutgoingSendStatusSent, nil
	case sqlite.OutboxRejected, sqlite.OutboxCanceled:
		return db.OutgoingSendStatusFailed, nil
	default:
		return "", nil
	}
}

// --- stats.go ---

func (r referenceSource) LatestConversationPreviews(ids []string) (map[string]string, error) {
	s := r.s
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
		attachment, hasAttachment, err := r.messageAttachment(
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

// --- messages.go ---

func (r referenceSource) GetMessagesByConversation(conversationID string, limit int) ([]*db.Message, error) {
	return r.getMessagesBefore(conversationID, 0, "", limit)
}

func (r referenceSource) GetMessagesByConversationBefore(
	conversationID string,
	beforeMS int64,
	beforeID string,
	limit int,
) ([]*db.Message, error) {
	return r.getMessagesBefore(conversationID, beforeMS, beforeID, limit)
}

func (r referenceSource) getMessagesBefore(
	conversationID string,
	beforeMS int64,
	beforeID string,
	limit int,
) ([]*db.Message, error) {
	s := r.s
	if err := s.ready(); err != nil {
		return nil, err
	}
	messages, err := s.messages.ListMessagesByConversation(
		context.Background(), s.resolveConversationID(conversationID), beforeMS, beforeID, limit,
	)
	if err != nil {
		return nil, err
	}
	return r.mapMessages(messages)
}

func (r referenceSource) GetMessagesByConversationAfter(
	conversationID string,
	afterMS int64,
	afterID string,
	limit int,
) ([]*db.Message, error) {
	s := r.s
	if err := s.ready(); err != nil {
		return nil, err
	}
	messages, err := s.messages.ListMessagesByConversationAfter(
		context.Background(), s.resolveConversationID(conversationID), afterMS, afterID, limit,
	)
	if err != nil {
		return nil, err
	}
	return r.mapMessages(messages)
}

func (r referenceSource) GetMessagesAroundMessage(
	conversationID string,
	messageID string,
	before int,
	after int,
) ([]*db.Message, error) {
	s := r.s
	if err := s.ready(); err != nil {
		return nil, err
	}
	messages, err := s.messages.ListMessagesAroundMessage(
		context.Background(), s.resolveConversationID(conversationID), messageID, before, after,
	)
	if errors.Is(err, sqlite.ErrNotFound) {
		return nil, db.ErrMessageNotFound
	}
	if err != nil {
		return nil, err
	}
	return r.mapMessages(messages)
}

// --- search.go ---

func (r referenceSource) SearchMessagesFiltered(
	query string,
	filter db.SearchFilter,
) ([]*db.Message, error) {
	s := r.s
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
	return r.mapMessages(messages)
}

// --- batch.go ---

func (r referenceSource) GetMessagesByConversationsRange(
	conversationIDs []string,
	afterMS, beforeMS int64,
	limit int,
) ([]*db.Message, error) {
	s := r.s
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

	var merged []sqlite.Message
	floorMS := afterMS
	for _, conversationID := range resolved {
		rows, err := s.conversationMessagesInRange(conversationID, floorMS, beforeMS, limit)
		if err != nil {
			return nil, err
		}
		merged = keepNewest(append(merged, rows...), limit)
		if len(merged) == limit {
			if oldest := merged[len(merged)-1].OccurredAtMS; oldest > floorMS {
				floorMS = oldest
			}
		}
	}

	for i, j := 0, len(merged)-1; i < j; i, j = i+1, j-1 {
		merged[i], merged[j] = merged[j], merged[i]
	}
	return r.mapMessages(merged)
}

// --- oracles for the batch-only methods ---

// GetConversationsByID's oracle: GetConversation per direct, existing ID.
func (r referenceSource) GetConversationsByID(ids []string) (map[string]*db.Conversation, error) {
	result := map[string]*db.Conversation{}
	for _, id := range ids {
		if id == "" || strings.TrimSpace(id) != id {
			continue
		}
		if _, err := r.s.store.GetConversation(id); err != nil {
			continue
		}
		conversation, err := r.GetConversation(id)
		if err != nil {
			return nil, err
		}
		result[id] = conversation
	}
	return result, nil
}

// LatestMessagesByConversation's oracle: GetMessagesByConversation(id, 1) per
// direct, existing ID.
func (r referenceSource) LatestMessagesByConversation(ids []string) (map[string][]*db.Message, error) {
	result := map[string][]*db.Message{}
	for _, id := range ids {
		if id == "" || strings.TrimSpace(id) != id {
			continue
		}
		if _, err := r.s.store.GetConversation(id); err != nil {
			continue
		}
		messages, err := r.GetMessagesByConversation(id, 1)
		if err != nil {
			return nil, err
		}
		result[id] = messages
	}
	return result, nil
}
