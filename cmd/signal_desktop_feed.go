package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/freshness"
	"github.com/maxghenis/openmessage/internal/ingest"
	"github.com/maxghenis/openmessage/internal/signaldesktop"
	"github.com/maxghenis/openmessage/internal/signallive"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

const (
	// The first scan waits for startup to settle and for WhatsApp Desktop's
	// first scan (20 s), so the two node and SQLite reads do not overlap.
	signalDesktopInitialDelay = 40 * time.Second
	signalDesktopScanInterval = 30 * time.Second
	signalDesktopFullInterval = 6 * time.Hour
	signalDesktopOfflineDelay = 5 * time.Minute
	// signalDesktopRetryDelay spaces attempts after a failed scan. Reading the
	// key can raise a macOS keychain prompt, so a failure must never turn
	// into a prompt every 30 seconds.
	signalDesktopRetryDelay = 30 * time.Minute
	signalDesktopSilenceTTL = 5 * time.Minute
	signalDesktopChunkSize  = signaldesktop.MaxChunkMessages
)

// Gate reasons reported in /api/status.signal_desktop.gate.
const (
	signalDesktopGateUnpaired       = "live_unpaired"
	signalDesktopGateOffline        = "live_offline"
	signalDesktopGateOfflineWaiting = "live_offline_waiting"
	signalDesktopGateSilent         = "live_silent"
	signalDesktopGateReceiving      = "live_receiving"
)

// signalDesktopFeedStatus describes the history feed only. It never changes
// the meaning of the signal block (signal-cli's pairing and connection) or of
// the live ingest counters.
type signalDesktopFeedStatus struct {
	Enabled             bool             `json:"enabled"`
	Available           bool             `json:"available"`
	Active              bool             `json:"active"`
	Gate                string           `json:"gate"`
	AccountVerified     bool             `json:"account_verified"`
	DBPath              string           `json:"db_path"`
	LastScanAtMS        int64            `json:"last_scan_at_ms"`
	LastFullScanAtMS    int64            `json:"last_full_scan_at_ms"`
	HighWaterRowID      int64            `json:"high_water_rowid"`
	FramesAppended      uint64           `json:"frames_appended"`
	MessagesAppended    uint64           `json:"messages_appended"`
	RowsIncluded        int64            `json:"rows_included"`
	RowsInV2            int64            `json:"rows_in_v2"`
	RowsSkippedByReason map[string]int64 `json:"rows_skipped_by_reason"`
	NextAttemptAtMS     int64            `json:"next_attempt_at_ms,omitempty"`
	LastError           string           `json:"last_error"`
}

// signalDesktopGate decides whether Desktop history may be appended. The
// live signal-cli bridge owns the account's rows whenever it is up and
// receiving; Desktop fills in while it is unpaired, offline for five
// continuous minutes, or connected but silent past its own baseline (the
// /api/status silence rule), which is how a remotely unlinked device looks.
type signalDesktopGate struct {
	mu             sync.Mutex
	disconnectedAt time.Time
}

// observe records a live status transition, so a brief reconnect between
// scans restarts the offline window.
func (g *signalDesktopGate) observe(status signallive.StatusSnapshot, now time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.observeLocked(status, now)
}

func (g *signalDesktopGate) observeLocked(status signallive.StatusSnapshot, now time.Time) {
	if !status.Paired || status.Connected {
		g.disconnectedAt = time.Time{}
		return
	}
	if g.disconnectedAt.IsZero() {
		g.disconnectedAt = now
	}
}

func (g *signalDesktopGate) decide(status signallive.StatusSnapshot, liveSilent bool, now time.Time) (bool, string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.observeLocked(status, now)
	switch {
	case !status.Paired:
		return true, signalDesktopGateUnpaired
	case status.Connected && liveSilent:
		return true, signalDesktopGateSilent
	case status.Connected:
		return false, signalDesktopGateReceiving
	case now.Sub(g.disconnectedAt) >= signalDesktopOfflineDelay:
		return true, signalDesktopGateOffline
	default:
		return false, signalDesktopGateOfflineWaiting
	}
}

