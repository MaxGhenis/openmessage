package story

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/quick"

	"github.com/maxghenis/openmessage/internal/db"
)

const testStoryJSON = `{"title":"T","summary":"S","chapters":[{"title":"C1","content":"body","period":"2023","quotes":[{"sender":"Alice","text":"hi","timestamp":"2023-01-01"}]}]}`

func claudeTestMessages() []*db.Message {
	return []*db.Message{
		{MessageID: "1", SenderName: "Alice", Body: "First message ever", TimestampMS: 1672531200000},
		{MessageID: "2", Body: "Replying to you!", TimestampMS: 1672531260000, IsFromMe: true},
		{MessageID: "3", SenderName: "Alice", Body: "A year later message", TimestampMS: 1704067200000},
	}
}

// withClaudeServer points claudeAPIURL at a local server for one test, so no
// test reaches the live API.
func withClaudeServer(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(handler)
	orig := claudeAPIURL
	claudeAPIURL = srv.URL
	t.Cleanup(func() {
		claudeAPIURL = orig
		srv.Close()
	})
}

// claudeReply serves a fixed Messages API response body.
func claudeReply(t *testing.T, resp map[string]any) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}
}

func thinkingBlock(text string) map[string]any {
	return map[string]any{"type": "thinking", "thinking": text, "signature": "sig"}
}

func textBlock(text string) map[string]any {
	return map[string]any{"type": "text", "text": text}
}

func runClaudeStory(t *testing.T) ([]Chapter, string, string, error) {
	t.Helper()
	msgs := claudeTestMessages()
	return generateWithClaude(msgs, ComputeStats(msgs, nil), GenerateConfig{APIKey: "test-key"})
}

func TestClaudeStoryRequestShape(t *testing.T) {
	var body map[string]any
	var headers http.Header
	withClaudeServer(t, func(w http.ResponseWriter, r *http.Request) {
		headers = r.Header.Clone()
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		claudeReply(t, map[string]any{
			"stop_reason": "end_turn",
			"content":     []any{textBlock(testStoryJSON)},
		})(w, r)
	})

	if _, _, _, err := runClaudeStory(t); err != nil {
		t.Fatalf("generateWithClaude: %v", err)
	}

	if got := body["model"]; got != "claude-sonnet-5-5" {
		t.Errorf("model = %v, want claude-sonnet-5-5", got)
	}
	if got := body["max_tokens"]; got != float64(16000) {
		t.Errorf("max_tokens = %v, want 16000", got)
	}
	outputConfig, _ := body["output_config"].(map[string]any)
	if got := outputConfig["effort"]; got != "medium" {
		t.Errorf("output_config.effort = %v, want medium", got)
	}
	// Each of these returns a 400 on Sonnet 5.5 (or, for thinking, is only
	// valid in shapes this request doesn't need).
	for _, key := range []string{"temperature", "top_p", "top_k", "thinking", "tool_choice"} {
		if _, ok := body[key]; ok {
			t.Errorf("request sends %q; Sonnet 5.5 rejects it", key)
		}
	}
	messages, _ := body["messages"].([]any)
	if len(messages) != 1 {
		t.Fatalf("messages = %d, want 1 user turn", len(messages))
	}
	if role := messages[0].(map[string]any)["role"]; role != "user" {
		t.Errorf("last message role = %v, want user (no assistant prefill)", role)
	}
	if got := headers.Get("x-api-key"); got != "test-key" {
		t.Errorf("x-api-key = %q, want test-key", got)
	}
	if got := headers.Get("anthropic-version"); got != "2023-06-01" {
		t.Errorf("anthropic-version = %q, want 2023-06-01", got)
	}
}

func TestGenerateWithClaudeSkipsLeadingThinkingBlock(t *testing.T) {
	withClaudeServer(t, claudeReply(t, map[string]any{
		"stop_reason": "end_turn",
		"content": []any{
			thinkingBlock(""),
			textBlock("```json\n" + testStoryJSON + "\n```"),
		},
	}))

	chapters, title, summary, err := runClaudeStory(t)
	if err != nil {
		t.Fatalf("generateWithClaude: %v", err)
	}
	if title != "T" || summary != "S" {
		t.Errorf("title, summary = %q, %q; want T, S", title, summary)
	}
	if len(chapters) != 1 || chapters[0].Title != "C1" || len(chapters[0].Quotes) != 1 {
		t.Errorf("chapters = %+v, want one chapter C1 with one quote", chapters)
	}
}

func TestGenerateWithClaudeJoinsTextBlocksAroundThinking(t *testing.T) {
	split := len(testStoryJSON) / 2
	withClaudeServer(t, claudeReply(t, map[string]any{
		"stop_reason": "end_turn",
		"content": []any{
			textBlock(testStoryJSON[:split]),
			thinkingBlock("summarized reasoning that is not part of the reply {"),
			textBlock(testStoryJSON[split:]),
		},
	}))

	_, title, _, err := runClaudeStory(t)
	if err != nil {
		t.Fatalf("generateWithClaude: %v", err)
	}
	if title != "T" {
		t.Errorf("title = %q, want T", title)
	}
}

