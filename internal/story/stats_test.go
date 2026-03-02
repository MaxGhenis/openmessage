package story

import (
	"testing"
	"time"

	"github.com/maxghenis/openmessage/internal/db"
)

func TestComputeStatsEmpty(t *testing.T) {
	stats := ComputeStats(nil, nil)
	if stats.TotalMessages != 0 {
		t.Errorf("total = %d, want 0", stats.TotalMessages)
	}
}

func TestComputeStats(t *testing.T) {
	messages := []*db.Message{
		{MessageID: "1", SenderName: "Alice", Body: "Hello there!", TimestampMS: 1700000000000, IsFromMe: false},
		{MessageID: "2", SenderName: "", Body: "Hi Alice!", TimestampMS: 1700000060000, IsFromMe: true},
		{MessageID: "3", SenderName: "Alice", Body: "How are you doing today?", TimestampMS: 1700000120000, IsFromMe: false},
		{MessageID: "4", SenderName: "", Body: "Great, thanks!", TimestampMS: 1700000180000, IsFromMe: true},
		{MessageID: "5", SenderName: "Alice", Body: "Want to grab coffee?", TimestampMS: 1700000240000, IsFromMe: false},
	}

	stats := ComputeStats(messages, nil)

	if stats.TotalMessages != 5 {
		t.Errorf("total = %d, want 5", stats.TotalMessages)
	}

	if stats.SenderSplit["Alice"] != 3 {
		t.Errorf("Alice count = %d, want 3", stats.SenderSplit["Alice"])
	}
	if stats.SenderSplit["me"] != 2 {
		t.Errorf("me count = %d, want 2", stats.SenderSplit["me"])
	}

	if len(stats.Yearly) != 1 {
		t.Fatalf("yearly count = %d, want 1", len(stats.Yearly))
	}
	if stats.Yearly[0].Year != "2023" {
		t.Errorf("year = %s, want 2023", stats.Yearly[0].Year)
	}

	// Hour heatmap should have 24*7 = 168 entries
	if len(stats.HourHeatmap) != 168 {
		t.Errorf("heatmap entries = %d, want 168", len(stats.HourHeatmap))
	}

	// Longest gap should be computed
	if stats.DateRange.Start == "" || stats.DateRange.End == "" {
		t.Error("date range should be set")
	}
}

func TestComputeStatsResponseTimes(t *testing.T) {
	// Alice sends, then "me" responds 5 minutes later
	messages := []*db.Message{
		{MessageID: "1", SenderName: "Alice", Body: "Hello", TimestampMS: 1700000000000, IsFromMe: false},
		{MessageID: "2", Body: "Hi", TimestampMS: 1700000300000, IsFromMe: true}, // 5 min later
		{MessageID: "3", SenderName: "Alice", Body: "What's up", TimestampMS: 1700000600000, IsFromMe: false}, // 5 min later
	}

	stats := ComputeStats(messages, nil)

	// "me" responded to Alice in 5 minutes
	if rt, ok := stats.AvgResponseTimes["me"]; !ok || rt != 5 {
		t.Errorf("me avg response = %d, want 5", rt)
	}
	// Alice responded to me in 5 minutes
	if rt, ok := stats.AvgResponseTimes["Alice"]; !ok || rt != 5 {
		t.Errorf("Alice avg response = %d, want 5", rt)
	}
}

func TestComputeStatsLongestGap(t *testing.T) {
	messages := []*db.Message{
		{MessageID: "1", Body: "Hello", TimestampMS: 1700000000000, IsFromMe: true},
		// 10 day gap
		{MessageID: "2", Body: "Hi again", TimestampMS: 1700864000000, IsFromMe: true},
	}

	stats := ComputeStats(messages, nil)

	if stats.LongestGap.Days != 10 {
		t.Errorf("longest gap = %d days, want 10", stats.LongestGap.Days)
	}
}

func TestPhraseCountBySender(t *testing.T) {
	// Alice says "really great" twice, "me" says "really great" once
	// "really" alone (>3 chars, not stop word) should also appear per-sender
	messages := []*db.Message{
		{MessageID: "1", SenderName: "Alice", Body: "This is really great stuff", TimestampMS: 1700000000000, IsFromMe: false},
		{MessageID: "2", Body: "I think really great work here", TimestampMS: 1700000060000, IsFromMe: true},
		{MessageID: "3", SenderName: "Alice", Body: "Yes really great results overall", TimestampMS: 1700000120000, IsFromMe: false},
	}

	stats := ComputeStats(messages, nil)

	// Find "really great" in top phrases
	var found *PhraseCount
	for i := range stats.TopPhrases {
		if stats.TopPhrases[i].Phrase == "really great" {
			found = &stats.TopPhrases[i]
			break
		}
	}
	if found == nil {
		t.Fatal("expected 'really great' in top phrases")
	}
	if found.Count != 3 {
		t.Errorf("'really great' total count = %d, want 3", found.Count)
	}
	if found.BySender == nil {
		t.Fatal("BySender should not be nil")
	}
	if found.BySender["Alice"] != 2 {
		t.Errorf("'really great' Alice count = %d, want 2", found.BySender["Alice"])
	}
	if found.BySender["me"] != 1 {
		t.Errorf("'really great' me count = %d, want 1", found.BySender["me"])
	}
}

