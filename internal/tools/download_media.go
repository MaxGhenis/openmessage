package tools

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/maxghenis/openmessage/internal/app"
)

func downloadMediaTool() mcp.Tool {
	return mcp.NewTool("download_media",
		mcp.WithDescription("Return the read-only /api/media/<message-id> URL and available metadata for a media attachment. Supports Google Messages, WhatsApp, and Signal."),
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

		mimeType := strings.TrimSpace(msg.MimeType)
		if strings.TrimSpace(mimeType) == "" {
			mimeType = "application/octet-stream"
		}
		ext := extensionForMime(mimeType)
		safeID := strings.NewReplacer("/", "_", ":", "_", "\\", "_", " ", "_").Replace(msgID)
		filename := fmt.Sprintf("openmessage-%s%s", safeID, ext)
		mediaURL := "/api/media/" + url.PathEscape(msgID)

		return structuredResult(map[string]any{
			"message_id": msgID,
			"url":        mediaURL,
			"filename":   filename,
			"mime_type":  mimeType,
			// Legacy message rows do not retain attachment size. The serving
			// endpoint resolves it lazily (including projected v2blob: refs), so
			// report the value as unknown instead of fetching or inventing bytes.
			"size_bytes": nil,
		}, fmt.Sprintf("Media is available at %s (%s, %s; size unknown)", mediaURL, filename, mimeType)), nil
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
