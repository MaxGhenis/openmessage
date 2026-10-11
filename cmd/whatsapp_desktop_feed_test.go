package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/ingest"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
	"github.com/maxghenis/openmessage/internal/whatsappdesktop"
	"github.com/maxghenis/openmessage/internal/whatsapplive"
)

type desktopFeedTestSink struct {
	records []bridge.RawIngressRecord
	failAt  int
}

func (s *desktopFeedTestSink) AppendHistoryIngress(_ context.Context, record bridge.RawIngressRecord) error {
	if s.failAt > 0 && len(s.records)+1 == s.failAt {
		return errors.New("synthetic append failure")
	}
	s.records = append(s.records, record)
	return nil
}

func desktopFeedTestChunk() whatsappdesktop.Chunk {
	return whatsappdesktop.Chunk{
		Chat:     whatsappdesktop.Chat{PK: 1, JID: "15550100001@s.whatsapp.net", Title: "Synthetic contact", Participants: []whatsappdesktop.Participant{{JID: "15550100001@s.whatsapp.net"}}},
		Messages: []whatsappdesktop.Message{{PK: 1, StanzaID: "synthetic-stanza", SenderJID: "15550100001@s.whatsapp.net", TimestampMS: 1800000000000, Type: 0, Text: "synthetic body"}},
	}
}

func newDesktopFeedTest(t *testing.T, live func() whatsapplive.StatusSnapshot, scan whatsappDesktopScan, sink bridge.HistoryIngressSink) *whatsappDesktopFeed {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ChatStorage.sqlite")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return &whatsappDesktopFeed{
		status: whatsappDesktopFeedStatus{Enabled: true, Available: true, DBPath: path, RowsSkippedByReason: map[string]int64{}},
		gate:   &whatsappDesktopGate{}, live: live, scan: scan, sink: sink,
		logger: zerolog.Nop(), logged: map[string]bool{},
	}
}

func TestWhatsAppDesktopFeedRuntimeGuards(t *testing.T) {
	// The launcher marker is intentionally irrelevant to desktop readability.
	t.Setenv("OPENMESSAGES_APP_SANDBOX", "1")
	for _, value := range []string{"", "1", "true", "0", "false", "off", "  FaLsE  "} {
		t.Run(fmt.Sprintf("env_%q", value), func(t *testing.T) {
			t.Setenv("OPENMESSAGES_WHATSAPP_DESKTOP_FEED", value)
			wantEnv := value == "" || value == "1" || value == "true"
			for _, transports := range []bool{false, true} {
				for _, demo := range []bool{false, true} {
					for _, enabled := range []bool{false, true} {
						var stack *v2Stack
						if enabled {
							stack = &v2Stack{}
						}
						if got, want := whatsappDesktopFeedEnabled(stack, transports, demo), enabled && transports && !demo && wantEnv; got != want {
							t.Fatalf("runtime guards = %v, want %v", got, want)
						}
					}
				}
			}
		})
	}
}

func TestWhatsAppDesktopGateRequiresContinuousFiveMinutesOffline(t *testing.T) {
	now := time.Now()
	gate := &whatsappDesktopGate{}
	paired := whatsapplive.StatusSnapshot{Paired: true}
	for _, elapsed := range []time.Duration{0, time.Minute, whatsappDesktopOfflineDelay - time.Nanosecond} {
		if gate.allowed(paired, now.Add(elapsed)) {
			t.Fatalf("paired disconnected bridge admitted after %v", elapsed)
		}
	}
	if !gate.allowed(paired, now.Add(whatsappDesktopOfflineDelay)) {
		t.Fatal("bridge refused after five continuous minutes offline")
	}
	if gate.allowed(whatsapplive.StatusSnapshot{Paired: true, Connected: true}, now.Add(6*time.Minute)) {
		t.Fatal("connected bridge admitted")
	}
	if gate.allowed(paired, now.Add(7*time.Minute)) || gate.allowed(paired, now.Add(12*time.Minute-time.Nanosecond)) {
		t.Fatal("brief reconnect did not restart the offline window")
	}
	if !gate.allowed(paired, now.Add(12*time.Minute)) {
		t.Fatal("second five minute offline window refused")
	}
	if !gate.allowed(whatsapplive.StatusSnapshot{}, now.Add(13*time.Minute)) {
		t.Fatal("unpaired bridge must immediately permit history")
	}
	if gate.allowed(paired, now.Add(14*time.Minute)) {
		t.Fatal("pairing must begin a new offline window")
	}
}

