package tools

import (
	"context"
	"fmt"
	"strings"

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
	description := "Send a text message to an existing conversation by conversation ID. The message goes out on the conversation's own platform — there is never a fallback to a different platform; pass the optional platform argument to assert which platform you intend, and the tool fails on a mismatch instead of sending."
	options := []mcp.ToolOption{
		mcp.WithDescription(description),
		mcp.WithString("conversation_id", mcp.Required(), mcp.Description("Existing conversation ID from list_conversations or get_conversation")),
		mcp.WithString("message", mcp.Required(), mcp.Description("Message text to send")),
		mcp.WithString("platform", mcp.Description("Optional assertion of the platform this conversation must be on (sms, whatsapp, signal). Mismatch fails the send instead of routing to an unintended channel.")),
	}
	if v2Requested(v2Enabled) {
		options[0] = mcp.WithDescription(description + v2DeliveryDescription)
		options = append(options, mcp.WithString("idempotency_key", mcp.Description(v2IdempotencyDescription)))
		options = withSendControlOptions(options, true)
	}
	options = append(options,
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(false),
	)
	return mcp.NewTool("send_to_conversation", options...)
}

// checkConversationPlatformAssertion enforces the optional platform argument
// against the conversation's actual platform.
func checkConversationPlatformAssertion(args map[string]any, actualPlatform, conversationID string) *mcp.CallToolResult {
	requestedRaw := strArg(args, "platform")
	if strings.TrimSpace(requestedRaw) == "" {
		return nil
	}
	requested := normalizeDirectSendPlatform(requestedRaw)
	if actualPlatform != "" && requested != actualPlatform {
		return platformMismatchResult(requested, actualPlatform, conversationID)
	}
	return nil
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
			if failure := checkConversationPlatformAssertion(args, v2.sendPlatform(a, conversationID), conversationID); failure != nil {
				return failure, nil
			}
			return submitV2Text(ctx, a, v2, args, conversationID, message), nil
		}

		if failure := checkConversationPlatformAssertion(args, legacyConversationPlatform(a, conversationID), conversationID); failure != nil {
			return failure, nil
		}
		conv, msg, err := sendTextToConversation(a, conversationID, message)
		if err != nil {
			return errorResult(fmt.Sprintf("failed to send: %v", err)), nil
		}

		return structuredResult(map[string]any{
			"ok":           true,
			"platform":     conv.SourcePlatform,
			"conversation": conv,
			"message":      msg,
		}, fmt.Sprintf("Message sent to %s (%s): %s", conv.Name, conversationID, message)), nil
	}
}

// legacyConversationPlatform resolves a conversation's platform from the
// legacy store, with the same prefix shortcuts as the durable paths.
func legacyConversationPlatform(a *app.App, conversationID string) string {
	switch {
	case strings.HasPrefix(conversationID, "whatsapp:"):
		return "whatsapp"
	case strings.HasPrefix(conversationID, "signal:"), strings.HasPrefix(conversationID, "signal-group:"):
		return "signal"
	}
	if a != nil && a.Store != nil {
		if conversation, err := a.Store.GetConversation(conversationID); err == nil && conversation != nil {
			return normalizedPlatform(conversation.SourcePlatform)
		}
	}
	return ""
}
