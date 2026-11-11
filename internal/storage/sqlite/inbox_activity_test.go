package sqlite

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"
)

func TestInboxActivityQueriesReadReceiptTimesByCodec(t *testing.T) {
	var nowMS int64
	store, repository := openMessageTestRepository(t, func() time.Time { return time.UnixMilli(nowMS) })
	seedMessageAccount(t, store, "google-primary", "google_messages")
	seedMessageAccount(t, store, "signal-primary", "signal_cli")

	appendAt := func(ms int64, accountID, codec string) {
		t.Helper()
		nowMS = ms
		record := messageTestInbox(fmt.Sprintf("inbox-%s-%d", codec, ms), accountID, fmt.Sprintf("%s-%d", codec, ms), []byte("frame"))
		record.Codec = codec
		if _, err := repository.AppendInbox(context.Background(), record); err != nil {
			t.Fatalf("AppendInbox(%d): %v", ms, err)
		}
	}
	for _, ms := range []int64{1_000, 3_000, 2_000, 5_000} {
		appendAt(ms, "google-primary", "google.protobuf")
	}
	appendAt(4_000, "signal-primary", "signal.jsonrpc")
	appendAt(6_000, "google-primary", "google.alt")

	latest, high, err := store.InboxReceiptsAfterRow(context.Background(), 0)
	if err != nil {
		t.Fatalf("InboxReceiptsAfterRow(0): %v", err)
	}
	want := map[string]int64{"google.protobuf": 5_000, "signal.jsonrpc": 4_000, "google.alt": 6_000}
	if !reflect.DeepEqual(latest, want) || high != 6 {
		t.Fatalf("InboxReceiptsAfterRow(0) = %v, %d; want %v, 6", latest, high, want)
	}
	// Past the high-water mark only newer rows are read.
	appendAt(8_000, "signal-primary", "signal.jsonrpc")
	latest, high, err = store.InboxReceiptsAfterRow(context.Background(), 6)
	if err != nil {
		t.Fatalf("InboxReceiptsAfterRow(6): %v", err)
	}
	if want := map[string]int64{"signal.jsonrpc": 8_000}; !reflect.DeepEqual(latest, want) || high != 7 {
		t.Fatalf("InboxReceiptsAfterRow(6) = %v, %d; want %v, 7", latest, high, want)
	}
	if latest, high, err := store.InboxReceiptsAfterRow(context.Background(), 7); err != nil || len(latest) != 0 || high != 7 {
		t.Fatalf("InboxReceiptsAfterRow(7) = %v, %d, %v; want empty, 7", latest, high, err)
	}

	got, err := store.InboxReceiptsBetween(context.Background(), []string{"google.protobuf", "google.alt"}, 2_000, 6_000)
	if err != nil {
		t.Fatalf("InboxReceiptsBetween(): %v", err)
	}
	if want := []int64{2_000, 3_000, 5_000, 6_000}; !reflect.DeepEqual(got, want) {
		t.Fatalf("InboxReceiptsBetween() = %v, want %v (inclusive bounds, ascending)", got, want)
	}
	for _, tc := range []struct {
		name   string
		codecs []string
		from   int64
		to     int64
	}{
		{"no codecs", nil, 0, 10_000},
		{"inverted window", []string{"google.protobuf"}, 6_000, 1_000},
		{"unknown codec", []string{"whatsapp.event"}, 0, 10_000},
	} {
		got, err := store.InboxReceiptsBetween(context.Background(), tc.codecs, tc.from, tc.to)
		if err != nil || len(got) != 0 {
			t.Fatalf("%s: InboxReceiptsBetween() = %v, %v; want empty", tc.name, got, err)
		}
	}
}
