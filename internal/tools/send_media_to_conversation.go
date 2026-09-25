package tools

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
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
	"github.com/maxghenis/openmessage/internal/messaging"
	"github.com/maxghenis/openmessage/internal/v2wire"
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

func sendMediaToConversationTool(v2Enabled ...bool) mcp.Tool {
	description := "Send a media attachment to an existing conversation by conversation ID across supported platforms"
	options := []mcp.ToolOption{
		mcp.WithDescription(description),
		mcp.WithString("conversation_id", mcp.Required(), mcp.Description("Existing conversation ID from list_conversations or get_conversation")),
		mcp.WithString("file_path", mcp.Required(), mcp.Description("Absolute or relative path to the local file to send")),
		mcp.WithString("caption", mcp.Description("Optional caption for platforms that support media captions")),
		mcp.WithString("mime_type", mcp.Description("Optional MIME type override, for example image/png")),
		mcp.WithString("reply_to_id", mcp.Description("Optional message ID to reply to when the platform supports media replies")),
	}
	if v2Requested(v2Enabled) {
		options[0] = mcp.WithDescription(description + v2DeliveryDescription)
		options = append(options, mcp.WithString("idempotency_key", mcp.Description(v2IdempotencyDescription)))
		options = withSendControlOptions(options, false)
	}
	options = append(options,
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(false),
	)
	return mcp.NewTool("send_media_to_conversation", options...)
}

// maxMediaUploadBytes bounds a single MCP media send. It mirrors the HTTP
// upload cap so an MCP client cannot read an arbitrarily large local file fully
// into memory and take the backend down.
const maxMediaUploadBytes = 128 << 20

func sendMediaToConversationHandler(a *app.App, v2Options ...*V2Dependencies) server.ToolHandlerFunc {
	v2 := activeV2(v2Options)
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

		if info, err := os.Stat(filePath); err != nil {
			return errorResult(fmt.Sprintf("stat file: %v", err)), nil
		} else if info.IsDir() {
			return errorResult("file_path must point to a file"), nil
		} else if info.Size() > maxMediaUploadBytes {
			return errorResult(fmt.Sprintf("file too large (%d bytes; limit %d MB)", info.Size(), maxMediaUploadBytes>>20)), nil
		}
		if v2 != nil {
			return submitV2MediaFile(ctx, a, v2, args, filePath, mimeType, caption, replyToID), nil
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

func submitV2MediaFile(
	ctx context.Context,
	a *app.App,
	v2 *V2Dependencies,
	args map[string]any,
	filePath string,
	mimeType string,
	caption string,
	replyToID string,
) *mcp.CallToolResult {
	file, err := os.Open(filePath)
	if err != nil {
		return errorResult(fmt.Sprintf("read file: %v", err))
	}
	defer file.Close()

	filename := filepath.Base(filePath)
	if filename == "." || filename == string(filepath.Separator) || filename == "" {
		return errorResult("file_path must point to a file")
	}
	sniff := make([]byte, 512)
	n, err := file.Read(sniff)
	if err != nil && !errors.Is(err, io.EOF) {
		return errorResult(fmt.Sprintf("read file: %v", err))
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return errorResult(fmt.Sprintf("read file: %v", err))
	}
	mimeType = detectMediaMimeType(filename, sniff[:n], mimeType)

	key, err := v2IdempotencyKey(args)
	if err != nil {
		return errorResult(err.Error())
	}
	ttl, err := parseSendTTL(args)
	if err != nil {
		return errorResult(err.Error())
	}
	wait, err := parseSendWaitOptions(args)
	if err != nil {
		return errorResult(err.Error())
	}
	conversationID := strArg(args, "conversation_id")
	platform := v2.sendPlatform(a, conversationID)
	if failure := checkPlatformSendable(localSendCapability(a, v2), platform); failure != nil {
		return failure
	}
	submission, err := v2.submitMedia(ctx, a, v2wire.MediaInput{
		ConversationID: conversationID,
		Content:        file,
		Filename:       filename,
		MIME:           mimeType,
		Caption:        caption,
		ReplyToID:      replyToID,
		IdempotencyKey: key,
		TTL:            ttl,
	})
	if err != nil {
		if errors.Is(err, messaging.ErrTooLarge) {
			return errorResult(fmt.Sprintf("file too large (limit %d MB)", maxMediaUploadBytes>>20))
		}
		if errors.Is(err, v2wire.ErrPlatformNotSendable) {
			return platformUnavailableResult(firstNonEmpty(platform, "the requested platform"), err.Error())
		}
		return errorResult(fmt.Sprintf("failed to submit media: %v", err))
	}
	return waitForV2Delivery(ctx, a, v2, submission, key, platform, conversationID, wait)
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
