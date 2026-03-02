package viz

import (
	"strings"
	"testing"

	"github.com/maxghenis/openmessage/internal/story"
)

// minimalStats returns a small but valid Stats for testing.
func minimalStats() *story.Stats {
	return &story.Stats{
		TotalMessages: 142,
		DateRange: story.DateRange{
			Start: "2020-03-15",
			End:   "2025-12-01",
		},
		Yearly: []story.YearStats{
			{Year: "2020", Total: 40, BySender: map[string]int{"Max": 20, "Mary": 20}},
			{Year: "2021", Total: 50, BySender: map[string]int{"Max": 30, "Mary": 20}},
			{Year: "2022", Total: 52, BySender: map[string]int{"Max": 22, "Mary": 30}},
		},
		HourHeatmap: makeHeatmap(),
		TopPhrases: []story.PhraseCount{
			{Phrase: "love you", Count: 50, BySender: map[string]int{"Max": 30, "Mary": 20}},
			{Phrase: "good morning", Count: 35, BySender: map[string]int{"Max": 10, "Mary": 25}},
			{Phrase: "miss you", Count: 20, BySender: map[string]int{"Max": 12, "Mary": 8}},
		},
		SenderSplit: map[string]int{
			"Max":  72,
			"Mary": 70,
		},
		AvgResponseTimes: map[string]int{
			"Max":  8,
			"Mary": 12,
		},
		LongestGap: story.Gap{
			Start: "2021-06-15",
			End:   "2021-07-20",
			Days:  35,
		},
	}
}

// makeHeatmap builds a 24x7 heatmap with a few nonzero entries.
func makeHeatmap() []story.HourCount {
	hm := make([]story.HourCount, 0, 168)
	for h := 0; h < 24; h++ {
		for d := 0; d < 7; d++ {
			count := 0
			if h >= 9 && h <= 22 && d >= 1 && d <= 5 {
				count = h + d // make weekday daytime nonzero
			}
			hm = append(hm, story.HourCount{Hour: h, Day: d, Count: count})
		}
	}
	return hm
}

// minimalStory returns a small but valid Story for testing.
func minimalStory() *story.Story {
	return &story.Story{
		Title:   "Our story",
		Summary: "A love story told through texts.",
		Chapters: []story.Chapter{
			{
				Title:   "The beginning",
				Content: "They met in spring 2020.",
				Quotes: []story.Quote{
					{Sender: "Max", Text: "Hey, nice to meet you!", Timestamp: "2020-03-15T10:00:00Z"},
					{Sender: "Mary", Text: "You too! Let's chat more.", Timestamp: "2020-03-15T10:05:00Z"},
				},
				Period: "2020",
			},
			{
				Title:   "Growing closer",
				Content: "Through 2021 the conversation deepened.",
				Quotes: []story.Quote{
					{Sender: "Mary", Text: "I've been thinking about you.", Timestamp: "2021-05-10T20:00:00Z"},
				},
				Period: "2021",
			},
		},
	}
}

// minimalConfig returns a VizConfig with all fields populated.
func minimalConfig() VizConfig {
	return VizConfig{
		Person1:         "Max",
		Person2:         "Mary",
		PrimaryColor:    "#be123c",
		SecondaryColor:  "#d97706",
		AccentColor:     "#fbbf24",
		BackgroundColor: "#0c0a09",
		Timezone:        "America/New_York",
		PasswordHash:    "",
		Sections:        nil, // use default
	}
}

