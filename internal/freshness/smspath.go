package freshness

import (
	"math"
	"sort"
	"time"
)

// Transport is the network path an incoming Google Messages message took to
// the phone.
type Transport int

const (
	// TransportUnknown marks a message whose path the frames do not reveal.
	TransportUnknown Transport = iota
	// TransportSMS is the carrier messaging path: SMS and MMS. On a phone that
	// delivers SMS over IMS it depends on the IMS stack, not mobile data.
	TransportSMS
	// TransportRCS is Rich Communication Services, which rides mobile data or
	// Wi-Fi.
	TransportRCS
)

func (t Transport) String() string {
	switch t {
	case TransportSMS:
		return "sms"
	case TransportRCS:
		return "rcs"
	default:
		return "unknown"
	}
}

// TransportEvent is one incoming message and the path it took.
type TransportEvent struct {
	At        time.Time
	Transport Transport
}

// SMSPathConfig tunes when quiet carrier SMS counts as a broken SMS path.
type SMSPathConfig struct {
	// Window is the shortest SMS silence ever judged broken. RCS must keep
	// arriving inside the same trailing window.
	Window time.Duration
	// MinExpectedArrivals is how many separate SMS arrivals the phone's usual
	// pace must predict for the silence before it counts as broken. Arrivals
	// that are memoryless at the usual pace would all miss a silence with
	// probability exp(-MinExpectedArrivals).
	MinExpectedArrivals float64
	// ArrivalGap merges SMS closer together than this into one arrival, so a
	// burst of verification codes or a group MMS counts once.
	ArrivalGap time.Duration
	// BaselineDays is how many days (of 24 hours), ending at the last SMS,
	// make up the baseline the usual pace is measured on. The silence is
	// judged only when at least MinActiveDays of them had an incoming SMS and
	// the baseline holds at least MinBaselineArrivals arrivals. A phone that
	// gets texts only on weekdays (20 of any 28 days), rarely, or in clusters
	// days apart is never judged.
	//
	// Days are counted without a calendar: under each of the 24 hour-aligned
	// ways to cut time into 24-hour days, count the days with an SMS among the
	// BaselineDays ending on the last SMS's day, and take the smallest count.
	// A weekday-only texter scores 20 under the cut that matches their own
	// midnight, wherever this machine's clock is set, and no daylight-saving
	// or time-zone change can move the count.
	BaselineDays        int
	MinActiveDays       int
	MinBaselineArrivals int
	// MinRCSInWindow is how many incoming RCS messages the trailing window must
	// hold, and RCSRecency how old the newest may be, to show that the phone,
	// its data connection and the Google relay are alive now. A relay that
	// stopped more than RCSRecency ago reads as rcs_quiet, not as an SMS-only
	// failure.
	MinRCSInWindow int
	RCSRecency     time.Duration
}

// DefaultSMSPathConfig is calibrated on the Google Messages history of the
// install that lost carrier SMS from 2026-10-03 16:16 to 2026-10-07 15:29 EDT
// while RCS kept arriving (a phone IMS-stack failure; a full restart fixed it).
// That phone gets about 5.8 SMS arrivals a day, so six expected arrivals take
// about 25 hours, and the longest normal gap between incoming SMS from
// 2026-09-04 to the outage was 19.2 hours. Replayed as live checks saw it,
// frames counting only once received, the rule flags the outage about 25 hours
// after the last SMS and nowhere before it. In simulation (texts in waking
// hours, RCS hourly) it never fired for weekday-only texters, with or without
// holidays, and fired zero to four times in 330 days for memoryless daily
// texters at one to ten arrivals a day, the statistical floor for a phone
// whose texts come at random.
var DefaultSMSPathConfig = SMSPathConfig{
	Window:              24 * time.Hour,
	MinExpectedArrivals: 6,
	ArrivalGap:          30 * time.Minute,
	BaselineDays:        28,
	MinActiveDays:       21,
	MinBaselineArrivals: 10,
	MinRCSInWindow:      3,
	RCSRecency:          3 * time.Hour,
}

// Reasons reported in SMSPathVerdict.Reason.
const (
	// SMSPathNoHistory: no incoming SMS has ever been seen.
	SMSPathNoHistory = "no_sms_history"
	// SMSPathThinBaseline: the baseline is too sparse or too irregular to
	// judge a silence against.
	SMSPathThinBaseline = "thin_baseline"
	// SMSPathFlowing: an incoming SMS arrived inside the window.
	SMSPathFlowing = "sms_recent"
	// SMSPathUsualPace: SMS has been silent for the window, but at the usual
	// pace fewer than MinExpectedArrivals arrivals would have come, or the
	// phone has recently gone this long between texts (UsualGap).
	SMSPathUsualPace = "within_usual_pace"
	// SMSPathRCSQuiet: SMS is silent but RCS is not flowing either, so the
	// silence cannot be pinned on the SMS path.
	SMSPathRCSQuiet = "rcs_quiet"
	// SMSPathStalled: SMS silent far longer than its usual pace explains while
	// RCS flowed.
	SMSPathStalled = "sms_silent_rcs_flowing"
)

