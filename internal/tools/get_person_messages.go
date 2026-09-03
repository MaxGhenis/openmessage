package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/maxghenis/openmessage/internal/app"
)

func getPersonMessagesTool() mcp.Tool {
	return mcp.NewTool("get_person_messages",
		mcp.WithDescription("Get all messages with a person across all platforms (SMS, Google Chat, iMessage, WhatsApp). Searches by name or identifier."),
		mcp.WithString("name", mcp.Required(), mcp.Description("Person's name to search for (case-insensitive partial match)")),
		mcp.WithNumber("limit", mcp.Description("Maximum messages to return (default 50)")),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
	)
}

func getPersonMessagesHandler(a *app.App, configured ...Options) server.ToolHandlerFunc {
	options := resolvedOptions(a, configured)
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		name := strArg(args, "name")
		if name == "" {
			return errorResult("name is required"), nil
		}
		limit := intArg(args, "limit", 50)

		// Find all conversations that mention this person
		allConvs, err := options.Reads.ListConversations(1000)
		if err != nil {
			return errorResult(fmt.Sprintf("list conversations: %v", err)), nil
		}

		nameLower := strings.ToLower(name)
		var matchingConvIDs []string
		for _, c := range allConvs {
			if strings.Contains(strings.ToLower(c.Name), nameLower) ||
				strings.Contains(strings.ToLower(c.Participants), nameLower) {
				matchingConvIDs = append(matchingConvIDs, c.ConversationID)
			}
		}

		if len(matchingConvIDs) == 0 {
			return textResult(fmt.Sprintf("No conversations found with '%s'.", name)), nil
		}

		// Build a map of conversation metadata for display
		convMap := make(map[string]*struct{ name, platform string })
		for _, c := range allConvs {
			for _, id := range matchingConvIDs {
				if c.ConversationID == id {
					platform := c.SourcePlatform
					if platform == "" {
						platform = "sms"
					}
					convMap[id] = &struct{ name, platform string }{c.Name, platform}
				}
			}
		}

		// Batch fetch all messages in one query
		msgs, err := options.Reads.GetMessagesByConversations(matchingConvIDs, limit)
		if err != nil {
			return errorResult(fmt.Sprintf("get messages: %v", err)), nil
		}

		// Group messages by conversation for display
		var sb strings.Builder
		sb.WriteString(messagePreamble)
		fmt.Fprintf(&sb, "Messages with '%s' across %d conversation(s):\n\n", name, len(matchingConvIDs))

		currentConv := ""
		totalMsgs := 0
		for _, m := range msgs {
			if m.ConversationID != currentConv {
				if currentConv != "" {
					sb.WriteString("\n")
				}
				currentConv = m.ConversationID
				if info, ok := convMap[currentConv]; ok {
					fmt.Fprintf(&sb, "--- %s [%s] (ID: %s) ---\n", info.name, info.platform, currentConv)
				}
			}

			sb.WriteString(formatMessageLine(m))
			sb.WriteByte('\n')
			totalMsgs++
		}

		fmt.Fprintf(&sb, "\nTotal: %d messages across %d conversation(s)\n", totalMsgs, len(matchingConvIDs))
		return textResult(sb.String()), nil
	}
}