// signalLiveSilenceProbe reports whether the live Signal channel has been
// silent past its baseline, by the same rule as freshness.signal.silence. It
// reads only live codecs (see freshnessActivitySource), so Desktop history
// can never make the live channel look alive or dead. A platform with no
// live delivery at all, or a failed query, counts as not silent: the gate
// stays closed unless there is evidence of a stall.
func signalLiveSilenceProbe(source freshness.ActivitySource) func(time.Time) bool {
	if source == nil {
		return func(time.Time) bool { return false }
	}
	var mu sync.Mutex
	var checkedAt time.Time
	var silent bool
	return func(now time.Time) bool {
		mu.Lock()
		defer mu.Unlock()
		if !checkedAt.IsZero() && now.Sub(checkedAt) < signalDesktopSilenceTTL && now.After(checkedAt) {
			return silent
		}
		checkedAt = now
		silent = false
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		latest, err := source.Latest(ctx)
		if err != nil {
			return false
		}
		last := latest["signal"]
		if last.IsZero() {
			return false
		}
		config := freshness.DefaultSilenceConfig
		from, to := freshness.BaselineRange(last, time.Local, config)
		events, err := source.Between(ctx, "signal", from, to)
		if err != nil {
			return false
		}
		silent = freshness.EvaluateSilence(last, events, now, time.Local, config).Stalled
		return silent
	}
}

type signalDesktopScanFunc func(context.Context, *signaldesktop.Facts, signaldesktop.ScanOptions, func(signaldesktop.Chunk) error) (signaldesktop.Stats, error)

type signalDesktopFeed struct {
	mu     sync.RWMutex
	scanMu sync.Mutex
	status signalDesktopFeedStatus

	gate       *signalDesktopGate
	live       func() signallive.StatusSnapshot
	liveSilent func(time.Time) bool
	scan       signalDesktopScanFunc
	facts      func(context.Context) (*signaldesktop.Facts, error)
	stamp      func() (string, error)
	sink       bridge.HistoryIngressSink
	logger     zerolog.Logger
	logged     map[string]bool
	initErr    error

	lastStamp string
	retryAt   time.Time
	key       *signalDesktopKeyCache
}

type signalDesktopFeedDeps struct {
	stack      *v2Stack
	transports bool
	demo       bool
	gate       *signalDesktopGate
	live       func() signallive.StatusSnapshot
	liveSilent func(time.Time) bool
	supportDir string
	logger     zerolog.Logger
}

func signalDesktopFeedEnabled(stack *v2Stack, transports, demo bool) bool {
	return stack != nil && transports && !demo && !v2FlagExplicitlyDisabled("OPENMESSAGES_SIGNAL_DESKTOP_FEED")
}

func newSignalDesktopFeed(deps signalDesktopFeedDeps) *signalDesktopFeed {
	if deps.gate == nil {
		deps.gate = &signalDesktopGate{}
	}
	if deps.liveSilent == nil {
		deps.liveSilent = func(time.Time) bool { return false }
	}
	if deps.supportDir == "" {
		deps.supportDir = signaldesktop.DefaultSupportDir()
	}
	dbPath := signaldesktop.DBPath(deps.supportDir)
	f := &signalDesktopFeed{
		status: signalDesktopFeedStatus{
			Enabled:             signalDesktopFeedEnabled(deps.stack, deps.transports, deps.demo),
			DBPath:              dbPath,
			RowsSkippedByReason: map[string]int64{},
		},
		gate: deps.gate, live: deps.live, liveSilent: deps.liveSilent,
		logger: deps.logger, logged: map[string]bool{},
	}
	if !f.status.Enabled {
		return f
	}
	// Like WhatsApp Desktop, this does not follow app.Sandboxed(): the
	// launcher's OPENMESSAGES_APP_SANDBOX=1 never meant a real sandbox, and
	// reading the database is the actual permission check.
	if err := ensureSignalDesktopAccount(deps.stack); err != nil {
		f.initErr = err
		f.recordError(err)
		return f
	}
	f.sink = deps.stack.Sink
	f.facts = func(ctx context.Context) (*signaldesktop.Facts, error) {
		return signaldesktop.LoadFacts(ctx, deps.stack.Store, signalAccountID)
	}
	f.stamp = func() (string, error) { return signalDesktopFileStamp(dbPath) }
	f.key = &signalDesktopKeyCache{derive: func(ctx context.Context) (string, error) {
		return signaldesktop.SQLKey(ctx, deps.supportDir)
	}}
	f.scan = func(ctx context.Context, facts *signaldesktop.Facts, options signaldesktop.ScanOptions, emit func(signaldesktop.Chunk) error) (signaldesktop.Stats, error) {
		node, err := signaldesktop.NodePath()
		if err != nil {
			return signaldesktop.Stats{}, err
		}
		addon, err := signaldesktop.AddonPath()
		if err != nil {
			return signaldesktop.Stats{}, err
		}
		runner := &signaldesktop.NodeRunner{DBPath: dbPath, NodePath: node, AddonPath: addon, Key: f.key.get}
		stats, err := (&signaldesktop.Reader{Runner: runner}).Scan(ctx, facts, options, emit)
		if err != nil && strings.Contains(err.Error(), "file is not a database") {
			// The key no longer opens the database (Signal re-keyed it).
			// Derive it again on the next attempt.
			f.key.invalidate()
		}
		return stats, err
	}
	return f
}

