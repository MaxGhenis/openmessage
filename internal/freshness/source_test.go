package freshness

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

type stubInboxStore struct {
	latest   map[string]int64
	receipts []int64
	calls    [][]string
	err      error
}

func (s *stubInboxStore) LatestInboxReceipts(context.Context) (map[string]int64, error) {
	return s.latest, s.err
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
		latest: map[string]int64{
			"google.protobuf": 2_000,
			"signal.jsonrpc":  5_000,
			"test.frame":      9_000, // unmapped codec: ignored
			"whatsapp.event":  0,     // no receipt: absent
		},
		receipts: []int64{1_000, 1_500, 2_000, 3_000},
	}
	source := NewInboxActivity(store, map[string]string{
		"google.protobuf": "google",
		"signal.jsonrpc":  "signal",
		"whatsapp.event":  "whatsapp",
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
	latest    map[string]int64
	stamps    map[string][]int64
	platforms [][]string
}

func (s *stubMessageStore) LatestMessageTimestamps() (map[string]int64, error) {
	return s.latest, nil
}

func (s *stubMessageStore) MessageTimestampsBetween(platforms []string, fromMS, toMS int64) ([]int64, error) {
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
		latest: map[string]int64{"sms": 4_000, "rcs": 7_000, "whatsapp": 3_000, "imessage": 9_000},
		stamps: map[string][]int64{"sms": {4_000}, "rcs": {7_000}},
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
	if len(store.platforms) != 1 || len(store.platforms[0]) != 2 {
		t.Fatalf("queried platforms = %v, want sms and rcs together", store.platforms)
	}
}
