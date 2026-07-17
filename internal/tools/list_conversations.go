package tools

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/maxghenis/openmessage/internal/app"
	"github.com/maxghenis/openmessage/internal/db"
)

func listConversationsTool() mcp.Tool {
	return mcp.NewTool("list_conversations",
		mcp.WithDescription("List recent conversations, sorted by most recent message. Includes conversations from all synced platforms such as SMS/RCS, Google Chat, iMessage, WhatsApp, and Signal."),
		mcp.WithNumber("limit", mcp.Description("Maximum conversations to return (default 20)")),
		mcp.WithString("source_platform", mcp.Description("Filter by platform: sms, gchat, imessage, whatsapp, signal, telegram")),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
	)
}

func listConversationsHandler(a *app.App, configured ...Options) server.ToolHandlerFunc {
	options := resolvedOptions(a, configured)
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		limit := intArg(args, "limit", 20)
		platform := strArg(args, "source_platform")

		var convs []*db.Conversation
		var err error
		if !options.V2Primary && platform != "" {
			convs, err = a.Store.ListConversationsByPlatform(platform, limit)
		} else if !options.V2Primary {
			convs, err = a.Store.ListConversations(limit)
		} else if platform == "" {
			convs, err = options.Reads.ListConversations(limit)
		} else if limit <= 0 {
			convs = []*db.Conversation{}
		} else {
			// ReadSource intentionally mirrors only the canonical legacy methods;
			// filter the v2 cross-account recency result here rather than widening
			// the cutover seam with a platform-specific convenience query.
			var candidates []*db.Conversation
			candidates, err = options.Reads.ListConversations(math.MaxInt)
			if err == nil {
				for _, conversation := range candidates {
					if conversation != nil && normalizedPlatform(conversation.SourcePlatform) == normalizedPlatform(platform) {
						convs = append(convs, conversation)
						if len(convs) >= limit {
							break
						}
					}
				}
			}
		}
		if err != nil {
			return errorResult(fmt.Sprintf("query failed: %v", err)), nil
		}

		if len(convs) == 0 {
			msg := "No conversations found."
			if platform == "" {
				msg += " Messages may not have synced yet."
			}
			return structuredResult(map[string]any{
				"count":           0,
				"source_platform": platform,
				"conversations":   []conversationSummary{},
			}, msg), nil
		}

		var sb strings.Builder
		summaries := make([]conversationSummary, 0, len(convs))
		fmt.Fprintf(&sb, "%d conversations:\n\n", len(convs))
		for _, c := range convs {
			summaries = append(summaries, summarizeConversation(c))
			ts := time.UnixMilli(c.LastMessageTS).Format(time.RFC3339)
			group := ""
			if c.IsGroup {
				group = " [group]"
			}
			unread := ""
			if c.UnreadCount > 0 {
				unread = fmt.Sprintf(" (%d unread)", c.UnreadCount)
			}
			platform := ""
			if c.SourcePlatform != "" && c.SourcePlatform != "sms" {
				platform = fmt.Sprintf(" [%s]", c.SourcePlatform)
			}
			fmt.Fprintf(&sb, "- %s%s%s%s (ID: %s, last: %s)\n", c.Name, platform, group, unread, c.ConversationID, ts)
		}
		return structuredResult(map[string]any{
			"count":           len(summaries),
			"source_platform": platform,
			"conversations":   summaries,
		}, sb.String()), nil
	}
}
