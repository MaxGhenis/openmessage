package story

import (
	"fmt"
	"strings"
	"time"

	"github.com/maxghenis/openmessage/internal/db"
)

// RelationshipSummary builds a short, human-readable description of a
// relationship from message history, using only local stats (no API key). It is
// a few sentences covering volume, time span, who initiates, responsiveness,
// the longest quiet stretch, and common words.
func RelationshipSummary(messages []*db.Message, name string, tz *time.Location) string {
	if name == "" {
		name = "this person"
	}
	real := FilterRealMessages(messages)
	if len(real) == 0 {
		return "No communication with " + name + " yet."
	}
	if tz == nil {
		tz = time.UTC
	}
	stats := ComputeStats(real, tz)

	var sb strings.Builder
	span := ""
	if stats.DateRange.Start != "" && stats.DateRange.End != "" {
		if stats.DateRange.Start == stats.DateRange.End {
			span = " on " + stats.DateRange.Start
		} else {
			span = " between " + stats.DateRange.Start + " and " + stats.DateRange.End
		}
	}
	fmt.Fprintf(&sb, "You and %s have exchanged %s%s.", name, plural(stats.TotalMessages, "message"), span)

	mine := stats.SenderSplit["me"]
	theirs := 0
	for k, v := range stats.SenderSplit {
		if k != "me" {
			theirs += v
		}
	}
	if total := mine + theirs; total > 0 {
		switch {
		case mine > theirs:
			fmt.Fprintf(&sb, " You send a bit more — about %d%% of messages are from you.", pct(mine, total))
		case theirs > mine:
			fmt.Fprintf(&sb, " %s sends a bit more — about %d%% are from them.", name, pct(theirs, total))
		default:
			sb.WriteString(" It's an even back-and-forth.")
		}
	}

	if mins, ok := stats.AvgResponseTimes["me"]; ok && mins > 0 {
		fmt.Fprintf(&sb, " You usually reply within %s.", humanizeMinutes(mins))
	}

	if stats.LongestGap.Days >= 14 && stats.LongestGap.Start != "" {
		fmt.Fprintf(&sb, " Your longest quiet stretch was %d days (%s to %s).", stats.LongestGap.Days, stats.LongestGap.Start, stats.LongestGap.End)
	}

	if len(stats.TopPhrases) > 0 {
		words := make([]string, 0, 6)
		for i, p := range stats.TopPhrases {
			if i >= 6 {
				break
			}
			words = append(words, p.Phrase)
		}
		if len(words) > 0 {
			fmt.Fprintf(&sb, " Recurring words: %s.", strings.Join(words, ", "))
		}
	}

	return sb.String()
}

// FilterRealMessages drops messages that aren't actual person-to-person
// communication: RCS/system notifications ("RCS chat with Abby", "This RCS chat
// is now end-to-end encrypted") and contentless stubs (no body, no media). It's
// used so the count and summary reflect real conversation, not protocol noise.
func FilterRealMessages(messages []*db.Message) []*db.Message {
	out := make([]*db.Message, 0, len(messages))
	for _, m := range messages {
		if m == nil || isSystemMessage(m.Body) {
			continue
		}
		if strings.TrimSpace(m.Body) == "" && strings.TrimSpace(m.MediaID) == "" {
			continue
		}
		out = append(out, m)
	}
	return out
}

// isSystemMessage reports whether a body is an RCS/SMS system notification
// rather than something a person typed.
func isSystemMessage(body string) bool {
	b := strings.ToLower(strings.TrimSpace(body))
	if b == "" {
		return false
	}
	prefixes := []string{
		"rcs chat with",
		"sms/mms with",
		"you created this rcs",
		"you started an rcs",
		"this rcs chat is now",
		"this chat is now",
	}
	for _, p := range prefixes {
		if strings.HasPrefix(b, p) {
			return true
		}
	}
	// Catch-all for short RCS chat banners.
	if strings.Contains(b, "rcs chat") && len(b) < 60 {
		return true
	}
	return false
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

func pct(part, total int) int {
	if total == 0 {
		return 0
	}
	return int(float64(part)/float64(total)*100 + 0.5)
}

func humanizeMinutes(mins int) string {
	if mins < 60 {
		return plural(mins, "minute")
	}
	if mins < 60*24 {
		return plural(mins/60, "hour")
	}
	return plural(mins/(60*24), "day")
}
