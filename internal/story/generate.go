package story

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/maxghenis/openmessage/internal/db"
)

// Story represents a generated narrative about a conversation/relationship.
type Story struct {
	Title    string    `json:"title"`
	Summary  string    `json:"summary"`
	Chapters []Chapter `json:"chapters"`
	Stats    *Stats    `json:"stats"`
}

// Chapter is a thematic section of the story.
type Chapter struct {
	Title    string  `json:"title"`
	Content  string  `json:"content"`
	Quotes   []Quote `json:"quotes"`
	Period   string  `json:"period"` // e.g. "2013-2015"
}

// Quote is a notable message from the conversation.
type Quote struct {
	Sender    string `json:"sender"`
	Text      string `json:"text"`
	Timestamp string `json:"timestamp"`
}

// GenerateConfig controls story generation.
type GenerateConfig struct {
	// Style: "intimate", "professional", "friendship" (default: auto-detect)
	Style string
	// APIKey for Claude API calls.
	APIKey string
	// MaxSampleMessages limits how many messages are sent to the API.
	MaxSampleMessages int
}

// Generate creates a story from a set of messages.
// If no API key is provided, it generates a stats-only story with sampled quotes.
func Generate(messages []*db.Message, config GenerateConfig) (*Story, error) {
	if len(messages) == 0 {
		return nil, fmt.Errorf("no messages to generate story from")
	}

	// Sort ascending
	sorted := make([]*db.Message, len(messages))
	copy(sorted, messages)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].TimestampMS < sorted[j].TimestampMS
	})

	stats := ComputeStats(sorted, nil)
	sampled := sampleMessages(sorted, config.MaxSampleMessages)

	story := &Story{
		Stats: stats,
	}

	if config.APIKey != "" {
		// Use Claude API to generate the narrative
		chapters, title, summary, err := generateWithClaude(sampled, stats, config)
		if err != nil {
			// Fall back to local generation
			story.Title, story.Summary, story.Chapters = generateLocal(sorted, sampled, stats)
		} else {
			story.Title = title
			story.Summary = summary
			story.Chapters = chapters
		}
	} else {
		story.Title, story.Summary, story.Chapters = generateLocal(sorted, sampled, stats)
	}

	return story, nil
}

// sampleMessages picks representative messages across the timeline.
func sampleMessages(messages []*db.Message, maxSamples int) []*db.Message {
	if maxSamples <= 0 {
		maxSamples = 200
	}
	if len(messages) <= maxSamples {
		return messages
	}

	var sampled []*db.Message

	// Always include first and last 5 messages
	n := 5
	if n > len(messages)/4 {
		n = len(messages) / 4
	}
	sampled = append(sampled, messages[:n]...)
	sampled = append(sampled, messages[len(messages)-n:]...)

	// Sample evenly from the rest
	remaining := maxSamples - 2*n
	if remaining <= 0 {
		return sampled
	}
	step := float64(len(messages)-2*n) / float64(remaining)
	for i := 0; i < remaining; i++ {
		idx := n + int(float64(i)*step)
		if idx < len(messages) {
			sampled = append(sampled, messages[idx])
		}
	}

	// Deduplicate and sort
	seen := map[string]bool{}
	var unique []*db.Message
	for _, m := range sampled {
		if !seen[m.MessageID] {
			seen[m.MessageID] = true
			unique = append(unique, m)
		}
	}
	sort.Slice(unique, func(i, j int) bool {
		return unique[i].TimestampMS < unique[j].TimestampMS
	})
	return unique
}

