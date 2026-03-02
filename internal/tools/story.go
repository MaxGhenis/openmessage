package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/maxghenis/openmessage/internal/app"
	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/story"
)

func conversationStatsTool() mcp.Tool {
	return mcp.NewTool("conversation_stats",
		mcp.WithDescription("Compute statistics for a conversation: message volume, heatmap, top phrases, response times, gaps. Works with any platform."),
		mcp.WithString("conversation_id", mcp.Required(), mcp.Description("The conversation ID")),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
	)
}

func conversationStatsHandler(a *app.App) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		convID := strArg(args, "conversation_id")
		if convID == "" {
			return errorResult("conversation_id is required"), nil
		}

		// Get all messages for this conversation
		msgs, err := a.Store.GetMessagesByConversation(convID, 100000)
		if err != nil {
			return errorResult(fmt.Sprintf("get messages: %v", err)), nil
		}
		if len(msgs) == 0 {
			return textResult("No messages found in this conversation."), nil
		}

		stats := story.ComputeStats(msgs, nil)
		statsJSON, _ := json.MarshalIndent(stats, "", "  ")

		var sb strings.Builder
		conv, _ := a.Store.GetConversation(convID)
		if conv != nil {
			platform := conv.SourcePlatform
			if platform == "" {
				platform = "sms"
			}
			fmt.Fprintf(&sb, "Stats for: %s [%s]\n\n", conv.Name, platform)
		}

		fmt.Fprintf(&sb, "Total messages: %d\n", stats.TotalMessages)
		fmt.Fprintf(&sb, "Date range: %s to %s\n", stats.DateRange.Start, stats.DateRange.End)
		fmt.Fprintf(&sb, "Sender split: ")
		for sender, count := range stats.SenderSplit {
			fmt.Fprintf(&sb, "%s=%d ", sender, count)
		}
		sb.WriteString("\n")

		if stats.LongestGap.Days > 0 {
			fmt.Fprintf(&sb, "Longest gap: %d days (%s to %s)\n", stats.LongestGap.Days, stats.LongestGap.Start, stats.LongestGap.End)
		}

		for sender, rt := range stats.AvgResponseTimes {
			fmt.Fprintf(&sb, "Avg response time (%s): %d min\n", sender, rt)
		}

		if len(stats.Yearly) > 0 {
			sb.WriteString("\nYearly breakdown:\n")
			for _, ys := range stats.Yearly {
				fmt.Fprintf(&sb, "  %s: %d messages\n", ys.Year, ys.Total)
			}
		}

		if len(stats.TopPhrases) > 0 {
			sb.WriteString("\nTop phrases:\n")
			limit := 20
			if len(stats.TopPhrases) < limit {
				limit = len(stats.TopPhrases)
			}
			for _, p := range stats.TopPhrases[:limit] {
				fmt.Fprintf(&sb, "  %q (%d)\n", p.Phrase, p.Count)
			}
		}

		sb.WriteString("\nFull stats JSON:\n")
		sb.Write(statsJSON)

		return textResult(sb.String()), nil
	}
}

func generateStoryTool() mcp.Tool {
	return mcp.NewTool("generate_story",
		mcp.WithDescription("Generate a narrative story about a conversation or relationship. Creates chapters with themes, quotes, and emotional arc."),
		mcp.WithString("conversation_id", mcp.Required(), mcp.Description("The conversation ID")),
		mcp.WithString("style", mcp.Description("Story style: intimate, professional, friendship (default: auto-detect)")),
		mcp.WithString("api_key", mcp.Description("Anthropic API key for AI-generated narrative (without it, generates stats + sampled quotes)")),
		mcp.WithDestructiveHintAnnotation(false),
	)
}

func generateStoryHandler(a *app.App) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		convID := strArg(args, "conversation_id")
		if convID == "" {
			return errorResult("conversation_id is required"), nil
		}
		style := strArg(args, "style")
		apiKey := strArg(args, "api_key")

		msgs, err := a.Store.GetMessagesByConversation(convID, 100000)
		if err != nil {
			return errorResult(fmt.Sprintf("get messages: %v", err)), nil
		}
		if len(msgs) == 0 {
			return textResult("No messages found in this conversation."), nil
		}

		s, err := story.Generate(msgs, story.GenerateConfig{
			Style:             style,
			APIKey:            apiKey,
			MaxSampleMessages: 200,
		})
		if err != nil {
			return errorResult(fmt.Sprintf("generate story: %v", err)), nil
		}

		var sb strings.Builder
		fmt.Fprintf(&sb, "# %s\n\n", s.Title)
		fmt.Fprintf(&sb, "%s\n\n", s.Summary)
		fmt.Fprintf(&sb, "---\n\n")

		for _, ch := range s.Chapters {
			fmt.Fprintf(&sb, "## %s", ch.Title)
			if ch.Period != "" {
				fmt.Fprintf(&sb, " (%s)", ch.Period)
			}
			sb.WriteString("\n\n")

			if ch.Content != "" {
				fmt.Fprintf(&sb, "%s\n\n", ch.Content)
			}

			for _, q := range ch.Quotes {
				ts := q.Timestamp
				if t, err := time.Parse(time.RFC3339, ts); err == nil {
					ts = t.Format("Jan 2, 2006")
				}
				fmt.Fprintf(&sb, "> **%s** (%s): \"%s\"\n\n", q.Sender, ts, q.Text)
			}
		}

		// Append key stats
		fmt.Fprintf(&sb, "---\n\n**Stats:** %d messages, %s to %s\n",
			s.Stats.TotalMessages, s.Stats.DateRange.Start, s.Stats.DateRange.End)

		return textResult(sb.String()), nil
	}
}

