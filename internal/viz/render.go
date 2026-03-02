package viz

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"time"

	"github.com/maxghenis/openmessage/internal/story"
)

// templateData holds all data passed to the HTML template.
type templateData struct {
	Config         VizConfig
	Stats          *story.Stats
	Story          *story.Story
	Sections       []section
	MessagesPerDay float64
	DaysSpan       int
	// Pre-serialized JSON for Chart.js
	MonthlyLabelsJSON  string
	MonthlySeries1JSON string
	MonthlySeries2JSON string
	SenderLabelsJSON   string
	SenderValuesJSON   string
	HeatmapJSON        string
}

// section represents a renderable section of the visualization.
type section struct {
	Type string // "hero", "chapter", "volume-chart", "sender-split", "heatmap", "phrases", "silence", "photos", "closing", "interlude", "timeline-nav"
	Data any    // chapter data, interlude text, etc.
}

// heatmapCell is a pre-computed cell for the heatmap grid.
type heatmapCell struct {
	Hour    int
	Day     int
	Count   int
	Opacity float64
}

// RenderHTML generates a self-contained HTML file from stats, story, and config.
func RenderHTML(stats *story.Stats, st *story.Story, config VizConfig) ([]byte, error) {
	data := buildTemplateData(stats, st, config)

	var buf bytes.Buffer
	tmpl, err := buildTemplate()
	if err != nil {
		return nil, fmt.Errorf("build template: %w", err)
	}
	if err := tmpl.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("execute template: %w", err)
	}
	return buf.Bytes(), nil
}

// buildTemplateData assembles the templateData struct from stats, story, and config.
func buildTemplateData(stats *story.Stats, st *story.Story, config VizConfig) templateData {
	// Apply defaults
	if config.PrimaryColor == "" {
		config.PrimaryColor = "#be123c"
	}
	if config.SecondaryColor == "" {
		config.SecondaryColor = "#d97706"
	}
	if config.AccentColor == "" {
		config.AccentColor = "#fbbf24"
	}
	if config.BackgroundColor == "" {
		config.BackgroundColor = "#0c0a09"
	}

	// Compute days span and messages per day
	daysSpan := 1
	msgsPerDay := float64(stats.TotalMessages)
	if stats.DateRange.Start != "" && stats.DateRange.End != "" {
		startT, err1 := time.Parse("2006-01-02", stats.DateRange.Start)
		endT, err2 := time.Parse("2006-01-02", stats.DateRange.End)
		if err1 == nil && err2 == nil {
			d := int(endT.Sub(startT).Hours()/24) + 1
			if d > 0 {
				daysSpan = d
				msgsPerDay = float64(stats.TotalMessages) / float64(d)
			}
		}
	}

	// Build sections list
	sectionOrder := config.Sections
	if len(sectionOrder) == 0 {
		sectionOrder = DefaultSections()
	}

	chapterIdx := 0

	knownSections := map[string]bool{
		"hero": true, "timeline-nav": true, "volume-chart": true,
		"sender-split": true, "heatmap": true, "phrases": true,
		"silence": true, "photos": true, "closing": true,
	}
	chapterSlotSet := map[string]bool{
		"chapter-early": true, "chapter-middle": true, "chapter-late": true,
	}

	var sections []section
	for _, name := range sectionOrder {
		switch {
		case name == "password-gate":
			// Handled separately in template
			continue
		case knownSections[name]:
			sections = append(sections, section{Type: name, Data: nil})
		case chapterSlotSet[name]:
			if st != nil && chapterIdx < len(st.Chapters) {
				sections = append(sections, section{Type: "chapter", Data: st.Chapters[chapterIdx]})
				chapterIdx++
			}
		}

		// Append interludes that go after this section
		for _, interlude := range config.Interludes {
			if interlude.Position == name {
				sections = append(sections, section{Type: "interlude", Data: interlude.Text})
			}
		}
	}

	// If there are remaining chapters, append them
	if st != nil {
		for chapterIdx < len(st.Chapters) {
			sections = append(sections, section{Type: "chapter", Data: st.Chapters[chapterIdx]})
			chapterIdx++
		}
	}

	// Pre-serialize chart data as JSON for Chart.js
	monthlyLabels, series1, series2 := buildMonthlyChartData(stats, config)
	monthlyLabelsJSON, _ := json.Marshal(monthlyLabels)
	series1JSON, _ := json.Marshal(series1)
	series2JSON, _ := json.Marshal(series2)

	senderLabels, senderValues := buildSenderSplitData(stats)
	senderLabelsJSON, _ := json.Marshal(senderLabels)
	senderValuesJSON, _ := json.Marshal(senderValues)

	heatmapData := buildHeatmapData(stats)
	heatmapJSON, _ := json.Marshal(heatmapData)

	return templateData{
		Config:             config,
		Stats:              stats,
		Story:              st,
		Sections:           sections,
		MessagesPerDay:     math.Round(msgsPerDay*10) / 10,
		DaysSpan:           daysSpan,
		MonthlyLabelsJSON:  string(monthlyLabelsJSON),
		MonthlySeries1JSON: string(series1JSON),
		MonthlySeries2JSON: string(series2JSON),
		SenderLabelsJSON:   string(senderLabelsJSON),
		SenderValuesJSON:   string(senderValuesJSON),
		HeatmapJSON:        string(heatmapJSON),
	}
}

// buildMonthlyChartData creates month labels and per-sender series for the stacked bar chart.
func buildMonthlyChartData(stats *story.Stats, config VizConfig) (labels []string, series1 []int, series2 []int) {
	monthNames := []string{"Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"}

	for _, ys := range stats.Yearly {
		for m := 0; m < 12; m++ {
			labels = append(labels, fmt.Sprintf("%s %s", monthNames[m], ys.Year))

			p1Count, p2Count := splitBySender(ys.BySender, config.Person1)
			series1 = append(series1, p1Count)
			series2 = append(series2, p2Count)
		}
	}
	return
}

// splitBySender splits a sender count map into person1 vs everyone else.
// If person1's name does not match any key, falls back to "me" as person1.
func splitBySender(bySender map[string]int, person1 string) (p1Count, p2Count int) {
	if bySender == nil {
		return 0, 0
	}

	p1Count = bySender[person1]
	for sender, count := range bySender {
		if sender != person1 {
			p2Count += count
		}
	}

	// Fall back to "me" mapping if person1's name didn't match any key
	if p1Count == 0 && p2Count == 0 {
		for sender, count := range bySender {
			if sender == "me" {
				p1Count += count
			} else {
				p2Count += count
			}
		}
	}
	return
}

// buildSenderSplitData creates labels and values for the donut chart.
func buildSenderSplitData(stats *story.Stats) (labels []string, values []int) {
	for sender, count := range stats.SenderSplit {
		labels = append(labels, sender)
		values = append(values, count)
	}
	return
}

// buildHeatmapData converts the heatmap into a grid with pre-computed opacities.
func buildHeatmapData(stats *story.Stats) []heatmapCell {
	maxCount := 0
	for _, hc := range stats.HourHeatmap {
		if hc.Count > maxCount {
			maxCount = hc.Count
		}
	}

	cells := make([]heatmapCell, len(stats.HourHeatmap))
	for i, hc := range stats.HourHeatmap {
		opacity := 0.0
		if maxCount > 0 && hc.Count > 0 {
			opacity = 0.1 + 0.9*float64(hc.Count)/float64(maxCount)
		}
		cells[i] = heatmapCell{
			Hour:    hc.Hour,
			Day:     hc.Day,
			Count:   hc.Count,
			Opacity: math.Round(opacity*100) / 100,
		}
	}
	return cells
}