func TestComputeStatsTimezone(t *testing.T) {
	// Create a message at a known UTC time: 2023-11-14 03:00:00 UTC
	// That's 2023-11-13 22:00:00 EST (UTC-5, November is standard time)
	// TimestampMS for 2023-11-14 03:00 UTC = 1699930800000
	utcTime := time.Date(2023, 11, 14, 3, 0, 0, 0, time.UTC)
	tsMS := utcTime.UnixMilli()

	messages := []*db.Message{
		{MessageID: "1", SenderName: "Alice", Body: "Late night message", TimestampMS: tsMS, IsFromMe: false},
	}

	nyTZ, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("failed to load timezone: %v", err)
	}

	stats := ComputeStats(messages, nyTZ)

	// In UTC: hour=3, day=Tuesday (2023-11-14 is a Tuesday)
	// In EST: hour=22, day=Monday (2023-11-13 is a Monday)
	// dayOfWeek: Monday=1

	// Verify heatmap has the count in the EST bucket
	var utcBucket, estBucket HourCount
	for _, hc := range stats.HourHeatmap {
		if hc.Hour == 3 && hc.Day == 2 { // UTC: hour 3, Tuesday
			utcBucket = hc
		}
		if hc.Hour == 22 && hc.Day == 1 { // EST: hour 22, Monday
			estBucket = hc
		}
	}
	if estBucket.Count != 1 {
		t.Errorf("EST bucket (hour=22, day=Monday) count = %d, want 1", estBucket.Count)
	}
	if utcBucket.Count != 0 {
		t.Errorf("UTC bucket (hour=3, day=Tuesday) count = %d, want 0 (should be shifted to EST)", utcBucket.Count)
	}

	// Verify date range is formatted in EST
	// 2023-11-14 03:00 UTC = 2023-11-13 in EST
	if stats.DateRange.Start != "2023-11-13" {
		t.Errorf("DateRange.Start = %s, want 2023-11-13 (EST date)", stats.DateRange.Start)
	}
}

func TestComputeStatsTimezoneNil(t *testing.T) {
	// Same message as above: 2023-11-14 03:00:00 UTC
	utcTime := time.Date(2023, 11, 14, 3, 0, 0, 0, time.UTC)
	tsMS := utcTime.UnixMilli()

	messages := []*db.Message{
		{MessageID: "1", SenderName: "Alice", Body: "A test message", TimestampMS: tsMS, IsFromMe: false},
	}

	stats := ComputeStats(messages, nil)

	// With nil timezone, should use UTC
	// UTC: hour=3, day=Tuesday (dayOfWeek=2)
	var utcBucket HourCount
	for _, hc := range stats.HourHeatmap {
		if hc.Hour == 3 && hc.Day == 2 { // UTC: hour 3, Tuesday
			utcBucket = hc
		}
	}
	if utcBucket.Count != 1 {
		t.Errorf("UTC bucket (hour=3, day=Tuesday) count = %d, want 1", utcBucket.Count)
	}

	// Date should be in UTC: 2023-11-14
	if stats.DateRange.Start != "2023-11-14" {
		t.Errorf("DateRange.Start = %s, want 2023-11-14 (UTC date)", stats.DateRange.Start)
	}
}

func TestGenerateLocalStory(t *testing.T) {
	messages := []*db.Message{
		{MessageID: "1", SenderName: "Alice", Body: "First message ever", TimestampMS: 1672531200000, IsFromMe: false},
		{MessageID: "2", Body: "Replying to you!", TimestampMS: 1672531260000, IsFromMe: true},
		{MessageID: "3", SenderName: "Alice", Body: "A year later message", TimestampMS: 1704067200000, IsFromMe: false},
	}

	story, err := Generate(messages, GenerateConfig{})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	if story.Title == "" {
		t.Error("title should not be empty")
	}
	if story.Stats == nil {
		t.Fatal("stats should not be nil")
	}
	if story.Stats.TotalMessages != 3 {
		t.Errorf("total = %d, want 3", story.Stats.TotalMessages)
	}
	if len(story.Chapters) == 0 {
		t.Error("should have at least one chapter")
	}
}
