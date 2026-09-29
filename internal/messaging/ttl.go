package messaging

import (
	"fmt"
	"math"
	"time"
)

// MaxTTL is the longest send window any surface accepts (HTTP ttl_ms, MCP
// ttl_seconds, OPENMESSAGES_SEND_TTL_SECONDS, and CommonCommand.TTL).
const MaxTTL = 24 * time.Hour

// maxTTLMilliseconds is MaxTTL in whole milliseconds (86,400,000).
const maxTTLMilliseconds = int64(MaxTTL / time.Millisecond)

// InvalidTTLError reports a send window rejected by the shared TTL validator.
// Reason is a predicate phrase ("must not exceed 86400 (24 hours)") so each
// surface can prefix the name of the field it parsed.
type InvalidTTLError struct {
	Reason string
}

func (e *InvalidTTLError) Error() string {
	return fmt.Sprintf("%v: TTL %s", ErrInvalidCommand, e.Reason)
}

func (e *InvalidTTLError) Unwrap() error { return ErrInvalidCommand }

func invalidTTL(reason string) error { return &InvalidTTLError{Reason: reason} }

// ValidateTTL accepts 0 (the intent never expires) or a window from 1ms to
// MaxTTL. A positive window below 1ms is rejected rather than rounded: stored
// expiry has millisecond resolution, so such a window would either be born
// expired or, after truncation to 0 on a millisecond wire field, never
// expire.
func ValidateTTL(ttl time.Duration) error {
	switch {
	case ttl == 0:
		return nil
	case ttl < 0:
		return invalidTTL("must not be negative")
	case ttl < time.Millisecond:
		return invalidTTL("must be 0 (never expire) or at least 1 millisecond")
	case ttl > MaxTTL:
		return invalidTTL(fmt.Sprintf("must not exceed %s", MaxTTL))
	}
	return nil
}

// TTLFromMilliseconds converts a millisecond send window (HTTP ttl_ms) to a
// Duration. Bounds are checked before multiplying, so no input can wrap:
// without the check, 2^58 ms times 10^6 ns/ms is 0 mod 2^64, which would turn
// a positive window into "never expire".
func TTLFromMilliseconds(ms int64) (time.Duration, error) {
	if ms < 0 {
		return 0, invalidTTL("must not be negative")
	}
	if ms > maxTTLMilliseconds {
		return 0, invalidTTL(fmt.Sprintf("must not exceed %d milliseconds (24 hours)", maxTTLMilliseconds))
	}
	return time.Duration(ms) * time.Millisecond, nil
}

// TTLFromSeconds converts a fractional-second send window (MCP ttl_seconds,
// OPENMESSAGES_SEND_TTL_SECONDS) to a Duration. Every range check runs on the
// float64 before any integer conversion, because an out-of-range or NaN float
// to integer conversion is implementation-defined in Go: on arm64 it
// saturates (NaN gives 0, i.e. "never expire"), and on amd64 every such input
// gives math.MinInt64. An accepted positive value is rounded to
// the nearest millisecond, so it is identical after the daemon path's
// Milliseconds() round trip and identical on every GOARCH.
func TTLFromSeconds(seconds float64) (time.Duration, error) {
	switch {
	case math.IsNaN(seconds) || math.IsInf(seconds, 0):
		return 0, invalidTTL("must be a finite number")
	case seconds < 0:
		return 0, invalidTTL("must not be negative")
	case seconds == 0:
		return 0, nil
	case seconds < 0.001:
		return 0, invalidTTL("must be 0 (never expire) or at least 0.001 (1 millisecond)")
	case seconds > MaxTTL.Seconds():
		return 0, invalidTTL(fmt.Sprintf("must not exceed %d (24 hours)", int64(MaxTTL/time.Second)))
	}
	// 1 <= round(seconds*1000) <= 86,400,000, so the conversion is exact.
	return time.Duration(math.Round(seconds*1000)) * time.Millisecond, nil
}
