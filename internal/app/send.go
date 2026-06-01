package app

import (
	"fmt"
	"strings"
	"time"

	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"

	"github.com/maxghenis/openmessage/internal/db"
)

var (
	sendWhatsAppConversationText = func(a *App, conversationID, body, replyToID string) (*db.Message, error) {
		return a.SendWhatsAppText(conversationID, body, replyToID)
	}
	sendSignalConversationText = func(a *App, conversationID, body, replyToID string) (*db.Message, error) {
		return a.SendSignalText(conversationID, body, replyToID)
	}
)

func (a *App) SendTextToConversation(conversationID, body string) (*db.Conversation, *db.Message, error) {
	conv, err := a.Store.GetConversation(conversationID)
	if err != nil {
		return nil, nil, fmt.Errorf("get conversation: %w", err)
	}
	if conv == nil {
		return nil, nil, fmt.Errorf("conversation %s not found", conversationID)
	}

	switch normalizeConversationPlatform(conv.SourcePlatform) {
	case "whatsapp":
		msg, err := sendWhatsAppConversationText(a, conversationID, body, "")
		if err != nil {
			return conv, nil, fmt.Errorf("send WhatsApp message: %w", err)
		}
		if err := a.Store.RecordOutgoingMessage(msg, ""); err != nil {
			return conv, nil, fmt.Errorf("persist sent message: %w", err)
		}
		return conv, msg, nil
	case "signal":
		msg, err := sendSignalConversationText(a, conversationID, body, "")
		if err != nil {
			return conv, nil, fmt.Errorf("send Signal message: %w", err)
		}
		if err := a.Store.RecordOutgoingMessage(msg, ""); err != nil {
			return conv, nil, fmt.Errorf("persist sent message: %w", err)
		}
		return conv, msg, nil
	case "sms":
		gmConv, err := getGoogleConversationForSend(a, conversationID)
		if err != nil {
			return conv, nil, fmt.Errorf("get Google conversation: %w", err)
		}
		payload, err := buildGoogleTextPayload(gmConv, conversationID, body)
		if err != nil {
			return conv, nil, err
		}
		resp, err := sendGoogleTextPayload(a, payload)
		if err != nil {
			return conv, nil, fmt.Errorf("send Google message: %w", err)
		}
		if resp.GetStatus() != gmproto.SendMessageResponse_SUCCESS {
			return conv, nil, fmt.Errorf("send Google message: %s", resp.GetStatus().String())
		}
		msg := &db.Message{
			MessageID:      payload.TmpID,
			ConversationID: conversationID,
			Body:           body,
			IsFromMe:       true,
			TimestampMS:    time.Now().UnixMilli(),
			Status:         "OUTGOING_SENDING",
			SourcePlatform: "sms",
		}
		if err := a.Store.RecordOutgoingMessage(msg, ""); err != nil {
			return conv, nil, fmt.Errorf("persist sent message: %w", err)
		}
		return conv, msg, nil
	default:
		return conv, nil, fmt.Errorf("sending is not supported for platform %s via OpenMessage yet", conv.SourcePlatform)
	}
}

func normalizeConversationPlatform(platform string) string {
	switch strings.ToLower(strings.TrimSpace(platform)) {
	case "", "sms":
		return "sms"
	case "whatsapp", "signal":
		return strings.ToLower(strings.TrimSpace(platform))
	default:
		return strings.ToLower(strings.TrimSpace(platform))
	}
}

func buildGoogleTextPayload(conv *gmproto.Conversation, conversationID, body string) (*gmproto.SendMessageRequest, error) {
	if conv == nil {
		return nil, fmt.Errorf("get Google conversation: no conversation returned")
	}
	myParticipantID, simPayload := ExtractSIMAndParticipant(conv)
	return BuildSendPayload(conversationID, body, "", myParticipantID, simPayload), nil
}
