package cmd

// Signal Desktop feed tests. Every archive, address and body is synthetic;
// nothing here reads the real Signal Desktop database or the keychain.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/ingest"
	"github.com/maxghenis/openmessage/internal/signaldesktop"
	"github.com/maxghenis/openmessage/internal/signaldesktop/fixture"
	"github.com/maxghenis/openmessage/internal/signallive"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
	"github.com/maxghenis/openmessage/internal/v2keys"
)

const (
	sdfSelf     = "+15555550100"
	sdfSelfACI  = "00000000-0000-4000-8000-000000000000"
	sdfPeerE164 = "+15555550101"
	sdfPeerACI  = "11111111-2222-4333-8444-555555555555"
	sdfBaseMS   = int64(1_790_000_000_000)
)

func sdfChunk() signaldesktop.Chunk {
	return signaldesktop.Chunk{
		Chat:     signaldesktop.Chat{RemoteID: "signal:" + sdfPeerE164, Participants: []signaldesktop.Participant{{Address: sdfPeerE164}}},
		Messages: []signaldesktop.Message{{RowID: 1, AuthorAddress: sdfPeerE164, SentAtMS: sdfBaseMS, Body: "synthetic"}},
	}
}

type sdfScanCall struct {
	options signaldesktop.ScanOptions
}

// newSDFTest builds a feed with injected live status, silence, scan, facts
// and file stamp, so each rule can be driven directly.
func newSDFTest(t *testing.T, live *signallive.StatusSnapshot, scan signalDesktopScanFunc, sink bridge.HistoryIngressSink) *signalDesktopFeed {
	t.Helper()
	stamp := "stamp-1"
	feed := &signalDesktopFeed{
		status:     signalDesktopFeedStatus{Enabled: true, RowsSkippedByReason: map[string]int64{}},
		gate:       &signalDesktopGate{},
		live:       func() signallive.StatusSnapshot { return *live },
		liveSilent: func(time.Time) bool { return false },
		scan:       scan,
		facts: func(context.Context) (*signaldesktop.Facts, error) {
			return signaldesktop.NewFacts(signalAccountID, sdfSelf, nil, nil, nil), nil
		},
		stamp:  func() (string, error) { return stamp, nil },
		sink:   sink,
		logger: zerolog.Nop(), logged: map[string]bool{},
	}
	return feed
}

func TestSignalDesktopFeedRuntimeGuards(t *testing.T) {
	// The launcher's sandbox marker is irrelevant; readability decides.
	t.Setenv("OPENMESSAGES_APP_SANDBOX", "1")
	for _, value := range []string{"", "1", "true", "0", "false", "off", "  OFF  "} {
		t.Run(fmt.Sprintf("env_%q", value), func(t *testing.T) {
			t.Setenv("OPENMESSAGES_SIGNAL_DESKTOP_FEED", value)
			wantEnv := value == "" || value == "1" || value == "true"
			for _, transports := range []bool{false, true} {
				for _, demo := range []bool{false, true} {
					for _, withStack := range []bool{false, true} {
						var stack *v2Stack
						if withStack {
							stack = &v2Stack{}
						}
						if got, want := signalDesktopFeedEnabled(stack, transports, demo), withStack && transports && !demo && wantEnv; got != want {
							t.Fatalf("enabled = %v, want %v", got, want)
						}
					}
				}
			}
		})
	}
}

