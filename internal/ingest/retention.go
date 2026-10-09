package ingest

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	"github.com/maxghenis/openmessage/internal/storage/sqlite"
	"github.com/rs/zerolog"
)

const (
	defaultPayloadPruneStartDelay = 2 * time.Minute
	defaultPayloadPruneInterval   = 24 * time.Hour
	defaultPayloadPruneBatchSize  = 500
	defaultPayloadPruneBatchPause = 25 * time.Millisecond
)

// PayloadPrunerConfig configures the daemon's inbox payload retention loop.
// Zero durations and sizes take the defaults; Retention is required.
type PayloadPrunerConfig struct {
	Messages  *sqlite.MessageRepository
	Logger    zerolog.Logger
	Retention time.Duration
	// StartDelay keeps the first pass clear of the startup drain and backfill.
	StartDelay time.Duration
	Interval   time.Duration
	BatchSize  int
	// BatchPause separates batches so the ingest worker can take the write
	// lock between them.
	BatchPause time.Duration
	// HoldPath, when set, names a file whose presence skips scheduled passes,
	// so an operator can stop pruning on a live install without a restart.
	HoldPath string
}

// PayloadPruner empties the payloads of processed inbox frames older than its
// retention, keeping every row and dedupe key (see
// sqlite.MessageRepository.PruneInboxPayloads).
type PayloadPruner struct {
	messages   *sqlite.MessageRepository
	logger     zerolog.Logger
	retention  time.Duration
	startDelay time.Duration
	interval   time.Duration
	batchSize  int
	batchPause time.Duration
	holdPath   string

	// afterPass observes each scheduled pass in tests.
	afterPass func(summary PayloadPruneSummary, held bool)
}

// PayloadPruneSummary totals one pruning pass.
type PayloadPruneSummary struct {
	CutoffMS int64
	Batches  int
	Rows     int
	Bytes    int64
}

// NewPayloadPruner validates config. A retention below
// sqlite.MinInboxPayloadRetention is rejected here rather than at the first
// pass.
func NewPayloadPruner(config PayloadPrunerConfig) (*PayloadPruner, error) {
	if config.Messages == nil {
		return nil, fmt.Errorf("create payload pruner: message repository is nil")
	}
	if config.Retention < sqlite.MinInboxPayloadRetention {
		return nil, fmt.Errorf(
			"create payload pruner: retention %s is shorter than the %s floor",
			config.Retention,
			sqlite.MinInboxPayloadRetention,
		)
	}
	if config.StartDelay < 0 || config.Interval < 0 || config.BatchSize < 0 || config.BatchPause < 0 {
		return nil, fmt.Errorf("create payload pruner: durations and batch size must not be negative")
	}
	if config.StartDelay == 0 {
		config.StartDelay = defaultPayloadPruneStartDelay
	}
	if config.Interval == 0 {
		config.Interval = defaultPayloadPruneInterval
	}
	if config.BatchSize == 0 {
		config.BatchSize = defaultPayloadPruneBatchSize
	}
	if config.BatchPause == 0 {
		config.BatchPause = defaultPayloadPruneBatchPause
	}
	return &PayloadPruner{
		messages:   config.Messages,
		logger:     config.Logger,
		retention:  config.Retention,
		startDelay: config.StartDelay,
		interval:   config.Interval,
		batchSize:  config.BatchSize,
		batchPause: config.BatchPause,
		holdPath:   config.HoldPath,
	}, nil
}

// held reports whether the hold file exists. A file that cannot be checked
// counts as a hold: pausing pruning is the safe failure.
func (p *PayloadPruner) held() bool {
	if p.holdPath == "" {
		return false
	}
	_, err := os.Stat(p.holdPath)
	if err == nil {
		return true
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false
	}
	p.logger.Warn().Err(err).Str("hold_path", p.holdPath).Msg("Cannot check the inbox retention hold file; skipping this pass")
	return true
}

// Run prunes once after the start delay and then once per interval until ctx
// ends. A failed pass is logged and retried at the next interval; a pass that
// finds the hold file is skipped.
func (p *PayloadPruner) Run(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("run payload pruner: context is nil")
	}
	timer := time.NewTimer(p.startDelay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
		}
		if p.held() {
			p.logger.Info().Str("hold_path", p.holdPath).Msg("V2 inbox payload pruning held; skipping this pass")
			if p.afterPass != nil {
				p.afterPass(PayloadPruneSummary{}, true)
			}
			timer.Reset(p.interval)
			continue
		}
		summary, err := p.PruneOnce(ctx)
		if p.afterPass != nil && ctx.Err() == nil {
			p.afterPass(summary, false)
		}
		switch {
		case ctx.Err() != nil:
			return nil
		case err != nil:
			p.logger.Warn().
				Err(err).
				Int("rows", summary.Rows).
				Int64("bytes", summary.Bytes).
				Msg("V2 inbox payload pruning failed; retrying next interval")
		case summary.Rows > 0:
			p.logger.Info().
				Int("rows", summary.Rows).
				Int64("bytes", summary.Bytes).
				Int("batches", summary.Batches).
				Int64("cutoff_ms", summary.CutoffMS).
				Dur("retention", p.retention).
				Msg("Pruned v2 inbox payloads past retention")
		}
		timer.Reset(p.interval)
	}
}

// PruneOnce prunes every eligible payload in batches, pausing between them,
// and returns the totals so far even when a batch fails or ctx ends.
func (p *PayloadPruner) PruneOnce(ctx context.Context) (PayloadPruneSummary, error) {
	var summary PayloadPruneSummary
	for {
		var batch sqlite.InboxPruneResult
		err := retryTransient(ctx, func() error {
			var err error
			batch, err = p.messages.PruneInboxPayloads(ctx, p.retention, p.batchSize)
			return err
		})
		if err != nil {
			return summary, err
		}
		summary.CutoffMS = batch.CutoffMS
		if batch.Rows == 0 {
			return summary, nil
		}
		summary.Batches++
		summary.Rows += batch.Rows
		summary.Bytes += batch.Bytes
		if batch.Rows < p.batchSize {
			return summary, nil
		}
		if err := waitForRetry(ctx, p.batchPause); err != nil {
			return summary, err
		}
	}
}
