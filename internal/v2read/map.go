package v2read

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

// participantDTO mirrors the legacy participants JSON, including the
// is_me flag readers use to tell the account owner from the peer.
type participantDTO struct {
	Name   string `json:"name"`
	Number string `json:"number"`
	IsMe   bool   `json:"is_me,omitempty"`
}

type reactionDTO struct {
	Emoji  string   `json:"emoji"`
	Count  int      `json:"count"`
	Actors []string `json:"actors,omitempty"`
}

func platformForBridgeKey(bridgeKey string) string {
	bridgeKey = strings.TrimSpace(bridgeKey)
	switch bridgeKey {
	case "google_messages":
		return "sms"
	case "whatsmeow":
		return "whatsapp"
	case "signal_cli":
		return "signal"
	case "gchat", "imessage":
		return bridgeKey
	default:
		return bridgeKey
	}
}

func (s *Source) accountIndex() (map[string]sqlite.Account, error) {
	accounts, err := s.store.ListAccounts()
	if err != nil {
		return nil, err
	}
	return indexAccounts(accounts), nil
}

func indexAccounts(accounts []sqlite.Account) map[string]sqlite.Account {
	index := make(map[string]sqlite.Account, len(accounts))
	for _, account := range accounts {
		index[account.AccountID] = account
	}
	return index
}

// mapConversations maps conversations to legacy DTOs in input order. Every
// roster comes from one batched read (ListParticipantIdentities) rather than
// ListParticipants plus GetIdentity per participant, so the statement count no
// longer grows with the number of conversations or participants.
func (s *Source) mapConversations(
	conversations []sqlite.Conversation,
	accounts map[string]sqlite.Account,
) ([]*db.Conversation, error) {
	mapped := make([]*db.Conversation, 0, len(conversations))
	if len(conversations) == 0 {
		return mapped, nil
	}
	conversationIDs := make([]string, 0, len(conversations))
	for _, conversation := range conversations {
		conversationIDs = append(conversationIDs, conversation.ConversationID)
	}
	rosters, err := s.store.ListParticipantIdentities(conversationIDs)
	if err != nil {
		return nil, fmt.Errorf("map conversations: %w", err)
	}
	for _, conversation := range conversations {
		dto, err := mapConversation(conversation, accounts, rosters[conversation.ConversationID])
		if err != nil {
			return nil, err
		}
		mapped = append(mapped, dto)
	}
	return mapped, nil
}

