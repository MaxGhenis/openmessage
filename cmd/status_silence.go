package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/freshness"
	"github.com/maxghenis/openmessage/internal/localapi"
)

// Who judged a platform's silence in `openmessage status`.
const (
	silenceJudgedByDaemon = "daemon"
	silenceJudgedByLocal  = "local"
)

// statusSilenceQueryTimeout bounds the activity queries behind a local
// verdict, like the daemon's own silenceQueryTimeout.
const statusSilenceQueryTimeout = 5 * time.Second

// platformSilence is one status platform's silence verdict: the
// freshness.<platform>.silence object of /api/status, kept as raw JSON so
// --json shows the daemon's verdict unchanged (same keys, order and values;
// only the indentation differs).
type platformSilence struct {
	Block    json.RawMessage
	JudgedBy string
	// SilentMS, Stalled and BaselineUnavailable are read back from Block for
	// the text output.
	SilentMS            int64
	Stalled             bool
	BaselineUnavailable bool
	source              string
}

// statusSilence holds the verdicts by status platform key ("google",
// "whatsapp", "signal") and the sentences saying who judged them.
type statusSilence struct {
	byPlatform map[string]platformSilence
	note       string
}

// judgeStatusSilence returns a silence verdict for each status platform in
// platforms. A platform takes the running app's verdict when the app serves
// this data dir and reports one in freshness.<platform>.silence measured on
// the source this command reads (daemon truth, as for sends). Otherwise it is
// judged here with the same rule and config (freshness.EvaluateSilence,
// DefaultSilenceConfig) over the activity source the daemon would read: the v2
// inbox when reads come from the v2 store, the legacy store's incoming
// messages otherwise.
func judgeStatusSilence(
	ctx context.Context,
	session *commandReadSession,
	deps statusDeps,
	now time.Time,
	platforms []string,
) statusSilence {
	// Demo data is a frozen fixture; the daemon doesn't judge its silence
	// either.
	if deps.demo || len(platforms) == 0 {
		return statusSilence{}
	}
	out := statusSilence{byPlatform: map[string]platformSilence{}}
	source := deps.source
	if source == nil {
		source = session.activitySource()
	}
	localSource := ""
	if source != nil {
		localSource = source.Name()
	}
	daemonBlocks, whyLocal := daemonSilenceBlocks(ctx, deps.daemon, session.DataDir, localSource)
	var missing []string
	for _, platform := range platforms {
		if block, ok := daemonBlocks[platform]; ok {
			out.byPlatform[platform] = newPlatformSilence(block, silenceJudgedByDaemon)
			continue
		}
		missing = append(missing, platform)
	}
	if len(missing) > 0 && len(daemonBlocks) > 0 {
		whyLocal = "the running app reported none"
	}

	var localErr error
	var noActivity []string
	if len(missing) > 0 {
		if source == nil {
			localErr = fmt.Errorf("no activity source")
		} else {
			queryCtx, cancel := context.WithTimeout(ctx, statusSilenceQueryTimeout)
			blocks, err := judgeSilenceLocally(queryCtx, source, now, deps.loc, missing)
			cancel()
			localErr = err
			for platform, block := range blocks {
				out.byPlatform[platform] = newPlatformSilence(block, silenceJudgedByLocal)
			}
			if err == nil {
				for _, platform := range missing {
					if _, ok := blocks[platform]; !ok {
						noActivity = append(noActivity, platform)
					}
				}
			}
		}
	}
	out.note = silenceNote(out.byPlatform, localSource, whyLocal, localErr, noActivity)
	return out
}

