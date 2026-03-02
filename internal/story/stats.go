package story

import (
	"math"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/maxghenis/openmessage/internal/db"
)

// Stats holds computed statistics for a conversation or set of messages.
type Stats struct {
	TotalMessages int          `json:"total_messages"`
	DateRange     DateRange    `json:"date_range"`
	Yearly        []YearStats  `json:"yearly"`
	HourHeatmap   []HourCount  `json:"hour_heatmap"`
	TopPhrases    []PhraseCount `json:"top_phrases"`
	SenderSplit   map[string]int `json:"sender_split"`
	AvgResponseTimes map[string]int `json:"avg_response_times"` // sender -> avg minutes
	LongestGap    Gap          `json:"longest_gap"`
}

type DateRange struct {
	Start string `json:"start"` // YYYY-MM-DD
	End   string `json:"end"`
}

type YearStats struct {
	Year   string `json:"year"`
	Total  int    `json:"total"`
	Months [12]int `json:"months"`
	BySender map[string]int `json:"by_sender"`
}

type HourCount struct {
	Hour  int `json:"hour"`
	Day   int `json:"day"`   // 0=Sunday
	Count int `json:"count"`
}

type PhraseCount struct {
	Phrase   string         `json:"phrase"`
	Count    int            `json:"count"`
	BySender map[string]int `json:"by_sender"`
}

type Gap struct {
	Start string `json:"start"` // YYYY-MM-DD
	End   string `json:"end"`
	Days  int    `json:"days"`
}

// ComputeStats calculates conversation statistics from a list of messages.
// Messages should be sorted by timestamp ascending for correct gap/response calculations.
func ComputeStats(messages []*db.Message, tz *time.Location) *Stats {
	if tz == nil {
		tz = time.UTC
	}

	if len(messages) == 0 {
		return &Stats{
			SenderSplit:      map[string]int{},
			AvgResponseTimes: map[string]int{},
		}
	}

	// Sort by timestamp ascending
	sorted := make([]*db.Message, len(messages))
	copy(sorted, messages)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].TimestampMS < sorted[j].TimestampMS
	})

	stats := &Stats{
		TotalMessages: len(sorted),
		SenderSplit:   map[string]int{},
		AvgResponseTimes: map[string]int{},
	}

	// Date range
	stats.DateRange = DateRange{
		Start: time.UnixMilli(sorted[0].TimestampMS).In(tz).Format("2006-01-02"),
		End:   time.UnixMilli(sorted[len(sorted)-1].TimestampMS).In(tz).Format("2006-01-02"),
	}

	// Group by year
	yearMap := map[string]*YearStats{}
	hourMap := map[[2]int]int{} // [hour, dayOfWeek] -> count
	phraseMap := map[string]int{}
	phraseBySender := map[string]map[string]int{} // phrase -> sender -> count

	// Response time tracking
	responseTotal := map[string]float64{}
	responseCount := map[string]int{}
	var longestGap struct {
		start, end int64
		days       float64
	}

	var prevSender string
	var prevTS int64

	for _, m := range sorted {
		t := time.UnixMilli(m.TimestampMS).In(tz)
		year := t.Format("2006")
		month := int(t.Month()) - 1
		hour := t.Hour()
		dow := int(t.Weekday())

		sender := senderKey(m)
		stats.SenderSplit[sender]++

		// Year stats
		ys, ok := yearMap[year]
		if !ok {
			ys = &YearStats{Year: year, BySender: map[string]int{}}
			yearMap[year] = ys
		}
		ys.Total++
		ys.Months[month]++
		ys.BySender[sender]++

		// Hour heatmap
		hourMap[[2]int{hour, dow}]++

		// Phrase counting (with per-sender tracking)
		countPhrases(m.Body, phraseMap, phraseBySender, sender)

		// Response times and gaps
		if prevTS > 0 {
			diffMin := float64(m.TimestampMS-prevTS) / 60000
			gapDays := float64(m.TimestampMS-prevTS) / 86400000

			// Response time (only count if sender changed and within 24h)
			if prevSender != "" && prevSender != sender && diffMin < 1440 {
				responseTotal[sender] += diffMin
				responseCount[sender]++
			}

			// Longest gap
			if gapDays > longestGap.days {
				longestGap.start = prevTS
				longestGap.end = m.TimestampMS
				longestGap.days = gapDays
			}
		}

		prevSender = sender
		prevTS = m.TimestampMS
	}

	// Build yearly array sorted
	var years []string
	for y := range yearMap {
		years = append(years, y)
	}
	sort.Strings(years)
	for _, y := range years {
		stats.Yearly = append(stats.Yearly, *yearMap[y])
	}

	// Build hour heatmap
	for h := 0; h < 24; h++ {
		for d := 0; d < 7; d++ {
			stats.HourHeatmap = append(stats.HourHeatmap, HourCount{
				Hour:  h,
				Day:   d,
				Count: hourMap[[2]int{h, d}],
			})
		}
	}

	// Top phrases (filter stop words)
	stats.TopPhrases = topPhrases(phraseMap, phraseBySender, 100)

	// Average response times
	for sender, total := range responseTotal {
		if count := responseCount[sender]; count > 0 {
			stats.AvgResponseTimes[sender] = int(math.Round(total / float64(count)))
		}
	}

	// Longest gap
	if longestGap.days > 0 {
		stats.LongestGap = Gap{
			Start: time.UnixMilli(longestGap.start).In(tz).Format("2006-01-02"),
			End:   time.UnixMilli(longestGap.end).In(tz).Format("2006-01-02"),
			Days:  int(math.Round(longestGap.days)),
		}
	}

	return stats
}