func TestSignalDesktopGateFollowsLiveHealth(t *testing.T) {
	now := time.Now()
	gate := &signalDesktopGate{}
	paired := signallive.StatusSnapshot{Paired: true}
	connected := signallive.StatusSnapshot{Paired: true, Connected: true}
	expect := func(status signallive.StatusSnapshot, silent bool, at time.Duration, want bool, wantReason string) {
		t.Helper()
		got, reason := gate.decide(status, silent, now.Add(at))
		if got != want || reason != wantReason {
			t.Fatalf("at %v: decide = %v/%s, want %v/%s", at, got, reason, want, wantReason)
		}
	}
	expect(signallive.StatusSnapshot{}, false, 0, true, signalDesktopGateUnpaired)
	expect(connected, false, time.Second, false, signalDesktopGateReceiving)
	expect(connected, true, 2*time.Second, true, signalDesktopGateSilent)
	expect(paired, false, time.Minute, false, signalDesktopGateOfflineWaiting)
	expect(paired, false, time.Minute+signalDesktopOfflineDelay-time.Nanosecond, false, signalDesktopGateOfflineWaiting)
	expect(paired, false, time.Minute+signalDesktopOfflineDelay, true, signalDesktopGateOffline)
	// A reconnect seen only by the status callback restarts the window.
	gate.observe(connected, now.Add(7*time.Minute))
	expect(paired, false, 8*time.Minute, false, signalDesktopGateOfflineWaiting)
	expect(paired, false, 13*time.Minute-time.Nanosecond, false, signalDesktopGateOfflineWaiting)
	expect(paired, false, 13*time.Minute, true, signalDesktopGateOffline)
	// Needing re-authentication is offline, not connected.
	expect(signallive.StatusSnapshot{Paired: true, NeedsReauth: true}, false, 20*time.Minute, true, signalDesktopGateOffline)
}

// A connected, receiving live bridge never lets the feed read Desktop, and so
// never appends; every other gate state appends.
func TestSignalDesktopFeedConnectedAndReceivingAppendsNothing(t *testing.T) {
	counts := [4]int{}
	state := uint64(0x9e3779b97f4a7c15)
	for iteration := 0; iteration < 120; iteration++ {
		state = state*6364136223846793005 + 1442695040888963407
		category := int((state >> 33) % 4)
		counts[category]++
		live := signallive.StatusSnapshot{Paired: category != 0, Connected: category >= 2}
		silent := category == 3
		calls := 0
		sink := &desktopFeedTestSink{}
		feed := newSDFTest(t, &live, func(_ context.Context, _ *signaldesktop.Facts, _ signaldesktop.ScanOptions, emit func(signaldesktop.Chunk) error) (signaldesktop.Stats, error) {
			calls++
			return signaldesktop.Stats{HighWaterRowID: 1, AccountVerified: true}, emit(sdfChunk())
		}, sink)
		feed.liveSilent = func(time.Time) bool { return silent }
		now := time.UnixMilli(sdfBaseMS)
		if category == 1 {
			// Paired and offline: only after five continuous minutes.
			feed.scanOnce(context.Background(), now)
			if calls != 0 {
				t.Fatalf("iteration %d: scanned within the offline window", iteration)
			}
			now = now.Add(signalDesktopOfflineDelay)
		}
		feed.scanOnce(context.Background(), now)
		wantAppend := category != 2
		if (calls == 1) != wantAppend || (len(sink.records) == 1) != wantAppend {
			t.Fatalf("iteration %d category %d: scans=%d appends=%d, want append=%v", iteration, category, calls, len(sink.records), wantAppend)
		}
		if gate := feed.Snapshot().Gate; category == 2 && gate != signalDesktopGateReceiving {
			t.Fatalf("gate reason = %q", gate)
		}
	}
	for category, count := range counts {
		if count == 0 {
			t.Fatalf("category %d never exercised", category)
		}
	}
}

func TestSignalDesktopFeedStopsWhenLiveResumesMidScan(t *testing.T) {
	live := signallive.StatusSnapshot{}
	sink := &desktopFeedTestSink{}
	feed := newSDFTest(t, &live, func(_ context.Context, _ *signaldesktop.Facts, _ signaldesktop.ScanOptions, emit func(signaldesktop.Chunk) error) (signaldesktop.Stats, error) {
		if err := emit(sdfChunk()); err != nil {
			return signaldesktop.Stats{}, err
		}
		live = signallive.StatusSnapshot{Paired: true, Connected: true}
		if err := emit(sdfChunk()); err != nil {
			return signaldesktop.Stats{}, err
		}
		return signaldesktop.Stats{HighWaterRowID: 99}, nil
	}, sink)
	feed.scanOnce(context.Background(), time.UnixMilli(sdfBaseMS))
	status := feed.Snapshot()
	if len(sink.records) != 1 || status.HighWaterRowID != 0 || status.Active || status.LastFullScanAtMS != 0 {
		t.Fatalf("appends=%d status=%+v, want one append and no advance", len(sink.records), status)
	}
}

