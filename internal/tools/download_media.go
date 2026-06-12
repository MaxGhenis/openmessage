package tools

import (
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/maxghenis/openmessage/internal/app"
	"github.com/maxghenis/openmessage/internal/db"
)

var (
	downloadWhatsAppMedia = func(a *app.App, msg *db.Message) ([]byte, string, error) {
		return a.DownloadWhatsAppMedia(msg)
	}
	downloadSignalMedia = func(a *app.App, msg *db.Message) ([]byte, string, error) {
		return a.DownloadSignalMedia(msg)
	}
)

func downloadMediaTool() mcp.Tool {
	return mcp.NewTool("download_media",
		mcp.WithDescription("Download media (voice messages, images, videos) from a message and save to a local file. Supports Google Messages, WhatsApp, and Signal."),
		mcp.WithString("message_id", mcp.Required(), mcp.Description("The message ID containing the media")),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
	)
}

func downloadMediaHandler(a *app.App) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		msgID := strArg(args, "message_id")
		if msgID == "" {
			return errorResult("message_id is required"), nil
		}

		msg, err := a.Store.GetMessageByID(msgID)
		if err != nil {
			return errorResult(fmt.Sprintf("get message: %v", err)), nil
		}
		if msg == nil {
			return errorResult("message not found"), nil
		}
		if msg.MediaID == "" {
			return errorResult("this message has no media attachment"), nil
		}

		mimeType := msg.MimeType
		var data []byte
		switch {
		case msg.SourcePlatform == "whatsapp" || strings.HasPrefix(msg.MessageID, "whatsapp:") || strings.HasPrefix(msg.MediaID, "wa:"):
			data, mimeType, err = downloadWhatsAppMedia(a, msg)
			if err != nil {
				return errorResult(fmt.Sprintf("download media: %v", err)), nil
			}
		case msg.SourcePlatform == "signal" || strings.HasPrefix(msg.MessageID, "signal:") || strings.HasPrefix(msg.MediaID, "signalatt:"):
			data, mimeType, err = downloadSignalMedia(a, msg)
			if err != nil {
				return errorResult(fmt.Sprintf("download media: %v", err)), nil
			}
		default:
			cli := a.GetClient()
			if cli == nil {
				return errorResult(app.ErrNotConnected), nil
			}

			key, err := hex.DecodeString(msg.DecryptionKey)
			if err != nil {
				return errorResult(fmt.Sprintf("invalid decryption key: %v", err)), nil
			}

			data, err = cli.GM.DownloadMedia(msg.MediaID, key)
			if err != nil {
				return errorResult(fmt.Sprintf("download media: %v", err)), nil
			}
		}
		if strings.TrimSpace(mimeType) == "" {
			mimeType = msg.MimeType
		}

		// Determine file extension from mime type
		ext := extensionForMime(mimeType)

		// Save to a temp file. Sanitize the message id for use in a filename —
		// iMessage GUIDs and conversation-scoped ids contain "/" and ":", which
		// would turn into bogus nested paths and fail the write.
		tmpDir := os.TempDir()
		safeID := strings.NewReplacer("/", "_", ":", "_", "\\", "_", " ", "_").Replace(msgID)
		filename := fmt.Sprintf("openmessage-%s%s", safeID, ext)
		filePath := filepath.Join(tmpDir, filename)

		if err := os.WriteFile(filePath, data, 0644); err != nil {
			return errorResult(fmt.Sprintf("write file: %v", err)), nil
		}

		return textResult(fmt.Sprintf("Downloaded %s (%d bytes) to:\n%s", mimeType, len(data), filePath)), nil
	}
}

func extensionForMime(mime string) string {
	switch {
	case strings.HasPrefix(mime, "audio/ogg"):
		return ".ogg"
	case strings.HasPrefix(mime, "audio/aac"):
		return ".aac"
	case strings.HasPrefix(mime, "audio/mp4"), strings.HasPrefix(mime, "audio/m4a"):
		return ".m4a"
	case strings.HasPrefix(mime, "audio/mpeg"):
		return ".mp3"
	case strings.HasPrefix(mime, "audio/amr"):
		return ".amr"
	case strings.HasPrefix(mime, "audio/"):
		return ".audio"
	case strings.HasPrefix(mime, "image/jpeg"):
		return ".jpg"
	case strings.HasPrefix(mime, "image/png"):
		return ".png"
	case strings.HasPrefix(mime, "image/gif"):
		return ".gif"
	case strings.HasPrefix(mime, "image/webp"):
		return ".webp"
	case strings.HasPrefix(mime, "image/"):
		return ".img"
	case strings.HasPrefix(mime, "video/mp4"):
		return ".mp4"
	case strings.HasPrefix(mime, "video/3gpp"):
		return ".3gp"
	case strings.HasPrefix(mime, "video/"):
		return ".video"
	default:
		return ".bin"
	}
}
