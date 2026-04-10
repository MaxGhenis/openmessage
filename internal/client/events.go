package client

import (
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/rs/zerolog"
	"go.mau.fi/mautrix-gmessages/pkg/libgm"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/events"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"

	"github.com/maxghenis/openmessage/internal/db"
)

// OnDisconnect is called when the client fatally disconnects (e.g. unpaired).
type OnDisconnect func()

type EventHandler struct {
	Store                  *db.Store
	Logger                 zerolog.Logger
	SessionPath            string
	Client                 *Client
	OnConversationsChange  func()
	OnDisconnect           OnDisconnect
	OnIncomingMessage      func(*db.Message)
	OnPendingMedia         func(conversationID, messageID string)
	OnMessagesChange       func(string)
	OnRealtimeGapRecovered func(string)
	OnTypingChange         func(conversationID, senderName, senderNumber string, typing bool)
}

func (h *EventHandler) Handle(rawEvt any) {
	switch evt := rawEvt.(type) {
	case *events.ClientReady:
		h.handleClientReady(evt)
	case *libgm.WrappedMessage:
		h.handleMessage(evt)
	case *gmproto.Conversation:
		h.handleConversation(evt)
	case *events.AuthTokenRefreshed:
		h.handleAuthRefresh()
	case *events.PairSuccessful:
		h.Logger.Info().Str("phone_id", evt.PhoneID).Msg("Pairing successful")
	case *events.ListenFatalError:
		h.Logger.Error().Err(evt.Error).Msg("Listen fatal error")
		if h.OnDisconnect != nil {
			h.OnDisconnect()
		}
	case *events.ListenTemporaryError:
		h.Logger.Warn().Err(evt.Error).Msg("Listen temporary error")
	case *events.ListenRecovered:
		h.Logger.Info().Msg("Listen recovered")
		if h.OnRealtimeGapRecovered != nil {
			h.OnRealtimeGapRecovered("listen_recovered")
		}
	case *events.PhoneNotResponding:
		h.Logger.Warn().Msg("Phone not responding")
	case *events.PhoneRespondingAgain:
		h.Logger.Info().Msg("Phone responding again")
		if h.OnRealtimeGapRecovered != nil {
			h.OnRealtimeGapRecovered("phone_responding_again")
		}
	case *gmproto.TypingData:
		h.handleTyping(evt)
	default:
		h.Logger.Debug().Type("type", evt).Msg("Unhandled event")
	}
}

func (h *EventHandler) handleClientReady(evt *events.ClientReady) {
	h.Logger.Info().
		Str("session_id", evt.SessionID).
		Int("conversations", len(evt.Conversations)).
		Msg("Client ready")

	for _, conv := range evt.Conversations {
		h.storeConversation(conv)
	}
	if len(evt.Conversations) > 0 && h.OnConversationsChange != nil {
		h.OnConversationsChange()
	}
}

func (h *EventHandler) handleMessage(evt *libgm.WrappedMessage) {
	msg := evt.Message
	body := ExtractMessageBody(msg)
	senderName, senderNumber := ExtractSenderInfo(msg)

	status := "unknown"
	if ms := msg.GetMessageStatus(); ms != nil {
		status = ms.GetStatus().String()
	}

	dbMsg := &db.Message{
		MessageID:      msg.GetMessageID(),
		ConversationID: msg.GetConversationID(),
		SenderName:     senderName,
		SenderNumber:   senderNumber,
		Body:           body,
		TimestampMS:    msg.GetTimestamp() / 1000, // proto timestamp is microseconds
		Status:         status,
		IsFromMe:       MessageIsFromMe(msg),
	}

	if media := ExtractMediaInfo(msg); media != nil {
		dbMsg.MediaID = media.MediaID
		dbMsg.MimeType = media.MimeType
		dbMsg.DecryptionKey = hex.EncodeToString(media.DecryptionKey)
	}

	if reactions := ExtractReactions(msg); reactions != nil {
		if b, err := json.Marshal(reactions); err == nil {
			dbMsg.Reactions = string(b)
		}
	}
	dbMsg.ReplyToID = ExtractReplyToID(msg)

	if err := h.Store.UpsertMessage(dbMsg); err != nil {
		h.Logger.Error().Err(err).Str("msg_id", dbMsg.MessageID).Msg("Failed to store message")
		return
	}
	if err := h.Store.BumpConversationTimestamp(dbMsg.ConversationID, dbMsg.TimestampMS); err != nil {
		h.Logger.Warn().Err(err).Str("conv_id", dbMsg.ConversationID).Msg("Failed to update conversation timestamp from message")
	}

	// When our sent message echoes back with a real server ID, clean up the
	// exact tmp_ placeholder we stored at send time to avoid duplicates.
	if dbMsg.IsFromMe {
		if tmpID := msg.GetTmpID(); tmpID != "" && tmpID != dbMsg.MessageID {
			if err := h.Store.DeleteMessageByID(tmpID); err == nil {
				h.Logger.Debug().Str("tmp_id", tmpID).Str("conv_id", dbMsg.ConversationID).Msg("Cleaned up tmp message")
			}
		}
	}

	h.Logger.Debug().
		Str("msg_id", dbMsg.MessageID).
		Str("from", senderName).
		Bool("is_old", evt.IsOld).
		Msg("Stored message")

	if !dbMsg.IsFromMe && !evt.IsOld && h.OnIncomingMessage != nil {
		h.OnIncomingMessage(dbMsg)
	}
	if !dbMsg.IsFromMe && !evt.IsOld && h.OnPendingMedia != nil && messageNeedsPendingMediaRefresh(msg, dbMsg) {
		h.OnPendingMedia(dbMsg.ConversationID, dbMsg.MessageID)
	}
	if h.OnMessagesChange != nil {
		h.OnMessagesChange(dbMsg.ConversationID)
	}
	if h.OnConversationsChange != nil {
		h.OnConversationsChange()
	}
}