func TestRenderHTMLBasic(t *testing.T) {
	stats := minimalStats()
	st := minimalStory()
	cfg := minimalConfig()

	html, err := RenderHTML(stats, st, cfg)
	if err != nil {
		t.Fatalf("RenderHTML failed: %v", err)
	}
	if len(html) == 0 {
		t.Fatal("RenderHTML returned empty bytes")
	}

	s := string(html)

	// Must be valid HTML document
	if !strings.Contains(s, "<!DOCTYPE html>") {
		t.Error("missing <!DOCTYPE html>")
	}
	if !strings.Contains(s, "</html>") {
		t.Error("missing </html>")
	}

	// Must contain person names
	if !strings.Contains(s, "Max") {
		t.Error("missing person name Max")
	}
	if !strings.Contains(s, "Mary") {
		t.Error("missing person name Mary")
	}

	// Must contain Chart.js CDN
	if !strings.Contains(s, "cdn.jsdelivr.net/npm/chart.js") {
		t.Error("missing Chart.js CDN link")
	}

	// Must contain CSS variable definitions
	if !strings.Contains(s, "--primary") {
		t.Error("missing CSS variable --primary")
	}
	if !strings.Contains(s, "--secondary") {
		t.Error("missing CSS variable --secondary")
	}
	if !strings.Contains(s, "--accent") {
		t.Error("missing CSS variable --accent")
	}
	if !strings.Contains(s, "--bg") {
		t.Error("missing CSS variable --bg")
	}
	if !strings.Contains(s, "#be123c") {
		t.Error("missing primary color value")
	}

	// Must contain Google Fonts
	if !strings.Contains(s, "Cormorant Garamond") {
		t.Error("missing Cormorant Garamond font reference")
	}
	if !strings.Contains(s, "DM Sans") {
		t.Error("missing DM Sans font reference")
	}

	// Must contain the story title
	if !strings.Contains(s, "Our story") {
		t.Error("missing story title")
	}

	// Must contain message count
	if !strings.Contains(s, "142") {
		t.Error("missing total message count")
	}
}

func TestRenderHTMLPasswordGate(t *testing.T) {
	stats := minimalStats()
	st := minimalStory()
	cfg := minimalConfig()
	cfg.PasswordHash = "abc123def456"

	html, err := RenderHTML(stats, st, cfg)
	if err != nil {
		t.Fatalf("RenderHTML failed: %v", err)
	}

	s := string(html)

	// Must contain password gate elements
	if !strings.Contains(s, "password-gate") {
		t.Error("missing password-gate element")
	}
	if !strings.Contains(s, "abc123def456") {
		t.Error("missing password hash in JS")
	}
	// Must have SubtleCrypto / SHA-256 reference
	if !strings.Contains(s, "SHA-256") {
		t.Error("missing SHA-256 reference for password hashing")
	}
}

func TestRenderHTMLNoPassword(t *testing.T) {
	stats := minimalStats()
	st := minimalStory()
	cfg := minimalConfig()
	cfg.PasswordHash = ""

	html, err := RenderHTML(stats, st, cfg)
	if err != nil {
		t.Fatalf("RenderHTML failed: %v", err)
	}

	s := string(html)

	// Should NOT contain password gate overlay
	if strings.Contains(s, "password-overlay") {
		t.Error("password overlay should not be present when PasswordHash is empty")
	}
}

func TestRenderHTMLSections(t *testing.T) {
	stats := minimalStats()
	st := minimalStory()
	cfg := minimalConfig()

	html, err := RenderHTML(stats, st, cfg)
	if err != nil {
		t.Fatalf("RenderHTML failed: %v", err)
	}

	s := string(html)

	// All default sections should appear
	expectedSections := []string{
		"section-hero",
		"section-volume-chart",
		"section-sender-split",
		"section-heatmap",
		"section-phrases",
		"section-silence",
	}
	for _, sec := range expectedSections {
		if !strings.Contains(s, sec) {
			t.Errorf("missing section %q", sec)
		}
	}

	// Chapter content should appear
	if !strings.Contains(s, "The beginning") {
		t.Error("missing chapter title 'The beginning'")
	}
	if !strings.Contains(s, "Growing closer") {
		t.Error("missing chapter title 'Growing closer'")
	}

	// Chat bubble quotes should appear
	if !strings.Contains(s, "Hey, nice to meet you!") {
		t.Error("missing quote text")
	}
}

func TestRenderHTMLPhraseCloud(t *testing.T) {
	stats := minimalStats()
	st := minimalStory()
	cfg := minimalConfig()

	html, err := RenderHTML(stats, st, cfg)
	if err != nil {
		t.Fatalf("RenderHTML failed: %v", err)
	}

	s := string(html)

	// Phrases should appear in the HTML
	if !strings.Contains(s, "love you") {
		t.Error("missing phrase 'love you'")
	}
	if !strings.Contains(s, "good morning") {
		t.Error("missing phrase 'good morning'")
	}
	if !strings.Contains(s, "miss you") {
		t.Error("missing phrase 'miss you'")
	}
}