// The feed appends to the account live ingress uses, including when the live
// adapter failed before it could bootstrap that account.
func ensureSignalDesktopAccount(stack *v2Stack) error {
	if _, err := stack.Store.GetAccount(signalAccountID); err == nil {
		return nil
	} else if !errors.Is(err, sqlite.ErrNotFound) {
		return fmt.Errorf("Signal Desktop feed: read account: %w", err)
	}
	spec, err := liveAccountSpec(bridge.PlatformSignal)
	if err != nil {
		return err
	}
	nowMS := time.Now().UnixMilli()
	return stack.Store.UpsertAccount(sqlite.Account{
		AccountID: signalAccountID, BridgeKey: spec.bridgeKey, DisplayName: spec.displayName,
		Mode: sqlite.AccountModeLive, Enabled: true, ConfigJSON: "{}",
		CreatedAtMS: nowMS, UpdatedAtMS: nowMS,
	})
}

// signalDesktopFileStamp changes whenever Signal Desktop writes: the
// database and its WAL, by size and modification time. It is also the
// readability check.
func signalDesktopFileStamp(dbPath string) (string, error) {
	file, err := os.Open(dbPath)
	if err != nil {
		return "", fmt.Errorf("Signal Desktop database unavailable: %w", err)
	}
	info, err := file.Stat()
	closeErr := file.Close()
	if err = errors.Join(err, closeErr); err != nil {
		return "", fmt.Errorf("Signal Desktop database unavailable: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("Signal Desktop database unavailable: not a regular file")
	}
	stamp := fmt.Sprintf("%d:%d", info.Size(), info.ModTime().UnixNano())
	if wal, err := os.Stat(dbPath + "-wal"); err == nil {
		stamp += fmt.Sprintf("|%d:%d", wal.Size(), wal.ModTime().UnixNano())
	}
	return stamp, nil
}

// signalDesktopKeyCache derives the SQLCipher key once and keeps it for the
// daemon's life. After a failure it waits signalDesktopRetryDelay before
// asking the keychain again.
type signalDesktopKeyCache struct {
	mu       sync.Mutex
	key      string
	failedAt time.Time
	lastErr  error
	derive   func(context.Context) (string, error)
	now      func() time.Time
}

func (c *signalDesktopKeyCache) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

func (c *signalDesktopKeyCache) get(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.key != "" {
		return c.key, nil
	}
	if !c.failedAt.IsZero() && c.clock().Sub(c.failedAt) < signalDesktopRetryDelay {
		return "", fmt.Errorf("%w (not retried until %s)", c.lastErr, c.failedAt.Add(signalDesktopRetryDelay).Format(time.RFC3339))
	}
	key, err := c.derive(ctx)
	if err != nil {
		c.failedAt = c.clock()
		c.lastErr = err
		return "", err
	}
	c.key = key
	c.failedAt = time.Time{}
	return key, nil
}

func (c *signalDesktopKeyCache) invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.key = ""
}

// Snapshot is the /api/status.signal_desktop block.
func (f *signalDesktopFeed) Snapshot() signalDesktopFeedStatus {
	f.mu.RLock()
	defer f.mu.RUnlock()
	snapshot := f.status
	snapshot.RowsSkippedByReason = make(map[string]int64, len(f.status.RowsSkippedByReason))
	for reason, count := range f.status.RowsSkippedByReason {
		snapshot.RowsSkippedByReason[reason] = count
	}
	return snapshot
}

func (f *signalDesktopFeed) recordError(err error) {
	if err == nil {
		return
	}
	message := err.Error()
	f.mu.Lock()
	f.status.LastError = message
	f.status.Active = false
	logged := f.logged[message]
	f.logged[message] = true
	f.mu.Unlock()
	if !logged {
		f.logger.Warn().Err(err).Msg("Signal Desktop history feed unavailable")
	}
}

var errSignalDesktopLiveServing = errors.New("Signal live bridge resumed during the Desktop scan")

// observeLive records a live status change from the Signal callback.
func (f *signalDesktopFeed) observeLive(now time.Time) {
	if f == nil || f.live == nil || !f.Snapshot().Enabled {
		return
	}
	f.gate.observe(f.live(), now)
}

func (f *signalDesktopFeed) allowed(now time.Time) (bool, string) {
	status := f.live()
	silent := status.Paired && status.Connected && f.liveSilent(now)
	return f.gate.decide(status, silent, now)
}

