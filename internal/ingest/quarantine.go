package ingest

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/maxghenis/openmessage/internal/storage/sqlite"
	moderncsqlite "modernc.org/sqlite"
)

const maxTransientRetries = 3

var transientBackoffs = [...]time.Duration{
	5 * time.Millisecond,
	10 * time.Millisecond,
	20 * time.Millisecond,
}

type quarantineError struct{ cause error }

func (e quarantineError) Error() string { return e.cause.Error() }
func (e quarantineError) Unwrap() error { return e.cause }

type staleReplayError struct{ cause error }

func (e staleReplayError) Error() string { return e.cause.Error() }
func (e staleReplayError) Unwrap() error { return e.cause }

type transientExhaustedError struct{ cause error }

func (e transientExhaustedError) Error() string { return e.cause.Error() }
func (e transientExhaustedError) Unwrap() error { return e.cause }

func quarantine(cause error) error {
	return quarantineError{cause: cause}
}

func classifyApplyError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, sqlite.ErrInboxProjectionConflict) {
		return staleReplayError{cause: err}
	}
	if isTransientDBError(err) {
		return err
	}
	return quarantine(err)
}

func isTransientDBError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var sqliteErr *moderncsqlite.Error
	if !errors.As(err, &sqliteErr) {
		return false
	}
	switch sqliteErr.Code() & 0xff {
	case 5, 6: // SQLITE_BUSY, SQLITE_LOCKED (including extended result codes).
		return true
	default:
		return false
	}
}

func waitForRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func retryTransient(ctx context.Context, operation func() error) error {
	var err error
	for attempt := 0; attempt <= maxTransientRetries; attempt++ {
		err = operation()
		var deterministic quarantineError
		var stale staleReplayError
		if errors.As(err, &deterministic) || errors.As(err, &stale) {
			return err
		}
		if err == nil || !isTransientDBError(err) {
			return err
		}
		if attempt == maxTransientRetries {
			break
		}
		if waitErr := waitForRetry(ctx, transientBackoffs[attempt]); waitErr != nil {
			return waitErr
		}
	}
	return transientExhaustedError{cause: fmt.Errorf(
		"transient database failure after %d retries: %w",
		maxTransientRetries,
		err,
	)}
}
