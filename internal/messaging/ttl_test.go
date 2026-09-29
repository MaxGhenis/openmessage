package messaging

// The shared TTL validator (ttl.go). Invariants executed here:
//
//	I7 any accepted positive ttl_ms or ttl_seconds yields 1ms <= d <= MaxTTL on
//	   every GOARCH; ttl_seconds results are millisecond-aligned and within
//	   0.5ms of the request; 0 means no expiry; NaN, ±Inf, negative, over-max,
//	   and positive sub-millisecond inputs are rejected before any integer
//	   conversion or multiplication.
//	I8 an accepted TTL > 0 gives expires_at_ms >= scheduled_for_ms + 1.
//	I9 the daemon path (seconds -> Milliseconds() -> ttl_ms) and the in-process
//	   path stamp the same window.

import (
	"errors"
	"math"
	"math/rand"
	"reflect"
	"testing"
	"testing/quick"
	"time"
)

// ttlSecondsInput is a generated ttl_seconds value biased toward the edges
// of the accepted range and toward inputs whose float-to-integer conversion
// is implementation-defined.
type ttlSecondsInput float64

var ttlSecondsEdges = []float64{
	0, math.Copysign(0, -1), 0.001, math.Nextafter(0.001, 0), math.Nextafter(0.001, 1), 0.0009999999,
	0.0005, 0.0015, 1e-10, 5e-324, 86400, math.Nextafter(86400, 0), math.Nextafter(86400, math.Inf(1)),
	86400.0000001, 1e10, 1e300, math.MaxFloat64, -1, -1e-10, math.NaN(), math.Inf(1), math.Inf(-1),
	9.223372036854776e9, 2.8823037615171174e14,
}

func (ttlSecondsInput) Generate(r *rand.Rand, _ int) reflect.Value {
	var value float64
	switch r.Intn(6) {
	case 0:
		value = math.Float64frombits(r.Uint64()) // any bit pattern: NaN, Inf, subnormal
	case 1:
		value = r.Float64() * 0.002 // around the 1ms floor
	case 2:
		value = r.Float64() * 90_000 // around the 24h ceiling
	case 3:
		value = (r.Float64() - 0.5) * 1e20
	case 4:
		value = ttlSecondsEdges[r.Intn(len(ttlSecondsEdges))]
	default:
		value = float64(r.Intn(86_401))
	}
	return reflect.ValueOf(ttlSecondsInput(value))
}

// ttlMillisecondsInput is a generated ttl_ms value biased toward the 24h
// ceiling and toward the values that wrap when multiplied into nanoseconds.
type ttlMillisecondsInput int64

var ttlMillisecondsEdges = []int64{
	0, 1, -1, 999, 1_000, maxTTLMilliseconds - 1, maxTTLMilliseconds, maxTTLMilliseconds + 1,
	1 << 58, 1<<58 + 1, 1<<58 + 600_000, 9_223_372_036_855, 9_223_372_036_854_775, math.MaxInt64, math.MinInt64,
}

func (ttlMillisecondsInput) Generate(r *rand.Rand, _ int) reflect.Value {
	var value int64
	switch r.Intn(4) {
	case 0:
		value = int64(r.Uint64())
	case 1:
		value = ttlMillisecondsEdges[r.Intn(len(ttlMillisecondsEdges))]
	case 2:
		value = r.Int63n(2*maxTTLMilliseconds) - maxTTLMilliseconds/2
	default:
		value = 1<<58 + r.Int63n(1<<20) // the wrap-to-small-window family
	}
	return reflect.ValueOf(ttlMillisecondsInput(value))
}