func TestSignalDesktopFeedCadenceStampSkipAndFullRescan(t *testing.T) {
	live := signallive.StatusSnapshot{}
	var calls []sdfScanCall
	sink := &desktopFeedTestSink{}
	feed := newSDFTest(t, &live, func(_ context.Context, _ *signaldesktop.Facts, options signaldesktop.ScanOptions, emit func(signaldesktop.Chunk) error) (signaldesktop.Stats, error) {
		calls = append(calls, sdfScanCall{options: options})
		high := int64(10 * len(calls))
		return signaldesktop.Stats{HighWaterRowID: high, MaxRowID: high, AccountVerified: true}, emit(sdfChunk())
	}, sink)
	stamp := "a"
	feed.stamp = func() (string, error) { return stamp, nil }
	start := time.UnixMilli(sdfBaseMS)
	feed.scanOnce(context.Background(), start)
	feed.scanOnce(context.Background(), start.Add(30*time.Second))
	if len(calls) != 1 || calls[0].options.AfterRowID != 0 || calls[0].options.ChunkSize != signaldesktop.MaxChunkMessages {
		t.Fatalf("calls = %+v, want one full scan and a skipped unchanged tick", calls)
	}
	stamp = "b"
	feed.scanOnce(context.Background(), start.Add(time.Minute))
	if len(calls) != 2 || calls[1].options.AfterRowID != 10 {
		t.Fatalf("calls = %+v, want an incremental scan after the first high water", calls)
	}
	feed.scanOnce(context.Background(), start.Add(signalDesktopFullInterval))
	if len(calls) != 3 || calls[2].options.AfterRowID != 0 {
		t.Fatalf("calls = %+v, want a full rescan after the interval even with an unchanged stamp", calls)
	}
	status := feed.Snapshot()
	if status.LastFullScanAtMS != start.Add(signalDesktopFullInterval).UnixMilli() || status.HighWaterRowID != 30 || status.FramesAppended != 3 || status.MessagesAppended != 3 {
		t.Fatalf("status = %+v", status)
	}
	for _, record := range sink.records {
		if record.Generation != 0 || record.Codec != ingest.SignalDesktopHistoryCodec || record.AccountID != signalAccountID {
			t.Fatalf("record = %+v, want generation 0 Signal Desktop history", record)
		}
	}
}

func TestSignalDesktopFeedRestartsWhenTheArchiveIsReplaced(t *testing.T) {
	live := signallive.StatusSnapshot{}
	var afters []int64
	feed := newSDFTest(t, &live, func(_ context.Context, _ *signaldesktop.Facts, options signaldesktop.ScanOptions, _ func(signaldesktop.Chunk) error) (signaldesktop.Stats, error) {
		afters = append(afters, options.AfterRowID)
		if len(afters) == 1 {
			return signaldesktop.Stats{HighWaterRowID: 500, MaxRowID: 500}, nil
		}
		// A re-linked Desktop starts its rows again from 1.
		return signaldesktop.Stats{HighWaterRowID: options.AfterRowID, MaxRowID: 12}, nil
	}, &desktopFeedTestSink{})
	stamp := "a"
	feed.stamp = func() (string, error) { return stamp, nil }
	start := time.UnixMilli(sdfBaseMS)
	feed.scanOnce(context.Background(), start)
	stamp = "b"
	feed.scanOnce(context.Background(), start.Add(time.Minute))
	stamp = "c"
	feed.scanOnce(context.Background(), start.Add(2*time.Minute))
	if fmt.Sprint(afters) != "[0 500 0]" {
		t.Fatalf("scan marks = %v, want a full scan once the archive shrank below the mark", afters)
	}
}

