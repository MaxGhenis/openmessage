package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"

	"github.com/maxghenis/openmessage/internal/app"
	"github.com/maxghenis/openmessage/internal/db"
)

var (
	sendWhatsAppText = func(a *app.App, conversationID, body, replyToID string) (*db.Message, error) {
		return a.SendWhatsAppText(conversationID, body, replyToID)
	}
	sendSignalText = func(a *app.App, conversationID, body, replyToID string) (*db.Message, error) {
		return a.SendSignalText(conversationID, body, replyToID)
	}
	getOrCreateGoogleConversation = func(a *app.App, phone string) (*gmproto.Conversation, error) {
		cli := a.GetClient()
		if cli == nil {
			return nil, fmt.Errorf(app.ErrNotConnected)
		}
		convResp, err := cli.GM.GetOrCreateConversation(&gmproto.GetOrCreateConversationRequest{
			Numbers: app.NewContactNumbers([]string{phone}),
		})
		if err != nil {
			return nil, err
		}
		return convResp.GetConversation(), nil
	}
	sendGoogleTextPayload = func(a *app.App, payload *gmproto.SendMessageRequest) (*gmproto.SendMessageResponse, error) {
		cli := a.GetClient()
		if cli == nil {
			return nil, fmt.Errorf(app.ErrNotConnected)
		}
		return cli.GM.SendMessage(payload)
	}
)

func sendMessageTool() mcp.Tool {
	return mcp.NewTool("send_message",
		mcp.WithDescription("Send a direct text message across supported platforms. Defaults to SMS/RCS when no platform is specified."),
		mcp.WithString("phone_number", mcp.Description("Legacy alias for recipient. For SMS/RCS use a phone number with country code (e.g., +15551234567).")),
		mcp.WithString("recipient", mcp.Description("Recipient identifier. Use a phone number for SMS/RCS or Signal, and a phone number or WhatsApp JID for WhatsApp.")),
		mcp.WithString("platform", mcp.Description("Target platform: sms, rcs, whatsapp, or signal. Defaults to sms.")),
		mcp.WithString("message", mcp.Required(), mcp.Description("Message text to send")),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(false),
	)
}

