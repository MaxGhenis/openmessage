package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
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
// freshness.<platform>.silence object of /api/status, kept as JSON so the
// daemon's verdict is shown byte for byte.
type platformSilence struct {
	Block    json.RawMessage
	JudgedBy string
	// SilentMS and Stalled are read back from Block for the text output.
	SilentMS int64
	Stalled  bool
	source   string
}

// statusSilence holds the verdicts by status platform key ("google",
// "whatsapp", "signal") and one sentence saying who judged them.
type statusSilence struct {
	byPlatform map[string]platformSilence
	note       string
}

// judgeStatusSilence returns a silence verdict for each status platform in
// platforms. A platform takes the running app's verdict when the app serves
// this data dir and reports one in freshness.<platform>.silence (daemon
// truth, as for sends). Otherwise it is judged here with the same rule and
// config (freshness.EvaluateSilence, DefaultSilenceConfig) over the same
// activity source the daemon would read: the v2 inbox when reads come from
// the v2 store, the legacy store's incoming messages otherwise.
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
	source := session.activitySource()
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
		}
	}
	out.note = silenceNote(out.byPlatform, localSource, whyLocal, localErr)
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
		return nil, "the app isn't running"
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
	blocks := map[string]json.RawMessage{}
	otherSource := ""
	for platform, raw := range status.Freshness {
		var entry struct {
			Silence json.RawMessage `json:"silence"`
		}
		// Top-level scalars (newest_ms, silence_stalled) aren't objects.
		if json.Unmarshal(raw, &entry) != nil {
			continue
		}
		block := entry.Silence
		if len(block) == 0 || string(block) == "null" {
			continue
		}
		// On a v2-primary install the app measures the v2 inbox, but this
		// command reads the legacy store unless OPENMESSAGES_V2_PRIMARY=1 is
		// set; a verdict on one says nothing about the other (the two diverge
		// when the projection stalls, #155).
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

func newPlatformSilence(block json.RawMessage, judgedBy string) platformSilence {
	var view struct {
		Source   string `json:"source"`
		SilentMS int64  `json:"silent_ms"`
		Stalled  bool   `json:"stalled"`
	}
	_ = json.Unmarshal(block, &view)
	return platformSilence{
		Block:    block,
		JudgedBy: judgedBy,
		SilentMS: view.SilentMS,
		Stalled:  view.Stalled,
		source:   view.Source,
	}
}

// silenceNote is the sentence under the status table that says who judged
// the silence verdicts, from what, and why any were judged locally:
//
//	Silence judged by the running app from the v2 inbox.
//	Silence judged locally from the v2 inbox (the app isn't running).
//	Silence: google judged by the running app from the v2 inbox; signal judged locally from the v2 inbox (the running app reported none).
func silenceNote(byPlatform map[string]platformSilence, localSource, whyLocal string, localErr error) string {
	var daemon, local []string
	daemonSource := ""
	for platform, verdict := range byPlatform {
		if verdict.JudgedBy == silenceJudgedByDaemon {
			daemon = append(daemon, platform)
			daemonSource = verdict.source
		} else {
			local = append(local, platform)
		}
	}
	sort.Strings(daemon)
	sort.Strings(local)
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

	switch {
	case len(daemon) == 0 && len(local) == 0:
		if localErr == nil {
			return ""
		}
		return "Silence not judged" + reason(whyLocal) + "."
	case len(local) == 0 && localErr == nil:
		return "Silence judged by the running app" + fromSource(daemonSource) + "."
	case len(daemon) == 0:
		return "Silence judged locally" + fromSource(localSource) + reason(whyLocal) + "."
	}
	parts := []string{strings.Join(daemon, ", ") + " judged by the running app" + fromSource(daemonSource)}
	if len(local) > 0 {
		parts = append(parts, strings.Join(local, ", ")+" judged locally"+fromSource(localSource)+reason(whyLocal))
	} else {
		parts = append(parts, "the rest not judged"+reason(whyLocal))
	}
	return "Silence: " + strings.Join(parts, "; ") + "."
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
