package app

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"

	"github.com/maxghenis/openmessage/internal/db"
)

const (
	schedulerTickInterval = 20 * time.Second
	scheduleMaxAttempts   = 5
)

// StartScheduler launches the background loop that sends due scheduled messages.
// It catches up immediately on startup (for messages whose time passed while the
// app was closed), then ticks.
func (a *App) StartScheduler() {
	go func() {
		a.processDueScheduledMessages()
		ticker := time.NewTicker(schedulerTickInterval)
		defer ticker.Stop()
		for range ticker.C {
			a.processDueScheduledMessages()
		}
	}()
}

func (a *App) processDueScheduledMessages() {
	due, err := a.Store.GetDueScheduledMessages(time.Now().UnixMilli())
	if err != nil {
		a.Logger.Warn().Err(err).Msg("Scheduler: failed to load due messages")
		return
	}
	for _, sm := range due {
		a.sendScheduledMessage(sm)
	}
}

func (a *App) sendScheduledMessage(sm *db.ScheduledMessage) {
	// Atomic claim — only one worker/tick wins, so a message is sent once.
	claimed, err := a.Store.ClaimScheduledMessage(sm.ID)
	if err != nil || !claimed {
		return
	}

	// If the route's platform isn't connected, retry later (don't lose it),
	// unless we've already burned through the attempt budget.
	if !a.routeReady(sm.ConversationID) {
		a.retryOrFail(sm, "platform not connected")
		return
	}

	var msg *db.Message
	if sm.MediaFilename != "" {
		msg, err = a.sendScheduledMedia(sm)
	} else {
		send := a.sendScheduledText
		if a.sendTextOverride != nil {
			send = a.sendTextOverride
		}
		msg, err = send(sm.ConversationID, sm.Body, sm.ReplyToID)
	}
	if err != nil {
		a.Logger.Warn().Err(err).Str("scheduled_id", sm.ID).Msg("Scheduler: send failed")
		a.retryOrFail(sm, err.Error())
		return
	}

	sentID := ""
	if msg != nil {
		sentID = msg.MessageID
	}
	if err := a.Store.MarkScheduledMessageSent(sm.ID, sentID); err != nil {
		a.Logger.Warn().Err(err).Str("scheduled_id", sm.ID).Msg("Scheduler: failed to mark sent")
	}
	a.emitMessagesChange(sm.ConversationID)
	a.emitConversationsChange()
	a.Logger.Info().Str("scheduled_id", sm.ID).Str("conv_id", sm.ConversationID).Msg("Scheduler: sent scheduled message")
}

// retryOrFail reverts a claimed message to pending (to retry on a later tick),
// or marks it permanently failed once attempts are exhausted.
func (a *App) retryOrFail(sm *db.ScheduledMessage, reason string) {
	if sm.Attempts+1 >= scheduleMaxAttempts {
		_ = a.Store.MarkScheduledMessageFailed(sm.ID, reason)
		a.emitConversationsChange()
		return
	}
	_ = a.Store.RevertScheduledMessageToPending(sm.ID, reason)
}

// sendScheduledText sends a scheduled text message to an existing conversation,
// routing by platform (WhatsApp / Signal / SMS-RCS). Distinct from the
// CLI-facing SendTextToConversation in send.go: this one carries a reply-to ID
// and returns just the message, which is what the scheduler needs.
func (a *App) sendScheduledText(conversationID, body, replyToID string) (*db.Message, error) {
	switch a.conversationPlatform(conversationID) {
	case "whatsapp":
		return a.recordScheduledSend(a.SendWhatsAppText(conversationID, body, replyToID))
	case "signal":
		return a.recordScheduledSend(a.SendSignalText(conversationID, body, replyToID))
	default:
		return a.sendSMSText(conversationID, body, replyToID)
	}
}

// sendScheduledMedia loads the stored blob and dispatches it to the right
// platform (or the test override).
func (a *App) sendScheduledMedia(sm *db.ScheduledMessage) (*db.Message, error) {
	data, err := a.Store.GetScheduledMediaData(sm.ID)
	if err != nil {
		return nil, fmt.Errorf("load media: %w", err)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("scheduled media blob missing")
	}
	if a.sendMediaOverride != nil {
		return a.sendMediaOverride(sm.ConversationID, data, sm.MediaFilename, sm.MediaMime, sm.Body, sm.ReplyToID)
	}
	return a.SendMediaToConversation(sm.ConversationID, data, sm.MediaFilename, sm.MediaMime, sm.Body, sm.ReplyToID)
}

// SendMediaToConversation sends a media attachment (with optional caption) to an
// existing conversation, routing by platform (WhatsApp / Signal / SMS-RCS).
func (a *App) SendMediaToConversation(conversationID string, data []byte, filename, mime, caption, replyToID string) (*db.Message, error) {
	switch a.conversationPlatform(conversationID) {
	case "whatsapp":
		return a.recordScheduledSend(a.SendWhatsAppMedia(conversationID, data, filename, mime, caption, replyToID))
	case "signal":
		return a.recordScheduledSend(a.SendSignalMedia(conversationID, data, filename, mime, caption, replyToID))
	default:
		return a.sendSMSMedia(conversationID, data, filename, mime, caption, replyToID)
	}
}

