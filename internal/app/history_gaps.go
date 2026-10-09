package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Google history gaps.
//
// The startup backfill and the recent reconcile page each conversation down to
// the newest message the legacy store already holds (its boundary), then store
// what they fetched oldest first. Paging is bounded (recentReconcileMaxPages).
// When a run stops before any page reaches the boundary, the messages between
// the boundary and the oldest message it fetched were not fetched, and once the
// fetched ones are stored the boundary sits above them, where no later
// reconcile looks. The run records that gap here rather than hiding it: a
// warning in the log, a record persisted in the data dir (a restart must not
// erase it, because the next startup backfill can no longer see the hole), and
// google.history_gaps in /api/status. A deep, window or phone backfill that
// pages the conversation from its newest message down to the gap's boundary
// (or until the phone has nothing older) fetches the whole gap, and clears its
// record.

// googleHistoryGapsFile holds the recorded gaps, in the data dir.
const googleHistoryGapsFile = "google-history-gaps.json"

// googleHistoryGapsListed bounds how many gaps /api/status lists; Count is the
// total.
const googleHistoryGapsListed = 20

// Reasons a catch-up stopped paging a conversation before reaching its
// boundary.
const (
	// googleHistoryGapPageLimit: recentReconcileMaxPages pages came back
	// without reaching the boundary.
	googleHistoryGapPageLimit = "page_limit"
	// googleHistoryGapFetchError: a later page failed to fetch.
	googleHistoryGapFetchError = "fetch_error"
	// googleHistoryGapNoOlderPage: asked for the messages below the oldest
	// fetched one, the phone served only messages already fetched (it does not
	// honour that request), so nothing below could be fetched.
	googleHistoryGapNoOlderPage = "no_older_page"
)

// GoogleHistoryGap is one conversation whose catch-up stored messages above a
// range it could not fetch: every message newer than AfterMS and older than
// BeforeMS may be missing from both stores.
type GoogleHistoryGap struct {
	ConversationID string `json:"conversation_id"`
	// AfterMS and AfterID are the boundary the catch-up paged toward: the
	// newest message the legacy store held before it ran.
	AfterMS int64  `json:"after_ms"`
	AfterID string `json:"after_id,omitempty"`
	// BeforeMS is the oldest message the catch-up stored (0 when none of them
	// had a timestamp).
	BeforeMS int64 `json:"before_ms"`
	// Stored is how many messages the catch-up stored above the gap.
	Stored     int    `json:"stored"`
	Reason     string `json:"reason"`
	Source     string `json:"source"`
	DetectedMS int64  `json:"detected_ms"`
}

// GoogleHistoryGapsSnapshot is the status view of the recorded gaps.
type GoogleHistoryGapsSnapshot struct {
	Count int `json:"count"`
	// SinceMS is the earliest gap's boundary. A window backfill from here
	// (POST /api/backfill {"since": <SinceMS>}, Unix milliseconds) pages every
	// gap's conversation down to its boundary, which clears the gap.
	SinceMS        int64 `json:"since_ms"`
	LastDetectedMS int64 `json:"last_detected_ms"`
	// Gaps lists up to googleHistoryGapsListed gaps, earliest boundary first.
	Gaps []GoogleHistoryGap `json:"gaps"`
}

type googleHistoryGaps struct {
	mu     sync.Mutex
	loaded bool
	byConv map[string]GoogleHistoryGap
}

func (a *App) googleHistoryGapsPath() string {
	if a.DataDir == "" {
		return ""
	}
	return filepath.Join(a.DataDir, googleHistoryGapsFile)
}

// loadLocked reads the persisted gaps once. A missing file is no gaps; an
// unreadable one is logged and treated as no gaps (it is rewritten at the next
// change).
func (a *App) loadGoogleHistoryGapsLocked() {
	g := &a.googleGaps
	if g.loaded {
		return
	}
	g.loaded = true
	g.byConv = map[string]GoogleHistoryGap{}
	path := a.googleHistoryGapsPath()
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err != nil {
		a.Logger.Warn().Err(err).Str("path", path).Msg("Failed to read recorded Google history gaps")
		return
	}
	var gaps []GoogleHistoryGap
	if err := json.Unmarshal(data, &gaps); err != nil {
		a.Logger.Warn().Err(err).Str("path", path).Msg("Recorded Google history gaps are unreadable; starting from none")
		return
	}
	for _, gap := range gaps {
		if gap.ConversationID != "" {
			g.byConv[gap.ConversationID] = gap
		}
	}
}

