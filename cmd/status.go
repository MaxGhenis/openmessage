package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/rs/zerolog"

	"github.com/maxghenis/openmessage/internal/app"
	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/freshness"
	"github.com/maxghenis/openmessage/internal/localapi"
)

// staleBehindDays is how many whole days a platform's latest message may
// trail the newest message overall before it is flagged "Nd behind", the
// relative rule /api/status freshness applies as behind_days.
const staleBehindDays = 3

// RunStatus handles "openmessage status [--json]".
//
// It reports stored message coverage per platform — counts and the latest
// sent/received timestamps — straight from the local store, without starting
// any live transports. Use it to spot a platform that has silently stopped
// syncing. Two rules flag one, as in /api/status freshness: its row trails the
// newest platform by days ("Nd behind"), or its transport has delivered
// nothing for longer than its own baseline explains ("silent Nh", the
// freshness.EvaluateSilence rule). The second rule also catches a stall of the
// newest platform, which the first never can.
func RunStatus(logger zerolog.Logger, args ...string) error {
	session, err := openCommandReadSource(logger, os.Stderr)
	if err != nil {
		return err
	}
	defer session.Close()
	return runStatus(context.Background(), session, statusDeps{
		daemon: localapi.NewClient("", localapi.LoadControlToken(session.DataDir)),
		now:    time.Now,
		loc:    time.Local,
		demo:   app.DemoMode(),
		output: os.Stdout,
	}, hasFlag(args, "--json"))
}

// statusDeps is what runStatus reads besides the store, so tests can pin the
// clock and stand in for the running app.
type statusDeps struct {
	// daemon probes the running app for its silence verdicts; nil judges
	// every platform locally.
	daemon *localapi.Client
	// now is the clock silence is judged at. The legacy activity source
	// still drops rows stamped more than 10 minutes past the wall clock
	// (freshness.NewMessageActivity), whatever now says.
	now func() time.Time
	// loc is the zone silence baselines are built in; the daemon uses
	// time.Local.
	loc *time.Location
	// source overrides the activity source local verdicts are measured on;
	// nil picks the session's own (commandReadSession.activitySource).
	source freshness.ActivitySource
	// demo skips silence: demo data is a frozen fixture.
	demo   bool
	output io.Writer
}

// statusRow is one stored platform's line in the status output.
type statusRow struct {
	db.PlatformStat
	BehindDays int
	// Silence is the verdict for the status platform the row's messages
	// arrive on (sms and rcs both arrive on google), nil without one.
	Silence *platformSilence
}

func runStatus(ctx context.Context, session *commandReadSession, deps statusDeps, asJSON bool) error {
	stats, err := session.Reads.PlatformStats()
	if err != nil {
		return fmt.Errorf("platform stats: %w", err)
	}

	now := deps.now()
	var total int
	var newestOverall int64
	for _, st := range stats {
		total += st.Count
		if st.LatestMS > newestOverall {
			newestOverall = st.LatestMS
		}
	}

	var platforms []string
	seen := map[string]bool{}
	for _, st := range stats {
		if key := freshnessPlatformByStorage[st.Platform]; key != "" && !seen[key] {
			seen[key] = true
			platforms = append(platforms, key)
		}
	}
	silence := judgeStatusSilence(ctx, session, deps, now, platforms)
	rows := make([]statusRow, 0, len(stats))
	for _, st := range stats {
		row := statusRow{PlatformStat: st, BehindDays: daysBetween(st.LatestMS, newestOverall)}
		if verdict, ok := silence.byPlatform[freshnessPlatformByStorage[st.Platform]]; ok {
			row.Silence = &verdict
		}
		rows = append(rows, row)
	}

	dbPath := session.StorePath
	out := deps.output
	// Only the v2 inbox can tell SMS from RCS; a legacy-mode session skips it.
	var smsPath *freshness.SMSPathReport
	if session.V2Store != nil && !deps.demo {
		smsPath = statusSMSPath(ctx, session.V2Store, now)
	}

	if asJSON {
		return writeStatusJSON(out, dbPath, session.DataDir, total, rows, silence.note, smsPath)
	}

	fmt.Fprintf(out, "OpenMessage store — %s\n", dbPath)
	if len(stats) == 0 {
		fmt.Fprintln(out, "\nNo messages stored yet. Pair and serve, or run an `openmessage import …`.")
		// The inbox can hold frames that never became stored messages; --json
		// reports their verdict too.
		if smsPath != nil {
			fmt.Fprintln(out, smsPathStatusLine(*smsPath))
		}
		return nil
	}
	fmt.Fprintln(out)

	w := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "PLATFORM\tMESSAGES\tLATEST\tLAST RECEIVED\tAGE")
	for _, row := range rows {
		age := humanAge(row.LatestMS, now) + staleWarning(row.BehindDays, row.Silence)
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			row.Platform, commaInt(row.Count), fmtTS(row.LatestMS), fmtTS(row.LatestRecvMS), age)
	}
	w.Flush()

	fmt.Fprintf(out, "\nTotal: %s messages across %d platform(s).\n", commaInt(total), len(stats))
	if newestOverall > 0 {
		fmt.Fprintf(out, "Newest message overall: %s (%s).\n", fmtTS(newestOverall), humanAge(newestOverall, now))
	}
	if silence.note != "" {
		fmt.Fprintln(out, silence.note)
	}
	if smsPath != nil {
		fmt.Fprintln(out, smsPathStatusLine(*smsPath))
	}
	return nil
}

