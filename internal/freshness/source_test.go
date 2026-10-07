package freshness

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

type stubInboxStore struct {
	rows     []stubInboxRow
	afters   []int64
	receipts []int64
	calls    [][]string
	err      error
}

type stubInboxRow struct {
	rowID int64
	codec string
	ms    int64
}

func (s *stubInboxStore) InboxReceiptsAfterRow(_ context.Context, after int64) (map[string]int64, int64, error) {
	s.afters = append(s.afters, after)
	if s.err != nil {
		return nil, 0, s.err
	}
	latest, high := map[string]int64{}, after
	for _, row := range s.rows {
		if row.rowID <= after {
			continue
		}
		if row.ms > latest[row.codec] {
			latest[row.codec] = row.ms
		}
		if row.rowID > high {
			high = row.rowID
		}
	}
	return latest, high, nil
}

func (s *stubInboxStore) InboxReceiptsBetween(_ context.Context, codecs []string, fromMS, toMS int64) ([]int64, error) {
	s.calls = append(s.calls, append([]string(nil), codecs...))
	var out []int64
	for _, ms := range s.receipts {
		if ms >= fromMS && ms <= toMS {
			out = append(out, ms)
		}
	}
	return out, s.err
}

func TestInboxActivityMapsCodecsToPlatforms(t *testing.T) {
	store := &stubInboxStore{
		rows: []stubInboxRow{
			{1, "google.protobuf", 2_000},
			{2, "signal.jsonrpc", 5_000},
			{3, "test.frame", 9_000}, // unmapped codec: ignored
		},
		receipts: []int64{1_000, 1_500, 2_000, 3_000},
	}
	source := NewInboxActivity(store, map[string]string{
		"google.protobuf": "google",
		"signal.jsonrpc":  "signal",
		"whatsapp.event":  "whatsapp", // no rows: absent
	})
	if source.Name() != SourceV2Inbox {
		t.Fatalf("Name() = %q, want %q", source.Name(), SourceV2Inbox)
	}
	latest, err := source.Latest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]time.Time{"google": time.UnixMilli(2_000), "signal": time.UnixMilli(5_000)}
	if !reflect.DeepEqual(latest, want) {
		t.Fatalf("Latest() = %v, want %v", latest, want)
	}
	got, err := source.Between(context.Background(), "google", time.UnixMilli(1_500), time.UnixMilli(2_000))
	if err != nil {
		t.Fatal(err)
	}
	if want := []time.Time{time.UnixMilli(1_500), time.UnixMilli(2_000)}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Between() = %v, want %v", got, want)
	}
	if want := [][]string{{"google.protobuf"}}; !reflect.DeepEqual(store.calls, want) {
		t.Fatalf("queried codecs = %v, want %v", store.calls, want)
	}
	if got, err := source.Between(context.Background(), "telegram", time.UnixMilli(0), time.UnixMilli(9_999)); err != nil || got != nil {
		t.Fatalf("Between(unknown platform) = %v, %v; want nil, nil", got, err)
	}
}

// Latest reads only rows past its high-water mark, keeps what it already
// knows, and rescans everything after inboxRescanInterval.
// (Review of PR #190: a full scan per refresh grew with the inbox.)
func TestInboxActivityLatestIsIncremental(t *testing.T) {
	store := &stubInboxStore{rows: []stubInboxRow{{1, "google.protobuf", 1_000}, {2, "signal.jsonrpc", 4_000}}}
	clock := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	source := NewInboxActivity(store, map[string]string{"google.protobuf": "google", "signal.jsonrpc": "signal"}).(*inboxActivity)
	source.now = func() time.Time { return clock }

	latest := func() map[string]time.Time {
		t.Helper()
		got, err := source.Latest(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	latest()
	store.rows = append(store.rows, stubInboxRow{3, "google.protobuf", 7_000})
	got := latest()
	// Signal's time comes from memory; only row 3 was read.
	want := map[string]time.Time{"google": time.UnixMilli(7_000), "signal": time.UnixMilli(4_000)}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Latest() after an append = %v, want %v", got, want)
	}
	if want := []int64{0, 2}; !reflect.DeepEqual(store.afters, want) {
		t.Fatalf("high-water marks queried = %v, want %v", store.afters, want)
	}
	latest() // nothing new
	clock = clock.Add(inboxRescanInterval)
	latest()
	if want := []int64{0, 2, 3, 0}; !reflect.DeepEqual(store.afters, want) {
		t.Fatalf("high-water marks queried = %v, want %v (full rescan after the interval)", store.afters, want)
	}
}

func TestInboxActivityPropagatesStoreErrors(t *testing.T) {
	boom := errors.New("boom")
	source := NewInboxActivity(&stubInboxStore{err: boom}, map[string]string{"google.protobuf": "google"})
	if _, err := source.Latest(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("Latest() error = %v, want %v", err, boom)
	}
	if _, err := source.Between(context.Background(), "google", time.UnixMilli(0), time.UnixMilli(1)); !errors.Is(err, boom) {
		t.Fatalf("Between() error = %v, want %v", err, boom)
	}
}

type stubMessageStore struct {
	stamps    map[string][]int64
	platforms [][]string
}

func (s *stubMessageStore) LatestIncomingMessageTimestamp(_ context.Context, platforms []string) (int64, error) {
	var latest int64
	for _, platform := range platforms {
		for _, ms := range s.stamps[platform] {
			if ms > latest {
				latest = ms
			}
		}
	}
	return latest, nil
}

func (s *stubMessageStore) IncomingMessageTimestampsBetween(_ context.Context, platforms []string, fromMS, toMS int64) ([]int64, error) {
	s.platforms = append(s.platforms, append([]string(nil), platforms...))
	var out []int64
	for _, platform := range platforms {
		for _, ms := range s.stamps[platform] {
			if ms >= fromMS && ms <= toMS {
				out = append(out, ms)
			}
		}
	}
	return out, nil
}

// SMS and RCS are both Google Messages: the newer of the two is Google's
// latest activity, and a baseline query covers both.
func TestMessageActivityMergesStoragePlatforms(t *testing.T) {
	store := &stubMessageStore{
		stamps: map[string][]int64{"sms": {4_000}, "rcs": {7_000}, "whatsapp": {3_000}, "imessage": {9_000}},
	}
	source := NewMessageActivity(store, map[string]string{"sms": "google", "rcs": "google", "whatsapp": "whatsapp"})
	if source.Name() != SourceMessages {
		t.Fatalf("Name() = %q, want %q", source.Name(), SourceMessages)
	}
	latest, err := source.Latest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]time.Time{"google": time.UnixMilli(7_000), "whatsapp": time.UnixMilli(3_000)}
	if !reflect.DeepEqual(latest, want) {
		t.Fatalf("Latest() = %v, want %v", latest, want)
	}
	got, err := source.Between(context.Background(), "google", time.UnixMilli(0), time.UnixMilli(10_000))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("Between() = %v, want both sms and rcs stamps", got)
	}
	if want := [][]string{{"rcs", "sms"}}; !reflect.DeepEqual(store.platforms, want) {
		t.Fatalf("queried platforms = %v, want %v", store.platforms, want)
	}
}
