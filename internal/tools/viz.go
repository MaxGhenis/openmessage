package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/maxghenis/openmessage/internal/app"
	"github.com/maxghenis/openmessage/internal/story"
	"github.com/maxghenis/openmessage/internal/viz"
)

func generateVizTool() mcp.Tool {
	return mcp.NewTool("generate_viz",
		mcp.WithDescription("Generate a self-contained HTML visualization of a relationship. Combines data dashboards (charts, heatmap, phrase cloud) with narrative chapters. Output is a single HTML file deployable to Vercel or viewable locally."),
		mcp.WithString("name", mcp.Required(), mcp.Description("Person's name to search for (case-insensitive partial match)")),
		mcp.WithString("output_path", mcp.Required(), mcp.Description("File path to write the HTML output (e.g. /tmp/viz.html)")),
		mcp.WithString("person1", mcp.Description("First person's display name (default: 'Max')")),
		mcp.WithString("person2", mcp.Description("Second person's display name (default: matched name)")),
		mcp.WithString("timezone", mcp.Description("Timezone for heatmap and dates (default: America/New_York)")),
		mcp.WithString("password", mcp.Description("Password to protect the viz (will be SHA-256 hashed client-side). Empty = no gate.")),
		mcp.WithString("primary_color", mcp.Description("Primary CSS color for person2 (e.g. '#be123c')")),
		mcp.WithString("secondary_color", mcp.Description("Secondary CSS color for person1 (e.g. '#d97706')")),
		mcp.WithString("accent_color", mcp.Description("Accent CSS color (e.g. '#fbbf24')")),
		mcp.WithString("background_color", mcp.Description("Background CSS color (e.g. '#0c0a09')")),
		mcp.WithString("style", mcp.Description("Story style: intimate, professional, friendship (default: auto-detect)")),
		mcp.WithString("api_key", mcp.Description("Anthropic API key for AI-generated narrative (without it, generates stats + sampled quotes)")),
		mcp.WithDestructiveHintAnnotation(false),
	)
}

func generateVizHandler(a *app.App) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		name := strArg(args, "name")
		if name == "" {
			return errorResult("name is required"), nil
		}
		outputPath := strArg(args, "output_path")
		if outputPath == "" {
			return errorResult("output_path is required"), nil
		}

		// Collect messages across all platforms
		msgs, convNames, err := collectPersonMessages(a, name)
		if err != nil {
			return errorResult(err.Error()), nil
		}
		if len(msgs) == 0 {
			return textResult(fmt.Sprintf("No messages found with '%s'.", name)), nil
		}

		// Parse timezone
		tzName := strArg(args, "timezone")
		if tzName == "" {
			tzName = "America/New_York"
		}
		tz, err := time.LoadLocation(tzName)
		if err != nil {
			return errorResult(fmt.Sprintf("invalid timezone %q: %v", tzName, err)), nil
		}

		// Compute stats with timezone
		stats := story.ComputeStats(msgs, tz)

		// Generate narrative
		style := strArg(args, "style")
		apiKey := strArg(args, "api_key")
		st, err := story.Generate(msgs, story.GenerateConfig{
			Style:             style,
			APIKey:            apiKey,
			MaxSampleMessages: 200,
		})
		if err != nil {
			return errorResult(fmt.Sprintf("generate story: %v", err)), nil
		}
		// Use the stats we computed with timezone
		st.Stats = stats

		// Find media messages
		mediaMessages := viz.FindMediaMessages(msgs)

		// Build config
		person1 := strArg(args, "person1")
		if person1 == "" {
			person1 = "Max"
		}
		person2 := strArg(args, "person2")
		if person2 == "" {
			// Use the matched name from conversations
			for sender := range stats.SenderSplit {
				if sender != "me" {
					person2 = sender
					break
				}
			}
			if person2 == "" {
				person2 = name
			}
		}

		config := viz.VizConfig{
			Person1:         person1,
			Person2:         person2,
			PrimaryColor:    strArg(args, "primary_color"),
			SecondaryColor:  strArg(args, "secondary_color"),
			AccentColor:     strArg(args, "accent_color"),
			BackgroundColor: strArg(args, "background_color"),
			Timezone:        tzName,
			PasswordHash:    strArg(args, "password"), // raw password — template does SHA-256 client-side
		}

		// Render HTML
		html, err := viz.RenderHTML(stats, st, config)
		if err != nil {
			return errorResult(fmt.Sprintf("render HTML: %v", err)), nil
		}

		// Ensure output directory exists
		if dir := filepath.Dir(outputPath); dir != "" {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return errorResult(fmt.Sprintf("create output dir: %v", err)), nil
			}
		}

		// Write the file
		if err := os.WriteFile(outputPath, html, 0o644); err != nil {
			return errorResult(fmt.Sprintf("write file: %v", err)), nil
		}

		// Build summary
		fileInfo, _ := os.Stat(outputPath)
		sizeKB := 0
		if fileInfo != nil {
			sizeKB = int(fileInfo.Size() / 1024)
		}

		summary := fmt.Sprintf("Visualization generated!\n\n"+
			"Output: %s (%d KB)\n"+
			"Person: %s & %s\n"+
			"Messages: %d across %d conversation(s)\n"+
			"Conversations: %s\n"+
			"Date range: %s to %s\n"+
			"Timezone: %s\n"+
			"Media messages found: %d\n"+
			"Chapters: %d\n",
			outputPath, sizeKB,
			person1, person2,
			stats.TotalMessages, len(convNames),
			joinNames(convNames),
			stats.DateRange.Start, stats.DateRange.End,
			tzName,
			len(mediaMessages),
			len(st.Chapters),
		)

		if config.PasswordHash != "" {
			summary += "Password gate: enabled\n"
		}

		return textResult(summary), nil
	}
}

func joinNames(names []string) string {
	if len(names) > 3 {
		return fmt.Sprintf("%s, %s, %s, +%d more", names[0], names[1], names[2], len(names)-3)
	}
	return strings.Join(names, ", ")
}