// SMSPathVerdict reports whether carrier SMS has stopped while RCS flows.
type SMSPathVerdict struct {
	// LastSMS is the newest incoming SMS. A stalled verdict keeps the same
	// LastSMS for the whole outage, so it identifies the episode.
	LastSMS time.Time
	LastRCS time.Time
	// Silence is how long ago LastSMS arrived.
	Silence time.Duration
	// RCSInWindow counts incoming RCS messages in (now-Window, now].
	RCSInWindow int
	// BaselineSMS and BaselineArrivals count the incoming SMS, and the
	// arrivals they merge into, in the baseline; ActiveDays counts the
	// baseline days that had any, under the least favourable cut into days.
	BaselineSMS      int
	BaselineArrivals int
	ActiveDays       int
	// UsualGap is the second-longest stretch between two baseline SMS: how
	// long a quiet spell normally lasts. The single longest is left out so
	// one earlier outage does not raise the bar; a weekly lull repeats and
	// still counts. A stall must outlast it.
	UsualGap time.Duration
	// ArrivalsPerDay is the usual pace: arrivals after the first, over the
	// time from the first to LastSMS. ExpectedArrivals is that pace times
	// Silence.
	ArrivalsPerDay   float64
	ExpectedArrivals float64
	// Evaluated is false when there is no SMS history or the baseline is too
	// thin to judge; Stalled is then always false.
	Evaluated bool
	Stalled   bool
	Reason    string
}

// EvaluateSMSPath judges, as of now, whether incoming carrier SMS has been
// absent far longer than its usual pace explains while RCS kept arriving.
// events holds incoming messages only, in any order. Events timed after now
// are ignored; a replay of stored history that should match what a live check
// saw must also drop events not yet received by then, which the event times
// alone cannot show. Outgoing messages are not evidence either way: they
// measure what the user sent, not what the carrier delivered.
func EvaluateSMSPath(events []TransportEvent, now time.Time, cfg SMSPathConfig) SMSPathVerdict {
	var verdict SMSPathVerdict
	for _, event := range events {
		if event.At.After(now) {
			continue
		}
		switch event.Transport {
		case TransportSMS:
			if event.At.After(verdict.LastSMS) {
				verdict.LastSMS = event.At
			}
		case TransportRCS:
			if event.At.After(verdict.LastRCS) {
				verdict.LastRCS = event.At
			}
		}
	}
	if verdict.LastSMS.IsZero() {
		verdict.Reason = SMSPathNoHistory
		return verdict
	}
	verdict.Silence = now.Sub(verdict.LastSMS)

	baselineStart := verdict.LastSMS.Add(-time.Duration(cfg.BaselineDays) * 24 * time.Hour)
	windowStart := now.Add(-cfg.Window)
	var baseline []time.Time
	for _, event := range events {
		if event.At.After(now) {
			continue
		}
		switch event.Transport {
		case TransportSMS:
			if event.At.After(baselineStart) {
				baseline = append(baseline, event.At)
			}
		case TransportRCS:
			if event.At.After(windowStart) {
				verdict.RCSInWindow++
			}
		}
	}
	sort.Slice(baseline, func(i, j int) bool { return baseline[i].Before(baseline[j]) })
	verdict.BaselineSMS = len(baseline)
	var longest time.Duration
	for i, at := range baseline {
		if i == 0 {
			verdict.BaselineArrivals++
			continue
		}
		gap := at.Sub(baseline[i-1])
		if gap > cfg.ArrivalGap {
			verdict.BaselineArrivals++
		}
		switch {
		case gap > longest:
			verdict.UsualGap, longest = longest, gap
		case gap > verdict.UsualGap:
			verdict.UsualGap = gap
		}
	}
	verdict.ActiveDays = leastActiveDays(baseline, verdict.LastSMS, cfg.BaselineDays)
	if span := verdict.LastSMS.Sub(baseline[0]); span > 0 && verdict.BaselineArrivals > 1 {
		verdict.ArrivalsPerDay = float64(verdict.BaselineArrivals-1) / span.Hours() * 24
	}
	verdict.ExpectedArrivals = verdict.ArrivalsPerDay * verdict.Silence.Hours() / 24

	if verdict.BaselineArrivals < cfg.MinBaselineArrivals || verdict.ActiveDays < cfg.MinActiveDays {
		verdict.Reason = SMSPathThinBaseline
		return verdict
	}
	verdict.Evaluated = true
	switch {
	case verdict.Silence < cfg.Window:
		verdict.Reason = SMSPathFlowing
	case verdict.ExpectedArrivals < cfg.MinExpectedArrivals, verdict.Silence <= verdict.UsualGap:
		verdict.Reason = SMSPathUsualPace
	case verdict.RCSInWindow < cfg.MinRCSInWindow,
		verdict.LastRCS.IsZero(),
		now.Sub(verdict.LastRCS) > cfg.RCSRecency:
		verdict.Reason = SMSPathRCSQuiet
	default:
		verdict.Stalled = true
		verdict.Reason = SMSPathStalled
	}
	return verdict
}

