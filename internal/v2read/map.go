package v2read

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

type participantDTO struct {
	Name   string `json:"name"`
	Number string `json:"number"`
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
	index := make(map[string]sqlite.Account, len(accounts))
	for _, account := range accounts {
		index[account.AccountID] = account
	}
	return index, nil
}

func (s *Source) mapConversation(
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
	participants, err := s.participantsJSON(conversation.ConversationID)
	if err != nil {
		return nil, err
	}
	return &db.Conversation{
		ConversationID:   conversation.ConversationID,
		Name:             conversation.Title,
		IsGroup:          conversation.Kind == sqlite.ConversationKindGroup,
		Participants:     participants,
		LastMessageTS:    conversation.LastMessageAtMS,
		UnreadCount:      0,
		SourcePlatform:   platformForBridgeKey(account.BridgeKey),
		IsFavorite:       conversation.IsFavorite,
		NotificationMode: string(conversation.NotificationMode),
	}, nil
}

func (s *Source) participantsJSON(conversationID string) (string, error) {
	participants, err := s.store.ListParticipants(conversationID)
	if err != nil {
		return "", fmt.Errorf("map conversation %q participants: %w", conversationID, err)
	}
	dtos := make([]participantDTO, 0, len(participants))
	for _, participant := range participants {
		identity, err := s.store.GetIdentity(participant.IdentityID)
		if err != nil {
			return "", fmt.Errorf(
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
		dtos = append(dtos, participantDTO{
			Name:   name,
			Number: identity.CanonicalValue,
		})
	}
	encoded, err := json.Marshal(dtos)
	if err != nil {
		return "", fmt.Errorf("map conversation %q participants: %w", conversationID, err)
	}
	return string(encoded), nil
}

func (s *Source) mapMessages(
	messages []sqlite.Message,
) ([]*db.Message, error) {
	accounts, err := s.accountIndex()
	if err != nil {
		return nil, fmt.Errorf("map messages: %w", err)
	}
	mapped := make([]*db.Message, 0, len(messages))
	for _, message := range messages {
		dto, err := s.mapMessage(message, accounts)
		if err != nil {
			return nil, err
		}
		mapped = append(mapped, dto)
	}
	return mapped, nil
}

func (s *Source) mapMessage(
	message sqlite.Message,
	accounts map[string]sqlite.Account,
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
	attachment, ok, err := s.messageAttachment(context.Background(), message.MessageID)
	if err != nil {
		return nil, err
	}
	if ok {
		dto.MediaID = fmt.Sprintf("v2msg:%s:%d", message.MessageID, attachment.Ordinal)
		dto.MimeType = attachment.MIME
	}
	return dto, nil
}

func (s *Source) messageAttachment(
	ctx context.Context,
	messageID string,
) (sqlite.MessageAttachment, bool, error) {
	// R2 historical migration and every current decoder number attachments from
	// zero, so ordinal zero is the canonical legacy MediaID representative.
	attachment, err := s.attachments.GetForDownload(ctx, messageID, 0)
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