func TestValidateTTLBounds(t *testing.T) {
	accepted := []time.Duration{0, time.Millisecond, 1500 * time.Microsecond, time.Minute, MaxTTL}
	for _, ttl := range accepted {
		if err := ValidateTTL(ttl); err != nil {
			t.Errorf("ValidateTTL(%v) = %v, want nil", ttl, err)
		}
	}
	rejected := []time.Duration{
		-1, -time.Hour, math.MinInt64, 1, 500 * time.Microsecond, time.Millisecond - 1,
		MaxTTL + 1, MaxTTL + time.Millisecond, math.MaxInt64,
	}
	for _, ttl := range rejected {
		err := ValidateTTL(ttl)
		if !errors.Is(err, ErrInvalidCommand) {
			t.Errorf("ValidateTTL(%v) = %v, want ErrInvalidCommand", ttl, err)
		}
		var invalid *InvalidTTLError
		if !errors.As(err, &invalid) || invalid.Reason == "" {
			t.Errorf("ValidateTTL(%v) = %v, want *InvalidTTLError with a reason", ttl, err)
		}
	}
}

func TestTTLConversionEdgeTable(t *testing.T) {
	seconds := []struct {
		in   float64
		want time.Duration
		ok   bool
	}{
		{0, 0, true},
		{math.Copysign(0, -1), 0, true},
		{0.001, time.Millisecond, true},
		{0.0015, 2 * time.Millisecond, true}, // rounds half away from zero; never 0
		{0.0014, time.Millisecond, true},
		{1.2345, 1235 * time.Millisecond, true},
		{600, 10 * time.Minute, true},
		{86400, MaxTTL, true},
		{0.0005, 0, false}, // positive but sub-millisecond: was born expired in-process, never-expire via daemon
		{1e-10, 0, false},  // was 0 (never expire) on arm64
		{0.0009999999, 0, false},
		{86400.0000001, 0, false},
		{1e10, 0, false},  // was MinInt64 ns (never expire via daemon) on amd64
		{1e300, 0, false}, // likewise
		{-1, 0, false},
		{math.NaN(), 0, false}, // was 0 on arm64 and MinInt64 ns on amd64
		{math.Inf(1), 0, false},
		{math.Inf(-1), 0, false},
	}
	for _, test := range seconds {
		got, err := TTLFromSeconds(test.in)
		if test.ok && (err != nil || got != test.want) {
			t.Errorf("TTLFromSeconds(%v) = %v, %v; want %v, nil", test.in, got, err, test.want)
		}
		if !test.ok && (err == nil || !errors.Is(err, ErrInvalidCommand)) {
			t.Errorf("TTLFromSeconds(%v) = %v, %v; want ErrInvalidCommand", test.in, got, err)
		}
	}

	milliseconds := []struct {
		in   int64
		want time.Duration
		ok   bool
	}{
		{0, 0, true},
		{1, time.Millisecond, true},
		{600_000, 10 * time.Minute, true},
		{maxTTLMilliseconds, MaxTTL, true},
		{maxTTLMilliseconds + 1, 0, false},
		{1 << 58, 0, false},           // 2^58 ms * 1e6 ns/ms == 0 mod 2^64: was "never expire"
		{1<<58 + 1, 0, false},         // wrapped to a 1ms window
		{9_223_372_036_855, 0, false}, // wrapped negative
		{math.MaxInt64, 0, false},
		{-1, 0, false},
		{math.MinInt64, 0, false},
	}
	for _, test := range milliseconds {
		got, err := TTLFromMilliseconds(test.in)
		if test.ok && (err != nil || got != test.want) {
			t.Errorf("TTLFromMilliseconds(%d) = %v, %v; want %v, nil", test.in, got, err, test.want)
		}
		if !test.ok && (err == nil || !errors.Is(err, ErrInvalidCommand)) {
			t.Errorf("TTLFromMilliseconds(%d) = %v, %v; want ErrInvalidCommand", test.in, got, err)
		}
	}
}

