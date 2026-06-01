package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"

	"github.com/maxghenis/openmessage/internal/app"
	"github.com/maxghenis/openmessage/internal/db"
)

func sendGroupMessageTool() mcp.Tool {
	return mcp.NewTool("send_group_message",
		mcp.WithDescription("Send a text message to a group conversation (MMS group). Creates the group if it doesn't exist."),
		mcp.WithString("phone_numbers", mcp.Required(), mcp.Description(`JSON array of phone numbers with country code, e.g. ["+15551234567", "+15559876543"]`)),
		mcp.WithString("message", mcp.Required(), mcp.Description("Message text to send")),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(false),
	)
}

func sendGroupMessageHandler(a *app.App) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		phonesRaw := strArg(args, "phone_numbers")
		message := strArg(args, "message")

		if phonesRaw == "" {
			return errorResult("phone_numbers is required"), nil
		}
		if message == "" {
			return errorResult("message is required"), nil
		}

		var phones []string
		if err := json.Unmarshal([]byte(phonesRaw), &phones); err != nil {
			return errorResult(fmt.Sprintf("phone_numbers must be a JSON array of strings: %v", err)), nil
		}
		if len(phones) < 2 {
			return errorResult("phone_numbers must contain at least 2 numbers for a group message"), nil
		}

		cli := a.GetClient()
		if cli == nil {
			return errorResult(app.ErrNotConnected), nil
		}

		convResp, err := cli.GM.GetOrCreateConversation(&gmproto.GetOrCreateConversationRequest{
			Numbers: app.NewContactNumbers(phones),
		})
		if err != nil {
			return errorResult(fmt.Sprintf("failed to get/create group conversation: %v", err)), nil
		}

		conv := convResp.GetConversation()
		if conv == nil {
			return errorResult("no conversation returned"), nil
		}

		payload := app.BuildSendPayload(conv.GetConversationID(), message, "", "", nil)
		resp, err := cli.GM.SendMessage(payload)
		if err != nil {
			return errorResult(fmt.Sprintf("failed to send group message: %v", err)), nil
		}
		// Surface a carrier/Google rejection instead of reporting success on a
		// non-SUCCESS status (matches the 1:1 send path).
		if resp.GetStatus() != gmproto.SendMessageResponse_SUCCESS {
			return errorResult(fmt.Sprintf("failed to send group message: %s", resp.GetStatus().String())), nil
		}

		// Persist so the group send appears in local history (like 1:1 sends).
		if err := a.Store.RecordOutgoingMessage(&db.Message{
			MessageID:      payload.TmpID,
			ConversationID: conv.GetConversationID(),
			Body:           message,
			IsFromMe:       true,
			TimestampMS:    time.Now().UnixMilli(),
			Status:         "OUTGOING_SENDING",
			SourcePlatform: "sms",
		}, ""); err != nil {
			return errorResult(fmt.Sprintf("group message sent but failed to persist: %v", err)), nil
		}

		return textResult(fmt.Sprintf("Group message sent to %s: %s", strings.Join(phones, ", "), message)), nil
	}
}
