package freshness

import (
	"context"
	"sync"
	"time"
)

// Reasons a monitor or its caller reports on top of EvaluateSMSPath's.
const (
	// SMSPathHistoryStale: the history could not be reloaded for longer than
	// the monitor's StaleAfter, so a stall it shows may already be over.
	SMSPathHistoryStale = "history_stale"
	// SMSPathGoogleUnreachable: the daemon itself cannot reach the phone right
	// now, so missing SMS says nothing about the phone's SMS path.
	SMSPathGoogleUnreachable = "google_unreachable"
)

// SMSPathLoad is one read of the incoming-message history: the events plus
// counts that tell an empty result from an unreadable one.
type SMSPathLoad struct {
	Events    []TransportEvent
	Frames    int
	Malformed int
	Unknown   int
}

// SMSPathLoader returns the incoming messages, labelled by transport, that
// arrived since the given time.
type SMSPathLoader func(ctx context.Context, since time.Time) (SMSPathLoad, error)

// SMSPathMonitor keeps the recent incoming-message history the SMS-path check
// reads and judges it on demand. Loading scans the inbox and decodes every
// Google frame in the lookback (about 65 ms for 6,700 frames on the install it
// was calibrated on), so the history is cached for Refresh; each verdict is
// still judged against the caller's clock, so silence keeps growing between
// loads. Ages are measured on the wall clock: Go's monotonic clock stops while
// a Mac sleeps, and a history loaded before a night's sleep must not pass for
// fresh after it.
type SMSPathMonitor struct {
	load SMSPathLoader
	// Config is the rule the verdict applies.
	Config SMSPathConfig
	// Lookback is how much history each load reads. It covers the 28-day
	// baseline plus a silence of up to about two weeks.
	Lookback time.Duration
	// Refresh is how long a loaded history is reused.
	Refresh time.Duration
	// StaleAfter is how old the last loaded history may be before a stall is
	// withheld as SMSPathHistoryStale.
	StaleAfter time.Duration

	mu       sync.Mutex
	last     SMSPathLoad
	loadedAt time.Time
	loaded   bool
}

// NewSMSPathMonitor returns a monitor with the default rule, a 42-day
// lookback, a five-minute refresh and a 15-minute staleness limit. An SMS that
// ends an outage can take up to the refresh to clear the verdict, which is
// noise against a 24-hour rule.
func NewSMSPathMonitor(load SMSPathLoader) *SMSPathMonitor {
	return &SMSPathMonitor{
		load:       load,
		Config:     DefaultSMSPathConfig,
		Lookback:   42 * 24 * time.Hour,
		Refresh:    5 * time.Minute,
		StaleAfter: 15 * time.Minute,
	}
}

// Report judges the SMS path as of now. ok is false when no history has
// loaded yet; a failed reload keeps judging the last history that loaded, but
// withholds a stall once that history is older than StaleAfter.
func (m *SMSPathMonitor) Report(ctx context.Context, now time.Time) (report SMSPathReport, ok bool) {
	if m == nil || m.load == nil {
		return SMSPathReport{}, false
	}
	wall := now.Round(0)
	m.mu.Lock()
	defer m.mu.Unlock()
	if age := wall.Sub(m.loadedAt); !m.loaded || age >= m.Refresh || age < 0 {
		if load, err := m.load(ctx, wall.Add(-m.Lookback)); err == nil {
			m.last = load
			m.loadedAt = wall
			m.loaded = true
		}
	}
	if !m.loaded {
		return SMSPathReport{}, false
	}
	verdict := EvaluateSMSPath(m.last.Events, now, m.Config)
	report = NewSMSPathReport(verdict, m.Config)
	report.HistoryLoadedAtMS = m.loadedAt.UnixMilli()
	report.Frames = m.last.Frames
	report.Malformed = m.last.Malformed
	report.Unknown = m.last.Unknown
	if report.Stalled && wall.Sub(m.loadedAt) > m.StaleAfter {
		report.Stalled = false
		report.Reason = SMSPathHistoryStale
	}
	return report, true
}
