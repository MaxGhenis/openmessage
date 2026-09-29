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

var getOrCreateGoogleGroupConversationV2 = func(a *app.App, phones []string) (*gmproto.Conversation, error) {
	cli := a.GetClient()
	if cli == nil {
		return nil, fmt.Errorf(app.ErrNotConnected)
	}
	response, err := cli.GM.GetOrCreateConversation(&gmproto.GetOrCreateConversationRequest{
		Numbers: app.NewContactNumbers(phones),
	})
	if err != nil {
		return nil, err
	}
	return response.GetConversation(), nil
}

func sendGroupMessageTool(v2Enabled ...bool) mcp.Tool {
	description := "Send a text message to a group conversation (MMS group). Creates the group if it doesn't exist."
	options := []mcp.ToolOption{
		mcp.WithDescription(description),
		mcp.WithString("phone_numbers", mcp.Required(), mcp.Description(`JSON array of phone numbers with country code, e.g. ["+15551234567", "+15559876543"]`)),
		mcp.WithString("message", mcp.Required(), mcp.Description("Message text to send")),
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
	return mcp.NewTool("send_group_message", options...)
}

func sendGroupMessageHandler(a *app.App, v2Options ...*V2Dependencies) server.ToolHandlerFunc {
	v2 := activeV2(v2Options)
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
		if v2 != nil {
			conv, err := getOrCreateGoogleGroupConversationV2(a, phones)
			if err != nil {
				if !a.HandleGoogleAuthExpiredError(err) {
					a.RecordGoogleSendError(err)
				}
				return errorResult(fmt.Sprintf("failed to get/create group conversation: %v", err)), nil
			}
			if conv == nil {
				return errorResult("no conversation returned"), nil
			}
			if err := upsertGoogleConversation(a, conv); err != nil {
				return errorResult(fmt.Sprintf("failed to persist conversation: %v", err)), nil
			}
			return submitV2Text(ctx, a, v2, args, conv.GetConversationID(), message), nil
		}

		cli := a.GetClient()
		if cli == nil {
			return errorResult(app.ErrNotConnected), nil
		}

		convResp, err := cli.GM.GetOrCreateConversation(&gmproto.GetOrCreateConversationRequest{
			Numbers: app.NewContactNumbers(phones),
		})
		if err != nil {
			if !a.HandleGoogleAuthExpiredError(err) {
				a.RecordGoogleSendError(err)
			}
			return errorResult(fmt.Sprintf("failed to get/create group conversation: %v", err)), nil
		}

		conv := convResp.GetConversation()
		if conv == nil {
			return errorResult("no conversation returned"), nil
		}

		payload := app.BuildSendPayload(conv.GetConversationID(), message, "", "", nil)
		resp, err := cli.GM.SendMessage(payload)
		if err != nil {
			if !a.HandleGoogleAuthExpiredError(err) {
				a.RecordGoogleSendError(err)
			}
			return errorResult(fmt.Sprintf("failed to send group message: %v", err)), nil
		}
		// Surface a carrier/Google rejection instead of reporting success on a
		// non-SUCCESS status (matches the 1:1 send path).
		if resp.GetStatus() != gmproto.SendMessageResponse_SUCCESS {
			a.RecordGoogleSendOutcomeWithPhone(false, a.GooglePhoneResponding())
			return errorResult(app.GoogleSendRejectedMessage(resp.GetStatus().String(), a.GooglePhoneResponding())), nil
		}
		a.RecordGoogleSendOutcome(true)

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
