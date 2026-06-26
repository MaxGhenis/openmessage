package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/maxghenis/openmessage/internal/app"
	"github.com/maxghenis/openmessage/internal/story"
	"github.com/maxghenis/openmessage/internal/viz"
)

func renderStoryTool() mcp.Tool {
	return mcp.NewTool("render_story",
		mcp.WithDescription("Render a pre-built Story (title, summary, chapters with quotes) into a self-contained HTML visualization with data dashboards. The story JSON is provided by the caller; stats are computed from all messages with the person. Use this after agentically writing a story to produce the final HTML."),
		mcp.WithString("name", mcp.Required(), mcp.Description("Person's name (for stats computation from all messages)")),
		mcp.WithString("story_json", mcp.Required(), mcp.Description("JSON string of Story struct: {title, summary, chapters: [{title, content, period, quotes: [{sender, text, timestamp}]}]}")),
		mcp.WithString("output_path", mcp.Required(), mcp.Description("Path to write the HTML output. Relative paths are written under OPENMESSAGES_EXPORT_DIR or ~/Documents/OpenMessage; set OPENMESSAGES_ALLOW_ANY_EXPORT_PATH=1 to allow arbitrary paths.")),
		mcp.WithString("person1", mcp.Description("First person's display name (default: 'Max')")),
		mcp.WithString("person2", mcp.Description("Second person's display name (default: matched name)")),
		mcp.WithString("timezone", mcp.Description("Timezone for heatmap and dates (default: America/New_York)")),
		mcp.WithString("password", mcp.Description("Password to protect the viz (SHA-256 hashed client-side). Empty = no gate.")),
		mcp.WithString("primary_color", mcp.Description("Primary CSS color for person2 (e.g. '#be123c')")),
		mcp.WithString("secondary_color", mcp.Description("Secondary CSS color for person1 (e.g. '#d97706')")),
		mcp.WithString("accent_color", mcp.Description("Accent CSS color (e.g. '#fbbf24')")),
		mcp.WithString("background_color", mcp.Description("Background CSS color (e.g. '#0c0a09')")),
		mcp.WithString("photos_dir", mcp.Description("Directory containing photos to include in the gallery")),
		mcp.WithNumber("max_photos", mcp.Description("Maximum number of photos to include (default 20)")),
		mcp.WithString("photo_paths", mcp.Description("JSON array of specific photo file paths to include (overrides photos_dir). Use this for curated photo selection.")),
		mcp.WithDestructiveHintAnnotation(false),
	)
}

func renderStoryHandler(a *app.App) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		name := strArg(args, "name")
		if name == "" {
			return errorResult("name is required"), nil
		}
		storyJSON := strArg(args, "story_json")
		if storyJSON == "" {
			return errorResult("story_json is required"), nil
		}
		outputPath := strArg(args, "output_path")
		if outputPath == "" {
			return errorResult("output_path is required"), nil
		}
		outputPath, err := resolveExportWritePath(outputPath)
		if err != nil {
			return errorResult(err.Error()), nil
		}

		// Parse story JSON
		var parsedStory story.Story
		if err := json.Unmarshal([]byte(storyJSON), &parsedStory); err != nil {
			return errorResult(fmt.Sprintf("invalid story_json: %v", err)), nil
		}

		// Collect all messages for stats computation
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
		parsedStory.Stats = stats

		// Build viz config
		person1 := strArg(args, "person1")
		if person1 == "" {
			person1 = "Max"
		}
		person2 := strArg(args, "person2")
		if person2 == "" {
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
			PasswordHash:    hashPassword(strArg(args, "password")),
		}

		// Encode photos: photo_paths (curated list) takes priority over photos_dir
		photoPathsJSON := strArg(args, "photo_paths")
		photosDir := strArg(args, "photos_dir")
		if photoPathsJSON != "" {
			var paths []string
			if err := json.Unmarshal([]byte(photoPathsJSON), &paths); err != nil {
				return errorResult(fmt.Sprintf("invalid photo_paths JSON: %v", err)), nil
			}
			for i, path := range paths {
				resolved, err := resolveExportReadPath(path)
				if err != nil {
					return errorResult(fmt.Sprintf("photo_paths[%d]: %v", i, err)), nil
				}
				paths[i] = resolved
			}
			photos, err := viz.EncodePhotosFromPaths(paths)
			if err != nil {
				return errorResult(fmt.Sprintf("encode photos: %v", err)), nil
			}
			config.Photos = photos
		} else if photosDir != "" {
			resolved, err := resolveExportReadPath(photosDir)
			if err != nil {
				return errorResult(fmt.Sprintf("photos_dir: %v", err)), nil
			}
			photosDir = resolved
			maxPhotos := intArg(args, "max_photos", 20)
			photos, err := viz.EncodePhotosFromDir(photosDir, maxPhotos)
			if err != nil {
				return errorResult(fmt.Sprintf("encode photos: %v", err)), nil
			}
			config.Photos = photos
		}

		// Render HTML
		html, err := viz.RenderHTML(stats, &parsedStory, config)
		if err != nil {
			return errorResult(fmt.Sprintf("render HTML: %v", err)), nil
		}

		// Write file
		if err := writePrivateExportFile(outputPath, html); err != nil {
			return errorResult(fmt.Sprintf("write file: %v", err)), nil
		}

		// Build summary
		fileInfo, _ := os.Stat(outputPath)
		sizeKB := 0
		if fileInfo != nil {
			sizeKB = int(fileInfo.Size() / 1024)
		}

		summary := fmt.Sprintf("Visualization rendered!\n\n"+
			"Output: %s (%d KB)\n"+
			"Person: %s & %s\n"+
			"Messages: %d across %d conversation(s)\n"+
			"Conversations: %s\n"+
			"Date range: %s to %s\n"+
			"Timezone: %s\n"+
			"Chapters: %d\n",
			outputPath, sizeKB,
			person1, person2,
			stats.TotalMessages, len(convNames),
			joinNames(convNames),
			stats.DateRange.Start, stats.DateRange.End,
			tzName,
			len(parsedStory.Chapters),
		)

		if config.PasswordHash != "" {
			summary += "Password gate: enabled\n"
		}
		if len(config.Photos) > 0 {
			summary += fmt.Sprintf("Photos: %d\n", len(config.Photos))
		}

		return textResult(summary), nil
	}
}
