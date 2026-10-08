package client

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/mautrix-gmessages/pkg/libgm"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/events"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"

	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/sim"
)

// OnSessionInvalid is called when the session is genuinely dead (server-side
// logout / invalid credentials) and the user must re-pair. The session file
// should be deleted.
type OnSessionInvalid func()

// OnConnectionLost is called when the live connection dropped but the session
// is (likely) still valid — a transient error. The session must be KEPT so a
// reconnect can succeed; deleting it here would force a spurious manual re-pair
// on a mere network blip or token-refresh race.
type OnConnectionLost func()

type EventHandler struct {
	Store                    *db.Store
	Logger                   zerolog.Logger
	SessionPath              string
	Client                   *Client
	Now                      func() time.Time
	PersistCookiesEvery      time.Duration
	OnConversationsChange    func()
	OnSessionInvalid         OnSessionInvalid
	OnConnectionLost         OnConnectionLost
	OnIncomingMessage        func(*db.Message)
	OnPendingMedia           func(conversationID, messageID string)
	OnMessagesChange         func(string)
	OnRealtimeGapRecovered   func(string)
	OnTypingChange           func(conversationID, senderName, senderNumber string, typing bool)
	OnGoogleAvatarCandidates func([]db.ContactAvatarCandidate)
	OnPhoneRespondingChange  func(bool)
	// SIMs receives the phone's SIM cards from Settings events (may be nil).
	SIMs *sim.Registry

	cookieSaveMu     sync.Mutex
	nextCookieSaveAt time.Time
	lastCookieHash   [32]byte
}

// cookieSaveRetryDivisor shortens the cookie-persist throttle after a failed
// save. A transient failure then costs a fraction of the interval instead of a
// full one, while a persistent failure still cannot turn every inbound event
// into a write attempt.
const cookieSaveRetryDivisor = 10

func (h *EventHandler) Handle(rawEvt any) {
	switch evt := rawEvt.(type) {
	case *events.ClientReady:
		h.handleClientReady(evt)
	case *libgm.WrappedMessage:
		h.handleMessage(evt)
	case *gmproto.Conversation:
		h.handleConversation(evt)
	case *gmproto.Settings:
		h.handleSettings(evt)
	case *events.AuthTokenRefreshed:
		h.handleAuthRefresh()
	case *events.PairSuccessful:
		h.Logger.Info().Str("phone_id", evt.PhoneID).Msg("Pairing successful")
	case *events.ListenFatalError:
		// Treat as transient: mark the connection lost (keep the session) and
		// let the reconnect watchdog retry. libgm raises this on a single
		// failed token refresh or a one-off 401, so deleting the session here
		// would force a needless re-pair. A genuine logout arrives separately
		// as GaiaLoggedOut (handled below) and DOES drop the session.
		h.Logger.Error().Err(evt.Error).Msg("Listen fatal error — marking connection lost")
		if h.OnConnectionLost != nil {
			h.OnConnectionLost()
		}
	case *events.GaiaLoggedOut:
		// Explicit server-side logout: the session is genuinely dead. Without
		// handling this, libgm keeps long-polling and getting "logged out"
		// replies while Connected stays true — the classic zombie where SMS
		// silently stops for months.
		h.Logger.Warn().Msg("Google account logged out server-side — session invalid")
		if h.OnSessionInvalid != nil {
			h.OnSessionInvalid()
		}
	case *events.PingFailed:
		// Repeated ping failures mean the long-poll is no longer healthy.
		// Surface it and, once it persists, mark the connection lost so the
		// watchdog reconnects instead of sitting in a silent zombie state.
		h.Logger.Warn().Err(evt.Error).Int("count", evt.ErrorCount).Msg("Google Messages ping failed")
		if evt.ErrorCount >= 3 && h.OnConnectionLost != nil {
			h.OnConnectionLost()
		}
	case *events.NoDataReceived:
		h.Logger.Debug().Msg("Google Messages long-poll received no data")
	case *events.ListenTemporaryError:
		h.Logger.Warn().Err(evt.Error).Msg("Listen temporary error")
	case *events.ListenRecovered:
		h.Logger.Info().Msg("Listen recovered")
		if h.OnRealtimeGapRecovered != nil {
			h.OnRealtimeGapRecovered("listen_recovered")
		}
	case *events.PhoneNotResponding:
		h.Logger.Warn().Msg("Phone not responding")
		if h.OnPhoneRespondingChange != nil {
			h.OnPhoneRespondingChange(false)
		}
	case *events.PhoneRespondingAgain:
		h.Logger.Info().Msg("Phone responding again")
		if h.OnPhoneRespondingChange != nil {
			h.OnPhoneRespondingChange(true)
		}
		if h.OnRealtimeGapRecovered != nil {
			h.OnRealtimeGapRecovered("phone_responding_again")
		}
	case *gmproto.TypingData:
		h.handleTyping(evt)
	default:
		h.Logger.Debug().Type("type", evt).Msg("Unhandled event")
	}

	switch rawEvt.(type) {
	case *libgm.WrappedMessage, *gmproto.Conversation, *events.ListenRecovered, *events.ClientReady:
		h.maybePersistRotatedCookies()
	}
}

