package cmd

import (
	"context"
	"runtime/debug"
	"time"

	"github.com/rs/zerolog"

	"github.com/maxghenis/openmessage/internal/db"
)

const (
	// The first pass waits out startup's own writes; later passes run often
	// enough that a burst of deletes is back on disk within the quarter hour.
	legacyReclaimFirstDelay = 3 * time.Minute
	legacyReclaimInterval   = 15 * time.Minute
)

// legacyFreePageReclaimer returns the legacy store's free pages to the
// filesystem once a store is in incremental auto-vacuum mode, which new
// stores are and openmessage compact makes old ones. Deletes leave pages on
// SQLite's freelist; later writes reuse them, but the file never shrinks on
// its own, so one burst of deletes or a rebuilt search index used to leave
// the file at its largest size.
//
// A pass starts once the freelist holds minFree bytes and returns them in
// steps of step bytes, pausing between steps: each step is one write
// transaction, so the daemon's own writers and other processes wait at most
// one step for the lock.
type legacyFreePageReclaimer struct {
	store   *db.Store
	logger  zerolog.Logger
	minFree int64
	step    int64
	pause   time.Duration
	// warnFree is how much a store without auto-vacuum must hold before the
	// reclaimer says, once, to run openmessage compact.
	warnFree int64
	warned   bool
}

func newLegacyFreePageReclaimer(store *db.Store, logger zerolog.Logger) *legacyFreePageReclaimer {
	return &legacyFreePageReclaimer{
		store:    store,
		logger:   logger,
		minFree:  16 << 20,
		step:     4 << 20,
		pause:    200 * time.Millisecond,
		warnFree: 64 << 20,
	}
}

// run makes a pass after firstDelay and then every interval until ctx ends.
func (r *legacyFreePageReclaimer) run(ctx context.Context, firstDelay, interval time.Duration) {
	timer := time.NewTimer(firstDelay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		r.safePass(ctx)
		timer.Reset(interval)
	}
}

func (r *legacyFreePageReclaimer) safePass(ctx context.Context) {
	defer func() {
		if p := recover(); p != nil {
			r.logger.Error().
				Interface("panic", p).
				Bytes("stack", debug.Stack()).
				Msg("legacy store free-page reclaim panicked; will retry next interval")
		}
	}()
	if _, err := r.pass(ctx); err != nil && ctx.Err() == nil {
		r.logger.Warn().Err(err).Msg("legacy store free-page reclaim failed; will retry next interval")
	}
}

// pass returns the free pages and reports how many bytes left the freelist.
func (r *legacyFreePageReclaimer) pass(ctx context.Context) (int64, error) {
	stats, err := r.store.StorageStats(ctx)
	if err != nil {
		return 0, err
	}
	if stats.AutoVacuum != db.AutoVacuumIncremental {
		if !r.warned && stats.FreeBytes() >= r.warnFree {
			r.warned = true
			r.logger.Warn().
				Int64("free_bytes", stats.FreeBytes()).
				Str("auto_vacuum", stats.AutoVacuum.String()).
				Msg("the legacy store holds free pages that only a rewrite can return; quit the app and run `openmessage compact --apply`")
		}
		return 0, nil
	}
	if stats.PageSize <= 0 || stats.FreeBytes() < r.minFree {
		return 0, nil
	}
	stepPages := max(r.step/stats.PageSize, 1)
	var freedPages int64
	for {
		freed, err := r.store.ReclaimFreePages(ctx, stepPages)
		freedPages += freed
		if err != nil {
			return freedPages * stats.PageSize, err
		}
		// A short step emptied the freelist.
		if freed < stepPages {
			break
		}
		select {
		case <-ctx.Done():
			return freedPages * stats.PageSize, ctx.Err()
		case <-time.After(r.pause):
		}
	}
	if freedPages > 0 {
		r.logger.Info().
			Int64("freed_bytes", freedPages*stats.PageSize).
			Msg("returned the legacy store's free pages to the filesystem")
	}
	return freedPages * stats.PageSize, nil
}
