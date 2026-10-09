package web

import (
	"context"
	"time"

	"github.com/maxghenis/openmessage/internal/freshness"
)

// smsPathQueryTimeout bounds the inbox read behind one status refresh.
const smsPathQueryTimeout = 5 * time.Second

// addGoogleSMSPath annotates the Google freshness entry with an "sms_path"
// block saying whether incoming carrier SMS has stopped while RCS keeps
// arriving, and stamps a top-level "sms_path_stalled".
//
// googleReachable reports whether the daemon can reach the phone right now;
// while it cannot, missing SMS says nothing about the phone's SMS path, so a
// stall is withheld as google_unreachable. The block never changes "stale":
// the macOS app reads a stale Google entry as "needs re-pairing", and a phone
// that has lost SMS needs a restart, not a re-pair. A nil monitor, one that
// has never loaded, or an install that has never received an SMS leaves the
// payload untouched.
func addGoogleSMSPath(
	out map[string]any,
	monitor *freshness.SMSPathMonitor,
	now time.Time,
	googleReachable func() bool,
) {
	if out == nil || monitor == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), smsPathQueryTimeout)
	defer cancel()
	report, ok := monitor.Report(ctx, now)
	if !ok || report.Reason == freshness.SMSPathNoHistory {
		return
	}
	if report.Stalled && googleReachable != nil && !googleReachable() {
		report.Stalled = false
		report.Reason = freshness.SMSPathGoogleUnreachable
	}
	entry, ok := out["google"].(map[string]any)
	if !ok {
		entry = map[string]any{
			"latest_ms":          int64(0),
			"latest_received_ms": int64(0),
			"behind_days":        0,
			"stale":              false,
		}
		out["google"] = entry
	}
	entry["sms_path"] = report
	out["sms_path_stalled"] = report.Stalled
}

// withFreshGoogleSMSPath returns a copy of a cached freshness value with the
// SMS path judged again. The cached map is shared with responses already being
// written, so it is copied rather than changed in place.
func withFreshGoogleSMSPath(
	cached map[string]any,
	monitor *freshness.SMSPathMonitor,
	now time.Time,
	googleReachable func() bool,
) map[string]any {
	if cached == nil || monitor == nil {
		return cached
	}
	out := make(map[string]any, len(cached))
	for key, value := range cached {
		out[key] = value
	}
	delete(out, "sms_path_stalled")
	if google, ok := cached["google"].(map[string]any); ok {
		entry := make(map[string]any, len(google))
		for key, value := range google {
			if key != "sms_path" {
				entry[key] = value
			}
		}
		out["google"] = entry
	}
	addGoogleSMSPath(out, monitor, now, googleReachable)
	return out
}
