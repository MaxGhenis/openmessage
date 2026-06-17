package tools

import (
	"context"
	"encoding/hex"
	"fmt"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"

	"github.com/maxghenis/openmessage/internal/app"
	"github.com/maxghenis/openmessage/internal/db"
)

var (
	sendWhatsAppMediaMessage = func(a *app.App, conversationID string, data []byte, filename, mimeType, caption, replyToID string) (*db.Message, error) {
		return a.SendWhatsAppMedia(conversationID, data, filename, mimeType, caption, replyToID)
	}
	sendSignalMediaMessage = func(a *app.App, conversationID string, data []byte, filename, mimeType, caption, replyToID string) (*db.Message, error) {
		return a.SendSignalMedia(conversationID, data, filename, mimeType, caption, replyToID)
	}
	uploadGoogleMedia = func(a *app.App, data []byte, filename, mimeType string) (*gmproto.MediaContent, error) {
		cli := a.GetClient()
		if cli == nil {
			return nil, fmt.Errorf(app.ErrNotConnected)
		}
		return cli.GM.UploadMedia(data, filename, mimeType)
	}
	getGoogleConversation = func(a *app.App, conversationID string) (*gmproto.Conversation, error) {
		cli := a.GetClient()
		if cli == nil {
			return nil, fmt.Errorf(app.ErrNotConnected)
		}
		return cli.GM.GetConversation(conversationID)
	}
	sendGoogleMediaMessage = func(a *app.App, payload *gmproto.SendMessageRequest) (*gmproto.SendMessageResponse, error) {
		cli := a.GetClient()
		if cli == nil {
			return nil, fmt.Errorf(app.ErrNotConnected)
		}
		return cli.GM.SendMessage(payload)
	}
)

func sendMediaToConversationTool() mcp.Tool {
	return mcp.NewTool("send_media_to_conversation",
		mcp.WithDescription("Send a media attachment to an existing conversation by conversation ID across supported platforms"),
		mcp.WithString("conversation_id", mcp.Required(), mcp.Description("Existing conversation ID from list_conversations or get_conversation")),
		mcp.WithString("file_path", mcp.Required(), mcp.Description("Absolute or relative path to the local file to send")),
		mcp.WithString("caption", mcp.Description("Optional caption for platforms that support media captions")),
		mcp.WithString("mime_type", mcp.Description("Optional MIME type override, for example image/png")),
		mcp.WithString("reply_to_id", mcp.Description("Optional message ID to reply to when the platform supports media replies")),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(false),
	)
}