// daemonSilenceBlocks probes the running app and returns the silence verdicts
// it reports for this store, by status platform: only when it serves dataDir
// and measured them on source, the activity source this command reads. With
// none it says why, for the note.
func daemonSilenceBlocks(ctx context.Context, daemon *localapi.Client, dataDir, source string) (map[string]json.RawMessage, string) {
	if daemon == nil {
		return nil, ""
	}
	probeCtx, cancel := context.WithTimeout(ctx, clientProbeTimeout)
	status, reachable, err := daemon.Status(probeCtx)
	cancel()
	switch {
	case err != nil && !reachable:
		return nil, unreachableReason(err)
	case err != nil:
		return nil, "the running app's /api/status was unreadable"
	}
	// The verdict describes the store the app writes. When that is a
	// different data dir (the CLI default ~/.local/share/openmessage while the
	// macOS app serves Application Support), this store is the one the table
	// and any search read, so judge it instead.
	served := strings.TrimSpace(status.Auth.DataDir)
	if served == "" {
		return nil, "the running app doesn't report its data dir"
	}
	if !samePath(served, dataDir) {
		return nil, "the running app serves " + served
	}
	var entries map[string]json.RawMessage
	if len(status.Freshness) > 0 && json.Unmarshal(status.Freshness, &entries) != nil {
		return nil, "the running app's freshness block was unreadable"
	}
	blocks := map[string]json.RawMessage{}
	otherSource := ""
	for platform, raw := range entries {
		var entry struct {
			Silence json.RawMessage `json:"silence"`
		}
		// Top-level scalars (newest_ms, silence_stalled, sms_path_stalled)
		// aren't objects.
		if json.Unmarshal(raw, &entry) != nil {
			continue
		}
		block := entry.Silence
		if len(block) == 0 || string(block) == "null" {
			continue
		}
		// The two sources clock different things: v2 inbox receipt times
		// (every frame a bridge hands to ingest) against the sender
		// timestamps of incoming rows in the legacy store. A verdict on one
		// does not describe the other; they part when frames stop reaching
		// one store while the other keeps ingesting (#155). On a v2-primary
		// install the app measures the v2 inbox, while this command reads the
		// legacy store unless OPENMESSAGES_V2_PRIMARY=1 is set.
		var measured struct {
			Source string `json:"source"`
		}
		if json.Unmarshal(block, &measured) != nil || measured.Source != source {
			otherSource = measured.Source
			continue
		}
		blocks[platform] = block
	}
	if len(blocks) == 0 {
		if otherSource != "" {
			return nil, "the running app measures " + sourceLabel(otherSource) + ", not " + sourceLabel(source)
		}
		return nil, "the running app doesn't report silence"
	}
	return blocks, ""
}

// unreachableReason says why no app answered the status probe: a refused
// connection means nothing listens on the port, while a timeout means
// something did and was too slow (the daemon's own refresh may spend up to its
// silenceQueryTimeout inside /api/status).
func unreachableReason(err error) string {
	var netErr net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()):
		return fmt.Sprintf("the running app didn't answer within %s", clientProbeTimeout)
	case errors.Is(err, syscall.ECONNREFUSED):
		return "the app isn't running"
	default:
		return "the app couldn't be reached: " + err.Error()
	}
}

// judgeSilenceLocally judges each of platforms that has any activity, the way
// the daemon's addSilence does on a refresh with nothing cached or carried:
// the baseline is the BaselineRange before the last event, and a baseline
// that can't be read still yields a verdict, judged without one.
func judgeSilenceLocally(
	ctx context.Context,
	source freshness.ActivitySource,
	now time.Time,
	loc *time.Location,
	platforms []string,
) (map[string]json.RawMessage, error) {
	cfg := freshness.DefaultSilenceConfig
	latest, err := source.Latest(ctx)
	if err != nil {
		return nil, err
	}
	blocks := map[string]json.RawMessage{}
	for _, platform := range platforms {
		last, ok := latest[platform]
		if !ok {
			continue
		}
		from, to := freshness.BaselineRange(last, loc, cfg)
		events, baselineErr := source.Between(ctx, platform, from, to)
		if baselineErr != nil {
			events = nil
		}
		verdict := freshness.EvaluateSilence(last, events, now, loc, cfg)
		block, err := json.Marshal(freshness.SilenceBlock(source.Name(), verdict, cfg, baselineErr != nil))
		if err != nil {
			return nil, err
		}
		blocks[platform] = block
	}
	return blocks, nil
}

// newPlatformSilence reads back the fields the text output needs. A daemon
// block is foreign input: silent_ms is read as any JSON number and a
// negative one counts as zero.
func newPlatformSilence(block json.RawMessage, judgedBy string) platformSilence {
	var view struct {
		Source              string  `json:"source"`
		SilentMS            float64 `json:"silent_ms"`
		Stalled             bool    `json:"stalled"`
		BaselineUnavailable bool    `json:"baseline_unavailable"`
	}
	_ = json.Unmarshal(block, &view)
	return platformSilence{
		Block:               block,
		JudgedBy:            judgedBy,
		SilentMS:            int64(math.Max(0, view.SilentMS)),
		Stalled:             view.Stalled,
		BaselineUnavailable: view.BaselineUnavailable,
		source:              view.Source,
	}
}