func senderKey(m *db.Message) string {
	if m.IsFromMe {
		return "me"
	}
	if m.SenderName != "" {
		return m.SenderName
	}
	if m.SenderNumber != "" {
		return m.SenderNumber
	}
	return "unknown"
}

var wordRe = regexp.MustCompile(`[^\w\s]`)

func countPhrases(text string, phraseMap map[string]int, phraseBySender map[string]map[string]int, sender string) {
	cleaned := wordRe.ReplaceAllString(strings.ToLower(text), "")
	words := strings.Fields(cleaned)
	// Filter short words
	var filtered []string
	for _, w := range words {
		if len([]rune(w)) > 1 {
			filtered = append(filtered, w)
		}
	}

	addPhrase := func(phrase string) {
		phraseMap[phrase]++
		if phraseBySender[phrase] == nil {
			phraseBySender[phrase] = map[string]int{}
		}
		phraseBySender[phrase][sender]++
	}

	// Bigrams
	for i := 0; i < len(filtered)-1; i++ {
		phrase := filtered[i] + " " + filtered[i+1]
		addPhrase(phrase)
	}
	// Single words > 3 chars
	for _, w := range filtered {
		if len([]rune(w)) > 3 && !isStopWord(w) {
			addPhrase(w)
		}
	}
}

func topPhrases(phraseMap map[string]int, phraseBySender map[string]map[string]int, n int) []PhraseCount {
	type kv struct {
		phrase string
		count  int
	}
	var items []kv
	for phrase, count := range phraseMap {
		words := strings.Fields(phrase)
		skip := false
		for _, w := range words {
			if isStopWord(w) {
				skip = true
				break
			}
		}
		if skip {
			continue
		}
		// Skip phrases that are just numbers or very short
		allDigit := true
		for _, r := range phrase {
			if !unicode.IsDigit(r) && r != ' ' {
				allDigit = false
				break
			}
		}
		if allDigit {
			continue
		}
		items = append(items, kv{phrase, count})
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].count > items[j].count
	})
	if len(items) > n {
		items = items[:n]
	}
	result := make([]PhraseCount, len(items))
	for i, kv := range items {
		bySender := phraseBySender[kv.phrase]
		if bySender == nil {
			bySender = map[string]int{}
		}
		result[i] = PhraseCount{Phrase: kv.phrase, Count: kv.count, BySender: bySender}
	}
	return result
}

var stopWords = map[string]bool{
	"the": true, "and": true, "that": true, "this": true, "with": true,
	"have": true, "from": true, "they": true, "will": true, "been": true,
	"what": true, "when": true, "your": true, "about": true, "would": true,
	"there": true, "their": true, "which": true, "could": true, "other": true,
	"were": true, "more": true, "some": true, "than": true, "them": true,
	"each": true, "make": true, "like": true, "just": true, "know": true,
	"dont": true, "think": true, "also": true, "yeah": true, "okay": true,
	"sure": true, "good": true, "going": true, "right": true, "well": true,
	"need": true, "want": true, "said": true, "come": true, "back": true,
	"time": true, "very": true, "much": true, "take": true, "look": true,
	"here": true, "still": true, "even": true,
}

func isStopWord(w string) bool {
	return stopWords[w]
}