// --- Person-level tools (cross-platform) ---

func personStatsTool() mcp.Tool {
	return mcp.NewTool("person_stats",
		mcp.WithDescription("Compute statistics for all messages with a person across all platforms (SMS, iMessage, WhatsApp, etc). Merges and deduplicates cross-platform messages."),
		mcp.WithString("name", mcp.Required(), mcp.Description("Person's name to search for (case-insensitive partial match)")),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
	)
}

func personStatsHandler(a *app.App) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		name := strArg(args, "name")
		if name == "" {
			return errorResult("name is required"), nil
		}

		msgs, convNames, err := collectPersonMessages(a, name)
		if err != nil {
			return errorResult(err.Error()), nil
		}
		if len(msgs) == 0 {
			return textResult(fmt.Sprintf("No messages found with '%s'.", name)), nil
		}

		stats := story.ComputeStats(msgs, nil)
		statsJSON, _ := json.MarshalIndent(stats, "", "  ")

		var sb strings.Builder
		fmt.Fprintf(&sb, "Stats for messages with '%s' across %d conversation(s):\n", name, len(convNames))
		for _, cn := range convNames {
			fmt.Fprintf(&sb, "  - %s\n", cn)
		}
		sb.WriteString("\n")

		fmt.Fprintf(&sb, "Total messages: %d (after dedup)\n", stats.TotalMessages)
		fmt.Fprintf(&sb, "Date range: %s to %s\n", stats.DateRange.Start, stats.DateRange.End)
		fmt.Fprintf(&sb, "Sender split: ")
		for sender, count := range stats.SenderSplit {
			fmt.Fprintf(&sb, "%s=%d ", sender, count)
		}
		sb.WriteString("\n")

		if stats.LongestGap.Days > 0 {
			fmt.Fprintf(&sb, "Longest gap: %d days (%s to %s)\n", stats.LongestGap.Days, stats.LongestGap.Start, stats.LongestGap.End)
		}

		for sender, rt := range stats.AvgResponseTimes {
			fmt.Fprintf(&sb, "Avg response time (%s): %d min\n", sender, rt)
		}

		if len(stats.Yearly) > 0 {
			sb.WriteString("\nYearly breakdown:\n")
			for _, ys := range stats.Yearly {
				fmt.Fprintf(&sb, "  %s: %d messages\n", ys.Year, ys.Total)
			}
		}

		if len(stats.TopPhrases) > 0 {
			sb.WriteString("\nTop phrases:\n")
			limit := 20
			if len(stats.TopPhrases) < limit {
				limit = len(stats.TopPhrases)
			}
			for _, p := range stats.TopPhrases[:limit] {
				fmt.Fprintf(&sb, "  %q (%d)\n", p.Phrase, p.Count)
			}
		}

		sb.WriteString("\nFull stats JSON:\n")
		sb.Write(statsJSON)

		return textResult(sb.String()), nil
	}
}

func generatePersonStoryTool() mcp.Tool {
	return mcp.NewTool("generate_person_story",
		mcp.WithDescription("Generate a narrative story about your relationship with a person, across all platforms (SMS, iMessage, WhatsApp, etc). Merges and deduplicates cross-platform messages."),
		mcp.WithString("name", mcp.Required(), mcp.Description("Person's name to search for (case-insensitive partial match)")),
		mcp.WithString("style", mcp.Description("Story style: intimate, professional, friendship (default: auto-detect)")),
		mcp.WithString("api_key", mcp.Description("Anthropic API key for AI-generated narrative (without it, generates stats + sampled quotes)")),
		mcp.WithDestructiveHintAnnotation(false),
	)
}