func TestWhatsAppDesktopGateMissedBriefReconnectRestartsOfflineWindow(t *testing.T) {
	now := time.Now()
	gate := &whatsappDesktopGate{}
	status := whatsapplive.StatusSnapshot{Paired: true}
	if gate.allowed(status, now) || !gate.allowed(status, now.Add(5*time.Minute)) {
		t.Fatal("initial five minute offline window behaved incorrectly")
	}
	// Both status callbacks saw offline because the connected callback was
	// delayed by SendPresence. The bridge retains the usable connection time.
	status.LastConnectedAt = now.Add(5*time.Minute + 10*time.Second)
	observedAt := now.Add(6 * time.Minute)
	if gate.allowed(status, observedAt) || gate.allowed(status, observedAt.Add(5*time.Minute-time.Nanosecond)) {
		t.Fatal("missed reconnect did not restart the continuous offline window")
	}
	if !gate.allowed(status, observedAt.Add(5*time.Minute)) {
		t.Fatal("retained timestamp restarted an already restarted offline window")
	}
}

func TestWhatsAppDesktopI7ConnectedAppendsNothing(t *testing.T) {
	// Seeded categories come from high bits, so the connected/unpaired/paired
	// paths all run. I7 also asserts that a connected bridge never reads rows.
	counts := [3]int{}
	state := uint64(0xd2b74407b1ce6e93)
	for iteration := 0; iteration < 90; iteration++ {
		state = state*6364136223846793005 + 1442695040888963407
		category := int((state >> 33) % 3)
		counts[category]++
		status := whatsapplive.StatusSnapshot{Paired: category != 1, Connected: category == 0}
		calls := 0
		sink := &desktopFeedTestSink{}
		feed := newDesktopFeedTest(t, func() whatsapplive.StatusSnapshot { return status }, func(_ context.Context, _ whatsappdesktop.ScanOptions, emit func(whatsappdesktop.Chunk) error) (whatsappdesktop.Stats, error) {
			calls++
			return whatsappdesktop.Stats{HighWaterPK: 1}, emit(desktopFeedTestChunk())
		}, sink)
		feed.gate.allowed(status, time.Now().Add(-whatsappDesktopOfflineDelay))
		feed.scanOnce(context.Background(), time.Now())
		wantCalls := 1
		if status.Connected {
			wantCalls = 0
		}
		if calls != wantCalls || len(sink.records) != wantCalls {
			t.Fatalf("category %d: scans/appends = %d/%d, want %d/%d", category, calls, len(sink.records), wantCalls, wantCalls)
		}
	}
	for category, count := range counts {
		if count < 10 {
			t.Fatalf("category %d exercised only %d times; all categories must be covered", category, count)
		}
	}
}

func TestWhatsAppDesktopFeedFullIncrementalCadenceAndGenerationZero(t *testing.T) {
	if whatsappDesktopInitialDelay != 20*time.Second || whatsappDesktopScanInterval != 30*time.Second || whatsappDesktopFullInterval != 6*time.Hour {
		t.Fatal("desktop history cadence changed")
	}
	var scans []whatsappdesktop.ScanOptions
	sink := &desktopFeedTestSink{}
	feed := newDesktopFeedTest(t, func() whatsapplive.StatusSnapshot { return whatsapplive.StatusSnapshot{} }, func(_ context.Context, options whatsappdesktop.ScanOptions, emit func(whatsappdesktop.Chunk) error) (whatsappdesktop.Stats, error) {
		scans = append(scans, options)
		return whatsappdesktop.Stats{HighWaterPK: 13, RowsSkippedByReason: map[string]int64{"unsupported_type": 2}}, emit(desktopFeedTestChunk())
	}, sink)
	now := time.Now()
	feed.scanOnce(context.Background(), now)
	feed.scanOnce(context.Background(), now.Add(whatsappDesktopScanInterval))
	feed.scanOnce(context.Background(), now.Add(whatsappDesktopFullInterval))
	if want := []whatsappdesktop.ScanOptions{{ChunkSize: 100}, {AfterPK: 13, ChunkSize: 100}, {ChunkSize: 100}}; !reflect.DeepEqual(scans, want) {
		t.Fatalf("scan options = %+v, want %+v", scans, want)
	}
	for _, record := range sink.records {
		if record.Generation != 0 || record.AccountID != whatsappAccountID || record.Codec != ingest.WhatsAppDesktopHistoryCodec || !strings.HasPrefix(record.DedupeKey, "wadesk:") {
			t.Fatalf("desktop record uses wrong capture contract: %+v", record)
		}
		if record.DedupeKey != sink.records[0].DedupeKey || !bytes.Equal(record.Payload, sink.records[0].Payload) {
			t.Fatal("identical desktop snapshot did not yield deterministic payload/key")
		}
	}
	status := feed.Snapshot()
	if status.HighWaterPK != 13 || status.FramesAppended != 3 || status.LastFullScanAtMS != now.Add(whatsappDesktopFullInterval).UnixMilli() || status.RowsSkippedByReason["unsupported_type"] != 2 {
		t.Fatalf("scan status = %+v", status)
	}
	status.RowsSkippedByReason["unsupported_type"] = 100
	if feed.Snapshot().RowsSkippedByReason["unsupported_type"] != 2 {
		t.Fatal("status caller can mutate feeder skip counters")
	}
}