func TestGenerateWithClaudeRefusal(t *testing.T) {
	cases := []struct {
		name        string
		stopDetails any
		want        string
	}{
		{"with category", map[string]any{"type": "refusal", "category": "general_harms"}, "general_harms"},
		{"null category", map[string]any{"type": "refusal", "category": nil}, "unspecified"},
		{"no stop_details", nil, "unspecified"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withClaudeServer(t, claudeReply(t, map[string]any{
				"stop_reason":  "refusal",
				"stop_details": tc.stopDetails,
				"content":      []any{},
			}))
			_, _, _, err := runClaudeStory(t)
			if err == nil {
				t.Fatal("refusal returned no error")
			}
			if !strings.Contains(err.Error(), "refusal") || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to name the refusal and %q", err, tc.want)
			}
		})
	}
}

// A refusal mid-stream can leave partial text; it must not become a story.
func TestGenerateWithClaudeRefusalIgnoresPartialText(t *testing.T) {
	withClaudeServer(t, claudeReply(t, map[string]any{
		"stop_reason": "refusal",
		"content":     []any{textBlock(testStoryJSON)},
	}))
	if _, _, _, err := runClaudeStory(t); err == nil {
		t.Fatal("refusal with partial text returned no error")
	}
}

func TestGenerateWithClaudeMaxTokens(t *testing.T) {
	withClaudeServer(t, claudeReply(t, map[string]any{
		"stop_reason": "max_tokens",
		"content":     []any{thinkingBlock(""), textBlock(`{"title":"T","chap`)},
	}))
	_, _, _, err := runClaudeStory(t)
	if err == nil || !strings.Contains(err.Error(), "max_tokens") {
		t.Fatalf("error = %v, want a max_tokens truncation error", err)
	}
}

func TestGenerateWithClaudeNoTextBlocks(t *testing.T) {
	withClaudeServer(t, claudeReply(t, map[string]any{
		"stop_reason": "end_turn",
		"content":     []any{thinkingBlock("")},
	}))
	if _, _, _, err := runClaudeStory(t); err == nil {
		t.Fatal("reply with only a thinking block returned no error")
	}
}

// Generate keeps its existing contract: any API failure, a refusal included,
// falls back to the local stats-and-quotes story.
func TestGenerateFallsBackToLocalStoryOnRefusal(t *testing.T) {
	withClaudeServer(t, claudeReply(t, map[string]any{
		"stop_reason":  "refusal",
		"stop_details": map[string]any{"type": "refusal", "category": "general_harms"},
		"content":      []any{},
	}))
	story, err := Generate(claudeTestMessages(), GenerateConfig{APIKey: "test-key"})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if story.Title != "Messages with Alice" {
		t.Errorf("title = %q, want the local story title", story.Title)
	}
	if len(story.Chapters) == 0 {
		t.Error("local fallback story has no chapters")
	}
}

// Property: for any mix of thinking, text, and other blocks in a completed
// reply, text() returns exactly the text blocks' text, in order; it errors
// only when that is empty. A refusal or max_tokens stop always errors.
func TestClaudeMessageTextProperty(t *testing.T) {
	blockTypes := []string{"text", "thinking", "fallback"}
	stopReasons := []string{"end_turn", "stop_sequence", "refusal", "max_tokens"}

	prop := func(kinds []uint8, texts []string, stop uint8) bool {
		var content []any
		var want strings.Builder
		for i, k := range kinds {
			text := ""
			if i < len(texts) {
				text = texts[i]
			}
			switch blockTypes[int(k)%len(blockTypes)] {
			case "text":
				content = append(content, textBlock(text))
				want.WriteString(text)
			case "thinking":
				content = append(content, thinkingBlock(text))
			default:
				content = append(content, map[string]any{
					"type": "fallback",
					"from": map[string]any{"model": "a"},
					"to":   map[string]any{"model": "b"},
				})
			}
		}
		stopReason := stopReasons[int(stop)%len(stopReasons)]
		raw, err := json.Marshal(map[string]any{"stop_reason": stopReason, "content": content})
		if err != nil {
			return false
		}
		var msg claudeMessage
		if err := json.Unmarshal(raw, &msg); err != nil {
			return false
		}

		got, err := msg.text()
		switch {
		case stopReason == "refusal" || stopReason == "max_tokens":
			return err != nil && got == ""
		case want.Len() == 0:
			return err != nil
		default:
			return err == nil && got == want.String()
		}
	}
	if err := quick.Check(prop, &quick.Config{MaxCount: 2000}); err != nil {
		t.Error(err)
	}
}
