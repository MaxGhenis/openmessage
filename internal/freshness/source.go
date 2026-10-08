package freshness

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Activity source names reported in the status payload.
const (
	SourceV2Inbox  = "v2_inbox"
	SourceMessages = "messages"
)

// ActivitySource reports when each platform's transport last delivered
// anything and the delivery times inside a window. Platform keys are the
// status-block keys: "google", "whatsapp", "signal".
type ActivitySource interface {
	// Name identifies the data the times come from (SourceV2Inbox or
	// SourceMessages) so readers know what "silent" measured.
	Name() string
	// Latest returns the newest activity time per platform key. Platforms
	// with no activity at all are absent.
	Latest(ctx context.Context) (map[string]time.Time, error)
	// Between returns the platform's activity times inside [from, to].
	Between(ctx context.Context, platform string, from, to time.Time) ([]time.Time, error)
}

// InboxStore is the slice of the v2 store an inbox activity source reads.
type InboxStore interface {
	InboxReceiptsAfterRow(ctx context.Context, afterRowID int64) (map[string]int64, int64, error)
	InboxReceiptsBetween(ctx context.Context, codecs []string, fromMS, toMS int64) ([]int64, error)
}

// inboxRescanInterval is how often the inbox source rereads every row instead
// of only the rows past its high-water mark, so a reused rowid or a deleted
// row cannot leave its latest times wrong for long.
const inboxRescanInterval = 6 * time.Hour

// NewInboxActivity measures activity as v2 inbox receipt times: each distinct
// message or conversation event a transport hands to ingest, before decoding
// or projection. That is the earliest point at which "the platform delivered
// something" is observable, so a stalled projection or a decoder quarantine
// does not read as silence. An event re-delivered with the same dedupe key
// keeps its first receipt time, and events the bridges do not write to the
// inbox (typing, presence, pings) never count. platformByCodec maps ingest
// codecs to status platform keys; codecs missing from the map are ignored.
//
// Latest reads only rows appended since its previous call (a full rescan every
// inboxRescanInterval), so its cost does not grow with the inbox.
func NewInboxActivity(store InboxStore, platformByCodec map[string]string) ActivitySource {
	codecs := map[string][]string{}
	for codec, platform := range platformByCodec {
		codec = strings.TrimSpace(codec)
		platform = strings.TrimSpace(platform)
		if codec == "" || platform == "" {
			continue
		}
		codecs[platform] = append(codecs[platform], codec)
	}
	return &inboxActivity{
		store:            store,
		platformByCodec:  platformByCodec,
		codecsByPlatform: codecs,
		now:              time.Now,
	}
}

type inboxActivity struct {
	store            InboxStore
	platformByCodec  map[string]string
	codecsByPlatform map[string][]string
	now              func() time.Time

	mu          sync.Mutex
	latestCodec map[string]int64
	highWater   int64
	scannedAt   time.Time
}

func (*inboxActivity) Name() string { return SourceV2Inbox }