// leastActiveDays counts the days with an SMS among the `days` 24-hour days
// ending on the last SMS's day, under each of the 24 hour-aligned cuts of time
// into days, and returns the smallest count. Every such day lies within
// days*24h before last, so sms need only hold that stretch.
func leastActiveDays(sms []time.Time, last time.Time, days int) int {
	const day = int64(24 * 60 * 60)
	least := -1
	for offset := int64(0); offset < 24; offset++ {
		index := func(at time.Time) int64 {
			return floorDiv(at.Unix()-offset*3600, day)
		}
		lastIndex := index(last)
		active := map[int64]bool{}
		for _, at := range sms {
			if i := index(at); i <= lastIndex && lastIndex-i < int64(days) {
				active[i] = true
			}
		}
		if least < 0 || len(active) < least {
			least = len(active)
		}
	}
	return least
}

func floorDiv(a, b int64) int64 {
	q := a / b
	if a%b != 0 && (a < 0) != (b < 0) {
		q--
	}
	return q
}

// SMSPathReport is the published form of a verdict, shared by /api/status
// (freshness.google.sms_path) and `openmessage status --json`. Times are Unix
// milliseconds, 0 when absent; the rule's thresholds ride along so a reader
// can tell what was judged.
type SMSPathReport struct {
	// Source names the data judged: the v2 inbox, where Google frames keep the
	// protobuf type that tells SMS from RCS.
	Source              string  `json:"source"`
	Evaluated           bool    `json:"evaluated"`
	Stalled             bool    `json:"stalled"`
	Reason              string  `json:"reason"`
	LastSMSMS           int64   `json:"last_sms_ms"`
	LastRCSMS           int64   `json:"last_rcs_ms"`
	SilentMS            int64   `json:"silent_ms"`
	RCSInWindow         int     `json:"rcs_in_window"`
	BaselineSMS         int     `json:"baseline_sms"`
	BaselineArrivals    int     `json:"baseline_arrivals"`
	ActiveDays          int     `json:"active_days"`
	ArrivalsPerDay      float64 `json:"arrivals_per_day"`
	ExpectedArrivals    float64 `json:"expected_arrivals"`
	UsualGapMS          int64   `json:"usual_gap_ms"`
	WindowMS            int64   `json:"window_ms"`
	MinExpectedArrivals float64 `json:"min_expected_arrivals"`
	BaselineDays        int     `json:"baseline_days"`
	MinActiveDays       int     `json:"min_active_days"`
	MinBaselineArrivals int     `json:"min_baseline_arrivals"`
	MinRCSInWindow      int     `json:"min_rcs_in_window"`
	RCSRecencyMS        int64   `json:"rcs_recency_ms"`
	// HistoryLoadedAtMS is when the judged history was read from the inbox,
	// when a monitor supplied it.
	HistoryLoadedAtMS int64 `json:"history_loaded_at_ms,omitempty"`
	// Frames, Malformed and Unknown describe the inbox frames read, when the
	// loader reports them, so an empty result can be told from an unreadable
	// one.
	Frames    int `json:"frames,omitempty"`
	Malformed int `json:"malformed,omitempty"`
	Unknown   int `json:"unknown,omitempty"`
}

// NewSMSPathReport publishes a verdict judged under cfg.
func NewSMSPathReport(verdict SMSPathVerdict, cfg SMSPathConfig) SMSPathReport {
	return SMSPathReport{
		Source:              "v2_inbox",
		Evaluated:           verdict.Evaluated,
		Stalled:             verdict.Stalled,
		Reason:              verdict.Reason,
		LastSMSMS:           unixMilliOrZero(verdict.LastSMS),
		LastRCSMS:           unixMilliOrZero(verdict.LastRCS),
		SilentMS:            verdict.Silence.Milliseconds(),
		RCSInWindow:         verdict.RCSInWindow,
		BaselineSMS:         verdict.BaselineSMS,
		BaselineArrivals:    verdict.BaselineArrivals,
		ActiveDays:          verdict.ActiveDays,
		ArrivalsPerDay:      math.Round(verdict.ArrivalsPerDay*100) / 100,
		ExpectedArrivals:    math.Round(verdict.ExpectedArrivals*100) / 100,
		UsualGapMS:          verdict.UsualGap.Milliseconds(),
		WindowMS:            cfg.Window.Milliseconds(),
		MinExpectedArrivals: cfg.MinExpectedArrivals,
		BaselineDays:        cfg.BaselineDays,
		MinActiveDays:       cfg.MinActiveDays,
		MinBaselineArrivals: cfg.MinBaselineArrivals,
		MinRCSInWindow:      cfg.MinRCSInWindow,
		RCSRecencyMS:        cfg.RCSRecency.Milliseconds(),
	}
}

func unixMilliOrZero(at time.Time) int64 {
	if at.IsZero() {
		return 0
	}
	return at.UnixMilli()
}
