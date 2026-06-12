package tools

import (
	"context"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/maxghenis/openmessage/internal/app"
)

var (
	sendTextToConversation = func(a *app.App, conversationID, body string) (conversationSummary, messageSummary, error) {
		conv, msg, err := a.SendTextToConversation(conversationID, body)
		if err != nil {
			return conversationSummary{}, messageSummary{}, err
		}
		return summarizeConversation(conv), summarizeMessage(msg), nil
	}
)

func sendToConversationTool() mcp.Tool {
	return mcp.NewTool("send_to_conversation",
		mcp.WithDescription("Send a text message to an existing conversation by conversation ID across supported platforms"),
		mcp.WithString("conversation_id", mcp.Required(), mcp.Description("Existing conversation ID from list_conversations or get_conversation")),
		mcp.WithString("message", mcp.Required(), mcp.Description("Message text to send")),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(false),
	)
}

func sendToConversationHandler(a *app.App) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		conversationID := strArg(args, "conversation_id")
		message := strArg(args, "message")

		if conversationID == "" {
			return errorResult("conversation_id is required"), nil
		}
		if message == "" {
			return errorResult("message is required"), nil
		}

		conv, msg, err := sendTextToConversation(a, conversationID, message)
		if err != nil {
			return errorResult(fmt.Sprintf("failed to send: %v", err)), nil
		}

		return structuredResult(map[string]any{
			"ok":           true,
			"conversation": conv,
			"message":      msg,
		}, fmt.Sprintf("Message sent to %s (%s): %s", conv.Name, conversationID, message)), nil
	}
}
