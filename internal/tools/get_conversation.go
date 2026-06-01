package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/maxghenis/openmessage/internal/app"
)

func getConversationTool() mcp.Tool {
	return mcp.NewTool("get_conversation",
		mcp.WithDescription("Get messages in a specific conversation by ID"),
		mcp.WithString("conversation_id", mcp.Required(), mcp.Description("The conversation ID")),
		mcp.WithNumber("limit", mcp.Description("Maximum messages to return (default 50)")),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
	)
}

func getConversationHandler(a *app.App) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		convID := strArg(args, "conversation_id")
		if convID == "" {
			return errorResult("conversation_id is required"), nil
		}
		limit := intArg(args, "limit", 50)

		msgs, err := a.Store.GetMessagesByConversation(convID, limit)
		if err != nil {
			return errorResult(fmt.Sprintf("query failed: %v", err)), nil
		}

		conv, convErr := a.Store.GetConversation(convID)

		if len(msgs) == 0 {
			return structuredResult(map[string]any{
				"conversation": summarizeConversation(conv),
				"count":        0,
				"messages":     []messageSummary{},
			}, "No messages found in this conversation."), nil
		}

		var sb strings.Builder
		// Show conversation info
		if convErr == nil && conv != nil {
			platform := conv.SourcePlatform
			if platform == "" {
				platform = "sms"
			}
			fmt.Fprintf(&sb, "Conversation: %s (ID: %s, platform: %s)\n", conv.Name, conv.ConversationID, platform)
			if conv.IsGroup {
				sb.WriteString("Type: Group\n")
			}
			sb.WriteString("---\n")
		}

		sb.WriteString(messagePreamble)
		summaries := make([]messageSummary, 0, len(msgs))
		for _, m := range msgs {
			summaries = append(summaries, summarizeMessage(m))
			sb.WriteString(formatMessageLine(m))
			sb.WriteByte('\n')
		}
		return structuredResult(map[string]any{
			"conversation":            summarizeConversation(conv),
			"count":                   len(summaries),
			"messages":                summaries,
			"contains_untrusted_text": true,
		}, sb.String()), nil
	}
}