func TestWhatsAppDesktopFeedReconnectDuringScanStopsBeforeNextAppend(t *testing.T) {
	status := whatsapplive.StatusSnapshot{}
	sink := &desktopFeedTestSink{}
	calls := 0
	feed := newDesktopFeedTest(t, func() whatsapplive.StatusSnapshot { return status }, func(_ context.Context, _ whatsappdesktop.ScanOptions, emit func(whatsappdesktop.Chunk) error) (whatsappdesktop.Stats, error) {
		calls++
		if err := emit(desktopFeedTestChunk()); err != nil {
			return whatsappdesktop.Stats{}, err
		}
		status = whatsapplive.StatusSnapshot{Paired: true, Connected: true}
		return whatsappdesktop.Stats{HighWaterPK: 2}, emit(desktopFeedTestChunk())
	}, sink)
	feed.scanOnce(context.Background(), time.Now())
	feed.scanOnce(context.Background(), time.Now().Add(time.Minute))
	if calls != 1 || len(sink.records) != 1 || feed.Snapshot().HighWaterPK != 0 || feed.Snapshot().Active || feed.Snapshot().LastError != "" {
		t.Fatalf("reconnect did not halt cleanly: scans=%d appends=%d status=%+v", calls, len(sink.records), feed.Snapshot())
	}
}

func TestWhatsAppDesktopFeedFailedAppendDoesNotAdvanceHighWater(t *testing.T) {
	sink := &desktopFeedTestSink{failAt: 2}
	var afterPKs []int64
	feed := newDesktopFeedTest(t, func() whatsapplive.StatusSnapshot { return whatsapplive.StatusSnapshot{} }, func(_ context.Context, options whatsappdesktop.ScanOptions, emit func(whatsappdesktop.Chunk) error) (whatsappdesktop.Stats, error) {
		afterPKs = append(afterPKs, options.AfterPK)
		if err := emit(desktopFeedTestChunk()); err != nil {
			return whatsappdesktop.Stats{HighWaterPK: 1}, err
		}
		return whatsappdesktop.Stats{HighWaterPK: 2}, emit(desktopFeedTestChunk())
	}, sink)
	feed.scanOnce(context.Background(), time.Now())
	if feed.Snapshot().HighWaterPK != 0 || feed.Snapshot().LastFullScanAtMS != 0 || feed.Snapshot().LastError == "" {
		t.Fatalf("failed partial scan advanced cursor: %+v", feed.Snapshot())
	}
	sink.failAt = 0
	feed.scanOnce(context.Background(), time.Now().Add(time.Minute))
	if !reflect.DeepEqual(afterPKs, []int64{0, 0}) || feed.Snapshot().HighWaterPK != 2 || feed.Snapshot().LastError != "" || sink.records[0].DedupeKey != sink.records[1].DedupeKey {
		t.Fatalf("retry skipped gap or failed to recover: afterPKs=%v status=%+v", afterPKs, feed.Snapshot())
	}
}

func TestWhatsAppDesktopFeedLogsEachDistinctAvailabilityErrorOnce(t *testing.T) {
	var output bytes.Buffer
	feed := newDesktopFeedTest(t, func() whatsapplive.StatusSnapshot { return whatsapplive.StatusSnapshot{} }, nil, nil)
	feed.logger = zerolog.New(&output)
	for _, message := range []string{"synthetic missing DB", "synthetic missing DB", "synthetic permission", "synthetic missing DB"} {
		feed.recordError(errors.New(message))
	}
	if strings.Count(output.String(), "WhatsApp desktop history feed unavailable") != 2 {
		t.Fatalf("distinct errors were not logged once: %s", output.String())
	}
}

