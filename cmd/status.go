package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/rs/zerolog"

	"github.com/maxghenis/openmessage/internal/app"
	"github.com/maxghenis/openmessage/internal/db"
)

// RunStatus handles "openmessage status [--json]".
//
// It reports stored message coverage per platform — counts and the latest
// sent/received timestamps — straight from the local store, without starting
// any live transports. Use it to spot a platform that has silently stopped
// syncing: its row falls behind the others and is flagged "Nd behind".
func RunStatus(logger zerolog.Logger, args ...string) error {
	asJSON := hasFlag(args, "--json")

	a, err := app.New(logger)
	if err != nil {
		return fmt.Errorf("init app: %w", err)
	}
	defer a.Close()

	stats, err := a.Store.PlatformStats()
	if err != nil {
		return fmt.Errorf("platform stats: %w", err)
	}

	now := time.Now()
	var total int
	var newestOverall int64
	for _, st := range stats {
		total += st.Count
		if st.LatestMS > newestOverall {
			newestOverall = st.LatestMS
		}
	}

	dbPath := filepath.Join(a.DataDir, "messages.db")

	if asJSON {
		return writeStatusJSON(dbPath, a.DataDir, total, stats)
	}

	fmt.Printf("OpenMessage store — %s\n", dbPath)
	if len(stats) == 0 {
		fmt.Println("\nNo messages stored yet. Pair and serve, or run an `openmessage import …`.")
		return nil
	}
	fmt.Println()

	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "PLATFORM\tMESSAGES\tLATEST\tLAST RECEIVED\tAGE")
	for _, st := range stats {
		age := humanAge(st.LatestMS, now)
		if behind := daysBetween(st.LatestMS, newestOverall); st.LatestMS > 0 && behind >= 3 {
			age += fmt.Sprintf("  ⚠ %dd behind", behind)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			st.Platform, commaInt(st.Count), fmtTS(st.LatestMS), fmtTS(st.LatestRecvMS), age)
	}
	w.Flush()

	fmt.Printf("\nTotal: %s messages across %d platform(s).\n", commaInt(total), len(stats))
	if newestOverall > 0 {
		fmt.Printf("Newest message overall: %s (%s).\n", fmtTS(newestOverall), humanAge(newestOverall, now))
	}
	return nil
}

func writeStatusJSON(dbPath, dataDir string, total int, stats []db.PlatformStat) error {
	type platformJSON struct {
		Platform         string `json:"platform"`
		Count            int    `json:"count"`
		LatestMS         int64  `json:"latest_ms"`
		LatestReceivedMS int64  `json:"latest_received_ms"`
		Latest           string `json:"latest,omitempty"`
		LatestReceived   string `json:"latest_received,omitempty"`
	}
	out := struct {
		DataDir   string         `json:"data_dir"`
		DBPath    string         `json:"db_path"`
		Total     int            `json:"total_messages"`
		Platforms []platformJSON `json:"platforms"`
	}{DataDir: dataDir, DBPath: dbPath, Total: total}

	for _, st := range stats {
		pj := platformJSON{
			Platform: st.Platform, Count: st.Count,
			LatestMS: st.LatestMS, LatestReceivedMS: st.LatestRecvMS,
		}
		if st.LatestMS > 0 {
			pj.Latest = time.UnixMilli(st.LatestMS).Format(time.RFC3339)
		}
		if st.LatestRecvMS > 0 {
			pj.LatestReceived = time.UnixMilli(st.LatestRecvMS).Format(time.RFC3339)
		}
		out.Platforms = append(out.Platforms, pj)
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

// fmtTS renders a millisecond timestamp in local time, or an em dash if zero.
func fmtTS(ms int64) string {
	if ms == 0 {
		return "—"
	}
	return time.UnixMilli(ms).Format("2006-01-02 15:04")
}

// humanAge renders how long ago a timestamp was, relative to now.
func humanAge(ms int64, now time.Time) string {
	if ms == 0 {
		return "—"
	}
	d := now.Sub(time.UnixMilli(ms))
	if d < 0 {
		d = 0
	}
	switch days := int(d.Hours() / 24); {
	case days <= 0:
		return "today"
	case days == 1:
		return "1d ago"
	default:
		return fmt.Sprintf("%dd ago", days)
	}
}

// daysBetween returns how many whole days `older` trails `newer` (0 if not behind).
func daysBetween(older, newer int64) int {
	if older == 0 || newer <= older {
		return 0
	}
	return int(time.UnixMilli(newer).Sub(time.UnixMilli(older)).Hours() / 24)
}

// commaInt formats an integer with thousands separators (e.g. 506664 → "506,664").
func commaInt(n int) string {
	s := strconv.Itoa(n)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var b strings.Builder
	pre := len(s) % 3
	if pre > 0 {
		b.WriteString(s[:pre])
		if len(s) > pre {
			b.WriteByte(',')
		}
	}
	for i := pre; i < len(s); i += 3 {
		b.WriteString(s[i : i+3])
		if i+3 < len(s) {
			b.WriteByte(',')
		}
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}