func TestRenderHTMLHeatmap(t *testing.T) {
	stats := minimalStats()
	st := minimalStory()
	cfg := minimalConfig()

	html, err := RenderHTML(stats, st, cfg)
	if err != nil {
		t.Fatalf("RenderHTML failed: %v", err)
	}

	s := string(html)

	// Heatmap grid should be present
	if !strings.Contains(s, "heatmap") {
		t.Error("missing heatmap section")
	}

	// Should contain day labels
	dayLabels := []string{"Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"}
	for _, d := range dayLabels {
		if !strings.Contains(s, d) {
			t.Errorf("missing day label %q", d)
		}
	}
}

func TestRenderHTMLResponseTimes(t *testing.T) {
	stats := minimalStats()
	st := minimalStory()
	cfg := minimalConfig()

	html, err := RenderHTML(stats, st, cfg)
	if err != nil {
		t.Fatalf("RenderHTML failed: %v", err)
	}

	s := string(html)

	// Response time values should appear
	if !strings.Contains(s, "8") && !strings.Contains(s, "8 min") {
		t.Error("missing Max's response time")
	}
	if !strings.Contains(s, "12") && !strings.Contains(s, "12 min") {
		t.Error("missing Mary's response time")
	}
}

func TestRenderHTMLLongestGap(t *testing.T) {
	stats := minimalStats()
	st := minimalStory()
	cfg := minimalConfig()

	html, err := RenderHTML(stats, st, cfg)
	if err != nil {
		t.Fatalf("RenderHTML failed: %v", err)
	}

	s := string(html)

	// Longest gap section
	if !strings.Contains(s, "35") {
		t.Error("missing longest gap days count (35)")
	}
	if !strings.Contains(s, "silence") || !strings.Contains(s, "gap") {
		t.Error("missing silence/gap reference")
	}
}

func TestRenderHTMLInterludes(t *testing.T) {
	stats := minimalStats()
	st := minimalStory()
	cfg := minimalConfig()
	cfg.Interludes = []Interlude{
		{Text: "Between the lines, love grew quietly.", Position: "hero"},
	}

	html, err := RenderHTML(stats, st, cfg)
	if err != nil {
		t.Fatalf("RenderHTML failed: %v", err)
	}

	s := string(html)

	if !strings.Contains(s, "Between the lines, love grew quietly.") {
		t.Error("missing interlude text")
	}
	if !strings.Contains(s, "interlude") {
		t.Error("missing interlude class/section")
	}
}

func TestRenderHTMLCustomSections(t *testing.T) {
	stats := minimalStats()
	st := minimalStory()
	cfg := minimalConfig()
	cfg.Sections = []string{"hero", "phrases", "closing"}

	html, err := RenderHTML(stats, st, cfg)
	if err != nil {
		t.Fatalf("RenderHTML failed: %v", err)
	}

	s := string(html)

	// Should have hero and phrases
	if !strings.Contains(s, "section-hero") {
		t.Error("missing hero section")
	}
	if !strings.Contains(s, "section-phrases") {
		t.Error("missing phrases section")
	}
}

func TestRenderHTMLGrainOverlay(t *testing.T) {
	stats := minimalStats()
	st := minimalStory()
	cfg := minimalConfig()

	html, err := RenderHTML(stats, st, cfg)
	if err != nil {
		t.Fatalf("RenderHTML failed: %v", err)
	}

	s := string(html)

	// Should have grain overlay CSS
	if !strings.Contains(s, "grain") {
		t.Error("missing grain overlay")
	}
}

func TestRenderHTMLScrollReveal(t *testing.T) {
	stats := minimalStats()
	st := minimalStory()
	cfg := minimalConfig()

	html, err := RenderHTML(stats, st, cfg)
	if err != nil {
		t.Fatalf("RenderHTML failed: %v", err)
	}

	s := string(html)

	// Should have IntersectionObserver-based scroll reveal
	if !strings.Contains(s, "IntersectionObserver") {
		t.Error("missing IntersectionObserver for scroll reveal")
	}
}