func generatePersonStoryHandler(a *app.App) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		name := strArg(args, "name")
		if name == "" {
			return errorResult("name is required"), nil
		}
		style := strArg(args, "style")
		apiKey := strArg(args, "api_key")

		msgs, convNames, err := collectPersonMessages(a, name)
		if err != nil {
			return errorResult(err.Error()), nil
		}
		if len(msgs) == 0 {
			return textResult(fmt.Sprintf("No messages found with '%s'.", name)), nil
		}

		s, err := story.Generate(msgs, story.GenerateConfig{
			Style:             style,
			APIKey:            apiKey,
			MaxSampleMessages: 200,
		})
		if err != nil {
			return errorResult(fmt.Sprintf("generate story: %v", err)), nil
		}

		var sb strings.Builder
		fmt.Fprintf(&sb, "# %s\n\n", s.Title)
		fmt.Fprintf(&sb, "*Based on %d messages across %d conversation(s): %s*\n\n",
			s.Stats.TotalMessages, len(convNames), strings.Join(convNames, ", "))
		fmt.Fprintf(&sb, "%s\n\n", s.Summary)
		fmt.Fprintf(&sb, "---\n\n")

		for _, ch := range s.Chapters {
			fmt.Fprintf(&sb, "## %s", ch.Title)
			if ch.Period != "" {
				fmt.Fprintf(&sb, " (%s)", ch.Period)
			}
			sb.WriteString("\n\n")

			if ch.Content != "" {
				fmt.Fprintf(&sb, "%s\n\n", ch.Content)
			}

			for _, q := range ch.Quotes {
				ts := q.Timestamp
				if t, err := time.Parse(time.RFC3339, ts); err == nil {
					ts = t.Format("Jan 2, 2006")
				}
				fmt.Fprintf(&sb, "> **%s** (%s): \"%s\"\n\n", q.Sender, ts, q.Text)
			}
		}

		fmt.Fprintf(&sb, "---\n\n**Stats:** %d messages, %s to %s\n",
			s.Stats.TotalMessages, s.Stats.DateRange.Start, s.Stats.DateRange.End)

		return textResult(sb.String()), nil
	}
}

// collectPersonMessages finds all 1:1 conversations matching the name, loads
// all messages, deduplicates cross-platform duplicates, and returns them sorted
// chronologically. Also returns conversation display names for context.
func collectPersonMessages(a *app.App, name string) ([]*db.Message, []string, error) {
	allConvs, err := a.Store.ListConversations(1000)
	if err != nil {
		return nil, nil, fmt.Errorf("list conversations: %v", err)
	}

	nameLower := strings.ToLower(name)
	var matchingConvIDs []string
	var convNames []string
	for _, c := range allConvs {
		// Skip group chats — we want 1:1 conversations with this person
		if c.IsGroup {
			continue
		}
		if strings.Contains(strings.ToLower(c.Name), nameLower) ||
			strings.Contains(strings.ToLower(c.Participants), nameLower) {
			matchingConvIDs = append(matchingConvIDs, c.ConversationID)
			platform := c.SourcePlatform
			if platform == "" {
				platform = "sms"
			}
			convNames = append(convNames, fmt.Sprintf("%s [%s]", c.Name, platform))
		}
	}

	if len(matchingConvIDs) == 0 {
		return nil, nil, nil
	}

	msgs, err := a.Store.GetMessagesByConversations(matchingConvIDs, 500000)
	if err != nil {
		return nil, nil, fmt.Errorf("get messages: %v", err)
	}

	// Deduplicate: messages with identical body + timestamp within 2 seconds
	// are likely the same message on different platforms (SMS + iMessage).
	deduped := deduplicateMessages(msgs)

	return deduped, convNames, nil
}

// deduplicateMessages removes near-duplicate messages (same body and timestamp
// within 2 seconds). Keeps the first occurrence (by platform order).
func deduplicateMessages(msgs []*db.Message) []*db.Message {
	if len(msgs) <= 1 {
		return msgs
	}

	// Sort by timestamp
	sort.Slice(msgs, func(i, j int) bool {
		return msgs[i].TimestampMS < msgs[j].TimestampMS
	})

	var result []*db.Message
	for _, m := range msgs {
		isDup := false
		// Check against recent messages (look back up to 20 for efficiency)
		start := len(result) - 20
		if start < 0 {
			start = 0
		}
		for i := len(result) - 1; i >= start; i-- {
			prev := result[i]
			tsDiff := m.TimestampMS - prev.TimestampMS
			if tsDiff > 2000 {
				break // Too far apart in time
			}
			if tsDiff >= 0 && tsDiff <= 2000 && m.Body == prev.Body && m.IsFromMe == prev.IsFromMe {
				isDup = true
				break
			}
		}
		if !isDup {
			result = append(result, m)
		}
	}
	return result
}