func TestSignalDesktopFeedBacksOffAfterAFailedRead(t *testing.T) {
	live := signallive.StatusSnapshot{}
	calls := 0
	feed := newSDFTest(t, &live, func(context.Context, *signaldesktop.Facts, signaldesktop.ScanOptions, func(signaldesktop.Chunk) error) (signaldesktop.Stats, error) {
		calls++
		return signaldesktop.Stats{}, errors.New("synthetic read failure")
	}, &desktopFeedTestSink{})
	stamp := 0
	feed.stamp = func() (string, error) { stamp++; return fmt.Sprint(stamp), nil }
	start := time.UnixMilli(sdfBaseMS)
	for tick := 0; tick < 50; tick++ {
		feed.scanOnce(context.Background(), start.Add(time.Duration(tick)*signalDesktopScanInterval))
	}
	status := feed.Snapshot()
	if calls != 1 || status.Available || status.LastError == "" || status.NextAttemptAtMS != start.Add(signalDesktopRetryDelay).UnixMilli() {
		t.Fatalf("calls=%d status=%+v, want one attempt in the retry window", calls, status)
	}
	feed.scanOnce(context.Background(), start.Add(signalDesktopRetryDelay))
	if calls != 2 {
		t.Fatalf("calls=%d, want a retry once the window passed", calls)
	}
}

func TestSignalDesktopFeedAppendFailureRetriesNextTickWithoutAdvancing(t *testing.T) {
	live := signallive.StatusSnapshot{}
	calls := 0
	sink := &desktopFeedTestSink{failAt: 1}
	feed := newSDFTest(t, &live, func(_ context.Context, _ *signaldesktop.Facts, _ signaldesktop.ScanOptions, emit func(signaldesktop.Chunk) error) (signaldesktop.Stats, error) {
		calls++
		return signaldesktop.Stats{HighWaterRowID: 7, MaxRowID: 7}, emit(sdfChunk())
	}, sink)
	start := time.UnixMilli(sdfBaseMS)
	feed.scanOnce(context.Background(), start)
	if status := feed.Snapshot(); status.HighWaterRowID != 0 || status.LastFullScanAtMS != 0 || !strings.Contains(status.LastError, "synthetic append failure") || status.NextAttemptAtMS != 0 {
		t.Fatalf("status = %+v", status)
	}
	sink.failAt = 0
	feed.scanOnce(context.Background(), start.Add(signalDesktopScanInterval))
	if status := feed.Snapshot(); calls != 2 || status.HighWaterRowID != 7 || status.LastError != "" || len(sink.records) != 1 {
		t.Fatalf("calls=%d status=%+v records=%d", calls, status, len(sink.records))
	}
}

func TestSignalDesktopKeyCacheAsksTheKeychainOncePerWindow(t *testing.T) {
	now := time.UnixMilli(sdfBaseMS)
	asks := 0
	fail := true
	cache := &signalDesktopKeyCache{
		now: func() time.Time { return now },
		derive: func(context.Context) (string, error) {
			asks++
			if fail {
				return "", errors.New("synthetic keychain refusal")
			}
			return "abcd", nil
		},
	}
	for i := 0; i < 20; i++ {
		if _, err := cache.get(context.Background()); err == nil {
			t.Fatal("failed derive returned a key")
		}
		now = now.Add(time.Minute)
	}
	if asks != 1 {
		t.Fatalf("keychain asked %d times in the retry window, want 1", asks)
	}
	now = now.Add(signalDesktopRetryDelay)
	fail = false
	for i := 0; i < 5; i++ {
		if key, err := cache.get(context.Background()); err != nil || key != "abcd" {
			t.Fatalf("get = %q %v", key, err)
		}
	}
	if asks != 2 {
		t.Fatalf("keychain asked %d times, want the key cached after success", asks)
	}
	cache.invalidate()
	if _, err := cache.get(context.Background()); err != nil || asks != 3 {
		t.Fatalf("invalidate did not re-derive: asks=%d err=%v", asks, err)
	}
}