func mapConversation(
	conversation sqlite.Conversation,
	accounts map[string]sqlite.Account,
	roster []sqlite.ParticipantIdentity,
) (*db.Conversation, error) {
	account, ok := accounts[conversation.AccountID]
	if !ok {
		return nil, fmt.Errorf(
			"map conversation %q: account %q is missing",
			conversation.ConversationID,
			conversation.AccountID,
		)
	}
	infos, err := participantInfos(conversation.ConversationID, roster)
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

type participantInfo struct {
	dto    participantDTO
	isSelf bool
}

// participantInfos renders one conversation's roster (every participant row,
// ordered by identity ID). A participant whose identity row is missing is an
// error, as GetIdentity's not-found was before rosters were batched.
func participantInfos(conversationID string, roster []sqlite.ParticipantIdentity) ([]participantInfo, error) {
	infos := make([]participantInfo, 0, len(roster))
	for _, participant := range roster {
		if !participant.IdentityFound {
			return nil, fmt.Errorf(
				"map conversation %q participant %q: %w",
				conversationID,
				participant.IdentityID,
				missingIdentityError(participant.IdentityID),
			)
		}
		name := strings.TrimSpace(participant.ParticipantDisplayName)
		if name == "" {
			name = participant.IdentityDisplayName
		}
		infos = append(infos, participantInfo{
			dto: participantDTO{
				Name:   name,
				Number: participant.CanonicalValue,
				IsMe:   participant.IsSelf,
			},
			isSelf: participant.IsSelf,
		})
	}
	return infos, nil
}

// missingIdentityError is the error GetIdentity returns for an absent row.
func missingIdentityError(identityID string) error {
	return fmt.Errorf("identity %q: %w", identityID, sqlite.ErrNotFound)
}

func participantsJSON(conversationID string, infos []participantInfo) (string, error) {
	dtos := make([]participantDTO, 0, len(infos))
	for _, info := range infos {
		dtos = append(dtos, info.dto)
	}
	encoded, err := json.Marshal(dtos)
	if err != nil {
		return "", fmt.Errorf("map conversation %q participants: %w", conversationID, err)
	}
	return string(encoded), nil
}

// directPeerName picks the display name for a direct conversation from its
// remote peer, preferring the peer's name and falling back to the canonical
// address so the thread is at least addressable by number.
func directPeerName(infos []participantInfo) string {
	for _, info := range infos {
		if info.isSelf {
			continue
		}
		if name := strings.TrimSpace(info.dto.Name); name != "" {
			return name
		}
		if number := strings.TrimSpace(info.dto.Number); number != "" {
			return number
		}
	}
	return ""
}

// messageRelations holds what a page of messages renders besides its own rows,
// each loaded for the whole page in one batched read: sender identities,
// ordinal-0 attachments, and outgoing messages' newest outbox states.
type messageRelations struct {
	senders     map[string]sqlite.Identity
	attachments map[string]sqlite.MessageAttachment
	// sendStates is keyed by account ID, then local message ID, because an
	// outbox row belongs to its account (LatestStateForLocalMessage scopes it).
	sendStates map[string]map[string]sqlite.OutboxState
}

// mapMessages maps messages to legacy DTOs in input order. Reactions, senders,
// attachments and send states are each read once per page (batched by key)
// rather than once per message, so a page costs a fixed handful of statements
// however many messages it holds.
func (s *Source) mapMessages(
	messages []sqlite.Message,
) ([]*db.Message, error) {
	ctx := context.Background()
	accounts, err := s.accountIndex()
	if err != nil {
		return nil, fmt.Errorf("map messages: %w", err)
	}
	mapped := make([]*db.Message, 0, len(messages))
	if len(messages) == 0 {
		return mapped, nil
	}
	messageIDs := make([]string, 0, len(messages))
	for _, message := range messages {
		messageIDs = append(messageIDs, message.MessageID)
	}
	reactions, err := s.reactions.ReactionsForMessages(ctx, messageIDs)
	if err != nil {
		return nil, fmt.Errorf("map messages: load reactions: %w", err)
	}
	relations, err := s.loadMessageRelations(ctx, messages, messageIDs)
	if err != nil {
		return nil, err
	}
	for _, message := range messages {
		dto, err := mapMessage(message, accounts, reactions[message.MessageID], relations)
		if err != nil {
			return nil, err
		}
		mapped = append(mapped, dto)
	}
	return mapped, nil
}

func (s *Source) loadMessageRelations(
	ctx context.Context,
	messages []sqlite.Message,
	messageIDs []string,
) (messageRelations, error) {
	var senderIDs []string
	outgoingByAccount := make(map[string][]string)
	var accountOrder []string
	for _, message := range messages {
		if message.SenderIdentityID != nil {
			senderIDs = append(senderIDs, *message.SenderIdentityID)
		}
		if message.Direction != sqlite.MessageDirectionOutgoing ||
			strings.TrimSpace(message.MessageID) == "" {
			continue
		}
		if _, ok := outgoingByAccount[message.AccountID]; !ok {
			accountOrder = append(accountOrder, message.AccountID)
		}
		outgoingByAccount[message.AccountID] = append(outgoingByAccount[message.AccountID], message.MessageID)
	}

	relations := messageRelations{sendStates: make(map[string]map[string]sqlite.OutboxState, len(accountOrder))}
	var err error
	if len(senderIDs) > 0 {
		if relations.senders, err = s.store.IdentitiesByID(senderIDs); err != nil {
			return messageRelations{}, fmt.Errorf("map messages: load senders: %w", err)
		}
	}
	if relations.attachments, err = s.attachments.ListForDownload(ctx, messageIDs, 0); err != nil {
		return messageRelations{}, fmt.Errorf("map messages: load attachments: %w", err)
	}
	for _, accountID := range accountOrder {
		states, err := s.outbox.LatestStatesForLocalMessages(ctx, accountID, outgoingByAccount[accountID])
		if err != nil {
			return messageRelations{}, fmt.Errorf("map messages: load send states: %w", err)
		}
		relations.sendStates[accountID] = states
	}
	return relations, nil
}

func mapMessage(
	message sqlite.Message,
	accounts map[string]sqlite.Account,
	reactionRows []sqlite.ReactionRow,
	relations messageRelations,
) (*db.Message, error) {
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
		identity, ok := relations.senders[*message.SenderIdentityID]
		if !ok {
			return nil, fmt.Errorf(
				"map message %q sender %q: %w",
				message.MessageID,
				*message.SenderIdentityID,
				missingIdentityError(*message.SenderIdentityID),
			)
		}
		dto.SenderNumber = identity.CanonicalValue
		dto.SenderName = identity.DisplayName
	}
	if message.ReplyToRemoteID != nil {
		dto.ReplyToID = *message.ReplyToRemoteID
	}
	if dto.IsFromMe && strings.TrimSpace(message.MessageID) != "" {
		if state, ok := relations.sendStates[message.AccountID][message.MessageID]; ok {
			dto.Status = sendStatusForState(state)
		}
	}
	// R2 historical migration and every current decoder number attachments from
	// zero, so ordinal zero is the canonical legacy MediaID representative.
	if attachment, ok := relations.attachments[message.MessageID]; ok {
		dto.MediaID = fmt.Sprintf("v2msg:%s:%d", message.MessageID, attachment.Ordinal)
		dto.MimeType = attachment.MIME
	}
	return dto, nil
}

