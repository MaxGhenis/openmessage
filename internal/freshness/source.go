package freshness

import (
	"context"
	"fmt"
	"strings"
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
	LatestInboxReceipts(ctx context.Context) (map[string]int64, error)
	InboxReceiptsBetween(ctx context.Context, codecs []string, fromMS, toMS int64) ([]int64, error)
}

// NewInboxActivity measures activity as v2 inbox receipt times: every frame a
// transport hands to ingest, message or not, before any decoding. That is the
// earliest point at which "the platform delivered something" is observable, so
// a stalled projection or a decoder quarantine does not read as silence.
// platformByCodec maps ingest codecs to status platform keys; codecs missing
// from the map are ignored.
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
	return inboxActivity{store: store, platformByCodec: platformByCodec, codecsByPlatform: codecs}
}

type inboxActivity struct {
	store            InboxStore
	platformByCodec  map[string]string
	codecsByPlatform map[string][]string
}

func (inboxActivity) Name() string { return SourceV2Inbox }

func (s inboxActivity) Latest(ctx context.Context) (map[string]time.Time, error) {
	if s.store == nil {
		return nil, fmt.Errorf("inbox activity: store is nil")
	}
	byCodec, err := s.store.LatestInboxReceipts(ctx)
	if err != nil {
		return nil, fmt.Errorf("inbox activity: %w", err)
	}
	latest := map[string]time.Time{}
	for codec, ms := range byCodec {
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

func (s inboxActivity) Between(ctx context.Context, platform string, from, to time.Time) ([]time.Time, error) {
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
	LatestMessageTimestamps() (map[string]int64, error)
	MessageTimestampsBetween(platforms []string, fromMS, toMS int64) ([]int64, error)
}

// NewMessageActivity measures activity as stored message timestamps, for
// daemons without v2 ingest. Message timestamps are the sender's clock, not
// receipt time, so a late-delivered backlog counts at its original time; that
// can only make a stall look shorter, never invent one. platformByStorage maps
// storage platforms ("sms", "rcs", ...) to status platform keys.
func NewMessageActivity(store MessageStore, platformByStorage map[string]string) ActivitySource {
	storage := map[string][]string{}
	for stored, platform := range platformByStorage {
		if stored == "" || platform == "" {
			continue
		}
		storage[platform] = append(storage[platform], stored)
	}
	return messageActivity{store: store, platformByStorage: platformByStorage, storageByPlatform: storage}
}

type messageActivity struct {
	store             MessageStore
	platformByStorage map[string]string
	storageByPlatform map[string][]string
}

func (messageActivity) Name() string { return SourceMessages }

func (s messageActivity) Latest(context.Context) (map[string]time.Time, error) {
	if s.store == nil {
		return nil, fmt.Errorf("message activity: store is nil")
	}
	byStorage, err := s.store.LatestMessageTimestamps()
	if err != nil {
		return nil, fmt.Errorf("message activity: %w", err)
	}
	latest := map[string]time.Time{}
	for stored, ms := range byStorage {
		platform := s.platformByStorage[stored]
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

func (s messageActivity) Between(_ context.Context, platform string, from, to time.Time) ([]time.Time, error) {
	if s.store == nil {
		return nil, fmt.Errorf("message activity: store is nil")
	}
	stored := s.storageByPlatform[platform]
	if len(stored) == 0 {
		return nil, nil
	}
	rows, err := s.store.MessageTimestampsBetween(stored, from.UnixMilli(), to.UnixMilli())
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