func sendMessageHandler(a *app.App) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		recipient := firstNonEmpty(strings.TrimSpace(strArg(args, "recipient")), strings.TrimSpace(strArg(args, "phone_number")))
		platform := normalizeDirectSendPlatform(strArg(args, "platform"))
		message := strArg(args, "message")

		if recipient == "" {
			return errorResult("recipient or phone_number is required"), nil
		}
		if message == "" {
			return errorResult("message is required"), nil
		}

		switch platform {
		case "whatsapp":
			conversationID, name, number, isGroup, err := canonicalWhatsAppDirectConversation(recipient)
			if err != nil {
				return errorResult(err.Error()), nil
			}
			msg, err := sendWhatsAppText(a, conversationID, message, "")
			if err != nil {
				return errorResult(fmt.Sprintf("failed to send: %v", err)), nil
			}
			if err := ensureDirectConversationExists(a, conversationID, "whatsapp", name, number, isGroup, msg.TimestampMS); err != nil {
				return errorResult(fmt.Sprintf("failed to persist conversation: %v", err)), nil
			}
			if err := a.Store.RecordOutgoingMessage(msg, ""); err != nil {
				return errorResult(fmt.Sprintf("failed to persist sent message: %v", err)), nil
			}
			return structuredResult(map[string]any{
				"ok": true,
				"conversation": conversationSummary{
					ConversationID: conversationID,
					Name:           name,
					SourcePlatform: "whatsapp",
					IsGroup:        isGroup,
				},
				"message": summarizeMessage(msg),
			}, fmt.Sprintf("Message sent to %s: %s", name, message)), nil
		case "signal":
			conversationID, name, number, isGroup, err := canonicalSignalDirectConversation(recipient)
			if err != nil {
				return errorResult(err.Error()), nil
			}
			msg, err := sendSignalText(a, conversationID, message, "")
			if err != nil {
				return errorResult(fmt.Sprintf("failed to send: %v", err)), nil
			}
			if err := ensureDirectConversationExists(a, conversationID, "signal", name, number, isGroup, msg.TimestampMS); err != nil {
				return errorResult(fmt.Sprintf("failed to persist conversation: %v", err)), nil
			}
			if err := a.Store.RecordOutgoingMessage(msg, ""); err != nil {
				return errorResult(fmt.Sprintf("failed to persist sent message: %v", err)), nil
			}
			return structuredResult(map[string]any{
				"ok": true,
				"conversation": conversationSummary{
					ConversationID: conversationID,
					Name:           name,
					SourcePlatform: "signal",
					IsGroup:        isGroup,
				},
				"message": summarizeMessage(msg),
			}, fmt.Sprintf("Message sent to %s: %s", name, message)), nil
		case "sms":
			// Validate the recipient is a phone number before resolving it.
			// Google's GetOrCreateConversation will otherwise normalize an
			// arbitrary/name-like string against the address book and can
			// silently resolve to an unintended conversation — then send.
			if !looksLikePhoneNumber(recipient) {
				return errorResult(fmt.Sprintf("SMS recipient must be a phone number with country code (e.g. +15551234567), got %q", recipient)), nil
			}
			conv, err := getOrCreateGoogleConversation(a, recipient)
			if err != nil {
				return errorResult(fmt.Sprintf("failed to get/create conversation: %v", err)), nil
			}
			if conv == nil {
				return errorResult("no conversation returned"), nil
			}
			if err := upsertGoogleConversation(a, conv); err != nil {
				return errorResult(fmt.Sprintf("failed to persist conversation: %v", err)), nil
			}
			myParticipantID, simPayload := app.ExtractSIMAndParticipant(conv)
			payload := app.BuildSendPayload(conv.GetConversationID(), message, "", myParticipantID, simPayload)
			resp, err := sendGoogleTextPayload(a, payload)
			if err != nil {
				return errorResult(fmt.Sprintf("failed to send: %v", err)), nil
			}
			if resp.GetStatus() != gmproto.SendMessageResponse_SUCCESS {
				return errorResult(fmt.Sprintf("failed to send: %s", resp.GetStatus().String())), nil
			}
			now := time.Now().UnixMilli()
			if err := a.Store.RecordOutgoingMessage(&db.Message{
				MessageID:      payload.TmpID,
				ConversationID: conv.GetConversationID(),
				Body:           message,
				IsFromMe:       true,
				TimestampMS:    now,
				Status:         "OUTGOING_SENDING",
				SourcePlatform: "sms",
			}, ""); err != nil {
				return errorResult(fmt.Sprintf("failed to persist sent message: %v", err)), nil
			}
			storedMsg, err := a.Store.GetMessageByID(payload.TmpID)
			if err != nil {
				return errorResult(fmt.Sprintf("failed to load sent message: %v", err)), nil
			}
			persistedConv, err := a.Store.GetConversation(conv.GetConversationID())
			if err != nil {
				return errorResult(fmt.Sprintf("failed to load conversation: %v", err)), nil
			}
			return structuredResult(map[string]any{
				"ok":           true,
				"conversation": summarizeConversation(persistedConv),
				"message":      summarizeMessage(storedMsg),
			}, fmt.Sprintf("Message sent to %s: %s", firstNonEmpty(conv.GetName(), recipient), message)), nil
		default:
			return errorResult(fmt.Sprintf("unsupported platform %q (supported: sms, whatsapp, signal)", platform)), nil
		}
	}
}

func normalizeDirectSendPlatform(platform string) string {
	switch strings.ToLower(strings.TrimSpace(platform)) {
	case "", "sms", "rcs", "google", "google_messages", "gmessages":
		return "sms"
	case "whatsapp", "signal":
		return strings.ToLower(strings.TrimSpace(platform))
	default:
		return strings.ToLower(strings.TrimSpace(platform))
	}
}

func canonicalWhatsAppDirectConversation(recipient string) (conversationID, name, number string, isGroup bool, err error) {
	raw := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(recipient), "whatsapp:"))
	if raw == "" {
		return "", "", "", false, fmt.Errorf("whatsapp recipient is required")
	}
	if strings.Contains(raw, "@") {
		isGroup = strings.HasSuffix(raw, "@g.us")
		if !isGroup && strings.HasSuffix(raw, "@s.whatsapp.net") {
			number = "+" + strings.TrimSuffix(raw, "@s.whatsapp.net")
			name = number
		} else {
			name = raw
		}
		return "whatsapp:" + raw, name, number, isGroup, nil
	}
	digits := digitsOnly(raw)
	if digits == "" {
		return "", "", "", false, fmt.Errorf("invalid WhatsApp recipient: %s", recipient)
	}
	number = "+" + digits
	return "whatsapp:" + digits + "@s.whatsapp.net", number, number, false, nil
}

