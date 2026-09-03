package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/maxghenis/openmessage/internal/app"
	"github.com/maxghenis/openmessage/internal/db"
)

func getMessagesTool() mcp.Tool {
	return mcp.NewTool("get_messages",
		mcp.WithDescription("Get recent messages with optional filters by phone number, date range, and limit"),
		mcp.WithString("phone_number", mcp.Description("Filter by sender phone number")),
		mcp.WithString("after", mcp.Description("Only messages on/after this date (YYYY-MM-DD, local time, e.g. 2026-02-01)")),
		mcp.WithString("before", mcp.Description("Only messages on/before this date (YYYY-MM-DD, local time, inclusive to end of day)")),
		mcp.WithNumber("limit", mcp.Description("Maximum messages to return (default 20)")),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
	)
}

func getMessagesHandler(a *app.App, configured ...Options) server.ToolHandlerFunc {
	options := resolvedOptions(a, configured)
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		phone := strArg(args, "phone_number")
		limit := intArg(args, "limit", 20)

		// Same parser as the CLI, /api/search/messages, and the person tools,
		// so one date string selects the same local-time window everywhere.
		afterMS, err := db.ParseDayBound(strArg(args, "after"), false)
		if err != nil {
			return errorResult(fmt.Sprintf("invalid 'after' date: %v", err)), nil
		}
		beforeMS, err := db.ParseDayBound(strArg(args, "before"), true)
		if err != nil {
			return errorResult(fmt.Sprintf("invalid 'before' date: %v", err)), nil
		}

		var msgs []*db.Message
		if options.V2Primary {
			// v2 has no separate unscoped-list query. An empty substring query is
			// deliberately LIKE '%%', with the same filters and recency ordering.
			msgs, err = options.Reads.SearchMessagesFiltered("", db.SearchFilter{
				Phone:   phone,
				SinceMS: afterMS,
				UntilMS: beforeMS,
				Limit:   limit,
			})
		} else {
			// Keep the legacy call exact: unlike filtered search, GetMessages does
			// not add message_id as a timestamp tie-breaker.
			msgs, err = a.Store.GetMessages(phone, afterMS, beforeMS, limit)
		}
		if err != nil {
			return errorResult(fmt.Sprintf("query failed: %v", err)), nil
		}

		if len(msgs) == 0 {
			return textResult("No messages found."), nil
		}

		var sb strings.Builder
		sb.WriteString(messagePreamble)
		for _, m := range msgs {
			sb.WriteString(formatMessageLine(m))
			sb.WriteByte('\n')
		}
		return textResult(sb.String()), nil
	}
}
