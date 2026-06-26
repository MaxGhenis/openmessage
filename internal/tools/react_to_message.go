package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"

	"github.com/maxghenis/openmessage/internal/app"
)

var (
	sendWhatsAppReactionMessage = func(a *app.App, conversationID, messageID, emoji, action string) error {
		return a.SendWhatsAppReaction(conversationID, messageID, emoji, action)
	}
	sendSignalReactionMessage = func(a *app.App, conversationID, messageID, emoji, action string) error {
		return a.SendSignalReaction(conversationID, messageID, emoji, action)
	}
	sendGoogleReaction = func(a *app.App, payload *gmproto.SendReactionRequest) (*gmproto.SendReactionResponse, error) {
		cli := a.GetClient()
		if cli == nil {
			return nil, fmt.Errorf(app.ErrNotConnected)
		}
		return cli.GM.SendReaction(payload)
	}
	getGoogleReactionConversation = func(a *app.App, conversationID string) (*gmproto.Conversation, error) {
		cli := a.GetClient()
		if cli == nil {
			return nil, fmt.Errorf(app.ErrNotConnected)
		}
		return cli.GM.GetConversation(conversationID)
	}
)

func reactToMessageTool() mcp.Tool {
	return mcp.NewTool("react_to_message",
		mcp.WithDescription("Add, remove, or switch a reaction on an existing message across supported platforms"),
		mcp.WithString("conversation_id", mcp.Required(), mcp.Description("Conversation ID containing the target message")),
		mcp.WithString("message_id", mcp.Required(), mcp.Description("Target message ID")),
		mcp.WithString("emoji", mcp.Required(), mcp.Description("Emoji reaction to apply")),
		mcp.WithString("action", mcp.Description("Optional action: add, remove, or switch. Defaults to add.")),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(false),
	)
}

func reactToMessageHandler(a *app.App) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		conversationID := strArg(args, "conversation_id")
		messageID := strArg(args, "message_id")
		emoji := strArg(args, "emoji")
		action := normalizeReactionAction(strArg(args, "action"))

		if conversationID == "" {
			return errorResult("conversation_id is required"), nil
		}
		if messageID == "" {
			return errorResult("message_id is required"), nil
		}
		if emoji == "" {
			return errorResult("emoji is required"), nil
		}

		conv, err := a.Store.GetConversation(conversationID)
		if err != nil {
			return errorResult(fmt.Sprintf("failed to load conversation: %v", err)), nil
		}
		if conv == nil {
			return errorResult(fmt.Sprintf("conversation %s not found", conversationID)), nil
		}

		switch conv.SourcePlatform {
		case "whatsapp":
			if err := sendWhatsAppReactionMessage(a, conversationID, messageID, emoji, action); err != nil {
				return errorResult(fmt.Sprintf("failed to send reaction: %v", err)), nil
			}
		case "signal":
			if err := sendSignalReactionMessage(a, conversationID, messageID, emoji, action); err != nil {
				return errorResult(fmt.Sprintf("failed to send reaction: %v", err)), nil
			}
		case "", "sms":
			gmConv, err := getGoogleReactionConversation(a, conversationID)
			if err != nil {
				a.HandleGoogleAuthExpiredError(err)
				return errorResult(fmt.Sprintf("get conversation: %v", err)), nil
			}
			_, simPayload := app.ExtractSIMAndParticipant(gmConv)
			payload := app.BuildReactionPayload(messageID, emoji, action, simPayload)
			resp, err := sendGoogleReaction(a, payload)
			if err != nil {
				a.HandleGoogleAuthExpiredError(err)
				return errorResult(fmt.Sprintf("failed to send reaction: %v", err)), nil
			}
			if !resp.GetSuccess() {
				return errorResult("failed to send reaction"), nil
			}
		default:
			return errorResult(fmt.Sprintf("reactions are not supported for platform %s via OpenMessage MCP yet", conv.SourcePlatform)), nil
		}

		return textResult(fmt.Sprintf("Reaction %s applied to message %s in %s", action, messageID, conversationName(conv))), nil
	}
}

func normalizeReactionAction(action string) string {
	switch strings.ToLower(strings.TrimSpace(action)) {
	case "remove":
		return "remove"
	case "switch":
		return "switch"
	default:
		return "add"
	}
}