func TestWhatsAppDesktopFeedUnavailableDatabaseDoesNotScanOrAdvance(t *testing.T) {
	calls := 0
	feed := newDesktopFeedTest(t, func() whatsapplive.StatusSnapshot { return whatsapplive.StatusSnapshot{} }, func(context.Context, whatsappdesktop.ScanOptions, func(whatsappdesktop.Chunk) error) (whatsappdesktop.Stats, error) {
		calls++
		return whatsappdesktop.Stats{}, errors.New("synthetic unreadable schema")
	}, &desktopFeedTestSink{})
	if err := os.Remove(feed.status.DBPath); err != nil {
		t.Fatal(err)
	}
	feed.scanOnce(context.Background(), time.Now())
	if calls != 0 || feed.Snapshot().Available || feed.Snapshot().Active || feed.Snapshot().HighWaterPK != 0 || feed.Snapshot().LastError == "" {
		t.Fatalf("missing database scanned/advanced: calls=%d status=%+v", calls, feed.Snapshot())
	}
	if err := os.WriteFile(feed.status.DBPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	feed.scanOnce(context.Background(), time.Now())
	if calls != 1 || feed.Snapshot().Available || feed.Snapshot().Active || feed.Snapshot().HighWaterPK != 0 || feed.Snapshot().LastError != "synthetic unreadable schema" {
		t.Fatalf("unsupported database claimed availability: calls=%d status=%+v", calls, feed.Snapshot())
	}
}

func TestWhatsAppDesktopSelfIdentityCaptureDoesNotMintOrChangeIdentity(t *testing.T) {
	for _, test := range []struct{ name, kind, canonical, want string }{
		{"e164", "e164", "+15550100002", "15550100002@s.whatsapp.net"},
		{"pn jid", "jid", "15550100002:4@s.whatsapp.net", "15550100002@s.whatsapp.net"},
		{"lid is not a phone identity", "jid", "991122@lid", ""},
		{"missing", "", "", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, err := sqlite.Open(filepath.Join(t.TempDir(), "store.sqlite3"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			if err := ensureWhatsAppDesktopAccount(store); err != nil {
				t.Fatal(err)
			}
			if test.kind != "" {
				if err := store.UpsertIdentity(sqlite.Identity{IdentityID: "synthetic-self", AccountID: whatsappAccountID, Kind: sqlite.IdentityKind(test.kind), CanonicalValue: test.canonical, RawValue: test.canonical, DisplayName: "Synthetic self", IsSelf: true, MetadataJSON: "{}", CreatedAtMS: 100, UpdatedAtMS: 200}); err != nil {
					t.Fatal(err)
				}
			}
			before, err := store.ListIdentities(whatsappAccountID)
			if err != nil {
				t.Fatal(err)
			}
			got, err := whatsappDesktopSelfJID(store)
			if err != nil || got != test.want {
				t.Fatalf("captured self JID = %q, %v; want %q", got, err, test.want)
			}
			after, err := store.ListIdentities(whatsappAccountID)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("self capture changed identity rows: before=%+v after=%+v err=%v", before, after, err)
			}
		})
	}
}

func TestWhatsAppDesktopHistoryRegisteredAndGenerationZeroAccepted(t *testing.T) {
	stack, err := newV2Stack(v2StackDeps{Logger: zerolog.Nop(), DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer stack.Store.Close()
	if err := ensureWhatsAppDesktopAccount(stack.Store); err != nil {
		t.Fatal(err)
	}
	chunk := desktopFeedTestChunk()
	frame := ingest.WhatsAppDesktopHistorySnapshot{ChatJID: chunk.Chat.JID, Title: chunk.Chat.Title, Participants: []ingest.WhatsAppDesktopHistoryParticipant{{JID: chunk.Chat.JID}}, Messages: []ingest.WhatsAppDesktopHistoryMessage{{StanzaID: chunk.Messages[0].StanzaID, TimestampMS: chunk.Messages[0].TimestampMS, Type: 0, Text: chunk.Messages[0].Text}}}
	record, err := ingest.WhatsAppDesktopHistoryRecord(whatsappAccountID, 0, frame, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := stack.Sink.AppendHistoryIngress(context.Background(), record); err != nil {
		t.Fatalf("daemon history generation zero rejected: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- stack.worker.Run(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("stop worker: %v", err)
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for stack.Counters.Snapshot(whatsappAccountID).HistoryImported == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	counters := stack.Counters.Snapshot(whatsappAccountID)
	if counters.HistoryAppended != 1 || counters.HistoryImported != 1 || counters.Appended != 0 || counters.Projected != 0 {
		t.Fatalf("desktop history counted as live evidence: %+v", counters)
	}
}