func mapReactions(rows []sqlite.ReactionRow) (string, error) {
	if len(rows) == 0 {
		return "", nil
	}
	type group struct {
		dto   reactionDTO
		first int64
		seen  map[string]struct{}
	}
	groups := map[string]*group{}
	for _, row := range rows {
		item := groups[row.Emoji]
		if item == nil {
			item = &group{dto: reactionDTO{Emoji: row.Emoji}, first: row.OccurredAtMS, seen: map[string]struct{}{}}
			groups[row.Emoji] = item
		}
		actor := row.ReactorLabel
		if row.ReactorIsSelf {
			actor = "me"
		} else if row.ReactorCanonical != "" {
			actor = row.ReactorCanonical
		}
		if actor != "" {
			if _, ok := item.seen[actor]; !ok {
				item.seen[actor] = struct{}{}
				item.dto.Actors = append(item.dto.Actors, actor)
			}
		}
		item.dto.Count++
	}
	ordered := make([]*group, 0, len(groups))
	for _, item := range groups {
		ordered = append(ordered, item)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].first != ordered[j].first {
			return ordered[i].first < ordered[j].first
		}
		return ordered[i].dto.Emoji < ordered[j].dto.Emoji
	})
	dtos := make([]reactionDTO, 0, len(ordered))
	for _, item := range ordered {
		dtos = append(dtos, item.dto)
	}
	encoded, err := json.Marshal(dtos)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

// sendStatusForState maps an outgoing message's most recent outbox delivery
// state onto the legacy status vocabulary the web UI already renders
// (sending/sent/failed). It is deliberately honest per the durable-send
// contract: queued/dispatching/not_dispatched/uncertain all read as "sending"
// — never "failed" — because none of them is a settled failure and an
// ambiguous send must not be shown as failed. store_failed reads "sent"
// (the transport delivered; only the local record needs repair). Terminal
// rejected/canceled read "failed". A message with no outbox row (received, or
// pre-outbox) carries no status, so callers do not call this for it.
func sendStatusForState(state sqlite.OutboxState) string {
	switch state {
	case sqlite.OutboxQueued, sqlite.OutboxDispatching, sqlite.OutboxNotDispatched, sqlite.OutboxUncertain:
		return db.OutgoingSendStatusSending
	case sqlite.OutboxConfirmed, sqlite.OutboxStoreFailed:
		return db.OutgoingSendStatusSent
	case sqlite.OutboxRejected, sqlite.OutboxCanceled:
		return db.OutgoingSendStatusFailed
	default:
		return ""
	}
}
