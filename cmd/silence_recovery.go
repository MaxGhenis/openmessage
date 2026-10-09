package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/maxghenis/openmessage/internal/app"
	"github.com/maxghenis/openmessage/internal/freshness"
	"github.com/maxghenis/openmessage/internal/silencerecovery"
)

// silenceRecoveryEnv turns the automatic window backfill after a flagged
// Google silence off when set to 0, false, no or off.
const silenceRecoveryEnv = "OPENMESSAGES_SILENCE_RECOVERY"

func silenceRecoveryDisabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(silenceRecoveryEnv))) {
	case "0", "false", "no", "off":
		return true
	default:
		return false
	}
}

// newGoogleSilenceRecovery builds the recovery that runs one window backfill
// after each flagged Google silence ends (internal/silencerecovery). It
// watches the same activity clock /api/status judges silence by, so "flagged"
// means the same thing in both. It returns nil, with the reason, when there is
// no clock or the recovery is turned off. Only the transport-owning daemon may
// build one: a second process would fetch with no client of its own.
func newGoogleSilenceRecovery(
	a *app.App,
	source freshness.ActivitySource,
	v2Primary bool,
	logger zerolog.Logger,
) (*silencerecovery.Recoverer, string) {
	if source == nil {
		return nil, "no activity clock"
	}
	if silenceRecoveryDisabled() {
		return nil, "disabled by " + silenceRecoveryEnv
	}
	cfg := silencerecovery.DefaultConfig()
	// v2-primary readers see only what reaches v2, so a run whose history
	// never reached v2 ingest has not recovered anything for them.
	cfg.RequireV2 = v2Primary
	statePath := ""
	if strings.TrimSpace(a.DataDir) != "" {
		statePath = filepath.Join(a.DataDir, silencerecovery.StateFileName)
	}
	return silencerecovery.New(
		cfg,
		source,
		googleWindowRunner{app: a},
		statePath,
		logger,
	), ""
}

// silenceRecoveryStatus is /api/status's "silence_recovery" block.
func silenceRecoveryStatus(recoverer *silencerecovery.Recoverer, disabledReason string) func() any {
	return func() any {
		if recoverer == nil {
			return map[string]any{"google": map[string]any{"enabled": false, "reason": disabledReason}}
		}
		return map[string]any{"google": recoverer.Snapshot()}
	}
}

// googleWindowRunner runs the recovery's window backfills on the app.
type googleWindowRunner struct {
	app *app.App
}

func (r googleWindowRunner) Ready() (bool, string) {
	return googleWindowBackfillReady(r.app.GoogleStatus())
}

func (r googleWindowRunner) RunWindowBackfill(since time.Time) (silencerecovery.RunResult, bool) {
	result, started := r.app.RunGoogleWindowBackfill(since, app.BackfillTriggerSilenceRecovery)
	return silenceRecoveryRunResult(result), started
}

func silenceRecoveryRunResult(result app.GoogleWindowBackfillResult) silencerecovery.RunResult {
	return silencerecovery.RunResult{
		Connected:          result.Connected,
		Aborted:            result.Aborted,
		Listed:             result.Listed,
		Conversations:      result.Conversations,
		Messages:           result.Messages,
		EmptyConversations: result.EmptyConversations,
		Errors:             result.Errors,
		HistoryTeed:        result.HistoryTeed,
		HistoryTeeFailed:   result.HistoryTeeFailed,
		InboxOutcome:       string(result.InboxOutcome),
	}
}

// googleWindowBackfillReady reports whether a recovery run may start. The
// recovery only fetches over a healthy session: it waits out a disconnect, an
// expired or broken session and an unresponsive phone instead of acting on
// them, and never starts a reconnect or a re-pair itself. A 401 during its
// fetch is handled like any other pull's: HandleGoogleAuthExpiredError marks
// the session expired and ends the current connection generation, and the
// supervisor, which owns repair and reconnect, takes it from there.
func googleWindowBackfillReady(status app.GoogleStatusSnapshot) (bool, string) {
	switch {
	case !status.Paired:
		return false, "not_paired"
	case !status.Connected:
		return false, "disconnected"
	case status.AuthExpired:
		return false, "auth_expired"
	case status.NeedsRepair:
		return false, "needs_repair"
	case !status.PhoneResponding:
		return false, "phone_not_responding"
	default:
		return true, ""
	}
}