func TestSignalLiveSilenceProbeIgnoresDesktopHistory(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "store.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.Local)
	if err := store.UpsertAccount(sqlite.Account{AccountID: signalAccountID, BridgeKey: "signal_cli", Mode: sqlite.AccountModeLive, Enabled: true, ConfigJSON: "{}", CreatedAtMS: base.UnixMilli(), UpdatedAtMS: base.UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	clock := base
	repository, err := sqlite.NewMessageRepository(store, func() time.Time { return clock })
	if err != nil {
		t.Fatal(err)
	}
	appendAt := func(at time.Time, id, codec string) {
		t.Helper()
		clock = at
		if _, err := repository.AppendInbox(ctx, sqlite.InboxRecord{InboxID: id, AccountID: signalAccountID, DedupeKey: id, Codec: codec, CodecVersion: 1, Payload: []byte(`{}`)}); err != nil {
			t.Fatal(err)
		}
	}
	source := freshnessActivitySource(&v2Stack{Store: store}, nil, true)
	probe := signalLiveSilenceProbe(source)
	if probe(base) {
		t.Fatal("no live delivery at all must not count as a stall")
	}
	// Two busy weeks of live delivery, then nothing live, while Desktop
	// history keeps arriving.
	for day := 0; day < 14; day++ {
		for hour := 8; hour < 22; hour++ {
			for n := 0; n < 3; n++ {
				appendAt(base.AddDate(0, 0, day).Add(time.Duration(hour-12)*time.Hour+time.Duration(n)*time.Minute), fmt.Sprintf("live-%d-%d-%d", day, hour, n), ingest.SignalJSONRPCCodec)
			}
		}
	}
	lastLive := base.AddDate(0, 0, 13).Add(9*time.Hour + 2*time.Minute)
	for hour := 1; hour <= 48; hour++ {
		appendAt(lastLive.Add(time.Duration(hour)*time.Hour), fmt.Sprintf("desktop-%d", hour), ingest.SignalDesktopHistoryCodec)
	}
	latest, err := source.Latest(ctx)
	if err != nil || !latest["signal"].Equal(lastLive) {
		t.Fatalf("latest live = %v (%v), want the last signal.jsonrpc receipt", latest, err)
	}
	if probe(lastLive.Add(time.Hour)) {
		t.Fatal("an hour of quiet counted as a stall")
	}
	silentAt := lastLive.Add(2 * 24 * time.Hour)
	if !signalLiveSilenceProbe(source)(silentAt) {
		t.Fatal("two days without live delivery did not count as a stall, despite Desktop history in that window")
	}
}

// End to end on a real v2 stack and a synthetic Desktop archive read through
// the production queries: the first scan fills the gap, later scans append
// nothing for what v2 already holds, and a new Desktop message is picked up
// incrementally.
func TestSignalDesktopFeedEndToEndOnAV2Stack(t *testing.T) {
	ctx := context.Background()
	stack, err := newV2Stack(v2StackDeps{Logger: zerolog.Nop(), DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer stack.Store.Close()
	if err := ensureSignalDesktopAccount(stack); err != nil {
		t.Fatal(err)
	}
	nowMS := time.Now().UnixMilli()
	selfID := v2keys.DeriveID("identity", signalAccountID, "e164\x1f"+sdfSelf)
	if err := stack.Store.UpsertIdentity(sqlite.Identity{IdentityID: selfID, AccountID: signalAccountID, Kind: "e164", CanonicalValue: sdfSelf, RawValue: sdfSelf, IsSelf: true, MetadataJSON: "{}", CreatedAtMS: nowMS, UpdatedAtMS: nowMS}); err != nil {
		t.Fatal(err)
	}
	archivePath := filepath.Join(t.TempDir(), "db.sqlite")
	archive := fixture.Archive{
		SelfNumber: sdfSelf, SelfACI: sdfSelfACI,
		Conversations: []fixture.Conversation{
			{ID: "conv-self", Type: "private", E164: sdfSelf, ServiceID: sdfSelfACI},
			{ID: "conv-peer", Type: "private", E164: sdfPeerE164, ServiceID: sdfPeerACI, Name: "Synthetic Peer"},
		},
	}
	for row := int64(1); row <= 120; row++ {
		message := fixture.Message{RowID: row, ID: fmt.Sprintf("m%d", row), ConversationID: "conv-peer", Type: "incoming", Body: fmt.Sprintf("synthetic %d", row), SentAt: sdfBaseMS + row, SourceServiceID: sdfPeerACI}
		if row%3 == 0 {
			message.Type = "outgoing"
			message.SourceServiceID = ""
		}
		archive.Messages = append(archive.Messages, message)
	}
	if err := fixture.Write(archivePath, archive); err != nil {
		t.Fatal(err)
	}
	reader, err := sql.Open("sqlite", "file:"+archivePath+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	live := signallive.StatusSnapshot{}
	feed := &signalDesktopFeed{
		status:     signalDesktopFeedStatus{Enabled: true, RowsSkippedByReason: map[string]int64{}},
		gate:       &signalDesktopGate{},
		live:       func() signallive.StatusSnapshot { return live },
		liveSilent: func(time.Time) bool { return false },
		scan: func(ctx context.Context, facts *signaldesktop.Facts, options signaldesktop.ScanOptions, emit func(signaldesktop.Chunk) error) (signaldesktop.Stats, error) {
			return (&signaldesktop.Reader{Runner: signaldesktop.SQLRunner{DB: reader}}).Scan(ctx, facts, options, emit)
		},
		facts: func(ctx context.Context) (*signaldesktop.Facts, error) {
			return signaldesktop.LoadFacts(ctx, stack.Store, signalAccountID)
		},
		stamp:  func() (string, error) { return signalDesktopFileStamp(archivePath) },
		sink:   stack.Sink,
		logger: zerolog.Nop(), logged: map[string]bool{},
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- stack.worker.Run(runCtx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("stop worker: %v", err)
		}
	}()
	waitImported := func(want uint64) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for stack.Counters.Snapshot(signalAccountID).HistoryImported < want && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		if got := stack.Counters.Snapshot(signalAccountID).HistoryImported; got != want {
			t.Fatalf("history imported = %d, want %d", got, want)
		}
	}
	start := time.UnixMilli(sdfBaseMS + 1_000_000)
	feed.scanOnce(ctx, start)
	waitImported(120)
	status := feed.Snapshot()
	if status.FramesAppended != 2 || status.MessagesAppended != 120 || status.HighWaterRowID != 120 || !status.AccountVerified || status.LastError != "" {
		t.Fatalf("first scan status = %+v", status)
	}
	counters := stack.Counters.Snapshot(signalAccountID)
	if counters.Appended != 0 || counters.Projected != 0 || counters.HistoryConversations != 1 {
		t.Fatalf("counters = %+v", counters)
	}

	// A full rescan finds everything in v2 and appends nothing.
	feed.scanOnce(ctx, start.Add(signalDesktopFullInterval))
	if status := feed.Snapshot(); status.FramesAppended != 2 || status.RowsInV2 != 120 || status.RowsIncluded != 0 {
		t.Fatalf("full rescan status = %+v", status)
	}

	// A new Desktop message arrives; the incremental scan sends only it.
	writer, err := sql.Open("sqlite", archivePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Exec(`INSERT INTO messages (rowid, id, json, sent_at, conversationId, received_at, source, type, body, sourceServiceId) VALUES (121, 'm121', '{}', ?, 'conv-peer', 0, NULL, 'incoming', 'synthetic new', ?)`, sdfBaseMS+121, sdfPeerACI); err != nil {
		t.Fatal(err)
	}
	writer.Close()
	future := time.Now().Add(time.Second)
	if err := os.Chtimes(archivePath, future, future); err != nil {
		t.Fatal(err)
	}
	feed.scanOnce(ctx, start.Add(signalDesktopFullInterval+signalDesktopScanInterval))
	waitImported(121)
	if status := feed.Snapshot(); status.FramesAppended != 3 || status.MessagesAppended != 121 || status.HighWaterRowID != 121 {
		t.Fatalf("incremental status = %+v", status)
	}
	conversation, err := stack.Store.GetConversationByRemote(signalAccountID, "signal:"+sdfPeerE164)
	if err != nil {
		t.Fatal(err)
	}
	if conversation.LastMessageAtMS != sdfBaseMS+121 {
		t.Fatalf("recency = %d, want the new message", conversation.LastMessageAtMS)
	}
}
