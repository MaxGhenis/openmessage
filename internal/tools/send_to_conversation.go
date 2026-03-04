package tools

import (
	"context"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"

	"github.com/maxghenis/openmessage/internal/app"
	"github.com/maxghenis/openmessage/internal/web"
)

func sendToConversationTool() mcp.Tool {
	return mcp.NewTool("send_to_conversation",
		mcp.WithDescription("Send a text message to an existing conversation by its ID. Use list_conversations to find conversation IDs."),
		mcp.WithString("conversation_id", mcp.Required(), mcp.Description("The conversation ID to send to (from list_conversations)")),
		mcp.WithString("message", mcp.Required(), mcp.Description("Message text to send")),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(false),
	)
}

func sendToConversationHandler(a *app.App) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		convID := strArg(args, "conversation_id")
		message := strArg(args, "message")

		if convID == "" {
			return errorResult("conversation_id is required"), nil
		}
		if message == "" {
			return errorResult("message is required"), nil
		}
		if a.Client == nil {
			return errorResult("not connected to Google Messages"), nil
		}

		// Fetch conversation to get SIM and participant info
		conv, err := a.Client.GM.GetConversation(convID)
		if err != nil {
			return errorResult(fmt.Sprintf("failed to get conversation: %v", err)), nil
		}

		// Find our participant ID and SIM payload
		var myParticipantID string
		var simPayload *gmproto.SIMPayload
		for _, p := range conv.GetParticipants() {
			if p.GetIsMe() {
				if id := p.GetID(); id != nil {
					myParticipantID = id.GetNumber()
				}
				simPayload = p.GetSimPayload()
				break
			}
		}

		// Fall back to SIM card from conversation itself
		if simPayload == nil {
			if sc := conv.GetSimCard(); sc != nil {
				simPayload = sc.GetSIMData().GetSIMPayload()
			}
		}

		payload := web.BuildSendPayload(convID, message, "", myParticipantID, simPayload)

		resp, err := a.Client.GM.SendMessage(payload)
		if err != nil {
			return errorResult(fmt.Sprintf("failed to send: %v", err)), nil
		}

		if resp.GetStatus() != gmproto.SendMessageResponse_SUCCESS {
			return errorResult(fmt.Sprintf("send failed with status: %v", resp.GetStatus())), nil
		}

		// Get conversation name for confirmation
		name := convID
		dbConv, err := a.Store.GetConversation(convID)
		if err == nil && dbConv != nil {
			name = dbConv.Name
		}

		return textResult(fmt.Sprintf("Message sent to %s: %s", name, message)), nil
	}
}