// staleWarning is the AGE-column suffix for a stale platform: the days it
// trails the newest platform or, failing that, how many whole hours its
// transport has been silent past its baseline. Behind outranks silent, as in
// /api/status stale_reason: a platform days behind is usually logged out or
// unpaired, while a silent newest platform usually means the phone stopped
// relaying. A stalled verdict without a usable silent_ms gets no hours.
func staleWarning(behindDays int, silence *platformSilence) string {
	switch {
	case behindDays >= staleBehindDays:
		return fmt.Sprintf("  ⚠ %dd behind", behindDays)
	case silence != nil && silence.Stalled && silence.SilentMS > 0:
		return fmt.Sprintf("  ⚠ silent %dh", silence.SilentMS/time.Hour.Milliseconds())
	case silence != nil && silence.Stalled:
		return "  ⚠ silent"
	default:
		return ""
	}
}

func writeStatusJSON(
	out io.Writer,
	dbPath, dataDir string,
	total int,
	rows []statusRow,
	silenceNote string,
	smsPath *freshness.SMSPathReport,
) error {
	type platformJSON struct {
		Platform         string `json:"platform"`
		Count            int    `json:"count"`
		LatestMS         int64  `json:"latest_ms"`
		LatestReceivedMS int64  `json:"latest_received_ms"`
		Latest           string `json:"latest,omitempty"`
		LatestReceived   string `json:"latest_received,omitempty"`
		BehindDays       int    `json:"behind_days"`
		// Silence is the freshness.<platform>.silence object of /api/status,
		// the daemon's own when SilenceJudgedBy is "daemon".
		Silence         json.RawMessage `json:"silence,omitempty"`
		SilenceJudgedBy string          `json:"silence_judged_by,omitempty"`
	}
	payload := struct {
		DataDir     string                   `json:"data_dir"`
		DBPath      string                   `json:"db_path"`
		Total       int                      `json:"total_messages"`
		Platforms   []platformJSON           `json:"platforms"`
		SilenceNote string                   `json:"silence_note,omitempty"`
		SMSPath     *freshness.SMSPathReport `json:"google_sms_path,omitempty"`
	}{DataDir: dataDir, DBPath: dbPath, Total: total, SilenceNote: silenceNote, SMSPath: smsPath}

	for _, row := range rows {
		pj := platformJSON{
			Platform: row.Platform, Count: row.Count,
			LatestMS: row.LatestMS, LatestReceivedMS: row.LatestRecvMS,
			BehindDays: row.BehindDays,
		}
		if row.LatestMS > 0 {
			pj.Latest = time.UnixMilli(row.LatestMS).Format(time.RFC3339)
		}
		if row.LatestRecvMS > 0 {
			pj.LatestReceived = time.UnixMilli(row.LatestRecvMS).Format(time.RFC3339)
		}
		if row.Silence != nil {
			pj.Silence = row.Silence.Block
			pj.SilenceJudgedBy = row.Silence.JudgedBy
		}
		payload.Platforms = append(payload.Platforms, pj)
	}

	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(payload)
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