// TestQuickTTLFromMillisecondsBounded executes I7 for ttl_ms: rejection is
// exactly the out-of-range set, and every accepted value is exact.
func TestQuickTTLFromMillisecondsBounded(t *testing.T) {
	property := func(input ttlMillisecondsInput) bool {
		ms := int64(input)
		ttl, err := TTLFromMilliseconds(ms)
		inRange := ms >= 0 && ms <= maxTTLMilliseconds
		if err != nil {
			return !inRange && errors.Is(err, ErrInvalidCommand)
		}
		if !inRange {
			return false
		}
		if ms == 0 {
			return ttl == 0
		}
		return ttl >= time.Millisecond && ttl <= MaxTTL &&
			ttl == time.Duration(ms)*time.Millisecond &&
			ttl.Milliseconds() == ms &&
			ValidateTTL(ttl) == nil
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 50_000, Rand: rand.New(rand.NewSource(166_11))}); err != nil {
		t.Fatal(err)
	}
}

// TestQuickTTLFromSecondsBounded executes I7 for ttl_seconds.
func TestQuickTTLFromSecondsBounded(t *testing.T) {
	property := func(input ttlSecondsInput) bool {
		seconds := float64(input)
		ttl, err := TTLFromSeconds(seconds)
		wantRejected := math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds < 0 ||
			(seconds > 0 && seconds < 0.001) || seconds > 86_400
		if err != nil {
			return wantRejected && errors.Is(err, ErrInvalidCommand)
		}
		if wantRejected {
			return false
		}
		if seconds == 0 {
			return ttl == 0
		}
		requested := seconds * float64(time.Second)
		return ttl >= time.Millisecond && ttl <= MaxTTL &&
			ttl%time.Millisecond == 0 &&
			math.Abs(float64(ttl)-requested) <= float64(time.Millisecond)/2+1 &&
			ValidateTTL(ttl) == nil
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 100_000, Rand: rand.New(rand.NewSource(166_12))}); err != nil {
		t.Fatal(err)
	}
}

// TestQuickDaemonAndInProcessTTLAgree is the I9 differential: the daemon
// path forwards ttl.Milliseconds() as ttl_ms, which the daemon parses with
// TTLFromMilliseconds; the in-process path uses the Duration directly. They
// must stamp the identical window for every accepted ttl_seconds.
func TestQuickDaemonAndInProcessTTLAgree(t *testing.T) {
	property := func(input ttlSecondsInput) bool {
		inProcess, err := TTLFromSeconds(float64(input))
		if err != nil {
			return true
		}
		if inProcess == 0 {
			// The daemon client omits ttl_ms for 0; absent ttl_ms is 0 too.
			return true
		}
		daemon, err := TTLFromMilliseconds(inProcess.Milliseconds())
		return err == nil && daemon == inProcess
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 100_000, Rand: rand.New(rand.NewSource(166_13))}); err != nil {
		t.Fatal(err)
	}
}

// TestQuickAcceptedTTLNeverBornExpired executes I8 through the production
// expiry computation, over every sub-millisecond phase of the schedule
// instant and TTLs from both the millisecond and sub-millisecond grids.
func TestQuickAcceptedTTLNeverBornExpired(t *testing.T) {
	property := func(phaseNS uint32, rawTTL int64, subMillisecond bool) bool {
		var ttl time.Duration
		if subMillisecond {
			ttl = time.Duration(rawTTL % int64(2*time.Millisecond)) // straddles the 1ms floor
		} else {
			ttl = time.Duration(rawTTL%(maxTTLMilliseconds+2)) * time.Millisecond
		}
		if ttl < 0 {
			ttl = -ttl
		}
		scheduledFor := messagingTestTime.Add(time.Duration(phaseNS % uint32(time.Millisecond)))
		expiresAtMS, err := expiryMilliseconds(CommonCommand{TTL: ttl}, scheduledFor)
		if ValidateTTL(ttl) != nil {
			return err != nil
		}
		if err != nil {
			return false
		}
		if ttl == 0 {
			return expiresAtMS == 0
		}
		return expiresAtMS >= scheduledFor.UnixMilli()+1
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 50_000, Rand: rand.New(rand.NewSource(166_14))}); err != nil {
		t.Fatal(err)
	}
}