// sortedGoogleHistoryGapsLocked returns every recorded gap, earliest boundary
// first.
func (a *App) sortedGoogleHistoryGapsLocked() []GoogleHistoryGap {
	gaps := make([]GoogleHistoryGap, 0, len(a.googleGaps.byConv))
	for _, gap := range a.googleGaps.byConv {
		gaps = append(gaps, gap)
	}
	sort.Slice(gaps, func(i, j int) bool {
		if gaps[i].AfterMS != gaps[j].AfterMS {
			return gaps[i].AfterMS < gaps[j].AfterMS
		}
		return gaps[i].ConversationID < gaps[j].ConversationID
	})
	return gaps
}

// persistGoogleHistoryGapsLocked writes the gaps atomically (temp file, fsync,
// rename), or removes the file when there are none.
func (a *App) persistGoogleHistoryGapsLocked() {
	path := a.googleHistoryGapsPath()
	if path == "" {
		return
	}
	gaps := a.sortedGoogleHistoryGapsLocked()
	if len(gaps) == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			a.Logger.Warn().Err(err).Str("path", path).Msg("Failed to remove the recorded Google history gaps")
		}
		return
	}
	if err := writeFileAtomic(path, gaps); err != nil {
		a.Logger.Warn().Err(err).Str("path", path).Msg("Failed to persist Google history gaps; they are reported until the daemon restarts")
	}
}

func writeFileAtomic(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

// recordGoogleHistoryGap records that a catch-up stored messages of a
// conversation above a range it did not fetch. A conversation already
// recorded keeps one gap spanning both: the earlier boundary and the later
// oldest stored message, so a window backfill from its AfterMS covers both.
func (a *App) recordGoogleHistoryGap(gap GoogleHistoryGap) {
	if gap.DetectedMS == 0 {
		gap.DetectedMS = time.Now().UnixMilli()
	}
	a.googleGaps.mu.Lock()
	defer a.googleGaps.mu.Unlock()
	a.loadGoogleHistoryGapsLocked()
	if previous, ok := a.googleGaps.byConv[gap.ConversationID]; ok {
		if previous.AfterMS < gap.AfterMS {
			gap.AfterMS, gap.AfterID = previous.AfterMS, previous.AfterID
		}
		gap.BeforeMS = max(gap.BeforeMS, previous.BeforeMS)
		gap.Stored += previous.Stored
	}
	a.googleGaps.byConv[gap.ConversationID] = gap
	a.persistGoogleHistoryGapsLocked()
}

// clearGoogleHistoryGapCovered clears conversationID's gap when a backfill
// that paged the conversation from its newest message down, page after page,
// fetched the whole gap: it stored a message at or before the gap's boundary
// (oldestStoredMS, the oldest it stored), or the phone answered with an empty
// page after the ones it stored (exhausted: there is nothing older). A run that
// stopped above the boundary for any other reason (a page limit, a failed
// page, a phone that served a page again) leaves the gap recorded. It reports
// whether it cleared one.
func (a *App) clearGoogleHistoryGapCovered(conversationID string, oldestStoredMS int64, exhausted bool) bool {
	a.googleGaps.mu.Lock()
	defer a.googleGaps.mu.Unlock()
	a.loadGoogleHistoryGapsLocked()
	gap, ok := a.googleGaps.byConv[conversationID]
	if !ok {
		return false
	}
	if !exhausted && (oldestStoredMS <= 0 || oldestStoredMS > gap.AfterMS) {
		return false
	}
	delete(a.googleGaps.byConv, conversationID)
	a.persistGoogleHistoryGapsLocked()
	a.Logger.Info().
		Str("conv_id", conversationID).
		Int64("after_ms", gap.AfterMS).
		Int64("before_ms", gap.BeforeMS).
		Int64("oldest_stored_ms", oldestStoredMS).
		Bool("exhausted", exhausted).
		Msg("Google history gap filled by a backfill")
	return true
}

// GoogleHistoryGaps reports the recorded gaps, or nil when there are none.
func (a *App) GoogleHistoryGaps() *GoogleHistoryGapsSnapshot {
	a.googleGaps.mu.Lock()
	defer a.googleGaps.mu.Unlock()
	a.loadGoogleHistoryGapsLocked()
	gaps := a.sortedGoogleHistoryGapsLocked()
	if len(gaps) == 0 {
		return nil
	}
	snap := &GoogleHistoryGapsSnapshot{Count: len(gaps), SinceMS: gaps[0].AfterMS}
	for _, gap := range gaps {
		snap.LastDetectedMS = max(snap.LastDetectedMS, gap.DetectedMS)
	}
	snap.Gaps = gaps[:min(len(gaps), googleHistoryGapsListed)]
	return snap
}

// googleHistoryGapRecovery is the request that fills a gap whose boundary is at
// afterMS, for logs.
func googleHistoryGapRecovery(afterMS int64) string {
	return fmt.Sprintf(`POST /api/backfill {"since":%d}`, afterMS)
}
