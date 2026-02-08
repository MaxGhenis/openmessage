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
	Store        *db.Store
	Logger       zerolog.Logger
	SessionPath  string
	Client       *Client
	OnDisconnect OnDisconnect
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
	case *events.PhoneNotResponding:
		h.Logger.Warn().Msg("Phone not responding")
	case *events.PhoneRespondingAgain:
		h.Logger.Info().Msg("Phone responding again")
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
		h.handleConversation(conv)
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
		IsFromMe:       msg.GetSenderParticipant() != nil && msg.GetSenderParticipant().GetIsMe(),
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

	// When our sent message echoes back with a real server ID, clean up the
	// tmp_ placeholder we stored at send time to avoid duplicates in the UI.
	if dbMsg.IsFromMe && !strings.HasPrefix(dbMsg.MessageID, "tmp_") {
		if n, err := h.Store.DeleteTmpMessages(dbMsg.ConversationID); err == nil && n > 0 {
			h.Logger.Debug().Int64("deleted", n).Str("conv_id", dbMsg.ConversationID).Msg("Cleaned up tmp messages")
		}
	}

	h.Logger.Debug().
		Str("msg_id", dbMsg.MessageID).
		Str("from", senderName).
		Bool("is_old", evt.IsOld).
		Msg("Stored message")
}

func (h *EventHandler) handleConversation(conv *gmproto.Conversation) {
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
		return
	}
	h.Logger.Debug().Str("conv_id", dbConv.ConversationID).Str("name", dbConv.Name).Msg("Stored conversation")
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