// silenceNote is the text under the status table that says who judged the
// silence verdicts, from what, and why any were judged locally, then which
// verdicts lacked a baseline and which platforms had nothing to judge:
//
//	Silence judged by the running app from the v2 inbox.
//	Silence judged locally from the v2 inbox (the app isn't running).
//	Silence: Google Messages judged by the running app from the v2 inbox; Signal judged locally from the v2 inbox (the running app reported none).
func silenceNote(
	byPlatform map[string]platformSilence,
	localSource, whyLocal string,
	localErr error,
	noActivity []string,
) string {
	var daemon, local, noBaseline []string
	daemonSource := ""
	platforms := make([]string, 0, len(byPlatform))
	for platform := range byPlatform {
		platforms = append(platforms, platform)
	}
	sort.Strings(platforms)
	for _, platform := range platforms {
		verdict := byPlatform[platform]
		name := platformDisplayName(platform)
		if verdict.JudgedBy == silenceJudgedByDaemon {
			daemon = append(daemon, name)
			daemonSource = verdict.source
		} else {
			local = append(local, name)
		}
		if verdict.BaselineUnavailable {
			noBaseline = append(noBaseline, name)
		}
	}
	reason := func(why string) string {
		if why == "" {
			return ""
		}
		return " (" + why + ")"
	}
	if localErr != nil {
		// A failed local query explains the platforms left without a verdict.
		whyLocal = strings.TrimPrefix(whyLocal+"; ", "; ") + "local query failed: " + localErr.Error()
	}

	var sentences []string
	switch {
	case len(daemon) == 0 && len(local) == 0:
		if localErr != nil {
			sentences = append(sentences, "Silence not judged"+reason(whyLocal)+".")
		}
	case len(local) == 0 && localErr == nil:
		sentences = append(sentences, "Silence judged by the running app"+fromSource(daemonSource)+".")
	case len(daemon) == 0:
		sentences = append(sentences, "Silence judged locally"+fromSource(localSource)+reason(whyLocal)+".")
	default:
		parts := []string{strings.Join(daemon, ", ") + " judged by the running app" + fromSource(daemonSource)}
		if len(local) > 0 {
			parts = append(parts, strings.Join(local, ", ")+" judged locally"+fromSource(localSource)+reason(whyLocal))
		} else {
			parts = append(parts, "the rest not judged"+reason(whyLocal))
		}
		sentences = append(sentences, "Silence: "+strings.Join(parts, "; ")+".")
	}
	if len(noBaseline) > 0 {
		// EvaluateSilence without a baseline can only apply LongSilence.
		sentences = append(sentences, fmt.Sprintf("%s judged without a baseline, so only the %dh floor applies.",
			strings.Join(noBaseline, ", "), int(freshness.DefaultSilenceConfig.LongSilence.Hours())))
	}
	if len(noActivity) > 0 {
		names := make([]string, len(noActivity))
		for i, platform := range noActivity {
			names[i] = platformDisplayName(platform)
		}
		sort.Strings(names)
		sentence := strings.Join(names, ", ") + " not judged: no activity recorded" + strings.Replace(fromSource(localSource), " from ", " in ", 1)
		if len(daemon) == 0 && len(local) == 0 {
			sentence += reason(whyLocal)
		}
		sentences = append(sentences, sentence+".")
	}
	return strings.Join(sentences, " ")
}

// platformDisplayName names a status platform key for people. The table
// labels Google Messages rows by what they store (sms, rcs).
func platformDisplayName(platform string) string {
	switch platform {
	case "google":
		return "Google Messages"
	case "whatsapp":
		return "WhatsApp"
	case "signal":
		return "Signal"
	default:
		return platform
	}
}

// fromSource names an ActivitySource for people, as a " from ..." phrase.
func fromSource(name string) string {
	if name == "" {
		return ""
	}
	return " from " + sourceLabel(name)
}

// sourceLabel names an ActivitySource for people.
func sourceLabel(name string) string {
	switch name {
	case freshness.SourceV2Inbox:
		return "the v2 inbox"
	case freshness.SourceMessages:
		return "stored incoming messages"
	default:
		return name
	}
}

// activitySource picks what this session's silence is measured on, with the
// daemon's own picker (freshnessActivitySource): the v2 inbox when reads come
// from the v2 store, the legacy store's incoming messages otherwise. It reuses
// the stores openCommandReadSource already opened, so judging silence opens
// nothing new.
func (s *commandReadSession) activitySource() freshness.ActivitySource {
	legacy, _ := s.Reads.(*db.Store)
	return freshnessActivitySource(&v2Stack{Store: s.V2Store}, legacy, s.V2Store != nil)
}