func (h *EventHandler) handleConversation(conv *gmproto.Conversation) {
	if !h.storeConversation(conv) {
		return
	}
	if h.OnConversationsChange != nil {
		h.OnConversationsChange()
	}
}

func (h *EventHandler) storeConversation(conv *gmproto.Conversation) bool {
	participantsJSON := "[]"
	if ps := conv.GetParticipants(); len(ps) > 0 {
		type pInfo struct {
			Name   string `json:"name"`
			Number string `json:"number"`
			IsMe   bool   `json:"is_me,omitempty"`
		}
		var infos []pInfo
		for _, p := range ps {
			info := pInfo{
				Name: p.GetFullName(),
				IsMe: p.GetIsMe(),
			}
			if id := p.GetID(); id != nil {
				info.Number = id.GetNumber()
			}
			if info.Number == "" {
				info.Number = p.GetFormattedNumber()
			}
			infos = append(infos, info)
		}
		if b, err := json.Marshal(infos); err == nil {
			participantsJSON = string(b)
		}
	}

	unread := 0
	if conv.GetUnread() {
		unread = 1
	}

	dbConv := &db.Conversation{
		ConversationID: conv.GetConversationID(),
		Name:           conv.GetName(),
		IsGroup:        conv.GetIsGroupChat(),
		Participants:   participantsJSON,
		LastMessageTS:  conv.GetLastMessageTimestamp() / 1000, // microseconds to milliseconds
		UnreadCount:    unread,
	}

	if err := h.Store.UpsertConversation(dbConv); err != nil {
		h.Logger.Error().Err(err).Str("conv_id", dbConv.ConversationID).Msg("Failed to store conversation")
		return false
	}
	h.Logger.Debug().Str("conv_id", dbConv.ConversationID).Str("name", dbConv.Name).Msg("Stored conversation")
	return true
}

func (h *EventHandler) handleTyping(evt *gmproto.TypingData) {
	if evt == nil || h.OnTypingChange == nil {
		return
	}
	conversationID := strings.TrimSpace(evt.GetConversationID())
	if conversationID == "" {
		return
	}
	senderNumber := strings.TrimSpace(evt.GetUser().GetNumber())
	senderName := h.typingSenderName(conversationID, senderNumber)
	typing := evt.GetType() == gmproto.TypingTypes_STARTED_TYPING
	h.Logger.Debug().
		Str("conv_id", conversationID).
		Str("sender_number", senderNumber).
		Bool("typing", typing).
		Msg("Received typing event")
	h.OnTypingChange(conversationID, senderName, senderNumber, typing)
}

func (h *EventHandler) typingSenderName(conversationID, senderNumber string) string {
	conv, err := h.Store.GetConversation(conversationID)
	if err != nil || conv == nil {
		return ""
	}

	type participant struct {
		Name   string `json:"name"`
		Number string `json:"number"`
		IsMe   bool   `json:"is_me,omitempty"`
	}
	var participants []participant
	if err := json.Unmarshal([]byte(conv.Participants), &participants); err == nil {
		normalizedSender := normalizeTypingParticipant(senderNumber)
		for _, participant := range participants {
			if participant.IsMe {
				continue
			}
			if normalizedSender != "" && normalizeTypingParticipant(participant.Number) == normalizedSender {
				return strings.TrimSpace(participant.Name)
			}
		}
	}

	if !conv.IsGroup {
		return strings.TrimSpace(conv.Name)
	}
	return ""
}

func messageNeedsPendingMediaRefresh(msg *gmproto.Message, dbMsg *db.Message) bool {
	if msg == nil || dbMsg == nil || dbMsg.IsFromMe {
		return false
	}
	if ExtractMediaInfo(msg) != nil {
		return false
	}
	if msg.GetType() == 3 {
		return true
	}
	body := strings.ToLower(strings.TrimSpace(dbMsg.Body))
	return body != "" && strings.HasSuffix(body, "from phone")
}

func normalizeTypingParticipant(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	replacer := strings.NewReplacer(" ", "", "-", "", "(", "", ")", "")
	return replacer.Replace(value)
}

func (h *EventHandler) handleAuthRefresh() {
	if h.Client == nil || h.SessionPath == "" {
		return
	}
	sessionData, err := h.Client.SessionData()
	if err != nil {
		h.Logger.Error().Err(err).Msg("Failed to get session data for save")
		return
	}
	if err := SaveSession(h.SessionPath, sessionData); err != nil {
		h.Logger.Error().Err(err).Msg("Failed to save refreshed session")
		return
	}
	h.Logger.Debug().Msg("Saved refreshed auth token")
}