func sendMediaToConversationHandler(a *app.App) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		conversationID := strArg(args, "conversation_id")
		filePath := strArg(args, "file_path")
		caption := strArg(args, "caption")
		mimeType := strArg(args, "mime_type")
		replyToID := strArg(args, "reply_to_id")

		if conversationID == "" {
			return errorResult("conversation_id is required"), nil
		}
		if filePath == "" {
			return errorResult("file_path is required"), nil
		}

		conv, err := a.Store.GetConversation(conversationID)
		if err != nil {
			return errorResult(fmt.Sprintf("failed to load conversation: %v", err)), nil
		}
		if conv == nil {
			return errorResult(fmt.Sprintf("conversation %s not found", conversationID)), nil
		}

		data, err := os.ReadFile(filePath)
		if err != nil {
			return errorResult(fmt.Sprintf("read file: %v", err)), nil
		}
		filename := filepath.Base(filePath)
		if filename == "." || filename == string(filepath.Separator) || filename == "" {
			return errorResult("file_path must point to a file"), nil
		}
		mimeType = detectMediaMimeType(filename, data, mimeType)

		switch conv.SourcePlatform {
		case "whatsapp":
			msg, err := sendWhatsAppMediaMessage(a, conversationID, data, filename, mimeType, caption, replyToID)
			if err != nil {
				return errorResult(fmt.Sprintf("failed to send media: %v", err)), nil
			}
			if err := a.Store.RecordOutgoingMessage(msg, ""); err != nil {
				return errorResult(fmt.Sprintf("failed to persist sent message: %v", err)), nil
			}
			return textResult(fmt.Sprintf("Media sent to %s (%s): %s", conversationName(conv), conversationID, filename)), nil
		case "signal":
			msg, err := sendSignalMediaMessage(a, conversationID, data, filename, mimeType, caption, replyToID)
			if err != nil {
				return errorResult(fmt.Sprintf("failed to send media: %v", err)), nil
			}
			if err := a.Store.RecordOutgoingMessage(msg, ""); err != nil {
				return errorResult(fmt.Sprintf("failed to persist sent message: %v", err)), nil
			}
			return textResult(fmt.Sprintf("Media sent to %s (%s): %s", conversationName(conv), conversationID, filename)), nil
		case "", "sms":
			media, err := uploadGoogleMedia(a, data, filename, mimeType)
			if err != nil {
				if !a.HandleGoogleAuthExpiredError(err) {
					a.RecordGoogleSendError(err)
				}
				return errorResult(fmt.Sprintf("upload media: %v", err)), nil
			}
			gmConv, err := getGoogleConversation(a, conversationID)
			if err != nil {
				if !a.HandleGoogleAuthExpiredError(err) {
					a.RecordGoogleSendError(err)
				}
				return errorResult(fmt.Sprintf("get conversation: %v", err)), nil
			}
			myParticipantID, simPayload := app.ExtractSIMAndParticipant(gmConv)
			payload := app.BuildSendMediaPayload(conversationID, media, myParticipantID, simPayload)
			resp, err := sendGoogleMediaMessage(a, payload)
			if err != nil {
				if !a.HandleGoogleAuthExpiredError(err) {
					a.RecordGoogleSendError(err)
				}
				return errorResult(fmt.Sprintf("failed to send media: %v", err)), nil
			}
			if resp.GetStatus() != gmproto.SendMessageResponse_SUCCESS {
				a.RecordGoogleSendOutcomeWithPhone(false, a.GooglePhoneResponding())
				return errorResult(app.GoogleSendRejectedMessage(resp.GetStatus().String(), a.GooglePhoneResponding())), nil
			}
			a.RecordGoogleSendOutcome(true)
			now := time.Now().UnixMilli()
			msg := &db.Message{
				MessageID:      payload.TmpID,
				ConversationID: conversationID,
				Body:           "",
				IsFromMe:       true,
				TimestampMS:    now,
				Status:         "OUTGOING_SENDING",
				MediaID:        media.MediaID,
				MimeType:       media.MimeType,
				DecryptionKey:  hex.EncodeToString(media.DecryptionKey),
			}
			if err := a.Store.RecordOutgoingMessage(msg, ""); err != nil {
				return errorResult(fmt.Sprintf("failed to persist sent message: %v", err)), nil
			}
			return textResult(fmt.Sprintf("Media sent to %s (%s): %s", conversationName(conv), conversationID, filename)), nil
		default:
			return errorResult(fmt.Sprintf("media sending is not supported for platform %s via OpenMessage MCP yet", conv.SourcePlatform)), nil
		}
	}
}

func detectMediaMimeType(filename string, data []byte, explicit string) string {
	if typed := strings.TrimSpace(explicit); typed != "" {
		return typed
	}
	if ext := strings.TrimSpace(filepath.Ext(filename)); ext != "" {
		if typed := mime.TypeByExtension(ext); typed != "" {
			if idx := strings.Index(typed, ";"); idx >= 0 {
				typed = typed[:idx]
			}
			if typed != "" {
				return typed
			}
		}
	}
	sniffLen := len(data)
	if sniffLen > 512 {
		sniffLen = 512
	}
	if sniffLen > 0 {
		return http.DetectContentType(data[:sniffLen])
	}
	return "application/octet-stream"
}

func conversationName(conv *db.Conversation) string {
	if conv == nil || strings.TrimSpace(conv.Name) == "" {
		if conv == nil {
			return ""
		}
		return conv.ConversationID
	}
	return conv.Name
}
