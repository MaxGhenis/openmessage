package sqlite

import (
	"context"
	"fmt"
	"math/rand/v2"
	"testing"
)

func TestListInboxByCodecSinceFiltersCodecAndWindowAndKeepsProcessedFrames(t *testing.T) {
	ctx := context.Background()
	clock := newMessageTestClock(1_000)
	store, repository := openMessageTestRepository(t, clock.Now)
	seedMessageAccount(t, store, "google-primary", "google_messages")

	appendAt := func(nowMS int64, id, codec string) {
		t.Helper()
		clock.Set(nowMS)
		record := messageTestInbox(id, "google-primary", "dedupe-"+id, []byte("payload-"+id))
		record.Codec = codec
		if _, err := repository.AppendInbox(ctx, record); err != nil {
			t.Fatalf("AppendInbox(%s): %v", id, err)
		}
	}
	appendAt(1_000, "before-window", "google.protobuf")
	appendAt(2_000, "at-boundary", "google.protobuf")
	appendAt(2_500, "other-codec", "signal.jsonrpc")
	appendAt(3_000, "processed", "google.protobuf")
	appendAt(3_000, "a-same-time", "google.protobuf")
	if err := repository.MarkInboxProcessed(ctx, "processed", "google-primary"); err != nil {
		t.Fatalf("MarkInboxProcessed(): %v", err)
	}

	records, err := store.ListInboxByCodecSince(ctx, "google.protobuf", 2_000)
	if err != nil {
		t.Fatalf("ListInboxByCodecSince(): %v", err)
	}
	var got []string
	for _, record := range records {
		got = append(got, record.InboxID)
		if record.Codec != "google.protobuf" {
			t.Fatalf("record %s codec = %q", record.InboxID, record.Codec)
		}
		if string(record.Payload) != "payload-"+record.InboxID {
			t.Fatalf("record %s payload = %q", record.InboxID, record.Payload)
		}
	}
	want := []string{"at-boundary", "a-same-time", "processed"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("records = %v, want %v (receipt order, inbox_id tiebreak)", got, want)
	}
	if records[2].ProcessedAtMS == nil {
		t.Fatalf("processed frame lost its processed_at_ms: %+v", records[2])
	}
}

// TestListInboxByCodecSinceMatchesAFilterOverAllRows checks the query against
// a plain in-memory filter over random frames.
func TestListInboxByCodecSinceMatchesAFilterOverAllRows(t *testing.T) {
	ctx := context.Background()
	clock := newMessageTestClock(1)
	store, repository := openMessageTestRepository(t, clock.Now)
	seedMessageAccount(t, store, "account", "google_messages")

	r := rand.New(rand.NewPCG(7, 11))
	codecs := []string{"google.protobuf", "signal.jsonrpc", "whatsapp.event"}
	type frame struct {
		id      string
		codec   string
		atMS    int64
		process bool
	}
	var frames []frame
	for i := 0; i < 300; i++ {
		f := frame{
			id:      fmt.Sprintf("frame-%03d", i),
			codec:   codecs[r.IntN(len(codecs))],
			atMS:    1 + r.Int64N(50),
			process: r.IntN(2) == 0,
		}
		frames = append(frames, f)
		clock.Set(f.atMS)
		record := messageTestInbox(f.id, "account", "dedupe-"+f.id, []byte(f.id))
		record.Codec = f.codec
		if _, err := repository.AppendInbox(ctx, record); err != nil {
			t.Fatalf("AppendInbox(%s): %v", f.id, err)
		}
	}
	clock.Set(100)
	for _, f := range frames {
		if f.process {
			if err := repository.MarkInboxProcessed(ctx, f.id, "account"); err != nil {
				t.Fatalf("MarkInboxProcessed(%s): %v", f.id, err)
			}
		}
	}

	for _, codec := range codecs {
		for sinceMS := int64(0); sinceMS <= 51; sinceMS += 3 {
			records, err := store.ListInboxByCodecSince(ctx, codec, sinceMS)
			if err != nil {
				t.Fatalf("ListInboxByCodecSince(%s, %d): %v", codec, sinceMS, err)
			}
			want := 0
			for _, f := range frames {
				if f.codec == codec && f.atMS >= sinceMS {
					want++
				}
			}
			if len(records) != want {
				t.Fatalf("%s since %d: %d records, want %d", codec, sinceMS, len(records), want)
			}
			for i, record := range records {
				if record.Codec != codec || record.ReceivedAtMS < sinceMS {
					t.Fatalf("%s since %d: record %+v outside the filter", codec, sinceMS, record)
				}
				if i > 0 {
					prev := records[i-1]
					if prev.ReceivedAtMS > record.ReceivedAtMS ||
						(prev.ReceivedAtMS == record.ReceivedAtMS && prev.InboxID >= record.InboxID) {
						t.Fatalf("%s since %d: records out of order at %d", codec, sinceMS, i)
					}
				}
			}
		}
	}
}
