package freshness

import (
	"context"
	"errors"
	"testing"
	"time"
)

func monitorFor(load SMSPathLoader) *SMSPathMonitor {
	monitor := NewSMSPathMonitor(load)
	monitor.Config = testSMSPathConfig
	return monitor
}

func TestSMSPathMonitorCachesHistoryButJudgesAgainstTheCallersClock(t *testing.T) {
	lastSMS := time.Date(2026, 10, 3, 20, 0, 0, 0, time.UTC)
	start := lastSMS.Add(20 * time.Hour)
	history := outage(lastSMS, start.Add(20*time.Hour))
	var loads []time.Time
	monitor := monitorFor(func(_ context.Context, since time.Time) (SMSPathLoad, error) {
		loads = append(loads, since)
		return SMSPathLoad{Events: history, Frames: 7, Malformed: 1, Unknown: 2}, nil
	})

	report, ok := monitor.Report(context.Background(), start)
	if !ok || report.Stalled || report.Reason != SMSPathFlowing {
		t.Fatalf("20h in: report = %+v ok = %v, want flowing", report, ok)
	}
	if len(loads) != 1 || !loads[0].Equal(start.Add(-42*24*time.Hour)) {
		t.Fatalf("loads = %v, want one from the 42-day lookback", loads)
	}
	if report.Frames != 7 || report.Malformed != 1 || report.Unknown != 2 || report.HistoryLoadedAtMS != start.UnixMilli() {
		t.Fatalf("load details not reported: %+v", report)
	}

	// Ninety seconds later the cached history is reused; ten hours later it is
	// reloaded; either way the verdict uses the new clock.
	report, _ = monitor.Report(context.Background(), start.Add(90*time.Second))
	if len(loads) != 1 || report.SilentMS != (20*time.Hour+90*time.Second).Milliseconds() {
		t.Fatalf("cached: loads = %d report = %+v", len(loads), report)
	}
	report, _ = monitor.Report(context.Background(), start.Add(10*time.Hour))
	if len(loads) != 2 || !report.Stalled {
		t.Fatalf("reloaded: loads = %d report = %+v, want a reload and a stall", len(loads), report)
	}
}

// Go's monotonic clock stops while a Mac sleeps, so a history loaded before
// sleep would look minutes old after a night. The monitor keeps wall-clock
// times only, which subtract on the wall clock.
func TestSMSPathMonitorMeasuresAgeOnTheWallClock(t *testing.T) {
	monitor := monitorFor(func(context.Context, time.Time) (SMSPathLoad, error) { return SMSPathLoad{}, nil })
	monitor.Report(context.Background(), time.Now())
	if monitor.loadedAt != monitor.loadedAt.Round(0) {
		t.Fatal("loadedAt carries a monotonic reading; ages would stop counting while the Mac sleeps")
	}
}

func TestSMSPathMonitorWithholdsAStallFromStaleHistory(t *testing.T) {
	lastSMS := time.Date(2026, 10, 3, 20, 0, 0, 0, time.UTC)
	start := lastSMS.Add(30 * time.Hour)
	fail := false
	monitor := monitorFor(func(context.Context, time.Time) (SMSPathLoad, error) {
		if fail {
			return SMSPathLoad{}, errors.New("database is locked")
		}
		return SMSPathLoad{Events: outage(lastSMS, start.Add(2*time.Hour))}, nil
	})
	if report, ok := monitor.Report(context.Background(), start); !ok || !report.Stalled {
		t.Fatalf("first load: %+v ok = %v, want stalled", report, ok)
	}
	fail = true
	// Ten minutes on, the failed reload still leaves usable history.
	if report, _ := monitor.Report(context.Background(), start.Add(10*time.Minute)); !report.Stalled {
		t.Fatalf("10 min after a failed reload: %+v, want the last history judged", report)
	}
	// Past StaleAfter the stall is withheld: SMS may have arrived since.
	report, ok := monitor.Report(context.Background(), start.Add(20*time.Minute))
	if !ok || report.Stalled || report.Reason != SMSPathHistoryStale || report.HistoryLoadedAtMS != start.UnixMilli() {
		t.Fatalf("20 min after the last load: %+v, want %q", report, SMSPathHistoryStale)
	}
}

func TestSMSPathMonitorReportsNothingBeforeTheFirstLoad(t *testing.T) {
	monitor := monitorFor(func(context.Context, time.Time) (SMSPathLoad, error) {
		return SMSPathLoad{}, errors.New("no store")
	})
	if report, ok := monitor.Report(context.Background(), smsPathNow); ok {
		t.Fatalf("report = %+v, want ok=false before any load", report)
	}
	var nilMonitor *SMSPathMonitor
	if _, ok := nilMonitor.Report(context.Background(), smsPathNow); ok {
		t.Fatal("nil monitor reported")
	}
}