func (f *signalDesktopFeed) scanOnce(ctx context.Context, now time.Time) {
	f.scanMu.Lock()
	defer f.scanMu.Unlock()
	snapshot := f.Snapshot()
	if !snapshot.Enabled || f.initErr != nil {
		return
	}
	ok, reason := f.allowed(now)
	f.mu.Lock()
	f.status.Gate = reason
	if !ok {
		f.status.Active = false
	}
	f.mu.Unlock()
	if !ok {
		return
	}
	if now.Before(f.retryAt) {
		return
	}
	stamp, err := f.stamp()
	if err != nil {
		f.mu.Lock()
		f.status.Available = false
		f.mu.Unlock()
		f.recordError(err)
		return
	}
	full := snapshot.LastFullScanAtMS == 0 || now.Sub(time.UnixMilli(snapshot.LastFullScanAtMS)) >= signalDesktopFullInterval
	if !full && stamp == f.lastStamp {
		// Signal Desktop has written nothing since the last scan.
		f.mu.Lock()
		f.status.Active = true
		f.mu.Unlock()
		return
	}
	fail := func(err error) {
		f.retryAt = now.Add(signalDesktopRetryDelay)
		f.mu.Lock()
		f.status.NextAttemptAtMS = f.retryAt.UnixMilli()
		f.mu.Unlock()
		f.recordError(err)
	}
	facts, err := f.facts(ctx)
	if err != nil {
		fail(err)
		return
	}
	afterRowID := snapshot.HighWaterRowID
	if full {
		afterRowID = 0
	}
	f.mu.Lock()
	f.status.Active = true
	f.status.LastScanAtMS = now.UnixMilli()
	f.mu.Unlock()
	var emitErr error
	stats, err := f.scan(ctx, facts, signaldesktop.ScanOptions{AfterRowID: afterRowID, ChunkSize: signalDesktopChunkSize}, func(chunk signaldesktop.Chunk) error {
		if err := ctx.Err(); err != nil {
			emitErr = err
			return err
		}
		// Re-check before each durable append, so a reconnect during a long
		// full scan stops Desktop history before its next frame.
		if ok, _ := f.allowed(time.Now()); !ok {
			emitErr = errSignalDesktopLiveServing
			return emitErr
		}
		// Generation 0: the feed owns no supervisor generation, and the
		// direct history append records no supervisor activity, so a Desktop
		// frame is never live-delivery evidence.
		record, err := ingest.SignalDesktopHistoryRecord(signalAccountID, 0, chunk.HistorySnapshot(facts.SelfE164), now)
		if err != nil {
			emitErr = err
			return err
		}
		if err := f.sink.AppendHistoryIngress(ctx, record); err != nil {
			emitErr = err
			return err
		}
		f.mu.Lock()
		f.status.FramesAppended++
		f.status.MessagesAppended += uint64(len(chunk.Messages))
		f.mu.Unlock()
		return nil
	})
	f.mu.Lock()
	f.status.AccountVerified = stats.AccountVerified
	f.status.RowsIncluded = stats.RowsIncluded
	f.status.RowsInV2 = stats.RowsInV2
	if stats.RowsSkippedByReason != nil {
		f.status.RowsSkippedByReason = stats.RowsSkippedByReason
	}
	switch {
	case err == nil:
		f.status.Available = true
		f.status.LastError = ""
		f.status.NextAttemptAtMS = 0
		f.status.HighWaterRowID = stats.HighWaterRowID
		if stats.MaxRowID < afterRowID {
			// The archive's rows end below the mark: Desktop was re-linked or
			// its database replaced. Start over with a full scan.
			f.status.HighWaterRowID = 0
			f.status.LastFullScanAtMS = 0
		} else if full {
			f.status.LastFullScanAtMS = now.UnixMilli()
		}
		f.lastStamp = stamp
	case errors.Is(err, errSignalDesktopLiveServing):
		f.status.Active = false
	case emitErr == nil && !errors.Is(err, context.Canceled):
		// The read failed, not the append: the source is unavailable.
		f.status.Available = false
	}
	f.mu.Unlock()
	switch {
	case err == nil, errors.Is(err, errSignalDesktopLiveServing), errors.Is(err, context.Canceled):
	case emitErr != nil:
		// An append failed: v2, not Desktop. Try again on the next tick.
		f.recordError(err)
	default:
		fail(err)
	}
}

func (f *signalDesktopFeed) Start(ctx context.Context) func() {
	if !f.Snapshot().Enabled || f.initErr != nil {
		return func() {}
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		initial := time.NewTimer(signalDesktopInitialDelay)
		defer initial.Stop()
		select {
		case <-runCtx.Done():
			return
		case now := <-initial.C:
			f.scanOnce(runCtx, now)
		}
		ticker := time.NewTicker(signalDesktopScanInterval)
		defer ticker.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case now := <-ticker.C:
				f.scanOnce(runCtx, now)
			}
		}
	}()
	return func() { cancel(); <-done }
}
