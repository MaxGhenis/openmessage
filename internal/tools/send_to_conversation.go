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

func sendToConversationTool(v2Enabled ...bool) mcp.Tool {
	description := "Send a text message to an existing conversation by conversation ID across supported platforms"
	options := []mcp.ToolOption{
		mcp.WithDescription(description),
		mcp.WithString("conversation_id", mcp.Required(), mcp.Description("Existing conversation ID from list_conversations or get_conversation")),
		mcp.WithString("message", mcp.Required(), mcp.Description("Message text to send")),
	}
	if v2Requested(v2Enabled) {
		options[0] = mcp.WithDescription(description + v2DeliveryDescription)
		options = append(options, mcp.WithString("idempotency_key", mcp.Description(v2IdempotencyDescription)))
	}
	options = append(options,
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(false),
	)
	return mcp.NewTool("send_to_conversation", options...)
}

func sendToConversationHandler(a *app.App, v2Options ...*V2Dependencies) server.ToolHandlerFunc {
	v2 := activeV2(v2Options)
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
		if v2 != nil {
			return submitV2Text(ctx, a, v2, args, conversationID, message), nil
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