func (h *EventHandler) handleClientReady(evt *events.ClientReady) {
	h.Logger.Info().
		Str("session_id", evt.SessionID).
		Int("conversations", len(evt.Conversations)).
		Msg("Client ready")
	if h.OnPhoneRespondingChange != nil {
		h.OnPhoneRespondingChange(true)
	}

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

	// iMessage tapbacks (e.g. `Loved "see you then"`) arrive from iPhones as
	// plain SMS/RCS text. Convert them into an emoji reaction on the message
	// they refer to instead of storing them as a separate message.
	if applied, err := h.Store.ApplyTapback(dbMsg); err != nil {
		h.Logger.Warn().Err(err).Str("msg_id", dbMsg.MessageID).Msg("Failed to apply tapback")
	} else if applied {
		h.Logger.Debug().Str("msg_id", dbMsg.MessageID).Str("conv_id", dbMsg.ConversationID).Msg("Applied tapback as reaction")
		if h.OnMessagesChange != nil {
			h.OnMessagesChange(dbMsg.ConversationID)
		}
		if h.OnConversationsChange != nil {
			h.OnConversationsChange()
		}
		return
	}

	// Drop completed contentless stubs (e.g. an empty message that group
	// activity leaks into a 1:1 thread). They would render as "Empty message"
	// and wrongly surface the conversation in recents.
	if db.IsEmptyStubMessage(dbMsg) {
		h.Logger.Debug().Str("msg_id", dbMsg.MessageID).Str("conv_id", dbMsg.ConversationID).Msg("Skipped empty stub message")
		return
	}

	if err := h.Store.UpsertMessage(dbMsg); err != nil {
		h.Logger.Error().Err(err).Str("msg_id", dbMsg.MessageID).Msg("Failed to store message")
		return
	}
	// Only real content advances inbox recency. A contentless stub (e.g. an
	// emoji reaction made in a group that arrives as an empty message in the
	// reactor's 1:1 thread) must not float that conversation to the top.
	if err := h.Store.AdvanceConversationRecency(dbMsg); err != nil {
		h.Logger.Warn().Err(err).Str("conv_id", dbMsg.ConversationID).Msg("Failed to update conversation timestamp from message")
	}

	// When our sent message echoes back with a real server ID, clean up the
	// exact tmp_ placeholder we stored at send time to avoid duplicates.
	if dbMsg.IsFromMe {
		if tmpID := msg.GetTmpID(); tmpID != "" && tmpID != dbMsg.MessageID {
			h.cleanupTmpPlaceholder(tmpID, dbMsg.ConversationID)
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
	if !dbMsg.IsFromMe && !evt.IsOld && h.OnPhoneRespondingChange != nil {
		h.OnPhoneRespondingChange(true)
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

// tmpPlaceholderRetryDelays spaces out the retried deletes of a tmp_ row after
// its echo arrived. The echo regularly lands during the send RPC, i.e. before
// the send path has written the placeholder (observed: 246 ms early), so a
// single immediate delete finds nothing and the thread shows the message
// twice - once delivered, once forever "sending".
var tmpPlaceholderRetryDelays = []time.Duration{500 * time.Millisecond, 2 * time.Second, 8 * time.Second}

// cleanupTmpPlaceholder deletes the tmp_ row for an echoed send now and again
// after each retry delay, so a placeholder written after the echo is still
// removed; the thread is republished when a late delete actually hits.
func (h *EventHandler) cleanupTmpPlaceholder(tmpID, conversationID string) {
	deleted, err := h.Store.DeleteMessageByIDIfExists(tmpID)
	if err == nil && deleted {
		h.Logger.Debug().Str("tmp_id", tmpID).Str("conv_id", conversationID).Msg("Cleaned up tmp message")
		return
	}
	go func() {
		for _, delay := range tmpPlaceholderRetryDelays {
			time.Sleep(delay)
			deleted, err := h.Store.DeleteMessageByIDIfExists(tmpID)
			if err != nil || !deleted {
				continue
			}
			h.Logger.Debug().Str("tmp_id", tmpID).Str("conv_id", conversationID).Dur("after", delay).Msg("Cleaned up late tmp message")
			if h.OnMessagesChange != nil {
				h.OnMessagesChange(conversationID)
			}
			return
		}
	}()
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
	participantsJSON, avatarCandidates := BuildParticipantsJSON(conv, "live")

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

	if err := h.Store.ApplyConversationSnapshot(dbConv); err != nil {
		h.Logger.Error().Err(err).Str("conv_id", dbConv.ConversationID).Msg("Failed to store conversation")
		return false
	}
	if h.OnGoogleAvatarCandidates != nil && len(avatarCandidates) > 0 {
		h.OnGoogleAvatarCandidates(avatarCandidates)
	}
	h.Logger.Debug().Str("conv_id", dbConv.ConversationID).Str("name", dbConv.Name).Msg("Stored conversation")
	return true
}

// handleSettings records the phone's SIM cards (carrier, number, slot) so
// send replies and message labels can name them.
func (h *EventHandler) handleSettings(evt *gmproto.Settings) {
	if evt == nil || h.SIMs == nil || len(evt.GetSIMCards()) == 0 {
		return
	}
	h.SIMs.SetCards(evt.GetSIMCards())
	h.Logger.Info().Int("sim_cards", len(evt.GetSIMCards())).Msg("Received SIM cards from phone settings")
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
	authData := h.Client.GM.AuthData
	authData.CookiesLock.RLock()
	sessionData, err := h.Client.SessionData()
	authData.CookiesLock.RUnlock()
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

func (h *EventHandler) maybePersistRotatedCookies() {
	if h.Client == nil || h.SessionPath == "" {
		return
	}

	now := time.Now()
	if h.Now != nil {
		now = h.Now()
	}
	interval := h.PersistCookiesEvery
	if interval == 0 {
		interval = 5 * time.Minute
	}

	h.cookieSaveMu.Lock()
	defer h.cookieSaveMu.Unlock()
	if !h.nextCookieSaveAt.IsZero() && now.Before(h.nextCookieSaveAt) {
		return
	}
	// The throttle covers every outcome, not just a successful save: these
	// events arrive per message, so an unthrottled failure path would marshal
	// and rewrite the session on each one. Failures instead come back after
	// interval/cookieSaveRetryDivisor and leave lastCookieHash untouched, so
	// the retry still sees the pending rotation and a transient error does not
	// cost a full interval of durability.
	h.nextCookieSaveAt = now.Add(interval)
	retryAt := now.Add(interval / cookieSaveRetryDivisor)

	authData := h.Client.GM.AuthData
	authData.CookiesLock.RLock()
	cookiesJSON, err := json.Marshal(authData.Cookies)
	if err != nil {
		// Keeps the full interval on purpose: a marshal failure is deterministic
		// for these cookies, so an early retry only repeats it.
		authData.CookiesLock.RUnlock()
		h.Logger.Warn().Err(err).Msg("Failed to marshal rotated Google cookies")
		return
	}
	cookieHash := sha256.Sum256(cookiesJSON)
	if cookieHash == h.lastCookieHash {
		authData.CookiesLock.RUnlock()
		return
	}
	sessionData, err := h.Client.SessionData()
	authData.CookiesLock.RUnlock()
	if err != nil {
		h.nextCookieSaveAt = retryAt
		h.Logger.Warn().Err(err).Msg("Failed to get session data for rotated cookie save")
		return
	}
	if err := SaveSession(h.SessionPath, sessionData); err != nil {
		h.nextCookieSaveAt = retryAt
		h.Logger.Warn().Err(err).Msg("Failed to persist rotated Google cookies")
		return
	}
	h.lastCookieHash = cookieHash
	h.Logger.Debug().Msg("persisted rotated Google cookies")
}