// generateLocal creates chapters from the data without an LLM.
func generateLocal(all, sampled []*db.Message, stats *Stats) (title, summary string, chapters []Chapter) {
	// Determine participants
	var otherNames []string
	for sender := range stats.SenderSplit {
		if sender != "me" {
			otherNames = append(otherNames, sender)
		}
	}

	if len(otherNames) == 1 {
		title = fmt.Sprintf("Messages with %s", otherNames[0])
	} else {
		title = "Message history"
	}

	summary = fmt.Sprintf("%d messages from %s to %s", stats.TotalMessages, stats.DateRange.Start, stats.DateRange.End)

	// Group messages by year for chapters
	yearMsgs := map[string][]*db.Message{}
	for _, m := range sampled {
		year := time.UnixMilli(m.TimestampMS).UTC().Format("2006")
		yearMsgs[year] = append(yearMsgs[year], m)
	}

	var years []string
	for y := range yearMsgs {
		years = append(years, y)
	}
	sort.Strings(years)

	for _, year := range years {
		msgs := yearMsgs[year]
		ch := Chapter{
			Title:  year,
			Period: year,
		}

		// Pick notable quotes (longest messages)
		sort.Slice(msgs, func(i, j int) bool {
			return len(msgs[i].Body) > len(msgs[j].Body)
		})

		maxQuotes := 5
		if len(msgs) < maxQuotes {
			maxQuotes = len(msgs)
		}
		for _, m := range msgs[:maxQuotes] {
			ch.Quotes = append(ch.Quotes, Quote{
				Sender:    senderKey(m),
				Text:      truncate(m.Body, 200),
				Timestamp: time.UnixMilli(m.TimestampMS).UTC().Format(time.RFC3339),
			})
		}

		// Build content summary
		var ys *YearStats
		for i := range stats.Yearly {
			if stats.Yearly[i].Year == year {
				ys = &stats.Yearly[i]
				break
			}
		}
		if ys != nil {
			ch.Content = fmt.Sprintf("%d messages exchanged", ys.Total)
		}

		chapters = append(chapters, ch)
	}

	return title, summary, chapters
}

func truncate(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen-3]) + "..."
}

// generateWithClaude calls the Claude API to create a narrative story.
func generateWithClaude(sampled []*db.Message, stats *Stats, config GenerateConfig) ([]Chapter, string, string, error) {
	// Format messages for the prompt
	var msgLines []string
	for _, m := range sampled {
		ts := time.UnixMilli(m.TimestampMS).UTC().Format("2006-01-02 15:04")
		sender := senderKey(m)
		body := truncate(m.Body, 300)
		msgLines = append(msgLines, fmt.Sprintf("[%s] %s: %s", ts, sender, body))
	}

	style := config.Style
	if style == "" {
		style = "auto"
	}

	prompt := fmt.Sprintf(`You are analyzing a conversation between people to create an engaging story about their relationship.

Stats:
- Total messages: %d
- Date range: %s to %s
- Participants: %v

Style: %s (if "auto", determine from the messages whether this is intimate/romantic, professional, or friendship)

Here are sampled messages across the timeline:

%s

Create a JSON response with:
{
  "title": "A compelling title for their story",
  "summary": "A 2-3 sentence summary of the relationship arc",
  "chapters": [
    {
      "title": "Chapter title",
      "content": "2-3 paragraph narrative about this period",
      "period": "YYYY or YYYY-YYYY",
      "quotes": [{"sender": "name", "text": "notable quote", "timestamp": "ISO date"}]
    }
  ]
}

Create 3-6 chapters that capture the emotional arc. Focus on themes, turning points, and the evolution of the relationship. Use specific quotes from the messages as evidence.`,
		stats.TotalMessages,
		stats.DateRange.Start,
		stats.DateRange.End,
		participantList(stats),
		style,
		strings.Join(msgLines, "\n"),
	)

	// Call Claude API
	body := map[string]any{
		"model":      "claude-sonnet-4-5-20250929",
		"max_tokens": 4096,
		"messages": []map[string]string{
			{"role": "user", "content": prompt},
		},
	}
	bodyJSON, _ := json.Marshal(body)

	req, err := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", bytes.NewReader(bodyJSON))
	if err != nil {
		return nil, "", "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", config.APIKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, "", "", fmt.Errorf("API call failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, "", "", fmt.Errorf("API returned %d: %s", resp.StatusCode, string(respBody))
	}

	var apiResp struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return nil, "", "", fmt.Errorf("decode response: %w", err)
	}

	if len(apiResp.Content) == 0 {
		return nil, "", "", fmt.Errorf("empty response from API")
	}

	// Parse the JSON from the response
	text := apiResp.Content[0].Text
	// Extract JSON from possible markdown code block
	if idx := strings.Index(text, "{"); idx >= 0 {
		text = text[idx:]
	}
	if idx := strings.LastIndex(text, "}"); idx >= 0 {
		text = text[:idx+1]
	}

	var storyResp struct {
		Title    string    `json:"title"`
		Summary  string    `json:"summary"`
		Chapters []Chapter `json:"chapters"`
	}
	if err := json.Unmarshal([]byte(text), &storyResp); err != nil {
		return nil, "", "", fmt.Errorf("parse story JSON: %w", err)
	}

	return storyResp.Chapters, storyResp.Title, storyResp.Summary, nil
}

func participantList(stats *Stats) string {
	var parts []string
	for sender, count := range stats.SenderSplit {
		parts = append(parts, fmt.Sprintf("%s (%d msgs)", sender, count))
	}
	return strings.Join(parts, ", ")
}
