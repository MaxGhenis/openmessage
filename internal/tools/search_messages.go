package tools

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/maxghenis/openmessage/internal/app"
)

func searchMessagesTool() mcp.Tool {
	return mcp.NewTool("search_messages",
		mcp.WithDescription("Search messages by text content across all conversations and synced platforms (SMS/RCS, Google Chat, iMessage, WhatsApp, Signal)"),
		mcp.WithString("query", mcp.Required(), mcp.Description("Search text")),
		mcp.WithString("phone_number", mcp.Description("Filter by phone number")),
		mcp.WithNumber("limit", mcp.Description("Maximum results (default 20)")),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
	)
}

func searchMessagesHandler(a *app.App) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		query := strArg(args, "query")
		if query == "" {
			return errorResult("query is required"), nil
		}
		phone := strArg(args, "phone_number")
		limit := intArg(args, "limit", 20)

		msgs, err := a.Store.SearchMessages(query, phone, limit)
		if err != nil {
			return errorResult(fmt.Sprintf("search failed: %v", err)), nil
		}

		if len(msgs) == 0 {
			return structuredResult(map[string]any{
				"query":                   query,
				"phone_number":            phone,
				"count":                   0,
				"messages":                []messageSummary{},
				"contains_untrusted_text": true,
			}, fmt.Sprintf("No messages found matching '%s'.", query)), nil
		}

		var sb strings.Builder
		sb.WriteString(messagePreamble)
		fmt.Fprintf(&sb, "Found %d messages matching '%s':\n\n", len(msgs), query)
		summaries := make([]messageSummary, 0, len(msgs))
		for _, m := range msgs {
			summaries = append(summaries, summarizeMessage(m))
			ts := time.UnixMilli(m.TimestampMS).Format(time.RFC3339)
			direction := "←"
			if m.IsFromMe {
				direction = "→"
			}
			sender := resolveSender(m)
			platform := ""
			if m.SourcePlatform != "" && m.SourcePlatform != "sms" {
				platform = fmt.Sprintf(" [%s]", m.SourcePlatform)
			}
			display := formatMessageBody(m.Body, m.MediaID, m.MimeType, m.MessageID)
			fmt.Fprintf(&sb, "[%s] %s %s%s (conv: %s): «%s»\n", ts, direction, sender, platform, m.ConversationID, display)
		}
		return structuredResult(map[string]any{
			"query":                   query,
			"phone_number":            phone,
			"count":                   len(summaries),
			"messages":                summaries,
			"contains_untrusted_text": true,
		}, sb.String()), nil
	}
}