// recordScheduledSend persists a WhatsApp/Signal message that the live bridges
// return but do not store themselves (the web send handlers record them
// separately). Without this, a scheduled WhatsApp/Signal send is delivered but
// never appears in the user's own thread.
func (a *App) recordScheduledSend(msg *db.Message, err error) (*db.Message, error) {
	if err != nil {
		return nil, err
	}
	if msg != nil {
		if rerr := a.Store.RecordOutgoingMessage(msg, ""); rerr != nil {
			a.Logger.Warn().Err(rerr).Msg("Scheduler: sent message but failed to record locally")
		}
	}
	return msg, nil
}

// sendSMSMedia uploads and sends a Google Messages (SMS/RCS) media attachment
// and records it locally. The SMS media payload carries no caption, so a
// non-empty caption is sent as a follow-up text (lossless).
func (a *App) sendSMSMedia(conversationID string, data []byte, filename, mime, caption, replyToID string) (*db.Message, error) {
	cli := a.GetClient()
	if cli == nil {
		return nil, errors.New(ErrNotConnected)
	}
	media, err := cli.GM.UploadMedia(data, filename, mime)
	if err != nil {
		return nil, fmt.Errorf("upload media: %w", err)
	}
	conv, err := cli.GM.GetConversation(conversationID)
	if err != nil {
		return nil, fmt.Errorf("get conversation: %w", err)
	}
	myParticipantID, simPayload := ExtractSIMAndParticipant(conv)
	payload := BuildSendMediaPayload(conversationID, media, myParticipantID, simPayload)
	resp, err := cli.GM.SendMessage(payload)
	if err != nil {
		return nil, fmt.Errorf("send message: %w", err)
	}
	if resp.GetStatus() != gmproto.SendMessageResponse_SUCCESS {
		return nil, fmt.Errorf("send failed: %s", resp.GetStatus().String())
	}
	msg := &db.Message{
		MessageID:      payload.TmpID,
		ConversationID: conversationID,
		Body:           "",
		IsFromMe:       true,
		TimestampMS:    time.Now().UnixMilli(),
		Status:         "OUTGOING_SENDING",
		ReplyToID:      replyToID,
		MediaID:        media.MediaID,
		MimeType:       media.MimeType,
		DecryptionKey:  hex.EncodeToString(media.DecryptionKey),
	}
	if err := a.Store.RecordOutgoingMessage(msg, ""); err != nil {
		a.Logger.Warn().Err(err).Msg("Scheduler: sent SMS media but failed to record locally")
	}
	// Caption can't ride on the SMS media payload — send it as a follow-up text.
	if c := strings.TrimSpace(caption); c != "" {
		if _, err := a.sendSMSText(conversationID, c, replyToID); err != nil {
			a.Logger.Warn().Err(err).Msg("Scheduler: media sent but caption text failed")
		}
	}
	return msg, nil
}

func (a *App) conversationPlatform(conversationID string) string {
	switch {
	case strings.HasPrefix(conversationID, "whatsapp:"):
		return "whatsapp"
	case strings.HasPrefix(conversationID, "signal:"), strings.HasPrefix(conversationID, "signal-group:"):
		return "signal"
	}
	if conv, err := a.Store.GetConversation(conversationID); err == nil && conv != nil {
		switch conv.SourcePlatform {
		case "whatsapp":
			return "whatsapp"
		case "signal":
			return "signal"
		}
	}
	return "sms"
}

func (a *App) routeReady(conversationID string) bool {
	switch a.conversationPlatform(conversationID) {
	case "whatsapp":
		return a.WhatsAppStatus().Connected
	case "signal":
		return a.SignalStatus().Connected
	default:
		return a.Connected.Load()
	}
}

// sendSMSText sends a Google Messages (SMS/RCS) text and records it locally.
func (a *App) sendSMSText(conversationID, body, replyToID string) (*db.Message, error) {
	cli := a.GetClient()
	if cli == nil {
		return nil, errors.New(ErrNotConnected)
	}
	conv, err := cli.GM.GetConversation(conversationID)
	if err != nil {
		return nil, fmt.Errorf("get conversation: %w", err)
	}
	myParticipantID, simPayload := ExtractSIMAndParticipant(conv)
	payload := BuildSendPayload(conversationID, body, replyToID, myParticipantID, simPayload)
	resp, err := cli.GM.SendMessage(payload)
	if err != nil {
		return nil, fmt.Errorf("send message: %w", err)
	}
	if resp.GetStatus() != gmproto.SendMessageResponse_SUCCESS {
		return nil, fmt.Errorf("send failed: %s", resp.GetStatus().String())
	}
	msg := &db.Message{
		MessageID:      payload.TmpID,
		ConversationID: conversationID,
		Body:           body,
		IsFromMe:       true,
		TimestampMS:    time.Now().UnixMilli(),
		Status:         "OUTGOING_SENT",
		ReplyToID:      replyToID,
	}
	if err := a.Store.RecordOutgoingMessage(msg, ""); err != nil {
		a.Logger.Warn().Err(err).Msg("Scheduler: sent SMS but failed to record locally")
	}
	return msg, nil
}