func (s *inboxActivity) Latest(ctx context.Context) (map[string]time.Time, error) {
	if s.store == nil {
		return nil, fmt.Errorf("inbox activity: store is nil")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	full := s.latestCodec == nil || now.Sub(s.scannedAt) >= inboxRescanInterval
	after := s.highWater
	if full {
		after = 0
	}
	byCodec, highWater, err := s.store.InboxReceiptsAfterRow(ctx, after)
	if err != nil {
		return nil, fmt.Errorf("inbox activity: %w", err)
	}
	if full {
		s.latestCodec = map[string]int64{}
		s.scannedAt = now
	}
	for codec, ms := range byCodec {
		if ms > s.latestCodec[codec] {
			s.latestCodec[codec] = ms
		}
	}
	if full || highWater > s.highWater {
		s.highWater = highWater
	}
	latest := map[string]time.Time{}
	for codec, ms := range s.latestCodec {
		platform := s.platformByCodec[codec]
		if platform == "" || ms <= 0 {
			continue
		}
		at := time.UnixMilli(ms)
		if at.After(latest[platform]) {
			latest[platform] = at
		}
	}
	return latest, nil
}

func (s *inboxActivity) Between(ctx context.Context, platform string, from, to time.Time) ([]time.Time, error) {
	if s.store == nil {
		return nil, fmt.Errorf("inbox activity: store is nil")
	}
	codecs := s.codecsByPlatform[platform]
	if len(codecs) == 0 {
		return nil, nil
	}
	rows, err := s.store.InboxReceiptsBetween(ctx, codecs, from.UnixMilli(), to.UnixMilli())
	if err != nil {
		return nil, fmt.Errorf("inbox activity %s: %w", platform, err)
	}
	return millisToTimes(rows), nil
}

// MessageStore is the slice of the legacy store a message activity source
// reads.
type MessageStore interface {
	LatestIncomingMessageTimestamp(ctx context.Context, platforms []string, notAfterMS int64) (int64, error)
	IncomingMessageTimestampsBetween(ctx context.Context, platforms []string, fromMS, toMS int64) ([]int64, error)
}

// NewMessageActivity measures activity as stored incoming-message timestamps,
// for daemons whose readers use the legacy store. Only incoming messages count:
// the app writes outgoing rows itself, so a send, even a failed one, must not
// read as the platform delivering. Message timestamps are the sender's clock,
// not receipt time, so a backlog delivered late counts at its original time:
// it can make the current silence look longer and keep a platform flagged
// until a message with a current timestamp arrives. The v2 inbox source has
// receipt times and no such lag. platformByStorage maps storage platforms
// ("sms", "rcs", ...) to status platform keys.
func NewMessageActivity(store MessageStore, platformByStorage map[string]string) ActivitySource {
	storage := map[string][]string{}
	for stored, platform := range platformByStorage {
		if stored == "" || platform == "" {
			continue
		}
		storage[platform] = append(storage[platform], stored)
	}
	for _, stored := range storage {
		sort.Strings(stored)
	}
	return messageActivity{store: store, storageByPlatform: storage, now: time.Now}
}

// futureSkewAllowance is how far past now a sender's timestamp may run and
// still count as activity; later ones are clock skew.
const futureSkewAllowance = 10 * time.Minute

type messageActivity struct {
	store             MessageStore
	storageByPlatform map[string][]string
	now               func() time.Time
}

func (messageActivity) Name() string { return SourceMessages }

func (s messageActivity) Latest(ctx context.Context) (map[string]time.Time, error) {
	if s.store == nil {
		return nil, fmt.Errorf("message activity: store is nil")
	}
	latest := map[string]time.Time{}
	for platform, stored := range s.storageByPlatform {
		ms, err := s.store.LatestIncomingMessageTimestamp(ctx, stored, s.now().Add(futureSkewAllowance).UnixMilli())
		if err != nil {
			return nil, fmt.Errorf("message activity %s: %w", platform, err)
		}
		if ms > 0 {
			latest[platform] = time.UnixMilli(ms)
		}
	}
	return latest, nil
}

func (s messageActivity) Between(ctx context.Context, platform string, from, to time.Time) ([]time.Time, error) {
	if s.store == nil {
		return nil, fmt.Errorf("message activity: store is nil")
	}
	stored := s.storageByPlatform[platform]
	if len(stored) == 0 {
		return nil, nil
	}
	rows, err := s.store.IncomingMessageTimestampsBetween(ctx, stored, from.UnixMilli(), to.UnixMilli())
	if err != nil {
		return nil, fmt.Errorf("message activity %s: %w", platform, err)
	}
	return millisToTimes(rows), nil
}

func millisToTimes(rows []int64) []time.Time {
	times := make([]time.Time, 0, len(rows))
	for _, ms := range rows {
		if ms > 0 {
			times = append(times, time.UnixMilli(ms))
		}
	}
	return times
}