func canonicalSignalDirectConversation(recipient string) (conversationID, name, number string, isGroup bool, err error) {
	raw := strings.TrimSpace(recipient)
	if raw == "" {
		return "", "", "", false, fmt.Errorf("signal recipient is required")
	}
	switch {
	case strings.HasPrefix(raw, "signal-group:"):
		groupID := strings.TrimSpace(strings.TrimPrefix(raw, "signal-group:"))
		if groupID == "" {
			return "", "", "", false, fmt.Errorf("signal group id is required")
		}
		return "signal-group:" + groupID, groupID, "", true, nil
	case strings.HasPrefix(raw, "group:"):
		groupID := strings.TrimSpace(strings.TrimPrefix(raw, "group:"))
		if groupID == "" {
			return "", "", "", false, fmt.Errorf("signal group id is required")
		}
		return "signal-group:" + groupID, groupID, "", true, nil
	case strings.HasPrefix(raw, "signal:"):
		address := strings.TrimSpace(strings.TrimPrefix(raw, "signal:"))
		if address == "" {
			return "", "", "", false, fmt.Errorf("signal recipient is required")
		}
		return "signal:" + address, address, address, false, nil
	default:
		return "signal:" + raw, raw, raw, false, nil
	}
}

func ensureDirectConversationExists(a *app.App, conversationID, platform, name, number string, isGroup bool, ts int64) error {
	if existing, err := a.Store.GetConversation(conversationID); err == nil && existing != nil {
		return nil
	}
	participantsJSON := "[]"
	if !isGroup && strings.TrimSpace(number) != "" {
		if b, err := json.Marshal([]map[string]string{{
			"name":   firstNonEmpty(strings.TrimSpace(name), strings.TrimSpace(number)),
			"number": strings.TrimSpace(number),
		}}); err == nil {
			participantsJSON = string(b)
		}
	}
	return a.Store.UpsertConversation(&db.Conversation{
		ConversationID: conversationID,
		Name:           firstNonEmpty(strings.TrimSpace(name), conversationID),
		IsGroup:        isGroup,
		Participants:   participantsJSON,
		LastMessageTS:  ts,
		SourcePlatform: platform,
	})
}

func upsertGoogleConversation(a *app.App, conv *gmproto.Conversation) error {
	if conv == nil {
		return fmt.Errorf("conversation is nil")
	}
	type participantInfo struct {
		Name   string `json:"name"`
		Number string `json:"number"`
		IsMe   bool   `json:"is_me,omitempty"`
	}
	participantsJSON := "[]"
	if ps := conv.GetParticipants(); len(ps) > 0 {
		infos := make([]participantInfo, 0, len(ps))
		for _, p := range ps {
			info := participantInfo{
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
	lastTS := conv.GetLastMessageTimestamp() / 1000
	if lastTS == 0 {
		lastTS = time.Now().UnixMilli()
	}
	return a.Store.UpsertConversation(&db.Conversation{
		ConversationID: conv.GetConversationID(),
		Name:           firstNonEmpty(conv.GetName(), conv.GetConversationID()),
		IsGroup:        conv.GetIsGroupChat(),
		Participants:   participantsJSON,
		LastMessageTS:  lastTS,
		SourcePlatform: "sms",
	})
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func digitsOnly(value string) string {
	var b strings.Builder
	for _, r := range value {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// looksLikePhoneNumber reports whether s is plausibly an E.164-style phone
// number: 7–15 digits (after stripping +, spaces, dashes, parens) and no
// letters. Rejects names like "Mom" or "John Smith" that would otherwise be
// resolved against the address book and could send to the wrong contact.
func looksLikePhoneNumber(s string) bool {
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			return false
		}
	}
	digits := digitsOnly(s)
	return len(digits) >= 7 && len(digits) <= 15
}
