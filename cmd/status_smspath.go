package cmd

import (
	"context"
	"fmt"
	"time"

	"github.com/maxghenis/openmessage/internal/freshness"
	"github.com/maxghenis/openmessage/internal/ingest"
)

// statusSMSPath judges whether carrier SMS has stopped reaching the phone while
// RCS flows, from the v2 inbox: the only store that keeps the protobuf type
// telling SMS from RCS. It uses the daemon's monitor and rule, and returns nil
// when the read fails. Unlike /api/status it cannot see whether the daemon
// reaches the phone right now, so it never reports google_unreachable.
func statusSMSPath(ctx context.Context, store ingest.GoogleInboxReader, now time.Time) *freshness.SMSPathReport {
	report, ok := freshness.NewSMSPathMonitor(ingest.GoogleSMSPathLoader(store)).Report(ctx, now)
	if !ok {
		return nil
	}
	return &report
}

// smsPathStatusLine renders the SMS-path verdict as one line for
// `openmessage status`.
func smsPathStatusLine(report freshness.SMSPathReport) string {
	since := func(ms int64) string {
		return fmt.Sprintf("%s (%s)", fmtTS(ms), humanHours(time.Duration(report.SilentMS)*time.Millisecond))
	}
	switch report.Reason {
	case freshness.SMSPathStalled:
		return fmt.Sprintf(
			"⚠ Google SMS: no incoming SMS since %s; at its usual %.1f arrivals a day about %.0f "+
				"would have come, while %d RCS message(s) arrived in the last %s. Carrier SMS seems "+
				"to have stopped reaching the phone; try restarting the phone.",
			since(report.LastSMSMS), report.ArrivalsPerDay, report.ExpectedArrivals,
			report.RCSInWindow, humanHours(time.Duration(report.WindowMS)*time.Millisecond))
	case freshness.SMSPathUsualPace:
		return fmt.Sprintf(
			"Google SMS: no incoming SMS since %s, within this phone's usual pace (%.1f arrivals a day).",
			since(report.LastSMSMS), report.ArrivalsPerDay)
	case freshness.SMSPathRCSQuiet:
		return fmt.Sprintf(
			"Google SMS: no incoming SMS since %s, but RCS is quiet too, so this is not an SMS-only failure.",
			since(report.LastSMSMS))
	case freshness.SMSPathHistoryStale:
		return "Google SMS: not judged; the inbox history could not be reloaded."
	case freshness.SMSPathFlowing:
		return fmt.Sprintf("Google SMS: last incoming SMS %s.", since(report.LastSMSMS))
	case freshness.SMSPathThinBaseline:
		return fmt.Sprintf(
			"Google SMS: not judged; in the %s before the last SMS it went a whole %s without one %d time(s) "+
				"(allowed %d) and kept its usual rhythm for %s (needs %s), with %d separate arrivals (needs %d).",
			humanHours(time.Duration(report.BaselineWindowMS)*time.Millisecond),
			humanHours(time.Duration(report.WindowMS)*time.Millisecond), report.LongGaps, report.MaxLongGaps,
			humanHours(time.Duration(report.RegularSpanMS)*time.Millisecond),
			humanHours(time.Duration(report.MinRegularSpanMS)*time.Millisecond),
			report.BaselineArrivals, report.MinBaselineArrivals)
	default:
		return "Google SMS: not judged; no incoming SMS in the Google inbox."
	}
}

// humanHours renders a duration as whole hours below two days, else days.
func humanHours(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	if d < 48*time.Hour {
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}
